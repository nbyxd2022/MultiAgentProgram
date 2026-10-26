package agents

import (
	"context"
	"strings"
	"testing"
)

// 这个文件守住 **L2-11**：二次提交**不再静默**覆盖。
//
// 覆盖语义本身是对的（L1 的教训：累加会让同一条算两遍，指标虚高）。
// 错的是"它是静默的" —— 老结论无声消失，没有回执、没有日志、调用方也读不到。
//
// L1-3 的教训在这里复用：**凡是重要事实只进了日志，就等于没发生。**

func TestSubmitOverwriteIsVisible(t *testing.T) {
	submit, err := NewSubmitTool(outlineSpec())
	if err != nil {
		t.Fatalf("构造提交出口失败: %v", err)
	}
	ctx := context.Background()

	// ① 第一次提交：不该出现"替换"字样，也不该计入覆盖
	first, err := submit.Execute(ctx, threePagesArgs)
	if err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	if strings.Contains(first, "替换") {
		t.Errorf("首次提交没有可替换的上一份，回执不该这么说：%s", first)
	}
	if submit.Overwrites() != 0 {
		t.Errorf("首次提交不该计入覆盖，实际 %d", submit.Overwrites())
	}

	// ② 第二次提交：必须**说出来**，而且要告诉模型该怎么办
	second, err := submit.Execute(ctx, threePagesArgs)
	if err != nil {
		t.Fatalf("二次提交失败: %v", err)
	}
	if !strings.Contains(second, "替换") {
		t.Errorf("二次提交必须告知「上一份已被替换」：%s", second)
	}
	if !strings.Contains(second, "合并") {
		t.Errorf("回执必须给出**行动方向**（合并成一份完整结论），而不只是通报事实：%s", second)
	}
	if submit.Overwrites() != 1 {
		t.Errorf("覆盖次数应为 1，实际 %d", submit.Overwrites())
	}

	// ③ 覆盖语义没变：留下的永远是最后一份
	if submit.Product() == nil {
		t.Fatal("提交过就必须有产物")
	}
}

// TestSubmitDescriptionWarnsAboutReplacement 守住"事前告知"。
//
// 光在**事后**告诉模型"你替换掉了上一份"是不够的 ——
// 它已经白花了一轮。所以覆盖语义必须**事前**写进工具描述（第三讲：描述即 Prompt）。
func TestSubmitDescriptionWarnsAboutReplacement(t *testing.T) {
	submit, err := NewSubmitTool(outlineSpec())
	if err != nil {
		t.Fatalf("构造提交出口失败: %v", err)
	}

	desc := submit.Definition().Description
	if !strings.Contains(desc, "替换") {
		t.Errorf("覆盖语义必须事前写进描述，否则模型只会事后才发现：%s", desc)
	}
	if !strings.Contains(desc, "空数组") {
		t.Errorf("必须告诉模型「没有内容也要提交空数组」—— 否则它可能什么都不交，"+
			"系统就无法区分「没发现问题」和「没做检查」：%s", desc)
	}
}

// TestSubmitDecodeFailureDoesNotCountAsOverwrite 守住"校验失败零副作用"。
//
// 解码失败时什么都没写进去，自然也不该计入覆盖 ——
// 否则"模型反复提交非法参数"会被误记成"它在覆盖结论"，
// 掩盖掉真正的问题（L1-7：失败没有计数）。
func TestSubmitDecodeFailureDoesNotCountAsOverwrite(t *testing.T) {
	submit, err := NewSubmitTool(outlineSpec())
	if err != nil {
		t.Fatalf("构造提交出口失败: %v", err)
	}
	ctx := context.Background()

	if _, err := submit.Execute(ctx, threePagesArgs); err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	if _, err := submit.Execute(ctx, `{"pages":`); err == nil {
		t.Fatal("非法 JSON 必须被拒绝")
	}

	if submit.Overwrites() != 0 {
		t.Errorf("解码失败没有覆盖任何东西，不该计数，实际 %d", submit.Overwrites())
	}
	if !submit.Submitted() {
		t.Error("此前成功的提交必须仍然算数")
	}
}
