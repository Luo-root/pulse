package pulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckpointHostStopJoinsExecutionCloseout(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("store-error=%t", fail), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "stop-closeout")
			key := NewKey[Stream[int]]("output")
			declareOut(t, g, NewPort(key))
			messageTestOK(t, ProduceStream(g, "source", "", key, func(*RunCtx, *StreamWriter[int]) error { return nil }, WithStreamSource("empty", true)))
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			cause := errors.New("Stop save failed")
			var commits, finished atomic.Int32
			var e *Execution
			hooks := checkpointStreamTestHooks(&checkpointProbe{})
			hooks.StreamHooks.Commit = func(hookCtx context.Context, record CheckpointMessageCommit) error {
				if len(record.Stops) != 1 {
					return errors.New("unexpected control record")
				}
				commits.Add(1)
				_, raw := e.slotOf(key.asRef()).snapshot()
				state := raw.(Stream[int]).state
				for _, mu := range []*sync.Mutex{&state.mu, &e.mu} {
					if !mu.TryLock() {
						return errors.New("Stop hook ran under an engine state lock")
					}
					mu.Unlock()
				}
				close(entered)
				select {
				case <-release:
				case <-hookCtx.Done():
					return hookCtx.Err()
				}
				if fail {
					return cause
				}
				return nil
			}
			var err error
			e, err = g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks), WithExecObserver(ObserverFunc{
				GraphFinished: func(Site, NodeFinishReason, error) { finished.Add(1) },
			}))
			messageTestOK(t, err)
			defer e.Cancel()
			sub, err := SubscribeStream(e, key)
			messageTestOK(t, err)
			messageTestOK(t, e.Start())
			e.wg.Wait() // 空来源与必需读取都已结束；终结尚未由 Wait 封口。
			stopResult := make(chan error, 1)
			go func() { stopResult <- sub.Stop(ctx) }()
			waitCheckpointSignal(t, ctx, entered)
			waitResults := make(chan error, 2)
			for range 2 {
				go func() { waitResults <- e.Wait() }()
			}
			for {
				e.mu.Lock()
				closing, done := e.streamClosing, e.done
				e.mu.Unlock()
				if done {
					t.Fatal("Wait finalized while accepted Stop was still saving")
				}
				if closing {
					break
				}
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				runtime.Gosched()
			}
			if finished.Load() != 0 || e.Snapshot().Graph.Status != GraphStatusRunning {
				t.Fatal("GraphFinished preceded accepted Stop completion")
			}
			once.Do(func() { close(release) })
			stopErr := waitCheckpointSignal(t, ctx, stopResult)
			first, second := waitCheckpointSignal(t, ctx, waitResults), waitCheckpointSignal(t, ctx, waitResults)
			if first != second || e.Err() != first || e.Wait() != first {
				t.Fatalf("unstable result: Stop=%v Wait=%v/%v Err=%v", stopErr, first, second, e.Err())
			}
			wantStatus := GraphStatusCompleted
			if fail {
				wantStatus = GraphStatusFailed
				if !isCheckpointFailure(first) || !errors.Is(first, cause) || first != stopErr {
					t.Fatalf("real Stop failure did not reach first Wait: Stop=%v Wait=%v", stopErr, first)
				}
			} else {
				messageTestOK(t, stopErr)
				messageTestOK(t, first)
				messageTestOK(t, sub.Stop(ctx)) // 终结后重复 Stop 不写、不捕获清理取消。
				if _, err := sub.Receive(ctx); err != io.EOF {
					t.Fatalf("stopped stream after Wait = %v", err)
				}
			}
			if finished.Load() != 1 || e.Snapshot().Graph.Status != wantStatus || e.Snapshot().Graph.Err != first || ctx.Err() != nil || commits.Load() != 1 {
				t.Fatalf("closeout facts differ: snapshot=%+v finished=%d commits=%d parent=%v", e.Snapshot().Graph, finished.Load(), commits.Load(), ctx.Err())
			}
		})
	}
}

