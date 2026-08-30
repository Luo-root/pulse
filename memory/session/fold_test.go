package session

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Luo-root/pulse/llm"
)

// userDraft / assistantDraft / toolResultDraft 等构造器统一放这里，供各
// 测试文件复用。

func mustJSONPayload(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return data
}

func userDraft(t *testing.T, text string) EventDraft {
	t.Helper()
	return EventDraft{
		Type:    EventMessageUser,
		Data:    mustJSONPayload(t, MessagePayload{Parts: []llm.Part{llm.Text(text)}}),
		Surface: &SurfaceIntent{Op: SurfaceAppend},
	}
}

func assistantDraft(t *testing.T, parts ...llm.Part) EventDraft {
	t.Helper()
	return EventDraft{
		Type:    EventMessageAssistant,
		Data:    mustJSONPayload(t, MessagePayload{Parts: parts}),
		Surface: &SurfaceIntent{Op: SurfaceAppend},
	}
}

func toolResultDraft(t *testing.T, callID, text string, isError bool) EventDraft {
	t.Helper()
	return EventDraft{
		Type:    EventToolResult,
		Data:    mustJSONPayload(t, ToolResultPayload{ToolCallID: callID, Text: text, IsError: isError}),
		Surface: &SurfaceIntent{Op: SurfaceAppend},
	}
}

func toolCalledDraft(t *testing.T, callID, name string) EventDraft {
	t.Helper()
	return EventDraft{
		Type: EventToolCalled,
		Data: mustJSONPayload(t, ToolCalledPayload{ToolCallID: callID, Name: name, Arguments: json.RawMessage(`{"x":1}`)}),
	}
}

func lifecycleDraft(t *testing.T, typ EventType, id, reason string) EventDraft {
	t.Helper()
	return EventDraft{Type: typ, Data: mustJSONPayload(t, LifecyclePayload{ID: id, Reason: reason})}
}

// TestFoldMappingTable 是 §6.3 fold 映射表的全量验收：一张表钉死每个事件
// 族在 surface 上的形态。
func TestFoldMappingTable(t *testing.T) {
	store := NewMemoryStore()
	sess, err := store.Create(t.Context(), SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	appends := []EventDraft{
		lifecycleDraft(t, EventTurnStarted, "turn-1", ""),
		lifecycleDraft(t, EventStepStarted, "step-1", ""),
		{Type: EventRequestHeader, Data: mustJSONPayload(t, RequestHeaderPayload{Model: "gpt-test"})},
		{Type: EventRequestRoute, Data: mustJSONPayload(t, RequestRoutePayload{Model: "gpt-test"})},
		userDraft(t, "hi"),
		{Type: EventAssistantChunk, Data: mustJSONPayload(t, ChunkPayload{Text: "par"})},
		assistantDraft(t,
			llm.Reasoning("thinking"),
			llm.Call(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)}),
			llm.Text("let me check"),
		),
		toolCalledDraft(t, "c1", "lookup"),
		toolResultDraft(t, "c1", "found it", false),
		assistantDraft(t, llm.Text("done")),
		lifecycleDraft(t, EventStepEnded, "step-1", ReasonCompleted),
		lifecycleDraft(t, EventTurnEnded, "turn-1", ReasonCompleted),
	}
	for _, d := range appends {
		if _, err := sess.Append(t.Context(), d); err != nil {
			t.Fatalf("append %s: %v", d.Type, err)
		}
	}

	surface, err := sess.Surface(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 只有 message.user / message.assistant / tool.result 进 surface。
	wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleTool, llm.RoleAssistant}
	if len(surface) != len(wantRoles) {
		t.Fatalf("surface len = %d, want %d; roles = %v", len(surface), len(wantRoles), surface)
	}
	for i, role := range wantRoles {
		if surface[i].Role != role {
			t.Errorf("surface[%d].Role = %q, want %q", i, surface[i].Role, role)
		}
	}
	if got := surface[0].Text(); got != "hi" {
		t.Errorf("user text = %q", got)
	}
	// assistant Parts 原样：PartReasoning + PartToolCall + PartText 顺序与内容不变。
	first := surface[1]
	if len(first.Parts) != 3 {
		t.Fatalf("assistant parts = %d, want 3", len(first.Parts))
	}
	if first.Parts[0].Kind != llm.PartReasoning || first.Parts[0].Text != "thinking" {
		t.Errorf("reasoning part = %+v", first.Parts[0])
	}
	call := first.Parts[1]
	if call.Kind != llm.PartToolCall || call.ToolCallValue == nil ||
		call.ToolCallValue.ID != "c1" || call.ToolCallValue.Name != "lookup" {
		t.Errorf("tool call part = %+v", call)
	}
	if first.Parts[2].Text != "let me check" {
		t.Errorf("text part = %+v", first.Parts[2])
	}
	// tool.result → RoleTool（IsError + 文本，对齐 loop toolResultMsg 形态）。
	res := surface[2].Parts[0]
	if res.Kind != llm.PartToolResult || res.ToolResultValue == nil {
		t.Fatalf("tool result part = %+v", res)
	}
	if res.ToolResultValue.ToolCallID != "c1" || res.ToolResultValue.IsError {
		t.Errorf("tool result value = %+v", res.ToolResultValue)
	}
	if res.ToolResultValue.Content[0].Text != "found it" {
		t.Errorf("tool result text = %+v", res.ToolResultValue.Content[0])
	}
}

