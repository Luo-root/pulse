package observe

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/Luo-root/pulse"
)

// 正常跳过覆盖两段：NoValue 在 Run 内收尾，全输入 skipped 在等待后收尾。
// ready 对照同时固定出口值、常规记录数与下游执行次数。
func TestRecordObserverNormalTerminalRecords(t *testing.T) {
	for _, mode := range []string{"no-value", "all-inputs-skipped", "ready"} {
		t.Run(mode, func(t *testing.T) {
			sink := &MemorySink{}
			obs, err := NewRecordObserver(testObsCfg(sink))
			if err != nil {
				t.Fatal(err)
			}
			g := mustGraph(t, "normal-terminal", pulse.WithObserver(obs))
			out := pulse.NewKey[int]("out")
			var inputs pulse.Inputs
			requires := pulse.Requires[int]()
			if mode == "all-inputs-skipped" {
				a, b := pulse.NewKey[int]("a"), pulse.NewKey[int]("b")
				skipIn(t, g, &inputs, a)
				skipIn(t, g, &inputs, b)
				requires = pulse.Requires(a, b)
			}
			var sourceCalls, consumerCalls int
			if err := g.Add(pulse.NewNode("source", "正常终态", requires, pulse.Provides(out), func(rc *pulse.RunCtx) error {
				sourceCalls++
				if mode == "no-value" {
					return pulse.NoValue(out.Name())
				}
				return pulse.Set(rc, out, 42)
			})); err != nil {
				t.Fatal(err)
			}
			if err := g.Add(pulse.NewNode("consumer", "跳过不执行", pulse.Requires(out), nil, func(*pulse.RunCtx) error {
				consumerCalls++
				return nil
			})); err != nil {
				t.Fatal(err)
			}
			if err := g.DeclareOutput(pulse.NewPort(out)); err != nil {
				t.Fatal(err)
			}
			e, err := g.NewExecution(context.Background(), inputs)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			if err := e.Wait(); err != nil || e.Err() != nil {
				t.Fatalf("正常终态 Wait=%v Err=%v", err, e.Err())
			}
			value, state, err := pulse.ReadOutput(e, out)
			wantState, wantValue, wantCalls, wantRecords := pulse.SlotStateSkipped, 0, 0, 5
			if mode == "ready" {
				wantState, wantValue, wantCalls, wantRecords = pulse.SlotStateReady, 42, 1, 6
			} else if mode == "all-inputs-skipped" {
				wantRecords = 4
			}
			if err != nil || value != wantValue || state != wantState || consumerCalls != wantCalls {
				t.Fatalf("ReadOutput=(%d,%s,%v) consumer=%d, want (%d,%s,nil) consumer=%d", value, state, err, consumerCalls, wantValue, wantState, wantCalls)
			}
			wantSourceCalls := 1
			if mode == "all-inputs-skipped" {
				wantSourceCalls = 0
			}
			if sourceCalls != wantSourceCalls {
				t.Fatalf("source calls=%d, want %d", sourceCalls, wantSourceCalls)
			}
			recs := sink.Snapshot()
			if len(recs) != wantRecords {
				t.Fatalf("records=%v, want %d 条", eventsOf(recs), wantRecords)
			}
			var skippedWait, skippedRun int
			for _, rec := range recs {
				if rec.Err != nil {
					t.Errorf("正常终态记录被标成失败: event=%s status=%s Err=%v", rec.Event, rec.Status, rec.Err)
				}
				if rec.Event == EventNodeFinished {
					t.Errorf("已观测分段的节点不该补终态记录: %+v", rec)
				}
				if rec.Status == string(pulse.NodeSkipped) {
					switch rec.Event {
					case EventNodeWaitFinished:
						skippedWait++
					case EventNodeRunFinished:
						skippedRun++
					}
				}
			}
			wantSkippedWait, wantSkippedRun := 0, 0
			if mode == "no-value" {
				wantSkippedWait, wantSkippedRun = 1, 1
			} else if mode == "all-inputs-skipped" {
				wantSkippedWait = 2
			}
			if skippedWait != wantSkippedWait || skippedRun != wantSkippedRun {
				t.Fatalf("skipped wait/run=%d/%d, want %d/%d", skippedWait, skippedRun, wantSkippedWait, wantSkippedRun)
			}
			last := recs[len(recs)-1]
			if last.Event != EventGraphFinished || last.Status != string(pulse.NodeCompleted) {
				t.Fatalf("整轮必须正常完成: %+v", last)
			}
		})
	}
}

