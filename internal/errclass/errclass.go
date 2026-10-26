// Package errclass 把 error 映射成一个**标准化的小类型**。
//
// # ⚠️ 它抄的是参考实现的**形状**，不是那张表（R49）
//
//	`beautifulagent-go/internal/errclass/v5_html_ppt.go` 是"**全项目唯一错误类型定义源**"，
//	里面有一张很大的表（A 类请求入口 / B 类 LLM 下游 / C 类生成结果，约 25 个常量）。
//
//	> ### **而那张表是它业务的 —— 抄过来毫无意义。**
//	> 要抄的是它的形状：
//	>
//	>	① 一组常量 + 一个 `Classify(err) Class` 函数 + **写清的分类优先级**
//	>	② **结构化优先、字符串兜底**（它把这条写进了函数注释）
//	>	③ `Canceled` 与 `DeadlineExceeded` **要分开判**，因为它们可能**同时为真**
//
// # ⚠️ 而我们的分类**只有两个值** —— 因为**只有一个消费者**
//
//	它的表有 25 个常量，因为它的消费者很多（Prom 上报 / 日志 / Controller 分流）。
//	**我们只有一个：重试**（`retry.Do` 的 `canRetry`）。
//
//	> ### 类的数量**由消费者的数量决定**，不由"错误的丰富程度"决定。
//	> 多定义一个没人问的类，就是"为用不到的场景预留机制"（R44 的教训）。
//
//	⭐ 所以这里只有 `Retryable` / `Permanent` ——
//	  而**将来**多一个消费者（比如"要报警的"）时再加，那时它会**自己逼出**第三类。
package errclass

import (
	"context"
	"errors"
	"net"
)

// Class 是一个错误在**我们关心的问题**下的类别。
//
// ⚠️ 它答的是"**重试有没有用**"，而不是"这是什么错误" —
//
//	那是**不同的问题**，而混起来会让这个类型慢慢长成一张大表。
type Class string

const (
	// Retryable 表示"再来一次**可能**会好"：限流、网络抖动、下游 5xx。
	Retryable Class = "retryable"

	// Permanent 表示"再来一次还是这样"。
	//
	// ⚠️ 它是**兜底值**（见 `Of`）—— 而那个方向是**故意的**，理由见 `Of` 的注释。
	Permanent Class = "permanent"
)

// RetryableError 是一个错误**自己声明**"我可以被重试"。
//
// # ⚠️ 为什么是接口，而不是"判类型 + 抠字符串"
//
//	参考实现的优先级是"结构化的类型判定优先，字符串兜底"。
//	而它的**类型判定**只能认它自己那几个类型（`GatewayError`）——
//	**下游库的错误它认不出来**，所以它必须留一层字符串匹配。
//
//	> ### 一个接口把这个顺序**反过来**：让**错误自己说**。
//	> 这样下游新增一种错误时，**改的是那个错误**，而不是这里再加一条 `strings.Contains`。
//
// ⭐ 判据：`strings.Contains(err.Error(), "timeout")` 这种代码，
//
//	**每写一条就多一个假的匹配**（"不 timeout 了" 里也有 "timeout"）。
//	而接口**不可能误匹配**。
type RetryableError interface {
	// Retryable 报告"再试一次是否有意义"。
	Retryable() bool
}

// Of 判定一个错误的类别。
//
// # ⚠️ 优先级（**顺序不能换** —— 换了就是另一个函数）
//
//	① `RetryableError` 接口        错误**自己说** —— 最可信，放最前
//	② `context.Canceled`           人取消的 → 重试**不该做**（他要的就是停）
//	③ `context.DeadlineExceeded`   见下
//	④ `net.Error`（超时类）         网络抖动 → 可重试
//	⑤ 兜底 `Permanent`
//
// # ⚠️ ② 与 ③ 要**分开写** —— 而"分开"的理由是**它们的理由不同**，不是顺序
//
//	参考实现里有一句很精的细节（`ClassifyV5Error`）：
//
//		errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
//
//	它那么写是因为**两个值属于它表里两个不同的类**（`canceled` / `timeout`），
//	而**一个错误可以同时满足这两条**（`errors.Join` 出来的、或库里包了几层的）——
//	所以"先判哪个"**决定了它被归成哪一类**。
//
// ⭐ 而**在我们这里，那个顺序是空操作** —— 因为我们只有两个类，而两条都归 `Permanent`。
//
//	> ### 所以：**参考实现里的一句细节，可能在你的系统里是空操作** ——
//	> 因为**类的数量不同**。抄细节之前要问："它防的那个情况，在我这里存在吗？"
//	> （而答案是"不存在"时，**不能顺手把它抄过来假装自己也想过了**。）
//
//	⚠️ 但仍然**要分开写**，因为**它们不该重试的理由是两条不同的理由**：
//
//		· **人取消**：他要的就是停，重试是**抗命**
//		· **终局超时**：重试只会让"更慢"变成"**更更慢**"
//
//	而哪天多一个消费者（比如"要报警的"）时，它们会**自然分成两类** ——
//	那时候这一段注释就是"为什么它们本来就该分开"的答案。
//
// ⭐ 还有**第三种超时**：单次模型调用的 HTTP 60s 超时。
//
//	它**值得**重试，而 `Of` 在这里**区分不出来**（`ctx.Err()` 只有一个值）。
//	→ 那条路要靠**错误自己**声明（`RetryableError`）—— 这正是①存在的理由。
//
// # ⚠️ 兜底是 `Permanent`，而这是**有方向的选择**
//
//	参考实现的兜底是 `ErrTypeGeneric`（一个**中性**的值）—— 因为它不回答"要不要重试"。
//	**而我们的兜底必须有方向**，而方向是**不重试**：
//
//	> 一个**我们不认识**的错误，重试它可能是在**放大一个真问题** ——
//	> "参数非法"重试 3 次，只是用 3 倍的钱得到同一个错误。
//
//	⭐ 两个兜底**都是对的**，因为**它们回答的问题不同**（它问"是什么"，我们问"要不要重试"）。
func Of(err error) Class {
	if err == nil {
		// ⚠️ `nil` 传进来是**调用方的 bug** —— 而这里不 panic：
		//	一个分类函数不该成为新的故障点。返回 `Permanent`（不重试）是安全的。
		return Permanent
	}

	// ① 错误自己说（结构化优先）
	var self RetryableError
	if errors.As(err, &self) {
		if self.Retryable() {
			return Retryable
		}
		return Permanent
	}

	// ② 人取消的：他要的就是停
	//
	// ⚠️ 这一条**必须在 ③ 之前** —— 见 `Of` 的注释（两者可能同时为真）。
	if errors.Is(err, context.Canceled) {
		return Permanent
	}

	// ③ 超时
	if errors.Is(err, context.DeadlineExceeded) {
		return Permanent
	}

	// ④ 网络抖动（超时类）
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return Retryable
	}

	// ⑤ 兜底：不认识 → **不重试**（理由见 `Of` 的注释）
	return Permanent
}

// CanRetry 是 `retry.Do` 要的那个回调 —— 一个**名字说完**它答案的东西。
//
// ⚠️ 它存在的理由是**可读性**，不是抽象：
//
//	retry.Do(fn, errclass.CanRetry, 3)        // ← 读者立刻知道策略
//	retry.Do(fn, func(e error) bool { ... })  // ← 读者要知道重试策略，得读进去
func CanRetry(err error) bool { return Of(err) == Retryable }
