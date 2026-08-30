package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/session"
)

// ---- 构造器 ----

func draftUser(t *testing.T, text string) session.EventDraft {
	t.Helper()
	return session.EventDraft{
		Type:    session.EventMessageUser,
		Data:    mustJSON(session.MessagePayload{Parts: []llm.Part{llm.Text(text)}}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

func draftAssistant(t *testing.T, parts ...llm.Part) session.EventDraft {
	t.Helper()
	return session.EventDraft{
		Type:    session.EventMessageAssistant,
		Data:    mustJSON(session.MessagePayload{Parts: parts}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

func draftToolResult(t *testing.T, callID, text string) session.EventDraft {
	t.Helper()
	return session.EventDraft{
		Type:    session.EventToolResult,
		Data:    mustJSON(session.ToolResultPayload{ToolCallID: callID, Text: text}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

// seedTurn 落一个完整的 tool turn：user → assistant(call) → tool → assistant。
// 返回 4 个 surface 节点。
func seedTurn(t *testing.T, ctx context.Context, sess session.Session) {
	t.Helper()
	appends := []session.EventDraft{
		draftUser(t, "find the config"),
		draftAssistant(t, llm.Call(llm.ToolCall{ID: "c1", Name: "lookup"})),
		draftToolResult(t, "c1", "config found at /etc/app.yaml"),
		draftAssistant(t, llm.Text("done, config loaded")),
	}
	for _, d := range appends {
		if _, err := sess.Append(ctx, d); err != nil {
			t.Fatalf("seed %s: %v", d.Type, err)
		}
	}
}

type failingEngine struct{}

func (failingEngine) Summarize(context.Context, SummarizeInput) (SummarizeResult, error) {
	return SummarizeResult{}, errors.New("summarizer offline")
}

type fakeChatModel struct{ text string }

func (f *fakeChatModel) Generate(context.Context, *llm.GenerateRequest) (*llm.Response, error) {
	return &llm.Response{
		Message: llm.AssistantText(f.text),
		Usage:   llm.TokenUsage{InputTokens: 120, OutputTokens: 30},
	}, nil
}

func (f *fakeChatModel) Stream(context.Context, *llm.GenerateRequest) (<-chan llm.StreamEvent, error) {
	return nil, errors.New("stream not implemented")
}

// closeJSONL 释放文件版会话（Close 在实现类型上，不在 Session 接口）。
func closeJSONL(t *testing.T, s session.Session) {
	t.Helper()
	c, ok := s.(interface{ Close() error })
	if !ok {
		t.Fatal("jsonl session must implement Close")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

// ---- 事务 ----

// TestCompactTransaction：八步事务全流程——raw log 只增不减、surface
// 反映 Replace、Replaced 完整、header 版本抬到 CompactedVersion。
func TestCompactTransaction(t *testing.T) {
	sess, err := session.NewMemoryStore().Create(t.Context(), session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	seedTurn(t, ctx, sess)
	before, _ := sess.Events(ctx, 0)
	if got := sess.Header().FormatVersion; got != session.FormatVersion {
		t.Fatalf("pre-compaction version = %d", got)
	}

	rep, err := Compact(ctx, sess, Options{Engine: DeterministicSummarizer{}, ModelName: "deterministic"})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := sess.Events(ctx, 0)
	if len(after) != len(before)+4 {
		t.Fatalf("events %d → %d, want +4（started/summarized/checkpoint/ended；raw log 只增不减）",
			len(before), len(after))
	}
	for i, ev := range before {
		if after[i].Seq != ev.Seq || string(after[i].Data) != string(ev.Data) {
			t.Fatalf("raw log mutated at %d", i)
		}
	}
	// 事件序：started → summarized → checkpoint → ended。
	tail := after[len(before):]
	wantTypes := []session.EventType{
		session.EventCompactionStarted, session.EventCompactionSummarized,
		session.EventCompactionCheckpoint, session.EventCompactionEnded,
	}
	for i, want := range wantTypes {
		if tail[i].Type != want {
			t.Fatalf("event[%d] = %s, want %s", i, tail[i].Type, want)
		}
	}
	// Replaced 完整：窗口 4 个节点的 source seqs。
	if len(rep.Replaced) != 4 || rep.Replaced[0] != 1 || rep.Replaced[3] != 4 {
		t.Fatalf("Replaced = %v, want [1 2 3 4]", rep.Replaced)
	}
	if rep.CheckpointSeq != tail[2].Seq {
		t.Fatalf("CheckpointSeq = %d, want %d", rep.CheckpointSeq, tail[2].Seq)
	}
	// Surface：Replace 后只剩 RoleUser 稳定前缀消息（事件类型不是
	// message.user，但 fold 成 RoleUser）。
	surface, err := sess.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) != 1 || surface[0].Role != llm.RoleUser {
		t.Fatalf("surface = %v, want single RoleUser summary", surface)
	}
	if !strings.HasPrefix(surface[0].Text(), "[compacted 4 messages]") {
		t.Fatalf("summary text = %q", surface[0].Text())
	}
	if got := sess.Header().FormatVersion; got != session.CompactedVersion {
		t.Fatalf("post-compaction version = %d, want %d", got, session.CompactedVersion)
	}
}

// TestCompactWindowSplitsToolGroup：选区切在 tool 组中间 → 预检失败，
// 零落盘（事务未开始）。
func TestCompactWindowSplitsToolGroup(t *testing.T) {
	sess, _ := session.NewMemoryStore().Create(t.Context(), session.SessionHeader{})
	ctx := t.Context()
	seedTurn(t, ctx, sess)
	before, _ := sess.Events(ctx, 0)

	// 窗口 [1,1]：assistant(call c1) 单独，result 在窗口外 → 切组。
	win := [2]int{1, 1}
	_, err := Compact(ctx, sess, Options{Engine: DeterministicSummarizer{}, Window: &win})
	if err == nil {
		t.Fatal("split window must be rejected")
	}
	after, _ := sess.Events(ctx, 0)
	if len(after) != len(before) {
		t.Fatalf("events %d → %d（预检失败不允许落任何事件）", len(before), len(after))
	}
	// 窗口 [1,2]：assistant + 其 result，整组合法。
	win = [2]int{1, 2}
	rep, err := Compact(ctx, sess, Options{Engine: DeterministicSummarizer{}, Window: &win})
	if err != nil {
		t.Fatalf("whole-group window must pass: %v", err)
	}
	if len(rep.Replaced) != 2 || rep.Replaced[0] != 2 || rep.Replaced[1] != 3 {
		t.Fatalf("Replaced = %v, want [2 3]", rep.Replaced)
	}
	surface, _ := sess.Surface(ctx)
	// [user] + [summary] + [assistant done] = 3 节点。
	if len(surface) != 3 || surface[1].Role != llm.RoleUser {
		t.Fatalf("surface = %v", surface)
	}
}

// TestCompactSummarizerFailure：Summarize 失败 → started 已落、无
// checkpoint/ended——未闭合 compaction 留审计（§9.1）。
func TestCompactSummarizerFailure(t *testing.T) {
	sess, _ := session.NewMemoryStore().Create(t.Context(), session.SessionHeader{})
	ctx := t.Context()
	seedTurn(t, ctx, sess)
	before, _ := sess.Events(ctx, 0)

	if _, err := Compact(ctx, sess, Options{Engine: failingEngine{}}); err == nil {
		t.Fatal("summarizer failure must surface")
	}
	after, _ := sess.Events(ctx, 0)
	if len(after) != len(before)+1 {
		t.Fatalf("events = %d, want +%d（只有 started 落盘）", len(after), 1)
	}
	if after[len(after)-1].Type != session.EventCompactionStarted {
		t.Fatalf("last event = %s, want compaction.started", after[len(after)-1].Type)
	}
	for _, ev := range after {
		if ev.Type == session.EventCompactionCheckpoint || ev.Type == session.EventCompactionEnded {
			t.Fatalf("%s must not exist after failure", ev.Type)
		}
	}
	// Surface 不变（无 Replace 发生）。
	surface, _ := sess.Surface(ctx)
	if len(surface) != 4 {
		t.Fatalf("surface = %d nodes, want 4（失败不影响投影）", len(surface))
	}
}

// TestLLMSummarizer：模型摘要走 fake ChatModel；usage 传播进结果。
func TestLLMSummarizer(t *testing.T) {
	eng := &LLMSummarizer{Model: &fakeChatModel{text: "the config lives in /etc/app.yaml"}, ModelName: "gpt-test"}
	res, err := eng.Summarize(t.Context(), SummarizeInput{
		Messages: []*llm.Message{llm.UserText("find the config")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Role != llm.RoleUser || !strings.Contains(res.Summary.Text(), "/etc/app.yaml") {
		t.Fatalf("summary = %+v", res.Summary)
	}
	if res.Model != "gpt-test" || res.Usage.InputTokens != 120 {
		t.Fatalf("usage = %+v", res)
	}
	if _, err := (&LLMSummarizer{}).Summarize(t.Context(), SummarizeInput{}); err == nil {
		t.Fatal("nil model must error（不静默假装压缩成功）")
	}
}

// TestMeterAndPressure：CharMeter rune 估算 + Pressure 判定。
func TestMeterAndPressure(t *testing.T) {
	m := CharMeter{}
	msgs := []*llm.Message{llm.UserText(strings.Repeat("a", 400))}
	if got := m.Tokens(msgs); got != 100 {
		t.Fatalf("tokens = %d, want 100", got)
	}
	if !Pressure(m, msgs, 99) || Pressure(m, msgs, 100) {
		t.Fatal("pressure threshold semantics wrong")
	}
}

// TestPruneResults：超预算 result 裁剪为 head+marker+tail；原 result 事件
// 保留在 raw log；checkpoint Replace 只替代该节点。
func TestPruneResults(t *testing.T) {
	sess, _ := session.NewMemoryStore().Create(t.Context(), session.SessionHeader{})
	ctx := t.Context()
	if _, err := sess.Append(ctx, draftUser(t, "run it")); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(ctx, draftAssistant(t, llm.Call(llm.ToolCall{ID: "c1"}))); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("x", 5000)
	if _, err := sess.Append(ctx, draftToolResult(t, "c1", huge)); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(ctx, draftAssistant(t, llm.Text("ok"))); err != nil {
		t.Fatal(err)
	}

	n, checkpoints, err := PruneResults(ctx, sess, PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(checkpoints) != 1 {
		t.Fatalf("pruned = %d checkpoints = %v, want 1", n, checkpoints)
	}
	surface, _ := sess.Surface(ctx)
	pruned := surface[2].Parts[0].ToolResultValue
	if pruned == nil {
		t.Fatal("node 2 missing")
	}
	text := pruned.Content[0].Text
	if len([]rune(text)) >= 5000 {
		t.Fatal("text not pruned")
	}
	if !strings.Contains(text, "pruned") || !strings.HasPrefix(text, "xxx") || !strings.HasSuffix(text, "xxx") {
		t.Fatalf("pruned text lacks head/marker/tail: %q", text[:60])
	}
	// 原 result 事件保留在 raw log。
	events, _ := sess.Events(ctx, 0)
	var rawFound bool
	for _, ev := range events {
		if ev.Type == session.EventToolResult && strings.Contains(string(ev.Data), strings.Repeat("x", 100)) {
			rawFound = true
		}
	}
	if !rawFound {
		t.Fatal("raw result event must be kept（原日志完整保存）")
	}
	// 未超预算的节点不动。
	if n2, _, _ := PruneResults(ctx, sess, PruneOptions{}); n2 != 0 {
		t.Fatalf("second pass pruned %d nodes, want 0（幂等）", n2)
	}
}

// TestJSONLCompactPersists：JSONL 上压缩——checkpoint roundtrip、header
// 版本持久化、重开 surface 一致。
func TestJSONLCompactPersists(t *testing.T) {
	store, err := session.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sess, err := store.Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	seedTurn(t, ctx, sess)
	if _, err := Compact(ctx, sess, Options{Engine: DeterministicSummarizer{}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	id := sess.Header().SessionID
	closeJSONL(t, sess)
	reopened, err := store.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer closeJSONL(t, reopened)
	if got := reopened.Header().FormatVersion; got != session.CompactedVersion {
		t.Fatalf("persisted version = %d, want %d（header.json 同步抬升）", got, session.CompactedVersion)
	}
	surface, err := reopened.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) != 1 || surface[0].Role != llm.RoleUser {
		t.Fatalf("reopened surface = %v", surface)
	}
	// 压缩后的 surface 上继续压缩（窗口 [0,0] 单节点合法）。
	if _, err := Compact(ctx, reopened, Options{Engine: DeterministicSummarizer{}}); err != nil {
		t.Fatal(err)
	}
	surface2, _ := reopened.Surface(ctx)
	if len(surface2) != 1 || !strings.Contains(surface2[0].Text(), "[compacted 1 messages]") {
		t.Fatalf("re-compact surface = %v", surface2)
	}
}

// TestFoldTraceSources：溯源映射——Append 节点 source = 事件 Seq；
// checkpoint 新节点 source = checkpoint Seq。
func TestFoldTraceSources(t *testing.T) {
	reg := session.NewRegistry()
	sess, _ := session.NewMemoryStore().Create(t.Context(), session.SessionHeader{})
	ctx := t.Context()
	seedTurn(t, ctx, sess)
	events, _ := sess.Events(ctx, 0)
	msgs, sources, err := session.FoldTrace(events, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 || len(sources) != 4 || sources[0] != 1 || sources[3] != 4 {
		t.Fatalf("sources = %v, want [1 2 3 4]", sources)
	}
	if _, err := Compact(ctx, sess, Options{Engine: DeterministicSummarizer{}}); err != nil {
		t.Fatal(err)
	}
	events, _ = sess.Events(ctx, 0)
	msgs, sources, err = session.FoldTrace(events, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || sources[0] != 7 {
		t.Fatalf("post-compact source = %v, want [7]（checkpoint Seq：started 5 / summarized 6 / checkpoint 7）", sources)
	}
	_ = json.RawMessage(nil)
}
