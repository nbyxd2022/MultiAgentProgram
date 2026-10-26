// Package agent 定义 Agent 抽象与 ReAct 循环。
//
// Agent 的本质：
//
//	一个「带工具、带记忆、带终止条件」的模型调用循环。
//
// 我们刻意不引入任何框架，把循环写出来，是为了理解：
//   - 循环为什么一定会终止（自然结束 + 硬上限双保险）；
//   - 工具错误为什么要回灌给模型，而不是抛给用户；
//   - 上下文是怎么一步步变长、最终必须被管理的。
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"multiagent/internal/budget"
	"multiagent/internal/llm"
	"multiagent/internal/message"
	"multiagent/internal/observ"
	"multiagent/internal/retry"
	"multiagent/internal/tool"
)

// Stats 是一次运行的统计信息（成本与行为的量化基础）。
type Stats struct {
	Steps            int
	ToolCalls        int
	PromptTokens     int
	CompletionTokens int
}

// ⚠️ 这里**曾经有一个 `agent.Event`**（R46 删掉了）。
//
// # 它去哪了，以及这次迁移和 R44 那次（`graph.Event`）的**差别**
//
//	两次迁移用的是**同一条判据**（"这个字段能被算出来吗"），而结论**不同**：
//
//		graph.Event     `Step` **能算**（数 `KindStart`）        → **删**
//		agent.Event     `Step` **也能算**（同一条）             → **删**
//		                `ToolName` / `ToolArgs` **不能算**      → **保留**（进 observ）
//
//	⭐ 而 R46 比 R44 多知道的一件事是：**"不能算"只是"该保留"的一半条件**。
//	  另一半是 **"它属于哪一侧"** —— 而 `observ` 的字段命名约定
//	  （`Redact` 的白名单 + 那条反射看门人）**替这次迁移回答了它**：
//
//		`ToolName` 不在正文词表（`Prompt`/`Response`/`Args`/`Payload`/`Body`/`Content`）里
//			→ **元信息** → 必须被 `Redact` 保留
//		`ToolArgs` 命中 "Args"
//			→ **正文** → 必须被 `Redact` 清空
//
//	**名字一写下来，那一侧就定了** —— 这正是那条约定的价值。

// Agent 是一个最小可用的 ReAct Agent。
type Agent struct {
	Name         string
	Description  string
	SystemPrompt string
	Client       llm.Client
	Tools        *tool.Registry

	// MaxSteps 是循环步数的硬上限，防止模型不收敛导致成本失控。
	MaxSteps int

	// rec 是**内建的累积出口** —— 它是 `Stats()` 的来源（见 `Stats`）。
	//
	// ⚠️ 它对应参考实现的 `requestTokenAccum`：那边也是**一个内建的累加器**
	//	（`addTokens` 是唯一的累加点），而 Prometheus 的 `agent_*` 与 `request_*`
	//	**两套指标都从它派生**。
	rec *observ.Recorder

	// tracer 已经归一化过：**永不为 nil**（`Fanout` 跳过 nil，而 `rec` 非 nil）。
	tracer observ.Tracer

	// retry 是"**一次模型调用**失败后怎么办"的策略。
	//
	// ⚠️ 零值是**可用的**（试一次，不退避）—— 于是"没配"= "不重试"，
	//	而那是一个**安全的默认**（重试会花钱，默认不该偷偷花）。
	retry retry.Policy

	// guard 是预算守卫（永不为 nil）。
	//
	// 它与 Runner 那个是同一个对象，而那正是「谁看见用量，谁报」：
	//
	//	Runner 报 步数 + 执行时间（它知道跑了几跳、每跳多久）
	//	Agent  报 prompt + completion（只有它看得见模型回了多少）
	//
	// ⚠️ 而靠它拦住这一次是做不到的：token 要跑完才知道，只能拦住下一次。
	//
	// ⭐ 而参考实现更干脆：它根本不用 token 做事前检查 —— 它的 Limiter 拦的是
	//	页数 / 次数（AllowN(requestPages)），而那两个量事前可知。
	//
	//	一个量事前不可知，就不该被放进事前检查里。
	//
	// 我们的 Guard 是累计预算（不是事前配额），所以它可以晚一步 —— 但要有数。
	guard budget.Guard
}

