// Package llm 定义模型调用的抽象层。
//
// 设计要点（端口-适配器 / Ports & Adapters）：
//   - Client 是"端口"：上层（Agent、图编排）只依赖它，不知道背后是 Mock 还是真实模型；
//   - mock.go / openai.go 是"适配器"：各自实现 Client；
//   - 这样切换模型不需要改动任何业务代码，也让离线测试成为可能。
package llm

import (
	"context"
	"time"

	"multiagent/internal/message"
	"multiagent/internal/tool"
)

// Request 是一次模型调用请求。
type Request struct {
	System   string            // 系统提示词
	Messages []message.Message // 对话历史
	Tools    []tool.Definition // 可用工具契约（为空表示不给工具）
}

// Usage 是本次调用的 token 用量（用于成本核算）。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// Response 是一次模型调用结果。
type Response struct {
	Content   string
	ToolCalls []message.ToolCall
	Usage     Usage

	// FinishReason 是模型给的**终止原因**（`stop` / `length` / `tool_calls` / `content_filter`）。
	//
	// # ⚠️ 它曾经被解析了却**没传出来**（R59）
	//
	//	OpenAI 的响应里有 `finish_reason`，而 `oaiResponse` **解析了它**
	//	（`openai.go:238`），却在转换成 `Response` 时**丢掉了**。
	//
	//	后果不是"少一个字段"，是**一个失败类别看不见**：
	//
	//		`length`        输出被截断 → JSON 必然不完整 → 下游报"参数不是合法 JSON"
	//		                而**真正的原因是"模型说到一半被截断了"**
	//		`content_filter` 内容被拦 → 空响应，而下游会说"模型没回答"
	//
	//	> ### 一个被丢掉的 `finish_reason`，会让"截断"伪装成"模型不会写"。
	//
	// ⭐ 而它正好是那份评测建议里点名的第 3 个真问题 ——
	//   **它挡住的是"失败分类"里的"输出截断"那一格**。
	FinishReason string
}

// 几个已知的 `FinishReason` 取值（**受控词表**：可聚合、可切片）。
//
// ⚠️ 它**不是**枚举类型 —— 因为服务商可能加新值，而我们不该因此拒绝一次成功的调用。
// （与 `observ.Kind` 的选择**相反**，那里的强类型是必须的：拼错会让指标静默地另开一列。）
const (
	FinishStop      = "stop"
	FinishLength    = "length"
	FinishToolCalls = "tool_calls"
	FinishFiltered  = "content_filter"
)

// Client 是模型客户端的统一抽象。
type Client interface {
	Generate(ctx context.Context, req Request) (*Response, error)
}

// Name 返回客户端名称，便于日志与 trace 区分。
type Named interface {
	Name() string
}

// TimeoutReporter 是**可选能力**：客户端能报出自己的单次调用超时。
//
// 用途只有一个 —— **让它可见**（L1-6：看不见的东西管不住）。
//
// 超时是这一类里最隐蔽的参数：它不出现在任何输出里，却能让**每一步**都失败。
// L2-9 接真实模型时就撞上了这个：推理型模型的单次调用比硬编码的 60s 长，
// 于是整轮都在超时，而输出里只有一句 "context deadline exceeded"。
//
// 为什么用可选接口、而不是塞进 `Client`？
//
//	Mock 没有"超时"这个概念。强迫它实现一个恒返回 0 的方法，
//	只会让主接口变胖、把真正的契约（Generate）淹没 —— 与 `StatsReporter`、
//	`schema.Partial` 同一个取舍：**接口要小，能力要可探测。**
type TimeoutReporter interface {
	Timeout() time.Duration
}
