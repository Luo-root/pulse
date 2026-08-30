// Package store 是 P2-C 的长期记忆 canonical store：MemoryItem 的
// Put/Get/Search/Supersede/Revoke。
//
// # 定位与不变式
//
//   - Namespace 是 canonical 键：权限、隔离、检索都走 Namespace；
//     MemoryScope 只是构造 helper（展开成 Namespace 层级），不与
//     Namespace 并存两套真相。可见性 = Namespace 前缀匹配：父 scope
//     读得到子 scope 的 item，兄弟 scope 绝不互见。
//   - 禁止物理 DELETE（§17.7-1）：接口无 Delete；同一事实的更新走
//     Supersede（旧 item Status=Superseded，新 item 入库），撤销走
//     Revoke（Status=Revoked + 审计）——两者都可追溯。
//   - SourceRefs 是防幻觉的根：每条 active 记忆必须回链 session/event
//     或人工输入；无来源的 Put 被拒（§10.2）。
//   - 并发写用 revision CAS（§13.1：MemoryItem CAS 是 P2-C 验收；
//     session 单写者锁是 P2-A 的事，两者不相干）。
//   - Confidence 检索排序不依赖：P2-C 无 scoring 产出方，写入方必须
//     显式提供（scoring 属 P2-D）。
//   - 状态迁移只有 Supersede/Revoke 两条路：Put 更新禁止改变 Status
//     （否则 active→pending 绕过 P2-D 的 taint gate、active→superseded
//     绕过替代链——ErrStatusTransition）。
//
// # 分层纪律
//
// 不 import llm / session（§7.1 import 图：memory/store 独立，连 llm 都
// 不依赖）；kernel 仅借 ServiceKey 机制（§7.2 / toolset 先例——kernel 不
// import memory，反向引用只到 kernel.NewServiceKey）。service key
// （MemoryStoreKey）归本包。
//
// # 接入姿态
//
//	store := store.NewMemoryStore() // C1：内存实现（SQLite + FTS 在 C2）
//	scope := store.MemoryScope{TenantID: "acme", UserID: "u1"}
//	it, err := store.Put(ctx, store.MemoryItem{
//	    ID: "d1", Namespace: scope.Namespace(), Kind: store.KindDecision,
//	    Content: "prefer toml", Status: store.StatusActive,
//	    Confidence: 1.0, Taint: store.TaintTrusted,
//	    SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: sid, Seq: 7}},
//	}, store.PutMemoryOptions{})
//	hits, err := store.Search(ctx, store.MemoryQuery{Namespace: scope.Namespace()})
//
// 使用细节（backend 对比、状态机、错误速查表）见 README_zh.md。
//
// 设计全貌见 docs/design/memory-layer-research-and-v2-design.md §6.5/
// §10/§13.1；实现票 #76（C1）、C2（SQLite+FTS）、C3（Assembler）。
package store
