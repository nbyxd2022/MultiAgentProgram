// Package observ 提供**统一**的运行时观测。
//
// # 它替代什么
//
//	L0/L1/L2/L3 **曾经**各有一套事件类型（`agent.Event` / 渲染事件 / `graph.Event` …），
//	形状**刻意对齐**（R34 的原话："同一个 Tracer 不加一行就能接住这一层"），
//	**但没有任何东西真的接住了** —— 四处各自 fmt.Printf。
//
//	本包把它们统一成**一种**。⚠️ 是**迁移**，不是并存 ——
//	两套事件形状并存正是我们一路在避免的病（F4：同一个 store 两种写入语义）。
//
//	⭐ **迁移已完成**（R44 图那一层 / R46 agent 那一层 + 四份打印）：
//	  全仓现在只有 `observ.Event` **一种**事件类型，`agent.Event` / `graph.Event` 都已删除。
//
// # 边界（原理二：依赖必须向外）
//
//	本包**不依赖任何监控库**。它只定义两件事：
//
//		「事件长什么样」（本文件）
//		「事件往哪去」（tracer.go 的 `Tracer`）
//
//	具体的后端（文件 / Prometheus / OTel）都是**适配器**，由最外层注入。
//	这与 `llm.Client` / `checkpoint.Store` 是同一条（原则五）。
//
// # 为什么观测要单独一个包（而不是挂在 graph 上）
//
//	因为它**横切所有层**：agent 的 ReAct 循环、图调度、工具调用、模型调用、评测
//	都要发事件。挂在任何一层都会让那一层变成"上游"。
package observ

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ─── 词表：Layer 与 Kind ────────────────────────────────────────────
//
// ⚠️ 这两个是**词表**（代码定义的有限集合）—— 所以**强类型**。
//
// # 判据：什么时候必须强类型
//
//	企业仓库的两处做法不同，而差别给出了判据：
//
//	  · `LimitType` / `LimiterType` → **强类型**（`type X string` + 常量）
//	    因为**它们被用作 map key**（`limiters map[LimitType]Limiter`）
//	    —— 拼错**不会报错**，只会静默地创建一个新键。
//	  · `TraceEvent.Phase` / `Status` → **自由字符串**（取值散落在 20+ 个文件）
//	    当时它们只用来"看"。
//
//	> **强类型的必要性 = 「拼错的后果有多重」。**
//	> 被用作键 / 聚合维度 → 静默分裂 → **必须强类型**。
//
// ⚠️ 而这个判断**会随时间变化**：今天只用来打印的东西，明天可能被拿去聚合。
//	`Phase` 显然属于后者 —— 所以我们的 `Kind` / `Layer` 从一开始就强类型。

// Layer 是事件所属的层。
type Layer string

const (
	LayerAgent Layer = "agent" // ReAct 循环（L0 / L1 / L2 的角色）
	LayerGraph Layer = "graph" // 图运行时（L3）
	LayerTool  Layer = "tool"  // 工具调用
	LayerLLM   Layer = "llm"   // 模型调用
	LayerEval  Layer = "eval"  // 评测（L4）
)

// layerDescs 是层的目录（**同一份数据**同时喂给 Valid 与 AllLayers）。
//
// ⚠️ 与 `kindSpecs` 一样做成目录表，而不是 switch ——
//
//	因为"合法取值"与"清单"必须是同一份数据（否则它们会漂移）。
var layerDescs = map[Layer]string{
	LayerAgent: "ReAct 循环（角色的内部循环）",
	LayerGraph: "图运行时",
	LayerTool:  "工具调用",
	LayerLLM:   "模型调用",
	LayerEval:  "评测",
}

// Kind 是事件类型。
//
// ⚠️ 它**不是**"阶段名"。阶段名（哪个节点）在 `Event.Phase` 里 ——
//
//	那是**数据**（由图的装配决定），不是词表。
//
//	把数据做成词表是错的（每加一个节点都要改枚举）；
//	把词表做成自由字符串是危险的（拼错静默）。**两者的分界就是这条注释。**
type Kind string

