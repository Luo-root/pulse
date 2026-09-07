package bridge

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/observability"
)

// scripted 构造带模型标识与 token 用量的响应（工具调用响应 finish=tool_calls）。
func scripted(text string, calls ...llm.ToolCall) *llm.Response {
	r := &llm.Response{
		Model:        "test-model",
		Usage:        llm.TokenUsage{InputTokens: 10, OutputTokens: 5, CachedInputTokens: 2},
		FinishReason: llm.FinishStop,
	}
	if len(calls) > 0 {
		r.FinishReason = llm.FinishToolCalls
		parts := make([]llm.Part, 0, len(calls))
		for _, c := range calls {
			parts = append(parts, llm.Call(c))
		}
		r.Message = llm.Assistant(parts...)
		return r
	}
	r.Message = llm.AssistantText(text)
	return r
}

// newToolSet 注册一个 echo 型 lookup 工具，记录是否真实执行。
func newToolSet(ran *atomic.Int32) *loop.MemToolSet {
	ts := loop.NewMemToolSet()
	_ = ts.Register(llm.ToolDef{
		Name:       "lookup",
		Parameters: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, json.RawMessage) (string, error) {
		if ran != nil {
			ran.Add(1)
		}
		return "ok", nil
	})
	return ts
}

// newRegModel 经 Registry 装配 scripted 模型：事件派发在 observed
// 包装层（registry.go），裸 ScriptedModel 不发 llm 事件。
func newRegModel(t *testing.T, scope *kernel.Context, steps ...*llm.Response) llm.ChatModel {
	t.Helper()
	reg := llm.NewRegistry(scope)
	if _, err := reg.RegisterProvider(scope, "mock", func(llm.Config) (llm.ChatModel, error) {
		return llm.NewScripted(steps...), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Declare("main", llm.Config{Provider: "mock"}); err != nil {
		t.Fatal(err)
	}
	reg.SetDefault("main")
	m, err := reg.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// 全链路：scripted agent 一次工具调用 + 一步收尾，桥折叠出的记录
// 序列与 attrs 完整（Source/HostID/TraceID 逐条携带）。
func TestAttachFullChain(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}
	if _, err := Attach(scope, Config{Sink: sink, HostID: "h1", TraceID: "tr-1"}); err != nil {
		t.Fatal(err)
	}

	ran := &atomic.Int32{}
	model := newRegModel(t, scope,
		scripted("", llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)}),
		scripted("done"),
	)
	agent, err := loop.NewAgent(model, loop.WithToolSet(newToolSet(ran)), loop.WithEventScope(scope))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), nil, llm.UserText("call lookup")); err != nil {
		t.Fatal(err)
	}

	var gens, tools, turns int
	for _, rec := range sink.Snapshot() {
		if rec.HostID != "h1" || rec.TraceID != "tr-1" || rec.Source != observability.SourceBridge {
			t.Fatalf("envelope mismatch: %+v", rec)
		}
		switch rec.Event {
		case EventGenerateFinished:
			gens++
			if v, ok := observability.Get[string](rec.Attrs, llm.AttrModel); !ok || v != "test-model" {
				t.Fatalf("generate model attr: %q %v", v, ok)
			}
			if v, ok := observability.Get[int64](rec.Attrs, llm.AttrTokensIn); !ok || v != 10 {
				t.Fatalf("tokens_in attr: %d %v", v, ok)
			}
			if v, ok := observability.Get[int64](rec.Attrs, llm.AttrTokensOut); !ok || v != 5 {
				t.Fatalf("tokens_out attr: %d %v", v, ok)
			}
			if v, ok := observability.Get[int64](rec.Attrs, llm.AttrTokensCached); !ok || v != 2 {
				t.Fatalf("tokens_cached attr: %d %v", v, ok)
			}
		case EventToolFinished:
			tools++
			if rec.Status != StatusCompleted {
				t.Fatalf("tool status = %q, want completed", rec.Status)
			}
			if v, ok := observability.Get[string](rec.Attrs, loop.AttrTool); !ok || v != "lookup" {
				t.Fatalf("tool attr: %q %v", v, ok)
			}
		case EventTurnFinished:
			turns++
			if rec.Status != string(loop.StopCompleted) {
				t.Fatalf("turn status = %q, want completed", rec.Status)
			}
			if v, ok := observability.Get[int64](rec.Attrs, loop.AttrSteps); !ok || v != 2 {
				t.Fatalf("steps attr: %d %v", v, ok)
			}
		default:
			t.Fatalf("unexpected bridge event: %s", rec.Event)
		}
	}
	if gens != 2 || tools != 1 || turns != 1 {
		t.Fatalf("counts gens=%d tools=%d turns=%d, want 2/1/1", gens, tools, turns)
	}
	if ran.Load() != 1 {
		t.Fatalf("tool ran %d times, want 1", ran.Load())
	}
}

