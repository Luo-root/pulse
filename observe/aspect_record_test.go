package observe

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/Luo-root/pulse"
)

func TestRecordObserverAutomaticallyRecordsRetry(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "skip"}[skip], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			sink := &MemorySink{}
			obs, err := NewRecordObserver(testObsCfg(sink))
			if err != nil {
				t.Fatal(err)
			}
			legacy := &recordingObserver{} // 仅原五方法，没有可选接口。
			g := mustGraph(t, "auto-aspect")
			cause := errors.New("first attempt")
			calls := 0
			if err := g.Add(pulse.NewNode("n", "原有Retry用法", nil, nil, func(*pulse.RunCtx) error {
				calls++
				if skip {
					return pulse.NoValue()
				}
				if calls == 1 {
					return cause
				}
				return nil
			}, pulse.Retry(3, time.Millisecond))); err != nil {
				t.Fatal(err)
			}
			if err := g.Run(ctx, pulse.Inputs{}, pulse.WithExecObserver(pulse.MultiObserver{legacy, obs})); err != nil {
				t.Fatal(err)
			}
			if got := legacy.snapshot(); !reflect.DeepEqual(got, []string{"GS", "W", "R", "F", "GF"}) {
				t.Fatalf("legacy callbacks=%v", got)
			}
			var phases []string
			var execution string
			var segments int
			for _, r := range sink.Snapshot() {
				if r.Event != EventAspect {
					segments++
					continue
				}
				phase, _ := Get[string](r.Attrs, pulse.AttrAspectPhase)
				phases = append(phases, phase)
				graph, _ := Get[string](r.Attrs, pulse.AttrGraph)
				node, _ := Get[string](r.Attrs, pulse.AttrNode)
				exec, _ := Get[string](r.Attrs, pulse.AttrExecution)
				kind, _ := Get[string](r.Attrs, pulse.AttrAspect)
				boundary, _ := Get[int64](r.Attrs, pulse.AttrBoundary)
				limit, _ := Get[int64](r.Attrs, pulse.AttrAttemptLimit)
				if graph != "auto-aspect" || node != "n" || exec == "" || kind != "retry" || boundary != 1 || limit != 3 ||
					r.HostID != "h" || r.TraceID != "tr-f" || r.Source != SourceObserver || r.Time.IsZero() {
					t.Fatalf("record=%+v attrs=%v", r, r.Attrs)
				}
				if execution != "" && execution != exec {
					t.Fatalf("mixed identity %q/%q", execution, exec)
				}
				execution = exec
				if phase == "attempt_finished" {
					attempt, _ := Get[int64](r.Attrs, pulse.AttrAttempt)
					origin, _ := Get[string](r.Attrs, pulse.AttrAttemptOrigin)
					want := "completed"
					if skip {
						want = "skipped"
					} else if attempt == 1 {
						want = "failed"
					}
					if r.Status != want || origin != "new" || (r.Err != nil) != (want == "failed") {
						t.Fatalf("attempt result=%+v", r)
					}
					if want == "failed" && !errors.Is(r.Err, cause) {
						t.Fatalf("lost original failure: %v", r.Err)
					}
				}
				if phase == "backoff_finished" {
					delay, _ := Get[int64](r.Attrs, pulse.AttrRetryDelay)
					if delay != int64(time.Millisecond) || r.Duration <= 0 || r.Status != "completed" {
						t.Fatalf("backoff=%+v", r)
					}
				}
			}
			want := []string{"started", "attempt_started", "attempt_finished", "backoff_started", "backoff_finished", "retry", "attempt_started", "attempt_finished", "finished"}
			if skip {
				want = []string{"started", "attempt_started", "attempt_finished", "finished"}
			}
			if !reflect.DeepEqual(phases, want) || segments != 4 {
				t.Fatalf("phases=%v want=%v legacy records=%d", phases, want, segments)
			}
		})
	}
}

type aspectReadPanicSink struct {
	exec *pulse.Execution
}

func (s *aspectReadPanicSink) Write(r Record) {
	if r.Event == EventAspect {
		_ = s.exec.Snapshot()
		_ = s.exec.Err()
		panic(nil)
	}
}

