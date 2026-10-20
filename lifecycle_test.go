package pulse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type lifecyclePanicCause struct{ error }

// 每个形状在独立测试进程执行；旧实现的 goroutine panic 不得炸掉回归集宿主。
func TestLifecycleAspectPanicContained(t *testing.T) {
	if shape := os.Getenv("PULSE_ASPECT_PANIC_SHAPE"); shape != "" {
		lifecycleCheckAspectPanic(t, shape, os.Getenv("PULSE_ASPECT_PANIC_VALUE"))
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{"ordinary", "timeout", "checkpoint", "checkpoint-timeout"} {
		for _, value := range []string{"string", "error", "skipped", "nil", "nil-legacy"} {
			t.Run(shape+"/"+value, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, exe, "-test.run=^TestLifecycleAspectPanicContained$", "-test.v")
				cmd.Env = append(os.Environ(), "PULSE_ASPECT_PANIC_SHAPE="+shape, "PULSE_ASPECT_PANIC_VALUE="+value, "GORACE=atexit_sleep_ms=0")
				if value == "nil-legacy" {
					cmd.Env = append(cmd.Env, "GODEBUG=panicnil=1")
				}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("child failed: %v\n%s", err, out)
				}
			})
		}
	}
}

func lifecycleCheckAspectPanic(t *testing.T, shape, value string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	obs := &recordingObserver{}
	g := mustNew(t, ctx, "aspect-panic", WithObserver(obs))
	key := NewKey[int]("value")
	cause := &lifecyclePanicCause{errors.New("aspect cause")}
	var payload any = "aspect panic"
	switch value {
	case "error":
		payload = cause
	case "skipped":
		payload = fmt.Errorf("panic is not a skip: %w", ErrSkipped)
	case "nil", "nil-legacy":
		payload = nil
	}
	aspects := []Aspect{func(rc *RunCtx, next func(*RunCtx) error) error {
		if err := next(rc); err != nil {
			return err
		}
		panic(payload)
	}}
	if strings.Contains(shape, "timeout") {
		aspects = append([]Aspect{Timeout(time.Second)}, aspects...)
	}
	mustAdd(t, g, NewNode("n", "切面返回后 panic", nil, Provides(key), func(rc *RunCtx) error {
		return Set(rc, key, 7)
	}, aspects...))
	p := &checkpointProbe{}
	var opts []ExecOption
	if strings.HasPrefix(shape, "checkpoint") {
		opts = append(opts, WithExecCheckpoint("root", p.hooks()))
	}
	e, err := g.NewExecution(ctx, Inputs{}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cancel()
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	err = e.Wait()
	if err == nil || !strings.Contains(err.Error(), "panic") || isSkipped(err) {
		t.Fatalf("panic was not a real failure: %v", err)
	}
	if value == "error" {
		var typed *lifecyclePanicCause
		if !errors.Is(err, cause) || !errors.As(err, &typed) || typed != cause {
			t.Fatalf("panic cause lost Is/As identity: %v", err)
		}
	}
	if value == "skipped" && !errors.Is(err, ErrSkipped) {
		t.Fatalf("panic lost its original cause: %v", err)
	}
	log := obs.snapshot()
	if countPref(log, "F:n:failed") != 1 || countPref(log, "GF:failed") != 1 {
		t.Fatalf("panic lifecycle = %v", log)
	}
	snapshot := e.Snapshot()
	if snapshot.Graph.Status != GraphStatusFailed || snapshot.Nodes[0].Status != NodeStatusFailed {
		t.Fatalf("panic snapshot = %+v / %+v", snapshot.Graph, snapshot.Nodes[0])
	}
	if len(opts) != 0 {
		if len(p.failures) != 1 || len(p.commits) != 0 || snapshot.Nodes[0].Outputs[0].State != SlotStatePending {
			t.Fatalf("panic bypassed full-chain confirmation: failures=%v commits=%d output=%s", p.failures, len(p.commits), snapshot.Nodes[0].Outputs[0].State)
		}
	}
}

func TestLifecycleAspectPanicRetry(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, timeout := range []bool{false, true} {
			t.Run(fmt.Sprintf("checkpoint=%v/timeout=%v", checkpoint, timeout), func(t *testing.T) {
				ctx := checkpointTestContext(t)
				g := mustNew(t, ctx, "panic-retry")
				key := NewKey[int]("value")
				calls := 0
				aspects := []Aspect{Retry(2, 0)}
				if timeout {
					aspects = append(aspects, Timeout(time.Second))
				}
				aspects = append(aspects, func(rc *RunCtx, next func(*RunCtx) error) error {
					if err := next(rc); err != nil {
						return err
					}
					if calls == 1 {
						panic(fmt.Errorf("first attempt: %w", ErrSkipped))
					}
					return nil
				})
				mustAdd(t, g, NewNode("n", "panic 尝试不发布", nil, Provides(key), func(rc *RunCtx) error {
					calls++
					return Set(rc, key, calls)
				}, aspects...))
				p := &checkpointProbe{}
				var opts []ExecOption
				if checkpoint {
					opts = append(opts, WithExecCheckpoint("root", p.hooks()))
				}
				e, err := g.NewExecution(ctx, Inputs{}, opts...)
				if err != nil {
					t.Fatal(err)
				}
				defer e.Cancel()
				if err := e.Start(); err != nil {
					t.Fatal(err)
				}
				if err := e.Wait(); err != nil || calls != 2 {
					t.Fatalf("retry err=%v calls=%d", err, calls)
				}
				if state, got := e.slotOf(key.asRef()).snapshot(); state != slotReady || got != 2 {
					t.Fatalf("failed attempt leaked: state=%v value=%v", state, got)
				}
				if checkpoint && (len(p.failures) != 1 || len(p.commits) != 1) {
					t.Fatalf("retry confirmation: failures=%v commits=%d", p.failures, len(p.commits))
				}
			})
		}
	}
}