// 双请求共用同一 Sink：TraceID 按 scope 严格分组，A 的记录数在 B 运行
// 后不变（Local 派发隔离 + scope 销毁摘除的回归锚）。
func TestTraceIDIsolationAndDispose(t *testing.T) {
	sink := &observability.MemorySink{}

	scopeA := kernel.New()
	if _, err := Attach(scopeA, Config{Sink: sink, HostID: "h", TraceID: "tr-a"}); err != nil {
		t.Fatal(err)
	}
	agentA, err := loop.NewAgent(newRegModel(t, scopeA, scripted("a done")), loop.WithEventScope(scopeA))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentA.Run(context.Background(), nil, llm.UserText("a")); err != nil {
		t.Fatal(err)
	}
	countA := countTrace(sink, "tr-a")
	if countA == 0 {
		t.Fatal("request A produced no records")
	}
	scopeA.Dispose()

	// B 在 A 销毁后运行：不能出现 traceID=tr-a 的新记录。
	scopeB := kernel.New()
	t.Cleanup(scopeB.Dispose)
	if _, err := Attach(scopeB, Config{Sink: sink, HostID: "h", TraceID: "tr-b"}); err != nil {
		t.Fatal(err)
	}
	agentB, err := loop.NewAgent(newRegModel(t, scopeB, scripted("b done")), loop.WithEventScope(scopeB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentB.Run(context.Background(), nil, llm.UserText("b")); err != nil {
		t.Fatal(err)
	}
	if got := countTrace(sink, "tr-a"); got != countA {
		t.Fatalf("request A grew after dispose: %d -> %d", countA, got)
	}
	if countTrace(sink, "tr-b") == 0 {
		t.Fatal("request B produced no records")
	}
}

// HITL 中立：桥不订阅 before_tool_call（AfterToolCall 自带 Duration/Err，
// 订阅无观测增益）——审批监听只被调到一次；拒绝后桥如实记 rejected，
// 工具未执行。
func TestBridgeIsHITLNeutral(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}
	if _, err := Attach(scope, Config{Sink: sink, HostID: "h", TraceID: "tr-h"}); err != nil {
		t.Fatal(err)
	}

	// Attach 之后挂审批监听：若桥也订阅了 before_tool_call，
	// 回调次数会超过 1。
	var hitlCount atomic.Int32
	if _, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
		func(btc *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
			hitlCount.Add(1)
			btc.Rejected = true
			btc.RejectReason = "denied by policy"
			return btc // 拒绝语义：不委托 next
		}); err != nil {
		t.Fatal(err)
	}

	ran := &atomic.Int32{}
	model := newRegModel(t, scope,
		scripted("", llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)}),
		scripted("ok"),
	)
	agent, err := loop.NewAgent(model, loop.WithToolSet(newToolSet(ran)), loop.WithEventScope(scope))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), nil, llm.UserText("denied")); err != nil {
		t.Fatal(err)
	}

	if got := hitlCount.Load(); got != 1 {
		t.Fatalf("before_tool_call listeners fired %d times, want 1 (bridge gains nothing by subscribing)", got)
	}
	if ran.Load() != 0 {
		t.Fatal("rejected tool must not execute")
	}
	found := false
	for _, rec := range sink.Snapshot() {
		if rec.Event == EventToolFinished {
			if rec.Status != StatusRejected {
				t.Fatalf("rejected tool status = %q, want rejected", rec.Status)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no tool_finished record for rejected call")
	}
}

// Collector 服务化：Attach 后业务插件经 CollectorKey 直写，
// HostID/TraceID 自动携带。
func TestCollectorServiceWrite(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}
	if _, err := Attach(scope, Config{Sink: sink, HostID: "h9", TraceID: "tr-9"}); err != nil {
		t.Fatal(err)
	}

	c, ok := kernel.Get(scope, CollectorKey)
	if !ok {
		t.Fatal("collector service not found in request scope")
	}
	c.Write("app.evt", "done")
	c.WriteAttrs("app.custom", "ok", func(a *observability.Attrs) {
		observability.Set(a, "app.k", "v")
	})

	var sawEvt, sawCustom bool
	for _, rec := range sink.Snapshot() {
		if rec.Event != "app.evt" && rec.Event != "app.custom" {
			continue
		}
		if rec.HostID != "h9" || rec.TraceID != "tr-9" || rec.Source != observability.SourceBridge {
			t.Fatalf("collector record envelope mismatch: %+v", rec)
		}
		switch rec.Event {
		case "app.evt":
			sawEvt = rec.Status == "done"
		case "app.custom":
			sawCustom = true
			if v, _ := observability.Get[string](rec.Attrs, "app.k"); v != "v" {
				t.Fatalf("custom attr = %q", v)
			}
		}
	}
	if !sawEvt || !sawCustom {
		t.Fatalf("collector writes missing: evt=%v custom=%v", sawEvt, sawCustom)
	}
}

