package graph

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// validInterrupt 返回一条**合法**的中断，供各用例当基线。
//
// 每个"应该被拒绝"的用例都可以从它出发只改一个字段 ——
// 这样断言失败时能确定"是那一个字段引起的"，而不是"不知道哪个字段有问题"。
func validInterrupt() Interrupt {
	return NewInterrupt("run-1", "outline", 1, KindConfirmArtifact, "请确认这份大纲")
}

// ─── 目录（单一真相来源）────────────────────────────────────────────

// 目录必须是**唯一**的合法性来源：表里有的都合法，表外的一律不合法。
//
// 这条测试的作用：任何人新增一个类型，只要忘了加进 interruptKinds()，
// 或者加进了常量却没加进表，这里立刻报错。
// —— 它在守的就是 F1 那个"约束没有单一来源"的病。
func TestInterruptKindCatalogIsSingleSource(t *testing.T) {
	for _, s := range interruptKinds() {
		if !s.Kind.Valid() {
			t.Errorf("目录里的 %q 竟然不合法 —— 说明 Valid 没查这张表", s.Kind)
		}
	}

	bogus := []InterruptKind{"", "FollowUpInfo", "confirm-artifact", "CONFIRM_ARTIFACT"}
	for _, k := range bogus {
		if k.Valid() {
			t.Errorf("%q 不该被当成合法类型", k)
		}
	}
}

// 每个类型都必须有一句人能读懂的说明。
//
// 因为它是**唯一**会被渲染给模型/人看的中断说明来源，
// 空说明等于让调用方自己编一句 —— 那就是"两处描述同一件事"。
func TestEveryKindHasDescription(t *testing.T) {
	for _, s := range interruptKinds() {
		if strings.TrimSpace(s.Description) == "" {
			t.Errorf("类型 %q 没有说明", s.Kind)
		}
	}
}

