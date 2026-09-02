[English](README.md) | [中文](README_zh.md)

# memory/index/openai

An OpenAI-compatible adapter for `index.EmbeddingProvider` (Issue #86, D1.5): a thin wrapper over the openai-go SDK (same-origin dependency as `llm/openai`), speaking the official `/v1/embeddings` wire format — vLLM/Ollama and other OpenAI-compatible gateways just need `BaseURL` pointed at them.

> The package name collides with `llm/openai` (same name, different path); alias it on import:
> `indexopenai "github.com/Luo-root/pulse/memory/index/openai"`.

## Integration

```go
provider, err := indexopenai.New(indexopenai.Config{
    // BaseURL: "http://localhost:11434/v1", // vLLM/Ollama/网关；空 = 官方端点
    Model:  "text-embedding-3-small",        // 必填
    APIKey: os.Getenv("OPENAI_API_KEY"),     // 必填；env 注入，不落库不打日志
    // BatchSize: 64,                        // 单请求条数上限（默认 64）
    // MaxInputChars: 8000,                  // 单条输入字符预算（默认 8000）
    // Retries: 0,                           // 默认 0 = 不静默重试（见下）
    // OnTruncate: func(original, kept int) { /* 可观测 */ },
})
idx, err := index.NewMemIndex(memStore, provider)
```

## Responsibility boundaries

- **Owned by the SDK**: wire format, transport, built-in backoff. `Retries` defaults to 0 = off (aligned with the `llm/openai` precedent: retries and failover belong to upper-layer orchestration; the upper-layer fallback for embeddings = `AsyncIndexer` drop counting + `Rebuild`); the host explicitly sets `Retries > 0` to enable SDK exponential backoff + Retry-After.
- **Owned by the adapter**: batch splitting (64/request by default, output realigned to the original order via the response `Index`), oversized-input truncation ([textsplit](../../../textsplit/README.md) picks separator-boundary cut points + the `OnTruncate` callback), shape validation (vector count mismatch / empty vectors / out-of-range index → wraps `index.ErrProviderShape`).
- **Owned by index**: dimension fail-closed (`ErrDimsMismatch`, pinned at the first embed) — the adapter does not duplicate it.

## Truncation semantics

Under the one-item-one-vector model, truncating oversized input means the tail content becomes **silently invisible** to vector recall (the canonical copy in the store stays complete and unaffected). `MaxInputChars` defaults to 8000 (the text-embedding-3 family caps input at 8191 tokens; CJK ≈1-2 chars/token, English ≈4 — prefer short over wrong); adjust at deployment time to the actual limit of the configured model, and it is recommended to wire `OnTruncate` for observability. Characters ≠ tokens; an exact token budget needs a tokenizer, which this package does not import.

## Tests

```bash
go test -race -count=1 ./memory/index/openai/...
# live smoke（可选，三个 env 齐全才跑）：
#   PULSE_OPENAI_EMBED_API_KEY / PULSE_OPENAI_EMBED_MODEL / PULSE_OPENAI_EMBED_BASE_URL
go test -run TestLiveEmbed ./memory/index/openai/ -v
```
