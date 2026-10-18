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

func TestCheckpointRestoredMessageValuesStayStaged(t *testing.T) {
	for _, failCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "commit-fails"}[failCommit], func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "restored-message-value")
			value := NewKey[int]("value")
			absent := NewKey[int]("absent")
			var readers atomic.Int32
			mustAdd(t, g, NewNode("processor", "", nil, Provides(value, absent), func(rc *RunCtx) error {
				got, err := Get(rc, value)
				if err != nil || got != 42 {
					return errors.New("confirmed message value was not restored before body")
				}
				if _, err := Get(rc, absent); !isSkipped(err) {
					return errors.New("confirmed missing value was not restored")
				}
				if state, _ := rc.exec.slotOf(value.asRef()).snapshot(); state != slotPending {
					return errors.New("message value published before complete-chain commit")
				}
				return nil
			}))
			mustAdd(t, g, NewNode("reader", "", Requires(value), nil, func(rc *RunCtx) error {
				readers.Add(1)
				return nil
			}))
			probe := &checkpointProbe{}
			hooks := probe.hooks()
			open := hooks.OpenAttempt
			hooks.OpenAttempt = func(ctx context.Context, request CheckpointAttemptRequest) (CheckpointAttempt, error) {
				attempt, err := open(ctx, request)
				if request.NodeID == "processor" {
					attempt.Values = []CheckpointOutput{
						{Key: checkpointKey(value.asRef()), State: SlotStateReady, Value: 42},
						{Key: checkpointKey(absent.asRef()), State: SlotStateSkipped},
					}
				}
				return attempt, err
			}
			cause := errors.New("store did not confirm full node")
			commit := hooks.Commit
			hooks.Commit = func(ctx context.Context, result CheckpointCommit) error {
				if failCommit && result.Node.NodeID == "processor" {
					return cause
				}
				return commit(ctx, result)
			}
			err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if failCommit {
				if !errors.Is(err, cause) || readers.Load() != 0 {
					t.Fatalf("unconfirmed value became visible: readers=%d err=%v", readers.Load(), err)
				}
			} else if err != nil || readers.Load() != 1 {
				t.Fatalf("restored value was lost: readers=%d err=%v", readers.Load(), err)
			}
		})
	}
}

func TestCheckpointAttemptRejectsInvalidMessageValueSubset(t *testing.T) {
	key := CheckpointKey{Name: "value", Type: reflect.TypeFor[int]()}
	valid := CheckpointOutput{Key: key, State: SlotStateReady, Value: 42}
	for _, test := range []struct {
		name   string
		values []CheckpointOutput
	}{
		{"duplicate", []CheckpointOutput{valid, valid}},
		{"undeclared", []CheckpointOutput{{Key: CheckpointKey{Name: "other", Type: key.Type}, State: SlotStateReady, Value: 42}}},
		{"mistyped", []CheckpointOutput{{Key: key, State: SlotStateReady, Value: "wrong"}}},
		{"pending", []CheckpointOutput{{Key: key, State: SlotStatePending}}},
		{"skipped-with-value", []CheckpointOutput{{Key: key, State: SlotStateSkipped, Value: 42}}},
		{"stream-descriptor", []CheckpointOutput{{Key: key, State: SlotStateReady, Value: 42, Stream: &CheckpointStreamDescriptor{}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, test.name)
			value := NewKey[int]("value")
			var called atomic.Int32
			mustAdd(t, g, NewNode("processor", "", nil, Provides(value), func(*RunCtx) error {
				called.Add(1)
				return nil
			}))
			hooks := (&checkpointProbe{}).hooks()
			hooks.OpenAttempt = func(context.Context, CheckpointAttemptRequest) (CheckpointAttempt, error) {
				return CheckpointAttempt{Scope: "attempt", Index: 1, Values: test.values}, nil
			}
			err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if err == nil || !isCheckpointFailure(err) || called.Load() != 0 {
				t.Fatalf("invalid subset reached body: calls=%d err=%v", called.Load(), err)
			}
		})
	}
}

