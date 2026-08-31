package reflection

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/candidate"
	"github.com/Luo-root/pulse/memory/store"
)

// fakeExtractor 是确定性假提炼 seam（记录收到的 surface——截断断言用；
// mu 保护 captured——并发用例下多 goroutine 同时 Extract）。
type fakeExtractor struct {
	mu       sync.Mutex
	items    []store.MemoryItem
	err      error
	captured [][]*llm.Message
}

func (f *fakeExtractor) Extract(_ context.Context, surface []*llm.Message) ([]store.MemoryItem, error) {
	f.mu.Lock()
	f.captured = append(f.captured, surface)
	f.mu.Unlock()
	return f.items, f.err
}

// origin 是测试用固定会话回链。
func origin() store.SourceRef {
	return store.SourceRef{Type: store.SourceSession, SessionID: "s1", Seq: 1}
}

// newReflector 建 Reflector 与底层 store（mutate 同时改 candidate.Options
// 与 reflection.Options；Pipeline 在 candidate.New 之后钉入）。
func newReflector(t *testing.T, mutate func(*candidate.Options, *Options)) (*Reflector, store.MemoryStore) {
	t.Helper()
	cOpt := candidate.Options{
		Store:     store.NewMemoryStore(),
		Extractor: &fakeExtractor{},
		Namespace: []string{"tenant:a"},
		OriginFn:  origin,
	}
	rOpt := Options{}
	if mutate != nil {
		mutate(&cOpt, &rOpt)
	}
	p, err := candidate.New(cOpt)
	if err != nil {
		t.Fatal(err)
	}
	rOpt.Pipeline = p
	r, err := New(rOpt)
	if err != nil {
		t.Fatal(err)
	}
	return r, cOpt.Store
}

