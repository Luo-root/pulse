package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type readOutputResult[T any] struct {
	value T
	state SlotState
	err   error
}

func readOutputMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func readOutputContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func readOutputReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("operation did not finish within 2s")
		var zero T
		return zero
	}
}

func readOutputNow[T any](t *testing.T, e *Execution, k Key[T]) (T, SlotState, error) {
	t.Helper()
	ch := make(chan readOutputResult[T], 1)
	go func() {
		v, st, err := ReadOutput(e, k)
		ch <- readOutputResult[T]{v, st, err}
	}()
	r := readOutputReceive(t, ch)
	return r.value, r.state, r.err
}

func readOutputWait(t *testing.T, e *Execution) error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- e.Wait() }()
	return readOutputReceive(t, ch)
}

func readOutputWant[T any](t *testing.T, e *Execution, k Key[T], want T, state SlotState) {
	t.Helper()
	v, st, err := readOutputNow(t, e, k)
	if err != nil || st != state || !reflect.DeepEqual(v, want) {
		t.Fatalf("ReadOutput(%q) = (%#v, %q, %v), want (%#v, %q, nil)", k.Name(), v, st, err, want, state)
	}
}

func readOutputError[T any](t *testing.T, e *Execution, k Key[T], cause error) {
	t.Helper()
	v, st, err := readOutputNow(t, e, k)
	var zero T
	if err == nil || st != "" || !reflect.DeepEqual(v, zero) {
		t.Fatalf("ReadOutput(%q) = (%#v, %q, %v), want zero value, empty state and error", k.Name(), v, st, err)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("error %v does not preserve cause %v", err, cause)
	}
	if cause == nil && (errors.Is(err, ErrGraphNotStarted) || errors.Is(err, ErrUndeclared)) {
		t.Fatalf("contract/lifecycle error must be plain, got %v", err)
	}
}

func readOutputSlotStates(e *Execution) map[string]slotState {
	e.slotsMu.Lock()
	slots := make(map[string]*slot, len(e.slots))
	for name, s := range e.slots {
		slots[name] = s
	}
	e.slotsMu.Unlock()
	states := make(map[string]slotState, len(slots))
	for name, s := range slots {
		states[name], _ = s.snapshot()
	}
	return states
}

func TestReadOutputRunsIsolation(t *testing.T) {
	ctx := readOutputContext(t)
	input, output := NewKey[int]("input"), NewKey[int]("output")
	arrived, release := make(chan int, 2), make(chan struct{})
	g, err := New("read-output-runs", "")
	readOutputMust(t, err)
	readOutputMust(t, g.DeclareInput(NewPort(input)))
	readOutputMust(t, g.DeclareOutput(NewPort(output)))
	readOutputMust(t, g.Add(NewNode("double", "", Requires(input), Provides(output), func(rc *RunCtx) error {
		v, err := Get(rc, input)
		if err != nil {
			return err
		}
		if err := Set(rc, output, v*2); err != nil {
			return err
		}
		if v > 5 {
			arrived <- v
			select {
			case <-release:
			case <-rc.Context().Done():
				return rc.Context().Err()
			}
		}
		return nil
	})))

	var sequential []*Execution
	for _, v := range []int{2, 5} {
		var in Inputs
		readOutputMust(t, Seed(&in, input, v))
		e, err := g.NewExecution(ctx, in)
		readOutputMust(t, err)
		readOutputMust(t, e.Start())
		readOutputMust(t, readOutputWait(t, e))
		readOutputWant(t, e, output, v*2, SlotStateReady)
		sequential = append(sequential, e)
	}
	var concurrent []*Execution
	for _, v := range []int{7, 11} {
		var in Inputs
		readOutputMust(t, Seed(&in, input, v))
		e, err := g.NewExecution(ctx, in)
		readOutputMust(t, err)
		readOutputMust(t, e.Start())
		concurrent = append(concurrent, e)
	}
	// 两份执行都已写出结果、仍在体内会合，确定它们确实同时运行。
	readOutputReceive(t, arrived)
	readOutputReceive(t, arrived)
	close(release)
	waited := make(chan error, 2)
	for _, e := range concurrent {
		go func(e *Execution) { waited <- e.Wait() }(e)
	}
	for range concurrent {
		readOutputMust(t, readOutputReceive(t, waited))
	}
	for i, want := range []int{14, 22} {
		readOutputWant(t, concurrent[i], output, want, SlotStateReady)
	}
	for i, want := range []int{4, 10} {
		readOutputWant(t, sequential[i], output, want, SlotStateReady)
	}
}

