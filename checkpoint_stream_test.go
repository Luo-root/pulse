package pulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync/atomic"
	"testing"
)

// 仅验证 Core 时点与载荷，不冒充真实持久后端或跨进程验收。
func checkpointStreamTestHooks(probe *checkpointProbe) CheckpointHooks {
	hooks := probe.hooks()
	hooks.StreamHooks = &CheckpointStreamHooks{
		Open: func(_ context.Context, request CheckpointStreamOpen) (CheckpointStreamRecord, error) {
			return CheckpointStreamRecord{Stream: CheckpointStreamDescriptor{
				ID:    CheckpointStreamID{Scope: request.Scope, NodeID: request.NodeID, Key: request.Key.Name, Group: request.Group, Generation: 1},
				Graph: request.Site.Graph, Path: request.Site.Path, Attempt: request.Attempt, Element: request.Key.Element,
				Source: request.Source, Capacity: request.Capacity,
			}}, nil
		},
		Append: func(context.Context, CheckpointStreamAppend) error { return nil },
		Commit: func(context.Context, CheckpointMessageCommit) error { return nil },
		OpenMessage: func(_ context.Context, request CheckpointMessageAttemptRequest) (CheckpointMessageAttempt, error) {
			return CheckpointMessageAttempt{Attempt: CheckpointAttempt{Scope: request.Attempt + "/message", Index: 1}}, nil
		},
		FailMessage: func(context.Context, string) error { return nil },
		Finish:      func(context.Context, CheckpointStreamFinish) error { return nil },
	}
	return hooks
}

func waitCheckpointSignal[T any](t *testing.T, ctx context.Context, signal <-chan T) T {
	t.Helper()
	select {
	case value := <-signal:
		return value
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		var zero T
		return zero
	}
}

