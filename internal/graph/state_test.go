package graph

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"multiagent/internal/schema"
	"multiagent/internal/state"
)

// ─── 测试脚手架 ─────────────────────────────────────────────────────

// art 造一份产物。
//
// 直接构造结构体而不是走 `schema.NewArtifact`，是因为测试需要
// **精确控制 Version 与 BasedOn** —— 而这两个恰恰是"存储"与"血缘"
// 的职责，正常路径下没法手填。测试有时候需要一点特权。
func art(key string, version int, opts ...func(*schema.Artifact)) schema.Artifact {
	a := schema.Artifact{
		Key:        key,
		Kind:       schema.Kind(key),
		Version:    version,
		ProducedBy: "test",
		Summary:    key + " 的摘要",
		Payload:    json.RawMessage(`{}`),
	}
	for _, o := range opts {
		o(&a)
	}
	return a
}

func withBasedOn(bs ...schema.Basis) func(*schema.Artifact) {
	return func(a *schema.Artifact) { a.BasedOn = bs }
}

func withShortfall(gaps ...string) func(*schema.Artifact) {
	return func(a *schema.Artifact) { a.Shortfall = gaps }
}

// newTestState 造一份基线快照（黑板上有 topic@1）。
func newTestState(t *testing.T) *State {
	t.Helper()
	st, err := NewState("run-1", "新能源汽车行业趋势", nil)
	if err != nil {
		t.Fatalf("构造状态失败: %v", err)
	}
	return st
}

// puttable 造一份**可以交给黑板**的产物（版本留空，由黑板分配）。
//
// ⚠️ 与 `art` 的区别很重要：`art` 直接构造结构体，测试要精确控制
// `Version`/`BasedOn`（那是"存储"和"血缘"的职责，正常路径下填不了）；
// 而 `Put` **会拒绝**外部指定的版本号 —— 所以走正常路径时必须用这个。
//
// 写测试时踩过一次：把 `art(key, 1)` 直接喂给 `Put`，得到
// "Put 不接受外部指定的版本号"。**这个拒绝是对的，是我的测试用错了工具。**
// ─── Hints：输入必须能跨进程活下来 ──────────────────────────────────

