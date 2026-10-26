// Package eval 提供**离线可评测**：一组用例、一键跑完、给出可比指标。
//
// # 它解决什么（R38 原理四：**可观测 ≠ 可评测**）
//
//	L0~L3 一路把"**可观测**"做起来了（事件流 / 录播 / 行为投影）。
//	而可观测回答的是"**这一次发生了什么**" —— 它**不能回答**
//	"**改了 Prompt 之后是变好还是变坏**"。
//
//	因为后者要的是一件观测**没有**的东西：**一个期望**。
//
//		`-verify`（cmd/l3）  比的是"**两次运行**是否相同"  → **自比**
//		本包                 比的是"**一次运行 vs 一个期望**" → **他比**
//
//	> ### 这两件事的判据**不一样**，而它们**都会用到事件流**。
//	> 混在一起会得到一个"看起来在评测、其实只是自己跟自己比"的东西。
//
// ⚠️ 而"他比"要付一个**具体的代价**：**期望从哪来**？
//
//	只能**人写**。而人写的东西会过期、会被改错、会在某次重构后变得不再成立 ——
//	所以每一条判据**失败时都必须说清"是哪一条、期望什么、实际什么"**（见 `check`）。
//
// # ⚠️ 评测集最容易消失（R38 原理四）
//
//	它不在业务代码里、**没有调用点**、每跑一次都花钱。
//	把它做成一个**有类型、有测试、有 README** 的东西，
//	至少让"删掉它"变成一次**显式的决定**，而不是"慢慢没人跑了"。
package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"multiagent/internal/schema"
)

// Case 是一个评测用例。
type Case struct {
	ID    string            `json:"id"`
	Topic string            `json:"topic"`
	Hints map[string]string `json:"hints,omitempty"`
	Tags  []string          `json:"tags,omitempty"` // 用于跑子集（"只跑 smoke"）

	// Expect 是**硬判据**（原理五）：能由代码给出确定答案的那些。
	//
	// ⚠️ "硬"的判据是**判据本身的性质**，不是"它严不严" ——
	//	`MinFindings: 1` 也是硬的（数得出来）；"文笔好不好"才是软的。
	Expect Expectation `json:"expect"`
}

// Expectation 是一个用例的**期望**。
//
// # ⚠️ 每一条都对应"一种真实的回归"
//
//	这不是"把能查的都查一遍" —— 每一条的存在理由是**具体的**：
//
//		MustHaveKinds   漏产出了某一种产物（图跑完了，但少了 design）
//		MinFindings     研究变"薄"了（流程没断，但结论从 5 条变成 0 条）
//		Pages           页数漂了（改 Prompt 时最容易碰坏的那个数）
//		MaxSteps        打转（某节点在反复重做自己 —— 而它**没触发**上限）
//		MaxTokens       成本翻了几倍（质量没变，钱涨了 —— 那也是一次回归）
//		MaxWall         变慢了
//		MustContain     页面上**必须**出现的话（最便宜、最能挡住一类回归的判据）
//		MustNotContain  页面上**不许**出现的话（占位符 / 报错文本漏出去）
//
//	⭐ **零值 = 这一条不检查**（不设 `MaxTokens` 就是"不限成本"）——
//	  这与 `budget.Limits` 的 fail-closed 契约**相反**，所以要在这里写明：
//
//		期望是**人写的**，而"人没写的那些"本来就是"他没要求"。
//		要求"没写就必须为 0"会让每一个用例都必须写全八条 —— 而那样
//		最大的可能是**有人随手填一个 0**，于是那个用例**永远过不了**。
type Expectation struct {
	// MustHaveKinds 是**必须产出**的产物种类（顺序无关）。
	MustHaveKinds []schema.Kind `json:"must_have_kinds,omitempty"`

	// MinFindings 是研究素材的**最少**条数（`0` = 不检查）。
	MinFindings int `json:"min_findings,omitempty"`

	// Pages 是期望的页数（`0` = 不检查）。
	Pages int `json:"pages,omitempty"`

	// MaxSteps / MaxTokens / MaxWall 是**上界**（`0` = 不检查）。
	MaxSteps  int           `json:"max_steps,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	MaxWall   time.Duration `json:"max_wall,omitempty"`

	// MustContain / MustNotContain —— 见 `Expectation` 的注释。
	MustContain    []string `json:"must_contain,omitempty"`
	MustNotContain []string `json:"must_not_contain,omitempty"`
}

// CaseSet 是一组用例。
type CaseSet struct {
	Cases []Case
}

// Filter 返回带某个 tag 的用例（tag 为空则返回全部）。
//
// ⚠️ 返回的是**新切片**，但它与原来的用例**共享 `Hints` 等 map** ——
//
//	评测用例是**只读**的，所以这里不深拷（与 `observ.Recorder.Events` 的
//	"要不要深拷，取决于拿到之后会不会有人改它"同一条）。
func (s CaseSet) Filter(tag string) CaseSet {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return s
	}
	out := make([]Case, 0, len(s.Cases))
	for _, c := range s.Cases {
		for _, t := range c.Tags {
			if t == tag {
				out = append(out, c)
				break
			}
		}
	}
	return CaseSet{Cases: out}
}

// ─── 读写 ───────────────────────────────────────────────────────────

// LoadCases 读一份 **JSONL** 用例集（一行一个用例）。
//
// # ⚠️ 为什么是 JSONL，而不是一个大 JSON 数组
//
//	因为**一行一个用例**让"加一条用例"变成一次**单行改动**：
//
//		· diff 里只看得到那一行（代码评审时能一眼看清加了什么）
//		· 两个人同时加用例**不会冲突**（一个大数组里几乎必然冲突）
//		· 坏一行**只坏那一行** —— 报错能指明行号（见下）
//
//	⭐ 这与 checkpoint 是"一个 run 一个文件"、录播是"一行一个事件"是同一条：
//	  **让存储的单位与"人操作的单位"对齐。**
//
// ⚠️ 空行与 `//` 开头的行被跳过 —— 于是用例集里**能写注释**
//
//	（一个"为什么这条用例长这样"的说明，比用例本身更该留下来）。
func LoadCases(path string) (CaseSet, error) {
	f, err := os.Open(path)
	if err != nil {
		return CaseSet{}, fmt.Errorf("打开用例集失败: %w", err)
	}
	defer f.Close()

	var out []Case
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 一行可能很长（一个用例）

	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "//") {
			continue
		}

		var c Case
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			// ⚠️ **必须报行号** —— "JSON 解析失败"在一个几十行的用例集里
			//	是没法排查的（而"发现问题却无从下手"等于没发现）。
			return CaseSet{}, fmt.Errorf("%s 第 %d 行: %w", filepath.Base(path), line, err)
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		return CaseSet{}, fmt.Errorf("读用例集失败: %w", err)
	}
	return CaseSet{Cases: out}, nil
}