// ⚠️ 不变量：**每一个 kind 都必须允许 abort**。
//
// 理由：一个只能"继续"的 HITL 不是 HITL，而是一个必须无条件服从的弹窗。
// 这条规则写在注释里会漂移，写成测试不会。
func TestEveryKindAllowsAbort(t *testing.T) {
	for _, s := range interruptKinds() {
		ok := false
		for _, i := range s.Intents {
			if i == IntentAbort {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("类型 %q 不允许 abort —— 人必须永远有权说「不做了」", s.Kind)
		}
	}
}

// 对外暴露的清单必须是**副本**：调用方改了它，不能污染目录。
func TestInterruptKindNamesIsCopy(t *testing.T) {
	first := InterruptKindNames()
	if len(first) == 0 {
		t.Fatal("类型清单为空")
	}
	first[0] = "被改坏了"

	if got := InterruptKindNames(); got[0] == "被改坏了" {
		t.Errorf("InterruptKindNames 返回的不是副本：第二次调用被第一次的修改污染了")
	}
}

func TestIntentCatalog(t *testing.T) {
	if len(intentCatalog()) != 3 {
		t.Errorf("意图应有 3 个，实际 %d 个", len(intentCatalog()))
	}
	for _, i := range intentCatalog() {
		if !i.Valid() {
			t.Errorf("目录里的意图 %q 竟然不合法", i)
		}
	}
	if Intent("whatever").Valid() {
		t.Error("乱写的意图不该合法")
	}

	names := IntentNames()
	names[0] = "被改坏了"
	if IntentNames()[0] == "被改坏了" {
		t.Error("IntentNames 返回的不是副本")
	}
}

// ─── 构造：确定性的 ID ──────────────────────────────────────────────

// ID 必须由状态**确定性**派生 —— 这是"可重放"在协议层的落地。
//
// 若 ID 随机：同一份状态重跑会得到不同的 ID，之前存下的答案就对不上了。
func TestNewInterruptDerivesDeterministicID(t *testing.T) {
	a := NewInterrupt("run-1", "outline", 1, KindConfirmArtifact, "确认大纲？")
	b := NewInterrupt("run-1", "outline", 1, KindConfirmArtifact, "确认大纲？")

	if a.ID != b.ID {
		t.Errorf("同一状态必须派生出同一个 ID：%q vs %q", a.ID, b.ID)
	}
	if a.ID != "run-1:outline:1" {
		t.Errorf("ID 形状不符合预期：%q", a.ID)
	}
}

// 同一个节点**第几次**中断必须体现在 ID 上。
//
// 否则：用户 revise 之后重新中断，会算出和上次一样的 ID，
// 于是"上次的答案"被套用到"这次的问题"上 —— 极难查的串台。
func TestNewInterruptDistinguishesAttempts(t *testing.T) {
	a := NewInterrupt("run-1", "outline", 1, KindConfirmArtifact, "确认大纲？")
	b := NewInterrupt("run-1", "outline", 2, KindConfirmArtifact, "确认大纲？")

	if a.ID == b.ID {
		t.Errorf("不同 attempt 必须得到不同 ID，实际都是 %q", a.ID)
	}
}

// 各字段要规范化：attempt < 1 归一为 1，首尾空白要去掉。
func TestNewInterruptNormalizes(t *testing.T) {
	in := NewInterrupt("  run-1  ", " outline ", 0, KindConfirmArtifact, "  问一句  ")

	if in.RunID != "run-1" || in.Node != "outline" || in.Prompt != "问一句" {
		t.Errorf("首尾空白没有被去掉：%+v", in)
	}
	if in.Attempt != 1 {
		t.Errorf("attempt<1 应归一为 1，实际 %d", in.Attempt)
	}
	if in.Version != InterruptVersion {
		t.Errorf("新建的中断必须带当前协议版本，实际 %d", in.Version)
	}
	if in.CreatedAt.IsZero() {
		t.Error("新建的中断必须有 created_at")
	}
	if err := in.Validate(); err != nil {
		t.Errorf("归一化之后应当合法，却报：%v", err)
	}
}

// ─── Interrupt.Validate ────────────────────────────────────────────

func TestInterruptValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Interrupt)
		wantSub string
	}{
		{
			"版本为 0（忘了填）",
			func(in *Interrupt) { in.Version = 0 },
			"protocol version=0 不合法",
		},
		{
			"版本为负",
			func(in *Interrupt) { in.Version = -1 },
			"protocol version=-1 不合法",
		},
		{
			"缺 id",
			func(in *Interrupt) { in.ID = "" },
			"缺少 id",
		},
		{
			"缺 run_id",
			func(in *Interrupt) { in.RunID = "" },
			"缺少 run_id",
		},
		{
			"缺 node（恢复时不知道该重进哪一跳）",
			func(in *Interrupt) { in.Node = "" },
			"不知道该重进哪一跳",
		},
		{
			"attempt 从 0 开始",
			func(in *Interrupt) { in.Attempt = 0 },
			"attempt=0",
		},
		{
			"类型不在目录里",
			func(in *Interrupt) { in.Kind = "FollowUpInfo" },
			"不合法",
		},
		{
			"没有 prompt（人无法回答）",
			func(in *Interrupt) { in.Prompt = "   " },
			"人会一直等下去",
		},
		{
			"缺 created_at",
			func(in *Interrupt) { in.CreatedAt = time.Time{} },
			"缺少 created_at",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validInterrupt()
			c.mutate(&in)

			err := in.Validate()
			if err == nil {
				t.Fatalf("应当被拒绝，却通过了")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误信息里应包含 %q，实际：%v", c.wantSub, err)
			}
		})
	}

	// 基线本身必须合法 —— 否则上面每个用例都可能是"因为别的原因"失败的。
	if err := validInterrupt().Validate(); err != nil {
		t.Fatalf("基线中断应当合法，却报：%v", err)
	}
}

