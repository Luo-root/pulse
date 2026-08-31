// Package index 是 P2-D 的派生检索索引：embedding provider seam +
// 向量索引（内存版）。canonical 永远在 memory/store——索引可丢可重建。
//
// # 定位与不变式
//
//   - 向量仅派生索引（§6.5）：索引只存 item_id → (vector, namespace)
//     拷贝，`Rebuild` 全量从 store 重 embed；删除或重建索引不损失任何
//     canonical item（验收钉）。
//   - namespace 先过滤再召回（§8.2）：授权过滤在相似度计算之前——兄弟
//     namespace 的 item 向量再近也不出现在结果，不泄漏存在性。
//   - EmbeddingProvider 是宿主注入的 seam：不进 llm 词汇表（embedding
//     非跨 provider 稳定生成语义），不进 kernel；维度由 provider 决定，
//     索引按维度校验（不符 fail closed）。
//   - 状态同步靠写入方：import 图 index → store 单向，store 不知道
//     index 存在；Upsert/Remove 由宿主/装配层在 Put/Supersede/Revoke
//     后调用，索引只放 Active item。
//
// # 分层纪律
//
// import memory/store + kernel（仅 ServiceKey）；不 import llm/session/
// assemble。embed 输入 = Content（Structured 领域载荷不进向量——与
// C2/C3「Structured 不入检索域」口径一致）。
//
// # 接入姿态
//
//	idx, err := index.NewMemIndex(memStore, myProvider) // provider 宿主注入
//	_ = idx.Upsert(ctx, item)                           // 写入方在 store.Put 后调
//	hits, _ := idx.Search(ctx, ns, "deploy", 10)        // 先过滤再 top-k
//	async, err := index.NewAsyncIndexer(idx, 64)        // 异步：写路径不阻塞
//	defer async.Close(ctx)
//
// 设计全貌见 docs/design/memory-layer-research-and-v2-design.md §6.5/
// §8.2/§12 P2-D；实现票 #84（D1）。hybrid retrieval 接 assemble 在 D2。
package index
