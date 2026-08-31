# memory/assemble

P2-C3 的上下文装配层：把 stable memory、检索型记忆与 session surface 组装成带预算边界的模型请求序列。
包文档（godoc）见 `doc.go`；设计事实源 [docs/design/memory-layer-research-and-v2-design.md](../../docs/design/memory-layer-research-and-v2-design.md) §8；实现票 #80。

## 接入

```go
a := assemble.NewDefaultAssembler(memStore, nil, assemble.Budget{
    StableMemoryTokens: 300,   // frozen profile/facts 的小固定预算
    RetrievedTokens:    200,   // 检索型记忆动态预算
    MaxSurfaceTail:     40,    // surface 尾部节点数检查
})
ac, err := a.Assemble(ctx, assemble.AssembleInput{
    Namespace: scope.Namespace(),
    Surface:   surface,       // session.Surface() 原样
    Query:     userText,      // 检索召回信号
    Injected:  injected,      // 本轮立即应用的记忆
    // RefreshStable: true,    // 显式重建稳定前缀缓存
})
// ac.Messages        = 稳定前缀 → surface → 检索 → injected
// ac.StablePrefixLen = 稳定前缀边界（provider 侧缓存用）
// ac.Diagnostics     = 每类省略/降级/缓存事件（预算可解释）
```

### 接入向量路（D2 hybrid，可选）

assemble 不 import index（§17 决议 4「四接口解耦」）——装配层把 `index.VectorIndex` 包成函数 seam 接进来：

```go
memIdx, _ := index.NewMemIndex(memStore, provider)
a.Semantic = func(ctx context.Context, ns []string, q string, k int) ([]store.MemoryItem, []float64, error) {
    hits, err := memIdx.Search(ctx, ns, q, k) // []index.ScoredHit（余弦降序）
    if err != nil {
        return nil, nil, err
    }
    items := make([]store.MemoryItem, len(hits))
    scores := make([]float64, len(hits))
    for i, h := range hits {
        items[i], scores[i] = h.Item, h.Score
    }
    return items, scores, nil
}
// nil = keyword-only；失败/形状不符 → 诊断记录，组装不中断
```

## 组装顺序（§8.1）

```
[稳定前缀] frozen profile + stable decisions（RoleUser 引用模板；缓存，§8.3）
[surface 尾部] checkpoint + recent surface（原样，不裁切）
[检索记忆] ranked + cited（Query 召回，预算内 top-k）
[injected] 本轮立即应用（无预算约束，紧贴当前消息）
```

- **预算按类**（§8.1 不是只给一个 max messages）：超限**省略并记 Diagnostics**，不静默丢；surface 超限**只诊断不裁切**（裁切归 compaction §9.1 / prune §9.2）。
- **stable snapshot（§8.3）**：同 namespace 二次组装命中缓存（不重查 store）；`RefreshStable` 显式重建；per-namespace 隔离；重建失败退回旧快照并记诊断。
- **引用模板**：每条注入记忆形如 `[memory:<kind> <id> (source: session s9#12)] <content>`——SourceRefs 可读化，模型不当无条件事实。

## 召回口径（C2↔C3 接缝）

检索记忆按 store 能力分两路召回（路径对宿主可见，落 Diagnostics）：

- **FTS 优先**：store 实现 `SearchFTS`（SQLite 版）时走 token 前缀召回（`deploy` → `"deploy"*`，词边界 + FTS rank），命中记诊断 `recall via fts token prefix`；
- **回退子串**：不支持 FTS（内存版）或 FTS 失败时回退 `MemoryStore.Search` 子串召回（ASCII 折叠）；FTS 失败记诊断 `fts failed, falling back to substring`。

两路口径有差异（词边界 vs 子串包含）：同一 query 在内存版与 SQLite 版下的召回结果可能不同——这是显式声明的接缝增强，不是 store 自身 `Search` 的可替换性回归（那是 C2 已钉死的不变式）。两路候选都过同一 `rankHits` 确定性排序；召回失败不中断组装（诊断记录，surface 照常）。

## 排序口径（§8.2 hybrid，D2）

keyword 路（FTS 优先回退子串）∪ semantic 路（可选 `Semantic` 函数 seam）按 item ID 去重，融合评分（双路命中得分叠加）：

```
score = w_semantic*sim + w_keyword*lexical + w_conf*conf − w_taint*taint_pen
```

- 默认权重 `Semantic=0.5 / Keyword=0.3 / Confidence=0.2 / Taint=0.3`（sim = max(0, 余弦)；taint_pen：untrusted-external = 1）；宿主经 `Ranking *RankingWeights` 整体覆盖（nil = 默认，指针区分显式 0）。
- **确定性**：score 降序 → UpdatedAt 降序 → ID 升序；recency 不进 score（时钟注入引入不确定性），保留为第一 tiebreaker。
- **w_conf 在 D2 启用**（设计文「仅 P2-D 启用」兑现）——P2-C 的 taint 固定 −4 常量由 w_taint 取代。
- 刻意不实现：w_scope（前缀可见已是硬过滤）、w_recall（无 reuse 信号源）、w_stale（ValidUntil 过期处理未建）——票 #88 裁决 3。
- 召回路径可观测：`viaFTS` / `recall via semantic seam (n hits)` / 失败与形状不符诊断落 Diagnostics。
- `TokenCounter` 由宿主注入（nil = 字符/4 估算）；不 import compaction（meter 复用走装配层接线票）。

## 测试

```bash
go test -race -count=1 ./memory/assemble/...
```