// options 的规则**由 kind 决定**，两个方向都要挡。
func TestInterruptValidateOptionsPerKind(t *testing.T) {
	t.Run("需要选项的类型，少于 2 个要拒", func(t *testing.T) {
		in := NewInterrupt("run-1", "pick", 1, KindChooseOption, "选一个")
		in.Options = []string{"只有一个"}
		err := in.Validate()
		if err == nil || !strings.Contains(err.Error(), "至少 2 个选项") {
			t.Errorf("应当因为选项太少被拒，实际：%v", err)
		}
	})

	t.Run("需要选项的类型，2 个就通过", func(t *testing.T) {
		in := NewInterrupt("run-1", "pick", 1, KindChooseOption, "选一个")
		in.Options = []string{"方案甲", "方案乙"}
		if err := in.Validate(); err != nil {
			t.Errorf("应当通过，却报：%v", err)
		}
	})

	t.Run("不需要选项的类型，夹带选项也要拒", func(t *testing.T) {
		// 反向校验容易被忽略，但它防的是**自相矛盾**：
		// Prompt 里没让人挑，却给了 options —— 下游会以为该从中挑一个。
		in := validInterrupt()
		in.Options = []string{"甲", "乙"}
		err := in.Validate()
		if err == nil || !strings.Contains(err.Error(), "不接受 options") {
			t.Errorf("应当因为夹带选项被拒，实际：%v", err)
		}
	})

	t.Run("空选项要拒", func(t *testing.T) {
		in := NewInterrupt("run-1", "pick", 1, KindChooseOption, "选一个")
		in.Options = []string{"甲", "  "}
		err := in.Validate()
		if err == nil || !strings.Contains(err.Error(), "options[1] 为空") {
			t.Errorf("应当因为空选项被拒，实际：%v", err)
		}
	})
}

// 拒绝必须**可自愈**（L1-5）：错误信息里要带上全部合法类型名。
func TestInterruptErrorListsLegalKinds(t *testing.T) {
	in := validInterrupt()
	in.Kind = "编的类型"

	err := in.Validate()
	if err == nil {
		t.Fatal("应当被拒绝")
	}
	for _, name := range InterruptKindNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("错误信息里应列出合法类型 %q，实际：%v", name, err)
		}
	}
}

// ─── Decision.Validate ─────────────────────────────────────────────

func TestDecisionValidate(t *testing.T) {
	cases := []struct {
		name    string
		kind    InterruptKind
		d       Decision
		wantSub string // "" 表示应当通过
	}{
		{"意图乱写", KindConfirmArtifact, Decision{Intent: "accept"}, "不合法"},
		{"确认", KindConfirmArtifact, Decision{Intent: IntentConfirm}, ""},
		{"放弃（不需要理由）", KindConfirmArtifact, Decision{Intent: IntentAbort}, ""},
		{"允许 revise 的类型，带 note 就通过", KindConfirmArtifact,
			Decision{Intent: IntentRevise, Note: "第 3 页换成建议"}, ""},
		{"允许 revise 的类型，没 note 也要拒", KindConfirmArtifact,
			Decision{Intent: IntentRevise}, "必须说明要改什么"},
		{"追问类不接受 revise（它不知道「改什么」该怎么用）", KindAskUser,
			Decision{Intent: IntentRevise, Note: "改成 2027"}, "不接受 \"revise\""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := NewInterrupt("run-1", "node", 1, c.kind, "问一句")
			err := c.d.Validate(in)

			if c.wantSub == "" {
				if err != nil {
					t.Errorf("应当通过，却报：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应当被拒绝，却通过了")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误信息里应包含 %q，实际：%v", c.wantSub, err)
			}
		})
	}
}

