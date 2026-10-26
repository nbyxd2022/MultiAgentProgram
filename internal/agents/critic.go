// Package agents 实现具体的 Agent **角色**。
//
// 与 internal/agent 的区别（两个包名很像，务必分清）：
//
//	internal/agent  = Agent 的**运行时**：ReAct 循环、终止条件、事件流。与业务无关。
//	internal/agents = 具体的**角色**：今天只有 Critic，L2 会加 research/outline/designer/renderer。
//
// 为什么要分成两个包？因为**运行时不该知道"评审"是什么东西**。
// 如果把 Critic 写进 internal/agent，运行时就被迫依赖 schema 与 tools，
// 依赖方向立刻倒挂（原则五：每层只该知道它职责之内的概念）。
//
// 命名与对照物 beautifulagent-go 的 agents/ 目录保持一致，便于后续 1:1 精读。
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

// Critic 是「软判据」评审员。
//
// 它只做一件事：找出**代码判不了的**内容质量问题。
// 机械问题（条数、长度、字面重复、HTML 标签……）已经由 tools.Check 判过——
// 让 LLM 再算一遍，是又慢、又贵、还可能算错。
//
// 这正是**原则六**：硬判据交给代码，软判据才交给 LLM。
//
// 另一点很关键：硬判据由**代码采集**、直接注入 Critic 的上下文，
// 而不是"让 Critic 调 layout_check 再把结果搬进结论里"。
// 后者等于让概率模型做无损数据搬运——那正是企业仓库 A2/A3 的坑。
type Critic struct {
	client   llm.Client
	maxSteps int
	tracer   observ.Tracer

	// guard 是预算守卫（与 Runner 同一个对象）。
	guard budget.Guard

	lastStats agent.Stats
}

// CriticOptions 是构造 Critic 的参数。
type CriticOptions struct {
	Client   llm.Client
	MaxSteps int
	Tracer   observ.Tracer

	// Guard 是预算守卫（可以为 nil -> 归一成 Unlimited）。
	// 它该与 RunnerOptions.Guard 是同一个对象（见 agent.Options.Guard）。
	Guard budget.Guard
}

// NewCritic 构造一个评审员。
func NewCritic(opt CriticOptions) *Critic {
	if opt.MaxSteps <= 0 {
		opt.MaxSteps = 6
	}
	return &Critic{
		client:   opt.Client,
		maxSteps: opt.MaxSteps,
		tracer:   opt.Tracer,
		guard:    opt.Guard,
	}
}

// Stats 返回最近一次评审的模型用量（把 L0 的成本统计链路延续下去，供 L4 核算）。
func (c *Critic) Stats() agent.Stats { return c.lastStats }

// Review 评审一份幻灯片，返回**合并后**的报告（硬判据 + 软判据）。
//
// 返回值约定：
//   - 评审成功 → (完整报告, nil)。报告里 issues 为空表示"检查过了，没有任何问题"。
//   - 评审失败 → (只含硬判据的报告, error)。**绝不静默当作"没问题"**。
//
// 第二条约定非常重要：把"没发现问题"和"没做检查"混为一谈，
// 是评审系统里最危险的 bug——它会让一个没被检查过的产物被当成合格品交付。
func (c *Critic) Review(ctx context.Context, slide schema.Slide, round int, subject string) (schema.Report, error) {
	// ① 硬判据：代码直接跑。零 token、确定性、可复现。
	hard := tools.Check(slide)
	hard.Round, hard.Subject = round, subject

	if c.client == nil {
		return markIncomplete(hard, "critic 未配置模型客户端，语义评审未执行")
	}

	// ② 软判据：只把"代码判不了的"交给 Critic。
	//
	// ⚠️ 关键：只要软判据没跑成，就必须把报告标成 Incomplete 并**带着它返回**。
	//
	// 之前这里只返回 error，调用方打印一行日志就继续了 —— 于是"没有硬问题"
	// 被当成了"可以交付"，一个**从未做过语义评审**的产物就这样被交付（L1-3）。
	// 现在即使调用方完全忽略 error，它也**不可能**把这份报告判成可交付。
	soft, stats, err := c.reviewSoft(ctx, slide, hard)
	c.lastStats = stats
	soft.Round, soft.Subject = round, subject
	if err != nil {
		return markIncomplete(hard, err.Error())
	}

	return hard.Merged(soft), nil
}

// markIncomplete 给报告打上"评审不完整"标记，并返回一份 error 供日志使用。
//
// 为什么既标状态又返回 error？
//
//	状态（Incomplete）保证**正确性**：调用方就算忽略 error 也不会误判；
//	error 提供**细节**：调用方想记录、想重试时拿得到原因。
//	两者职责不重合 —— 这也是"重要状态放在数据里"的具体做法。
func markIncomplete(rep schema.Report, reason string) (schema.Report, error) {
	rep.Incomplete = true
	rep.IncompleteReason = reason
	return rep, fmt.Errorf("critic: %s", reason)
}

