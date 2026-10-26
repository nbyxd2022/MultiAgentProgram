package schema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 这个文件定义 L2 的**结构化协议**：Agent 之间到底传什么。
//
// 面试考点 Q6：「多 Agent 之间传自然语言还是结构化数据？」
// 这里是我们的答案，也是 L1 那个最小版本（Slide + Issue）的正式化。
//
// 三个设计决策（每一个都直接针对企业仓库 A2/A3 的坑）：
//
//  1. **统一信封 + 类型化 Payload**
//     黑板上只存一种东西（Artifact），取用者再解成具体类型。
//     于是黑板不必为每种产物写一套读写逻辑（存储与语义分层，原则五），
//     而"具体类型"又保证解出来是强类型、可校验的。
//
//  2. **handoff 只传 {ref, summary}，不传 payload 全文**（原则四）
//     上下文里只放"决策所需的最小信息"；要全文的人，自己拿 ref 去黑板取。
//
//  3. **summary 由代码派生，不由模型自述**
//     L1 的教训："重要状态放在数据里"。模型自述的摘要**可能说谎**，
//     代码从结构化字段投影出来的不会。

// Kind 是产物的种类。
type Kind string

const (
	// KindTopic 是**输入产物** —— 它不是任何角色产出的，是代码写进去的。
	//
	// ⚠️ 它存在的唯一理由，是让**根节点也有上游**（见 06-L3设计.md §10 问题 2）：
	//
	//	root 节点没有上游 → 它的"新鲜度"无从判断 →
	//	于是"用户改了主题"这件事**没有任何一层能发现**。
	//
	//	把输入也变成黑板上一份产物之后：
	//	  topic@1 ──▶ research@1 ──▶ outline@1 ──▶ …
	//	"新鲜"这条判据就**没有根节点特例**了，StaleReasons 一路通用。
	KindTopic Kind = "topic"

	KindResearch Kind = "research"
	KindOutline  Kind = "outline"
	KindDesign   Kind = "design"
	KindRender   Kind = "render"
)

// Valid 报告种类是否合法。
//
// 用途：校验外部输入（尤其是模型给出的名字）。
// 脏数据不能进系统 —— 这是**原则一（确定性兜底）在协议层的落地**，
// 也正是企业仓库 A1（路由全靠自然语言、无法校验）缺的那一环。
func (k Kind) Valid() bool {
	switch k {
	case KindTopic, KindResearch, KindOutline, KindDesign, KindRender:
		return true
	default:
		return false
	}
}

// Payload 是"能被放进黑板的产物"必须满足的契约。
//
//	Kind()      —— 我是谁（信封上的 kind 从这里取，调用方传不进来）
//	Summarize() —— 我该怎么被摘要（**代码派生**，不是让模型写）
//
// 为什么把这两个方法写进接口，而不是让调用方当参数传？
//
//	因为"我是什么"和"我该怎么被摘要"是**产物自己的知识**。
//	交给调用方，一定会出现"同一个产物、两处写法不一致"。
//	契约只能有一个来源（单一真相来源）。
type Payload interface {
	Kind() Kind
	Summarize() string
}

// Reader 是"按引用取回产物"的**最小能力**。
//
// ⚠️ 它是**只读**的 —— 接口里没有 `Put`，所以拿到它的人**写不了黑板**。
//
// # 为什么需要这样一个窄接口
//
// 因为有两个调用方**都只需要"读"**：
//
//	· `SubAgent` 要按需取回上游全文（原则四：要全文的人自己伸手去取）；
//	· `graph.Node` 也是。
//
// 而它们各自的构造函数拿到的都是 `*state.Blackboard`（可读可写）。
// 直接把它递下去，就等于**顺手把"写"也给了**。
//
// "子 Agent / 节点不要写黑板"是一句约定 —— 而约定会漂移。
// **把不该有的能力从类型上拿掉，比写一句"请不要这样做"可靠。**
//
// `*state.Blackboard` 天然满足它（方法签名 `Get(ref string) (Artifact, error)` 一致），
// 所以调用方**不需要任何适配代码** —— 这是"面向接口"能免费拿到的部分。
type Reader interface {
	Get(ref string) (Artifact, error)
}

