// Command l3 是 L3 的端到端演示：**图驱动 + 中断 + 杀掉进程重启仍能恢复**。
//
// # 怎么演示"杀掉进程"
//
// 这个程序**故意不自己模拟重启** —— 它就是"两个进程"：
//
//	第一次：go run ./cmd/l3                  → 跑到中断，打印问题，进程**正常退出**
//	第二次：go run ./cmd/l3 -answer=confirm  → **全新的进程**，从磁盘恢复，跑到完成
//
// 两次之间内存里什么都没有 —— 唯一的信息来源是 `out/l3/ckpt/` 下的那个文件。
// 这比"在同一个进程里假装重启"可信得多。
//
// # ⚠️ 一个诚实的标注：三个角色是确定性的，不是真实模型
//
// 验收标准是"**中断 → 重启 → 恢复，产出与不中断一致**"。
// 而"一致"这件事，**用真实模型就断言不了** —— 它两次运行本来就会不同。
//
// 所以 research / outline / design 三个节点用**确定性内容**（演示脚手架），
// 而 `renderer` 用的是**真实角色**（`agents.NewRenderer`）——
// 它不调模型、有真副作用，所以最后落盘的 3 个 HTML **是真的**。
//
// 真实角色已经由 L2 端到端验证过（`cmd/l2` 实跑 + 真实模型跑通）；
// 图只是换了一种**驱动方式**，角色本身不参与这次验证。
//
// **⚠️ 这意味着本演示没有验证"三个 LLM 角色在图驱动下也能跑通"。**
// 那是一件明确记为待办的事 —— 触发条件是 L4 接评测集时（那时需要一个稳定的输入）。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"multiagent/internal/agent"
	"multiagent/internal/agents"
	"multiagent/internal/budget"
	"multiagent/internal/checkpoint"
	"multiagent/internal/graph"
	"multiagent/internal/observ"
	"multiagent/internal/schema"
)

const (
	outDir  = "out/l3"
	ckptDir = "out/l3/ckpt"

	defaultTopic = "新能源汽车行业趋势"
	defaultRunID = "l3-demo"

	// interruptNode 是"产出之后停下来等人确认"的那一跳。
	interruptNode = "outline"

	// defaultOutlineHint 是给 outline 的默认额外要求。
	defaultOutlineHint = "控制在 3 页"

	// defaultRunTimeout 是整轮运行的默认时间上限。
	//
	// ⚠️ 它必须**大于**任何单次模型调用的超时（`llm` 的 HTTP client 默认 60s），
	//	否则真正的"单次调用慢"会被误报成"整轮超时"，而那句错误会把人引错方向。
	defaultRunTimeout = 5 * time.Minute

	// defaultMaxSteps 是这一轮的步数预算。
	//
	// ⚠️ 它比"四个节点"宽得多（4）—— 因为节点**允许重做自己**
	//	（`Outcome.Next == 自己`），所以步数**不等于**节点数。
	//	给 10 是"正常跑得完，但跑飞了会被拦"。
	defaultMaxSteps = 10
)

// runTimeout 是 `-timeout` 的值。
//
// ⚠️ 约定与 `InterruptTTL` 一致：**零值 ≠ 不限**，
//
//	「不限」要显式（写负数）。零值在这里就是"真的 0 秒"（立刻超时）。
//
// # ⚠️ 而它**不**与 `budget` 一致 —— 那句注释写错过，而修掉它的过程比它本身重要
//
//	R42 我在这里写下过："约定与 `budget.WithWall` / `InterruptTTL` 一致"。
//	**而本文件当时根本没有导入 `budget`** —— 那句话不是"已经统一"的证据，
//	只是一句**声称**统一的话（而"声称"与"事实"之间的距离，正是 R43 的方法论）。
//
//	⭐ 更根本的是：**这两个数不是同一个判据**（R43 才想清）——
//
//		这里     **墙钟**（含人等待）—— "别让进程永远挂着"（运维视角）
//		budget   **执行时间**（不含等待）—— "别烧太多机器时间"（成本视角）
//
//	**两个判据、两个名字，不该共用一个数。** 把它们写成"一致"，
//	等于把"这一轮跑了多久"这个**含混的词**同时塞进两个地方 ——
//	而 L3 的核心恰恰是"人可以过很久再回来"，所以这两个数**会差得很远**。
var runTimeout = defaultRunTimeout

// budgetSteps 是 `-max-steps` 的值（负数 = 不限）。
//
// ⚠️ 与 `runTimeout` 一样是包级变量 —— 因为 `newGuard` 要读它，而它在 main 之外。
var budgetSteps = defaultMaxSteps

// newGuard 造这一轮的预算守卫。
//
// # ⚠️ 它接的是 `budget` —— 而在 R43 之前，**那个包全仓没有一个导入者**
//
//	`internal/budget` 写完之后，全仓只有 3 处提到它：
//	两处是它**自己的文档注释**，第三处是本文件里一句
//	"约定与 `budget.WithWall` 一致"的注释 —— **而本文件并没有导入它。**
//
//	> ### 一句声称"与 X 一致"的注释，**不是**"与 X 一致"的证据。
//	> 它比没有注释更糟：它让下一个人以为这件事已经统一了。
//
//	所以这里做的是**把话兑现**：真的造一个 `budget.Guard`，真的交给 `Runner`。
//
// # ⚠️ 为什么"不限"也要显式写出来
//
//	`Limits{...}` 的零值是"**该维上限 0**"（fail-closed ✅）——
//	所以五维都写清，而"不限"要写 `NoLimit` / `NoWall`。
//	省掉一行 = 那一维**悄悄变成 0**，而 `Steps: 8` 却看起来像配好了预算。
func newGuard() budget.Guard {
	return budget.New(budget.Limits{
		Steps:            int64(budgetSteps),
		PromptTokens:     budget.NoLimit,
		CompletionTokens: budget.NoLimit,
		CostMilli:        budget.NoLimit,
		Elapsed:          budget.NoWall, // 执行时间不限（墙钟另有 `-timeout`）
	})
}

