package assemble_test

// 结构性缓存命中评测（#148，§13.2 缓存指标的离线形态）：
//
// 同一 session 逐轮「Append 用户消息 → Surface() → Assemble → Append 回复」，
// 用相邻两轮请求的最长公共消息前缀的 token 占比近似 provider 前缀缓存命中
// （消息级粒度；token 与 CharMeter 同口径 rune/4）。四个场景：
//
//	① 基线（只增不减）：命中率随会话变长上升、趋于高位；
//	② 中途 RefreshStable（改写前缀）：该轮塌陷、后续恢复；
//	③ 中途 compaction（事务压缩）：该轮塌陷、恢复；
//	④ compaction vs 不动：同位置超大 tool result 的对照（原 prune 臂随
//	  #150 移除，三方对照数据存档于 #148 评审记录），含 15→30 轮计费总量
//	  （简化价模型：命中 0.1×，未命中 1.0×）。
//
// 全部确定性、无网络；数字是「组装结构本身」的命中上界形状，外生因素
// （provider TTL、真实 tokenizer 差异）不在本评测口径内。

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/assemble"
	"github.com/Luo-root/pulse/memory/compaction"
	"github.com/Luo-root/pulse/memory/session"
	"github.com/Luo-root/pulse/memory/store"
)

// ---- 计数与命中率 ----

// meterTokens 与 CharMeter 同口径：按 rune 计数 / 4（text / call / result）。
func meterTokens(msgs []*llm.Message) int {
	total := 0
	for _, m := range msgs {
		if m == nil {
			continue
		}
		for _, p := range m.Parts {
			switch p.Kind {
			case llm.PartText, llm.PartReasoning:
				total += len([]rune(p.Text))
			case llm.PartToolCall:
				if p.ToolCallValue != nil {
					total += len([]rune(p.ToolCallValue.Name)) + len([]rune(string(p.ToolCallValue.Arguments)))
				}
			case llm.PartToolResult:
				if p.ToolResultValue != nil {
					for _, rp := range p.ToolResultValue.Content {
						total += len([]rune(rp.Text))
					}
				}
			}
		}
	}
	return total / 4
}

// prefixHit 计算相邻请求的最长公共消息前缀（provider 前缀缓存的消息级近似）。
func prefixHit(prev, cur []*llm.Message) (hit, total int) {
	total = meterTokens(cur)
	n := len(prev)
	if n > len(cur) {
		n = len(cur)
	}
	c := 0
	for c < n && reflect.DeepEqual(prev[c], cur[c]) {
		c++
	}
	return meterTokens(cur[:c]), total
}

// ---- session 事件草稿（与 examples/05 同构） ----

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func draftUser(text string) session.EventDraft {
	return session.EventDraft{
		Type:    session.EventMessageUser,
		Data:    mustJSON(session.MessagePayload{Parts: []llm.Part{llm.Text(text)}}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

func draftAssistant(text string) session.EventDraft {
	return session.EventDraft{
		Type:    session.EventMessageAssistant,
		Data:    mustJSON(session.MessagePayload{Parts: []llm.Part{llm.Text(text)}}),
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

// ---- 逐轮模拟器 ----

var nsEval = []string{"cache-eval"}

const (
	turnsTotal    = 30
	cachePrice    = 0.1 // 简化价模型：命中 token 计 0.1×
	fullPrice     = 1.0 // 未命中 token 计 1.0×
	oversizeRunes = 30000
)

type hitRow struct {
	turn  int
	total int
	hit   int
	rate  float64
}

type cacheSim struct {
	t        *testing.T
	memStore store.MemoryStore
	sess     session.Session
	asm      *assemble.DefaultAssembler
	pool     *[]store.MemoryItem // semantic seam 的条目池（即时可见路径）
	prev     []*llm.Message      // 上一轮请求（组装产物）
	turnNo   int
}

func seedItems() []store.MemoryItem {
	return []store.MemoryItem{
		{ID: "seed-postgres", Namespace: nsEval, Kind: store.KindDecision,
			Content: "Audit logs must use postgres storage", Status: store.StatusActive, Taint: store.TaintTrusted, Confidence: 0.9,
			SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: "cache-eval-sim", Seq: 1}}},
		{ID: "seed-yaml", Namespace: nsEval, Kind: store.KindProfile,
			Content: "Pulse flows are declared in yaml", Status: store.StatusActive, Taint: store.TaintTrusted, Confidence: 0.9,
			SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: "cache-eval-sim", Seq: 2}}},
		{ID: "seed-deploy", Namespace: nsEval, Kind: store.KindProfile,
			Content: "Deploy pipeline runs deploy checks first", Status: store.StatusActive, Taint: store.TaintTrusted, Confidence: 0.9,
			SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: "cache-eval-sim", Seq: 3}}},
	}
}

