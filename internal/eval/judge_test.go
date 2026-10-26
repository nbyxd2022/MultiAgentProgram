package eval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"multiagent/internal/schema"
)

// scriptedJudge 按脚本回答 —— 而且**记下收到过什么 prompt**（回灌要能被验证）。
type scriptedJudge struct {
	replies []string
	calls   int
	prompts []string
}

func (s *scriptedJudge) Judge(_ context.Context, prompt string) (string, error) {
	s.prompts = append(s.prompts, prompt)
	if s.calls >= len(s.replies) {
		return "", errors.New("脚本用完了")
	}
	r := s.replies[s.calls]
	s.calls++
	return r, nil
}

const goodJudgement = `{"scores":{"faithful":8,"complete":7,"readable":9},"reason":"结构清楚，素材贴题"}`

func judgeCase(id string, pages int) Case {
	return Case{ID: id, Topic: "主题", Expect: Expectation{Pages: pages}}
}

func goodOutcome(t *testing.T, pages int) Outcome {
	t.Helper()
	return outcomeWith(
		map[string][]schema.Artifact{string(schema.KindOutline): {outlineArtifact(t, pages)}},
		[]string{"<html>正文</html>"})
}

// ─── ★★★ 自愈：非法输出要**回灌重问**，而不是直接失败 ────────────────

// ⚠️ 这条守着"与 L0 的 `SubmitTool` 同一条"的设计（原理五 ②）：
// **输出必须结构化 + 代码校验** —— 而校验不过时走**回灌自愈**。
func TestJudgeSelfHealsOnInvalidOutput(t *testing.T) {
	j := &scriptedJudge{replies: []string{
		`{"scores":{"faithful":8,"creativity":5},"reason":""}`, // 自创维度 + 空理由
		goodJudgement, // 改对了
	}}
	o := goodOutcome(t, 3)

	vs := Run(context.Background(), CaseSet{Cases: []Case{judgeCase("c", 3)}},
		Options{Run: staticRunner(o, nil), Judge: j})

	v := vs[0]
	if v.Judgement == nil {
		t.Fatalf("回灌自愈之后应当拿到一份合法的判定，实际 JudgeErr=%v", v.JudgeErr)
	}
	if v.Judgement.Scores[DimFaithful] != 8 {
		t.Errorf("分数不对: %+v", v.Judgement.Scores)
	}
	if j.calls != 2 {
		t.Errorf("应当问了 2 次（第 1 次不合法 → 回灌），实际 %d 次", j.calls)
	}

	// ⚠️ **回灌的内容必须是"校验器的原话"** —— 不是"你格式不对，请重试"。
	//	后者对模型**没有信息量**，于是它大概率会再错一遍。
	//	（这与 L0 的 `SubmitTool` 回灌、R48 那条"报错要说清哪一条/期望/实际"是同一条。）
	second := j.prompts[1]
	for _, want := range []string{"creativity", "维度", "不合法"} {
		if !strings.Contains(second, want) {
			t.Errorf("回灌的 prompt 里应当带上校验器的原话（含 %q）—— "+
				"只说「请重试」等于什么都没说。实际 prompt：\n%s", want, second)
		}
	}
}

// ⚠️ 自愈失败**不是"0 分"** —— 把"没跑成"记成 0 分**会污染平均值**。
//
//	（这与 `countIn` 那条"真的 0 条 vs 读不出来"是同一个病：
//	 **两种东西长得一样时，数据会开始说谎**。）
func TestJudgeHealingFailureIsNotZeroScore(t *testing.T) {
	j := &scriptedJudge{replies: []string{
		`不是 JSON`, `还是不是 JSON`,
	}}
	o := goodOutcome(t, 3)

	vs := Run(context.Background(), CaseSet{Cases: []Case{judgeCase("c", 3)}},
		Options{Run: staticRunner(o, nil), Judge: j})

	v := vs[0]
	if v.JudgeErr == nil {
		t.Fatal("两次都不合法 → 应当报 JudgeErr")
	}
	if v.Judgement != nil {
		t.Errorf("没能拿到判定时 `Judgement` 该是 nil（**不是**一份全 0 的分数），实际 %+v", v.Judgement)
	}
	if j.calls != MaxJudgeAttempts {
		t.Errorf("应当试满 %d 次，实际 %d 次", MaxJudgeAttempts, j.calls)
	}
}

// ─── ★★★ 软判据**不参与** `Passed` ──────────────────────────────────

