package tools

import (
	"strings"
	"testing"

	"multiagent/internal/schema"
)

func slideWith(bullets ...string) schema.Slide {
	return schema.Slide{Title: "新能源汽车行业趋势", Bullets: bullets}
}

// ─── 顺序：先剥 HTML，再去重 ────────────────────────────────────────

func TestQuickFixStripsThenDedups(t *testing.T) {
	// 这一对要点在"剥标签之前"看起来是**不同的两条**：
	//   "<b>产业链成本下降</b>，电池价格进入下行通道。"
	//   "产业链成本下降，电池价格进入下行通道。"
	// 只有先剥标签，去重才抓得到它们 —— 这就是 tier 顺序的意义。
	in := slideWith(
		"<b>产业链成本下降</b>，电池价格进入下行通道。",
		"产业链成本下降，电池价格进入下行通道。",
		"政策支持力度加大，补贴与路权双重驱动。",
	)

	got, applied := QuickFix(in)

	if len(got.Bullets) != 2 {
		t.Fatalf("剥标签后应发现重复并去重，实际 %d 条：%v", len(got.Bullets), got.Bullets)
	}
	if strings.Contains(got.Bullets[0], "<") {
		t.Errorf("HTML 标签未被剥离：%q", got.Bullets[0])
	}
	if len(applied) != 2 {
		t.Errorf("应记录两个动作（剥离 + 去重），实际：%v", applied)
	}
}

// ─── 删空要点：有前置条件 ───────────────────────────────────────────

func TestQuickFixDropEmptyOnlyWhenCountStaysValid(t *testing.T) {
	t.Run("删完仍达标 → 删", func(t *testing.T) {
		got, applied := QuickFix(slideWith("甲。", "  ", "乙。", "丙。", "丁。"))

		if len(got.Bullets) != 4 {
			t.Fatalf("应删掉 1 条空要点，实际 %d 条：%v", len(got.Bullets), got.Bullets)
		}
		if len(applied) != 1 {
			t.Errorf("应记录 1 个动作，实际：%v", applied)
		}
	})

	t.Run("删完不达标 → 不删", func(t *testing.T) {
		// 3 条里 1 条是空的，删掉只剩 2 条 < MinBullets。
		// 修了会把"空条目"问题换成"条数不足"问题 —— 净收益为负，宁可不修。
		got, applied := QuickFix(slideWith("甲。", "  ", "乙。"))

		if len(got.Bullets) != 3 {
			t.Fatalf("不达标时不应删除，实际 %d 条：%v", len(got.Bullets), got.Bullets)
		}
		if len(applied) != 0 {
			t.Errorf("不应有任何动作，实际：%v", applied)
		}
	})
}

// ─── 标点统一：跟多数派（改动半径最小）─────────────────────────────

func TestQuickFixUnifyPunctFollowsMajority(t *testing.T) {
	t.Run("多数不带句号 → 去掉少数派的句号", func(t *testing.T) {
		got, _ := QuickFix(slideWith("甲", "乙", "丙", "丁。"))

		if got.Bullets[3] != "丁" {
			t.Errorf("应跟多数派去掉句号（只改 1 条），实际 %q", got.Bullets[3])
		}
	})

	t.Run("多数带句号 → 给少数派补上", func(t *testing.T) {
		got, _ := QuickFix(slideWith("甲", "乙。", "丙。", "丁。"))

		if got.Bullets[0] != "甲。" {
			t.Errorf("应给少数派补上句号，实际 %q", got.Bullets[0])
		}
	})

	t.Run("本来统一 → 不动", func(t *testing.T) {
		_, applied := QuickFix(slideWith("甲。", "乙。", "丙。"))
		if len(applied) != 0 {
			t.Errorf("风格本来就统一，不应有动作，实际：%v", applied)
		}
	})
}

// ─── 不动点 ─────────────────────────────────────────────────────────

func TestQuickFixReachesFixedPoint(t *testing.T) {
	in := slideWith("<b>甲</b>", "甲", "  ", "乙", "丙")

	once, _ := QuickFix(in)
	twice, applied := QuickFix(once)

	if len(applied) != 0 {
		t.Errorf("第二次运行必须无事可做（已到不动点），实际：%v", applied)
	}
	if len(twice.Bullets) != len(once.Bullets) {
		t.Fatalf("条数不稳定：%d → %d", len(once.Bullets), len(twice.Bullets))
	}
	for i := range once.Bullets {
		if once.Bullets[i] != twice.Bullets[i] {
			t.Errorf("bullets[%d] 不稳定：%q → %q", i, once.Bullets[i], twice.Bullets[i])
		}
	}
}

// ─── 边界：只做"唯一解"，内容一律不碰 ────────────────────────────────

func TestQuickFixDoesNotTouchContent(t *testing.T) {
	// "空洞"是内容问题，没有唯一解 —— QuickFix 必须完全不动它。
	in := slideWith(
		"要重视创新。",
		"要重视创新。", // 与上一条完全重复 → 会被去重
		"市场规模持续扩大，渗透率快速提升。",
		"政策支持力度加大，补贴与路权双重驱动。",
	)

	got, _ := QuickFix(in)

	if len(got.Bullets) != 3 {
		t.Fatalf("应去重掉 1 条，实际 %d 条：%v", len(got.Bullets), got.Bullets)
	}
	// 内容一字未改
	if got.Bullets[1] != "市场规模持续扩大，渗透率快速提升。" {
		t.Errorf("内容被改动了（QuickFix 不该做内容判断）：%q", got.Bullets[1])
	}
}

func TestQuickFixOnCleanSlideIsNoOp(t *testing.T) {
	in := slideWith(
		"结论先行：先给出可验证的核心判断。",
		"数据支撑：每个判断配一个可量化指标。",
		"行动建议：落到 3 条可执行的事情。",
	)

	got, applied := QuickFix(in)

	if len(applied) != 0 {
		t.Errorf("合格产物不应被 QuickFix 改动，实际：%v", applied)
	}
	for i := range in.Bullets {
		if got.Bullets[i] != in.Bullets[i] {
			t.Errorf("bullets[%d] 被改动：%q → %q", i, in.Bullets[i], got.Bullets[i])
		}
	}
}

func TestQuickFixDoesNotMutateInput(t *testing.T) {
	// 切片是引用类型：修复不能顺手改掉调用方手里的原版，
	// 否则"修复前的版本"就再也拿不到了（best-so-far 会失效）。
	in := slideWith("<b>甲</b>", "甲", "乙", "丙")

	_, _ = QuickFix(in)

	if in.Bullets[0] != "<b>甲</b>" {
		t.Errorf("原始产物被修改了：%q", in.Bullets[0])
	}
}
