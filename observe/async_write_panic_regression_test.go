package observe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

type asyncWritePanicRegressionSink struct{ MemorySink }

func (s *asyncWritePanicRegressionSink) Write(r Record) {
	switch r.Event {
	case "panic-error":
		panic(errors.New("write panic error"))
	case "panic-string":
		panic("write panic string")
	case "panic-nil":
		panic(nil)
	default:
		s.MemorySink.Write(r)
	}
}

func runAsyncWritePanicRegression(t *testing.T, entry string) {
	t.Helper()
	inner := &asyncWritePanicRegressionSink{}
	as := NewAsyncSink(inner, WithCapacity(8))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	t.Cleanup(func() {
		_ = as.Close(ctx)
		waitAsyncTestValue(t, as.workerDone)
	})
	wantEvents := []string{"after-error", "after-string", "after-nil"}
	for _, kind := range []string{"error", "string", "nil"} {
		as.Write(Record{Event: "panic-" + kind})
		as.Write(Record{Event: "after-" + kind})
	}
	if entry == "Flush" {
		if err := as.Flush(ctx); err != nil {
			t.Fatalf("Flush poisoned by Write panic: %v", err)
		}
		if got := as.Dropped(); got != 3 {
			t.Errorf("after Flush: Dropped=%d, want 3", got)
		}
		as.Write(Record{Event: "after-flush"})
		wantEvents = append(wantEvents, "after-flush")
	}
	if err := as.Close(ctx); err != nil {
		t.Fatalf("Close poisoned by Write panic: %v", err)
	}
	waitAsyncTestValue(t, as.workerDone)
	var events []string
	for _, rec := range inner.Snapshot() {
		events = append(events, rec.Event)
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Errorf("delivered=%v, want %v", events, wantEvents)
	}
	if got := as.Dropped(); got != 3 {
		t.Errorf("after Close: Dropped=%d, want 3 (error, string, nil)", got)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	for _, call := range []func(context.Context) error{as.Close, as.Flush} {
		if err := call(canceled); err != nil {
			t.Errorf("latched successful Close result changed: %v", err)
		}
	}
	t.Log("ASYNC_WRITE_PANIC_REGRESSION_COMPLETED")
}

// #402：用独立运行时验证默认与旧 nil-panic 行为，不能靠 t.Setenv 动态
// 切换全局运行时。子进程和父进程都设超时，删除 recover 的变异也不会杀掉 CI。
func TestAsyncSinkWritePanicRuntimeRegression(t *testing.T) {
	const envKey = "PULSE_ASYNC_WRITE_PANIC_REGRESSION"
	if entry := os.Getenv(envKey); entry != "" {
		if entry != "Flush" && entry != "Close" {
			t.Fatalf("invalid child entry %q", entry)
		}
		runAsyncWritePanicRegression(t, entry)
		return
	}
	for _, runtimeCase := range []struct{ name, setting string }{
		{"default", "panicnil=0"},
		{"legacy-nil", "panicnil=1"},
	} {
		for _, entry := range []string{"Flush", "Close"} {
			t.Run(runtimeCase.name+"/"+entry, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAsyncSinkWritePanicRuntimeRegression$", "-test.v", "-test.timeout=8s")
				cmd.Env = append(os.Environ(), envKey+"="+entry,
					"GODEBUG="+os.Getenv("GODEBUG")+","+runtimeCase.setting,
					"GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
				cmd.WaitDelay = time.Second
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("child failed (context=%v): %v\n%s", ctx.Err(), err, output)
				}
				if !strings.Contains(string(output), "ASYNC_WRITE_PANIC_REGRESSION_COMPLETED") {
					t.Fatalf("child did not verify completion:\n%s", output)
				}
			})
		}
	}
}
