# memory

P2「记忆与会话」层（设计事实源：[docs/design/memory-layer-research-and-v2-design.md](../docs/design/memory-layer-research-and-v2-design.md)，Accepted）。本包是 Pulse v2 的记忆基础设施：**9 个子包覆盖「会话真相 → 上下文投影 → 压缩治理 → 长期记忆 → 检索注入 → 自动提炼 → 审批晋升」全链路**，全部 P2 票（A/B/C/D 四阶段）已落地合入。

每个子包有独立的 `README_zh.md`（接口面/语义/错误速查）与 `doc.go`（godoc）——本文档是**全局视角**：问题梳理、全链路数据流、完整依赖关系、跨包不变式、装配桥接点。读某个子包前先看这里建立地图。

## 这一层解决什么问题

Agent 记忆不是一个「向量数据库 + 对话历史」组件，而是五类彼此独立、通过统一投影协作的数据（设计文 §0）：

| 数据 | 本层落点 |
|---|---|
| 会话事实（Session Journal） | `session`——append-only 事件日志，一次运行的唯一真相 |
| 模型上下文（Model Context Projection） | `assemble`——从日志/记忆/检索计算出受 token 预算约束的请求输入，**不是持久化真相** |
| 跨会话工作记忆（Working/Episodic） | `session` 事件 + `assemble` 检索注入（episode 类 item） |
| 稳定记忆（Semantic/Profile） | `store`（canonical）+ `index`（派生向量）+ `candidate`/`selfedit`（两条写入通道） |
| 程序记忆（Procedural） | 不在本层——`skills/`（Skill ≠ 记忆条目，不混入 facts 表） |

对应要解决的五类问题（§1）：多轮 tool-calling 的精确恢复与调试；跨进程/会话/Agent 的上下文复用；长会话 token 预算控制；用户/项目/Agent 三类作用域隔离；自动记忆的投毒防护（ASI06）。

### 四条铁律（先于一切子包语义）

1. **event-sourced**：append-only 日志是唯一真相，`Surface()` 只是投影；投影可以替换、重建，原始事件永不删改。
2. **model-visible means logged**：凡给模型看的（包括崩溃恢复时合成的闭合事件）必须真实写回日志——投影与日志不一致即为 bug。
3. **压缩是事务不是删除**：压缩/pruning 只追加 + surface replace，`Replaced` 记录被替代窗口的完整溯源，失败留审计不假装完成。
4. **记忆管理权在宿主管线 + 审批（HITL）**：自动记忆只写候选（Pending），审批人盖章才晋升 Active；模型自编辑走显式 opt-in 工具 + `before_tool_call` 审批。没有「全自动记忆」。

## 全链路数据流（一条记忆的生命周期）

```
【运行期】
loop.Run ⇄ 装配层桥 ⇄ session.Append(事件)          ← 每轮消息/工具调用/usage 落日志
                 │
                 ▼
          session.Surface()                          ← fold 投影（checkpoint Replace 已应用）
                 │
                 ▼
          assemble.Assemble                          ← 稳定前缀(frozen) + surface 尾部
                 │                                      + 混合召回(FTS∪向量) + injected
                 ▼
              模型请求                                    ← 预算按类、诊断可解释

【治理期——长会话】
compaction.Pressure ─▶ Compact（§9.1 八步事务）       ← surface 治理，raw log 只增不减
                  └─▶ PruneResults（§9.2）            ← 超长 tool result head+marker+tail

【提炼期——会话末/每 N 轮，宿主触发】
reflection.Reflect（预算截断）─▶ candidate.Extract ─▶ Pending 入库（双归一去重；检索不可见）
                                          │
宿主审批面 ◀── candidate.Pending ─────────┘
   ├─ Approve ─▶ store.Supersede ─▶ Active（Confidence=1.0 + 审批标记）
   └─ Reject ──▶ store.Revoke（reason 落审计）

【索引与召回】
store 写入后 ─▶ index.Upsert（异步队列；Supersede/Revoke 后 Remove）
下一轮 assemble 召回：keyword(FTS/子串) ∪ semantic(向量) —— 只有 Active 可见

【模型自编辑通道（opt-in）】
模型 ─▶ selfedit 三工具（memory_put/supersede/revoke，HITL 审批）─▶ store 直写 Active（taint 保守标记）
```

