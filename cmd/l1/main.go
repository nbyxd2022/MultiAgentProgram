// Command l1 是 L1 阶段的 Demo：**生成 → 评审 → 返工** 闭环。
//
// 运行方式（无需任何密钥）：
//
//	go run ./cmd/l1
//	go run ./cmd/l1 新能源汽车行业趋势
//
// 与 L0 的差别只有一件事：**生成完不再直接交付，而是先被检查**。
//
//	① 生成       Generator Agent（复用 L0 的 agent.Agent，只换了 Prompt）
//	② 硬判据     tools.Check —— 代码，零 token、确定性
//	③ 软判据     agents.Critic —— LLM，只判代码判不了的
//	④ 合并判断   还有 blocker/major 吗？
//	⑤ 返工       把问题清单回灌给 Generator，重做
//	⑥ 停止       只剩建议级 / 严重问题数不再下降 / 达到轮数上限（三者先到）
//	⑦ 交付       **best-so-far**，而不是最后一版
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"multiagent/internal/agent"
	"multiagent/internal/agents"
	"multiagent/internal/boot"
	"multiagent/internal/llm"
	"multiagent/internal/message"
	"multiagent/internal/observ"
	"multiagent/internal/schema"
	"multiagent/internal/tool"
	"multiagent/internal/tools"
)

// maxRounds 是返工轮数的**硬兜底**（原则一：确定性兜底）。
//
// 没有它，一个"每轮都能挑出点问题"的 Critic 会让循环永远转下去——
// 而这正是 L1 复盘里 S3（请求卡死 / 计费暴涨）的雏形。
const maxRounds = 4

// reworkMarker / localFixMarker 只在离线 Mock 里用来识别"这一轮用哪种修复方式"。
// 真实模型不需要它们——它直接读得懂下面那两段修复要求。
const (
	reworkMarker   = "【返工要求】" // 整体重写
	localFixMarker = "【局部修复】" // 受限局部修复
)

// staleLimit 是"连续多少轮没能刷新历史最好"就判定为停滞。
//
// 为什么改成这样（**修 L1-2**）？
//
//	老判据是 `rep.HardCount() >= prevReport.HardCount()`，它有两个毛病：
//	  ① **丢掉了严重度**：3 个 blocker（总分 9）→ 3 个 major（总分 6），
//	     明明是巨大进步，却因为"条数没下降"被判成停滞，**提前终止了返工**；
//	  ② 比较对象是"上一轮"，而返工本来就会震荡。
//
// 新判据只依赖 Best：**连续 K 轮没能刷新历史最好**才算停滞，
// 而"更好"的定义直接复用 `schema.Report.BetterThan` ——
// 不再自己造一个"差不多的"判据（老 bug 的根因正是这个）。
//
// 对照企业仓库（layout_guard_repair.go:203-207）：它**没有**停滞判据，
// 只靠"轮数上限压到 2 + 严格变好才采纳 `score > bestScore`"。
// 我们保留一个极简的停滞闸门，目的只是省 token；轮数上限仍是硬兜底。
const staleLimit = 2