func TestRecordObserverAspectSinkPanicAndReentrantSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sink := &aspectReadPanicSink{}
	obs, err := NewRecordObserver(ObserveConfig{Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	g := mustGraph(t, "sink-isolation")
	calls := 0
	if err := g.Add(pulse.NewNode("n", "坏出口不能改变业务", nil, nil, func(*pulse.RunCtx) error {
		calls++
		if calls == 1 {
			return errors.New("retry me")
		}
		return nil
	}, pulse.Retry(2, 0))); err != nil {
		t.Fatal(err)
	}
	sink.exec, err = g.NewExecution(ctx, pulse.Inputs{}, pulse.WithExecObserver(obs))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.exec.Start(); err != nil {
		t.Fatal(err)
	}
	if err := sink.exec.Wait(); err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestRecordObserverAspectTimeoutAndNamedRecords(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sink := &MemorySink{}
	obs, err := NewRecordObserver(testObsCfg(sink))
	if err != nil {
		t.Fatal(err)
	}
	g := mustGraph(t, "timeout-record")
	custom := pulse.NamedAspect("gate", func(rc *pulse.RunCtx, next func(*pulse.RunCtx) error) error { return next(rc) })
	if err := g.Add(pulse.NewNode("n", "具名包装与真实截止", nil, nil, func(rc *pulse.RunCtx) error {
		<-rc.Context().Done()
		return rc.Context().Err()
	}, custom, pulse.Timeout(time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(ctx, pulse.Inputs{}, pulse.WithExecObserver(obs)); err == nil {
		t.Fatal("timeout lost")
	}
	var phases []string
	for _, r := range sink.Snapshot() {
		if r.Event != EventAspect {
			continue
		}
		kind, _ := Get[string](r.Attrs, pulse.AttrAspect)
		phase, _ := Get[string](r.Attrs, pulse.AttrAspectPhase)
		phases = append(phases, kind+":"+phase)
		if kind == "custom" {
			name, _ := Get[string](r.Attrs, pulse.AttrAspectName)
			if name != "gate" {
				t.Fatalf("name=%q", name)
			}
		} else {
			parent, _ := Get[int64](r.Attrs, pulse.AttrParentBoundary)
			duration, _ := Get[int64](r.Attrs, pulse.AttrTimeout)
			if parent != 1 || duration != int64(time.Millisecond) {
				t.Fatalf("timeout attrs=%v", r.Attrs)
			}
		}
		if phase == "deadline" && (r.Status != "expired" || r.Err != nil) {
			t.Fatalf("deadline=%+v", r)
		}
	}
	want := []string{"custom:started", "timeout:started", "timeout:deadline", "timeout:finished", "custom:finished"}
	if !reflect.DeepEqual(phases, want) {
		t.Fatalf("phases=%v", phases)
	}
}

func TestRecordObserverCleanupDoesNotCancelRPCFailure(t *testing.T) {
	for _, named := range []bool{false, true} {
		for _, cleanup := range []bool{false, true} {
			for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
				t.Run(fmt.Sprintf("named=%t/cleanup=%t/%v", named, cleanup, cause), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
					defer cancel()
					sink := &MemorySink{}
					obs, err := NewRecordObserver(testObsCfg(sink))
					if err != nil {
						t.Fatal(err)
					}
					g := mustGraph(t, "rpc-cleanup-record")
					aspect := pulse.Retry(1, 0)
					if named {
						aspect = pulse.NamedAspect("rpc", func(rc *pulse.RunCtx, next func(*pulse.RunCtx) error) error { return next(rc) })
					}
					failure := fmt.Errorf("independent RPC: %w", cause)
					if err := g.Add(pulse.NewNode("n", "清理后仍记录真实RPC失败", nil, nil, func(rc *pulse.RunCtx) error {
						if cleanup {
							defer rc.Cancel()
						}
						return failure
					}, aspect)); err != nil {
						t.Fatal(err)
					}
					if err := g.Run(ctx, pulse.Inputs{}, pulse.WithExecObserver(obs)); !errors.Is(err, failure) {
						t.Fatalf("original failure lost: %v", err)
					}
					var terminals, lifecycle int
					for _, record := range sink.Snapshot() {
						if record.Event == EventNodeRunFinished || record.Event == EventGraphFinished {
							lifecycle++
							if record.Status != "failed" || !errors.Is(record.Err, failure) {
								t.Errorf("lifecycle failure changed: %+v", record)
							}
						}
						if record.Event != EventAspect {
							continue
						}
						phase, _ := Get[string](record.Attrs, pulse.AttrAspectPhase)
						if phase != "finished" && phase != "attempt_finished" {
							continue
						}
						terminals++
						if record.Status != "failed" || !errors.Is(record.Err, failure) {
							t.Errorf("cleanup changed aspect record: %+v", record)
						}
					}
					want := 2
					if named {
						want = 1
					}
					if terminals != want || lifecycle != 2 {
						t.Fatalf("records: aspect=%d want=%d lifecycle=%d", terminals, want, lifecycle)
					}
				})
			}
		}
	}
}
