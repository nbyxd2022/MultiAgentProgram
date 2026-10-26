package agents

import (
	"context"
	"strings"
	"testing"

	"multiagent/internal/llm"
	"multiagent/internal/schema"
	"multiagent/internal/state"
)

// 这个文件守住 **L2-9 接真实模型时暴露的两件事**：
//
//	① **预算必须告诉模型** —— 它不知道步数上限，就只能一直搜到撞墙；
//	② **触顶 ≠ 失败** —— 已经提交过的结论就是有效成果，
//	   丢掉它重跑一遍（还要重新搜索）是纯粹的浪费。
//
// 两条都是 L1 那句"超限 ≠ 失败，应当降级交付"的下沉 ——
// 只不过这次落在 `Run` 这一层，而不是编排层。

func TestBudgetNoticeCarriesTheNumber(t *testing.T) {
	if got := BudgetNotice(7); !strings.Contains(got, "7") {
		t.Errorf("预算数字必须写进提示（否则模型无法为它规划）：%q", got)
	}

	// 各角色的输入里都应当带上它 —— 漏一个，那个角色就会去撞墙。
	in := SubAgentInput{Topic: "主题"}

	if p := buildResearchPrompt(in, 10); !strings.Contains(p, "最多只能调用 10 次工具") {
		t.Errorf("research 的输入里必须写明预算：\n%s", p)
	}

	board := state.NewBlackboard()

	// outline 的输入是 research 产物
	ra, err := schema.NewArtifact(string(schema.KindResearch), "research", schema.Research{
		Topic:    "主题",
		Findings: []schema.Finding{{Claim: "渗透率 35%", Source: "web_search#1"}},
	})
	if err != nil {
		t.Fatalf("构造上游失败: %v", err)
	}
	storedResearch, _ := board.Put(ra)
	withResearch := SubAgentInput{
		Topic:    "主题",
		Inputs:   []schema.Handoff{schema.HandoffOf(storedResearch)},
		Upstream: board,
	}

	if p, err := buildOutlinePrompt(withResearch, 8); err != nil {
		t.Fatalf("构造 Prompt 失败: %v", err)
	} else if !strings.Contains(p, "最多只能调用 8 次工具") {
		t.Errorf("outline 的输入里必须写明预算：\n%s", p)
	}

	// designer 的输入是 outline 产物
	oa, err := schema.NewArtifact(string(schema.KindOutline), "outline", schema.Outline{
		Pages: []schema.PageOutline{
			{Index: 0, Title: "甲", Points: []string{"p"}},
			{Index: 1, Title: "乙", Points: []string{"p", "q", "r"}},
			{Index: 2, Title: "丙", Points: []string{"p"}},
		},
	})
	if err != nil {
		t.Fatalf("构造上游失败: %v", err)
	}
	storedOutline, _ := board.Put(oa)
	withOutline := SubAgentInput{
		Topic:    "主题",
		Inputs:   []schema.Handoff{schema.HandoffOf(storedOutline)},
		Upstream: board,
	}

	outline, ref, err := loadOutline(withOutline)
	if err != nil {
		t.Fatalf("取上游失败: %v", err)
	}
	if p, err := buildDesignPrompt(withOutline, outline, ref, 8); err != nil {
		t.Fatalf("构造 Prompt 失败: %v", err)
	} else if !strings.Contains(p, "最多只能调用 8 次工具") {
		t.Errorf("designer 的输入里必须写明预算：\n%s", p)
	}
}

// exhaustScript 造一段"先成功提交，然后一直调工具把步数用光"的剧本。
func exhaustScript(submitTool, submitArgs string) []llm.Step {
	steps := []llm.Step{callStep(submitTool, submitArgs)}
	for i := 0; i < 6; i++ {
		steps = append(steps, callStep("web_search", `{"query":"再搜一下"}`))
	}
	return steps
}

func TestResearchKeepsProductWhenBudgetExhausted(t *testing.T) {
	client := llm.NewScriptedClient("m", "done",
		exhaustScript("submit_research", `{"findings":[{"claim":"渗透率 35%","source":"web_search#1"}]}`)...)

	r, err := NewResearchAgent(ResearchOptions{Client: client, MaxSteps: 2})
	if err != nil {
		t.Fatalf("构造研究员失败: %v", err)
	}

	product, err := r.Run(context.Background(), SubAgentInput{
		Topic: "主题", Upstream: state.NewBlackboard(),
	})
	if err != nil {
		t.Fatalf("它已经提交过结论了，触顶不该把成果一起丢掉：%v", err)
	}

	got, ok := product.(schema.Research)
	if !ok {
		t.Fatalf("产物类型不对：%T", product)
	}
	if len(got.Findings) != 1 || got.Findings[0].Claim != "渗透率 35%" {
		t.Errorf("应当拿到提交过的那份结论，实际 %+v", got)
	}
}

// TestBudgetExhaustedWithoutSubmitStillFails 是上面那条的**反向约束**。
//
// 没有它，一个"无论触顶还是真失败都返回空产物"的实现也是绿的 ——
// 而那会把"什么都没做"当成"做完了"（"存在 ≠ 完整"的又一个变体）。
func TestBudgetExhaustedWithoutSubmitStillFails(t *testing.T) {
	steps := make([]llm.Step, 0, 6)
	for i := 0; i < 6; i++ {
		steps = append(steps, callStep("web_search", `{"query":"再搜一下"}`))
	}
	client := llm.NewScriptedClient("m", "done", steps...)

	r, err := NewResearchAgent(ResearchOptions{Client: client, MaxSteps: 2})
	if err != nil {
		t.Fatalf("构造研究员失败: %v", err)
	}

	_, err = r.Run(context.Background(), SubAgentInput{
		Topic: "主题", Upstream: state.NewBlackboard(),
	})
	if err == nil {
		t.Fatal("从来没提交过结论，触顶就是失败 —— 不能返回一个空产物充数")
	}
	if !strings.Contains(err.Error(), "最大步数") {
		t.Errorf("错误信息应当说明是触顶：%v", err)
	}
}
