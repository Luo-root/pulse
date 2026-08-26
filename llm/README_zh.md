# llm

pulse v2 的模型适配层：provider 中立的对话词汇表 + 适配器注册中心。

消费方（`loop`、批处理、评测）只依赖 `ChatModel`，不接触任何供应商线格式。分层与 DSH 的 `ctx.llm` 同构：

```
adapter 插件（openai / anthropic / …）
    │ RegisterProvider(scope, "openai", factory)
    ▼
Registry（本包，kernel 服务 pulse.llm）
    │ Open / OpenDefault
    ▼
ChatModel
```

## 词汇表

### 消息 = content-block

`Message{Role, []Part}`。六种块：

| Kind | 字段 | 说明 |
|---|---|---|
| `text` | `Text` | 普通文本 |
| `image` | `Image` | 内联字节或 URL |
| `tool_call` | `ToolCallValue` | 仅 assistant |
| `tool_result` | `ToolResultValue` | 仅 tool / user |
| `reasoning` | `Text` | 思维链 |
| `custom` | `Media` | 开放模态：`audio/*`、`video/*`、`application/pdf`…… |

`PartCustom` 用 MIME 承载未知扩展，**不必改词汇表**。adapter 不支持某种 MIME 时应返回 `ErrBadRequest`，禁止静默丢弃。

docx / xlsx / pptx **不属于**对话词汇表——它们是 zip+XML 容器，没有「每页渲染成图」的视觉管线。应用层先解析成 Markdown / 图块再喂模型。纯文本（txt / md）读成字符串即可。

### 请求

`GenerateRequest` 是一次补全的完整描述。零值 = 交给 provider 默认，不设魔法值。

```
messages / tools / tool_choice
temperature / top_p / max_tokens / stop
response_format          JSON Schema 结构化输出
audio                    官方对话接口的音频输出模态（voice + format）
metadata                 审计透传；provider 不理解则忽略
```

`Audio` 仅 Chat Completions 线格式支持。Responses 变体必须显式 `bad_request`，禁止静默丢掉。

### 流事件

`Stream` 是增量的 `Generate`。channel 在 `done` 或 `error` 后必然关闭；ctx 取消以 `EventError` 收尾。

| Kind | 含义 |
|---|---|
| `text_delta` / `reasoning_delta` | 文本 / 思维链增量 |
| `tool_call_begin` / `tool_call_delta` | 工具调用开始（CallID/Name）与参数片段 |
| `error` | 失败，此后关闭 |
| `done` | 聚合 `Response`（含 Usage、FinishReason），此后关闭 |

**有意不做 audio 增量事件。** 音频半帧对逐字 UI 无意义；适配器在流内聚合，随 `done` 的 Message 以 `custom` 块交付。

`Index`：0 = 文本 / 思维链，1 起 = 工具调用（按到达顺序）。

### 错误

`Error{Kind, Provider, StatusCode}`。可重试性由 `Kind` **唯一**决定：

| Kind | 可重试 |
|---|---|
| `rate_limit` / `network` / `provider` | 是 |
| `auth` / `bad_request` / `context_length` / `content_filter` / `no_model` / `canceled` | 否 |

`KindOf` / `IsRetryable` 供上层退避与 failover。重试本身不在本包。

## 注册中心

```go
ctx := kernel.New()
defer ctx.Dispose()

reg := llm.NewRegistry(ctx)
_ = openai.Register(ctx, reg) // 可逆效应：插件卸载则工厂收回

_ = reg.Declare("main", llm.Config{
    Provider: "openai",
    Model:    "gpt-4o",
    APIKey:   os.Getenv("OPENAI_API_KEY"),
    BaseURL:  "", // 空 = 官方端点；填网关即接兼容服务
})
model, err := reg.Open("main")
```

拦截 seam（能力挂载，不包裹实例）：

- `pulse.llm.before_generate`（waterfall，载荷 `*GenerateRequest`）：路由、默认参数、脱敏、限流。监听器应 `req.Clone()` 后改写再 `next`。
- `pulse.llm.after_response`（emit，载荷值类型 `Response`）：计量、审计、缓存。观察者改不了调用方结果。

与 `loop` 层分工：本包管 token 级；loop 管 agent 决策级（审批、轨迹）。两层不重复。

## 最小调用

```go
req := llm.NewRequest(llm.UserText("用一句话介绍 Go"))
resp, err := model.Generate(ctx, req)
fmt.Println(resp.Message.Text())

ch, err := model.Stream(ctx, req)
for ev := range ch {
    if ev.Kind == llm.EventTextDelta {
        fmt.Print(ev.Text)
    }
}
```

多模态输入只给词汇表块，映射由 adapter 完成：

```go
req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
    llm.Text("这张图里有什么"),
    llm.ImageURL("https://example.com/cat.png", "image/png"),
    llm.MediaURL("video/mp4", "https://example.com/clip.mp4"),
}})
```

## 有意钉死

- SDK / 传输层自动重试默认关闭：重试与 failover 属上层编排。
- 输入侧 reasoning 块不回传（Completions 线格式无此概念；Responses 的有状态链依赖 `PreviousResponseID`）。
- `Clone` 只深拷贝拦截会改的字段（标量指针、ToolChoice、ResponseFormat、Audio、Metadata）。Messages / Tools 切片级复制、元素共享。

## 不做

会话存储、重试 failover、子 agent、音频独立端点（`/v1/audio/speech` 等——ASR/TTS 走对话线格式）、Azure/Bedrock 专用签名、Office 文档原生理解。
