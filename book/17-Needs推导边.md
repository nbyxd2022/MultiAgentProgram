# 第 17 章 从"检查"到"保证"：`Needs` 推导边

系统能识别“不完整”了。到了 L3，问题变成“怎么让顺序从结构上成立”，这一章看 `Needs()` 怎么推导边。

## 17.1 L2 的顺序是"检查"出来的，L3 的是"保证"的

L2 里，Supervisor 是模型。它可能先调 `outline`，也可能先调 `research`。代码在 `SubAgentTool` 里检查依赖：

```text
前置产物未就绪：缺少 research。请先调用对应工具产出它们
```

模型被拒绝后，再重新选。顺序是"试出来"的。

L3 里，节点声明：

```go
outline.Needs() = []schema.Kind{schema.KindResearch}
```

`BuildGraph` 据此推导出一条边：

```text
research ──▶ outline
```

Runner 按拓扑序执行。顺序是"算出来"的，不是"试出来"的。

这就是第 1 章说的那句话：

> 同一件事，从"检查它有没有错"变成"它不可能错"。

## 17.2 Node 的五个声明

`internal/graph/graph.go:79`：

```go
type Node interface {
    Name() string
    Kind() schema.Kind
    Description() string
    Needs() []schema.Kind
    Run(ctx context.Context, in NodeInput) (*Outcome, error)
}
```

和 L2 的 `SubAgent` 几乎同形：Name / Kind / Description / Needs 逐字一致，只有 `Run` 的签名不同（它要能返回中断）。

`Kind` 仍然是三合一：产物种类、黑板 key、边的端点。

`Needs()` 是边的唯一来源。没有手写的边表，没有 `add_edge`。

## 17.3 边是推导出来的

`BuildGraph`（`graph.go:312`）做两件事：

1. 校验所有节点的声明；
2. 从 `Needs()` 生成边。

生成边的逻辑很简单（`graph.go:378`）：

```go
for _, name := range sortedNames(nodes) {
    for _, dep := range nodes[name].Needs() {
        if presets[dep] {
            continue // 预置输入不是节点，不产生节点之间的边
        }
        edges = append(edges, Edge{From: kindOwner(nodes, dep), To: name})
    }
}
```

比如：

```text
research.Needs() = [topic]              → topic 是预置输入，不产生边
outline.Needs() = [research]            → research ──▶ outline
designer.Needs() = [outline]            → outline ──▶ designer
renderer.Needs() = [outline, design]    → outline ──▶ renderer
                                           design  ──▶ renderer
```

为什么不让调用方手写边表？

- 加一个角色，只写它的 `Needs()`，图、前置检查、工具描述三处自动同步；
- 改依赖只改一处；手写边表时"改了 Needs 忘了改边"是静默的；
- 并行是白拿的：将来加一个只依赖 `outline` 的"配图"节点，它会自动和 `designer` 排进同一层。

## 17.4 拓扑排序：后序遍历 + 确定性

`topoSort`（`graph.go:617`）用三色标记：

- white：没访问；
- gray：在栈上；
- black：访问完。

遇到 gray 就是环，`cyclePath` 会打印出环上的节点：

```text
图里有环：a → b → c → a
```

为什么不只说"有环"？因为排查的人需要知道"哪几个节点互相咬住了"。

拓扑序用后序遍历：先访问依赖，再把当前节点加入 `order`。于是依赖一定排在使用者之前。

确定性靠两处排序：

- 起点按名字排序；
- 每个节点的依赖列表也排序。

map 的遍历顺序是随机的，不排序就无法 diff、无法做 golden 测试。这和全书反复出现的"输出必须稳定"是同一条。

## 17.5 默认调度：没有游标

`Graph.Next`（`graph.go:443`）：

```go
for _, name := range g.order {
    if !st.CanSkip(string(g.nodes[name].Kind())).Skip {
        return name
    }
}
return ""
```

规则只有一条：按拓扑序，找第一个产物还不能跳过的节点。

没有"当前节点"游标。为什么？

- "完成"是产物的属性，不是节点的属性；
- "接下来跑谁"可以从产物算出来；
- 恢复时不需要"从游标往后跑"，只需要再问一次这个问题。

这就是"黑板即状态"的完整含义：状态不是记在游标里，而是记在产物里。

## 17.6 NodeInput 与 Outcome

`NodeInput`（`graph.go:112`）包含：

- `Topic`：用户硬约束；
- `Hint`：这一跳的额外要求；
- `Inputs`：上游的 `{ref, summary}`；
- `RunID` / `Attempt`：用于派生确定性的中断 ID；
- `Answer`：人针对这一跳的回答（恢复时才有）；
- `board`（未导出）：只读的上游能力，`Fetch()` 取全文。

`Outcome`（`graph.go:204`）包含：

- `Product`：这一步的产物（nil 表示不产出）；
- `Next`：强制跳到某个节点；空表示按默认调度；
- `Interrupt`：在这里停下来等人。

