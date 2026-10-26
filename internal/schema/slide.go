package schema

// Slide 是 L1 的「结构化产物」——生成器与评审者之间的公共语言。
//
// 为什么需要它（而不是让 Critic 去读 HTML 或读自然语言）？
//
//	这是面试题"多 Agent 之间传自然语言还是结构化数据"的最小答案：
//	  - 传自然语言 → Critic 要"先理解再挑错"，误差叠加，且无法校验；
//	  - 传 HTML     → Critic 得解析 DOM，脆，而且和渲染实现强耦合；
//	  - 传 Slide    → 两端共享同一个类型，字段可枚举、可校验、可比较。
//
// 它与 issues.go 的分工：
//
//	Slide  = 被评审的**产物**
//	Report = 评审的**结论**
//	两者都是"协议"，都**不带业务逻辑**——逻辑属于工具与 Agent。
type Slide struct {
	Title   string   `json:"title"`
	Bullets []string `json:"bullets"`
}

// L1 的版面约束。
//
// 为什么这些数字要放在协议层，而不是散落在 layout_check 和 Prompt 里？
//
//	因为"约束写在两处，两处都会失守"——这正是 Q4（P3）讲的坑。
//	放在这里之后：
//	  · Prompt 由它生成（告诉模型 3-5 条）；
//	  · layout_check 由它执行（真的按 3-5 条判）；
//	  · 单一真相来源，改一处即全局生效。
//
// 注意：当前 `render_slide.go` 的 Description 里手写了"2-5 条"，与这里的
// MinBullets=3 **不一致**——这是待修的债，下调 render_slide 时一并改掉。
const (
	// MinBullets / MaxBullets 是一页幻灯片的要点条数区间。
	MinBullets = 3
	MaxBullets = 5

	// MaxTitleRunes 是标题的字数上限（与 render_slide 的 "不超过 30 个字" 对齐）。
	MaxTitleRunes = 30

	// MaxBulletRunes 是单条要点的字数上限。
	MaxBulletRunes = 40
)
