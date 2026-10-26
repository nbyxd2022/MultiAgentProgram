# 第 2 章 Agent 的第一性原理：一次调用、一个工具、一个循环

上一章反复说“模型只负责下一步”。这句话落到一次 HTTP 请求上，到底是什么意思？这一章只看模型收到的 JSON 和返回的 JSON。

## 2.1 一个失败场景

你写了一个小程序，调用模型：

```text
用户：北京今天天气怎么样？
模型：我无法获取实时天气信息。
```

你决定给它一个工具。你听说这叫 function calling，于是在请求里加了 `get_weather` 的定义，重新问了一次。

模型这次返回了这样的东西：

```json
{
  "role": "assistant",
  "content": null,
  "tool_calls": [
    {
      "id": "call_abc123",
      "type": "function",
      "function": {
        "name": "get_weather",
        "arguments": "{\"city\":\"北京\"}"
      }
    }
  ]
}
```

而你的代码只读了 `content`。你看到 `null`，以为模型坏了。

模型没坏。它做了它该做的事：告诉你"我想调用这个工具，参数是这些"。它没有执行工具，也不会执行工具。执行是你的事。

这就是本章要建立的第一条认知：**模型只负责"决定下一步"，运行时负责"真的去做"。**

## 2.2 把模型还原成一个函数

抛开所有术语，模型就是一个无状态的函数：

```text
下一条消息 = model(消息列表, 工具列表)
```

它有四个"没有"：

| 它没有 | 后果 |
|---|---|
| 没有记忆 | 每次调用你都要把完整历史重新发过去 |
| 没有工具 | 你能用的工具，必须写进请求的 `tools` 字段 |
| 没有循环 | "调用工具 → 拿结果 → 再问一次"是你写的 |
| 没有副作用 | 它只返回文本或工具调用请求，不会读文件、不会发请求 |

理解这四个"没有"，后面很多设计就不用背了：

- 为什么上下文会越来越长？ 因为历史每次都重发，模型不会帮你记。
- 为什么工具描述写得不好，模型就不会用？ 因为它只能看到你写的那段描述。
- 为什么需要最大步数？ 因为循环是你写的，模型没有"我该停了"的硬保证。
- 为什么工具执行必须由你的代码控制？ 因为模型只能"请求"，权限在运行时手里。

> **一个常见误解**：以为模型"知道"当前时间、天气、数据库里的数据。它不知道。它只知道这次请求里写了什么。

## 2.3 一次真实的请求长什么样

下面是一个最小可用的请求体。你可以先不逐字读懂，先看结构：

```json
{
  "model": "gpt-4o-mini",
  "messages": [
    {
      "role": "system",
      "content": "你是一个简洁的助手。"
    },
    {
      "role": "user",
      "content": "北京今天天气怎么样？"
    }
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "get_weather",
        "description": "查询某个城市当前的天气。",
        "parameters": {
          "type": "object",
          "properties": {
            "city": {
              "type": "string",
              "description": "城市名，例如：北京"
            }
          },
          "required": ["city"]
        }
      }
    }
  ]
}
```

逐个字段解释：

| 字段 | 作用 | 初学者常犯的错 |
|---|---|---|
| `model` | 用哪个模型 | 以为换模型只影响"聪明程度"，其实还影响价格、速度、上下文长度 |
| `messages` | 这次对话的完整历史 | 以为模型记得上一轮；其实每次都要重发 |
| `tools` | 模型可以选择的工具清单 | 忘了把工具定义放进来，然后怪模型不会用 |
| `tool_choice` | 是否强制调用工具 | 不写就是 `auto`，模型自己决定；本实验不写 |

`tools` 里的 `parameters` 是一段 JSON Schema。你不需要成为 JSON Schema 专家，先记住三件事：

1. `type: "object"` 表示参数是一个对象。
2. `properties` 列出每个字段的名字、类型、说明。
3. `required` 列出哪些字段必填。

工具描述是给模型看的接口文档。 描述写得含糊，模型就会传错参数；这不是模型"笨"，是你没把契约写清楚。第 5 章会把这件事拆开讲。

## 2.4 模型返回了什么

一个带工具调用的响应，大概长这样：

```json
{
  "choices": [
    {
      "message": {
        "role": "assistant",
        "content": null,
        "tool_calls": [
          {
            "id": "call_abc123",
            "type": "function",
            "function": {
              "name": "get_weather",
              "arguments": "{\"city\":\"北京\"}"
            }
          }
        ]
      },
      "finish_reason": "tool_calls"
    }
  ],
  "usage": {
    "prompt_tokens": 120,
    "completion_tokens": 18
  }
}
```

四个关键点：

### 1. `content` 可以是 `null`

当模型决定调用工具时，它可能不返回任何自然语言，`content` 就是 `null`。

很多初学者只读 `content`，看到 `null` 就以为出错了。实际上真正的信息在 `tool_calls` 里。

