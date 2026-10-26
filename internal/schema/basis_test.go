package schema

import (
	"strings"
	"testing"
	"time"
)

// 这个文件单测 **L2-1 的判定规则本身**：`StaleReasons`。
//
// 注意它**不依赖黑板** —— 因为"什么叫过期"是**协议语义**，
// 而"当前最新版是谁"才是存储的事。把 lookup 做成参数（`Latest`），
// 规则就能被单独验证（原则五：端口-适配器）。
//
// 这条好处在测试里立刻体现：构造 "outline@1 与 outline@2 同时存在"
// 只需要两行，不需要起一个黑板、也不需要考虑写入顺序。

// fakeLatest 用一个 map 冒充"当前最新版"。
func fakeLatest(arts ...Artifact) Latest {
	m := make(map[string]Artifact, len(arts))
	for _, a := range arts {
		if cur, ok := m[a.Key]; !ok || a.Version > cur.Version {
			m[a.Key] = a
		}
	}
	return func(key string) (Artifact, bool) {
		a, ok := m[key]
		return a, ok
	}
}

// art 造一份最简产物：只关心 key / version / 血缘 / 产出者。
func art(key string, version int, basis ...Basis) Artifact {
	return Artifact{
		Key: key, Kind: KindOutline, Version: version,
		ProducedBy: "agent-" + key, BasedOn: basis,
	}
}

func TestBasisOfAndRef(t *testing.T) {
	a := art("outline", 3)

	b := BasisOf(a)
	if b.Key != "outline" || b.Version != 3 {
		t.Fatalf("BasisOf 结果不对：%+v", b)
	}
	// 给人看的形态必须和 RefOf 一致（同一个东西不能有两种写法）
	if b.Ref() != RefOf(a) {
		t.Errorf("Basis.Ref()=%q 与 RefOf=%q 不一致", b.Ref(), RefOf(a))
	}
}

func TestStaleReasonsFreshIsEmpty(t *testing.T) {
	design := art("design", 1, Basis{Key: "outline", Version: 1})
	latest := fakeLatest(art("outline", 1))

	if got := StaleReasons(design, latest); len(got) != 0 {
		t.Errorf("依据仍是最新版时不该报过期，实际 %v", got)
	}
}

func TestStaleReasonsDetectsDirectStaleness(t *testing.T) {
	design := art("design", 1, Basis{Key: "outline", Version: 1})
	latest := fakeLatest(art("outline", 2)) // 上游出了新版

	got := StaleReasons(design, latest)
	if len(got) != 1 {
		t.Fatalf("应当报出 1 条过期，实际 %v", got)
	}
	for _, want := range []string{"design@1", "outline@1", "outline@2"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("描述里应当出现 %q：%s", want, got[0])
		}
	}
}

// TestStaleReasonsIsTransitive 是本文件最重要的一条。
//
// 它构造的是"**直接依据仍是最新、但它的依据过期了**"——
// 只查一层是发现不了的。
func TestStaleReasonsIsTransitive(t *testing.T) {
	// design@1 → outline@1 → research@1，而 research 已经出了 @2
	// （outline 停在 @1，所以 design 的**直接**依据看起来还是最新的）
	design := art("design", 1, Basis{Key: "outline", Version: 1})
	outline := art("outline", 1, Basis{Key: "research", Version: 1})
	latest := fakeLatest(outline, art("research", 2))

	got := StaleReasons(design, latest)
	if len(got) != 1 {
		t.Fatalf("依据链深处的过期也必须被发现，实际 %v", got)
	}
	if !strings.Contains(got[0], "outline@1") {
		t.Errorf("应当指出过期的环节是 outline@1：%s", got[0])
	}
}

// TestStaleReasonsReportsRootCauseFirst 守住报错的**顺序**。
//
// 上游没修好之前，重做下游只是白做一遍（它还会被同一条规则拦下）。
// 所以列表必须以"最上游的那个问题"开头。
func TestStaleReasonsReportsRootCauseFirst(t *testing.T) {
	a := art("a", 1, Basis{Key: "b", Version: 1})
	b2 := art("b", 2, Basis{Key: "c", Version: 1}) // b 自己也是基于旧的 c 做出来的
	latest := fakeLatest(b2, art("c", 2))          // b 与 c 都动了，而 a 停在旧 b 上

	got := StaleReasons(a, latest)
	if len(got) != 2 {
		t.Fatalf("应当报出 2 条（两条链都断了），实际 %v", got)
	}
	if !strings.Contains(got[0], "b@2") {
		t.Errorf("第一条应当是更上游的那个问题（b@2 依据的 c@1 过期），实际 %q", got[0])
	}
	if !strings.Contains(got[1], "a@1") {
		t.Errorf("第二条才是 a 自己的问题，实际 %q", got[1])
	}
}

