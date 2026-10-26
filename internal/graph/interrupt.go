// 本文件定义 L3 的**中断协议** —— 一个问题，和一个回答。
//
// # 为什么它值得独立一个文件
//
// 因为它是整个 L3 里**唯一必须跨进程存活的东西**。
//
//	节点、边、Runner 都在一个进程里活着 —— 它们挂了就挂了，没有人需要记住它们。
//	只有中断要"出进程、等人、再回来"（见 06-L3设计.md §4.1）。
//
// 协议与运行时分开放，将来换运行时、换存储（文件 → Redis）都不用动协议一行（原则五）。
//
// # 它是两个企业仓库隐患的直接靶子
//
//	F1（无协议版本 + 分派逻辑散落四处）→ 本文件的 Version / Kind / 目录表
//	F2（恢复语义单一，只能"确认"）      → 本文件的 Intent 三态
package graph

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ─── 协议版本 ───────────────────────────────────────────────────────
//
// 为什么协议需要**自己的**版本号，而不是"跟代码版本走"？
//
//	因为这条记录是**唯一会被跨越时间去读的东西**：
//	今天写的中断，可能是三天后、由**另一个版本的代码**读回来的。
//	而"代码版本"无法比较 —— `git sha` 的大小没有任何意义，也无法回答
//	"我还能读懂它吗"。所以需要一个**能比较的整数**。
//
// ⚠️ 两个常量，不是一个 —— 这个区分很重要：
//
//	InterruptVersion    我们**现在写出**的版本
//	InterruptMinVersion 我们**还能读懂**的最低版本
//
//	两者中间那段（min ≤ v ≤ version）是**兼容窗口**：老记录仍可恢复，
//	只是可能缺字段。把它们合成一个常量，就等于宣布"每次加字段都让所有
//	在途会话失效" —— 而滚动发布期间，在途会话恰恰是最多的。
const (
	// InterruptVersion 是当前写出的协议版本。
	InterruptVersion = 1

	// InterruptMinVersion 是仍能读懂的最低版本。
	InterruptMinVersion = 1
)

// ─── 中断类型（受控词表）─────────────────────────────────────────────
//
// ⚠️ 这是 **F1 的核心靶子**。企业仓库存的是 `"FollowUpInfo"` ——
// **Go 结构体的字符串名**。那个选择带来四个后果：
//
//	① 协议与**代码结构**耦合：重命名结构体就改变了线上协议；
//	② 加一种类型要改**四处**手写分派（InterruptInfo 字段 / Set switch /
//	   Get switch / controller 的 if-else），而四处之间**没有编译期约束**；
//	③ 无法回答"系统里有哪些中断类型"（没有目录）；
//	④ 旧的、不被支持的类型名 → 运行期 `unknown type` 崩溃，而不是
//	   "这条记录太老，无法恢复"这样一句人话。
//
// 我们的 Kind 是一个**受控词表**：改 Go 类型名不影响它，
// 而"有哪些类型"由下面这张表一次说清。
type InterruptKind string

const (
	// KindConfirmArtifact 请人确认一份产物（L3 的主场景：大纲）。
	KindConfirmArtifact InterruptKind = "confirm_artifact"

	// KindAskUser 通用追问（自由文本回答）。
	KindAskUser InterruptKind = "ask_user"

	// KindChooseOption 从给定选项里挑一个。
	KindChooseOption InterruptKind = "choose_option"
)