// TestReflectEndToEnd：Extract 闭环（Pending 入库、默认检索不可见）→
// Items/Report 透传 → Metrics 计数。
func TestReflectEndToEnd(t *testing.T) {
	r, st := newReflector(t, func(c *candidate.Options, _ *Options) {
		c.Extractor = &fakeExtractor{items: []store.MemoryItem{
			{Kind: store.KindLesson, Content: "alpha lesson"},
		}}
	})
	res, err := r.Reflect(t.Context(), []*llm.Message{llm.UserText("chat")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Content != "alpha lesson" || res.Items[0].Status != store.StatusPending {
		t.Fatalf("items = %+v, want pending alpha lesson", res.Items)
	}
	if res.Report != (candidate.Report{Extracted: 1, Stored: 1}) {
		t.Fatalf("report = %+v", res.Report)
	}
	if res.InputChars != 4 || res.TruncatedChars != 0 { // "chat" = 4 rune
		t.Fatalf("chars = %d/%d, want 4/0", res.InputChars, res.TruncatedChars)
	}
	// 候选对默认检索不可见（store 只 Active——candidate 测试已锁，全链复核）。
	hits, err := st.Search(t.Context(), store.MemoryQuery{Namespace: []string{"tenant:a"}})
	if err != nil || len(hits) != 0 {
		t.Fatalf("default search = %d/%v, want 0（未批准不进上下文）", len(hits), err)
	}
	m := r.Metrics()
	if m.Runs != 1 || m.TotalInputChars != 4 || m.TruncatedChars != 0 {
		t.Fatalf("metrics = %+v, want {1,4,0}", m)
	}
}

// TestReflectTruncatesTailKeepingLatest：超预算从头部丢整条消息（尾部
// 保留）；多字节字符不截半（整条为粒度）；Metrics 记截断。
func TestReflectTruncatesTailKeepingLatest(t *testing.T) {
	ex := &fakeExtractor{items: []store.MemoryItem{{Kind: store.KindLesson, Content: "lesson from tail"}}}
	r, _ := newReflector(t, func(c *candidate.Options, r *Options) {
		c.Extractor = ex
		r.MaxInputChars = 3 // "尾段！" = 3 rune，恰好放下末条
	})
	first := llm.UserText("第一段是十个字的中文内容超预算") // 15 rune，整条超预算
	last := llm.UserText("尾段！")              // 3 rune
	res, err := r.Reflect(t.Context(), []*llm.Message{first, last})
	if err != nil {
		t.Fatal(err)
	}
	if res.TruncatedChars != 15 || res.InputChars != 3 {
		t.Fatalf("chars = %d/%d, want 3/15（首条整条丢弃）", res.InputChars, res.TruncatedChars)
	}
	// extractor 只收到末条，且多字节完整（不截半）。
	if len(ex.captured) != 1 || len(ex.captured[0]) != 1 || ex.captured[0][0].Parts[0].Text != "尾段！" {
		t.Fatalf("captured = %+v, want [尾段！]（多字节完整）", ex.captured)
	}
	if r.Metrics().TruncatedChars != 15 {
		t.Fatalf("metrics truncated = %d, want 15", r.Metrics().TruncatedChars)
	}
}

// TestTruncateTailKeepsLastOversized：末条自身超预算 → 整条保留（不截
// 半条、不丢光）；max<=0 不截断；预算内原样。
func TestTruncateTailKeepsLastOversized(t *testing.T) {
	big := llm.UserText(strings.Repeat("长", 10))
	got, truncated := truncateTail([]*llm.Message{big}, 3)
	if len(got) != 1 || truncated != 0 {
		t.Fatalf("got = %d msg, truncated = %d, want oversized last kept intact", len(got), truncated)
	}
	got, truncated = truncateTail([]*llm.Message{big, llm.UserText("x")}, 0)
	if len(got) != 2 || truncated != 0 {
		t.Fatalf("max=0 must not truncate, got %d/%d", len(got), truncated)
	}
	got, truncated = truncateTail([]*llm.Message{llm.UserText("a"), llm.UserText("b")}, 10)
	if len(got) != 2 || truncated != 0 {
		t.Fatalf("within budget must not truncate, got %d/%d", len(got), truncated)
	}
}

// TestReflectErrorNotCounted：错误透传不静默；错误轮不计数（Reflector
// 与 candidate 两侧都零）。
func TestReflectErrorNotCounted(t *testing.T) {
	r, _ := newReflector(t, func(c *candidate.Options, _ *Options) {
		c.Extractor = &fakeExtractor{err: errors.New("llm offline")}
	})
	if _, err := r.Reflect(t.Context(), []*llm.Message{llm.UserText("chat")}); err == nil || !strings.Contains(err.Error(), "llm offline") {
		t.Fatalf("err = %v, want passthrough", err)
	}
	if m := r.Metrics(); m.Runs != 0 || m.TotalInputChars != 0 || m.TruncatedChars != 0 {
		t.Fatalf("metrics = %+v, want zero（错误轮不计数）", m)
	}
}

// TestNewValidation：必填缺失 fail closed；MaxInputChars=0 合法（不限）。
func TestNewValidation(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("missing pipeline must fail")
	}
	p, err := candidate.New(candidate.Options{
		Store:     store.NewMemoryStore(),
		Extractor: &fakeExtractor{},
		Namespace: []string{"tenant:a"},
		OriginFn:  origin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Pipeline: p, MaxInputChars: -1}); err == nil {
		t.Fatal("negative max input chars must fail")
	}
	if _, err := New(Options{Pipeline: p}); err != nil {
		t.Fatalf("zero max input chars must be legal（不限）: %v", err)
	}
}

// TestReflectConcurrentMetrics：-race 下并发 Reflect 计数无丢失（票 #92
// 验收明文）——N 轮成功 Reflect 的累计 == N 次动作完整累计（Runs 与
// 字符计数与去重结果无关，断言竞态无关）。
func TestReflectConcurrentMetrics(t *testing.T) {
	r, _ := newReflector(t, func(c *candidate.Options, _ *Options) {
		c.Extractor = &fakeExtractor{items: []store.MemoryItem{
			{Kind: store.KindLesson, Content: "concurrent lesson"},
		}}
	})
	const n = 16
	surface := []*llm.Message{llm.UserText("chat")} // 4 rune
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Reflect(t.Context(), surface); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	m := r.Metrics()
	if m.Runs != n || m.TotalInputChars != n*4 || m.TruncatedChars != 0 {
		t.Fatalf("metrics = %+v, want runs=%d input=%d truncated=0（并发计数无丢失）", m, n, n*4)
	}
}
