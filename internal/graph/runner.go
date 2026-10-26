// 本文件是**图运行时**：它把 `Graph` 那张静态结构跑起来。
//
// # 恢复与正常推进是**同一段代码**
//
// 这是"位置由产物推导"带来的最大简化（见 `Graph.Next`）：
//
//	没有游标 → 没有"从游标往后跑"这回事 → **没有单独的 resume 路径**
//
// `Runner.Run` 在磁盘上找到 checkpoint 就接着跑，没有就从零开始。
// 两者的差别只有"黑板上有多少产物" —— 而 `CanSkip` 会照单全收。
//
// # 人的回答也是状态
//
// `Resume` 先把回答**落盘**，再开始跑（见 `Interrupt.Decision` 的注释）。
// 于是"恢复跑了一半又崩了"不是一种特殊情况：磁盘上仍是"已回答未消费"，
// 下次 `Run` **自动重放**这个决定。
//
//	**"重放"不需要任何额外机制 —— 它只是"再读一次状态"。**
package graph

import (
	"context"
	"fmt"
	"maps"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"multiagent/internal/budget"
	"multiagent/internal/checkpoint"
	"multiagent/internal/observ"
	"multiagent/internal/schema"
	"multiagent/internal/state"
)

// ─── 可观测性 ───────────────────────────────────────────────────────

// ⚠️ 这里**曾经有一个 `graph.Event`** 和七个 `EventXxx` 常量（R44 删掉了）。
//
// # 它们去哪了，以及为什么"删"比"迁移"更值得记
//
//	`observ` 包的 `Kind` 常量里早就写着一句"**与 L3 的 `graph.Event` 一一对应
//	（迁移，不是并存）**" —— 也就是说迁移计划**早就写好了**，只是没人执行。
//
//	而 R44 动手去接的时候，第一步是**对比两份字段清单**：
//
//		graph.Event     { Step int; Node string; Kind string; Detail string }
//		observ.Event    { RunID; Span; Layer; Phase; Kind; Err; 正文; 数值; Reconcile }
//
//	对比的结论看起来很明显：**`observ` 缺两个字段**（`Step` 和 `Detail`）。
//
//	> ### ⚠️ 而那个结论是**错的**，或者至少是**半个** ——
//	> **字段清单的差异是中立的**：它既可以读成"那边缺"，也可以读成"这边多"。
//	> 而它**不会自己告诉你该往哪边改**。
//
//	分辨方向的判据是那句老话：**这个字段能被算出来吗？**
//
//		`Step`    **能** —— 它等于数一数 `KindStart` 出现过几次。
//		          一个 step 可以发出多条事件（start + product），
//		          所以"第几步"是**调度器的迭代序号**，由**消费者**数得出来。
//		          → **派生值 → 删掉它**（在哪一边都删）✅
//
//		`Detail`  **不能** —— "已就绪、跳过：research" 是**那一刻**写下的一句话；
//		          回放一份录播时，那些对象（节点 / 产物 / 中断）**早就不在了**。
//		          → **它是数据 → 该存** ✅（所以 `observ.Event` 加了 `Detail`）
//
//	> ### 同一条判据，两个字段，**两个相反的结论**。
//	> 而"以谁为准"这种问法在这里**完全使不上力** ——
//	> 因为差异的两边一个是"多了个派生值"、一个是"少了个数据字段"。
//
// # 顺带发现：`Step` 里 9 处调用有 5 处传的是 `0`
//
//	那也是一个信号 —— 一个"有时候有值、有时候是 0"的字段，
//	通常说明它**不是这一类事件共有的属性**，而是**某个消费者的显示需要**。
//	（现在那个需要由 `cmd/l3` 的 tracer 自己数 —— 它本来就是消费者。）
//
// # 现在的形态
//
//	直接用 `observ.Event`（`Layer: LayerGraph`），**没有第二套类型**。
//	"迁移，不是并存" —— 两套事件形状并存正是我们一路在避免的病（F4）。

// eventKindsOfGraph 说明这一层会发哪些 `observ.Kind`（**文档，不是代码**）。
//
//	KindStart / KindProduct   **成对** —— 只有 start 没有 product 的话，
//	                          观测者看不出"这一跳到底做成了没有"，
//	                          而"开始"和"做成"之间恰好是**最需要被看见**的那一段。
//	KindSkipped               一批产物已就绪，被跳过
//	KindInterrupt / KindResume 停下来等人 / 人给了一个回答（含重启后重放的）
//	KindNotice                一条**需要注意、但不改变执行**的事实
//	                          ⚠️ 它让**不可避免的静默**变得可见：
//	                          有信息但我们选择不说 → 那是可以修掉的静默（不是这一类）；
//	                          信息**本来就不存在**（v1 记录没有 hints 字段）→ 只能放行，
//	                          但**必须让人知道"这里有个洞，且它补不上"**。
//	                          与 `KindSkipped` 的差别：那个说"我跳过了什么"（有结论），
//	                          这个说"我不知道什么"（**没有结论，也要说**）。
//	KindDone                  这一轮全部完成

// ─── 结果 ───────────────────────────────────────────────────────────

// RunStatus 是"这一轮结束时系统处于什么状态"。
//
// 做成枚举而不是一堆 bool（`Completed` / `Interrupted` / `Aborted`），
// 是因为它们**互斥** —— 三个 bool 有 8 种组合，其中 5 种是非法状态。
type RunStatus string

