package pulse

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

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
