package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"multiagent/internal/llmjson"
)

// ─── 软判据（原理五）───────────────────────────────────────────────
//
// 「这段 HTML 好不好看」只能由模型判；「这一页有没有 3 个要点」必须由代码判。
//
// 而两者**不是"哪个更准"**，是"**哪个能给出确定答案**"：
//
//	硬判据  结果确定、可复现   失败 = **报错**
//	软判据  结果概率、有解释   失败 = **给出一个自信的错误评分**
//
// ⭐ 所以软判据的对策不是"更严"，是 **把它也当成一次运行**：
//   走同一个 `Tracer`、留 prompt/response —— 否则"它为什么给 7 分"**永远无法归因**。
//
// # ⚠️⚠️ 一个设计文档没有回答的问题：**软判据的分数算不算"通过"？**
//
//	答案是**不算** —— 而理由不是"它还不可靠"，是**它属于另一类东西**：
//
//		硬判据  人写的期望  → 确定  → **决定 `Verdict.Passed`**
//		软判据  模型给的分数 → 概率  → **只进报告，不进判定**
//
//	> ### 让一个概率值决定"通过/失败"，就是让评测**开始 flake**。
//	> 而 flake 的评测比没有评测**更坏** —— 它会训练人**忽略红色**。
//
//	这与 R23 那条"拿时间当判据的测试会 flake"是**同一族**：
//	判据的性质（确定 vs 概率）决定了它能被用在哪。
//
//	⭐ 而 §6 那条"不做"**独立印证**了它：
//	  "**Judge 用真实模型自动跑** —— 每一轮都花钱；而**判据的稳定性**要先在
//	   离线替身上验证 → 触发条件：用例集稳定之后（L5）"。
//
//	⚠️ 那么**它有什么用**？—— 它是**给人看的第二列信息**：
//	  "这次改动硬判据全过，而 `complete` 从 4 分掉到 2 分" —— **那是一条线索**，
//	  而人看到线索会去看产物。**线索不需要决定成败，它需要被看见。**

// Dimension 是软判据的**维度**（受控词表）。
//
// ⚠️ 受控的理由与 `observ.Kind` 完全相同（R39 那条判据）：
//
//	**强类型的必要性 = 拼错的后果有多重。**
//
//	维度是**会被聚合的**（"这次 `complete` 平均掉了几分"）——
//	而一个自创的 `Completeness` 会**静默地**另开一列，
//	于是"平均分"慢慢变成一堆各自只出现一次的维度。
type Dimension string

const (
	// DimFaithful 问"产物**忠于**主题与素材吗"（有没有编造 / 跑题）。
	DimFaithful Dimension = "faithful"

	// DimComplete 问"产物**完整**吗"（页数够不够、有没有半截的）。
	DimComplete Dimension = "complete"

	// DimReadable 问"产物**可读**吗"（结构清不清楚、有没有废话）。
	DimReadable Dimension = "readable"
)

// Dimensions 返回全部维度（**常量顺序**，不是 map 遍历 —— 顺序要稳定）。
//
// ⚠️ 默认全部维度都要打分 —— 因为"模型少给一个维度"与"那个维度 0 分"**不同**，
//
//	而"少给的算 0"会让缺项静默地拉低平均。
func Dimensions() []Dimension {
	return []Dimension{DimFaithful, DimComplete, DimReadable}
}

// Judgement 是一次软判据的结果。
type Judgement struct {
	Scores map[Dimension]int `json:"scores"`
	Reason string            `json:"reason"`
}

