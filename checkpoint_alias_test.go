package pulse

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestCheckpointRestoredAliasesShareOneDeliveryLedger(t *testing.T) {
	ctx := checkpointTestContext(t)
	child, parent := streamTestGraph(t, "alias-child"), streamTestGraph(t, "alias-parent")
	inside := NewKey[Stream[int]]("inside")
	left, right := NewKey[Stream[int]]("left"), NewKey[Stream[int]]("right")
	streamTestMust(t, child.DeclareOutput(NewPort(inside)))
	streamTestMust(t, child.Add(NewNode("source", "", nil, Provides(inside), func(*RunCtx) error {
		return fmt.Errorf("restored completed source must not run")
	})))
	streamTestMust(t, Sub(parent, "mount", "", []SubBind{Out(inside, left), Out(inside, right)}, child))
	streamTestMust(t, parent.Add(NewNode("reader", "", Requires(left, right), nil, func(rc *RunCtx) error {
		a, err := Get(rc, left)
		if err != nil {
			return err
		}
		b, err := Get(rc, right)
		if err != nil {
			return err
		}
		message, err := a.Receive(rc)
		if err != nil || message.Value != 7 {
			return fmt.Errorf("first alias delivery = %v, %v", message.Value, err)
		}
		if err := message.Ack(); err != nil {
			return err
		}
		if _, err := b.Receive(rc); err != io.EOF {
			return fmt.Errorf("second alias repeated already acknowledged message: %v", err)
		}
		return nil
	})))
	probe := &checkpointProbe{}
	hooks := checkpointStreamTestHooks(probe)
	var opens atomic.Int32
	hooks.StreamHooks.Open = func(_ context.Context, request CheckpointStreamOpen) (CheckpointStreamRecord, error) {
		opens.Add(1)
		descriptor := *request.Restore
		descriptor.ID.Generation++
		return CheckpointStreamRecord{Stream: descriptor, LastSequence: 1,
			Pending: []CheckpointStreamMessage{{Sequence: 1, Value: 7}}, Resumed: true}, nil
	}
	e, err := parent.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	streamTestMust(t, err)
	defer e.Cancel()
	descriptor := CheckpointStreamDescriptor{
		ID:    CheckpointStreamID{Scope: "source-scope", NodeID: "source", Key: inside.Name(), Group: "default", Generation: 1},
		Graph: "alias-child", Path: "mount", Attempt: "source-attempt", Element: reflect.TypeFor[int](),
		Source: CheckpointStreamSource{ID: "numbers", Resumable: true}, Capacity: 1, Terminal: StreamCompleted,
	}
	outputs := []CheckpointOutput{
		{Key: checkpointKey(left.asRef()), State: SlotStateReady, Stream: &descriptor},
		{Key: checkpointKey(right.asRef()), State: SlotStateReady, Stream: &descriptor},
	}
	conflict := descriptor
	conflict.Capacity++
	invalid := append([]CheckpointOutput(nil), outputs...)
	invalid[1].Stream = &conflict
	if err := e.RestoreNodes([]NodeCheckpoint{{NodeID: "mount", Reason: NodeCompleted, Outputs: invalid}}); err == nil {
		t.Fatal("same stream aliases accepted conflicting descriptors")
	}
	streamTestMust(t, e.RestoreNodes([]NodeCheckpoint{{NodeID: "mount", Reason: NodeCompleted, Outputs: outputs}}))
	streamTestMust(t, e.Start())
	streamTestMust(t, e.Wait())
	if opens.Load() != 1 {
		t.Fatalf("aliases restored the same transport %d times", opens.Load())
	}
}
