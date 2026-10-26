package agents

import (
	"context"
	"strings"
	"testing"

	"multiagent/internal/llm"
	"multiagent/internal/schema"
	"multiagent/internal/state"
)

// ─── 测试夹具 ───────────────────────────────────────────────────────

// outlineInput 造一个"上游已就绪"的输入。
func outlineInput(t *testing.T, board *state.Blackboard, topic string) SubAgentInput {
	t.Helper()

	a, err := schema.NewArtifact(string(schema.KindResearch), "research", schema.Research{
		Topic:   topic,
		Queries: []string{"新能源汽车 渗透率"},
		Findings: []schema.Finding{
			{Claim: "渗透率已达 35%", Source: "web_search#1"},
			{Claim: "电池成本两年下降 40%", Source: "web_search#2"},
		},
	})
	if err != nil {
		t.Fatalf("构造上游产物失败: %v", err)
	}
	stored, err := board.Put(a)
	if err != nil {
		t.Fatalf("上游入库失败: %v", err)
	}

	return SubAgentInput{
		Topic:    topic,
		Inputs:   []schema.Handoff{schema.HandoffOf(stored)},
		Upstream: board,
	}
}

// threePagesArgs 是一份合法的 3 页大纲提交参数。
//
// 注意内容点的条数：首页/末页是**结构页**（1 条即可），
// 中间那页是**正文页**（必须 3-5 条）。
// 这条区分由 `schema.IsStructuralPage` 给出，outline 与 designer 共用同一个判断。
const threePagesArgs = `{"pages":[
	{"title":"行业现状","points":["渗透率已达 35%"]},
	{"title":"核心矛盾","points":["增速放缓","产能仍在扩张","价格战加剧"]},
	{"title":"结论与建议","points":["优先布局充电网络"]}
]}`

func newOutline(t *testing.T, client llm.Client) *OutlineAgent {
	t.Helper()
	o, err := NewOutlineAgent(OutlineOptions{Client: client})
	if err != nil {
		t.Fatalf("构造大纲编写者失败: %v", err)
	}
	return o
}

// ─── 角色身份与依赖 ─────────────────────────────────────────────────

func TestOutlineAgentIdentity(t *testing.T) {
	o := newOutline(t, llm.NewScriptedClient("x", ""))

	if o.Name() != "outline" {
		t.Errorf("角色名不对: %q", o.Name())
	}
	if o.Kind() != schema.KindOutline {
		t.Errorf("产物种类不对: %q", o.Kind())
	}

	// ★ 第一个"有上游依赖"的角色
	needs := o.Needs()
	if len(needs) != 1 || needs[0] != schema.KindResearch {
		t.Fatalf("必须声明依赖 research，实际 %v", needs)
	}

	if strings.TrimSpace(o.Description()) == "" {
		t.Error("必须给编排者一句'什么时候该调我'")
	}
	// 依赖由 Needs 表达，就**不要**在 Description 里再写一遍（两处会漂移）
	if strings.Contains(o.Description(), "前置") || strings.Contains(o.Description(), "先有") {
		t.Errorf("依赖不应在描述里重复表达：%q", o.Description())
	}
}

func TestNewOutlineAgentValidates(t *testing.T) {
	if _, err := NewOutlineAgent(OutlineOptions{}); err == nil {
		t.Fatal("未配置模型客户端时应在构造期报错")
	}
}

// ─── 正常路径 ───────────────────────────────────────────────────────

func TestOutlineRunsAndSubmits(t *testing.T) {
	board := state.NewBlackboard()
	in := outlineInput(t, board, "新能源汽车行业趋势")

	client := llm.NewScriptedClient("outline", "完成。",
		callStep("submit_outline", threePagesArgs),
		textStep("完成。"),
	)

	o := newOutline(t, client)
	product, err := o.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	got, ok := product.(schema.Outline)
	if !ok {
		t.Fatalf("产物类型不对: %T", product)
	}
	if len(got.Pages) != TargetOutlinePages {
		t.Fatalf("应有 %d 页，实际 %d", TargetOutlinePages, len(got.Pages))
	}

	// 页码由代码按数组位置赋值
	for i, p := range got.Pages {
		if p.Index != i {
			t.Errorf("pages[%d].Index 应为 %d，实际 %d", i, i, p.Index)
		}
	}
	if got.Pages[0].Title != "行业现状" {
		t.Errorf("标题不对: %q", got.Pages[0].Title)
	}
	if len(got.Pages[2].Points) != 1 {
		t.Errorf("第三页应有 1 条内容点，实际 %v", got.Pages[2].Points)
	}
	if o.Stats().PromptTokens == 0 {
		t.Error("用量统计应当被记录")
	}
}

