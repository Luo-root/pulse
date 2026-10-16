package pulse

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"sync/atomic"
	"testing"
)

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
