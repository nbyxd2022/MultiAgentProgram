package schema

import (
	"fmt"
	"strings"
)

// 这个文件定义 designer 阶段的"词汇表"：版式枚举 + 版式目录。
//
// 它是从企业仓库学来的**长处**（它有 page_presets.json 注册表 + PresetID 校验 +
// 非法值代码兜底），同时避开它的**短处**（define.PptxRequirementOutline 里的
// `DesignStyle string` 是一个自由文本字段，和旁边的枚举 PageType 待遇完全不同）。

// Layout 是版式标签。
//
// 为什么必须是**枚举**，而不是像企业仓库那样用一个自由文本字段（`design_style string`）？
//
//	① 自由文本**无法校验**：模型写 "two-column" / "两栏布局" / "左右分栏" 都"看起来对"，
//	   但渲染端按名字查不到模板，只能猜 —— 最后干脆忽略这个字段。
//	② 自由文本**无法复用**：设计规范散落在每页的自然语言里，改一处不等于改全局。
//	③ 自由文本**会漂移**：第 1 页叫 "cards"，第 3 页叫 "card-grid"，其实是同一个东西。
//
// 枚举的代价是"表达力受限"。这个代价我们**明确接受**，并用 `PageDesign.Note`
// 去补表达力 —— **外壳结构化、血肉自由**，这是从企业仓库
// `PageBlueprint`（PresetID 结构化 + Hints 自由）学到的最有价值的一条设计哲学。
type Layout string

const (
	LayoutCover      Layout = "cover"      // 封面：大标题 + 副标题
	LayoutBullets    Layout = "bullets"    // 要点列表：标题 + 3~5 条要点
	LayoutComparison Layout = "comparison" // 对比：左右两栏对照
	LayoutData       Layout = "data"       // 数据：突出一个关键指标或趋势
	LayoutTimeline   Layout = "timeline"   // 时间线：一组有先后关系的事件
	LayoutQuote      Layout = "quote"      // 引用：单个观点被强调
	LayoutClosing    Layout = "closing"    // 收尾：结论 / 行动号召
)

// MaxNoteRunes 是每页设计说明的长度上限。
//
// 说明是给渲染端与评审看的"页级意图"，一两句话就够；
// 给它设上限，是为了防止它膨胀成一段新 Prompt —— 那等于把结构化设计又退化成自由文本。
const MaxNoteRunes = 60

// LayoutSpec 是版式目录里的一项。
//
// BestFor 字段是**从企业仓库学来的**（它的 page_presets.json 里每个 preset 都带
// `bestFor: ["KPI","指标","数据",...]`）。
//
// 它的价值被严重低估：**它把"选版式"从"猜名字"变成了"按内容特征匹配"。**
// 模型看到 "cover：封面…｜适合：开场、标题页" 时，判断依据是内容，
// 而不是"cover 这个词我认不认识"。
type LayoutSpec struct {
	Layout      Layout
	Description string
	BestFor     string
}

// layoutCatalog 是版式目录（唯一真相来源）。
//
// ⚠️ 注意它的**地位**：它不是"一份给模型看的说明文档"，而是**代码里的常量**。
// Prompt 的选项表由它渲染，校验也查它。
//
//	→ 加一个版式只需在这里加一行：校验、Prompt、错误提示里的合法清单**自动同步**。
//
// 这是 L1-7「约束的单一真相来源」在 L2 的继续兑现。
var layoutCatalog = []LayoutSpec{
	{LayoutCover, "封面：大标题 + 副标题，整页留白", "开场、标题页"},
	{LayoutBullets, "要点列表：标题 + 3~5 条要点，纵向排列", "并列的多个要点、措施、特征"},
	{LayoutComparison, "对比：左右两栏对照，强调差异", "两种方案/前后/优劣的对照"},
	{LayoutData, "数据：用一个突出的大数字或趋势图承载结论", "关键指标、占比、增长率"},
	{LayoutTimeline, "时间线：一组按先后排列的事件", "发展阶段、路线图、演进过程"},
	{LayoutQuote, "引用：单个观点被放大强调", "一句核心判断、专家观点、结论句"},
	{LayoutClosing, "收尾：总结与行动号召", "结尾、行动建议、致谢"},
}

