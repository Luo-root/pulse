package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Luo-root/pulse/llm"
)

// RecoverPolicy 决定 JSONL 冷恢复对「未闭合现场」（悬空 turn/step、缺
// result 的 ToolCall）的处理（§9.3 恢复策略参数化，#158）。
type RecoverPolicy int

const (
	// RecoverSyntheticInterrupted（默认）：合成 interrupted 闭环真实写回
	// 日志（tool.result(IsError) → step.ended → turn.ended，与嵌套闭合
	// 顺序一致）——fail closed，那轮作废但日志合法。
	RecoverSyntheticInterrupted RecoverPolicy = iota
	// RecoverExposePending：不合成任何闭环——未决现场挂在会话句柄上，
	// 由宿主经 Pending() / ResolvePending 系列裁决（补真实结果 / 显式中
	// 断 / 一键走默认合成）。进程在 HITL 等待批准时挂掉的场景由此回到
	// 「等待点」而不是作废。未决期间 Surface 照常投影（运行中间态语义）。
	RecoverExposePending
	// RecoverReject：存在未决即拒绝 Open（ErrPendingEvents）——比默认更
	// 严的档位，供审计场景。
	RecoverReject
)

// ErrPendingEvents 是 RecoverReject 档下 Open 遇到未决现场的拒绝原因
// （errors.Is 判定）。
var ErrPendingEvents = errors.New("session: pending events require resolution")

// WithRecoverPolicy 设置 JSONLStore 的冷恢复策略；默认
// RecoverSyntheticInterrupted（不传即现状，既有行为零变化）。
func WithRecoverPolicy(p RecoverPolicy) jsonlOption {
	return func(s *JSONLStore) { s.recover = p }
}

// PendingCall 是 ExposePending 模式下的一个未决 ToolCall。
type PendingCall struct {
	ToolCallID string
	// Seq 是发出该 tool call 的 assistant 消息事件序号（溯源锚点）。
	Seq uint64
}

// PendingState 是 ExposePending 模式下 Open 时的未决现场快照。
type PendingState struct {
	Calls []PendingCall
	// HasOpenStep / OpenStepID：悬空的 step（started 未闭合）。
	HasOpenStep bool
	OpenStepID  string
	// HasOpenTurn / OpenTurnID：悬空的 turn。
	HasOpenTurn bool
	OpenTurnID  string
}

// empty 报告快照是否无任何未决。
func (p PendingState) empty() bool {
	return len(p.Calls) == 0 && !p.HasOpenStep && !p.HasOpenTurn
}

// stateFromIncomplete 把内部未决现场转成对外快照。
func stateFromIncomplete(st incompleteState, seqByID map[string]uint64) PendingState {
	ps := PendingState{
		Calls:       make([]PendingCall, 0, len(st.pendingCalls)),
		HasOpenStep: st.openStep,
		OpenStepID:  st.stepID,
		HasOpenTurn: st.openTurn,
		OpenTurnID:  st.turnID,
	}
	for _, id := range st.pendingCalls {
		ps.Calls = append(ps.Calls, PendingCall{ToolCallID: id, Seq: seqByID[id]})
	}
	return ps
}

// Pending 返回当前未决现场（ExposePending 模式）；默认模式恒为空——
// 未决在 Open 时已合成为 interrupted 闭环。
func (s *jsonlSession) Pending() PendingState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return PendingState{}
	}
	seqByID := map[string]uint64{}
	for _, ev := range s.events {
		if ev.Type == EventMessageAssistant {
			var p MessagePayload
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				continue
			}
			for _, part := range p.Parts {
				if part.Kind == llm.PartToolCall && part.ToolCallValue != nil {
					seqByID[part.ToolCallValue.ID] = ev.Seq
				}
			}
		}
	}
	return stateFromIncomplete(*s.pending, seqByID)
}

// ResolvePendingOption 是一次未决裁决。
type ResolvePendingOption struct {
	// ToolCallID 要补结果的 ToolCall（与 Result 二选一）。
	ToolCallID string
	// Result 非空 = 补写真实 tool.result（重发或人工补结果）。
	Result *ToolResultPayload
	// Interrupted 为 true = 对该项走默认合成闭环（tool.result
	// IsError "interrupted" / step.ended / turn.ended reason=interrupted）。
	Interrupted bool
}