func TestHostStreamFinalizedControlsPreserveEOFAndIdempotency(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, stopped := range []bool{false, true} {
			t.Run(fmt.Sprintf("checkpoint=%t/stopped=%t", checkpoint, stopped), func(t *testing.T) {
				ctx := checkpointTestContext(t)
				g := mustNew(t, ctx, "finalized-host")
				key := NewKey[Stream[int]]("output")
				declareOut(t, g, NewPort(key))
				messageTestOK(t, ProduceStream(g, "source", "", key, func(_ *RunCtx, writer *StreamWriter[int]) error {
					return writer.Send(7)
				}, WithStreamSource("finite", true)))
				var calls atomic.Int32
				var opts []ExecOption
				if checkpoint {
					hooks := checkpointStreamTestHooks(&checkpointProbe{})
					hooks.StreamHooks.Commit = func(context.Context, CheckpointMessageCommit) error { calls.Add(1); return nil }
					opts = append(opts, WithExecCheckpoint("root", hooks))
				}
				e, err := g.NewExecution(ctx, Inputs{}, opts...)
				messageTestOK(t, err)
				defer e.Cancel()
				sub, err := SubscribeStream(e, key)
				messageTestOK(t, err)
				messageTestOK(t, e.Start())
				delivery, err := sub.Receive(ctx)
				messageTestOK(t, err)
				if stopped {
					messageTestOK(t, sub.Stop(ctx))
				} else {
					messageTestOK(t, delivery.Ack())
				}
				messageTestOK(t, e.Wait())
				before := calls.Load()
				if _, err := sub.Receive(ctx); err != io.EOF {
					t.Fatalf("Receive after successful Wait=%v", err)
				}
				if stopped {
					messageTestOK(t, sub.Stop(ctx))
					if err := delivery.Ack(); !errors.Is(err, ErrDeliveryExpired) {
						t.Fatalf("stopped old delivery=%v", err)
					}
				} else {
					messageTestOK(t, delivery.Ack())
					err := sub.Stop(ctx)
					if checkpoint && err == nil {
						t.Fatal("new reliable Stop was accepted after finalization")
					}
					if !checkpoint {
						messageTestOK(t, err)
					}
				}
				if calls.Load() != before || e.Err() != nil || e.Wait() != nil || e.Snapshot().Graph.Status != GraphStatusCompleted {
					t.Fatalf("post-finalization controls changed result: calls=%d->%d Err=%v", before, calls.Load(), e.Err())
				}
			})
		}
	}
}

func TestCheckpointStopAndWriterFinishShareOperationOrder(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "stop-writer-order", WithMaxRunning(1))
	key := NewKey[Stream[int]]("output")
	declareOut(t, g, NewPort(key))
	bodyRelease, writerWaiting := make(chan struct{}), make(chan struct{})
	messageTestOK(t, ProduceStream(g, "source", "", key, func(rc *RunCtx, _ *StreamWriter[int]) error {
		select {
		case <-bodyRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
		rc.ctx = &messageWaitWitness{Context: rc.ctx, waiting: writerWaiting}
		return nil
	}, WithStreamSource("empty", true)))
	stopEntered, stopRelease, stopSaved := make(chan struct{}), make(chan struct{}), make(chan struct{})
	commitEntered := make(chan CheckpointCommit, 1)
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(stopRelease) })
	hooks := checkpointStreamTestHooks(&checkpointProbe{})
	hooks.StreamHooks.Commit = func(hookCtx context.Context, _ CheckpointMessageCommit) error {
		close(stopEntered)
		select {
		case <-stopRelease:
		case <-hookCtx.Done():
			return hookCtx.Err()
		}
		close(stopSaved)
		return nil
	}
	hooks.Commit = func(hookCtx context.Context, record CheckpointCommit) error {
		commitEntered <- record
		select {
		case <-stopSaved:
		case <-hookCtx.Done():
			return hookCtx.Err()
		}
		if len(record.Streams) != 1 || record.Streams[0].Terminal != StreamStopped || len(record.Node.Outputs) != 1 || !record.Node.Outputs[0].Stream.Stopped {
			return errors.New("writer committed a terminal sampled before Stop")
		}
		return nil
	}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	messageTestOK(t, err)
	defer e.Cancel()
	sub, err := SubscribeStream(e, key)
	messageTestOK(t, err)
	messageTestOK(t, e.Start())
	stopResult := make(chan error, 1)
	go func() { stopResult <- sub.Stop(ctx) }()
	waitCheckpointSignal(t, ctx, stopEntered)
	close(bodyRelease)
	select {
	case <-writerWaiting: // 正常实现先等流操作门，尚未把旧终态交给 Store。
	case record := <-commitEntered:
		t.Errorf("writer Commit overtook in-flight Stop: %+v", record.Streams)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	releaseOnce.Do(func() { close(stopRelease) })
	messageTestOK(t, waitCheckpointSignal(t, ctx, stopResult))
	messageTestOK(t, e.Wait())
	_, value := e.slotOf(key.asRef()).snapshot()
	if snapshot := value.(Stream[int]).Snapshot(); snapshot.Terminal != StreamStopped || !snapshot.Stopped || snapshot.Err != nil {
		t.Fatalf("local writer and reading group differ: %+v", snapshot)
	}
}

