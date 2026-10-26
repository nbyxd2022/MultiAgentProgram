package observ

import (
	"bytes"
	"strings"
	"testing"
)

// ⚠️ 这条测试守的是**格式本身** —— 而它有额外的分量：
//
//	`ConsoleTracer` 是从 `cmd/l0~l3` 那**四份** `printEvent` 里抽出来的，
//	而"抽出来之后输出不变"是这次重构**唯一的正确性证据**。
//	（与 R44 删 `graph.Event.Step` 时那条"逐字节不变"同一个用法。）
//
//	所以它比的是**完整的字符串**，不是 `Contains`。
//	—— `Contains` 会让排版错误（多一个空格、少一个缩进）全部漏过去。
func TestConsoleTracerFormat(t *testing.T) {
	var buf bytes.Buffer
	tr := NewConsoleTracer(&buf)

	tr.Trace(Event{Layer: LayerGraph, Phase: "research", Kind: KindStart, Detail: "研究员"})
	tr.Trace(Event{Layer: LayerGraph, Phase: "research", Kind: KindProduct, Detail: "research@1\n3 页"})
	tr.Trace(Event{Layer: LayerGraph, Kind: KindDone, Detail: "全部完成"})

	// ⚠️ 第三行是 R46 **改过**的：整体事件（`Phase == ""`）现在**也打 Kind**。
	//
	//	旧的是 `      全部完成`（只有 `Detail`）——
	//	而那**丢了一样东西**：`skipped` / `notice` / `done` 三条都走这条分支，
	//	**光看那一行分不出是哪一种**。
	//
	//	> 观测的第一职责是**如实** —— 丢掉 `Kind` 就是不准。
	//	> （代价是 `cmd/l3` 那几行的输出变了，而那是一次**改进**，不是回归。）
	want := "[步1][research] start     研究员\n" +
		"[步1][research] product   research@1 / 3 页\n" +
		"      done      全部完成\n"

	if got := buf.String(); got != want {
		t.Errorf("格式不对：\n期望 %q\n实际 %q", want, got)
	}
}

// ⚠️ **步号只在 `KindStart` 时推进** —— 一个 step 会发多条事件
//
//	（start + product + 可能还有 interrupt），而它们**属于同一跳**。
//
//	这条守的是上面那条测试**看不出来**的东西：格式对了，而计数**错了**
//	（比如每条事件都 +1）也会打出一模一样的三行 —— 只要恰好一个 step 一条事件。
func TestConsoleTracerCountsOnlyStarts(t *testing.T) {
	var buf bytes.Buffer
	tr := NewConsoleTracer(&buf)

	// 一跳发三条事件 —— 三条都该是 `[步1]`
	for _, k := range []Kind{KindStart, KindProduct, KindInterrupt} {
		tr.Trace(Event{Layer: LayerGraph, Phase: "outline", Kind: k, Detail: "x"})
	}
	// 第二跳
	tr.Trace(Event{Layer: LayerGraph, Phase: "design", Kind: KindStart, Detail: "y"})

	got := buf.String()
	if n := strings.Count(got, "[步1][outline]"); n != 3 {
		t.Errorf("第一跳的三条事件都该打 [步1]，实际只有 %d 条：\n%s", n, got)
	}
	if !strings.Contains(got, "[步2][design]") {
		t.Errorf("第二条 `start` 应当推进到 [步2]：\n%s", got)
	}
}

// ⚠️ `NewConsoleTracerAt` 让"**这一轮**的第几跳"成为可能 ——
//
//	而那个起点**不是它自己算的**（它手上只有自己见到的事件）。
//
//	`cmd/l3` 从**录播**里读回上一批进程跑过的跳数（R45 的兑现）。
func TestConsoleTracerStartsFromGivenStep(t *testing.T) {
	var buf bytes.Buffer
	tr := NewConsoleTracerAt(&buf, 2) // 前面已经跑过 2 跳

	tr.Trace(Event{Layer: LayerGraph, Phase: "design", Kind: KindStart, Detail: "x"})

	if !strings.Contains(buf.String(), "[步3][design]") {
		t.Errorf("应当从第 3 跳开始，实际：%q", buf.String())
	}
}

// ─── OneLine ────────────────────────────────────────────────────────

// ⚠️ **空行不丢** —— 这是本条测试要守的那**一个**决定。
//
//	`"a\n\nb"` → `"a /  / b"`（而不是 `"a / b"`），看起来没那么干净 —— 而这是故意的：
//	**观测的第一职责是如实，不是美化。**
func TestOneLineKeepsBlankLines(t *testing.T) {
	if got := OneLine("a\n\nb"); got != "a /  / b" {
		t.Errorf("空行不该被吞掉（观测要如实）：%q", got)
	}
	if got := OneLine("  a  \n b "); got != "  a   /  b " {
		t.Errorf("空格也不该被顺手 trim —— 那是另一种美化：%q", got)
	}
}

// ⚠️ 截断按 **rune**，不按字节 —— 按字节会把一个汉字切成半个，
//
//	而那在终端里比"截短了"难看得多（一个乱码字符）。
func TestOneLineTruncatesByRune(t *testing.T) {
	long := strings.Repeat("汉", 200)
	got := OneLine(long)

	if !strings.HasSuffix(got, "…") {
		t.Fatal("超长应当被截断")
	}
	// ⚠️ 断言的是"**没有**半个汉字" —— 而不是"长度恰好是 110"：
	//	后者只要改一次 `lineLimit` 就会红，而它并不是这条测试要守的东西。
	if strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("出现了替换字符 —— 说明按字节切了：%q", got)
	}
	if n := len([]rune(got)); n != lineLimit+1 { // +1 是那个省略号
		t.Errorf("应当截到 %d 个 rune（+省略号），实际 %d", lineLimit, n)
	}
}

// 短文本原样返回 —— **不该有任何"顺手"的加工**。
func TestOneLineLeavesShortTextAlone(t *testing.T) {
	for _, s := range []string{"", "x", "research@1  research"} {
		if got := OneLine(s); got != s {
			t.Errorf("短文本应当原样返回：%q → %q", s, got)
		}
	}
}