// runCtx 造一个带**整轮时间上限**的 context —— 两个调用路径共用这一处。
//
// # ⚠️ 这个函数存在的理由：`Runner` 里那个 `ctx.Err()` 检查**曾经没有生产者**
//
//	（R42 的"与参考实现差异审计"照出来的）
//
//	`Runner.drive` 一直有 `if err := ctx.Err(); err != nil { …返回"被 context 打断"… }`，
//	而**全仓 `WithTimeout` / `WithDeadline` = 0 处** —— 也就是说：
//
//		**那个检查永远不会触发。**
//
//	这与 L3 复盘抓到的那 6 个半成品是**同一个形状**：声明 ✅ 实现 ✅ **生产 ❌**。
//	而它比之前那些更隐蔽 —— 因为 `context.Context` 是一个**标准接口**，
//	看起来"当然有人会给它一个 deadline"。
//
// # 参考实现的做法
//
//	它有 **45+ 处 `WithTimeout`**：每次模型调用、每次外部请求、每次存储访问都设。
//	而 `internal/common/context.go` 把 `WithTimeout` 做成了 `Context` 的一等方法。
//
//	我们只有 HTTP client 自己的 60s（`llm.OpenAIClient`）—— 那覆盖了
//	"**单次调用**不返回"，但**不覆盖"整轮跑不完"**：
//
//		一个每次调用都成功、却一直不收敛的运行，
//		只能靠 `MaxSteps`（8 步）兜住 —— 而那是**步数**的界，不是**时间**的界。
//
//	两者都要有：步数防"打转"，时间防"慢"。
func runCtx() (context.Context, context.CancelFunc) {
	if runTimeout < 0 {
		return context.Background(), func() {} // 显式的"不限"
	}
	return context.WithTimeout(context.Background(), runTimeout)
}

// countStepsBefore 数一份录播里**已经发生过**的 `KindStart` 行，即"这一轮跑过几跳"。
//
// # ⚠️ 它是 R44 那个结论的**兑现**
//
//	R44 删掉了 `graph.Event.Step`（派生值），把计数器搬到了消费者这里 ——
//	而搬完发现问题**还在**：步号在恢复后归零（`[步2]` → `[步0]` → `[步1]`）。
//
//	当时的结论是："派生值不是随时可算，是**由收集了全部数据的那一方**算"。
//	**而那句话里的关键词不是"哪一方"，是"有数据"。**
//
//	> ### 兑现之后可以说得更精确：
//	> **派生值随时可算 —— 前提是算它的人手上有数据。**
//	> tracer 算不出来**不是因为它"是消费者"**，是因为它**无状态**。
//
//	⭐ 而录播**就是**那份数据：一个文件 = 一轮运行（跨进程追加）。
//	  打开它、数一遍 `KindStart`，那个数就是"**这一轮**的第几跳" ✅
//
// ⚠️ 读不出来时返回 0（文件不存在 = 这一轮还没跑过）。
//
//	**这里"没有"和"空"是同一件事** —— 而对 `checkpoint.Load` 我们就
//	**必须**区分它们（"不存在"与"读失败"的处置完全不同，混淆会掩盖真问题）。
//
//	> ### 同一个问题（"分不分得清"），两个相反的答案 ——
//	> **判据是"混淆之后会发生什么"**：
//	> 	checkpoint：混淆 → 一个坏文件被当成"没跑过" → **静默重做**（后果重）
//	> 	步号：混淆 → 步号从 0 开始 → 一个数字不对（**后果轻，且看得见**）
func countStepsBefore(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e observ.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // 坏行跳过：历史不该因为一行坏了就整份不算
		}
		if e.Kind == observ.KindStart {
			n++
		}
	}
	return n
}

