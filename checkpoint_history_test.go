package pulse

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCheckpointRestoreHistoricalPassThrough(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprintf("reader-stopped-%t", stopped), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g, child := mustNew(t, ctx, "history-parent"), mustNew(t, ctx, "history-child")
			source, inside, alias := NewKey[Stream[int]]("source"), NewKey[Stream[int]]("inside"), NewKey[Stream[int]]("alias")
			declareIn(t, child, NewPort(inside))
			declareOut(t, child, NewPort(inside))
			release := make(chan struct{})
			var releaseOnce sync.Once
			var sourceRuns atomic.Int32
			mustAdd(t, g, NewNode("writer", "", nil, Provides(source), func(rc *RunCtx) error {
				sourceRuns.Add(1)
				if _, err := OpenStream(rc, source, WithStreamSource("empty", true)); err != nil {
					return err
				}
				if !stopped {
					select {
					case <-release:
					case <-rc.Context().Done():
						return rc.Context().Err()
					}
				}
				return nil
			}))
			if stopped {
				mustAdd(t, child, NewNode("hold-alias", "", nil, nil, func(rc *RunCtx) error {
					select {
					case <-release:
						return nil
					case <-rc.Context().Done():
						return rc.Context().Err()
					}
				}))
			}
			if err := Sub(g, "mount", "", []SubBind{In(source, inside), Out(inside, alias)}, child); err != nil {
				t.Fatal(err)
			}
			mustAdd(t, g, NewNode("reader", "", Requires(alias), nil, func(rc *RunCtx) error {
				stream, err := Get(rc, alias)
				if err != nil {
					return err
				}
				if stopped {
					select {
					case <-stream.Done():
					case <-rc.Context().Done():
						return rc.Context().Err()
					}
					if err := stream.Stop(rc); err != nil {
						return err
					}
					releaseOnce.Do(func() { close(release) })
				}
				if _, err := stream.Receive(rc); err != io.EOF {
					return fmt.Errorf("completed history did not end at EOF: %v", err)
				}
				return nil
			}))
			probe := &checkpointProbe{}
			hooks := checkpointStreamTestHooks(probe)
			baseCommit := hooks.Commit
			mountSaved := make(chan struct{}, 1)
			var authoritative CheckpointStreamDescriptor
			hooks.Commit = func(ctx context.Context, commit CheckpointCommit) error {
				if commit.Node.NodeID == "writer" {
					authoritative = *commit.Node.Outputs[0].Stream
				}
				if err := baseCommit(ctx, commit); err != nil {
					return err
				}
				if commit.Node.NodeID == "mount" {
					mountSaved <- struct{}{}
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
			if !stopped {
				select {
				case <-mountSaved:
				case <-ctx.Done():
					t.Fatal("pass-through Sub waited for the actual writer: ", ctx.Err())
				}
				releaseOnce.Do(func() { close(release) })
			}
			if err := e.Wait(); err != nil {
				t.Fatal(err)
			}
			var records []NodeCheckpoint
			for _, commit := range probe.commits {
				if commit.Node.NodeID == "writer" || commit.Node.NodeID == "mount" {
					records = append(records, commit.Node)
				}
			}
			if len(records) != 2 {
				t.Fatalf("saved records = %+v", records)
			}
			for _, record := range records {
				descriptor := record.Outputs[0].Stream
				if record.NodeID == "mount" && ((!stopped && descriptor.Terminal != "") || (stopped && (!descriptor.Stopped || descriptor.Terminal != StreamCompleted))) {
					t.Fatalf("did not reproduce the historical alias: %+v", descriptor)
				}
				if record.NodeID == "writer" && (descriptor.Stopped || descriptor.Terminal != StreamCompleted) {
					t.Fatalf("writer history was not completed before reader Stop: %+v", descriptor)
				}
			}
			authoritative.Stopped = stopped
			for _, reverse := range []bool{false, true} {
				restoreHooks := checkpointStreamTestHooks(&checkpointProbe{})
				var opens atomic.Int32
				restoreHooks.StreamHooks.Open = func(_ context.Context, request CheckpointStreamOpen) (CheckpointStreamRecord, error) {
					opens.Add(1)
					if request.Restore == nil {
						return CheckpointStreamRecord{}, fmt.Errorf("restored source was reopened as new")
					}
					descriptor := authoritative
					descriptor.ID.Generation++
					return CheckpointStreamRecord{Stream: descriptor, Resumed: true}, nil
				}
				restored, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", restoreHooks))
				if err != nil {
					t.Fatal(err)
				}
				plan := copyCheckpoints(records)
				if reverse {
					plan[0], plan[1] = plan[1], plan[0]
				}
				if err := restored.RestoreNodes(plan); err != nil {
					t.Fatalf("Core rejected its own historical source/alias records: %v", err)
				}
				if err := restored.Start(); err != nil {
					t.Fatal(err)
				}
				if err := restored.Wait(); err != nil {
					t.Fatal(err)
				}
				if opens.Load() != 1 || sourceRuns.Load() != 1 {
					t.Fatalf("history re-created source: opens=%d runs=%d", opens.Load(), sourceRuns.Load())
				}
			}
		})
	}
}