// Options 是构造 Agent 的参数。
type Options struct {
	Name         string
	Description  string
	SystemPrompt string
	Client       llm.Client
	Tools        *tool.Registry
	MaxSteps     int

	// Tracer 接事件。可以为 nil（那等于没有外部出口）。
	Tracer observ.Tracer

	// Retry 是**一次模型调用**的重试策略。
	//
	// ⚠️ 零值 = **试一次**（不重试）—— 见 `retry.Do` 的归一化。
	//
	//	判据：重试会**花钱**，所以"默认重试"是一个**替用户决定花钱**的默认值。
	//	而不重试只是"这一次失败得干脆"—— 代价小得多，且用户看得见。
	//
	//	⭐ 参考实现的那个 `Retry` **没有 ctx、也没有退避** ——
	//	  而那个设计意味着"限流时立刻重试三次"（几乎一定再次被限流）。
	//	  我们把两样都补上了，理由写在 `retry` 包注释里。
	Retry retry.Policy

	// Guard 是预算守卫。可以为 nil -> 归一成 Unlimited。
	//
	// ⚠️ 它该与 RunnerOptions.Guard 是同一个对象 —— 否则步数与 token 会记在
	//	两本互不相识的账上（而「同一个预算」就成了两个预算）。
	//
	// ⚠️ 而这件事现在没有任何东西保证（装配方传两个不同的 Guard 也能跑）——
	//	登记为待办，触发条件是「出现第二个装配方」。
	Guard budget.Guard
}

// New 构造一个 Agent。
func New(opt Options) *Agent {
	if opt.Guard == nil {
		// 归一化（而不是判空）—— 与 Tracer / RunnerOptions.Guard 同一个手法：
		// 不限额是一个显式的对象，不是没有守卫。
		opt.Guard = budget.NewUnlimited()
	}
	if opt.MaxSteps <= 0 {
		opt.MaxSteps = 8
	}
	if opt.Tools == nil {
		opt.Tools = tool.NewRegistry()
	}
	rec := observ.NewRecorder()
	return &Agent{
		Name:         opt.Name,
		Description:  opt.Description,
		SystemPrompt: opt.SystemPrompt,
		Client:       opt.Client,
		Tools:        opt.Tools,
		MaxSteps:     opt.MaxSteps,
		retry:        opt.Retry,
		guard:        opt.Guard,
		rec:          rec,
		// ⚠️ 顺序：**累积器在前** —— 与 `Fanout` 的"不可重建的放前面"一致。
		//	外部的出口可能丢事件（网络、缓冲、被关掉），
		//	而**本地的累计不能丢**（丢了 `Stats()` 就不准了）。
		tracer: observ.Fanout{rec, opt.Tracer},
	}
}

// Stats 返回最近一次运行的统计信息。
//
// # ⚠️ R46：它现在是**从明细算**的，而不是"另存的一份"
//
//	在此之前 `a.stats` 是**另存的一份**：它和事件流各自累加同一些数，
//	而两边**谁都不会报错** —— 它们只会**慢慢不一样**（L2-3 的形状）。
//
//	⭐ 而参考实现给"总量只有一个来源"这个说法提供了**更准确的版本**：
//
//		它有两套成本指标（`agent_*` 按 Agent 累计 / `request_*` 按请求分布），
//		**而它们都从同一个 `addTokens` 累加器派生**。
//
//		> ### 所以那条规矩不是"只能有一个数字"，
//		> ### 是"**只能有一份明细**"。
//		> 可以有很多统计（累计 / 分布 / 比率 / 按模型），
//		> 但它们**必须都从同一份明细算出来**。
//
//	⚠️ 而这里有一个**行为上的细微差别**，要写下来：
//
//		`Steps` 现在是"**真的发起了几次模型调用**"（数 `KindStart`），
//		而以前是"循环进到第几轮"（一进循环就 +1）。
//
//	`Generate` 失败时两者会差 1 —— 而**"真的发起了几次"才是这个数该有的意思**
//	（"跑了 3 步"不该包含那个一次都没发出去的）。以前的版本会把失败那次也算进去。
func (a *Agent) Stats() Stats {
	p, c := a.rec.Usage()
	return Stats{
		Steps:            a.rec.Count(observ.KindStart),
		ToolCalls:        a.rec.Count(observ.KindToolCall),
		PromptTokens:     p,
		CompletionTokens: c,
	}
}

