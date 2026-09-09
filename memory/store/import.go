package store

// item 导出/导入（#152）：跨后端搬迁的保真路径。
//
// 导出 = Search 全量薄包装（强制 IncludeInactive——Superseded/Revoked 也
// 是记忆库的一部分）。导入 = 可选能力接口 ImportStore（PutImport：保留
// item 携带的 KnownAt/CreatedAt/UpdatedAt/Revision/Status/Taint，校验链
// 照常 fail closed）；未实现 → ErrImportUnsupported，不做静默降级——
// 双时态字段被重置的「迁移成功」比失败更糟。
//
// 幂等：ImportItems 逐条 Get 探测——已存在且内容一致 → Skipped；存在且
// 不同 → Conflicts 记录（不覆盖，先到先得）；不存在 → PutImport。
// Taint 原样保留：导出导入不得绕过 promotion gate 洗白信任级。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrImportUnsupported：目标 store 未实现 ImportStore——保真导入不可用。
var ErrImportUnsupported = errors.New("store: does not support import (PutImport)")

// ImportStore 是支持保真导入的 MemoryStore 可选能力：item 携带的时间域
// 与 Revision 原样入库（validate 照常全量执行）；已存在同 ID →
// ErrItemExists（导入方先探测，防静默覆盖）。
type ImportStore interface {
	PutImport(ctx context.Context, item MemoryItem) (MemoryItem, error)
}

// ExportItems 按 q 导出 items（强制 IncludeInactive：非 Active 状态也是
// 记忆库状态机的一部分，丢掉它们等于丢掉 Supersede/Revoke 历史）。产物
// 为值切片，序列化交给调用方（MemoryItem 是导出字段集的 canonical 表示，
// 序列化用 Go 默认 JSON 字段名，与导入侧同一结构对称）。
func ExportItems(ctx context.Context, ms MemoryStore, q MemoryQuery) ([]MemoryItem, error) {
	q.IncludeInactive = true
	hits, err := ms.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	items := make([]MemoryItem, 0, len(hits))
	for _, h := range hits {
		items = append(items, h.Item)
	}
	return items, nil
}

// Conflict 是导入冲突记录：目标已存在同 ID 且内容与导出不一致。
type Conflict struct {
	ID     string
	Reason string
}

// ImportReport 是一次导入的回执。
type ImportReport struct {
	Imported  int
	Skipped   int
	Conflicts []Conflict
}

// ImportOptions 预留导入开关（当前无字段；namespace 重映射等后续需求
// 落这里，避免签名破坏）。
type ImportOptions struct{}

// ImportItems 把导出的 items 保真写入目标 store。失败语义：item 校验
// 失败 / 目标已存在且不同 → 记入 Conflicts 继续（回执完整呈现）；store
// 未实现 ImportStore → ErrImportUnsupported 整单拒绝。
func ImportItems(ctx context.Context, ms MemoryStore, items []MemoryItem) (ImportReport, error) {
	imp, ok := ms.(ImportStore)
	if !ok {
		return ImportReport{}, ErrImportUnsupported
	}
	var report ImportReport
	for _, item := range items {
		if err := item.validate(); err != nil {
			report.Conflicts = append(report.Conflicts, Conflict{ID: item.ID, Reason: err.Error()})
			continue
		}
		cur, err := ms.Get(ctx, item.Namespace, item.ID)
		switch {
		case err == nil:
			if itemEqual(cur, item) {
				report.Skipped++
				continue
			}
			report.Conflicts = append(report.Conflicts, Conflict{ID: item.ID, Reason: "target exists with different content"})
		case errors.Is(err, ErrItemNotFound):
			if _, err := imp.PutImport(ctx, item); err != nil {
				if errors.Is(err, ErrItemExists) {
					report.Conflicts = append(report.Conflicts, Conflict{ID: item.ID, Reason: "target exists with different content"})
					continue
				}
				return report, fmt.Errorf("store: import %s: %w", item.ID, err)
			}
			report.Imported++
		default:
			return report, fmt.Errorf("store: probe %s: %w", item.ID, err)
		}
	}
	return report, nil
}

// itemEqual 判定内容一致（幂等重跑口径）：字段级比较，三个 time 字段用
// Equal()——同一时刻的本地时区表示（手动 Put 分配）与 UTC 表示（JSON
// 往返）视为一致（SameInstant 语义），不误报 Conflict；Structured 做
// 空/null 归一后按字节比较（json.Marshal 对 nil/空/null 同样归一为 null，
// 口径不变）；Namespace/SourceRefs 按切片逐元素相等。
func itemEqual(a, b MemoryItem) bool {
	if a.ID != b.ID || a.Kind != b.Kind || a.Content != b.Content ||
		a.Status != b.Status || a.Confidence != b.Confidence || a.Taint != b.Taint {
		return false
	}
	if !slices.Equal(a.Namespace, b.Namespace) || !slices.Equal(a.SourceRefs, b.SourceRefs) {
		return false
	}
	if a.Revision != b.Revision || !structuredEqual(a.Structured, b.Structured) {
		return false
	}
	return a.ValidFrom.Equal(b.ValidFrom) && timePtrEqual(a.ValidUntil, b.ValidUntil) &&
		a.KnownAt.Equal(b.KnownAt) && a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt)
}

// structuredEqual 比较 Structured：TrimSpace 后把字面 null 归一为空——
// json.Marshal 对 nil / 空切片 / 字面 null 都产出 null，字段级比较保持
// 同一归一口径；其余按字节相等。
func structuredEqual(a, b json.RawMessage) bool {
	a, b = bytes.TrimSpace(a), bytes.TrimSpace(b)
	if string(a) == "null" {
		a = nil
	}
	if string(b) == "null" {
		b = nil
	}
	return bytes.Equal(a, b)
}

// timePtrEqual 双指针 SameInstant 比较：都 nil 视为相等，都非 nil 用
// Equal（单侧 nil 不等）。
func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// PutImport 实现 ImportStore（内存版）：item 携带的时间域与 Revision
// 原样入库；校验链与 Put 同一套；已存在拒绝（导入方先探测）。
func (s *memStore) PutImport(ctx context.Context, item MemoryItem) (MemoryItem, error) {
	if err := item.validate(); err != nil {
		return MemoryItem{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctxErr(ctx); err != nil {
		return MemoryItem{}, err
	}
	if _, exists := s.items[item.ID]; exists {
		return MemoryItem{}, fmt.Errorf("%w: id %s", ErrItemExists, item.ID)
	}
	stored := item
	s.items[item.ID] = &storedItem{item: stored}
	return item, nil
}