func TestCheckpointWriterInstallsTerminalWithinOperation(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "install-writer-terminal")
	key := NewKey[Stream[int]]("output")
	node := NewNode("source", "", nil, Provides(key), nil)
	mustAdd(t, g, node)
	hooks := checkpointStreamTestHooks(&checkpointProbe{})
	var stream Stream[int]
	hooks.Commit = func(context.Context, CheckpointCommit) error {
		if stream.Snapshot().Terminal != "" || len(stream.state.operation.gate) != 1 {
			return errors.New("writer commit did not hold its operation before local terminal installation")
		}
		if !stream.state.mu.TryLock() {
			return errors.New("writer commit held its state lock")
		}
		stream.state.mu.Unlock()
		return nil
	}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	messageTestOK(t, err)
	defer e.Cancel()
	rc := newRunCtx(e, node, ctx)
	defer rc.Cancel()
	messageTestOK(t, e.runCheckpointChain(node, rc, func(rc *RunCtx) error {
		writer, err := OpenStream(rc, key, WithStreamSource("empty", true))
		if err == nil {
			stream = Stream[int]{state: writer.state}
		}
		return err
	}))
	if snapshot := stream.Snapshot(); snapshot.Terminal != StreamCompleted || len(stream.state.operation.gate) != 0 {
		t.Fatalf("confirmed terminal was not installed before unlocking: %+v", snapshot)
	}
}

type lifecycleRPCCause struct{ error }

func (e *lifecycleRPCCause) Unwrap() error { return e.error }

