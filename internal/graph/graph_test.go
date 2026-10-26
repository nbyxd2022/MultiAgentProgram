package graph

import (
	"context"
	"strings"
	"testing"

	"multiagent/internal/schema"
)

// ─── 脚手架 ─────────────────────────────────────────────────────────

// fakeNode 是测试用的节点：只声明依赖与产出，不真的做事。
//
// 它刻意**只有声明**没有逻辑 —— 因为本文件测的是**图**，
// 而图只读声明（`Needs` / `Kind` /`Name`），从不调用 `Run`。
// 一个需要真实逻辑才能测的图，说明它管得太多了。
type fakeNode struct {
	name  string
	kind  schema.Kind
	desc  string
	needs []schema.Kind
	run   func(ctx context.Context, in NodeInput) (*Outcome, error)
}

func (f *fakeNode) Name() string         { return f.name }
func (f *fakeNode) Kind() schema.Kind    { return f.kind }
func (f *fakeNode) Description() string  { return f.desc }
func (f *fakeNode) Needs() []schema.Kind { return f.needs }
func (f *fakeNode) Run(ctx context.Context, in NodeInput) (*Outcome, error) {
	if f.run != nil {
		return f.run(ctx, in)
	}
	return nil, nil
}

// stdNodes 是 L3 的真实形状：四个角色 + 一个预置输入。
func stdNodes() []Node {
	return []Node{
		&fakeNode{name: "research", kind: schema.KindResearch, desc: "研究员",
			needs: []schema.Kind{schema.KindTopic}},
		&fakeNode{name: "outline", kind: schema.KindOutline, desc: "大纲编写者",
			needs: []schema.Kind{schema.KindResearch}},
		&fakeNode{name: "design", kind: schema.KindDesign, desc: "版面设计者",
			needs: []schema.Kind{schema.KindOutline}},
		&fakeNode{name: "render", kind: schema.KindRender, desc: "渲染执行者",
			needs: []schema.Kind{schema.KindOutline, schema.KindDesign}},
	}
}

func buildStd(t *testing.T, nodes []Node) *Graph {
	t.Helper()
	g, err := BuildGraph(GraphSpec{Nodes: nodes, Presets: []schema.Kind{schema.KindTopic}})
	if err != nil {
		t.Fatalf("建图失败: %v", err)
	}
	return g
}

func stdGraph(t *testing.T) *Graph { return buildStd(t, stdNodes()) }

// fillAll 让图上所有节点都产出"完整且新鲜"的产物，使 CanSkip 全部通过。
//
// 按拓扑序填是关键：填下游时上游已经是最新版，于是 BasedOn 天然成立。
func fillAll(t *testing.T, g *Graph, st *State) {
	t.Helper()
	for _, name := range g.Order() {
		n, _ := g.Node(name)
		a := art(string(n.Kind()), 1)
		for _, dep := range n.Needs() {
			if latest, ok := st.Latest(string(dep)); ok {
				a.BasedOn = append(a.BasedOn, schema.BasisOf(latest))
			}
		}
		put(st, a)
	}
}

// ─── 核心：边是推导出来的 ───────────────────────────────────────────

// ⚠️ 本文件最重要的一条：**边由 `Needs()` 推导，不是手写的**。
//
// 对照企业仓库的 `buildGraph`：那里有一张 `[][2]string` 的边表，
// 七行边、七个依赖，全靠人再抄一遍。抄漏一条是**静默的** ——
// 图还能跑，只是少了一条约束。
func TestBuildGraphDerivesEdgesFromNeeds(t *testing.T) {
	g := stdGraph(t)

	got := make([]string, 0, len(g.Edges()))
	for _, e := range g.Edges() {
		got = append(got, e.From+"→"+e.To)
	}
	// 四条边，**一条都不是手写的**，全部来自 Needs()
	want := []string{
		"design→render",    // render.Needs() 含 design
		"outline→design",   // design.Needs() 含 outline
		"outline→render",   // render.Needs() 也含 outline
		"research→outline", // outline.Needs() 含 research
	}
	// 注意：topic → research 这条**不是节点之间的边**
	// （topic 是预置输入，不是一个节点）。
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("推导出的边不对：\n  得到 %v\n  期望 %v", got, want)
	}

	// 边界也要验：预置输入不该出现在边的任一端。
	for _, e := range g.Edges() {
		if e.From == KeyTopic || e.To == KeyTopic {
			t.Errorf("预置输入不该出现在节点之间的边上：%+v", e)
		}
	}
}

