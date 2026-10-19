package pulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 第一轮真实执行钩子，取得 source 和提前退出 worker 的确认记录，再取消已 Receive
// 但未 Ack 的 worker。这里只验证同进程 Core 恢复契约，不冒充持久后端或进程恢复。
func TestCheckpointFanOutInterruptedResumeObligations(t *testing.T) {
	for _, workers := range []int{2, 3} {
		for _, earlySkip := range []bool{false, true} {
			for _, action := range []string{"unacked", "unacked-skip", "unread", "unread-skip", "ack", "stop"} {
				t.Run(fmt.Sprintf("workers=%d/early-skip=%t/%s", workers, earlySkip, action), func(t *testing.T) {
					ctx := checkpointTestContext(t)
					g, err := New("interrupted-fanout", "", WithMaxRunning(1))
					managedTestOK(t, err)
					key := NewKey[Stream[int]]("stream")
					outs := make([]Key[int], workers)
					for i := range outs {
						outs[i] = NewKey[int](fmt.Sprintf("result-%d", i+1))
					}
					firstCommitted, received, handedOff := make(chan struct{}), make(chan struct{}), make(chan struct{})
					resumed := false // 只在第一轮 Wait 汇合所有节点后修改。
					managedTestOK(t, ProduceStream(g, "source", "", key, func(_ *RunCtx, writer *StreamWriter[int]) error {
						if resumed {
							return errors.New("restored source ran")
						}
						return writer.Send(7)
					}, WithStreamSource("finite", true)))
					managedTestOK(t, FanOut(g, "workers", "", key, outs, func(rc *RunCtx, shard int, stream Stream[int]) (int, error) {
						if shard == 1 {
							if resumed {
								return 0, errors.New("restored worker ran")
							}
							if earlySkip {
								return 0, NoValue()
							}
							return 10, nil
						}
						signal := firstCommitted
						if shard == 3 {
							signal = handedOff
						}
						if err := readerLifecycleWait(rc, signal); err != nil {
							return 0, err
						}
						if err := readerLifecycleWait(rc, stream.Done()); err != nil {
							return 0, err
						}
						if resumed && shard == workers && strings.HasPrefix(action, "unread") {
							if action == "unread-skip" {
								return 0, NoValue()
							}
							return 0, nil
						}
						delivery, err := stream.Receive(rc)
						if err != nil {
							return 0, err
						}
						if !resumed {
							close(received)
							<-rc.Context().Done()
							return 0, rc.Context().Err()
						}
						if shard < workers {
							// 恢复后仍有真实 peer：先退出者必须把交付移交给尚未读取的它。
							if earlySkip {
								return 0, NoValue()
							}
							return delivery.Value, nil
						}
						switch action {
						case "unacked-skip":
							return 0, NoValue()
						case "ack":
							if err := delivery.Ack(); err != nil {
								return 0, err
							}
							if _, err := stream.Receive(rc); err != io.EOF {
								return 0, fmt.Errorf("after Ack: want EOF, got %v", err)
							}
						case "stop":
							if err := stream.Stop(rc); err != nil {
								return 0, err
							}
						}
						return delivery.Value, nil
					}))
					probe := &checkpointProbe{}
					hooks := checkpointStreamTestHooks(probe)
					baseCommit, baseOpen := hooks.Commit, hooks.StreamHooks.Open
					var appended CheckpointStreamAppend
					var messageCommits []CheckpointMessageCommit
					hooks.StreamHooks.Append = func(_ context.Context, request CheckpointStreamAppend) error {
						appended = request
						return nil
					}
					hooks.StreamHooks.Commit = func(_ context.Context, request CheckpointMessageCommit) error {
						messageCommits = append(messageCommits, request)
						return nil
					}
					hooks.StreamHooks.Open = func(ctx context.Context, request CheckpointStreamOpen) (CheckpointStreamRecord, error) {
						if request.Restore == nil {
							return baseOpen(ctx, request)
						}
						if request.Restore.ID != appended.Stream {
							return CheckpointStreamRecord{}, errors.New("restore did not identify the actual appended stream")
						}
						return CheckpointStreamRecord{Stream: *request.Restore, Resumed: true,
							LastSequence: appended.Message.Sequence, Pending: []CheckpointStreamMessage{appended.Message}}, nil
					}
					hooks.Commit = func(ctx context.Context, request CheckpointCommit) error {
						if err := baseCommit(ctx, request); err != nil {
							return err
						}
						if request.Node.NodeID == "workers-1" {
							close(firstCommitted)
						}
						if resumed && request.Node.NodeID == "workers-2" && workers == 3 {
							close(handedOff)
						}
						return nil
					}
					first, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
					managedTestOK(t, err)
					t.Cleanup(first.Cancel)
					managedTestOK(t, first.Start())
					waitCheckpointSignal(t, ctx, received)
					first.Cancel()
					if err := first.Wait(); !errors.Is(err, context.Canceled) {
						t.Fatalf("first interruption=%v", err)
					}
					var plan []NodeCheckpoint
					for _, commit := range probe.commits {
						plan = append(plan, commit.Node)
					}
					if len(plan) != 2 || len(probe.failures) != 0 || len(messageCommits) != 0 {
						t.Fatalf("first interruption: records=%+v, failures=%v, message commits=%v", plan, probe.failures, messageCommits)
					}
					resumed = true
					second, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
					managedTestOK(t, err)
					t.Cleanup(second.Cancel)
					managedTestOK(t, second.RestoreNodes(plan))
					managedTestOK(t, second.Start())
					err = second.Wait()
					_, raw := second.slotOf(key.asRef()).snapshot()
					snapshot := raw.(Stream[int]).Snapshot()
					valid := action == "ack" || action == "stop"
					wantCommits := workers // source + restored worker + any active peer handing off.
					if valid {
						wantCommits++
						managedTestOK(t, err)
					} else if err == nil || !strings.Contains(err.Error(), "reading obligations") {
						t.Fatalf("unfulfilled reader succeeded: Wait=%v snapshot=%+v", err, snapshot)
					}
					finalNode := fmt.Sprintf("workers-%d", workers)
					finalCommits, failed := 0, false
					for _, commit := range probe.commits {
						if commit.Node.NodeID == finalNode {
							finalCommits++
						}
					}
					for request, attempt := range probe.latest {
						if request.NodeID == finalNode && probe.failed[attempt.Scope] {
							failed = true
						}
					}
					wantFinalCommits := 0
					if valid {
						wantFinalCommits = 1
					}
					if len(probe.commits) != wantCommits || finalCommits != wantFinalCommits || failed == valid {
						t.Fatalf("reader confirmation: commits=%d/%d final=%d failure=%t", len(probe.commits), wantCommits, finalCommits, failed)
					}
					wantAcked, wantPending := uint64(0), 1
					if valid {
						wantPending = 0
					}
					if action == "ack" {
						wantAcked = 1
					}
					if snapshot.Terminal != StreamCompleted || snapshot.LastSequence != 1 || snapshot.AckedThrough != wantAcked ||
						snapshot.InFlight != wantPending || snapshot.Stopped != (action == "stop") {
						t.Fatalf("wrong stream progress: %+v", snapshot)
					}
					if valid {
						wantInputs, wantStops := 1, 0
						if action == "stop" {
							wantInputs, wantStops = 0, 1
						}
						if len(messageCommits) != 1 || len(messageCommits[0].Inputs) != wantInputs || len(messageCommits[0].Stops) != wantStops {
							t.Fatalf("Stop/Ack were conflated: %+v", messageCommits)
						}
					} else if len(messageCommits) != 0 {
						t.Fatalf("reader cleanup confirmed messages: %+v", messageCommits)
					}
				})
			}
		}
	}
}

