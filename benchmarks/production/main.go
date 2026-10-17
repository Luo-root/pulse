package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

type sample struct {
	Config              Config            `json:"config"`
	Status              string            `json:"status"`
	Error               string            `json:"error,omitempty"`
	Toolchain           string            `json:"toolchain"`
	Modules             map[string]string `json:"modules"`
	Iterations          int               `json:"iterations"`
	RequestedIterations int               `json:"requested_iterations"`
	FirstCompleted      bool              `json:"first_completed"`
	ServerSetupNS       int64             `json:"server_setup_ns"`
	AdapterSetupNS      int64             `json:"adapter_setup_ns"`
	FirstNS             *int64            `json:"first_ns"`
	RunNS               *float64          `json:"run_ns"`
	AllocBytes          *uint64           `json:"alloc_bytes_per_run"`
	AllocObjects        *uint64           `json:"alloc_objects_per_run"`
	Checksum            *uint64           `json:"checksum"`
	Observations        map[string]int64  `json:"observations,omitempty"`
	Details             any               `json:"details,omitempty"`
	OperationNS         *int64            `json:"operation_ns,omitempty"`
	StoreBefore         *int64            `json:"store_before_bytes"`
	StoreFirst          *int64            `json:"store_after_first_bytes"`
	StoreAfter          *int64            `json:"store_after_batch_bytes"`
}

func main() {
	c := Config{}
	flag.StringVar(&c.Engine, "engine", "pulse", "pulse/eino/temporal")
	flag.StringVar(&c.Shape, "shape", "chain", "chain/wide/diamonds")
	flag.IntVar(&c.Nodes, "nodes", 10, "actual task count")
	flag.IntVar(&c.Rounds, "rounds", 1, "uint64 business rounds")
	flag.IntVar(&c.Parallelism, "parallelism", 4, "business concurrency")
	flag.StringVar(&c.Mode, "mode", "plain", "plain/observe/checkpoint")
	flag.StringVar(&c.Address, "address", "", "existing Temporal server address")
	flag.StringVar(&c.Queue, "queue", "", "isolated Temporal task queue")
	flag.StringVar(&c.StorePath, "store", "", "per-case SQLite path")
	flag.StringVar(&c.RunID, "run-id", "", "run ID prefix")
	flag.IntVar(&c.CheckpointAt, "checkpoint-at", 5, "confirmed prefix for recovery")
	iterations := flag.Int("iterations", 1, "reuse batch length")
	check := flag.Bool("check", false, "audit each node on two independent executions")
	operation := flag.String("operation", "measure", "measure or engine-specific recovery operation")
	timeout := flag.Duration("timeout", 60*time.Second, "whole-case budget including setup/first run")
	procs := flag.Int("gomaxprocs", 4, "GOMAXPROCS")
	temporalBin := flag.String("temporal-bin", "", "workspace Temporal CLI executable")
	flag.Parse()
	runtime.GOMAXPROCS(*procs)
	s := sample{Config: c, Status: "error", Toolchain: runtime.Version(), Modules: versions()}
	if err := measure(&s, c, *operation, *temporalBin, *iterations, *check, *timeout); err != nil {
		s.Error = err.Error()
		if s.Status != "not_equivalent" {
			s.Status = sampleErrorStatus(err)
		}
	} else {
		s.Status = "ok"
	}
	_ = json.NewEncoder(os.Stdout).Encode(s)
	if s.Status != "ok" {
		os.Exit(1)
	}
}
func sampleErrorStatus(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	return "error"
}

