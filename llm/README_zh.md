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
Messages / Tools / ToolChoice
Temperature / TopP / MaxTokens / StopSequences
ResponseFormat     text | json_object | json_schema
Audio              官方对话接口的音频输出（voice + format）；仅 Completions
Metadata           审计透传；provider 不理解则忽略
```

`Audio` 是 OpenAI Chat Completions 官方 `audio` 参数，不是某家网关私货。Responses 线格式没有该字段——adapter 必须显式 `bad_request`，禁止当没看见。

`Response`：`Message` + `FinishReason`（stop / tool_calls / length / content_filter / error）+ `TokenUsage`（含 cached input）。

`Clone` 深拷贝拦截会改的字段（标量指针、ToolChoice、ResponseFormat、Audio、Metadata）。Messages / Tools 切片级复制、元素共享。waterfall 监听器应 `req.Clone()` 再改，避免污染调用方。

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

## 不做

会话存储、重试 failover、子 agent、独立语音端点（`/v1/audio/speech` 等；ASR/TTS 走对话线格式）、Azure/Bedrock 专用签名、Office 文档原生理解。
