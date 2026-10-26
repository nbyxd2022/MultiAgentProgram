// Command l2 是 L2 阶段的 Demo：**Supervisor 编排四个角色，端到端产出 3 页 PPT**。
//
// 运行方式（离线，无需任何密钥）：
//
//	go run ./cmd/l2
//	go run ./cmd/l2 新能源汽车行业趋势
//
// 接真实模型：
//
//	$env:MULTIAGENT_LLM="openai"; $env:OPENAI_API_KEY="sk-xxx"; go run ./cmd/l2
//
// 故障注入（用来验证"顺序约束由代码强制、而不是 Prompt 建议"）：
//
//	$env:L2_SKIP_ORDER="1"; go run ./cmd/l2
//
// ─────────────────────────────────────────────────────────────────────
//
// # 这条链与 L0 / L1 的关系
//
//	L0：一个循环 + 工具，一条路走到黑。
//	L1：多了「评审 → 返工」闭环，产物不再直接交付。
//	L2：多了**编排** —— 把一件大事拆成四个角色，每个只做自己那一段，
//	    彼此之间**只传引用、不传正文**（共享黑板）。
//
// # 四个角色的分工，完全由代码约束表达
//
//	research  需要 []                 → 产出 research（唯一联网）
//	outline   需要 [research]         → 产出 outline（第一个有上游依赖）
//	designer  需要 [outline]          → 产出 design（枚举约束）
//	renderer  需要 [outline, design]  → 产出 render（**不调模型**、唯一的副作用）
//
// 于是"先搜索、再大纲、再设计、最后渲染"这条顺序，
// **想违反也违反不了** —— 违反会被 `Needs()` 的检查直接拒绝。这正是 P3 的治本。
//
// # 对照企业仓库
//
// 它的 `GetAgent()` 是一个长长的装配函数：8 个子 Agent 各写一遍
// "构造 + 包装错误 + append"，外围还顺手配置了 4 个全局可观测性 handler。
// 装配顺序在这里没有语义 —— 谁能被调、以什么顺序，全靠每个 Agent 内部的 Prompt 与 transfer 配置，
// **装配点看不出任何依赖关系**。
//
// 我们这里相反：`subAgents` 这个切片是**声明式**的，
// 每个角色把"我需要什么"写在 `Needs()` 里，`NewSupervisor` 在**构造期**就校验
// "kind 唯一"与"必需产物有角色负责" —— 配置错了当场就炸，
// 而不是等某次线上跑到一半才发现某个角色永远调不通。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"multiagent/internal/agent"
	"multiagent/internal/agents"
	"multiagent/internal/boot"
	"multiagent/internal/llm"
	"multiagent/internal/message"
	"multiagent/internal/observ"
	"multiagent/internal/schema"
	"multiagent/internal/state"
	"multiagent/internal/tools"
)

// outDir 是本阶段的输出目录。
//
// 特意与 L0/L1 分开（它们写 `out/`）：**同名文件互相覆盖**是 P10 那个技术债，
// L2 还没有版本化能力，能做的就是先把目录隔离掉。
const outDir = "out/l2"

// 四个角色在编排者眼里的**工具名**，同时也是它们产物在黑板上 key。
//
// 工具名 = Kind，这是 `subAgentTool` 定的规矩：**路由的"合法值集合"就是工具名集合**，
// 而工具名又必须过 `schema.Kind.Valid()` —— 两层校验，天然挡住"路由到不存在的 Agent"。
const (
	nameResearch = "research"
	nameOutline  = "outline"
	nameDesigner = "design"
	nameRenderer = "render"
)

// 各角色提交出口的工具名（离线 Mock 靠它识别"我现在是谁"）。
const (
	submitResearch = "submit_research"
	submitOutline  = "submit_outline"
	submitDesign   = "submit_design"
)