// TestOutlineIgnoresModelProvidedIndex 守住"能由代码确定的，别让模型复述"。
//
// 即使模型硬塞了一个 index（0-based / 1-based 混用是常见错误），
// 也会被代码按数组位置覆盖 —— 索引错位这一整类 bug 从根上消失。
func TestOutlineIgnoresModelProvidedIndex(t *testing.T) {
	board := state.NewBlackboard()
	in := outlineInput(t, board, "主题")

	args := `{"pages":[
		{"index":99,"title":"甲","points":["p"]},
		{"index":42,"title":"乙","points":["p","q","r"]},
		{"index":-1,"title":"丙","points":["p"]}
	]}`

	client := llm.NewScriptedClient("outline", "完成。",
		callStep("submit_outline", args), textStep("完成。"))

	product, err := newOutline(t, client).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	for i, p := range product.(schema.Outline).Pages {
		if p.Index != i {
			t.Errorf("模型给的 index 必须被忽略：pages[%d].Index = %d", i, p.Index)
		}
	}
}

// ─── 硬约束：页数 / 标题 / 内容点 ────────────────────────────────────

func TestOutlineSelfHealsOnWrongPageCount(t *testing.T) {
	board := state.NewBlackboard()
	in := outlineInput(t, board, "主题")

	twoPages := `{"pages":[{"title":"甲","points":["p"]},{"title":"乙","points":["p"]}]}`

	client := llm.NewScriptedClient("outline", "完成。",
		callStep("submit_outline", twoPages),       // 第 1 次：2 页 → 被拒绝
		callStep("submit_outline", threePagesArgs), // 第 2 次：补足 → 通过（回灌自愈）
		textStep("完成。"),
	)

	product, err := newOutline(t, client).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("回灌自愈后应当成功: %v", err)
	}
	if n := len(product.(schema.Outline).Pages); n != TargetOutlinePages {
		t.Fatalf("最终应有 %d 页，实际 %d", TargetOutlinePages, n)
	}
}

func TestOutlineRejectsBadPages(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{
			"标题为空",
			`{"pages":[{"title":"  ","points":["p"]},{"title":"乙","points":["p"]},{"title":"丙","points":["p"]}]}`,
			"title 不能为空",
		},
		{
			"标题超长（复用 slide 层常量）",
			`{"pages":[{"title":"` + strings.Repeat("超", schema.MaxTitleRunes+1) + `","points":["p"]},{"title":"乙","points":["p"]},{"title":"丙","points":["p"]}]}`,
			"超过上限",
		},
		{
			"首页（结构页）没有内容点",
			`{"pages":[{"title":"甲","points":[]},{"title":"乙","points":["p","q","r"]},{"title":"丙","points":["p"]}]}`,
			"应为 1-5",
		},
		{
			"正文页内容点太少（下限是 3，不是 1）",
			`{"pages":[{"title":"甲","points":["p"]},{"title":"乙","points":["p"]},{"title":"丙","points":["p"]}]}`,
			"应为 3-5",
		},
		{
			"正文页内容点太多",
			`{"pages":[{"title":"甲","points":["p"]},{"title":"乙","points":["a","b","c","d","e","f"]},{"title":"丙","points":["p"]}]}`,
			"应为 3-5",
		},
		{
			"内容点为空串",
			`{"pages":[{"title":"甲","points":["  "]},{"title":"乙","points":["p"]},{"title":"丙","points":["p"]}]}`,
			"不能为空",
		},
		{
			"页数不对",
			`{"pages":[{"title":"甲","points":["p"]}]}`,
			"必须正好",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodeOutline(c.args)
			if err == nil {
				t.Fatal("非法提交必须被拒绝")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应包含 %q，实际 %q", c.want, err.Error())
			}
		})
	}
}

// ─── 底线 ───────────────────────────────────────────────────────────