func main() {
	topic := flag.String("topic", defaultTopic, "主题")
	answer := flag.String("answer", "",
		"带答案继续：confirm / revise / abort（留空 = 只跑一轮，遇到中断就退出）")
	note := flag.String("note", "", "revise / abort 的说明")
	// ⚠️ hint 做成可变的，是为了**能演示 L3-1 的修复**：
	//	"同一个 run 换了输入"必须被拒绝，而一个写死的常量演示不出来。
	hint := flag.String("hint", defaultOutlineHint,
		"给 outline 节点的额外要求（换成别的值再跑同一个 run，会被**拒绝** —— 见 L3-1）")
	verify := flag.Bool("verify", false,
		"**全量重放校验**：跑两遍（含真实的中断恢复），逐字节比较两遍的产物")
	// ⚠️ 一整轮的**时间上限**（负数 = 不限）。
	//
	//	它是**包级变量**而不是局部变量 —— 因为 `runCtx` 要读它，
	//	而 `runCtx` 在 main 之外（**两个调用路径共用同一个来源**）。
	flag.DurationVar(&runTimeout, "timeout", defaultRunTimeout,
		"整轮运行的时间上限（负数 = 不限）；超时后已完成的产物都在磁盘上，可直接重跑")
	// ⚠️ **步数预算** —— 它与 `-timeout` **不是同一件事**，这正是 R43 要说的（见 `runCtx` 的注释）。
	flag.IntVar(&budgetSteps, "max-steps", defaultMaxSteps,
		"步数预算（负数 = 不限）—— 想看预算拦截就设成 2")
	flag.Parse()

	runID := envOr("L3_RUN", defaultRunID)

	// 重放校验是一次**独立的运行**，与下面那条流程无关。
	if *verify {
		fmt.Println("===== L3 全量重放校验：跑两遍，逐字节比较产物 =====")
		if err := verifyReplay(*topic, *hint); err != nil {
			fail("重放校验失败: %v", err)
		}
		return
	}

	fmt.Println("===== L3 Demo：图驱动 + 中断 + 杀掉进程重启仍能恢复 =====")
	fmt.Printf("主题      : %s\n", *topic)
	fmt.Printf("run       : %s\n", runID)
	fmt.Printf("checkpoint: %s\n", filepath.Join(ckptDir, runID+".json"))
	fmt.Printf("产物目录  : %s\n\n", outDir)

	// ① 故障注入：**在 Runner 读 checkpoint 之前**动手。
	if err := injectCheckpointDamage(runID); err != nil {
		fail("%v", err)
	}

	// ② 装配图。
	g, nodes, err := buildGraph(*topic)
	if err != nil {
		fail("建图失败: %v", err)
	}
	fmt.Println(g.Describe())

	// ③ 装配 Runner。
	store, err := checkpoint.NewFileStore(ckptDir)
	if err != nil {
		fail("装配存储失败: %v", err)
	}
	listRuns(store, runID)

	// ★ 录播：**一个文件 = 一轮运行**（跨进程**追加**）。
	//
	//	⚠️ 它不是"又一个日志" —— 它是 R44 那个结论的**兑现装置**（见 `countStepsBefore`）：
	//	"第几跳"这个派生值，在**只看到自己进程**的 tracer 手里算不出来，
	//	而在**手上有完整历史**的人手里就算得出来 —— 历史就是这份文件。
	recPath := filepath.Join(outDir, "events", runID+".jsonl")
	rec, err := observ.NewJSONLTracer(recPath)
	if err != nil {
		fail("打开录播失败: %v", err)
	}
	defer func() {
		// ⚠️ `Tracer.Trace` **不返回 error**（观测失败不该拖垮业务）——
		//	所以这里就是"**谁在乎谁去查**"里的那个"在乎的人"。
		//	不查的话，录播坏掉是**完全静默**的：文件在那儿，只是少了内容。
		if err := rec.Err(); err != nil {
			fmt.Printf("（⚠️ 录播写失败过：%v —— 事件可能不完整）\n", err)
		}
		_ = rec.Close()
	}()
	doneBefore := countStepsBefore(recPath)
	// ⚠️ 数**一次**，用两处 —— 不是洁癖：这个数如果被算两遍，
	//	两遍之间只要发生任何事（比如某个进程刚好在写），它们就会不一致。
	fmt.Printf("事件录播  : %s（这一轮已经跑过 %d 跳）\n\n", recPath, doneBefore)

	// ★ 两个出口：录播 + 终端打印。
	//
	//	顺序按 `Fanout` 的规矩 —— **不可重建的放前面**：
	//	录播是历史（丢了就没了），终端输出重跑一次就有。
	//	⚠️ 对"杀进程"那个注入还有一层好处：录播**先把事件落了盘**，
	//	  才轮到终端打印（而打印里会 `os.Exit`）。
	recorder := eventPrinter(envOr("L3_KILL_AFTER", ""), doneBefore)

	r, err := graph.NewRunner(graph.RunnerOptions{
		Graph:  g,
		Store:  store,
		RunID:  runID,
		Topic:  *topic,
		Hints:  map[string]string{interruptNode: *hint},
		Tracer: observ.Fanout{rec, recorder},
		Guard:  newGuard(),
	})
	if err != nil {
		fail("装配 Runner 失败: %v", err)
	}

	// ④ 跑：带答案就是 Resume，不带就是 Run。
	fmt.Println("---- 开始 ----")
	ctx, cancel := runCtx()
	defer cancel()

	var (
		res    graph.Result
		runErr error
	)
	if strings.TrimSpace(*answer) == "" {
		res, runErr = r.Run(ctx)
	} else {
		d, err := parseDecision(*answer, *note)
		if err != nil {
			fail("%v", err)
		}
		res, runErr = r.Resume(ctx, d)
	}
	if runErr != nil {
		printStop(res)
		printRecovery(runErr)
		printCost(nodes) // ★ 失败也要算钱 —— 失败的运行往往是最贵的（L2-9 的教训）
		fail("运行失败: %v", runErr)
	}

	// ⑤ 打印。
	printStop(res)
	fmt.Print(renderReport(res, runID))
	printCost(nodes)
}

// ─── 装配 ───────────────────────────────────────────────────────────

// buildGraph 装配图，**并把节点原样交出去**。
//
// ⚠️ 第二个返回值不是多余的：`printCost` 需要拿到 `[]graph.Node` 去做
// `StatsReporter` 断言。让这个函数把节点藏起来，调用方就只能再建一遍图 ——
// 而那正是"同一件事有两个产出点"的开端。
func buildGraph(topic string) (*graph.Graph, []graph.Node, error) {
	// 三个确定性角色 + 一个**真实**角色（renderer）。
	roles := []agents.SubAgent{
		&demoRole{
			name: "research", kind: schema.KindResearch,
			desc:  "研究员：围绕主题给出可引用的量化结论（本演示为确定性内容）",
			needs: []schema.Kind{schema.KindTopic},
			build: func(agents.SubAgentInput) schema.Payload { return researchPayload(topic) },
		},
		&demoRole{
			name: "outline", kind: schema.KindOutline,
			desc:  "大纲编写者：把素材组织成 3 页",
			needs: []schema.Kind{schema.KindResearch},
			build: outlinePayload,
		},
		&demoRole{
			name: "design", kind: schema.KindDesign,
			desc:  "版面设计者：给每页挑一个能承载内容的版式",
			needs: []schema.Kind{schema.KindOutline},
			build: designPayload,
		},
		// ★ 真实角色：它不调模型、有真副作用（写盘）。
		agents.NewRenderer(agents.RendererOptions{OutDir: outDir}),
	}

	nodes := make([]graph.Node, 0, len(roles))
	for _, s := range roles {
		opt := agents.SubAgentNodeOptions{Sub: s}
		if s.Name() == interruptNode {
			// 产出之后停下来等人确认（HITL）。
			opt.Confirm = "请确认这份大纲；要改的话告诉我改哪里。"
		}
		n, err := agents.NewSubAgentNode(opt)
		if err != nil {
			return nil, nil, fmt.Errorf("包装 %s 失败: %w", s.Name(), err)
		}
		nodes = append(nodes, n)
	}

	// ⚠️ 注意这里**没有任何一条边是手写的** —— 四个角色的 `Needs()` 就是全部依据。
	g, err := graph.BuildGraph(graph.GraphSpec{
		Nodes:   nodes,
		Presets: []schema.Kind{schema.KindTopic},
	})
	if err != nil {
		return nil, nil, err
	}
	return g, nodes, nil
}

