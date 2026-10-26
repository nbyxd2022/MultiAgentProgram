package checkpoint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 本文件测的是**存储**，不是状态。
//
// ⚠️ 关于"原子性"这一组测试，先说实话：**单元测试只能给出证据，给不出证明。**
//
//	真正的原子性由内核的 `rename` 语义保证（同一个文件系统内），
//	测试能做的是三件事：
//	  ① 证明机制**被走过了**（目录里不留临时文件）；
//	  ② 证明垃圾**不会**被当成有效记录（`List` 只认 `.json`）；
//	  ③ 并发写之后读到的**永远是某一个完整版本**，不是混合体。
//
//	而"进程在写盘中途被 kill -9"这个真正的场景，只能在 `cmd/l3` 里
//	用**真的杀进程**来验证（`L3_KILL_AFTER`）—— 见 06-L3设计.md §9。

func newStore(t *testing.T) *FileStore {
	t.Helper()
	// t.TempDir() 会自动清理，所以本文件的测试不需要留任何残留
	s, err := NewFileStore(filepath.Join(t.TempDir(), "ckpt"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	return s
}

func mustSave(t *testing.T, s *FileStore, runID, content string) {
	t.Helper()
	if err := s.Save(runID, []byte(content)); err != nil {
		t.Fatalf("保存 %s 失败: %v", runID, err)
	}
}

func mustLoad(t *testing.T, s *FileStore, runID string) string {
	t.Helper()
	raw, found, err := s.Load(runID)
	if err != nil {
		t.Fatalf("读取 %s 出错: %v", runID, err)
	}
	if !found {
		t.Fatalf("读取 %s 说「没有」", runID)
	}
	return string(raw)
}

// ─── 基本存取 ───────────────────────────────────────────────────────

func TestFileStoreRoundTrip(t *testing.T) {
	s := newStore(t)
	mustSave(t, s, "run-1", `{"a":1}`)

	if got := mustLoad(t, s, "run-1"); got != `{"a":1}` {
		t.Errorf("读回来的内容不对：%q", got)
	}

	// 字节必须**逐字节一致** —— 存储不该"顺手美化"内容。
	// （状态层的"两次序列化字节一致"依赖它；存储一旦改动内容，那条就断了。）
	tricky := "{\n  \"b\":\t2,\n  \"中文\": \"值\"\n}"
	mustSave(t, s, "run-tricky", tricky)
	if got := mustLoad(t, s, "run-tricky"); got != tricky {
		t.Errorf("存储改动了内容：\n  写入 %q\n  读出 %q", tricky, got)
	}
}

// ⚠️ "还没存过" 必须是 (found=false, err=nil)，不能是一个 error。
//
// 把两者合成一个 error，会让**正常路径和故障路径长得一样** ——
// 排查的人会去查磁盘，而磁盘是好的。
func TestLoadMissingIsNotAnError(t *testing.T) {
	s := newStore(t)

	// ⚠️ 这里的 id 必须是**合法**的（ASCII）。
	//    第一版我写了个中文 id（"从没存过"），结果报的是
	//    「含不安全字符 '从'」—— 于是这条用例测到的是**另一条校验**，
	//    完全没碰到它想测的"没有"分支。
	//    **用一个会先撞上别的校验的输入去测分支，是最常见的假通过。**
	raw, found, err := s.Load("never-saved")
	if err != nil {
		t.Errorf("「没有」不该是一个 error，实际：%v", err)
	}
	if found {
		t.Error("不该说「找到了」")
	}
	if raw != nil {
		t.Errorf("没找到时不该返回内容，实际 %q", raw)
	}
}

func TestSaveOverwritesPreviousVersion(t *testing.T) {
	s := newStore(t)
	mustSave(t, s, "run-1", "v1")
	mustSave(t, s, "run-1", "v2")

	if got := mustLoad(t, s, "run-1"); got != "v2" {
		t.Errorf("覆盖之后应当读到 v2，实际 %q", got)
	}

	// 覆盖**不该**留下第二份文件（我们的模型是"一个 run 一个文件"，
	// 版本由状态自己管 —— 那是 `State.Version` 与黑板版本的事，不是存储的事）
	files := readDirNames(t, s.Dir())
	if len(files) != 1 {
		t.Errorf("覆盖之后目录里应当只有 1 个文件，实际 %d 个：%v", len(files), files)
	}
}

// 空内容在源头就被拒绝。
//
// 理由：一个空文件和一个"曾经正常、后来被截断"的文件**完全无法区分**，
// 而后者是我们最想避免的东西。在源头挡住，错误信息里还带着 run id。
func TestSaveRejectsEmptyContent(t *testing.T) {
	s := newStore(t)

	if err := s.Save("run-1", nil); err == nil {
		t.Fatal("应当拒绝空内容")
	}
	if err := s.Save("run-1", []byte{}); err == nil {
		t.Fatal("应当拒绝零长度内容")
	}

	// 被拒绝时**零副作用**（与 Blackboard.Put 的校验失败同一条：
	// 一次非法提交不该留下任何痕迹）
	if files := readDirNames(t, s.Dir()); len(files) != 0 {
		t.Errorf("被拒绝的保存留下了文件：%v", files)
	}
}

// ─── 文件名安全 ─────────────────────────────────────────────────────

// run id 会被拼成**路径的一部分**，所以必须拒绝不安全的字符。
//
// 判据不是"看起来危险"，而是"这个字符会不会改变路径的结构"。
func TestFileNameRejectsUnsafeIDs(t *testing.T) {
	cases := []struct {
		name    string
		runID   string
		wantSub string
	}{
		{"空", "", "不能为空"},
		{"纯空白", "   ", "不能为空"},
		{"单点", ".", "不是合法名字"},
		{"双点", "..", "不是合法名字"},
		{"含路径分隔（正斜杠）", "a/b", "不安全字符"},
		{"含路径分隔（反斜杠）", `a\b`, "不安全字符"},
		{"路径遍历", "../etc/passwd", "不安全字符"},
		{"含冒号（Windows 保留）", "a:b", "不安全字符"},
		{"含空格", "a b", "不安全字符"},
		{"含中文", "运行-1", "不安全字符"},
		{"太长", strings.Repeat("x", maxRunIDLen+1), "太长"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := fileName(c.runID)
			if err == nil {
				t.Fatalf("应当被拒绝，却通过了")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误里应含 %q，实际: %v", c.wantSub, err)
			}
		})
	}

	// 边界：正好等于上限的 id 必须通过（否则上限就不是上限了）
	if _, err := fileName(strings.Repeat("x", maxRunIDLen)); err != nil {
		t.Errorf("正好等于长度上限的 id 应当通过，实际: %v", err)
	}
}

// 不安全的 id 在 Save / Load 两个入口都要被挡，而且**不能碰磁盘**。
func TestUnsafeIDIsRejectedBeforeTouchingDisk(t *testing.T) {
	s := newStore(t)
	mustSave(t, s, "good", "v1")

	if err := s.Save("../evil", []byte("x")); err == nil {
		t.Fatal("Save 应当拒绝路径遍历的 id")
	}
	if _, _, err := s.Load("../evil"); err == nil {
		t.Fatal("Load 应当拒绝路径遍历的 id")
	}

	// 目录里仍然只有那一个文件 —— 拒绝是**零副作用**的
	files := readDirNames(t, s.Dir())
	if len(files) != 1 || files[0] != "good.json" {
		t.Errorf("被拒绝的操作影响了目录：%v", files)
	}
}

// ─── 原子写的证据 ───────────────────────────────────────────────────

// 保存之后目录里**不留**临时文件。
func TestSaveLeavesNoTemporaryFiles(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 5; i++ {
		mustSave(t, s, fmt.Sprintf("run-%d", i), fmt.Sprintf("content-%d", i))
	}

	files := readDirNames(t, s.Dir())
	if len(files) != 5 {
		t.Fatalf("应当只有 5 个文件，实际 %d 个：%v", len(files), files)
	}
	for _, f := range files {
		if strings.HasSuffix(f, ".tmp") {
			t.Errorf("残留了临时文件 %q", f)
		}
	}
}

// ⚠️ **本文件最重要的一条**：写到一半留下的垃圾，绝不能被当成一条记录。
//
// 这是"存在 ≠ 完整"（L2-2）在存储层的同一个形状：
// **一个能被列举出来的东西，必须真的可用。**
//
// 场景：上一次 Save 写到一半被 kill -9（清理代码根本没机会执行），
// 目录里留下一个 `ckpt-XXXX.tmp`，里面是**半截 JSON**。
// 如果 `List` 把它列出来，调用方会去加载一个残缺的副本 ——
// 而它和一个正常的 run **长得一模一样**。
func TestListIgnoresPartialTemporaryFiles(t *testing.T) {
	s := newStore(t)
	mustSave(t, s, "run-1", `{"ok":true}`)

	// 手工造一个"写到一半被杀"的现场：合法的临时文件名 + 半截内容
	partial := filepath.Join(s.Dir(), "ckpt-123456.tmp")
	if err := os.WriteFile(partial, []byte(`{"version":1,"run_id":"run-2","boa`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.List()
	if err != nil {
		t.Fatalf("List 出错: %v", err)
	}
	if len(got) != 1 || got[0] != "run-1" {
		t.Errorf("只应列出完好的 run-1，实际 %v —— 残缺的临时文件被当成了记录", got)
	}

	// 顺带验一遍"它即便是合法的 JSON 名字也不该出现"：
	// 换一个不带 .json 后缀的名字，同样不该被列出。
	other := filepath.Join(s.Dir(), "ckpt-999999.tmp.json.bak")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.List(); len(got) != 1 {
		t.Errorf("非 .json 后缀的文件不该被列出，实际 %v", got)
	}
}

// 并发写同一个 run：读到的必须是**某一个完整版本**，不是它们的混合。
//
// ⚠️ 这条测试断言的是**原子性**，不是"Save 永不失败"。区别很重要：
//
//	原子性契约的核心是「**永远看不到半截文件**」。
//	而 Windows 的 `MoveFileEx(REPLACE_EXISTING)` 在目标正被另一个替换占用时
//	会返回 `ERROR_ACCESS_DENIED` —— 实测 12 个并发写有 6 次失败。
//
//	那些失败**不破坏契约**：目标文件仍然是**上一个完整版本**。
//	（`Save` 已经做了有界重试把这种情况压低，但不保证 100%。）
//
// 所以这里统计失败次数并打印出来，但**不因为它红** ——
// 红只可能因为"读到了混合体"。
//
// 没有原子 rename 时，典型的坏结果是"前半段来自 writer 3、后半段来自 writer 7"，
// 而那种内容**解不开**，且看不出是谁造成的。
func TestConcurrentSavesLeaveACompleteVersion(t *testing.T) {
	s := newStore(t)

	const n = 12
	contents := make([][]byte, n)
	for i := range contents {
		// 每条都足够大，让"边写边被读"有机会发生
		contents[i], _ = json.Marshal(map[string]any{
			"writer": i,
			"filler": strings.Repeat(fmt.Sprintf("%d", i), 2048),
		})
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Save("run", contents[i]); err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if len(failures) > 0 {
		// 打印但不判失败 —— 它是平台特性，不是契约违背（见函数注释）
		t.Logf("并发保存中有 %d/%d 次失败（Windows 上属预期）：%v",
			len(failures), n, failures[0])
	}

	got, found, err := s.Load("run")
	if err != nil || !found {
		t.Fatalf("读取失败: found=%v err=%v", found, err)
	}

	for _, want := range contents {
		if bytes.Equal(got, want) {
			// ✅ 命中了某一个完整版本 —— 原子性成立
			//
			// 顺带验一件事：失败的那些尝试**没有留下垃圾**
			//（`discard()` 在 rename 失败时会删掉临时文件）。
			if files := readDirNames(t, s.Dir()); len(files) != 1 {
				t.Errorf("并发之后目录里应当只有 1 个文件，实际 %v", files)
			}
			return
		}
	}
	t.Errorf("读到的内容不是任何一个完整版本（长度 %d）—— 说明写入不是原子的：\n%s",
		len(got), got)
}

// ─── List ───────────────────────────────────────────────────────────

func TestListIsSortedAndIgnoresSubdirs(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"zebra", "alpha", "middle"} {
		mustSave(t, s, id, "x")
	}
	// 子目录不是记录
	if err := os.Mkdir(filepath.Join(s.Dir(), "sub.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "middle", "zebra"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("List 应当字典序且忽略子目录：得到 %v，期望 %v", got, want)
	}
}

func TestListEmptyStore(t *testing.T) {
	got, err := newStore(t).List()
	if err != nil {
		t.Fatalf("空目录不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("空目录应当返回空列表，实际 %v", got)
	}
}

// ─── 构造 ───────────────────────────────────────────────────────────

// 目录在构造期就建好 —— "这个存储能不能用"是装配期的问题。
func TestNewFileStoreCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "ckpt")

	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if s.Dir() != dir {
		t.Errorf("Dir() 不对：%q", s.Dir())
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("构造期应当把目录建好，实际 err=%v", err)
	}
}

func TestNewFileStoreRejectsEmptyDir(t *testing.T) {
	for _, d := range []string{"", "   "} {
		if _, err := NewFileStore(d); err == nil {
			t.Errorf("空目录名 %q 应当被拒绝", d)
		}
	}
}

// 目录被一个**文件**占住时，构造期就该报错 —— 而不是等到第一次 Save。
func TestNewFileStoreFailsWhenPathIsAFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := NewFileStore(dir); err == nil {
		t.Fatal("路径被文件占住时应当报错")
	}
}

// ─── 小工具 ─────────────────────────────────────────────────────────

func readDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
