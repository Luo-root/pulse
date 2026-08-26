# llm/openai

OpenAI 两个线协议变体的 `llm.ChatModel` 适配器。

传输层用官方 SDK `github.com/openai/openai-go/v3`（本包内别名 `sdk`）。本包只做 **llm 词汇表 ↔ SDK 类型** 的语义映射：消息块、工具、流事件、计量、错误分类。SSE 解析、类型生成由 SDK 承担。

读完这篇应能：登记两个 provider、按 MIME 喂多模态、知道哪个变体不支持 stop/Audio、用环境变量跑冒烟。

| 常量 | Provider 名 | 线协议 |
|---|---|---|
| `ProviderCompletions` | `"openai"` | `POST /v1/chat/completions` |
| `ProviderResponses` | `"openai-responses"` | `POST /v1/responses` |

## 上手

```go
ctx := kernel.New()
defer ctx.Dispose()

reg := llm.NewRegistry(ctx)
if err := openai.Register(ctx, reg); err != nil { /* ... */ }
// Register 内部两次 RegisterProvider，各自是可逆效应。

if err := reg.Declare("main", llm.Config{
    Provider: openai.ProviderCompletions, // 或 ProviderResponses
    Model:    "gpt-4o",
    APIKey:   os.Getenv("OPENAI_API_KEY"),
    BaseURL:  "", // 空 = 官方；兼容网关填完整前缀，如 https://api.minimaxi.com/v1
}); err != nil { /* ... */ }

model, err := reg.Open("main")
resp, err := model.Generate(ctx, llm.NewRequest(llm.UserText("hi")))
```

`Config.Options` 本包认识的键（未知键忽略）：

| 键 | 作用 |
|---|---|
| `organization` / `project` | OpenAI-Organization / OpenAI-Project 头 |
| `timeout_seconds` | HTTP 客户端超时；0 = 由 ctx 管 |
| `max_retries` | SDK 传输层重试次数；**默认显式 0（关闭）** |
| `headers` | 附加请求头 |

APIKey 必填，本包不读环境变量——配置显式优先。

## 两个变体怎么选

**Completions（`openai`）** — 生态默认，兼容网关最多。

- 工具结果展开为顶层 `role=tool` 消息
- 思维链：响应里的 `reasoning_content` ExtraFields（DeepSeek 等）。SDK 对未知字段 `Valid()` 恒 false，只看 `Raw()`
- 流式才带 `stream_options.include_usage`；**Generate 不带**（部分网关对非流式 400）
- 长度上限用 `max_completion_tokens`，不用已废弃的 `max_tokens`
- `req.Audio`：下发 `modalities: ["text","audio"]` + `audio.voice`；`Format` 非空才下发 `audio.format`（空 = 交给 provider，解码 MIME 仍按 `audio/wav`）
- 响应 `message.audio.data` → `PartCustom`；`transcript` 非空 → 额外 `PartText`

**Responses（`openai-responses`）** — 原生 reasoning 块。

- system 进顶层 `instructions`；`Store=false`（无状态，历史全量传入）
- 工具历史：`function_call` / `function_call_output`
- `StopSequences`、`req.Audio` → **显式 `ErrBadRequest`**，请求不会发出
- `FinishReason`：output 含 function_call → `FinishToolCalls`；仅 `status=incomplete` 才映射截断 / 内容过滤
- user 内容按遇到顺序构建：图在文前就是图在前

## 多模态（调用方只给词汇表块）

| MIME | Completions | Responses |
|---|---|---|
| `image/*` | 官方 `image_url` | 官方 `input_image` |
| `audio/*` | 官方 `input_audio`（须内联字节，不接受 URL） | Override `input_audio` |
| `application/pdf` | 官方 `file`（须内联） | 官方 `input_file`（Data 或 URL） |
| `video/*` | Override `video_url` | Override `input_video` |
| 其他 / 空 MediaType | `ErrBadRequest`，**不发请求** | 同左 |

官方不认的块（video）在官方端点会 400，不静默丢。docx/xlsx/pptx 不在本包。

```go
req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
    llm.Text("这段视频里发生了什么"),
    llm.MediaURL("video/mp4", "https://example.com/clip.mp4"),
}})
```

## 流式契约

- ctx 取消：先发 `EventError(canceled)` 再关 channel
- 流式 audio：每片独立 base64，**先 decode 再拼字节**（拼字符串再解会因 padding 错位）
- transcript 可能只出现在末片，pump 会累积进 done 的文本块
- `pcm` / `pcm16` 的 MIME 标为 `audio/pcm`（不是非法的 `audio/pcm16`）

## 错误

| 条件 | Kind |
|---|---|
| 401 / 403 | `auth` |
| 429 / 错误码含 `rate_limit` | `rate_limit` |
| 404 | `no_model` |
| 文案含 context length / prompt too long | `context_length` |
| 错误码含 `content_filter` | `content_filter` |
| 400 / 405 / 422 | `bad_request` |
| 5xx / `server_error` | `provider` |
| `context.Canceled` | `canceled` |
| 传输失败 | `network` |

## 测试与真机

离线（httptest，无网络）：

```
go test -race -skip TestLive ./llm/openai
```

真机冒烟：无对应环境变量则 `Skip`。凭据放仓库根 `.env`（已 gitignore），`TestMain` 只填尚未设置的变量。

| 变量 | 用途 |
|---|---|
| `PULSE_OPENAI_API_KEY` | Completions / Responses 真机（必填才跑） |
| `PULSE_OPENAI_BASE_URL` | 覆盖端点，如 MiniMax `https://api.minimaxi.com/v1` |
| `PULSE_OPENAI_MODEL` | 默认 `gpt-4o-mini` |
| `PULSE_OPENAI_SKIP_RESPONSES` | 设为 `1` 时跳过 Responses |
| `PULSE_MIMO_API_KEY` | MiMo TTS / ASR 闭环 |
| `PULSE_MIMO_BASE_URL` | 如 `https://api.xiaomimimo.com/v1` |

应用代码用各家官方变量名（`OPENAI_API_KEY` 等）即可；**不要**把测试门控的 `PULSE_*` 写进生产配置。

已用 MiniMax-M3 跑通 Completions（文本/流/工具/图/视频）和 Responses（文本/流/工具/图）；用 MiMo 跑通 TTS 合成 wav 再喂 ASR。

## 有意钉死

- SDK 自动重试默认关。
- 输入侧 reasoning 不回传。
- 不写供应商专有字段（`thinking` / `reasoning_split` / `service_tier`）。网关扩展只要不破坏通用路径即可用（如 `video_url`）。

## 不做

`PreviousResponseID` / Conversations、内置工具（web_search 等）、独立端点 `/v1/audio/speech` 与 `/v1/audio/transcriptions`、Azure/Bedrock 专用签名。
