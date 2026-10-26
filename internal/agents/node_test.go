package agents

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"multiagent/internal/agent"
	"multiagent/internal/checkpoint"
	"multiagent/internal/graph"
	"multiagent/internal/schema"
	"multiagent/internal/state"
)

// ─── 脚手架 ─────────────────────────────────────────────────────────

// fakeSub 是最小的 SubAgent：行为由闭包决定。
//
// 它**不做**"按调用次数返回不同剧本"（R23 的教训：那种替身把状态藏在替身里）。
type fakeSub struct {
	name  string
	kind  schema.Kind
	desc  string
	needs []schema.Kind
	run   func(in SubAgentInput) (schema.Payload, error)
}

func (f *fakeSub) Name() string         { return f.name }
func (f *fakeSub) Kind() schema.Kind    { return f.kind }
func (f *fakeSub) Description() string  { return f.desc }
func (f *fakeSub) Needs() []schema.Kind { return f.needs }
func (f *fakeSub) Run(_ context.Context, in SubAgentInput) (schema.Payload, error) {
	return f.run(in)
}

func produce(p schema.Payload) func(SubAgentInput) (schema.Payload, error) {
	return func(SubAgentInput) (schema.Payload, error) { return p, nil }
}

func newFakeSub(name string, kind schema.Kind, needs []schema.Kind,
	run func(SubAgentInput) (schema.Payload, error)) *fakeSub {
	return &fakeSub{name: name, kind: kind, desc: name + "（说明）", needs: needs, run: run}
}

// nodeInput 造一个 NodeInput。
//
// ⚠️ 只能用到**导出的**部分 —— `board` 未导出，所以测试也**写不了黑板**
// （那正是 R33 想要的：类型上做不到，而不是"约定不要做"）。
// 需要取用能力时用 `graph.NodeInput.Upstream()`，但那要 Runner 来装配。
func nodeInput(runID string, attempt int) graph.NodeInput {
	return graph.NodeInput{RunID: runID, Topic: "主题", Attempt: attempt}
}

// ─── 可选能力必须**穿透适配器** ─────────────────────────────────────

// statsSub 是一个**会报用量**的 SubAgent（模拟研究 / 大纲 / 设计三个真实角色）。
type statsSub struct {
	*fakeSub
	st agent.Stats
}

func (s *statsSub) Stats() agent.Stats { return s.st }

// ⚠️ 适配器**必须转发**可选能力（07-L3复盘.md L3-3）。
//
// # 这是一个"让漏掉会被测试抓到"的看门人
//
//	`SubAgentNode` 当初没有转发 `StatsReporter`，于是调用方拿到的
//	一堆 `graph.Node` 断言**全部失败** —— 而失败是**静默**的：
//	它看起来像"这一层没有用量"，而不是"我没把能力接出来"。
//
//	后果不是"少打印一个数"：**L2 花一整轮统一的口径（必须是"累计"）就此作废**，
//	而 L4 的预算守卫会因此**失去数据来源**。
//
// 修法不是"下次注意"，是这一条测试。
func TestSubAgentNodeForwardsStatsReporter(t *testing.T) {
	inner := &statsSub{
		fakeSub: newFakeSub("outline", schema.KindOutline, nil, produce(schema.Outline{})),
		st:      agent.Stats{PromptTokens: 120, CompletionTokens: 30},
	}
	n, err := NewSubAgentNode(SubAgentNodeOptions{Sub: inner})
	if err != nil {
		t.Fatal(err)
	}

	r, ok := any(n).(StatsReporter)
	if !ok {
		t.Fatal("SubAgentNode 必须实现 StatsReporter —— 否则图驱动下的用量会**静默消失**")
	}
	if got := r.Stats(); got.PromptTokens != 120 || got.CompletionTokens != 30 {
		t.Errorf("用量没有转发：%+v", got)
	}
}

