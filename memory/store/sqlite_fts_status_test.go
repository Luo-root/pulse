//go:build !plan9 && !js

package store

import "testing"

// TestSQLiteSearchFTSStatusFilter：FTS 召回只给 Active——被 Supersede /
// Revoke 的事实与未过审批的 Pending 一律不进召回（与 Search 同口径）。
func TestSQLiteSearchFTSStatusFilter(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	ns := []string{"tenant:a", "project:p1"}

	put := func(t *testing.T, it MemoryItem) {
		t.Helper()
		if _, err := s.Put(ctx, it, PutMemoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	put(t, sqliteItem("live", "zebra deploy policy is A"))
	put(t, sqliteItem("old", "zebra deploy policy is B"))
	if _, err := s.Supersede(ctx, "old", sqliteItem("old-v2", "zebra deploy policy is C")); err != nil {
		t.Fatal(err)
	}
	put(t, sqliteItem("gone", "zebra deploy policy is D"))
	if err := s.Revoke(ctx, "gone", "revoked by test"); err != nil {
		t.Fatal(err)
	}
	pend := sqliteItem("pend", "zebra deploy policy is E")
	pend.Status = StatusPending
	put(t, pend)

	hits, err := s.SearchFTS(ctx, ns, "zebra", 0)
	if err != nil {
		t.Fatal(err)
	}
	has := func(id string) bool {
		for _, h := range hits {
			if h.Item.ID == id {
				return true
			}
		}
		return false
	}
	if !has("live") {
		t.Fatalf("SearchFTS must return the active hit, got %v", hits)
	}
	for _, bad := range []string{"old", "gone", "pend"} {
		if has(bad) {
			t.Fatalf("SearchFTS returned non-active item %q; hits=%v", bad, hits)
		}
	}

	// 对照组：Search 的口径本来就是 Active-only，两路口径必须一致。
	sh, err := s.Search(ctx, MemoryQuery{Namespace: ns, Query: "zebra"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range sh {
		if h.Item.Status != StatusActive {
			t.Fatalf("Search returned non-active item %q (status=%s)", h.Item.ID, h.Item.Status)
		}
	}
}
