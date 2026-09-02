package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/session"
)

// ---- 事件构造（公开 API；与各包内测试 helper 同形，避免 import test 内部）----

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("eval: marshal payload %T: %v", v, err))
	}
	return data
}

func draftUser(r *rng, text string) session.EventDraft {
	return session.EventDraft{
		Type:    session.EventMessageUser,
		Data:    mustJSON(session.MessagePayload{Parts: []llm.Part{llm.Text(text)}}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

func draftAssistant(parts ...llm.Part) session.EventDraft {
	return session.EventDraft{
		Type:    session.EventMessageAssistant,
		Data:    mustJSON(session.MessagePayload{Parts: parts}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

func draftToolResult(callID, text string) session.EventDraft {
	return session.EventDraft{
		Type:    session.EventToolResult,
		Data:    mustJSON(session.ToolResultPayload{ToolCallID: callID, Text: text}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

// genTurnDrafts 生成一个结构完整的回合：user → [assistant(call) + tool.result]* →
// assistant(text)。每回合 3~7 个 surface 节点，工具组天然闭合。
func genTurnDrafts(r *rng, turnIdx int) []session.EventDraft {
	drafts := []session.EventDraft{draftUser(r, r.text(80))}
	for c := 0; c < r.IntN(3); c++ {
		callID := fmt.Sprintf("c%d-%d", turnIdx, c)
		drafts = append(drafts,
			draftAssistant(llm.Call(llm.ToolCall{ID: callID, Name: r.randStr(8), Arguments: json.RawMessage(`{}`)})),
			draftToolResult(callID, r.text(40)),
		)
	}
	drafts = append(drafts, draftAssistant(llm.Text(r.text(60))))
	return drafts
}

// copyTruncated 把 {srcRoot}/{id} 的 header.json + events.jsonl 拷到新 root，
// events.jsonl 只保留前 keepBytes 字节（模拟撕裂写入 / 崩溃丢尾）。lock 与
// blobs 不拷贝——副本是干净的「崩溃现场」。
func copyTruncated(t *testing.T, srcRoot, id string, keepBytes int) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(srcRoot, id, "events.jsonl"))
	if err != nil {
		t.Fatalf("eval: read events.jsonl: %v", err)
	}
	hdr, err := os.ReadFile(filepath.Join(srcRoot, id, "header.json"))
	if err != nil {
		t.Fatalf("eval: read header.json: %v", err)
	}
	if keepBytes > len(raw) {
		keepBytes = len(raw)
	}
	dstRoot := t.TempDir()
	dstDir := filepath.Join(dstRoot, id)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("eval: mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "header.json"), hdr, 0o644); err != nil {
		t.Fatalf("eval: write header: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "events.jsonl"), raw[:keepBytes], 0o644); err != nil {
		t.Fatalf("eval: write truncated events: %v", err)
	}
	return dstRoot
}

// closeJSONL 释放文件实现的句柄与文件锁（Close 是 JSONL 特有能力，不在
// Session 接口上——按包注释口径做类型断言；内存实现等无 Close 的直接跳过）。
func closeJSONL(t *testing.T, sess session.Session) {
	t.Helper()
	if c, ok := sess.(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			t.Fatalf("eval: close: %v", err)
		}
	}
}

// assertSurfacePrefix 断言 got 是 want 的逐条前缀（Role + 全部文本 Part）。
func assertSurfacePrefix(t *testing.T, r *rng, iter, phase string, got, want []*llm.Message) {
	t.Helper()
	if len(got) > len(want) {
		t.Fatal(r.failf("iter=%s %s: surface %d nodes > baseline %d", iter, phase, len(got), len(want)))
	}
	for i := range got {
		if got[i].Role != want[i].Role || got[i].Text() != want[i].Text() {
			t.Fatal(r.failf("iter=%s %s: surface[%d] = (%s,%q), want (%s,%q)",
				iter, phase, i, got[i].Role, got[i].Text(), want[i].Role, want[i].Text()))
		}
	}
}

// TestPropertySessionTornRecovery 崩溃恢复不变式（memory/session）：
//
//	P1 撕裂识别：任意字节截断的 events.jsonl 在 Open 时被识别——合法前缀
//	   完整保留、损坏尾行丢弃，Open 不失败；
//	P2 合法前缀保持：回合边界截断后，Surface 与基准逐条相等（零合成事件）；
//	P3 fold 合法：任意截断后 Surface 可折叠（配对由合成闭合事件保证，
//	   不产生孤儿）；
//	P4 可续写：恢复后 Append 新事件成功；
//	P5 恢复幂等：二次 Open 的合成事件只补一次，Events 数不变。
func TestPropertySessionTornRecovery(t *testing.T) {
	seed := seedFor(t.Name())
	ctx := t.Context()
	for iter := 0; iter < 10; iter++ {
		r := newRng(seed + int64(iter)*7919)

		// ① 随机回合序列 + 安全边界（回合结束的行号）。
		var all []session.EventDraft
		var safeLines []int
		for ti := 0; ti < 2+r.IntN(4); ti++ {
			ds := genTurnDrafts(r, ti)
			all = append(all, ds...)
			safeLines = append(safeLines, len(all))
		}

		// ② 内存基准 + JSONL 全量；两实现 surface 必须一致。
		memSess, err := session.NewMemoryStore().Create(ctx, session.SessionHeader{SessionID: "base"})
		if err != nil {
			t.Fatal(r.failf("iter=%d: mem create: %v", iter, err))
		}
		jlRoot := t.TempDir()
		jl, err := session.NewJSONLStore(jlRoot)
		if err != nil {
			t.Fatal(r.failf("iter=%d: jsonl create store: %v", iter, err))
		}
		jlSess, err := jl.Create(ctx, session.SessionHeader{SessionID: "prop"})
		if err != nil {
			t.Fatal(r.failf("iter=%d: jsonl create: %v", iter, err))
		}
		for i, d := range all {
			if _, err := memSess.Append(ctx, d); err != nil {
				t.Fatal(r.failf("iter=%d: mem append[%d] %s: %v", iter, i, d.Type, err))
			}
			if _, err := jlSess.Append(ctx, d); err != nil {
				t.Fatal(r.failf("iter=%d: jsonl append[%d] %s: %v", iter, i, d.Type, err))
			}
		}
		if err := jlSess.Flush(ctx); err != nil {
			t.Fatal(r.failf("iter=%d: flush: %v", iter, err))
		}
		baseSurface, err := memSess.Surface(ctx)
		if err != nil {
			t.Fatal(r.failf("iter=%d: mem surface: %v", iter, err))
		}
		jlSurface, err := jlSess.Surface(ctx)
		if err != nil {
			t.Fatal(r.failf("iter=%d: jsonl surface: %v", iter, err))
		}
		if len(jlSurface) != len(baseSurface) {
			t.Fatal(r.failf("iter=%d: jsonl surface %d != mem %d", iter, len(jlSurface), len(baseSurface)))
		}
		assertSurfacePrefix(t, r, fmt.Sprint(iter), "baseline", jlSurface, baseSurface)
		closeJSONL(t, jlSess)

		// ③ 安全边界截断（P2/P4 强断言）。
		safe := safeLines[r.IntN(len(safeLines))]
		raw, err := os.ReadFile(filepath.Join(jlRoot, "prop", "events.jsonl"))
		if err != nil {
			t.Fatal(r.failf("iter=%d: read raw: %v", iter, err))
		}
		lines := strings.SplitAfter(string(raw), "\n")
		keep := 0
		for i := 0; i < safe && i < len(lines); i++ {
			keep += len(lines[i])
		}
		root1 := copyTruncated(t, jlRoot, "prop", keep)
		store1, err := session.NewJSONLStore(root1)
		if err != nil {
			t.Fatal(r.failf("iter=%d safe=%d: store: %v", iter, safe, err))
		}
		s1, err := store1.Open(ctx, "prop")
		if err != nil {
			t.Fatal(r.failf("iter=%d safe=%d: reopen: %v", iter, safe, err))
		}
		got1, err := s1.Surface(ctx)
		if err != nil {
			t.Fatal(r.failf("iter=%d safe=%d: surface after safe cut: %v", iter, safe, err))
		}
		// 每条事件恰好折叠出一个 surface 节点；回合边界截断保留 safe 行，
		// 因此节点数应恰为 safe（多 = 合成事件泄漏，少 = 丢完整事件）。
		assertSurfacePrefix(t, r, fmt.Sprint(iter), fmt.Sprintf("safe=%d", safe), got1, baseSurface)
		if len(got1) != safe {
			t.Fatal(r.failf("iter=%d safe=%d: surface %d nodes, want exactly %d",
				iter, safe, len(got1), safe))
		}
		if _, err := s1.Append(ctx, draftUser(r, "after recovery")); err != nil {
			t.Fatal(r.failf("iter=%d safe=%d: append after recovery: %v", iter, safe, err))
		}
		got1b, err := s1.Surface(ctx)
		if err != nil || len(got1b) != len(got1)+1 {
			t.Fatal(r.failf("iter=%d safe=%d: append after recovery not reflected (%d nodes, err=%v)",
				iter, safe, len(got1b), err))
		}
		closeJSONL(t, s1)

		// ④ 任意字节截断（P1/P3/P5 弱断言 + P4）。
		if len(raw) < 2 {
			continue
		}
		cut := 1 + r.IntN(len(raw)-1)
		root2 := copyTruncated(t, jlRoot, "prop", cut)
		store2, err := session.NewJSONLStore(root2)
		if err != nil {
			t.Fatal(r.failf("iter=%d cut=%d: store: %v", iter, cut, err))
		}
		s2, err := store2.Open(ctx, "prop")
		if err != nil {
			t.Fatal(r.failf("iter=%d cut=%d: torn reopen must succeed: %v", iter, cut, err))
		}
		if _, err := s2.Surface(ctx); err != nil {
			t.Fatal(r.failf("iter=%d cut=%d: fold after torn cut must succeed: %v", iter, cut, err))
		}
		if _, err := s2.Append(ctx, draftUser(r, "torn recovery")); err != nil {
			t.Fatal(r.failf("iter=%d cut=%d: append after torn recovery: %v", iter, cut, err))
		}
		if err := s2.Flush(ctx); err != nil {
			t.Fatal(r.failf("iter=%d cut=%d: flush: %v", iter, cut, err))
		}
		evs1, err := s2.Events(ctx, 0)
		if err != nil {
			t.Fatal(r.failf("iter=%d cut=%d: events: %v", iter, cut, err))
		}
		// 二次 Open 前必须先关闭 s2：A2 语义下同进程重复 Open 命中 store
		// 缓存直接返回 live 会话、不做冷恢复——不关的话 evs2 与 evs1 是同
		// 一个 live session 的两次自查询，恒等，「合成事件只补一次」这条
		// 真不变式（二次冷恢复重复合成）测不出来。关闭后第二次 Open 走
		// 真·冷恢复路径，读回的是已写回合成事件且 Flush 过的 healed 文件。
		closeJSONL(t, s2)
		s2b, err := store2.Open(ctx, "prop")
		if err != nil {
			t.Fatal(r.failf("iter=%d cut=%d: reopen#2: %v", iter, cut, err))
		}
		evs2, err := s2b.Events(ctx, 0)
		if err != nil {
			t.Fatal(r.failf("iter=%d cut=%d: events#2: %v", iter, cut, err))
		}
		if len(evs2) != len(evs1) {
			t.Fatal(r.failf("iter=%d cut=%d: reopen events %d != %d（合成事件只补一次）",
				iter, cut, len(evs2), len(evs1)))
		}
		closeJSONL(t, s2b)
	}
}
