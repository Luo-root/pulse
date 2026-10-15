package pulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
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
