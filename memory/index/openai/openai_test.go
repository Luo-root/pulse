package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Luo-root/pulse/memory/index"
	"github.com/Luo-root/pulse/memory/store"
)

// embedRequest 记录假服务收到的请求体（线格式断言）。
type embedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
}

// fakeServer 是 OpenAI 兼容 /embeddings 假服务：按输入首词返回预置
// 向量（未登记词给零向量），可注入 429/恒定错误/形状破坏。
type fakeServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	bodies   []embedRequest
	fail429  int  // 剩余 429 次数（Retry-After: 0——测 SDK 重试不拖慢测试）
	status   int  // 非 0 = 恒定错误状态码
	dropOne  bool // 少返回一个向量（形状错）
	emptyVec bool // 返回空向量（形状错）
	vectors  map[string][]float64
	dims     int
}

func (f *fakeServer) handler(w http.ResponseWriter, r *http.Request) {
	var body embedRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies = append(f.bodies, body)
	switch {
	case f.fail429 > 0:
		f.fail429--
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		return
	case f.status != 0:
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		return
	}
	type dataItem struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
		Object    string    `json:"object"`
	}
	n := len(body.Input)
	if f.dropOne && n > 0 {
		n--
	}
	data := make([]dataItem, 0, n)
	for i := 0; i < n; i++ {
		first := ""
		if fs := strings.Fields(body.Input[i]); len(fs) > 0 {
			first = fs[0]
		}
		vec := f.vectors[first]
		if vec == nil {
			vec = make([]float64, f.dims)
		}
		if f.emptyVec {
			vec = []float64{}
		}
		data = append(data, dataItem{Index: i, Embedding: vec, Object: "embedding"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data":   data,
		"model":  body.Model,
		"object": "list",
		"usage":  map[string]any{"prompt_tokens": 1, "total_tokens": 1},
	})
}

func (f *fakeServer) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func newFakeServer(t *testing.T, dims int, vectors map[string][]float64) *fakeServer {
	t.Helper()
	f := &fakeServer{vectors: vectors, dims: dims}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.srv.Close)
	return f
}

