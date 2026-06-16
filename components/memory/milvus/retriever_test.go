//go:build integration

package milvus

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/memory"
	"github.com/Luo-root/pulse/components/schema"
)

func milvusAddr() string {
	if addr := os.Getenv("MILVUS_ADDR"); addr != "" {
		return addr
	}
	return "localhost:19530"
}

func mockEmbedding(ctx context.Context, text string) ([]float32, error) {
	vec := []float32{0.1, 0.2, 0.3, 0.4}
	for i, c := range text {
		vec[i%4] += float32(c) * 0.001
	}
	return vec, nil
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	cfg := DefaultStoreConfig()
	cfg.Addr = milvusAddr()
	cfg.Collection = "pulse_test_messages"
	cfg.VectorDim = 4
	s, err := NewStore(cfg)
	if err != nil {
		t.Skipf("Milvus not available: %v", err)
	}
	return s
}

func newTestRetrieverFromStore(t *testing.T, s *Store) *Retriever {
	t.Helper()
	r, err := NewRetrieverFromStore(s, mockEmbedding)
	if err != nil {
		t.Skipf("Milvus retriever creation failed: %v", err)
	}
	return r
}

func cleanupMilvus(t *testing.T, s *Store) {
	t.Helper()
	// 删除所有测试数据
	s.client.Delete(context.Background(), s.config.Collection, "", "session_id != \"\"")
}

func cleanupAll(t *testing.T) {
	t.Helper()
	s := newTestStore(t)
	if s == nil {
		return
	}
	defer s.Close()
	s.client.DropCollection(context.Background(), s.config.Collection)
}

// ============================================================================
// Store 测试
// ============================================================================

func TestStore_Save(t *testing.T) {
	cleanupAll(t)
	store := newTestStore(t)
	defer store.Close()
	defer cleanupAll(t)

	ctx := context.Background()
	msgs := []*schema.Message{
		schema.UserMessage("hello world"),
		schema.AssistantMessage("hi there", ""),
	}
	if err := store.Save(ctx, "sess_save", msgs); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	result, err := store.GetSession(ctx, "sess_save")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
}

func TestStore_ClearSession(t *testing.T) {
	cleanupAll(t)
	store := newTestStore(t)
	defer store.Close()
	defer cleanupAll(t)

	ctx := context.Background()
	err := store.Save(ctx, "sDel", []*schema.Message{schema.UserMessage("to be deleted")})
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// 检查数据存在
	before, _ := store.GetSession(ctx, "sDel")
	t.Logf("Before clear: %d messages", len(before))

	err = store.ClearSession(ctx, "sDel")
	if err != nil {
		t.Logf("ClearSession error: %v", err)
	}

	// 等待一下
	time.Sleep(500 * time.Millisecond)

	after, _ := store.GetSession(ctx, "sDel")
	t.Logf("After clear: %d messages", len(after))
	if len(after) != 0 {
		t.Fatalf("expected 0 after clear, got %d", len(after))
	}
}

func TestStore_SessionIsolation(t *testing.T) {
	cleanupAll(t)
	store := newTestStore(t)
	defer store.Close()
	defer cleanupAll(t)

	ctx := context.Background()
	store.Save(ctx, "sA", []*schema.Message{schema.UserMessage("secret A")})
	store.Save(ctx, "sB", []*schema.Message{schema.UserMessage("secret B")})

	a, _ := store.GetSession(ctx, "sA")
	b, _ := store.GetSession(ctx, "sB")
	if len(a) != 1 || a[0].Content != "secret A" {
		t.Fatalf("session A mismatch")
	}
	if len(b) != 1 || b[0].Content != "secret B" {
		t.Fatalf("session B mismatch")
	}
}

// ============================================================================
// Retriever 测试
// ============================================================================

func TestRetriever_RebuildIndex(t *testing.T) {
	cleanupAll(t)
	store := newTestStore(t)
	defer store.Close()
	defer cleanupAll(t)

	retriever := newTestRetrieverFromStore(t, store)
	defer retriever.Close()

	ctx := context.Background()
	items := []IndexItem{
		{ID: "m1", SessionID: "s1", Role: "user", Content: "hello world", Timestamp: 1000, Embedding: []float32{0.1, 0.2, 0.3, 0.4}},
		{ID: "m2", SessionID: "s1", Role: "assistant", Content: "hi there", Timestamp: 1001, Embedding: []float32{0.5, 0.6, 0.7, 0.8}},
	}
	if err := retriever.RebuildIndex(ctx, items); err != nil {
		t.Fatalf("RebuildIndex failed: %v", err)
	}
}

