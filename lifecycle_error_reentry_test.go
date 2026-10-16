package pulse

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

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
		e, err := g.NewExecution(t.Context(), Inputs{})
		managedTestOK(t, err)
		defer e.Cancel()
		var cause error
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
		if got := e.fail(cause); got != cause || e.Err() != cause {
			t.Fatalf("first error changed: %v", got)
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
