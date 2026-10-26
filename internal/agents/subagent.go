package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"multiagent/internal/agent"
	"multiagent/internal/llmjson"
	"multiagent/internal/schema"
	"multiagent/internal/state"
	"multiagent/internal/tool"
)

// 这个文件定义 L2 的**被调度者**：SubAgent，以及把它包装成工具的适配器。
//
// 为什么"每个 SubAgent 暴露成一个工具"？（本阶段最关键的一个设计）
//
//	「路由」的本质就是**从有限集合里选一个动作** —— 这正是工具调用的定义。
//	于是我们白拿三件事：
//
//	  ① **路由合法性由工具注册表天然保证**：模型只能调用注册过的名字，
//	     调一个不存在的 → 注册表返回结构化错误 → 回灌自愈。
//	     （直接治企业仓库 A1：路由全靠 Prompt、无法校验、无法统计）
//	  ② **路由决策是结构化的**（工具名 + 参数），可统计、可测试、可回归。
//	  ③ **子 Agent 执行完，结果作为 tool 消息回灌给编排者** ——
//	     "确定性回传"是**免费的**。
//	     （对照企业仓库：必须显式用 AgentWithDeterministicTransferTo 才能做到）
//
// 另一个关键设计：**依赖由 `Needs()` 声明、由代码强制**。
//
//	企业仓库把"先搜索后渲染"写在 Prompt 里（P3：用自然语言写 if-else）；
//	我们让 SubAgent 声明"我需要哪些上游产物"，代码在调用前检查。
//	**依赖未就绪 → 这一跳根本调不通** —— 顺序约束从此不靠模型自觉。

// SubAgent 是一个可被 Supervisor 调度的角色。
type SubAgent interface {
	// Name 是角色名（也写进产物的 produced_by）。
	Name() string

	// Kind 是它产出的产物种类，同时**就是**它在 Supervisor 眼里的工具名。
	Kind() schema.Kind

	// Description 是给编排者看的"什么时候该调我"。
	//
	// 注意：它**只描述能力与边界**，不描述上游数据长什么样 ——
	// 后者是代码的事（见 Needs）。
	Description() string

	// Needs 声明我依赖哪些上游产物。
	//
	// 这是**能被代码检查的依赖声明**：
	//   · 缺任何一个 → 调用被拒绝，并回灌"还缺什么"；
	//   · 全部就绪 → 由适配器自动把它们放进 SubAgentInput（人工不参与搬运）。
	Needs() []schema.Kind

	// Run 执行一次，返回**类型化的产物**（还没入黑板）。
	Run(ctx context.Context, in SubAgentInput) (schema.Payload, error)
}

// StatsReporter 是 SubAgent 的**可选能力**：能报出自己的用量。
//
// 为什么不把它并进 `SubAgent`？
//
//	"能被调度"和"能报用量"是两件事。把可选能力塞进主接口，会强迫每一个实现
//	都写一个可能恒返回零值的方法 —— 接口越来越胖，而真正的约束
//	（Name / Kind / Description / Needs / Run）反而被淹没。
//	→ 用类型断言探测即可（与 `schema.Partial` 同一个取舍）。
//
// ⚠️ **口径必须是"累计"**（本角色实例跨多次 Run 累加），不是"最近一次"。
//
//	这是 L2-3 的修复。此前的实现是 `lastStats = a.Stats()`（**覆盖**），
//	而编排者那边是 `total = addStats(total, ...)`（**累加**）——
//	同一张成本表里两行用了两种口径。
//
//	后果很具体：**某个角色跑了两遍时，它的 token 只算最后一遍** ——
//	也就是"返工最多、成本最高"的那一次，我们反而算得最少。
//
//	更深一层的根因（留给 L4）：统计**挂在实例上**，就必然要回答
//	"跨 Run 是覆盖还是累加"。更稳的形状是让 `Run` 把 stats 作为**返回值**带出来 ——
//	没有共享可变字段，就没有这个问题。但那要先有并发压力，现在不急。
type StatsReporter interface {
	// Stats 返回**本角色实例的累计用量**（跨多次 Run 累加）。
	Stats() agent.Stats
}

