package agents

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"multiagent/internal/agent"
	"multiagent/internal/observ"
	"multiagent/internal/schema"
	"multiagent/internal/tools"
)

// 这个文件回答一个**看起来该有、其实不该有**的问题：
//
//	渲染这一跳，要不要用模型？
//
// 答案：**不要。**
//
// 理由是 L1 就定下的那条分工（原则六）：
//
//	**硬判据交给代码，软判据才交给 LLM** —— 推广开就是
//	**能用代码确定的事，就不要交给模型。**
//
// 而"把标题和要点填进一个模板"正是纯粹确定的事：
// 标题来自大纲、要点来自大纲、版式来自设计 ——
// **三个输入全部已经确定，没有任何需要"判断"的地方。**
//
// ⚠️ 企业仓库在这里用了模型（`slot_gen.md` 让 LLM 直接生成 HTML），
// 代价清清楚楚写在它的 Prompt 里：
//
//	"禁止丢内容"、"不得用概括句替换原文"、"不得把多条信息收成一条"、
//	"禁止删句、删表行列"、"不得以精简输出为由减少信息点"……
//	反复叮嘱了十几次，还额外做了一个 `slot_gen_preserve.md` 变体，
//	外加一套爆版检测 + QuickFix + LLM 修复链。
//
// 那是在为**第二次有损压缩**买单：
//
//	内容在大纲阶段已经被压缩过一次（研究素材 → 3 页要点），
//	渲染阶段又让模型"重新组织"一次，于是只能靠反复叮嘱来防丢失。
//
// 我们不做第二次压缩。**内容只被压缩一次，之后就只被搬运。**
//
// 由此得到本文件最重要的两个结论：
//
//	① **SubAgent ≠ "必须调 LLM 的 Agent"。**
//	   `SubAgent` 是一个**接口**：它的定义是"能被编排者调度的角色"，
//	   而不是"能思考的东西"。企业仓库自己也是这么分的 ——
//	   结构页（cover / 章节 / 结尾）走 `generateStructuralPage` 代码路径，
//	   只有正文页才调 LLM。
//
//	② **"多 Agent 系统"里不是每个角色都需要大脑。**
//	   一个不花 token、确定性、可复现的角色，是整条链上最便宜也最可靠的一环。

const (
	// renderToolName 与工具定义里的名字保持一致。
	renderToolName = "render_slide_html"

	// defaultRenderParallel 是并发渲染的默认上限。
	//
	// 为什么必须有上限？因为渲染是**有副作用**的操作（建目录 + 写文件）。
	// 不设限时页数一多，就会同时创建大量文件句柄、同时抢磁盘。
	// 企业仓库同样设了上限（`MaxRenderParallel = 8`），这是同一个考虑。
	//
	// 我们取 4：本阶段只有 3 页，够用，也不必和磁盘较劲。
	defaultRenderParallel = 4
)

// SlideRenderer 是"渲染一页幻灯片"的**端口**（原则五）。
//
// 之所以抽成接口而不是直接依赖 `*tools.RenderSlideHTML`：
//
//	· 测试里可以注入一个"必定失败"的实现，用来验证失败隔离；
//	· 将来换渲染引擎（HTML → PPTX）不必动这个角色一行。
//
// 这与 `ResearchAgent` 把 `Search` 做成可注入是同一个手法。
type SlideRenderer interface {
	// Render 渲染一页并写盘，返回文件路径与字节数。
	Render(slide schema.Slide) (string, int, error)

	// FilePathFor 返回某一页**将会**写入的路径（不写盘）。
	// 用于在产生副作用之前做冲突预检。
	FilePathFor(title string) string
}

// Renderer 是第四个角色：**把每一页落盘**。
//
// 它是三个"特殊"：
//
//	① 唯一**不调用模型**的角色（所以它的 Stats 恒为零）；
//	② 唯一**有副作用**的角色（写文件）；
//	③ 唯一需要**两个上游**的角色（outline + design）。
type Renderer struct {
	renderer SlideRenderer
	parallel int
	tracer   observ.Tracer

	// mu 保护 observer 的调用。
	//
	// 渲染是并发的，于是 emit 会被多个 goroutine 同时调用。
	// **让调用方不必操心"我的 observer 是不是并发安全的"，是我们的责任** ——
	// 否则每个使用方都要自己去加一层锁，迟早有人忘了加。
	mu sync.Mutex
}

