package graph

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"multiagent/internal/budget"
	"multiagent/internal/checkpoint"
	"multiagent/internal/observ"
	"multiagent/internal/schema"
)

// ─── 脚手架 ─────────────────────────────────────────────────────────

// testNode 是一个只由闭包决定的节点 —— 行为是**输入的纯函数**。
//
// ⚠️ 刻意**不**做"按第几次调用返回不同剧本"的替身。
// 那种替身会把状态藏在替身里，一旦有重试 / 恢复 / 乱序，次数就对不上了 ——
// 这是 R23 拒绝 `ScriptedClient` 时的同一条理由，在节点上同样成立。
type testNode struct {
	name  string
	kind  schema.Kind
	desc  string
	needs []schema.Kind
	run   func(NodeInput) (*Outcome, error)
}

func (n *testNode) Name() string         { return n.name }
func (n *testNode) Kind() schema.Kind    { return n.kind }
func (n *testNode) Description() string  { return n.desc }
func (n *testNode) Needs() []schema.Kind { return n.needs }
func (n *testNode) Run(_ context.Context, in NodeInput) (*Outcome, error) {
	return n.run(in)
}

// produce 是"老老实实产出一步"的默认行为。
func produce(p schema.Payload) func(NodeInput) (*Outcome, error) {
	return func(NodeInput) (*Outcome, error) { return &Outcome{Product: p}, nil }
}

const testTopic = "新能源汽车行业趋势"

// stdGraphRuns 建一张与 L3 真实形状一致的图，允许逐个节点替换行为。
func stdGraphRuns(t *testing.T, runs map[string]func(NodeInput) (*Outcome, error)) *Graph {
	t.Helper()

	mk := func(name string, kind schema.Kind, desc string, needs []schema.Kind,
		def func(NodeInput) (*Outcome, error)) Node {
		run := def
		if f, ok := runs[name]; ok {
			run = f
		}
		return &testNode{name: name, kind: kind, desc: desc, needs: needs, run: run}
	}

	g, err := BuildGraph(GraphSpec{
		Nodes: []Node{
			mk("research", schema.KindResearch, "研究员", []schema.Kind{schema.KindTopic},
				produce(schema.Research{Topic: testTopic})),
			mk("outline", schema.KindOutline, "大纲编写者", []schema.Kind{schema.KindResearch},
				produce(schema.Outline{})),
			mk("design", schema.KindDesign, "版面设计者", []schema.Kind{schema.KindOutline},
				produce(schema.Design{})),
			mk("render", schema.KindRender, "渲染执行者",
				[]schema.Kind{schema.KindOutline, schema.KindDesign},
				produce(schema.RenderResult{Files: []string{"a.html"}})),
		},
		Presets: []schema.Kind{schema.KindTopic},
	})
	if err != nil {
		t.Fatalf("建图失败: %v", err)
	}
	return g
}

// newRunner 在**给定的目录**上建一个 Runner。
//
// 传目录（而不是 `t.TempDir()`）是为了支持"重启进程"的测试：
// 同一个目录 + 全新的 Runner 实例 = 进程重启后重来。
func newRunnerOn(t *testing.T, g *Graph, dir, runID string, opt ...func(*RunnerOptions)) *Runner {
	t.Helper()
	store, err := checkpoint.NewFileStore(dir)
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	o := RunnerOptions{Graph: g, Store: store, RunID: runID, Topic: testTopic}
	for _, f := range opt {
		f(&o)
	}
	r, err := NewRunner(o)
	if err != nil {
		t.Fatalf("装配 Runner 失败: %v", err)
	}
	return r
}

func newRunner(t *testing.T, g *Graph, runID string, opt ...func(*RunnerOptions)) (*Runner, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ckpt")
	return newRunnerOn(t, g, dir, runID, opt...), dir
}

// mustRun 跑一轮并要求它正常结束（不中断、不出错）。
func mustRun(t *testing.T, r *Runner) Result {
	t.Helper()
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	return res
}

// ─── 正常推进 ───────────────────────────────────────────────────────

func TestRunnerWalksTheGraphAndProducesEverything(t *testing.T) {
	g := stdGraphRuns(t, nil)
	r, _ := newRunner(t, g, "run-1")

	var events []observ.Event
	r.tracer = observ.TracerFunc(func(e observ.Event) { events = append(events, e) })

	res := mustRun(t, r)

	if res.Status != StatusCompleted {
		t.Fatalf("应当跑完，实际 %s", res.Status)
	}
	if strings.Join(res.Ran, ",") != "research,outline,design,render" {
		t.Errorf("执行顺序应当是拓扑序，实际 %v", res.Ran)
	}
	for _, k := range []string{KeyTopic, "research", "outline", "design", "render"} {
		if res.State.Versions(k) == 0 {
			t.Errorf("黑板上有 %q 的 0 版", k)
		}
	}

	// 事件里必须能看出顺序（可观测性不该因为"这一层是图"而缺席）
	//
	// ⚠️ R44 之后**没有** `e.Step` 可断言了 —— 因为它是派生值（见 `runner.go` 顶部）。
	//	所以这里数 `KindStart` 的顺序：**那本来就是"第几步"的定义**。
	var started []string
	for _, e := range events {
		if e.Kind == observ.KindStart {
			started = append(started, e.Phase)
		}
	}
	if strings.Join(started, ",") != "research,outline,design,render" {
		t.Errorf("start 事件顺序不对：%v", started)
	}
}

// ⚠️ 血缘必须由 Runner 填，而且**链条要连得上**：
// research 依据 topic，outline 依据 research，……最终 render 依据 outline+design。
//
// 这条把 L3 的图与 L2 的血缘接在了一起：**自动化了，但没绕过判据。**
func TestRunnerRecordsLineage(t *testing.T) {
	g := stdGraphRuns(t, nil)
	r, _ := newRunner(t, g, "run-1")
	res := mustRun(t, r)

	want := map[string][]string{
		"research": {KeyTopic + "@1"},
		"outline":  {"research@1"},
		"design":   {"outline@1"},
		"render":   {"outline@1", "design@1"},
	}
	for key, refs := range want {
		a := res.State.Board[key][0]
		got := make([]string, 0, len(a.BasedOn))
		for _, b := range a.BasedOn {
			got = append(got, b.Ref())
		}
		if strings.Join(got, ",") != strings.Join(refs, ",") {
			t.Errorf("%s 的血缘应当是 %v，实际 %v", key, refs, got)
		}
	}
}

