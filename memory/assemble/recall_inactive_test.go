package assemble

import (
	"context"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/memory/store"
)

// fakeFTSSearcher 模拟「宿主自实现的 FTS 召回缝」：它只按 query 返回条目，
// 不做状态复核——融合层必须自己兜底（与 semantic 路同口径）。
type fakeFTSSearcher struct {
	store.MemoryStore
	hits []store.MemoryHit
}

func (f fakeFTSSearcher) SearchFTS(ctx context.Context, ns []string, match string, limit int) ([]store.MemoryHit, error) {
	return f.hits, nil
}

// TestAssembleFusionDropsInactiveKeywordHits：召回来源不保证复核状态时，
// keyword 路的非 Active 命中一律不得进组装产物（被 Revoke / Supersede 的
// 事实与未过审批的 Pending 都不进上下文）。
func TestAssembleFusionDropsInactiveKeywordHits(t *testing.T) {
	ctx := t.Context()
	inner := store.NewMemoryStore()

	revoked := item("dead", store.KindLesson, "zebra body revoked")
	revoked.Status = store.StatusRevoked
	pending := item("waiting", store.KindLesson, "zebra body pending")
	pending.Status = store.StatusPending

	fs := fakeFTSSearcher{
		MemoryStore: inner,
		hits: []store.MemoryHit{
			{Item: revoked},
			{Item: pending},
			{Item: item("alive", store.KindLesson, "zebra body alive")},
		},
	}
	a := NewDefaultAssembler(fs, nil, Budget{})
	ac, err := a.Assemble(ctx, AssembleInput{Namespace: []string{"tenant:a"}, Query: "zebra"})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, m := range ac.Messages {
		b.WriteString(m.Text())
		b.WriteString("\n")
	}
	out := b.String()
	if !strings.Contains(out, "zebra body alive") {
		t.Fatalf("active keyword hit missing: %q", out)
	}
	for _, leaked := range []string{"zebra body revoked", "zebra body pending"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("inactive keyword hit leaked into assembly (%q): %q", leaked, out)
		}
	}
}