// reviewSoft 让模型做语义评审，并收集它通过 submit_review 提交的结论。
func (c *Critic) reviewSoft(ctx context.Context, slide schema.Slide, hard schema.Report) (schema.Report, agent.Stats, error) {
	collector := &reviewCollector{}

	registry := tool.NewRegistry()
	registry.Register(newSubmitReview(collector))

	a := agent.New(agent.Options{
		Name:         "critic",
		Description:  "幻灯片语义质量评审员",
		SystemPrompt: criticSystemPrompt,
		Client:       c.client,
		Tools:        registry,
		MaxSteps:     c.maxSteps,
		Tracer:       c.tracer,
		Guard:        c.guard,
	})

	if _, err := a.Run(ctx, buildReviewPrompt(slide, hard)); err != nil {
		return schema.Report{}, a.Stats(), fmt.Errorf("调用模型失败: %w", err)
	}

	if !collector.submitted {
		return schema.Report{}, a.Stats(), fmt.Errorf(
			"模型没有调用 submit_review，评审未完成——不能当作\"没有问题\"")
	}

	return schema.Report{Issues: collector.issues}, a.Stats(), nil
}

// buildReviewPrompt 组装评审请求。
//
// 注意这里把**硬判据结论**直接写进了 Prompt：这既是"不让模型搬运数据"，
// 也是在明确告诉它"这些已经查过了，你换个角度"。
func buildReviewPrompt(slide schema.Slide, hard schema.Report) string {
	var sb strings.Builder

	sb.WriteString("请评审下面这一页幻灯片。\n\n【被评审产物】\n")
	payload, _ := json.MarshalIndent(slide, "", "  ")
	sb.Write(payload)

	sb.WriteString("\n\n【版式硬检查结果（由代码产出，不要重复报告）】\n")
	if len(hard.Issues) == 0 {
		sb.WriteString("（无）\n")
	} else {
		for _, line := range hard.Lines() {
			sb.WriteString("- " + line + "\n")
		}
	}

	sb.WriteString("\n请只报告**语义质量问题**，并按约定调用 submit_review 提交结论。")
	return sb.String()
}

// criticSystemPrompt 是评审员的"人格与标准"。
//
// 它和生成器的 Prompt 是**两套完全不同的目标**：
// 生成要发散（探索解空间），评审要收敛（对照标准）。
// 塞进同一个上下文会互相干扰——这就是 Critic 必须独立成 Agent 的原因之一。
const criticSystemPrompt = `你是「幻灯片评审员」。你的唯一职责是找出**代码判不了的内容质量问题**。

【被评审对象】
一页幻灯片的结构化产物：{"title": "...", "bullets": ["...", "..."]}

【已经由代码检查过的项（不要重复报告）】
标题与要点的空值、超长、条数超限、字面重复、含 HTML 标签、标点风格——
这些由 layout_check 在代码里判过了，结论会随评审请求一起给你。
你再报一遍只会浪费一轮返工。

【你只负责以下语义问题（级别只能是 major 或 suggestion）】
1. 空洞：只是口号，没有可验证的信息（例："要重视创新"）
2. 不可验证：出现具体数据，但没有任何来源或依据
3. 换句话重复：两条要点在说同一件事（注意：不是字面重复）
4. 跑题：要点与标题主题无关
5. 无结论：只罗列现象，给不出判断或倾向
6. 结构失衡：全部是同类内容（例如三条都在讲趋势，没有结论与建议）

【提交方式（必须遵守）】
- 把全部结论写进 submit_review 的 issues 参数，一次性提交；
- 不要只在文字里描述问题而不调用工具——那样系统会判定"评审未完成"；
- 如果确实没有语义问题，就提交空的 issues 数组，明确表达"检查过了，没问题"；
- 不要修改产物、不要重新生成、不要调用 layout_check——你只做评审。

【判断纪律】
- 每条 message 必须说清"错在哪"，fix 必须说清"怎么改"，不允许含糊；
- 不要臆测产物之外的背景信息；
- 不要因为"还能更好"就报问题，只在确实影响可用性或可信度时才报。`

// ─── 提交出口 ───────────────────────────────────────────────────────

