// Package compaction 是 P2-B 的压缩层：token meter、§9.1 八步压缩事务
// 编排、§9.2 tool result deterministic pruning。
//
// # 定位与不变式
//
//   - 压缩是事务不是删除：raw log 只增不减；Replace 窗口的 source refs
//     完整落进 `compaction.checkpoint.Replaced`，重放可追溯；
//   - 不会打断 tool pairing：选区与 Fork 切边同一口径（整组移动），
//     编排前预检 + fold 重放复核（session.ValidateReplaceWindow）；
//   - checkpoint 折成稳定前缀消息（压缩摘要 = RoleUser），事件类型
//     `compaction.checkpoint` 不得伪装 `message.user`（评审定案）；
//   - 失败留审计：Summarize 失败时 started 已落盘、无 checkpoint、
//     ended 不写——未闭合 compaction 在日志里保持可见（恢复不假装完成）。
//
// # 依赖方向
//
// memory/compaction → memory/session + llm（§7.1 import 图）；kernel 与
// loop 不 import 本包。压缩触发后的请求重试（overflow retry）是装配层
// 编排，本包只提供 `Pressure` 检测与手动 `Compact` 入口。
//
// # 接入姿态
//
//	sess := ... // session.Session（Memory 或 JSONL store）
//	rep, err := compaction.Compact(ctx, sess, compaction.Options{
//	    Engine: &compaction.LLMSummarizer{Model: model, ModelName: "gpt-test"},
//	    ModelName: "gpt-test",
//	})
//	// rep.Replaced = 被替代窗口的 source seqs；checkpoint 已 Replace surface
//
// 设计全貌见 docs/design/memory-layer-research-and-v2-design.md §8/§9；
// 实现票 #73。
package compaction
