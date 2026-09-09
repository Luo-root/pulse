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

func TestSessionStackMemory(t *testing.T) {
	ctx := context.Background()
	ss, err := NewSessionStack(SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
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
	// Store() 暴露完整接口面（List 可用）。
	if _, _, err := ss.Store().List(ctx, session.SessionFilter{}); err != nil {
		t.Fatalf("store list: %v", err)
	}
}

func TestSessionStackJSONL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ss, err := NewSessionStack(SessionOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := ss.Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	id := sess.Header().SessionID
	if id == "" {
		t.Fatal("session id must be assigned")
	}

	// 重开验证落盘（JSONL store 的 live 会话表按 id 去重，直接 Open 同 id）。
	sess2, err := ss.Open(ctx, id)
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

func TestItemStackAssemble(t *testing.T) {
	ctx := context.Background()
	st := NewItemStack(ItemOptions{})
	if st.Store == nil || st.Assembler == nil {
		t.Fatal("stack must wire store and assembler")
	}
	ns := store.MemoryScope{TenantID: "t1"}.Namespace()
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
