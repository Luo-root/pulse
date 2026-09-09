package session_test

// 会话导出/导入往返测试（#152）：Seq/Time/Type/Data/Surface/Ignorable
// 逐字段保真；blob 内联与重建；版本闸门与 fail-closed 校验。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/compaction"
	"github.com/Luo-root/pulse/memory/session"
)

func mustJSONP(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func expDraftUser(text string) session.EventDraft {
	return session.EventDraft{
		Type:    session.EventMessageUser,
		Data:    mustJSONP(session.MessagePayload{Parts: []llm.Part{llm.Text(text)}}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

func expDraftAssistant(parts ...llm.Part) session.EventDraft {
	return session.EventDraft{
		Type:    session.EventMessageAssistant,
		Data:    mustJSONP(session.MessagePayload{Parts: parts}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

func expDraftToolResult(callID, text string) session.EventDraft {
	return session.EventDraft{
		Type:    session.EventToolResult,
		Data:    mustJSONP(session.ToolResultPayload{ToolCallID: callID, Text: text}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

func appendAll(t *testing.T, sess session.Session, drafts ...session.EventDraft) {
	t.Helper()
	for i, d := range drafts {
		if _, err := sess.Append(context.Background(), d); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// assertEnvelopesEqual 逐字段比较两份信封（Time 用 SameInstant 口径）。
func assertEnvelopesEqual(t *testing.T, want, got []session.EventEnvelope) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("envelopes = %d, want %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if g.Seq != w.Seq || g.Type != w.Type || g.Ignorable != w.Ignorable {
			t.Fatalf("env %d header mismatch: got %+v want %+v", i, g, w)
		}
		if !g.Time.Equal(w.Time) {
			t.Fatalf("env %d time: got %v want %v", i, g.Time, w.Time)
		}
		if string(g.Data) != string(w.Data) {
			t.Fatalf("env %d data:\n got %s\nwant %s", i, g.Data, w.Data)
		}
		if (g.Surface == nil) != (w.Surface == nil) {
			t.Fatalf("env %d surface nil mismatch", i)
		}
		if g.Surface != nil && w.Surface != nil && !reflect.DeepEqual(g.Surface, w.Surface) {
			t.Fatalf("env %d surface: got %+v want %+v", i, *g.Surface, *w.Surface)
		}
	}
}

func TestSessionExportImportRoundTripJSONL(t *testing.T) {
	ctx := context.Background()
	srcStore, err := session.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src, err := srcStore.Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	// 一轮完整 tool 调用 + 一条超 32KiB 的图片（触发 blob 落盘）。
	appendAll(t, src,
		expDraftUser("find the config"),
		expDraftAssistant(llm.Call(llm.ToolCall{ID: "c1", Name: "lookup"})),
		expDraftToolResult("c1", "config found at /etc/app.yaml"),
		expDraftAssistant(llm.Text("done")),
		expDraftUser("here is the chart"),
		session.EventDraft{
			Type: session.EventMessageUser,
			Data: mustJSONP(session.MessagePayload{Parts: []llm.Part{{
				Kind:  llm.PartImage,
				Image: &llm.ImageSource{Data: bytes.Repeat([]byte{0xAB}, 40000), MediaType: "image/png"},
			}}}),
			Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
		},
	)
	if err := src.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := session.ExportSession(ctx, src, &stream); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stream.String(), `"blob:`) {
		t.Fatal("export must inline blob bytes (no blob: refs in the stream)")
	}

	dstStore, err := session.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dst, err := session.ImportSession(ctx, dstStore, bytes.NewReader(stream.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	// header 保真：SessionID 原样。
	if dst.Header().SessionID != src.Header().SessionID {
		t.Fatalf("session id: got %q want %q", dst.Header().SessionID, src.Header().SessionID)
	}
	// blob 重建验证：Close 后 Open（Open 走 decodeBlobs，引用缺失即失败），
	// surface 的图片字节与原图一致。
	if c, ok := dst.(interface{ Close() error }); !ok {
		t.Fatal("JSONL session must implement Close")
	} else if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := dstStore.Open(ctx, src.Header().SessionID)
	if err != nil {
		t.Fatal(err)
	}
	surface, err := reopened.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var img *llm.ImageSource
	for _, m := range surface {
		for _, p := range m.Parts {
			if p.Kind == llm.PartImage {
				img = p.Image
			}
		}
	}
	if img == nil || len(img.Data) != 40000 || !bytes.Contains(img.Data, []byte{0xAB, 0xAB}) {
		t.Fatalf("imported image part lost bytes: %+v", img)
	}

	// 幂等：重开的会话再导出，字节流一致（Seq/Time/Type/Data/Surface/
	// Ignorable 的全部保真由字节级比较一次覆盖）。
	var again bytes.Buffer
	if err := session.ExportSession(ctx, reopened, &again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stream.Bytes(), again.Bytes()) {
		t.Fatal("re-export must be byte-identical (round-trip idempotent)")
	}
	// 释放文件句柄（TempDir 清理需要）。
	for _, s := range []session.Session{src, reopened} {
		if c, ok := s.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}
}

func TestSessionExportImportMemoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	srcStore := session.NewMemoryStore()
	src, err := srcStore.Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, src,
		expDraftUser("hello"),
		expDraftAssistant(llm.Text("hi")),
	)
	want, err := src.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := session.ExportSession(ctx, src, &stream); err != nil {
		t.Fatal(err)
	}
	dst, err := session.ImportSession(ctx, session.NewMemoryStore(), bytes.NewReader(stream.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := dst.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertEnvelopesEqual(t, want, got)
}

func TestSessionImportCompactedVersionPreserved(t *testing.T) {
	ctx := context.Background()
	srcStore, err := session.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src, err := srcStore.Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, src,
		expDraftUser("q1"),
		expDraftAssistant(llm.Text("a1")),
		expDraftUser("q2"),
		expDraftAssistant(llm.Text("a2")),
	)
	if _, err := compaction.Compact(ctx, src, compaction.Options{
		Engine: &compaction.DeterministicSummarizer{}, Meter: compaction.CharMeter{}, ModelName: "deterministic",
	}); err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := session.ExportSession(ctx, src, &stream); err != nil {
		t.Fatal(err)
	}
	dst, err := session.ImportSession(ctx, session.NewMemoryStore(), bytes.NewReader(stream.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if dst.Header().FormatVersion != src.Header().FormatVersion {
		t.Fatalf("format version: got %d want %d", dst.Header().FormatVersion, src.Header().FormatVersion)
	}
	if dst.Header().FormatVersion != session.CompactedVersion {
		t.Fatalf("compacted session must stay CompactedVersion, got %d", dst.Header().FormatVersion)
	}
	if c, ok := src.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}

func TestSessionImportRejects(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	// 名义上合法的 header + 三种非法 seed。
	header := mustJSONP(session.SessionHeader{FormatVersion: session.FormatVersion, SessionID: "bad-1"})
	cases := []struct {
		name    string
		lines   []string
		wantErr error
	}{
		{
			name:  "seq gap",
			lines: []string{`{"seq":1,"type":"message.user"}`, `{"seq":3,"type":"message.user"}`},
		},
		{
			name:  "unknown required",
			lines: []string{`{"seq":1,"type":"x.custom"}`},
		},
		{
			name: "unclosed tool call",
			lines: []string{
				`{"seq":1,"type":"message.user","data":{"parts":[{"kind":"text","text":"q"}]}}`,
				`{"seq":2,"type":"message.assistant","data":{"parts":[{"kind":"tool_call","toolCallValue":{"id":"c1","name":"lookup"}}]}}`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := strings.Join(append([]string{string(header)}, tc.lines...), "\n")
			_, err := session.ImportSession(ctx, store, strings.NewReader(stream))
			if err == nil {
				t.Fatal("import must fail closed")
			}
			t.Logf("rejected: %v", err)
		})
	}
}

func TestSessionImportSeedUnsupported(t *testing.T) {
	ctx := context.Background()
	// 剥离 Seeder 能力的包装 store：SessionStore 接口满足，Seeder 不满足。
	type noSeed struct{ session.SessionStore }
	src, err := session.NewMemoryStore().Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := session.ExportSession(ctx, src, &stream); err != nil {
		t.Fatal(err)
	}
	_, err = session.ImportSession(ctx, noSeed{session.NewMemoryStore()}, bytes.NewReader(stream.Bytes()))
	if !errors.Is(err, session.ErrSeedUnsupported) {
		t.Fatalf("want ErrSeedUnsupported, got %v", err)
	}
}
