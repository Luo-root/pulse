# llm

pulse v2 的模型适配层：provider 中立的对话词汇表 + 适配器注册中心。

消费方（`loop`、批处理、评测）只依赖 `ChatModel`，不接触任何供应商线格式。分层与 DSH 的 `ctx.llm` 同构：

```
adapter 插件（openai / anthropic / …）
    │ RegisterProvider(scope, "openai", factory)   // 可逆效应
    ▼
Registry（本包，kernel 服务 pulse.llm）
    │ Declare + Open / OpenDefault
    ▼
ChatModel.Generate / Stream
```

读完这篇应能：构造消息和请求、打开一个命名模型、读流事件、按 `ErrKind` 做退避决策。具体线格式见 [`openai/README_zh.md`](openai/README_zh.md)。

## ChatModel

```go
type ChatModel interface {
    Generate(ctx context.Context, req *GenerateRequest) (*Response, error)
    Stream(ctx context.Context, req *GenerateRequest) (<-chan StreamEvent, error)
}
```

两条路径语义一致：`Stream` 是增量的 `Generate`。都必须可被 `ctx` 取消。错误应为本包 `*Error`（带 `Kind`）。

## 消息：content-block

`Message{Role, []Part}`。角色：`system` / `user` / `assistant` / `tool`。

六种块：

| Kind | 有效字段 | 出现位置 |
|---|---|---|
| `text` | `Text` | 任意 |
| `image` | `Image`（`Data` 或 `URL` + `MediaType`） | 通常 user |
| `tool_call` | `ToolCallValue` | 仅 assistant |
| `tool_result` | `ToolResultValue` | 仅 tool / user |
| `reasoning` | `Text` | assistant；**输入侧 adapter 不回传** |
| `custom` | `Media`（MIME + Data/URL） | 开放模态 |

`PartCustom` 用 IANA MIME 承载音频、视频、PDF 及未来类型，**不必改词汇表**。adapter 不支持某种 MIME 必须 `ErrBadRequest`，禁止静默丢。

构造器（节选）：`System` / `UserText` / `ToolMessage` / `Text` / `ImageURL` / `ImageData` / `Media` / `MediaURL` / `Call` / `Result` / `Reasoning`。

**文件输入边界**（不是厂商偷懒，是格式本质）：

| 格式 | 对话 API | 怎么喂模型 |
|---|---|---|
| 文本 txt/md | 当字符串 | `PartText` |
| 图片 / 音频 / 视频 | 视觉或音视频块 | `PartImage` / `PartCustom` |
| PDF | 三家都有视觉管线（文本层 + 每页渲染） | `PartCustom(application/pdf)` |
| docx / xlsx / pptx | **没有一家原生进视觉管线** | 应用层先解析成 Markdown/图，再当 text/image |

Office 文档是 zip+XML 容器，解析属于独立组件，不是本包。

## 请求与响应

`GenerateRequest` 零值 = 交给 provider 默认，不设魔法值。

```
Messages / Tools / ToolChoice（含 Parallel 并行控制）
Temperature / TopP / TopK / MaxTokens
StopSequences / ResponseFormat
Audio              官方对话接口的音频输出（voice + format）；仅 Completions
Reasoning          Effort（OpenAI）/ BudgetTokens（Anthropic extended thinking）
Output             Verbosity / Logprobs / TopLogprobs（仅 OpenAI）
Metadata           审计透传；provider 不理解则忽略
```

**能力矩阵是显式契约**：字段在某家线格式无对应参数时，adapter 返回
`ErrBadRequest`（如 TopK 传 OpenAI、StopSequences 传 Responses、
Output.Verbosity 传 Anthropic），不静默忽略、不做跨家隐式换算。
`Reasoning` 例外：`Effort` 归 OpenAI、`BudgetTokens` 归 Anthropic，
两个 knob 各归各家是结构注释写明的契约。

词汇表**不收** provider 独有的采样与路由长尾（OpenAI 的 seed /
penalties / logit_bias / store / prompt_cache_key / safety_identifier /
service_tier / user；Anthropic 的 service_tier / cache_control）——
需要完整官方能力时直连对应 adapter 或等 provider 专属请求类型。

`Audio` 是 OpenAI Chat Completions 官方 `audio` 参数，不是某家网关私货。Responses 线格式没有该字段——adapter 必须显式 `bad_request`，禁止当没看见。

