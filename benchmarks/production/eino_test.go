package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/compose"
)

func TestEinoValuesReuse(t *testing.T) {
	for _, shape := range []string{"chain", "wide", "diamonds"} {
		for _, n := range []int{1, 2, 7, 32} {
			for _, mode := range []string{"plain", "observe"} {
				t.Run(fmt.Sprintf("%s/%d/%s", shape, n, mode), func(t *testing.T) {
					spec, err := newGraph(shape, n)
					if err != nil {
						t.Fatal(err)
					}
					unchanged, _ := newGraph(shape, n)
					cfg := Config{Mode: mode, Rounds: 256, Parallelism: 4}
					p := newProbe(n)
					r, err := buildEino(context.Background(), spec, cfg, p)
					if err != nil {
						t.Fatal(err)
					}
					defer r.close()
					want, sum := spec.reference(cfg.Rounds)
					for repeat := 0; repeat < 2; repeat++ {
						got, err := r.run(context.Background())
						if err != nil || got != sum {
							t.Fatalf("run: sum=%d want=%d err=%v", got, sum, err)
						}
						if err := p.check(want, cfg.Parallelism); err != nil {
							t.Fatal(err)
						}
						p.reset()
					}
					if !reflect.DeepEqual(spec, unchanged) {
						t.Fatal("adapter mutated shared graph spec")
					}
					observations := r.(*einoRunner).observations()
					if mode == "plain" {
						if len(observations) != 0 || len(r.(*einoRunner).options) != 0 {
							t.Fatal("plain installed callbacks")
						}
					} else {
						wantCounts := map[string]int64{"graph_start": 2, "graph_end": 2, "graph_error": 0,
							"node_start": int64(2 * n), "node_end": int64(2 * n), "node_error": 0}
						if !reflect.DeepEqual(observations, wantCounts) {
							t.Fatalf("callbacks=%v want=%v", observations, wantCounts)
						}
					}
				})
			}
		}
	}
}

