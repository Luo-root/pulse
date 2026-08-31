package index

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/memory/store"
)

// defaultTopK 是 Search 的 k<=0 时的默认召回条数。
const defaultTopK = 8

// VectorIndexKey 是 memory/index 的 kernel 服务键（对齐
// toolset.ServiceKey 先例：service key 归 memory/* 各包）。
var VectorIndexKey = kernel.NewServiceKey[VectorIndex]("memory.index.vector")

// EmbeddingProvider 是向量嵌入的宿主注入 seam：把一批文本映到向量。
// 不进 llm 词汇表（embedding 非跨 provider 稳定生成语义：OpenAI 有、
// Anthropic 无）；实现可以是 OpenAI embeddings、本地模型或测试假实现。
// 维度由 provider 决定；texts 与返回向量必须等长（不符 fail closed）。
type EmbeddingProvider interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ScoredHit 是向量召回命中：item 本体 + 余弦相似度。Score 是 §8.2
// hybrid 融合的 semantic 路输入（D2）——keyword 检索无分，store 的
// MemoryHit 不动（store 不知道评分）。
type ScoredHit struct {
	Item  store.MemoryItem
	Score float64 // cosine ∈ [-1,1]；融合侧 clamp 到 [0,1]
}

// VectorIndex 是派生向量索引（§12 P2-D）：canonical 在 store，本接口的
// 一切数据都可用 Rebuild 从 store 重建。
type VectorIndex interface {
	// Upsert 索引一条 Active item（embed Content + 写入）；item 非
	// Active 时等价 Remove（索引只放生效版本）。维度不符 →
	// ErrDimsMismatch（fail closed）。
	Upsert(ctx context.Context, item store.MemoryItem) error
	// Remove 摘除一条（Supersede/Revoke 后由写入方调用）；不存在幂等。
	Remove(ctx context.Context, id string) error
	// Search 召回：embed query → 索引内先按 ns 前缀过滤（§8.2，ANN
	// 之前授权过滤）→ 余弦 top-k（Score 降序，同分 ID 升序）→ 回 store
	// 复核 Active（竞态 fail safe）。D2 起 Score 随命中返回——§8.2
	// hybrid 融合的 semantic 路输入。k<=0 用 defaultTopK；空 query →
	// ErrInvalidQuery。
	Search(ctx context.Context, ns []string, query string, k int) ([]ScoredHit, error)
	// Rebuild 全量从 store 重建（全量 Active item 重 embed，原子替换
	// 索引内容）。索引删除/损坏后的恢复路径；代价 = 重 embed 全部。
	Rebuild(ctx context.Context) error
}

// 包内哨兵错误。调用方用 errors.Is 判别。
var (
	// ErrDimsMismatch：provider 返回向量维度与索引已建维度不符（维度由
	// 首次成功 embed 钉死——中途换 provider/模型必须 Rebuild）。
	ErrDimsMismatch = errors.New("index: embedding dims mismatch")
	// ErrProviderShape：provider 返回向量数与输入文本数不符，或空向量。
	ErrProviderShape = errors.New("index: provider response shape invalid")
	// ErrInvalidQuery：Search 条件非法（空 query）。
	ErrInvalidQuery = errors.New("index: query invalid")
	// ErrIndexClosed：AsyncIndexer Close 之后的写入——显式拒绝，不
	// 静默丢弃（丢弃只发生在运行中队列满）。
	ErrIndexClosed = errors.New("index: indexer closed")
)

// indexEntry 是索引内的一条拷贝：向量 + namespace（过滤在索引内完成，
// 不回查 store——先过滤再召回要求授权判定发生在相似度排序之前）。
// seq 是写入代际：Rebuild 与并发 Upsert 竞态时，swap 保留 seq 大于
// Rebuild 起始代际的条目（并发窗口写入不丢——见 Rebuild）。
type indexEntry struct {
	vector    []float32
	namespace []string
	seq       uint64
}

