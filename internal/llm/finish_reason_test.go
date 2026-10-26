package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// oaiServer 起一个假的 OpenAI 服务，返回给定的响应体。
//
// ⚠️ 它**走真实 HTTP 路径**（`httptest` + 真 `http.Client`）——
// 而不是"给 `Response` 结构体赋一个值"。
//
//	⭐ 因为这一轮修的正是"**解析了却没传出来**"（`oaiResponse.FinishReason`
//	  早就在，而 `Response` 里没有）—— 一条"直接构造 `Response`"的测试
//	  **永远发现不了它**（它测的是结构体，不是那条路径）。
func oaiServer(t *testing.T, body string) *OpenAIClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return NewOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "test", Model: "m"})
}

// ⚠️⚠️ 这条守着"**`finish_reason` 被传出来了**"（R59）。
//
//	在修之前：`oaiResponse` 解析了它（`openai.go:238`），而 `Response` 里没有那个字段
//	→ 于是"**输出被截断**"这个失败类别**永远看不见**：
//
//		`length`          → 正文是不完整的 JSON → 下游报"参数不是合法 JSON"
//		                   而**真正的原因是"模型说到一半被截断了"**
//		`content_filter`  → 空响应，而下游会说"模型没回答"
//
//	> ### 一个被丢掉的 `finish_reason`，会让"截断"伪装成"模型不会写"。
//
// ⭐ 而它也是"失败分类"里那一格的**唯一生产者** —— 没有它，
//   `docs/` 里那张失败分类表就少一行。
func TestFinishReasonIsPropagated(t *testing.T) {
	cases := []struct{ wire, want string }{
		{`{"choices":[{"message":{"content":"好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, FinishStop},
		{`{"choices":[{"message":{"content":"半截"},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, FinishLength},
		{`{"choices":[{"message":{"content":""},"finish_reason":"content_filter"}],"usage":{}}`, FinishFiltered},
		// ⚠️ 而**服务商加新值**时不该报错 —— 原样透传（它不是枚举）
		{`{"choices":[{"message":{"content":"x"},"finish_reason":"brand_new_reason"}],"usage":{}}`, "brand_new_reason"},
	}

	for _, c := range cases {
		resp, err := oaiServer(t, c.wire).Generate(context.Background(), Request{})
		if err != nil {
			t.Errorf("%s：不该失败: %v", c.want, err)
			continue
		}
		if resp.FinishReason != c.want {
			t.Errorf("`finish_reason` 没被透传：期望 %q，实际 %q —— "+
				"而它丢了之后，「输出截断」会被误报成「模型不会写」", c.want, resp.FinishReason)
		}
	}
}

// ⚠️ 顺带守一条**相邻的、同样静默**的事：`usage` 缺字段时**不该 panic** 也不该编造。
func TestUsageZeroWhenMissing(t *testing.T) {
	resp, err := oaiServer(t, `{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}]}`).Generate(
		context.Background(), Request{})
	if err != nil {
		t.Fatalf("`usage` 缺失不该让调用失败: %v", err)
	}
	if resp.Usage.PromptTokens != 0 || resp.Usage.CompletionTokens != 0 {
		t.Errorf("没有 `usage` 时应当是 0（**不编造**），实际 %+v", resp.Usage)
	}
}

// ⚠️ 而**错误路径**也要有一条：服务端返回 `error` 字段时，必须报出来而不是静默返回空。
//
//	⭐ 那与"截断伪装成不会写"是同一族：**一条不该安静的地方安静了**。
func TestServiceErrorIsReported(t *testing.T) {
	_, err := oaiServer(t, `{"error":{"message":"rate limit exceeded"}}`).Generate(
		context.Background(), Request{})
	if err == nil {
		t.Fatal("服务端返回 error 时必须报错 —— 静默返回空串会让上游以为模型没话说")
	}
}
