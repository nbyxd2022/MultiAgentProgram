// Package budget 提供**四维预算守卫**：步数 / prompt token / completion token / 成本。
//
// # 语义：检查与记账**分离**
//
//	Allow(need)      检查，**不记账** —— "还能不能做这一步？"
//	ReportUse(used)  **成功才**记账
//
// 这是从企业仓库的 `Limiter` 直接学来的，而理由在它的接口注释里：
//
//	"每次请求先 Allow 检查是否受限，生成成功后再 ReportUse 计入窗口。"
//
// # ⚠️ 已知的 TOCTOU 窗口 —— 而**在我们这里它不存在**（但原因要说清）
//
// 上面那条注释接着说："注意 Allow/ReportUse 分离存在固有的 TOCTOU 窗口
// （**高并发突发时可能多放一次**），这是「成功才计入」语义的折中。"
//
// **那句话对它的系统是事实描述，对我们是照抄。** 我们的形态是：
//
//	CLI 单进程、图**串行**跑（`Runner.drive` 是一个 for 循环）
//
// 串行意味着 `Allow` 之后**必然**先 `ReportUse` 才轮到下一个 —— **那个窗口不存在**。
//
// > ### 所以这里要做的不是"照抄一句注释"，是问"它为什么成立"
// > 企业仓库是 HTTP 服务（真并发），我们是单进程串行。
// > **同一句话在两处的真假不同，而错的是"照抄"这个动作。**
//
// 触发条件（什么时候它对我们才成立）：**图开始并行执行时**。
// 那时要么接受这个窗口，要么改成"事前预留 + 事后结算"。
//
// # 为什么要"成功才计入"（而不是"开始就扣"）
//
//	事前扣 → 一次**失败**的运行也算了钱 → 那是**持续的系统性偏差**
//	事后记 → 只有真花掉的才算 → 准确；代价是上面那个窗口
//
// **我们接受"可能超一点"，拒绝"每次都多算"** —— 判据是偏差的方向：
// 「偶尔多花」可以容忍，「永远算贵」会让人不再相信这个数字。
package budget

import (
	"fmt"
	"strings"
	"time"
)

// NoLimit 是 `Limits` / `Remaining` 里的一个特殊取值：**该维不限**。
//
// ⚠️ 它是 `-1`，而**零值仍然是 0** —— 于是"零"只有一个含义：**上限 0**。
//
//	> 0  上限就是这个数
//	= 0  **上限 0**（真的不让做）
//	< 0  **不限**（显式选择）
//
// 这与 R36 的 `InterruptTTL`（`< 0` = 永不过期）是同一个手法：
// **"不限"是一个需要被写下来的决定，而不是忘记配置的默认结果。**
//
// # ⚠️ 这一条是为了绕开一颗**已经炸过一次的雷**
//
//	设计文档的第一版写的是"**零值表示该维度不限**"（fail-open）——
//	一个忘记配预算的调用方会得到"**无限花钱**"。
//
//	而精读企业仓库的 `EmptyLimiter` 时看到：
//
//		func (l *EmptyLimiter) AllowN(...) *AllowResult { return &AllowResult{} }
//
//	那个零值里 `Limited: false` 读作"不限"✅ 而 `Remaining: 0` 读作
//	"**没额度了**"⚠️ —— **同一个零，两个相反的含义**。
//
// 所以这里把两件事分开：**零 = 限量 0**，**负数 = 不限**。
const NoLimit int64 = -1

// ─── 量 ─────────────────────────────────────────────────────────────