// ─── 确定性内容（演示脚手架）────────────────────────────────────────

func researchPayload(topic string) schema.Research {
	return schema.Research{
		Topic:   topic,
		Queries: []string{topic},
		Findings: []schema.Finding{
			{Claim: "新能源汽车渗透率已达 35%，机构预计 2027 年突破 50%。", Source: "web_search#1"},
			{Claim: "动力电池成本两年内下降 40%，进入 0.5 元/Wh 区间。", Source: "web_search#2"},
			{Claim: "公共充电桩保有量同比增长 46%，但区域分布严重不均。", Source: "web_search#3"},
			{Claim: "价格战使行业平均毛利率从 18% 压缩到 11%。", Source: "web_search#4"},
			{Claim: "出口成为最大增量，海外销量同比翻倍。", Source: "web_search#5"},
		},
	}
}

// outlinePayload 组 3 页大纲。
//
// ⚠️ 它会**真的去取上游全文**（`in.Fetch()`）——
//
//	于是它顺带验证了"适配器把只读取用能力正确转交下去"这件事：
//	R33 把 `SubAgentInput.Blackboard` 收窄成 `schema.Reader` 之后，`Fetch()` 仍然能用。
//
// 而"人的要求"会**改变第 3 页的内容** —— 这是刻意的：
// **revise 的效果必须在最终产物里看得见**，否则演示无法证明"改了"。
func outlinePayload(in agents.SubAgentInput) schema.Payload {
	var claims []string
	if arts, err := in.Fetch(); err == nil {
		for _, a := range arts {
			if r, err := schema.Decode[schema.Research](a); err == nil {
				for _, f := range r.Findings {
					claims = append(claims, f.Claim)
				}
			}
		}
	}
	if len(claims) == 0 {
		// "没有"也是信息，必须如实转达（L2-20 的规矩，这里照做）
		claims = []string{"（上游没有找到任何可用素材，只能写通用结构）"}
	}

	pick := func(i int) string {
		if i < len(claims) {
			return claims[i]
		}
		return claims[len(claims)-1]
	}

	pages := []schema.PageOutline{
		// Index 由代码按位置赋值 —— **不问模型**（L2-20：模型只会在 0/1-based 上写错）
		{Index: 0, Title: "行业现状", Points: []string{pick(0)}},
		{Index: 1, Title: "增长的三重压力", Points: []string{pick(1), pick(2), pick(3)}},
		{Index: 2, Title: "结论与建议", Points: []string{pick(4)}},
	}

	// 人要求改的那一页，内容跟着变（真实模型会重写，这里退化成"把要求写进去"）
	if note := answerNote(in); note != "" {
		pages[2].Points = append([]string{"（按人的要求修改：" + note + "）"}, pages[2].Points...)
	}
	return schema.Outline{Pages: pages}
}

func designPayload(agents.SubAgentInput) schema.Payload {
	return schema.Design{Pages: []schema.PageDesign{
		{PageIndex: 0, Layout: schema.LayoutCover},
		{PageIndex: 1, Layout: schema.LayoutBullets},
		{PageIndex: 2, Layout: schema.LayoutClosing},
	}}
}

// ─── 确定性角色 ─────────────────────────────────────────────────────

// demoRole 是一个**确定性的 SubAgent**：产出固定内容，不调用模型。
//
// 它是"演示脚手架"，不是"能力" —— 所以它走到哪都被标注着。
type demoRole struct {
	name  string
	kind  schema.Kind
	desc  string
	needs []schema.Kind
	build func(in agents.SubAgentInput) schema.Payload
}

func (d *demoRole) Name() string         { return d.name }
func (d *demoRole) Kind() schema.Kind    { return d.kind }
func (d *demoRole) Description() string  { return d.desc }
func (d *demoRole) Needs() []schema.Kind { return d.needs }

func (d *demoRole) Run(_ context.Context, in agents.SubAgentInput) (schema.Payload, error) {
	// 故障注入 ④：让某个节点 panic。
	//
	// ⚠️ 它存在的理由是"**演示不出来的修复等于没验证**"（R36 立的规矩）：
	//	R42 把"节点 panic 翻译成 error"做好了，而一个不会 panic 的图
	//	**演示不出**这件事。
	if envOr("L3_PANIC", "") == d.name {
		panic("故障注入：这个节点故意 panic 了")
	}
	return d.build(in), nil
}

// answerNote 从 Hint 里把"人的要求"抠回来。
//
// ⚠️ 这只是**演示脚手架的取巧**：真实角色不会去解析 Hint，
//
//	它会**把 Hint 作为指令直接读进 Prompt**（那正是 Hint 的设计用途）。
//	这里解析它，只为让"改了之后产物确实不同"肉眼可见。
func answerNote(in agents.SubAgentInput) string {
	const marker = "【人的要求】"
	i := strings.Index(in.Hint, marker)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(in.Hint[i+len(marker):])
}

// ─── 全量重放校验（L3-13）────────────────────────────────────────────