func topicOf(content string) string {
	for _, topic := range []string{"postgres", "yaml", "deploy"} {
		if strings.Contains(content, topic) {
			return topic
		}
	}
	return ""
}

func newCacheSim(t *testing.T, items []store.MemoryItem) *cacheSim {
	t.Helper()
	ctx := context.Background()
	memStore := store.NewMemoryStore()
	for _, it := range items {
		if _, err := memStore.Put(ctx, it, store.PutMemoryOptions{}); err != nil {
			t.Fatalf("put seed %s: %v", it.ID, err)
		}
	}
	sess, err := session.NewMemoryStore().Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	asm := assemble.NewDefaultAssembler(memStore, nil, assemble.Budget{
		StableMemoryTokens: 200, RetrievedTokens: 200, MaxSurfaceTail: 500,
	})
	// 确定性 semantic seam：按 query 里的主题词匹配条目——检索块每轮随
	// Query 变化（尾部动态语义），不依赖任何索引实现。池子用指针持有，
	// 场景②中途补写的新记忆即时进入检索面（§8.3：检索路径立即可见）。
	pool := append([]store.MemoryItem(nil), items...)
	asm.Semantic = func(_ context.Context, _ []string, q string, k int) ([]store.MemoryItem, []float64, error) {
		var out []store.MemoryItem
		for _, it := range pool {
			if strings.Contains(q, topicOf(it.Content)) {
				out = append(out, it)
			}
			if len(out) >= k {
				break
			}
		}
		scores := make([]float64, len(out))
		for i := range scores {
			scores[i] = 1
		}
		return out, scores, nil
	}
	return &cacheSim{t: t, memStore: memStore, sess: sess, asm: asm, pool: &pool}
}

// putSeed 写入新记忆并同步 seam 池子（检索路径即时可见）。
func (s *cacheSim) putSeed(item store.MemoryItem) {
	s.t.Helper()
	if _, err := s.memStore.Put(context.Background(), item, store.PutMemoryOptions{}); err != nil {
		s.t.Fatalf("put %s: %v", item.ID, err)
	}
	*s.pool = append(*s.pool, item)
}

// turn 组装一轮：记录命中率行并推进 session（user → assemble → assistant）。
func (s *cacheSim) turn(query, reply string, refresh bool) hitRow {
	s.t.Helper()
	ctx := context.Background()
	if _, err := s.sess.Append(ctx, draftUser(query)); err != nil {
		s.t.Fatalf("append user: %v", err)
	}
	surface, err := s.sess.Surface(ctx)
	if err != nil {
		s.t.Fatalf("surface: %v", err)
	}
	ac, err := s.asm.Assemble(ctx, assemble.AssembleInput{
		Namespace:     nsEval,
		Surface:       surface,
		Query:         query,
		RefreshStable: refresh,
	})
	if err != nil {
		s.t.Fatalf("assemble turn %d: %v", s.turnNo+1, err)
	}
	hit, total := prefixHit(s.prev, ac.Messages)
	row := hitRow{turn: s.turnNo + 1, total: total, hit: hit}
	if total > 0 {
		row.rate = float64(hit) / float64(total)
	}
	s.turnNo++
	s.t.Logf("turn %2d  total=%6d hit=%6d rate=%.3f", row.turn, row.total, row.hit, row.rate)
	if _, err := s.sess.Append(ctx, draftAssistant(reply)); err != nil {
		s.t.Fatalf("append assistant: %v", err)
	}
	s.prev = ac.Messages
	return row
}

