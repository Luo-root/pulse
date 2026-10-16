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

// 错误链允许只读诊断；Unwrap 不应在持有同一流状态锁时被调用。
type streamSnapshotError struct {
	stream  Stream[int]
	cause   error
	reenter bool
}

func (*streamSnapshotError) Error() string { return "stream diagnostic failure" }
func (e *streamSnapshotError) Unwrap() error {
	if e.reenter {
		_ = e.stream.Snapshot()
	}
	return e.cause
}

// 子进程有自己的测试超时：回归成锁重入时必须明确失败，不能拖死整套测试。
func TestManagedStreamErrorReentry(t *testing.T) {
	if mode := os.Getenv("PULSE_TEST_STREAM_ERROR_REENTRY"); mode != "" {
		testManagedStreamErrorReentry(t, mode)
		return
	}
	for _, path := range []string{"local-writer", "checkpoint-writer", "reader", "latched-writer"} {
		for _, reenter := range []bool{false, true} {
			mode := fmt.Sprintf("%s/%t", path, reenter)
			t.Run(mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagedStreamErrorReentry$", "-test.timeout=2s", "-test.v")
				cmd.Env = append(os.Environ(), "PULSE_TEST_STREAM_ERROR_REENTRY="+mode, "GORACE=atexit_sleep_ms=0")
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("stream error classification did not complete: %v\n%s", err, output)
				}
			})
		}
	}
}

func testManagedStreamErrorReentry(t *testing.T, mode string) {
	t.Helper()
	path, reenter, ok := strings.Cut(mode, "/")
	if !ok {
		t.Fatalf("invalid child mode %q", mode)
	}
	cause := errors.New("original business failure")
	if path == "reader" || path == "latched-writer" {
		writer, stream, _, readerRC := managedTestEndpoint[int](t, 1)
		failure := &streamSnapshotError{stream: stream, cause: cause, reenter: reenter == "true"}
		if path == "reader" {
			if err := stream.pulseReaderFinished(readerRC, failure); !errors.Is(err, cause) {
				t.Fatalf("reader failure changed: %v", err)
			}
			if !errors.Is(writer.state.failure(), cause) {
				t.Fatal("reader failure was not latched for the writer")
			}
			return
		}
		writer.state.recordFailure(failure)
		writer.state.finish(nil) // 完整链表面成功，也必须保留此前发送失败。
		if snapshot := stream.Snapshot(); snapshot.Terminal != StreamFailed || !errors.Is(snapshot.Err, cause) {
			t.Fatalf("latched writer failure changed: %+v", snapshot)
		}
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	g, err := New("error-reentry", "错误链读取流快照")
	managedTestOK(t, err)
	key := NewKey[Stream[int]]("events")
	managedTestOK(t, g.DeclareOutput(NewPort(key)))
	var stream Stream[int]
	managedTestOK(t, g.Add(NewNode("source", "完整链失败仍须收尾", nil, Provides(key), func(rc *RunCtx) error {
		_, err := OpenStream(rc, key, WithStreamSource("finite-source", false))
		if err != nil {
			return err
		}
		stream, err = Get(rc, key)
		if err != nil {
			return err
		}
		return &streamSnapshotError{stream: stream, cause: cause, reenter: reenter == "true"}
	})))
	var opts []ExecOption
	if path == "checkpoint-writer" {
		opts = append(opts, WithExecCheckpoint("root", checkpointStreamTestHooks(&checkpointProbe{})))
	} else if path != "local-writer" {
		t.Fatalf("unknown child path %q", path)
	}
	e, err := g.NewExecution(ctx, Inputs{}, opts...)
	managedTestOK(t, err)
	defer e.Cancel()
	_, err = SubscribeStream(e, key)
	managedTestOK(t, err)
	managedTestOK(t, e.Start())
	if err := e.Wait(); !errors.Is(err, cause) {
		t.Fatalf("business failure changed: %v", err)
	}
	if state := e.Snapshot(); state.Graph.Status != GraphStatusFailed {
		t.Fatalf("business failure became %s", state.Graph.Status)
	}
	if snapshot := stream.Snapshot(); snapshot.Terminal != StreamFailed || !errors.Is(snapshot.Err, cause) {
		t.Fatalf("writer terminal changed: %+v", snapshot)
	}
}