func TestCheckpointNamedValues(t *testing.T) {
	pointed := 42
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"slice", func(t *testing.T) { checkpointNamedValue(t, []string{"x"}, inputNames{"x"}) }},
		{"map", func(t *testing.T) { checkpointNamedValue(t, map[string]int{"x": 1}, inputLookup{"x": 1}) }},
		{"array", func(t *testing.T) { checkpointNamedValue(t, [1]string{"x"}, inputArray{"x"}) }},
		{"struct", func(t *testing.T) { checkpointNamedValue(t, struct{ N int }{1}, inputRecord{1}) }},
		{"pointer", func(t *testing.T) { checkpointNamedValue(t, &pointed, inputPointer(&pointed)) }},
		{"nil-slice", func(t *testing.T) { checkpointNamedValue(t, []string(nil), inputNames(nil)) }},
		{"nil-map", func(t *testing.T) { checkpointNamedValue(t, map[string]int(nil), inputLookup(nil)) }},
		{"nil-pointer", func(t *testing.T) { checkpointNamedValue(t, (*int)(nil), inputPointer(nil)) }},
		{"nil-interface", func(t *testing.T) { checkpointNamedValue[any](t, nil, nil) }},
		{"interface-dynamic-type", func(t *testing.T) { checkpointNamedValue[any](t, []string{"x"}, []string{"x"}) }},
		{"interface-typed-nil", func(t *testing.T) { checkpointNamedValue[any](t, (*int)(nil), (*int)(nil)) }},
	} {
		t.Run(test.name, test.run)
	}
}

func checkpointNamedValue[T any](t *testing.T, raw any, want T) {
	t.Helper()
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint("active=", active), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "named-restore")
			key := NewKey[T]("value")
			declareOut(t, g, NewPort(key))
			mustAdd(t, g, NewNode("source", "", nil, Provides(key), func(rc *RunCtx) error {
				if !active {
					return errors.New("completed node body reran")
				}
				got, err := Get(rc, key)
				if err != nil || !reflect.DeepEqual(got, want) {
					return fmt.Errorf("active restored Get = (%#v, %v), want %#v", got, err, want)
				}
				if st, _ := rc.exec.slotOf(key.asRef()).snapshot(); st != slotPending {
					return errors.New("active restored value published before Commit")
				}
				return nil
			}))
			mustAdd(t, g, NewNode("reader", "", Requires(key), nil, func(rc *RunCtx) error {
				got, err := Get(rc, key)
				if err != nil || !reflect.DeepEqual(got, want) {
					return fmt.Errorf("restored Get = (%#v, %v), want %#v", got, err, want)
				}
				return nil
			}))
			hooks := (&checkpointProbe{}).hooks()
			output := CheckpointOutput{Key: checkpointKey(key.asRef()), State: SlotStateReady, Value: raw}
			if active {
				open := hooks.OpenAttempt
				hooks.OpenAttempt = func(ctx context.Context, request CheckpointAttemptRequest) (CheckpointAttempt, error) {
					attempt, err := open(ctx, request)
					if request.NodeID == "source" {
						attempt.Values = []CheckpointOutput{output}
					}
					return attempt, err
				}
			}
			e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if err != nil {
				t.Fatal(err)
			}
			if !active {
				if err := e.RestoreNodes([]NodeCheckpoint{{NodeID: "source", Reason: NodeCompleted, Outputs: []CheckpointOutput{output}}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			if err := e.Wait(); err != nil {
				t.Fatal(err)
			}
			got, state, err := ReadOutput(e, key)
			if err != nil || state != SlotStateReady || !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadOutput = (%#v, %s, %v), want %#v ready", got, state, err, want)
			}
			// 接口必须保留原动态类型；引用类型必须继续指向同一业务对象。
			rawValue, gotValue := reflect.ValueOf(raw), reflect.ValueOf(got)
			if rawValue.IsValid() && (rawValue.Kind() == reflect.Slice || rawValue.Kind() == reflect.Pointer) && rawValue.Pointer() != gotValue.Pointer() {
				t.Fatal("restore deep-copied a shared business reference")
			}
			if rawValue.IsValid() && rawValue.Kind() == reflect.Map && rawValue.UnsafePointer() != gotValue.UnsafePointer() {
				t.Fatal("restore deep-copied a shared business map")
			}
		})
	}
}

type checkpointNamedText string

func (v checkpointNamedText) String() string { return string(v) }

