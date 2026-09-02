[English](README.md) | [中文](README_zh.md)

# llm

The model adaptation layer of pulse v2: a provider-neutral chat vocabulary + adapter Registry.

Consumers (`loop`, batch processing, evaluation) depend only on `ChatModel` and never touch any provider wire format. The layering is isomorphic to DSH's `ctx.llm`:

```
adapter 插件（openai / anthropic / …）
    │ RegisterProvider(scope, "openai", factory)   // 可逆效应
    ▼
Registry（本包，kernel 服务 pulse.llm）
    │ Declare + Open / OpenDefault
    ▼
ChatModel.Generate / Stream
```

After reading this you should be able to: construct messages and requests, open a named model, read stream events, and make backoff decisions based on `ErrKind`. For the concrete wire formats see [`openai/README.md`](openai/README.md).

## ChatModel

```go
type ChatModel interface {
    Generate(ctx context.Context, req *GenerateRequest) (*Response, error)
    Stream(ctx context.Context, req *GenerateRequest) (<-chan StreamEvent, error)
}
```

The two paths are semantically consistent: `Stream` is the incremental `Generate`. Both must be cancellable via `ctx`. Errors should be this package's `*Error` (carrying a `Kind`).

## Messages: content-block

`Message{Role, []Part}`. Roles: `system` / `user` / `assistant` / `tool`.

Six block kinds:

| Kind | Valid fields | Where it appears |
|---|---|---|
| `text` | `Text` | anywhere |
| `image` | `Image` (`Data` or `URL` + `MediaType`) | usually user |
| `tool_call` | `ToolCallValue` | assistant only |
| `tool_result` | `ToolResultValue` | tool / user only |
| `reasoning` | `Text` | assistant; **adapters do not echo it back on the input side** |
| `custom` | `Media` (MIME + Data/URL) | open modality |

`PartCustom` uses IANA MIME to carry audio, video, PDF and future types — **no vocabulary change needed**. An adapter that does not support a MIME must return `ErrBadRequest`; silently dropping it is forbidden.

Constructors (excerpt): `System` / `UserText` / `ToolMessage` / `Text` / `ImageURL` / `ImageData` / `Media` / `MediaURL` / `Call` / `Result` / `Reasoning`.

**File input boundaries** (not vendor laziness — it is the nature of the formats):

| Format | Chat API | How to feed the model |
|---|---|---|
| Text txt/md | as a string | `PartText` |
| Image / audio / video | vision or audio/video block | `PartImage` / `PartCustom` |
| PDF | all three providers have a vision pipeline (text layer + per-page rendering) | `PartCustom(application/pdf)` |
| docx / xlsx / pptx | **no provider natively feeds them into the vision pipeline** | the application layer parses them into Markdown/images first, then treats them as text/image |

Office documents are zip+XML containers; parsing belongs to a separate component, not this package.

## Request and Response

The zero value of `GenerateRequest` = defer to provider defaults; no magic values.

```
Messages / Tools / ToolChoice（含 Parallel 并行控制）
Temperature / TopP / TopK / MaxTokens
StopSequences / ResponseFormat
Audio              官方对话接口的音频输出（voice + format）；仅 Completions
Reasoning          Effort（OpenAI）/ BudgetTokens（Anthropic extended thinking）
Output             Verbosity / Logprobs / TopLogprobs（仅 OpenAI）
Metadata           审计透传；provider 不理解则忽略
```

**The capability matrix is an explicit contract**: when a field has no counterpart parameter in a provider's wire format, the adapter returns
`ErrBadRequest` (e.g. TopK to OpenAI, StopSequences to Responses,
Output.Verbosity to Anthropic) — no silent ignoring, no implicit cross-provider conversion.
`Reasoning` is the exception: `Effort` belongs to OpenAI, `BudgetTokens` to Anthropic;
each knob belonging to its own provider is a contract written in the struct comments, **with no implicit cross-provider conversion**. When both are set, each adapter consumes only its own field: OpenAI sends only Effort, Anthropic
sends only BudgetTokens; the other field does not participate in the request, which does not mean double reasoning.

