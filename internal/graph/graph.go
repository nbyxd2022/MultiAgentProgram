// Package graph 提供 L3 的**图运行时**：节点、边、以及"下一步该谁"的推导。
//
// # 它取代了 L2 的 Supervisor，但只取代了它的一半
//
// L2 的 `Supervisor` 是一个"用 LLM 实现的状态机"：它做的事（按依赖顺序调度）
// **本来就有确定答案**，却交给一个概率模型每一轮重新推一遍。
// 代价实测过：不可测（路由准确率算不出）、不可复现（同输入两次可能不同）、
// 不可恢复（没有"执行到哪了"这个概念）。
//
// 一句话概括这一层做的事：
//
//	**图负责「谁下一步」，节点负责「这一步怎么做」。**
//	图是确定的骨架，节点是骨架里的肌肉（节点内部仍可以是完整的 Agent）。
//
// # 边**不手写** —— 它是推出来的
//
// 对照企业仓库 `v5_html_ppt_agent/graph.go`：
//
//	edges := [][2]string{
//		{compose.START, NodeImage}, {NodeImage, NodeClassify},
//		{NodeClassify, NodePlan},   {NodeClassify, NodeStyle},
//		{NodePlan, NodeRender},     {NodeStyle, NodeRender},
//		{NodeRender, compose.END},
//	}
//
// 那是一张**手写的边表** —— 好读，但每一个依赖都要在这里再写一遍。
// 而我们早就有一份**能被代码检查的**依赖声明：
//
//	research.Needs() = [topic]              outline.Needs() = [research]
//	designer.Needs() = [outline]            renderer.Needs() = [outline, design]
//
// 所以 `BuildGraph` **推导**边，而不是要求调用方手写。换来的是：
//
//	· **加角色只写它的 Needs** —— 图、工具描述、前置检查三处自动同步；
//	· **改依赖只改一处** —— 而手写边表时"改了 Needs 忘了改边"是静默的；
//	· **并行是白拿的** —— 将来加一个只依赖 outline 的"配图"，
//	  它会自动和 designer 排进同一层，不需要谁去说"这两个可以并行"。
//
// # 一个"别人有、我们不需要"的机制
//
// 企业仓库的第一行是：
//
//	compose.RegisterValuesMergeFunc(func(inputs []*PipelineInput) (*PipelineInput, error) {
//		return inputs[0], nil   // 并行分支汇入同一下游时取首条
//	})
//
// 它必须回答"**多路输入怎么合并**"，因为它的节点之间**流动数据**（PipelineInput）。
//
// 我们不需要这个函数 —— 因为我们的节点之间**不流动数据**：上游产出留在黑板上，
// 谁需要谁按 ref 去取（L2 的核心决定，原则四）。
//
//	**流会分叉汇合，黑板不会。**
//
// 这是一个架构决定免费换来的简化：不是我们想到了更好的合并策略，
// 而是**我们没有那个问题**。
package graph

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"multiagent/internal/schema"
	"multiagent/internal/state"
)

// ─── 节点 ───────────────────────────────────────────────────────────

// Node 是图里的一个节点。
//
// ⚠️ 它**不知道图的存在**：没有 `Next()`、没有邻居、没有游标。
// 它只回答两件事："我是谁"和"做完了这一步，下一步我想去哪"。
// 谁在调它、前面是谁、后面是谁 —— 全是图的事。
//
// 接口形状刻意与 `agents.SubAgent` **保持同形**（Name / Kind / Description /
// Needs 四个方法逐字一致），只有 `Run` 的签名不同（它要能中断）。
// 于是"把 SubAgent 包成 Node"是一次**几乎不用翻译**的适配（原则五）。
type Node interface {
	// Name 是节点名（唯一），用于日志、错误信息与调度。
	Name() string

	// Kind 是它产出的产物种类，**同时就是它在黑板上的 key**。
	//
	// 这个"一个节点一个 key"的约定不是巧合：它让"节点产出了什么"与
	// "黑板上的哪个 key"天然对应，于是边可以直接用 Kind 表达。
	Kind() schema.Kind

	// Description 是给人看的一句话说明（图拓扑里会打印它）。
	//
	// 它**不是**给模型看的 Prompt —— 图是确定性的，没有模型来读它。
	// （这是与 L2 `SubAgent.Description()` 的用途差异，虽然名字一样。）
	Description() string

	// Needs 声明我依赖哪些上游产物（在黑板上的 key）。
	//
	// 它是**边的唯一来源**，也是"输入"这个概念存在的唯一位置 ——
	// 见 GraphSpec.Presets。
	Needs() []schema.Kind

	// Run 执行一步。
	//
	// 返回的 Outcome 可以是 nil（表示"没有额外意见，按图的默认调度继续"）。
	Run(ctx context.Context, in NodeInput) (*Outcome, error)
}

