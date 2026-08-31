package index

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse/memory/store"
)

// fakeProvider 是确定性假 embedding：把文本首词映到预置向量表，未登记
// 的词给零向量——测试据此控制「哪些 item 相近」。dims 维数可配。
// onEmbed 是非 nil 时每次 Embed 调用触发（测试注入阻塞点）。
type fakeProvider struct {
	dims    int
	vecs    map[string][]float32 // 首词 → 向量
	onEmbed func()
}

func (f *fakeProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.onEmbed != nil {
		f.onEmbed()
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		first := strings.Fields(t)
		if len(first) == 0 {
			out[i] = make([]float32, f.dims)
			continue
		}
		if v, ok := f.vecs[first[0]]; ok {
			out[i] = v
			continue
		}
		out[i] = make([]float32, f.dims)
	}
	return out, nil
}

// failingProvider 的 Embed 永远失败（Upsert/Rebuild 失败路径断言）。
type failingProvider struct{}

func (failingProvider) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("provider offline")
}

func newTestIndex(t *testing.T, s store.MemoryStore, p EmbeddingProvider) *MemIndex {
	t.Helper()
	idx, err := NewMemIndex(s, p)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func putItem(t *testing.T, ctx context.Context, s store.MemoryStore, id string, ns []string, content string) store.MemoryItem {
	t.Helper()
	it := store.MemoryItem{
		ID:         id,
		Namespace:  ns,
		Kind:       store.KindEpisode,
		Content:    content,
		Status:     store.StatusActive,
		Confidence: 1.0,
		Taint:      store.TaintTrusted,
		SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: "s1", Seq: 1}},
	}
	saved, err := s.Put(ctx, it, store.PutMemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

// 单位向量（dim 维，第 i 位为 1）——两向量同向相似度 1，正交 0。
func unit(dims, i int) []float32 {
	v := make([]float32, dims)
	v[i] = 1
	return v
}

// TestSearchFiltersByNamespaceFirst：兄弟 namespace 的 item 向量再近也
// 不命中（先过滤再召回，§8.2）；父前缀可见子项。
func TestSearchFiltersByNamespaceFirst(t *testing.T) {
	ctx := t.Context()
	s := store.NewMemoryStore()
	p := &fakeProvider{dims: 4, vecs: map[string][]float32{"deploy": unit(4, 0)}}
	idx := newTestIndex(t, s, p)

	// tenant:a 与 tenant:b 各一条「deploy」——向量完全相同（相似度 1）。
	itA := putItem(t, ctx, s, "a1", []string{"tenant:a", "project:p1"}, "deploy via kubectl")
	itB := putItem(t, ctx, s, "b1", []string{"tenant:b", "project:p1"}, "deploy via helm")
	if err := idx.Upsert(ctx, itA); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, itB); err != nil {
		t.Fatal(err)
	}

	// 兄弟 namespace 查询：tenant:b 的 item 向量同样是满分，但不可见。
	hits, err := idx.Search(ctx, []string{"tenant:a"}, "deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Item.ID != "a1" {
		t.Fatalf("hits = %+v, want only a1（先过滤再召回，不泄漏存在性）", hits)
	}
	if hits[0].Score != 1 {
		t.Fatalf("score = %v, want 1（同向单位向量）", hits[0].Score)
	}
	// 父前缀可见子项。
	hits, err = idx.Search(ctx, []string{"tenant:a"}, "deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("parent hits = %d, want 1", len(hits))
	}
	// 空 ns = 全局可见。
	hits, err = idx.Search(ctx, nil, "deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("global hits = %d, want 2", len(hits))
	}
}

