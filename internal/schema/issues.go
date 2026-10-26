// Package schema 定义多 Agent 之间交换的「结构化协议」。
//
// 为什么需要这样一层（它存在的理由）：
//
//	多 Agent 系统里最容易出事的环节，是"Agent 之间传什么"。
//	  传自然语言 → 无法校验、无法计数、无法统计，多跳必然失真；
//	  传结构化产物 → 可校验、可计数、可比较，而且能被代码直接判断。
//
// 本文件只解决其中一件事：**评审结论该长什么样**。
// 它是"代码工具"（硬判据）与"LLM Critic"（软判据）共同使用的公共语言——
// 正因为两边产出同一种结构，才能被合并、排序、加权、比较。
//
// 这正是 L1 的核心设计决策：**分数是给人看的，问题清单是给模型用的。**
package schema

import (
	"fmt"
	"sort"
	"strings"
)

// ─── 严重度 ─────────────────────────────────────────────────────────

// Level 是问题的严重度。
//
// 为什么要分级？
//
//	因为"要不要为它返工"必须由代码判断（而不是每一轮都无差别重试）。
//	有了级别，"停止条件"才写得出来：只剩 suggestion 就不用再返工了。
type Level string

const (
	// LevelBlocker 致命：产出不可交付，必须返工（如要点为空、标题缺失）。
	LevelBlocker Level = "blocker"

	// LevelMajor 严重：明显影响质量，应当返工（如要点空洞、条目超限）。
	LevelMajor Level = "major"

	// LevelSuggestion 建议：锦上添花，不阻塞交付（如措辞可以更凝练）。
	LevelSuggestion Level = "suggestion"
)

// Weight 是严重度的权重，用来把一份 Report 折算成"可比较的分数"。
//
// 为什么用加权而不是"数条数"？
//
//	① "1 个致命问题"和"1 个建议"完全不是一回事，数条数会把它们等同；
//	② 条数不变但严重度下降（3 个 blocker → 3 个 suggestion）其实是巨大进步，
//	   只看条数会误判成"没有收敛"，从而错误地停止返工。
//
// 未知级别返回 0：新加级别时务必同步这里，并用 Valid() 在入口处拦截脏数据。
func (l Level) Weight() int {
	switch l {
	case LevelBlocker:
		return 3
	case LevelMajor:
		return 2
	case LevelSuggestion:
		return 1
	default:
		return 0
	}
}

// Valid 报告级别是否合法。
//
// 用途：校验外部输入（尤其是模型返回的 level）。
// 脏数据不能让进系统——否则它权重为 0，会静默地把问题"洗掉"。
func (l Level) Valid() bool {
	switch l {
	case LevelBlocker, LevelMajor, LevelSuggestion:
		return true
	default:
		return false
	}
}

// ─── 问题来源 ───────────────────────────────────────────────────────

// Source 标记问题从哪来。
//
// 为什么要在结构里带上它？
//
//	因为它决定了"该信任到什么程度"：
//	rule 是确定性的、可复现的；critic 是概率的、可能有误报。
//	出问题排查时，"这条是谁说的"是第一件要问的事。
type Source string

const (
	// SourceRule 由代码规则产出（确定性，原则六里的"硬判据"）。
	SourceRule Source = "rule"

	// SourceCritic 由 LLM Critic 产出（语义判断，"软判据"）。
	SourceCritic Source = "critic"
)

// ─── 一条问题 ───────────────────────────────────────────────────────

// Issue 是一条**可执行**的问题。
//
// 四个字段缺一不可，少任何一个都会退化成"没用的抱怨"：
//
//	Level   → 决定"要不要为它返工"（代码可判断，不用问模型）
//	Field   → 定位（改哪里）
//	Message → 事实（错在哪）
//	Fix     → 行动方向（怎么改）—— 这一条是给模型看的，最容易被省，也最值钱
type Issue struct {
	Level   Level  `json:"level"`
	Field   string `json:"field"`   // 定位，如 "bullets" / "bullets[2]" / "title"
	Message string `json:"message"` // 事实
	Fix     string `json:"fix"`     // 行动方向
	Source  Source `json:"source"`  // 谁判的：rule / critic
}

