package pulse_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/Luo-root/pulse"
)

func TestSubRetryKeepsRenamedNestedMissingStreamNormal(t *testing.T) {
	for _, mode := range []string{"fixed-skip", "seeded-skip", "produced-skip"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			root, err := p.New("root", "")
			streamAspectsMust(t, err)
			child, err := p.New("child", "")
			streamAspectsMust(t, err)
			pass, err := p.New("pass", "")
			streamAspectsMust(t, err)
			input, inner := p.NewKey[p.Stream[int]]("input"), p.NewKey[p.Stream[int]]("inner")
			alias, outside := p.NewKey[p.Stream[int]]("alias"), p.NewKey[p.Stream[int]]("outside")
			result := p.NewKey[int]("result")
			streamAspectsMust(t, pass.DeclareInput(p.NewPort(inner)))
			streamAspectsMust(t, pass.DeclareOutput(p.NewPort(inner)))
			streamAspectsMust(t, child.DeclareInput(p.NewPort(input)))
			streamAspectsMust(t, child.DeclareOutput(p.NewPort(alias), p.NewPort(result)))
			streamAspectsMust(t, p.Sub(child, "pass", "", []p.SubBind{p.In(input, inner), p.Out(inner, alias)}, pass))
			var attempts atomic.Int32
			streamAspectsMust(t, child.Add(p.NewNode("scalar", "", nil, p.Provides(result), func(rc *p.RunCtx) error {
				if attempts.Add(1) == 1 {
					return errors.New("retry scalar")
				}
				return p.Set(rc, result, 42)
			})))
			binds := []p.SubBind{p.Out(alias, outside), p.Out(result, result)}
			var inputs p.Inputs
			switch mode {
			case "fixed-skip":
				binds = append(binds, p.FixSkip(input))
			case "seeded-skip":
				streamAspectsMust(t, root.DeclareInput(p.NewPort(input)))
				streamAspectsMust(t, p.SkipSeed(&inputs, input))
				binds = append(binds, p.In(input, input))
			case "produced-skip":
				streamAspectsMust(t, root.Add(p.NewNode("absent-source", "", nil, p.Provides(input), func(rc *p.RunCtx) error {
					return p.Skip(rc, input)
				})))
				binds = append(binds, p.In(input, input))
			}
			streamAspectsMust(t, root.DeclareOutput(p.NewPort(outside), p.NewPort(result)))
			streamAspectsMust(t, p.Sub(root, "mount", "", binds, child, p.Retry(2, 0)))
			execution, err := root.NewExecution(ctx, inputs)
			streamAspectsMust(t, err)
			_, err = p.SubscribeStream(execution, outside)
			streamAspectsMust(t, err)
			streamAspectsMust(t, execution.Start())
			streamAspectsMust(t, execution.Wait())
			got, state, err := p.ReadOutput(execution, result)
			if err != nil || got != 42 || state != p.SlotStateReady || attempts.Load() != 2 {
				t.Fatalf("result=%d state=%v attempts=%d err=%v", got, state, attempts.Load(), err)
			}
			if _, state, err := p.ReadOutput(execution, outside); err != nil || state != p.SlotStateSkipped {
				t.Fatalf("missing renamed stream state=%v err=%v", state, err)
			}
		})
	}
}
