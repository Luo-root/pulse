package observe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type asyncPanicBadError struct{ calls atomic.Uint32 }

func (e *asyncPanicBadError) Error() string {
	if e != nil {
		e.calls.Add(1)
	}
	panic("private-panic-error.Error")
}

type asyncPanicStringer struct{ calls atomic.Uint32 }

func (s *asyncPanicStringer) String() string {
	s.calls.Add(1)
	return "private-panic-Stringer"
}

type asyncPanicProbe struct {
	mode     string
	payload  any
	ordinary error
	gate     *asyncTestBarrier
	entered  chan struct{}

	mu      sync.Mutex
	events  []string
	buffers []string
	calls   int
}

func (p *asyncPanicProbe) Write(r Record) {
	if p.mode == "write" && r.Event == "panic-event-a" {
		panic(p.payload)
	}
	p.mu.Lock()
	p.events = append(p.events, r.Event)
	p.mu.Unlock()
}

func (p *asyncPanicProbe) invoke() error {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if p.gate != nil && call == 1 {
		close(p.entered)
		<-p.gate.done
	}
	switch p.mode {
	case "ordinary":
		return p.ordinary
	case "write":
		return nil
	default:
		panic(p.payload)
	}
}

type asyncPanicFlushSink struct{ probe *asyncPanicProbe }

func (s asyncPanicFlushSink) Write(r Record) { s.probe.Write(r) }
func (s asyncPanicFlushSink) Flush() error   { return s.probe.invoke() }

type asyncPanicErrSink struct{ probe *asyncPanicProbe }

func (s asyncPanicErrSink) Write(r Record) { s.probe.Write(r) }
func (s asyncPanicErrSink) Err() error     { return s.probe.invoke() }

type asyncPanicLineWriter struct{ probe *asyncPanicProbe }

func (w asyncPanicLineWriter) Write(b []byte) (int, error) {
	w.probe.mu.Lock()
	w.probe.buffers = append(w.probe.buffers, string(b))
	w.probe.mu.Unlock()
	if err := w.probe.invoke(); err != nil {
		return 0, err
	}
	return len(b), nil
}

func checkAsyncPanicResult(t *testing.T, err error, payload any) {
	t.Helper()
	if err == nil {
		t.Fatal("inner panic returned nil error")
	}
	text := err.Error() // 在子进程直接调用，错误格式化本身也不得 panic。
	if !strings.Contains(text, "panicked") || strings.Contains(text, "private-panic") {
		t.Fatalf("panic report is missing or includes its payload: %q", text)
	}
	if cause, ok := payload.(error); ok {
		if errors.Unwrap(err) != cause || !errors.Is(err, cause) {
			t.Fatal("panic error cause was not preserved safely")
		}
	} else if payload != nil && errors.Unwrap(err) != nil {
		t.Fatal("non-error panic payload was retained as an error cause")
	}
	if e, ok := payload.(*asyncPanicBadError); ok && e != nil && e.calls.Load() != 0 {
		t.Fatal("panic error.Error was invoked while reporting the panic")
	}
	if s, ok := payload.(*asyncPanicStringer); ok && s.calls.Load() != 0 {
		t.Fatal("panic Stringer was invoked while reporting the panic")
	}
}

