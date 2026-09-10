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
//
// 注意：策略是 Store 级、不随会话持久化，且**默认档的合成会真实写回
// 日志（破坏性）**——同一份 JSONL 用默认档 Open 过一次，未决就被合成了，
// ExposePending 的机会窗口就此关闭。要用 ExposePending 裁决，从第一次
// Open 起就该带这个策略。
type RecoverPolicy int

const (
	// RecoverSyntheticInterrupted（默认）：合成 interrupted 闭环真实写回
	// 日志（tool.result(IsError) → step.ended → turn.ended，与嵌套闭合
	// 顺序一致）——fail closed，那轮作废但日志合法。
	RecoverSyntheticInterrupted RecoverPolicy = iota
	// RecoverExposePending：不合成任何闭环——未决现场挂在会话句柄上，
	// 由宿主经 Recoverable 接口裁决（补真实结果 / 显式中断 / 一键走默认
	// 合成）。进程在 HITL 等待批准时挂掉的场景由此回到「等待点」而不是
	// 作废。未决期间 Surface() 返回 ErrPendingEvents——未决 surface 喂给
	// 模型是坏请求（unpaired PartToolCall）；裁决依据用 Pending() 快照。
	RecoverExposePending
	// RecoverReject：存在未决即拒绝 Open（ErrPendingEvents）——比默认更
	// 严的档位，供审计场景。
	RecoverReject
)

// ErrPendingEvents 是恢复路径的统一哨兵：RecoverReject 档的 Open 拒绝、
// ExposePending 档的 Surface 拒绝、裁决参数错误，均可用 errors.Is 判定。
var ErrPendingEvents = errors.New("session: pending events require resolution")

// WithRecoverPolicy 设置 JSONLStore 的冷恢复策略；默认
// RecoverSyntheticInterrupted（不传即现状，既有行为零变化）。未知枚举值
// 视同默认档（落回合成闭环——未知即保守）。
func WithRecoverPolicy(p RecoverPolicy) jsonlOption {
	return func(s *JSONLStore) { s.recover = p }
}

// Recoverable 是 ExposePending 模式下会话句柄的裁决面。与 Close 同为
// 类型断言使用的扩展（Session 接口方法集冻结不破）：
//
//	if r, ok := sess.(session.Recoverable); ok { ... r.Pending() ... }
type Recoverable interface {
	// Pending 返回当前未决现场快照。
	Pending() PendingState
	// ResolvePending 裁决一个未决项。
	ResolvePending(ctx context.Context, opt ResolvePendingOption) error
	// ResolveAsInterrupted 一键把全部未决走默认合成闭环。
	ResolveAsInterrupted(ctx context.Context) error
}

// PendingCall 是 ExposePending 模式下的一个未决 ToolCall：ID + 发出事件
// 的 Seq（溯源锚点）+ 当时的调用载荷（重发或人工补齐需要的全部信息）。
type PendingCall struct {
	ToolCallID string
	Seq        uint64
	// Name / Arguments 是当时 assistant 消息里的调用载荷。
	Name      string
	Arguments json.RawMessage
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

// pendingCallInfo 是 Pending() 扫描 assistant 消息时收集的调用载荷。
type pendingCallInfo struct {
	seq  uint64
	name string
	args json.RawMessage
}

// stateFromIncomplete 把内部未决现场转成对外快照（含调用载荷）。
func stateFromIncomplete(st incompleteState, calls map[string]pendingCallInfo) PendingState {
	ps := PendingState{
		Calls:       make([]PendingCall, 0, len(st.pendingCalls)),
		HasOpenStep: st.openStep,
		OpenStepID:  st.stepID,
		HasOpenTurn: st.openTurn,
		OpenTurnID:  st.turnID,
	}
	for _, id := range st.pendingCalls {
		info := calls[id]
		ps.Calls = append(ps.Calls, PendingCall{
			ToolCallID: id, Seq: info.seq, Name: info.name, Arguments: info.args,
		})
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
	calls := map[string]pendingCallInfo{}
	for _, ev := range s.events {
		if ev.Type != EventMessageAssistant {
			continue
		}
		var p MessagePayload
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			continue
		}
		for _, part := range p.Parts {
			if part.Kind == llm.PartToolCall && part.ToolCallValue != nil {
				calls[part.ToolCallValue.ID] = pendingCallInfo{
					seq: ev.Seq, name: part.ToolCallValue.Name, args: part.ToolCallValue.Arguments,
				}
			}
		}
	}
	return stateFromIncomplete(*s.pending, calls)
}

// ResolvePendingOption 是一次未决裁决。字段约束：
//   - Result != nil：补真实 tool.result（ToolCallID 必填且须在未决集）；
//   - Interrupted && ToolCallID != ""：对该 ToolCall 补默认中断闭环；
//   - Interrupted && ToolCallID == ""：闭合一个悬空的 step（若有），否则
//     闭合悬空的 turn（调用方循环调用直到 Pending().empty()）；
//   - Result 与 Interrupted 互斥；全部为零值是参数错误。
//
// 参数校验（互斥、空参数、目标不在未决集）全部前置——**校验不过不落
// 盘**，未知 ID 绝不写事件；校验通过后走先 append 后改内存的原子序。
type ResolvePendingOption struct {
	// ToolCallID 要裁决的 ToolCall（配合 Result 或 Interrupted）。
	ToolCallID string
	// Result 非空 = 补写真实 tool.result（重发或人工补结果）。
	Result *ToolResultPayload
	// Interrupted 为 true = 走默认中断闭环（见结构注释的三种子形态）。
	Interrupted bool
}

// ResolvePending 裁决一个未决项（ExposePending 模式）。落盘走与 Append
// 同一条校验链；**先 append 成功、后改内存未决集**——append 失败时未决
// 集原样保留（内存与日志不分叉），重试不会丢裁决目标。
func (s *jsonlSession) ResolvePending(ctx context.Context, opt ResolvePendingOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return ErrPendingEvents
	}
	// 参数正交校验：Result 与 Interrupted 互斥；两者都空是参数错误。
	if opt.Result != nil && opt.Interrupted {
		return fmt.Errorf("%w: Result and Interrupted are mutually exclusive", ErrPendingEvents)
	}
	if opt.Result != nil {
		if opt.Result.ToolCallID == "" {
			opt.Result.ToolCallID = opt.ToolCallID
		}
		if opt.Result.ToolCallID == "" {
			return fmt.Errorf("%w: Result requires ToolCallID", ErrPendingEvents)
		}
		// 校验前置：目标不在未决集就不落盘，日志绝不出现配不了对的
		// 野 tool.result。
		if !s.pendingHasCallLocked(opt.Result.ToolCallID) {
			return fmt.Errorf("%w: tool call %q is not pending", ErrPendingEvents, opt.Result.ToolCallID)
		}
		if _, err := s.appendLocked(EventDraft{
			Type:    EventToolResult,
			Data:    mustJSON(*opt.Result),
			Surface: &SurfaceIntent{Op: SurfaceAppend},
		}); err != nil {
			return err // append 失败：pending 原样，重试安全
		}
		return s.removePendingCallLocked(opt.Result.ToolCallID)
	}
	if opt.Interrupted {
		if opt.ToolCallID != "" {
			if !s.pendingHasCallLocked(opt.ToolCallID) {
				return fmt.Errorf("%w: tool call %q is not pending", ErrPendingEvents, opt.ToolCallID)
			}
			if _, err := s.appendLocked(EventDraft{
				Type:    EventToolResult,
				Data:    mustJSON(ToolResultPayload{ToolCallID: opt.ToolCallID, Text: interruptedResultText, IsError: true}),
				Surface: &SurfaceIntent{Op: SurfaceAppend},
			}); err != nil {
				return err
			}
			return s.removePendingCallLocked(opt.ToolCallID)
		}
		// 无 ID 的中断 = 闭合悬空 step（优先），否则闭合悬空 turn。
		if s.pending.openStep {
			return s.closeStepLocked()
		}
		if s.pending.openTurn {
			return s.closeTurnLocked()
		}
		return fmt.Errorf("%w: nothing pending to interrupt (no calls, no open step/turn)", ErrPendingEvents)
	}
	return fmt.Errorf("%w: resolve requires Result or Interrupted", ErrPendingEvents)
}

