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

// designerInput 造一个"上游大纲已就绪"的输入（固定 3 页）。
func designerInput(t *testing.T, board *state.Blackboard, topic string) SubAgentInput {
	t.Helper()

	a, err := schema.NewArtifact(string(schema.KindOutline), "outline", schema.Outline{
		Pages: []schema.PageOutline{
			{Index: 0, Title: "行业现状", Points: []string{"渗透率已达 35%"}},
			{Index: 1, Title: "核心矛盾", Points: []string{"增速放缓与产能扩张的冲突"}},
			{Index: 2, Title: "结论与建议", Points: []string{"优先布局充电网络"}},
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

// threeDesignsArgs 是一份合法的 3 页设计提交。
const threeDesignsArgs = `{"pages":[
	{"page_index":0,"layout":"cover","note":"整页留白，大标题压住主题"},
	{"page_index":1,"layout":"data","note":"用一个大数字突出增速变化"},
	{"page_index":2,"layout":"closing","note":"给出行动建议并收束"}
]}`

func newDesigner(t *testing.T, client llm.Client) *Designer {
	t.Helper()
	d, err := NewDesigner(DesignerOptions{Client: client})
	if err != nil {
		t.Fatalf("构造版面设计者失败: %v", err)
	}
	return d
}

// ─── 角色身份与依赖 ─────────────────────────────────────────────────

func TestDesignerIdentity(t *testing.T) {
	d := newDesigner(t, llm.NewScriptedClient("x", ""))

	if d.Name() != "designer" {
		t.Errorf("角色名不对: %q", d.Name())
	}
	if d.Kind() != schema.KindDesign {
		t.Errorf("产物种类不对: %q", d.Kind())
	}

	needs := d.Needs()
	if len(needs) != 1 || needs[0] != schema.KindOutline {
		t.Fatalf("必须声明依赖 outline，实际 %v", needs)
	}
	if strings.TrimSpace(d.Description()) == "" {
		t.Error("必须给编排者一句'什么时候该调我'")
	}
}

func TestNewDesignerValidates(t *testing.T) {
	if _, err := NewDesigner(DesignerOptions{}); err == nil {
		t.Fatal("未配置模型客户端时应在构造期报错")
	}
}

// ─── 正常路径 ───────────────────────────────────────────────────────

func TestDesignerRunsAndSubmits(t *testing.T) {
	board := state.NewBlackboard()
	in := designerInput(t, board, "新能源汽车行业趋势")

	client := llm.NewScriptedClient("designer", "完成。",
		callStep("submit_design", threeDesignsArgs), textStep("完成。"))

	d := newDesigner(t, client)
	product, err := d.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	got, ok := product.(schema.Design)
	if !ok {
		t.Fatalf("产物类型不对: %T", product)
	}
	if len(got.Pages) != 3 {
		t.Fatalf("应有 3 页设计，实际 %d", len(got.Pages))
	}
	if got.Pages[0].Layout != schema.LayoutCover || got.Pages[2].Layout != schema.LayoutClosing {
		t.Errorf("版式不对: %+v", got.Pages)
	}
	if got.Pages[1].Note == "" {
		t.Error("note 必须被保留")
	}
	if d.Stats().PromptTokens == 0 {
		t.Error("用量统计应当被记录")
	}
}

// TestDesignerAcceptsPageIndexZero 是本文件最重要的一个用例。
//
// 它守住的是一个**真实的、企业仓库里正在发生的** bug：
//
//	case t.PageIndex != 0:   // ← 把 0 当成"没给"
//
// 0 是合法的页码（第一页）。用零值当哨兵，首页就会被误判为缺失。
// 我们用 `*int` 区分"没给"与"给了 0"，这个用例就是它的看门人。
func TestDesignerAcceptsPageIndexZero(t *testing.T) {
	board := state.NewBlackboard()
	in := designerInput(t, board, "主题")

	client := llm.NewScriptedClient("designer", "完成。",
		callStep("submit_design", threeDesignsArgs), textStep("完成。"))

	product, err := newDesigner(t, client).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("page_index=0 是合法值，不该被当成缺失: %v", err)
	}
	if product.(schema.Design).Pages[0].PageIndex != 0 {
		t.Error("首页的 page_index 必须是 0")
	}
}

// TestDesignerSortsByPageIndex：模型乱序提交，结果必须按页序排好。
//
// 这样下游拿到的 Design.Pages[i].PageIndex 恒等于 i，
// "数组顺序即页序"重新成立，下游不必再自己排序（也不必相信自己是排过的）。
func TestDesignerSortsByPageIndex(t *testing.T) {
	board := state.NewBlackboard()
	in := designerInput(t, board, "主题")

	shuffled := `{"pages":[
		{"page_index":2,"layout":"closing","note":"收束"},
		{"page_index":0,"layout":"cover","note":"开场"},
		{"page_index":1,"layout":"timeline","note":"按时间展开"}
	]}`

	client := llm.NewScriptedClient("designer", "完成。",
		callStep("submit_design", shuffled), textStep("完成。"))

	product, err := newDesigner(t, client).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	want := []schema.Layout{schema.LayoutCover, schema.LayoutTimeline, schema.LayoutClosing}
	for i, p := range product.(schema.Design).Pages {
		if p.PageIndex != i {
			t.Errorf("pages[%d].PageIndex 应为 %d，实际 %d", i, i, p.PageIndex)
		}
		if p.Layout != want[i] {
			t.Errorf("pages[%d].Layout 应为 %q，实际 %q", i, want[i], p.Layout)
		}
	}
}

// ─── 硬约束与回灌自愈 ───────────────────────────────────────────────

func TestDesignerSelfHealsOnInvalidLayout(t *testing.T) {
	board := state.NewBlackboard()
	in := designerInput(t, board, "主题")

	// 模型第一轮自创了一个版式名（自由文本的本能）
	selfMade := `{"pages":[
		{"page_index":0,"layout":"cover","note":"开场"},
		{"page_index":1,"layout":"two-column","note":"左右分栏"},
		{"page_index":2,"layout":"closing","note":"收束"}
	]}`

	client := llm.NewScriptedClient("designer", "完成。",
		callStep("submit_design", selfMade),         // 被拒 → 回灌
		callStep("submit_design", threeDesignsArgs), // 改对 → 通过
		textStep("完成。"),
	)

	product, err := newDesigner(t, client).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("回灌自愈后应当成功: %v", err)
	}
	if product.(schema.Design).Pages[1].Layout != schema.LayoutData {
		t.Error("第二轮应使用合法的枚举值")
	}
}

func TestDesignerRejectsBadSubmissions(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{
			"份数少于页数",
			`{"pages":[{"page_index":0,"layout":"cover","note":"a"}]}`,
			"各给一份设计",
		},
		{
			"缺 page_index（不能靠数组顺序猜）",
			`{"pages":[
				{"layout":"cover","note":"a"},
				{"page_index":1,"layout":"bullets","note":"b"},
				{"page_index":2,"layout":"closing","note":"c"}]}`,
			"page_index 缺失",
		},
		{
			"page_index 越界",
			`{"pages":[
				{"page_index":0,"layout":"cover","note":"a"},
				{"page_index":1,"layout":"bullets","note":"b"},
				{"page_index":9,"layout":"closing","note":"c"}]}`,
			"越界",
		},
		{
			"page_index 重复（会顶掉另一页）",
			`{"pages":[
				{"page_index":0,"layout":"cover","note":"a"},
				{"page_index":0,"layout":"bullets","note":"b"},
				{"page_index":2,"layout":"closing","note":"c"}]}`,
			"出现了不止一次",
		},
		{
			"缺了第 2 页",
			`{"pages":[
				{"page_index":0,"layout":"cover","note":"a"},
				{"page_index":0,"layout":"bullets","note":"b"},
				{"page_index":2,"layout":"closing","note":"c"}]}`,
			"出现了不止一次",
		},
		{
			"自创版式名",
			`{"pages":[
				{"page_index":0,"layout":"cover","note":"a"},
				{"page_index":1,"layout":"grid-2col","note":"b"},
				{"page_index":2,"layout":"closing","note":"c"}]}`,
			"不是合法版式",
		},
		{
			"首页不是封面（产品规则）",
			`{"pages":[
				{"page_index":0,"layout":"bullets","note":"a"},
				{"page_index":1,"layout":"data","note":"b"},
				{"page_index":2,"layout":"closing","note":"c"}]}`,
			"必须是 \"cover\"",
		},
		{
			"末页不是收尾（产品规则）",
			`{"pages":[
				{"page_index":0,"layout":"cover","note":"a"},
				{"page_index":1,"layout":"data","note":"b"},
				{"page_index":2,"layout":"bullets","note":"c"}]}`,
			"必须是 \"closing\"",
		},
		{
			"note 为空",
			`{"pages":[
				{"page_index":0,"layout":"cover","note":"  "},
				{"page_index":1,"layout":"data","note":"b"},
				{"page_index":2,"layout":"closing","note":"c"}]}`,
			"note 不能为空",
		},
		{
			"note 超长",
			`{"pages":[
				{"page_index":0,"layout":"cover","note":"` + strings.Repeat("长", schema.MaxNoteRunes+1) + `"},
				{"page_index":1,"layout":"data","note":"b"},
				{"page_index":2,"layout":"closing","note":"c"}]}`,
			"超过上限",
		},
		{
			"非法 JSON",
			`{"pages":`,
			"不是合法 JSON",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodeDesign(c.args, 3)
			if err == nil {
				t.Fatal("非法提交必须被拒绝")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应包含 %q，实际 %q", c.want, err.Error())
			}
		})
	}
}

