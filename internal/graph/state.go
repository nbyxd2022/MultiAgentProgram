// 本文件定义**可持久化的图状态** —— 一份能跨进程、跨时间存活的快照。
//
// 它和 `interrupt.go` 一起构成"出进程的那部分"：
//
//	state.go    —— 全部状态（黑板快照 + 一次待回答的中断）
//	interrupt.go —— 中断协议（state 里那个 Pending 字段的类型）
//
// 依赖方向：`interrupt.go` → `state.go`（所以它先写）。
package graph

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"multiagent/internal/schema"
	"multiagent/internal/state"
)

// ─── 快照格式版本 ───────────────────────────────────────────────────
//
// ⚠️ 它与 `InterruptVersion` 是**两个**版本号，不是同一个。理由：
//
//	它们的**演进节奏不同**。中断协议是"机器问人一个问题"的形状，
//	几十次改动里可能只动一次；而快照会随着"图里多了什么"频繁变化。
//	绑成一个，任何一边加字段都会让另一边也跟着失效。
//
// 但**判断规则只有一份** —— `checkProtocolVersion`（见 interrupt.go）。
const (
	// StateVersion 是当前写出的快照格式版本。
	//
	//	v1 → v2：新增 `hints`（**输入**必须进状态，否则换了它没人知道 —— L3-1）
	StateVersion = 2

	// StateMinVersion 是仍能读懂的最低版本。
	//
	// ⚠️ 它**故意留在 1** —— 这是"兼容窗口"的一次真实使用：
	//
	//	v1 的记录**没有 `hints` 字段**。那不是"这一轮没有 hint"，
	//	而是"**那时还没有这个概念**"。
	StateMinVersion = 1

	// StateVersionWithHints 是"hints 校验"这条规则的**起始版本**。
	//
	// # ⚠️ 它存在的原因是"报错了对象"（**第四次**）
	//
	//	加上 `hints` 并比对之后，磁盘上那份 **v1 时代**的 `l3-demo.json`
	//	立刻被拒了，理由是：
	//
	//	  记录的 hints 与本次调用不一致（记录 [无]，本次 [outline="控制在 3 页"]）
	//
	//	而真实原因是"**这份记录写在 `hints` 存在之前**"，不是"输入变了"。
	//	**把 A 的问题报成 B 的问题，会把排查引向一条完全错误的路。**
	//
	//	（前三次：R29 来自未来的记录被报成"缺少 prompt"（实为字段改名）；
	//	  R30 `Version=0` 被报成"协议太老"（实为字段没写）；
	//	  R34 快照读不出来被报成"中断的问题"（实为 subject 写死）。）
	//
	// # 修法正是 R30 补完整的那半句话
	//
	//	`min ≤ v ≤ current` 不只是"**能**读"，还意味着"**结构兼容**" ——
	//	所以**只有落在窗口内的记录，才适用本版本的结构规则**。
	//
	//	**兼容窗口的判据是「当时的代码遵守什么规则」，而不是「现在的代码想要什么规则」。**
	//
	// # ⚠️ 为什么**不迁移**（读入时把 v1 补成 v2）
	//
	//	因为那会把"**我们没有的信息**"变成一个"**我们编出来的值**"：
	//
	//		v1 记录的前几个节点，可能用的是**另一套 hint**。给它补上本次的 hints，
	//		血缘判据从此会认为"全部产物都基于这套 hint" —— **那是一个永久的谎**。
	//
	//	放行 + 保持 v1 才是诚实的：**"我们不知道它之前用的是什么，所以不假装知道。"**
	//	代价是一个**不可避免的静默**（不是可避免的那种）—— 所以它必须被**写下来**
	//	（本注释）并被**看见**（`load` 会发一条 `EventNotice`）。
	StateVersionWithHints = 2
)

// KeyTopic 是那条"输入产物"在黑板上使用的 key。
//
// 约定：**黑板上的 key 就是产物 kind 的字符串**（`research` / `outline` / …）。
// 这不是巧合 —— 它让"节点产出了什么"和"黑板上的哪个 key"天然对应，
// 于是图的边可以直接用 kind 表达（见 graph.go 的 BuildEdges）。
const KeyTopic = string(schema.KindTopic)

