package agents

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"multiagent/internal/observ"
	"multiagent/internal/schema"
	"multiagent/internal/state"
	"multiagent/internal/tools"
)

// ─── 测试夹具 ───────────────────────────────────────────────────────

func sampleOutlinePages() []schema.PageOutline {
	return []schema.PageOutline{
		{Index: 0, Title: "行业现状", Points: []string{"渗透率已达 35%"}},
		{Index: 1, Title: "核心矛盾", Points: []string{"增速放缓", "产能仍在扩张", "价格战加剧"}},
		{Index: 2, Title: "结论与建议", Points: []string{"优先布局充电网络"}},
	}
}

func sampleDesignPages() []schema.PageDesign {
	return []schema.PageDesign{
		{PageIndex: 0, Layout: schema.LayoutCover, Note: "开场"},
		{PageIndex: 1, Layout: schema.LayoutBullets, Note: "列三条矛盾"},
		{PageIndex: 2, Layout: schema.LayoutClosing, Note: "收束"},
	}
}

// renderInput 造一个"大纲 + 设计都就绪"的输入。
func renderInput(t *testing.T, outPages []schema.PageOutline, desPages []schema.PageDesign) SubAgentInput {
	t.Helper()
	board := state.NewBlackboard()

	oa, err := schema.NewArtifact(string(schema.KindOutline), "outline",
		schema.Outline{Pages: outPages})
	if err != nil {
		t.Fatalf("构造大纲失败: %v", err)
	}
	storedO, err := board.Put(oa)
	if err != nil {
		t.Fatalf("大纲入库失败: %v", err)
	}

	da, err := schema.NewArtifact(string(schema.KindDesign), "designer",
		schema.Design{Pages: desPages})
	if err != nil {
		t.Fatalf("构造设计失败: %v", err)
	}
	storedD, err := board.Put(da)
	if err != nil {
		t.Fatalf("设计入库失败: %v", err)
	}

	return SubAgentInput{
		Topic:    "新能源汽车行业趋势",
		Inputs:   []schema.Handoff{schema.HandoffOf(storedO), schema.HandoffOf(storedD)},
		Upstream: board,
	}
}

// fakeRenderer 是测试用的渲染端口（原则五：端口可替换）。
type fakeRenderer struct {
	mu      sync.Mutex
	calls   []string
	live    int
	maxLive int
	delay   time.Duration
	failOn  map[string]string // 标题 → 失败原因
}

func (f *fakeRenderer) Render(s schema.Slide) (string, int, error) {
	f.mu.Lock()
	f.live++
	if f.live > f.maxLive {
		f.maxLive = f.live
	}
	f.mu.Unlock()

	time.Sleep(f.delay)

	f.mu.Lock()
	f.live--
	f.calls = append(f.calls, s.Title)
	reason, shouldFail := f.failOn[s.Title]
	f.mu.Unlock()

	if shouldFail {
		return "", 0, fmt.Errorf("%s", reason)
	}
	return "out/slide-" + s.Title + ".html", 100, nil
}

func (f *fakeRenderer) FilePathFor(title string) string { return "out/slide-" + title + ".html" }

func (f *fakeRenderer) snapshot() (calls []string, maxLive int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...), f.maxLive
}

// ─── 角色身份 ───────────────────────────────────────────────────────

func TestRendererIdentity(t *testing.T) {
	r := NewRenderer(RendererOptions{Renderer: &fakeRenderer{}})

	if r.Name() != "renderer" {
		t.Errorf("角色名不对: %q", r.Name())
	}
	if r.Kind() != schema.KindRender {
		t.Errorf("产物种类不对: %q", r.Kind())
	}

	// ★ 第一个需要两个上游的角色
	needs := r.Needs()
	if len(needs) != 2 || needs[0] != schema.KindOutline || needs[1] != schema.KindDesign {
		t.Fatalf("必须声明依赖 outline + design，实际 %v", needs)
	}
	if strings.TrimSpace(r.Description()) == "" {
		t.Error("必须给编排者一句'什么时候该调我'")
	}
}

// TestRendererStatsAreZero 把"这一跳不花 token"变成可观测的事实。
func TestRendererStatsAreZero(t *testing.T) {
	r := NewRenderer(RendererOptions{Renderer: &fakeRenderer{}})
	s := r.Stats()

	if s.PromptTokens != 0 || s.CompletionTokens != 0 || s.Steps != 0 || s.ToolCalls != 0 {
		t.Errorf("renderer 不做模型调用，用量必须恒为 0，实际 %+v", s)
	}
}

// ─── 正常路径（用真实渲染器，真的写盘） ─────────────────────────────