func newProvider(t *testing.T, f *fakeServer, mutate func(*Config)) *Provider {
	t.Helper()
	cfg := Config{BaseURL: f.srv.URL, Model: "text-embedding-3-small", APIKey: "sk-test-secret"}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestNewValidation：Model/APIKey 必填。
func TestNewValidation(t *testing.T) {
	if _, err := New(Config{APIKey: "sk"}); err == nil {
		t.Fatal("missing model must fail")
	}
	if _, err := New(Config{Model: "text-embedding-3-small"}); err == nil {
		t.Fatal("missing api key must fail")
	}
}

// TestEmbedRoundtripAndBatches：线格式（model/input/encoding_format）、
// 批量分批、输出按输入顺序恒等返回。
func TestEmbedRoundtripAndBatches(t *testing.T) {
	f := newFakeServer(t, 4, map[string][]float64{
		"deploy": {1, 0, 0, 0},
		"yaml":   {0, 1, 0, 0},
	})
	p := newProvider(t, f, func(c *Config) { c.BatchSize = 2 })

	texts := []string{"deploy a", "yaml b", "deploy c", "yaml d", "deploy e"}
	vecs, err := p.Embed(t.Context(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("vecs = %d, want %d", len(vecs), len(texts))
	}
	want := [][]float32{{1, 0, 0, 0}, {0, 1, 0, 0}, {1, 0, 0, 0}, {0, 1, 0, 0}, {1, 0, 0, 0}}
	for i, v := range vecs {
		if len(v) != 4 {
			t.Fatalf("vec[%d] len = %d, want 4", i, len(v))
		}
		for j := range v {
			if v[j] != want[i][j] {
				t.Fatalf("vec[%d][%d] = %v, want %v（输出必须与输入顺序恒等）", i, j, v[j], want[i][j])
			}
		}
	}
	// 线格式与批切。
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != 3 {
		t.Fatalf("requests = %d, want 3（BatchSize=2 五条输入）", len(f.bodies))
	}
	for i, b := range f.bodies {
		if b.Model != "text-embedding-3-small" || b.EncodingFormat != "float" {
			t.Fatalf("request[%d] = %+v", i, b)
		}
	}
	wantLens := []int{2, 2, 1}
	for i, b := range f.bodies {
		if len(b.Input) != wantLens[i] {
			t.Fatalf("request[%d] input = %d, want %d", i, len(b.Input), wantLens[i])
		}
	}
}

// TestEmbedRetryDefaultOffAndExplicit：默认 Retries=0 不重试（对齐
// llm/openai 先例）；显式 Retries>0 时 SDK 重试生效。
func TestEmbedRetryDefaultOffAndExplicit(t *testing.T) {
	f := newFakeServer(t, 4, map[string][]float64{"deploy": {1, 0, 0, 0}})
	f.fail429 = 1
	p := newProvider(t, f, nil) // 默认 Retries=0
	if _, err := p.Embed(t.Context(), []string{"deploy a"}); err == nil {
		t.Fatal("429 with retries=0 must surface")
	}
	if got := f.requestCount(); got != 1 {
		t.Fatalf("requests = %d, want 1（默认不静默重试）", got)
	}

	f2 := newFakeServer(t, 4, map[string][]float64{"deploy": {1, 0, 0, 0}})
	f2.fail429 = 1
	p2 := newProvider(t, f2, func(c *Config) { c.Retries = 2 })
	if _, err := p2.Embed(t.Context(), []string{"deploy a"}); err != nil {
		t.Fatalf("retries=2 must recover via SDK backoff: %v", err)
	}
	if got := f2.requestCount(); got != 2 {
		t.Fatalf("requests = %d, want 2（SDK 内置重试生效）", got)
	}
}

// TestEmbedErrorShapeAndNoKeyLeak：非 2xx 结构化错误且不泄 API key；
// 形状错（少向量/空向量）包 index.ErrProviderShape。
func TestEmbedErrorShapeAndNoKeyLeak(t *testing.T) {
	f := newFakeServer(t, 4, map[string][]float64{"deploy": {1, 0, 0, 0}})
	f.status = http.StatusUnauthorized
	p := newProvider(t, f, nil)
	_, err := p.Embed(t.Context(), []string{"deploy a"})
	if err == nil {
		t.Fatal("401 must surface")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want structured status", err)
	}
	if strings.Contains(err.Error(), "sk-test-secret") {
		t.Fatal("API key must never appear in errors")
	}

	f2 := newFakeServer(t, 4, map[string][]float64{"deploy": {1, 0, 0, 0}})
	f2.dropOne = true
	p2 := newProvider(t, f2, nil)
	_, err = p2.Embed(t.Context(), []string{"deploy a", "deploy b"})
	if !errors.Is(err, index.ErrProviderShape) {
		t.Fatalf("err = %v, want ErrProviderShape（向量数不符）", err)
	}

	f3 := newFakeServer(t, 4, map[string][]float64{"deploy": {1, 0, 0, 0}})
	f3.emptyVec = true
	p3 := newProvider(t, f3, nil)
	_, err = p3.Embed(t.Context(), []string{"deploy a"})
	if !errors.Is(err, index.ErrProviderShape) {
		t.Fatalf("err = %v, want ErrProviderShape（空向量）", err)
	}
}

// TestEmbedTruncation：超长输入在句读边界截断 + OnTruncate 触发
// （rune 数口径）；预算内输入不截断。
func TestEmbedTruncation(t *testing.T) {
	f := newFakeServer(t, 4, map[string][]float64{"deploy": {1, 0, 0, 0}})
	var calls [][2]int
	long := "一二三四五六七。八九十一二三四" // 15 runes，。在第 8 rune
	p := newProvider(t, f, func(c *Config) {
		c.MaxInputChars = 10
		c.OnTruncate = func(original, kept int) { calls = append(calls, [2]int{original, kept}) }
	})
	if _, err := p.Embed(t.Context(), []string{long, "deploy ok"}); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies[0].Input[0]; got != "一二三四五六七。" {
		t.Fatalf("truncated input = %q, want sentence-boundary cut", got)
	}
	if len(calls) != 1 || calls[0] != [2]int{15, 8} {
		t.Fatalf("onTruncate calls = %v, want [(15, 8)]", calls)
	}
}

// TestEmbedCtxCancel：调用方取消即时返回且保留 errors.Is 链。
func TestEmbedCtxCancel(t *testing.T) {
	f := newFakeServer(t, 4, map[string][]float64{"deploy": {1, 0, 0, 0}})
	p := newProvider(t, f, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Embed(ctx, []string{"deploy a"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled chain", err)
	}
}

// TestE2EWithMemIndex：MemIndex + httptest provider 全链路——
// Upsert/Search/Rebuild 走真 HTTP 线格式。
func TestE2EWithMemIndex(t *testing.T) {
	f := newFakeServer(t, 4, map[string][]float64{"deploy": {1, 0, 0, 0}})
	p := newProvider(t, f, nil)
	s := store.NewMemoryStore()
	it := store.MemoryItem{
		ID: "e1", Namespace: []string{"tenant:a"}, Kind: store.KindEpisode,
		Content: "deploy via kubectl", Status: store.StatusActive, Confidence: 1.0,
		Taint:      store.TaintTrusted,
		SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: "s1", Seq: 1}},
	}
	saved, err := s.Put(t.Context(), it, store.PutMemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.NewMemIndex(s, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(t.Context(), saved); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.Search(t.Context(), []string{"tenant:a"}, "deploy", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Item.ID != "e1" {
		t.Fatalf("hits = %+v, want e1（真 HTTP 全链路召回）", hits)
	}
	if err := idx.Rebuild(t.Context()); err != nil {
		t.Fatal(err)
	}
	hits, err = idx.Search(t.Context(), []string{"tenant:a"}, "deploy", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("after rebuild hits = %d, want 1", len(hits))
	}
}

// TestLiveEmbed 是 env-gated 真网 smoke（对齐仓库 TestLive 先例）：
// PULSE_OPENAI_EMBED_API_KEY 未配置自动 skip；BASE_URL 可指向兼容网关。
func TestLiveEmbed(t *testing.T) {
	key := os.Getenv("PULSE_OPENAI_EMBED_API_KEY")
	if key == "" {
		t.Skip("PULSE_OPENAI_EMBED_API_KEY not set; skipping live embed smoke")
	}
	p, err := New(Config{
		BaseURL: os.Getenv("PULSE_OPENAI_EMBED_BASE_URL"),
		Model:   os.Getenv("PULSE_OPENAI_EMBED_MODEL"),
		APIKey:  key,
	})
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := p.Embed(t.Context(), []string{"pulse live smoke test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		t.Fatalf("live embed = %d vectors, first len %d", len(vecs), len(vecs[0]))
	}
	fmt.Printf("live embed dims = %d\n", len(vecs[0]))
}