// 拓扑序必须**依赖在前**，而且**确定**。
//
// 确定性这件事在 map 上是白拿的（Go 的 map 遍历顺序随机），
// 所以这里建 20 次图来把它逼出来 —— 一次是碰运气，二十次才是证据。
func TestBuildGraphOrderIsDependencyFirstAndDeterministic(t *testing.T) {
	want := []string{"research", "outline", "design", "render"}

	for i := 0; i < 20; i++ {
		g := stdGraph(t)
		if got := strings.Join(g.Order(), ","); got != strings.Join(want, ",") {
			t.Fatalf("第 %d 次建图，拓扑序是 %v，期望 %v（顺序不确定！）", i+1, g.Order(), want)
		}
	}

	// 换一个**节点列表顺序**再建一次 —— 顺序不该影响结果。
	nodes := stdNodes()
	for i, j := 0, len(nodes)-1; i < j; i, j = i+1, j-1 {
		nodes[i], nodes[j] = nodes[j], nodes[i]
	}
	if got := buildStd(t, nodes).Order(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("打乱节点列表顺序后拓扑序变了：%v", got)
	}
}

// 依赖在前这条性质，用一句断言抓不住，用"每个节点的依赖都排在它之前"能抓住。
func TestEveryDependencyComesFirst(t *testing.T) {
	g := stdGraph(t)
	pos := map[string]int{}
	for i, name := range g.Order() {
		pos[name] = i
	}

	for _, name := range g.Order() {
		n, _ := g.Node(name)
		for _, dep := range n.Needs() {
			owner, ok := nodeOfKind(g.nodes, dep)
			if !ok {
				continue // 预置输入
			}
			if pos[owner] >= pos[name] {
				t.Errorf("%s 依赖 %s，但 %s 排在它后面（%d >= %d）",
					name, owner, owner, pos[owner], pos[name])
			}
		}
	}
}

// ─── 建图期的五种拒绝 ───────────────────────────────────────────────

func TestBuildGraphRejects(t *testing.T) {
	ok := func() Node {
		return &fakeNode{name: "a", kind: schema.KindResearch, desc: "a", needs: []schema.Kind{schema.KindTopic}}
	}

	cases := []struct {
		name    string
		spec    GraphSpec
		wantSub string
	}{
		{
			"没有节点",
			GraphSpec{Presets: []schema.Kind{schema.KindTopic}},
			"至少要有一个节点",
		},
		{
			"列表里有 nil",
			GraphSpec{Nodes: []Node{nil}, Presets: []schema.Kind{schema.KindTopic}},
			"nil",
		},
		{
			"节点没有名字",
			GraphSpec{Nodes: []Node{&fakeNode{name: "  ", kind: schema.KindResearch}},
				Presets: []schema.Kind{schema.KindTopic}},
			"没有名字",
		},
		{
			"节点名重复",
			GraphSpec{Nodes: []Node{
				&fakeNode{name: "a", kind: schema.KindResearch, needs: []schema.Kind{schema.KindTopic}},
				&fakeNode{name: "a", kind: schema.KindOutline},
			}, Presets: []schema.Kind{schema.KindTopic}},
			"重复",
		},
		{
			"kind 不合法",
			GraphSpec{Nodes: []Node{&fakeNode{name: "a", kind: "乱写的"}},
				Presets: []schema.Kind{schema.KindTopic}},
			"不合法",
		},
		{
			// 两个节点抢同一个黑板 key：版本号会交替递增，
			// 于是 "outline@2" 到底是谁的第 2 版就说不清了 —— 引用语义直接失效。
			"两个节点产出同一个 kind",
			GraphSpec{Nodes: []Node{
				&fakeNode{name: "a", kind: schema.KindResearch, needs: []schema.Kind{schema.KindTopic}},
				&fakeNode{name: "b", kind: schema.KindResearch},
			}, Presets: []schema.Kind{schema.KindTopic}},
			"抢同一个黑板 key",
		},
		{
			"依赖了一个不存在的产物（永不就绪的前置）",
			GraphSpec{Nodes: []Node{
				&fakeNode{name: "a", kind: schema.KindResearch, needs: []schema.Kind{schema.KindOutline}},
			}, Presets: []schema.Kind{schema.KindTopic}},
			"永远不会就绪",
		},
		{
			"依赖了一个不合法的 kind",
			GraphSpec{Nodes: []Node{
				&fakeNode{name: "a", kind: schema.KindResearch, needs: []schema.Kind{"乱写的"}},
			}, Presets: []schema.Kind{schema.KindTopic}},
			"不合法的 kind",
		},
		{
			"预置输入不合法",
			GraphSpec{Nodes: []Node{ok()}, Presets: []schema.Kind{"乱写的"}},
			"不合法",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, err := BuildGraph(c.spec)
			if err == nil {
				t.Fatalf("应当被拒绝，却建出来了（%d 个节点）", len(g.Order()))
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误里应含 %q，实际: %v", c.wantSub, err)
			}
		})
	}
}

