# 第 13 章 SubAgent 即工具：依赖检查、熔断、新鲜度

编排方式选定了，Supervisor 要调用的角色还不是工具。角色有自己的依赖、失败、产物，适配器要把这些接住。

## 13.1 一个"模型很努力，但一直在做错事"的场景

Supervisor 拿到任务，决定先调 `outline`。

但 `outline` 需要 `research` 的产物，而 `research` 还没跑。

如果没有代码检查，会发生什么？

- `outline` 拿到空的上游，开始编内容；
- 它可能编得很像真的；
- 最后交付一份没有事实依据的大纲。

或者，真实运行里的另一种场景（L2-9）：

`outline` 因为单次调用超时，每一步都失败。Supervisor（真实模型）换了 10 种措辞反复重试它，直到 `MaxSteps` 强制终止。烧掉 **3056 prompt + 7364 completion**，整轮 42906 + 12415 tokens，任务却没完成——而 Mock 跑完全套只要 4964 tokens。

重试没有改变根因，只放大了成本。

这一章的四道检查，就是为这两类场景准备的。

## 13.2 SubAgent 的五个声明

`internal/agents/subagent.go:38`：

```go
type SubAgent interface {
    Name() string
    Kind() schema.Kind
    Description() string
    Needs() []schema.Kind
    Run(ctx context.Context, in SubAgentInput) (schema.Payload, error)
}
```

五个方法，正好回答五个问题：

| 方法 | 回答 |
|---|---|
| `Name` | 我是谁（写进 `produced_by`） |
| `Kind` | 我产出什么（同时是工具名、黑板 key） |
| `Description` | 模型什么时候该调我 |
| `Needs` | 我依赖哪些上游产物 |
| `Run` | 怎么执行 |

`Kind` 同时承担三个身份：产物种类、工具名、黑板 key。

为什么合并？因为三者本来就该一致：

- 如果工具名和 Kind 不同，Supervisor 调 `outline`，产物却存成 `design`；
- 如果黑板的 key 和 Kind 不同，下游按 `Needs` 去取 `outline`，会取到别的东西。

合并之后，"路由的合法值集合 = 工具名集合 = 黑板 key 集合"，一层校验管三件事。

## 13.3 适配器的三件事

`subAgentTool`（`subagent.go:183`）把 SubAgent 变成 `tool.Tool`。它做三件事：

1. 依赖检查：上游没就绪，调用直接拒绝；
2. 自动装配输入：上游产物按 `Needs` 顺序放进 `SubAgentInput`；
3. 产物入库：写黑板，只回传 `{ref, summary}`。

`Execute` 的顺序非常重要（`subagent.go:258`）：

```text
熔断检查
  → 依赖检查（有没有）
  → 取一次上游产物
  → 新鲜度检查（还对不对）
  → 装配输入（只给 ref + summary）
  → 执行 SubAgent
  → Kind 一致性校验
  → NewArtifact（填 BasedOn）
  → 写黑板
  → 返回 ref + summary
```

每一步的位置都不是随便放的，下面逐个说。

## 13.4 依赖检查：顺序由代码强制

`Definition` 里会把前置条件写进工具描述（`subagent.go:217`）：

```text
【前置条件】必须先产出 research、outline；未就绪时调用会被拒绝。
```

但这只是给模型看的。真正的检查在 `Execute`：

```go
missing := t.missingNeeds()
if len(missing) > 0 {
    return "", fmt.Errorf("前置产物未就绪：缺少 %s。请先调用对应工具产出它们", ...)
}
```

`missingNeeds` 用 `board.Versions(key) == 0` 判断。

这条检查治的是 L2 的一个核心问题：顺序约束不能写在 Prompt 里。Prompt 是建议，模型可以不遵守；代码检查是保证。

拒绝信息必须是可执行的：它要告诉模型"缺什么、该先做什么"，而不是只说"不行"。

> 你可以在 `cmd/l2` 里用 `$env:L2_SKIP_ORDER="1"` 做故障注入，看模型试图跳过前置角色时被怎样拒绝。

## 13.5 新鲜度检查：有，不等于对

依赖检查只回答"有没有"。新鲜度检查回答"还对不对"。

看这个场景：

```text
outline@1 ──> design@1
outline 重跑 ──> outline@2
```