// 以引擎给出的 reason 为准，不要求正常跳过的诊断错误有某个哨兵形状。
func TestRecordObserverSkippedReasonClearsDiagnostic(t *testing.T) {
	for _, ran := range []bool{false, true} {
		t.Run(fmt.Sprint(ran), func(t *testing.T) {
			sink := &MemorySink{}
			obs, err := NewRecordObserver(testObsCfg(sink))
			if err != nil {
				t.Fatal(err)
			}
			site := pulse.Site{Graph: "reason", Execution: "reason#1"}
			obs.OnNodeWaiting(site, "n")
			if ran {
				obs.OnNodeRunning(site, "n")
			}
			obs.OnNodeFinished(site, "n", pulse.NodeSkipped, errors.New("missing input diagnostic"))
			recs := sink.Snapshot()
			last := recs[len(recs)-1]
			if last.Status != string(pulse.NodeSkipped) || last.Err != nil {
				t.Fatalf("正常 skipped 不得带失败字段: %+v", last)
			}
		})
	}
}

// 错误链含 ErrSkipped 的可靠失败仍是 failed；同时覆盖 Finished-only 与 Run 段。
func TestRecordObserverReliableSkippedErrorRemainsFailure(t *testing.T) {
	for _, stage := range []string{"open", "commit"} {
		t.Run(stage, func(t *testing.T) {
			sink := &MemorySink{}
			obs, err := NewRecordObserver(testObsCfg(sink))
			if err != nil {
				t.Fatal(err)
			}
			g := mustGraph(t, "reliable-terminal", pulse.WithObserver(obs))
			out := pulse.NewKey[int]("out")
			if err := g.Add(pulse.NewNode("n", "可靠失败不是跳过", nil, pulse.Provides(out), func(rc *pulse.RunCtx) error {
				return pulse.Set(rc, out, 7)
			})); err != nil {
				t.Fatal(err)
			}
			hooks := recordTerminalCheckpointHooks()
			cause := fmt.Errorf("uncertain storage: %w", pulse.ErrSkipped)
			if stage == "open" {
				hooks.OpenAttempt = func(context.Context, pulse.CheckpointAttemptRequest) (pulse.CheckpointAttempt, error) {
					return pulse.CheckpointAttempt{}, cause
				}
			} else {
				hooks.Commit = func(context.Context, pulse.CheckpointCommit) error { return cause }
			}
			e, err := g.NewExecution(context.Background(), pulse.Inputs{}, pulse.WithExecCheckpoint("root", hooks))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			err = e.Wait()
			var failure *pulse.CheckpointError
			if !errors.As(err, &failure) || !errors.Is(err, pulse.ErrSkipped) {
				t.Fatalf("丢失可靠失败原因: %v", err)
			}
			if snap := e.Snapshot(); snap.Graph.Status != pulse.GraphStatusFailed {
				t.Fatalf("可靠失败必须保留 failed 终态: %+v", snap)
			}
			recs := sink.Snapshot()
			nodeRecs := recordTerminalNodeRecords(recs, "n")
			wantRecords, wantEvent := 2, EventNodeRunFinished
			if stage == "open" {
				wantRecords, wantEvent = 1, EventNodeFinished
			}
			if len(nodeRecs) != wantRecords {
				t.Fatalf("node records=%v, want %d", eventsOf(nodeRecs), wantRecords)
			}
			terminal := nodeRecs[len(nodeRecs)-1]
			if terminal.Event != wantEvent || terminal.Status != string(pulse.NodeFailed) || !errors.As(terminal.Err, &failure) || !errors.Is(terminal.Err, cause) {
				t.Fatalf("可靠失败记录被丢弃或降成 skipped: %+v", terminal)
			}
			last := recs[len(recs)-1]
			if last.Event != EventGraphFinished || last.Status != string(pulse.NodeFailed) || !errors.Is(last.Err, cause) {
				t.Fatalf("整轮失败记录丢失: %+v", last)
			}
		})
	}
}

