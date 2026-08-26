# loop

pulse v2 的 ReAct 循环：一个**无状态**的回合执行器。

Agent 接收对话历史与本轮输入，驱动「模型 ↔ 工具」直至模型不再发起工具调用，返回本回合新增的全部消息。

## 刻意不做的三件事

| 不做 | 归谁 |
|---|---|
| 会话存储 | 调用方持有历史；P2 的 session 层再补 |
| 重试 / failover | 上层编排；`llm.Error.Kind` 是弹药 |
| 硬接模型或工具实现 | 只依赖 `llm.ChatModel` 与 `ToolSet` 接口 |

Agent 是库对象，不是插件：无副作用可装载就不硬套 Plugin/Loader。`scope == nil` 时零派发、零内核足迹。

## 最小用法

```go
tools := loop.NewMemToolSet()
_ = tools.Register(llm.ToolDef{
    Name:        "echo",
    Description: "原样返回",
    Parameters:  json.RawMessage(`{"type":"object","properties":{"s":{"type":"string"}}}`),
}, func(ctx context.Context, args json.RawMessage) (string, error) {
    return string(args), nil
})

agent, err := loop.NewAgent(model,
    loop.WithToolSet(tools),
    loop.WithSystemPrompt("你是助手"),
    loop.WithEventScope(host), // 可选：轨迹进入宿主作用域树
)
res, err := agent.Run(ctx, history, llm.UserText("帮我查一下…"))
fmt.Println(res.Final.Text())
// 多轮 = history = append(history, input..., res.Messages...)
```

流式：

```go
ch, err := agent.RunStream(ctx, history, llm.UserText("…"))
for ev := range ch { /* 同 llm.StreamEvent */ }
```

## Result

```
Messages   本回合新产生的消息（assistant / tool 交替），不含 system / history / input
Final      最后一条 assistant；MaxSteps 打断时可能是带未执行工具调用的中间态
Usage      本回合全部模型调用的用量累计
Steps      实际推理-行动步数
StoppedBy  completed / max_steps / canceled / error
```

所有退出路径由 `defer` 统一收尾：`Messages` 部分产出非 nil，`turn_end` 恰好一次。

## 选项

| Option | 默认 | 说明 |
|---|---|---|
| `WithToolSet` | nil = 纯对话 | 工具集合 |
| `WithSystemPrompt` | 空 | 每回合组装为首条消息 |
| `WithMaxSteps` | **0 = 不限制** | 安全阀按需打开；触发上限不是错误 |
| `WithEventScope` | nil = 不派发 | 传入宿主或其子作用域以记录轨迹 |

`MaxSteps <= 0` 归一为无限。几十上百次工具调用是常态。

## 事件（决策级）

与 `llm.before_generate` 分工：那边管 token（路由/限流/脱敏），这边管 agent 决策（审批/审计/轨迹）。

| 事件 | 模式 | 载荷 |
|---|---|---|
| `pulse.loop.turn_start` | emit | 输入 + 历史 |
| `pulse.loop.step_start` | emit | 步号 |
| `pulse.loop.after_model` | emit | 模型响应（含 Usage） |
| `pulse.loop.before_tool_call` | **waterfall** | 可改写参数；`Rejected=true` 且不委托 next 即短路（HITL） |
| `pulse.loop.after_tool_call` | emit | 结果文本、时长、错误；`Rejected` 表示未真实执行 |
| `pulse.loop.turn_end` | emit | Final / Usage / Steps / StoppedBy |

`before_tool_call` 全程以返回值 `btc.Call` 为准（effective call）。`AfterToolCall.Result` 与回传模型的文本严格同源。

文本增量（流式 UI）走独立的 `onDelta` 回调，与结构化事件分离。

## ToolSet

```go
type ToolSet interface {
    Definitions() []llm.ToolDef                          // 同一次 Run 内顺序稳定
    Execute(ctx context.Context, call llm.ToolCall) (string, error)
}
```

- `Execute` 返回非 nil error → 作为 `IsError` 结果回传模型，**回合不中断**
- 必须尊重 ctx 取消；panic 由 loop 恢复为失败结果
- `MemToolSet` 是内存实现，同名重复登记报错（装配期冲突尽早暴露）

## 有意钉死

- 实例不可变、无锁；并发 `Run` 共享同一 Agent 安全（下游依赖自行保证）
- `Messages` 仅含本回合产出
- 工具执行失败不升格为 `StopError`
- 不做子 agent、多模态工具结果（工具返回文本）