// ResolvePending 裁决一个未决项（ExposePending 模式）：补真实结果或
// 显式中断。落盘走与 Append 同一条校验链（fail closed 不放水）。
// 裁决不存在/已解决的项 → ErrPendingEvents。
func (s *jsonlSession) ResolvePending(ctx context.Context, opt ResolvePendingOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return ErrPendingEvents
	}
	// tool.result 裁决。
	if opt.Result != nil {
		if err := s.removePendingCall(opt.Result.ToolCallID); err != nil {
			return err
		}
		_, err := s.appendLocked(EventDraft{
			Type:    EventToolResult,
			Data:    mustJSON(*opt.Result),
			Surface: &SurfaceIntent{Op: SurfaceAppend},
		})
		return err
	}
	// interrupted 裁决：对单项补默认闭环。
	if opt.Interrupted {
		if opt.ToolCallID != "" {
			if err := s.removePendingCall(opt.ToolCallID); err != nil {
				return err
			}
			_, err := s.appendLocked(EventDraft{
				Type:    EventToolResult,
				Data:    mustJSON(ToolResultPayload{ToolCallID: opt.ToolCallID, Text: interruptedResultText, IsError: true}),
				Surface: &SurfaceIntent{Op: SurfaceAppend},
			})
			return err
		}
		// 无 ID 的 interrupted = 闭合悬空 step/turn（按嵌套顺序 step 先）。
		if s.pending.openStep {
			id := s.pending.stepID
			if err := s.closeStepLocked(ctx, id); err != nil {
				return err
			}
			return nil
		}
		if s.pending.openTurn {
			id := s.pending.turnID
			if err := s.closeTurnLocked(ctx, id); err != nil {
				return err
			}
			return nil
		}
		return ErrPendingEvents
	}
	return fmt.Errorf("%w: resolve requires Result or Interrupted", ErrPendingEvents)
}

// ResolveAsInterrupted 一键把全部未决走默认合成闭环——等价默认档行为，
// 但由宿主显式选择（先检查再裁决，例如人工看过现场后决定作废）。
// 全部解决后 Pending() 归零。
func (s *jsonlSession) ResolveAsInterrupted(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return ErrPendingEvents
	}
	drafts := synthDrafts(*s.pending)
	for _, d := range drafts {
		if _, err := s.appendLocked(d); err != nil {
			return err
		}
	}
	s.pending = nil
	return nil
}

// removePendingCall 从未决集中摘除一个 ToolCall；不存在即拒绝。
func (s *jsonlSession) removePendingCall(id string) error {
	for i, c := range s.pending.pendingCalls {
		if c == id {
			s.pending.pendingCalls = append(s.pending.pendingCalls[:i], s.pending.pendingCalls[i+1:]...)
			s.maybeClearPendingLocked()
			return nil
		}
	}
	return fmt.Errorf("%w: tool call %q is not pending", ErrPendingEvents, id)
}

// maybeClearPendingLocked 在全部未决解决后清空 pending（nil = 无待裁决）。
func (s *jsonlSession) maybeClearPendingLocked() {
	if s.pending != nil && len(s.pending.pendingCalls) == 0 && !s.pending.openStep && !s.pending.openTurn {
		s.pending = nil
	}
}

// closeStepLocked / closeTurnLocked 闭合悬空的 step/turn。
func (s *jsonlSession) closeStepLocked(ctx context.Context, id string) error {
	if _, err := s.appendLocked(EventDraft{
		Type: EventStepEnded,
		Data: mustJSON(LifecyclePayload{ID: id, Reason: ReasonInterrupted}),
	}); err != nil {
		return err
	}
	s.pending.openStep = false
	s.pending.stepID = ""
	s.maybeClearPendingLocked()
	return nil
}

func (s *jsonlSession) closeTurnLocked(ctx context.Context, id string) error {
	if _, err := s.appendLocked(EventDraft{
		Type: EventTurnEnded,
		Data: mustJSON(LifecyclePayload{ID: id, Reason: ReasonInterrupted}),
	}); err != nil {
		return err
	}
	s.pending.openTurn = false
	s.pending.turnID = ""
	s.maybeClearPendingLocked()
	return nil
}
