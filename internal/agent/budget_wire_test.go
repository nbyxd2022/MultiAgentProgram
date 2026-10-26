package agent

import (
	"context"
	"testing"

	"multiagent/internal/budget"
	"multiagent/internal/retry"
)

// openLimits 是"五维全不限"的一份额度（显式写清每一维 —— 见 R43 那条注释）。
func openLimits() budget.Limits {
	return budget.Limits{
		Steps:            budget.NoLimit,
		PromptTokens:     budget.NoLimit,
		CompletionTokens: budget.NoLimit,
		CostMilli:        budget.NoLimit,
		Elapsed:          budget.NoWall,
	}
}

// ⚠️⚠️ 这条守着"**token 真的被报进守卫了**" —— 而它是 R52 那个注入抓出来的洞。
//
//	在修之前：`Runner.ReportUse` 只报 `Steps` + `Elapsed`，而 **token 没有任何生产点**
//	（`agent` 层看得见用量，却没有 `Guard`）→ `spent.PromptTokens` **恒为 0** →
//	于是"token 预算"这一维**永远不触发**（`L4_HUGE_TOKENS` 就是这样被抓到的）。
//
//	> ### 它不只是"拦不住这一次"，是**"连记都没有"**。
//
// ⭐ 而它必须走**真实路径**（配 Guard → 跑 → 查 `Snapshot`），不能只测 `ReportUse`：
//
//	一条"只测守卫"的测试**证明不了它接上了**（R43 的教训：`budget` 满绿的测试 + 零导入者）。
func TestAgentReportsTokensToGuard(t *testing.T) {
	c := &flakyClient{} // 第一次就成功
	g := budget.New(openLimits())

	a := New(Options{
		Name: "worker", SystemPrompt: "sys", Client: c,
		Guard: g,
	})
	if _, err := a.Run(context.Background(), "做个 PPT"); err != nil {
		t.Fatalf("应当成功: %v", err)
	}

	spent := g.Snapshot().Spent
	// 前置：`flakyClient` 每次报 100 prompt + 10 completion
	if spent.PromptTokens == 0 {
		t.Fatalf("守卫一个 token 都没收到 —— `Agent` 根本没调 `ReportUse`。"+
			"而这意味着**token 预算这一维永远不会触发**（实际 Spent = %s）", spent)
	}
	if spent.PromptTokens != 100 || spent.CompletionTokens != 10 {
		t.Errorf("token 报错了：期望 100 / 10，实际 %d / %d", spent.PromptTokens, spent.CompletionTokens)
	}
}

// ⚠️ 而**失败的调用报不出 token** —— 那是因为 `llm.Client` 失败时返回 `(nil, err)`，
//
//	**错误里没有用量**（R49 登记的缺口）。
//
//	⭐ 而这条测试把那个缺口的**形状**钉下来：**一次重试成功 = 只有 1 次的 token 进账**。
//	  哪天 `Client` 的契约改了（错误能带用量），这个数会变 —— 而**那次改动会被这里抓住**。
func TestOnlySuccessfulCallsReportTokens(t *testing.T) {
	c := &flakyClient{fails: 2} // 前两次失败，第三次成功
	g := budget.New(openLimits())

	a := New(Options{
		Name: "worker", SystemPrompt: "sys", Client: c,
		Guard: g, Retry: retry.Policy{Attempts: 3},
	})
	if _, err := a.Run(context.Background(), "做个 PPT"); err != nil {
		t.Fatalf("应当成功: %v", err)
	}

	if c.calls != 3 {
		t.Fatalf("前置不成立：应当调了 3 次，实际 %d", c.calls)
	}
	// ⚠️ 只有**成功的那一次**有 `Usage` → 只能报一次的量
	if got := g.Snapshot().Spent.PromptTokens; got != 100 {
		t.Errorf("当前契约下只有成功那次能报 token（100）；实际 %d。\n"+
			"⚠️ 若你刚改了 `Client` 让失败也能带用量，这个期望该改成 300 —— "+
			"而那正是这条断言在做的事：**把缺口的大小钉成一个数字**", got)
	}
}

// 零值 `Guard` 必须**不 panic**、且归一成 `Unlimited`（与 `Tracer` 同一个手法）。
func TestZeroGuardIsUnlimited(t *testing.T) {
	c := &flakyClient{}
	a := New(Options{Name: "worker", SystemPrompt: "sys", Client: c}) // 不传 Guard

	if a.guard == nil {
		t.Fatal("`guard` 被归一成了 nil —— `ReportUse` 会崩")
	}
	if _, ok := a.guard.(*budget.Unlimited); !ok {
		t.Errorf("没配预算时应当归一成 `*budget.Unlimited`（**显式的对象**，不是 nil），实际 %T", a.guard)
	}
	if _, err := a.Run(context.Background(), "做个 PPT"); err != nil {
		t.Fatalf("没配预算时应当照常跑完: %v", err)
	}
}