const (
	// ── 与 L3 的 `graph.Event` 一一对应（**迁移，不是并存**）──
	KindStart     Kind = "start"     // 开始跑一跳
	KindProduct   Kind = "product"   // 产出了产物（带 ref）
	KindSkipped   Kind = "skipped"   // 一批产物已就绪，被跳过
	KindInterrupt Kind = "interrupt" // 停下来等人
	KindResume    Kind = "resume"    // 人在恢复时给了回答（含重启后重放的）
	KindNotice    Kind = "notice"    // 需要注意、但**不改变执行**的事实
	KindDone      Kind = "done"      // 这一轮全部完成

	// ── L4 新增 ──
	KindLLMCall   Kind = "llm_call"  // 一次模型调用（**聚合层的原料**）
	KindBudget    Kind = "budget"    // 预算的检查 / 记账 / 触顶
	KindEval      Kind = "eval"      // 评测：一个用例的开始 / 结束 / 判定
	KindReconcile Kind = "reconcile" // **对账不符**（见 Reconcile）

	// ── R46 补：工具那一对 ──
	//
	// ⚠️ `LayerTool` 这个**层**早就在词表里，而它的事件类型**不存在** ——
	//	那是"**半套**"（声明了一个层，没有配套的种类）。
	//
	//	它一直没被发现，是因为这两件事**在不同的地方看**：
	//	图那一层不做工具调用（`graph` 只调度节点），而 `agent.Event` 里的
	//	`tool_call` / `tool_result` 是**它自造的词表** ——
	//	直到 R46 做第二批迁移，**两边的词表第一次被放在一起看**，缺口才露出来。
	//
	//	⭐ 教训（已经写进日志）：**声明一个层 / 分类 / 枚举时，
	//	  要检查它有没有配套的"种类"。**
	KindToolCall   Kind = "tool_call"   // 要调用一个工具
	KindToolResult Kind = "tool_result" // 工具返回了结果
)

// Valid 报告这个 Kind 是否在词表里。
//
// 用途：校验**外部输入**（事件可能来自文件回放、或未来的跨进程上报）。
// 脏数据不能进系统 —— 与 `schema.Kind.Valid()` 同一条。
func (k Kind) Valid() bool {
	_, ok := kindSpecs[k]
	return ok
}

// Valid 报告这个 Layer 是否在词表里。
func (l Layer) Valid() bool {
	_, ok := layerDescs[l]
	return ok
}

// kindSpec 描述一种事件类型的**契约**：它必须带什么。
//
// ⚠️ 做成一张目录表（而不是三个 switch），是为了让
//
//	**校验 / 清单 / 错误提示**三处从**同一份数据**派生 ——
//	这与 `graph.InterruptKind` 的 `kindSpecs` 是同一条
//	（F1 的靶子：企业仓库加一种中断类型要改四处手写分派，而四处没有编译期约束）。
//
// # 判据：哪些字段该被约束
//
//	**只约束"缺了它这一类事件就没有意义"的字段** —— 不是"有它更好"的字段。
//
//	· `KindLLMCall` 缺**用量** → 它作为"成本原料"**完全失效** → 必须约束 ✅
//	· `KindLLMCall` 缺**模型名** → 还能用（只是按模型切片时少一维）→ **不约束**
//
//	不这么划线的话，每加一个字段就加一条要求，最后没人能发出一个事件。
type kindSpec struct {
	// NeedsPhase 表示这个事件**必须**带阶段名（哪一个节点/角色）。
	//
	//	哪些不需要：`done` / `notice`（它们说的是"整体"，不属于任何一跳）。
	NeedsPhase bool

	// NeedsReconcile 表示这个事件**必须**带一份对账。
	NeedsReconcile bool

	// NeedsUsage 表示这个事件**必须**带用量（至少一项非零）。
	NeedsUsage bool

	// NeedsTool 表示这个事件**必须**带工具名。
	//
	//	判据与 `NeedsUsage` 完全相同：**缺了它这一类事件就没有意义** ——
	//	一个不知道"调的是哪个工具"的工具事件，聚合的时候什么都答不了
	//	（"哪个工具失败最多"这类问题**一个**都问不出来）。
	NeedsTool bool

	// Desc 是给错误信息用的一句话（**列出合法取值时直接从这里取**）。
	Desc string
}

