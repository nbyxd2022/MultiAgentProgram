package tools

import (
	"fmt"
	"strings"

	"multiagent/internal/schema"
)

// 这个文件是「零 token 的确定性修复」——对应企业仓库的
// `agents/v5_html_ppt_agent/layout_guard_quickfix.go`。
//
// 为什么要有它？（L1-1：返工粒度太粗）
//
//	因为"格式问题"和"内容问题"的修复成本差了几个数量级：
//	  · 格式问题（重复、HTML 标签、标点不统一）—— **正确答案唯一**，
//	    代码几微秒就能改完，不花一分钱 token；
//	  · 内容问题（空洞、跑题、无依据）—— 没有唯一解，只能交给 LLM。
//
//	先让代码把便宜的那部分吃掉，LLM 才只需要处理真正需要判断的部分。
//	这就是**原则七：分级修复**。
//
// 判据（很关键）：**只做"唯一解"的修复。**
//
//	"这条要点怎么改更好"有无数个解 → **不在这里做**，交给 LLM。
//	一旦开始"猜哪个答案更好"，QuickFix 就退化成了一个又慢又差的 LLM。

// QuickFixTier 是确定性修复的层级。
//
// 顺序不是随便定的：**先剥 HTML、再去重**，才能抓到
// "剥掉标签之后才发现是重复的"那一对。
// （企业仓库同样把"移除违规属性"排在"删除过大 flex"之前，理由相同。）
type QuickFixTier int

const (
	// QuickFixStripHTML 剥离要点里的 HTML 标签。
	QuickFixStripHTML QuickFixTier = iota
	// QuickFixDedup 去除完全重复的要点（保留第一次出现的）。
	QuickFixDedup
	// QuickFixDropEmpty 删除空要点（**仅当删后条数仍然达标**）。
	QuickFixDropEmpty
	// QuickFixUnifyPunct 统一句末标点风格（跟多数派）。
	QuickFixUnifyPunct

	quickFixTierCount
)

// quickFixMaxPass 是不动点迭代的**硬上限**（原则一：确定性兜底）。
//
// 正常情况下 2 轮就到不动点；留 5 轮是为了"即使某个 tier 之间
// 意外互相激发，也一定停得下来"。
const quickFixMaxPass = 5

// QuickFix 反复应用全部 tier，直到**不再有任何变化**（到达不动点）。
//
// 为什么是"迭代到不动点"，而不是像企业仓库那样固定轮数？
//
//	因为它的每一轮后面都要重新跑一次布局检测（Gecko 级测量，很贵），
//	所以必须把轮数压死；而我们的修复是纯字符串操作、成本趋近于零，
//	于是"迭代到不动点"是更准确的模型：**该修的都修完，一条也不多跑**。
//
//	"固定轮数"是为了控成本；当成本趋于零时，就该换成"修到底"。
//
// 返回：修复后的产物 + 实际应用过的动作（按发生顺序）。
func QuickFix(s schema.Slide) (schema.Slide, []string) {
	current := s
	var applied []string

	for pass := 0; pass < quickFixMaxPass; pass++ {
		changed := false
		for tier := QuickFixStripHTML; tier < quickFixTierCount; tier++ {
			next, note, ok := ApplyQuickFixTier(current, tier)
			if !ok {
				continue
			}
			current = next
			applied = append(applied, note)
			changed = true
		}
		if !changed {
			// 不动点：再跑一轮不会有任何变化
			return current, applied
		}
	}
	return current, applied
}

// ApplyQuickFixTier 应用单个 tier。
//
// 返回：修复后的产物、动作描述、是否发生变化。
// 没有变化时返回原值与空描述 —— 调用方据此判断"这一层无事可做"。
func ApplyQuickFixTier(s schema.Slide, tier QuickFixTier) (schema.Slide, string, bool) {
	switch tier {
	case QuickFixStripHTML:
		return stripHTMLTier(s)
	case QuickFixDedup:
		return dedupTier(s)
	case QuickFixDropEmpty:
		return dropEmptyTier(s)
	case QuickFixUnifyPunct:
		return unifyPunctTier(s)
	default:
		return s, "", false
	}
}

// stripHTMLTier 剥离要点里的 HTML 标签（"<b>x</b>" → "x"）。
//
// 为什么这个修复是**安全**的？
//
//	因为产物本来就约定是纯文本（见 render_slide_html 的 Description），
//	标签是**意外混入**的；留着会被 html.EscapeString 转义成一行乱码
//	（第三讲里那个"合法但错误"的坑）。剥掉一定比留着更接近正确。
//
// ⚠️ 它**不做 TrimSpace**，哪怕只多这一步。
//
//	本文件第一版就踩了这个坑：顺手 trim 了一下，结果"首尾空白"也被算成
//	"剥离 HTML 标签"，动作日志开始**说谎**——而"空要点"本该由 DropEmpty 负责。
//	一个 tier 只做一件事，不是为了好看，是为了**出问题时定位得到是谁改的**。
//	（首尾空白本身不是问题：layout_check 内部会 TrimSpace 后再判断。）
func stripHTMLTier(s schema.Slide) (schema.Slide, string, bool) {
	out := cloneSlide(s)
	changed := false

	for i, b := range out.Bullets {
		cleaned := htmlTagRe.ReplaceAllString(b, "")
		if cleaned != b {
			out.Bullets[i] = cleaned
			changed = true
		}
	}

	if !changed {
		return s, "", false
	}
	return out, "剥离 HTML 标签", true
}

