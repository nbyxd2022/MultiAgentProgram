package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"multiagent/internal/llm"
	"multiagent/internal/observ"
	"multiagent/internal/retry"
)

// throttle 是一个**自己声明"我能被重试"**的错误 —— 见 `errclass.RetryableError`。
//
// ⚠️ 用它而不是 `errors.New("限流")`，是因为**文本不是证据**：
//
//	`errclass` 判一个**不认识**的错时兜底是 `Permanent` —— 于是
//	`errors.New("限流")` **不会被重试**（那是对的，只是名字里恰好有"限流"两个字）。
//
//	> 这正是 `errclass` 要接口而不是抠字符串的理由：**"叫什么"和"是什么"是两件事。**
type throttle struct{}

func (throttle) Error() string   { return "限流了" }
func (throttle) Retryable() bool { return true }

// flakyClient 前 `fails` 次失败，之后成功。
//
// ⚠️ 它**每次都报用量**（即使失败的那次）—— 因为真实的模型调用**失败也会计费**
//
//	（R43 的实测：连续失败 10 次烧掉 55321 tokens）。
//
//	⭐ 而这里我让它**在失败时也返回用量** —— 于是下面那条"重试几次就有几条明细"
//	  的断言才有东西可测。⚠️ 而我们**真正的** `llm.Client` 契约
//	  目前**做不到这件事**（错误里没有用量，见 `agent.go` 里登记的待办）。
type flakyClient struct {
	fails int
	calls int
}

func (c *flakyClient) Generate(context.Context, llm.Request) (*llm.Response, error) {
	c.calls++
	if c.calls <= c.fails {
		return nil, throttle{}
	}
	return &llm.Response{
		Content: "完成",
		Usage:   llm.Usage{PromptTokens: 100, CompletionTokens: 10},
	}, nil
}

func newRetryAgent(t *testing.T, c llm.Client, p retry.Policy) *Agent {
	t.Helper()
	return New(Options{Name: "worker", SystemPrompt: "sys", Client: c, Retry: p})
}

// ⚠️⚠️ 这条守着"**接线真的生效了**"—— 把 `retry.Do` 从 `agent.go` 摘掉，它立刻红。
//
//	R42 立的判据是"能不能写出一条会失败的测试"，而这条**能**：
//	今天之前（没有重试）`Generate` 第一次就返回错误，整个角色直接失败。
func TestAgentRetriesModelCall(t *testing.T) {
	c := &flakyClient{fails: 2}
	a := newRetryAgent(t, c, retry.Policy{Attempts: 3})

	out, err := a.Run(context.Background(), "做个 PPT")
	if err != nil {
		t.Fatalf("前两次失败、第三次成功 —— 整个角色应当成功: %v", err)
	}
	if out == "" {
		t.Error("应当拿到模型的最终答案")
	}
	if c.calls != 3 {
		t.Errorf("应当调 3 次模型，实际 %d 次", c.calls)
	}
}

