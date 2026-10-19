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
	"time"
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
					if len(request.Bindings) != 1 || request.Bindings[0].Parent.Name != p.Name() ||
						request.Bindings[0].Child.Name != c.Name() || request.Bindings[0].Child.Type != reflect.TypeFor[T]() {
						return nil, fmt.Errorf("child checkpoint lost stable input binding: %+v", request.Bindings)
					}
					// In 可以尚未到达；若已附带快照，ready nil 仍必须原样保留。
					if len(request.Inputs) > 1 || (len(request.Inputs) == 1 && (request.Inputs[0].State != SlotStateReady ||
						request.Inputs[0].Key.Type != reflect.TypeFor[T]() || !reflect.DeepEqual(request.Inputs[0].Value, value))) {
						return nil, fmt.Errorf("child checkpoint changed an arrived value: %+v", request.Inputs)
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

func TestSubRejectsSealedDefinitionAtItsOwnEntry(t *testing.T) {
	for _, seal := range []string{"Validate", "Start"} {
		t.Run(seal, func(t *testing.T) {
			parent, child := coreContractGraph(t, "sealed-parent"), coreContractGraph(t, "mountable-child")
			in, out := NewKey[int]("child-in"), NewKey[int]("parent-out")
			coreContractOK(t, child.DeclareInput(NewPort(in)))
			coreContractOK(t, child.DeclareOutput(NewPort(in)))
			if seal == "Validate" {
				coreContractOK(t, parent.Validate())
			} else {
				coreContractRun(t, parent, Inputs{})
			}
			err := Sub(parent, "late", "", []SubBind{Fix(in, 1), Out(in, out)}, child)
			if !errors.Is(err, ErrGraphStarted) || !strings.Contains(err.Error(), "Sub") ||
				!strings.Contains(err.Error(), "sealed") || strings.Contains(err.Error(), "Add") {
				t.Fatalf("sealed Sub = %v, want Sub diagnostic wrapping ErrGraphStarted", err)
			}
			if len(parent.nodes) != 0 || len(parent.nodeIDs) != 0 || len(parent.producer) != 0 {
				t.Fatal("rejected Sub changed the parent definition")
			}
			coreContractOK(t, child.Add(NewNode("still-open", "", nil, nil, func(*RunCtx) error { return nil })))
		})
	}
}

func TestSubContractFixedInputsKeepHostNilRule(t *testing.T) {
	c, out := NewKey[any]("child"), NewKey[any]("out")
	child, parent := coreContractGraph(t, "fixed-nil-child"), coreContractGraph(t, "fixed-nil-parent")
	coreContractOK(t, child.DeclareInput(NewPort(c)))
	coreContractOK(t, child.DeclareOutput(NewPort(c)))
	coreContractOK(t, parent.DeclareOutput(NewPort(out)))
	err := Sub(parent, "mount", "", []SubBind{Fix[any](c, nil), Out(c, out)}, child)
	if err == nil || !strings.Contains(err.Error(), "nil value") ||
		!strings.Contains(err.Error(), "FixSkip") || strings.Contains(err.Error(), "SkipSeed") || len(parent.Topology().Nodes) != 0 {
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

// 节点 ID 恰好叫 `seed` 时，「一个 Key 一个来源」不能被绕过。
//
// 旧实现把外部来源记成字符串哨兵 `"seed"`：节点 `seed` 提供 K 之后来源表里存的就是
// `"seed"`，于是那条冲突判据认为「同一个来源」——**静默返回 nil**，宿主的值抢先占住
// 槽位，下游读到宿主值，整图照样成功。结构化来源之后，表里只有节点来源、判据只按
// `kind` 走，名字再像也分得开。
//
// 阶段 2 之后「喂值」不再往定义里写（`Seed` 写的是**这一次运行的** `Inputs`），所以这次
// 撞车从「喂值那一刻报」搬到「建执行那一刻报」——纪律不变：**图内已有来源时，外面喂进来
// 的值一律被拒**（放进来的话那个节点的产出会被幂等首写静默顶掉），下游读到的仍是节点
// 那条值。两个方向都钉住，才算把「撞名」这件事盖全。
//
// 这里**不**给那条键声明端口：那样装配期会先撞「声明过的进口 + 节点产出」那条判据
// （见 TestInputPortAndProducerConflict），盖住这里要测的「图内来源 vs 外面喂值」。
func TestSourceConflictWhenNodeIsNamedSeed(t *testing.T) {
	k := NewKey[string]("src.clash")
	provide := func(rc *RunCtx) error { return Set(rc, k, "from-node") }

	// 先 Add 后喂：建执行当场报。
	g := mustNew(t, context.Background(), "P")
	mustAdd(t, g, NewNode("seed", "", nil, Provides(k), provide))
	if err := seedOn(g, k, "host"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.NewExecution(context.Background(), pendingOf(g).in); err == nil {
		t.Fatal("先 Add 后喂：图内已有来源，外面喂的值必须被拒，不能静默顶掉节点的产出")
	}
	// 「值不许被顶掉」的正面证据：清掉那份输入再跑，读到的必须是节点那条。
	pendingOf(g).in = Inputs{}
	var got string
	mustAdd(t, g, NewNode("reader", "", Requires(k), nil, func(rc *RunCtx) error {
		v, err := Get(rc, k)
		if err != nil {
			return err
		}
		got = v
		return nil
	}))
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if got != "from-node" {
		t.Fatalf("读到的值 = %q，want from-node（宿主那份不该被写进去）", got)
	}

	// 先喂后 Add：装得进去（喂值不占来源），但拿那份输入去跑的建执行那一刻同样被拒
	// ——这一侧旧实现是**静默通过**的（来源表里存的是 `"seed"`，节点 id 恰好也叫
	// `"seed"`，相等那一支放行）。
	g2 := mustNew(t, context.Background(), "P2")
	if err := seedOn(g2, k, "host"); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g2, NewNode("seed", "", nil, Provides(k), provide))
	if _, err := g2.NewExecution(context.Background(), pendingOf(g2).in); err == nil {
		t.Fatal("先喂后 Add：装完节点再拿那份输入跑，必须报")
	}

	// 来源表里不许出现零值（sourceNone）——「没有来源」的表达是「表里没有这个 Key」。
	for name, src := range g.producer {
		if !src.present() {
			t.Fatalf("来源表里出现了零值：%s → %+v", name, src)
		}
	}
}

// 节点 ID 恰好叫 `seed` 的**合法无环图**必须跑得起来。
//
// 旧实现在环检查里拿来源名去查节点索引：external 由外部 Seed 提供、来源名是 `"seed"`，
// 正好查到那个节点自己 → 造出一条**自环** → `Start` 报一个不存在的环。对照组（节点改个
// 名字）旧实现是通过的——两条一起测，才说明修的是「来源名偶然等于节点名」，而不是
// 把合法图拒掉。
//
// 图里必须有**两个以上节点**：`problemsCycleLocked` 在只有一个节点时直接返回（自环
// 在 `Add` 就被拒了，不可能存在），单节点图根本走不到这段判据——用它测不出这个 bug。
func TestNodeNamedSeedDoesNotFakeCycle(t *testing.T) {
	for _, id := range []string{"seed", "loader"} {
		external := NewKey[string]("src.fake.ext." + id)
		out := NewKey[string]("src.fake.out." + id)
		final := NewKey[string]("src.fake.final." + id)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		g := mustNew(t, ctx, "P-"+id)
		// 第一个节点不依赖自己：它读的 external 由外部 Seed 提供。
		err := g.Add(NewNode(id, "", Requires(external), Provides(out), func(rc *RunCtx) error {
			v, err := Get(rc, external)
			if err != nil {
				return err
			}
			return Set(rc, out, v+"-ok")
		}))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		// 第二个节点让图「不止一个节点」，环检查才会真的跑。
		err = g.Add(NewNode("sink", "", Requires(out), Provides(final), func(rc *RunCtx) error {
			v, err := Get(rc, out)
			if err != nil {
				return err
			}
			return Set(rc, final, v+"!")
		}))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		declareIn(t, g, NewPort(external))
		if err := seedOn(g, external, "v"); err != nil {
			cancel()
			t.Fatal(err)
		}
		err = runOn(g)
		cancel()
		if err != nil {
			t.Fatalf("节点 id %q：合法的无环图被拒了：%v", id, err)
		}
		if st, v := slotOn(g, final.asRef()); st != slotReady || v != "v-ok!" {
			t.Fatalf("节点 id %q：final = (%v, %v)，want (ready, v-ok!)", id, st, v)
		}
	}
}

// 冲突文案：报文要**点名那条键与那个节点**，且节点 id 是任意用户字符串——一律写成
// `node %q` 统一格式（不是裸 id，也不是给 `seed` 这个哨兵名字开小灶），所以节点叫
// `seed`、叫 `none`、id 本身长成 `node "seed"` 的都分得开（见
// TestSourceNameDistinguishesKinds）。两个方向（先 Add 后喂 / 先喂后 Add）走同一条报文。
//
// 同样**不**声明端口：声明了的话装配期先撞「进口 vs 产出」那条，这条文案就测不到了。
func TestSourceConflictText(t *testing.T) {
	const wantTmpl = `pulse: input src.txt.confl:string: this key is provided by node %q inside ` +
		`graph "P", so a value fed from outside would be silently ignored (a key has exactly one source)`
	for _, nodeID := range []string{"load", "seed", "none", `node "seed"`} {
		t.Run(nodeID, func(t *testing.T) {
			k := NewKey[string]("src.txt.confl")
			provide := func(*RunCtx) error { return nil }

			// 先 Add 后喂。
			g := mustNew(t, context.Background(), "P")
			mustAdd(t, g, NewNode(nodeID, "", nil, Provides(k), provide))
			if err := seedOn(g, k, "v"); err != nil {
				t.Fatal(err)
			}
			_, err := g.NewExecution(context.Background(), pendingOf(g).in)
			if err == nil {
				t.Fatal("want 冲突，got nil")
			}
			if want := fmt.Sprintf(wantTmpl, nodeID); err.Error() != want {
				t.Fatalf("文案不对\n got %s\nwant %s", err.Error(), want)
			}

			// 先喂后 Add：同一条报文（撞车在建执行那一刻判，不在 Add 那一刻）。
			g2 := mustNew(t, context.Background(), "P")
			if err := seedOn(g2, k, "v"); err != nil {
				t.Fatal(err)
			}
			mustAdd(t, g2, NewNode(nodeID, "", nil, Provides(k), provide))
			_, err = g2.NewExecution(context.Background(), pendingOf(g2).in)
			if err == nil {
				t.Fatal("want 冲突，got nil")
			}
			if want := fmt.Sprintf(wantTmpl, nodeID); err.Error() != want {
				t.Fatalf("文案不对\n got %s\nwant %s", err.Error(), want)
			}
		})
	}
}

// 来源名必须**两两可分**：节点 id 是任意用户字符串，只给某个哨兵名字开小灶是不够的
// （实测：节点 id 叫 `none` 会与「没有来源」的 `none` 撞，字面量 id `node "seed"` 会与
// 节点 `seed` 的输出撞）。这里把几种名字钉死，并断言两两不同——「报错里分得开」正是
// 这次修的那两处静默错误在**文案层**的对应物。
func TestSourceNameDistinguishesKinds(t *testing.T) {
	cases := []struct {
		name string
		src  source
		want string
	}{
		{"声明过的入口", source{kind: sourceInput}, "input"},
		{"普通节点", source{kind: sourceNode, nodeID: "load"}, `node "load"`},
		{"节点 id 恰好是 seed", source{kind: sourceNode, nodeID: "seed"}, `node "seed"`},
		{"节点 id 恰好是 none", source{kind: sourceNode, nodeID: "none"}, `node "none"`},
		{"节点 id 本身长成 node \"seed\"", source{kind: sourceNode, nodeID: `node "seed"`}, `node "node \"seed\""`},
		{"没有来源（零值）", source{}, "none"},
	}
	seen := map[string]string{}
	for _, c := range cases {
		got := c.src.String()
		if got != c.want {
			t.Fatalf("%s：String() = %q，want %q", c.name, got, c.want)
		}
		if prev, dup := seen[got]; dup {
			t.Fatalf("%s 与 %s 都写成 %q——来源名必须两两可分", c.name, prev, got)
		}
		seen[got] = c.name
	}
}

// 来源表里落了零值（`sourceNone`）时，所有判据都必须把它当「没有来源」。
//
// 零值只可能由包内写进去（字段未导出），所以这条只能在包内测：一旦谁把它当「有来源」，
// 等这个 Key 的节点会永远停在 pending——`Run` 不返回，也没有任何报错。
func TestZeroSourceInTableCountsAsMissing(t *testing.T) {
	k := NewKey[string]("src.zero")

	g := mustNew(t, context.Background(), "P")
	mustAdd(t, g, NewNode("waiter", "", Requires(k), nil, func(*RunCtx) error { return nil }))

	// 正常路径：没声明过的 Key 根本不在表里。
	if _, ok := g.producer[k.name]; ok {
		t.Fatal("没声明过的 Key 不该出现在来源表里")
	}
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "nothing provides it") {
		t.Fatalf("没有来源的 Key 必须被拒：%v", err)
	}

	// 防御：把零值塞进表里，判据仍须按「没有来源」拒。
	g.mu.Lock()
	g.producer[k.name] = source{}
	g.mu.Unlock()
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "nothing provides it") {
		t.Fatalf("零值来源必须按「没有来源」拒：%v", err)
	}

	// 同一个零值也不能让 `Start` 放行——那是「等不到值的图」被判成合法。
	if err := startOn(g); err == nil || !strings.Contains(err.Error(), "nothing provides it") {
		t.Fatalf("零值来源必须挡住 Start：%v", err)
	}
}

// 装配期可以先问一句「这张图跑得起来吗」：Validate 跑的是 Start 那套只读判据
// （每个 Requires 都有来源 / 依赖无环 / 流式名额与出口），但**不改图的状态**
// ——不置 started、不建 goroutine，所以问完还能接着装、接着跑。
//
// 它存在的理由在装配器那一侧：声明式装配（pulse/yaml）里的子图是运行到才建的，
// 没有这个入口，「子图里这条键没人提供」与「子图里依赖成环」就只能等它第一次
// 运行才由引擎拒掉——而这两条读一遍声明就能定。
func TestValidateReportsWhatStartWouldReject(t *testing.T) {
	x := NewKey[int]("val.x")
	y := NewKey[int]("val.y")

	t.Run("依赖成环：与 Start 报同一句话", func(t *testing.T) {
		g := mustNew(t, context.Background(), "test")
		mustAdd(t, g, NewNode("A", "", Requires(y), Provides(x), func(rc *RunCtx) error { return nil }))
		mustAdd(t, g, NewNode("B", "", Requires(x), Provides(y), func(rc *RunCtx) error { return nil }))

		verr := g.Validate()
		if verr == nil {
			t.Fatal("Validate 放行了一张含环的图")
		}
		if !strings.Contains(verr.Error(), "dependency cycle") {
			t.Fatalf("环要报出成环的那条路径，got %v", verr)
		}
		// 一份判据、两个入口：两边的报文逐字相同，装图期提前报的那句才不需要翻译
		serr := startOn(g)
		if serr == nil || serr.Error() != verr.Error() {
			t.Fatalf("Validate 与 Start 的判据不一致：\nValidate %v\nStart    %v", verr, serr)
		}
		// 校验不改状态：图仍是「没启动」，Wait 照报 ErrGraphNotStarted
		if werr := waitOn(g); !errors.Is(werr, ErrGraphNotStarted) {
			t.Fatalf("Validate 把图启动起来了？Wait() = %v", werr)
		}
	})

	t.Run("无来源的 Requires", func(t *testing.T) {
		g := mustNew(t, context.Background(), "test")
		mustAdd(t, g, NewNode("waiter", "", Requires(kA), nil, func(rc *RunCtx) error { return nil }))
		err := g.Validate()
		if err == nil {
			t.Fatal("Validate 放行了一张等不到值的图")
		}
		if !strings.Contains(err.Error(), `"waiter"`) || !strings.Contains(err.Error(), `nothing provides it`) {
			t.Fatalf("错误里要能读到节点 id 与病因，got %v", err)
		}
		// 补上来源（这正是装配器要留出的余地：问一次不该把图问死）
		declareIn(t, g, NewPort(kA))
		if err := seedOn(g, kA, "seeded"); err != nil {
			t.Fatalf("Validate 之后补 Seed 被拒：%v（说明它改了状态）", err)
		}
		if err := g.Validate(); err != nil {
			t.Fatalf("补上来源后应通过：%v", err)
		}
	})

	t.Run("通过即封口：定义不再变，但运行照旧", func(t *testing.T) {
		g := mustNew(t, context.Background(), "test")
		// 这条输入是**声明的入口**：它让「没人在图里提供 kA」不再算毛病
		if err := g.DeclareInput(NewPort(kA)); err != nil {
			t.Fatalf("声明入口失败：%v", err)
		}
		mustAdd(t, g, NewNode("n", "", Requires(kA), Provides(kB), func(rc *RunCtx) error {
			v, err := Get(rc, kA)
			if err != nil {
				return err
			}
			return Set(rc, kB, v)
		}))
		if err := g.Validate(); err != nil {
			t.Fatalf("干净的图不该被 Validate 拒：%v", err)
		}
		// 封口 = 定义不再变化：补节点被拒，而且是 ErrGraphStarted 这个哨兵
		err := g.Add(NewNode("m", "", Requires(kB), nil, func(rc *RunCtx) error { return nil }))
		if !errors.Is(err, ErrGraphStarted) {
			t.Fatalf("封口后 Add 的错应是 ErrGraphStarted，got %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "sealed") {
			t.Fatalf("封口后 Add 的报错该说清是定义封口，got %v", err)
		}
		// Seed 是**本次运行**的值，不属于定义：封口后照样能喂、能跑（`kA` 在开头就
		// 声明成入口了，封口不会再拦它）
		if err := seedOn(g, kA, "from-host"); err != nil {
			t.Fatalf("封口后 Seed 被拒：%v（Seed 是本次运行的值，不属于定义）", err)
		}
		if err := runOn(g); err != nil {
			t.Fatalf("封口之后照常跑：%v", err)
		}
		// 已经启动的图直接通过：它在 Start 那一刻就过了同一套判据
		if err := g.Validate(); err != nil {
			t.Fatalf("已跑完的图 Validate = %v, want nil", err)
		}
	})

	t.Run("校验失败不封口：补上缺的那块再校验正是要支持的用法", func(t *testing.T) {
		g := mustNew(t, context.Background(), "test")
		mustAdd(t, g, NewNode("waiter", "", Requires(kA), nil, func(rc *RunCtx) error { return nil }))
		if err := g.Validate(); err == nil {
			t.Fatal("缺来源的图不该过校验")
		}
		// 失败之后**没有**封口：补一个提供者就能接着装、接着校验
		mustAdd(t, g, NewNode("feeder", "", nil, Provides(kA), func(rc *RunCtx) error {
			return Set(rc, kA, "v")
		}))
		if err := g.Validate(); err != nil {
			t.Fatalf("补上提供者后应通过：%v", err)
		}
	})

	// passThrough 是三层嵌套的一棵树：root → mid → leaf，每层都是「入口 → 出口」。
	// 挂上去的子图是**父图这张定义的一部分**，所以封口封的是整棵子树（见 Graph.Validate）。
	passThrough := func(t *testing.T) (root, mid, leaf *Graph, keys map[string]Key[string]) {
		t.Helper()
		keys = map[string]Key[string]{}
		k := func(name string) Key[string] {
			if _, ok := keys[name]; !ok {
				keys[name] = NewKey[string](name)
			}
			return keys[name]
		}
		ports := func(id string) *Graph {
			t.Helper()
			g := mustNew(t, context.Background(), id)
			if err := g.DeclareInput(NewPort(k(id + ".in"))); err != nil {
				t.Fatalf("%s.DeclareInput = %v", id, err)
			}
			if err := g.DeclareOutput(NewPort(k(id + ".out"))); err != nil {
				t.Fatalf("%s.DeclareOutput = %v", id, err)
			}
			return g
		}
		// 只有最内层有真正干活的节点，中间两层各只有「挂子图」那一步——
		// 于是每层那条出口的来源都是它自己的挂载节点，不会跟别的节点撞来源。
		leaf = ports("seal.leaf")
		mustAdd(t, leaf, NewNode("step", "", Requires(k("seal.leaf.in")), Provides(k("seal.leaf.out")), func(rc *RunCtx) error {
			v, err := Get(rc, k("seal.leaf.in"))
			if err != nil {
				return err
			}
			return Set(rc, k("seal.leaf.out"), v)
		}))
		mid = ports("seal.mid")
		if err := Sub(mid, "inner", "", []SubBind{
			In(k("seal.mid.in"), k("seal.leaf.in")),
			Out(k("seal.leaf.out"), k("seal.mid.out")),
		}, leaf); err != nil {
			t.Fatalf("mid 挂 leaf = %v", err)
		}
		root = ports("seal.root")
		if err := Sub(root, "outer", "", []SubBind{
			In(k("seal.root.in"), k("seal.mid.in")),
			Out(k("seal.mid.out"), k("seal.root.out")),
		}, mid); err != nil {
			t.Fatalf("root 挂 mid = %v", err)
		}
		return root, mid, leaf, keys
	}

	t.Run("通过即封口：整棵子树一起封（挂上去的子图也是这张定义的一部分）", func(t *testing.T) {
		root, mid, leaf, keys := passThrough(t)
		if err := root.Validate(); err != nil {
			t.Fatalf("干净的图树不该被 Validate 拒：%v", err)
		}
		for _, g := range []*Graph{root, mid, leaf} {
			err := g.Add(NewNode("late", "", nil, nil, func(rc *RunCtx) error { return nil }))
			if !errors.Is(err, ErrGraphStarted) {
				t.Fatalf("%s：挂上去的子图也在这次校验里，该一起封，got %v", g.ID(), err)
			}
			if err == nil || !strings.Contains(err.Error(), "sealed") {
				t.Fatalf("%s：报错该点明是定义封口，got %v", g.ID(), err)
			}
			if err := g.DeclareInput(NewPort(NewKey[string](g.ID() + ".late_in"))); !errors.Is(err, ErrGraphStarted) {
				t.Fatalf("%s：声明也该被拒，got %v", g.ID(), err)
			}
		}
		// 封口只管**定义**：这一次运行的值不属于定义，所以封口之后照样喂得进去、
		// 整棵树照样能跑。（旧版本这里还有半句「封口后子图上照样能 seed」——阶段 2
		// 起「往子图种值」这条入口整个消失：子图的输入由挂载点的绑定给，那是装配期的
		// 事，不是某一次运行的值。）
		if err := seedOn(root, keys["seal.root.in"], "from-host"); err != nil {
			t.Fatalf("封口后根图 Seed 被拒：%v（Seed 是本次运行的值，不属于定义）", err)
		}
		if err := runOn(root); err != nil {
			t.Fatalf("封口之后照常跑：%v", err)
		}
		// 同一张子图挂两处：两处都封得上（重复走到已经封住的那张上不报错、也不重复递归）
		other := mustNew(t, context.Background(), "seal.other")
		if err := other.DeclareInput(NewPort(keys["seal.leaf.in"])); err != nil {
			t.Fatalf("other.DeclareInput = %v", err)
		}
		if err := Sub(other, "again", "", []SubBind{
			In(keys["seal.leaf.in"], keys["seal.leaf.in"]),
			Out(keys["seal.leaf.out"], keys["seal.leaf.out"]),
		}, leaf); err != nil {
			t.Fatalf("已封口的子图照样能被别处挂：%v", err)
		}
		if err := other.Validate(); err != nil {
			t.Fatalf("other.Validate = %v", err)
		}
	})

	// 与上一条是**两个入口、同一条规则**：`Start()` 成功同样封整棵子树。少了它，持有子图
	// 指针的调用方能在「父图已经跑起来、子图还没轮到」之间接着改子图（实测 `Add` 与
	// `DeclareInput` 都进得去），于是真正跑的就不是观察与拓扑看到的那张图。
	t.Run("启动成功也封整棵子树：父图跑起来之后、子图还没轮到也改不了", func(t *testing.T) {
		_, mid, leaf, keys := passThrough(t)
		// 根图这一份单独装：入口那条键由 gate 提供，而且它是一条**内部键**（不是声明的端口）
		// ——声明过的进口不许再被节点产出（见 DeclareInput），那样就装不出「等信号」这个
		// 确定窗口了。
		paper := NewKey[string]("seal.root.paper")
		rootOut := NewKey[string]("seal.root.out2")
		root := mustNew(t, context.Background(), "seal.root2")
		release := make(chan struct{})
		mustAdd(t, root, NewNode("gate", "", nil, Provides(paper),
			func(rc *RunCtx) error {
				<-release
				return Set(rc, paper, "v")
			}))
		if err := Sub(root, "outer", "", []SubBind{
			In(paper, keys["seal.mid.in"]),
			Out(keys["seal.mid.out"], rootOut),
		}, mid); err != nil {
			t.Fatalf("root 挂 mid = %v", err)
		}
		if err := startOn(root); err != nil {
			t.Fatalf("Start = %v", err)
		}
		// 此刻 root 的挂载步骤还堵在 gate 上，mid / leaf 一次都没跑过——封口照样已经盖上。
		for _, g := range []*Graph{root, mid, leaf} {
			err := g.Add(NewNode("late", "", nil, nil, func(rc *RunCtx) error { return nil }))
			if !errors.Is(err, ErrGraphStarted) {
				t.Fatalf("%s：父图 Start 成功该连子图一起封（此刻它还没跑），got %v", g.ID(), err)
			}
		}
		if err := mid.DeclareInput(NewPort(NewKey[string]("seal.mid.late"))); !errors.Is(err, ErrGraphStarted) {
			t.Fatalf("mid.DeclareInput after Start = %v, want ErrGraphStarted", err)
		}
		close(release)
		if err := waitOn(root); err != nil {
			t.Fatalf("封了并不妨碍跑：Wait = %v", err)
		}
	})

	t.Run("校验失败时整棵都不封（补上缺的那块再校验正是要支持的用法）", func(t *testing.T) {
		ghost := NewKey[string]("sealfail.ghost")
		in := NewKey[string]("sealfail.in")
		out := NewKey[string]("sealfail.out")
		rootIn := NewKey[string]("sealfail.root_in")

		child := mustNew(t, context.Background(), "sealfail.child")
		if err := child.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := child.DeclareOutput(NewPort(out)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		mustAdd(t, child, NewNode("hungry", "", Requires(ghost), Provides(out), func(rc *RunCtx) error {
			return Set(rc, out, "v")
		}))

		root := mustNew(t, context.Background(), "sealfail.root")
		if err := root.DeclareInput(NewPort(rootIn)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := Sub(root, "mount", "", []SubBind{In(rootIn, in), Out(out, out)}, child); err != nil {
			t.Fatalf("Sub = %v", err)
		}

		err := root.Validate()
		if err == nil || !strings.Contains(err.Error(), "sealfail.ghost") {
			t.Fatalf("子图里那条缺来源该从根图报出来，got %v", err)
		}
		// 失败的校验**不留封口**——两层都还该能接着装（子图被顺手封住是这里最容易犯的错）
		for _, g := range []*Graph{root, child} {
			if err := g.Add(NewNode("after", "", nil, nil, func(rc *RunCtx) error { return nil })); err != nil {
				t.Fatalf("%s：校验失败不该封口，got %v", g.ID(), err)
			}
		}
		// 补上病根，再校验一次：这次整棵一起封。
		//
		// 补法是一条**子图内部的产出**，而不是「把 ghost 声明成子图的进口」：声明成进口
		// 之后它就成了这张子图必需的进口（节点 `hungry` 要它），而这一处挂载并没有绑它
		// ——那是另一条装配期判据（见 TestSubRejectsUnboundRequiredInlet），会先报出来。
		// 两种补法都对，只是选哪一条决定了谁会报。
		mustAdd(t, child, NewNode("feeder", "", nil, Provides(ghost), func(rc *RunCtx) error {
			return Set(rc, ghost, "v")
		}))
		if err := root.Validate(); err != nil {
			t.Fatalf("补上来源之后该通过：%v", err)
		}
		for _, g := range []*Graph{root, child} {
			err := g.Add(NewNode("late", "", nil, nil, func(rc *RunCtx) error { return nil }))
			if !errors.Is(err, ErrGraphStarted) {
				t.Fatalf("%s：通过之后该封住，got %v", g.ID(), err)
			}
		}
	})
}

// `Validate()` 的两趟（判完整棵树 → 逐层封）必须在**同一个装配临界区**里：改定义的入口
// （`Add` / `DeclareInput` / `DeclareOutput` / `Sub`）与这里共用 `assemblyMu`，所以判过的
// 那棵树在封口那一刻不可能已经被改过。
//
// 少了这把锁会怎样（实测）：窗口里给挂着的子图加一个「要一条没人提供的输入」的节点，
// `Validate()` 返回 nil，却把**没校验过**的那份封上了——此后既改不了（坏节点留在里面），
// `Start()` 又重新报错。
//
// 判据是那条不变式：**`Validate()` 返回 nil ⇒ 刚封住的那棵树没有静态毛病**。
// 窗口靠 C2 那段大子图撑开：第一趟走到 C2 要花掉一整个校验的时间，而 C1 的锁在这段时间里
// 是空的，动刀几乎必然落进窗口（实测：改前第 0 轮就中；改后全部轮次干净）。
func TestValidateSealsOnlyWhatItChecked(t *testing.T) {
	const (
		trials = 10
		chain  = 3000
		gap    = 300 * time.Microsecond
	)
	for trial := 0; trial < trials; trial++ {
		// C1 是动刀的目标，C2 很大（第一趟在 C1 之后还要走很久）。
		c1 := mustNew(t, context.Background(), fmt.Sprintf("atomic.c1.%d", trial))
		k1 := NewKey[string](fmt.Sprintf("atomic.c1.k%d", trial))
		if err := c1.DeclareInput(NewPort(k1)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}

		c2 := mustNew(t, context.Background(), fmt.Sprintf("atomic.c2.%d", trial))
		head := NewKey[int](fmt.Sprintf("atomic.c2.k%d.0", trial))
		if err := c2.DeclareInput(NewPort(head)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		prev := head
		for i := 1; i <= chain; i++ {
			next := NewKey[int](fmt.Sprintf("atomic.c2.k%d.%d", trial, i))
			mustAdd(t, c2, NewNode(fmt.Sprintf("n%d", i), "",
				Requires(prev), Provides(next), func(rc *RunCtx) error { return nil }))
			prev = next
		}

		root := mustNew(t, context.Background(), fmt.Sprintf("atomic.root.%d", trial))
		pin1 := NewKey[string](fmt.Sprintf("atomic.p1.%d", trial))
		pin2 := NewKey[int](fmt.Sprintf("atomic.p2.%d", trial))
		if err := Sub(root, "s1", "", []SubBind{In(pin1, k1)}, c1); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		if err := Sub(root, "s2", "", []SubBind{In(pin2, head)}, c2); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		declareIn(t, root, NewPort(pin1))
		if err := seedOn(root, pin1, "v"); err != nil {
			t.Fatalf("Seed = %v", err)
		}
		declareIn(t, root, NewPort(pin2))
		if err := seedOn(root, pin2, 1); err != nil {
			t.Fatalf("Seed = %v", err)
		}

		// 动刀：给 C1 加一个「要一条没人提供的输入」的节点——加完 C1 就有静态毛病。
		bad := NewNode("bad", "", Requires(NewKey[string](fmt.Sprintf("atomic.missing.%d", trial))),
			nil, func(rc *RunCtx) error { return nil })
		attacked := make(chan error, 1)
		go func() {
			time.Sleep(gap)
			attacked <- c1.Add(bad)
		}()

		err := root.Validate()
		aerr := <-attacked // 先等动刀落地：下面那句读的是它的结果
		if err != nil {
			continue // 这轮动刀抢在了校验之前（判的时候子图已经有毛病）——窗口没被碰到
		}
		if aerr == nil {
			if probs := c1.Topology().Problems; len(probs) > 0 {
				t.Fatalf("第 %d 轮：Validate() 返回 nil，却把带毛病的子图封上了（%v）", trial, probs[0])
			}
		}
	}
}

func TestPlainValidationReuseKeepsEachRunsInputsAndSkip(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	g, err := New("plain-validation-reuse", "封口只复用静态事实")
	streamTestMust(t, err)
	input, optional, output := NewKey[int]("input"), NewKey[int]("optional"), NewKey[int]("output")
	streamTestMust(t, g.DeclareInput(NewPort(input), NewPort(optional)))
	streamTestMust(t, g.DeclareOutput(NewPort(output)))
	var ran atomic.Int32
	streamTestMust(t, g.Add(NewNode("double", "", Requires(input), Provides(output), func(rc *RunCtx) error {
		ran.Add(1)
		value, err := Get(rc, input)
		if err != nil {
			return err
		}
		return Set(rc, output, value*2)
	})))
	streamTestMust(t, g.Validate())
	if _, err := g.NewExecution(ctx, Inputs{}); err == nil || !strings.Contains(err.Error(), "has no value for this run") {
		t.Fatalf("sealed graph forgot the next run's required input: %v", err)
	}
	for i, value := range []int{7, 0, 11} {
		var in Inputs
		if i == 1 {
			streamTestMust(t, SkipSeed(&in, input))
		} else {
			streamTestMust(t, Seed(&in, input, value))
		}
		// 未被读的optional始终不必提供；普通图改变本轮额度仍合法。
		e, err := g.NewExecution(ctx, in, WithExecMaxRunning(i))
		streamTestMust(t, err)
		streamTestMust(t, e.Start())
		streamTestMust(t, e.Wait())
		got, state, err := ReadOutput(e, output)
		streamTestMust(t, err)
		if i == 1 {
			if state != SlotStateSkipped {
				t.Fatalf("normal skip became %s", state)
			}
		} else if state != SlotStateReady || got != value*2 {
			t.Fatalf("run %d: (%d,%s), want (%d,ready)", i, got, state, value*2)
		}
		streamTestMust(t, g.Validate())
	}
	if ran.Load() != 2 {
		t.Fatalf("body executions=%d, want 2", ran.Load())
	}
}

// 不能把“已封口”扩大成“任何图都不必重新判本轮额度”。
func TestSealedValidationStillChecksNativeQuota(t *testing.T) {
	for _, shape := range []string{"direct", "child", "nested-child"} {
		t.Run(shape, func(t *testing.T) {
			var g *Graph
			if shape == "direct" {
				g = streamTestGraph(t, "direct-sealed-stream", WithMaxRunning(4))
				source, left, right := NewKey[<-chan int]("source"), NewKey[<-chan int]("left"), NewKey[<-chan int]("right")
				streamTestProduce(t, g, "source", source, 2, 0)
				streamTestMust(t, Tee(g, "tee", "", source, Keys(left, right)))
				values := &streamTestValues{}
				streamTestConsume(t, g, "left", left, values, "left")
				streamTestConsume(t, g, "right", right, values, "right")
			} else {
				g, _ = streamTestBroadcastSub(t, 4, nil, shape == "nested-child", 0, 2)
			}
			streamTestMust(t, g.Validate())
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			e, err := g.NewExecution(ctx, Inputs{}, WithExecMaxRunning(3))
			streamTestMust(t, err)
			defer e.Cancel()
			if err := e.Start(); err == nil || !strings.Contains(err.Error(), "needs 4 slots") {
				t.Fatalf("sealed stream ignored this run's quota: %v", err)
			}
			if !errors.Is(e.Wait(), ErrGraphNotStarted) {
				t.Fatal("rejected execution was marked started")
			}
			streamTestMust(t, g.Run(ctx, Inputs{}, WithExecMaxRunning(4)))
		})
	}
}

func TestSealedValidationStillRequiresManagedHostReader(t *testing.T) {
	g := streamTestGraph(t, "sealed-host-output")
	out := NewKey[Stream[int]]("messages")
	var ran atomic.Bool
	streamTestMust(t, g.Add(NewNode("source", "", nil, Provides(out), func(*RunCtx) error {
		ran.Store(true)
		return nil
	})))
	streamTestMust(t, g.DeclareOutput(NewPort(out)))
	// 定义允许宿主稍后订阅；封口不是登记了本轮真实读者。
	streamTestMust(t, g.Validate())
	e, err := g.NewExecution(t.Context(), Inputs{})
	streamTestMust(t, err)
	defer e.Cancel()
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "nothing consumes it") {
		t.Fatalf("sealed definition invented a host reader: %v", err)
	}
	if ran.Load() || !errors.Is(e.Wait(), ErrGraphNotStarted) {
		t.Fatal("missing host reader reached business")
	}
}

func TestPlainValidationReuseStillChecksCheckpointInputs(t *testing.T) {
	g := streamTestGraph(t, "sealed-checkpoint-input")
	input := NewKey[any]("input")
	streamTestMust(t, g.DeclareInput(NewPort(input)))
	var ran atomic.Int32
	streamTestMust(t, g.Add(NewNode("read", "", Requires(input), nil, func(rc *RunCtx) error {
		ran.Add(1)
		_, err := Get(rc, input)
		return err
	})))
	streamTestMust(t, g.Validate())
	var in Inputs
	streamTestMust(t, Seed(&in, input, any(make(chan int))))
	probe := &checkpointProbe{}
	e, err := g.NewExecution(t.Context(), in, WithExecCheckpoint("root", probe.hooks()))
	streamTestMust(t, err)
	defer e.Cancel()
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "channel") {
		t.Fatalf("plain static reuse bypassed dynamic checkpoint checks: %v", err)
	}
	if ran.Load() != 0 || len(probe.opens) != 0 || !errors.Is(e.Wait(), ErrGraphNotStarted) {
		t.Fatal("rejected checkpoint input started attempts or business")
	}
	// 相同普通值在非checkpoint模式仍合法，不能把可靠存储限制扩大到本地执行。
	streamTestMust(t, g.Run(t.Context(), in))
	if ran.Load() != 1 {
		t.Fatalf("ordinary execution body count=%d, want 1", ran.Load())
	}
}

func TestRequiredInputsWithoutPortsDoesNotHideStaticErrors(t *testing.T) {
	g := streamTestGraph(t, "no-inlet-unsourced")
	missing := NewKey[int]("missing")
	streamTestMust(t, g.Add(NewNode("waiter", "", Requires(missing), nil, nil)))
	e, err := g.NewExecution(t.Context(), Inputs{})
	streamTestMust(t, err)
	defer e.Cancel()
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "nothing provides it") {
		t.Fatalf("no-inlet shortcut hid static missing source: %v", err)
	}
	if g.sealed || !errors.Is(e.Wait(), ErrGraphNotStarted) {
		t.Fatal("invalid graph was sealed or started")
	}
	streamTestMust(t, g.Add(NewNode("source", "", nil, Provides(missing), func(rc *RunCtx) error {
		return Set(rc, missing, 1)
	})))
	streamTestMust(t, g.Run(context.Background(), Inputs{}))
}
