package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 屏障固定在恢复槽位已发布、权威 Open 尚未返回之间；不靠调度概率或 sleep。
// 这里只验证 Core 钩子契约，不冒充文件后端或进程退出/重启验收。
func TestCheckpointFanOutRestoreReadyBeforeReaderFinish(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		for _, skipped := range []bool{false, true} {
			t.Run(fmt.Sprintf("stopped=%t/skip=%t", stopped, skipped), func(t *testing.T) {
				f := newCheckpointReaderRestoreFixture(t, stopped, false, func(*RunCtx) error {
					if skipped {
						return NoValue()
					}
					return nil
				})
				f.awaitCloseout(t)
				f.unblockOpen()
				if err := f.exec.Wait(); err != nil {
					t.Errorf("normal restored reader failed: %v", err)
				}
				f.assertConfirmation(t, 1, 0)
				stream := f.stream(t)
				snapshot := stream.Snapshot()
				wantTerminal, wantAcked := StreamCompleted, uint64(1)
				if stopped {
					wantTerminal, wantAcked = StreamStopped, 0
				}
				if snapshot.Terminal != wantTerminal || snapshot.Err != nil || snapshot.Stopped != stopped ||
					snapshot.LastSequence != 1 || snapshot.AckedThrough != wantAcked || snapshot.InFlight != 0 {
					t.Errorf("authoritative progress was not preserved: %+v", snapshot)
				}
				stream.state.mu.Lock()
				_, ghost := stream.state.readers[managedReaderID{exec: f.exec, node: "workers-1"}]
				readers := len(stream.state.readers)
				stream.state.mu.Unlock()
				if ghost || readers != 1 {
					t.Errorf("restored worker became a ghost reader: ghost=%t readers=%d", ghost, readers)
				}
				f.probe.mu.Lock()
				for _, commit := range f.probe.commits {
					wantReason := NodeCompleted
					if skipped {
						wantReason = NodeSkipped
					}
					if commit.Node.NodeID != "workers-2" || commit.Node.Reason != wantReason {
						t.Errorf("wrong restored reader confirmation: %+v", commit.Node)
					}
				}
				f.probe.mu.Unlock()

				// 继续使用同一持久预算：正常空尾/Stop 不能留下 FailAttempt，污染下次恢复。
				next, err := f.graph.NewExecution(f.ctx, Inputs{}, WithExecCheckpoint("root", f.hooks))
				managedTestOK(t, err)
				t.Cleanup(next.Cancel)
				managedTestOK(t, next.RestoreNodes(f.plan))
				managedTestOK(t, next.Start())
				if err := next.Wait(); err != nil {
					t.Errorf("next restore inherited a false failure: %v", err)
				}
				f.assertConfirmation(t, 2, 0)
			})
		}
	}
}

func TestCheckpointFanOutRestoreReadyStillRejectsUnreadTail(t *testing.T) {
	for _, skipped := range []bool{false, true} {
		t.Run(fmt.Sprint(skipped), func(t *testing.T) {
			f := newCheckpointReaderRestoreFixture(t, false, true, func(*RunCtx) error {
				if skipped {
					return NoValue()
				}
				return nil
			})
			f.awaitCloseout(t)
			f.unblockOpen()
			if err := f.exec.Wait(); err == nil || !strings.Contains(err.Error(), "reading obligations") {
				t.Fatalf("unread restored tail: want reading-obligation failure, got %v", err)
			}
			f.assertConfirmation(t, 0, 1)
			if snapshot := f.stream(t).Snapshot(); snapshot.Terminal != StreamCompleted ||
				snapshot.LastSequence != 1 || snapshot.AckedThrough != 0 || snapshot.InFlight != 1 || snapshot.Stopped {
				t.Fatalf("reader cleanup changed unacknowledged progress: %+v", snapshot)
			}
		})
	}
}