func TestLifecycleAspectPanicFinishesManagedStream(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "panic-stream")
			key := NewKey[Stream[int]]("stream")
			var stream Stream[int]
			cause := errors.New("writer aspect panic")
			mustAdd(t, g, NewNode("source", "完整切面链失败终结流", nil, Provides(key), func(rc *RunCtx) error {
				writer, err := OpenStream(rc, key, WithStreamSource("source", true))
				if err != nil {
					return err
				}
				stream = Stream[int]{state: writer.state}
				return nil
			}, Timeout(time.Second), func(rc *RunCtx, next func(*RunCtx) error) error {
				if err := next(rc); err != nil {
					return err
				}
				panic(cause)
			}))
			mustAdd(t, g, NewNode("sink", "等真实写端终态", Requires(key), nil, func(rc *RunCtx) error {
				s, err := Get(rc, key)
				if err != nil {
					return err
				}
				_, err = s.Receive(rc)
				return err
			}))
			p := &checkpointProbe{}
			var finishes []CheckpointStreamFinish
			var opts []ExecOption
			if checkpoint {
				hooks := checkpointStreamTestHooks(p)
				hooks.StreamHooks.Finish = func(_ context.Context, f CheckpointStreamFinish) error {
					finishes = append(finishes, f)
					return nil
				}
				opts = append(opts, WithExecCheckpoint("root", hooks))
			}
			if err := g.Run(ctx, Inputs{}, opts...); !errors.Is(err, cause) {
				t.Fatalf("stream panic cause = %v", err)
			}
			if got := stream.Snapshot(); got.Terminal != StreamFailed || !errors.Is(got.Err, cause) {
				t.Fatalf("stream terminal = %+v", got)
			}
			if checkpoint && (len(finishes) != 1 || finishes[0].Terminal != StreamFailed || len(p.commits) != 0) {
				t.Fatalf("stream confirmation: finishes=%+v commits=%d", finishes, len(p.commits))
			}
		})
	}
}

func TestLifecycleTimeoutReleasesFinishedFork(t *testing.T) {
	for _, shape := range []string{"success", "failure", "retry", "without-timeout"} {
		t.Run(shape, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "timeout-cleanup")
			remaining, calls := -1, 0
			probe := func(rc *RunCtx, next func(*RunCtx) error) error {
				original := rc.ctx
				tracked := &lifecycleTrackingContext{Context: original}
				rc.ctx = tracked
				defer func() { rc.ctx = original }()
				err := next(rc)
				// 节点还没退出；不能靠 runNode 的最终 Cancel 掩盖 Timeout 留下的 Fork。
				remaining = tracked.registrations()
				return err
			}
			aspects := []Aspect{probe}
			attempts := 1
			if shape == "retry" || shape == "without-timeout" {
				attempts = 20
				aspects = append(aspects, Retry(attempts, 0))
			}
			if shape != "without-timeout" {
				aspects = append(aspects, Timeout(time.Hour))
			}
			cause := errors.New("quick failure")
			mustAdd(t, g, NewNode("n", "快速返回的 Fork 必须释放", nil, nil, func(*RunCtx) error {
				calls++
				if shape == "success" {
					return nil
				}
				return cause
			}, aspects...))
			err := g.Run(ctx, Inputs{})
			if (shape == "success" && err != nil) || (shape != "success" && !errors.Is(err, cause)) || calls != attempts {
				t.Fatalf("Run err=%v calls=%d want=%d", err, calls, attempts)
			}
			if remaining != 0 {
				t.Fatalf("%s retained %d completed Fork contexts before node cleanup", shape, remaining)
			}
		})
	}
}

func TestLifecycleTimeoutStillWaitsForInnerExit(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "timeout-cooperation")
	canceled, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	mustAdd(t, g, NewNode("n", "取消之后仍须等业务退出", nil, nil, func(rc *RunCtx) error {
		defer close(exited)
		<-rc.Context().Done()
		close(canceled)
		<-release
		return nil
	}, Timeout(time.Millisecond)))
	e, err := g.NewExecution(ctx, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cancel()
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- e.Wait() }()
	select {
	case <-canceled:
	case <-ctx.Done():
		close(release)
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-finished:
		close(release)
		t.Fatalf("Wait returned before the canceled body exited: %v", err)
	case <-time.After(20 * time.Millisecond):
		// 给超时调用方运行机会，不能用立即 default 掩盖提前返回的变异。
	}
	close(release)
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("timeout completion err=%v", err)
		}
		select {
		case <-exited:
		default:
			t.Fatal("Wait returned without the body's exit signal")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got := e.Snapshot(); got.Graph.Status != GraphStatusFailed || got.Nodes[0].Status != NodeStatusFailed {
		t.Fatalf("timeout outcome = %+v / %+v", got.Graph, got.Nodes[0])
	}
}