`Response`：`Message` + `FinishReason`（stop / tool_calls / length / content_filter / error）+ `TokenUsage`（含 cached input）。

`Clone` 深拷贝拦截会改的字段（标量指针、ToolChoice、ResponseFormat、Audio、Reasoning、Output、Options、Metadata）。Messages / Tools 切片级复制、元素共享。waterfall 监听器应 `req.Clone()` 再改，避免污染调用方。

## 流事件

channel 在 `done` 或 `error` 后必然关闭；`ctx` 取消以 `EventError` 收尾再关。`range` 即可。

| Kind | 含义 |
|---|---|
| `text_delta` / `reasoning_delta` | 文本 / 思维链增量 |
| `tool_call_begin` / `tool_call_delta` | 工具开始（CallID/Name）与参数 JSON 片段 |
| `error` | 失败，此后关闭 |
| `done` | 聚合 `Response`，此后关闭 |

`Index`：0 = 文本/思维链，1 起 = 工具调用（按到达顺序）。

**有意不做 audio 增量事件。** 半帧音频对逐字 UI 无意义；适配器在流内聚合，随 `done` 的 Message 以 `custom` 块交付。

## 错误

```go
type Error struct {
    Kind       ErrKind
    Provider   string // 注册中心自身错误为 ""
    StatusCode int    // 非 HTTP 为 0
    Detail     string
    Err        error
}
```

可重试性由 `Kind` **唯一**决定（`KindOf` / `IsRetryable`）：

| Kind | 可重试 | 典型来源 |
|---|---|---|
| `rate_limit` / `network` / `provider` | 是 | 429、传输失败、5xx |
| `auth` / `bad_request` / `context_length` / `content_filter` / `no_model` / `canceled` | 否 | 凭据、参数、超长、安全策略、取消 |

本包**不做重试**。上层按 Kind 退避或 failover。

## 注册中心

kernel 服务键：`llm.ServiceKey`（`"pulse.llm"`）。也可以直接 `NewRegistry` 当库用。

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

包内测试门控用的是 `PULSE_OPENAI_API_KEY` / `PULSE_OPENAI_BASE_URL` / `PULSE_OPENAI_MODEL`（以及 MiMo 的 `PULSE_MIMO_*`），和上面应用示例的 `OPENAI_API_KEY` 不是同一组名字。

`RegisterProvider(scope, name, factory)` 是内核效应：adapter 应传入自己 `Apply` 的 Context，插件卸载则工厂与已开实例一并收回。同名覆盖 = 撤旧，该 provider 下已开实例立即关闭。

拦截 seam（不包裹实例）：

- `pulse.llm.before_generate`（waterfall，`*GenerateRequest`）：路由、默认参数、脱敏、限流
- `pulse.llm.after_response`（emit，值类型 `Response`）：计量、审计；观察者改不了调用方结果

与 loop 分工：本包 token 级；loop 决策级（审批、轨迹）。

也可用 `llm.Plugin()` 把 Registry Provide 到所在作用域，卸载时 `Close` 全部实例。

## 最小调用

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

多模态只给词汇表块，线格式由 adapter 映射：

```go
req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
    llm.Text("这张图里有什么"),
    llm.ImageURL("https://example.com/cat.png", "image/png"),
}})
```

TTS（Completions）：`req.Audio = &llm.AudioOutput{Voice: "alloy", Format: "wav"}`，响应里 `PartCustom(audio/*)`；`transcript` 非空会多一个文本块。

## 有意钉死

- 传输层自动重试默认关闭。
- 输入侧 reasoning 不回传。
- 未知 MIME / 变体不支持的字段：显式 `bad_request`，不静默丢。

## 导出一览

定位：provider 中立词汇表 + 注册中心。消费方只见 `ChatModel`。

**消息**

