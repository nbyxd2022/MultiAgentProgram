package agents

import (
	"context"
	"strings"
	"testing"

	"multiagent/internal/llm"
	"multiagent/internal/schema"
	"multiagent/internal/state"
)

// 这个文件复现并守住 **L2-2**：
//
//	"产物存在" ≠ "产物完整"
//
// renderer 明确支持"部分成功"（`RenderResult.Failed`），
// 但编排者的完成判断只看"黑板上有这份产物"（`Versions(key) != 0`）。
// 于是 3 页里失败 1 页，系统依然会宣布"必需产物齐全"。
//
// 它与 L1-3（"没检查" ≠ "没问题"）是同构的：
// **两者都是用一个更弱的判据，去代替一个更强的判据。**

// shortRenderAgent 是一个"只会产出一份**残缺** render 产物"的替身。
//
// 用它而不是真渲染器，是因为要**确定性地**制造"部分成功" ——
// 真渲染器只在磁盘出错时才会失败，测不了。
type shortRenderAgent struct{ calls int }

func (s *shortRenderAgent) Name() string         { return "renderer" }
func (s *shortRenderAgent) Kind() schema.Kind    { return schema.KindRender }
func (s *shortRenderAgent) Description() string  { return "替身：产出残缺的渲染结果" }
func (s *shortRenderAgent) Needs() []schema.Kind { return nil }

func (s *shortRenderAgent) Run(_ context.Context, _ SubAgentInput) (schema.Payload, error) {
	s.calls++
	return schema.RenderResult{
		Files: []string{"out/a.html", "out/b.html"},
		Failed: []schema.PageFailure{
			{PageIndex: 2, Reason: "写盘失败: 磁盘已满"},
		},
	}, nil
}

func TestSupervisorDetectsIncompleteProduct(t *testing.T) {
	board := state.NewBlackboard()
	sub := &shortRenderAgent{}

	// 三轮脚本：每轮「派一次活 + 收尾」
	client := llm.NewScriptedClient("sup", "完成。",
		callStep("render", "{}"), textStep("完成。"),
		callStep("render", "{}"), textStep("完成。"),
		callStep("render", "{}"), textStep("完成。"),
	)

	sup, err := NewSupervisor(SupervisorOptions{
		Client:    client,
		Board:     board,
		SubAgents: []SubAgent{sub},
		Required:  []schema.Kind{schema.KindRender},
	})
	if err != nil {
		t.Fatalf("构造编排者失败: %v", err)
	}

	result, err := sup.Run(context.Background(), "主题")
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	// ① 产物**确实存在**（作品入库了）
	if board.Versions(string(schema.KindRender)) == 0 {
		t.Fatal("产物应当已经入库")
	}

	// ② 旧判据（只看 `Missing`）在这个场景下**必然放行** —— 这就是 L2-2 本体。
	//
	//    把它显式断言出来有个好处：**bug 的形状被固定在这条测试里了**。
	//    以后如果谁把 `Missing` 和 `Incomplete` 又合并回去，这里会立刻提醒他
	//    "两者不是一回事"。
	if len(result.Missing) != 0 {
		t.Fatalf("本场景里没有「从未产出」的产物，旧判据必然说完成；"+
			"若 Missing 非空说明场景被改坏了：%v", result.Missing)
	}

	// ③ 新判据必须拦住它 —— 这是 L2-2 的修复本体
	if result.Complete() {
		t.Error("产物存在但有缺口（第 3 页写盘失败），不能算「必需产物齐全」")
	}

	// ④ 缺口必须被**说清楚**，否则调用方只能猜
	joined := strings.Join(result.Incomplete, " | ")
	if !strings.Contains(joined, "render") || !strings.Contains(joined, "第 3 页") {
		t.Errorf("必须报告「哪个产物、缺了什么」，实际 %q", joined)
	}

	// ⑤ 编排者应当**继续尝试**（上限内），而不是第 1 轮就收工
	if sub.calls < 2 {
		t.Errorf("发现缺口后应当继续调度，实际只调用了 %d 次", sub.calls)
	}
}

// TestSupervisorTreatsFullProductAsComplete 是上面那个用例的**反向约束**。
//
// 没有它，一个"永远报不完整"的实现在上面那个用例里也是绿的 ——
// 那条测试只证明了"能发现缺口"，没证明"没缺口时不误报"。
func TestSupervisorTreatsFullProductAsComplete(t *testing.T) {
	board := state.NewBlackboard()
	sub := &fullRenderAgent{}

	client := llm.NewScriptedClient("sup", "完成。",
		callStep("render", "{}"), textStep("完成。"),
	)

	sup, err := NewSupervisor(SupervisorOptions{
		Client:    client,
		Board:     board,
		SubAgents: []SubAgent{sub},
		Required:  []schema.Kind{schema.KindRender},
	})
	if err != nil {
		t.Fatalf("构造编排者失败: %v", err)
	}

	result, err := sup.Run(context.Background(), "主题")
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if !result.Complete() {
		t.Errorf("产物完整时不该报不完整：Missing=%v Incomplete=%v",
			result.Missing, result.Incomplete)
	}
	if sub.calls != 1 {
		t.Errorf("产物完整时不该重复调度，实际调用了 %d 次", sub.calls)
	}
}

// fullRenderAgent 产出**完整**的渲染结果（反向约束用）。
type fullRenderAgent struct{ calls int }

func (f *fullRenderAgent) Name() string         { return "renderer" }
func (f *fullRenderAgent) Kind() schema.Kind    { return schema.KindRender }
func (f *fullRenderAgent) Description() string  { return "替身：产出完整渲染结果" }
func (f *fullRenderAgent) Needs() []schema.Kind { return nil }

func (f *fullRenderAgent) Run(_ context.Context, _ SubAgentInput) (schema.Payload, error) {
	f.calls++
	return schema.RenderResult{Files: []string{"out/a.html", "out/b.html", "out/c.html"}}, nil
}
