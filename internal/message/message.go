// Package message 定义全系统统一的消息模型。
//
// 设计要点：
//   - 只描述"一条消息长什么样"，不关心它从哪来、到哪去；
//   - 覆盖四种角色：system / user / assistant / tool；
//   - assistant 消息可携带 ToolCalls，tool 消息可携带 ToolCallID 与之配对。
//
// 这是整个系统最底层的契约，所有上层（LLM 客户端、Agent、图编排）都基于它通信。
package message

// Role 是消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 表示模型请求调用某个工具。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串，由工具自行解析
}

// Message 是一条对话消息。
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // 仅 assistant
	ToolCallID string     `json:"tool_call_id,omitempty"` // 仅 tool，与 ToolCall.ID 配对
	Name       string     `json:"name,omitempty"`         // 仅 tool，记录工具名
}

// System 构造一条 system 消息。
func System(content string) Message {
	return Message{Role: RoleSystem, Content: content}
}

// User 构造一条 user 消息。
func User(content string) Message {
	return Message{Role: RoleUser, Content: content}
}

// Assistant 构造一条 assistant 消息。
func Assistant(content string) Message {
	return Message{Role: RoleAssistant, Content: content}
}

// AssistantToolCall 构造一条带工具调用的 assistant 消息。
func AssistantToolCall(content string, calls ...ToolCall) Message {
	return Message{Role: RoleAssistant, Content: content, ToolCalls: calls}
}

// ToolResult 构造一条工具执行结果消息。
func ToolResult(callID, name, content string) Message {
	return Message{
		Role:       RoleTool,
		ToolCallID: callID,
		Name:       name,
		Content:    content,
	}
}