func (r hitRow) billed() float64 {
	return float64(r.hit)*cachePrice + float64(r.total-r.hit)*fullPrice
}

func queryAt(i int) string {
	topics := []string{"postgres", "yaml", "deploy"}
	return fmt.Sprintf("turn %02d: question about %s configuration and related details", i+1, topics[i%3])
}

func replyAt(i int) string {
	return fmt.Sprintf("answer %02d: the %s question is handled per existing policy and recorded in the session log for later review", i+1, []string{"postgres", "yaml", "deploy"}[i%3])
}

// ---- 场景 ①：基线（只增不减） ----

func TestCacheHitBaseline(t *testing.T) {
	s := newCacheSim(t, seedItems())
	rates := make([]float64, 0, turnsTotal)
	for i := 0; i < turnsTotal; i++ {
		rates = append(rates, s.turn(queryAt(i), replyAt(i), false).rate)
	}
	if rates[turnsTotal-1] < 0.85 {
		t.Fatalf("final hit rate = %.3f, want >= 0.85 (append-only prefix must approach high hit)", rates[turnsTotal-1])
	}
	if rates[turnsTotal-1] < rates[2]+0.15 {
		t.Fatalf("hit rate must grow with session length: turn3=%.3f turn30=%.3f", rates[2], rates[turnsTotal-1])
	}
}

// ---- 场景 ②：中途 RefreshStable（带新记忆写入） ----

func TestCacheHitMidSessionRefreshStable(t *testing.T) {
	s := newCacheSim(t, seedItems())
	for i := 0; i < 14; i++ {
		s.turn(queryAt(i), replyAt(i), false)
	}
	// 会话中途写入新记忆并显式重建稳定前缀：前缀内容变化 → 整体失效。
	s.putSeed(store.MemoryItem{ID: "seed-rate-limit", Namespace: nsEval, Kind: store.KindDecision,
		Content: "Rate limit deploy jobs to two concurrent runs", Status: store.StatusActive, Taint: store.TaintTrusted, Confidence: 0.9,
		SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: "cache-eval-sim", Seq: 4}}})
	row15 := s.turn(queryAt(14), replyAt(14), true)
	t.Logf("refresh turn: rate=%.3f (prefix rewritten -> collapse)", row15.rate)
	if row15.rate > 0.05 {
		t.Fatalf("refresh-with-new-item should collapse hit: rate=%.3f", row15.rate)
	}
	rate30 := 0.0
	for i := 15; i < turnsTotal; i++ {
		rate30 = s.turn(queryAt(i), replyAt(i), false).rate
	}
	if rate30 < 0.85 {
		t.Fatalf("hit must recover after refresh: got %.3f", rate30)
	}
}

// ---- 场景 ③：中途 compaction（全量，确定性摘要） ----

func TestCacheHitCompactionRecovery(t *testing.T) {
	s := newCacheSim(t, seedItems())
	for i := 0; i < 14; i++ {
		s.turn(queryAt(i), replyAt(i), false)
	}
	if _, err := compaction.Compact(context.Background(), s.sess, compaction.Options{
		Engine: &compaction.DeterministicSummarizer{}, Meter: compaction.CharMeter{}, ModelName: "deterministic",
	}); err != nil {
		t.Fatalf("compact: %v", err)
	}
	row15 := s.turn(queryAt(14), replyAt(14), false)
	t.Logf("post-compaction: rate=%.3f (surface replaced -> prefix region miss)", row15.rate)
	if row15.rate > 0.5 {
		t.Fatalf("compaction should collapse hit for the compacted region: %.3f", row15.rate)
	}
	rate30 := 0.0
	for i := 15; i < turnsTotal; i++ {
		rate30 = s.turn(queryAt(i), replyAt(i), false).rate
	}
	if rate30 < 0.80 {
		t.Fatalf("hit must recover after compaction: got %.3f", rate30)
	}
}

// ---- 场景 ④：compaction vs 不动 ----