func TestLifecycleTimeoutKeepsDeadlineOrigin(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, shape := range []string{"host-deadline", "host-cancel", "node-timer", "nested-timer"} {
			t.Run(fmt.Sprintf("checkpoint=%t/%s", checkpoint, shape), func(t *testing.T) {
				var host context.Context
				var cancel context.CancelFunc
				if shape == "host-deadline" {
					host, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
				} else {
					host, cancel = context.WithCancel(checkpointTestContext(t))
				}
				defer cancel()
				obs := &recordingObserver{}
				g := mustNew(t, host, "timeout-origin", WithObserver(obs))
				aspects := []Aspect{Timeout(time.Hour)}
				if shape == "node-timer" {
					aspects = []Aspect{Timeout(time.Millisecond)}
				} else if shape == "nested-timer" {
					aspects = append([]Aspect{Timeout(time.Millisecond)}, aspects...)
				}
				mustAdd(t, g, NewNode("n", "父域截止不是本层一小时预算耗尽", nil, nil, func(rc *RunCtx) error {
					if shape == "host-cancel" {
						cancel()
					}
					<-rc.Context().Done()
					return rc.Context().Err()
				}, aspects...))
				p := &checkpointProbe{}
				var opts []ExecOption
				if checkpoint {
					opts = append(opts, WithExecCheckpoint("root", p.hooks()))
				}
				e, err := g.NewExecution(host, Inputs{}, opts...)
				managedTestOK(t, err)
				defer e.Cancel()
				managedTestOK(t, e.Start())
				err = e.Wait()
				want := NodeFailed
				if shape == "host-deadline" || shape == "host-cancel" {
					want = NodeCanceled
					cause := context.Canceled
					if shape == "host-deadline" {
						cause = context.DeadlineExceeded
					}
					if !errors.Is(err, cause) {
						t.Errorf("parent cancellation cause lost: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "timeout after 1ms") || host.Err() != nil {
					t.Errorf("own timer attributed to parent/wrong Timeout: err=%v host=%v", err, host.Err())
				}
				state := e.Snapshot()
				if state.Graph.Status != GraphStatus(want) || state.Nodes[0].Status != NodeStatus(want) {
					t.Errorf("want %s, graph=%s node=%s", want, state.Graph.Status, state.Nodes[0].Status)
				}
				if log := obs.snapshot(); countPref(log, "F:n:"+string(want)) != 1 || countPref(log, "GF:"+string(want)) != 1 {
					t.Errorf("wrong observer origin: %v", log)
				}
				if checkpoint {
					wantFailures := 1
					if want == NodeCanceled {
						wantFailures = 0
					}
					if len(p.failures) != wantFailures || len(p.commits) != 0 {
						t.Errorf("failure budget changed: failures=%v commits=%d", p.failures, len(p.commits))
					}
				}
			})
		}
	}
}

func TestLifecycleTimeoutPreservesPanicAfterLocalCancel(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, localCancel := range []bool{false, true} {
			for _, timeout := range []bool{false, true} {
				t.Run(fmt.Sprintf("checkpoint=%t/cancel=%t/timeout=%t", checkpoint, localCancel, timeout), func(t *testing.T) {
					ctx := checkpointTestContext(t)
					obs := &recordingObserver{}
					g := mustNew(t, ctx, "timeout-panic-origin", WithObserver(obs))
					cause := &lifecyclePanicCause{errors.New("panic after local cleanup")}
					var aspects []Aspect
					if timeout {
						aspects = append(aspects, Timeout(time.Second))
					}
					aspects = append(aspects, func(rc *RunCtx, next func(*RunCtx) error) error {
						if err := next(rc); err != nil {
							return err
						}
						panic(cause)
					})
					mustAdd(t, g, NewNode("n", "已捕获 panic 不能被本层清理覆盖", nil, nil, func(rc *RunCtx) error {
						if localCancel {
							rc.Cancel()
						}
						return nil
					}, aspects...))
					p := &checkpointProbe{}
					var opts []ExecOption
					if checkpoint {
						opts = append(opts, WithExecCheckpoint("root", p.hooks()))
					}
					e, err := g.NewExecution(ctx, Inputs{}, opts...)
					managedTestOK(t, err)
					defer e.Cancel()
					managedTestOK(t, e.Start())
					err = e.Wait()
					var typed *lifecyclePanicCause
					if !errors.Is(err, cause) || !errors.As(err, &typed) || typed != cause {
						t.Errorf("Timeout replaced the captured panic: %v", err)
					}
					state := e.Snapshot()
					if ctx.Err() != nil || state.Graph.Status != GraphStatusFailed || state.Nodes[0].Status != NodeStatusFailed || !errors.Is(state.Nodes[0].Err, cause) {
						t.Errorf("panic origin lost: host=%v graph=%+v node=%+v", ctx.Err(), state.Graph, state.Nodes[0])
					}
					if log := obs.snapshot(); countPref(log, "F:n:failed") != 1 || countPref(log, "GF:failed") != 1 {
						t.Errorf("wrong panic callbacks: %v", log)
					}
					if checkpoint && (len(p.failures) != 1 || len(p.commits) != 0) {
						t.Errorf("panic bypassed confirmation: failures=%v commits=%d", p.failures, len(p.commits))
					}
				})
			}
		}
	}
}

