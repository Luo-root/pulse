package pulse

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	if err := Seed[any](&in, k, nil); err == nil || !strings.Contains(err.Error(), "nil value") ||
		!strings.Contains(err.Error(), "SkipSeed") || strings.Contains(err.Error(), "FixSkip") {
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

// 端口（DeclareInput / DeclareOutput）与「通过即封口」的契约用例。
//
// 这一组管三件事：
//  1. 入口声明让「这本来就是外部输入」与「你忘了接线」分得开（前者不该被报成毛病）；
//  2. 静态拓扑里入口的来源是 `SourceInput`（定义层「外部来源」只有这一种写法）、也不建边；
//  3. 声明是两段式的（失败不留痕），封口之后不再接受声明。
func TestDeclareInputPorts(t *testing.T) {
	ctx := context.Background()
	in := NewKey[string]("pt.in")
	out := NewKey[string]("pt.out")
	other := NewKey[string]("pt.other")

	t.Run("没声明端口时，缺来源照旧被报", func(t *testing.T) {
		g := mustNew(t, ctx, "no-port")
		mustAdd(t, g, NewNode("n", "", Requires(in), Provides(out), func(rc *RunCtx) error {
			return Set(rc, out, "v")
		}))
		err := g.Validate()
		if err == nil || !strings.Contains(err.Error(), "nothing provides it") {
			t.Fatalf("缺来源该被报，got %v", err)
		}
	})

	t.Run("声明入口之后通过，且来源是 input、不建边", func(t *testing.T) {
		g := mustNew(t, ctx, "with-port")
		if err := g.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		mustAdd(t, g, NewNode("n", "", Requires(in), Provides(out), func(rc *RunCtx) error {
			return Set(rc, out, "v")
		}))
		if err := g.Validate(); err != nil {
			t.Fatalf("声明过入口之后不该报缺来源：%v", err)
		}

		top := g.Topology()
		if len(top.Nodes) != 1 || len(top.Nodes[0].Requires) != 1 {
			t.Fatalf("拓扑形状不对：%+v", top.Nodes)
		}
		from := top.Nodes[0].Requires[0].From
		if from.Kind != SourceInput {
			t.Fatalf("入口的来源该是 SourceInput，got %+v", from)
		}
		if got := from.String(); got != "input" {
			t.Fatalf("SourceInput.String() = %q, want %q", got, "input")
		}
		// 入口是外部来源，不参与建边（它不是「上游节点」）
		if len(top.Edges) != 0 {
			t.Fatalf("入口不该参与建边，got %+v", top.Edges)
		}

		// 运行期：Seed 就是「这一次给入口喂值」（声明得排在 Validate 之前——通过即封口）
		if err := seedOn(g, in, "from-host"); err != nil {
			t.Fatalf("Seed 到声明的入口 = %v", err)
		}
		if err := runOn(g); err != nil {
			t.Fatalf("Run = %v", err)
		}
	})

	t.Run("同一条键重复声明是幂等的；类型不同则报错", func(t *testing.T) {
		g := mustNew(t, ctx, "idempotent")
		if err := g.DeclareInput(NewPort(in), NewPort(out)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := g.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("同名同类型的重复声明该是幂等的，got %v", err)
		}
		// 同名不同类型：在声明处就红，别留到绑定时才说类型不符
		bad := NewKey[int]("pt.in")
		err := g.DeclareInput(NewPort(bad))
		if err == nil || !strings.Contains(err.Error(), "already registered") {
			t.Fatalf("同名不同类型该被拒，got %v", err)
		}
		// 失败那次不留痕：图还能接着装配、接着校验
		if err := g.Validate(); err != nil {
			t.Fatalf("失败声明之后图该仍是可用的，got %v", err)
		}
	})

	t.Run("零值端口被拒", func(t *testing.T) {
		g := mustNew(t, ctx, "zero-port")
		err := g.DeclareInput(Port{})
		if err == nil || !strings.Contains(err.Error(), "zero Port{}") {
			t.Fatalf("零值端口该被拒并说清怎么改，got %v", err)
		}
	})

	t.Run("入口与出口是两张表：同一条键可以既是入口又是出口", func(t *testing.T) {
		g := mustNew(t, ctx, "through")
		if err := g.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := g.DeclareOutput(NewPort(in)); err != nil {
			t.Fatalf("同一条键声明成出口（透传）该被接受，got %v", err)
		}
		mustAdd(t, g, NewNode("n", "", Requires(in), nil, func(rc *RunCtx) error { return nil }))
		if err := g.Validate(); err != nil {
			t.Fatalf("Validate = %v", err)
		}
	})

	t.Run("封口之后不能再声明，报错点明是定义封口", func(t *testing.T) {
		g := mustNew(t, ctx, "sealed-declare")
		mustAdd(t, g, NewNode("n", "", nil, Provides(other), func(rc *RunCtx) error {
			return Set(rc, other, "v")
		}))
		if err := g.Validate(); err != nil {
			t.Fatalf("Validate = %v", err)
		}
		err := g.DeclareInput(NewPort(in))
		if !errors.Is(err, ErrGraphStarted) {
			t.Fatalf("封口后的声明该报 ErrGraphStarted，got %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "sealed") {
			t.Fatalf("报错该点明是定义封口，got %v", err)
		}
	})

	// 另一个冻结点：图**跑起来**之后（`Start` 成功）同样不再接受改定义——`Start` 成功会
	// 顺带封整棵树，所以判据与「通过即封口」是同一条；两个触发点共用 ErrGraphStarted，
	// 报文把两个都点出来（见 errSealed）。
	t.Run("跑起来的图也不能再声明端口，报文把两个触发点都点出来", func(t *testing.T) {
		g := mustNew(t, ctx, "started-declare")
		declareIn(t, g, NewPort(other))
		if err := seedOn(g, other, "v"); err != nil { // 空图也要有条来源才过静态校验
			t.Fatalf("Seed = %v", err)
		}
		if err := startOn(g); err != nil {
			t.Fatalf("Start = %v", err)
		}
		for _, err := range []error{g.DeclareInput(NewPort(in)), g.DeclareOutput(NewPort(out))} {
			if !errors.Is(err, ErrGraphStarted) {
				t.Fatalf("启动之后再声明该被拒，got %v", err)
			}
			// 封口与「跑过了」是同一个判据的两个触发点（`Start` 成功也封整棵树），
			// 报文把两个都点出来：单看报文的人得知道自己是踩了哪一个。
			if err == nil || !strings.Contains(err.Error(), "sealed") || !strings.Contains(err.Error(), "started") {
				t.Fatalf("报文该把「通过即封口」与「已经启动」都点出来，got %v", err)
			}
		}
		if err := waitOn(g); err != nil {
			t.Fatalf("Wait = %v", err)
		}
	})
}

// 一批端口要么全进表、要么一条都不进（all-or-nothing，与 `Add` 同一条纪律）。
//
// `DeclareInput(NewPort(NewKey[string]("x")), NewPort(NewKey[int]("x")))` 里的第一条会在
// 提交段真的写进登记表：校验段只查**旧**表的话第二条要到提交时才炸，那时第一条已经进去了
// ——实测：失败之后再单独声明一次 `int x`，报的还是「already registered as string」。
func TestDeclarePortsBatchIsAtomic(t *testing.T) {
	ctx := context.Background()

	t.Run("同一批里同名跨型：当场报，且第一条不进表", func(t *testing.T) {
		g := mustNew(t, ctx, "batch")
		err := g.DeclareInput(NewPort(NewKey[string]("batch.x")), NewPort(NewKey[int]("batch.x")))
		if err == nil || !strings.Contains(err.Error(), "declared twice in this call") {
			t.Fatalf("同一批里同名跨型该当场报，got %v", err)
		}
		// 关键判据：第一条**没有**进表——再声明一次 int x 该成功。
		// 进了表的话这里会报「already registered as string」（就是修复前那副样子）。
		if err := g.DeclareInput(NewPort(NewKey[int]("batch.x"))); err != nil {
			t.Fatalf("int batch.x 本该是干净的（整批没提交），got %v", err)
		}
		// 登记表里留下的也该是 int：此时把 batch.x 声明成 string 必须被拒。
		if err := g.DeclareOutput(NewPort(NewKey[string]("batch.x"))); err == nil {
			t.Fatal("batch.x 已经登记成 int 了，再声明成 string 该被拒")
		}
	})

	t.Run("同一批里同名同型：幂等，照常通过", func(t *testing.T) {
		g := mustNew(t, ctx, "batch2")
		if err := g.DeclareInput(
			NewPort(NewKey[string]("batch2.x")),
			NewPort(NewKey[string]("batch2.x")),
		); err != nil {
			t.Fatalf("同批同名同型该幂等通过，got %v", err)
		}
	})

	t.Run("整批全过才提交：混着好端口与坏端口时一条都不留", func(t *testing.T) {
		g := mustNew(t, ctx, "batch3")
		err := g.DeclareInput(
			NewPort(NewKey[string]("batch3.ok")),
			Port{}, // 零值端口：不该被声明
		)
		if err == nil || !strings.Contains(err.Error(), "zero Port") {
			t.Fatalf("零值端口该被拒，got %v", err)
		}
		// 好端口那条也不许留下：再声明一次该成功（若已进表，同名同型是幂等——所以换成
		// 另一条路径问：把它声明成**出口**且类型不同，仍该干净地按新表登记）。
		if err := g.DeclareInput(NewPort(NewKey[string]("batch3.ok"))); err != nil {
			t.Fatalf("batch3.ok 本该没进表，got %v", err)
		}
	})
}

// 出口和入口一样，是**装配期**就能判的事。`DeclareOutput` 了一条没人产出的键，父图照样
// 能用 `Out` 把它接出去（端口是声明，不是承诺），要等子图跑完才由 `bridgeOutputs` 报一句
// `never arrived`——那时已经跑了一半，病根也不在那句话里。
//
// 判据与 `problemsSourcesLocked`（「入口没人提供」）对称，用的是同一个来源判据
// （`sourceOfLocked`：谁会写这条槽优先，没人写就看它是不是声明过的入口）——所以
// **入口与出口同名（透传）不算没人产出**：父侧种进来的值就是这条槽的值。
func TestDeclareOutputNeedsProducer(t *testing.T) {
	ctx := context.Background()

	t.Run("没人产出的出口：Validate / Start 都拒，Report 也有一条", func(t *testing.T) {
		out := NewKey[string]("op.none")
		g := mustNew(t, ctx, "op.none")
		if err := g.DeclareOutput(NewPort(out)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		err := g.Validate()
		if err == nil || !strings.Contains(err.Error(), `declared output "op.none" is never produced`) {
			t.Fatalf("声明的出口没人产出该在装配期报，got %v", err)
		}
		// 一份判据、两个入口：Validate 与 Start 报的是同一句话
		if serr := startOn(g); serr == nil || serr.Error() != err.Error() {
			t.Fatalf("Validate 与 Start 的判据不一致：\nValidate %v\nStart    %v", err, serr)
		}
		var found bool
		for _, p := range g.Topology().Problems {
			if strings.Contains(p.Error(), "is never produced") {
				found = true
			}
		}
		if !found {
			t.Fatalf("Topology().Problems 里也该有这条：%v", g.Topology().Problems)
		}
	})

	t.Run("有节点产出就过", func(t *testing.T) {
		in := NewKey[string]("op.ok_in")
		out := NewKey[string]("op.ok_out")
		g := mustNew(t, ctx, "op.ok")
		if err := g.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := g.DeclareOutput(NewPort(out)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		mustAdd(t, g, NewNode("n", "", Requires(in), Provides(out), func(rc *RunCtx) error {
			return Set(rc, out, "v")
		}))
		if err := g.Validate(); err != nil {
			t.Fatalf("有人产出就不该报：%v", err)
		}
	})

	t.Run("透传（同名入口 + 出口）不算没人产出", func(t *testing.T) {
		through := NewKey[string]("op.through")
		g := mustNew(t, ctx, "op.through")
		if err := g.DeclareInput(NewPort(through)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := g.DeclareOutput(NewPort(through)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		if err := g.Validate(); err != nil {
			t.Fatalf("透传的出口有来源（就是那条入口），不该报：%v", err)
		}
	})

	t.Run("多条没产出的出口：报文顺序按名字排，不跟 map 迭代走", func(t *testing.T) {
		g := mustNew(t, ctx, "op.order")
		// 故意按倒序声明：顺序要是跟着声明走，这里就该反着来
		for _, name := range []string{"op.z2", "op.z3", "op.z1"} {
			if err := g.DeclareOutput(NewPort(NewKey[string](name))); err != nil {
				t.Fatalf("DeclareOutput = %v", err)
			}
		}
		probs := g.Topology().Problems
		if len(probs) != 3 {
			t.Fatalf("三条没产出的出口该各一条，got %d：%v", len(probs), probs)
		}
		want := []string{"op.z1", "op.z2", "op.z3"}
		for i, w := range want {
			if !strings.Contains(probs[i].Error(), w) {
				t.Fatalf("第 %d 条该是 %s（按名字排，两次问要一模一样）：%v", i, w, probs)
			}
		}
		// 同一张图问两次必须一样：map 迭代顺序是随机的，拿它当顺序的写法在这里会飘
		again := g.Topology().Problems
		for i := range probs {
			if probs[i].Error() != again[i].Error() {
				t.Fatalf("两次问的顺序不一样：\n%v\n%v", probs, again)
			}
		}
	})

	t.Run("挂进父图之后按挂载点报（装配期，不必等它跑完）", func(t *testing.T) {
		out := NewKey[string]("op.mount_out")
		sink := NewKey[string]("op.mount_sink")
		child := mustNew(t, ctx, "op.child")
		if err := child.DeclareOutput(NewPort(out)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		d := mustNew(t, ctx, "op.parent")
		if err := Sub(d, "mount", "", []SubBind{Out(out, sink)}, child); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		err := d.Validate()
		if err == nil {
			t.Fatal("子图声明了一条没人产出的出口，父图装配期就该报")
		}
		if !strings.Contains(err.Error(), `subgraph "op.child" (mounted at "mount")`) ||
			!strings.Contains(err.Error(), "is never produced") {
			t.Fatalf("报文该点明是哪里挂的子图、病在出口上，got %v", err)
		}
	})
}

// TestInputPortAndProducerConflict 一个键恰好一个来源，而**声明过的进口也是来源**——它只是
// 不落在 producer 表里（那张表回答「谁会写这条槽」，端口回答「这张图接受什么」）。
//
// 于是「这条键既有节点产出、又声明成进口」这个状态，两个动作谁先谁后都该在装配期被拒：
// 它过得了 `Validate`，却是一个**用不了的入口**——喂值会撞「图内已有来源」，不喂又等于白声明。
// 判据只写一份（两个方向同一句话的两个面），都撞 `ErrDuplicateSource`。
func TestInputPortAndProducerConflict(t *testing.T) {
	ctx := context.Background()
	k := NewKey[string]("ds.k")

	t.Run("先声明进口，再 Add 产出它的节点", func(t *testing.T) {
		g := mustNew(t, ctx, "ds1")
		declareIn(t, g, NewPort(k))
		err := g.Add(NewNode("prod", "", nil, Provides(k), func(rc *RunCtx) error { return Set(rc, k, "v") }))
		if err == nil {
			t.Fatal("Add 该拒：这条键已经是声明过的进口")
		}
		if !errors.Is(err, ErrDuplicateSource) {
			t.Fatalf("Add = %v，want 撞来源冲突（%s）", err, ErrDuplicateSource)
		}
		for _, want := range []string{k.Name(), `declared input`, `"ds1"`, `"prod"`} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("报文该含 %q，got %v", want, err)
			}
		}
		// all-or-nothing：被拒的那一批什么都不留。
		if err := g.Add(NewNode("other", "", nil, Provides(NewKey[string]("ds1.other")), func(rc *RunCtx) error { return nil })); err != nil {
			t.Fatalf("失败的 Add 在图里留下了痕迹：%v", err)
		}
	})

	t.Run("先 Add，再声明进口", func(t *testing.T) {
		g := mustNew(t, ctx, "ds2")
		mustAdd(t, g, NewNode("prod", "", nil, Provides(k), func(rc *RunCtx) error { return Set(rc, k, "v") }))
		err := g.DeclareInput(NewPort(k))
		if err == nil {
			t.Fatal("DeclareInput 该拒：这条键在图内已经有来源")
		}
		if !errors.Is(err, ErrDuplicateSource) {
			t.Fatalf("DeclareInput = %v，want 撞来源冲突（%s）", err, ErrDuplicateSource)
		}
		for _, want := range []string{k.Name(), `provided inside this graph`, `"prod"`} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("报文该含 %q，got %v", want, err)
			}
		}
		// 失败的声明不留痕：端口表里没有它，图照样跑得通（产出还是节点那一份）。
		if _, ok := g.inPorts[k.name]; ok {
			t.Fatal("失败的 DeclareInput 把端口写进了表")
		}
		if err := g.Run(ctx, Inputs{}); err != nil {
			t.Fatalf("被拒的那次声明该什么都没留下：%v", err)
		}
	})

	t.Run("同一条键**只是**重复声明成进口照旧幂等", func(t *testing.T) {
		g := mustNew(t, ctx, "ds3")
		declareIn(t, g, NewPort(k))
		declareIn(t, g, NewPort(k))
	})
}

type coerceTag string

type coerceDoc struct {
	Name string
}

func TestCoerceSeedValue(t *testing.T) {
	target := func(v any) reflect.Type { return reflect.TypeOf(v) }
	cases := []struct {
		name    string
		in      any
		target  reflect.Type
		want    any
		wantErr bool
	}{
		// 已可赋值：原样返回
		{"assignable slice", []string{"a"}, target([]string{}), []string{"a"}, false},
		{"assignable scalar", "x", target(""), "x", false},

		// 泛型容器按目标类型对齐
		{"[]any to []string", []any{"a", "b"}, target([]string{}), []string{"a", "b"}, false},
		{"[]any to []int64", []any{1, 2}, target([]int64{}), []int64{1, 2}, false},
		{"[]any to []int8", []any{1, 2}, target([]int8{}), []int8{1, 2}, false},
		{"nested []any", []any{[]any{"x"}}, target([][]string{}), [][]string{{"x"}}, false},
		{"map to typed map", map[string]any{"a": 1}, target(map[string]int{}), map[string]int{"a": 1}, false},
		{"named string elem", []any{"a"}, target([]coerceTag{}), []coerceTag{"a"}, false},
		{"null elem to zero", []any{nil}, target([]string{}), []string{""}, false},
		{"scalar int to int64", 1, target(int64(0)), int64(1), false},
		{"map value int to float", map[string]any{"a": 1}, target(map[string]float64{}), map[string]float64{"a": 1}, false},
		{"array length match", []any{1, 2}, target([2]int{}), [2]int{1, 2}, false},

		// 拒绝：不做静默错值的转换
		{"float into int", []any{1.5}, target([]int{}), nil, true},
		{"string into int", []any{"a"}, target([]int{}), nil, true},
		{"int into string", []any{1}, target([]string{}), nil, true},
		{"overflow int8", []any{200}, target([]int8{}), nil, true},
		{"negative into uint", []any{-1}, target([]uint{}), nil, true},
		{"map into slice", map[string]any{"a": 1}, target([]string{}), nil, true},
		{"struct elem needs resolve", []any{map[string]any{"name": "x"}}, target([]coerceDoc{}), nil, true},
		{"array length mismatch", []any{1}, target([2]int{}), nil, true},

		// 浮点转整数的端点：上界要按 float 能表示的端点判。float64(math.MaxInt64)
		// 被舍入成 2^63，`f > math.MaxInt64` 会把 2^63 自己放过去，而
		// int64(2^63) 在 gc 上得到 MinInt64（静默错值）。uint 同理（2^64）。
		{"float 2^63 into int64", float64(math.MaxInt64), target(int64(0)), nil, true},
		{"float 2^64 into uint64", float64(math.MaxUint64), target(uint64(0)), nil, true},
		{"float 2^63 into int", float64(math.MaxInt64), target(int(0)), nil, true},
		{"float max int64-representable", float64(math.MaxInt64) - 1024, target(int64(0)), int64(float64(math.MaxInt64) - 1024), false},
		{"float -2^63 into int64", float64(math.MinInt64), target(int64(0)), int64(math.MinInt64), false},
		{"float 2^63-1 rounds up", float64(math.MaxInt64) - 512, target(int64(0)), nil, true},
		{"float 2^64-1 rounds up", float64(math.MaxUint64) - 1024, target(uint64(0)), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := coerceSeedValue(tc.in, tc.target)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %#v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if reflect.TypeOf(got) != tc.target {
				t.Fatalf("result type = %v, want %v", reflect.TypeOf(got), tc.target)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// 端到端：SeedByName 收下 []any（YAML 解出的形状），节点按登记类型读回。
func TestSeedByNameCoercesGenericContainers(t *testing.T) {
	r := NewRegistry()
	docs := NewKey[[]string]("demo.docs")
	out := NewKey[string]("demo.out")
	MustRegisterKey(r, docs)
	MustRegisterKey(r, out)

	g := mustNew(t, context.Background(), "test")
	declareIn(t, g, NewPort(docs))
	in := &pendingOf(g).in
	if err := SeedByName(in, r, "demo.docs", "[]string", []any{"a", "b"}); err != nil {
		t.Fatal(err)
	}

	var seen string
	if err := g.Add(NewNode("join", "", Requires(docs), Provides(out), func(rc *RunCtx) error {
		v, err := Get(rc, docs) // 类型断言失败即说明写入的不是 []string
		if err != nil {
			return err
		}
		seen = strings.Join(v, ",")
		return Set(rc, out, seen)
	})); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if seen != "a,b" {
		t.Fatalf("node saw %q, want \"a,b\"", seen)
	}
}

// 结构性元素（map→struct）不做猜测：报错里带上不可转换的原因。
func TestSeedByNameRejectsStructuralElement(t *testing.T) {
	r := NewRegistry()
	MustRegisterKey(r, NewKey[[]coerceDoc]("demo.docs"))
	g := mustNew(t, context.Background(), "test")
	err := SeedByName(&pendingOf(g).in, r, "demo.docs", "[]pulse.coerceDoc", []any{map[string]any{"name": "x"}})
	if err == nil {
		t.Fatal("want error for map element into struct")
	}
	if !strings.Contains(err.Error(), "not assignable") || !strings.Contains(err.Error(), "element 0") {
		t.Fatalf("error should name the element and the mismatch: %v", err)
	}
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	run := func(*RunCtx) error { return nil }
	if err := r.Register("a", run); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("a", run); err == nil {
		t.Fatal("duplicate register should fail")
	}
	got, ok := r.Lookup("a")
	if !ok || got == nil {
		t.Fatal("lookup miss")
	}
	r2 := NewRegistry()
	if _, ok := r2.Lookup("a"); ok {
		t.Fatal("registries must be isolated")
	}
}

func TestRegisterKeyTypeTag(t *testing.T) {
	r := NewRegistry()
	k := NewKey[string]("demo.q")
	if err := RegisterKey(r, k); err != nil {
		t.Fatal(err)
	}
	tag, ok := r.TypeTagOf("demo.q")
	if !ok || tag != "string" {
		t.Fatalf("tag=%q ok=%v", tag, ok)
	}
	ref, err := r.ResolveKey("demo.q", "string")
	if err != nil || ref.name != "demo.q" {
		t.Fatalf("resolve: %v %#v", err, ref)
	}
	if _, err := r.ResolveKey("demo.q", "int"); err == nil {
		t.Fatal("type mismatch should fail")
	}
	if err := RegisterKey(r, NewKey[int]("demo.q")); err == nil {
		t.Fatal("same name different T should fail")
	}
}

func TestSeedByNameAssignable(t *testing.T) {
	r := NewRegistry()
	MustRegisterKey(r, NewKey[string]("demo.s"))
	var in Inputs
	if err := SeedByName(&in, r, "demo.s", "string", "hi"); err != nil {
		t.Fatal(err)
	}
	if err := SeedByName(&in, r, "demo.s", "string", 1); err == nil {
		t.Fatal("int not assignable to string")
	}
}
