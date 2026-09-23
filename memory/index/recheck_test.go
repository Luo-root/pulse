package index

import (
	"context"
	"errors"
	"testing"

	"github.com/Luo-root/pulse/memory/store"
)

// errorGetStore 包装内存 store：Get 按注入的错误应答（检索复核路径）。
type errorGetStore struct {
	store.MemoryStore
	err error
}

func (e errorGetStore) Get(ctx context.Context, ns []string, id string) (store.MemoryItem, error) {
	if e.err != nil {
		return store.MemoryItem{}, e.err
	}
	return e.MemoryStore.Get(ctx, ns, id)
}

func recheckItem() store.MemoryItem {
	return store.MemoryItem{
		ID:         "m1",
		Namespace:  []string{"tenant:a"},
		Kind:       store.KindLesson,
		Content:    "deploy via argocd",
		Status:     store.StatusActive,
		Confidence: 1.0,
		Taint:      store.TaintTrusted,
		SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: "s1", Seq: 1}},
	}
}

// TestSearchRecheckErrorSemantics：命中复核阶段的错误语义——ctx 取消必须
// 上报（不得伪装成「无命中」）、stale 条目（ErrItemNotFound）跳过、其余
// store 故障不得静默降召回。
func TestSearchRecheckErrorSemantics(t *testing.T) {
	ctx := t.Context()
	inner := store.NewMemoryStore()
	saved, err := inner.Put(ctx, recheckItem(), store.PutMemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{dims: 2, vecs: map[string][]float32{"deploy": {1, 0}}}
	ns := []string{"tenant:a"}

	// ① 已取消的 ctx：Search 必须把取消传上去。
	idx := newTestIndex(t, errorGetStore{MemoryStore: inner}, provider)
	if err := idx.Upsert(ctx, saved); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := idx.Search(canceled, ns, "deploy", 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled search err = %v, want context.Canceled", err)
	}

	// ② stale 条目：Get 报 ErrItemNotFound → 跳过，不算故障。
	idx = newTestIndex(t, errorGetStore{MemoryStore: inner, err: store.ErrItemNotFound}, provider)
	if err := idx.Upsert(ctx, saved); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.Search(ctx, ns, "deploy", 5)
	if err != nil || len(hits) != 0 {
		t.Fatalf("stale entry: hits=%v err=%v, want empty hits + nil err", hits, err)
	}

	// ③ 其余 store 故障：上报，不静默降召回。
	boom := errors.New("store offline")
	idx = newTestIndex(t, errorGetStore{MemoryStore: inner, err: boom}, provider)
	if err := idx.Upsert(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Search(ctx, ns, "deploy", 5); !errors.Is(err, boom) {
		t.Fatalf("store failure err = %v, want the underlying error", err)
	}
}
