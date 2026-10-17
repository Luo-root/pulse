package main

import (
	"context"
	"fmt"
)

func init() {
	recoveryCommands["pulse/prepare"] = func(ctx context.Context, c Config) (any, error) {
		c.Mode = "recover-prepare"
		return durableRecovery(ctx, c)
	}
	recoveryCommands["pulse/resume"] = func(ctx context.Context, c Config) (any, error) {
		c.Mode = "recover-resume"
		return durableRecovery(ctx, c)
	}
	recoveryCommands["eino/prepare"] = func(ctx context.Context, c Config) (any, error) { return einoRecoveryCommand(ctx, c, true) }
	recoveryCommands["eino/resume"] = func(ctx context.Context, c Config) (any, error) { return einoRecoveryCommand(ctx, c, false) }
}

// 这两个命令分别在新的 OS 进程中运行；中断之后父进程另读 SQLite 确认 blob。
func einoRecoveryCommand(ctx context.Context, c Config, pause bool) (any, error) {
	start := benchNow()
	g, err := newGraph(c.Shape, c.Nodes)
	if err != nil {
		return nil, err
	}
	store, err := newBlobStore(c.StorePath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	p := newProbe(c.Nodes)
	c.Mode = "plain"
	r, err := buildEinoRecovery(ctx, g, c, p, store, pause)
	if err != nil {
		return nil, err
	}
	defer r.close()
	sum, err := r.run(ctx)
	if pause {
		if !einoInterrupted(err) {
			return nil, fmt.Errorf("wanted confirmed Eino interrupt, got %v", err)
		}
	} else if err != nil {
		return nil, err
	}
	blob, exists, loadErr := store.Get(ctx, c.RunID)
	if loadErr != nil || !exists {
		return nil, fmt.Errorf("checkpoint missing after operation: %v", loadErr)
	}
	values, want := g.reference(c.Rounds)
	executed := []int{}
	for i := 0; i < c.Nodes; i++ {
		count := int32(0)
		if pause && i < c.CheckpointAt || !pause && i >= c.CheckpointAt {
			count = 1
		}
		if got := p.counts[i].Load(); got != count {
			return nil, fmt.Errorf("recovery node %d executions=%d want%d", i, got, count)
		}
		if count == 1 {
			if p.values[i].Load() != values[i] {
				return nil, fmt.Errorf("recovery node %d wrong value", i)
			}
			executed = append(executed, i)
		}
	}
	if !pause && sum != want {
		return nil, fmt.Errorf("resume checksum %d want%d", sum, want)
	}
	details := map[string]any{"operation_ns": benchSince(start).Nanoseconds(), "checkpoint_bytes": len(blob), "executed": executed, "prefix": c.CheckpointAt, "native_active_interrupt": true}
	if !pause {
		details["checksum"] = sum
	}
	return details, nil
}