三条边界：

1. 节点不写黑板：`Product` 由 Runner 统一入库，`BasedOn` 也由 Runner 填。节点只拿到 `{ref, summary}` 字符串，它不知道版本号。
2. **`Next == 自己` 是合法的**：表示"这一跳重做"。它只表达"要不要重试"，不表达"最多重试几次"——有界性由 Runner 保证。
3. **`Next` 跳到别的节点，必须有边**：否则 `CheckOutcome` 拒绝。

`CheckOutcome`（`graph.go:459`）的错误信息直接给出修法：

```text
要允许它，请让 next.Needs() 里包含 from 的 Kind（边会自动出现）；
或者在节点内部把这次跳转改成重做自己
```

这是一个很好的设计：**修法就是改声明，不用去另一个地方再改一条边。**

## 17.7 Layers：候选，不是并行

`Layers()`（`graph.go:512`）按依赖深度分层：

```text
第 0 层：research
第 1 层：outline
第 2 层：designer
第 3 层：renderer
```

同一层内的节点互不依赖，理论上可以并行。

但要注意：**`Runner` 目前是串行的。** `Layers()` 的唯一生产调用者是 `Describe()`（打印拓扑）。

源码里专门写了一句警告（`graph.go:499`）：

> 一个看起来"已经准备好了"的接口，会让人以为只差一个开关。那是谎言。

并行要付的账单：有序交付、失败隔离、合并语义、早渲染的协调。这些都没做。所以 `Layers()` 只是候选。

这个态度值得学习：不要把一个"候选能力"包装成"已支持"。

## 17.8 为什么不加条件边

设计文档里曾经有一个 `Edge.When func(*State) bool`（图级条件边）。实现时被否决了。

理由：那会立刻制造两套条件机制：

- `Edge.When`：图级的条件；
- `Outcome.Next`：节点级的跳转。

而"两套并存"正是全书一直在批评的病。

判据是：**L3 里真的有"不属于任何节点"的条件判断吗？**

- "素材不够就再搜一轮" → 是 research 自己的判断 → `Outcome.Next`；
- "主题改了要重做" → 由 `CanSkip` / `StaleReasons` 自动处理，不需要条件。

答案是"没有"。所以不加。

> 为用不到的场景预留机制，就是提前制造两套并存。

## 17.9 练习

先自己做，再看 `answers/17-参考答案.md`。

### 练习 1：加一个节点

给图加一个 `fact_check` 节点：

- `Needs() = [research, outline]`；
- `Kind() = "fact_check"`；
- 写一个最简单的 Run（返回一个 payload）。

观察：

- `Edges()` 里多了哪几条边？
- `Order()` 里它排在哪？
- `Layers()` 里它和谁同一层？

### 练习 2：删掉一个生产者

把 `outline` 节点从图里删掉，但保留 `designer`（它 `Needs` outline）。

观察 `BuildGraph` 的错误。回答：

- 它在建图期就拒绝，还是运行到 designer 才失败？
- 这条错误信息能不能直接告诉你"该加哪个节点"？

### 练习 3：制造一个环

让 A `Needs` B，B `Needs` C，C `Needs` A。

观察：

- 错误信息里有没有环的完整路径？
- 如果没有 `cyclePath`，你排查起来会多难？

### 练习 4：两个节点产出同一个 Kind

加一个 `outline_v2` 节点，`Kind()` 也返回 `outline`。

观察错误。回答：

- 为什么这会破坏"版本"语义？
- 如果放行，`Get("outline@2")` 会取到谁的产物？

### 练习 5：非法的 Next

让一个节点返回 `Next = "render"`，但图里没有 `outline → render` 的边。

观察 `CheckOutcome` 的错误。然后按它的提示，把 `outline` 加进 `render.Needs()`，再跑一次。

回答：为什么"改 Needs"比"改边"更好？

## 17.10 小结与自检
合上书，回答：

- L2 的顺序和 L3 的顺序，分别是怎么来的？
- 边为什么由 `Needs()` 推导，而不是手写？
- `Graph.Next` 为什么不需要游标？
- `Outcome.Next` 的两种合法情况是什么？
- `CheckOutcome` 只校验什么，不校验什么？
- `Layers()` 为什么只是候选，不是并行？
- 为什么没有 `Edge.When`？

能答出五题以上，再进下一章。

## 17.10 拓展：删掉一条 Needs

把 `render.Needs()` 里的 `outline` 删掉，只留 `design`。图还能建出来吗？如果建得出来，运行时会在哪里出问题？

这个练习想让你看到：`Needs` 少了不是“检查不严”，而是图本身错了。后面 `BuildGraph` 能挡住一部分，但挡不住“语义上需要、声明里没写”的依赖。

## 17.11 下一章

边和调度有了，但坏图还没挡。下一章看五种必须在建图期拒绝的形态，以及每条如果不拦，会在运行期变成什么怪事。
