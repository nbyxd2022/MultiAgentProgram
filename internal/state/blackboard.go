// Package state 保存多 Agent 协作过程中的**共享状态** —— 目前只有黑板。
//
// 为什么单独一个包，而不是塞进 schema？
//
//	schema 是"数据长什么样"（协议）；state 是"数据存在哪里"（存储）。
//	黑板是会演进的东西（L3 会加 checkpoint 持久化），而协议应该保持稳定。
//	把两者分开，将来换存储（文件 / Redis）不必动协议一行（原则五）。
//
// 这个包**只依赖 schema**，不知道任何 Agent 的存在 ——
// 黑板不该知道"谁在用它"，否则加一个 Agent 就要改一次黑板。
package state

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"multiagent/internal/schema"
)

// 哨兵错误：调用方可以用 errors.Is 判断，而不必去匹配错误文案。
var (
	// ErrArtifactNotFound 表示引用指向的产物不存在（或版本超出范围）。
	ErrArtifactNotFound = errors.New("黑板中不存在该产物")
	// ErrBadRef 表示引用格式非法。
	ErrBadRef = errors.New("非法的产物引用")
)

// Blackboard 是 Agent 之间的**共享产物存储**。
//
// 三个设计约束（直接对应设计课上的结论）：
//
//  1. **追加，绝不覆盖**
//     同一个 key 每 Put 一次就多一版。这既是"可追溯"，也是 `schema.RefOf`
//     能返回 `key@version` 的**前提** —— 引用不可变，靠的不是引用本身，
//     而是"被引用的那一版永远不会被改写"。
//
//  2. **只依赖 schema**
//     它不知道 research / outline / supervisor 的存在。加、删 Agent 都不用动它。
//
//  3. **进出都是深拷贝**
//     黑板是**跨 Agent 共享的可变状态**：任何一个 Agent 拿到产物后顺手改一下，
//     其他人就全被污染了。这类 bug 极难定位（"我明明没改它"）。
type Blackboard struct {
	mu sync.RWMutex

	// byKey: key → 按版本升序的历史；下标 i 对应的版本号是 i+1。
	// 用切片而不是 map[int]Artifact：版本是**连续递增**的，
	// 切片天然表达"第几版"，也让"取最新"变成 O(1)。
	byKey map[string][]schema.Artifact
}

// NewBlackboard 创建一个空黑板。
func NewBlackboard() *Blackboard {
	return &Blackboard{byKey: make(map[string][]schema.Artifact)}
}

