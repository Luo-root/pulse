package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func coreContractGraph(t *testing.T, id string) *Graph {
	t.Helper()
	g, err := New(id, "")
	coreContractOK(t, err)
	return g
}

func coreContractOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func coreContractContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func coreContractRun(t *testing.T, g *Graph, in Inputs, opts ...ExecOption) *Execution {
	t.Helper()
	e, err := g.NewExecution(coreContractContext(t), in, opts...)
	coreContractOK(t, err)
	t.Cleanup(e.Cancel)
	coreContractOK(t, e.Start())
	coreContractOK(t, e.Wait())
	return e
}

// 只认公开的检查点钩子；不依赖其它修复新增的探针或恢复 helper。
func coreContractHooks(commits *atomic.Int32) CheckpointHooks {
	return CheckpointHooks{
		OpenAttempt: func(_ context.Context, r CheckpointAttemptRequest) (CheckpointAttempt, error) {
			return CheckpointAttempt{Scope: r.Parent + "/" + r.NodeID + "/" + r.Boundary, Index: 1}, nil
		},
		FailAttempt: func(context.Context, string) error { return nil },
		OpenChild:   func(context.Context, CheckpointChild) ([]NodeCheckpoint, error) { return nil, nil },
		Commit: func(context.Context, CheckpointCommit) error {
			commits.Add(1)
			return nil
		},
	}
}

func coreContractRead[T any](rc *RunCtx, k Key[T], want T) error {
	v, ok, skipped, err := TryGet(rc, k)
	if err != nil || !ok || skipped || !reflect.DeepEqual(v, want) {
		return fmt.Errorf("TryGet %q = (%#v, %t, %t, %v), want (%#v, true, false, nil)", k.Name(), v, ok, skipped, err, want)
	}
	v, err = Get(rc, k)
	if err != nil || !reflect.DeepEqual(v, want) {
		return fmt.Errorf("Get %q = (%#v, %v), want (%#v, nil)", k.Name(), v, err, want)
	}
	return nil
}

func coreContractReadyValue[T any](t *testing.T, value T) {
	t.Helper()
	for _, mode := range []string{"ordinary", "retry", "checkpoint", "restored"} {
		t.Run(mode, func(t *testing.T) {
			k := NewKey[T]("value")
			g := coreContractGraph(t, "ready-value")
			coreContractOK(t, g.DeclareOutput(NewPort(k)))
			var bodies, readers, commits atomic.Int32
			var aspects []Aspect
			if mode == "retry" {
				aspects = []Aspect{Retry(1, 0)}
			}
			coreContractOK(t, g.Add(NewNode("source", "", nil, Provides(k), func(rc *RunCtx) error {
				bodies.Add(1)
				if err := Set(rc, k, value); err != nil {
					return err
				}
				return coreContractRead(rc, k, value) // 同一尝试内的读回也得认 ready nil。
			}, aspects...)))
			coreContractOK(t, g.Add(NewNode("sink", "", Requires(k), nil, func(rc *RunCtx) error {
				readers.Add(1)
				return coreContractRead(rc, k, value)
			})))
			var opts []ExecOption
			if mode == "checkpoint" || mode == "restored" {
				opts = []ExecOption{WithExecCheckpoint("root", coreContractHooks(&commits))}
			}
			e, err := g.NewExecution(coreContractContext(t), Inputs{}, opts...)
			coreContractOK(t, err)
			t.Cleanup(e.Cancel)
			if mode == "restored" {
				coreContractOK(t, e.RestoreNodes([]NodeCheckpoint{{
					NodeID: "source", Reason: NodeCompleted,
					Outputs: []CheckpointOutput{{
						Key:   CheckpointKey{Name: k.Name(), Type: reflect.TypeFor[T]()},
						State: SlotStateReady, Value: value,
					}},
				}}))
			}
			coreContractOK(t, e.Start())
			coreContractOK(t, e.Wait())
			got, state, err := ReadOutput(e, k)
			if err != nil || state != SlotStateReady || !reflect.DeepEqual(got, value) {
				t.Fatalf("ReadOutput = (%#v, %q, %v), want (%#v, ready, nil)", got, state, err, value)
			}
			wantBodies := int32(1)
			if mode == "restored" {
				wantBodies = 0
			}
			if bodies.Load() != wantBodies || readers.Load() != 1 {
				t.Fatalf("source bodies=%d, sink bodies=%d; want %d, 1", bodies.Load(), readers.Load(), wantBodies)
			}
			if (mode == "checkpoint" && commits.Load() != 2) || (mode == "restored" && commits.Load() != 1) {
				t.Fatalf("commits=%d: restored source must not recommit", commits.Load())
			}
		})
	}
}

type coreContractError struct{}

func (*coreContractError) Error() string { return "typed nil error" }