func TestLifecycleTimeoutKeepsOwnTimerDuringParentCleanup(t *testing.T) {
	_, _, rc, _ := managedTestEndpoint[int](t, 1)
	err := Timeout(time.Millisecond)(rc, func(inner *RunCtx) error {
		<-inner.Context().Done() // 本层计时器先触发，之后父层才被清理取消。
		rc.Cancel()
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "timeout after 1ms") || finishReason(err) != NodeFailed {
		t.Fatalf("later parent cleanup erased the first timer: %v", err)
	}
}

func TestLifecycleTimeoutPreservesReliableFailureAfterCancel(t *testing.T) {
	_, _, rc, _ := managedTestEndpoint[int](t, 1)
	cause := errors.New("reliable confirmation failed")
	failure := &CheckpointError{Operation: "inner confirmation", Err: cause}
	err := Timeout(time.Hour)(rc, func(inner *RunCtx) error {
		inner.Cancel()
		return failure
	})
	var typed *CheckpointError
	if !errors.Is(err, cause) || !errors.As(err, &typed) || typed != failure || finishReason(err) != NodeFailed {
		t.Fatalf("Timeout discarded reliable failure: %v", err)
	}
}

// 隐去 cancelCtx 的私有 Value，使标准 context 通过公开 AfterFunc 协议注册取消。
// 计数只描述仍注册的回调，不依赖标准库私有 children 字段，也不声称保留业务载荷。
type lifecycleTrackingContext struct {
	context.Context
	mu     sync.Mutex
	active int
}

func (c *lifecycleTrackingContext) Value(any) any { return nil }

func (c *lifecycleTrackingContext) AfterFunc(f func()) func() bool {
	c.mu.Lock()
	c.active++
	c.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			c.mu.Lock()
			c.active--
			c.mu.Unlock()
		})
	}
	stop := context.AfterFunc(c.Context, func() { release(); f() })
	return func() bool {
		stopped := stop()
		if stopped {
			release()
		}
		return stopped
	}
}

func (c *lifecycleTrackingContext) registrations() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func TestLifecycleRunCleansRejectedStart(t *testing.T) {
	for _, shape := range []string{"missing-source", "cycle", "missing-output", "checkpoint-hooks"} {
		t.Run(shape, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			tracked := &lifecycleTrackingContext{Context: parent}
			g := mustNew(t, parent, "rejected-start")
			x, y := NewKey[int]("x"), NewKey[int]("y")
			var opts []ExecOption
			switch shape {
			case "missing-source":
				mustAdd(t, g, NewNode("n", "缺来源", Requires(x), nil, nil))
			case "cycle":
				mustAdd(t, g, NewNode("a", "环的一边", Requires(x), Provides(y), nil))
				mustAdd(t, g, NewNode("b", "环的另一边", Requires(y), Provides(x), nil))
			case "missing-output":
				declareOut(t, g, NewPort(x))
			case "checkpoint-hooks":
				opts = append(opts, WithExecCheckpoint("root", CheckpointHooks{}))
			}
			for range 20 {
				if err := g.Run(tracked, Inputs{}, opts...); err == nil {
					t.Fatal("invalid Start accepted")
				}
			}
			if got := tracked.registrations(); got != 0 {
				t.Fatalf("20 rejected Run calls retained %d host cancellation registrations", got)
			}
			if g.sealed || parent.Err() != nil {
				t.Fatal("rejected Run sealed the definition or canceled the host")
			}
		})
	}
}

func TestLifecycleRunCleansTerminalExecution(t *testing.T) {
	for _, shape := range []string{"success", "failure", "cancel"} {
		t.Run(shape, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			tracked := &lifecycleTrackingContext{Context: parent}
			g := mustNew(t, parent, "terminal-run")
			cause := errors.New("business failure")
			mustAdd(t, g, NewNode("n", "终态清理", nil, nil, func(rc *RunCtx) error {
				switch shape {
				case "failure":
					return cause
				case "cancel":
					cancel()
					<-rc.Context().Done()
					return rc.Context().Err()
				}
				return nil
			}))
			err := g.Run(tracked, Inputs{})
			if (shape == "success" && err != nil) || (shape == "failure" && !errors.Is(err, cause)) || (shape == "cancel" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("%s Run = %v", shape, err)
			}
			if got := tracked.registrations(); got != 0 {
				t.Fatalf("terminal Run retained %d registrations", got)
			}
		})
	}
}

func TestLifecycleRejectedExecutionStartCanBeRepaired(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprintf("restore=%v", restore), func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			tracked := &lifecycleTrackingContext{Context: parent}
			g := mustNew(t, parent, "repair-start")
			x, y := NewKey[int]("x"), NewKey[int]("y")
			mustAdd(t, g, NewNode("n", "补装配后能启动", Requires(x), Provides(y), func(rc *RunCtx) error { return Set(rc, y, 2) }))
			var opts []ExecOption
			if restore {
				opts = append(opts, WithExecCheckpoint("root", (&checkpointProbe{}).hooks()))
			}
			e, err := g.NewExecution(tracked, Inputs{}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Cancel()
			if restore {
				if err := e.RestoreNodes([]NodeCheckpoint{{NodeID: "n", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: CheckpointKey{Name: "y", Type: reflect.TypeFor[int]()}, State: SlotStateReady, Value: 2}}}}); err != nil {
					t.Fatal(err)
				}
			}
			if !restore {
				if err := e.Start(); err == nil {
					t.Fatal("missing source accepted")
				}
			}
			mustAdd(t, g, NewNode("source", "补来源", nil, Provides(x), func(rc *RunCtx) error { return Set(rc, x, 1) }))
			if restore {
				if err := e.Start(); err == nil {
					t.Fatal("late-invalidated restore accepted")
				}
				if err := e.RestoreNodes(nil); err != nil {
					t.Fatal(err)
				}
			}
			registrations := 1
			if restore {
				registrations = 2 // 检查点的业务/控制链独立注册；拒绝 Start 不清掉任一活跃链。
			}
			if e.Context().Err() != nil || e.started || g.sealed || tracked.registrations() != registrations {
				t.Fatalf("rejected standalone Start lost repairability: ctx=%v started=%v sealed=%v registrations=%d", e.Context().Err(), e.started, g.sealed, tracked.registrations())
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			if err := e.Wait(); err != nil {
				t.Fatal(err)
			}
			if got := tracked.registrations(); got != 0 {
				t.Fatalf("repaired Execution retained %d registrations", got)
			}
		})
	}
}

