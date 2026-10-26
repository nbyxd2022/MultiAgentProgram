// Command ch02 打印一次带工具的 Chat Completions 请求与原始响应。
//
// 它故意不执行工具：这一章只观察协议，执行与循环放到下一章。
//
// PowerShell:
//
//	$env:OPENAI_API_KEY="sk-..."
//	go run ./book/code/ch02
//
// 可选：
//
//	$env:OPENAI_BASE_URL="https://api.openai.com/v1"
//	$env:OPENAI_MODEL="gpt-4o-mini"
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		fmt.Println(`缺少 OPENAI_API_KEY。先在 PowerShell 里设置：$env:OPENAI_API_KEY="sk-..."`)
		os.Exit(1)
	}
	baseURL := strings.TrimRight(envOr("OPENAI_BASE_URL", "https://api.openai.com/v1"), "/")
	model := envOr("OPENAI_MODEL", "gpt-4o-mini")

	reqBody := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": "你是一个简洁的助手。"},
			{"role": "user", "content": "北京今天天气怎么样？"},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "get_weather",
					"description": "查询某个城市当前的天气。",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"city": map[string]any{
								"type":        "string",
								"description": "城市名，例如：北京",
							},
						},
						"required": []string{"city"},
					},
				},
			},
		},
	}

	raw, err := json.MarshalIndent(reqBody, "", "  ")
	if err != nil {
		fmt.Println("序列化请求失败:", err)
		os.Exit(1)
	}

	fmt.Println("===== REQUEST =====")
	fmt.Println(string(raw))

	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		fmt.Println("构造请求失败:", err)
		os.Exit(1)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		fmt.Println("请求失败:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("读取响应失败:", err)
		os.Exit(1)
	}

	fmt.Printf("\n===== RAW RESPONSE (HTTP %d) =====\n", resp.StatusCode)
	fmt.Println(string(body))

	var parsed struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
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
		fmt.Println("\n[解析失败]", err)
		return
	}
	if parsed.Error != nil {
		fmt.Println("\n[服务端错误]", parsed.Error.Message)
		return
	}
	if len(parsed.Choices) == 0 {
		fmt.Println("\n[没有 choices]")
		return
	}

	c := parsed.Choices[0]
	fmt.Println("\n===== 关键字段 =====")
	fmt.Println("finish_reason:", c.FinishReason)
	fmt.Println("prompt_tokens:", parsed.Usage.PromptTokens, "completion_tokens:", parsed.Usage.CompletionTokens)
	if c.Message.Content != nil {
		fmt.Println("content:", *c.Message.Content)
	} else {
		fmt.Println("content: <null>")
	}
	if len(c.Message.ToolCalls) == 0 {
		fmt.Println("tool_calls: <none>")
		return
	}
	for _, tc := range c.Message.ToolCalls {
		fmt.Println("tool_call id:", tc.ID)
		fmt.Println("tool_call name:", tc.Function.Name)
		fmt.Println("tool_call arguments (字符串，不是对象):", tc.Function.Arguments)
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
