package eval

import (
	"context"
	"testing"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/compaction"
	"github.com/Luo-root/pulse/memory/session"
)

// shortEngine 是「诚实摘要引擎」测试替身：把任意选区压成一段固定短摘
// 要（非零 token——CharMeter 按 rune/4 整除，单字符会被算成 0）。压缩后
// token 严格下降的性质在合理摘要引擎下必须成立（框架不承诺任意引擎下
// 下降，但承诺事务正确性 + 替换语义；本 property 用可控引擎把
// 「替换正确 → token 下降」这条链钉住）。
type shortEngine struct{}

func (shortEngine) Summarize(_ context.Context, in compaction.SummarizeInput) (compaction.SummarizeResult, error) {
	return compaction.SummarizeResult{
		Summary: llm.Message{Role: llm.RoleUser, Parts: []llm.Part{llm.Text("[property-test summary for compaction]")}},
		Model:   "short",
	}, nil
}

// TestPropertyCompactionTransaction 压缩事务不变式（memory/compaction）：
//
//	P1 二选一：任意合法范围窗口下，Compact 要么事务成功、要么零落盘
//	   失败（预检拒绝）——绝不产生中间态（落了部分事件）；
//	P2 raw log 只增不减：成功时原事件逐条原样保留，恰好追加 4 事件
//	   （started → summarized → checkpoint → ended）；
//	P3 Replaced 与 CheckpointSeq 一致：Report 与日志尾部互相印证；
//	P4 压缩后 surface 可折叠，FormatVersion 抬升到 CompactedVersion。
func TestPropertyCompactionTransaction(t *testing.T) {
	seed := seedFor(t.Name())
	ctx := t.Context()
	for iter := 0; iter < 10; iter++ {
		r := newRng(seed + int64(iter)*104729)

		// ① 随机回合序列（1~4 回合，每回合 3~7 节点）。
		var all []session.EventDraft
		for ti := 0; ti < 1+r.IntN(4); ti++ {
			all = append(all, genTurnDrafts(r, ti)...)
		}
		sess, err := session.NewMemoryStore().Create(ctx, session.SessionHeader{})
		if err != nil {
			t.Fatal(r.failf("iter=%d: create: %v", iter, err))
		}
		for i, d := range all {
			if _, err := sess.Append(ctx, d); err != nil {
				t.Fatal(r.failf("iter=%d: append[%d]: %v", iter, i, err))
			}
		}
		before, err := sess.Events(ctx, 0)
		if err != nil {
			t.Fatal(r.failf("iter=%d: events: %v", iter, err))
		}
		surfaceBefore, err := sess.Surface(ctx)
		if err != nil {
			t.Fatal(r.failf("iter=%d: surface: %v", iter, err))
		}

		// ② 随机窗口：全量 / 随机 [s,e]。
		var win *[2]int
		switch r.IntN(3) {
		case 0:
			// nil = 全量
		default:
			n := len(surfaceBefore)
			s := r.IntN(n)
			win = &[2]int{s, s + r.IntN(n-s)}
		}

		// ③ 压缩，二选一断言。
		rep, err := compaction.Compact(ctx, sess, compaction.Options{
			Engine:    compaction.DeterministicSummarizer{},
			ModelName: "deterministic",
			Window:    win,
		})
		after, errEvents := sess.Events(ctx, 0)
		if errEvents != nil {
			t.Fatal(r.failf("iter=%d: events after: %v", iter, errEvents))
		}
		if err != nil {
			// P1 失败分支：零落盘。
			if len(after) != len(before) {
				t.Fatal(r.failf("iter=%d: compaction rejected (%v) but log mutated %d → %d",
					iter, err, len(before), len(after)))
			}
			continue
		}

		// P2 成功分支：raw log 前缀不变 + 恰好 +4。
		if len(after) != len(before)+4 {
			t.Fatal(r.failf("iter=%d: events %d → %d, want +4", iter, len(before), len(after)))
		}
		for i := range before {
			if after[i].Seq != before[i].Seq || string(after[i].Data) != string(before[i].Data) {
				t.Fatal(r.failf("iter=%d: raw log mutated at %d", iter, i))
			}
		}
		tail := after[len(before):]
		wantTypes := []session.EventType{
			session.EventCompactionStarted, session.EventCompactionSummarized,
			session.EventCompactionCheckpoint, session.EventCompactionEnded,
		}
		for i, want := range wantTypes {
			if tail[i].Type != want {
				t.Fatal(r.failf("iter=%d: tail[%d] = %s, want %s", iter, i, tail[i].Type, want))
			}
		}

		// P3 Replaced == 窗口 source seqs；CheckpointSeq == tail[2]。
		_, sources, err := session.FoldTrace(before, sess.Registry())
		if err != nil {
			t.Fatal(r.failf("iter=%d: foldtrace: %v", iter, err))
		}
		s, e := 0, len(surfaceBefore)-1
		if win != nil {
			s, e = win[0], win[1]
		}
		if len(rep.Replaced) != e-s+1 {
			t.Fatal(r.failf("iter=%d: Replaced len %d, want %d", iter, len(rep.Replaced), e-s+1))
		}
		for i := range rep.Replaced {
			if rep.Replaced[i] != sources[s+i] {
				t.Fatal(r.failf("iter=%d: Replaced[%d] = %d, want %d", iter, i, rep.Replaced[i], sources[s+i]))
			}
		}
		if rep.CheckpointSeq != tail[2].Seq {
			t.Fatal(r.failf("iter=%d: CheckpointSeq = %d, want %d", iter, rep.CheckpointSeq, tail[2].Seq))
		}

		// P4 surface 合法 + 版本抬升。
		surfaceAfter, err := sess.Surface(ctx)
		if err != nil {
			t.Fatal(r.failf("iter=%d: surface after compaction: %v", iter, err))
		}
		if len(surfaceAfter) == 0 {
			t.Fatal(r.failf("iter=%d: surface empty after compaction", iter))
		}
		if got := sess.Header().FormatVersion; got != session.CompactedVersion {
			t.Fatal(r.failf("iter=%d: version = %d, want %d", iter, got, session.CompactedVersion))
		}
	}
}

