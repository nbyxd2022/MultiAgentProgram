package eval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"multiagent/internal/observ"
	"multiagent/internal/schema"
)

// ─── 端口：怎么跑 ───────────────────────────────────────────────────

// Runner 跑**一个**用例 —— 它是一个**端口**。
//
// # ⚠️ 为什么 `eval` 不该知道"怎么跑"
//
//	判据与 `ConsoleTracer.WithIndent` 那个决定**完全相同**：
//
//		**"怎么跑"是领域知识（配哪几个角色 / 用哪个模型 / 什么预算），
//		  而"怎么判"才是评测的职责。**
//
//	把它写进 `eval`，等于让评测框架**只能评一种东西**
//	（而"评另一种东西"时你得改框架）。
//
//	⚠️ 而调用方拿到这个端口之后，**必须自己装配并接一个 `Recorder`** ——
//	因为判据要看的 `Outcome.Events` 就是从那来的。
//	（`cmd/l4` 里那段装配是**唯一的**消费者，而它同时也是"怎么跑"的定义。）
type Runner func(ctx context.Context, c Case) (Outcome, error)

// Outcome 是"跑了一个用例"之后，**判据能看到的全部东西**。
//
// ⚠️ 它刻意**不是**"Run 的返回值" —— 它是从**可观测的东西**里抽出来的：
// 黑板（产物）与事件流（过程）。于是"判据"与"怎么跑"之间只有这一层薄接口。
type Outcome struct {
	// Board 是这一轮结束时的黑板 —— **中间产物**。
	Board map[string][]schema.Artifact

	// Rendered 是**给用户的最终产物**（如每页的 HTML 全文）。
	//
	// ⚠️ 它与 `Board` **不是一回事**，这一点很容易混：
	//
	//		`Board`        装的是**中间产物**（大纲 / 版式 / 素材）
	//		`Rendered`     装的是**用户最终看到的那份东西**
	//
	//	而 `MustContain` 问的是"**用户最终看到的东西里**有没有这句话" ——
	//	问错对象会得到一个恒真的判据（中间产物里当然有大纲里的词）。
	Rendered []string

	// Events 是这一轮的**全部事件**（`Recorder.Events()`）。
	//
	// ⚠️ 指标**从这里算**，而不是让 `Runner` 顺便返回一份 ——
	//	R47 那条："**总量只能有一份明细**"（可以有很多统计，但必须同源）。
	//	多返回一份"步数/token"就等于**另开一本账**，而两本账会漂移。
	Events []observ.Event
}

// ─── 指标 ───────────────────────────────────────────────────────────

// Metrics 是从**事件流**算出来的指标。
//
// # ⚠️ 两个 `Steps` 名字很像，而它们是**两个问题**（第一次跑真实用例时撞出来的）
//
//	跑第一个真实用例时，`max_steps` 报"期望 ≤12，实际 **30**" ——
//	而那一轮只有 4 个节点。多出来的 26 是从哪来的？
//
//	**`KindStart` 有两层都在发**：
//
//		图层（`Runner`）     每个节点发一次        → 4 条
//		agent 层（每个角色）  每轮 ReAct 迭代发一次 → 每个角色若干条
//
//	而 `Count(KindStart)` 把**两层混在了一起**。
//
//	> ### 这不是"指标算错了"，是**"步数"这个词在两个问题下含义不同**：
//	> 图层的"一步" = 跑了一个节点；agent 层的"一步" = 一轮 ReAct 迭代。
//	> **而 `Expectation.MaxSteps` 问的是前者**（"这一轮跑了几跳"）。
//
//	⭐ 这与 R43 那条同源：**同一个词在两个问题下含义不同 —— 用之前先说清是哪一个。**
//	  而 `observ.Event.Layer` **正是为这件事存在的**（它的注释写着"它回答'哪一层'"）——
//	  只是在此之前，没有任何一个消费者**用它筛过**。
//
// ⚠️ 两个都留着，且**都标清楚**：图层的是判据，agent 层的是**成本**的原料
//
//	（"4 个节点、共 26 轮迭代，平均每个节点 6.5 轮"是排查"为什么这么贵"的第一维）。
type Metrics struct {
	// Steps 是**图跳数**（跑过几个节点）—— `Expectation.MaxSteps` 问的就是它。
	Steps int

	// AgentSteps 是所有角色的 ReAct 迭代**总数**（跨角色累加）。
	//
	// ⚠️ 它**不是** `Steps` 的细分，是**另一个量**：4 个节点可能跑 26 轮迭代。
	AgentSteps int

	PromptTokens     int
	CompletionTokens int
	Wall             time.Duration
}

