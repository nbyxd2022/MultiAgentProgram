package llm

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

	"multiagent/internal/message"
	"multiagent/internal/tool"
)

// 环境变量名。做成常量而不是散落的字符串字面量 ——
// 它们是**对外契约**（用户要照着设），两处拼错就会变成"设了没用"。
const (
	EnvBaseURL = "OPENAI_BASE_URL"
	EnvAPIKey  = "OPENAI_API_KEY"
	EnvModel   = "OPENAI_MODEL"
	EnvTimeout = "OPENAI_TIMEOUT"
)

// OpenAIConfig 是 OpenAI 兼容服务的配置。
//
// 零值会被环境变量补齐，因此可以直接 OpenAIConfig{} 使用：
//
//	OPENAI_BASE_URL  默认 https://api.openai.com/v1
//	OPENAI_API_KEY   必填
//	OPENAI_MODEL     默认 gpt-4o-mini
//	OPENAI_TIMEOUT   默认 60s（推荐写 "240s" 这样的时长）
type OpenAIConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
}

// OpenAIClient 通过 /chat/completions 调用任意 OpenAI 兼容服务。
type OpenAIClient struct {
	cfg  OpenAIConfig
	http *http.Client
}

// NewOpenAIClient 创建客户端，并用环境变量补齐缺省配置。
func NewOpenAIClient(cfg OpenAIConfig) *OpenAIClient {
	if cfg.BaseURL == "" {
		cfg.BaseURL = os.Getenv(EnvBaseURL)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv(EnvAPIKey)
	}
	if cfg.Model == "" {
		cfg.Model = os.Getenv(EnvModel)
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-mini"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = envDuration(EnvTimeout, 60*time.Second)
	}

	return &OpenAIClient{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout},
	}
}

// envDuration 读一个时长型环境变量；未设或非法则用默认值。
//
// 为什么加它？（**L2-9 接真实模型时被逼出来的**）
//
//	原来超时是**硬编码 60s**。接推理型模型（reasoning model）时发现：
//	它单次调用可能远超 60s，于是**每一步都在超时** ——
//	而代码里**没有任何旋钮可以调**。
//
//	一个"看不见、也调不了"的超时，会让排查停在"是不是我网络不好"，
//	而真实原因是"这个模型的单次调用就是比 60s 长"。
//
// 非法值（比如写成 "abc"）**回退到默认值而不是报错**：
// 配置错误不该让整个程序起不来 —— 但要能通过 Describe 看见生效值。
func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// Name 实现 Named 接口。
func (c *OpenAIClient) Name() string { return "openai:" + c.cfg.Model }

// Timeout 返回**生效的**单次调用超时。
//
// 存在的理由就是"让它可见"：超时是这一类里最隐蔽的参数 ——
// 它不出现在任何输出里，却能让每一步都失败（L1-6：看不见的东西管不住）。
func (c *OpenAIClient) Timeout() time.Duration { return c.cfg.Timeout }

// Generate 实现 Client 接口。
func (c *OpenAIClient) Generate(ctx context.Context, req Request) (*Response, error) {
	if c.cfg.APIKey == "" {
		return nil, fmt.Errorf("缺少 API Key：请设置环境变量 OPENAI_API_KEY")
	}

	body := oaiRequest{Model: c.cfg.Model}
	if req.System != "" {
		body.Messages = append(body.Messages, oaiMessage{Role: string(message.RoleSystem), Content: req.System})
	}
	for _, m := range req.Messages {
		om := oaiMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			var otc oaiToolCall
			otc.ID = tc.ID
			otc.Type = "function"
			otc.Function.Name = tc.Name
			otc.Function.Arguments = tc.Arguments
			om.ToolCalls = append(om.ToolCalls, otc)
		}
		body.Messages = append(body.Messages, om)
	}
	for _, d := range req.Tools {
		var ot oaiTool
		ot.Type = "function"
		ot.Function.Name = d.Name
		ot.Function.Description = d.Description
		ot.Function.Parameters = buildJSONSchema(d)
		body.Tools = append(body.Tools, ot)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	url := c.cfg.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求模型服务失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var parsed oaiResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("解析响应失败 (HTTP %d): %s", resp.StatusCode, truncate(string(respBody), 500))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("模型服务返回错误 (HTTP %d): %s", resp.StatusCode, parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("模型服务返回 HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("模型服务未返回任何选择项")
	}

	choice := parsed.Choices[0]
	out := &Response{
		Content: choice.Message.Content,
		Usage: Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
		},
		// ⚠️ 这一行曾经**不存在** —— 而 `oaiResponse` 早就解析了它（见 `llm.Response` 的注释）。
		//
		//	它挡住的是"输出截断"这个失败类别：`length` 时正文是不完整的，
		//	而下游只会看到"参数不是合法 JSON"。
		FinishReason: choice.FinishReason,
	}
	for _, tc := range choice.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, message.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return out, nil
}

// ─── OpenAI 协议结构 ────────────────────────────────────────────────

type oaiRequest struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	Tools    []oaiTool    `json:"tools,omitempty"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type oaiResponse struct {
	Choices []struct {
		Message      oaiMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ─── Definition → JSON Schema ──────────────────────────────────────

func buildJSONSchema(def tool.Definition) map[string]any {
	return objectSchema(def.Parameters)
}

// objectSchema 把一组参数转成 JSON Schema 里的 object 定义。
//
// 为什么单独抽出来？因为要**递归复用**：
// `submit_review` 的 issues 是"对象数组"，其元素又是一个对象，
// 它内部的 required/properties 生成逻辑，和外层工具参数的逻辑**完全一样**。
// 抽成函数才不会出现"外层对了、内层忘了"的半套实现。
func objectSchema(params []tool.Parameter) map[string]any {
	props := make(map[string]any, len(params))
	required := make([]string, 0, len(params))

	for _, p := range params {
		props[p.Name] = paramSchema(p)
		if p.Required {
			required = append(required, p.Name)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func paramSchema(p tool.Parameter) map[string]any {
	s := map[string]any{
		"type":        string(p.Type),
		"description": p.Description,
	}

	switch p.Type {
	case tool.TypeObject:
		// 对象必须把 properties 展开，否则模型只看到 "type": "object"，
		// 完全不知道里面该放哪些字段——这是最典型的 schema 写残。
		nested := objectSchema(p.Properties)
		s["properties"] = nested["properties"]
		if req, ok := nested["required"]; ok {
			s["required"] = req
		}
	case tool.TypeArray:
		if p.Items != nil {
			s["items"] = paramSchema(*p.Items)
		}
	}
	return s
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