// ⚠️ Hint 是**输入** —— 它必须跟着快照一起走。
//
//	不走的话，"换了 Hint 重启"就是静默的：节点收到新的指令，
//	而它产出的产物血缘上仍写着旧的依据（07-L3复盘.md L3-1）。
func TestStateHintsRoundTrip(t *testing.T) {
	st, err := NewState("run-1", "主题", map[string]string{"outline": "控制在 3 页"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := st.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Hints["outline"] != "控制在 3 页" {
		t.Errorf("hints 没有跟着快照走：%+v", back.Hints)
	}
}

// ⚠️ `NewState` 必须**拷一份** hints —— 调用方（Runner）之后还在用它。
//
//	共享同一个 map 意味着"谁都能改输入" —— 而输入改了是**没有任何一层**
//	能发现的（这正是 L3-1 的形状）。
func TestNewStateCopiesHints(t *testing.T) {
	src := map[string]string{"outline": "3 页"}
	st, err := NewState("run-1", "主题", src)
	if err != nil {
		t.Fatal(err)
	}
	src["outline"] = "改掉了"
	if st.Hints["outline"] != "3 页" {
		t.Error("NewState 没有拷 hints —— 调用方改一下就把这一轮的输入改了")
	}
}

// `Clone` 也要深拷 hints（它是 map，赋值只拷了引用）。
func TestCloneDeepCopiesHints(t *testing.T) {
	st, err := NewState("run-1", "主题", map[string]string{"outline": "3 页"})
	if err != nil {
		t.Fatal(err)
	}
	cp := st.Clone()
	cp.Hints["outline"] = "改了"
	if st.Hints["outline"] != "3 页" {
		t.Error("Clone 没有深拷 hints")
	}
}

// nil 与空 map 在序列化之后都应还原成"没有任何额外要求"。
//
// ⚠️ 它们**不必**还原成同一个形态（`omitempty` 让 nil 的 key 直接消失），
//
//	所以断言比的是 `len()` 而不是 `== nil` —— 那是**语义**，不是表示。
func TestStateHintsNilAndEmptyAreEquivalent(t *testing.T) {
	for _, h := range []map[string]string{nil, {}} {
		st, err := NewState("run-1", "主题", h)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := st.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		back, err := UnmarshalState(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(back.Hints) != 0 {
			t.Errorf("空输入还原出了 %d 条 hint", len(back.Hints))
		}
	}
}

func puttable(key string, kind schema.Kind) schema.Artifact {
	return schema.Artifact{
		Key:        key,
		Kind:       kind,
		ProducedBy: "test",
		Summary:    key + " 的摘要",
		Payload:    json.RawMessage(`{}`),
	}
}

// put 往基线快照的黑板上塞一批产物（按 key）。
func put(st *State, artifacts ...schema.Artifact) {
	for _, a := range artifacts {
		st.Board[a.Key] = append(st.Board[a.Key], a)
	}
}

// ─── NewState：输入就是一份产物 ─────────────────────────────────────

// 输入必须以**产物**的形式进黑板，而不是只存在字段里。
// 这是"让根节点有上游"的全部机制所在。
func TestNewStateWritesTopicAsArtifact(t *testing.T) {
	st := newTestState(t)

	if got := st.Versions(KeyTopic); got != 1 {
		t.Fatalf("黑板上的 topic 应有 1 版，实际 %d 版", got)
	}

	a := st.Board[KeyTopic][0]
	if a.Kind != schema.KindTopic {
		t.Errorf("kind 应为 %s，实际 %s", schema.KindTopic, a.Kind)
	}
	if a.ProducedBy != ProducedByCode {
		t.Errorf("produced_by 应为 %q（它没有任何模型参与），实际 %q", ProducedByCode, a.ProducedBy)
	}
	if a.Version != 1 {
		t.Errorf("版本应为 1，实际 %d", a.Version)
	}

	// 信封上必须**看得出**它没经过模型 —— 排查时这一条能直接排除一整类可能。
	if _, err := st.Topic(); err != nil {
		t.Errorf("Topic() 应当能读出来，却报：%v", err)
	}
	if got, _ := st.Topic(); got != "新能源汽车行业趋势" {
		t.Errorf("主题读出来不对：%q", got)
	}
}

func TestNewStateRejectsEmptyInput(t *testing.T) {
	cases := []struct {
		name    string
		runID   string
		topic   string
		wantSub string
	}{
		{"run id 为空", "", "主题", "run id 不能为空"},
		{"run id 全是空白", "   ", "主题", "run id 不能为空"},
		{"主题为空", "run-1", "", "主题不能为空"},
		{"主题全是空白", "run-1", "  \t ", "主题不能为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewState(c.runID, c.topic, nil); err == nil {
				t.Fatal("应当被拒绝")
			} else if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误里应含 %q，实际：%v", c.wantSub, err)
			}
		})
	}
}

// ⚠️ 本文件最重要的一条测试：**State 里没有 Topic 字段**。
//
// 第一版设计里有 `Topic string`，那会埋下一个"会说谎的字段"：
// 主题的正确值在黑板上的 `topic@1`，字段是它的**副本** ——
// 主题一改（topic@2），字段就过期了，而且没有任何一层能发现。
//
// 这与 L2-1（血缘/新鲜度）是**同一个错**，只是发生在状态自己身上。
// 所以这里断言的是"没有可撒谎的副本"：换了黑板，读出来的就必须跟着换。
func TestStateTopicHasNoStaleCopy(t *testing.T) {
	st := newTestState(t)

	// 模拟"用户改了主题"：黑板上多一版 topic@2。
	//
	// 注意这里**只动黑板**。如果 State 里还有一份 Topic 副本，
	// 它就会停在旧值上 —— 而本测试的下一条断言会立刻抓到。
	put(st, art(KeyTopic, 2, func(a *schema.Artifact) {
		a.Payload = json.RawMessage(`{"text":"AI 应用落地现状"}`)
	}))

	got, err := st.Topic()
	if err != nil {
		t.Fatalf("Topic() 失败: %v", err)
	}
	if got != "AI 应用落地现状" {
		t.Errorf("Topic() 必须**从黑板读**（应得到新值 %q），实际 %q —— "+
			"说明 State 里存了一份会过期的副本", "AI 应用落地现状", got)
	}
}