func TestReadOutputCancelOneRun(t *testing.T) {
	ctx := readOutputContext(t)
	input, output, skipped := NewKey[int]("input"), NewKey[int]("output"), NewKey[int]("skipped")
	arrived, release := make(chan int, 2), make(chan struct{})
	cancelReturn := make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case cancelReturn <- struct{}{}:
		default:
		}
	})
	g, err := New("read-output-cancel", "")
	readOutputMust(t, err)
	readOutputMust(t, g.DeclareInput(NewPort(input)))
	readOutputMust(t, g.DeclareOutput(NewPort(output), NewPort(skipped)))
	readOutputMust(t, g.Add(NewNode("write-then-wait", "", Requires(input), Provides(output, skipped), func(rc *RunCtx) error {
		v, err := Get(rc, input)
		if err != nil {
			return err
		}
		if err := Set(rc, output, v); err != nil {
			return err
		}
		if err := Skip(rc, skipped); err != nil {
			return err
		}
		arrived <- v
		if v == 1 {
			<-rc.Context().Done()
			<-cancelReturn // 读数前不退出体：e.err 仍为空，必须识别真实 ctx 取消。
			return rc.Context().Err()
		}
		select {
		case <-release:
			return nil
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	})))
	var runs []*Execution
	for _, v := range []int{1, 2} {
		var in Inputs
		readOutputMust(t, Seed(&in, input, v))
		e, err := g.NewExecution(ctx, in)
		readOutputMust(t, err)
		readOutputMust(t, e.Start())
		runs = append(runs, e)
	}
	readOutputReceive(t, arrived)
	readOutputReceive(t, arrived)
	runs[0].Cancel()
	// 会合点在 Set / Skip 之后，取消不能抢先制造“本来就没有输出”的假证据。
	for _, k := range []Key[int]{output, skipped} {
		readOutputError(t, runs[0], k, context.Canceled)
	}
	if err := runs[0].Snapshot().Graph.Err; err != nil {
		t.Fatalf("reading cancellation changed the captured error: %v", err)
	}
	cancelReturn <- struct{}{}
	if err := readOutputWait(t, runs[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Wait = %v", err)
	}
	readOutputError(t, runs[0], output, context.Canceled)
	readOutputError(t, runs[0], skipped, context.Canceled)
	readOutputMust(t, runs[1].Err())
	close(release)
	readOutputMust(t, readOutputWait(t, runs[1]))
	readOutputWant(t, runs[1], output, 2, SlotStateReady)
	readOutputWant(t, runs[1], skipped, 0, SlotStateSkipped)
}

func TestReadOutputLifecycle(t *testing.T) {
	output := NewKey[int]("output")
	readOutputError(t, nil, output, nil)
	readOutputError(t, &Execution{}, output, nil)
	written, finished, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	g, err := New("read-output-lifecycle", "", WithObserver(ObserverFunc{
		Finished: func(Site, string, NodeFinishReason, error) { close(finished) },
	}))
	readOutputMust(t, err)
	readOutputMust(t, g.DeclareOutput(NewPort(output)))
	readOutputMust(t, g.Add(NewNode("write", "", nil, Provides(output), func(rc *RunCtx) error {
		if err := Set(rc, output, 23); err != nil {
			return err
		}
		close(written)
		select {
		case <-release:
			return nil
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	})))
	e, err := g.NewExecution(readOutputContext(t), Inputs{})
	readOutputMust(t, err)
	readOutputError(t, e, output, nil) // 尚未 Start。
	readOutputMust(t, e.Start())
	readOutputReceive(t, written)
	readOutputError(t, e, output, nil) // Set 已完成，但本轮仍在运行。
	readOutputMust(t, e.Err())
	if st := e.Snapshot().Nodes[0].Outputs[0].State; st != SlotStateReady {
		t.Fatalf("already written output = %q", st)
	}
	close(release)
	readOutputReceive(t, finished)
	if st := e.Snapshot().Nodes[0].Status; st != NodeStatusCompleted {
		t.Fatalf("finished node = %q", st)
	}
	readOutputError(t, e, output, nil) // 节点已结束，但尚未 Wait 收尾。
	readOutputMust(t, readOutputWait(t, e))
	if !errors.Is(e.Context().Err(), context.Canceled) {
		t.Fatal("Wait must have released the execution context")
	}
	readOutputWant(t, e, output, 23, SlotStateReady)
	readOutputMust(t, readOutputWait(t, e))
	readOutputWant(t, e, output, 23, SlotStateReady)
}

func TestReadOutputStates(t *testing.T) {
	zero, explicit, automatic := NewKey[int]("zero"), NewKey[int]("explicit"), NewKey[int]("automatic")
	noValue, allSkipped, optional := NewKey[int]("no-value"), NewKey[int]("all-skipped"), NewKey[int]("optional")
	empty := NewKey[string]("empty")
	g, err := New("read-output-states", "")
	readOutputMust(t, err)
	readOutputMust(t, g.DeclareInput(NewPort(optional)))
	readOutputMust(t, g.DeclareOutput(NewPort(zero), NewPort(explicit), NewPort(automatic),
		NewPort(noValue), NewPort(allSkipped), NewPort(optional), NewPort(empty)))
	for _, n := range []*Node{
		NewNode("source", "", nil, Provides(zero, explicit, automatic), func(rc *RunCtx) error {
			if err := Set(rc, zero, 0); err != nil {
				return err
			}
			return Skip(rc, explicit)
		}),
		NewNode("empty", "", nil, Provides(empty), func(rc *RunCtx) error { return Set(rc, empty, "") }),
		NewNode("no-value", "", nil, Provides(noValue), func(*RunCtx) error { return NoValue() }),
		NewNode("all-skipped", "", Requires(explicit), Provides(allSkipped), func(*RunCtx) error {
			return errors.New("all-skipped node must not enter Run")
		}),
	} {
		readOutputMust(t, g.Add(n))
	}
	e, err := g.NewExecution(readOutputContext(t), Inputs{})
	readOutputMust(t, err)
	readOutputMust(t, e.Start())
	readOutputMust(t, readOutputWait(t, e))
	readOutputWant(t, e, zero, 0, SlotStateReady)
	readOutputWant(t, e, empty, "", SlotStateReady)
	for _, k := range []Key[int]{explicit, automatic, noValue, allSkipped} {
		readOutputWant(t, e, k, 0, SlotStateSkipped)
	}
	readOutputWant(t, e, optional, 0, SlotStatePending)
}

func TestReadOutputNilAndReferences(t *testing.T) {
	nilInterface, typedNil := NewKey[any]("nil-interface"), NewKey[any]("typed-nil")
	nilPointer, pointer := NewKey[*int]("nil-pointer"), NewKey[*int]("pointer")
	slice, mapping := NewKey[[]int]("slice"), NewKey[map[string]int]("map")
	items, values, number := []int{3}, map[string]int{"n": 4}, 5
	g, err := New("read-output-values", "")
	readOutputMust(t, err)
	readOutputMust(t, g.DeclareOutput(NewPort(nilInterface), NewPort(typedNil), NewPort(nilPointer),
		NewPort(pointer), NewPort(slice), NewPort(mapping)))
	for _, n := range []*Node{
		NewNode("nil-interface", "", nil, Provides(nilInterface), func(rc *RunCtx) error { return Set(rc, nilInterface, nil) }),
		NewNode("typed-nil", "", nil, Provides(typedNil), func(rc *RunCtx) error { return Set(rc, typedNil, any((*int)(nil))) }),
		NewNode("nil-pointer", "", nil, Provides(nilPointer), func(rc *RunCtx) error { return Set(rc, nilPointer, nil) }),
		NewNode("pointer", "", nil, Provides(pointer), func(rc *RunCtx) error { return Set(rc, pointer, &number) }),
		NewNode("slice", "", nil, Provides(slice), func(rc *RunCtx) error { return Set(rc, slice, items) }),
		NewNode("map", "", nil, Provides(mapping), func(rc *RunCtx) error { return Set(rc, mapping, values) }),
	} {
		readOutputMust(t, g.Add(n))
	}
	e, err := g.NewExecution(readOutputContext(t), Inputs{})
	readOutputMust(t, err)
	readOutputMust(t, e.Start())
	readOutputMust(t, readOutputWait(t, e))
	readOutputWant(t, e, nilInterface, nil, SlotStateReady)
	readOutputWant(t, e, nilPointer, nil, SlotStateReady)
	readOutputWant(t, e, typedNil, any((*int)(nil)), SlotStateReady)
	gotSlice, _, err := readOutputNow(t, e, slice)
	readOutputMust(t, err)
	gotMap, _, err := readOutputNow(t, e, mapping)
	readOutputMust(t, err)
	gotPointer, _, err := readOutputNow(t, e, pointer)
	readOutputMust(t, err)
	if gotPointer != &number {
		t.Fatal("pointer was copied")
	}
	gotSlice[0], gotMap["n"], *gotPointer = 30, 40, 50
	if items[0] != 30 || values["n"] != 40 || number != 50 {
		t.Fatal("ReadOutput must return the original mutable values")
	}
	readOutputWant(t, e, slice, items, SlotStateReady)
	readOutputWant(t, e, mapping, values, SlotStateReady)
	readOutputWant(t, e, pointer, &number, SlotStateReady)
}

func TestReadOutputAccess(t *testing.T) {
	input, through := NewKey[int]("input-only"), NewKey[int]("through")
	internal, output, bridged := NewKey[int]("internal"), NewKey[int]("output"), NewKey[int]("bridged")
	childOutput := NewKey[int]("child-output")
	child, err := New("read-output-child", "")
	readOutputMust(t, err)
	readOutputMust(t, child.DeclareOutput(NewPort(childOutput)))
	readOutputMust(t, child.Add(NewNode("child-source", "", nil, Provides(childOutput), func(rc *RunCtx) error {
		return Set(rc, childOutput, 50)
	})))
	g, err := New("read-output-access", "")
	readOutputMust(t, err)
	readOutputMust(t, g.DeclareInput(NewPort(input), NewPort(through)))
	readOutputMust(t, g.DeclareOutput(NewPort(output), NewPort(bridged), NewPort(through)))
	readOutputMust(t, g.Add(NewNode("source", "", nil, Provides(internal, output), func(rc *RunCtx) error {
		if err := Set(rc, internal, 8675309); err != nil {
			return err
		}
		return Set(rc, output, 29)
	})))
	readOutputMust(t, Sub(g, "child", "", []SubBind{Out(childOutput, bridged)}, child))
	// 另一张图登记的同名同类型键仍等价，唯一键名则不授予本图出口权限。
	alias, otherOnly := NewKey[int](output.Name()), NewKey[int]("other-only")
	other, err := New("read-output-other", "")
	readOutputMust(t, err)
	readOutputMust(t, other.DeclareOutput(NewPort(alias), NewPort(otherOnly)))
	var in Inputs
	readOutputMust(t, Seed(&in, input, 10))
	readOutputMust(t, Seed(&in, through, 41))
	e, err := g.NewExecution(readOutputContext(t), in)
	readOutputMust(t, err)
	denied := []Key[int]{input, internal, childOutput, otherOnly, NewKey[int]("unknown"), {}}
	for _, k := range denied {
		readOutputError(t, e, k, ErrUndeclared) // 权限先于生命周期。
	}
	readOutputError(t, e, NewKey[string](output.Name()), ErrUndeclared)
	readOutputError(t, e, NewKey[any](output.Name()), ErrUndeclared)
	readOutputMust(t, e.Start())
	readOutputMust(t, readOutputWait(t, e))
	for _, k := range denied {
		readOutputError(t, e, k, ErrUndeclared)
	}
	readOutputError(t, e, NewKey[string](output.Name()), ErrUndeclared)
	readOutputError(t, e, NewKey[any](output.Name()), ErrUndeclared)
	readOutputWant(t, e, output, 29, SlotStateReady)
	readOutputWant(t, e, alias, 29, SlotStateReady)
	readOutputWant(t, e, bridged, 50, SlotStateReady)
	readOutputWant(t, e, through, 41, SlotStateReady)
}

func TestReadOutputFailures(t *testing.T) {
	for _, outer := range []bool{false, true} {
		t.Run(fmt.Sprintf("outer-aspect=%t", outer), func(t *testing.T) {
			output, skipped, pending := NewKey[int]("output"), NewKey[int]("skipped"), NewKey[int]("pending")
			cause, finished := errors.New("business failure"), make(chan struct{})
			g, err := New("read-output-failure", "", WithObserver(ObserverFunc{
				Finished: func(Site, string, NodeFinishReason, error) { close(finished) },
			}))
			readOutputMust(t, err)
			readOutputMust(t, g.DeclareInput(NewPort(pending)))
			readOutputMust(t, g.DeclareOutput(NewPort(output), NewPort(skipped), NewPort(pending)))
			var aspects []Aspect
			if outer {
				aspects = append(aspects, func(rc *RunCtx, next func(*RunCtx) error) error {
					if err := next(rc); err != nil {
						return err
					}
					return fmt.Errorf("outer aspect: %w", cause)
				})
			}
			readOutputMust(t, g.Add(NewNode("write-then-fail", "", nil, Provides(output, skipped), func(rc *RunCtx) error {
				if err := Set(rc, output, 12345); err != nil {
					return err
				}
				if err := Skip(rc, skipped); err != nil {
					return err
				}
				if !outer {
					return fmt.Errorf("node body: %w", cause)
				}
				return nil
			}, aspects...)))
			e, err := g.NewExecution(readOutputContext(t), Inputs{})
			readOutputMust(t, err)
			readOutputMust(t, e.Start())
			readOutputReceive(t, finished) // fail 已锁存，尚未 Wait 也应返回真实 cause。
			if st := e.Snapshot().Nodes[0].Outputs[0].State; st != SlotStateReady {
				t.Fatalf("published output = %q", st)
			}
			readOutputError(t, e, output, cause)
			if err := readOutputWait(t, e); !errors.Is(err, cause) {
				t.Fatalf("failed Wait = %v", err)
			}
			for _, k := range []Key[int]{output, skipped, pending} {
				readOutputError(t, e, k, cause)
			}
			readOutputError(t, e, NewKey[int]("unknown"), ErrUndeclared)
		})
	}
	t.Run("deadline", func(t *testing.T) {
		output := NewKey[int]("output")
		g, err := New("read-output-deadline", "")
		readOutputMust(t, err)
		readOutputMust(t, g.DeclareOutput(NewPort(output)))
		readOutputMust(t, g.Add(NewNode("source", "", nil, Provides(output), func(rc *RunCtx) error {
			return Set(rc, output, 9)
		})))
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		e, err := g.NewExecution(ctx, Inputs{})
		readOutputMust(t, err)
		readOutputError(t, e, output, context.DeadlineExceeded)
		readOutputMust(t, e.Start())
		if err := readOutputWait(t, e); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline Wait = %v", err)
		}
		readOutputError(t, e, output, context.DeadlineExceeded)
	})
}

func TestReadOutputFinishedObserverAndReadOnly(t *testing.T) {
	output := NewKey[int]("output")
	var e *Execution
	var events atomic.Int32
	observed := make(chan readOutputResult[int], 1)
	g, err := New("read-output-observer", "", WithObserver(ObserverFunc{
		GraphStarted: func(Site) { events.Add(1) },
		Waiting:      func(Site, string) { events.Add(1) },
		Running:      func(Site, string) { events.Add(1) },
		Finished:     func(Site, string, NodeFinishReason, error) { events.Add(1) },
		GraphFinished: func(_ Site, reason NodeFinishReason, graphErr error) {
			events.Add(1)
			v, st, err := ReadOutput(e, output)
			if reason != NodeCompleted || graphErr != nil {
				err = fmt.Errorf("unexpected finish: %s, %v", reason, graphErr)
			}
			observed <- readOutputResult[int]{v, st, err}
		},
	}))
	readOutputMust(t, err)
	readOutputMust(t, g.DeclareOutput(NewPort(output)))
	readOutputMust(t, g.Add(NewNode("source", "", nil, Provides(output), func(rc *RunCtx) error {
		return Set(rc, output, 41)
	})))
	e, err = g.NewExecution(readOutputContext(t), Inputs{})
	readOutputMust(t, err)
	readOutputMust(t, e.Start())
	readOutputMust(t, readOutputWait(t, e))
	r := readOutputReceive(t, observed)
	if r.value != 41 || r.state != SlotStateReady || r.err != nil {
		t.Fatalf("GraphFinished read = %+v", r)
	}
	before, snapshot := readOutputSlotStates(e), e.Snapshot()
	for range 3 {
		readOutputWant(t, e, output, 41, SlotStateReady)
	}
	readOutputError(t, e, NewKey[int]("unknown"), ErrUndeclared)
	if !reflect.DeepEqual(before, readOutputSlotStates(e)) || !reflect.DeepEqual(snapshot, e.Snapshot()) {
		t.Fatal("reading changed slot count, arrival states or execution snapshot")
	}
	if count := events.Load(); count != 5 {
		t.Fatalf("reading added lifecycle events: got %d, want 5", count)
	}
}

func TestReadOutputLateOptionalPort(t *testing.T) {
	early, late := NewKey[int]("early"), NewKey[int]("late")
	g, err := New("read-output-late-port", "")
	readOutputMust(t, err)
	readOutputMust(t, g.DeclareInput(NewPort(early)))
	readOutputMust(t, g.DeclareOutput(NewPort(early)))
	e, err := g.NewExecution(readOutputContext(t), Inputs{})
	readOutputMust(t, err)
	// 执行创建以后、Start 封口之前新增可选透传；本轮还没有它的槽位。
	readOutputMust(t, g.DeclareInput(NewPort(late)))
	readOutputMust(t, g.DeclareOutput(NewPort(late)))
	readOutputMust(t, e.Start())
	readOutputMust(t, readOutputWait(t, e))
	before := readOutputSlotStates(e)
	if len(before) != 1 || before[early.Name()] != slotPending {
		t.Fatalf("unexpected slots before reading: %v", before)
	}
	for range 2 {
		readOutputWant(t, e, early, 0, SlotStatePending)
		readOutputWant(t, e, late, 0, SlotStatePending)
	}
	if !reflect.DeepEqual(before, readOutputSlotStates(e)) {
		t.Fatal("reading pending outputs created or resolved a slot")
	}
}