var kindSpecs = map[Kind]kindSpec{
	KindStart:     {NeedsPhase: true, Desc: "开始跑一跳"},
	KindProduct:   {NeedsPhase: true, Desc: "产出了产物"},
	KindSkipped:   {Desc: "一批产物已就绪被跳过"},
	KindInterrupt: {NeedsPhase: true, Desc: "停下来等人"},
	KindResume:    {NeedsPhase: true, Desc: "人给了一个回答"},
	KindNotice:    {Desc: "需要注意但不改变执行"},
	KindDone:      {Desc: "这一轮完成"},

	KindLLMCall:   {NeedsPhase: true, NeedsUsage: true, Desc: "一次模型调用"},
	KindBudget:    {Desc: "预算检查/记账/触顶"},
	KindEval:      {Desc: "评测的一个用例"},
	KindReconcile: {NeedsPhase: true, NeedsReconcile: true, Desc: "对账不符"},

	KindToolCall:   {NeedsPhase: true, NeedsTool: true, Desc: "要调用一个工具"},
	KindToolResult: {NeedsPhase: true, NeedsTool: true, Desc: "工具返回了结果"},
}

// AllKinds 返回词表里的全部事件类型，**字典序**（**从同一份目录表派生**）。
//
// 它存在的理由与 `graph.JoinInterruptKinds` 相同：
// **校验、清单、错误提示必须是同一份数据**，否则它们会漂移。
//
// ⚠️ **必须排序**：map 的遍历顺序是随机的，不排序会让
//
//	"同一份观测打印两次却不一样" → 于是无法 diff（与 `State.Keys` 同一条）。
//	（第一版写漏了 `sort`，是写测试时发现的 —— 那正好说明这条规矩值得写下来。）
func AllKinds() []Kind {
	out := make([]Kind, 0, len(kindSpecs))
	for k := range kindSpecs {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AllLayers 返回词表里的全部层，**字典序**。
func AllLayers() []Layer {
	out := make([]Layer, 0, len(layerDescs))
	for l := range layerDescs {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ─── 对账 ───────────────────────────────────────────────────────────

// Reconcile 是一次**对账**：声明了多少、实际多少。
//
// # ⚠️ 这一类事件比"成功/失败"值钱得多
//
//	它捕捉的是**静默的错**：流程走完了、**没有报错**，但**内容不对**。
//
//	企业仓库的 `OnImageCountMismatch(pageIndex, expected, actual)` 就是这一类：
//	"这一页声明了 N 张图，实际画了 M 张" —— 而这类指标
//	**只有深刻理解业务的人才会想到要报**。
//
//	这与我们从 L0 追到 L3 的那一类东西是同一条
//	（L2-2 `存在 ≠ 完整`、L3-12 观测说反话），
//	而**它的对策在这里第一次变得具体：把"对账"做成一个可观测量。**
//
// ⚠️ `Declared` / `Actual` 两个数**都要在** —— 一个 `mismatch: true`
//
//	只能告诉你"不对"，**不能告诉你差在哪**，而"发现问题却无从下手"等于没发现。
type Reconcile struct {
	What     string `json:"what"`     // "图片数量" / "页数" / "要点条数"
	Declared int    `json:"declared"` // 声明了多少
	Actual   int    `json:"actual"`   // 实际多少
}

// OK 报告这次对账是否相符。
func (r Reconcile) OK() bool { return r.Declared == r.Actual }

// Diff 是给人和模型看的一句话；相符时返回空串。
//
// ⚠️ 它说**方向与幅度**（"少 2"），不说"不一致" ——
//
//	排查的人拿着"不一致"要去自己算，而算错一次就多绕一圈。
func (r Reconcile) Diff() string {
	if r.OK() {
		return ""
	}
	delta := r.Actual - r.Declared
	dir := "少"
	if delta > 0 {
		dir = "多"
	} else {
		delta = -delta
	}
	return fmt.Sprintf("%s：声明 %d，实际 %d（%s %d）", r.What, r.Declared, r.Actual, dir, delta)
}

// ─── 事件 ───────────────────────────────────────────────────────────

// Event 是一次运行时事件。
//
// # ⚠️ 它同时承载「明细」与「录播」—— 而两者由**不同的出口**消费
//
//	明细（结构 + 数值）→ 聚合 / 看板（体积小，**可采样**）
//	正文（prompt / response）→ JSONL 录播（体积大，**不可关**）
//
// 这不是"设计得不够干净"，而是刻意的：企业仓库的 `TraceEvent` 就是这么做的，
// 而它的对策是**两个出口**（`OnTrace` 可关 / `OnTaskLogTrace` 不可关），
// 不是把结构拆成两个。
//
// > **判据：同一个事件的多个消费者，先问他们的「保留要求」是否相同。**
// > 相同 → 一个出口就够；**不同 → 必须是多个出口**。
//
// # 为什么**没有** `Seq` 字段
//
//	"谁先谁后"是**录播的属性**，不是事件的属性：
//
//		`Recorder` 的顺序 = 切片下标；`JSONLTracer` 的顺序 = **行号**。
//
//	所以一个序号字段是**派生值** —— 而"能从别处算出来的，就不该单独存"
//	（R29/R30 那条原则，L4 第三次应用）。存了就要维护，维护就会漏，
//	漏了就有一个**会说谎的字段**。
type Event struct {
	// RunID 是这一轮的身份（跨进程也一致）。
	RunID string `json:"run_id"`

	// Span 是所属跨度的 id（"render_p3" / "style"）。空表示不属于任何 span。
	//
	// ⚠️ 它与 `Phase` 的差别：`Phase` 回答"哪一个节点"，`Span` 回答
	//	"**这一个节点里的哪一段**"（同一个节点可以开出多个 span，如逐页渲染）。
	Span string `json:"span,omitempty"`

	Layer Layer  `json:"layer"`
	Phase string `json:"phase,omitempty"` // **数据**（节点/角色名），不是词表
	Kind  Kind   `json:"kind"`

	// Err 是失败原因（空表示没有失败）。
	//
	// ⚠️ 它是**字符串**而不是 error —— 因为它要跨进程、要落盘、要能 diff。
	//	（这与 `checkpoint` 里"存的永远是可序列化的东西"是同一条。）
	Err string `json:"err,omitempty"`

	// Detail 是"**这一步发生了什么**"的一句话（可以空）。
	//
	// # ⚠️ 这个字段是**被消费者逼出来的**（R44）
	//
	//	本类型是在**精读参考实现**的基础上设计的（照搬了 `TraceEvent` 的字段清单），
	//	而它**没有**这个位置 —— 直到真的去接 `graph` 的那一刻才发现：
	//	图发的 9 类事件里，有一半携带的是**给人和模型看的一句话**
	//	（"已就绪、跳过：research" / "请确认这份大纲？" / "产出了 outline@1、3 页大纲"）。
	//
	//	> ### 抄一个事件结构，等于抄了一套**信息分布**。
	//	> 而信息分布是**你的系统**的属性，不是那个结构的属性。
	//
	// # 它和 `Err` 的关系（**不该合并**）
	//
	//	Err     "**为什么失败**" —— 只有失败时才有
	//	Detail  "**发生了什么**" —— 总是有（哪怕是成功）
	//
	//	一个事件**可以两者都有**（`Err: "模型超时"` + `Detail: "第 3 次尝试"`）；
	//	合并成一个字段的话，这两个信息会互相盖住。
	//
	// # ⚠️ 而加一个自由文本字段是**有代价的**
	//
	//	它会吸引**本该结构化的东西**住进来（一旦有了"随便写点什么"的地方，
	//	就没人愿意去设计那几个字段了）。
	//
	//	所以这条界线必须写在这里：**能被算出来的，不许写进 `Detail`。**
	//
	//		"第几步" —— 等于数一数 `KindStart` 出现过几次 —— **不要写**。
	//		（`graph.Event` 里那个 `Step` 字段就是这么被删掉的：它是个派生值。）
	Detail string `json:"detail,omitempty"`

	// ── 工具（R46 补）────────────────────────────────────────────────
	//
	// ⚠️ 这两个字段是**在第二批迁移时才被发现的**（`agent.Event` 带着它们），
	//	而它们**分属两侧** —— 这是本包字段命名约定的又一次实战：
	//
	//		`ToolName`  → **元信息**（进 `Redact` 白名单）
	//		`ToolArgs`  → **正文**（与 prompt / response 同一档）

	// ToolName 是工具名（`KindToolCall` / `KindToolResult` 必有）。
	//
	// ⚠️ 它是**元信息**：必须能聚合（"哪个工具用得最多 / 失败最多"）、
	//	也必须被 `Redact` **保留** —— 上游拿不到工具名的话，
	//	那张看板就少了一整个切面。
	ToolName string `json:"tool_name,omitempty"`

	// ToolArgs 是工具调用的参数 —— **正文类**，`Redact` **不保留**。
	//
	// ⚠️ 两条理由都指向"它不是元信息"：
	//	· 它可能**很大**（一整段 HTML / 一份完整 JSON）
	//	· 它可能含**敏感内容**（路径、域名、用户数据）
	//
	//	⭐ 而它进正文档这件事**不需要在这里论证** —— `Redact` 的命名约定
	//	  （`TestRedactPreservesEveryMetaField`）里 "Args" 已经在正文词表里，
	//	  所以**名字一写下来，那一侧就定了**。
	//	  这正是那条约定的价值：**它强迫写字段的人面对"这个字段敏不敏感"。**
	ToolArgs string `json:"tool_args,omitempty"`

	// ── 正文（录播层）────────────────────────────────────────────────
	//
	// ⚠️ 这三个字段是**大且敏感**的：它们必须能被 `Redact()` 去掉。
	//	加新的正文类字段时，**必须同步改 `Redact`** —— 有测试守着（见 event_test.go）。
	SystemPrompt string `json:"system_prompt,omitempty"`
	UserPrompt   string `json:"user_prompt,omitempty"`
	Response     string `json:"response,omitempty"`

	// Model 是这次调用用的模型名。
	//
	// ⚠️ 它是**元信息**（不是正文）：它必须能上游、能被聚合，也必须被
	//	`Redact` **保留**（先故意漏掉它，验证看门人会拦 —— 见 event_test.go）。
	Model string `json:"model,omitempty"`

	// ── 数值（聚合层）────────────────────────────────────────────────
	//
	// ⚠️ 注意这里**没有"总数"** —— 一轮的总量是
	//	"把 `KindLLMCall` 的这几个数加起来"（**派生值**）。
	//
	//	理由不是"省一个字段"，而是 L2-3 的教训：
	//	**一个由多处维护的数字，一定会不一致。**
	//	让它只有一个来源（事件流），总量就永远不会跟明细打架。
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`

	// Duration 是这一跳/这次调用花了多久。
	Duration Duration `json:"duration,omitempty"`

	// Reconcile 非空 = 这是一次**对账不符**（见 Reconcile 的注释）。
	Reconcile *Reconcile `json:"reconcile,omitempty"`
}

// Redact 返回一份**只保留元信息**的副本（去掉正文）。
//
// # ⚠️ 它是**白名单**，不是"清空已知的正文"
//
//	理由是**失败的方向**（这条一直在用）：
//
//		黑名单漏了一个字段 → 正文被发出去 → **泄露**（后果重，且不会报错）
//		白名单漏了一个字段 → 少一个元信息   → 后果轻，**而且容易发现**
//
//	而"显式列出每一个字段"还有一个副作用：**加新字段的人会被迫决定
//	它属于哪一边**（而不是"默认它不敏感"）。
//
// # 谁需要它
//
//	直接来自企业仓库 `emitTrace` 的做法：
//
//		本地 tasklog → **完整**（offline replay 的依据）
//		上游 / 前端 → **脱敏**（"避免把 prompt/response 泄露给前端"）
//
//	⚠️ 两个出口的差别**不只是"开/关"，还有脱敏** —— 这一点我之前只想到了"关"。
func (e Event) Redact() Event {
	return Event{
		RunID: e.RunID,
		Span:  e.Span,
		Layer: e.Layer,
		Phase: e.Phase,
		Kind:  e.Kind,
		Err:   e.Err,
		// ⚠️⚠️ `Detail` **故意不复制** —— 而这里原本写着"元信息（不是正文）"。
		//
		// # 那句话与事实不符（R59）
		//
		//	`Detail` 的实际内容是**工具回执 / 错误原文**：
		//
		//		`KindToolResult`  → 工具返回的那段东西（`supervisor_test.go` 断言它含"未知工具"）
		//		`KindToolFail`    → 错误原文
		//
		//	而工具回执**会带业务数据**（检索片段、文件内容、API 返回）。
		//
		//	> ### 一个"元信息"的标签，被贴在了一个装着正文的字段上。
		//	> 而那条标签是**注释**，不是类型 —— 所以没有任何东西会因为它不成立而报错。
		//
		// # 那为什么不改 `agent` 让它只装"一句话"
		//
		//	因为它**有诊断价值**（"这一步到底发生了什么"），而那个价值
		//	**只对本地录播成立**。两个出口的要求本来就相反（见 `Redact` 的注释）。
		//
		// ⭐ 所以取舍是：**本地留全文，外部清空** —— 而不是"两边都留一句话"
		//   （后者会让本地录播在排查时不够用）。
		ToolName: e.ToolName, // 元信息（可聚合、可切片）
		// ⚠️ `ToolArgs` **故意不复制** —— 它是正文。
		Model:            e.Model,
		PromptTokens:     e.PromptTokens,
		CompletionTokens: e.CompletionTokens,
		Duration:         e.Duration,
		Reconcile:        e.Reconcile,
		// ⚠️ 正文三个字段**故意不复制**。
		//
		// 加新字段时这里最可能漏 —— 所以有一条测试守着（`TestRedactPreservesEveryMetaField`）：
		// 它用反射断言"**除了正文类字段，其余都必须逐字保留**"。
		// 而那条测试本身也是被一次**失败的注入实验**逼出来的（见它的注释）。
	}
}

// Behavior 返回这个事件的**行为指纹** —— 用来比较"两次运行的**过程**是否相同"。
//
// # ⚠️ 为什么需要它：一个事件里装着**两类东西**
//
//	① **行为**：发生了什么 → `Layer` / `Kind` / `Phase` / `Err` / `Detail`
//	② **环境**：它发生在什么条件下 → `RunID` / `Duration` / 数值（token 数…）
//
//	而"确定性"这条承诺**只覆盖 ①** —— 承诺的是
//	**"同样的输入产生同样的行为"，不是"同样的输入产生同样的耗时/同样的 token 数"**。
//
//	> ### 判据：**这个量能不能"由输入决定"？**
//	> 能 → 行为；不能（取决于机器 / 网络 / 时钟 / 模型实现）→ 环境。
//
// # ⚠️ 而"把哪些字段算进来"，错**两个方向都有代价**
//
//	算**多**了（比如带上 `Duration`）→ 两次运行**永远不可能相同** →
//	  判据永远红 → 而**永远红的判据会被关掉**，然后就再没人看它了。
//	算**少**了（比如丢掉 `Detail`）→ 两个**真的不同**的过程被判为相同 →
//	  **假绿** —— 而假绿比假红糟得多：**假红会被修，假绿会被信任。**
//
//	这与"比较的射程要正好覆盖承诺过的稳定性"是同一条（`verifyReplay` 的注释）。
//
// ⚠️ 数值（`PromptTokens` / `CompletionTokens`）被归到**环境**这一侧 ——
//
//	因为真实模型的 token 数**不由输入决定**。
//	⭐ 所以"成本是否确定"是**另一个**判据，需要**另一个**投影 ——
//	  那不是"这个函数漏了字段"，是"两个问题"。
func (e Event) Behavior() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", e.Layer, e.Kind, e.Phase, e.Err, e.Detail)
}

// ─── 自校验 ─────────────────────────────────────────────────────────

// Validate 校验一个事件**自洽**：类型合法，且**带上了它这一类该带的东西**。
//
// # ⚠️ 为什么观测也要校验
//
//	因为它**没有编译期约束**。一个 `KindReconcile` 事件忘了填 `Reconcile`，
//	聚合的时候就少一条对账 —— 而**没有任何东西会报错**。
//
//	**这正是 L4 的根本风险（做漏不会发生任何事），所以这里把它变成"会发生的事"。**
//
// # ⚠️ 但校验是**有代价**的，所以它不在热路径上
//
//	它由**开发期的出口**调用（`Recorder` / 打开调试的 `JSONLTracer`），
//	而不是在每个事件上都跑一遍。判据：
//
//	**"能在源头挡住的问题，不要留给下游的读方"**（`State.Marshal` 的同一条）
//	—— 但源头分两种：生产与开发。生产的源头**没有**预算做这件事。
func (e Event) Validate() error {
	if !e.Kind.Valid() {
		return fmt.Errorf("未知的事件类型 %q。合法取值：%s",
			e.Kind, joinKinds(AllKinds()))
	}
	if !e.Layer.Valid() {
		return fmt.Errorf("未知的层 %q。合法取值：%s",
			e.Layer, joinLayers(AllLayers()))
	}

	spec := kindSpecs[e.Kind]
	switch {
	case spec.NeedsPhase && strings.TrimSpace(e.Phase) == "":
		return fmt.Errorf("%s（%s）必须带阶段名 —— 否则它说的是「某件事发生了」，却不说是哪一步",
			e.Kind, spec.Desc)
	case spec.NeedsReconcile && e.Reconcile == nil:
		return fmt.Errorf("%s（%s）必须带一份对账 —— 只有一个「对账不符」的标记是查不下去的",
			e.Kind, spec.Desc)
	case spec.NeedsUsage && e.PromptTokens == 0 && e.CompletionTokens == 0:
		return fmt.Errorf("%s（%s）必须带用量 —— 不带用量的模型调用事件在聚合时是空的",
			e.Kind, spec.Desc)
	case spec.NeedsTool && strings.TrimSpace(e.ToolName) == "":
		return fmt.Errorf("%s（%s）必须带工具名 —— 不知道调的是哪个工具，"+
			"这条事件在聚合时问不出任何问题", e.Kind, spec.Desc)
	}
	return nil
}

func joinKinds(ks []Kind) string {
	parts := make([]string, 0, len(ks))
	for _, k := range ks {
		parts = append(parts, string(k))
	}
	return strings.Join(parts, " / ")
}

func joinLayers(ls []Layer) string {
	parts := make([]string, 0, len(ls))
	for _, l := range ls {
		parts = append(parts, string(l))
	}
	return strings.Join(parts, " / ")
}

// ─── Duration：一个自己的类型（而不是 time.Duration）───────────────

// Duration 是"这件事花了多久"，单位毫秒。
//
// ⚠️ 为什么不用 `time.Duration`：
//
//	它**会被序列化**，而 `time.Duration` 的 JSON 表示是**纳秒整数**
//	（`400000000` 而不是 `400`）—— 人读不了、diff 也不直观。
//
//	观测量是**给人看的东西**，所以它的表示方式要为"看"服务。
//	（对照：`TraceEvent.DurationMs int64` 也是毫秒 —— 同一个理由。）
//
// 为什么是 int64 而不是 float64：**时间不做浮点运算**，
// 累加时 float64 会引入误差，而误差在"总耗时"这种聚合上会累积。
type Duration int64

// Milliseconds 从 time.Duration 构造。
func Milliseconds(d time.Duration) Duration { return Duration(d.Milliseconds()) }

// String 实现 Stringer（`400ms` / `1.2s`）。
func (d Duration) String() string {
	if d < 1000 {
		return fmt.Sprintf("%dms", int64(d))
	}
	return fmt.Sprintf("%.1fs", float64(d)/1000)
}
