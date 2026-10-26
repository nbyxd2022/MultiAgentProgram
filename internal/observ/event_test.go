package observ

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// ─── 词表自洽 ───────────────────────────────────────────────────────

// 词表的每一格都必须能被 `Valid` 认，且都在目录表里。
//
// ⚠️ 它守的是"声明与实际一致"：如果加了常量却忘了加目录项，
//
//	`Valid()` 会说它非法，而 `AllKinds()` **不会列出它** —— 于是
//	错误信息会让人去一个不存在的清单里找。**两处必须同源。**
func TestKindVocabularyIsSelfConsistent(t *testing.T) {
	all := AllKinds()
	if len(all) != len(kindSpecs) {
		t.Fatalf("AllKinds 有 %d 项，目录表有 %d 项 —— 它们不是同一份数据",
			len(all), len(kindSpecs))
	}
	for _, k := range all {
		if !k.Valid() {
			t.Errorf("%q 在目录表里，但 Valid() 说它非法", k)
		}
		if kindSpecs[k].Desc == "" {
			t.Errorf("%q 没有描述 —— 错误信息里会缺一句解释", k)
		}
	}
}

// ⚠️ **必须排序**：map 遍历顺序随机，不排序会让"同一份观测打印两次却不一样"。
//
//	（第一版 `AllKinds` 写漏了 `sort` —— 写这条测试时才发现的。）
func TestAllKindsIsSorted(t *testing.T) {
	kinds := AllKinds()
	for i := 1; i < len(kinds); i++ {
		if kinds[i-1] > kinds[i] {
			t.Fatalf("AllKinds 没有排序：%v", kinds)
		}
	}
	layers := AllLayers()
	for i := 1; i < len(layers); i++ {
		if layers[i-1] > layers[i] {
			t.Fatalf("AllLayers 没有排序：%v", layers)
		}
	}
	// 反复调用必须得到**同一个**结果（这才是"稳定"的实际含义）
	for i := 0; i < 5; i++ {
		if joinKinds(kinds) != joinKinds(AllKinds()) {
			t.Fatal("AllKinds 两次调用结果不同 —— 顺序不稳定")
		}
	}
}

func TestValidRejectsGarbage(t *testing.T) {
	if Kind("").Valid() || Kind("不存在的类型").Valid() {
		t.Error("Kind.Valid 应当拒绝词表外的值")
	}
	if Layer("").Valid() || Layer("不存在的层").Valid() {
		t.Error("Layer.Valid 应当拒绝词表外的值")
	}
}

// ─── 自校验 ─────────────────────────────────────────────────────────

// ⚠️ 这一类**必须有消费者**，否则 `kindSpecs` 的四个字段就是"声明了没人读"
//
//	—— 而那正是 L3 复盘抓到的那类半成品。
func TestValidateRequiresWhatTheSpecSays(t *testing.T) {
	cases := []struct {
		name    string
		e       Event
		wantSub string // "" = 应当通过
	}{
		{
			"完整的一次模型调用",
			Event{Layer: LayerLLM, Phase: "research", Kind: KindLLMCall, PromptTokens: 10},
			"",
		},
		{
			"完整的一次对账",
			Event{Layer: LayerGraph, Phase: "render", Kind: KindReconcile,
				Reconcile: &Reconcile{What: "图片数量", Declared: 3, Actual: 2}},
			"",
		},
		{
			"完整的一次完成事件（它不需要阶段名）",
			Event{Layer: LayerGraph, Kind: KindDone},
			"",
		},
		{
			"类型不在词表里",
			Event{Layer: LayerGraph, Kind: "凭空造的类型"},
			"未知的事件类型",
		},
		{
			"层不在词表里",
			Event{Layer: "凭空造的层", Kind: KindDone},
			"未知的层",
		},
		{
			"模型调用没有阶段名 —— 说了「发生了」却不说「哪一步」",
			Event{Layer: LayerLLM, Kind: KindLLMCall, PromptTokens: 10},
			"必须带阶段名",
		},
		{
			"对账没有带对账那两句话 —— 只有一个「不符」的标记是查不下去的",
			Event{Layer: LayerGraph, Phase: "render", Kind: KindReconcile},
			"必须带一份对账",
		},
		{
			"模型调用没有用量 —— 聚合时它是空的",
			Event{Layer: LayerLLM, Phase: "research", Kind: KindLLMCall},
			"必须带用量",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.e.Validate()
			if c.wantSub == "" {
				if err != nil {
					t.Errorf("应当通过，却报：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应当被拒绝，却通过了")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误里应包含 %q，实际：%v", c.wantSub, err)
			}
		})
	}
}

