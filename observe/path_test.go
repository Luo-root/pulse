package observe

import (
	"testing"

	"github.com/Luo-root/pulse"
)

// #294：`pulse.path` 由**引擎给值**（`SubCtx.Path()`）、**出口写记录**
// （`ObserveConfig.Path`）——引擎不认识本包，记录长什么样是本包的事。
func TestPathWrittenOnEveryRecord(t *testing.T) {
	sink := &MemorySink{}
	cfg := testObsCfg(sink)
	cfg.Path = "step1"
	obs, err := NewRecordObserver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	in := pulse.NewKey[string]("path.in")
	g := mustGraph(t, "P", pulse.WithObserver(obs))
	if err := pulse.Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(pulse.NewNode("n", pulse.Requires(in), nil, func(rc *pulse.RunCtx) error {
		_, err := pulse.Get(rc, in)
		return err
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	recs := sink.Snapshot()
	if len(recs) != 4 { // 运行级两条 + 节点两条
		t.Fatalf("记录数 = %d，want 4：%v", len(recs), eventsOf(recs))
	}
	// **每一条**都带：运行级两条也算（它们同样属于这一层）。
	for _, rec := range recs {
		got, ok := Get[string](rec.Attrs, pulse.AttrPath)
		if !ok || got != "step1" {
			t.Fatalf("%s 这条应当带 %s=step1，实得 (%q, %v)", rec.Event, pulse.AttrPath, got, ok)
		}
	}
}

// 空串**不写**这个 key：`Attrs` 是「有才有」，写空串会让「根」与「忘了传」
// 长得一样，出口就没法用它判层级。
func TestPathEmptyIsNotWritten(t *testing.T) {
	sink := &MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink)) // testObsCfg 不设 Path
	if err != nil {
		t.Fatal(err)
	}
	in := pulse.NewKey[string]("path0.in")
	g := mustGraph(t, "P", pulse.WithObserver(obs))
	if err := pulse.Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(pulse.NewNode("n", pulse.Requires(in), nil, func(rc *pulse.RunCtx) error {
		_, err := pulse.Get(rc, in)
		return err
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	for _, rec := range sink.Snapshot() {
		if v, ok := Get[string](rec.Attrs, pulse.AttrPath); ok {
			t.Fatalf("Path 为空时不该写 %s（实得 %q）：%s", pulse.AttrPath, v, rec.Event)
		}
	}
}

// 同一张子图模板跑两遍 = 两个图实例：记录靠 `pulse.path` 分得开，且**每个图
// 实例自己的**次数契约各自成立（冻结面按图实例算，不跨实例累加）。
func TestPathSeparatesTwoInstances(t *testing.T) {
	sink := &MemorySink{}
	parent, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	in := pulse.NewKey[string]("np.in")
	cin := pulse.NewKey[string]("np.cin")
	cout := pulse.NewKey[string]("np.cout")
	outA := pulse.NewKey[string]("np.outA")
	outB := pulse.NewKey[string]("np.outB")

	g := mustGraph(t, "P", pulse.WithObserver(parent))
	if err := pulse.Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}

	// 同一个模板，两个实例：出口按层各建一个（`build` 里正合适）。
	build := func(sc *pulse.SubCtx) (*pulse.Graph, error) {
		childObs, err := NewRecordObserver(ObserveConfig{
			Sink: sink, HostID: "h", TraceID: "tr-f", Path: sc.Path(),
		})
		if err != nil {
			return nil, err
		}
		child, err := pulse.New(sc.Context(), sc.GraphID(), pulse.WithObserver(childObs))
		if err != nil {
			return nil, err
		}
		if err := pulse.Seed(child, cin, "x"); err != nil {
			return nil, err
		}
		return child, child.Add(pulse.NewNode("inner", pulse.Requires(cin), pulse.Provides(cout),
			func(rc *pulse.RunCtx) error {
				v, err := pulse.Get(rc, cin)
				if err != nil {
					return err
				}
				return pulse.Set(rc, cout, v)
			}))
	}
	if err := pulse.Sub(g, "a", []pulse.SubBind{pulse.In(in, cin), pulse.Out(cout, outA)}, build); err != nil {
		t.Fatal(err)
	}
	if err := pulse.Sub(g, "b", []pulse.SubBind{pulse.In(in, cin), pulse.Out(cout, outB)}, build); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	byPath := map[string][]Record{}
	for _, rec := range sink.Snapshot() {
		p, _ := Get[string](rec.Attrs, pulse.AttrPath) // 没有这个 attr → 空桶（父图自己的记录）
		byPath[p] = append(byPath[p], rec)
	}
	for _, tc := range []struct{ path, graphID string }{{"a", "P/a"}, {"b", "P/b"}} {
		recs := byPath[tc.path]
		if len(recs) != 4 {
			t.Fatalf("path=%q 的记录数 = %d，want 4（运行级两条 + 节点两条）：%v",
				tc.path, len(recs), eventsOf(recs))
		}
		var started, finished, waits, runs int
		for _, rec := range recs {
			if gid, _ := Get[string](rec.Attrs, pulse.AttrGraph); gid != tc.graphID {
				t.Fatalf("path=%q 的图归因 = %q，want %q", tc.path, gid, tc.graphID)
			}
			switch rec.Event {
			case EventGraphStarted:
				started++
			case EventGraphFinished:
				finished++
			case EventNodeWaitFinished:
				waits++
			case EventNodeRunFinished:
				runs++
				if nid, _ := Get[string](rec.Attrs, pulse.AttrNode); nid != "inner" {
					t.Fatalf("path=%q 的节点归因 = %q，want inner", tc.path, nid)
				}
			}
		}
		if started != 1 || finished != 1 || waits != 1 || runs != 1 {
			t.Fatalf("path=%q 的次数契约：started=%d finished=%d wait=%d run=%d，want 各 1",
				tc.path, started, finished, waits, runs)
		}
	}
	// 父图自己的记录（两条运行级 + 两个 Sub 节点的四条）：不带 path。
	parentRecs := byPath[""]
	if len(parentRecs) != 6 {
		t.Fatalf("父图记录数 = %d，want 6（运行级 2 + 两个节点各 2）：%v",
			len(parentRecs), eventsOf(parentRecs))
	}
	for _, rec := range parentRecs {
		if _, ok := Get[string](rec.Attrs, pulse.AttrPath); ok {
			t.Fatalf("父图自己的记录不该带 %s：%s", pulse.AttrPath, rec.Event)
		}
	}
}

// 一条守卫：本文件里那个「没有 attr → 空桶」的约定，靠的是 Attrs 的两值读取
// （`Get` 的第二返回值），不是「空串等于没有」——把这条写成断言，免得以后有人
// 改成写空串还以为没差别。
func TestGetDistinguishesAbsentFromEmpty(t *testing.T) {
	var rec Record
	Set(&rec.Attrs, pulse.AttrPath, "")
	if v, ok := Get[string](rec.Attrs, pulse.AttrPath); !ok || v != "" {
		t.Fatalf("显式写入空串应当 ok=true：(%q, %v)", v, ok)
	}
	var other Record
	if _, ok := Get[string](other.Attrs, pulse.AttrPath); ok {
		t.Fatal("没写过这个 key 应当 ok=false")
	}
}