// dedupTier 去除完全重复的要点，保留第一次出现的那条。
//
// 注意：这里比的是"剥标签之后"的文本 —— 所以 tier 顺序必须是 StripHTML 在前。
// 否则 "<b>X</b>" 和 "X" 会被当成两条不同的要点，去重就漏了。
//
// 空要点**不在这里处理**：那是 DropEmpty 的职责。一个 tier 只做一件事，
// 出问题时才定位得到是谁改的。
func dedupTier(s schema.Slide) (schema.Slide, string, bool) {
	seen := make(map[string]bool, len(s.Bullets))
	out := make([]string, 0, len(s.Bullets))
	removed := 0

	for _, b := range s.Bullets {
		key := strings.ToLower(strings.TrimSpace(b))
		if key != "" && seen[key] {
			removed++
			continue
		}
		if key != "" {
			seen[key] = true
		}
		out = append(out, b)
	}

	if removed == 0 {
		return s, "", false
	}

	res := cloneSlide(s)
	res.Bullets = out
	return res, fmt.Sprintf("去除 %d 条重复要点", removed), true
}

// dropEmptyTier 删除空要点——**但只在删完之后条数仍然达标时**。
//
// 为什么要加这个条件？
//
//	因为"删除空条目"和"补足条数"是两个相反的方向：
//	如果删完只剩 2 条（低于下限），我们只是把一个"空条目"问题
//	换成了一个"条数不足"问题，**净收益为负**。
//
//	这种"修 A 引入 B"的情况，宁可不修，交给 LLM 一次性处理。
//	这也是**原则八（修复要局部化）**的另一面：
//	改动半径要小到**不触发新问题**。
func dropEmptyTier(s schema.Slide) (schema.Slide, string, bool) {
	empties := 0
	for _, b := range s.Bullets {
		if strings.TrimSpace(b) == "" {
			empties++
		}
	}
	if empties == 0 {
		return s, "", false
	}
	if len(s.Bullets)-empties < schema.MinBullets {
		// 修了会引入更严重的问题 → 主动放弃这一层
		return s, "", false
	}

	out := make([]string, 0, len(s.Bullets)-empties)
	for _, b := range s.Bullets {
		if strings.TrimSpace(b) == "" {
			continue
		}
		out = append(out, b)
	}

	res := cloneSlide(s)
	res.Bullets = out
	return res, fmt.Sprintf("删除 %d 条空要点", empties), true
}

// unifyPunctTier 统一句末标点风格。
//
// 方向选择：**跟多数派**，平局时补句号。
//
//	为什么不"一律补句号"？因为那会改掉少数派（也可能是改掉多数派）——
//	假如 1 条带句号、4 条不带，"一律补"要改 4 条，"跟多数派"只改 1 条。
//	**改动半径最小的那个选择，就是这里的选择**（原则八）。
func unifyPunctTier(s schema.Slide) (schema.Slide, string, bool) {
	with, without := 0, 0
	for _, b := range s.Bullets {
		t := strings.TrimSpace(b)
		if t == "" {
			continue
		}
		if hasEndPunct(t) {
			with++
		} else {
			without++
		}
	}

	// 本来就统一（或没有可比对象）→ 无事可做
	if with == 0 || without == 0 {
		return s, "", false
	}

	addPunct := with >= without // 平局时补句号
	out := cloneSlide(s)
	for i, b := range out.Bullets {
		t := strings.TrimSpace(b)
		if t == "" {
			continue
		}
		switch {
		case addPunct && !hasEndPunct(t):
			out.Bullets[i] = t + "。"
		case !addPunct && hasEndPunct(t):
			out.Bullets[i] = strings.TrimRight(t, "。.！!？?")
		}
	}

	return out, "统一句末标点风格", true
}

// cloneSlide 深拷贝一份产物。
//
// 切片是引用类型：不复制的话，修复会**顺手改掉调用方手里的原版**，
// 于是"修复前的版本"就再也拿不到了（best-so-far 会失效）。
// 这个坑我们在 Report.Merged 与 render_slide 里各踩过一次。
func cloneSlide(s schema.Slide) schema.Slide {
	return schema.Slide{
		Title:   s.Title,
		Bullets: append([]string(nil), s.Bullets...),
	}
}
