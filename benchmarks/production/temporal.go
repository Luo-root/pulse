package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const (
	TemporalWorkflowName   = "pulse-production-graph-v1"
	TemporalActivityName   = "pulse-production-node-v1"
	TemporalRecoverySignal = "pulse-production-continue"
	temporalCleanupTimeout = 5 * time.Second
)

func init() { adapters["temporal"] = buildTemporal }

// TemporalWorkflowInput 是真实服务保存的图定义；前驱下标必须小于节点下标。
// GateAt=0 是普通执行；正数使完整前缀结束后等待恢复 signal，不增加业务节点。
type TemporalWorkflowInput struct {
	Predecessors [][]int
	Terminals    []int
	Rounds       int
	Parallelism  int
	GateAt       int
}

type TemporalNodeInput struct {
	ID                int
	Rounds            int
	PredecessorValues []uint64
}

func (in TemporalWorkflowInput) validate() error {
	if len(in.Predecessors) == 0 || len(in.Terminals) == 0 || in.Parallelism < 1 || in.Rounds < 0 {
		return fmt.Errorf("temporal: invalid graph, parallelism, or rounds")
	}
	if in.GateAt < 0 || in.GateAt > len(in.Predecessors) {
		return fmt.Errorf("temporal: invalid gate %d", in.GateAt)
	}
	for i, preds := range in.Predecessors {
		for _, from := range preds {
			if from < 0 || from >= i {
				return fmt.Errorf("temporal: node %d has non-preceding dependency %d", i, from)
			}
		}
	}
	for _, id := range in.Terminals {
		if id < 0 || id >= len(in.Predecessors) {
			return fmt.Errorf("temporal: invalid terminal %d", id)
		}
	}
	return nil
}

// TemporalBenchmarkWorkflow 仅用公开 Workflow API；每个节点对应一个常规 Activity。
// 信号量在 ExecuteActivity 之前取得，不能靠 worker 并发上限掩盖海量已调度任务。
func TemporalBenchmarkWorkflow(ctx workflow.Context, in TemporalWorkflowInput) (uint64, error) {
	if err := in.validate(); err != nil {
		return 0, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidGraph", nil)
	}
	ctx, cancel := workflow.WithCancel(ctx)
	defer cancel()
	sem := workflow.NewSemaphore(ctx, int64(in.Parallelism))
	futures := make([]workflow.Future, len(in.Predecessors))
	setters := make([]workflow.Settable, len(futures))
	for i := range futures {
		futures[i], setters[i] = workflow.NewFuture(ctx)
	}
	var firstErr error
	runRange := func(start, end int) error {
		for i := start; i < end; i++ {
			workflow.Go(ctx, func(ctx workflow.Context) {
				var value uint64
				var err error
				defer func() {
					if err != nil && firstErr == nil {
						firstErr = err
						cancel()
					}
					setters[i].Set(value, err)
				}()
				values := make([]uint64, len(in.Predecessors[i]))
				for j, from := range in.Predecessors[i] {
					if err = futures[from].Get(ctx, &values[j]); err != nil {
						return
					}
				}
				if err = sem.Acquire(ctx, 1); err != nil {
					return
				}
				defer sem.Release(1)
				activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
					ActivityID:             "n" + strconv.Itoa(i),
					StartToCloseTimeout:    time.Minute,
					ScheduleToCloseTimeout: time.Minute,
					WaitForCancellation:    true,
					RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: 1},
				})
				err = workflow.ExecuteActivity(activityCtx, TemporalActivityName, TemporalNodeInput{
					ID: i, Rounds: in.Rounds, PredecessorValues: values,
				}).Get(ctx, &value)
			})
		}
		// 包括取消路径也等全部 future 收尾；WaitForCancellation 保留 Activity 确认。
		for i := start; i < end; i++ {
			_ = futures[i].Get(ctx, nil)
		}
		return firstErr
	}
	if in.GateAt > 0 {
		if err := runRange(0, in.GateAt); err != nil {
			return 0, err
		}
		selector := workflow.NewSelector(ctx)
		selector.AddReceive(workflow.GetSignalChannel(ctx, TemporalRecoverySignal), func(ch workflow.ReceiveChannel, _ bool) {
			ch.Receive(ctx, nil)
		})
		selector.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) {})
		selector.Select(ctx)
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}
	if err := runRange(in.GateAt, len(futures)); err != nil {
		return 0, err
	}
	var sum uint64
	for _, id := range in.Terminals {
		var value uint64
		if err := futures[id].Get(ctx, &value); err != nil {
			return 0, err
		}
		sum += value
	}
	return sum, nil
}

