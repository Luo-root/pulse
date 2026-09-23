package eval

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/assemble"
	"github.com/Luo-root/pulse/memory/compaction"
	"github.com/Luo-root/pulse/memory/index"
	"github.com/Luo-root/pulse/memory/session"
	"github.com/Luo-root/pulse/memory/store"
)

// ---- D2 混合检索链（memory/index → memory/assemble.Semantic）----

// conceptEmbedder 是确定性假 EmbeddingProvider：按「文本命中概念词表的哪些
// 维度」出向量（维度恒为 len(conceptLexicon)；未命中 = 零向量，余弦 0）。
// 不随机、不看时间、不看 map 顺序——同一文本永远同一向量。
//
// 词表刻意让同一概念有多种写法（英文 / 中文）：查询取英文、item 用中文时
// 两者正文互不为子串——这正是向量路存在的理由，也是本文件「字面不相交但
// 语义近」的构造基础。
type conceptEmbedder struct{}

// conceptLexicon 每条 = 一个概念维度的同义词集合。
var conceptLexicon = [][]string{
	{"release", "deploy", "发布", "上线"},
	{"database", "schema", "数据库", "表结构", "索引"},
	{"rollback", "revert", "回滚", "撤销"},
}

// Embed 实现 index.EmbeddingProvider。
func (conceptEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec := make([]float32, len(conceptLexicon))
		lower := strings.ToLower(t)
		for d, words := range conceptLexicon {
			for _, w := range words {
				if strings.Contains(lower, w) {
					vec[d] = 1
					break
				}
			}
		}
		out[i] = vec
	}
	return out, nil
}

// queryCase 是一对同概念的不同写法：query 是检索信号，synonym 出现在
// 「语义近」item 的正文里（两者互不为子串）。
type queryCase struct{ query, synonym string }

// hybridQueryCases 覆盖两对写法（全跑，不抽样）。
var hybridQueryCases = []queryCase{
	{query: "rollback", synonym: "回滚"},
	{query: "revert", synonym: "撤销"},
}

// retrievalItem 造一条可入库的 Active 记忆。Kind 刻意避开 Profile/Decision：
// 那两类会进 stable 前缀（§8.3 frozen snapshot），与检索路混在一起就分不清
// 「这条 item 是谁带进来的」。
func retrievalItem(id string, ns []string, content string) store.MemoryItem {
	return store.MemoryItem{
		ID:         id,
		Namespace:  ns,
		Kind:       store.KindEpisode,
		Content:    content,
		Status:     store.StatusActive,
		Confidence: 1.0,
		SourceRefs: []store.SourceRef{{Type: store.SourceManual, Ref: "eval"}},
		Taint:      store.TaintTrusted,
	}
}

// messageIndexOf 返回第一条正文含 marker 的消息下标（-1 = 未命中）。
// 断言落在**渲染后的消息正文**上，不打桩任何内部结构。
func messageIndexOf(msgs []*llm.Message, marker string) int {
	for i, m := range msgs {
		if m != nil && strings.Contains(m.Text(), marker) {
			return i
		}
	}
	return -1
}

// textsOf 把消息序列压成一行（断言失败时的现场快照）。
func textsOf(msgs []*llm.Message) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		txt := []rune(m.Text())
		if len(txt) > 40 {
			txt = append(txt[:40], '…')
		}
		parts = append(parts, fmt.Sprintf("%s:%q", m.Role, string(txt)))
	}
	return "[" + strings.Join(parts, " | ") + "]"
}

// hasSemanticDiag 报告诊断里是否出现 semantic 路记录——「缝真被消费」的
// 可观测信号（接缝与不接缝必须一个有一个没有）。
func hasSemanticDiag(ac assemble.AssembledContext) bool {
	for _, d := range ac.Diagnostics {
		if strings.Contains(d.Reason, "semantic") {
			return true
		}
	}
	return false
}

