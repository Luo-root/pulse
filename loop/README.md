[English](README.md) | [中文](README_zh.md)

# loop

The ReAct loop of pulse v2: a **stateless** turn executor.

The Agent receives the conversation history and this turn's input, drives "model ↔ tools" until the model stops initiating tool calls, and returns the messages newly added in this turn.

After reading this you should be able to: configure an Agent with tools, run `Run` / `RunStream`, attach HITL on `before_tool_call`, and read `Result.StoppedBy`.

## Deliberately Not Done

| Not done | Owned by |
|---|---|
| Session storage | The caller holds `history`; for the source of truth on sessions see [`memory/session`](../memory/session/README.md) (P2 shipped; loop does not import memory) |
| Retry / failover | Upper-layer orchestration; `llm.KindOf` is the ammunition |
| Hard-wiring a specific model or tool set | Depends only on `llm.ChatModel` and `ToolSet` |
| Filling in request sampling/length-cap fields | The request the Agent assembles mainly carries Messages/Tools; Temperature / **MaxTokens** etc. are filled in by the caller via `before_generate` or an explicit `GenerateRequest` |

**The seam with Anthropic**: the Messages wire format **requires MaxTokens** (`nil` → `ErrBadRequest`). This package does not fill it and sets no magic default. The assembly layer (e.g. `examples/internal/demoapp`) can inject a default via `before_generate` only when the value is empty — this is a host demonstration, not a full request Option surface for the Agent.

The Agent is a library object, not a plugin. `WithEventScope(nil)` (the default) means zero dispatch and zero kernel footprint.

An instance holds only configuration references — immutable, lock-free; concurrent `Run` calls sharing the same Agent are safe (downstream ChatModel / ToolSet guarantee their own concurrency safety).

## Getting Started

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

`Run` is `RunStream` without an incremental callback. Streaming UIs use **`onDelta`**, **not** `<-chan llm.StreamEvent`:

```go
res, err := agent.RunStream(ctx, func(s string) {
    fmt.Print(s) // assistant 文本增量
}, history, llm.UserText("…"))
```

Structured traces (steps, tools, usage) travel through kernel events; see below.

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

| StoppedBy | `error` return value | Meaning |
|---|---|---|
| `completed` | nil | the model no longer calls tools |
| `max_steps` | nil | the safety valve cut in, **not an error** |
| `canceled` | `ctx.Err()` | the caller canceled |
| `error` | non-nil | model call failed / stream terminated abnormally |

All exit paths send exactly one `turn_end` via `defer`. `Messages` carries **what has already happened** (it may still be nil, `len==0`, if canceled before the first step). Tool execution failure is never escalated to `StopError`: the error text goes back to the model as an `IsError` tool result, and the turn continues.

## Options

| Option | Default | Description |
|---|---|---|
| `WithToolSet` | nil = pure chat | see ToolSet |
| `WithSystemPrompt` | empty | inserted at the very front of the working buffer every turn |
| `WithMaxSteps` | **0 = unlimited** | `<=0` all normalize to unlimited. Set a positive number explicitly when you need a safety valve |
| `WithEventScope` | nil | dispatches only when a scope is passed; **request-level facts use `EmitLocal`/`WaterfallLocal`**; a separate sub-scope per request is recommended |

Dozens to hundreds of tool calls are the norm, so steps are unlimited by default.

## ToolSet

```go
type ToolSet interface {
    Definitions() []llm.ToolDef // 同一次 Run 内多次调用顺序必须稳定
    Execute(ctx context.Context, call llm.ToolCall) (string, error)
}
```

- `Execute` must respect `ctx` cancellation
- A returned error becomes the failure text sent back to the model, **without interrupting the turn**
- Unknown tool names are reported as errors by the implementation; panics are recovered by loop at turn boundaries

`MemToolSet`: an in-memory table, concurrency-safe. `Register(def, fn) error` returns an error on duplicate names or empty name / nil handler (assembly-time conflicts surface early). `Definitions` is sorted by tool name for stability.

## Events (Decision Level)

