package llm

import (
	"context"
	"fmt"
	"sync"

	"multiagent/internal/message"
)

// Step 是剧本中的一步模型输出。
type Step struct {
	Content   string
	ToolCalls []message.ToolCall
}

// ScriptedClient 按预设剧本依次返回结果。
//
// 用途：离线跑通 Demo、编写可复现的单元测试。
// 局限：它"不会思考"，因此不能用来评估 Prompt 质量——这是刻意的。
//
//	真实效果必须用真实模型验证（见 docs/00-教学计划.md 的 P0 复盘）。
type ScriptedClient struct {
	mu       sync.Mutex
	script   []Step
	idx      int
	fallback string
	label    string
}

// NewScriptedClient 创建一个剧本客户端。fallback 为剧本耗尽后的兜底回复。
func NewScriptedClient(label, fallback string, steps ...Step) *ScriptedClient {
	if label == "" {
		label = "scripted-mock"
	}
	if fallback == "" {
		fallback = "（剧本已用尽，任务结束）"
	}
	return &ScriptedClient{script: steps, fallback: fallback, label: label}
}

// Name 实现 Named 接口。
func (c *ScriptedClient) Name() string { return c.label }

// Generate 实现 Client 接口。
func (c *ScriptedClient) Generate(_ context.Context, _ Request) (*Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.idx >= len(c.script) {
		return &Response{
			Content: c.fallback,
			Usage:   Usage{PromptTokens: 64, CompletionTokens: 16},
		}, nil
	}

	step := c.script[c.idx]
	c.idx++
	return &Response{
		Content:   step.Content,
		ToolCalls: step.ToolCalls,
		// Mock 的 token 用量按内容长度粗估，仅用于打通成本统计链路。
		Usage: Usage{
			PromptTokens:     estimateTokens(step.Content) + 32,
			CompletionTokens: estimateTokens(step.Content),
		},
	}, nil
}

// Reset 重置剧本指针。
func (c *ScriptedClient) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.idx = 0
}

// FuncClient 用函数决定每次输出，可基于输入与轮次做判断。
//
// 用途：需要"对输入做出反应"的离线 Demo（比 ScriptedClient 更灵活）。
type FuncClient struct {
	mu    sync.Mutex
	turn  int
	label string
	fn    func(ctx context.Context, turn int, req Request) (*Response, error)
}

// NewFuncClient 创建一个函数式客户端。
func NewFuncClient(label string, fn func(ctx context.Context, turn int, req Request) (*Response, error)) *FuncClient {
	if label == "" {
		label = "func-mock"
	}
	return &FuncClient{label: label, fn: fn}
}

// Name 实现 Named 接口。
func (c *FuncClient) Name() string { return c.label }

// Generate 实现 Client 接口。
func (c *FuncClient) Generate(ctx context.Context, req Request) (*Response, error) {
	c.mu.Lock()
	turn := c.turn
	c.turn++
	c.mu.Unlock()

	if c.fn == nil {
		return nil, fmt.Errorf("FuncClient: fn 未配置")
	}
	return c.fn(ctx, turn, req)
}

// estimateTokens 用字符数粗估 token。
// 真实项目应使用模型返回的 usage 字段，这里仅用于离线打通成本统计链路。
func estimateTokens(s string) int {
	n := len([]rune(s))
	return (n + 1) / 2
}
