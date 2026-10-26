// Package tool 定义工具的契约（Contract）与注册表（Registry）。
//
// 设计要点：
//   - Tool 是"模型的手"：模型只能通过 Tool 与外部世界交互；
//   - Definition 是给模型看的"接口文档"，其中 Description 的写法直接决定模型用得对不对；
//   - Execute 接收原始 JSON 字符串，由工具自己解析——这样参数错误可以被结构化为工具结果，
//     而不是在框架层抛出 panic。
package tool

import "context"

// ParameterType 是 JSON Schema 基础类型。
type ParameterType string

const (
	TypeString  ParameterType = "string"
	TypeNumber  ParameterType = "number"
	TypeInteger ParameterType = "integer"
	TypeBoolean ParameterType = "boolean"
	TypeArray   ParameterType = "array"

	// TypeObject 是"对象"类型。
	//
	// 为什么要在 L1 补上它？
	//
	//	因为 Critic 要提交的是「问题清单」，而问题清单天然是**对象数组**：
	//		issues: [ {level, field, message, fix}, ... ]
	//	在补齐之前，我们要么让模型吐文本 JSON 再解析（脆），
	//	要么用几个平行数组（把"对齐"责任推给模型，错位即静默出错）。
	//
	//	协议层表达力不足时，正确做法是**扩展协议**，而不是让业务去将就。
	//	L2 的结构化 handoff 传的也是对象数组，这一步迟早要补。
	TypeObject ParameterType = "object"
)

// Parameter 描述一个工具参数。
//
// 它是一棵**树**，不是一张平表：
//
//	array  → 用 Items 描述"元素长什么样"
//	object → 用 Properties 描述"有哪些字段"
//
// 两者可以嵌套，例如"对象数组"就是 Type=array + Items=TypeObject + Properties=[...]。
type Parameter struct {
	Name        string
	Type        ParameterType
	Description string
	Required    bool
	Items       *Parameter  // 仅当 Type == TypeArray 时使用：数组元素的形状
	Properties  []Parameter // 仅当 Type == TypeObject 时使用：对象各字段的形状
}

// Definition 是工具对外暴露的契约。
type Definition struct {
	Name        string
	Description string
	Parameters  []Parameter
}

// Tool 是工具的定义与执行体。
type Tool interface {
	Definition() Definition
	Execute(ctx context.Context, args string) (string, error)
}
