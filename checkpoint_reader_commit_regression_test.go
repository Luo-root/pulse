package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// 等待点由 Done 的求值证明，不靠 sleep 让来源“恰好晚到”。
// 同时钉住失败确认和成功 Commit：仅让最终 Wait 报错并不能防住坏检查点。
func TestCheckpointReaderObligationsBeforeCommit(t *testing.T) {
	for _, late := range []bool{false, true} {
		for _, skipped := range []bool{false, true} {
			t.Run(fmt.Sprintf("late=%t/skip=%t", late, skipped), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				key := NewKey[Stream[int]]("input")
				waiting, published := make(chan struct{}), make(chan struct{})
				var publishOnce sync.Once
				g, err := New("reader-before-commit", "", WithMaxRunning(1))
				managedTestOK(t, err)
				managedTestOK(t, g.Add(NewNode("source", "", nil, Provides(key), func(rc *RunCtx) error {
					if late {
						if err := readerLifecycleWait(rc, waiting); err != nil {
							return err
						}
					}
					writer, err := OpenStream(rc, key, WithStreamSource("reader-before-commit", true))
					if err != nil {
						return err
					}
					if err := writer.Send(1); err != nil {
						return err
					}
					publishOnce.Do(func() { close(published) })
					return nil
				})))
				managedTestOK(t, g.Add(NewNode("sink", "", Requires(key), nil, func(*RunCtx) error {
					return errors.New("short-circuited reader body ran")
				}, func(rc *RunCtx, _ func(*RunCtx) error) error {
					if late {
						rc.ctx = &managedWaitWitness{Context: rc.ctx, entered: waiting}
					} else if err := readerLifecycleWait(rc, published); err != nil {
						return err
					}
					if skipped {
						return NoValue("reader aspect")
					}
					return nil
				})))
				probe := &checkpointProbe{}
				hooks := checkpointStreamTestHooks(probe)
				err = g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
				if err == nil || !strings.Contains(err.Error(), "reading obligations") {
					t.Fatalf("Run=%v; want real reader-obligation failure", err)
				}
				probe.mu.Lock()
				committed := 0
				failed := false
				for _, commit := range probe.commits {
					if commit.Node.NodeID == "sink" {
						committed++
					}
				}
				for request, attempt := range probe.latest {
					if request.NodeID == "sink" && probe.failed[attempt.Scope] {
						failed = true
					}
				}
				probe.mu.Unlock()
				if committed != 0 || !failed {
					t.Fatalf("failed reader persisted: successful commits=%d, failure confirmed=%t", committed, failed)
				}
				// 同一持久尝试不能在下一轮变成已完成成果，也不能补回失败预算。
				err = g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
				if err == nil || !strings.Contains(err.Error(), "already failed") {
					t.Fatalf("failed reader attempt was reusable as success: %v", err)
				}
			})
		}
	}
}

func TestCheckpointReaderPendingAbsenceRemainsSuccessful(t *testing.T) {
	for _, shortSkip := range []bool{false, true} {
		t.Run(fmt.Sprint(shortSkip), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			key := NewKey[Stream[int]]("absent")
			waiting := make(chan struct{})
			g, err := New("reader-absence", "", WithMaxRunning(1))
			managedTestOK(t, err)
			managedTestOK(t, g.Add(NewNode("source", "", nil, Provides(key), func(rc *RunCtx) error {
				if err := readerLifecycleWait(rc, waiting); err != nil {
					return err
				}
				return Skip(rc, key)
			})))
			managedTestOK(t, g.Add(NewNode("sink", "", Requires(key), nil, nil,
				func(rc *RunCtx, _ func(*RunCtx) error) error {
					rc.ctx = &managedWaitWitness{Context: rc.ctx, entered: waiting}
					if shortSkip {
						return NoValue()
					}
					return nil
				})))
			probe := &checkpointProbe{}
			managedTestOK(t, g.Run(ctx, Inputs{}, WithExecCheckpoint("root", checkpointStreamTestHooks(probe))))
			probe.mu.Lock()
			defer probe.mu.Unlock()
			found := false
			for _, commit := range probe.commits {
				if commit.Node.NodeID == "sink" {
					found = true
					reason := NodeCompleted
					if shortSkip {
						reason = NodeSkipped
					}
					if commit.Node.Reason != reason {
						t.Fatalf("normal absence reason=%s, want %s", commit.Node.Reason, reason)
					}
				}
			}
			if !found {
				t.Fatal("normal absent reader was not confirmed")
			}
		})
	}
}
