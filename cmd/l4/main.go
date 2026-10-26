// Command l4 是 L4 的一键回归：**跑一遍评测集，给出可比指标**。
//
//	go run ./cmd/l4                  # 跑全部用例
//	go run ./cmd/l4 -tag=smoke       # 只跑子集
//	go run ./cmd/l4 -dump            # 生成一份初始用例集（人改它）
//
// # ⚠️ 它顺手还了一笔债：L3-10（"真实角色在图驱动下没跑过"）
//
//	`cmd/l3` 的四个节点是**确定性脚手架**（`demoRole`）——
//	它刻意如此，因为 L3 要验证的是"**中断 → 重启 → 恢复，产出与不中断一致**"，
//	而那件事**用真实模型断言不了**（两次运行本来就会不同）。
//
//	而 `cmd/l4` 用的是**真实角色**（`agents.NewResearchAgent` …）+ **图驱动** ✅
//	那正是 L3-10 记下的待办，它的触发条件写的是
//	"**L4 接评测集时（那时需要一个稳定的输入）**" —— 现在就是那个时候。
//
// # ⚠️ 一个诚实的射程标注（与 `cmd/l3` 那条同源）
//
//	模型是**离线替身**，所以这一套评测的是**骨架**：
//	流程有没有走完、产物齐不齐、成本/步数有没有漂。
//
//	> ### 它**测不出**"这句 Prompt 写得更好" —— 那需要真实模型。
//	> 但它**测得出**"改了 Prompt 之后，流程与成本**变了**" ——
//	> 而那正是 L4 验收标准①说的事（"改一次 Prompt → 一键看到指标升降"）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"multiagent/internal/agents"
	"multiagent/internal/budget"
	"multiagent/internal/checkpoint"
	"multiagent/internal/eval"
	"multiagent/internal/graph"
	"multiagent/internal/llm"
	"multiagent/internal/message"
	"multiagent/internal/observ"
	"multiagent/internal/schema"
)

const (
	outDir   = "out/l4"
	caseFile = "internal/eval/cases.jsonl"
)

func main() {
	tag := flag.String("tag", "", "只跑带这个 tag 的用例（留空 = 全部）")
	dump := flag.Bool("dump", false, "把内置的初始用例集写到 "+caseFile+"（然后人改它）")
	trace := flag.String("trace", "", "把事件录播到这个文件（留空 = 不录）")
	flag.Parse()

	if *dump {
		if err := dumpCases(); err != nil {
			fail("%v", err)
		}
		fmt.Printf("已生成初始用例集：%s\n", caseFile)
		return
	}

	set, err := eval.LoadCases(caseFile)
	if err != nil {
		fail("%v", err)
	}
	// ⚠️ **跑之前**校验 —— 见 `CaseSet.Validate` 的注释（永不绿的用例最危险）。
	if err := set.Validate(); err != nil {
		fail("用例集不自洽: %v", err)
	}
	set = set.Filter(*tag)

	// ★ 故障注入 ③：往用例集里**临时**注入一条必然失败的用例。
	//
	//	它要证明的**不是**判据能红（cases.jsonl 里那条 expect-mismatch 已经证明了），
	//	而是**指标会动**：三用例 2 通过 → 四用例 2 通过（成功率 66.7% → 50%）。
	//
	//	一个**永远 66.7%** 的评测，与一个**永远 100%** 的评测一样可疑。
	//
	//	临时注入比常驻一条必红用例更强：它证明的是**这一轮的汇总真的在算**。
	if os.Getenv("L4_BAD_CASE") != "" {
		set.Cases = append(set.Cases, eval.Case{
			ID: "injected-bad", Topic: "注入的用例",
			Expect: eval.Expectation{Pages: 99}, // 图只会产 3 页 -> 必然红
		})
	}

	fmt.Printf("===== L4 一键回归 =====\n用例集 : %s\n子集   : %s\n用例数 : %d\n\n",
		caseFile, orNone(*tag), len(set.Cases))

	// ⚠️ 留一个**引用** —— 因为"它自愈过几次"必须能被打印出来。
	//	见 `printJudgements` 里那条注释：**一次成功的自愈，在输出上
	//	与"从来没坏过"完全一样** —— 而那让注入**演示不出来**。
	js := &judgeStub{}
	vs := eval.Run(context.Background(), set, eval.Options{
		Run:   runOneCase(*trace),
		Judge: js, // ★ 软判据（离线替身 —— 见它的注释）
	})
	fmt.Print(eval.Render(vs))
	printJudgements(vs, js)

	if s := eval.Summarize(vs); s.Failed > 0 || s.Errored > 0 {
		os.Exit(1) // 回归失败 → 非零退出（**能被 CI 用**）
	}
}