// RendererOptions 是构造渲染执行者的参数。
type RendererOptions struct {
	// OutDir 是输出目录；留空则为 "out"。
	OutDir string

	// Renderer 是渲染端口；留空则使用内置的 HTML 渲染器。
	Renderer SlideRenderer

	// Parallel 是并发渲染上限；<=0 则取默认值。
	Parallel int

	Tracer observ.Tracer
}

// NewRenderer 构造渲染执行者。
//
// 注意它**不返回 error** —— 这个角色没有任何前置条件需要在构造期校验
// （它不需要模型客户端）。对比 `NewResearchAgent` 必须在构造期拒绝 nil client：
// **有前置条件就校验，没有就别为了形式统一而返回一个恒为 nil 的 error。**
func NewRenderer(opt RendererOptions) *Renderer {
	if opt.Renderer == nil {
		opt.Renderer = tools.NewRenderSlideHTML(opt.OutDir)
	}
	if opt.Parallel <= 0 {
		opt.Parallel = defaultRenderParallel
	}
	return &Renderer{
		renderer: opt.Renderer,
		parallel: opt.Parallel,
		// 归一化：nil → `NopTracer`（"没有出口"是一个对象，不是 nil）。
		tracer: observ.From(opt.Tracer),
	}
}

// Name 实现 SubAgent 接口。
func (r *Renderer) Name() string { return "renderer" }

// Kind 实现 SubAgent 接口。
func (r *Renderer) Kind() schema.Kind { return schema.KindRender }

// Description 告诉编排者"什么时候该调我"。
func (r *Renderer) Description() string {
	return "渲染执行者：按照大纲与版式设计，把每一页落盘成 HTML 文件。" +
		"它做的是**确定性搬运**，不改写内容 —— 要点会原样出现在页面上。"
}

// Needs 实现 SubAgent 接口：这是第一个需要**两个上游**的角色。
//
// 顺序有意义：适配器按这个顺序装配 Inputs（outline 在前、design 在后）。
func (r *Renderer) Needs() []schema.Kind {
	return []schema.Kind{schema.KindOutline, schema.KindDesign}
}

// Stats 返回用量 —— **恒为零**，因为这一跳不做任何模型调用。
//
// 让"不花 token"成为一个**可被观测的事实**，而不是一句口头承诺。
//
// （"累计"与"最近一次"在这一跳没有区别：两边都是 0。）
func (r *Renderer) Stats() agent.Stats { return agent.Stats{} }