type lifecycleInspectUnwrap struct {
	exec  *Execution
	cause error
}

func (e *lifecycleInspectUnwrap) Error() string { return "ordinary business failure" }
func (e *lifecycleInspectUnwrap) Unwrap() error {
	_ = e.exec.Err()
	return e.cause
}

type lifecycleInspectIs struct{ exec *Execution }

func (*lifecycleInspectIs) Error() string { return "ordinary business failure" }
func (e *lifecycleInspectIs) Is(error) bool {
	_ = e.exec.Err()
	return false
}

type lifecycleInspectAs struct{ exec *Execution }

func (*lifecycleInspectAs) Error() string { return "ordinary business failure" }
func (e *lifecycleInspectAs) As(any) bool {
	_ = e.exec.Err()
	return false
}

func TestLifecycleFailureErrorMethodsCanInspectExecution(t *testing.T) {
	// 错误方法会同步重入 Err；子进程隔离让锁回归有明确栈和时限，
	// 而不是把一条永久阻塞的 goroutine 留在其余测试里。
	if kind := os.Getenv("PULSE_TEST_FAILURE_REENTRY"); kind != "" {
		g := mustNew(t, t.Context(), "failure-reentry")
		var cause error
		mustAdd(t, g, NewNode("work", "错误链可观察执行状态", nil, nil, func(*RunCtx) error { return cause }))
		e, err := g.NewExecution(t.Context(), Inputs{})
		managedTestOK(t, err)
		defer e.Cancel()
		sentinel := errors.New("business cause")
		switch kind {
		case "Unwrap":
			cause = &lifecycleInspectUnwrap{exec: e, cause: sentinel}
		case "Is":
			cause = &lifecycleInspectIs{exec: e}
		case "As":
			cause = &lifecycleInspectAs{exec: e}
		default:
			t.Fatalf("unknown child mode %q", kind)
		}
		managedTestOK(t, e.Start())
		if got := e.Wait(); got != cause || e.Err() != cause {
			t.Fatalf("first error changed: %v", got)
		}
		if snapshot := e.Snapshot(); snapshot.Graph.Status != GraphStatusFailed || snapshot.Graph.Err != cause {
			t.Fatalf("failure snapshot changed: %+v", snapshot.Graph)
		}
		if kind == "Unwrap" && !errors.Is(e.Err(), sentinel) {
			t.Fatal("original cause lost")
		}
		return
	}
	for _, kind := range []string{"Unwrap", "Is", "As"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleFailureErrorMethodsCanInspectExecution$", "-test.timeout=3s")
			cmd.Env = append(os.Environ(), "PULSE_TEST_FAILURE_REENTRY="+kind)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("error.%s could not inspect Execution.Err: %v\n%s", kind, err, output)
			}
		})
	}
}

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

func TestLifecycleTimeoutRetainsCleanedResult(t *testing.T) {
	business := errors.New("business failed")
	for _, checkpoint := range []bool{false, true} {
		for _, cause := range []error{nil, NoValue(), business, fmt.Errorf("RPC: %w", context.Canceled), fmt.Errorf("RPC: %w", context.DeadlineExceeded)} {
			t.Run(fmt.Sprintf("checkpoint=%t/%v", checkpoint, cause), func(t *testing.T) {
				ctx := checkpointTestContext(t)
				g := mustNew(t, ctx, "timeout-cleaned-result")
				key := NewKey[int]("result")
				mustAdd(t, g, NewNode("work", "Timeout只判断自己的时限", nil, Provides(key), func(rc *RunCtx) error {
					defer rc.Cancel()
					if cause != nil {
						return cause
					}
					return Set(rc, key, 7)
				}, Timeout(time.Hour)))
				p := &checkpointProbe{}
				var opts []ExecOption
				if checkpoint {
					opts = []ExecOption{WithExecCheckpoint("root", p.hooks())}
				}
				e, err := g.NewExecution(ctx, Inputs{}, opts...)
				managedTestOK(t, err)
				managedTestOK(t, e.Start())
				err = e.Wait()
				wantReason := finishReason(cause)
				if cause == nil || isSkipped(cause) {
					if err != nil {
						t.Fatalf("cleanup changed successful result: %v", err)
					}
				} else if !errors.Is(err, cause) {
					t.Fatalf("cleanup replaced business error: got %v, want %v", err, cause)
				}
				if isCanceled(cause) {
					wantReason = NodeFailed // 健康执行中独立RPC的context错误是业务失败。
				}
				if got := e.Snapshot().Nodes[0].Status; string(got) != string(wantReason) {
					t.Fatalf("node status=%s, want %s", got, wantReason)
				}
				if checkpoint {
					if (cause == nil || isSkipped(cause)) && (len(p.commits) != 1 || len(p.failures) != 0) {
						t.Fatalf("successful cleanup not committed: commits=%d failures=%v", len(p.commits), p.failures)
					}
					if cause != nil && !isSkipped(cause) && (len(p.commits) != 0 || len(p.failures) != 1) {
						t.Fatalf("business failure not confirmed: commits=%d failures=%v", len(p.commits), p.failures)
					}
				}
			})
		}
	}
}

