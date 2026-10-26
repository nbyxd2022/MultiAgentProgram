package observ

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ─── 端口 ───────────────────────────────────────────────────────────

// Tracer 是观测的**唯一出口**。
//
// # ⚠️ 它**不返回 error**
//
//	观测失败不该让业务失败 —— 判据是那条一直在用的
//	"**失败的严重性由能不能补救决定**"（R30 精读 `cached.go` 得来的）：
//
//		业务失败  → 不可重建 → 必须上报
//		观测失败  → **可重建**（重跑一次就有了）→ 不该拖垮业务
//
//	而"录播写失败了"这件事**不会被吞掉** —— 实现**自己记账**：
//
//		`JSONLTracer.Err()`   —— 谁在乎谁去查
//		`Recorder.Invalid()`  —— 开发期出口会顺手校验，非法事件记在这里
//
//	这与 `StatsReporter`（可选能力）、`EmptyLimiter`（空对象）是同一条取舍：
//	**接口要小，能力要可探测。**
type Tracer interface {
	Trace(Event)
}

// TracerFunc 让一个普通函数直接满足 Tracer（学 `http.HandlerFunc`）。
//
// 用途：写**适配器**（打印到终端、转成 OTLP、喂给 Prometheus）时
// 不必为每种出口定义一个类型 —— **适配器的活儿是翻译，不是造类型。**
type TracerFunc func(Event)

func (f TracerFunc) Trace(e Event) { f(e) }

// NopTracer 不记录任何东西。
//
// ⚠️ 它**必须存在**，而不是让调用方传 nil ——
//
//	学企业仓库的 `EmptyLimiter`：
//
//		"未注册 ≠ 错误。**不限额是一个合法的状态**，不是错误。"
//		（R38 精读的结论）
//
//	如果 `Tracer` 可以是 nil，那**每个透传点都要判空** ——
//	而"每个调用点都要记得做同一件事"就是 R23 那条教训的形状。
type NopTracer struct{}

func (NopTracer) Trace(Event) {}

// From 把一个可能为 nil 的 Tracer 归一成**永不为 nil** 的。
//
// ⚠️ 只在**装配的边界**调用它一次（`NewRunner` / `NewAgent`），
//
//	而不是在每个使用点上判空 —— 那样就又回到"每个调用点都要记得"了。
func From(t Tracer) Tracer {
	if t == nil {
		return NopTracer{}
	}
	return t
}

// ─── 出口一：内存收集（开发 / 测试 / 评测）──────────────────────────

// Recorder 把事件收在内存里。
//
// 它是**开发期的出口** —— 因此它顺手做一件生产出口不做的事：**校验**（见 `Trace`）。
//
// # ⚠️ 它是并发安全的 —— 但这个说法要**限定范围**
//
//	评测可能并发跑用例（那是它的主要用途），而"每个用例独占一个 Recorder"
//	不是一个能靠约定保证的约束。
//
//	加锁三行，换掉"要不要共享"这个问题。**判据是失败方向**：
//
//		加锁（现在可能用不到）→ 代价 3 行 + 一点性能，**无误伤**
//		不加锁（未来某个并发使用）→ **静默丢数据** ← 最难查的那一类
//
//	⚠️ 但**验证是"半"的**，这一点必须写下来：
//
//		并发测试只能断言"**不丢事件**"（那能抓到大部分"忘了加锁"）——
//		它**抓不到真正的数据竞争**，那需要 `-race`，
//		而 `-race` 需要 cgo，**本机没有 C 编译器**。
//
//	> **"跑过了"不等于"验证过了"** —— 当验证需要的能力不存在时，
//	> 要么补上那个能力，要么**如实标注这是一个已知缺口**。
//	> （不许把它说成"已验证"。）
type Recorder struct {
	mu      sync.Mutex
	events  []Event
	invalid []error
}

func NewRecorder() *Recorder { return &Recorder{} }

// Trace 收下一个事件，并顺手校验它。
//
// # ⚠️ 校验放在这里，是因为它是"源头"里**开发期那一侧**
//
//	判据原本是"**能在源头挡住的问题，不要留给下游的读方**"
//	（`State.Marshal` 写盘前自校验的同一条）。
//
//	但源头分两种：
//
//		生产期的源头 → 每个事件都校验 → **没有这个预算**
//		开发期的源头 → 事件量小、出错要立刻知道 → **校验是对的**
//
//	`Recorder` 就是开发期的出口。生产出口（JSONL / OTLP）不校验。
//
// # 校验失败的事件**仍然被收下**
//
//	观测的第一职责是"**如实记录**"，不是"拒绝脏数据" ——
//	拒绝会让"究竟发生了什么"更难查（少了那一条）。
//	所以失败只**记账**，由调用方（测试）断言 `Invalid()` 为空。
func (r *Recorder) Trace(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := e.Validate(); err != nil {
		r.invalid = append(r.invalid, err)
	}
	r.events = append(r.events, e)
}