### 2. `arguments` 是一个 JSON **字符串**，不是对象

注意这一行：

```json
"arguments": "{\"city\":\"北京\"}"
```

它的值是一个字符串，里面才是 JSON。你需要先把它当字符串取出来，再自己做一次 `json.Unmarshal`。

为什么协议要这样设计？因为不同工具的参数字段完全不同，协议层只能保证"它是一段 JSON 文本"。解析参数是运行时的责任。

### 3. `finish_reason` 告诉你它为什么停

常见取值：

| 值 | 含义 | 你该做什么 |
|---|---|---|
| `stop` | 模型正常说完了 | 结束这一轮 |
| `tool_calls` | 模型想调用工具 | 执行工具，把结果放回去，再问一次 |
| `length` | 输出被长度限制截断 | 不要当成功；JSON 很可能不完整，需要重试或续写 |
| `content_filter` | 内容被安全策略拦截 | 记录并处理，不要当成"模型没回答" |

`finish_reason` 很容易被忽略，但它是失败分类的入口。仓库早年就踩过这个坑：`internal/llm/openai.go` 解析了它却没传出来，于是"输出被截断"会伪装成"模型不会写 JSON"。现在 `internal/llm/llm.go` 的 `Response.FinishReason` 把它带出来了，你可以在 Agent 层决定怎么处理。

### 4. `usage` 是这次调用的 token 用量

`prompt_tokens` 是输入，`completion_tokens` 是输出。后面做预算时，这两个数就是账本的基础。

> **token 是什么？** 可以先把它理解成"模型计费和计长度的单位"。一个中文字大约 1 到 2 个 token，一个英文单词大约 1 个多 token。你不需要精确换算，只需要知道：**上下文越长，prompt_tokens 越大，钱越多。**

## 2.5 工具结果怎么放回去

模型请求了工具，你的运行时执行完，必须把结果放回消息列表，再发起第二次请求。

第二次请求的消息列表要包含：

1. 原来的 system 和 user 消息；
2. **assistant 的那条带 `tool_calls` 的消息**；
3. 每个工具调用对应的 `tool` 消息。

```json
{
  "model": "gpt-4o-mini",
  "messages": [
    { "role": "system", "content": "你是一个简洁的助手。" },
    { "role": "user", "content": "北京今天天气怎么样？" },
    {
      "role": "assistant",
      "content": null,
      "tool_calls": [
        {
          "id": "call_abc123",
          "type": "function",
          "function": {
            "name": "get_weather",
            "arguments": "{\"city\":\"北京\"}"
          }
        }
      ]
    },
    {
      "role": "tool",
      "tool_call_id": "call_abc123",
      "name": "get_weather",
      "content": "{\"temperature\":22,\"condition\":\"晴\"}"
    }
  ],
  "tools": [ "..." ]
}
```

三个细节，每一个都值得记住：

### 细节一：assistant 那条消息不能省

你必须把模型刚才那条带 `tool_calls` 的 assistant 消息原样放回去。它不是"聊天记录"，它是协议的一部分：它告诉模型"你刚才请求过这个调用"。

省掉它，模型就不知道工具结果对应的是哪个请求。

### 细节二：`tool_call_id` 必须严格对应

模型可能在一轮里请求多个工具：

```json
"tool_calls": [
  { "id": "call_1", "function": { "name": "get_weather", "arguments": "{\"city\":\"北京\"}" } },
  { "id": "call_2", "function": { "name": "get_weather", "arguments": "{\"city\":\"上海\"}" } }
]
```

这时你要返回两条 `tool` 消息，每条带自己的 `tool_call_id`。ID 就是用来做配对的。写错 ID，模型会把北京的结果当成上海的。

### 细节三：工具返回也应该是结构化文本

工具的执行结果通常是一段 JSON 字符串。为什么不让工具直接返回自然语言？

因为自然语言会丢字段。第 8 章会专门讲这件事：**让概率模型做无损数据搬运，是在给自己埋雷。** 现在先记住：工具返回 JSON，模型再决定怎么用它。

## 2.6 Agent 运行时只做三件事

把上面的流程画出来，就是一个循环：

```text
        ┌──────────────────────────────────────────┐
        │                                          │
        v                                          │
  组装消息 ──> 调用模型 ──> 有 tool_calls? ──否──> 返回最终回复
                            │
                            是
                            v
                      逐个执行工具
                            │
                            v
                      追加 tool 消息
                            │
                            └──────── 回到开头
```

一个 Agent 运行时，核心就三件事：

1. 组装上下文：system + 历史消息 + 工具定义。
2. 执行工具：把模型请求的调用真的做掉，把结果追加回上下文。
3. 决定继续还是停止：有 `tool_calls` 就继续，没有就结束；同时有一个硬上限，防止永远继续。

后面的多 Agent、黑板、图编排、断点恢复，都是在这个循环上长出来的。**这个循环没搞懂，后面的东西都会变成死记硬背。**