func TestCheckpointFanOutRestoreReadyFailureDoesNotWaitForOpen(t *testing.T) {
	cause := errors.New("reader aspect failed")
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			want := cause
			if canceled {
				want = context.Canceled
			}
			f := newCheckpointReaderRestoreFixture(t, false, true, func(rc *RunCtx) error {
				if canceled {
					rc.exec.Cancel()
					return rc.Context().Err()
				}
				return cause
			})
			// Open 在显式释放前保持阻塞；失败/取消收尾必须自己退出，不能等它。
			if err := waitCheckpointSignal(t, f.ctx, f.finished); !errors.Is(err, want) {
				t.Fatalf("reader lost its original failure: want %v, got %v", want, err)
			}
			wantFailures := 1
			if canceled {
				wantFailures = 0
			}
			f.assertConfirmation(t, 0, wantFailures)
			f.unblockOpen()
			if err := f.exec.Wait(); !errors.Is(err, want) {
				t.Fatalf("execution lost its original failure: want %v, got %v", want, err)
			}
		})
	}
}

func TestCheckpointFanOutRestoreReadyCancellationInterruptsWait(t *testing.T) {
	for _, nodeOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("node-only=%t", nodeOnly), func(t *testing.T) {
			cancelNode := make(chan context.CancelFunc, 1)
			f := newCheckpointReaderRestoreFixture(t, false, false, func(rc *RunCtx) error {
				cancelNode <- rc.Cancel
				return nil
			})
			f.awaitCloseout(t)
			if err := f.exec.ctx.Err(); err != nil {
				t.Fatalf("execution was already canceled before the probe: %v", err)
			}
			wantFailures := 0
			if nodeOnly {
				// 只取消本层，Open 所用的根执行 ctx 此刻仍健康。
				waitCheckpointSignal(t, f.ctx, cancelNode)()
				wantFailures = 1 // 本层终止是真实节点失败，不冒充整次执行的外部中断。
			} else {
				f.exec.Cancel()
			}
			// 同步 Open 仍未返回；并发等待者必须独立响应自己的 ctx 取消。
			if err := waitCheckpointSignal(t, f.ctx, f.finished); !errors.Is(err, context.Canceled) {
				t.Errorf("canceled reader closeout: %v", err)
			}
			f.assertConfirmation(t, 0, wantFailures)
			f.unblockOpen()
			if err := f.exec.Wait(); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled execution: %v", err)
			}
		})
	}
}

func TestCheckpointFanOutRestoreReadyOpenFailureRemainsFailure(t *testing.T) {
	for _, skipped := range []bool{false, true} {
		t.Run(fmt.Sprint(skipped), func(t *testing.T) {
			f := newCheckpointReaderRestoreFixture(t, false, false, func(*RunCtx) error {
				if skipped {
					return NoValue()
				}
				return nil
			})
			f.awaitCloseout(t)
			cause := errors.New("authoritative Open failed")
			f.openErr = cause // 屏障释放之前写入，Open 返回之前读取。
			f.unblockOpen()
			if err := f.exec.Wait(); !isCheckpointFailure(err) || !errors.Is(err, cause) {
				t.Fatalf("Open failure was replaced or downgraded: %v", err)
			}
			f.assertConfirmation(t, 0, 0)
		})
	}
}

type checkpointReaderRestoreFixture struct {
	ctx      context.Context
	graph    *Graph
	exec     *Execution
	key      Key[Stream[int]]
	plan     []NodeCheckpoint
	hooks    CheckpointHooks
	probe    *checkpointProbe
	waiting  chan struct{}
	finished chan error
	release  chan struct{}
	released sync.Once
	openErr  error
	messages atomic.Int32
}

