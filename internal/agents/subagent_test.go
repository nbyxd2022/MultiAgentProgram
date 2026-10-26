package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"multiagent/internal/schema"
	"multiagent/internal/state"
)

// ─── 测试替身 ───────────────────────────────────────────────────────

type fakeSubAgent struct {
	name  string
	kind  schema.Kind
	needs []schema.Kind

	// 让它"产出错种类"的产物，用来测双向校验
	produceOtherKind bool
	failWith         error

	runs     int
	gotInput SubAgentInput
}

func (f *fakeSubAgent) Name() string      { return f.name }
func (f *fakeSubAgent) Kind() schema.Kind { return f.kind }
func (f *fakeSubAgent) Description() string {
	return "测试用角色：只负责产出可预测的结果"
}
func (f *fakeSubAgent) Needs() []schema.Kind { return f.needs }

func (f *fakeSubAgent) Run(_ context.Context, in SubAgentInput) (schema.Payload, error) {
	f.runs++
	f.gotInput = in

	if f.failWith != nil {
		return nil, f.failWith
	}
	if f.produceOtherKind {
		return schema.Research{Topic: "装错的东西"}, nil
	}

	// 把"我看到了哪些引用"写进产物 —— 用来证明 SubAgent 拿到的是引用而不是正文
	refs := make([]string, 0, len(in.Inputs))
	for _, h := range in.Inputs {
		refs = append(refs, h.Ref)
	}

	// 默认返回与自身 kind 匹配的产物（适配器会做双向校验）
	if f.kind == schema.KindResearch {
		return schema.Research{
			Topic:    in.Topic,
			Queries:  []string{"q1"},
			Findings: []schema.Finding{{Claim: secretClaim, Source: "web_search#1"}},
		}, nil
	}
	return schema.Outline{Pages: []schema.PageOutline{
		{Index: 0, Title: "封面", Points: append(refs, "看到的上游引用")},
	}}, nil
}

func newOutlineAgent(needs ...schema.Kind) *fakeSubAgent {
	return &fakeSubAgent{name: "outline", kind: schema.KindOutline, needs: needs}
}

func newResearchAgent() *fakeSubAgent {
	return &fakeSubAgent{name: "research", kind: schema.KindResearch}
}

// secretClaim 是只应存在于 **payload 正文** 里的一段内容。
//
// 用它来验证：摘要与工具回执里都不该出现它（原则四）。
//
// 注意别拿 Topic 当标记 —— 摘要是**允许**带 Topic 的：
// 那是"识别信息"（让人认出这是哪一份），不是正文。
const secretClaim = "绝密正文：渗透率 35%，成本下降 40%"

func seedResearch(t *testing.T, b *state.Blackboard, topic string) schema.Artifact {
	t.Helper()
	a, err := schema.NewArtifact(string(schema.KindResearch), "research", schema.Research{
		Topic:    topic,
		Queries:  []string{"q1"},
		Findings: []schema.Finding{{Claim: secretClaim, Source: "web_search#1"}},
	})
	if err != nil {
		t.Fatalf("构造上游产物失败: %v", err)
	}
	stored, err := b.Put(a)
	if err != nil {
		t.Fatalf("上游产物入库失败: %v", err)
	}
	return stored
}

// ─── 依赖检查：顺序约束由代码强制 ───────────────────────────────────

func TestSubAgentToolRejectsWhenDependencyMissing(t *testing.T) {
	board := state.NewBlackboard()
	sub := newOutlineAgent(schema.KindResearch)

	tl, err := NewSubAgentTool(sub, board, "主题")
	if err != nil {
		t.Fatalf("构造工具失败: %v", err)
	}

	_, err = tl.Execute(context.Background(), `{}`)
	if err == nil {
		t.Fatal("上游缺失时调用必须被拒绝 —— 这就是「顺序约束由代码强制」")
	}
	if !strings.Contains(err.Error(), "缺少 research") {
		t.Errorf("错误信息必须说清还缺什么，实际 %q", err.Error())
	}
	if sub.runs != 0 {
		t.Error("前置条件不满足时，SubAgent 根本不该被调用")
	}
	if board.Versions(string(schema.KindOutline)) != 0 {
		t.Error("被拒绝的调用不应产出任何产物")
	}
}

