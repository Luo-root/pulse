package pulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
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

func checkpointActiveAliasRun(t *testing.T, order, invalid string) {
	t.Helper()
	ctx := checkpointTestContext(t)
	root, child, pass := mustNew(t, ctx, "active-root"), mustNew(t, ctx, "active-child"), mustNew(t, ctx, "active-pass")
	original, inlet := NewKey[Stream[int]]("original"), NewKey[Stream[int]]("inlet")
	passIn, alias := NewKey[Stream[int]]("pass-in"), NewKey[Stream[int]]("alias")
	ordinary := NewKey[int]("ordinary")
	declareIn(t, child, NewPort(inlet))
	declareIn(t, pass, NewPort(passIn))
	declareOut(t, pass, NewPort(passIn))
	mustAdd(t, child, NewNode("ordinary", "", nil, Provides(ordinary), func(*RunCtx) error {
		return fmt.Errorf("restored ordinary node reran")
	}))
	if err := Sub(child, "pass", "", []SubBind{In(inlet, passIn), Out(passIn, alias)}, pass); err != nil {
		t.Fatal(err)
	}
	aliasEnsured, openEntered, sourceOpening, sourceOpened, finishAllowed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var aliasOnce, openOnce sync.Once
	var opens, sourceCalls, reads, appends, acknowledgements, finishes atomic.Int32
	var execution *Execution
	mustAdd(t, root, NewNode("source", "", nil, Provides(original), func(rc *RunCtx) error {
		sourceCalls.Add(1)
		close(sourceOpening)
		writer, err := OpenStream(rc, original, WithBuffer(1), WithStreamSource("resumable-numbers", invalid != "not-resumable"))
		if err != nil {
			return err
		}
		position := writer.Position()
		if position.Sequence != 1 || position.Cursor != "at-7" {
			return fmt.Errorf("source lost reliable position: %+v", position)
		}
		close(sourceOpened)
		if err := writer.SendAt(9, "at-9"); err != nil {
			return err
		}
		if position := writer.Position(); position.Sequence != 2 || position.Cursor != "at-9" {
			return fmt.Errorf("source did not advance the shared position: %+v", position)
		}
		select {
		case <-finishAllowed:
			return nil
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	}))
	mustAdd(t, child, NewNode("reader", "", Requires(alias), nil, func(rc *RunCtx) error {
		stream, err := Get(rc, alias)
		if err != nil {
			return err
		}
		first, err := stream.Receive(rc)
		if err != nil || first.Value != 7 || first.Sequence != 1 {
			return fmt.Errorf("restored tail = %+v, %v", first, err)
		}
		reads.Add(1)
		select {
		case <-sourceOpened:
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
		_, value := execution.slotOf(original.asRef()).snapshot()
		if stream != value.(Stream[int]) {
			return fmt.Errorf("source and restored alias acquired different managed streams")
		}
		select {
		case <-stream.Done():
			return fmt.Errorf("restored alias finished the actual writer early")
		default:
		}
		if err := first.Ack(); err != nil {
			return err
		}
		second, err := stream.Receive(rc)
		if err != nil || second.Value != 9 || second.Sequence != 2 {
			return fmt.Errorf("continued source = %+v, %v", second, err)
		}
		reads.Add(1)
		if err := second.Ack(); err != nil {
			return err
		}
		snapshot := stream.Snapshot()
		if snapshot.Terminal != "" || snapshot.AckedThrough != 2 || snapshot.LastSequence != 2 || snapshot.InFlight != 0 {
			return fmt.Errorf("Ack and source progress did not share one live ledger: %+v", snapshot)
		}
		select {
		case <-stream.Done():
			return fmt.Errorf("alias completion closed Done before actual source confirmation")
		default:
		}
		close(finishAllowed)
		if _, err := stream.Receive(rc); err != io.EOF {
			return fmt.Errorf("actual writer did not finish shared stream: %v", err)
		}
		if stream.Snapshot().Terminal != StreamCompleted || finishes.Load() != 1 {
			return fmt.Errorf("writer terminal was published before its reliable confirmation")
		}
		return nil
	}))
	delay := func(rc *RunCtx, next func(*RunCtx) error) error {
		if order == "source-first" {
			select {
			case <-sourceOpened:
			case <-rc.Context().Done():
				return rc.Context().Err()
			}
		}
		return next(rc)
	}
	if err := Sub(root, "scope", "", []SubBind{In(original, inlet)}, child, delay); err != nil {
		t.Fatal(err)
	}
	history := CheckpointStreamDescriptor{
		ID:    CheckpointStreamID{Scope: "root", NodeID: "source", Key: original.Name(), Group: "default", Generation: 1},
		Graph: "active-root", Attempt: "source-attempt", Element: reflect.TypeFor[int](),
		Source: CheckpointStreamSource{ID: "resumable-numbers", Resumable: invalid != "not-resumable"}, Capacity: 1,
	}
	authoritative := history
	authoritative.ID.Generation = 2
	reference := history
	switch invalid {
	case "completed":
		reference.Terminal = StreamCompleted
	case "stopped":
		reference.Terminal, reference.Stopped = StreamStopped, true
	case "future-generation":
		reference.ID.Generation = 3
	case "stopped-regression":
		reference.Stopped = true
	case "source":
		reference.Source.ID = "forged"
	case "attempt":
		reference.Attempt = "forged"
	case "graph":
		reference.Graph = "forged"
	case "path":
		reference.Path = "forged"
	case "capacity":
		reference.Capacity++
	}
	hooks := checkpointStreamTestHooks(&checkpointProbe{})
	hooks.OpenAttempt = func(ctx context.Context, request CheckpointAttemptRequest) (CheckpointAttempt, error) {
		if request.NodeID == "source" {
			var signal <-chan struct{}
			switch order {
			case "alias-first":
				signal = aliasEnsured
			case "interleaved":
				signal = openEntered
			}
			if signal != nil {
				select {
				case <-signal:
				case <-ctx.Done():
					return CheckpointAttempt{}, ctx.Err()
				}
			}
			return CheckpointAttempt{Scope: "source-attempt", Index: 1}, nil
		}
		return CheckpointAttempt{Scope: request.Parent + "/" + request.NodeID, Index: 1}, nil
	}
	hooks.FailAttempt = func(context.Context, string) error { return nil }
	hooks.OpenChild = func(_ context.Context, request CheckpointChild) ([]NodeCheckpoint, error) {
		if request.Graph != "active-child" {
			return nil, nil
		}
		return []NodeCheckpoint{
			{NodeID: "ordinary", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(ordinary.asRef()), State: SlotStateReady, Value: 42}}},
			{NodeID: "pass", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(alias.asRef()), State: SlotStateReady, Stream: &reference}}},
		}, nil
	}
	hooks.StreamHooks.Open = func(ctx context.Context, request CheckpointStreamOpen) (CheckpointStreamRecord, error) {
		if opens.Add(1) != 1 {
			return CheckpointStreamRecord{}, fmt.Errorf("one live source was opened twice")
		}
		openOnce.Do(func() { close(openEntered) })
		if order == "interleaved" {
			select {
			case <-sourceOpening:
			case <-ctx.Done():
				return CheckpointStreamRecord{}, ctx.Err()
			}
		}
		return CheckpointStreamRecord{Stream: authoritative, Resumed: invalid != "not-resumed", LastSequence: 1,
			Pending: []CheckpointStreamMessage{{Sequence: 1, Value: 7}}, Cursor: "at-7"}, nil
	}
	hooks.StreamHooks.Append = func(_ context.Context, append CheckpointStreamAppend) error {
		if append.Stream != authoritative.ID || append.Message.Sequence != 2 || append.Message.Value != 9 || append.Cursor != "at-9" {
			return fmt.Errorf("source append did not use authoritative shared progress: %+v", append)
		}
		appends.Add(1)
		return nil
	}
	hooks.StreamHooks.Commit = func(_ context.Context, commit CheckpointMessageCommit) error {
		if len(commit.Inputs) != 1 || commit.Inputs[0].Stream != authoritative.ID || commit.Inputs[0].Sequence != uint64(acknowledgements.Add(1)) {
			return fmt.Errorf("Ack did not use the same authoritative reading group: %+v", commit)
		}
		return nil
	}
	hooks.Commit = func(_ context.Context, commit CheckpointCommit) error {
		if commit.Node.NodeID == "source" {
			if len(commit.Streams) != 1 || commit.Streams[0].Stream != authoritative.ID || commit.Streams[0].Terminal != StreamCompleted ||
				commit.Streams[0].LastSequence != 2 || commit.Streams[0].Cursor != "at-9" || commit.Streams[0].Site != execution.site {
				return fmt.Errorf("writer terminal lost actual owner or shared progress: %+v", commit)
			}
			finishes.Add(1)
		} else if len(commit.Streams) != 0 {
			return fmt.Errorf("alias was registered as an actual writer: %+v", commit)
		}
		return nil
	}
	observer := ObserverFunc{Finished: func(site Site, node string, _ NodeFinishReason, err error) {
		if site.Graph == "active-child" && node == "pass" && err == nil {
			aliasOnce.Do(func() { close(aliasEnsured) })
		}
	}}
	e, err := root.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks), WithExecObserver(observer))
	if err != nil {
		t.Fatal(err)
	}
	execution = e
	defer e.Cancel()
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	err = e.Wait()
	if invalid != "" {
		if !isCheckpointFailure(err) || errors.Is(err, context.DeadlineExceeded) || reads.Load() != 0 || appends.Load() != 0 || finishes.Load() != 0 {
			t.Fatalf("invalid %s reference escaped %s validation: err=%v reads=%d appends=%d finishes=%d", invalid, order, err, reads.Load(), appends.Load(), finishes.Load())
		}
		if order == "source-first" && invalid != "not-resumable" {
			sub := e.Snapshot().Nodes[1].Child
			if sub == nil || sub.started || len(sub.restored) != 0 {
				t.Fatal("invalid active reference installed a partial restoration")
			}
			if state, _ := sub.slotOf(ordinary.asRef()).snapshot(); state != slotPending {
				t.Fatal("invalid active reference installed an earlier ordinary output")
			}
			_, value := e.slotOf(original.asRef()).snapshot()
			snapshot := value.(Stream[int]).Snapshot()
			if snapshot.Stopped || snapshot.LastSequence != 1 || snapshot.InFlight != 1 || snapshot.AckedThrough != 0 {
				t.Fatalf("invalid later reference changed the active source: %+v", snapshot)
			}
		}
		return
	}
	if err != nil || opens.Load() != 1 || sourceCalls.Load() != 1 || reads.Load() != 2 || appends.Load() != 1 || acknowledgements.Load() != 2 || finishes.Load() != 1 {
		t.Fatalf("live alias did not share one source: err=%v opens=%d source=%d reads=%d appends=%d acks=%d finishes=%d", err, opens.Load(), sourceCalls.Load(), reads.Load(), appends.Load(), acknowledgements.Load(), finishes.Load())
	}
}

func TestCheckpointActiveAliasSharesResumableSource(t *testing.T) {
	for _, order := range []string{"alias-first", "source-first", "interleaved"} {
		t.Run(order, func(t *testing.T) { checkpointActiveAliasRun(t, order, "") })
	}
}

func TestCheckpointActiveAliasRejectsInvalidReferences(t *testing.T) {
	for _, order := range []string{"alias-first", "source-first"} {
		for _, invalid := range []string{"completed", "stopped", "future-generation", "stopped-regression", "source", "attempt", "graph", "path", "capacity", "not-resumed", "not-resumable"} {
			t.Run(order+"/"+invalid, func(t *testing.T) { checkpointActiveAliasRun(t, order, invalid) })
		}
	}
}
