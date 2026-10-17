package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/compose"
)

func init() { adapters["eino"] = buildEino }

type einoRunner struct {
	graph       compose.Runnable[map[string]any, map[string]any]
	spec        *graphSpec
	parallelism int
	options     []compose.Option
	observer    *einoObservations
}

func buildEino(ctx context.Context, spec *graphSpec, cfg Config, p *probe) (runner, error) {
	return buildEinoGraph(ctx, spec, cfg, p)
}

func buildEinoGraph(ctx context.Context, spec *graphSpec, cfg Config, p *probe, extra ...compose.GraphCompileOption) (*einoRunner, error) {
	if spec == nil || len(spec.names) == 0 {
		return nil, fmt.Errorf("eino: empty graph")
	}
	if cfg.Parallelism < 1 || cfg.Rounds < 0 {
		return nil, fmt.Errorf("eino: parallelism must be positive and rounds nonnegative")
	}
	if cfg.Mode != "plain" && cfg.Mode != "observe" {
		return nil, fmt.Errorf("eino: unknown mode %q", cfg.Mode)
	}

	r := &einoRunner{spec: spec, parallelism: cfg.Parallelism}
	g := compose.NewGraph[map[string]any, map[string]any]()
	for id, name := range spec.names {
		node := compose.InvokableLambda(func(ctx context.Context, in map[string]any) (uint64, error) {
			scope, err := enterEinoBody(ctx)
			if err != nil {
				return 0, err
			}
			defer scope.leave()
			p.enter(id)
			defer p.leave()
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			input, err := einoNodeInput(spec, id, in)
			if err != nil {
				return 0, err
			}
			value := compute(input, id, cfg.Rounds)
			p.record(id, value)
			return value, nil
		})
		// 唯一输出键避免 fork 携带祖先结果；输入 map 只读，不做原地更新。
		if err := g.AddLambdaNode(name, node, compose.WithNodeName(name), compose.WithOutputKey(name)); err != nil {
			return nil, fmt.Errorf("eino: add node %s: %w", name, err)
		}
	}
	for id, preds := range spec.predecessors {
		if len(preds) == 0 {
			if err := g.AddEdge(compose.START, spec.names[id]); err != nil {
				return nil, err
			}
		}
		for _, from := range preds {
			if err := g.AddEdge(spec.names[from], spec.names[id]); err != nil {
				return nil, err
			}
		}
	}
	for _, id := range spec.terminals {
		if err := g.AddEdge(spec.names[id], compose.END); err != nil {
			return nil, err
		}
	}
	options := []compose.GraphCompileOption{
		compose.WithNodeTriggerMode(compose.AllPredecessor),
		compose.WithGraphName("production"),
	}
	runnable, err := g.Compile(ctx, append(options, extra...)...)
	if err != nil {
		return nil, fmt.Errorf("eino: compile: %w", err)
	}
	r.graph = runnable
	if cfg.Mode == "observe" {
		r.observer = &einoObservations{}
		r.options = []compose.Option{compose.WithCallbacks(r.observer.handler())}
	}
	return r, nil
}

func einoNodeInput(spec *graphSpec, id int, in map[string]any) (uint64, error) {
	preds := spec.predecessors[id]
	if len(preds) == 0 {
		seed, ok := in["seed"].(uint64)
		if !ok || len(in) != 1 {
			return 0, fmt.Errorf("eino: root %s needs only a uint64 seed", spec.names[id])
		}
		return seed, nil
	}
	if len(in) != len(preds) {
		return 0, fmt.Errorf("eino: %s received %d keys, want %d direct predecessors", spec.names[id], len(in), len(preds))
	}
	input := uint64(1)
	for _, from := range preds {
		value, ok := in[spec.names[from]].(uint64)
		if !ok {
			return 0, fmt.Errorf("eino: %s missing uint64 predecessor %s", spec.names[id], spec.names[from])
		}
		input += value
	}
	return input, nil
}

