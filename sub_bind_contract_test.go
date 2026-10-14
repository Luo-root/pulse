package pulse

import (
	"context"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	texttemplate "text/template"
)

// 父 ready 值经过 In、子节点读取与 Out 后仍为 ready，包含恢复出来的 nil 接口。
func subContractReadyValue[T any](t *testing.T, value T) {
	t.Helper()
	for _, mode := range []string{"ordinary", "checkpoint", "restored"} {
		t.Run(mode, func(t *testing.T) {
			p, c := NewKey[T]("parent"), NewKey[T]("child")
			out, other := NewKey[T]("out"), NewKey[T]("other")
			child, parent := coreContractGraph(t, "nil-child"), coreContractGraph(t, "nil-parent")
			coreContractOK(t, child.DeclareInput(NewPort(c)))
			coreContractOK(t, child.DeclareOutput(NewPort(c)))
			coreContractOK(t, parent.DeclareOutput(NewPort(out), NewPort(other)))
			var sourceRuns, childRuns, opened, commits atomic.Int32
			coreContractOK(t, parent.Add(NewNode("source", "", nil, Provides(p), func(rc *RunCtx) error {
				sourceRuns.Add(1)
				return Set(rc, p, value)
			})))
			coreContractOK(t, child.Add(NewNode("read", "", Requires(c), nil, func(rc *RunCtx) error {
				childRuns.Add(1)
				return coreContractRead(rc, c, value)
			})))
			coreContractOK(t, Sub(parent, "mount", "", []SubBind{In(p, c), Out(c, out), Out(c, other)}, child))
			var opts []ExecOption
			if mode != "ordinary" {
				hooks := coreContractHooks(&commits)
				hooks.OpenChild = func(_ context.Context, request CheckpointChild) ([]NodeCheckpoint, error) {
					opened.Add(1)
					if len(request.Inputs) != 1 || request.Inputs[0].State != SlotStateReady ||
						request.Inputs[0].Key.Type != reflect.TypeFor[T]() || !reflect.DeepEqual(request.Inputs[0].Value, value) {
						return nil, fmt.Errorf("child checkpoint inputs lost ready value: %+v", request.Inputs)
					}
					return nil, nil
				}
				opts = []ExecOption{WithExecCheckpoint("root", hooks)}
			}
			e, err := parent.NewExecution(coreContractContext(t), Inputs{}, opts...)
			coreContractOK(t, err)
			t.Cleanup(e.Cancel)
			if mode == "restored" {
				coreContractOK(t, e.RestoreNodes([]NodeCheckpoint{{
					NodeID: "source", Reason: NodeCompleted,
					Outputs: []CheckpointOutput{{
						Key: CheckpointKey{Name: p.Name(), Type: reflect.TypeFor[T]()}, State: SlotStateReady, Value: value,
					}},
				}}))
			}
			coreContractOK(t, e.Start())
			coreContractOK(t, e.Wait())
			for _, k := range []Key[T]{out, other} {
				got, state, err := ReadOutput(e, k)
				if err != nil || state != SlotStateReady || !reflect.DeepEqual(got, value) {
					t.Fatalf("output %q = (%#v, %q, %v), want (%#v, ready, nil)", k.Name(), got, state, err, value)
				}
			}
			wantSources := int32(1)
			if mode == "restored" {
				wantSources = 0
			}
			if sourceRuns.Load() != wantSources || childRuns.Load() != 1 || (mode != "ordinary" && opened.Load() != 1) {
				t.Fatalf("source=%d child=%d OpenChild=%d", sourceRuns.Load(), childRuns.Load(), opened.Load())
			}
		})
	}
}

func TestSubContractReadyInputs(t *testing.T) {
	t.Run("nil-any", func(t *testing.T) { subContractReadyValue[any](t, nil) })
	t.Run("nil-error-interface", func(t *testing.T) { subContractReadyValue[error](t, nil) })
	t.Run("typed-nil-interface", func(t *testing.T) { subContractReadyValue[any](t, (*int)(nil)) })
	t.Run("nil-pointer", func(t *testing.T) { subContractReadyValue[*int](t, nil) })
	t.Run("non-nil-interface", func(t *testing.T) { subContractReadyValue[any](t, 42) })
}

