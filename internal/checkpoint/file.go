package checkpoint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// dataSuffix 是**完好**记录的文件后缀。
//
// 用它而不是"凡是文件都是记录"，是因为目录里还会短暂地存在临时文件。
// 拿后缀当过滤器，是"什么算一条记录"这件事的**唯一定义**。
const dataSuffix = ".json"

// tmpPattern 是临时文件的命名模板。
//
// 前缀 `ckpt-` 与后缀 `.tmp` 都是刻意的：
// **它一眼就能被认出来不是一条记录**（不会被 `.json` 后缀匹配到）。
const tmpPattern = "ckpt-*.tmp"

// maxRunIDLen 是 run id 的长度上限。
//
// 留得很保守：多数文件系统的单个文件名上限是 255 字节，
// 而我们要在它后面再拼一个后缀、再留一点余地。
// 太长的 id 会得到一个 `ENAMETOOLONG` —— 那句错误完全看不出"是我把 id 起太长了"。
const maxRunIDLen = 128

// FileStore 是 `Store` 的**文件实现**（一个 run 一个文件）。
//
// # 为什么这一版要自己实现原子性
//
// 企业仓库的三种后端都不需要（见 store.go 的包注释）。而文件写入
// **不是**原子的：`os.WriteFile` 会先截断再写，中途被杀就是一个**半截文件** ——
// 而它长得和一个正常文件一模一样，下次启动时只会看到
// `unexpected end of JSON input`，完全看不出"是上次写崩了"。
//
// 做法是经典的 **write-temp → fsync → rename**：
//
//	① 写到同目录下的临时文件（**同目录**很关键：rename 只在同一文件系统内原子）
//	② fsync 数据，确保它真的落到磁盘上而不只是落在页缓存里
//	③ rename 覆盖目标名字 —— 这一步是**原子**的
//	④ fsync 目录，让"改名"这件事本身也持久化
//
// 于是任何时刻的磁盘状态只有两种：**旧的完整版**，或者**新的完整版**。
type FileStore struct {
	dir string
}

// NewFileStore 创建一个文件存储，并**在构造期就把目录建好**。
//
// 为什么在这里建目录，而不是拖到第一次 Save？
//
//	因为"这个存储能不能用"是**装配期**的问题。
//	等到第一次 Save 才发现目录不存在（甚至路径被一个文件占住了），
//	错误会出现在一个离原因很远的地方 —— 而那时任务已经跑到一半了。
//
//	与 L2 适配器的构造期校验、`BuildGraph` 的建图期校验是同一条：
//	**错配是本程序的 bug，越靠近它的来源发现越便宜。**
func NewFileStore(dir string) (*FileStore, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("checkpoint 目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 checkpoint 目录 %s 失败: %w", dir, err)
	}
	return &FileStore{dir: dir}, nil
}

// Dir 返回存储目录（打印用）。
func (s *FileStore) Dir() string { return s.dir }

// Save 原子地写入一份状态。
func (s *FileStore) Save(runID string, raw []byte) error {
	name, err := fileName(runID)
	if err != nil {
		return err
	}
	target := filepath.Join(s.dir, name)

	// 空内容几乎一定是上游的 bug（没人会故意存空的）。
	//
	// 为什么在**这里**挡，而不是等到读回来时报 "corrupt"？
	//
	//	因为写进去之后，那份空文件就和一个"曾经正常、后来被截断"的文件
	//	**完全无法区分**了。在源头挡住，错误信息里还带着 run id；
	//	到下游再挡，只能对着一段字节说"这不是合法 JSON"。
	//	（"能在源头挡住的问题，不要留给下游的读方" —— 与 R30 的写盘前自校验同一条。）
	if len(raw) == 0 {
		return fmt.Errorf("拒绝保存空内容（run=%s）：空文件之后无法与「被截断的损坏文件」区分", runID)
	}

	tmp, err := os.CreateTemp(s.dir, tmpPattern)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()

	// 任何一步失败都要把这个临时文件删掉。
	//
	// ⚠️ 但**不能只靠它**：进程被 kill -9 时这段清理代码根本不会执行。
	// 所以 `List` 还要独立地忽略临时文件 —— **双保险**，
	// 因为"进程被杀"恰好是这一整套机制存在的理由，不能假设它在。
	discard := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		discard()
		return fmt.Errorf("写临时文件失败: %w", err)
	}

	// ★ fsync 数据。
	//
	// 没有它，rename 之后进程/机器崩掉时，可能出现
	// **文件名在、内容还在页缓存里没落盘** —— 一个长度为 0 或半截的文件。
	// 那正是我们想避免的东西，只是换了个地方发生。
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		discard()
		return fmt.Errorf("fsync 临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		discard()
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}

	// ★ 原子替换。同一个文件系统内，rename 是内核保证的原子操作。
	if err := renameWithRetry(tmpName, target); err != nil {
		discard()
		return fmt.Errorf("原子替换 %s 失败: %w", target, err)
	}

	syncDir(s.dir)
	return nil
}