// 跑完之后**再跑一次**：`Ran` 必须是空的 —— 没有任何东西需要重做。
func TestRerunSkipsEverything(t *testing.T) {
	g := stdGraphRuns(t, nil)
	r, _ := newRunner(t, g, "run-1")
	mustRun(t, r)

	res := mustRun(t, r) // 第二遍
	if len(res.Ran) != 0 {
		t.Errorf("全部做完之后再跑不该执行任何节点，实际 %v", res.Ran)
	}
	if res.Status != StatusCompleted {
		t.Errorf("应当报完成，实际 %s", res.Status)
	}
}

// ⚠️⚠️ **L3 的验收标准**：杀掉进程重启后仍能恢复，且产出与不中断一致。
//
// 这里用"同一个目录 + 全新的 Runner 实例"来模拟重启 —— 内存里什么都没有，
// 唯一的信息来源是磁盘上的 checkpoint。
func TestResumeAcrossProcessRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ckpt")
	g := stdGraphRuns(t, nil)

	// ── 进程 A：在 outline **产出之后**被掐断（模拟"进程从这里没了"）
	//
	// ⚠️ 挂在 `KindProduct` 而不是 `KindStart`：`KindStart` 在 `node.Run`
	//	**之前**发出，挂在那里的话节点照样会跑完 —— 断言就变成在测一件别的事。
	rA := newRunnerOn(t, g, dir, "run-1")
	ctxA, cancelA := context.WithCancel(context.Background())
	rA.tracer = observ.TracerFunc(func(e observ.Event) {
		if e.Kind == observ.KindProduct && e.Phase == "outline" {
			cancelA()
		}
	})
	if _, err := rA.Run(ctxA); err == nil {
		t.Fatal("ctx 被取消时应当报错")
	}
	cancelA()

	// 磁盘上应当已经有 research 与 outline（**每跑完一个节点就落盘**）
	storeA, _ := checkpoint.NewFileStore(dir)
	raw, found, err := storeA.Load("run-1")
	if err != nil || !found {
		t.Fatalf("中途被打断之后磁盘上应当有 checkpoint：found=%v err=%v", found, err)
	}
	stA, err := UnmarshalState(raw)
	if err != nil {
		t.Fatalf("读回 checkpoint 失败: %v", err)
	}
	if stA.Versions("research") != 1 || stA.Versions("outline") != 1 {
		t.Fatalf("被打断前已完成的产物都该落盘：research=%d outline=%d",
			stA.Versions("research"), stA.Versions("outline"))
	}
	if stA.Versions("design") != 0 {
		t.Fatalf("design 还没跑过，不该有产物，实际 %d 版", stA.Versions("design"))
	}

	// ── 进程 B：全新的 Runner，内存里什么都没有
	rB := newRunnerOn(t, g, dir, "run-1")
	var skipped string
	rB.tracer = observ.TracerFunc(func(e observ.Event) {
		if e.Kind == observ.KindSkipped {
			skipped = e.Detail
		}
	})
	res := mustRun(t, rB)

	if res.Status != StatusCompleted {
		t.Fatalf("恢复后应当跑完，实际 %s", res.Status)
	}
	// ⚠️ 恢复只跑**没做完**的两个节点 —— 这是本用例最该盯住的一行
	if strings.Join(res.Ran, ",") != "design,render" {
		t.Errorf("恢复时应当只跑未完成的节点，实际 %v", res.Ran)
	}
	// 而且它知道"哪些已经做完了" —— 这是"位置由产物推导"最直观的证据
	if !strings.Contains(skipped, "research") || !strings.Contains(skipped, "outline") {
		t.Errorf("应当报告 research/outline 已就绪被跳过，实际 %q", skipped)
	}

	// 产出与"一口气跑完"的结果一致：**已完成的绝不重做**
	for _, k := range []string{"research", "outline", "design", "render"} {
		if res.State.Versions(k) != 1 {
			t.Errorf("%s 应当恰好 1 版（不重做已完成的那部分），实际 %d 版",
				k, res.State.Versions(k))
		}
	}
}

// ─── 中断与恢复 ─────────────────────────────────────────────────────

// 让 outline 在产出之后中断（"我做完了，你看看再往下"）。
func interruptingOutline(t *testing.T, seen *[]NodeInput) func(NodeInput) (*Outcome, error) {
	return func(in NodeInput) (*Outcome, error) {
		if in.Answer == nil {
			if seen != nil {
				*seen = append(*seen, in)
			}
			it := in.Interrupt("outline", KindConfirmArtifact, "请确认这份大纲")
			return &Outcome{Product: schema.Outline{}, Interrupt: &it}, nil
		}
		if seen != nil {
			*seen = append(*seen, in)
		}
		return &Outcome{Product: schema.Outline{}}, nil
	}
}

func TestRunnerInterruptPersistsAndStops(t *testing.T) {
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){
		"outline": interruptingOutline(t, nil),
	})
	r, dir := newRunner(t, g, "run-1")

	res := mustRun(t, r)
	if res.Status != StatusInterrupted {
		t.Fatalf("应当停在中断上，实际 %s", res.Status)
	}
	if res.Interrupted == nil || res.Interrupted.Node != "outline" {
		t.Fatalf("中断信息不对：%+v", res.Interrupted)
	}
	// 它**已经产出了**（"我做完了，你看看"）—— 这是 confirm 能自然继续的前提
	if res.State.Versions("outline") != 1 {
		t.Errorf("中断时产物应当已经入库，实际 %d 版", res.State.Versions("outline"))
	}
	if strings.Join(res.Ran, ",") != "research,outline" {
		t.Errorf("应当在 outline 之后停下，实际 %v", res.Ran)
	}

	// ⚠️⚠️ 中断必须**已经落盘**，而且重启之后**仍然在等** ——
	// 这是"中断是状态、不是一个执行点"的直接验证。
	//
	//	（我第一版这里断言的是"重启后会继续跑完"。那是错的：
	//	  中断的那一跳**已经把产物投进黑板了**，调度器会跳过它并往下走 ——
	//	  于是**用户的问题被静默忽略**。HITL 的语义要求它停下来等。）
	r2 := newRunnerOn(t, g, dir, "run-1")
	again, err := r2.Run(context.Background())
	if err != nil {
		t.Fatalf("重启后 Run 失败: %v", err)
	}
	if again.Status != StatusInterrupted {
		t.Fatalf("重启后应当仍然在等同一个问题的回答，实际 %s（跑了 %v）", again.Status, again.Ran)
	}
	if again.Interrupted == nil || again.Interrupted.ID != res.Interrupted.ID {
		t.Errorf("中断 ID 应当完全一致（它是确定性的）：%v vs %v",
			again.Interrupted, res.Interrupted)
	}
	if len(again.Ran) != 0 {
		t.Errorf("在等人回答时不该跑任何节点，实际 %v", again.Ran)
	}
}

