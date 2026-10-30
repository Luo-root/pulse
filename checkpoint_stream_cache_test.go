package pulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 根来源先恢复，随后才解码子图已保存的透传别名。
// 不使用 Durable 私有的规范化处理：历史描述符和当前描述符
// 都是 Core 公开恢复契约允许的合法输入。
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
		// 恢复节点完成时必须重建 Done 和尾部，不能依赖读取方的首次 Receive。
		// 等待两份账本都安装完成。
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
		// 保留可观测结果，但清偿第二份本地账本的读取义务，
		// 避免无关的读取义务错误掩盖此缺陷。
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

func TestCheckpointHostRestoreOwnsSharedOpen(t *testing.T) {
	for _, mode := range []string{"host-first", "root-first", "store-error", "execution-cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "host-restore")
			key := NewKey[Stream[int]]("output")
			declareOut(t, g, NewPort(key))
			mustAdd(t, g, NewNode("source", "", nil, Provides(key), func(*RunCtx) error {
				return errors.New("restored source reran")
			}))
			history := CheckpointStreamDescriptor{
				ID:    CheckpointStreamID{Scope: "root", NodeID: "source", Key: "output", Group: "default", Generation: 1},
				Graph: "host-restore", Attempt: "source-attempt", Element: reflect.TypeFor[int](),
				Source: CheckpointStreamSource{ID: "finite"}, Capacity: 1, Terminal: StreamCompleted,
			}
			entered, release := make(chan context.Context, 1), make(chan struct{})
			var opens, acks atomic.Int32
			cause := errors.New("generation save failed")
			probe := &checkpointProbe{}
			hooks := checkpointStreamTestHooks(probe)
			hooks.StreamHooks.Open = func(openCtx context.Context, req CheckpointStreamOpen) (CheckpointStreamRecord, error) {
				opens.Add(1)
				entered <- openCtx
				select {
				case <-release:
				case <-openCtx.Done():
					return CheckpointStreamRecord{}, openCtx.Err()
				}
				if mode == "store-error" {
					return CheckpointStreamRecord{}, cause
				}
				saved := *req.Restore
				saved.ID.Generation++
				return CheckpointStreamRecord{Stream: saved, Resumed: true, LastSequence: 1,
					Pending: []CheckpointStreamMessage{{Sequence: 1, Value: 7}}}, nil
			}
			hooks.StreamHooks.Commit = func(_ context.Context, commit CheckpointMessageCommit) error {
				if len(commit.Inputs) != 1 || commit.Inputs[0].Stream.Generation != 2 {
					return errors.New("Ack used wrong generation")
				}
				acks.Add(1)
				return nil
			}
			callCtx, cancelCall := context.WithCancel(ctx)
			defer cancelCall()
			first := make(chan error, 1)
			var sub *StreamSubscription[int]
			var e *Execution
			read := func() { _, err := sub.Receive(callCtx); first <- err }
			cancelRead := func() {
				openCtx := waitCheckpointSignal(t, ctx, entered)
				cancelCall()
				if err := waitCheckpointSignal(t, ctx, first); !errors.Is(err, context.Canceled) {
					t.Errorf("single Receive = %v", err)
				}
				if openCtx.Err() != nil || e.Context().Err() != nil {
					t.Errorf("shared Open did not retain healthy execution context: open=%v execution=%v", openCtx.Err(), e.Context().Err())
				}
				if mode == "execution-cancel" {
					e.Cancel()
				}
				close(release)
			}
			observer := ObserverFunc{GraphStarted: func(Site) {
				if mode != "root-first" {
					go read()
					cancelRead() // 必须先返回单次取消，才允许共享 Open 完成。
				}
			}}
			var err error
			e, err = g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks), WithExecObserver(observer))
			messageTestOK(t, err)
			defer e.Cancel()
			sub, err = SubscribeStream(e, key)
			messageTestOK(t, err)
			messageTestOK(t, e.RestoreNodes([]NodeCheckpoint{{NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{
				{Key: checkpointKey(key.asRef()), State: SlotStateReady, Stream: &history},
			}}}))
			messageTestOK(t, e.Start())
			if mode == "root-first" {
				// 根恢复进入 Open 后才启动宿主；不让宿主抢初始化所有权。
				openCtx := waitCheckpointSignal(t, ctx, entered)
				entered <- openCtx
				go read()
				cancelRead()
			}
			if mode == "store-error" || mode == "execution-cancel" {
				err = e.Wait()
				if mode == "store-error" && (!isCheckpointFailure(err) || !errors.Is(err, cause)) {
					t.Fatalf("Open failure lost: %v", err)
				}
				if mode == "execution-cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("execution cancel lost: %v", err)
				}
			} else {
				delivery, err := sub.Receive(ctx)
				messageTestOK(t, err)
				if delivery.Sequence != 1 || delivery.Value != 7 {
					t.Fatalf("restored tail=%+v", delivery)
				}
				if acks.Load() != 0 {
					t.Fatal("canceled Receive automatically acknowledged the restored tail")
				}
				messageTestOK(t, delivery.Ack())
				messageTestOK(t, e.Wait())
				if _, err := sub.Receive(ctx); err != io.EOF {
					t.Fatalf("tail repeated after Wait: %v", err)
				}
				if acks.Load() != 1 {
					t.Fatalf("reliable Acks=%d", acks.Load())
				}
			}
			wantStatus := GraphStatusCompleted
			// Open 也响应根取消时，已接纳保存回错仍是优先的可靠失败。
			if mode == "store-error" || isCheckpointFailure(err) {
				wantStatus = GraphStatusFailed
			} else if mode == "execution-cancel" {
				wantStatus = GraphStatusCanceled
			}
			if e.Snapshot().Graph.Status != wantStatus || len(probe.failures) != 0 || len(probe.commits) != 0 || opens.Load() != 1 {
				t.Fatalf("host cancellation changed execution: graph=%s Open=%d FailAttempt=%d Commit=%d", e.Snapshot().Graph.Status, opens.Load(), len(probe.failures), len(probe.commits))
			}
		})
	}
}

