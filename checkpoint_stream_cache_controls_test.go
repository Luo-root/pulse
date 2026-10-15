package pulse

import (
	"fmt"
	"reflect"
	"testing"
)

func TestCheckpointPendingScopeSharesOneAuthoritativeOpen(t *testing.T) {
	for _, shape := range []string{"old", "current", "old-empty-terminal", "current-empty-terminal", "future"} {
		t.Run(shape, func(t *testing.T) {
			err, opens, same, repeated, cached := independentLateScope(t, func(descriptor CheckpointStreamDescriptor) CheckpointStreamDescriptor {
				switch shape {
				case "current", "current-empty-terminal":
					descriptor.ID.Generation = 2
				case "future":
					descriptor.ID.Generation = 3
				}
				if shape == "old-empty-terminal" || shape == "current-empty-terminal" {
					descriptor.Terminal = ""
				}
				return descriptor
			}, true)
			t.Logf("Wait=%v Open calls=%d same endpoint=%t repeated delivery=%d cache entries=%d", err, opens, same, repeated, cached)
			if shape == "future" {
				if !isCheckpointFailure(err) || opens != 1 || cached != 1 {
					t.Fatal("pending later reference invented an authoritative future generation")
				}
			} else if err != nil || opens != 1 || !same || repeated != 0 || cached != 1 {
				t.Fatal("pending scopes did not share one Open and delivery ledger")
			}
		})
	}
}

func TestCheckpointLateScopeHistoricalEmptyTerminal(t *testing.T) {
	for _, current := range []bool{false, true} {
		t.Run(fmt.Sprintf("current-generation-%t", current), func(t *testing.T) {
			err, opens, same, repeated, cached := independentLateScope(t, func(descriptor CheckpointStreamDescriptor) CheckpointStreamDescriptor {
				descriptor.Terminal = ""
				if current {
					descriptor.ID.Generation = 2
				}
				return descriptor
			})
			if err != nil || opens != 1 || !same || repeated != 0 || cached != 1 {
				t.Fatalf("historical empty terminal did not reuse the authoritative source: err=%v opens=%d same=%t repeated=%d cached=%d", err, opens, same, repeated, cached)
			}
		})
	}
}

func TestCheckpointSameBatchReferencesRejectBeforeReplacingPlan(t *testing.T) {
	for _, field := range []string{"source", "attempt", "graph", "path", "capacity", "terminal", "generation"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse-%t", field, reverse), func(t *testing.T) {
				ctx := checkpointTestContext(t)
				root, child := mustNew(t, ctx, "batch-root"), mustNew(t, ctx, "batch-child")
				original, inlet, alias := NewKey[Stream[int]]("original"), NewKey[Stream[int]]("inlet"), NewKey[Stream[int]]("alias")
				mustAdd(t, root, NewNode("source", "", nil, Provides(original), nil))
				declareIn(t, child, NewPort(inlet))
				declareOut(t, child, NewPort(inlet))
				if err := Sub(root, "pass", "", []SubBind{In(original, inlet), Out(inlet, alias)}, child); err != nil {
					t.Fatal(err)
				}
				descriptor := CheckpointStreamDescriptor{
					ID:    CheckpointStreamID{Scope: "root", NodeID: "source", Key: "original", Group: "default", Generation: 1},
					Graph: "batch-root", Attempt: "source-attempt", Element: reflect.TypeFor[int](),
					Source: CheckpointStreamSource{ID: "finite", Resumable: true}, Capacity: 1, Terminal: StreamCompleted,
				}
				valid := []NodeCheckpoint{
					{NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(original.asRef()), State: SlotStateReady, Stream: &descriptor}}},
					{NodeID: "pass", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(alias.asRef()), State: SlotStateReady, Stream: &descriptor}}},
				}
				e, err := root.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", checkpointStreamTestHooks(&checkpointProbe{})))
				if err != nil {
					t.Fatal(err)
				}
				defer e.Cancel()
				if err := e.RestoreNodes(valid); err != nil {
					t.Fatal(err)
				}
				bad := copyCheckpoints(valid)
				altered := bad[1].Outputs[0].Stream
				switch field {
				case "source":
					altered.Source.ID = "forged"
				case "attempt":
					altered.Attempt = "forged"
				case "graph":
					altered.Graph = "forged"
				case "path":
					altered.Path = "forged"
				case "capacity":
					altered.Capacity++
				case "terminal":
					altered.Terminal, altered.Stopped = StreamStopped, true
				case "generation":
					altered.ID.Generation++
				}
				if reverse {
					bad[0], bad[1] = bad[1], bad[0]
				}
				if err := e.RestoreNodes(bad); err == nil {
					t.Fatal("conflicting same-batch reference accepted")
				}
				if !reflect.DeepEqual(e.restore, valid) || len(e.restored) != 0 || e.started || root.sealed {
					t.Fatal("invalid batch replaced accepted plan or installed restoration")
				}
				for _, key := range []Key[Stream[int]]{original, alias} {
					if state, _ := e.slotOf(key.asRef()).snapshot(); state != slotPending {
						t.Fatal("invalid batch resolved a slot")
					}
				}
			})
		}
	}
}