func TestResumeConfirmContinues(t *testing.T) {
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){
		"outline": interruptingOutline(t, nil),
	})
	r, _ := newRunner(t, g, "run-1")
	mustRun(t, r)

	res, err := r.Resume(context.Background(), Decision{Intent: IntentConfirm})
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("确认之后应当跑完，实际 %s", res.Status)
	}
	// 中断的那一跳**不该重做**（它已经有产物了）
	if res.State.Versions("outline") != 1 {
		t.Errorf("confirm 不该让 outline 重做，实际 %d 版", res.State.Versions("outline"))
	}
	if strings.Join(res.Ran, ",") != "design,render" {
		t.Errorf("确认之后应当接着跑下游，实际 %v", res.Ran)
	}
}

// ⚠️ `revise` 必须做两件事，缺一不可：
//
//	① 强制重做中断的那一跳（否则它会被 CanSkip 跳过 —— 改了跟没改一样）
//	② 把"要改什么"（Note）**交给那个节点**（否则它只能瞎猜）
//
// 没有 ② 的 revise 是一个**假动作**。这条测试就是它的看门人。
func TestResumeReviseForcesRedoAndPassesTheAnswer(t *testing.T) {
	var seen []NodeInput
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){
		"outline": interruptingOutline(t, &seen),
	})
	r, _ := newRunner(t, g, "run-1")
	mustRun(t, r)

	res, err := r.Resume(context.Background(), Decision{
		Intent: IntentRevise,
		Note:   "第 3 页换成建议",
	})
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("revise 之后应当继续跑完，实际 %s", res.Status)
	}

	// ① 重做过：outline 从 1 版变成 2 版
	if got := res.State.Versions("outline"); got != 2 {
		t.Errorf("revise 应当让 outline 重做（2 版），实际 %d 版", got)
	}
	// 下游**首次执行**（它们本来就没跑过 —— 中断停在 outline）
	// ⚠️ 注意这里刻意**不是** 2 版：设计这个用例时我第一版写成了 2，
	//	脑子里想的是"下游跟着重做"，但那个场景要求下游**先做完**
	//	（见 TestRevisePullsDownstreamBackIn）。
	//	**写断言时要想清楚"此刻系统应该处于什么状态"，而不是想当然。**
	for _, k := range []string{"design", "render"} {
		if got := res.State.Versions(k); got != 1 {
			t.Errorf("%s 应当是首次执行（1 版），实际 %d 版", k, got)
		}
	}

	// ② 节点确实拿到了人的回答
	if len(seen) < 2 {
		t.Fatalf("outline 应当被调用至少两次，实际 %d 次", len(seen))
	}
	last := seen[len(seen)-1]
	if last.Answer == nil {
		t.Fatal("重做时 outline 必须拿到 Answer —— 否则 revise 是假动作")
	}
	if last.Answer.Note != "第 3 页换成建议" {
		t.Errorf("Answer 的内容不对：%+v", last.Answer)
	}
	// 而第一次（中断那次）不该有 Answer
	if seen[0].Answer != nil {
		t.Errorf("第一次执行不该有 Answer，实际 %+v", seen[0].Answer)
	}
}

// ⚠️⚠️ **L2-1（血缘/新鲜度）在 L3 自动驱动图** —— 这条是本轮最该看懂的测试。
//
// 场景：全部跑完之后，用户要求改大纲。我们**没有为"下游要重做"写任何代码** ——
// 只是 outline 多了一版，`StaleReasons` 就自动让 design / render 变成"该做"，
// 而 `Graph.Next` 又自动把它们排回队里。
//
//	**"改了大纲要重做哪几步"这个问题，图不需要单独实现。**
func TestRevisePullsDownstreamBackIn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ckpt")
	g := stdGraphRuns(t, nil) // 不中断：先一口气跑完
	newRunnerOn(t, g, dir, "run-1").Run(context.Background())

	r := newRunnerOn(t, g, dir, "run-1")

	// 制造一次"针对 outline 的中断"。
	//
	// ⚠️ 为什么要在**全部做完之后**注入？因为要观察"下游跟着重做"，
	//	下游必须**先做过** —— 否则它只是"首次执行"，看不出重做。
	//	（这正是第一个 revise 用例与这个用例的分工。）
	st := r.mustLoadState(t)
	in := NewInterrupt("run-1", "outline", st.AttemptFor(schema.KindOutline),
		KindConfirmArtifact, "请确认这份大纲")
	st.Pending = &in
	if err := r.persist(st); err != nil {
		t.Fatal(err)
	}

	res, err := r.Resume(context.Background(), Decision{
		Intent: IntentRevise, Note: "第 3 页换成建议",
	})
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("revise 之后应当继续跑完，实际 %s", res.Status)
	}

	// 三个节点**都**重做了一版 —— 而我们没有为此写一行逻辑
	for _, k := range []string{"outline", "design", "render"} {
		if got := res.State.Versions(k); got != 2 {
			t.Errorf("%s 应当跟着重做（2 版），实际 %d 版", k, got)
		}
	}
	// research 不受影响（它不依赖 outline）—— 这条同样重要：
	// **重做要"只做该做的"，而不是全量重来。**
	if got := res.State.Versions("research"); got != 1 {
		t.Errorf("research 不该被牵连（1 版），实际 %d 版", got)
	}

	// 执行顺序也必须是"先上游后下游"（照着重做的顺序做一次就对）
	if strings.Join(res.Ran, ",") != "outline,design,render" {
		t.Errorf("重做顺序应当是 outline→design→render，实际 %v", res.Ran)
	}
}

// ⚠️⚠️ **"人的回答也是状态"** 的可验证后果：崩溃后的重放。
//
// 模拟"Resume 刚落完盘就崩了"：磁盘上是"已回答、未消费"，
// 而这次调用**是 `Run` 不是 `Resume`** —— 它必须自己把这个决定重放出来。
func TestRunReplaysAnUnconsumedDecision(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ckpt")
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){
		"outline": interruptingOutline(t, nil),
	})
	r, _ := newRunnerOn(t, g, dir, "run-1"), dir
	mustRun(t, r)

	// ── 手工制造"已回答未消费"的现场（等价于 Resume 落盘之后、drive 之前崩掉）
	store, err := checkpoint.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ := store.Load("run-1")
	st, err := UnmarshalState(raw)
	if err != nil {
		t.Fatal(err)
	}
	st.Pending.Decision = &Decision{Intent: IntentRevise, Note: "第 3 页换成建议"}
	if err := r.persist(st); err != nil {
		t.Fatal(err)
	}

	// ── 重启进程，这次只调 Run（不带任何回答）
	r2 := newRunnerOn(t, g, dir, "run-1")
	var replayed bool
	r2.tracer = observ.TracerFunc(func(e observ.Event) {
		if e.Kind == observ.KindResume {
			replayed = true
		}
	})
	res := mustRun(t, r2)

	if !replayed {
		t.Error("Run 应当自动重放那个未被消费的决定")
	}
	if res.State.Versions("outline") != 2 {
		t.Errorf("重放 revise 应当让 outline 重做，实际 %d 版", res.State.Versions("outline"))
	}
	if res.Status != StatusCompleted {
		t.Errorf("重放之后应当跑完，实际 %s", res.Status)
	}
}

