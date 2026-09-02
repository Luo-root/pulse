package eval

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/candidate"
	"github.com/Luo-root/pulse/memory/store"
)

var (
	kinds  = []store.MemoryKind{store.KindProfile, store.KindDecision, store.KindEnvironment, store.KindEpisode, store.KindLesson}
	taints = []store.TaintLevel{store.TaintTrusted, store.TaintUserSupplied, store.TaintUntrustedExt}
)

// randItem 生成一个合法 Active item（随机 ns 树 / kind / taint / 内容）。
func randItem(r *rng) store.MemoryItem {
	ns := []string{"app", r.randStr(6)}
	for i := 0; i < r.IntN(2); i++ {
		ns = append(ns, r.randStr(5))
	}
	return store.MemoryItem{
		ID:         r.randStr(12),
		Namespace:  ns,
		Kind:       pick(r, kinds),
		Content:    r.text(50),
		Status:     store.StatusActive,
		Confidence: 1.0,
		SourceRefs: []store.SourceRef{{Type: store.SourceManual, Ref: r.randStr(8)}},
		Taint:      pick(r, taints),
	}
}

// randomActive 从 store 随机取一个 Active item（无则返回 nil）。
func randomActive(t *testing.T, r *rng, ctx context.Context, st store.MemoryStore) *store.MemoryItem {
	t.Helper()
	hits, err := st.Search(ctx, store.MemoryQuery{})
	if err != nil {
		t.Fatal(r.failf("search: %v", err))
	}
	var actives []store.MemoryItem
	for _, h := range hits {
		if h.Item.Status == store.StatusActive {
			actives = append(actives, h.Item)
		}
	}
	if len(actives) == 0 {
		return nil
	}
	it := pick(r, actives)
	return &it
}

// checkGlobalInvariants 每步后必查的跨状态不变式：
//
//	G1 禁物理删除：任意曾写入的 ID 仍可 Get（任意状态）；
//	G2 Search 默认（仅 Active）不返回任何非 Active item。
func checkGlobalInvariants(t *testing.T, r *rng, iter, step int, st store.MemoryStore, ledger []store.MemoryItem) {
	t.Helper()
	ctx := t.Context()
	for _, it := range ledger {
		if _, err := st.Get(ctx, it.Namespace, it.ID); err != nil {
			t.Fatal(r.failf("iter=%d step=%d: item %s vanished（G1 禁物理删除被破坏）: %v",
				iter, step, it.ID, err))
		}
	}
	hits, err := st.Search(ctx, store.MemoryQuery{})
	if err != nil {
		t.Fatal(r.failf("iter=%d step=%d: search: %v", iter, step, err))
	}
	for _, h := range hits {
		if h.Item.Status != store.StatusActive {
			t.Fatal(r.failf("iter=%d step=%d: search returned %s status=%s（G2 仅 Active 被破坏）",
				iter, step, h.Item.ID, h.Item.Status))
		}
	}
}