// metricsOf 只做一件事：把明细**加起来**。
//
// ⚠️ 它认的 Kind 是**写死的三个**（`llm_call` / `start` / 全部事件的 `Duration`）——
//
//	而这是**故意的**：口径散在多处就会漂移（`Recorder.Usage` 的注释同一条）。
//
// ⚠️ 而 `KindStart` **必须按 `Layer` 分流** —— 见 `Metrics` 的长注释：
//
//	两层都发 `start`，混起来得到的那个数**不是任何一个问题想要的**。
func metricsOf(events []observ.Event) Metrics {
	var m Metrics
	for _, e := range events {
		switch {
		case e.Kind == observ.KindStart && e.Layer == observ.LayerGraph:
			m.Steps++
		case e.Kind == observ.KindStart:
			// agent 层（或别的层）的迭代 —— 它不是"图跳数"。
			m.AgentSteps++
		case e.Kind == observ.KindLLMCall:
			m.PromptTokens += e.PromptTokens
			m.CompletionTokens += e.CompletionTokens
		}
		m.Wall += time.Duration(e.Duration)
	}
	return m
}

// ─── 判据 ───────────────────────────────────────────────────────────

// check 是**一条**判据的结果。
//
// # ⚠️ 三个字段缺一不可
//
//	一个只会说"失败"的判据，排查的人得自己去算期望和实际 ——
//	**而算错一次就多绕一圈**（`observ.Reconcile.Diff` 的注释同一条）。
//
//	所以每条判据都必须能渲染成 "**名字**：期望 X，实际 Y"。
type check struct {
	name string // 判据名（"pages" / "min_findings" …）—— **可 grep、可讨论**
	want string
	got  string
	ok   bool
}

func (c check) String() string {
	if c.ok {
		return c.name + "：✅"
	}
	return fmt.Sprintf("%s：期望 %s，实际 %s", c.name, c.want, c.got)
}

// checks 跑**全部**判据（而不是遇到第一个失败就返回）。
//
// ⚠️ 这是刻意的：一个用例一次只报一条失败，会让人**修一条、再跑一次、再看到下一条** ——
//
//	而每一次都花一次运行的代价。**判据是免费的，运行不是。**
func (c Case) checks(o Outcome, m Metrics) []check {
	e := c.Expect
	var out []check

	// ── 产物：有哪些 ──
	out = append(out, c.checkKinds(o))

	// ── 产物：里面有什么 ──
	if e.MinFindings > 0 {
		n, err := countIn(o.Board, schema.KindResearch,
			func(r schema.Research) int { return len(r.Findings) })
		out = append(out, numberCheck("min_findings", "≥"+itoa(e.MinFindings), n, err,
			func(n int) bool { return n >= e.MinFindings }))
	}
	if e.Pages > 0 {
		n, err := countIn(o.Board, schema.KindOutline,
			func(ol schema.Outline) int { return len(ol.Pages) })
		out = append(out, numberCheck("pages", itoa(e.Pages), n, err,
			func(n int) bool { return n == e.Pages }))
	}

	// ── 最终产物里的话 ──
	// ⚠️ 注意查的是 `o.Rendered`（**用户看到的那份**），不是 `Board`。
	all := strings.Join(o.Rendered, "\n")
	for _, s := range e.MustContain {
		out = append(out, check{
			name: "must_contain", want: fmt.Sprintf("含 %q", s),
			got: pick(all, s != "" && strings.Contains(all, s), "不含"),
			ok:  strings.Contains(all, s),
		})
	}
	for _, s := range e.MustNotContain {
		has := strings.Contains(all, s)
		out = append(out, check{
			name: "must_not_contain", want: fmt.Sprintf("不含 %q", s),
			got: pick(all, !has, fmt.Sprintf("含 %q", s)),
			ok:  !has,
		})
	}

	// ── 成本 / 时间：都是**上界** ──
	out = append(out, upper("max_steps", e.MaxSteps, m.Steps)...)
	out = append(out, upper("max_tokens", e.MaxTokens, m.PromptTokens+m.CompletionTokens)...)
	if e.MaxWall > 0 {
		out = append(out, check{
			name: "max_wall", want: "≤" + e.MaxWall.String(),
			got: m.Wall.Round(time.Millisecond).String(),
			ok:  m.Wall <= e.MaxWall,
		})
	}
	return out
}

