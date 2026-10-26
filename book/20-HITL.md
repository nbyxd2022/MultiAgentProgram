# 第 20 章 HITL：先产出，再问人

执行循环里，中断只是一个返回值。可一旦要跨进程，它就得变成协议。这一章拆 HITL。

## 20.1 两种"问人"

HITL（Human-in-the-loop）有两种形态：

1. 先产出，再问："我做完了，你看看再往下。"（`confirm_artifact`）
2. 先问，再产出："我需要一个信息，不然做不下去。"（`ask_user` / `choose_option`）

L3 只实现了第一种。

`SubAgentNode` 的注释写得很清楚：

> L3 只做"先产出、再问"这一种中断。`ask_user` 在协议层是支持的，但没有节点用它——所以适配器不为它预留开关。

为什么？因为"为用不到的场景预留机制，就是提前制造两套并存"。

第一种已经覆盖了 L3 的核心场景：大纲生成后，人确认/修改/放弃。

## 20.2 先产出再问，为什么能"自然继续"

看这个组合：

```go
out := &graph.Outcome{
    Product:   product,
    Interrupt: &it,
}
```

产物已经写进黑板，然后才停下来问人。

人点"确认"之后会发生什么？

- `CanSkip(outline)` 发现产物存在、完整、新鲜 → 跳过 `outline`；
- 调度自然往下走。

"确认"不需要任何特殊逻辑——它只是"再读一次状态"，发现这一跳已经做完了。

反过来，如果是"先问再产出"：

- 中断时没有产物；
- 人回答后，节点必须重新执行，从 `Answer` 里拿到信息；
- 这要求节点理解 `Answer`，也要求"没有答案就不能继续"。

两种语义不同，仓库只实现了第一种。

## 20.3 中断协议的字段

`Interrupt`（`interrupt.go:255`）：

```go
type Interrupt struct {
    Version   int
    ID        string
    RunID     string
    Node      string
    Attempt   int
    Kind      InterruptKind
    Prompt    string
    Options   []string
    Payload   json.RawMessage
    CreatedAt time.Time
    ExpiresAt time.Time
    Decision  *Decision
}
```

逐个说：

| 字段 | 作用 | 为什么不能省 |
|---|---|---|
| `Version` | 协议版本 | 记录可能被几天后、另一个版本的代码读回来 |
| `ID` | 唯一标识 | 恢复时靠它把答案对回来 |
| `RunID` | 哪一轮 | 防止 A 会话的回答串到 B 会话 |
| `Node` | 哪一跳 | 恢复时从这里重进 |
| `Attempt` | 第几次执行 | 同一节点可能中断多次，ID 要区分 |
| `Kind` | 中断类型 | 决定允许哪些意图、要不要选项/备注 |
| `Prompt` | 问人的问题 | 没有问题的中断，人无法回答 |
| `Options` | 选项 | `choose_option` 必须有至少 2 个 |
| `Payload` | 业务上下文 | 例如"待确认的大纲"；store 不解释它 |
| `CreatedAt` | 创建时间 | 审计 |
| `ExpiresAt` | 业务过期 | 超过它就别恢复了 |
| `Decision` | 人的回答 | **已回答但未消费**的状态 |

`Payload` 是 `json.RawMessage`，不是 `any`。为什么？

因为 `any` 要序列化就必须记住"每个 key 对应哪个具体类型"，那就需要类型注册表或 switch 分派——而 store 只需要存字节、读回来还是字节。分派的责任交给真正需要解它的那一方。

## 20.4 中断类型与意图

三种类型（`interrupt.go:66`）：

| Kind | 用途 | 允许的意图 | 额外要求 |
|---|---|---|---|
| `confirm_artifact` | 确认一份产物 | confirm / revise / abort | 无 |
| `ask_user` | 追问自由文本 | confirm / abort | `Note` 必填 |
| `choose_option` | 从选项里挑 | confirm / abort | 至少 2 个选项，`Note` 必填 |

三种意图（`interrupt.go:199`）：

- confirm：就这样，继续；
- revise：重做，`Note` 说明改什么；
- abort：不做了，带着已完成部分收尾。

