package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	return p
}

// ⚠️ 解析失败**必须报行号**。
//
//	"JSON 解析失败"在一个几十行的用例集里是**没法排查的**——
//	而"发现问题却无从下手"等于没发现（这与 R44 的 `dumpToolResults` 同一条：
//	**让过程可读**）。
func TestLoadCasesReportsLineNumber(t *testing.T) {
	p := writeTemp(t, "cases.jsonl", `{"id":"好","topic":"t","expect":{"pages":3}}
{"id":"坏","topic"  这是一行坏 JSON
{"id":"后","topic":"t","expect":{"pages":3}}
`)

	_, err := LoadCases(p)
	if err == nil {
		t.Fatal("坏 JSON 应当报错")
	}
	for _, want := range []string{"第 2 行", "cases.jsonl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应有 %q（否则无从下手），实际：%v", want, err)
		}
	}
}

// ⚠️ 空行与 `//` 注释被跳过 —— 于是用例集里**能写注释**。
//
//	一条"为什么这条用例长这样"的说明，比用例本身**更该留下来**：
//	用例会因为某次重构而看起来过时，而**理由不会**。
func TestLoadCasesSkipsCommentsAndBlankLines(t *testing.T) {
	p := writeTemp(t, "cases.jsonl", `
// 这一条守的是"改 Prompt 时最容易碰坏的那个数"
{"id":"a","topic":"t","expect":{"pages":3}}

{"id":"b","topic":"t","expect":{"max_steps":12}}
`)

	set, err := LoadCases(p)
	if err != nil {
		t.Fatalf("注释与空行不该让解析失败: %v", err)
	}
	if len(set.Cases) != 2 {
		t.Fatalf("应当读到 2 个用例，实际 %d", len(set.Cases))
	}
	if err := set.Validate(); err != nil {
		t.Errorf("读出来的用例集应当自洽: %v", err)
	}
}

// ⚠️ 往返：`Marshal` 写出来的**必须能被 `LoadCases` 读回来**（逐字段一致）。
//
//	`Marshal` 存在的理由是"能生成初始用例集"（`cmd/l4 -dump`）——
//	而一个**写得出但读不回**的格式，会让那条命令产出一个
//	"看起来有用、一跑就报错"的文件。
func TestCaseSetRoundTrip(t *testing.T) {
	orig := CaseSet{Cases: []Case{
		{ID: "b", Topic: "主题二", Hints: map[string]string{"outline": "6 页"},
			Tags:   []string{"smoke"},
			Expect: Expectation{Pages: 6, MaxSteps: 12, MustContain: []string{"结论"}}},
		{ID: "a", Topic: "主题一", Expect: Expectation{MinFindings: 2, MustNotContain: []string{"{{"}}},
	}}

	raw, err := orig.Marshal()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	p := writeTemp(t, "cases.jsonl", string(raw))

	got, err := LoadCases(p)
	if err != nil {
		t.Fatalf("读回来失败: %v", err)
	}
	if len(got.Cases) != 2 {
		t.Fatalf("应当读到 2 个，实际 %d", len(got.Cases))
	}

	// ⚠️ `Marshal` 按 id 排序（**稳定顺序**）——
	//	顺序不稳定会让每次 `-dump` 都产生一个巨大的 diff，
	//	于是"这次改动到底改了什么"就看不见了。
	if got.Cases[0].ID != "a" || got.Cases[1].ID != "b" {
		t.Errorf("`Marshal` 应当按 id 排序（稳定顺序），实际 %s, %s",
			got.Cases[0].ID, got.Cases[1].ID)
	}

	byID := map[string]Case{}
	for _, c := range got.Cases {
		byID[c.ID] = c
	}
	b := byID["b"]
	switch {
	case b.Topic != "主题二":
		t.Errorf("topic 丢了: %q", b.Topic)
	case b.Expect.Pages != 6:
		t.Errorf("pages 丢了: %d", b.Expect.Pages)
	case len(b.Expect.MustContain) != 1 || b.Expect.MustContain[0] != "结论":
		t.Errorf("must_contain 丢了: %v", b.Expect.MustContain)
	case b.Hints["outline"] != "6 页":
		t.Errorf("hints 丢了: %v", b.Hints)
	case len(b.Tags) != 1 || b.Tags[0] != "smoke":
		t.Errorf("tags 丢了: %v", b.Tags)
	}
}

// 跑子集（"只跑 smoke"）—— 而**没有 tag 的用例不该被选中**。
func TestFilterByTag(t *testing.T) {
	set := CaseSet{Cases: []Case{
		{ID: "a", Tags: []string{"smoke", "slow"}},
		{ID: "b", Tags: []string{"slow"}},
		{ID: "c"}, // 没有 tag
	}}

	if got := set.Filter(""); len(got.Cases) != 3 {
		t.Errorf("空 tag = 全部，实际 %d 个", len(got.Cases))
	}
	if got := set.Filter("smoke"); len(got.Cases) != 1 || got.Cases[0].ID != "a" {
		t.Errorf("tag=smoke 应当只选中 a，实际 %v", got.Cases)
	}
	// ⚠️ **没有 tag 的用例在任何子集里都不出现** —— 它是"默认不跑"的。
	//	（而这是对的：一个默认被跑的用例，作者就不会去给它想一个 tag。）
	if got := set.Filter("slow"); len(got.Cases) != 2 {
		t.Errorf("tag=slow 应当选中 a 和 b，实际 %d 个", len(got.Cases))
	}
	if got := set.Filter("没这个tag"); len(got.Cases) != 0 {
		t.Errorf("不存在的 tag 应当选中 0 个，实际 %d 个", len(got.Cases))
	}
}

// ⚠️ 而 `Filter` 返回的用例**与原集合共享 map** —— 那是有意的（用例是**只读**的）。
//
//	这条测试把那个约定写下来，免得有人把返回值当成"可以随便改的副本"。
func TestFilterDoesNotDeepCopy(t *testing.T) {
	set := CaseSet{Cases: []Case{{ID: "a", Tags: []string{"smoke"}, Hints: map[string]string{"k": "v"}}}}
	got := set.Filter("smoke")

	if len(got.Cases) != 1 {
		t.Fatalf("应当选中 1 个")
	}
	// 改返回值里的 map —— 原来的**会跟着变**（共享，不深拷）
	got.Cases[0].Hints["k"] = "改了"
	if set.Cases[0].Hints["k"] != "改了" {
		t.Error("这里**刻意不深拷**（用例是只读的）—— 若你加了深拷，请同时改掉 case.go 里那条注释")
	}
}