func TestCheckpointStreamConfirmsAppendAndTerminalBeforePublishing(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "stream-confirmation")
	key, ordinary := NewKey[Stream[int]]("stream"), NewKey[int]("ordinary")
	appendEntered, appendRelease := make(chan CheckpointStreamAppend, 1), make(chan struct{})
	finishEntered, finishRelease := make(chan CheckpointCommit, 1), make(chan struct{})
	received, eof := make(chan int, 1), make(chan struct{}, 1)
	mustAdd(t, g, NewNode("source", "渐进输出与普通值共存", nil, Deps(Provides(key), Provides(ordinary)), func(rc *RunCtx) error {
		writer, err := OpenStream(rc, key, WithBuffer(2), WithStreamSource("numbers", true))
		if err != nil {
			return err
		}
		if err := Set(rc, ordinary, 9); err != nil {
			return err
		}
		return writer.SendAt(3, "position-3")
	}))
	mustAdd(t, g, NewNode("sink", "确认后交付，终态确认后 EOF", Requires(key), nil, func(rc *RunCtx) error {
		stream, err := Get(rc, key)
		if err != nil {
			return err
		}
		delivery, err := stream.Receive(rc)
		if err != nil {
			return err
		}
		received <- delivery.Value
		if err := delivery.Ack(); err != nil {
			return err
		}
		if _, err := stream.Receive(rc); err != io.EOF {
			return fmt.Errorf("expected EOF, got %v", err)
		}
		eof <- struct{}{}
		return nil
	}))
	probe := &checkpointProbe{}
	hooks := checkpointStreamTestHooks(probe)
	hooks.StreamHooks.Append = func(ctx context.Context, append CheckpointStreamAppend) error {
		appendEntered <- append
		select {
		case <-appendRelease:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	baseCommit := hooks.Commit
	hooks.Commit = func(ctx context.Context, commit CheckpointCommit) error {
		if commit.Node.NodeID == "source" {
			finishEntered <- commit
			select {
			case <-finishRelease:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return baseCommit(ctx, commit)
	}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	append := waitCheckpointSignal(t, ctx, appendEntered)
	if append.Message.Sequence != 1 || append.Message.Value != 3 || append.Cursor != "position-3" {
		t.Fatalf("incomplete reliable append: %+v", append)
	}
	_, endpoint := e.slotOf(key.asRef()).snapshot()
	if snapshot := endpoint.(Stream[int]).Snapshot(); snapshot.LastSequence != 0 || snapshot.InFlight != 0 {
		t.Fatalf("append became visible before confirmation: %+v", snapshot)
	}
	if state, _ := e.slotOf(ordinary.asRef()).snapshot(); state != slotPending {
		t.Fatal("ordinary Set bypassed the complete-node checkpoint frame")
	}
	close(appendRelease)
	if value := waitCheckpointSignal(t, ctx, received); value != 3 {
		t.Fatalf("received %d", value)
	}
	commit := waitCheckpointSignal(t, ctx, finishEntered)
	if len(commit.Streams) != 1 || commit.Streams[0].Terminal != StreamCompleted || commit.Streams[0].LastSequence != 1 {
		t.Fatalf("source terminal is not part of complete Commit: %+v", commit)
	}
	for _, output := range commit.Node.Outputs {
		if output.Key.Name == key.Name() && (output.Value != nil || output.Stream == nil || output.Stream.Terminal != StreamCompleted) {
			t.Fatalf("endpoint leaked into ordinary Codec payload: %+v", output)
		}
	}
	if terminal := endpoint.(Stream[int]).Snapshot().Terminal; terminal != "" {
		t.Fatalf("terminal visible before reliable complete Commit: %q", terminal)
	}
	select {
	case <-eof:
		t.Fatal("EOF opened before confirmation")
	default:
	}
	close(finishRelease)
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
	if state, value := e.slotOf(ordinary.asRef()).snapshot(); state != slotReady || value != 9 {
		t.Fatalf("ordinary output did not publish after Commit: %v %v", state, value)
	}
}

func TestCheckpointStreamUncertainAppendStopsWithoutPublishingOrRetrying(t *testing.T) {
	for _, panicHook := range []bool{false, true} {
		t.Run(fmt.Sprint(panicHook), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "uncertain-stream")
			key := NewKey[Stream[int]]("stream")
			var calls atomic.Int32
			swallow := func(rc *RunCtx, next func(*RunCtx) error) error { _ = next(rc); return nil }
			mustAdd(t, g, NewNode("source", "不确定结果不能假成功", nil, Provides(key), func(rc *RunCtx) error {
				writer, err := OpenStream(rc, key, WithStreamSource("numbers", true))
				if err != nil {
					return err
				}
				_ = writer.Send(1)
				return writer.Send(2)
			}, swallow))
			mustAdd(t, g, NewNode("sink", "不能收到未确认记录", Requires(key), nil, func(rc *RunCtx) error {
				stream, err := Get(rc, key)
				if err != nil {
					return err
				}
				_, err = stream.Receive(rc)
				if err == nil {
					return errors.New("unconfirmed message was delivered")
				}
				return err
			}))
			probe := &checkpointProbe{}
			hooks := checkpointStreamTestHooks(probe)
			hooks.StreamHooks.Append = func(context.Context, CheckpointStreamAppend) error {
				calls.Add(1) // 模拟已写成，但响应丢失；权威记录留给后续恢复读取。
				if panicHook {
					panic("storage panic")
				}
				return fmt.Errorf("written but response lost: %w", ErrSkipped)
			}
			e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			err = e.Wait()
			var failure *CheckpointError
			if !errors.As(err, &failure) || isSkipped(err) || calls.Load() != 1 || len(probe.failures) != 0 {
				t.Fatalf("uncertain append was swallowed/retried/failed as business: err=%v calls=%d fails=%v", err, calls.Load(), probe.failures)
			}
			_, raw := e.slotOf(key.asRef()).snapshot()
			if snapshot := raw.(Stream[int]).Snapshot(); snapshot.LastSequence != 0 || snapshot.InFlight != 0 {
				t.Fatalf("unconfirmed append published: %+v", snapshot)
			}
		})
	}
}

func TestCheckpointStreamFailedAspectConfirmsTerminalBeforeFinish(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "failed-stream-terminal")
			key := NewKey[Stream[int]]("stream")
			business := errors.New("outer aspect failed after sending")
			acked := make(chan struct{}, 1)
			outer := func(rc *RunCtx, next func(*RunCtx) error) error {
				if err := next(rc); err != nil {
					return err
				}
				return business
			}
			mustAdd(t, g, NewNode("source", "终态来自完整切面链", nil, Provides(key), func(rc *RunCtx) error {
				writer, err := OpenStream(rc, key, WithStreamSource("numbers", true))
				if err != nil {
					return err
				}
				return writer.Send(1)
			}, outer))
			mustAdd(t, g, NewNode("sink", "失败不能变成 EOF", Requires(key), nil, func(rc *RunCtx) error {
				stream, err := Get(rc, key)
				if err != nil {
					return err
				}
				for {
					delivery, err := stream.Receive(rc)
					if err != nil {
						if err == io.EOF {
							return errors.New("failed source became successful EOF")
						}
						return err
					}
					if err := delivery.Ack(); err != nil {
						return err
					}
					acked <- struct{}{}
				}
			}))
			entered, release := make(chan CheckpointStreamFinish, 1), make(chan struct{})
			probe := &checkpointProbe{}
			hooks := checkpointStreamTestHooks(probe)
			sourceOpened := make(chan CheckpointAttempt, 1)
			baseOpen := hooks.OpenAttempt
			hooks.OpenAttempt = func(ctx context.Context, request CheckpointAttemptRequest) (CheckpointAttempt, error) {
				attempt, err := baseOpen(ctx, request)
				if request.NodeID == "source" && request.Boundary == "" {
					sourceOpened <- attempt
				}
				return attempt, err
			}
			type failureEvent struct {
				scope     string
				confirmed bool
			}
			failed := make(chan failureEvent, 2)
			var finishConfirmed atomic.Bool
			baseFail := hooks.FailAttempt
			hooks.FailAttempt = func(ctx context.Context, scope string) error {
				err := baseFail(ctx, scope)
				failed <- failureEvent{scope: scope, confirmed: finishConfirmed.Load()}
				return err
			}
			var calls atomic.Int32
			hooks.StreamHooks.Finish = func(ctx context.Context, finish CheckpointStreamFinish) error {
				calls.Add(1)
				entered <- finish
				select {
				case <-release:
					if uncertain {
						return fmt.Errorf("terminal may be saved: %w", ErrSkipped)
					}
					finishConfirmed.Store(true)
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			finish := waitCheckpointSignal(t, ctx, entered)
			if finish.Terminal != StreamFailed || finish.LastSequence != 1 {
				t.Fatalf("body success hid full-chain failure: %+v", finish)
			}
			_, raw := e.slotOf(key.asRef()).snapshot()
			if raw.(Stream[int]).Snapshot().Terminal != "" {
				t.Fatal("failure terminal published before reliable Finish confirmation")
			}
			source := waitCheckpointSignal(t, ctx, sourceOpened)
			waitCheckpointSignal(t, ctx, acked)
			if uncertain {
				close(release)
			} else {
				// 等 sink 已进入读取后，暂缓 e.fail 的执行域取消：source 的真实
				// 终态先开放，sink 因同一业务错误退出并合法确认自己的失败尝试。
				// 这把锁不覆盖流终结和可靠钩子，故两个 FailAttempt 都能结束。
				func() {
					e.mu.Lock()
					defer e.mu.Unlock()
					close(release)
					first := waitCheckpointSignal(t, ctx, failed)
					second := waitCheckpointSignal(t, ctx, failed)
					if first.scope != source.Scope || !first.confirmed || second.scope == source.Scope {
						t.Fatalf("source failure must follow Finish, then sink may fail: source=%s failures=%+v %+v", source.Scope, first, second)
					}
				}()
			}
			err = e.Wait()
			if calls.Load() != 1 {
				t.Fatalf("Finish retried %d times", calls.Load())
			}
			sourceFailures := 0
			for _, scope := range probe.failures {
				if scope == source.Scope {
					sourceFailures++
				}
			}
			if uncertain {
				var failure *CheckpointError
				if !errors.As(err, &failure) || isSkipped(err) || sourceFailures != 0 {
					t.Fatalf("uncertain Finish invalidated source attempt: err=%v failures=%v", err, probe.failures)
				}
			} else if !errors.Is(err, business) || sourceFailures != 1 {
				t.Fatalf("confirmed terminal lost real failure: err=%v source=%s failures=%v", err, source.Scope, probe.failures)
			}
		})
	}
}