func main() {
	topic := "新能源汽车行业趋势"
	if len(os.Args) > 1 {
		topic = strings.Join(os.Args[1:], " ")
	}
	ctx := context.Background()

	// ─────────────────────────────────────────────────────────────
	// 组合根（Composition Root）：**只有这里知道全部实现**
	//
	// 业务包（agents / tools）里没有任何一处"决定用哪个实现"。
	// 这是原则五在装配层的落地：越具体的东西越易变，所以它们只能出现在最外层。
	// ─────────────────────────────────────────────────────────────
	board := state.NewBlackboard()

	// 一个客户端扮演所有角色 —— 下面有详细解释为什么这样可行。
	// 传黑板进去只是为了**故障注入**能判断"时机到了没"（见 newRoleMock 的说明）。
	client := boot.NewClient(newRoleMock(board))

	subAgents := []agents.SubAgent{
		// 检索工具**显式**指定（`ResearchOptions.Search` 是可注入端口）。
		//
		// 虽然留空也有默认值，但装配点应当把"用了哪个实现"写出来 ——
		// 这样换真实搜索服务时，改的就是这一行，而不是去翻研究员的默认值。
		maybeFailingSubAgent(must(agents.NewResearchAgent(agents.ResearchOptions{
			Client: client,
			Search: tools.NewWebSearch(),
			Tracer: observ.NewConsoleTracer(os.Stdout).
				WithIndent(func(e observ.Event) bool { return e.Phase != "supervisor" }),
		}))),
		maybeFailingSubAgent(must(agents.NewOutlineAgent(agents.OutlineOptions{
			Client: client,
			Tracer: observ.NewConsoleTracer(os.Stdout).
				WithIndent(func(e observ.Event) bool { return e.Phase != "supervisor" }),
		}))),
		maybeFailingSubAgent(must(agents.NewDesigner(agents.DesignerOptions{
			Client: client,
			Tracer: observ.NewConsoleTracer(os.Stdout).
				WithIndent(func(e observ.Event) bool { return e.Phase != "supervisor" }),
		}))),
		// ⚠️ 渲染器**不需要 client** —— 它不调模型（见 R22）。
		agents.NewRenderer(agents.RendererOptions{
			// OutDir 只在 Renderer 留空时才起作用（默认实现的输出目录）。
			OutDir: outDir,
			// 故障注入：`L2_RENDER_FAIL=<标题片段>` 时，命中的页**必定**渲染失败。
			// 未开启时返回 nil，走默认实现。
			Renderer: maybeFailingRenderer(outDir),
			Tracer:   observ.NewConsoleTracer(os.Stdout),
		}),
	}

	supervisor := must(agents.NewSupervisor(agents.SupervisorOptions{
		Client:    client,
		Board:     board,
		SubAgents: subAgents,

		// Required 是**终止判断的确定性兜底**（原则一）：
		// 模型可以有自己的判断，但"必需产物齐没齐"由代码数了算。
		Required: []schema.Kind{
			schema.KindResearch, schema.KindOutline, schema.KindDesign, schema.KindRender,
		},
		Tracer: observ.NewConsoleTracer(os.Stdout).
			WithIndent(func(e observ.Event) bool { return e.Phase != "supervisor" }),
	}))

	// 用 Describe 而不是 ClientName：它会连**生效的超时**一起打出来。
	// 超时是最隐蔽的一类参数 —— L2-9 的真实运行里，
	// 屏幕上只有一句 "context deadline exceeded"，人根本不知道等了多久、能不能调。
	printHeader(topic, boot.Describe(client))

	// ─────────────────────────────────────────────────────────────
	// 一次编排
	// ─────────────────────────────────────────────────────────────
	result, runErr := supervisor.Run(ctx, topic)
	if runErr != nil {
		fmt.Printf("\n[编排异常] %v\n", runErr)
	}

	printBoard(board)
	printDeliver(board, result)
	printCost(supervisor.Stats(), subAgents)

	if runErr != nil {
		os.Exit(1)
	}
}

// ─── 打印 ───────────────────────────────────────────────────────────

func printHeader(topic, model string) {
	fmt.Println("===== L2 Demo：Supervisor 编排四个角色 =====")
	fmt.Printf("主题     : %s\n", topic)
	fmt.Printf("模型     : %s\n", model)
	fmt.Printf("角色     : research → outline → designer → renderer（依赖由 Needs() 强制）\n")
	fmt.Printf("产物目录 : %s\n", outDir)
	if os.Getenv("L2_SKIP_ORDER") == "1" {
		fmt.Println("故障注入 : 已开启 —— 编排者会**故意先派 designer**（它的前置还没就绪）")
	}
	if needle := strings.TrimSpace(os.Getenv("L2_RENDER_FAIL")); needle != "" {
		fmt.Printf("故障注入 : 已开启 —— 标题含 %q 的那一页**必定渲染失败**（验证「存在 ≠ 完整」）\n", needle)
	}
	if os.Getenv("L2_REDO") != "" {
		fmt.Println("故障注入 : 已开启 —— 渲染完之后**再重做一次 outline**（验证「依据过期」）")
	}
	if role := strings.TrimSpace(os.Getenv("L2_FAIL_ROLE")); role != "" {
		fmt.Printf("故障注入 : 已开启 —— 角色 %q 必定失败（验证「失败熔断」）\n", role)
	}
	fmt.Println("-----------------------------------------------------")
}

// printEvent 把事件按角色缩进打印。
//
// 这里能看出 L2 与 L0/L1 的一个差别：**事件是嵌套的**。
// 编排者的一次 tool_call 内部，会打印出子 Agent 自己的若干步。
// ⚠️ 这里**曾经有一个 `printEvent`**（R46 删掉了）——
//
//	而它里面那条**缩进规则**没有被丢掉，而是**搬到了调用点**：
//
//		observ.NewConsoleTracer(os.Stdout).
//			WithIndent(func(e observ.Event) bool { return e.Phase != supervisorName })
//
//	⭐ 为什么搬而不是内置：**"谁是顶层"是这个世界观里的知识**，
//	  而 `ConsoleTracer` 是**通用**的 —— 把某个命令的分层规则写进观测层，
//	  下一个命令就得跟它打架。
//
//	> ### 判据：**谁知道这件事，谁把它传进来。**
//
//	（L2 的这个教学点仍然在：编排者的一次调用内部会打出子 Agent 的若干步 ——
//	 而它们**缩进着**，因为它们是**被调用**的，不是并列的。）