func TestLifecycleFinishReasonUsesExecutionCancellation(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, shape := range []string{"rpc-deadline", "rpc-cancel", "host-cancel", "host-deadline", "execution-cancel", "node-cancel", "timeout"} {
			t.Run(fmt.Sprintf("checkpoint=%v/%s", checkpoint, shape), func(t *testing.T) {
				var host context.Context
				var cancel context.CancelFunc
				if shape == "host-deadline" {
					host, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
				} else {
					host, cancel = context.WithCancel(t.Context())
				}
				defer cancel()
				obs := &recordingObserver{}
				g := mustNew(t, host, "finish-reason", WithObserver(obs))
				cause := context.Canceled
				if shape == "rpc-deadline" || shape == "host-deadline" {
					cause = context.DeadlineExceeded
				}
				typed := &lifecycleRPCCause{cause}
				original := fmt.Errorf("independent RPC: %w", typed)
				var e *Execution
				var aspects []Aspect
				if shape == "timeout" {
					aspects = append(aspects, Timeout(time.Millisecond))
				}
				mustAdd(t, g, NewNode("n", "独立 RPC 错误与执行中断分开归因", nil, nil, func(rc *RunCtx) error {
					switch shape {
					case "host-cancel":
						cancel()
					case "execution-cancel":
						e.Cancel()
					case "node-cancel":
						rc.Cancel()
					}
					if shape == "host-deadline" || shape == "timeout" {
						<-rc.Context().Done()
					}
					return original
				}, aspects...))
				p := &checkpointProbe{}
				var opts []ExecOption
				if checkpoint {
					opts = append(opts, WithExecCheckpoint("root", p.hooks()))
				}
				var err error
				e, err = g.NewExecution(host, Inputs{}, opts...)
				if err != nil {
					t.Fatal(err)
				}
				defer e.Cancel()
				if err := e.Start(); err != nil {
					t.Fatal(err)
				}
				err = e.Wait()
				if shape == "timeout" {
					if err == nil || !strings.Contains(err.Error(), "timeout") {
						t.Fatalf("Timeout error changed: %v", err)
					}
				} else {
					var gotTyped *lifecycleRPCCause
					if !errors.Is(err, original) || !errors.Is(err, cause) || !errors.As(err, &gotTyped) || gotTyped != typed {
						t.Fatalf("original RPC cause lost: %v", err)
					}
				}
				want := NodeFailed
				if shape == "host-cancel" || shape == "host-deadline" || shape == "execution-cancel" {
					want = NodeCanceled
				}
				log := obs.snapshot()
				if countPref(log, "F:n:"+string(want)) != 1 || countPref(log, "GF:"+string(want)) != 1 {
					t.Errorf("host.Err=%v: want %s, callbacks=%v", host.Err(), want, log)
				}
				state := e.Snapshot()
				if state.Graph.Status != GraphStatus(want) || state.Nodes[0].Status != NodeStatus(want) {
					t.Errorf("want %s, graph=%s node=%s", want, state.Graph.Status, state.Nodes[0].Status)
				}
				if state.Graph.Err != err || state.Nodes[0].Err != err || e.Err() != err || e.Wait() != err {
					t.Error("node, graph and repeated Wait did not share the latched error")
				}
				if (shape == "rpc-cancel" || shape == "rpc-deadline" || shape == "node-cancel" || shape == "timeout") && host.Err() != nil {
					t.Fatal("node failure canceled the host")
				}
				if checkpoint {
					wantFailures := 1
					if want == NodeCanceled {
						wantFailures = 0
					}
					if len(p.failures) != wantFailures || len(p.commits) != 0 {
						t.Fatalf("checkpoint failures=%v commits=%d want failures=%d", p.failures, len(p.commits), wantFailures)
					}
				}
			})
		}
	}
}

func TestLifecycleRPCFailureKeepsOriginAcrossSub(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			obs := &recordingObserver{}
			parent := mustNew(t, ctx, "parent", WithObserver(obs))
			child := mustNew(t, ctx, "child")
			cause := fmt.Errorf("child RPC: %w", context.DeadlineExceeded)
			input := NewKey[int]("input")
			declareIn(t, child, NewPort(input))
			mustAdd(t, child, NewNode("leaf", "子执行真实失败", Requires(input), nil, func(*RunCtx) error { return cause }))
			if err := Sub(parent, "mount", "失败来源跨挂载保留", []SubBind{Fix(input, 1)}, child); err != nil {
				t.Fatal(err)
			}
			p := &checkpointProbe{}
			var opts []ExecOption
			if checkpoint {
				opts = append(opts, WithExecCheckpoint("root", p.hooks()))
			}
			e, err := parent.NewExecution(ctx, Inputs{}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Cancel()
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			if err := e.Wait(); !errors.Is(err, cause) {
				t.Fatal(err)
			}
			state := e.Snapshot()
			childState := state.Nodes[0].Child.Snapshot()
			if state.Graph.Status != GraphStatusFailed || state.Nodes[0].Status != NodeStatusFailed || childState.Graph.Status != GraphStatusFailed || childState.Nodes[0].Status != NodeStatusFailed {
				t.Fatalf("origin changed across Sub: parent=%s/%s child=%s/%s", state.Graph.Status, state.Nodes[0].Status, childState.Graph.Status, childState.Nodes[0].Status)
			}
			if checkpoint && (len(p.failures) != 2 || len(p.commits) != 0) {
				t.Fatalf("Sub failure confirmation: failures=%v commits=%d", p.failures, len(p.commits))
			}
		})
	}
}