func TestSubAgentToolRunsWhenDependenciesReady(t *testing.T) {
	board := state.NewBlackboard()
	seedResearch(t, board, "新能源汽车")
	sub := newOutlineAgent(schema.KindResearch)

	tl, _ := NewSubAgentTool(sub, board, "新能源汽车")
	raw, err := tl.Execute(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("依赖就绪时应能执行: %v", err)
	}

	var out struct {
		OK      bool   `json:"ok"`
		Ref     string `json:"ref"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("回执不是合法 JSON: %v\n%s", err, raw)
	}

	if !out.OK || out.Ref != "outline@1" {
		t.Errorf("回执异常: %+v", out)
	}
	if out.Summary == "" {
		t.Error("回执必须带摘要（编排者靠它决定要不要取全文）")
	}
	if board.Versions("outline") != 1 {
		t.Error("产物应当已入黑板")
	}
}

// ─── 零搬运：SubAgent 只拿到引用与摘要 ──────────────────────────────

func TestSubAgentReceivesRefsNotPayload(t *testing.T) {
	board := state.NewBlackboard()
	seedResearch(t, board, "新能源汽车行业趋势")

	sub := newOutlineAgent(schema.KindResearch)
	tl, _ := NewSubAgentTool(sub, board, "新能源汽车行业趋势")
	if _, err := tl.Execute(context.Background(), `{}`); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if len(sub.gotInput.Inputs) != 1 {
		t.Fatalf("应收到 1 条上游引用，实际 %d", len(sub.gotInput.Inputs))
	}
	h := sub.gotInput.Inputs[0]
	if h.Ref != "research@1" {
		t.Errorf("引用应带版本，实际 %q", h.Ref)
	}
	if h.Summary == "" {
		t.Error("handoff 必须带摘要")
	}
	// 上游正文没有跟着进来 —— 这就是"零数据搬运"
	if strings.Contains(h.Summary, secretClaim) {
		t.Errorf("摘要里不应出现正文：%q", h.Summary)
	}
}

func TestToolOutputCarriesNoPayload(t *testing.T) {
	board := state.NewBlackboard()
	seedResearch(t, board, "新能源汽车行业趋势")

	sub := newOutlineAgent(schema.KindResearch)
	tl, _ := NewSubAgentTool(sub, board, "新能源汽车行业趋势")
	raw, err := tl.Execute(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	// 工具回执是要进编排者上下文的 —— 上游正文绝不能跟着走（原则四）
	if strings.Contains(raw, secretClaim) {
		t.Errorf("工具回执里出现了上游正文: %s", raw)
	}
}

func TestSubAgentCanFetchFullPayloadOnDemand(t *testing.T) {
	board := state.NewBlackboard()
	seedResearch(t, board, "新能源汽车行业趋势")

	sub := newOutlineAgent(schema.KindResearch)
	tl, _ := NewSubAgentTool(sub, board, "新能源汽车行业趋势")
	if _, err := tl.Execute(context.Background(), `{}`); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	// 摘要是"形状"，全文要**按需去取**，而且取得到
	fetched, err := sub.gotInput.Fetch()
	if err != nil {
		t.Fatalf("按需取回失败: %v", err)
	}
	if len(fetched) != 1 {
		t.Fatalf("应取回 1 份产物，实际 %d", len(fetched))
	}
	got, err := schema.Decode[schema.Research](fetched[0])
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if got.Findings[0].Claim != secretClaim {
		t.Errorf("取回的全文里应当有正文，实际 %q", got.Findings[0].Claim)
	}
}

// ─── 版本：一个角色一个 key，多次执行只是多版本 ─────────────────────

func TestRepeatedRunsCreateNewVersions(t *testing.T) {
	board := state.NewBlackboard()
	seedResearch(t, board, "主题")
	sub := newOutlineAgent(schema.KindResearch)

	tl, _ := NewSubAgentTool(sub, board, "主题")

	var refs []string
	for i := 0; i < 2; i++ {
		raw, err := tl.Execute(context.Background(), `{}`)
		if err != nil {
			t.Fatalf("第 %d 次执行失败: %v", i+1, err)
		}
		var out struct {
			Ref string `json:"ref"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		refs = append(refs, out.Ref)
	}

	if refs[0] != "outline@1" || refs[1] != "outline@2" {
		t.Errorf("重复执行应产生新版本（而不是覆盖），实际 %v", refs)
	}
	if board.Versions("outline") != 2 {
		t.Error("旧版本必须仍在（这是引用不可变的地基）")
	}
}