// printBoard 打印黑板上的产物清单。
//
// **注意这里打印的是"引用 + 摘要"，不是正文** —— 与编排者看到的东西完全一致。
// 这不是偷懒：如果编排者的上下文里有正文，它就迟早会去搬运它（A2/A3 的坑）。
func printBoard(board *state.Blackboard) {
	fmt.Println("\n========== 黑板（Agent 之间只传引用 + 摘要）==========")

	found := false
	for _, k := range []schema.Kind{
		schema.KindResearch, schema.KindOutline, schema.KindDesign, schema.KindRender,
	} {
		a, err := board.Get(string(k))
		if err != nil {
			continue
		}
		found = true
		fmt.Printf("  %-10s %s\n", schema.RefOf(a), a.Summary)
		fmt.Printf("  %-10s 产出者=%s · 版本数=%d\n", "", a.ProducedBy, board.Versions(string(k)))
		// 缺口也要打出来 —— 它现在是产物的一部分（信封上的 Shortfall）。
		// 只看"有没有产物"会漏掉"产物是残缺的"（L2-2）。
		for _, gap := range a.Shortfall {
			fmt.Printf("  %-10s ⚠ 缺口：%s\n", "", gap)
		}
	}
	if !found {
		fmt.Println("  （空）")
	}
}

func printDeliver(board *state.Blackboard, result agents.Result) {
	fmt.Println("\n========== 交付 ==========")

	if result.Complete() {
		fmt.Printf("必需产物**齐全且完整**（%d 轮完成）：research → outline → design → render\n", result.Rounds)
	} else {
		// ⚠️ 与 L1 的"超限 ≠ 失败"同一条道理：
		//    不齐全不等于失败，应当**交付已完成的部分 + 说明还缺什么**。
		//
		// ⚠️ 三类"没完成"必须分开说 —— 它们的**行动完全不同**：
		//    Missing    —— 从来没产出过        → 去做
		//    Incomplete —— 产出过，但有缺口    → 去补（别整个重做）
		//    Stale      —— 产出过，但依据过期  → 按顺序重做（先上游、后下游）
		fmt.Println("注意：这是**降级交付**，不是失败。")
		if len(result.Missing) > 0 {
			fmt.Printf("  · 从未产出：%s\n", kindNames(result.Missing))
		}
		if len(result.Incomplete) > 0 {
			fmt.Println("  · 产出过但不完整：")
			for _, gap := range result.Incomplete {
				fmt.Println("      - " + gap)
			}
		}
		if len(result.Stale) > 0 {
			fmt.Println("  · 产出过但依据已过期（顺序即执行顺序）：")
			for _, reason := range result.Stale {
				fmt.Println("      - " + reason)
			}
		}
	}

	a, err := board.Get(string(schema.KindRender))
	if err != nil {
		fmt.Println("没有可交付的渲染产物。")
		return
	}
	rr, err := schema.Decode[schema.RenderResult](a)
	if err != nil {
		fmt.Printf("渲染产物无法解析：%v\n", err)
		return
	}

	fmt.Printf("\n产出文件（共 %d 个）：\n", len(rr.Files))
	for i, f := range rr.Files {
		fmt.Printf("  %d. %s\n", i+1, f)
	}
	if len(rr.Failed) > 0 {
		// 失败必须被说出来。沉默的失败比报错更危险（R22）。
		fmt.Printf("\n未产出的页（共 %d 页）：\n", len(rr.Failed))
		for _, f := range rr.Failed {
			fmt.Printf("  第 %d 页：%s\n", f.PageIndex+1, f.Reason)
		}
	}

	// 顺带把内容打出来 —— 这是**验证"搬运无损"最直接的方式**：
	// 页面上出现的每一条要点，都应该能在研究结论里找到出处。
	printSlideOutline(board)
}

// printSlideOutline 打印大纲的最终内容（说明"内容只被压缩一次"）。
func printSlideOutline(board *state.Blackboard) {
	a, err := board.Get(string(schema.KindOutline))
	if err != nil {
		return
	}
	o, err := schema.Decode[schema.Outline](a)
	if err != nil {
		return
	}

	fmt.Println("\n页面内容（与渲染结果逐字一致 —— renderer 不做二次压缩）：")
	for _, p := range o.Pages {
		fmt.Printf("  第 %d 页「%s」\n", p.Index+1, p.Title)
		for _, pt := range p.Points {
			fmt.Printf("      - %s\n", pt)
		}
	}
}

// ─── 成本汇总 ───────────────────────────────────────────────────────
//
// L2 把成本结构又变复杂了一档：一次编排 = 编排者 + 4 个角色。
// **看不见的成本不可能被管住**（L1-6 的教训），所以这里必须打出来。
//
// 特别注意 renderer 那一行：**它是 0**。
// 这不是"没统计到"，而是这一跳真的没花一个 token ——
// 把"不花 token"变成一个可被观测的事实，而不是一句口头承诺。