// judgeStub 是**离线的软判据替身**。
//
// # ⚠️ 为什么是替身，而不是真实模型 —— 那是设计文档 §6 那条"不做"的**落地**
//
//	> **不做**：Judge 用真实模型自动跑。
//	> **理由**：每一轮都花钱；而**判据的稳定性**要先在离线替身上验证。
//	> **触发条件**：用例集稳定之后（L5）。
//
//	⭐ 所以这个替身不是"权宜之计"，是**那个决定的实现**：
//	  它让"软判据这条通路"**现在就能被验证**（含自愈），
//	  而把"换真实模型"降级成**换一个实现** —— 那正是端口的全部意义。
//
// ⚠️ 而它必须**随内容变化** —— 一个永远给 8 分的替身，
//
//	会让"软判据动了没有"这件事**测不出来**（与 `cmd/l4` 那个模型替身同一条：**读 Prompt，并照做**）。
type judgeStub struct {
	calls  int
	healed int // "靠回灌救回来的次数" —— 见 `printJudgements`
}

func (s *judgeStub) Judge(_ context.Context, prompt string) (string, error) {
	s.calls++

	// ★ 故障注入 ④：**第一次给一个非法结构** —— 用来演示回灌自愈。
	//
	//	三样**同时**坏（自创维度 + 分数越界 + 空理由）—— 于是它一次演示了
	//	`Judgement.Validate` 的全部三条校验。
	//
	//	⚠️ 而"自愈"与 `retry` **不是一回事**：这里**改了输入**（回灌校验器的原话），
	//	而 `retry` 是同一个请求再发一次。**重试治链路，自愈治输出。**
	//
	//	⚠️ 条件里那个 `s.calls == 1` 是**故意只用第一个用例演练**：
	//	`judgeStub` 是**一个实例**，`calls` 是全局计数 —— 所以只有第一个用例的
	//	第一次调用会返回坏的。若写成"每个用例的第一次"，那它就不是一个
	//	能复现的注入（注入必须**确定**）。
	if os.Getenv("L4_JUDGE_REFUSES") != "" && s.calls == 1 {
		return `{"scores":{"creativity":100},"reason":""}`, nil
	}

	// ⚠️ **记下"这一次是靠回灌救回来的"** —— 否则它**演示不出来**：
	//
	//	一次**成功的**自愈，在最终输出上与"从来没坏过"**完全一样**。
	//
	//	> ### 一个成功了的故障注入，会被当成"注入没生效"。
	//	> （这与 R48 那条"一个符合预期的错误结果最不容易被怀疑"是同一族 ——
	//	>  只不过这次它藏在**成功**里。）
	if os.Getenv("L4_JUDGE_REFUSES") != "" && s.calls == 2 {
		s.healed++
	}

	// ⭐ 分数**跟着 Prompt 里的内容走** —— 于是"改了 Prompt，软判据也会动"是真的，
	//	而不是一个固定值造成的错觉。
	body := prompt
	if i := strings.Index(body, "===== 页面内容 ====="); i >= 0 {
		body = body[i:]
	}
	score := 8
	switch {
	case strings.Contains(body, "待补充"), strings.Contains(body, "TODO"):
		score = 3 // 页面里漏了内容 —— 而那正是软判据该抓的
	case len([]rune(body)) < 400:
		score = 5 // 内容太薄
	}

	return mustJSON(map[string]any{
		"scores": map[string]int{
			string(eval.DimFaithful): score,
			string(eval.DimComplete): score,
			string(eval.DimReadable): score,
		},
		"reason": fmt.Sprintf("离线替身按 Prompt 里的内容长度给了 %d 分", score),
	}), nil
}