func versions() map[string]string {
	out := map[string]string{}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, d := range info.Deps {
			out[d.Path] = d.Version
			if d.Replace != nil {
				out[d.Path] += " REPLACED"
			}
		}
	}
	return out
}
func measure(s *sample, c Config, operation, binary string, iterations int, check bool, budget time.Duration) (err error) {
	if c.Parallelism < 1 || c.Rounds < 1 || iterations < 1 || budget <= 0 {
		return fmt.Errorf("positive parallelism, rounds, iterations and timeout required")
	}
	if c.Mode != "plain" && c.Mode != "observe" && c.Mode != "checkpoint" {
		return fmt.Errorf("invalid mode %q", c.Mode)
	}
	if c.RunID == "" {
		c.RunID = fmt.Sprintf("case-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	if c.Queue == "" {
		c.Queue = c.RunID
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	defer func() {
		if err != nil {
			err = errors.Join(ctx.Err(), err)
		}
	}()
	s.RequestedIterations = iterations
	if c.Engine == "temporal" {
		t := benchNow()
		stop, e := startServer(ctx, &c, binary)
		s.ServerSetupNS = benchSince(t).Nanoseconds()
		if e != nil {
			return e
		}
		defer stop()
	}
	s.Config = c
	if operation != "measure" {
		fn := recoveryCommands[c.Engine+"/"+operation]
		if fn == nil {
			return fmt.Errorf("unknown recovery operation %s/%s", c.Engine, operation)
		}
		started := benchNow()
		s.Details, err = fn(ctx, c)
		duration := benchSince(started).Nanoseconds()
		s.OperationNS = &duration
		return err
	}
	g, e := newGraph(c.Shape, c.Nodes)
	if e != nil {
		return e
	}
	_, want := g.reference(c.Rounds)
	var p *probe
	if check {
		p = newProbe(c.Nodes)
	}
	engine := c.Engine
	if c.Engine == "pulse" && c.Mode == "checkpoint" {
		engine = "durable"
	}
	fn := adapters[engine]
	if fn == nil {
		return fmt.Errorf("unknown engine %q", engine)
	}
	if c.Engine == "eino" && c.Mode == "checkpoint" {
		s.Status = "not_equivalent"
		return fmt.Errorf("Eino ordinary success does not save per-node checkpoints; use the separate interrupt recovery experiment")
	}
	t := benchNow()
	r, e := fn(ctx, g, c, p)
	s.AdapterSetupNS = benchSince(t).Nanoseconds()
	if e != nil {
		return e
	}
	defer func() {
		if e := r.close(); err == nil {
			err = e
		}
	}()
	storeBefore := storeBytes(c.StorePath)
	s.StoreBefore = &storeBefore
	t = benchNow()
	got, e := r.run(ctx)
	elapsed := benchSince(t).Nanoseconds()
	s.FirstNS = &elapsed
	if e != nil {
		return e
	}
	if got != want {
		return fmt.Errorf("first checksum %d want %d", got, want)
	}
	if check {
		values, _ := g.reference(c.Rounds)
		if e = p.check(values, c.Parallelism); e != nil {
			return e
		}
		p.reset()
		iterations = 1
	}
	s.FirstCompleted = true
	s.RequestedIterations = iterations
	storeFirst := storeBytes(c.StorePath)
	s.StoreFirst = &storeFirst
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	t = benchNow()
	for i := 0; i < iterations; i++ {
		got, e = r.run(ctx)
		if e != nil {
			return e
		}
		if got != want {
			return fmt.Errorf("reuse %d checksum %d want %d", i, got, want)
		}
		s.Iterations++
	}
	ns := float64(benchSince(t).Nanoseconds()) / float64(iterations)
	runtime.ReadMemStats(&after)
	storeAfter := storeBytes(c.StorePath)
	s.StoreAfter = &storeAfter
	if check {
		values, _ := g.reference(c.Rounds)
		if e = p.check(values, c.Parallelism); e != nil {
			return e
		}
	}
	s.RunNS = &ns
	s.Checksum = &got
	bytes, objects := (after.TotalAlloc-before.TotalAlloc)/uint64(iterations), (after.Mallocs-before.Mallocs)/uint64(iterations)
	s.AllocBytes = &bytes
	s.AllocObjects = &objects
	if o, ok := r.(interface{ observations() map[string]int64 }); ok {
		s.Observations = o.observations()
	}
	return ctx.Err()
}

// 文件长度快照含 SQLite 主文件、WAL、SHM；不是写入字节数，也不是每任务记录大小。
func storeBytes(path string) int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(path + suffix); err == nil && !info.IsDir() {
			total += info.Size()
		}
	}
	return total
}