// 错误信息必须**列出合法取值**（人能据此自愈，而不是去翻代码）。
func TestValidateErrorListsLegalValues(t *testing.T) {
	err := Event{Layer: LayerGraph, Kind: "瞎写的"}.Validate()
	if err == nil {
		t.Fatal("应当被拒绝")
	}
	for _, k := range AllKinds() {
		if !strings.Contains(err.Error(), string(k)) {
			t.Errorf("错误里应列出合法类型 %q，实际：%v", k, err)
		}
	}
}

// ─── 脱敏 ───────────────────────────────────────────────────────────

func TestRedactClearsBodyKeepsMeta(t *testing.T) {
	in := Event{
		RunID: "run-1", Span: "render_p1", Layer: LayerLLM, Phase: "render",
		Kind: KindLLMCall, Err: "boom",
		SystemPrompt: "系统提示", UserPrompt: "用户提示", Response: "模型回答",
		PromptTokens: 3700, CompletionTokens: 820,
		Duration:  1234,
		Reconcile: &Reconcile{What: "要点条数", Declared: 3, Actual: 2},
	}

	out := in.Redact()

	// ① 正文必须没了
	if out.SystemPrompt != "" || out.UserPrompt != "" || out.Response != "" {
		t.Errorf("脱敏没有清空正文：%+v", out)
	}
	// ② **元信息一个都不能少** —— 脱敏不是"清空"，是"只留元信息"
	if out.RunID != in.RunID || out.Span != in.Span || out.Layer != in.Layer ||
		out.Phase != in.Phase || out.Kind != in.Kind || out.Err != in.Err {
		t.Errorf("脱敏丢了元信息：\n原 %+v\n后 %+v", in, out)
	}
	if out.PromptTokens != 3700 || out.CompletionTokens != 820 || out.Duration != 1234 {
		t.Errorf("脱敏丢了数值：%+v", out)
	}
	if out.Reconcile == nil || out.Reconcile.Actual != 2 {
		t.Errorf("脱敏丢了对账：%+v", out.Reconcile)
	}
	// ③ 它是**值语义**：脱敏后原事件不能被改（否则第二个消费者拿不到完整版）
	if in.SystemPrompt != "系统提示" {
		t.Error("Redact 改了原事件 —— 它会毁掉本地录播（那个出口要完整的）")
	}
}

