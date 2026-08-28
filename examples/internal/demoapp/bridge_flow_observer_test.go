package demoapp

import (
	"context"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/observability"
)

func TestBridgeFlowObserverWritesWaitAndRunRecords(t *testing.T) {
	sink := &observability.MemorySink{}
	b := &Bridge{Sink: sink, HostID: "h", TraceID: "t-1"}
	peak := &FlowPeak{}

	in := flow.NewKey[string]("demo.obs.in")
	out := flow.NewKey[string]("demo.obs.out")
	g := flow.New(context.Background(), flow.WithObserver(b.FlowObserver(peak)))
	if err := flow.Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(flow.NewNode("n", flow.Requires(in), flow.Provides(out), func(rc *flow.RunCtx) error {
		time.Sleep(5 * time.Millisecond)
		v, err := flow.Get(rc, in)
		if err != nil {
			return err
		}
		return flow.Set(rc, out, v)
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	var waits, runs int
	for _, rec := range sink.Snapshot() {
		if rec.Source != observability.SourceBridge || rec.TraceID != "t-1" {
			continue
		}
		switch rec.Event {
		case EventFlowNodeWaitFinished:
			waits++
			if rec.FiberName != "n" {
				t.Fatalf("wait FiberName = %q, want n", rec.FiberName)
			}
			if rec.Status != "running" {
				t.Fatalf("wait status = %q", rec.Status)
			}
		case EventFlowNodeRunFinished:
			runs++
			if rec.FiberName != "n" {
				t.Fatalf("run FiberName = %q, want n", rec.FiberName)
			}
			if rec.Status != string(flow.NodeCompleted) {
				t.Fatalf("run status = %q", rec.Status)
			}
			if rec.Duration <= 0 {
				t.Fatal("run duration should be > 0")
			}
		}
	}
	if waits != 1 || runs != 1 {
		t.Fatalf("waits=%d runs=%d records=%v", waits, runs, sink.Snapshot())
	}
}

func TestBridgeFlowObserverSkipHasWaitNoRun(t *testing.T) {
	sink := &observability.MemorySink{}
	b := &Bridge{Sink: sink, HostID: "h", TraceID: "t-2"}
	a := flow.NewKey[string]("demo.obs.a")
	bKey := flow.NewKey[string]("demo.obs.b")
	g := flow.New(context.Background(), flow.WithObserver(b.FlowObserver(nil)))
	if err := flow.SkipSeed(g, a); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(flow.NewNode("down", flow.Requires(a), flow.Provides(bKey), func(rc *flow.RunCtx) error {
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
		case EventFlowNodeWaitFinished:
			waits++
			if rec.Status != string(flow.NodeSkipped) {
				t.Fatalf("wait status = %q", rec.Status)
			}
		case EventFlowNodeRunFinished:
			runs++
		}
	}
	if waits != 1 || runs != 0 {
		t.Fatalf("skip path waits=%d runs=%d", waits, runs)
	}
}

// 线性两节点链：wait/run 必须能按 FiberName（nodeID）区分。
func TestBridgeFlowObserverTwoNodeChainIdentities(t *testing.T) {
	sink := &observability.MemorySink{}
	b := &Bridge{Sink: sink, HostID: "h", TraceID: "t-3"}
	k1 := flow.NewKey[string]("demo.chain.1")
	k2 := flow.NewKey[string]("demo.chain.2")
	k3 := flow.NewKey[string]("demo.chain.3")
	g := flow.New(context.Background(), flow.WithObserver(b.FlowObserver(nil)))
	if err := flow.Seed(g, k1, "x"); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(flow.NewNode("extract", flow.Requires(k1), flow.Provides(k2), func(rc *flow.RunCtx) error {
		v, err := flow.Get(rc, k1)
		if err != nil {
			return err
		}
		return flow.Set(rc, k2, v)
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(flow.NewNode("answer", flow.Requires(k2), flow.Provides(k3), func(rc *flow.RunCtx) error {
		v, err := flow.Get(rc, k2)
		if err != nil {
			return err
		}
		return flow.Set(rc, k3, v)
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	waitNodes := map[string]int{}
	runNodes := map[string]int{}
	for _, rec := range sink.Snapshot() {
		if rec.Source != observability.SourceBridge {
			continue
		}
		switch rec.Event {
		case EventFlowNodeWaitFinished:
			waitNodes[rec.FiberName]++
		case EventFlowNodeRunFinished:
			runNodes[rec.FiberName]++
		}
	}
	for _, id := range []string{"extract", "answer"} {
		if waitNodes[id] != 1 || runNodes[id] != 1 {
			t.Fatalf("node %s waits=%d runs=%d; waitMap=%v runMap=%v",
				id, waitNodes[id], runNodes[id], waitNodes, runNodes)
		}
	}
}
