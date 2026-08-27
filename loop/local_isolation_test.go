package loop

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
)

// 双 Agent 隔离：A 的 tool 事件不得进入 B 的 listener。
func TestRequestScopedAgentsDoNotCrossTalk(t *testing.T) {
	root := kernel.New()
	defer root.Dispose()
	scopeA, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer scopeA.Dispose()
	scopeB, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer scopeB.Dispose()

	var aTools, bTools atomic.Int32
	if _, err := kernel.On(scopeA, EventAfterToolCall, func(*AfterToolCall) { aTools.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if _, err := kernel.On(scopeB, EventAfterToolCall, func(*AfterToolCall) { bTools.Add(1) }); err != nil {
		t.Fatal(err)
	}

	tools := NewMemToolSet()
	if err := tools.Register(llm.ToolDef{
		Name:       "lookup",
		Parameters: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("done"),
	)
	agentA, err := NewAgent(model, WithToolSet(tools), WithEventScope(scopeA))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentA.Run(context.Background(), nil, llm.UserText("call lookup")); err != nil {
		t.Fatal(err)
	}
	if aTools.Load() != 1 || bTools.Load() != 0 {
		t.Fatalf("cross-talk: A=%d B=%d", aTools.Load(), bTools.Load())
	}
}

// HITL 隔离：A 拒绝策略不得影响 B。
func TestRequestScopedHITLPoliciesDoNotCrossTalk(t *testing.T) {
	root := kernel.New()
	defer root.Dispose()
	scopeA, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer scopeA.Dispose()
	scopeB, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer scopeB.Dispose()

	if _, err := kernel.OnWaterfall(scopeA, EventBeforeToolCall,
		func(btc *BeforeToolCall, next func(*BeforeToolCall) *BeforeToolCall) *BeforeToolCall {
			btc.Rejected = true
			btc.RejectReason = "denied by A"
			return btc
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := kernel.OnWaterfall(scopeB, EventBeforeToolCall,
		func(btc *BeforeToolCall, next func(*BeforeToolCall) *BeforeToolCall) *BeforeToolCall {
			return next(btc)
		}); err != nil {
		t.Fatal(err)
	}

	var ranA, ranB atomic.Int32
	toolsA := NewMemToolSet()
	_ = toolsA.Register(llm.ToolDef{Name: "danger", Parameters: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, json.RawMessage) (string, error) {
			ranA.Add(1)
			return "a", nil
		})
	toolsB := NewMemToolSet()
	_ = toolsB.Register(llm.ToolDef{Name: "danger", Parameters: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, json.RawMessage) (string, error) {
			ranB.Add(1)
			return "b", nil
		})

	modelA := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "a1", Name: "danger", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("a done"),
	)
	modelB := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "b1", Name: "danger", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("b done"),
	)
	agentA, _ := NewAgent(modelA, WithToolSet(toolsA), WithEventScope(scopeA))
	agentB, _ := NewAgent(modelB, WithToolSet(toolsB), WithEventScope(scopeB))

	if _, err := agentA.Run(context.Background(), nil, llm.UserText("danger")); err != nil {
		t.Fatal(err)
	}
	if _, err := agentB.Run(context.Background(), nil, llm.UserText("danger")); err != nil {
		t.Fatal(err)
	}
	if ranA.Load() != 0 {
		t.Fatalf("A should be rejected, ran=%d", ranA.Load())
	}
	if ranB.Load() != 1 {
		t.Fatalf("B should run despite A's deny policy, ran=%d", ranB.Load())
	}
}