func TestCheckpointWaitJoinsCanceledSharedOpen(t *testing.T) {
	for _, mode := range []string{"accepted-error", "accepted-wrapped-error", "not-started", "accepted-success"} {
		t.Run(mode, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "join-open")
			key := NewKey[Stream[int]]("output")
			declareOut(t, g, NewPort(key))
			mustAdd(t, g, NewNode("source", "", nil, Provides(key), nil))
			history := CheckpointStreamDescriptor{
				ID:    CheckpointStreamID{Scope: "root", NodeID: "source", Key: "output", Group: "default", Generation: 1},
				Graph: "join-open", Attempt: "source-attempt", Element: reflect.TypeFor[int](),
				Source: CheckpointStreamSource{ID: "finite"}, Capacity: 1, Terminal: StreamCompleted,
			}
			entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			canceled := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			hooks := checkpointStreamTestHooks(&checkpointProbe{})
			hooks.StreamHooks.Open = func(openCtx context.Context, _ CheckpointStreamOpen) (CheckpointStreamRecord, error) {
				close(entered)
				// 来源先报告 canceled Finished，已接纳的同步保存才允许返回。
				select {
				case <-release:
				case <-ctx.Done():
				}
				defer close(returned)
				switch mode {
				case "not-started":
					return CheckpointStreamRecord{}, &CheckpointNotStarted{}
				case "accepted-wrapped-error":
					return CheckpointStreamRecord{}, fmt.Errorf("Open save failed: %w", openCtx.Err())
				case "accepted-success":
					authoritative := history
					authoritative.ID.Generation++
					return CheckpointStreamRecord{Stream: authoritative, Resumed: true}, nil
				default:
					return CheckpointStreamRecord{}, openCtx.Err()
				}
			}
			var e *Execution
			var finished atomic.Int32
			reliableFailure := mode == "accepted-error" || mode == "accepted-wrapped-error"
			wantReason := NodeCanceled
			if reliableFailure {
				wantReason = NodeFailed
			}
			observer := ObserverFunc{
				Finished: func(_ Site, _ string, reason NodeFinishReason, _ error) {
					if reason != NodeCanceled {
						t.Errorf("source finished=%s before Save reply", reason)
					}
					close(canceled)
				},
				GraphFinished: func(_ Site, reason NodeFinishReason, err error) {
					finished.Add(1)
					if reason != wantReason || e.Err() != err || e.Snapshot().Graph.Err != err || e.Snapshot().Graph.Status != GraphStatus(wantReason) {
						t.Errorf("GraphFinished/Err/Snapshot disagree: reason=%s err=%v", reason, err)
					}
					value, state, readErr := ReadOutput(e, key)
					if value.state != nil || state != "" || readErr != err {
						t.Errorf("failed output readable: state=%s err=%v", state, readErr)
					}
				},
			}
			var err error
			e, err = g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks), WithExecObserver(observer))
			messageTestOK(t, err)
			defer e.Cancel()
			_, err = SubscribeStream(e, key)
			messageTestOK(t, err)
			messageTestOK(t, e.RestoreNodes([]NodeCheckpoint{{NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{
				{Key: checkpointKey(key.asRef()), State: SlotStateReady, Stream: &history},
			}}}))
			messageTestOK(t, e.Start())
			waitCheckpointSignal(t, ctx, entered)
			e.Cancel()
			waitCheckpointSignal(t, ctx, canceled)
			e.wg.Wait() // 节点与宿主已终止，只有共享保存仍在执行。
			if !errors.Is(e.Err(), context.Canceled) || e.checkpointErr() != nil {
				t.Fatal("cancellation did not precede the reliable failure")
			}
			const waiters = 8
			result := make(chan error, waiters)
			for range waiters {
				go func() { result <- e.Wait() }()
			}
			for {
				e.mu.Lock()
				closing, done := e.streamClosing, e.done
				e.mu.Unlock()
				if done {
					t.Fatal("Wait finalized before shared Open returned")
				}
				if closing {
					break
				}
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				runtime.Gosched()
			}
			select {
			case err := <-result:
				t.Fatalf("Wait escaped a running shared Open: %v", err)
			default:
			}
			releaseOnce.Do(func() { close(release) })
			for range waiters {
				err = waitCheckpointSignal(t, ctx, result)
				var failure *CheckpointError
				if got := errors.As(err, &failure); got != reliableFailure {
					t.Errorf("accepted Save classification lost/created: mode=%s checkpoint=%t result=%v", mode, got, err)
				}
				if !errors.Is(err, context.Canceled) || e.Err() != err || e.Wait() != err {
					t.Errorf("final result changed: Wait=%v Err=%v", err, e.Err())
				}
			}
			waitCheckpointSignal(t, ctx, returned)
			if finished.Load() != 1 {
				t.Fatalf("GraphFinished calls=%d", finished.Load())
			}
		})
	}
}