// TestDimsMismatch：维度由首次 embed 钉死，其后不符 fail closed。
func TestDimsMismatch(t *testing.T) {
	ctx := t.Context()
	s := store.NewMemoryStore()
	p := &fakeProvider{dims: 4, vecs: map[string][]float32{"a": unit(4, 0)}}
	idx := newTestIndex(t, s, p)
	it := putItem(t, ctx, s, "d1", []string{"tenant:a"}, "a fact")
	if err := idx.Upsert(ctx, it); err != nil {
		t.Fatal(err)
	}
	// 同一 provider 维度漂移（如宿主换模型未 Rebuild）：Upsert 与 Search 都拒。
	p.dims = 8
	p.vecs = map[string][]float32{"b": unit(8, 0)}
	it2 := putItem(t, ctx, s, "d2", []string{"tenant:a"}, "b fact")
	if err := idx.Upsert(ctx, it2); !errors.Is(err, ErrDimsMismatch) {
		t.Fatalf("upsert err = %v, want ErrDimsMismatch", err)
	}
	if _, err := idx.Search(ctx, []string{"tenant:a"}, "b", 10); !errors.Is(err, ErrDimsMismatch) {
		t.Fatalf("search err = %v, want ErrDimsMismatch", err)
	}
}

// TestRebuildLosesNothing：索引删除后 Rebuild 重建，top-k 结果一致——
// 派生索引可丢，canonical 零损失（验收钉第一条）。
func TestRebuildLosesNothing(t *testing.T) {
	ctx := t.Context()
	s := store.NewMemoryStore()
	p := &fakeProvider{dims: 4, vecs: map[string][]float32{
		"deploy": unit(4, 0), "yaml": unit(4, 1), "toml": unit(4, 2),
	}}
	idx := newTestIndex(t, s, p)
	for _, it := range []store.MemoryItem{
		putItem(t, ctx, s, "e1", []string{"tenant:a"}, "deploy via kubectl"),
		putItem(t, ctx, s, "e2", []string{"tenant:a"}, "yaml for CI"),
		putItem(t, ctx, s, "e3", []string{"tenant:a"}, "toml config"),
	} {
		if err := idx.Upsert(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	before, err := idx.Search(ctx, []string{"tenant:a"}, "deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	// 删除索引（模拟丢失），Rebuild 后结果一致。
	idx.entries = map[string]indexEntry{}
	idx.dims = 0
	if err := idx.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := idx.Search(ctx, []string{"tenant:a"}, "deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) || len(after) == 0 {
		t.Fatalf("rebuild hits %d → %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Item.ID != after[i].Item.ID {
			t.Fatalf("rebuild order changed at %d: %s vs %s", i, before[i].Item.ID, after[i].Item.ID)
		}
	}
	// Rebuild 只索引 Active：Supersede 后旧 item 不进新索引。
	if _, err := s.Supersede(ctx, "e1", store.MemoryItem{
		ID: "e1v2", Namespace: []string{"tenant:a"}, Kind: store.KindEpisode,
		Content: "deploy via argocd", Status: store.StatusActive, Confidence: 1.0,
		Taint:      store.TaintTrusted,
		SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: "s1", Seq: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	hits, _ := idx.Search(ctx, []string{"tenant:a"}, "deploy", 10)
	for _, h := range hits {
		if h.Item.ID == "e1" {
			t.Fatal("superseded item must not be indexed（索引只放 Active）")
		}
	}
}

// TestSearchScoresDescending：Score = 余弦相似度，降序返回；未登记词
//（零向量）相似度 0。
func TestSearchScoresDescending(t *testing.T) {
	ctx := t.Context()
	s := store.NewMemoryStore()
	p := &fakeProvider{dims: 4, vecs: map[string][]float32{"deploy": {1, 0, 0, 0}}}
	idx := newTestIndex(t, s, p)
	for _, it := range []store.MemoryItem{
		putItem(t, ctx, s, "e1", []string{"tenant:a"}, "deploy via kubectl"),
		putItem(t, ctx, s, "e2", []string{"tenant:a"}, "unrelated topic"),
	} {
		if err := idx.Upsert(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := idx.Search(ctx, []string{"tenant:a"}, "deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].Item.ID != "e1" || hits[0].Score != 1 {
		t.Fatalf("hits[0] = %s/%v, want e1/1", hits[0].Item.ID, hits[0].Score)
	}
	if hits[1].Item.ID != "e2" || hits[1].Score != 0 {
		t.Fatalf("hits[1] = %s/%v, want e2/0（零向量 cosine 0）", hits[1].Item.ID, hits[1].Score)
	}
}

// TestSearchRechecksStatus：命中后回 store 复核——索引里还有但 store 已
// Revoke 的项不返回（写入方同步窗口的 fail safe）。
func TestSearchRechecksStatus(t *testing.T) {
	ctx := t.Context()
	s := store.NewMemoryStore()
	p := &fakeProvider{dims: 4, vecs: map[string][]float32{"deploy": unit(4, 0)}}
	idx := newTestIndex(t, s, p)
	it := putItem(t, ctx, s, "e1", []string{"tenant:a"}, "deploy note")
	if err := idx.Upsert(ctx, it); err != nil {
		t.Fatal(err)
	}
	// 写入方尚未 Remove 索引，store 已 Revoke：命中复核拦住。
	if err := s.Revoke(ctx, "e1", "stale"); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.Search(ctx, []string{"tenant:a"}, "deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %d, want 0（revoked 不返回，即使索引未同步）", len(hits))
	}
	// 同步 Remove 后同样为空（双保险）。
	if err := idx.Remove(ctx, "e1"); err != nil {
		t.Fatal(err)
	}
}

// TestUpsertNonActiveRemoves：Upsert 非 Active item 等价 Remove。
func TestUpsertNonActiveRemoves(t *testing.T) {
	ctx := t.Context()
	s := store.NewMemoryStore()
	p := &fakeProvider{dims: 4, vecs: map[string][]float32{"deploy": unit(4, 0)}}
	idx := newTestIndex(t, s, p)
	it := putItem(t, ctx, s, "e1", []string{"tenant:a"}, "deploy note")
	if err := idx.Upsert(ctx, it); err != nil {
		t.Fatal(err)
	}
	it.Status = store.StatusSuperseded
	if err := idx.Upsert(ctx, it); err != nil {
		t.Fatal(err)
	}
	idx.mu.RLock()
	n := len(idx.entries)
	idx.mu.RUnlock()
	if n != 0 {
		t.Fatalf("entries = %d, want 0（非 Active Upsert 等价 Remove）", n)
	}
}

// TestAsyncQueueDropAndDrain：队列满丢弃计数（不阻塞写路径）；Close
// drain 已入队操作；Close 后写入拒绝。
func TestAsyncQueueDropAndDrain(t *testing.T) {
	ctx := t.Context()
	s := store.NewMemoryStore()
	p := &fakeProvider{dims: 4, vecs: map[string][]float32{"deploy": unit(4, 0)}}
	idx := newTestIndex(t, s, p)
	// 队列容量 1：灌 50 条必然大量丢弃。
	a, err := NewAsyncIndexer(idx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		it := putItem(t, ctx, s, strings.Repeat("x", 1)+string(rune('a'+i%26))+string(rune('0'+i/26)), []string{"tenant:a"}, "deploy note")
		if err := a.Upsert(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	if a.Dropped() == 0 {
		t.Fatal("queue full must drop and count（不背压阻塞写路径）")
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// Close 后拒绝。
	if err := a.Upsert(ctx, store.MemoryItem{ID: "late", Namespace: []string{"tenant:a"}}); !errors.Is(err, ErrIndexClosed) {
		t.Fatalf("upsert after close = %v, want ErrIndexClosed", err)
	}
	// Rebuild 兜底：丢弃的更新全量重建回来（透传不进队列）。
	if err := a.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	hits, err := a.Search(ctx, []string{"tenant:a"}, "deploy", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 50 {
		t.Fatalf("after rebuild hits = %d, want 50（Rebuild 兜底一致性）", len(hits))
	}
}

// TestProviderFailure：provider 失败时 Upsert/Rebuild 失败、不污染索引。
func TestProviderFailure(t *testing.T) {
	ctx := t.Context()
	s := store.NewMemoryStore()
	idx := newTestIndex(t, s, failingProvider{})
	it := putItem(t, ctx, s, "e1", []string{"tenant:a"}, "deploy note")
	if err := idx.Upsert(ctx, it); err == nil {
		t.Fatal("provider failure must surface")
	}
	idx.mu.RLock()
	n := len(idx.entries)
	idx.mu.RUnlock()
	if n != 0 {
		t.Fatal("failed upsert must not pollute index")
	}
	if err := idx.Rebuild(ctx); err == nil {
		t.Fatal("rebuild with failing provider must surface")
	}
}

// TestSearchValidation：空 query 拒绝；k<=0 走默认上限。
func TestSearchValidation(t *testing.T) {
	ctx := t.Context()
	s := store.NewMemoryStore()
	p := &fakeProvider{dims: 4, vecs: map[string][]float32{"deploy": unit(4, 0)}}
	idx := newTestIndex(t, s, p)
	if _, err := idx.Search(ctx, nil, "  ", 10); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("err = %v, want ErrInvalidQuery", err)
	}
	// k=0 走 defaultTopK：放 defaultTopK+2 条，召回不超过 defaultTopK。
	for i := 0; i < defaultTopK+2; i++ {
		it := putItem(t, ctx, s, string(rune('a'+i)), []string{"tenant:a"}, "deploy note")
		if err := idx.Upsert(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := idx.Search(ctx, []string{"tenant:a"}, "deploy", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != defaultTopK {
		t.Fatalf("hits = %d, want defaultTopK=%d", len(hits), defaultTopK)
	}
}

// TestRebuildMergesConcurrentWrites：Rebuild 的锁外 embed 窗口内并发的
// Upsert 不被 swap 抹掉（写入代际合并——swap 保留 seq 晚于 Rebuild 起点
// 的条目）。
func TestRebuildMergesConcurrentWrites(t *testing.T) {
	ctx := t.Context()
	s := store.NewMemoryStore()
	p := &fakeProvider{dims: 4, vecs: map[string][]float32{"deploy": unit(4, 0)}}
	idx := newTestIndex(t, s, p)
	it1 := putItem(t, ctx, s, "e1", []string{"tenant:a"}, "deploy via kubectl")
	if err := idx.Upsert(ctx, it1); err != nil {
		t.Fatal(err)
	}
	// hook 在 Upsert(e1) 之后才挂上——否则第一次阻塞发生在本线程的
	// Upsert(e1) 上（放行也在本线程，死锁）。
	entered := make(chan struct{})
	release := make(chan struct{})
	var blocked atomic.Bool
	// 第一次 Embed（Rebuild 的批量）阻塞，制造并发窗口；后续 Embed
	// （主线程 Upsert 的）直接通过——不用 sync.Once（并发 Do 会等待
	// 首次完成，与「放行在主线程」互相等待死锁）。
	p.onEmbed = func() {
		if blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	rebuildDone := make(chan error, 1)
	go func() { rebuildDone <- idx.Rebuild(ctx) }()
	<-entered // Rebuild 已扫完 store、正在 embed（锁外窗口）
	// 并发窗口写入 e2：写入代际晚于 Rebuild 起点。
	it2 := putItem(t, ctx, s, "e2", []string{"tenant:a"}, "deploy via helm")
	if err := idx.Upsert(ctx, it2); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-rebuildDone; err != nil {
		t.Fatal(err)
	}
	// e2 不被 swap 抹掉：fresh（扫描时快照，只有 e1）+ 并发保留（e2）。
	hits, err := idx.Search(ctx, []string{"tenant:a"}, "deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2（并发窗口写入不丢）", len(hits))
	}
}

// TestConcurrentIndexAccess：并发 Upsert/Remove/Search/Rebuild（-race）。
func TestConcurrentIndexAccess(t *testing.T) {
	ctx := t.Context()
	s := store.NewMemoryStore()
	p := &fakeProvider{dims: 4, vecs: map[string][]float32{"deploy": unit(4, 0)}}
	idx := newTestIndex(t, s, p)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				id := fmt.Sprintf("w%d-i%d", w, i)
				// t.Fatal 不能用于非测试 goroutine——内联构造 + t.Errorf。
				it := store.MemoryItem{
					ID: id, Namespace: []string{"tenant:a"}, Kind: store.KindEpisode,
					Content: "deploy note", Status: store.StatusActive, Confidence: 1.0,
					Taint:      store.TaintTrusted,
					SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: "s1", Seq: 1}},
				}
				if _, err := s.Put(ctx, it, store.PutMemoryOptions{}); err != nil {
					t.Errorf("put %s: %v", id, err)
					return
				}
				_ = idx.Upsert(ctx, it)
				_, _ = idx.Search(ctx, []string{"tenant:a"}, "deploy", 5)
				if i%5 == 0 {
					_ = idx.Rebuild(ctx)
				}
				_ = idx.Remove(ctx, id)
			}
		}(w)
	}
	wg.Wait()
}

// 编译期接口断言。
var (
	_ VectorIndex = (*MemIndex)(nil)
	_ VectorIndex = (*AsyncIndexer)(nil)
)

// TestAsyncWorkerUsesBackgroundContext：worker 在调用方 ctx 取消后仍能
// 完成已入队的更新（不丢索引更新——可重建≠随意丢）。
func TestAsyncWorkerUsesBackgroundContext(t *testing.T) {
	s := store.NewMemoryStore()
	p := &fakeProvider{dims: 4, vecs: map[string][]float32{"deploy": unit(4, 0)}}
	idx := newTestIndex(t, s, p)
	a, err := NewAsyncIndexer(idx, 8)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	it := putItem(t, context.Background(), s, "e1", []string{"tenant:a"}, "deploy note")
	if err := a.Upsert(ctx, it); err != nil {
		t.Fatal(err)
	}
	cancel() // 调用方 ctx 已取消
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// drain 后更新应已落（worker 用 background ctx）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		hits, _ := idx.Search(context.Background(), []string{"tenant:a"}, "deploy", 10)
		if len(hits) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("queued update lost after caller cancel（worker 必须用独立 ctx）")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAsyncCloseRaceWithEnqueue：enqueue 与 Close 并发交叠的历史竞态回归
// 锁——无互斥时「通过 closed 检查的 enqueue」会对已关闭 channel 发送
// （panic: send on closed channel；select/default 只防满队列）。RWMutex
// 互斥后只允许三种合法结果：入队成功 / 满丢弃 / ErrIndexClosed。
func TestAsyncCloseRaceWithEnqueue(t *testing.T) {
	for round := 0; round < 20; round++ {
		s := store.NewMemoryStore()
		p := &fakeProvider{dims: 4, vecs: map[string][]float32{"deploy": unit(4, 0)}}
		idx := newTestIndex(t, s, p)
		a, err := NewAsyncIndexer(idx, 1)
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				it := store.MemoryItem{
					ID: fmt.Sprintf("w%d", w), Namespace: []string{"tenant:a"},
					Kind: store.KindEpisode, Content: "deploy note",
					Status: store.StatusActive, Confidence: 1.0,
					Taint:      store.TaintTrusted,
					SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: "s1", Seq: 1}},
				}
				for {
					select {
					case <-stop:
						return
					default:
					}
					_ = a.Upsert(context.Background(), it)
				}
			}(w)
		}
		time.Sleep(2 * time.Millisecond) // writer 先转起来
		if err := a.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		close(stop) // Close 之后才停 writer——确保 enqueue 与 Close 真正交叠
		wg.Wait()
	}
}
