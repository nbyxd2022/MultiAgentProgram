// Package boot 负责装配运行期依赖（这里是模型客户端）。
//
// 目的：把"用 Mock 还是用真实模型"这个决策收敛到一个地方，
// 让每个 cmd/demo 的 main 都能一行切换，而不必各自读环境变量。
package boot

import (
	"fmt"
	"os"

	"multiagent/internal/llm"
)

const EnvLLM = "MULTIAGENT_LLM"

// NewClient 返回模型客户端：
//   - 默认返回传入的离线 mock（保证无密钥即可运行）；
//   - 当环境变量 MULTIAGENT_LLM=openai 时，改用真实 OpenAI 兼容服务。
//
// 相关环境变量：
//
//	MULTIAGENT_LLM=openai
//	OPENAI_API_KEY / OPENAI_BASE_URL / OPENAI_MODEL
func NewClient(mock llm.Client) llm.Client {
	if os.Getenv(EnvLLM) == "openai" {
		return llm.NewOpenAIClient(llm.OpenAIConfig{})
	}
	return mock
}

// ClientName 返回客户端名称，便于在 Demo 输出中标明"当前用的是谁"。
func ClientName(c llm.Client) string {
	if named, ok := c.(llm.Named); ok {
		return named.Name()
	}
	return "unknown"
}

// Describe 返回客户端的一句话描述（模型 + **生效的**超时），供 Demo 表头打印。
//
// 为什么要专门把超时打出来？
//
//	因为它是最隐蔽的一类参数：不出现在任何输出里，却能让每一步都失败。
//	L2-9 的真实运行就撞上了 —— 屏幕上只有 "context deadline exceeded"，
//	人根本不知道"到底等多长时间算超时、这个值是谁定的、能不能调"。
//
// 用**可选接口**探测（而不是断言到具体类型）：`boot` 不需要知道
// `OpenAIClient` 的存在，Mock 也不会被牵连（它不实现这个能力，就只显示名字）。
func Describe(c llm.Client) string {
	name := ClientName(c)
	if tr, ok := c.(llm.TimeoutReporter); ok {
		return fmt.Sprintf("%s · 单次超时 %s", name, tr.Timeout())
	}
	return name
}
