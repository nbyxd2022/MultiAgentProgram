package agents

import (
	"context"
	"strings"
	"testing"

	"multiagent/internal/schema"
	"multiagent/internal/state"
	"multiagent/internal/tool"
)

// 这个文件复现并守住 **L2-1**：黑板的"不可变引用"在装配处被降级成"最新版"，
// 于是**上游重做之后，下游拿着一份"基于旧上游做出来的"产物继续往下走**，
// 而且没有任何一层能发现。
//
// 场景（旧行为下完全静默）：
//
//	outline@1 ──> design@1        第 1 轮（design@1 的版式是按 outline@1 的页面做的）
//	outline 重跑 ──> outline@2    第 2 轮
//	再渲染：collectInputs 取到 outline@2 + design@1
//	        renderer 只校验"页数一致" → 3 == 3 → 通过 ✅（错配）
//
// ⚠️ 这个文件的写法有讲究：**它只使用修复前就存在的 API**，
//    所以它在修复前能编译、并且会**逻辑上变红**。
//    （R25 那一轮的复现测试因为引用了还不存在的字段而编译不过 ——
//     那只能证明"能力缺失"，证明不了"旧行为会误判"。这次的写法补上了这一点。）

// stubAgent 是一个行为完全确定的替身：按 kind 产出一份最简的合法产物。
type stubAgent struct {
	name  string
	kind  schema.Kind
	needs []schema.Kind
	runs  int
}

func (s *stubAgent) Name() string         { return s.name }
func (s *stubAgent) Kind() schema.Kind    { return s.kind }
func (s *stubAgent) Description() string  { return "替身：" + s.name }
func (s *stubAgent) Needs() []schema.Kind { return s.needs }

func (s *stubAgent) Run(_ context.Context, _ SubAgentInput) (schema.Payload, error) {
	s.runs++
	switch s.kind {
	case schema.KindOutline:
		return schema.Outline{Pages: []schema.PageOutline{
			{Index: 0, Title: "行业现状", Points: []string{"p"}},
			{Index: 1, Title: "核心矛盾", Points: []string{"p", "q", "r"}},
			{Index: 2, Title: "结论与建议", Points: []string{"p"}},
		}}, nil
	case schema.KindDesign:
		return schema.Design{Pages: []schema.PageDesign{
			{PageIndex: 0, Layout: schema.LayoutCover, Note: "开场"},
			{PageIndex: 1, Layout: schema.LayoutBullets, Note: "正文"},
			{PageIndex: 2, Layout: schema.LayoutClosing, Note: "收束"},
		}}, nil
	case schema.KindRender:
		return schema.RenderResult{Files: []string{"out/a.html"}}, nil
	default:
		return schema.Research{
			Topic:    "主题",
			Findings: []schema.Finding{{Claim: "c", Source: "web_search#1"}},
		}, nil
	}
}

func toolFor(t *testing.T, board *state.Blackboard, sub SubAgent) tool.Tool {
	t.Helper()
	tl, err := NewSubAgentTool(sub, board, "主题")
	if err != nil {
		t.Fatalf("包装子 Agent 失败: %v", err)
	}
	return tl
}

// runOnce 跑一跳并断言它成功。
func runOnce(t *testing.T, board *state.Blackboard, sub SubAgent) {
	t.Helper()
	out, err := toolFor(t, board, sub).Execute(context.Background(), "{}")
	if err != nil {
		t.Fatalf("这一跳本应成功：%v", err)
	}
	if !strings.Contains(out, `"ok":true`) {
		t.Fatalf("回执应当表示成功：%s", out)
	}
}

// TestStaleUpstreamIsRejected 是 L2-1 的复现用例。
func TestStaleUpstreamIsRejected(t *testing.T) {
	board := state.NewBlackboard()
	ctx := context.Background()

	// 第 1 轮：outline@1 → design@1
	runOnce(t, board, &stubAgent{name: "outline", kind: schema.KindOutline})
	runOnce(t, board, &stubAgent{
		name: "designer", kind: schema.KindDesign,
		needs: []schema.Kind{schema.KindOutline},
	})

	// 上游被重做：outline 出了第 2 版（内容可能变了）
	runOnce(t, board, &stubAgent{name: "outline", kind: schema.KindOutline})

	// 现在派 renderer：needs = [outline, design]
	render := &stubAgent{
		name: "renderer", kind: schema.KindRender,
		needs: []schema.Kind{schema.KindOutline, schema.KindDesign},
	}
	out, err := toolFor(t, board, render).Execute(ctx, "{}")

	// ★ 核心断言：design@1 是在 outline@1 之上做出来的，而当前是 outline@2 ——
	//   这一跳必须被拒绝（否则渲染出的就是"新内容 + 旧版式"的错配产物）
	if err == nil {
		t.Fatalf("上游重做之后，基于旧上游的产物必须让这一跳被拒绝；实际返回：%s", out)
	}
	if !strings.Contains(err.Error(), "过期") {
		t.Errorf("错误信息应当说明「依据已过期」：%v", err)
	}
	// 拒绝必须**可自愈**：要告诉模型重新调度谁（L1-5：反馈要可执行）
	if !strings.Contains(err.Error(), "designer") {
		t.Errorf("错误信息应当指出该重新调度哪个角色：%v", err)
	}
	// 被拒绝就不该产生副作用
	if render.runs != 0 {
		t.Errorf("前置检查不通过时，子 Agent 不该被真正执行（实际执行 %d 次）", render.runs)
	}
	if board.Versions(string(schema.KindRender)) != 0 {
		t.Error("被拒绝的这一跳不该往黑板里写任何东西")
	}
}