// ResolveAsInterrupted 一键把全部未决走默认合成闭环——等价默认档行为，
// 但由宿主显式选择（先检查再裁决，例如人工看过现场后决定作废）。
// 逐项落盘、**每项成功后立即从未决集摘除**：中途失败即停，已写入的与
// 内存对齐，重试只补剩余项（不会重写已成功的合成事件）。全部完成后
// pending 置 nil。
func (s *jsonlSession) ResolveAsInterrupted(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil || !pendingIncomplete(*s.pending) {
		return ErrPendingEvents
	}
	// 与 synthDrafts 同序：tool results → step → turn。
	for _, id := range s.pending.pendingCalls {
		if _, err := s.appendLocked(EventDraft{
			Type:    EventToolResult,
			Data:    mustJSON(ToolResultPayload{ToolCallID: id, Text: interruptedResultText, IsError: true}),
			Surface: &SurfaceIntent{Op: SurfaceAppend},
		}); err != nil {
			return err
		}
		s.removePendingCallLocked(id)
	}
	if s.pending.openStep {
		if err := s.closeStepLocked(); err != nil {
			return err
		}
	}
	if s.pending.openTurn {
		if err := s.closeTurnLocked(); err != nil {
			return err
		}
	}
	s.pending = nil
	return nil
}

// pendingHasCallLocked 报告 id 是否在当前未决调用集里（pending 为 nil
// 恒 false）——append 前的成员校验用。
func (s *jsonlSession) pendingHasCallLocked(id string) bool {
	if s.pending == nil {
		return false
	}
	for _, c := range s.pending.pendingCalls {
		if c == id {
			return true
		}
	}
	return false
}

// removePendingCallLocked 从未决集摘除一个 ToolCall（**只在 append 成功
// 后调用**——顺序错了内存会与日志分叉）；不存在即拒绝。
func (s *jsonlSession) removePendingCallLocked(id string) error {
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

// closeStepLocked / closeTurnLocked 闭合悬空的 step/turn（append 成功后
// 才改内存）。
func (s *jsonlSession) closeStepLocked() error {
	if _, err := s.appendLocked(EventDraft{
		Type: EventStepEnded,
		Data: mustJSON(LifecyclePayload{ID: s.pending.stepID, Reason: ReasonInterrupted}),
	}); err != nil {
		return err
	}
	s.pending.openStep = false
	s.pending.stepID = ""
	s.maybeClearPendingLocked()
	return nil
}

func (s *jsonlSession) closeTurnLocked() error {
	if _, err := s.appendLocked(EventDraft{
		Type: EventTurnEnded,
		Data: mustJSON(LifecyclePayload{ID: s.pending.turnID, Reason: ReasonInterrupted}),
	}); err != nil {
		return err
	}
	s.pending.openTurn = false
	s.pending.turnID = ""
	s.maybeClearPendingLocked()
	return nil
}
