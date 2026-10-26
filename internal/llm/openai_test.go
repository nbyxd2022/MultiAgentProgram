package llm

import (
	"testing"

	"multiagent/internal/tool"
)

// 这个文件只测一件事：Definition → JSON Schema 的翻译。
//
// 为什么值得单独测？
//
//	因为它是"工具能不能被模型正确调用"的**唯一通道**。
//	翻译错了（比如对象没展开 properties），模型会看不到字段名，
//	而**代码层面一点报错都没有**——这属于最隐蔽的一类故障。
//
// 另：本文件也顺手补上了 P0 遗留的一笔债——
// 我们一直在说"Definition 是给模型看的接口文档"，却从没验证过它翻译出来长什么样。

func TestBuildJSONSchemaNestedObjectArray(t *testing.T) {
	def := tool.Definition{
		Name: "submit_review",
		Parameters: []tool.Parameter{
			{
				Name:     "issues",
				Type:     tool.TypeArray,
				Required: true,
				Items: &tool.Parameter{
					Name: "issue",
					Type: tool.TypeObject,
					Properties: []tool.Parameter{
						{Name: "level", Type: tool.TypeString, Required: true, Description: "严重度"},
						{Name: "field", Type: tool.TypeString, Required: true},
						{Name: "message", Type: tool.TypeString, Required: true},
						{Name: "fix", Type: tool.TypeString, Required: true},
					},
				},
			},
		},
	}

	got := buildJSONSchema(def)

	if got["type"] != "object" {
		t.Fatalf("顶层 type = %v, want object", got["type"])
	}
	required, ok := got["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "issues" {
		t.Fatalf("顶层 required = %v, want [issues]", got["required"])
	}

	issues, ok := got["properties"].(map[string]any)["issues"].(map[string]any)
	if !ok {
		t.Fatal("properties.issues 缺失")
	}
	if issues["type"] != "array" {
		t.Errorf("issues.type = %v, want array", issues["type"])
	}

	items, ok := issues["items"].(map[string]any)
	if !ok {
		t.Fatal("issues.items 缺失——数组不写 items，模型不知道元素长什么样")
	}
	if items["type"] != "object" {
		t.Errorf("items.type = %v, want object", items["type"])
	}

	// 关键断言：对象的 properties 必须被**展开**。
	// 如果这里只输出 {"type":"object"}，模型会完全不知道可以填哪些字段，
	// 于是要么乱编字段名，要么干脆不调用这个工具。
	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatal("对象的 properties 没有被展开——模型将看不到任何字段名")
	}
	for _, name := range []string{"level", "field", "message", "fix"} {
		if _, ok := itemProps[name]; !ok {
			t.Errorf("缺少字段 %q", name)
		}
	}

	itemRequired, ok := items["required"].([]string)
	if !ok || len(itemRequired) != 4 {
		t.Errorf("items.required = %v, want 4 个字段全部必填", items["required"])
	}
}

func TestBuildJSONSchemaFlatParamsStillWork(t *testing.T) {
	def := tool.Definition{
		Name: "web_search",
		Parameters: []tool.Parameter{
			{Name: "query", Type: tool.TypeString, Required: true, Description: "关键词"},
			{Name: "top_k", Type: tool.TypeInteger, Description: "返回条数"},
		},
	}

	got := buildJSONSchema(def)
	props, ok := got["properties"].(map[string]any)
	if !ok || len(props) != 2 {
		t.Fatalf("properties = %v", got["properties"])
	}

	query := props["query"].(map[string]any)
	if query["type"] != "string" || query["description"] != "关键词" {
		t.Errorf("query schema = %v", query)
	}
	// 非必填参数不应出现在 required 里
	required, _ := got["required"].([]string)
	if len(required) != 1 || required[0] != "query" {
		t.Errorf("required = %v, want [query]", got["required"])
	}
}

func TestBuildJSONSchemaArrayOfString(t *testing.T) {
	def := tool.Definition{
		Name: "render_slide_html",
		Parameters: []tool.Parameter{
			{
				Name: "bullets", Type: tool.TypeArray, Required: true,
				Items: &tool.Parameter{Name: "item", Type: tool.TypeString, Description: "单条要点"},
			},
		},
	}

	got := buildJSONSchema(def)
	bullets := got["properties"].(map[string]any)["bullets"].(map[string]any)
	items := bullets["items"].(map[string]any)

	if items["type"] != "string" || items["description"] != "单条要点" {
		t.Errorf("items = %v", items)
	}
}

func TestBuildJSONSchemaOmitsRequiredWhenNoneRequired(t *testing.T) {
	def := tool.Definition{
		Name:       "no_required",
		Parameters: []tool.Parameter{{Name: "note", Type: tool.TypeString}},
	}

	got := buildJSONSchema(def)
	if _, ok := got["required"]; ok {
		t.Errorf("没有必填参数时不应输出 required 字段，实际 %v", got["required"])
	}
}