// printJudgements 把软判据打出来。
//
// ⚠️ 它**单独一段**，而不是塞进 `eval.Render` 的表格里 —— 因为两者的读法不同：
//
//	硬判据表  "**过了没有**"       → 决定成败，必看
//	软判据    "**像不像变差了**"   → 只是线索，看一眼
//
//	⭐ 而"**它不参与 `Passed`**"这个决定，必须在输出里**看得出来** ——
//	  否则下一个人会以为"软判据给了 3 分怎么还是 ✅"是个 bug。
func printJudgements(vs []eval.Verdict, js *judgeStub) {
	var shown []string
	for _, v := range vs {
		switch {
		case v.Err != nil:
			continue // 没跑完的没有产物可评（软判据也**不跑**，见 `runOne`）
		case v.JudgeErr != nil:
			shown = append(shown, fmt.Sprintf("  %-16s ⚠️ 没能用上：%s", v.CaseID, oneLine(v.JudgeErr.Error())))
		case v.Judgement != nil:
			parts := make([]string, 0, len(v.Judgement.Scores))
			for _, d := range eval.Dimensions() {
				parts = append(parts, fmt.Sprintf("%s=%d", d, v.Judgement.Scores[d]))
			}
			shown = append(shown, fmt.Sprintf("  %-16s %s", v.CaseID, strings.Join(parts, " ")))
		}
	}
	if len(shown) == 0 {
		return
	}

	fmt.Println("\n软判据（模型给的分 —— ⚠️ **它不参与上面的通过/失败**）：")
	for _, s := range shown {
		fmt.Println(s)
	}
	fmt.Println("  ⚠️ 理由：分是**概率**的，让它决定成败会让评测**开始 flake**；")
	fmt.Println("     而 flake 的评测比没有评测更坏 —— 它会训练人忽略红色。它是**线索**，不是判据。")

	// ⚠️ **回灌自愈必须被打印出来** —— 否则它演示不出来：
	//
	//	一次**成功的**自愈，最终结果与"从来没坏过"**完全一样**。
	//	没有这一行，`L4_JUDGE_REFUSES=1` 会看起来**没生效** ——
	//	而"注入没生效"与"注入生效且被救回来了"是**两个完全相反**的结论。
	if js.healed > 0 {
		fmt.Printf("\n  ★ 回灌自愈：%d 次非法输出被回灌后改对（共调用 %d 次）\n", js.healed, js.calls)
		fmt.Println("    ⚠️ 而它**不是** `retry`：自愈**改了输入**（回灌校验器的原话），")
		fmt.Println("       `retry` 是同一个请求再发一次。**重试治链路，自愈治输出。**")
	}
}

// evalGuard 是评测用的预算守卫。
//
// # ⚠️ 它只在 `L4_HUGE_TOKENS` 下才有限额 —— 而那个限额**故意设得很小**
//
//	替身会报一次**巨大**用量 → 于是**下一次** `Allow` 就会拒绝。
//
//	⭐⭐ 而它正好演示了 R43 写过的那条边界：
//
//		`Allow(need)` 的 `need` 里只有 `Steps: 1` —— 因为它是唯一**可预估**的一维。
//		token / 成本**要跑完才知道**，所以它们只能事后记，
//		于是那两维**永远晚一步**：**拦不住"这一次"，只能拦住"下一次"。**
//
//	> ### 这不是 bug，是"估算"这件事的物理边界。
//	> 而 `L4_HUGE_TOKENS` 让它**看得见** —— 否则那条注释只是一句辩解。
//
// ⚠️ 平时返回 `Unlimited`（一个**显式的对象**，不是 nil）——
//
//	与 `RunnerOptions.Guard` 的归一化同一条：**"不限额"是一个对象选择，不是"没有"。**
func evalGuard() budget.Guard {
	if os.Getenv("L4_HUGE_TOKENS") == "" {
		return budget.NewUnlimited()
	}
	return budget.New(budget.Limits{
		Steps:            budget.NoLimit,
		PromptTokens:     100_000, // ← 比替身报的"巨大用量"小一个量级
		CompletionTokens: budget.NoLimit,
		CostMilli:        budget.NoLimit,
		Elapsed:          budget.NoWall,
	})
}