几个不变量：

1. 每种 Kind 都必须允许 abort：一个只能"继续"的 HITL 不是 HITL，是一个必须无条件服从的弹窗；
2. **`revise` 必须带 Note**：没有 Note，代码不知道该重做什么，不可执行；
3. **`ask_user` / `choose_option` 的 Note 必填**：它们问的就是"一个信息"，空回答等于没回答，节点只能再问一遍——跨越多次恢复的死循环。

这些都是关于"能不能继续跑"的约束，不是文风要求。

## 20.5 确定性的中断 ID

`NewInterrupt`（`interrupt.go:344`）：

```go
ID = "<runID>:<node>:<attempt>"
```

为什么不用随机 UUID？

因为中断会被写进 checkpoint 再读回来。如果 ID 随机，同一份状态重跑会生成不同的 ID，之前存下的答案就对不上了。

`attempt` 从哪来？

```go
func (s *State) AttemptFor(k schema.Kind) int {
    return s.Versions(string(k)) + 1
}
```

它从黑板推导，不是运行期计数器。

为什么？

- 中断时没有产出 → 版本数不变 → attempt 不变 → ID 不变 → 答案对得上；
- 真正重做过一次（版本 +1）之后 → attempt 递增 → 新问题有新 ID，不会串台。

如果 attempt 是计数器，恢复后节点重跑一次，计数器 +1，派生出一个新 ID，人上次的答案永远对不上——系统会一遍又一遍重新提问。

## 20.6 Decision 必须先落盘

`Resume` 的顺序（`runner.go:347`）：

```text
① 记录还能不能读（版本 → 结构 → 过期）
② 回答合不合法
③ 先把回答落盘，再开始跑
```

第 ③ 步最关键。

如果只在内存里记答案，然后跑到一半崩了：

- 内存里的答案没了；
- 磁盘上还是"未回答"；
- 下次恢复会重新问一遍——用户以为已经回答过了。

先落盘之后，磁盘上是"已回答未消费"，下次 `Run` 会自动重放它。

> **"重放"不需要任何额外机制——它只是"再读一次状态"。**

## 20.7 三种意图的执行

`planFrom`（`runner.go:722`）从状态里读计划：

### confirm

什么都不做。

因为中断的那一跳已经把产物投进黑板了，`CanSkip` 会跳过它，调度自然往下走。

这依赖一个不变量：**能被 confirm 的中断，必须已经产出。**

### revise

强制重做中断的那一跳，并把 `Note` 交给它。

只重跑不给答案 = 假动作：它会用同样的输入产出同样的东西。

重做之后，新产物版本 +1，`StaleReasons` 会让下游自动过期——这是 L2 血缘在 L3 的第一次真实用途。

### abort

停止推进，交付已有部分。

`StatusAborted` 与 `StatusBudgetStopped` 必须分开：前者是人说"不做了"，后者是系统的上限到了。把后者报成前者，用户会以为自己按错了什么。

## 20.8 过期：业务时间 vs 存储时间

`ExpiresAt` 是业务有效期，默认 7 天。

它和存储层的 TTL 不是一回事：

- 业务过期：超过它就别恢复了（"这个中断已经没意义了"）；
- 存储 TTL：垃圾回收，必须远大于业务有效期。

为什么要分开？因为企业仓库用 Redis 的 7 天 TTL 同时承担两者，后果是过期是静默的：`Get` 返回"不存在"，调用方无法区分"key 写错"、"被 GC"、"业务过期"。

仓库把业务过期写进数据，于是 `CheckResumable` 能报 `ResumeExpired`，而不是 `ResumeNotFound`。

`CheckResumable` 的顺序也不能换：

```text
① 版本：我读得懂它吗？
② 结构：它自己写得对吗？
③ 过期：它在业务上还有意义吗？
```

用本版本的规则去校验一个来自未来版本的记录，会得出"缺少 prompt"这种误导性结论。先问"读得懂吗"，再问"写得对吗"。

## 20.9 HITL 的失败模式