// ─── hint：指令可以传，数据不能传 ───────────────────────────────────

func TestHintIsForwarded(t *testing.T) {
	board := state.NewBlackboard()
	seedResearch(t, board, "主题")
	sub := newOutlineAgent(schema.KindResearch)

	tl, _ := NewSubAgentTool(sub, board, "主题")
	if _, err := tl.Execute(context.Background(), `{"hint":"  只讲 3 条  "}`); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if sub.gotInput.Hint != "只讲 3 条" {
		t.Errorf("hint 应被透传并去掉首尾空白，实际 %q", sub.gotInput.Hint)
	}
	if sub.gotInput.Topic != "主题" {
		t.Errorf("主题必须每跳都带（它是用户硬约束的一部分），实际 %q", sub.gotInput.Topic)
	}
}

// ─── 双向校验：声明的 kind 与产出的 kind 必须一致 ───────────────────

func TestToolRejectsKindMismatch(t *testing.T) {
	board := state.NewBlackboard()
	seedResearch(t, board, "主题")

	sub := newOutlineAgent(schema.KindResearch)
	sub.produceOtherKind = true // 声明 outline，却产出 research

	tl, _ := NewSubAgentTool(sub, board, "主题")
	_, err := tl.Execute(context.Background(), `{}`)
	if err == nil {
		t.Fatal("产物 kind 与角色声明的 kind 不一致时必须报错（否则黑板会错位）")
	}
	if !strings.Contains(err.Error(), "不一致") {
		t.Errorf("错误信息应说明不一致，实际 %q", err.Error())
	}
}

func TestToolPropagatesSubAgentFailure(t *testing.T) {
	board := state.NewBlackboard()
	seedResearch(t, board, "主题")

	sub := newOutlineAgent(schema.KindResearch)
	sub.failWith = fmt.Errorf("模型调用失败")

	tl, _ := NewSubAgentTool(sub, board, "主题")
	if _, err := tl.Execute(context.Background(), `{}`); err == nil {
		t.Fatal("SubAgent 失败必须向上传播（由编排者决定降级还是终止）")
	}
	if board.Versions("outline") != 0 {
		t.Error("失败时不应产生产物")
	}
}

// ─── 工具契约 ───────────────────────────────────────────────────────

func TestDefinitionDeclaresPrecondition(t *testing.T) {
	board := state.NewBlackboard()
	tl, _ := NewSubAgentTool(newOutlineAgent(schema.KindResearch), board, "主题")

	def := tl.Definition()

	if def.Name != "outline" {
		t.Errorf("工具名应等于 Kind（路由的合法值集合就是工具名集合），实际 %q", def.Name)
	}
	if !strings.Contains(def.Description, "前置条件") || !strings.Contains(def.Description, "research") {
		t.Errorf("描述里必须写明前置条件，实际 %q", def.Description)
	}
	if len(def.Parameters) != 1 || def.Parameters[0].Name != "hint" {
		t.Errorf("工具参数只应有 hint（上游数据由代码装配，编排者无处可搬），实际 %+v", def.Parameters)
	}
}

func TestNewSubAgentToolValidates(t *testing.T) {
	board := state.NewBlackboard()

	bad := &fakeSubAgent{name: "x", kind: "whatever"}
	if _, err := NewSubAgentTool(bad, board, "主题"); err == nil {
		t.Error("非法 Kind 必须在构造时就被拒绝（错误要靠近它的来源）")
	}
	if _, err := NewSubAgentTool(newOutlineAgent(), nil, "主题"); err == nil {
		t.Error("缺少黑板必须在构造时被拒绝")
	}
	if _, err := NewSubAgentTool(nil, board, "主题"); err == nil {
		t.Error("空 SubAgent 必须被拒绝")
	}
}

func TestExecuteRejectsBadJSON(t *testing.T) {
	board := state.NewBlackboard()
	seedResearch(t, board, "主题")
	tl, _ := NewSubAgentTool(newOutlineAgent(schema.KindResearch), board, "主题")

	if _, err := tl.Execute(context.Background(), `{"hint":`); err == nil {
		t.Fatal("非法 JSON 必须返回 error，走 Agent 的回灌通道自愈")
	}
}