func runOneCase(tracePath string) eval.Runner {
	return func(ctx context.Context, c eval.Case) (eval.Outcome, error) {
		// ⚠️⚠️ **先校验，再碰磁盘** —— `c.ID` 来自 `cases.jsonl`（**人的输入**）。
		//
		//	没有这一行时，一个 `id: "../../x"` 的用例会让下面那行
		//	`RemoveAll` 删掉 `outDir/ckpt` **之外**的目录 ——
		//	而 `checkpoint.FileStore` 的校验**在它之后才发生**，挡不住。
		//
		//	> ### 用例集不只是数据 —— 它是**可以被执行的**输入。
		//
		// ⭐ 而这里调的是 `checkpoint` 里**同一份**校验（不是抄一份）——
		//	两处会漂移，而"漂移的两份校验"比"没有校验"更坏（R58 的 `KindReconcile` 同族）。
		if err := checkpoint.ValidRunID(c.ID); err != nil {
			return eval.Outcome{}, fmt.Errorf("用例 id 不安全（它会被拼进路径）: %w", err)
		}

		dir := filepath.Join(outDir, "ckpt", c.ID)
		if err := os.RemoveAll(dir); err != nil {
			return eval.Outcome{}, fmt.Errorf("清理 %s 的 checkpoint 失败: %w", c.ID, err)
		}

		// ★ **一个用例一个 `Recorder`** —— 于是判据看到的 `Events`
		//	**永远不会串到别的用例**。（串行跑 + 每个用例独立 = 双重保证。）
		rec := observ.NewRecorder()

		// ★ 录播是可选的（`-trace`）—— 而 `Fanout` 让"没开录播"不需要一个分支。
		var outgoing observ.Tracer = observ.NopTracer{}
		if tracePath != "" {
			jt, err := observ.NewJSONLTracer(tracePath)
			if err != nil {
				return eval.Outcome{}, err
			}
			defer jt.Close()
			outgoing = jt
		}

		bg := evalGuard()
		g, err := buildGraph(c.Topic, c.Hints, observ.Fanout{rec, outgoing}, bg)
		if err != nil {
			return eval.Outcome{}, err
		}

		store, err := checkpoint.NewFileStore(dir)
		if err != nil {
			return eval.Outcome{}, err
		}
		r, err := graph.NewRunner(graph.RunnerOptions{
			Graph:  g,
			Store:  store,
			RunID:  c.ID,
			Topic:  c.Topic,
			Hints:  c.Hints,
			Tracer: observ.Fanout{rec, outgoing},
			Guard:  bg,
		})
		if err != nil {
			return eval.Outcome{}, err
		}

		res, err := r.Run(ctx)
		if err != nil {
			// ★ 跑不完时把**过程**打出来 —— 见 `dumpToolResults` 的注释。
			dumpToolResults(c.ID, rec.Events())
			return eval.Outcome{}, err
		}
		// 评测里**不该有中断** —— 有人守着才跑得完（而 HITL 的恢复路径
		// 已经由 `cmd/l3 -verify` 专门验过）。所以这里直接 confirm。
		if res.Status == graph.StatusInterrupted {
			res, err = r.Resume(ctx, graph.Decision{Intent: graph.IntentConfirm})
			if err != nil {
				return eval.Outcome{}, err
			}
		}

		// ★★ 预算用完 —— **"降级交付"，而不是"崩溃"**（设计文档 §5 的硬要求）
		//
		//	> 触顶的行为**必须是"交付部分 + 说明"**，而不是 `return err`。
		//
		// ⚠️ 而**`eval` 仍然要把这一轮判成"跑不完"** —— 那不是矛盾，是两条判据：
		//
		//		"降级交付" 说的是 **产物不丢**（它们在磁盘上、在 `State` 里）
		//		"跑不完"   说的是 **判据不成立**（没跑完就没有完整的产物可判）
		//
		//	> ### 一个**没跑完**的用例，"硬判据全过"是**巧合**，不是信号。
		//
		//	⭐ 所以这里返回 error，但**措辞必须与"出错了"不同** ——
		//	  否则人会去查模型 / 网络，而真实原因只是"我们把上限设小了"。
		if res.Status == graph.StatusBudgetStopped {
			return eval.Outcome{}, fmt.Errorf(
				"预算用完 → **降级交付**（不是出错）：已完成的 %d 个节点产物都在（%v）。"+
					"说明：%s", len(res.Ran), res.Ran, res.StopReason)
		}
		if res.Status != graph.StatusCompleted {
			return eval.Outcome{}, fmt.Errorf("没有跑完（状态 %s，执行了 %v）", res.Status, res.Ran)
		}

		return eval.Outcome{
			Board:    res.State.Board,
			Rendered: readRendered(res.State.Board, c.ID),
			Events:   rec.Events(),
		}, nil
	}
}

// ─── 装配：**图 + 真实角色**（L3-10 的兑现）─────────────────────────

func buildGraph(topic string, hints map[string]string, tr observ.Tracer, bg budget.Guard) (*graph.Graph, error) {
	research, err := agents.NewResearchAgent(agents.ResearchOptions{Client: newRuleClient("research"), Tracer: tr, Guard: bg})
	if err != nil {
		return nil, err
	}
	outline, err := agents.NewOutlineAgent(agents.OutlineOptions{Client: newRuleClient("outline"), Tracer: tr, Guard: bg})
	if err != nil {
		return nil, err
	}
	design, err := agents.NewDesigner(agents.DesignerOptions{Client: newRuleClient("design"), Tracer: tr, Guard: bg})
	if err != nil {
		return nil, err
	}

	roles := []agents.SubAgent{
		research, outline, design,
		agents.NewRenderer(agents.RendererOptions{OutDir: filepath.Join(outDir, "html"), Tracer: tr}),
	}

	nodes := make([]graph.Node, 0, len(roles))
	for _, s := range roles {
		// ⚠️ 这里**不传 Hint** —— 它是**运行期输入**，由 `RunnerOptions.Hints`
		//	按节点名给（`cmd/l3` 同一种写法）。在节点上再传一次就是**两处**。
		n, err := agents.NewSubAgentNode(agents.SubAgentNodeOptions{Sub: s})
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	// ⚠️ 与 `cmd/l3` 一样：**没有一条边是手写的** —— 四个角色的 `Needs()` 就是全部依据。
	return graph.BuildGraph(graph.GraphSpec{Nodes: nodes, Presets: []schema.Kind{schema.KindTopic}})
}

// readRendered 读回**给用户的最终产物**（渲染出来的 HTML 全文）。
//
// ⚠️ 判据里 `must_contain` / `must_not_contain` 问的是**这一份**，
//
//	不是黑板上的中间产物 —— 见 `eval.Outcome.Rendered` 的注释。
func readRendered(board map[string][]schema.Artifact, caseID string) []string {
	vs := board[string(schema.KindRender)]
	if len(vs) == 0 {
		return nil
	}
	r, err := schema.Decode[schema.RenderResult](vs[len(vs)-1])
	if err != nil {
		return nil
	}
	var out []string
	for _, f := range r.Files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		out = append(out, string(raw))
	}
	return out
}

