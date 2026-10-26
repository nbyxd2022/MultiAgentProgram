package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"multiagent/internal/agent"
	"multiagent/internal/budget"
	"multiagent/internal/llm"
	"multiagent/internal/llmjson"
	"multiagent/internal/observ"
	"multiagent/internal/schema"
	"multiagent/internal/tool"
	"multiagent/internal/tools"
)

// ResearchAgent 是四个角色里的第一个：**找素材**。
//
// 它的两个特殊之处：
//
//	① 它是唯一**必须联网**的角色 —— 所以它真的带工具（web_search）；
//	② 它是 **L1-5（断言无来源锚点）** 在本项目的落点：
//	   **每条发现都必须带 source，否则提交会被直接拒绝。**
//
// 第 ② 条值得展开：我们不是"在 Prompt 里请求它带上来源"，而是**让不带来源的提交根本进不来**。
// 这就把"要求"变成了"约束"（原则一）。
type ResearchAgent struct {
	client   llm.Client
	search   tool.Tool
	maxSteps int
	tracer   observ.Tracer

	// guard 是预算守卫（与 Runner 同一个对象）。
	// ⚠️ 必须**传到** `agent.Options` —— 否则字段在、连接没有（R54 的教训）。
	guard budget.Guard

	// totalStats 是**累计**用量（跨多次 Run 累加）。
	// 口径与理由见 `subagent.go` 的 `StatsReporter` —— 那不是"随便选的"，
	// 而是 L2-3 那个"成本少算"bug 的修复点。
	totalStats agent.Stats
}

// ResearchOptions 是构造研究员的参数。
type ResearchOptions struct {
	Client llm.Client

	// Search 是检索工具。留空则使用内置的离线检索实现。
	//
	// 做成可注入，是为了"端口-适配器"（原则五）：
	// 换真实搜索服务、或在测试里换一个确定的替身，都不必动这个角色一行。
	Search tool.Tool

	MaxSteps int
	Tracer   observ.Tracer

	// Guard 是预算守卫（可以为 nil -> 归一成 Unlimited）。
	// 它该与 RunnerOptions.Guard 是同一个对象（见 agent.Options.Guard）。
	Guard budget.Guard
}

// NewResearchAgent 构造研究员。
func NewResearchAgent(opt ResearchOptions) (*ResearchAgent, error) {
	if opt.Client == nil {
		return nil, fmt.Errorf("research: 未配置模型客户端")
	}
	if opt.Search == nil {
		opt.Search = tools.NewWebSearch() // 默认是离线 Mock 数据
	}
	if opt.MaxSteps <= 0 {
		// 10 而不是 6：**真实模型会搜很多次**（实测 6 步不够 —— L2-9）。
		// 但"调大"只是给余量，真正的解法是下面把预算**告诉模型**（BudgetNotice）。
		opt.MaxSteps = 10
	}
	return &ResearchAgent{
		client:   opt.Client,
		search:   opt.Search,
		maxSteps: opt.MaxSteps,
		tracer:   opt.Tracer,
		guard:    opt.Guard,
	}, nil
}

// Name 实现 SubAgent 接口。
func (r *ResearchAgent) Name() string { return "research" }

// Kind 实现 SubAgent 接口。
func (r *ResearchAgent) Kind() schema.Kind { return schema.KindResearch }

// Description 告诉编排者"什么时候该调我"。
//
// 只描述**能力与边界**，不描述上游数据长什么样 —— 后者由 Needs 表达（代码可检查）。
func (r *ResearchAgent) Description() string {
	return "资料研究员：围绕主题联网检索，产出**带来源的**事实与数据。" +
		"当需要外部素材（数据、事件、行业情况）时调用它。不需要任何前置产物。"
}

// Needs 实现 SubAgent 接口：研究员不依赖任何上游产物。
//
// 返回 nil 而不是空切片：语义上"没有依赖"，用 nil 表达最清楚。
func (r *ResearchAgent) Needs() []schema.Kind { return nil }

// Stats 返回本角色实例的**累计**用量（实现 agents.StatsReporter）。
func (r *ResearchAgent) Stats() agent.Stats { return r.totalStats }

// Run 实现 SubAgent 接口。
func (r *ResearchAgent) Run(ctx context.Context, in SubAgentInput) (schema.Payload, error) {
	submit, err := NewSubmitTool(researchSpec(in.Topic))
	if err != nil {
		return nil, err
	}

	registry := tool.NewRegistry()
	registry.Register(r.search)
	registry.Register(submit)

	// 复用 L0 的运行时：ReAct 循环、终止双保险、错误回灌，全是现成的。
	a := agent.New(agent.Options{
		Name:         r.Name(),
		Description:  r.Description(),
		SystemPrompt: researchPrompt,
		Client:       r.client,
		Tools:        registry,
		MaxSteps:     r.maxSteps,
		Tracer:       r.tracer,
		Guard:        r.guard,
	})

	if _, err := a.Run(ctx, buildResearchPrompt(in, r.maxSteps)); err != nil {
		r.totalStats = addStats(r.totalStats, a.Stats())

		// ⚠️ **触顶 ≠ 失败** —— L1 那条结论，下沉到 `Run` 这一层。
		//
		// 如果它其实**已经提交过**结论（只是提交之后还在继续调工具，直到步数用光），
		// 那份结论就是有效成果。丢掉它重跑一遍是纯粹的浪费 ——
		// 而且重跑要重新搜索，成本更高。
		if submit.Submitted() {
			return submit.Product(), nil
		}
		return nil, fmt.Errorf("research: %w", err)
	}
	r.totalStats = addStats(r.totalStats, a.Stats())

	// 与 Critic 同一条底线：**没提交 ≠ 没问题**。
	if !submit.Submitted() {
		return nil, fmt.Errorf("research: 模型没有调用 %s，无法判定产出", submit.Definition().Name)
	}
	return submit.Product(), nil
}