type temporalRunner struct {
	client client.Client
	worker worker.Worker
	input  TemporalWorkflowInput
	queue  string
	prefix string
	probe  *probe
	stats  *temporalCounters
	seq    atomic.Uint64
	bodies sync.WaitGroup

	mu                      sync.Mutex
	closed                  bool
	runDone                 chan struct{}
	runCancel               context.CancelFunc
	pendingID, pendingRunID string
	closeOnce               sync.Once
	closeErr                error
}

func buildTemporal(ctx context.Context, g *graphSpec, cfg Config, p *probe) (runner, error) {
	if cfg.Address == "" || cfg.Queue == "" || (cfg.Mode != "plain" && cfg.Mode != "observe") {
		return nil, fmt.Errorf("temporal: require real server address, case-exclusive queue, and plain/observe mode")
	}
	if g == nil {
		return nil, fmt.Errorf("temporal: nil graph")
	}
	in := TemporalWorkflowInput{Predecessors: g.predecessors, Terminals: g.terminals, Rounds: cfg.Rounds, Parallelism: cfg.Parallelism}
	if err := in.validate(); err != nil {
		return nil, err
	}
	prefix, err := temporalIDPrefix(cfg.RunID)
	if err != nil {
		return nil, err
	}
	c, err := client.DialContext(ctx, client.Options{HostPort: cfg.Address, Logger: temporalQuietLogger{}})
	if err != nil {
		return nil, fmt.Errorf("temporal dial: %w", err)
	}
	r := &temporalRunner{client: c, input: in, queue: cfg.Queue, prefix: prefix, probe: p}
	opts := worker.Options{MaxConcurrentActivityExecutionSize: cfg.Parallelism, WorkerStopTimeout: time.Second}
	if cfg.Mode == "observe" {
		r.stats = &temporalCounters{}
		opts.Interceptors = []interceptor.WorkerInterceptor{&temporalCounterInterceptor{counters: r.stats}}
	}
	r.worker = worker.New(c, cfg.Queue, opts)
	r.worker.RegisterWorkflowWithOptions(TemporalBenchmarkWorkflow, workflow.RegisterOptions{Name: TemporalWorkflowName})
	r.worker.RegisterActivityWithOptions(r.executeActivity, activity.RegisterOptions{Name: TemporalActivityName})
	if err := r.worker.Start(); err != nil {
		c.Close()
		return nil, fmt.Errorf("temporal worker start: %w", err)
	}
	return r, nil
}

func temporalIDPrefix(prefix string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("temporal workflow identity: %w", err)
	}
	return prefix + "/" + hex.EncodeToString(nonce[:]), nil
}

func (r *temporalRunner) executeActivity(ctx context.Context, in TemporalNodeInput) (uint64, error) {
	r.bodies.Add(1)
	defer r.bodies.Done()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.probe != nil {
		r.probe.enter(in.ID)
		defer r.probe.leave()
	}
	input := uint64(1)
	for _, value := range in.PredecessorValues {
		input += value
	}
	value := compute(input, in.ID, in.Rounds)
	if r.probe != nil {
		r.probe.record(in.ID, value)
	}
	return value, nil
}

func (r *temporalRunner) run(ctx context.Context) (uint64, error) {
	r.mu.Lock()
	if r.closed || r.runDone != nil || r.pendingID != "" {
		r.mu.Unlock()
		return 0, fmt.Errorf("temporal: runner closed or already running")
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.runDone, r.runCancel = done, cancel
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		r.runDone, r.runCancel = nil, nil
		close(done)
		r.mu.Unlock()
	}()
	id := r.prefix + "/" + strconv.FormatUint(r.seq.Add(1), 10)
	r.mu.Lock()
	r.pendingID = id
	r.mu.Unlock()
	wr, err := r.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: r.queue}, TemporalWorkflowName, r.input)
	if wr != nil {
		r.mu.Lock()
		r.pendingRunID = wr.GetRunID()
		r.mu.Unlock()
	}
	var value uint64
	if err == nil {
		err = wr.Get(ctx, &value)
	}
	if err != nil {
		// SDK 将 RPC 取消转为没有 Is/Unwrap 的 serviceerror.Canceled；保留宿主
		// context 的实际 cause，同时保留原 SDK 错误，不能只靠错误文本判断取消。
		err = errors.Join(ctx.Err(), err)
		// Get 的 context 只取消客户端等待，不能拿它当 Workflow 已取消的证据。
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), temporalCleanupTimeout)
		defer cleanupCancel()
		runID := ""
		if wr != nil {
			runID = wr.GetRunID()
		}
		cleanupErr := cancelTemporalRun(cleanupCtx, r.client, id, runID)
		if cleanupErr == nil {
			cleanupErr = waitTemporalBodies(cleanupCtx, &r.bodies)
		}
		if cleanupErr != nil {
			return 0, errors.Join(err, fmt.Errorf("temporal cleanup: %w", cleanupErr))
		}
		r.clearPending()
		return 0, err
	}
	r.clearPending()
	return value, nil
}