// abort：停止推进，交付已有的部分（**超限 ≠ 失败**）。
func TestResumeAbortDeliversPartial(t *testing.T) {
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){
		"outline": interruptingOutline(t, nil),
	})
	r, _ := newRunner(t, g, "run-1")
	mustRun(t, r)

	res, err := r.Resume(context.Background(), Decision{Intent: IntentAbort})
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	if res.Status != StatusAborted {
		t.Fatalf("应当报「已放弃」，实际 %s", res.Status)
	}
	if len(res.Ran) != 0 {
		t.Errorf("放弃之后不该再跑任何节点，实际 %v", res.Ran)
	}
	// 已完成的部分**必须留着** —— 这正是"降级交付 + 说明"
	if res.State.Versions("research") != 1 || res.State.Versions("outline") != 1 {
		t.Errorf("放弃不该丢掉已有产物：research=%d outline=%d",
			res.State.Versions("research"), res.State.Versions("outline"))
	}
}

// 没有中断时 Resume 要报明确的错，而不是静默地变成一次 Run。
func TestResumeWithoutPendingIsRejected(t *testing.T) {
	g := stdGraphRuns(t, nil)
	r, _ := newRunner(t, g, "run-1")

	if _, err := r.Resume(context.Background(), Decision{Intent: IntentConfirm}); err == nil {
		t.Fatal("没有待回答的中断时应当报错")
	} else if !strings.Contains(err.Error(), "没有在等待回答") {
		t.Errorf("错误信息应说清原因，实际: %v", err)
	}
}

// Resume 在**校验阶段**就要拦住不合法的回答，且**不改动磁盘上的状态**。
func TestResumeRejectsBadDecisionBeforeTouchingState(t *testing.T) {
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){
		"outline": interruptingOutline(t, nil),
	})
	r, _ := newRunner(t, g, "run-1")
	mustRun(t, r)

	before := r.mustLoadState(t)

	// revise 没带 Note —— 不可执行
	_, err := r.Resume(context.Background(), Decision{Intent: IntentRevise})
	if err == nil {
		t.Fatal("revise 缺 note 应当被拒绝")
	}
	// 意图乱写
	if _, err := r.Resume(context.Background(), Decision{Intent: "乱写的"}); err == nil {
		t.Fatal("非法意图应当被拒绝")
	}

	after := r.mustLoadState(t)
	if after.Pending == nil {
		t.Error("被拒绝的 Resume 不该清掉 Pending")
	}
	if after.Pending != nil && after.Pending.Decision != nil {
		t.Error("被拒绝的 Resume 不该把回答写进状态")
	}
	if after.Versions("outline") != before.Versions("outline") {
		t.Error("被拒绝的 Resume 不该改变黑板")
	}
}

// ─── 有界性 ─────────────────────────────────────────────────────────

// ⚠️ "判据可以不精确，但**绝不能无界**"的看门人。
//
// 这个节点永远要求"重做自己"（`Next == 自己`）—— 一个典型的无界判据。
// Runner 必须在有限次之后**停下来并如实报错**，而不是跑到天荒地老。
func TestRunnerBoundsSelfRetry(t *testing.T) {
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){
		"research": func(NodeInput) (*Outcome, error) {
			// 永远不满意，永远要求再来一次
			return &Outcome{Product: schema.Research{Topic: testTopic}, Next: "research"}, nil
		},
	})
	r, _ := newRunner(t, g, "run-1", func(o *RunnerOptions) { o.MaxRunsPerNode = 2 })

	_, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("无界的自我重做必须被拦下")
	}
	// 错误信息要**可自愈**：说清是"判据无界"，而不是只报"超限"
	if !strings.Contains(err.Error(), "无界") {
		t.Errorf("错误里应点出「无界」，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "research") {
		t.Errorf("错误里应点出是哪个节点，实际: %v", err)
	}
}

// 有界但**允许**那些合法的自我重做（上限之内不该误伤）。
func TestRunnerAllowsSelfRetryWithinBound(t *testing.T) {
	calls := 0
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){
		"research": func(NodeInput) (*Outcome, error) {
			calls++
			if calls < 2 {
				// 第一次不满意，要求再来一次
				return &Outcome{Product: schema.Research{Topic: testTopic}, Next: "research"}, nil
			}
			return &Outcome{Product: schema.Research{Topic: testTopic}}, nil
		},
	})
	r, _ := newRunner(t, g, "run-1")
	res := mustRun(t, r)

	if res.Status != StatusCompleted {
		t.Fatalf("上限之内的重试不该被打断，实际 %s", res.Status)
	}
	if got := res.State.Versions("research"); got != 2 {
		t.Errorf("应当产出 2 版 research，实际 %d 版", got)
	}
}

// ─── 装配与校验 ─────────────────────────────────────────────────────

func TestNewRunnerRejectsBadOptions(t *testing.T) {
	g := stdGraphRuns(t, nil)
	store, err := checkpoint.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		opt     RunnerOptions
		wantSub string
	}{
		{"没有图", RunnerOptions{Store: store, RunID: "r", Topic: "t"}, "需要一个图"},
		{"没有存储", RunnerOptions{Graph: g, RunID: "r", Topic: "t"}, "需要一个 checkpoint 存储"},
		{"run id 为空", RunnerOptions{Graph: g, Store: store, Topic: "t"}, "run id 不能为空"},
		{"主题为空", RunnerOptions{Graph: g, Store: store, RunID: "r"}, "主题不能为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewRunner(c.opt); err == nil {
				t.Fatal("应当被拒绝")
			} else if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误里应含 %q，实际: %v", c.wantSub, err)
			}
		})
	}
}

