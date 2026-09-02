# 记忆层

`memory/` 是 P2 记忆与会话层，九个子包覆盖从回合日志到长期记忆的完整链路。核心不变式：**model-visible means logged**——模型看到的一切都会写进仅追加的会话日志。

## 全景

| 子包 | 职责 | 阶段 |
|---|---|---|
| `memory/session` | 会话核心：事件信封、codec registry、surface fold、JSONL store + blobs + 文件锁 | P2-A |
| `memory/compaction` | Token 计量 + §9.1 八步事务压缩 + pruning | P2-B |
| `memory/store` | 长期记忆存储：namespace 隔离 + Supersede/Revoke + CAS；SQLite/FTS5 后端（build tag 隔离） | P2-C |
| `memory/assemble` | Context Assembler：按类预算 + stable snapshot + 引用模板 + 混合融合排序 | P2-C3 |
| `memory/selfedit` | self-edit 记忆工具组（put/supersede/revoke），显式 opt-in | P2-C4 |
| `memory/index` | 派生向量索引：EmbeddingProvider seam + 先过滤再召回 + 异步队列 | P2-D1 |
| `memory/index/openai` | OpenAI 兼容 embeddings 适配（SDK 薄包装） | P2-D1.5 |
| `memory/candidate` | 候选管线：extractor → 去重 → pending approval 状态机 | P2-D3 |
| `memory/reflection` | 可配置后台反思：预算截断 + 编排提炼，默认关 | P2-D4 |

## 设计立场

- **禁止物理 DELETE**：长期记忆只 Supersede（取代）/ Revoke（撤销），审计链不断；
- **KnownAt 摄取时间**：区分「发生时间」与「系统知道的时间」，检索按 bi-temporal 语义过滤；
- **渐进披露**：stable memory 注入用「摘要常驻小预算 + 正文按需检索」，禁全量注入；
- **管理权在宿主**：self-edit 工具显式 opt-in，候选采纳走审批（对齐 HITL 立场）——不是模型全自主；
- **图谱后端不进核心**：只留 provider 对接口。

## 会话（session）

会话是事件溯源日志：append-only 的 `EventEnvelope` 流 + 按类型注册的 codec + fold 投影。关键语义：

- 完整行（含 `\n`）= 成功 append；撕裂行在冷恢复时重建；
- Open 即冷恢复：合成事件写回 log，保证重放一致；
- 配对主键 = assistant 消息上的 `PartToolCall`（工具调用与结果配对）；
- 单写者：进程内锁 + 文件锁 `O_EXCL`（stale 检测看 mtime，Flush 兼作心跳）；
- 大载荷（>32KiB）溢出到 blob 存储，带 sha 自校验。

## 压缩（compaction）

`CharMeter` 计量 → 达阈值触发 §9.1 八步事务：started → 摘要 → checkpoint 写入（FormatVersion 抬升）→ ended。checkpoint 的 fold Role 是 `user`（不伪装 system），pruning 按「不新增孤儿」四规则校验——预检与 fold 重放同口径。

## 上下文装配（assemble）

按类预算（system / memory / history / tools 分账）+ stable snapshot + 引用模板，检索结果用 D2 hybrid 融合排序（语义通道走函数 seam，不依赖 index 包）。

## 检索与反思（index / candidate / reflection）

- **index**：先过滤（scope / namespace / 预算）再向量召回，异步队列解耦写入；EmbeddingProvider 是 seam——内置内存版与 OpenAI 适配器可替换；
- **candidate**：记忆候选走 Extract → 双归一去重 → Pending 入库 → 人工 Approve（=Supersede）/ Reject（=Revoke）；外部来源默认 untrusted-external 污染标记；
- **reflection**：预算截断 + 候选提炼编排，无后台循环、默认关闭——开不开由宿主决定。

详见 [memory 包文档](/packages/memory/)（全局地图：数据流、依赖清单、跨包不变式、桥接点）。