// ─── 完成判据：存在 ∧ 完整 ∧ 新鲜 ──────────────────────────────────

func TestCanSkip(t *testing.T) {
	t.Run("不存在 → 不能跳过", func(t *testing.T) {
		v := newTestState(t).CanSkip("research")
		if v.Skip {
			t.Fatal("没有的产物不该被标成可跳过")
		}
		if !strings.Contains(v.Reason, "还没有产出 research") {
			t.Errorf("原因应说清「没有产出」，实际：%q", v.Reason)
		}
	})

	t.Run("存在且完整且新鲜 → 可以跳过", func(t *testing.T) {
		st := newTestState(t)
		put(st, art("research", 1, withBasedOn(schema.BasisOf(st.Board[KeyTopic][0]))))

		v := st.CanSkip("research")
		if !v.Skip {
			t.Fatalf("应当可以跳过，却被拒：%s", v.Reason)
		}
		if v.Reason != "" {
			t.Errorf("可跳过时不该有原因，实际：%q", v.Reason)
		}
	})

	t.Run("存在但不完整 → 不能跳过（L2-2）", func(t *testing.T) {
		st := newTestState(t)
		put(st, art("render", 1, withShortfall("第 3 页：写盘失败")))

		v := st.CanSkip("render")
		if v.Skip {
			t.Fatal("有缺口的不该被标成可跳过")
		}
		// 错误信息要**可自愈**：说清缺什么（L1-5）。
		if !strings.Contains(v.Reason, "只完成了一部分") ||
			!strings.Contains(v.Reason, "第 3 页：写盘失败") {
			t.Errorf("原因应说清缺了什么，实际：%q", v.Reason)
		}
	})

	t.Run("存在且完整但不新鲜 → 不能跳过（L2-1）", func(t *testing.T) {
		st := newTestState(t)
		put(st,
			art("outline", 1), art("outline", 2),
			// design@1 的依据是 outline@1，而 outline 已经到 @2
			art("design", 1, withBasedOn(schema.Basis{Key: "outline", Version: 1})),
		)

		v := st.CanSkip("design")
		if v.Skip {
			t.Fatal("依据过期的产物不该被标成可跳过")
		}
		if !strings.Contains(v.Reason, "design@1") || !strings.Contains(v.Reason, "outline@2") {
			t.Errorf("原因应说清「谁依据了谁的新版本」，实际：%q", v.Reason)
		}
	})
}

// 三条判据必须**各自报各自的原因**，不能合并成一句"不行"。
//
// 因为它们**修法完全不同**：
//
//	不存在 → 去跑那个节点
//	不完整 → 重跑那个节点
//	不新鲜 → 先去重做上游，再回来（顺序反了就白做）
func TestCanSkipReasonsAreDistinct(t *testing.T) {
	missing := newTestState(t).CanSkip("research").Reason

	broken := newTestState(t)
	put(broken, art("research", 1, withShortfall("第 1 页失败")))
	incomplete := broken.CanSkip("research").Reason

	stale := newTestState(t)
	put(stale, art("outline", 1), art("outline", 2),
		art("design", 1, withBasedOn(schema.Basis{Key: "outline", Version: 1})))
	old := stale.CanSkip("design").Reason

	if missing == incomplete || incomplete == old || missing == old {
		t.Errorf("三种原因不该有相同的文案：\n  不存在=%q\n  不完整=%q\n  不新鲜=%q",
			missing, incomplete, old)
	}
}

