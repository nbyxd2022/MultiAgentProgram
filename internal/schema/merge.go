package schema

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 这个文件解决一件事：**受限的局部修复**（L1-1）。
//
// 为什么需要它？
//
//	"一条要点写得空洞"和"整页跑题"是两种问题，修复方式本该不同：
//	  · 可精确定位的问题（`title` / `bullets[2]`）→ 只改那**一个字段**，其余原样保留；
//	  · 整体问题（`bullets` / 空 field）        → 没有局部解，只能整体重写。
//
//	改动半径越小，回归风险越低（**原则八**）：
//	返工震荡的根源就是"每轮都在整页重新掷骰子"，把对的也一起改坏了。
//
// 对照企业仓库：
//
//	它用 `ExtractLayoutHTML` / `ReplaceLayoutHTML` 划出"可变区段"，
//	LLM 只能改那一段，CSS 骨架与脚本原样保留。
//	在我们这套 JSON 产物上：
//	  · **"区段"就是 `Issue.Field`** —— 我们已经有了，只是之前没用上；
//	  · **`ReplaceLayoutHTML` 的对应物就是本文件的 `MergeLocal`**。

// bulletFieldRe 匹配置确定位到某一条要点的字段，如 `bullets[2]`。
var bulletFieldRe = regexp.MustCompile(`^bullets\[(\d+)\]$`)

// FixScope 描述一次修复**被允许改动的范围**。
type FixScope struct {
	Title       bool
	BulletIndex map[int]bool
}

// Empty 表示范围内什么都不能改（不该走局部修复这条路）。
func (s FixScope) Empty() bool { return !s.Title && len(s.BulletIndex) == 0 }

// Fields 返回范围内可修改字段的**稳定排序**列表。
//
// 排序是为了"输出稳定"：同一份范围每次打印顺序一致，才能 diff、才能写进 prompt。
// 注意下标要按**数值**排序（bullets[2] 在 bullets[10] 之前），不能按字符串排。
func (s FixScope) Fields() []string {
	out := make([]string, 0, len(s.BulletIndex)+1)
	if s.Title {
		out = append(out, "title")
	}

	idx := make([]int, 0, len(s.BulletIndex))
	for i := range s.BulletIndex {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		out = append(out, fmt.Sprintf("bullets[%d]", i))
	}
	return out
}

// LocalTarget 返回"这一轮允许被局部修改的字段范围"。
//
// 两条筛选规则：
//
//	① 只看**硬问题**（blocker / major）—— 建议级问题不阻塞交付，不值得为它返工；
//	② 只收**可精确定位**的字段 —— `bullets` 这种集合级定位没有局部解。
func (r Report) LocalTarget() FixScope {
	scope := FixScope{BulletIndex: make(map[int]bool)}

	for _, it := range r.Issues {
		if !it.isHard() {
			continue
		}
		switch {
		case it.Field == "title":
			scope.Title = true
		case bulletFieldRe.MatchString(it.Field):
			if i, err := strconv.Atoi(bulletFieldRe.FindStringSubmatch(it.Field)[1]); err == nil {
				scope.BulletIndex[i] = true
			}
		}
	}
	return scope
}

// NeedsFullRewrite 表示是否存在**无法精确定位的硬问题**。
//
// 例如"三条要点全是趋势，缺结论与建议"——它指向的是整个集合，
// 局部改任何一条都解决不了，只能整体重写。
func (r Report) NeedsFullRewrite() bool {
	for _, it := range r.Issues {
		if !it.isHard() {
			continue
		}
		if it.Field == "title" || bulletFieldRe.MatchString(it.Field) {
			continue
		}
		return true
	}
	return false
}

func (i Issue) isHard() bool {
	return i.Level == LevelBlocker || i.Level == LevelMajor
}

// MergeLocal 用 next 中**被允许修改的字段**更新 prev，其余字段一律保留 prev 的原值。
//
// 返回：合并结果 + 越界修改说明（供日志与排查）。
//
// # 为什么必须由**代码**做这件事，而不是相信模型"我只改这一条"
//
// 因为"修复者"完全可能顺手改坏别处（修好了 A、改坏了 B）。
// 提示词只能**减少**这种行为，不能**阻止**它；能阻止它的是这一行代码。
//
// 这正是企业仓库 `layout_guard_repair.go:336-339` 那段
// "修复结果太短（< 50 字符）就直接丢弃"的同一种防御思想 ——
// 只不过它防的是"删空"，我们防的是"越界"。
//
// 三条护栏：
//
//	① 未被点名的字段被改动 → 回退，并记一条越界；
//	② 被点名的字段被改成空值 → 回退（修复不能把内容改没）；
//	③ 要点数量被增删 → 全部回退到最后一次确认过的数量（局部修复不允许改变结构）。
func MergeLocal(prev, next Slide, scope FixScope) (Slide, []string) {
	out := cloneSlide(prev)
	var violations []string

	// ── 标题 ──
	switch {
	case scope.Title:
		if strings.TrimSpace(next.Title) == "" {
			violations = append(violations, "title 被改成空值，已回退")
		} else {
			out.Title = next.Title
		}
	case next.Title != prev.Title:
		violations = append(violations, "title 未被要求修改，已回退")
	}

	// ── 结构：局部修复不允许增删要点 ──
	if len(next.Bullets) != len(prev.Bullets) {
		violations = append(violations, fmt.Sprintf(
			"要点数量被改动（%d → %d），已回退到最后一次确认的结构",
			len(prev.Bullets), len(next.Bullets)))
	}

	// ── 逐条要点 ──
	for i := range out.Bullets {
		field := fmt.Sprintf("bullets[%d]", i)

		if !scope.BulletIndex[i] {
			if i < len(next.Bullets) && next.Bullets[i] != prev.Bullets[i] {
				violations = append(violations, field+" 未被要求修改，已回退")
			}
			continue
		}

		if i >= len(next.Bullets) {
			violations = append(violations, field+" 在修复结果中缺失，已回退")
			continue
		}
		if strings.TrimSpace(next.Bullets[i]) == "" {
			violations = append(violations, field+" 被改成空值，已回退")
			continue
		}
		out.Bullets[i] = next.Bullets[i]
	}

	return out, violations
}

// cloneSlide 深拷贝（切片是引用类型，不复制会改到调用方手里的原版）。
func cloneSlide(s Slide) Slide {
	return Slide{
		Title:   s.Title,
		Bullets: append([]string(nil), s.Bullets...),
	}
}