// 拒绝时必须说清"是哪个类型不接受"，并且列出它**能**接受什么（可自愈）。
func TestRejectedIntentListsWhatIsAllowed(t *testing.T) {
	in := NewInterrupt("run-1", "ask", 1, KindAskUser, "你指的是哪一年？")

	err := Decision{Intent: IntentRevise, Note: "改成 2027"}.Validate(in)
	if err == nil {
		t.Fatal("ask_user 不该接受 revise")
	}
	if !strings.Contains(err.Error(), string(KindAskUser)) {
		t.Errorf("错误里应指明是哪个类型不接受，实际：%v", err)
	}
	// 它应该告诉调用方这个类型**能**接受哪些意图
	for _, i := range []string{string(IntentConfirm), string(IntentAbort)} {
		if !strings.Contains(err.Error(), i) {
			t.Errorf("错误里应列出该类型可接受的意图 %q，实际：%v", i, err)
		}
	}
}

// ⚠️ 这条测试锁住的是**判据**，不是文案：
// **只拦"不满足就不能继续执行"的情况。**
//
//	revise 没有 Note → 代码不知道重做什么 → 不可执行 → 必须拒
//	abort  没有 Note → 放弃不需要理由   → 信息少而已 → 不该卡住人
func TestReviseNeedsNoteButAbortDoesNot(t *testing.T) {
	in := validInterrupt()

	err := Decision{Intent: IntentRevise}.Validate(in)
	if err == nil || !strings.Contains(err.Error(), "必须说明要改什么") {
		t.Errorf("revise 缺 note 必须被拒，实际：%v", err)
	}

	// 补上 note 就通过 —— 证明被拒的原因**只有** note，不是别的。
	if err := (Decision{Intent: IntentRevise, Note: "第 3 页换成建议"}).Validate(in); err != nil {
		t.Errorf("带上 note 的 revise 应当通过，却报：%v", err)
	}

	if err := (Decision{Intent: IntentAbort}).Validate(in); err != nil {
		t.Errorf("abort 不需要理由，不该被拒，实际：%v", err)
	}
}

// 意图非法时也要给合法清单（可自愈）。
func TestDecisionErrorListsLegalIntents(t *testing.T) {
	err := Decision{Intent: "nope"}.Validate(validInterrupt())
	if err == nil {
		t.Fatal("应当被拒绝")
	}
	for _, n := range IntentNames() {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("错误信息里应列出合法意图 %q，实际：%v", n, err)
		}
	}
}

// ─── 版本兼容窗口（参数化，才能测到 TooOld）─────────────────────────

// checkProtocolVersion 的 `current` 是参数，所以可以直接模拟"未来版本"。
//
//	这是 `schema.Latest` 的同一条手法（原则五）：
//	把规则做成参数，它就能被单测 —— 而不必先把生产常量改坏才能测到那条分支。
func TestCheckProtocolVersion(t *testing.T) {
	cases := []struct {
		name    string
		v       int
		min     int
		current int
		want    ResumeErrorKind // "" 表示应当通过
	}{
		{"正好是当前版本", 1, 1, 1, ""},
		{"在兼容窗口内（老但能读）", 1, 1, 3, ""},
		{"比当前新（滚动发布期间的新实例写的）", 4, 1, 1, ResumeFromFuture},
		{"太老，低于最低可读版本", 1, 2, 3, ResumeTooOld},
		{"太老（边界：比 min 小 1）", 1, 2, 2, ResumeTooOld},
		{"正好等于 min（可读）", 2, 2, 3, ""},
		// ⚠️ 这两条是**报错了对象**的那个 bug 的看门人：
		//    版本字段没被填写，不是"协议太老"，而是"记录没写好"。
		{"版本字段是 0（根本没被写过）", 0, 1, 1, ResumeCorrupt},
		{"版本字段是负数", -1, 1, 1, ResumeCorrupt},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkProtocolVersion(c.v, c.min, c.current)
			if c.want == "" {
				if err != nil {
					t.Errorf("应当通过，却报：%v", err)
				}
				return
			}
			var re *ResumeError
			if !errors.As(err, &re) {
				t.Fatalf("应当是 *ResumeError，实际 %T：%v", err, err)
			}
			if re.Kind != c.want {
				t.Errorf("应当是 %s，实际 %s（%v）", c.want, re.Kind, err)
			}
		})
	}
}