## 2.7 最小实验：把原始 JSON 打出来

书配套了一个小程序：`book/code/ch02/main.go`。它只做一件事：发一次带工具的请求，把请求体和原始响应都打印出来。

在 PowerShell 里运行：

```powershell
$env:OPENAI_API_KEY="sk-..."
go run ./book/code/ch02
```

如果你用的是第三方 OpenAI 兼容服务，再加两个环境变量：

```powershell
$env:OPENAI_BASE_URL="https://你的服务地址/v1"
$env:OPENAI_MODEL="你的模型名"
```

你会看到三段输出：

1. `===== REQUEST =====`：你发出去的完整请求。
2. `===== RAW RESPONSE =====`：服务端返回的原始 JSON。
3. `===== 关键字段 =====`：程序帮你提取的 `finish_reason`、`content`、`tool_calls`。

这个程序故意不执行工具。它只是让你看清楚协议。执行工具、循环、终止条件，是下一章的事。

如果模型没有返回 `tool_calls`，而是直接回答"我无法获取实时天气"，先别怀疑代码。换一个更明确的工具描述，或者换一个支持 function calling 的模型，再试一次。

## 2.8 坏版本：只调用一次

下面是一个"看起来像 Agent、其实不是"的版本：

```go
func run(messages []message.Message) (string, error) {
    resp, err := client.Generate(ctx, llm.Request{
        System:   "你是一个助手。",
        Messages: messages,
        Tools:    registry.Definitions(),
    })
    if err != nil {
        return "", err
    }

    // 模型请求了工具，但这里只执行了第一个
    if len(resp.ToolCalls) > 0 {
        result, _ := registry.Execute(ctx, resp.ToolCalls[0].Name, resp.ToolCalls[0].Arguments)
        _ = result // 故意丢掉
    }

    // 直接返回 content —— 它很可能是空的
    return resp.Content, nil
}
```

把它跑起来，你会看到两种结果：

- 模型返回 `content: null`，你的程序输出空字符串；
- 或者模型在下一轮（如果还有下一轮）反复请求同一个工具。

原因很简单：**工具结果没有回到上下文里，模型不知道事情已经做完了。**

这个坏版本的价值在于：它让你亲眼看到"少了一步"会发生什么。下一章我们会补上这一步，然后立刻遇到下一个问题——循环该在哪里停。

## 2.9 练习

先自己做，再看 `answers/02-参考答案.md`。

### 练习 1：读懂一次响应

运行 `book/code/ch02`，从原始 JSON 里找出：

- `finish_reason` 的值；
- `tool_calls[0].id`；
- `tool_calls[0].function.arguments`，并说明它是字符串还是对象。

### 练习 2：描述决定行为

把工具描述从"查询某个城市当前的天气"改成"获取数据"，再运行一次。观察：

- 模型还会调用这个工具吗？
- 它传的参数还对吗？
- 如果不对，它传了什么？

这个练习没有标准答案，但有标准结论：工具描述是模型唯一的接口文档。

### 练习 3：补上第二次请求

修改 `book/code/ch02/main.go`，在拿到 `tool_calls` 之后：

1. 假装执行工具，构造一条 `tool` 消息；
2. 把 assistant 的 `tool_calls` 消息和这条 `tool` 消息追加到消息列表；
3. 再发一次请求；
4. 打印第二次的 `content`。

提示：第二次请求不要再让模型调用工具也可以；先把 `tools` 留空，观察它如何基于工具结果回答。

### 练习 4：删掉 `tool_call_id`

把练习 3 里的 `tool_call_id` 删掉，再发一次。记录服务端返回的错误。

这个错误信息很重要。以后你看到它，就知道是协议配对出了问题，而不是模型出了问题。

## 2.10 小结与自检
合上书，回答：

- 用一句话说清"模型调用工具"这句话为什么不准确。
- 为什么 `arguments` 是字符串，而不是对象？
- 为什么第二次请求必须带上 assistant 的 `tool_calls` 消息？
- `finish_reason=length` 意味着什么？为什么不能当成功？
- 一个 Agent 运行时最核心的三件事是什么？

能答出四题以上，再进下一章。答不出，回到 2.4 和 2.5。

## 2.10 拓展：空回复不等于模型坏了

拿 2.7 节打印出来的原始响应，只看 `content` 和 `finish_reason`，先不看 `tool_calls`。你会发现：`content` 为 null 时，模型不一定出了问题，它可能正在请求工具。

下次遇到空回复，先打印原始 JSON，确认它属于哪一种情况，再决定是改 Prompt、改工具，还是改解析代码。

## 2.11 下一章

协议看懂了，下一步是把它跑起来。第 3 章写一个 150 行左右的 Agent：两个工具、一个循环、一个硬上限。你会第一次看到模型自己决定先搜索、再渲染，也会遇到第一个坑：循环什么时候停。