// TestFreshUpstreamIsAccepted 是上面那个用例的**反向约束**。
//
// 没有它，一个"永远报过期"的实现在上面也是绿的 ——
// 那条测试只证明了"能发现过期"，没证明"没过期时不误报"。
func TestFreshUpstreamIsAccepted(t *testing.T) {
	board := state.NewBlackboard()

	runOnce(t, board, &stubAgent{name: "outline", kind: schema.KindOutline})
	runOnce(t, board, &stubAgent{
		name: "designer", kind: schema.KindDesign,
		needs: []schema.Kind{schema.KindOutline},
	})

	// 上游没有再动过 → 这一跳必须放行
	render := &stubAgent{
		name: "renderer", kind: schema.KindRender,
		needs: []schema.Kind{schema.KindOutline, schema.KindDesign},
	}
	runOnce(t, board, render)

	if render.runs != 1 {
		t.Errorf("没有过期时应当正常执行，实际执行 %d 次", render.runs)
	}
}

// TestAdapterRecordsBasis 守住"血缘真的被记下来了"。
//
// 上面三个用例都在验证"过期能被发现"，但它们**不依赖血缘被正确记录**这件事本身
// （只有记录了才谈得上检查）。这条用例把"记录"这个动作单独钉住：
// 一旦有人把 `a.BasedOn = basisOf(inputs)` 那行删掉，
// 新鲜度检查会立刻变成"永远查不出问题"，而且上面三个用例都会变红 ——
// 但原因会很难看懂。这条用例让原因一眼可见。
func TestAdapterRecordsBasis(t *testing.T) {
	board := state.NewBlackboard()

	runOnce(t, board, &stubAgent{name: "outline", kind: schema.KindOutline})
	runOnce(t, board, &stubAgent{
		name: "designer", kind: schema.KindDesign,
		needs: []schema.Kind{schema.KindOutline},
	})

	root, err := board.Get("outline")
	if err != nil {
		t.Fatalf("取产物失败: %v", err)
	}
	if len(root.BasedOn) != 0 {
		t.Errorf("没有上游的角色不该凭空产生血缘，实际 %+v", root.BasedOn)
	}

	derived, err := board.Get("design")
	if err != nil {
		t.Fatalf("取产物失败: %v", err)
	}
	if len(derived.BasedOn) != 1 {
		t.Fatalf("应当记录 1 条依据，实际 %+v", derived.BasedOn)
	}
	if derived.BasedOn[0] != (schema.Basis{Key: "outline", Version: 1}) {
		t.Errorf("依据应当精确到版本，实际 %+v", derived.BasedOn[0])
	}
}

// TestBasisFollowsLatestUpstream 守住"重做之后血缘会更新"。
//
// 上游出了 @2 之后重新调度下游：新产物的依据必须指向 @2，
// 否则它一出生就是过期的 —— 那等于把问题原样传下去。
func TestBasisFollowsLatestUpstream(t *testing.T) {
	board := state.NewBlackboard()

	designer := &stubAgent{
		name: "designer", kind: schema.KindDesign,
		needs: []schema.Kind{schema.KindOutline},
	}

	runOnce(t, board, &stubAgent{name: "outline", kind: schema.KindOutline})
	runOnce(t, board, designer) // design@1，依据 outline@1

	runOnce(t, board, &stubAgent{name: "outline", kind: schema.KindOutline})
	runOnce(t, board, designer) // design@2，应当依据 outline@2

	a, err := board.Get("design") // 最新版
	if err != nil {
		t.Fatalf("取产物失败: %v", err)
	}
	if a.Version != 2 {
		t.Fatalf("应当拿到第 2 版，实际 %d", a.Version)
	}
	if len(a.BasedOn) != 1 || a.BasedOn[0].Version != 2 {
		t.Errorf("重做之后的依据应当指向 outline@2，实际 %+v", a.BasedOn)
	}
}

// TestStaleChainIsDetectedThroughUpstreams 守住"传递性"。
//
// 反例构造：让**直接**上游保持一致，只有**更上游**变了。
//
//	research@1 → outline@1（依据 research@1）→ design@1（依据 outline@1）
//	然后 research 出了 @2，outline 停在 @1。
//
// 此时 design@1 的直接依据 outline@1 **仍然是最新** —— 只看一层发现不了。
func TestStaleChainIsDetectedThroughUpstreams(t *testing.T) {
	board := state.NewBlackboard()

	runOnce(t, board, &stubAgent{name: "research", kind: schema.KindResearch})
	runOnce(t, board, &stubAgent{
		name: "outline", kind: schema.KindOutline,
		needs: []schema.Kind{schema.KindResearch},
	})
	runOnce(t, board, &stubAgent{
		name: "designer", kind: schema.KindDesign,
		needs: []schema.Kind{schema.KindOutline},
	})

	// 更上游被重做，中间那层没动
	runOnce(t, board, &stubAgent{name: "research", kind: schema.KindResearch})

	render := &stubAgent{
		name: "renderer", kind: schema.KindRender,
		needs: []schema.Kind{schema.KindOutline, schema.KindDesign},
	}
	out, err := toolFor(t, board, render).Execute(context.Background(), "{}")
	if err == nil {
		t.Fatalf("依据链深处已经过期，必须被检出；实际返回：%s", out)
	}
}
