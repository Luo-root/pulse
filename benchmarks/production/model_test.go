package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestIndependentGolden(t *testing.T) {
	// 与历史相同业务；这些固定值由 Python 任意精度整数逐步截断独立计算。
	for _, v := range []struct {
		input      uint64
		id, rounds int
		want       uint64
	}{
		{1, 0, 1, 1164970750538294679}, {1, 0, 256, 13017035472905934870}, {^uint64(0), 99, 1, 6596097752651701936},
	} {
		if got := compute(v.input, v.id, v.rounds); got != v.want {
			t.Fatalf("compute %+v got %d", v, got)
		}
	}
}

func TestEmbeddedCorrectness(t *testing.T) {
	for _, engine := range []string{"pulse", "eino"} {
		for _, shape := range []string{"chain", "wide", "diamonds"} {
			for _, n := range []int{1, 2, 10, 100} {
				for _, mode := range []string{"plain", "observe"} {
					t.Run(fmt.Sprintf("%s/%s/N%d/%s", engine, shape, n, mode), func(t *testing.T) {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						c := Config{Engine: engine, Shape: shape, Nodes: n, Rounds: 256, Parallelism: 4, Mode: mode}
						g, err := newGraph(shape, n)
						if err != nil {
							t.Fatal(err)
						}
						p := newProbe(n)
						r, err := adapters[engine](ctx, g, c, p)
						if err != nil {
							t.Fatal(err)
						}
						defer r.close()
						values, want := g.reference(c.Rounds)
						for run := 0; run < 2; run++ {
							got, err := r.run(ctx)
							if err != nil || got != want {
								t.Fatalf("run%d: %d %v want%d", run, got, err, want)
							}
							if err = p.check(values, 4); err != nil {
								t.Fatal(err)
							}
							p.reset()
						}
					})
				}
			}
		}
	}
}

func TestBudgetIncludesSetup(t *testing.T) {
	adapters["budget-setup"] = func(ctx context.Context, _ *graphSpec, _ Config, _ *probe) (runner, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	defer delete(adapters, "budget-setup")
	c := Config{Engine: "budget-setup", Shape: "chain", Nodes: 10, Rounds: 1, Parallelism: 4, Mode: "plain"}
	s := sample{}
	if err := measure(&s, c, "measure", "", 1, false, time.Millisecond); err == nil {
		t.Fatal("setup did not consume the whole-case budget")
	}
	if s.RunNS != nil || s.Checksum != nil {
		t.Fatal("expired case invented completed timing/checksum")
	}
}