// ⚠️⚠️ 加新字段时的看门人 —— 而**方向**很关键。
//
// # 它守的是哪一侧
//
//	`Redact` 是**白名单**（返回一个新结构，只复制要保留的字段）。所以：
//
//		正文**不可能**泄露（不在白名单里 = 被零值覆盖）✅
//		**元信息会静默丢失**（不在白名单里 = 也没了）⚠️
//
//	所以**唯一需要守的是元信息那一侧**。
//
// # 这条测试是被一次**失败的注入实验**逼出来的
//
//	第一版我写的是反方向（"正文类字段必须被清空"），然后注入一个
//	`ToolArgs` 字段去验证它会拦 —— **它没拦**。
//
//	因为白名单里根本没有 `ToolArgs`，所以 `rr.ToolArgs` **必然**是空串，
//	那个断言**永远不成立** —— **它是一条看起来在守、实际恒真的测试。**
//
//	（R37 那条"验证必须能自证"的第三次应用 —— 而这次它抓到的是
//	 **我自己刚写的守卫是假的**。）
//
// # 判据：用命名约定画出"正文类"的边界
//
//	字段名里带这些词的 → 算正文（**应当**被清空）；
//	其余的 → 算元信息（**必须**逐字保留）。
//
//	它不替你判断"这个新字段敏不敏感" —— 它保证**你必须面对这个问题**：
//	要么名字落在正文类里（于是被清空），要么被 `Redact` 保留。
//	**没有第三种可能**（新增字段默认被丢掉 —— 那正是会红的那种情况）。
func TestRedactPreservesEveryMetaField(t *testing.T) {
	full := Event{
		RunID: "r", Span: "s", Layer: LayerLLM, Phase: "p",
		Kind: KindLLMCall, Err: "e", Model: "doubao-seed",
		SystemPrompt: "sp", UserPrompt: "up", Response: "resp",
		ToolArgs:     "ta", // 正文类（"Args"）—— 应当被清空
		ToolName:     "web_search",
		PromptTokens: 1, CompletionTokens: 2, Duration: 3,
		Reconcile: &Reconcile{What: "w", Declared: 1, Actual: 2},
	}
	red := full.Redact()

	// ⚠️ `Detail` 曾经**不在**这个名单里 —— 而它装的是**工具回执原文**（R59）。
	//
	//	那条白名单是按**字段名**判"是不是正文"的，而 `Detail` 这个名字
	//	**听起来像元信息** —— 于是它被放过了一次。
	//
	//	> ### 一个按名字判定的白名单，会被**名字**骗过去。
	//
	//	⭐ 而这一条无法自动发现：名字与内容不符，只有**读实现**才知道。
	bodyLike := []string{"Prompt", "Response", "Args", "Payload", "Body", "Content", "Detail"}
	zeroOf := func(f reflect.StructField) any { return reflect.Zero(f.Type).Interface() }
	rt, rv, rr := reflect.TypeOf(full), reflect.ValueOf(full), reflect.ValueOf(red)

	checkedMeta := 0
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		want, got := rv.Field(i).Interface(), rr.Field(i).Interface()

		// ① 正文那一侧：**必须**被清空。
		//    （白名单让这一步几乎不可能失败，但把它写出来，"意图"才是可读的。）
		//
		// ⚠️ 命名约定**只对字符串字段套用** —— 因为"正文"的定义就是
		//	"大段的、敏感的**文本**"。
		//
		//	（第一版没加这个类型约束，`PromptTokens` 撞上了 `Prompt`
		//	 被判成正文 —— 而它是**数值**，是元信息，`Redact` **必须保留**它。
		//	 纯命名约定会把它误判，而误判的方向是"要求清掉一个该保留的字段"。）
		if f.Type.Kind() == reflect.String && hasAnyBodyWord(f.Name, bodyLike) {
			if !reflect.DeepEqual(got, zeroOf(f)) {
				t.Errorf("字段 %s 是正文，`Redact` 却没清空它 —— 它会被发到上游", f.Name)
			}
			continue
		}

		// ② 元信息那一侧：**一个都不能少** —— 这是真正会出事的方向。
		if reflect.DeepEqual(want, zeroOf(f)) {
			continue // 这个字段这次没填，测不出什么
		}
		checkedMeta++
		if !reflect.DeepEqual(got, want) {
			t.Errorf("字段 %s 是**元信息**，但 `Redact` 把它丢了（%v → %v）：\n"+
				"  上游收到的观测会少一个字段，而且**静默**（不报错、不告警）。\n"+
				"  请把它加进 `Redact` 的白名单；或者如果它其实是正文，就把名字改成正文类的",
				f.Name, want, got)
		}
	}

	// ⚠️ **防空跑**（R37 的教训）：一个元信息字段都没检查到的话，
	//	这条测试只是安静地绿着，什么都没验证。
	if checkedMeta == 0 {
		t.Fatal("一个元信息字段都没被检查到 —— 字段列表或命名约定变了，这条测试在空跑")
	}
}

func hasAnyBodyWord(name string, pats []string) bool {
	for _, p := range pats {
		if strings.Contains(name, p) {
			return true
		}
	}
	return false
}

// ─── 行为指纹：投影的**射程**要正好 ─────────────────────────────────