func TestEinoInputsAndConcurrentReuse(t *testing.T) {
	spec, _ := newGraph("diamonds", 10)
	r, err := buildEinoGraph(context.Background(), spec, Config{Mode: "plain", Rounds: 1, Parallelism: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 不同 seed 的真实输入必须影响各轮结果，不能由共享结果数组或预计算结果代替。
	var wg sync.WaitGroup
	for seed := uint64(2); seed < 6; seed++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := r.invoke(context.Background(), map[string]any{"seed": seed})
			if err != nil {
				t.Error(err)
				return
			}
			values := make([]uint64, len(spec.names))
			for id, preds := range spec.predecessors {
				input := uint64(1)
				if len(preds) == 0 {
					input = seed
				}
				for _, from := range preds {
					input += values[from]
				}
				values[id] = compute(input, id, 1)
			}
			for _, id := range spec.terminals {
				if out[spec.names[id]] != values[id] {
					t.Errorf("seed=%d terminal=%s got=%v want=%d", seed, spec.names[id], out, values[id])
				}
			}
		}()
	}
	wg.Wait()
	for _, input := range []map[string]any{
		{"n1": uint64(2)},
		{"n1": uint64(2), "n2": "wrong type"},
		{"n1": uint64(2), "n2": uint64(3), "n0": uint64(4)},
	} {
		if _, err := einoNodeInput(spec, 3, input); err == nil {
			t.Fatalf("accepted invalid direct-predecessor input %v", input)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled run: %v", err)
	}
}

// 用公开 Eino 图和通道控制错误/取消窗口；测试业务体有意等到宿主取消。
func einoLifecycleRunner(t *testing.T, p *probe, body func(context.Context, int) error) *einoRunner {
	t.Helper()
	spec, _ := newGraph("wide", 2)
	g := compose.NewGraph[map[string]any, map[string]any]()
	for id, name := range spec.names {
		err := g.AddLambdaNode(name, compose.InvokableLambda(func(ctx context.Context, _ map[string]any) (uint64, error) {
			scope, err := enterEinoBody(ctx)
			if err != nil {
				return 0, err
			}
			defer scope.leave()
			p.enter(id)
			defer p.leave()
			return 0, body(ctx, id)
		}), compose.WithOutputKey(name), compose.WithNodeName(name))
		if err != nil {
			t.Fatal(err)
		}
		if err := g.AddEdge(compose.START, name); err != nil {
			t.Fatal(err)
		}
		if err := g.AddEdge(name, compose.END); err != nil {
			t.Fatal(err)
		}
	}
	graph, err := g.Compile(context.Background(), compose.WithNodeTriggerMode(compose.AllPredecessor))
	if err != nil {
		t.Fatal(err)
	}
	return &einoRunner{graph: graph, spec: spec, parallelism: 2}
}

func TestEinoNativeErrorAndHostCleanup(t *testing.T) {
	for _, host := range []bool{false, true} {
		t.Run(fmt.Sprintf("host=%v", host), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			p := newProbe(2)
			boom := errors.New("eino test business failure")
			r := einoLifecycleRunner(t, p, func(ctx context.Context, id int) error {
				if id == 0 {
					select {
					case <-entered:
						return boom
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-release
				return ctx.Err()
			})
			done := make(chan error, 1)
			scope := &einoRunScope{slots: make(chan struct{}, 2)}
			go func() {
				if host {
					_, err := r.run(ctx)
					done <- err
				} else {
					nativeCtx := context.WithValue(ctx, einoRunScopeKey{}, scope)
					_, err := r.graph.Invoke(nativeCtx, map[string]any{"seed": uint64(1)})
					done <- err
				}
			}()
			if host {
				select {
				case <-canceled: // 只有宿主的错误收尾能在 caller context 到期之前触发。
				case <-ctx.Done():
					t.Fatal("host did not cancel sibling after node failure")
				}
				select {
				case err := <-done:
					t.Fatalf("host returned before admitted body exited: %v", err)
				default:
				}
				unblock()
			} else {
				select {
				case err := <-done:
					if !errors.Is(err, boom) || p.active.Load() != 1 {
						t.Fatalf("native boundary: err=%v active=%d", err, p.active.Load())
					}
				case <-ctx.Done():
					t.Fatal("expected v0.9.21 eager node-error return")
				}
				cancel()
				unblock()
				scope.bodies.Wait()
				return
			}
			select {
			case err := <-done:
				if !errors.Is(err, boom) || p.active.Load() != 0 {
					t.Fatalf("host cleanup: err=%v active=%d", err, p.active.Load())
				}
			case <-ctx.Done():
				t.Fatal("host cleanup did not finish")
			}
		})
	}
}

func TestEinoCancellationAndClosedAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{}, 2)
	p := newProbe(2)
	r := einoLifecycleRunner(t, p, func(ctx context.Context, _ int) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	done := make(chan error, 1)
	go func() { _, err := r.run(ctx); done <- err }()
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("business bodies did not enter")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || p.active.Load() != 0 {
			t.Fatalf("cancellation: err=%v active=%d", err, p.active.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation cleanup did not finish")
	}
	// 已关闭的 run 准入不能被迟到的 Eino lambda 重新打开。
	scope := &einoRunScope{closed: true, slots: make(chan struct{}, 1)}
	lateCtx := context.WithValue(context.Background(), einoRunScopeKey{}, scope)
	if _, err := enterEinoBody(lateCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("late body admission: %v", err)
	}
}

type einoMemoryCheckpoints struct {
	mu     sync.Mutex
	blobs  map[string][]byte
	writes int
	setErr error
}

func (s *einoMemoryCheckpoints) Get(_ context.Context, id string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	blob, ok := s.blobs[id]
	return append([]byte(nil), blob...), ok, nil
}

func (s *einoMemoryCheckpoints) Set(_ context.Context, id string, blob []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	if s.blobs == nil {
		s.blobs = make(map[string][]byte)
	}
	s.blobs[id] = append([]byte(nil), blob...)
	s.writes++
	return nil
}

func TestEinoCheckpointAndFreshProcessResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	spec, _ := newGraph("chain", 9)
	cfg := Config{Mode: "plain", Rounds: 256, Parallelism: 4, RunID: "eino-probe", CheckpointAt: 4}
	store := &einoMemoryCheckpoints{}
	if _, err := buildEinoRecovery(ctx, spec, cfg, nil, store, false); err == nil {
		t.Fatal("missing checkpoint was accepted as a resume")
	}
	p := newProbe(9)
	r, err := buildEinoRecovery(ctx, spec, cfg, p, store, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.run(ctx); !einoInterrupted(err) {
		t.Fatalf("expected active interrupt: %v", err)
	}
	if p.active.Load() != 0 || store.writes != 1 {
		t.Fatalf("interrupt active=%d writes=%d", p.active.Load(), store.writes)
	}
	want, _ := spec.reference(cfg.Rounds)
	for id := range spec.names {
		count := int32(0)
		if id < cfg.CheckpointAt {
			count = 1
			if p.values[id].Load() != want[id] {
				t.Fatalf("prefix value %d mismatch", id)
			}
		}
		if p.counts[id].Load() != count {
			t.Fatalf("prefix node %d count=%d want=%d", id, p.counts[id].Load(), count)
		}
	}
	if _, err := buildEinoRecovery(ctx, spec, cfg, nil, store, true); err == nil {
		t.Fatal("pause accepted a stale checkpoint")
	}
	blob, _, _ := store.Get(ctx, cfg.RunID)
	// 子进程只收到序列化 checkpoint，不继承图、probe 或业务值内存。
	// 这是格式/进程隔离探针；真实 SQLite 落盘、退出和重开由统一宿主验证。
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEinoCheckpointResumeSubprocess$", "-test.v")
	cmd.Env = append(os.Environ(), "PULSE_EINO_RESUME_BLOB="+base64.StdEncoding.EncodeToString(blob))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh-process resume: %v\n%s", err, output)
	}
}

func TestEinoCheckpointResumeSubprocess(t *testing.T) {
	encoded := os.Getenv("PULSE_EINO_RESUME_BLOB")
	if encoded == "" {
		t.Skip("only invoked by checkpoint parent probe")
	}
	blob, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	store := &einoMemoryCheckpoints{blobs: map[string][]byte{"eino-probe": blob}}
	spec, _ := newGraph("chain", 9)
	cfg := Config{Mode: "plain", Rounds: 256, Parallelism: 4, RunID: "eino-probe", CheckpointAt: 4}
	p := newProbe(9)
	r, err := buildEinoRecovery(context.Background(), spec, cfg, p, store, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.run(context.Background())
	want, sum := spec.reference(cfg.Rounds)
	if err != nil || got != sum {
		t.Fatalf("resumed sum=%d want=%d err=%v", got, sum, err)
	}
	for id := range spec.names {
		count := int32(0)
		if id >= cfg.CheckpointAt {
			count = 1
			if p.values[id].Load() != want[id] {
				t.Fatalf("resumed node %d value mismatch", id)
			}
		}
		if p.counts[id].Load() != count {
			t.Fatalf("resumed node %d count=%d want=%d", id, p.counts[id].Load(), count)
		}
	}
	if p.active.Load() != 0 || store.writes != 0 {
		t.Fatalf("resume active=%d checkpoint writes=%d", p.active.Load(), store.writes)
	}
}

func TestEinoCheckpointWriteBoundary(t *testing.T) {
	ctx := context.Background()
	spec, _ := newGraph("chain", 3)
	cfg := Config{Mode: "plain", Rounds: 1, Parallelism: 1, RunID: "write-boundary", CheckpointAt: 1}
	store := &einoMemoryCheckpoints{}
	r, err := buildEinoGraph(ctx, spec, cfg, nil, compose.WithCheckPointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	r.options = append(r.options, compose.WithCheckPointID(cfg.RunID))
	_, want := spec.reference(cfg.Rounds)
	if got, err := r.run(ctx); err != nil || got != want || store.writes != 0 {
		t.Fatalf("normal success: got=%d err=%v checkpoint writes=%d", got, err, store.writes)
	}
	boom := errors.New("checkpoint store rejected write")
	store.setErr = boom
	r, err = buildEinoRecovery(ctx, spec, cfg, nil, store, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(ctx); !errors.Is(err, boom) || einoInterrupted(err) {
		t.Fatalf("failed checkpoint must remain a failure, got %v", err)
	}
}