// Run 实现 SubAgent 接口。
func (r *Renderer) Run(ctx context.Context, in SubAgentInput) (schema.Payload, error) {
	outline, design, err := loadRenderInputs(in)
	if err != nil {
		return nil, err
	}

	// ① 跨产物一致性校验：**页数必须对得上**。
	//
	//    这是本角色最该关心的事 —— 少一页设计，就会有一页渲染不出来；
	//    多一页设计，就会有一个设计无处安放。两种情况都必须挡住。
	if len(design.Pages) != len(outline.Pages) {
		return nil, fmt.Errorf("renderer: 页数不一致：大纲 %d 页，设计 %d 页",
			len(outline.Pages), len(design.Pages))
	}

	// ② 逐页组装。标题与要点**原样来自大纲**，一个字的改写都没有。
	slides := make([]schema.Slide, len(outline.Pages))
	for i, p := range outline.Pages {
		// designer 已经保证过按页序排列；但**渲染方不能假设上游一定守规矩**
		//（黑板上任何东西都可能被别处写入）。这条校验很便宜，而错了很贵。
		if design.Pages[i].PageIndex != i {
			return nil, fmt.Errorf("renderer: 设计未按页序排列：pages[%d].PageIndex=%d",
				i, design.Pages[i].PageIndex)
		}
		slides[i] = schema.Slide{
			Title: p.Title,
			// 防御性拷贝：切片是引用类型，不复制的话调用方改它我们也会跟着变
			//（同一个坑在 schema.Report.Merged 与 RenderSlideHTML 里都踩过）。
			Bullets: append([]string(nil), p.Points...),
		}
	}

	// ③ **写盘之前**先做冲突预检 —— 一旦写了就无法回滚。
	if err := r.checkFileConflicts(slides); err != nil {
		return nil, err
	}

	// ④ 并发渲染，结果按页序写回
	outcomes := r.renderAll(ctx, slides)

	result := schema.RenderResult{Files: make([]string, 0, len(outcomes))}
	for i, oc := range outcomes {
		if oc.err != nil {
			result.Failed = append(result.Failed, schema.PageFailure{
				PageIndex: i,
				Reason:    oc.err.Error(),
			})
			continue
		}
		result.Files = append(result.Files, oc.path)
	}

	// ⑤ **全部失败才算失败**。
	//
	//    "部分成功"是一个**合法的产物**（对照 L1 的结论：超限 ≠ 失败，
	//    应当降级交付 + 说明）。3 页里出来 2 页，交付这 2 页 + 说明第 3 页为什么失败，
	//    永远优于"整体报错、一个文件都不给"。
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("renderer: %d 页全部渲染失败：%s",
			len(outcomes), failureReasons(result.Failed))
	}
	return result, nil
}

// ─── 上游读取 ───────────────────────────────────────────────────────

// loadRenderInputs 取回并解析两个上游产物。
func loadRenderInputs(in SubAgentInput) (schema.Outline, schema.Design, error) {
	var (
		outline schema.Outline
		design  schema.Design
		hasOut  bool
		hasDes  bool
	)

	fetched, err := in.Fetch()
	if err != nil {
		return outline, design, fmt.Errorf("renderer: 取回上游产物失败: %w", err)
	}

	for _, a := range fetched {
		switch a.Kind {
		case schema.KindOutline:
			outline, err = schema.Decode[schema.Outline](a)
			if err != nil {
				return outline, design, fmt.Errorf("renderer: 上游产物无法解析: %w", err)
			}
			hasOut = true
		case schema.KindDesign:
			design, err = schema.Decode[schema.Design](a)
			if err != nil {
				return outline, design, fmt.Errorf("renderer: 上游产物无法解析: %w", err)
			}
			hasDes = true
		}
	}

	if !hasOut {
		return outline, design, fmt.Errorf("renderer: 输入里没有 outline 产物")
	}
	if !hasDes {
		return outline, design, fmt.Errorf("renderer: 输入里没有 design 产物")
	}
	if len(outline.Pages) == 0 {
		return outline, design, fmt.Errorf("renderer: 上游大纲是空的")
	}
	return outline, design, nil
}

// ─── 冲突预检 ───────────────────────────────────────────────────────

// checkFileConflicts 在**写任何文件之前**检查"会不会互相覆盖"。
//
// 文件名由标题派生（见 `RenderSlideHTML.Render`），所以两页标题相同时，
// 后写的那个会把先写的顶掉 —— 而**覆盖是不可回滚的**：前一个文件已经没了。
//
// 所以有副作用的操作，必须在动手之前把"注定要坏"的情况挡掉。
// 这是"预检"与"事后修复"的分界：
//
//	能预检的，就不要留到事后。事后的每一个修复动作，都在**已经造成的损失上**做补救。
func (r *Renderer) checkFileConflicts(slides []schema.Slide) error {
	seen := make(map[string]int, len(slides))
	for i, s := range slides {
		path := r.renderer.FilePathFor(s.Title)
		if prev, ok := seen[path]; ok {
			return fmt.Errorf(
				"renderer: 第 %d 页与第 %d 页标题相同（%q），会写进同一个文件 %s；"+
					"请先让大纲里的页面标题互不相同",
				prev+1, i+1, strings.TrimSpace(s.Title), path)
		}
		seen[path] = i
	}
	return nil
}

// ─── 并发渲染 ───────────────────────────────────────────────────────