// Usage 是一组量：五维的用量（或**预估**用量）。
//
// # ⚠️ `Elapsed` 曾经不在这里 —— 而那是一个**已经成立的错误**
//
//	R43 之前这里写着："它**不含时间**，因为**时间的算法只有一个**：
//	`Guard` 自己从开始算到此刻。"
//
//	**而那句话在写下的那一刻就已经不成立了**，因为它把**三个不同的东西**
//	塞进了同一个词"时间"：
//
//		① **事实**：这一轮从哪一刻开始 —— 只有 `State.CreatedAt` 知道
//		② **用量**：这一步**实际执行**了多久 —— 只有执行它的那一层知道
//		③ **墙钟**：现在减起点（**含人等待的时间**）
//
//	而 `guarded.started = g.now()` 同时冒充了 ①②③，于是：
//
//		· 它**发明**了一个事实（"这一轮从我被创建时开始"）——
//		  而每次重启都会归零，于是崩溃三次就拿到三倍预算；
//		· 它把**人思考的时间**算进了"这一轮跑了多久"——
//		  而 L3 的核心恰恰是"人可以过很久再回来"。
//
// # 判据：这件事是**加**出来的，还是**量**出来的
//
//	· 加出来的 → 守卫做（它只做加法，**与 token / 成本完全同构**）
//	· 量出来的 → 执行它的那一层做，**报进来**
//
//	而"这一步执行了多久"是**量出来的** ——
//	它和 `PromptTokens` **是同一类东西**：守卫从来不算 token，它只累加调用方报的 token。
//
// > ### 守卫能做对的是**加法**，不是**时间**。
// > 把"读数"当成"算式"，就是它发明事实的方式。
//
// ⚠️ 墙钟（含人等待）**不该是预算判据** —— 那是"进程别永远挂着"的运维约束，
//
//	由 `context` 的 deadline 表达（两者是**两个判据、两个名字**，见 `cmd/l3`）。
type Usage struct {
	Steps            int64
	PromptTokens     int64
	CompletionTokens int64
	CostMilli        int64 // 成本，单位**厘**（整数：**钱不做浮点运算**）

	// Elapsed 是**实际执行**耗掉的时间（不含人等待）。
	//
	// ⚠️ 它是 `time.Duration` 而不是 `int64` —— 与 `Limits.Elapsed` 同一个理由：
	//	把时长塞进 int64 会让"单位是什么"变成只能靠注释记住的事。
	Elapsed time.Duration
}

// Add 返回两个用量之和。
func (u Usage) Add(v Usage) Usage {
	return Usage{
		Steps:            u.Steps + v.Steps,
		PromptTokens:     u.PromptTokens + v.PromptTokens,
		CompletionTokens: u.CompletionTokens + v.CompletionTokens,
		CostMilli:        u.CostMilli + v.CostMilli,
		Elapsed:          u.Elapsed + v.Elapsed,
	}
}

// String 是给人和模型看的一行。
func (u Usage) String() string {
	return fmt.Sprintf("步数 %d / prompt %d / completion %d / 成本 %s / 耗时 %s",
		u.Steps, u.PromptTokens, u.CompletionTokens, yuan(u.CostMilli),
		u.Elapsed.Round(time.Millisecond))
}

// ─── 额度 ───────────────────────────────────────────────────────────

// Limits 是四维**额度**（上限，或者"剩余"）。
//
// # ⚠️ 它与 `Usage` 字段完全相同，却分成两个类型 —— 为什么
//
//	因为**混用的后果是静默的**：把上限当成实际用量（或反过来）会得到一个
//	"看起来合理"的错数字，而没有任何东西会报错。
//
//	⭐ 判据与 R39 的 `Kind` 相同：**这个错误有多容易被发现？**
//	  而 R39 里我**拒绝**给 `Allow` / `ReportUse` 的参数分类型 ——
//	  因为那里方法名就在那一行上，混用一眼可见。
//	  **同一个判据、两个相反的决定，因为处境不同。**
type Limits struct {
	Steps            int64
	PromptTokens     int64
	CompletionTokens int64
	CostMilli        int64

	// Elapsed 是"这一轮最多**执行**多久"的额度（`NoLimit` 见下）。
	//
	// ⚠️ R43 之前它不在这个结构里，而是藏在 `guarded.maxWall` ——
	//	于是它**逃过了这个类型的一切规矩**：
	//
	//		· `Dims()` 不列它（它是**孤儿维度**，`DimWall` 常量没有对应字段）
	//		· `Bounded()` 不看它
	//		· `remaining()` 算不出它
	//		· `DescribeLimits()` 不渲染它
	//
	//	**"一个字段该在哪里"不是排版问题 —— 它决定了哪套规矩会管到它。**
	//	现在它与其余四维**完全同构**（加/减/剩余/渲染/描述都自动生效）。
	Elapsed time.Duration
}

// 每一维的名字（**强类型词表** —— 它会被用作判据与错误信息里的分支）。
type Dimension string

const (
	DimSteps      Dimension = "steps"
	DimPrompt     Dimension = "prompt_tokens"
	DimCompletion Dimension = "completion_tokens"
	DimCost       Dimension = "cost"
	DimWall       Dimension = "wall"
)