// TestDesignerErrorListsValidLayouts 守住"拒绝必须可自愈"。
func TestDesignerErrorListsValidLayouts(t *testing.T) {
	bad := `{"pages":[
		{"page_index":0,"layout":"cover","note":"a"},
		{"page_index":1,"layout":"自创版式","note":"b"},
		{"page_index":2,"layout":"closing","note":"c"}]}`

	_, err := decodeDesign(bad, 3)
	if err == nil {
		t.Fatal("应被拒绝")
	}
	// 不给合法清单，模型只能瞎猜（这是 L1-5 的教训）
	for _, name := range schema.LayoutNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("错误信息必须列出合法版式 %q：%v", name, err)
		}
	}
}

// ─── 底线 ───────────────────────────────────────────────────────────

func TestDesignerErrorsWhenNothingSubmitted(t *testing.T) {
	board := state.NewBlackboard()
	in := designerInput(t, board, "主题")

	_, err := newDesigner(t, llm.NewScriptedClient("designer", "设计好了。")).Run(context.Background(), in)
	if err == nil {
		t.Fatal("模型没调用提交口时必须报错 —— 不能当成'空白设计'")
	}
	if !strings.Contains(err.Error(), "没有调用") {
		t.Errorf("错误信息应说明原因，实际 %q", err.Error())
	}
}

