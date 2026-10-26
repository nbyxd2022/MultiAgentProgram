package agents

import (
	"context"
	"fmt"
	"strings"

	"multiagent/internal/agent"
	"multiagent/internal/budget"
	"multiagent/internal/llm"
	"multiagent/internal/observ"
	"multiagent/internal/schema"
	"multiagent/internal/state"
	"multiagent/internal/tool"
)

// Supervisor 是中心化的编排者。
//
// 它的职责**只有两件**：
//
//	① **路由**：决定下一步派给哪个角色；
//	② **判终止**：判断必需产物是否已经齐全。
//
// 它**不做**的事（这几条是硬规矩，不是风格偏好）：
//
//	· 不创作内容；
//	· **不搬运数据** —— 它的上下文里只有 {引用, 摘要}，正文根本不会经过它；
//	· 不在 Prompt 里出现任何业务字段名（title / bullets / pages ...）。
//	  一旦出现，就说明它正在做"逐字段提取重组"，也就是企业仓库 A2/A3 的坑。
//
// 它**不需要**发明 transfer_to_agent 之类的机制：
// 每个角色都已经被包装成工具，于是"派活"= 调用工具，
// "子 Agent 完成后回到我这里"= 工具结果自动回灌 —— 都是白拿的。
// （对照企业仓库：那需要显式用 adk.AgentWithDeterministicTransferTo 才能做到。）
type Supervisor struct {
	client   llm.Client
	board    *state.Blackboard
	subs     []SubAgent
	required []schema.Kind

	maxSteps int
	tracer   observ.Tracer

	// guard 是预算守卫（与 Runner 同一个对象）。
	guard budget.Guard

	lastStats agent.Stats
}

// SupervisorOptions 是构造编排者的参数。
type SupervisorOptions struct {
	Client    llm.Client
	Board     *state.Blackboard
	SubAgents []SubAgent

	// Required 是"这次任务必须产出的产物种类"。
	//
	// 它是**终止判断的确定性兜底**：模型可以有自己的判断，
	// 但"必需产物齐没齐"由代码数了算（原则一）。
	Required []schema.Kind

	MaxSteps int
	Tracer   observ.Tracer

	// Guard 是预算守卫（可以为 nil -> 归一成 Unlimited）。
	// 它该与 RunnerOptions.Guard 是同一个对象（见 agent.Options.Guard）。
	Guard budget.Guard
}

// supervisorRounds 是"补齐缺失产物"的外层轮数上限（硬兜底）。
const supervisorRounds = 3

// NewSupervisor 构造编排者。
//
// 这里做的是**构造期校验**：配置错了当场就说，别等到跑到一半才发现某个角色永远调不通。
func NewSupervisor(opt SupervisorOptions) (*Supervisor, error) {
	if opt.Client == nil {
		return nil, fmt.Errorf("supervisor: 未配置模型客户端")
	}
	if opt.Board == nil {
		return nil, fmt.Errorf("supervisor: 未提供黑板")
	}
	if len(opt.SubAgents) == 0 {
		return nil, fmt.Errorf("supervisor: 至少要有一个子 Agent")
	}

	// 角色声明的 kind 必须唯一：两个角色产同一种产物 → 黑板会撞车。
	seen := make(map[schema.Kind]string, len(opt.SubAgents))
	for _, sub := range opt.SubAgents {
		if !sub.Kind().Valid() {
			return nil, fmt.Errorf("supervisor: 子 Agent %s 的 kind=%q 不合法", sub.Name(), sub.Kind())
		}
		if prev, dup := seen[sub.Kind()]; dup {
			return nil, fmt.Errorf("supervisor: %s 与 %s 都声明产出 %q，key 会撞车",
				prev, sub.Name(), sub.Kind())
		}
		seen[sub.Kind()] = sub.Name()
	}

	// Required 里不能要求一个谁都产不出的东西 —— 否则这个任务永远无法"完成"。
	for _, k := range opt.Required {
		if _, ok := seen[k]; !ok {
			return nil, fmt.Errorf("supervisor: 必需产物 %q 没有任何角色负责产出", k)
		}
	}

	if opt.MaxSteps <= 0 {
		opt.MaxSteps = 12 // 每个角色各一次 + 收尾，留些余量
	}

	return &Supervisor{
		client:   opt.Client,
		board:    opt.Board,
		subs:     opt.SubAgents,
		required: opt.Required,
		maxSteps: opt.MaxSteps,
		tracer:   opt.Tracer,
		guard:    opt.Guard,
	}, nil
}