// dumpToolResults 在一个用例跑不完时，把**工具回执**打出来。
//
// # ⚠️ 它为什么必须有（而它本该在第一次失败时就存在）
//
//	为了修"`submit_outline` 被拒"，我猜了 4 次 ——
//	每一次都是"改一个假设 → 跑一次完整评测（3 用例 × 4 角色）"。
//
//	**而真正的原因一直就在事件流里**：`tool_result` 的 `Detail` 带着工具原文
//	（"必须正好 3 页，实际 2 页" / "layout 不在枚举内" …）。
//	只是我**从来没把它打出来看过**。
//
//	> ### **"我大概知道"是排查里最贵的四个字。**
//
//	⭐ 而这个函数是那句教训的**物化**：让"过程"可读，而不是让人去猜。
//	  它是**长期能力**，不是一次性的调试语句 ——
//	  `eval` 的价值一半在"判过了没有"，另一半在"**没过时能不能归因**"。
//
// ⚠️ 只打**末尾几条**：一个跑不完的用例会用满步数，全打会把终端冲掉
//
//	（而看不见的诊断等于没有 —— `OneLine` / `clip` 存在的同一个理由）。
func dumpToolResults(caseID string, events []observ.Event) {
	var last []string
	for _, e := range events {
		if e.Kind == observ.KindToolResult {
			last = append(last, fmt.Sprintf("[%s] %s → %s", e.Phase, e.ToolName, oneLine(e.Detail)))
		}
	}
	if len(last) == 0 {
		return
	}
	const keep = 3
	if len(last) > keep {
		last = last[len(last)-keep:]
	}
	fmt.Fprintf(os.Stderr, "\n  ↳ %s 最后的工具回执（%d 条中的末尾 %d 条）：\n", caseID, len(last), keep)
	for _, s := range last {
		fmt.Fprintf(os.Stderr, "      %s\n", s)
	}
}

// oneLine 把多行压成一行（诊断信息也要能一眼看完）。
func oneLine(s string) string { return observ.OneLine(s) }

// ─── 模型替身：**读 Prompt，并照做** ────────────────────────────────

// ruleClient 是一个**按 Prompt 决定行为**的确定性替身。
//
// # ⚠️ 它为什么不是"预先写死一串回复"（`llm.ScriptedClient`）
//
//	因为 L4 验收标准①是"**改一次 Prompt → 一键看到指标升降**"。
//
//	而"预先写死的回复"**与 Prompt 无关** —— 改 Prompt 它照旧回同样的东西，
//	于是**指标永远不会动**，那条验收标准就**演示不出来**。
//
//	> ### 一个不读输入的替身，测不出"输入改了会怎样"。
//
// # 它读什么（都确定）
//
//	`req.System` 里含哪个角色的标记 → 调哪个 `submit_*`
//	`req.System` 里的"**正好 N 页**"   → 交几页（← **这就是"改 Prompt"的入口**）
//
// ⚠️ **它解析 Prompt 的措辞，所以措辞改了它会失效** —— 而这一点是**故意的**：
//
//	解析不到页数时它**不提交**（而不是猜一个），于是那个用例**会红**，
//	而不是**假装成功**。
//
//	> 一个解析失败却静默给默认值的替身，会让"Prompt 被改坏了"这件事
//	> **表现为全绿** —— 而那正是最坏的一种：**它会让人相信一个错的东西。**
func newRuleClient(role string) llm.Client { return &ruleClient{role: role} }

type ruleClient struct{ role string }

func (c *ruleClient) Name() string { return "rule-" + c.role }

