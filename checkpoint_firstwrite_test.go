package pulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

// 只模拟桥接载荷：每个普通键只接收一次消息贡献，和 Durable 的首写契约一致。
// 不以此声称跨进程持久化；真实退出/重启由集成层验收。
type checkpointFirstWriteStore struct {
	mu         sync.Mutex
	values     []CheckpointOutput
	messages   []CheckpointMessageCommit
	acked      uint64
	source     NodeCheckpoint
	descriptor CheckpointStreamDescriptor
}

func (s *checkpointFirstWriteStore) hooks() CheckpointHooks {
	hooks := checkpointStreamTestHooks(&checkpointProbe{})
	openAttempt := hooks.OpenAttempt
	hooks.OpenAttempt = func(ctx context.Context, request CheckpointAttemptRequest) (CheckpointAttempt, error) {
		attempt, err := openAttempt(ctx, request)
		if request.NodeID == "processor" && request.Boundary == "" {
			s.mu.Lock()
			attempt.Values = append([]CheckpointOutput(nil), s.values...)
			s.mu.Unlock()
		}
		return attempt, err
	}
	openStream := hooks.StreamHooks.Open
	hooks.StreamHooks.Open = func(ctx context.Context, request CheckpointStreamOpen) (CheckpointStreamRecord, error) {
		if request.Restore == nil {
			return openStream(ctx, request)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		descriptor := s.descriptor
		descriptor.ID.Generation++
		record := CheckpointStreamRecord{Stream: descriptor, LastSequence: 2, AckedThrough: s.acked, Resumed: true}
		for sequence := s.acked + 1; sequence <= 2; sequence++ {
			record.Pending = append(record.Pending, CheckpointStreamMessage{Sequence: sequence, Value: int(sequence)})
		}
		return record, nil
	}
	commitNode := hooks.Commit
	hooks.Commit = func(ctx context.Context, commit CheckpointCommit) error {
		if commit.Node.NodeID == "source" {
			s.mu.Lock()
			s.source = copyCheckpoints([]NodeCheckpoint{commit.Node})[0]
			s.descriptor = *commit.Node.Outputs[0].Stream
			s.mu.Unlock()
		}
		return commitNode(ctx, commit)
	}
	hooks.StreamHooks.Commit = func(_ context.Context, commit CheckpointMessageCommit) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, value := range commit.Values {
			for _, previous := range s.values {
				if previous.Key.Name == value.Key.Name {
					return fmt.Errorf("ordinary key %q was submitted twice", value.Key.Name)
				}
			}
		}
		s.values = append(s.values, commit.Values...)
		s.messages = append(s.messages, commit)
		for _, input := range commit.Inputs {
			s.acked = input.Sequence
		}
		return nil
	}
	return hooks
}