// ProducedByCode 表示"这份产物不是模型产出的，是代码写的"。
//
// 它出现在信封上是有意义的：排查时一眼就能看出
// **"这一版错了不可能是模型的问题"**。
const ProducedByCode = "code"

// ─── State ─────────────────────────────────────────────────────────

// State 是图的一次完整快照 —— **能跨进程、跨时间存活的那部分**。
//
// # 它比直觉上要小，这是刻意的
//
// 设计文档 §3.2 里我原本写了五个字段（多了 `Node` 游标与 `Done` 已完成集合）。
// 写实现时发现**那两个是冗余的**：
//
//	· **"完成"是产物的属性，不是节点的属性** —— 产物在，节点就是完成了；
//	· **"接下来跑谁"** = "按拓扑序，第一个依赖齐了但产物还没齐的节点"，算得出来。
//
// 于是删掉它们。**留下的每一个字段都必须回答同一个问题：它不可推导吗？**
//
//	字段        为什么不可推导
//	──────────  ────────────────────────────────────────────────
//	Version     元信息
//	RunID       身份（读回来时得知道这是谁的状态）
//	Board       **本体**（详见下面的注释）
//	Pending     **人的决定** —— 机器停下来问了问题、人还没回答，
//	            这件事没有任何别的地方记着
//	Hints       **输入** —— 逐节点的额外要求，黑板上没有它，算不出来
//	            （`Topic` 也曾属于这一类，但它后来变成了黑板产物 `topic@1`）
//	CreatedAt   / UpdatedAt  时间事实
//
// 三个好处：① 状态**不可能自相矛盾**（存不下"游标说在 render 但 design 没做"
// 这种状态）；② 恢复不需要"从游标往后跑"，只需要问"现在缺什么"；
// ③ 体积小，而它每次都要写盘。
//
// # ⚠️ 这里**没有** Topic 字段 —— 而且这是改过一次的
//
// 第一版我写了 `Topic string`，图个方便。然后意识到它和 L2-1 犯的是**同一个错**：
//
//	主题的"正确值"在黑板上的 `topic@1`（它有版本、能被 StaleReasons 比较）。
//	再存一份 `State.Topic`，就等于**同一个信息存在两处** ——
//	用户一旦改主题（`topic@2`），这个字段就开始**说谎**，而且没有任何一层能发现。
//
// 我在 L2 花了一整轮修"新鲜度"，结果自己的新结构里又埋了一个"会变旧"的字段。
// → 删除，改成 `Topic()` 方法**从黑板读**。
type State struct {
	// Version 是**快照格式版本**（不是业务版本）。
	Version int `json:"version"`

	RunID string `json:"run_id"`

	// Board 是黑板的**全量快照**。
	//
	// 为什么是全量、而不是"增量日志"？
	//
	//	黑板本来就是**追加、绝不覆盖**（L2 的第一条设计约束），
	//	所以它的每一版都不可变 —— 全量快照与"截取一个时间点"在语义上完全等价。
	//	增量日志只在体积成为问题时才值得引入，而我们还没到那一步。
	//
	// ⚠️ 它能被 `json.Marshal` 干净地处理，靠的是 L2 的一个刻意选择：
	// `Artifact.Payload` 是 `json.RawMessage`（字节），不是 `any`（接口）。
	// 后者会让序列化必须"记住每个 key 对应哪个具体类型" ——
	// 那正是企业仓库 `InterruptInfo` 那团并集结构要解决的问题。
	Board map[string][]schema.Artifact `json:"board"`

	// Pending 是一次"还没被回答的中断"；nil 表示没有在等人。
	//
	// 它是**唯一不可推导**的那类状态：产物能从黑板算出来，
	// 但"机器停下来问了一个问题、人还没回答"没有别的地方记着。
	Pending *Interrupt `json:"pending,omitempty"`

	// Hints 是逐节点的额外要求（`RunnerOptions.Hints` 的一份记录）。
	//
	// # ⚠️ 它为什么必须进状态，而 `Topic` 却不必
	//
	//	`Topic` 在黑板上有一份产物（`topic@1`）—— **它是可推导的**；
	//	Hint 没有任何别的存储位置，**它就是本体**。
	//
	//	（07-L3复盘.md L3-1：Hint 是节点的**输入**，而在此之前
	//	 "换了 Hint 重启"会发生什么？—— 静默接收新指令，
	//	 产出的产物血缘上仍写着旧依据，于是**血缘判据还认为这条链是新鲜的**。
	//	 这是"输入不进状态就会说谎"的**第三次**：L2-1 → Topic → Hints。）
	//
	// # ⚠️ 但它与 `Topic` 的**安全等级不同** —— 这一点要记住
	//
	//	Topic  靠"**是黑板产物**"保证新鲜：改了主题 → `StaleReasons` 自动让下游过期；
	//	Hints  只靠"`load` 时比对"：改了 Hint → **拒绝启动**。
	//
	//	**拒绝 ≠ 正确。** 它把"静默的错误"变成了"显式的错误"（这已经是大改善），
	//	但用户得到的是"跑不了"，而不是"那一个节点正确地重做"。
	//
	// # ⚠️ 为什么不产物化（④ 第二批评审的结论：**否决**）
	//
	//	"产物化"（`hint:<node>@1`，让它进血缘）看起来是"下一步"，
	//	但评审之后**否决了** —— 三条理由，一条比一条硬：
	//
	//	① **语义不对**：`Hints` 是「**装配期的静态配置**」（`RunnerOptions` 的原话）。
	//	   静态配置**承诺不变** —— 所以"变了"是一个**错误**，不是一次**变更**。
	//	   而产物化恰恰是把它变成一个"**可以变**"的东西，也就是**赋予了它新语义**。
	//
	//	② **要动输入契约，而且方向是错的**：`Needs()` 返回 `[]schema.Kind`，
	//	   而 `Kind` 是**受控词表**（`Kind.Valid()` 用来校验**模型给出**的名字）。
	//	   `hint:outline` 不是合法的 Kind；放宽它等于把"**代码写的依赖**"
	//	   与"**模型说的名字**"混进同一个类型 —— 而这两者的可信程度完全不同。
	//
	//	③ **它换来的东西没有用户**：产物化的收益是"换 hint → 那一个节点**正确地重做**"
	//	   （而不是拒绝）。但真实场景里"人的要求变了"走的是
	//	   `IntentRevise` + `Decision.Note`（见 `SubAgentNode.hintWithAnswer`）——
	//	   **那才是一次"变更"，而它已经有正确的机制了。**
	//
	//	**所以这一级的终点就是「存起来 + 比对」。** 它不是"阶梯的第一级"。
	//
	//	（"想过了不做"要有证据 —— 上面三条，加上下面这个触发条件。）
	//
	//	触发条件（什么时候它**该**变成可变的）：出现一个
	//	「**在运行期改 hint，且要影响别的节点**」的真实需求 ——
	//	比如用户在 HITL 里说"顺便把 research 也限制到 3 条"
	//	（一个不重做当前节点、但要影响**上游**的要求）。
	//
	//	⚠️ 那时要设计的**不是"hint 产物化"，而是「全局要求」这个新概念** ——
	//	   把 hint 硬塞进 `Needs()` 只会让"逐节点的指令"这个概念变形。
	Hints map[string]string `json:"hints,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewState 构造一轮全新的状态，并把**输入也写成一份产物**。
//
// 写 `topic@1` 这一步不是仪式，它是让根节点有上游的关键：
//
//	topic@1 ──▶ research@1 ──▶ outline@1 ──▶ …
//
// 于是"新鲜"这条判据**没有根节点特例**，`StaleReasons` 一路通用 ——
// 而"用户改了主题"这件事**不用写任何特殊逻辑**就会自动让下游全部过期。
//
// 它走的是与普通产物**完全相同**的路径（`schema.NewArtifact` → `Blackboard.Put`），
// 所以版本分配、校验、深拷贝这些规矩一条都不用重写。
//
// # ⚠️ 为什么 `hints` 是一个**参数**（而不是事后再设）
//
//	因为它是**输入** —— 与 `topic` 同性质。把输入留在构造期一次给全，
//	有两层好处：
//
//	· 语义正确：一轮状态的"输入"在它被创造出来的那一刻就定死了；
//	· **强迫每个调用点想一遍"hints 是什么"** —— 在此之前
//	  `cmd/l3` 传了 Hints 却从没想过它会不会被记下来（那正是 L3-1）。
//
//	传 nil 表示"这一轮没有任何额外要求"，与传一个空 map 等价。
func NewState(runID, topic string, hints map[string]string) (*State, error) {
	runID = strings.TrimSpace(runID)
	topic = strings.TrimSpace(topic)

	switch {
	case runID == "":
		return nil, fmt.Errorf("run id 不能为空")
	case topic == "":
		// 没有主题的这一轮没有任何意义：research 不知道搜什么，
		// 而且恢复时会找不到输入产物。
		return nil, fmt.Errorf("主题不能为空")
	}

	b := state.NewBlackboard()
	a, err := schema.NewArtifact(KeyTopic, ProducedByCode, schema.Topic{Text: topic})
	if err != nil {
		return nil, fmt.Errorf("构造输入产物失败: %w", err)
	}
	if _, err := b.Put(a); err != nil {
		return nil, fmt.Errorf("写入输入产物失败: %w", err)
	}

	now := time.Now().UTC()
	return &State{
		Version:   StateVersion,
		RunID:     runID,
		Board:     b.Snapshot(),
		Hints:     maps.Clone(hints),
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// ─── 读写黑板 ───────────────────────────────────────────────────────

// RestoreBoard 把快照还原成一块**可用的黑板**。
//
// ⚠️ 方法名是 `RestoreBoard` 而不是 `Board` —— 因为字段已经叫 `Board` 了，
// Go 不允许同名。这个限制反而逼出一个更好的命名：
//
//	字段 `Board`         描述**它是什么**（一份快照数据）
//	方法 `RestoreBoard`  描述**它做什么**（把它还原成一块黑板）
//
// 与之成对的是 `AdoptBoard`（黑板 → 快照）。**两个方向各有一个名字，
// 谁读谁写一眼可见** —— 比一个含义模糊的 `SetBoard` 清楚得多。
func (s *State) RestoreBoard() (*state.Blackboard, error) {
	b, err := state.LoadSnapshot(s.Board)
	if err != nil {
		return nil, fmt.Errorf("还原黑板失败: %w", err)
	}
	return b, nil
}

// AdoptBoard 把一块黑板**当前的状态**收进快照。
//
// 方向与 RestoreBoard 相反。节点跑完之后，Runner 调它把黑板的新版本带回来。
//
// 它不校验 —— 校验由 `Validate()`（写盘前）统一负责。
// **一件事只有一个地方做**：如果这里也校验一遍，两处就会漂移。
func (s *State) AdoptBoard(b *state.Blackboard) {
	s.Board = b.Snapshot()
}

// Topic 返回这一轮的输入主题。
//
// ⚠️ 它**从黑板读**，而不是从 State 的一个字段读 —— 原因见 State 的注释。
// 这样的代价是"读主题要解一次 payload"，收益是**主题只有一个来源**。
//
// 判据很简单：**多存一份便宜的能不能换来"少一个会说谎的字段"？**
// 换不到就不存。
func (s *State) Topic() (string, error) {
	versions := s.Board[KeyTopic]
	if len(versions) == 0 {
		return "", fmt.Errorf("快照里没有输入产物 %q —— 这一轮没有输入，无法恢复", KeyTopic)
	}
	t, err := schema.Decode[schema.Topic](versions[len(versions)-1])
	if err != nil {
		return "", fmt.Errorf("解析输入产物失败: %w", err)
	}
	return t.Text, nil
}

// AttemptFor 返回某个节点当前的"第几次执行"（从 1 起）。
//
// ⚠️ 它从**黑板推导**，而不是一个运行期计数器 —— 这一点是"确定性中断 ID"
// 能不能真正成立的关键：
//
//	中断 ID = (runID, node, attempt)（见 NewInterrupt）。
//	如果 attempt 是个计数器，那么**恢复时节点重跑一次，计数器 +1**，
//	于是它派生出一个**新的 ID** —— 而"人上次给的答案"挂在旧 ID 上，
//	两者永远对不上，系统会**一遍又一遍重新提问**。
//
//	改成"已产出版本数 + 1"之后，两件事同时成立：
//
//	· 中断时**没有产出** → 版本数不变 → attempt 不变 → **ID 不变** → 答案对得上；
//	· 真正重做过一次（版本 +1）之后 → attempt 递增 → **新问题有新 ID**，不会串台。
//
// 这正是"**能从黑板算出来的，就不要单独存**"的第三次应用 ——
// 而这次它换来的不是一个少存的字段，而是**恢复能对上号**。
func (s *State) AttemptFor(k schema.Kind) int {
	return s.Versions(string(k)) + 1
}

// Latest 实现 `schema.Latest`：返回某个 key 的当前最新版。
//
// 签名与那个类型**逐字一致**，所以 `s.Latest` 可以直接当参数传给
// `schema.StaleReasons` —— 不需要一层包装函数。
//
// （之所以能做到，是因为 `schema.Latest` 被定义成函数类型而不是接口：
// 一个方法值天然满足它。这也是 L2 那个设计的红利。）
func (s *State) Latest(key string) (schema.Artifact, bool) {
	versions := s.Board[strings.TrimSpace(key)]
	if len(versions) == 0 {
		return schema.Artifact{}, false
	}
	// 深拷贝：调用方（StaleReasons）只读，但规矩不能靠"它反正不会改"。
	a := versions[len(versions)-1]
	a.Payload = append([]byte(nil), a.Payload...)
	return a, true
}

// Versions 返回某个 key 已有的版本数（不存在为 0）。
func (s *State) Versions(key string) int {
	return len(s.Board[strings.TrimSpace(key)])
}

// Keys 返回黑板上出现过的全部 key，**字典序**排列。
//
// 排序是硬要求（与 `Blackboard.Keys` 同一条）：map 遍历顺序随机，
// 会让"同一份输入打印两次却不一样"，于是无法 diff、无法回归。
func (s *State) Keys() []string {
	out := make([]string, 0, len(s.Board))
	for k := range s.Board {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ─── 完成判据：能不能跳过 ───────────────────────────────────────────

// SkipVerdict 是"某份产物能不能被信任"的结论。
//
// 注意它**不是 bool** —— 理由与 L1 的 `Deliverable()` 一样：
// 一个光秃秃的 `false` 只告诉调用方"不行"，不告诉它"差什么"。
// 而"差什么"才是能驱动下一步的信息（可自愈）。
type SkipVerdict struct {
	Skip bool

	// Reason 是**不能跳过的原因**，写成一句可执行的说明。
	// Skip 为 true 时为空。
	Reason string
}

// CanSkip 判断某个产物是否已经"做完且有效"，从而在下一次恢复时可以跳过。
//
// # 判据就是 L2 定好的那三条
//
//	可以跳过 ⟺ 产物「存在 ∧ 完整 ∧ 新鲜」
//
//	· 存在 —— 黑板上有这个 key
//	· 完整 —— `Artifact.Shortfall` 为空（L2-2）
//	· 新鲜 —— `StaleReasons` 为空（L2-1）
//
// ⚠️ 这三条在 L2 是"**能不能交付**"的判据，在 L3 是"**能不能跳过**"的判据。
// 同一个判据跨阶段复用 —— 这说明它抓到的是本质，不是某个场景的巧合。
//
// 三条各自报各自的错，**不合并**：因为它们的修法完全不同。
//
//	不存在 → 去跑那个节点
//	不完整 → 重跑那个节点（这次要成功）
//	不新鲜 → **先去重做上游**，再回来（顺序反了就白做一遍）
func (s *State) CanSkip(key string) SkipVerdict {
	key = strings.TrimSpace(key)
	versions := s.Board[key]
	if len(versions) == 0 {
		return SkipVerdict{Reason: fmt.Sprintf("还没有产出 %s", key)}
	}
	a := versions[len(versions)-1]

	if len(a.Shortfall) > 0 {
		return SkipVerdict{Reason: fmt.Sprintf("%s 只完成了一部分：%s",
			schema.RefOf(a), strings.Join(a.Shortfall, "；"))}
	}

	if reasons := schema.StaleReasons(a, s.Latest); len(reasons) > 0 {
		// StaleReasons 的输出顺序就是**可执行的修复顺序**（后序遍历换来的），
		// 所以这里不必再排序、也不必再组织语言。
		return SkipVerdict{Reason: strings.Join(reasons, "\n")}
	}
	return SkipVerdict{Skip: true}
}

// ─── 时间戳 ─────────────────────────────────────────────────────────

// Stamp 记录"这份快照刚刚被更新过"。
//
// ⚠️ 为什么不让 `Marshal` 顺手做掉？
//
//	因为 `Marshal` 这个名字**承诺了它是纯函数**。一个会改自身状态的 Marshal：
//	  · 无法断言"两次序列化结果一致"（第二次的 UpdatedAt 已经变了）
//	  · 无法在多处安全调用（每次调用都改了数据）
//
//	**名字承诺了什么，就不要偷偷做别的。**
//	省下这一行，换来的是一个不可测、不可重入的函数 —— 不划算。
//
// 谁有权改时间戳，就由谁显式地调它（Runner 在保存前调一次）。
func (s *State) Stamp(now time.Time) {
	s.UpdatedAt = now.UTC()
}

// ─── 序列化 ─────────────────────────────────────────────────────────

// Marshal 序列化成可写盘的字节。
//
// ⚠️ **写盘之前先自校验** —— 这一条是有代价意识的：
//
//	一份坏快照写到磁盘之后，只有**另一个进程、在很久以后、带着极少的上下文**
//	才能发现它。而此刻（写方）拥有全部上下文：知道自己刚做了什么。
//
//	**能在源头挡住的问题，不要留给下游的读方。**
//
// 这与 renderer 的"写盘前预检"是同一条（06-L3设计.md §5.4）：
// 事后的每一个修复动作，都在已经造成的损失上做补救。
//
// 它**不改时间戳**（见 Stamp 的注释），所以同一份 State 序列化两次，
// 字节完全一致 —— `encoding/json` 对 map 会按键排序输出，
// 而 `Artifact.Payload` 是 `RawMessage`（会被 compact），
// 于是"输出稳定"（L1-6）是白拿的。
func (s *State) Marshal() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("快照自校验失败，拒绝写盘: %w", err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("序列化快照失败: %w", err)
	}
	return raw, nil
}

// UnmarshalState 从字节还原一份快照，并**判它能不能用**。
//
// # 错误信息要能区分"读不出来"和"读出来了但不能用"
//
//	JSON 解不开        → corrupt（可能是写到一半被杀 / 文件损坏）
//	解得开但结构不对    → corrupt（字段缺失、黑板版本序列有洞）
//	版本读不懂         → from_future / too_old（不是"损坏"，是"我们读不懂"）
//
// 这三种的**处置完全不同**，所以不能都回一句 "invalid checkpoint"。
func UnmarshalState(raw []byte) (*State, error) {
	if len(raw) == 0 {
		return nil, withSubject(
			&ResumeError{Kind: ResumeCorrupt, Detail: "快照内容为空"}, "快照", "")
	}

	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		// ⚠️ 必须把"可能是写到一半被杀"这层意思说出来 ——
		// `json` 的原生错误只会说 "unexpected end of JSON input"，
		// 排查的人完全看不出"是上次写崩了"还是"这个文件本来就不是 JSON"。
		//
		// 这里**还没有 run_id**（JSON 都没解开），所以 id 留空 ——
		// 空 id 不会退化成一对空引号（见 `ResumeError.Error`）。
		return nil, withSubject(&ResumeError{
			Kind: ResumeCorrupt,
			Detail: fmt.Sprintf(
				"快照不是合法 JSON（常见原因：上次写盘被中断 / 文件损坏）: %v", err),
		}, "快照", "")
	}

	if err := s.CheckRestorable(); err != nil {
		return nil, err
	}
	return &s, nil
}

// ─── 校验 ───────────────────────────────────────────────────────────

// Validate 校验快照的**结构与自洽性**（不判版本范围 —— 那是 CheckRestorable 的第一步）。
//
// ⚠️ 它**包含黑板自洽性检查**（版本序列不能有洞）。
// 这不是锦上添花：`Get("outline@2")` 靠 `versions[1]` 取值，
// 序列里一旦有洞，取值就会**静默地命中另一版**。
// 一份被错取的产物比一份读不出来的产物危险得多 ——
// 后者让任务失败，前者让任务**成功地做错**。
func (s *State) Validate() error {
	switch {
	case s.Version <= 0:
		return fmt.Errorf("快照的 format version=%d 不合法", s.Version)
	case strings.TrimSpace(s.RunID) == "":
		return fmt.Errorf("快照缺少 run_id")
	case len(s.Board) == 0:
		return fmt.Errorf("快照的黑板是空的 —— 这一轮什么都没做过，不该存在")
	case s.CreatedAt.IsZero():
		return fmt.Errorf("快照缺少 created_at")
	case s.UpdatedAt.IsZero():
		return fmt.Errorf("快照缺少 updated_at")
	}

	// ① 输入产物必须在。它不只是"一条数据" ——
	//    没有它，`Topic()` 会失败，而"用户改了主题"这条链整条断掉。
	if _, err := s.Topic(); err != nil {
		return err
	}

	// ② 挂在身上的中断必须自洽，而且**必须属于同一轮**。
	//
	//    后者是一次"串台"检查：把 A 会话的中断挂到 B 会话的 checkpoint 上，
	//    后果是**人的回答被应用到另一件事上** —— 没有任何一层会报错。
	if s.Pending != nil {
		if err := s.Pending.Validate(); err != nil {
			return fmt.Errorf("快照里挂着一次不合法的中断: %w", err)
		}
		if s.Pending.RunID != s.RunID {
			return fmt.Errorf("快照 run_id=%s，但它挂的中断 %s 属于另一轮 %s（串台）",
				s.RunID, s.Pending.ID, s.Pending.RunID)
		}
	}

	// ③ 黑板必须自洽（版本连续、信封与位置一致、每份都能过校验）。
	if _, err := s.RestoreBoard(); err != nil {
		return err
	}
	return nil
}

// CheckRestorable 判断"这份快照现在能不能被恢复"。
//
// 两步，**顺序不能换**（与 `Interrupt.CheckResumable` 同一条）：
//
//	① 版本 —— 我读得懂它吗？
//	② Validate —— 它自己写得对吗？
//
// 理由在 `Interrupt.CheckResumable` 里写全了：来自未来版本的记录
// 可能有**不同的结构**，用本版本的规则去校验它，会得出"缺字段"这种
// **误导性的**结论 —— 而**误导性的错误比没有错误更难查**。
func (s *State) CheckRestorable() error {
	// 上下文**只在这一处补**（见 `withSubject`）。
	// ⚠️ 这个 subject 是"快照"而不是"中断" —— `ResumeError` 被两处共用，
	//	写死其中一个的名字就会**报错了对象**。
	return withSubject(s.checkRestorable(), "快照", s.RunID)
}

// checkRestorable 只描述"哪里不对"，不关心自己是谁。
func (s *State) checkRestorable() error {
	// ① 版本
	if err := checkProtocolVersion(s.Version, StateMinVersion, StateVersion); err != nil {
		return err
	}
	// ② 结构与自洽
	if err := s.Validate(); err != nil {
		return &ResumeError{Kind: ResumeCorrupt, Detail: err.Error()}
	}
	return nil
}

// ─── 深拷贝 ─────────────────────────────────────────────────────────

// Clone 深拷贝一份状态。
//
// 为什么需要它：快照是**会被反复改写**的东西（每跑一个节点就多几版产物）。
// 持有者要是把一份 State 的**引用**留在手里，之后任何一次改写都会
// **连带改掉他手上那份** —— 而这个坑本项目已经踩过三次
// （`Report.Merged` / `render_slide` / `MergeLocal`），黑板更是为此专门写了注释。
//
// 这里非拷不可的字段：`Board`（map + 切片的切片）、每份产物的
// `Payload`/`Shortfall`/`BasedOn`（都是引用类型）、`Pending`（指针）、`Hints`（map）。
// 其余是值类型，赋值即拷贝。
func (s *State) Clone() *State {
	out := *s

	out.Hints = maps.Clone(s.Hints)

	out.Board = make(map[string][]schema.Artifact, len(s.Board))
	for k, versions := range s.Board {
		cp := make([]schema.Artifact, 0, len(versions))
		for _, a := range versions {
			a.Payload = append([]byte(nil), a.Payload...)
			a.Shortfall = append([]string(nil), a.Shortfall...)
			a.BasedOn = append([]schema.Basis(nil), a.BasedOn...)
			cp = append(cp, a)
		}
		out.Board[k] = cp
	}

	if s.Pending != nil {
		p := s.Pending.Clone()
		out.Pending = &p
	}
	return &out
}