// NodeInput 是节点执行时能拿到的一切。
//
// ⚠️ 与 `agents.SubAgentInput` 的结构刻意对齐（Topic / Hint / Inputs / Board），
// 只多了两样**只有图才知道**的东西：`RunID` 与 `Attempt`。
// 它们的存在只为一件事：让节点能派生一个**确定性的中断 ID**。
type NodeInput struct {
	// Topic 是用户主题。它每跳都带，因为它是**用户硬约束**的一部分。
	Topic string

	// Hint 是这一跳的额外要求（原 Supervisor 的 `hint` 参数，现在来自 Runner 配置）。
	//
	// 它是**指令**，不是数据搬运 —— 这个区别很重要（A3 的教训）。
	Hint string

	// Inputs 是上游产物的引用 + 摘要（按 Needs 的顺序）。
	Inputs []schema.Handoff

	// RunID 是这一轮的身份（中断 ID 的一部分）。
	RunID string

	// Attempt 是这一跳的**第几次执行**（由 `State.AttemptFor` 从黑板推出）。
	//
	// ⚠️ 它不是运行期计数器。理由见 `State.AttemptFor` 的注释 ——
	// 计数器会让"恢复后重跑"产生一个新的中断 ID，于是人给的答案永远对不上。
	Attempt int

	// Answer 是**人针对这一跳给出的回答**；不是恢复的那一跳就是 nil。
	//
	// ⚠️ 没有它，`IntentRevise` 就是一个**假动作**：
	// 节点只会被"再跑一遍"，而它根本不知道人要改什么 ——
	// 于是它会用同样的输入产出同样的东西，用户看到"改了跟没改一样"。
	//
	// 所以 `Answer` 不是可选的便利设施，它是**revise 语义能成立的前提**。
	Answer *Decision

	// board 是节点**只读**上游全文的能力。
	//
	// ⚠️ 刻意**未导出**：节点能 `Fetch()`，但**拿不到黑板本身**。
	//
	//	于是"节点不要写黑板"不是一条靠自觉遵守的约定，而是**结构上做不到**：
	//	产出由 `Outcome.Product` 交回，入库与血缘由 Runner 统一做。
	//
	//	这与 `Node` 接口"不知道自己被谁编排"、`Interrupt.Payload` 是字节
	//	（"谁来解它"）是同一条思路：**把不该有的能力从类型上拿掉，
	//	比写一句"请不要这样做"可靠。**
	board *state.Blackboard
}

// Upstream 返回节点的**只读取用能力**（按引用取回上游产物）。
//
// ⚠️ 它返回的是窄接口 `schema.Reader`，**不是** `*state.Blackboard`。
// 于是"节点不要写黑板"不是一条约定，而是**类型上做不到**。
//
// 需要它的人主要是适配器：把 `SubAgent` 包成 `Graph.Node` 时，
// 要能把它转交给 `SubAgentInput.Upstream`。
func (in NodeInput) Upstream() schema.Reader { return in.board }