// TestPropertyStoreLifecycleInvariants 记忆治理不变式（memory/store）：
//
//	G1 禁物理删除（随机操作序列下任意曾写入 item 永远可 Get）；
//	G2 Search 默认仅见 Active（Superseded / Revoked 不可召回）；
//	G3 状态迁移合法：Supersede 后旧 = Superseded、新 = Active；Revoke
//	   后 = Revoked；迁移目标永远存在且状态如实。
func TestPropertyStoreLifecycleInvariants(t *testing.T) {
	seed := seedFor(t.Name())
	ctx := t.Context()
	for iter := 0; iter < 8; iter++ {
		r := newRng(seed + int64(iter)*15485863)
		st := store.NewMemoryStore()
		var ledger []store.MemoryItem

		steps := 8 + r.IntN(20)
		for step := 0; step < steps; step++ {
			switch r.IntN(4) {
			case 0, 1: // Put 新 item
				it := randItem(r)
				saved, err := st.Put(ctx, it, store.PutMemoryOptions{})
				if err != nil {
					t.Fatal(r.failf("iter=%d step=%d put: %v", iter, step, err))
				}
				ledger = append(ledger, saved)
			case 2: // Supersede 随机 Active
				act := randomActive(t, r, ctx, st)
				if act == nil {
					continue
				}
				next := randItem(r)
				next.Namespace = act.Namespace
				next.Kind = act.Kind
				next.Taint = act.Taint
				next.SourceRefs = act.SourceRefs
				saved, err := st.Supersede(ctx, act.ID, next)
				if err != nil {
					t.Fatal(r.failf("iter=%d step=%d supersede %s: %v", iter, step, act.ID, err))
				}
				old, err := st.Get(ctx, act.Namespace, act.ID)
				if err != nil {
					t.Fatal(r.failf("iter=%d step=%d: superseded old vanished: %v", iter, step, err))
				}
				if old.Status != store.StatusSuperseded {
					t.Fatal(r.failf("iter=%d step=%d: old status = %s, want superseded", iter, step, old.Status))
				}
				if saved.Status != store.StatusActive {
					t.Fatal(r.failf("iter=%d step=%d: new status = %s, want active", iter, step, saved.Status))
				}
				ledger = append(ledger, saved)
			case 3: // Revoke 随机 Active
				act := randomActive(t, r, ctx, st)
				if act == nil {
					continue
				}
				if err := st.Revoke(ctx, act.ID, r.randStr(10)); err != nil {
					t.Fatal(r.failf("iter=%d step=%d revoke %s: %v", iter, step, act.ID, err))
				}
				old, err := st.Get(ctx, act.Namespace, act.ID)
				if err != nil {
					t.Fatal(r.failf("iter=%d step=%d: revoked item vanished: %v", iter, step, err))
				}
				if old.Status != store.StatusRevoked {
					t.Fatal(r.failf("iter=%d step=%d: revoked status = %s", iter, step, old.Status))
				}
			}
			checkGlobalInvariants(t, r, iter, step, st, ledger)
		}
	}
}

// fixedExtractor 是 candidate 的测试替身：返回固定候选集（Content 互不为
// 子串——「candNN 」唯一首 token 保证去重口径不误拦）。
type fixedExtractor struct{ items []store.MemoryItem }

func (f fixedExtractor) Extract(_ context.Context, _ []*llm.Message) ([]store.MemoryItem, error) {
	return f.items, nil
}

// newPipeline 建一条钉死 namespace 的候选管线。
func newPipeline(t *testing.T, r *rng, st store.MemoryStore, ns []string, items []store.MemoryItem) *candidate.Pipeline {
	t.Helper()
	p, err := candidate.New(candidate.Options{
		Store:     st,
		Extractor: fixedExtractor{items: items},
		Namespace: ns,
		OriginFn:  func() store.SourceRef { return store.SourceRef{Type: store.SourceSession, SessionID: "s", Seq: 1} },
	})
	if err != nil {
		t.Fatal(r.failf("pipeline: %v", err))
	}
	return p
}

