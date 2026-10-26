// Package retry 是"再试一次"这件事的**机制** —— 而**策略**由调用方给。
//
// # ⚠️ 它抄自参考实现（`internal/util/retry.go`，共 38 行）—— 而抄之前先读了它
//
//	它那个函数的签名是：
//
//		func Retry(fn func() error, canRetry func(err error) bool, attempts int) (retryCount int, err error)
//
//	三个设计点值得**逐字抄**：
//
//		① `canRetry` 是**回调** —— 机制不认识"什么错值得重试"，那是策略
//		② 返回值带 **`retryCount`** —— 于是最终的错误能说"我试了 3 次"
//		③ `RetryError` 有 **`Unwrap()`** —— 于是 `errors.Is` 能穿透它
//
// # ⚠️ 而抄的时候读出了**它两个 bug**（都出自同一行）
//
//		for retryCount = 0; retryCount < attempts; retryCount++ { err = fn() ... }
//		return retryCount, &RetryError{Err: err, RetryCount: retryCount}
//
//	· **`attempts = 0`** → 循环**一次都不进** → `fn` **从未被调用** →
//	  而 `err` 还是**零值 nil** → 于是它返回 `&RetryError{Err: nil}`。
//	  也就是：**"一次都没试"被报成了"试失败了"**。
//
//	· 而那个值的 `Error()` 里写着 `e.Err.Error()` → **nil panic** 🚨
//
//	> ### 判据（R42）：**能不能写出一条今天就会失败的测试？** —— 能。
//	> 所以那**不是"风格不同"，是缺陷**。
//
// ⭐ 我们的修法是**显式归一**：`attempts <= 0` 读作"**试一次**"（见 `Do`）——
//
//	因为"试 0 次"不是一个有意义的配置，而它有两种坏读法（不试却报错 / 不试却报成功）。
//
// # 另外两处**有理由的偏离**（都写在下面各自的注释里）
//
//	· 加了 **`ctx`** —— 一个**不看 ctx 的重试循环**会在取消后继续重试
//	· 加了 **退避** —— **立即重试对"限流"几乎一定失败**（那是它最主要的用途）
package retry

import (
	"context"
	"fmt"
	"time"

	"multiagent/internal/errclass"
)

// Error 是"试过 `Attempts` 次仍然失败"。
//
// ⚠️ 它**必须带 `Error()` 的保护**（`err` 不为 nil）—— 见包注释里那个 bug。
//
//	归一化放在 `Do` 里做，而这里仍然显式处理 `nil`：
//	**两个地方各防一道，因为 panic 的代价远大于那两行**。
type Error struct {
	Err error

	// Attempts 是**总共试了几次**（含第一次）—— 不是"重试了几次"。
	//
	// ⚠️ 这两个数差一，而报错时**说错哪一个都会误导**：
	//	"试了 3 次"和"重试了 3 次"（= 共 4 次）是两个不同的量。
	Attempts int
}

func (e *Error) Error() string {
	if e.Err == nil {
		// 不可达（`Do` 归一到至少 1 次）—— 而它是一次**便宜的保险**：
		// 一个装错误的类型在 `Error()` 里 panic，会让"报错"变成"崩溃"。
		return fmt.Sprintf("重试 %d 次仍然失败（而错误是空的 —— 这是个 bug）", e.Attempts)
	}
	return fmt.Sprintf("%s（已试 %d 次）", e.Err.Error(), e.Attempts)
}

// Unwrap 让 `errors.Is` / `errors.As` 能穿透到**真正的那个错误**。
//
// ⚠️ 少了它，`errors.Is(err, context.DeadlineExceeded)` 会失效 ——
//
//	而所有"按错误类型分流"的代码都会**静默退化**。
func (e *Error) Unwrap() error { return e.Err }