func main() {
	topic := "新能源汽车行业趋势"
	if len(os.Args) > 1 {
		topic = strings.Join(os.Args[1:], " ")
	}
	ctx := context.Background()

	// ── 组装（组合根：只有这里知道全部实现）────────────────
	renderer := tools.NewRenderSlideHTML("out")

	registry := tool.NewRegistry()
	registry.Register(tools.NewWebSearch())
	registry.Register(renderer)

	client := boot.NewClient(newGeneratorMock())
	generator := agent.New(agent.Options{
		Name:         "generator",
		Description:  "根据主题生成单页 HTML 幻灯片",
		SystemPrompt: generatorPrompt(),
		Client:       client,
		Tools:        registry,
		MaxSteps:     8,
		Tracer:       observ.NewConsoleTracer(os.Stdout),
	})

	critic := agents.NewCritic(agents.CriticOptions{
		Client: newCriticMock(),
		Tracer: observ.NewConsoleTracer(os.Stdout),
	})

	printHeader(topic, boot.Describe(client))

	// ── 返工循环 ────────────────────────────────────────────
	var best schema.Best[schema.Slide]
	var prev *schema.Slide
	var prevReport schema.Report
	var cost costSummary

	stale := 0 // 连续多少轮 best 没有被刷新
	stopReason := ""
	for round := 1; round <= maxRounds; round++ {
		fmt.Printf("\n──────── 第 %d 轮 ────────\n", round)

		// ── ① 产出候选版本：按"上一轮问题的可定位性"分流（原则七）──
		//
		//   有整体性硬问题（局部改解决不了）→ 整体重写；
		//   只有可精确定位的问题            → 受限局部修复。
		//
		// 对照企业仓库：它先穷尽 7 轮零成本的 QuickFix，再上 LLM 修复。
		// 我们把"零成本"那一段做成了下面的 ③，这里分流的是"LLM 该怎么改"。
		localFix := false
		var scope schema.FixScope

		prompt := firstPrompt(topic)
		if prev != nil {
			if prevReport.NeedsFullRewrite() {
				fmt.Println("[修复方式] 整体重写（存在无法精确定位的问题）")
				prompt = reworkPrompt(topic, *prev, prevReport)
			} else {
				localFix = true
				scope = prevReport.LocalTarget()
				fmt.Printf("[修复方式] 受限局部修复，只允许修改：%s\n",
					strings.Join(scope.Fields(), " / "))
				prompt = localFixPrompt(topic, *prev, prevReport, scope)
			}
		}

		if _, err := generator.Run(ctx, prompt); err != nil {
			fmt.Printf("[生成失败] %v\n", err)
			break
		}
		cost.addGenerator(generator.Stats())

		// 产物由**工具**交出，而不是由模型转述（不让概率模型搬数据）。
		slide, ok := renderer.LastSlide()
		if !ok {
			fmt.Println("[终止] 本轮没有产生任何产物（模型没有调用 render_slide_html）")
			break
		}

		// ── ② 受限合并：局部修复时，由**代码**强制改动范围 ──
		//
		// prompt 里写了"不要改其他字段"，但那只是建议；
		// 真正保证"只改该改的"的，是下面这一行（原则八）。
		// 对照企业仓库的 ReplaceLayoutHTML —— 只替换"可变区段"。
		if localFix {
			merged, violations := schema.MergeLocal(*prev, slide, scope)
			slide = merged
			printViolations(violations)
		}

		// ── ③ QuickFix：零 token 的确定性修复（原则七）──
		//
		// 对照企业仓库的 layout_guard_quickfix.go：先让代码把便宜的那部分吃掉，
		// LLM 才只需要处理真正需要判断的部分。
		if fixed, applied := tools.QuickFix(slide); len(applied) > 0 {
			slide = fixed
			fmt.Printf("[quickfix] 零成本修复：%s\n", strings.Join(applied, " / "))
		}

		rep, reviewErr := critic.Review(ctx, slide, round, fmt.Sprintf("第%d轮", round))
		cost.addCritic(critic.Stats())
		printReport(rep, reviewErr)

		// best-so-far：只在**更好**时更新，绝不无脑覆盖最后一版。
		if best.Observe(round, slide, rep) {
			stale = 0
			fmt.Printf("[best] 已更新为第 %d 轮（总分 %d）\n", round, rep.Score())
		} else {
			stale++
			fmt.Printf("[best] 第 %d 轮未超越第 %d 轮，保留旧版（连续无改进 %d 轮）\n",
				round, best.Round, stale)
		}

		// ── 停止条件（顺序即优先级）──
		//
		// 用 Deliverable() 而不是 !HasHardIssue()：
		// 前者同时要求"没有硬问题"**且**"评审完整"。
		// 只看后者，就会交付一个从未被语义评审过的产物（L1-3）。
		switch {
		case rep.Deliverable():
			stopReason = "已无致命/严重问题，且评审完整"
		case stale >= staleLimit:
			stopReason = fmt.Sprintf("连续 %d 轮没有变得更好（停滞）", stale)
		}
		if stopReason != "" {
			break
		}

		prev, prevReport = &slide, rep
	}
	if stopReason == "" {
		stopReason = fmt.Sprintf("达到返工上限 %d 轮（硬兜底）", maxRounds)
	}

	deliver(ctx, renderer, best, stopReason, cost)
}

