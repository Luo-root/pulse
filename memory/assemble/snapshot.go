package assemble

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Luo-root/pulse/memory/store"
)

// snapshotNSKey 是稳定前缀缓存的 namespace 键（与 store 的 nsSep 同约定：
// 元素边界安全）。
func snapshotNSKey(ns []string) string {
	return strings.Join(ns, "\x1f")
}

// stableSnapshotCache 是一个 namespace 的 frozen memory 快照。
type stableSnapshotCache struct {
	items   []store.MemoryItem
	builtAt time.Time
}

// stableSnapshot 实现 §8.3 frozen snapshot policy：
//
//   - 选择 = Kind ∈ {Profile, Decision} 的 Active item（frozen profile +
//     stable project facts），UpdatedAt 降序——「按优先级/最新 revision」；
//   - 缓存键 = namespace；命中即复用（**不重查 store**——保 cache 是本
//     policy 的目的），`RefreshStable` 显式重建；
//   - 缓存 per-namespace 隔离；重建失败不缓存旧空值。
func (a *DefaultAssembler) stableSnapshot(ctx context.Context, in AssembleInput) ([]store.MemoryItem, []Diagnostic) {
	key := snapshotNSKey(in.Namespace)
	a.snapMu.RLock()
	snap, hit := a.snapshots[key]
	a.snapMu.RUnlock()
	if hit && !in.RefreshStable {
		return snap.items, []Diagnostic{{
			Region: "stable-snapshot",
			Reason: fmt.Sprintf("cache hit (built %s)", snap.builtAt.Format(time.RFC3339)),
		}}
	}
	hits, err := a.Store.Search(ctx, store.MemoryQuery{
		Namespace: in.Namespace,
		Kinds:     []store.MemoryKind{store.KindProfile, store.KindDecision},
	})
	if err != nil {
		// 重建失败：有旧缓存则退回旧值并记诊断；无缓存则空前缀 + 诊断
		// （组装不因 store 故障中断，但不静默）。
		if hit {
			return snap.items, []Diagnostic{{
				Region: "stable-snapshot",
				Reason: fmt.Sprintf("refresh failed, serving stale snapshot: %v", err),
			}}
		}
		return nil, []Diagnostic{{Region: "stable-snapshot", Reason: fmt.Sprintf("refresh failed: %v", err)}}
	}
	items := make([]store.MemoryItem, 0, len(hits))
	for _, h := range hits {
		items = append(items, h.Item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].UpdatedAt.After(items[j].UpdatedAt)
		}
		return items[i].ID < items[j].ID
	})
	a.snapMu.Lock()
	a.snapshots[key] = &stableSnapshotCache{items: items, builtAt: time.Now()}
	a.snapMu.Unlock()
	diag := []Diagnostic{{Region: "stable-snapshot", Reason: "rebuilt"}}
	if hit {
		diag = append(diag, Diagnostic{Region: "stable-snapshot", Reason: "explicit refresh"})
	}
	return items, diag
}