func (r *einoRunner) run(ctx context.Context) (uint64, error) {
	out, err := r.invoke(ctx, map[string]any{"seed": uint64(1)})
	if err != nil {
		return 0, err
	}
	if len(out) != len(r.spec.terminals) {
		return 0, fmt.Errorf("eino: received %d terminal keys, want %d", len(out), len(r.spec.terminals))
	}
	var sum uint64
	for _, id := range r.spec.terminals {
		value, ok := out[r.spec.names[id]].(uint64)
		if !ok {
			return 0, fmt.Errorf("eino: missing uint64 terminal %s", r.spec.names[id])
		}
		sum += value
	}
	return sum, nil
}

func (r *einoRunner) invoke(ctx context.Context, in map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	scope := &einoRunScope{slots: make(chan struct{}, r.parallelism)}
	runCtx = context.WithValue(runCtx, einoRunScopeKey{}, scope)
	defer func() {
		// Eino v0.9.21 的节点错误路径可能先于兄弟任务返回。
		// 宿主只保证自身业务体退出、迟到业务体不得进入；不保证引擎 goroutine 全部退出。
		scope.mu.Lock()
		scope.closed = true
		scope.mu.Unlock()
		cancel()
		scope.bodies.Wait()
	}()
	out, err := r.graph.Invoke(runCtx, in, r.options...)
	if err == nil {
		err = ctx.Err()
	}
	return out, err
}

func (*einoRunner) close() error { return nil }

type einoRunScopeKey struct{}

type einoRunScope struct {
	mu     sync.Mutex
	closed bool
	bodies sync.WaitGroup
	slots  chan struct{}
}

func enterEinoBody(ctx context.Context) (*einoRunScope, error) {
	scope, ok := ctx.Value(einoRunScopeKey{}).(*einoRunScope)
	if !ok {
		return nil, fmt.Errorf("eino: missing host run scope")
	}
	scope.mu.Lock()
	if scope.closed {
		scope.mu.Unlock()
		return nil, context.Canceled
	}
	// Add 与关闭准入共用一把锁；Wait 开始后不再出现新的 Add。
	scope.bodies.Add(1)
	scope.mu.Unlock()
	select {
	case scope.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			scope.leave()
			return nil, err
		}
		return scope, nil
	case <-ctx.Done():
		scope.bodies.Done()
		return nil, ctx.Err()
	}
}

func (s *einoRunScope) leave() {
	<-s.slots
	s.bodies.Done()
}

// 仅计公开 Invoke callbacks：Graph 和 Lambda 各自的 start/end/error。
// START/END 没有任务或事件；计数跨 run 累加，失败时迟到的引擎 callbacks 也可能继续累计。
// plain 不安装 handler，返回 nil。这里的节点 start 不等同于业务体获准进入。
func (r *einoRunner) observations() map[string]int64 {
	if r.observer == nil {
		return nil
	}
	out := make(map[string]int64, 6)
	for component, prefix := range []string{"graph_", "node_"} {
		for timing, suffix := range []string{"start", "end", "error"} {
			out[prefix+suffix] = r.observer.counts[component][timing].Load()
		}
	}
	return out
}

type einoObservations struct {
	counts [2][3]atomic.Int64
}

func (o *einoObservations) record(info *callbacks.RunInfo, timing int) {
	if info == nil {
		return
	}
	switch info.Component {
	case compose.ComponentOfGraph:
		o.counts[0][timing].Add(1)
	case compose.ComponentOfLambda:
		o.counts[1][timing].Add(1)
	}
}

func (o *einoObservations) handler() callbacks.Handler {
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, _ callbacks.CallbackInput) context.Context {
			o.record(info, 0)
			return ctx
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, _ callbacks.CallbackOutput) context.Context {
			o.record(info, 1)
			return ctx
		}).
		OnErrorFn(func(ctx context.Context, info *callbacks.RunInfo, _ error) context.Context {
			o.record(info, 2)
			return ctx
		}).Build()
}