// ⚠️ 这条测试守的是"投影"这个概念本身 —— 而它**两个方向都要守**。
//
// # 为什么两个方向都要
//
//	算**多**了（把环境量算进行为）→ 两次运行永远不同 → **判据永远红** →
//	  而永远红的判据会被关掉，然后没人再看它。
//	算**少**了（丢掉真的行为差异）→ 真的不同的过程被判为相同 → **假绿**。
//
//	> **假绿比假红糟得多：假红会被修，假绿会被信任。**
func TestBehaviorProjectsOutTheEnvironment(t *testing.T) {
	base := Event{
		RunID: "run-1", Layer: LayerGraph, Phase: "outline", Kind: KindProduct,
		Detail: "outline@1  3 页", Duration: Milliseconds(1200),
		PromptTokens: 500, CompletionTokens: 100,
	}

	// ① **环境量变了，行为指纹必须不变** —— 否则判据永远红
	same := base
	same.RunID = "run-2"               // 换了一轮
	same.Duration = Milliseconds(9000) // 机器这次慢
	same.PromptTokens = 502            // 模型这次多吐了两个 token
	if base.Behavior() != same.Behavior() {
		t.Errorf("环境量不该影响行为指纹：\n  %s\n  %s", base.Behavior(), same.Behavior())
	}

	// ② **行为量变了，指纹必须变** —— 否则是假绿。
	//
	//	⚠️ **逐个字段验**，而不是只验一个 —— "漏了哪个字段"正是这条测试要抓的。
	changes := map[string]func(*Event){
		"Layer":  func(e *Event) { e.Layer = LayerAgent },
		"Kind":   func(e *Event) { e.Kind = KindStart },
		"Phase":  func(e *Event) { e.Phase = "design" },
		"Err":    func(e *Event) { e.Err = "超时" },
		"Detail": func(e *Event) { e.Detail = "换了一句话" },
	}
	for name, mut := range changes {
		got := base
		mut(&got)
		if base.Behavior() == got.Behavior() {
			t.Errorf("改 %s 之后行为指纹**没有变** —— "+
				"那会让两个真的不同的过程被判为相同（**假绿**）", name)
		}
	}
}

// ⚠️ `Behavior()` 必须**稳定**（同一个事件算两次得到同一个串）——
//
//	否则它做不了"可 diff"的依据。
//
//	（当前实现是 `fmt.Sprintf`，本身是确定的；这条测试守的是
//	 **将来有人给它加一个 map 或一个时间戳**。）
func TestBehaviorIsStable(t *testing.T) {
	e := Event{RunID: "r", Layer: LayerGraph, Phase: "p", Kind: KindNotice, Detail: "d"}
	for i := 0; i < 10; i++ {
		if e.Behavior() != e.Behavior() {
			t.Fatal("Behavior() 不稳定 —— 那它做不了 diff 的依据")
		}
	}
}

// ─── 对账 ───────────────────────────────────────────────────────────

func TestReconcileDiff(t *testing.T) {
	cases := []struct {
		name string
		r    Reconcile
		want string // "" = 相符
	}{
		{"相符", Reconcile{What: "页数", Declared: 3, Actual: 3}, ""},
		{"少了", Reconcile{What: "要点条数", Declared: 3, Actual: 1}, "少 2"},
		{"多了", Reconcile{What: "要点条数", Declared: 3, Actual: 5}, "多 2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.r.Diff()
			if c.want == "" {
				if got != "" {
					t.Errorf("相符时应当返回空串，实际 %q", got)
				}
				if !c.r.OK() {
					t.Error("相符时 OK() 应当为 true")
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("Diff 里应含 %q（**方向 + 幅度**），实际 %q", c.want, got)
			}
			// 两个数都要在 —— 只说"不一致"是查不下去的
			if !strings.Contains(got, "3") {
				t.Errorf("Diff 里应带上声明的数，实际 %q", got)
			}
			if c.r.OK() {
				t.Error("不符时 OK() 应当为 false")
			}
		})
	}
}

// ─── Duration ───────────────────────────────────────────────────────

// ⚠️ 它是**毫秒**，不是 `time.Duration` 的纳秒 ——
//
//	因为观测量是给人看的东西，表示方式要为"看"服务。
func TestDurationIsMilliseconds(t *testing.T) {
	d := Milliseconds(400 * time.Millisecond)
	if d != 400 {
		t.Errorf("400ms 应当是 400（毫秒），实际 %d", d)
	}
	if got := d.String(); got != "400ms" {
		t.Errorf("String() = %q，期望 400ms", got)
	}
	if got := Milliseconds(1500 * time.Millisecond).String(); got != "1.5s" {
		t.Errorf("1.5s 的 String() = %q", got)
	}
}