// 主题变了，一切下游都该自动过期 —— **不需要任何为"改主题"写的特殊逻辑**。
//
// 这是"给根节点造一个上游"换来的东西：`StaleReasons` 一路通用，没有特例。
func TestChangingTopicInvalidatesEverythingDownstream(t *testing.T) {
	st := newTestState(t)
	topicV1 := schema.BasisOf(st.Board[KeyTopic][0])

	put(st,
		art("research", 1, withBasedOn(topicV1)),
		art("outline", 1, withBasedOn(schema.Basis{Key: "research", Version: 1})),
	)

	if v := st.CanSkip("outline"); !v.Skip {
		t.Fatalf("改主题之前 outline 应当可以跳过，却被拒：%s", v.Reason)
	}

	// 用户改了主题。
	put(st, art(KeyTopic, 2, func(a *schema.Artifact) {
		a.Payload = json.RawMessage(`{"text":"AI 应用落地现状"}`)
	}))

	if v := st.CanSkip("research"); v.Skip {
		t.Error("主题变了之后 research 必须过期")
	}
	// ⚠️ 关键：outline 依据的是 research@1，而 research 还没重做 ——
	// 它自己看是"新鲜"的，但**上游已经脏了**。
	// 传递遍历必须能发现这一点（否则"改了主题"只会重做第一跳）。
	if v := st.CanSkip("outline"); v.Skip {
		t.Errorf("上游脏了，下游即便自己看着新鲜也必须过期，实际却跳过了")
	}
}

// ─── 序列化 ─────────────────────────────────────────────────────────

func TestStateRoundTrip(t *testing.T) {
	st := newTestState(t)
	put(st, art("research", 1, withBasedOn(schema.BasisOf(st.Board[KeyTopic][0]))))
	st.Pending = ptr(NewInterrupt(st.RunID, "outline", 1, KindConfirmArtifact, "确认大纲？"))

	raw, err := st.Marshal()
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}

	back, err := UnmarshalState(raw)
	if err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}

	if back.RunID != st.RunID || back.Version != st.Version {
		t.Errorf("身份字段丢了：%+v", back)
	}
	if got, _ := back.Topic(); got != "新能源汽车行业趋势" {
		t.Errorf("主题丢了：%q", got)
	}
	if back.Versions("research") != 1 {
		t.Errorf("黑板内容丢了：%+v", back.Board)
	}
	if back.Pending == nil || back.Pending.ID != st.Pending.ID {
		t.Errorf("中断丢了：%+v", back.Pending)
	}

	// 读回来之后必须仍然**可用**：黑板能还原成真的黑板。
	b, err := back.RestoreBoard()
	if err != nil {
		t.Fatalf("还原黑板失败：%v", err)
	}
	if _, err := b.Get("research@1"); err != nil {
		t.Errorf("还原出来的黑板应按引用取值，却报：%v", err)
	}
}

// 同一份状态序列化两次，字节必须完全一致 —— 否则"恢复前后一致"无从断言。
func TestStateMarshalIsStable(t *testing.T) {
	st := newTestState(t)
	put(st,
		art("research", 1), art("outline", 1),
		art("design", 1, withBasedOn(schema.Basis{Key: "outline", Version: 1})),
	)

	first, err := st.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("两次序列化结果不一致：\n%s\n%s", first, second)
	}
}

// ⚠️ `Marshal` **不许改状态**。
//
// 名字承诺了它是纯函数。一个会顺手刷新 UpdatedAt 的 Marshal：
//
//	· 无法断言"两次序列化结果一致"（第二次的时间戳已经变了）
//	· 无法在多处安全调用
//
// 时间戳由谁改，就由谁显式调 `Stamp`（Runner 在保存前调一次）。
func TestMarshalDoesNotMutateState(t *testing.T) {
	st := newTestState(t)
	before := st.UpdatedAt

	if _, err := st.Marshal(); err != nil {
		t.Fatal(err)
	}
	if !st.UpdatedAt.Equal(before) {
		t.Errorf("Marshal 不该改 UpdatedAt：%v → %v", before, st.UpdatedAt)
	}
}