// 同一个 run id 配不同的主题必须被拒绝。
//
// 理由：那等于**两件事共用一个身份** —— 黑板上会混着两次不同输入产生的产物，
// 而血缘判据会认为它们是同一条链。
func TestRunnerRejectsDifferentTopicForSameRunID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ckpt")
	g := stdGraphRuns(t, nil)
	newRunnerOn(t, g, dir, "run-1").Run(context.Background())

	store, err := checkpoint.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(RunnerOptions{
		Graph: g, Store: store, RunID: "run-1", Topic: "换了个主题",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = r.Run(context.Background())
	if err == nil {
		t.Fatal("换了主题必须被拒绝")
	}
	if !strings.Contains(err.Error(), "共用一个身份") {
		t.Errorf("错误信息应说清后果，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "新增一版 topic") {
		t.Errorf("错误信息应给出**可执行的修法**，实际: %v", err)
	}
}

// 节点产出的 kind 与它声明的 kind 不一致时必须拒绝 —— 否则黑板会错位。
func TestRunnerRejectsMismatchedKind(t *testing.T) {
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){
		// 声明自己是 outline，却产出一份 research
		"outline": produce(schema.Research{Topic: "冒名顶替"}),
	})
	r, _ := newRunner(t, g, "run-1")

	_, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("kind 不一致必须被拒绝")
	}
	if !strings.Contains(err.Error(), "黑板会错位") {
		t.Errorf("错误信息应说清后果，实际: %v", err)
	}
}

// 节点想跳到一条不存在的边上 → 建图期定的规矩在这里生效。
func TestRunnerRejectsInvalidNext(t *testing.T) {
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){
		"research": func(NodeInput) (*Outcome, error) {
			// research 与 render 之间没有依赖边（render 不需要 research 的产出）
			return &Outcome{Product: schema.Research{Topic: testTopic}, Next: "render"}, nil
		},
	})
	r, _ := newRunner(t, g, "run-1")

	_, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("跳到没有边的节点必须被拒绝")
	}
	if !strings.Contains(err.Error(), "没有这条依赖边") {
		t.Errorf("错误信息应说清是边的问题，实际: %v", err)
	}
}

// ctx 被取消时必须立刻停，且**已完成的产物都在磁盘上**。
func TestRunnerHonorsContextCancellation(t *testing.T) {
	g := stdGraphRuns(t, nil)
	r, dir := newRunner(t, g, "run-1")

	ctx, cancel := context.WithCancel(context.Background())
	r.tracer = observ.TracerFunc(func(e observ.Event) {
		if e.Kind == observ.KindStart && e.Phase == "outline" {
			cancel()
		}
	})

	_, err := r.Run(ctx)
	if err == nil {
		t.Fatal("ctx 取消应当报错")
	}
	if !strings.Contains(err.Error(), "可以直接 Run 恢复") {
		t.Errorf("错误信息应告诉调用方怎么继续，实际: %v", err)
	}

	// 已完成的 research 必须已经落盘
	store, _ := checkpoint.NewFileStore(dir)
	raw, found, _ := store.Load("run-1")
	if !found {
		t.Fatal("应当已经落盘")
	}
	st, err := UnmarshalState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if st.Versions("research") != 1 {
		t.Errorf("被打断前的产物应当保留，实际 research=%d 版", st.Versions("research"))
	}
}

// ⚠️ 不变量：`Graph.Next` 选中的节点，它的 `Needs` **一定**都已就绪。
//
// 理由在拓扑序：上游没产出时它自己也"不可跳过"，而它排在前面。
// 这条性质让 `Runner.step` 不必处理"缺前置"的分支 ——
// 而**不变量写进测试才不会漂移**（写进注释的会）。
func TestNextAlwaysPicksANodeWhoseInputsAreReady(t *testing.T) {
	g := stdGraphRuns(t, nil)
	st := newTestState(t)

	// 从"什么都还没做"走到"全部做完"，每一步都验一遍
	for i := 0; i < 10; i++ {
		name := g.Next(st)
		if name == "" {
			break
		}
		node, _ := g.Node(name)
		for _, k := range node.Needs() {
			if _, ok := st.Latest(string(k)); !ok {
				t.Fatalf("第 %d 步选中了 %s，但它的前置 %q 还没就绪", i+1, name, k)
			}
		}
		// 模拟它产出一版
		a := art(string(node.Kind()), 1)
		for _, k := range node.Needs() {
			if latest, ok := st.Latest(string(k)); ok {
				a.BasedOn = append(a.BasedOn, schema.BasisOf(latest))
			}
		}
		put(st, a)
	}
}

// ─── 小工具 ─────────────────────────────────────────────────────────

func (r *Runner) mustLoadState(t *testing.T) *State {
	t.Helper()
	st, err := r.load()
	if err != nil {
		t.Fatalf("读取状态失败: %v", err)
	}
	return st
}

// ═══════════════════════════════════════════════════════════════════
// L3 ④ 改进第一批的回归测试
//
// 这一节里的每一条，对应的都是 07-L3复盘.md 里的一个**半成品**：
// 能力声明了、测了，但没有生产者或没有消费者。
//
// ⚠️ 它们的共同形状是**静默失败** —— 不会让人报错，只会让人误判。
//	所以每一条都必须有一个"看门人"。
// ═══════════════════════════════════════════════════════════════════

// ─── L3-1：Hint 换了必须被发现 ─────────────────────────────────────

// ⚠️ Hint 是节点的**输入** —— 换了它重启必须被拒绝（07-L3复盘.md L3-1）。
//
//	修之前：同一个 run 换 Hint → **静默接收新指令** →
//	产出的产物血缘上仍写着旧依据 → **血缘判据还认为这条链是新鲜的**。
//
//	这是"输入不进状态就会说谎"的**第三次**：L2-1 → `State.Topic` → `Hints`。
func TestRunnerRejectsChangedHints(t *testing.T) {
	g := stdGraphRuns(t, nil)
	dir := filepath.Join(t.TempDir(), "ckpt")
	hint := func(v string) func(*RunnerOptions) {
		return func(o *RunnerOptions) { o.Hints = map[string]string{"outline": v} }
	}

	mustRun(t, newRunnerOn(t, g, dir, "run-1", hint("控制在 3 页")))

	// 模拟重启：同一个 run id，**换了 hint**
	_, err := newRunnerOn(t, g, dir, "run-1", hint("不限页数")).Run(context.Background())
	if err == nil {
		t.Fatal("换了 Hint 重启应当被拒绝，却跑成功了")
	}
	if !strings.Contains(err.Error(), "hints") {
		t.Errorf("错误里应点出 hints，实际: %v", err)
	}
	// 错误信息要给**两条出路**（换 run id / 别换），并让它可 diff
	for _, want := range []string{"run id", "控制在 3 页", "不限页数"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应包含 %q，实际: %v", want, err)
		}
	}

	// ⚠️ 别把正常路径也挡了：**同样的 hint** 必须能恢复
	res, err := newRunnerOn(t, g, dir, "run-1", hint("控制在 3 页")).Run(context.Background())
	if err != nil {
		t.Fatalf("同样的 hint 应当能恢复，却报: %v", err)
	}
	if res.Status != StatusCompleted || len(res.Ran) != 0 {
		t.Errorf("应当直接判定为完成（产物已就绪），实际 %s / 跑了 %v", res.Status, res.Ran)
	}
}