The vocabulary **does not take** provider-specific sampling and routing long-tail fields (OpenAI's seed /
penalties / logit_bias / store / prompt_cache_key / safety_identifier /
service_tier / user; Anthropic's service_tier / cache_control) —
when you need the full official capability, connect to the corresponding adapter directly or wait for provider-specific request types.

`Audio` is the official `audio` parameter of OpenAI Chat Completions, not some gateway's private extension. The Responses wire format has no such field — the adapter must return an explicit `bad_request`; pretending not to see it is forbidden.

`Response`: `Message` + `FinishReason` (stop / tool_calls / length / content_filter / error) + `TokenUsage` (including cached input).

`Clone` deep-copies the fields that interception may mutate (scalar pointers, ToolChoice, ResponseFormat, Audio, Reasoning, Output, Metadata). Messages / Tools are copied at slice level with elements shared. Waterfall listeners should call `req.Clone()` before mutating, to avoid polluting the caller.

## Stream Events

The channel always closes after `done` or `error`; a `ctx` cancellation ends with `EventError` before closing. Just `range` over it.

| Kind | Meaning |
|---|---|
| `text_delta` / `reasoning_delta` | text / chain-of-thought delta |
| `tool_call_begin` / `tool_call_delta` | tool call start (CallID/Name) and argument JSON fragments |
| `error` | failure; closes after this |
| `done` | aggregated `Response`; closes after this |

`Index`: 0 = text/chain-of-thought, 1 onwards = tool calls (in arrival order).

**No audio delta events, by design.** Half frames of audio are meaningless to a word-by-word UI; the adapter aggregates within the stream and delivers the result as a `custom` block on the Message arriving with `done`.

## Errors

```go
type Error struct {
    Kind       ErrKind
    Provider   string // 注册中心自身错误为 ""
    StatusCode int    // 非 HTTP 为 0
    Detail     string
    Err        error
}
```

Retryability is determined **solely** by `Kind` (`KindOf` / `IsRetryable`):

| Kind | Retryable | Typical sources |
|---|---|---|
| `rate_limit` / `network` / `provider` | yes | 429, transport failures, 5xx |
| `auth` / `bad_request` / `context_length` / `content_filter` / `no_model` / `canceled` | no | credentials, parameters, over-length input, safety policy, cancellation |

This package **does not retry**. Upper layers back off or fail over based on Kind.

## Registry

Kernel service key: `llm.ServiceKey` (`"pulse.llm"`). You can also use `NewRegistry` directly as a library.

```go
ctx := kernel.New()
defer ctx.Dispose()

reg := llm.NewRegistry(ctx)
_ = openai.Register(ctx, reg) // 一次登记两个 OpenAI 变体；可逆

_ = reg.Declare("main", llm.Config{
    Provider: "openai",          // 已 RegisterProvider 的名字
    Model:    "gpt-4o",
    APIKey:   os.Getenv("OPENAI_API_KEY"), // 应用凭据用各家官方变量名
    BaseURL:  "",                          // 空 = 官方端点；填网关地址即兼容服务
})
model, err := reg.Open("main")
```

In-package test gating uses `PULSE_OPENAI_API_KEY` / `PULSE_OPENAI_BASE_URL` / `PULSE_OPENAI_MODEL` (plus MiMo's `PULSE_MIMO_*`) — not the same set of names as the `OPENAI_API_KEY` in the application example above.

`RegisterProvider(scope, name, factory)` is a kernel effect: the adapter should pass in the Context from its own `Apply`; when the plugin unloads, the factory and all opened instances are reclaimed together. Overwriting the same name = revoking the old one; already-opened instances under that provider close immediately.

Interception seams (no instance wrapping; dispatch goes through **Local**):

- `pulse.llm.before_generate` (WaterfallLocal, `*GenerateRequest`): routing, default parameters, redaction, rate limiting
- `pulse.llm.after_response` (EmitLocal, value-type `Response`): metering, auditing; observers cannot alter the caller's result

Request-level scope injection (option A):

```go
ctx = llm.WithEventScope(ctx, reqScope) // loop 在调模型前会做这一步
// observed：优先 EventScopeFrom(ctx)；没有则回退 Registry 构造时的 ctx（仍 Local）
```

Division of labor with loop: this package covers the token level; loop covers the decision level (approvals, traces). A request-level Bridge must attach to the same `reqScope`, otherwise it will not hear the Local events.

**The seam with loop's request assembly**: the `GenerateRequest` assembled by `loop.Agent` mainly carries Messages/Tools and **leaves blank** Temperature / MaxTokens etc. Anthropic Messages' `MaxTokens` is **required** (`nil` → `ErrBadRequest`). When calling Anthropic, the assembly layer's `before_generate` or an explicit request must fill it in; examples/demoapp attaches the defaults to `Registry.EventScope()` (the 01-chat fallback path) and the per-request `reqScope` (02/03) — it is not about adding a full request Option surface to the Agent, nor about attaching to the host root.

You can also use `llm.Plugin()` to Provide the Registry into the enclosing scope; on unload, `Close` all instances.

## Minimal Call

```go
req := llm.NewRequest(llm.UserText("用一句话介绍 Go"))
resp, err := model.Generate(ctx, req)
fmt.Println(resp.Message.Text())

ch, err := model.Stream(ctx, req)
for ev := range ch {
    switch ev.Kind {
    case llm.EventTextDelta:
        fmt.Print(ev.Text)
    case llm.EventError:
        log.Println(ev.Err)
    }
}
```

Multimodal input only supplies vocabulary blocks; the wire format is mapped by the adapter:

```go
req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
    llm.Text("这张图里有什么"),
    llm.ImageURL("https://example.com/cat.png", "image/png"),
}})
```

TTS (Completions): `req.Audio = &llm.AudioOutput{Voice: "alloy", Format: "wav"}`; the response carries `PartCustom(audio/*)`; a non-empty `transcript` adds one more text block.

## Deliberately Pinned

- Transport-layer automatic retry is off by default.
- Input-side reasoning is not echoed back.
- Unknown MIME / fields unsupported by a variant: explicit `bad_request`, no silent dropping.

## Export Overview

Positioning: provider-neutral vocabulary + Registry. Consumers see only `ChatModel`.

**Messages**

| Symbol | What it does |
|---|---|
| `Role` / `RoleSystem|User|Assistant|Tool` | Roles. Tool stays close to OpenAI; the Anthropic adapter folds it into a tool_result inside a user message |
| The six `PartKind` constants | Block kinds, see the table above |
| `Part` | Kind determines which pointer/string is valid |
| `MediaContent` / `ImageSource` | Open modality / image: Data first, otherwise URL; MediaType required |
| `ToolCall` / `ToolResult` | Calls initiated by the model; results sent back (`IsError` lets the model self-correct) |
| `Message` | `Role + Parts`; optional `Name` (multiple personas; ignored when the provider does not support it) |
| `Text` / `Reasoning` / `ImageURL` / `ImageData` / `Media` / `MediaURL` / `Call` / `Result` / `ResultParts` | Block constructors. `Result` = single-text success; `ResultParts` can carry `isError` and multiple blocks |
| `System` / `User` / `UserText` / `Assistant` / `AssistantText` / `ToolMessage` | Message constructors |
| `(*Message).Text` | Concatenates all `PartText` (excluding reasoning), joined with newlines |
| `(*Message).ReasoningText` | Concatenates the chain of thought; `""` when there is none |
| `(*Message).ToolCalls` | All tool calls in this message |
| `(*Message).Clone` | Top-level deep copy; pointers inside Parts are shared (messages are immutable by convention) |

**Request / Response / Stream**

| Symbol | What it does |
|---|---|
| `ToolDef` | A tool exposed to the model: Name / Description / Parameters(JSON Schema) |
| `ToolChoice` / `ToolAuto|None|Any|Specific` | nil = provider default. Fill `Name` for Specific |
| `ResponseFormat` / `FormatText|JSONObject|JSONSchema` | nil = plain text |
| `AudioOutput` | Completions audio output; empty `Format` = provider default |
| `ReasoningOptions` | `Effort` (OpenAI) / `BudgetTokens` (Anthropic extended thinking) |
| `OutputOptions` | `Verbosity` / `Logprobs` / `TopLogprobs` (OpenAI only; Anthropic rejects explicitly) |
| `GenerateRequest` / `NewRequest` | The full request; the zero value defers to the provider |
| `(*GenerateRequest).Clone` | For interception rewrites; deep-copies Reasoning/Output and scalar fields |
| The five `FinishReason` constants | stop / tool_calls / length / content_filter / error |
| `TokenUsage` / `Total` | Input, output, cached; `Total` = input + output |
| `Response` | Message + FinishReason + Usage |
| The six `StreamEventKind` constants | see the stream events table |
| `StreamEvent` | Kind / Index / Text / CallID / ToolName / Response / Err |
| `ChatModel` | `Generate` + `Stream` |

**Errors**

| Symbol | What it does |
|---|---|
| The ten `ErrKind` constants | see the errors table |
| `Error` | Kind + Provider + StatusCode + Detail + the underlying Err |
| `NewError` | For adapters to construct classified errors |
| `KindOf` / `IsRetryable` | Extracts the Kind from the error chain; retryable only for rate_limit/network/provider |
| `(*Error).Error` / `Unwrap` | The standard error interface |

**Registry**

| Symbol | What it does |
|---|---|
| `ServiceKey` | kernel service key `"pulse.llm"` |
| `EventBeforeGenerate` / `EventAfterResponse` | waterfall `*GenerateRequest` / emit value `Response` |
| `Config` | Provider / Model / BaseURL / APIKey / Options (**client-level keys only**: organization / project / timeout_seconds / max_retries / headers; unknown keys are ignored. Do not stuff request parameters such as top_k / service_tier into Options) |
| `Factory` | `func(Config) (ChatModel, error)` |
| `Registry` / `NewRegistry` | Factories + named instances. The construction Context is the Local fallback dispatch domain when there is no request scope |
| `(*Registry).EventScope` | That fallback dispatch domain (a private sub-ctx of Plugin Apply). Without `WithEventScope`, observed falls back here; the assembly layer's `before_generate` must attach to this scope, not the host root (EmitLocal does not bubble up to parents) |
| `WithEventScope` / `EventScopeFrom` | Injects the request scope into `context.Context` for observed Local dispatch |
| `Plugin` | Provides the Registry into a scope; `Close` on unload |
| `RegisterProvider` | Registers a factory reversibly; overwriting the same name closes that provider's opened instances |
| `Declare` | Declares a named instance; a repeated id replaces it and closes the old instance |
| `SetDefault` / `DefaultID` | The default instance id |
| `Open` / `OpenDefault` | Opens or reuses a cached instance; undeclared / no factory → `ErrNoModel` |
| `Drop` | Closes the instance and deletes the declaration; if it was the default, the default is cleared |
| `Close` | Closes everything and invalidates registrations; idempotent |
| `Providers` | Registered provider names (for diagnostics) |

**Test doubles (`mock.go`)**

| Symbol | What it does |
|---|---|
| `ScriptedModel` | Replays preset Responses in order; when exhausted, repeats the last one. Implements ChatModel |
| `Resp` / `RespToolCalls` | Plain-text / tool-call responses |
| `NewScripted` / `NewFailing` | Scripted model / always-failing model |

## Out of Scope

Session storage, retry failover, sub-agents, standalone speech endpoints (`/v1/audio/speech` etc.; ASR/TTS go through the chat wire format), Azure/Bedrock-specific signing, native understanding of Office documents.