// 内层**不报用量**时，"问得出来"仍然要成立（返回零值）。
//
// ⚠️ 这是"**能问**"与"**有数**"的区别：
//
//	调用方的 `s.(agents.StatsReporter)` 不该因为"某个角色恰好不调模型"而失败 ——
//	那样每个调用方都要写两种分支（`cmd/l2` 的遍历就是这么写的）。
func TestSubAgentNodeReportsZeroWhenInnerSilent(t *testing.T) {
	sub := newFakeSub("render", schema.KindRender, nil, produce(schema.RenderResult{}))
	n, err := NewSubAgentNode(SubAgentNodeOptions{Sub: sub})
	if err != nil {
		t.Fatal(err)
	}

	r, ok := any(n).(StatsReporter)
	if !ok {
		t.Fatal("即使内层不报用量，SubAgentNode 也必须满足断言")
	}
	if got := r.Stats(); got != (agent.Stats{}) {
		t.Errorf("内层不报用量时应为零值，实际 %+v", got)
	}
}

// ─── 身份直通 ───────────────────────────────────────────────────────

func TestSubAgentNodeDelegatesIdentity(t *testing.T) {
	sub := newFakeSub("outline", schema.KindOutline, []schema.Kind{schema.KindResearch},
		produce(schema.Outline{}))
	n, err := NewSubAgentNode(SubAgentNodeOptions{Sub: sub})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}

	if n.Name() != "outline" || n.Kind() != schema.KindOutline {
		t.Errorf("身份没有直通：%s / %s", n.Name(), n.Kind())
	}
	if got := n.Needs(); len(got) != 1 || got[0] != schema.KindResearch {
		t.Errorf("依赖没有直通：%v", got)
	}
	if !strings.Contains(n.Description(), "说明") {
		t.Errorf("说明没有直通：%q", n.Description())
	}
}

// 配了 confirm 时，说明里要**看得出来**（图拓扑打印时用得上）。
func TestSubAgentNodeDescriptionMentionsConfirm(t *testing.T) {
	sub := newFakeSub("outline", schema.KindOutline, nil, produce(schema.Outline{}))
	n, err := NewSubAgentNode(SubAgentNodeOptions{Sub: sub, Confirm: "确认大纲？"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(n.Description(), "等人确认") {
		t.Errorf("说明里应体现它会中断：%q", n.Description())
	}
}

func TestNewSubAgentNodeRejects(t *testing.T) {
	ok := newFakeSub("outline", schema.KindOutline, nil, produce(schema.Outline{}))

	cases := []struct {
		name    string
		opt     SubAgentNodeOptions
		wantSub string
	}{
		{"Sub 为 nil", SubAgentNodeOptions{}, "SubAgent 不能为空"},
		{
			"kind 不合法",
			SubAgentNodeOptions{Sub: newFakeSub("x", "乱写的", nil, produce(schema.Outline{}))},
			"kind",
		},
		{
			"中断类型不在目录里",
			SubAgentNodeOptions{Sub: ok, Confirm: "问一句", ConfirmKind: "乱写的"},
			"中断类型",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewSubAgentNode(c.opt); err == nil {
				t.Fatal("应当被拒绝")
			} else if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误里应含 %q，实际: %v", c.wantSub, err)
			}
		})
	}
}

// ─── 产出与中断 ─────────────────────────────────────────────────────

func TestSubAgentNodeWrapsProductIntoOutcome(t *testing.T) {
	sub := newFakeSub("outline", schema.KindOutline, nil, produce(schema.Outline{}))
	n, _ := NewSubAgentNode(SubAgentNodeOptions{Sub: sub})

	out, err := n.Run(context.Background(), nodeInput("run-1", 1))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if out == nil || out.Product == nil {
		t.Fatal("产出必须被包进 Outcome.Product（Runner 靠它入库）")
	}
	if out.Interrupt != nil {
		t.Error("没配 confirm 时不该中断")
	}
	if out.Next != "" {
		t.Error("适配器不该替节点决定下一步（图会自己算）")
	}
}