// guard 在 next 之前拒绝，合法父子端口绑定保证失败来自 guard 而不是装配。
func TestRecordObserverFinishedOnlyGuard(t *testing.T) {
	for _, mounted := range []bool{false, true} {
		for _, reason := range []pulse.NodeFinishReason{pulse.NodeFailed, pulse.NodeCanceled} {
			t.Run(fmt.Sprintf("mounted=%t/%s", mounted, reason), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				sink := &MemorySink{}
				obs, err := NewRecordObserver(testObsCfg(sink))
				if err != nil {
					t.Fatal(err)
				}
				var site pulse.Site
				var finished, bodyCalls int
				host := pulse.ObserverFunc{
					Waiting: func(_ pulse.Site, id string) {
						if id == "guarded" {
							t.Error("guard 不应发 Waiting")
						}
					},
					Running: func(_ pulse.Site, id string) {
						if id == "guarded" {
							t.Error("guard 不应发 Running")
						}
					},
					Finished: func(got pulse.Site, id string, why pulse.NodeFinishReason, _ error) {
						if id == "guarded" {
							site, finished = got, finished+1
							if why != reason {
								t.Errorf("guard reason=%s, want %s", why, reason)
							}
						}
					},
				}
				g := mustGraph(t, "guard-root", pulse.WithObserver(pulse.MultiObserver{obs, host}))
				in, out := pulse.NewKey[int]("in"), pulse.NewKey[int]("out")
				var inputs pulse.Inputs
				feedIn(t, g, &inputs, in, 3)
				cause := errors.New("guard denied")
				if reason == pulse.NodeCanceled {
					cause = context.Canceled
				}
				guard := func(*pulse.RunCtx, func(*pulse.RunCtx) error) error {
					if reason == pulse.NodeCanceled {
						cancel()
					}
					return cause
				}
				target, nodeIn, nodeOut := g, in, out
				if mounted {
					target = mustGraph(t, "guard-child")
					nodeIn, nodeOut = pulse.NewKey[int]("child.in"), pulse.NewKey[int]("child.out")
					if err := target.DeclareInput(pulse.NewPort(nodeIn)); err != nil {
						t.Fatal(err)
					}
					if err := target.DeclareOutput(pulse.NewPort(nodeOut)); err != nil {
						t.Fatal(err)
					}
				}
				if err := target.Add(pulse.NewNode("guarded", "提前终止", pulse.Requires(nodeIn), pulse.Provides(nodeOut), func(*pulse.RunCtx) error {
					bodyCalls++
					return nil
				}, guard)); err != nil {
					t.Fatal(err)
				}
				if mounted {
					if err := pulse.Sub(g, "mount", "合法绑定", []pulse.SubBind{pulse.In(in, nodeIn), pulse.Out(nodeOut, out)}, target); err != nil {
						t.Fatal(err)
					}
				}
				e, err := g.NewExecution(ctx, inputs)
				if err != nil {
					t.Fatal(err)
				}
				if err := e.Start(); err != nil {
					t.Fatal(err)
				}
				if err := e.Wait(); !errors.Is(err, cause) {
					t.Fatalf("Wait=%v, want %v", err, cause)
				}
				if bodyCalls != 0 || finished != 1 {
					t.Fatalf("body=%d Finished=%d, want 0/1", bodyCalls, finished)
				}
				recs := sink.Snapshot()
				wantRecords := 3
				if mounted {
					wantRecords = 7
				}
				if len(recs) != wantRecords {
					t.Fatalf("records=%v, want %d", eventsOf(recs), wantRecords)
				}
				nodeRecs := recordTerminalNodeRecords(recs, "guarded")
				if len(nodeRecs) != 1 {
					t.Fatalf("Finished-only guard records=%v, want 一条终态", eventsOf(nodeRecs))
				}
				rec := nodeRecs[0]
				if rec.Event != "pulse.node_finished" || rec.Status != string(reason) || rec.Duration != 0 || !errors.Is(rec.Err, cause) {
					t.Fatalf("未测量终态记录错误: %+v", rec)
				}
				for key, want := range map[string]string{pulse.AttrGraph: target.ID(), pulse.AttrNode: "guarded", pulse.AttrExecution: site.Execution} {
					if got, ok := Get[string](rec.Attrs, key); !ok || got != want || want == "" {
						t.Errorf("归因 %s=(%q,%t), want %q", key, got, ok, want)
					}
				}
				path, hasPath := Get[string](rec.Attrs, pulse.AttrPath)
				if path != site.Path || hasPath != mounted || (mounted && path != "mount") || (!mounted && site.Execution != e.ID()) {
					t.Errorf("挂载位置/执行身份错误: path=(%q,%t) site=%v root=%s", path, hasPath, site, e.ID())
				}
				if rec.HostID != "h" || rec.TraceID != "tr-f" || rec.Source != SourceObserver {
					t.Errorf("终态信封不同源: %+v", rec)
				}
			})
		}
	}
}