两条写入通道的安全差异是刻意的：**自动提炼**（reflection→candidate）产出永远待审（Pending 不可见）；**模型自编辑**（selfedit）直写 Active 但 taint 默认 `untrusted-external` 且每笔过 HITL 审批——前者靠不可见性隔离，后者靠信任标记 + 审批闸隔离。

## 子包总览

| 包 | 票/阶段 | 职责一句话 | 必填 seam（宿主注入） | 默认状态 |
|---|---|---|---|---|
| [`session`](session/README_zh.md) | #68/#70/#73（A1+A2+B） | append-only 事件日志 + fold 投影 + 冷恢复；内存/JSONL 两 backend | 无（Registry 可扩展事件） | 基础设施，无开关 |
| [`compaction`](compaction/README_zh.md) | #73（B） | token meter + §9.1 八步压缩事务 + §9.2 tool result pruning | `Engine`（LLM/Deterministic） | 手动入口，无自动触发 |
| [`store`](store/README_zh.md) | #76/#78（C1+C2） | MemoryItem canonical store：namespace 隔离 + Supersede/Revoke 状态机 + SQLite/FTS5 | 无 | 内存版即用；SQLite 按 DSN 启用 |
| [`assemble`](assemble/README_zh.md) | #80/#88（C3+D2） | 上下文装配：按类预算 + 稳定前缀缓存 + §8.2 hybrid 融合排序 + 引用模板 | `TokenCounter`（nil 估算）、`Semantic`（可选向量路） | nil seam = keyword-only 可用 |
| [`selfedit`](selfedit/README_zh.md) | #82（C4） | self-edit 记忆工具组（put/supersede/revoke），模型可见写路径 | `OriginFn`、`toolset.Registry` | **显式 opt-in 注册** |
| [`index`](index/README_zh.md) | #84/#86（D1+D1.5） | 派生向量索引（可丢可重建）+ 异步队列 + 计数装饰器；`openai/` 适配器 | `EmbeddingProvider` | 不接 = 无向量召回，功能不缺 |
| [`candidate`](candidate/README_zh.md) | #90/#91（D3） | 候选管线：extractor → 双归一去重 → Pending → 审批晋升/否决；`Metrics()` | `Extractor`、`OriginFn` | **默认关**（无后台循环） |
| [`reflection`](reflection/README_zh.md) | #92/#93（D4） | 可配置 background reflection：输入预算截断 → 候选提炼 → 计数 | 无新增（复用 candidate 的） | **默认关**（无后台循环/计时器） |

设计取向：**每一层都可独立缺席**——不配 index 就 keyword-only，不配 candidate/reflection 就无自动记忆，不配 selfedit 就模型不可写记忆；只有 session+store 是不可省的地基。

## 完整依赖关系

实际 import 关系（已核实，2026-08-31；`go list` 可复核）：

```text
memory/session        → kernel, llm
memory/store          → kernel
memory/compaction     → llm, memory/session
memory/assemble       → kernel, llm, memory/store
memory/selfedit       → kernel, llm, memory/store, toolset
memory/index          → kernel, memory/store
memory/index/openai   → memory/index, textsplit
memory/candidate      → kernel, llm, memory/store
memory/reflection     → kernel, llm, memory/candidate, memory/store
```

内部依赖分层（箭头只允许向下）：

```text
【投影与治理】  compaction ──────────────▶ session
【上下文组装】  assemble ─────────────────▶ store
【模型写通道】  selfedit ─────────────────▶ store
【召回索引】    index ────────────────────▶ store        index/openai ─▶ index + textsplit
【自动提炼】    reflection ─▶ candidate ──▶ store
```

依赖规则（评审定案，不可违反）：

