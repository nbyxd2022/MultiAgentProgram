package agents

import (
	"context"
	"fmt"
	"strings"

	"multiagent/internal/agent"
	"multiagent/internal/graph"
	"multiagent/internal/schema"
)

// 本文件把 L2 的 `SubAgent` 适配成 L3 的 `graph.Node`。
//
// # 同一套模式的第三次出现
//
//	① `tools.RenderSlideHTML` → `tool.Tool`    （L0：内核 + 协议适配器）
//	② `SubAgent`              → `tool.Tool`    （L2：subAgentTool）
//	③ `SubAgent`              → `graph.Node`   （L3：本文件）
//
// 三次都是"**内核不变，外面包一层薄适配器**"（原则五）。
// **同一个模式能复用三次，说明它抓对了** —— 而每次的差别只在"适配哪两个协议"。
//
// # 它做的事与 L2 的适配器**刻意相反**
//
//	L2（subAgentTool）：SubAgent 是一**步动作**，编排者（模型）决定什么时候调它。
//	                   于是适配器要**防着**模型：依赖检查、熔断、新鲜度、拒绝自愈。
//
//	L3（SubAgentNode）：图**已经**确定了顺序（边由 `Needs()` 推导）。
//	                   于是那些检查**全都不需要了** —— 顺序是代码保证的，
//	                   而不是"检查"出来的。
//
// > **同一件事，从"检查它有没有错"变成"它不可能错"。**
// > 这是 L3 相对 L2 最大的简化，也是"把约束交给代码"的最终形态。
type SubAgentNode struct {
	sub SubAgent

	// confirm 非空 = 这一跳产出之后**停下来等人确认**。
	confirm string

	confirmKind graph.InterruptKind
}

// SubAgentNodeOptions 是装配参数。
type SubAgentNodeOptions struct {
	// Sub 是被包装的角色。
	Sub SubAgent

	// Confirm 非空时，这一跳产出之后发一次中断等人确认（HITL）。
	//
	// ⚠️ L3 **只做"先产出、再问"** 这一种中断（`confirm_artifact`）。
	//	"先问、再产出"（`ask_user`）在协议层是支持的（`kindSpec.RequiresNote`），
	//	但**没有节点用它** —— 所以适配器不为它预留开关。
	//	（"为用不到的场景预留机制，就是提前制造两套并存。"）
	//
	//	触发条件：当真出现一个"不问就无法开始"的节点时再加。
	Confirm string

	// ConfirmKind 覆盖中断类型；留空用 `KindConfirmArtifact`。
	ConfirmKind graph.InterruptKind
}

// NewSubAgentNode 把 SubAgent 包成 graph.Node。
//
// 构造期校验三件事，与 `NewSubAgentTool` 同一个理由
// （**错配是本程序的 bug，越靠近它的来源发现越便宜**）：
//
//	· Sub 不能为 nil；
//	· Kind 必须合法（它是黑板上的 key，也是图的边所依赖的东西）；
//	· 中断类型若指定了，必须在目录里。
func NewSubAgentNode(opt SubAgentNodeOptions) (*SubAgentNode, error) {
	if opt.Sub == nil {
		return nil, fmt.Errorf("SubAgent 不能为空")
	}
	if !opt.Sub.Kind().Valid() {
		return nil, fmt.Errorf("SubAgent %s 的 kind=%q 不合法", opt.Sub.Name(), opt.Sub.Kind())
	}

	kind := opt.ConfirmKind
	if kind == "" {
		kind = graph.KindConfirmArtifact
	}
	if !kind.Valid() {
		return nil, fmt.Errorf("中断类型 %q 不合法。合法取值：%s",
			kind, graph.JoinInterruptKinds(" / "))
	}

	return &SubAgentNode{sub: opt.Sub, confirm: strings.TrimSpace(opt.Confirm), confirmKind: kind}, nil
}

// ─── graph.Node 的四个声明方法：直通 ────────────────────────────────
//
// 它们**一行都不改**地转给 SubAgent。这不是偷懒 ——
// 而是因为"我是谁 / 我产出什么 / 我依赖什么"这三件事**本来就是角色的属性**，
// 与"谁来调度我"无关（L2 的工具名 = Kind，L3 的黑板 key = Kind，同一个东西）。

func (n *SubAgentNode) Name() string { return n.sub.Name() }

func (n *SubAgentNode) Kind() schema.Kind { return n.sub.Kind() }

func (n *SubAgentNode) Needs() []schema.Kind { return n.sub.Needs() }

// Description 在图拓扑里会打印出来。
//
// ⚠️ 这里**不是**直接转给 `SubAgent.Description()` ——
// 那个字段的用途是"给**模型**看：什么时候该调我"（L2 的工具描述）。
// 而图里没有模型在读它（图是确定性的），所以这里的说明可以更直白。
func (n *SubAgentNode) Description() string {
	if n.confirm == "" {
		return n.sub.Description()
	}
	return n.sub.Description() + "（产出后停下来等人确认）"
}

