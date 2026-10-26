package observ

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ─── 空对象 ─────────────────────────────────────────────────────────

// ⚠️ 空对象哲学（学企业仓库的 `EmptyLimiter`）：
//
//	**"不观测"是一个合法的状态，不是错误** —— 于是**没有调用点需要判空**。
func TestFromNormalizesNilToNop(t *testing.T) {
	var nilTracer Tracer
	got := From(nilTracer)
	if got == nil {
		t.Fatal("From(nil) 不该返回 nil —— 那会让每个使用点都要判空")
	}
	got.Trace(Event{Layer: LayerGraph, Kind: KindDone}) // 不 panic 就够

	// 非 nil 的原样返回（From 是**归一**，不是包装）
	rec := NewRecorder()
	if From(rec) != Tracer(rec) {
		t.Error("From 不该包装一个已经非 nil 的 Tracer")
	}
}

// 普通函数直接满足 Tracer —— 适配器不必为每种出口定义类型。
func TestTracerFunc(t *testing.T) {
	var got []Kind
	tr := TracerFunc(func(e Event) { got = append(got, e.Kind) })

	tr.Trace(Event{Layer: LayerGraph, Kind: KindStart, Phase: "x"})
	tr.Trace(Event{Layer: LayerGraph, Kind: KindDone})

	if len(got) != 2 || got[0] != KindStart || got[1] != KindDone {
		t.Errorf("TracerFunc 没有正确转发：%v", got)
	}
}

// ─── Recorder ───────────────────────────────────────────────────────

func TestRecorderCollectsAndValidates(t *testing.T) {
	r := NewRecorder()

	ok := Event{Layer: LayerLLM, Phase: "research", Kind: KindLLMCall,
		PromptTokens: 10, Model: "m"}
	r.Trace(ok)

	// 一条**非法**事件：`KindLLMCall` 必须带用量
	r.Trace(Event{Layer: LayerLLM, Phase: "research", Kind: KindLLMCall})

	// ① **如实记录**：非法事件也被收下（拒绝会让"发生了什么"更难查）
	if len(r.Events()) != 2 {
		t.Errorf("两条都该被收下，实际 %d 条", len(r.Events()))
	}
	// ② 但它被**记账**了
	invalid := r.Invalid()
	if len(invalid) != 1 {
		t.Fatalf("应当记下 1 条非法事件，实际 %d 条", len(invalid))
	}
	if !strings.Contains(invalid[0].Error(), "必须带用量") {
		t.Errorf("记账内容应说清为什么，实际：%v", invalid[0])
	}
	// ③ 按类查询
	if r.Count(KindLLMCall) != 2 {
		t.Errorf("Count(KindLLMCall) = %d，期望 2", r.Count(KindLLMCall))
	}
	if r.Count(KindDone) != 0 {
		t.Error("没发过 KindDone，Count 不该是 1")
	}
}

// ⚠️ **口径写死在 `Usage()` 里**：只累加 `KindLLMCall`。
//
//	如果两个地方各算一遍"总数"，它们迟早会不一致（L2-3 的教训）。
func TestRecorderUsageOnlyCountsLLMCalls(t *testing.T) {
	r := NewRecorder()
	r.Trace(Event{Layer: LayerLLM, Phase: "a", Kind: KindLLMCall, PromptTokens: 100, CompletionTokens: 10})
	r.Trace(Event{Layer: LayerLLM, Phase: "b", Kind: KindLLMCall, PromptTokens: 200, CompletionTokens: 20})
	// 这条**不是**模型调用 —— 它的 tokens 不该被算进去
	r.Trace(Event{Layer: LayerGraph, Kind: KindProduct, PromptTokens: 9999})

	p, c := r.Usage()
	if p != 300 || c != 30 {
		t.Errorf("Usage = (%d, %d)，期望 (300, 30) —— 只有 KindLLMCall 该被累加", p, c)
	}
}

