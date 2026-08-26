package flow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

var (
	kA = NewKey[string]("a")
	kB = NewKey[string]("b")
	kC = NewKey[string]("c")
	kL = NewKey[string]("left")
	kR = NewKey[string]("right")
)

func TestLinear(t *testing.T) {
	g := New(context.Background())
	mustAdd(t, g, NewNode("n1", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "hello")
	}))
	mustAdd(t, g, NewNode("n2", Requires(kA), Provides(kB), func(rc *RunCtx) error {
		v, err := Get(rc, kA)
		if err != nil {
			return err
		}
		// 二次 SetOnce 忽略
		if err := Set(rc, kB, v+"!"); err != nil {
			return err
		}
		return Set(rc, kB, "ignored")
	}))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	rc := inspect(g)
	v, ok, skipped, err := TryGet(rc, kB)
	if err != nil || !ok || skipped || v != "hello!" {
		t.Fatalf("got %q ok=%v skip=%v err=%v", v, ok, skipped, err)
	}
}

func TestFanIn(t *testing.T) {
	g := New(context.Background())
	var order atomic.Int32
	mustAdd(t, g, NewNode("a", nil, Provides(kA), func(rc *RunCtx) error {
		order.Add(1)
		return Set(rc, kA, "A")
	}))
	mustAdd(t, g, NewNode("b", nil, Provides(kB), func(rc *RunCtx) error {
		order.Add(1)
		return Set(rc, kB, "B")
	}))
	mustAdd(t, g, NewNode("c", Deps(Requires(kA), Requires(kB)), Provides(kC), func(rc *RunCtx) error {
		if order.Load() != 2 {
			t.Fatalf("fan-in ran before both parents, order=%d", order.Load())
		}
		a, _ := Get(rc, kA)
		b, _ := Get(rc, kB)
		return Set(rc, kC, a+b)
	}))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	rc := inspect(g)
	v, ok, _, _ := TryGet(rc, kC)
	if !ok || v != "AB" {
		t.Fatalf("fan-in result = %q", v)
	}
}

func TestBranchSkip(t *testing.T) {
	g := New(context.Background())
	var leftRan, rightRan atomic.Bool
	mustAdd(t, g, NewNode("split", nil, Deps(Provides(kL), Provides(kR)), func(rc *RunCtx) error {
		if err := Set(rc, kL, "go-left"); err != nil {
			return err
		}
		return Skip(rc, kR)
	}))
	mustAdd(t, g, NewNode("left", Requires(kL), Provides(kA), func(rc *RunCtx) error {
		leftRan.Store(true)
		v, err := Get(rc, kL)
		if err != nil {
			return err
		}
		return Set(rc, kA, v)
	}))
	mustAdd(t, g, NewNode("right", Requires(kR), Provides(kB), func(rc *RunCtx) error {
		rightRan.Store(true)
		return Set(rc, kB, "should-not")
	}))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if !leftRan.Load() || rightRan.Load() {
		t.Fatalf("left=%v right=%v", leftRan.Load(), rightRan.Load())
	}
	rc := inspect(g)
	if _, ok, skipped, _ := TryGet(rc, kB); ok || !skipped {
		t.Fatal("right output should be skipped")
	}
}

func TestCascadeSkip(t *testing.T) {
	g := New(context.Background())
	var bRan, cRan atomic.Bool
	mustAdd(t, g, NewNode("a", nil, Provides(kA), func(rc *RunCtx) error {
		return Skip(rc, kA)
	}))
	mustAdd(t, g, NewNode("b", Requires(kA), Provides(kB), func(rc *RunCtx) error {
		bRan.Store(true)
		return Set(rc, kB, "x")
	}))
	mustAdd(t, g, NewNode("c", Requires(kB), Provides(kC), func(rc *RunCtx) error {
		cRan.Store(true)
		return Set(rc, kC, "y")
	}))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if bRan.Load() || cRan.Load() {
		t.Fatal("cascade should not run B or C")
	}
}

func TestDuplicateProviderRejected(t *testing.T) {
	g := New(context.Background())
	mustAdd(t, g, NewNode("a", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "1")
	}))
	err := g.Add(NewNode("b", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "2")
	}))
	if err == nil {
		t.Fatal("expected duplicate provider error")
	}
}

func TestSelfEdgeRejected(t *testing.T) {
	g := New(context.Background())
	err := g.Add(NewNode("loop", Requires(kA), Provides(kA), func(rc *RunCtx) error {
		return nil
	}))
	if err == nil {
		t.Fatal("expected self-edge error")
	}
}

func TestSeedAndNodeProviderConflict(t *testing.T) {
	g := New(context.Background())
	if err := Seed(g, kA, "in"); err != nil {
		t.Fatal(err)
	}
	err := g.Add(NewNode("load", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "from-node")
	}))
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("Seed then Add: want ErrDuplicateSource, got %v", err)
	}

	g2 := New(context.Background())
	mustAdd(t, g2, NewNode("load", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "from-node")
	}))
	err = Seed(g2, kA, "in")
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("Add then Seed: want ErrDuplicateSource, got %v", err)
	}
}