// kindSpec 描述一种中断类型。
//
// 注意它的形状与 `schema.LayoutSpec` **完全一致**（枚举 + 描述 + 元信息）。
// 这是同一个模式在本项目的第三次出现：
//
//	① schema/layout.go    —— 版式枚举 + LayoutCatalog()
//	② schema/artifact.go  —— schema.Kind 枚举 + Valid()
//	③ graph/interrupt.go  —— 中断类型枚举 + 本表
//
// 三次都遵循同一条：**枚举 + 一张表 = 单一真相来源**。
// 校验、给人看的清单、错误提示里的候选项 —— 全部从这一张表派生。
// 写成 `switch` 就要"记得回来补 case"，忘了就是校验放行脏值。
type kindSpec struct {
	Kind InterruptKind

	// Description 是给人看的（也是给模型看的）一句话说明。
	Description string

	// Intents 列出**对这种类型有意义**的回答意图。
	//
	// ⚠️ 每个 kind 都必须包含 IntentAbort ——
	// **人永远有权说"不做了"**。这条不变量由测试守住
	//（写进注释的规则会漂移，写成测试的规则不会）。
	Intents []Intent

	// NeedsOptions 表示这种类型必须先给出选项，人才能回答。
	//
	// 	1 个选项的"选择"不是选择，所以要求 >= 2 个。
	NeedsOptions bool

	// RequiresNote 表示**回答必须带内容**（`Decision.Note` 不能为空）。
	//
	// ⚠️ 它的语义不是"我们喜欢看到理由"，而是关于**能不能继续跑**的：
	//
	//	这类中断是在向人要**一个信息**（"你指的是哪一年？"/"选哪一版？"）。
	//	空回答等于**没回答** —— 节点拿不到信息就走不下去，只能再问一遍；
	//	而用户可以一直点"确认" → **跨越多次恢复的无限循环**。
	//
	//	所以它是"防死循环的第一道防线"，不是文风要求。
	//
	// 判据很干净：
	//
	//	· 需要人**提供信息**的类型（问问题、选选项）→ true
	//	· 只需要人**点个头**的类型（确认产物）      → false
	RequiresNote bool
}

// interruptKinds 是**唯一**的中断类型清单。
func interruptKinds() []kindSpec {
	return []kindSpec{
		{
			Kind:        KindConfirmArtifact,
			Description: "请人确认一份产物；可以确认、要求修改、或放弃",
			Intents:     []Intent{IntentConfirm, IntentRevise, IntentAbort},
		},
		{
			Kind:        KindAskUser,
			Description: "向人追问一个自由文本问题（没有选项可挑）",
			Intents:     []Intent{IntentConfirm, IntentAbort},

			// 它问的就是"信息"，空回答等于没回答 → 节点只能再问一遍（死循环）
			RequiresNote: true,
		},
		{
			Kind:         KindChooseOption,
			Description:  "请人从给定选项里挑一个",
			Intents:      []Intent{IntentConfirm, IntentAbort},
			NeedsOptions: true,

			// 同上：不回答"选了哪个"，节点走不下去
			RequiresNote: true,
		},
	}
}

// interruptSpecOf 按类型查表。ok=false 表示这个类型不在目录里。
//
// ⚠️ 校验一律**查表**，不写 `switch` —— 这是 `Layout.Valid()` 的同一条规矩。
func interruptSpecOf(k InterruptKind) (kindSpec, bool) {
	for _, s := range interruptKinds() {
		if s.Kind == k {
			return s, true
		}
	}
	return kindSpec{}, false
}

// Valid 报告这个类型是否在目录里。
func (k InterruptKind) Valid() bool {
	_, ok := interruptSpecOf(k)
	return ok
}

// InterruptKindNames 返回全部合法类型名（**副本**，调用方改不动目录）。
//
// 存在的意义：**拒绝必须可自愈**（L1-5）。
// 只回一句"类型不合法"，模型只能瞎猜；给出清单，它就能自己改对。
func InterruptKindNames() []string {
	specs := interruptKinds()
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, string(s.Kind))
	}
	return out
}

// JoinInterruptKinds 把合法类型拼成一行，供错误信息使用。
func JoinInterruptKinds(sep string) string {
	return strings.Join(InterruptKindNames(), sep)
}

// ─── 回答的意图（F2 的靶子）─────────────────────────────────────────
//
// 企业仓库的恢复语义是**单一**的：`UserAnswer string`。
//
//	它的注释写着 `ConfirmOutlinesInfo：自由文本场景` —— 也就是说
//	用户说"删掉第 3 页"时，这个字符串会被当成**确认**。
//	（01 索引的故障场景 S7 就是这一步。）
//
// 我们把"回答"提升成一个**有意图的结构**：三种意图，各自的后继动作完全不同。
type Intent string

