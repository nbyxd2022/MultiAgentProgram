package main

import (
	"context"
	"fmt"
	"strings"
)

// Agent 是一个最小可用的工具调用循环。
//
// 它只做三件事：
//  1. 组装上下文（system 作为历史的第一条消息，之后追加工具结果）；
//  2. 执行模型请求的工具，把结果追加回历史；
//  3. 没有工具调用时返回最终回复；超过 MaxSteps 就报错。
type Agent struct {
	System   string
	Client   Client
	Tools    *Registry
	MaxSteps int
}

func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	if a.Client == nil {
		return "", fmt.Errorf("Client 不能为空")
	}
	if a.Tools == nil {
		return "", fmt.Errorf("Tools 不能为空")
	}
	if a.MaxSteps <= 0 {
		a.MaxSteps = 6
	}

	history := make([]Message, 0, 2)
	if strings.TrimSpace(a.System) != "" {
		history = append(history, Message{Role: "system", Content: a.System})
	}
	history = append(history, Message{Role: "user", Content: input})

	for step := 1; step <= a.MaxSteps; step++ {
		fmt.Printf("\n--- step %d ---\n", step)

		resp, err := a.Client.Generate(ctx, LLMRequest{
			Messages: history,
			Tools:    a.Tools.Definitions(),
		})
		if err != nil {
			return "", fmt.Errorf("step %d 调用模型失败: %w", step, err)
		}
		if resp.FinishReason == "length" {
			// 截断不是"模型不会写"，是"输出没写完"。
			// 不在这里区分，后面会把它误判成 JSON 解析失败。
			return "", fmt.Errorf("step %d 输出被截断（finish_reason=length）", step)
		}

		// 自然终止：模型不再请求工具。
		if len(resp.ToolCalls) == 0 {
			return resp.Content, nil
		}

		// 关键一步：把 assistant 的 tool_calls 原样放回历史。
		// 少了它，下一轮的 tool 消息就没有"对应的请求"。
		history = append(history, Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		for _, tc := range resp.ToolCalls {
			result, execErr := a.Tools.Execute(ctx, tc.Function.Name, tc.Function.Arguments)
			if execErr != nil {
				// 工具错误不向上抛，而是变成一条结构化结果回灌给模型。
				// 这是"让模型自愈"与"让程序崩溃"的分界线。
				result = fmt.Sprintf(`{"ok":false,"error":%q}`, execErr.Error())
			}
			fmt.Printf("tool %s(%s)\n  -> %s\n", tc.Function.Name, tc.Function.Arguments, oneLine(result))

			history = append(history, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    result,
			})
		}
	}

	return "", fmt.Errorf("达到最大步数 %d，仍未收敛", a.MaxSteps)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > 160 {
		return string(r[:160]) + "..."
	}
	return s
}