// printCost 打印成本汇总。
//
// 用量走的是**可选能力** `agents.StatsReporter`（接口对"能被调度"与"能报用量"
// 做了区分；需要时用类型断言探测 —— 这是 Go 里表达可选能力的惯用法）。
//
// ⚠️ 口径必须统一为**累计**。此前这里是"最近一次"，而 supervisor 那边是"跨轮累加" ——
// 同一张表两种口径，结果就是"某个角色跑了两遍时，它的 token 只算最后一遍"（L2-3）。
func printCost(total agent.Stats, subs []agents.SubAgent) {
	fmt.Println("\n----- 成本汇总 -----")
	fmt.Printf("%-10s %8s %8s %8s %8s\n", "角色", "步数", "工具调用", "prompt", "completion")
	fmt.Printf("%-10s %8d %8d %8d %8d\n",
		"supervisor", total.Steps, total.ToolCalls, total.PromptTokens, total.CompletionTokens)

	var sub agent.Stats
	for _, s := range subs {
		st := agent.Stats{}
		if r, ok := s.(agents.StatsReporter); ok {
			st = r.Stats()
		}
		sub = addStats(sub, st)
		fmt.Printf("%-10s %8d %8d %8d %8d\n",
			s.Name(), st.Steps, st.ToolCalls, st.PromptTokens, st.CompletionTokens)
	}

	all := addStats(total, sub)
	fmt.Printf("%-10s %8d %8d %8d %8d\n",
		"合计", all.Steps, all.ToolCalls, all.PromptTokens, all.CompletionTokens)
	fmt.Println("（口径：本角色实例**累计**；renderer 恒为 0 —— 它不调用模型）")
}

// ─── 故障注入：让"部分成功"可以被人工复现 ─────────────────────────────
//
// 为什么需要它？
//
//	`RenderSlideHTML` 只在磁盘出错时才会失败，而"3 页里失败 1 页"这条路径
//	**必须能被人工复现** —— 否则 L2-2 的修复（"存在 ≠ 完整"）
//	就永远只是"读代码觉得对"，而不是"看见过它工作"。
//
// 这也是 `SlideRenderer` 被做成**端口**（原则五）的直接回报：
// 注入一个故障，只需要写一个"转发 3 行 + 命中就报错"的装饰器，
// `renderer.go` 一行都不用改。
//
//	$env:L2_RENDER_FAIL="结论"; go run ./cmd/l2
//	  → 标题里含"结论"的那一页必定渲染失败
//	  → 编排者发现"render 产出过但不完整" → 继续调度 → 仍失败 →
//	    最终**降级交付**并说清缺了哪一页

// maybeFailingSubAgent 是**故障注入**：让指定角色（按 `Name()` 匹配）必定失败。
//
//	$env:L2_FAIL_ROLE="outline"; go run ./cmd/l2
//
// 为什么需要它？因为"熔断"这条路径**很难自然复现**：
//
//	超时类的故障是**全局**的 —— 连编排者自己的模型调用也会一起失败，
//	于是整个编排在第一句就报错退出，根本走不到"熔断某个角色"。
//
//	而熔断要验证的恰恰是"**一个角色**坏掉时，编排者能不能换一条路"。
//	所以注入必须**只坏一个**。
//
// 它同时是 `SubAgent` 这个接口的又一次红利：装饰一个角色
// 只需要转发 5 个方法，`outline.go` 一行都不用改（原则五）。
func maybeFailingSubAgent(sub agents.SubAgent) agents.SubAgent {
	if strings.TrimSpace(os.Getenv("L2_FAIL_ROLE")) != sub.Name() {
		return sub
	}
	return &alwaysFailSubAgent{inner: sub}
}

type alwaysFailSubAgent struct{ inner agents.SubAgent }

func (a *alwaysFailSubAgent) Name() string         { return a.inner.Name() }
func (a *alwaysFailSubAgent) Kind() schema.Kind    { return a.inner.Kind() }
func (a *alwaysFailSubAgent) Description() string  { return a.inner.Description() }
func (a *alwaysFailSubAgent) Needs() []schema.Kind { return a.inner.Needs() }

func (a *alwaysFailSubAgent) Run(_ context.Context, _ agents.SubAgentInput) (schema.Payload, error) {
	return nil, fmt.Errorf("注入的故障：角色 %s 被设为必定失败", a.inner.Name())
}

// maybeFailingRenderer 返回一个故障注入装饰器；未开启注入时返回 nil（走默认实现）。
func maybeFailingRenderer(outDir string) agents.SlideRenderer {
	needle := strings.TrimSpace(os.Getenv("L2_RENDER_FAIL"))
	if needle == "" {
		return nil
	}
	return &failingSlideRenderer{inner: tools.NewRenderSlideHTML(outDir), needle: needle}
}

type failingSlideRenderer struct {
	inner  agents.SlideRenderer
	needle string
}

// FilePathFor 必须**原样转发** —— 否则写盘前的冲突预检就换了基准，
// 预检说"不冲突"而实际写同一个文件。装饰器要把"决策所依赖的全部信息"一起代理。
func (f *failingSlideRenderer) FilePathFor(title string) string { return f.inner.FilePathFor(title) }

func (f *failingSlideRenderer) Render(s schema.Slide) (string, int, error) {
	if strings.Contains(s.Title, f.needle) {
		return "", 0, fmt.Errorf("注入的故障：模拟写盘失败（标题命中 %q）", f.needle)
	}
	return f.inner.Render(s)
}