// MemIndex 是 VectorIndex 的内存实现：暴力扫描余弦 top-k（HNSW 等近似
// 索引是 provider/后续的事，内存语义测绿优先）。纯 Go，零新依赖。
// provider 构造后不可换（无锁字段）——换 provider/模型 = 新建 MemIndex
// 或先 Rebuild（Rebuild 会按新 provider 重钉维度）。
type MemIndex struct {
	store    store.MemoryStore
	provider EmbeddingProvider

	mu       sync.RWMutex
	entries  map[string]indexEntry
	dims     int    // 首次成功 embed 钉死；其后不符 → ErrDimsMismatch
	writeSeq uint64 // 写入代际：Upsert/Remove 递增，Rebuild 合并用
}

// NewMemIndex 创建内存向量索引。store 是 canonical 来源（Rebuild 与
// 命中复核用）；provider 必填。
func NewMemIndex(s store.MemoryStore, p EmbeddingProvider) (*MemIndex, error) {
	if s == nil {
		return nil, fmt.Errorf("index: store is required")
	}
	if p == nil {
		return nil, fmt.Errorf("index: embedding provider is required")
	}
	return &MemIndex{store: s, provider: p, entries: make(map[string]indexEntry)}, nil
}

// Upsert 实现 VectorIndex。
func (m *MemIndex) Upsert(ctx context.Context, item store.MemoryItem) error {
	if item.Status != store.StatusActive {
		return m.Remove(ctx, item.ID)
	}
	vec, err := m.embedOne(ctx, item.Content)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkDimsLocked(vec); err != nil {
		return err
	}
	m.writeSeq++
	m.entries[item.ID] = indexEntry{vector: vec, namespace: item.Namespace, seq: m.writeSeq}
	return nil
}

// Remove 实现 VectorIndex（幂等）。
func (m *MemIndex) Remove(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeSeq++
	delete(m.entries, id)
	return nil
}

// Search 实现 VectorIndex：先 namespace 前缀过滤，再余弦 top-k
//（Score 降序、同分 ID 升序——D2 起随命中返回）。
func (m *MemIndex) Search(ctx context.Context, ns []string, query string, k int) ([]ScoredHit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: empty query", ErrInvalidQuery)
	}
	if k <= 0 {
		k = defaultTopK
	}
	qvec, err := m.embedOne(ctx, query)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	if err := m.checkDimsRLocked(qvec); err != nil {
		m.mu.RUnlock()
		return nil, err
	}
	type scored struct {
		id    string
		score float64
	}
	cands := make([]scored, 0, len(m.entries))
	for id, e := range m.entries {
		// 先过滤（§8.2）：namespace 不可见的项不参与相似度排序——
		// 不泄漏存在性，也不让不可见项挤占 top-k。
		if !namespacePrefix(ns, e.namespace) {
			continue
		}
		cands = append(cands, scored{id: id, score: cosine(qvec, e.vector)})
	}
	m.mu.RUnlock()
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].id < cands[j].id // 同分按 ID 稳定序
	})
	if len(cands) > k {
		cands = cands[:k]
	}
	hits := make([]ScoredHit, 0, len(cands))
	for _, c := range cands {
		// 回 store 复核：索引与 store 之间存在写入方同步窗口，命中项
		// 可能已被 Revoke/Supersede——非 Active 不返回（fail safe），
		// 不可见（理论上已过滤，复核兜底）也不返回。
		it, err := m.store.Get(ctx, ns, c.id)
		if err != nil {
			continue
		}
		if it.Status != store.StatusActive {
			continue
		}
		hits = append(hits, ScoredHit{Item: it, Score: c.score})
	}
	return hits, nil
}