// Validate 校验一份软判据 —— 与 L0~L3 的 `SubmitTool` **同一条**：
// **输出结构化 + 代码校验**。
//
// # ⚠️ 三样都要校验，而每一样对应一种真实的失败
//
//	① **分数在范围内**（0~10）：模型给 `-1` 或 `100` 时，那个数会**静默地**
//	   污染平均值（一个 100 能把均值从 3 拉到 12）
//	② **理由非空**：一个没有理由的分数**无法归因** —— 而"无法归因"正是
//	   软判据最大的风险（它给出一个自信的错误评分，而你不知道它凭什么）
//	③ **不许自创维度**：见 `Dimension`
//
// ⭐ 而那三样**必须在代码里**，不能靠 prompt 里写"请给 0-10 分" ——
//
//	**prompt 是请求，校验是保证。** 两者都要有，而只有后者是硬的。
func (j Judgement) Validate() error {
	if len(j.Scores) == 0 {
		return fmt.Errorf("软判据没有任何维度分数 —— 与「期望全空」的用例同一个病：" +
			"它会永远\"通过\"，而它什么都没说")
	}

	known := map[Dimension]bool{}
	for _, d := range Dimensions() {
		known[d] = true
	}

	// ⚠️ 遍历前先排序：map 的遍历顺序是**随机的**，而"报哪个错"必须是确定的
	//	（否则同一个坏输入有时报 A、有时报 B —— 那是 flake 的另一个来源）。
	keys := make([]string, 0, len(j.Scores))
	for d := range j.Scores {
		keys = append(keys, string(d))
	}
	sort.Strings(keys)

	for _, k := range keys {
		d := Dimension(k)
		if !known[d] {
			return fmt.Errorf("维度 %q 不在受控词表里（可用：%s）—— "+
				"自创的维度会**静默地**另开一列，而聚合出来的平均分会慢慢变成一堆"+
				"各自只出现一次的维度", d, joinDims(Dimensions()))
		}
		if v := j.Scores[d]; v < 0 || v > 10 {
			return fmt.Errorf("维度 %q 的分数 %d 超出 0~10 —— "+
				"一个越界的分数会**静默地**污染平均值（一个 100 能把均值从 3 拉到 12）", d, v)
		}
	}
	if strings.TrimSpace(j.Reason) == "" {
		return fmt.Errorf("软判据没有给理由 —— 而它最大的风险恰恰是" +
			"**给出一个自信的错误评分**；没有理由，那个评分就**无法归因**")
	}
	return nil
}

func joinDims(ds []Dimension) string {
	ss := make([]string, 0, len(ds))
	for _, d := range ds {
		ss = append(ss, string(d))
	}
	return strings.Join(ss, " / ")
}

// ─── 端口 ───────────────────────────────────────────────────────────

// Judge 是软判据的**端口** —— 它只做一件事：**把 prompt 发出去，把文本收回来**。
//
// # ⚠️ 为什么它收的是 `prompt` 而不是 `Case`（这是对设计文档的一处偏离）
//
//	设计文档里写的是 `Judge(ctx, c Case, res Result) (Judgement, error)` ——
//	也就是"**实现自己造 prompt**"。
//
//	⭐ 而那样会把**"问什么"**交给实现 —— 可"**问什么**"恰恰是判据的定义：
//
//		问"这份 PPT 完整吗"      与问"这份 PPT 有没有创意"
//		是两个**完全不同**的判据，而它们都叫 `Judge`。
//
//	> ### prompt 是判据的一部分，不是实现的细节。
//
//	⚠️ 而"怎么发出去"（哪个模型 / 要不要重试 / 限流）才是实现的职责 ——
//	那与 `llm.Client` 的分工**一样**（它也不认识"角色人设"，只认识 `Request`）。
//
//	⭐ 顺带一个好处：这个端口**小到可以用一个函数实现** ——
//	  于是"离线替身"是几行，而判据的稳定性可以先在它上面验证（§6 那条不做）。
type Judge interface {
	Judge(ctx context.Context, prompt string) (string, error)
}

// JudgePrompt 造软判据的 prompt。
//
// ⚠️ 它**只喂最终产物**（`Rendered`）与主题 —— **不喂**中间产物。
//
//	理由与本包"`MustContain` 查 `Rendered` 而不是 `Board`"完全相同：
//	**判据要问的是"用户看到的那份东西好不好"** ——
//	把大纲喂进去会让模型根据"计划"评分，而不是根据"结果"。
func JudgePrompt(c Case, o Outcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "请评价这份演示文稿（主题：%s）。\n\n", c.Topic)
	b.WriteString("===== 页面内容 =====\n")
	for i, r := range o.Rendered {
		fmt.Fprintf(&b, "--- 第 %d 页 ---\n%s\n\n", i+1, r)
	}
	b.WriteString("===== 评分要求 =====\n")
	for _, d := range Dimensions() {
		fmt.Fprintf(&b, "- %s：0~10 分\n", d)
	}
	b.WriteString("\n只输出 JSON，不要解释：\n")
	b.WriteString(`{"scores":{"faithful":0,"complete":0,"readable":0},"reason":"为什么给这些分"}`)
	return b.String()
}