// 同一公开调用仅改变取消的归属和时点；Fork 错误交回健康完整节点时必须确认失败。
// 宿主单次 Receive 与真正恢复子执行的控制分别见 HostRestoreOwnsSharedOpen / RestoredChildCancelPreservesBudget。
func TestCheckpointStreamWaitCancellationOrigin(t *testing.T) {
	for _, entry := range []string{"OpenStream", "Receive"} {
		for _, mode := range []string{"fork-before", "fork-during", "fork-ready", "nested-fork", "node-cancel", "execution-cancel", "timeout"} {
			if entry == "Receive" && mode == "fork-before" {
				continue
			}
			t.Run(entry+"/"+mode, func(t *testing.T) {
				ctx := checkpointTestContext(t)
				g := mustNew(t, ctx, "stream-wait-origin")
				key := NewKey[Stream[int]]("stream")
				probe := &checkpointProbe{}
				hooks := checkpointStreamTestHooks(probe)
				open := hooks.StreamHooks.Open
				entered, release := make(chan context.Context, 1), make(chan struct{})
				permit := make(chan struct{})
				var releaseOnce, permitOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				allowReceive := func() { permitOnce.Do(func() { close(permit) }) }
				defer unblock()
				defer allowReceive()
				var opens, acks atomic.Int32
				var openReturned atomic.Bool
				hooks.StreamHooks.Open = func(openCtx context.Context, req CheckpointStreamOpen) (CheckpointStreamRecord, error) {
					opens.Add(1)
					entered <- openCtx
					// 已接纳的保存不受局部等待取消影响，根取消后也可成功返回。
					select {
					case <-release:
					case <-ctx.Done():
						return CheckpointStreamRecord{}, ctx.Err()
					}
					defer openReturned.Store(true)
					if req.Restore == nil {
						return open(openCtx, req)
					}
					d := *req.Restore
					d.ID.Generation++
					return CheckpointStreamRecord{Stream: d, Resumed: true, LastSequence: 1,
						Pending: []CheckpointStreamMessage{{Sequence: 1, Value: 17}}}, nil
				}
				hooks.StreamHooks.Commit = func(context.Context, CheckpointMessageCommit) error { acks.Add(1); return nil }
				callReady, ownerHealthy := make(chan *RunCtx, 1), make(chan bool, 1)
				streamSeen := make(chan Stream[int], 1)
				run := func(rc *RunCtx) error {
					waiter := rc
					var parent *RunCtx
					if strings.HasPrefix(mode, "fork-") || mode == "nested-fork" {
						waiter = rc.Fork()
						defer waiter.Cancel()
						if mode == "nested-fork" {
							parent = waiter
							waiter = parent.Fork()
							defer waiter.Cancel()
						}
					}
					var err error
					if entry == "OpenStream" {
						callReady <- waiter
						if mode == "fork-before" {
							waiter.Cancel()
						}
						var writer *StreamWriter[int]
						writer, err = OpenStream(waiter, key, WithStreamSource("resumable", true))
						if mode == "fork-ready" && err == nil {
							waiter.Cancel()
							err = writer.Send(1)
						}
					} else {
						stream, getErr := Get(rc, key)
						if getErr != nil {
							return getErr
						}
						streamSeen <- stream
						callReady <- waiter // Get 已成功，驱动取消只能命中随后被测的 Receive。
						if mode == "fork-ready" {
							select {
							case <-permit:
							case <-ctx.Done():
								return ctx.Err()
							}
						}
						_, err = stream.Receive(waiter)
					}
					ownerHealthy <- rc.Context().Err() == nil && (parent == nil || parent.Context().Err() == nil)
					return err
				}
				var aspects []Aspect
				if mode == "timeout" {
					aspects = append(aspects, Timeout(250*time.Millisecond))
				}
				target := "source"
				if entry == "OpenStream" {
					declareOut(t, g, NewPort(key))
					mustAdd(t, g, NewNode(target, "交回局部等待的结果", nil, Provides(key), run, aspects...))
				} else {
					target = "sink"
					mustAdd(t, g, NewNode("source", "已确认来源不重跑", nil, Provides(key), func(*RunCtx) error {
						return errors.New("restored source reran")
					}))
					mustAdd(t, g, NewNode(target, "交回局部读取的结果", Requires(key), nil, run, aspects...))
				}
				type event struct {
					reason NodeFinishReason
					err    error
				}
				nodeDone, graphDone := make(chan event, 1), make(chan event, 1)
				sourceDone := make(chan struct{}, 1)
				observer := ObserverFunc{
					Finished: func(_ Site, node string, reason NodeFinishReason, err error) {
						if node == target {
							nodeDone <- event{reason, err}
						} else if node == "source" {
							sourceDone <- struct{}{}
						}
					},
					GraphFinished: func(_ Site, reason NodeFinishReason, err error) {
						if opens.Load() != 0 && !openReturned.Load() {
							t.Error("Wait finalized before accepted Open returned")
						}
						graphDone <- event{reason, err}
					},
				}
				e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks), WithExecObserver(observer))
				messageTestOK(t, err)
				defer e.Cancel()
				if entry == "OpenStream" {
					_, err = SubscribeStream(e, key)
					messageTestOK(t, err)
				} else {
					d := CheckpointStreamDescriptor{
						ID:    CheckpointStreamID{Scope: "root", NodeID: "source", Key: "stream", Group: "default", Generation: 1},
						Graph: "stream-wait-origin", Attempt: "source-attempt", Element: reflect.TypeFor[int](),
						Source: CheckpointStreamSource{ID: "finite"}, Capacity: 1, Terminal: StreamCompleted,
					}
					messageTestOK(t, e.RestoreNodes([]NodeCheckpoint{{NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{
						{Key: checkpointKey(key.asRef()), State: SlotStateReady, Stream: &d},
					}}}))
				}
				messageTestOK(t, e.Start())
				result := make(chan error, 1)
				go func() { result <- e.Wait() }()
				waiter := waitCheckpointSignal(t, ctx, callReady)
				if mode != "fork-before" {
					openCtx := waitCheckpointSignal(t, ctx, entered)
					if openCtx.Err() != nil || e.Context().Err() != nil {
						t.Fatal("probe did not enter a healthy shared Open")
					}
					switch mode {
					case "fork-ready":
						unblock()
						if entry == "Receive" {
							waitCheckpointSignal(t, ctx, sourceDone) // Finished 证明权威尾部已安装。
							waiter.Cancel()
							allowReceive()
						}
					case "execution-cancel":
						e.Cancel()
					case "timeout": // 由真实 Timeout 计时器打断公开调用。
					default:
						waiter.Cancel()
					}
				}
				ev := waitCheckpointSignal(t, ctx, nodeDone)
				local := strings.HasPrefix(mode, "fork-") || mode == "nested-fork"
				if healthy := waitCheckpointSignal(t, ctx, ownerHealthy); local && !healthy {
					t.Error("local cancellation reached the owning node or parent Fork")
				}
				if mode != "fork-before" && mode != "fork-ready" {
					select {
					case err := <-result:
						t.Fatalf("Wait escaped accepted Open: %v", err)
					default:
					}
				}
				unblock()
				err = waitCheckpointSignal(t, ctx, result)
				graph := waitCheckpointSignal(t, ctx, graphDone)
				want, failures := NodeFailed, 1
				if mode == "node-cancel" || mode == "execution-cancel" {
					want, failures = NodeCanceled, 0
				}
				snapshot := e.Snapshot()
				if ev.reason != want || graph.reason != want || snapshot.Graph.Status != GraphStatus(want) ||
					snapshotNode(t, snapshot, target).Status != NodeStatus(want) || len(probe.failures) != failures || len(probe.commits) != 0 {
					t.Errorf("cancellation origin changed: node=%s graph=%s FailAttempt=%d Commit=%d; want=%s FailAttempt=%d", ev.reason, snapshot.Graph.Status, len(probe.failures), len(probe.commits), want, failures)
				}
				for _, got := range []error{err, ev.err, graph.err} {
					if got == nil || isCheckpointFailure(got) || (mode == "timeout" && !strings.Contains(got.Error(), "timeout after")) ||
						(mode != "timeout" && !errors.Is(got, context.Canceled)) {
						t.Errorf("unexpected public call error: %T %v", got, got)
					}
				}
				wantOpens := int32(1)
				if mode == "fork-before" {
					wantOpens = 0
				}
				if opens.Load() != wantOpens || acks.Load() != 0 || e.Err() != err || e.Wait() != err {
					t.Errorf("shared work/result changed: Open=%d Ack=%d Wait=%v Err=%v", opens.Load(), acks.Load(), err, e.Err())
				}
				if entry == "Receive" {
					tail := waitCheckpointSignal(t, ctx, streamSeen).Snapshot()
					if tail.LastSequence != 1 || tail.InFlight != 1 || tail.AckedThrough != 0 || tail.Stopped {
						t.Errorf("cancellation consumed the unclaimed tail: %+v", tail)
					}
				}
				t.Logf("node=%s graph=%s Open=%d FailAttempt=%d Commit=%d Ack=%d", ev.reason, snapshot.Graph.Status, opens.Load(), len(probe.failures), len(probe.commits), acks.Load())
			})
		}
	}
}