// Dims 返回五维（**常量顺序**，不是 map 遍历 —— 顺序要稳定）。
//
// ⚠️ `DimWall` 曾经**不在这里** —— 而它是一个有常量、没有字段、没有消费者的孤儿。
//
//	把它收进来之后，`Bounded()` / `remaining()` / `Allow()` 的循环 / `DescribeLimits()`
//	**一处都没有 `if d == DimWall` 的特例**，全部自动生效。
func Dims() []Dimension {
	return []Dimension{DimSteps, DimPrompt, DimCompletion, DimCost, DimWall}
}

// get 取一维的额度。
func (l Limits) get(d Dimension) int64 {
	switch d {
	case DimSteps:
		return l.Steps
	case DimPrompt:
		return l.PromptTokens
	case DimCompletion:
		return l.CompletionTokens
	case DimCost:
		return l.CostMilli
	case DimWall:
		// ⚠️ 这一维的单位是**纳秒**（`time.Duration` 的底层表示）。
		//
		//	换算**只发生在 `lim,get` 与 `Usage.get` 两个函数里** ——
		//	"单位盲"的范围被压到这两处，换来的是它**与其余四维完全同构**。
		//
		//	（判据是**两个错误的代价谁更大**：误读单位的代价被限制在两个
		//	 紧邻注释的表达式里；而"逃过全部规矩"的代价我们已经付过一次了 ——
		//	 它让 `Bounded()` / `DescribeLimits()` 全都看不见时间这一维。）
		return int64(l.Elapsed)
	default:
		return NoLimit
	}
}

// usage 报告某个用量在某一维上的数。
func (u Usage) get(d Dimension) int64 {
	switch d {
	case DimSteps:
		return u.Steps
	case DimPrompt:
		return u.PromptTokens
	case DimCompletion:
		return u.CompletionTokens
	case DimCost:
		return u.CostMilli
	case DimWall:
		return int64(u.Elapsed) // 单位见 `Limits.get`
	default:
		return 0
	}
}

// remaining 返回"还能再做多少"（该维不限时返回 `NoLimit`）。
func (l Limits) remaining(d Dimension, spent Usage) int64 {
	cap := l.get(d)
	if cap < 0 {
		return NoLimit // 不限
	}
	left := cap - spent.get(d)
	if left < 0 {
		left = 0 // **剩余不报负数** —— "还欠多少"不是调用方能用的信息
	}
	return left
}

// Bounded 报告"至少有一维是有上限的"。
func (l Limits) Bounded() bool {
	for _, d := range Dims() {
		if l.get(d) >= 0 {
			return true
		}
	}
	return false
}

// ─── 检查结果 ───────────────────────────────────────────────────────

// Decision 是一次检查的结果。
type Decision struct {
	// OK 表示"可以做这一步"。
	OK bool

	// Exceeded 是**超限的那一维**；零值（""）表示没超。
	//
	// ⚠️ **它是判据** —— 而 `Remaining` 不是（见下）。
	//
	//	用一个**显式的判据字段**，是为了回应 `EmptyLimiter` 那颗雷：
	//	那里的调用方要自己猜"看 Limited 还是看 Remaining"，
	//	而两个字段的零值含义相反。**这里把"看哪个"写死在类型里。**
	Exceeded Dimension

	// Remaining 是**最紧的那一维**的剩余额度。
	//
	// # 为什么是"最紧的那一维"
	//
	//	学企业仓库 `CompositeLimiter` 的 `AllowN`：
	//	"遍历所有子 limiter，任一窗口触发即拒绝；**AllowResult 取最严格窗口
	//	（Remaining 最小）**。"
	//
	//	因为**卡住你的就是最紧的那一维** —— 报其余的是在给无用信息。
	//
	// ⚠️ **它只在 `Exceeded == ""` 时有意义**：超限时"还能做多少"这个问题
	//	已经没有意义了（所以那边 `remaining` 被夹到 0，不报负数）。
	Remaining Limits

	// Notice 是给**模型**看的一句话（可为空）。
	//
	// ⚠️ 这一条来自 L2-9 的实测教训：**预算必须告诉模型** ——
	//	否则模型不知道自己快没额度了，会一直重试到触顶（那次实测：
	//	同角色连续失败 10 次、55321 tokens 且最终失败）。
	//
	//	上把它做成 `BudgetNotice` 拼进 Prompt；**这里把它放进 `Decision`** ——
	//	等于**每次检查都提醒一次**，而不是只在开始时说一次。
	Notice string
}

// ─── 守卫 ───────────────────────────────────────────────────────────

