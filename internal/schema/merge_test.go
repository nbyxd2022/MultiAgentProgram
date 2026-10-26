package schema

import (
	"strings"
	"testing"
)

func baseSlide() Slide {
	return Slide{
		Title: "新能源汽车行业趋势",
		Bullets: []string{
			"市场规模持续扩大，渗透率快速提升。",
			"产业链成本下降，电池价格进入下行通道。",
			"政策支持力度加大，补贴与路权双重驱动。",
			"充电基础设施加速铺设，缓解里程焦虑。",
			"出口成为新的增长极，海外销量翻倍增长。",
		},
	}
}

// ─── 范围判定 ───────────────────────────────────────────────────────

func TestLocalTargetOnlyPicksHardLocalizableIssues(t *testing.T) {
	rep := Report{Issues: []Issue{
		{Level: LevelMajor, Field: "bullets[2]"},      // ✅ 硬 + 可定位
		{Level: LevelBlocker, Field: "title"},         // ✅ 硬 + 可定位
		{Level: LevelSuggestion, Field: "bullets[0]"}, // ❌ 建议级：不值得为它返工
		{Level: LevelMajor, Field: "bullets"},         // ❌ 集合级：没有局部解
	}}

	scope := rep.LocalTarget()

	if !scope.Title {
		t.Error("title 应落在范围内")
	}
	if !scope.BulletIndex[2] {
		t.Error("bullets[2] 应落在范围内")
	}
	if scope.BulletIndex[0] {
		t.Error("建议级问题不应触发返工")
	}
	if scope.BulletIndex[3] || len(scope.BulletIndex) != 1 {
		t.Errorf("范围异常：%v", scope.Fields())
	}
}

func TestNeedsFullRewriteWhenIssueIsCollectionLevel(t *testing.T) {
	local := Report{Issues: []Issue{{Level: LevelMajor, Field: "bullets[1]"}}}
	if local.NeedsFullRewrite() {
		t.Error("只有可定位问题时，不应要求整体重写")
	}

	global := Report{Issues: []Issue{
		{Level: LevelMajor, Field: "bullets[1]"},
		{Level: LevelMajor, Field: "bullets"},
	}}
	if !global.NeedsFullRewrite() {
		t.Error("存在集合级硬问题时，必须整体重写")
	}

	// 空 field（例如"整页跑题"）同样属于无法精确定位
	if !(Report{Issues: []Issue{{Level: LevelMajor, Field: ""}}}).NeedsFullRewrite() {
		t.Error("空 field 无法精确定位，应要求整体重写")
	}

	// 建议级的集合问题不该触发整体重写
	if (Report{Issues: []Issue{{Level: LevelSuggestion, Field: "bullets"}}}).NeedsFullRewrite() {
		t.Error("建议级问题不应触发整体重写")
	}
}

func TestFixScopeFieldsSortedNumerically(t *testing.T) {
	scope := FixScope{
		Title:       true,
		BulletIndex: map[int]bool{10: true, 2: true, 1: true},
	}

	got := strings.Join(scope.Fields(), ",")
	want := "title,bullets[1],bullets[2],bullets[10]" // 数值序，不是字典序

	if got != want {
		t.Errorf("Fields() = %q, want %q", got, want)
	}
}

// ─── 合并：只采纳被点名的字段 ────────────────────────────────────────

func TestMergeLocalAcceptsAllowedAndRevertsOthers(t *testing.T) {
	prev := baseSlide()
	next := baseSlide()

	next.Bullets[0] = "结论：未来三年仍是增量市场，但增速将回落到 20%。" // 被点名 → 采纳
	next.Bullets[4] = "出口增速高达 300%。"               // 未被点名 → 回退

	merged, violations := MergeLocal(prev, next, FixScope{BulletIndex: map[int]bool{0: true}})

	if merged.Bullets[0] != next.Bullets[0] {
		t.Errorf("被点名的字段应被采纳，实际 %q", merged.Bullets[0])
	}
	if merged.Bullets[4] != prev.Bullets[4] {
		t.Errorf("未被点名的字段必须回退，实际 %q", merged.Bullets[4])
	}
	if len(violations) != 1 || !strings.Contains(violations[0], "bullets[4]") {
		t.Errorf("应记录 1 条越界（bullets[4]），实际：%v", violations)
	}
}

func TestMergeLocalAcceptsTitleWhenInScope(t *testing.T) {
	prev := baseSlide()
	next := baseSlide()
	next.Title = "2026 新能源汽车趋势"

	merged, violations := MergeLocal(prev, next, FixScope{Title: true})

	if merged.Title != "2026 新能源汽车趋势" {
		t.Errorf("title 在范围内时应被采纳，实际 %q", merged.Title)
	}
	if len(violations) != 0 {
		t.Errorf("不应有越界，实际：%v", violations)
	}
}

func TestMergeLocalRevertsTitleWhenNotInScope(t *testing.T) {
	prev := baseSlide()
	next := baseSlide()
	next.Title = "我自己顺手改的标题"

	merged, violations := MergeLocal(prev, next, FixScope{})

	if merged.Title != prev.Title {
		t.Errorf("title 不在范围内必须回退，实际 %q", merged.Title)
	}
	if len(violations) != 1 || !strings.Contains(violations[0], "title") {
		t.Errorf("应记录 title 越界，实际：%v", violations)
	}
}

func TestMergeLocalRejectsStructuralChange(t *testing.T) {
	prev := baseSlide() // 5 条
	next := baseSlide()
	next.Bullets = append(next.Bullets[:0:0], prev.Bullets[:4]...) // 只剩 4 条

	merged, violations := MergeLocal(prev, next, FixScope{BulletIndex: map[int]bool{0: true}})

	if len(merged.Bullets) != len(prev.Bullets) {
		t.Fatalf("局部修复不允许改变结构，条数应保持 %d，实际 %d",
			len(prev.Bullets), len(merged.Bullets))
	}
	if len(violations) == 0 || !strings.Contains(violations[0], "要点数量被改动") {
		t.Errorf("应记录结构变更，实际：%v", violations)
	}
}

func TestMergeLocalRejectsEmptyValue(t *testing.T) {
	prev := baseSlide()
	next := baseSlide()
	next.Bullets[1] = "   "

	merged, violations := MergeLocal(prev, next, FixScope{BulletIndex: map[int]bool{1: true}})

	if merged.Bullets[1] != prev.Bullets[1] {
		t.Errorf("被点名的字段被改成空值时必须回退，实际 %q", merged.Bullets[1])
	}
	if len(violations) != 1 || !strings.Contains(violations[0], "空值") {
		t.Errorf("应记录空值越界，实际：%v", violations)
	}
}

func TestMergeLocalDoesNotMutatePrev(t *testing.T) {
	prev := baseSlide()
	original := prev.Bullets[0]

	next := baseSlide()
	next.Bullets[0] = "被改过的内容。"

	_, _ = MergeLocal(prev, next, FixScope{BulletIndex: map[int]bool{0: true}})

	if prev.Bullets[0] != original {
		t.Errorf("MergeLocal 不应修改 prev：%q", prev.Bullets[0])
	}
}
