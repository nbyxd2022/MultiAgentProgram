package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"multiagent/internal/schema"
)

func mustArtifact(t *testing.T, key, producer string, r schema.Research) schema.Artifact {
	t.Helper()
	a, err := schema.NewArtifact(key, producer, r)
	if err != nil {
		t.Fatalf("构造产物失败: %v", err)
	}
	return a
}

func research(topic string) schema.Research {
	return schema.Research{
		Topic:    topic,
		Queries:  []string{"q1"},
		Findings: []schema.Finding{{Claim: "结论", Source: "web_search#1"}},
	}
}

// ─── 追加，绝不覆盖 ─────────────────────────────────────────────────

func TestPutAppendsVersionsInsteadOfOverwriting(t *testing.T) {
	b := NewBlackboard()

	first, err := b.Put(mustArtifact(t, "research", "research", research("第一版")))
	if err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	second, err := b.Put(mustArtifact(t, "research", "research", research("第二版")))
	if err != nil {
		t.Fatalf("二次提交失败: %v", err)
	}

	if first.Version != 1 || second.Version != 2 {
		t.Fatalf("版本应连续递增，实际 %d / %d", first.Version, second.Version)
	}
	if b.Versions("research") != 2 {
		t.Fatalf("两次提交必须保留两版，实际 %d 版", b.Versions("research"))
	}

	// ★ 这是"引用不可变"的地基：旧版本**没有被改写**
	old, err := b.Get("research@1")
	if err != nil {
		t.Fatalf("旧版本必须仍然可取: %v", err)
	}
	got, _ := schema.Decode[schema.Research](old)
	if got.Topic != "第一版" {
		t.Errorf("旧版本被覆盖了！期望「第一版」，实际 %q", got.Topic)
	}
}

func TestPutRejectsCallerProvidedVersion(t *testing.T) {
	b := NewBlackboard()

	a := mustArtifact(t, "k", "research", research("x"))
	a.Version = 7 // 调用方自己拍了一个版本号

	if _, err := b.Put(a); err == nil {
		t.Fatal("版本由黑板分配，调用方指定的版本必须被拒绝（而不是静默忽略）")
	}
	if b.Versions("k") != 0 {
		t.Error("被拒绝的提交不应占用任何版本")
	}
}

func TestPutNormalizesKey(t *testing.T) {
	b := NewBlackboard()

	if _, err := b.Put(mustArtifact(t, "  research  ", " research ", research("x"))); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	// 规范化之后，" k " 和 "k" 必须是同一个东西
	if b.Versions("research") != 1 {
		t.Fatal("key 未被规范化：会出现「明明提交过却找不到」的幽灵故障")
	}
	if _, err := b.Get(" research "); err != nil {
		t.Errorf("查询也应容忍首尾空白: %v", err)
	}
}

func TestPutValidatesBeforeStoring(t *testing.T) {
	b := NewBlackboard()

	bad := mustArtifact(t, "k", "research", research("x"))
	bad.Payload = nil // 脏数据

	if _, err := b.Put(bad); err == nil {
		t.Fatal("脏数据必须被挡在黑板之外")
	}
	if b.Versions("k") != 0 {
		t.Error("校验失败必须零副作用：不能白白推进版本号（否则后续引用全部错位）")
	}
}

// ─── 引用：带版本 vs 最新 ───────────────────────────────────────────

func TestGetLatestWhenNoVersionSpecified(t *testing.T) {
	b := NewBlackboard()
	for _, topic := range []string{"一", "二", "三"} {
		if _, err := b.Put(mustArtifact(t, "outline", "outline", research(topic))); err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}

	a, err := b.Get("outline")
	if err != nil {
		t.Fatalf("取最新版失败: %v", err)
	}
	if a.Version != 3 {
		t.Errorf("不带版号应取最新版（3），实际 %d", a.Version)
	}
}

