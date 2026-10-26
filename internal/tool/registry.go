package tool

import (
	"context"
	"fmt"
	"sort"
)

// Registry 是工具的注册表：Agent 通过它获得"可用工具清单"，并按名调用。
//
// 设计要点：
//   - 注册顺序被保留（order），便于稳定地生成提示词与调试输出；
//   - 调用未知工具时返回明确错误，而不是 panic——错误最终会回灌给模型自愈。
type Registry struct {
	tools map[string]Tool
	order []string
}

// NewRegistry 创建一个空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register 注册一个工具；同名工具会被覆盖，但保持原有顺序。
func (r *Registry) Register(t Tool) {
	name := t.Definition().Name
	if _, exists := r.tools[name]; !exists {
		r.order = append(r.order, name)
	}
	r.tools[name] = t
}

// Get 按名获取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Definitions 返回全部工具契约，顺序与注册顺序一致。
func (r *Registry) Definitions() []Definition {
	defs := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		defs = append(defs, r.tools[name].Definition())
	}
	return defs
}

// Execute 按名执行工具。
func (r *Registry) Execute(ctx context.Context, name, args string) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("未知工具: %q（可用工具: %v）", name, r.Names())
	}
	return t.Execute(ctx, args)
}

// Names 返回全部工具名（字典序）。
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	sort.Strings(out)
	return out
}

// Len 返回工具数量。
func (r *Registry) Len() int {
	return len(r.tools)
}