func (r *temporalRunner) clearPending() {
	r.mu.Lock()
	r.pendingID, r.pendingRunID = "", ""
	r.mu.Unlock()
}

func cancelTemporalRun(ctx context.Context, c client.Client, id, runID string) error {
	cancelErr := c.CancelWorkflow(ctx, id, runID)
	// Cancel 的 NotFound 也可能表示已经结束；必须继续查终态。Start 结果不明时，
	// 连续 NotFound 仍不是确认，不能悄悄把未证实的取消当作成功。
	// 只有 WorkflowExecutionError（或成功结果）证明服务端已有终态；RPC 取消不算。
	getErr := c.GetWorkflow(ctx, id, runID).Get(ctx, nil)
	var ended *temporal.WorkflowExecutionError
	if getErr == nil || errors.As(getErr, &ended) {
		return nil
	}
	return errors.Join(cancelErr, getErr)
}

func waitTemporalBodies(ctx context.Context, bodies *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() { bodies.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *temporalRunner) close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		done, cancel := r.runDone, r.runCancel
		r.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		ctx, cancelCleanup := context.WithTimeout(context.Background(), 2*temporalCleanupTimeout)
		defer cancelCleanup()
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				r.closeErr = fmt.Errorf("temporal close waiting for run: %w", ctx.Err())
			}
		}
		r.mu.Lock()
		id, runID := r.pendingID, r.pendingRunID
		r.mu.Unlock()
		if id != "" {
			r.closeErr = errors.Join(r.closeErr, cancelTemporalRun(ctx, r.client, id, runID))
		}
		stopped := make(chan struct{})
		go func() { r.worker.Stop(); close(stopped) }()
		select {
		case <-stopped:
		case <-ctx.Done():
			r.closeErr = errors.Join(r.closeErr, fmt.Errorf("temporal worker stop: %w", ctx.Err()))
		}
		r.closeErr = errors.Join(r.closeErr, waitTemporalBodies(ctx, &r.bodies))
		r.client.Close()
	})
	return r.closeErr
}

// 业务日志关闭；服务端原生历史及默认持久化不受影响。
type temporalQuietLogger struct{}

func (temporalQuietLogger) Debug(string, ...any) {}
func (temporalQuietLogger) Info(string, ...any)  {}
func (temporalQuietLogger) Warn(string, ...any)  {}
func (temporalQuietLogger) Error(string, ...any) {}

type temporalCounters struct {
	workflowStarted, workflowFinished, workflowFailed atomic.Int64
	activityStarted, activityFinished, activityFailed atomic.Int64
}

type temporalCounterInterceptor struct {
	interceptor.WorkerInterceptorBase
	counters *temporalCounters
}

func (i *temporalCounterInterceptor) InterceptActivity(_ context.Context, next interceptor.ActivityInboundInterceptor) interceptor.ActivityInboundInterceptor {
	return &temporalActivityCounter{ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next}, counters: i.counters}
}
func (i *temporalCounterInterceptor) InterceptWorkflow(_ workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	return &temporalWorkflowCounter{WorkflowInboundInterceptorBase: interceptor.WorkflowInboundInterceptorBase{Next: next}, counters: i.counters}
}

type temporalActivityCounter struct {
	interceptor.ActivityInboundInterceptorBase
	counters *temporalCounters
}

func (i *temporalActivityCounter) ExecuteActivity(ctx context.Context, in *interceptor.ExecuteActivityInput) (any, error) {
	i.counters.activityStarted.Add(1)
	value, err := i.Next.ExecuteActivity(ctx, in)
	i.counters.activityFinished.Add(1)
	if err != nil {
		i.counters.activityFailed.Add(1)
	}
	return value, err
}

type temporalWorkflowCounter struct {
	interceptor.WorkflowInboundInterceptorBase
	counters *temporalCounters
}

func (i *temporalWorkflowCounter) ExecuteWorkflow(ctx workflow.Context, in *interceptor.ExecuteWorkflowInput) (any, error) {
	if !workflow.IsReplaying(ctx) {
		i.counters.workflowStarted.Add(1)
	}
	value, err := i.Next.ExecuteWorkflow(ctx, in)
	if !workflow.IsReplaying(ctx) {
		i.counters.workflowFinished.Add(1)
		if err != nil {
			i.counters.workflowFailed.Add(1)
		}
	}
	return value, err
}

func (r *temporalRunner) observations() map[string]int64 {
	if r.stats == nil {
		return nil
	}
	return map[string]int64{
		"workflow_started": r.stats.workflowStarted.Load(), "workflow_finished": r.stats.workflowFinished.Load(),
		"workflow_failed": r.stats.workflowFailed.Load(), "activity_started": r.stats.activityStarted.Load(),
		"activity_finished": r.stats.activityFinished.Load(), "activity_failed": r.stats.activityFailed.Load(),
	}
}
