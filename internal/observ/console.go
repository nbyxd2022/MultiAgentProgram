package observ

import (
	"fmt"
	"io"
	"strings"
)

// ConsoleTracer 把事件打成**给人看的一行**。
//
// # ⚠️ 它为什么在 `observ` 里，而不是某个 `cmd`
//
//	因为"把事件打成一行"这件事**被抄了四遍**：
//
//		cmd/l0/main.go   printEvent
//		cmd/l1/main.go   printEvent
//		cmd/l2/main.go   printEvent
//		cmd/l3/main.go   eventPrinter 里那段
//
//	而它**不依赖任何一个命令** —— 它只依赖 `Event`。
//
//	> ### 判据：**这段逻辑如果我在另一个命令里需要它，我会抄一遍吗？**
//	> 会 → 它**不专属**这个命令 → **它住错了地方。**
//
//	⭐ 而"抄一遍"的代价是**具体的**：抄出来的那一份**永远不会被测**。
//	  这与 R45 那条"判据住在哪决定了它能不能被测"是同一件事的两面。
//
// # ⚠️ 它一度是"**半迁移**"，而那个状态在 R46 的同一天就结束了
//
//	它先在 `cmd/l3` 落地（L3 那套 `observ.Tracer` 的一部分），
//	而 `cmd/l0~l2` 当时**还用不了** —— 因为它们发的是 `agent.Event`。
//	第二批迁移做完之后，**五个命令都用它**，四份 `printEvent` 全部删掉。
//
//	⭐ 而那段经历值得留着，因为"半迁移"与"半成品"的**区别是当时写下的**：
//
//		半成品 = 建了**没人用**的东西             ← 危险（让人"以为已经有了"）
//		半迁移 = 建了**有人在用、但还没铺开**的东西  ← 正常，**但必须有登记**
//
//	判据：**有没有一个真实的消费者。**
type ConsoleTracer struct {
	w io.Writer

	// step 是"**已经跑过几跳**"。
	//
	// ⚠️ 它是**派生值**（等于数一数 `KindStart` 出现过几次）——
	//	按 R44/R45 的结论，它**由消费者算**（生产者不存它）。
	//
	//	而它从**哪一刻**开始数，是一个**输入**：`NewConsoleTracerAt`。
	//	（`cmd/l3` 从录播里读回上一批进程跑过的跳数 —— 那是"这一轮的第几跳"，
	//	 而 `NewConsoleTracer` 从 0 开始 —— 那是"这一次进程的第几跳"。）
	step int

	// indent 判定"这一条该不该缩进"；nil = 都不缩进。
	//
	// ⚠️ 它是**调用方给的知识，不是本类型的** —— 本类型不知道"哪个角色是顶层"。
	//
	//	`cmd/l2` 用它显示**嵌套**：编排者（supervisor）的一次 `tool_call` 内部，
	//	会打出子 Agent 自己的若干步 —— 而**那是它的教学点之一**
	//	（"事件是嵌套的，而嵌套是调用关系，不是时间关系"）。
	//
	//	> ### 判据：**谁知道这件事，谁把它传进来。**
	//	> 本类型知道"怎么缩进"（渲染），而"谁该缩进"是**领域知识** ——
	//	写死在 `observ` 里就等于把某一个命令的世界观烧进了观测层。
	indent func(Event) bool
}

// WithIndent 设置"哪些事件该缩进"（**链式**，因为构造处往往是一个表达式）。
//
// 见 `indent` 字段的注释。
func (t *ConsoleTracer) WithIndent(f func(Event) bool) *ConsoleTracer {
	t.indent = f
	return t
}

// NewConsoleTracer 从第 0 跳开始数。
func NewConsoleTracer(w io.Writer) *ConsoleTracer { return &ConsoleTracer{w: w} }

// NewConsoleTracerAt 从 `doneBefore` 接着数 —— 用于**跨进程**的"这一轮第几跳"。
//
// ⚠️ 那个数**不该由本类型自己算**（它手上只有自己见到的事件）——
//
//	调用方从**录播**里读回来（`cmd/l3` 的 `countStepsBefore`）。
//	这与 `budget` 的 `Usage.Elapsed` 是同一个道理：
//	**"这一步花了多久"只有执行的那一层知道；"这一轮跑过几跳"只有录播知道。**
func NewConsoleTracerAt(w io.Writer, doneBefore int) *ConsoleTracer {
	return &ConsoleTracer{w: w, step: doneBefore}
}