func (c *ruleClient) Generate(_ context.Context, req llm.Request) (*llm.Response, error) {
	usage := llm.Usage{PromptTokens: approxTokens(req.System) + approxTokens(lastUser(req)), CompletionTokens: 20}

	// ★★ 故障注入 ②：**伪造一次超大用量** —— 而它是 L4 验收标准的核心。
	//
	//	> "**预算超限时直接报错丢失可用结果**"是教学计划里点名的"可能问题" →
	//	> 触顶的行为**必须是"交付部分 + 说明"**，而不是 `return err`。
	//
	// ⚠️ 而它**一次就爆**：报 1000 万 tokens，而 `evalGuard` 的上限是 10 万。
	//
	//	⭐⭐ 于是**下一次** `Allow` 才会拒绝 —— 而那正是 R43 那条边界的实证：
	//	  **token 维拦不住"这一次"，只能拦住"下一次"**
	//	  （因为用量要跑完才知道）。
	//
	//	> 一条"这不是 bug 是物理边界"的注释，**只有在这种注入下才可信** ——
	//	> 否则它只是一句替自己开脱的话。
	// ⚠️ 值可以是 `1`（**所有**角色）或**角色名**（只那个角色）——
	//
	//	而后者让"**某个角色的 token 有没有进账**"变成**可证伪的**：
	//
	//		L4_HUGE_TOKENS=design   → 若 design 的 token 真的记账了，它会被拦住
	//		                        → 若没拦住，说明**那个角色没接 `Guard`**
	//
	//	⭐ 而那是"装配完整性"唯一**行为级**的证明 —— 比读代码数 6 处 `guard` 可靠
	//	  （字段在、传下去了、真的接到了，是三件事）。
	if v := os.Getenv("L4_HUGE_TOKENS"); v != "" && (v == "1" || v == c.role) {
		usage.PromptTokens = 10_000_000
	}

	// ★ 故障注入 ①：让某个角色**永远不收敛**（一直提交、一直被拒）。
	//
	//	设计文档里那条叫让某节点**无限重做自己**（Outcome.Next == 自己）——
	//	**那个形态在 cmd/l4 里不存在**：SubAgentNode 返回的 Outcome 的 Next 恒空，
	//	所以图层的 maxRunsPerNode 在这条路径上**永远不触发**（它只在 runner_test.go 里活着）。
	//
	//	所以改成**我们架构里真实存在**的那种打转：**模型不收敛**。
	//
	//	而它拦在 MaxSteps（角色内的界）—— 与 maxRunsPerNode（图层的界）是**两层不同的界**。
	if os.Getenv("L4_LOOP_FOREVER") == c.role {
		// 永远给一份**必被拒**的提交（points 只有 2 条，而要求 3-5 条）
		return tool("loop", "submit_outline", `{"pages":[{"title":"一","points":["a","b"]},{"title":"二","points":["a","b"]},{"title":"三","points":["a","b"]}]}`, usage)
	}

	switch c.role {
	case "research":
		// 第一次：先搜（**不搜就没有 `web_search#N` 的来源，会被角色拒收**）；
		// 第二次：提交结论。
		// ⚠️ 参数一律用 `json.Marshal` 造，**不要手拼字符串** ——
		//	这里踩过一次：`extractTopic` 取的是**角色构建的 prompt**（含换行），
		//	裸插进 JSON 会让参数非法 → 工具根本没执行 → 替身以为"还没搜过" →
		//	**一直重搜到步数上限**。
		//
		//	> 而那次故障的现象是"**达到最大步数**"，与"JSON 拼错"差得很远 ——
		//	> 这正是"**拼接字符串当结构化数据**"最难查的地方。
		q := extractTopic(req)
		if !hasSearched(req) {
			return tool("c1", "web_search", mustJSON(map[string]string{"query": q}), usage)
		}
		return tool("c2", "submit_research", mustJSON(map[string]any{
			"queries": []string{q},
			"findings": []map[string]string{{
				"claim":  "新能源汽车渗透率已达 35%，机构预计 2027 年突破 50%。",
				"source": "web_search#1",
			}},
		}), usage)

	case "outline":
		// ⚠️ 页数从 **`agents.TargetOutlinePages`** 读，而**不是**从 prompt 里抠。
		//
		// # 为什么（这一条是踩出来的）
		//
		//	第一版是 `targetPages(allText(req))` —— 在 prompt 里找"正好 N 页"。
		//	而它**失败了**：prompt 落到哪里（`System` 还是某条 `user`）
		//	是**角色自己的事**，替身看不全；于是解析不出 → 不提交 → 步数打满。
		//
		//	> ### 替身不该**解析 prompt 去猜意图**，它该读**那个意图的来源**。
		//
		//	⭐ 而"prompt 里的 3 页"与"`decodeOutline` 校验的 3 页"**本来就是
		//	  同一个常量**（`outline.go` 的注释原话："它同时被用在两个地方，且只写一次"）。
		//	  所以读它**不是抄近路，是读同一个来源**。
		//
		// ⭐ 而"改 Prompt → 指标动"**照样能演示**：改 `TargetOutlinePages` 的值 →
		//	prompt 变了、替身交的页数也变了 → **而用例的 `pages` 期望还是 3** → **判据红** ✅
		n := agents.TargetOutlinePages
		if n <= 0 {
			// ⚠️ 见 `ruleClient` 的注释：**解析不到就不提交**，让用例红。
			return &llm.Response{Content: "（替身读不出页数 —— 不提交）", Usage: usage}, nil
		}
		return tool("c1", "submit_outline", outlineArgs(n), usage)

	case "design":
		n := agents.TargetOutlinePages
		return tool("c1", "submit_design", designArgs(n, attemptOf(req, "submit_design")), usage)
	}
	return nil, fmt.Errorf("未知角色 %q", c.role)
}