func subContractFixedValue[T any](t *testing.T, value T) {
	t.Helper()
	c, out := NewKey[T]("child"), NewKey[T]("out")
	child, parent := coreContractGraph(t, "fixed-child"), coreContractGraph(t, "fixed-parent")
	coreContractOK(t, child.DeclareInput(NewPort(c)))
	coreContractOK(t, child.DeclareOutput(NewPort(c)))
	coreContractOK(t, parent.DeclareOutput(NewPort(out)))
	coreContractOK(t, Sub(parent, "mount", "", []SubBind{Fix(c, value), Out(c, out)}, child))
	e := coreContractRun(t, parent, Inputs{})
	got, state, err := ReadOutput(e, out)
	if err != nil || state != SlotStateReady || !reflect.DeepEqual(got, value) {
		t.Fatalf("Fix output = (%#v, %q, %v)", got, state, err)
	}
}

func TestSubContractFixedInputsKeepHostNilRule(t *testing.T) {
	c, out := NewKey[any]("child"), NewKey[any]("out")
	child, parent := coreContractGraph(t, "fixed-nil-child"), coreContractGraph(t, "fixed-nil-parent")
	coreContractOK(t, child.DeclareInput(NewPort(c)))
	coreContractOK(t, child.DeclareOutput(NewPort(c)))
	coreContractOK(t, parent.DeclareOutput(NewPort(out)))
	err := Sub(parent, "mount", "", []SubBind{Fix[any](c, nil), Out(c, out)}, child)
	if err == nil || !strings.Contains(err.Error(), "nil value") || len(parent.Topology().Nodes) != 0 {
		t.Fatalf("Fix nil = %v", err)
	}
	coreContractOK(t, Sub(parent, "mount", "", []SubBind{FixSkip(c), Out(c, out)}, child))
	e := coreContractRun(t, parent, Inputs{})
	if got, state, err := ReadOutput(e, out); got != nil || state != SlotStateSkipped || err != nil {
		t.Fatalf("FixSkip = (%#v, %q, %v)", got, state, err)
	}
	t.Run("typed-nil-interface", func(t *testing.T) { subContractFixedValue[any](t, (*int)(nil)) })
	t.Run("nil-pointer", func(t *testing.T) { subContractFixedValue[*int](t, nil) })
}

func subContractResolve[T any](t *testing.T, reg *Registry, k Key[T]) keyRef {
	t.Helper()
	coreContractOK(t, RegisterKey(reg, k))
	ref, err := reg.ResolveKey(k.Name(), reflect.TypeFor[T]().String())
	coreContractOK(t, err)
	return ref
}

type subContractInt int