// ─── 离线 Mock：一个客户端扮演所有角色 ───────────────────────────────
//
// # 为什么一个 Mock 就够
//
// 判别依据是请求里的**工具清单** —— 这件事本身就是 L2 的核心结论之一：
//
//	"角色" = SystemPrompt（人格与纪律） + 工具集（能做什么）
//
// 它不是"一个独立的模型"。所以：
//
//	① 同一个模型完全可以扮演所有角色；
//	② 角色之间真正的隔离，不是模型不同，而是**上下文各自独立**
//	   （每个角色都是一次全新的 Run，只看得到自己该看的东西）。
//
// 这也顺带回答了 L1 设计四问的第 2 题：用同一个模型做 Generator/Critic 的隐患是
// "相关盲区"（同训练数据 = 同一个知识盲点）。但**在这里不成问题** ——
// 因为这四个角色之间是**分工**，不是**相互审查**。
//
// # 这不代表 Mock 能替代真实模型
//
// 它没有"思考"，只是脚本逻辑（P4：Mock 让 Prompt 的真实质量无法被验证）。
// 它的全部价值是：**证明编排机制本身是通的**。
// 真实效果必须接真实模型。
func newRoleMock(board *state.Blackboard) llm.Client {
	return llm.NewFuncClient("l2-role-mock",
		func(_ context.Context, _ int, req llm.Request) (*llm.Response, error) {
			switch {
			case hasTool(req, submitResearch):
				return researchTurn(req)
			case hasTool(req, submitOutline):
				return outlineTurn(req)
			case hasTool(req, submitDesign):
				return designTurn(req)
			case hasTool(req, nameResearch) && hasTool(req, nameRenderer):
				return supervisorTurn(req, board)
			default:
				// 故意"吵"一点：一旦有人给某个角色加了新工具、
				// 或者新加了一个角色，Mock 会立刻说不认识这组工具，
				// 而不是**默默走进一个错误的分支**。
				return nil, fmt.Errorf("Mock 不认识这组工具：%s（请给它加一个分支）",
					strings.Join(toolNames(req), ", "))
			}
		})
}

// ── 研究员 ──────────────────────────────────────────────────────────

func researchTurn(req llm.Request) (*llm.Response, error) {
	// 已经提交过 → 收尾（不再调工具，否则循环会一路转到 MaxSteps）
	if succeeded(req.Messages, submitResearch) {
		return finalResponse("研究完成。")
	}

	// 已经拿到搜索结果 → 把结果整理成结论
	//
	// 这里同样要"**成功**搜到过"：一次失败的检索没有可供整理的素材，
	// 拿它去写结论只会编出内容来。失败就再搜一次。
	if succeeded(req.Messages, "web_search") {
		findings := findingsFromSearch(lastToolContent(req.Messages, "web_search"))
		if len(findings) == 0 {
			// 空 findings 是**合法结论**（research 的提交口允许传 []）。
			return callResponse("没有搜到可用素材，如实提交空结论。",
				submitResearch, `{"findings":[]}`, 300, 30)
		}
		return callResponse("素材够了，整理成带来源的结论。", submitResearch,
			mustJSON(map[string]any{
				"findings": findings,
				"queries":  []string{topicFromPrompt(lastUserText(req.Messages))},
			}), 520, 160)
	}

	return callResponse("先搜一下资料。", "web_search",
		mustJSON(map[string]any{
			"query": topicFromPrompt(lastUserText(req.Messages)),
			"top_k": 5, // 多取几条，后面的页面才有内容可放
		}), 220, 40)
}

// findingsFromSearch 把 web_search 的结果转成 findings，**逐条带上来源编号**。
//
// 这里的 `web_search#N` 就是"断言锚点"：它会在 Prompt 里一路传下去
// （outline 的 Prompt 会带上它），于是页面上的每一句都能回溯到它来自第几条检索结果。
func findingsFromSearch(raw string) []map[string]any {
	var out struct {
		Results []struct {
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}

	findings := make([]map[string]any, 0, len(out.Results))
	for i, r := range out.Results {
		claim := strings.TrimSpace(r.Snippet)
		if claim == "" {
			continue // 没有内容就没有结论 —— 不要为了凑数编一条
		}
		findings = append(findings, map[string]any{
			"claim":  claim,
			"source": fmt.Sprintf("web_search#%d", i+1),
		})
	}
	return findings
}

// ── 大纲编写者 ──────────────────────────────────────────────────────

func outlineTurn(req llm.Request) (*llm.Response, error) {
	if succeeded(req.Messages, submitOutline) {
		return finalResponse("大纲完成。")
	}

	// 从 Prompt 里把上游结论抄出来（格式见 outline.renderResearch：`- 结论（来源：xxx）`）。
	//
	// ⚠️ 这是 Mock 的特权：真实模型直接"读得懂"那几行文本。
	//	它这么做有个很有用的副产品 —— **链路是可见的**：
	//	你改研究结论，大纲的内容会跟着变；改了 Research 的字段，页面上的字也会变。
	//	如果 Mock 写死内容，"内容有没有被搬运对"就永远无法被观察。
	claims := claimsFromPrompt(lastUserText(req.Messages))
	pick := func(i int, fallback string) string {
		if i < len(claims) && strings.TrimSpace(claims[i]) != "" {
			return claims[i]
		}
		return fallback
	}

	// 3 页的叙事结构：开场 → 展开 → 收束。
	//
	// 条数是**结构页 1 条 + 正文页 3 条 + 结构页 1 条** —— 正好踩在
	// `decodeOutline` 的约束上（正文 3-5，首页/末页 1-5）。
	pages := []map[string]any{
		{"title": "行业现状", "points": []string{pick(0, "渗透率与规模的最新情况")}},
		{"title": "增长的三重压力", "points": []string{
			pick(1, "需求增速放缓"),
			pick(2, "产能仍在扩张"),
			pick(3, "价格战加剧"),
		}},
		{"title": "结论与建议", "points": []string{pick(4, "优先布局充电网络与售后体系")}},
	}

	return callResponse("组织成 3 页，提交大纲。", submitOutline,
		mustJSON(map[string]any{"pages": pages}), 760, 220)
}

// ── 版面设计者 ──────────────────────────────────────────────────────

func designTurn(req llm.Request) (*llm.Response, error) {
	if succeeded(req.Messages, submitDesign) {
		return finalResponse("版面设计完成。")
	}

	// 页数从 Prompt 里数出来（`buildDesignPrompt` 每页写一行「第 N 页「标题」：...」）。
	n := countOutlinePages(lastUserText(req.Messages))
	if n <= 0 {
		n = 3
	}

	// 中间的正文页轮换用不同版式 —— 让 Demo 能看出"设计确实在影响产物"。
	middle := []string{
		string(schema.LayoutBullets),
		string(schema.LayoutData),
		string(schema.LayoutTimeline),
		string(schema.LayoutComparison),
	}
	notes := []string{
		"整页留白，大标题压住主题",
		"三条并列展开，用编号与色块区分",
		"用一个关键指标承担这一页的结论",
		"收束成一句可执行的建议",
	}

	pages := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		layout := middle[i%len(middle)]
		note := notes[i%len(notes)]
		// 结构页的版式由产品规则固定，系统会校验 —— 这里必须照着填。
		switch {
		case i == 0:
			layout, note = string(schema.LayoutCover), notes[0]
		case i == n-1:
			layout, note = string(schema.LayoutClosing), notes[3]
		}
		pages = append(pages, map[string]any{
			"page_index": i,
			"layout":     layout,
			"note":       note,
		})
	}

	return callResponse("为每一页挑一个版式，提交设计。", submitDesign,
		mustJSON(map[string]any{"pages": pages}), 640, 180)
}

