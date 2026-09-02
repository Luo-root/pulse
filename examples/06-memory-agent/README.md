# 06-memory-agent

长期记忆全链路课：store → index → assemble → candidate → 审批 → 闭环。一次运行走完一条记忆的完整生命周期，全部离线（embedding 用确定性词表假实现，任何 `EmbeddingProvider` 实现可直接替换）。

## 本课依赖

[05-memory-session](../05-memory-session/)：会话层的 event-sourced 立场（本课的 surface 来源）。

## 六段演示（对照 main.go）

### ① store：宿主权威写入

`store.Put` 直写 Active 记忆：显式 `Namespace`（`MemoryScope` 展开）、`Kind`、`Confidence`（Active 必填）、`Taint`、`SourceRefs`（**没有来源不进 active**）。宿主权威写入用 `TaintTrusted`。

### ② index：派生向量召回

`index.NewMemIndex(memStore, provider)`——canonical 在 store，索引可丢可重建（`Rebuild`）。**写入方负责 store 写后 `Upsert`**（import 图 index → store 单向的代价，漏调只影响召回）。Search 是「namespace 先过滤再召回」：授权判定发生在相似度排序之前。

demo 的 `demoProvider` 是词表 one-hot 假实现（首维恒 0.1——刻意规避全零向量：余弦相似度对零向量未定义，demo 复改时别把这句去掉）；真实项目换 `memory/index/openai`（OpenAI 兼容 embeddings）或自研，调用侧零改动。

### ③ assemble：把记忆装进请求

`assemble.NewDefaultAssembler(memStore, nil, Budget{...})`：稳定前缀（frozen snapshot 缓存）→ surface 尾部 → 检索记忆 → injected。**生产路径不 import index**——向量路经函数 seam 由装配层接线（本课示范了接法）。注入的记忆带**引用模板**：

```text
[memory:decision seed-postgres (source: seed (host-authoritative))] Use PostgreSQL for audit logs
```

SourceRefs 可读化——模型不当无条件事实。`Diagnostics` 记录每条召回路径（`recall via semantic seam (2 hits)`）。

### ④ candidate：自动提炼只写 Pending

`candidate.New(Options{Store, Extractor, Namespace, OriginFn})` 四必填；Extractor 是宿主 seam（提取协议归宿主 prompt）。`Extract` 内部：双归一去重 → Pending 入库 → **Report 守恒计数**（Extracted/Stored/Duplicates/Invalid，禁止静默丢）。

关键验证：Pending 后默认 Search 仍只看到 2 条 Active——**未过审批的记忆对 assemble/index 检索天然不可见**（渐进披露由 store 状态机免费保证）。

### ⑤ 宿主审批：人盖章

```go
pending, _ := cand.Pending(ctx)          // 宿主审批面列表
active, _ := cand.Approve(ctx, p.ID)     // = Supersede：批准版新 ID、Confidence=1.0
```

- 批准版是**新 ID 的 Active item**，旧候选 Superseded 留痕（完整审计链）；
- SourceRefs 继承 + **manual 审批标记**（`approved via candidate.Pipeline`）——审批动作在 provenance 显式可辨；
- **taint 不变**（untrusted-external 还是 untrusted-external）：审批是晋升闸，taint 是数据属性，批准不洗白；
- 审批作用域 = namespace 完全相等（越界 `ErrOutsideScope`）；`Reject` = Revoke（reason 落审计）。

### ⑥ 闭环 + 指标

批准后的记忆（同步索引后）在下一轮 `Assemble` 被召回注入——闭环完成。`candidate.Metrics()` 给出六项指标中四项的原料：提炼率（Stored/Extracted）、批准率、撤销率、污染拒绝率（RejectedUntrusted）。

## 与生产的距离

本课缺三块，在 07 课聚合：真实 LLM 提炼（Extractor 接你的模型与 prompt）、后台反思调度（`memory/reflection`，触发时机归宿主）、观测桥（Metrics 快照 → 你的监控栈）。

## 运行

```powershell
go run ./examples/06-memory-agent
```

## 下一课

[07-production](../07-production/)：生产形态——MCP/Skills 多来源工具、观测桥、反思与指标面聚合。
