package session

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Luo-root/pulse/llm"
)

func newTestSession(t *testing.T) *memSession {
	t.Helper()
	sess, err := NewMemoryStore().Create(t.Context(), SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	return sess.(*memSession)
}

// TestAppendAssignsSeqAndTime：Seq 由 store 严格连续分配、Time 非空、
// Append 非幂等（同一 draft 重放产生两个事件）。
func TestAppendAssignsSeqAndTime(t *testing.T) {
	sess := newTestSession(t)
	draft := userDraft(t, "hi")
	first, err := sess.Append(t.Context(), draft)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sess.Append(t.Context(), draft) // 非幂等：重放 = 双份
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("seq = %d, %d; want 1, 2", first.Seq, second.Seq)
	}
	if first.Time.IsZero() || second.Time.IsZero() {
		t.Fatal("Time must be assigned by store")
	}
	events, err := sess.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2（重放不合并）", len(events))
	}
	// Events(fromSeq) 含 fromSeq 本身。
	tail, err := sess.Events(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].Seq != 2 {
		t.Fatalf("tail = %v, want [seq 2]", tail)
	}
}

// TestAppendUnknownEventFailClosed：未注册类型必须显式标 Ignorable 才能写入。
func TestAppendUnknownEventFailClosed(t *testing.T) {
	sess := newTestSession(t)
	_, err := sess.Append(t.Context(), EventDraft{Type: EventType("plugin/x/note"), Data: []byte(`{}`)})
	if !errors.Is(err, ErrUnknownEvent) {
		t.Fatalf("err = %v, want ErrUnknownEvent", err)
	}
	env, err := sess.Append(t.Context(), EventDraft{Type: EventType("plugin/x/note"), Data: []byte(`{}`), Ignorable: true})
	if err != nil {
		t.Fatal(err)
	}
	if !env.Ignorable {
		t.Fatal("unknown ignorable envelope must carry Ignorable=true")
	}
	// fold 跳过它。
	surface, err := sess.Surface(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) != 0 {
		t.Fatalf("surface = %v, want empty", surface)
	}
}

// TestAppendClassificationWins：已知类型的信封分级以注册表为准，忽略
// draft 上的 flag——已知 Required 永不降级；已知 Ignorable 不因漏标 flag
// 而变成 required。
func TestAppendClassificationWins(t *testing.T) {
	sess := newTestSession(t)
	req, err := sess.Append(t.Context(), EventDraft{Type: EventMessageUser, Data: userDraft(t, "x").Data, Ignorable: true})
	if err != nil {
		t.Fatal(err)
	}
	if req.Ignorable {
		t.Fatal("known Required must never be marked Ignorable（忽略信封 flag）")
	}
	ign, err := sess.Append(t.Context(), EventDraft{Type: EventAssistantChunk, Data: mustJSONPayload(t, ChunkPayload{Text: "c"})})
	if err != nil {
		t.Fatal(err)
	}
	if ign.Ignorable != true {
		t.Fatal("known Ignorable must be marked Ignorable regardless of draft flag")
	}
}

// TestAppendSurfaceGuards：surface 许可与 Replace 坐标校验（§6.4）。
func TestAppendSurfaceGuards(t *testing.T) {
	sess := newTestSession(t)
	ctx := t.Context()
	// 非 surface 类型带 SurfaceIntent。
	_, err := sess.Append(ctx, EventDraft{Type: EventToolCalled, Data: toolCalledDraft(t, "c", "n").Data, Surface: &SurfaceIntent{Op: SurfaceAppend}})
	if !errors.Is(err, ErrSurfaceNotAllowed) {
		t.Fatalf("err = %v, want ErrSurfaceNotAllowed", err)
	}
	// 本阶段没有注册 Replace 事件类型（compaction.checkpoint 在 P2-B）。
	_, err = sess.Append(ctx, EventDraft{Type: EventMessageUser, Data: userDraft(t, "x").Data, Surface: &SurfaceIntent{Op: SurfaceReplace, Start: 0, End: 0}})
	if !errors.Is(err, ErrReplaceNotSupported) {
		t.Fatalf("err = %v, want ErrReplaceNotSupported", err)
	}
	// Start > End 反向 → ErrReplaceRange。
	_, err = sess.Append(ctx, EventDraft{Type: EventMessageUser, Data: userDraft(t, "x").Data, Surface: &SurfaceIntent{Op: SurfaceAppend, Start: 2, End: 1}})
	if !errors.Is(err, ErrReplaceRange) {
		t.Fatalf("err = %v, want ErrReplaceRange", err)
	}
	// 非法 Op。
	_, err = sess.Append(ctx, EventDraft{Type: EventMessageUser, Data: userDraft(t, "x").Data, Surface: &SurfaceIntent{Op: "upsert"}})
	if !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("err = %v, want ErrPayloadInvalid", err)
	}
}