const (
	// StatusCompleted 全部产物齐全。
	StatusCompleted RunStatus = "completed"

	// StatusInterrupted 停在一次中断上等人。
	StatusInterrupted RunStatus = "interrupted"

	// StatusAborted 人说了「不做了」，带着已完成的部分收尾。
	StatusAborted RunStatus = "aborted"

	// StatusBudgetStopped 预算用完，**带着已完成的部分收尾**。
	//
	// # ⚠️ 它与 `StatusAborted` **不是一回事** —— 而那正是它单独存在的原因
	//
	//		`StatusAborted`         **人**说了「不做了」
	//		`StatusBudgetStopped`   **系统**自己的上限到了
	//
	//	把后者归进前者，就是**把系统的决定报告成人的决定** ——
	//	而人看到「不做了」会以为自己按错了什么。
	//
	// # ⚠️ 而它与"出错"**也不是一回事**（见 `drive` 里那个分支的注释）
	//
	//	**我们自己设的上限被触发，不是一个"异常"。**
	//	把它当 error 返回，就是把自己的决定报告成"出事了" ——
	//	而代价是**调用方会把已经做好的产物一起丢掉**。
	StatusBudgetStopped RunStatus = "budget_stopped"
)

// Result 是一轮运行的结果。
type Result struct {
	State *State

	Status RunStatus

	// Interrupted 只在 Status == StatusInterrupted 时非 nil。
	Interrupted *Interrupt

	// StopReason 是"**为什么在这里停下**"的人话说明（只在 `StatusBudgetStopped` 时非空）。
	//
	// ⚠️ 它装的是 `budget.Decision.Notice` —— 那句**本来是给模型看的**
	//	("请基于已有的信息收尾")，而它现在有了第二个读者：**人**。
	//
	//	⭐ 一句写给模型的提示，往往**正好**是人需要的那句 ——
	//	  因为两者缺的是同一件东西：**"现在该停下"这个判断**。
	StopReason string

	// Ran 是这一轮**实际执行过**的节点名（按时间顺序，重试会出现多次）。
	//
	// 它是"跳过"的对照：恢复时 `Ran` 往往很短（大部分被 `CanSkip` 跳过），
	// 而它恰恰是"位置由产物推导"最直观的证据。
	Ran []string
}

// ─── Runner ─────────────────────────────────────────────────────────

// maxRunsPerNodeDefault 是同一节点在**一次运行**里的执行次数上限。
//
// ⚠️ 它是"判据可以不精确，但**绝不能无界**"（06-L3设计.md §10 问题 1）的落地。
//
//	`Outcome.Next == 自己` 让节点可以要求"重做自己"（"素材不够再搜一轮"）。
//	而一个只写"要不要重试"、不写"最多几次"的判据是**无界**的 ——
//	最坏结果不是"多花点 token"，而是**这一轮永远跑不完**。
//
// **为什么它不跨进程**（每次 Resume 是新的一轮，计数重置）：
//
//	一次 `Run` 的边界是"进程活着的一段时间"，而中断会结束它。
//	所以"跨越多次恢复还在重试"意味着**人在反复同意重试** ——
//	那不是死循环，那是有意的。（把它做成跨进程计数要往 `State` 里加字段，
//	触发条件是"真的出现了忽略回答的节点"。现在不加。）
const maxRunsPerNodeDefault = 4

// DefaultInterruptTTL 是中断的默认业务有效期。
//
// ⚠️ 它与"存储层的 TTL"是**两件事**（06-L3设计.md §5.3）：
//
//	业务过期（本字段）  语义：超过它就别恢复了（"这个中断已经没意义了"）
//	存储 TTL            语义：垃圾回收，且必须**远大于**业务有效期
//
// 7 天是"人还会回来接着做"的合理上界 —— 与企业仓库的 `redisExpiration`
// **数量级一致、语义不同**：它在存储层，我们在数据里。
// 所以我们的 `Get` 能区分"过期"与"不存在"，而它的 `Get` 分不清（07-L3复盘.md §4.3）。
const DefaultInterruptTTL = 7 * 24 * time.Hour

// RunnerOptions 是 Runner 的装配参数。
type RunnerOptions struct {
	// Graph 是要跑的图（构造期已经完成全部结构校验）。
	Graph *Graph

	// Store 是持久化。**必填** —— L3 的重点就是"能跨进程恢复"，
	// 一个不落盘的 Runner 无法验证这一点。
	Store checkpoint.Store

	RunID string
	Topic string

	// Hints 是逐节点的额外要求（原 Supervisor 的 `hint` 参数，
	// 现在没有模型来逐轮决定它了，于是它变成装配期的静态配置）。
	Hints map[string]string

	// Tracer 接事件。可以为 nil —— 那会被归一成 `observ.NopTracer{}`。
	//
	// ⚠️ 它**与 `Observer func(Event)` 不是同一个东西**（R44 换了）：
	//
	//	旧的  `func(Event)` —— 一个**函数**
	//	新的  `observ.Tracer` —— 一个**端口**（接口），出口要多少有多少
	//	      （打印 / JSONL 录播 / 内存收集 / 扇出 / 脱敏 / 未来的 Prometheus）
	//
	//	而"出口要多少有多少"这件事，`func(Event)` **做不到** ——
	//	它只能有一个，想要两个就得自己写一层拼接。
	Tracer observ.Tracer

	// MaxRunsPerNode 覆盖默认上限（0 = 用默认）。
	MaxRunsPerNode int

	// Guard 是预算守卫。可以为 nil —— 那会被归一成 `budget.NewUnlimited()`。
	//
	// ⚠️ 归一化（而不是"判空"）与 `Observer` 是同一个手法：
	//	**"不限额"是一个显式的对象，不是"没有守卫"。**
	//	于是循环里那一行不需要 `if r.guard != nil` —— 而那正是 R43 想要的：
	//	**预算这件事只有一个地方做，且永远有人做。**
	Guard budget.Guard

	// InterruptTTL 是中断的**业务有效期**（0 = 用 DefaultInterruptTTL）。
	//
	// ⚠️ 它填的是 `Interrupt.ExpiresAt` —— 而在此之前，
	//	那个字段**没有任何生产代码写过**（07-L3复盘.md L3-2：
	//	一个论证得很漂亮、但没人生产的机制）。
	//
	// # 为什么它在这里，而不是在节点里
	//
	//	"这个中断多久之后就没意义了"是**运行期的策略**，不是节点的事 ——
	//	与 `Interrupt.ID` 由 (RunID, Node, Attempt) 派生是同一个位置：
	//	**节点不操心它。**
	//
	//	放进节点还有一个具体代价：每个节点都要记得把 TTL 传进 `in.Interrupt(...)`。
	//	而"每个调用点都要记得做同一件事"的设计迟早会漏一次（R23 那条教训）；
	//	这里**只有一个地方**设置它。
	//
	// # 三种取值
	//
	//	0    未设置 → 用 DefaultInterruptTTL（零值仍然只有一个含义 ✅）
	//	> 0  用它
	//	< 0  **永不过期** —— 这是一次**显式的选择**：
	//	     "永不过期"是一个需要被写在装配参数里的决定，而不是"忘了设"的默认结果
	InterruptTTL time.Duration
}

