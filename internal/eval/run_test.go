package eval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"multiagent/internal/observ"
	"multiagent/internal/schema"
)

// ─── 夹具 ───────────────────────────────────────────────────────────

// staticRunner 返回一份**固定**的结果 —— 于是"判据"能被**单独**测，
// 不需要真的跑一遍图。
//
// ⚠️ 这个夹具是 `Runner` 被设计成**端口**的直接红利（见 `run.go` 里它的注释）：
//
//	如果 `eval` 自己知道"怎么跑"（配哪几个角色 / 调哪个模型），
//	那么**测一条判据就得跑一整轮真实流程** —— 而一轮要花钱、要好几秒、
//	还会因为模型的行为而 flake。
//
//	> ### 端口的价值不只在"换实现"，还在"**让被测的东西变小**"。
func staticRunner(o Outcome, err error) Runner {
	return func(context.Context, Case) (Outcome, error) { return o, err }
}

// outlineArtifact 造一份 N 页的大纲产物（够 `pages` / `min_findings` 那类判据用）。
func outlineArtifact(t *testing.T, pages int) schema.Artifact {
	t.Helper()
	ps := make([]schema.PageOutline, 0, pages)
	for i := 0; i < pages; i++ {
		ps = append(ps, schema.PageOutline{Index: i, Title: "页", Points: []string{"点"}})
	}
	a, err := schema.NewArtifact(string(schema.KindOutline), "outline", schema.Outline{Pages: ps})
	if err != nil {
		t.Fatalf("造产物失败: %v", err)
	}
	return a
}

// outcomeWith 造一个"跑完了"的结果。
func outcomeWith(board map[string][]schema.Artifact, rendered []string, events ...observ.Event) Outcome {
	return Outcome{Board: board, Rendered: rendered, Events: events}
}

func pageCase(id string, pages int) Case {
	return Case{ID: id, Topic: "主题", Expect: Expectation{Pages: pages}}
}

// ─── ★★★ 判据的判据：它能红吗？它能绿吗？ ────────────────────────────

// ⚠️ 这两条（本测试 + 下面那条）是**一个评测框架最该有的两个测试**。
//
//	因为一个评测框架**最危险的失败模式不是"报错了"，是"永远报通过"**：
//
//		· 报错 → 你会去修
//		· 永远报通过 → **你以为一切正常**
//
//	> ### 所以"评测"这件事本身要有一组判据，而它必须**成对**：
//	> 给一个**必然不满足**的期望 → 它必须红；
//	> 给一个**必然满足**的期望 → 它必须绿。
//	>
//	⭐ 只测其中一条都是**假的**：
//	> 一个恒红的框架"很严格"（而它没用），一个恒绿的框架"很稳"（而它也没用）。
//	（这与 R36 那条"否定形式的断言必须配正例前置"是同一条。）
func TestChecksCanFail(t *testing.T) {
	o := outcomeWith(map[string][]schema.Artifact{
		string(schema.KindOutline): {outlineArtifact(t, 3)}, // 实际 3 页
	}, []string{"<html>正文</html>"})

	vs := Run(context.Background(), CaseSet{Cases: []Case{pageCase("三页", 5)}}, Options{Run: staticRunner(o, nil)})
	if len(vs) != 1 {
		t.Fatalf("应当有 1 条判定，实际 %d", len(vs))
	}
	v := vs[0]

	if v.Passed {
		t.Fatal("期望 5 页而实际 3 页 —— 判据**必须红**。" +
			"一个不会红的判据是**伪装成保障的空白**（它绿着，于是没人去看它）")
	}
	if v.Err != nil {
		t.Fatalf("这是**判据没过**，不是**跑不完**：%v", v.Err)
	}

	// ⚠️ 而失败信息必须**可执行**：说清是哪一条、期望什么、实际什么。
	//	只说"失败"的判据，排查的人得自己去算 —— 而**算错一次就多绕一圈**。
	joined := strings.Join(v.Failed, " | ")
	for _, want := range []string{"pages", "期望 5", "实际 3"} {
		if !strings.Contains(joined, want) {
			t.Errorf("失败信息里应当有 %q（一条判据的三个字段缺一不可），实际：%s", want, joined)
		}
	}
}