// FlowObserver 分段计时：线性节点 wait/run 两条记录；跳过节点只有
// 一条 skipped 等待记录；nodeID 走 flow.AttrNode，不占具名字段。
func TestFlowObserverSegments(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}
	b, err := Attach(scope, Config{Sink: sink, HostID: "h", TraceID: "tr-f"})
	if err != nil {
		t.Fatal(err)
	}

	in := flow.NewKey[string]("obs.in")
	out := flow.NewKey[string]("obs.out")
	g := flow.New(context.Background(), flow.WithObserver(b.FlowObserver()))
	if err := flow.Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(flow.NewNode("n", flow.Requires(in), flow.Provides(out), func(rc *flow.RunCtx) error {
		time.Sleep(5 * time.Millisecond)
		v, err := flow.Get(rc, in)
		if err != nil {
			return err
		}
		return flow.Set(rc, out, v)
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	var waits, runs int
	for _, rec := range sink.Snapshot() {
		switch rec.Event {
		case EventNodeWaitFinished:
			waits++
			if rec.Status != "running" {
				t.Fatalf("wait status = %q, want running", rec.Status)
			}
		case EventNodeRunFinished:
			runs++
			if rec.Status != string(flow.NodeCompleted) {
				t.Fatalf("run status = %q, want completed", rec.Status)
			}
			if rec.Duration <= 0 {
				t.Fatal("run segment duration should be > 0")
			}
		}
		if rec.Event == EventNodeWaitFinished || rec.Event == EventNodeRunFinished {
			if v, ok := observability.Get[string](rec.Attrs, flow.AttrNode); !ok || v != "n" {
				t.Fatalf("node attr: %q %v", v, ok)
			}
			if rec.FiberName != "" {
				t.Fatalf("FlowObserver must not use named field FiberName: %+v", rec)
			}
		}
	}
	if waits != 1 || runs != 1 {
		t.Fatalf("segments waits=%d runs=%d, want 1/1", waits, runs)
	}
}

// 多节点链：wait/run 记录按 nodeID（flow.AttrNode）严格区分。
func TestFlowObserverTwoNodeIdentities(t *testing.T) {
	sink := &observability.MemorySink{}
	b := New(Config{Sink: sink, HostID: "h", TraceID: "tr-chain"})

	k1 := flow.NewKey[string]("obs.chain.1")
	k2 := flow.NewKey[string]("obs.chain.2")
	k3 := flow.NewKey[string]("obs.chain.3")
	g := flow.New(context.Background(), flow.WithObserver(b.FlowObserver()))
	if err := flow.Seed(g, k1, "x"); err != nil {
		t.Fatal(err)
	}
	pass := func(req, ack flow.Key[string]) func(*flow.RunCtx) error {
		return func(rc *flow.RunCtx) error {
			v, err := flow.Get(rc, req)
			if err != nil {
				return err
			}
			return flow.Set(rc, ack, v)
		}
	}
	if err := g.Add(flow.NewNode("extract", flow.Requires(k1), flow.Provides(k2), pass(k1, k2))); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(flow.NewNode("answer", flow.Requires(k2), flow.Provides(k3), pass(k2, k3))); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	waitNodes := map[string]int{}
	runNodes := map[string]int{}
	for _, rec := range sink.Snapshot() {
		id, _ := observability.Get[string](rec.Attrs, flow.AttrNode)
		switch rec.Event {
		case EventNodeWaitFinished:
			waitNodes[id]++
		case EventNodeRunFinished:
			runNodes[id]++
		}
	}
	for _, id := range []string{"extract", "answer"} {
		if waitNodes[id] != 1 || runNodes[id] != 1 {
			t.Fatalf("node %s waits=%d runs=%d; waitMap=%v runMap=%v",
				id, waitNodes[id], runNodes[id], waitNodes, runNodes)
		}
	}
}

// 跳过路径：SkipSeed 下游节点只有一条 skipped 等待记录，无运行记录。
func TestFlowObserverSkip(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}
	b, err := Attach(scope, Config{Sink: sink, HostID: "h", TraceID: "tr-s"})
	if err != nil {
		t.Fatal(err)
	}

	a := flow.NewKey[string]("obs.sa")
	g := flow.New(context.Background(), flow.WithObserver(b.FlowObserver()))
	if err := flow.SkipSeed(g, a); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(flow.NewNode("down", flow.Requires(a), nil, func(rc *flow.RunCtx) error {
		t.Fatal("Run must not execute when input skipped")
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	var waits, runs int
	for _, rec := range sink.Snapshot() {
		switch rec.Event {
		case EventNodeWaitFinished:
			waits++
			if rec.Status != string(flow.NodeSkipped) {
				t.Fatalf("skip wait status = %q, want skipped", rec.Status)
			}
		case EventNodeRunFinished:
			runs++
		}
	}
	if waits != 1 || runs != 0 {
		t.Fatalf("skip segments waits=%d runs=%d, want 1/0", waits, runs)
	}
}

func countTrace(sink *observability.MemorySink, traceID string) int {
	n := 0
	for _, rec := range sink.Snapshot() {
		if rec.TraceID == traceID {
			n++
		}
	}
	return n
}
