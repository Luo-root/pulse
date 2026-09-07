package llm

// 观测 attrs key 契约（归属 llm 层）：事件折叠为 observability.Record
// 时，llm 事实的 key 由本包定义、装配层桥执行——字段语义的知识留在
// 事实归属包，observability 只提供信封与出口（双层观测模型见
// observability-v1-design.md）。
//
// key 约定 <组件>.<字段> 点分，各组件独立 key 空间互不冲突。
const (
	// AttrModel 是产生响应的模型标识（取自 Response.Model）。
	AttrModel = "llm.model"
	// AttrTokensIn 是输入 token 数（after_response 为单次口径）。
	AttrTokensIn = "llm.tokens_in"
	// AttrTokensOut 是输出 token 数（after_response 为单次口径）。
	AttrTokensOut = "llm.tokens_out"
	// AttrTokensCached 是命中缓存的输入 token 数（部分 provider 提供，
	// 无缓存命中时为 0）。
	AttrTokensCached = "llm.tokens_cached"
)
