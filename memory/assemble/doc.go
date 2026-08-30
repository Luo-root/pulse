// Package assemble 是 P2-C 的上下文装配层：把 stable memory、检索型
// 记忆与 session surface 组装成带预算边界的模型请求序列。
//
// # 定位与不变式
//
//   - 预算按类配置（§8.1）：稳定记忆小固定预算（超限省略并记诊断——
//     不静默丢）、检索记忆动态预算（超限降 top-k）、surface 尾部保留
//     完整合法尾部（超限只诊断不裁切——裁切归 compaction/prune）；
//   - 预算可解释：每次组装的省略/降级都落在 Diagnostics 里；
//   - stable snapshot policy（§8.3）：frozen profile 默认缓存（同
//     namespace 复用，保 cache），RefreshStable 显式重建；
//   - 本轮立即应用的记忆走 Injected（明确 session injected context，
//     紧贴当前消息），不修改已缓存稳定前缀；
//   - 检索排序确定性（P2-C）：keyword 命中优先 + recency + taint 降权，
//     不依赖 Confidence（w_conf 权重 0 是 P2-D 的事）；semantic 归 P2-D；
//   - 每条注入记忆带 SourceRefs 可读引用模板——避免模型把低置信候选
//     当成无条件事实（§8.2）。
//
// # 分层纪律
//
// import memory/store + llm + kernel（仅 ServiceKey）；**不 import
// memory/session**——Surface 由宿主从 session.Surface() 取好传入，组装
// 器不感知 session 包。TokenCounter 由宿主注入（不 import compaction，
// meter 复用走装配层接线票）。检索召回优先 store 的可选 FTS 能力（类型
// 断言 SearchFTS），不支持则回退 Search 子串——两路口径差异见 README_zh.md。
//
// # 接入姿态
//
//	a := assemble.NewDefaultAssembler(memStore, nil, assemble.Budget{
//	    StableMemoryTokens: 300, RetrievedTokens: 200, MaxSurfaceTail: 40,
//	})
//	ac, err := a.Assemble(ctx, assemble.AssembleInput{
//	    Namespace: scope.Namespace(), Surface: surface,
//	    Query: userText, Injected: injected,
//	})
//	// ac.Messages / ac.StablePrefixLen / ac.Diagnostics
//
// 设计全貌见 docs/design/memory-layer-research-and-v2-design.md §8；
// 实现票 #80（C3）。使用细节见 README_zh.md。
package assemble
