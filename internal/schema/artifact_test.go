package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleResearch() Research {
	return Research{
		Topic:   "新能源汽车行业趋势",
		Queries: []string{"新能源汽车 渗透率", "动力电池 成本"},
		Findings: []Finding{
			{Claim: "渗透率已达 35%。", Source: "web_search#1"},
			{Claim: "电池成本两年内下降 40%。", Source: "web_search#2"},
		},
	}
}

// ─── 打包：Kind 只能来自产物自己 ────────────────────────────────────

func TestNewArtifactTakesKindFromPayload(t *testing.T) {
	a, err := NewArtifact("research-1", "research", sampleResearch())
	if err != nil {
		t.Fatalf("打包失败: %v", err)
	}

	if a.Kind != KindResearch {
		t.Errorf("Kind 应取自产物自身，实际 %q", a.Kind)
	}
	if a.Summary == "" {
		t.Error("摘要必须由代码派生，不能为空")
	}
	if a.Version != 0 {
		t.Errorf("版本由黑板分配，NewArtifact 不应赋值，实际 %d", a.Version)
	}
	if len(a.Payload) == 0 {
		t.Error("payload 不能为空")
	}
}

func TestNewArtifactRecordsProducer(t *testing.T) {
	a, _ := NewArtifact("outline-1", "  outline  ", sampleResearch())
	if a.ProducedBy != "outline" {
		t.Errorf("产出者应被去掉首尾空白，实际 %q", a.ProducedBy)
	}
}

// ─── 校验 ───────────────────────────────────────────────────────────

func TestArtifactValidate(t *testing.T) {
	good, _ := NewArtifact("k", "research", sampleResearch())
	good.Version = 1

	if err := good.Validate(); err != nil {
		t.Fatalf("合法产物不应报错: %v", err)
	}

	cases := []struct {
		name string
		mut  func(Artifact) Artifact
	}{
		{"缺少 key", func(a Artifact) Artifact { a.Key = "  "; return a }},
		{"kind 非法", func(a Artifact) Artifact { a.Kind = "whatever"; return a }},
		{"版本未分配", func(a Artifact) Artifact { a.Version = 0; return a }},
		{"payload 为空", func(a Artifact) Artifact { a.Payload = nil; return a }},
	}
	for _, c := range cases {
		if err := c.mut(good).Validate(); err == nil {
			t.Errorf("%s：应当校验失败", c.name)
		}
	}
}

func TestKindValidRejectsUnknown(t *testing.T) {
	for _, k := range []Kind{KindResearch, KindOutline, KindDesign, KindRender} {
		if !k.Valid() {
			t.Errorf("%q 应当合法", k)
		}
	}
	for _, k := range []Kind{"", "supervisor", "Research"} {
		if k.Valid() {
			t.Errorf("%q 不应合法（脏数据必须被挡住）", k)
		}
	}
}

// ─── 解码：双向校验 Kind ────────────────────────────────────────────

func TestDecodeRoundTrip(t *testing.T) {
	orig := sampleResearch()
	a, _ := NewArtifact("research-1", "research", orig)
	a.Version = 1

	got, err := Decode[Research](a)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if got.Topic != orig.Topic || len(got.Findings) != len(orig.Findings) {
		t.Errorf("解码结果与原文不一致：%+v", got)
	}
	if got.Findings[0].Source != orig.Findings[0].Source {
		t.Error("Source 字段丢失 —— 这正是 L1-5 要防的（断言无锚点）")
	}
}

// TestDecodeDetectsKindMismatch 守住最隐蔽的一类故障。
//
// 把 Outline 装进 Kind=research 的信封里：类型没报错、字段都在，
// 但下游会照着错误的假设去读字段 —— 语义全错，而且不报错。
func TestDecodeDetectsKindMismatch(t *testing.T) {
	raw, _ := json.Marshal(Outline{Pages: []PageOutline{{Index: 0, Title: "封面"}}})

	mismatched := Artifact{
		Key: "outline-1", Kind: KindResearch, Version: 1, Payload: raw,
	}

	if _, err := Decode[Outline](mismatched); err == nil {
		t.Fatal("信封与 payload 的 kind 不一致时，必须报错")
	}
}

func TestDecodeReportsBadPayload(t *testing.T) {
	bad := Artifact{Key: "k", Kind: KindResearch, Version: 1, Payload: json.RawMessage(`{"topic":`)}
	if _, err := Decode[Research](bad); err == nil {
		t.Fatal("payload 无法解析时必须报错")
	}
}

// ─── 摘要：只投影形状，不含正文 ──────────────────────────────────────

func TestSummarizeProjectsShapeOnly(t *testing.T) {
	r := sampleResearch()
	s := r.Summarize()

	if !strings.Contains(s, "2 条查询") || !strings.Contains(s, "2 条发现") {
		t.Errorf("摘要应包含形状信息（数量），实际 %q", s)
	}
	if strings.Contains(s, "渗透率已达") {
		t.Errorf("摘要里不应出现正文（正文属于 payload，摘要必须小），实际 %q", s)
	}
	if !strings.Contains(s, r.Topic) {
		t.Errorf("摘要应能让人认出这是哪个主题，实际 %q", s)
	}
}

func TestOutlineSummarizeListsPageTitles(t *testing.T) {
	o := Outline{Pages: []PageOutline{
		{Title: "封面"}, {Title: "行业现状"}, {Title: "结论与建议"},
	}}
	s := o.Summarize()

	if !strings.Contains(s, "3 页") {
		t.Errorf("应报出页数，实际 %q", s)
	}
	if !strings.Contains(s, "封面") || !strings.Contains(s, "结论与建议") {
		t.Errorf("设计者需要知道各页标题，实际 %q", s)
	}
}

func TestSummarizeCapsLength(t *testing.T) {
	pages := make([]PageOutline, 0, 20)
	for i := 0; i < 20; i++ {
		pages = append(pages, PageOutline{Title: strings.Repeat("长标题", 5)})
	}
	s := (Outline{Pages: pages}).Summarize()

	if n := len([]rune(s)); n > 140 {
		t.Errorf("摘要必须被压在上限内，实际 %d 字：%s", n, s)
	}
	if !strings.Contains(s, "...") {
		t.Errorf("超出上限时应出现截断标记，实际 %q", s)
	}
}

// ─── handoff：只有 ref + summary ────────────────────────────────────

func TestHandoffCarriesNoPayload(t *testing.T) {
	a, _ := NewArtifact("research-1", "research", sampleResearch())
	a.Version = 3

	h := HandoffOf(a)

	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	// handoff 是要塞进上下文的东西 —— 正文绝不能跟着走（原则四）
	if strings.Contains(string(raw), "渗透率已达") {
		t.Errorf("handoff 里出现了正文，违反「上下文只放最小信息」：%s", raw)
	}
	if h.Ref != "research-1@3" {
		t.Errorf("引用必须带版本（否则会指向漂移），实际 %q", h.Ref)
	}
	if h.Summary == "" {
		t.Error("handoff 必须带摘要")
	}
}

func TestRefOfIncludesVersion(t *testing.T) {
	a, _ := NewArtifact("outline", "outline", Outline{})
	a.Version = 2

	if got, want := RefOf(a), "outline@2"; got != want {
		t.Errorf("RefOf() = %q, want %q", got, want)
	}
}