func TestLifecycleCheckpointNilPanicRejected(t *testing.T) {
	if os.Getenv("PULSE_TEST_CHECKPOINT_NIL_PANIC") == "1" {
		for _, operation := range []string{"commit", "open attempt", "chain"} {
			t.Run(operation, func(t *testing.T) {
				ctx := checkpointTestContext(t)
				g := mustNew(t, ctx, "hook-nil-panic")
				key := NewKey[int]("result")
				mustAdd(t, g, NewNode("work", "异常退出不得发布", nil, Provides(key), func(rc *RunCtx) error { return Set(rc, key, 7) }))
				hooks := (&checkpointProbe{}).hooks()
				if operation == "commit" {
					hooks.Commit = func(context.Context, CheckpointCommit) error { panic(nil) }
				} else if operation == "open attempt" {
					hooks.OpenAttempt = func(context.Context, CheckpointAttemptRequest) (CheckpointAttempt, error) { panic(nil) }
				}
				e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
				managedTestOK(t, err)
				if operation == "chain" {
					rc := newRunCtx(e, g.nodes[0], e.ctx)
					defer rc.Cancel()
					if got := invokeCheckpointChain(func(*RunCtx) error { panic(nil) }, rc); !isPanicFailure(got) {
						t.Fatalf("chain panic(nil) was lost: %v", got)
					}
					return
				}
				managedTestOK(t, e.Start())
				err = e.Wait()
				var failure *CheckpointError
				if !errors.As(err, &failure) || failure.Operation != operation || e.checkpointErr() == nil {
					t.Fatalf("hook panic(nil) was not latched: %v", err)
				}
				if got := e.Snapshot().Nodes[0].Outputs[0].State; got == SlotStateReady || (operation == "commit" && got != SlotStatePending) {
					t.Fatalf("unconfirmed hook published %s", got)
				}
			})
		}
		return
	}
	for _, debug := range []string{"panicnil=0", "panicnil=1"} {
		t.Run(debug, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleCheckpointNilPanicRejected$", "-test.timeout=5s")
			cmd.Env = append(os.Environ(), "PULSE_TEST_CHECKPOINT_NIL_PANIC=1", "GODEBUG="+debug, "GORACE=atexit_sleep_ms=0")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("checkpoint nil panic escaped: %v\n%s", err, output)
			}
		})
	}
}

type lifecycleErrorMethodPanic struct {
	method      string
	value       any
	calls       int
	inspections [3]int
	onPanic     func()
}

func (*lifecycleErrorMethodPanic) Error() string { return "error method panic probe" }
func (e *lifecycleErrorMethodPanic) inspect(method string) {
	index, trigger := 0, 3
	if method == "As" {
		index, trigger = 1, 6
	} else if method == "Unwrap" {
		index, trigger = 2, 8
	}
	e.inspections[index]++
	if e.method == method || (e.method == "late-"+method && e.inspections[index] == trigger) {
		e.calls++
		if e.onPanic != nil {
			e.onPanic()
		}
		panic(e.value)
	}
}
func (e *lifecycleErrorMethodPanic) Is(target error) bool {
	if e.method == "skip-target" && target == ErrSkipped {
		e.calls++
		if e.onPanic != nil {
			e.onPanic()
		}
		panic(e.value)
	}
	e.inspect("Is")
	return false
}
func (e *lifecycleErrorMethodPanic) As(any) bool   { e.inspect("As"); return false }
func (e *lifecycleErrorMethodPanic) Unwrap() error { e.inspect("Unwrap"); return nil }

