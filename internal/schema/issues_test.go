package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

// 为什么这个文件必须先有测试？
//
//	issues.go 全是纯逻辑（没有 I/O、没有网络），是**最适合写单测**的一类代码：
//	给定输入必然得到确定输出。而它又是"停止条件""best-so-far"的判断依据——
//	一旦算错，返工循环要么早停（交付坏产物）、要么永不停（烧钱）。
//	所以这 60 行测试，比 L1 里任何一段代码都值得写。

// ─── 严重度权重 ─────────────────────────────────────────────────────

func TestLevelWeightAndValid(t *testing.T) {
	cases := []struct {
		level Level
		want  int
		valid bool
	}{
		{LevelBlocker, 3, true},
		{LevelMajor, 2, true},
		{LevelSuggestion, 1, true},
		{Level("bloker"), 0, false}, // 拼错的级别：权重 0 且不合法，入口必须拦住
		{Level(""), 0, false},
	}

	for _, c := range cases {
		if got := c.level.Weight(); got != c.want {
			t.Errorf("Level(%q).Weight() = %d, want %d", c.level, got, c.want)
		}
		if got := c.level.Valid(); got != c.valid {
			t.Errorf("Level(%q).Valid() = %v, want %v", c.level, got, c.valid)
		}
	}
}

// ─── 评分与计数 ─────────────────────────────────────────────────────

func TestReportScoreAndCount(t *testing.T) {
	rep := Report{Issues: []Issue{
		{Level: LevelBlocker},
		{Level: LevelMajor}, {Level: LevelMajor},
		{Level: LevelSuggestion}, {Level: LevelSuggestion}, {Level: LevelSuggestion},
	}}

	// 3*1 + 2*2 + 1*3 = 10
	if got, want := rep.Score(), 10; got != want {
		t.Errorf("Score() = %d, want %d", got, want)
	}
	if got, want := rep.Count(LevelBlocker), 1; got != want {
		t.Errorf("Count(blocker) = %d, want %d", got, want)
	}
	if got, want := rep.Count(LevelMajor), 2; got != want {
		t.Errorf("Count(major) = %d, want %d", got, want)
	}
	if got, want := rep.HardCount(), 3; got != want {
		t.Errorf("HardCount() = %d, want %d", got, want)
	}
	if !rep.HasHardIssue() {
		t.Error("有 blocker/major 时必须报告 HasHardIssue() = true")
	}

	empty := Report{}
	if empty.Score() != 0 {
		t.Errorf("空报告 Score() 应为 0，实际 %d", empty.Score())
	}
	if empty.HasHardIssue() {
		t.Error("空报告不应有硬问题")
	}
}

// ─── BetterThan：best-so-far 的比较规则 ─────────────────────────────

func TestReportBetterThan(t *testing.T) {
	// 3 个 blocker：score=9，hard=3
	rBlocker3 := Report{Issues: []Issue{{Level: LevelBlocker}, {Level: LevelBlocker}, {Level: LevelBlocker}}}
	// 1 个 major：score=2，hard=1
	rMajor1 := Report{Issues: []Issue{{Level: LevelMajor}}}
	// 1 blocker + 3 suggestion：score=6，hard=1
	rMixedA := Report{Issues: []Issue{
		{Level: LevelBlocker},
		{Level: LevelSuggestion}, {Level: LevelSuggestion}, {Level: LevelSuggestion},
	}}
	// 3 个 major：score=6，hard=3（与 rMixedA 同分，但硬问题更多）
	rMajor3 := Report{Issues: []Issue{{Level: LevelMajor}, {Level: LevelMajor}, {Level: LevelMajor}}}
	// 1 major + 1 suggestion：score=3，hard=1
	rMixedB := Report{Issues: []Issue{{Level: LevelMajor}, {Level: LevelSuggestion}}}

	cases := []struct {
		name       string
		r, other   Report
		wantBetter bool
	}{
		{"总分更低者更优", rMajor1, rBlocker3, true},
		{"总分更高者更差", rBlocker3, rMajor1, false},
		{"同分时硬问题更少者更优", rMixedA, rMajor3, true},
		{"同分时硬问题更多者更差", rMajor3, rMixedA, false},
		{"完全相同时不更新（保留更早版本）", rMixedB, rMixedB, false},
	}

	for _, c := range cases {
		if got := c.r.BetterThan(c.other); got != c.wantBetter {
			t.Errorf("%s: BetterThan() = %v, want %v", c.name, got, c.wantBetter)
		}
	}
}