// reviewCollector 收集模型提交的结论。
//
// 为什么要用一个对象接收，而不是读模型的最终回复文本？
//
//	因为"让模型吐 JSON 文本再解析"是最脆的一环：markdown 包裹、转义、
//	输出截断都会让解析失败。用一个工具收结论，就把"结构化"从
//	**文本约定**升级成**契约约定**（字段名与类型写在 JSON Schema 里）。
//	同时解析失败还能走 L0 那条现成的回灌通道自愈。
//
// 这是"结构化 handoff"的最小版本，L2 会把它正式化。
type reviewCollector struct {
	submitted bool
	issues    []schema.Issue
}

// submitReview 是 Critic 的提交出口。
type submitReview struct {
	c *reviewCollector
}

func newSubmitReview(c *reviewCollector) *submitReview { return &submitReview{c: c} }

func (t *submitReview) Definition() tool.Definition {
	return tool.Definition{
		Name: "submit_review",
		Description: "提交本次评审的**最终结论**。" +
			"issues 为你发现的所有语义质量问题；**如果确实没有问题，必须传空数组 []**——" +
			"只在文字里说\"没问题\"而不调用本工具，系统会判定评审未完成。" +
			"重复调用会用新结论**替换**上一次，不会累加。",
		Parameters: []tool.Parameter{
			{
				Name:        "issues",
				Type:        tool.TypeArray,
				Required:    true,
				Description: "语义质量问题列表；没有问题时传 []",
				Items: &tool.Parameter{
					Name:        "issue",
					Type:        tool.TypeObject,
					Description: "一条问题",
					Properties: []tool.Parameter{
						{
							Name:     "level",
							Type:     tool.TypeString,
							Required: true,
							Description: "严重度，只能是 major（严重）或 suggestion（建议）。" +
								"blocker 由代码硬判据产出，不允许在这里使用",
						},
						{
							Name: "field", Type: tool.TypeString, Required: true,
							Description: "问题定位，如 bullets[2] / title / bullets",
						},
						{
							Name: "message", Type: tool.TypeString, Required: true,
							Description: "错在哪（事实描述）",
						},
						{
							Name: "fix", Type: tool.TypeString, Required: true,
							Description: "怎么改（可执行的修改方向）",
						},
					},
				},
			},
		},
	}
}

type submitReviewArgs struct {
	Issues []struct {
		Level   string `json:"level"`
		Field   string `json:"field"`
		Message string `json:"message"`
		Fix     string `json:"fix"`
	} `json:"issues"`
}

// Execute 校验并收集模型提交的结论。
//
// 这里做了三件"防御性"的事，每一件都对应一种真实事故：
//  1. 校验 level 合法性 —— schema.Level.Weight() 对未知级别返回 0，
//     若不拦住，拼错级别的问题会被静默"洗掉"（日志里看不见、分数也不涨）；
//  2. 拒绝 blocker 级别 —— 致命问题由硬判据说了算，否则模型可以随意把质量分级搞乱；
//  3. 覆盖而非累加 —— 累加会让同一条问题算两遍，分数虚高，返工永不收敛。
func (t *submitReview) Execute(_ context.Context, args string) (string, error) {
	var in submitReviewArgs
	if err := json.Unmarshal([]byte(llmjson.Clean(args, false)), &in); err != nil {
		return "", fmt.Errorf("参数不是合法 JSON: %w", err)
	}

	issues := make([]schema.Issue, 0, len(in.Issues))
	for i, raw := range in.Issues {
		level := schema.Level(strings.ToLower(strings.TrimSpace(raw.Level)))
		if !level.Valid() {
			return "", fmt.Errorf("issues[%d].level=%q 不合法，只能是 blocker/major/suggestion",
				i, raw.Level)
		}
		if level == schema.LevelBlocker {
			return "", fmt.Errorf(
				"issues[%d].level 不能是 blocker：致命问题由硬判据（layout_check）产出", i)
		}
		message := strings.TrimSpace(raw.Message)
		if message == "" {
			return "", fmt.Errorf("issues[%d].message 不能为空：没有 message 就不知道错在哪", i)
		}
		fix := strings.TrimSpace(raw.Fix)
		if fix == "" {
			return "", fmt.Errorf("issues[%d].fix 不能为空：没有 fix 模型就不知道该怎么改", i)
		}

		issues = append(issues, schema.Issue{
			Level:   level,
			Field:   strings.TrimSpace(raw.Field),
			Message: message,
			Fix:     fix,
			Source:  schema.SourceCritic,
		})
	}

	note := "结论已记录。"
	if t.c.submitted {
		note = "已用新结论替换上一次提交（不会累加）。"
	}
	t.c.submitted = true
	t.c.issues = issues

	out, err := json.Marshal(map[string]any{
		"ok":       true,
		"received": len(issues),
		"note":     note,
	})
	if err != nil {
		return "", fmt.Errorf("序列化提交回执失败: %w", err)
	}
	return string(out), nil
}
