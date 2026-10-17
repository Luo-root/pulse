package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"slices"
	"time"
)

type config struct {
	engine, shape, mode        string
	nodes, rounds, parallelism int
	procs, iterations          int
	minTime, timeout           time.Duration
	verify                     bool
}

type result struct {
	Engine       string   `json:"engine"`
	Shape        string   `json:"shape"`
	Nodes        int      `json:"nodes"`
	Edges        int      `json:"edges"`
	CriticalPath int      `json:"critical_path"`
	Rounds       int      `json:"rounds"`
	Parallelism  int      `json:"parallelism"`
	Procs        int      `json:"procs"`
	Mode         string   `json:"mode"`
	Iterations   int      `json:"iterations"`
	ElapsedNS    int64    `json:"elapsed_ns"`
	NSPerOp      float64  `json:"ns_per_op"`
	BytesPerOp   float64  `json:"bytes_per_op"`
	AllocsPerOp  float64  `json:"allocs_per_op"`
	Status       string   `json:"status"`
	Checksum     *uint64  `json:"checksum"`
	Notes        []string `json:"notes"`
	GoVersion    string   `json:"go_version"`
	Error        string   `json:"error,omitempty"`
}

func (c config) validate() error {
	if !slices.Contains([]string{"direct", "pulse", "azure", "taskflow"}, c.engine) ||
		!slices.Contains([]string{"chain", "wide", "diamonds"}, c.shape) ||
		!slices.Contains([]string{"e2e", "build", "reuse"}, c.mode) {
		return fmt.Errorf("unknown engine, shape or mode")
	}
	if c.nodes < 1 || (c.rounds != 1 && c.rounds != 256) || c.parallelism < 1 || c.procs < 1 ||
		c.iterations < 0 || c.minTime <= 0 || c.timeout <= 0 {
		return fmt.Errorf("positive nodes/parallelism/procs/durations, rounds 1 or 256 and nonnegative iterations required")
	}
	return nil
}

func resultFor(c config, g *graphSpec) result {
	r := result{Engine: c.engine, Shape: c.shape, Nodes: c.nodes, Rounds: c.rounds,
		Parallelism: c.parallelism, Procs: c.procs, Mode: c.mode, Status: "ok", GoVersion: runtime.Version(),
		Notes: []string{
			"Common graph specification, serial oracle, one warmup, optional instrumented verification, GC before sample and MemStats reads are excluded.",
			"critical_path counts nodes. bytes_per_op/allocs_per_op are cumulative Go TotalAlloc/Mallocs deltas, not peak memory or RSS.",
			"Each operation includes final output reads, native task completion waits and checksum comparison; build has no execution/checksum. Ordinary GC during the sample is included.",
		}}
	if g != nil {
		r.Edges, r.CriticalPath = g.edges, g.criticalPath
	}
	switch c.mode {
	case "e2e":
		r.Notes = append(r.Notes, "Definition, adapter and executor initialization are included in every operation; no shared pre-initialized executor.")
	case "build":
		r.Notes = append(r.Notes, "Only definition/adapter/executor assembly; implicit Start/Do/Run validation stays in execution and is not performed here. Last assembled definition is retained through sample end.")
	case "reuse":
		r.Notes = append(r.Notes, "Definition/adapter/executor initialization and warmup are excluded; native per-run state setup/reset, output buffer clearing and output reads are included.")
	}
	switch c.engine {
	case "direct":
		r.Notes = append(r.Notes, "Direct is a serial topological compute reference, without an engine scheduler; parallelism is only an upper bound.")
	case "pulse":
		r.Notes = append(r.Notes, "Pulse uses typed Get/Set and declared terminal ReadOutput; every run creates a fresh Execution, including in reuse mode.")
	case "azure":
		r.Notes = append(r.Notes, "Azure uses dependency-ordered shared uint64 outputs. Do's native reset and preflight are included on every run.")
	case "taskflow":
		r.Notes = append(r.Notes, "Taskflow uses dependency-ordered shared uint64 outputs and native Run setup. Run/Wait includes all task work; its pool has no shutdown/join API, so short self-terminating worker epilogues may outlive Wait.",
			"Taskflow has no context cancellation API; node bodies check context and the CLI process watchdog bounds assembly/scheduling/wait for every engine. A timeout aborts the process and yields no successful sample.")
	}
	return r
}

type operation struct {
	config config
	graph  *graphSpec
	reused runner
	last   runner
}

