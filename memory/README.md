# memory

P2「记忆与会话」层（设计事实源：[docs/design/memory-layer-research-and-v2-design.md](../docs/design/memory-layer-research-and-v2-design.md)，Accepted）。

核心立场：**event-sourced**——append-only 会话日志是唯一真相，模型 surface 只是投影；
**model-visible means logged**——凡给模型看的（含崩溃恢复合成的结果）必须真实写回日志；
压缩是「追加 + surface replace」，不删原始证据；记忆管理权默认在宿主管线 + 审批。

## 子包

| 包 | 状态 | 职责 |
|---|---|---|
| `session` | **已落地**（P2-A1 in-memory + P2-A2 JSONL，Issue #68/#70） | 会话事件日志契约、事件分级 codec registry、surface fold（§6.3 映射表）、in-memory store、JSONL 持久层（blobs 溢出 + 文件锁 + 撕裂恢复）、Open 即冷恢复 |
| `store` | 未建（P2-C） | 长期记忆 item canonical store（Namespace/SourceRefs/Status/Taint，Supersede/Revoke，禁物理 DELETE） |
| `assemble` | 未建（P2-C） | Context Assembler：渐进披露装配（摘要常驻小预算 + 正文按需检索） |
| `compaction` | 未建（P2-B） | Token meter + compaction（append + surface replace 事务） |
| `index` | 未建（P2-D） | FTS / 向量索引 provider（派生索引，可重建） |

## 依赖方向（评审定案，不可违反）

```text
memory/session   → kernel + llm          （禁止 import loop / observability）
memory/compaction → memory/session
memory/store     → （独立；llm 不依赖）
memory/assemble  → memory/session + memory/store
memory/index     → memory/store
```

- service key 归 `memory/*` 各包（对齐 `toolset.ServiceKey` 先例）；**kernel 不 import memory，loop 不 import memory**。
- session→loop 的接线（把 `loop.Run` history 与 `session.Surface()` 对上）由**装配层桥**做，不进本层。