func TestRendererRendersAllPagesWithRealTool(t *testing.T) {
	dir := t.TempDir()
	in := renderInput(t, sampleOutlinePages(), sampleDesignPages())

	r := NewRenderer(RendererOptions{OutDir: dir})
	product, err := r.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	got, ok := product.(schema.RenderResult)
	if !ok {
		t.Fatalf("产物类型不对: %T", product)
	}
	if len(got.Files) != 3 {
		t.Fatalf("应产出 3 个文件，实际 %d（失败 %+v）", len(got.Files), got.Failed)
	}
	if len(got.Failed) != 0 {
		t.Errorf("不应有失败页: %+v", got.Failed)
	}

	// 文件真的存在，且内容**逐字**来自大纲
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取输出目录失败: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("磁盘上应有 3 个文件，实际 %d", len(files))
	}

	raw, err := os.ReadFile(got.Files[1])
	if err != nil {
		t.Fatalf("读取产物失败: %v", err)
	}
	page := string(raw)
	for _, want := range []string{"核心矛盾", "增速放缓", "产能仍在扩张", "价格战加剧"} {
		if !strings.Contains(page, want) {
			t.Errorf("页面里必须原样出现 %q —— renderer 不做任何改写", want)
		}
	}
}

// TestRendererIsIdempotent：同样的输入两次运行，得到同样的文件集合。
//
// 这是"不调模型"直接换来的性质：**没有采样方差**。
// 一个调了模型的渲染器，两次运行必然产出不同的 HTML。
func TestRendererIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	in1 := renderInput(t, sampleOutlinePages(), sampleDesignPages())
	in2 := renderInput(t, sampleOutlinePages(), sampleDesignPages())

	r := NewRenderer(RendererOptions{OutDir: dir})

	p1, err := r.Run(context.Background(), in1)
	if err != nil {
		t.Fatalf("第一次执行失败: %v", err)
	}
	p2, err := r.Run(context.Background(), in2)
	if err != nil {
		t.Fatalf("第二次执行失败: %v", err)
	}

	f1 := p1.(schema.RenderResult).Files
	f2 := p2.(schema.RenderResult).Files
	if strings.Join(f1, "|") != strings.Join(f2, "|") {
		t.Errorf("两次运行结果必须一致：\n%v\n%v", f1, f2)
	}

	// 顺序也必须**按页序**稳定 —— 并发完成的顺序是随机的，
	// 但我们把结果按下标写回，所以输出顺序永远是页序。
	wantNames := []string{"slide-行业现状.html", "slide-核心矛盾.html", "slide-结论与建议.html"}
	for i, name := range wantNames {
		if !strings.HasSuffix(f1[i], name) {
			t.Errorf("第 %d 个文件应是 %s，实际 %s", i+1, name, f1[i])
		}
	}
}

// ─── 跨产物一致性 ───────────────────────────────────────────────────

func TestRendererRejectsPageCountMismatch(t *testing.T) {
	// 3 页大纲，2 页设计
	in := renderInput(t, sampleOutlinePages(), sampleDesignPages()[:2])

	_, err := NewRenderer(RendererOptions{Renderer: &fakeRenderer{}}).Run(context.Background(), in)
	if err == nil {
		t.Fatal("页数不一致必须被拒绝 —— 少一页设计就有一页渲染不出来")
	}
	if !strings.Contains(err.Error(), "页数不一致") {
		t.Errorf("错误信息应说明页数问题，实际 %q", err.Error())
	}
}

func TestRendererRejectsUnorderedDesign(t *testing.T) {
	bad := sampleDesignPages()
	bad[1].PageIndex = 2 // 打乱：第 2 项谎称自己是第 3 页
	bad[2].PageIndex = 1

	in := renderInput(t, sampleOutlinePages(), bad)

	_, err := NewRenderer(RendererOptions{Renderer: &fakeRenderer{}}).Run(context.Background(), in)
	if err == nil {
		t.Fatal("设计未按页序排列时必须拒绝")
	}
	if !strings.Contains(err.Error(), "未按页序") {
		t.Errorf("错误信息应说明页序问题，实际 %q", err.Error())
	}
}

