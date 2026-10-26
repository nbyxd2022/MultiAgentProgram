// Package llmjson 处理**模型返回的 JSON 不可靠**这件事。
//
// # ⚠️ 它抄自参考实现（R48），而抄的是一个**我们已经付过代价的洞**
//
//	`beautifulagent-go` 里有一个 `internal/util/llm_json.go`，而它被用在**三处**：
//
//		`agent_helper.repairToolArguments`  工具参数（**执行之前**修一次）
//		`util.RepairLLMJSON`                通用出口
//		`controller/chat.go` · `a2a.go`     最终响应解析
//
//	它的判据写在函数名里：**`Repair`**、**`Extract`** ——
//	也就是说，它**预期模型给的东西是不完整/畸形的**，所以多走一步。
//
//	⭐ 而我们没有这一层：`agents/*` 的 `decode` 直接 `json.Unmarshal`，
//	  于是模型多说一句话（"好的，这是结果：{...}"）就**整个解析失败**。
//
// # 两层，职责不同（**不要合并**）
//
//	Extract  从一段**混着自然语言的文本**里，抠出**第一个完整的** JSON 值
//	Repair   一个**长得像 JSON 但有小毛病**的串（尾逗号 / 单引号 / 缺引号）
//
//	⚠️ 顺序是 **先 Extract 再 Repair** —— 反过来的话，`Repair` 会把
//	\"好的，这是结果：\" 当成一个畸形的字符串去修，修出一些**谁也看不懂的东西**。
//
// # ⚠️ 而两条失败路径都**返回原文**，不返回空串
//
//	参考实现的契约（`RepairLLMJSON`）：
//
//		out, err := jsonrepair.RepairJSON(s)
//		if err != nil || out == "" {
//			return s          // ← **修不了就返回原文**
//		}
//
//	⭐ 而那是**比"返回空/默认值"更好的选择** —— 因为下游的报错里会带上
//	  **模型真正给的原文**，而不是一个"它什么都没给"的假象。
//
//	  这与 `cmd/l4` 的 `mustJSON` 里那句 `return "{}"` 是**相反**的 ——
//	  而 `{}` 恰好会掩盖真实输入（下一个人看不到模型到底给了什么）。
package llmjson

import "strings"

// ExtractObject 抠出**第一个完整的 JSON 对象**（`{...}`），找不到返回空串。
//
// ⚠️ 它尊重**双引号内的括号** —— `{"a":"}"}` 里的那个 `}` **不算**结构。
//
//	（这是"自己写状态机"而不用正则的全部理由：JSON 的嵌套与转义不是正则的语言。）
func ExtractObject(s string) string { return extractPair(s, '{', '}') }

// ExtractArray 抠出**第一个完整的 JSON 数组**（`[...]`），找不到返回空串。
func ExtractArray(s string) string { return extractPair(s, '[', ']') }

// extractPair 是两者的共同实现（`open`/`close` 是一对括号）。
//
// ⚠️ 两遍扫描，而不是一遍：
//
//	第一遍找**字符串外的**第一个 `open`（引号里的 `{` 不是结构）
//	第二遍从那里起做**配平**，同样跳过字符串与转义
//
//	一遍扫描写不出来，因为"我还没进结构"和"我在结构里"对字符的解释不同。
func extractPair(s string, open, close byte) string {
	start := indexFirstOutsideStrings(s, open)
	if start < 0 {
		return ""
	}
	return balancedFrom(s, start, open, close)
}

// indexFirstOutsideStrings 找**不在字符串里**的第一个 `want`，找不到返回 -1。
func indexFirstOutsideStrings(s string, want byte) int {
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case want:
			return i
		}
	}
	return -1
}