// ParseJudgement 解析模型的输出 —— **不信任它**。
//
// ⚠️ 它走 `llmjson.Clean`（R48）—— 因为**模型给的 JSON 是不可靠的**：
//
//	它常常在 JSON 外面加一句话（"好的，这是我的评价：{...}"），
//	而裸的 `json.Unmarshal` 会**整个失败**（一个"说得对但格式差"的输出被丢掉）。
//
//	> ### 解析变宽容，而**校验一条都不能少**（见 `Validate`）。
//	> 那是两件事：前者帮模型把话说完，后者是**判据本身**。
func ParseJudgement(text string) (Judgement, error) {
	var j Judgement
	cleaned := llmjson.Clean(text, false)
	if err := json.Unmarshal([]byte(cleaned), &j); err != nil {
		return Judgement{}, fmt.Errorf("软判据的输出不是合法 JSON（原文：%s）: %w",
			oneLine(text), err)
	}
	if err := j.Validate(); err != nil {
		return Judgement{}, err
	}
	return j, nil
}

// ─── 自愈 ───────────────────────────────────────────────────────────

// MaxJudgeAttempts 是"回灌自愈"的次数上限。
//
// ⚠️ 2 次（= 重试 1 次）—— 而那个数字的理由与 `agent.MaxSteps` 那条**不同**：
//
//	那里是"防打转"（成本），这里是"**格式错通常一次就能改对**"。
//	而一个改不过来的模型**再问十次也改不过来**（它不是在"试"，是**不会**）。
const MaxJudgeAttempts = 2

// judgeOne 跑一次软判据，**并在输出不合法时回灌自愈**。
//
// # ⚠️⚠️ "自愈"与 `retry` **不是一回事**（这一轮最容易混的地方）
//
//	retry.Do（R49）    **同一个请求**再发一次  → 治的是"网络抖动 / 限流"
//	自愈（这里）        **改了输入**再发一次    → 治的是"模型给的格式不对"
//
//	> ### 重试是"链路的问题"，自愈是"**输出的问题**"。
//	> 把后者当成前者，会得到"同一个坏格式被要了三遍" —— 而它三次都会是坏的。
//
//	⭐ 而这也解释了为什么 `retry` **替不了它**：`retry` 的 `fn` 是幂等的
//	  （同样输入同样输出），而自愈的全部要点**就是改变输入**。
//
// ⚠️ 而回灌的内容是**校验器的原话**（"维度 `x` 不在受控词表里"），
//
//	不是"你格式不对，请重试" —— 后者对模型**没有信息量**。
//	这与 L0 的 `SubmitTool` 回灌、以及 R48 那条"报错要说清哪一条 / 期望 / 实际"是同一条。
func judgeOne(ctx context.Context, j Judge, c Case, o Outcome) (Judgement, error) {
	prompt := JudgePrompt(c, o)

	var lastWhy string
	for attempt := 0; attempt < MaxJudgeAttempts; attempt++ {
		text, err := j.Judge(ctx, prompt)
		if err != nil {
			// ⚠️ **调用的失败不在这里重试** —— 那是 `retry` 的事（链路问题）。
			//	这里只负责"输出不合法"这一种。
			return Judgement{}, fmt.Errorf("软判据调用失败: %w", err)
		}

		got, err := ParseJudgement(text)
		if err == nil {
			return got, nil
		}

		lastWhy = err.Error()
		// ★ 回灌：把**校验器的原话**放进下一轮的输入
		prompt = JudgePrompt(c, o) +
			"\n\n===== 上一次的输出不合法 =====\n" + lastWhy +
			"\n\n请**只**输出合法的 JSON（维度只能用上面列出的那几个，分数 0~10，理由不能为空）。"
	}

	// ⚠️ 自愈失败**不是"判据没过"** —— 它没有给出任何分数。
	//	所以它既不进 `Passed`，也不该被伪装成"0 分"（那会污染平均值）。
	//	它是一条**独立的**信息："这一次的软判据没能用上"。
	return Judgement{}, fmt.Errorf("软判据连续 %d 次输出不合法（最后一次：%s）",
		MaxJudgeAttempts, lastWhy)
}
