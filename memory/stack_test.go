package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/assemble"
	"github.com/Luo-root/pulse/memory/session"
	"github.com/Luo-root/pulse/memory/store"
)

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

func assembleInput(ns []string, query string) assemble.AssembleInput {
	return assemble.AssembleInput{
		Namespace: ns,
		Surface:   []*llm.Message{llm.User(llm.Text(query))},
		Query:     query,
	}
}

// TestSessionStackInjected：最泛化构造——注入任意 SessionStore（含宿主
// 自定义实现）必须开箱可用。
func TestSessionStackInjected(t *testing.T) {
	ctx := context.Background()
	ss := NewSessionStack(session.NewMemoryStore())
	sess, err := ss.Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(ctx, session.EventDraft{
		Type:    session.EventMessageUser,
		Data:    mustRaw(session.MessagePayload{Parts: []llm.Part{llm.Text("hello")}}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}); err != nil {
		t.Fatal(err)
	}
	surface, err := sess.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) != 1 || surface[0].Role != llm.RoleUser || surface[0].Parts[0].Text != "hello" {
		t.Fatalf("surface = %+v", surface)
	}
	if _, _, err := ss.Store().List(ctx, session.SessionFilter{}); err != nil {
		t.Fatalf("store list: %v", err)
	}
}

// TestSessionStackConvenience：便捷封装——内存 / JSONL 两套默认。
func TestSessionStackConvenience(t *testing.T) {
	ctx := context.Background()

	mem := NewMemorySessionStack()
	if _, err := mem.Create(ctx, session.SessionHeader{}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	js, err := NewJSONLSessionStack(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := js.Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	id := sess.Header().SessionID
	sess2, err := js.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if sess2.Header().SessionID != id {
		t.Fatalf("reopened id = %q", sess2.Header().SessionID)
	}
	// Windows TempDir 清理依赖句柄释放：JSONL 会话经类型断言 Close。
	for _, s := range []session.Session{sess, sess2} {
		if c, ok := s.(interface{ Close() error }); ok {
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestItemStackInjected：条目栈最泛化构造——注入 store + meter + budget。
func TestItemStackInjected(t *testing.T) {
	ctx := context.Background()
	ns := store.MemoryScope{TenantID: "t1"}.Namespace()
	st := NewItemStack(store.NewMemoryStore(), nil, assemble.Budget{})
	if st.Store == nil || st.Assembler == nil {
		t.Fatal("stack must wire store and assembler")
	}
	if _, err := st.Store.Put(ctx, store.MemoryItem{
		ID: "d1", Namespace: ns, Kind: store.KindProfile, Content: "Use PostgreSQL for audit logs",
		Status: store.StatusActive, Confidence: 1.0, Taint: store.TaintTrusted,
		SourceRefs: []store.SourceRef{{Type: store.SourceManual, Ref: "seed"}},
	}, store.PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	ac, err := st.Assemble(ctx, assembleInput(ns, "which database for audit logs?"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(func() []string {
		var out []string
		for _, m := range ac.Messages {
			for _, p := range m.Parts {
				out = append(out, p.Text)
			}
		}
		return out
	}(), "\n")
	if !strings.Contains(joined, "PostgreSQL") {
		t.Fatalf("assembled context should recall the stored item, got:\n%s", joined)
	}
}

// TestItemStackConvenience：条目栈便捷封装。
func TestItemStackConvenience(t *testing.T) {
	st := NewMemoryItemStack(assemble.Budget{})
	if st.Store == nil || st.Assembler == nil {
		t.Fatal("convenience stack must be wired")
	}
}