// Run 以单条用户输入启动一次 ReAct 循环，返回最终回复。
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	return a.RunWithMessages(ctx, []message.Message{message.User(input)})
}

// RunWithMessages 以上下文消息启动一次 ReAct 循环。
//
// 循环结构：
//
//	for step < MaxSteps:
//	    调用模型
//	    若无 tool_call  → 返回内容（自然终止）
//	    若有 tool_call  → 逐个执行，把结果追加为 tool 消息，继续下一轮
//	超出 MaxSteps → 返回错误（硬终止）
func (a *Agent) RunWithMessages(ctx context.Context, msgs []message.Message) (string, error) {
	if a.Client == nil {
		return "", fmt.Errorf("%s: 未配置模型客户端", a.Name)
	}
	if a.MaxSteps <= 0 {
		a.MaxSteps = 8
	}
	a.rec.Reset() // 一次运行 = 一份新的明细（替换旧的 `a.stats = Stats{}`）

	history := make([]message.Message, 0, len(msgs)+a.MaxSteps*2)
	history = append(history, msgs...)

	for step := 0; step < a.MaxSteps; step++ {
		// ★ 一轮 ReAct 迭代的开始 —— 与图层"开始跑一跳"**同一个 Kind**。
		//
		//	⚠️ 它替换了旧的 `a.stats.Steps = step + 1`：
		//	那个字段是**另存的一份**（"跑了几步"），而它其实就是**这条事件的条数**。
		a.emit(observ.Event{
			Layer: observ.LayerAgent, Phase: a.Name, Kind: observ.KindStart,
			Detail: fmt.Sprintf("第 %d 轮", step+1),
		})

		// ★ 重试在**这一次调用**上 —— 而**不在整个 ReAct 循环**上。
		//
		// # ⚠️ 这两个位置不是"都行"，它们**不一样**
		//
		//	重试**这一次调用**：模型没答上来 → 再问一次（上下文不变）。
		//	重试**整个循环**：把已经跑过的工具**再跑一遍**（副作用会重放 🚨）。
		//
		//	> ### 副作用能不能重放，决定了重试该放在哪一层 —— 而工具**不可能**保证幂等。
		//
		// # ⚠️ 而发事件的位置是**关键**
		//
		//	`a.emit(call)` 在 `fn` **里面** —— 于是**每一次成功的调用都发一条**。
		//
		//	如果只发最后那一条，重试花的钱**在账上根本不存在** ——
		//	而 R47 那条说的是"**总量只能有一份明细**"：明细少一条，总量就少一笔。
		//
		// ⚠️ **而失败的尝试仍然看不见**（它没有 `Usage`）——
		//	那是一个**真实的缺口**：一次失败的调用**可能已经产生了费用**
		//	（R43 那条"失败也要花钱"的实测：连续失败 10 次烧掉 55321 tokens）。
		//	补它需要 `llm.Client` **在错误里带用量**，那要动 `Client` 的契约 ——
		//	登记为待办，触发条件是"出现按失败率对账的真实需求"。
		var resp *llm.Response
		_, err := retry.Do(ctx, a.retry, func(ctx context.Context) error {
			var genErr error
			resp, genErr = a.Client.Generate(ctx, llm.Request{
				System:   a.SystemPrompt,
				Messages: history,
				Tools:    a.Tools.Definitions(),
			})
			if genErr != nil {
				return genErr
			}
			a.emit(llmCallEvent(a.Name, resp))
			a.guard.ReportUse(budget.Usage{
				PromptTokens:     int64(resp.Usage.PromptTokens),
				CompletionTokens: int64(resp.Usage.CompletionTokens),
			})
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("%s: 调用模型失败 (step %d): %w", a.Name, step, err)
		}

		// 自然终止：模型不再请求工具，说明它认为可以给出最终答案。
		//
		// ⚠️ 这里**不再** `a.emit(call)` —— 那条 `llm_call` 事件由
		//	`retry.Do` 的 `fn` 发出（**每次成功的调用一条**，见上面那段注释）。
		if len(resp.ToolCalls) == 0 {
			a.emit(observ.Event{
				Layer: observ.LayerAgent, Phase: a.Name, Kind: observ.KindDone,
				Detail: "模型不再请求工具 —— 这一轮结束",
			})
			return resp.Content, nil
		}

		history = append(history, message.AssistantToolCall(resp.Content, resp.ToolCalls...))

		for _, tc := range resp.ToolCalls {
			a.emit(observ.Event{
				Layer: observ.LayerTool, Phase: a.Name, Kind: observ.KindToolCall,
				ToolName: tc.Name, ToolArgs: tc.Arguments,
			})

			result, execErr := a.Tools.Execute(ctx, tc.Name, tc.Arguments)
			if execErr != nil {
				// 关键设计：工具错误不向上抛，而是转成结构化文本回灌给模型自愈。
				result = formatToolError(tc.Name, execErr)
			}

			// ⚠️ `Detail` 装结果、`ToolArgs` 装参数 —— **两者都被 `Redact` 清掉**
			//	（一个命中 "Content"、一个命中 "Args"），而 `ToolName` 保留 ✅
			a.emit(observ.Event{
				Layer: observ.LayerTool, Phase: a.Name, Kind: observ.KindToolResult,
				ToolName: tc.Name, Detail: result,
			})
			history = append(history, message.ToolResult(tc.ID, tc.Name, result))
		}
	}

	return "", fmt.Errorf("%s: 达到最大步数 %d 仍未收敛，已强制终止（已调用工具 %d 次）",
		a.Name, a.MaxSteps, a.Stats().ToolCalls)
}

// formatToolError 把工具错误转成模型能理解的结构化结果。
func formatToolError(name string, err error) string {
	payload := map[string]any{
		"ok":    false,
		"tool":  name,
		"error": err.Error(),
		"hint":  "请检查参数是否符合工具定义；如无法修复，请改用其他工具或调整方案。",
	}
	b, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return fmt.Sprintf(`{"ok":false,"tool":%q,"error":%q}`, name, err.Error())
	}
	return string(b)
}

