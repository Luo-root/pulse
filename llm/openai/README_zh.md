# llm/openai

OpenAI 两个线协议变体的 `llm.ChatModel` 适配器。传输层用官方 SDK `openai-go/v3`（本包内别名 `sdk`）；适配器只做 **llm 词汇表 ↔ SDK 类型** 的语义映射。

| Provider 名 | 线协议 | 路径 |
|---|---|---|
| `openai` | Chat Completions | `POST /v1/chat/completions` |
| `openai-responses` | Responses | `POST /v1/responses` |

## 用法

```go
reg := llm.NewRegistry(ctx)
_ = openai.Register(ctx, reg) // 一次登记两个 provider，各自是可逆效应

_ = reg.Declare("main", llm.Config{
    Provider: openai.ProviderCompletions, // 或 ProviderResponses
    Model:    "gpt-4o",
    APIKey:   os.Getenv("OPENAI_API_KEY"),
    BaseURL:  "", // 空 = 官方；填 https://api.minimaxi.com/v1 即接兼容网关
})
model, _ := reg.Open("main")
```

`Config.Options` 本包认识的键（未知键忽略）：

```
organization / project     OpenAI-Organization / OpenAI-Project 头
timeout_seconds            HTTP 客户端超时；0 = 由 ctx 管控
max_retries                SDK 传输层重试；默认显式 0（关闭）
headers                    附加请求头
```

## 两个变体

**Completions（`openai`）** — 生态默认，兼容网关最多。

- 工具结果展开为顶层 `tool` 消息
- 思维链走 `reasoning_content` ExtraFields（DeepSeek 等网关；SDK 对未知字段 `Valid()` 恒 false，只看 `Raw()`）
- 流式带 `stream_options.include_usage`；**Generate 不带** `stream_options`（部分网关对非流式会 400）
- `max_completion_tokens` 新字段，不用已废弃的 `max_tokens`
- `Audio` 输出：下发 `modalities: ["text","audio"]` + `audio.{voice,format}`；响应 `message.audio` 解码为 `PartCustom`，`transcript` 非空映射为文本块

**Responses（`openai-responses`）** — OpenAI 主推接口，原生 reasoning。

- system 进顶层 `instructions`
- `Store=false`：无状态，历史由调用方全量传入
- 工具历史项是 `function_call` / `function_call_output`
- **不支持** `StopSequences`、`Audio` 输出 → 显式 `ErrBadRequest`，禁止静默丢
- `FinishReason`：output 含 function_call → `FinishToolCalls`；`status=incomplete` 才走截断 / 内容过滤

## 多模态映射（适配层一次性打通）

调用方只给 `PartImage` / `PartCustom`，不要在使用处手写线格式。

| MIME | Completions | Responses |
|---|---|---|
| `image/*` | 官方 `image_url` | 官方 `input_image` |
| `audio/*` | 官方 `input_audio`（须内联字节） | `input_audio` Override |
| `application/pdf` | 官方 `file`（须内联） | 官方 `input_file` |
| `video/*` | `video_url` Override | `input_video` Override |
| 其他 | `ErrBadRequest`，不发请求 | 同左 |

官方不认的块（video）会 `bad_request`，不静默丢。内容块**按遇到顺序**下发：图在文前就是图在前。

Office 文档（docx/xlsx/pptx）不在本包。纯文本读成字符串。PDF 走视觉管线（文本层 + 每页渲染）。

## 流式契约

- ctx 取消：先发 `EventError(canceled)` 再关 channel
- 流式 audio：每片独立 base64，**先 decode 再拼字节**（拼字符串再解会因 padding 错位）
- 空 `Audio.Format`：请求不下发 `format` 字段，交给 provider；解码 MIME 空仍标 `audio/wav`；`pcm`/`pcm16` → `audio/pcm`

## 错误映射

| HTTP / 特征 | Kind |
|---|---|
| 401 / 403 | `auth` |
| 429 / `rate_limit` | `rate_limit` |
| 404 | `no_model` |
| 上下文超长文案 | `context_length` |
| `content_filter` | `content_filter` |
| 400 / 422 | `bad_request` |
| 5xx / `server_error` | `provider` |
| ctx 取消 | `canceled` |
| 传输失败 | `network` |

## 真机冒烟（门控，凭据不入库）

环境变量（也可放仓库根 `.env`，已被 gitignore）：

```
PULSE_OPENAI_API_KEY / BASE_URL / MODEL
PULSE_OPENAI_SKIP_RESPONSES=1     可选，跳过 Responses
PULSE_MIMO_API_KEY / BASE_URL     MiMo TTS/ASR 闭环
```

```
go test -race ./llm/openai                  # 离线 httptest
go test -run TestLive ./llm/openai          # 真机（无 key 自动 skip）
```

已用 MiniMax-M3 跑通 Completions（文本/流/工具/图/视频）与 Responses（文本/流/工具/图）；用 MiMo 跑通 TTS→ASR 闭环。

## 不做

`PreviousResponseID` / Conversations 有状态链、内置工具（web_search 等）、独立语音端点 `/v1/audio/speech` 与 `/v1/audio/transcriptions`（ASR/TTS 走对话线格式）、Azure/Bedrock 专用签名、供应商专有字段（`thinking` / `reasoning_split` / `service_tier`）。