func TestStamp(t *testing.T) {
	st := newTestState(t)
	later := time.Now().UTC().Add(time.Hour)

	st.Stamp(later)
	if !st.UpdatedAt.Equal(later) {
		t.Errorf("Stamp 之后应当是 %v，实际 %v", later, st.UpdatedAt)
	}
}

// 写盘前自校验：坏状态**不许**变成磁盘上的字节。
//
// 理由：坏快照一旦落盘，就只有"另一个进程、很久以后、带着极少的上下文"
// 才能发现它。**能在源头挡住的问题，不要留给下游的读方。**
func TestMarshalRefusesInvalidState(t *testing.T) {
	st := newTestState(t)
	// 造一个版本有洞的黑板：Get("outline@1") 会取到版本号 5 的那一份。
	st.Board["outline"] = []schema.Artifact{art("outline", 5)}

	raw, err := st.Marshal()
	if err == nil {
		t.Fatalf("应当拒绝写盘，却写出来了：%s", raw)
	}
	if raw != nil {
		t.Errorf("被拒绝时不该返回任何字节，实际返回了 %d 字节", len(raw))
	}
	if !strings.Contains(err.Error(), "版本序列有洞或乱序") {
		t.Errorf("错误应说清是黑板版本序列的问题，实际：%v", err)
	}
}

// ─── 反序列化：错误必须分类 ─────────────────────────────────────────

func TestUnmarshalStateRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want ResumeErrorKind
	}{
		{"空的", "", ResumeCorrupt},
		{"截断的 JSON（模拟写到一半被杀）", `{"version":1,"run_id":"r",`, ResumeCorrupt},
		{"根本不是 JSON", "这不是 JSON", ResumeCorrupt},
		{"JSON 合法但缺字段", `{"version":1}`, ResumeCorrupt},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := UnmarshalState([]byte(c.raw))
			var re *ResumeError
			if !errors.As(err, &re) {
				t.Fatalf("应当是 *ResumeError，实际 %T：%v", err, err)
			}
			if re.Kind != c.want {
				t.Errorf("应当是 %s，实际 %s（%v）", c.want, re.Kind, err)
			}
		})
	}
}

// "截断的 JSON" 这一类的错误信息必须**指出可能的原因**。
//
// `json` 的原生错误只会说 "unexpected end of JSON input" ——
// 排查的人完全看不出"是上次写崩了"还是"这个文件本来就不是 JSON"，
// 而这两种情况的处置完全不同（前者要查原子写，后者要查是不是读错了文件）。
func TestUnmarshalCorruptErrorExplainsLikelyCause(t *testing.T) {
	_, err := UnmarshalState([]byte(`{"version":1,"run_id":"r",`))
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), "写盘被中断") {
		t.Errorf("错误信息应点出「可能写到一半被杀」，实际：%v", err)
	}
}

// 来自未来版本的快照 → from_future（**不是** corrupt）。
//
// 这个区分在滚动发布期间有实际后果：
//
//	corrupt     → 用户被要求"重新开始"
//	from_future → **重试到新实例**就好，用户什么都不用做
func TestUnmarshalStateFromFutureVersion(t *testing.T) {
	st := newTestState(t)
	st.Version = StateVersion + 3

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}

	_, err = UnmarshalState(raw)
	var re *ResumeError
	if !errors.As(err, &re) || re.Kind != ResumeFromFuture {
		t.Fatalf("应当得到 from_future，实际：%v", err)
	}
	if re.ID != st.RunID {
		t.Errorf("错误里应带上 run_id（方便定位是哪一轮），实际 %q", re.ID)
	}
	if !re.Kind.Retryable() {
		t.Error("from_future 必须可重试")
	}
	if !strings.Contains(err.Error(), "路由") {
		t.Errorf("应给出可执行的处置建议，实际：%v", err)
	}
}