// hasHitID 报告向量命中里是否有该 ID。
func hasHitID(hits []index.ScoredHit, id string) bool {
	for _, h := range hits {
		if h.Item.ID == id {
			return true
		}
	}
	return false
}

// TestPropertyHybridRetrievalChain hybrid 检索链可执行样例（memory/index →
// memory/assemble）：D2 的 Semantic seam 在仓库内没有可执行消费点（审计票
// #224 第 3 条——改签名不会有生产编译错误提醒），本测试把真实 MemIndex 接
// 上 DefaultAssembler.Semantic，用双向对照把这条链钉住：
//
//	H1 双向对照：同一 store / 同一预算 / 同一查询下，一条**与查询字面不相交
//	   但语义近**的 item 只有接了 seam 才进产物；不接 seam（nil = keyword-
//	   only）时它进不去，而关键词命中的 item 两次都在——证明是向量路携带，
//	   不是关键词路的巧合；
//	H2 融合分数被消费：接缝时产物顺序 = §8.2 融合分降序（关键词双路 1.0 >
//	   语义单路 0.7 > 无关项 0.2）——seam 传错分数、[]ScoredHit 拆 items/
//	   scores 拆错或形状不符（assemble 记诊断并丢语义路）都会打散顺序；
//	H3 Namespace 透传：跨 namespace 的语义近 item 被「先过滤再召回」挡在产物
//	   外——seam 没把 AssembleInput.Namespace 交给向量路时它就会漏出来。
func TestPropertyHybridRetrievalChain(t *testing.T) {
	seed := seedFor(t.Name())
	ctx := t.Context()
	meter := compaction.CharMeter{}
	ns := []string{"tenant:acme", "project:pulse"}
	foreignNS := []string{"tenant:other"}
	// 预算取宽：本测试问的是「谁被召回」，裁切口径归 assemble 包内用例。
	budget := assemble.Budget{RetrievedTokens: 256}
	// 干扰项：同 namespace、Active，但与查询概念正交（余弦 0）——用来把
	// 「语义路把索引里所有东西都倒出来」与「按相似度召回」区分开。
	decoyTopics := []string{
		"数据库索引重建的窗口安排在周日凌晨 标记DEC",
		"表结构变更需要先跑一遍兼容性检查 标记DEC",
		"上线检查清单的回执已经归档 标记DEC",
	}
	const (
		kwMarker      = "标记KW"
		semMarker     = "标记SEM"
		decMarker     = "标记DEC"
		foreignMarker = "标记FOREIGN"
	)
	for c, qc := range hybridQueryCases {
		r := newRng(seed + int64(c)*2654435761)
		decoy := pick(r, decoyTopics)

		// ① 真 store + 真索引 + 真装配器（只有 embedding 是假实现）。
		st := store.NewMemoryStore()
		semContent := "预案要求：" + qc.synonym + "演练必须先写清楚并演练 " + semMarker
		if strings.Contains(strings.ToLower(semContent), qc.query) {
			t.Fatal(r.failf("case=%d: 构造失效——语义 item 正文含查询串 %q，不再「字面不相交」", c, qc.query))
		}
		items := []store.MemoryItem{
			retrievalItem("kw-aaa", ns, "演练记录 "+qc.query+" 已归档 "+kwMarker),
			retrievalItem("sem-zzz", ns, semContent),
			retrievalItem("dec-mmm", ns, decoy),
			retrievalItem("foreign-nnn", foreignNS, "预案要求："+qc.synonym+"演练回执已归档 "+foreignMarker),
		}
		saved := make([]store.MemoryItem, 0, len(items))
		for _, it := range items {
			s, err := st.Put(ctx, it, store.PutMemoryOptions{})
			if err != nil {
				t.Fatal(r.failf("case=%d: put %s: %v", c, it.ID, err))
			}
			saved = append(saved, s)
		}
		idx, err := index.NewMemIndex(st, conceptEmbedder{})
		if err != nil {
			t.Fatal(r.failf("case=%d: new index: %v", c, err))
		}
		for _, it := range saved {
			if err := idx.Upsert(ctx, it); err != nil {
				t.Fatal(r.failf("case=%d: upsert %s: %v", c, it.ID, err))
			}
		}
		// 反证 H3 不是空转：跨 ns item 确实在索引里（只应被 ns 先过滤挡住）。
		global, err := idx.Search(ctx, nil, qc.query, 0)
		if err != nil {
			t.Fatal(r.failf("case=%d: index search(global ns): %v", c, err))
		}
		if len(global) != len(items) {
			t.Fatal(r.failf("case=%d: 索引全局命中 %d 条，want %d（构造失效）", c, len(global), len(items)))
		}
		if !hasHitID(global, "foreign-nnn") {
			t.Fatal(r.failf("case=%d: 构造失效——跨 ns item 不在索引内，H3 断言会空转", c))
		}

		// ② 接缝：[]index.ScoredHit 拆成 items + 平行 scores（形状不符会被
		//    assemble 记诊断并丢语义路——正是 H1/H2 要拦的回归）。
		wired := assemble.NewDefaultAssembler(st, meter.Tokens, budget)
		wired.Semantic = func(ctx context.Context, ns []string, query string, k int) ([]store.MemoryItem, []float64, error) {
			hits, err := idx.Search(ctx, ns, query, k)
			if err != nil {
				return nil, nil, err
			}
			outItems := make([]store.MemoryItem, len(hits))
			outScores := make([]float64, len(hits))
			for j, h := range hits {
				outItems[j] = h.Item
				outScores[j] = h.Score
			}
			return outItems, outScores, nil
		}
		// 对照组：同一 store / 同一预算 / 同一查询，Semantic 保持 nil。
		plain := assemble.NewDefaultAssembler(st, meter.Tokens, budget)
		in := assemble.AssembleInput{Namespace: ns, Query: qc.query}

		hybrid, err := wired.Assemble(ctx, in)
		if err != nil {
			t.Fatal(r.failf("case=%d: assemble(seam): %v", c, err))
		}
		kwOnly, err := plain.Assemble(ctx, in)
		if err != nil {
			t.Fatal(r.failf("case=%d: assemble(keyword-only): %v", c, err))
		}

		// H1：接缝 3 条（关键词 + 语义 + 正交干扰），不接缝只剩关键词 1 条——
		// 两条对照用同一查询，差异只能是向量路带来的。
		if len(hybrid.Messages) != 3 {
			t.Fatal(r.failf("case=%d: 接缝产物 %d 条，want 3 %s",
				c, len(hybrid.Messages), textsOf(hybrid.Messages)))
		}
		if len(kwOnly.Messages) != 1 {
			t.Fatal(r.failf("case=%d: 不接缝产物 %d 条，want 1 %s",
				c, len(kwOnly.Messages), textsOf(kwOnly.Messages)))
		}
		if at := messageIndexOf(kwOnly.Messages, semMarker); at >= 0 {
			t.Fatal(r.failf("case=%d: 不接 seam 时语义近 item 仍进产物（「字面不相交」构造失效）", c))
		}
		if messageIndexOf(hybrid.Messages, semMarker) < 0 {
			t.Fatal(r.failf("case=%d: 接 seam 后语义近 item 未进产物——向量路没接上 %s",
				c, textsOf(hybrid.Messages)))
		}
		// H2：顺序 = 融合分降序；关键词 item 两条路都命中，两次都在。
		for want, marker := range []string{kwMarker, semMarker, decMarker} {
			if at := messageIndexOf(hybrid.Messages, marker); at != want {
				t.Fatal(r.failf("case=%d: 接缝产物顺序 %s，want [kw sem dec]（marker %s 落在 %d）",
					c, textsOf(hybrid.Messages), marker, at))
			}
		}
		if at := messageIndexOf(kwOnly.Messages, kwMarker); at != 0 {
			t.Fatal(r.failf("case=%d: 不接缝产物 %s，want 仅关键词命中 %s",
				c, textsOf(kwOnly.Messages), kwMarker))
		}
		// H3：跨 ns item 两轮都不进产物。
		if messageIndexOf(hybrid.Messages, foreignMarker) >= 0 {
			t.Fatal(r.failf("case=%d: 跨 namespace item 漏进产物——seam 丢了 AssembleInput.Namespace %s",
				c, textsOf(hybrid.Messages)))
		}
		if messageIndexOf(kwOnly.Messages, foreignMarker) >= 0 {
			t.Fatal(r.failf("case=%d: 跨 namespace item 漏进 keyword 产物 %s",
				c, textsOf(kwOnly.Messages)))
		}
		// 缝被消费的可观测面：接缝有 semantic 诊断，不接缝没有。
		if !hasSemanticDiag(hybrid) {
			t.Fatal(r.failf("case=%d: 接缝产物没有 semantic 诊断——语义路未被消费: %+v",
				c, hybrid.Diagnostics))
		}
		if hasSemanticDiag(kwOnly) {
			t.Fatal(r.failf("case=%d: 不接缝却有 semantic 诊断: %+v", c, kwOnly.Diagnostics))
		}
	}
}