// Policy 是**策略**（而 `Do` 是**机制**）。
//
// ⚠️ 零值 `Policy{}` 是**可用的**：试一次、可重试性交给 `errclass`、不退避。
//
//	一条能用的默认值是"这个结构没有必填字段"的证据。
type Policy struct {
	// Attempts 是**总尝试次数**（含第一次）。`<= 0` → 归一成 1（见 `Do`）。
	Attempts int

	// CanRetry 决定"这个错值不值得再试一次"。
	//
	// nil → 用 `errclass.CanRetry`（本项目对"可重试性"的唯一答案）。
	CanRetry func(error) bool

	// Backoff 给出"第 n 次失败后等多久"（n 从 1 起）；返回 0 = 不等。
	//
	// ⚠️ nil = **不退避** —— 而那是一个**合法策略**，不是一个测试后门：
	//	"立刻重试"对**幂等且瞬时**的故障是对的（比如一次瞬时 5xx）。
	//
	// ⚠️ 而"限流"**必须**退避 —— 立即重试几乎一定再次撞上限流，
	//	于是 3 次尝试在几毫秒内烧完，得到的是**三倍的账单**和**同一个错误**。
	Backoff func(attempt int) time.Duration
}

// Do 跑 `fn`，按 `p` 的策略重试。
//
// 返回 `(n, err)`：
//
//	n = 0        第一次就成功
//	n = k        重试了 k 次之后成功
//	n = k, err   试了 k+1 次仍然失败（err 是 `*Error`，除非它**不值得重试**）
//
// ⚠️ **"不值得重试"时返回的是原始错误**（不是 `*Error`）——
//
//	因为那时候"我试了几次"不重要（**第一次就知道了**），
//	而包里多一层会让调用方的 `errors.Is` 多绕一圈。
//
// # `ctx` 是本实现**加**的（参考实现没有）
//
//	一个不看 ctx 的重试循环，会在**用户已经取消之后继续重试** ——
//	而那与"人取消 = 他要的就是停"是**直接冲突**的（`errclass` 把取消归为 `Permanent`，
//	可那只挡住"取消**造成**的错"，挡不住"取消**之后**开始的新尝试"）。
//
//	⚠️ 退避的等待**也必须能被 ctx 打断** —— `time.Sleep` 不行。
func Do(ctx context.Context, p Policy, fn func(context.Context) error) (int, error) {
	attempts := p.Attempts
	if attempts <= 0 {
		// ⚠️ 归一成 1（"试一次"）而不是 0 —— 理由见包注释里那两条 bug。
		attempts = 1
	}
	canRetry := p.CanRetry
	if canRetry == nil {
		canRetry = errclass.CanRetry
	}

	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if d := backoff(p.Backoff, i); d > 0 {
				select {
				case <-ctx.Done():
					// 已经取消 → 不再试。返回**ctx 的错**，而不是上一次的 err：
					// "为什么停了"的答案是"因为取消了"，而不是"因为上一次失败了"。
					return i, ctx.Err()
				case <-time.After(d):
				}
			}
		}

		err = fn(ctx)
		if err == nil {
			return i, nil // i = 重试了几次
		}
		if !canRetry(err) {
			// ⚠️ **立刻返回，不再试** —— 这是"分类"存在的全部意义。
			//	不分类的重试就是"无论什么错都试 3 次"，而那会把
			//	"参数非法"这种**第一次就注定失败**的错，变成三倍的账单。
			return i, err
		}
	}
	return attempts - 1, &Error{Err: err, Attempts: attempts}
}

// backoff 取"第 attempt 次失败后该等多久"（`nil` 策略 = 不等）。
func backoff(f func(int) time.Duration, attempt int) time.Duration {
	if f == nil {
		return 0
	}
	if d := f(attempt); d > 0 {
		return d
	}
	return 0 // 负数也读作"不等"（别让一个笔误变成"睡一个负的时长"）
}

// Fixed 是一个**最简的退避策略**：每次都等同一个时长。
//
// ⚠️ 它**不是**最好的（指数退避更好），而它**够用**，且**一眼看得懂** ——
//
//	指数退避要配抖动，而"没有抖动的指数退避"会让多个并发的重试**同时醒来**，
//	于是限流**再来一次**。我们还没有那个并发规模，所以先给这个。
//
// ⭐ 而 `Backoff` 是**一个函数**而不是一个枚举 —— 所以将来要换成指数退避时，
//
//	**改的是调用方那一行**，这个包一行都不用动。
func Fixed(d time.Duration) func(int) time.Duration {
	return func(int) time.Duration { return d }
}
