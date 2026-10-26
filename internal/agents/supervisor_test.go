package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"multiagent/internal/llm"
	"multiagent/internal/message"
	"multiagent/internal/observ"
	"multiagent/internal/schema"
	"multiagent/internal/state"
)

// ─── 测试夹具 ───────────────────────────────────────────────────────

func callStep(name, args string) llm.Step {
	return llm.Step{
		Content:   "派活：" + name,
		ToolCalls: []message.ToolCall{{ID: "call_" + name, Name: name, Arguments: args}},
	}
}

func textStep(content string) llm.Step { return llm.Step{Content: content} }

// twoRoles 返回一份"研究 + 大纲"的最小角色表。
func twoRoles() []SubAgent {
	return []SubAgent{newResearchAgent(), newOutlineAgent(schema.KindResearch)}
}

func newSupervisor(t *testing.T, client llm.Client, subs []SubAgent, required []schema.Kind, obs observ.Tracer) (*Supervisor, *state.Blackboard) {
	t.Helper()
	board := state.NewBlackboard()
	s, err := NewSupervisor(SupervisorOptions{
		Client:    client,
		Board:     board,
		SubAgents: subs,
		Required:  required,
		Tracer:    obs,
	})
	if err != nil {
		t.Fatalf("构造编排者失败: %v", err)
	}
	return s, board
}

// ─── 构造期校验 ─────────────────────────────────────────────────────

func TestNewSupervisorValidates(t *testing.T) {
	board := state.NewBlackboard()

	cases := []struct {
		name string
		opt  SupervisorOptions
	}{
		{"缺少模型客户端", SupervisorOptions{Board: board, SubAgents: twoRoles()}},
		{"缺少黑板", SupervisorOptions{Client: llm.NewScriptedClient("x", ""), SubAgents: twoRoles()}},
		{"没有子 Agent", SupervisorOptions{Client: llm.NewScriptedClient("x", ""), Board: board}},
		{"角色 kind 非法", SupervisorOptions{
			Client: llm.NewScriptedClient("x", ""), Board: board,
			SubAgents: []SubAgent{&fakeSubAgent{name: "x", kind: "whatever"}},
		}},
		{"两个角色产同一种产物", SupervisorOptions{
			Client: llm.NewScriptedClient("x", ""), Board: board,
			SubAgents: []SubAgent{newResearchAgent(), newResearchAgent()},
		}},
		{"必需产物没有任何角色负责", SupervisorOptions{
			Client: llm.NewScriptedClient("x", ""), Board: board,
			SubAgents: twoRoles(),
			Required:  []schema.Kind{schema.KindRender},
		}},
	}

	for _, c := range cases {
		if _, err := NewSupervisor(c.opt); err == nil {
			t.Errorf("%s：应当在构造期就被拒绝", c.name)
		}
	}
}

// ─── 正常路径：一串派活，全部产出 ───────────────────────────────────

func TestSupervisorRoutesThroughAllRoles(t *testing.T) {
	client := llm.NewScriptedClient("sup", "全部完成。",
		callStep("research", `{}`),
		callStep("outline", `{"hint":"只讲 3 条"}`),
		textStep("全部完成。"),
	)
	s, board := newSupervisor(t, client, twoRoles(),
		[]schema.Kind{schema.KindResearch, schema.KindOutline}, nil)

	result, err := s.Run(context.Background(), "新能源汽车行业趋势")
	if err != nil {
		t.Fatalf("编排失败: %v", err)
	}

	if !result.Complete() {
		t.Fatalf("应当产出全部必需产物，仍缺 %v", result.Missing)
	}
	if result.Rounds != 1 {
		t.Errorf("一次就够，不该补轮，实际 %d 轮", result.Rounds)
	}

	// 产物在黑板里
	if board.Versions("research") != 1 || board.Versions("outline") != 1 {
		t.Errorf("黑板上的产物不对：research=%d outline=%d",
			board.Versions("research"), board.Versions("outline"))
	}

	// 结果按**角色声明顺序**给出引用（输出稳定，便于 diff 与回归）
	if len(result.Completed) != 2 {
		t.Fatalf("应报告 2 份产物，实际 %d", len(result.Completed))
	}
	if result.Completed[0].Ref != "research@1" || result.Completed[1].Ref != "outline@1" {
		t.Errorf("引用顺序/内容不对：%+v", result.Completed)
	}
	if result.Stats.PromptTokens == 0 {
		t.Error("用量统计必须被记录（L1-6 的教训：看不见的成本管不住）")
	}
}

// ─── 终止判断的确定性兜底 ───────────────────────────────────────────

func TestSupervisorRunsAnotherRoundWhenArtifactsMissing(t *testing.T) {
	// 第 1 轮模型直接说"我做完了"，什么都没产出。
	// 代码应当发现必需产物缺失，再补一轮。
	client := llm.NewScriptedClient("sup", "我做完了。",
		textStep("我做完了。"),
		callStep("research", `{}`),
		callStep("outline", `{}`),
		textStep("这次真的做完了。"),
	)
	s, _ := newSupervisor(t, client, twoRoles(),
		[]schema.Kind{schema.KindResearch, schema.KindOutline}, nil)

	result, err := s.Run(context.Background(), "主题")
	if err != nil {
		t.Fatalf("编排失败: %v", err)
	}

	if result.Rounds != 2 {
		t.Errorf("模型说完成但产物缺失时，必须再补一轮，实际 %d 轮", result.Rounds)
	}
	if !result.Complete() {
		t.Errorf("补轮后应当齐全，仍缺 %v", result.Missing)
	}
}

