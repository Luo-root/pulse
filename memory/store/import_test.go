package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	report, err := ImportItems(ctx, dst, items, ImportOptions{})
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
	first, err := ImportItems(ctx, dst, items, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Imported != 2 || first.Skipped != 0 {
		t.Fatalf("first report = %+v", first)
	}
	second, err := ImportItems(ctx, dst, items, ImportOptions{})
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
	report, err := ImportItems(ctx, dst, items, ImportOptions{})
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
	report, err := ImportItems(ctx, dst, []MemoryItem{bad, good}, ImportOptions{})
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
	_, err := ImportItems(ctx, noImportStore{}, []MemoryItem{it}, ImportOptions{})
	if !errors.Is(err, ErrImportUnsupported) {
		t.Fatalf("err = %v, want ErrImportUnsupported", err)
	}
}

// TestItemEqualSameInstant：同一时刻不同时区表示必须判等（SameInstant
// 语义）——JSON 字节比较会把 +08:00 与 Z 写成不同字节，字段级 + Equal
// 才是幂等口径的锚定判例（修前 json.Marshal 实现此处为红）。
func TestItemEqualSameInstant(t *testing.T) {
	base := itemOf("d1", []string{"tenant:a"}, "same")
	local := time.Date(2026, 9, 9, 12, 0, 0, 123456789, time.FixedZone("CST", 8*3600))
	utc := local.UTC()
	a, b := base, base
	a.KnownAt, a.CreatedAt, a.UpdatedAt = local, local, local
	b.KnownAt, b.CreatedAt, b.UpdatedAt = utc, utc, utc
	if !itemEqual(a, b) {
		t.Fatal("same-instant different-location times must compare equal (SameInstant)")
	}
	// 反向判例：内容不同必须判不等；Structured nil/字面 null 同一归一。
	c := b
	c.Content = "different"
	if itemEqual(b, c) {
		t.Fatal("different content must not compare equal")
	}
	d, e := base, base
	d.Structured = nil
	e.Structured = json.RawMessage("null")
	if !itemEqual(d, e) {
		t.Fatal("Structured nil and literal null must compare equal (marshal-parity)")
	}
}

