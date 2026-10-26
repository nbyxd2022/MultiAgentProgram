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
)

// TargetOutlinePages 是本阶段的产品硬约束：**正好 3 页**。
//
// 为什么写死？因为"一句话主题 → 3 页 PPT"就是这个阶段的题目定义。
//
// 更重要的是——**页数约束必须由代码校验，而不是写在 Prompt 里**。
// 如果在 Prompt 里说"请生成 3 页"，模型生成 2 页或 5 页时**没有任何一层会发现**，
// 错误会一路漂到渲染阶段才暴露（这正是 L1-8 那类"流程约束靠 Prompt"的坑）。
//
// 所以它同时被用在两个地方，且只写一次：
//
//	① 拼进 Prompt（让模型知道目标）
//	② decodeOutline 里做硬校验（模型做不到就被打回）
const TargetOutlinePages = 3

// OutlineAgent 是第二个角色：**把素材组织成叙事结构**。
//
// 它是第一个**有上游依赖**的角色（Needs = [research]），
// 于是它也是第一个真正用到"按需取全文"的角色：
//
//	它拿到的 Inputs 只有 {ref, summary}，
//	正文要靠 in.Fetch() 自己去黑板取 —— "谁真的需要上游全文"从此是**可观测的**。
type OutlineAgent struct {
	client   llm.Client
	maxSteps int
	tracer   observ.Tracer

	// guard 是预算守卫（与 Runner 同一个对象）。
	guard budget.Guard

	// totalStats 是**累计**用量（跨多次 Run 累加）—— 口径见 `subagent.go` 的 `StatsReporter`。
	totalStats agent.Stats
}

// OutlineOptions 是构造大纲编写者的参数。
type OutlineOptions struct {
	Client   llm.Client
	MaxSteps int
	Tracer   observ.Tracer

	// Guard 是预算守卫（可以为 nil -> 归一成 Unlimited）。
	// 它该与 RunnerOptions.Guard 是同一个对象（见 agent.Options.Guard）。
	Guard budget.Guard
}

// NewOutlineAgent 构造大纲编写者。
func NewOutlineAgent(opt OutlineOptions) (*OutlineAgent, error) {
	if opt.Client == nil {
		return nil, fmt.Errorf("outline: 未配置模型客户端")
	}
	if opt.MaxSteps <= 0 {
		opt.MaxSteps = 8
	}
	return &OutlineAgent{
		client:   opt.Client,
		maxSteps: opt.MaxSteps,
		tracer:   opt.Tracer,
		guard:    opt.Guard,
	}, nil
}

// Name 实现 SubAgent 接口。
func (o *OutlineAgent) Name() string { return "outline" }

// Kind 实现 SubAgent 接口。
func (o *OutlineAgent) Kind() schema.Kind { return schema.KindOutline }

// Description 告诉编排者"什么时候该调我"。
//
// 注意：**依赖不在这里写**。
//
//	"要先有 research" 这件事由 Needs() 表达、由适配器渲染成【前置条件】。
//	如果在描述里再写一遍，就成了"同一件事写在两处"——两处迟早会漂移。
func (o *OutlineAgent) Description() string {
	return fmt.Sprintf("大纲编写者：把研究素材组织成 %d 页的叙事结构（每页含标题与内容要点）。",
		TargetOutlinePages)
}

// Needs 实现 SubAgent 接口：必须先有研究结论。
//
// 这一行让"顺序约束"第一次真正由**代码**强制：
//
//	没产出 research 时，调用 outline 会被直接拒绝（并回灌"还缺什么"）。
//	——而不是"先做错、再被 Critic 抓、再返工"。
func (o *OutlineAgent) Needs() []schema.Kind {
	return []schema.Kind{schema.KindResearch}
}

// Stats 返回本角色实例的**累计**用量（实现 agents.StatsReporter）。
func (o *OutlineAgent) Stats() agent.Stats { return o.totalStats }

// Run 实现 SubAgent 接口。
func (o *OutlineAgent) Run(ctx context.Context, in SubAgentInput) (schema.Payload, error) {
	// ① 按需取回上游全文（这一步是显式的，所以是可观测的）
	prompt, err := buildOutlinePrompt(in, o.maxSteps)
	if err != nil {
		return nil, err
	}

	// ② 提交出口
	submit, err := NewSubmitTool(outlineSpec())
	if err != nil {
		return nil, err
	}

	// 这个角色不需要检索、不需要渲染 —— 它只需要一个"地方"把结论交出来。
	registry := tool.NewRegistry()
	registry.Register(submit)

	a := agent.New(agent.Options{
		Name:         o.Name(),
		Description:  o.Description(),
		SystemPrompt: outlinePrompt,
		Client:       o.client,
		Tools:        registry,
		MaxSteps:     o.maxSteps,
		Tracer:       o.tracer,
		Guard:        o.guard,
	})

	if _, err := a.Run(ctx, prompt); err != nil {
		o.totalStats = addStats(o.totalStats, a.Stats())

		// ⚠️ **触顶 ≠ 失败**（同 research）：已经提交过的结论就是有效成果。
		if submit.Submitted() {
			return submit.Product(), nil
		}
		return nil, fmt.Errorf("outline: %w", err)
	}
	o.totalStats = addStats(o.totalStats, a.Stats())

	if !submit.Submitted() {
		return nil, fmt.Errorf("outline: 模型没有调用 %s，无法判定产出", submit.Definition().Name)
	}
	return submit.Product(), nil
}