func TestLifecycleReturnedErrorPanicContained(t *testing.T) {
	if os.Getenv("PULSE_TEST_ERROR_METHOD_PANIC") == "1" {
		for _, method := range []string{"Is", "As", "Unwrap", "skip-target", "late-Is", "late-As", "late-Unwrap"} {
			for _, shape := range []string{"ordinary", "timeout", "retry", "retry-timeout", "observed-timeout", "observed-retry", "checkpoint", "checkpoint-timeout", "checkpoint-retry", "checkpoint-retry-timeout", "checkpoint-observed-timeout", "checkpoint-observed-retry"} {
				for _, payload := range []string{"string", "nil", "self"} {
					t.Run(method+"/"+shape+"/"+payload, func(t *testing.T) {
						ctx := checkpointTestContext(t)
						g := mustNew(t, ctx, "error-method-panic")
						cause := &lifecycleErrorMethodPanic{method: method, value: "inspection failed"}
						if payload == "nil" {
							cause.value = nil
						}
						if payload == "self" {
							cause.value = cause
						}
						var aspects []Aspect
						if strings.Contains(shape, "timeout") {
							aspects = []Aspect{Timeout(time.Hour)}
						}
						if strings.Contains(shape, "retry") {
							aspects = append([]Aspect{Retry(2, 0)}, aspects...)
						}
						calls := 0
						mustAdd(t, g, NewNode("work", "错误方法异常也是失败", nil, nil, func(*RunCtx) error { calls++; return cause }, aspects...))
						p := &checkpointProbe{}
						callsAtPanic, failuresAtPanic := 0, 0
						cause.onPanic = func() { callsAtPanic, failuresAtPanic = calls, len(p.failures) }
						var opts []ExecOption
						if strings.HasPrefix(shape, "checkpoint") {
							opts = []ExecOption{WithExecCheckpoint("root", p.hooks())}
						}
						if strings.Contains(shape, "observed") {
							opts = append(opts, WithExecObserver((&aspectTrace{}).observer()))
						}
						e, err := g.NewExecution(ctx, Inputs{}, opts...)
						managedTestOK(t, err)
						managedTestOK(t, e.Start())
						err = e.Wait()
						// 延迟触发前可能已完成普通失败重试；异常实际发生后不得再次进入业务体。
						if !isPanicFailure(err) || finishReason(err) != NodeFailed || cause.calls != 1 || calls != callsAtPanic {
							t.Fatalf("error inspection panic changed control flow: err=%v calls=%d at-panic=%d panics=%d", err, calls, callsAtPanic, cause.calls)
						}
						if !strings.HasPrefix(method, "late-") && calls != 1 {
							t.Fatalf("initial control inspection retried business: calls=%d", calls)
						}
						wantFailures := failuresAtPanic + 1 // 检查异常只确认完整节点的根尝试。
						before := cause.inspections
						_ = errors.Is(err, ErrSkipped)
						_ = errors.Is(err, context.Canceled)
						_ = errors.Is(err, context.DeadlineExceeded)
						_ = e.Snapshot()
						_ = e.Wait()
						if cause.inspections != before {
							t.Fatal("recovered result traversed the broken error again")
						}
						if strings.HasPrefix(shape, "checkpoint") && (len(p.failures) != wantFailures || len(p.commits) != 0) {
							t.Fatalf("inspection failure was not confirmed: failures=%v commits=%d", p.failures, len(p.commits))
						}
					})
				}
			}
		}
		return
	}
	for _, debug := range []string{"panicnil=0", "panicnil=1"} {
		t.Run(debug, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleReturnedErrorPanicContained$", "-test.timeout=10s")
			cmd.Env = append(os.Environ(), "PULSE_TEST_ERROR_METHOD_PANIC=1", "GODEBUG="+debug, "GORACE=atexit_sleep_ms=0")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("returned error panic escaped: %v\n%s", err, output)
			}
		})
	}
}

// Is 可声明叶子是 skip；即使多错误包装自身声称 skip，也不能盖住真实失败分支。
type lifecycleSkipTree struct{ branches []error }

func (*lifecycleSkipTree) Error() string        { return "skip tree" }
func (*lifecycleSkipTree) Is(target error) bool { return target == ErrSkipped }
func (e *lifecycleSkipTree) Unwrap() []error    { return e.branches }

func TestLifecycleSkipErrorTree(t *testing.T) {
	business := errors.New("business failed")
	for _, tc := range []struct {
		name string
		err  error
		skip bool
	}{
		{"nil", nil, false},
		{"plain", NoValue(), true},
		{"wrapped", fmt.Errorf("wrapped: %w", NoValue()), true},
		{"all-skip", errors.Join(NoValue(), fmt.Errorf("wrapped: %w", NoValue())), true},
		{"nested-skip", fmt.Errorf("outer: %w", errors.Join(NoValue(), errors.Join(NoValue()))), true},
		{"leaf-Is", &checkpointConfirmedSkip{}, true},
		{"nil-branch", &lifecycleSkipTree{[]error{nil, NoValue()}}, true},
		{"empty-branches", &lifecycleSkipTree{}, false},
		{"business", business, false},
		{"skip-first", errors.Join(NoValue(), business), false},
		{"failure-first", errors.Join(business, NoValue()), false},
		{"multi-wrap", fmt.Errorf("two: %w and %w", NoValue(), business), false},
		{"nested", fmt.Errorf("outer: %w", errors.Join(NoValue(), errors.Join(NoValue(), business))), false},
		{"parent-Is", &lifecycleSkipTree{[]error{NoValue(), business}}, false},
		{"checkpoint", errors.Join(NoValue(), &CheckpointError{Operation: "save", Err: NoValue()}), false},
		{"panic", errors.Join(NoValue(), &nodePanicError{value: NoValue()}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSkipped(tc.err); got != tc.skip {
				t.Fatalf("isSkipped(%v)=%t, want %t", tc.err, got, tc.skip)
			}
		})
	}
}

