package budget

import (
	"strings"
	"testing"
	"time"
)

// ─── 脚手架 ─────────────────────────────────────────────────────────

// ⚠️⚠️ 这里**曾经有一个 `fakeClock`** —— R43 把它删了，而它消失的原因值得记。
//
//	它当时存在的理由是："`Wall` 必须用时间做判据，而拿真实时间做判据的测试会 flake（R23）——
//	所以时钟必须能被注入。"
//
//	**而那个理由的前提是错的**：它假设"时间"是守卫**读**出来的。
//	R43 之后时间维是**报**进来的（`Usage.Elapsed`，与 token 完全同构）——
//	于是守卫**不再看时钟**，也就**没有什么需要被注入**。
//
//	> ### 一个测试接缝的消失，往往意味着被测的东西**换了职责**。
//	> 当时我把它当成"时间这类判据的必要代价"；其实它是"守卫在做一件
//	> 它不该做的事（发明事实）"的**症状** —— 而我当时把症状当成了需求。

// newTestGuard 造一个按五维检查的守卫（`*guarded` 的具体类型，供快照断言用）。
func newTestGuard(t *testing.T, limits Limits, opts ...Option) *guarded {
	t.Helper()
	g, ok := New(limits, opts...).(*guarded)
	if !ok {
		t.Fatal("New 应当返回 *guarded")
	}
	return g
}

// openAll 返回"**五维全不限**"的额度，供测试**只覆盖它关心的那一维**：
//
//	l := openAll()
//	l.Steps = 3          // 只有步数有限，其余敞着
//
// # ⚠️ 不要写 `Limits{Steps: 3}`
//
//	那读作"步数 ≤ 3 **且时间上限 0**"—— 于是**一步都不让跑**（fail-closed ✅），
//	测试会变成一条**空跑**（因为没有任何一步能过）。
//
//	⚠️ 而 R43 之前那种写法**碰巧能用** —— 因为时间维当时不在 `Limits` 里，
//	  它藏在 `guarded.maxWall`，测试**看不见也就不受它管**。
//
//	> ### 那是一颗静默的雷：**"测试里能写出来"不等于"语义对"**。
//	> 它之所以没炸，只是因为 `maxWall` 的默认值恰好是"不限"——
//	> 而那是一次**幸运**，不是一次设计。
func openAll() Limits { return allUnlimited() }

// ─── 零值语义（这一节是整包最重要的一条）────────────────────────────

// ⚠️⚠️ **`Limits{}` = 四维上限都是 0**（fail-closed）—— 什么都不让做。
//
// # 这条测试守的是一个改过的设计
//
//	第一版写的是"**零值表示该维度不限**"（fail-open）——
//	一个忘记配预算的调用方会得到"**无限花钱**"。
//
//	而精读 `EmptyLimiter` 时看到那颗雷：它的零值里
//	`Limited=false`（不限）与 `Remaining=0`（没额度）**含义相反**。
//
//	修法：**零 = 限量 0**，**负数 = 不限**（`NoLimit`）。
//
//	> 判据：**忘记配置时，系统该往哪边倒？**
//	> 时间往"有默认值"倒（没有默认才危险）；预算往"什么都不让做"倒（不限才危险）。
func TestZeroLimitsIsFailClosed(t *testing.T) {
	g := newTestGuard(t, Limits{}) // ← 零值

	d := g.Allow(Usage{Steps: 1})
	if d.OK {
		t.Fatal("零值 Limits 应当**什么都不让做** —— 那正是 fail-closed 的含义")
	}
	if d.Exceeded != DimSteps {
		t.Errorf("应当指出是 steps 超了，实际 %q", d.Exceeded)
	}
}

