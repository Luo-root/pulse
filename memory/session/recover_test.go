package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Luo-root/pulse/llm"
)

// buildBrokenLog 构造「进程死在工具执行中」的日志：assistant 发出 tool
// call（c1）+ step/turn 悬空，然后直接 Close（不做任何恢复）。
func buildBrokenLog(t *testing.T) (dir string, id string) {
	t.Helper()
	ctx := context.Background()
	dir = t.TempDir()
	st, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.Create(ctx, SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	id = sess.Header().SessionID
	drafts := []EventDraft{
		{
			Type:    EventTurnStarted,
			Data:    mustJSON(LifecyclePayload{ID: "turn-1"}),
			Surface: nil,
		},
		{
			Type:    EventStepStarted,
			Data:    mustJSON(LifecyclePayload{ID: "step-1"}),
			Surface: nil,
		},
		{
			Type:    EventMessageUser,
			Data:    mustJSON(MessagePayload{Parts: []llm.Part{llm.Text("run the tool")}}),
			Surface: &SurfaceIntent{Op: SurfaceAppend},
		},
		{
			Type:    EventMessageAssistant,
			Data:    mustJSON(MessagePayload{Parts: []llm.Part{llm.Call(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)})}}),
			Surface: &SurfaceIntent{Op: SurfaceAppend},
		},
	}
	for _, d := range drafts {
		if _, err := sess.Append(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if c, ok := sess.(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return dir, id
}

// TestRecoverDefaultSynthetic：默认档回归——Open 合成 interrupted 闭环，
// Pending() 恒空。
func TestRecoverDefaultSynthetic(t *testing.T) {
	ctx := context.Background()
	dir, id := buildBrokenLog(t)
	st, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := sess.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	js := sess.(*jsonlSession)
	if js.pending != nil {
		t.Fatal("default policy must not leave pending state")
	}
	if p := js.Pending(); !p.empty() {
		t.Fatalf("default policy Pending = %+v, want empty", p)
	}
	// 合成闭环在位：tool.result(IsError) + step/turn ended。
	surface, err := sess.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last := surface[len(surface)-1]
	if last.Role != llm.RoleTool || last.Parts[0].ToolResultValue.ToolCallID != "c1" || !last.Parts[0].ToolResultValue.IsError {
		t.Fatalf("synthetic result missing, surface tail = %+v", last)
	}
}

// TestRecoverExposePending：未决挂句柄、宿主裁决回到等待点。
func TestRecoverExposePending(t *testing.T) {
	ctx := context.Background()
	dir, id := buildBrokenLog(t)
	st, err := NewJSONLStore(dir, WithRecoverPolicy(RecoverExposePending))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	js := sess.(*jsonlSession)
	t.Cleanup(func() {
		_ = js.ResolveAsInterrupted(ctx)
		if c, ok := sess.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})

	p := js.Pending()
	if len(p.Calls) != 1 || p.Calls[0].ToolCallID != "c1" || p.Calls[0].Seq == 0 {
		t.Fatalf("pending calls = %+v", p.Calls)
	}
	if !p.HasOpenStep || p.OpenStepID != "step-1" || !p.HasOpenTurn || p.OpenTurnID != "turn-1" {
		t.Fatalf("pending lifecycle = %+v", p)
	}

	// 未决期间 Surface 拒绝投影（unpaired tool call 喂给模型是坏请求）；
	// 裁决依据用 Pending() 快照。
	surface, err := sess.Surface(ctx)
	if !errors.Is(err, ErrPendingEvents) {
		t.Fatalf("pending surface err = %v, want ErrPendingEvents", err)
	}
	if surface != nil {
		t.Fatal("pending surface must be nil")
	}

	// 裁决不存在的项 → ErrPendingEvents。
	err = js.ResolvePending(ctx, ResolvePendingOption{
		ToolCallID: "nope",
		Result:     &ToolResultPayload{ToolCallID: "nope", Text: "x"},
	})
	if !errors.Is(err, ErrPendingEvents) {
		t.Fatalf("unknown resolve err = %v", err)
	}

	// 补真实结果（模拟宿主重发/人工补齐）→ 未决减一，Surface 恢复可用。
	if err := js.ResolvePending(ctx, ResolvePendingOption{
		ToolCallID: "c1",
		Result:     &ToolResultPayload{ToolCallID: "c1", Text: "pong:ping"},
	}); err != nil {
		t.Fatal(err)
	}
	if p := js.Pending(); len(p.Calls) != 0 {
		t.Fatalf("calls after resolve = %+v", p.Calls)
	}
	if !js.Pending().HasOpenStep {
		t.Fatal("step must still be open after tool resolution")
	}
	// step/turn 仍悬空 → Surface 仍拒绝（turn 事件族未闭合）。
	if _, err := sess.Surface(ctx); !errors.Is(err, ErrPendingEvents) {
		t.Fatalf("surface with open step/turn err = %v", err)
	}

	// 显式闭合 step/turn。
	if err := js.ResolvePending(ctx, ResolvePendingOption{Interrupted: true}); err != nil {
		t.Fatal(err)
	}
	if err := js.ResolvePending(ctx, ResolvePendingOption{Interrupted: true}); err != nil {
		t.Fatal(err)
	}
	if p := js.Pending(); !p.empty() {
		t.Fatalf("pending after closure = %+v", p)
	}
	if js.pending != nil {
		t.Fatal("pending must be cleared after full resolution")
	}

	// 关闭重开（默认档）：日志合法，无残留未决、无需再合成。
	if err := js.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess2, err := st2.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := sess2.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	surface2, err := sess2.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// user + assistant(tool call) + 补写的 tool result；turn/step 事件是
	// log-only，不进 surface。
	if len(surface2) != 3 {
		var roles []string
		for _, m := range surface2 {
			for _, p := range m.Parts {
				roles = append(roles, string(m.Role)+":"+string(p.Kind)+":"+p.Text)
			}
		}
		t.Fatalf("surface after reopen = %d messages (%v), want 3", len(surface2), roles)
	}
}

// TestRecoverReject：存在未决即拒绝 Open。
func TestRecoverReject(t *testing.T) {
	ctx := context.Background()
	dir, id := buildBrokenLog(t)
	st, err := NewJSONLStore(dir, WithRecoverPolicy(RecoverReject))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Open(ctx, id); !errors.Is(err, ErrPendingEvents) {
		t.Fatalf("open err = %v, want ErrPendingEvents", err)
	}
}

// TestRecoverResolveAsInterrupted：一键默认合成——等价默认档但宿主显式选。
func TestRecoverResolveAsInterrupted(t *testing.T) {
	ctx := context.Background()
	dir, id := buildBrokenLog(t)
	st, err := NewJSONLStore(dir, WithRecoverPolicy(RecoverExposePending))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	js := sess.(*jsonlSession)
	if err := js.ResolveAsInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	if p := js.Pending(); !p.empty() || js.pending != nil {
		t.Fatalf("pending after ResolveAsInterrupted = %+v", p)
	}
	if err := js.ResolveAsInterrupted(ctx); !errors.Is(err, ErrPendingEvents) {
		t.Fatalf("double resolve err = %v", err)
	}
	surface, err := sess.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tail := surface[len(surface)-1]; tail.Role != llm.RoleTool || !tail.Parts[0].ToolResultValue.IsError {
		t.Fatalf("synthetic tail = %+v", tail)
	}
	if c, ok := sess.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}