// 当前 `min == current == 1`，所以"太老"分支在**生产调用路径上**不可达。
//
// 这条测试存在的意义不是覆盖率，而是：**证明那段代码不是死的** ——
// `current` 涨到 2 的那一天，它的行为已经是确定的，不用临时再想。
func TestTooOldBranchIsWiredEvenThoughUnreachableToday(t *testing.T) {
	if InterruptMinVersion != InterruptVersion {
		t.Skip("min 与 current 已经拉开，本测试的前提变了")
	}
	// 用参数把"未来"模拟出来 —— 这正是把它做成参数的原因。
	//
	// ⚠️ 注意这里是 (1, 2, 2) 而不是 (0, 1, 1)：版本 0 现在是 **corrupt**
	// 而不是 too_old，用 0 会测到另一条分支（那也是一个真实的 bug 修完之后的样子）。
	err := checkProtocolVersion(InterruptVersion, InterruptVersion+1, InterruptVersion+1)
	var re *ResumeError
	if !errors.As(err, &re) || re.Kind != ResumeTooOld {
		t.Errorf("应当得到 too_old，实际：%v", err)
	}
}

// ─── CheckResumable：三步，顺序不能换 ──────────────────────────────

func TestCheckResumable(t *testing.T) {
	now := time.Now().UTC()

	t.Run("正常可恢复", func(t *testing.T) {
		if err := validInterrupt().CheckResumable(now); err != nil {
			t.Errorf("应当可恢复，却报：%v", err)
		}
	})

	t.Run("业务上已过期", func(t *testing.T) {
		in := validInterrupt()
		in.ExpiresAt = now.Add(-time.Minute)

		err := in.CheckResumable(now)
		var re *ResumeError
		if !errors.As(err, &re) || re.Kind != ResumeExpired {
			t.Fatalf("应当得到 expired，实际：%v", err)
		}
		if !strings.Contains(err.Error(), "过期") {
			t.Errorf("消息里应说明是过期，实际：%v", err)
		}
	})

	t.Run("没有 ExpiresAt 就不算过期", func(t *testing.T) {
		in := validInterrupt()
		if !in.ExpiresAt.IsZero() {
			t.Fatal("基线不该带过期时间")
		}
		if err := in.CheckResumable(now.Add(1000 * time.Hour)); err != nil {
			t.Errorf("零值 ExpiresAt 表示不过期，却报：%v", err)
		}
	})

	t.Run("结构损坏 → corrupt（不是 bad_decision）", func(t *testing.T) {
		in := validInterrupt()
		in.Prompt = ""

		err := in.CheckResumable(now)
		var re *ResumeError
		if !errors.As(err, &re) {
			t.Fatalf("应当是 *ResumeError，实际 %T", err)
		}
		// 这条断言很重要：记录自己写坏了，与"人给的答案不对"是两件事。
		// 混在一起会让一个磁盘故障看起来像一次用户误操作。
		if re.Kind != ResumeCorrupt {
			t.Errorf("应当是 corrupt，实际 %s", re.Kind)
		}
	})

	t.Run("来自未来版本", func(t *testing.T) {
		in := validInterrupt()
		in.Version = InterruptVersion + 1

		err := in.CheckResumable(now)
		var re *ResumeError
		if !errors.As(err, &re) || re.Kind != ResumeFromFuture {
			t.Fatalf("应当得到 from_future，实际：%v", err)
		}
		// 它必须是**可重试**的：用户只需要再点一次，而不是"重新开始"。
		if !re.Kind.Retryable() {
			t.Error("from_future 必须标记为可重试")
		}
		if !strings.Contains(err.Error(), "路由") {
			t.Errorf("消息里应给出可执行的处置建议，实际：%v", err)
		}
	})
}