// ─── 排序：输出必须稳定可复现 ───────────────────────────────────────

func TestReportSorted(t *testing.T) {
	rep := Report{Issues: []Issue{
		{Level: LevelSuggestion, Field: "b"},
		{Level: LevelBlocker, Field: "z"},
		{Level: LevelMajor, Field: "a"},
		{Level: LevelBlocker, Field: "a"},
	}}

	got := []string{}
	for _, it := range rep.Sorted() {
		got = append(got, it.Field)
	}
	want := []string{"a", "z", "a", "b"} // blocker(a) → blocker(z) → major(a) → suggestion(b)

	if len(got) != len(want) {
		t.Fatalf("Sorted() 长度 = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Sorted() = %v, want %v", got, want)
		}
	}

	// Sorted 必须返回副本：不能把调用方的数据顺序改掉
	if rep.Issues[0].Field != "b" || rep.Issues[0].Level != LevelSuggestion {
		t.Errorf("Sorted() 修改了原数据：%+v", rep.Issues[0])
	}
}

// ─── 合并 ───────────────────────────────────────────────────────────

func TestReportMerged(t *testing.T) {
	hard := Report{Round: 2, Subject: "slide-1", Issues: []Issue{
		{Level: LevelBlocker, Field: "title", Source: SourceRule},
	}}
	soft := Report{Round: 2, Issues: []Issue{
		{Level: LevelSuggestion, Field: "bullets[0]", Source: SourceCritic},
	}}

	merged := hard.Merged(soft)

	if len(merged.Issues) != 2 {
		t.Fatalf("合并后应有 2 条问题，实际 %d", len(merged.Issues))
	}
	if merged.Subject != "slide-1" || merged.Round != 2 {
		t.Errorf("合并后上下文丢失：Subject=%q Round=%d", merged.Subject, merged.Round)
	}

	// 关键：合并必须是深拷贝，改合并结果不能影响原报告
	merged.Issues[0].Level = LevelSuggestion
	if hard.Issues[0].Level != LevelBlocker {
		t.Error("Merged() 与原报告共享了底层数组，产生了幽灵修改")
	}
}

// ─── best-so-far：返工震荡场景 ──────────────────────────────────────

func TestBestKeepsBestSoFar(t *testing.T) {
	var best Best[string] // 零值即可用：Has() == false

	if best.Has() {
		t.Fatal("零值 Best 不应有内容")
	}

	// 第 1 轮：3 个致命问题，score=9 → 首个版本，必须接受
	r1 := Report{Round: 1, Issues: []Issue{
		{Level: LevelBlocker}, {Level: LevelBlocker}, {Level: LevelBlocker},
	}}
	if !best.Observe(1, "v1", r1) {
		t.Fatal("首个版本应被接受")
	}

	// 第 2 轮：1 个严重问题，score=2 → 更优，必须接受
	r2 := Report{Round: 2, Issues: []Issue{{Level: LevelMajor}}}
	if !best.Observe(2, "v2", r2) {
		t.Fatal("更优版本应被接受")
	}

	// 第 3 轮：震荡回 3 个严重问题，score=6 → 更差，必须拒绝
	r3 := Report{Round: 3, Issues: []Issue{
		{Level: LevelMajor}, {Level: LevelMajor}, {Level: LevelMajor},
	}}
	if best.Observe(3, "v3", r3) {
		t.Fatal("更差的版本不应覆盖 best-so-far")
	}

	if best.Payload != "v2" || best.Round != 2 {
		t.Fatalf("best 应停在第 2 轮 v2，实际 %q 第 %d 轮", best.Payload, best.Round)
	}
	if !best.Has() {
		t.Fatal("记录过版本后 Has() 应为 true")
	}
}

