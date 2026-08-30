// Package session 是 P2 记忆层的第一块地基：event-sourced 的会话事件日志。
//
// # 定位与不变式
//
//   - append-only 事件日志是唯一真相；surface 只是投影，每个模型可见
//     节点都能定位到 canonical event；
//   - model-visible means logged：凡是要给模型看的（含崩溃恢复合成的
//     IsError 结果 / interrupted 结束事件），必须真实 Append 进日志；
//   - 压缩是「追加 + surface replace」，不删原始证据（P2-B 落地）。
//
// # 分层纪律
//
// 只 import kernel + llm（禁止 import loop / observability）；kernel 与
// loop 不反向 import 本包。service key（SessionStoreKey）归本包，对齐
// toolset.ServiceKey 先例。session→loop 的接线（把 loop.Run 的 history
// 与 session.Surface() 对上）由装配层桥做，不进本包。
//
// # 接入姿态
//
//	store := session.NewMemoryStore()          // A1：内存实现
//	store, _ := session.NewJSONLStore(root)    // A2：JSONL + blobs + 文件锁
//	sess, _ := store.Create(ctx, session.SessionHeader{})
//	env, _ := sess.Append(ctx, session.EventDraft{Type: ..., Data: ...})
//	surface, _ := sess.Surface(ctx)            // []*llm.Message，交给 loop.Run 的 history
//
// Surface 直接产出 []*llm.Message，不含 system（归宿主/Assembler），
// assistant.chunk 永不进 surface，Parts 原样保留（含 PartToolCall）。
//
// # Open 即冷恢复
//
// 没有独立 Recover 方法：Open 非 live 会话时发现未闭合 turn/step 或
// unpaired ToolCall → 合成 `tool.result(IsError, "interrupted")` /
// `turn.ended(interrupted)` 真实 Append 写回日志后再 fold；live 会话
// 不做冷补；恢复幂等。
//
// # 事件分级与裁决表
//
// 类型经 Registry 绑定 codec 与分级（Required / Ignorable）：
//
//	已知 Required   —— 永不跳过（忽略信封上的 Ignorable flag）
//	已知 Ignorable  —— 可跳过；fold 不读它
//	未知 + Ignorable=true  —— 跳过
//	未知 + flag 默认 false —— Append 即拒绝（比「拒绝 Open」更早的 fail closed）
//
// Ignorable ≠ 可以不记：request.header 标 Ignorable 只表示「fold 不需
// 要它」，写入方仍必须发（system + ToolDef + model 三样）。
//
// # 两个 backend
//
//   - NewMemoryStore：完整 §7.1 接口，进程内单写者，Flush 为语义占位；
//   - NewJSONLStore：JSONL 落盘（撕裂恢复）、blobs 溢出（32KiB，内容寻址）、
//     文件锁（O_EXCL + stale 抢占 + Flush 心跳）、Flush 真 fsync。
//
// JSONL 为明文：文件即密钥面、路径宿主拥有；`blob:` URL 前缀为本包
// 保留。行为细节见本包 README_zh.md。
//
// 设计全貌（数据契约 §6 / 接口边界 §7 / 压缩与恢复 §9 / 分票 §12）见
// docs/design/memory-layer-research-and-v2-design.md；实现票 #68（A1）、
// #70（A2）。
package session