// ⚠️ 这条锁住的是一个**不变量**（整个 confirm 语义都建立在它上面）：
//
//	中断时**产物已经在 `Outcome.Product` 里**。
//
// 因为 `confirm` 之后的"自然继续"靠的是"产物已入库 → `CanSkip` 跳过它"。
// 如果适配器只发中断不交产物，那么 confirm 之后这一跳会**又被执行一遍** ——
// 而它会**再中断一次**，于是形成"跨进程的无限问答"。
func TestSubAgentNodeConfirmEmitsProductAndInterruptTogether(t *testing.T) {
	sub := newFakeSub("outline", schema.KindOutline, nil, produce(schema.Outline{}))
	n, _ := NewSubAgentNode(SubAgentNodeOptions{Sub: sub, Confirm: "请确认这份大纲"})

	out, err := n.Run(context.Background(), nodeInput("run-1", 1))
	if err != nil {
		t.Fatal(err)
	}
	if out.Product == nil {
		t.Fatal("中断的同时必须交出产物 —— 否则 confirm 之后会无限重复提问")
	}
	if out.Interrupt == nil {
		t.Fatal("配了 confirm 就该中断")
	}
	if out.Interrupt.Prompt != "请确认这份大纲" {
		t.Errorf("问题文案不对：%q", out.Interrupt.Prompt)
	}
	if out.Interrupt.Node != "outline" {
		t.Errorf("中断要记住是哪一跳发的，实际 %q", out.Interrupt.Node)
	}
	if out.Interrupt.Kind != graph.KindConfirmArtifact {
		t.Errorf("默认中断类型应为 confirm_artifact，实际 %q", out.Interrupt.Kind)
	}
}

// 中断 ID 必须**确定性**：同一跳、同一个 attempt 得到同一个 ID。
//
// （这条性质真正的用处是"恢复时能把人的回答对回来"，见 R29/R31。
//
//	适配器只是把它接通 —— 但接错了就全盘失效，所以要在这儿守着。）
func TestSubAgentNodeInterruptIDIsDeterministic(t *testing.T) {
	sub := newFakeSub("outline", schema.KindOutline, nil, produce(schema.Outline{}))
	n, _ := NewSubAgentNode(SubAgentNodeOptions{Sub: sub, Confirm: "确认？"})

	a, _ := n.Run(context.Background(), nodeInput("run-1", 1))
	b, _ := n.Run(context.Background(), nodeInput("run-1", 1))
	if a.Interrupt.ID != b.Interrupt.ID {
		t.Errorf("同状态必须得到同一个 ID：%q vs %q", a.Interrupt.ID, b.Interrupt.ID)
	}

	c, _ := n.Run(context.Background(), nodeInput("run-1", 2))
	if c.Interrupt.ID == a.Interrupt.ID {
		t.Error("不同 attempt 必须得到不同 ID（否则上次的答案会被套到新问题上）")
	}
}

// ─── 人的回答 → Hint ────────────────────────────────────────────────