// ── 编排者 ──────────────────────────────────────────────────────────

// supervisorTurn 是**唯一"有全局视野"的角色**：它只看"哪些角色已经产出过"。
//
// 它的"状态"就是它自己的工具结果历史 —— 这正是"上下文即状态"的直观体现。
// 注意它**看不到任何正文**：工具返回的只有 {ref, summary}。
//
// `board` 只被**故障注入**用（判断"注入时机到了没"）。
// 真实模型看不到黑板 —— 那是它的定义所限（它只有上下文）。
func supervisorTurn(req llm.Request, board *state.Blackboard) (*llm.Response, error) {
	// "产出过"必须是"**成功**产出过"。
	//
	// ⚠️ 这个区分是 L2-9 之后修正的。原来数的是"所有回灌"，
	//	于是一次失败也算"派过了" —— Mock 因此**一轮只试一次**，
	//	永远走不到熔断那一步（而真实模型会在一轮里试很多次）。
	//
	//	**把失败算成完成，是"存在 ≠ 完整"的又一个变体**（L2-2 的同族）。
	succeed := func(name string) bool { return succeeded(req.Messages, name) }
	tripped := func(name string) bool { return breakerTripped(req.Messages, name) }

	// ── 故障注入 A：故意乱序 ──
	//
	// 目的：亲眼看到 `Needs()` 把顺序约束拦住，以及**拦下之后模型能自愈**。
	//
	// 流程是：先派 designer → 被代码拒绝（"前置产物未就绪：缺少 outline"）
	//        → 这条拒绝作为 tool 结果回灌 → Mock 看到它，下一轮改派 outline。
	//
	// 这就是"错误也是数据"的完整闭环（原则三）：
	// 拒绝不是崩溃，而是一条**模型可以据此纠正自己**的信息。
	if os.Getenv("L2_SKIP_ORDER") == "1" && !succeed(nameOutline) &&
		!preconditionRejected(req.Messages, nameDesigner) {
		return callResponse("（故障注入）我要先把设计做了。", nameDesigner, "{}", 180, 24)
	}

	// ── 故障注入 B：把**已经做完的 outline 再重做一遍** ──
	//
	// 目的：验证 L2-1 —— 上游一变，所有"基于旧上游"的下游产物立刻就过期了。
	// 它刻意发生在 render **之后**，这样 design / render 都还停在旧版上。
	//
	// 预期：本轮结束后，编排者发现 render@1 的依据过期 → 不肯宣布完成 →
	//       下一轮按「系统检查」的顺序先重做 design、再重做 render → 收敛。
	//
	// ⚠️ 触发条件为什么用**黑板版本数**，而不是"本轮计数"？
	//
	//	因为编排者每一轮的上下文都是**全新的**（L2-5）——
	//	"本轮计数"在下一轮会**从零开始**，于是注入每轮都触发，
	//	上游被无限重做，永远收敛不了（第一版就是这样，跑出来 outline@6 还在涨）。
	//
	//	用黑板上的版本数描述"注入时机"是**无状态**的：
	//	outline 恰好 1 版、render 恰好 1 版 —— 这个瞬间只出现一次。
	//	这也顺带说明：**Mock 能看到黑板，而真实模型只能看到自己的上下文。**
	//	所以它只是"制造一个局面"的工具，不是"决策逻辑"的样本。
	if os.Getenv("L2_REDO") != "" &&
		board.Versions(nameOutline) == 1 && board.Versions(nameRenderer) == 1 {
		return callResponse("（故障注入）我要重做一次大纲。", nameOutline, "{}", 300, 30)
	}

	// 优先按「系统检查」提示走 —— 那是**代码算出来的待办**，比 Mock 自己数历史更权威。
	//
	// ⚠️ 这里有一个很值得注意的事实：编排者每一轮的上下文都是**全新的**（L2-5），
	//	所以它不可能靠"回忆"知道自己做过什么。
	//	**提醒 Prompt 就是它唯一的跨轮信息来源** —— 于是这段文案的准确度
	//	直接决定了它会不会把已经做完的事重做一遍。
	pending := pendingFromPrompt(lastUserText(req.Messages))
	if len(pending) > 0 {
		for _, k := range pending {
			// 已成功产出 → 不用再派；**已熔断 → 更不该再派**。
			//
			// 第二条是称职编排者的基本纪律：熔断的意义就是"别再撞它了"。
			// 如果被熔断之后还一直派，那熔断除了浪费一条回灌以外毫无作用。
			if succeed(k) || tripped(k) {
				continue
			}
			return callResponse(fmt.Sprintf("系统提示「%s」还没到位，我继续调度它。", k), k,
				mustJSON(map[string]any{"hint": "补齐系统检查里指出的部分"}), 320, 40)
		}
		// 待办已处理完 → 直接收尾。
		//
		// ⚠️ 不要再掉进下面的默认顺序 —— 那会把整条链重跑一遍。
		//	**有了准确的待办清单，就不该再靠"从头扫一遍"。**
		//	（这条纪律对真实模型同样适用：它最省 token 的行为就是"照着清单做"。）
		return finalResponse("系统检查里指出的待办已经处理完。")
	}

	switch {
	case !succeed(nameResearch) && !tripped(nameResearch):
		return callResponse("先让研究员找素材。", nameResearch,
			mustJSON(map[string]any{"hint": "围绕主题给出可引用的量化结论"}), 260, 40)

	case !succeed(nameOutline) && !tripped(nameOutline):
		return callResponse("素材齐了，进入大纲。", nameOutline,
			mustJSON(map[string]any{"hint": "3 页，把关键数字放进正文页"}), 300, 42)

	case !succeed(nameDesigner) && !tripped(nameDesigner):
		return callResponse("大纲好了，做版面设计。", nameDesigner,
			mustJSON(map[string]any{"hint": "正文页优先用能承载数据的版式"}), 320, 44)

	case !succeed(nameRenderer) && !tripped(nameRenderer):
		return callResponse("最后渲染成文件。", nameRenderer, "{}", 300, 30)

	default:
		// 有角色被熔断时，措辞要**准确** —— 否则人和模型都会以为"做完了"。
		if anyBreakerTripped(req.Messages) {
			return finalResponse("有角色已被熔断，本轮不再尝试它 —— 交给外层决定。")
		}
		return finalResponse("四个角色的必需产物都已产出，任务完成。")
	}
}