// 成功/跳过短路与恢复都只发 Finished；不编造计时或凭回调猜恢复来源。
func TestRecordObserverFinishedOnlySuccessBoundary(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, reason := range []pulse.NodeFinishReason{pulse.NodeCompleted, pulse.NodeSkipped} {
			t.Run(fmt.Sprintf("restored=%t/%s", restored, reason), func(t *testing.T) {
				sink := &MemorySink{}
				obs, err := NewRecordObserver(testObsCfg(sink))
				if err != nil {
					t.Fatal(err)
				}
				var bodies, aspects, hooksCalled, finished int
				host := pulse.ObserverFunc{
					Waiting: func(pulse.Site, string) { t.Error("Finished-only 节点不得发 Waiting") },
					Running: func(pulse.Site, string) { t.Error("Finished-only 节点不得发 Running") },
					Finished: func(_ pulse.Site, _ string, why pulse.NodeFinishReason, _ error) {
						finished++
						if why != reason {
							t.Errorf("reason=%s, want %s", why, reason)
						}
					},
				}
				g := mustGraph(t, "finished-only", pulse.WithObserver(pulse.MultiObserver{obs, host}))
				out := pulse.NewKey[int]("out")
				short := func(*pulse.RunCtx, func(*pulse.RunCtx) error) error {
					aspects++
					if reason == pulse.NodeSkipped {
						return pulse.NoValue()
					}
					return nil
				}
				if err := g.Add(pulse.NewNode("n", "短路或恢复", nil, pulse.Provides(out), func(*pulse.RunCtx) error { bodies++; return nil }, short)); err != nil {
					t.Fatal(err)
				}
				hooks := recordTerminalCheckpointHooks()
				open := hooks.OpenAttempt
				hooks.OpenAttempt = func(ctx context.Context, req pulse.CheckpointAttemptRequest) (pulse.CheckpointAttempt, error) {
					hooksCalled++
					return open(ctx, req)
				}
				hooks.Commit = func(context.Context, pulse.CheckpointCommit) error { hooksCalled++; return nil }
				hooks.FailAttempt = func(context.Context, string) error { t.Error("正常终态不得 FailAttempt"); return nil }
				hooks.OpenChild = func(context.Context, pulse.CheckpointChild) ([]pulse.NodeCheckpoint, error) {
					t.Error("没有子图")
					return nil, nil
				}
				e, err := g.NewExecution(context.Background(), pulse.Inputs{}, pulse.WithExecCheckpoint("root", hooks))
				if err != nil {
					t.Fatal(err)
				}
				if restored {
					output := pulse.CheckpointOutput{Key: pulse.CheckpointKey{Name: out.Name(), Type: reflect.TypeFor[int]()}, State: pulse.SlotStateSkipped}
					if reason == pulse.NodeCompleted {
						output.State, output.Value = pulse.SlotStateReady, 42
					}
					if err := e.RestoreNodes([]pulse.NodeCheckpoint{{NodeID: "n", Reason: reason, Outputs: []pulse.CheckpointOutput{output}}}); err != nil {
						t.Fatal(err)
					}
				}
				if err := e.Start(); err != nil {
					t.Fatal(err)
				}
				if err := e.Wait(); err != nil {
					t.Fatal(err)
				}
				wantAspects, wantHooks := 1, 2
				if restored {
					wantAspects, wantHooks = 0, 0
				}
				if bodies != 0 || aspects != wantAspects || hooksCalled != wantHooks || finished != 1 {
					t.Fatalf("body/aspect/hooks/Finished=%d/%d/%d/%d, want 0/%d/%d/1", bodies, aspects, hooksCalled, finished, wantAspects, wantHooks)
				}
				recs := sink.Snapshot()
				if len(recs) != 2 || recs[0].Event != EventGraphStarted || recs[1].Event != EventGraphFinished || recs[1].Status != string(pulse.NodeCompleted) || recs[1].Err != nil {
					t.Fatalf("成功/跳过 Finished-only 不得编造节点记录: %v", eventsOf(recs))
				}
			})
		}
	}
}

func recordTerminalNodeRecords(recs []Record, id string) []Record {
	var out []Record
	for _, rec := range recs {
		if node, ok := Get[string](rec.Attrs, pulse.AttrNode); ok && node == id {
			out = append(out, rec)
		}
	}
	return out
}

func recordTerminalCheckpointHooks() pulse.CheckpointHooks {
	return pulse.CheckpointHooks{
		OpenAttempt: func(_ context.Context, req pulse.CheckpointAttemptRequest) (pulse.CheckpointAttempt, error) {
			return pulse.CheckpointAttempt{Scope: req.Parent + "/" + req.NodeID, Index: 1}, nil
		},
		FailAttempt: func(context.Context, string) error { return nil },
		OpenChild:   func(context.Context, pulse.CheckpointChild) ([]pulse.NodeCheckpoint, error) { return nil, nil },
		Commit:      func(context.Context, pulse.CheckpointCommit) error { return nil },
	}
}