// Done 的访问是等待域选择的屏障；只观察 context 协议，不用调度延时猜测是否已经开始等。
type checkpointWaitContext struct {
	context.Context
	selected chan<- string
	name     string
	once     sync.Once
}

func (c *checkpointWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { c.selected <- c.name })
	return c.Context.Done()
}

// 三节点公开图：恢复来源先报告 Finished，未完成的兄弟节点随后才返回取消。
// 子执行单独 Cancel 时，父和共享 Open 此刻仍健康，不能确认父挂载的业务失败。
func TestCheckpointRestoredChildCancelPreservesBudget(t *testing.T) {
	for _, mode := range []string{"cancel-child", "cancel-root", "healthy"} {
		t.Run(mode, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			root, child := mustNew(t, ctx, "budget-root"), mustNew(t, ctx, "budget-child")
			inside, outside := NewKey[Stream[int]]("inside"), NewKey[Stream[int]]("outside")
			declareOut(t, child, NewPort(inside))
			declareOut(t, root, NewPort(outside))
			mustAdd(t, child, NewNode("source", "恢复来源不重跑", nil, Provides(inside), func(*RunCtx) error {
				return errors.New("restored source reran")
			}))
			sourceObserved, holdStarted, releaseHold := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var holdOnce sync.Once
			defer holdOnce.Do(func() { close(releaseHold) })
			mustAdd(t, child, NewNode("hold", "来源结束后才交还取消", nil, nil, func(rc *RunCtx) error {
				close(holdStarted)
				if mode == "healthy" {
					return nil
				}
				<-rc.Context().Done()
				select {
				case <-sourceObserved:
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case <-releaseHold:
					return rc.Context().Err()
				case <-ctx.Done():
					return ctx.Err()
				}
			}))
			messageTestOK(t, Sub(root, "mount", "保留子取消来源", []SubBind{Out(inside, outside)}, child))
			probe := &checkpointProbe{}
			hooks := checkpointStreamTestHooks(probe)
			hooks.OpenChild = func(_ context.Context, req CheckpointChild) ([]NodeCheckpoint, error) {
				d := CheckpointStreamDescriptor{
					ID:    CheckpointStreamID{Scope: req.Attempt, NodeID: "source", Key: "inside", Group: "default", Generation: 1},
					Graph: req.Graph, Path: req.Site.Path, Attempt: "source-attempt", Element: reflect.TypeFor[int](),
					Source: CheckpointStreamSource{ID: "finite"}, Capacity: 1, Terminal: StreamCompleted,
				}
				return []NodeCheckpoint{{NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{
					{Key: checkpointKey(inside.asRef()), State: SlotStateReady, Stream: &d},
				}}}, nil
			}
			entered, releaseOpen := make(chan context.Context, 1), make(chan struct{})
			var openOnce sync.Once
			defer openOnce.Do(func() { close(releaseOpen) })
			var opens atomic.Int32
			var openReturned atomic.Bool
			hooks.StreamHooks.Open = func(openCtx context.Context, req CheckpointStreamOpen) (CheckpointStreamRecord, error) {
				opens.Add(1)
				entered <- openCtx
				// 已接纳的同步保存先保持在途；即使根取消，也允许它正常成功返回。
				select {
				case <-releaseOpen:
				case <-ctx.Done():
					return CheckpointStreamRecord{}, ctx.Err()
				}
				defer openReturned.Store(true)
				d := *req.Restore
				d.ID.Generation++
				return CheckpointStreamRecord{Stream: d, Resumed: true, LastSequence: 1,
					Pending: []CheckpointStreamMessage{{Sequence: 1, Value: 31}}}, nil
			}
			type event struct {
				reason NodeFinishReason
				err    error
			}
			childStarted := make(chan *Execution, 1)
			sourceDone, mountDone := make(chan event, 1), make(chan event, 1)
			var e *Execution
			observer := ObserverFunc{
				GraphStarted: func(site Site) {
					if site.Path == "mount" {
						childStarted <- e.Snapshot().Nodes[0].Child
					}
				},
				Finished: func(site Site, node string, reason NodeFinishReason, err error) {
					if site.Path == "mount" && node == "source" {
						sourceDone <- event{reason, err}
						close(sourceObserved)
					} else if site.Path == "" && node == "mount" {
						mountDone <- event{reason, err}
					}
				},
				GraphFinished: func(site Site, _ NodeFinishReason, _ error) {
					if site.Path == "" && !openReturned.Load() {
						t.Error("root Wait finalized before accepted Open returned")
					}
				},
			}
			var err error
			e, err = root.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks), WithExecObserver(observer))
			messageTestOK(t, err)
			defer e.Cancel()
			subscription, err := SubscribeStream(e, outside)
			messageTestOK(t, err)
			messageTestOK(t, e.Start())
			nested := waitCheckpointSignal(t, ctx, childStarted)
			openCtx := waitCheckpointSignal(t, ctx, entered)
			waitCheckpointSignal(t, ctx, holdStarted)
			if nested == nil || e.Context().Err() != nil || openCtx.Err() != nil {
				t.Fatal("probe did not reach healthy accepted Open")
			}
			result := make(chan error, 1)
			go func() { result <- e.Wait() }()
			var source event
			if mode != "healthy" {
				if mode == "cancel-child" {
					nested.Cancel()
				} else {
					e.Cancel()
				}
				source = waitCheckpointSignal(t, ctx, sourceDone)
				if source.reason != NodeCanceled || !errors.Is(source.err, context.Canceled) {
					t.Errorf("source cancellation=%s: %v", source.reason, source.err)
				}
				if mode == "cancel-child" && (e.Context().Err() != nil || openCtx.Err() != nil) {
					t.Error("child Cancel escaped into healthy parent/shared Open")
				}
				holdOnce.Do(func() { close(releaseHold) })
			}
			var mount event
			if mode != "healthy" {
				mount = waitCheckpointSignal(t, ctx, mountDone)
				select {
				case err := <-result:
					t.Errorf("Wait escaped held shared Open: %v", err)
				default:
				}
			}
			openOnce.Do(func() { close(releaseOpen) })
			if mode == "healthy" {
				delivery, err := subscription.Receive(ctx)
				messageTestOK(t, err)
				if delivery.Sequence != 1 || delivery.Value != 31 {
					t.Fatalf("restored tail=%+v", delivery)
				}
				messageTestOK(t, delivery.Ack())
				source = waitCheckpointSignal(t, ctx, sourceDone)
				mount = waitCheckpointSignal(t, ctx, mountDone)
			}
			err = waitCheckpointSignal(t, ctx, result)
			if mode == "healthy" {
				if err != nil || source.reason != NodeCompleted || mount.reason != NodeCompleted || len(probe.commits) != 2 {
					t.Errorf("healthy restore: source=%s mount=%s err=%v commits=%d", source.reason, mount.reason, err, len(probe.commits))
				}
				if _, err := subscription.Receive(ctx); err != io.EOF {
					t.Errorf("restored EOF=%v", err)
				}
			} else {
				for name, ev := range map[string]event{"source": source, "mount": mount, "graph": {NodeFinishReason(e.Snapshot().Graph.Status), err}} {
					var cancellation *executionCancellation
					if ev.reason != NodeCanceled || !errors.Is(ev.err, context.Canceled) || isCheckpointFailure(ev.err) ||
						(mode == "cancel-child" && !errors.As(ev.err, &cancellation)) {
						t.Errorf("cancel-only was promoted at %s: reason=%s type=%T err=%v", name, ev.reason, ev.err, ev.err)
					}
				}
				if nested.Snapshot().Graph.Status != GraphStatusCanceled || len(probe.commits) != 0 {
					t.Errorf("cancel-only result: child=%s Commit=%d", nested.Snapshot().Graph.Status, len(probe.commits))
				}
			}
			if len(probe.failures) != 0 || opens.Load() != 1 {
				t.Errorf("cancel-only consumed persistent budget: FailAttempt=%d shared Open=%d", len(probe.failures), opens.Load())
			}
			if e.Err() != err || e.Wait() != err {
				t.Error("final result changed")
			}
			t.Logf("mode=%s source=%s/%T mount=%s/%T graph=%s Open=%d FailAttempt=%d Commit=%d", mode, source.reason, source.err, mount.reason, mount.err, e.Snapshot().Graph.Status, opens.Load(), len(probe.failures), len(probe.commits))
		})
	}
}

