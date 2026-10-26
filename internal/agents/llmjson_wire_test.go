package agents

import (
	"encoding/json"
	"testing"

	"multiagent/internal/schema"
)

// ⚠️ 这组守的是"**接线生效了**" —— 而**不是** `llmjson` 本身
// （那个包有自己的单元测试，边界情形在那里测）。
//
// # 它必须能红 —— 而这是它存在的全部理由
//
//	把 5 个 `decode` 里的 `llmjson.Clean(args, false)` 换回裸的 `[]byte(args)`，
//	下面每一条**立刻失败**。R42 立的判据是"**能不能写出一条会失败的测试**"——
//	这条能，所以它不是装饰。
//
// # 为什么"接线"要有自己的测试
//
//	因为 `llmjson` **全绿**和"它被接上了"是**两件事** ——
//	R43 那条教训（`budget` 满绿的测试 + **零个导入者**）就是这么来的。
//
//	> **能力在 ≠ 能力通。** 一个包可以有满绿的测试，而没有一个调用点。
func TestDecodeToleratesChattyLLMOutput(t *testing.T) {
	// 一份**合法**的提交（最小：1 条带来源的结论）
	const good = `{"queries":["新能源汽车"],"findings":[{"claim":"渗透率 35%","source":"web_search#1"}]}`

	cases := []struct {
		name string
		args string
		// fresh 表示"**只有加了 `llmjson` 这一层，它才可能过**"。
		//
		// ⚠️ 这一个字段是**被一次失败逼出来的**：
		//	我一开始给所有 case 都加了"裸解析必须失败"的断言，
		//	而它**立刻抓住了 `纯 JSON` 那一条** —— 它裸解析就能过，
		//	于是它**什么都没验证**。
		//
		//	> 而修法不是删掉它：它守的是**另一个方向** ——
		//	> **`Clean` 不能把一个合法的 JSON 改坏**。
		//	> 两条用例的**意图不同**，所以要用一个字段把它们**说开**。
		fresh bool
	}{
		{"纯 JSON", good, false}, // ← 回归：`Clean` 不该把它改坏
		{"前面有自然语言", "好的，我查完了：" + good + " 还需要我做什么？", true},
		{"整个被当成字符串返回", `"` + good + `"`, true},
		{"混着自然语言 + 尾逗号",
			"结果如下：{\"queries\":[\"新能源汽车\"],\"findings\":[{\"claim\":\"渗透率 35%\",\"source\":\"web_search#1\"},]}", true},
	}

	for _, c := range cases {
		// ⚠️ **先证明"没有那一层它会失败"** —— 否则这条测试可能是**恒真**的。
		//
		//	R42 立的判据是"能不能写出一条会失败的测试"，而那件事**可以内建**：
		//	与其手工把 `Clean` 摘掉跑一次，不如让测试**每次自己验一遍**。
		//
		//	> 一条不能红的测试，是一种**伪装成保障的空白**（R36）。
		//	> 而"它能红"这个事实，最好也**由代码来守**，而不是靠一次记忆。
		bare := json.Unmarshal([]byte(c.args), &struct{}{})
		if c.fresh && bare == nil {
			t.Fatalf("%s：这个参数**裸解析**居然成功了 —— 那这条用例什么都没验证。"+
				"请换一个真的需要 `llmjson` 的例子", c.name)
		}
		if !c.fresh && bare != nil {
			t.Fatalf("%s：标着「回归」的用例**必须裸解析就能过**（它守的是另一个方向）", c.name)
		}

		got, err := decodeResearch("新能源汽车行业趋势", c.args)
		if err != nil {
			t.Errorf("%s：应当被解出来（这一层就是为此存在的），实际 %v\n  原始参数：%q",
				c.name, err, c.args)
			continue
		}
		r, ok := got.(schema.Research)
		if !ok {
			t.Errorf("%s：产物类型不对 %T", c.name, got)
			continue
		}
		if len(r.Findings) != 1 {
			t.Errorf("%s：应当解出 1 条结论，实际 %d 条", c.name, len(r.Findings))
		}
	}
}

// ⚠️ 而"宽容"**只该发生在语法层** —— **语义校验一条都不能少**。
//
//	这一条守着那个边界：混着自然语言的文本**能解出来**，
//	但解出来的内容**照样要过**角色自己的硬约束（无来源的断言进不来）。
//
//	> 宽容一个"更宽松的解析"和宽容一个"更宽松的规则"是**两件事**。
//	> 前者是帮模型把话说完；后者是**放弃判据**。
func TestCleanDoesNotWeakenSemanticChecks(t *testing.T) {
	// 结论文本混着自然语言 —— 而它**缺 source**，必须照样被拒
	chatty := "好的：{\"findings\":[{\"claim\":\"没有来源的说法\"}]}"
	if _, err := decodeResearch("主题", chatty); err == nil {
		t.Fatal("解析变宽容了，而**语义校验**不该跟着变宽 —— " +
			"没有来源的断言必须照样进不来（L1-5）")
	}
}