// ⚠️⚠️ 这是这一轮最重要的一个设计决定的看门人。
//
//	让一个**概率**值决定"通过/失败"，就是让评测**开始 flake**；
//	而 flake 的评测比没有评测**更坏**：它会训练人**忽略红色**。
//
//	⭐ 这条测试用**三个方向**证明它：
//	  ① 软判据失败（JudgeErr）—— `Passed` 不受影响
//	  ② 软判据给**极低分** —— `Passed` 不受影响
//	  ③ 硬判据不过 —— `Passed` 仍然是 false（**证明 `Passed` 真的在看硬判据**）
func TestJudgementNeverAffectsPassed(t *testing.T) {
	o := goodOutcome(t, 3) // 实际 3 页

	cases := []struct {
		name  string
		c     Case
		judge Judge
	}{
		{"软判据失败", judgeCase("x", 3), &scriptedJudge{replies: []string{"坏的", "还是坏的"}}},
		{"软判据给极低分", judgeCase("x", 3), &scriptedJudge{replies: []string{
			`{"scores":{"faithful":0,"complete":0,"readable":0},"reason":"很糟"}`}}},
	}

	for _, tc := range cases {
		vs := Run(context.Background(), CaseSet{Cases: []Case{tc.c}},
			Options{Run: staticRunner(o, nil), Judge: tc.judge})
		if !vs[0].Passed {
			t.Errorf("%s：**硬判据全过**，所以 `Passed` 必须为 true —— "+
				"若软判据能把它拉红，评测就**开始 flake** 了。实际失败：%v", tc.name, vs[0].Failed)
		}
	}

	// ③ 反方向：硬判据不过 → 必须红（否则上面那两条是恒真的）
	vs := Run(context.Background(), CaseSet{Cases: []Case{judgeCase("x", 99)}}, // 期望 99 页
		Options{Run: staticRunner(o, nil),
			Judge: &scriptedJudge{replies: []string{goodJudgement}}})
	if vs[0].Passed {
		t.Error("硬判据没过时 `Passed` 必须为 false —— 否则上面那两条测试什么都没证明")
	}
	if vs[0].Judgement == nil {
		t.Error("判据没过**不影响**软判据照常跑（它们互不干扰）")
	}
}

// ─── ★★ 校验：三样都要，而每一样对应一种真实的失败 ──────────────────

func TestJudgementValidate(t *testing.T) {
	cases := []struct {
		name string
		j    Judgement
		want string
	}{
		{"空分数", Judgement{Reason: "好"}, "没有任何维度"},
		{"自创维度", Judgement{Scores: map[Dimension]int{"creativity": 5}, Reason: "好"}, "受控词表"},
		{"分数越界", Judgement{Scores: map[Dimension]int{DimFaithful: 100}, Reason: "好"}, "超出 0~10"},
		{"负分", Judgement{Scores: map[Dimension]int{DimFaithful: -1}, Reason: "好"}, "超出 0~10"},
		{"空理由", Judgement{Scores: map[Dimension]int{DimFaithful: 8}}, "没有给理由"},
		{"全空白理由", Judgement{Scores: map[Dimension]int{DimFaithful: 8}, Reason: "   "}, "没有给理由"},
	}
	for _, c := range cases {
		err := c.j.Validate()
		if err == nil {
			t.Errorf("%s：应当被拒", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误该说清原因（含 %q），实际 %v", c.name, c.want, err)
		}
	}

	// ⚠️ **正例前置** —— 否则上面那一串是恒真的（一个永远报错的 Validate 也能满足）
	ok := Judgement{Scores: map[Dimension]int{DimFaithful: 8, DimComplete: 7}, Reason: "贴题"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法的判定不该被拒: %v", err)
	}
}

// ─── ★★ 解析：宽容语法，**不宽容语义** ──────────────────────────────

// ⚠️ 走 `llmjson`（R48）—— 因为**模型给的 JSON 是不可靠的**：
//
//	它常常在 JSON 外面加一句话，而裸 `Unmarshal` 会**整个失败**。
//
//	> 解析变宽容，而**校验一条都不能少** —— 那是两件事。
func TestParseJudgementIsLenientAboutSyntaxNotSemantics(t *testing.T) {
	// 语法差但内容对 → 应当解出来
	chatty := "好的，我的评价如下：" + goodJudgement + " 希望有帮助"
	got, err := ParseJudgement(chatty)
	if err != nil {
		t.Fatalf("混着自然语言的合法输出应当能解出来（这一层就是为此存在的）: %v", err)
	}
	if got.Scores[DimComplete] != 7 {
		t.Errorf("分数解错了: %+v", got.Scores)
	}

	// ⚠️ 而**语义差**的输出必须照拒 —— 解析宽容**不等于**判据变宽
	if _, err := ParseJudgement(`好的：{"scores":{"creativity":9},"reason":"x"}`); err == nil {
		t.Error("自创的维度必须照样被拒 —— 解析宽容**不等于**校验变宽")
	}
}

// ─── ★ `JudgePrompt` 只喂**最终产物** ───────────────────────────────

// ⚠️ 与本包"`MustContain` 查 `Rendered` 而不是 `Board`"**完全相同**的理由：
//
//	判据要问的是"**用户看到的那份东西好不好**"。
//	把大纲喂进去会让模型根据**计划**评分，而不是根据**结果** ——
//	而"计划得挺好、结果不行"恰恰是最该被软判据抓的那一类。
func TestJudgePromptFeedsRenderedNotBoard(t *testing.T) {
	// 黑板里有 3 页大纲；而**渲染出来的只有一句话**
	o := outcomeWith(
		map[string][]schema.Artifact{string(schema.KindOutline): {outlineArtifact(t, 3)}},
		[]string{"<html>只写了「待补充」</html>"})

	p := JudgePrompt(Case{ID: "x", Topic: "新能源"}, o)

	if !strings.Contains(p, "待补充") {
		t.Error("最终产物必须在 prompt 里 —— 它才是被评价的对象")
	}
	if !strings.Contains(p, "新能源") {
		t.Error("主题必须在 prompt 里（没有它就没法判「faithful」）")
	}
	// 受控词表要**写进 prompt** —— 否则"不许自创维度"这条校验是在**惩罚模型猜错**
	for _, d := range Dimensions() {
		if !strings.Contains(p, string(d)) {
			t.Errorf("prompt 里应当列出受控维度 %q —— "+
				"不告诉它可用的维度，却在事后拒绝它自创的，那不公平", d)
		}
	}
}