// emit 发一个事件。
//
// ⚠️ **没有判空** —— 而那不是疏忽：`a.tracer` 在 `New` 里被构造成
// `Fanout{rec, opt.Tracer}`，而 `Fanout` 会跳过 nil 元素、`rec` 永不为 nil。
//
//	> **"没有出口"是一个合法的状态，不是错误** —— 于是**没有调用点需要判空**
//	> （`NopTracer` / `Unlimited` / `EmptyLimiter` 是同一条）。
//
// llmCallEvent 造一条**模型调用**的明细事件（带用量）。
//
// ⚠️ 它被抽出来是因为**它的发出位置变了**（R49）：
//
//	以前它在循环体里发一次，现在它在 `retry.Do` 的 `fn` **里面**发 ——
//	于是**每一次成功的调用都有一条**，重试的那几次**也在账上**。
//
//	> ### 明细少一条，总量就少一笔（R47：「总量只能有一份明细」）。
//
// ⚠️ `Model` **留空**：agent 不知道模型名（只有 `llm.Client` 知道）。
//
//	而这是**允许的** —— `kindSpecs[KindLLMCall]` 只要求用量，不要求模型名，
//	那条注释的原话是："缺模型名 → 还能用（只是按模型切片时少一维）→ 不约束"。
//
//	⭐ 但"按模型切片"**确实是一维有用的信息** —— 要补它需要一个
//	  `llm.ModelReporter` 可选能力（与 `TimeoutReporter` 同一个手法）。
//	  登记为待办，触发条件是"出现按模型切片 / 成本分摊的真实需求"。
func llmCallEvent(phase string, resp *llm.Response) observ.Event {
	return observ.Event{
		Layer: observ.LayerLLM, Phase: phase, Kind: observ.KindLLMCall,
		Detail:           resp.Content,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
	}
}

func (a *Agent) emit(e observ.Event) { a.tracer.Trace(e) }
