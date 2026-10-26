package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"multiagent/internal/schema"
	"multiagent/internal/tool"
)

// LayoutCheck 是「硬判据」检查器：只判**能写成 if 的**问题。
//
// 为什么它必须是代码，而不是塞进 Critic 的 Prompt？
//
//	① 确定性：同一输入永远同一结论，不会随采样漂移；
//	② 零成本：不花 token、不花时间（对比一次 LLM 调用）；
//	③ 可测试：每条规则都能单测（见 layout_check_test.go）；
//	④ 不占注意力：Critic 不必把"数条数"这种机械活写进 Prompt。
//
// 这是**原则六**「硬判据交给代码，软判据才交给 LLM」的第一个落地件。
//
// 边界（非常重要）：语义判断——"要点是否空洞""论据是否支撑结论"——
// **不在这里**。它们判不了 if，属于 Critic。混进来会让规则不可测，
// 也会让 Critic 变懒（反正代码已经说"没问题"了）。
type LayoutCheck struct{}

// NewLayoutCheck 创建一个硬判据检查工具。
func NewLayoutCheck() *LayoutCheck { return &LayoutCheck{} }

// Definition 实现 tool.Tool 接口。
//
// 注意 Description 的最后一句"不做语义质量判断"——这是**刻意写明边界**：
// 模型只有知道"这个工具不管什么"，才不会拿它当万能评审用。
func (t *LayoutCheck) Definition() tool.Definition {
	return tool.Definition{
		Name: "layout_check",
		Description: "对幻灯片做版式硬检查（**不做语义质量判断**）。检查项：" +
			"标题是否为空或超过 30 字；要点条数是否在 3-5 条之间；" +
			"单条要点是否为空、超过 40 字、完全重复、或含 HTML 标签；" +
			"句末标点风格是否统一。" +
			"返回 passed（是否无致命/严重问题）与 issues（每条含 level/field/message/fix）；issues 为空表示通过。",
		Parameters: []tool.Parameter{
			{
				Name:        "title",
				Type:        tool.TypeString,
				Description: "幻灯片标题（纯文本）",
				Required:    true,
			},
			{
				Name:        "bullets",
				Type:        tool.TypeArray,
				Description: "正文要点列表（纯文本，2-5 条，不要传 HTML 片段）",
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

// checkOutput 是 layout_check 返回给模型的结构。
//
// ⚠️ 这里有两个布尔值，语义**完全不同**，绝不能合并成一个 ok：
//
//	ok     —— 工具是否执行成功（参数合法、跑得动）
//	passed —— 内容是否合格（没有致命/严重问题）
//
// 如果只给一个 ok，模型会误以为"工具跑通了 = 内容没问题"，
// 于是直接交付一个爆版的产物。这是"协议语义必须无歧义"的典型一课。
type checkOutput struct {
	OK     bool           `json:"ok"`
	Passed bool           `json:"passed"`
	Score  int            `json:"score"`
	Issues []schema.Issue `json:"issues"`
}

// Execute 实现 tool.Tool 接口。
//
// 参数解析失败返回 error，由 Agent 层转成结构化结果回灌（与 L0 的约定一致）。
func (t *LayoutCheck) Execute(_ context.Context, args string) (string, error) {
	// 直接用协议类型解析：这就是 schema.Slide 存在的价值——
	// 工具契约、渲染器、评审器共享同一个字段定义，不再各写一份。
	var in schema.Slide
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("参数不是合法 JSON: %w", err)
	}

	rep := Check(in)

	issues := rep.Sorted()
	if issues == nil {
		// 必须给 []，不能给 null：
		// ① 模型对 null 的处理不稳定；② [] 能明确表达"检查过了，没有问题"。
		issues = []schema.Issue{}
	}

	out, err := json.Marshal(checkOutput{
		OK:     true,
		Passed: !rep.HasHardIssue(),
		Score:  rep.Score(),
		Issues: issues,
	})
	if err != nil {
		return "", fmt.Errorf("序列化问题清单失败: %w", err)
	}
	return string(out), nil
}

// Check 是 layout_check 的**纯函数形态**。
//
// 为什么工具要同时提供纯函数和 Tool 两个入口？
//
//	Tool.Execute 只收 JSON 字符串——代码里想直接调用就得先序列化，很别扭。
//	把"规则"放进纯函数、把"协议适配"留在 Execute，于是：
//	  · 规则可以被单测直接覆盖，不用构造 JSON；
//	  · cmd/l1 想直接调用也可以（不必非得让模型去调）；
//	  · 这也是"端口-适配器"思路在单个工具内部的复用（原则五）。
//
// 返回值 Round/Subject 留空，由调用方（cmd/l1）填写。
func Check(s schema.Slide) schema.Report {
	rep := schema.NewReport(0, "")

	// ── 规则 1/2：标题 ──────────────────────────────────────────
	title := strings.TrimSpace(s.Title)
	switch n := utf8.RuneCountInString(title); {
	case n == 0:
		addIssue(&rep, schema.LevelBlocker, "title",
			"标题为空，无法生成幻灯片", "补一个不超过 30 字的标题")
	case n > schema.MaxTitleRunes:
		addIssue(&rep, schema.LevelMajor, "title",
			fmt.Sprintf("标题 %d 字，超过上限 %d 字", n, schema.MaxTitleRunes),
			fmt.Sprintf("压缩到 %d 字以内", schema.MaxTitleRunes))
	}

	// ── 规则 3-6：逐条要点 ──────────────────────────────────────
	seen := make(map[string]int, len(s.Bullets)) // 归一化文本 → 首次出现的下标
	texts := make([]string, 0, len(s.Bullets))   // 非空要点，供"整体规则"使用

	for i, raw := range s.Bullets {
		field := fmt.Sprintf("bullets[%d]", i)
		b := strings.TrimSpace(raw)

		if b == "" {
			addIssue(&rep, schema.LevelBlocker, field,
				"要点为空", "补一条有实际信息的要点，或删掉这一条")
			continue
		}
		texts = append(texts, b)

		if n := utf8.RuneCountInString(b); n > schema.MaxBulletRunes {
			addIssue(&rep, schema.LevelMajor, field,
				fmt.Sprintf("要点 %d 字，超过上限 %d 字", n, schema.MaxBulletRunes),
				fmt.Sprintf("压缩到 %d 字以内", schema.MaxBulletRunes))
		}

		if m := htmlTagRe.FindString(b); m != "" {
			addIssue(&rep, schema.LevelMajor, field,
				fmt.Sprintf("要点含 HTML 标签 %s，应为纯文本", m),
				"改成纯文本，不要传 HTML 片段")
		}

		key := strings.ToLower(b)
		if first, dup := seen[key]; dup {
			addIssue(&rep, schema.LevelMajor, field,
				fmt.Sprintf("与 bullets[%d] 完全重复", first),
				"合并或替换为一条不同的要点")
			continue
		}
		seen[key] = i
	}

	// ── 规则 7：条数（整体规则）────────────────────────────────
	//
	// 只数**非空**条目：空条目已在上面逐条报过，
	// 若这里再把它计入总数，会给出一个误导性的数字（"要点 3 条"里有一条是空的）。
	switch n := len(texts); {
	case n < schema.MinBullets:
		addIssue(&rep, schema.LevelMajor, "bullets",
			fmt.Sprintf("要点 %d 条，少于下限 %d 条", n, schema.MinBullets),
			fmt.Sprintf("补充到 %d-%d 条", schema.MinBullets, schema.MaxBullets))
	case n > schema.MaxBullets:
		addIssue(&rep, schema.LevelMajor, "bullets",
			fmt.Sprintf("要点 %d 条，超过上限 %d 条", n, schema.MaxBullets),
			fmt.Sprintf("删减到 %d-%d 条", schema.MinBullets, schema.MaxBullets))
	}

	// ── 规则 8：风格一致性（suggestion，不阻塞交付）────────────
	//
	// 这是全套规则里唯一的 suggestion：它不影响"能不能交付"，
	// 只影响观感。它的作用是让 Report 的三个等级都有真实用例，
	// 从而能验证"只剩 suggestion 就不用返工"这条停止条件。
	if len(texts) >= 2 {
		withEnd, withoutEnd := 0, 0
		for _, b := range texts {
			if hasEndPunct(b) {
				withEnd++
			} else {
				withoutEnd++
			}
		}
		if withEnd > 0 && withoutEnd > 0 {
			addIssue(&rep, schema.LevelSuggestion, "bullets",
				"要点句末标点风格不一致（有的有句号、有的没有）",
				"统一风格：要么全部加句末标点，要么全部不加")
		}
	}

	return rep
}

// htmlTagRe 只匹配"看起来像标签"的片段（如 <div>、</b>、<br/>）。
//
// 为什么要用正则而不是简单地找 '<' 和 '>'？
//
//	因为硬判据同样要控制**误报**（false positive）：
//	"增长 < 10%" 里的 '<' 是正常文本，被误判会触发一次无意义返工，白烧一轮 token。
//	误报比漏报更常见，也更贵。
var htmlTagRe = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)

// addIssue 统一追加一条问题。
//
// 把 Source 固定在函数里，是为了**保证它不会被忘记**——
// 这类"每条都要填、但很容易漏"的字段，应该由构造函数/helper 兜住，而不是靠自觉。
func addIssue(rep *schema.Report, level schema.Level, field, message, fix string) {
	rep.Issues = append(rep.Issues, schema.Issue{
		Level:   level,
		Field:   field,
		Message: message,
		Fix:     fix,
		Source:  schema.SourceRule,
	})
}

// hasEndPunct 判断文本是否以句末标点结尾。
func hasEndPunct(s string) bool {
	for _, p := range []string{"。", ".", "！", "!", "？", "?"} {
		if strings.HasSuffix(s, p) {
			return true
		}
	}
	return false
}