// Guard 是预算守卫。
//
// # ⚠️ 时间由**守卫自己**算，不由调用方给
//
//	判据是"**这件事谁能做对**"：
//
//		时间    → 守卫能做对（记下开始时刻，每次算差值）→ **它做**
//		步数/token/成本 → 只有调用方知道 → **调用方给**
//
//	如果让调用方给时间，那**每个调用点都要记得 `time.Since(start)`** ——
//	而"每个调用点都要记得做同一件事"就是 R23 那条教训的形状。
type Guard interface {
	// Allow 检查"再做一步会不会超"，**不记账**。
	Allow(need Usage) Decision

	// ReportUse 记账。**只在这一步真的做成之后调**（"成功才计入"）。
	ReportUse(used Usage)

	// Snapshot 返回现状（供打印与断言）。
	Snapshot() Snapshot
}

// Snapshot 是一份"预算的现状"。
//
// ⚠️ R43 删掉了它的 `Elapsed` 字段 —— 因为那个数**已经有地方放了**：
//
//	`Limits.Elapsed`  这一轮最多**执行**多久（额度）
//	`Spent.Elapsed`   这一轮已经**执行**了多久（用量）
//
//	而删掉的那个字段存的是"从 `New()` 到此刻" —— 它是**第三个**说法，
//	而"同一个数在两处放"正是 `Usage.Elapsed` 的长注释在讲的那个病。
type Snapshot struct {
	Limits Limits
	Spent  Usage
}

// ─── 实现一：不限额（空对象）────────────────────────────────────────

// Unlimited 是"不限额"的**显式**实现。
//
// ⚠️ 它存在的理由与 `observ.NopTracer` / 企业仓库的 `EmptyLimiter` 完全相同：
//
//	**"不限额"是一个合法的状态，不是错误** —— 于是**没有调用点需要判空**。
//	（`LimiterFactory.Get` 对未注册的 type 也返回 `EmptyLimiter`，
//	 注释原话："安全降级，不返回 nil"。）
type Unlimited struct{}

// NewUnlimited 返回一个不限额的守卫。
func NewUnlimited() *Unlimited { return &Unlimited{} }

func (u *Unlimited) Allow(Usage) Decision {
	return Decision{OK: true, Remaining: allUnlimited()}
}

func (u *Unlimited) ReportUse(Usage) {}

func (u *Unlimited) Snapshot() Snapshot { return Snapshot{Limits: allUnlimited()} }

// allUnlimited 是"五维都不限"的那一份 `Limits`。
//
// ⚠️ 抽成一个函数，是因为**它被写了三遍**（`Allow` / `Snapshot` / 现在的 `NoWall` 归一）
// —— 而那样迟早会漏掉一维（`Elapsed` 就是加进来时最容易漏的那个，
// 因为它是唯一不是 `int64` 字面量的那个）。
func allUnlimited() Limits {
	return Limits{
		Steps:            NoLimit,
		PromptTokens:     NoLimit,
		CompletionTokens: NoLimit,
		CostMilli:        NoLimit,
		Elapsed:          NoWall,
	}
}

// ─── 实现二：有限额 ─────────────────────────────────────────────────

// guarded 是按 `Limits` 检查的守卫。
//
// ⚠️ **零值 `Limits{}` = 四维上限都是 0**（fail-closed）：什么都不让做。
//
//	**要"不限"，请用 `Unlimited`** —— 那是一个显式的对象选择，
//	不是字段的零值。
//
//	⚠️ **五维都适用**，`Elapsed` 不例外（见 `NoWall` 的长注释）。
type guarded struct {
	limits Limits
	spent  Usage
}

// NoWall 表示"不限执行时间"。
//
// # ⚠️ 我一度想给它开个"零值 = 不限"的豁免 —— 而那是错的，理由值得记下来
//
//	当时的论证是："**零时间的运行没有意义**（它和'不许运行'是同一件事），
//	所以这一维的零值可以读作'不限'。"
//
//	**论证听起来成立，但它漏了一个对照**：`Steps: 0` 同样"没有意义"
//	（零步的运行也不存在）—— 而我们从没给 `Steps` 开这个豁免。
//
//	> ### 判据：**如果一条豁免的理由同样适用于别的维度，那它就不是这一维的特性，
//	> 而是"我想给这一维开个后门"。**
//
//	而豁免的代价是**可预期的**：`Limits{}` 的零值含义会**按维分裂**
//	（四维读作"上限 0"、一维读作"不限"）—— 那正是 `EmptyLimiter` 那颗雷的形状
//	（同一个零，两个相反的含义），也正是本包开篇第一条规矩要防的东西。
//
//	所以最后**没有豁免**：五维一条规矩，`NoWall` 与其余 `NoLimit` **完全同构**。
//	代价是老实地付：每个 `Limits{...}` 字面量都要写清时间维（见测试里的 `openExcept`）。
const NoWall = time.Duration(NoLimit)