### 失败一：confirm 没有产物

如果节点只返回中断、没有 Product，人点 confirm 后：

- `CanSkip` 不会跳过（产物不存在）；
- 节点会再跑一遍；
- 但它没有拿到任何新信息（confirm 的 Note 可以为空）；
- 于是可能再中断、再 confirm——死循环。

所以 L3 的 `confirm_artifact` 必须和 Product 一起返回。

### 失败二：随机 ID

答案对不上，系统反复提问。修法：确定性派生。

### 失败三：答案只在内存

崩溃后丢失，用户被重复提问。修法：先落盘。

### 失败四：不支持 abort

用户被卡住，只能强杀进程。修法：每种 Kind 都必须允许 abort。

### 失败五：把过期报成不存在

用户以为记录丢了，实际只是过期。修法：业务过期进数据，错误分类。

## 20.10 对照 Eino / LangGraph

LangGraph 有 `interrupt` 原语，Eino 也有 HITL 相关能力。它们通常提供：

- 在节点里暂停；
- 把状态存到 checkpointer；
- 恢复时把人的回答传回去。

但有几件事仍然要你自己决定：

- 中断 ID 怎么派生（确定性 vs 随机）；
- 答案先落盘还是先执行；
- 哪些意图允许、哪些必须带说明；
- 业务过期与存储 TTL 怎么分开；
- 恢复失败怎么分类。

这些不是框架能替你回答的，因为它们是你的业务语义。

## 20.11 练习

先自己做，再看 `answers/20-参考答案.md`。

### 练习 1：跑一次完整的中断

```powershell
go run ./cmd/l3
```

预期：跑到 `outline` 后停下，打印问题，进程退出。

观察 `out/l3/ckpt/` 下的文件：

- `Pending` 里有哪些字段？
- `Decision` 是 nil 吗？
- `Attempt` 是多少？

### 练习 2：确认后继续

```powershell
go run ./cmd/l3 -answer=confirm
```

预期：新进程从磁盘恢复，跳过已完成节点，继续到完成。

回答：

- 它有没有重新执行 `outline`？
- `Ran` 列表里有哪些节点？
- 为什么 confirm 不需要任何额外逻辑？

### 练习 3：修改后重做

先清掉 checkpoint（只在想重来时做）：

```powershell
Remove-Item -Recurse -Force out/l3/ckpt
```

然后：

```powershell
go run ./cmd/l3
go run ./cmd/l3 -answer=revise -note="第二页改成讲成本"
```

观察：

- `outline` 有没有被强制重做？
- 版本号有没有 +1？
- 下游有没有因为 `StaleReasons` 被重做？

### 练习 4：放弃

```powershell
go run ./cmd/l3 -answer=abort
```

观察：

- 状态是什么？
- 已完成的产物还在吗？
- 交付说明有没有说"这是降级交付"？

### 练习 5：把 attempt 改成计数器

假设你把 `State.AttemptFor` 改成一个运行期计数器。

回答：

- 恢复后第一次重跑，中断 ID 会变成什么？
- 人上次的答案还能对上吗？
- 系统会表现成什么现象？

## 20.12 小结与自检
合上书，回答：

- L3 只实现了哪一种中断？为什么？
- "先产出再问"为什么能让 confirm 自然继续？
- 中断 ID 为什么必须确定性派生？attempt 从哪来？
- Decision 为什么必须先落盘？
- confirm / revise / abort 分别发生什么？
- 业务过期和存储 TTL 为什么要分开？
- HITL 的五种失败模式是什么？

能答出五题以上，再进下一章。

## 20.12 拓展：把 attempt 改成计数器

把 `State.AttemptFor` 改成一个运行期计数器，再跑一次中断恢复。观察中断 ID 会变成什么，人上次的回答还能不能对上。

如果对不上，系统会再次提问；用户会觉得“我明明回答过了”。这个实验比读十遍注释更能说明“确定性 ID”为什么重要。

## 20.13 下一章

中断能存、能回答，但恢复还没讲透。下一章专门看 decision 重放、幂等，以及为什么恢复是 at-least-once。