func TestCheckpointChildBindingsDoNotAwaitPendingInputs(t *testing.T) {
	ctx := checkpointTestContext(t)
	parent, child := mustNew(t, ctx, "parent-bindings"), mustNew(t, ctx, "child-bindings")
	parentInput, childInput := NewKey[int]("late-parent"), NewKey[int]("late-child")
	fixed, childOut, parentOut := NewKey[int]("fixed"), NewKey[int]("child-out"), NewKey[int]("parent-out")
	declareIn(t, child, NewPort(childInput), NewPort(fixed))
	declareOut(t, child, NewPort(childOut))
	mustAdd(t, child, NewNode("independent", "不等无关入口", Requires(fixed), Provides(childOut), func(rc *RunCtx) error {
		value, err := Get(rc, fixed)
		if err != nil {
			return err
		}
		return Set(rc, childOut, value)
	}))
	opened, proceed := make(chan CheckpointChild, 1), make(chan struct{})
	mustAdd(t, parent, NewNode("late-source", "等 child Open 完成后才供给", nil, Provides(parentInput), func(rc *RunCtx) error {
		select {
		case <-proceed:
			return Set(rc, parentInput, 7)
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	}))
	if err := Sub(parent, "mount", "pending In 不阻止 child Open", []SubBind{In(parentInput, childInput), Fix(fixed, 11), Out(childOut, parentOut)}, child); err != nil {
		t.Fatal(err)
	}
	probe := &checkpointProbe{}
	hooks := probe.hooks()
	hooks.OpenChild = func(_ context.Context, request CheckpointChild) ([]NodeCheckpoint, error) {
		opened <- request
		close(proceed)
		return nil, nil
	}
	e, err := parent.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	request := waitCheckpointSignal(t, ctx, opened)
	if len(request.Bindings) != 1 || request.Bindings[0].Scope != "root" || request.Bindings[0].Attempt != request.Attempt ||
		request.Bindings[0].Parent.Name != parentInput.Name() || request.Bindings[0].Child.Name != childInput.Name() ||
		len(request.Inputs) != 1 || request.Inputs[0].Key.Name != fixed.Name() || request.Inputs[0].Value != 11 {
		t.Fatalf("pending binding became missing input/fake outcome: %+v", request)
	}
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointStreamMetadataAndUnresumableSource(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "metadata")
	key := NewKey[Stream[map[string]int]]("records")
	mustAdd(t, g, NewNode("source", "真实元素类型", nil, Provides(key), nil))
	if declaration := g.CheckpointNodes()[0].Provides[0]; declaration.Type != reflect.TypeFor[Stream[map[string]int]]() || declaration.Element != reflect.TypeFor[map[string]int]() {
		t.Fatalf("missing actual stream element type: %+v", declaration)
	}
	probe := &checkpointProbe{}
	hooks := checkpointStreamTestHooks(probe)
	request := CheckpointStreamOpen{Site: Site{Graph: "metadata"}, Scope: "root", Attempt: "source", NodeID: "source", Key: checkpointKey(key.asRef()), Group: "default", Source: CheckpointStreamSource{ID: "not-replayable"}, Capacity: 1}
	record, err := hooks.StreamHooks.Open(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	record.Resumed = true
	if err := checkCheckpointStreamRecord(request, record); err == nil {
		t.Fatal("active closure was treated as resumable without a source promise")
	}
	if err := checkCheckpointOutput(CheckpointOutput{Key: checkpointKey(key.asRef()), State: SlotStateReady, Value: Stream[map[string]int]{}}); err == nil {
		t.Fatal("live endpoint entered ordinary Codec payload")
	}
}