// ⚠️ 本轮最重要的**顺序**测试：版本必须在结构之前判。
//
// 一条来自未来版本的记录，**结构可能已经变了**（v2 把 prompt 改名成 question）。
// 用 v1 的规则去校验它，会得出"缺少 prompt"——
// 而真实原因是"字段改名了"，不是"它写错了"。
//
// **误导性的错误比没有错误更难查。**
func TestResumeChecksVersionBeforeStructure(t *testing.T) {
	in := validInterrupt()
	in.Version = InterruptVersion + 5 // 来自未来
	in.Prompt = ""                    // 同时：在 v1 的规则下结构也不完整
	in.Kind = ""                      // 而且类型也认不出来

	err := in.CheckResumable(time.Now().UTC())
	var re *ResumeError
	if !errors.As(err, &re) {
		t.Fatalf("应当是 *ResumeError，实际 %T：%v", err, err)
	}
	if re.Kind != ResumeFromFuture {
		t.Errorf("必须先报版本问题（from_future），实际报了 %s —— 顺序反了", re.Kind)
	}
	if strings.Contains(err.Error(), "prompt") {
		t.Errorf("不该把版本问题报成结构问题：%v", err)
	}
}

// ⚠️ 第二条顺序测试：**先确认路能走，再判断往哪走。**
//
// 一条已经过期的中断，即便同时带着一个不合法的答案，
// `CheckResumable` 也只能报"过期" ——
// 若它先校验答案，用户会看到"你的答案不合法"，于是去改答案、
// 再提交、再被拒…… 而他真正该做的是**重新开始**。
func TestResumeChecksExpiryBeforeAnswer(t *testing.T) {
	in := validInterrupt()
	in.ExpiresAt = time.Now().UTC().Add(-time.Hour)

	// 答案的校验根本不归 CheckResumable 管（那是 Decision.Validate 的事），
	// 所以这里不需要"同时给一个坏答案"—— 只要证明它不会被答案的操作干扰即可。
	var re *ResumeError
	err := in.CheckResumable(time.Now().UTC())
	if !errors.As(err, &re) || re.Kind != ResumeExpired {
		t.Errorf("应当只报 expired，实际：%v", err)
	}

	// 反过来确认一遍：这条中断确实**也**会让一个非法答案被拒，
	// 但两条路径是分开的，各自报各自的。
	if err := (Decision{Intent: IntentRevise}).Validate(in); err == nil {
		t.Error("非法答案应当由 Decision.Validate 拒绝，与 CheckResumable 无关")
	}
}

func TestResumeErrorRetryable(t *testing.T) {
	cases := []struct {
		kind ResumeErrorKind
		want bool
	}{
		{ResumeFromFuture, true},  // 路由到新实例就行
		{ResumeBadDecision, true}, // 让人改答案
		{ResumeNotFound, false},   // 只能重新开始
		{ResumeExpired, false},
		{ResumeCorrupt, false}, // 磁盘/代码问题，重试没有意义
		{ResumeTooOld, false},  // 协议太老
		{ResumeErrorKind("??"), false},
	}
	for _, c := range cases {
		if got := c.kind.Retryable(); got != c.want {
			t.Errorf("%s.Retryable() = %v，期望 %v", c.kind, got, c.want)
		}
	}
}

func TestResumeErrorMessage(t *testing.T) {
	withDetail := &ResumeError{Kind: ResumeExpired, ID: "r:n:1", Detail: "已于 X 过期"}
	if !strings.Contains(withDetail.Error(), "expired") ||
		!strings.Contains(withDetail.Error(), "已于 X 过期") {
		t.Errorf("错误信息应同时带类型与细节：%v", withDetail)
	}

	noDetail := &ResumeError{Kind: ResumeNotFound, ID: "r:n:1"}
	if !strings.Contains(noDetail.Error(), "not_found") {
		t.Errorf("没有细节时也要能看出类型：%v", noDetail)
	}
}