// deliver 交付 best-so-far。
//
// 注意这里**故意不是"最后一版"**：返工可能震荡（Critic 让改成 A，
// 下一轮又让改回 B），最后一版经常比中间某一版更差。
func deliver(ctx context.Context, renderer *tools.RenderSlideHTML, best schema.Best[schema.Slide], stopReason string, cost costSummary) {
	fmt.Println("\n========== 交付 ==========")
	fmt.Printf("停止原因：%s\n", stopReason)

	cost.print()

	if !best.Has() {
		fmt.Println("\n没有任何可交付的版本。")
		return
	}

	fmt.Printf("\n交付版本：第 %d 轮 | %s\n", best.Round, best.Report)

	// 交付结论必须用 Deliverable()：它同时要求"没有硬问题"**且**"评审完整"。
	// 只说"没有硬问题"，就会把一个从没做过语义评审的产物说成合格品（L1-3）。
	if best.Report.Deliverable() {
		fmt.Println("这一版没有致命/严重问题，且评审完整。")
	} else {
		fmt.Println("注意：这一版**不能算「已验证合格」**——这是降级交付，不是失败。")
		if best.Report.Incomplete {
			fmt.Println("  原因：评审不完整 —— " + best.Report.IncompleteReason)
		}
		for _, line := range best.Report.Lines() {
			fmt.Println("  " + line)
		}
	}

	// 磁盘上的 HTML 已经被后续轮次**同名覆盖**了（P10 技术债：无版本化）。
	// L1 的临时解法：交付时把 best-so-far 重新渲染一遍，
	// 保证"交付的版本"与"磁盘上的文件"是同一个东西。
	// 真正的解法是版本化文件名 / 幂等键（→ L3）。
	payload, err := json.Marshal(best.Payload)
	if err != nil {
		fmt.Printf("[警告] 无法重新渲染：%v\n", err)
		return
	}
	out, err := renderer.Execute(ctx, string(payload))
	if err != nil {
		fmt.Printf("[警告] 重新渲染失败：%v\n", err)
		return
	}
	fmt.Printf("产物：%s\n", out)

	fmt.Println("\n最终要点：")
	for i, b := range best.Payload.Bullets {
		fmt.Printf("  %d. %s\n", i+1, b)
	}
}

// ─── Prompt（约束全部由 schema 常量生成）────────────────────────────

// generatorPrompt 由 schema 常量拼出来。
//
// 这就是"约束的单一真相来源"：改一次 schema.MinBullets，
// Prompt 与检查器同时生效——不可能再出现"Prompt 说 2-5 条、
// 检查器按 3-5 条判"的自相矛盾。
func generatorPrompt() string {
	return fmt.Sprintf(`你是"单页幻灯片生成助手"。

工作流程：
1. 先用 web_search 搜索与主题相关的素材（至少调用一次）；
2. 再调用 render_slide_html 生成一页幻灯片；
3. 不要臆造数据；如果搜索没有结果，就用通用的结构化建议。

版面约束（会被自动检查，违反会被打回重做）：
- 标题不超过 %d 个字；
- 要点 %d-%d 条，每条不超过 %d 个字；
- 每条要点必须有实际信息（可验证的判断、依据或行动），不要空话；
- 要点之间不要重复；
- 要点是纯文本，不要包含任何 HTML 标签。

全部完成后，用一句中文总结你做了什么。`,
		schema.MaxTitleRunes, schema.MinBullets, schema.MaxBullets, schema.MaxBulletRunes)
}

func firstPrompt(topic string) string {
	return fmt.Sprintf("主题：%s\n\n请按工作流程生成这一页幻灯片。", topic)
}

