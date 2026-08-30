package selfedit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/memory/store"
	"github.com/Luo-root/pulse/toolset"
)

// countingStore 包装内存 store：记录写调用次数（Preview 不落盘断言）。
type countingStore struct {
	inner  store.MemoryStore
	mu     sync.Mutex
	writes int
}

func (c *countingStore) Put(ctx context.Context, it store.MemoryItem, o store.PutMemoryOptions) (store.MemoryItem, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return c.inner.Put(ctx, it, o)
}
func (c *countingStore) Get(ctx context.Context, ns []string, id string) (store.MemoryItem, error) {
	return c.inner.Get(ctx, ns, id)
}
func (c *countingStore) Search(ctx context.Context, q store.MemoryQuery) ([]store.MemoryHit, error) {
	return c.inner.Search(ctx, q)
}
func (c *countingStore) Supersede(ctx context.Context, oldID string, next store.MemoryItem) (store.MemoryItem, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return c.inner.Supersede(ctx, oldID, next)
}
func (c *countingStore) Revoke(ctx context.Context, id, reason string) error {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return c.inner.Revoke(ctx, id, reason)
}
func (c *countingStore) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

// origin 是测试用固定回链（session 来源，store 校验 SessionID+Seq>0）。
func origin() store.SourceRef {
	return store.SourceRef{Type: store.SourceSession, SessionID: "s9", Seq: 12}
}

// newEnv 造一个绑定 ns 的工具组（测试装配错误直接 Fatal）。
func newEnv(t *testing.T, cs store.MemoryStore, ns []string) *env {
	t.Helper()
	opt, err := Options{Store: cs, Namespace: ns, OriginFn: origin}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	return &env{opt: opt}
}

func jsonArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// firstID 取 ns 下唯一 active item 的 ID。
func firstID(t *testing.T, ctx context.Context, s store.MemoryStore, ns []string) string {
	t.Helper()
	hits, err := s.Search(ctx, store.MemoryQuery{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	return hits[0].Item.ID
}

// TestPutAnchorsItem：写入 item 的 namespace/回链/信任级/置信度/状态全部
// 来自 env 装配项，模型只贡献 kind/content/structured；结果串带 ID。
func TestPutAnchorsItem(t *testing.T) {
	cs := &countingStore{inner: store.NewMemoryStore()}
	e := newEnv(t, cs, []string{"tenant:a", "user:u1"})
	ctx := t.Context()

	out, err := e.put(ctx, jsonArgs(t, map[string]any{
		"kind":       "lesson",
		"content":    "always dry-run first",
		"structured": `{"risk":"low"}`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	ns := []string{"tenant:a", "user:u1"}
	hits, err := cs.inner.Search(ctx, store.MemoryQuery{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	it := hits[0].Item
	if it.Kind != store.KindLesson || it.Content != "always dry-run first" {
		t.Fatalf("item = %+v", it)
	}
	if len(it.Namespace) != 2 || it.Namespace[0] != "tenant:a" || it.Namespace[1] != "user:u1" {
		t.Fatalf("namespace = %v, want env-bound", it.Namespace)
	}
	if len(it.SourceRefs) != 1 || it.SourceRefs[0] != origin() {
		t.Fatalf("sourceRefs = %+v, want OriginFn value（回链恒等）", it.SourceRefs)
	}
	if it.Taint != store.TaintTrusted {
		t.Fatalf("taint = %s, want default trusted", it.Taint)
	}
	if it.Status != store.StatusActive || it.Confidence != 1.0 {
		t.Fatalf("status/confidence = %s/%v, want active/1.0", it.Status, it.Confidence)
	}
	if string(it.Structured) != `{"risk":"low"}` {
		t.Fatalf("structured = %s", it.Structured)
	}
	if !strings.Contains(out, it.ID) {
		t.Fatalf("result %q must carry the new id（模型后续 supersede 用）", out)
	}
}

// TestSupersedeChain：替换后旧条目留痕 superseded、新条目 active；kind 缺省
// 沿用、显式可覆盖；next 保持原 item namespace（env.ns 只是可见性前缀）。
func TestSupersedeChain(t *testing.T) {
	cs := &countingStore{inner: store.NewMemoryStore()}
	e := newEnv(t, cs, []string{"tenant:a"})
	ctx := t.Context()

	if _, err := e.put(ctx, jsonArgs(t, map[string]any{"kind": "decision", "content": "use yaml for CI"})); err != nil {
		t.Fatal(err)
	}
	oldID := firstID(t, ctx, cs.inner, []string{"tenant:a"})

	out, err := e.supersede(ctx, jsonArgs(t, map[string]any{"id": oldID, "content": "use yaml for CI (v2)"}))
	if err != nil {
		t.Fatal(err)
	}
	hits, err := cs.inner.Search(ctx, store.MemoryQuery{Namespace: []string{"tenant:a"}, IncludeInactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2（旧留痕 + 新生效）", len(hits))
	}
	var oldItem, newItem store.MemoryItem
	for _, h := range hits {
		switch h.Item.ID {
		case oldID:
			oldItem = h.Item
		default:
			newItem = h.Item
		}
	}
	if oldItem.ID != oldID || oldItem.Status != store.StatusSuperseded {
		t.Fatalf("old = %+v, want superseded", oldItem)
	}
	if newItem.Status != store.StatusActive || newItem.Content != "use yaml for CI (v2)" {
		t.Fatalf("next = %+v, want active with new content", newItem)
	}
	if newItem.Kind != store.KindDecision {
		t.Fatalf("next kind = %s, want inherited decision", newItem.Kind)
	}
	if len(newItem.Namespace) != 1 || newItem.Namespace[0] != "tenant:a" {
		t.Fatalf("next namespace = %v, want original item namespace", newItem.Namespace)
	}
	if len(newItem.SourceRefs) != 1 || newItem.SourceRefs[0] != origin() {
		t.Fatalf("next sourceRefs = %+v, want 本次编辑回链", newItem.SourceRefs)
	}
	if !strings.Contains(out, oldID) || !strings.Contains(out, newItem.ID) {
		t.Fatalf("result %q must carry both ids", out)
	}

	// 显式 kind 覆盖。
	out2, err := e.supersede(ctx, jsonArgs(t, map[string]any{"id": newItem.ID, "content": "v3", "kind": "lesson"}))
	if err != nil {
		t.Fatal(err)
	}
	final, err := cs.inner.Get(ctx, []string{"tenant:a"}, mustNewID(t, out2, oldID))
	if err != nil {
		t.Fatal(err)
	}
	if final.Kind != store.KindLesson || final.Content != "v3" {
		t.Fatalf("final = %+v, want lesson/v3", final)
	}
}

// mustNewID 从 supersede 结果串里解析新 ID（"superseded <old> -> <new> ..."）。
func mustNewID(t *testing.T, out, oldID string) string {
	t.Helper()
	const sep = " -> "
	i := strings.Index(out, sep)
	if i < 0 {
		t.Fatalf("result %q missing separator", out)
	}
	id := out[i+len(sep):]
	if j := strings.IndexByte(id, ' '); j >= 0 {
		id = id[:j]
	}
	if id == "" || id == oldID {
		t.Fatalf("result %q: bad new id", out)
	}
	return id
}

// TestScopeIsolation：绑定 ns B 的工具对 ns A 的 item 一律 not found——
// supersede/revoke 都先 Get（跨 namespace 不互见）。
func TestScopeIsolation(t *testing.T) {
	cs := &countingStore{inner: store.NewMemoryStore()}
	eA := newEnv(t, cs, []string{"tenant:a"})
	eB := newEnv(t, cs, []string{"tenant:b"})
	ctx := t.Context()

	if _, err := eA.put(ctx, jsonArgs(t, map[string]any{"kind": "profile", "content": "A-only fact"})); err != nil {
		t.Fatal(err)
	}
	id := firstID(t, ctx, cs.inner, []string{"tenant:a"})

	if _, err := eB.supersede(ctx, jsonArgs(t, map[string]any{"id": id, "content": "hijack"})); !errors.Is(err, store.ErrItemNotFound) {
		t.Fatalf("supersede err = %v, want ErrItemNotFound", err)
	}
	if _, err := eB.revoke(ctx, jsonArgs(t, map[string]any{"id": id, "reason": "hijack"})); !errors.Is(err, store.ErrItemNotFound) {
		t.Fatalf("revoke err = %v, want ErrItemNotFound", err)
	}
	// B 写入的 item 落在 B 的 namespace，A 同样不可见。
	if _, err := eB.put(ctx, jsonArgs(t, map[string]any{"kind": "profile", "content": "B-only fact"})); err != nil {
		t.Fatal(err)
	}
	idB := firstID(t, ctx, cs.inner, []string{"tenant:b"})
	if _, err := eA.supersede(ctx, jsonArgs(t, map[string]any{"id": idB, "content": "hijack"})); !errors.Is(err, store.ErrItemNotFound) {
		t.Fatalf("A supersede B item err = %v, want ErrItemNotFound", err)
	}
}

// TestRevokeLifecycle：revoke 幂等；对 superseded 目标拒绝；supersede 对
// revoked 目标拒绝——store 状态机哨兵原样透传。
func TestRevokeLifecycle(t *testing.T) {
	cs := &countingStore{inner: store.NewMemoryStore()}
	e := newEnv(t, cs, []string{"tenant:a"})
	ctx := t.Context()

	if _, err := e.put(ctx, jsonArgs(t, map[string]any{"kind": "episode", "content": "one-off event"})); err != nil {
		t.Fatal(err)
	}
	id := firstID(t, ctx, cs.inner, []string{"tenant:a"})

	if _, err := e.revoke(ctx, jsonArgs(t, map[string]any{"id": id, "reason": "wrong memory"})); err != nil {
		t.Fatal(err)
	}
	// 幂等：再 revoke 成功。
	if _, err := e.revoke(ctx, jsonArgs(t, map[string]any{"id": id, "reason": "again"})); err != nil {
		t.Fatalf("revoke must be idempotent: %v", err)
	}
	// revoked 是终态：supersede 拒绝。
	if _, err := e.supersede(ctx, jsonArgs(t, map[string]any{"id": id, "content": "zombie"})); !errors.Is(err, store.ErrSupersedeRevoked) {
		t.Fatalf("supersede revoked err = %v, want ErrSupersedeRevoked", err)
	}

	// superseded 目标不可 revoke（应操作生效版本）。
	if _, err := e.put(ctx, jsonArgs(t, map[string]any{"kind": "episode", "content": "will be replaced"})); err != nil {
		t.Fatal(err)
	}
	id2 := firstID(t, ctx, cs.inner, []string{"tenant:a"})
	newID := mustNewID(t, mustSupersede(t, e, ctx, id2), id2) // 新生效版本
	if _, err := e.revoke(ctx, jsonArgs(t, map[string]any{"id": id2, "reason": "wrong target"})); !errors.Is(err, store.ErrRevokeSuperseded) {
		t.Fatalf("revoke superseded err = %v, want ErrRevokeSuperseded", err)
	}
	// 撤销生效版本没问题。
	if _, err := e.revoke(ctx, jsonArgs(t, map[string]any{"id": newID, "reason": "all gone"})); err != nil {
		t.Fatal(err)
	}
}

func mustSupersede(t *testing.T, e *env, ctx context.Context, id string) string {
	t.Helper()
	out, err := e.supersede(ctx, jsonArgs(t, map[string]any{"id": id, "content": "v2"}))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestToolValidation：模型参数最小面的必填与形状校验（空 kind 由 store 拒）。
func TestToolValidation(t *testing.T) {
	cs := &countingStore{inner: store.NewMemoryStore()}
	e := newEnv(t, cs, []string{"tenant:a"})
	ctx := t.Context()

	cases := []struct {
		name string
		call func() error
	}{
		{"put empty content", func() error {
			_, err := e.put(ctx, jsonArgs(t, map[string]any{"kind": "lesson", "content": "  "}))
			return err
		}},
		{"put bad structured", func() error {
			_, err := e.put(ctx, jsonArgs(t, map[string]any{"kind": "lesson", "content": "x", "structured": "{not json"}))
			return err
		}},
		{"put empty kind", func() error {
			_, err := e.put(ctx, jsonArgs(t, map[string]any{"kind": "", "content": "x"}))
			return err
		}},
		{"supersede empty id", func() error {
			_, err := e.supersede(ctx, jsonArgs(t, map[string]any{"id": " ", "content": "x"}))
			return err
		}},
		{"revoke empty reason", func() error {
			_, err := e.revoke(ctx, jsonArgs(t, map[string]any{"id": "whatever", "reason": " "}))
			return err
		}},
	}
	for _, tc := range cases {
		if err := tc.call(); err == nil {
			t.Fatalf("%s: want error", tc.name)
		}
	}
	// Options 必填项（fail closed）。if 初始化里的复合字面量要加括号。
	if _, err := (Options{Namespace: []string{"tenant:a"}, OriginFn: origin}).withDefaults(); err == nil {
		t.Fatal("missing store must fail")
	}
	if _, err := (Options{Store: cs, OriginFn: origin}).withDefaults(); err == nil {
		t.Fatal("missing namespace must fail")
	}
	if _, err := (Options{Store: cs, Namespace: []string{"tenant:a"}}).withDefaults(); err == nil {
		t.Fatal("missing origin fn must fail")
	}
	if _, err := (Options{Store: cs, Namespace: []string{"tenant:a", " "}, OriginFn: origin}).withDefaults(); err == nil {
		t.Fatal("empty namespace element must fail")
	}
}

// TestPreviewsDoNotWrite：三工具 Preview 只读——store 零写入；卡片
// kind=opaque / action=write / subject 带 ns 键；长摘要显式截断。
func TestPreviewsDoNotWrite(t *testing.T) {
	cs := &countingStore{inner: store.NewMemoryStore()}
	e := newEnv(t, cs, []string{"tenant:a", "user:u1"})
	ctx := t.Context()

	p1, err := e.previewPut(ctx, jsonArgs(t, map[string]any{"kind": "lesson", "content": strings.Repeat("记忆内容", 200)}))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := e.previewSupersede(ctx, jsonArgs(t, map[string]any{"id": "m1", "content": "new text"}))
	if err != nil {
		t.Fatal(err)
	}
	p3, err := e.previewRevoke(ctx, jsonArgs(t, map[string]any{"id": "m1", "reason": "stale"}))
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []toolset.Preview{p1, p2, p3} {
		if p.Kind != toolset.KindOpaque || p.Action != toolset.ActionWrite {
			t.Fatalf("preview[%d] kind/action = %s/%s, want opaque/write", i, p.Kind, p.Action)
		}
		if !strings.HasPrefix(p.Subject, "memory: tenant:a/user:u1/") {
			t.Fatalf("preview[%d] subject = %q, want ns-key prefixed", i, p.Subject)
		}
	}
	if !strings.Contains(p1.Opaque.Summary, "truncated") {
		t.Fatalf("long summary must be explicitly truncated: %q", p1.Opaque.Summary)
	}
	if n := cs.writeCount(); n != 0 {
		t.Fatalf("store writes after previews = %d, want 0（Preview 只读）", n)
	}
}

// TestRegisterGroup：opt-in 登记三工具（source/risk/preview 齐）、dispose
// 整组撤销、必填缺失拒绝。
func TestRegisterGroup(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, toolset.Plugin()); err != nil {
		t.Fatal(err)
	}
	reg, ok := kernel.Get(host, toolset.ServiceKey)
	if !ok {
		t.Fatal("no registry")
	}
	dispose, err := Register(host, reg, Options{Store: store.NewMemoryStore(), Namespace: []string{"tenant:a"}, OriginFn: origin})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"memory_put", "memory_supersede", "memory_revoke"} {
		src, risk, ok := reg.LookupMeta(name)
		if !ok || src != defaultSource || risk != toolset.RiskReadWrite {
			t.Fatalf("%s meta = (%q,%v,%v), want (%q,%v,true)", name, src, risk, ok, defaultSource, toolset.RiskReadWrite)
		}
		if _, ok := reg.LookupPreview(name); !ok {
			t.Fatalf("%s: preview missing", name)
		}
	}
	dispose()
	if _, _, ok := reg.LookupMeta("memory_put"); ok {
		t.Fatal("dispose must remove the whole group")
	}
	// 必填缺失：Register 阶段拒绝。
	if _, err := Register(host, reg, Options{Store: store.NewMemoryStore(), Namespace: []string{"tenant:a"}}); err == nil {
		t.Fatal("missing OriginFn must fail at Register")
	}
}