// 一个 kind 不能既是"预置输入"又被某个节点产出 —— 它是输入还是产品？
//
// 这条不是学究：**两者只能选一个**。都允许的话，
// "它该在开跑前就在黑板上、还是该被某个节点产出"这个问题就没有答案。
func TestBuildGraphRejectsPresetClash(t *testing.T) {
	_, err := BuildGraph(GraphSpec{
		Nodes: []Node{
			&fakeNode{name: "a", kind: schema.KindTopic, desc: "产出 topic 的节点"},
		},
		Presets: []schema.Kind{schema.KindTopic},
	})
	if err == nil {
		t.Fatal("应当被拒绝")
	}
	if !strings.Contains(err.Error(), "输入还是产品") {
		t.Errorf("错误应点出这个矛盾，实际: %v", err)
	}
}

// 有环必须被拒绝，而且**报出环上的节点**。
//
// 只说"有环"，排查的人得自己去找是哪几个节点咬住了 ——
// 与"拒绝必须可自愈"同一条：只说不行，等于让调用方自己去找。
func TestBuildGraphRejectsCycleAndNamesIt(t *testing.T) {
	_, err := BuildGraph(GraphSpec{
		Nodes: []Node{
			&fakeNode{name: "a", kind: schema.KindResearch, needs: []schema.Kind{schema.KindOutline}},
			&fakeNode{name: "b", kind: schema.KindOutline, needs: []schema.Kind{schema.KindResearch}},
		},
		Presets: []schema.Kind{schema.KindTopic},
	})
	if err == nil {
		t.Fatal("有环应当被拒绝")
	}
	if !strings.Contains(err.Error(), "环") {
		t.Errorf("错误里应含「环」，实际: %v", err)
	}
	// 环上的两个节点名都要出现
	for _, n := range []string{"a", "b"} {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("错误里应点出环上的节点 %q，实际: %v", n, err)
		}
	}
}

// 三节点环也要能报出来（不只是一对一对的环）。
func TestBuildGraphRejectsThreeNodeCycle(t *testing.T) {
	_, err := BuildGraph(GraphSpec{
		Nodes: []Node{
			&fakeNode{name: "a", kind: schema.KindResearch, needs: []schema.Kind{schema.KindDesign}},
			&fakeNode{name: "b", kind: schema.KindOutline, needs: []schema.Kind{schema.KindResearch}},
			&fakeNode{name: "c", kind: schema.KindDesign, needs: []schema.Kind{schema.KindOutline}},
		},
		Presets: []schema.Kind{schema.KindTopic},
	})
	if err == nil {
		t.Fatal("三节点环应当被拒绝")
	}
	for _, n := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("错误里应点出环上的节点 %q，实际: %v", n, err)
		}
	}
}

// ─── Next：位置由产物推导 ───────────────────────────────────────────

// ⚠️ 图里**没有游标**。"下一个该跑谁"是从产物算出来的。
//
// 于是它顺带白拿一个性质：**恢复时不需要"从游标往后跑"** ——
// 再问一次这个问题，答案会自动跳过所有已经做完的节点。
func TestNextWalksTheTopologicalOrder(t *testing.T) {
	g := stdGraph(t)
	st := newTestState(t)

	// 一开始谁都没做 → 第一个是 research（它是唯一没有节点前置的）
	if got := g.Next(st); got != "research" {
		t.Fatalf("第一步应当是 research，实际 %q", got)
	}

	// 做完 research → 下一个是 outline
	put(st, art("research", 1, withBasedOn(schema.BasisOf(st.Board[KeyTopic][0]))))
	if got := g.Next(st); got != "outline" {
		t.Fatalf("第二步应当是 outline，实际 %q", got)
	}

	// 做完 outline → 下一个是 design（render 还要等 design）
	put(st, art("outline", 1, withBasedOn(schema.Basis{Key: "research", Version: 1})))
	if got := g.Next(st); got != "design" {
		t.Fatalf("第三步应当是 design，实际 %q", got)
	}
}