func TestCheckpointHistoricalAliasUsesAuthoritativeTail(t *testing.T) {
	ctx := checkpointTestContext(t)
	g, child := mustNew(t, ctx, "history"), mustNew(t, ctx, "child")
	inside, alias := NewKey[Stream[int]]("inside"), NewKey[Stream[int]]("alias")
	mustAdd(t, child, NewNode("writer", "", nil, Provides(inside), func(*RunCtx) error {
		return fmt.Errorf("restored child writer reran")
	}))
	declareOut(t, child, NewPort(inside))
	if err := Sub(g, "mount", "", []SubBind{Out(inside, alias)}, child); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("reader", "", Requires(alias), nil, func(rc *RunCtx) error {
		stream, err := Get(rc, alias)
		if err != nil {
			return err
		}
		delivery, err := stream.Receive(rc)
		if err != nil || delivery.Value != 7 {
			return fmt.Errorf("historical tail = %v, %v", delivery.Value, err)
		}
		if err := delivery.Ack(); err != nil {
			return err
		}
		if _, err := stream.Receive(rc); err != io.EOF {
			return fmt.Errorf("historical tail did not reach EOF: %v", err)
		}
		return nil
	}))
	descriptor := checkpointHistoryDescriptor(inside)
	descriptor.Graph, descriptor.Path, descriptor.Terminal = "child", "mount", ""
	hooks := checkpointStreamTestHooks(&checkpointProbe{})
	var opens atomic.Int32
	hooks.StreamHooks.Open = func(_ context.Context, request CheckpointStreamOpen) (CheckpointStreamRecord, error) {
		opens.Add(1)
		if request.Restore == nil || request.Restore.Terminal != "" {
			return CheckpointStreamRecord{}, fmt.Errorf("historical alias pretended to prove writer completion")
		}
		authoritative := descriptor
		authoritative.ID.Generation++
		authoritative.Terminal = StreamCompleted
		return CheckpointStreamRecord{Stream: authoritative, LastSequence: 1, Pending: []CheckpointStreamMessage{{Sequence: 1, Value: 7}}, Resumed: true}, nil
	}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RestoreNodes([]NodeCheckpoint{{NodeID: "mount", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(alias.asRef()), State: SlotStateReady, Stream: &descriptor}}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 1 {
		t.Fatalf("historical alias opened %d transports", opens.Load())
	}
}

func checkpointHistoryDescriptor(key Key[Stream[int]]) CheckpointStreamDescriptor {
	return CheckpointStreamDescriptor{
		ID:    CheckpointStreamID{Scope: "root", NodeID: "writer", Key: key.Name(), Group: "default", Generation: 3},
		Graph: "history", Attempt: "writer-attempt", Element: reflect.TypeFor[int](),
		Source: CheckpointStreamSource{ID: "empty", Resumable: true}, Capacity: 1, Terminal: StreamCompleted,
	}
}

