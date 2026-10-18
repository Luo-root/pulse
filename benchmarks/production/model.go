package main

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"sync/atomic"
)

// 所有适配器使用同一任务、真实前驱值和终端校验；Spec 准备不计入引擎装配。
type graphSpec struct {
	shape               string
	predecessors        [][]int
	names               []string
	terminals           []int
	edges, criticalPath int
}

func newGraph(shape string, n int) (*graphSpec, error) {
	if n < 1 {
		return nil, fmt.Errorf("nodes must be positive")
	}
	g := &graphSpec{shape: shape, predecessors: make([][]int, n), names: make([]string, n)}
	switch shape {
	case "chain":
		for i := 1; i < n; i++ {
			g.predecessors[i] = []int{i - 1}
		}
	case "wide":
	case "diamonds":
		var previous []int
		for first, width := 0, 1; first < n; width = 3 - width {
			end := min(n, first+width)
			layer := make([]int, 0, end-first)
			for i := first; i < end; i++ {
				g.predecessors[i] = append([]int(nil), previous...)
				layer = append(layer, i)
			}
			previous, first = layer, end
		}
	default:
		return nil, fmt.Errorf("unknown shape %q", shape)
	}
	depth, successors := make([]int, n), make([]int, n)
	for i, preds := range g.predecessors {
		g.names[i] = "n" + strconv.Itoa(i)
		depth[i] = 1
		for _, from := range preds {
			successors[from]++
			g.edges++
			depth[i] = max(depth[i], depth[from]+1)
		}
		g.criticalPath = max(g.criticalPath, depth[i])
	}
	for i, count := range successors {
		if count == 0 {
			g.terminals = append(g.terminals, i)
		}
	}
	return g, nil
}

func compute(input uint64, id, rounds int) uint64 {
	x := input ^ (uint64(id+1) * 0x9e3779b97f4a7c15)
	for r := 0; r < rounds; r++ {
		x ^= x >> 12
		x ^= x << 25
		x ^= x >> 27
		x *= 0x2545f4914f6cdd1d
	}
	return x
}

func (g *graphSpec) reference(rounds int) ([]uint64, uint64) {
	values := make([]uint64, len(g.names))
	for i, preds := range g.predecessors {
		input := uint64(1)
		for _, from := range preds {
			input += values[from]
		}
		values[i] = compute(input, i, rounds)
	}
	var sum uint64
	for _, i := range g.terminals {
		sum += values[i]
	}
	return values, sum
}

// Config 是宿主配置；服务地址、持久化目录与运行身份不由性能结果覆盖。
type Config struct {
	Engine       string `json:"engine"`
	Shape        string `json:"shape"`
	Nodes        int    `json:"nodes"`
	Rounds       int    `json:"rounds"`
	Parallelism  int    `json:"parallelism"`
	Mode         string `json:"mode"` // plain / observe；可靠恢复由单独的宿主命令量测。
	Address      string `json:"address,omitempty"`
	Queue        string `json:"queue,omitempty"`
	StorePath    string `json:"store_path,omitempty"`
	RunID        string `json:"run_id,omitempty"`
	CheckpointAt int    `json:"checkpoint_at,omitempty"`
}

type runner interface {
	run(context.Context) (uint64, error)
	close() error
}
type factory func(context.Context, *graphSpec, Config, *probe) (runner, error)

var adapters = map[string]factory{}

// 恢复命令与普通计时分开，由专门的进程控制器调用。
var recoveryCommands = map[string]func(context.Context, Config) (any, error){}

// probe 只用于计时前的独立正确性验收，正式采样始终为 nil。
type probe struct {
	counts       []atomic.Int32
	values       []atomic.Uint64
	active, peak atomic.Int32
}

func newProbe(n int) *probe {
	return &probe{counts: make([]atomic.Int32, n), values: make([]atomic.Uint64, n)}
}
func (p *probe) enter(i int) {
	if p == nil {
		return
	}
	p.counts[i].Add(1)
	active := p.active.Add(1)
	for peak := p.peak.Load(); active > peak; peak = p.peak.Load() {
		if p.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	runtime.Gosched()
}
func (p *probe) leave() {
	if p != nil {
		p.active.Add(-1)
	}
}
func (p *probe) record(i int, v uint64) {
	if p != nil {
		p.values[i].Store(v)
	}
}
func (p *probe) check(want []uint64, limit int) error {
	if p.active.Load() != 0 {
		return fmt.Errorf("node bodies remain active: %d", p.active.Load())
	}
	if peak := p.peak.Load(); peak < 1 || int(peak) > limit {
		return fmt.Errorf("peak concurrency %d outside [1,%d]", peak, limit)
	}
	for i, v := range want {
		if got := p.counts[i].Load(); got != 1 {
			return fmt.Errorf("node %d ran %d times", i, got)
		}
		if got := p.values[i].Load(); got != v {
			return fmt.Errorf("node %d value %d want %d", i, got, v)
		}
	}
	return nil
}
func (p *probe) reset() {
	for i := range p.counts {
		p.counts[i].Store(0)
		p.values[i].Store(0)
	}
	p.peak.Store(0)
}