// 要"不限"，必须**显式**——用 `NoLimit`（负数），而不是靠零值。
//
// ⚠️ **五维都是**：`Elapsed` 也要显式写 `NoWall`（R43 之前它有豁免，现在没有）。
func TestNoLimitMeansUnbounded(t *testing.T) {
	g := newTestGuard(t, openAll())

	for i := 0; i < 100; i++ {
		g.ReportUse(Usage{Steps: 1, PromptTokens: 1000, CostMilli: 1000, Elapsed: time.Hour})
	}
	if d := g.Allow(Usage{Steps: 1}); !d.OK {
		t.Fatalf("五维全 NoLimit 时不该拦，实际被 %q 拦了", d.Exceeded)
	}
}

// 一个显式的空对象（而不是 nil）—— 于是**没有调用点需要判空**。
func TestUnlimitedNeverBlocks(t *testing.T) {
	var g Guard = NewUnlimited()
	if d := g.Allow(Usage{Steps: 1 << 40, CostMilli: 1 << 40}); !d.OK {
		t.Error("Unlimited 永远不该拦")
	}
	// 而它的 Snapshot 要能说清"我不限额"
	sn := g.Snapshot()
	if sn.Limits.Bounded() {
		t.Error("Unlimited 的 Limits 不该是「有上限」")
	}
	if DescribeLimits(sn.Limits) != "不限额" {
		t.Errorf("应当渲染成「不限额」，实际 %q", DescribeLimits(sn.Limits))
	}
}

// nil 守卫的读者（`cmd/l4`）应当拿 `Unlimited`，而不是 nil —— 这条测试**说明**这个约定。
func TestGuardsAreInterfacesNotNil(t *testing.T) {
	var g Guard = NewUnlimited()
	g.ReportUse(Usage{Steps: 1}) // 不 panic
	if g.Snapshot().Spent.Steps != 0 {
		t.Error("Unlimited 不该记账（它没有账）")
	}
}

// ─── 检查与记账分离 ─────────────────────────────────────────────────

// ⚠️ `Allow` **不记账** —— 这是"成功才计入"的前提。
//
//	如果 `Allow` 顺手记了，那一次**失败**的运行也算了钱 ——
//	那是**持续的系统性偏差**（比"偶尔多花"严重得多，因为它永远偏一个方向）。
func TestAllowDoesNotSpend(t *testing.T) {
	l := openAll()
	l.Steps = 3
	g := newTestGuard(t, l)

	for i := 0; i < 10; i++ { // 反复检查，一次都不记账
		if d := g.Allow(Usage{Steps: 1}); !d.OK {
			t.Fatalf("第 %d 次检查就被拦了 —— Allow 记账了", i+1)
		}
	}
	if got := g.Snapshot().Spent.Steps; got != 0 {
		t.Errorf("Allow 之后 Spent.Steps = %d，期望 0", got)
	}
}

// `ReportUse` 累加；到上限就该拦。
func TestReportUseAccumulatesAndBlocks(t *testing.T) {
	l := openAll()
	l.Steps = 3
	g := newTestGuard(t, l)

	for i := 1; i <= 3; i++ {
		if d := g.Allow(Usage{Steps: 1}); !d.OK {
			t.Fatalf("第 %d 步不该被拦", i)
		}
		g.ReportUse(Usage{Steps: 1})
	}
	if got := g.Snapshot().Spent.Steps; got != 3 {
		t.Fatalf("Spent.Steps = %d，期望 3", got)
	}
	// 第 4 步超
	d := g.Allow(Usage{Steps: 1})
	if d.OK {
		t.Error("第 4 步应当被拦")
	}
	if d.Exceeded != DimSteps {
		t.Errorf("应当指出是 steps，实际 %q", d.Exceeded)
	}
}

// ⚠️ 边界：**用满但没超**必须放行（`>` 而不是 `>=`）。
//
//	（"正好用尽"和"超了"是两件事。把它们合并会让**最后一格永远用不上** ——
//	 而那个 bug 只在"刚好到上限"时出现，平时测不出来。）
func TestAllowAtExactBoundary(t *testing.T) {
	l := openAll()
	l.PromptTokens = 100
	g := newTestGuard(t, l)

	g.ReportUse(Usage{PromptTokens: 60})
	if d := g.Allow(Usage{PromptTokens: 40}); !d.OK {
		t.Error("60+40=100，正好用满，应当放行")
	}
	if d := g.Allow(Usage{PromptTokens: 41}); d.OK {
		t.Error("60+41=101，超了 1，应当拦")
	}
}

