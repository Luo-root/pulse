package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// NewMemoryStore 创建 C1 的内存 item store：先把 canonical 语义测绿
// （namespace 隔离、状态机、CAS）；SQLite + FTS 是 C2。
func NewMemoryStore() *memStore {
	return &memStore{items: make(map[string]*storedItem)}
}

// memStore 是 MemoryStore 的内存实现。
type memStore struct {
	mu    sync.RWMutex
	items map[string]*storedItem
	audit []AuditEntry
	nowFn func() time.Time // 测试注入
	seqFn func() uint64    // 审计序号
}

// storedItem 是内存中的 item 本体（值拷贝出入，防外部改写内部状态）。
type storedItem struct {
	item MemoryItem
}

// AuditEntry 是 Supersede/Revoke 的审计记录（「可解释审计」）：C1 内存版
// 通过实现特有 AuditLog() 读取；C2 落 SQLite audit 表。MemoryItem 结构
// （设计冻结）不承载 reason。
type AuditEntry struct {
	Seq    uint64 // 单调递增
	At     time.Time
	Action string // supersede / revoke
	ItemID string // 被操作 item（Supersede 时为旧 ID）
	Reason string // Revoke 的 reason；Supersede 为空
	NextID string // Supersede 的 next.ID；Revoke 为空
}

// Put 实现 MemoryStore：新建（ExpectedRevision=0）或更新（CAS）。
// Revision/KnownAt/CreatedAt/UpdatedAt 由 store 分配，调用方不填
// （传入值被覆盖）。
func (s *memStore) Put(ctx context.Context, item MemoryItem, opts PutMemoryOptions) (MemoryItem, error) {
	if err := item.validate(); err != nil {
		return MemoryItem{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctxErr(ctx); err != nil {
		return MemoryItem{}, err
	}
	now := s.now()
	cur, exists := s.items[item.ID]
	if opts.ExpectedRevision == 0 {
		if exists {
			return MemoryItem{}, fmt.Errorf("%w: id %s", ErrItemExists, item.ID)
		}
		item.Revision = 1
		item.KnownAt = now
		item.CreatedAt = now
		item.UpdatedAt = now
		stored := item
		s.items[item.ID] = &storedItem{item: stored}
		return item, nil
	}
	if !exists {
		return MemoryItem{}, fmt.Errorf("%w: id %s", ErrItemNotFound, item.ID)
	}
	if cur.item.Revision != opts.ExpectedRevision {
		return MemoryItem{}, fmt.Errorf("%w: id %s expected rev %d, current %d",
			ErrRevisionConflict, item.ID, opts.ExpectedRevision, cur.item.Revision)
	}
	// 状态迁移只走 Supersede/Revoke（各有审计与替代链）：Put 更新禁止
	// 改变 Status——否则 active→pending 绕过 P2-D 的 taint gate、active→
	// superseded 绕过替代链（§10.2 追溯性）。
	if cur.item.Status != item.Status {
		return MemoryItem{}, fmt.Errorf("%w: id %s %s → %s（用 Supersede/Revoke）",
			ErrStatusTransition, item.ID, cur.item.Status, item.Status)
	}
	// CAS 更新：保留不可变字段（CreatedAt/KnownAt），Revision 前进。
	prev := cur.item
	item.Revision = prev.Revision + 1
	item.CreatedAt = prev.CreatedAt
	item.KnownAt = prev.KnownAt
	item.UpdatedAt = now
	stored := item
	s.items[item.ID] = &storedItem{item: stored}
	s.appendAuditLocked(AuditEntry{Action: "put", ItemID: item.ID, At: now})
	return item, nil
}

// Get 实现 MemoryStore：ns 必须是 item.Namespace 的前缀（层级可见），
// 否则按不存在处理——跨 namespace 不互见。
func (s *memStore) Get(ctx context.Context, ns []string, id string) (MemoryItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ctxErr(ctx); err != nil {
		return MemoryItem{}, err
	}
	cur, exists := s.items[id]
	if !exists || !namespaceVisible(ns, cur.item.Namespace) {
		return MemoryItem{}, fmt.Errorf("%w: id %s", ErrItemNotFound, id)
	}
	return cur.item, nil
}

// Search 实现 MemoryStore：namespace 前缀 + Kind + 关键词（大小写不敏感
// 子串）+ 状态过滤；按 UpdatedAt 降序稳定排序；Limit 硬上限。未命中返回
// 空切片，不伪造。
func (s *memStore) Search(ctx context.Context, q MemoryQuery) ([]MemoryHit, error) {
	if q.Limit < 0 {
		return nil, fmt.Errorf("%w: negative limit", ErrInvalidQuery)
	}
	needle := asciiFold(strings.TrimSpace(q.Query))
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ctxErr(ctx); err != nil {
		return nil, err
	}
	hits := make([]MemoryHit, 0)
	for _, cur := range s.items {
		it := cur.item
		if !namespaceVisible(q.Namespace, it.Namespace) {
			continue
		}
		if !q.IncludeInactive && it.Status != StatusActive {
			continue
		}
		if len(q.Kinds) > 0 && !containsKind(q.Kinds, it.Kind) {
			continue
		}
		if needle != "" && !strings.Contains(asciiFold(it.Content), needle) {
			continue
		}
		hits = append(hits, MemoryHit{Item: it})
	}
	// UpdatedAt 降序 + ID tiebreak：稳定排序，同刻不抖动。
	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i].Item, hits[j].Item
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.ID < b.ID
	})
	if q.Limit > 0 && len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	return hits, nil
}