func TestCoreReadyNilReads(t *testing.T) {
	t.Run("nil-any", func(t *testing.T) { coreContractReadyValue[any](t, nil) })
	t.Run("nil-error-interface", func(t *testing.T) { coreContractReadyValue[error](t, nil) })
	t.Run("typed-nil-in-any", func(t *testing.T) { coreContractReadyValue[any](t, (*int)(nil)) })
	t.Run("typed-nil-in-error", func(t *testing.T) { coreContractReadyValue[error](t, (*coreContractError)(nil)) })
	t.Run("nil-pointer", func(t *testing.T) { coreContractReadyValue[*int](t, nil) })
	t.Run("nil-slice", func(t *testing.T) { coreContractReadyValue[[]int](t, nil) })
	t.Run("non-nil-interface", func(t *testing.T) { coreContractReadyValue[any](t, 42) })
}

// 无类型的 nil 只对接口合法；错类型的值仍得报错，不能借返回 T 的零值掩盖。
func coreContractWrongValue[T any](t *testing.T, raw any) {
	t.Helper()
	k := NewKey[T]("value")
	g := coreContractGraph(t, "mistyped")
	n := NewNode("source", "", nil, Provides(k), func(*RunCtx) error { return nil })
	coreContractOK(t, g.Add(n))
	e, err := g.NewExecution(coreContractContext(t), Inputs{})
	coreContractOK(t, err)
	t.Cleanup(e.Cancel)
	rc := newRunCtx(e, n, e.Context())
	t.Cleanup(rc.Cancel)
	// 直接造损坏槽，隔离读取兜底；公开的 Set 不会造出这种错类型值。
	coreContractOK(t, e.slotOf(k.asRef()).resolveValue(raw))
	var zero T
	got, getErr := Get(rc, k)
	if getErr == nil || !strings.Contains(getErr.Error(), "type assertion failed") || !reflect.DeepEqual(got, zero) {
		t.Fatalf("Get mistyped value = (%#v, %v)", got, getErr)
	}
	got, ok, skipped, tryErr := TryGet(rc, k)
	if tryErr == nil || !strings.Contains(tryErr.Error(), "type assertion failed") || ok || skipped || !reflect.DeepEqual(got, zero) {
		t.Fatalf("TryGet mistyped value = (%#v, %t, %t, %v)", got, ok, skipped, tryErr)
	}
}

func TestCoreReadyReadsRejectMistypedValues(t *testing.T) {
	t.Run("wrong-concrete", func(t *testing.T) { coreContractWrongValue[string](t, 42) })
	t.Run("wrong-interface", func(t *testing.T) { coreContractWrongValue[error](t, 42) })
	t.Run("nil-concrete", func(t *testing.T) { coreContractWrongValue[string](t, nil) })
	t.Run("untyped-nil-pointer", func(t *testing.T) { coreContractWrongValue[*int](t, nil) })
}

func TestCoreNilReadKeepsSlotStatesAndPermissions(t *testing.T) {
	k := NewKey[any]("value")
	g := coreContractGraph(t, "read-states")
	n := NewNode("source", "", nil, Provides(k), func(*RunCtx) error { return nil })
	coreContractOK(t, g.Add(n))
	e, err := g.NewExecution(coreContractContext(t), Inputs{})
	coreContractOK(t, err)
	t.Cleanup(e.Cancel)
	rc := newRunCtx(e, n, e.Context())
	t.Cleanup(rc.Cancel)
	if v, ok, skipped, err := TryGet(rc, k); v != nil || ok || skipped || err != nil {
		t.Fatalf("pending = (%v, %t, %t, %v)", v, ok, skipped, err)
	}
	coreContractOK(t, e.slotOf(k.asRef()).resolveSkip())
	if v, ok, skipped, err := TryGet(rc, k); v != nil || ok || !skipped || err != nil {
		t.Fatalf("skipped = (%v, %t, %t, %v)", v, ok, skipped, err)
	}
	if _, err := Get(rc, k); !errors.Is(err, ErrSkipped) {
		t.Fatalf("Get skipped = %v", err)
	}
	wrong := NewKey[error](k.Name())
	if _, err := Get(rc, wrong); !errors.Is(err, ErrUndeclared) {
		t.Fatalf("wrong declaration Get = %v", err)
	}
	if _, ok, skipped, err := TryGet(rc, wrong); !errors.Is(err, ErrUndeclared) || ok || skipped {
		t.Fatalf("wrong declaration TryGet = (%t, %t, %v)", ok, skipped, err)
	}
}

