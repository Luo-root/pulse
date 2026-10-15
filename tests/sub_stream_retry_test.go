package pulse_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/Luo-root/pulse"
)

func TestSubRetryKeepsMissingStreamsNormal(t *testing.T) {
	for _, mode := range []string{"fixed-skip", "seeded-skip", "produced-skip", "skipped-pass-through"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			child, err := p.New("skip-retry-child", "")
			streamAspectsMust(t, err)
			root, err := p.New("skip-retry-root", "")
			streamAspectsMust(t, err)
			absent, outside := p.NewKey[p.Stream[int]]("absent"), p.NewKey[p.Stream[int]]("outside")
			result := p.NewKey[int]("result")
			streamAspectsMust(t, child.DeclareInput(p.NewPort(absent)))
			streamAspectsMust(t, child.DeclareOutput(p.NewPort(result)))
			streamAspectsMust(t, child.Add(p.NewNode("reader", "", p.Requires(absent), nil, func(*p.RunCtx) error {
				return errors.New("missing stream entered its reader")
			})))
			var calls atomic.Int32
			streamAspectsMust(t, child.Add(p.NewNode("independent", "", nil, p.Provides(result), func(rc *p.RunCtx) error {
				if calls.Add(1) == 1 {
					return errors.New("retry scalar work")
				}
				return p.Set(rc, result, 42)
			})))
			binds := []p.SubBind{p.Out(result, result)}
			var inputs p.Inputs
			switch mode {
			case "fixed-skip", "skipped-pass-through":
				binds = append(binds, p.FixSkip(absent))
			case "seeded-skip":
				streamAspectsMust(t, root.DeclareInput(p.NewPort(absent)))
				streamAspectsMust(t, p.SkipSeed(&inputs, absent))
				binds = append(binds, p.In(absent, absent))
			case "produced-skip":
				streamAspectsMust(t, root.Add(p.NewNode("skip-source", "", nil, p.Provides(absent), func(rc *p.RunCtx) error {
					return p.Skip(rc, absent)
				})))
				binds = append(binds, p.In(absent, absent))
			}
			if mode == "skipped-pass-through" {
				streamAspectsMust(t, child.DeclareOutput(p.NewPort(absent)))
				streamAspectsMust(t, root.DeclareOutput(p.NewPort(outside)))
				binds = append(binds, p.Out(absent, outside))
			}
			streamAspectsMust(t, root.DeclareOutput(p.NewPort(result)))
			streamAspectsMust(t, p.Sub(root, "mount", "", binds, child, p.Retry(2, 0)))
			execution, err := root.NewExecution(ctx, inputs)
			streamAspectsMust(t, err)
			defer execution.Cancel()
			streamAspectsMust(t, execution.Start())
			streamAspectsMust(t, execution.Wait())
			value, state, err := p.ReadOutput(execution, result)
			if err != nil || value != 42 || state != p.SlotStateReady || calls.Load() != 2 {
				t.Fatalf("value=%d state=%s calls=%d err=%v", value, state, calls.Load(), err)
			}
			if mode == "skipped-pass-through" {
				_, state, err := p.ReadOutput(execution, outside)
				if err != nil || state != p.SlotStateSkipped {
					t.Fatalf("missing pass-through = %s, %v", state, err)
				}
			}
		})
	}
}

func TestSubRetryFindsExternalReaderThroughPassThrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	root, err := p.New("alias-retry-root", "")
	streamAspectsMust(t, err)
	child, err := p.New("alias-retry-child", "")
	streamAspectsMust(t, err)
	pass, err := p.New("alias-retry-pass", "")
	streamAspectsMust(t, err)
	input, alias := p.NewKey[<-chan int]("input"), p.NewKey[<-chan int]("alias")
	result := p.NewKey[int]("result")
	streamAspectsMust(t, root.DeclareInput(p.NewPort(input)))
	streamAspectsMust(t, child.DeclareInput(p.NewPort(input)))
	streamAspectsMust(t, child.DeclareOutput(p.NewPort(result)))
	streamAspectsMust(t, pass.DeclareInput(p.NewPort(input)))
	streamAspectsMust(t, pass.DeclareOutput(p.NewPort(input)))
	streamAspectsMust(t, p.Sub(child, "pass", "", []p.SubBind{p.In(input, input), p.Out(input, alias)}, pass))
	streamAspectsMust(t, p.Consume(child, "reader", "", alias, func(*p.RunCtx, int) error { return nil }))
	var calls atomic.Int32
	streamAspectsMust(t, child.Add(p.NewNode("scalar", "", nil, p.Provides(result), func(rc *p.RunCtx) error {
		calls.Add(1)
		return p.Set(rc, result, 42)
	})))
	streamAspectsMust(t, p.Sub(root, "mount", "", []p.SubBind{p.In(input, input), p.Out(result, result)}, child, p.Retry(2, 0)))
	ch := make(chan int)
	close(ch)
	var inputs p.Inputs
	streamAspectsMust(t, p.Seed(&inputs, input, (<-chan int)(ch)))
	if err := root.Run(ctx, inputs); err == nil || !strings.Contains(err.Error(), "live stream input") || calls.Load() != 0 {
		t.Fatalf("external aliased reader passed Retry guard: err=%v calls=%d", err, calls.Load())
	}
}

func TestSubRetryDoesNotWaitForUnusedStreamInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	child, err := p.New("unused-child", "")
	streamAspectsMust(t, err)
	root, err := p.New("unused-root", "")
	streamAspectsMust(t, err)
	unused, result := p.NewKey[<-chan int]("unused"), p.NewKey[int]("result")
	streamAspectsMust(t, child.DeclareInput(p.NewPort(unused)))
	streamAspectsMust(t, child.DeclareOutput(p.NewPort(result)))
	var attempts atomic.Int32
	streamAspectsMust(t, child.Add(p.NewNode("scalar", "", nil, p.Provides(result), func(rc *p.RunCtx) error {
		if attempts.Add(1) == 1 {
			return errors.New("retry once")
		}
		return p.Set(rc, result, 42)
	})))
	// 此 channel 的发布依赖整个 Sub 的普通输出；它在 child 中没有实际读者。
	streamAspectsMust(t, root.Add(p.NewNode("late-unused", "", p.Requires(result), p.Provides(unused), func(rc *p.RunCtx) error {
		ch := make(chan int)
		close(ch)
		return p.Set(rc, unused, (<-chan int)(ch))
	})))
	streamAspectsMust(t, p.Sub(root, "mount", "", []p.SubBind{p.In(unused, unused), p.Out(result, result)}, child, p.Retry(2, 0)))
	streamAspectsMust(t, root.Run(ctx, p.Inputs{}))
	if attempts.Load() != 2 {
		t.Fatalf("scalar attempts=%d", attempts.Load())
	}
}