func TestSubContractRefsRejectTypeMismatch(t *testing.T) {
	reg := NewRegistry()
	integer := subContractResolve(t, reg, NewKey[int]("int"))
	text := subContractResolve(t, reg, NewKey[string]("text"))
	iface := subContractResolve(t, reg, NewKey[any]("any"))
	named := subContractResolve(t, reg, NewKey[subContractInt]("named"))
	html := subContractResolve(t, reg, NewKey[*htmltemplate.Template]("html"))
	tmpl := subContractResolve(t, reg, NewKey[*texttemplate.Template]("text-template"))
	if html.typ == tmpl.typ || html.typ.String() != tmpl.typ.String() {
		t.Fatal("same-display-name fixture must have different real types")
	}
	for _, tc := range []struct {
		name string
		p, c keyRef
	}{
		{"int-string", integer, text},
		{"assignable-is-not-same-type", integer, iface},
		{"same-underlying-type", integer, named},
		{"same-display-name", html, tmpl},
	} {
		for _, intoChild := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/in=%t", tc.name, intoChild), func(t *testing.T) {
				child, parent := coreContractGraph(t, "type-child"), coreContractGraph(t, "type-parent")
				goodParent, goodChild := NewKey[bool]("good-parent"), NewKey[bool]("good-child")
				coreContractOK(t, child.DeclareInput(NewPort(goodChild)))
				var bad, repaired SubBind
				correctParent := keyRef{name: tc.p.name, typ: tc.c.typ}
				if intoChild {
					coreContractOK(t, child.DeclareInput(PortRef(tc.c)))
					bad, repaired = InRef(tc.p, tc.c), InRef(correctParent, tc.c)
				} else {
					coreContractOK(t, child.DeclareOutput(PortRef(tc.c)))
					bad, repaired = OutRef(tc.c, tc.p), OutRef(tc.c, correctParent)
				}
				binds := []SubBind{In(goodParent, goodChild), bad}
				err := Sub(parent, "mount", "", binds, child)
				if err == nil || !strings.Contains(err.Error(), "type mismatch") {
					t.Fatalf("mismatched dynamic bind accepted: %v", err)
				}
				for _, want := range []string{"mount", tc.p.name, tc.c.name} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("type mismatch must identify %q: %v", want, err)
					}
				}
				if len(parent.Topology().Nodes) != 0 {
					t.Fatal("bad bind installed a node")
				}
				if _, found := parent.keys.typeOf(goodParent.Name()); found {
					t.Fatal("bad bind registered a preceding good key")
				}
				if _, found := parent.keys.typeOf(tc.p.name); found {
					t.Fatal("bad bind registered its parent key")
				}
				binds[1] = repaired
				coreContractOK(t, Sub(parent, "mount", "", binds, child))
			})
		}
	}
}

func TestSubContractRefsAcceptMatchingTypes(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		t.Run(fmt.Sprintf("dynamic=%t", dynamic), func(t *testing.T) {
			p, c, result, out := NewKey[int]("p"), NewKey[int]("c"), NewKey[int]("result"), NewKey[int]("out")
			child, parent := coreContractGraph(t, "matching-child"), coreContractGraph(t, "matching-parent")
			coreContractOK(t, child.DeclareInput(NewPort(c)))
			coreContractOK(t, child.DeclareOutput(NewPort(result)))
			coreContractOK(t, child.Add(NewNode("double", "", Requires(c), Provides(result), func(rc *RunCtx) error {
				v, err := Get(rc, c)
				if err != nil {
					return err
				}
				return Set(rc, result, 2*v)
			})))
			binds := []SubBind{In(p, c), Out(result, out)}
			if dynamic {
				reg := NewRegistry()
				binds = []SubBind{
					InRef(subContractResolve(t, reg, p), subContractResolve(t, reg, c)),
					OutRef(subContractResolve(t, reg, result), subContractResolve(t, reg, out)),
				}
			}
			coreContractOK(t, parent.DeclareInput(NewPort(p)))
			coreContractOK(t, parent.DeclareOutput(NewPort(out)))
			coreContractOK(t, Sub(parent, "mount", "", binds, child))
			var in Inputs
			coreContractOK(t, Seed(&in, p, 21))
			e := coreContractRun(t, parent, in)
			if got, state, err := ReadOutput(e, out); got != 42 || state != SlotStateReady || err != nil {
				t.Fatalf("matching bind output = (%d, %q, %v)", got, state, err)
			}
		})
	}
}

