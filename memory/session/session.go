package session

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Luo-root/pulse/llm"
)

// memSession 是 Session 的内存实现。写路径（Append / 冷恢复补写）互斥；
// 信封一旦写入不可变。deleted 标记让已 Delete 的实例继续写入时 fail closed。
type memSession struct {
	mu     sync.Mutex
	hdr    SessionHeader
	reg    *Registry
	store  *memStore // Fork 时注册子会话用；独立测试可为 nil
	events []EventEnvelope
	seq    uint64
	// recovering 是 Open 冷恢复临界区的 try-lock（单写者：第二写者拒绝）。
	recovering atomic.Bool
	deleted    atomic.Bool
}

func newMemSession(header SessionHeader, reg *Registry, store *memStore) *memSession {
	return &memSession{hdr: header, reg: reg, store: store}
}

// Header 返回会话头快照。
func (s *memSession) Header() SessionHeader {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hdr
}

// Append 实现 Session：非幂等，Seq/Time 由 store 分配。校验链（fail closed）：
//
//  1. 未注册类型：Ignorable=true 才放行（写入日志，fold 跳过）；否则拒绝
//     ——把裁决表的「未知 + 默认 false 拒绝 Open」提前到写入端，避免日志
//     造出永远打不开的会话；
//  2. 已注册类型：信封 Ignorable 以注册表分级为准，忽略 draft 上的 flag
//     （已知 Required 永不降级为 Ignorable）；
//  3. SurfaceIntent 仅允许注册为 surface 的类型；Replace 在本阶段直接拒绝
//     （compaction.checkpoint 在 P2-B 引入）；Start > End 拒绝。
//  4. payload 过 codec 校验后才入库。
func (s *memSession) Append(ctx context.Context, draft EventDraft) (EventEnvelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ignorable, err := s.prepareAppend(draft)
	if err != nil {
		return EventEnvelope{}, err
	}
	return s.appendLocked(draft, ignorable), nil
}

// prepareAppend 执行 Append 的校验链（锁内调用，fail closed）：
//
//  1. deleted 检查——与写路径同锁（Delete 置位也持 s.mu）：锁外检查存在
//     「已丢弃会话仍接受写入」的竞态窗口；
//  2. 未注册类型：Ignorable=true 才放行（写入日志，fold 跳过）；否则拒绝
//     ——把裁决表的「未知 + 默认 false 拒绝 Open」提前到写入端，避免日志
//     造出永远打不开的会话；
//  3. 已注册类型：信封 Ignorable 以注册表分级为准，忽略 draft 上的 flag
//     （已知 Required 永不降级为 Ignorable）；
//  4. SurfaceIntent 仅允许注册为 surface 的类型；Replace 在本阶段直接拒绝
//     （compaction.checkpoint 在 P2-B 引入）；Start > End 拒绝。
//  5. payload 过 codec 校验后才入库。
//
// 返回按裁决表归一的信封 Ignorable。JSONL 版复用：校验通过后先落盘、
// 再 appendLocked。
func (s *memSession) prepareAppend(draft EventDraft) (bool, error) {
	if s.deleted.Load() {
		return false, ErrDeleted
	}
	entry, known := s.reg.lookup(draft.Type)
	ignorable := false
	if !known {
		if !draft.Ignorable {
			return false, fmt.Errorf("%w: %q", ErrUnknownEvent, draft.Type)
		}
		ignorable = true
		entry = codecEntry{}
	} else {
		ignorable = entry.class == ClassIgnorable
	}
	if draft.Surface != nil {
		if !entry.hasSurface {
			return false, fmt.Errorf("%w: %q", ErrSurfaceNotAllowed, draft.Type)
		}
		switch draft.Surface.Op {
		case SurfaceAppend:
			// ok
		case SurfaceReplace:
			return false, fmt.Errorf("%w: %q", ErrReplaceNotSupported, draft.Type)
		default:
			return false, fmt.Errorf("%w: op %q", ErrPayloadInvalid, draft.Surface.Op)
		}
		if draft.Surface.Start < 0 || draft.Surface.End < draft.Surface.Start {
			return false, fmt.Errorf("%w: Start=%d End=%d", ErrReplaceRange, draft.Surface.Start, draft.Surface.End)
		}
	}
	if entry.codec != nil {
		if err := entry.codec(draft.Data); err != nil {
			return false, err
		}
	}
	return ignorable, nil
}

