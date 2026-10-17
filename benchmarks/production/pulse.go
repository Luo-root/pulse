package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/Luo-root/pulse"
)

func init() { adapters["pulse"] = buildPulse }

type pulseRunner struct {
	definition *pulse.Graph
	outputs    []pulse.Key[uint64]
	events     atomic.Int64
}

func buildPulse(_ context.Context, g *graphSpec, c Config, p *probe) (runner, error) {
	return buildPulseGraph(g, c, p, nil)
}

// before 只供独立恢复宿主设置确认前缀后的阻塞点；正式普通采样为 nil。
func buildPulseGraph(g *graphSpec, c Config, p *probe, before func(context.Context, int) error) (*pulseRunner, error) {
	r := &pulseRunner{}
	opts := []pulse.Option{pulse.WithMaxRunning(c.Parallelism)}
	if c.Mode == "observe" {
		count := func() { r.events.Add(1) }
		opts = append(opts, pulse.WithObserver(pulse.ObserverFunc{
			GraphStarted:  func(pulse.Site) { count() },
			GraphFinished: func(pulse.Site, pulse.NodeFinishReason, error) { count() },
			Waiting:       func(pulse.Site, string) { count() }, Running: func(pulse.Site, string) { count() },
			Finished: func(pulse.Site, string, pulse.NodeFinishReason, error) { count() },
		}))
	}
	definition, err := pulse.New("production-dag", "", opts...)
	if err != nil {
		return nil, err
	}
	r.definition = definition
	keys := make([]pulse.Key[uint64], len(g.names))
	for i, name := range g.names {
		keys[i] = pulse.NewKey[uint64](name)
	}
	for i, preds := range g.predecessors {
		requires := make([]pulse.Key[uint64], len(preds))
		for j, from := range preds {
			requires[j] = keys[from]
		}
		node := pulse.NewNode(g.names[i], "", pulse.Requires(requires...), pulse.Provides(keys[i]), func(rc *pulse.RunCtx) error {
			ctx := rc.Context()
			if before != nil {
				if err := before(ctx, i); err != nil {
					return err
				}
			}
			p.enter(i)
			defer p.leave()
			if err := ctx.Err(); err != nil {
				return err
			}
			input := uint64(1)
			for _, k := range requires {
				v, err := pulse.Get(rc, k)
				if err != nil {
					return err
				}
				input += v
			}
			v := compute(input, i, c.Rounds)
			if err := pulse.Set(rc, keys[i], v); err != nil {
				return err
			}
			p.record(i, v)
			return nil
		})
		if err := definition.Add(node); err != nil {
			return nil, err
		}
	}
	for _, i := range g.terminals {
		if err := definition.DeclareOutput(pulse.NewPort(keys[i])); err != nil {
			return nil, err
		}
		r.outputs = append(r.outputs, keys[i])
	}
	return r, nil
}
func (r *pulseRunner) run(ctx context.Context) (uint64, error) {
	e, err := r.definition.NewExecution(ctx, pulse.Inputs{})
	if err != nil {
		return 0, err
	}
	defer e.Cancel()
	if err = e.Start(); err != nil {
		return 0, err
	}
	if err = e.Wait(); err != nil {
		return 0, err
	}
	return r.checksum(e)
}
func (r *pulseRunner) checksum(e *pulse.Execution) (uint64, error) {
	var sum uint64
	for _, k := range r.outputs {
		v, state, err := pulse.ReadOutput(e, k)
		if err != nil {
			return 0, err
		}
		if state != pulse.SlotStateReady {
			return 0, fmt.Errorf("output %s state %s", k.Name(), state)
		}
		sum += v
	}
	return sum, nil
}
func (r *pulseRunner) close() error { return nil }
func (r *pulseRunner) observations() map[string]int64 {
	return map[string]int64{"lifecycle_callbacks": r.events.Load()}
}
