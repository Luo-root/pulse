package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// itemOf 构造一个可入库的最小 item（显式 Confidence=1.0、session 来源）。
func itemOf(id string, ns []string, content string) MemoryItem {
	return MemoryItem{
		ID:         id,
		Namespace:  ns,
		Kind:       KindDecision,
		Content:    content,
		Status:     StatusActive,
		Confidence: 1.0,
		Taint:      TaintTrusted,
		SourceRefs: []SourceRef{{Type: SourceSession, SessionID: "s1", Seq: 7}},
	}
}

// TestNamespaceIsolation：兄弟 namespace 绝不互见；父 scope 前缀可读子
// scope；子查询看不到父级 item。
func TestNamespaceIsolation(t *testing.T) {
	s := NewMemoryStore()
	ctx := t.Context()
	tenantA := []string{"tenant:a", "project:p1"}
	tenantB := []string{"tenant:b", "project:p1"}
	if _, err := s.Put(ctx, itemOf("d1", tenantA, "use yaml config"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	// 兄弟 namespace 互查为空。
	hits, err := s.Search(ctx, MemoryQuery{Namespace: tenantB})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("cross-tenant hits = %d, want 0（不同 namespace 绝不互见）", len(hits))
	}
	if _, err := s.Get(ctx, tenantB, "d1"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("cross-tenant get: %v, want ErrItemNotFound", err)
	}
	// 父前缀（tenant:a）可见子项。
	parent := []string{"tenant:a"}
	hits, err = s.Search(ctx, MemoryQuery{Namespace: parent})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Item.ID != "d1" {
		t.Fatalf("parent hits = %+v", hits)
	}
	// 子查询看不到只属于父级的 item。
	parentOnly := itemOf("d2", parent, "tenant wide policy")
	if _, err := s.Put(ctx, parentOnly, PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	hits, err = s.Search(ctx, MemoryQuery{Namespace: tenantA})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Item.ID != "d1" {
		t.Fatalf("child query must not see parent-only items: %+v", hits)
	}
}

// TestPutValidation：来源强制、active 置信度显式、未知状态、非法 JSON。
func TestPutValidation(t *testing.T) {
	s := NewMemoryStore()
	ctx := t.Context()
	ns := []string{"tenant:a"}
	cases := []struct {
		name string
		item MemoryItem
	}{
		{"无 SourceRef", func() MemoryItem { it := itemOf("v1", ns, "x"); it.SourceRefs = nil; return it }()},
		{"来源类型未知", func() MemoryItem {
			it := itemOf("v2", ns, "x")
			it.SourceRefs = []SourceRef{{Type: "aliens", Ref: "x"}}
			return it
		}()},
		{"session 来源缺 Seq", func() MemoryItem {
			it := itemOf("v3", ns, "x")
			it.SourceRefs = []SourceRef{{Type: SourceSession, SessionID: "s1"}}
			return it
		}()},
		{"active 无显式 confidence", func() MemoryItem { it := itemOf("v4", ns, "x"); it.Confidence = 0; return it }()},
		{"未知 status", func() MemoryItem { it := itemOf("v5", ns, "x"); it.Status = "archived"; return it }()},
		{"空 content", func() MemoryItem { it := itemOf("v6", ns, "  "); return it }()},
		{"structured 非法 JSON", func() MemoryItem { it := itemOf("v7", ns, "x"); it.Structured = []byte(`{bad`); return it }()},
		{"空 namespace", func() MemoryItem { it := itemOf("v8", nil, "x"); return it }()},
		{"validUntil 早于 validFrom", func() MemoryItem {
			it := itemOf("v9", ns, "x")
			from := time.Now()
			early := from.Add(-time.Hour)
			it.ValidFrom = from
			it.ValidUntil = &early
			return it
		}()},
	}
	for _, tc := range cases {
		if _, err := s.Put(ctx, tc.item, PutMemoryOptions{}); err == nil {
			t.Errorf("%s: must be rejected", tc.name)
		}
	}
	// open string：宿主自定义 Kind/Taint 放行。
	custom := itemOf("custom", ns, "x")
	custom.Kind = MemoryKind("team-convention")
	custom.Taint = TaintLevel("partner-fed")
	if _, err := s.Put(ctx, custom, PutMemoryOptions{}); err != nil {
		t.Fatalf("open string kinds must pass: %v", err)
	}
}

// TestPutCAS：新建撞 ID、revision 冲突数据不变、匹配后前进且不可变字段
// 保留。
func TestPutCAS(t *testing.T) {
	s := NewMemoryStore()
	ctx := t.Context()
	ns := []string{"tenant:a"}
	first, err := s.Put(ctx, itemOf("d1", ns, "v1"), PutMemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 {
		t.Fatalf("revision = %d, want 1", first.Revision)
	}
	// 新建撞已存在 ID。
	if _, err := s.Put(ctx, itemOf("d1", ns, "v2"), PutMemoryOptions{}); !errors.Is(err, ErrItemExists) {
		t.Fatalf("err = %v, want ErrItemExists", err)
	}
	// CAS 不匹配 → 冲突且数据不变。
	stale := itemOf("d1", ns, "v3")
	stale.Structured = nil
	if _, err := s.Put(ctx, stale, PutMemoryOptions{ExpectedRevision: 99}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("err = %v, want ErrRevisionConflict", err)
	}
	got, _ := s.Get(ctx, ns, "d1")
	if got.Content != "v1" || got.Revision != 1 {
		t.Fatalf("conflicted put mutated data: %+v", got)
	}
	// 匹配 → revision 前进，CreatedAt/KnownAt 保留。
	updated := itemOf("d1", ns, "v2")
	if _, err := s.Put(ctx, updated, PutMemoryOptions{ExpectedRevision: first.Revision}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, ns, "d1")
	if got.Revision != 2 || got.Content != "v2" {
		t.Fatalf("updated = %+v", got)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) || !got.KnownAt.Equal(first.KnownAt) {
		t.Fatal("immutable fields must be preserved")
	}
	// ExpectedRevision>0 但 ID 不存在。
	if _, err := s.Put(ctx, itemOf("ghost", ns, "x"), PutMemoryOptions{ExpectedRevision: 1}); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("err = %v, want ErrItemNotFound", err)
	}
	// 更新路径禁止状态迁移（Supersede/Revoke 之外的翻转一律拒）。
	demotion := itemOf("d1", ns, "v2")
	demotion.Status = StatusPending
	if _, err := s.Put(ctx, demotion, PutMemoryOptions{ExpectedRevision: 2}); !errors.Is(err, ErrStatusTransition) {
		t.Fatalf("err = %v, want ErrStatusTransition（active→pending 绕过 taint gate）", err)
	}
	supersededPut := itemOf("d1", ns, "v2")
	supersededPut.Status = StatusSuperseded
	if _, err := s.Put(ctx, supersededPut, PutMemoryOptions{ExpectedRevision: 2}); !errors.Is(err, ErrStatusTransition) {
		t.Fatalf("err = %v, want ErrStatusTransition（active→superseded 绕过替代链）", err)
	}
	// 状态不变的同 Status 更新正常。
	if _, err := s.Put(ctx, itemOf("d1", ns, "v3"), PutMemoryOptions{ExpectedRevision: 2}); err != nil {
		t.Fatal(err)
	}
}

// TestSupersedeRevokeStateMachine：状态机与审计——Supersede 后旧 item 可
// 查（IncludeInactive）；Revoked 是终态；reason 走审计不进 MemoryItem。
func TestSupersedeRevokeStateMachine(t *testing.T) {
	s := NewMemoryStore()
	ctx := t.Context()
	ns := []string{"tenant:a"}
	if _, err := s.Put(ctx, itemOf("old", ns, "prefer yaml"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	// 自引用拒绝。
	if _, err := s.Supersede(ctx, "old", itemOf("old", ns, "self")); !errors.Is(err, ErrSupersedeSelf) {
		t.Fatalf("err = %v, want ErrSupersedeSelf", err)
	}
	next, err := s.Supersede(ctx, "old", itemOf("new", ns, "prefer toml"))
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != 1 || next.Status != StatusActive {
		t.Fatalf("next = %+v", next)
	}
	// 旧 item：默认 Search 不可见，IncludeInactive 可见且 Status=Superseded。
	hits, _ := s.Search(ctx, MemoryQuery{Namespace: ns})
	if len(hits) != 1 || hits[0].Item.ID != "new" {
		t.Fatalf("active hits = %+v（同一事实只有一条生效版本）", hits)
	}
	hits, _ = s.Search(ctx, MemoryQuery{Namespace: ns, IncludeInactive: true})
	if len(hits) != 2 {
		t.Fatalf("inactive-inclusive hits = %d, want 2（禁止物理删除）", len(hits))
	}
	got, _ := s.Get(ctx, ns, "old")
	if got.Status != StatusSuperseded {
		t.Fatalf("old status = %s, want superseded", got.Status)
	}
	// 对 Superseded item Revoke → 拒绝（专属哨兵：操作对象错了）。
	if err := s.Revoke(ctx, "old", "cleanup"); !errors.Is(err, ErrRevokeSuperseded) {
		t.Fatalf("err = %v, want ErrRevokeSuperseded", err)
	}
	// Revoke 生效版本：状态 + 审计。
	if err := s.Revoke(ctx, "new", "policy changed"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, ns, "new")
	if got.Status != StatusRevoked {
		t.Fatalf("new status = %s", got.Status)
	}
	if strings.Contains(got.Content, "policy changed") || got.Content != "prefer toml" {
		t.Fatal("reason must not leak into item content")
	}
	// Revoked 是终态：不可再 Supersede；Revoke 幂等。
	if _, err := s.Supersede(ctx, "new", itemOf("newer", ns, "x")); !errors.Is(err, ErrSupersedeRevoked) {
		t.Fatalf("err = %v, want ErrSupersedeRevoked", err)
	}
	if err := s.Revoke(ctx, "new", "again"); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
	// 审计：reason 在 store 审计面（MemoryItem 结构不加字段）。
	audit := s.AuditLog()
	var revokeEntry bool
	for _, e := range audit {
		if e.Action == "revoke" && e.ItemID == "new" && e.Reason == "policy changed" {
			revokeEntry = true
		}
	}
	if !revokeEntry {
		t.Fatalf("audit = %+v, want revoke entry with reason", audit)
	}
	// 不存在目标。
	if _, err := s.Supersede(ctx, "ghost", itemOf("n", ns, "x")); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("err = %v, want ErrItemNotFound", err)
	}
	if err := s.Revoke(ctx, "ghost", "x"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("err = %v, want ErrItemNotFound", err)
	}
}

// TestSearchFilters：Kind 过滤、关键词大小写不敏感、Limit 硬上限、稳定
// 排序、未命中返回空。
func TestSearchFilters(t *testing.T) {
	s := NewMemoryStore()
	ctx := t.Context()
	ns := []string{"tenant:a"}
	items := []MemoryItem{
		itemOf("a-lesson", ns, "Always run go vet before push"),
		func() MemoryItem {
			it := itemOf("b-profile", ns, "User prefers TOML config")
			it.Kind = KindProfile
			return it
		}(),
		func() MemoryItem {
			it := itemOf("c-lesson", ns, "GO VET catches unused imports")
			it.Kind = KindLesson
			return it
		}(),
	}
	for _, it := range items {
		if _, err := s.Put(ctx, it, PutMemoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Kind 过滤。
	hits, err := s.Search(ctx, MemoryQuery{Namespace: ns, Kinds: []MemoryKind{KindProfile}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Item.ID != "b-profile" {
		t.Fatalf("kind filter hits = %+v", hits)
	}
	// 关键词大小写不敏感。
	hits, _ = s.Search(ctx, MemoryQuery{Namespace: ns, Query: "go vet"})
	if len(hits) != 2 {
		t.Fatalf("keyword hits = %d, want 2", len(hits))
	}
	// 未命中不伪造。
	hits, _ = s.Search(ctx, MemoryQuery{Namespace: ns, Query: "nonexistent-thing"})
	if hits == nil || len(hits) != 0 {
		t.Fatalf("miss = %v, want empty non-nil slice", hits)
	}
	// Limit 硬上限。
	hits, _ = s.Search(ctx, MemoryQuery{Namespace: ns, Limit: 2})
	if len(hits) != 2 {
		t.Fatalf("limit hits = %d, want 2", len(hits))
	}
	// 负 Limit 非法。
	if _, err := s.Search(ctx, MemoryQuery{Namespace: ns, Limit: -1}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("err = %v, want ErrInvalidQuery", err)
	}
}

// TestScopeHelper：MemoryScope 按 fixed 顺序展开、空字段跳过。
func TestScopeHelper(t *testing.T) {
	ns := MemoryScope{TenantID: "acme", UserID: "u1", AgentID: "ag"}.Namespace()
	want := []string{"tenant:acme", "user:u1", "agent:ag"}
	if len(ns) != len(want) {
		t.Fatalf("ns = %v, want %v", ns, want)
	}
	for i := range want {
		if ns[i] != want[i] {
			t.Fatalf("ns = %v, want %v", ns, want)
		}
	}
	if len(MemoryScope{}.Namespace()) != 0 {
		t.Fatal("empty scope must expand to empty namespace")
	}
}

// TestSearchASCIIFoldParity：大小写折叠口径统一为仅 ASCII（与 SQLite 版
// 同断言——复审实测的 parity break 回归锁）：ASCII 大写命中；非 ASCII
// 重音不折叠不命中。
func TestSearchASCIIFoldParity(t *testing.T) {
	s := NewMemoryStore()
	ctx := t.Context()
	it := itemOf("d1", []string{"tenant:a"}, "Meet at CAFÉ du Nord")
	if _, err := s.Put(ctx, it, PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, MemoryQuery{Namespace: it.Namespace, Query: "NORD"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("ascii fold hits = %d, want 1", len(hits))
	}
	hits, _ = s.Search(ctx, MemoryQuery{Namespace: it.Namespace, Query: "café"})
	if len(hits) != 0 {
		t.Fatalf("non-ASCII fold must not happen: %d hits, want 0（口径：折叠仅 ASCII）", len(hits))
	}
}

// TestConcurrentCAS：并发 Put 同一 item，CAS 保证 revision 严格递增、
// 无丢失更新（-race）。
func TestConcurrentCAS(t *testing.T) {
	s := NewMemoryStore()
	ctx := t.Context()
	ns := []string{"tenant:a"}
	if _, err := s.Put(ctx, itemOf("hot", ns, "v0"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	done := make(chan int, workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer func() { done <- w }()
			for attempt := 0; attempt < 50; attempt++ {
				cur, err := s.Get(ctx, ns, "hot")
				if err != nil {
					continue
				}
				it := itemOf("hot", ns, strings.Repeat("v", 1)+string(rune('a'+w)))
				if _, err := s.Put(ctx, it, PutMemoryOptions{ExpectedRevision: cur.Revision}); err == nil {
					return // 本 worker 赢得一次 CAS 更新即退出
				}
			}
		}(w)
	}
	for w := 0; w < workers; w++ {
		<-done
	}
	got, _ := s.Get(ctx, ns, "hot")
	if got.Revision < 2 || got.Revision > workers+1 {
		t.Fatalf("revision = %d, want within [2, %d]", got.Revision, workers+1)
	}
}