// Trace 打一行。
//
// 两种形态：
//
//	[步3][design] start     版面设计者：给每页挑一个能承载内容的版式
//	      （整体事件 —— 不属于任何一跳）
//
// ⚠️ 第二个分支（`Phase == ""`）**不是排版偏好**：它对应的是
//
//	"**说的是整体、不属于任何一跳**"那几类（`skipped` / `notice` / `done`）——
//	而那正好是 `kindSpecs` 里 `NeedsPhase: false` 的那几个 ✅
//
//	**两个地方对"哪些事件不属于任何一跳"的判断必须一致** ——
//	这里用 `Phase == ""` 让它**自动**一致（而不是再列一遍 Kind）。
func (t *ConsoleTracer) Trace(e Event) {
	if e.Kind == KindStart {
		t.step++
	}

	var b strings.Builder

	// ⚠️ 缩进在**最前面** —— 因为它的语义是"这一条属于别人"，而不是"这一条的内容"。
	if t.indent != nil && t.indent(e) {
		b.WriteString("    ")
	}

	if e.Phase == "" {
		// 整体事件（skipped / notice / done）：缩进，不占步号
		b.WriteString("      ")
	} else {
		fmt.Fprintf(&b, "[步%d][%s] ", t.step, e.Phase)
	}

	// `Span` = "这个节点里的哪一段"（`render_p3`）—— 它比步号更细，
	// 所以紧跟在 Phase 后面。（逐页并发渲染时，**光有 Phase 分不出是哪一页**。）
	if e.Span != "" {
		fmt.Fprintf(&b, "(%s) ", e.Span)
	}

	fmt.Fprintf(&b, "%-9s ", e.Kind)

	// ⚠️ 工具事件：**工具名是它的核心信息，而它不在 `Detail` 里** ——
	//	不打出来的话，"调了哪个工具"这一整条信息在这一行上就消失了。
	//
	//	（这不是"美化"：`ToolName` 是结构化字段，而 `ConsoleTracer` 的职责是
	//	 把事件**如实**变成一行 —— 丢掉一个非空字段就不叫如实了。）
	if e.ToolName != "" {
		fmt.Fprintf(&b, "%s  ", e.ToolName)
	}

	// 正文/失败：有才打。
	// `tool_call` 常常只有 `ToolName` + `ToolArgs`（参数是正文，不进 `Detail`）——
	// 所以这一行可能**只有工具名**，那是正常的。
	switch {
	case e.Detail != "":
		b.WriteString(OneLine(e.Detail))
	case e.Err != "":
		b.WriteString("⚠️ " + OneLine(e.Err))
	}

	// ⚠️ 用量：`llm_call` 的核心 —— **它是成本明细在终端上的唯一出口**。
	//	不在这儿打，"这一次花了多少"就只能去录播文件里翻。
	//	（而"累计"由 `Recorder` 算 —— 终端只报这一次的。）
	if e.PromptTokens > 0 || e.CompletionTokens > 0 {
		fmt.Fprintf(&b, "  (prompt %d / completion %d)", e.PromptTokens, e.CompletionTokens)
	}

	fmt.Fprintln(t.w, strings.TrimRight(b.String(), " "))
}

// OneLine 把一段多行文本压成**一行**（供 `ConsoleTracer` 用，也可供别处）。
//
// # ⚠️ 它为什么在 `observ` 里（以及一个诚实的保留）
//
//	它是"打印事件"的渲染细节 —— 那件事归本包管（`ConsoleTracer`）。
//	而 `cmd/l1` 的**报告渲染**也在用它。
//
//	⭐ 也就是说：它其实**比观测更通用** —— 它是"给人看的输出"的公共部分。
//	而"给人看的输出"目前**没有自己的包**，所以它暂住在这里。
//
//	⚠️ **触发条件**：出现**第三处**与事件无关的使用时，把它搬到一个更中性的地方。
//	不现在搬的理由是 R36 那条：**为用不到的场景预留机制**。
//
// # ⚠️ `lineLimit` 从哪来 —— 而它为什么是个"没道理"的数
//
//	它来自 `cmd/l3` 的既有实现（110）。**而 110 不是一条判据，是一个显示上的选择**
//	（终端宽度）。这类数字**没有"正确的值"**，只有"够不够看"，
//	所以它该被写成常量并说明来源，而不是散在 `Printf` 里。
//
// # ⚠️ 空行**不丢**
//
//	`"\n\n"` 会变成 `" /  / "`（而不是被吞掉）—— 看起来没那么干净，而这是**故意的**：
//
//	> ### 观测的第一职责是**如实**，不是**美化**。
//
//	空行也是内容的一部分（它表示"这里有一段分隔"）。悄悄吃掉它，
//	等于让观测**输出一份和原始内容不同的东西**。
//	这与 `Recorder` "**收下**非法事件而不是拒收"、`Redact` 是白名单
//	（而不是"清掉我们记得的敏感字段"）是同一条。
func OneLine(s string) string {
	return Clip(strings.ReplaceAll(s, "\n", " / "), lineLimit)
}

// lineLimit 是 `OneLine` 的截断长度（见它的注释：它是显示选择，不是判据）。
const lineLimit = 110

// Clip 按 **rune** 截断一段文本（太长会把终端冲掉，反而看不清）。
//
// ⚠️ 按 rune 而不是按字节 —— 按字节会把一个汉字切成半个，
//
//	而那在终端里会显示成一个乱码字符（比"截短了"难看得多）。
func Clip(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
