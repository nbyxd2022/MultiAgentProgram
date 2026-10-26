package agents

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"multiagent/internal/schema"
	"multiagent/internal/state"
)

// 这个文件守住 **子 Agent 级失败熔断**。
//
// 它是 L2-9（接真实模型）逼出来的：
//
//	outline 因为超时每一步都失败，编排者**换了 10 种措辞反复重试它**，
//	直到 MaxSteps 强制终止 —— 根因一次没变，成本却涨了十几倍。
//
// 熔断要守住的不是"别失败"，而是"**别把同一个失败重复十遍**"。

// flakyAgent 是一个"按剧本决定成败"的替身。
type flakyAgent struct {
	kind    schema.Kind
	outcome []bool // true = 成功；用完后一律成功
	calls   int
}

func (f *flakyAgent) Name() string         { return "flaky-" + string(f.kind) }
func (f *flakyAgent) Kind() schema.Kind    { return f.kind }
func (f *flakyAgent) Description() string  { return "替身：按剧本成败" }
func (f *flakyAgent) Needs() []schema.Kind { return nil }

func (f *flakyAgent) Run(_ context.Context, _ SubAgentInput) (schema.Payload, error) {
	i := f.calls
	f.calls++
	if i < len(f.outcome) && !f.outcome[i] {
		return nil, fmt.Errorf("模型服务返回 HTTP 500")
	}
	return schema.RenderResult{Files: []string{"out/a.html"}}, nil
}

// TestBreakerStopsRepeatedFailures 是核心用例。
func TestBreakerStopsRepeatedFailures(t *testing.T) {
	board := state.NewBlackboard()
	sub := &flakyAgent{
		kind:    schema.KindRender,
		outcome: []bool{false, false, false, false, false, false},
	}
	tl := toolFor(t, board, sub)
	ctx := context.Background()

	// 前 N 次：把原始错误如实回灌给模型（它有机会自己纠正）
	for i := 1; i <= maxConsecutiveFailures; i++ {
		_, err := tl.Execute(ctx, "{}")
		if err == nil {
			t.Fatalf("第 %d 次应当失败", i)
		}
		if strings.Contains(err.Error(), "熔断") {
			t.Fatalf("第 %d 次还不该熔断（上限是 %d）", i, maxConsecutiveFailures)
		}
		// "已经第几次了"必须告诉模型 —— 它自己数不出来（上下文每轮会重置），
		// 而"第 3 次"和"第一次"应当导致完全不同的决策。
		if !strings.Contains(err.Error(), "连续第 "+strconv.Itoa(i)+" 次") {
			t.Errorf("应当告诉模型这是第几次失败：%v", err)
		}
	}

	// 之后：熔断，而且**不再真正执行**（省下的就是钱）
	ranBefore := sub.calls
	_, err := tl.Execute(ctx, "{}")
	if err == nil {
		t.Fatal("达到上限之后必须熔断")
	}
	if !strings.Contains(err.Error(), "熔断") {
		t.Errorf("错误信息应当说明已被熔断：%v", err)
	}
	if sub.calls != ranBefore {
		t.Errorf("熔断后不该再真正执行子 Agent，实际多执行了 %d 次", sub.calls-ranBefore)
	}
	// 拒绝必须**可执行**（L1-5）：要给出出路，而不是只说"不行"
	for _, want := range []string{"换一条路", "直接结束"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("熔断信息应当给出出路 %q：%v", want, err)
		}
	}
}

// TestBreakerResetsAfterSuccess 守住"它是**连续**计数，不是累计计数"。
//
// 没有这条，一个"失败两次 → 成功 → 再失败两次"的健康流程也会被误熔断 ——
// 而"间歇性失败"恰恰是网络抖动最常见的形态，**它就该被允许继续**。
func TestBreakerResetsAfterSuccess(t *testing.T) {
	board := state.NewBlackboard()
	// 第 1、2 次失败 → 第 3 次成功（**计数清零**）→ 第 4、5 次再失败
	//
	// 如果计数是"累计"而不是"连续"，第 4 次就会撞上上限而被熔断 ——
	// 这条用例正是用来区分这两种语义的。
	sub := &flakyAgent{
		kind:    schema.KindRender,
		outcome: []bool{false, false, true, false, false, false},
	}
	tl := toolFor(t, board, sub)
	ctx := context.Background()

	wantFail := []bool{true, true, false, true, true}
	for i, want := range wantFail {
		_, err := tl.Execute(ctx, "{}")
		switch {
		case want && err == nil:
			t.Fatalf("第 %d 次按剧本应当失败", i+1)
		case !want && err != nil:
			t.Fatalf("第 %d 次按剧本应当成功：%v", i+1, err)
		}
		if err != nil && strings.Contains(err.Error(), "熔断") {
			t.Fatalf("第 %d 次不该熔断（成功过一次就应当清零）：%v", i+1, err)
		}
	}
	if sub.calls != 5 {
		t.Errorf("5 次都应当真正执行，实际 %d 次", sub.calls)
	}
}

// TestBreakerIsPerRole 守住"熔断是按角色算的"。
//
// 一个角色坏掉不该把别的角色一起停掉 ——
// 编排者正是靠"换一条路"自救的；熔断如果是全局的，那就连路都没了。
func TestBreakerIsPerRole(t *testing.T) {
	board := state.NewBlackboard()
	ctx := context.Background()

	badTool := toolFor(t, board, &flakyAgent{
		kind:    schema.KindDesign,
		outcome: []bool{false, false, false, false},
	})
	for i := 0; i < maxConsecutiveFailures+1; i++ {
		_, _ = badTool.Execute(ctx, "{}")
	}
	if _, err := badTool.Execute(ctx, "{}"); err == nil || !strings.Contains(err.Error(), "熔断") {
		t.Fatalf("坏角色应当已经熔断：%v", err)
	}

	// 另一个角色**不受影响**
	if _, err := toolFor(t, board, &flakyAgent{kind: schema.KindRender}).
		Execute(ctx, "{}"); err != nil {
		t.Errorf("别的角色不该被牵连：%v", err)
	}
}
