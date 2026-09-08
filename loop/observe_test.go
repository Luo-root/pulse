package loop

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/observability"
)

func newTestToolSet(ran *atomic.Int32, fail bool) *MemToolSet {
	ts := NewMemToolSet()
	_ = ts.Register(llm.ToolDef{
		Name:       "lookup",
		Parameters: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, json.RawMessage) (string, error) {
		if ran != nil {
			ran.Add(1)
		}
		if fail {
			return "", errors.New("boom")
		}
		return "ok", nil
	})
	return ts
}

// 折叠锚：一次工具调用 + 收尾，tool_finished 与 turn_finished 逐项断言。
func TestObserveFoldsToolAndTurn(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}
	if err := Observe(scope, observability.ObserveConfig{Sink: sink, HostID: "h1", TraceID: "tr-1"}); err != nil {
		t.Fatal(err)
	}

	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("done"),
	)
	agent, err := NewAgent(model, "test", WithToolSet(newTestToolSet(nil, false)), WithEventScope(scope))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), nil, llm.UserText("call lookup")); err != nil {
		t.Fatal(err)
	}

	var sawTool, sawTurn bool
	for _, rec := range sink.Snapshot() {
		if rec.HostID != "h1" || rec.TraceID != "tr-1" || rec.Source != observability.SourceAdapter {
			t.Fatalf("envelope mismatch: %+v", rec)
		}
		switch rec.Event {
		case EventToolFinished:
			sawTool = true
			if rec.Status != StatusCompleted {
				t.Fatalf("tool status = %q, want completed", rec.Status)
			}
			if rec.Duration < 0 {
				t.Fatalf("tool duration = %v", rec.Duration)
			}
			if v, ok := observability.Get[string](rec.Attrs, AttrTool); !ok || v != "lookup" {
				t.Fatalf("tool attr: %q %v", v, ok)
			}
		case EventTurnFinished:
			sawTurn = true
			if rec.Status != string(StopCompleted) {
				t.Fatalf("turn status = %q, want completed", rec.Status)
			}
			if v, ok := observability.Get[int64](rec.Attrs, AttrSteps); !ok || v != 2 {
				t.Fatalf("steps attr: %v %v", v, ok)
			}
			if _, ok := observability.Get[int64](rec.Attrs, llm.AttrTokensIn); ok {
				t.Fatal("turn_finished must not repeat token usage (single-call figure is authoritative)")
			}
		}
	}
	if !sawTool || !sawTurn {
		t.Fatalf("missing records: tool=%v turn=%v", sawTool, sawTurn)
	}
}

// 失败路径：工具错误 → Status=failed 且 Err 透传。
func TestObserveToolFailure(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}
	if err := Observe(scope, observability.ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr"}); err != nil {
		t.Fatal(err)
	}

	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("recovered"),
	)
	agent, err := NewAgent(model, "test", WithToolSet(newTestToolSet(nil, true)), WithEventScope(scope))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), nil, llm.UserText("boom")); err != nil {
		t.Fatal(err)
	}

	var saw bool
	for _, rec := range sink.Snapshot() {
		if rec.Event != EventToolFinished {
			continue
		}
		saw = true
		if rec.Status != StatusFailed {
			t.Fatalf("status = %q, want failed", rec.Status)
		}
		if rec.Err == nil || rec.Err.Error() != "boom" {
			t.Fatalf("err passthrough: %v", rec.Err)
		}
	}
	if !saw {
		t.Fatal("no tool_finished record")
	}
}

// HITL 中立锚（三重断言，缺一不可）：审批监听恰一次 + 工具未执行 +
// tool_finished Status=rejected——只数监听次数无法发现「订阅但行为污染」。
func TestObserveHITLNeutral(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}
	if err := Observe(scope, observability.ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-h"}); err != nil {
		t.Fatal(err)
	}

	// Observe 之后挂审批监听：若 Observe 也订阅了 before_tool_call，
	// 回调次数会超过 1。
	var hitlCount atomic.Int32
	if _, err := kernel.OnWaterfall(scope, EventBeforeToolCall,
		func(btc *BeforeToolCall, next func(*BeforeToolCall) *BeforeToolCall) *BeforeToolCall {
			hitlCount.Add(1)
			btc.Rejected = true
			btc.RejectReason = "denied by policy"
			return btc // 拒绝语义：不委托 next
		}); err != nil {
		t.Fatal(err)
	}

	ran := &atomic.Int32{}
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("ok"),
	)
	agent, err := NewAgent(model, "test", WithToolSet(newTestToolSet(ran, false)), WithEventScope(scope))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), nil, llm.UserText("denied")); err != nil {
		t.Fatal(err)
	}

	// 断言 1：审批监听恰一次（Observe 不订阅 before_tool_call）。
	if got := hitlCount.Load(); got != 1 {
		t.Fatalf("before_tool_call fired %d times, want 1", got)
	}
	// 断言 2：被拒工具未真实执行。
	if ran.Load() != 0 {
		t.Fatal("rejected tool must not execute")
	}
	// 断言 3：折叠如实记 rejected。
	var saw bool
	for _, rec := range sink.Snapshot() {
		if rec.Event == EventToolFinished {
			if rec.Status != StatusRejected {
				t.Fatalf("rejected tool status = %q, want rejected", rec.Status)
			}
			saw = true
		}
	}
	if !saw {
		t.Fatal("no tool_finished record for rejected call")
	}
}