func TestCheckpointNamedManagedValues(t *testing.T) {
	pointed := 42
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"slice", func(t *testing.T) { checkpointNamedManagedValue(t, []string{"x"}, inputNames{"x"}, false) }},
		{"map", func(t *testing.T) { checkpointNamedManagedValue(t, map[string]int{"x": 1}, inputLookup{"x": 1}, false) }},
		{"array", func(t *testing.T) { checkpointNamedManagedValue(t, [1]string{"x"}, inputArray{"x"}, false) }},
		{"pointer", func(t *testing.T) { checkpointNamedManagedValue(t, &pointed, inputPointer(&pointed), false) }},
		{"typed-nil", func(t *testing.T) { checkpointNamedManagedValue(t, (*int)(nil), inputPointer(nil), false) }},
		{"nil-slice", func(t *testing.T) { checkpointNamedManagedValue(t, []string(nil), inputNames(nil), false) }},
		{"nil-map", func(t *testing.T) { checkpointNamedManagedValue(t, map[string]int(nil), inputLookup(nil), false) }},
		{"nonempty-interface", func(t *testing.T) {
			checkpointNamedManagedValue[fmt.Stringer](t, checkpointNamedText("x"), checkpointNamedText("x"), false)
		}},
		{"nil-nonempty-interface", func(t *testing.T) { checkpointNamedManagedValue[fmt.Stringer](t, nil, nil, false) }},
		{"nil-interface", func(t *testing.T) { checkpointNamedManagedValue[any](t, nil, nil, false) }},
		{"interface-dynamic-type", func(t *testing.T) { checkpointNamedManagedValue[any](t, []string{"x"}, []string{"x"}, false) }},
		{"interface-typed-nil", func(t *testing.T) { checkpointNamedManagedValue[any](t, (*int)(nil), (*int)(nil), false) }},
	} {
		t.Run(test.name, test.run)
	}
}

func TestCheckpointNamedManagedRejectsCoercion(t *testing.T) {
	checkpointNamedManagedValue(t, float64(1), 1, true)
}

// payload 覆盖 Open -> 实际安装 -> Receive/Ack/EOF；state 覆盖完整
// OpenMessage -> HandleMessage -> GetMessageState -> Commit，不能只测校验器。
func checkpointNamedManagedValue[T any](t *testing.T, raw any, want T, reject bool) {
	t.Helper()
	for _, state := range []bool{false, true} {
		t.Run(map[bool]string{false: "payload", true: "state"}[state], func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "named-managed")
			key := NewKey[Stream[T]]("input")
			panics := make(chan any, 1)
			mustAdd(t, g, NewNode("source", "", nil, Provides(key), func(rc *RunCtx) error {
				defer func() {
					if rec := recover(); rec != nil {
						panics <- rec
						panic(rec)
					}
				}()
				writer, err := OpenStream(rc, key, WithStreamSource("source", true))
				if err != nil || !state {
					return err
				}
				return writer.Send(want)
			}))
			var callbacks atomic.Int32
			check := func(value T) error {
				callbacks.Add(1)
				if !reflect.DeepEqual(value, want) || reflect.TypeOf(value) != reflect.TypeOf(want) {
					return fmt.Errorf("restored value type %T differs from %T", value, want)
				}
				before, after := reflect.ValueOf(raw), reflect.ValueOf(value)
				if before.IsValid() && (before.Kind() == reflect.Slice || before.Kind() == reflect.Pointer) && before.Pointer() != after.Pointer() {
					return fmt.Errorf("restored value lost its shared reference")
				}
				if before.IsValid() && before.Kind() == reflect.Map && before.UnsafePointer() != after.UnsafePointer() {
					return fmt.Errorf("restored map lost its shared reference")
				}
				return nil
			}
			mustAdd(t, g, NewNode("sink", "", Requires(key), nil, func(rc *RunCtx) error {
				stream, err := Get(rc, key)
				if err != nil {
					return err
				}
				delivery, err := stream.Receive(rc)
				if err != nil {
					return err
				}
				if state {
					err = HandleMessage(rc, delivery, func(rc *RunCtx, _ T) error {
						value, present, err := GetMessageState[T](rc, "saved")
						if err != nil || !present {
							return fmt.Errorf("restored state missing: present=%t err=%v", present, err)
						}
						return check(value)
					})
				} else if err = check(delivery.Value); err == nil {
					err = delivery.Ack()
				}
				if err != nil {
					return err
				}
				if _, err := stream.Receive(rc); err != io.EOF {
					return fmt.Errorf("restored stream EOF: %v", err)
				}
				return nil
			}))
			hooks := checkpointStreamTestHooks(&checkpointProbe{})
			pending := []CheckpointStreamMessage{{Sequence: 1, Value: raw}}
			states := []CheckpointState{{Name: "saved", Type: reflect.TypeFor[T](), Value: raw}}
			open := hooks.StreamHooks.Open
			hooks.StreamHooks.Open = func(ctx context.Context, request CheckpointStreamOpen) (CheckpointStreamRecord, error) {
				record, err := open(ctx, request)
				if !state {
					record.Resumed, record.LastSequence, record.Pending = true, 1, pending
				}
				return record, err
			}
			hooks.StreamHooks.OpenMessage = func(_ context.Context, request CheckpointMessageAttemptRequest) (CheckpointMessageAttempt, error) {
				return CheckpointMessageAttempt{Attempt: CheckpointAttempt{Scope: request.Attempt + "/message", Index: 1}, State: states}, nil
			}
			e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- e.Wait() }()
			select {
			case err = <-done:
				if reject {
					if !isCheckpointFailure(err) || callbacks.Load() != 0 {
						t.Fatalf("non-assignable recovery reached business: callbacks=%d err=%v", callbacks.Load(), err)
					}
				} else if err != nil || callbacks.Load() != 1 {
					t.Fatalf("validated recovery failed: callbacks=%d err=%v", callbacks.Load(), err)
				}
			case <-ctx.Done():
				select {
				case rec := <-panics:
					t.Fatalf("install panicked and Wait stayed blocked: %v", rec)
				default:
					t.Fatal("Wait did not finish before the execution deadline")
				}
			}
			if reflect.TypeOf(pending[0].Value) != reflect.TypeOf(raw) || reflect.TypeOf(states[0].Value) != reflect.TypeOf(raw) {
				t.Fatal("normalization rewrote the hook's container")
			}
		})
	}
}

