package pulse

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestOnlyPreflightWriteFrames(t *testing.T) {
	for _, test := range []struct {
		name       string
		checkpoint bool
		depth      int
		outer      bool
	}{
		{"ordinary", false, 0, false},
		{"retry-current", false, 1, false},
		{"retry-final-slot", false, 1, true},
		{"nested-current", false, 2, false},
		{"nested-outer", false, 2, true},
		{"checkpoint-current", true, 0, false},
		{"checkpoint-outer", true, 1, true},
		{"checkpoint-nested", true, 2, true},
	} {
		for _, skipped := range []bool{false, true} {
			for _, recoverConflict := range []bool{false, true} {
				name := fmt.Sprintf("%s/skipped=%t/recover=%t", test.name, skipped, recoverConflict)
				t.Run(name, func(t *testing.T) {
					ctx := checkpointTestContext(t)
					g := mustNew(t, ctx, "only-frame")
					a, b := NewKey[int]("a"), NewKey[int]("b")
					declareOut(t, g, NewPort(a))
					declareOut(t, g, NewPort(b))
					writeSibling := func(rc *RunCtx) error {
						if skipped {
							return Skip(rc, b)
						}
						return Set(rc, b, 2)
					}
					inspect := func(rc *RunCtx, next func(*RunCtx) error) error {
						err := next(rc)
						if !skipped && errors.Is(err, ErrConflict) {
							if _, ready, absent, rerr := TryGet(rc, a); rerr != nil || ready || absent {
								return fmt.Errorf("inner aspect saw selected output after conflict: ready=%t skipped=%t err=%v", ready, absent, rerr)
							}
							if recoverConflict {
								return nil
							}
						}
						return err
					}
					var aspects []Aspect
					for i := 0; i < test.depth; i++ {
						if test.outer && i == test.depth-1 {
							aspects = append(aspects, func(rc *RunCtx, next func(*RunCtx) error) error {
								if err := writeSibling(rc); err != nil {
									return err
								}
								return next(rc)
							})
						}
						aspects = append(aspects, Retry(1, 0))
					}
					aspects = append(aspects, inspect)
					mustAdd(t, g, NewNode("route", "", nil, Provides(a, b), func(rc *RunCtx) error {
						if !test.outer {
							if err := writeSibling(rc); err != nil {
								return err
							}
						}
						before := make(map[*writeSet]int)
						for ws := rc.writes; ws != nil; ws = ws.outer {
							ws.mu.Lock()
							before[ws] = len(ws.staged)
							ws.mu.Unlock()
						}
						err := Only(rc, a, 1)
						if skipped {
							return err
						}
						if !errors.Is(err, ErrConflict) {
							return fmt.Errorf("Only must reject an already-ready sibling before writing: %v", err)
						}
						if _, ready, absent, rerr := TryGet(rc, a); rerr != nil || ready || absent {
							return fmt.Errorf("Only changed selected output before rejecting sibling: ready=%t skipped=%t err=%v", ready, absent, rerr)
						}
						for ws, count := range before {
							ws.mu.Lock()
							after := len(ws.staged)
							ws.mu.Unlock()
							if after != count {
								return fmt.Errorf("Only preflight created write entries: before=%d after=%d", count, after)
							}
						}
						return err
					}, aspects...))
					var options []ExecOption
					if test.checkpoint {
						options = append(options, WithExecCheckpoint("root", (&checkpointProbe{}).hooks()))
					}
					e, err := g.NewExecution(ctx, Inputs{}, options...)
					if err != nil {
						t.Fatal(err)
					}
					if err := e.Start(); err != nil {
						t.Fatal(err)
					}
					err = e.Wait()
					if !skipped && !recoverConflict {
						if !errors.Is(err, ErrConflict) {
							t.Fatalf("Wait = %v, want original conflict", err)
						}
						if state, _ := e.slotOf(a.asRef()).snapshot(); state == slotReady {
							t.Fatal("failed Only published its selected output")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					got, state, err := ReadOutput(e, a)
					if skipped {
						if err != nil || state != SlotStateReady || got != 1 {
							t.Fatalf("idempotent sibling skip rejected: (%d, %s, %v)", got, state, err)
						}
					} else if err != nil || state != SlotStateSkipped {
						t.Fatalf("recovered conflict retained selected output: (%d, %s, %v)", got, state, err)
					}
					got, state, err = ReadOutput(e, b)
					if skipped {
						if err != nil || state != SlotStateSkipped {
							t.Fatalf("sibling skip changed: (%d, %s, %v)", got, state, err)
						}
					} else if err != nil || state != SlotStateReady || got != 2 {
						t.Fatalf("recovered sibling value changed: (%d, %s, %v)", got, state, err)
					}
				})
			}
		}
	}
}

func TestOnlyPreflightRestoredCheckpointSibling(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint("retry=", retry), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "only-restored-sibling")
			a, b := NewKey[int]("a"), NewKey[int]("b")
			declareOut(t, g, NewPort(a))
			var aspects []Aspect
			if retry {
				aspects = append(aspects, Retry(1, 0))
			}
			mustAdd(t, g, NewNode("route", "", nil, Provides(a, b), func(rc *RunCtx) error {
				err := Only(rc, a, 1)
				if !errors.Is(err, ErrConflict) {
					return fmt.Errorf("Only ignored restored ready sibling: %v", err)
				}
				if _, ready, skipped, err := TryGet(rc, a); err != nil || ready || skipped {
					return fmt.Errorf("recovered conflict retained a: ready=%t skipped=%t err=%v", ready, skipped, err)
				}
				return nil // 恢复冲突也不能把 Only 失败前的新写入提交出去。
			}, aspects...))
			hooks := (&checkpointProbe{}).hooks()
			open := hooks.OpenAttempt
			hooks.OpenAttempt = func(ctx context.Context, req CheckpointAttemptRequest) (CheckpointAttempt, error) {
				attempt, err := open(ctx, req)
				if req.Boundary == "" {
					attempt.Values = []CheckpointOutput{{Key: checkpointKey(b.asRef()), State: SlotStateReady, Value: 2}}
				}
				return attempt, err
			}
			e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			if err := e.Wait(); err != nil {
				t.Fatal(err)
			}
			if _, state, err := ReadOutput(e, a); err != nil || state != SlotStateSkipped {
				t.Fatalf("recovered Only published a: state=%s err=%v", state, err)
			}
		})
	}
}

func TestOnlyPreflightChecksShadowedOuterWrites(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "only-shadowed")
	a, b := NewKey[int]("a"), NewKey[int]("b")
	outer := func(rc *RunCtx, next func(*RunCtx) error) error {
		if err := Set(rc, b, 2); err != nil {
			return err
		}
		return next(rc)
	}
	mustAdd(t, g, NewNode("route", "", nil, Provides(a, b), func(rc *RunCtx) error {
		// 本层 skipped 不能遮住最终提交时仍会撞到的外层 ready。
		if err := Skip(rc, b); err != nil {
			return err
		}
		err := Only(rc, a, 1)
		if !errors.Is(err, ErrConflict) {
			return fmt.Errorf("Only ignored shadowed ready sibling: %v", err)
		}
		if _, ready, skipped, rerr := TryGet(rc, a); rerr != nil || ready || skipped {
			return fmt.Errorf("Only changed selected output: ready=%t skipped=%t err=%v", ready, skipped, rerr)
		}
		return err
	}, Retry(1, 0), outer, Retry(1, 0)))
	if err := g.Run(ctx, Inputs{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Wait = %v, want conflict without a selected output", err)
	}
}
