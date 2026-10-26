package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"multiagent/internal/tool"
)

// WebSearch 是一个"无副作用"的搜索工具。
//
// 当前返回 Mock 数据——这是刻意的：
//   - 让我们先聚焦"工具如何被模型正确调用"，而不是对接具体搜索服务；
//   - 后续替换为真实实现时，只改 Execute 内部，不影响 Agent 与 Prompt。
type WebSearch struct{}

// NewWebSearch 创建一个搜索工具。
func NewWebSearch() *WebSearch { return &WebSearch{} }

// Definition 实现 tool.Tool 接口。
func (t *WebSearch) Definition() tool.Definition {
	return tool.Definition{
		Name:        "web_search",
		Description: "联网搜索资料。输入查询关键词，返回若干条结构化摘要，用于补充事实与数据。",
		Parameters: []tool.Parameter{
			{
				Name:        "query",
				Type:        tool.TypeString,
				Description: "查询关键词，尽量具体",
				Required:    true,
			},
			{
				Name:        "top_k",
				Type:        tool.TypeInteger,
				Description: "返回条数，默认 3，最大 5",
				Required:    false,
			},
		},
	}
}

type webSearchArgs struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

// Execute 实现 tool.Tool 接口。
func (t *WebSearch) Execute(_ context.Context, args string) (string, error) {
	var in webSearchArgs
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" {
		return "", fmt.Errorf("query 不能为空")
	}
	if in.TopK <= 0 {
		in.TopK = 3
	}
	if in.TopK > 5 {
		in.TopK = 5
	}

	snippets := mockSnippets(in.Query, in.TopK)

	results := make([]map[string]any, 0, in.TopK)
	for i := 0; i < in.TopK; i++ {
		results = append(results, map[string]any{
			"title":   fmt.Sprintf("%s · 资料 %d", in.Query, i+1),
			"source":  "mock-search",
			"snippet": snippets[i],
		})
	}

	out, _ := json.Marshal(map[string]any{
		"ok":      true,
		"query":   in.Query,
		"count":   len(results),
		"results": results,
	})
	return string(out), nil
}

// mockCorpus 是给 Demo 用的"看起来像真的"语料。
//
// ⚠️ 这**不是能力提升** —— 检索依然是假的（P4：Mock 检索让幻觉无法被拦截）。
// 它存在的理由只有一个：**让链路可读**。
//
// 原来每条都返回"第 N 条 Mock 摘要：包含一个可引用的事实与一个可量化的指标"，
// 于是 L2 那条 `research → outline → 页面` 的链路从头到尾都是噪音 ——
// **"内容到底有没有被搬运对"根本看不出来**，而这恰恰是 L2 最该被看见的东西。
//
// 真实项目里这一段会被真实搜索服务替换（只改 Execute 内部，接口不变）。
var mockCorpus = []struct {
	Match    string
	Snippets []string
}{
	{
		Match: "新能源",
		Snippets: []string{
			"新能源汽车渗透率已达 35%，机构预计 2027 年突破 50%。",
			"动力电池成本两年内下降 40%，进入 0.5 元/Wh 区间。",
			"公共充电桩保有量同比增长 46%，但区域分布严重不均。",
			"价格战使行业平均毛利率从 18% 压缩到 11%。",
			"出口成为最大增量，海外销量同比翻倍。",
		},
	},
	{
		Match: "AI",
		Snippets: []string{
			"企业级 AI 应用渗透率约 28%，集中在客服与代码生成两类场景。",
			"推理成本一年内下降约 80%，中小团队开始能自建。",
			"超过六成项目卡在数据治理，而不是模型能力。",
			"合规与审计要求正在成为选型的第一约束。",
			"自建与采购的比例从 2:8 转向 5:5。",
		},
	},
	{
		Match: "效率",
		Snippets: []string{
			"团队协作成本中，约 40% 消耗在信息同步而非实际执行。",
			"异步沟通可把会议时长压缩约三分之一。",
			"工具链碎片化是效率流失的首要原因。",
			"明确单一负责人可显著缩短决策周期。",
			"复盘机制缺失会让同类问题重复出现。",
		},
	},
}

// mockSnippets 返回 k 条语料，带一个通用兜底。
//
// 遍历的是**有序切片**而不是 map：map 的遍历顺序是随机的，
// 用它做匹配会让同一份输入得到不同结果 —— 那正是我们一直在避免的事
// （"输出必须稳定"，L1-6）。
func mockSnippets(query string, k int) []string {
	picked := genericSnippets
	for _, c := range mockCorpus {
		if strings.Contains(query, c.Match) {
			picked = c.Snippets
			break
		}
	}

	out := make([]string, 0, k)
	for i := 0; i < k; i++ {
		// 语料不够时循环取用（k 最大为 5，实际不会触发）
		out = append(out, picked[i%len(picked)])
	}
	return out
}

// genericSnippets 是没命中任何主题时的兜底 —— **长度必须 >= 5**。
var genericSnippets = []string{
	"关于该主题，公开资料显示存在若干可量化的关键指标。",
	"行业集中度较低，头部玩家份额合计不足三成。",
	"需求侧增速快于供给侧，短期仍有结构性缺口。",
	"成本结构正在变化，人力占比下降、技术占比上升。",
	"政策与合规是中期最大的不确定因素。",
}