// ⚠️ 与上一条**成对**——见它的注释。
func TestChecksCanPass(t *testing.T) {
	o := outcomeWith(map[string][]schema.Artifact{
		string(schema.KindOutline): {outlineArtifact(t, 3)},
	}, []string{"<html>正文</html>"})

	vs := Run(context.Background(), CaseSet{Cases: []Case{pageCase("三页", 3)}}, Options{Run: staticRunner(o, nil)})

	if !vs[0].Passed {
		t.Fatalf("期望 3 页而实际 3 页 —— 判据该绿，实际报：%v", vs[0].Failed)
	}
	if len(vs[0].Failed) != 0 {
		t.Errorf("通过时不该有失败明细，实际 %v", vs[0].Failed)
	}
}

// ─── ★★ "跑不完"与"判据没过"是**两件事** ────────────────────────────

// ⚠️ 合并成一个"失败"会让人**去错地方**：
//
//	判据没过 → 去看**产物**（角色产出的东西不对）
//	跑不完   → 去看**错误**（可能是预算 / 模型 / 图的问题）
//
// ⭐ 而更坏的一种实现是"跑不完时返回一个空的 Outcome 去跑判据" ——
//
//	那样它会报"期望 5 页，实际 0 页"，**一个把人引向产物的错误方向**。
func TestRunOneSeparatesErroredFromFailed(t *testing.T) {
	boom := errors.New("预算不够，停止推进")
	vs := Run(context.Background(), CaseSet{Cases: []Case{pageCase("炸了", 5)}}, Options{Run: staticRunner(Outcome{}, boom)})

	v := vs[0]
	if v.Err == nil {
		t.Fatal("跑不完时 `Err` 必须非空 —— 否则它会被当成「判据没过」")
	}
	if v.Passed {
		t.Error("跑不完不该算通过")
	}
	if len(v.Failed) != 0 {
		t.Errorf("跑不完时**不该**有判据明细（一条判据都没跑）—— "+
			"而 `%v` 会把排查的人引向产物，而真正的原因在错误里", v.Failed)
	}
	if !strings.Contains(v.Err.Error(), "预算不够") {
		t.Errorf("错误要原样带出来（它是排查的唯一线索），实际 %v", v.Err)
	}
}

// ─── ★★ 指标：图跳数 ≠ agent 迭代数（R48 那条发现的看门人）──────────

// ⚠️ 这是 R48 第一次跑真实用例时撞出来的：`max_steps` 报 30 而那一轮只有 4 个节点。
//
//	根因是 `KindStart` **两层都发**，而 `Count(KindStart)` 把它们混在了一起。
//
//	> ### "步数"这个词在两个问题下含义不同 —— 而混起来得到的那个数
//	> **不是任何一个问题想要的**。
func TestMetricsSeparateGraphStepsFromAgentSteps(t *testing.T) {
	events := []observ.Event{
		// 图层：4 跳（4 个节点）
		{Layer: observ.LayerGraph, Kind: observ.KindStart},
		{Layer: observ.LayerGraph, Kind: observ.KindStart},
		{Layer: observ.LayerGraph, Kind: observ.KindStart},
		{Layer: observ.LayerGraph, Kind: observ.KindStart},
		// agent 层：26 轮 ReAct 迭代（跨 4 个角色）
		{Layer: observ.LayerAgent, Kind: observ.KindStart},
		{Layer: observ.LayerAgent, Kind: observ.KindStart},
		// 一次模型调用
		{Layer: observ.LayerLLM, Kind: observ.KindLLMCall, PromptTokens: 100, CompletionTokens: 20},
	}
	m := metricsOf(events)

	if m.Steps != 4 {
		t.Errorf("`Steps` 应当是**图跳数** 4（`Expectation.MaxSteps` 问的就是它），实际 %d", m.Steps)
	}
	if m.AgentSteps != 2 {
		t.Errorf("`AgentSteps` 应当是 agent 层的迭代数 2（**另一个量**，不是 `Steps` 的细分），实际 %d", m.AgentSteps)
	}
	if m.PromptTokens != 100 || m.CompletionTokens != 20 {
		t.Errorf("用量算错了：%d / %d", m.PromptTokens, m.CompletionTokens)
	}
}