// 版本判断必须在结构校验**之前**：一份来自未来的快照，
// 结构可能已经变了，用本版本的规则去校验它只会得到**误导性的**结论。
func TestUnmarshalChecksVersionBeforeStructure(t *testing.T) {
	st := newTestState(t)
	st.Version = StateVersion + 5
	st.Board = nil // 同时：在本版本的规则下结构也不完整

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}

	_, err = UnmarshalState(raw)
	var re *ResumeError
	if !errors.As(err, &re) || re.Kind != ResumeFromFuture {
		t.Fatalf("必须先报版本问题，实际：%v", err)
	}
	if strings.Contains(err.Error(), "黑板是空的") {
		t.Errorf("不该把版本问题报成结构问题：%v", err)
	}
}

// ─── Validate ───────────────────────────────────────────────────────

func TestStateValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*State)
		wantSub string
	}{
		{
			// ⚠️ 版本 0 现在是 **corrupt**（"这个字段没被写过"），不是 too_old。
			// 这条用例是那个 bug 修完之后的样子。
			"版本为 0（字段没被写过）",
			func(s *State) { s.Version = 0 },
			"版本字段是 0",
		},
		{"缺 run_id", func(s *State) { s.RunID = "" }, "缺少 run_id"},
		{"黑板为空", func(s *State) { s.Board = nil }, "黑板是空的"},
		{"缺 created_at", func(s *State) { s.CreatedAt = time.Time{} }, "缺少 created_at"},
		{"缺 updated_at", func(s *State) { s.UpdatedAt = time.Time{} }, "缺少 updated_at"},
		{
			"没有输入产物（但黑板上有别的东西）",
			func(s *State) {
				// 故意留一个别的 key —— 否则会先撞上"黑板是空的"，
				// 于是这条用例就测不到它真正想测的东西。
				delete(s.Board, KeyTopic)
				s.Board["outline"] = []schema.Artifact{art("outline", 1)}
			},
			"没有输入产物",
		},
		{
			"黑板版本序列有洞",
			func(s *State) { s.Board["outline"] = []schema.Artifact{art("outline", 3)} },
			"版本序列有洞或乱序",
		},
		{
			"信封上的 key 与所在位置不一致",
			func(s *State) { s.Board["outline"] = []schema.Artifact{art("design", 1)} },
			"与所在 key 不一致",
		},
		{
			"挂了一次不合法的中断",
			func(s *State) {
				bad := NewInterrupt(s.RunID, "", 1, KindConfirmArtifact, "问一句")
				s.Pending = &bad
			},
			"不合法的中断",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newTestState(t)
			c.mutate(st)

			// ⚠️ 用 CheckRestorable 而不是 Validate 来断言，
			// 因为它才是"读回来时的真实入口"（版本 → 结构）。
			err := st.CheckRestorable()
			if err == nil {
				t.Fatalf("应当被拒绝，却通过了")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误里应含 %q，实际：%v", c.wantSub, err)
			}
		})
	}

	if err := newTestState(t).CheckRestorable(); err != nil {
		t.Fatalf("基线快照应当通过，却报：%v", err)
	}
}

// ⚠️ 串台检查：挂在快照上的中断，必须属于**同一轮**。
//
// 后果具体得很：把 A 会话的中断挂到 B 会话的 checkpoint 上，
// 人的回答就会被应用到另一件事上 —— 而**没有任何一层会报错**。
func TestPendingMustBelongToTheSameRun(t *testing.T) {
	st := newTestState(t)
	other := NewInterrupt("run-OTHER", "outline", 1, KindConfirmArtifact, "确认大纲？")
	st.Pending = &other

	err := st.Validate()
	if err == nil {
		t.Fatal("跨轮的挂载必须被拒绝")
	}
	if !strings.Contains(err.Error(), "串台") {
		t.Errorf("错误里应点出「串台」，实际：%v", err)
	}
}