// reworkPrompt 是"返工要求"。
//
// 注意它**只带"上一版 + 问题清单"**，而不是把整个历史会话叠上去——
// 这是 L1 对 P1（上下文只增不减）的局部缓解：每轮返工开一次新会话。
// 真正的上下文管理（裁剪 / 摘要）留给 L2。
func reworkPrompt(topic string, prev schema.Slide, rep schema.Report) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("主题：%s\n\n", topic))
	sb.WriteString(reworkMarker + "\n")
	sb.WriteString("上一版没有通过检查，请修正后重新生成。\n\n")

	sb.WriteString("【上一版产物】\n")
	payload, _ := json.MarshalIndent(prev, "", "  ")
	sb.Write(payload)

	sb.WriteString("\n\n【必须修复的问题】（每条都给出了修法）\n")
	for _, line := range rep.Lines() {
		sb.WriteString("- " + line + "\n")
	}

	sb.WriteString("\n要求：只修问题，不要推翻没有问题的部分。重新调用 render_slide_html 输出完整的一版。")
	return sb.String()
}

// localFixPrompt 是"受限局部修复"指令（L1-1 的核心）。
//
// 与整体重写的关键差别：
//
//	· 明确列出**只允许修改的字段**；
//	· 明确要求其余字段**逐字保持不变**；
//	· 不重新搜索素材（改措辞不需要新素材，省一轮往返）。
//
// 为什么敢这么要求？因为**代码会强制执行**（schema.MergeLocal）——
// 越界的改动会被直接回退。所以这段 prompt 的作用只是"**减少浪费**"，
// 而不是"保证正确"。把约束交给代码、而不是模型的自觉，才是这里真正的设计（原则一）。
//
// 对照企业仓库：它的 LLM 修复同样只被允许改"layout 区段"，
// 其余部分由 ReplaceLayoutHTML 保留 —— 思路完全一致，只是"区段"的定义不同。
func localFixPrompt(topic string, prev schema.Slide, rep schema.Report, scope schema.FixScope) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("主题：%s\n\n", topic))
	sb.WriteString(localFixMarker + "\n")
	sb.WriteString("上一版只有少数几个字段需要修改，请**精确修改**，不要重写整页。\n\n")

	sb.WriteString("【上一版产物】\n")
	payload, _ := json.MarshalIndent(prev, "", "  ")
	sb.Write(payload)

	sb.WriteString("\n\n【只允许修改的字段】\n")
	for _, f := range scope.Fields() {
		sb.WriteString("- " + f + "\n")
	}

	sb.WriteString("\n【必须修复的问题】（每条都给出了修法）\n")
	for _, line := range rep.Lines() {
		sb.WriteString("- " + line + "\n")
	}

	sb.WriteString(fmt.Sprintf(`
【硬性要求】
1. 未被列出的字段必须**逐字保持不变**（不要"顺手优化"、不要改标点、不要调顺序）；
2. 要点数量必须保持 %d 条，不许增删；
3. 不需要重新搜索素材；
4. 改完后调用 render_slide_html 输出**完整的一版**（含未修改的字段）。
`, len(prev.Bullets)))

	return sb.String()
}

// printViolations 打印"修复范围校验"的结果。
//
// 这个输出本身就是教学素材：它让"模型会不会越界"变成**每次运行都能看见的事实**，
// 而不是一个需要读代码去相信的假设。
func printViolations(violations []string) {
	if len(violations) == 0 {
		fmt.Println("[merge] 范围校验通过：只有被点名的字段发生了变化")
		return
	}
	fmt.Printf("[merge] 修复者越界 %d 处，已按范围回退：\n", len(violations))
	for _, v := range violations {
		fmt.Println("        - " + v)
	}
}

// ─── 打印 ───────────────────────────────────────────────────────────

func printHeader(topic, model string) {
	fmt.Println("===== L1 Demo：生成 → 评审 → 返工 =====")
	fmt.Printf("主题     : %s\n", topic)
	fmt.Printf("模型     : %s\n", model)
	fmt.Printf("版面约束 : 标题≤%d 字，要点 %d-%d 条，每条≤%d 字（来自 schema 常量）\n",
		schema.MaxTitleRunes, schema.MinBullets, schema.MaxBullets, schema.MaxBulletRunes)
	fmt.Printf("返工上限 : %d 轮\n", maxRounds)
	fmt.Println("-----------------------------------------------------")
}