// Result 是一次编排的结果。
type Result struct {
	// Completed 是各角色产物的引用（按**角色声明顺序**，不是完成顺序）。
	// 用声明顺序是为了"输出稳定"：同一份输入打印两次必须一样。
	Completed []schema.Handoff

	// Missing 是**完全没有产出**的必需产物。
	Missing []schema.Kind

	// Incomplete 是**产出了、但自报留有缺口**的必需产物，形如 `"render：第 3 页：写盘失败"`。
	//
	// ⚠️ 它与 Missing **必须分开**，因为给模型的反馈必须准确：
	//
	//	说 "render 还没有产出" → 模型会把整件事**重做一遍**（白烧一整轮）
	//	说 "render 已经做过，但第 3 页没出来" → 模型只会去补那一页
	//
	// 这一条来自 L2-2：**"存在"不等于"完整"**。
	Incomplete []string

	// Stale 是**依据链已经过期**的必需产物，形如
	// `"render：render@1 依据的 outline@1 已经更新到 outline@2（产出者 renderer，请重新调度它）"`。
	//
	// 这是 **L2-1**：产物"有"、也"完整"，但它是**基于旧版上游**做出来的 ——
	// 内容与版式错配、设计基于过时内容，全都属于这一类。
	//
	// 列表顺序是**可执行的修复顺序**（先上游、后下游），
	// 由 `schema.StaleReasons` 的后序遍历保证。
	//
	// ⚠️ 为什么这一项也要进**完成判断**？因为"不派活"也是一种交付：
	// 如果只在下发时拦，编排者完全可能在"上游被重做、下游没人再派"的情况下
	// 宣布完成 —— 交付一份过期产物，而且没有任何一层发现。
	Stale []string

	// Rounds 是外层补齐轮数。
	Rounds int

	// Stats 是这次编排的总用量（跨轮累加）。
	Stats agent.Stats
}

// Complete 表示必需产物是否**齐全、完整、且仍然有效**。
//
// 三个条件缺一不可 —— 这就是 **L2 版的 `Deliverable()`**：
//
//	Missing 为空    —— 该有的都有
//	Incomplete 为空 —— 有的都是完整的      （L2-2）
//	Stale 为空      —— 有的都还是"对的"    （L2-1）
//
// ⚠️ 在此之前这里只看 Missing，于是两类问题都会被判成"必需产物齐全"：
// "3 页里失败 1 页"与"它是基于旧版上游做出来的"。
// 它们与 L1-3 同构：**都是用一个更弱的判据，代替一个更强的判据。**
//
// ⚠️ 另外：**不完整/过期都 ≠ 失败** —— 与 L1 的"超限 ≠ 失败"同一个道理。
// 调用方应当"交付已完成的部分 + 说明还差什么"，而不是把整轮工作丢掉。
func (r Result) Complete() bool {
	return len(r.Missing) == 0 && len(r.Incomplete) == 0 && len(r.Stale) == 0
}

// Stats 返回最近一次编排的用量。
func (s *Supervisor) Stats() agent.Stats { return s.lastStats }