// TestRendererPreflightCatchesDuplicateTitles 守住"副作用前预检"。
//
// 两页标题相同 → 会写进同一个文件 → 后写的那次把先写的顶掉，**且无法回滚**。
// 所以必须在**动笔之前**发现，并且**一个文件都不许写**。
func TestRendererPreflightCatchesDuplicateTitles(t *testing.T) {
	dir := t.TempDir()

	pages := sampleOutlinePages()
	pages[2].Title = pages[0].Title // 制造重名

	in := renderInput(t, pages, sampleDesignPages())

	_, err := NewRenderer(RendererOptions{OutDir: dir}).Run(context.Background(), in)
	if err == nil {
		t.Fatal("标题重复会导致互相覆盖，必须被预检挡住")
	}
	if !strings.Contains(err.Error(), "标题相同") {
		t.Errorf("错误信息应说明是标题冲突，实际 %q", err.Error())
	}

	// ★ 关键断言：预检失败时**磁盘上一个文件都不该有**
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取输出目录失败: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("预检必须在写盘之前完成，实际已写入 %d 个文件", len(files))
	}
}

// ─── 失败隔离与"部分成功" ───────────────────────────────────────────

func TestRendererCollectsPartialFailure(t *testing.T) {
	fake := &fakeRenderer{failOn: map[string]string{"核心矛盾": "写盘失败: 磁盘已满"}}
	in := renderInput(t, sampleOutlinePages(), sampleDesignPages())

	product, err := NewRenderer(RendererOptions{Renderer: fake}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("部分成功是合法产物，不该整体报错: %v", err)
	}

	got := product.(schema.RenderResult)
	if len(got.Files) != 2 {
		t.Errorf("应有 2 个成功产物，实际 %d", len(got.Files))
	}
	if len(got.Failed) != 1 {
		t.Fatalf("应有 1 页失败，实际 %+v", got.Failed)
	}
	if got.Failed[0].PageIndex != 1 {
		t.Errorf("失败页应是第 2 页（index=1），实际 %d", got.Failed[0].PageIndex)
	}
	if !strings.Contains(got.Failed[0].Reason, "磁盘已满") {
		t.Errorf("失败原因必须被保留，实际 %q", got.Failed[0].Reason)
	}
	// 摘要里必须体现失败，否则下一跳会以为全都成功了
	if !strings.Contains(got.Summarize(), "未产出") {
		t.Errorf("摘要必须体现失败页数，实际 %q", got.Summarize())
	}
}

func TestRendererErrorsWhenAllPagesFail(t *testing.T) {
	fake := &fakeRenderer{failOn: map[string]string{
		"行业现状":  "写盘失败",
		"核心矛盾":  "写盘失败",
		"结论与建议": "写盘失败",
	}}
	in := renderInput(t, sampleOutlinePages(), sampleDesignPages())

	_, err := NewRenderer(RendererOptions{Renderer: fake}).Run(context.Background(), in)
	if err == nil {
		t.Fatal("全部失败才算失败 —— 必须报错")
	}
	if !strings.Contains(err.Error(), "全部渲染失败") {
		t.Errorf("错误信息应说明全部失败，实际 %q", err.Error())
	}
	// 失败原因要能被看见，否则排查只能靠猜
	if !strings.Contains(err.Error(), "第 1 页") {
		t.Errorf("错误信息应带上具体页与原因，实际 %q", err.Error())
	}
}

// ─── 并发 ───────────────────────────────────────────────────────────

func TestRendererRunsInParallel(t *testing.T) {
	fake := &fakeRenderer{delay: 30 * time.Millisecond}
	in := renderInput(t, sampleOutlinePages(), sampleDesignPages())

	r := NewRenderer(RendererOptions{Renderer: fake, Parallel: 4})
	if _, err := r.Run(context.Background(), in); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	calls, maxLive := fake.snapshot()
	if len(calls) != 3 {
		t.Fatalf("应渲染 3 页，实际 %d", len(calls))
	}
	if maxLive < 2 {
		t.Errorf("3 页互不依赖，应当并发执行（观测到的最大并发=%d）", maxLive)
	}
}

func TestRendererRespectsParallelLimit(t *testing.T) {
	fake := &fakeRenderer{delay: 20 * time.Millisecond}
	in := renderInput(t, sampleOutlinePages(), sampleDesignPages())

	r := NewRenderer(RendererOptions{Renderer: fake, Parallel: 1})
	if _, err := r.Run(context.Background(), in); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if _, maxLive := fake.snapshot(); maxLive != 1 {
		t.Errorf("Parallel=1 时最大并发必须是 1，实际 %d", maxLive)
	}
}

// ─── 事件（可观测性不该因为"没调模型"而缺席） ───────────────────────