func TestSubContractRejectZeroBinds(t *testing.T) {
	p, c := NewKey[int]("parent"), NewKey[int]("child")
	var zero Key[int]
	for _, tc := range []struct {
		name string
		bind SubBind
	}{
		{"zero-bind", SubBind{}},
		{"in-zero-parent", In(zero, c)},
		{"in-zero-child", In(p, zero)},
		{"out-zero-parent", Out(c, zero)},
		{"out-zero-child", Out(zero, p)},
		{"dynamic-empty-parent", InRef(keyRef{typ: reflect.TypeFor[int]()}, c.asRef())},
		{"dynamic-empty-child", OutRef(keyRef{typ: reflect.TypeFor[int]()}, p.asRef())},
		{"dynamic-missing-parent-type", OutRef(c.asRef(), keyRef{name: "missing-type"})},
		{"dynamic-missing-child-type", InRef(p.asRef(), keyRef{name: "missing-type"})},
		{"zero-fix", Fix(zero, 1)},
		{"zero-fix-skip", FixSkip(zero)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child, parent := coreContractGraph(t, "zero-bind-child"), coreContractGraph(t, "zero-bind-parent")
			// 子图接口本身有效，避免新判据让旧式「先声明零键」的准备阶段就失败。
			coreContractOK(t, child.DeclareInput(NewPort(c)))
			coreContractOK(t, child.DeclareOutput(NewPort(c)))
			err := Sub(parent, "mount", "", []SubBind{tc.bind}, child)
			if err == nil || !strings.Contains(err.Error(), "zero") || len(parent.Topology().Nodes) != 0 {
				t.Fatalf("zero bind = %v", err)
			}
			coreContractOK(t, Sub(parent, "mount", "", []SubBind{In(p, c), Out(c, NewKey[int]("out"))}, child))
		})
	}
}

func TestSubContractParentDependenciesDeduplicate(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		for _, skipQ := range []bool{false, true} {
			t.Run(fmt.Sprintf("dynamic=%t/skip-q=%t", dynamic, skipQ), func(t *testing.T) {
				p, q := NewKey[int]("p"), NewKey[int]("q")
				a, b, c := NewKey[int]("a"), NewKey[int]("b"), NewKey[int]("c")
				sum, out := NewKey[int]("sum"), NewKey[int]("out")
				child, parent := coreContractGraph(t, "fan-in-child"), coreContractGraph(t, "fan-in-parent")
				coreContractOK(t, child.DeclareInput(NewPort(a), NewPort(b), NewPort(c)))
				coreContractOK(t, child.DeclareOutput(NewPort(sum)))
				coreContractOK(t, child.Add(NewNode("sum", "", Requires(a, b, c), Provides(sum), func(rc *RunCtx) error {
					total := 0
					for _, k := range []Key[int]{a, b, c} {
						v, err := Get(rc, k)
						if errors.Is(err, ErrSkipped) {
							continue
						}
						if err != nil {
							return err
						}
						total += v
					}
					return Set(rc, sum, total)
				})))
				binds := []SubBind{In(q, a), In(p, b), In(q, c), Out(sum, out)}
				if dynamic {
					reg := NewRegistry()
					pr, qr := subContractResolve(t, reg, p), subContractResolve(t, reg, q)
					binds = []SubBind{InRef(qr, subContractResolve(t, reg, a)), InRef(pr, subContractResolve(t, reg, b)),
						InRef(qr, subContractResolve(t, reg, c)), OutRef(subContractResolve(t, reg, sum), subContractResolve(t, reg, out))}
				}
				coreContractOK(t, parent.DeclareInput(NewPort(p), NewPort(q)))
				coreContractOK(t, parent.DeclareOutput(NewPort(out)))
				coreContractOK(t, Sub(parent, "mount", "", binds, child))
				top := parent.Topology()
				req := top.Nodes[0].Requires
				if len(req) != 2 || req[0].Name != q.Name() || req[1].Name != p.Name() {
					t.Fatalf("parent Requires must preserve first appearance: %+v", req)
				}
				if len(top.Nodes[0].Binds) != len(binds) || len(parent.nodes[0].binds) != len(binds) {
					t.Fatal("deduplication dropped a real child binding")
				}
				nodes := parent.CheckpointNodes()
				if len(nodes[0].Requires) != 2 || nodes[0].Requires[0].Name != q.Name() || nodes[0].Requires[1].Name != p.Name() {
					t.Fatalf("checkpoint declaration lost parent dependency order: %+v", nodes[0])
				}
				var in Inputs
				coreContractOK(t, Seed(&in, p, 7))
				want := 17
				if skipQ {
					coreContractOK(t, SkipSeed(&in, q))
					want = 7
				} else {
					coreContractOK(t, Seed(&in, q, 5))
				}
				e := coreContractRun(t, parent, in)
				if got, state, err := ReadOutput(e, out); got != want || state != SlotStateReady || err != nil {
					t.Fatalf("fan-in result = (%d, %q, %v), want %d", got, state, err, want)
				}
			})
		}
	}
}