// nil 与空 map 是**同一件事**（"这一轮没有额外要求"），不该被当成"变了"。
//
//	它们序列化后的表示不同（`omitempty` 让 nil 直接消失），
//	所以这条测试守的是**语义**，不是表示。
func TestRunnerTreatsNilAndEmptyHintsAlike(t *testing.T) {
	g := stdGraphRuns(t, nil)
	dir := filepath.Join(t.TempDir(), "ckpt")

	mustRun(t, newRunnerOn(t, g, dir, "run-1")) // Hints 留 nil
	res, err := newRunnerOn(t, g, dir, "run-1", func(o *RunnerOptions) {
		o.Hints = map[string]string{} // 换成空 map
	}).Run(context.Background())
	if err != nil {
		t.Fatalf("nil 与空 map 应当等价，却报: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Errorf("应当判定为完成，实际 %s", res.Status)
	}
}

// ⚠️ **v1 的记录没有 `hints` 字段** —— 那不是"这一轮没有 hint"，
//
//	而是"那时还没有这个概念"。
//
// # 这条测试是被一个**真实的 bug** 逼出来的
//
//	加上 hints 校验之后，磁盘上那份 v1 时代的 `l3-demo.json` 立刻被拒了，
//	理由是"记录的 hints 与本次调用不一致（记录 [无]，本次 [outline=…]）"——
//
//	**而真实原因是"记录太老"，不是"输入变了"。**
//	这是"报错了对象"的**第四次**（R29 / R30 / R34 / 本次）。
//
// 修法用的是**已有的机制**：兼容窗口。`min ≤ v ≤ current` 不只是"能读"，
// 还意味着"**结构兼容**" —— 所以只有窗口内的记录才适用本版本的结构规则。
func TestRunnerAcceptsOldSnapshotWithoutHints(t *testing.T) {
	g := stdGraphRuns(t, nil)
	dir := filepath.Join(t.TempDir(), "ckpt")

	mustRun(t, newRunnerOn(t, g, dir, "run-1", func(o *RunnerOptions) {
		o.Hints = map[string]string{"outline": "3 页"}
	}))

	// 把它"降级"成 v1 的记录：删掉 hints、版本改回 1
	path := filepath.Join(dir, "run-1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "hints")
	m["version"] = 1
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}

	// ⚠️ 故意传一个**完全不同的** hint：v1 记录**不该**被 v2 的规则判它 ——
	//	因为我们无从知道它之前用的是哪套（信息本来就不存在）。
	r2 := newRunnerOn(t, g, dir, "run-1", func(o *RunnerOptions) {
		o.Hints = map[string]string{"outline": "完全不同的要求"}
	})
	var notices []string
	r2.tracer = observ.TracerFunc(func(e observ.Event) {
		if e.Kind == observ.KindNotice {
			notices = append(notices, e.Detail)
		}
	})

	res, err := r2.Run(context.Background())
	if err != nil {
		t.Fatalf("v1 的旧快照不该被 hints 校验拦住，却报: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Errorf("应当直接从旧快照判定为完成，实际 %s", res.Status)
	}

	// ⚠️ 但放行**不许是静默的** —— 这是一个"不可避免的静默"（信息本来就不存在），
	//	它与"可以修掉的静默"不同：后者要修，前者**必须被看见**。
	if len(notices) == 0 {
		t.Fatal("放行了 v1 快照却没有发任何 notice —— 那用户永远不知道自己的输入没被校验")
	}
	if !strings.Contains(notices[0], "hints") {
		t.Errorf("notice 应说清跳过了什么校验，实际: %q", notices[0])
	}
}

// ─── R43：预算守卫必须**真的接在调度循环上** ────────────────────────

// openBudget 造一份"五维全不限"的额度（测试用；显式写清每一维）。
//
// ⚠️ 不能只写 `budget.Limits{Steps: 1}` —— 那读作"**时间上限 0**"，
//
//	于是**一步都跑不了**，测试会变成一条**空跑**。
func openBudget() budget.Limits {
	return budget.Limits{
		Steps:            budget.NoLimit,
		PromptTokens:     budget.NoLimit,
		CompletionTokens: budget.NoLimit,
		CostMilli:        budget.NoLimit,
		Elapsed:          budget.NoWall,
	}
}

// ⚠️ 一条"只测守卫"的测试**证明不了**它接上了 —— 所以这里走真实路径：
//
//	装配 Runner → 跑 → 被拦。
//
//	（这正是 R43 的出发点：`internal/budget` 写完之后**全仓没有一个导入者**，
//	 而它的测试全绿。**测试绿 ≠ 能力通。**）
func TestRunnerStopsWhenStepsBudgetExhausted(t *testing.T) {
	g := stdGraphRuns(t, nil)
	l := openBudget()
	l.Steps = 1
	r, dir := newRunner(t, g, "run-1", func(o *RunnerOptions) {
		o.Guard = budget.New(l)
	})

	// ⚠️⚠️ **这里断言的是 R52 改过的新行为** —— 而那条测试原本断言的是
	//	"返回一个 error"（R43 写的，而**那正是设计文档 §5 点名的错误写法**）。
	//
	//	> 触顶的行为**必须是"交付部分 + 说明"**，而不是 `return err`。
	//
	//	⭐ 它在改行为的当天**红了** —— 而那正是一条好测试的用途：
	//	  **在它断言的行为被改变时，红给你看。**
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("预算用完**不是错误**（那个上限是我们自己设的）—— 返回 err 会让调用方\n"+
			"把已经做好的产物一起丢掉。实际: %v", err)
	}

	if res.Status != StatusBudgetStopped {
		t.Errorf("状态应当是 `%s`（**不是** `%s`：那是「人说了不做了」），实际 %s",
			StatusBudgetStopped, StatusAborted, res.Status)
	}
	// ⚠️ "说明"必须说清是**哪一维**不够了 —— 否则人只知道"停了"
	if !strings.Contains(res.StopReason, string(budget.DimSteps)) {
		t.Errorf("`StopReason` 里应当说清超的是哪一维（%q），实际：%s",
			budget.DimSteps, res.StopReason)
	}

	// ★★★ 而**这一条才是修复的意义**：**产物没丢**。
	//
	//	"降级交付"的全部内容就是这一句 —— 而它必须被**断言**，
	//	否则它只是一句写在注释里的承诺。
	if res.State == nil {
		t.Fatal("状态不能是 nil —— 它装着**已经做好的产物**")
	}
	if got := len(res.State.Board[string(schema.KindResearch)]); got == 0 {
		t.Error("第一个节点的产物（research）应当在 `State` 里 —— " +
			"「交付部分结果」的意思是**那部分真的还在**，而不只是" + "「非零退出码」")
	}
	if len(res.Ran) != 1 {
		t.Errorf("应当只跑了 1 个节点，实际 %v", res.Ran)
	}

	// ⚠️ **落盘这一条不在这里断言** —— 而理由是诚实的：
	//
	//	`checkpoint.FileStore.Load` 的签名我不确定（它返回多个值，
	//	而**猜一个签名**正是这个项目一路在防的事）。
	//
	//	⭐ 而它**已经被两处覆盖**，其中一处比这里更硬：
	//
	//		`internal/checkpoint` 的测试   查"写进去能读回来"
	//		`cmd/l3 -verify`              **跑两遍 + 真实中断恢复，逐字节比**
	//
	//	> 一条**猜出来的**断言，比没有断言更坏：它会在签名恰好对上时
	//	> **静默地什么都不验证**（R48 那条"能红的测试"的同一个担心）。
	//
	//	⚠️ 而"预算停下时也会 persist"这件事**确实需要一条断言** ——
	//	登记为待办，触发条件是"下一次动 `persist` 的调用点"。
	_ = dir
}

// ⚠️⚠️ 这一条守的是**最容易静默失败**的那一点：**执行时间真的被报进来了**。
//
// # 为什么单测不够
//
//	一个"永远报 `Elapsed: 0`"的 Runner **不会报任何错** ——
//	它只会让**时间维形同虚设**：
//
//		`Allow` 的边界是 `>`（不是 `>=`），所以"上限 X + 本步耗时 0"
//		**永远放行**。于是无论 `WithWall` 配多小，一步都不会被时间拦住。
//
//	> 这与 L3-3（`SubAgentNode` 吞掉 `StatsReporter`）是**同一个形状**：
//	> **失败的方向是"静默"的** —— 它看起来像"这一层没有用量"，
//	> 而不是"我没把能力接出来"。
//
//	所以这里断言的不是"时间超了"，而是"**守卫真的收到了时间**"。
func TestRunnerReportsElapsedToGuard(t *testing.T) {
	// ⚠️ 这里**必须真的睡一下** —— 而那不是"为了让测试好过"，是在对齐一个物理事实。
	//
	// # 这条测试第一次是**失败着**暴露自己的
	//
	//	最初它用的是默认节点（毫秒级就跑完），断言 `spent.Elapsed > 0`。
	//	它**通过了好几次** —— 然后在一次全量验收里红了。
	//
	//	原因：**Windows 的 `time.Now()` 粒度约 0.5~15ms**，
	//	所以一个几微秒的节点算出来**正好是 0**。
	//
	// # ⭐ 而这次 flake 是 R43 那句断言的**回归**，值得记清楚
	//
	//	R43 我写的是："假时钟没了 —— 因为守卫不再看时钟，
	//	所以 R23 那个'拿时间当判据会 flake'的问题在这里**消失了**。"
	//
	//	**它没有消失。** 它只是**从守卫搬到了 `Runner`**（`time.Since(startedAt)`）：
	//
	//		以前：守卫读时钟  → 测试要**注入**一个假时钟
	//		现在：Runner 读时钟 → 测试要**断言一个真实测量值 > 0**
	//
	//	> ### 而"谁读时钟"不是重点 —— 重点是**这条链路里有真实时钟**。
	//	> 凡是被测的链路上有时钟，测试就要替它的**粒度**负责。
	//
	//	⭐ 所以修法不是把断言放松成 `>= 0`（那这条测试就什么都没守），
	//	  而是**让被测量的那个量真的可测**：睡 3ms，远大于时钟粒度。
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){
		"outline": func(NodeInput) (*Outcome, error) {
			time.Sleep(3 * time.Millisecond)
			return &Outcome{Product: schema.Outline{}}, nil
		},
	})
	guard := budget.New(openBudget())
	r, _ := newRunner(t, g, "run-1", func(o *RunnerOptions) { o.Guard = guard })

	mustRun(t, r)

	spent := guard.Snapshot().Spent
	if spent.Steps == 0 {
		t.Fatal("守卫一步都没记 —— Runner 根本没调 ReportUse")
	}
	if spent.Elapsed <= 0 {
		t.Errorf("执行时间是 0 —— Runner 报了 `Elapsed: 0`，"+
			"而那会让时间维**静默地**形同虚设（实际 Spent = %s）", spent)
	}
}