// tool 造一个"模型请求调用工具"的响应。
func tool(id, name, args string, u llm.Usage) (*llm.Response, error) {
	return &llm.Response{ToolCalls: []message.ToolCall{{ID: id, Name: name, Arguments: args}}, Usage: u}, nil
}

// hasSearched 报告对话历史里有没有工具回执（= 已经搜过了）。
func hasSearched(req llm.Request) bool {
	for _, m := range req.Messages {
		if m.Role == message.RoleTool {
			return true
		}
	}
	return false
}

// targetPages 从系统提示里抠出"正好 N 页"。
//
// ⚠️ 与 `cmd/l3` 的 `answerNote` 是同一种**演示脚手架的取巧**：
//
//	它依赖 Prompt 的措辞。真实项目里测试替身该更稳（比如让角色**声明**它要几页）。
func targetPages(s string) int {
	i := strings.Index(s, "正好")
	if i < 0 {
		return 0
	}
	rest := strings.TrimLeft(s[i+len("正好"):], " \t")
	j := strings.Index(rest, "页")
	if j < 0 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest[:j]))
	if err != nil {
		return 0
	}
	return n
}

func extractTopic(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == message.RoleUser {
			return trim(req.Messages[i].Content, 40)
		}
	}
	return "主题"
}

// allText 把**全部消息**拼起来 —— 替身在这一整段里找"正好 N 页"。
//
// ⚠️ 为什么不是"只看最后一条 user 消息"：那个 prompt 到底落在
//
//	`System` 还是某条 `user` 里，**是角色自己的事**（而我们不该抄它的实现细节）。
//	替身读的是"**模型能看到的一切**" —— 那是它在真实场景里真正拥有的输入。
//
//	⭐ 这也让它对"角色改了 prompt 的组织方式"**免疫** ——
//	  而一个脆弱的替身会在角色重构时**静默失效**（表现成"模型没提交"）。
func allText(req llm.Request) string {
	var b strings.Builder
	b.WriteString(req.System)
	for _, m := range req.Messages {
		b.WriteString("\n")
		b.WriteString(m.Content)
	}
	return b.String()
}

func lastUser(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == message.RoleUser {
			return req.Messages[i].Content
		}
	}
	return ""
}

// approxTokens 是一个**粗糙但确定**的 token 估算（字数 / 2）。
func approxTokens(s string) int { return len([]rune(s)) / 2 }