// ⚠️ 这里**曾经有一个 `printEvent`**（R46 删掉了）—— 它现在是 `observ.ConsoleTracer`。
//
//	它是同一段逻辑的**第二份**（`cmd/l0` 也有一份），而两份**格式还不一样**：
//
//		l0：  [步0][研究员] 调用工具: web_search  参数=…
//		l1：  [步0][生成者] 调用: web_search  …
//
//	> ### 两份**长得不一样**的复制品，是一个比"重复"更强的信号 ——
//	> 它说明这段逻辑**每次被抄的时候都在被改**，
//	> 而改它的人**没有去动那一份**（因为动它会牵连另一个命令）。
//	> 于是"哪个格式是对的"这个问题**永远不会有人回答**。
//
//	⭐ 现在它们都是 `ConsoleTracer` 的同一份格式（而它是**被测试钉住的**）。

// printReport 打印一轮的评审结论，并把"硬判据 / 软判据"的来源也显示出来。
func printReport(rep schema.Report, err error) {
	fmt.Printf("\n[评审] 总分 %d | 致命 %d / 严重 %d / 建议 %d\n",
		rep.Score(),
		rep.Count(schema.LevelBlocker),
		rep.Count(schema.LevelMajor),
		rep.Count(schema.LevelSuggestion))
	for _, line := range rep.Lines() {
		fmt.Println("   " + line)
	}

	// 评审失败 ≠ 没有问题。这里不再"打印一行警告就继续"，而是把状态显式说出来：
	// 这份报告已经被标记为不完整，**不可能**被判定为可交付。
	if rep.Incomplete {
		fmt.Printf("   [评审不完整] %s\n", rep.IncompleteReason)
		fmt.Println("   [影响] 本轮结论不完整，不会被判定为「可交付」")
	}
	if err != nil {
		fmt.Printf("   [详情] %v\n", err)
	}
}

// ─── 成本汇总（**修 L1-6**）──────────────────────────────────────────
//
// 为什么要打印这个？
//
//	L1 把最坏成本从 L0 的 1× 变成了「轮数上限 × 2 个 Agent」，
//	而 generator / critic 的 Stats 各算各的、**谁都没打印** —— 成本链路在这里断了。
//	**看不见的成本不可能被管住**：统计先于限制（真正的预算是 L4 的事）。
type costSummary struct {
	genPrompt, genCompletion, genSteps, genCalls int
	criticPrompt, criticCompletion, criticSteps  int
}

func (c *costSummary) addGenerator(s agent.Stats) {
	c.genPrompt += s.PromptTokens
	c.genCompletion += s.CompletionTokens
	c.genSteps += s.Steps
	c.genCalls += s.ToolCalls
}

func (c *costSummary) addCritic(s agent.Stats) {
	c.criticPrompt += s.PromptTokens
	c.criticCompletion += s.CompletionTokens
	c.criticSteps += s.Steps
}

func (c *costSummary) print() {
	total := func() int { return c.genPrompt + c.genCompletion + c.criticPrompt + c.criticCompletion }()

	fmt.Println("\n----- 成本汇总 -----")
	fmt.Printf("生成器 : %d 步 / %d 次工具调用 / prompt %d + completion %d\n",
		c.genSteps, c.genCalls, c.genPrompt, c.genCompletion)
	fmt.Printf("评审员 : %d 步 / prompt %d + completion %d\n",
		c.criticSteps, c.criticPrompt, c.criticCompletion)
	fmt.Printf("合计   : prompt %d + completion %d = %d tokens\n",
		c.genPrompt+c.criticPrompt, c.genCompletion+c.criticCompletion, total)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > 150 {
		return string([]rune(s)[:150]) + "..."
	}
	return s
}

// ─── 离线 Mock ──────────────────────────────────────────────────────
//
// ⚠️ 这些 Mock 不是"智能"的，只是脚本逻辑：靠"看上下文"决定下一步。
// 它们能验证"循环接得对不对"，但**验证不了 Prompt 写得好不好**（P4）。
// 真实效果必须接真实模型：
//
//	$env:MULTIAGENT_LLM="openai"; $env:OPENAI_API_KEY="sk-xxx"; go run ./cmd/l1