// 同一轮的中断则必须放行 —— 否则上面那条测试可能是"因为别的原因"过的。
func TestPendingFromSameRunIsAccepted(t *testing.T) {
	st := newTestState(t)
	p := NewInterrupt(st.RunID, "outline", 1, KindConfirmArtifact, "确认大纲？")
	st.Pending = &p

	if err := st.Validate(); err != nil {
		t.Errorf("同轮的中断应当放行，却报：%v", err)
	}
}

// ─── 深拷贝 ─────────────────────────────────────────────────────────

// 克隆必须是**深**的：改克隆不能影响原版。
//
// 这个坑本项目踩过三次（Report.Merged / render_slide / MergeLocal），
// 而快照比它们都危险 —— 它是**会被反复改写**的东西。
func TestCloneIsDeep(t *testing.T) {
	st := newTestState(t)
	p := NewInterrupt(st.RunID, "outline", 1, KindConfirmArtifact, "确认大纲？")
	p.Payload = json.RawMessage(`{"orig":true}`)
	p.Options = []string{"甲", "乙"}
	st.Pending = &p

	cp := st.Clone()

	// 改克隆的每一层
	cp.Board[KeyTopic][0].Payload[0] = 'X'
	cp.Board["新增"] = []schema.Artifact{art("新增", 1)}
	cp.Pending.Payload[0] = 'Y'
	cp.Pending.Options[0] = "改了"

	if string(st.Board[KeyTopic][0].Payload[:1]) == "X" {
		t.Error("改克隆的 payload 影响了原版 —— Payload 没被深拷贝")
	}
	if _, ok := st.Board["新增"]; ok {
		t.Error("给克隆加 key 影响了原版 —— map 没被深拷贝")
	}
	if string(st.Pending.Payload[:1]) == "Y" {
		t.Error("改克隆的 Pending.Payload 影响了原版")
	}
	if st.Pending.Options[0] == "改了" {
		t.Error("改克隆的 Pending.Options 影响了原版")
	}
}

// 克隆出来的必须仍然是一份**合法可用**的快照。
func TestCloneRemainsValid(t *testing.T) {
	st := newTestState(t)
	cp := st.Clone()

	if err := cp.CheckRestorable(); err != nil {
		t.Errorf("克隆应当仍然可恢复，却报：%v", err)
	}
	if _, err := cp.RestoreBoard(); err != nil {
		t.Errorf("克隆的黑板应当能还原，却报：%v", err)
	}
}

// ─── 与黑板交互 ─────────────────────────────────────────────────────

// RestoreBoard → 改黑板 → AdoptBoard，是一轮完整的读改写。
func TestAdoptAndRestoreBoardRoundTrip(t *testing.T) {
	st := newTestState(t)

	b, err := st.RestoreBoard()
	if err != nil {
		t.Fatalf("还原黑板失败：%v", err)
	}

	topicRef := schema.BasisOf(st.Board[KeyTopic][0])
	a, err := schema.NewArtifact("research", "research", schema.Research{
		Topic: "新能源汽车行业趋势",
	})
	if err != nil {
		t.Fatal(err)
	}
	a.BasedOn = []schema.Basis{topicRef}
	if _, err := b.Put(a); err != nil {
		t.Fatalf("写入产物失败：%v", err)
	}

	st.AdoptBoard(b)

	if got := st.Versions("research"); got != 1 {
		t.Fatalf("收进快照后应有 1 版 research，实际 %d", got)
	}
	if v := st.CanSkip("research"); !v.Skip {
		t.Errorf("刚写进去的 research 应当可以跳过，却被拒：%s", v.Reason)
	}

	// ⚠️ 关键：收进来的是**快照**，不是那块黑板本身。
	// 之后继续改那块黑板，不该影响快照（否则"保存"就没有确定的时间点了）。
	if _, err := b.Put(puttable("outline", schema.KindOutline)); err != nil {
		t.Fatal(err)
	}
	if st.Versions("outline") != 0 {
		t.Error("改黑板影响了已收进快照的数据 —— AdoptBoard 没有拷贝")
	}
}