func TestLifecycleIndependentRPCFailureCancelsSibling(t *testing.T) {
	ctx := checkpointTestContext(t)
	obs := &recordingObserver{}
	g := mustNew(t, ctx, "rpc-sibling", WithObserver(obs))
	entered := make(chan struct{})
	cause := fmt.Errorf("independent RPC: %w", context.DeadlineExceeded)
	mustAdd(t, g, NewNode("rpc", "自身 RPC 失败", nil, nil, func(*RunCtx) error {
		<-entered
		return cause
	}))
	mustAdd(t, g, NewNode("sibling", "被首错取消", nil, nil, func(rc *RunCtx) error {
		close(entered)
		<-rc.Context().Done()
		return fmt.Errorf("sibling interrupted: %w", rc.Context().Err())
	}))
	e, err := g.NewExecution(ctx, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cancel()
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	state := e.Snapshot()
	if state.Graph.Status != GraphStatusFailed || state.Nodes[0].Status != NodeStatusFailed || state.Nodes[1].Status != NodeStatusCanceled {
		t.Fatalf("wrong failure source: graph=%s rpc=%s sibling=%s callbacks=%v", state.Graph.Status, state.Nodes[0].Status, state.Nodes[1].Status, obs.snapshot())
	}
}

func TestLifecycleStreamFailureReasonBeforePublication(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, panicking := range []bool{false, true} {
			for _, cause := range []error{errors.New("business failure"), context.Canceled, context.DeadlineExceeded} {
				t.Run(fmt.Sprintf("checkpoint=%t/panic=%t/%v", checkpoint, panicking, cause), func(t *testing.T) {
					ctx := checkpointTestContext(t)
					g := mustNew(t, ctx, "stream-reason")
					key := NewKey[Stream[int]]("messages")
					var stream Stream[int]
					mustAdd(t, g, NewNode("source", "业务和 panic 中的 context cause 不等于宿主取消", nil, Provides(key), func(rc *RunCtx) error {
						writer, err := OpenStream(rc, key, WithStreamSource("source", true))
						if err != nil {
							return err
						}
						stream = Stream[int]{state: writer.state}
						if panicking {
							return nil
						}
						return fmt.Errorf("independent RPC: %w", cause)
					}, func(rc *RunCtx, next func(*RunCtx) error) error {
						if err := next(rc); err != nil {
							return err
						}
						panic(cause)
					}))
					mustAdd(t, g, NewNode("sink", "流错误可能先被下游观察", Requires(key), nil, func(rc *RunCtx) error {
						s, err := Get(rc, key)
						if err != nil {
							return err
						}
						_, err = s.Receive(rc)
						return err
					}))
					probe := &checkpointProbe{}
					var finishes []CheckpointStreamFinish
					var opts []ExecOption
					if checkpoint {
						hooks := checkpointStreamTestHooks(probe)
						hooks.StreamHooks.Finish = func(_ context.Context, finish CheckpointStreamFinish) error {
							finishes = append(finishes, finish)
							return nil
						}
						opts = append(opts, WithExecCheckpoint("root", hooks))
					}
					e, err := g.NewExecution(ctx, Inputs{}, opts...)
					managedTestOK(t, err)
					defer e.Cancel()
					managedTestOK(t, e.Start())
					if err := e.Wait(); !errors.Is(err, cause) {
						t.Fatalf("original cause lost: %v", err)
					}
					if ctx.Err() != nil {
						t.Fatalf("host was canceled by the execution: %v", ctx.Err())
					}
					if snapshot := stream.Snapshot(); snapshot.Terminal != StreamFailed || !errors.Is(snapshot.Err, cause) {
						t.Errorf("stream failure was misclassified: %+v", snapshot)
					}
					if snapshot := e.Snapshot(); snapshot.Graph.Status != GraphStatusFailed || snapshot.Nodes[0].Status != NodeStatusFailed {
						t.Errorf("source/graph failure reason changed during propagation: %+v", snapshot)
					}
					if checkpoint && (len(finishes) != 1 || finishes[0].Terminal != StreamFailed) {
						t.Errorf("persistent stream failure was misclassified: %+v", finishes)
					}
				})
			}
		}
	}
}