// ─── 一份评审结论 ───────────────────────────────────────────────────

// Report 是一轮评审的完整结论。
//
// 为什么不是直接用 []Issue？
//
//	因为报告需要携带上下文（第几轮、评审的是哪一版产物），
//	这些信息在比较 best-so-far、打印进度、回溯问题时都要用。
type Report struct {
	Round   int     `json:"round"`
	Subject string  `json:"subject"` // 被评审对象（如文件名、产物名）
	Issues  []Issue `json:"issues"`

	// Incomplete 表示这份报告**不完整**：有评审环节没有成功执行。
	//
	// 为什么必须是一等字段，而不是只体现在函数的 error 里？
	//
	//	因为"没发现问题"和"没做检查"必须能区分（R8 立的规矩）。
	//	如果这个信息只活在某个函数的返回值里，它就会在调用链上
	//	被"打印一行日志然后继续"吃掉 —— 最终一个**从未被检查过**的产物
	//	会被当成合格品交付。这正是 L1-3 那个真 bug。
	//
	// 一句话：**重要状态要放在数据里，不要只放在控制流里** ——
	// 控制流里的 error 可以被忽略，数据不会。
	Incomplete bool `json:"incomplete,omitempty"`

	// IncompleteReason 说明是哪个环节没跑成（用于交付说明与事后排查）。
	IncompleteReason string `json:"incomplete_reason,omitempty"`
}

// NewReport 构造一份空报告。
func NewReport(round int, subject string) Report {
	return Report{Round: round, Subject: subject}
}

// Merged 合并若干份报告，产出这一轮的完整结论。
//
// 典型用法：把 layout_check（硬判据）与 critic（软判据）的结论合成一份，
// 这样"停止条件"只需要看一个对象，不用到处判断。
//
// Round 取最大（最新），Subject 取第一个非空。
func (r Report) Merged(others ...Report) Report {
	// 注意：这里显式分配新切片，不复用 r.Issues 的底层数组。
	// 否则调用方后续 append 会踩到同一块内存，产生"改了一份、另一份也变了"的幽灵 bug。
	merged := Report{
		Round:            r.Round,
		Subject:          r.Subject,
		Issues:           append([]Issue(nil), r.Issues...),
		Incomplete:       r.Incomplete,
		IncompleteReason: r.IncompleteReason,
	}
	for _, o := range others {
		if merged.Subject == "" {
			merged.Subject = o.Subject
		}
		if o.Round > merged.Round {
			merged.Round = o.Round
		}
		merged.Issues = append(merged.Issues, o.Issues...)

		// "不完整"必须**传播**：只要有一个来源没跑完，整份报告就不完整。
		// 这里用 OR 而不是"取最新"——因为"没检查"这件事不会因为
		// 另一份报告检查过了就消失。
		if o.Incomplete {
			merged.Incomplete = true
			merged.IncompleteReason = joinReasons(merged.IncompleteReason, o.IncompleteReason)
		}
	}
	return merged
}

// joinReasons 把两个"不完整原因"拼起来（去重、去空）。
func joinReasons(a, b string) string {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "" || a == b:
		return a
	default:
		return a + "; " + b
	}
}

// Score 返回加权严重度总分。0 表示没有任何问题（分数越低越好）。
func (r Report) Score() int {
	total := 0
	for _, it := range r.Issues {
		total += it.Level.Weight()
	}
	return total
}

// Count 返回指定严重度的问题条数。
func (r Report) Count(level Level) int {
	n := 0
	for _, it := range r.Issues {
		if it.Level == level {
			n++
		}
	}
	return n
}

// HardCount 返回"必须处理"的问题条数（blocker + major）。
func (r Report) HardCount() int {
	return r.Count(LevelBlocker) + r.Count(LevelMajor)
}

// HasHardIssue 表示是否还存在致命/严重问题。
//
// ⚠️ 注意：**它不等于"可以交付"**。请用 Deliverable()。
// 只看 HasHardIssue 会漏掉一种情况：评审根本没跑完，
// 于是"没有硬问题"只是因为"没人检查过"。
func (r Report) HasHardIssue() bool { return r.HardCount() > 0 }