func TestCheckpointMessageValuesKeepFirstWriteAcrossResume(t *testing.T) {
	for _, prewrite := range []bool{false, true} {
		for _, interrupted := range []bool{false, true} {
			t.Run(fmt.Sprintf("outer-first-%t/interrupted-%t", prewrite, interrupted), func(t *testing.T) {
				ctx := checkpointTestContext(t)
				g := mustNew(t, ctx, "message-first-write")
				input, result := NewKey[Stream[int]]("input"), NewKey[int]("result")
				later := NewKey[int]("later")
				mustAdd(t, g, NewNode("source", "", nil, Provides(input), func(rc *RunCtx) error {
					writer, err := OpenStream(rc, input, WithBuffer(2), WithStreamSource("two", true))
					if err != nil {
						return err
					}
					if err := writer.Send(1); err != nil {
						return err
					}
					return writer.Send(2)
				}))
				want := 22
				if prewrite {
					want = 11
				}
				processed := make(chan struct{}, 1)
				var interrupt atomic.Bool
				interrupt.Store(interrupted)
				mustAdd(t, g, NewNode("processor", "", Requires(input), Provides(result, later), func(rc *RunCtx) error {
					if prewrite {
						if err := Set(rc, result, 11); err != nil {
							return err
						}
					}
					stream, err := Get(rc, input)
					if err != nil {
						return err
					}
					// 来源完整 Commit 已落地，恢复只需重建其可靠尾部。
					select {
					case <-stream.Done():
					case <-rc.Context().Done():
						return rc.Context().Err()
					}
					for {
						delivery, err := stream.Receive(rc)
						if err == io.EOF {
							return nil
						}
						if err != nil {
							return err
						}
						if err := HandleMessage(rc, delivery, func(rc *RunCtx, value int) error {
							if err := Set(rc, result, 20+2*value); err != nil {
								return err
							}
							if value == 2 {
								return Set(rc, later, 99)
							}
							return nil
						}); err != nil {
							return err
						}
						if value, err := Get(rc, result); err != nil || value != want {
							return fmt.Errorf("effective first write = %v, %v; want %d", value, err, want)
						}
						if state, _ := rc.exec.slotOf(result.asRef()).snapshot(); state != slotPending {
							return fmt.Errorf("ordinary message output published before complete-node Commit")
						}
						if interrupt.Load() {
							processed <- struct{}{}
							<-rc.Context().Done()
							return rc.Context().Err()
						}
					}
				}))
				declareOut(t, g, NewPort(result))
				store := &checkpointFirstWriteStore{}
				e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", store.hooks()))
				if err != nil {
					t.Fatal(err)
				}
				if err := e.Start(); err != nil {
					t.Fatal(err)
				}
				if interrupted {
					select {
					case <-processed:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					e.Cancel()
					if err := e.Wait(); !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
					store.mu.Lock()
					values := append([]CheckpointOutput(nil), store.values...)
					source := store.source
					store.mu.Unlock()
					if len(values) != 1 || values[0].Value != want {
						t.Fatalf("confirmed message saved a value that never won the local merge: %+v; want %d", values, want)
					}
					interrupt.Store(false)
					e, err = g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", store.hooks()))
					if err != nil {
						t.Fatal(err)
					}
					if err := e.RestoreNodes([]NodeCheckpoint{source}); err != nil {
						t.Fatal(err)
					}
					if err := e.Start(); err != nil {
						t.Fatal(err)
					}
				}
				if err := e.Wait(); err != nil {
					t.Fatal(err)
				}
				if value, state, err := ReadOutput(e, result); value != want || state != SlotStateReady || err != nil {
					t.Fatalf("final first write = %v, %s, %v; want %d", value, state, err, want)
				}
				if len(store.values) != 2 || store.values[0].Value != want || store.values[1].Key.Name != later.Name() || store.values[1].Value != 99 ||
					len(store.messages) != 2 || len(store.messages[0].Values) != 1 || len(store.messages[1].Values) != 1 || store.messages[1].Values[0].Key.Name != later.Name() || store.acked != 2 {
					t.Fatalf("ordinary key was lost or re-submitted: values=%+v messages=%+v acked=%d", store.values, store.messages, store.acked)
				}
			})
		}
	}
}

func TestCheckpointMessageValuesKeepSkipAndFailureBoundaries(t *testing.T) {
	for _, scenario := range []string{"skip-first", "ready-skip-conflict", "skip-ready-conflict", "failed-attempt", "exhausted", "message-commit-fails", "node-commit-fails"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, scenario)
			input, result := NewKey[Stream[int]]("input"), NewKey[int]("result")
			mustAdd(t, g, NewNode("source", "", nil, Provides(input), func(rc *RunCtx) error {
				writer, err := OpenStream(rc, input, WithBuffer(2), WithStreamSource("two", true))
				if err != nil {
					return err
				}
				if err := writer.Send(1); err != nil {
					return err
				}
				return writer.Send(2)
			}))
			cause := errors.New("unconfirmed or failed work")
			mustAdd(t, g, NewNode("processor", "", Requires(input), Provides(result), func(rc *RunCtx) error {
				if scenario == "ready-skip-conflict" {
					if err := Set(rc, result, 11); err != nil {
						return err
					}
				}
				if scenario == "skip-ready-conflict" || scenario == "skip-first" {
					if err := Skip(rc, result); err != nil {
						return err
					}
				}
				stream, err := Get(rc, input)
				if err != nil {
					return err
				}
				for {
					delivery, err := stream.Receive(rc)
					if err == io.EOF {
						return nil
					}
					if err != nil {
						return err
					}
					tries := 0
					if err := HandleMessage(rc, delivery, func(rc *RunCtx, _ int) error {
						tries++
						if scenario == "ready-skip-conflict" || scenario == "skip-first" {
							return Skip(rc, result)
						}
						if err := Set(rc, result, 20+tries); err != nil {
							return err
						}
						if scenario == "exhausted" || (scenario == "failed-attempt" && tries == 1) {
							return cause
						}
						return nil
					}, WithMessageRetry(2, 0)); err != nil {
						return err
					}
				}
			}))
			store := &checkpointFirstWriteStore{}
			hooks := store.hooks()
			// 该探针为每个失败后的消息尝试分配新游标，与真实预算分开验证。
			var opens atomic.Int32
			hooks.StreamHooks.OpenMessage = func(_ context.Context, request CheckpointMessageAttemptRequest) (CheckpointMessageAttempt, error) {
				index := int(opens.Add(1)-1)%2 + 1
				if scenario != "failed-attempt" && scenario != "exhausted" {
					index = 1
				}
				return CheckpointMessageAttempt{Attempt: CheckpointAttempt{Scope: fmt.Sprintf("message-%d", opens.Load()), Index: index}}, nil
			}
			if scenario == "message-commit-fails" {
				hooks.StreamHooks.Commit = func(context.Context, CheckpointMessageCommit) error { return cause }
			}
			commit := hooks.Commit
			hooks.Commit = func(ctx context.Context, result CheckpointCommit) error {
				if scenario == "node-commit-fails" && result.Node.NodeID == "processor" {
					return cause
				}
				return commit(ctx, result)
			}
			e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			err = e.Wait()
			state, value := e.slotOf(result.asRef()).snapshot()
			switch scenario {
			case "skip-first":
				if err != nil || state != slotSkipped || len(store.values) != 1 || store.values[0].State != SlotStateSkipped || len(store.messages) != 2 || len(store.messages[1].Values) != 0 {
					t.Fatalf("skip first-write changed: err=%v state=%v values=%+v messages=%+v", err, state, store.values, store.messages)
				}
			case "failed-attempt":
				if err != nil || state != slotReady || value != 22 || len(store.values) != 1 || store.values[0].Value != 22 || store.acked != 2 {
					t.Fatalf("failed message attempt leaked: err=%v state=%v value=%v stored=%+v", err, state, value, store.values)
				}
			default:
				if err == nil || state != slotPending {
					t.Fatalf("failed complete chain published ordinary value: err=%v state=%v value=%v", err, state, value)
				}
				if scenario != "node-commit-fails" && (len(store.values) != 0 || store.acked != 0) {
					t.Fatalf("failed/conflicting attempt saved or Acked: values=%+v acked=%d", store.values, store.acked)
				}
				if (scenario == "ready-skip-conflict" || scenario == "skip-ready-conflict") && !errors.Is(err, ErrConflict) {
					t.Fatalf("skip conflict lost its cause: %v", err)
				}
				if (scenario == "exhausted" || scenario == "message-commit-fails" || scenario == "node-commit-fails") && !errors.Is(err, cause) {
					t.Fatalf("failed work lost its actual cause: %v", err)
				}
			}
		})
	}
}