// appendLocked 在已持锁状态下追加事件。ignorable 已按裁决表归一：未知
// 扩展保留 draft.Ignorable；已知类型取注册表分级。合成路径（冷恢复）
// 传入的值均为 false。
func (s *memSession) appendLocked(draft EventDraft, ignorable bool) EventEnvelope {
	env := EventEnvelope{
		Seq:       s.seq + 1,
		Time:      time.Now(),
		Type:      draft.Type,
		Data:      draft.Data,
		Ignorable: ignorable,
		Surface:   draft.Surface,
	}
	return s.appendEnvelopeLocked(env)
}

// appendEnvelopeLocked 在已持锁状态下追加一个完整信封（Seq 由调用方定：
// JSONL 版先落盘再入内存，两者必须同 Seq）。返回入参信封。
func (s *memSession) appendEnvelopeLocked(env EventEnvelope) EventEnvelope {
	s.seq = env.Seq
	s.events = append(s.events, env)
	return env
}

// Events 实现 Session：返回 Seq >= fromSeq 的拷贝。信封视为不可变——
// 调用方不得改写 Data/Surface（RawMessage 与指针按共享只读约定处理）。
func (s *memSession) Events(ctx context.Context, fromSeq uint64) ([]EventEnvelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EventEnvelope, 0, len(s.events))
	for _, ev := range s.events {
		if ev.Seq >= fromSeq {
			out = append(out, ev)
		}
	}
	return out, nil
}

// Surface 实现 Session：锁内拷贝日志、锁外 fold（投影是无副作用的纯函数）。
// live 会话不在这里修复未闭合现场——修复只发生在 Open 冷恢复并写回日志。
func (s *memSession) Surface(ctx context.Context) ([]*llm.Message, error) {
	s.mu.Lock()
	events := make([]EventEnvelope, len(s.events))
	copy(events, s.events)
	s.mu.Unlock()
	return Fold(events, s.reg)
}

// Fork 实现 Session：拷贝 Seq 1..atSeq 为子会话 seed。切点校验（§9.3）：
// 落在 tool 组中间（seed 里仍有缺 result 的 ToolCall）→ ErrForkSplitToolGroup，
// 禁止拷出非法 surface。子会话注册进同一 store（内存实现：进程内唯一），
// 父会话后续追加不会污染子 seed。
func (s *memSession) Fork(ctx context.Context, atSeq uint64) (Session, error) {
	s.mu.Lock()
	if s.deleted.Load() {
		s.mu.Unlock()
		return nil, ErrDeleted
	}
	if atSeq == 0 || atSeq > s.seq {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: atSeq=%d max=%d", ErrForkBadAt, atSeq, s.seq)
	}
	seed := make([]EventEnvelope, atSeq)
	copy(seed, s.events[:atSeq])
	s.mu.Unlock()

	// 组边界校验在锁外做（纯扫描）：pending 非空即切在组内。
	pending, err := pendingToolCalls(seed, s.reg)
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("%w: atSeq=%d unpaired=%v", ErrForkSplitToolGroup, atSeq, pending)
	}

	hdr := s.Header()
	hdr.SessionID = newID()
	hdr.CreatedAt = time.Now()
	hdr.ParentSessionID = s.hdr.SessionID
	hdr.SeedLength = atSeq
	hdr.DelegationDepth++
	child := newMemSession(hdr, s.reg, s.store)
	child.events = seed
	child.seq = atSeq

	if s.store != nil {
		s.store.mu.Lock()
		for s.store.sessions[hdr.SessionID] != nil { // newID 冲突概率极低，防御性重试
			s.store.mu.Unlock()
			hdr.SessionID = newID()
			child.hdr.SessionID = hdr.SessionID
			s.store.mu.Lock()
		}
		s.store.sessions[hdr.SessionID] = child
		s.store.mu.Unlock()
	}
	return child, nil
}

// Flush 实现 Session：内存实现是成功空操作（语义占位，设计 §7.1）；
// 「Flush 才 fsync、崩溃只保证 Flush 点之前」的语义在 P2-A2 落地。
// deleted 检查与写路径同锁（同 Append）。
func (s *memSession) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted.Load() {
		return ErrDeleted
	}
	return nil
}
