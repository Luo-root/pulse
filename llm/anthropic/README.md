[English](README.md) | [中文](README_zh.md)

# llm/anthropic

`llm.ChatModel` adapter for the Anthropic Messages wire protocol (`POST /v1/messages`). The transport layer uses the official SDK `github.com/anthropics/anthropic-sdk-go` (alias `sdk`); this package only does the semantic mapping of **llm vocabulary ↔ SDK types**.

| Constant | Provider name |
|---|---|
| `ProviderAnthropic` | `"anthropic"` |

## Usage

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

Keys understood in `Config.Options`: `timeout_seconds`, `max_retries` (explicitly 0 by default = SDK auto-retry off), `headers`. APIKey is required; this package does not read environment variables.

## Key Differences from the OpenAI Wire Format (handled here; invisible to callers)

| Difference | Mapping |
|---|---|
| system is not a message role but a top-level parameter | All system messages merge into `System []TextBlockParam` |
| **max_tokens is required**; the provider has no default | `req.MaxTokens == nil` → explicit `ErrBadRequest`, no magic default. `loop.Agent` does not fill it; the assembly layer (e.g. demoapp) can fill a default via `before_generate` |
| tool results are not a separate role | `RoleTool` → a user message containing only `tool_result` blocks (Anthropic requires tool_result at the head of the user turn) |
| audio / video not supported | `PartCustom(audio/*\|video/*)` → explicit `ErrBadRequest` |
| PDF goes through document blocks | `PartCustom(application/pdf)`: Data → base64 source, URL → url source |
| Images | inline → base64 source; URL → url source |
| echoing thinking back requires signature pairing | input-side reasoning blocks are not echoed back (same policy as OpenAI) |
| structured output | `ResponseFormat` json_schema → `output_config.format`; json_object has no counterpart → explicit `ErrBadRequest` |
| no audio output modality | `req.Audio` → explicit `ErrBadRequest` |

Tool history: `tool_call` in assistant messages → `tool_use` blocks (empty Arguments get `{}`).

## StopReason Mapping

| Anthropic | llm FinishReason |
|---|---|
| `end_turn` / `stop_sequence` / `pause_turn` | `stop` |
| `tool_use` | `tool_calls` |
| `max_tokens` / `model_context_window_exceeded` | `length` |
| `refusal` | `content_filter` |

## Streaming Contract

The Anthropic stream has no `[DONE]` sentinel; `message_stop` is the final event. After `message_stop` the pump wraps up immediately, so a connection close is not misread as an anomaly.

| SSE event | llm event |
|---|---|
| `content_block_delta.text_delta` | `text_delta` |
| `content_block_delta.thinking_delta` | `reasoning_delta` (`signature_delta` skipped) |
| `content_block_start`(tool_use) | `tool_call_begin` (llm Index 0=text, 1 onwards in arrival order; the wire-format block index is only used to correlate fragments) |
| `content_block_delta.input_json_delta` | `tool_call_delta` |
| `message_delta` | records stop_reason and the cumulative Usage (input/cache_read are cumulative values) |
| `message_stop` | `done` aggregates the Response |
| `event: error` | `EventError` (overloaded → provider) |

ctx cancellation: emit `EventError(canceled)` first, then close the channel.

## Request-Level Parameters

| Field | Anthropic Messages |
|---|---|
| `TopK` | native `top_k` (neither OpenAI variant has this parameter; they return an explicit bad_request) |
| `Reasoning.BudgetTokens` | `thinking: {type: enabled, budget_tokens}`; ≥1024 and < max_tokens |
| `Reasoning.Effort` | no counterpart parameter, ignored; when set together with BudgetTokens only the latter is consumed, which does not mean double reasoning |
| `Output.Verbosity` / Logprobs / TopLogprobs | no counterpart, explicit bad_request |
| `ToolChoice.Parallel` | `disable_parallel_tool_use` (the adapter negates it automatically) |

`Metadata` only passes through `user_id` (the wire format has only this one key).

Anthropic-specific `service_tier` and `cache_control` breakpoints are not in the vocabulary (no cross-provider semantics); connect to the adapter directly when needed.

## Deliberately Not Done (present in the wire format but not adapted)

`cache_control` cache breakpoints, `container` (code execution), `InferenceGeo` / `UserProfileID`, server-side tools such as `mcp_servers`, `redacted_thinking` echo-back (no signature).

## Errors

| Condition | Kind |
|---|---|
| 401 / 403 | `auth` |
| 429 / rate-limit message text | `rate_limit` |
| 404 | `no_model` |
| over-length message text such as "prompt is too long" | `context_length` |
| 400 / 422 | `bad_request` |
| 529 / overloaded | `provider` |
| 5xx | `provider` |
| ctx cancellation | `canceled` |
| transport failure | `network` |

## Live Smoke Tests (Gated)

```
PULSE_ANTHROPIC_API_KEY      必填才跑
PULSE_ANTHROPIC_BASE_URL     可选，覆盖端点
PULSE_ANTHROPIC_MODEL        默认 claude-sonnet-4-5
```

Covers text / streaming / tools / images (URL). Offline httptest covers the wire format, system merging, tool_result-first position, PDF base64, the error classification table, the full streaming event sequence, and cancellation shutdown.

## Deliberately Pinned

- SDK automatic retry is off by default.
- Input-side reasoning is not echoed back (echoing thinking back requires signature pairing; the vocabulary carries no signature).
- Unknown MIME / fields unsupported by the variant: explicit `bad_request`, no silent dropping.
- `PartCustom(image/*)` and `PartImage` take the same path; both map to the official image block.
- Adjacent same-role messages merge into one (multiple tool_results from consecutive `RoleTool` messages must live in the same user turn).
- `Metadata` only passes through `user_id` (the Anthropic wire format has only this one key); other keys never get out — do not expect `metadata.trace_id` and the like to travel through here.

## Out of Scope

Prompt caching breakpoints (`cache_control`), the thinking switch (`ThinkingConfig`), server-side tools (web_search etc.), the token counting endpoint, the batch API, Azure/Bedrock/Vertex-specific signing.