现在 `design@1` 存在、完整、类型正确。但它是基于 `outline@1` 做的，而 `outline` 已经到 `@2` 了。

如果直接把它发给下游，会得到"新内容配旧版式"——而且不会报错。

所以 `staleInputs`（`subagent.go:418`）在装配输入之前检查：

```go
if stale := t.staleInputs(inputs); len(stale) > 0 {
    return "", fmt.Errorf("前置产物已过期（依据链被上游的新版本打断）：...")
}
```

为什么必须在装配之前？因为一旦发下去，子 Agent 会基于过期的假设做决定，而它产出的东西看起来完全正常。

`staleInputs` 还做了跨输入的去重：同一条依据可能被多个输入共享，不去重的话模型会看到同一句话两遍。

## 13.6 熔断：连续失败三次，换条路

`maxConsecutiveFailures = 3`（`subagent.go:181`）。

计数器挂在 `subAgentTool` 实例上。这个实例在一次编排里是共享的，所以计数是跨轮累计的：Supervisor 换措辞重试，计数照样会涨。

规则：

- 失败一次，`failures++`；
- 成功一次，`failures = 0`（它是"连续"计数，不是"累计"）；
- 连续失败 3 次后，第 4 次调用在最前面就被拒绝。

拒绝信息不是"不行"，而是给出三条出路（`subagent.go:269`）：

```text
请换一条路：换别的角色、换策略，或者直接结束并说明卡在哪里。
```

为什么熔断要放在依赖检查之前？因为它是最强的信号：换上游、换参数都救不了它，只有换路才行。

这条检查治的是 L2-9 的真实事故：模型不知道"这个角色已经失败了三次"，于是不断换措辞重试。把连续失败次数告诉模型，它才有机会做出不同决策。

## 13.7 自动装配与血缘

`collectInputs`（`subagent.go:395`）按 `Needs` 顺序取最新版上游产物。注意：只取一次。

为什么只取一次？因为如果分别取三次，三次之间理论上可以插入新的写入，于是"检查的"和"用的"就不是同一份了。

取到的是 `Artifact`，不是 `Handoff`，因为它要做两件事：

1. 生成 `Handoff{Ref, Summary}` 发给子 Agent；
2. 生成 `Basis{Key, Version}` 记进产物的 `BasedOn`。

`BasedOn` 由适配器填，不是子 Agent 自己填（`subagent.go:344`）：

- 子 Agent 只拿到 `Handoff`，里面是字符串 ref，它不知道版本结构；
- 适配器同时握着 Artifact 和版本，填它是一行的事。

这和"版本由黑板分配"是同一条原则：谁拥有这个信息，谁负责维护它。

`SubAgentInput.Upstream` 的类型是 `schema.Reader`，不是 `*state.Blackboard`（`subagent.go:106`）：

- 子 Agent 只需要"取"，不该拿到"写"；
- 把不该有的能力从类型上拿掉，比写一句"请不要这样做"可靠。

需要全文时，调用 `Fetch()` 显式取（`subagent.go:123`）。为什么不一上来就把全文放进 `Inputs`？因为大多数角色不需要全文；把取全文变成显式动作，才能看出谁真的需要它。

## 13.8 只给 hint，不给数据字段

工具参数只有一个 `hint`（`subagent.go:245`）：

```go
type subAgentArgs struct {
    Hint string `json:"hint"`
}
```

没有任何"上游数据"字段。 上游产物由代码按 `Needs` 自动装配——编排者即使想搬数据也无处可搬。

这是"防止 Supervisor 做逐字段提取重组"的物理隔离：不是靠 Prompt 说"不要搬"，而是没有地方可搬。

## 13.9 预算要告诉模型

`BudgetNotice`（`subagent.go:455`）：

```text
【预算】你最多只能调用 4 次工具；超出会被强制终止，且本次全部工作作废。
```

为什么必须写出来？因为 L2-9 的真实运行里，`research` 的 `MaxSteps` 是 6，而模型完全不知道这个数，于是一直搜到第 6 步被强制终止，已经搜到的素材全部丢弃。

> **一个不告诉模型的预算，等于没有预算。**

数字由调用方传入，改预算只改一处，Prompt 自动跟着变——这是"约束单一来源"的又一次应用。