func TestOutlineErrorsWhenNothingSubmitted(t *testing.T) {
	board := state.NewBlackboard()
	in := outlineInput(t, board, "主题")

	client := llm.NewScriptedClient("outline", "我觉得这样挺好。")

	_, err := newOutline(t, client).Run(context.Background(), in)
	if err == nil {
		t.Fatal("模型没调用提交口时必须报错 —— 不能当成'没有大纲'")
	}
	if !strings.Contains(err.Error(), "没有调用") {
		t.Errorf("错误信息应说明原因，实际 %q", err.Error())
	}
}

func TestOutlineErrorsWhenUpstreamMissingFromInput(t *testing.T) {
	// 依赖已经被适配器拦过一轮；这里测的是"绕开适配器直接调用"时的兜底。
	board := state.NewBlackboard()
	in := SubAgentInput{Topic: "主题", Upstream: board}

	client := llm.NewScriptedClient("outline", "完成。", callStep("submit_outline", threePagesArgs))
	_, err := newOutline(t, client).Run(context.Background(), in)
	if err == nil {
		t.Fatal("输入里没有上游产物时应直接报错，而不是让模型对着空气编")
	}
	if !strings.Contains(err.Error(), "research") {
		t.Errorf("错误信息应指出缺的是 research，实际 %q", err.Error())
	}
}

func TestOutlinePropagatesModelError(t *testing.T) {
	board := state.NewBlackboard()
	in := outlineInput(t, board, "主题")

	if _, err := newOutline(t, &failingClient{}).Run(context.Background(), in); err == nil {
		t.Fatal("模型调用失败必须向上传播")
	}
}

// ─── Prompt：上游内容必须带来源与版本 ───────────────────────────────

func TestOutlinePromptCarriesFindingsWithSource(t *testing.T) {
	board := state.NewBlackboard()
	in := outlineInput(t, board, "新能源汽车行业趋势")

	prompt, err := buildOutlinePrompt(in, 4)
	if err != nil {
		t.Fatalf("构造 Prompt 失败: %v", err)
	}

	if !strings.Contains(prompt, "渗透率已达 35%") {
		t.Errorf("上游结论必须真的被渲染进 Prompt：\n%s", prompt)
	}
	if !strings.Contains(prompt, "web_search#1") {
		t.Error("每条结论都要带来源，让下游看得见断言出处（L1-5 的延续）")
	}
	if !strings.Contains(prompt, "research@1") {
		t.Error("必须写明依据的是哪一版素材（引用可回溯）")
	}
	if !strings.Contains(prompt, "3 页") {
		t.Errorf("页数目标必须写进去：\n%s", prompt)
	}
}

func TestOutlinePromptReportsEmptyResearch(t *testing.T) {
	board := state.NewBlackboard()

	a, err := schema.NewArtifact(string(schema.KindResearch), "research",
		schema.Research{Topic: "主题"}) // 一条发现都没有
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	stored, _ := board.Put(a)

	in := SubAgentInput{
		Topic:    "主题",
		Inputs:   []schema.Handoff{schema.HandoffOf(stored)},
		Upstream: board,
	}

	prompt, err := buildOutlinePrompt(in, 4)
	if err != nil {
		t.Fatalf("空素材是合法输入，不应报错: %v", err)
	}
	if !strings.Contains(prompt, "不要编造") {
		t.Errorf("空素材必须如实转达，否则模型会开始编：\n%s", prompt)
	}
}

func TestOutlinePromptCarriesHint(t *testing.T) {
	board := state.NewBlackboard()
	in := outlineInput(t, board, "主题")
	in.Hint = "偏重政策影响"

	prompt, _ := buildOutlinePrompt(in, 4)
	if !strings.Contains(prompt, "偏重政策影响") {
		t.Errorf("编排者的额外要求必须透传：\n%s", prompt)
	}
}

// ─── 提交契约 ───────────────────────────────────────────────────────

func TestOutlineSpecDeclaresNoIndexParam(t *testing.T) {
	spec := outlineSpec()
	submit, err := NewSubmitTool(spec)
	if err != nil {
		t.Fatalf("构造提交出口失败: %v", err)
	}

	params := submit.Definition().Parameters
	if len(params) != 1 || params[0].Name != "pages" {
		t.Fatalf("只应有一个 pages 参数，实际 %+v", params)
	}

	// 页码不由模型填写 —— 参数里不该出现 index
	for _, p := range params[0].Items.Properties {
		if p.Name == "index" {
			t.Error("index 必须由代码赋值，不该让模型填（它只可能写错）")
		}
	}
}