func TestRendererEmitsEvents(t *testing.T) {
	var (
		mu     sync.Mutex
		events []observ.Event
	)
	fake := &fakeRenderer{failOn: map[string]string{"核心矛盾": "写盘失败"}}
	in := renderInput(t, sampleOutlinePages(), sampleDesignPages())

	r := NewRenderer(RendererOptions{
		Renderer: fake,
		Tracer: observ.TracerFunc(func(e observ.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, e)
		}),
	})
	if _, err := r.Run(context.Background(), in); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var calls, results int
	spans := map[string]bool{}
	for _, e := range events {
		if e.Phase != "renderer" {
			t.Errorf("事件必须带上角色名，实际 %q", e.Phase)
		}
		if e.ToolName != renderToolName {
			t.Errorf("事件必须带上工具名，实际 %q", e.ToolName)
		}
		// ⚠️ `Span` 是 **R46 新增的守点** —— 它守的是"**逐页可区分**"这个能力。
		//
		//	旧的 `agent.Event.Step` 里装的其实是**页号**（`i`），而不是"第几步"——
		//	那是一个**借用了错误名字**的字段。而 `observ.Event.Span` 有了正确的名字
		//	（它的注释原文就是 `"render_p3"`、`"如逐页渲染"`）。
		//
		//	不守它的话，"三页的事件混在一起分不出哪页是哪页"**不会有任何东西报错** ——
		//	而那是并发渲染时最要紧的一维。
		if e.Span == "" {
			t.Errorf("逐页渲染的事件必须带 `Span`（分不出是哪一页）：%+v", e)
		}
		spans[e.Span] = true

		switch e.Kind {
		case observ.KindToolCall:
			calls++
		case observ.KindToolResult:
			results++
		default:
			t.Errorf("未知事件类型 %q", e.Kind)
		}
	}
	if calls != 3 || results != 3 {
		t.Errorf("3 页应各产生一对事件，实际 call=%d result=%d", calls, results)
	}
	if len(spans) != 3 {
		t.Errorf("3 页应当是 3 个**不同**的 span，实际 %d 个：%v", len(spans), spans)
	}
	// 失败也必须被观测到
	foundFailure := false
	for _, e := range events {
		if e.Kind == observ.KindToolResult && strings.Contains(e.Detail, "失败") {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Error("失败的渲染也必须产生可观测事件")
	}
}

// ─── 上游缺失 ───────────────────────────────────────────────────────

func TestRendererErrorsWhenUpstreamMissing(t *testing.T) {
	t.Run("缺 outline", func(t *testing.T) {
		board := state.NewBlackboard()
		da, _ := schema.NewArtifact(string(schema.KindDesign), "designer",
			schema.Design{Pages: sampleDesignPages()})
		stored, _ := board.Put(da)
		in := SubAgentInput{
			Topic:    "主题",
			Inputs:   []schema.Handoff{schema.HandoffOf(stored)},
			Upstream: board,
		}

		_, err := NewRenderer(RendererOptions{Renderer: &fakeRenderer{}}).
			Run(context.Background(), in)
		if err == nil || !strings.Contains(err.Error(), "outline") {
			t.Fatalf("应报缺少 outline，实际 %v", err)
		}
	})

	t.Run("缺 design", func(t *testing.T) {
		board := state.NewBlackboard()
		oa, _ := schema.NewArtifact(string(schema.KindOutline), "outline",
			schema.Outline{Pages: sampleOutlinePages()})
		stored, _ := board.Put(oa)
		in := SubAgentInput{
			Topic:    "主题",
			Inputs:   []schema.Handoff{schema.HandoffOf(stored)},
			Upstream: board,
		}

		_, err := NewRenderer(RendererOptions{Renderer: &fakeRenderer{}}).
			Run(context.Background(), in)
		if err == nil || !strings.Contains(err.Error(), "design") {
			t.Fatalf("应报缺少 design，实际 %v", err)
		}
	})
}

// ─── 端口可替换（原则五的实证） ─────────────────────────────────────

func TestRendererPortIsReplaceable(t *testing.T) {
	// 换一个渲染后端，这个角色一行都不用改
	fake := &fakeRenderer{}
	in := renderInput(t, sampleOutlinePages(), sampleDesignPages())

	r := NewRenderer(RendererOptions{Renderer: fake})
	if _, err := r.Run(context.Background(), in); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	calls, _ := fake.snapshot()
	if len(calls) != 3 {
		t.Fatalf("替身应被调用 3 次，实际 %d", len(calls))
	}
	// 顺序稳定：调用记录里能看出 3 页都被渲染了（并发顺序不定，所以用集合比对）
	joined := strings.Join(calls, "|")
	for _, title := range []string{"行业现状", "核心矛盾", "结论与建议"} {
		if !strings.Contains(joined, title) {
			t.Errorf("第 %q 页没被渲染", title)
		}
	}
}

// 兜底：确认真实渲染器满足端口接口。
var _ SlideRenderer = (*tools.RenderSlideHTML)(nil)
