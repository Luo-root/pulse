package gorm

import (
	"context"
	"testing"

	"github.com/Luo-root/pulse/components/schema"
)

func newTestDocStore(t *testing.T) *GORMDocStore {
	t.Helper()
	dbPath := t.TempDir() + "/docstore_test.db"
	storeCfg := DefaultConfig()
	storeCfg.DBPath = dbPath
	storeCfg.ChunkSize = 0

	store, err := NewGORMStore(storeCfg, nil)
	if err != nil {
		t.Fatalf("NewGORMStore failed: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	return NewGORMDocStore(store)
}

func TestDocStore_SaveAndRetrieve(t *testing.T) {
	ds := newTestDocStore(t)
	ctx := context.Background()

	docs := []*schema.Document{
		{ID: "doc1", Content: "张三是技术部的软件工程师"},
		{ID: "doc2", Content: "李四是产品部的产品经理"},
		{ID: "doc3", Content: "王五是技术部的测试工程师"},
	}
	if err := ds.SaveDocuments(ctx, "employees", docs); err != nil {
		t.Fatalf("SaveDocuments failed: %v", err)
	}

	// 检索
	results, err := ds.RecallDocuments(ctx, "employees", "技术部", 5)
	if err != nil {
		t.Fatalf("RecallDocuments failed: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results for '技术部', got %d", len(results))
	}
}

func TestDocStore_MetaData(t *testing.T) {
	ds := newTestDocStore(t)
	ctx := context.Background()

	docs := []*schema.Document{
		{
			ID:      "meta1",
			Content: "产品A的描述",
			MetaData: map[string]any{
				"category": "电子产品",
				"price":    999,
			},
		},
	}
	ds.SaveDocuments(ctx, "products", docs)

	results, err := ds.GetDocuments(ctx, "products")
	if err != nil {
		t.Fatalf("GetDocuments failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(results))
	}
	if results[0].MetaData["category"] != "电子产品" {
		t.Fatalf("metadata mismatch: %v", results[0].MetaData)
	}
}

func TestDocStore_CollectionIsolation(t *testing.T) {
	ds := newTestDocStore(t)
	ctx := context.Background()

	ds.SaveDocuments(ctx, "col_a", []*schema.Document{{ID: "a1", Content: "collection A"}})
	ds.SaveDocuments(ctx, "col_b", []*schema.Document{{ID: "b1", Content: "collection B"}})

	aDocs, _ := ds.GetDocuments(ctx, "col_a")
	bDocs, _ := ds.GetDocuments(ctx, "col_b")

	if len(aDocs) != 1 || aDocs[0].Content != "collection A" {
		t.Fatalf("collection A mismatch")
	}
	if len(bDocs) != 1 || bDocs[0].Content != "collection B" {
		t.Fatalf("collection B mismatch")
	}
}

func TestDocStore_DeleteCollection(t *testing.T) {
	ds := newTestDocStore(t)
	ctx := context.Background()

	ds.SaveDocuments(ctx, "to_delete", []*schema.Document{
		{ID: "d1", Content: "will be deleted"},
	})
	ds.DeleteCollection(ctx, "to_delete")

	docs, _ := ds.GetDocuments(ctx, "to_delete")
	if len(docs) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(docs))
	}
}

func TestDocStore_EmptyCollection(t *testing.T) {
	ds := newTestDocStore(t)
	ctx := context.Background()

	docs, err := ds.GetDocuments(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetDocuments failed: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("expected 0 docs, got %d", len(docs))
	}
}

func TestDocStore_AutoID(t *testing.T) {
	ds := newTestDocStore(t)
	ctx := context.Background()

	docs := []*schema.Document{
		{Content: "auto id test"},
	}
	ds.SaveDocuments(ctx, "autoid", docs)

	results, _ := ds.GetDocuments(ctx, "autoid")
	if len(results) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(results))
	}
	if results[0].ID == "" {
		t.Fatal("expected auto-generated ID")
	}
}