- **kernel 不 import memory，loop 不 import memory**——memory 是 capability seam / plugin 接入 `kernel.Context`（service key 归 `memory/*` 各包：`SessionStoreKey` / `MemoryStoreKey` / `ContextAssemblerKey` / `VectorIndexKey` / `PipelineKey` / `ReflectorKey`）；装配层把 `session.Surface()` 交给 `loop.Run`。
- **store 不知道 index 存在**（index → store 单向）——索引是派生物，写入方负责 store 写后调 `Upsert/Remove`；删除 index 不损失 canonical。
- **assemble 生产路径不 import index**（§17 决议 4 四接口解耦）——向量路经 `DefaultAssembler.Semantic` 函数 seam 由装配层接线；E2E 缝合仅在测试。
- **memory/* 不 import observability**——观测是旁路：组件暴露返回值/快照（`ReflectionResult`、`Metrics()`、`Counted.Metrics()`），桥由装配层做（`request.usage` 同先例）。
- **reflection 不 import session**——surface 由宿主取出喂入（compaction 依赖 session 是因为要 fold/写回，reflection 只读输入）；也不 import compaction（字符计数口径对齐 `CharMeter` 而非复用类型）。
- **index/openai 不 import llm**——embedding 非跨 provider 稳定生成语义，不进 llm 词汇表；SDK 与 `llm/openai` 同源但是独立薄包装。

## 跨包不变式（铁律落在哪）

| 不变式 | 锚点 |
|---|---|
| append-only：投影可换，原始事件永不删改 | `session`（fold 只读日志；checkpoint 是追加事件） |
| model-visible means logged | `session.Open` 冷恢复合成的闭合事件**写回日志**再 fold |
| 压缩是事务不是删除 | `compaction` §9.1 八步（失败停在未闭合）；§9.2 原文完整保留 raw log |
| scope 是存储层边界，不是提示词约定 | `store` namespace 前缀可见（父读子、兄弟不互见）；`selfedit`/`candidate` 写与审批要求 **namespace 完全相等**（`ErrOutsideScope`，父不下钻子） |
| 禁物理 DELETE | `store` 无 Delete 接口；状态迁移只走 Supersede/Revoke（Put 翻状态 → `ErrStatusTransition`） |
| 没有来源不进 active memory | `store` SourceRefs 强制校验；`candidate`/`selfedit` 的 `OriginFn` 缺省即 New 失败 |
| 未过审批不进上下文 | Pending 对默认检索不可见（`store.Search` 只 Active）——assemble/index 各召回路径零接线即生效 |
| taint 是数据属性，审批是晋升闸 | `candidate` 批准不改 taint；`selfedit` 写入默认 `TaintUntrustedExt`（ASI06 对位） |
| 派生索引可丢可重建 | `index` 只存向量拷贝；`Rebuild` 全量从 store 重 embed；队列满丢弃计数 + Rebuild 兜底 |
| 反思默认关、有预算有审计 | `reflection` 无后台循环/计时器；`MaxInputChars` 预算门；`ReflectionResult` + `Metrics()` 审计面 |

## 宿主装配桥接点（memory 不做的事，谁做）

memory 层刻意不做的胶水，全部归装配层/宿主：

| 桥接点 | 归属 | 说明 |
|---|---|---|
| `session.Surface()` → `loop.Run` history | 装配层 | loop 不 import memory |
| `request.header` / `request.usage` 事件写入 | 装配层 | system+ToolDef+model 三样与 token usage 落日志（Ignorable 但必须发） |
| `index.VectorIndex` → `assemble.Semantic` | 装配层 | 生产路径解耦，见 assemble README「接入向量路」 |
| store 写后 `index.Upsert/Remove` | 装配层/写入方 | import 单向的代价；漏调只影响召回（Rebuild 可兜底） |
| `ReflectionResult`/三处 `Metrics()` → 观测面 | 装配层 | memory 不 import observability |
| Extractor / Summarizer / EmbeddingProvider 三个 LLM seam | 宿主 | 提取协议、摘要模型、embedding 路由全归宿主 prompt/配置 |
| 审批面 UI（Pending 列表 / Approve/Reject 按钮 / HITL 卡片） | 宿主 | 包提供同步 API，不做面板 |

## 指标面（D4，票 #92）

六项指标 → 三处就地快照（**不建独立 metrics 聚合包**；率值计算归宿主）：

| 指标 | 快照 | 计数点 |
|---|---|---|
| 提炼率 | `candidate.Metrics`：Stored/Extracted | `Pipeline.Extract` |
| 批准率 | Approved/(Approved+Rejected) | `Pipeline.Approve` |
| 撤销率 | Rejected/(Approved+Rejected) | `Pipeline.Reject`（= Revoke） |
| 召回命中 | `index.Counted`：Searches/Hits（次均命中；**≠ Recall@K 离线评测**） | `Counted.Search` |
| token 成本 | `reflection.Metrics`：Runs/TotalInputChars/TruncatedChars；真实 LLM usage 归宿主桥 | `Reflector.Reflect` |
| 污染拒绝率 | RejectedUntrusted/Rejected（仅 untrusted-external 档计入） | `Pipeline.Reject` |

计数只在动作**完整成功**时累计（错误中断的批次不计）；全部 atomic，-race 下并发安全（有并发用例锚定）。

## 平台与构建约束

- **纯 Go、零 CGO**：全部子包 `GOOS=plan9` / `GOOS=js GOARCH=wasm` 编译通过——除 SQLite backend。
- **SQLite 是唯一平台受限件**：`store/sqlite.go` 带 `//go:build !plan9 && !js`，驱动 `modernc.org/sqlite`（CGO-free，FTS5 内置）；plan9/js 下 SQLite 缺席但 store 主包照常编译（内存实现可用），core 不被锁死。
- **JSONL 明文**：文件即密钥面；`blob:` URL 前缀为本包保留；文件锁 stale 阈值默认 1h（`Flush` 兼作心跳）——细节见 session README。
- 测试矩阵：`go test -race -count=1 ./textsplit/... ./memory/...`（十包全绿为合入门槛）；live smoke 全部 env 门控（`PULSE_OPENAI_*`）。

## JSONL backend 注意事项（P2-A2）

- **明文存储**：JSONL/blobs/header 都是明文——文件即密钥面、路径宿主拥有；P2-A 不做加密。
- **`blob:` URL 前缀为本包保留**：引用形态用 `URL = "blob:<sha256>"` 标识溢出字节。宿主自带 `blob:` 开头的 `Image.URL`/`Media.URL` 会在载入时被误当内部引用还原，请改用其它 scheme。
- **文件锁心跳**：`Flush` 兼作锁心跳（touch 锁文件 mtime）。长命会话只要定期 Flush 就不会被 stale 抢占；从不 Flush 的会话持锁超过阈值（默认 1h，`JSONLStale` 可配）可能被另一进程接管。锁文件创建后立即关闭句柄、锁只靠存在性——若将来改为持句柄锁，Windows 的抢占路径会失效。
- **List 与损坏目录**：header.json 无法解析的目录从 `List` 结果消失（并发创建中/无关目录同理）；对应会话要等 `Open` 才报 `ErrCorruptLog`。
- **跨进程 Delete**：常态下不静默丢数据——Unix unlink 立即生效，对方进程的 Append 靠写前 stat 校验返回 `ErrDeleted`（stat 与 Write 之间仅剩微 TOCTOU 竞态窗口，库层无法根除）；Windows 上打开中的文件删不掉（Go 句柄无 FILE_SHARE_DELETE），`RemoveAll` 会先删掉能删的部分（blobs/、header.json）再失败留下半删目录，由宿主协调、对方释放句柄后重试即可完成。两条路都不做「报成功但丢数据」。
- **与 in-memory 的行为差**：A2 的 `Open` 对缓存命中（同进程重复打开）不做冷恢复——live 会话不被误补写；恢复路径是 `Close → Open`（重新载入磁盘日志时触发）。

## 状态机总览（store，含 Pending 的完整路径）

```text
                    candidate.Extract（自动提炼）
                              │
                              ▼
                         StatusPending          ← 对检索不可见（默认只 Active）
                        /           \
        candidate.Approve           candidate.Reject
        = Supersede（宿主盖章）      = Revoke（reason 落审计）
              /                         \
             ▼                           ▼
        StatusActive ──Supersede──▶ 新 item（Active）；旧 item ──▶ Superseded
             │
        （selfedit 三工具可直写 Active——HITL 审批 + taint 保守默认）
             │
        Active/Pending ──Revoke──▶ Revoked（终态；Superseded 不可 Revoke → ErrRevokeSuperseded）
```

- 审批晋升走 **Supersede 而非 Put**（`ErrStatusTransition` 挡 Put 翻状态——绕过审批面/替代链的路径全部封死）；批准版 Confidence=1.0、SourceRefs 继承 + manual 审批标记。
- 完整状态机语义与哨兵错误见 [store README](store/README_zh.md)；候选侧语义见 [candidate README](candidate/README_zh.md)。