// 全部做完 → 返回空串，表示这一轮结束。
func TestNextReturnsEmptyWhenEverythingCanSkip(t *testing.T) {
	g := stdGraph(t)
	st := newTestState(t)
	fillAll(t, g, st)

	if got := g.Next(st); got != "" {
		t.Errorf("全部做完时应当返回空，实际 %q（CanSkip 出问题了？）", got)
	}
}

// Next 必须**跳过已经做完的**，而不是"按顺序重跑一遍"。
func TestNextSkipsCompletedNodes(t *testing.T) {
	g := stdGraph(t)
	st := newTestState(t)

	// 只做完前两个
	put(st, art("research", 1, withBasedOn(schema.BasisOf(st.Board[KeyTopic][0]))))
	put(st, art("outline", 1, withBasedOn(schema.Basis{Key: "research", Version: 1})))

	if got := g.Next(st); got != "design" {
		t.Errorf("应当跳过已完成的 research/outline，直接到 design，实际 %q", got)
	}
}

// 上游重做之后，**下游必须重新变成"该做"**。
//
// 这条把 L2-1（血缘/新鲜度）与图的调度接在了一起：
// `CanSkip` 判"不新鲜" → 图自然把它排回队首。
// 换句话说：**"改了大纲要重做哪几步"这个问题，图不需要单独实现。**
func TestStaleUpstreamPullsDownstreamBackIn(t *testing.T) {
	g := stdGraph(t)
	st := newTestState(t)
	fillAll(t, g, st)

	if got := g.Next(st); got != "" {
		t.Fatalf("先决条件不成立：应当全部可跳过，实际 %q", got)
	}

	// 用户改了大纲：outline **重做了一版**（带上正常血缘 —— 它仍依据 research@1）。
	//
	// ⚠️ 注意这里是"重做完成"而不是"刚开始重做"：
	// outline@2 自己已经**新鲜**了，所以它不是下一个该跑的。
	// **该重做的是它的下游** —— design@1 与 render@1 的依据都指向被顶掉的 outline@1。
	put(st, art("outline", 2, withBasedOn(schema.Basis{Key: "research", Version: 1})))

	if got := g.Next(st); got != "design" {
		t.Fatalf("outline 重做之后应当接着重做 design，实际 %q", got)
	}

	// 补上 design → 接着轮到 render（它同时依据 outline 与 design）
	put(st, art("design", 2, withBasedOn(schema.Basis{Key: "outline", Version: 2})))
	if got := g.Next(st); got != "render" {
		t.Errorf("design 补上之后应当轮到 render，实际 %q", got)
	}
}

// ⚠️ 改成"输入"也一样：**主题变了要让整条链重做**。
//
// 这是 06-L3设计.md §10 问题 2 的答案能不能落地的验证 ——
// 如果 topic 被特殊对待（不是一份产物），这里就必须**单独写一套逻辑**。
func TestChangingTopicRestartsTheWholeGraph(t *testing.T) {
	g := stdGraph(t)
	st := newTestState(t)
	fillAll(t, g, st)

	// 用户换了主题
	put(st, art(KeyTopic, 2, func(a *schema.Artifact) {
		a.Payload = []byte(`{"text":"AI 应用落地现状"}`)
	}))

	if got := g.Next(st); got != "research" {
		t.Errorf("换了主题应当从 research 重来，实际 %q —— "+
			"说明输入被判据特殊对待了", got)
	}
}

// ─── Outcome 校验 ───────────────────────────────────────────────────

func TestCheckOutcome(t *testing.T) {
	g := stdGraph(t)

	cases := []struct {
		name    string
		from    string
		o       *Outcome
		wantSub string // "" = 应当通过
	}{
		{"nil 表示没有意见", "outline", nil, ""},
		{"空 Next 表示没有意见", "outline", &Outcome{}, ""},
		{"跳到自己的下游", "outline", &Outcome{Next: "design"}, ""},
		{"重做自己（重试）", "outline", &Outcome{Next: "outline"}, ""},
		// ⚠️ 这里不能选 outline→render：render.Needs() 里**确实有** outline，
		//    所以那条边是存在的（写这条用例时第一版就选错了）。
		//    design→research 才是真的没有边：research 不需要 design 的产出。
		{"跳到没有依赖关系的节点", "design", &Outcome{Next: "research"}, "没有这条依赖边"},
		{"跳到不存在的节点", "outline", &Outcome{Next: "不存在的"}, "不是一个已知节点"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := g.CheckOutcome(c.from, c.o)
			if c.wantSub == "" {
				if err != nil {
					t.Errorf("应当通过，却报：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应当被拒绝，却通过了")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误里应含 %q，实际: %v", c.wantSub, err)
			}
		})
	}
}

