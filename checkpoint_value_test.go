package pulse

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func TestCheckpointBusinessContextErrorConfirmsRootFailure(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "business-context-error")
			var calls int
			mustAdd(t, g, NewNode("n", "独立RPC失败不是宿主中断", nil, nil, func(*RunCtx) error {
				calls++
				return fmt.Errorf("independent RPC failed: %w", cause)
			}))
			p := &checkpointProbe{}
			if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); err == nil {
				t.Fatal("first failure lost")
			}
			if ctx.Err() != nil || len(p.failures) != 1 {
				t.Fatalf("live host did not confirm failure: ctx=%v failures=%v", ctx.Err(), p.failures)
			}
			if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); err == nil {
				t.Fatal("exhausted root succeeded")
			}
			if calls != 1 {
				t.Fatalf("final failed body reran %d times", calls)
			}
		})
	}
}

func TestCheckpointNestedDynamicChannelsFailClosed(t *testing.T) {
	ch := make(chan int)
	cyclic := map[string]any{}
	cyclic["self"], cyclic["channel"] = cyclic, ch
	values := []any{[]any{ch}, map[string]any{"channel": ch}, struct{ Payload any }{ch}, cyclic}
	for i, value := range values {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "nested-dynamic-channel")
			k := NewKey[any]("k")
			mustAdd(t, g, NewNode("n", "接口藏在容器里面", nil, Provides(k), func(rc *RunCtx) error { return Set[any](rc, k, value) }))
			p := &checkpointProbe{}
			e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks()))
			if err != nil {
				t.Fatal(err)
			}
			bad := []NodeCheckpoint{{NodeID: "n", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: CheckpointKey{Name: "k", Type: reflect.TypeFor[any]()}, State: SlotStateReady, Value: value}}}}
			if err := e.RestoreNodes(bad); err == nil {
				t.Fatal("channel entered a restore plan")
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			if err := e.Wait(); err == nil || len(p.commits) != 0 {
				t.Fatalf("channel reached commit: %v", err)
			}
		})
	}
}

func TestCheckpointValueCyclesWithoutChannelsRemainLegal(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "normal-cycle")
	k := NewKey[any]("k")
	m := map[string]any{}
	m["self"] = m
	var slice []any
	slice = make([]any, 1)
	slice[0] = slice
	value := []any{m, slice}
	mustAdd(t, g, NewNode("n", "业务对象可以自指", nil, Provides(k), func(rc *RunCtx) error { return Set[any](rc, k, value) }))
	p := &checkpointProbe{}
	if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); err != nil {
		t.Fatal(err)
	}
	if len(p.commits) != 1 {
		t.Fatal("non-channel cycles were over-rejected")
	}
}