// TestPropertyCompactionShrinks token 效率不变式：
//
//	P5 合理摘要引擎下，全量压缩后 surface token 严格下降，且收敛为
//	   单条 RoleUser 摘要（Meter 口径，CharMeter）。
func TestPropertyCompactionShrinks(t *testing.T) {
	seed := seedFor(t.Name())
	ctx := t.Context()
	meter := compaction.CharMeter{}
	for iter := 0; iter < 8; iter++ {
		r := newRng(seed + int64(iter)*6700417)

		var all []session.EventDraft
		for ti := 0; ti < 2+r.IntN(4); ti++ {
			all = append(all, genTurnDrafts(r, ti)...)
		}
		sess, err := session.NewMemoryStore().Create(ctx, session.SessionHeader{})
		if err != nil {
			t.Fatal(r.failf("iter=%d: create: %v", iter, err))
		}
		for i, d := range all {
			if _, err := sess.Append(ctx, d); err != nil {
				t.Fatal(r.failf("iter=%d: append[%d]: %v", iter, i, err))
			}
		}
		before, err := sess.Surface(ctx)
		if err != nil {
			t.Fatal(r.failf("iter=%d: surface: %v", iter, err))
		}
		beforeTokens := meter.Tokens(before)

		if _, err := compaction.Compact(ctx, sess, compaction.Options{Engine: shortEngine{}}); err != nil {
			t.Fatal(r.failf("iter=%d: compact: %v", iter, err))
		}
		after, err := sess.Surface(ctx)
		if err != nil {
			t.Fatal(r.failf("iter=%d: surface after: %v", iter, err))
		}
		afterTokens := meter.Tokens(after)
		if len(after) != 1 || after[0].Role != llm.RoleUser {
			t.Fatal(r.failf("iter=%d: surface = %d nodes, want single RoleUser", iter, len(after)))
		}
		if afterTokens >= beforeTokens {
			t.Fatal(r.failf("iter=%d: tokens %d → %d（诚实摘要引擎下必须下降）",
				iter, beforeTokens, afterTokens))
		}
		if afterTokens == 0 {
			t.Fatal(r.failf("iter=%d: summary vanished（0 token）", iter))
		}
	}
}
