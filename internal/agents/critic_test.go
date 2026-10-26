package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"multiagent/internal/llm"
	"multiagent/internal/message"
	"multiagent/internal/schema"
	"multiagent/internal/tools"
)

// ─── 测试夹具 ───────────────────────────────────────────────────────

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

// submitStep 模拟"模型调用 submit_review 提交结论"。
func submitStep(args string) llm.Step {
	return llm.Step{
		Content: "我来评审。",
		ToolCalls: []message.ToolCall{{
			ID: "call_1", Name: "submit_review", Arguments: args,
		}},
	}
}

// issueArg 生成一条 issue 的 JSON。
func issueArg(level, field, msg, fix string) string {
	b, _ := json.Marshal(map[string]string{
		"level": level, "field": field, "message": msg, "fix": fix,
	})
	return string(b)
}

func reviewArgs(issues ...string) string {
	return `{"issues":[` + strings.Join(issues, ",") + `]}`
}

func find(issues []schema.Issue, src schema.Source, field string) *schema.Issue {
	for i := range issues {
		if issues[i].Source == src && issues[i].Field == field {
			return &issues[i]
		}
	}
	return nil
}

// ─── Review：硬判据 + 软判据 的合并 ─────────────────────────────────

func TestReviewMergesHardAndSoftIssues(t *testing.T) {
	slide := cleanSlide()
	slide.Bullets = slide.Bullets[:2] // 只留 2 条 → 硬判据必然报"少于下限"

	client := llm.NewScriptedClient("critic-test", "评审完毕。",
		submitStep(reviewArgs(
			issueArg("major", "bullets[0]", "要点空洞，只是口号", "补一个可量化指标"),
		)),
	)

	rep, err := NewCritic(CriticOptions{Client: client}).
		Review(context.Background(), slide, 2, "out/slide-x.html")
	if err != nil {
		t.Fatalf("评审不应报错: %v", err)
	}

	if rep.Round != 2 || rep.Subject != "out/slide-x.html" {
		t.Errorf("轮次/对象上下文未传递：round=%d subject=%q", rep.Round, rep.Subject)
	}

	// 硬判据：条数不足，来自 tools.Check
	if got := find(rep.Issues, schema.SourceRule, "bullets"); got == nil {
		t.Errorf("缺少硬判据问题（条数不足）：%v", rep.Lines())
	}

	// 软判据：模型提交的语义问题
	soft := find(rep.Issues, schema.SourceCritic, "bullets[0]")
	if soft == nil {
		t.Fatalf("缺少软判据问题：%v", rep.Lines())
	}
	if soft.Message != "要点空洞，只是口号" || soft.Fix != "补一个可量化指标" {
		t.Errorf("软判据内容被改写：%+v", *soft)
	}
	if soft.Level != schema.LevelMajor {
		t.Errorf("level 未正确解析：%q", soft.Level)
	}
}

// TestReviewWithoutSubmissionIsError 是本文件最重要的一条测试。
//
// 它守住一条底线：**"没发现问题"和"没做检查"必须区分**。
// 混为一谈时，一个根本没被检查过的产物会被当成合格品交付——
// 这是评审系统里最危险、也最难发现的 bug。
func TestReviewWithoutSubmissionIsError(t *testing.T) {
	// 剧本里没有任何工具调用：模型只是聊了两句就结束了。
	client := llm.NewScriptedClient("critic-test", "看起来不错，没有问题。")

	rep, err := NewCritic(CriticOptions{Client: client}).
		Review(context.Background(), cleanSlide(), 1, "s")
	if err == nil {
		t.Fatal("模型没有提交结论时必须报错，绝不能静默当作\"没问题\"")
	}
	if !strings.Contains(err.Error(), "评审未完成") {
		t.Errorf("错误信息应说明原因，实际 %q", err.Error())
	}
	// 即使评审失败，硬判据仍要交回调用方，便于"降级交付 + 说明"
	if rep.HasHardIssue() {
		t.Error("合格样本的硬判据里不应有硬问题")
	}

	// ★ L1-3 的回归断言：状态必须在**报告里**，不能只活在 error 里。
	// 否则调用方一旦忽略 error（我们之前就是这样），就会把这份报告判成可交付。
	if !rep.Incomplete {
		t.Error("评审未完成时，报告必须被标记为 Incomplete")
	}
	if rep.Deliverable() {
		t.Fatal("不完整的报告绝不能判成可交付——这正是 L1-3 那个 bug")
	}
}