// ⚠️ 没配 Guard 时，Runner **不能**崩，也不能"悄悄什么都不做"——
//
//	它该拿一个**显式的** `Unlimited`（与 `Observer` 同一个手法：不判空）。
func TestRunnerDefaultsToUnlimitedGuard(t *testing.T) {
	g := stdGraphRuns(t, nil)
	r, _ := newRunner(t, g, "run-1") // 不传 Guard

	res := mustRun(t, r) // 不 panic、不被拦
	if res.Status != StatusCompleted {
		t.Errorf("没配预算时应当照常跑完，实际 %s", res.Status)
	}
	if r.guard == nil {
		t.Fatal("Guard 被归一成了 nil —— 循环里那一行会崩")
	}
	if _, ok := r.guard.(*budget.Unlimited); !ok {
		t.Errorf("没配预算时应当归一成 *budget.Unlimited（**显式的对象**，不是 nil），实际 %T", r.guard)
	}
}

// ─── R42 差异审计：节点的 panic 必须被翻译成 error ──────────────────

// ⚠️ **一个节点的 panic 不该杀掉整个进程。**
//
// # 这条是"与参考实现对比"照出来的
//
//	企业仓库在每个**边界**上都 recover（`grqueue` 协程 / HTTP middleware /
//	外部 API 调用），共同点是：**显式的 `crash` 字段 + stack + 不静默**。
//
//	而我们在 R42 之前**全仓 `recover()` = 0 处** —— 一个节点的 panic
//	会让用户看到一屏栈，而不是"第 3 步失败了，产物都在，可以重试"。
func TestNodePanicBecomesError(t *testing.T) {
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){
		"outline": func(NodeInput) (*Outcome, error) { panic("节点里炸了") },
	})
	r, dir := newRunner(t, g, "run-1")

	_, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("panic 必须被翻译成一个 error，而不是让进程崩掉")
	}
	// 错误必须**可诊断**：哪个节点 + 什么原因 + 栈
	for _, want := range []string{"outline", "节点里炸了", "panic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应包含 %q，实际: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "goroutine") {
		t.Error("错误里应当带栈 —— panic 的全部价值就是那个栈，丢了它只剩一句「出错了」")
	}

	// ⚠️ 最要紧的一条：**checkpoint 仍然自洽**。
	//
	//	panic 发生在节点**内部**，而状态写入全部由 Runner 控制
	//	（节点只能读黑板，产出走 `Outcome`）—— 所以下一次 Run 必须能接着跑，
	//	而不是读到一个半截状态。**这才是"翻译成 error"真正的意义。**
	good := stdGraphRuns(t, nil)
	res2, err2 := newRunnerOn(t, good, dir, "run-1").Run(context.Background())
	if err2 != nil {
		t.Fatalf("panic 之后应当还能接着跑，却报: %v", err2)
	}
	if res2.Status != StatusCompleted {
		t.Errorf("应当能跑完，实际 %s", res2.Status)
	}
}