// ─── 可选能力：必须**穿透适配器** ────────────────────────────────────

// Stats 把用量上报**转发**给被包装的角色。
//
// # ⚠️ 这个方法存在本身就是一条教训
//
//	L2 用"可选能力 + 类型断言"（`StatsReporter`）来避免接口变胖 —— **取舍是对的**
//	（`schema` 的原话："接口要小，能力要可探测"）。
//
//	但它的代价是：**每一层适配器都要记得转发。** 而本适配器当初漏了 ——
//	于是调用方拿到的是一堆 `graph.Node`，断言 `StatsReporter` **全部失败**。
//
//	失败的方向是**静默的**：没有任何报错。它看起来像"这一层没有用量"，
//	而不是"我没把能力接出来"。
//	（07-L3复盘.md L3-3：L2-3 花一整轮统一的口径，在 L3 直接作废；
//	 而 L4 的预算守卫会因此**失去数据来源**。）
//
// 修法不是"下次注意"，是**让漏掉会被测试抓到** ——
// 见 `node_test.go` 的 `TestSubAgentNodeForwardsStatsReporter`。
func (n *SubAgentNode) Stats() agent.Stats {
	if r, ok := n.sub.(StatsReporter); ok {
		return r.Stats()
	}
	// 内层不报用量时返回零值 —— 与 L0/L1/L2 的口径一致
	// （`Renderer.Stats()` 也是零值：它不调用模型）。
	return agent.Stats{}
}

// 编译期断言：`SubAgentNode` 必须**始终**满足这个可选能力。
//
// ⚠️ 注意它不依赖内层是否实现 —— 因为"**能问**"与"**有数**"是两件事：
//
//	调用方的 `s.(agents.StatsReporter)` 不该因为"某个角色恰好不调模型"而失败。
//	（`cmd/l2` 就是这么遍历的；L3 的 `cmd/l3` 现在也能用同一种写法。）
var _ StatsReporter = (*SubAgentNode)(nil)

// ─── 执行 ───────────────────────────────────────────────────────────

// Run 把 NodeInput 翻译成 SubAgentInput，跑一次，再包成 Outcome。
//
// 三处翻译，各自都有讲究：
//
//	① 上游清单：`Handoff` 直接转交（两边用的是同一个协议类型）
//	② 人的回答：`Decision.Note` → **追加进 `Hint`**（见下）
//	③ 产出：`schema.Payload` → `Outcome.Product`（由 Runner 入库，适配器不碰黑板）
func (n *SubAgentNode) Run(ctx context.Context, in graph.NodeInput) (*graph.Outcome, error) {
	product, err := n.sub.Run(ctx, SubAgentInput{
		Topic:    in.Topic,
		Hint:     hintWithAnswer(in),
		Inputs:   in.Inputs,
		Upstream: in.Upstream(),
	})
	if err != nil {
		return nil, err
	}

	out := &graph.Outcome{Product: product}

	// 产出之后停下来等人确认（HITL）。
	//
	// ⚠️ 中断的**位置**很重要：产物已经在 `Outcome.Product` 里了。
	//	于是 `confirm` 之后系统能**自然往下走**（产物已入库 → 被 `CanSkip` 跳过）——
	//	这正是"能 confirm 的中断必须已经产出"这条不变量的来源（见 graph/runner.go）。
	if n.confirm != "" {
		it := in.Interrupt(n.sub.Name(), n.confirmKind, n.confirm)
		out.Interrupt = &it
	}
	return out, nil
}

// hintWithAnswer 把人给的回答**翻译**成 SubAgent 能懂的指令。
//
// # 为什么是"翻译"而不是"传下去"
//
//	`Decision` 是**图层的协议**（谁中断了、意图是什么、ID 是什么）；
//	SubAgent **完全不需要知道 HITL 的存在** —— 它只知道"这一次的额外要求是什么"。
//
// 而 `Decision.Note` 与 `SubAgentInput.Hint` **是同一个东西**：
//
//	Hint 的定义是"**指令**，不是数据搬运"（L2 的注释原话）；
//	Note 的定义是"人要求改什么"（同样是指令）。
//
// 于是适配器把前者译成后者，两边都不必为对方改变形状。
//
// > **适配器的活儿就是翻译，而不是让两边互相认识。**
// > 一旦 SubAgent 开始出现 `if in.Answer.Intent == ...`，这一层就白设了。
func hintWithAnswer(in graph.NodeInput) string {
	hint := strings.TrimSpace(in.Hint)
	if in.Answer == nil {
		return hint
	}

	// 人的原话要**原样带上**（不是复述）—— 它是用户的硬约束，
	// 复述一次就多一个写错的机会（与 L2 的"不让模型复述输入"同一条）。
	note := strings.TrimSpace(in.Answer.Note)
	switch {
	case note == "":
		return hint
	case hint == "":
		return "【人的要求】" + note
	default:
		return hint + "\n【人的要求】" + note
	}
}