// TestImportSameInstantSkipped：目标已有同内容 item（Put 分配本地时区
// 表示），导入其时间域 UTC 化的导出副本——同一时刻不得误报 Conflict，
// 应 Skipped。
func TestImportSameInstantSkipped(t *testing.T) {
	ctx := context.Background()
	dst := NewMemoryStore()
	ns := []string{"tenant:a"}
	if _, err := dst.Put(ctx, itemOf("d1", ns, "same"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	stored, err := dst.Get(ctx, ns, "d1")
	if err != nil {
		t.Fatal(err)
	}
	alias := stored // 同一内容的导出副本，时间域转 UTC 表示（JSON 往返形态）
	alias.KnownAt, alias.CreatedAt, alias.UpdatedAt = stored.KnownAt.UTC(), stored.CreatedAt.UTC(), stored.UpdatedAt.UTC()
	report, err := ImportItems(ctx, dst, []MemoryItem{alias}, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Skipped != 1 || report.Imported != 0 || len(report.Conflicts) != 0 {
		t.Fatalf("same-instant alias must be Skipped, report = %+v", report)
	}
}

// TestImportNamespaceRemap：重映射命中即把 item 挪到目标层级——源 namespace
// 不可见、目标 namespace 可见；未命中的 item 原样保留。
func TestImportNamespaceRemap(t *testing.T) {
	ctx := context.Background()
	src := NewMemoryStore()
	srcNS := []string{"tenant:a", "project:p1"}
	dstNS := []string{"tenant:b", "project:p9"}
	if _, err := src.Put(ctx, itemOf("d1", srcNS, "moved"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	inPlace := []string{"tenant:a", "project:other"}
	if _, err := src.Put(ctx, itemOf("d2", inPlace, "kept"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	items, err := ExportItems(ctx, src, MemoryQuery{Namespace: []string{"tenant:a"}})
	if err != nil {
		t.Fatal(err)
	}
	dst := NewMemoryStore()
	report, err := ImportItems(ctx, dst, items, ImportOptions{
		NamespaceRemap: map[string]string{"tenant:a/project:p1": "tenant:b/project:p9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Imported != 2 || len(report.Conflicts) != 0 {
		t.Fatalf("report = %+v", report)
	}
	got, err := dst.Get(ctx, dstNS, "d1")
	if err != nil {
		t.Fatalf("remapped item must live in the target namespace: %v", err)
	}
	if !slices.Equal(got.Namespace, dstNS) {
		t.Fatalf("namespace = %v, want %v", got.Namespace, dstNS)
	}
	// 源 namespace 不得再看到它（跨 namespace 不互见）。
	if _, err := dst.Get(ctx, srcNS, "d1"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("source namespace must not see the remapped item, err = %v", err)
	}
	// 未命中的映射项原样落在源层级。
	if _, err := dst.Get(ctx, inPlace, "d2"); err != nil {
		t.Fatalf("unmapped item must stay in place: %v", err)
	}
}

// TestImportRemapConflictAtTarget：冲突判定落在**重映射后的目标位置**——
// 目标已有同 ID 不同内容则拒绝且不覆盖，源位置也不留下副本。
func TestImportRemapConflictAtTarget(t *testing.T) {
	ctx := context.Background()
	srcNS := []string{"tenant:a"}
	dstNS := []string{"tenant:b"}
	items := []MemoryItem{itemOf("d1", srcNS, "from src")}
	dst := NewMemoryStore()
	if _, err := dst.Put(ctx, itemOf("d1", dstNS, "already here"), PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	report, err := ImportItems(ctx, dst, items, ImportOptions{
		NamespaceRemap: map[string]string{"tenant:a": "tenant:b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Imported != 0 || len(report.Conflicts) != 1 || report.Conflicts[0].ID != "d1" {
		t.Fatalf("report = %+v", report)
	}
	got, err := dst.Get(ctx, dstNS, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "already here" {
		t.Fatalf("target must not be overwritten, content = %q", got.Content)
	}
	if _, err := dst.Get(ctx, srcNS, "d1"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("no copy may land in the source namespace, err = %v", err)
	}
}

// TestImportRemapInvalidTarget：映射值含空元素 → 该条记 Conflict 且 reason
// 如实说明，不静默落到非法 namespace；未命中该映射的 item 照常导入。
func TestImportRemapInvalidTarget(t *testing.T) {
	ctx := context.Background()
	dst := NewMemoryStore()
	items := []MemoryItem{
		itemOf("bad", []string{"tenant:a"}, "x"),
		itemOf("good", []string{"tenant:c"}, "y"),
	}
	report, err := ImportItems(ctx, dst, items, ImportOptions{
		NamespaceRemap: map[string]string{"tenant:a": "tenant:b/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Imported != 1 || len(report.Conflicts) != 1 || report.Conflicts[0].ID != "bad" {
		t.Fatalf("report = %+v", report)
	}
	if !strings.Contains(report.Conflicts[0].Reason, "empty element") {
		t.Fatalf("reason = %q, want it to name the invalid remap target", report.Conflicts[0].Reason)
	}
	if _, err := dst.Get(ctx, []string{"tenant:c"}, "good"); err != nil {
		t.Fatalf("unmapped item must import: %v", err)
	}
}

// raceImportStore：Get 恒 NotFound（空 store），PutImport 恒 ErrItemExists
// ——模拟「探测与写入之间目标被并发写入」的窗口。
type raceImportStore struct {
	MemoryStore
}

func (s *raceImportStore) PutImport(ctx context.Context, item MemoryItem) (MemoryItem, error) {
	return MemoryItem{}, fmt.Errorf("%w: id %s", ErrItemExists, item.ID)
}

// TestImportPutImportRaceReason：探测未命中但写入撞 ErrItemExists → reason
// 如实说「探测后出现」（此时并未核实内容是否相同），不借用探测分支文案。
func TestImportPutImportRaceReason(t *testing.T) {
	ctx := context.Background()
	dst := &raceImportStore{MemoryStore: NewMemoryStore()}
	report, err := ImportItems(ctx, dst, []MemoryItem{itemOf("d1", []string{"tenant:a"}, "x")}, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if reason := report.Conflicts[0].Reason; !strings.Contains(reason, "appeared after probe") {
		t.Fatalf("reason = %q, want the honest post-probe wording", reason)
	}
}