func TestCheckpointFanOutRestoredSkippedInput(t *testing.T) {
	ctx := checkpointTestContext(t)
	g, err := New("absent-fanout", "")
	managedTestOK(t, err)
	key := NewKey[Stream[int]]("stream")
	managedTestOK(t, g.Add(NewNode("source", "", nil, Provides(key), func(rc *RunCtx) error { return Skip(rc, key) })))
	managedTestOK(t, FanOut(g, "workers", "", key, Keys(NewKey[int]("one"), NewKey[int]("two")),
		func(*RunCtx, int, Stream[int]) (int, error) { return 0, errors.New("absent input reached worker body") }))
	probe := &checkpointProbe{}
	hooks := checkpointStreamTestHooks(probe)
	managedTestOK(t, g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks)))
	var plan []NodeCheckpoint
	for _, commit := range probe.commits {
		if commit.Node.NodeID != "workers-2" {
			plan = append(plan, commit.Node)
		}
	}
	second, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	managedTestOK(t, err)
	managedTestOK(t, second.RestoreNodes(plan))
	managedTestOK(t, second.Start())
	managedTestOK(t, second.Wait())
	if len(probe.commits) != 4 || probe.commits[3].Node.NodeID != "workers-2" || probe.commits[3].Node.Reason != NodeSkipped || len(probe.failures) != 0 {
		t.Fatalf("normal missing stream became failure: commits=%+v failures=%v", probe.commits, probe.failures)
	}
}