const (
	// IntentConfirm 就这样，继续往下走。
	IntentConfirm Intent = "confirm"

	// IntentRevise 不对，需要重做（Note 说明改什么）。
	//
	// ⚠️ 它是 L3 与 L2 之间的一条**真实接线**：
	// 收到 revise 之后，代码把上游产物重做一版，`StaleReasons` 就会
	// 自动算出"哪些下游该跟着重做" —— 这就是 L2-1 血缘的第一次真实用途。
	// 在此之前，它只能靠 `L2_REDO` 故障注入来演示。
	IntentRevise Intent = "revise"

	// IntentAbort 不做了，带着已完成的部分收尾。
	//
	// **必须支持它**：一个只能"继续"的 HITL 不是 HITL，
	// 而是一个必须无条件服从的弹窗。
	IntentAbort Intent = "abort"
)

// intentCatalog 是**唯一**的意图清单（与 interruptKinds 同一个模式）。
func intentCatalog() []Intent {
	return []Intent{IntentConfirm, IntentRevise, IntentAbort}
}

// Valid 报告意图是否合法。
func (i Intent) Valid() bool {
	for _, c := range intentCatalog() {
		if i == c {
			return true
		}
	}
	return false
}

// IntentNames 返回全部合法意图名（**副本**）。
func IntentNames() []string {
	cs := intentCatalog()
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, string(c))
	}
	return out
}

// ─── 中断 ──────────────────────────────────────────────────────────