func TestLifecycleJoinedSkipKeepsBusinessFailure(t *testing.T) {
	business := errors.New("business failed")
	for _, tc := range []struct {
		name string
		err  error
		skip bool
	}{
		{"plain-skip", NoValue(), true},
		{"wrapped-skip", fmt.Errorf("wrapped: %w", NoValue()), true},
		{"all-skip", errors.Join(NoValue(), fmt.Errorf("wrapped: %w", NoValue())), true},
		{"business", business, false},
		{"skip-first", errors.Join(NoValue(), business), false},
		{"failure-first", errors.Join(business, NoValue()), false},
		{"multi-wrap", fmt.Errorf("two: %w and %w", NoValue(), business), false},
		{"nested", fmt.Errorf("outer: %w", errors.Join(NoValue(), errors.Join(NoValue(), business))), false},
	} {
		for _, checkpoint := range []bool{false, true} {
			for _, retry := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/checkpoint=%t/retry=%t", tc.name, checkpoint, retry), func(t *testing.T) {
					ctx := checkpointTestContext(t)
					g := mustNew(t, ctx, "mixed-skip")
					key := NewKey[int]("result")
					calls := 0
					var aspects []Aspect
					if retry {
						aspects = []Aspect{Retry(2, 0)}
					}
					mustAdd(t, g, NewNode("work", "skip不能盖住真实失败", nil, Provides(key), func(rc *RunCtx) error {
						calls++
						if err := Set(rc, key, 42); err != nil {
							return err
						}
						return tc.err
					}, aspects...))
					managedTestOK(t, g.DeclareOutput(NewPort(key)))
					probe := &checkpointProbe{}
					var opts []ExecOption
					if checkpoint {
						opts = append(opts, WithExecCheckpoint("root", probe.hooks()))
					}
					nodeReason, graphReason := NodeFinishReason(""), NodeFinishReason("")
					opts = append(opts, WithExecObserver(ObserverFunc{
						Finished:      func(_ Site, _ string, reason NodeFinishReason, _ error) { nodeReason = reason },
						GraphFinished: func(_ Site, reason NodeFinishReason, _ error) { graphReason = reason },
					}))
					e, err := g.NewExecution(ctx, Inputs{}, opts...)
					managedTestOK(t, err)
					managedTestOK(t, e.Start())
					err = e.Wait()
					wantCalls, wantNode, wantGraph := 1, NodeSkipped, NodeCompleted
					wantStatus := GraphStatusCompleted
					if !tc.skip {
						wantNode, wantGraph, wantStatus = NodeFailed, NodeFailed, GraphStatusFailed
						if retry {
							wantCalls = 2
						}
						if !errors.Is(err, business) {
							t.Errorf("business error lost: %v", err)
						}
					} else if err != nil {
						t.Errorf("pure skip failed: %v", err)
					}
					state := e.Snapshot()
					if calls != wantCalls || nodeReason != wantNode || graphReason != wantGraph || state.Graph.Status != wantStatus || string(state.Nodes[0].Status) != string(wantNode) {
						t.Errorf("calls=%d node=%s graph=%s snapshot=%s/%s; want %d/%s/%s", calls, nodeReason, graphReason, state.Graph.Status, state.Nodes[0].Status, wantCalls, wantNode, wantGraph)
					}
					// 没有暂存时已经发布的值不回滚；Retry/检查点的失败写集不得发布。
					if slot, _ := e.slotOf(key.asRef()).snapshot(); slot == slotReady && !tc.skip && (retry || checkpoint) {
						t.Error("failed attempt published its staged output")
					}
					if tc.skip {
						value, slot, readErr := ReadOutput(e, key)
						if value != 42 || slot != SlotStateReady || readErr != nil {
							t.Errorf("normal skip lost output: %d/%s/%v", value, slot, readErr)
						}
					}
					if checkpoint {
						wantCommits, wantFailures := 1, 0
						if !tc.skip {
							wantCommits, wantFailures = 0, 1
							if retry {
								wantFailures += wantCalls
							}
						}
						if len(probe.commits) != wantCommits || len(probe.failures) != wantFailures {
							t.Errorf("commits=%d failures=%d, want %d/%d", len(probe.commits), len(probe.failures), wantCommits, wantFailures)
						}
					}
				})
			}
		}
	}
}

func TestLifecycleJoinedSkipRetriesBeforePublishing(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkpoint=%t", checkpoint), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "retry-mixed-skip")
			key := NewKey[int]("result")
			calls := 0
			mustAdd(t, g, NewNode("work", "混合失败后只发布成功尝试", nil, Provides(key), func(rc *RunCtx) error {
				calls++
				if state, _ := rc.exec.slotOf(key.asRef()).snapshot(); state != slotPending {
					t.Error("failed attempt became visible before retry succeeded")
				}
				if err := Set(rc, key, calls); err != nil {
					return err
				}
				if calls == 1 {
					return errors.Join(NoValue(), errors.New("try again"))
				}
				return errors.Join(NoValue(), fmt.Errorf("normal absence: %w", NoValue()))
			}, Retry(2, 0)))
			managedTestOK(t, g.DeclareOutput(NewPort(key)))
			var opts []ExecOption
			if checkpoint {
				opts = []ExecOption{WithExecCheckpoint("root", (&checkpointProbe{}).hooks())}
			}
			e, err := g.NewExecution(ctx, Inputs{}, opts...)
			managedTestOK(t, err)
			managedTestOK(t, e.Start())
			managedTestOK(t, e.Wait())
			value, state, err := ReadOutput(e, key)
			if err != nil || calls != 2 || value != 2 || state != SlotStateReady || e.Snapshot().Nodes[0].Status != NodeStatusSkipped {
				t.Fatalf("mixed error ended retry early or published stale output: calls=%d output=%d/%s err=%v", calls, value, state, err)
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