// replayOnce 跑**一整轮**（含一次真实的中断与恢复），返回它的黑板快照。
//
// ⚠️ 每次都用一个**全新的 checkpoint 目录**（先删干净）——
//
//	否则第二次进来会发现"全部已就绪"，一个节点都不跑，
//	于是"比较"退化成**自己跟自己比**：永远相等、永远绿。
//
//	这正是"**看起来在验证、实际什么都没验证**"的典型形状，
//	所以这里宁可多删一次目录，也不省这一行。
//
// ⚠️ 它返回**两样**：黑板（产物层）与事件流（行为层）—— 见 `verifyReplay`。
func replayOnce(topic, hint, tag string) (map[string][]schema.Artifact, []observ.Event, error) {
	dir := filepath.Join(outDir, "verify", tag)
	if err := os.RemoveAll(dir); err != nil {
		return nil, nil, fmt.Errorf("[%s] 清理旧的校验目录失败: %w", tag, err)
	}

	g, _, err := buildGraph(topic)
	if err != nil {
		return nil, nil, err
	}
	store, err := checkpoint.NewFileStore(dir)
	if err != nil {
		return nil, nil, err
	}

	// ★ 用 `Recorder` 收事件 —— 而在此之前它**零消费者**（R45）。
	//
	//	它同时是**开发期出口**：`Trace` 会顺手 `Validate()` 每一个事件，
	//	非法事件记在 `Invalid()` 里（生产出口不校验 —— 那没有预算）。
	rec := observ.NewRecorder()

	r, err := graph.NewRunner(graph.RunnerOptions{
		Graph:  g,
		Store:  store,
		RunID:  "replay",
		Topic:  topic,
		Hints:  map[string]string{interruptNode: hint},
		Tracer: rec,
	})
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := runCtx()
	defer cancel()
	res, err := r.Run(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("[%s] 运行失败: %w", tag, err)
	}
	// 中断是这一轮的**正常形态** —— 走一遍真实的恢复，而不是绕开它。
	// （绕开的话，"恢复路径是否确定"就没被验证到。）
	if res.Status == graph.StatusInterrupted {
		res, err = r.Resume(ctx, graph.Decision{Intent: graph.IntentConfirm})
		if err != nil {
			return nil, nil, fmt.Errorf("[%s] 恢复失败: %w", tag, err)
		}
	}
	if res.Status != graph.StatusCompleted {
		return nil, nil, fmt.Errorf("[%s] 没有跑完（状态 %s，执行了 %v）", tag, res.Status, res.Ran)
	}

	// ⚠️ 顺手查一次"有没有非法事件" —— 这是 `Recorder` 存在的理由之一。
	//
	//	不查的话，一个 `KindProduct` 忘了带 Phase 的事件会**安静地**被收下，
	//	而"校验了但没人看结果"等于没校验（这与 `Store.List()` 曾零消费者是同一类）。
	if bad := rec.Invalid(); len(bad) > 0 {
		return nil, nil, fmt.Errorf("[%s] 发出了 %d 条**不合法**的事件（第一条：%v）",
			tag, len(bad), bad[0])
	}

	fmt.Printf("  第 %s 遍：跑了 %d 跳，黑板 %d 个 key，事件 %d 条\n",
		tag, len(res.Ran), len(res.State.Keys()), len(rec.Events()))
	return res.State.Board, rec.Events(), nil
}

// verifyReplay 是 **L3-13** 的落地：**同一份输入跑两遍，产物必须逐字节一致**。
//
// # 为什么需要它
//
//	"输出稳定"是 L1-6 立下的硬要求。但在此之前，只有两处被验证过：
//
//	  · `Describe()`（图拓扑字符串）—— 有 `TestDescribeIsStable`
//	  · `State.Marshal`（**同一份**状态序列化两次）—— R30 验过
//
//	而 **"两轮完整的执行会不会产生同样的产物"，从来没有被验证过。**
//
//	这才是"确定性"的硬证据，也是 **L4 评测集的前提**：
//	评测要比较两次运行，而一个"两次结果本来就不同"的系统**没法被评测**。
//
// # 比较的射程：只比**黑板**，不比 checkpoint 文件
//
//	快照里有 `run_id` / `created_at` / `updated_at` —— 那些**本来就该不同**。
//
//	比较的射程要**正好覆盖"承诺过的稳定性"**：承诺的是**产物**稳定，
//	不是"文件字节"稳定。
//	射程放宽了会漏掉真问题，收窄了会制造假问题 —— 两头都不行。
//
// # ⚠️ R45 之后它比**两层**，而这是"确定性"这个概念的正确粒度
//
//	① **产物层**：两遍的黑板逐字节一致
//	② **行为层**：两遍的**事件序列**逐字节一致（先投影，见 `behaviorOf`）
//
//	为什么需要第 ② 层？因为第 ① 层**看不见过程**：
//
//		两遍都产出同样的 3 页 → 产物层绿 ✅
//		而其中一遍是"research 跑了两次" → **行为层才看得见**
//
//	> ### 产物一致是"**结果**的确定性"，行为一致是"**过程**的确定性" ——
//	> 而过程确定**解释**了结果确定，反过来说不通。
//	> L4 的评测要复现的是**过程**（"它为什么得出这个结论"），不只是结果。
func verifyReplay(topic, hint string) error {
	boardA, eventsA, err := replayOnce(topic, hint, "a")
	if err != nil {
		return err
	}
	boardB, eventsB, err := replayOnce(topic, hint, "b")
	if err != nil {
		return err
	}

	if err := compareBoards(boardA, boardB); err != nil {
		return err
	}
	if err := compareBehaviors(eventsA, eventsB); err != nil {
		return err
	}
	fmt.Println("\n✅ **两层**都一致：产物逐字节 + **行为序列**逐字节")
	return nil
}

// compareBoards 比**产物层**：两遍的黑板逐字节一致。
func compareBoards(a, b map[string][]schema.Artifact) error {
	rawA, err := json.Marshal(a)
	if err != nil {
		return err
	}
	rawB, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if bytes.Equal(rawA, rawB) {
		fmt.Printf("  ✅ 产物层：%d 个 key / %d 字节，逐字节一致\n", len(a), len(rawA))
		return nil
	}

	// ⚠️ 不一致时必须说清**是哪一个 key** ——
	//	"两坨 JSON 不相等"是没法排查的（而"发现问题却无从下手"等于没发现）。
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		if _, ok := a[k]; !ok {
			return fmt.Errorf("key %q 只在第一遍出现", k)
		}
		if _, ok := b[k]; !ok {
			return fmt.Errorf("key %q 只在第二遍出现", k)
		}
		ra, _ := json.Marshal(a[k])
		rb, _ := json.Marshal(b[k])
		if !bytes.Equal(ra, rb) {
			return fmt.Errorf("key %q 的产物两遍不一致：\n  第一遍 %s\n  第二遍 %s",
				k, clip(string(ra), 300), clip(string(rb), 300))
		}
	}
	return fmt.Errorf("两遍的产物整体不一致（%d vs %d 字节），但没有单个 key 不同 —— 请查 key 集合",
		len(rawA), len(rawB))
}

