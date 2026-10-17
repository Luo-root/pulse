package main

import (
	"fmt"
	"runtime"
	"strconv"
	"sync/atomic"
)

// graphSpec 是各引擎共用的输入，不计入装配或执行时间；索引本身就是拓扑序。
type graphSpec struct {
	shape        string
	predecessors [][]int
	names        []string
	terminals    []int
	edges        int
	criticalPath int // 最长路径的节点数，单节点为 1。
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
	for i, predecessors := range g.predecessors {
		g.names[i] = "n" + strconv.Itoa(i)
		depth[i] = 1
		for _, from := range predecessors {
			depth[i] = max(depth[i], depth[from]+1)
			successors[from]++
			g.edges++
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

func (g *graphSpec) checksum(values []uint64) uint64 {
	var sum uint64
	for _, i := range g.terminals {
		sum += values[i]
	}
	return sum
}

func (g *graphSpec) reference(rounds int) ([]uint64, uint64) {
	values := make([]uint64, len(g.predecessors))
	for i, predecessors := range g.predecessors {
		input := uint64(1)
		for _, from := range predecessors {
			input += values[from]
		}
		values[i] = compute(input, i, rounds)
	}
	return values, g.checksum(values)
}

// probe 只用于独立验证，主计时永远传 nil；让出一次处理器以增加并发重叠机会。
type probe struct {
	counts []atomic.Int32
	values []atomic.Uint64
	active atomic.Int32
	peak   atomic.Int32
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

func (p *probe) record(i int, value uint64) {
	if p != nil {
		p.values[i].Store(value)
	}
}

func (p *probe) check(want []uint64, limit int) error {
	if active := p.active.Load(); active != 0 {
		return fmt.Errorf("%d node bodies still active", active)
	}
	if peak := p.peak.Load(); peak < 1 || int(peak) > limit {
		return fmt.Errorf("peak concurrency %d outside [1,%d]", peak, limit)
	}
	for i, value := range want {
		if count := p.counts[i].Load(); count != 1 {
			return fmt.Errorf("node %d ran %d times, want 1", i, count)
		}
		if got := p.values[i].Load(); got != value {
			return fmt.Errorf("node %d output %d, want %d", i, got, value)
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