func TestCheckpointFanOutReaderRestoreIsMountLocal(t *testing.T) {
	ctx := checkpointTestContext(t)
	child, err := New("shared-workers", "")
	managedTestOK(t, err)
	key := NewKey[Stream[int]]("stream")
	one, two := NewKey[int]("one"), NewKey[int]("two")
	managedTestOK(t, ProduceStream(child, "source", "", key, func(_ *RunCtx, writer *StreamWriter[int]) error {
		return writer.Send(7)
	}, WithStreamSource("finite", true)))
	handedOff := make(chan struct{})
	managedTestOK(t, FanOut(child, "workers", "", key, Keys(one, two), func(rc *RunCtx, shard int, stream Stream[int]) (int, error) {
		if rc.exec.site.Path == "fresh" && shard == 2 {
			if err := readerLifecycleWait(rc, handedOff); err != nil {
				return 0, err
			}
		}
		if err := readerLifecycleWait(rc, stream.Done()); err != nil {
			return 0, err
		}
		delivery, err := stream.Receive(rc)
		if err != nil {
			return 0, err
		}
		if rc.exec.site.Path == "fresh" && shard == 1 {
			return 0, NoValue() // 该挂载仍有真实 peer，必须移交，不能套用另一挂载的恢复记录。
		}
		if err := delivery.Ack(); err != nil {
			return 0, err
		}
		return delivery.Value, nil
	}))
	root, err := New("mounted-fanout", "", WithMaxRunning(1))
	managedTestOK(t, err)
	managedTestOK(t, child.DeclareOutput(NewPort(two)))
	managedTestOK(t, Sub(root, "restored", "", []SubBind{Out(two, NewKey[int]("restored-result"))}, child))
	managedTestOK(t, Sub(root, "fresh", "", []SubBind{Out(two, NewKey[int]("fresh-result"))}, child))
	probe := &checkpointProbe{}
	hooks := checkpointStreamTestHooks(probe)
	baseOpen, baseCommit := hooks.StreamHooks.Open, hooks.Commit
	hooks.OpenChild = func(_ context.Context, request CheckpointChild) ([]NodeCheckpoint, error) {
		if request.NodeID != "restored" {
			return nil, nil
		}
		descriptor := CheckpointStreamDescriptor{
			ID:    CheckpointStreamID{Scope: request.Attempt, NodeID: "source", Key: key.Name(), Group: "default", Generation: 1},
			Graph: child.id, Path: request.Site.Path, Attempt: "confirmed-source", Element: reflect.TypeFor[int](),
			Source: CheckpointStreamSource{ID: "finite", Resumable: true}, Capacity: 1, Terminal: StreamCompleted,
		}
		return []NodeCheckpoint{
			{NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(key.asRef()), State: SlotStateReady, Stream: &descriptor}}},
			{NodeID: "workers-1", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: checkpointKey(one.asRef()), State: SlotStateReady, Value: 10}}},
		}, nil
	}
	hooks.StreamHooks.Open = func(ctx context.Context, request CheckpointStreamOpen) (CheckpointStreamRecord, error) {
		if request.Restore == nil {
			return baseOpen(ctx, request)
		}
		return CheckpointStreamRecord{Stream: *request.Restore, Resumed: true, LastSequence: 1,
			Pending: []CheckpointStreamMessage{{Sequence: 1, Value: 7}}}, nil
	}
	hooks.Commit = func(ctx context.Context, request CheckpointCommit) error {
		if err := baseCommit(ctx, request); err != nil {
			return err
		}
		if request.Site.Path == "fresh" && request.Node.NodeID == "workers-1" {
			close(handedOff)
		}
		return nil
	}
	managedTestOK(t, root.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks)))
	committed := make(map[string]int)
	for _, commit := range probe.commits {
		committed[commit.Site.Path+"/"+commit.Node.NodeID]++
	}
	if committed["restored/workers-1"] != 0 || committed["restored/workers-2"] != 1 ||
		committed["fresh/workers-1"] != 1 || committed["fresh/workers-2"] != 1 || len(probe.failures) != 0 {
		t.Fatalf("mounts shared reader completion: commits=%v failures=%v", committed, probe.failures)
	}
}

