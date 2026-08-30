package session

import (
	"encoding/json"
	"fmt"

	"github.com/Luo-root/pulse/llm"
)

// Fold 把事件日志折成模型 surface 投影（§6.3 fold 映射表，评审定案）：
//
//	message.user           → llm.RoleUser
//	message.assistant      → llm.RoleAssistant（Parts 原样，含 PartToolCall）
//	tool.called            → log-only，不进 surface
//	tool.result            → llm.RoleTool（IsError + 文本）
//	compaction.checkpoint  → SurfaceReplace：删除 [Start, End] 窗口，插入
//	                         载荷节点集（压缩摘要为单条 RoleUser 稳定前缀
//	                         消息；事件类型不得伪装 message.user）
//	其余已知类型           → 不进 surface（turn/step、chunk、request.*）
//	未知 + Ignorable       → 跳过
//	未知 + 不可跳过        → ErrUnknownRequired（拒绝，fail closed）
//
// fold 只做投影，不修复：unpaired ToolCall 原样折出（修复只发生在 Open
// 的冷恢复路径，且必须写回日志——model-visible means logged）。
// 不含 system 消息（归宿主/Assembler 选择）。
func Fold(events []EventEnvelope, reg *Registry) ([]*llm.Message, error) {
	msgs, _, err := FoldTrace(events, reg)
	return msgs, err
}

// FoldTrace 是 Fold 的带溯源版本：返回与消息一一对应的 source event seq
// ——Append 节点的来源即该事件 Seq；checkpoint 替换出的新节点来源即
// checkpoint 事件 Seq。compaction 编排据此填 `Replaced`（source refs 完整，
// 重放可追溯）。
func FoldTrace(events []EventEnvelope, reg *Registry) ([]*llm.Message, []uint64, error) {
	surface := make([]*llm.Message, 0, len(events))
	sources := make([]uint64, 0, len(events))
	for _, ev := range events {
		entry, known := reg.lookup(ev.Type)
		if !known {
			if ev.Ignorable {
				continue // 裁决表：未知扩展 + Ignorable=true → 跳过
			}
			return nil, nil, fmt.Errorf("%w: seq %d type %q", ErrUnknownRequired, ev.Seq, ev.Type)
		}
		if ev.Surface != nil && !entry.hasSurface {
			return nil, nil, fmt.Errorf("%w: seq %d type %q", ErrSurfaceNotAllowed, ev.Seq, ev.Type)
		}
		switch ev.Type {
		case EventMessageUser, EventMessageAssistant:
			var p MessagePayload
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				return nil, nil, fmt.Errorf("%w: seq %d %s: %v", ErrPayloadInvalid, ev.Seq, ev.Type, err)
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
			sources = append(sources, ev.Seq)
		case EventToolResult:
			var p ToolResultPayload
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				return nil, nil, fmt.Errorf("%w: seq %d tool.result: %v", ErrPayloadInvalid, ev.Seq, err)
			}
			surface = append(surface, &llm.Message{Role: llm.RoleTool, Parts: []llm.Part{
				llm.ResultParts(p.ToolCallID, p.IsError, llm.Text(p.Text)),
			}})
			sources = append(sources, ev.Seq)
		case EventCompactionCheckpoint:
			// §6.4 Surface Replace：坐标是当前 fold 后 surface 的 0-based
			// 消息下标（含端点），不复用 event Seq；范围与 pairing 完整性
			// 由 fold 复核（fail closed）。
			si := ev.Surface
			if si == nil || si.Op != SurfaceReplace {
				return nil, nil, fmt.Errorf("%w: checkpoint seq %d requires SurfaceReplace", ErrSurfaceNotAllowed, ev.Seq)
			}
			if si.Start < 0 || si.End < si.Start || si.End >= len(surface) {
				return nil, nil, fmt.Errorf("%w: checkpoint seq %d range [%d,%d] over surface len %d",
					ErrReplaceRange, ev.Seq, si.Start, si.End, len(surface))
			}
			var p CompactionCheckpointPayload
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				return nil, nil, fmt.Errorf("%w: seq %d compaction.checkpoint: %v", ErrPayloadInvalid, ev.Seq, err)
			}
			replacement := make([]*llm.Message, len(p.Messages))
			for i := range p.Messages {
				msg := p.Messages[i] // 值拷贝：信封不可变，fold 产物独立
				replacement[i] = &msg
			}
			if err := ValidateReplace(surface[:si.Start], surface[si.Start:si.End+1], replacement, surface[si.End+1:]); err != nil {
				return nil, nil, fmt.Errorf("checkpoint seq %d: %w", ev.Seq, err)
			}
			out := make([]*llm.Message, 0, len(surface)-(si.End-si.Start)+len(replacement))
			outSrc := make([]uint64, 0, cap(out))
			out = append(out, surface[:si.Start]...)
			outSrc = append(outSrc, sources[:si.Start]...)
			out = append(out, replacement...)
			for range replacement {
				outSrc = append(outSrc, ev.Seq)
			}
			out = append(out, surface[si.End+1:]...)
			outSrc = append(outSrc, sources[si.End+1:]...)
			surface, sources = out, outSrc
		default:
			// 已知 Ignorable（chunk / request.*）与已知非 surface 的
			// Required（turn/step、tool.called、compaction.status）：fold
			// 不读它。
		}
	}
	return surface, sources, nil
}

