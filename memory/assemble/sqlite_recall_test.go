//go:build !plan9 && !js

package assemble

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/memory/store"
)

// TestAssembleSQLiteRecallExcludesInactive：SQLite 后端（FTS token 前缀是
// 召回首选路）下，被 Supersede / Revoke 的事实与未过审批的 Pending 都不得
// 出现在组装产物里——与内存后端、与 Search 同口径。
func TestAssembleSQLiteRecallExcludesInactive(t *testing.T) {
	ctx := t.Context()
	st, err := store.NewSQLiteStore(ctx, "file:"+filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ns := []string{"tenant:probe"}
	mk := func(id, content string) store.MemoryItem {
		return store.MemoryItem{
			ID:         id,
			Namespace:  ns,
			Kind:       store.KindEnvironment,
			Content:    content,
			Status:     store.StatusActive,
			Confidence: 1.0,
			Taint:      store.TaintTrusted,
			SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: "s1", Seq: 1}},
		}
	}
	put := func(t *testing.T, it store.MemoryItem) {
		t.Helper()
		if _, err := st.Put(ctx, it, store.PutMemoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	put(t, mk("live", "zebra deploy policy is A")) // active（对照）
	put(t, mk("old", "zebra deploy policy is B"))  // 即将被取代
	put(t, mk("gone", "zebra deploy policy is D")) // 即将被撤销
	pend := mk("pend", "zebra deploy policy is E") // 未过审批
	pend.Status = store.StatusPending
	put(t, pend)
	if _, err := st.Supersede(ctx, "old", mk("old-v2", "zebra deploy policy is C")); err != nil {
		t.Fatal(err)
	}
	if err := st.Revoke(ctx, "gone", "revoked by test"); err != nil {
		t.Fatal(err)
	}

	a := NewDefaultAssembler(st, nil, Budget{})
	ac, err := a.Assemble(ctx, AssembleInput{Namespace: ns, Query: "zebra"})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, m := range ac.Messages {
		b.WriteString(m.Text())
		b.WriteString("\n")
	}
	out := b.String()
	if !strings.Contains(out, "is A") {
		t.Fatalf("active FTS hit missing from assembly: %q", out)
	}
	for _, leaked := range []string{"is B", "is D", "is E"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("inactive item leaked into assembly (%q): %q", leaked, out)
		}
	}
}