// ⚠️ 并发安全必须**被验证**，否则它只是一种说法 ——
//
//	但这条测试的验证是**有限**的，而这必须写下来。
//
// # 它验证了什么、没验证什么
//
//	✅ 能抓到：忘了加锁导致的**丢事件**（append 互相覆盖、切片重分配）
//	❌ 抓不到：真正的**数据竞争** —— 那需要 `-race`
//
//	而 `-race` 需要 cgo，**本机没有 C 编译器**（`gcc not found`）：
//
//		go test -race  →  "-race requires cgo"
//		CGO_ENABLED=1  →  "C compiler gcc not found"
//
//	> ### 这是"跑过了"与"验证过了"的差别
//	> 这条测试**跑过了**，而它证明的东西**比名字暗示的少**。
//	> 名字叫 `ConcurrencySafe`，而它实际证明的是"NoEventsLost"。
//	>
//	**所以结论是"半验证 + 已知缺口"，不是"已验证"。**
//	（补上它需要装一个 C 编译器 —— 那是环境问题，不是代码问题；
//	  在补上之前，任何"Recorder 并发安全"的说法都该带着这个限定。）
func TestRecorderIsConcurrencySafe(t *testing.T) {
	const goroutines, each = 8, 50
	r := NewRecorder()

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				r.Trace(Event{Layer: LayerGraph, Kind: KindDone, Phase: "p"})
			}
		}()
	}
	wg.Wait()

	if got := len(r.Events()); got != goroutines*each {
		t.Errorf("并发写入丢了事件：%d 条，期望 %d 条", got, goroutines*each)
	}
	if r.Count(KindDone) != goroutines*each {
		t.Error("Count 与 Events 不一致 —— 两个读数走了不同的账")
	}
}

func TestRecorderReset(t *testing.T) {
	r := NewRecorder()
	r.Trace(Event{Layer: LayerGraph, Kind: KindDone})
	r.Trace(Event{Layer: LayerGraph, Kind: "不合法"})
	r.Reset()

	if len(r.Events()) != 0 || len(r.Invalid()) != 0 {
		t.Error("Reset 之后应当什么都没有")
	}
}

// ─── JSONLTracer（录播）──────────────────────────────────────────────

// ⚠️ 录播要能被**读回来** —— 而"行号就是序号"这件事由**位置**保证。
func TestJSONLTracerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace", "run-1.jsonl")

	tr, err := NewJSONLTracer(path) // 父目录会被自动创建
	if err != nil {
		t.Fatalf("打开录播失败: %v", err)
	}
	sent := []Kind{KindStart, KindLLMCall, KindDone}
	for _, k := range sent {
		tr.Trace(Event{Layer: LayerGraph, Phase: "p", Kind: k,
			PromptTokens: 1, CompletionTokens: 1, Model: "m"})
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if tr.Lines() != 3 {
		t.Errorf("Lines() = %d，期望 3", tr.Lines())
	}

	// 读回来，**逐行**都是合法 JSON，且顺序与写入顺序一致
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var back []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v", len(back)+1, err)
		}
		back = append(back, e)
	}
	if len(back) != len(sent) {
		t.Fatalf("读回 %d 行，期望 %d 行", len(back), len(sent))
	}
	for i, k := range sent {
		if back[i].Kind != k {
			t.Errorf("第 %d 行（行号即序号）应当是 %s，实际 %s", i+1, k, back[i].Kind)
		}
	}
	// 正文与元信息都要在（这是**录播**，不是脱敏后的上游）
	if back[1].Model != "m" {
		t.Error("录播丢了元信息")
	}
}

// ⚠️ 录播是**追加**的（它是历史，不是状态）—— 重开一个不该截断已有内容。
//
//	（对照：checkpoint 是**原子替换**的。两者方向不同，因为一个是历史、一个是状态。）
func TestJSONLTracerAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")

	first, err := NewJSONLTracer(path)
	if err != nil {
		t.Fatal(err)
	}
	first.Trace(Event{Layer: LayerGraph, Kind: KindDone})
	first.Close()

	second, err := NewJSONLTracer(path)
	if err != nil {
		t.Fatal(err)
	}
	second.Trace(Event{Layer: LayerGraph, Kind: KindStart, Phase: "p"})
	second.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; got != 2 {
		t.Errorf("应当是 2 行（追加），实际 %d 行：%s", got, raw)
	}
}