// Events 返回收下的事件（**深拷贝**）。
//
// ⚠️ 拷贝的是切片，不是每个事件 —— `Event` 里唯一的引用类型是 `*Reconcile`，
//
//	而它**只在构造时写一次**（不是"会被反复改的状态"）。
//	（对照：`Blackboard` 的产物要深拷，因为 `Payload` 是会被改的字节。
//	 **要不要深拷，取决于"拿到之后会不会有人改它"。**）
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// OfKind 返回某一类事件（按收下顺序）。
func (r *Recorder) OfKind(k Kind) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// Count 返回某一类事件的条数。
func (r *Recorder) Count(k Kind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

// Invalid 返回校验失败的事件原因（**测试断言 `len(...) == 0`**）。
func (r *Recorder) Invalid() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]error, len(r.invalid))
	copy(out, r.invalid)
	return out
}

// Usage 把**模型调用事件**的用量加起来。
//
// # ⚠️ 它是"总量"的唯一来源
//
//	`Event` 里**没有"总数"字段**，一轮的总量就是"把 `KindLLMCall` 加起来"。
//
//	理由不是"省一个字段"，而是 L2-3 的教训：
//	**一个由多处维护的数字，一定会不一致。**
//	让它只有一个来源（事件流），总量就永远不会跟明细打架。
//
// ⚠️ 它只累加 `KindLLMCall` —— 不按 `Layer` 或"全部事件"累加。
//
//	**口径必须写死在这里**，否则两处算法一漂移，"总数"就又有了两个版本。
func (r *Recorder) Usage() (promptTokens, completionTokens int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Kind != KindLLMCall {
			continue
		}
		promptTokens += e.PromptTokens
		completionTokens += e.CompletionTokens
	}
	return
}

// Reset 清空（评测在用例之间复用一个 Recorder 时用）。
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
	r.invalid = nil
}

// ─── 出口二：JSONL 录播（事后可重放、可 diff）────────────────────────

// JSONLTracer 把事件**逐行**写盘。
//
// # ⚠️ 一个文件 = 一轮运行（学企业仓库 tasklog 的 Collector-per-request）
//
//	那边的做法是 `tasklog.NewCollector(reqID)` —— **每个请求一个收集器**，
//	而不是一个全局的日志流。于是天然隔离（并发请求不串台）、
//	生命周期清楚（请求结束就序列化存起来）。
//
//	我们用它：`out/l4/trace/<runID>.jsonl`。**不靠 `RunID` 字段过滤，
//	而是让文件天然分家** —— 少一次"想读哪一轮"的判断，就少一处判断写错的机会。
//
// # ⚠️ 它**没有缓冲**（每个事件一次 write）
//
//	录播的用途是"**事后排查**" —— 而排查的场景**往往就是崩溃**。
//
//		无缓冲 → 已写的都在（崩了只丢最后一条）
//		有缓冲 → 快，但**崩溃时最后 N 条丢失** —— 而那 N 条正是崩溃前的现场
//
//	所以这里选"慢一点但都在"。**这与 checkpoint 的取舍是同一类问题的不同答案**：
//
//		checkpoint（状态）→ **要么全有要么全无**（临时文件 + rename 原子替换）
//		录播（历史）      → **追加，已写的不可丢**（无缓冲）
//
//	两者都是"为崩溃场景选的落盘策略"，方向不同是因为**一个是状态、一个是历史**。
type JSONLTracer struct {
	mu       sync.Mutex
	f        *os.File
	enc      *json.Encoder
	lines    int
	firstErr error
}

// NewJSONLTracer 打开（或创建）一个录播文件。父目录会自动创建。
func NewJSONLTracer(path string) (*JSONLTracer, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建录播目录失败: %w", err)
		}
	}
	// O_APPEND：录播是**追加**的（它是历史，不是状态）。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开录播文件失败: %w", err)
	}
	return &JSONLTracer{f: f, enc: json.NewEncoder(f)}, nil
}