// renameAttempts 是原子替换的重试上限。
//
// **有界**是这里唯一重要的事（06-L3设计.md §10 问题 1）：
// 一个"一直重试直到成功"的实现，在目标被永久占用时会**卡死** ——
// 而"重试 3 次然后如实报错"最坏只是浪费几毫秒。
const renameAttempts = 3

// renameWithRetry 做一次原子替换，失败时有界重试。
//
// # 为什么需要重试 —— 一个实测出来的平台差异
//
//	POSIX   → `rename(2)` 是内核保证的原子替换，只要有目录写权限就一定成功。
//	Windows → `MoveFileEx(REPLACE_EXISTING)` **在目标正被另一次替换占用时**
//	          会返回 `ERROR_ACCESS_DENIED`。
//
//	实测（12 个 goroutine 并发 Save 同一个 run）：Windows 上 6 次失败，
//	错误全是 `rename ...: Access is denied.`；POSIX 上不会出现。
//
// ⚠️ 但**它不破坏原子性契约**。契约的核心是"**永远看不到半截文件**"，
// 而不是"Save 永不失败" —— 这种情况下目标文件仍然是**上一个完整版本**。
// **失败是安全的，只是不理想。**
//
// # 为什么不区分错误类型
//
// 直觉是"只在瞬时错误上重试"。但**判断一个 rename 失败是不是瞬时的，
// 需要平台相关的错误码**：Windows 的 `ERROR_ACCESS_DENIED`(5) 与 POSIX 的
// `EACCES`(13) 不是同一个值，`os.IsPermission` 在两边的行为并不一致 ——
// 那会写出一段"在 Linux 上看着对、在 Windows 上静默不重试"的代码。
//
// 不区分的代价很有限：**有界重试最多多花几毫秒**，然后原样返回错误（错误没有丢）。
//
//	**"少猜一点、代价可控"，比"猜对了很优雅、猜错就静默失效"更稳。**
func renameWithRetry(tmpName, target string) error {
	var last error
	for i := 0; i < renameAttempts; i++ {
		if err := os.Rename(tmpName, target); err == nil {
			return nil
		} else {
			last = err
		}
		// 递增的短等待：错开瞬时竞争的窗口。
		// 这里不需要抖动（企业仓库那套 5~30s 抖动是防"多页并发同时打爆上游"），
		// 因为竞争者最多是同一个进程里的几个 goroutine，量级完全不同。
		time.Sleep(time.Duration(i+1) * time.Millisecond)
	}
	return fmt.Errorf("重试 %d 次后仍然失败（常见原因：目标文件正被另一个写入者占用）: %w",
		renameAttempts, last)
}

// Load 读回一份状态。
func (s *FileStore) Load(runID string) ([]byte, bool, error) {
	name, err := fileName(runID)
	if err != nil {
		return nil, false, err
	}

	raw, err := os.ReadFile(filepath.Join(s.dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		// ⚠️ "还没存过" 不是错误 —— 第一次跑就是这样的。
		//    把它变成一个 error，会让正常路径和故障路径长得一样。
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("读取 %s 失败: %w", name, err)
	}
	return raw, true, nil
}

// List 返回全部**完好**的 run id（字典序）。
func (s *FileStore) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 构造函数会建目录，所以这里基本走不到 —— 但"目录被人在运行期删了"
			// 不该表现成"列表查询失败"，它和"一个 run 都没有"是同一件事。
			return nil, nil
		}
		return nil, fmt.Errorf("读取目录 %s 失败: %w", s.dir, err)
	}

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		// 子目录不是记录（我们从不建子目录；出现了就是别人放的东西）
		if e.IsDir() {
			continue
		}
		// ⚠️ 关键的一条：**只认 `.json` 后缀**。
		//
		//	一次"写到一半就被杀"会留下 `ckpt-XXXX.tmp`。
		//	如果把它也当成一条记录列出来，调用方会去 Load 一个**残缺的副本** ——
		//	而它看起来和一个正常的 run **一模一样**（同样的体积、同样的位置）。
		//
		//	这是"存在 ≠ 完整"（L2-2）在存储层的同一个形状：
		//	**一个能被列举出来的东西，必须真的可用。**
		name := e.Name()
		if !strings.HasSuffix(name, dataSuffix) {
			continue
		}
		out = append(out, strings.TrimSuffix(name, dataSuffix))
	}
	sort.Strings(out) // 排序：与黑板 Keys / state.Keys 同一条（输出必须稳定）
	return out, nil
}

