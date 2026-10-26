// Package tools 提供具体的工具实现。
//
// 这里刻意保留两个"性格不同"的工具，用于教学对比：
//   - render_slide_html：有副作用（写文件）、参数结构复杂、可能失败；
//   - web_search：无副作用、参数简单，但当前是 Mock 数据。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"multiagent/internal/schema"
	"multiagent/internal/tool"
)

// RenderSlideHTML 根据标题与要点生成一页 16:9 的 HTML 幻灯片并写入磁盘。
type RenderSlideHTML struct {
	OutDir string

	// last / has 记录最近一次成功渲染的**结构化产物**。
	//
	// 为什么工具要主动交出产物，而不是让编排层去解析模型的话？
	//
	//	因为产物本来就是工具产出的，就该由工具交出。
	//	让模型用自然语言把它转述一遍，等于让概率模型做无损数据搬运
	//	（企业仓库 A2/A3 那个坑），必然丢字段。
	//
	// 这也是 L2「共享黑板 Blackboard」的最小版本：产物落在工具侧，Agent 之间传引用。
	// 加锁是因为工具可能被并发调用（P12 会把并行真正打开）。
	mu   sync.Mutex
	last schema.Slide
	has  bool
}

// NewRenderSlideHTML 创建一个幻灯片渲染工具。
func NewRenderSlideHTML(outDir string) *RenderSlideHTML {
	if outDir == "" {
		outDir = "out"
	}
	return &RenderSlideHTML{OutDir: outDir}
}

// LastSlide 返回最近一次成功渲染的结构化产物。
//
// 调用方用它拿到"这一轮到底生成了什么"，而不是去猜模型的自然语言。
func (t *RenderSlideHTML) LastSlide() (schema.Slide, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last, t.has
}

// Definition 实现 tool.Tool 接口。
//
// 注意：这里的数字**全部来自 schema 常量**，不再手写。
//
// 之前这里写死了 "2-5 条"，而 layout_check 按 MinBullets=3 判——
// 两处约束自相矛盾，模型无论怎么改都过不了检查，而人还以为是模型的锅。
// 这就是"约束写在两处，两处都会失守"。现在只有一个来源。
func (t *RenderSlideHTML) Definition() tool.Definition {
	return tool.Definition{
		Name: "render_slide_html",
		Description: fmt.Sprintf(
			"根据标题与要点生成一页 16:9 的 HTML 幻灯片，并写入输出目录。"+
				"返回生成文件的路径与字节数。"+
				"注意：标题不超过 %d 字；bullets 为 %d-%d 条纯文本要点，每条不超过 %d 字，不要传 HTML 片段。",
			schema.MaxTitleRunes, schema.MinBullets, schema.MaxBullets, schema.MaxBulletRunes),
		Parameters: []tool.Parameter{
			{
				Name:        "title",
				Type:        tool.TypeString,
				Description: fmt.Sprintf("幻灯片标题，一句话，不超过 %d 个字", schema.MaxTitleRunes),
				Required:    true,
			},
			{
				Name:        "bullets",
				Type:        tool.TypeArray,
				Description: fmt.Sprintf("正文要点列表，%d-%d 条", schema.MinBullets, schema.MaxBullets),
				Required:    true,
				Items: &tool.Parameter{
					Name:        "item",
					Type:        tool.TypeString,
					Description: "单条要点文本",
				},
			},
		},
	}
}

// Render 渲染一页并写盘，返回文件路径与字节数。
//
// 这是**纯内核**：不解析协议、不记录状态。副作用只有"写文件"。
//
// 为什么要有这个入口？（与 L1 的 `layout_check` 的 `Check`/`Execute` 双入口同一个理由）
//
//	Tool.Execute 只收 JSON 字符串。**代码里想直接渲染一页**（比如 L2 的 renderer
//	要一口气渲染 3 页）时，逼它先序列化再解析一遍是没有意义的。
//	把"渲染"这个内核单独暴露出来，于是：
//	  · 可以被单测直接调用，不用构造 JSON；
//	  · 可以被编排代码直接调用；
//	  · 也可以照旧作为工具被模型调用。
//
// 这就是"端口-适配器"思路在**单个工具内部**的复用（原则五）。
func (t *RenderSlideHTML) Render(slide schema.Slide) (string, int, error) {
	title := strings.TrimSpace(slide.Title)
	if title == "" {
		return "", 0, fmt.Errorf("title 不能为空")
	}
	if len(slide.Bullets) == 0 {
		return "", 0, fmt.Errorf("bullets 不能为空，至少需要 1 条要点")
	}
	for i, b := range slide.Bullets {
		if strings.TrimSpace(b) == "" {
			return "", 0, fmt.Errorf("bullets[%d] 不能为空", i)
		}
	}

	page := buildSlideHTML(title, slide.Bullets)

	if err := os.MkdirAll(t.OutDir, 0o755); err != nil {
		return "", 0, fmt.Errorf("创建输出目录失败: %w", err)
	}
	name := fmt.Sprintf("slide-%s.html", sanitizeFilename(title))
	path := filepath.Join(t.OutDir, name)
	if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
		return "", 0, fmt.Errorf("写入文件失败: %w", err)
	}
	return filepath.ToSlash(path), len(page), nil
}