// pageOutcome 是一页的渲染结果。
type pageOutcome struct {
	path string
	err  error
}

// renderAll 并发渲染全部页面。
//
// 三个设计点：
//
//	① **结果按索引写回**（`outcomes[i] = ...`），不是 append。
//	   并发完成的顺序是随机的；append 会让"哪一页是哪个文件"取决于调度，
//	   于是同一份输入两次运行得到不同顺序的输出 —— 无法 diff、无法回归。
//	   企业仓库同样是 `results := make([]pageResult, total)` + 按下标写。
//
//	② **失败隔离**：一页失败不中断其他页（goroutine 内各自记录 error）。
//	   这就是"部分成功"能成为合法产物的实现基础。
//
//	③ **信号量限流**：`sem` 保证同时最多 parallel 个 goroutine 真正在写盘。
func (r *Renderer) renderAll(ctx context.Context, slides []schema.Slide) []pageOutcome {
	outcomes := make([]pageOutcome, len(slides))

	parallel := r.parallel
	if parallel > len(slides) {
		parallel = len(slides) // 不必开比任务数更多的并发
	}
	if parallel < 1 {
		parallel = 1
	}

	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	for i := range slides {
		wg.Add(1)
		sem <- struct{}{} // 占位：超过并发上限就在这里等

		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			if err := ctx.Err(); err != nil {
				outcomes[i] = pageOutcome{err: fmt.Errorf("渲染前上下文已结束: %w", err)}
				return
			}

			// ⚠️ 旧的 `Step: i` 里那个 `i` 是**页号**，不是"第几步" ——
			//	而 `observ.Event.Span` 的注释**早就为它准备好了位置**：
			//
			//		"`Span` 是所属跨度的 id（**"render_p3"** / "style"）…
			//		 同一个节点可以开出多个 span，**如逐页渲染**。"
			//
			//	⭐ 而 `Phase` 与它分工明确：`Phase` = "哪一个节点"（renderer），
			//	  `Span` = "这个节点里的哪一段"（第几页）。
			//	  混用会让"第几页"看起来像"第几步"—— 而那是两个完全不同的量。
			span := fmt.Sprintf("render_p%d", i+1)

			r.emit(observ.Event{
				Layer: observ.LayerTool, Phase: r.Name(), Span: span,
				Kind:     observ.KindToolCall,
				ToolName: renderToolName, Detail: slides[i].Title,
			})

			path, _, err := r.renderer.Render(slides[i])
			outcomes[i] = pageOutcome{path: path, err: err}

			if err != nil {
				r.emit(observ.Event{
					Layer: observ.LayerTool, Phase: r.Name(), Span: span,
					Kind:     observ.KindToolResult,
					ToolName: renderToolName, Detail: "失败：" + err.Error(),
				})
				return
			}
			r.emit(observ.Event{
				Layer: observ.LayerTool, Phase: r.Name(), Span: span,
				Kind:     observ.KindToolResult,
				ToolName: renderToolName, Detail: path,
			})
		}(i)
	}

	wg.Wait()
	return outcomes
}

// emit 把事件交给出口（**并发安全**）。
//
// ⚠️ **没有判空** —— `r.tracer` 在 `NewRenderer` 里被 `observ.From` 归一过。
//
//	`mu` 仍然在，因为它的理由与"出口是不是 nil"无关：
//	**渲染是并发的**，多个 goroutine 会同时 emit。
//	而"让调用方不必操心'我的出口是不是并发安全的'，是我们的责任" ——
//	否则每个使用方都要自己去加一层锁，迟早有人忘了加。
func (r *Renderer) emit(e observ.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tracer.Trace(e)
}

// failureReasons 把失败原因压成一行（只取前 3 条，避免错误信息本身变成大文本）。
func failureReasons(failed []schema.PageFailure) string {
	parts := make([]string, 0, len(failed))
	for i, f := range failed {
		if i >= 3 {
			parts = append(parts, "...")
			break
		}
		parts = append(parts, fmt.Sprintf("第 %d 页：%s", f.PageIndex+1, f.Reason))
	}
	return strings.Join(parts, "；")
}
