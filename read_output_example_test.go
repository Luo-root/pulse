package pulse_test

import (
	"context"
	"fmt"
	"time"

	"github.com/Luo-root/pulse"
)

func ExampleReadOutput() {
	input := pulse.NewKey[int]("input")
	output := pulse.NewKey[int]("output")
	g, err := pulse.New("double", "读取本轮声明出口")
	if err != nil {
		panic(err)
	}
	if err := g.DeclareInput(pulse.NewPort(input)); err != nil {
		panic(err)
	}
	if err := g.DeclareOutput(pulse.NewPort(output)); err != nil {
		panic(err)
	}
	if err := g.Add(pulse.NewNode("double", "负数没有结果", pulse.Requires(input), pulse.Provides(output), func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, input)
		if err != nil {
			return err
		}
		if v < 0 {
			return pulse.NoValue()
		}
		return pulse.Set(rc, output, v*2)
	})); err != nil {
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, v := range []int{3, -1} {
		var in pulse.Inputs
		if err := pulse.Seed(&in, input, v); err != nil {
			panic(err)
		}
		e, err := g.NewExecution(ctx, in)
		if err != nil {
			panic(err)
		}
		if err := e.Start(); err != nil {
			panic(err)
		}
		if err := e.Wait(); err != nil {
			panic(err)
		}
		value, state, err := pulse.ReadOutput(e, output)
		if err != nil {
			panic(err)
		}
		switch state {
		case pulse.SlotStateReady:
			fmt.Println("result:", value)
		case pulse.SlotStateSkipped:
			fmt.Println("result: skipped")
		}
	}
	// Output:
	// result: 6
	// result: skipped
}
