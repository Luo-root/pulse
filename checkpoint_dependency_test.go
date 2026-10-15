package pulse

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

func TestCheckpointRestoreSubWithUnusedPendingInput(t *testing.T) {
	ctx := checkpointTestContext(t)
	g, child := mustNew(t, ctx, "pending-parent"), mustNew(t, ctx, "independent-child")
	optional, inside, result := NewKey[int]("optional"), NewKey[int]("inside"), NewKey[int]("result")
	declareIn(t, child, NewPort(optional))
	declareOut(t, child, NewPort(inside))
	var childRuns atomic.Int32
	mustAdd(t, child, NewNode("independent", "", nil, Provides(inside), func(rc *RunCtx) error {
		childRuns.Add(1)
		return Set(rc, inside, 42)
	}))
	if err := Sub(g, "mount", "", []SubBind{In(optional, optional), Out(inside, result)}, child); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	mustAdd(t, g, NewNode("late", "", nil, Provides(optional), func(rc *RunCtx) error {
		select {
		case <-release:
			return Set(rc, optional, 7)
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	}))
	declareOut(t, g, NewPort(result))
	saved := make(chan NodeCheckpoint, 1)
	hooks := (&checkpointProbe{}).hooks()
	hooks.Commit = func(_ context.Context, commit CheckpointCommit) error {
		if commit.Node.NodeID == "mount" {
			saved <- commit.Node
		}
		return nil
	}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	var record NodeCheckpoint
	select {
	case record = <-saved:
	case <-ctx.Done():
		t.Fatal("Sub waited for its unused input: ", ctx.Err())
	}
	e.Cancel()
	if err := e.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted run = %v", err)
	}
	restored, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", (&checkpointProbe{}).hooks()))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Cancel()
	if err := restored.RestoreNodes([]NodeCheckpoint{record}); err != nil {
		t.Fatalf("Core rejected its own completed Sub while only an unused In was pending: %v", err)
	}
	close(release)
	if err := restored.Start(); err != nil {
		t.Fatal(err)
	}
	if err := restored.Wait(); err != nil {
		t.Fatal(err)
	}
	if value, state, err := ReadOutput(restored, result); value != 42 || state != SlotStateReady || err != nil || childRuns.Load() != 1 {
		t.Fatalf("restored result = %v, %s, %v; child runs = %d", value, state, err, childRuns.Load())
	}
}

func TestCheckpointRestoreKeepsActualOrdinaryDependencies(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "ordinary-dependencies")
	input, output := NewKey[int]("input"), NewKey[int]("output")
	mustAdd(t, g, NewNode("source", "", nil, Provides(input), func(*RunCtx) error {
		return fmt.Errorf("restored source ran")
	}))
	mustAdd(t, g, NewNode("sink", "", Requires(input), Provides(output), func(*RunCtx) error {
		return fmt.Errorf("restored sink ran")
	}))
	declareOut(t, g, NewPort(output))
	valid := []NodeCheckpoint{
		{NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: CheckpointKey{Name: input.Name(), Type: input.asRef().typ}, State: SlotStateReady, Value: 7}}},
		{NodeID: "sink", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: CheckpointKey{Name: output.Name(), Type: output.asRef().typ}, State: SlotStateReady, Value: 42}}},
	}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", (&checkpointProbe{}).hooks()))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cancel()
	if err := e.RestoreNodes(valid); err != nil {
		t.Fatal(err)
	}
	if err := e.RestoreNodes(valid[1:]); err == nil {
		t.Fatal("missing actual ordinary producer accepted")
	}
	if e.started || g.sealed || len(e.restored) != 0 || len(e.restore) != 2 {
		t.Fatal("rejected batch changed the accepted plan or installed outputs")
	}
	if state, _ := e.slotOf(output.asRef()).snapshot(); state != slotPending {
		t.Fatal("rejected batch published a partial output")
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointRestoreShortCircuitedSub(t *testing.T) {
	for _, passThrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("pass-through-%t", passThrough), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g, child := mustNew(t, ctx, "short-parent"), mustNew(t, ctx, "short-child")
			input, output := NewKey[int]("input"), NewKey[int]("output")
			declareIn(t, child, NewPort(input))
			childOut := output
			if passThrough {
				childOut = input
			} else {
				mustAdd(t, child, NewNode("reader", "", Requires(input), Provides(output), func(*RunCtx) error {
					return fmt.Errorf("short-circuited child entered business")
				}))
			}
			declareOut(t, child, NewPort(childOut))
			short := func(*RunCtx, func(*RunCtx) error) error { return nil }
			if err := Sub(g, "mount", "", []SubBind{In(input, input), Out(childOut, output)}, child, short); err != nil {
				t.Fatal(err)
			}
			release := make(chan struct{})
			mustAdd(t, g, NewNode("late", "", nil, Provides(input), func(rc *RunCtx) error {
				select {
				case <-release:
					return Set(rc, input, 7)
				case <-rc.Context().Done():
					return rc.Context().Err()
				}
			}))
			probe := &checkpointProbe{}
			saved := make(chan NodeCheckpoint, 1)
			hooks := probe.hooks()
			hooks.Commit = func(_ context.Context, commit CheckpointCommit) error {
				if commit.Node.NodeID == "mount" {
					saved <- commit.Node
				}
				return nil
			}
			e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			var record NodeCheckpoint
			select {
			case record = <-saved:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			e.Cancel()
			_ = e.Wait()
			if record.Outputs[0].State != SlotStateSkipped {
				t.Fatal("short circuit did not save its implicit skip")
			}
			restored, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", (&checkpointProbe{}).hooks()))
			if err != nil {
				t.Fatal(err)
			}
			if err := restored.RestoreNodes([]NodeCheckpoint{record}); err != nil {
				t.Fatalf("completed Sub was required to prove an input that its Aspect bypassed: %v", err)
			}
			close(release)
			if err := restored.Start(); err != nil {
				t.Fatal(err)
			}
			if err := restored.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