// Partial 是**可选能力**：产物知道自己有没有留下缺口。
//
// 为什么不把它并进 `Payload`？
//
//	因为绝大多数产物**不可能残缺**：Research 要么产出了几条发现、要么一条都没有；
//	Outline 要么是完整的 3 页、要么根本交不上来。强迫它们都写一个恒返回 nil 的方法，
//	只会让真正的契约（Kind / Summarize）被噪音淹没。
//	→ 接口要小，能力要可探测（用类型断言）—— 与 `StatsReporter` 同一个取舍。
//
// ⚠️ 它是 **L2-2** 那个真 bug 的修复：
//
//	`renderer` 支持"部分成功"（`RenderResult.Failed`），而编排者的完成判断
//	只看"黑板上有这份产物"。于是 3 页里失败 1 页，系统依然会宣布"必需产物齐全"。
//	**"存在"不等于"完整"。**
//
// 它与 L1-3（"没检查" ≠ "没问题"）是同构的：两者都是
// **用一个更弱的判据，去代替一个更强的判据**。
type Partial interface {
	// Shortfall 返回未完成的部分；空（或 nil）表示完整。
	//
	// 返回值是**给人和给下游看的短句**，例如 "第 3 页：写盘失败"。
	// 注意它不该是结构化字段的堆砌 —— 那是 payload 的事，信封只放"一句话能说清的事"。
	Shortfall() []string
}

// ─── 统一信封 ───────────────────────────────────────────────────────

// Basis 是"我依据了哪一份产物"。
//
// ⚠️ 为什么是**结构**（key + version），而不是 `"outline@1"` 这样的字符串？
//
//	因为它**要给代码比较**。字符串形态必须先解析再比较，而"格式"与"解析"
//	一旦分处两地就一定会漂移（`RefOf` 在 schema、解析器在 state ——
//	改一个忘了另一个，比较就会**静默失效**）。
//
//	字符串形态是**给人看的**（日志 / 模型上下文），那是 `RefOf` 的职责。
//
//	→ 给模型看的是字符串，给代码比的是结构。两者不要混。
type Basis struct {
	Key     string `json:"key"`
	Version int    `json:"version"`
}

// BasisOf 从产物生成一条依据引用。
func BasisOf(a Artifact) Basis { return Basis{Key: a.Key, Version: a.Version} }

// Ref 返回给人看的形态（与 RefOf 一致）。
func (b Basis) Ref() string { return fmt.Sprintf("%s@%d", b.Key, b.Version) }