// Runner 把图跑起来。
type Runner struct {
	graph  *Graph
	store  checkpoint.Store
	runID  string
	topic  string
	hints  map[string]string
	tracer observ.Tracer

	maxRunsPerNode int
	// interruptTTL 已经归一化过：不为 0；负数 = 永不过期。
	interruptTTL time.Duration
	// guard 已经归一化过：永不为 nil。
	guard budget.Guard
}

// NewRunner 装配一个 Runner，并**在构造期校验**两件最容易搞错的事：
// 图与存储都不能缺，runID / topic 不能空。
//
// 理由与 L2 适配器、`BuildGraph`、`NewFileStore` 是同一条：
// **错配是本程序的 bug，越靠近它的来源发现越便宜。**
func NewRunner(opt RunnerOptions) (*Runner, error) {
	switch {
	case opt.Graph == nil:
		return nil, fmt.Errorf("Runner 需要一个图")
	case opt.Store == nil:
		return nil, fmt.Errorf("Runner 需要一个 checkpoint 存储（L3 的重点就是落盘）")
	case strings.TrimSpace(opt.RunID) == "":
		return nil, fmt.Errorf("run id 不能为空")
	case strings.TrimSpace(opt.Topic) == "":
		return nil, fmt.Errorf("主题不能为空")
	}

	max := opt.MaxRunsPerNode
	if max <= 0 {
		max = maxRunsPerNodeDefault
	}
	// 归一化 Tracer：nil → NopTracer（"没有出口"是一个对象，不是 nil）。
	//
	// ⚠️ 这是 `observ.From` 存在的**唯一**用法（装配的边界调一次），
	//	于是 `emit` 里那一行**永远不需要判空** —— 见 `observ` 里那条注释：
	//	"如果 Tracer 可以是 nil，那每个透传点都要判空，
	//	 而'每个调用点都要记得做同一件事'就是 R23 那条教训的形状。"
	tracer := observ.From(opt.Tracer)
	// 归一化 TTL：0 → 默认；负数原样保留（= 永不过期）。
	ttl := opt.InterruptTTL
	if ttl == 0 {
		ttl = DefaultInterruptTTL
	}
	// 归一化 Guard：nil → Unlimited（"不限额"是一个对象，不是"没有"）。
	guard := opt.Guard
	if guard == nil {
		guard = budget.NewUnlimited()
	}

	return &Runner{
		graph:          opt.Graph,
		store:          opt.Store,
		runID:          strings.TrimSpace(opt.RunID),
		topic:          strings.TrimSpace(opt.Topic),
		hints:          maps.Clone(opt.Hints),
		tracer:         tracer,
		maxRunsPerNode: max,
		interruptTTL:   ttl,
		guard:          guard,
	}, nil
}

// RunID 返回这一轮的身份（演示程序打印用）。
func (r *Runner) RunID() string { return r.runID }

// Run 从**当前状态**继续推进，直到"全部做完 / 停下来等人 / 出错"。
//
// ⚠️ 它**不是"从头开始"**：磁盘上有 checkpoint 就接着跑
// （做完的节点会被 `CanSkip` 跳过），并且**自动重放一个还没被消费的人的决定**。
func (r *Runner) Run(ctx context.Context) (Result, error) {
	st, err := r.load()
	if err != nil {
		return Result{}, err
	}
	return r.drive(ctx, st)
}

// Resume 记下人的回答，然后继续推进。
//
// 三步的**顺序不能换**：
//
//	① 记录本身还能不能读（版本 → 结构 → 过期）
//	② 回答合不合法（含"这个类型要不要内容"）
//	③ **先把回答落盘**，再开始跑
//
// 第 ③ 步最关键：中途崩掉之后磁盘上会留下"已回答但未消费"，
// 于是下次 `Run` 会自动重放它。**宁可重做一遍，不要静默丢失。**
func (r *Runner) Resume(ctx context.Context, d Decision) (Result, error) {
	st, err := r.load()
	if err != nil {
		return Result{}, err
	}

	p := st.Pending
	if p == nil {
		return Result{State: st}, fmt.Errorf(
			"run %s 没有在等待回答的中断（上一次的回答可能已经被消费掉了）", r.runID)
	}
	in := *p

	if err := in.CheckResumable(time.Now()); err != nil {
		return Result{State: st}, err
	}
	if err := d.Validate(in); err != nil {
		return Result{State: st}, err
	}

	// 覆盖已存在的未消费回答：**后到的答案是用户最新的意思**。
	p.Decision = &d
	if err := r.persist(st); err != nil {
		return Result{State: st}, err
	}

	return r.drive(ctx, st)
}

