package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func TestTemporalInputValidation(t *testing.T) {
	for _, in := range []TemporalWorkflowInput{
		{},
		{Predecessors: [][]int{{0}}, Terminals: []int{0}, Parallelism: 4},
		{Predecessors: [][]int{nil}, Terminals: []int{1}, Parallelism: 4},
		{Predecessors: [][]int{nil}, Terminals: []int{0}, Parallelism: 0},
		{Predecessors: [][]int{nil}, Terminals: []int{0}, Parallelism: 4, GateAt: 2},
	} {
		if err := in.validate(); err == nil {
			t.Fatalf("accepted invalid input: %+v", in)
		}
	}
	g, err := newGraph("diamonds", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := (TemporalWorkflowInput{Predecessors: g.predecessors, Terminals: g.terminals, Parallelism: 4}).validate(); err != nil {
		t.Fatal(err)
	}
}

// 没有真实 Server 时明确跳过；不使用 testsuite、mock 或本地 workflow simulation。
func temporalLiveConfig(t *testing.T) Config {
	t.Helper()
	address := os.Getenv("PULSE_TEMPORAL_ADDRESS")
	if address == "" {
		t.Skip("set PULSE_TEMPORAL_ADDRESS to an independently started real Temporal server")
	}
	queue, err := temporalIDPrefix("pulse-temporal-test")
	if err != nil {
		t.Fatal(err)
	}
	return Config{Engine: "temporal", Address: address, Queue: queue, RunID: queue, Shape: "chain", Nodes: 8, Rounds: 256, Parallelism: 4, Mode: "plain"}
}

func TestTemporalLiveRunner(t *testing.T) {
	base := temporalLiveConfig(t)
	old := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(old)
	for _, mode := range []string{"plain", "observe"} {
		for _, shape := range []string{"chain", "wide", "diamonds"} {
			t.Run(mode+"/"+shape, func(t *testing.T) {
				cfg := base
				cfg.Mode, cfg.Shape, cfg.Queue = mode, shape, base.Queue+"/"+mode+"/"+shape
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				g, _ := newGraph(shape, cfg.Nodes)
				p := newProbe(cfg.Nodes)
				run, err := buildTemporal(ctx, g, cfg, p)
				if err != nil {
					t.Fatal(err)
				}
				r := run.(*temporalRunner)
				defer func() {
					if err := r.close(); err != nil {
						t.Error(err)
					}
				}()
				values, want := g.reference(cfg.Rounds)
				for repeat := 0; repeat < 2; repeat++ {
					p.reset()
					got, err := r.run(ctx)
					if err != nil || got != want {
						t.Fatalf("run %d: value %d want %d: %v", repeat, got, want, err)
					}
					if err := p.check(values, cfg.Parallelism); err != nil {
						t.Fatal(err)
					}
				}
				// 正式路径的 probe=nil 同样必须有正确结果。
				r.probe = nil
				if got, err := r.run(ctx); err != nil || got != want {
					t.Fatalf("unprobed run: value %d want %d: %v", got, want, err)
				}
				if obs := r.observations(); mode == "plain" {
					if obs != nil {
						t.Fatalf("plain observations: %v", obs)
					}
				} else if obs["activity_started"] != int64(3*cfg.Nodes) || obs["activity_finished"] != int64(3*cfg.Nodes) || obs["workflow_finished"] != 3 || obs["activity_failed"] != 0 {
					t.Fatalf("observe callback counts: %v", obs)
				}
				id := r.prefix + "/" + strconv.FormatUint(r.seq.Load(), 10)
				assertTemporalHistory(t, ctx, r.client, id, cfg.Nodes, cfg.Parallelism, false)
			})
		}
	}
}

func TestTemporalLiveCancelAndReuse(t *testing.T) {
	cfg := temporalLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	g, _ := newGraph(cfg.Shape, cfg.Nodes)
	p := newProbe(cfg.Nodes)
	run, err := buildTemporal(ctx, g, cfg, p)
	if err != nil {
		t.Fatal(err)
	}
	r := run.(*temporalRunner)
	defer func() {
		if err := r.close(); err != nil {
			t.Error(err)
		}
	}()
	r.input.GateAt = 3
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	outcome := make(chan error, 1)
	go func() { _, err := r.run(runCtx); outcome <- err }()
	// 使用第二个 client，独立从服务端证明前缀完成后才取消客户端等待。
	control, err := temporalControlClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	id := r.prefix + "/1"
	if _, err := WaitTemporalPrefix(ctx, control, id, "", 3); err != nil {
		t.Fatal(err)
	}
	cancelRun()
	select {
	case err := <-outcome:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run cancellation: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	description, err := control.DescribeWorkflowExecution(ctx, id, "")
	if err != nil || description.GetWorkflowExecutionInfo().GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED {
		t.Fatalf("server did not confirm cancellation: %v, %v", description, err)
	}
	if p.active.Load() != 0 {
		t.Fatalf("activities still active: %d", p.active.Load())
	}
	r.input.GateAt = 0
	p.reset()
	values, want := g.reference(cfg.Rounds)
	if got, err := r.run(ctx); err != nil || got != want {
		t.Fatalf("reuse after cancellation: %d want %d: %v", got, want, err)
	}
	if err := p.check(values, cfg.Parallelism); err != nil {
		t.Fatal(err)
	}
}

func TestTemporalLiveActivityFailureHasOneAttempt(t *testing.T) {
	cfg := temporalLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := temporalControlClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &temporalRunner{client: c}
	r.worker = worker.New(c, cfg.Queue, worker.Options{MaxConcurrentActivityExecutionSize: 4, WorkerStopTimeout: time.Second})
	r.worker.RegisterWorkflowWithOptions(TemporalBenchmarkWorkflow, workflow.RegisterOptions{Name: TemporalWorkflowName})
	var attempts atomic.Int32
	r.worker.RegisterActivityWithOptions(func(context.Context, TemporalNodeInput) (uint64, error) {
		attempts.Add(1)
		return 0, fmt.Errorf("deliberate real Activity failure")
	}, activity.RegisterOptions{Name: TemporalActivityName})
	if err := r.worker.Start(); err != nil {
		c.Close()
		t.Fatal(err)
	}
	defer func() {
		if err := r.close(); err != nil {
			t.Error(err)
		}
	}()
	wr, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: cfg.RunID, TaskQueue: cfg.Queue}, TemporalWorkflowName,
		TemporalWorkflowInput{Predecessors: [][]int{nil, {0}}, Terminals: []int{1}, Parallelism: 4, Rounds: 1})
	if err != nil {
		t.Fatal(err)
	}
	var result uint64
	err = wr.Get(ctx, &result)
	var failed *temporal.WorkflowExecutionError
	if !errors.As(err, &failed) || attempts.Load() != 1 {
		t.Fatalf("attempts=%d, error=%v", attempts.Load(), err)
	}
	assertTemporalHistory(t, ctx, c, wr.GetID(), 1, 4, true)
}

func assertTemporalHistory(t *testing.T, ctx context.Context, c client.Client, id string, wantNodes, limit int, failed bool) {
	t.Helper()
	iter := c.GetWorkflowHistory(ctx, id, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	pending, scheduled, finished, peak := 0, 0, 0, 0
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatal(err)
		}
		if attrs := event.GetActivityTaskScheduledEventAttributes(); attrs != nil {
			if attrs.GetActivityType().GetName() != TemporalActivityName || attrs.GetRetryPolicy().GetMaximumAttempts() != 1 {
				t.Fatalf("not a regular single-attempt Activity: %v", attrs)
			}
			pending++
			scheduled++
			peak = max(peak, pending)
		}
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
			if failed {
				t.Fatal("failed test unexpectedly completed Activity")
			}
			pending--
			finished++
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_FAILED:
			if !failed {
				t.Fatal("successful test failed Activity")
			}
			pending--
			finished++
		}
	}
	if pending != 0 || scheduled != wantNodes || finished != wantNodes || peak > limit {
		t.Fatalf("server history: pending=%d scheduled=%d finished=%d peak=%d wantNodes=%d limit=%d", pending, scheduled, finished, peak, wantNodes, limit)
	}
}
