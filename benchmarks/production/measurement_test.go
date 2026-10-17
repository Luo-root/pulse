package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type interruptedBatch struct {
	calls int
	value uint64
	cause error
}

func (r *interruptedBatch) run(context.Context) (uint64, error) {
	r.calls++
	if r.calls == 3 {
		return 0, r.cause
	}
	return r.value, nil
}
func (*interruptedBatch) close() error { return nil }

func TestPartialBatchRetainsProgress(t *testing.T) {
	for _, cause := range []error{errors.New("business failure"), context.DeadlineExceeded} {
		g, _ := newGraph("chain", 1)
		_, want := g.reference(1)
		r := &interruptedBatch{value: want, cause: cause}
		adapters["controlled"] = func(context.Context, *graphSpec, Config, *probe) (runner, error) { return r, nil }
		c := Config{Engine: "controlled", Shape: "chain", Nodes: 1, Rounds: 1, Parallelism: 4, Mode: "plain"}
		s := sample{}
		err := measure(&s, c, "measure", "", 3, false, time.Second)
		delete(adapters, "controlled")
		if !errors.Is(err, cause) || r.calls != 3 || !s.FirstCompleted || s.Iterations != 1 || s.RequestedIterations != 3 {
			t.Fatalf("lost partial progress: %+v calls%d error%v", s, r.calls, err)
		}
		if s.RunNS != nil || s.AllocBytes != nil || s.Checksum != nil {
			t.Fatalf("incomplete batch invented aggregate: %+v", s)
		}
	}
}

func TestDeadlineUsesCause(t *testing.T) {
	c := Config{Engine: "context deadline exceeded", Shape: "chain", Nodes: 1, Rounds: 1, Parallelism: 4, Mode: "plain"}
	s := sample{}
	err := measure(&s, c, "measure", "", 1, false, time.Second)
	if err == nil || sampleErrorStatus(err) != "error" {
		t.Fatalf("diagnostic text became deadline: %v", err)
	}
	if got := sampleErrorStatus(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)); got != "deadline" {
		t.Fatalf("lost typed deadline: %s", got)
	}
}

func TestBenchmarkClockAdvances(t *testing.T) {
	start := benchNow()
	for i := 0; i < 10000; i++ {
		if benchSince(start) > 0 {
			return
		}
	}
	t.Fatal("benchmark clock did not resolve any interval")
}
