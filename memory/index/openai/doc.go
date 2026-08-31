// Package openai 是 index.EmbeddingProvider 的 OpenAI 兼容适配器：
// openai-go SDK 薄包装（与 llm/openai 同源依赖，v3），面向官方
// /v1/embeddings 线格式——vLLM/Ollama/各类 OpenAI 兼容网关通用
// （option.WithBaseURL 指向即可）。
//
// 包名与 llm/openai 同名不同路径——import 时建议别名，例如
//
//	indexopenai "github.com/Luo-root/pulse/memory/index/openai"
//
// # 职责边界
//
//   - SDK 拥有线格式、传输与内置退避（Retries > 0 才启用；默认 0 =
//     不静默重试，对齐 llm/openai 先例——重试与 failover 属上层编排，
//     embeddings 的上层兜底 = AsyncIndexer 丢弃计数 + Rebuild 重建）；
//   - 适配器只做：Config 映射 → 批量分批 → 超长截断（textsplit 选
//     分隔符边界切点，截断经 OnTruncate 可观测）→ 形状校验（形状错
//     包 index.ErrProviderShape）；
//   - **维度校验不进适配器**：维度由首次成功 embed 钉死、不符
//     ErrDimsMismatch 是 index 的 fail closed 职责，本包不重复造。
//
// # API key 纪律
//
// APIKey 由宿主从 env/配置注入——不落库、不提交、不打日志；本包
// 错误信息不携带 key。
//
// 设计全貌见 docs/design/memory-layer-research-and-v2-design.md
// §8.2/§12 P2-D；实现票 #86（D1.5）。
package openai
