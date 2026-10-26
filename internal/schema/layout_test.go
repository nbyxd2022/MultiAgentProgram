package schema

import (
	"strings"
	"testing"
)

func TestLayoutCatalogIsSingleSourceOfTruth(t *testing.T) {
	catalog := LayoutCatalog()
	if len(catalog) == 0 {
		t.Fatal("版式目录不能为空")
	}

	for _, spec := range catalog {
		// 目录里出现的每一个版式，都必须自动成为"合法值" ——
		// 这就是"加一行即全局生效"的兑现，不需要再去 switch 里补 case
		if !spec.Layout.Valid() {
			t.Errorf("目录里的 %q 竟然不是合法版式（校验与目录脱钩了）", spec.Layout)
		}
		if strings.TrimSpace(spec.Description) == "" {
			t.Errorf("%q 缺少说明 —— 模型看不到它就只能靠猜名字", spec.Layout)
		}
		if strings.TrimSpace(spec.BestFor) == "" {
			t.Errorf("%q 缺少 BestFor —— 这是把'猜名字'变成'按内容匹配'的关键", spec.Layout)
		}
	}

	// 反面：自由文本进不来
	for _, bad := range []Layout{"", "two-column", "两栏布局", "COVER", "cover "} {
		if bad.Valid() {
			t.Errorf("%q 不该被当成合法版式", bad)
		}
	}
}

// TestLayoutCatalogReturnsCopy 守住 L1-6 那个教训：包级切片不能把底层数组交出去。
func TestLayoutCatalogReturnsCopy(t *testing.T) {
	before := len(LayoutCatalog())

	c := LayoutCatalog()
	c[0].Layout = "被改坏了"

	if after := len(LayoutCatalog()); after != before {
		t.Fatalf("目录长度被外部改动影响：%d → %d", before, after)
	}
	if LayoutCatalog()[0].Layout == "被改坏了" {
		t.Fatal("返回的必须是副本，否则调用方能改坏全局目录（幽灵修改）")
	}
}

func TestLayoutNamesMatchesCatalog(t *testing.T) {
	names := LayoutNames()
	catalog := LayoutCatalog()

	if len(names) != len(catalog) {
		t.Fatalf("名字数 %d 与目录项数 %d 不一致", len(names), len(catalog))
	}
	for i, spec := range catalog {
		if names[i] != string(spec.Layout) {
			t.Errorf("第 %d 项名字不一致：%q vs %q", i, names[i], spec.Layout)
		}
	}
	if !strings.Contains(JoinLayoutNames(" / "), string(LayoutCover)) {
		t.Error("JoinLayoutNames 应包含全部版式名")
	}
}

func TestRequiredLayout(t *testing.T) {
	cases := []struct {
		name      string
		index     int
		count     int
		want      Layout
		wantFixed bool
	}{
		{"首页是封面", 0, 3, LayoutCover, true},
		{"末页是收尾", 2, 3, LayoutClosing, true},
		{"中间页不固定", 1, 3, "", false},
		{"只有一页时首页优先", 0, 1, LayoutCover, true},
		{"页数为 0 时不约束", 0, 0, "", false},
		{"负数页数不约束", 0, -1, "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := RequiredLayout(c.index, c.count)
			if ok != c.wantFixed || got != c.want {
				t.Errorf("RequiredLayout(%d,%d) = (%q,%v)，期望 (%q,%v)",
					c.index, c.count, got, ok, c.want, c.wantFixed)
			}
		})
	}
}

func TestDescribeLayout(t *testing.T) {
	got := DescribeLayout(LayoutCover)
	if !strings.Contains(got, string(LayoutCover)) || !strings.Contains(got, "封面") {
		t.Errorf("应同时给出名字与说明，实际 %q", got)
	}
	if unknown := DescribeLayout("nope"); unknown != "nope" {
		t.Errorf("未知版式应原样返回，实际 %q", unknown)
	}
}

// TestDesignCarriesLayoutEnum 确认协议层的字段类型真的是枚举，不是 string。
//
// 这是个"编译期契约"的回归测试：一旦有人把 PageDesign.Layout 改回 string，
// 想再回到"自由文本无法校验"的老路，这里会立刻提醒他。
func TestDesignCarriesLayoutEnum(t *testing.T) {
	d := Design{Pages: []PageDesign{
		{PageIndex: 0, Layout: LayoutCover, Note: "开场"},
		{PageIndex: 1, Layout: LayoutBullets, Note: "要点"},
	}}

	var _ Layout = d.Pages[0].Layout // 编译期断言：它必须是 Layout 而不是 string

	// 摘要必须带页码，让下一跳不必去黑板就知道页序
	s := d.Summarize()
	if !strings.Contains(s, "P1:cover") || !strings.Contains(s, "P2:bullets") {
		t.Errorf("摘要应带页序，实际 %q", s)
	}
}