func TestCheckpointNamedPayloadInstall(t *testing.T) {
	key := NewKey[Stream[inputNames]]("stream")
	request := CheckpointStreamOpen{Site: Site{Graph: "install"}, Scope: "root", Attempt: "attempt", NodeID: "source", Key: checkpointKey(key.asRef()), Group: "default", Source: CheckpointStreamSource{ID: "source", Resumable: true}, Capacity: 1}
	raw := []string{"pending"}
	record := CheckpointStreamRecord{Stream: CheckpointStreamDescriptor{ID: CheckpointStreamID{Scope: request.Scope, NodeID: request.NodeID, Key: key.Name(), Group: request.Group, Generation: 1}, Graph: request.Site.Graph, Attempt: request.Attempt, Element: request.Key.Element, Source: request.Source, Capacity: request.Capacity}, Resumed: true, LastSequence: 1, Pending: []CheckpointStreamMessage{{Sequence: 1, Value: raw}}}
	if err := checkCheckpointStreamRecord(request, record); err != nil {
		t.Fatal(err)
	}
	stream := checkpointManagedStream[inputNames](1)
	defer func() {
		if !stream.mu.TryLock() {
			t.Error("checkpoint install retained the stream lock")
		} else {
			stream.mu.Unlock()
		}
		if rec := recover(); rec != nil {
			t.Errorf("validated payload installation panicked: %v", rec)
		}
	}()
	if err := stream.installCheckpointRecord(record); err != nil {
		t.Fatal(err)
	}
	select {
	case delivery := <-stream.queue:
		if !reflect.DeepEqual(delivery.Value, inputNames(raw)) || reflect.ValueOf(delivery.Value).Pointer() != reflect.ValueOf(raw).Pointer() {
			t.Fatal("installed payload lost its type or shared reference")
		}
	default:
		t.Fatal("validated payload was not installed")
	}
	// 首项可赋值、后项仅能 coerce：整批拒绝，不能留下已安装的第一项。
	invalid := record
	invalid.Stream.Capacity, invalid.LastSequence = 2, 2
	invalid.Pending = append(append([]CheckpointStreamMessage(nil), record.Pending...), CheckpointStreamMessage{Sequence: 2, Value: []any{"x"}})
	request.Capacity = 2
	if err := checkCheckpointStreamRecord(request, invalid); err == nil {
		t.Fatal("checkpoint validation accepted a merely coercible payload")
	}
	untouched := checkpointManagedStream[inputNames](2)
	if err := untouched.installCheckpointRecord(invalid); err == nil {
		t.Fatal("checkpoint installer accepted a merely coercible payload")
	}
	if !untouched.mu.TryLock() {
		t.Fatal("rejected batch retained the stream lock")
	}
	defer untouched.mu.Unlock()
	if untouched.last != 0 || len(untouched.pending) != 0 || len(untouched.queue) != 0 {
		t.Fatal("rejected batch installed a partial stream record")
	}
}

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