func TestSkipSeedAndNodeProviderConflict(t *testing.T) {
	g := New(context.Background())
	if err := SkipSeed(g, kA); err != nil {
		t.Fatal(err)
	}
	err := g.Add(NewNode("load", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "from-node")
	}))
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("SkipSeed then Add: want ErrDuplicateSource, got %v", err)
	}

	g2 := New(context.Background())
	mustAdd(t, g2, NewNode("load", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "from-node")
	}))
	err = SkipSeed(g2, kA)
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("Add then SkipSeed: want ErrDuplicateSource, got %v", err)
	}
}

func TestEmptyNodeIDRejected(t *testing.T) {
	g := New(context.Background())
	err := g.Add(NewNode("", nil, Provides(kA), func(rc *RunCtx) error { return nil }))
	if err == nil {
		t.Fatal("expected empty id error")
	}
}

func TestDuplicateRequiresRejected(t *testing.T) {
	g := New(context.Background())
	err := g.Add(NewNode("n", Deps(Requires(kA), Requires(kA)), Provides(kB), func(rc *RunCtx) error { return nil }))
	if err == nil {
		t.Fatal("expected duplicate requires error")
	}
}

func TestDuplicateProvidesRejected(t *testing.T) {
	g := New(context.Background())
	err := g.Add(NewNode("n", nil, Deps(Provides(kA), Provides(kA)), func(rc *RunCtx) error { return nil }))
	if err == nil {
		t.Fatal("expected duplicate provides error")
	}
}

func TestDuplicateNodeIDRejected(t *testing.T) {
	g := New(context.Background())
	mustAdd(t, g, NewNode("n", nil, Provides(kA), func(rc *RunCtx) error { return Skip(rc, kA) }))
	err := g.Add(NewNode("n", nil, Provides(kB), func(rc *RunCtx) error { return Skip(rc, kB) }))
	if err == nil {
		t.Fatal("expected duplicate id error")
	}
}

func TestNodeErrorCancels(t *testing.T) {
	g := New(context.Background())
	boom := errors.New("boom")
	var bRan atomic.Bool
	mustAdd(t, g, NewNode("a", nil, Provides(kA), func(rc *RunCtx) error {
		return boom
	}))
	mustAdd(t, g, NewNode("b", Requires(kA), Provides(kB), func(rc *RunCtx) error {
		bRan.Store(true)
		return Set(rc, kB, "x")
	}))
	err := g.Run()
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	if bRan.Load() {
		t.Fatal("downstream should not run after failure")
	}
}

func TestTimeoutInterruptsWait(t *testing.T) {
	g := New(context.Background())
	mustAdd(t, g, NewNode("slow", Requires(kA), Provides(kB), func(rc *RunCtx) error {
		_, err := Get(rc, kA)
		return err
	}, Timeout(30*time.Millisecond)))
	// 永不 Seed kA
	err := g.Run()
	if err == nil {
		t.Fatal("expected timeout")
	}
}

func TestRecoveryPanic(t *testing.T) {
	g := New(context.Background())
	mustAdd(t, g, NewNode("p", nil, Provides(kA), func(rc *RunCtx) error {
		panic("explode")
	}))
	err := g.Run()
	if err == nil || err.Error() == "" {
		t.Fatalf("want panic error, got %v", err)
	}
}

func TestMaxRunningSerializes(t *testing.T) {
	g := New(context.Background(), WithMaxRunning(1))
	var current, max atomic.Int32
	run := func(rc *RunCtx) error {
		n := current.Add(1)
		for {
			old := max.Load()
			if n <= old || max.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		current.Add(-1)
		// 无 Provides：漏写自动 skip，无所谓
		return nil
	}
	mustAdd(t, g, NewNode("x", nil, nil, run))
	mustAdd(t, g, NewNode("y", nil, nil, run))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if max.Load() != 1 {
		t.Fatalf("max concurrent Run = %d, want 1", max.Load())
	}
}

func TestKeyTypeConflict(t *testing.T) {
	g := New(context.Background())
	mustAdd(t, g, NewNode("a", nil, Provides(NewKey[string]("dup")), func(rc *RunCtx) error { return nil }))
	err := g.Add(NewNode("b", nil, Provides(NewKey[int]("dup")), func(rc *RunCtx) error { return nil }))
	if err == nil {
		t.Fatal("expected type conflict")
	}
}

func TestUndeclaredWrite(t *testing.T) {
	g := New(context.Background())
	mustAdd(t, g, NewNode("a", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kB, "nope")
	}))
	if err := g.Run(); err == nil {
		t.Fatal("expected undeclared write error")
	}
}

func TestSetSkipConflict(t *testing.T) {
	g := New(context.Background())
	mustAdd(t, g, NewNode("a", nil, Provides(kA), func(rc *RunCtx) error {
		if err := Set(rc, kA, "v"); err != nil {
			return err
		}
		return Skip(rc, kA)
	}))
	if err := g.Run(); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

func mustAdd(t *testing.T, g *Graph, n *Node) {
	t.Helper()
	if err := g.Add(n); err != nil {
		t.Fatal(err)
	}
}

// inspect 构造一个能读图上任意已登记 Key 的 RunCtx（仅测试用）。
func inspect(g *Graph) *RunCtx {
	n := &Node{id: "inspect"}
	for name := range g.slots {
		ref := keyRef{name: name, typ: g.keys.seen[name]}
		n.requires = append(n.requires, ref)
	}
	return newRunCtx(g, n, context.Background())
}