func TestLifecycleStreamRealHostCancellation(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			ctx, cancel := context.WithCancel(checkpointTestContext(t))
			defer cancel()
			g := mustNew(t, ctx, "stream-real-cancel")
			key := NewKey[Stream[int]]("messages")
			var stream Stream[int]
			mustAdd(t, g, NewNode("source", "", nil, Provides(key), func(rc *RunCtx) error {
				writer, err := OpenStream(rc, key, WithStreamSource("source", true))
				if err != nil {
					return err
				}
				stream = Stream[int]{state: writer.state}
				cancel()
				return rc.Context().Err()
			}))
			mustAdd(t, g, NewNode("sink", "", Requires(key), nil, func(rc *RunCtx) error {
				s, err := Get(rc, key)
				if err != nil {
					return err
				}
				_, err = s.Receive(rc)
				return err
			}))
			var opts []ExecOption
			if checkpoint {
				opts = append(opts, WithExecCheckpoint("root", checkpointStreamTestHooks(&checkpointProbe{})))
			}
			if err := g.Run(ctx, Inputs{}, opts...); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if got := stream.Snapshot(); got.Terminal != StreamCanceled {
				t.Fatalf("real host cancellation became business failure: %+v", got)
			}
		})
	}
}

func TestLifecycleStreamFinishClassifiesLatchedFailure(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, message := range []bool{false, true} {
			for _, propagate := range []bool{false, true} {
				t.Run(fmt.Sprintf("checkpoint=%t/message=%t/propagate=%t", checkpoint, message, propagate), func(t *testing.T) {
					ctx := checkpointTestContext(t)
					obs := &recordingObserver{}
					g := mustNew(t, ctx, "finish-origin", WithObserver(obs))
					input, output := NewKey[Stream[int]]("input"), NewKey[Stream[int]]("output")
					cause := context.Canceled
					var requires []keyRef
					if message {
						cause = context.DeadlineExceeded
						requires = Requires(input)
						mustAdd(t, g, NewNode("input", "给消息处理提供交付", nil, Provides(input), func(rc *RunCtx) error {
							w, err := OpenStream(rc, input, WithStreamSource("input", true))
							if err != nil {
								return err
							}
							return w.Send(1)
						}))
					}
					var stream Stream[int]
					mustAdd(t, g, NewNode("processor", "忽略失败也必须按原来源收尾", requires, Provides(output), func(rc *RunCtx) error {
						writerRC := rc
						if !message {
							writerRC = rc.Fork()
							defer writerRC.Cancel()
						}
						w, err := OpenStream(writerRC, output, WithStreamSource("output", true))
						if err != nil {
							return err
						}
						stream = Stream[int]{state: w.state}
						if !message {
							writerRC.Cancel()
							if err := w.Send(1); !errors.Is(err, cause) {
								return fmt.Errorf("Fork cancellation changed: %v", err)
							}
							return nil
						}
						in, err := Get(rc, input)
						if err != nil {
							return err
						}
						delivery, err := in.Receive(rc)
						if err != nil {
							return err
						}
						if err := HandleMessage(rc, delivery, func(*RunCtx, int) error {
							return fmt.Errorf("independent RPC: %w", cause)
						}); !errors.Is(err, cause) {
							return fmt.Errorf("message cause changed: %v", err)
						}
						return in.Stop(rc)
					}))
					mustAdd(t, g, NewNode("sink", "读取终态后可继续传播原错误", Requires(output), nil, func(rc *RunCtx) error {
						out, err := Get(rc, output)
						if err != nil {
							return err
						}
						_, err = out.Receive(rc)
						if propagate {
							return err
						}
						return nil
					}))
					var opts []ExecOption
					var mu sync.Mutex
					var terminals []CheckpointStreamFinish
					if checkpoint {
						hooks := checkpointStreamTestHooks(&checkpointProbe{})
						hooks.StreamHooks.Finish = func(_ context.Context, finish CheckpointStreamFinish) error {
							mu.Lock()
							terminals = append(terminals, finish)
							mu.Unlock()
							return nil
						}
						opts = append(opts, WithExecCheckpoint("root", hooks))
					}
					e, err := g.NewExecution(ctx, Inputs{}, opts...)
					managedTestOK(t, err)
					defer e.Cancel()
					managedTestOK(t, e.Start())
					if err := e.Wait(); !errors.Is(err, cause) {
						t.Fatalf("original failure lost: %v", err)
					}
					if got := stream.Snapshot(); got.Terminal != StreamFailed || !errors.Is(got.Err, cause) {
						t.Errorf("latched failure published as %+v", got)
					}
					state := e.Snapshot()
					processor := snapshotNode(t, state, "processor")
					if ctx.Err() != nil || state.Graph.Status != GraphStatusFailed || processor.Status != NodeStatusFailed {
						t.Errorf("origin changed: host=%v graph=%s node=%s", ctx.Err(), state.Graph.Status, processor.Status)
					}
					if log := obs.snapshot(); countPref(log, "F:processor:failed") != 1 || countPref(log, "GF:failed") != 1 {
						t.Errorf("wrong observer origin: %v", log)
					}
					if checkpoint {
						found := false
						for _, terminal := range terminals {
							if terminal.Stream.NodeID == "processor" {
								found = true
								if terminal.Terminal != StreamFailed {
									t.Errorf("persistent terminal = %s", terminal.Terminal)
								}
							}
						}
						if !found {
							t.Error("processor failure terminal not confirmed")
						}
					}
				})
			}
		}
	}
}