func TestCoreInputsKeepHostNilContract(t *testing.T) {
	k := NewKey[any]("value")
	var in Inputs
	if err := Seed[any](&in, k, nil); err == nil || !strings.Contains(err.Error(), "nil value") {
		t.Fatalf("Seed nil interface = %v", err)
	}
	if len(in.entries) != 0 {
		t.Fatal("rejected nil left an input")
	}
	coreContractOK(t, Seed[any](&in, k, (*int)(nil)))
	g := coreContractGraph(t, "host-typed-nil")
	coreContractOK(t, g.DeclareInput(NewPort(k)))
	coreContractOK(t, g.DeclareOutput(NewPort(k)))
	e := coreContractRun(t, g, in)
	v, state, err := ReadOutput(e, k)
	if err != nil || state != SlotStateReady || !reflect.DeepEqual(v, (*int)(nil)) {
		t.Fatalf("host typed nil = (%#v, %q, %v)", v, state, err)
	}
	var pointerInput Inputs
	coreContractOK(t, Seed[*int](&pointerInput, NewKey[*int]("pointer"), nil))
	if err := Seed[any](nil, k, 1); err == nil {
		t.Fatal("nil Inputs accepted")
	}
	for _, firstSkip := range []bool{false, true} {
		t.Run(fmt.Sprintf("first-skip=%t", firstSkip), func(t *testing.T) {
			var duplicate Inputs
			if firstSkip {
				coreContractOK(t, SkipSeed(&duplicate, k))
			} else {
				coreContractOK(t, Seed[any](&duplicate, k, 7))
			}
			if err := Seed[any](&duplicate, k, 8); err == nil {
				t.Fatal("second value accepted")
			}
			if err := SkipSeed(&duplicate, k); err == nil {
				t.Fatal("second skip accepted")
			}
			en := duplicate.entries[k.Name()]
			if en.skip != firstSkip || (!firstSkip && en.value != 7) {
				t.Fatalf("first input overwritten: %+v", en)
			}
		})
	}
}

func TestCoreInputsRejectZeroKeys(t *testing.T) {
	for _, k := range []Key[string]{{}, NewKey[string]("")} {
		for _, skip := range []bool{false, true} {
			var in Inputs
			var err error
			if skip {
				err = SkipSeed(&in, k)
			} else {
				err = Seed(&in, k, "value")
			}
			if err == nil || !strings.Contains(err.Error(), "zero key") || len(in.entries) != 0 {
				t.Fatalf("empty-name input skip=%t: err=%v entries=%v", skip, err, in.entries)
			}
			coreContractOK(t, Seed(&in, NewKey[string]("valid"), "value"))
		}
	}
	for _, ref := range []keyRef{{}, {name: "missing-type"}, {typ: reflect.TypeFor[string]()}} {
		var in Inputs
		if err := in.setArrived(ref, "value", false); err == nil || len(in.entries) != 0 {
			t.Fatalf("internal bridge accepted invalid key %v: %v", ref, err)
		}
	}
	// 内部允许的是接口零值，不是任意损坏的 nil 槽。
	var in Inputs
	if err := in.setArrived(NewKey[string]("value").asRef(), nil, false); err == nil {
		t.Fatal("internal bridge accepted untyped nil for string")
	}
}

func TestCorePortsRejectZeroKeysAtomically(t *testing.T) {
	for _, tc := range []struct {
		name string
		port Port
	}{
		{"zero-port", Port{}},
		{"zero-key", NewPort(Key[string]{})},
		{"empty-key", NewPort(NewKey[string](""))},
		{"dynamic-empty-name", PortRef(keyRef{typ: reflect.TypeFor[string]()})},
		{"dynamic-missing-type", PortRef(keyRef{name: "missing-type"})},
	} {
		for _, input := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/input=%t", tc.name, input), func(t *testing.T) {
				g := coreContractGraph(t, "invalid-port")
				good := NewKey[string]("good")
				var err error
				if input {
					err = g.DeclareInput(NewPort(good), tc.port)
				} else {
					err = g.DeclareOutput(NewPort(good), tc.port)
				}
				if err == nil || !strings.Contains(err.Error(), "zero Port") {
					t.Fatalf("Declare port = %v", err)
				}
				top := g.Topology()
				if len(top.Graph.InPorts) != 0 || len(top.Graph.OutPorts) != 0 {
					t.Fatalf("failed batch left ports: %+v", top.Graph)
				}
				// 换类型登记第一条，证明键表也没有半批痕迹；合法端口仍可用。
				coreContractOK(t, g.DeclareInput(NewPort(NewKey[int](good.Name()))))
				coreContractOK(t, g.DeclareOutput(NewPort(NewKey[int](good.Name()))))
				coreContractOK(t, g.Validate())
			})
		}
	}
}

func TestCoreNodeDeclarationsRejectZeroKeys(t *testing.T) {
	for _, ref := range []keyRef{Key[string]{}.asRef(), {name: "missing-type"}} {
		for _, requires := range []bool{true, false} {
			g := coreContractGraph(t, "invalid-node-key")
			var req, prov []keyRef
			if requires {
				req = []keyRef{NewKey[int]("good").asRef(), ref}
			} else {
				prov = []keyRef{NewKey[int]("good").asRef(), ref}
			}
			err := g.Add(NewNode("bad", "", req, prov, func(*RunCtx) error { return nil }))
			if err == nil || !strings.Contains(err.Error(), "zero key") || len(g.Topology().Nodes) != 0 {
				t.Fatalf("invalid node requires=%t ref=%v: %v", requires, ref, err)
			}
			coreContractOK(t, g.Add(NewNode("bad", "", nil, Provides(NewKey[string]("good")), func(*RunCtx) error { return nil })))
		}
	}
}
