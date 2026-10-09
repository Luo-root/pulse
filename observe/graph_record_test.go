package observe

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse"
)

func testObsCfg(sink *MemorySink) ObserveConfig {
	return ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-f"}
}

// mustGraph 建图；本文件只关心观测折叠，建图失败即 Fatal。
func mustGraph(t *testing.T, id string, opts ...pulse.Option) *pulse.Graph {
	t.Helper()
	g, err := pulse.New(context.Background(), id, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// recordingObserver 是宿主自有 Observer 的最小实现（组合锚用）。
type recordingObserver struct {
	mu  sync.Mutex
	log []string
}

func (o *recordingObserver) OnGraphStarted(_ string) { o.add("GS") }
func (o *recordingObserver) OnGraphFinished(_ string, _ pulse.NodeFinishReason, _ error) {
	o.add("GF")
}
func (o *recordingObserver) OnNodeWaiting(_, _ string) { o.add("W") }
func (o *recordingObserver) OnNodeRunning(_, _ string) { o.add("R") }
func (o *recordingObserver) OnNodeFinished(_, _ string, _ pulse.NodeFinishReason, _ error) {
	o.add("F")
}

func (o *recordingObserver) add(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.log = append(o.log, s)
}

func (o *recordingObserver) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.log...)
}

// 分段锚：线性节点 wait/run 两条记录，run 段 Duration>0，Status 分别为
// running/completed，nodeID 走 AttrNode 且具名字段保持零值。
func TestRecordObserverSegments(t *testing.T) {
	sink := &MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	in := pulse.NewKey[string]("obs.in")
	out := pulse.NewKey[string]("obs.out")
	g := mustGraph(t, "test", pulse.WithObserver(obs))
	if err := pulse.Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(pulse.NewNode("n", pulse.Requires(in), pulse.Provides(out), func(rc *pulse.RunCtx) error {
		time.Sleep(2 * time.Millisecond)
		v, err := pulse.Get(rc, in)
		if err != nil {
			return err
		}
		return pulse.Set(rc, out, v)
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	var waits, runs int
	for _, rec := range sink.Snapshot() {
		switch rec.Event {
		case EventNodeWaitFinished:
			waits++
			if rec.Status != "running" {
				t.Fatalf("wait status = %q, want running", rec.Status)
			}
		case EventNodeRunFinished:
			runs++
			if rec.Status != string(pulse.NodeCompleted) {
				t.Fatalf("run status = %q, want completed", rec.Status)
			}
			if rec.Duration <= 0 {
				t.Fatal("run segment duration should be > 0")
			}
		}
		if rec.Event == EventNodeWaitFinished || rec.Event == EventNodeRunFinished {
			if rec.HostID != "h" || rec.TraceID != "tr-f" || rec.Source != SourceObserver {
				t.Fatalf("envelope mismatch: %+v", rec)
			}
			if v, ok := Get[string](rec.Attrs, pulse.AttrNode); !ok || v != "n" {
				t.Fatalf("node attr: %q %v", v, ok)
			}
		}
	}
	if waits != 1 || runs != 1 {
		t.Fatalf("segments waits=%d runs=%d, want 1/1", waits, runs)
	}
}

// skip 锚：SkipSeed 下游只有一条 skipped 等待记录，无运行记录。
func TestRecordObserverSkip(t *testing.T) {
	sink := &MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	a := pulse.NewKey[string]("obs.sa")
	g := mustGraph(t, "test", pulse.WithObserver(obs))
	if err := pulse.SkipSeed(g, a); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(pulse.NewNode("down", pulse.Requires(a), nil, func(rc *pulse.RunCtx) error {
		t.Fatal("must not run")
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	var waits, runs int
	for _, rec := range sink.Snapshot() {
		switch rec.Event {
		case EventNodeWaitFinished:
			waits++
			if rec.Status != string(pulse.NodeSkipped) {
				t.Fatalf("skip wait status = %q, want skipped", rec.Status)
			}
		case EventNodeRunFinished:
			runs++
		}
	}
	if waits != 1 || runs != 0 {
		t.Fatalf("skip segments waits=%d runs=%d, want 1/0", waits, runs)
	}
}

// 双节点锚：wait/run 按 nodeID（AttrNode）严格区分、各恰一条。
func TestRecordObserverTwoNodeIdentities(t *testing.T) {
	sink := &MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	k1 := pulse.NewKey[string]("obs.c1")
	k2 := pulse.NewKey[string]("obs.c2")
	k3 := pulse.NewKey[string]("obs.c3")
	g := mustGraph(t, "test", pulse.WithObserver(obs))
	if err := pulse.Seed(g, k1, "x"); err != nil {
		t.Fatal(err)
	}
	pass := func(req, ack pulse.Key[string]) func(*pulse.RunCtx) error {
		return func(rc *pulse.RunCtx) error {
			v, err := pulse.Get(rc, req)
			if err != nil {
				return err
			}
			return pulse.Set(rc, ack, v)
		}
	}
	if err := g.Add(pulse.NewNode("extract", pulse.Requires(k1), pulse.Provides(k2), pass(k1, k2))); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(pulse.NewNode("answer", pulse.Requires(k2), pulse.Provides(k3), pass(k2, k3))); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	waitNodes := map[string]int{}
	runNodes := map[string]int{}
	for _, rec := range sink.Snapshot() {
		id, _ := Get[string](rec.Attrs, pulse.AttrNode)
		switch rec.Event {
		case EventNodeWaitFinished:
			waitNodes[id]++
		case EventNodeRunFinished:
			runNodes[id]++
		}
	}
	for _, id := range []string{"extract", "answer"} {
		if waitNodes[id] != 1 || runNodes[id] != 1 {
			t.Fatalf("node %s waits=%d runs=%d; waitMap=%v runMap=%v",
				id, waitNodes[id], runNodes[id], waitNodes, runNodes)
		}
	}
}

// 组合锚：与宿主自有 Observer 经 MultiObserver 组合互不干扰。
func TestRecordObserverCombinedWithHostObserver(t *testing.T) {
	sink := &MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	host := &recordingObserver{}
	in := pulse.NewKey[string]("obs.cc.in")
	out := pulse.NewKey[string]("obs.cc.out")
	g := mustGraph(t, "test", pulse.WithObserver(pulse.MultiObserver{obs, host}))
	if err := pulse.Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(pulse.NewNode("n", pulse.Requires(in), pulse.Provides(out), func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, in)
		if err != nil {
			return err
		}
		return pulse.Set(rc, out, v)
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	log := host.snapshot()
	// 宿主自有 observer 与 observe 适配器看到的是同一批事件：五条，图级两条在两头。
	want := []string{"GS", "W", "R", "F", "GF"}
	if len(log) != len(want) {
		t.Fatalf("host observer saw %v, want %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("host observer saw %v, want %v", log, want)
		}
	}
	if len(sink.Snapshot()) != 4 {
		t.Fatalf("records = %d, want 4（图级两条 + 节点两条）", len(sink.Snapshot()))
	}
}

// 装配校验：nil Sink 哨兵。
func TestRecordObserverNilSink(t *testing.T) {
	if _, err := NewRecordObserver(ObserveConfig{TraceID: "t"}); err != ErrNilSink {
		t.Fatalf("nil sink err = %v", err)
	}
}

// 归因锚（#144）：同 sink 并发双图——同名节点跨图复用，图 ID 折为
// AttrGraph 区分归属；-race 下 RecordObserver 无共享竞态。
func TestRecordObserverConcurrentGraphAttribution(t *testing.T) {
	sink := &MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	in := pulse.NewKey[string]("obs.g.in")
	out := pulse.NewKey[string]("obs.g.out")

	var wg sync.WaitGroup
	for _, graphID := range []string{"ga", "gb"} {
		g := mustGraph(t, graphID, pulse.WithObserver(obs))
		if err := pulse.Seed(g, in, "x"); err != nil {
			t.Fatal(err)
		}
		if err := g.Add(pulse.NewNode("n", pulse.Requires(in), pulse.Provides(out), func(rc *pulse.RunCtx) error {
			time.Sleep(time.Millisecond)
			v, err := pulse.Get(rc, in)
			if err != nil {
				return err
			}
			return pulse.Set(rc, out, v)
		})); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.Run(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	counts := map[string]int{}
	for _, rec := range sink.Snapshot() {
		if rec.Event != EventNodeRunFinished {
			continue
		}
		gid, ok := Get[string](rec.Attrs, pulse.AttrGraph)
		if !ok {
			t.Fatalf("record missing %s attr: %+v", pulse.AttrGraph, rec)
		}
		if v, ok := Get[string](rec.Attrs, pulse.AttrNode); !ok || v != "n" {
			t.Fatalf("node attr: %q %v", v, ok)
		}
		counts[gid]++
	}
	if counts["ga"] != 1 || counts["gb"] != 1 {
		for i, rec := range sink.Snapshot() {
			g, _ := Get[string](rec.Attrs, pulse.AttrGraph)
			n, _ := Get[string](rec.Attrs, pulse.AttrNode)
			t.Logf("rec[%d] event=%s graph=%q node=%q status=%s", i, rec.Event, g, n, rec.Status)
		}
		t.Fatalf("graph attribution = %v, want ga=1 gb=1", counts)
	}
}

// 运行级锚：一轮恰好两条（started / finished）且夹住节点记录，整轮耗时与宿主在
// Run() 外掐的表同量级；归因只带图（节点维度对运行级事实没有意义）。
func TestRecordObserverGraphSegments(t *testing.T) {
	sink := &MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	in := pulse.NewKey[string]("obs.gs.in")
	out := pulse.NewKey[string]("obs.gs.out")
	g := mustGraph(t, "segment", pulse.WithObserver(obs))
	if err := pulse.Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(pulse.NewNode("n", pulse.Requires(in), pulse.Provides(out), func(rc *pulse.RunCtx) error {
		time.Sleep(5 * time.Millisecond)
		v, err := pulse.Get(rc, in)
		if err != nil {
			return err
		}
		return pulse.Set(rc, out, v)
	})); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	recs := sink.Snapshot()
	want := []string{EventGraphStarted, EventNodeWaitFinished, EventNodeRunFinished, EventGraphFinished}
	if len(recs) != len(want) {
		t.Fatalf("records = %d（%v），want %v", len(recs), eventsOf(recs), want)
	}
	for i := range want {
		if recs[i].Event != want[i] {
			t.Fatalf("records = %v, want %v", eventsOf(recs), want)
		}
	}

	first, last := recs[0], recs[len(recs)-1]
	if first.Status != "running" || first.Duration != 0 {
		t.Fatalf("started = %+v, want status=running duration=0", first)
	}
	if last.Status != string(pulse.NodeCompleted) || last.Err != nil {
		t.Fatalf("finished = %+v, want status=completed err=nil", last)
	}
	// 验收口径：与宿主在 Run() 外掐的表同一数量级。本轮节点体睡 5ms，两端多出来
	// 的开销都在微秒级，所以下界取一半已足够稳，又足以抓住「计时点挂错地方」。
	if last.Duration <= 0 || last.Duration > elapsed || last.Duration < elapsed/2 {
		t.Fatalf("finished.Duration = %v，宿主掐表 = %v：不同量级", last.Duration, elapsed)
	}
	for _, rec := range []Record{first, last} {
		if got, ok := Get[string](rec.Attrs, pulse.AttrGraph); !ok || got != "segment" {
			t.Fatalf("%s 图归因 = %q %v, want segment", rec.Event, got, ok)
		}
		if v, ok := Get[string](rec.Attrs, pulse.AttrNode); ok {
			t.Fatalf("%s 不该带节点维度: %q", rec.Event, v)
		}
		if rec.HostID != "h" || rec.TraceID != "tr-f" || rec.Source != SourceObserver {
			t.Fatalf("%s 信封与节点记录不同源: %+v", rec.Event, rec)
		}
	}
}

// 失败路径也收满两条：finished 的 Status=failed、Err 与 Run 的返回值同源；
// started 照发（这一轮确实开始了）。
func TestRecordObserverGraphFinishedFailed(t *testing.T) {
	sink := &MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("node boom")
	out := pulse.NewKey[string]("obs.gfail.out")
	g := mustGraph(t, "failgraph", pulse.WithObserver(obs))
	if err := g.Add(pulse.NewNode("bad", nil, pulse.Provides(out), func(rc *pulse.RunCtx) error {
		time.Sleep(2 * time.Millisecond) // 让整轮耗时量得出来（毫秒以内会被计时精度取整成 0）
		return boom
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); !errors.Is(err, boom) {
		t.Fatalf("Run err = %v, want node error", err)
	}

	var started, finished int
	var finErr error
	for _, rec := range sink.Snapshot() {
		switch rec.Event {
		case EventGraphStarted:
			started++
		case EventGraphFinished:
			finished++
			if rec.Status != string(pulse.NodeFailed) {
				t.Fatalf("status = %q, want failed", rec.Status)
			}
			if rec.Duration <= 0 {
				t.Fatal("失败轮也该有整轮耗时")
			}
			finErr = rec.Err
		}
	}
	if started != 1 || finished != 1 {
		t.Fatalf("started=%d finished=%d, want 1/1", started, finished)
	}
	if !errors.Is(finErr, boom) {
		t.Fatalf("finished.Err = %v, want node error", finErr)
	}
}

// 跳过是节点级的事实：一轮里全部节点都跳过，整轮仍算 completed——不因为「没人
// 干活」升格成 failed（Err 也不含 ErrSkipped）。
func TestRecordObserverGraphFinishedOnAllSkipped(t *testing.T) {
	sink := &MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	a := pulse.NewKey[string]("obs.allskip")
	g := mustGraph(t, "allskip", pulse.WithObserver(obs))
	if err := pulse.SkipSeed(g, a); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(pulse.NewNode("down", pulse.Requires(a), nil, func(rc *pulse.RunCtx) error {
		t.Fatal("must not run")
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	recs := sink.Snapshot()
	last := recs[len(recs)-1]
	if last.Event != EventGraphFinished || last.Status != string(pulse.NodeCompleted) || last.Err != nil {
		t.Fatalf("全跳过的一轮收尾 = %+v, want completed / nil", last)
	}
}

// 防御分支：没见过 started 就收到 finished（半路接上的实例）——记录照发，但不编造
// 整轮耗时（Duration **恰好** 0，不是「约等于 0」）：那个 0 是「不知道」，不是
// 「耗时为零」。（别在这个分支里塞 `time.Since(time.Now())`——Linux 上它会留下
// 90ns 级的残值，把「没量到」渲染成一个看着像真数字的值。）
func TestRecordObserverGraphFinishedWithoutStarted(t *testing.T) {
	sink := &MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	obs.OnGraphFinished("orphan", pulse.NodeCompleted, nil)

	recs := sink.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Event != EventGraphFinished || rec.Status != "completed" || rec.Duration != 0 {
		t.Fatalf("orphan finished = %+v", rec)
	}
	if got, ok := Get[string](rec.Attrs, pulse.AttrGraph); !ok || got != "orphan" {
		t.Fatalf("图归因 = %q %v, want orphan", got, ok)
	}
}

func eventsOf(recs []Record) []string {
	out := make([]string, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.Event)
	}
	return out
}