// Fetch 按输入里的引用取回上游产物的**全文**。
//
// 与 `SubAgentInput.Fetch` 同一个理由（原则四）：把"取全文"变成**显式动作**，
// 才能看出"谁真的需要它"—— 不需要的人上下文自然更小。
func (in NodeInput) Fetch() ([]schema.Artifact, error) {
	if in.board == nil {
		return nil, fmt.Errorf("未提供黑板，无法取回上游产物")
	}
	out := make([]schema.Artifact, 0, len(in.Inputs))
	for _, h := range in.Inputs {
		a, err := in.board.Get(h.Ref)
		if err != nil {
			return nil, fmt.Errorf("取回上游产物 %s 失败: %w", h.Ref, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// Interrupt 构造一次中断，ID 由 (RunID, node, Attempt) **确定性派生**。
//
// 节点因此完全不用操心 ID —— 那是协议层的约定，不是业务的事。
// 而"节点名"要传进来（而不是从 `in` 里取），因为 `NodeInput` 刻意不认识调用它的那个节点。
func (in NodeInput) Interrupt(node string, kind InterruptKind, prompt string) Interrupt {
	return NewInterrupt(in.RunID, node, in.Attempt, kind, prompt)
}

// InputLines 把上游清单渲染成多行文本（供节点的 Prompt 复用）。
func (in NodeInput) InputLines() []string {
	if len(in.Inputs) == 0 {
		return nil
	}
	lines := make([]string, 0, len(in.Inputs))
	for _, h := range in.Inputs {
		lines = append(lines, fmt.Sprintf("- %s：%s", h.Ref, h.Summary))
	}
	return lines
}

// Outcome 是节点执行完后说的"这一步产出了什么，下一步想去哪"。
type Outcome struct {
	// Product 是这一步产出的东西（nil = 这一步不产出产物）。
	//
	// ⚠️ 由 **Runner 统一入库**，节点**不自己写黑板**。三条理由：
	//
	//	① **血缘（BasedOn）是装配的职责，不是业务内容的职责** ——
	//	   这与 L2 的适配器完全一致："我依赖了谁"只有装配层同时握着
	//	   `Artifact` 与**版本号**（节点只拿到 `{ref, summary}` 字符串）。
	//	   让每个节点自己申报，就会有 N 种写法，而且它们根本不知道版本号。
	//
	//	② **单向写入**：黑板只有 Runner 一个写入口，`Put` 的校验与版本分配
	//	   就不会被绕过去（与"一个持久化对象只有一种写入语义"同源）。
	//
	//	③ **可测**：一个纯计算的节点（给定输入 → 返回 `Outcome`）
	//	   是最好测的形状；一旦它自己去写黑板，单测就必须先建一块黑板。
	Product schema.Payload

	// Next 非空 = **强制**跳到这个节点；空 = 按图的默认调度继续。
	//
	// 允许 `Next == 自己`，表示"**这一跳没做好，再来一次**"。
	// 它是"素材不够就再搜一轮"这类重试的**唯一**表达方式 ——
	// 判断逻辑写在**节点自己**里（它最懂"我这步做得够不够好"），
	// 而不是散在图定义里。
	//
	// ⚠️ 但**有界性必须由 Runner 保证**：一个只写"要不要重试"、
	// 不写"最多重试几次"的判据是**无界**的，最坏结果是跑不完
	//（见 06-L3设计.md §10 问题 1）。图这里只管"这条边合不合法"。
	Next string

	// Interrupt 非 nil = 在这里停下来等人（会连同状态一起被持久化）。
	//
	// 它与 `Product` **互相独立**，两种组合都有意义：
	//
	//	Product + Interrupt → "我做完了，你看看再往下"（`confirm_artifact`）
	//	只有 Interrupt      → "我需要你决定，不然做不下去"（`ask_user`）
	//
	// ⚠️ 第一种组合是 `IntentConfirm` 能"自然继续"的前提：
	// 产物已经入库 → 恢复时 `CanSkip` 会把这一跳跳过 → 调度自然往下走。
	// 第二种组合则**必须**从 `Answer` 里拿到信息才走得下去（见 NodeInput.Answer）。
	Interrupt *Interrupt
}

// ─── 边 ─────────────────────────────────────────────────────────────

// Edge 是一条边。
//
// ⚠️ 它**只是个结构**（谁指向谁），不带判断函数。这一点是刻意的：
//
//	边的来源只有一种 —— **从 `Needs()` 推导**。无条件、无例外。
//
// 我原本在设计文档里写过一个 `When func(*State) bool`（"图级条件边"）。
// 动手时发现那会立刻制造**两套条件机制**：`Edge.When` 和 `Outcome.Next`。
// 而"两套并存"正是我们一路在批评的病（企业仓库的隐患 G）。
//
// 判据是：**L3 里真的有"不属于任何节点"的条件判断吗？**
//
//	"素材不够就再搜一轮"  → 是 research 自己的判断 → `Outcome.Next`
//	"主题改了要重做"      → 由 CanSkip / StaleReasons **自动**处理，根本不用条件
//
// 所以答案是"没有"。**为用不到的场景预留机制，就是提前制造两套并存。**
// 等真的出现一个"谁都不该负责"的判断，再加 —— 那时它会有一个具体的形状。
type Edge struct {
	From string
	To   string
}

// ─── 建图 ───────────────────────────────────────────────────────────

// GraphSpec 是建图所需的全部输入。
type GraphSpec struct {
	// Nodes 是全部节点。
	Nodes []Node

	// Presets 是"**图开始之前就已经在黑板上**的产物"（比如 topic）。
	//
	// 它们不被任何节点产出，但可以被 `Needs()` 依赖 —— 于是图里
	// **没有"输入"这种特例，输入就是一个"已经完成的节点"**。
	//
	// ⚠️ 这不是文字游戏，它换来一件实事：
	// 若把输入特殊对待，那么"输入变了要不要重做"就得**单独写一套逻辑**；
	// 而把它当普通产物之后，`StaleReasons` 一路通用 ——
	// **"用户改了主题"不用写任何代码就会让下游全部过期**。
	// （06-L3设计.md §10 问题 2 的答案。）
	Presets []schema.Kind
}

// Graph 是一张**由依赖关系推导出来**的有向无环图。
type Graph struct {
	nodes   map[string]Node
	order   []string // 拓扑序（**确定**：同层按名字排序）
	edges   []Edge   // 推导出来的边（**排序后**，便于打印与断言）
	presets []schema.Kind
}

// BuildGraph 从节点的 `Needs()` 推导出整张图。
//
// 它会拒绝五种情况 —— 每一种都是"能建出来但跑起来会出怪事"的：
//
//	① 节点名为空 / 重复
//	② Kind 不合法（脏数据不能进系统）
//	③ 两个节点产出同一个 Kind（它们会**抢同一个黑板 key**，
//	   版本号交替递增，于是"第几版"这个引用语义直接失效）
//	④ Needs 指向一个既不是节点产出、也不在 Presets 里的东西（**永不就绪的前置**）
//	⑤ 有环（会**永远跑不完**）
//
// ⚠️ 五种都在**建图期**拒绝，而不是留到运行期。
// 理由与 L2 的适配器构造期校验、checkpoint 的写盘前自检是同一条：
// **错配是本程序的 bug，越靠近它的来源发现越便宜。**
func BuildGraph(spec GraphSpec) (*Graph, error) {
	if len(spec.Nodes) == 0 {
		return nil, fmt.Errorf("图里至少要有一个节点")
	}

	nodes := make(map[string]Node, len(spec.Nodes))
	kinds := make(map[schema.Kind]string, len(spec.Nodes)) // kind → 占用它的节点名
	for _, n := range spec.Nodes {
		if n == nil {
			return nil, fmt.Errorf("节点列表里有 nil")
		}
		name := strings.TrimSpace(n.Name())
		if name == "" {
			return nil, fmt.Errorf("有一个节点没有名字")
		}
		if _, dup := nodes[name]; dup {
			return nil, fmt.Errorf("节点名 %q 重复（名字必须是唯一标识）", name)
		}
		if !n.Kind().Valid() {
			return nil, fmt.Errorf("节点 %s 的 kind=%q 不合法", name, n.Kind())
		}
		// ③ 一个 key 只能有一个产出者。
		if owner, dup := kinds[n.Kind()]; dup {
			return nil, fmt.Errorf("节点 %s 与 %s 都产出 %q：它们会抢同一个黑板 key",
				owner, name, n.Kind())
		}
		nodes[name] = n
		kinds[n.Kind()] = name
	}

	presets := make(map[schema.Kind]bool, len(spec.Presets))
	for _, k := range spec.Presets {
		if !k.Valid() {
			return nil, fmt.Errorf("预置输入 %q 不合法", k)
		}
		if owner, clash := kinds[k]; clash {
			return nil, fmt.Errorf(
				"预置输入 %q 同时被节点 %s 产出 —— 它是输入还是产品？两者只能选一个", k, owner)
		}
		presets[k] = true
	}

	// ④ 每一条依赖都必须有着落。
	for _, name := range sortedNames(nodes) {
		for _, dep := range nodes[name].Needs() {
			if !dep.Valid() {
				return nil, fmt.Errorf("节点 %s 依赖了不合法的 kind=%q", name, dep)
			}
			if _, ok := kinds[dep]; ok {
				continue
			}
			if presets[dep] {
				continue
			}
			return nil, fmt.Errorf(
				"节点 %s 依赖 %q，但没有节点产出它、它也不在预置输入里 —— "+
					"这条前置**永远不会就绪**", name, dep)
		}
	}

	// ⑤ 拓扑排序（顺带报环）。
	order, err := topoSort(nodes, presets)
	if err != nil {
		return nil, err
	}

	// 边：依赖 → 产出。排序后存，这样打印与断言都是确定的。
	var edges []Edge
	for _, name := range sortedNames(nodes) {
		for _, dep := range nodes[name].Needs() {
			if presets[dep] {
				continue // 预置输入不是节点，不产生"节点之间的边"
			}
			edges = append(edges, Edge{From: kindOwner(nodes, dep), To: name})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})

	return &Graph{nodes: nodes, order: order, edges: edges, presets: spec.Presets}, nil
}

// ─── 查询 ───────────────────────────────────────────────────────────

// Order 返回拓扑序（**依赖一定排在使用者之前**）。
func (g *Graph) Order() []string { return append([]string(nil), g.order...) }

// Edges 返回推导出来的边（已排序）。
func (g *Graph) Edges() []Edge { return append([]Edge(nil), g.edges...) }

// Presets 返回预置输入。
func (g *Graph) Presets() []schema.Kind { return append([]schema.Kind(nil), g.presets...) }

// Node 按名字取节点。
func (g *Graph) Node(name string) (Node, bool) {
	n, ok := g.nodes[strings.TrimSpace(name)]
	return n, ok
}

// Has 报告某个名字是不是一个已知节点。
func (g *Graph) Has(name string) bool {
	_, ok := g.nodes[strings.TrimSpace(name)]
	return ok
}

// nodeOfKind 按 kind 找产出它的节点。
//
// 结果唯一，由 `BuildGraph` 的第 ③ 条保证（两个节点不许产出同一个 kind）——
// 所以这里返回 ok 而不是列表。
func (g *Graph) nodeOfKind(k schema.Kind) (string, bool) {
	return nodeOfKind(g.nodes, k)
}

// Next 返回"按默认调度，下一个该跑的节点"。
//
// 规则只有一条：
//
//	按拓扑序，找第一个**产物还不能跳过**的节点。
//
// 全部都能跳过 → 返回 ""（这一轮做完了）。
//
// ⚠️ 这里没有游标 —— **位置是从产物算出来的**。
// 这就是"黑板即状态"的完整含义：不需要记"执行到哪了"，
// 因为它等于"哪些做完了、哪些没做"（`State.CanSkip`）。
//
// 它顺带白拿一个性质：**恢复时不需要"从游标往后跑"**，
// 只需要再问一次这个问题 —— 而答案会自动跳过所有已经做完的节点。
func (g *Graph) Next(st *State) string {
	for _, name := range g.order {
		if !st.CanSkip(string(g.nodes[name].Kind())).Skip {
			return name
		}
	}
	return ""
}

// CheckOutcome 校验一个节点返回的 Outcome 是否合法。
//
// 允许 `Next == 自己`（表示"这一跳重做"）—— 这是**唯一**一种
// "没有边支撑"的合法跳转，因为重试不自带依赖关系。
//
// ⚠️ 它只判**结构**，不判**有界性**。
// "最多重试几次"是调度策略，属于 Runner —— 图不该知道"预算"这种东西。
func (g *Graph) CheckOutcome(from string, o *Outcome) error {
	if o == nil || strings.TrimSpace(o.Next) == "" {
		return nil
	}
	next := strings.TrimSpace(o.Next)

	node, ok := g.Node(from)
	if !ok {
		return fmt.Errorf("CheckOutcome 的 from=%q 不是一个已知节点（调用方的 bug）", from)
	}
	if next == from {
		return nil // 重做自己
	}
	if !g.Has(next) {
		return fmt.Errorf("节点 %s 想跳到 %q，但它不是一个已知节点。已知节点：%s",
			from, next, strings.Join(g.order, " / "))
	}
	for _, e := range g.edges {
		if e.From == from && e.To == next {
			return nil
		}
	}

	// 错误信息要给出**可执行的修法**（L1-5）：把 "from 的产物" 加进 "next 的 Needs"，
	// 边就会在下次建图时自动出现 —— 这正是"边由 Needs 推导"给的好处：
	// **修法就是改声明，不用去另一个地方再改一条边。**
	return fmt.Errorf(
		"节点 %s 想跳到 %q，但图里没有这条依赖边 —— %q 并不需要 %q 的产出。"+
			"要允许它，请让 %s.Needs() 里包含 %q（边会自动出现）；或者在节点内部把这次跳转改成重做自己",
		from, next, next, from, next, node.Kind())
}

// Layers 返回**分层结果**：同一层内的节点互不依赖，因此理论上可以并行。
//
// 它存在的意义不是"现在就要并行"，而是让一个**未来的问题变得可见**：
//
//	L2 复盘里我说过"加一个只依赖 outline 的配图角色，它会自动和 designer
//	排成同一层"。`Layers()` 就是那句话的**可验证形式** ——
//	它把"哪里可以并行"从口头承诺变成一行输出。
//
// # ⚠️ 它目前**只是候选** —— `Runner` 还没有并行
//
//	`Runner.drive` 是一个 `for` 循环，**一次只跑一个节点**。
//	`Layers()` 唯一的生产调用者是 `Describe()`（打印）。
//
//	这一点必须写在类型旁边，不能只写在复盘文档里：
//	**一个看起来"已经准备好了"的接口，会让人以为只差一个开关。**
//	那是谎言 —— 兑现并行要付的账单见 07-L3复盘.md §4.4
//	（有序交付 / 失败隔离 / 合并语义 / 早渲染的协调机制）。
//
//	触发条件：出现一个**真的需要它**的场景（节点数 × 单节点耗时 > 体验阈值）。
//
// 层号 = max(各依赖的层号) + 1；预置输入算作第 -1 层（不参与）。
func (g *Graph) Layers() [][]string {
	layer := make(map[string]int, len(g.nodes))
	for _, name := range g.order { // 拓扑序保证依赖先算
		max := -1
		for _, dep := range g.nodes[name].Needs() {
			owner, ok := g.nodeOfKind(dep)
			if !ok {
				continue // 预置输入
			}
			if layer[owner] > max {
				max = layer[owner]
			}
		}
		layer[name] = max + 1
	}

	depth := 0
	for _, l := range layer {
		if l+1 > depth {
			depth = l + 1
		}
	}
	out := make([][]string, depth)
	for name, l := range layer {
		out[l] = append(out[l], name)
	}
	for i := range out {
		sort.Strings(out[i]) // 层内也排序 → 打印稳定
	}
	return out
}

// Describe 渲染一份**可打印的图拓扑**（教学计划的产出物之一）。
//
// 它的用途不是"好看"，而是把三个通常只存在于脑子里的东西变成可读的：
//
//	① 依赖是从哪来的（每一条边都标着它由谁的 Needs() 推导）
//	② 调度顺序是什么（拓扑序 —— 也就是**不再由模型决定**的那个顺序）
//	③ 哪里本来可以并行（Layers 里长度 > 1 的层）
func (g *Graph) Describe() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("图拓扑 · %d 个节点 · %d 条边\n", len(g.nodes), len(g.edges)))

	if len(g.presets) > 0 {
		names := make([]string, 0, len(g.presets))
		for _, k := range g.presets {
			names = append(names, string(k))
		}
		sort.Strings(names)
		sb.WriteString(fmt.Sprintf("\n预置输入（开跑前已在黑板上）：%s\n", strings.Join(names, "、")))
	}

	sb.WriteString("\n依赖边（**由 Needs() 推导，不是手写的**）：\n")
	if len(g.edges) == 0 {
		sb.WriteString("  （没有 —— 所有前置都来自预置输入）\n")
	}
	for _, e := range g.edges {
		sb.WriteString(fmt.Sprintf("  %-10s ──▶ %s\n", e.From, e.To))
	}

	sb.WriteString("\n调度顺序（拓扑序，同层按名字排序 —— **确定**）：\n  ")
	sb.WriteString(strings.Join(g.order, " → "))
	sb.WriteString("\n")

	sb.WriteString("\n节点：\n")
	for i, name := range g.order {
		n := g.nodes[name]
		need := "无前置"
		if len(n.Needs()) > 0 {
			parts := make([]string, 0, len(n.Needs()))
			for _, k := range n.Needs() {
				parts = append(parts, string(k))
			}
			need = "前置：" + strings.Join(parts, "、")
		}
		sb.WriteString(fmt.Sprintf("  %d. %-10s %s\n        %s\n",
			i+1, name, need, n.Description()))
	}

	layers := g.Layers()
	sb.WriteString("\n分层（同层互不依赖 = 可并行的候选）：\n")
	for i, l := range layers {
		mark := ""
		if len(l) > 1 {
			mark = "   ← 可并行"
		}
		sb.WriteString(fmt.Sprintf("  第 %d 层：%s%s\n", i, strings.Join(l, " / "), mark))
	}
	return sb.String()
}

// ─── 内部：拓扑排序 ─────────────────────────────────────────────────

// topoSort 用**后序遍历**求拓扑序，顺带报出环上的节点。
//
// 为什么是后序？因为它天然给出"**依赖排在使用者之前**"——
// 与 `schema.StaleReasons` 用的是同一个技巧，换来的也是同一个东西：
// **输出顺序本身就是可执行的顺序**。
//
// 确定性靠两处排序保证（map 的遍历顺序是随机的）：
//
//	· 起点按名字排序
//	· 每个节点的依赖列表也按名字排序
//
// 与 L1-6 那条"输出必须稳定"是同一条：不稳定就无法 diff、无法做 golden 测试。
func topoSort(nodes map[string]Node, presets map[schema.Kind]bool) ([]string, error) {
	const (
		white = iota // 没访问过
		gray         // 正在访问（在栈上）
		black        // 访问完了
	)
	color := make(map[string]int, len(nodes))
	var (
		order []string
		stack []string
	)

	var visit func(name string) error
	visit = func(name string) error {
		switch color[name] {
		case black:
			return nil
		case gray:
			return fmt.Errorf("图里有环：%s", strings.Join(cyclePath(stack, name), " → "))
		}
		color[name] = gray
		stack = append(stack, name)

		deps := nodeDeps(nodes[name])
		for _, dep := range deps {
			owner, ok := nodeOfKind(nodes, dep)
			if !ok {
				continue // 预置输入，不是节点
			}
			if err := visit(owner); err != nil {
				return err
			}
		}

		stack = stack[:len(stack)-1]
		color[name] = black
		order = append(order, name)
		return nil
	}

	for _, name := range sortedNames(nodes) {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// cyclePath 从 DFS 栈里截出环的那一段（`a → b → c → a`）。
//
// 报环时给出**环上的节点**，而不是只说"有环"：
// 排查的人需要知道的是"哪几个节点互相咬住了"，而不是"存在环"这个事实。
// （与"拒绝必须可自愈"同一条 —— 只说不行，等于让调用方自己去找。）
func cyclePath(stack []string, back string) []string {
	for i, name := range stack {
		if name == back {
			return append(append([]string(nil), stack[i:]...), back)
		}
	}
	return append(append([]string(nil), stack...), back)
}

// kindOwner 返回产出某个 kind 的节点名（`BuildGraph` 已保证它存在）。
func kindOwner(nodes map[string]Node, k schema.Kind) string {
	if name, ok := nodeOfKind(nodes, k); ok {
		return name
	}
	return string(k) // 理论上到不了这里
}

func nodeOfKind(nodes map[string]Node, k schema.Kind) (string, bool) {
	for name, n := range nodes {
		if n.Kind() == k {
			return name, true
		}
	}
	return "", false
}

func nodeDeps(n Node) []schema.Kind {
	deps := append([]schema.Kind(nil), n.Needs()...)
	// 排序：让遍历顺序确定（map 与调用方顺序都不可信）
	sort.Slice(deps, func(i, j int) bool { return deps[i] < deps[j] })
	return deps
}

func sortedNames(nodes map[string]Node) []string {
	out := make([]string, 0, len(nodes))
	for name := range nodes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
