package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// noImportStore 是未实现 ImportStore 的最小 MemoryStore stub：验证
// ImportItems 对不支持保真导入的后端整单拒绝（ErrImportUnsupported），
// 不做静默降级。
type noImportStore struct{}

var _ MemoryStore = noImportStore{}

func (noImportStore) Put(ctx context.Context, item MemoryItem, opts PutMemoryOptions) (MemoryItem, error) {
	return MemoryItem{}, errors.New("not implemented")
}

func (noImportStore) Get(ctx context.Context, ns []string, id string) (MemoryItem, error) {
	return MemoryItem{}, ErrItemNotFound
}

func (noImportStore) Search(ctx context.Context, q MemoryQuery) ([]MemoryHit, error) {
	return nil, nil
}

func (noImportStore) Supersede(ctx context.Context, oldID string, next MemoryItem) (MemoryItem, error) {
	return MemoryItem{}, errors.New("not implemented")
}

func (noImportStore) Revoke(ctx context.Context, id string, reason string) error {
	return errors.New("not implemented")
}

// TestExportImportRoundTripMem：内存版全状态往返——Active/Superseded/
// Revoked 三种状态、双时态字段与 Revision 原样保留（不重置、不重编号），
// taint 原样（导入不洗白信任级）。
func TestExportImportRoundTripMem(t *testing.T) {
	ctx := context.Background()
	src := NewMemoryStore()
	ns := []string{"tenant:a", "project:p1"}

	// d1：Active / trusted / session 来源。
	if _, err := src.Put(ctx, itemOf("d1", ns, "use yaml config"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	// d2：untrusted-external / Structured / ValidUntil，随后 Revoke（终态
	// 也是记忆库的一部分，导出必须带上）。
	d2 := itemOf("d2", ns, "scraped from blog comment")
	d2.Taint = TaintUntrustedExt
	d2.Confidence = 0.4
	d2.SourceRefs = []SourceRef{{Type: SourceExternal, Ref: "blog-comment-42"}}
	d2.Structured = []byte(`{"url":"https://example.com/post"}`)
	until := time.Now().Add(24 * time.Hour).UTC()
	d2.ValidUntil = &until
	if _, err := src.Put(ctx, d2, PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := src.Revoke(ctx, "d2", "spam"); err != nil {
		t.Fatal(err)
	}
	// d3 → d3next：Supersede 链（d3 Status=Superseded、Revision 前进）。
	if _, err := src.Put(ctx, itemOf("d3", ns, "v1 policy"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	next := itemOf("d3next", ns, "v2 policy")
	if _, err := src.Supersede(ctx, "d3", next); err != nil {
		t.Fatal(err)
	}

	items, err := ExportItems(ctx, src, MemoryQuery{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}
	// 强制 IncludeInactive：Superseded 的 d3 与 Revoked 的 d2 都必须导出。
	if len(items) != 4 {
		t.Fatalf("exported %d items, want 4（含非 Active 状态）", len(items))
	}
	byID := map[string]MemoryItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	if byID["d2"].Status != StatusRevoked || byID["d3"].Status != StatusSuperseded {
		t.Fatalf("inactive states lost: d2=%s d3=%s", byID["d2"].Status, byID["d3"].Status)
	}

	dst := NewMemoryStore()
	report, err := ImportItems(ctx, dst, items)
	if err != nil {
		t.Fatal(err)
	}
	if report.Imported != 4 || report.Skipped != 0 || len(report.Conflicts) != 0 {
		t.Fatalf("report = %+v", report)
	}

	// 逐条保真断言：双时态时间域与 Revision 必须与导出值逐字段相等
	// （PutImport 的核心契约——重置时间域的「迁移成功」比失败更糟）。
	for _, want := range items {
		got, err := dst.Get(ctx, want.Namespace, want.ID)
		if err != nil {
			t.Fatalf("get %s: %v", want.ID, err)
		}
		if !itemEqual(got, want) {
			t.Fatalf("item %s not faithful:\n got %+v\nwant %+v", want.ID, got, want)
		}
		if !got.KnownAt.Equal(want.KnownAt) || !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
			t.Fatalf("item %s time domain reset: got %v/%v/%v want %v/%v/%v",
				want.ID, got.KnownAt, got.CreatedAt, got.UpdatedAt, want.KnownAt, want.CreatedAt, want.UpdatedAt)
		}
		if got.Revision != want.Revision {
			t.Fatalf("item %s revision: got %d want %d", want.ID, got.Revision, want.Revision)
		}
	}
	// 关键状态点显式复核：终态不重置为 Active、taint 不洗白、替代链
	// Revision 不重来。
	got3, err := dst.Get(ctx, ns, "d3")
	if err != nil {
		t.Fatal(err)
	}
	if got3.Status != StatusSuperseded || got3.Revision != 2 {
		t.Fatalf("superseded item: status=%s revision=%d, want Superseded/2", got3.Status, got3.Revision)
	}
	got2, err := dst.Get(ctx, ns, "d2")
	if err != nil {
		t.Fatal(err)
	}
	if got2.Taint != TaintUntrustedExt || got2.Status != StatusRevoked || got2.ValidUntil == nil || !got2.ValidUntil.Equal(until) {
		t.Fatalf("revoked item: %+v", got2)
	}
}

// TestImportIdempotentMem：同一批导出导入两次，第二轮全部 Skipped 且无
// 冲突——内容寻址的幂等重跑口径。
func TestImportIdempotentMem(t *testing.T) {
	ctx := context.Background()
	src := NewMemoryStore()
	ns := []string{"tenant:a"}
	if _, err := src.Put(ctx, itemOf("d1", ns, "alpha"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Put(ctx, itemOf("d2", ns, "beta"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	items, err := ExportItems(ctx, src, MemoryQuery{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}
	dst := NewMemoryStore()
	first, err := ImportItems(ctx, dst, items)
	if err != nil {
		t.Fatal(err)
	}
	if first.Imported != 2 || first.Skipped != 0 {
		t.Fatalf("first report = %+v", first)
	}
	second, err := ImportItems(ctx, dst, items)
	if err != nil {
		t.Fatal(err)
	}
	if second.Imported != 0 || second.Skipped != 2 || len(second.Conflicts) != 0 {
		t.Fatalf("second report = %+v, want all skipped", second)
	}
}

// TestImportConflictMem：目标已存在同 ID 不同内容 → 记入 Conflicts 且
// 不覆盖（先到先得），其余 item 照常导入。
func TestImportConflictMem(t *testing.T) {
	ctx := context.Background()
	src := NewMemoryStore()
	ns := []string{"tenant:a"}
	if _, err := src.Put(ctx, itemOf("d1", ns, "from src"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Put(ctx, itemOf("d2", ns, "fine"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	items, err := ExportItems(ctx, src, MemoryQuery{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}
	dst := NewMemoryStore()
	if _, err := dst.Put(ctx, itemOf("d1", ns, "already here"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	report, err := ImportItems(ctx, dst, items)
	if err != nil {
		t.Fatal(err)
	}
	if report.Imported != 1 || len(report.Conflicts) != 1 || report.Conflicts[0].ID != "d1" {
		t.Fatalf("report = %+v", report)
	}
	// 先到先得：目标 d1 内容保持不变。
	got, err := dst.Get(ctx, ns, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "already here" {
		t.Fatalf("conflicting item must not be overwritten, content = %q", got.Content)
	}
	if _, err := dst.Get(ctx, ns, "d2"); err != nil {
		t.Fatalf("non-conflicting item must import: %v", err)
	}
}

// TestImportValidationMem：非法 item（缺来源）记入 Conflicts 继续整单
// （校验链 fail closed，但单条失败不中断批量），合法 item 照常入库。
func TestImportValidationMem(t *testing.T) {
	ctx := context.Background()
	dst := NewMemoryStore()
	ns := []string{"tenant:a"}
	bad := itemOf("bad", ns, "x")
	bad.SourceRefs = nil
	good := itemOf("good", ns, "y")
	report, err := ImportItems(ctx, dst, []MemoryItem{bad, good})
	if err != nil {
		t.Fatal(err)
	}
	if report.Imported != 1 || len(report.Conflicts) != 1 || report.Conflicts[0].ID != "bad" {
		t.Fatalf("report = %+v", report)
	}
	if report.Conflicts[0].Reason == "" {
		t.Fatal("conflict must carry the validation reason")
	}
}

// TestImportUnsupported：store 未实现 ImportStore → 整单拒绝，不做静默
// 降级（时间域被重置的「迁移成功」比失败更糟）。
func TestImportUnsupported(t *testing.T) {
	ctx := context.Background()
	it := itemOf("d1", []string{"tenant:a"}, "x")
	_, err := ImportItems(ctx, noImportStore{}, []MemoryItem{it})
	if !errors.Is(err, ErrImportUnsupported) {
		t.Fatalf("err = %v, want ErrImportUnsupported", err)
	}
}