// ─── 提交契约 ───────────────────────────────────────────────────────

// outlineSpec 声明"大纲长什么样"。
//
// ⚠️ 参数里**故意没有 `index`**：
//
//	页码由代码按数组位置赋值（见 decodeOutline）。
//	理由与 research 的 Topic 相同 —— **能由代码确定的事，不要交给模型复述**：
//	它只可能在 0-based / 1-based 之间写错，不可能比代码写得更准。
func outlineSpec() SubmitSpec {
	return SubmitSpec{
		ToolName: "submit_outline",
		Description: fmt.Sprintf(
			"提交 PPT 大纲。必须**正好 %d 页**，每页都要有标题和至少一条内容要点。"+
				"页码不用你填，系统会按顺序自动编号。", TargetOutlinePages),
		Parameters: []tool.Parameter{
			{
				Name:        "pages",
				Type:        tool.TypeArray,
				Required:    true,
				Description: fmt.Sprintf("页面列表，顺序即页序，正好 %d 页", TargetOutlinePages),
				Items: &tool.Parameter{
					Name:        "page",
					Type:        tool.TypeObject,
					Description: "一页大纲",
					Properties: []tool.Parameter{
						{
							Name: "title", Type: tool.TypeString, Required: true,
							Description: fmt.Sprintf("页面标题，不超过 %d 个字", schema.MaxTitleRunes),
						},
						{
							Name: "points", Type: tool.TypeArray, Required: true,
							Description: "该页要讲的内容点，至少 1 条；尽量来自上游研究结论，不要编造",
							Items: &tool.Parameter{
								Name: "point", Type: tool.TypeString, Description: "一个内容点",
							},
						},
					},
				},
			},
		},
		Decode: decodeOutline,
	}
}

func decodeOutline(args string) (schema.Payload, error) {
	var in struct {
		Pages []struct {
			Title  string   `json:"title"`
			Points []string `json:"points"`
		} `json:"pages"`
	}
	if err := json.Unmarshal([]byte(llmjson.Clean(args, false)), &in); err != nil {
		return nil, fmt.Errorf("参数不是合法 JSON: %w", err)
	}

	// ① 页数：硬约束，模型做不到就被打回（回灌自愈）
	if len(in.Pages) != TargetOutlinePages {
		return nil, fmt.Errorf("必须正好 %d 页，实际 %d 页。请调整后重新提交",
			TargetOutlinePages, len(in.Pages))
	}

	pages := make([]schema.PageOutline, 0, len(in.Pages))
	for i, p := range in.Pages {
		title := strings.TrimSpace(p.Title)
		if title == "" {
			return nil, fmt.Errorf("pages[%d].title 不能为空", i)
		}
		// ② 标题长度**复用 slide 层的常量**：页标题最终就是幻灯片标题，
		//    在这里拦一次，就不必等到渲染完才发现（约束只有一个来源）。
		if n := len([]rune(title)); n > schema.MaxTitleRunes {
			return nil, fmt.Errorf("pages[%d].title 有 %d 字，超过上限 %d 字，请精简",
				i, n, schema.MaxTitleRunes)
		}

		points := make([]string, 0, len(p.Points))
		for j, pt := range p.Points {
			pt = strings.TrimSpace(pt)
			if pt == "" {
				return nil, fmt.Errorf("pages[%d].points[%d] 不能为空", i, j)
			}
			points = append(points, pt)
		}

		// ③ 内容点的条数：这里就要卡死。
		//
		// ⚠️ 这一条是**对 R20 判断的修正**（当时的结论是"条数留到 layout_check 去管"）。
		//
		// 为什么必须提前到这里？
		//
		//	因为这些点会**原样变成幻灯片上的 bullets** —— renderer 不做二次压缩
		//	（见 renderer.go 里那段说明）。所以条数在大纲阶段就已经定死了。
		//	等渲染完写盘再发现"只有 1 条太少"，模型已经改不动它了：
		//	那个文件已经躺在磁盘上，只能整个重来。
		//
		// 一般原则：**约束要放在"最晚还能改得动它"的那一层** ——
		//	比这更早是浪费，比这更晚就来不及。
		//
		// 而结构页（首页/末页）内容密度天然更低：封面就是一句话，
		//	硬凑 3 条要点反而会把封面撑坏。所以下限对它放宽到 1。
		minPoints := schema.MinBullets
		if schema.IsStructuralPage(i, len(in.Pages)) {
			minPoints = 1
		}
		if len(points) < minPoints || len(points) > schema.MaxBullets {
			return nil, fmt.Errorf(
				"pages[%d] 有 %d 条内容点，应为 %d-%d 条（正文页 %d-%d 条，首页/末页 %d-%d 条）",
				i, len(points), minPoints, schema.MaxBullets,
				schema.MinBullets, schema.MaxBullets, 1, schema.MaxBullets)
		}

		// ④ 页码由代码赋值，不采信模型（即使它硬塞了一个 index，也会被忽略）
		pages = append(pages, schema.PageOutline{Index: i, Title: title, Points: points})
	}

	return schema.Outline{Pages: pages}, nil
}