// ─── 主循环 ─────────────────────────────────────────────────────────

// drive 是唯一的主循环 —— **正常推进与恢复走的是同一条路**。
func (r *Runner) drive(ctx context.Context, st *State) (Result, error) {
	plan := planFrom(st)

	// ⚠️ 有一个**还没被回答的**中断时，什么都不做，就地停下来等。
	//
	//	HITL 的语义就是"系统问了问题，就该等人回答"。
	//	如果这里放过去，调度器会因为"中断的那一跳**已经把产物投进黑板了**"
	//	而把它跳过 —— 于是**问题被静默忽略**，
	//	用户以为系统在等他，系统却已经跑完了。
	//
	//	（这也说明了为什么"产出 + 中断"这个组合必须存在：
	//	  它让 `confirm` 之后的继续是**自然的**，
	//	  代价就是必须在这里显式地挡住"没人回答就往下跑"。）
	if st.Pending != nil && st.Pending.Decision == nil {
		r.emit(st.Pending.Node, observ.KindInterrupt, st.Pending.Prompt)
		return Result{State: st, Status: StatusInterrupted, Interrupted: st.Pending}, nil
	}

	// 决定一旦被"读成计划"，就当作已消费。
	//
	// ⚠️ 这里只清**内存**；落盘发生在下一个检查点（循环里每步之后）。
	// 于是"清掉之后、落盘之前崩掉"时，磁盘上仍是"已回答未消费"——
	// **决定会被重放一次**，而那正是我们想要的。
	st.Pending = nil

	if plan.Answer != nil {
		// 报告"人在给一个回答" —— 包括**重启后重放的那个**。
		// 它出现在 Run 路径上是刻意的：重放不是特例，它就是"再读一次状态"。
		r.emit(plan.Answer.node, observ.KindResume,
			fmt.Sprintf("人的回答 %q%s", plan.Answer.d.Intent, noteSuffix(*plan.Answer.d)))
	}

	// ⚠️ 这里**曾经有一个 `step int`**（R44 删掉了）—— 它只被用来喂
	//	`Event.Step` 那个字段。字段没了（它是派生值），它也就没了。
	var (
		ran    []string
		runs   = map[string]int{}
		next   = plan.Force
		answer = plan.Answer
	)

	// 报告"这次进来，哪些已经做完了" —— 恢复时它是最直观的证据：
	// **没有"从游标往后跑"，只有"现在缺什么"。**
	//
	// ⚠️ 必须排除 `plan.Force` 那一跳：它**看起来**已就绪，但这一次会被**强制重做**。
	//	不排除的话会打出一句自相矛盾的话（"跳过：outline" → 然后 outline 就跑了）——
	//	**一句话与紧随其后的行为互相打脸，比没有这句话更糟。**
	if done := r.skipped(st, plan.Force); len(done) > 0 {
		r.emit("", observ.KindSkipped, fmt.Sprintf("已就绪、跳过：%s", strings.Join(done, "、")))
	}

	// abort：不再往下跑，带着已完成的部分收尾。
	if plan.Stop {
		r.emit("", observ.KindDone, "人选择了「不做了」 —— 停止推进，交付已有的部分")
		if err := r.persist(st); err != nil {
			return Result{State: st, Status: StatusAborted, Ran: ran}, err
		}
		return Result{State: st, Status: StatusAborted, Ran: ran}, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return Result{State: st, Status: StatusAborted, Ran: ran},
				fmt.Errorf("图执行被 context 打断（已完成的产物都在磁盘上，可以直接 Run 恢复）: %w", err)
		}

		if next == "" {
			next = r.graph.Next(st)
			if next == "" {
				r.emit("", observ.KindDone, "全部产物齐全，这一轮结束")
				if err := r.persist(st); err != nil {
					return Result{State: st, Status: StatusCompleted, Ran: ran}, err
				}
				return Result{State: st, Status: StatusCompleted, Ran: ran}, nil
			}
		}

		name := next
		next = "" // 强制跳转**只用一次**；之后回到默认调度

		runs[name]++
		if runs[name] > r.maxRunsPerNode {
			return Result{State: st, Status: StatusAborted, Ran: ran}, fmt.Errorf(
				"节点 %s 在一次运行里被执行了 %d 次（上限 %d）—— "+
					"它在反复要求重做自己，请检查它的重做判据是否**无界**",
				name, runs[name]-1, r.maxRunsPerNode)
		}

		// ★ 预算检查：**在开始这一步之前**（"事前问、事后记"）。
		//
		// ⚠️ `need` 里**只有 `Steps: 1`** —— 因为它是唯一**可预估**的一维。
		//
		//	token / 成本要**跑完才知道**，所以它们只能事后记；
		//	而 `Allow` 用的是**投影**（已用 + need），于是那两维**永远晚一步**：
		//	**它们拦不住"这一次"，只能拦住"下一次"。**
		//
		//	> ### 这不是 bug，是"估算"这件事的边界
		//	> 一个量**只有事后才知道**时，任何"事前拦截"都只能基于**上一次**的信息。
		//	> 而这一点**必须写在这里** —— 否则下一个人会以为 token 维坏了。
		if d := r.guard.Allow(budget.Usage{Steps: 1}); !d.OK {
			// # ★★ 预算用完 —— **不返回 error**（设计文档 §5 的硬要求）
			//
			//	> 触顶的行为**必须是"交付部分 + 说明"**，而不是 `return err`。
			//	> 而"预算超限时直接报错丢失可用结果"是教学计划里**点名的"可能问题"**。
			//
			// ⚠️ 而我在 R43 写这一处时，写的正是那个被点名的写法（`return …, err`）——
			//	**设计文档预见了它，而实现时我没去对照那一节。**
			//
			// # 为什么"用完预算"不是错误
			//
			//	**那个上限是我们自己设的。** 它被触发，说明系统**按设计工作** ——
			//	而把它报告成一个 error，就是**把自己的决定报告成"出事了"**。
			//
			//	> ### 一个我们自己设的上限被触发，不是一个"异常"。
			//
			// # 而它的代价是**具体的**
			//
			//	调用方看到 `err != nil`，会把它当失败 —— 于是**已经做好的产物被丢掉**。
			//	而此刻 `st` 里**装着所有已完成的产物**，它们只是**不完整**。
			//
			//	⭐ 与 L2-9 那条"**触顶但已提交要保留成果**"是**同一条** ——
			//	  只是那次在角色层，这次在图层。
			//
			// ⚠️ 落盘仍然要做：**"产物都在磁盘上"这句话必须是真的**，
			//	否则"可以直接 Run 恢复"就只是一句安慰。
			if perr := r.persist(st); perr != nil {
				// 落盘失败**是**错误 —— 那与"预算用完"完全不同。
				return Result{State: st, Status: StatusBudgetStopped, StopReason: d.Notice, Ran: ran}, perr
			}
			return Result{State: st, Status: StatusBudgetStopped, StopReason: d.Notice, Ran: ran}, nil
		}

		// ★ 计时在这一层 —— 因为**只有执行它的人知道它花了多久**（R43）。
		//
		//	守卫不会去看时钟：它只会做加法。所以"这一步执行了多久"是**报**进去的，
		//	与 token / 成本完全同构。
		startedAt := time.Now()
		out, err := r.step(ctx, st, name, answer)

		// ★ 记账 —— **不管成功还是失败都记**。
		//
		// # ⚠️ 这与 `budget` 包开篇那句"成功才计入"**不矛盾** —— 因为问的不是同一个问题
		//
		//	限流问："**你用了多少配额？**" → 一次失败的请求不该占配额 ✅
		//	预算问："**你花了多少钱？**" → 一次失败的调用**同样要付钱**
		//
		//	而后者有实测支持（L2-9）：同一个角色连续失败 10 次、
		//	烧掉 55321 tokens，**最后仍然失败** —— 那次的钱一分没少花。
		//
		//	> ### 同一句"成功才计入"，在两个问题下答案不同。
		//	> 抄一句话之前，先问它**在回答哪个问题**。
		//
		// ⚠️ 而"事前不扣"这条**仍然要守**：`Allow` 一个字都不记（见它的测试）。
		r.guard.ReportUse(budget.Usage{Steps: 1, Elapsed: time.Since(startedAt)})

		if err != nil {
			return Result{State: st, Status: StatusAborted, Ran: ran}, err
		}
		ran = append(ran, name)

		var interrupted *Interrupt
		if out != nil && out.Interrupt != nil {
			in := out.Interrupt.Clone()
			// ★ 有效期在这里补上 —— **节点不操心它**（与 ID 由协议派生同一个位置）。
			//
			//	在此之前没有任何生产代码写过 `ExpiresAt`，
			//	于是 `CheckResumable` 里那条过期检查**永远不触发**（L3-2）。
			//	修法不是在节点里加一行 —— 那样每个节点都要记得加；
			//	而是**唯一的生产点**在这里。
			in.ExpiresAt = r.interruptExpiry(time.Now())
			st.Pending = &in
			interrupted = &in
		}

		// ★ 每跑完一个节点就落盘 —— 这才是"进程被杀也不丢"的全部内容。
		if err := r.persist(st); err != nil {
			return Result{State: st, Status: StatusAborted, Ran: ran}, err
		}

		if interrupted != nil {
			r.emit(name, observ.KindInterrupt, interrupted.Prompt)
			return Result{State: st, Status: StatusInterrupted, Interrupted: interrupted, Ran: ran}, nil
		}
		if out != nil && strings.TrimSpace(out.Next) != "" {
			next = strings.TrimSpace(out.Next)
		}
	}
}