// Put 提交一份产物，返回**已经分配好版本**的那一份。
//
// 规则：
//
//	· 版本由黑板分配（调用方指定版本会被拒绝 —— 谁拥有它，谁维护它）；
//	· key / producedBy 会被规范化（去首尾空白），保证"一个 key 只有一种写法"；
//	· 存储前必须过 Validate()：脏数据进不了黑板（原则一：确定性兜底）；
//	· 入参会被深拷贝，之后调用方再改它也不会影响黑板。
func (b *Blackboard) Put(a schema.Artifact) (schema.Artifact, error) {
	// 规范化：如果 Put 存的是 " k "、而 Get 查的是 "k"，
	// 就会出现"明明提交过却找不到"的幽灵故障。
	a.Key = strings.TrimSpace(a.Key)
	a.ProducedBy = strings.TrimSpace(a.ProducedBy)

	if a.Version != 0 {
		return schema.Artifact{}, fmt.Errorf(
			"Put 不接受外部指定的版本号（收到 %d）：版本由黑板分配", a.Version)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	a.Version = len(b.byKey[a.Key]) + 1
	if err := a.Validate(); err != nil {
		// 注意：这里什么都没写进去 —— 校验失败必须是"零副作用"的，
		// 否则一次非法提交会白白推进版本号，让后续引用全部错位。
		return schema.Artifact{}, err
	}

	stored := cloneArtifact(a)
	b.byKey[a.Key] = append(b.byKey[a.Key], stored)
	return cloneArtifact(stored), nil
}

// Get 按引用取产物。
//
// ref 支持两种写法：
//
//	"outline"     → 取该 key 的**最新版**
//	"outline@2"   → 取指定版本（这样写出来的引用是**不可变**的）
//
// 为什么两种都支持？
//
//	"最新版"适合人手动查看看板；"带版本"适合**写进流程的引用** ——
//	返工/重试时，旧引用必须仍指向它当时看到的那一版，否则日志与回放全部失真。
func (b *Blackboard) Get(ref string) (schema.Artifact, error) {
	key, version, err := ParseRef(ref)
	if err != nil {
		return schema.Artifact{}, err
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	versions := b.byKey[key]
	if len(versions) == 0 {
		return schema.Artifact{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, key)
	}

	if version == 0 {
		return cloneArtifact(versions[len(versions)-1]), nil
	}
	if version > len(versions) {
		return schema.Artifact{}, fmt.Errorf("%w: %s@%d（当前最高版本 %d）",
			ErrArtifactNotFound, key, version, len(versions))
	}
	return cloneArtifact(versions[version-1]), nil
}

// Keys 返回黑板上出现过的全部 key，**字典序**排列。
//
// 排序是硬要求：map 遍历顺序随机，会让"同一份输入打印两次却不一样"，
// 于是 trace 无法 diff、实验无法复现（这个坑我们在 Report.Sorted 里已经讲过）。
func (b *Blackboard) Keys() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]string, 0, len(b.byKey))
	for k := range b.byKey {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Versions 返回某个 key 已有的版本数（不存在则为 0）。
//
// 它存在的意义：让"**追加而不是覆盖**"这条性质变成**可断言的**。
func (b *Blackboard) Versions(key string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.byKey[strings.TrimSpace(key)])
}

// ParseRef 把引用拆成 (key, version)。
//
// version == 0 表示"未指定版本，取最新"。
// 这个函数被导出，是为了让上层在**写引用之前**就能校验格式，
// 而不是等到 Get 失败才发现（错误要尽量靠近它的来源）。
func ParseRef(ref string) (string, int, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", 0, fmt.Errorf("%w: 引用为空", ErrBadRef)
	}

	key, verStr, hasVersion := strings.Cut(ref, "@")
	key = strings.TrimSpace(key)
	if key == "" {
		return "", 0, fmt.Errorf("%w: %q 缺少 key", ErrBadRef, ref)
	}
	if !hasVersion {
		return key, 0, nil
	}

	version, err := strconv.Atoi(strings.TrimSpace(verStr))
	if err != nil || version < 1 {
		return "", 0, fmt.Errorf("%w: %q 的版本号不合法（应为正整数）", ErrBadRef, ref)
	}
	return key, version, nil
}

// ─── 持久化：导出与还原 ─────────────────────────────────────────────
//
// 这两件事是 L2 的包注释里就预言过的：
//
//	"黑板是会演进的东西（L3 会加 checkpoint 持久化）"
//
// 现在兑现。**两者的形状完全对称**：导出一份 map，从一份 map 还原。
// 这个形状不是随便定的 —— 它让"持久化"变成一次纯粹的 `json.Marshal`，
// 不需要任何类型注册表（因为 Artifact.Payload 本来就是 json.RawMessage）。

// Snapshot 导出黑板的全量快照。
//
// ⚠️ **导出的是深拷贝**，与 `Get` 同一个规矩：
// 调用方拿到快照后顺手改一下，不应该影响黑板本身。
// （这个坑本项目踩过三次，见 cloneArtifact 的注释。）
func (b *Blackboard) Snapshot() map[string][]schema.Artifact {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make(map[string][]schema.Artifact, len(b.byKey))
	for k, versions := range b.byKey {
		cp := make([]schema.Artifact, 0, len(versions))
		for _, a := range versions {
			cp = append(cp, cloneArtifact(a))
		}
		out[k] = cp
	}
	return out
}

