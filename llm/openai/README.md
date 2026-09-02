[English](README.md) | [中文](README_zh.md)

# llm/openai

`llm.ChatModel` adapters for OpenAI's two wire-protocol variants.

The transport layer uses the official SDK `github.com/openai/openai-go/v3` (aliased as `sdk` inside this package). This package only does the semantic mapping of **llm vocabulary ↔ SDK types**: message blocks, tools, stream events, metering, error classification. SSE parsing and type generation are handled by the SDK.

After reading this you should be able to: register the two providers, feed multimodal input by MIME, know which variant does not support stop/Audio, and run a smoke test with environment variables.

| Constant | Provider name | Wire protocol |
|---|---|---|
| `ProviderCompletions` | `"openai"` | `POST /v1/chat/completions` |
| `ProviderResponses` | `"openai-responses"` | `POST /v1/responses` |

## Getting Started

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

Keys of `Config.Options` this package understands (unknown keys are ignored):

| Key | Effect |
|---|---|
| `organization` / `project` | OpenAI-Organization / OpenAI-Project headers |
| `timeout_seconds` | HTTP client timeout; 0 = governed by ctx |
| `max_retries` | SDK transport-layer retry count; **explicitly 0 (off) by default** |
| `headers` | Additional request headers |

APIKey is required; this package does not read environment variables — explicit configuration wins.

## Choosing Between the Two Variants

**Completions (`openai`)** — the ecosystem default; supported by the most gateways.

- Tool results are expanded into top-level `role=tool` messages
- Chain of thought: `reasoning_content` ExtraFields in the response (DeepSeek etc.). The SDK's `Valid()` is always false for unknown fields; only look at `Raw()`
- `stream_options.include_usage` is sent only when streaming; **Generate does not send it** (some gateways return 400 for non-streaming)
- The length cap uses `max_completion_tokens`, not the deprecated `max_tokens`
- `req.Audio`: sends `modalities: ["text","audio"]` + `audio.voice`; `audio.format` is sent only when `Format` is non-empty (empty = leave it to the provider; the decoding MIME is still treated as `audio/wav`)
- Response `message.audio.data` → `PartCustom`; non-empty `transcript` → an extra `PartText`

**Responses (`openai-responses`)** — native reasoning blocks.

- system goes into top-level `instructions`; `Store=false` (stateless, full history passed in)
- Tool history: `function_call` / `function_call_output`
- `StopSequences`, `req.Audio` → **explicit `ErrBadRequest`**; the request is never sent
- `FinishReason`: output containing function_call → `FinishToolCalls`; truncation / content filtering maps only from `status=incomplete`
- user content is built in encounter order: image before text stays image before text

## Multimodal (callers only supply vocabulary blocks)

| MIME | Completions | Responses |
|---|---|---|
| `image/*` | official `image_url` | official `input_image` |
| `audio/*` | official `input_audio` (must be inline bytes; URLs not accepted) | Override `input_audio` |
| `application/pdf` | official `file` (must be inline) | official `input_file` (Data or URL) |
| `video/*` | Override `video_url` | Override `input_video` |
| other / empty MediaType | `ErrBadRequest`, **the request is not sent** | same as left |

Blocks the official API does not recognize (video) will 400 at the official endpoint — they are not silently dropped. docx/xlsx/pptx are not in this package.

```go
req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
    llm.Text("这段视频里发生了什么"),
    llm.MediaURL("video/mp4", "https://example.com/clip.mp4"),
}})
```

## Streaming Contract

- ctx cancellation: emit `EventError(canceled)` first, then close the channel
- Streaming audio: each chunk is independent base64 — **decode before concatenating bytes** (concatenating strings first and then decoding misaligns due to padding)
- transcript may only appear in the final chunk; the pump accumulates it into the text block delivered with done
- The MIME for `pcm` / `pcm16` is labeled `audio/pcm` (not the invalid `audio/pcm16`)

## Request-Level Parameters

Common vocabulary fields: `Temperature` / `TopP` / `MaxTokens` / `StopSequences` / `ResponseFormat` / `Audio` / `Reasoning` / `Output` / `ToolChoice.Parallel`.