// LayoutCatalog 返回版式目录的**副本**。
//
// 为什么返回副本？因为它是包级变量。直接返回切片会让调用方
// `append` 到共享底层数组，产生"我改了一份、另一份也变了"的幽灵修改
// —— 这正是 L1-6 在 `Report.Merged` 里踩过的坑，同一个教训不重复踩。
func LayoutCatalog() []LayoutSpec {
	out := make([]LayoutSpec, len(layoutCatalog))
	copy(out, layoutCatalog)
	return out
}

// layoutSpecOf 在目录里查一个版式。
func layoutSpecOf(l Layout) (LayoutSpec, bool) {
	for _, spec := range layoutCatalog {
		if spec.Layout == l {
			return spec, true
		}
	}
	return LayoutSpec{}, false
}

// Valid 报告版式是否为已知枚举值。
//
// 注意它是**查目录**，不是 `switch`。这个区别很重要：
//
//	switch 版：新增版式必须记得回来补一个 case，忘了就是"校验放行脏值"
//	查目录版：新增版式 = 在 catalog 加一行，校验**自动**跟着生效
//
// 凡是"必须在多处保持一致"的东西，都不该写成两处。
func (l Layout) Valid() bool {
	_, ok := layoutSpecOf(l)
	return ok
}

// LayoutNames 返回全部合法版式名，供 Prompt 选项表与错误信息共用。
//
// 错误信息里带上它，是为了让拒绝**可自愈**：
// 模型看到 "layout=\"two-column\" 不是合法版式。可选值：cover / bullets / ..."
// 就知道下一步该填什么；只说"不合法"它只能瞎猜（L1-5 的教训）。
func LayoutNames() []string {
	out := make([]string, 0, len(layoutCatalog))
	for _, spec := range layoutCatalog {
		out = append(out, string(spec.Layout))
	}
	return out
}

// IsStructuralPage 报告某页是否为**结构页**（首页 / 末页）。
//
// 结构页在叙事上承担固定角色：首页开场、末页收束。
// 这个概念是从企业仓库学来的（它的 `isStructuralPage`：cover/toc/chapter/ending
// 走与正文页完全不同的渲染路径）。
//
// 我们把它抽成一个共享判断，是因为**不只版式要用它**：
//
//	· designer 用它校验"首页必须是 cover、末页必须是 closing"；
//	· outline 用它区分内容密度要求 —— 封面本来就不该硬凑 3 条要点。
//
// 一个概念一个函数。如果两处各写一遍 `i == 0 || i == n-1`，
// 那么"什么算结构页"这件事就永远没有唯一定义。
func IsStructuralPage(pageIndex, pageCount int) bool {
	if pageCount <= 0 || pageIndex < 0 || pageIndex >= pageCount {
		return false
	}
	return pageIndex == 0 || pageIndex == pageCount-1
}

// RequiredLayout 返回"结构页"必须使用的版式。
//
// 两份 PPT 的首页理应是封面、末页理应是收尾 —— 这是**产品规则**。
//
// 企业仓库里同样存在这个概念，但它主要靠 Prompt 里写"严禁自造"来约束。
// 我们做得更硬：**由代码校验**。
//
// 理由就是 L1 第 4 题那句结论：
//
//	Prompt 是建议，代码是保证。凡是"必须"，都要落到代码。
//
// 但**事前也要告知**（写进提交工具的 Description）——
// 事前说清 + 事后校验，两者配合才不会白白浪费一轮返工。
func RequiredLayout(pageIndex, pageCount int) (Layout, bool) {
	if !IsStructuralPage(pageIndex, pageCount) {
		return "", false
	}
	if pageIndex == 0 {
		return LayoutCover, true
	}
	return LayoutClosing, true
}

// DescribeLayout 返回"版式名 + 说明"，用于错误信息。
func DescribeLayout(l Layout) string {
	if spec, ok := layoutSpecOf(l); ok {
		return fmt.Sprintf("%s（%s）", spec.Layout, spec.Description)
	}
	return string(l)
}

// JoinLayoutNames 把合法版式拼成一行，供 Prompt / 错误信息使用。
func JoinLayoutNames(sep string) string {
	return strings.Join(LayoutNames(), sep)
}