type lifecycleFinishWitness struct {
	cause   error
	publish func(error)
}

func (w lifecycleFinishWitness) failure() error   { return w.cause }
func (w lifecycleFinishWitness) finish(err error) { w.publish(err) }

func TestLifecycleStreamOriginIsLatchedBeforePublication(t *testing.T) {
	for _, message := range []bool{false, true} {
		t.Run(fmt.Sprintf("message=%t", message), func(t *testing.T) {
			writer, stream, rc, _ := managedTestEndpoint[int](t, 1)
			cause := fmt.Errorf("independent RPC: %w", context.DeadlineExceeded)
			witness := lifecycleFinishWitness{publish: func(err error) {
				if finishReason(err) != NodeFailed {
					t.Errorf("published before failure classification: %v", err)
				}
				rc.exec.Cancel() // 模拟下游刚观察错误就取消整图，随后不得改判来源。
			}}
			if message {
				rc.streams.failMessage(cause)
			} else {
				witness.cause = cause
			}
			rc.streams.outputs = []streamLifecycle{witness, writer.state}
			err := rc.finishStreams(nil)
			if finishReason(err) != NodeFailed || !errors.Is(err, cause) || stream.Snapshot().Terminal != StreamFailed {
				t.Fatalf("publication cancellation changed source: err=%v stream=%+v", err, stream.Snapshot())
			}
		})
	}
}

func TestLifecycleStreamFailureOriginSurvivesLocalCleanup(t *testing.T) {
	for _, message := range []bool{false, true} {
		t.Run(fmt.Sprintf("message=%t", message), func(t *testing.T) {
			writer, stream, rc, _ := managedTestEndpoint[int](t, 1)
			var cause error
			if message {
				cause = fmt.Errorf("independent RPC: %w", context.DeadlineExceeded)
				rc.streams.failMessage(cause)
			} else {
				fork := rc.Fork()
				forkWriter := *writer
				forkWriter.rc = fork
				fork.Cancel()
				cause = context.Canceled
				if err := forkWriter.Send(1); !errors.Is(err, cause) {
					t.Fatal(err)
				}
			}
			rc.Cancel() // 后来的清理不能改写此前健康节点已经确认的失败来源。
			err := rc.finishStreams(nil)
			if !errors.Is(err, cause) || finishReason(err) != NodeFailed || stream.Snapshot().Terminal != StreamFailed {
				t.Fatalf("cleanup changed recorded failure: err=%v stream=%+v", err, stream.Snapshot())
			}
		})
	}
}

func TestLifecycleStreamCancellationOriginSurvivesForwarding(t *testing.T) {
	writer, stream, rc, _ := managedTestEndpoint[int](t, 1)
	rc.Cancel()
	err := writer.Send(1)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	err = rc.finishStreams(nil)
	if stream.Snapshot().Terminal != StreamCanceled {
		t.Fatalf("actual node cancellation became failure: %+v", stream.Snapshot())
	}
	// 另一个健康执行收到已确认的取消事实时不能把它重新解释成独立 RPC 失败。
	_, _, other, _ := managedTestEndpoint[int](t, 1)
	if got := other.exec.classifyFailure(err); finishReason(got) != NodeCanceled || !errors.Is(got, context.Canceled) {
		t.Fatalf("cancellation origin was reclassified by a healthy execution: %v", got)
	}
}