// Run 按用户主题驱动一次完整编排。
//
// 注意黑板是**有状态**的：如果板子上已经有某些产物（例如上一次运行的残留，
// 或者上游流程已经跑过研究阶段），该角色的前置条件就已经满足，可以直接进入下一环。
// 这正是"续跑 / 返工时只重做一部分"的基础。
func (s *Supervisor) Run(ctx context.Context, topic string) (Result, error) {
	registry := tool.NewRegistry()
	for _, sub := range s.subs {
		t, err := NewSubAgentTool(sub, s.board, topic)
		if err != nil {
			return Result{}, err
		}
		registry.Register(t)
	}

	// 编排者就是 L0 那个 agent.Agent —— 不新造循环。
	// 它自带的"自然终止 + MaxSteps 硬上限"正好就是我们要的双保险。
	sup := agent.New(agent.Options{
		Name:         "supervisor",
		Description:  "中心化编排者：只路由、只判终止",
		SystemPrompt: supervisorPrompt(s.required),
		Client:       s.client,
		Tools:        registry,
		MaxSteps:     s.maxSteps,
		Tracer:       s.tracer,
		Guard:        s.guard,
	})

	var total agent.Stats
	result := Result{}

	for round := 1; round <= supervisorRounds; round++ {
		result.Rounds = round

		// 第 1 轮是「开工」，之后才是「系统检查」。
		//
		// ⚠️ 这个 `round > 1` 不是可有可无的 —— 它是本轮演示里**暴露出来的**：
		//
		//	第 1 轮开始时黑板是空的，`missing` 必然非空，
		//	于是 `taskPrompt` **永远轮不到**，一开口就变成"你还没做完"。
		//	对模型来说那是个很别扭的开场（它还没开始做），
		//	而且"还没有产出 research、outline、design、render"这句话
		//	和系统提示里的任务说明完全重复 —— 白花的 token。
		prompt := taskPrompt(topic)
		if round > 1 {
			// 三个问题分开问，回答也分开给：
			//
			//	Missing    —— 完全没产出          → 反馈："还没有产出"
			//	Incomplete —— 产出过但有缺口      → 反馈："做过了，但缺这几处"
			//	Stale      —— 产出过但依据过期了  → 反馈："做过了，但它依据的上游变了"
			//
			// 混为一谈的代价很真实：模型会把整件事重做一遍，白烧一整轮。
			missing, incomplete, stale := s.gaps()
			if len(missing) > 0 || len(incomplete) > 0 || len(stale) > 0 {
				prompt = reminderPrompt(topic, missing, incomplete, stale)
			}
		}

		if _, err := sup.Run(ctx, prompt); err != nil {
			total = addStats(total, sup.Stats())
			s.lastStats = total
			result.complete(s)
			return result, fmt.Errorf("supervisor: %w", err)
		}
		total = addStats(total, sup.Stats())

		// ── 终止判断的确定性兜底（原则一）──
		//
		// 模型不再调用工具，只代表"它认为做完了"。
		// 真正说了算的是这里，而且必须问**三个**问题：
		//   · 该有的都有了吗？（Missing）
		//   · 有的都是完整的吗？（Incomplete）  ← L2-2 补上的那一问
		//   · 有的都还是"对的"吗？（Stale）      ← L2-1 补上的这一问
		missing, incomplete, stale := s.gaps()
		if len(missing) == 0 && len(incomplete) == 0 && len(stale) == 0 {
			break
		}
	}

	s.lastStats = total
	result.complete(s)
	return result, nil
}

// gaps 一次问清"必需产物还差在哪里"，并保证三份列表**基于同一次快照**。
//
// 分开取三次也能跑，但三份列表可能来自不同的黑板状态 ——
// 于是提醒 Prompt 里会出现"自相矛盾的两句话"（例如既说 render 没产出、
// 又说 render@1 过期了）。这类矛盾会让模型彻底摸不着头脑。
func (s *Supervisor) gaps() (missing []schema.Kind, incomplete, stale []string) {
	return s.missingRequired(), s.incompleteRequired(), s.staleRequired()
}

// complete 把最终结果一次性写全。
//
// 抽出来的理由有两个：
//
//	① **保证多处调用点写法一致**（错误分支、循环结束后各一处）——
//	   少写一处就会漏报一类缺口；
//	② **把"结果由哪些字段组成"收敛到一处** ——
//	   本轮就踩过：改成 `complete()` 之后漏掉了 `Stats`，
//	   被既有用例 `TestSupervisorRoutesThroughAllRoles` 当场抓住
//	   （它断言"用量统计必须被记录"）。
//	   **分散赋值就会漏，这是结构问题，不是细心问题。**
func (r *Result) complete(s *Supervisor) {
	r.Missing, r.Incomplete, r.Stale = s.gaps()
	r.Completed = s.completed()
	r.Stats = s.lastStats
}