// numberCheck 把"数出来的值"渲染成一条判据（**解不出来时也说清楚**）。
func numberCheck(name, want string, got int, err error, ok func(int) bool) check {
	if err != nil {
		return check{name: name, want: want, got: "读不出来：" + oneLine(err.Error())}
	}
	return check{name: name, want: want, got: itoa(got), ok: ok(got)}
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// upper 生成一条"上界"判据（`limit <= 0` = 不检查）。
func upper(name string, limit, got int) []check {
	if limit <= 0 {
		return nil
	}
	return []check{{
		name: name, want: fmt.Sprintf("≤%d", limit),
		got: fmt.Sprintf("%d", got), ok: got <= limit,
	}}
}

// checkKinds 检查"必须产出的产物种类"。
//
// ⚠️ 它只报**缺的**，不报多的 —— "多产出了 design"不是失败
//
//	（那是流程的自由度，而"自由度"不是回归）。
func (c Case) checkKinds(o Outcome) check {
	var missing []string
	for _, k := range c.Expect.MustHaveKinds {
		if len(o.Board[string(k)]) == 0 {
			missing = append(missing, string(k))
		}
	}
	sort.Strings(missing)
	return check{
		name: "must_have_kinds",
		want: fmt.Sprintf("%d 种", len(c.Expect.MustHaveKinds)),
		got:  pick("", len(missing) == 0, "缺 "+strings.Join(missing, "、")),
		ok:   len(missing) == 0,
	}
}

// pick 是一个**可读性**小工具：`ok` 时返回 `a`，否则返回 `b`。
//
// ⚠️ 它存在的理由是"判据的**实际值**那一栏要短" ——
//
//	表格里塞一段 HTML 会把整张表冲掉（而看不见的表等于没有）。
func pick(a string, ok bool, b string) string {
	if ok {
		return a
	}
	return b
}

// countIn 解出某一类产物的**最新版**并数一个数。
//
// # ⚠️ 解不出来时返回 error，而**不是** 0
//
//	因为"**真的 0 条素材**"与"**读不出来**"在判据表里长得一模一样
//	（都是 `0`），而它们要去看的地方**完全不同**：
//
//		真的 0 条 → 去看那个**角色**（它为什么没产出结论）
//		读不出来  → 去看**产物本身**（信封与 payload 的 kind 对不上 / JSON 坏了）
//
//	> **判据说错方向，比不说更贵** —— 它让人去错地方
//	> （L3 复盘那条"误导比缺失贵"是同一条）。
//
// ⚠️ 它用 `schema.Decode` 而不是 `json.Unmarshal` —— 前者**双向校验 Kind**
//
//	（信封说它是什么 + 产物自己也必须这么说），于是"装错了的产物"
//	不会被静默地当成对的东西读出来。
func countIn[T schema.Payload](
	board map[string][]schema.Artifact, kind schema.Kind, f func(T) int,
) (int, error) {
	latest := latestOf(board, string(kind))
	if latest == nil {
		return 0, nil // 根本没产出 —— 那不是"读不出来"，`must_have_kinds` 会单独报它
	}
	v, err := schema.Decode[T](*latest)
	if err != nil {
		return 0, err
	}
	return f(v), nil
}

// latestOf 取某一类产物的**最新版**（没有则 nil）。
func latestOf(board map[string][]schema.Artifact, key string) *schema.Artifact {
	vs := board[key]
	if len(vs) == 0 {
		return nil
	}
	return &vs[len(vs)-1]
}

// ─── 判定与报告 ─────────────────────────────────────────────────────

// Verdict 是一个用例的判定结果。
type Verdict struct {
	CaseID  string
	Passed  bool
	Failed  []string // **每一条失败都说清了是哪一条判据**（可执行）
	Metrics Metrics

	// Err 非空表示**这个用例根本没跑完**（与"跑完了但判据没过"是两件事）。
	//
	// ⚠️ 分开的理由：两者的**处置完全不同** ——
	//	判据没过 → 去看产物；跑不完 → 去看错误（可能是预算 / 模型 / 图的问题）。
	//	合并成一个"失败"会让人**去错地方**。
	Err error

	// Judgement 是**软判据**的结果（没配 `Judge` 时为 nil）。
	//
	// # ⚠️⚠️ 它**不参与 `Passed`** —— 而这是这一轮最重要的一个决定
	//
	//	让一个**概率**值决定"通过/失败"，就是让评测**开始 flake**；
	//	而 flake 的评测比没有评测**更坏**：它会训练人**忽略红色**。
	//
	//	⭐ 那它有什么用？—— 它是**给人看的第二列信息**：
	//	  "这次硬判据全过，而 `complete` 从 4 分掉到 2 分" —— **那是一条线索**，
	//	  而人看到线索会去看产物。
	//
	//	> ### 线索不需要决定成败，它需要被看见。
	//	> （判据的**性质**决定了它能被用在哪 —— 与 R23 那条"拿时间当判据会 flake"同一族。）
	Judgement *Judgement

	// JudgeErr 非空表示**软判据没能用上**（自愈也失败了）。
	//
	// ⚠️ 它**不是**"0 分" —— 把"没跑成"记成 0 分会**污染平均值**
	//	（与 `countIn` 那条"真的 0 条 vs 读不出来"是同一条：
	//	**两种东西长得一样时，数据会开始说谎**）。
	JudgeErr error
}

// Options 是"怎么跑这一批"。
//
// ⚠️ 它取代了原来的 `Run(ctx, set, run)` 三个位置参数 —— 而那一步是被
// **软判据**（第 6 项）逼出来的：加第四个参数会让那个调用**读不出来**。
//
//	Run(ctx, set, run, judge, scope)      // ← 四个位置参数，读者得去数
//	Run(ctx, set, Options{Run: run, Judge: j})   // ← 自解释
//
//	⭐ 判据与 `Runner` 被设计成端口是同一条：**接口的形状来自使用它的那一行**。
type Options struct {
	// Run 跑一个用例 —— **必填**（为 nil 时 `Run` 返回空，见下）。
	Run Runner

	// Judge 是软判据。**可以为 nil** —— 那表示"这一轮不跑软判据"。
	//
	// ⚠️ 而 nil **不等于** `Nop`（与 `observ.Tracer` 那条**相反**）——
	//	因为这两件事的读法不同：
	//
	//		`Tracer` 为 nil  → 仍然要**接住**事件（只是没人看）→ 用空对象
	//		`Judge` 为 nil   → **根本没有"软判据"这件事** → 用 nil
	//
	//	> 空对象治的是"**这一层不该判空**"；而这里**该判** ——
	//	> 因为"没配"与"配了一个什么都不做的"在报告里**必须长得不一样**
	//	> （前者不显示那一列，后者显示一列空）。
	Judge Judge
}

// Run 跑一批用例，返回每个用例的判定。
//
// ⚠️ **没有"并发跑"** —— 而那是刻意的（R38 原理三：预算检查与记账分离
//
//	在有并发时需要预留 + 结算，而我们还没想清那件事）。
//	现在的形态是**串行 + 每个用例独立**，于是"一个用例的用量"
//	**永远不会串到另一个**（`Outcome.Events` 是每个用例自己的 Recorder）。
func Run(ctx context.Context, set CaseSet, opt Options) []Verdict {
	if opt.Run == nil {
		// ⚠️ 没配 `Run` 是**调用方的 bug** —— 而返回"每个用例都跑不完"
		//	比 panic 好：报告里会明确写出"没有配置 runner"。
		out := make([]Verdict, 0, len(set.Cases))
		for _, c := range set.Cases {
			out = append(out, Verdict{CaseID: c.ID, Err: errNoRunner})
		}
		return out
	}
	out := make([]Verdict, 0, len(set.Cases))
	for _, c := range set.Cases {
		out = append(out, runOne(ctx, c, opt))
	}
	return out
}

var errNoRunner = fmt.Errorf("没有配置 Runner —— 这一批用例一个都没跑")

func runOne(ctx context.Context, c Case, opt Options) Verdict {
	o, err := opt.Run(ctx, c)
	if err != nil {
		// ⚠️ 跑不完**不是**"判据没过" —— 见 `Verdict.Err` 的注释。
		//	⚠️ 而它**也不跑软判据** —— 产物都没有，让模型评什么？
		return Verdict{CaseID: c.ID, Err: err}
	}

	m := metricsOf(o.Events)
	res := c.checks(o, m)

	failed := make([]string, 0, len(res))
	for _, r := range res {
		if !r.ok {
			failed = append(failed, r.String())
		}
	}
	v := Verdict{
		CaseID:  c.ID,
		Passed:  len(failed) == 0,
		Failed:  failed,
		Metrics: m,
	}

	// ★ 软判据 —— 而它**只填 Judgement / JudgeErr，不碰 Passed**（见那两个字段的注释）。
	if opt.Judge != nil {
		j, jerr := judgeOne(ctx, opt.Judge, c, o)
		if jerr != nil {
			v.JudgeErr = jerr
		} else {
			v.Judgement = &j
		}
	}
	return v
}

// Summary 是一批用例的汇总（报告的最后一行）。
type Summary struct {
	Total     int
	Passed    int
	Failed    int
	Errored   int
	AvgPrompt int
	AvgWall   time.Duration
}

// Rate 是成功率（**分母是全部用例** —— 跑不完的也算不成功）。
func (s Summary) Rate() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Passed) / float64(s.Total) * 100
}

