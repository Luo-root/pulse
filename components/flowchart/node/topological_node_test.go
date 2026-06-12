package node

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Luo-root/pulse/components/flow"
)

func TestTopologicalNode_LinearChain(t *testing.T) {
	var order []string
	var mu = &sync.Mutex{}

	record := func(id string) func(*flow.FlowContext, map[string]any) (map[string]any, error) {
		return func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
			return map[string]any{id + "_out": true}, nil
		}
	}

	a := NewNode("a", nil, []string{"a_out"}, record("a"))
	b := NewNode("b", []string{"a_out"}, []string{"b_out"}, record("b"))
	c := NewNode("c", []string{"b_out"}, []string{"c_out"}, record("c"))

	tn, err := NewTopologicalNode("topo", []Node{c, a, b}, []string{"c_out"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx := flow.NewFlowContext(context.Background())
	_, err = tn.Run(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(order) != 3 {
		t.Fatalf("expected 3 executions, got %d", len(order))
	}
	if order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("expected order [a b c], got %v", order)
	}
}

func mockRun(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
	return map[string]any{"result": true}, nil
}

func TestTopologicalNode_DuplicateOutput(t *testing.T) {
	a := NewNode("a", nil, []string{"x"}, mockRun)
	b := NewNode("b", nil, []string{"x"}, mockRun)

	_, err := NewTopologicalNode("topo", []Node{a, b}, nil)
	if err == nil {
		t.Fatal("expected duplicate output error")
	}
}

func TestTopologicalNode_NodeError_StopsExecution(t *testing.T) {
	var cRan atomic.Bool

	a := NewNode("a", nil, []string{"x"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return nil, fmt.Errorf("a failed")
	})
	b := NewNode("b", []string{"x"}, []string{"y"}, mockRun)
	c := NewNode("c", nil, nil, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		cRan.Store(true)
		return nil, nil
	})

	tn, _ := NewTopologicalNode("topo", []Node{a, b, c}, nil)
	ctx := flow.NewFlowContext(context.Background())
	_, err := tn.Run(ctx, nil)

	if err == nil {
		t.Fatal("expected error from node a")
	}
}

func TestTopologicalNode_ExternalInputs(t *testing.T) {
	a := NewNode("a", []string{"external"}, []string{"x"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return map[string]any{"x": inputs["external"].(string) + "_done"}, nil
	})

	tn, _ := NewTopologicalNode("topo", []Node{a}, []string{"x"})

	// Inputs 应该包含 "external"
	inputs := tn.Inputs()
	found := false
	for _, in := range inputs {
		if in == "external" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'external' in Inputs(), got %v", inputs)
	}

	ctx := flow.NewFlowContext(context.Background())
	result, err := tn.Run(ctx, map[string]any{"external": "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["x"] != "hello_done" {
		t.Fatalf("expected 'hello_done', got %v", result["x"])
	}
}
