# memory

P2「记忆与会话」层（设计事实源：[docs/design/memory-layer-research-and-v2-design.md](../docs/design/memory-layer-research-and-v2-design.md)，Accepted）。

核心立场：**event-sourced**——append-only 会话日志是唯一真相，模型 surface 只是投影；
**model-visible means logged**——凡给模型看的（含崩溃恢复合成的结果）必须真实写回日志；
压缩是「追加 + surface replace」，不删原始证据；记忆管理权默认在宿主管线 + 审批。

## 子包

| 包 | 状态 | 职责 |
|---|---|---|
| `session` | **已落地**（P2-A1 in-memory + P2-A2 JSONL + P2-B Replace fold，Issue #68/#70/#73） | 会话事件日志契约、事件分级 codec registry、surface fold（§6.3 映射表 + checkpoint Replace）、in-memory store、JSONL 持久层（blobs 溢出 + 文件锁 + 撕裂恢复）、Open 即冷恢复、`FoldTrace` 溯源 |
| `compaction` | **已落地**（P2-B，Issue #73） | token meter（CharMeter 估算 + Pressure）、CompactionEngine seam（LLM / deterministic backend）、§9.1 八步压缩事务编排、§9.2 tool result deterministic pruning |
| `store` | **已落地**（P2-C 内存版 + SQLite/FTS5，Issue #76/#78） | MemoryItem canonical store：Namespace 前缀隔离（scope helper 展开）、Supersede/Revoke 状态机（禁物理 DELETE）、revision CAS、SourceRefs 强制回链、Search 过滤、SQLite + FTS5（CGO-free，build tag 排除 plan9/js） |
| `assemble` | **已落地**（P2-C3，Issue #80） | Context Assembler：按类预算（稳定记忆/检索/surface，诊断可解释）、稳定前缀缓存（§8.3 frozen snapshot）、确定性排序（taint/recency，不依赖 Confidence）、引用模板注入 |
| `selfedit` | **已落地**（P2-C4，Issue #82） | self-edit 记忆工具组：`memory_put`/`memory_supersede`/`memory_revoke`（模型参数最小化、scope env 钉死、OriginFn 回链强制、Preview opaque 卡片），显式 opt-in 注册 |
| `index` | **已落地**（P2-D1 内存向量索引，Issue #84） | 派生向量索引（EmbeddingProvider seam，可丢可重建）：namespace 先过滤再召回、余弦 top-k、命中复核 Active、异步队列（满丢弃计数 + Rebuild 兜底）；`openai/` 适配器（P2-D1.5，Issue #86）：OpenAI 兼容 embeddings（SDK 薄包装 + textsplit 截断 + OnTruncate 可观测）；hybrid 接 assemble 在 D2 |

## 依赖方向（评审定案，不可违反）

```text
memory/session   → kernel + llm          （禁止 import loop / observability）
memory/compaction → memory/session
memory/store     → （独立；llm 不依赖）
memory/assemble  → memory/session + memory/store
memory/selfedit  → memory/store + toolset + llm（opt-in 写工具组；不直接 import loop）
memory/index     → memory/store
memory/index/openai → memory/index + textsplit
```

- service key 归 `memory/*` 各包（对齐 `toolset.ServiceKey` 先例）；**kernel 不 import memory，loop 不 import memory**。
- session→loop 的接线（把 `loop.Run` history 与 `session.Surface()` 对上）由**装配层桥**做，不进本层。

## JSONL backend（P2-A2）注意事项

- **明文存储**：JSONL/blobs/header 都是明文——文件即密钥面、路径宿主拥有；P2-A 不做加密。
- **`blob:` URL 前缀为本包保留**：引用形态用 `URL = "blob:<sha256>"` 标识溢出字节。宿主自带 `blob:` 开头的 `Image.URL`/`Media.URL` 会在载入时被误当内部引用还原，请改用其它 scheme。
- **文件锁心跳**：`Flush` 兼作锁心跳（touch 锁文件 mtime）。长命会话只要定期 Flush 就不会被 stale 抢占；从不 Flush 的会话持锁超过阈值（默认 1h，`JSONLStale` 可配）可能被另一进程接管。锁文件创建后立即关闭句柄、锁只靠存在性——若将来改为持句柄锁，Windows 的抢占路径会失效。
- **List 与损坏目录**：header.json 无法解析的目录从 `List` 结果消失（并发创建中/无关目录同理）；对应会话要等 `Open` 才报 `ErrCorruptLog`。
- **跨进程 Delete**：常态下不静默丢数据——Unix unlink 立即生效，对方进程的 Append 靠写前 stat 校验返回 `ErrDeleted`（stat 与 Write 之间仅剩微 TOCTOU 竞态窗口，库层无法根除）；Windows 上打开中的文件删不掉（Go 句柄无 FILE_SHARE_DELETE），`RemoveAll` 会先删掉能删的部分（blobs/、header.json）再失败留下半删目录，由宿主协调、对方释放句柄后重试即可完成。两条路都不做「报成功但丢数据」。
- **与 in-memory 的行为差**：A2 的 `Open` 对缓存命中（同进程重复打开）不做冷恢复——live 会话不被误补写；恢复路径是 `Close → Open`（重新载入磁盘日志时触发）。