// ⚠️ 这条是 `revise` 语义能不能成立的关键：
// **人说的话必须被送到节点手里**，否则它只能用同样的输入产出同样的东西。
func TestSubAgentNodeTranslatesAnswerIntoHint(t *testing.T) {
	var seen []string
	sub := newFakeSub("outline", schema.KindOutline, nil,
		func(in SubAgentInput) (schema.Payload, error) {
			seen = append(seen, in.Hint)
			return schema.Outline{}, nil
		})
	n, _ := NewSubAgentNode(SubAgentNodeOptions{Sub: sub})

	// 第一次：没有人的回答
	in := nodeInput("run-1", 1)
	in.Hint = "不超过 3 页"
	if _, err := n.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}

	// 第二次：人要求改
	in2 := nodeInput("run-1", 1)
	in2.Hint = "不超过 3 页"
	in2.Answer = &graph.Decision{Intent: graph.IntentRevise, Note: "第 3 页换成建议"}
	if _, err := n.Run(context.Background(), in2); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 2 {
		t.Fatalf("应当被调用两次，实际 %d 次", len(seen))
	}
	if seen[0] != "不超过 3 页" {
		t.Errorf("没有回答时 Hint 应原样保留，实际 %q", seen[0])
	}
	if !strings.Contains(seen[1], "不超过 3 页") {
		t.Errorf("原有的 Hint 不该被丢掉，实际 %q", seen[1])
	}
	// ⚠️ 人的原话要**原样带上**（不复述 —— 复述一次就多一个写错的机会）
	if !strings.Contains(seen[1], "第 3 页换成建议") {
		t.Errorf("人的要求必须原样到达节点，实际 %q", seen[1])
	}
	if !strings.Contains(seen[1], "【人的要求】") {
		t.Errorf("应当有个标记让它一眼可辨，实际 %q", seen[1])
	}
}

// 回答里没有 Note（比如 confirm）时不该往 Hint 里塞空标记。
func TestSubAgentNodeAnswerWithoutNoteLeavesHintAlone(t *testing.T) {
	var seen string
	sub := newFakeSub("outline", schema.KindOutline, nil,
		func(in SubAgentInput) (schema.Payload, error) {
			seen = in.Hint
			return schema.Outline{}, nil
		})
	n, _ := NewSubAgentNode(SubAgentNodeOptions{Sub: sub})

	in := nodeInput("run-1", 1)
	in.Hint = "不超过 3 页"
	in.Answer = &graph.Decision{Intent: graph.IntentConfirm}
	if _, err := n.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if seen != "不超过 3 页" {
		t.Errorf("没有内容时不该改动 Hint，实际 %q", seen)
	}
}

func TestSubAgentNodePropagatesError(t *testing.T) {
	boom := errors.New("上游炸了")
	sub := newFakeSub("outline", schema.KindOutline, nil,
		func(SubAgentInput) (schema.Payload, error) { return nil, boom })
	n, _ := NewSubAgentNode(SubAgentNodeOptions{Sub: sub})

	_, err := n.Run(context.Background(), nodeInput("run-1", 1))
	if !errors.Is(err, boom) {
		t.Errorf("错误必须原样上抛（Runner 要按它决定退不退），实际: %v", err)
	}
}

// 适配器**拿不到**黑板（只有 `Upstream()` 那个窄接口）。
//
// 这条是"把能力从类型上拿掉"的看门人：编译期它就成立，
// 而注释里那句"请不要写黑板"**永远不会漂移**，因为它无从写起。
func TestSubAgentNodeOnlyGetsReadAccess(t *testing.T) {
	n, _ := NewSubAgentNode(SubAgentNodeOptions{
		Sub: newFakeSub("outline", schema.KindOutline, nil, produce(schema.Outline{})),
	})

	var seen schema.Reader
	sub, _ := n.sub.(*fakeSub)
	sub.run = func(in SubAgentInput) (schema.Payload, error) {
		seen = in.Upstream
		return schema.Outline{}, nil
	}
	if _, err := n.Run(context.Background(), nodeInput("run-1", 1)); err != nil {
		t.Fatal(err)
	}
	// 没有 Runner 装配时 Upstream 是 nil —— 而它的**静态类型**只有 Get，没有 Put。
	// 这一行能编译，就说明"写"这件事不在能力范围内。
	_ = seen
}

// ─── 端到端：适配器在真实 Graph + Runner 里跑 ────────────────────────