func TestSupervisorReportsMissingWithoutFailing(t *testing.T) {
	// 模型自始至终都不干活。
	client := llm.NewScriptedClient("sup", "做完了。", textStep("做完了。"))
	s, _ := newSupervisor(t, client, twoRoles(),
		[]schema.Kind{schema.KindResearch, schema.KindOutline}, nil)

	result, err := s.Run(context.Background(), "主题")

	// ★ 不齐全 ≠ 失败：调用方应"交付已完成的部分 + 说明还缺什么"（L1 的降级交付语义）
	if err != nil {
		t.Fatalf("产物不齐不应报错（降级交付），实际 %v", err)
	}
	if result.Complete() {
		t.Fatal("什么都没产出，不该报告为完成")
	}
	if len(result.Missing) != 2 {
		t.Errorf("应报出 2 项缺失，实际 %v", result.Missing)
	}
	if result.Rounds != supervisorRounds {
		t.Errorf("应把补齐轮数用满才放弃，实际 %d 轮", result.Rounds)
	}
}

// ─── 路由合法性：工具注册表天然挡住非法名字 ─────────────────────────

func TestSupervisorRejectsUnknownToolName(t *testing.T) {
	var events []observ.Event
	obs := observ.TracerFunc(func(e observ.Event) { events = append(events, e) })

	client := llm.NewScriptedClient("sup", "完成。",
		callStep("hack_the_blackboard", `{}`), // ← 不存在的角色
		callStep("research", `{}`),
		callStep("outline", `{}`),
		textStep("完成。"),
	)
	s, board := newSupervisor(t, client, twoRoles(),
		[]schema.Kind{schema.KindResearch, schema.KindOutline}, obs)

	result, err := s.Run(context.Background(), "主题")
	if err != nil {
		t.Fatalf("非法路由应被回灌自愈，而不是让整个任务失败: %v", err)
	}
	if !result.Complete() {
		t.Errorf("回灌后应当继续完成，仍缺 %v", result.Missing)
	}

	// 出错被记进了事件流（可观测），而不是静默消失
	var sawUnknown bool
	for _, e := range events {
		if e.Kind == observ.KindToolResult && strings.Contains(e.Detail, "未知工具") {
			sawUnknown = true
		}
	}
	if !sawUnknown {
		t.Error("非法工具名必须产生一条可观测的 tool_result（回灌），否则无从排查")
	}
	if board.Versions("hack_the_blackboard") != 0 {
		t.Error("非法角色不该在黑板上留下任何东西")
	}
}

// ─── 错误传播 ───────────────────────────────────────────────────────

func TestSupervisorPropagatesModelError(t *testing.T) {
	s, _ := newSupervisor(t, &failingClient{}, twoRoles(),
		[]schema.Kind{schema.KindResearch}, nil)

	if _, err := s.Run(context.Background(), "主题"); err == nil {
		t.Fatal("模型调用失败必须向上传播")
	}
}

type failingClient struct{}

func (f *failingClient) Generate(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, errors.New("模型服务不可用")
}

// ─── Prompt：可检验的两条纪律 ───────────────────────────────────────

// TestSupervisorPromptKeepsBusinessFieldsOut 是一条**可执行的**架构检查。
//
// 编排者的 Prompt 里一旦出现业务字段名，就说明它正在做"逐字段提取重组" ——
// 也就是企业仓库 A2/A3 那个坑。这条测试让它不可能悄悄发生。
func TestSupervisorPromptKeepsBusinessFieldsOut(t *testing.T) {
	p := supervisorPrompt([]schema.Kind{schema.KindResearch, schema.KindOutline})
	lower := strings.ToLower(p)

	for _, banned := range []string{"title", "bullets", "points", "payload", "claims", "files"} {
		if strings.Contains(lower, banned) {
			t.Errorf("编排者 Prompt 里出现了业务字段名 %q —— 职责已越界（A3）：\n%s", banned, p)
		}
	}

	// 但"这次要产出什么"必须写清楚（那是目标，不是数据）
	for _, k := range []string{"research", "outline"} {
		if !strings.Contains(p, k) {
			t.Errorf("必须写明必需产出 %q：\n%s", k, p)
		}
	}
	if !strings.Contains(p, "正文不在你这里") {
		t.Errorf("必须从第一天就告诉它：正文不经过它（否则它总想搬运）：\n%s", p)
	}
}

// TestSupervisorCannotDuplicateRoleDescriptions 记录一个**结构上**的保证。
//
// supervisorPrompt 的签名里根本没有 SubAgent —— 它拿不到角色描述，
// 因此"描述写两遍、然后两边漂移"这件事在结构上就不可能发生。
// （与 NewArtifact 不接受 kind 参数、Put 不接受 version 是同一个手法。）
func TestSupervisorCannotDuplicateRoleDescriptions(t *testing.T) {
	sub := &fakeSubAgent{name: "outline", kind: schema.KindOutline}

	if strings.Contains(supervisorPrompt([]schema.Kind{sub.Kind()}), sub.Description()) {
		t.Error("角色描述只应在工具清单里出现一次（单一真相来源）")
	}
}
