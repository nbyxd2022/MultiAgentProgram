package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"multiagent/internal/checkpoint"
	"multiagent/internal/graph"
	"multiagent/internal/llm"
	"multiagent/internal/message"
	"multiagent/internal/schema"
)

// ═══════════════════════════════════════════════════════════════════
// L3-10 的落地：**四个真实角色在真图里跑通**
//
// 在此之前，`node_test.go` 里的图层测试用的都是 `fakeSub`（一个假角色），
// 而**真实的 research / outline / designer / renderer 从来没有在图里跑过** ——
// 它们的全部验证都在 `cmd/l2`（Supervisor 驱动）下完成。
//
// ⚠️ 那是一个具体的风险：**"角色本身没问题"不等于"角色被图驱动时没问题"**。
//	适配器（`SubAgentNode`）要正确地
//
//	  · 把上游产物装进 `SubAgentInput.Inputs`（角色据此 `Fetch()`）
//	  · 把 `NodeInput.Topic` / `Hint` / `Answer` 翻译过去
//	  · 把 `schema.Payload` 装回 `Outcome.Product`
//
//	—— 任何一处错了，`fakeSub` 都**测不出来**（它不看输入）。
//
// # 为什么只断言"能跑完"
//
//	因为真实模型不确定，而离线替身（下面）是确定的 —— 但**它的内容是我们编的**。
//	拿它去断言"内容对不对"等于在断言自己写的替身。
//
//	所以断言范围**正好落在"装配与契约"上**：跑得完、产物齐、血缘连得上、
//	文件真的落了盘。这是替代不了的、且不需要真模型就能验证的那一部分。
// ═══════════════════════════════════════════════════════════════════

// ─── 离线替身 ───────────────────────────────────────────────────────

// 提交口与工具名。它们**就是角色的身份**（见下面的分发）。
const (
	smokeSearch   = "web_search"
	smokeResearch = "submit_research"
	smokeOutline  = "submit_outline"
	smokeDesign   = "submit_design"
)

// smokeResearchArgs 是一份合法的研究结论（带来源）。
const smokeResearchArgs = `{"queries":["新能源汽车 渗透率"],"findings":[
	{"claim":"渗透率已达 35%，机构预计 2027 年突破 50%","source":"web_search#1"},
	{"claim":"动力电池成本两年内下降 40%","source":"web_search#2"},
	{"claim":"公共充电桩保有量同比增长 46%","source":"web_search#3"}
]}`

// smokeOutlineArgs 是 3 页大纲：**结构页 1 条 + 正文页 3 条 + 结构页 1 条**。
//
// 条数不是随便定的：`decodeOutline` 会校验（正文页 3-5 条），
// 而那是**真实角色的契约**，替身必须照着填 —— 这也是一份"替身必须知道的角色知识"。
const smokeOutlineArgs = `{"pages":[
	{"title":"行业现状","points":["渗透率已达 35%"]},
	{"title":"增长的三重压力","points":["增速放缓","产能仍在扩张","价格战加剧"]},
	{"title":"结论与建议","points":["优先布局充电网络"]}
]}`

// smokeDesignArgs 三页版式：第 1 页 cover、最后一页 closing 是**产品规则**（系统会校验）。
const smokeDesignArgs = `{"pages":[
	{"page_index":0,"layout":"cover","note":"大标题压住主题"},
	{"page_index":1,"layout":"bullets","note":"三条并列展开"},
	{"page_index":2,"layout":"closing","note":"收束成一句建议"}
]}`

