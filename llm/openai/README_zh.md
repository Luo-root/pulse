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

## 工具结果的失败标记、Name 与拒答（#246 / #247 定下的口径）

| 词表字段 | Completions | Responses | Anthropic（对照） |
|---|---|---|---|
| `ToolResult.IsError` | 文本前缀 `[tool error] ` | 同左 | 原生 `is_error` |
| `Message.Name` | `name`（system / user / assistant） | 无对应字段 → 忽略 | 无对应字段 → 忽略 |
| system 消息里的非文本块 | `ErrBadRequest`（请求不发出） | 同左 | 同左 |
| 拒答（refusal） | `message.refusal` / 流式 delta → 文本块 | `refusal` 内容块 → 文本块 | — |
| `Output.Logprobs=false` + `TopLogprobs` | `ErrBadRequest` | `ErrBadRequest` | — |

- **`IsError` 以文本前缀表达**：OpenAI 两种线格式都没有工具结果的错误字段（唯一能下发的只有 tool 消息的 `content` / `function_call_output.output`），模型只能从文本分辨「工具坏了」与「工具说完了」。前缀是该语义在两变体上的**唯一**表达；结果内容为空时只留前缀本体，不留一个只有空格的尾巴。
- **`Name` 只落到有字段的那一档**：OpenAI 官方支持 `name`（system / user / assistant；tool 消息线格式没有）；Responses 变体与 Anthropic 无对应字段，忽略该值。这里的宽松口径是**有意**的——与「未支持的请求参数一律 `ErrBadRequest`」不同，见 `llm.Message.Name` godoc。
- **拒答不静默**：completions 原来把 `message.refusal`（含流式 delta）整段丢掉，拒答时上层拿到的是空回复；现在两变体一致折成文本块，`Message.Text()` 直接读得到。
- **system 里的非文本块显式报错**：两变体的系统提示都是字符串字段，图像等块不能表达——报 `ErrBadRequest` 而不是悄悄削掉一块提示词。

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

## 请求级参数

词汇表公共字段：`Temperature` / `TopP` / `MaxTokens` / `StopSequences` / `ResponseFormat` / `Audio` / `Reasoning` / `Output` / `ToolChoice.Parallel`。

| 字段 | Completions | Responses |
|---|---|---|
| `Reasoning.Effort` | `reasoning_effort` | `reasoning.effort` |
| `Output.Verbosity` | `verbosity` | `text.verbosity` |
| `Output.Logprobs` / `TopLogprobs` | `logprobs` / `top_logprobs`（TopLogprobs 自动隐含 logprobs=true；显式 `Logprobs=false` 与 TopLogprobs 冲突 → 显式 bad_request） | 仅 `top_logprobs`；单设 Logprobs=true 会显式 bad_request |
| `ToolChoice.Parallel` | `parallel_tool_calls` | 同左 |
| `TopK` | 无官方字段 → **显式 bad_request**（不为兼容网关做 JSON 注入） | 同左 |

`Config.Options` 是**客户端级**配置（organization / project / timeout_seconds / max_retries / headers），不进请求体。

OpenAI 独有的采样与路由长尾——`seed`、`frequency_penalty` / `presence_penalty`、`logit_bias`、`store`、`prompt_cache_key`、`safety_identifier`、`service_tier`、`user`——**不在词汇表**（无跨 provider 语义）。需要时直连 adapter 或经 provider 专属请求类型。

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

## 导出一览

本包对外只有登记入口和两个工厂；`completionsModel` / `responsesModel` 不导出。

| 符号 | 做什么 | 怎么用 |
|---|---|---|
| `ProviderCompletions` | `"openai"` | `Config.Provider` |
| `ProviderResponses` | `"openai-responses"` | 同上 |
| `Register` | 向 Registry 登记两个工厂 | `openai.Register(scope, reg)`，内部两次 `RegisterProvider` |
| `NewCompletions` | Completions 工厂 | `llm.Factory` 签名；也可不经 Registry 直接 `NewCompletions(cfg)` |
| `NewResponses` | Responses 工厂 | 同上 |

`Generate` / `Stream` 在未导出的实现类型上，满足 `llm.ChatModel`。线格式、MIME、错误映射见上文各节。

## 不做

`PreviousResponseID` / Conversations、内置工具（web_search 等）、独立端点 `/v1/audio/speech` 与 `/v1/audio/transcriptions`、Azure/Bedrock 专用签名。