// ⚠️ 而这条守着"**判据用的是哪一个数**" ——
//
//	`max_steps` 若错用了 `AgentSteps`，那它测的就不是"图打转了"，
//	而是"角色迭代得多" —— **两件不同的事，而后者完全正常**。
func TestMaxStepsUsesGraphStepsNotAgentSteps(t *testing.T) {
	events := []observ.Event{{Layer: observ.LayerGraph, Kind: observ.KindStart}} // 图只跑了 1 跳
	for i := 0; i < 50; i++ {                                                    // 而 agent 迭代了 50 轮
		events = append(events, observ.Event{Layer: observ.LayerAgent, Kind: observ.KindStart})
	}

	c := Case{ID: "x", Topic: "t", Expect: Expectation{MaxSteps: 10}}
	// ⚠️ 前置：证明这个前提**真的成立** —— 否则这条测试是恒真的
	if m := metricsOf(events); m.Steps != 1 || m.AgentSteps != 50 {
		t.Fatalf("前置不成立：Steps=%d AgentSteps=%d", m.Steps, m.AgentSteps)
	}

	vs := Run(context.Background(), CaseSet{Cases: []Case{c}}, Options{Run: staticRunner(outcomeWith(nil, nil, events...), nil)})
	if !vs[0].Passed {
		t.Errorf("`max_steps` 该看**图跳数**（1 ≤ 10 → 过）；"+
			"若它去看 50 轮 agent 迭代，就是把「正常的多轮」误判成「图打转」。实际：%v", vs[0].Failed)
	}
}

// ─── ★★ 判据不能问错对象 ────────────────────────────────────────────

// ⚠️ `MustContain` 查的是 `Rendered`（**用户最终看到的那份**），不是 `Board`（中间产物）。
//
//	问错对象会得到一个**恒真**的判据 —— 因为中间产物里当然有大纲里的词。
//
//	> 一个恒真的判据比没有判据更危险：它**看起来在保护你**。
func TestMustContainLooksAtRenderedNotBoard(t *testing.T) {
	// 黑板里有 "充电网络"（大纲的标题），而**渲染出来的页面里没有**
	o := outcomeWith(map[string][]schema.Artifact{
		string(schema.KindOutline): {outlineArtifact(t, 3)},
	}, []string{"<html>一页什么都没说的占位内容</html>"})

	c := Case{ID: "x", Topic: "t", Expect: Expectation{MustContain: []string{"充电网络"}}}
	vs := Run(context.Background(), CaseSet{Cases: []Case{c}}, Options{Run: staticRunner(o, nil)})

	if vs[0].Passed {
		t.Error("用户在页面上**看不到**这句话，判据必须红 —— " +
			"若它去查中间产物，就会得到一个**恒真**的判据（中间产物里当然有）")
	}
}

// ⚠️ 而 `must_not_contain` 的两侧都要守：它最容易"因为查不到东西而恒绿"。
func TestMustNotContainDetectsLeakedPlaceholder(t *testing.T) {
	c := Case{ID: "x", Topic: "t", Expect: Expectation{MustNotContain: []string{"{{"}}}

	good := outcomeWith(nil, []string{"<html>正常页面</html>"})
	if vs := Run(context.Background(), CaseSet{Cases: []Case{c}}, Options{Run: staticRunner(good, nil)}); !vs[0].Passed {
		t.Errorf("没有占位符时该绿，实际 %v", vs[0].Failed)
	}

	leaked := outcomeWith(nil, []string{"<html>{{title}}</html>"})
	if vs := Run(context.Background(), CaseSet{Cases: []Case{c}}, Options{Run: staticRunner(leaked, nil)}); vs[0].Passed {
		t.Error("页面里漏出了模板占位符 —— 判据必须红（那是最典型的一类回归）")
	}
}