// Validate 检查用例集**自洽**：ID 不重复、Topic 非空、判据不是空壳。
//
// # ⚠️ 为什么它必须在**跑之前**做
//
//	因为一个"期望全空"的用例**永远会过** —— 而它是**最危险的那一种**：
//
//		它绿着，于是没人去看它；
//		而它**什么都没验证**（一个本该失败的改动，它会放过）。
//
//	> ### 一个永远绿的用例，是一种**伪装成保障的空白**。
//	> （这与 R36 那条"否定形式的断言必须配正例前置"、"防空跑"是同一条。）
//
//	所以这里**强制**每个用例至少有一条硬判据 —— 做不到断言的话，
//	那个用例就该被删掉，而不是留着一个永远绿的。
func (s CaseSet) Validate() error {
	if len(s.Cases) == 0 {
		return fmt.Errorf("用例集是空的 —— 一次「什么都没跑」的评测会报出 100%% 的成功率")
	}

	seen := map[string]bool{}
	for i, c := range s.Cases {
		id := strings.TrimSpace(c.ID)
		if id == "" {
			return fmt.Errorf("第 %d 个用例没有 id", i+1)
		}
		if seen[id] {
			return fmt.Errorf("用例 id %q 重复 —— 报告里两个用例会同名，分不清是哪一个", id)
		}
		seen[id] = true
		if strings.TrimSpace(c.Topic) == "" {
			return fmt.Errorf("用例 %s 没有 topic", id)
		}
		if c.Expect.isEmpty() {
			return fmt.Errorf("用例 %s 的 expect **一条判据都没有** —— "+
				"它会**永远通过**，于是没人会去看它，而它什么都没验证。"+
				"请至少给一条硬判据（`pages` / `max_steps` / `must_contain` …）；"+
				"如果这个用例做不到任何断言，那它该被删掉", id)
		}
	}
	return nil
}

// isEmpty 报告"这个期望有没有任何一条判据"。
func (e Expectation) isEmpty() bool {
	return len(e.MustHaveKinds) == 0 &&
		e.MinFindings == 0 && e.Pages == 0 &&
		e.MaxSteps == 0 && e.MaxTokens == 0 && e.MaxWall == 0 &&
		len(e.MustContain) == 0 && len(e.MustNotContain) == 0
}

// Marshal 把用例集写回 JSONL（一行一个，**稳定顺序**）。
//
// ⚠️ 它存在的理由是"**能生成初始用例集**"（`cmd/l4 -dump`）——
//
//	手写 JSONL 很容易漏字段，而漏掉的那个字段会静默变成零值（= 不检查）。
func (s CaseSet) Marshal() ([]byte, error) {
	sorted := make([]Case, len(s.Cases))
	copy(sorted, s.Cases)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	var b strings.Builder
	for _, c := range sorted {
		raw, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}