func TestSubContractDuplicateBindingsStayRejected(t *testing.T) {
	p, q, a, b := NewKey[int]("p"), NewKey[int]("q"), NewKey[int]("a"), NewKey[int]("b")
	for _, binds := range [][]SubBind{
		{In(p, a), In(p, a)},
		{In(p, a), In(q, a)},
		{In(p, a), Fix(a, 1)},
		{Fix(a, 1), In(p, a)},
		{In(p, a), FixSkip(a)},
	} {
		child, parent := coreContractGraph(t, "duplicate-child"), coreContractGraph(t, "duplicate-parent")
		coreContractOK(t, child.DeclareInput(NewPort(a)))
		err := Sub(parent, "mount", "", binds, child)
		if err == nil || !strings.Contains(err.Error(), "bound twice") || len(parent.Topology().Nodes) != 0 {
			t.Fatalf("duplicate inlet accepted: %v", err)
		}
	}
	t.Run("ordinary-node-requires", func(t *testing.T) {
		g := coreContractGraph(t, "ordinary-duplicate")
		err := g.Add(NewNode("node", "", Requires(p, p), nil, func(*RunCtx) error { return nil }))
		if err == nil || !strings.Contains(err.Error(), "twice in Requires") || len(g.Topology().Nodes) != 0 {
			t.Fatalf("ordinary duplicate Requires = %v", err)
		}
	})
	t.Run("duplicate-parent-output", func(t *testing.T) {
		child, parent := coreContractGraph(t, "output-child"), coreContractGraph(t, "output-parent")
		coreContractOK(t, child.DeclareOutput(NewPort(a)))
		err := Sub(parent, "mount", "", []SubBind{Out(a, p), Out(a, p)}, child)
		if err == nil || !strings.Contains(err.Error(), "twice in Provides") || len(parent.Topology().Nodes) != 0 {
			t.Fatalf("duplicate Provides = %v", err)
		}
	})
	t.Run("same-parent-name-different-types", func(t *testing.T) {
		child, parent := coreContractGraph(t, "conflict-child"), coreContractGraph(t, "conflict-parent")
		textParent, textChild := NewKey[string](p.Name()), NewKey[string]("text-child")
		coreContractOK(t, child.DeclareInput(NewPort(a), NewPort(textChild)))
		err := Sub(parent, "mount", "", []SubBind{In(p, a), In(textParent, textChild)}, child)
		if err == nil || len(parent.Topology().Nodes) != 0 {
			t.Fatalf("dependency dedup hid parent type conflict: %v", err)
		}
		coreContractOK(t, parent.DeclareInput(NewPort(p)))
		coreContractOK(t, Sub(parent, "mount", "", []SubBind{In(p, a), Fix(textChild, "fixed")}, child))
	})
	t.Run("declared-parent-type-conflict", func(t *testing.T) {
		child, parent := coreContractGraph(t, "registered-child"), coreContractGraph(t, "registered-parent")
		coreContractOK(t, child.DeclareInput(NewPort(a), NewPort(b)))
		coreContractOK(t, parent.DeclareInput(NewPort(NewKey[string](p.Name()))))
		err := Sub(parent, "mount", "", []SubBind{In(p, a), In(p, b)}, child)
		if err == nil || !strings.Contains(err.Error(), "already registered") || len(parent.Topology().Nodes) != 0 {
			t.Fatalf("dedup hid existing parent type conflict: %v", err)
		}
	})
}