// New 构造一个按五维检查的守卫。
//
// `opts` 里目前只有一项：`WithWall`（这一轮最多**执行**多久）。
//
// ⚠️ 它**不做任何归一化** —— `Limits{Steps: 10}` 的含义是
//
//	"步数 ≤ 10 **且** 时间上限 0"，于是**一步都不让跑**（fail-closed ✅）。
//
//	要"不限时间"请**显式**写 `Elapsed: NoWall`（或 `WithWall(NoWall)`）——
//	与其余四维要"不限"时写 `NoLimit` 是**同一个动作**。
func New(limits Limits, opts ...Option) Guard {
	g := &guarded{limits: limits}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Option 是守卫的可选配置。
type Option func(*guarded)

// WithWall 设置"这一轮最多**执行**多久"。
//
// ⚠️ 注意是**执行**，不是墙钟 —— 人等待（HITL）的时间**不算在里面**，
//
//	因为那段时间机器什么都没做。理由见 `Usage.Elapsed` 的注释。
//
//	（"别让进程永远挂着"是**另一个**判据，由 `context` 的 deadline 表达 ——
//	 两者是**两个判据、两个名字**，不该共用一个数。）
func WithWall(d time.Duration) Option {
	return func(g *guarded) { g.limits.Elapsed = d }
}

// Allow 见 `Guard`。
//
// # ⚠️ 现在**只有一个循环**，没有时间维的特例
//
//	R43 之前时间维是单独判的（`elapsed >= g.maxWall`），因为 `maxWall`
//	不在 `Dims()` 里 —— 而那次"特殊化"正是它逃过全部规矩的**起点**：
//	特殊化 → 不在 `Dims()` → `Bounded()` 看不见 → `DescribeLimits()` 渲染不出。
//
//	把它收进 `Limits` 之后，五维走**同一条**判据。
//
// # ⚠️ 而这条教训在这里**升级**了（R43 之前它只关于时间维）
//
//	上一版的时间维分支里，我第一版写的是 `Remaining: Limits{}` —— 顺手一个零值。
//	而 `Limits{}` 的零值是"**上限 0**"，读作"五维剩余都是 0"；
//	**而实际上四维可能一点都没超**，只是时间到了。
//
//	> **错误分支里的默认值最容易被写错** —— 写错误分支时注意力在"报什么错"上，
//	> 返回值往往顺手一个零值；而零值在这个类型里恰好有**相反**的含义。
//	>
//	> 那条是**自查**出来的（刚在 R40 写下"判据与剩余要分开"，写代码时仍然漏了一处）。
//	> 现在它不再只关于时间：**任何一个分支只要返回 `Limits` 而不是 `remainingAll()`，
//	> 都会掉进同一个坑** —— 而五维同构之后，"只有一个地方算剩余"了。
func (g *guarded) Allow(need Usage) Decision {
	projected := g.spent.Add(need)
	for _, d := range Dims() {
		cap := g.limits.get(d)
		if cap < 0 {
			continue // 该维不限
		}
		if projected.get(d) > cap {
			return Decision{
				Exceeded:  d,
				Remaining: g.remainingAll(),
				Notice:    g.notice(d, cap, need),
			}
		}
	}
	return Decision{OK: true, Remaining: g.remainingAll()}
}

// notice 生成给**模型**看的那一句话（`Decision.Notice`）。
//
// ⚠️ 时间维是**唯一**需要用"人话"报已用量的维度 —— 因为纳秒对模型没有意义
//
//	（"已用 300000000" 是什么？）。其余四维报数字就好。
//
//	所以那个 `if` **不是**"时间维又一次特殊化"，它是**渲染**上的区别：
//	**判据仍然只有一条**（上面那个循环），这里只负责怎么说。
func (g *guarded) notice(d Dimension, cap int64, need Usage) string {
	if d == DimWall {
		return fmt.Sprintf("时间预算已用尽（上限 %s，已执行 %s）—— 请立刻收尾，"+
			"把手上的结论提交掉，不要开始新的步骤",
			g.limits.Elapsed, g.spent.Elapsed.Round(time.Millisecond))
	}
	return fmt.Sprintf("%s 预算不够了（上限 %d，已用 %d，这一步要 %d）—— "+
		"请基于**已有的**信息收尾，不要开始新的步骤",
		d, cap, g.spent.get(d), need.get(d))
}

// remainingAll 返回五维的剩余额（**最紧的那一维**由使用方从 `Decision.Remaining` 里看）。
//
// ⚠️ 它返回的是**全部五维**，而不是"最紧的一维" —— 因为"最紧"这个选择
//
//	需要一个**判据**（哪个数更小），而五维的单位不同（步数 / token / 厘 / 纳秒），
//	**它们之间没有可比性**。
//
//	企业仓库的 `CompositeLimiter` 能取 min，是因为它的多个窗口**单位相同**
//	（都是"次数"）。我们的五维单位不同 —— 照抄那个 `min` 会得到一个
//	**把不同单位的数放在一起比大小**的错误。
//
//	⚠️ R43 之后这条理由**更强了**：以前最"离谱"的对比是"步数 vs 厘"（至少都是计数），
//	  现在多了"**纳秒 vs 步数**" —— 那是**量纲**都不同的两个东西。
//
//	所以：`Remaining` 给全五维，**"最紧"由使用方按它关心的那一维去看**。
//	（`Notice` 里会直接点名是**哪一维**不够了 —— 那才是真正有用的信息。）
func (g *guarded) remainingAll() Limits {
	return Limits{
		Steps:            g.limits.remaining(DimSteps, g.spent),
		PromptTokens:     g.limits.remaining(DimPrompt, g.spent),
		CompletionTokens: g.limits.remaining(DimCompletion, g.spent),
		CostMilli:        g.limits.remaining(DimCost, g.spent),
		Elapsed:          time.Duration(g.limits.remaining(DimWall, g.spent)),
	}
}

// ReportUse 见 `Guard`。
//
// # ⚠️ 它是**唯一**能推进"已执行多久"的入口
//
//	而这意味着**它必须收到 `Elapsed`** —— 守卫不会自己去问时钟
//	（那正是 R43 修掉的东西：见 `Usage.Elapsed`）。
//
//	⭐ 这条也能变成一条**断言**：一个调用方如果**永远报 `Elapsed: 0`**，
//	  那么时间这一维**形同虚设**。所以 `TestWallActuallyTripsOnReportedElapsed`
//	  走的是真实路径（报 → 累加 → 拦），而不是"构造一个已经超时的守卫"。
func (g *guarded) ReportUse(used Usage) {
	g.spent = g.spent.Add(used)
}

// Snapshot 见 `Guard`。
func (g *guarded) Snapshot() Snapshot {
	return Snapshot{Limits: g.limits, Spent: g.spent}
}

// ─── 小工具 ─────────────────────────────────────────────────────────

// yuan 把"厘"渲染成人读的形式（12345 厘 → ¥12.345）。
func yuan(milli int64) string {
	if milli == 0 {
		return "¥0"
	}
	sign := ""
	if milli < 0 {
		sign, milli = "-", -milli
	}
	return fmt.Sprintf("%s¥%d.%03d", sign, milli/1000, milli%1000)
}

// DescribeLimits 把上限渲染成一行（**消费者是 `cmd/l4` 的那张预算表**）。
//
// ⚠️ 它先问 `Bounded()` —— 因为"四维全都不限"渲染成
//
//	"steps=不限 / prompt_tokens=不限 / …" 是一句又长又没用的话。
//	（`Bounded()` 的唯一消费者就是这里。）
func DescribeLimits(l Limits) string {
	if !l.Bounded() {
		return "不限额"
	}
	parts := make([]string, 0, len(Dims()))
	for _, d := range Dims() {
		v := l.get(d)
		if v < 0 {
			parts = append(parts, fmt.Sprintf("%s=不限", d))
			continue
		}
		switch d {
		case DimCost:
			// ⚠️ 这两维**要换算才像人话**（厘 → 元 / 纳秒 → 时长）。
			//	其余三维是纯计数，直接打数字。
			parts = append(parts, fmt.Sprintf("%s=%s", d, yuan(v)))
		case DimWall:
			parts = append(parts, fmt.Sprintf("%s=%s", d, time.Duration(v)))
		default:
			parts = append(parts, fmt.Sprintf("%s=%d", d, v))
		}
	}
	return strings.Join(parts, " / ")
}
