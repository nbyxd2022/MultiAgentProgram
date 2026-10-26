package retry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// alwaysRetry / neverRetry —— 两个**只关于策略**的回调，让机制测试不被分类干扰。
func alwaysRetry(error) bool { return true }
func neverRetry(error) bool  { return false }

// ⚠️ 最要紧的一条：**"不值得重试"必须只试一次**。
//
//	这是"分类"存在的全部意义 —— 不分类的重试就是"无论什么错都试 3 次"，
//	而它会把"参数非法"这种**第一次就注定失败**的错变成三倍的账单。
func TestDoStopsWhenNotWorthRetrying(t *testing.T) {
	calls := 0
	_, err := Do(context.Background(), Policy{
		Attempts: 5,
		CanRetry: neverRetry,
	}, func(context.Context) error {
		calls++
		return errors.New("参数非法")
	})

	if calls != 1 {
		t.Fatalf("不值得重试的错应当**只试一次**，实际试了 %d 次 —— "+
			"多试的每一次都是白花的钱", calls)
	}
	if err == nil || err.Error() != "参数非法" {
		t.Errorf("应当原样返回那个错（不该包一层 —— 第一次就知道了），实际 %v", err)
	}
}

func TestDoRetriesAndReportsCount(t *testing.T) {
	calls := 0
	n, err := Do(context.Background(), Policy{
		Attempts: 5,
		CanRetry: alwaysRetry,
	}, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("限流")
		}
		return nil
	})

	if err != nil {
		t.Fatalf("第 3 次该成功了: %v", err)
	}
	if calls != 3 {
		t.Errorf("应当调 3 次，实际 %d", calls)
	}
	// ⚠️ `n` 是"**重试**了几次"（不含第一次）—— 与 `Error.Attempts` 差一。
	if n != 2 {
		t.Errorf("返回值应当是「重试了 2 次」，实际 %d", n)
	}
}

// ⚠️⚠️ **这一条守着参考实现的两个 bug**（包注释里写的那一行）。
//
//	它的 `Retry(fn, canRetry, 0)`：循环一次都不进 → `fn` 从未被调用 →
//	`err` 还是零值 `nil` → 返回 `&RetryError{Err: nil}` →
//	而 `Error()` 里 `e.Err.Error()` → **panic**。
//
//	> 判据（R42）：**能不能写出一条今天就会失败的测试？** —— 这就是那一条。
//
// ⭐ 我们读作"**试一次**"：因为"试 0 次"不是一个有意义的配置，
//
//	而它有两种坏读法（不试却报错 / 不试却报成功）。
func TestZeroAttemptsMeansTryOnce(t *testing.T) {
	for _, attempts := range []int{0, -1, -100} {
		calls := 0
		_, err := Do(context.Background(), Policy{Attempts: attempts, CanRetry: alwaysRetry},
			func(context.Context) error { calls++; return nil })

		if calls != 1 {
			t.Errorf("Attempts=%d：应当归一成「试一次」，实际调了 %d 次", attempts, calls)
		}
		if err != nil {
			t.Errorf("Attempts=%d：第一次就成功，不该有错，实际 %v", attempts, err)
		}
	}

	// ⚠️ 而**失败**的那条路也要守住"错误不为 nil 且 Error() 不 panic"
	_, err := Do(context.Background(), Policy{Attempts: 0, CanRetry: alwaysRetry},
		func(context.Context) error { return errors.New("炸了") })
	if err == nil {
		t.Fatal("试了一次且失败了，应当有错")
	}
	var re *Error
	if !errors.As(err, &re) {
		t.Fatalf("应当是可穿透的 *Error，实际 %T", err)
	}
	if re.Attempts != 1 {
		t.Errorf("Attempts 应当是**总次数** 1，实际 %d", re.Attempts)
	}
	// `Error()` 必须能**安全地**调用（参考实现在这条路上会 panic）
	if s := err.Error(); !strings.Contains(s, "炸了") || !strings.Contains(s, "已试 1 次") {
		t.Errorf("错误串应当说清「什么错」+「试了几次」，实际 %q", s)
	}
}

// ⚠️ `Unwrap` 少了会怎样见它的注释 —— 这里守着它真的在。
func TestErrorUnwrapAllowsErrorsIs(t *testing.T) {
	sentinel := errors.New("哨兵")
	_, err := Do(context.Background(), Policy{Attempts: 3, CanRetry: alwaysRetry},
		func(context.Context) error { return sentinel })

	if !errors.Is(err, sentinel) {
		t.Fatal("`errors.Is` 应当能穿透 `*Error` —— 否则所有按错误类型分流的代码都会静默退化")
	}
}

// ⚠️ 加 `ctx` 的全部理由：**取消之后不该再开始新的尝试**。
func TestDoStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	_, err := Do(ctx, Policy{
		Attempts: 10,
		CanRetry: alwaysRetry,
		Backoff:  Fixed(time.Millisecond),
	}, func(context.Context) error {
		calls++
		if calls == 1 {
			cancel() // 第一次失败的同时，用户取消了
		}
		return errors.New("网络抖动")
	})

	if calls != 1 {
		t.Errorf("取消之后不该再试，实际试了 %d 次", calls)
	}
	// ⚠️ 返回的是 **ctx 的错**，不是上一次的失败 ——
	//	"为什么停了"的答案是"因为取消了"。
	if !errors.Is(err, context.Canceled) {
		t.Errorf("应当报「因为取消而停」，实际 %v", err)
	}
}

// ⚠️ 退避的等待**必须能被 ctx 打断**（`time.Sleep` 不行）——
//
//	否则一个"取消后还要睡 30 秒"的循环，会让用户以为程序挂了。
func TestBackoffWaitIsInterruptible(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := Do(ctx, Policy{
		Attempts: 2,
		CanRetry: alwaysRetry,
		Backoff:  Fixed(10 * time.Second), // 一个**会被打断**的长等待
	}, func(context.Context) error { return errors.New("网络抖动") })

	if took := time.Since(start); took > time.Second {
		t.Errorf("等待应当被 ctx 打断（实际等了 %v）—— 一个不可打断的退避会让取消看起来没生效", took)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("应当报取消，实际 %v", err)
	}
}

// ⚠️ 零值 `Policy{}` 必须**能用** —— 试一次、可重试性交给 `errclass`、不退避。
func TestZeroPolicyIsUsable(t *testing.T) {
	calls := 0
	// 用一个**不可重试**的错：零值策略只该试一次
	// （`context.Canceled` 是 `errclass` 判为永久失败的典型 —— 而它是个真 error）
	_, err := Do(context.Background(), Policy{}, func(context.Context) error {
		calls++
		return context.Canceled
	})
	if calls != 1 {
		t.Errorf("零值策略应当只试一次，实际 %d", calls)
	}
	if err == nil {
		t.Error("应当返回那个错")
	}
}

// ⚠️ 顺带证明"退避真的等了" —— 用**极短**的时长，让测试不 flake（R23）。
func TestBackoffIsActuallyWaited(t *testing.T) {
	start := time.Now()
	const each = 30 * time.Millisecond
	_, _ = Do(context.Background(), Policy{
		Attempts: 3, // 两次失败 → 等两轮
		CanRetry: alwaysRetry,
		Backoff:  Fixed(each),
	}, func(context.Context) error { return errors.New("抖动") })

	if took := time.Since(start); took < 2*each {
		t.Errorf("应当等够 2×%v，实际只用了 %v —— 退避没生效（而限流场景就靠它）", each, took)
	}
}