// oversizedTurn3 埋第 3 轮：user → assistant(call) → 30k 字符 result → assistant。
func (s *cacheSim) oversizedTurn3() {
	s.t.Helper()
	ctx := context.Background()
	big := strings.Repeat("x", oversizeRunes)
	if _, err := s.sess.Append(ctx, draftUser(queryAt(2))); err != nil {
		s.t.Fatalf("append: %v", err)
	}
	// call 必须是真实 ToolCall（session 的 Replace 校验要求 call/result 配对）。
	callDraft := session.EventDraft{
		Type:    session.EventMessageAssistant,
		Data:    mustJSON(session.MessagePayload{Parts: []llm.Part{llm.Call(llm.ToolCall{ID: "big-1", Name: "lookup"})}}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
	if _, err := s.sess.Append(ctx, callDraft); err != nil {
		s.t.Fatalf("append: %v", err)
	}
	if _, err := s.sess.Append(ctx, draftToolResult("big-1", big)); err != nil {
		s.t.Fatalf("append: %v", err)
	}
	surface, err := s.sess.Surface(ctx)
	if err != nil {
		s.t.Fatalf("surface: %v", err)
	}
	ac, err := s.asm.Assemble(ctx, assemble.AssembleInput{Namespace: nsEval, Surface: surface, Query: queryAt(2)})
	if err != nil {
		s.t.Fatalf("assemble turn3: %v", err)
	}
	hit, total := prefixHit(s.prev, ac.Messages)
	s.turnNo++
	s.t.Logf("turn  3  total=%6d hit=%6d rate=%.3f (oversized result in surface)", total, hit, float64(hit)/float64(total))
	if _, err := s.sess.Append(ctx, draftAssistant("lookup done")); err != nil {
		s.t.Fatalf("append: %v", err)
	}
	s.prev = ac.Messages
}

// runThrough 返回第 15..30 轮的行与计费总量；op 在第 15 轮组装前执行。
func runThrough(t *testing.T, withOversized bool, op func(s *cacheSim)) (rows []hitRow, billed float64) {
	t.Helper()
	s := newCacheSim(t, seedItems())
	s.turn(queryAt(0), replyAt(0), false)
	s.turn(queryAt(1), replyAt(1), false)
	if withOversized {
		s.oversizedTurn3()
	} else {
		s.turn(queryAt(2), replyAt(2), false)
	}
	for i := 3; i < 14; i++ {
		s.turn(queryAt(i), replyAt(i), false)
	}
	if op != nil {
		op(s)
	}
	for i := 14; i < turnsTotal; i++ {
		row := s.turn(queryAt(i), replyAt(i), false)
		rows = append(rows, row)
		billed += row.billed()
	}
	return rows, billed
}

func TestCacheHitCompactionVersusBaseline(t *testing.T) {
	ctx := context.Background()

	// A：不动（超大 result 留在前缀里，每轮吃 cached 价——长期累计并非免费）。
	rowsA, billedA := runThrough(t, true, nil)

	// C：第 15 轮前 compaction 窗口 [0,7]（覆盖超大 result 所在轮，
	//   窗口端点落在 assistant 消息上，不切 tool 组）。
	rowsC, billedC := runThrough(t, true, func(s *cacheSim) {
		w := [2]int{0, 7}
		if _, err := compaction.Compact(ctx, s.sess, compaction.Options{
			Engine: &compaction.DeterministicSummarizer{}, Meter: compaction.CharMeter{},
			ModelName: "deterministic", Window: &w,
		}); err != nil {
			t.Fatalf("compact: %v", err)
		}
	})

	t.Logf("hit@15:  baseline=%.3f  compact=%.3f", rowsA[0].rate, rowsC[0].rate)
	t.Logf("hit@30:  baseline=%.3f  compact=%.3f", rowsA[len(rowsA)-1].rate, rowsC[len(rowsC)-1].rate)
	t.Logf("billed 15-30 (hit*%.1f + miss*%.1f):  baseline=%.0f  compact=%.0f",
		cachePrice, fullPrice, billedA, billedC)

	if rowsC[0].rate >= rowsA[0].rate {
		t.Fatalf("compaction must break cache vs baseline: compact=%.3f baseline=%.3f", rowsC[0].rate, rowsA[0].rate)
	}
	if billedC >= billedA {
		t.Fatalf("compaction should bill less than keeping the oversized result: compact=%.0f baseline=%.0f", billedC, billedA)
	}
}
