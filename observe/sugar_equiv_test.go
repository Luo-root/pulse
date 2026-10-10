package observe

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Luo-root/pulse"
)

// 等价锚（#279 验收）：同一拓扑用「糖」与「手写 NewNode」各装一张图，跑出来的
// 观测记录**逐字段一致**——糖只做名字与签名的收束，不许偷偷改语义。
//
// 对的是「观测面」：事件名、节点归因、Status、Err、信封（host/trace/source）与
// 图级夹住顺序；时间戳 / 耗时随运行变，不参与比对（它们是同一时钟源的两条实现，
// 不是同一次运行）。
func TestSugarMatchesHandwrittenRecords(t *testing.T) {
	in := pulse.NewKey[string]("equiv.in")
	r1 := pulse.NewKey[string]("equiv.r1")
	r2 := pulse.NewKey[string]("equiv.r2")
	joined := pulse.NewKey[string]("equiv.joined")

	// —— 糖版本 ——
	sugarSink := &MemorySink{}
	sugarObs, err := NewRecordObserver(ObserveConfig{Sink: sugarSink, HostID: "h", TraceID: "equiv"})
	if err != nil {
		t.Fatal(err)
	}
	sg := mustGraph(t, "equiv", pulse.WithObserver(sugarObs))
	if err := pulse.Seed(sg, in, "doc"); err != nil {
		t.Fatal(err)
	}
	if err := pulse.FanOut(sg, "worker", in, []pulse.Key[string]{r1, r2},
		func(rc *pulse.RunCtx, shard int, v string) (string, error) {
			if shard == 2 {
				return "", pulse.NoValue()
			}
			return v + "!", nil
		}); err != nil {
		t.Fatal(err)
	}
	if err := pulse.Join(sg, "collect", []pulse.Key[string]{r1, r2}, joined,
		func(rc *pulse.RunCtx, b pulse.Batch[string]) (string, error) {
			return fmt.Sprintf("%d/%d", b.Len(), len(b.Missing())), nil
		}); err != nil {
		t.Fatal(err)
	}
	if err := sg.Run(); err != nil {
		t.Fatal(err)
	}

	// —— 手写版本：同一拓扑、同样的节点名与同样的结论 ——
	handSink := &MemorySink{}
	handObs, err := NewRecordObserver(ObserveConfig{Sink: handSink, HostID: "h", TraceID: "equiv"})
	if err != nil {
		t.Fatal(err)
	}
	hg := mustGraph(t, "equiv", pulse.WithObserver(handObs))
	if err := pulse.Seed(hg, in, "doc"); err != nil {
		t.Fatal(err)
	}
	for i, out := range []pulse.Key[string]{r1, r2} {
		out, shard := out, i+1
		if err := hg.Add(pulse.NewNode("worker-"+fmt.Sprint(shard), pulse.Requires(in), pulse.Provides(out),
			func(rc *pulse.RunCtx) error {
				v, err := pulse.Get(rc, in)
				if err != nil {
					return err
				}
				if shard == 2 {
					return pulse.NoValue() // 与糖里那个分支同一结论：这一次没有值
				}
				return pulse.Set(rc, out, v+"!")
			})); err != nil {
			t.Fatal(err)
		}
	}
	if err := hg.Add(pulse.NewNode("collect", pulse.Requires(r1, r2), pulse.Provides(joined),
		func(rc *pulse.RunCtx) error {
			n, missing := 0, 0
			for _, k := range []pulse.Key[string]{r1, r2} {
				_, ok, skipped, err := pulse.TryGet(rc, k)
				if err != nil {
					return err
				}
				if ok {
					n++
				}
				if skipped {
					missing++
				}
			}
			return pulse.Set(rc, joined, fmt.Sprintf("%d/%d", n, missing))
		})); err != nil {
		t.Fatal(err)
	}
	if err := hg.Run(); err != nil {
		t.Fatal(err)
	}

	got, want := canonRecords(sugarSink.Snapshot()), canonRecords(handSink.Snapshot())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("糖与手写的观测记录不一致：\n糖  = %v\n手写= %v", got, want)
	}
	if len(got) == 0 {
		t.Fatal("两边都没有记录：等价锚失效")
	}
	// 光「两边一致」还不够——两边都空、都错也会一致。三条节点记录各自钉一下。
	dump := strings.Join(got, "\n")
	for _, needle := range []string{
		"node=worker-1|graph=equiv|status=completed",
		"node=worker-2|graph=equiv|status=skipped", // 没产出的那一份：跳过，不是失败
		"node=collect|graph=equiv|status=completed",
	} {
		if !strings.Contains(dump, needle) {
			t.Fatalf("记录里缺少 %q：\n%s", needle, dump)
		}
	}
}