// ValidateReplace 校验一次 Surface Replace 不新增 tool pairing 孤儿
// （§9.3：assistant 的 tool_call 与后续 tool_result 必须成对——OpenAI /
// Anthropic 都拒绝孤儿请求）。
//
// 语义是「不新增破坏」而非「窗口内自成整组」：§9.2 的 pruning 替代单个
// result 节点时其 call 在窗口外，但替代节点保留同 ToolCallID，配对仍然
// 成立——合法。四条规则（集合口径，ID 唯一）：
//
//  1. 保留的 call（在 before/after）其 result 落入被删窗口且 replacement
//     不保留该 ID → 违规（call 悬空）；
//  2. 保留的 result 其 call 落入被删窗口且 replacement 不提供 → 违规
//     （result 孤儿）；
//  3. replacement 的 result 在前后文中找不到 call、replacement 也不提供
//     → 违规（新孤儿 result）；
//  4. replacement 的 call 无 result 着落（replacement 与前后文都没有）
//     → 违规（新孤儿 call）。
//
// compaction 编排在 Append 前预检，fold 重放时复核（同一口径）。
func ValidateReplace(before, window, replacement, after []*llm.Message) error {
	delCalls, delResults := collectPairs(window)
	keepCalls, keepResults := mergePairs(before, after)
	repCalls, repResults := collectPairs(replacement)
	for id := range keepCalls {
		if delResults[id] && !repResults[id] {
			return fmt.Errorf("%w: kept tool call %q loses its result to the replaced window", ErrReplaceRange, id)
		}
	}
	for id := range keepResults {
		if delCalls[id] && !repCalls[id] {
			return fmt.Errorf("%w: kept tool result %q loses its call to the replaced window", ErrReplaceRange, id)
		}
	}
	for id := range repResults {
		if !keepCalls[id] && !repCalls[id] {
			return fmt.Errorf("%w: replacement tool result %q has no call", ErrReplaceRange, id)
		}
	}
	for id := range repCalls {
		if !keepResults[id] && !repResults[id] {
			return fmt.Errorf("%w: replacement tool call %q has no result", ErrReplaceRange, id)
		}
	}
	return nil
}

// collectPairs 汇总一组消息的 ToolCallID → result ID 集合。
func collectPairs(msgs []*llm.Message) (calls, results map[string]bool) {
	calls, results = make(map[string]bool), make(map[string]bool)
	for _, m := range msgs {
		if m == nil {
			continue
		}
		if m.Role == llm.RoleAssistant {
			for _, c := range m.ToolCalls() {
				calls[c.ID] = true
			}
		}
		if m.Role == llm.RoleTool {
			for _, p := range m.Parts {
				if p.Kind == llm.PartToolResult && p.ToolResultValue != nil {
					results[p.ToolResultValue.ToolCallID] = true
				}
			}
		}
	}
	return calls, results
}

func mergePairs(groups ...[]*llm.Message) (calls, results map[string]bool) {
	calls, results = make(map[string]bool), make(map[string]bool)
	for _, g := range groups {
		c, r := collectPairs(g)
		for id := range c {
			calls[id] = true
		}
		for id := range r {
			results[id] = true
		}
	}
	return calls, results
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
// compaction 未闭合**不在**这里：恢复不补 `compaction.ended`——未闭合
// 压缩在日志里保持可见，视作失败尝试（§9.1，不假装完成）。
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
