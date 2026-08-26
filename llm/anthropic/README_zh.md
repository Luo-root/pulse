# llm/anthropic

Anthropic Messages 线协议（`POST /v1/messages`）的 `llm.ChatModel` 适配器。传输层用官方 SDK `github.com/anthropics/anthropic-sdk-go`（别名 `sdk`）；本包只做 **llm 词汇表 ↔ SDK 类型** 的语义映射。

| 常量 | Provider 名 |
|---|---|
| `ProviderAnthropic` | `"anthropic"` |

## 用法

```go
reg := llm.NewRegistry(ctx)
_ = anthropic.Register(ctx, reg)

_ = reg.Declare("main", llm.Config{
    Provider: anthropic.ProviderAnthropic,
    Model:    "claude-sonnet-4-5",
    APIKey:   os.Getenv("ANTHROPIC_API_KEY"),
    BaseURL:  "", // 空 = 官方；填网关地址即接兼容服务
})
model, _ := reg.Open("main")
```

`Config.Options` 认识的键：`timeout_seconds`、`max_retries`（默认显式 0 = 关闭 SDK 自动重试）、`headers`。APIKey 必填，本包不读环境变量。

## 与 OpenAI 线格式的关键差异（已处理，调用方无感）

| 差异 | 映射 |
|---|---|
| system 不是消息角色，是顶层参数 | 全部 system 消息合并为 `System []TextBlockParam` |
| **max_tokens 必填**，provider 无默认 | `req.MaxTokens == nil` → 显式 `ErrBadRequest`，不设魔法默认值 |
| 工具结果不是独立角色 | `RoleTool` → 只含 `tool_result` 块的 user 消息（Anthropic 要求 tool_result 在 user 轮首位） |
| 音频 / 视频不支持 | `PartCustom(audio/*\|video/*)` → 显式 `ErrBadRequest` |
| PDF 走 document 块 | `PartCustom(application/pdf)`：Data → base64 source，URL → url source |
| 图片 | 内联 → base64 source；URL → url source |
| thinking 回传需 signature 配对 | 输入侧 reasoning 块不回传（与 OpenAI 同口径） |
| 无结构化输出参数 | `ResponseFormat` 非 text → 显式 `ErrBadRequest` |
| 无 audio 输出模态 | `req.Audio` → 显式 `ErrBadRequest` |

工具历史：assistant 消息里的 `tool_call` → `tool_use` 块（空 Arguments 补 `{}`）。

## StopReason 映射

| Anthropic | llm FinishReason |
|---|---|
| `end_turn` / `stop_sequence` / `pause_turn` | `stop` |
| `tool_use` | `tool_calls` |
| `max_tokens` / `model_context_window_exceeded` | `length` |
| `refusal` | `content_filter` |

## 流式契约

Anthropic 流没有 `[DONE]` 哨兵，`message_stop` 即最后一条；`message_stop` 后直接收口，连接关闭不误判为异常。

| SSE 事件 | llm 事件 |
|---|---|
| `content_block_delta.text_delta` | `text_delta` |
| `content_block_delta.thinking_delta` | `reasoning_delta`（`signature_delta` 跳过） |
| `content_block_start`(tool_use) | `tool_call_begin`（llm Index 0=文本，1 起按到达顺序；线格式 block index 只用于关联分片） |
| `content_block_delta.input_json_delta` | `tool_call_delta` |
| `message_delta` | 记录 stop_reason 与累计 Usage（input/cache_read 为累计值） |
| `message_stop` | `done` 聚合 Response |
| `event: error` | `EventError`（overloaded → provider） |

ctx 取消：先发 `EventError(canceled)` 再关 channel。

## 错误

| 条件 | Kind |
|---|---|
| 401 / 403 | `auth` |
| 429 / rate limit 文案 | `rate_limit` |
| 404 | `no_model` |
| "prompt is too long" 等超长文案 | `context_length` |
| 400 / 422 | `bad_request` |
| 529 / overloaded | `provider` |
| 5xx | `provider` |
| ctx 取消 | `canceled` |
| 传输失败 | `network` |

## 真机冒烟（门控）

```
PULSE_ANTHROPIC_API_KEY      必填才跑
PULSE_ANTHROPIC_BASE_URL     可选，覆盖端点
PULSE_ANTHROPIC_MODEL        默认 claude-sonnet-4-5
```

覆盖文本 / 流式 / 工具 / 图像（URL）。离线 httptest 覆盖线格式、system 合并、tool_result 首位、PDF base64、错误分类表、流式全事件序列、取消收尾。

## 有意钉死

- SDK 自动重试默认关闭。
- 输入侧 reasoning 不回传（thinking 回传需 signature 配对，词汇表不承载签名）。
- 未知 MIME / 变体不支持的字段：显式 `bad_request`，不静默丢。
- `PartCustom(image/*)` 与 `PartImage` 同路，都映射官方 image 块。
- 相邻同角色消息合并为一条（连续 `RoleTool` 的多个 tool_result 必须同处一条 user 轮）。
- `Metadata` 只透传 `user_id`（Anthropic 线格式仅此一键）；其余键不出去，`metadata.trace_id` 之类不要指望走这里。

## 不做

Prompt caching 断点（`cache_control`）、thinking 开关（`ThinkingConfig`）、服务端工具（web_search 等）、token counting 端点、batch API、Azure/Bedrock/Vertex 专用签名。
