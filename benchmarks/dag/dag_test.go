package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestComputeGolden(t *testing.T) {
	// Python 大整数实现逐步 & ((1<<64)-1) 独立计算的固定值。
	for _, tc := range []struct {
		input  uint64
		id     int
		rounds int
		want   uint64
	}{
		{1, 0, 1, 1164970750538294679},
		{1, 0, 256, 13017035472905934870},
		{^uint64(0), 99, 1, 6596097752651701936},
	} {
		if got := compute(tc.input, tc.id, tc.rounds); got != tc.want {
			t.Fatalf("compute(%d,%d,%d) = %d, want %d", tc.input, tc.id, tc.rounds, got, tc.want)
		}
	}
}

func TestGraphSpec(t *testing.T) {
	for _, tc := range []struct {
		shape           string
		edges, critical int
		terminals       []int
		predecessors    [][]int
	}{
		{"chain", 9, 10, []int{9}, [][]int{nil, {0}, {1}, {2}, {3}, {4}, {5}, {6}, {7}, {8}}},
		{"wide", 0, 1, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, make([][]int, 10)},
		{"diamonds", 12, 7, []int{9}, [][]int{nil, {0}, {0}, {1, 2}, {3}, {3}, {4, 5}, {6}, {6}, {7, 8}}},
	} {
		g, err := newGraph(tc.shape, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(g.names) != 10 || g.edges != tc.edges || g.criticalPath != tc.critical ||
			!reflect.DeepEqual(g.terminals, tc.terminals) || !reflect.DeepEqual(g.predecessors, tc.predecessors) {
			t.Fatalf("wrong %s specification: %+v", tc.shape, g)
		}
	}
	for _, shape := range []string{"chain", "wide", "diamonds"} {
		g, err := newGraph(shape, 1)
		if err != nil || len(g.names) != 1 || g.edges != 0 || g.criticalPath != 1 || !reflect.DeepEqual(g.terminals, []int{0}) {
			t.Fatalf("wrong singleton %s: %+v, %v", shape, g, err)
		}
	}
	if _, err := newGraph("invalid", 10); err == nil {
		t.Fatal("invalid shape accepted")
	}
	if _, err := newGraph("chain", 0); err == nil {
		t.Fatal("zero nodes accepted")
	}
}

func TestCorrectness(t *testing.T) {
	// 每个配置验证两轮原生复用，检查所有节点的值、恰好一次和并发上限。
	for _, n := range []int{1, 2, 10, 100} {
		for _, shape := range []string{"chain", "wide", "diamonds"} {
			g, err := newGraph(shape, n)
			if err != nil {
				t.Fatal(err)
			}
			for _, rounds := range []int{1, 256} {
				want, checksum := g.reference(rounds)
				for _, parallelism := range []int{1, 4} {
					for _, engine := range []string{"direct", "pulse", "azure", "taskflow"} {
						t.Run(fmt.Sprintf("%s/%s/N%d/R%d/P%d", engine, shape, n, rounds, parallelism), func(t *testing.T) {
							old := runtime.GOMAXPROCS(parallelism)
							defer runtime.GOMAXPROCS(old)
							ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
							defer cancel()
							c := config{engine: engine, nodes: n, rounds: rounds, parallelism: parallelism}
							if err := verify(ctx, c, g, want, checksum); err != nil {
								t.Fatal(err)
							}
						})
					}
				}
			}
		}
	}
}

func TestAzureConcurrencyProgress(t *testing.T) {
	const child = "PULSE_DAG_WAKEUP_CHILD"
	if os.Getenv(child) != "1" {
		// 独立进程限定丢失唤醒的等待时间；旧依赖即使不响应 ctx，也不会挂住整个测试套件。
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAzureConcurrencyProgress$", "-test.timeout=15s")
		cmd.Env = append(os.Environ(), child+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Azure bounded-concurrency progress failed: %v (context: %v)\n%s", err, ctx.Err(), output)
		}
		return
	}

	old := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(old)
	g, err := newGraph("chain", 3)
	if err != nil {
		t.Fatal(err)
	}
	expected, checksum := g.reference(1)
	c := config{engine: "azure", nodes: 3, rounds: 1, parallelism: 1}
	// 将业务额度压至 1，同时允许调度循环与完成通知并行，覆盖归还额度与唤醒的先后窗口。
	for trial := range 4096 {
		if err := verify(context.Background(), c, g, expected, checksum); err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
	}
}

func TestMeasuredModes(t *testing.T) {
	g, _ := newGraph("diamonds", 10)
	_, want := g.reference(256)
	for _, engine := range []string{"direct", "pulse", "azure", "taskflow"} {
		for _, mode := range []string{"e2e", "build", "reuse"} {
			t.Run(engine+"/"+mode, func(t *testing.T) {
				c := config{engine: engine, shape: "diamonds", mode: mode, nodes: 10, rounds: 256,
					parallelism: 4, procs: 4, iterations: 2, minTime: time.Millisecond, timeout: 30 * time.Second}
				ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
				defer cancel()
				got := measure(ctx, c, g)
				if got.Status != "ok" || got.Iterations != 2 || got.NSPerOp < 0 || got.BytesPerOp < 0 || got.AllocsPerOp < 0 {
					t.Fatalf("bad sample: %+v", got)
				}
				if mode == "build" {
					if got.Checksum != nil {
						t.Fatal("build must not claim an executed checksum")
					}
				} else if got.Checksum == nil || *got.Checksum != want {
					t.Fatalf("checksum = %v, want %d", got.Checksum, want)
				}
			})
		}
	}
}

func TestAdaptiveDuration(t *testing.T) {
	g, _ := newGraph("wide", 10)
	c := config{engine: "direct", shape: "wide", mode: "reuse", nodes: 10, rounds: 1,
		parallelism: 4, procs: 4, minTime: 20 * time.Millisecond, timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	got := measure(ctx, c, g)
	if got.Status != "ok" || got.Iterations < 1 || got.ElapsedNS < c.minTime.Nanoseconds() || got.NSPerOp <= 0 {
		t.Fatalf("adaptive sample did not reach min-time: %+v", got)
	}
}

func TestCanceledRun(t *testing.T) {
	g, _ := newGraph("diamonds", 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, engine := range []string{"direct", "pulse", "azure", "taskflow"} {
		r, err := build(engine, g, 1, 4, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.run(ctx); err == nil {
			t.Fatalf("%s accepted a canceled run", engine)
		}
	}
}
