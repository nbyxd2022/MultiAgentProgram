package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Message 是本示例自己的会话消息模型。
//
// 注意：仓库里有更完整的 internal/message，字段含义相同。
// 这里重写一份，是为了让你看到"协议本身有多小"。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall 直接按 OpenAI 的线格式定义，省掉一层转换。
// 仓库的做法是领域模型与线格式分开（message.ToolCall vs oaiToolCall），
// 好处是换服务商时只改适配器；这里为了短，先合在一起。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type FunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolDefinition struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

type LLMRequest struct {
	Messages []Message
	Tools    []ToolDefinition
}

type LLMResponse struct {
	Content          string
	ToolCalls        []ToolCall
	FinishReason     string
	PromptTokens     int
	CompletionTokens int
}

type Client interface {
	Generate(ctx context.Context, req LLMRequest) (*LLMResponse, error)
}

// ─── 离线 Mock ─────────────────────────────────────────────────────

type MockClient struct{}

// Generate 按"已经出现了几条 tool 消息"决定下一步。
// 它不是智能的，只是把一段多步流程脚本化，让你在没有 API Key 时也能观察循环。
func (m *MockClient) Generate(_ context.Context, req LLMRequest) (*LLMResponse, error) {
	toolResults := countRole(req.Messages, "tool")
	switch toolResults {
	case 0:
		return &LLMResponse{
			Content: "我先查一下北京和上海的天气。",
			ToolCalls: []ToolCall{
				{ID: "call_bj", Type: "function", Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"北京"}`}},
				{ID: "call_sh", Type: "function", Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"上海"}`}},
			},
			FinishReason: "tool_calls",
		}, nil
	case 2:
		return &LLMResponse{
			Content: "两座城市都查到了，我把结果整理成一条笔记。",
			ToolCalls: []ToolCall{
				{ID: "call_note", Type: "function", Function: FunctionCall{
					Name:      "save_note",
					Arguments: `{"title":"天气笔记","content":"北京 22 度晴；上海 26 度多云。"}`,
				}},
			},
			FinishReason: "tool_calls",
		}, nil
	default:
		return &LLMResponse{
			Content:      "已完成：查询了北京和上海的天气，并保存为 out/note-天气笔记.md。",
			FinishReason: "stop",
		}, nil
	}
}

func countRole(msgs []Message, role string) int {
	n := 0
	for _, m := range msgs {
		if m.Role == role {
			n++
		}
	}
	return n
}

// ─── OpenAI 兼容客户端 ─────────────────────────────────────────────

type OpenAIClient struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

func NewOpenAIClientFromEnv() (*OpenAIClient, error) {
	key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if key == "" {
		return nil, fmt.Errorf("缺少 OPENAI_API_KEY")
	}
	return &OpenAIClient{
		baseURL: strings.TrimRight(envOr("OPENAI_BASE_URL", "https://api.openai.com/v1"), "/"),
		apiKey:  key,
		model:   envOr("OPENAI_MODEL", "gpt-4o-mini"),
		http:    &http.Client{Timeout: 120 * time.Second},
	}, nil
}

func (c *OpenAIClient) Generate(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	wire := struct {
		Model    string           `json:"model"`
		Messages []Message        `json:"messages"`
		Tools    []ToolDefinition `json:"tools,omitempty"`
	}{
		Model:    c.model,
		Messages: req.Messages,
		Tools:    req.Tools,
	}

	raw, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求模型失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content   *string    `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析响应失败 (HTTP %d): %s", resp.StatusCode, truncate(string(body), 300))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("服务端错误 (HTTP %d): %s", resp.StatusCode, parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("服务端返回 HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("响应里没有 choices")
	}

	choice := parsed.Choices[0]
	out := &LLMResponse{
		ToolCalls:        choice.Message.ToolCalls,
		FinishReason:     choice.FinishReason,
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
	}
	if choice.Message.Content != nil {
		out.Content = *choice.Message.Content
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