// 等价锚·续（#284 验收最后一条）：`Only` 与流式三件（`Produce` / `Tee` /
// `Consume`）同样只收束名字与签名，不许改语义——比对口径与上面那条完全一致
// （事件名、节点归因、Status、Err、信封与图级夹住顺序；时间戳 / 耗时除外）。
func TestOnlyMatchesHandwrittenRecords(t *testing.T) {
	a := pulse.NewKey[string]("only.a")
	b := pulse.NewKey[string]("only.b")

	// addUse 两侧共用：同名的下游、同样的读法。
	addUse := func(g *pulse.Graph, id string, k pulse.Key[string], hit *bool) {
		t.Helper()
		if err := g.Add(pulse.NewNode(id, pulse.Requires(k), nil, func(rc *pulse.RunCtx) error {
			v, err := pulse.Get(rc, k)
			if err != nil {
				return err
			}
			if v != "走A" {
				return fmt.Errorf("下游 %s 拿到 %q", id, v)
			}
			*hit = true
			return nil
		})); err != nil {
			t.Fatal(err)
		}
	}

	// —— 糖版本：Only(rc, a, v) = 写 a、其余出口作废 ——
	sugarSink := &MemorySink{}
	sugarObs, err := NewRecordObserver(ObserveConfig{Sink: sugarSink, HostID: "h", TraceID: "only"})
	if err != nil {
		t.Fatal(err)
	}
	sugarUseA, sugarUseB := false, false
	sg := mustGraph(t, "only", pulse.WithObserver(sugarObs))
	if err := sg.Add(pulse.NewNode("route", nil, pulse.Provides(a, b), func(rc *pulse.RunCtx) error {
		return pulse.Only(rc, a, "走A")
	})); err != nil {
		t.Fatal(err)
	}
	addUse(sg, "useA", a, &sugarUseA)
	addUse(sg, "useB", b, &sugarUseB)
	if err := sg.Run(); err != nil {
		t.Fatal(err)
	}

	// —— 手写版本：同一拓扑、同名的节点、同一结论（1 次 Set + N−1 次 Skip）——
	handSink := &MemorySink{}
	handObs, err := NewRecordObserver(ObserveConfig{Sink: handSink, HostID: "h", TraceID: "only"})
	if err != nil {
		t.Fatal(err)
	}
	handUseA, handUseB := false, false
	hg := mustGraph(t, "only", pulse.WithObserver(handObs))
	if err := hg.Add(pulse.NewNode("route", nil, pulse.Provides(a, b), func(rc *pulse.RunCtx) error {
		if err := pulse.Set(rc, a, "走A"); err != nil {
			return err
		}
		return pulse.Skip(rc, b)
	})); err != nil {
		t.Fatal(err)
	}
	addUse(hg, "useA", a, &handUseA)
	addUse(hg, "useB", b, &handUseB)
	if err := hg.Run(); err != nil {
		t.Fatal(err)
	}

	if !sugarUseA || !handUseA {
		t.Fatal("选中的那条下游两侧都应当执行")
	}
	if sugarUseB || handUseB {
		t.Fatal("作废的那条下游两侧都不该执行")
	}

	got, want := canonRecords(sugarSink.Snapshot()), canonRecords(handSink.Snapshot())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Only 与手写的观测记录不一致：\nOnly = %v\n手写 = %v", got, want)
	}
	dump := strings.Join(got, "\n")
	for _, needle := range []string{
		"node=route|graph=only|status=completed",
		"node=useA|graph=only|status=completed",
		"node=useB|graph=only|status=skipped", // 作废的出口：下游跳过，不是失败
	} {
		if !strings.Contains(dump, needle) {
			t.Fatalf("记录里缺少 %q：\n%s", needle, dump)
		}
	}
}

