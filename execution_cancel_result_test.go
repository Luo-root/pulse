package pulse_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Luo-root/pulse"
)

// 节点正常返回与整轮取消分别判断；这也是中英文取消指南的可执行依据。
func TestCompletedBodyRetainsHostCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, err := pulse.New("completed-body-canceled-run", "业务体退出与整轮终态")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Add(pulse.NewNode("work", "取消宿主后正常返回", nil, nil, func(*pulse.RunCtx) error {
		cancel()
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	e, err := g.NewExecution(ctx, pulse.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.Wait(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait = %v, want host cancellation", err)
		}
	}
	state := e.Snapshot()
	if state.Nodes[0].Status != pulse.NodeStatusCompleted || state.Nodes[0].Err != nil {
		t.Fatalf("body returned nil: node = %+v", state.Nodes[0])
	}
	if state.Graph.Status != pulse.GraphStatusCanceled || !errors.Is(state.Graph.Err, context.Canceled) {
		t.Fatalf("run canceled before finalization: graph = %+v", state.Graph)
	}
}