// behaviorOf 把事件流**投影**成可比较的"行为序列"。
//
// # ⚠️ 为什么必须投影，而不是直接比事件
//
//	因为一个事件里装着**两类东西**：
//
//		① **行为**：发生了什么（`Kind` / `Phase` / `Detail`）
//		② **环境**：它发生在什么条件下（`RunID` / `Duration` / 未来的时间戳）
//
//	而"确定性"这条承诺**只覆盖 ①** —— 承诺的是"**同样的输入产生同样的行为**"，
//	不是"同样的输入产生同样的耗时"。
//
//	> ### 判据：**这个量能不能"由输入决定"？**
//	> 能 → 它是**行为**，该比；不能（它取决于机器/网络/时钟）→ 它是**环境**，不该比。
//
// ⚠️ 而"不该比"的东西**留在事件里是对的** —— 它们有别的用途（性能分析、按 run 归档）。
//
//	> ### **判据与数据分开：数据存全，判据做投影。**
//
//	（这与 `compareBoards` 只比黑板、不比 checkpoint 文件是同一条：
//	 射程要**正好覆盖"承诺过的稳定性"**，宽了漏真问题，窄了造假问题。）
//
// ⚠️ 实测过一件事：**图这一层的事件里目前根本没有 `Duration`**
//
//	（`emit` 不填它 —— 那是 `KindLLMCall` 的事）。
//	所以现在"投影"看起来什么也没做。**而它不是白写的**：
//	等 `LLM` 层接上，`Duration` 就会进事件流，那时**没有投影就会立刻红**。
func behaviorOf(events []observ.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Behavior())
	}
	return out
}

// compareBehaviors 比**行为层**：两遍的事件序列**投影之后**逐字节一致。
//
// ⚠️ 不一致时必须说清**是第几条** —— 与 `compareBoards`"说清是哪一个 key"同一条：
//
//	"两串字符串不相等"是没法排查的（而"发现问题却无从下手"等于没发现）。
func compareBehaviors(a, b []observ.Event) error {
	ba, bb := behaviorOf(a), behaviorOf(b)

	if len(ba) != len(bb) {
		return fmt.Errorf("行为层：事件条数不同 —— 第一遍 %d 条，第二遍 %d 条。"+
			"（**过程**不确定：可能某一边重做了某一跳）", len(ba), len(bb))
	}
	for i := range ba {
		if ba[i] != bb[i] {
			return fmt.Errorf("行为层：第 %d 条事件不一致（注意只比行为，**不比耗时/时间戳**）：\n"+
				"  第一遍 %s\n  第二遍 %s", i+1, ba[i], bb[i])
		}
	}
	fmt.Printf("  ✅ 行为层：%d 条事件，投影后逐字节一致（投影 = 丢掉 run_id / duration 等**环境量**）\n",
		len(ba))
	return nil
}

// clip 同 `observ.Clip` —— 保留它是为了少改几处调用点（见 `oneLine` 的同一条注记）。
func clip(s string, n int) string { return observ.Clip(s, n) }

// ─── 故障注入 ───────────────────────────────────────────────────────

// injectCheckpointDamage 故意把磁盘上的 checkpoint 弄坏。
//
// ⚠️ 它**直接改文件**，绕过了 `checkpoint.Store` —— 这正是要点：
//
//	模拟的不是"我们的代码写坏了"，而是"**磁盘上的东西被外力弄坏了**"
//	（写到一半断电、人为编辑、存储层故障）。
//
// 两个注入对应两类完全不同的失败：
//
//	L3_CORRUPT_CKPT=1  截断文件 → `corrupt`（记录自己写坏了）
//	L3_OLD_VERSION=1   版本改 99 → `from_future`（记录完好，只是本进程读不懂）
func injectCheckpointDamage(runID string) error {
	corrupt := os.Getenv("L3_CORRUPT_CKPT") == "1"
	oldVer := os.Getenv("L3_OLD_VERSION") == "1"
	if !corrupt && !oldVer {
		return nil
	}

	path := filepath.Join(ckptDir, runID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("注入失败：还没有 checkpoint 可改（先不带注入跑一轮）: %w", err)
	}

	switch {
	case corrupt:
		if len(raw) < 2 {
			return fmt.Errorf("注入失败：checkpoint 太短")
		}
		fmt.Printf("\n!! 故障注入：把 checkpoint 截断一半（模拟「写到一半被杀」）\n")
		return os.WriteFile(path, raw[:len(raw)/2], 0o644)
	default:
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("注入失败：checkpoint 不是合法 JSON: %w", err)
		}
		m["version"] = 99
		out, err := json.Marshal(m)
		if err != nil {
			return err
		}
		fmt.Printf("\n!! 故障注入：把 checkpoint 的版本改成 99（模拟「来自更新的版本」）\n")
		return os.WriteFile(path, out, 0o644)
	}
}

// ─── 打印 ───────────────────────────────────────────────────────────