func TestGetUnknownKeyOrVersion(t *testing.T) {
	b := NewBlackboard()
	if _, err := b.Put(mustArtifact(t, "outline", "outline", research("x"))); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	if _, err := b.Get("design"); !errors.Is(err, ErrArtifactNotFound) {
		t.Errorf("未知 key 应返回 ErrArtifactNotFound，实际 %v", err)
	}
	if _, err := b.Get("outline@9"); !errors.Is(err, ErrArtifactNotFound) {
		t.Errorf("越界版本应返回 ErrArtifactNotFound，实际 %v", err)
	}
	if _, err := b.Get("outline@1"); err != nil {
		t.Errorf("已存在的版本应可取: %v", err)
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		ref         string
		wantKey     string
		wantVersion int
		wantErr     bool
	}{
		{"outline", "outline", 0, false},
		{"outline@2", "outline", 2, false},
		{" outline@2 ", "outline", 2, false},
		{"", "", 0, true},
		{"   ", "", 0, true},
		{"@2", "", 0, true},
		{"outline@0", "", 0, true},
		{"outline@-1", "", 0, true},
		{"outline@abc", "", 0, true},
	}

	for _, c := range cases {
		key, version, err := ParseRef(c.ref)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseRef(%q) 应报错", c.ref)
			} else if !errors.Is(err, ErrBadRef) {
				t.Errorf("ParseRef(%q) 应返回 ErrBadRef，实际 %v", c.ref, err)
			}
			continue
		}
		if err != nil || key != c.wantKey || version != c.wantVersion {
			t.Errorf("ParseRef(%q) = (%q, %d, %v)，期望 (%q, %d, nil)",
				c.ref, key, version, err, c.wantKey, c.wantVersion)
		}
	}
}

func TestGetRejectsBadRef(t *testing.T) {
	b := NewBlackboard()
	if _, err := b.Get("outline@abc"); !errors.Is(err, ErrBadRef) {
		t.Errorf("非法引用应返回 ErrBadRef，实际 %v", err)
	}
}

// ─── 深拷贝：黑板是全项目唯一真正的共享可变状态 ─────────────────────

func TestBlackboardDoesNotAliasCallerPayload(t *testing.T) {
	b := NewBlackboard()

	a := mustArtifact(t, "research", "research", research("原始内容"))
	if _, err := b.Put(a); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	// 提交之后，调用方又改了它手里的那份
	for i := range a.Payload {
		a.Payload[i] = 'X'
	}

	got, err := b.Get("research")
	if err != nil {
		t.Fatalf("取回失败: %v", err)
	}
	if strings.Contains(string(got.Payload), "XXXXXXXX") {
		t.Fatal("黑板里的 payload 被调用方改动了 —— Put 没有深拷贝")
	}
}

func TestBlackboardDoesNotAliasReturnedPayload(t *testing.T) {
	b := NewBlackboard()
	if _, err := b.Put(mustArtifact(t, "research", "research", research("原始内容"))); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	got, _ := b.Get("research")
	for i := range got.Payload { // 拿到手之后顺手改
		got.Payload[i] = 'Y'
	}

	again, _ := b.Get("research")
	if strings.Contains(string(again.Payload), "YYYYYYYY") {
		t.Fatal("改从黑板取回的副本，污染了黑板里的原件 —— Get 没有深拷贝")
	}
}

// ─── 输出稳定 ───────────────────────────────────────────────────────

func TestKeysAreSorted(t *testing.T) {
	b := NewBlackboard()
	for _, k := range []string{"render", "outline", "design", "research"} {
		if _, err := b.Put(mustArtifact(t, k, k, research(k))); err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}

	got := strings.Join(b.Keys(), ",")
	if want := "design,outline,render,research"; got != want {
		t.Errorf("Keys() = %q, want %q（map 遍历顺序随机，必须排序）", got, want)
	}
}

// ─── 并发：版本分配不能撞车 ─────────────────────────────────────────

