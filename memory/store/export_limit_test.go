package store

import "testing"

// TestExportItemsIgnoresLimit：导出是**全量**语义——查询面的分页参数不得
// 静默截断冷备（少导不算错，比报错更危险）。
func TestExportItemsIgnoresLimit(t *testing.T) {
	ctx := t.Context()
	s := NewMemoryStore()
	ns := []string{"tenant:a"}
	for _, id := range []string{"a1", "a2", "a3"} {
		it := MemoryItem{
			ID:         id,
			Namespace:  ns,
			Kind:       KindDecision,
			Content:    id,
			Status:     StatusActive,
			Confidence: 1.0,
			Taint:      TaintTrusted,
			SourceRefs: []SourceRef{{Type: SourceSession, SessionID: "s1", Seq: 1}},
		}
		if _, err := s.Put(ctx, it, PutMemoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	items, err := ExportItems(ctx, s, MemoryQuery{Namespace: ns, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("ExportItems with Limit:2 returned %d items, want the full 3", len(items))
	}
}