// ⚠️ `Allow` 看的是**投影后的**总量（已用 + 这一次要用的）——
//
//	而不是只看这一次的。
//
//	（只看这一次的话，一个"每次只要 1 步"的循环会永远通过。）
func TestAllowProjectsCurrentNeed(t *testing.T) {
	l := openAll()
	l.Steps = 5
	g := newTestGuard(t, l)
	g.ReportUse(Usage{Steps: 5})

	if d := g.Allow(Usage{Steps: 1}); d.OK {
		t.Error("已经用满 5 步，再来 1 步应当被拦（投影 = 6 > 5）")
	}
}

// ─── 结果里的人话 ───────────────────────────────────────────────────

// ⚠️ 剩余**不报负数** —— "还欠多少"不是调用方能用的信息。
//
//	（而它也不能靠"零值"表达"不限"：那是 `NoLimit` 的事。）
func TestRemainingIsClampedAtZero(t *testing.T) {
	l := openAll()
	l.Steps = 2 // 只有步数有限（成本维显式不限 —— 但它是 `openAll` 给的，见那条注释）
	g := newTestGuard(t, l)
	g.ReportUse(Usage{Steps: 5}) // 故意超出

	d := g.Allow(Usage{Steps: 1})
	if d.Remaining.Steps != 0 {
		t.Errorf("剩余应当被夹到 0，实际 %d", d.Remaining.Steps)
	}
	if d.Remaining.CostMilli != NoLimit {
		t.Errorf("不限的那一维剩余应当是 NoLimit，实际 %d", d.Remaining.CostMilli)
	}
}

// ⚠️ `Notice` 是给**模型**看的 —— 所以它必须**可执行**。
//
//	L2-9 实测的教训：预算不告诉模型，它会一直重试到触顶
//	（那次：同角色连续失败 10 次、55321 tokens 且最终失败）。
//
//	而"可执行"的意思是：**说清是哪一维不够 + 现在该做什么**，
//	而不是只报一个数字。
func TestNoticeIsActionableForTheModel(t *testing.T) {
	l := openAll()
	l.PromptTokens = 100
	g := newTestGuard(t, l)
	g.ReportUse(Usage{PromptTokens: 100})

	d := g.Allow(Usage{PromptTokens: 10})
	if d.OK {
		t.Fatal("应当被拦")
	}
	for _, want := range []string{
		"prompt_tokens", // 是哪一维
		"100",           // 上限
		"收尾",            // 该干什么
	} {
		if !strings.Contains(d.Notice, want) {
			t.Errorf("Notice 里应含 %q（可执行），实际：%q", want, d.Notice)
		}
	}
	// 而**成功**的时候不该有 Notice —— 别在一切正常时打扰模型
	if d := g.Allow(Usage{}); d.OK && d.Notice != "" {
		t.Errorf("没超限时不该有 Notice，实际 %q", d.Notice)
	}
}

// ─── 时间维：**报**出来的，不是**读**出来的（R43）────────────────────