// balancedFrom 从 `start`（必须是 `open`）起找到**配平**的那一对，返回含首尾的子串。
//
// ⚠️ 找不到配平就返回**空串**（而不是"从 start 到结尾"）——
//
//	截断的 JSON 交给 `Repair` 去救，而不是在这里假装它是完整的。
func balancedFrom(s string, start int, open, close byte) string {
	if start < 0 || start >= len(s) || s[start] != open {
		return ""
	}
	depth := 0
	inString, escaped := false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// Repair 修**常见的几种**模型 JSON 畸变；修不了（或没变化）就返回原文。
//
// # ⚠️ 它覆盖哪些、不覆盖哪些（**写清楚，否则下一个人会以为它万能**）
//
//	覆盖（都是**实测高频**的）：
//
//		尾逗号          {"a":1,}            → {"a":1}
//		单引号          {'a':1}             → {"a":1}
//		大括号外多余引号  "{...}\""           → {...}     （只剥首尾一层）
//
//	**不覆盖**（修不了就返回原文，让下游按原样报错）：
//
//		缺逗号 / 缺引号 / 截断 / 注释 / NaN / 十六进制 …
//
//	> ### 参考实现用了一个第三方库（`jsonrepair`）做**完整**的修复。
//	> 我们**不引**（本项目的基调是标准库之上）—— 所以只做那几种高频的。
//	> ⚠️ **而这个取舍必须写在这里**：一个"看起来能修一切"的函数
//	>   比一个"写清了只修三种"的函数**危险得多**（前者会让人放弃去看原文）。
func Repair(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	out := s

	// ① 剥掉 "..." 最外层的引号（模型有时把整个对象当字符串返回）
	if len(out) >= 2 && out[0] == '"' && out[len(out)-1] == '"' &&
		strings.ContainsAny(out[1:len(out)-1], "{}[]") {
		out = out[1 : len(out)-1]
	}

	// ② 单引号 → 双引号（只在**字符串外**替换，否则会改到内容里）
	out = singleToDoubleQuotes(out)

	// ③ 尾逗号（`,}` / `,]`，允许中间有空白）
	out = stripTrailingCommas(out)

	return out
}

// singleToDoubleQuotes 把**字符串定界用的**单引号换成双引号。
//
// ⚠️ 它不做"内容里的单引号"这种聪明事 —— 它假设**整个串用的是单引号**，
//
//	而那样替换一定是安全的（因为双引号在那种串里是内容，换完仍是内容）。
//
//	⭐ 判据：**能区分"定界符"和"内容"的语言不是字符串替换** ——
//	  所以这里只做那一种**不可能错**的情况（全单引号），
//	  其余一律交给 `Repair` 的"修不了返回原文"。
func singleToDoubleQuotes(s string) string {
	if !strings.Contains(s, "'") || strings.Contains(s, `"`) {
		return s
	}
	return strings.ReplaceAll(s, "'", `"`)
}

// stripTrailingCommas 去掉 `,` 紧跟 `}` 或 `]` 的情况（中间允许空白）。
func stripTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString, escaped := false, false

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		}

		if !inString && c == ',' && onlySpaceUntilCloser(s[i+1:]) {
			continue // 丢掉这个逗号
		}
		b.WriteByte(c)
	}
	return b.String()
}

// onlySpaceUntilCloser 报告 `s` 是否"只由空白组成、直到一个 `}` 或 `]`"。
func onlySpaceUntilCloser(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			continue
		case '}', ']':
			return true
		default:
			return false
		}
	}
	return false
}

// Clean 是**给调用方用的那一个**：先 `Extract` 再 `Repair`，最后仍是原文兜底。
//
// ⚠️ 顺序不能反 —— 见包注释。
//
//	kind 决定抠对象还是抠数组：
//		对象（`{...}`）→ 角色的提交参数、聊天响应
//		数组（`[...]`）→ 有些模型会把列表单独给出来
//
// ⭐ 它**不保证**结果是合法 JSON —— 它只保证"**尽力了，而且没丢掉原文**"。
//
//	调用方仍要用 `json.Unmarshal` 去判成败（那一步不能省）。
func Clean(s string, wantArray bool) string {
	var got string
	if wantArray {
		got = ExtractArray(s)
	} else {
		got = ExtractObject(s)
	}
	if got == "" {
		got = s // 抠不出来 → 用原文（交给 `Repair` 与 `Unmarshal`）
	}
	return Repair(got)
}
