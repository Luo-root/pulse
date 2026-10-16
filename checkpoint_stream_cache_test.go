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

// A root source restores before a child's saved pass-through alias is decoded.
// No Durable-private canonicalization is used: both historical and current
// descriptors are legal inputs to the public Core restore contract.
func independentLateScope(t *testing.T, reference func(CheckpointStreamDescriptor) CheckpointStreamDescriptor, pending ...bool) (error, int32, bool, int, uint64) {
	t.Helper()
	ctx := checkpointTestContext(t)
	waitForScope := len(pending) != 0 && pending[0]
	root, child, pass := mustNew(t, ctx, "independent-root"), mustNew(t, ctx, "independent-child"), mustNew(t, ctx, "independent-pass")
	original, inlet := NewKey[Stream[int]]("original"), NewKey[Stream[int]]("inlet")
	passIn, alias := NewKey[Stream[int]]("pass-in"), NewKey[Stream[int]]("alias")
	ordinary := NewKey[int]("ordinary")
	mustAdd(t, child, NewNode("ordinary", "", nil, Provides(ordinary), func(*RunCtx) error {
		return fmt.Errorf("restored ordinary body reran")
	}))
	mustAdd(t, root, NewNode("source", "", nil, Provides(original), func(*RunCtx) error {
		return fmt.Errorf("completed source body reran")
	}))
	declareOut(t, root, NewPort(original))
	declareIn(t, child, NewPort(inlet))
	declareIn(t, pass, NewPort(passIn))
	declareOut(t, pass, NewPort(passIn))
	if err := Sub(child, "pass", "", []SubBind{In(inlet, passIn), Out(passIn, alias)}, pass); err != nil {
		t.Fatal(err)
	}
	var same atomic.Bool
	var repeated atomic.Int32
	mustAdd(t, child, NewNode("reader", "", Requires(inlet, alias), nil, func(rc *RunCtx) error {
		a, err := Get(rc, inlet)
		if err != nil {
			return err
		}
		b, err := Get(rc, alias)
		if err != nil {
			return err
		}
		same.Store(a == b)
		// Restored node completion must rebuild Done and the tail independently
		// of the reader's first Receive. Wait for both ledgers to be installed.
		select {
		case <-b.Done():
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
		d, err := a.Receive(rc)
		if err != nil || d.Value != 7 {
			return fmt.Errorf("first tail: %v %v", d.Value, err)
		}
		if err := d.Ack(); err != nil {
			return err
		}
		d, err = b.Receive(rc)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		repeated.Add(1)
		// Leave an observable result but discharge the second local ledger so
		// that an unrelated reading-obligation error cannot hide the defect.
		return d.Ack()
	}))
	ready := make(chan struct{})
	var readyOnce sync.Once
	delay := func(rc *RunCtx, next func(*RunCtx) error) error {
		select {
		case <-ready:
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
		return next(rc)
	}
	swallow := func(rc *RunCtx, next func(*RunCtx) error) error { _ = next(rc); return nil }
	if err := Sub(root, "mount", "", []SubBind{In(original, inlet)}, child, swallow, delay); err != nil {
		t.Fatal(err)
	}
	history := CheckpointStreamDescriptor{
		ID:    CheckpointStreamID{Scope: "root", NodeID: "source", Key: "original", Group: "default", Generation: 1},
		Graph: "independent-root", Attempt: "source-attempt", Element: reflect.TypeFor[int](),
		Source: CheckpointStreamSource{ID: "numbers", Resumable: true}, Capacity: 1, Terminal: StreamCompleted,
	}
	hooks := checkpointStreamTestHooks(&checkpointProbe{})
	var opens atomic.Int32
	var execution *Execution
	scopeStarted := make(chan struct{})
	var scopeOnce sync.Once
	hooks.StreamHooks.Open = func(ctx context.Context, req CheckpointStreamOpen) (CheckpointStreamRecord, error) {
		opens.Add(1)
		if req.Restore == nil {
			return CheckpointStreamRecord{}, fmt.Errorf("source opened without restore")
		}
		// 子图此刻仍被 ready 挡住；在钩子返回前证明装配、图、树和端点锁均可用。
		unlocked := make(chan struct{})
		go func() {
			assemblyMu.Lock()
			_ = root.CheckpointNodes()
			failure := execution.checkpoint.failure
			failure.mu.Lock()
			_, value := execution.slotOf(original.asRef()).snapshot()
			_ = value.(Stream[int]).pulseCheckpointDescriptor()
			failure.mu.Unlock()
			assemblyMu.Unlock()
			close(unlocked)
		}()
		select {
		case <-unlocked:
		case <-ctx.Done():
			return CheckpointStreamRecord{}, fmt.Errorf("Open held an engine lock: %w", ctx.Err())
		}
		if waitForScope {
			readyOnce.Do(func() { close(ready) })
			select {
			case <-scopeStarted:
			case <-ctx.Done():
				return CheckpointStreamRecord{}, ctx.Err()
			}
		}
		authoritative := history
		authoritative.ID.Generation = 2
		return CheckpointStreamRecord{Stream: authoritative, Resumed: true, LastSequence: 1,
			Pending: []CheckpointStreamMessage{{Sequence: 1, Value: 7}}}, nil
	}
	hooks.OpenChild = func(_ context.Context, req CheckpointChild) ([]NodeCheckpoint, error) {
		if req.Graph != "independent-child" {
			return nil, nil
		}
		late := reference(history)
		return []NodeCheckpoint{
			{NodeID: "ordinary", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(ordinary.asRef()), State: SlotStateReady, Value: 42}}},
			{NodeID: "pass", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(alias.asRef()), State: SlotStateReady, Stream: &late}}},
		}, nil
	}
	observer := ObserverFunc{GraphStarted: func(site Site) {
		if site.Graph == "independent-child" {
			scopeOnce.Do(func() { close(scopeStarted) })
		}
	}, Finished: func(site Site, node string, reason NodeFinishReason, err error) {
		if site.Graph == "independent-root" && node == "source" && err == nil {
			readyOnce.Do(func() { close(ready) })
		}
	}}
	e, err := root.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks), WithExecObserver(observer))
	if err != nil {
		t.Fatal(err)
	}
	execution = e
	defer e.Cancel()
	if err := e.RestoreNodes([]NodeCheckpoint{{NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{
		{Key: checkpointKey(original.asRef()), State: SlotStateReady, Stream: &history},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		return err, opens.Load(), same.Load(), int(repeated.Load()), 0
	}
	err = e.Wait()
	if err != nil && !waitForScope {
		sub := e.Snapshot().Nodes[1].Child
		if sub == nil || sub.started || len(sub.restored) != 0 {
			t.Error("rejected late batch started or installed node restoration")
		} else {
			for _, ref := range []keyRef{ordinary.asRef(), alias.asRef()} {
				if state, _ := sub.slotOf(ref).snapshot(); state != slotPending {
					t.Error("rejected late batch installed a partial output")
				}
			}
		}
		_, value := e.slotOf(original.asRef()).snapshot()
		stream := value.(Stream[int])
		descriptor := stream.pulseCheckpointDescriptor()
		snapshot := stream.Snapshot()
		if descriptor.Source != history.Source || descriptor.Attempt != history.Attempt || descriptor.Graph != history.Graph ||
			descriptor.Path != history.Path || descriptor.Capacity != history.Capacity || descriptor.ID.Generation != 2 ||
			snapshot.Terminal != StreamCompleted || snapshot.Stopped || snapshot.LastSequence != 1 || snapshot.AckedThrough != 0 || snapshot.InFlight != 1 {
			t.Errorf("invalid later alias changed the existing endpoint: descriptor=%+v snapshot=%+v", descriptor, snapshot)
		}
	}
	e.checkpoint.failure.mu.Lock()
	cached := uint64(len(e.checkpoint.failure.streams))
	e.checkpoint.failure.mu.Unlock()
	return err, opens.Load(), same.Load(), int(repeated.Load()), cached
}

func TestIndependentLateScopeSharesSourceAfterGenerationAdvance(t *testing.T) {
	for _, current := range []bool{false, true} {
		t.Run(fmt.Sprintf("current-generation-%t", current), func(t *testing.T) {
			err, opens, same, repeated, cached := independentLateScope(t, func(d CheckpointStreamDescriptor) CheckpointStreamDescriptor {
				if current {
					d.ID.Generation = 2
				}
				return d
			})
			t.Logf("Wait=%v Open calls=%d same endpoint=%t repeated delivery=%d cache entries=%d", err, opens, same, repeated, cached)
			if err != nil || opens != 1 || !same || repeated != 0 || cached != 1 {
				t.Fatalf("one persistent source acquired two local identities after Open advanced generation")
			}
		})
	}
}

func TestIndependentLateScopeMustValidateCachedReference(t *testing.T) {
	for _, field := range []string{"source", "attempt", "graph", "path", "capacity", "terminal", "future-generation"} {
		t.Run(field, func(t *testing.T) {
			err, opens, same, repeated, cached := independentLateScope(t, func(d CheckpointStreamDescriptor) CheckpointStreamDescriptor {
				switch field {
				case "source":
					d.Source.ID = "forged"
				case "attempt":
					d.Attempt = "forged"
				case "graph":
					d.Graph = "forged"
				case "path":
					d.Path = "forged"
				case "capacity":
					d.Capacity++
				case "terminal":
					d.Terminal, d.Stopped = StreamStopped, true
				case "future-generation":
					d.ID.Generation = 3
				}
				return d
			})
			t.Logf("Wait=%v Open calls=%d same endpoint=%t repeated delivery=%d cache entries=%d", err, opens, same, repeated, cached)
			if !isCheckpointFailure(err) || opens != 1 || cached != 1 {
				t.Fatalf("a conflicting later reference bypassed reliable validation or allocated another endpoint")
			}
		})
	}
}

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