// ─── ★★ `Validate`：拒绝"伪装成保障的空白" ──────────────────────────

// ⚠️ 一个**期望全空**的用例**永远会过** —— 而它是最危险的那一种：
//
//	它绿着，于是没人去看它；而它**什么都没验证**（一个本该失败的改动，它会放过）。
//
//	> ### 一个永远绿的用例，是一种**伪装成保障的空白**。
func TestValidateRejectsEmptyExpectation(t *testing.T) {
	err := (CaseSet{Cases: []Case{{ID: "空壳", Topic: "主题"}}}).Validate()
	if err == nil {
		t.Fatal("一个判据都没有的用例必须被拒 —— 它会永远通过，而没人会去看它")
	}
	for _, want := range []string{"空壳", "永远通过"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应有 %q（说清是哪个用例 + 为什么），实际：%v", want, err)
		}
	}
}

func TestValidateRejectsBadCaseSets(t *testing.T) {
	cases := []struct {
		name string
		set  CaseSet
		want string
	}{
		{"空用例集", CaseSet{}, "什么都没跑"},
		{"没 id", CaseSet{Cases: []Case{{Topic: "t", Expect: Expectation{Pages: 3}}}}, "没有 id"},
		{"id 重复", CaseSet{Cases: []Case{
			{ID: "a", Topic: "t", Expect: Expectation{Pages: 3}},
			{ID: "a", Topic: "t2", Expect: Expectation{Pages: 3}},
		}}, "重复"},
		{"没 topic", CaseSet{Cases: []Case{{ID: "a", Expect: Expectation{Pages: 3}}}}, "没有 topic"},
	}
	for _, c := range cases {
		err := c.set.Validate()
		if err == nil {
			t.Errorf("%s：应当被拒", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误里该说清原因（含 %q），实际：%v", c.name, c.want, err)
		}
	}

	// ⚠️ 而**正例前置**：一份合法的用例集必须**通过** ——
	//	否则上面的"都该被拒"是恒真的（一个永远报错的 Validate 也能满足）
	ok := CaseSet{Cases: []Case{{ID: "a", Topic: "t", Expect: Expectation{Pages: 3}}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法用例集不该被拒: %v", err)
	}
}

// ─── ★ 摘要的分母 ───────────────────────────────────────────────────

// ⚠️ 跑不完的用例**不算进平均** —— 因为它可能是"5 毫秒内就撞上了预算"，
//
//	把它算进去会把均值**拉向一个假的位置**。
//
//	> **一个统计的分母，必须与它想问的问题一致。**
func TestSummarizeExcludesErroredFromAverages(t *testing.T) {
	vs := []Verdict{
		{CaseID: "好", Passed: true, Metrics: Metrics{PromptTokens: 1000, Wall: time.Second}},
		{CaseID: "坏", Failed: []string{"pages：期望 3，实际 2"}, Metrics: Metrics{PromptTokens: 1000, Wall: time.Second}},
		{CaseID: "没跑完", Err: errors.New("预算不够"), Metrics: Metrics{PromptTokens: 999999, Wall: time.Hour}},
	}
	s := Summarize(vs)

	if s.Total != 3 || s.Passed != 1 || s.Failed != 1 || s.Errored != 1 {
		t.Errorf("三态计数错了：%+v", s)
	}
	if s.AvgPrompt != 1000 {
		t.Errorf("平均 prompt 应当是 1000（两个跑完的），实际 %d —— "+
			"把「跑不完」那个 999999 算进来会把均值拉到一个**假的位置**", s.AvgPrompt)
	}
	// ⚠️ 而**成功率的分母是全部用例**（跑不完的也算不成功）——
	//	与上面的平均**用的是不同的分母**，而这是对的：它们问的问题不同。
	if got := s.Rate(); got < 33.3 || got > 33.4 {
		t.Errorf("成功率应当是 1/3≈33.3%%（跑不完的算不成功），实际 %.1f%%", got)
	}
}
