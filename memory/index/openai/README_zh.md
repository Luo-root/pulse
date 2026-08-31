# memory/index/openai

`index.EmbeddingProvider` 的 OpenAI 兼容适配器（Issue #86，D1.5）：openai-go SDK 薄包装（与 `llm/openai` 同源依赖），官方 `/v1/embeddings` 线格式——vLLM/Ollama/各类 OpenAI 兼容网关用 `BaseURL` 指向即可。

> 包名与 `llm/openai` 同名不同路径，import 时建议别名：
> `indexopenai "github.com/Luo-root/pulse/memory/index/openai"`。

## 接入

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

## 职责边界

- **SDK 拥有**：线格式、传输、内置退避。`Retries` 默认 0 = 关闭（对齐 `llm/openai` 先例：重试与 failover 归上层编排；embeddings 的上层兜底 = `AsyncIndexer` 丢弃计数 + `Rebuild` 重建）；宿主显式 `Retries > 0` 启用 SDK 指数退避 + Retry-After。
- **适配器拥有**：批量分批（默认 64/请求，输出按响应 `Index` 对齐回原顺序）、超长截断（[textsplit](../../../textsplit/README_zh.md) 取分隔符边界切点 + `OnTruncate` 回调）、形状校验（向量数不符 / 空向量 / index 越界 → 包 `index.ErrProviderShape`）。
- **index 拥有**：维度 fail closed（`ErrDimsMismatch`，首次 embed 钉死）——适配器不重复造。

## 截断语义

一 item 一向量模型下，超长输入截断 = 尾部内容在向量召回中**静默不可见**（canonical 在 store 完整，不受影响）。`MaxInputChars` 默认 8000（text-embedding-3 系输入上限 8191 token；CJK ≈1-2 字符/token、英文 ≈4——宁短勿错）；部署时按所配模型实际上限调整，建议配 `OnTruncate` 观测。字符 ≠ token；精确 token 预算需 tokenizer，本包不引。

## 测试

```bash
go test -race -count=1 ./memory/index/openai/...
# live smoke（可选，三个 env 齐全才跑）：
#   PULSE_OPENAI_EMBED_API_KEY / PULSE_OPENAI_EMBED_MODEL / PULSE_OPENAI_EMBED_BASE_URL
go test -run TestLiveEmbed ./memory/index/openai/ -v
```