| 符号 | 做什么 |
|---|---|
| `Role` / `RoleSystem|User|Assistant|Tool` | 角色。Tool 贴近 OpenAI；Anthropic adapter 会折成 user 里的 tool_result |
| `PartKind` 六个常量 | 块类型，见上文表 |
| `Part` | Kind 决定哪个指针/字符串有效 |
| `MediaContent` / `ImageSource` | 开放模态 / 图像：Data 优先，否则 URL；MediaType 必填 |
| `ToolCall` / `ToolResult` | 模型发起的调用；回传结果（`IsError` 让模型自我修正） |
| `Message` | `Role + Parts`；可选 `Name`（多角色，provider 不支持则忽略） |
| `Text` / `Reasoning` / `ImageURL` / `ImageData` / `Media` / `MediaURL` / `Call` / `Result` / `ResultParts` | 块构造器。`Result` = 单文本成功；`ResultParts` 可带 `isError` 与多块 |
| `System` / `User` / `UserText` / `Assistant` / `AssistantText` / `ToolMessage` | 消息构造器 |
| `(*Message).Text` | 拼接全部 `PartText`（不含 reasoning），换行连接 |
| `(*Message).ReasoningText` | 拼接思维链；无则 `""` |
| `(*Message).ToolCalls` | 本条全部 tool_call |
| `(*Message).Clone` | 顶层深拷贝，Part 内指针共享（消息按不可变约定） |

**请求 / 响应 / 流**

| 符号 | 做什么 |
|---|---|
| `ToolDef` | 暴露给模型的工具：Name / Description / Parameters(JSON Schema) |
| `ToolChoice` / `ToolAuto|None|Any|Specific` | nil = provider 默认。Specific 时填 `Name` |
| `ResponseFormat` / `FormatText|JSONObject|JSONSchema` | nil = 纯文本 |
| `AudioOutput` | Completions 音频输出；`Format` 空 = provider 默认 |
| `ReasoningOptions` | `Effort`（OpenAI）/ `BudgetTokens`（Anthropic extended thinking） |
| `OutputOptions` | `Verbosity` / `Logprobs` / `TopLogprobs`（仅 OpenAI；Anthropic 显式拒绝） |
| `GenerateRequest` / `NewRequest` | 完整请求；零值交 provider |
| `(*GenerateRequest).Clone` | 拦截改写用；深拷贝 Reasoning/Output 与标量字段 |
| `FinishReason` 五个常量 | stop / tool_calls / length / content_filter / error |
| `TokenUsage` / `Total` | 输入、输出、cached；`Total` = 输入+输出 |
| `Response` | Message + FinishReason + Usage |
| `StreamEventKind` 六个常量 | 见流事件表 |
| `StreamEvent` | Kind / Index / Text / CallID / ToolName / Response / Err |
| `ChatModel` | `Generate` + `Stream` |

**错误**

| 符号 | 做什么 |
|---|---|
| `ErrKind` 十个常量 | 见错误表 |
| `Error` | Kind + Provider + StatusCode + Detail + 底层 Err |
| `NewError` | adapter 构造分类错误 |
| `KindOf` / `IsRetryable` | 从 error 链取 Kind；可重试仅 rate_limit/network/provider |
| `(*Error).Error` / `Unwrap` | 标准 error 接口 |

**注册中心**

| 符号 | 做什么 |
|---|---|
| `ServiceKey` | kernel 服务键 `"pulse.llm"` |
| `EventBeforeGenerate` / `EventAfterResponse` | waterfall `*GenerateRequest` / emit 值 `Response` |
| `Config` | Provider / Model / BaseURL / APIKey / Options |
| `Factory` | `func(Config) (ChatModel, error)` |
| `Registry` / `NewRegistry` | 工厂 + 命名实例。传入的 Context 是拦截事件的派发作用域 |
| `Plugin` | 把 Registry Provide 到作用域；卸载时 `Close` |
| `RegisterProvider` | 可逆登记工厂；同名覆盖关闭该 provider 已开实例 |
| `Declare` | 声明命名实例；重复同 id 替换并关旧实例 |
| `SetDefault` / `DefaultID` | 默认实例 id |
| `Open` / `OpenDefault` | 打开或复用缓存；未声明 / 无工厂 → `ErrNoModel` |
| `Drop` | 关实例并删声明；若是 default 则清空 default |
| `Close` | 关全部、作废登记；幂等 |
| `Providers` | 已登记 provider 名（诊断） |

**测试替身（`mock.go`）**

| 符号 | 做什么 |
|---|---|
| `ScriptedModel` | 按序回放预置 Response；耗尽则重复最后一条。实现 ChatModel |
| `Resp` / `RespToolCalls` | 纯文本 / 工具调用响应 |
| `NewScripted` / `NewFailing` | 脚本模型 / 恒失败模型 |

## 不做

会话存储、重试 failover、子 agent、独立语音端点（`/v1/audio/speech` 等；ASR/TTS 走对话线格式）、Azure/Bedrock 专用签名、Office 文档原生理解。
