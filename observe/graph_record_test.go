package observe

import (
	"context"
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
	if len(log) != 3 {
		t.Fatalf("host observer saw %v, want W/R/F", log)
	}
	if len(sink.Snapshot()) != 2 {
		t.Fatalf("records = %d, want 2", len(sink.Snapshot()))
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