// callNode 执行节点，并把**节点里的 panic 翻译成一个 error**。
//
// # ⚠️ 这一处是"照抄参考实现"照出来的（R42 差异审计）
//
//	企业仓库在每一个**边界**上都这么做：
//
//		`internal/grqueue/gr.go`        协程边界（goroutine 里 panic 会杀掉整个进程）
//		`internal/middleware/recovery.go`  HTTP 请求边界
//		`internal/api/api_ai_gateway/`    外部调用边界
//
//	而它们的共同点是三样东西：
//
//		· `crash: true` 是一个**显式的结构化字段**（不用去 grep "panic" 这个词）
//		· 带 **stack**（否则只剩一句"出错了"，而 panic 的全部价值就是那个栈）
//		· **不静默**（Error 级别，且带上下文）
//
//	对比之下我们**一处都没有**（全仓 `recover()` = 0）。而"边界"在哪？
//	答案很干脆：**节点调用**就是我们的边界 —— 工具、模型、schema 断言
//	全都跑在节点**里面**，所以包住这一处就包住了它们全部。
//
// # 为什么"崩掉"在我们的语境下更要紧
//
//	L3 的核心资产恰恰是 checkpoint —— 它保证"进程死了能接着跑"。
//	可一个节点的 panic 会让用户看到**一屏栈**，而不是
//	"第 3 步失败了（这是个 bug），产物都在，可以重试"。
//
//	**能力在，但没人把它翻译成人话。**
//
// 转成 error 之后三件事同时成立：
//
//	① checkpoint 仍然自洽 —— panic 发生在节点内，
//	   而状态写入**全部**由 Runner 控制（节点只能读黑板，产出走 `Outcome`）；
//	② 用户拿到一个**可诊断的失败**（不是崩溃）；
//	③ **栈被保留在错误里** —— 那正是排查需要的东西。
//
// ⚠️ 它**不重试、不掩盖**：panic 是代码 bug，不是外部故障。
//
//	所以措辞里写死"这是一个 bug"，免得下一个人把它当成"偶发故障"去重试。
func callNode(ctx context.Context, n Node, in NodeInput) (out *Outcome, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("节点 %s 发生 panic（这是一个 bug，不是外部故障；"+
				"产物与状态未受影响，可以直接重跑）: %v\n%s",
				n.Name(), p, debug.Stack())
		}
	}()
	return n.Run(ctx, in)
}