// missingRequired 返回**尚未产出**的必需产物。
func (s *Supervisor) missingRequired() []schema.Kind {
	var missing []schema.Kind
	for _, k := range s.required {
		if s.board.Versions(string(k)) == 0 {
			missing = append(missing, k)
		}
	}
	return missing
}

// staleRequired 返回**依据链已经过期**的必需产物。
//
// 这是 **L2-1** 的另一半（另一半在下发时拦，见 `subagent.go` 的 `staleInputs`）：
//
//	下发时拦 → 防止**做**出基于过期上游的东西；
//	完成时查 → 防止**交付**一份基于过期上游的东西。
//
// 两处都不能少。只拦下发的话，编排者完全可能因为
// "上游被重做、而下游没人再派"就宣布完成 —— 它交付的东西已经过期了，
// 而这正是最难发现的情况（没有任何异常，只有一个过时的产物）。
func (s *Supervisor) staleRequired() []string {
	latest := func(key string) (schema.Artifact, bool) {
		a, err := s.board.Get(key)
		if err != nil {
			return schema.Artifact{}, false
		}
		return a, true
	}

	var (
		out  []string
		seen = make(map[string]bool)
	)
	for _, k := range s.required {
		a, err := s.board.Get(string(k))
		if err != nil {
			continue // 完全没产出：由 missingRequired 负责报告
		}
		for _, why := range schema.StaleReasons(a, latest) {
			if seen[why] {
				continue // 同一条依据可能被多个必需产物共享，只报一次
			}
			seen[why] = true
			out = append(out, why)
		}
	}
	return out
}

// incompleteRequired 返回**产出了、但自报留有缺口**的必需产物。
//
// 这是 L2-2 的修复：**"存在"不等于"完整"**。
//
// 实现方式值得注意 —— 编排者**不解码任何 payload**，
// 它只读信封上的 `Shortfall` 字段（由 `schema.NewArtifact` 在产物被造出来的
// 那一刻向产物自己问到的）。
//
// 为什么要绕这一道，而不是让编排者直接去解 `RenderResult`？
//
//	因为那会让编排者重新开始"读业务字段"（Failed / Files / PageIndex）——
//	一旦它开始读字段，下一步它就会想"顺便重排一下"，那就是 A2/A3。
//
//	现在它读到的只是**几句人话**，和读 `Summary` 没有任何区别。
//	这条边界必须守住：**编排者的上下文里只有 prose，没有 schema。**
//
// 这也是本包第一次用"可选能力"：产物**自愿**实现 `schema.Partial`。
// 没有缺口的产物（Research / Outline / Design）什么都不用做。
func (s *Supervisor) incompleteRequired() []string {
	var out []string
	for _, k := range s.required {
		a, err := s.board.Get(string(k))
		if err != nil {
			continue // 完全没产出：由 missingRequired 负责报告
		}
		for _, gap := range a.Shortfall {
			out = append(out, fmt.Sprintf("%s：%s", a.Key, gap))
		}
	}
	return out
}

// completed 按**角色声明顺序**收集已完成产物的引用。
func (s *Supervisor) completed() []schema.Handoff {
	out := make([]schema.Handoff, 0, len(s.subs))
	for _, sub := range s.subs {
		a, err := s.board.Get(string(sub.Kind())) // 取最新版
		if err != nil {
			continue // 没产出过：跳过（缺失会由 Missing 报告）
		}
		out = append(out, schema.HandoffOf(a))
	}
	return out
}

// ─── Prompt ─────────────────────────────────────────────────────────

// taskPrompt 是首轮输入。
func taskPrompt(topic string) string {
	return fmt.Sprintf(`用户需求：%s

请按流程调度各角色，直到产出完整的一套产物。
说明：你不需要向我复述任何内容，直接调用工具即可。`, topic)
}