// ⚠️⚠️ 而**这一条比上面那条更要紧**：重试**花的钱必须记在账上**。
//
//	R47 那条是"**总量只能有一份明细**" —— 而它的推论是：
//	**明细少一条，总量就少一笔**。
//
//	所以发事件的位置不能是"循环体里发一次"，必须是 **`fn` 里面每次发一次** ——
//	否则重试的调用**在账上根本不存在**（而它们**确实花了钱**）。
func TestRetriedCallsAllAppearInTheLedger(t *testing.T) {
	c := &flakyClient{fails: 2}
	rec := observ.NewRecorder()
	a := New(Options{
		Name: "worker", SystemPrompt: "sys", Client: c,
		Retry:  retry.Policy{Attempts: 3},
		Tracer: rec,
	})

	if _, err := a.Run(context.Background(), "做个 PPT"); err != nil {
		t.Fatalf("应当成功: %v", err)
	}

	// ⚠️⚠️ **先确认"真的调了 3 次"** —— 否则下面那条计数是**恒真**的
	//	（一个从来没重试过的实现，明细也是 1 条）。
	//	⭐ 这是 R48 那条"把『能红』内建进测试"的做法：
	//	  **别让一个断言恰好被一个错误的实现满足。**
	if c.calls != 3 {
		t.Fatalf("前置条件不成立：应当调了 3 次，实际 %d 次", c.calls)
	}

	var calls int
	for _, e := range rec.Events() {
		if e.Kind == observ.KindLLMCall {
			calls++
		}
	}

	// ⚠️ 期望是 **1** 条（只有成功的那次），而**这不是"够用了"，是一个缺口**：
	//
	//	`llm.Client.Generate` 失败时返回 `(nil, err)` —— **错误里没有用量**。
	//	而一次失败的调用**可能已经产生了费用**（R43 的实测：
	//	连续失败 10 次烧掉 55321 tokens，最后仍然失败 —— 那笔钱一分没少花）。
	//
	//	> ### 所以"重试的花费在账上"这件事，**今天只做到了一半**。
	//	> 要补齐它得动 `Client` 的契约（**让错误能带用量**），
	//	> 那是一个真实的设计需求，而**它得先被记下来**（见 `agent.go` 里的待办）。
	//
	//	⭐ 而这条断言的价值**恰恰在这里**：它把"缺口有多大"写成了**一个数字**。
	//	  哪天 `Client` 改了契约，这个 `1` 会变成 `3` —— 而**那次改动会立刻被这里抓住**。
	if calls != 1 {
		t.Errorf("当前契约下应当有 **1 条**明细（只有成功的调用有 `Usage`），实际 %d 条。"+
			"⚠️ 若你刚改了 `Client` 让它能带用量，那这个期望该从 1 改成 3 —— "+
			"而那正是这条断言在做的事：**把缺口的大小钉成一个数字**", calls)
	}

	// 用量同理：只有成功那次算得进来
	if got := a.Stats().PromptTokens; got != 100 {
		t.Errorf("当前契约下累计应当是 100（只有成功那次），实际 %d", got)
	}
}

// ⚠️ 而**不值得重试**的错必须**只试一次** —— 否则"参数非法"会变成三倍的账单。
func TestAgentDoesNotRetryPermanentErrors(t *testing.T) {
	c := &permanentClient{}
	a := newRetryAgent(t, c, retry.Policy{Attempts: 5})

	if _, err := a.Run(context.Background(), "做个 PPT"); err == nil {
		t.Fatal("永久失败应当冒出来")
	}
	if c.calls != 1 {
		t.Errorf("不值得重试的错应当**只试一次**，实际 %d 次", c.calls)
	}
}

// permanentClient 永远返回一个**不可重试**的错（缺来源的断言之类）。
type permanentClient struct{ calls int }

func (c *permanentClient) Generate(context.Context, llm.Request) (*llm.Response, error) {
	c.calls++
	return nil, errors.New("参数非法")
}

// ⚠️ 零值 `Retry`（没配）必须**只试一次**、且**不 panic** ——
//
//	"没配"读作"不重试"（重试会花钱，默认不该偷偷花）。
func TestZeroRetryPolicyTriesOnce(t *testing.T) {
	c := &flakyClient{fails: 1}
	a := newRetryAgent(t, c, retry.Policy{}) // 零值

	if _, err := a.Run(context.Background(), "做个 PPT"); err == nil {
		t.Error("没配重试时，第一次失败就该冒出来")
	}
	if c.calls != 1 {
		t.Errorf("零值策略应当只试一次，实际 %d 次", c.calls)
	}
}

// 顺带：退避真的被等过（用极短时长，避免 flake —— R23）。
func TestRetryBackoffIsWaited(t *testing.T) {
	c := &flakyClient{fails: 2}
	const each = 20 * time.Millisecond
	a := newRetryAgent(t, c, retry.Policy{Attempts: 3, Backoff: retry.Fixed(each)})

	start := time.Now()
	if _, err := a.Run(context.Background(), "做个 PPT"); err != nil {
		t.Fatalf("应当成功: %v", err)
	}
	if took := time.Since(start); took < 2*each {
		t.Errorf("两次重试应当等够 2×%v，实际 %v —— 退避没生效（限流场景就靠它）", each, took)
	}
}
