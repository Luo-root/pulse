package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

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