func TestDesignerErrorsWhenUpstreamMissingFromInput(t *testing.T) {
	board := state.NewBlackboard()
	in := SubAgentInput{Topic: "主题", Upstream: board}

	_, err := newDesigner(t, llm.NewScriptedClient("designer", "完成。")).Run(context.Background(), in)
	if err == nil {
		t.Fatal("输入里没有大纲时应直接报错")
	}
	if !strings.Contains(err.Error(), "outline") {
		t.Errorf("错误信息应指出缺的是 outline，实际 %q", err.Error())
	}
}

func TestDesignerErrorsWhenOutlineIsEmpty(t *testing.T) {
	board := state.NewBlackboard()

	a, err := schema.NewArtifact(string(schema.KindOutline), "outline", schema.Outline{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	stored, _ := board.Put(a)

	in := SubAgentInput{
		Topic:    "主题",
		Inputs:   []schema.Handoff{schema.HandoffOf(stored)},
		Upstream: board,
	}

	_, err = newDesigner(t, llm.NewScriptedClient("designer", "完成。")).Run(context.Background(), in)
	if err == nil {
		t.Fatal("空大纲无法作为设计基准，必须报错")
	}
	if !strings.Contains(err.Error(), "空") {
		t.Errorf("错误信息应说明大纲是空的，实际 %q", err.Error())
	}
}

func TestDesignerPropagatesModelError(t *testing.T) {
	board := state.NewBlackboard()
	in := designerInput(t, board, "主题")

	if _, err := newDesigner(t, &failingClient{}).Run(context.Background(), in); err == nil {
		t.Fatal("模型调用失败必须向上传播")
	}
}

// ─── Prompt ─────────────────────────────────────────────────────────

func TestDesignerPromptCarriesOutlineAndCatalog(t *testing.T) {
	board := state.NewBlackboard()
	in := designerInput(t, board, "新能源汽车行业趋势")

	outline, ref, err := loadOutline(in)
	if err != nil {
		t.Fatalf("取上游失败: %v", err)
	}
	prompt, err := buildDesignPrompt(in, outline, ref, 4)
	if err != nil {
		t.Fatalf("构造 Prompt 失败: %v", err)
	}

	// ① 大纲正文（含页码）必须真的被渲染进去
	for _, want := range []string{"第 1 页「行业现状」", "第 3 页「结论与建议」", "渗透率已达 35%"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("Prompt 缺少 %q：\n%s", want, prompt)
		}
	}
	// ② 依据版本可回溯
	if !strings.Contains(prompt, "outline@1") {
		t.Error("必须写明依据的是哪一版大纲")
	}
	// ③ **选项表由 schema 渲染** —— 模型看到的与代码校验的是同一份数据
	for _, spec := range schema.LayoutCatalog() {
		if !strings.Contains(prompt, string(spec.Layout)) {
			t.Errorf("版式目录里的 %q 没出现在 Prompt 里", spec.Layout)
		}
		if !strings.Contains(prompt, spec.BestFor) {
			t.Errorf("BestFor（%q）必须出现 —— 它是'按内容匹配'的依据", spec.BestFor)
		}
	}
	// ④ 结构页规则要**事前告知**，不能只在代码里事后拒绝
	if !strings.Contains(prompt, string(schema.LayoutCover)) || !strings.Contains(prompt, "系统会校验") {
		t.Error("结构页规则必须事前写进 Prompt（事前说清 + 事后校验）")
	}
}

func TestDesignerSpecPinsPageCount(t *testing.T) {
	submit, err := NewSubmitTool(designerSpec(5))
	if err != nil {
		t.Fatalf("构造提交出口失败: %v", err)
	}

	def := submit.Definition()
	if !strings.Contains(def.Description, "5 页") {
		t.Errorf("提交口描述必须写明页数，实际 %q", def.Description)
	}
	// page_index 的取值上界也必须跟着页数走
	if !strings.Contains(def.Parameters[0].Items.Properties[0].Description, "0..4") {
		t.Errorf("page_index 的取值范围应与页数一致，实际 %q",
			def.Parameters[0].Items.Properties[0].Description)
	}
	// 版式参数必须把合法清单写进描述（描述即 Prompt）
	layoutParam := def.Parameters[0].Items.Properties[1]
	for _, name := range schema.LayoutNames() {
		if !strings.Contains(layoutParam.Description, name) {
			t.Errorf("layout 参数描述里必须列出 %q", name)
		}
	}
}
