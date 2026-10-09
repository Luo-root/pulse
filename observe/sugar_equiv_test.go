package observe

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
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
