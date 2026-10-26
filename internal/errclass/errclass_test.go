package errclass

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// selfRetryable / selfPermanent —— 两种"错误自己声明"的实现。
type selfRetryable struct{}

func (selfRetryable) Error() string   { return "限流" }
func (selfRetryable) Retryable() bool { return true }

type selfPermanent struct{}

func (selfPermanent) Error() string   { return "参数非法" }
func (selfPermanent) Retryable() bool { return false }

// ⚠️ ① 在最前：**错误自己说，比任何外部推断都可信**。
//
//	特别是它**压过**下面的字符串/类型推断 —— 一个 `Retryable() == false`
//	的错误，即使它的文本里有 "timeout"，也不该被重试。
func TestSelfDeclarationWins(t *testing.T) {
	// 一个**说自己是永久失败**、而文本里带 "timeout" 的错误
	wrapped := fmt.Errorf("调用超时（timeout）: %w", selfPermanent{})
	if got := Of(wrapped); got != Permanent {
		t.Errorf("错误自己声明了 Permanent，就该听它的（文本里的 timeout 是噪声），实际 %s", got)
	}

	if got := Of(fmt.Errorf("包了几层: %w", selfRetryable{})); got != Retryable {
		t.Errorf("接口应当在 Unwrap 之后仍然生效（errors.As 会穿透），实际 %s", got)
	}
}

// ⚠️ ②③ 两条**不重试**的理由不同，而结论一样 —— 所以**分开列**。
func TestCancelAndTimeoutAreBothPermanent(t *testing.T) {
	if got := Of(context.Canceled); got != Permanent {
		t.Errorf("人取消 = 他要的就是停，重试是抗命。实际 %s", got)
	}
	if got := Of(context.DeadlineExceeded); got != Permanent {
		t.Errorf("终局超时 = 重试只会更慢。实际 %s", got)
	}

	// ⚠️ 而**两者同时为真**是可能的（包了几层 / `errors.Join`）——
	//	参考实现为这个情况写了 `&& !DeadlineExceeded` 来定序。
	//	**而我们两条都归 Permanent，所以那个顺序在这里是空操作。**
	//	这条断言写下来是为了**记住那件事**：抄来的细节要问"它防的情况我这儿有吗"。
	both := errors.Join(context.Canceled, context.DeadlineExceeded)
	if !errors.Is(both, context.Canceled) || !errors.Is(both, context.DeadlineExceeded) {
		t.Fatal("前置条件不成立：这个值应当同时满足两条")
	}
	if got := Of(both); got != Permanent {
		t.Errorf("同时为真时也该是 Permanent，实际 %s", got)
	}
}

// ⚠️ ④ 网络抖动**可重试** —— 那是这个包最想抓的一类。
func TestNetworkTimeoutIsRetryable(t *testing.T) {
	netErr := &net.DNSError{IsTimeout: true, Err: "i/o timeout"}
	if got := Of(fmt.Errorf("请求失败: %w", netErr)); got != Retryable {
		t.Errorf("超时类网络错误值得重试，实际 %s", got)
	}

	// ⚠️ 而**非超时**的网络错（比如 DNS 解析失败）**不**自动可重试 ——
	//	它是"这个域名根本没有"，试十次也没有。
	notTimeout := &net.DNSError{Err: "no such host"}
	if got := Of(notTimeout); got != Permanent {
		t.Errorf("非超时的网络错不该被当成可重试，实际 %s", got)
	}
}

// ⚠️⚠️ 兜底的**方向**是这个包最重要的一个决定。
//
//	参考实现的兜底是一个**中性**的值（`ErrTypeGeneric`），因为它不回答"要不要重试"。
//	**我们的兜底必须有方向，而方向是不重试** ——
//	因为重试一个**我们不认识**的错误，可能是在**放大一个真问题**。
func TestUnknownErrorIsPermanent(t *testing.T) {
	if got := Of(errors.New("这是什么东西")); got != Permanent {
		t.Errorf("不认识的错误应当**不重试**（放大真问题的代价 > 少赚一次重试），实际 %s", got)
	}
}

// ⚠️ `nil` 传进来是调用方的 bug —— 而分类函数**不该成为新的故障点**。
func TestNilDoesNotPanic(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("`Of(nil)` 不该 panic —— 一个分类函数成为故障点是很荒唐的: %v", p)
		}
	}()
	if got := Of(nil); got != Permanent {
		t.Errorf("nil 的答案是 Permanent（安全的一侧），实际 %s", got)
	}
}

// ⚠️ 守着"`CanRetry` 是一个**名字说完它答案**的东西" —— 见它自己的注释。
func TestCanRetryMatchesOf(t *testing.T) {
	for _, err := range []error{
		nil, context.Canceled, context.DeadlineExceeded,
		selfRetryable{}, selfPermanent{}, errors.New("未知"),
	} {
		if CanRetry(err) != (Of(err) == Retryable) {
			t.Errorf("`CanRetry` 与 `Of` 必须是同一个答案的两个说法（%v 不一致）", err)
		}
	}
}

// 顺带：确认 `Class` 的两个值是**稳定字符串**（它们会被写进日志，改名会让历史查询失效）。
func TestClassValuesAreStableStrings(t *testing.T) {
	if Retryable != "retryable" || Permanent != "permanent" {
		t.Error("这两个值会被写进日志/指标，改名等于让历史数据查不出来 —— 改之前先想清楚")
	}
	_ = time.Second // 保持 time 的引用（若将来去掉退避相关的用例）
}