// step 跑一个节点：装配输入 → 执行 → 校验返回值 → 入库。
func (r *Runner) step(ctx context.Context, st *State, name string, answer *pendingAnswer) (*Outcome, error) {
	node, ok := r.graph.Node(name)
	if !ok {
		return nil, fmt.Errorf("图里没有节点 %q（这是 Runner 的 bug）", name)
	}
	r.emit(name, observ.KindStart, node.Description())

	// ① 还原一块黑板。节点**只能读**它（`NodeInput.board` 未导出）——
	//    产出走 `Outcome.Product`，入库与血缘由这里统一做。
	board, err := st.RestoreBoard()
	if err != nil {
		return nil, fmt.Errorf("节点 %s：还原黑板失败: %w", name, err)
	}

	// ② 取上游（**最新版**）。
	//
	//    ⚠️ 这里不需要"缺前置"的分支：`Graph.Next` 的拓扑序保证了
	//    被选中的节点其 `Needs` 一定已就绪（上游没产出时它自己也不可跳过，
	//    而它排在前面）。所以走到这里还缺东西，就是**图或 Runner 的 bug**。
	inputs, err := collectInputs(st, node)
	if err != nil {
		return nil, err
	}

	out, err := callNode(ctx, node, NodeInput{
		Topic:   r.topic,
		Hint:    r.hints[name],
		Inputs:  schema.HandoffsOf(inputs),
		RunID:   r.runID,
		Attempt: st.AttemptFor(node.Kind()),
		Answer:  answer.forNode(name),
		board:   board,
	})

	if err != nil {
		// ⚠️ 错误信息要带上"这是第几次"（来自黑板，不是运行期计数器）——
		//    与"错误里的数字必须来自实际发生的事"同一条。
		return nil, fmt.Errorf("节点 %s 执行失败（第 %d 次）: %w",
			name, st.AttemptFor(node.Kind()), err)
	}

	// ③ 校验它说的话：`Next` 必须是一条存在的边（或重做自己）。
	if err := r.graph.CheckOutcome(name, out); err != nil {
		return nil, err
	}

	// ④ 入库（有产出才入）+ 记血缘。
	if out != nil && out.Product != nil {
		stored, err := putProduct(st, node, board, out.Product, inputs)
		if err != nil {
			return nil, err
		}
		r.emit(name, observ.KindProduct, fmt.Sprintf("%s  %s", schema.RefOf(stored), stored.Summary))
	}
	return out, nil
}

// ─── 恢复计划 ───────────────────────────────────────────────────────

// pendingAnswer 是"人的回答应该交给哪一跳"。
type pendingAnswer struct {
	node string
	d    *Decision
}

// forNode 返回这一跳该拿到的回答（不是它的话返回 nil）。
//
// ⚠️ 它**不是"只送一次"** —— 同一个节点在一次运行里可能被执行多次
// （`Outcome.Next == 自己` 的重试）。而"人给的答案"本来就是这个节点的**输入**，
// 它的输入没变，重试时当然还应该看得见。
func (a *pendingAnswer) forNode(name string) *Decision {
	if a == nil || a.node != name {
		return nil
	}
	return a.d
}

// resumePlan 是"这一次进来要怎么走"。
type resumePlan struct {
	// Force 非空 = **先强制跑这个节点**（revise 用）。
	Force string

	// Stop = 不再往下跑（abort 用）。
	Stop bool

	// Answer 要送给哪一跳。
	Answer *pendingAnswer
}

