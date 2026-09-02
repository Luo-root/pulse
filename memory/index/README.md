[English](README.md) | [中文](README_zh.md)

# memory/index

The P2-D1 derived retrieval index: embedding provider seam + vector index (in-memory version). The canonical copy always lives in [memory/store](../store/README.md) — **the index is disposable and rebuildable** (§6.5: vectors are only a derived index). Design source of truth [docs/design/memory-layer-research-and-v2-design.md](../../docs/design/memory-layer-research-and-v2-design.md) §6.5/§8.2/§12 P2-D; implementation ticket #84 (D1).

## Interface surface

```go
idx, err := index.NewMemIndex(memStore, myProvider) // provider 宿主注入
err = idx.Upsert(ctx, item)                          // 写入方在 store.Put 后调
err = idx.Remove(ctx, oldID)                         // Supersede/Revoke 后调
hits, err := idx.Search(ctx, ns, "deploy", 10)       // 先过滤再 top-k（[]ScoredHit：Item + Score 余弦）
err = idx.Rebuild(ctx)                               // 全量重建（索引丢失兜底）

async, err := index.NewAsyncIndexer(idx, 64)         // 异步队列：写路径不阻塞
defer async.Close(ctx)
async.Upsert(ctx, item)                              // 非阻塞入队；满丢弃计数
n := async.Dropped()                                 // 丢弃数（Rebuild 兜底）

counted, err := index.NewCounted(idx)                // 计数装饰器（D4 指标面）
async, err = index.NewAsyncIndexer(counted, 64)      // 推荐内层叠放（见下）
m := counted.Metrics()                               // Upserts/Removes/Searches/Hits/Rebuilds
```

## Invariants

- **The derived index is disposable**: the index stores only `item_id → (vector, namespace)` copies; `Rebuild` re-embeds everything from the store (atomic swap). Deleting or corrupting the index loses no canonical item — pinned as acceptance criterion #1.
- **Filter by namespace before recall** (§8.2): the authorization filter runs before similarity ranking — items from sibling namespaces never appear in results no matter how close their vectors are (no existence leak; invisible items do not crowd out top-k). After a hit, the store re-verifies `Status==Active` (fail safe against the writer's sync window). `Search` returns `[]ScoredHit` (Item + cosine Score, descending) — since D2, assemble's hybrid fusion consumes it as the semantic path (the assembly layer wraps it into the `assemble.DefaultAssembler.Semantic` function seam; see the assemble README).
- **The index holds Active only**: `Upsert` of a non-Active item is equivalent to `Remove`; status sync is the writer's responsibility (the import graph is one-way index → store; the store does not know the index exists).
- **Search validation is fail closed**: empty query → `ErrInvalidQuery`; `k<=0` uses `defaultTopK` (8).
- **Dimensions are pinned by the provider**: the first successful embed fixes the dimension; any later mismatch → `ErrDimsMismatch` (fail closed; switching provider/model requires `Rebuild`).
- **Embed input = Content**: the `Structured` domain payload never enters the vector (consistent with the C2/C3 rule that Structured stays out of the search domain).

## EmbeddingProvider seam

```go
type EmbeddingProvider interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}
```

- Host-injected: OpenAI embeddings / local models / test fakes. **Not part of the llm vocabulary** (embedding is not a cross-provider stable generation semantic) and not part of kernel.
- `texts` and the returned vectors must be equal in length and non-empty — shape mismatch → `ErrProviderShape` (fail closed).
- Batching and rate limiting are the provider's responsibility (`Rebuild` delivers the full set in one shot).

## Counting decorator (D4 metrics surface)

`Counted` wraps any `VectorIndex` and passes calls through while counting (`Metrics()` snapshot, atomic):

- **Counted at call time** (Upserts/Removes/Searches/Rebuilds include failed executions — "executed but failed" is also real system behavior); when stacked inside, it complements `Dropped()`: enqueued count = Upserts + Dropped.
- **Hits counts items hit in successful rounds only** (hits per search = Hits/Searches).
- **Stacking order determines the Upserts semantics**: the recommended inner stacking is `AsyncIndexer(Counted(idx))` — counts "actually executed"; the outer `Counted(AsyncIndexer(idx))` counts "enqueued calls" (including ones later dropped by the queue, double-counted with Dropped).
- `Searches/Hits` are runtime counters (a hits-per-search observation), **not the §13.2 Recall@K** — that is an offline evaluation metric; the two must not be conflated.

## Async queue trade-offs

Embedding is an IO/LLM cost center: `AsyncIndexer` puts Upsert/Remove into a queue executed serially by a single worker, keeping the write path unblocked.

- **Queue full → drop and count** (`Dropped()`), **never backpressure-block the write path** — the index is rebuildable (`Rebuild` restores consistency), so dropping index updates fits the derived-index positioning better than blocking memory writes.
- The worker uses its own `context.Background()`: request end/cancellation from the caller does not lose already-enqueued updates (rebuildable ≠ free to drop).
- `Close` shuts the queue and waits for drain; writes afterwards → `ErrIndexClosed`. `Rebuild`/`Search` pass straight through to the underlying index (not queued).

## Default provider (the openai sub-package)

[`openai/`](openai/README.md) (P2-D1.5, Issue #86): an OpenAI-compatible embeddings adapter — a thin wrapper over the openai-go SDK (same origin as `llm/openai`); `BaseURL` can point at vLLM/Ollama/gateways; batch splitting, oversized-input truncation via textsplit (observable through `OnTruncate`), shape validation wrapping `ErrProviderShape`; `Retries` defaults to 0 (aligned with the llm/openai precedent). Import with an alias (same package name as `llm/openai`, different path).

## Platform and dependencies

Pure Go, zero new dependencies (cosine similarity hand-rolled); plan9/js compilation never locked out. Vector persistence and HNSW/approximate indexes are **not done** — the index is disposable and rebuildable; persistence is an optimization, not semantics; hybrid retrieval is already wired into assemble (landed in D2, the `Semantic` function seam — see the [assemble README](../assemble/README.md) "Wiring the vector path").

## Tests

```bash
go test -race -count=1 ./memory/index/
```