func TestReviewAcceptsExplicitEmptyIssues(t *testing.T) {
	client := llm.NewScriptedClient("critic-test", "评审完毕。",
		submitStep(`{"issues":[]}`))

	rep, err := NewCritic(CriticOptions{Client: client}).
		Review(context.Background(), cleanSlide(), 1, "s")
	if err != nil {
		t.Fatalf("显式提交空数组是合法结论，不应报错: %v", err)
	}
	if len(rep.Issues) != 0 {
		t.Errorf("合格产物不应有任何问题，实际 %v", rep.Lines())
	}
}

func TestReviewWithoutClientReturnsError(t *testing.T) {
	rep, err := NewCritic(CriticOptions{}).
		Review(context.Background(), cleanSlide(), 1, "s")
	if err == nil {
		t.Fatal("未配置模型客户端时应报错")
	}
	if !rep.Incomplete || rep.Deliverable() {
		t.Error("连模型都没有时，报告必须标记为不完整且不可交付")
	}
}

// ─── 上下文注入：硬判据必须随请求一起给模型 ─────────────────────────

func TestBuildReviewPromptCarriesHardIssues(t *testing.T) {
	slide := cleanSlide()
	slide.Bullets = slide.Bullets[:2]
	hard := tools.Check(slide)

	prompt := buildReviewPrompt(slide, hard)

	if !strings.Contains(prompt, slide.Title) {
		t.Error("Prompt 里必须带上被评审产物本身")
	}
	if !strings.Contains(prompt, "少于下限") {
		t.Error("硬判据结论必须注入上下文——否则 Critic 会把它重复报一遍，白白浪费一轮返工")
	}
	if !strings.Contains(prompt, "不要重复报告") {
		t.Error("必须明确告诉 Critic 不要重复报告硬判据")
	}
}

// ─── submit_review 工具本身 ────────────────────────────────────────

func TestSubmitReviewReplacesInsteadOfAccumulating(t *testing.T) {
	col := &reviewCollector{}
	submit := newSubmitReview(col)
	ctx := context.Background()

	if _, err := submit.Execute(ctx, reviewArgs(
		issueArg("major", "bullets[0]", "第一次", "改"),
		issueArg("suggestion", "bullets[1]", "第一次", "改"),
	)); err != nil {
		t.Fatalf("首次提交不应报错: %v", err)
	}
	if _, err := submit.Execute(ctx, reviewArgs(
		issueArg("major", "bullets[2]", "第二次", "改"),
	)); err != nil {
		t.Fatalf("二次提交不应报错: %v", err)
	}

	if len(col.issues) != 1 {
		t.Fatalf("二次提交必须**替换**而非累加：期望 1 条，实际 %d 条（累加会让分数虚高、返工永不收敛）",
			len(col.issues))
	}
	if col.issues[0].Message != "第二次" {
		t.Errorf("应保留最新结论，实际 %q", col.issues[0].Message)
	}
	if !col.submitted {
		t.Error("提交后 submitted 应为 true")
	}
}

func TestSubmitReviewRejectsInvalidLevel(t *testing.T) {
	col := &reviewCollector{}

	_, err := newSubmitReview(col).Execute(context.Background(),
		reviewArgs(issueArg("high", "bullets[0]", "问题", "改法")))
	if err == nil {
		t.Fatal("非法 level 必须被拒绝：Weight() 会把它算成 0，问题被静默洗掉")
	}
	if col.submitted {
		t.Error("非法提交不应被记录")
	}
}

func TestSubmitReviewRejectsBlockerLevel(t *testing.T) {
	col := &reviewCollector{}

	_, err := newSubmitReview(col).Execute(context.Background(),
		reviewArgs(issueArg("blocker", "title", "标题不行", "换一个")))
	if err == nil {
		t.Fatal("blocker 由硬判据说了算，不允许模型自行定级")
	}
}

func TestSubmitReviewRequiresMessageAndFix(t *testing.T) {
	cases := []struct{ name, arg string }{
		{"message 为空", issueArg("major", "bullets[0]", "", "改法")},
		{"fix 为空", issueArg("major", "bullets[0]", "问题", "")},
	}
	for _, c := range cases {
		col := &reviewCollector{}
		if _, err := newSubmitReview(col).Execute(context.Background(), reviewArgs(c.arg)); err == nil {
			t.Errorf("%s：应当被拒绝（没有 message/fix 的问题对模型没用）", c.name)
		}
	}
}

func TestSubmitReviewRejectsBadJSON(t *testing.T) {
	col := &reviewCollector{}
	if _, err := newSubmitReview(col).Execute(context.Background(), `{"issues":`); err == nil {
		t.Fatal("非法 JSON 必须返回 error，走 Agent 的回灌通道自愈")
	}
}