## 13.10 Stats 的口径：累计，不是最近一次

`StatsReporter` 是可选能力（`subagent.go:83`），契约很明确：返回本角色实例的累计用量（跨多次 Run 累加）。

L2-3 的 bug 是：子 Agent 用 `lastStats = a.Stats()`（覆盖），编排者用 `total = addStats(total, ...)`（累加）。同一张成本表两行用了两种口径。

后果：某个角色跑了两遍时，它的 token 只算最后一遍——返工最多、成本最高的那一次，反而算得最少。

修复后统一为累计。更深一层的教训是：**统计挂在实例上，就必然要回答"跨 Run 是覆盖还是累加"。** 更稳的形状是让 `Run` 把 stats 作为返回值带出来——没有共享可变字段，就没有这个问题。但那要等并发压力出现。

## 13.11 这些检查在 L3 为什么不需要了

到了 L3，节点由 `Needs()` 推导边，Runner 按拓扑序执行：

- 依赖检查 → 不需要，顺序由代码保证；
- 新鲜度检查 → 仍然需要（`CanSkip` 会检查 `StaleReasons`）；
- 熔断 → 仍然需要（节点连续失败上限）；
- Kind 一致性 → 仍然需要（`CheckOutcome`）。

所以不是"全部消失"，而是**从"每次调用前检查"变成"结构上不可能违反"**。这是 L3 相对 L2 最大的简化。

## 13.12 练习

先自己做，再看 `answers/13-参考答案.md`。

### 练习 1：跳过前置角色

在 `cmd/l2` 里用 `$env:L2_SKIP_ORDER="1"` 跑一次，观察：

- 被拒绝的是哪个工具？
- 错误信息里列出了哪些缺失的产物？
- 模型接下来做了什么？

### 练习 2：触发熔断

写一个总是失败的 `SubAgent`，让 Supervisor 连续调用它 4 次。观察：

- 前 3 次的错误里有没有"连续第 N 次"？
- 第 4 次是不是在依赖检查之前就被拒绝？
- 拒绝信息给了哪三条出路？

### 练习 3：制造过期上游

1. 让 `research`、`outline`、`design` 各产出一版；
2. 重跑 `outline`，产出 `outline@2`；
3. 再调用 `renderer`。

观察：

- `renderer` 的前置检查报了什么？
- 错误里的修复顺序是先 `design` 还是先 `render`？

### 练习 4：为什么参数只有 hint

把 `subAgentArgs` 改成：

```go
type subAgentArgs struct {
    Hint   string `json:"hint"`
    Inputs string `json:"inputs"`
}
```

然后让 Supervisor 试图把上游 JSON 塞进 `inputs`。

回答：

- 这样做会带来什么风险？
- 为什么"没有字段可搬"比"Prompt 说不要搬"更可靠？

### 练习 5：统计口径

让同一个角色连续跑两次，第一次失败、第二次成功。观察 `Stats()`：

- 如果实现是"最近一次"，两次的总量会怎么算？
- 如果是"累计"，两次的总量会怎么算？
- 哪个更符合"成本核算"？

## 13.13 小结与自检
合上书，回答：

- SubAgent 的五个方法分别回答什么？
- `Kind` 为什么同时是工具名和黑板 key？
- 适配器执行的四道检查是什么？顺序为什么重要？
- 熔断解决的是什么问题？为什么放在依赖检查之前？
- 为什么 `BasedOn` 由适配器填，而不是子 Agent 填？
- 为什么 `SubAgentInput.Upstream` 是只读接口？
- L3 里哪些检查消失了，哪些仍然存在？

能答出五题以上，再进下一章。

## 13.13 拓展：把熔断阈值改成 1

把 `maxConsecutiveFailures` 改成 1，再跑一次总是失败的子 Agent。观察模型拿到“已熔断”之后，会不会换一条路。

这个实验能让你体会：熔断不是惩罚，而是给模型一个明确的信号——“别再换措辞了，问题不在措辞”。

## 13.14 下一章

适配器把角色变成了工具，但产物之间的依赖还只是一堆 `{ref, summary}`。下一章看黑板本身：为什么只传引用、不传全文，以及共享可变状态为什么容易出静默错误。