// FilePathFor 返回某一页**将会**写入的路径（不写盘）。
//
// 存在的理由：调用方需要**在产生副作用之前**判断"会不会互相覆盖"。
// 文件名由标题派生，于是"两页标题相同"必然导致后写的那一页把它顶掉 ——
// 这种冲突一旦发生就无法回滚（前一个文件已经没了），所以必须**预检**。
//
// 谁承担后果，谁做校验：文件名的冲突由**渲染方**承担，所以由它提供预检手段。
func (t *RenderSlideHTML) FilePathFor(title string) string {
	return filepath.ToSlash(filepath.Join(t.OutDir, fmt.Sprintf("slide-%s.html", sanitizeFilename(title))))
}

// Execute 实现 tool.Tool 接口。
//
// 参数错误时返回 error，由 Agent 层转成结构化结果回灌给模型自愈。
//
// 参数直接解析成 schema.Slide —— 这里原本有一份私有的 renderSlideArgs，
// 与幻灯片的结构重复定义。现在渲染器、检查器、评审器共用同一个协议类型。
func (t *RenderSlideHTML) Execute(_ context.Context, args string) (string, error) {
	var in schema.Slide
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("参数不是合法 JSON: %w", err)
	}

	path, size, err := t.Render(in)
	if err != nil {
		return "", err
	}

	// 记录产物——**只在写盘成功之后**。
	// 如果提前记录，一次失败的渲染也会把产物登记进去，
	// 后续就会去评审一个"根本不存在的版本"。
	//
	// ⚠️ 注意：这个记录发生在 **Execute（适配器）** 里，而不是 `Render`（内核）里。
	//	于是内核是无状态的，可以被并发调用（L2 的 renderer 正是这么用的）；
	//	"最近一次产物"只对"把工具交给模型调用"的那条路径有意义。
	//	（并发调用 Execute 时 last 的语义是"最后完成的那个"，没有意义但不危险。）
	t.mu.Lock()
	t.last = schema.Slide{
		Title: strings.TrimSpace(in.Title),
		// 防御性拷贝：切片是引用类型，不复制的话调用方改了它，我们这边也会跟着变
		//（同一个坑在 schema.Report.Merged 里也踩过）。
		Bullets: append([]string(nil), in.Bullets...),
	}
	t.has = true
	t.mu.Unlock()

	result, _ := json.Marshal(map[string]any{
		"ok":    true,
		"file":  path,
		"bytes": size,
	})
	return string(result), nil
}

func buildSlideHTML(title string, bullets []string) string {
	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html>\n<html lang=\"zh-CN\">\n<head>\n<meta charset=\"utf-8\">\n<title>")
	sb.WriteString(html.EscapeString(title))
	sb.WriteString("</title>\n<style>\n")
	sb.WriteString(`  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { background: #1b1f24; display: flex; align-items: center; justify-content: center; min-height: 100vh; font-family: "PingFang SC", "Microsoft YaHei", sans-serif; }
  .slide { width: 1280px; height: 720px; background: linear-gradient(135deg, #ffffff 0%, #f4f7fb 100%); padding: 72px 88px; display: flex; flex-direction: column; justify-content: center; }
  h1 { font-size: 56px; color: #16233a; margin-bottom: 48px; line-height: 1.25; border-left: 12px solid #2f6df6; padding-left: 28px; }
  ul { list-style: none; }
  li { font-size: 30px; color: #33415c; margin: 22px 0; padding-left: 46px; position: relative; line-height: 1.5; }
  li::before { content: ""; position: absolute; left: 0; top: 16px; width: 18px; height: 18px; border-radius: 50%; background: #2f6df6; }
`)
	sb.WriteString("</style>\n</head>\n<body>\n  <section class=\"slide\">\n    <h1>")
	sb.WriteString(html.EscapeString(title))
	sb.WriteString("</h1>\n    <ul>")
	for _, b := range bullets {
		sb.WriteString("\n      <li>")
		sb.WriteString(html.EscapeString(b))
		sb.WriteString("</li>")
	}
	sb.WriteString("\n    </ul>\n  </section>\n</body>\n</html>\n")
	return sb.String()
}

// sanitizeFilename 保留中文/字母/数字，其余替换为连字符。
func sanitizeFilename(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			sb.WriteRune(r)
		case r == '-' || r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteRune('-')
		}
	}
	out := strings.Trim(sb.String(), "-")
	if out == "" {
		out = "slide"
	}
	runes := []rune(out)
	if len(runes) > 40 {
		out = string(runes[:40])
	}
	return out
}