// ⚠️⚠️ 判据的性质变了，所以这条测试的形状也变了 —— 而**变简单了**。
//
//	以前：`g.now()` 减 `g.started`。守卫**发明**了"这一轮从哪一刻开始"这个事实。
//	现在：`Spent.Elapsed` 由调用方**报** —— 与 token / 成本**完全同构**。
//
//	后果之一：**假时钟没了**（R23 那个"拿时间当判据会 flake"的顾虑在这里消失 ——
//	因为守卫不再看时钟）。见 `budget_test.go` 顶部那段关于 `fakeClock` 的注记。
//
//	⭐ 而它同时守住了一条**更重要**的性质：**时间维真的会被推进**。
//	  一个"永远报 `Elapsed: 0`"的调用方会让这一维**形同虚设** ——
//	  而那是**静默**的：不报错，只是永远不超时。
//	  （所以这条测试走的是**真实路径**：报 → 累加 → 拦。）
func TestWallTripsOnReportedElapsed(t *testing.T) {
	l := openAll()
	l.Elapsed = time.Minute
	g := newTestGuard(t, l)

	if d := g.Allow(Usage{Steps: 1}); !d.OK {
		t.Fatal("刚开始不该被时间拦")
	}

	// 报 59 秒的执行时间 —— 还没到上限
	g.ReportUse(Usage{Steps: 1, Elapsed: 59 * time.Second})
	if d := g.Allow(Usage{Steps: 1}); !d.OK {
		t.Error("已执行 59 秒，上限 1 分钟，不该拦")
	}

	// 再报 2 秒（共 61 秒）—— 超了
	g.ReportUse(Usage{Steps: 1, Elapsed: 2 * time.Second})
	d := g.Allow(Usage{Steps: 1})
	if d.OK {
		t.Fatal("已执行 61 秒 > 1 分钟，应当被拦")
	}
	if d.Exceeded != DimWall {
		t.Errorf("应当指出是 wall，实际 %q", d.Exceeded)
	}
	if !strings.Contains(d.Notice, "时间") {
		t.Errorf("Notice 该说清是时间，实际 %q", d.Notice)
	}
	// ⚠️ 超限时**剩余被夹到 0**（`remaining` 的规矩：剩余不报负数）
	if d.Remaining.Elapsed != 0 {
		t.Errorf("超限时剩余应当被夹到 0，实际 %v", d.Remaining.Elapsed)
	}

	// ⚠️⚠️ 而**没超的那些维，剩余必须照常算** —— 不能顺手给一个零值。
	//
	//	上一版这里写的是 `Remaining: Limits{}`，而 `Limits{}` 的零值
	//	读作"五维剩余都是 0"（= 没额度）—— **与实际矛盾**（那几维一点都没超）。
	//
	//	这正是 `EmptyLimiter` 那颗雷的复现：**同一个零值，两个相反的含义**。
	//
	//	⭐ R43 之后这条断言**更强了**：五维走同一条 `remainingAll()`，
	//	  所以"顺手给零值"这件事在结构上**已经做不到**了（只有一个地方算剩余）。
	if d.Remaining.Steps != NoLimit {
		t.Errorf(
			"时间超限不该让**量维**的剩余变成 0 —— 那会得出「预算全没了」这个错误的结论。"+
				"实际 Remaining = %s（其余四维都该是不限）",
			DescribeLimits(d.Remaining))
	}
}