func TestCheckpointRestoredChildCancellationDomain(t *testing.T) {
	for _, cancelRoot := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel-root-%t", cancelRoot), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			root, child := mustNew(t, ctx, "cancel-root"), mustNew(t, ctx, "cancel-child")
			inside, outside := NewKey[Stream[int]]("inside"), NewKey[Stream[int]]("outside")
			declareOut(t, child, NewPort(inside))
			mustAdd(t, child, NewNode("source", "已确认来源不能重跑", nil, Provides(inside), func(*RunCtx) error {
				return errors.New("restored source reran")
			}))
			// 固定父取消已发生、子通知尚未送达的合法窗口；驱动随后显式送达通知。
			delayed, cancelChild := context.WithCancel(context.WithoutCancel(ctx))
			defer cancelChild()
			delay := func(rc *RunCtx, next func(*RunCtx) error) error {
				cp := *rc
				cp.ctx = delayed
				return next(&cp)
			}
			messageTestOK(t, Sub(root, "mount", "恢复来源的取消域", []SubBind{Out(inside, outside)}, child, delay))
			mustAdd(t, root, NewNode("reader", "真实读取者", Requires(outside), nil, func(rc *RunCtx) error {
				if cancelRoot {
					<-rc.Context().Done()
					return rc.Context().Err()
				}
				stream, err := Get(rc, outside)
				if err != nil {
					return err
				}
				d, err := stream.Receive(rc)
				if err != nil || d.Value != 7 {
					return fmt.Errorf("restored tail=%d: %v", d.Value, err)
				}
				if err := d.Ack(); err != nil {
					return err
				}
				if _, err := stream.Receive(rc); err != io.EOF {
					return fmt.Errorf("restored EOF=%v", err)
				}
				return nil
			}))
			probe := &checkpointProbe{}
			hooks := checkpointStreamTestHooks(probe)
			hooks.OpenChild = func(_ context.Context, req CheckpointChild) ([]NodeCheckpoint, error) {
				d := CheckpointStreamDescriptor{
					ID:    CheckpointStreamID{Scope: req.Attempt, NodeID: "source", Key: "inside", Group: "default", Generation: 1},
					Graph: req.Graph, Path: req.Site.Path, Attempt: "source-attempt", Element: reflect.TypeFor[int](),
					Source: CheckpointStreamSource{ID: "finite"}, Capacity: 1, Terminal: StreamCompleted,
				}
				return []NodeCheckpoint{{NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{
					{Key: checkpointKey(inside.asRef()), State: SlotStateReady, Stream: &d},
				}}}, nil
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var opens atomic.Int32
			hooks.StreamHooks.Open = func(_ context.Context, req CheckpointStreamOpen) (CheckpointStreamRecord, error) {
				opens.Add(1)
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return CheckpointStreamRecord{}, ctx.Err()
				}
				d := *req.Restore
				d.ID.Generation++
				return CheckpointStreamRecord{Stream: d, Resumed: true, LastSequence: 1,
					Pending: []CheckpointStreamMessage{{Sequence: 1, Value: 7}}}, nil
			}
			type event struct {
				reason NodeFinishReason
				err    error
			}
			selected := make(chan string, 1)
			sourceDone, mountDone, childDone := make(chan event, 1), make(chan event, 1), make(chan event, 1)
			var e, sub *Execution
			observer := ObserverFunc{
				GraphStarted: func(site Site) {
					if site.Path == "mount" {
						sub = e.Snapshot().Nodes[0].Child
						// Start 尚未提交子节点，包装只记录 restored 分支真正选择的等待域。
						sub.ctx = &checkpointWaitContext{Context: sub.ctx, selected: selected, name: "child"}
					}
				},
				Finished: func(site Site, node string, reason NodeFinishReason, err error) {
					if site.Path == "mount" && node == "source" {
						sourceDone <- event{reason, err}
					} else if site.Path == "" && node == "mount" {
						mountDone <- event{reason, err}
					}
				},
				GraphFinished: func(site Site, reason NodeFinishReason, err error) {
					if site.Path == "mount" {
						childDone <- event{reason, err}
					} else if e.Err() != err || e.Snapshot().Graph.Status != GraphStatus(reason) {
						t.Error("root event/Err/Snapshot disagree")
					}
				},
			}
			var err error
			e, err = root.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks), WithExecObserver(observer))
			messageTestOK(t, err)
			defer e.Cancel()
			messageTestOK(t, e.Start())
			waitCheckpointSignal(t, ctx, entered)
			if cancelRoot {
				e.Cancel()
				// 旧实现从 root.ctx 先返回，并在健康 child 域把取消锁存为 failed；
				// 修后先选择 child.Done，直到驱动送达子通知才返回。两路均有事件，无需超时判红。
				var source event
				select {
				case <-selected:
					if sub.Context().Err() != nil {
						t.Error("child cancellation arrived before the controlled notification")
					}
					cancelChild()
					source = waitCheckpointSignal(t, ctx, sourceDone)
				case source = <-sourceDone:
					cancelChild()
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				childEvent := waitCheckpointSignal(t, ctx, childDone)
				mountEvent := waitCheckpointSignal(t, ctx, mountDone)
				for name, ev := range map[string]event{"source": source, "child": childEvent, "mount": mountEvent} {
					if ev.reason != NodeCanceled || !errors.Is(ev.err, context.Canceled) {
						t.Errorf("%s cancellation classified as %s: %v", name, ev.reason, ev.err)
					}
				}
				e.wg.Wait()
				result := make(chan error, 1)
				go func() { result <- e.Wait() }()
				for {
					e.mu.Lock()
					closing, done := e.streamClosing, e.done
					e.mu.Unlock()
					if done {
						t.Fatal("root Wait finalized before accepted Open returned")
					}
					if closing {
						break
					}
					if ctx.Err() != nil {
						t.Fatal(ctx.Err())
					}
					runtime.Gosched()
				}
				releaseOnce.Do(func() { close(release) })
				err = waitCheckpointSignal(t, ctx, result)
				for _, execution := range []*Execution{e, sub} {
					snapshot := execution.Snapshot()
					if !errors.Is(execution.Wait(), context.Canceled) || execution.Err() != snapshot.Graph.Err || snapshot.Graph.Status != GraphStatusCanceled {
						t.Errorf("canceled final result disagrees: %+v", snapshot.Graph)
					}
					if snapshot.Nodes[0].Status != NodeStatusCanceled {
						t.Errorf("source/mount snapshot=%s", snapshot.Nodes[0].Status)
					}
				}
				if !errors.Is(err, context.Canceled) || isCheckpointFailure(err) || len(probe.failures) != 0 || len(probe.commits) != 0 {
					t.Errorf("cancel-only result=%v failures=%d commits=%d", err, len(probe.failures), len(probe.commits))
				}
				_, value := sub.slotOf(inside.asRef()).snapshot()
				tail := value.(Stream[int]).Snapshot()
				if tail.Terminal != StreamCompleted || tail.LastSequence != 1 || tail.AckedThrough != 0 || tail.InFlight != 1 {
					t.Errorf("confirmed tail changed: %+v", tail)
				}
			} else {
				releaseOnce.Do(func() { close(release) })
				messageTestOK(t, e.Wait())
				if ev := waitCheckpointSignal(t, ctx, sourceDone); ev.reason != NodeCompleted || ev.err != nil {
					t.Errorf("healthy restore=%+v", ev)
				}
			}
			if opens.Load() != 1 {
				t.Errorf("shared Open calls=%d", opens.Load())
			}
		})
	}
}