func TestCheckpointRestorePreservesWriterTerminalAfterReaderStop(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "history")
	key := NewKey[Stream[int]]("source")
	mustAdd(t, g, NewNode("writer", "", nil, Provides(key), func(*RunCtx) error {
		return fmt.Errorf("completed writer reran")
	}))
	mustAdd(t, g, NewNode("reader", "", Requires(key), nil, func(rc *RunCtx) error {
		stream, err := Get(rc, key)
		if err != nil {
			return err
		}
		if _, err := stream.Receive(rc); err != io.EOF {
			return fmt.Errorf("stopped group did not end: %v", err)
		}
		snapshot := stream.Snapshot()
		if snapshot.Terminal != StreamCompleted || !snapshot.Stopped || snapshot.Err != nil {
			return fmt.Errorf("reader Stop rewrote persistent writer terminal: %+v", snapshot)
		}
		select {
		case <-stream.Done():
			return nil
		default:
			return fmt.Errorf("completed writer Done is still open")
		}
	}))
	descriptor := checkpointHistoryDescriptor(key)
	descriptor.Stopped = true
	hooks := checkpointStreamTestHooks(&checkpointProbe{})
	hooks.StreamHooks.Open = func(context.Context, CheckpointStreamOpen) (CheckpointStreamRecord, error) {
		return CheckpointStreamRecord{Stream: descriptor, Resumed: true}, nil
	}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RestoreNodes([]NodeCheckpoint{{NodeID: "writer", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(key.asRef()), State: SlotStateReady, Stream: &descriptor}}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointHistoricalAliasesRejectSourceConflicts(t *testing.T) {
	for _, field := range []string{"generation", "graph", "path", "attempt", "source", "resumable", "capacity", "element", "terminal"} {
		t.Run(field, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g, child := mustNew(t, ctx, "history"), mustNew(t, ctx, "child")
			source, inside, alias := NewKey[Stream[int]]("source"), NewKey[Stream[int]]("inside"), NewKey[Stream[int]]("alias")
			mustAdd(t, g, NewNode("writer", "", nil, Provides(source), func(*RunCtx) error { return nil }))
			declareIn(t, child, NewPort(inside))
			declareOut(t, child, NewPort(inside))
			if err := Sub(g, "mount", "", []SubBind{In(source, inside), Out(inside, alias)}, child); err != nil {
				t.Fatal(err)
			}
			descriptor := checkpointHistoryDescriptor(source)
			conflict := descriptor
			switch field {
			case "generation":
				conflict.ID.Generation++
			case "graph":
				conflict.Graph = "other"
			case "path":
				conflict.Path = "other"
			case "attempt":
				conflict.Attempt = "other"
			case "source":
				conflict.Source.ID = "other"
			case "resumable":
				conflict.Source.Resumable = false
			case "capacity":
				conflict.Capacity++
			case "element":
				conflict.Element = reflect.TypeFor[string]()
			case "terminal":
				conflict.Terminal, conflict.Stopped = StreamStopped, true
			}
			e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", checkpointStreamTestHooks(&checkpointProbe{})))
			if err != nil {
				t.Fatal(err)
			}
			defer e.Cancel()
			plan := []NodeCheckpoint{
				{NodeID: "writer", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(source.asRef()), State: SlotStateReady, Stream: &descriptor}}},
				{NodeID: "mount", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(alias.asRef()), State: SlotStateReady, Stream: &conflict}}},
			}
			if err := e.RestoreNodes(plan); err == nil {
				t.Fatal("conflicting historical source accepted")
			}
			if e.started || g.sealed || len(e.restore) != 0 {
				t.Fatal("invalid aliases had restoration side effects")
			}
		})
	}
}

func TestCheckpointHistoricalReferenceRequiresAuthoritativeCompletion(t *testing.T) {
	key := NewKey[Stream[int]]("source")
	for _, field := range []string{"active", "failed", "canceled", "regressed-terminal", "regressed-stop", "regressed-generation", "scope", "node", "key", "group", "graph", "path", "attempt", "source", "capacity", "not-resumed"} {
		t.Run(field, func(t *testing.T) {
			descriptor := checkpointHistoryDescriptor(key)
			history := descriptor
			history.Terminal = ""
			record := CheckpointStreamRecord{Stream: descriptor, Resumed: true}
			switch field {
			case "active":
				record.Stream.Terminal = ""
			case "failed":
				record.Stream.Terminal = StreamFailed
			case "canceled":
				record.Stream.Terminal = StreamCanceled
			case "regressed-terminal":
				history.Terminal = StreamStopped
			case "regressed-stop":
				history.Stopped = true
			case "regressed-generation":
				record.Stream.ID.Generation--
			case "scope":
				record.Stream.ID.Scope = "other"
			case "node":
				record.Stream.ID.NodeID = "other"
			case "key":
				record.Stream.ID.Key = "other"
			case "group":
				record.Stream.ID.Group = "other"
			case "graph":
				record.Stream.Graph = "other"
			case "path":
				record.Stream.Path = "other"
			case "attempt":
				record.Stream.Attempt = "other"
			case "source":
				record.Stream.Source.ID = "other"
			case "capacity":
				record.Stream.Capacity++
			case "not-resumed":
				record.Resumed = false
			}
			request := CheckpointStreamOpen{Site: Site{Graph: descriptor.Graph}, Scope: descriptor.ID.Scope, Attempt: descriptor.Attempt,
				NodeID: descriptor.ID.NodeID, Key: checkpointKey(key.asRef()), Group: descriptor.ID.Group,
				Source: descriptor.Source, Capacity: descriptor.Capacity, Restore: &history}
			if err := checkCheckpointStreamRecord(request, record); err == nil {
				t.Fatal("historical reference accepted without consistent authoritative completion")
			}
		})
	}
}
