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
    loop.WithEventScope(host),     // 可选：轨迹进宿主作用域树
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
| `WithEventScope` | nil | 传入宿主或其子作用域才派发事件 |

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

| 事件 | 模式 | 何时 |
|---|---|---|
| `pulse.loop.turn_start` | emit | 回合开始，带 Input + History |
| `pulse.loop.step_start` | emit | 每个推理-行动步 |
| `pulse.loop.after_model` | emit | 模型响应就绪（含 Usage） |
| `pulse.loop.before_tool_call` | **waterfall** | 工具执行前 |
| `pulse.loop.after_tool_call` | emit | 执行完或被拒绝 |
| `pulse.loop.turn_end` | emit | 任意退出路径恰好一次 |

HITL 挂在 `before_tool_call`：

```go
_, _ = kernel.OnWaterfall(host, loop.EventBeforeToolCall,
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

## 不做

会话存储、重试 failover、把 Agent 装配成 Plugin、暴露 `llm.StreamEvent` channel。