// Interrupt 是一次"机器停下来等人"的现场记录。
//
// # 为什么 Payload 是 json.RawMessage 而不是 any
//
// 因为 `any` 一旦要序列化，就必须**记住每个 key 对应哪个具体类型** ——
// 那就必须写类型注册表或 switch 分派，**正是企业仓库那团并集结构要解决的问题**。
//
// 而 store 只需要存字节、读回来同样是字节 —— **分派的责任被推给了
// 真正需要解它的那一方**（判据见 06-L3设计.md §3.3：谁来解它）。
// 于是加一种中断类型**不用动 store 一行**。
type Interrupt struct {
	// Version 是**协议版本**（不是业务版本）。
	Version int `json:"version"`

	// ID 唯一标识这次中断。恢复时靠它把答案对回来。
	//
	// ⚠️ ID 由**确定性**派生（见 NewInterrupt），不是随机 UUID。
	// 理由：中断会被写进 checkpoint 再读回来。若 ID 随机，
	// "按同样的状态重跑一次"会生成**不同的 ID**，之前存下的答案就对不上了。
	// **确定性 = 可重放**（知识点 ④）。
	ID string `json:"id"`

	RunID string `json:"run_id"`

	// Node 是在哪个节点中断的。
	//
	// 它的用途不是"显示"，而是**恢复时从这里重进** —— 一句话概括 L3 的恢复：
	// 带着答案，重新进入中断发生的那一跳。
	Node string `json:"node"`

	// Attempt 是该节点第几次执行（从 1 开始）。
	//
	// 为什么需要它：**同一个节点可以中断多次**（用户 revise 之后重跑、
	// 又觉得不对）。如果没有它，前后两次中断会算出同一个 ID，
	// 于是"上次的答案"会被套用到"这次的问题"上 —— 一个极难查的串台。
	Attempt int `json:"attempt"`

	Kind InterruptKind `json:"kind"`

	// Prompt 是**要问人的问题本身**。
	//
	// 它是必填的：没有问题的中断，人无法回答，
	// 而系统会一直停在那里等一个不可能到来的答案。
	Prompt string `json:"prompt"`

	// Options 只在 Kind 要求选项时使用（见 kindSpec.NeedsOptions）。
	Options []string `json:"options,omitempty"`

	// Payload 是业务自己带的上下文（例如"待确认的那份大纲"）。
	// store 不解释它。
	Payload json.RawMessage `json:"payload,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	// ExpiresAt 是**业务上的**过期时刻；零值表示不过期。
	//
	// ⚠️ 它与"存储层的 TTL"**不是一回事**，这个区分是本轮的一个结论：
	//
	//	ExpiresAt    业务语义：超过它就别恢复了（"这个中断已经没意义了"）
	//	存储层 TTL   垃圾回收：这条记录可以删了
	//
	// 企业仓库用 Redis 的 `7 * 24h` TTL 同时承担了两者，后果是
	// **过期是静默的** —— `Get` 返回"不存在"，调用方无法区分
	// "key 写错了" / "被 GC 了" / "业务上过期了"（后两者的处置完全不同）。
	//
	// 所以：**业务过期写进数据，存储 TTL 只负责回收，且必须远大于它。**
	ExpiresAt time.Time `json:"expires_at,omitempty"`

	// Decision 非 nil 表示"人已经回答了，但这次回答**还没被消费掉**"。
	//
	// ⚠️ 这是一个关键的设计决定：**人的回答也是状态的一部分，不是运行期变量。**
	//
	// 一开始我把"恢复后要怎么做"算成一个运行期的 `plan` 变量（Force/Stop），
	// 然后发现它有一个**静默不一致**的窗口：
	//
	//	Resume → 算出 plan（"要重做 outline"）→ 清掉 Pending → drive 跑到一半崩了
	//	  ↓
	//	重启后：Pending 没了，outline 也还没重做
	//	  ↓
	//	系统认为"这一跳做完了"（它确实有产物）→ **用户的修改要求凭空消失**
	//
	// 把它写进状态之后，性质完全变了：
	//
	//	· Resume 先把它**落盘**，再开始跑；
	//	· 中途崩了 → 磁盘上还有"已回答未消费" → 下次 `Run` **自动重放**它；
	//	· 跑完（消费掉了）→ 连同 Pending 一起清掉并落盘。
	//
	// 一句话：**"重放"不需要任何额外机制 —— 它只是"再读一次状态"。**
	Decision *Decision `json:"decision,omitempty"`
}

// NewInterrupt 构造一次中断，并派生一个**确定性**的 ID。
//
//	ID = "<runID>:<node>:<attempt>"
//
// 确定性带来的直接好处：**同一份状态重跑，得到同一条中断记录。**
// 这正是"可重放"（知识点 ④）在协议层的落地 —— 而随机 ID 会让它失效。
//
// 同时，`attempt` 保证了"同一个节点中断两次"得到**两个不同的 ID**。
func NewInterrupt(runID, node string, attempt int, kind InterruptKind, prompt string) Interrupt {
	runID = strings.TrimSpace(runID)
	node = strings.TrimSpace(node)
	if attempt < 1 {
		attempt = 1
	}
	return Interrupt{
		Version:   InterruptVersion,
		ID:        fmt.Sprintf("%s:%s:%d", runID, node, attempt),
		RunID:     runID,
		Node:      node,
		Attempt:   attempt,
		Kind:      kind,
		Prompt:    strings.TrimSpace(prompt),
		CreatedAt: time.Now().UTC(),
	}
}

// Validate 校验一条中断记录的**结构**是否自洽。
//
// 校验的责任是"**不管谁构造的都要挡**"（与 `schema.Artifact.Validate` 同一条）：
// 手工构造的、从 JSON 解出来的、从磁盘读回来的，都可能带脏值。
//
// ⚠️ 它**不检查版本范围**（"太老"/"太新"）。那是 `CheckResumable` 的头一步，
// 原因见那里的注释 —— 一句话：**版本不对的记录，结构本身就不该被本版本的规则评判**。
// 这里只挡 `Version <= 0`，因为那不是"版本不对"，而是"忘了填"。
func (in Interrupt) Validate() error {
	switch {
	case in.Version <= 0:
		return fmt.Errorf("中断 %q 的 protocol version=%d 不合法", in.ID, in.Version)
	case strings.TrimSpace(in.ID) == "":
		return fmt.Errorf("中断缺少 id")
	case strings.TrimSpace(in.RunID) == "":
		return fmt.Errorf("中断 %s 缺少 run_id", in.ID)
	case strings.TrimSpace(in.Node) == "":
		return fmt.Errorf("中断 %s 缺少 node：恢复时不知道该重进哪一跳", in.ID)
	case in.Attempt < 1:
		return fmt.Errorf("中断 %s 的 attempt=%d 不合法（应从 1 开始）", in.ID, in.Attempt)
	case !in.Kind.Valid():
		// 可自愈：给出合法清单，而不是只回一句"不合法"（L1-5）。
		return fmt.Errorf("中断 %s 的类型 %q 不合法。合法取值：%s",
			in.ID, in.Kind, JoinInterruptKinds(" / "))
	case strings.TrimSpace(in.Prompt) == "":
		return fmt.Errorf("中断 %s 没有 prompt：没有要问的问题，人会一直等下去", in.ID)
	case in.CreatedAt.IsZero():
		return fmt.Errorf("中断 %s 缺少 created_at", in.ID)
	}

	// 已经写进来的回答也必须过校验 ——
	// 它可能是从磁盘读回来的，也可能是被手工改过的（"不管谁构造的都要挡"）。
	if in.Decision != nil {
		if err := in.Decision.Validate(in); err != nil {
			return fmt.Errorf("中断 %s 上挂着的回答不合法: %w", in.ID, err)
		}
	}

	spec, _ := interruptSpecOf(in.Kind)
	switch {
	case spec.NeedsOptions && len(in.Options) < 2:
		// 1 个选项的"选择"不是选择，0 个更是。
		return fmt.Errorf("中断 %s 的类型 %q 需要至少 2 个选项，实际给了 %d 个",
			in.ID, in.Kind, len(in.Options))
	case !spec.NeedsOptions && len(in.Options) > 0:
		// 反向也要挡：不带选项的类型夹带了 options，
		// 下游会以为"人应该从里面挑"，而 Prompt 里根本没让人挑。
		// 两个字段描述同一件事却互相矛盾 —— 必须在入口拒绝。
		return fmt.Errorf("中断 %s 的类型 %q 不接受 options（实际给了 %d 个）",
			in.ID, in.Kind, len(in.Options))
	}
	for i, o := range in.Options {
		if strings.TrimSpace(o) == "" {
			return fmt.Errorf("中断 %s 的 options[%d] 为空", in.ID, i)
		}
	}
	return nil
}

// ─── 人的回答 ───────────────────────────────────────────────────────

// Decision 是人（或代人的系统）对一次中断的回答。
//
// 它**不是**一个字符串。企业仓库的 F2 就出在这里：
// `UserAnswer string` 无法表达"我不是在确认，我是在要求改"。
type Decision struct {
	Intent Intent `json:"intent"`

	// Note 是意图的补充说明。
	//
	// ⚠️ 对 revise 它是**必填**的，对 confirm / abort 不是 ——
	// 这个差别不是随意的，见 Validate 里的说明。
	Note string `json:"note"`
}

// Validate 校验回答是否合法，并且**对这个中断类型**是否合法。
//
// # 为什么 revise 必须有 Note，abort 不需要
//
// 判据：**只拦"不满足就不能继续执行"的情况。**
//
//	revise 没有 Note → 代码不知道该重做什么、重做到什么程度
//	                   → **不可执行**，必须拒绝
//	abort  没有 Note → 放弃不需要理由也能放弃
//	                   → 信息少一点而已，**不该卡住人**
//
// 校验太严的代价常常被低估：它会把"能用"的场景挡在门外，
// 而用户唯一能做的就是换个说法猜我们的规则。
// **校验的目标是让系统能继续，不是让人遵守我们的表格。**
func (d Decision) Validate(in Interrupt) error {
	if !d.Intent.Valid() {
		// 可自愈：给合法清单。
		return fmt.Errorf("回答意图 %q 不合法。合法取值：%s",
			d.Intent, strings.Join(IntentNames(), " / "))
	}

	spec, ok := interruptSpecOf(in.Kind)
	if !ok {
		// 理论上走不到这里（Interrupt.Validate 已经挡过），但不能静默通过：
		// 未知类型意味着**我们不知道哪些意图对它有意义**。
		return fmt.Errorf("中断 %s 的类型 %q 不在目录里，无法判断回答是否合法", in.ID, in.Kind)
	}
	allowed := false
	for _, a := range spec.Intents {
		if a == d.Intent {
			allowed = true
			break
		}
	}
	if !allowed {
		names := make([]string, 0, len(spec.Intents))
		for _, a := range spec.Intents {
			names = append(names, string(a))
		}
		return fmt.Errorf("中断 %s（类型 %q）不接受 %q 这种回答。它可以接受：%s",
			in.ID, in.Kind, d.Intent, strings.Join(names, " / "))
	}

	if d.Intent == IntentRevise && strings.TrimSpace(d.Note) == "" {
		return fmt.Errorf("回答 %q 必须说明要改什么（note 为空）：代码不知道该怎么重做", d.Intent)
	}

	// 这一类中断问的就是"一个信息"，空回答等于没回答 ——
	// 节点拿不到它只能再问一遍，而人可以一直点"确认"（**跨恢复的死循环**）。
	if spec.RequiresNote && strings.TrimSpace(d.Note) == "" {
		return fmt.Errorf(
			"中断 %s（类型 %q）是在向你要一个信息，回答不能为空 —— "+
				"空回答等于没回答，节点拿不到它只会再问一遍", in.ID, in.Kind)
	}
	return nil
}

// ─── 恢复失败（分类明确，这是企业仓库缺的一环）───────────────────────

// ResumeErrorKind 是"恢复为什么失败"的分类。
//
// ⚠️ 为什么要分这么细？因为**处置完全不同** —— 判据是"**谁的问题**"：
//
//	谁的问题         kind            处置
//	─────────────  ──────────────  ──────────────────────────────
//	我们（磁盘/代码）  corrupt         重新开始 + **必须告警**（这是 bug 或硬件问题）
//	我们（协议演进）   too_old         重新开始 + 运维关注（不该频繁发生）
//	我们（部署）      from_future     **重试到新实例**（滚动发布期间正常）
//	时间（业务过期）   expired         重新开始（但要告诉人"是过期"，不是"没找到"）
//	对方（引用错了）   not_found       重新开始（可能是重复提交，无害）
//	对方（答案不对）   bad_decision    **让人改答案，重试**
//
// 企业仓库把这些合并成一句 `"interrupt_id %q 不存在或已过期"`。
// 对用户足够，对排查**不够** —— 它把"用户的错"和"我们的错"混成了一句话，
// 而这两者的**告警级别**完全不同。
type ResumeErrorKind string

const (
	ResumeNotFound    ResumeErrorKind = "not_found"
	ResumeExpired     ResumeErrorKind = "expired"
	ResumeCorrupt     ResumeErrorKind = "corrupt"
	ResumeTooOld      ResumeErrorKind = "too_old"
	ResumeFromFuture  ResumeErrorKind = "from_future"
	ResumeBadDecision ResumeErrorKind = "bad_decision"
)

// ResumeError 是恢复失败的结构化原因。
//
// 做成结构而不是 `fmt.Errorf` 的一句话，是为了让**调用方能按类型分支**
// （HTTP 层要决定 400 还是 409 还是 503，CLI 层要决定"重试还是重新开始"）——
// 靠匹配错误文案来分支，是 A4（文本嗅探）那一类隐患的开始。
type ResumeError struct {
	// Subject 是"这条记录**是什么**"（"中断" / "快照"）。
	//
	// ⚠️ 它存在的理由很具体：这个类型被**两处**用
	//（`Interrupt.CheckResumable` 与 `State.CheckRestorable`），
	//	而第一版把"中断"写死在 `Error()` 里 ——
	//	于是**快照读不出来时，错误信息说的是"无法恢复中断"**。
	//	一句把排查引向错地方的话，正是"报错了对象"的生成机制。
	//
	//	现在由**调用方**补上（`withSubject`）：内部只负责描述"哪里不对"，
	//	"这是什么记录"是调用方才知道的上下文。
	Subject string

	Kind   ResumeErrorKind
	ID     string
	Detail string
}

func (e *ResumeError) Error() string {
	subject := e.Subject
	if subject == "" {
		subject = "记录"
	}
	// id 为空时不要打出一对空引号（`快照 ""` 看着像出了别的问题）
	what := subject
	if e.ID != "" {
		what = fmt.Sprintf("%s %q", subject, e.ID)
	}

	if e.Detail == "" {
		return fmt.Sprintf("无法恢复%s：%s", what, e.Kind)
	}
	return fmt.Sprintf("无法恢复%s：%s（%s）", what, e.Kind, e.Detail)
}

// withSubject 给一个 `*ResumeError` 补上"这是什么记录"与它的 id。
//
// 它让**上下文只在一个地方补一次** —— 内部的判断函数不必各自重复
// "我是谁的中断/快照"（那种重复一定会有一处漏掉）。
func withSubject(err error, subject, id string) error {
	if re, ok := err.(*ResumeError); ok {
		re.Subject, re.ID = subject, id
	}
	return err
}

// Retryable 报告这个失败"再试一次有没有可能成功"。
//
// ⚠️ 这不是给用户看的文案分类，而是**给调用方用的行为分类**：
//
//	true  → 可以重试，或者让人改一下答案再试
//	false → 唯一出路是重新开始；重试只是浪费时间，还会刷日志
//
// 这个区分是有实际后果的：把 `from_future`（滚动发布期间常见）当成不可重试，
// 会让用户看到一个"请重新开始"，而他其实只需要再点一次。
func (k ResumeErrorKind) Retryable() bool {
	switch k {
	case ResumeFromFuture, ResumeBadDecision:
		return true
	default:
		return false
	}
}

// checkProtocolVersion 判断一份记录的版本在我们这里还能不能读。
//
// ⚠️ `min` 与 `current` 都是**参数**，而不是直接读常量 ——
// 与 `schema.Latest` 同一个理由（原则五）：让规则能被单测，
// 而不必先把生产常量改坏才能测到那条分支。
//
// 两个调用方（中断协议 / 快照格式）各自有自己的 min 与 current，
// 但它们**要求的行为完全一样** —— 所以规则只有这一份。
//
// # 三种"读不懂"，各有各的原因，**不能合并**
//
//	v <= 0       版本字段**根本没被填写**  → corrupt
//	v > current  来自更新的版本           → from_future（重试到新实例）
//	v < min      协议太老                → too_old（重新开始）
//
// ⚠️ 第一行归为 `corrupt` 而不是 `too_old`，这个区分是**写测试时才发现**的：
//
//	一份 `Version=0` 的快照，真实原因是"它根本没被正确写入"（或读错了字节）。
//	报 `too_old` 会把排查引向"协议是不是演进了"—— 一条完全错误的路。
//
//	**这和"版本检查要跑在结构校验之前"是同一类错误：
//	把 A 的问题报成 B 的问题。** 而误导性的错误比没有错误更难查。
//
// # 一个副产品：兼容窗口的含义比想象中多一层
//
// `min ≤ v ≤ current` 不只是"**能**读"，还意味着"**结构兼容**"。
// 所以只有落在窗口内的记录，才适用本版本的结构规则去校验 ——
// 窗口之外的记录结构可能不同，校验它只会得到误导性的结论（见 CheckResumable）。
//
// 当前 `min == current == 1`，所以 TooOld 分支**在生产路径上暂时不可达**。
// 但它不是死代码：`current` 涨到 2 的那一刻，它立刻成为唯一保护"在途会话"的路径。
// **为一条必经之路留好位置，比等它来了再改结构便宜。**
func checkProtocolVersion(v, min, current int) error {
	switch {
	case v <= 0:
		return &ResumeError{
			Kind:   ResumeCorrupt,
			Detail: fmt.Sprintf("版本字段是 %d —— 它没有被正确写入", v),
		}
	case v > current:
		// 滚动发布期间一定会遇到：新实例写、旧实例读。
		// 不能当成"损坏" —— 它是**完好的、只是本进程读不懂**。
		return &ResumeError{
			Kind: ResumeFromFuture,
			Detail: fmt.Sprintf(
				"记录版本 %d，本进程支持到 %d —— 请把请求路由到已升级的实例", v, current),
		}
	case v < min:
		return &ResumeError{
			Kind: ResumeTooOld,
			Detail: fmt.Sprintf(
				"记录版本 %d，最低可读版本 %d —— 请放弃这次会话，重新开始", v, min),
		}
	}
	return nil
}

// Clone 深拷贝一份中断。
//
// 与黑板的"进出都是深拷贝"是同一条规矩（原则四的一致性）：
// 持有者拿到一份副本之后随手改，不该影响别人手上那份。
//
// 这里非拷不可的字段是 `Options`（切片）、`Payload`（[]byte）与 `Decision`（指针）——
// 三者都是引用类型。其余字段是值类型，赋值即拷贝。
func (in Interrupt) Clone() Interrupt {
	in.Options = append([]string(nil), in.Options...)
	in.Payload = append([]byte(nil), in.Payload...)
	if in.Decision != nil {
		d := *in.Decision
		in.Decision = &d
	}
	return in
}

// CheckResumable 判断"这条中断现在还能不能被恢复"。
//
// 三步，**顺序不能换**：
//
//	① 版本 —— 我读得懂它吗？
//	② 结构 —— 它自己写得对吗？
//	③ 过期 —— 它在业务上还有意义吗？
//
// ⚠️ 为什么版本必须在结构**之前**？
//
//	因为**窗口之外的记录结构可能不同**：
//	来自未来的 v2 可能把 `prompt` 改名成了 `question`；
//	太老的 v0（在别的版本里）可能根本没有这个字段。
//	用本版本的规则去校验它们，会得出"缺少 prompt"这种结论 ——
//	**而真实原因是"字段改名了 / 根本没有这个字段"，不是"它写错了"。**
//
//	先问"我读得懂它吗"，再问"它写得对吗"。
//	反过来会把错误报到别的地方去，而**误导性的错误比没有错误更难查**。
//
// 它**不检查答案**（那是 `Decision.Validate` 的事）：
// 先确认"这条路存在且可走"，再判断"你要往哪走"。
// 反过来的话，一条过期的中断会先报"你的答案不合法" —— 同样是误导。
func (in Interrupt) CheckResumable(now time.Time) error {
	// 上下文（"这是什么记录"、"它的 id 是什么"）**只在这一处补**。
	return withSubject(in.checkResumable(now), "中断", in.ID)
}

// checkResumable 是真正干活的：它只描述"哪里不对"，不关心自己是谁。
func (in Interrupt) checkResumable(now time.Time) error {
	// ① 版本
	if err := checkProtocolVersion(in.Version, InterruptMinVersion, InterruptVersion); err != nil {
		return err
	}

	// ② 结构
	if err := in.Validate(); err != nil {
		// ⚠️ 是 `corrupt` 而不是 `bad_decision`：
		// 记录**自己**写坏了，与"人给的答案不对"是两件事。
		// 混在一起会让一个磁盘故障看起来像一次用户误操作 ——
		// 于是没有人去查磁盘。
		return &ResumeError{Kind: ResumeCorrupt, Detail: err.Error()}
	}

	// ③ 业务过期
	if !in.ExpiresAt.IsZero() && now.UTC().After(in.ExpiresAt) {
		return &ResumeError{
			Kind:   ResumeExpired,
			Detail: fmt.Sprintf("已于 %s 过期", in.ExpiresAt.Format(time.RFC3339)),
		}
	}
	return nil
}