// Summarize 汇总一批判定。
//
// # ⚠️ 平均值**只对跑完的用例算**
//
//	一个**跑不完**的用例，它的耗时与 token **不是一个有意义的样本**
//	（它可能是"5 毫秒内就撞上了预算"）—— 把它算进平均会把均值**拉向一个假的位置**。
//
//	> 这与参考实现那条"**比率不能用全局的两个总量算**"是同一族：
//	> **一个统计的分母，必须与它想问的问题一致。**
func Summarize(vs []Verdict) Summary {
	s := Summary{Total: len(vs)}
	var promptSum int
	var wallSum time.Duration
	var n int

	for _, v := range vs {
		switch {
		case v.Err != nil:
			s.Errored++
		case v.Passed:
			s.Passed++
		default:
			s.Failed++
		}
		if v.Err == nil {
			promptSum += v.Metrics.PromptTokens + v.Metrics.CompletionTokens
			wallSum += v.Metrics.Wall
			n++
		}
	}
	if n > 0 {
		s.AvgPrompt = promptSum / n
		s.AvgWall = wallSum / time.Duration(n)
	}
	return s
}

// Render 把判定渲染成那张**指标表**（教学计划的产出物 ②）。
//
// ⚠️ 失败的用例**必须在表里说清是哪一条判据** —— 否则"12 个用例 3 个失败"
//
//	是一句没法行动的话。
func Render(vs []Verdict) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%-16s %-6s %5s %8s %7s %8s  %s\n",
		"用例", "结果", "步数", "prompt", "comp", "耗时", "判据失败")
	b.WriteString(strings.Repeat("-", 96) + "\n")

	for _, v := range vs {
		if v.Err != nil {
			fmt.Fprintf(&b, "%-16s %-6s %5s %8s %7s %8s  %s\n",
				v.CaseID, "💥", "—", "—", "—", "—", "跑不完："+oneLine(v.Err.Error()))
			continue
		}
		res := "✅"
		if !v.Passed {
			res = "❌"
		}
		m := v.Metrics
		reason := "—"
		if len(v.Failed) > 0 {
			// ⚠️ **只报第一条**（表宽有限），而**全部**在下面详列。
			reason = v.Failed[0]
			if len(v.Failed) > 1 {
				reason += fmt.Sprintf("（另 %d 条）", len(v.Failed)-1)
			}
		}
		fmt.Fprintf(&b, "%-16s %-6s %5d %8d %7d %8s  %s\n",
			v.CaseID, res, m.Steps, m.PromptTokens, m.CompletionTokens,
			m.Wall.Round(time.Millisecond), reason)
	}

	s := Summarize(vs)
	b.WriteString(strings.Repeat("-", 96) + "\n")
	fmt.Fprintf(&b, "合计 %d 个 · 通过 %d · 失败 %d · 跑不完 %d · 成功率 %.1f%% · 平均 token %d · 平均 %s\n",
		s.Total, s.Passed, s.Failed, s.Errored, s.Rate(), s.AvgPrompt,
		s.AvgWall.Round(time.Millisecond))

	// 详列：**每一条**失败判据。
	var detail []string
	for _, v := range vs {
		for _, f := range v.Failed {
			detail = append(detail, fmt.Sprintf("  %s  %s", v.CaseID, f))
		}
	}
	if len(detail) > 0 {
		b.WriteString("\n失败明细（每一条都说清了是哪一条判据）：\n")
		b.WriteString(strings.Join(detail, "\n") + "\n")
	}
	return b.String()
}

func oneLine(s string) string {
	return observ.OneLine(s)
}