// TestFoldUnknownRequired：未知类型且不可跳过 → 拒绝（fail closed）。
func TestFoldUnknownRequired(t *testing.T) {
	reg := NewRegistry()
	events := []EventEnvelope{
		{Seq: 1, Type: EventType("plugin/x/something"), Ignorable: false},
	}
	if _, err := Fold(events, reg); !errors.Is(err, ErrUnknownRequired) {
		t.Fatalf("err = %v, want ErrUnknownRequired", err)
	}
}

// TestFoldUnknownIgnorable：未知扩展 + Ignorable=true → 跳过。
func TestFoldUnknownIgnorable(t *testing.T) {
	reg := NewRegistry()
	events := []EventEnvelope{
		{Seq: 1, Type: EventMessageUser, Data: mustJSONPayload(t, MessagePayload{Parts: []llm.Part{llm.Text("hi")}})},
		{Seq: 2, Type: EventType("plugin/x/note"), Ignorable: true},
	}
	surface, err := Fold(events, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) != 1 || surface[0].Text() != "hi" {
		t.Fatalf("surface = %v", surface)
	}
}

// TestFoldSurfaceMismatch：非 surface 类型携带 SurfaceIntent → 拒绝。
func TestFoldSurfaceMismatch(t *testing.T) {
	reg := NewRegistry()
	events := []EventEnvelope{
		{Seq: 1, Type: EventToolCalled, Surface: &SurfaceIntent{Op: SurfaceAppend}},
	}
	if _, err := Fold(events, reg); !errors.Is(err, ErrSurfaceNotAllowed) {
		t.Fatalf("err = %v, want ErrSurfaceNotAllowed", err)
	}
}

// TestPendingToolCalls 覆盖配对口径：主键是 assistant 消息上的 PartToolCall；
// tool.called 不参与；并行部分回来时缺的保序可检出。
func TestPendingToolCalls(t *testing.T) {
	reg := NewRegistry()
	events := []EventEnvelope{
		{Seq: 1, Type: EventMessageAssistant, Data: mustJSONPayload(t, MessagePayload{Parts: []llm.Part{
			llm.Call(llm.ToolCall{ID: "c1"}),
			llm.Call(llm.ToolCall{ID: "c2"}),
			llm.Call(llm.ToolCall{ID: "c3"}),
		}})},
		{Seq: 2, Type: EventToolCalled, Data: mustJSONPayload(t, ToolCalledPayload{ToolCallID: "c9", Name: "audit-only"})},
		{Seq: 3, Type: EventToolResult, Data: mustJSONPayload(t, ToolResultPayload{ToolCallID: "c2"})},
	}
	pending, err := pendingToolCalls(events, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0] != "c1" || pending[1] != "c3" {
		t.Fatalf("pending = %v, want [c1 c3]（tool.called 不参与配对）", pending)
	}
}
