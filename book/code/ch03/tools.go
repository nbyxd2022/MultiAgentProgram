package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

type Tool interface {
	Definition() ToolDefinition
	Execute(ctx context.Context, args string) (string, error)
}

type Registry struct {
	tools map[string]Tool
	order []string
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

func (r *Registry) Register(t Tool) {
	name := t.Definition().Function.Name
	r.tools[name] = t
	r.order = append(r.order, name)
}

func (r *Registry) Definitions() []ToolDefinition {
	out := make([]ToolDefinition, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name].Definition())
	}
	return out
}

func (r *Registry) Execute(ctx context.Context, name, args string) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("未知工具: %s", name)
	}
	return t.Execute(ctx, args)
}

// ─── get_weather ───────────────────────────────────────────────────

type GetWeather struct{}

func (GetWeather) Definition() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: FunctionDefinition{
			Name:        "get_weather",
			Description: "查询某个城市当前的天气。仅在用户询问实时天气时使用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{
						"type":        "string",
						"description": "城市名，例如：北京、上海",
					},
				},
				"required": []string{"city"},
			},
		},
	}
}

func (GetWeather) Execute(_ context.Context, args string) (string, error) {
	var in struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	city := strings.TrimSpace(in.City)
	if city == "" {
		return "", fmt.Errorf("city 不能为空")
	}

	// 故意保留一个"未知城市"分支：第 7 章会用它演示错误回灌。
	if city == "火星" {
		return "", fmt.Errorf("没有 %s 的天气数据", city)
	}

	temp, condition := 22, "晴"
	if city == "上海" {
		temp, condition = 26, "多云"
	}
	raw, _ := json.Marshal(map[string]any{
		"ok":          true,
		"city":        city,
		"temperature": temp,
		"condition":   condition,
	})
	return string(raw), nil
}

// ─── save_note ─────────────────────────────────────────────────────

type SaveNote struct{}

func (SaveNote) Definition() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: FunctionDefinition{
			Name:        "save_note",
			Description: "把一段整理好的内容保存为 Markdown 笔记，写入 out/ 目录。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title": map[string]any{
						"type":        "string",
						"description": "笔记标题，一句话",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "笔记正文，纯文本或 Markdown",
					},
				},
				"required": []string{"title", "content"},
			},
		},
	}
}

func (SaveNote) Execute(_ context.Context, args string) (string, error) {
	var in struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return "", fmt.Errorf("title 不能为空")
	}
	if strings.TrimSpace(in.Content) == "" {
		return "", fmt.Errorf("content 不能为空")
	}

	if err := os.MkdirAll("out", 0o755); err != nil {
		return "", fmt.Errorf("创建输出目录失败: %w", err)
	}
	name := "note-" + sanitize(title) + ".md"
	path := filepath.Join("out", name)
	body := "# " + title + "\n\n" + in.Content + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("写入笔记失败: %w", err)
	}

	raw, _ := json.Marshal(map[string]any{
		"ok":   true,
		"file": filepath.ToSlash(path),
	})
	return string(raw), nil
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "note"
	}
	runes := []rune(out)
	if len(runes) > 40 {
		out = string(runes[:40])
	}
	return out
}