// Rebuild 实现 VectorIndex：全量 Active item 重 embed，原子替换。
// embed 在锁外（IO 不持锁），与并发 Upsert 存在窗口——swap 时保留
// 写入代际晚于 Rebuild 起点的条目（并发窗口写入不丢）；与 fresh 维度
// 不符的保留条目丢弃（Rebuild 换 provider 的场景，旧维度条目由下一次
// Rebuild/Upsert 收敛）。
func (m *MemIndex) Rebuild(ctx context.Context) error {
	m.mu.RLock()
	g0 := m.writeSeq
	m.mu.RUnlock()
	hits, err := m.store.Search(ctx, store.MemoryQuery{})
	if err != nil {
		return fmt.Errorf("index: rebuild scan store: %w", err)
	}
	texts := make([]string, 0, len(hits))
	for _, h := range hits {
		texts = append(texts, h.Item.Content)
	}
	var vecs [][]float32
	if len(texts) > 0 {
		// 一次性全量交给 provider——批量分块/限流是 provider 的职责。
		vecs, err = m.provider.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("index: rebuild embed: %w", err)
		}
		if len(vecs) != len(texts) {
			return fmt.Errorf("%w: embed %d texts got %d vectors", ErrProviderShape, len(texts), len(vecs))
		}
	}
	fresh := make(map[string]indexEntry, len(hits))
	dims := 0
	for i, h := range hits {
		if len(vecs[i]) == 0 {
			return fmt.Errorf("%w: empty vector for item %s", ErrProviderShape, h.Item.ID)
		}
		if dims == 0 {
			dims = len(vecs[i])
		} else if len(vecs[i]) != dims {
			return fmt.Errorf("%w: rebuild vector dims %d != %d", ErrProviderShape, len(vecs[i]), dims)
		}
		fresh[h.Item.ID] = indexEntry{vector: vecs[i], namespace: h.Item.Namespace}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// 合并并发窗口（g0 之后）的写入：fresh 是扫描时刻的 store 快照，
	// 并发 Upsert 的条目可能不在其中——保留之（Remove 的条目已不在
	// entries，天然不保留；其若残留在 fresh，Search 的 store 复核兜底）。
	for id, e := range m.entries {
		if e.seq > g0 && (dims == 0 || len(e.vector) == dims) {
			fresh[id] = e
			if dims == 0 {
				dims = len(e.vector)
			}
		}
	}
	m.entries = fresh
	m.dims = dims // 空索引时 dims 归零，下次 embed 重新钉
	return nil
}

// embedOne 调 provider 嵌入单条文本并校验返回形状。
func (m *MemIndex) embedOne(ctx context.Context, text string) ([]float32, error) {
	vecs, err := m.provider.Embed(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("index: embed: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, fmt.Errorf("%w: embed one text got %d vectors", ErrProviderShape, len(vecs))
	}
	return vecs[0], nil
}

// checkDimsLocked 校验/钉死维度（调用方持写锁）。
func (m *MemIndex) checkDimsLocked(vec []float32) error {
	if m.dims == 0 {
		m.dims = len(vec)
		return nil
	}
	if len(vec) != m.dims {
		return fmt.Errorf("%w: got %d dims, index speaks %d", ErrDimsMismatch, len(vec), m.dims)
	}
	return nil
}

// checkDimsRLocked 只读校验：索引尚无内容（dims==0）时不钉——空索引上
// 查询任何维度都合法（结果为空），首次 Upsert 才钉维度。
func (m *MemIndex) checkDimsRLocked(vec []float32) error {
	if m.dims != 0 && len(vec) != m.dims {
		return fmt.Errorf("%w: got %d dims, index speaks %d", ErrDimsMismatch, len(vec), m.dims)
	}
	return nil
}

// cosine 余弦相似度；零向量（无范数）得 0。
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// namespacePrefix 报告查询 ns 是否可见条目 ns（q 是 item 的前缀，含
// 相等；q 空 = 全局）——与 store.namespaceVisible 同口径（store 未导出，
// 此处保留 5 行重复以换 store API 稳定；口径变更须两侧同步）。
func namespacePrefix(q, itemNS []string) bool {
	if len(q) == 0 {
		return true
	}
	if len(q) > len(itemNS) {
		return false
	}
	for i, part := range q {
		if itemNS[i] != part {
			return false
		}
	}
	return true
}