// 拒绝时要给出已知节点的清单（可自愈）。
func TestCheckOutcomeListsKnownNodes(t *testing.T) {
	g := stdGraph(t)
	err := g.CheckOutcome("outline", &Outcome{Next: "不存在"})
	if err == nil {
		t.Fatal("应当被拒绝")
	}
	for _, name := range g.Order() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("错误里应列出已知节点 %q，实际: %v", name, err)
		}
	}
}

// ─── 分层：把"哪里可以并行"变成可验证的 ─────────────────────────────

func TestLayersOfALinearChain(t *testing.T) {
	g := stdGraph(t)
	layers := g.Layers()

	// research → outline → design → render：四层，每层一个
	if len(layers) != 4 {
		t.Fatalf("应当是 4 层，实际 %d 层：%v", len(layers), layers)
	}
	for i, l := range layers {
		if len(l) != 1 {
			t.Errorf("第 %d 层应当只有 1 个节点，实际 %v", i, l)
		}
	}
	if layers[0][0] != "research" || layers[3][0] != "render" {
		t.Errorf("分层顺序不对：%v", layers)
	}
}

// ⚠️ 这条测试兑现的是 L2 复盘里的一句**口头承诺**：
//
//	"加一个只依赖 outline 的配图角色，它会自动和 designer 排成同一层，
//	 不需要谁去说这两个可以并行。"
//
// 承诺写成代码就能验证。**这是 Layers() 存在的唯一理由** ——
// 不是"现在要并行"，而是让"未来那个问题"有一个可执行的定义。
func TestLayersRevealParallelOpportunities(t *testing.T) {
	// ⚠️ 这里不能用 `stdNodes()` 加一个节点：五个 Kind 在 stdNodes 里已经用满了，
	//    新节点只能复用 KindTopic —— 而 topic 是 research 的前置，
	//    于是会形成 illustrate→research→outline→illustrate 的**环**。
	//    （副产物的一条事实：**加第 6 个角色要先加一个 Kind**，
	//     这是枚举式协议的成本，也是它的收益 —— 见 schema.Kind.Valid。）
	nodes := []Node{
		&fakeNode{name: "research", kind: schema.KindResearch, desc: "研究员",
			needs: []schema.Kind{schema.KindTopic}},
		&fakeNode{name: "outline", kind: schema.KindOutline, desc: "大纲",
			needs: []schema.Kind{schema.KindResearch}},
		&fakeNode{name: "design", kind: schema.KindDesign, desc: "版面",
			needs: []schema.Kind{schema.KindOutline}},
		&fakeNode{name: "illustrate", kind: schema.KindRender, desc: "配图（只依赖大纲）",
			needs: []schema.Kind{schema.KindOutline}},
	}

	g, err := BuildGraph(GraphSpec{Nodes: nodes, Presets: []schema.Kind{schema.KindTopic}})
	if err != nil {
		t.Fatalf("建图失败: %v", err)
	}

	layers := g.Layers()
	found := false
	for _, l := range layers {
		if len(l) > 1 && contains(l, "design") && contains(l, "illustrate") {
			found = true
		}
	}
	if !found {
		t.Errorf("design 与 illustrate 都只依赖 outline，应当自动排在同一层，实际：%v", layers)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// ─── 打印图拓扑（教学计划产出物）─────────────────────────────────────

func TestDescribeShowsTheStructure(t *testing.T) {
	g := stdGraph(t)
	out := g.Describe()

	// 打出来给人和 AI 看：`go test -run TestDescribeShowsTheStructure -v -count=1`
	t.Logf("\n%s", out)

	must := []string{
		"图拓扑",                // 标题
		"4 个节点",              // 规模
		"4 条边",               // 推导出来的边的数量
		"topic",              // 预置输入
		"由 Needs() 推导，不是手写的", // ★ 本层最重要的那条性质
		"research → outline → design → render", // 调度顺序
		"研究员", // 节点的描述
	}
	for _, s := range must {
		if !strings.Contains(out, s) {
			t.Errorf("Describe 输出里应含 %q，实际：\n%s", s, out)
		}
	}

	// 边要逐条打出来（排查"为什么这个节点没跑"时第一眼看的就是它）
	for _, e := range g.Edges() {
		if !strings.Contains(out, e.From) || !strings.Contains(out, e.To) {
			t.Errorf("Describe 里缺少边 %s→%s：\n%s", e.From, e.To, out)
		}
	}
}

// Describe 必须**稳定**：同一张图打印两次要一模一样。
// （不稳定就没法放进 golden 测试、没法 diff —— 与 L1-6 同一条。）
func TestDescribeIsStable(t *testing.T) {
	a := stdGraph(t).Describe()
	b := stdGraph(t).Describe()
	if a != b {
		t.Errorf("两次打印不一致：\n%s\n---\n%s", a, b)
	}
}

// ─── 与 State 的接口：Attempt 与中断 ID ─────────────────────────────

// ⚠️ `AttemptFor` 必须从黑板推导，不能是计数器 ——
// 否则恢复时重跑会派生出一个**新的中断 ID**，人给的答案就永远对不上。
func TestAttemptForIsDerivedFromTheBoard(t *testing.T) {
	st := newTestState(t)

	// 还没做过 → 第 1 次
	if got := st.AttemptFor(schema.KindOutline); got != 1 {
		t.Errorf("还没做过应当是第 1 次，实际 %d", got)
	}

	// 中断了（没有产出）→ 仍然是第 1 次 → ID 不变 → 答案对得上
	if got := st.AttemptFor(schema.KindOutline); got != 1 {
		t.Errorf("中断没有产出，attempt 不该变，实际 %d", got)
	}

	// 真产出过一版 → 下一次是第 2 次
	put(st, art("outline", 1))
	if got := st.AttemptFor(schema.KindOutline); got != 2 {
		t.Errorf("产出一版之后应当是第 2 次，实际 %d", got)
	}
}

// 同一个 (runID, node) 在"没产出"时反复构造中断，ID 必须**完全一致**。
//
// 这条是"恢复能对上号"的最小复现：中断没产出 → AttemptFor 不变 → ID 不变。
func TestInterruptIDIsStableAcrossResume(t *testing.T) {
	st := newTestState(t)

	in1 := NodeInput{RunID: st.RunID, Attempt: st.AttemptFor(schema.KindOutline)}
	a := in1.Interrupt("outline", KindConfirmArtifact, "确认大纲？")

	// 模拟"进程重启后再走一遍"：状态没变（还是没有 outline）
	in2 := NodeInput{RunID: st.RunID, Attempt: st.AttemptFor(schema.KindOutline)}
	b := in2.Interrupt("outline", KindConfirmArtifact, "确认大纲？")

	if a.ID != b.ID {
		t.Errorf("中断没产出，ID 应当稳定（%q），重启后却变成 %q —— "+
			"人的回答将永远对不上", a.ID, b.ID)
	}
}

// 反过来：真重做过一版之后，新中断必须拿到**新的 ID**（否则会串台）。
func TestInterruptIDChangesAfterRealRework(t *testing.T) {
	st := newTestState(t)

	first := NodeInput{RunID: st.RunID, Attempt: st.AttemptFor(schema.KindOutline)}.
		Interrupt("outline", KindConfirmArtifact, "确认大纲？")

	put(st, art("outline", 1)) // 真的产出了一版

	second := NodeInput{RunID: st.RunID, Attempt: st.AttemptFor(schema.KindOutline)}.
		Interrupt("outline", KindConfirmArtifact, "再确认一次？")

	if first.ID == second.ID {
		t.Errorf("重做过之后应当是新 ID，两次都是 %q —— 上一次的答案会被套到新问题上", first.ID)
	}
}

// ─── GraphSpec 的契约 ──────────────────────────────────────────────

// 预置输入让"根节点"也有前置 —— 于是图里**没有"输入"这种特例**。
//
// 用一条更硬的断言来表达：把 topic 从 Presets 拿掉，图**必须建不出来**
// （因为 research.Needs() 里有它，而没有节点产出它）。
// 这说明 research 的依赖不是装饰，是**真的被检查的**。
func TestWithoutPresetTheGraphCannotBeBuilt(t *testing.T) {
	_, err := BuildGraph(GraphSpec{Nodes: stdNodes()}) // 忘了传 Presets
	if err == nil {
		t.Fatal("少了预置输入，research 的前置永远不会就绪 —— 建图期就该报错")
	}
	if !strings.Contains(err.Error(), "永远不会就绪") {
		t.Errorf("错误应说清是前置永不就绪，实际: %v", err)
	}
}