// 四个节点全部由适配器包成 Node，用**真实的 Runner**跑一遍：
//
//	① 到 outline 中断（confirm 模式）
//	② revise 恢复 → outline 重做且拿到人的要求
//	③ 下游跟着重做，最终跑完
//
// 这条测试是"适配器接对了"的最强证据 —— 它不测适配器的某个方法，
// 而是让整条链路（图 + 中断 + 恢复 + 适配器）一起动起来。
func TestSubAgentNodeWorksUnderTheRealRunner(t *testing.T) {
	var outlineHints []string

	subs := []SubAgent{
		newFakeSub("research", schema.KindResearch, []schema.Kind{schema.KindTopic},
			produce(schema.Research{Topic: "主题"})),
		newFakeSub("outline", schema.KindOutline, []schema.Kind{schema.KindResearch},
			func(in SubAgentInput) (schema.Payload, error) {
				outlineHints = append(outlineHints, in.Hint)
				return schema.Outline{}, nil
			}),
		newFakeSub("design", schema.KindDesign, []schema.Kind{schema.KindOutline},
			produce(schema.Design{})),
		newFakeSub("render", schema.KindRender,
			[]schema.Kind{schema.KindOutline, schema.KindDesign},
			produce(schema.RenderResult{Files: []string{"a.html"}})),
	}
	// ⚠️ 这一行值得多看一眼：4 个 SubAgent 的 Needs 是 L2 就写好的，
	//    而 **L3 的图是从它推导出来的** —— 图里没有一条边是手写的。
	nodes := make([]graph.Node, 0, len(subs))
	for i, s := range subs {
		opt := SubAgentNodeOptions{Sub: s}
		if s.Name() == "outline" {
			opt.Confirm = fmt.Sprintf("请确认第 %d 步的产物", i+1)
		}
		n, err := NewSubAgentNode(opt)
		if err != nil {
			t.Fatalf("包装 %s 失败: %v", s.Name(), err)
		}
		nodes = append(nodes, n)
	}

	g, err := graph.BuildGraph(graph.GraphSpec{
		Nodes:   nodes,
		Presets: []schema.Kind{schema.KindTopic},
	})
	if err != nil {
		t.Fatalf("建图失败: %v", err)
	}
	// 顺带确认：边确实是从 Needs 推出来的（4 条，一条都没手写）
	if got := len(g.Edges()); got != 4 {
		t.Errorf("应当推出 4 条边，实际 %d 条：%v", got, g.Edges())
	}

	store, err := checkpoint.NewFileStore(filepath.Join(t.TempDir(), "ckpt"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := graph.NewRunner(graph.RunnerOptions{
		Graph: g, Store: store, RunID: "run-1", Topic: "主题",
	})
	if err != nil {
		t.Fatal(err)
	}

	// ① 跑到中断
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if res.Status != graph.StatusInterrupted {
		t.Fatalf("应当在 outline 停下等人，实际 %s（跑了 %v）", res.Status, res.Ran)
	}
	if res.Interrupted.Node != "outline" {
		t.Fatalf("中断节点不对：%q", res.Interrupted.Node)
	}

	// ② revise 恢复
	//
	// ⚠️ 它**会再中断一次** —— 而这正是 HITL 该有的样子：
	//	"我改好了，你再看看"。revise 之后给出的是**新版本**，
	//	而"给人看新版本、再问一次"是确认类中断的完整语义。
	//
	//	（写这条测试时我第一版期望的是"直接跑完"，跑出来才发现想错了。
	//	  这不是 bug —— 是"确认"这件事本来就该在每次产出之后发生。）
	res2, err := r.Resume(context.Background(), graph.Decision{
		Intent: graph.IntentRevise, Note: "第 3 页换成建议",
	})
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if res2.Status != graph.StatusInterrupted {
		t.Fatalf("改完之后应当**再问一次**，实际 %s（%v）", res2.Status, res2.Ran)
	}
	// 而且这是一个**新问题**，不是上次那个 —— 靠 attempt 区分：
	// outline 已经产出了一版，于是 `AttemptFor` 从 1 变成了 2。
	if res2.Interrupted.ID == res.Interrupted.ID {
		t.Errorf("重做之后必须是一个新的中断 ID（上次的答案不该被套到新问题上）：%q", res2.Interrupted.ID)
	}

	// ③ outline 被调了两次，第二次拿到了人的要求
	if len(outlineHints) != 2 {
		t.Fatalf("outline 应当被调用两次，实际 %d 次：%v", len(outlineHints), outlineHints)
	}
	if strings.Contains(outlineHints[0], "人的要求") {
		t.Errorf("第一次不该有人的要求，实际 %q", outlineHints[0])
	}
	if !strings.Contains(outlineHints[1], "第 3 页换成建议") {
		t.Errorf("重做时必须带上人的要求，实际 %q", outlineHints[1])
	}

	// ④ 确认之后跑完
	res3, err := r.Resume(context.Background(), graph.Decision{Intent: graph.IntentConfirm})
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if res3.Status != graph.StatusCompleted {
		t.Fatalf("确认之后应当跑完，实际 %s", res3.Status)
	}
	// outline 不该被第三次执行（它已经有产物了）
	if len(outlineHints) != 2 {
		t.Errorf("confirm 不该让 outline 再做一遍，实际被调用 %d 次", len(outlineHints))
	}

	// ⑤ 版本账要算清楚
	//
	// ⚠️ design/render 是 **1 版**，不是 2 —— 因为**它们从来没跑过**：
	//	中断停在 outline，前两轮都没有机会走到它们。
	//
	//	（同一个坑我在这条测试里踩了两次：第一次以为 revise 会直接跑完，
	//	  第二次以为下游会"跟着重做"。**两次都是对"此刻系统处于什么状态"想当然。**）
	//
	//	"下游跟着重做"要先把下游**做完**才能观察 ——
	//	那个场景由 `graph.TestRevisePullsDownstreamBackIn` 覆盖，两者分工明确。
	if got := res3.State.Versions("outline"); got != 2 {
		t.Errorf("outline 应当 2 版（被 revise 重做），实际 %d", got)
	}
	for _, k := range []string{"design", "render"} {
		if got := res3.State.Versions(k); got != 1 {
			t.Errorf("%s 应当是**首次执行**（1 版），实际 %d", k, got)
		}
	}
	if got := res3.State.Versions("research"); got != 1 {
		t.Errorf("research 不该被牵连（1 版），实际 %d", got)
	}
}

// 顺带确认 L2 的 SubAgent 真的能用窄接口取全文（不是只在测试替身上成立）。
func TestRealSubAgentFetchesThroughNarrowReader(t *testing.T) {
	b := state.NewBlackboard()
	a, err := schema.NewArtifact("research", "research", schema.Research{Topic: "主题"})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := b.Put(a)
	if err != nil {
		t.Fatal(err)
	}

	// 只把 b 作为 `schema.Reader` 交出去（拿不到 Put）
	var r schema.Reader = b
	in := SubAgentInput{
		Inputs:   []schema.Handoff{schema.HandoffOf(stored)},
		Upstream: r,
	}
	got, err := in.Fetch()
	if err != nil {
		t.Fatalf("按窄接口取全文失败: %v", err)
	}
	if len(got) != 1 || got[0].Key != "research" {
		t.Errorf("取回的内容不对：%+v", got)
	}
}

// 没有取用能力时，Fetch 要报一句能看懂的错（而不是 nil 解引用）。
func TestFetchWithoutUpstreamIsAClearError(t *testing.T) {
	in := SubAgentInput{Inputs: []schema.Handoff{{Ref: "research@1", Summary: "x"}}}
	if _, err := in.Fetch(); err == nil {
		t.Fatal("没有取用能力时应当报错")
	} else if !strings.Contains(err.Error(), "上游读取能力") {
		t.Errorf("错误信息应说清缺什么，实际: %v", err)
	}
}
