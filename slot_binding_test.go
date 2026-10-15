package pulse

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// 每次 Done 被 wait 的 select 求值，就是该等待者确实走到某个 pending 来源的会合点。
type bindingWaitContext struct {
	context.Context
	entered chan struct{}
}

func (c bindingWaitContext) Done() <-chan struct{} {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestSlotBindingMovesExistingWaiterWithoutInventingArrival(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	watched := bindingWaitContext{Context: ctx, entered: make(chan struct{}, 8)}
	original, middle, source := newSlot(), newSlot(), newSlot()
	type result struct {
		value any
		err   error
	}
	resultCh := make(chan result, 1)
	go func() { value, err := original.wait(watched); resultCh <- result{value, err} }()
	entered := func() {
		t.Helper()
		select {
		case <-watched.entered:
		case early := <-resultCh:
			t.Fatalf("binding was mistaken for arrival: value=%v err=%v", early.value, early.err)
		case <-ctx.Done():
			t.Fatalf("waiter did not reach next pending source: %v", ctx.Err())
		}
	}
	entered()
	if err := original.bindTo(middle); err != nil {
		t.Fatal(err)
	}
	entered()
	if err := middle.bindTo(source); err != nil {
		t.Fatal(err)
	}
	entered()
	if state, value := original.snapshot(); state != slotPending || value != nil {
		t.Fatalf("binding changed arrival: state=%v value=%v", state, value)
	}
	if err := source.resolveValue(42); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resultCh:
		if got.err != nil || got.value != 42 {
			t.Fatalf("got %+v", got)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestSlotBindingSharesValueSkipAndCancellation(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready-nil", true: "normal-skip"}[skip], func(t *testing.T) {
			alias, source := newSlot(), newSlot()
			if err := alias.bindTo(source); err != nil {
				t.Fatal(err)
			}
			var err error
			if skip {
				err = source.resolveSkip()
			} else {
				err = source.resolveValue(nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			value, err := alias.wait(context.Background())
			if skip {
				if !errors.Is(err, ErrSkipped) {
					t.Fatalf("skip err=%v", err)
				}
			} else if err != nil || value != nil || !alias.isReady() {
				t.Fatalf("nil is a ready value: value=%v err=%v ready=%v", value, err, alias.isReady())
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := alias.wait(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled read=%v", err)
			}
		})
	}
}

func TestSlotBindingRejectsCyclesRebindAndArrivedTargets(t *testing.T) {
	a, b, c := newSlot(), newSlot(), newSlot()
	if err := a.bindTo(b); err != nil {
		t.Fatal(err)
	}
	if err := a.bindTo(b); err != nil {
		t.Fatalf("same binding should be idempotent: %v", err)
	}
	if err := b.bindTo(a); !errors.Is(err, ErrConflict) {
		t.Fatalf("cycle=%v", err)
	}
	if err := a.bindTo(c); !errors.Is(err, ErrConflict) {
		t.Fatalf("rebind=%v", err)
	}
	if err := c.resolveValue(7); err != nil {
		t.Fatal(err)
	}
	if err := c.bindTo(b); !errors.Is(err, ErrConflict) {
		t.Fatalf("arrived destination rebound=%v", err)
	}
	if state, value := b.snapshot(); state != slotPending || value != nil {
		t.Fatalf("failed binding modified source: %v %v", state, value)
	}
	if err := a.resolveValue(9); err != nil {
		t.Fatal(err)
	}
	if state, value := b.snapshot(); state != slotReady || value != 9 {
		t.Fatalf("write did not share source: %v %v", state, value)
	}
	if err := a.resolveSkip(); !errors.Is(err, ErrConflict) {
		t.Fatalf("opposite arrival=%v", err)
	}
}

func TestConcurrentSlotBindingsCannotInstallOppositeCycle(t *testing.T) {
	for i := 0; i < 64; i++ {
		a, b := newSlot(), newSlot()
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; results <- a.bindTo(b) }()
		go func() { defer wg.Done(); <-start; results <- b.bindTo(a) }()
		close(start)
		wg.Wait()
		first, second := <-results, <-results
		if (first == nil) == (second == nil) {
			t.Fatalf("opposite bindings: %v / %v", first, second)
		}
		if first != nil && !errors.Is(first, ErrConflict) {
			t.Fatal(first)
		}
		if second != nil && !errors.Is(second, ErrConflict) {
			t.Fatal(second)
		}
	}
}
