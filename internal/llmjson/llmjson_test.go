package llmjson

import (
	"encoding/json"
	"testing"
)

// ⚠️ 这一组守着"**抠得对**"—— 而它的核心是**引号内的括号不算结构**。
//
//	如果这一条错了，`{"a":"}"}` 会被抠成 `{"a":"}`（一个截断的串），
//	而下游报的错会是"JSON 不完整"—— **一个把人引向错误方向的信息**。
func TestExtractObjectRespectsStrings(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"纯对象", `{"a":1}`, `{"a":1}`},
		{"混着自然语言", `好的，这是结果：{"a":1} 希望有帮助`, `{"a":1}`},
		{"引号里的右括号", `{"a":"}"}`, `{"a":"}"}`},
		{"引号里的左括号", `{"a":"{"}`, `{"a":"{"}`},
		{"转义的引号", `{"a":"\""}`, `{"a":"\""}`},
		{"嵌套", `{"a":{"b":1}}`, `{"a":{"b":1}}`},
		{"前面有别的括号", `(注) {"a":1}`, `{"a":1}`},
		{"截断", `{"a":1`, ``},
		{"根本没有", `没有 JSON`, ``},
	}
	for _, c := range cases {
		if got := ExtractObject(c.in); got != c.want {
			t.Errorf("%s: ExtractObject(%q) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}
}

func TestExtractArray(t *testing.T) {
	if got := ExtractArray(`结果：[1,{"a":2}] 完毕`); got != `[1,{"a":2}]` {
		t.Errorf("数组抠错了: %q", got)
	}
	// ⚠️ 而"找对象"**不该**把数组当成对象 —— 两个函数各找各的
	if got := ExtractObject(`[1,2]`); got != `` {
		t.Errorf("只有数组时 ExtractObject 该返回空，实际 %q", got)
	}
}

// ⚠️ 这一组守着"**修得对，而且只修该修的**"。
//
//	最后两条是**最重要的**：它们守着"**修不了返回原文**"这条契约。
//	—— 一个返回空串或 `{}` 的修复会把"模型给了什么"这件事**擦掉**。
func TestRepair(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"尾逗号-对象", `{"a":1,}`, `{"a":1}`},
		{"尾逗号-数组", `{"a":[1,2,]}`, `{"a":[1,2]}`},
		// ⚠️ 逗号去掉，而**换行留着** —— 而这是对的，不是将就：
		//
		//	`{"a":1\n}` **是合法 JSON**（空白是允许的），
		//	而一个"修复"函数**只该修它声明要修的那一样**。
		//	顺手 trim / 重排会让它的行为**不可预测** ——
		//	下一个人会开始猜"它到底还会动什么"。
		{"尾逗号-空白保留", "{\"a\":1,\n}", "{\"a\":1\n}"},
		{"尾逗号-在字符串里不动", `{"a":","}`, `{"a":","}`},
		{"单引号", `{'a':1}`, `{"a":1}`},
		{"外面包了引号", `"{"a":1}"`, `{"a":1}`},
		{"本来就好", `{"a":1}`, `{"a":1}`},
		{"修不了就返回原文", `{坏的`, `{坏的`},
		{"空串原样", ``, ``},
	}
	for _, c := range cases {
		if got := Repair(c.in); got != c.want {
			t.Errorf("%s: Repair(%q) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}
}

// ⚠️ 端到端：`Clean` 之后的结果**必须能被 `json.Unmarshal` 解出来** ——
//
//	而这就是这一整包存在的理由（不然它只是一堆字符串函数）。
func TestCleanMakesUnmarshalable(t *testing.T) {
	cases := []struct {
		name, in string
		want     string
	}{
		{"混着自然语言", `当然可以！{"title":"封面",} 还需要别的吗？`, "封面"},
		{"单引号 + 尾逗号", `{'title':'封面',}`, "封面"},
		{"引号里带花括号", `结果：{"title":"a}b"}`, "a}b"},
	}
	for _, c := range cases {
		var got struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal([]byte(Clean(c.in, false)), &got); err != nil {
			t.Errorf("%s: Clean 之后仍解不出来（%v）：%q", c.name, err, Clean(c.in, false))
			continue
		}
		if got.Title != c.want {
			t.Errorf("%s: title = %q，期望 %q", c.name, got.Title, c.want)
		}
	}
}

// ⚠️ 顺序：**先 Extract 再 Repair**（包注释里那条）。
//
//	反过来的话，`Repair` 会把前面那段自然语言当成一个畸形字符串去修
//	（而"修"的副作用**不可预测**）—— 这条测试就是那个顺序的看门人。
func TestCleanExtractsBeforeRepairing(t *testing.T) {
	// 前缀里**故意**放一个单引号：如果先 Repair，它会被换成双引号，
	// 于是"引号配对"被打乱 —— 而先 Extract 就不会碰到它。
	in := `can't say more: {'a':1}`
	got := Clean(in, false)
	if got != `{"a":1}` {
		t.Errorf("顺序错了（应当先抠再修）：%q", got)
	}
}