Token-level concerns (routing, rate limiting, metering) hook `llm.before_generate` / `after_response`. This package only handles agent decisions. When `scope == nil`, everything below is a no-op.

Dispatch always goes through **Local** (this scope only; never to parents/children/siblings). Before calling the model it runs `llm.WithEventScope(ctx, a.scope)`, so llm interception also lands on the same request scope.

| Event | Mode | When |
|---|---|---|
| `pulse.loop.turn_start` | EmitLocal | turn begins, with Input + History |
| `pulse.loop.step_start` | EmitLocal | each reasoning-acting step |
| `pulse.loop.after_model` | EmitLocal | model response ready (with Usage) |
| `pulse.loop.before_tool_call` | **WaterfallLocal** | before tool execution |
| `pulse.loop.after_tool_call` | EmitLocal | after execution or rejection |
| `pulse.loop.turn_end` | EmitLocal | exactly once on any exit path |

HITL hangs off `before_tool_call` (the same `reqScope` as the Agent):

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

Contract: throughout, the `Call` in the return value is authoritative (Arguments / Name may be modified). `Rejected=true` without delegating to `next` means rejection; the model receives an `IsError` result. `AfterToolCall.Result` shares the same source as the text sent back to the model; `Rejected` means it was not actually executed.

Text deltas only travel through `onDelta`, not through these events.

## Deliberately Pinned

- `Messages` only contains what this turn produced; the caller appends them into multi-turn history itself
- Tool failure ≠ turn failure
- No sub-agents, no multimodal tool results (`Execute` returns a string)
- Request parameters such as `MaxTokens` / Temperature are not welded into the Agent; provider-required fields belong to the assembly layer or an explicit request

## Export Overview

Positioning: a stateless turn executor. Design: history lives with the caller, extension goes entirely through events, steps are unlimited by default.

**Executor**

| Symbol | What it does |
|---|---|
| `Agent` | Configuration + dependency references, immutable |
| `Option` | `func(*Agent)` |
| `NewAgent` | `model` is required; error otherwise |
| `WithToolSet` / `WithSystemPrompt` / `WithMaxSteps` / `WithEventScope` | see the options table |
| `(*Agent).Run` | `RunStream(ctx, nil, history, input...)` |
| `(*Agent).RunStream` | `onDelta func(string)` may be nil; returns `(*Result, error)` |
| `Result` | Messages / Final / Usage / Steps / StoppedBy |
| The four `StopReason` constants | completed / max_steps / canceled / error |

**Tools**

| Symbol | What it does |
|---|---|
| `ToolSet` | `Definitions` + `Execute` |
| `ToolFunc` | `func(ctx, json.RawMessage) (string, error)` |
| `MemToolSet` / `NewMemToolSet` | in-memory implementation, concurrency-safe |
| `(*MemToolSet).Register` | `(def, fn) error`; empty name / nil / duplicate names all error |
| `Definitions` / `Execute` | sorted by name; unknown tool names error. Panics are not recovered in this type |

**Event payloads** (not dispatched when `scope==nil`)

| Symbol | Fields | What it does |
|---|---|---|
| `EventTurnStart` + `TurnStart` | `Input`, `History` | this turn's new input vs existing history |
| `EventStepStart` + `StepStart` | `Step` | reasoning-acting step counted from 1 |
| `EventAfterModel` + `AfterModel` | `Response`, `Step` | includes Usage |
| `EventBeforeToolCall` + `BeforeToolCall` | `Call`, `Rejected`, `RejectReason` | waterfall; modify Call or set Rejected to short-circuit |
| `EventAfterToolCall` + `AfterToolCall` | `Call`, `Result`, `Duration`, `Err`, `Rejected` | Result shares the same source as the text sent back to the model |
| `EventTurnEnd` + `TurnEnd` | `Final`, `Usage`, `Steps`, `StoppedBy` | exactly once on any exit |

## Out of Scope

Session storage, retry failover, assembling the Agent into a Plugin, exposing the `llm.StreamEvent` channel.
