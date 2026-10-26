package agents

import (
	"context"
	"strings"
	"testing"

	"multiagent/internal/llm"
	"multiagent/internal/schema"
	"multiagent/internal/state"
)

// 这个文件守住 **L2-1 的另一半**：编排者的**完成判断**也必须看新鲜度。
//
// 为什么"另一半"必须单独守？
//
//	`subagent.go` 里的新鲜度检查只在下发时生效 —— 它防止**做**出过期的东西。
//	但"不派活"也是一种交付：如果上游被重做、而下游**没人再派**，
//	编排者会直接宣布完成，交付一份**基于旧上游**的产物。
//
//	这种情况没有任何异常、也没有缺口 —— 只有一个过时的产物。
//	只拦下发是拦不住它的。

// seedStaleDeck 造出"上下游齐全，但上游刚刚被重做"的局面。
//
// 返回黑板，以及最新一版 outline 的版本号（用来断言）。
func seedStaleDeck(t *testing.T, board *state.Blackboard) {
	t.Helper()

	outline := func() *stubAgent { return &stubAgent{name: "outline", kind: schema.KindOutline} }
	designer := func() *stubAgent {
		return &stubAgent{
			name: "designer", kind: schema.KindDesign,
			needs: []schema.Kind{schema.KindOutline},
		}
	}
	renderer := func() *stubAgent {
		return &stubAgent{
			name: "renderer", kind: schema.KindRender,
			needs: []schema.Kind{schema.KindOutline, schema.KindDesign},
		}
	}

	// 第 1 轮：全套产物都齐了（此时它们是自洽的）
	runOnce(t, board, outline())
	runOnce(t, board, designer())
	runOnce(t, board, renderer())

	// 上游被重做 —— 下游**没有任何人再派一次**
	runOnce(t, board, outline())
}

func TestSupervisorRefusesToCompleteWithStaleProduct(t *testing.T) {
	board := state.NewBlackboard()
	seedStaleDeck(t, board)

	// 前置断言：三份产物**都在**，而且都完整
	if board.Versions("render") != 1 {
		t.Fatalf("render 应当已经产出过：%d 版", board.Versions("render"))
	}
	if board.Versions("outline") != 2 {
		t.Fatalf("outline 应当有 2 版：%d", board.Versions("outline"))
	}

	renderer := &stubAgent{
		name: "renderer", kind: schema.KindRender,
		needs: []schema.Kind{schema.KindOutline, schema.KindDesign},
	}
	client := llm.NewScriptedClient("sup", "完成。",
		callStep("render", "{}"), textStep("完成。"),
		callStep("render", "{}"), textStep("完成。"),
		callStep("render", "{}"), textStep("完成。"),
	)
	sup, err := NewSupervisor(SupervisorOptions{
		Client:    client,
		Board:     board,
		SubAgents: []SubAgent{renderer},
		Required:  []schema.Kind{schema.KindRender},
	})
	if err != nil {
		t.Fatalf("构造编排者失败: %v", err)
	}

	result, err := sup.Run(context.Background(), "主题")
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	// ★ 核心断言：产物**存在且完整**，但依据过期了 —— 绝不能算完成
	if result.Complete() {
		t.Error("render@1 是基于旧的 outline 做出来的，编排者不该宣布完成")
	}
	if len(result.Missing) != 0 || len(result.Incomplete) != 0 {
		t.Errorf("这个场景里没有「缺失」也没有「缺口」，问题只在过期：Missing=%v Incomplete=%v",
			result.Missing, result.Incomplete)
	}
	if len(result.Stale) == 0 {
		t.Fatal("必须报告依据过期")
	}

	// 顺序必须是**可执行的修复顺序**：design 在前（它是 render 的上游）
	if !strings.HasPrefix(result.Stale[0], "design：") {
		t.Errorf("第一条应当是 design（上游优先），实际 %q", result.Stale[0])
	}
	// 行首的 key 要能让编排层**自动照做**（它就是派活用的工具名）
	if !strings.Contains(strings.Join(result.Stale, " | "), "render：") {
		t.Errorf("也应当报出 render 自己：%v", result.Stale)
	}
}

// TestSupervisorCompletesWhenAllFresh 是反向约束：
// 上游没有再动过时，绝不该误报过期。没有它，一个"永远报过期"的实现也是绿的。
func TestSupervisorCompletesWhenAllFresh(t *testing.T) {
	board := state.NewBlackboard()

	runOnce(t, board, &stubAgent{name: "outline", kind: schema.KindOutline})
	runOnce(t, board, &stubAgent{
		name: "designer", kind: schema.KindDesign,
		needs: []schema.Kind{schema.KindOutline},
	})
	runOnce(t, board, &stubAgent{
		name: "renderer", kind: schema.KindRender,
		needs: []schema.Kind{schema.KindOutline, schema.KindDesign},
	})

	renderer := &stubAgent{
		name: "renderer", kind: schema.KindRender,
		needs: []schema.Kind{schema.KindOutline, schema.KindDesign},
	}
	client := llm.NewScriptedClient("sup", "完成。",
		callStep("render", "{}"), textStep("完成。"),
	)
	sup, err := NewSupervisor(SupervisorOptions{
		Client:    client,
		Board:     board,
		SubAgents: []SubAgent{renderer},
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
		t.Errorf("一切新鲜、齐全、完整时应当完成：Missing=%v Incomplete=%v Stale=%v",
			result.Missing, result.Incomplete, result.Stale)
	}
}

// TestStaleReminderIsActionable 守住"提醒能直接照做"。
//
// 这条测验的是**反馈的可执行性**（L1-5 在编排层的延伸）：
// 提醒里必须同时给出「哪个角色」「为什么」「顺序」，
// 否则模型只能猜 —— 而猜错的代价是白烧一轮。
func TestStaleReminderIsActionable(t *testing.T) {
	board := state.NewBlackboard()
	seedStaleDeck(t, board)

	renderer := &stubAgent{
		name: "renderer", kind: schema.KindRender,
		needs: []schema.Kind{schema.KindOutline, schema.KindDesign},
	}
	client := llm.NewScriptedClient("sup", "完成。")
	sup, err := NewSupervisor(SupervisorOptions{
		Client:    client,
		Board:     board,
		SubAgents: []SubAgent{renderer},
		Required:  []schema.Kind{schema.KindRender},
	})
	if err != nil {
		t.Fatalf("构造编排者失败: %v", err)
	}

	missing, incomplete, stale := sup.gaps()
	prompt := reminderPrompt("主题", missing, incomplete, stale)

	for _, want := range []string{
		"依据已经过期",  // 说清是哪一类问题
		"design：", // 能直接照做的角色名（行首 key 就是工具名）
		"按上面的顺序",  // 顺序本身就是执行计划
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("提醒里应当出现 %q：\n%s", want, prompt)
		}
	}
}