// reminderPrompt 在"模型认为做完了、但代码发现还缺东西"时使用。
//
// 这就是**终止判断的确定性兜底**：模型可以有自己的判断，
// 但"必需产物齐不齐、完不完整"由代码数了算 —— 与 L1 的 `Deliverable()` 是同一个思想。
//
// ⚠️ 三段检查**分开写**，措辞必须准确。
// 这正是 L1-5 那条教训（反馈要可执行）在编排层的延伸：
//
//	"还没有产出"     → 模型应当**去做**这件事
//	"只完成了一部分" → 模型应当**去补缺口**，而不是重做
//	"依据已经过期"   → 模型应当**按顺序重做**（先上游、后下游）
//
// 如果三件事都写成"还没有产出"，模型会把已经做完的角色再跑一遍 ——
// **一句不准确的反馈，换来一整轮浪费。**
func reminderPrompt(topic string, missing []schema.Kind, incomplete, stale []string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("用户需求：%s\n", topic))

	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for _, k := range missing {
			names = append(names, string(k))
		}
		sb.WriteString(fmt.Sprintf("\n【系统检查】以下必需产物还没有产出：%s\n",
			strings.Join(names, "、")))
	}

	if len(incomplete) > 0 {
		sb.WriteString("\n【系统检查】以下产物**已经产出过**，但只完成了一部分：\n")
		for _, gap := range incomplete {
			sb.WriteString("  - " + gap + "\n")
		}
		sb.WriteString("（请针对缺口补齐，不要把它们整个重做一遍。）\n")
	}

	if len(stale) > 0 {
		sb.WriteString("\n【系统检查】以下产物的**依据已经过期**（上游出了新版本）：\n")
		for _, reason := range stale {
			sb.WriteString("  - " + reason + "\n")
		}
		// 顺序提醒不能省：列表是**可执行的修复顺序**，
		// 照着从上往下做就行；顺序反了会被系统的前置检查拒绝。
		sb.WriteString("（**请按上面的顺序**重新调度 —— 先修上游、再修下游。）\n")
	}

	sb.WriteString("\n请继续调度对应角色。不要解释、不要总结，直接调用工具。")
	return sb.String()
}

// supervisorPrompt 生成编排者的 System Prompt。
//
// 三条纪律：
//
//	① **只描述目标与纪律，不复述各角色的描述** ——
//	   角色描述已经在"工具清单"里了，写第二遍就等着漂移（单一真相来源）。
//	② **不出现任何业务字段名** —— 出现就意味着它在做"逐字段提取重组"（A2/A3）。
//	③ 明确告诉它"正文不在你这里" —— 让它从一开始就不去尝试搬运。
//
// 注意：Prompt 里**可以**描述流程与目标（帮模型少犯错），
// 但"必须"必须由代码强制 —— 这是"Prompt 是建议、代码是保证"的分界线。
func supervisorPrompt(required []schema.Kind) string {
	names := make([]string, 0, len(required))
	for _, k := range required {
		names = append(names, string(k))
	}

	return fmt.Sprintf(`你是「编排者」。你不创作任何内容，也不搬运任何数据。

你的职责只有两件：
  ① 决定下一步该由哪个角色来做 —— 通过调用对应工具派活；
  ② 判断任务是否已经完成。

【本次任务需要产出的东西】
%s
（它们之间的先后顺序，由各工具的「前置条件」约束，系统会强制检查。你不用向我确认。）

【纪律】
- 每次只派一个角色；
- 派活前想清楚：现在缺什么？谁负责产出它？
- 工具的返回值只有「引用 + 摘要」，**正文不在你这里** —— 你拿不到，也不需要；
- 不要替角色写内容，不要复述它们的产物，不要把它们的结果转述给下一个角色；
- 不要询问用户，用户不会回答；
- 当所有必需产出都已存在，直接结束，不要再调用任何工具。`,
		strings.Join(names, " → "))
}

// addStats 累加两次运行的用量。
func addStats(a, b agent.Stats) agent.Stats {
	return agent.Stats{
		Steps:            a.Steps + b.Steps,
		ToolCalls:        a.ToolCalls + b.ToolCalls,
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
	}
}