func newCheckpointReaderRestoreFixture(t *testing.T, stopped, pending bool, aspect func(*RunCtx) error) *checkpointReaderRestoreFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	f := &checkpointReaderRestoreFixture{
		ctx: ctx, key: NewKey[Stream[int]]("stream"), probe: &checkpointProbe{},
		waiting: make(chan struct{}), finished: make(chan error, 2), release: make(chan struct{}),
	}
	t.Cleanup(f.unblockOpen)
	g, err := New("restored-reader-ready", "", WithMaxRunning(1), WithObserver(ObserverFunc{
		Finished: func(_ Site, id string, _ NodeFinishReason, err error) {
			if id == "workers-2" {
				f.finished <- err
			}
		},
	}))
	managedTestOK(t, err)
	f.graph = g
	managedTestOK(t, g.Add(NewNode("source", "", nil, Provides(f.key), func(*RunCtx) error {
		return errors.New("restored source body ran")
	})))
	one, two := NewKey[int]("one"), NewKey[int]("two")
	opened := make(chan struct{})
	managedTestOK(t, FanOut(g, "workers", "", f.key, Keys(one, two),
		func(*RunCtx, int, Stream[int]) (int, error) { return 0, errors.New("short-circuited worker body ran") },
		func(rc *RunCtx, _ func(*RunCtx) error) error {
			if err := readerLifecycleWait(rc, opened); err != nil {
				return err
			}
			if rc.exec == f.exec {
				rc.ctx = &managedWaitWitness{Context: rc.ctx, entered: f.waiting}
			}
			return aspect(rc)
		}))
	descriptor := CheckpointStreamDescriptor{
		ID:    CheckpointStreamID{Scope: "root", NodeID: "source", Key: f.key.Name(), Group: "default", Generation: 1},
		Graph: g.id, Attempt: "confirmed-source", Element: reflect.TypeFor[int](),
		Source: CheckpointStreamSource{ID: "finite"}, Capacity: 1, Terminal: StreamCompleted,
	}
	if stopped {
		descriptor.Terminal, descriptor.Stopped = StreamStopped, true
	}
	record := CheckpointStreamRecord{Stream: descriptor, Resumed: true, LastSequence: 1, AckedThrough: 1}
	if stopped || pending {
		record.AckedThrough = 0
	}
	if pending {
		record.Pending = []CheckpointStreamMessage{{Sequence: 1, Value: 7}}
	}
	f.plan = []NodeCheckpoint{
		{NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(f.key.asRef()), State: SlotStateReady, Stream: &descriptor}}},
		{NodeID: "workers-1", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(one.asRef()), State: SlotStateReady, Value: 10}}},
	}
	f.hooks = checkpointStreamTestHooks(f.probe)
	var openOnce sync.Once
	f.hooks.StreamHooks.Open = func(_ context.Context, _ CheckpointStreamOpen) (CheckpointStreamRecord, error) {
		openOnce.Do(func() { close(opened) })
		// 测试控制同步钩子的返回；取消测试会在释放它之前检查 worker 已经退出。
		select {
		case <-f.release:
		case <-t.Context().Done():
			return CheckpointStreamRecord{}, t.Context().Err()
		}
		return record, f.openErr
	}
	f.hooks.StreamHooks.Commit = func(context.Context, CheckpointMessageCommit) error {
		f.messages.Add(1)
		return nil
	}
	f.exec, err = g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", f.hooks))
	managedTestOK(t, err)
	t.Cleanup(f.exec.Cancel)
	managedTestOK(t, f.exec.RestoreNodes(f.plan))
	managedTestOK(t, f.exec.Start())
	return f
}

func (f *checkpointReaderRestoreFixture) unblockOpen() {
	f.released.Do(func() { close(f.release) })
}

func (f *checkpointReaderRestoreFixture) awaitCloseout(t *testing.T) {
	t.Helper()
	select {
	case <-f.waiting:
	case err := <-f.finished:
		t.Errorf("reader finished before authoritative Open returned: %v", err)
		f.finished <- err // 取消对照还要核对该结论，不能吃掉完成事件。
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	stream := f.stream(t)
	if snapshot := stream.Snapshot(); snapshot.Terminal != "" || snapshot.LastSequence != 0 {
		t.Fatalf("Open barrier did not precede state installation: %+v", snapshot)
	}
	// 权威就绪等待不能持 operation 门；安装/并发流操作还需要进入它。
	if err := stream.state.operation.lock(&RunCtx{ctx: f.ctx}); err != nil {
		t.Fatalf("reader held operation while waiting for Open: %v", err)
	}
	stream.state.operation.unlock()
}

func (f *checkpointReaderRestoreFixture) stream(t *testing.T) Stream[int] {
	t.Helper()
	_, raw := f.exec.slotOf(f.key.asRef()).snapshot()
	stream, ok := raw.(Stream[int])
	if !ok || stream.state == nil {
		t.Fatal("restored stream endpoint was not visible")
	}
	return stream
}

func (f *checkpointReaderRestoreFixture) assertConfirmation(t *testing.T, commits, failures int) {
	t.Helper()
	f.probe.mu.Lock()
	defer f.probe.mu.Unlock()
	if len(f.probe.commits) != commits || len(f.probe.failures) != failures || f.messages.Load() != 0 {
		t.Errorf("confirmation: node commits=%d (want %d), FailAttempt=%d (want %d), message Ack/Stop commits=%d (want 0)",
			len(f.probe.commits), commits, len(f.probe.failures), failures, f.messages.Load())
	}
}