// ─── 提交契约 ───────────────────────────────────────────────────────

// researchSpec 声明"研究结论长什么样"。
//
// Topic 由闭包捕获：它是**输入**，不是模型该产出的东西 ——
// 让模型去重复一遍输入，除了给它一次写错的机会以外没有别的作用。
func researchSpec(topic string) SubmitSpec {
	return SubmitSpec{
		ToolName: "submit_research",
		Description: "提交研究结论。每条发现都必须给出 claim（一句话结论）与 source（来源标识）。" +
			"没有来源的结论**不要提交**，系统会拒绝；确实没搜到可用素材时，提交空的 findings。",
		Parameters: []tool.Parameter{
			{
				Name:        "findings",
				Type:        tool.TypeArray,
				Required:    true,
				Description: "研究发现列表；没有可用素材时传 []",
				Items: &tool.Parameter{
					Name:        "finding",
					Type:        tool.TypeObject,
					Description: "一条发现",
					Properties: []tool.Parameter{
						{
							Name: "claim", Type: tool.TypeString, Required: true,
							Description: "结论或事实，一句话，不要抄整段搜索结果",
						},
						{
							Name: "source", Type: tool.TypeString, Required: true,
							Description: "来源标识，例如 web_search#2；**没有来源就不要提交这条**",
						},
					},
				},
			},
			{
				Name:        "queries",
				Type:        tool.TypeArray,
				Required:    false,
				Description: "实际用过的查询词（便于复现与排查）",
				Items: &tool.Parameter{
					Name: "query", Type: tool.TypeString, Description: "一个查询词",
				},
			},
		},
		Decode: func(args string) (schema.Payload, error) { return decodeResearch(topic, args) },
	}
}

func decodeResearch(topic, args string) (schema.Payload, error) {
	var in struct {
		Queries  []string `json:"queries"`
		Findings []struct {
			Claim  string `json:"claim"`
			Source string `json:"source"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(llmjson.Clean(args, false)), &in); err != nil {
		return nil, fmt.Errorf("参数不是合法 JSON: %w", err)
	}

	queries := make([]string, 0, len(in.Queries))
	for _, q := range in.Queries {
		if q = strings.TrimSpace(q); q != "" {
			queries = append(queries, q)
		}
	}

	findings := make([]schema.Finding, 0, len(in.Findings))
	for i, f := range in.Findings {
		claim := strings.TrimSpace(f.Claim)
		source := strings.TrimSpace(f.Source)

		if claim == "" {
			return nil, fmt.Errorf("findings[%d].claim 不能为空", i)
		}
		// ★ L1-5 的落点：**无来源的断言不允许进系统**。
		//
		// 这不是"提醒模型带上来源"，而是**结构上让不带来源的结论进不来**。
		// 下游因此可以放心地相信：凡是 findings 里的东西，都能回溯到出处。
		if source == "" {
			return nil, fmt.Errorf(
				"findings[%d] 缺少 source：没有来源的断言不允许提交。请补上出处，或删掉这一条", i)
		}
		findings = append(findings, schema.Finding{Claim: claim, Source: source})
	}

	return schema.Research{Topic: topic, Queries: queries, Findings: findings}, nil
}

// ─── Prompt ─────────────────────────────────────────────────────────

func buildResearchPrompt(in SubAgentInput, maxSteps int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("研究主题：%s\n", in.Topic))
	if in.Hint != "" {
		sb.WriteString(fmt.Sprintf("额外要求：%s\n", in.Hint))
	}
	sb.WriteString("\n请先用搜索工具查资料，再用 submit_research 提交结论。")
	sb.WriteString(BudgetNotice(maxSteps))
	return sb.String()
}

// researchPrompt 是研究员的人格与纪律。
//
// 注意里面**没有出现任何"上游产物"的字段名** —— 研究员没有上游，
// 它的输入只有主题与额外要求。
const researchPrompt = `你是「资料研究员」。你只做一件事：找到**可引用的**素材。

【工作方式】
1. 用搜索工具查资料（可以查多次、换不同关键词）；
2. 把有价值的结论整理成 findings，**每条都要带 source**；
3. 调用 submit_research 提交。

【纪律】
- **没有来源的结论一律不要提交** —— 系统会直接拒绝没有 source 的条目；
- 不要编造数据：搜索没给出的事实，就不要写进 findings；
- claim 要写成**一句话结论**，不要把搜索结果整段抄进来；
- 如果确实什么都没搜到，就提交空的 findings，并在总结里说明 —— 这比编内容好得多。`