// SubAgentInput 是 SubAgent 的输入。
//
// ⚠️ 它**只带引用与摘要**，不带上游产物的正文（原则四）。
// 需要全文的人，自己用 Blackboard 去取 —— 而"什么时候需要全文"只有它自己知道。
type SubAgentInput struct {
	// Topic 是用户主题。它每跳都带，因为它是**用户硬约束**的一部分
	// （绝不可丢的那一类信息，见设计课上的压缩策略）。
	Topic string

	// Hint 是编排者给的额外要求（例：'重点讲成本，不超过 3 条'）。
	// 它是**指令**，不是数据搬运 —— 这点区别很重要。
	Hint string

	// Inputs 是上游产物的引用 + 摘要（按 Needs 的顺序）。
	Inputs []schema.Handoff

	// Upstream 用来按需取回上游产物全文。
	//
	// ⚠️ 类型是**窄接口** `schema.Reader` 而不是 `*state.Blackboard`：
	//	SubAgent 只需要"取"，**不该拿到"写"**。
	//
	//	（这一点在 L2 时没意识到 —— 当时字段类型就是 `*state.Blackboard`，
	//	  "SubAgent 不要写黑板"是一条靠自觉遵守的约定。
	//	  L3 写 `SubAgent → Node` 适配器时被逼出来：适配器手里只有
	//	  `NodeInput` 的只读能力，于是两处一起改成了窄接口。）
	Upstream schema.Reader
}