// Supersede 实现 MemoryStore：旧 item Status→Superseded；next 入库
// （新 Revision=1，KnownAt/CreatedAt=now）。next.ID 不得等于 oldID；
// oldID 必须存在且非 Revoked（终态）。
func (s *memStore) Supersede(ctx context.Context, oldID string, next MemoryItem) (MemoryItem, error) {
	if next.ID == oldID {
		return MemoryItem{}, fmt.Errorf("%w: %s", ErrSupersedeSelf, oldID)
	}
	if err := next.validate(); err != nil {
		return MemoryItem{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctxErr(ctx); err != nil {
		return MemoryItem{}, err
	}
	cur, exists := s.items[oldID]
	if !exists {
		return MemoryItem{}, fmt.Errorf("%w: id %s", ErrItemNotFound, oldID)
	}
	if cur.item.Status == StatusRevoked {
		return MemoryItem{}, fmt.Errorf("%w: id %s", ErrSupersedeRevoked, oldID)
	}
	if _, dup := s.items[next.ID]; dup {
		return MemoryItem{}, fmt.Errorf("%w: id %s", ErrItemExists, next.ID)
	}
	now := s.now()
	next.Revision = 1
	next.KnownAt = now
	next.CreatedAt = now
	next.UpdatedAt = now
	stored := next
	s.items[next.ID] = &storedItem{item: stored}

	superseded := cur.item
	superseded.Status = StatusSuperseded
	superseded.UpdatedAt = now
	superseded.Revision++
	cur.item = superseded
	s.appendAuditLocked(AuditEntry{Action: "supersede", ItemID: oldID, NextID: next.ID, At: now})
	return next, nil
}

// Revoke 实现 MemoryStore：Status→Revoked + 审计（reason 走 store 审计，
// 不进 MemoryItem）。Revoked 幂等成功；Superseded → 拒绝（宿主应明确
// 操作对象：先找到生效版本）。
func (s *memStore) Revoke(ctx context.Context, id string, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctxErr(ctx); err != nil {
		return err
	}
	cur, exists := s.items[id]
	if !exists {
		return fmt.Errorf("%w: id %s", ErrItemNotFound, id)
	}
	switch cur.item.Status {
	case StatusRevoked:
		return nil // 幂等
	case StatusSuperseded:
		return fmt.Errorf("%w: id %s（应撤销生效版本）", ErrRevokeSuperseded, id)
	}
	now := s.now()
	revoked := cur.item
	revoked.Status = StatusRevoked
	revoked.UpdatedAt = now
	revoked.Revision++
	cur.item = revoked
	s.appendAuditLocked(AuditEntry{Action: "revoke", ItemID: id, Reason: reason, At: now})
	return nil
}

// AuditLog 返回审计记录拷贝（实现特有方法，非 §7.1 接口面；reason 落点
// 见票 #76 定稿——MemoryItem 结构不加字段）。
func (s *memStore) AuditLog() []AuditEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AuditEntry, len(s.audit))
	copy(out, s.audit)
	return out
}

// appendAuditLocked 追加审计（调用方持写锁）。
func (s *memStore) appendAuditLocked(e AuditEntry) {
	e.Seq = s.nextAuditSeq()
	s.audit = append(s.audit, e)
}

func (s *memStore) nextAuditSeq() uint64 {
	if s.seqFn != nil {
		return s.seqFn()
	}
	if len(s.audit) == 0 {
		return 1
	}
	return s.audit[len(s.audit)-1].Seq + 1
}

func (s *memStore) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// ctxErr 尊重取消：持锁后、返回前检查（存储操作都是内存瞬时，取消检查
// 是接口礼貌而非必要路径）。
func (s *memStore) ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// namespaceVisible 报告查询 ns 是否可见 item ns：q 是 item 的前缀（含
// 相等）即可见——父 scope 读得到子 scope，兄弟绝不互见。q 为空 = 全局。
func namespaceVisible(q, itemNS []string) bool {
	if len(q) == 0 {
		return true
	}
	if len(q) > len(itemNS) {
		return false
	}
	for i, part := range q {
		if itemNS[i] != part {
			return false
		}
	}
	return true
}

func containsKind(kinds []MemoryKind, k MemoryKind) bool {
	for _, want := range kinds {
		if want == k {
			return true
		}
	}
	return false
}

// asciiFold 把 ASCII 大写折叠为小写，非 ASCII 字符原样保留——这是
// Search 关键词匹配的**统一口径**（内存版与 SQLite 版都必须如此：
// SQLite 内置 lower() 只折叠 ASCII，内存版若用 Unicode-aware ToLower
// 会与 SQL 下推分叉——复审实测）。「大小写折叠仅 ASCII」是契约的一部分，
// 重音/西里尔等非 ASCII 大写不做折叠。
func asciiFold(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}
