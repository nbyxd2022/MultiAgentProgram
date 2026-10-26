// Package checkpoint 负责一件事：**把一份状态存到某个地方，并且能按原样读回来**。
//
// # 它只搬字节，不解释内容
//
// 接口里出现的是 `[]byte`，不是 `*graph.State`。这个选择有三条理由，
// 一条比一条硬：
//
//	① **避免 import 环**。`graph.Runner` 要用 Store 保存状态 ——
//	   如果 Store 反过来依赖 `graph.State`，两个包就互相引用了。
//
//	② **判据**（06-L3设计.md §3.3）：**谁来解它。**
//	   框架自己解的东西 → 存不透明字节，store 不该知道格式。
//	   这里 store 是"存储"，解状态的是"图" —— 分工很清楚。
//
//	③ **对照企业仓库**：它的 `compose.CheckPointStore` 是
//	   `Set(ctx, key string, value []byte)` —— 一模一样的分工。
//	   而它自己的 `interruptstore`（业务要解的）就必须背类型名与分派。
//	   **两个 store 的分界线就是"谁来解它"。**
//
// 代价：调用方必须自己序列化。收益：换存储（文件 → Redis）**不用动状态一行**，
// 而改状态格式（加字段）也**不用动存储一行**。
//
// # 我们与企业仓库的一个具体差异：原子性
//
// 它有三个后端，**每一个都不需要"临时文件 + rename"**：
//
//	Redis     → `MULTI/EXEC` 事务（`memorystore/redis.go` 注释明写 atomically）；
//	            或 Lua 脚本做 CAS（`a2ataskstore` 的 prevVersion）
//	KS3       → `UploadByKey` 是 PUT 整个对象，天然原子
//	inmem     → 内存里的 map 赋值
//
// 所以 `checkpointstore/` 目录下**只有 inmem / redis / ks3 三个文件，没有 file.go**。
//
// **不是它没想到原子写** —— 是它的后端都不需要。而我们是文件后端，
// 所以这件事得自己实现（见 `file.go`）。**这是"选了什么后端"直接带来的工作量。**
package checkpoint

// Store 是一份"按 run id 存取字节"的持久化。
//
// 三个方法的语义边界要划清楚（这是接口设计里最容易被含糊掉的部分）：
//
//	Save  —— 失败就是失败，必须上报
//	Load  —— **"没有" 与 "出错" 必须分开**（第二个返回值就是干这个的）
//	List  —— 只列出**完好**的记录
type Store interface {
	// Save 保存一份完整的状态。**必须原子**：读的人要么看到旧的完整版，
	// 要么看到新的完整版，**永远不会看到半截**。
	Save(runID string, raw []byte) error

	// Load 读回一份状态。
	//
	// 返回 (raw, found, err)：
	//
	//	found=false, err=nil  表示"还没存过" —— 这是**正常情况**（第一次跑）
	//	err != nil            表示**读取出错** —— 这要上报
	//
	// ⚠️ 把两者合成一个 error 是很常见的偷懒，后果是
	// **"第一次跑"看起来像故障** —— 排查的人会去查磁盘，而磁盘是好的。
	Load(runID string) (raw []byte, found bool, err error)

	// List 返回全部**完好**的 run id（字典序）。
	//
	// "完好"这个限定词是有分量的：一次"写到一半就被杀"会留下垃圾，
	// 而垃圾**不能**被列成一个可恢复的 run（见 file.go 的实现说明）。
	List() ([]string, error)
}

// 关于"要不要加 Delete、要不要加乐观并发（prevVersion）"的两个决定：
//
//	**L3 都不加。**
//
// 这与 `Edge.When` 是同一个判断：**为用不到的场景预留机制，就是提前制造两套并存。**
//
//	Delete      —— L3 的每一轮都留着自己的 checkpoint（那是"可恢复"的凭据）。
//	              "什么时候可以清理"是一个**运维策略**问题，现在没有输入条件。
//	prevVersion —— 它是**乐观并发控制**（企业仓库 `a2ataskstore` 的第三种并发解法，
//	              与 `SessionLocker` 的悲观锁、`MULTI/EXEC` 的事务并列）。
//	              而 L3 是单进程 CLI，**没有第二个执行者**。
//
// ⚠️ 但两者的**触发条件是明确的**，写在这里让后来的人能区分
// "想过了用不上" 与 "没想到"（R28 §6.4）：
//
//	· Delete      → 当 checkpoint 的总体积成为一个问题时（那时要先定保留策略：
//	                保留最近 N 轮？保留每个 run 的最后一版？）
//	· prevVersion → **当出现第二个执行者时**（多实例部署 / 后台任务与前台请求并存）。
//	                它与 `SessionLocker` 解决的是同一个问题：谁有权威改这份状态。
//	                区别是悲观（先锁）还是乐观（写完发现冲突再重试）。
