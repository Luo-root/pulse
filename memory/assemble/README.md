[English](README.md) | [中文](README_zh.md)

# memory/assemble

The P2-C3 context assembly layer: assembles stable memory, retrieval-based memory, and the session surface into a model request sequence with budget boundaries.
Package docs (godoc) in `doc.go`; design source of truth [docs/design/memory-layer-research-and-v2-design.md](../../docs/design/memory-layer-research-and-v2-design.md) §8; implementation ticket #80.

## Integration

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

### Wiring the vector path (D2 hybrid, optional)

assemble's production path does not import index (§17 resolution 4, "four-interface decoupling"; E2E test stitching excepted) — the assembly layer wraps `index.VectorIndex` into a function seam and wires it in:

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

## Assembly order (§8.1)

```
[稳定前缀] frozen profile + stable decisions（RoleUser 引用模板；缓存，§8.3）
[surface 尾部] checkpoint + recent surface（原样，不裁切）
[检索记忆] ranked + cited（Query 召回，预算内 top-k）
[injected] 本轮立即应用（无预算约束，紧贴当前消息）
```

- **Budgets are per class** (§8.1 is not a single max-messages): over-budget items are **omitted and recorded in Diagnostics**, never silently dropped; an over-budget surface is **diagnosed but not truncated** (truncation belongs to compaction §9.1 / prune §9.2).
- **Stable snapshot (§8.3)**: a second assembly for the same namespace hits the cache (no re-query of the store); `RefreshStable` rebuilds explicitly; per-namespace isolation; a failed rebuild falls back to the old snapshot and records a diagnostic.
- **Citation template**: every injected memory looks like `[memory:<kind> <id> (source: session s9#12)] <content>` — SourceRefs made readable, so the model does not treat them as unconditional facts.

## Recall rules (the C2↔C3 seam)

Retrieved memory is recalled along two paths depending on store capability (the path is visible to the host and lands in Diagnostics):

- **FTS first**: when the store implements `SearchFTS` (the SQLite version), recall goes through token prefixes (`deploy` → `"deploy"*`, word boundaries + FTS rank); hits record the diagnostic `recall via fts token prefix`;
- **Substring fallback**: when FTS is unsupported (the memory version) or FTS fails, fall back to `MemoryStore.Search` substring recall (ASCII folding); FTS failure records the diagnostic `fts failed, falling back to substring`.

The two paths differ (word boundaries vs substring containment): the same query may recall differently on the memory version and the SQLite version — this is an explicitly declared seam enhancement, not a regression of the store's own `Search` replaceability (that invariant was pinned in C2). Candidates from both paths go through the same deterministic `rankHits` ordering; a recall failure never interrupts assembly (diagnostic recorded, the surface continues as usual).

## Ranking rules (§8.2 hybrid, D2)

The keyword path (FTS first, substring fallback) ∪ the semantic path (the optional `Semantic` function seam) are deduplicated by item ID, then fusion-scored (hits on both paths stack their scores):

```
score = w_semantic*sim + w_keyword*lexical + w_conf*conf − w_taint*taint_pen
```

- Default weights `Semantic=0.5 / Keyword=0.3 / Confidence=0.2 / Taint=0.3` (sim = max(0, cosine); taint_pen: untrusted-external = 1); the host overrides wholesale via `Ranking *RankingWeights` (nil = defaults; the pointer distinguishes an explicit 0).
- **Determinism**: score descending → UpdatedAt descending → ID ascending; recency does not enter the score (clock injection introduces nondeterminism) and is kept as the first tiebreaker.
- **w_conf is enabled in D2** (the design doc's "enabled only in P2-D" fulfilled) — P2-C's fixed taint constant of −4 is replaced by w_taint.
- Deliberately not implemented: w_scope (prefix visibility is already a hard filter), w_recall (no reuse signal source), w_stale (ValidUntil expiry handling not built) — ticket #88 verdict 3.
- The recall path is observable: `viaFTS` / `recall via semantic seam (n hits)` / failures and shape mismatches land in Diagnostics.
- `TokenCounter` is host-injected (nil = chars/4 estimate); it does not import compaction (meter reuse goes through the assembly-layer wiring ticket).

## Tests

```bash
go test -race -count=1 ./memory/assemble/...
```
