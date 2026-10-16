package pulse_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse"
)

func compositionOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestApprovalAspectLeavesQuotaAvailableBeforeNext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	waiting, approved, otherRan := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	gate := func(rc *pulse.RunCtx, next func(*pulse.RunCtx) error) error {
		close(waiting)
		select {
		case <-approved:
			return next(rc)
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	}
	g, err := pulse.New("approval-aspect", "审批只控制是否推进业务体", pulse.WithMaxRunning(1))
	compositionOK(t, err)
	compositionOK(t, g.Add(pulse.NewNode("gated", "批准前不进入", nil, nil, func(*pulse.RunCtx) error {
		calls.Add(1)
		return nil
	}, gate)))
	compositionOK(t, g.Add(pulse.NewNode("other", "审批等待时仍可工作", nil, nil, func(rc *pulse.RunCtx) error {
		select {
		case <-waiting:
			close(otherRan)
			return nil
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	})))
	e, err := g.NewExecution(ctx, pulse.Inputs{})
	compositionOK(t, err)
	defer e.Cancel()
	compositionOK(t, e.Start())
	select {
	case <-otherRan:
	case <-ctx.Done():
		t.Fatalf("pre-next approval occupied the only business quota: %v", ctx.Err())
	}
	if calls.Load() != 0 || e.Snapshot().Nodes[0].Status != pulse.NodeStatusPending {
		t.Fatal("business entered before approval")
	}
	close(approved)
	compositionOK(t, e.Wait())
	if calls.Load() != 1 {
		t.Fatalf("approved business calls = %d", calls.Load())
	}
}

func TestLoopNodeCallsReusableGraphTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	input, output := pulse.NewKey[int]("tool.input"), pulse.NewKey[int]("tool.output")
	tool, err := pulse.New("increment-tool", "可重复调用的工具定义")
	compositionOK(t, err)
	compositionOK(t, tool.DeclareInput(pulse.NewPort(input)))
	compositionOK(t, tool.DeclareOutput(pulse.NewPort(output)))
	var calls atomic.Int32
	compositionOK(t, tool.Add(pulse.NewNode("increment", "每次执行增加一", pulse.Requires(input), pulse.Provides(output), func(rc *pulse.RunCtx) error {
		value, err := pulse.Get(rc, input)
		if err != nil {
			return err
		}
		calls.Add(1)
		return pulse.Set(rc, output, value+1)
	})))
	invoke := func(ctx context.Context, value int) (int, error) {
		var in pulse.Inputs
		if err := pulse.Seed(&in, input, value); err != nil {
			return 0, err
		}
		e, err := tool.NewExecution(ctx, in)
		if err != nil {
			return 0, err
		}
		defer e.Cancel()
		if err := e.Start(); err != nil {
			return 0, err
		}
		if err := e.Wait(); err != nil {
			return 0, err
		}
		result, _, err := pulse.ReadOutput(e, output)
		return result, err
	}
	answer := pulse.NewKey[int]("answer")
	g, err := pulse.New("agent", "循环与选工具由业务节点负责", pulse.WithMaxRunning(1))
	compositionOK(t, err)
	compositionOK(t, g.DeclareOutput(pulse.NewPort(answer)))
	compositionOK(t, g.Add(pulse.NewNode("react", "两轮确定性决策替身", nil, pulse.Provides(answer), func(rc *pulse.RunCtx) error {
		value := 0
		// 替身确定性地调用两次工具；不依赖远端模型，也不修改 Graph 定义。
		for round := 0; round < 2; round++ {
			var err error
			value, err = invoke(rc.Context(), value)
			if err != nil {
				return err
			}
		}
		return pulse.Set(rc, answer, value)
	})))
	e, err := g.NewExecution(ctx, pulse.Inputs{})
	compositionOK(t, err)
	defer e.Cancel()
	compositionOK(t, e.Start())
	compositionOK(t, e.Wait())
	value, state, err := pulse.ReadOutput(e, answer)
	compositionOK(t, err)
	if value != 2 || state != pulse.SlotStateReady || calls.Load() != 2 {
		t.Fatalf("answer=%d state=%s tool calls=%d", value, state, calls.Load())
	}
}