// ─── 序列化：要能塞给模型，也要能被解析回来 ─────────────────────────

func TestReportJSONRoundTrip(t *testing.T) {
	orig := Report{Round: 1, Subject: "out/slide-x.html", Issues: []Issue{
		{Level: LevelMajor, Field: "bullets", Message: "要点 7 条，超过上限 5 条", Fix: "删减到 3-5 条", Source: SourceRule},
	}}

	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	var back Report
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	if back.Round != orig.Round || back.Subject != orig.Subject {
		t.Errorf("上下文字段未保持：%+v", back)
	}
	if len(back.Issues) != 1 {
		t.Fatalf("问题条数未保持：%d", len(back.Issues))
	}
	if back.Issues[0].Fix != orig.Issues[0].Fix {
		t.Errorf("Fix 字段未保持（模型要靠它知道怎么改）：%q", back.Issues[0].Fix)
	}

	// 序列化输出应带预期的字段名，否则模型看到的协议会不一致
	if s := string(raw); !strings.Contains(s, `"fix"`) || !strings.Contains(s, `"level"`) {
		t.Errorf("JSON 字段名不符合协议：%s", s)
	}
}

// ─── L1 改进：评审"不完整"是一等状态 ────────────────────────────────
//
// 这一组用例全是 **L1-3 真 bug 的回归测试**。
// bug 的原貌：Critic 评审失败 → 只返回 error → 调用方打印一行日志继续 →
// 报告里恰好没有硬问题 → 被判成"可以交付" → 交付了一个从未做过语义评审的产物。

func TestReportDeliverableRequiresCompleteReview(t *testing.T) {
	if !(Report{}).Deliverable() {
		t.Error("没有问题且评审完整 → 应当可交付")
	}

	partial := Report{Incomplete: true, IncompleteReason: "critic 超时"}
	if partial.HasHardIssue() {
		t.Fatal("本用例的前提是硬问题为 0，否则测不到「没做检查」这条路径")
	}
	if partial.Deliverable() {
		t.Fatal("评审不完整时绝不能判成可交付")
	}

	if (Report{Issues: []Issue{{Level: LevelMajor}}}).Deliverable() {
		t.Error("还有致命/严重问题时不可交付")
	}
}

func TestReportBetterThanPrefersCompleteReview(t *testing.T) {
	complete := Report{}
	incomplete := Report{Incomplete: true}

	if incomplete.BetterThan(complete) {
		t.Error(`不能拿"没测出来"去替换"测过且没问题"`)
	}
	if !complete.BetterThan(incomplete) {
		t.Error("分数相同时，评审完整的那一份更优")
	}
}

func TestReportMergedPropagatesIncomplete(t *testing.T) {
	hard := Report{Round: 1, Subject: "s"}
	soft := Report{Incomplete: true, IncompleteReason: "critic 超时"}

	merged := hard.Merged(soft)

	if !merged.Incomplete {
		t.Fatal("只要有一个来源不完整，合并结果就必须不完整")
	}
	if !strings.Contains(merged.IncompleteReason, "critic 超时") {
		t.Errorf("不完整的原因未被保留：%q", merged.IncompleteReason)
	}
	if merged.Deliverable() {
		t.Error("合并后的报告不可交付")
	}
}

func TestReportStringShowsIncomplete(t *testing.T) {
	if got := (Report{Incomplete: true}).String(); !strings.Contains(got, "评审不完整") {
		t.Errorf(`摘要里必须显眼地标出"评审不完整"，实际：%s`, got)
	}
}