// ─── Prompt ─────────────────────────────────────────────────────────

// buildOutlinePrompt 把上游研究结论**渲染成文本**注入 Prompt。
//
// 这一步与"让模型自己调工具去取"的区别很关键：
//
//	我们不是把正文塞进它的上下文就完事 —— 而是**在代码里取、在代码里渲染**。
//	模型只负责"读"，不负责"搬"（A2/A3 的教训）。
//	同时把上游的 ref 一起写进去，让每一页大纲都能**回溯到它依据的那一版素材**。
func buildOutlinePrompt(in SubAgentInput, maxSteps int) (string, error) {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("主题：%s\n", in.Topic))
	if in.Hint != "" {
		sb.WriteString(fmt.Sprintf("额外要求：%s\n", in.Hint))
	}

	// 显式取全文 —— 这个角色**确实需要**正文（它要照着素材写内容）。
	fetched, err := in.Fetch()
	if err != nil {
		return "", fmt.Errorf("outline: 取回上游产物失败: %w", err)
	}

	var research schema.Research
	found := false
	for _, a := range fetched {
		if a.Kind != schema.KindResearch {
			continue
		}
		research, err = schema.Decode[schema.Research](a)
		if err != nil {
			return "", fmt.Errorf("outline: 上游产物无法解析: %w", err)
		}
		sb.WriteString("\n" + renderResearch(a, research))
		found = true
	}
	if !found {
		// 到这一步通常意味着依赖装配有问题（Needs 已经拦过一轮）。
		// 与其让模型对着空气编内容，不如直接报错。
		return "", fmt.Errorf("outline: 输入里没有 research 产物")
	}

	sb.WriteString(fmt.Sprintf("\n请组织成正好 %d 页，然后用 submit_outline 提交。", TargetOutlinePages))
	sb.WriteString(BudgetNotice(maxSteps))
	return sb.String(), nil
}

// renderResearch 把研究结论渲染成 Prompt 片段。
//
// 注意每一行都带 source —— **让下游看得见每个断言的出处**（L1-5 的延续）。
func renderResearch(a schema.Artifact, r schema.Research) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("【上游研究结论】（依据版本：%s）\n", schema.RefOf(a)))

	if len(r.Findings) == 0 {
		// 空素材是一个**合法的结论**，必须如实转达 ——
		// 否则模型会默认"应该有素材"，然后开始编。
		sb.WriteString("（上游没有找到任何可用素材。不要编造数据，只能写通用结构。）\n")
		return sb.String()
	}
	for _, f := range r.Findings {
		sb.WriteString(fmt.Sprintf("- %s（来源：%s）\n", f.Claim, f.Source))
	}
	return sb.String()
}

// outlinePrompt 是大纲编写者的人格与纪律。
const outlinePrompt = `你是「大纲编写者」。你把别人找来的素材，组织成一份**有叙事线**的页面结构。

【工作方式】
1. 读输入里给你的「上游研究结论」（每条都带来源）；
2. 想清楚叙事线：这几页之间是什么关系（提出 → 展开 → 收束）；
3. 调用 submit_outline 提交。

【纪律】
- 页数、标题长度这些硬要求，系统会**直接校验**，不符合会被打回重填；
- 内容点要**基于上游结论**，不要编造上游没有的数据；
- 如果上游没有素材，就写通用结构，并让内容点明确是"待补充"的性质；
- 页码不用你操心，系统按顺序自动编号；
- 不要复述上游原文，要点要写成**你自己组织过的一句话**。`
