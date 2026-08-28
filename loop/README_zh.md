# loop

pulse v2 的 ReAct 循环：一个**无状态**的回合执行器。

Agent 接收对话历史与本轮输入，驱动「模型 ↔ 工具」直到模型不再发起工具调用，返回本回合新增消息。

读完这篇应能：配一个带工具的 Agent、跑 `Run` / `RunStream`、在 `before_tool_call` 上挂 HITL、读懂 `Result.StoppedBy`。

## 刻意不做

| 不做 | 归谁 |
|---|---|
| 会话存储 | 调用方持有 `history`；P2 session 再补 |
| 重试 / failover | 上层编排；`llm.KindOf` 是弹药 |
| 硬接某家模型或某套工具 | 只依赖 `llm.ChatModel` 与 `ToolSet` |
| 填齐请求采样/限长字段 | Agent 组请求主要带 Messages/Tools；Temperature / **MaxTokens** 等由调用方经 `before_generate` 或显式 `GenerateRequest` 补齐 |

**和 Anthropic 的缝**：Messages 线格式 **MaxTokens 必填**（`nil` → `ErrBadRequest`）。本包不填、也不设魔法默认。装配层（如 `examples/internal/demoapp`）可用 `before_generate` 仅在空值时注入默认——这是宿主示范，不是给 Agent 加完整请求 Option 面。

Agent 是库对象，不是插件。`WithEventScope(nil)`（默认）零派发、零内核足迹。

实例只持配置引用，不可变、无锁；并发 `Run` 共享同一 Agent 安全（下游 ChatModel / ToolSet 自行保证并发安全）。

## 上手

```go
tools := loop.NewMemToolSet()
err := tools.Register(llm.ToolDef{
    Name:        "echo",
    Description: "原样返回参数",
    Parameters:  json.RawMessage(`{"type":"object","properties":{"s":{"type":"string"}}}`),
}, func(ctx context.Context, args json.RawMessage) (string, error) {
    return string(args), nil
})

agent, err := loop.NewAgent(model,
    loop.WithToolSet(tools),
    loop.WithSystemPrompt("你是助手"),
    loop.WithMaxSteps(0),          // 默认就是 0 = 不限制
    loop.WithEventScope(reqScope), // 可选：请求级 scope；事件走 EmitLocal/WaterfallLocal
)

var history []*llm.Message
res, err := agent.Run(ctx, history, llm.UserText("帮我 echo 一句 hi"))
fmt.Println(res.Final.Text())
history = append(history, llm.UserText("帮我 echo 一句 hi"))
history = append(history, res.Messages...)
```

`Run` 是不带增量回调的 `RunStream`。流式 UI 用 **`onDelta`**，**不是** `<-chan llm.StreamEvent`：

```go
res, err := agent.RunStream(ctx, func(s string) {
    fmt.Print(s) // assistant 文本增量
}, history, llm.UserText("…"))
```

结构化轨迹（步、工具、用量）走内核事件，见下。

## Result

```go
type Result struct {
    Messages  []*llm.Message // 本回合新产生的 assistant/tool，不含 system/history/input
    Final     *llm.Message   // 最后一条 assistant；MaxSteps 打断时可能仍带未执行的 tool_call
    Usage     llm.TokenUsage
    Steps     int
    StoppedBy StopReason     // completed | max_steps | canceled | error
}
```

| StoppedBy | `error` 返回值 | 含义 |
|---|---|---|
| `completed` | nil | 模型不再调工具 |
| `max_steps` | nil | 安全阀打断，**不是错误** |
| `canceled` | `ctx.Err()` | 调用方取消 |
| `error` | 非 nil | 模型调用失败 / 流异常终止 |

所有退出路径 `defer` 统一发一次 `turn_end`。`Messages` 带上**已发生的部分**（第一步之前就取消时可能仍是 nil，`len==0`）。工具执行失败不会升格为 `StopError`：错误文本作为 `IsError` 的 tool 结果回传模型，回合继续。

## 选项

| Option | 默认 | 说明 |
|---|---|---|
| `WithToolSet` | nil = 纯对话 | 见 ToolSet |
| `WithSystemPrompt` | 空 | 每回合插在工作缓冲最前 |
| `WithMaxSteps` | **0 = 不限制** | `<=0` 都归一为无限。需要安全阀再显式设正数 |
| `WithEventScope` | nil | 传入 scope 才派发；**请求级事实用 `EmitLocal`/`WaterfallLocal`**，建议每请求独立子作用域 |

几十上百次工具调用是常态，所以默认不限步。

## ToolSet

```go
type ToolSet interface {
    Definitions() []llm.ToolDef // 同一次 Run 内多次调用顺序必须稳定
    Execute(ctx context.Context, call llm.ToolCall) (string, error)
}
```