// ─── 文件名 ─────────────────────────────────────────────────────────

// fileName 把 run id 变成一个安全的文件名，**不安全就拒绝**。
//
// # 为什么是"拒绝"而不是"清洗"
//
// 企业仓库的 `checkpointstore/ks3.go` 是直接拼的：
//
//	fmt.Sprintf("beautifulagent/checkpoint/%s.data", key)
//
// 它在**对象存储**里是安全的 —— 那里的 key 是一个**扁平字符串**，
// `../` 没有任何特殊含义。但**文件系统不是**：那里 `a/b` 是一层目录，
// `../x` 会写到**上一级**去。
//
// **同一个 key，后端不同、危险性不同。** 所以"要不要校验"的答案取决于后端 ——
// 而更稳的判断是：**不要依赖后端的宽容**。
// 文件系统是唯一一个"后端会给字符串赋予结构含义"的情形，所以它必须校验。
//
// 至于为什么不清洗（把 `/` 换成 `_`）：
//
//	清洗会**制造碰撞** —— `a/b` 与 `a_b` 都会被清成 `a_b`，
//	于是两个不同的 run 共用一个文件，**后写的把先写的顶掉，且无人知晓**。
//	那正是 F4（同一个 store 两种写入语义）那个"静默"的形状。
//
//	拒绝则把问题暴露在**调用方的错误**上，而不是变成一个数据事故。
//	（与 L2 的"非法版式：拒绝 + 回灌"而不是"降级"是同一个取舍 ——
//	 我们选质量，因为这里的代价只是"改一个 run id"。）
//
// ValidRunID 报告一个 run id 是否**能被安全地拼进路径**。
//
// # ⚠️ 它存在的理由：校验必须**先于任何碰磁盘的动作**
//
//	`Save` / `Load` 内部已经拒绝不安全的 id。而 `cmd/l4` 会在调它们**之前**
//	做一件事：
//
//		dir := filepath.Join(outDir, "ckpt", c.ID)   // ← c.ID 来自 cases.jsonl
//		os.RemoveAll(dir)                            // ← **这一步不经过 FileStore**
//
//	于是 `FileStore` 那层校验**形同虚设** —— 一个 `id: "../../x"` 的用例
//	会把 `outDir/ckpt` **之外**的目录删掉。
//
//	> ### 一道只装在门上的锁，挡不住从窗户进来的人。
//
// ⭐ 修法不是"在 `cmd/l4` 里再写一遍校验"（两处会漂移），
//
//	而是**把这唯一的一份导出**，让"碰磁盘之前"的那一步能自己验。
func ValidRunID(runID string) error {
	_, err := fileName(runID)
	return err
}

func fileName(runID string) (string, error) {
	id := strings.TrimSpace(runID)

	switch {
	case id == "":
		return "", fmt.Errorf("run id 不能为空")
	case id == "." || id == "..":
		return "", fmt.Errorf("run id %q 不是合法名字", id)
	case len(id) > maxRunIDLen:
		return "", fmt.Errorf("run id 太长（%d 字节，上限 %d）", len(id), maxRunIDLen)
	}

	for _, r := range id {
		if !safeIDRune(r) {
			return "", fmt.Errorf(
				"run id %q 含不安全字符 %q —— 在文件系统里它会被当成**路径的一部分**。"+
					"只允许字母、数字、'-'、'_'、'.'",
				id, r)
		}
	}
	return id + dataSuffix, nil
}

// safeIDRune 报告一个字符能不能出现在文件名里。
//
// 规则刻意收得很紧（ASCII 字母数字 + `-_.`）：
//
//	· 宽松的规则很难被读代码的人验证（"还有哪些字符是危险的？"）；
//	· 而 run id 是**我们自己生成的**，没有任何理由需要中文或空格。
//
// 一旦某天真的需要更宽的字符集，那是**一个明确的变更**，
// 而不是"当初没想全"。
func safeIDRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '-' || r == '_' || r == '.':
		return true
	default:
		return false
	}
}

// syncDir 把目录项本身也刷到磁盘。
//
// ⚠️ 这一步经常被忘掉，而它是 POSIX 的持久化要求：
//
//	`rename` 修改的是**目录项**，只 fsync 文件**不够**。
//	机器断电后可能出现"文件内容在、名字不在"，或者反过来。
//
// Windows 不允许 `os.Open` 一个目录，所以这里**容忍失败**：
// 该做的动作做了；在做不到的环境里不阻塞业务 ——
// **这不是正确性问题（不会产生半截文件），而是持久化强度问题**。
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
