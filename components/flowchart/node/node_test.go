package node

import (
	"context"
	"testing"

	"github.com/Luo-root/pulse/components/flowchart/flow"
)

// ============================================================
// SimpleNode 测试
// ============================================================

func TestSimpleNode_BasicExecution(t *testing.T) {
	n := NewNode("test", []string{"input"}, []string{"output"},
		func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
			return map[string]any{"output": "result"}, nil
		},
	)

	if n.ID() != "test" {
		t.Fatalf("expected ID 'test', got %q", n.ID())
	}
	if len(n.Inputs()) != 1 || n.Inputs()[0] != "input" {
		t.Fatalf("unexpected inputs: %v", n.Inputs())
	}
	if len(n.Outputs()) != 1 || n.Outputs()[0] != "output" {
		t.Fatalf("unexpected outputs: %v", n.Outputs())
	}
}

func TestSimpleNode_AddAspect(t *testing.T) {
	n := NewNode("test", nil, nil,
		func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
			return nil, nil
		},
	)

	aspect := AroundFunc(func(ctx *AspectContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
		return next()
	})
	n.AddAspect(aspect)

	if len(n.Aspects()) != 1 {
		t.Fatalf("expected 1 aspect, got %d", len(n.Aspects()))
	}
}

func TestSimpleNode_Run(t *testing.T) {
	n := NewNode("test", nil, []string{"out"},
		func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
			return map[string]any{"out": 42}, nil
		},
	)

	fc := flow.NewFlowContext(context.Background())
	outputs, err := n.Run(fc, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outputs["out"] != 42 {
		t.Errorf("expected 42, got %v", outputs["out"])
	}
}
