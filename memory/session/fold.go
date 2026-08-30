package session

import (
	"encoding/json"
	"fmt"

	"github.com/Luo-root/pulse/llm"
)

// Fold 把事件日志折成模型 surface 投影（§6.3 fold 映射表，评审定案）：
//
//	message.user      → llm.RoleUser
//	message.assistant → llm.RoleAssistant（Parts 原样，含 PartToolCall）
//	tool.called       → log-only，不进 surface
//	tool.result       → llm.RoleTool（IsError + 文本）
//	其余已知类型      → 不进 surface（turn/step 生命周期、chunk、request.*）
//	未知 + Ignorable  → 跳过
//	未知 + 不可跳过   → ErrUnknownRequired（拒绝，fail closed）
//
// fold 只做投影，不修复：unpaired ToolCall 原样折出（修复只发生在 Open
// 的冷恢复路径，且必须写回日志——model-visible means logged）。
// 不含 system 消息（归宿主/Assembler 选择）。
func Fold(events []EventEnvelope, reg *Registry) ([]*llm.Message, error) {
	surface := make([]*llm.Message, 0, len(events))
	for _, ev := range events {
		entry, known := reg.lookup(ev.Type)
		if !known {
			if ev.Ignorable {
				continue // 裁决表：未知扩展 + Ignorable=true → 跳过
			}
			return nil, fmt.Errorf("%w: seq %d type %q", ErrUnknownRequired, ev.Seq, ev.Type)
		}
		if ev.Surface != nil && !entry.hasSurface {
			return nil, fmt.Errorf("%w: seq %d type %q", ErrSurfaceNotAllowed, ev.Seq, ev.Type)
		}
		switch ev.Type {
		case EventMessageUser, EventMessageAssistant:
			var p MessagePayload
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				return nil, fmt.Errorf("%w: seq %d %s: %v", ErrPayloadInvalid, ev.Seq, ev.Type, err)
			}
			parts := p.Parts
			if parts == nil {
				parts = []llm.Part{}
			}
			role := llm.RoleUser
			if ev.Type == EventMessageAssistant {
				role = llm.RoleAssistant
			}
			surface = append(surface, &llm.Message{Role: role, Parts: parts})
		case EventToolResult:
			var p ToolResultPayload
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				return nil, fmt.Errorf("%w: seq %d tool.result: %v", ErrPayloadInvalid, ev.Seq, err)
			}
			surface = append(surface, &llm.Message{Role: llm.RoleTool, Parts: []llm.Part{
				llm.ResultParts(p.ToolCallID, p.IsError, llm.Text(p.Text)),
			}})
		default:
			// 已知 Ignorable（chunk / request.*）与已知非 surface 的
			// Required（turn/step 生命周期、tool.called）：fold 不读它。
		}
	}
	return surface, nil
}

// pendingToolCalls 返回 events 中仍未配对的 ToolCallID（保序）：assistant
// 消息上的 PartToolCall 进队，tool.result 按 ToolCallID 出队。同一 ID 的
// 多余 result 不重复出队。这是配对检测的唯一口径（§6.3：配对主键 =
// assistant 消息上的 PartToolCall；tool.called 不参与）。
func pendingToolCalls(events []EventEnvelope, reg *Registry) ([]string, error) {
	var pending []string
	for _, ev := range events {
		if _, known := reg.lookup(ev.Type); !known && !ev.Ignorable {
			return nil, fmt.Errorf("%w: seq %d type %q", ErrUnknownRequired, ev.Seq, ev.Type)
		}
		switch ev.Type {
		case EventMessageAssistant:
			var p MessagePayload
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				return nil, fmt.Errorf("%w: seq %d message.assistant: %v", ErrPayloadInvalid, ev.Seq, err)
			}
			for _, part := range p.Parts {
				if part.Kind == llm.PartToolCall && part.ToolCallValue != nil {
					pending = append(pending, part.ToolCallValue.ID)
				}
			}
		case EventToolResult:
			var p ToolResultPayload
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				return nil, fmt.Errorf("%w: seq %d tool.result: %v", ErrPayloadInvalid, ev.Seq, err)
			}
			for i, id := range pending {
				if id == p.ToolCallID {
					pending = append(pending[:i], pending[i+1:]...)
					break
				}
			}
		}
	}
	return pending, nil
}

// incompleteState 是一次扫描得到的未闭合现场（§9.3 崩溃恢复表）。
type incompleteState struct {
	openTurn bool // 最后一个 turn.started 未闭合
	openStep bool // 最后一个 step.started 未闭合
	// turnID/stepID 是对应 started 事件的 ID：合成 ended 时必须回带，
	// 保证 started/ended 配对可追溯。
	turnID string
	stepID string
	// pendingCalls 是缺 result 的 ToolCallID，保序；并行部分回来时缺的
	// 全补（§9.3）。
	pendingCalls []string
}

// scanIncomplete 扫描日志得到未闭合现场。turn/step 的开闭按最后一条
// started/ended 计：ended 之前允许新一轮 started 覆盖（正常流不会这样写，
// 但恢复判定只关心「日志结尾处是否悬空」）。
func scanIncomplete(events []EventEnvelope, reg *Registry) (incompleteState, error) {
	st := incompleteState{}
	var err error
	st.pendingCalls, err = pendingToolCalls(events, reg)
	if err != nil {
		return st, err
	}
	for _, ev := range events {
		if _, known := reg.lookup(ev.Type); !known && !ev.Ignorable {
			return st, fmt.Errorf("%w: seq %d type %q", ErrUnknownRequired, ev.Seq, ev.Type)
		}
		switch ev.Type {
		case EventTurnStarted:
			var p LifecyclePayload
			_ = json.Unmarshal(ev.Data, &p) // Append 已过 codec；白盒构造容忍缺 payload
			st.openTurn = true
			st.turnID = p.ID
		case EventTurnEnded:
			st.openTurn = false
			st.turnID = ""
		case EventStepStarted:
			var p LifecyclePayload
			_ = json.Unmarshal(ev.Data, &p)
			st.openStep = true
			st.stepID = p.ID
		case EventStepEnded:
			st.openStep = false
			st.stepID = ""
		}
	}
	return st, nil
}

// synthDrafts 把未闭合现场转成待补写的合成事件序列（A1 内存恢复与 A2
// JSONL 恢复共用）。顺序：先配对（result 回填在 assistant 消息之后），
// 再 step、再 turn——与嵌套闭合顺序一致。合成 ended 回带 started 的 ID。
func synthDrafts(st incompleteState) []EventDraft {
	drafts := make([]EventDraft, 0, len(st.pendingCalls)+2)
	for _, id := range st.pendingCalls {
		drafts = append(drafts, EventDraft{
			Type:    EventToolResult,
			Data:    mustJSON(ToolResultPayload{ToolCallID: id, Text: interruptedResultText, IsError: true}),
			Surface: &SurfaceIntent{Op: SurfaceAppend},
		})
	}
	if st.openStep {
		drafts = append(drafts, EventDraft{
			Type: EventStepEnded,
			Data: mustJSON(LifecyclePayload{ID: st.stepID, Reason: ReasonInterrupted}),
		})
	}
	if st.openTurn {
		drafts = append(drafts, EventDraft{
			Type: EventTurnEnded,
			Data: mustJSON(LifecyclePayload{ID: st.turnID, Reason: ReasonInterrupted}),
		})
	}
	return drafts
}