func prepare(c config, g *graphSpec) (*operation, error) {
	o := &operation{config: c, graph: g}
	if c.mode == "reuse" {
		var err error
		o.reused, err = build(c.engine, g, c.rounds, c.parallelism, nil)
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

func (o *operation) run(ctx context.Context) (uint64, error) {
	r := o.reused
	if o.config.mode != "reuse" {
		var err error
		r, err = build(o.config.engine, o.graph, o.config.rounds, o.config.parallelism, nil)
		if err != nil {
			return 0, err
		}
	}
	if o.config.mode == "build" {
		o.last = r
		return 0, nil
	}
	return r.run(ctx)
}

func verify(ctx context.Context, c config, g *graphSpec, expected []uint64, checksum uint64) error {
	p := newProbe(c.nodes)
	r, err := build(c.engine, g, c.rounds, c.parallelism, p)
	if err != nil {
		return err
	}
	// 同一原生定义连续运行两次，同时验证复用不能读取上一轮遗留值。
	for range 2 {
		got, err := r.run(ctx)
		if err != nil {
			return err
		}
		if got != checksum {
			return fmt.Errorf("checksum %d, want %d", got, checksum)
		}
		if err := p.check(expected, c.parallelism); err != nil {
			return err
		}
		p.reset()
	}
	return nil
}

func measure(ctx context.Context, c config, g *graphSpec) (out result) {
	out = resultFor(c, g)
	fail := func(err error) result {
		out.Status, out.Error = "error", err.Error()
		if ctx.Err() != nil {
			out.Status = "timeout"
		}
		return out
	}
	defer func() {
		if value := recover(); value != nil {
			out.Status, out.Error = "error", fmt.Sprintf("panic: %v", value)
		}
	}()
	expected, checksum := g.reference(c.rounds)
	if c.verify {
		if err := verify(ctx, c, g, expected, checksum); err != nil {
			return fail(err)
		}
	}
	o, err := prepare(c, g)
	if err != nil {
		return fail(err)
	}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		got, err := o.run(ctx)
		if err == nil && c.mode != "build" && got != checksum {
			err = fmt.Errorf("checksum %d, want %d", got, checksum)
		}
		return err
	}
	if err := check(); err != nil { // 所有模式都预热一次，不计入 iterations。
		return fail(err)
	}
	o.last = nil
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	count, batch := 0, 1
	for {
		if c.iterations > 0 {
			batch = c.iterations - count
		}
		for range batch {
			if err = check(); err != nil {
				return fail(err)
			}
			count++
		}
		if c.iterations > 0 || time.Since(started) >= c.minTime {
			break
		}
		batch = min(batch*2, 4096)
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(o)
	out.Iterations = count
	out.ElapsedNS = elapsed.Nanoseconds()
	if elapsed == 0 {
		out.Notes = append(out.Notes, "Elapsed time is below the platform clock resolution; use iterations=0/min-time or a larger fixed iteration count for timing conclusions.")
	}
	out.NSPerOp = float64(elapsed.Nanoseconds()) / float64(count)
	out.BytesPerOp = float64(after.TotalAlloc-before.TotalAlloc) / float64(count)
	out.AllocsPerOp = float64(after.Mallocs-before.Mallocs) / float64(count)
	if c.mode != "build" {
		out.Checksum = &checksum
	}
	return out
}

func main() { os.Exit(runCLI()) }

func runCLI() int {
	var c config
	flag.StringVar(&c.engine, "engine", "pulse", "direct|pulse|azure|taskflow")
	flag.StringVar(&c.shape, "shape", "chain", "chain|wide|diamonds")
	flag.IntVar(&c.nodes, "nodes", 10, "number of actual business nodes")
	flag.IntVar(&c.rounds, "rounds", 1, "compute rounds: 1|256")
	flag.IntVar(&c.parallelism, "parallelism", 4, "maximum running node bodies")
	flag.IntVar(&c.procs, "procs", 4, "GOMAXPROCS")
	flag.StringVar(&c.mode, "mode", "e2e", "e2e|build|reuse")
	flag.DurationVar(&c.minTime, "min-time", 100*time.Millisecond, "minimum measured duration when iterations=0")
	flag.IntVar(&c.iterations, "iterations", 0, "fixed measured operations; 0 uses min-time")
	flag.DurationVar(&c.timeout, "timeout", 60*time.Second, "whole case timeout after common graph creation")
	flag.BoolVar(&c.verify, "verify", false, "verify exact-once, all node values and concurrency outside timing")
	flag.Parse()
	out := resultFor(c, nil)
	if err := c.validate(); err != nil {
		out.Status, out.Error = "error", err.Error()
	} else {
		runtime.GOMAXPROCS(c.procs)
		g, _ := newGraph(c.shape, c.nodes) // validate 已检查输入；共用图不属于计时边界。
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		done := make(chan result, 1)
		go func() { done <- measure(ctx, c, g) }()
		select {
		case out = <-done:
		case <-ctx.Done():
			out = resultFor(c, g)
			out.Status, out.Error = "timeout", ctx.Err().Error()
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if out.Status != "ok" {
		fmt.Fprintln(os.Stderr, out.Error)
		return 1
	}
	return 0
}