// Deliverable 表示这份报告是否支持"直接交付"。
//
// 它同时检查两件事：**没有致命/严重问题** 且 **评审是完整的**。
// 这两个条件缺一不可——只看前者，就会把一个从未被检查过的产物交付出去。
func (r Report) Deliverable() bool {
	return !r.HasHardIssue() && !r.Incomplete
}

// BetterThan 判断 r 是否比 other 更值得交付。
//
// 规则（按顺序比较）：
//
//  1. 加权总分更低者更优；
//  2. 总分相同时，硬问题（blocker+major）更少者更优
//     —— 同等代价下，优先消灭致命问题；
//  3. 仍然相同时，**评审完整的那一份更优**：
//     不能拿"没测出来"去替换"测过且没问题"；
//  4. 全部相同 → 返回 false，**保留更早的版本**。
//     不要用"最新版覆盖 best"，否则会出现"分数没变但 best 一直在动"的假象。
func (r Report) BetterThan(other Report) bool {
	if rs, os := r.Score(), other.Score(); rs != os {
		return rs < os
	}
	if rh, oh := r.HardCount(), other.HardCount(); rh != oh {
		return rh < oh
	}
	if r.Incomplete != other.Incomplete {
		return !r.Incomplete
	}
	return false
}

// Sorted 返回按严重度降序（同级按 Field 升序）排列的副本。
//
// 为什么必须排序？
//
//	"输出稳定"是工程硬要求：同一份输入打印顺序一致，才能 diff、
//	才能做回归对比、别人才复现得了你的实验。map 遍历那种随机顺序是灾难。
func (r Report) Sorted() []Issue {
	out := make([]Issue, len(r.Issues))
	copy(out, r.Issues)
	sort.SliceStable(out, func(i, j int) bool {
		wi, wj := out[i].Level.Weight(), out[j].Level.Weight()
		if wi != wj {
			return wi > wj
		}
		return out[i].Field < out[j].Field
	})
	return out
}

// Lines 返回每条问题的一行描述（按严重度排序），用于终端打印与日志。
func (r Report) Lines() []string {
	issues := r.Sorted()
	out := make([]string, 0, len(issues))
	for _, it := range issues {
		line := fmt.Sprintf("[%-10s][%-6s] %s: %s", it.Level, it.Source, it.Field, it.Message)
		if it.Fix != "" {
			line += " → " + it.Fix
		}
		out = append(out, line)
	}
	return out
}

// String 返回一行摘要，供进度打印使用。
func (r Report) String() string {
	s := fmt.Sprintf("第 %d 轮「%s」| 总分 %d | 致命 %d / 严重 %d / 建议 %d",
		r.Round, r.Subject, r.Score(),
		r.Count(LevelBlocker), r.Count(LevelMajor), r.Count(LevelSuggestion))
	if r.Incomplete {
		// 摘要里必须显式出现"不完整"。
		// 一份"看不见自己缺口"的报告，比一份缺了数据但诚实的报告危险得多。
		s += " | [评审不完整]"
	}
	return s
}

// ─── best-so-far ───────────────────────────────────────────────────

// Best 记录"到目前为止最好的那一版产物及其评审结论"。
//
// 为什么必须单独记录（而不是直接交付最后一版）？
//
//	因为返工可能**震荡**：Critic 让改成 A，下一轮又让改回 B。
//	"最后一版"经常不是"最好的一版"——只交付最后一版，
//	等于把中间某一轮辛苦调好的版本白白丢掉。
//
// 用泛型是因为这个机制与产物类型无关：今天装的是字符串/幻灯片，
// 明天装的是 L2 的 Artifact —— 机制不变，只是换个类型参数。
type Best[T any] struct {
	Round   int
	Report  Report
	Payload T

	set bool
}

// Observe 观察一轮结果；若这一版比当前 best 更好，则更新并返回 true。
func (b *Best[T]) Observe(round int, payload T, rep Report) bool {
	if b.set && !rep.BetterThan(b.Report) {
		return false
	}
	b.set = true
	b.Round = round
	b.Payload = payload
	b.Report = rep
	return true
}

// Has 表示是否已经记录过至少一版（用于判断"有没有东西可交付"）。
func (b *Best[T]) Has() bool { return b.set }
