package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"multiagent/internal/schema"
	"multiagent/internal/tool"
)

// 这个文件提供 L2 的**结构化提交出口**（把 L1 的 `submit_review` 通用化）。
//
// 对照精读 R3 的发现（企业仓库 `agents/outline_agent/system_prompt.md`）：
// 它把"子 Agent 该返回什么 JSON"用 **Markdown 举例**写给模型看，
// 结尾加一句"确保 JSON 格式正确且可解析" —— **没有任何代码校验**。
// 于是唯一的安全网是模型自己的自觉。
//
// 我们的做法：**用工具收结构化结论**。
//
//	· 字段名与类型写在 JSON Schema 里（契约，不是举例）；
//	· 有明确的"提交处"，模型不会把结论散落在自然语言里；
//	· 解码失败 → 走 L0 那条现成的回灌通道自愈；
//	· 提交了什么、合规不合规，**代码全程可见**（可统计、可测试）。

// SubmitSpec 描述"某个角色的产物长什么样"。
//
// 为什么做成"数据 + 解码函数"，而不是给每个角色各写一个工具类型？
//
//	因为四个角色的提交出口**结构完全一样**：收 JSON → 校验 → 交给调用方。
//	不同的只有"字段有哪些"与"怎么解"。把差异压成一份 Spec，
//	就不必把同一套样板复制四遍（复制四遍 = 四个地方会各自腐化）。
type SubmitSpec struct {
	// ToolName 是模型要调用的工具名，如 submit_research。
	ToolName string

	// Description 是给模型看的说明（含"必须调用"这类纪律）。
	Description string

	// Parameters 是结论的字段声明 —— 这就是"契约"本身。
	Parameters []tool.Parameter

	// Decode 把模型给的 JSON 解成类型化产物，**并在这一层做语义校验**。
	// 校验失败 → 返回 error → 回灌给模型自愈。
	Decode func(args string) (schema.Payload, error)
}

// SubmitTool 是通用的提交出口。
type SubmitTool struct {
	spec SubmitSpec

	submitted bool
	product   schema.Payload

	// overwrites 记录"新结论替换旧结论"发生的次数。
	//
	// 它是 **L2-11** 的修复：此前二次提交会**静默**覆盖（老结论直接消失，
	// 没有日志、没有回执、调用方也读不到）。
	//
	// 现在它有两个出口，缺一不可：
	//   · 写进**回执**交给模型 —— 这样模型才知道自己刚才把上一份内容替换掉了；
	//   · 通过 `Overwrites()` 交给调用方 —— 这样它才是可统计、可断言的。
	//
	// 为什么必须进回执，而不只是打日志？
	//	因为 L1-3 的教训：**凡是重要事实只进了日志，就等于没发生。**
	//	日志给人看，回执给模型看 —— 而我们希望**模型**能纠正自己。
	overwrites int
}

// NewSubmitTool 构造提交出口。
//
// 构造期校验：这类错误是"程序员的错误"，必须在启动时就说，
// 而不是等模型调了半天才发现这个工具永远收不下结论。
func NewSubmitTool(spec SubmitSpec) (*SubmitTool, error) {
	switch {
	case strings.TrimSpace(spec.ToolName) == "":
		return nil, fmt.Errorf("提交工具缺少名字")
	case len(spec.Parameters) == 0:
		return nil, fmt.Errorf("提交工具 %s 没有声明任何参数（模型将无处填写结论）", spec.ToolName)
	case spec.Decode == nil:
		return nil, fmt.Errorf("提交工具 %s 没有提供解码函数", spec.ToolName)
	}
	return &SubmitTool{spec: spec}, nil
}

// Definition 实现 tool.Tool 接口。
func (t *SubmitTool) Definition() tool.Definition {
	return tool.Definition{
		Name: t.spec.ToolName,
		Description: t.spec.Description +
			" 每次调用都会**替换**上一次提交的全部内容（不累加）；" +
			"所以请一次提交**完整**的结论，而不是分几次「补充」。" +
			"如果没有内容要交，传一个空数组也比不调用好 —— 那样系统才知道你确实检查过了。",
		Parameters: t.spec.Parameters,
	}
}

// Execute 实现 tool.Tool 接口。
//
// 四条纪律，每一条都对应一种真实事故：
//
//	① 解码 + 校验失败 → 返回 error（走回灌自愈，而不是静默收下一份脏结论）；
//	② **覆盖而非累加**（L1 的教训：累加会让同一条算两遍，指标虚高）；
//	③ 记录"提交过没有" —— 让调用方能区分"没发现问题"和"没做检查"；
//	④ **覆盖本身要可见** —— 让模型知道自己刚把上一份内容替换掉了（L2-11）。
func (t *SubmitTool) Execute(_ context.Context, args string) (string, error) {
	product, err := t.spec.Decode(args)
	if err != nil {
		return "", err
	}
	if product == nil {
		return "", fmt.Errorf("%s：解码结果为空", t.spec.ToolName)
	}

	replaced := t.submitted
	note := "结论已记录。"
	if replaced {
		t.overwrites++
		// ⚠️ 这段文案是**给模型看的**，所以必须说明"该怎么办"，
		//    而不只是通报一件既成事实（L1-5：反馈要可执行）。
		note = fmt.Sprintf(
			"注意：本次提交**替换**了你上一次提交的全部内容（这是第 %d 次替换，上一份已丢弃）。"+
				"如果你本意是「分几次补充」，请把几次的内容合并成**一份完整结论**重新提交一次。",
			t.overwrites)
	}

	t.product = product
	t.submitted = true

	out, err := json.Marshal(map[string]any{
		"ok":                true,
		"note":              note,
		"replaced_previous": replaced,
	})
	if err != nil {
		return "", fmt.Errorf("序列化回执失败: %w", err)
	}
	return string(out), nil
}

// Submitted 表示模型是否**真的**调用过这个提交口。
//
// 调用方必须用它来区分两种"空产出"：
//
//	提交了空结论   → 检查过了，确实没有
//	从来没提交过   → **根本没检查**（绝不能当成"没有问题"）
//
// 这是 L1-3（评审不完整）与 Critic `submit_review` 立下的同一条底线。
func (t *SubmitTool) Submitted() bool { return t.submitted }

// Product 返回模型提交的产物（未提交时为 nil）。
func (t *SubmitTool) Product() schema.Payload { return t.product }

// Overwrites 返回"覆盖了上一次提交"发生的次数。
//
// 它存在的意义：让 L2-11 那个事实**可统计、可断言** ——
// 一个"模型总在分几次提交、每次都丢掉前面内容"的问题，
// 只有能被数出来，才谈得上被发现和优化。
func (t *SubmitTool) Overwrites() int { return t.overwrites }
