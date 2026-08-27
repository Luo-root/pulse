package kernel

import (
	"sync/atomic"
	"testing"
)

type localPayload struct {
	N int
}

var eventLocalProbe = NewEventKey[localPayload]("pulse.test.local_probe")
var eventLocalWF = NewEventKey[*localPayload]("pulse.test.local_waterfall")

// EmitLocal 只本 scope：root / 兄弟收不到。
func TestEmitLocalDoesNotCrossScopes(t *testing.T) {
	root := New()
	defer root.Dispose()
	a, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Dispose()
	b, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Dispose()

	var rootN, aN, bN atomic.Int32
	if _, err := On(root, eventLocalProbe, func(*localPayload) { rootN.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if _, err := On(a, eventLocalProbe, func(*localPayload) { aN.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if _, err := On(b, eventLocalProbe, func(*localPayload) { bN.Add(1) }); err != nil {
		t.Fatal(err)
	}

	EmitLocal(a, eventLocalProbe, localPayload{N: 1})
	if aN.Load() != 1 || bN.Load() != 0 || rootN.Load() != 0 {
		t.Fatalf("EmitLocal leaked: a=%d b=%d root=%d", aN.Load(), bN.Load(), rootN.Load())
	}

	// 对照：全树 Emit 仍广播到整棵树。
	Emit(a, eventLocalProbe, localPayload{N: 2})
	if aN.Load() < 2 || bN.Load() < 1 || rootN.Load() < 1 {
		t.Fatalf("Emit should still broadcast: a=%d b=%d root=%d", aN.Load(), bN.Load(), rootN.Load())
	}
}

// WaterfallLocal 只跑本层 around；兄弟策略互不影响。
func TestWaterfallLocalIsolatesHITLStylePolicies(t *testing.T) {
	root := New()
	defer root.Dispose()
	a, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Dispose()
	b, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Dispose()

	if _, err := OnWaterfall(a, eventLocalWF,
		func(p *localPayload, next func(*localPayload) *localPayload) *localPayload {
			p.N = -1 // A 拒绝/改写
			return p // 不调 next = 短路
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := OnWaterfall(b, eventLocalWF,
		func(p *localPayload, next func(*localPayload) *localPayload) *localPayload {
			p.N = 42 // B 放行并改写
			return next(p)
		}); err != nil {
		t.Fatal(err)
	}

	gotA := WaterfallLocal(a, eventLocalWF, &localPayload{N: 1})
	gotB := WaterfallLocal(b, eventLocalWF, &localPayload{N: 1})
	if gotA.N != -1 {
		t.Fatalf("A policy not applied: %d", gotA.N)
	}
	if gotB.N != 42 {
		t.Fatalf("B policy contaminated by A: %d", gotB.N)
	}
}

func TestEmitLocalNilSafe(t *testing.T) {
	EmitLocal[localPayload](nil, eventLocalProbe, localPayload{})
	got := WaterfallLocal(nil, eventLocalWF, &localPayload{N: 7})
	if got.N != 7 {
		t.Fatalf("nil WaterfallLocal should return payload, got %d", got.N)
	}
}