// ─── 小工具 ─────────────────────────────────────────────────────────

// callResponse / finalResponse 直接返回 `(*llm.Response, error)` 这个二元组：
// 它们本身永远不会失败（error 恒为 nil），但这个形状让**每个分支都只剩一种写法** ——
// 否则每个 `return` 都要在末尾手写一个 `, nil`，而漏掉一个就编译不过。
func callResponse(content, name, args string, promptTokens, completionTokens int) (*llm.Response, error) {
	return &llm.Response{
		Content: content,
		ToolCalls: []message.ToolCall{{
			ID: "call_" + name, Name: name, Arguments: args,
		}},
		Usage: llm.Usage{PromptTokens: promptTokens, CompletionTokens: completionTokens},
	}, nil
}

// finalResponse 构造一次"没有工具调用"的收尾回复。
//
// 没有 tool_call 就会自然终止 —— 这是 L0 就定下的自然终止条件，
// 在每一层（子 Agent 与编排者）都复用同一套循环。
func finalResponse(content string) (*llm.Response, error) {
	return &llm.Response{
		Content: content,
		Usage:   llm.Usage{PromptTokens: 180, CompletionTokens: 16},
	}, nil
}

func hasTool(req llm.Request, name string) bool {
	for _, d := range req.Tools {
		if d.Name == name {
			return true
		}
	}
	return false
}

func toolNames(req llm.Request) []string {
	out := make([]string, 0, len(req.Tools))
	for _, d := range req.Tools {
		out = append(out, d.Name)
	}
	return out
}

// succeeded 判断某个角色是否**成功**产出过（回执里 ok:true）。
//
// ⚠️ 注意它与"被调用过"的区别：一次**失败**的调用同样会产生一条 tool 消息。
//
//	把失败算成"派过了"，就会得出"这个角色做完了"的错误结论 ——
//	这是 L2-2（"存在" ≠ "完整"）的又一个同族变体。
//	Mock 第一版就是这么写的，结果是**一轮只试一次**，永远撞不到熔断。
func succeeded(msgs []message.Message, name string) bool {
	for _, m := range msgs {
		if m.Role == message.RoleTool && m.Name == name &&
			strings.Contains(m.Content, `"ok":true`) {
			return true
		}
	}
	return false
}

// breakerTripped 判断某个角色的**最近一次**调用是否被告知"已被熔断"。
func breakerTripped(msgs []message.Message, name string) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.RoleTool && msgs[i].Name == name {
			return strings.Contains(msgs[i].Content, "熔断")
		}
	}
	return false
}