// eventPrinter 把一个事件打成一行；`killAfter` 非空时顺便做"杀进程"注入。
//
// # ⚠️ 它自己**数步**（R44）—— 而这件事值得单独说一段
//
//	打印里那个 `[步2]` 曾经来自 `graph.Event.Step` —— 一个由**生产者**维护的字段。
//
//	而"第几步"是**派生值**：它等于"这是第几个 `KindStart`"。
//
//	> ### 派生值该由**消费者**算 —— 因为它本来就是**为消费者而存在的**。
//	> 生产者维护它，等于**替别人的显示需要**在自己身上加一个计数器：
//	> 那个计数器要自增、要跨函数传参、要保证跟得上 ——
//	> **而它出错时没有任何东西会报错**（打错一个数字，谁会去对？）。
//
//	所以 `graph.Event.Step` 被删掉了，而这个计数器搬到了这里。
//	⚠️ 搬过来之后输出**逐字节不变**（同一个数，只是换了个地方数）——
//	而那条"不变"本身就是一个旁证：**它确实是派生值**（换个地方算，结果一样）。
//
// # ⚠️⚠️ 而搬过来之后，一个**以前看不见的问题**第一次能看见了
//
//	看真实输出（`L3_RUN=l3-r44` 的两个进程）：
//
//		进程 1： [步1][research] …   [步2][outline] …
//		进程 2： [步0][outline] resume …  [步1][design] …  [步2][renderer] …
//
//	**步号在恢复后归零了。**
//
//	生产者数的本来就是"**这次进程**跑了几跳"（`drive` 里的一个局部变量，
//	每次进程重启从 0 开始）；搬到消费者这里还是只会数**自己见到的**。
//
//	> ### 而"这一轮的第几跳"才是任何人真正想问的 —— 它**不该**随进程归零。
//
//	那为什么搬过来还是没算对？因为**我这个消费者也只看到自己的进程**。
//
//	⭐ 而这件事把"派生值"这个概念逼得更精确了：
//
//		**派生值不是"随时可算"，是"由收集了全部数据的那一方算"。**
//
//		"第几跳"要完整的事件流才能算 —— 而那正是 `observ` 早就写在注释里的：
//
//			"`Recorder` 的顺序 = 切片下标；`JSONLTracer` 的顺序 = **行号**。"
//
//		也就是：**"第几跳"是录播的属性**，该由录播出口回答（L4 接 `JSONLTracer`）。
//
//	⚠️ 所以这里保留的是**一个诚实的"本进程第几跳"** ——
//	  它没有说谎，只是**回答的不是那个问题**。真正的答案在录播里。
//
// ⚠️ 杀进程挂在"**下一个节点开始**"的那一刻，而不是目标节点产出时。
//
//	因为 `KindProduct` 是在 `Runner.step` **内部**发出的，而落盘发生在
//	`step` **返回之后**。挂在产出上会在落盘之前就杀掉 —— 那是"最坏时机"，
//	而本次要演示的是"**跑完一个节点并被保存之后**被杀"（更常见的现场）。
func eventPrinter(killAfter string, start int) observ.Tracer {
	// ★ 通用部分（打成一行 + 数步）住进了 `observ.ConsoleTracer`（R46）——
	//	因为那段逻辑**被抄了四遍**，而它不专属任何命令。
	//
	//	这里留下的只有**这个命令特有**的东西：**杀进程注入**。
	console := observ.NewConsoleTracerAt(os.Stdout, start)

	armed := false
	return observ.Fanout{
		console,
		observ.TracerFunc(func(e observ.Event) {
			if killAfter == "" {
				return
			}
			if e.Kind == observ.KindProduct && e.Phase == killAfter {
				armed = true // 目标节点跑完了；下一次有人开始跑就动手
				return
			}
			if armed && e.Kind == observ.KindStart {
				fmt.Printf("\n!! 故障注入：%s 已跑完并落盘 → **直接杀掉进程**（os.Exit(9)）\n", killAfter)
				fmt.Println("   下一次运行会是一个全新的进程，只能从磁盘上的 checkpoint 恢复。")
				os.Exit(9)
			}
		}),
	}
}

// printCost 打印这一轮的 token 用量。
//
// # ⚠️ 它恢复了一条 L3 曾经完全断掉的能力
//
//	L2 的 R25（L2-3）花了一整轮统一成本口径（**必须是"累计"，不是"最近一次"**）。
//	而到了 L3，这条能力**三层全断**（07-L3复盘.md L3-3）：
//
//	  ① `SubAgentNode` 没有转发 `StatsReporter` → 断言**全部失败**
//	  ② `graph.Result` 里也没有 `Stats` 字段
//	  ③ 于是图驱动下的成本**完全不可见**
//
//	第 ① 条的失败是**静默的** —— 它看起来像"这一层没有用量"，
//	而不是"我没把能力接出来"。所以修完转发之后，这里必须**真的用一次**：
//	**一个只有测试在用的能力，会在下一次重构时被当成死代码删掉。**
//
// 口径与 L0/L1/L2 一致：**累计**。
// 但要注意一次运行 = 一个进程 —— 角色实例是每次装配新建的，
// 所以这里显示的是**本次进程**的用量（跨进程累计需要把 Stats 也写进状态，
// 那是 L4 预算守卫要回答的问题）。
func printCost(nodes []graph.Node) {
	type row struct {
		name string
		st   agent.Stats
	}
	rows := make([]row, 0, len(nodes))
	var total agent.Stats

	for _, n := range nodes {
		// 可选能力 —— 与 `cmd/l2` 完全同一种写法。
		st := agent.Stats{}
		if r, ok := n.(agents.StatsReporter); ok {
			st = r.Stats()
		}
		rows = append(rows, row{n.Name(), st})
		total.PromptTokens += st.PromptTokens
		total.CompletionTokens += st.CompletionTokens
	}

	fmt.Println("\n===== 用量（口径：本角色实例累计，即**本次进程**）=====")
	for _, r := range rows {
		if r.st.PromptTokens == 0 && r.st.CompletionTokens == 0 {
			fmt.Printf("  %-10s （无用量 —— 不调用模型）\n", r.name)
			continue
		}
		fmt.Printf("  %-10s prompt %-6d completion %d\n",
			r.name, r.st.PromptTokens, r.st.CompletionTokens)
	}
	fmt.Printf("  %-10s prompt %-6d completion %d\n",
		"合计", total.PromptTokens, total.CompletionTokens)
}

// printRecovery 把 `ResumeErrorKind.Retryable()` 变成一句**给用户的建议**。
//
// # ⚠️ 它存在的理由不是"多打一行字"，而是让一个能力**有出口**
//
//	`Retryable()` 是 R29 专门为"**给调用方做行为分支**"设计的，
//	而在此之前它**没有任何生产调用者**（07-L3复盘.md L3-4）——
//	于是 `from_future`（语义：**重试到新实例**）与 `corrupt`（语义：**查磁盘**）
//	在 CLI 里表现完全一样，都是"打印 + 退出"。
//
//	能力的语义差别必须**在某处被人看见**，否则它只是一份文档。
func printRecovery(err error) {
	var re *graph.ResumeError
	if !errors.As(err, &re) {
		return
	}
	if re.Kind.Retryable() {
		fmt.Printf("（这类失败**可以重试**：%s —— 再跑一次通常就好了）\n", re.Kind)
		return
	}
	fmt.Printf("（这类失败**重试没有用**：%s —— 要重新开始，或者检查磁盘）\n", re.Kind)
}

