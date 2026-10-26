package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"multiagent/internal/schema"
)

// ─── 测试夹具 ───────────────────────────────────────────────────────

// cleanSlide 是一个"必须零问题"的合格样本。
// 它同时也是对规则的隐式约束：只要它挂了，就说明某条规则太严（误报）。
func cleanSlide() schema.Slide {
	return schema.Slide{
		Title: "新能源汽车行业趋势",
		Bullets: []string{
			"结论先行：先给出可验证的核心判断。",
			"数据支撑：每个判断配一个可量化指标。",
			"行动建议：落到 3 条可执行的事情。",
		},
	}
}

// bullets 生成 n 条互不相同、长度合规、标点一致的要点。
func bullets(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("第 %d 条要点，长度足够，用来测试条数规则。", i+1)
	}
	return out
}

func findIssue(issues []schema.Issue, level schema.Level, field string) *schema.Issue {
	for i := range issues {
		if issues[i].Level == level && issues[i].Field == field {
			return &issues[i]
		}
	}
	return nil
}

// ─── 合格样本：不能有任何问题 ───────────────────────────────────────

func TestCheckCleanSlidePasses(t *testing.T) {
	rep := Check(cleanSlide())

	if len(rep.Issues) != 0 {
		t.Fatalf("合格样本不应有问题，实际 %d 条：%v", len(rep.Issues), rep.Lines())
	}
	if rep.HasHardIssue() {
		t.Error("合格样本不应有硬问题")
	}
	if rep.Score() != 0 {
		t.Errorf("合格样本 Score() 应为 0，实际 %d", rep.Score())
	}
}

// ─── 标题规则 ───────────────────────────────────────────────────────

func TestCheckTitleRules(t *testing.T) {
	cases := []struct {
		name      string
		title     string
		wantLevel schema.Level
	}{
		{"为空 → 致命（产出是坏的）", "   ", schema.LevelBlocker},
		{"超过 30 字 → 严重", strings.Repeat("长", 31), schema.LevelMajor},
	}

	for _, c := range cases {
		s := cleanSlide()
		s.Title = c.title
		rep := Check(s)

		if got := findIssue(rep.Issues, c.wantLevel, "title"); got == nil {
			t.Errorf("%s：未产出 %s/title 问题，实际 %v", c.name, c.wantLevel, rep.Lines())
		} else if got.Fix == "" {
			t.Errorf("%s：问题的 Fix 为空——没有 fix，模型就不知道该怎么改", c.name)
		}
	}
}

// ─── 条数规则（爆版 / 过少）─────────────────────────────────────────

func TestCheckBulletCountRules(t *testing.T) {
	cases := []struct {
		name      string
		n         int
		wantIssue bool
		wantWord  string
	}{
		{"少于下限", schema.MinBullets - 1, true, "少于下限"},
		{"正好下限", schema.MinBullets, false, ""},
		{"正好上限", schema.MaxBullets, false, ""},
		{"爆版（超过上限）", schema.MaxBullets + 2, true, "超过上限"},
	}

	for _, c := range cases {
		s := cleanSlide()
		s.Bullets = bullets(c.n)
		rep := Check(s)

		got := findIssue(rep.Issues, schema.LevelMajor, "bullets")
		if c.wantIssue {
			if got == nil {
				t.Errorf("%s：应产出 major/bullets 问题，实际 %v", c.name, rep.Lines())
				continue
			}
			if !strings.Contains(got.Message, c.wantWord) {
				t.Errorf("%s：问题描述应含 %q，实际 %q", c.name, c.wantWord, got.Message)
			}
			if !strings.Contains(got.Message, fmt.Sprint(c.n)) {
				t.Errorf("%s：问题描述应写出实际条数 %d，实际 %q", c.name, c.n, got.Message)
			}
		} else if got != nil {
			t.Errorf("%s：不该有条数问题，实际 %q", c.name, got.Message)
		}
	}
}

// ─── 单条要点规则 ───────────────────────────────────────────────────