// Artifact 是黑板上的一份产物（统一信封）。
//
// 注意它**没有**把 payload 展开成字段：payload 是不透明的字节，
// 只有真正要用它的人（拿着 ref 的那一跳）才去解它。
//
// 信封上除了"身份"（Key/Kind/Version/ProducedBy），还挂了三样**元信息**：
//
//	Summary   —— 我该怎么被摘要（给模型看的 prose）
//	Shortfall —— 我有没有留下缺口（L2-2）
//	BasedOn   —— **我是在哪一版上游之上做出来的**（L2-1 血缘）
//
// 三者的共同点：**它们都不是业务内容，却都必须跟着产物一起流转。**
// 挂在信封上，取用者就不必解码 payload 才能拿到 ——
// 这条边界很关键（一旦编排者能解码，它就会开始读字段，那就是 A2/A3）。
type Artifact struct {
	Key        string          `json:"key"`
	Kind       Kind            `json:"kind"`
	Version    int             `json:"version"`
	ProducedBy string          `json:"produced_by"`
	Summary    string          `json:"summary"`
	Shortfall  []string        `json:"shortfall,omitempty"`
	BasedOn    []Basis         `json:"based_on,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

// NewArtifact 打包一份产物。
//
// Version **不在这里赋值**：版本号是"存储"的职责，由黑板在 Put 时分配。
//
//	谁拥有它，谁负责维护它 —— 否则每个调用方都会自己拍一个版本号，必然撞车。
//
// 同理，Kind 也不接受调用方传参，而是向产物自己索取（看 Payload 接口的注释）。
func NewArtifact[T Payload](key, producedBy string, payload T) (Artifact, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Artifact{}, fmt.Errorf("序列化产物失败 (key=%s): %w", key, err)
	}

	a := Artifact{
		Key:        strings.TrimSpace(key),
		Kind:       payload.Kind(),
		ProducedBy: strings.TrimSpace(producedBy),
		Summary:    payload.Summarize(),
		Payload:    raw,
	}

	// 可选能力：产物自愿自报缺口。
	//
	// 注意这个位置 —— 与 `Summary` **完全同构**：
	// 都是在**产物刚刚被造出来、信息最全的那一刻**向它自己问一次，
	// 然后把答案固化到信封上。
	//
	// 这样做换来一个很重要的边界：**消费方（编排者）永远不需要解码 payload**。
	// 它只读信封上的几句话 —— 所以它不会重新开始"读业务字段"（A2/A3 的坑）。
	if p, ok := any(payload).(Partial); ok {
		if gaps := p.Shortfall(); len(gaps) > 0 {
			a.Shortfall = gaps
		}
	}
	return a, nil
}

// Validate 校验信封的完整性（不涉及 payload 的内部语义）。
//
// 注意 key 用的是 `strings.TrimSpace(...) == ""` 而不是 `== ""`：
// 校验层必须把这一类"看起来有值、其实是空白"的脏数据一次挡住。
// （本文件第一版就是 `== ""`，于是纯空格的 key 直接钻过去了 —— 由测试抓出来。）
//
// 校验的责任是"**不管谁构造的都要挡**"：
// 手工构造的、从 JSON 解出来的、模型返回的，都可能带脏值。
func (a Artifact) Validate() error {
	switch {
	case strings.TrimSpace(a.Key) == "":
		return fmt.Errorf("产物缺少 key（不能为空或纯空白）")
	case !a.Kind.Valid():
		return fmt.Errorf("产物 %s 的 kind=%q 不合法", a.Key, a.Kind)
	case a.Version < 1:
		return fmt.Errorf("产物 %s 的 version=%d 不合法（版本由黑板分配，从 1 开始）", a.Key, a.Version)
	case len(a.Payload) == 0:
		return fmt.Errorf("产物 %s 的 payload 为空", a.Key)
	}

	// 信封上新增的字段也要进校验 —— "不管谁构造的都要挡"。
	//
	// 一条 Version<1 的依据永远不会与任何真实版本相等，于是它会让
	// **每一次新鲜度检查都报过期** —— 一个脏字段把整条链永久卡死。
	// 这类"永久性误报"比缺一个校验难查得多，所以在入口就挡掉。
	for i, b := range a.BasedOn {
		if strings.TrimSpace(b.Key) == "" || b.Version < 1 {
			return fmt.Errorf("产物 %s 的第 %d 条依据不合法（应含 key 与 >=1 的 version）", a.Key, i)
		}
	}
	return nil
}

// Decode 把信封里的 payload 解回具体类型，并**双向校验 Kind**。
//
// 为什么要双向校验（信封说它是什么 + 产物自己也必须这么说）？
//
//	只信信封，就可能拿到一个"装错了"的产物 ——
//	这是最隐蔽的一类故障：类型没报错、字段都在，但**语义全是错的**。
//	（比如把 Outline 装进 Kind=research 的信封，下游会照着错误的假设去读字段。）
func Decode[T Payload](a Artifact) (T, error) {
	var out T
	if err := json.Unmarshal(a.Payload, &out); err != nil {
		return out, fmt.Errorf("解析产物失败 (key=%s): %w", a.Key, err)
	}
	if got := out.Kind(); got != a.Kind {
		return out, fmt.Errorf("产物 %s 的 kind 不一致：信封=%q，payload=%q", a.Key, a.Kind, got)
	}
	return out, nil
}

// ─── handoff：Agent 之间真正传递的东西 ──────────────────────────────

// Handoff 是一次跨 Agent 的交接。
//
// 只有两个字段，**这是故意的**：payload 全文不进上下文（原则四）。
// 想要全文的，拿 Ref 去黑板取。
//
// 对照企业仓库 A2/A3：它让子 Agent 用自然语言把产物回传 Supervisor，
// 再由 Supervisor "逐字段提取重组" —— 让概率模型做无损数据搬运，必然丢字段。
// 我们这里**没有任何一个 Agent 需要复述产物内容**。
type Handoff struct {
	Ref     string `json:"ref"`     // 黑板上的引用（含版本）
	Summary string `json:"summary"` // 代码派生的"形状投影"
}

// RefOf 返回"带版本"的引用，形如 `outline@2`。
//
// 为什么要带版本？因为这样引用就是**不可变**的：
// 黑板追加新版本时，旧引用依然能解析到它当时指向的那一版。
// 不带版本的引用会在返工/重试时"指向漂移"，让日志和回放全部失真。
func RefOf(a Artifact) string {
	return fmt.Sprintf("%s@%d", a.Key, a.Version)
}

// HandoffOf 从产物生成一次交接。
func HandoffOf(a Artifact) Handoff {
	return Handoff{Ref: RefOf(a), Summary: a.Summary}
}

// HandoffsOf 是一批产物的交接清单（按传入顺序）。
//
// ⚠️ 它放在 `schema` 而不是某个调用方里，是因为**已经有两个调用方了**：
// L2 的 `subagentTool` 与 L3 的 `Runner`。两份实现就是两种漂移的可能 ——
// 而"上游清单长什么样"恰恰是协议的一部分（"单一真相来源"）。
func HandoffsOf(arts []Artifact) []Handoff {
	out := make([]Handoff, 0, len(arts))
	for _, a := range arts {
		out = append(out, HandoffOf(a))
	}
	return out
}

// BasesOf 是一批产物的血缘记录（按传入顺序）。
//
// 与 `HandoffsOf` 成对出现，理由相同：给模型看的是 `Handoff`（字符串），
// 给代码比的是 `Basis`（结构），两者都从同一批产物派生。
func BasesOf(arts []Artifact) []Basis {
	out := make([]Basis, 0, len(arts))
	for _, a := range arts {
		out = append(out, BasisOf(a))
	}
	return out
}

// ─── 输入产物（不是任何角色产出的）───────────────────────────────────

// Topic 是"这次要做什么"。
//
// ⚠️ 它为什么是一份**产物**，而不是 State 里的一个字段？
//
//	因为要让**根节点也有上游**。见 KindTopic 的注释。
//
// 顺带一个不显眼但重要的性质：它的 `ProducedBy` 是 `"code"` 而不是角色名。
// 于是**"这一版有没有模型参与"在信封上是可读的** ——
// 排查时一眼就能看出"内容错了不可能是模型的问题"。
type Topic struct {
	Text string `json:"text"`
}

func (Topic) Kind() Kind { return KindTopic }

func (t Topic) Summarize() string { return fmt.Sprintf("topic：%s", t.Text) }

// ─── 各阶段的产物 ───────────────────────────────────────────────────

// Finding 是 research 阶段的一条发现。
//
// 注意 Source 字段：这是 **L1-5（断言无来源锚点）** 的最小修复 ——
// 每个断言都必须能说清"从哪来的"，否则下游的 Critic 只能凭"感觉像不像真的"。
type Finding struct {
	Claim  string `json:"claim"`
	Source string `json:"source"`
}

// Research 是 research 阶段的产物。
type Research struct {
	Topic    string    `json:"topic"`
	Queries  []string  `json:"queries"`
	Findings []Finding `json:"findings"`
}

func (Research) Kind() Kind { return KindResearch }

// Summarize **只投影"形状"，不含正文**。
//
//	判据：摘要必须能让下一跳判断"要不要去黑板取全文"，
//	并且能让人读懂这条链。正文属于 payload，不属于摘要。
func (r Research) Summarize() string {
	return fmt.Sprintf("research「%s」：%d 条查询 / %d 条发现", r.Topic, len(r.Queries), len(r.Findings))
}

// PageOutline 是大纲里的一页。
type PageOutline struct {
	Index  int      `json:"index"`
	Title  string   `json:"title"`
	Points []string `json:"points"`
}

// Outline 是 outline 阶段的产物。
type Outline struct {
	Pages []PageOutline `json:"pages"`
}

func (Outline) Kind() Kind { return KindOutline }

func (o Outline) Summarize() string {
	titles := make([]string, 0, len(o.Pages))
	for _, p := range o.Pages {
		titles = append(titles, p.Title)
	}
	return fmt.Sprintf("outline：%d 页 %s", len(o.Pages), joinShort(titles, 4, 90))
}

// PageDesign 是某一页的版面设计结果。
//
// ⚠️ 这里**故意没有** PageTitle 之类的字段。
//
//	标题已经在大纲里了。把它复制一份过来，就是"同一条信息存在两处" ——
//	两处迟早不一致（模型抄错一个字，或者返工时只改了一边）。
//	渲染端要标题，就同时读 outline 与 design。
//
// 这条与 `PageBlueprint`（企业仓库）形成对照：那个结构里塞了 hints/img 等
// 七八个字段，每个都是一次"复制粘贴"的机会。
type PageDesign struct {
	PageIndex int    `json:"page_index"`
	Layout    Layout `json:"layout"`
	Note      string `json:"note"`
}

// Design 是 designer 阶段的产物。
type Design struct {
	Pages []PageDesign `json:"pages"`
}

func (Design) Kind() Kind { return KindDesign }

func (d Design) Summarize() string {
	layouts := make([]string, 0, len(d.Pages))
	for _, p := range d.Pages {
		// 摘要里带上页序，让下一跳不必去黑板就知道"第几页是什么版式"
		layouts = append(layouts, fmt.Sprintf("P%d:%s", p.PageIndex+1, p.Layout))
	}
	return fmt.Sprintf("design：%d 页版式 %s", len(d.Pages), joinShort(layouts, 4, 90))
}

// PageFailure 是一页渲染失败的原因。
type PageFailure struct {
	PageIndex int    `json:"page_index"`
	Reason    string `json:"reason"`
}

// RenderResult 是 renderer 阶段的产物。
//
// ⚠️ 注意它**没有** Files 以外的正文 —— 只有**文件路径**（原则四：大产物外置，上下文里只留指针）。
//
// 更值得注意的是 `Failed` 字段的存在：它把"**部分成功**"表达成了一个**合法的产物**。
//
//	对照 L1 的结论"超限 ≠ 失败，应当降级交付 + 说明"：
//	3 页里渲染成功 2 页、失败 1 页，正确的结果**不是**整体 error、
//	而是"交付这 2 个文件 + 明确说明第 3 页为什么没出来"。
//
//	把它放进产物（而不是只写日志），下游与人都能看见 —— 这是"失败也要进数据"。
type RenderResult struct {
	Files  []string      `json:"files"`
	Failed []PageFailure `json:"failed,omitempty"`
}

func (RenderResult) Kind() Kind { return KindRender }

// Shortfall 实现 Partial：**把"没出来的那几页"报出来**。
//
// 这是 L2-2 修复的关键一环。在此之前，`Failed` 只在 `renderer.go` 里被构造、
// 在 `cmd/l2` 里被打印 —— **全仓没有任何一处"判断逻辑"读它**。
// 于是"2 成功 1 失败"在编排者眼里和"3 页齐全"长得一模一样。
//
// 现在它成为信封上的 `Shortfall`，编排者的终止判断看得见它了。
func (r RenderResult) Shortfall() []string {
	if len(r.Failed) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.Failed))
	for _, f := range r.Failed {
		out = append(out, fmt.Sprintf("第 %d 页：%s", f.PageIndex+1, f.Reason))
	}
	return out
}

func (r RenderResult) Summarize() string {
	s := fmt.Sprintf("render：%d 个文件 %s", len(r.Files), joinShort(r.Files, 4, 110))
	if len(r.Failed) > 0 {
		// 摘要里必须体现失败 —— 否则下一跳（或人）会以为"全都成功了"
		s += fmt.Sprintf("；%d 页未产出", len(r.Failed))
	}
	return s
}

// ─── 新鲜度：一份产物"有"，但它还"对"吗？─────────────────────────────

// Latest 按 key 取当前最新版；不存在时返回 false。
//
// 做成**参数**而不是直接依赖黑板，是为了让这条规则能在 schema 层被单测 ——
// 它描述的是协议语义（"什么叫过期"），不该只能通过一个真实黑板才能验证（原则五）。
type Latest func(key string) (Artifact, bool)

// StaleReasons 检查一份产物的**依据链**是否已经过期。
//
// 这是 **L2-1** 的修复。它回答的问题很朴素：
//
//	**一份产物"有"，但它还是"对"的吗？**
//
// 在它之前，黑板上只有两个概念：**存在性**（有没有）与**可追溯性**（哪一版）。
// 缺的正是**新鲜度** —— 而恰恰是它决定了下游能不能放心地用这份产物。
//
// 静默出错的场景（修复前，全程无任何一层能发现）：
//
//	outline@1 ──> design@1        第 1 轮（design 的版式按 outline@1 的页面做）
//	outline 重跑 ──> outline@2    第 2 轮
//	再渲染：collectInputs 取到 outline@2 + design@1
//	        renderer 只校验"页数一致" → 3 == 3 → 通过 → **新内容配旧版式**
//
// 由此得到一条判据，它就是 **L2 版的 `Deliverable()`**：
//
//	一份产物**有效** ⟺ ① 存在；② 完整（`Shortfall` 为空）；
//	                  ③ **依据链上的每一环都仍指向当前最新版**。
//
// ⚠️ 遍历是**传递**的。两种过期都要能发现：
//
//	· 直接过期：`design@1` 的依据 `outline@1` 已经不是最新；
//	· 间接过期：`design@1` 的依据 `outline@1` 仍是最新，**但 outline@1 的依据过期了**。
//
//	只查一层只能发现前者 —— 而后者在依赖链变长后一定会出现。
func StaleReasons(a Artifact, latest Latest) []string {
	var (
		out     []string
		visited = make(map[Basis]bool) // 按**产物**去重（同时兜住环）
	)

	// walk 是**后序遍历**：先把上游全都走一遍，再报自己。
	//
	// 为什么顺序这么重要？因为这份列表是**给模型当待办用的**。
	// 上游没修好之前重做下游只是白做一遍（它还会被同一条规则拦下），
	// 所以列表必须天然是**可执行的修复顺序**：先上游、后下游。
	//
	// 举例：`render@1` 的两个依据是 `outline@1` 与 `design@1`，
	// 而上游 `outline` 已经出了 @2。此时**两者都过期**：
	//
	//	先后序 → design（它先被深走过）→ render     ✅ 可以直接照着做
	//	先自报 → render → design                     ❌ 照着做第一遍必然被拒
	var walk func(cur Artifact)
	walk = func(cur Artifact) {
		self := BasisOf(cur)
		if visited[self] {
			return // 已经走过（也顺带兜住了环）
		}
		visited[self] = true

		var mine []string
		for _, b := range cur.BasedOn {
			now, ok := latest(b.Key)
			if !ok {
				// 上游不在了：那是**存在性**的问题，由 `missingNeeds` 负责报告。
				// 同一个问题不在两处报 —— 反馈啰嗦了，模型反而更难判断。
				continue
			}

			walk(now) // 深入上游（更上游的问题会先入列表）

			if now.Version != b.Version {
				// 拒绝必须**可自愈**（L1-5）：不止说"过期了"，
				// 还要说清"该重新调度谁"。
				//
				// 行首的 `cur.Key` 是**给机器读的**（它就是派活用的工具名），
				// 行内的 `ProducedBy` 是**给人读的**（角色名）。
				// 两者都不能省：前者让编排能自动照做，后者让人一眼看懂。
				mine = append(mine, fmt.Sprintf(
					"%s：%s 依据的 %s 已经更新到 %s（产出者 %s，请重新调度它）",
					cur.Key, RefOf(cur), b.Ref(), RefOf(now), cur.ProducedBy))
			}
		}
		out = append(out, mine...)
	}
	walk(a)

	// 不排序：遍历顺序完全由 BasedOn 的切片顺序决定（DFS），
	// 而 BasedOn 是按 Needs 顺序生成的 —— **天然确定**，不像 map。
	return out
}

// ─── 内部小工具 ─────────────────────────────────────────────────────

// joinShort 把若干短标签拼成一行：**项数与总长都设上限**。
//
// 摘要本身绝不能变成大文本，否则它就失去意义了 ——
// 一个和 payload 一样长的"摘要"，只会把原则四架空。
func joinShort(items []string, maxItems, maxRunes int) string {
	parts := make([]string, 0, maxItems+1)
	for i, it := range items {
		if i >= maxItems {
			parts = append(parts, "...")
			break
		}
		parts = append(parts, strings.TrimSpace(it))
	}
	out := "[" + strings.Join(parts, " / ") + "]"

	if rs := []rune(out); len(rs) > maxRunes {
		out = string(rs[:maxRunes]) + "...]"
	}
	return out
}