// ⚠️ 时间的**零值**读作"**上限 0**"（fail-closed），与其余四维**完全一致**。
//
// # 这条测试守的是**一次被放弃的豁免**（R43）
//
//	我一度想让这一维的零值表示"不限"，理由是"**零时间的运行没有意义**"。
//	**而那个理由同样适用于 `Steps: 0`** —— 我们从没给步数开这个豁免。
//
//	> 判据：**如果一条豁免的理由同样适用于别的维度，那它就不是这一维的特性。**
//
//	（当时那个豁免还有个具体代价：`Limits{}` 的零值含义按维分裂 ——
//	 四维读作"上限 0"、一维读作"不限"。而"同一个零、两个相反的含义"
//	 正是本包开篇第一条规矩要防的东西。）
func TestWallZeroMeansZero(t *testing.T) {
	// 只写成本维 —— **没写 `Elapsed`** → 它的零值 = 上限 0
	l := openAll()
	l.CostMilli = 1000
	l.Elapsed = 0

	g := newTestGuard(t, l)

	// ⚠️ 必须报一个**有耗时**的步骤 —— 而这不是"测试写得别扭"，是一条真实的边界：
	//
	//	`Allow` 的边界规矩是 **`>` 而不是 `>=`**（"用满但没超必须放行"，
	//	见 `TestAllowAtExactBoundary`）。所以"上限 0 + 本步耗时 0"**是允许的** ——
	//	那读作"一个零耗时的操作"，而不是"超支了一点点"。
	//
	//	⭐ 它顺带说明了一件事：**`Elapsed: 0` 永远通不过时间维的拦截**。
	//	  所以"调用方忘了报耗时"这个错误**不会报错，只会让这一维形同虚设** ——
	//	  那正是 `TestWallTripsOnReportedElapsed` 要走真实路径的原因。
	d := g.Allow(Usage{CostMilli: 1, Elapsed: time.Second})
	if d.OK {
		t.Fatal("`Elapsed` 零值 → 上限 0，而本步要花 1 秒 → 应当被拦")
	}
	if d.Exceeded != DimWall {
		t.Errorf("应当指出是 wall，实际 %q", d.Exceeded)
	}

	// 而"上限 0 + 本步耗时 0"确实**放行** —— 边界是 `>`，不是 `>=`
	if d := g.Allow(Usage{CostMilli: 1}); !d.OK {
		t.Errorf("零耗时的步骤应当放行（边界是 > 而不是 >=），实际被 %q 拦了", d.Exceeded)
	}
}

// 要"不限时间"，与其余四维一样：**显式**写 `NoWall`。
//
// ⚠️ 而这条测试还要证明一件容易搞混的事：**`NoWall` 只关掉时间这一维**，
// 它不是"什么都不拦"。
func TestNoWallOnlyUnboundsTime(t *testing.T) {
	l := openAll()
	l.Steps = 3
	g := newTestGuard(t, l)

	// 报大量执行时间 + 把步数用光
	for i := 0; i < 10; i++ {
		g.ReportUse(Usage{Steps: 1, Elapsed: time.Hour})
	}

	d := g.Allow(Usage{Steps: 1})
	if d.OK {
		t.Fatal("步数早该超了")
	}
	if d.Exceeded != DimSteps {
		t.Errorf("应当指出是 steps（**而不是 wall**）—— 证明 NoWall 只是"+
			"这一维不限，不是什么都不拦，实际 %q", d.Exceeded)
	}
	// 而时间维的剩余要照常说"不限"
	if d.Remaining.Elapsed != NoWall {
		t.Errorf("时间不限时剩余应当是 NoWall，实际 %v", d.Remaining.Elapsed)
	}
}

// ─── 渲染 ───────────────────────────────────────────────────────────

func TestDescribeLimits(t *testing.T) {
	l := openAll()
	l.Steps, l.PromptTokens, l.CostMilli, l.Elapsed = 8, 50000, 12345, 90*time.Second

	got := DescribeLimits(l)
	for _, want := range []string{
		"steps=8",    // 计数维：直接打数字
		"¥12.345",    // 钱：厘 → 元
		"wall=1m30s", // ⚠️ 时间：纳秒 → 时长（R43 之前这一维**根本渲染不出来**）
	} {
		if !strings.Contains(got, want) {
			t.Errorf("渲染里应含 %q，实际 %q", want, got)
		}
	}
	// 不限的那一维要**显式**说"不限"（`openAll` 里 completion 是不限的）
	if !strings.Contains(got, "completion_tokens=不限") {
		t.Errorf("不限的维度应当显式渲染成「不限」，实际 %q", got)
	}
}

func TestUsageString(t *testing.T) {
	got := Usage{Steps: 5, PromptTokens: 4820, CompletionTokens: 1130, CostMilli: 1234}.String()
	for _, want := range []string{"5", "4820", "1130", "¥1.234"} {
		if !strings.Contains(got, want) {
			t.Errorf("Usage.String() 里应含 %q，实际 %q", want, got)
		}
	}
}