// ─── L3-2：中断必须有生产出来的有效期 ───────────────────────────────

// interrupting 让 outline 产出之后停下来等人确认（与真实 HITL 同形状）。
func interrupting(in NodeInput) (*Outcome, error) {
	it := in.Interrupt("outline", KindConfirmArtifact, "确认大纲？")
	return &Outcome{Product: schema.Outline{}, Interrupt: &it}, nil
}

// ⚠️ 在此之前**没有任何生产代码写过** `ExpiresAt`，
//
//	于是 `CheckResumable` 里那条过期检查**永远不触发**（07-L3复盘.md L3-2）。
//
//	修法不是在节点里加一行 —— 那样每个节点都要记得加（R23 那条教训）；
//	而是**唯一的生产点在 Runner**（`RunnerOptions.InterruptTTL`）。
func TestRunnerStampsInterruptExpiry(t *testing.T) {
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){"outline": interrupting})
	r, dir := newRunner(t, g, "run-1", func(o *RunnerOptions) { o.InterruptTTL = time.Hour })

	res := mustRun(t, r)
	if res.Status != StatusInterrupted {
		t.Fatalf("应当中断，实际 %s", res.Status)
	}
	if res.Interrupted.ExpiresAt.IsZero() {
		t.Fatal("中断没有被写上有效期 —— 那条过期检查永远不会触发")
	}
	if d := time.Until(res.Interrupted.ExpiresAt); d > time.Hour || d < 30*time.Minute {
		t.Errorf("有效期落点不对：距现在 %v", d)
	}

	// ⚠️ 它必须**跟着 checkpoint 走** —— 否则"进程重启之后有效期就丢了"，
	//    而那正是"状态"与"运行期变量"的区别。
	st := newRunnerOn(t, g, dir, "run-1",
		func(o *RunnerOptions) { o.InterruptTTL = time.Hour }).mustLoadState(t)
	if st.Pending == nil || st.Pending.ExpiresAt.IsZero() {
		t.Fatal("有效期没有落盘 —— 重启之后过期检查又失效了")
	}
}

// 负数 = **显式的"永不过期"**：一个需要被写在装配参数里的决定，
// 而不是"忘了设"的默认结果（`Interrupt.ExpiresAt` 的零值恰好就是"不过期" ✅）。
func TestRunnerNegativeTTLMeansNeverExpires(t *testing.T) {
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){"outline": interrupting})
	r, _ := newRunner(t, g, "run-1", func(o *RunnerOptions) { o.InterruptTTL = -1 })

	res := mustRun(t, r)
	if res.Status != StatusInterrupted {
		t.Fatalf("应当中断，实际 %s", res.Status)
	}
	if !res.Interrupted.ExpiresAt.IsZero() {
		t.Errorf("显式的「永不过期」应当留零值，实际 %v", res.Interrupted.ExpiresAt)
	}
}

// 默认值必须**真的默认上** —— 否则所有没显式配 TTL 的调用方（比如 cmd/l3）
// 都会退化成"永不过期"，而那正是修之前的状态。
func TestRunnerDefaultsInterruptTTL(t *testing.T) {
	r, _ := newRunner(t, stdGraphRuns(t, nil), "run-1")
	if r.interruptTTL != DefaultInterruptTTL {
		t.Errorf("默认 TTL 应当是 %v，实际 %v", DefaultInterruptTTL, r.interruptTTL)
	}
}

// ─── L3-12：观测**不许说反话** ──────────────────────────────────────

// ⚠️ "已就绪、跳过：outline" 然后 outline 就跑了 ——
//
//	这是 R34 实跑才发现的（07-L3复盘.md L3-12）。
//
// # 为什么单测没抓到
//
//	`observ.KindSkipped` 的 **Detail 内容**从来没有被断言过 ——
//	而它出错时**不会让任务失败**，只会**主动误导排查的人**。
//	"误导"比"缺失"贵：缺失会让人去找，误导会让人找错方向。
func TestSkippedNeverListsForcedNode(t *testing.T) {
	g := stdGraphRuns(t, map[string]func(NodeInput) (*Outcome, error){"outline": interrupting})
	r, dir := newRunner(t, g, "run-1")
	mustRun(t, r)

	// 模拟重启 + revise → outline 会被**强制重做**
	r2 := newRunnerOn(t, g, dir, "run-1")
	var skipped []string
	r2.tracer = observ.TracerFunc(func(e observ.Event) {
		if e.Kind == observ.KindSkipped {
			skipped = append(skipped, e.Detail)
		}
	})

	res, err := r2.Resume(context.Background(), Decision{Intent: IntentRevise, Note: "换一版"})
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}

	// ① 先确保这条测试**没有在空跑**：没捕获到事件的话它什么都没验证
	if len(skipped) == 0 {
		t.Fatal("没有捕获到任何「跳过」事件 —— 这条测试没在验证什么")
	}
	// ② 真正要守的：跳过清单里不许出现被强制重做的那一跳
	for _, d := range skipped {
		if strings.Contains(d, "outline") {
			t.Errorf("「跳过」清单里出现了 outline，但它这一轮**被强制重做**了 —— 观测在说反话: %q", d)
		}
	}
	// ③ 顺带确认重做真的发生了（否则 ② 可能是"什么都没跑"换来的）
	if len(res.Ran) == 0 || res.Ran[0] != "outline" {
		t.Errorf("revise 之后第一跳应当是 outline，实际 %v", res.Ran)
	}
}