// planFrom 从**状态**里读出接下来要怎么走。
//
// ⚠️ 它读的是 `State`，不是一个运行期变量 —— 这正是"崩溃后能重放"的原因。
//
// 三种意图的处置，以及**它们为什么是这样**：
//
//	confirm  什么都不用做。中断的那一跳**已经把产物投进黑板了**
//	         （`Outcome.Product` + `Outcome.Interrupt` 的组合），
//	         于是 `CanSkip` 会把它跳过，调度自然往下走。
//	         —— 这条依赖一个不变量：**能被 confirm 的中断，必须已经产出**。
//
//	revise   **强制重做**中断的那一跳，并把"要改什么"（Note）交给它。
//	         只重跑不给答案 = 假动作：它会用同样的输入产出同样的东西。
//
//	abort    停止推进，交付已有部分（**超限 ≠ 失败**：降级交付 + 说明）。
func planFrom(st *State) resumePlan {
	p := st.Pending
	if p == nil || p.Decision == nil {
		return resumePlan{}
	}
	plan := resumePlan{Answer: &pendingAnswer{node: p.Node, d: p.Decision}}

	switch p.Decision.Intent {
	case IntentRevise:
		plan.Force = p.Node
	case IntentAbort:
		plan.Stop = true
	case IntentConfirm:
		// 见上：产物已经在黑板上，跳过它是自动的
	}
	return plan
}

// ─── 读写状态 ───────────────────────────────────────────────────────

func (r *Runner) load() (*State, error) {
	raw, found, err := r.store.Load(r.runID)
	if err != nil {
		return nil, fmt.Errorf("读取 checkpoint 失败: %w", err)
	}
	if !found {
		return NewState(r.runID, r.topic, r.hints) // 第一次：全新的一轮
	}

	st, err := UnmarshalState(raw)
	if err != nil {
		return nil, fmt.Errorf("恢复 checkpoint 失败: %w", err)
	}

	// ⚠️ 输入必须一致。
	//
	//	同一个 run id 配着不同的主题，等于**两件事共用一个身份** ——
	//	黑板上会混着两次不同输入产生的产物，而血缘判据会认为它们是同一条链。
	//
	//	要换主题，正确做法是**在图上改输入**（新增一版 `topic`，让
	//	`StaleReasons` 把下游全部标脏），而不是换一个调用参数。
	if got, err := st.Topic(); err != nil {
		return nil, fmt.Errorf("恢复 checkpoint 失败: %w", err)
	} else if got != r.topic {
		return nil, fmt.Errorf(
			"run %s 记录的主题是 %q，但这次调用传的是 %q —— "+
				"同一个 run id 配不同的输入等于两件事共用一个身份（血缘会串）。"+
				"要换主题，请在图上新增一版 topic，而不是换调用参数",
			r.runID, got, r.topic)
	}

	// ⚠️ Hint 同样是**输入**，同样必须一致（07-L3复盘.md L3-1）。
	//
	//	在此之前：同一个 run 换 Hint 重启 → **静默接收新指令** →
	//	产出的产物血缘上仍写着旧依据 → **血缘判据还认为这条链是新鲜的**。
	//
	// 两者的处置差别，正好说明了"输入存在哪"的两种等级：
	//
	//	Topic  是黑板产物 → 改了它会由 `StaleReasons` 自动让下游过期（**正确重做**）
	//	Hints  只存在状态里 → 这里只能**拒绝**（比静默强，但不是最优）
	//
	// **这一步就是终点** —— 产物化在 ④ 第二批被**否决了**
	// （理由见 `State.Hints` 的注释：语义不对 / 要动输入契约且方向是错的 /
	//  换来的"正确重做"没有用户）。它不是"阶梯的第一级"。
	//
	// # ⚠️ 但这条规则**从 v2 才开始** —— 兼容窗口的第二个含义
	//
	//	v1 的记录**没有 `hints` 字段**：那不是"这一轮没有 hint"，
	//	而是"那时还没有这个概念"。拿 v2 的规则去校验它，会得出
	//	"**输入变了**"这个误导性的结论 —— 而真实原因是"**记录太老**"。
	//	（详见 `StateVersionWithHints`：这是"报错了对象"的第四次。）
	if st.Version >= StateVersionWithHints && !maps.Equal(st.Hints, r.hints) {
		return nil, fmt.Errorf(
			"run %s 记录的 hints 与本次调用不一致（记录 [%s]，本次 [%s]）—— "+
				"hint 是节点的**输入**：静默换掉它，会让产物用着旧的依据却仍然被判为「新鲜」。\n"+
				"要换，请换一个 run id；或者干脆别换",
			r.runID, describeHints(st.Hints), describeHints(r.hints))
	}

	// v1 的快照没法做这个校验 —— 而且**永远**没法做（它不会是 v2）。
	//
	// ⚠️ 这是一个**不可避免的静默**（信息本来就不存在），
	//	与"可以修掉的静默"不同：后者要修，前者**必须被看见**。
	//	所以这里发一条 notice，而不是悄悄放行。
	if st.Version < StateVersionWithHints {
		r.emit("", observ.KindNotice, fmt.Sprintf(
			"这份快照是 v%d（没有 hints 字段）—— **本次不做**输入一致性校验。"+
				"它之前用的是哪套 hint 已经不可知；要一个被校验的 run，请换一个 run id",
			st.Version))
	}
	return st, nil
}

func (r *Runner) persist(st *State) error {
	st.Stamp(time.Now())
	raw, err := st.Marshal()
	if err != nil {
		return fmt.Errorf("序列化状态失败: %w", err)
	}
	if err := r.store.Save(r.runID, raw); err != nil {
		return fmt.Errorf("保存 checkpoint 失败: %w", err)
	}
	return nil
}

// ─── 小工具 ─────────────────────────────────────────────────────────