// smokeMock 是一个**对请求做出反应**的角色替身（不是按轮次的剧本）。
//
// # 分发判据：请求里的**工具清单**
//
//	"角色" = SystemPrompt（人格与纪律） + 工具集（能做什么）
//
// 所以"同一组工具 = 同一个角色" —— 这与 `cmd/l2` 的离线替身是同一条判据
// （它那里的注释把这件事讲得很清楚，本文件照搬这个手法）。
//
// # 为什么**不**用 `ScriptedClient`（按调用次数返回剧本）
//
//	R23 拒绝它的理由在 L3 **更**成立：图会跳过、强制重做、跨进程恢复，
//	"第几次调用"根本对不上。而这里的判据是"**对话里有没有那条工具的返回值**" ——
//	它是**请求的纯函数**，与 `testNode` / `fakeSub` 是同一条原则。
//
// ⚠️ 它**刻意不做** `cmd/l2` 那份替身做的事（从**真实检索结果**里抽 findings、
//
//	让 demo 的内容可追溯）—— 那是 **Demo 质量**的需求，不是**冒烟**的需求。
//
//	但两者共享同一份"角色契约"的知识（哪个角色用哪个提交口、正文页要几条要点），
//	**所以它们有漂移的风险**。这一点不靠"记得同步"来防，靠下面那个默认分支：
//
//		不认识这组工具 → **立刻报错**（而不是默默走进一个错误的分支）
//
//	于是漂移会**显形为失败**，而不是静默地测着一份过时的契约。
//
//	触发条件（合一）：**L4 建评测集时** —— 那时替身会变成一等公民
//	（要能被复用跑不同场景），把两份合一才是顺带的。
func smokeMock() llm.Client {
	return llm.NewFuncClient("l3-smoke-mock",
		func(_ context.Context, _ int, req llm.Request) (*llm.Response, error) {
			switch {
			case hasTool(req, smokeResearch):
				return smokeResearchTurn(req)
			case hasTool(req, smokeOutline):
				return smokeOutlineTurn(req)
			case hasTool(req, smokeDesign):
				return smokeDesignTurn(req)
			default:
				return nil, fmt.Errorf(
					"替身不认识这组工具：%s（请给它加一个分支 —— "+
						"不认识的工具集意味着角色的契约变了）", strings.Join(toolNames(req), ", "))
			}
		})
}

// smokeResearchTurn 先**真的**走一次检索（用离线检索工具），再提交结论。
//
// ⚠️ 走这一步是有意的：冒烟测试要覆盖的是**真实角色的完整路径**（含工具循环），
// 而不只是"提交口能收下结论"。少了它，`agent.Agent` 的 ReAct 循环在 L3 下
// 就一次都没被跑过。
func smokeResearchTurn(req llm.Request) (*llm.Response, error) {
	if called(req.Messages, smokeResearch) {
		// 已经提交过 → 回纯文本收尾（不这么做的活循环会一路转到 MaxSteps）。
		return textResponse("研究完成。"), nil
	}
	if !called(req.Messages, smokeSearch) {
		return callResponse("先搜一下资料。", smokeSearch,
			`{"query":"新能源汽车 渗透率","top_k":5}`), nil
	}
	return callResponse("整理成带来源的结论。", smokeResearch, smokeResearchArgs), nil
}

func smokeOutlineTurn(req llm.Request) (*llm.Response, error) {
	if called(req.Messages, smokeOutline) {
		return textResponse("大纲完成。"), nil
	}
	return callResponse("组织成 3 页。", smokeOutline, smokeOutlineArgs), nil
}

func smokeDesignTurn(req llm.Request) (*llm.Response, error) {
	if called(req.Messages, smokeDesign) {
		return textResponse("版面设计完成。"), nil
	}
	return callResponse("为每一页挑一个版式。", smokeDesign, smokeDesignArgs), nil
}

// ─── 替身的观察工具（都是**请求的纯函数**）───────────────────────────

func hasTool(req llm.Request, name string) bool {
	for _, d := range req.Tools {
		if d.Name == name {
			return true
		}
	}
	return false
}

// called 报告"这个工具的**结果**已经出现在对话里"。
//
// ⚠️ 判据是 `role == tool && name == …`，也就是"**工具返回过**"。
//
//	它比"请求过"强：请求了但工具报错时，没东西可整理 ——
//	拿它继续写结论只会编出内容来（`cmd/l2` 的替身把这条写得很明白）。
//
//	（更精确的判据要区分"成功 / 失败"，那需要解析工具返回的 JSON。
//	 这里用"返回过"就够了 —— 冒烟测试只需要"进度判据是请求的函数"。）
func called(msgs []message.Message, name string) bool {
	for _, m := range msgs {
		if m.Role == message.RoleTool && m.Name == name {
			return true
		}
	}
	return false
}