- `Execute` 必须尊重 `ctx` 取消
- 返回的 error 变成回传模型的失败文本，**不中断回合**
- 未知工具名由实现报错；panic 由 loop 在回合边界恢复

`MemToolSet`：内存表，并发安全。`Register(def, fn) error`，同名重复或空名 / nil handler 返回错误（装配期冲突尽早暴露）。`Definitions` 按工具名排序以保证稳定。

## 事件（决策级）

token 级（路由、限流、计量）挂 `llm.before_generate` / `after_response`。本包只管 agent 决策。`scope == nil` 时下列全部是空操作。

派发一律走 **Local**（只本 scope，不向父/子/兄弟）。调用模型前会 `llm.WithEventScope(ctx, a.scope)`，让 llm 拦截也落到同一请求 scope。

| 事件 | 模式 | 何时 |
|---|---|---|
| `pulse.loop.turn_start` | EmitLocal | 回合开始，带 Input + History |
| `pulse.loop.step_start` | EmitLocal | 每个推理-行动步 |
| `pulse.loop.after_model` | EmitLocal | 模型响应就绪（含 Usage） |
| `pulse.loop.before_tool_call` | **WaterfallLocal** | 工具执行前 |
| `pulse.loop.after_tool_call` | EmitLocal | 执行完或被拒绝 |
| `pulse.loop.turn_end` | EmitLocal | 任意退出路径恰好一次 |

HITL 挂在 `before_tool_call`（与 Agent 同一 `reqScope`）：

```go
_, _ = kernel.OnWaterfall(reqScope, loop.EventBeforeToolCall,
    func(btc *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
        if dangerous(btc.Call) {
            btc.Rejected = true
            btc.RejectReason = "需要人工批准"
            return btc // 不调 next → 短路，不执行工具
        }
        return next(btc)
    })
```

契约：全程以返回值里的 `Call` 为准（可改 Arguments / Name）。`Rejected=true` 且不委托 `next` 即拒绝；模型收到 `IsError` 结果。`AfterToolCall.Result` 与回传模型的文本同源；`Rejected` 表示未真实执行。

文本增量只走 `onDelta`，不走这些事件。

## 有意钉死

- `Messages` 仅本回合产出，调用方自己 append 成多轮历史
- 工具失败 ≠ 回合失败
- 不做子 agent、不做多模态工具结果（Execute 返回字符串）
- 不把 `MaxTokens` / Temperature 等请求参数焊进 Agent；provider 必填项归装配层或显式请求

## 导出一览

定位：无状态回合执行器。设计：历史在调用方、扩展全走事件、默认不限步。

**执行器**

| 符号 | 做什么 |
|---|---|
| `Agent` | 配置+依赖引用，不可变 |
| `Option` | `func(*Agent)` |
| `NewAgent` | `model` 必填，否则 error |
| `WithToolSet` / `WithSystemPrompt` / `WithMaxSteps` / `WithEventScope` | 见选项表 |
| `(*Agent).Run` | `RunStream(ctx, nil, history, input...)` |
| `(*Agent).RunStream` | `onDelta func(string)` 可 nil；返回 `(*Result, error)` |
| `Result` | Messages / Final / Usage / Steps / StoppedBy |
| `StopReason` 四常量 | completed / max_steps / canceled / error |

**工具**

| 符号 | 做什么 |
|---|---|
| `ToolSet` | `Definitions` + `Execute` |
| `ToolFunc` | `func(ctx, json.RawMessage) (string, error)` |
| `MemToolSet` / `NewMemToolSet` | 内存实现，并发安全 |
| `(*MemToolSet).Register` | `(def, fn) error`；空名 / nil / 重名都报错 |
| `Definitions` / `Execute` | 按名排序；未知工具名 error。panic 不在本类型恢复 |

**事件载荷**（`scope==nil` 时不派发）

| 符号 | 字段 | 做什么 |
|---|---|---|
| `EventTurnStart` + `TurnStart` | `Input`, `History` | 本轮新增 vs 已有历史 |
| `EventStepStart` + `StepStart` | `Step` | 从 1 计的推理-行动步 |
| `EventAfterModel` + `AfterModel` | `Response`, `Step` | 含 Usage |
| `EventBeforeToolCall` + `BeforeToolCall` | `Call`, `Rejected`, `RejectReason` | waterfall；改 Call 或置 Rejected 短路 |
| `EventAfterToolCall` + `AfterToolCall` | `Call`, `Result`, `Duration`, `Err`, `Rejected` | Result 与回传模型文本同源 |
| `EventTurnEnd` + `TurnEnd` | `Final`, `Usage`, `Steps`, `StoppedBy` | 任意退出恰好一次 |

## 不做

会话存储、重试 failover、把 Agent 装配成 Plugin、暴露 `llm.StreamEvent` channel。