// 等待点由 Done 的求值证明，不靠 sleep 让来源“恰好晚到”。
// 同时钉住失败确认和成功 Commit：仅让最终 Wait 报错并不能防住坏检查点。
func TestCheckpointReaderObligationsBeforeCommit(t *testing.T) {
	for _, late := range []bool{false, true} {
		for _, skipped := range []bool{false, true} {
			t.Run(fmt.Sprintf("late=%t/skip=%t", late, skipped), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				key := NewKey[Stream[int]]("input")
				waiting, published, readerFailed := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var publishOnce, failOnce sync.Once
				g, err := New("reader-before-commit", "", WithMaxRunning(1))
				managedTestOK(t, err)
				managedTestOK(t, g.Add(NewNode("source", "", nil, Provides(key), func(rc *RunCtx) error {
					if late {
						if err := readerLifecycleWait(rc, waiting); err != nil {
							return err
						}
					}
					writer, err := OpenStream(rc, key, WithStreamSource("reader-before-commit", true))
					if err != nil {
						return err
					}
					sendErr := writer.Send(1)
					publishOnce.Do(func() { close(published) })
					// source 也会观察到读取义务错误；先等 sink 确认失败，避免它抢先取消整轮。
					// Send 已报错也走同一屏障；等待会归还额度，并保留取消/截止出口。
					if err := readerLifecycleWait(rc, readerFailed); err != nil {
						return err
					}
					return sendErr
				})))
				managedTestOK(t, g.Add(NewNode("sink", "", Requires(key), nil, func(*RunCtx) error {
					return errors.New("short-circuited reader body ran")
				}, func(rc *RunCtx, _ func(*RunCtx) error) error {
					if late {
						rc.ctx = &managedWaitWitness{Context: rc.ctx, entered: waiting}
					} else if err := readerLifecycleWait(rc, published); err != nil {
						return err
					}
					if skipped {
						return NoValue("reader aspect")
					}
					return nil
				})))
				probe := &checkpointProbe{}
				hooks := checkpointStreamTestHooks(probe)
				failAttempt := hooks.FailAttempt
				hooks.FailAttempt = func(ctx context.Context, scope string) error {
					if err := failAttempt(ctx, scope); err != nil {
						return err
					}
					probe.mu.Lock()
					defer probe.mu.Unlock()
					for request, attempt := range probe.latest {
						if request.NodeID == "sink" && attempt.Scope == scope {
							failOnce.Do(func() { close(readerFailed) })
							break
						}
					}
					return nil
				}
				err = g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
				if err == nil || !strings.Contains(err.Error(), "reading obligations") {
					t.Fatalf("Run=%v; want real reader-obligation failure", err)
				}
				probe.mu.Lock()
				committed := 0
				failed := false
				for _, commit := range probe.commits {
					if commit.Node.NodeID == "sink" {
						committed++
					}
				}
				for request, attempt := range probe.latest {
					if request.NodeID == "sink" && probe.failed[attempt.Scope] {
						failed = true
					}
				}
				probe.mu.Unlock()
				if committed != 0 || !failed {
					t.Fatalf("failed reader persisted: successful commits=%d, failure confirmed=%t", committed, failed)
				}
				// 同一持久尝试不能在下一轮变成已完成成果，也不能补回失败预算。
				err = g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
				if err == nil || !strings.Contains(err.Error(), "already failed") {
					t.Fatalf("failed reader attempt was reusable as success: %v", err)
				}
			})
		}
	}
}

func TestCheckpointReaderPendingAbsenceRemainsSuccessful(t *testing.T) {
	for _, shortSkip := range []bool{false, true} {
		t.Run(fmt.Sprint(shortSkip), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			key := NewKey[Stream[int]]("absent")
			waiting := make(chan struct{})
			g, err := New("reader-absence", "", WithMaxRunning(1))
			managedTestOK(t, err)
			managedTestOK(t, g.Add(NewNode("source", "", nil, Provides(key), func(rc *RunCtx) error {
				if err := readerLifecycleWait(rc, waiting); err != nil {
					return err
				}
				return Skip(rc, key)
			})))
			managedTestOK(t, g.Add(NewNode("sink", "", Requires(key), nil, nil,
				func(rc *RunCtx, _ func(*RunCtx) error) error {
					rc.ctx = &managedWaitWitness{Context: rc.ctx, entered: waiting}
					if shortSkip {
						return NoValue()
					}
					return nil
				})))
			probe := &checkpointProbe{}
			managedTestOK(t, g.Run(ctx, Inputs{}, WithExecCheckpoint("root", checkpointStreamTestHooks(probe))))
			probe.mu.Lock()
			defer probe.mu.Unlock()
			found := false
			for _, commit := range probe.commits {
				if commit.Node.NodeID == "sink" {
					found = true
					reason := NodeCompleted
					if shortSkip {
						reason = NodeSkipped
					}
					if commit.Node.Reason != reason {
						t.Fatalf("normal absence reason=%s, want %s", commit.Node.Reason, reason)
					}
				}
			}
			if !found {
				t.Fatal("normal absent reader was not confirmed")
			}
		})
	}
}

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
			if nodeOnly {
				// 业务已返回 nil，读取收尾被本层取消打断；没有形成已知业务失败，
				// 即使 Open 使用的根执行 ctx 仍健康，也保留 active 而不消耗预算。
				waitCheckpointSignal(t, f.ctx, cancelNode)()
			} else {
				f.exec.Cancel()
			}
			// 同步 Open 仍未返回；并发等待者必须独立响应自己的 ctx 取消。
			if err := waitCheckpointSignal(t, f.ctx, f.finished); !errors.Is(err, context.Canceled) {
				t.Errorf("canceled reader closeout: %v", err)
			}
			f.assertConfirmation(t, 0, 0)
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