// TestAppendPayloadValidation：codec 在入库前校验 payload（§6.2）。
func TestAppendPayloadValidation(t *testing.T) {
	sess := newTestSession(t)
	ctx := t.Context()
	cases := []struct {
		name  string
		draft EventDraft
	}{
		{name: "message.user 缺 payload", draft: EventDraft{Type: EventMessageUser, Surface: &SurfaceIntent{Op: SurfaceAppend}}},
		{name: "tool.result 缺 toolCallID", draft: EventDraft{Type: EventToolResult, Data: mustJSONPayload(t, ToolResultPayload{Text: "x"}), Surface: &SurfaceIntent{Op: SurfaceAppend}}},
		{name: "request.header 缺 model", draft: EventDraft{Type: EventRequestHeader, Data: mustJSONPayload(t, RequestHeaderPayload{})}},
		{name: "tool.called 缺 name", draft: EventDraft{Type: EventToolCalled, Data: mustJSONPayload(t, ToolCalledPayload{ToolCallID: "c"})}},
		{name: "tool.called arguments 非法 JSON", draft: EventDraft{Type: EventToolCalled, Data: json.RawMessage(`{"toolCallID":"c","name":"n","arguments":{bad}`)}},
	}
	for _, tc := range cases {
		if _, err := sess.Append(ctx, tc.draft); !errors.Is(err, ErrPayloadInvalid) {
			t.Errorf("%s: err = %v, want ErrPayloadInvalid", tc.name, err)
		}
	}
	events, _ := sess.Events(ctx, 0)
	if len(events) != 0 {
		t.Fatalf("failed appends must not persist; got %d", len(events))
	}
}