// defectBullets 是**人为注入的缺陷版本**，刻意做成"正好覆盖三级修复"：
//
//	6 条（超过上限 5）
//	1 条含 HTML 标签          → QuickFix 能修（剥离）
//	1 条剥标签后与上一条重复   → QuickFix 能修（去重）
//	1 条语义空洞              → 只能靠 LLM
//
// 特别注意：**去重顺带解决了"条数超限"**。
// 一个看起来非 LLM 不可的问题（条数超限），因为多出来的那条正好是重复的，
// 被代码一并解决了 —— 这就是"先穷尽便宜手段"的价值（原则七）。
var defectBullets = []string{
	"市场规模持续扩大，渗透率快速提升。",          // [0] 语义空洞 → critic 出手
	"<b>产业链成本下降</b>，电池价格进入下行通道。", // [1] 含 HTML → QuickFix 剥离
	"产业链成本下降，电池价格进入下行通道。",        // [2] 剥标签后与 [1] 重复 → QuickFix 去重
	"政策支持力度加大，补贴与路权双重驱动。",
	"充电基础设施加速铺设，缓解里程焦虑。",
	"出口成为新的增长极，海外销量翻倍增长。",
}

// localFixedBullets 是"受限局部修复"的结果：5 条，只修好 bullets[0]。
//
// ⚠️ 注意 bullets[4] 被**故意多改**了 —— 用来演示"修复者越界会被代码挡回"。
// 真实模型经常"顺手优化"没被点名的地方；这正是我们必须在代码里强制改动范围、
// 而不是相信 prompt 的原因（原则八）。
var localFixedBullets = []string{
	"结论：未来三年仍是增量市场，但增速将从 50% 回落到 20% 左右。", // ← 被点名：采纳
	"产业链成本下降，电池价格进入下行通道。",                 // ← 未点名：必须逐字相同
	"政策支持力度加大，补贴与路权双重驱动。",                 // ← 未点名：必须逐字相同
	"充电基础设施加速铺设，缓解里程焦虑。",                  // ← 未点名：必须逐字相同
	"出口增速高达 300%，是最亮眼的增长点。",               // ← 未点名却被改：会被回退
}

// fixedBullets 是"返工后"的版本：4 条、无重复、无 HTML、标点一致 → 零硬问题。
var fixedBullets = []string{
	"结论：未来三年仍是增量市场，但增速将从 50% 回落到 20% 左右。",
	"依据：渗透率已达 35%，产业链成本两年内下降 40%。",
	"行动：优先布局充电网络与售后体系，而不是继续扩产能。",
	"风险：补贴退坡与价格战可能压缩 2027 年的利润率。",
}

// looksLikeDefect 让 Critic Mock 判断"这一版是不是缺陷版"。
// 判定依据取自 defectBullets 本身，保证与注入的缺陷**同源**；
// 真实模型不需要这种锚点。
func looksLikeDefect(prompt string) bool {
	return strings.Contains(prompt, defectBullets[0])
}

func newGeneratorMock() llm.Client {
	return llm.NewFuncClient("l1-generator-mock",
		func(_ context.Context, _ int, req llm.Request) (*llm.Response, error) {
			prompt := lastUserText(req.Messages)
			topic := extractTopic(prompt)

			// 局部修复：**不重新搜索**（改措辞不需要新素材，省一轮往返），
			// 直接输出"只改被点名字段"的完整一版。
			if strings.Contains(prompt, localFixMarker) {
				if countRole(req.Messages, message.RoleTool) == 0 {
					return toolCallResponse("我只修改被点名的字段，其余保持原样。", "render_slide_html",
						mustJSON(map[string]any{"title": topic, "bullets": localFixedBullets}), 380, 130), nil
				}
				return finalResponse("已按指定字段修正。", 300, 24), nil
			}

			switch countRole(req.Messages, message.RoleTool) {
			case 0:
				return toolCallResponse("我先搜索一下相关素材。", "web_search",
					mustJSON(map[string]any{"query": topic, "top_k": 3}), 220, 40), nil

			case 1:
				if strings.Contains(prompt, reworkMarker) {
					return toolCallResponse("我按问题清单整体重写。", "render_slide_html",
						mustJSON(map[string]any{"title": topic, "bullets": fixedBullets}), 460, 130), nil
				}
				return toolCallResponse("素材够了，我来生成一页幻灯片。", "render_slide_html",
					mustJSON(map[string]any{"title": topic, "bullets": defectBullets}), 430, 150), nil

			default:
				return finalResponse("已完成：搜索了素材，并生成了 1 页 HTML 幻灯片。", 520, 36), nil
			}
		})
}