// 可选注册：不调 Observe 即零观测足迹。
func TestObserveOptional(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}

	model := llm.NewScripted(llm.Resp("silent"))
	agent, err := NewAgent(model, "test", WithEventScope(scope))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), nil, llm.UserText("q")); err != nil {
		t.Fatal(err)
	}
	if len(sink.Snapshot()) != 0 {
		t.Fatalf("no observe attached but sink has %d records", len(sink.Snapshot()))
	}
}

// 隔离与摘除：双 scope 共用同一 Sink，A Dispose 后 B 运行 A 零增长。
func TestObserveIsolationAndDispose(t *testing.T) {
	sink := &observability.MemorySink{}
	countTrace := func(id string) int {
		n := 0
		for _, rec := range sink.Snapshot() {
			if rec.TraceID == id {
				n++
			}
		}
		return n
	}

	scopeA := kernel.New()
	if err := Observe(scopeA, observability.ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-a"}); err != nil {
		t.Fatal(err)
	}
	agentA, err := NewAgent(llm.NewScripted(llm.Resp("a")), "a", WithEventScope(scopeA))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentA.Run(context.Background(), nil, llm.UserText("a")); err != nil {
		t.Fatal(err)
	}
	countA := countTrace("tr-a")
	if countA == 0 {
		t.Fatal("request A produced no records")
	}
	scopeA.Dispose()

	scopeB := kernel.New()
	t.Cleanup(scopeB.Dispose)
	if err := Observe(scopeB, observability.ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-b"}); err != nil {
		t.Fatal(err)
	}
	agentB, err := NewAgent(llm.NewScripted(llm.Resp("b")), "b", WithEventScope(scopeB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentB.Run(context.Background(), nil, llm.UserText("b")); err != nil {
		t.Fatal(err)
	}
	if got := countTrace("tr-a"); got != countA {
		t.Fatalf("request A grew after dispose: %d -> %d", countA, got)
	}
	if countTrace("tr-b") == 0 {
		t.Fatal("request B produced no records")
	}
}

// 装配校验：nil scope / nil Sink 哨兵。
func TestObserveValidation(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	if err := Observe(nil, observability.ObserveConfig{Sink: &observability.MemorySink{}}); err != observability.ErrNilScope {
		t.Fatalf("nil scope err = %v", err)
	}
	if err := Observe(scope, observability.ObserveConfig{TraceID: "t"}); err != observability.ErrNilSink {
		t.Fatalf("nil sink err = %v", err)
	}
}

// 归因锚（#144）：同 scope 并发双 Agent——turn_finished 的 Agent
// 名折为 AttrAgent，两实例记录互不串扰；-race 下无共享竞态。
func TestObserveConcurrentAgentAttribution(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}
	if err := Observe(scope, observability.ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-multi"}); err != nil {
		t.Fatal(err)
	}

	const perAgent = 4
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b"} {
		agent, err := NewAgent(llm.NewScripted(llm.Resp("done")), name, WithEventScope(scope))
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perAgent; i++ {
				if _, err := agent.Run(context.Background(), nil, llm.UserText("hi")); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	counts := map[string]int{}
	for _, rec := range sink.Snapshot() {
		if rec.Event != EventTurnFinished {
			continue
		}
		v, ok := observability.Get[string](rec.Attrs, AttrAgent)
		if !ok {
			t.Fatalf("turn_finished missing %s attr: %+v", AttrAgent, rec)
		}
		counts[v]++
	}
	if counts["a"] != perAgent || counts["b"] != perAgent {
		t.Fatalf("agent attribution = %v, want a=%d b=%d", counts, perAgent, perAgent)
	}
}
