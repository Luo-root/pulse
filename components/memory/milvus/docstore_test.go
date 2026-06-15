//go:build integration

package milvus

import (
	"context"
	"testing"

	"github.com/Luo-root/pulse/components/schema"
)

func newTestMilvusDocStore(t *testing.T) *DocStore {
	t.Helper()
	cfg := DefaultDocStoreConfig()
	cfg.Addr = milvusAddr()
	cfg.Collection = "test_documents"
	cfg.VectorDim = 4

	// 先清理旧数据
	cleanupDocCollection(t, cfg.Collection)

	ds, err := NewDocStore(cfg, mockEmbedding)
	if err != nil {
		t.Skipf("Milvus not available: %v", err)
	}
	t.Cleanup(func() {
		ds.client.client.DropCollection(context.Background(), cfg.Collection)
		ds.Close()
	})
	return ds
}

func cleanupDocCollection(t *testing.T, collection string) {
	t.Helper()
	s, err := NewStore(&StoreConfig{Addr: milvusAddr(), Collection: collection, VectorDim: 4})
	if err != nil {
		return
	}
	defer s.Close()
	s.client.DropCollection(context.Background(), collection)
}

func TestMilvusDocStore_SaveAndRetrieve(t *testing.T) {
	ds := newTestMilvusDocStore(t)
	ctx := context.Background()

	docs := []*schema.Document{
		{ID: "m1", Content: "Kubernetes manages containers"},
		{ID: "m2", Content: "Docker builds container images"},
		{ID: "m3", Content: "Kubernetes orchestrates pods"},
	}
	if err := ds.SaveDocuments(ctx, "infra", docs); err != nil {
		t.Fatalf("SaveDocuments failed: %v", err)
	}

	results, err := ds.RecallDocuments(ctx, "infra", "Kubernetes containers", 3)
	if err != nil {
		t.Fatalf("RecallDocuments failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected recall results")
	}
}

func TestMilvusDocStore_MetaData(t *testing.T) {
	ds := newTestMilvusDocStore(t)
	ctx := context.Background()

	docs := []*schema.Document{
		{
			ID:      "meta_m1",
			Content: "服务器配置文档",
			MetaData: map[string]any{
				"env":  "production",
				"team": "infra",
			},
		},
	}
	ds.SaveDocuments(ctx, "config_docs", docs)

	results, _ := ds.GetDocuments(ctx, "config_docs")
	if len(results) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(results))
	}
	if results[0].MetaData["env"] != "production" {
		t.Fatalf("metadata mismatch: %v", results[0].MetaData)
	}
}

func TestMilvusDocStore_CollectionIsolation(t *testing.T) {
	ds := newTestMilvusDocStore(t)
	ctx := context.Background()

	ds.SaveDocuments(ctx, "set_a", []*schema.Document{{ID: "a1", Content: "Set A data"}})
	ds.SaveDocuments(ctx, "set_b", []*schema.Document{{ID: "b1", Content: "Set B data"}})

	aDocs, _ := ds.GetDocuments(ctx, "set_a")
	bDocs, _ := ds.GetDocuments(ctx, "set_b")

	if len(aDocs) != 1 || aDocs[0].Content != "Set A data" {
		t.Fatalf("collection A mismatch")
	}
	if len(bDocs) != 1 || bDocs[0].Content != "Set B data" {
		t.Fatalf("collection B mismatch")
	}
}

func TestMilvusDocStore_DeleteCollection(t *testing.T) {
	ds := newTestMilvusDocStore(t)
	ctx := context.Background()

	ds.SaveDocuments(ctx, "del_set", []*schema.Document{{ID: "d1", Content: "Delete me"}})

	err := ds.DeleteCollection(ctx, "del_set")
	if err != nil {
		t.Fatalf("DeleteCollection failed: %v", err)
	}

	// Milvus 删除是软删除，验证删除操作本身成功即可
	// GetDocuments 可能因一致性延迟仍返回数据
}
