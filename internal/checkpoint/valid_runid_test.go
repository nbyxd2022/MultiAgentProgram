package checkpoint

import (
	"strings"
	"testing"
)

// ⚠️⚠️ 这条守的是"**校验必须先于任何碰磁盘的动作**"（R59）。
//
//	`FileStore` 的 `Save`/`Load` 内部已经拒绝不安全的 id —— 而那**不够**：
//
//		cmd/l4 会先 `filepath.Join(outDir, "ckpt", c.ID)` 再 `os.RemoveAll(dir)`
//		—— **那两步都不经过 FileStore**。
//
//	于是 `FileStore` 那层校验**形同虚设**：一个 `id: "../../x"` 的用例
//	会把 `outDir/ckpt` **之外**的目录删掉。
//
//	> ### 一道只装在门上的锁，挡不住从窗户进来的人。
//
// ⭐ 而这条测试守的是**那个被导出的校验本身** —— 于是"碰磁盘之前"的那一步
//   能用**同一份**判断（而不是抄一份）。
func TestValidRunIDIsExportedForPreDiskChecks(t *testing.T) {
	cases := []struct {
		id   string
		want bool // true = 合法
		why  string
	}{
		{"baseline-3p", true, "正常的用例 id"},
		{"a.b_c-1", true, "点 / 下划线 / 连字符都该能过"},
		{"", false, "空 id"},
		{".", false, "单点"},
		{"..", false, "双点"},
		{"../evil", false, "**路径遍历** —— 就是它要挡的那种"},
		{"a/b", false, "路径分隔"},
		{`a\b`, false, "反斜杠（Windows）"},
		{"a:b", false, "冒号（Windows 保留）"},
		{strings.Repeat("x", maxRunIDLen+1), false, "太长"},
	}

	for _, c := range cases {
		err := ValidRunID(c.id)
		if c.want && err != nil {
			t.Errorf("%s（%q）：应当合法，实际被拒: %v", c.why, c.id, err)
		}
		if !c.want && err == nil {
			t.Errorf("%s（%q）：应当被拒 —— 而它会被拼进路径", c.why, c.id)
		}
	}

	// ⚠️ 边界：正好等于长度上限的必须过（否则"上限"就不是上限）
	if err := ValidRunID(strings.Repeat("x", maxRunIDLen)); err != nil {
		t.Errorf("正好等于上限的 id 应当通过，实际: %v", err)
	}
}

// ⚠️ 而**这一条是关键**：它证明"导出的那个"与"内部用的那个"**是同一份判断**。
//
//	如果哪天有人给 `ValidRunID` 写了一份新实现（而不是包 `fileName`），
//	两份校验就会**漂移** —— 而"漂移的两份校验"比"没有校验"更坏：
//	它让人以为两处的规则一样。
//
//	⭐ 判据（R58 的 `KindReconcile` 同族）：**一个规则只能有一处定义。**
func TestValidRunIDAgreesWithFileName(t *testing.T) {
	for _, id := range []string{"ok", "a.b", "", "..", "../x", "a/b", strings.Repeat("x", maxRunIDLen+1)} {
		_, nameErr := fileName(id)
		validErr := ValidRunID(id)
		if (nameErr == nil) != (validErr == nil) {
			t.Errorf("id %q：`fileName` 与 `ValidRunID` 的判断不一致（%v vs %v）—— "+
				"它们必须是同一份规则的两种说法", id, nameErr, validErr)
		}
	}
}