// anyBreakerTripped 判断这一轮里有没有任何角色被熔断。
func anyBreakerTripped(msgs []message.Message) bool {
	for _, m := range msgs {
		if m.Role == message.RoleTool && strings.Contains(m.Content, "熔断") {
			return true
		}
	}
	return false
}

// countToolResult 统计某个工具的**结果条数**（含失败）。
func countToolResult(msgs []message.Message, name string) int {
	n := 0
	for _, m := range msgs {
		if m.Role == message.RoleTool && m.Name == name {
			n++
		}
	}
	return n
}

// preconditionRejected 判断某个子 Agent 的最近一次调用是否**因前置条件不满足被拒绝**。
//
// 前置条件有**两个层次**（与适配器里的检查一一对应）：
//
//	"前置产物未就绪"  → 缺上游
//	"前置产物已过期"  → 有上游，但它已经不是最新的了
//
// 这是"故障注入"能自愈的关键：Mock 必须能**看懂**那条拒绝信息。
// 真实模型同样读得懂（那本来就是写给模型看的可执行反馈，见 L1-5）。
func preconditionRejected(msgs []message.Message, name string) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.RoleTool && msgs[i].Name == name {
			return strings.Contains(msgs[i].Content, "前置产物未就绪") ||
				strings.Contains(msgs[i].Content, "前置产物已过期")
		}
	}
	return false
}

// lastUserText 取最后一条 user 消息。
func lastUserText(msgs []message.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}

// lastToolContent 取某个工具最近一次的返回内容。
func lastToolContent(msgs []message.Message, name string) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.RoleTool && msgs[i].Name == name {
			return msgs[i].Content
		}
	}
	return ""
}

// topicFromPrompt 从各种 Prompt 里把主题抠出来。
//
// 这是 Mock 的"读题"动作 —— 真实模型不需要，它直接读得懂。
func topicFromPrompt(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		for _, prefix := range []string{"研究主题：", "主题：", "用户需求："} {
			if v, ok := strings.CutPrefix(line, prefix); ok {
				if t := strings.TrimSpace(v); t != "" {
					return t
				}
			}
		}
	}
	return "未命名主题"
}

// claimsFromPrompt 从大纲的 Prompt 里抄出上游结论（格式：`- 结论（来源：xxx）`）。
func claimsFromPrompt(prompt string) []string {
	var out []string
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		claim := strings.TrimPrefix(line, "- ")
		if i := strings.Index(claim, "（来源："); i >= 0 {
			claim = claim[:i]
		}
		if claim = strings.TrimSpace(claim); claim != "" {
			out = append(out, claim)
		}
	}
	return out
}

// pendingFromPrompt 从「系统检查」提示里读出"代码算出来的待办"。
//
// 它是 Mock 的"读题"能力（真实模型直接读得懂这段话），但它演示的事情是真的：
//
//	编排者**唯一**的跨轮信息来源就是这段提醒（L2-5：它的上下文每轮重置）。
//	所以这段文案的准确度，直接决定了它会不会把已经做完的角色重做一遍。
//
// 注意它同时解析**两类**待办：
//
//	"还没有产出：outline"     → 要**去做**
//	"- render：第 3 页：…"    → 要**去补**（别整个重做）
//
// 这两类混在一起，正是 L2-2 修复的重点。
func pendingFromPrompt(prompt string) []string {
	var out []string
	inShortfall := false

	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)

		if v, ok := strings.CutPrefix(line, "【系统检查】以下必需产物还没有产出："); ok {
			inShortfall = false
			for _, k := range strings.Split(v, "、") {
				if k = strings.TrimSpace(k); k != "" {
					out = append(out, k)
				}
			}
			continue
		}
		if strings.HasPrefix(line, "【系统检查】以下产物") {
			inShortfall = true
			continue
		}
		if !inShortfall {
			continue
		}
		// 短句形如 "- render：第 3 页：写盘失败" —— 冒号前是角色名。
		if v, ok := strings.CutPrefix(line, "- "); ok {
			if k, _, found := strings.Cut(v, "："); found {
				if k = strings.TrimSpace(k); k != "" {
					out = append(out, k)
				}
			}
		}
	}
	return out
}

// countOutlinePages 数出设计 Prompt 里的页面行数（`第 N 页「标题」：...`）。
func countOutlinePages(prompt string) int {
	n := 0
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, "第 ") && strings.Contains(line, " 页「") {
			n++
		}
	}
	return n
}

func kindNames(kinds []schema.Kind) string {
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, string(k))
	}
	return strings.Join(names, "、")
}

func addStats(a, b agent.Stats) agent.Stats {
	return agent.Stats{
		Steps:            a.Steps + b.Steps,
		ToolCalls:        a.ToolCalls + b.ToolCalls,
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// must 让装配期的错误**当场炸掉**。
//
// 装配错了一个角色，是这个程序自己的 bug —— 越早暴露越好。
// 让它一路跑到运行期才变成一个奇怪的 tool 错误，是最难查的一类故障。
func must[T any](v T, err error) T {
	if err != nil {
		fmt.Fprintf(os.Stderr, "[装配失败] %v\n", err)
		os.Exit(1)
	}
	return v
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > 160 {
		return string([]rune(s)[:160]) + "..."
	}
	return s
}
