package main

import (
	"context"
	"fmt"

	flow "github.com/Azure/go-workflow"
	"github.com/Luo-root/pulse"
	taskflow "github.com/noneback/go-taskflow"
)

type runner interface {
	run(context.Context) (uint64, error)
}

type business struct {
	graph  *graphSpec
	rounds int
	values []uint64
	probe  *probe
}

func (b *business) node(ctx context.Context, i int) error {
	b.probe.enter(i)
	defer b.probe.leave()
	if err := ctx.Err(); err != nil {
		return err
	}
	input := uint64(1)
	for _, from := range b.graph.predecessors[i] {
		input += b.values[from]
	}
	value := compute(input, i, b.rounds)
	b.values[i] = value
	b.probe.record(i, value)
	return nil
}

type directRunner struct{ *business }

func (r *directRunner) run(ctx context.Context) (uint64, error) {
	clear(r.values)
	for i := range r.values {
		if err := r.node(ctx, i); err != nil {
			return 0, err
		}
	}
	return r.graph.checksum(r.values), nil
}

type azureStep struct {
	business *business
	id       int
}

func (s *azureStep) Do(ctx context.Context) error { return s.business.node(ctx, s.id) }

type azureRunner struct {
	*business
	workflow *flow.Workflow
}

func (r *azureRunner) run(ctx context.Context) (uint64, error) {
	clear(r.values)
	// Do 自带 reset 与 preflight，不能在计时前替它执行。
	if err := r.workflow.Do(ctx); err != nil {
		return 0, err
	}
	return r.graph.checksum(r.values), ctx.Err()
}

type taskflowRunner struct {
	*business
	definition *taskflow.TaskFlow
	executor   taskflow.Executor
	ctx        context.Context // Run/Wait 之间只读，下一轮只在上一轮 Wait 之后设置。
}

func (r *taskflowRunner) run(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.ctx = ctx
	clear(r.values)
	// 固定版本 Run 会 setup/reset 内部图；Reset() 只解除装配冻结，不需要额外调用。
	r.executor.Run(r.definition).Wait()
	return r.graph.checksum(r.values), ctx.Err()
}

type pulseRunner struct {
	definition *pulse.Graph
	outputs    []pulse.Key[uint64]
}

func (r *pulseRunner) run(ctx context.Context) (uint64, error) {
	execution, err := r.definition.NewExecution(ctx, pulse.Inputs{})
	if err != nil {
		return 0, err
	}
	if err = execution.Start(); err != nil {
		return 0, err
	}
	if err = execution.Wait(); err != nil {
		return 0, err
	}
	var checksum uint64
	for _, key := range r.outputs {
		value, state, err := pulse.ReadOutput(execution, key)
		if err != nil {
			return 0, err
		}
		if state != pulse.SlotStateReady {
			return 0, fmt.Errorf("output %s is %s", key.Name(), state)
		}
		checksum += value
	}
	return checksum, nil
}

func build(engine string, g *graphSpec, rounds, parallelism int, p *probe) (runner, error) {
	if engine == "pulse" {
		return buildPulse(g, rounds, parallelism, p)
	}
	b := &business{graph: g, rounds: rounds, values: make([]uint64, len(g.names)), probe: p}
	switch engine {
	case "direct":
		return &directRunner{b}, nil
	case "azure":
		w := &flow.Workflow{MaxConcurrency: parallelism}
		steps := make([]flow.Steper, len(g.names))
		for i := range steps {
			steps[i] = &azureStep{business: b, id: i}
		}
		for i, predecessors := range g.predecessors {
			upstreams := make([]flow.Steper, len(predecessors))
			for j, from := range predecessors {
				upstreams[j] = steps[from]
			}
			w.Add(flow.Steps(steps[i]).DependsOn(upstreams...))
		}
		return &azureRunner{business: b, workflow: w}, nil
	case "taskflow":
		r := &taskflowRunner{business: b, definition: taskflow.NewTaskFlow("dag"), executor: taskflow.NewExecutor(uint(parallelism))}
		tasks := make([]*taskflow.Task, len(g.names))
		for i, name := range g.names {
			tasks[i] = r.definition.NewTask(name, func() { _ = b.node(r.ctx, i) })
		}
		for i, predecessors := range g.predecessors {
			for _, from := range predecessors {
				tasks[i].Succeed(tasks[from])
			}
		}
		return r, nil
	default:
		return nil, fmt.Errorf("unknown engine %q", engine)
	}
}

func buildPulse(g *graphSpec, rounds, parallelism int, p *probe) (runner, error) {
	definition, err := pulse.New("dag", "", pulse.WithMaxRunning(parallelism))
	if err != nil {
		return nil, err
	}
	keys := make([]pulse.Key[uint64], len(g.names))
	for i, name := range g.names {
		keys[i] = pulse.NewKey[uint64](name)
	}
	for i, predecessors := range g.predecessors {
		requires := make([]pulse.Key[uint64], len(predecessors))
		for j, from := range predecessors {
			requires[j] = keys[from]
		}
		node := pulse.NewNode(g.names[i], "", pulse.Requires(requires...), pulse.Provides(keys[i]), func(rc *pulse.RunCtx) error {
			p.enter(i)
			defer p.leave()
			if err := rc.Context().Err(); err != nil {
				return err
			}
			input := uint64(1)
			for _, key := range requires {
				value, err := pulse.Get(rc, key)
				if err != nil {
					return err
				}
				input += value
			}
			value := compute(input, i, rounds)
			if err := pulse.Set(rc, keys[i], value); err != nil {
				return err
			}
			p.record(i, value)
			return nil
		})
		if err := definition.Add(node); err != nil {
			return nil, err
		}
	}
	r := &pulseRunner{definition: definition}
	for _, i := range g.terminals {
		if err := definition.DeclareOutput(pulse.NewPort(keys[i])); err != nil {
			return nil, err
		}
		r.outputs = append(r.outputs, keys[i])
	}
	return r, nil
}