// mustJSON 把任意值序列化成**工具参数**。
//
// ⚠️ 它存在的理由是"**别再手拼 JSON**" —— 见 `Generate` 里那段踩坑记录。
//
//	（序列化失败时返回一个注定被拒的 `{}`，而不是 panic：
//	 替身是**演示脚手架**，它崩掉会掩盖掉真正的断言失败。）
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func outlineArgs(n int) string {
	titles := []string{"行业现状", "增长的三重压力", "结论与建议", "风险提示", "附录"}
	var b strings.Builder
	b.WriteString(`{"pages":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		t := titles[i%len(titles)]
		// ⚠️ **3 条** —— 不是随便写的：`decodeOutline` 的下游要求
		//	"正文页 3-5 条（首页/末页 1-5 条）"。我第一版给了 2 条，
		//	于是它被拒了 8 次（用满 `MaxSteps`）。
		//
		//	⭐ 而发现这条规则的方式值得记：**不是去读代码，是让工具把拒绝原因打出来**
		//	  （见 `dumpToolResults`）。我在这件事上先猜了 4 次 ——
		//	  而那 4 次加起来，比写那一个诊断函数贵得多。
		fmt.Fprintf(&b, `{"title":%q,"points":["要点 A","要点 B","要点 C"]}`, t)
	}
	b.WriteString(`]}`)
	return b.String()
}

// designArgs 造一份设计提交 —— 而**第几次提交**会改变它的内容。
//
// # ⚠️ 那个 `attempt` 参数是这一轮最重要的一个设计决定
//
//	第一版它是 `designArgs(n)` —— **无状态的**：每次都回一模一样的东西。
//	于是 `submit_design` 被拒 8 次、用满步数，而**原因一直没变**。
//
//	> ### 一个无状态的替身，会让角色的"**回灌自愈**"路径**永远走不通**。
//	> 而"自愈"恰恰是这个项目反复强调的能力（`TestDesignerSelfHealsOnInvalidLayout`
//	> 就是它的看门人）—— **所以那个评测永远测不到它。**
//
//	⭐ 而"解析回执里的错误文本"也不是正确形态：真实模型**不知道**产品规则
//	  （"末页必须是 closing" 那条在 `decode` 的校验里，**不在 Prompt 里**）。
//	  它能做的只是**换个策略再试**。
//
//	所以第一版给错的（全 `data`）、第二版给对的 —— **而那正是 ReAct 的本意**：
//
//		**不知道规则，但会因为被拒而改。**
//
// ⚠️ 字段名 `pages` 是从 `designer_test.go` 的 `threeDesignsArgs`
//
//	（一份**能通过的**提交）里抄来的，不是猜的 —— 我第一版写的是 `designs`，
//	于是报"**实际给了 0 份**"（解不出来），而不是"页数不对"。**两句话说的是两件事。**
func designArgs(n, attempt int) string {
	pages := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		layout := "data" // 第 1 次：**故意**用一个不满足"末页规则"的组合
		switch {
		case i == 0:
			layout = "cover"
		case i == n-1 && attempt > 0:
			layout = "closing" // 第 2 次：听进去了，改对末页
		}
		pages = append(pages, map[string]any{
			"page_index": i,
			"layout":     layout,
			"note":       fmt.Sprintf("版式 %d", i+1),
		})
	}
	return mustJSON(map[string]any{"pages": pages})
}

// attemptOf 数"这个角色已经提交过几次 `tool`" —— 从对话历史里数。
//
// ⚠️ 它是替身的**唯一状态** —— 而那个状态**不属于替身**：
//
//	它存在**对话历史**里（角色的 ReAct 循环自己维护的那个）。
//
//	> ### 替身是纯函数：`(System, Messages) → Response`。
//	> 凡是它"需要记住"的，都在 `Messages` 里 —— 因为**真实模型也只有那个**。
func attemptOf(req llm.Request, tool string) int {
	n := 0
	for _, m := range req.Messages {
		for _, tc := range m.ToolCalls {
			if tc.Name == tool {
				n++
			}
		}
	}
	return n
}

// ─── 初始用例集 ─────────────────────────────────────────────────────

// dumpCases 写一份**能跑起来**的初始用例集 —— 而它的价值是"人有东西可改"。
//
// ⚠️ 它**不是**"评测集的全部"：一份好的用例集是**改出来的**
//
//	（每一条都对应一次真实的回归），而不是生成出来的。
func dumpCases() error {
	cases := []eval.Case{
		{
			ID: "baseline-3p", Topic: "新能源汽车行业趋势", Tags: []string{"smoke"},
			Expect: eval.Expectation{
				MustHaveKinds: []schema.Kind{schema.KindResearch, schema.KindOutline, schema.KindDesign, schema.KindRender},
				MinFindings:   1, Pages: 3, MaxSteps: 12, MaxTokens: 20000,
				MustContain: []string{"行业现状"},
			},
		},
		{
			ID: "short-topic", Topic: "AI", Tags: []string{"smoke"},
			Expect: eval.Expectation{
				MustHaveKinds: []schema.Kind{schema.KindOutline, schema.KindRender},
				Pages:         3, MaxSteps: 12,
				MustNotContain: []string{"panic", "undefined"},
			},
		},
		{
			// ★ **这一条是故意的**：它的期望是 5 页，而角色的 Prompt 要 3 页。
			//	它**必须红** —— 否则"失败"这条路径从没被走过，
			//	而"从没红过的判据"是一种伪装成保障的空白（R36）。
			ID: "expect-mismatch", Topic: "新能源汽车行业趋势", Tags: []string{"self-test"},
			Expect: eval.Expectation{Pages: 5, MaxSteps: 12},
		},
	}
	raw, err := eval.CaseSet{Cases: cases}.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(caseFile), 0o755); err != nil {
		return err
	}
	return os.WriteFile(caseFile, raw, 0o644)
}

// ─── 小工具 ─────────────────────────────────────────────────────────

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "（全部）"
	}
	return s
}

func trim(s string, n int) string {
	rs := []rune(strings.TrimSpace(s))
	if len(rs) <= n {
		return string(rs)
	}
	return string(rs[:n])
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n[失败] "+format+"\n", args...)
	os.Exit(1)
}

var _ = errors.New // 保留：`errors` 在排查分支里会用上
