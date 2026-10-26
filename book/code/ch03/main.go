// Command ch03 是一个最小可用的多步 Agent。
//
// 默认使用离线 Mock，不需要 API Key：
//
//	go run ./book/code/ch03
//
// 接真实模型（PowerShell）：
//
//	$env:MULTIAGENT_LLM="openai"
//	$env:OPENAI_API_KEY="sk-..."
//	go run ./book/code/ch03
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	registry := NewRegistry()
	registry.Register(GetWeather{})
	registry.Register(SaveNote{})

	var client Client = &MockClient{}
	if os.Getenv("MULTIAGENT_LLM") == "openai" {
		c, err := NewOpenAIClientFromEnv()
		if err != nil {
			fmt.Println("初始化真实模型失败:", err)
			os.Exit(1)
		}
		client = c
		fmt.Println("模型: openai（真实调用）")
	} else {
		fmt.Println("模型: mock（离线，无需 API Key）")
	}

	a := &Agent{
		System: "你是一个天气助手。先查天气，再保存笔记。" +
			"不要编造工具没有返回的数据；工具返回 ok=false 时，先修正参数再试。",
		Client:   client,
		Tools:    registry,
		MaxSteps: 6,
	}

	final, err := a.Run(context.Background(), "帮我查北京和上海的天气，并保存成一条笔记")
	if err != nil {
		fmt.Println("\n[运行失败]", err)
		os.Exit(1)
	}

	fmt.Println("\n===== 最终回复 =====")
	fmt.Println(final)
	fmt.Println("\n提示：笔记已写入 out/ 目录。")
}
