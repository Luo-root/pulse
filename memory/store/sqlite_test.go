//go:build !plan9 && !js

package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(t.Context(), "file:"+filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sqliteItem(id, content string) MemoryItem {
	return MemoryItem{
		ID:         id,
		Namespace:  []string{"tenant:a", "project:p1"},
		Kind:       KindDecision,
		Content:    content,
		Status:     StatusActive,
		Confidence: 1.0,
		Taint:      TaintTrusted,
		SourceRefs: []SourceRef{{Type: SourceSession, SessionID: "s1", Seq: 3}},
	}
}

// TestSQLiteRoundTrip：全字段 Put/Get roundtrip（含 Structured/ValidUntil/
// Taint），与内存实现同契约。
func TestSQLiteRoundTrip(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	it := sqliteItem("d1", "prefer toml")
	st := jsonRaw(t, `{"lang":"go"}`)
	it.Structured = st
	until := time.Now().Add(24 * time.Hour).UTC()
	it.ValidUntil = &until
	got, err := s.Put(ctx, it, PutMemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1 || got.CreatedAt.IsZero() || got.KnownAt.IsZero() {
		t.Fatalf("stored = %+v", got)
	}
	reread, err := s.Get(ctx, got.Namespace, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if reread.Content != "prefer toml" || string(reread.Structured) != `{"lang":"go"}` ||
		reread.Taint != TaintTrusted || reread.Kind != KindDecision {
		t.Fatalf("roundtrip = %+v", reread)
	}
	if reread.ValidUntil == nil || !reread.ValidUntil.Equal(until) {
		t.Fatalf("validUntil = %v, want %v", reread.ValidUntil, until)
	}
	if len(reread.SourceRefs) != 1 || reread.SourceRefs[0].SessionID != "s1" || reread.SourceRefs[0].Seq != 3 {
		t.Fatalf("source refs = %+v", reread.SourceRefs)
	}
}

func jsonRaw(t *testing.T, s string) []byte {
	t.Helper()
	return []byte(s)
}

// TestSQLiteNamespaceIsolation：兄弟 namespace 互斥、父前缀可见——与内存
// 实现同验收。
func TestSQLiteNamespaceIsolation(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	tenantA := []string{"tenant:a", "project:p1"}
	tenantB := []string{"tenant:b", "project:p1"}
	if _, err := s.Put(ctx, sqliteItem("d1", "yaml"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, MemoryQuery{Namespace: tenantB})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("cross-tenant hits = %d, want 0", len(hits))
	}
	if _, err := s.Get(ctx, tenantB, "d1"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("err = %v, want ErrItemNotFound", err)
	}
	hits, _ = s.Search(ctx, MemoryQuery{Namespace: tenantA})
	if len(hits) != 1 {
		t.Fatalf("parent hits = %d, want 1", len(hits))
	}
	// 边界安全：tenant:ab 的 item 不被 tenant:a 前缀误命中。
	ab := sqliteItem("d2", "ab only")
	ab.Namespace = []string{"tenant:ab"}
	if _, err := s.Put(ctx, ab, PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	hits, _ = s.Search(ctx, MemoryQuery{Namespace: []string{"tenant:a"}})
	if len(hits) != 1 || hits[0].Item.ID != "d1" {
		t.Fatalf("prefix must respect element boundary: %+v", hits)
	}
}

// TestSQLiteCAS：revision 冲突数据不变、状态迁移拒绝、ghost ID——与内存
// 实现同契约。
func TestSQLiteCAS(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	first, err := s.Put(ctx, sqliteItem("d1", "v1"), PutMemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, sqliteItem("d1", "v2"), PutMemoryOptions{}); !errors.Is(err, ErrItemExists) {
		t.Fatalf("err = %v, want ErrItemExists", err)
	}
	stale := sqliteItem("d1", "v3")
	if _, err := s.Put(ctx, stale, PutMemoryOptions{ExpectedRevision: 99}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("err = %v, want ErrRevisionConflict", err)
	}
	got, _ := s.Get(ctx, first.Namespace, "d1")
	if got.Content != "v1" || got.Revision != 1 {
		t.Fatalf("conflicted put mutated: %+v", got)
	}
	// 状态迁移拒绝（active→pending / active→superseded）。
	demotion := sqliteItem("d1", "v2")
	demotion.Status = StatusPending
	if _, err := s.Put(ctx, demotion, PutMemoryOptions{ExpectedRevision: 1}); !errors.Is(err, ErrStatusTransition) {
		t.Fatalf("err = %v, want ErrStatusTransition", err)
	}
	supersededPut := sqliteItem("d1", "v2")
	supersededPut.Status = StatusSuperseded
	if _, err := s.Put(ctx, supersededPut, PutMemoryOptions{ExpectedRevision: 1}); !errors.Is(err, ErrStatusTransition) {
		t.Fatalf("err = %v, want ErrStatusTransition", err)
	}
	// 合法 CAS 更新。
	if _, err := s.Put(ctx, sqliteItem("d1", "v2"), PutMemoryOptions{ExpectedRevision: 1}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, first.Namespace, "d1")
	if got.Revision != 2 || got.Content != "v2" {
		t.Fatalf("updated = %+v", got)
	}
	// ghost ID + ExpectedRevision>0。
	if _, err := s.Put(ctx, sqliteItem("ghost", "x"), PutMemoryOptions{ExpectedRevision: 1}); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("err = %v, want ErrItemNotFound", err)
	}
}

// TestSQLiteSupersedeRevoke：事务替代链、终态、幂等、审计落表。
func TestSQLiteSupersedeRevoke(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	if _, err := s.Put(ctx, sqliteItem("old", "prefer yaml"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Supersede(ctx, "old", sqliteItem("old", "self")); !errors.Is(err, ErrSupersedeSelf) {
		t.Fatalf("err = %v, want ErrSupersedeSelf", err)
	}
	next, err := s.Supersede(ctx, "old", sqliteItem("new", "prefer toml"))
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != 1 {
		t.Fatalf("next revision = %d", next.Revision)
	}
	hits, _ := s.Search(ctx, MemoryQuery{Namespace: next.Namespace})
	if len(hits) != 1 || hits[0].Item.ID != "new" {
		t.Fatalf("active hits = %+v", hits)
	}
	hits, _ = s.Search(ctx, MemoryQuery{Namespace: next.Namespace, IncludeInactive: true})
	if len(hits) != 2 {
		t.Fatalf("inactive-inclusive = %d, want 2（禁止物理删除）", len(hits))
	}
	if err := s.Revoke(ctx, "old", "cleanup"); !errors.Is(err, ErrRevokeSuperseded) {
		t.Fatalf("err = %v, want ErrRevokeSuperseded", err)
	}
	if err := s.Revoke(ctx, "new", "policy changed"); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, "new", "again"); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
	if _, err := s.Supersede(ctx, "new", sqliteItem("newer", "x")); !errors.Is(err, ErrSupersedeRevoked) {
		t.Fatalf("err = %v, want ErrSupersedeRevoked", err)
	}
	// 审计落表：supersede + revoke 记录齐、reason 可查。
	audit, err := s.AuditLog()
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]AuditEntry{}
	for _, e := range audit {
		actions[e.Action+":"+e.ItemID] = e
	}
	if e := actions["supersede:old"]; e.NextID != "new" {
		t.Fatalf("supersede audit = %+v", e)
	}
	if e := actions["revoke:new"]; e.Reason != "policy changed" {
		t.Fatalf("revoke audit = %+v（reason 必须落在审计面）", e)
	}
}

// TestSQLiteSearchAndFTS：子串 LIKE 语义与 C1 一致；SearchFTS token 前缀
// 检索且 namespace 过滤生效。
func TestSQLiteSearchAndFTS(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	items := []MemoryItem{
		sqliteItem("a", "Always run go vet before push"),
		func() MemoryItem { it := sqliteItem("b", "User prefers TOML config"); it.Kind = KindProfile; return it }(),
		sqliteItem("c", "the toml parser ignores comments"),
	}
	for _, it := range items {
		if _, err := s.Put(ctx, it, PutMemoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// 子串语义（与内存实现一致，大小写不敏感）。
	hits, err := s.Search(ctx, MemoryQuery{Namespace: []string{"tenant:a"}, Query: "go vet"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Item.ID != "a" {
		t.Fatalf("substring hits = %+v", hits)
	}
	// FTS token 前缀：toml 命中 b/c 两节点。
	ftsHits, err := s.SearchFTS(ctx, []string{"tenant:a"}, "toml", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ftsHits) != 2 {
		t.Fatalf("fts hits = %d, want 2", len(ftsHits))
	}
	// FTS namespace 过滤。
	ftsHits, err = s.SearchFTS(ctx, []string{"tenant:zz"}, "toml", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ftsHits) != 0 {
		t.Fatalf("fts cross-ns hits = %d, want 0", len(ftsHits))
	}
	// FTS 更新同步：内容改写后旧 token 不再命中。
	if _, err := s.Put(ctx, sqliteItem("c", "the yaml parser ignores comments"), PutMemoryOptions{ExpectedRevision: 1}); err != nil {
		t.Fatal(err)
	}
	ftsHits, _ = s.SearchFTS(ctx, []string{"tenant:a"}, "toml", 0)
	if len(ftsHits) != 1 {
		t.Fatalf("fts after update = %d, want 1（触发器同步）", len(ftsHits))
	}
	// 未命中不伪造。
	hits, _ = s.Search(ctx, MemoryQuery{Namespace: []string{"tenant:a"}, Query: "nonexistent"})
	if hits == nil || len(hits) != 0 {
		t.Fatalf("miss = %v, want empty non-nil", hits)
	}
}

// TestSQLiteSchemaVersion：文件 schema 版本高于实现 → 拒绝加载。
func TestSQLiteSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	s, err := NewSQLiteStore(t.Context(), "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(t.Context(), sqliteItem("d1", "x"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// 手工把版本抬到未来：旧实现必须拒绝。
	future, err := NewSQLiteStore(t.Context(), "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := future.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	future.Close()
	if _, err := NewSQLiteStore(t.Context(), "file:"+dbPath); !errors.Is(err, ErrCorruptSchema) {
		t.Fatalf("err = %v, want ErrCorruptSchema（不猜测迁移）", err)
	}
}

// TestSQLiteEmptyDSN：空 DSN 拒绝。
func TestSQLiteEmptyDSN(t *testing.T) {
	if _, err := NewSQLiteStore(t.Context(), ""); err == nil {
		t.Fatal("empty dsn must be rejected")
	}
}

// TestSQLiteConcurrentCreate：并发 Put(ExpectedRevision=0) 同 ID——恰好
// 一个成功，失败者 ErrItemExists（INSERT PK 冲突映射，不外泄裸错误）。
func TestSQLiteConcurrentCreate(t *testing.T) {
	s := newSQLiteStore(t)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Put(t.Context(), sqliteItem("race", "concurrent"), PutMemoryOptions{})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrItemExists) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one creator must win; got %d", ok)
	}
}

// TestSQLiteASCIIFoldParity：大小写折叠口径统一为仅 ASCII——ASCII 大写
// 命中；非 ASCII 重音（É）不折叠不命中。与内存版同断言（对照在
// store_test.go TestSearchASCIIFoldParity）。
func TestSQLiteASCIIFoldParity(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	it := sqliteItem("d1", "Meet at CAFÉ du Nord")
	if _, err := s.Put(ctx, it, PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	// ASCII 折叠：query 大写 → 命中。
	hits, err := s.Search(ctx, MemoryQuery{Namespace: it.Namespace, Query: "NORD"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("ascii fold hits = %d, want 1", len(hits))
	}
	// 非 ASCII 重音不折叠：query 小写 é 不命中 CAFÉ 的 É。
	hits, _ = s.Search(ctx, MemoryQuery{Namespace: it.Namespace, Query: "café"})
	if len(hits) != 0 {
		t.Fatalf("non-ASCII fold must not happen: %d hits, want 0（口径：折叠仅 ASCII）", len(hits))
	}
}