// Fetch 按输入里的引用取回上游产物的**全文**。
//
// 为什么不一开始就把全文放进 Inputs？
//
//	因为大多数 SubAgent 其实不需要上游全文（比如 renderer 只看设计结果）。
//	把取全文变成一个**显式动作**，才能看出"谁真的需要它" ——
//	也才谈得上"上下文只放决策所需的最小信息"（原则四）。
func (in SubAgentInput) Fetch() ([]schema.Artifact, error) {
	if in.Upstream == nil {
		return nil, fmt.Errorf("未提供上游读取能力，无法取回上游产物")
	}
	out := make([]schema.Artifact, 0, len(in.Inputs))
	for _, h := range in.Inputs {
		a, err := in.Upstream.Get(h.Ref)
		if err != nil {
			return nil, fmt.Errorf("取回上游产物 %s 失败: %w", h.Ref, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// subAgentInputSummary 生成给 SubAgent 看的上游清单（只有引用与摘要）。
func (in SubAgentInput) summaryLines() []string {
	if len(in.Inputs) == 0 {
		return nil
	}
	lines := make([]string, 0, len(in.Inputs))
	for _, h := range in.Inputs {
		lines = append(lines, fmt.Sprintf("- %s：%s", h.Ref, h.Summary))
	}
	return lines
}

// ─── 把 SubAgent 包装成工具 ─────────────────────────────────────────

// subAgentTool 是 SubAgent 的"工具形态"。
//
// 它承担三件事，一件都不能少：
//
//	① 依赖检查（代码强制的顺序约束）
//	② 自动装配输入（零数据搬运）
//	③ 把产物写进黑板，并只回传 {ref, summary}（原则四）
//
// maxConsecutiveFailures 是同一个角色允许**连续失败**的次数上限。
//
// 为什么需要它？（**L2-9 接真实模型时被逼出来的**）
//
//	真实运行里，`outline` 因为单次调用超时而每一步都失败。
//	编排者（真实模型）**换了 10 种不同措辞反复重试它**，直到 MaxSteps 强制终止 ——
//	烧掉 3056 prompt + 7364 completion，而**根因一次都没变**。
//	整轮合计 42906 + 12415 tokens，任务却没完成（Mock 跑完全套只要 4964）。
//
//	**重试没有改变根因，只放大了成本。** 这就是 L1 复盘里 S3（计费暴涨）的现场。
//
// 与"原则一（确定性兜底）"的关系：
//
//	"该不该重试"是一个**模型判断** —— 而模型判断只是期望，不是保证。
//	所以必须有一条不依赖模型的兜底，就是这个计数。
//
// 对照企业仓库（01 索引 C2）：
//
//	它有模型级重试（3 次 + 指数退避 + 抖动，防"重试风暴"），
//	但**缺"Agent 级失败升级链"** —— 子 Agent 整体失败时没有明确策略。
//	这一条补的正是它。
const maxConsecutiveFailures = 3

type subAgentTool struct {
	sub   SubAgent
	board *state.Blackboard
	topic string

	// failures 记录这个角色**连续**失败了几次（成功即清零）。
	//
	// 计数挂在这个工具实例上，而它在一次编排内是**共享**的
	//（supervisor 每轮复用同一批工具）—— 所以它是"跨轮累计"的，
	// 这正是我们想要的：编排者换措辞重试，计数照样会涨。
	failures int
}

// NewSubAgentTool 把 SubAgent 包装成编排者可调用的工具。
//
// 构造时就校验 Kind 合法性：**错误要尽量靠近它的来源**，
// 而不是等到某一轮运行到一半才发现这个角色永远调不通。
func NewSubAgentTool(sub SubAgent, board *state.Blackboard, topic string) (tool.Tool, error) {
	if sub == nil {
		return nil, fmt.Errorf("SubAgent 不能为空")
	}
	if board == nil {
		return nil, fmt.Errorf("子 Agent %s：未提供黑板", sub.Name())
	}
	if !sub.Kind().Valid() {
		return nil, fmt.Errorf("子 Agent %s 的 kind=%q 不合法，无法作为工具名", sub.Name(), sub.Kind())
	}
	return &subAgentTool{sub: sub, board: board, topic: topic}, nil
}

// Definition 实现 tool.Tool 接口。
//
// 工具名 = Kind。这一点很重要：**路由的"合法值集合"就是工具名集合**，
// 而工具名又必须过 schema.Kind.Valid() —— 两层校验，天然挡住 A1 那类"路由到不存在的 Agent"。
func (t *subAgentTool) Definition() tool.Definition {
	desc := t.sub.Description()
	if needs := t.sub.Needs(); len(needs) > 0 {
		names := make([]string, 0, len(needs))
		for _, k := range needs {
			names = append(names, string(k))
		}
		desc += fmt.Sprintf(" 【前置条件】必须先产出 %s；未就绪时调用会被拒绝。",
			strings.Join(names, "、"))
	}

	return tool.Definition{
		Name:        string(t.sub.Kind()),
		Description: desc,
		Parameters: []tool.Parameter{
			{
				Name:        "hint",
				Type:        tool.TypeString,
				Description: "可选：对这一跳的额外要求（如「重点讲成本，不超过 3 条」）",
			},
		},
	}
}

// subAgentArgs 是工具入参。
//
// 故意只有 hint：**没有任何"上游数据"字段**。
// 上游产物由代码按 Needs 自动装配 —— 编排者即使想搬数据也无处可搬。
type subAgentArgs struct {
	Hint string `json:"hint"`
}

// subAgentOutput 是回传给编排者的东西：**只有引用与摘要**。
type subAgentOutput struct {
	OK      bool   `json:"ok"`
	Ref     string `json:"ref"`
	Summary string `json:"summary"`
	Note    string `json:"note"`
}

// Execute 实现 tool.Tool 接口。
func (t *subAgentTool) Execute(ctx context.Context, args string) (string, error) {
	var in subAgentArgs
	if err := json.Unmarshal([]byte(llmjson.Clean(args, false)), &in); err != nil {
		return "", fmt.Errorf("参数不是合法 JSON: %w", err)
	}

	// ⓪ 熔断检查：这个角色已经**连续失败太多次**了。
	//
	// 放在最前面，因为它是最强的信号：换上游、换参数都救不了它，
	// 只有"换一条路"才行。拒绝信息要**可执行**（L1-5），
	// 所以它明确指出三条出路，而不是只说"不行"。
	if t.failures >= maxConsecutiveFailures {
		return "", fmt.Errorf(
			"%s 已连续失败 %d 次，已被**熔断**。同一角色反复失败通常不是参数问题，"+
				"而是环境或上游的问题 —— 换个措辞重试不会改变它。"+
				"请**换一条路**：换别的角色、换策略，或者直接结束并说明卡在哪里",
			t.sub.Name(), t.failures)
	}

	// ① 依赖检查：**顺序约束由代码强制，不靠 Prompt**（治 P3）
	missing := t.missingNeeds()
	if len(missing) > 0 {
		return "", fmt.Errorf("前置产物未就绪：缺少 %s。请先调用对应工具产出它们",
			strings.Join(missing, "、"))
	}

	// 取一次上游产物，后面依赖检查 / 装配输入 / 记录血缘都用它。
	//
	// ⚠️ 注意"取最新版"这个动作只发生**一次** —— 如果分别取三次，
	// 三次之间理论上可以插入新的写入，于是"检查的"和"用的"就不是同一份了。
	inputs := t.collectInputs()

	// ② 新鲜度检查（L2-1）：**"有"不等于"是对的"**。
	//
	// 与 ① 是同一个检查的两个阶段：
	//
	//	①  有没有？    —— missingNeeds
	//	②  还对不对？  —— staleInputs
	//
	// 为什么必须在**装配输入之前**拦？因为一旦把过期的上游发下去，
	// 子 Agent 会基于过期的假设做决定，而它产出的东西**看起来完全正常** ——
	// 这是最难发现的一类错误（新内容配旧版式、设计基于旧内容）。
	if stale := t.staleInputs(inputs); len(stale) > 0 {
		return "", fmt.Errorf("前置产物已过期（依据链被上游的新版本打断）：\n  - %s\n"+
			"请先按上面的提示重新调度对应角色，再继续",
			strings.Join(stale, "\n  - "))
	}

	// ③ 装配输入：上游产物只以 {ref, summary} 的形式进入（零搬运）
	input := SubAgentInput{
		Topic:    t.topic,
		Hint:     strings.TrimSpace(in.Hint),
		Inputs:   schema.HandoffsOf(inputs),
		Upstream: t.board,
	}

	product, err := t.sub.Run(ctx, input)
	if err != nil {
		t.failures++
		// 把"这是连续第几次失败"告诉模型 —— 它自己数不出来（上下文每轮会重置），
		// 而"已经第 3 次了"和"第一次"应当导致**完全不同的决策**。
		return "", fmt.Errorf("%s 执行失败（连续第 %d 次）: %w",
			t.sub.Name(), t.failures, err)
	}
	t.failures = 0 // 成功即清零 —— 它是"连续"计数，不是"累计"计数
	if product == nil {
		return "", fmt.Errorf("%s 没有产出任何东西", t.sub.Name())
	}

	// 双向校验：产物自己声明的 kind 必须与这个角色声明的 kind 一致。
	//
	// 否则黑板会错位：工具名叫 "outline"，里面却存了一份 research。
	// 下游按 Needs 去取 outline，拿到的是 research —— 类型没报错、字段都在，
	// 但语义全错。这是最隐蔽的一类故障（与 schema.Decode 的双向校验同一个道理）。
	if product.Kind() != t.sub.Kind() {
		return "", fmt.Errorf("%s 产出的 kind=%q 与它声明的 %q 不一致",
			t.sub.Name(), product.Kind(), t.sub.Kind())
	}

	// ③ 入黑板：Key 用 Kind（一个角色一个 key，多次执行只是多版本）。
	//    ProducedBy 用角色名 —— 出问题时"这是谁产的"是第一件要问的事。
	a, err := schema.NewArtifact(string(t.sub.Kind()), t.sub.Name(), product)
	if err != nil {
		return "", err
	}

	// ★ 血缘：把"我是在哪一版上游之上做出来的"记进产物。
	//
	// 为什么由**这一层**填，而不是让子 Agent 自己填？
	//
	//	因为"我依赖了谁"这件事，是**编排/装配**的职责，不是业务内容的职责。
	//	让每个角色自己申报，就会有四种写法；而且它们根本不知道上游的**版本号**
	//	（它们只拿到 Handoff，里面是字符串 ref）。
	//	这里刚好同时握着"Artifact + 版本"，填它是一行的事。
	//
	// 这与 Version 由黑板填是同一条原则：**谁拥有这个信息，谁负责维护它。**
	a.BasedOn = schema.BasesOf(inputs)

	stored, err := t.board.Put(a)
	if err != nil {
		return "", fmt.Errorf("产物入库失败: %w", err)
	}

	out, err := json.Marshal(subAgentOutput{
		OK:      true,
		Ref:     schema.RefOf(stored),
		Summary: stored.Summary,
		Note:    "产物已存入黑板。你的上下文里不需要带正文；需要时用 ref 去取。",
	})
	if err != nil {
		return "", fmt.Errorf("序列化回执失败: %w", err)
	}
	return string(out), nil
}

// missingNeeds 返回尚未就绪的前置产物种类。
func (t *subAgentTool) missingNeeds() []string {
	var missing []string
	for _, k := range t.sub.Needs() {
		if t.board.Versions(string(k)) == 0 {
			missing = append(missing, string(k))
		}
	}
	return missing
}

// collectInputs 取回已就绪的前置产物（**最新版**）。
//
// 返回 `Artifact` 而不是 `Handoff`，是因为调用方需要它做两件不同的事：
//
//	① 生成 `Handoff`（只有 ref + summary）发给子 Agent —— 零搬运；
//	② 生成 `Basis`（key + version）记进产物的**血缘**里 —— L2-1。
//
// 如果这里只交出 `Handoff`（一堆 `"outline@1"` 字符串），第 ② 件事就得
// 先把字符串**解析**回 key 与版本 —— 那正是我们刻意避免的：
//
//	**给模型看的是字符串，给代码比的是结构，不要在两者之间来回编解码。**
func (t *subAgentTool) collectInputs() []schema.Artifact {
	needs := t.sub.Needs()
	out := make([]schema.Artifact, 0, len(needs))

	for _, k := range needs {
		a, err := t.board.Get(string(k)) // 取最新版
		if err != nil {
			continue // missingNeeds 已经拦过了，这里的兜底只是让代码不必 panic
		}
		out = append(out, a)
	}
	return out
}

// staleInputs 返回"依据链已过期"的前置产物（人话描述）。
//
// 为什么在**适配器**这一层查？因为它是唯一同时知道
// "我即将消费什么"与"当前最新版是什么"的地方 ——
// 与 `missingNeeds` 放在一起，前置条件的两个层次就都在同一处了。
//
// 这里做一次**跨输入的合并去重**：同一条依据常常被多个输入共享
// （例：`outline@1 过期` 会同时出现在 `outline@1` 自己和 `design@1` 的链条上），
// 不去重的话，模型会看到同一句话两遍 —— 反馈一啰嗦，可读性就掉。
func (t *subAgentTool) staleInputs(inputs []schema.Artifact) []string {
	latest := func(key string) (schema.Artifact, bool) {
		a, err := t.board.Get(key)
		if err != nil {
			return schema.Artifact{}, false
		}
		return a, true
	}

	var (
		out  []string
		seen = make(map[string]bool)
	)
	for _, a := range inputs {
		for _, why := range schema.StaleReasons(a, latest) {
			if seen[why] {
				continue
			}
			seen[why] = true
			out = append(out, why)
		}
	}
	return out
}

// BudgetNotice 把**步数预算**写进给模型的输入。
//
// 为什么必须写出来？（**L2-9 的真实运行逼出来的**）
//
//	`research` 的 MaxSteps 是 6，而模型**完全不知道**这个数 ——
//	于是它一直搜、一直搜，到第 6 步被强制终止，**已经搜到的素材全部丢弃**；
//	换一轮重来又要从头搜一遍。实测它连续撞了两次。
//
//	**一个不告诉模型的预算，等于没有预算** —— 模型无法为它做规划，只能撞上去。
//
// 数字由调用方传入（就是那个 `MaxSteps`），所以它只有一个来源：
// 改预算只改一处，Prompt 会自动跟着变（"约束的单一真相来源"）。
func BudgetNotice(steps int) string {
	return fmt.Sprintf(
		"\n【预算】你最多只能调用 %d 次工具；超出会被**强制终止，且本次全部工作作废**。"+
			"请据此规划：够用即止，然后尽快用提交工具交出结论。\n", steps)
}

// SubAgentInputLines 把上游清单渲染成给人/给模型看的多行文本。
//
// 导出的原因：各个 SubAgent 的 Prompt 都要用到它，
// 让"引用+摘要长什么样"只有一个实现（单一真相来源）。
func SubAgentInputLines(in SubAgentInput) []string { return in.summaryLines() }