func TestCheckBulletItemRules(t *testing.T) {
	t.Run("空要点定位到具体下标", func(t *testing.T) {
		s := cleanSlide()
		s.Bullets = []string{"第一条要点内容。", "   ", "第三条要点内容。"}
		rep := Check(s)

		if got := findIssue(rep.Issues, schema.LevelBlocker, "bullets[1]"); got == nil {
			t.Errorf("应定位到 bullets[1]，实际 %v", rep.Lines())
		}
	})

	t.Run("超长要点", func(t *testing.T) {
		s := cleanSlide()
		s.Bullets[0] = strings.Repeat("长", schema.MaxBulletRunes+1) + "。"
		rep := Check(s)

		if got := findIssue(rep.Issues, schema.LevelMajor, "bullets[0]"); got == nil {
			t.Errorf("应报 bullets[0] 超长，实际 %v", rep.Lines())
		}
	})

	t.Run("重复要点要指出与谁重复", func(t *testing.T) {
		s := cleanSlide()
		s.Bullets = []string{"同一句话。", "另一句话。", "同一句话。"}
		rep := Check(s)

		got := findIssue(rep.Issues, schema.LevelMajor, "bullets[2]")
		if got == nil {
			t.Fatalf("应报 bullets[2] 重复，实际 %v", rep.Lines())
		}
		if !strings.Contains(got.Message, "bullets[0]") {
			t.Errorf("重复问题应指出原始位置 bullets[0]，实际 %q", got.Message)
		}
	})

	t.Run("含 HTML 标签", func(t *testing.T) {
		s := cleanSlide()
		s.Bullets[0] = "<div>这是要点</div>"
		rep := Check(s)

		if got := findIssue(rep.Issues, schema.LevelMajor, "bullets[0]"); got == nil {
			t.Errorf("应报 bullets[0] 含 HTML，实际 %v", rep.Lines())
		}
	})

	t.Run("数学符号不能被误判成 HTML", func(t *testing.T) {
		s := cleanSlide()
		s.Bullets[0] = "增长 < 10% 的年份只有两年。"
		rep := Check(s)

		if got := findIssue(rep.Issues, schema.LevelMajor, "bullets[0]"); got != nil {
			t.Errorf("误报！%q 是正常文本，却被判成 HTML：%s", s.Bullets[0], got.Message)
		}
	})
}

// ─── suggestion 级：不阻塞交付 ─────────────────────────────────────

func TestCheckPunctuationMismatchIsSuggestionOnly(t *testing.T) {
	s := cleanSlide()
	s.Bullets = []string{"第一条要点内容", "第二条要点内容。", "第三条要点内容。"}

	rep := Check(s)

	if rep.HasHardIssue() {
		t.Errorf("标点风格不一致只是观感问题，不该阻塞交付：%v", rep.Lines())
	}
	if rep.Score() == 0 {
		t.Error("应产出 suggestion 级问题，否则三个等级里有一个永远测不到")
	}
	if got := findIssue(rep.Issues, schema.LevelSuggestion, "bullets"); got == nil {
		t.Errorf("应产出 suggestion/bullets 问题，实际 %v", rep.Lines())
	}
}

// ─── Execute：协议层 ───────────────────────────────────────────────

func TestExecuteProtocol(t *testing.T) {
	raw, err := NewLayoutCheck().Execute(context.Background(),
		`{"title":"新能源汽车行业趋势","bullets":["结论先行：先给出可验证的核心判断。","数据支撑：每个判断配一个可量化指标。","行动建议：落到 3 条可执行的事情。"]}`)
	if err != nil {
		t.Fatalf("Execute 不应报错: %v", err)
	}

	var out struct {
		OK     bool           `json:"ok"`
		Passed bool           `json:"passed"`
		Score  int            `json:"score"`
		Issues []schema.Issue `json:"issues"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("返回值不是合法 JSON: %v\n%s", err, raw)
	}

	if !out.OK {
		t.Error("工具执行成功时 ok 应为 true")
	}
	if !out.Passed {
		t.Errorf("合格样本 passed 应为 true，issues=%v", out.Issues)
	}
	if out.Score != 0 {
		t.Errorf("合格样本 score 应为 0，实际 %d", out.Score)
	}
}

// TestExecuteIssuesIsArrayNotNull 固定一个容易被忽略的协议细节。
//
// 原因：nil 切片序列化成 null，模型对 null 的处理不稳定；
// 而 [] 明确表达"检查过了，没有问题"。
func TestExecuteIssuesIsArrayNotNull(t *testing.T) {
	raw, err := NewLayoutCheck().Execute(context.Background(),
		`{"title":"新能源汽车行业趋势","bullets":["结论先行：先给出可验证的核心判断。","数据支撑：每个判断配一个可量化指标。","行动建议：落到 3 条可执行的事情。"]}`)
	if err != nil {
		t.Fatalf("Execute 不应报错: %v", err)
	}
	if !strings.Contains(raw, `"issues":[]`) {
		t.Errorf("无问题时 issues 必须是 []，不能是 null：%s", raw)
	}
}

func TestExecuteBadJSONReturnsError(t *testing.T) {
	_, err := NewLayoutCheck().Execute(context.Background(), `{"title":`)
	if err == nil {
		t.Fatal("非法 JSON 必须返回 error（由 Agent 层转成结构化结果回灌给模型自愈）")
	}
}

// TestDefinitionStatesBoundary 守护"描述即 Prompt"里最重要的一句话：边界。
//
// 如果 Description 不写"不做语义判断"，Critic 很容易把 layout_check
// 当成万能评审，于是"要点空洞"这类软问题就永远没人管了。
func TestDefinitionStatesBoundary(t *testing.T) {
	def := NewLayoutCheck().Definition()

	if def.Name != "layout_check" {
		t.Errorf("工具名 = %q，模型靠它调用，不能随意改", def.Name)
	}
	if !strings.Contains(def.Description, "不做语义质量判断") {
		t.Error("Description 必须写明边界：语义判断属于 Critic，不属于本工具")
	}
}