// 等价锚·续：流式三件（`Produce` / `Tee` / `Consume`）与手写「建 channel →
// `Set` 发布 → 活着发完 → `defer close`」的观测记录逐字段一致。
//
// 手写那侧刻意按糖的纪律写（关闭只在 `defer`、发送 / 接收都在 `select` 上让
// 取消出得来）——要证明的是**糖没有偷偷改语义**，不是「手写怎么写都行」。
func TestStreamSugarMatchesHandwrittenRecords(t *testing.T) {
	stream := pulse.NewKey[<-chan int]("st.stream")
	sA := pulse.NewKey[<-chan int]("st.a")
	sB := pulse.NewKey[<-chan int]("st.b")
	values := []int{1, 2, 3}

	// —— 糖版本 ——
	sugarSink := &MemorySink{}
	sugarObs, err := NewRecordObserver(ObserveConfig{Sink: sugarSink, HostID: "h", TraceID: "st"})
	if err != nil {
		t.Fatal(err)
	}
	sugarGot := map[string][]int{}
	sg := mustGraph(t, "st", pulse.WithObserver(sugarObs))
	if err := pulse.Produce(sg, "src", stream, func(rc *pulse.RunCtx, send func(int) error) error {
		for _, v := range values {
			if err := send(v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pulse.Tee(sg, "fan", stream, pulse.Keys(sA, sB)); err != nil {
		t.Fatal(err)
	}
	var sugarMu sync.Mutex
	consume := func(id string, k pulse.Key[<-chan int]) error {
		return pulse.Consume(sg, id, k, func(rc *pulse.RunCtx, v int) error {
			sugarMu.Lock()
			sugarGot[id] = append(sugarGot[id], v)
			sugarMu.Unlock()
			return nil
		})
	}
	if err := consume("sinkA", sA); err != nil {
		t.Fatal(err)
	}
	if err := consume("sinkB", sB); err != nil {
		t.Fatal(err)
	}
	if err := sg.Run(); err != nil {
		t.Fatal(err)
	}

	// —— 手写版本：同一拓扑、同样的节点名与同样的关闭 / 取消纪律 ——
	handSink := &MemorySink{}
	handObs, err := NewRecordObserver(ObserveConfig{Sink: handSink, HostID: "h", TraceID: "st"})
	if err != nil {
		t.Fatal(err)
	}
	handGot := map[string][]int{}
	hg := mustGraph(t, "st", pulse.WithObserver(handObs))
	if err := hg.Add(pulse.NewNode("src", nil, pulse.Provides(stream), func(rc *pulse.RunCtx) error {
		ch := make(chan int)
		defer close(ch) // 关闭只在 defer：成功 / 出错 / 取消都走它，不会双次 close
		if err := pulse.Set(rc, stream, ch); err != nil {
			return err
		}
		for _, v := range values {
			select {
			case ch <- v:
			case <-rc.Context().Done():
				return rc.Context().Err()
			}
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := hg.Add(pulse.NewNode("fan", pulse.Requires(stream), pulse.Provides(sA, sB),
		func(rc *pulse.RunCtx) error {
			src, err := pulse.Get(rc, stream)
			if err != nil {
				return err
			}
			chans := []chan int{make(chan int), make(chan int)}
			defer func() {
				for _, ch := range chans {
					close(ch)
				}
			}()
			for i, k := range []pulse.Key[<-chan int]{sA, sB} {
				if err := pulse.Set(rc, k, chans[i]); err != nil {
					return err
				}
			}
			for {
				select {
				case <-rc.Context().Done():
					return rc.Context().Err()
				case v, ok := <-src:
					if !ok {
						return nil
					}
					for _, ch := range chans {
						select {
						case ch <- v:
						case <-rc.Context().Done():
							return rc.Context().Err()
						}
					}
				}
			}
		})); err != nil {
		t.Fatal(err)
	}
	var handMu sync.Mutex
	for _, c := range []struct {
		id string
		k  pulse.Key[<-chan int]
	}{{"sinkA", sA}, {"sinkB", sB}} {
		id, k := c.id, c.k
		if err := hg.Add(pulse.NewNode(id, pulse.Requires(k), nil, func(rc *pulse.RunCtx) error {
			ch, err := pulse.Get(rc, k)
			if err != nil {
				return err
			}
			for {
				select {
				case <-rc.Context().Done():
					return rc.Context().Err()
				case v, ok := <-ch:
					if !ok {
						return nil
					}
					handMu.Lock()
					handGot[id] = append(handGot[id], v)
					handMu.Unlock()
				}
			}
		})); err != nil {
			t.Fatal(err)
		}
	}
	if err := hg.Run(); err != nil {
		t.Fatal(err)
	}

	// 两侧都必须是「广播」：每个下游拿到完整同序的 values（糖不改变分发语义）。
	for _, side := range []struct {
		name string
		got  map[string][]int
	}{{"糖", sugarGot}, {"手写", handGot}} {
		for _, id := range []string{"sinkA", "sinkB"} {
			if fmt.Sprint(side.got[id]) != fmt.Sprint(values) {
				t.Fatalf("%s版本的 %s 拿到 %v，want %v", side.name, id, side.got[id], values)
			}
		}
	}

	got, want := canonRecords(sugarSink.Snapshot()), canonRecords(handSink.Snapshot())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("流式糖与手写的观测记录不一致：\n糖  = %v\n手写= %v", got, want)
	}
	dump := strings.Join(got, "\n")
	for _, needle := range []string{
		"node=src|graph=st|status=completed",
		"node=fan|graph=st|status=completed",
		"node=sinkA|graph=st|status=completed",
		"node=sinkB|graph=st|status=completed",
	} {
		if !strings.Contains(dump, needle) {
			t.Fatalf("记录里缺少 %q：\n%s", needle, dump)
		}
	}
}

// canonRecords 把记录折成可排序的字符串（时间戳 / 耗时除外），用来做「同一拓扑
// 两种装法」的逐字段比对。并行节点的记录顺序本就不定，所以排序后比。
func canonRecords(recs []Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		node, _ := Get[string](r.Attrs, pulse.AttrNode)
		graph, _ := Get[string](r.Attrs, pulse.AttrGraph)
		errName := "<nil>"
		if r.Err != nil {
			errName = r.Err.Error()
		}
		out = append(out, fmt.Sprintf("%s|node=%s|graph=%s|status=%s|err=%s|host=%s|trace=%s|source=%s",
			r.Event, node, graph, r.Status, errName, r.HostID, r.TraceID, r.Source))
	}
	sort.Strings(out)
	return out
}
