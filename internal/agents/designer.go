package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"multiagent/internal/agent"
	"multiagent/internal/budget"
	"multiagent/internal/llm"
	"multiagent/internal/llmjson"
	"multiagent/internal/observ"
	"multiagent/internal/schema"
	"multiagent/internal/tool"
)

// Designer 是第三个角色：**为大纲的每一页挑一个版式**。
//
// 它和 outline 的关键差别在于：**它要与上游"对齐"**。
//
//	outline 只需要读素材；designer 必须为一页不落地给出设计，
//	并且**页数、页序都要和 outline 对得上**。
//
// 于是它引出一个新的契约问题：
//
//	`Needs()` 只保证"上游**存在**"，不保证"上游**对得上**"。
//	一致性必须由**消费方**自己校验 —— 这就是本文件里那一堆校验的由来。
type Designer struct {
	client   llm.Client
	maxSteps int
	tracer   observ.Tracer

	// guard 是预算守卫（与 Runner 同一个对象）。
	guard budget.Guard

	// totalStats 是**累计**用量（跨多次 Run 累加）—— 口径见 `subagent.go` 的 `StatsReporter`。
	totalStats agent.Stats
}

// DesignerOptions 是构造版面设计者的参数。
type DesignerOptions struct {
	Client   llm.Client
	MaxSteps int
	Tracer   observ.Tracer

	// Guard 是预算守卫（可以为 nil -> 归一成 Unlimited）。
	// 它该与 RunnerOptions.Guard 是同一个对象（见 agent.Options.Guard）。
	Guard budget.Guard
}

// NewDesigner 构造版面设计者。
func NewDesigner(opt DesignerOptions) (*Designer, error) {
	if opt.Client == nil {
		return nil, fmt.Errorf("designer: 未配置模型客户端")
	}
	if opt.MaxSteps <= 0 {
		opt.MaxSteps = 8
	}
	return &Designer{
		client:   opt.Client,
		maxSteps: opt.MaxSteps,
		tracer:   opt.Tracer,
		guard:    opt.Guard,
	}, nil
}

// Name 实现 SubAgent 接口。
func (d *Designer) Name() string { return "designer" }

// Kind 实现 SubAgent 接口。
func (d *Designer) Kind() schema.Kind { return schema.KindDesign }

// Description 告诉编排者"什么时候该调我"。
func (d *Designer) Description() string {
	return "版面设计者：为大纲的每一页挑选一个版式（从系统给定的枚举中选），并写一句视觉意图。"
}

// Needs 实现 SubAgent 接口：必须先有大纲。
//
// 注意它只声明"存在性"。**"页数对不对得上"不在这里管** ——
// 那属于消费方的一致性校验（见 Run）。
func (d *Designer) Needs() []schema.Kind {
	return []schema.Kind{schema.KindOutline}
}

// Stats 返回本角色实例的**累计**用量（实现 agents.StatsReporter）。
func (d *Designer) Stats() agent.Stats { return d.totalStats }

// Run 实现 SubAgent 接口。
func (d *Designer) Run(ctx context.Context, in SubAgentInput) (schema.Payload, error) {
	// ① 取回大纲 —— 不是为了"看看"，而是为了**拿到页数**：
	//    页数是校验的基准，必须来自上游的权威数据，不能来自模型自述。
	outline, ref, err := loadOutline(in)
	if err != nil {
		return nil, err
	}

	prompt, err := buildDesignPrompt(in, outline, ref, d.maxSteps)
	if err != nil {
		return nil, err
	}

	// ② 提交出口：**把页数闭包进去**。
	//
	//    于是这个提交口的 Description 与校验都建立在"上一跳刚刚产出的页数"之上。
	//    模型看到的约束，和代码执行的约束，是**同一个数字**（单一真相来源）。
	submit, err := NewSubmitTool(designerSpec(len(outline.Pages)))
	if err != nil {
		return nil, err
	}

	registry := tool.NewRegistry()
	registry.Register(submit)

	a := agent.New(agent.Options{
		Name:         d.Name(),
		Description:  d.Description(),
		SystemPrompt: designerPrompt,
		Client:       d.client,
		Tools:        registry,
		MaxSteps:     d.maxSteps,
		Tracer:       d.tracer,
		Guard:        d.guard,
	})

	if _, err := a.Run(ctx, prompt); err != nil {
		d.totalStats = addStats(d.totalStats, a.Stats())

		// ⚠️ **触顶 ≠ 失败**（同 research / outline）：已经提交过的设计就是有效成果。
		if submit.Submitted() {
			return submit.Product(), nil
		}
		return nil, fmt.Errorf("designer: %w", err)
	}
	d.totalStats = addStats(d.totalStats, a.Stats())

	if !submit.Submitted() {
		return nil, fmt.Errorf("designer: 模型没有调用 %s，无法判定产出", submit.Definition().Name)
	}
	return submit.Product(), nil
}

// ─── 上游读取 ───────────────────────────────────────────────────────