// Trace 写一行。
//
// # ⚠️ 写失败时的处置：**记账 + 一次性告警到 stderr**
//
//	为什么是 stderr：**观测自己的失败，只能走观测之外的路。**
//	用观测去报"观测坏了"是循环的，而丢掉它又是"重要事实被降级成日志"
//	（L1-3 / L2-11 / F3 同一个病，第三次之后我们立了规矩）。
//
//	为什么是**一次性**：一个坏掉的磁盘会给出几十万条错误。
//	第一条有用，其余是噪音 —— 而噪音会把第一条埋掉。
func (t *JSONLTracer) Trace(e Event) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.firstErr != nil {
		return // 已经坏过一次了：不再刷屏（但 `Err()` 一直查得到）
	}
	if err := t.enc.Encode(e); err != nil {
		t.firstErr = err
		// 这条 printf 是**故意的**：它不能走 Tracer（见上）。
		fmt.Fprintf(os.Stderr, "!! 观测录播写失败（此后的失败不再重复报告）: %v\n", err)
		return
	}
	t.lines++
}

// Lines 返回已写出的行数 —— 而它同时是**行号**（每行一个事件，顺序天然存在）。
func (t *JSONLTracer) Lines() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lines
}

// Err 返回第一次写失败的原因（没有失败则为 nil）。
//
// ⚠️ 它是"谁在乎谁去查"的形态 —— 接口不返回 error，
//
//	但**失败没有被吞掉**：调用方在结束时查一次就够了。
func (t *JSONLTracer) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.firstErr
}

// Close 关闭文件。
func (t *JSONLTracer) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.f.Close()
}

// ─── 组合 ───────────────────────────────────────────────────────────

// Fanout 把一个事件发给**多个出口**，按**参数顺序**。
//
// ⚠️ 顺序是有意义的 —— 企业仓库 `emitTrace` 先写本地 tasklog、再发上游，
//
//	并特意写了注释。理由可以概括成一句：
//
//		**把"不可重建"的那个放前面。**
//
//	本地录播是历史（丢了就没了），而上游可以重试/补发。
//
// nil 元素被**跳过** —— 于是"某个出口这次没有"不需要单独处理
// （空对象哲学的又一次应用：让缺失不成为一个分支）。
type Fanout []Tracer

func (f Fanout) Trace(e Event) {
	for _, t := range f {
		if t == nil {
			continue
		}
		t.Trace(e)
	}
}

// WithRedact 返回一个**只转交脱敏版**的出口包装。
//
// # ⚠️ 它是两个出口之间的那个转换（R39 精读 `emitTrace` 得来的）
//
//	企业仓库的 `emitTrace` 干了三件事，而**"脱敏"是最容易被忘掉的一件**：
//
//		① 本地 tasklog  ← **完整**（offline replay 的依据）
//		② 上游 / 前端   ← **脱敏**（"避免把 prompt/response 泄露给前端"）
//		③ 顺序：先本地、后上游
//
//	我原以为两个出口的差别是"开 / 关"。**真正的差别是"完整 / 脱敏"。**
//
// # 为什么做成包装，而不是"调用方记得调 Redact"
//
//	因为那样每个转发点都要记得 —— 而"每个调用点都要记得做同一件事"
//	就是 R23 那条教训。**把它做成类型上的事实**，就不可能忘。
func WithRedact(inner Tracer) Tracer {
	return TracerFunc(func(e Event) {
		if inner != nil {
			inner.Trace(e.Redact())
		}
	})
}

// LocalAndRemote 是"本地拿完整版、远端拿脱敏版"的**命名组合**。
//
// 它是 `Fanout` + `WithRedact` 的语法糖 —— 但值得单独一个名字，
// 因为**这个形状本身是一条设计结论**：观测量分三层，而**保留要求相反**
// （原理一）。名字让那条约定的意图在调用点上直接可读：
//
//	tracer := observ.LocalAndRemote(
//		local,                                  // 完整：JSONL 录播 / Recorder
//		observ.WithRedact(remote),              // ❌ 这样写就重复了
//	)
//
// 正确用法：
//
//	tracer := observ.LocalAndRemote(file, remote)
//
// ⚠️ 参数顺序 = 调用顺序，**本地在前**（不可重建的先拿）。
func LocalAndRemote(local, remote Tracer) Tracer {
	return Fanout{local, WithRedact(remote)}
}