| Field | Completions | Responses |
|---|---|---|
| `Reasoning.Effort` | `reasoning_effort` | `reasoning.effort` |
| `Output.Verbosity` | `verbosity` | `text.verbosity` |
| `Output.Logprobs` / `TopLogprobs` | `logprobs` / `top_logprobs` (TopLogprobs automatically implies logprobs=true) | only `top_logprobs`; setting Logprobs=true alone gets an explicit bad_request |
| `ToolChoice.Parallel` | `parallel_tool_calls` | same as left |
| `TopK` | no official field → **explicit bad_request** (no JSON injection for the sake of compatible gateways) | same as left |

`Config.Options` is **client-level** configuration (organization / project / timeout_seconds / max_retries / headers); it does not enter the request body.

OpenAI-specific sampling and routing long-tail — `seed`, `frequency_penalty` / `presence_penalty`, `logit_bias`, `store`, `prompt_cache_key`, `safety_identifier`, `service_tier`, `user` — is **not in the vocabulary** (no cross-provider semantics). When needed, connect to the adapter directly or go through provider-specific request types.

## Errors

| Condition | Kind |
|---|---|
| 401 / 403 | `auth` |
| 429 / error code containing `rate_limit` | `rate_limit` |
| 404 | `no_model` |
| message text containing context length / prompt too long | `context_length` |
| error code containing `content_filter` | `content_filter` |
| 400 / 405 / 422 | `bad_request` |
| 5xx / `server_error` | `provider` |
| `context.Canceled` | `canceled` |
| transport failure | `network` |

## Testing and Live API

Offline (httptest, no network):

```
go test -race -skip TestLive ./llm/openai
```

Live smoke tests: `Skip` when the corresponding environment variable is missing. Credentials go into the repo root `.env` (gitignored); `TestMain` only fills in variables not already set.

| Variable | Purpose |
|---|---|
| `PULSE_OPENAI_API_KEY` | Completions / Responses live tests (required to run) |
| `PULSE_OPENAI_BASE_URL` | Overrides the endpoint, e.g. MiniMax `https://api.minimaxi.com/v1` |
| `PULSE_OPENAI_MODEL` | defaults to `gpt-4o-mini` |
| `PULSE_OPENAI_SKIP_RESPONSES` | skips Responses when set to `1` |
| `PULSE_MIMO_API_KEY` | MiMo TTS / ASR round trip |
| `PULSE_MIMO_BASE_URL` | e.g. `https://api.xiaomimimo.com/v1` |

Application code just uses each provider's official variable names (`OPENAI_API_KEY` etc.); do **not** put the test-gating `PULSE_*` into production configuration.

Verified with MiniMax-M3: Completions (text/stream/tools/images/video) and Responses (text/stream/tools/images); verified with MiMo: TTS synthesizing a wav and feeding it into ASR.

## Deliberately Pinned

- SDK automatic retry is off by default.
- Input-side reasoning is not echoed back.
- No vendor-proprietary fields are written (`thinking` / `reasoning_split` / `service_tier`). Gateway extensions are fine as long as they do not break the generic path (e.g. `video_url`).

## Export Overview

This package exposes only the registration entry point and the two factories; `completionsModel` / `responsesModel` are unexported.

| Symbol | What it does | How to use |
|---|---|---|
| `ProviderCompletions` | `"openai"` | `Config.Provider` |
| `ProviderResponses` | `"openai-responses"` | same as above |
| `Register` | Registers the two factories with the Registry | `openai.Register(scope, reg)`, internally two `RegisterProvider` calls |
| `NewCompletions` | Completions factory | `llm.Factory` signature; can also be called directly as `NewCompletions(cfg)` without the Registry |
| `NewResponses` | Responses factory | same as above |

`Generate` / `Stream` live on unexported implementation types and satisfy `llm.ChatModel`. For wire formats, MIME, and error mapping see the sections above.

## Out of Scope

`PreviousResponseID` / Conversations, built-in tools (web_search etc.), the standalone endpoints `/v1/audio/speech` and `/v1/audio/transcriptions`, Azure/Bedrock-specific signing.