// ⚠️ 错误信息必须说清"**这是什么记录**读不出来"。
//
//	同一个 `ResumeError` 被两处共用（中断 / 快照），而第一版把"中断"写死在文案里 ——
//	于是**快照读不出来时，打出来的是「无法恢复中断」**。
//
//	这不是措辞问题：它会把排查引向"中断是不是过期了"，而真实原因是
//	"磁盘上那个文件坏了"。**报错了对象，比没有错误更难查。**
func TestResumeErrorNamesTheRightSubject(t *testing.T) {
	in := validInterrupt()
	in.Version = InterruptVersion + 1 // 来自未来 → 可恢复性检查会失败
	err := in.CheckResumable(time.Now().UTC())
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), "中断") {
		t.Errorf("中断的问题应当说「中断」：%v", err)
	}
	if strings.Contains(err.Error(), "快照") {
		t.Errorf("中断的问题不该说「快照」：%v", err)
	}

	st := newTestState(t)
	st.Version = StateVersion + 1
	serr := st.CheckRestorable()
	if serr == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(serr.Error(), "快照") {
		t.Errorf("快照的问题应当说「快照」：%v", serr)
	}
	if strings.Contains(serr.Error(), "无法恢复中断") {
		t.Errorf("**这里曾经写死成「中断」** —— 报错了对象：%v", serr)
	}
}

// 没有 id 时不该打出一对空引号（`快照 ""` 看着像出了别的问题）。
func TestResumeErrorOmitsEmptyID(t *testing.T) {
	_, err := UnmarshalState([]byte("{"))
	if err == nil {
		t.Fatal("截断的 JSON 应当报错")
	}
	if strings.Contains(err.Error(), `""`) {
		t.Errorf("没有 id 时不该出现空引号：%v", err)
	}
	if !strings.Contains(err.Error(), "快照") {
		t.Errorf("应当说清是快照的问题：%v", err)
	}
}

// ─── 序列化：它是要跨进程的，回环必须干净 ──────────────────────────

func TestInterruptJSONRoundTrip(t *testing.T) {
	in := validInterrupt()
	in.Payload = json.RawMessage(`{"pages":[{"title":"行业现状"}]}`)
	in.Options = nil
	in.ExpiresAt = time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}

	var back Interrupt
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}

	if back.ID != in.ID || back.Node != in.Node || back.Attempt != in.Attempt {
		t.Errorf("身份字段丢了：%+v", back)
	}
	if back.Kind != in.Kind || back.Prompt != in.Prompt {
		t.Errorf("协议字段丢了：%+v", back)
	}
	if !bytes.Equal(back.Payload, in.Payload) {
		t.Errorf("payload 不一致：%s vs %s", back.Payload, in.Payload)
	}
	if !back.ExpiresAt.Equal(in.ExpiresAt) {
		t.Errorf("过期时间不一致：%v vs %v", back.ExpiresAt, in.ExpiresAt)
	}

	// 读回来的记录必须仍然可用 —— 这是"跨进程"的最低要求。
	if err := back.CheckResumable(time.Now().UTC()); err != nil {
		t.Errorf("回环之后应当仍可恢复，却报：%v", err)
	}
}

// 同一个中断序列化两次，字节必须**完全一致**。
//
// 理由和 L1-6 那条"输出必须稳定"是同一条：
// 不稳定就无法 diff、无法做 golden 测试、无法断言"恢复前后一致"。
// （`json.Marshal` 对 `RawMessage` 会做 compact，所以空白差异不会漏进来。）
func TestInterruptJSONIsStable(t *testing.T) {
	in := validInterrupt()
	in.Payload = json.RawMessage("{\"a\":  1,\n  \"b\":2}")

	first, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("两次序列化结果不一致：\n%s\n%s", first, second)
	}
}

// 决策也要能干净地回环 —— 它同样要跨进程（人是在另一个请求里回答的）。
func TestDecisionJSONRoundTrip(t *testing.T) {
	d := Decision{Intent: IntentRevise, Note: "第 3 页换成建议"}

	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var back Decision
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back != d {
		t.Errorf("回环不一致：%+v vs %+v", back, d)
	}
	if err := back.Validate(validInterrupt()); err != nil {
		t.Errorf("回环之后的决策应当仍合法，却报：%v", err)
	}
}
