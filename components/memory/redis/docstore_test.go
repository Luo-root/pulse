//go:build integration

package redis

import (
	"context"
	"testing"

	"github.com/Luo-root/pulse/components/schema"
)

func cleanupDocs(t *testing.T, prefix string) {
	t.Helper()
	s, err := NewStore(&StoreConfig{Addr: redisAddr(), KeyPrefix: prefix})
	if err != nil {
		return
	}
	defer s.Close()
	keys, _ := s.GetClient().Keys(context.Background(), prefix+"doc:*").Result()
	if len(keys) > 0 {
		s.GetClient().Del(context.Background(), keys...)
	}
	keys, _ = s.GetClient().Keys(context.Background(), prefix+"coll:*").Result()
	if len(keys) > 0 {
		s.GetClient().Del(context.Background(), keys...)
	}
}

func newTestRedisDocStore(t *testing.T) *DocStore {
	t.Helper()
	cfg := DefaultStoreConfig()
	cfg.Addr = redisAddr()
	cfg.KeyPrefix = "pulse_doctest:"
	store, err := NewStore(cfg)
	if err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return NewDocStore(store)
}

func TestRedisDocStore_SaveAndRetrieve(t *testing.T) {
	cleanupDocs(t, "pulse_doctest:")
	defer cleanupDocs(t, "pulse_doctest:")

	ds := newTestRedisDocStore(t)
	ctx := context.Background()

	docs := []*schema.Document{
		{ID: "d1", Content: "Go is a compiled language"},
		{ID: "d2", Content: "Python is an interpreted language"},
		{ID: "d3", Content: "Go supports concurrency with goroutines"},
	}
	if err := ds.SaveDocuments(ctx, "languages", docs); err != nil {
		t.Fatalf("SaveDocuments failed: %v", err)
	}

	// 关键词检索
	results, err := ds.RecallDocuments(ctx, "languages", "Go", 5)
	if err != nil {
		t.Fatalf("RecallDocuments failed: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results for 'Go', got %d", len(results))
	}
}

func TestRedisDocStore_MetaData(t *testing.T) {
	cleanupDocs(t, "pulse_doctest:")
	defer cleanupDocs(t, "pulse_doctest:")

	ds := newTestRedisDocStore(t)
	ctx := context.Background()

	docs := []*schema.Document{
		{
			ID:      "meta1",
			Content: "测试文档",
			MetaData: map[string]any{
				"author": "张三",
				"type":   "技术文档",
			},
		},
	}
	ds.SaveDocuments(ctx, "docs", docs)

	results, _ := ds.GetDocuments(ctx, "docs")
	if len(results) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(results))
	}
	if results[0].MetaData["author"] != "张三" {
		t.Fatalf("metadata mismatch: %v", results[0].MetaData)
	}
}

func TestRedisDocStore_CollectionIsolation(t *testing.T) {
	cleanupDocs(t, "pulse_doctest:")
	defer cleanupDocs(t, "pulse_doctest:")

	ds := newTestRedisDocStore(t)
	ctx := context.Background()

	ds.SaveDocuments(ctx, "col_a", []*schema.Document{{ID: "a1", Content: "A的数据"}})
	ds.SaveDocuments(ctx, "col_b", []*schema.Document{{ID: "b1", Content: "B的数据"}})

	aDocs, _ := ds.GetDocuments(ctx, "col_a")
	bDocs, _ := ds.GetDocuments(ctx, "col_b")

	if len(aDocs) != 1 || aDocs[0].Content != "A的数据" {
		t.Fatalf("collection A mismatch")
	}
	if len(bDocs) != 1 || bDocs[0].Content != "B的数据" {
		t.Fatalf("collection B mismatch")
	}
}

func TestRedisDocStore_DeleteCollection(t *testing.T) {
	cleanupDocs(t, "pulse_doctest:")
	defer cleanupDocs(t, "pulse_doctest:")

	ds := newTestRedisDocStore(t)
	ctx := context.Background()

	ds.SaveDocuments(ctx, "to_delete", []*schema.Document{{ID: "d1", Content: "删除我"}})
	ds.DeleteCollection(ctx, "to_delete")

	docs, _ := ds.GetDocuments(ctx, "to_delete")
	if len(docs) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(docs))
	}
}