// ⚠️ 写失败**不许被吞掉** —— 但也不许把业务拖垮。
//
//	所以它记账（`Err()`）+ **一次性**告警到 stderr。
//	（"观测自己的失败，只能走观测之外的路。"）
func TestJSONLTracerRecordsWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	tr, err := NewJSONLTracer(path)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Err() != nil {
		t.Fatal("刚开始不该有错")
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	// 文件已关 —— 写必然失败
	tr.Trace(Event{Layer: LayerGraph, Kind: KindDone})

	if tr.Err() == nil {
		t.Fatal("写失败被吞掉了 —— 那正是 L1-3/L2-11 那个病")
	}
	if tr.Lines() != 0 {
		t.Error("没写成功就不该涨行数（行数必须是**事实**，不是意图）")
	}
	// 第二次失败不再改 `Err()`（第一条才有诊断价值）
	first := tr.Err()
	tr.Trace(Event{Layer: LayerGraph, Kind: KindDone})
	if tr.Err() != first {
		t.Error("第一条错才有诊断价值 —— 后续失败不该覆盖它")
	}
}

// ─── 组合 ───────────────────────────────────────────────────────────

func TestFanoutOrderAndNilTolerance(t *testing.T) {
	var order []string
	a := TracerFunc(func(Event) { order = append(order, "a") })
	b := TracerFunc(func(Event) { order = append(order, "b") })

	// 中间夹一个 nil —— 不该 panic，也不该挡住别人
	Fanout{a, nil, b}.Trace(Event{Layer: LayerGraph, Kind: KindDone})

	if strings.Join(order, ",") != "a,b" {
		t.Errorf("Fanout 应按参数顺序调用且跳过 nil，实际 %v", order)
	}
}

// ⚠️ `WithRedact` 让"这个出口只能收到脱敏版"成为**类型上的事实**，
//
//	而不是"调用方记得调 Redact"（R23 那条教训）。
func TestWithRedactSendsRedactedOnly(t *testing.T) {
	var got []Event
	remote := TracerFunc(func(e Event) { got = append(got, e) })

	WithRedact(remote).Trace(Event{
		Layer: LayerLLM, Phase: "p", Kind: KindLLMCall,
		Model: "m", PromptTokens: 1,
		SystemPrompt: "sp", UserPrompt: "up", Response: "resp",
	})

	if len(got) != 1 {
		t.Fatalf("应当转交 1 条，实际 %d 条", len(got))
	}
	e := got[0]
	if e.SystemPrompt != "" || e.UserPrompt != "" || e.Response != "" {
		t.Errorf("正文泄露到远端出口了：%+v", e)
	}
	if e.Model != "m" || e.PromptTokens != 1 || e.Phase != "p" {
		t.Errorf("元信息丢了：%+v", e)
	}
}

// ⭐ 这是原理一的实物：**同一个事件，两个出口拿到两种投影**。
func TestLocalAndRemoteGivesTwoProjections(t *testing.T) {
	var local, remote []Event

	full := Event{
		Layer: LayerLLM, Phase: "p", Kind: KindLLMCall, Model: "m", PromptTokens: 1,
		SystemPrompt: "sp", UserPrompt: "up", Response: "resp",
	}
	LocalAndRemote(
		TracerFunc(func(e Event) { local = append(local, e) }),
		TracerFunc(func(e Event) { remote = append(remote, e) }),
	).Trace(full)

	if len(local) != 1 || len(remote) != 1 {
		t.Fatal("两个出口都该收到一条")
	}
	// 本地：**完整**（离线复盘的依据）
	if local[0].Response != "resp" || local[0].UserPrompt != "up" {
		t.Errorf("本地出口应当拿到完整版：%+v", local[0])
	}
	// 远端：**脱敏**
	if remote[0].Response != "" || remote[0].UserPrompt != "" || remote[0].SystemPrompt != "" {
		t.Errorf("远端出口应当拿到脱敏版：%+v", remote[0])
	}
	// 而两者的**元信息必须完全一致**（否则聚合出来的两个数会对不上）
	if remote[0].Model != local[0].Model || remote[0].PromptTokens != local[0].PromptTokens {
		t.Error("两个投影的元信息不一致 —— 聚合会因此对不上")
	}
}