func runAsyncPanicProcessCase(t *testing.T, name string) {
	t.Helper()
	parts := strings.Split(name, "/")
	if len(parts) != 3 {
		t.Fatal("invalid panic process case")
	}
	innerName, entry, mode := parts[0], parts[1], parts[2]
	probe := &asyncPanicProbe{mode: mode, payload: "private-panic-payload", ordinary: errors.New("ordinary sink error")}
	switch mode {
	case "error":
		probe.payload = errors.New("private-panic-error")
	case "bad-error":
		probe.payload = &asyncPanicBadError{}
	case "typed-nil-error":
		probe.payload = (*asyncPanicBadError)(nil)
	case "nil", "legacy-nil":
		probe.payload = nil
	case "stringer":
		probe.payload = &asyncPanicStringer{}
	case "cancel":
		probe.gate = newAsyncTestBarrier()
		probe.entered = make(chan struct{})
	}
	var inner Sink
	switch innerName {
	case "flush", "write":
		inner = asyncPanicFlushSink{probe}
	case "err":
		inner = asyncPanicErrSink{probe}
	case "line":
		inner = NewLineSink(asyncPanicLineWriter{probe}, WithBufSize(4096))
	default:
		t.Fatal("invalid inner sink")
	}
	as := NewAsyncSink(inner, WithCapacity(4))
	t.Cleanup(func() {
		if probe.gate != nil {
			probe.gate.release()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = as.Close(ctx)
		waitAsyncTestValue(t, as.workerDone)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	as.Write(Record{Event: "panic-event-a"})
	call := as.Flush
	if entry == "Close" {
		as.Write(Record{Event: "panic-event-b"})
		call = as.Close
	}
	var first error
	if mode == "cancel" {
		waitCtx, stop := context.WithCancel(ctx)
		done := runAsyncTestCall(func() error { return call(waitCtx) })
		waitAsyncTestValue(t, probe.entered)
		stop()
		first = waitAsyncTestValue(t, done)
		if !errors.Is(first, context.Canceled) {
			t.Fatalf("cancellation while inner was blocked = %v, want context.Canceled", first)
		}
		probe.gate.release()
	} else {
		first = call(ctx)
		switch mode {
		case "ordinary":
			if first != probe.ordinary {
				t.Fatal("ordinary inner error was wrapped or lost")
			}
		case "write":
			if first != nil {
				t.Fatal(first)
			}
		default:
			checkAsyncPanicResult(t, first, probe.payload)
		}
	}
	closed := first
	if entry == "Flush" {
		as.Write(Record{Event: "panic-event-b"}) // 捕获刷新 panic 后，worker 必须仍能处理。
		closed = as.Close(ctx)
		switch mode {
		case "ordinary":
			if closed != probe.ordinary {
				t.Fatal("Close lost the ordinary inner error")
			}
		case "write":
			if closed != nil {
				t.Fatal(closed)
			}
		default:
			checkAsyncPanicResult(t, closed, probe.payload)
		}
	}
	waitAsyncTestValue(t, as.workerDone)
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if got := as.Close(canceled); got != closed {
		t.Fatal("repeated Close changed its first result")
	}
	wantDropped := uint64(0)
	wantEvents := []string{"panic-event-a", "panic-event-b"}
	if mode == "write" {
		wantDropped = 1
		wantEvents = wantEvents[1:]
	}
	if got := as.Dropped(); got != wantDropped {
		t.Fatalf("Dropped=%d, want %d", got, wantDropped)
	}
	probe.mu.Lock()
	events := append([]string(nil), probe.events...)
	buffers := strings.Join(probe.buffers, "\n")
	calls := probe.calls
	probe.mu.Unlock()
	wantCalls := 1
	if entry == "Flush" {
		wantCalls = 2
	}
	if calls != wantCalls {
		t.Fatalf("inner calls=%d, want %d", calls, wantCalls)
	}
	if innerName == "line" {
		for _, event := range wantEvents {
			if !strings.Contains(buffers, event) {
				t.Fatalf("LineSink did not receive %q", event)
			}
		}
	} else if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("delivered=%v, want %v", events, wantEvents)
	}
	if mode == "cancel" && entry == "Close" {
		// 未恢复 panic 的 worker defer 也可能先关闭 workerDone；留出运行时
		// 报告致命 panic 的时间，避免子进程提前退出掩盖崩溃（复审探针同口径）。
		time.Sleep(250 * time.Millisecond)
	}
	t.Log("ASYNC_PANIC_PROCESS_SURVIVED")
}

// 真子进程把未恢复 worker panic 隔离在子进程，去掉 recover 的变异也只会让
// 本用例收到非零 exit，而不会杀掉整个 CI 测试进程。
func TestAsyncSinkPanicProcess(t *testing.T) {
	const envKey = "PULSE_ASYNC_PANIC_PROCESS"
	if name := os.Getenv(envKey); name != "" {
		runAsyncPanicProcessCase(t, name)
		return
	}
	var cases []string
	for _, inner := range []string{"flush", "err", "line"} {
		for _, entry := range []string{"Flush", "Close"} {
			for _, mode := range []string{"string", "ordinary", "cancel"} {
				cases = append(cases, inner+"/"+entry+"/"+mode)
			}
		}
	}
	cases = append(cases, "flush/Flush/error", "flush/Close/bad-error", "err/Flush/typed-nil-error", "err/Close/nil", "err/Close/legacy-nil", "flush/Flush/stringer", "write/Flush/write", "write/Close/write")
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAsyncSinkPanicProcess$", "-test.v", "-test.timeout=8s")
			// 只去掉子进程 race 退出前的默认一秒等待，保留宿主的其他 GORACE 设置。
			cmd.Env = append(os.Environ(), envKey+"="+name, "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
			if strings.HasSuffix(name, "/legacy-nil") {
				cmd.Env = append(cmd.Env, "GODEBUG="+os.Getenv("GODEBUG")+",panicnil=1")
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child process failed: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "ASYNC_PANIC_PROCESS_SURVIVED") {
				t.Fatalf("child did not verify completion:\n%s", output)
			}
		})
	}
}