// loadOutline 取回并解析上游大纲，同时返回它的带版本引用。
func loadOutline(in SubAgentInput) (schema.Outline, string, error) {
	fetched, err := in.Fetch()
	if err != nil {
		return schema.Outline{}, "", fmt.Errorf("designer: 取回上游产物失败: %w", err)
	}
	for _, a := range fetched {
		if a.Kind != schema.KindOutline {
			continue
		}
		outline, err := schema.Decode[schema.Outline](a)
		if err != nil {
			return schema.Outline{}, "", fmt.Errorf("designer: 上游产物无法解析: %w", err)
		}
		if len(outline.Pages) == 0 {
			return schema.Outline{}, "", fmt.Errorf("designer: 上游大纲是空的")
		}
		return outline, schema.RefOf(a), nil
	}
	return schema.Outline{}, "", fmt.Errorf("designer: 输入里没有 outline 产物")
}

// ─── 提交契约 ───────────────────────────────────────────────────────

// designerSpec 声明"设计长什么样"，并把**页数**钉死。
func designerSpec(pageCount int) SubmitSpec {
	return SubmitSpec{
		ToolName: "submit_design",
		Description: fmt.Sprintf(
			"提交 %d 页的版式设计。必须为**每一页**各给一份，page_index 不得重复。"+
				"注意：第 1 页必须是 %s，最后一页必须是 %s（系统会校验）。",
			pageCount, schema.LayoutCover, schema.LayoutClosing),
		Parameters: []tool.Parameter{
			{
				Name:        "pages",
				Type:        tool.TypeArray,
				Required:    true,
				Description: fmt.Sprintf("每页一项，共 %d 项", pageCount),
				Items: &tool.Parameter{
					Name:        "page",
					Type:        tool.TypeObject,
					Description: "一页的设计",
					Properties: []tool.Parameter{
						{
							Name: "page_index", Type: tool.TypeInteger, Required: true,
							Description: fmt.Sprintf(
								"这份设计对应大纲的第几页，从 0 开始，取值 0..%d；每一页都要有且只有一份",
								pageCount-1),
						},
						{
							Name: "layout", Type: tool.TypeString, Required: true,
							Description: "版式。必须从这些值里**原样照抄**一个：" +
								schema.JoinLayoutNames(" / "),
						},
						{
							Name: "note", Type: tool.TypeString, Required: true,
							Description: fmt.Sprintf(
								"一句话说明这页用什么视觉手段承载内容（不超过 %d 字）",
								schema.MaxNoteRunes),
						},
					},
				},
			},
		},
		// 页数由闭包捕获 —— decode 与 Description 用的是同一个数字
		Decode: func(args string) (schema.Payload, error) {
			return decodeDesign(args, pageCount)
		},
	}
}

// designSubmission 是提交参数的原始形态。
//
// ⚠️ PageIndex 用 `*int` 而不是 `int`，这是一个**具体的、真实的坑**：
//
//	企业仓库里写的是 `case t.PageIndex != 0:` 来判"模型有没有给"——
//	但 **0 是合法的页码（第一页）**。用零值当"没给"的哨兵，
//	首页就会被判成缺省、然后去取另一个字段（p），逻辑从根上就是错的
//	（它恰好因为 p 也是 0 才没炸）。
//
// 指针的价值就在这：它能区分"没给"和"给了 0"。
// 凡是用零值当哨兵的地方，都要先问一句：**零是我这个字段的合法值吗？**
type designSubmission struct {
	Pages []struct {
		PageIndex *int   `json:"page_index"`
		Layout    string `json:"layout"`
		Note      string `json:"note"`
	} `json:"pages"`
}