// collectInputs 取回已就绪的上游产物（**最新版**）。
//
// 返回 `Artifact` 而不是 `Handoff`，因为调用方要做两件不同的事：
// 生成给节点看的清单（ref + summary）与记进血缘的 `Basis`（key + version）。
// 只交 `Handoff` 的话，第二件事就得把字符串**解析**回去 ——
// 那是我们一直避免的（给模型看的是字符串，给代码比的是结构）。
// interruptExpiry 算出这次中断的**业务**过期时刻。
//
//	interruptTTL > 0 → now + TTL
//	interruptTTL < 0 → 零值（`Interrupt.ExpiresAt` 的零值含义恰好是"不过期" ✅）
//
// `== 0` 不会出现 —— `NewRunner` 已经把它归一成默认值了。
func (r *Runner) interruptExpiry(now time.Time) time.Time {
	if r.interruptTTL < 0 {
		return time.Time{} // 显式的"永不过期"
	}
	return now.UTC().Add(r.interruptTTL)
}

// describeHints 把 hints 渲染成一行**稳定**的文本（供错误信息用）。
//
// ⚠️ 必须排序：map 遍历顺序随机，不排序会让"同样的错误两次打印不一样"，
// 于是无法 diff（与 `State.Keys` / `Blackboard.Keys` 同一条规矩）。
//
// 空 map 与 nil 渲染成同一句话 —— 它们在语义上确实是同一件事
// （"这一轮没有任何额外要求"），所以错误信息里**不该区别对待**。
func describeHints(h map[string]string) string {
	if len(h) == 0 {
		return "无"
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, h[k]))
	}
	return strings.Join(parts, "、")
}

func collectInputs(st *State, n Node) ([]schema.Artifact, error) {
	out := make([]schema.Artifact, 0, len(n.Needs()))
	for _, k := range n.Needs() {
		a, ok := st.Latest(string(k))
		if !ok {
			return nil, fmt.Errorf(
				"节点 %s 缺少前置产物 %q —— 图调度不该把它排到这一步（这是 Runner 的 bug）",
				n.Name(), k)
		}
		out = append(out, a)
	}
	return out, nil
}

// putProduct 把节点交回来的产物入黑板，并记下血缘。
//
// 为什么血缘由**这一层**填、不让节点自己填？与 L2 完全一致：
//
//	"我依赖了谁"是**装配**的职责，不是业务内容的职责。
//	让每个节点自己申报，就会有 N 种写法；而且它们根本不知道上游的**版本号**
//	（它们只拿到 `{ref, summary}` 字符串）。
//
// **谁拥有这个信息，谁负责维护它** —— 与"版本号由黑板分配"是同一条。
func putProduct(st *State, node Node, board *state.Blackboard, p schema.Payload, inputs []schema.Artifact) (schema.Artifact, error) {
	// 双向校验：节点声明的 kind 必须与产物自己声明的 kind 一致。
	//
	// 否则黑板会错位：节点叫 "outline"，里面却存了一份 research。
	// 下游按 Needs 取 outline，拿到的是 research —— 类型没报错、字段都在，
	// 但**语义全错**。这是最隐蔽的一类故障（与 `schema.Decode` 的双向校验同一个道理）。
	if got := p.Kind(); got != node.Kind() {
		return schema.Artifact{}, fmt.Errorf(
			"节点 %s 声明的 kind 是 %q，产出的却是 %q —— 黑板会错位",
			node.Name(), node.Kind(), got)
	}

	a, err := schema.NewArtifact(string(node.Kind()), node.Name(), p)
	if err != nil {
		return schema.Artifact{}, fmt.Errorf("节点 %s：打包产物失败: %w", node.Name(), err)
	}
	a.BasedOn = schema.BasesOf(inputs)

	stored, err := board.Put(a)
	if err != nil {
		return schema.Artifact{}, fmt.Errorf("节点 %s：产物入库失败: %w", node.Name(), err)
	}
	st.AdoptBoard(board)
	return stored, nil
}

// skipped 返回"当前已经可以跳过"的节点名（按拓扑序）。
//
// `except` 是要**排除**的节点（`plan.Force`）—— 它即将被强制重做，
// 所以不该出现在"跳过"的清单里（见 `drive` 里的说明）。
func (r *Runner) skipped(st *State, except string) []string {
	var out []string
	for _, name := range r.graph.Order() {
		if name == except {
			continue
		}
		n, _ := r.graph.Node(name)
		if st.CanSkip(string(n.Kind())).Skip {
			out = append(out, name)
		}
	}
	return out
}

// emit 发一条图事件。
//
// ⚠️ 它**不收 `step`**（R44）—— 见本文件顶部那段关于 `graph.Event` 的说明。
//
//	"第几步"是**派生值**（数一数 `KindStart` 出现过几次），由**消费者**算；
//	生产者只负责报"发生了什么"。
//
// ⚠️ 而删掉它之后，"维护步数的机制"也一起消失了：`drive` 里那个 `step++`
//
//	和传进 `step()` 的参数**都只为了给这个字段喂值** —— 字段没了，它们就没了。
//
//	> ### 那就是"不该存"的实证：**一个只为某个字段而存在的变量**。
func (r *Runner) emit(node string, kind observ.Kind, detail string) {
	r.tracer.Trace(observ.Event{
		RunID: r.runID,
		Layer: observ.LayerGraph,
		// Phase 是**数据**（节点名），不是词表 —— 见 `observ.Kind` 的注释。
		// 那几类"说的是整体、不属于任何一跳"的事件（skipped / notice / done）
		// 传空串，而 `kindSpecs` 里它们恰好也不要求 Phase ✅
		Phase:  node,
		Kind:   kind,
		Detail: detail,
	})
}

func noteSuffix(d Decision) string {
	if strings.TrimSpace(d.Note) == "" {
		return ""
	}
	return "（" + d.Note + "）"
}
