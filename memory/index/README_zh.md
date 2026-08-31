# memory/index

P2-D1 的派生检索索引：embedding provider seam + 向量索引（内存版）。canonical 永远在 [memory/store](../store/README_zh.md)——**索引可丢可重建**（§6.5：向量仅派生索引）。设计事实源 [docs/design/memory-layer-research-and-v2-design.md](../../docs/design/memory-layer-research-and-v2-design.md) §6.5/§8.2/§12 P2-D；实现票 #84（D1）。

## 接口面

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

## 不变式

- **派生索引可丢**：索引只存 `item_id → (vector, namespace)` 拷贝；`Rebuild` 全量从 store 重 embed（原子替换）。删除/损坏索引不损失任何 canonical item——验收钉第一条。
- **namespace 先过滤再召回**（§8.2）：授权过滤在相似度排序之前——兄弟 namespace 的 item 向量再近也不出现在结果（不泄漏存在性，不可见项也不挤占 top-k）。命中后回 store 复核 `Status==Active`（写入方同步窗口的 fail safe）。`Search` 返回 `[]ScoredHit`（Item + Score 余弦，降序）——D2 起 assemble 的 hybrid 融合把它当 semantic 路消费（装配层包成 `assemble.DefaultAssembler.Semantic` 函数 seam 接线，见 assemble README）。
- **索引只放 Active**：`Upsert` 非 Active 等价 `Remove`；状态同步靠写入方（import 图 index → store 单向，store 不知道 index 存在）。
- **Search 校验 fail closed**：空 query → `ErrInvalidQuery`；`k<=0` 用 `defaultTopK`（8）。
- **维度由 provider 钉死**：首次成功 embed 定维度，其后不符 → `ErrDimsMismatch`（fail closed；换 provider/模型必须 `Rebuild`）。
- **embed 输入 = Content**：`Structured` 领域载荷不进向量（与 C2/C3「Structured 不入检索域」口径一致）。

## EmbeddingProvider seam

```go
type EmbeddingProvider interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}
```

- 宿主注入：OpenAI embeddings / 本地模型 / 测试假实现。**不进 llm 词汇表**（embedding 非跨 provider 稳定生成语义），不进 kernel。
- `texts` 与返回向量必须等长、非空——形状不符 → `ErrProviderShape`（fail closed）。
- 批量分块/限流是 provider 的职责（`Rebuild` 一次性全量交付）。

## 计数装饰器（D4 指标面）

`Counted` 包装任意 `VectorIndex` 透传计数（`Metrics()` 快照，atomic）：

- **调用即计**（Upserts/Removes/Searches/Rebuilds 含失败执行——「执行了但失败」也是真实系统行为）；内层叠放时与 `Dropped()` 互补：入队数 = Upserts + Dropped。
- **Hits 只计成功轮**命中 item 数（次均命中 = Hits/Searches）。
- **叠放顺序决定 Upserts 口径**：推荐内层 `AsyncIndexer(Counted(idx))`——计「实际执行」；外层 `Counted(AsyncIndexer(idx))` 计「入队调用」（含后续被队列丢弃的，与 Dropped 双计）。
- `Searches/Hits` 是运行计数（次均命中观测），**不是 §13.2 的 Recall@K**——那是离线评测指标，两者不可混用。

## 异步队列取舍

embed 是 IO/LLM 成本中心：`AsyncIndexer` 把 Upsert/Remove 放进队列、单 worker 串行执行，写路径不阻塞。

- **队列满 → 丢弃计数**（`Dropped()`），**不背压阻塞写路径**——索引可重建（`Rebuild` 兜底一致性），丢索引更新比阻塞记忆写入更符合派生索引定位。
- worker 用独立 `context.Background()`：调用方请求结束/取消不丢已入队更新（可重建 ≠ 随意丢）。
- `Close` 关队列并等 drain；之后写入 → `ErrIndexClosed`。`Rebuild`/`Search` 直接透传底层（不进队列）。

## 默认 provider（openai 子包）

[`openai/`](openai/README_zh.md)（P2-D1.5，Issue #86）：OpenAI 兼容 embeddings 适配器——openai-go SDK 薄包装（与 `llm/openai` 同源），`BaseURL` 可指向 vLLM/Ollama/网关；批量分批、超长 textsplit 截断（`OnTruncate` 可观测）、形状校验包 `ErrProviderShape`；`Retries` 默认 0（对齐 llm/openai 先例）。import 需别名（与 `llm/openai` 同名不同路径）。

## 平台与依赖

纯 Go、零新依赖（余弦相似度手算）；plan9/js 编译不锁死。向量持久化与 HNSW/近似索引**不做**——索引可丢可重建，持久化是优化不是语义；hybrid retrieval 已接 assemble（D2 落地，`Semantic` 函数 seam——见 [assemble README](../assemble/README_zh.md)「接入向量路」）。

## 测试

```bash
go test -race -count=1 ./memory/index/
```
