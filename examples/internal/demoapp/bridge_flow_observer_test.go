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
			if rec.Status != "running" {
				t.Fatalf("wait status = %q", rec.Status)
			}
		case EventFlowNodeRunFinished:
			runs++
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
