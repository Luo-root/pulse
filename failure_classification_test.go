package pulse

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func classificationDetectors() []struct {
	name      string
	detect    func(error) bool
	reference func(error) bool
} {
	return []struct {
		name      string
		detect    func(error) bool
		reference func(error) bool
	}{
		{"checkpoint", isCheckpointFailure, func(err error) bool { var target *CheckpointError; return errors.As(err, &target) }},
		{"panic", isPanicFailure, func(err error) bool { var target *nodePanicError; return errors.As(err, &target) }},
		{"cancellation", isExecutionCancellation, func(err error) bool { var target *executionCancellation; return errors.As(err, &target) }},
	}
}

// nil是每个成功节点都会经过的路径；这里约束分配，不设墙钟阈值。
func TestFailureClassificationNilDoesNotAllocate(t *testing.T) {
	for _, detector := range classificationDetectors() {
		t.Run(detector.name, func(t *testing.T) {
			if detector.detect(nil) {
				t.Fatal("nil classified as failure")
			}
			if allocations := testing.AllocsPerRun(200, func() { detector.detect(nil) }); allocations != 0 {
				t.Fatalf("nil classification allocated %g objects, want 0", allocations)
			}
		})
	}
}

type classificationResult struct {
	matched   bool
	panicked  bool
	panicText string
}

func classificationObserve(detect func(error) bool, err error) (result classificationResult) {
	result.panicked = true
	defer func() {
		value := recover()
		if result.panicked {
			result.panicText = fmt.Sprint(value)
		}
	}()
	result.matched = detect(err)
	result.panicked = false
	return result
}

type classificationUnwrapPanic struct{}

func (classificationUnwrapPanic) Error() string { return "unwrap probe" }
func (classificationUnwrapPanic) Unwrap() error { panic("classification unwrap") }

// 非nil接口值保留errors.As的原始行为，包括typed nil和会panic的自定义错误链。
func TestFailureClassificationNonNilKeepsErrorsAsBehavior(t *testing.T) {
	plain := errors.New("business")
	var nilCheckpoint *CheckpointError
	var nilPanic *nodePanicError
	var nilCancellation *executionCancellation
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"business", plain},
		{"context", context.Canceled},
		{"checkpoint", &CheckpointError{Operation: "save", Err: plain}},
		{"panic", &nodePanicError{nodeID: "node", value: plain}},
		{"cancellation", &executionCancellation{cause: context.Canceled}},
		{"wrapped-checkpoint", fmt.Errorf("outer: %w", &CheckpointError{Operation: "save", Err: plain})},
		{"joined", errors.Join(plain, &nodePanicError{nodeID: "node", value: plain}, &executionCancellation{cause: context.Canceled})},
		{"typed-nil-checkpoint", nilCheckpoint},
		{"typed-nil-panic", nilPanic},
		{"typed-nil-cancellation", nilCancellation},
		{"panicking-unwrap", classificationUnwrapPanic{}},
	}
	for _, detector := range classificationDetectors() {
		for _, test := range cases {
			t.Run(detector.name+"/"+test.name, func(t *testing.T) {
				got := classificationObserve(detector.detect, test.err)
				want := classificationObserve(detector.reference, test.err)
				if got != want {
					t.Fatalf("got %+v, original errors.As behavior %+v", got, want)
				}
			})
		}
	}
}

type classificationAsProbe struct{ calls int }

func (*classificationAsProbe) Error() string { return "custom As" }
func (p *classificationAsProbe) As(any) bool { p.calls++; return false }

func TestFailureClassificationNonNilStillCallsCustomAs(t *testing.T) {
	for _, detector := range classificationDetectors() {
		t.Run(detector.name, func(t *testing.T) {
			probe := &classificationAsProbe{}
			if detector.detect(probe) || probe.calls != 1 {
				t.Fatalf("custom As: calls=%d, want one unsuccessful lookup", probe.calls)
			}
		})
	}
}

func BenchmarkFailureClassificationNil(b *testing.B) {
	for _, detector := range classificationDetectors() {
		b.Run(detector.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if detector.detect(nil) {
					b.Fatal("nil classified as failure")
				}
			}
		})
	}
}
