package pulse

import (
	"context"
	"reflect"
	"testing"
)

func TestCheckpointStoppedTerminalRequiresStoppedGroup(t *testing.T) {
	ctx := checkpointTestContext(t)
	root, child := mustNew(t, ctx, "invalid-stop-root"), mustNew(t, ctx, "invalid-stop-child")
	original, inlet, alias := NewKey[Stream[int]]("original"), NewKey[Stream[int]]("inlet"), NewKey[Stream[int]]("alias")
	mustAdd(t, root, NewNode("source", "", nil, Provides(original), nil))
	declareIn(t, child, NewPort(inlet))
	declareOut(t, child, NewPort(inlet))
	if err := Sub(root, "pass", "", []SubBind{In(original, inlet), Out(inlet, alias)}, child); err != nil {
		t.Fatal(err)
	}
	invalid := CheckpointStreamDescriptor{
		ID:    CheckpointStreamID{Scope: "root", NodeID: "source", Key: "original", Group: "default", Generation: 1},
		Graph: "invalid-stop-root", Attempt: "source-attempt", Element: reflect.TypeFor[int](),
		Source: CheckpointStreamSource{ID: "finite", Resumable: true}, Capacity: 1, Terminal: StreamStopped,
	}
	request := CheckpointStreamOpen{Site: Site{Graph: invalid.Graph}, Scope: invalid.ID.Scope, Attempt: invalid.Attempt,
		NodeID: invalid.ID.NodeID, Key: checkpointKey(original.asRef()), Group: invalid.ID.Group,
		Source: invalid.Source, Capacity: invalid.Capacity, Restore: &invalid}
	if err := checkCheckpointStreamRecord(request, CheckpointStreamRecord{Stream: invalid, Resumed: true}); err == nil {
		t.Error("Open accepted Terminal=stopped while Stopped=false")
	}
	valid := invalid
	valid.Stopped = true
	hooks := checkpointStreamTestHooks(&checkpointProbe{})
	hooks.StreamHooks.Open = func(context.Context, CheckpointStreamOpen) (CheckpointStreamRecord, error) {
		return CheckpointStreamRecord{Stream: valid, Resumed: true}, nil
	}
	e, err := root.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cancel()
	accepted := []NodeCheckpoint{
		{NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(original.asRef()), State: SlotStateReady, Stream: &valid}}},
		{NodeID: "pass", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(alias.asRef()), State: SlotStateReady, Stream: &valid}}},
	}
	if err := e.RestoreNodes(accepted); err != nil {
		t.Fatal(err)
	}
	for _, reverse := range []bool{false, true} {
		plan := copyCheckpoints(accepted)
		plan[0].Outputs[0].Stream = &invalid
		if reverse {
			plan[0], plan[1] = plan[1], plan[0]
		}
		if err := e.RestoreNodes(plan); err == nil {
			t.Errorf("history merge masked stopped/false with stopped/true (reverse=%t)", reverse)
		}
		if !reflect.DeepEqual(e.restore, accepted) || len(e.restored) != 0 || e.started || root.sealed {
			t.Error("invalid stream state replaced the accepted plan or installed restoration")
		}
		for _, key := range []Key[Stream[int]]{original, alias} {
			if state, _ := e.slotOf(key.asRef()).snapshot(); state != slotPending {
				t.Error("invalid stream state resolved a slot")
			}
		}
	}
	for _, terminal := range []StreamTerminal{StreamCompleted, StreamStopped} {
		valid.Terminal = terminal
		if err := checkCheckpointStreamRecord(request, CheckpointStreamRecord{Stream: valid, Resumed: true}); terminal == StreamStopped && err != nil {
			t.Fatalf("legitimate stopped writer rejected: %v", err)
		}
		if err := checkCheckpointStreamDescriptor(checkpointKey(original.asRef()), valid); err != nil {
			t.Fatalf("legitimate terminal/reader-stop pair rejected: %v", err)
		}
	}
}