// listRuns 打印磁盘上**已经存在的** run。
//
// # ⚠️ 它让 `checkpoint.Store.List()` 有了一个真实调用者
//
//	在此之前 `List` 零生产调用 —— 而 R32 又明确拒绝了 `Delete`
//	（"为用不到的场景预留机制，就是提前制造两套并存"）。
//
//	**两者是配套的**：列出才能选择删除。拒绝了后者却留下前者，
//	不是"想过了"，是"标准接口长这样所以我也这么写"（07-L3复盘.md L3-5）。
//
//	而它本来就该在：真实用户的第一个问题恰恰是
//	"**我上次那个跑到一半的，还在吗？**"
func listRuns(store checkpoint.Store, current string) {
	runs, err := store.List()
	if err != nil {
		fmt.Printf("（读不出已有 run 列表: %v）\n", err)
		return
	}
	if len(runs) == 0 {
		fmt.Println("磁盘上还没有任何 run（这是第一次跑）")
		return
	}
	parts := make([]string, 0, len(runs))
	for _, id := range runs {
		if id == current {
			parts = append(parts, id+"（本次）")
			continue
		}
		parts = append(parts, id)
	}
	fmt.Printf("磁盘上的 run：%s\n", strings.Join(parts, "、"))
}

func printStop(res graph.Result) {
	switch res.Status {
	case graph.StatusInterrupted:
		fmt.Printf("\n===== 停在一次中断上等人 =====\n")
		fmt.Printf("问题    ：%s\n", res.Interrupted.Prompt)
		fmt.Printf("中断 ID ：%s\n", res.Interrupted.ID)
		fmt.Printf("（已落盘。这一轮进程可以退出了 —— 状态在磁盘上。）\n")
		fmt.Printf("\n下一步（**新起一个进程**）：\n")
		fmt.Printf("  go run ./cmd/l3 -answer=confirm\n")
		fmt.Printf("  go run ./cmd/l3 -answer=revise -note=\"第 3 页换成建议\"\n")
		fmt.Printf("  go run ./cmd/l3 -answer=abort\n")
	case graph.StatusAborted:
		fmt.Printf("\n===== 已放弃（人选择了「不做了」）=====\n")
		fmt.Printf("已完成的部分**保留在磁盘上**（降级交付 + 说明，而不是整体失败）。\n")
	}
}

func renderReport(res graph.Result, runID string) string {
	var sb strings.Builder

	sb.WriteString("\n===== 结果 =====\n")
	sb.WriteString(fmt.Sprintf("状态        ：%s\n", res.Status))
	if len(res.Ran) == 0 {
		sb.WriteString("这一轮执行  ：（无 —— 全部产物都已就绪）\n")
	} else {
		sb.WriteString(fmt.Sprintf("这一轮执行  ：%s\n", strings.Join(res.Ran, " → ")))
	}

	sb.WriteString("\n黑板（每一次运行都只传 {ref, summary}，正文从不经过调度层）：\n")
	for _, k := range res.State.Keys() {
		latest := res.State.Board[k][len(res.State.Board[k])-1]
		sb.WriteString(fmt.Sprintf("  %-10s %d 版，最新 %s\n", k, res.State.Versions(k), latest.Summary))
	}

	// 页面内容 —— 它是"人的要求到底生效了没有"唯一能一眼看出来的地方
	// （摘要只投影标题，而人的要求落在**要点**上）。
	if vs := res.State.Board["outline"]; len(vs) > 0 {
		if o, err := schema.Decode[schema.Outline](vs[len(vs)-1]); err == nil {
			sb.WriteString("\n页面内容（与最终 HTML 逐字一致 —— renderer 不做二次压缩）：\n")
			for _, p := range o.Pages {
				sb.WriteString(fmt.Sprintf("  第 %d 页「%s」\n", p.Index+1, p.Title))
				for _, pt := range p.Points {
					sb.WriteString(fmt.Sprintf("      - %s\n", pt))
				}
			}
		}
	}

	if res.Status == graph.StatusCompleted {
		if files, ok := renderedFiles(res); ok {
			sb.WriteString("\nHTML 产物（**真写盘** —— renderer 是真实角色）：\n")
			for _, f := range files {
				sb.WriteString(fmt.Sprintf("  %s\n", f))
			}
		}
	}

	sb.WriteString(fmt.Sprintf("\n要看%q里到底存了什么：直接打开 %s\n",
		"状态", filepath.Join(ckptDir, runID+".json")))
	return sb.String()
}

func renderedFiles(res graph.Result) ([]string, bool) {
	vs := res.State.Board["render"]
	if len(vs) == 0 {
		return nil, false
	}
	r, err := schema.Decode[schema.RenderResult](vs[len(vs)-1])
	if err != nil {
		return nil, false
	}
	return r.Files, len(r.Files) > 0
}

// ─── 小工具 ─────────────────────────────────────────────────────────

func parseDecision(answer, note string) (graph.Decision, error) {
	var intent graph.Intent
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "confirm", "c":
		intent = graph.IntentConfirm
	case "revise", "r":
		intent = graph.IntentRevise
	case "abort", "a":
		intent = graph.IntentAbort
	default:
		return graph.Decision{}, fmt.Errorf(
			"-answer 只接受 confirm / revise / abort，收到 %q", answer)
	}
	return graph.Decision{Intent: intent, Note: strings.TrimSpace(note)}, nil
}

// oneLine 是 `observ.OneLine` 的一个**别名**（为了少改几处调用点）。
//
// ⚠️ 它**不是**"两套实现" —— 真正的实现只有一处（`observ`）。
//
//	而这里保留别名而不是全删，是因为调用点在**报告渲染**里（那部分本轮不动）。
//	⚠️ **触发条件**：报告渲染下一轮整理时，把这个别名也删掉。
func oneLine(s string) string { return observ.OneLine(s) }

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n[失败] "+format+"\n", args...)
	os.Exit(1)
}