// 还原出来的黑板必须是**独立的**：改它不影响快照，反之亦然。
func TestRestoreBoardIsIndependent(t *testing.T) {
	st := newTestState(t)

	b1, err := st.RestoreBoard()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b1.Put(puttable("outline", schema.KindOutline)); err != nil {
		t.Fatal(err)
	}

	if st.Versions("outline") != 0 {
		t.Error("改还原出来的黑板影响了快照")
	}

	// 再还原一次，应该是干净的
	b2, err := st.RestoreBoard()
	if err != nil {
		t.Fatal(err)
	}
	if b2.Versions("outline") != 0 {
		t.Error("两次还原共享了底层数据 —— LoadSnapshot 没有深拷贝")
	}
}

// Keys 必须**字典序** —— map 的遍历顺序是随机的，
// 不排序会让"同一份状态打印两次却不一样"，于是无法 diff、无法回归（L1-6）。
//
// ⚠️ 注意这里期望的是纯字典序（topic 排在最后），不是"topic 在最前"。
//
//	写这条断言时我第一版就写错了 —— 因为顺手把"逻辑上的顺序"
//	（输入在最前）当成了"字典序"。**测试的期望值也要推导，不能凭直觉填。**
func TestKeysIsSorted(t *testing.T) {
	st := newTestState(t)
	put(st, art("render", 1), art("design", 1), art("outline", 1))

	got := st.Keys()
	want := []string{"design", "outline", "render", KeyTopic}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Keys 应按字典序：得到 %v，期望 %v", got, want)
	}
}

// Latest 的签名要与 schema.Latest 逐字一致 ——
// 否则就必须再包一层函数，而那一层是纯噪音。
func TestLatestMatchesSchemaContract(t *testing.T) {
	var _ schema.Latest = newTestState(t).Latest

	st := newTestState(t)
	put(st, art("outline", 1), art("outline", 2))

	a, ok := st.Latest("outline")
	if !ok || a.Version != 2 {
		t.Errorf("Latest 应返回最新版（2），实际 %+v ok=%v", a, ok)
	}
	if _, ok := st.Latest("不存在"); ok {
		t.Error("不存在的 key 应当返回 false")
	}
}

// ⚠️ Latest 返回的必须是**副本**：调用方改了它，不该污染快照。
func TestLatestReturnsCopy(t *testing.T) {
	st := newTestState(t)
	a, ok := st.Latest(KeyTopic)
	if !ok {
		t.Fatal("应当能取到 topic")
	}

	a.Payload[0] = 'X'
	again, _ := st.Latest(KeyTopic)
	if string(again.Payload[:1]) == "X" {
		t.Error("Latest 返回的不是副本")
	}
}

// State 与真实黑板对同一份数据必须给出**同样的答案**。
//
// 这条测试防的是"两套实现各自漂移"：快照只是一个表示，
// 它不该改变黑板的行为。
func TestStateAgreesWithRealBlackboard(t *testing.T) {
	st := newTestState(t)
	b, err := st.RestoreBoard()
	if err != nil {
		t.Fatal(err)
	}

	for _, k := range st.Keys() {
		if st.Versions(k) != b.Versions(k) {
			t.Errorf("%s 的版本数不一致：快照 %d，黑板 %d", k, st.Versions(k), b.Versions(k))
		}
	}
	if _, err := b.Get("不存在的@9"); err == nil {
		t.Error("黑板对不存在的引用应当报错")
	} else if !errors.Is(err, state.ErrArtifactNotFound) {
		t.Errorf("应当是 ErrArtifactNotFound，实际：%v", err)
	}
}

func ptr[T any](v T) *T { return &v }
