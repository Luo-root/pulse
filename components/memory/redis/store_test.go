//go:build integration

package redis

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/memory"
	"github.com/Luo-root/pulse/components/schema"
)

func redisAddr() string {
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		return addr
	}
	return "localhost:6379"
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
	cfg.Addr = redisAddr()
	cfg.KeyPrefix = "pulse_test:"
	store, err := NewStore(cfg)
	if err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	return store
}

func newTestRetriever(t *testing.T, store *Store) *Retriever {
	t.Helper()
	cfg := DefaultRetrieverConfig()
	cfg.Addr = redisAddr()
	cfg.KeyPrefix = "pulse_test:"
	cfg.IndexName = "pulse_test:idx"
	cfg.VectorDim = 4
	r, err := NewRetrieverFromStore(store, cfg, mockEmbedding)
	if err != nil {
		t.Skipf("RediSearch not available: %v", err)
	}
	return r
}

func cleanup(t *testing.T) {
	t.Helper()
	store, err := NewStore(DefaultStoreConfig())
	if err != nil {
		return
	}
	defer store.Close()
	// 删除旧索引（如果 schema 变了需要重建）
	store.GetClient().FTDropIndex(context.Background(), "pulse_test:idx")
	keys, _ := store.GetClient().Keys(context.Background(), "pulse_test:*").Result()
	if len(keys) > 0 {
		store.GetClient().Del(context.Background(), keys...)
	}
}

// ============================================================================
// Store 测试
// ============================================================================

func TestStore_Save(t *testing.T) {
	cleanup(t)
	defer cleanup(t)

	store := newTestStore(t)
	defer store.Close()

	ctx := context.Background()
	msgs := []*schema.Message{
		schema.UserMessage("hello world"),
		schema.AssistantMessage("hi there", ""),
	}
	if err := store.Save(ctx, "test-session", msgs); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	result, err := store.GetSession(ctx, "test-session")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
}

func TestStore_ClearSession(t *testing.T) {
	cleanup(t)
	defer cleanup(t)

	store := newTestStore(t)
	defer store.Close()

	ctx := context.Background()
	store.Save(ctx, "sess-clear", []*schema.Message{schema.UserMessage("hello")})
	store.ClearSession(ctx, "sess-clear")

	result, _ := store.GetSession(ctx, "sess-clear")
	if len(result) != 0 {
		t.Fatalf("expected 0 after clear, got %d", len(result))
	}
}

func TestStore_SessionIsolation(t *testing.T) {
	cleanup(t)
	defer cleanup(t)

	store := newTestStore(t)
	defer store.Close()

	ctx := context.Background()
	store.Save(ctx, "sess-A", []*schema.Message{schema.UserMessage("secret A")})
	store.Save(ctx, "sess-B", []*schema.Message{schema.UserMessage("secret B")})

	a, _ := store.GetSession(ctx, "sess-A")
	b, _ := store.GetSession(ctx, "sess-B")
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

func TestRetriever_Recall_Keyword(t *testing.T) {
	cleanup(t)
	defer cleanup(t)

	store := newTestStore(t)
	defer store.Close()
	retriever := newTestRetriever(t, store)
	defer retriever.Close()

	ctx := context.Background()
	// 直接写入 Redis Hash（模拟 Store 已存储）
	msgs := []*schema.Message{
		schema.UserMessage("Python programming is great"),
		schema.AssistantMessage("Yes Python is versatile", ""),
		schema.UserMessage("Go is good for concurrency"),
	}
	store.Save(ctx, "sess-kw", msgs)

	time.Sleep(500 * time.Millisecond)

	results, err := retriever.Recall(ctx, "sess-kw", "Python", 3)
	if err != nil {
		t.Fatalf("Recall failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for keyword search")
	}
}

func TestRetriever_Recall_Vector(t *testing.T) {
	cleanup(t)
	defer cleanup(t)

	store := newTestStore(t)
	defer store.Close()
	retriever := newTestRetriever(t, store)
	defer retriever.Close()

	ctx := context.Background()
	store.Save(ctx, "sess-vec", []*schema.Message{
		schema.UserMessage("machine learning basics"),
		schema.AssistantMessage("ML is a subset of AI", ""),
	})

	// 手动添加 embedding
	ids, _ := store.GetClient().SMembers(ctx, store.sessKey("sess-vec")).Result()
	for _, id := range ids {
		content, _ := store.GetClient().HGet(ctx, store.msgKey(id), "content").Result()
		vec, _ := mockEmbedding(ctx, content)
		store.GetClient().HSet(ctx, store.msgKey(id), "embedding", vecToBytes(vec))
	}

	// 等待索引更新
	time.Sleep(1 * time.Second)

	results, err := retriever.Recall(ctx, "sess-vec", "artificial intelligence", 3)
	if err != nil {
		t.Fatalf("Recall failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for vector search")
	}
}

// ============================================================================
// CompositeLongTermStore 测试
// ============================================================================

func TestComposite_FullFlow(t *testing.T) {
	cleanup(t)
	defer cleanup(t)

	store := newTestStore(t)
	retriever := newTestRetriever(t, store)
	composite := memory.NewCompositeLongTermStore(store, retriever)

	ctx := context.Background()

	// Save
	msgs := []*schema.Message{
		schema.UserMessage("What is Go?"),
		schema.AssistantMessage("Go is a programming language", ""),
		schema.UserMessage("Tell me more about concurrency"),
		schema.AssistantMessage("Go has goroutines and channels", ""),
	}
	if err := composite.Save(ctx, "sess-full", msgs); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// GetSession
	session, err := composite.GetSession(ctx, "sess-full")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if len(session) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(session))
	}

	// Recall
	time.Sleep(500 * time.Millisecond)
	results, err := composite.Recall(ctx, "sess-full", "concurrency", 3)
	if err != nil {
		t.Fatalf("Recall failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected recall results")
	}

	// ClearSession
	composite.ClearSession(ctx, "sess-full")
	session, _ = composite.GetSession(ctx, "sess-full")
	if len(session) != 0 {
		t.Fatalf("expected 0 after clear, got %d", len(session))
	}
}

// ============================================================================
// 混合组合测试
// ============================================================================

func TestComposite_WithHook(t *testing.T) {
	cleanup(t)
	defer cleanup(t)

	store := newTestStore(t)
	retriever := newTestRetriever(t, store)
	composite := memory.NewCompositeLongTermStore(store, retriever)

	// 注册钩子
	hookCalled := false
	composite.AddHook(func(event memory.StoreEvent) {
		hookCalled = true
	})

	ctx := context.Background()
	composite.Save(ctx, "sess-hook", []*schema.Message{schema.UserMessage("test")})

	if !hookCalled {
		t.Fatal("hook was not called after Save")
	}
}
