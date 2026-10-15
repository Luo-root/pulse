package pulse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