// LoadSnapshot 从一份快照还原一块黑板 —— **带完整性校验**。
//
// # 为什么必须校验，而不能只是 Unmarshal 成功
//
// 这是本轮精读得到的一条结论：**"解码成功" ≠ "数据可用"。**
//
//	企业仓库的 `DecodeMeta` / `DecodeMessages` 都是"JSON 解得开就返回"，
//	一个字段非法（甚至整个文档语义错乱）的对象会被原样放行。
//
// 对黑板来说，最危险的一类"能解开但不可用"是**版本序列有洞**：
//
//	`Get("outline@2")` 是靠 `versions[version-1]` 取值的。
//	快照里一旦出现空洞（[1,3]）或乱序（[2,1]），下标就会**取错版本** ——
//	而且它不会报错，它会**静默地返回另一份产物**。
//
//	一份被错取的产物比一份读不出来的产物危险得多：
//	后者会让任务失败，前者会让任务**成功地做错**。
//
// 所以这里的校验不是"防御性编程"，而是**把一类静默错误变成显式错误**。
func LoadSnapshot(snap map[string][]schema.Artifact) (*Blackboard, error) {
	b := NewBlackboard()

	for key, versions := range snap {
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("快照里有一个空 key")
		}
		if len(versions) == 0 {
			// 空的版本列表意味着"这个 key 存在但一版都没有"——
			// 它在语义上不该存在（不存在就该没有这个 key）。
			// 放行的话，`Get` 会走到"不存在"分支，而 `Keys()` 会把它列出来，
			// 于是**看板和取用者对同一份数据有两种说法**。
			return nil, fmt.Errorf("快照里 %s 的版本列表为空", key)
		}

		for i, a := range versions {
			// ① 信封与位置必须一致：map 的 key 就是产物的 key。
			//
			//	不一致意味着"我按 outline 取出来的其实是一份 design" ——
			//	这类错位在跨进程之后**没有任何一层**能发现。
			if a.Key != key {
				return nil, fmt.Errorf("快照里 %s 的第 %d 版信封上写的是 %s（与所在 key 不一致）",
					key, i+1, a.Key)
			}
			// ② 版本必须从 1 开始、逐 1 递增、与下标一致。
			if a.Version != i+1 {
				return nil, fmt.Errorf(
					"快照里 %s 的版本序列有洞或乱序：第 %d 个位置的版本号是 %d（应为 %d）",
					key, i+1, a.Version, i+1)
			}
			// ③ 每份产物自己也要过校验。
			if err := a.Validate(); err != nil {
				return nil, fmt.Errorf("快照里 %s 的第 %d 版不合法: %w", key, i+1, err)
			}
		}
		// 存**深拷贝**：快照之后被调用方改了，不该影响还原出来的黑板。
		cp := make([]schema.Artifact, 0, len(versions))
		for _, a := range versions {
			cp = append(cp, cloneArtifact(a))
		}
		b.byKey[key] = cp
	}
	return b, nil
}

// cloneArtifact 深拷贝一份产物。
//
// 为什么非拷不可？
//
//	Artifact.Payload 是 json.RawMessage（也就是 []byte）—— **引用类型**。
//	不拷贝的话，一个 Agent 从黑板取到产物、顺手改了 Payload[0]，
//	黑板里存的那一份就跟着变了，而其他 Agent 完全不知道自己拿到了脏数据。
//
//	这个坑在本项目里已经踩过三次（Report.Merged / render_slide / MergeLocal），
//	而黑板这里最危险 —— 它是唯一一处**真正的跨 Agent 共享可变状态**。
func cloneArtifact(a schema.Artifact) schema.Artifact {
	a.Payload = append([]byte(nil), a.Payload...)
	return a
}