// 错误链允许只读诊断；Unwrap 不应在持有同一流状态锁时被调用。
type streamSnapshotError struct {
	stream  Stream[int]
	cause   error
	reenter bool
}

func (*streamSnapshotError) Error() string { return "stream diagnostic failure" }
func (e *streamSnapshotError) Unwrap() error {
	if e.reenter {
		_ = e.stream.Snapshot()
	}
	return e.cause
}

// 子进程有自己的测试超时：回归成锁重入时必须明确失败，不能拖死整套测试。
func TestManagedStreamErrorReentry(t *testing.T) {
	if mode := os.Getenv("PULSE_TEST_STREAM_ERROR_REENTRY"); mode != "" {
		testManagedStreamErrorReentry(t, mode)
		return
	}
	for _, path := range []string{"local-writer", "checkpoint-writer", "reader", "latched-writer"} {
		for _, reenter := range []bool{false, true} {
			mode := fmt.Sprintf("%s/%t", path, reenter)
			t.Run(mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagedStreamErrorReentry$", "-test.timeout=2s", "-test.v")
				cmd.Env = append(os.Environ(), "PULSE_TEST_STREAM_ERROR_REENTRY="+mode, "GORACE=atexit_sleep_ms=0")
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("stream error classification did not complete: %v\n%s", err, output)
				}
			})
		}
	}
}

func testManagedStreamErrorReentry(t *testing.T, mode string) {
	t.Helper()
	path, reenter, ok := strings.Cut(mode, "/")
	if !ok {
		t.Fatalf("invalid child mode %q", mode)
	}
	cause := errors.New("original business failure")
	if path == "reader" || path == "latched-writer" {
		writer, stream, _, readerRC := managedTestEndpoint[int](t, 1)
		failure := &streamSnapshotError{stream: stream, cause: cause, reenter: reenter == "true"}
		if path == "reader" {
			if err := stream.pulseReaderFinished(readerRC, failure); !errors.Is(err, cause) {
				t.Fatalf("reader failure changed: %v", err)
			}
			if !errors.Is(writer.state.failure(), cause) {
				t.Fatal("reader failure was not latched for the writer")
			}
			return
		}
		writer.state.recordFailure(failure)
		writer.state.finish(nil) // 完整链表面成功，也必须保留此前发送失败。
		if snapshot := stream.Snapshot(); snapshot.Terminal != StreamFailed || !errors.Is(snapshot.Err, cause) {
			t.Fatalf("latched writer failure changed: %+v", snapshot)
		}
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	g, err := New("error-reentry", "错误链读取流快照")
	managedTestOK(t, err)
	key := NewKey[Stream[int]]("events")
	managedTestOK(t, g.DeclareOutput(NewPort(key)))
	var stream Stream[int]
	managedTestOK(t, g.Add(NewNode("source", "完整链失败仍须收尾", nil, Provides(key), func(rc *RunCtx) error {
		_, err := OpenStream(rc, key, WithStreamSource("finite-source", false))
		if err != nil {
			return err
		}
		stream, err = Get(rc, key)
		if err != nil {
			return err
		}
		return &streamSnapshotError{stream: stream, cause: cause, reenter: reenter == "true"}
	})))
	var opts []ExecOption
	if path == "checkpoint-writer" {
		opts = append(opts, WithExecCheckpoint("root", checkpointStreamTestHooks(&checkpointProbe{})))
	} else if path != "local-writer" {
		t.Fatalf("unknown child path %q", path)
	}
	e, err := g.NewExecution(ctx, Inputs{}, opts...)
	managedTestOK(t, err)
	defer e.Cancel()
	_, err = SubscribeStream(e, key)
	managedTestOK(t, err)
	managedTestOK(t, e.Start())
	if err := e.Wait(); !errors.Is(err, cause) {
		t.Fatalf("business failure changed: %v", err)
	}
	if state := e.Snapshot(); state.Graph.Status != GraphStatusFailed {
		t.Fatalf("business failure became %s", state.Graph.Status)
	}
	if snapshot := stream.Snapshot(); snapshot.Terminal != StreamFailed || !errors.Is(snapshot.Err, cause) {
		t.Fatalf("writer terminal changed: %+v", snapshot)
	}
}