func toolNames(req llm.Request) []string {
	out := make([]string, 0, len(req.Tools))
	for _, d := range req.Tools {
		out = append(out, d.Name)
	}
	return out
}

// callResponse / textResponse 让替身的意图在调用点上就看得见。
func callResponse(note, tool, args string) *llm.Response {
	return &llm.Response{
		Content:   note,
		ToolCalls: []message.ToolCall{{ID: "call_" + tool, Name: tool, Arguments: args}},
		// 用量按内容长度粗估 —— 只为把**成本链路**接通（与 llm.Mock 同一个理由）。
		Usage: llm.Usage{PromptTokens: 400, CompletionTokens: 120},
	}
}

func textResponse(content string) *llm.Response {
	return &llm.Response{Content: content, Usage: llm.Usage{PromptTokens: 60, CompletionTokens: 10}}
}

// ─── 冒烟测试 ───────────────────────────────────────────────────────

// TestRealRolesRunUnderTheGraph 是 L3-10 的看门人。
//
// 它装配的是**与 cmd/l3 同形**的一张图，但四个节点全是**真实角色**：
//
//	topic ──▶ research ──▶ outline ──▶ design ──┐
//	                                    ▲        ├──▶ render
//	                                    └────────┘
//
// （边依然**一条都没手写** —— 它们来自四个真实角色的 `Needs()`。）
func TestRealRolesRunUnderTheGraph(t *testing.T) {
	outDir := t.TempDir()
	ckptDir := filepath.Join(t.TempDir(), "ckpt")

	res, nodes := runSmokeGraph(t, outDir, ckptDir)

	// ① 跑完了 —— 这是"装配与契约都对"的总判据
	if res.Status != graph.StatusCompleted {
		t.Fatalf("图没有跑完：%s（跑了 %v）", res.Status, res.Ran)
	}

	// ② 四类产物都进了黑板（run 路径里每一跳都必须真的产出）
	for _, k := range []string{
		graph.KeyTopic, string(schema.KindResearch), string(schema.KindOutline),
		string(schema.KindDesign), string(schema.KindRender),
	} {
		if res.State.Versions(k) == 0 {
			t.Errorf("黑板上 %q 有 0 版 —— 真实角色没有把产物交出来", k)
		}
	}

	// ③ 每一份产物都满足**完成判据**（存在 ∧ 完整 ∧ 新鲜）。
	//
	//	这条把"图上跑完"与"L2 定下的三条判据"接了起来 ——
	//	如果真实角色产出的东西缺了 `BasedOn`（血缘），这里会红。
	for _, k := range res.State.Keys() {
		if k == graph.KeyTopic {
			continue // 输入产物没有上游，不必谈"新鲜"
		}
		if v := res.State.CanSkip(k); !v.Skip {
			t.Errorf("%s 跑完了却不满足完成判据：%s", k, v.Reason)
		}
	}

	// ④ 血缘**连得上**：outline 依据 research，design 依据 outline，render 依据 outline+design。
	//
	//	这是适配器最容易错的地方 —— 它要正确地把 `Needs()` 变成黑板上的引用。
	assertBasedOn(t, res.State, string(schema.KindOutline), string(schema.KindResearch))
	assertBasedOn(t, res.State, string(schema.KindDesign), string(schema.KindOutline))
	assertBasedOn(t, res.State, string(schema.KindRender), string(schema.KindOutline))
	assertBasedOn(t, res.State, string(schema.KindRender), string(schema.KindDesign))

	// ⑤ renderer 是**唯一有真副作用**的角色 —— 文件必须真的在磁盘上。
	rr, err := schema.Decode[schema.RenderResult](lastOf(t, res.State, string(schema.KindRender)))
	if err != nil {
		t.Fatalf("解不出 render 产物: %v", err)
	}
	if len(rr.Files) == 0 {
		t.Fatal("render 报告 0 个文件")
	}
	for _, f := range rr.Files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("render 报告写了 %s，但磁盘上没有: %v", f, err)
		}
	}

	// ⑥ 真实角色会调模型 —— 它们的用量必须**报得出来**（L3-3 的连带验证）。
	//
	//	`fakeSub` 版本测不到这一条：它根本不实现 `StatsReporter`。
	//
	// ⚠️ 统计的必须是**真正跑过的那批实例** —— 新建一批得到 0，
	//	那样这条断言会永远"通过"（它测的是一个刚 new 出来、一次都没跑过的角色）。
	//	所以 `runSmokeGraph` 把节点一起交出来，而不是让这里再建一批。
	var total int
	for _, n := range nodes {
		if r, ok := n.(StatsReporter); ok {
			total += r.Stats().PromptTokens
		}
	}
	if total == 0 {
		t.Error("四个真实角色跑完了，却一点 prompt 用量都报不出来 —— " +
			"适配器又把可选能力吞了（L3-3）")
	}
}