// TestForkBasic：seed 是父日志前缀拷贝；header 承载 fork 血缘；子会话
// 注册进同一 store；父后续追加不污染子 seed（§13.1 fork tests）。
func TestForkBasic(t *testing.T) {
	store := NewMemoryStore()
	parent, err := store.Create(t.Context(), SessionHeader{AgentID: "a1", Workspace: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.Append(t.Context(), userDraft(t, "q")); err != nil {
		t.Fatal(err)
	}
	if _, err := parent.Append(t.Context(), assistantDraft(t, llm.Text("a"))); err != nil {
		t.Fatal(err)
	}
	child, err := parent.Fork(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	childHdr := child.Header()
	if childHdr.ParentSessionID != parent.Header().SessionID {
		t.Errorf("child.ParentSessionID = %q", childHdr.ParentSessionID)
	}
	if childHdr.SeedLength != 2 {
		t.Errorf("child.SeedLength = %d, want 2", childHdr.SeedLength)
	}
	if childHdr.DelegationDepth != 1 {
		t.Errorf("child.DelegationDepth = %d, want 1", childHdr.DelegationDepth)
	}
	if childHdr.AgentID != "a1" || childHdr.Workspace != "w1" {
		t.Errorf("child header not inherited: %+v", childHdr)
	}
	// 父再追加，子 seed 不变。
	if _, err := parent.Append(t.Context(), userDraft(t, "after-fork")); err != nil {
		t.Fatal(err)
	}
	childEvents, err := child.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(childEvents) != 2 {
		t.Fatalf("child events = %d, want 2（父后续事件不可污染子 seed）", len(childEvents))
	}
	// 子会话已注册进 store，可 Open。
	if _, err := store.Open(t.Context(), childHdr.SessionID); err != nil {
		t.Fatalf("open child: %v", err)
	}
}

// TestForkRejectSplitToolGroup：切点落在 tool 组中间 → 拒绝（§9.3）。
func TestForkRejectSplitToolGroup(t *testing.T) {
	sess := newTestSession(t)
	ctx := t.Context()
	parts := []llm.Part{
		llm.Call(llm.ToolCall{ID: "c1"}),
		llm.Call(llm.ToolCall{ID: "c2"}),
	}
	if _, err := sess.Append(ctx, assistantDraft(t, parts...)); err != nil {
		t.Fatal(err)
	}
	// 一个 result 已回、一个未回：切在组内仍拒绝。
	if _, err := sess.Append(ctx, toolResultDraft(t, "c1", "ok", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Fork(ctx, 2); !errors.Is(err, ErrForkSplitToolGroup) {
		t.Fatalf("err = %v, want ErrForkSplitToolGroup", err)
	}
	// result 补齐后同一切点合法。
	if _, err := sess.Append(ctx, toolResultDraft(t, "c2", "ok", false)); err != nil {
		t.Fatal(err)
	}
	child, err := sess.Fork(ctx, 3)
	if err != nil {
		t.Fatalf("fork after pairing: %v", err)
	}
	if child.Header().SeedLength != 3 {
		t.Fatalf("SeedLength = %d", child.Header().SeedLength)
	}
	// 越界切点。
	if _, err := sess.Fork(ctx, 0); !errors.Is(err, ErrForkBadAt) {
		t.Fatalf("err = %v, want ErrForkBadAt", err)
	}
	if _, err := sess.Fork(ctx, 99); !errors.Is(err, ErrForkBadAt) {
		t.Fatalf("err = %v, want ErrForkBadAt", err)
	}
}

// TestDeleteFailClosed：Delete 后 Open → NotFound；已持实例写入/Flush → ErrDeleted。
func TestDeleteFailClosed(t *testing.T) {
	store := NewMemoryStore()
	sess, err := store.Create(t.Context(), SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	id := sess.Header().SessionID
	if err := store.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(t.Context(), id); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("open after delete: %v, want ErrSessionNotFound", err)
	}
	if _, err := sess.Append(t.Context(), userDraft(t, "x")); !errors.Is(err, ErrDeleted) {
		t.Fatalf("append after delete: %v, want ErrDeleted", err)
	}
	if err := sess.Flush(t.Context()); !errors.Is(err, ErrDeleted) {
		t.Fatalf("flush after delete: %v, want ErrDeleted", err)
	}
	if err := store.Delete(t.Context(), id); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("double delete: %v, want ErrSessionNotFound", err)
	}
}

// TestFlushNoOp：A1 内存实现的 Flush 是成功空操作（§7.1）。
func TestFlushNoOp(t *testing.T) {
	sess := newTestSession(t)
	if err := sess.Flush(t.Context()); err != nil {
		t.Fatalf("flush = %v, want nil", err)
	}
}

// TestRegistryExtension：插件扩展事件类型；重复注册拒绝。
func TestRegistryExtension(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(EventMessageUser, ClassIgnorable, false, nil); !errors.Is(err, ErrEventRegistered) {
		t.Fatalf("overwrite built-in: %v, want ErrEventRegistered", err)
	}
	typ := EventType("plugin/demo/heartbeat")
	if err := reg.Register(typ, ClassIgnorable, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(typ, ClassIgnorable, false, nil); !errors.Is(err, ErrEventRegistered) {
		t.Fatalf("duplicate register: %v, want ErrEventRegistered", err)
	}
	store := NewMemoryStore(WithRegistry(reg))
	sess, err := store.Create(t.Context(), SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(t.Context(), EventDraft{Type: typ, Data: []byte(`{}`), Ignorable: true}); err != nil {
		t.Fatalf("append registered extension: %v", err)
	}
}