// TestPropertyPressureDrivesCompaction 压力闸可执行样例（memory/compaction）：
// Pressure 在仓库内没有可执行消费点（审计票 #224 第 3 条），本测试把它摆成
// Compact 的门，双向钉住「超阈值才压、压完压力真的回落」：
//
//	P6 闸门双向：同一 surface 的两侧阈值判定必须相反——strictly greater，
//	   等于阈值 = 不压；未超阈值分支宿主不调 Compact，会话事件零新增（压缩
//	   事务的落盘归 Compact，闸门自己有副作用就是 bug）；
//	P7 压缩真降压力：超阈值分支 Compact 真跑（事务 +4 事件、Replaced == 全量
//	   窗口），surface 节点数与 token 双降，随后同一阈值重判回落为 false。
func TestPropertyPressureDrivesCompaction(t *testing.T) {
	seed := seedFor(t.Name())
	ctx := t.Context()
	meter := compaction.CharMeter{}
	for iter := 0; iter < 8; iter++ {
		r := newRng(seed + int64(iter)*104729)

		// ① 随机会话（3~6 回合，结构与 compaction_budget_test.go 同源）。
		var all []session.EventDraft
		for ti := 0; ti < 3+r.IntN(4); ti++ {
			all = append(all, genTurnDrafts(r, ti)...)
		}
		sess, err := session.NewMemoryStore().Create(ctx, session.SessionHeader{})
		if err != nil {
			t.Fatal(r.failf("iter=%d: create: %v", iter, err))
		}
		for i, d := range all {
			if _, err := sess.Append(ctx, d); err != nil {
				t.Fatal(r.failf("iter=%d: append[%d] %s: %v", iter, i, d.Type, err))
			}
		}
		// 收尾追加一条确定性长消息：随机部分只管结构，长度下限由它保证
		// （阈值对偶与「压缩后 token 严格下降」都需要压缩前明显有余量）。
		if _, err := sess.Append(ctx, draftUser(r, strings.Repeat("pressure-budget ", 20))); err != nil {
			t.Fatal(r.failf("iter=%d: append filler: %v", iter, err))
		}
		// 称重对象 = 宿主即将发送的 surface；Compact 内部用
		// FoldTrace(events, registry) 折叠同一日志——本场景两者必须是同一序列
		// （否则闸门称的东西和压缩的东西不是一回事）。
		msgs, err := sess.Surface(ctx)
		if err != nil {
			t.Fatal(r.failf("iter=%d: surface: %v", iter, err))
		}
		events, err := sess.Events(ctx, 0)
		if err != nil {
			t.Fatal(r.failf("iter=%d: events: %v", iter, err))
		}
		folded, sources, err := session.FoldTrace(events, sess.Registry())
		if err != nil {
			t.Fatal(r.failf("iter=%d: foldtrace: %v", iter, err))
		}
		if len(folded) != len(msgs) {
			t.Fatal(r.failf("iter=%d: Surface %d 节点 != FoldTrace %d 节点（称重与压缩口径不一致）",
				iter, len(msgs), len(folded)))
		}
		spent := meter.Tokens(msgs)
		if spent <= 0 {
			t.Fatal(r.failf("iter=%d: 称重 %d token（构造失效）", iter, spent))
		}

		// ② 未超阈值：threshold = spent 恰在边界上——Pressure 是严格大于，
		//    判定必须为 false；宿主按 false 分支走 = 不调 Compact。
		if compaction.Pressure(meter, msgs, spent) {
			t.Fatal(r.failf("iter=%d: Pressure(%d token, threshold=%d) = true（边界应为严格大于）",
				iter, spent, spent))
		}
		frozen, err := sess.Events(ctx, 0)
		if err != nil {
			t.Fatal(r.failf("iter=%d: events after gate-false: %v", iter, err))
		}
		if len(frozen) != len(events) {
			t.Fatal(r.failf("iter=%d: 未超阈值却有 %d 个新事件（宿主不该压缩）",
				iter, len(frozen)-len(events)))
		}

		// ③ 超阈值：threshold = spent-1 → true → 宿主调 Compact，事务真跑。
		if !compaction.Pressure(meter, msgs, spent-1) {
			t.Fatal(r.failf("iter=%d: Pressure(%d token, threshold=%d) = false（应超阈值）",
				iter, spent, spent-1))
		}
		rep, err := compaction.Compact(ctx, sess, compaction.Options{
			Engine:    shortEngine{},
			Meter:     meter,
			ModelName: "short",
		})
		if err != nil {
			t.Fatal(r.failf("iter=%d: 超阈值分支 compact: %v", iter, err))
		}
		after, err := sess.Surface(ctx)
		if err != nil {
			t.Fatal(r.failf("iter=%d: surface after compact: %v", iter, err))
		}
		if len(after) >= len(msgs) {
			t.Fatal(r.failf("iter=%d: 压缩后 surface %d 节点 >= 压缩前 %d（没变小）",
				iter, len(after), len(msgs)))
		}
		if got := meter.Tokens(after); got >= spent {
			t.Fatal(r.failf("iter=%d: 压缩后 %d token >= 压缩前 %d（压力没降）", iter, got, spent))
		}
		if len(rep.Replaced) != len(msgs) {
			t.Fatal(r.failf("iter=%d: Replaced %d 条，want %d（全量窗口被替代）",
				iter, len(rep.Replaced), len(msgs)))
		}
		for i := range rep.Replaced {
			if rep.Replaced[i] != sources[i] {
				t.Fatal(r.failf("iter=%d: Replaced[%d] = %d, want %d（与 FoldTrace 来源不符）",
					iter, i, rep.Replaced[i], sources[i]))
			}
		}
		// 事务口径最小互证：恰好 +4 事件（细节归
		// TestPropertyCompactionTransaction，这里只证明「Compact 真跑了」）。
		closed, err := sess.Events(ctx, 0)
		if err != nil {
			t.Fatal(r.failf("iter=%d: events after compact: %v", iter, err))
		}
		if len(closed) != len(events)+4 {
			t.Fatal(r.failf("iter=%d: 压缩事件 %d → %d，want +4", iter, len(events), len(closed)))
		}

		// ④ 重判：同一阈值（spent-1）下压力回落为 false——压缩真的释放了预算，
		//    而不是「只换了内容」。
		if compaction.Pressure(meter, after, spent-1) {
			t.Fatal(r.failf("iter=%d: 压缩后仍判超阈值（压缩未释放压力）", iter))
		}
	}
}