func TestRetriever_Recall(t *testing.T) {
	cleanupAll(t)
	store := newTestStore(t)
	defer store.Close()
	defer cleanupAll(t)

	retriever := newTestRetrieverFromStore(t, store)
	defer retriever.Close()

	ctx := context.Background()
	items := []IndexItem{
		{ID: "r1", SessionID: "s2", Role: "user", Content: "Python programming", Timestamp: 2000, Embedding: []float32{0.1, 0.2, 0.3, 0.4}},
		{ID: "r2", SessionID: "s2", Role: "assistant", Content: "Python is great", Timestamp: 2001, Embedding: []float32{0.2, 0.3, 0.4, 0.5}},
	}
	retriever.RebuildIndex(ctx, items)

	results, err := retriever.Recall(ctx, "s2", "Python", 3)
	if err != nil {
		t.Fatalf("Recall failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected recall results")
	}
}

func TestRetriever_SessionIsolation(t *testing.T) {
	cleanupAll(t)
	store := newTestStore(t)
	defer store.Close()
	defer cleanupAll(t)

	retriever := newTestRetrieverFromStore(t, store)
	defer retriever.Close()

	ctx := context.Background()
	items := []IndexItem{
		{ID: "i1", SessionID: "sA", Role: "user", Content: "secret A", Timestamp: 3000, Embedding: []float32{0.1, 0.2, 0.3, 0.4}},
		{ID: "i2", SessionID: "sB", Role: "user", Content: "secret B", Timestamp: 3001, Embedding: []float32{0.5, 0.6, 0.7, 0.8}},
	}
	retriever.RebuildIndex(ctx, items)

	a, _ := retriever.Recall(ctx, "sA", "secret", 3)
	b, _ := retriever.Recall(ctx, "sB", "secret", 3)
	for _, msg := range a {
		if msg.Content == "secret B" {
			t.Fatal("session A got session B's message")
		}
	}
	for _, msg := range b {
		if msg.Content == "secret A" {
			t.Fatal("session B got session A's message")
		}
	}
}

// ============================================================================
// CompositeLongTermStore 测试
// ============================================================================

func TestComposite_FullFlow(t *testing.T) {
	cleanupAll(t)
	store := newTestStore(t)
	defer store.Close()
	defer cleanupAll(t)

	retriever := newTestRetrieverFromStore(t, store)
	defer retriever.Close()

	composite := memory.NewCompositeLongTermStore(store, retriever)
	ctx := context.Background()

	// Save
	msgs := []*schema.Message{
		schema.UserMessage("What is Go?"),
		schema.AssistantMessage("Go is a programming language", ""),
		schema.UserMessage("Tell me more about concurrency"),
		schema.AssistantMessage("Go has goroutines and channels", ""),
	}
	if err := composite.Save(ctx, "sFull", msgs); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// GetSession
	session, err := composite.GetSession(ctx, "sFull")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if len(session) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(session))
	}

	// Recall（用 Retriever 的向量检索）
	items := []IndexItem{
		{ID: "c1", SessionID: "sFull", Role: "user", Content: "concurrency", Timestamp: 9999, Embedding: []float32{0.1, 0.2, 0.3, 0.4}},
	}
	retriever.RebuildIndex(ctx, items)
	results, err := composite.Recall(ctx, "sFull", "concurrency", 3)
	if err != nil {
		t.Fatalf("Recall failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected recall results")
	}

	// ClearSession
	composite.ClearSession(ctx, "sFull")
	session, _ = composite.GetSession(ctx, "sFull")
	if len(session) != 0 {
		t.Fatalf("expected 0 after clear, got %d", len(session))
	}
}

func TestRetriever_IndexReady(t *testing.T) {
	cleanupAll(t)
	store := newTestStore(t)
	defer store.Close()
	defer cleanupAll(t)

	retriever := newTestRetrieverFromStore(t, store)
	defer retriever.Close()
	if !retriever.IndexReady() {
		t.Fatal("expected IndexReady to be true")
	}
}
