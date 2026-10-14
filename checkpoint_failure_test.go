package pulse

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件独立于 checkpointProbe，失败确认与根预算的回归可随桥接实现一起落地。
func TestCheckpointFailureUsesExecutionContext(t *testing.T) {
	businessErr := errors.New("business failed")
	cases := []struct {
		name        string
		cause       error
		localCancel bool
		hostCancel  bool
	}{
		{name: "business", cause: businessErr},
		{name: "defer-cancel", cause: businessErr, localCancel: true},
		{name: "rpc-canceled", cause: fmt.Errorf("RPC: %w", context.Canceled)},
		{name: "rpc-deadline", cause: fmt.Errorf("RPC: %w", context.DeadlineExceeded)},
		{name: "host-cancel", hostCancel: true},
	}
	for _, tc := range cases {
		for _, retry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retry=%t", tc.name, retry), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				host, cancelHost := context.WithCancel(ctx)
				defer cancelHost()
				g := mustNew(t, ctx, "failure-context")
				var calls atomic.Int32
				var aspects []Aspect
				if retry {
					aspects = []Aspect{Retry(2, 0)}
				}
				mustAdd(t, g, NewNode("leaf", "业务清理与宿主取消分别判断", nil, nil, func(rc *RunCtx) error {
					call := calls.Add(1)
					if tc.localCancel {
						defer rc.Cancel()
					}
					if tc.hostCancel {
						if call == 1 {
							cancelHost()
							return rc.Context().Err()
						}
						return businessErr
					}
					return tc.cause
				}, aspects...))

				// 只模拟权威尝试游标；确认失败后才能推进，活跃游标跨 Execution 认回。
				var mu sync.Mutex
				var seq, failCalls, commits int
				latest := make(map[CheckpointAttemptRequest]CheckpointAttempt)
				failed := make(map[string]bool)
				var requests []CheckpointAttemptRequest
				var opened []CheckpointAttempt
				hooks := CheckpointHooks{
					OpenAttempt: func(ctx context.Context, req CheckpointAttemptRequest) (result CheckpointAttempt, err error) {
						if err := ctx.Err(); err != nil {
							return result, err
						}
						mu.Lock()
						defer mu.Unlock()
						req.Site.Execution = ""
						requests = append(requests, req)
						defer func() { opened = append(opened, result) }()
						result = latest[req]
						if result.Scope != "" && !failed[result.Scope] {
							return result, nil
						}
						if result.Index == req.Limit {
							result.Exhausted = true
							return result, nil
						}
						seq++
						result = CheckpointAttempt{Scope: fmt.Sprintf("attempt-%d", seq), Index: result.Index + 1}
						latest[req] = result
						return result, nil
					},
					FailAttempt: func(ctx context.Context, scope string) error {
						mu.Lock()
						defer mu.Unlock()
						failCalls++
						// 后端必须能遵守传入 ctx；不能靠一个忽略取消的钩子掩盖缺陷。
						if err := ctx.Err(); err != nil {
							return err
						}
						failed[scope] = true
						return nil
					},
					OpenChild: func(context.Context, CheckpointChild) ([]NodeCheckpoint, error) { return nil, nil },
					Commit: func(context.Context, CheckpointCommit) error {
						mu.Lock()
						defer mu.Unlock()
						commits++
						return nil
					},
				}
				firstErr := g.Run(host, Inputs{}, WithExecCheckpoint("root", hooks))
				var reliable *CheckpointError
				wantCause := tc.cause
				if tc.hostCancel {
					wantCause = context.Canceled
				}
				if !errors.Is(firstErr, wantCause) || errors.As(firstErr, &reliable) {
					t.Fatalf("business/interruption cause replaced by checkpoint failure: %v", firstErr)
				}
				if (host.Err() != nil) != tc.hostCancel {
					t.Fatalf("host context = %v, hostCancel = %t", host.Err(), tc.hostCancel)
				}
				wantCalls, wantFailures := int32(1), 1
				if retry && !tc.localCancel && !tc.hostCancel {
					wantCalls, wantFailures = 2, 3 // 两次 Retry 失败，加完整链根失败。
				}
				if tc.hostCancel {
					wantFailures = 0
				}
				if calls.Load() != wantCalls || failCalls != wantFailures || len(failed) != wantFailures || commits != 0 {
					t.Fatalf("calls=%d fail hooks=%d confirmed=%d commits=%d; want calls=%d failures=%d", calls.Load(), failCalls, len(failed), commits, wantCalls, wantFailures)
				}
				root := opened[0]
				if requests[0].Boundary != "" || requests[0].Limit != 1 || root.Index != 1 || failed[root.Scope] == tc.hostCancel {
					t.Fatalf("incorrect root budget/state: request=%+v cursor=%+v failed=%t", requests[0], root, failed[root.Scope])
				}

				resumeStart := len(opened)
				resumeErr := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
				if resumeErr == nil || errors.As(resumeErr, &reliable) {
					t.Fatalf("unexpected resume result: %v", resumeErr)
				}
				resumed := opened[resumeStart]
				if resumed.Scope != root.Scope || resumed.Index != root.Index {
					t.Fatalf("resume replaced root cursor: before=%+v after=%+v", root, resumed)
				}
				if tc.hostCancel {
					wantCalls++
					wantFailures = 1
					if retry {
						wantCalls++
						wantFailures = 3
						if opened[resumeStart+1] != opened[1] {
							t.Fatalf("resume did not recover active Retry cursor: before=%+v after=%+v", opened[1], opened[resumeStart+1])
						}
					}
					if !errors.Is(resumeErr, businessErr) || resumed.Exhausted {
						t.Fatalf("host interruption was made terminal: %v cursor=%+v", resumeErr, resumed)
					}
				} else if !resumed.Exhausted {
					t.Fatalf("confirmed business failure replenished root budget: %+v", resumed)
				}
				if calls.Load() != wantCalls || failCalls != wantFailures || len(failed) != wantFailures || commits != 0 {
					t.Fatalf("resume calls=%d fail hooks=%d confirmed=%d commits=%d; want calls=%d failures=%d", calls.Load(), failCalls, len(failed), commits, wantCalls, wantFailures)
				}
			})
		}
	}
}