// ─── 夹具 ───────────────────────────────────────────────────────────

// runSmokeGraph 装图、跑完（含一次 HITL 确认），返回结果**与跑过的那批节点**。
//
// ⚠️ 节点要一起返回：用量是**角色实例**上的累计值，而"新建一批再看"
//
//	得到的是 0 —— 那样的断言会永远通过（它测的是一个没跑过的对象）。
func runSmokeGraph(t *testing.T, outDir, ckptDir string) (graph.Result, []graph.Node) {
	t.Helper()

	mock := smokeMock()

	research, err := NewResearchAgent(ResearchOptions{Client: mock})
	if err != nil {
		t.Fatalf("构造研究员失败: %v", err)
	}
	outline, err := NewOutlineAgent(OutlineOptions{Client: mock})
	if err != nil {
		t.Fatalf("构造大纲编写者失败: %v", err)
	}
	designer, err := NewDesigner(DesignerOptions{Client: mock})
	if err != nil {
		t.Fatalf("构造版面设计者失败: %v", err)
	}

	subs := []SubAgent{research, outline, designer, NewRenderer(RendererOptions{OutDir: outDir})}
	nodes := make([]graph.Node, 0, len(subs))
	for _, s := range subs {
		opt := SubAgentNodeOptions{Sub: s}
		if s.Name() == "outline" {
			// 顺带覆盖 HITL 路径：产出之后停下等人确认。
			opt.Confirm = "请确认这份大纲。"
		}
		n, err := NewSubAgentNode(opt)
		if err != nil {
			t.Fatalf("包装 %s 失败: %v", s.Name(), err)
		}
		nodes = append(nodes, n)
	}

	g, err := graph.BuildGraph(graph.GraphSpec{
		Nodes:   nodes,
		Presets: []schema.Kind{schema.KindTopic},
	})
	if err != nil {
		t.Fatalf("建图失败: %v", err)
	}

	store, err := checkpoint.NewFileStore(ckptDir)
	if err != nil {
		t.Fatalf("装配存储失败: %v", err)
	}
	r, err := graph.NewRunner(graph.RunnerOptions{
		Graph: g, Store: store, RunID: "smoke", Topic: "新能源汽车行业趋势",
	})
	if err != nil {
		t.Fatalf("装配 Runner 失败: %v", err)
	}

	ctx := context.Background()
	res, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("图执行失败: %v", err)
	}
	if res.Status == graph.StatusInterrupted {
		res, err = r.Resume(ctx, graph.Decision{Intent: graph.IntentConfirm})
		if err != nil {
			t.Fatalf("恢复失败: %v", err)
		}
	}
	return res, nodes
}

func lastOf(t *testing.T, st *graph.State, key string) schema.Artifact {
	t.Helper()
	a, ok := st.Latest(key)
	if !ok {
		t.Fatalf("黑板上有 %q", key)
	}
	return a
}

func assertBasedOn(t *testing.T, st *graph.State, key, want string) {
	t.Helper()
	a := lastOf(t, st, key)
	for _, b := range a.BasedOn {
		if b.Key == want {
			return
		}
	}
	raw, _ := json.Marshal(a.BasedOn)
	t.Errorf("%s 的血缘里应当有 %s，实际 %s", key, want, raw)
}