func TestCheckpointCachedEndpointUsesCurrentWaiter(t *testing.T) {
	for _, entry := range []string{"restored-alias", "host-completion"} {
		for _, cancelCall := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel-%t", entry, cancelCall), func(t *testing.T) {
				ctx := checkpointTestContext(t)
				g := mustNew(t, ctx, "waiter-owner")
				key := NewKey[Stream[int]]("stream")
				n := NewNode("source", "共享来源", nil, Provides(key), nil)
				mustAdd(t, g, n)
				selected := make(chan string, 2)
				entered, release := make(chan context.Context, 1), make(chan struct{})
				var releaseOnce sync.Once
				defer releaseOnce.Do(func() { close(release) })
				var opens atomic.Int32
				hooks := checkpointStreamTestHooks(&checkpointProbe{})
				hooks.StreamHooks.Open = func(openCtx context.Context, req CheckpointStreamOpen) (CheckpointStreamRecord, error) {
					opens.Add(1)
					entered <- openCtx
					select {
					case <-release:
					case <-ctx.Done():
						return CheckpointStreamRecord{}, ctx.Err()
					}
					d := *req.Restore
					d.ID.Generation++
					return CheckpointStreamRecord{Stream: d, Resumed: true}, nil
				}
				owner, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
				messageTestOK(t, err)
				defer owner.Cancel()
				owner.ctx = &checkpointWaitContext{Context: owner.ctx, selected: selected, name: "owner"}
				callCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				caller, err := g.NewExecution(callCtx, Inputs{})
				messageTestOK(t, err)
				defer caller.Cancel()
				caller.ctx = &checkpointWaitContext{Context: caller.ctx, selected: selected, name: "caller"}
				d := CheckpointStreamDescriptor{
					ID:    CheckpointStreamID{Scope: "root", NodeID: "source", Key: "stream", Group: "default", Generation: 1},
					Graph: g.id, Attempt: "source-attempt", Element: reflect.TypeFor[int](),
					Source: CheckpointStreamSource{ID: "finite"}, Capacity: 1, Terminal: StreamCompleted,
				}
				// 同一个缓存端点由 owner 创建，另一个执行的 restored 别名/宿主负责当前等待。
				stream := (Stream[int]{}).pulseRestoreCheckpoint(owner, d, nil).(Stream[int])
				caller.restored = map[*Node]NodeCheckpoint{n: {NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{
					{Key: checkpointKey(key.asRef()), State: SlotStateReady, Stream: &d},
				}}}
				messageTestOK(t, caller.slotOf(key.asRef()).resolveValue(stream))
				result := make(chan error, 1)
				go func() {
					if entry == "restored-alias" {
						result <- caller.restoreCheckpointStreams(n)
					} else {
						result <- stream.pulseWaitReadCompletion(caller.ctx)
					}
				}()
				chosen := waitCheckpointSignal(t, ctx, selected)
				openCtx := waitCheckpointSignal(t, ctx, entered)
				if cancelCall {
					cancel()
					if chosen != "caller" {
						t.Errorf("wait selected endpoint creator %q instead of current caller", chosen)
						// 旧实现也正常释放并返回，用具体错误/成功判据报红，不靠挂死。
						releaseOnce.Do(func() { close(release) })
					}
					if err := waitCheckpointSignal(t, ctx, result); !errors.Is(err, context.Canceled) {
						t.Errorf("current caller cancellation=%v", err)
					}
					if openCtx.Err() != nil || owner.Context().Err() != nil {
						t.Errorf("caller canceled root shared Open: open=%v owner=%v", openCtx.Err(), owner.Context().Err())
					}
				} else {
					releaseOnce.Do(func() { close(release) })
					messageTestOK(t, waitCheckpointSignal(t, ctx, result))
				}
				releaseOnce.Do(func() { close(release) })
				owner.streamWork.Wait()
				messageTestOK(t, stream.state.ensureCheckpoint(ctx))
				if opens.Load() != 1 {
					t.Errorf("shared Open calls=%d", opens.Load())
				}
			})
		}
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
