package agents

import (
	"context"
	"strings"
	"testing"

	"multiagent/internal/llm"
	"multiagent/internal/schema"
	"multiagent/internal/tool"
)

// recordingTool 是一个可注入的检索替身，用来证明"依赖是注入进去的"（原则五）。
type recordingTool struct{ calls int }

func (t *recordingTool) Definition() tool.Definition {
	return tool.Definition{Name: "web_search", Description: "测试用检索"}
}
func (t *recordingTool) Execute(_ context.Context, _ string) (string, error) {
	t.calls++
	return `{"ok":true,"results":[]}`, nil
}

func newResearch(t *testing.T, client llm.Client, search tool.Tool) *ResearchAgent {
	t.Helper()
	r, err := NewResearchAgent(ResearchOptions{Client: client, Search: search})
	if err != nil {
		t.Fatalf("构造研究员失败: %v", err)
	}
	return r
}

// ─── 角色身份 ───────────────────────────────────────────────────────

func TestResearchAgentIdentity(t *testing.T) {
	r := newResearch(t, llm.NewScriptedClient("x", ""), nil)

	if r.Name() != "research" {
		t.Errorf("角色名不对: %q", r.Name())
	}
	if r.Kind() != schema.KindResearch {
		t.Errorf("产物种类不对: %q", r.Kind())
	}
	if len(r.Needs()) != 0 {
		t.Errorf("研究员不应依赖任何上游产物，实际 %v", r.Needs())
	}
	if strings.TrimSpace(r.Description()) == "" {
		t.Error("必须给编排者一句'什么时候该调我'")
	}
}

func TestNewResearchAgentValidates(t *testing.T) {
	if _, err := NewResearchAgent(ResearchOptions{}); err == nil {
		t.Fatal("未配置模型客户端时应在构造期报错")
	}
}

// ─── 正常路径：检索 → 提交 ──────────────────────────────────────────

func TestResearchRunsSearchThenSubmits(t *testing.T) {
	search := &recordingTool{}
	client := llm.NewScriptedClient("research", "完成。",
		callStep("web_search", `{"query":"新能源汽车 渗透率"}`),
		callStep("submit_research", `{"queries":["新能源汽车 渗透率"],"findings":[
			{"claim":"渗透率已达 35%","source":"web_search#1"},
			{"claim":"电池成本两年下降 40%","source":"web_search#2"}
		]}`),
		textStep("完成。"),
	)

	r := newResearch(t, client, search)
	product, err := r.Run(context.Background(), SubAgentInput{Topic: "新能源汽车行业趋势"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if search.calls != 1 {
		t.Errorf("注入的检索工具应当被真正调用，实际 %d 次", search.calls)
	}

	got, ok := product.(schema.Research)
	if !ok {
		t.Fatalf("产物类型不对: %T", product)
	}
	// Topic 来自**输入**，不是模型产出的 —— 不给它写错的机会
	if got.Topic != "新能源汽车行业趋势" {
		t.Errorf("主题应取自输入，实际 %q", got.Topic)
	}
	if len(got.Findings) != 2 {
		t.Fatalf("应有 2 条发现，实际 %d", len(got.Findings))
	}
	if got.Findings[0].Claim != "渗透率已达 35%" || got.Findings[0].Source != "web_search#1" {
		t.Errorf("发现内容不对: %+v", got.Findings[0])
	}
	if len(got.Queries) != 1 {
		t.Errorf("查询词应被保留（便于复现），实际 %v", got.Queries)
	}
	if r.Stats().PromptTokens == 0 {
		t.Error("用量统计应当被记录")
	}
}

// ─── L1-5 的落点：无来源的断言进不来 ────────────────────────────────

func TestResearchRejectsFindingWithoutSource(t *testing.T) {
	client := llm.NewScriptedClient("research", "完成。",
		// 第 1 次：少写 source → 应当被拒绝
		callStep("submit_research", `{"findings":[{"claim":"没有来源的说法"}]}`),
		// 第 2 次：补上 source → 通过（这就是回灌自愈）
		callStep("submit_research", `{"findings":[{"claim":"有来源的说法","source":"web_search#3"}]}`),
		textStep("完成。"),
	)

	r := newResearch(t, client, nil)
	product, err := r.Run(context.Background(), SubAgentInput{Topic: "主题"})
	if err != nil {
		t.Fatalf("回灌自愈后应当成功: %v", err)
	}

	got := product.(schema.Research)
	if len(got.Findings) != 1 {
		t.Fatalf("最终应只保留 1 条合规发现，实际 %d", len(got.Findings))
	}
	if got.Findings[0].Source != "web_search#3" {
		t.Errorf("留下的应当是有来源的那条，实际 %+v", got.Findings[0])
	}
}

func TestResearchAcceptsEmptyFindings(t *testing.T) {
	client := llm.NewScriptedClient("research", "完成。",
		callStep("submit_research", `{"findings":[]}`),
		textStep("没搜到可用素材。"),
	)

	r := newResearch(t, client, nil)
	product, err := r.Run(context.Background(), SubAgentInput{Topic: "主题"})
	if err != nil {
		t.Fatalf("'什么都没搜到'是合法结论，不应报错: %v", err)
	}
	if n := len(product.(schema.Research).Findings); n != 0 {
		t.Errorf("应为空，实际 %d 条", n)
	}
}

// ─── 底线：没提交 ≠ 没问题 ──────────────────────────────────────────

func TestResearchErrorsWhenNothingSubmitted(t *testing.T) {
	client := llm.NewScriptedClient("research", "我觉得差不多了。")

	r := newResearch(t, client, nil)
	_, err := r.Run(context.Background(), SubAgentInput{Topic: "主题"})
	if err == nil {
		t.Fatal("模型没调用提交口时必须报错 —— 不能把它当成'没有发现'")
	}
	if !strings.Contains(err.Error(), "没有调用") {
		t.Errorf("错误信息应说明原因，实际 %q", err.Error())
	}
}

func TestResearchPropagatesModelError(t *testing.T) {
	r := newResearch(t, &failingClient{}, nil)
	if _, err := r.Run(context.Background(), SubAgentInput{Topic: "主题"}); err == nil {
		t.Fatal("模型调用失败必须向上传播，由编排者决定降级还是终止")
	}
}

// ─── Prompt 纪律 ────────────────────────────────────────────────────

func TestResearchPromptRequiresSource(t *testing.T) {
	if !strings.Contains(researchPrompt, "source") {
		t.Error("Prompt 必须强调每条发现都要带 source")
	}
	if !strings.Contains(researchPrompt, "不要编造") {
		t.Error("Prompt 必须写明不许编造数据")
	}
}

func TestResearchPromptCarriesTopicAndHint(t *testing.T) {
	p := buildResearchPrompt(SubAgentInput{Topic: "新能源汽车", Hint: "只看国内市场"}, 6)

	if !strings.Contains(p, "新能源汽车") || !strings.Contains(p, "只看国内市场") {
		t.Errorf("主题与额外要求都要带上：\n%s", p)
	}
}