func TestBlackboardConcurrentPutKeepsEveryVersion(t *testing.T) {
	b := NewBlackboard()
	const n = 50

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := schema.NewArtifact("shared", "research", research(fmt.Sprint(i)))
			if err != nil {
				t.Errorf("构造失败: %v", err)
				return
			}
			if _, err := b.Put(a); err != nil {
				t.Errorf("并发提交失败: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if got := b.Versions("shared"); got != n {
		t.Fatalf("并发 Put 一版都不能丢：期望 %d 版，实际 %d", n, got)
	}

	// 版本号必须是 1..n 且**互不重复** —— 撞车意味着有两次提交指向了同一版
	seen := make(map[int]bool, n)
	for v := 1; v <= n; v++ {
		a, err := b.Get(fmt.Sprintf("shared@%d", v))
		if err != nil {
			t.Fatalf("版本 %d 缺失: %v", v, err)
		}
		if seen[a.Version] {
			t.Fatalf("版本号撞车: %d", a.Version)
		}
		seen[a.Version] = true
	}
}

// ─── 与协议层的配合 ─────────────────────────────────────────────────

func TestStoredArtifactRoundTripsThroughHandoff(t *testing.T) {
	b := NewBlackboard()
	_, err := b.Put(mustArtifact(t, "outline", "outline", research("新能源汽车")))
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	latest, _ := b.Get("outline")
	handoff := schema.HandoffOf(latest) // Agent 之间真正传的东西

	if handoff.Ref != "outline@1" {
		t.Errorf("handoff 引用应带版本，实际 %q", handoff.Ref)
	}

	// 下游拿着 ref 就能取回原件
	back, err := b.Get(handoff.Ref)
	if err != nil {
		t.Fatalf("按 handoff 引用取回失败: %v", err)
	}
	if _, err := schema.Decode[schema.Research](back); err != nil {
		t.Fatalf("取回的产物无法解码: %v", err)
	}

	// 而 handoff 本身**不含正文**（原则四）
	raw, _ := json.Marshal(handoff)
	if strings.Contains(string(raw), "新能源汽车") && strings.Contains(string(raw), "结论") {
		t.Errorf("handoff 不应携带正文: %s", raw)
	}
}

// ─── 持久化：Snapshot / LoadSnapshot ────────────────────────────────

// 直接构造带指定版本号的产物 —— "存储"在正常路径下不让调用方填版本，
// 但测试需要精确控制它，才能造出"版本序列有洞"这类病态输入。
func artifactAt(t *testing.T, key string, version int) schema.Artifact {
	t.Helper()
	a := mustArtifact(t, key, key, research("主题"))
	a.Version = version
	return a
}

func TestSnapshotAndLoadRoundTrip(t *testing.T) {
	b := NewBlackboard()
	for i := 1; i <= 3; i++ {
		if _, err := b.Put(mustArtifact(t, "research", "research", research(fmt.Sprintf("第%d版", i)))); err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}
	if _, err := b.Put(mustArtifact(t, "outline", "outline", research("大纲"))); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	snap := b.Snapshot()
	back, err := LoadSnapshot(snap)
	if err != nil {
		t.Fatalf("还原失败: %v", err)
	}

	if got := back.Versions("research"); got != 3 {
		t.Errorf("research 应有 3 版，实际 %d", got)
	}
	if got := back.Versions("outline"); got != 1 {
		t.Errorf("outline 应有 1 版，实际 %d", got)
	}

	// ⚠️ 这是"版本序列必须连续"这条校验存在的**全部理由**：
	// 带版本取值靠下标，序列一旦错位，取值就会静默命中另一版。
	a, err := back.Get("research@2")
	if err != nil {
		t.Fatalf("按版本取值失败: %v", err)
	}
	var r schema.Research
	if err := json.Unmarshal(a.Payload, &r); err != nil {
		t.Fatal(err)
	}
	if r.Topic != "第2版" {
		t.Errorf("research@2 应当取到第二版，实际取到了 %q —— 版本序列错位了", r.Topic)
	}
}

// 快照必须是**深拷贝**：调用方改了它，不该影响黑板。
func TestSnapshotIsDeepCopy(t *testing.T) {
	b := NewBlackboard()
	if _, err := b.Put(mustArtifact(t, "research", "research", research("原始"))); err != nil {
		t.Fatal(err)
	}

	snap := b.Snapshot()
	snap["research"][0].Payload[0] = 'X'
	snap["新增"] = []schema.Artifact{artifactAt(t, "新增", 1)}

	if string(b.Snapshot()["research"][0].Payload[:1]) == "X" {
		t.Error("改快照影响了黑板 —— Snapshot 没有深拷贝")
	}
	if b.Versions("新增") != 0 {
		t.Error("给快照加 key 影响了黑板")
	}
}

// 还原出来的黑板必须是**独立的**：改它不该影响被还原的那份快照。
func TestLoadSnapshotIsDeepCopy(t *testing.T) {
	snap := map[string][]schema.Artifact{"research": {artifactAt(t, "research", 1)}}

	back, err := LoadSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := back.Put(mustArtifact(t, "research", "research", research("新的"))); err != nil {
		t.Fatal(err)
	}

	if len(snap["research"]) != 1 {
		t.Errorf("改还原出来的黑板影响了输入的快照：现在有 %d 版", len(snap["research"]))
	}
}

// ⚠️ 版本序列有洞必须被拒绝。
//
// 放行的后果是**静默取错版本**（见 TestSnapshotAndLoadRoundTrip 的注释）——
// 而"取错一版"比"读不出来"危险得多：后者让任务失败，前者让任务**成功地做错**。
func TestLoadSnapshotRejectsVersionGap(t *testing.T) {
	cases := []struct {
		name    string
		snap    map[string][]schema.Artifact
		wantSub string
	}{
		{
			"从 2 开始（缺第 1 版）",
			map[string][]schema.Artifact{"outline": {artifactAt(t, "outline", 2)}},
			"版本序列有洞或乱序",
		},
		{
			"中间缺一版",
			map[string][]schema.Artifact{"outline": {
				artifactAt(t, "outline", 1), artifactAt(t, "outline", 3),
			}},
			"版本序列有洞或乱序",
		},
		{
			"乱序",
			map[string][]schema.Artifact{"outline": {
				artifactAt(t, "outline", 2), artifactAt(t, "outline", 1),
			}},
			"版本序列有洞或乱序",
		},
		{
			"版本号是 0",
			map[string][]schema.Artifact{"outline": {artifactAt(t, "outline", 0)}},
			"版本序列有洞或乱序",
		},
		{
			"信封上的 key 与所在位置不一致",
			map[string][]schema.Artifact{"outline": {artifactAt(t, "design", 1)}},
			"与所在 key 不一致",
		},
		{
			"存在空 key",
			map[string][]schema.Artifact{"  ": {artifactAt(t, "outline", 1)}},
			"空 key",
		},
		{
			"版本列表为空",
			map[string][]schema.Artifact{"outline": {}},
			"版本列表为空",
		},
		{
			"其中一版自己不合法（kind 乱写）",
			map[string][]schema.Artifact{"outline": {func() schema.Artifact {
				a := artifactAt(t, "outline", 1)
				a.Kind = "不存在的种类"
				return a
			}()}},
			"不合法",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			back, err := LoadSnapshot(c.snap)
			if err == nil {
				t.Fatalf("应当被拒绝，却还原成功了（%d 个 key）", len(back.byKey))
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误里应含 %q，实际: %v", c.wantSub, err)
			}
		})
	}
}

// 一条**业务上说得通**的事实：快照回环之后，
// "同一个引用指向同一版内容"这条性质必须仍然成立。
//
// 这条与 L2-1（新鲜度）是配套的：新鲜度靠比较版本号来判断，
// 版本号一旦在持久化过程中错位，新鲜度检查就会**给出错误的答案**。
func TestLoadSnapshotPreservesRefSemantics(t *testing.T) {
	b := NewBlackboard()
	for i := 1; i <= 2; i++ {
		if _, err := b.Put(mustArtifact(t, "outline", "outline", research(fmt.Sprintf("第%d版", i)))); err != nil {
			t.Fatal(err)
		}
	}
	// 记下一个"当时看到的"引用
	first, err := b.Get("outline@1")
	if err != nil {
		t.Fatal(err)
	}
	ref := schema.RefOf(first)

	back, err := LoadSnapshot(b.Snapshot())
	if err != nil {
		t.Fatal(err)
	}

	again, err := back.Get(ref)
	if err != nil {
		t.Fatalf("回环后按 %s 取值失败: %v", ref, err)
	}
	if string(again.Payload) != string(first.Payload) {
		t.Error("回环后同一个引用指向了不同的内容 —— 引用不再不可变")
	}
}