// decodeDesign 校验并归一化设计提交。
//
// 校验清单（每一条都对应一种真实事故）：
//
//	① 份数 = 页数            —— 少给一页，那一页就没设计
//	② page_index 存在         —— 缺了就无法定位（指针判空，不用零值做哨兵）
//	③ page_index 不越界       —— 越界会指向不存在的页
//	④ page_index 不重复       —— 重复意味着另一页被顶掉
//	⑤ 覆盖完整 0..n-1         —— ④ + ① 其实已隐含，但显式检查能给出更好的错误信息
//	⑥ layout 在枚举内         —— 自由文本进不来（本文件的核心目的）
//	⑦ 结构页版式正确          —— 产品规则由代码保证
//	⑧ note 非空且不超长       —— 保表达力，但不让它膨胀成新 Prompt
func decodeDesign(args string, pageCount int) (schema.Payload, error) {
	var in designSubmission
	if err := json.Unmarshal([]byte(llmjson.Clean(args, false)), &in); err != nil {
		return nil, fmt.Errorf("参数不是合法 JSON: %w", err)
	}

	// ①
	if len(in.Pages) != pageCount {
		return nil, fmt.Errorf("必须为 %d 页各给一份设计，实际给了 %d 份", pageCount, len(in.Pages))
	}

	seen := make(map[int]bool, pageCount)
	pages := make([]schema.PageDesign, 0, pageCount)

	for i, p := range in.Pages {
		// ② 指针判空 —— 而不是 `p.PageIndex != 0`
		if p.PageIndex == nil {
			return nil, fmt.Errorf("pages[%d].page_index 缺失，无法确定它对应哪一页", i)
		}
		idx := *p.PageIndex

		// ③
		if idx < 0 || idx >= pageCount {
			return nil, fmt.Errorf("pages[%d].page_index=%d 越界，合法取值 0..%d",
				i, idx, pageCount-1)
		}

		// ④
		if seen[idx] {
			return nil, fmt.Errorf("page_index=%d 出现了不止一次；每一页都要有且只有一份设计",
				idx)
		}
		seen[idx] = true

		// ⑥ 版式必须是枚举值，且错误信息里给出**合法清单**（让拒绝可自愈）
		layout := schema.Layout(strings.TrimSpace(p.Layout))
		if !layout.Valid() {
			return nil, fmt.Errorf("pages[%d].layout=%q 不是合法版式。请从这些值里原样照抄一个：%s",
				i, p.Layout, schema.JoinLayoutNames(" / "))
		}

		// ⑦ 产品规则：结构页的版式由代码保证
		if must, ok := schema.RequiredLayout(idx, pageCount); ok && layout != must {
			return nil, fmt.Errorf("第 %d 页（共 %d 页）的版式必须是 %q，你给的是 %q",
				idx+1, pageCount, must, layout)
		}

		// ⑧
		note := strings.TrimSpace(p.Note)
		if note == "" {
			return nil, fmt.Errorf("pages[%d].note 不能为空 —— 一句话说明这页用什么视觉手段承载内容", i)
		}
		if n := len([]rune(note)); n > schema.MaxNoteRunes {
			return nil, fmt.Errorf("pages[%d].note 有 %d 字，超过上限 %d 字，请精简",
				i, n, schema.MaxNoteRunes)
		}

		pages = append(pages, schema.PageDesign{PageIndex: idx, Layout: layout, Note: note})
	}

	// ⑤ 显式覆盖检查（能给出比"重复"更准确的错误信息）
	for i := 0; i < pageCount; i++ {
		if !seen[i] {
			return nil, fmt.Errorf("第 %d 页没有设计，每一页都必须被覆盖", i+1)
		}
	}

	// 按页序排出 —— 这样下游拿到的 Design.Pages[i].PageIndex 必然等于 i，
	// 于是"数组顺序即页序"重新成立，下游不必再自己排序。
	sort.Slice(pages, func(i, j int) bool { return pages[i].PageIndex < pages[j].PageIndex })

	return schema.Design{Pages: pages}, nil
}

// ─── Prompt ─────────────────────────────────────────────────────────

const designerPrompt = `你是「版面设计者」。你为大纲里的每一页挑选一个版式。

【工作方式】
1. 读输入里给你的「大纲」（每页有标题与内容要点）；
2. 看每一项内容**是什么形态**（并列要点？对比？一个数据？先后序列？），
   据此从系统给定的版式目录里挑一个；
3. 给每页写一句"这页用什么视觉手段承载内容"；
4. 调用 submit_design 提交。

【纪律】
- 版式名必须**原样照抄**目录里的值，**严禁自创**（自造的会被代码打回）；
- 每一页都要有且只有一份设计；
- 第 1 页与最后一页的版式由产品规则固定，系统会校验；
- 不要复述大纲内容，note 只写"视觉意图"（用什么手段、强调什么），不写具体文案。`

// buildDesignPrompt 把大纲渲染成"每页一行"的可读清单，并附上版式目录。
//
// 两个细节：
//
//	① 大纲正文由**代码渲染**，模型只读不搬（A2/A3 的教训）；
//	② 每页都带页码，且带上依据的是哪一版大纲 —— 让这一跳可回溯。
func buildDesignPrompt(in SubAgentInput, outline schema.Outline, ref string, maxSteps int) (string, error) {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("主题：%s\n", in.Topic))
	if in.Hint != "" {
		sb.WriteString(fmt.Sprintf("额外要求：%s\n", in.Hint))
	}

	sb.WriteString(fmt.Sprintf("\n【大纲】（依据版本：%s，共 %d 页）\n", ref, len(outline.Pages)))
	for _, p := range outline.Pages {
		sb.WriteString(fmt.Sprintf("第 %d 页「%s」：%s\n",
			p.Index+1, p.Title, strings.Join(p.Points, "；")))
	}

	// 版式目录由 schema 渲染 —— 模型看到的选项与代码校验的集合是同一份数据
	sb.WriteString("\n【可选版式】（只能原样照抄其中一个）\n")
	for _, spec := range schema.LayoutCatalog() {
		sb.WriteString(fmt.Sprintf("- %s：%s｜适合：%s\n", spec.Layout, spec.Description, spec.BestFor))
	}

	sb.WriteString(fmt.Sprintf("\n注意：第 1 页必须是 %s，最后一页必须是 %s（系统会校验）。\n",
		schema.LayoutCover, schema.LayoutClosing))
	sb.WriteString(fmt.Sprintf("\n请为这 %d 页各给一份设计，然后用 submit_design 提交（page_index 从 0 开始）。",
		len(outline.Pages)))
	sb.WriteString(BudgetNotice(maxSteps))

	return sb.String(), nil
}