// TestStaleReasonsOrdersByFixOrder 是本文件里**最贴近真实场景**的一条。
//
// 它守住的不是"能不能发现问题"，而是"**问题以什么顺序列出来**"。
// 这份列表是给模型当待办用的 —— 顺序错了，模型会先做一件注定被拒的事。
//
//	render@1 ← outline@1
//	         ← design@1 ← outline@1
//	而 outline 已经出了 @2
//
// 两条链都断了，但**必须先重做 design、再重做 render**
// （design 没修好之前，render 拿到的仍是一份过期输入，必然被拒）。
func TestStaleReasonsOrdersByFixOrder(t *testing.T) {
	design := art("design", 1, Basis{Key: "outline", Version: 1})
	render := art("render", 1,
		Basis{Key: "outline", Version: 1},
		Basis{Key: "design", Version: 1},
	)
	latest := fakeLatest(art("outline", 2), design)

	got := StaleReasons(render, latest)
	if len(got) != 2 {
		t.Fatalf("两条链都断了，应当报 2 条，实际 %v", got)
	}
	if !strings.HasPrefix(got[0], "design：") {
		t.Errorf("第一条应当是 design（上游优先），实际 %q", got[0])
	}
	if !strings.HasPrefix(got[1], "render：") {
		t.Errorf("第二条才是 render，实际 %q", got[1])
	}
	// 行首的 key 是**给机器读的**（派活用的工具名）——
	// 编排层要能据此自动照做，而不是再去理解自然语言。
	if !strings.Contains(got[0], "agent-design") {
		t.Errorf("行内应当带上产出者（给人读）：%s", got[0])
	}
}

// TestStaleReasonsIgnoresMissingUpstream 守住"同一问题不报两遍"。
//
// 上游根本不在了，那是**存在性**问题，由 missingNeeds 负责。
// 这里再报一次只会让反馈变啰嗦。
func TestStaleReasonsIgnoresMissingUpstream(t *testing.T) {
	a := art("a", 1, Basis{Key: "ghost", Version: 1})

	if got := StaleReasons(a, fakeLatest()); len(got) != 0 {
		t.Errorf("上游不存在不是「过期」，不该在这里报：%v", got)
	}
}

func TestStaleReasonsIsDeterministic(t *testing.T) {
	a := art("a", 1, Basis{Key: "b", Version: 1}, Basis{Key: "c", Version: 1})
	latest := fakeLatest(art("b", 2), art("c", 2))

	first := StaleReasons(a, latest)
	second := StaleReasons(a, latest)
	if strings.Join(first, "|") != strings.Join(second, "|") {
		t.Errorf("同一输入两次调用必须一致：\n%v\n%v", first, second)
	}
}

// TestStaleReasonsTerminatesOnCycle 是一个健壮性保底。
//
// 正常情况不会有环（BasedOn 总是指向"更早产出的版本"），
// 但这是别人手工构造数据时最容易犯的错 —— 它绝不该让程序挂住。
func TestStaleReasonsTerminatesOnCycle(t *testing.T) {
	a := art("a", 1, Basis{Key: "b", Version: 1})
	b := art("b", 1, Basis{Key: "a", Version: 1})

	done := make(chan []string, 1)
	go func() { done <- StaleReasons(a, fakeLatest(a, b)) }()

	select {
	case got := <-done:
		if len(got) != 0 {
			t.Errorf("互为依据且版本都一致时不该报过期：%v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("遍历没有终止 —— 环没有被 visited 集合挡住")
	}
}

// TestValidateRejectsBadBasis 守住"信封上的新字段也要进校验"。
//
// 一条 Version<1 的依据永远不会与任何真实版本相等，
// 于是它会让**每一次新鲜度检查都报过期** —— 一个脏字段把整条链永久卡死。
func TestValidateRejectsBadBasis(t *testing.T) {
	base := func(basis []Basis) Artifact {
		return Artifact{
			Key: "k", Kind: KindResearch, Version: 1,
			Payload: []byte("{}"), BasedOn: basis,
		}
	}

	t.Run("版本小于 1", func(t *testing.T) {
		if err := base([]Basis{{Key: "x", Version: 0}}).Validate(); err == nil {
			t.Error("Version<1 的依据必须被拒绝（否则会造成永久性误报）")
		}
	})
	t.Run("key 为空", func(t *testing.T) {
		if err := base([]Basis{{Key: "  ", Version: 1}}).Validate(); err == nil {
			t.Error("空 key 的依据必须被拒绝")
		}
	})
	t.Run("合法依据通过", func(t *testing.T) {
		if err := base([]Basis{{Key: "x", Version: 2}}).Validate(); err != nil {
			t.Errorf("合法依据不该被拒：%v", err)
		}
	})
	t.Run("没有依据也通过", func(t *testing.T) {
		if err := base(nil).Validate(); err != nil {
			t.Errorf("没有上游的产物（如 research）不该被拒：%v", err)
		}
	})
}