// finalResponse 构造一次"没有工具调用"的收尾回复。
func finalResponse(content string, promptTokens, completionTokens int) *llm.Response {
	return &llm.Response{
		Content: content,
		Usage:   llm.Usage{PromptTokens: promptTokens, CompletionTokens: completionTokens},
	}
}

func newCriticMock() llm.Client {
	// 故障注入开关：L1_CRITIC_FAIL=1 时评审员**永远不提交结论**。
	//
	// 用途：主动复现 L1-3 —— 一个"从未被语义评审过"的产物，
	// 会不会被当成合格品交付？（这是"没发现问题"和"没做检查"的区别）
	//
	//	$env:L1_CRITIC_FAIL="1"; go run ./cmd/l1
	failInjected := os.Getenv("L1_CRITIC_FAIL") == "1"

	return llm.NewFuncClient("l1-critic-mock",
		func(_ context.Context, _ int, req llm.Request) (*llm.Response, error) {
			if failInjected {
				return &llm.Response{
					Content: "（故障注入：评审员没有提交任何结论就结束了）",
					Usage:   llm.Usage{PromptTokens: 120, CompletionTokens: 20},
				}, nil
			}

			// 已经提交过结论 → 收尾。不再调工具，否则循环会一直转到 MaxSteps。
			if countToolResult(req.Messages, "submit_review") > 0 {
				return &llm.Response{
					Content: "评审完成。",
					Usage:   llm.Usage{PromptTokens: 160, CompletionTokens: 12},
				}, nil
			}

			if !looksLikeDefect(lastUserText(req.Messages)) {
				// 硬判据已经判干净了，就别硬凑问题（避免制造无意义返工）
				return toolCallResponse("硬判据没有发现问题，语义上我也没有发现。", "submit_review",
					`{"issues":[]}`, 300, 30), nil
			}

			return toolCallResponse("我发现一条语义问题，提交结论。", "submit_review",
				mustJSON(map[string]any{"issues": []map[string]string{{
					"level":   "major",
					"field":   "bullets[0]",
					"message": "要点偏口号，没有可验证的信息，无法支撑判断",
					"fix":     "按「结论 + 依据」的结构改写，并补一个可量化指标",
				}}}), 320, 70), nil
		})
}

// ─── 小工具 ─────────────────────────────────────────────────────────

func toolCallResponse(content, name, args string, promptTokens, completionTokens int) *llm.Response {
	return &llm.Response{
		Content: content,
		ToolCalls: []message.ToolCall{{
			ID: "call_" + name, Name: name, Arguments: args,
		}},
		Usage: llm.Usage{PromptTokens: promptTokens, CompletionTokens: completionTokens},
	}
}

// lastUserText 取最后一条 user 消息。
// 每轮都是一次全新的 Run，所以实际上只有一条。
func lastUserText(msgs []message.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}

func extractTopic(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		if v, ok := strings.CutPrefix(line, "主题："); ok {
			if t := strings.TrimSpace(v); t != "" {
				return t
			}
		}
	}
	return "未命名主题"
}

func countRole(msgs []message.Message, role message.Role) int {
	n := 0
	for _, m := range msgs {
		if m.Role == role {
			n++
		}
	}
	return n
}

// countToolResult 统计某个工具**已产出的结果条数**。
func countToolResult(msgs []message.Message, name string) int {
	n := 0
	for _, m := range msgs {
		if m.Role == message.RoleTool && m.Name == name {
			n++
		}
	}
	return n
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
