// Command l0 是 L0 阶段的 Demo：单 Agent + 工具 + ReAct 循环。
//
// 运行方式（无需任何密钥）：
//
//	go run ./cmd/l0
//	go run ./cmd/l0 新能源汽车行业趋势
//
// 接入真实模型：
//
//	$env:MULTIAGENT_LLM="openai"; $env:OPENAI_API_KEY="sk-xxx"; go run ./cmd/l0
//
// 这个 Demo 刻意保持"最小但完整"：一个 Agent、两个工具、一条能跑通的循环。
// 它的问题（无预算精细化、无质检、无多 Agent 协作）会在后续阶段逐个暴露并解决。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"multiagent/internal/agent"
	"multiagent/internal/boot"
	"multiagent/internal/llm"
	"multiagent/internal/message"
	"multiagent/internal/observ"
	"multiagent/internal/tool"
	"multiagent/internal/tools"
)

const l0SystemPrompt = `你是一个"单页幻灯片生成助手"。

工作流程：
1. 先用 web_search 搜索与主题相关的素材（至少调用一次）；
2. 再调用 render_slide_html 生成一页幻灯片，title 使用用户给的主题，bullets 给出 3 条要点；
3. 不要臆造数据；如果搜索没有结果，就用通用的结构化建议。

约束：
- 必须先搜索、后渲染，不允许跳过搜索直接渲染；
- 工具的返回是 JSON，如果返回里 ok=false，请根据 error 与 hint 修正参数后重试；
- 全部完成后，用一句中文总结你做了什么。`

func main() {
	topic := "新能源汽车行业趋势"
	if len(os.Args) > 1 {
		topic = strings.Join(os.Args[1:], " ")
	}

	// 1) 组装工具
	registry := tool.NewRegistry()
	registry.Register(tools.NewWebSearch())
	registry.Register(tools.NewRenderSlideHTML("out"))

	// 2) 选择模型（默认离线 Mock，可切真实模型）
	client := boot.NewClient(newL0Mock())

	// 3) 构造 Agent
	a := agent.New(agent.Options{
		Name:         "slide-maker",
		Description:  "根据一个主题生成单页 HTML 幻灯片",
		SystemPrompt: l0SystemPrompt,
		Client:       client,
		Tools:        registry,
		MaxSteps:     6,
		Tracer:       observ.NewConsoleTracer(os.Stdout),
	})

	printHeader(topic, boot.Describe(client), registry.Names())

	// 4) 跑一次 ReAct 循环
	final, err := a.Run(context.Background(), topic)
	if err != nil {
		fmt.Printf("\n[运行失败] %v\n", err)
		fmt.Printf("[统计] %+v\n", a.Stats())
		os.Exit(1)
	}

	fmt.Printf("\n===== 最终回复 =====\n%s\n", final)
	st := a.Stats()
	fmt.Printf("\n===== 本次统计 =====\n步数=%d  工具调用=%d  PromptTokens=%d  CompletionTokens=%d\n",
		st.Steps, st.ToolCalls, st.PromptTokens, st.CompletionTokens)
	fmt.Println("\n提示：产物已写入 out/ 目录，用浏览器打开即可查看。")
}

func printHeader(topic, model string, toolNames []string) {
	fmt.Println("========== L0 Demo：单 Agent + ReAct 循环 ==========")
	fmt.Printf("主题      : %s\n", topic)
	fmt.Printf("模型      : %s\n", model)
	fmt.Printf("可用工具  : %s\n", strings.Join(toolNames, ", "))
	fmt.Println("---------------------------------------------------")
}

// ⚠️ 这里**曾经有一个 `printEvent`**（R46 删掉了）——
//
//	它是"把事件打成一行"的**第四份**实现（`cmd/l0` / `l1` / `l2` / `l3` 各一份），
//	而那段逻辑**不专属任何命令** → 它住错了地方 → 现在是 `observ.ConsoleTracer`。
//
//	⭐ 而旧的这一份还带一个**信息缺口**：它按 Kind 手工分派，
//	  于是**每加一种 Kind 就要记得加一个 case**（忘了就静默不打）。
//	  `ConsoleTracer` 走的是"按字段打"，加 Kind 不用改它。

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > 160 {
		return string([]rune(s)[:160]) + "..."
	}
	return s
}

// ─── 离线 Mock 模型 ─────────────────────────────────────────────────
//
// 这个 Mock 会"看上下文"决定下一步：先搜索、再渲染、最后收尾。
// 目的是让 Demo 在没有 API Key 的情况下也能完整演示 ReAct 循环。
//
// 注意：它不是"智能"的，只是脚本逻辑。真实效果必须接真实模型验证。

func newL0Mock() llm.Client {
	return llm.NewFuncClient("l0-offline-mock", func(_ context.Context, _ int, req llm.Request) (*llm.Response, error) {
		topic := firstUserText(req.Messages)
		toolResults := countRole(req.Messages, message.RoleTool)

		switch toolResults {
		case 0:
			return &llm.Response{
				Content: "我先搜索一下相关素材。",
				ToolCalls: []message.ToolCall{{
					ID:        "call_search_1",
					Name:      "web_search",
					Arguments: mustJSON(map[string]any{"query": topic, "top_k": 3}),
				}},
				Usage: llm.Usage{PromptTokens: 180, CompletionTokens: 40},
			}, nil

		case 1:
			return &llm.Response{
				Content: "素材够了，我来生成一页幻灯片。",
				ToolCalls: []message.ToolCall{{
					ID:   "call_render_1",
					Name: "render_slide_html",
					Arguments: mustJSON(map[string]any{
						"title": topic,
						"bullets": []string{
							"结论先行：先给出可验证的核心判断",
							"数据支撑：每个判断配一个可量化指标",
							"行动建议：落到 3 条可执行的事情",
						},
					}),
				}},
				Usage: llm.Usage{PromptTokens: 320, CompletionTokens: 90},
			}, nil

		default:
			return &llm.Response{
				Content: "已完成：搜索了相关素材，并生成 1 页 HTML 幻灯片（见 out/ 目录）。",
				Usage:   llm.Usage{PromptTokens: 420, CompletionTokens: 32},
			}, nil
		}
	})
}

func firstUserText(msgs []message.Message) string {
	for _, m := range msgs {
		if m.Role == message.RoleUser && strings.TrimSpace(m.Content) != "" {
			return strings.TrimSpace(m.Content)
		}
	}
	return "未命名主题"
}

func countRole(msgs []message.Message, role message.Role) int {
	n := 0
	for _, m := range msgs {
		if m.Role == role {
			n++
		}
	}
	return n
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