// TestPropertyCandidateApprovalInvariants 审批链不变式（memory/candidate）：
//
//	C1 Pending 对默认 Search 不可见（未过审批不进召回）；
//	C2 Approve = 晋升 Active：可见、Confidence=1.0、**Taint 继承不变**
//	   （审批是晋升闸，taint 是数据属性）、Content 继承；
//	C3 Reject = 永不可见；
//	C4 状态机闭合：审批后 Pending 清空；重复 Approve/Reject → ErrNotPending；
//	C5 scope 防污染：父 scope 管线批准子 scope 候选 → ErrOutsideScope。
func TestPropertyCandidateApprovalInvariants(t *testing.T) {
	seed := seedFor(t.Name())
	ctx := t.Context()
	for iter := 0; iter < 8; iter++ {
		r := newRng(seed + int64(iter)*2654435761)
		st := store.NewMemoryStore()

		nsChild := []string{"app", r.randStr(6)}
		n := 2 + r.IntN(5)
		var items []store.MemoryItem
		for i := 0; i < n; i++ {
			items = append(items, store.MemoryItem{
				Kind:    pick(r, kinds),
				Content: fmt.Sprintf("cand%02d %s", i, r.randStr(8)),
			})
		}
		pChild := newPipeline(t, r, st, nsChild, items)

		acc, rep, err := pChild.Extract(ctx, []*llm.Message{llm.UserText("seed")})
		if err != nil {
			t.Fatal(r.failf("iter=%d: extract: %v", iter, err))
		}
		if rep.Stored != n || len(acc) != n {
			t.Fatal(r.failf("iter=%d: stored %d / accepted %d, want %d（无既有内容不应触发去重）",
				iter, rep.Stored, len(acc), n))
		}

		// C1：Pending 不可见。
		hits, err := st.Search(ctx, store.MemoryQuery{Namespace: nsChild})
		if err != nil {
			t.Fatal(r.failf("iter=%d: search: %v", iter, err))
		}
		if len(hits) != 0 {
			t.Fatal(r.failf("iter=%d: pending visible via search（C1 被破坏）", iter))
		}
		pending, err := pChild.Pending(ctx)
		if err != nil {
			t.Fatal(r.failf("iter=%d: pending: %v", iter, err))
		}
		if len(pending) != n {
			t.Fatal(r.failf("iter=%d: pending %d, want %d", iter, len(pending), n))
		}

		// 随机分派 approve / reject。
		for _, p := range pending {
			if r.IntN(2) == 0 {
				saved, err := pChild.Approve(ctx, p.ID)
				if err != nil {
					t.Fatal(r.failf("iter=%d: approve %s: %v", iter, p.ID, err))
				}
				// C2：晋升语义。
				if saved.Status != store.StatusActive {
					t.Fatal(r.failf("iter=%d: approved status = %s", iter, saved.Status))
				}
				if saved.Taint != p.Taint {
					t.Fatal(r.failf("iter=%d: taint changed on approve: %s → %s（taint 不因审批改变）",
						iter, p.Taint, saved.Taint))
				}
				if saved.Confidence != 1.0 {
					t.Fatal(r.failf("iter=%d: approved confidence = %f, want 1.0", iter, saved.Confidence))
				}
				if saved.Content != p.Content {
					t.Fatal(r.failf("iter=%d: approved content drifted", iter))
				}
				viz, err := st.Search(ctx, store.MemoryQuery{Namespace: nsChild})
				if err != nil {
					t.Fatal(r.failf("iter=%d: search: %v", iter, err))
				}
				found := false
				for _, h := range viz {
					if h.Item.ID == saved.ID {
						found = true
					}
				}
				if !found {
					t.Fatal(r.failf("iter=%d: approved item not searchable", iter))
				}
			} else {
				if err := pChild.Reject(ctx, p.ID, r.randStr(8)); err != nil {
					t.Fatal(r.failf("iter=%d: reject %s: %v", iter, p.ID, err))
				}
			}
		}

		// C4：Pending 清空 + 重复操作拒绝。
		pending2, err := pChild.Pending(ctx)
		if err != nil {
			t.Fatal(r.failf("iter=%d: pending#2: %v", iter, err))
		}
		if len(pending2) != 0 {
			t.Fatal(r.failf("iter=%d: pending not drained: %d", iter, len(pending2)))
		}
		for _, p := range pending {
			if _, err := pChild.Approve(ctx, p.ID); !errors.Is(err, candidate.ErrNotPending) {
				t.Fatal(r.failf("iter=%d: re-approve %s err = %v, want ErrNotPending", iter, p.ID, err))
			}
			if err := pChild.Reject(ctx, p.ID, "again"); !errors.Is(err, candidate.ErrNotPending) {
				t.Fatal(r.failf("iter=%d: re-reject %s err = %v, want ErrNotPending", iter, p.ID, err))
			}
		}

		// C5：父 scope 拒绝子 scope 候选。
		nsParent := []string{"app"}
		pParent := newPipeline(t, r, st, nsParent, nil)
		pending3, err := pParent.Pending(ctx)
		if err != nil {
			t.Fatal(r.failf("iter=%d: parent pending: %v", iter, err))
		}
		if len(pending3) != 0 {
			t.Fatal(r.failf("iter=%d: parent pipeline sees child candidates（scope 泄漏）", iter))
		}
		// 父对子 scope 候选的显式批准必须被拒（Get 前缀可见 → 管线拒）。
		childID := pending[0].ID
		if _, err := pParent.Approve(ctx, childID); !errors.Is(err, candidate.ErrOutsideScope) {
			t.Fatal(r.failf("iter=%d: cross-scope approve err = %v, want ErrOutsideScope", iter, err))
		}
	}
}
