package flow

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/observability"
)

func testObsCfg(sink *observability.MemorySink) observability.ObserveConfig {
	return observability.ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-f"}
}

// 分段锚：线性节点 wait/run 两条记录，run 段 Duration>0，Status 分别为
// running/completed，nodeID 走 AttrNode 且具名字段保持零值。
func TestRecordObserverSegments(t *testing.T) {
	sink := &observability.MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	in := NewKey[string]("obs.in")
	out := NewKey[string]("obs.out")
	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	if err := Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("n", Requires(in), Provides(out), func(rc *RunCtx) error {
		time.Sleep(2 * time.Millisecond)
		v, err := Get(rc, in)
		if err != nil {
			return err
		}
		return Set(rc, out, v)
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
			if rec.Status != string(NodeCompleted) {
				t.Fatalf("run status = %q, want completed", rec.Status)
			}
			if rec.Duration <= 0 {
				t.Fatal("run segment duration should be > 0")
			}
		}
		if rec.Event == EventNodeWaitFinished || rec.Event == EventNodeRunFinished {
			if rec.HostID != "h" || rec.TraceID != "tr-f" || rec.Source != observability.SourceAdapter {
				t.Fatalf("envelope mismatch: %+v", rec)
			}
			if v, ok := observability.Get[string](rec.Attrs, AttrNode); !ok || v != "n" {
				t.Fatalf("node attr: %q %v", v, ok)
			}
			if rec.FiberName != "" {
				t.Fatalf("must not use named field FiberName: %+v", rec)
			}
		}
	}
	if waits != 1 || runs != 1 {
		t.Fatalf("segments waits=%d runs=%d, want 1/1", waits, runs)
	}
}

// skip 锚：SkipSeed 下游只有一条 skipped 等待记录，无运行记录。
func TestRecordObserverSkip(t *testing.T) {
	sink := &observability.MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	a := NewKey[string]("obs.sa")
	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	if err := SkipSeed(g, a); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("down", Requires(a), nil, func(rc *RunCtx) error {
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
			if rec.Status != string(NodeSkipped) {
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
	sink := &observability.MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	k1 := NewKey[string]("obs.c1")
	k2 := NewKey[string]("obs.c2")
	k3 := NewKey[string]("obs.c3")
	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	if err := Seed(g, k1, "x"); err != nil {
		t.Fatal(err)
	}
	pass := func(req, ack Key[string]) func(*RunCtx) error {
		return func(rc *RunCtx) error {
			v, err := Get(rc, req)
			if err != nil {
				return err
			}
			return Set(rc, ack, v)
		}
	}
	if err := g.Add(NewNode("extract", Requires(k1), Provides(k2), pass(k1, k2))); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("answer", Requires(k2), Provides(k3), pass(k2, k3))); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	waitNodes := map[string]int{}
	runNodes := map[string]int{}
	for _, rec := range sink.Snapshot() {
		id, _ := observability.Get[string](rec.Attrs, AttrNode)
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
	sink := &observability.MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	host := &recordingObserver{}
	in := NewKey[string]("obs.cc.in")
	out := NewKey[string]("obs.cc.out")
	g := mustNew(t, context.Background(), "test", WithObserver(MultiObserver{obs, host}))
	if err := Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("n", Requires(in), Provides(out), func(rc *RunCtx) error {
		v, err := Get(rc, in)
		if err != nil {
			return err
		}
		return Set(rc, out, v)
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
	if _, err := NewRecordObserver(observability.ObserveConfig{TraceID: "t"}); err != observability.ErrNilSink {
		t.Fatalf("nil sink err = %v", err)
	}
}

// 归因锚（#144）：同 sink 并发双图——同名节点跨图复用，图 ID 折为
// AttrGraph 区分归属；-race 下 RecordObserver 无共享竞态。
func TestRecordObserverConcurrentGraphAttribution(t *testing.T) {
	sink := &observability.MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	in := NewKey[string]("obs.g.in")
	out := NewKey[string]("obs.g.out")

	var wg sync.WaitGroup
	for _, graphID := range []string{"ga", "gb"} {
		g := mustNew(t, context.Background(), graphID, WithObserver(obs))
		if err := Seed(g, in, "x"); err != nil {
			t.Fatal(err)
		}
		if err := g.Add(NewNode("n", Requires(in), Provides(out), func(rc *RunCtx) error {
			time.Sleep(time.Millisecond)
			v, err := Get(rc, in)
			if err != nil {
				return err
			}
			return Set(rc, out, v)
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
		gid, ok := observability.Get[string](rec.Attrs, AttrGraph)
		if !ok {
			t.Fatalf("record missing %s attr: %+v", AttrGraph, rec)
		}
		if v, ok := observability.Get[string](rec.Attrs, AttrNode); !ok || v != "n" {
			t.Fatalf("node attr: %q %v", v, ok)
		}
		counts[gid]++
	}
	if counts["ga"] != 1 || counts["gb"] != 1 {
		for i, rec := range sink.Snapshot() {
			g, _ := observability.Get[string](rec.Attrs, AttrGraph)
			n, _ := observability.Get[string](rec.Attrs, AttrNode)
			t.Logf("rec[%d] event=%s graph=%q node=%q status=%s", i, rec.Event, g, n, rec.Status)
		}
		t.Fatalf("graph attribution = %v, want ga=1 gb=1", counts)
	}
}
