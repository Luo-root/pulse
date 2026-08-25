package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
)

// fakeTools 是可控的 ToolSet：记录每次调用并按 fn 行为响应。
type fakeTools struct {
	mu    sync.Mutex
	calls []llm.ToolCall
	fn    func(call llm.ToolCall) (string, error)
}

func (f *fakeTools) Definitions() []llm.ToolDef {
	return []llm.ToolDef{{
		Name:        "echo",
		Description: "echo tool for tests",
		Parameters:  []byte(`{"type":"object","properties":{"text":{"type":"string"}}}`),
	}}
}

func (f *fakeTools) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	return f.fn(call)
}

func (f *fakeTools) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// resp 构造带用量的响应。
func respWith(usage llm.TokenUsage, parts ...llm.Part) *llm.Response {
	return &llm.Response{
		Message:      llm.Assistant(parts...),
		FinishReason: llm.FinishStop,
		Usage:        usage,
	}
}

// trace 收集事件轨迹（事件名 + 关键字段），用于断言完整还原。
type trace struct {
	mu   sync.Mutex
	line []string
}

func (tr *trace) record(s string) {
	tr.mu.Lock()
	tr.line = append(tr.line, s)
	tr.mu.Unlock()
}

func (tr *trace) joined() string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return strings.Join(tr.line, " | ")
}

// attachTrace 把全部 loop 事件的监听挂到 scope 上按序记录
// （before_tool_call 是 waterfall 观察者，只记录并委托）。
func attachTrace(t *testing.T, scope *kernel.Context) *trace {
	t.Helper()
	tr := &trace{}

	subs := []struct {
		reg func() error
	}{
		{func() error { _, e := kernel.On(scope, EventTurnStart, func(p *TurnStart) { tr.record("turn_start") }); return e }},
		{func() error { _, e := kernel.On(scope, EventStepStart, func(p *StepStart) {
			tr.record(fmt.Sprintf("step_start(%d)", p.Step))
		}); return e }},
		{func() error { _, e := kernel.On(scope, EventAfterModel, func(p *AfterModel) {
			tr.record(fmt.Sprintf("after_model(%s)", p.Response.Message.Text()))
		}); return e }},
		{func() error { _, e := kernel.OnWaterfall(scope, EventBeforeToolCall, func(p *BeforeToolCall, next func(*BeforeToolCall) *BeforeToolCall) *BeforeToolCall {
			tr.record(fmt.Sprintf("before_tool(%s)", p.Call.Name))
			return next(p)
		}); return e }},
		{func() error { _, e := kernel.On(scope, EventAfterToolCall, func(p *AfterToolCall) {
			tr.record(fmt.Sprintf("after_tool(%s=%q err=%v rej=%v)", p.Call.Name, p.Result, p.Err != nil, p.Rejected))
		}); return e }},
		{func() error { _, e := kernel.On(scope, EventTurnEnd, func(p *TurnEnd) {
			tr.record(fmt.Sprintf("turn_end(%s,steps=%d,in=%d,out=%d)", p.StoppedBy, p.Steps, p.Usage.InputTokens, p.Usage.OutputTokens))
		}); return e }},
	}
	for i, s := range subs {
		if err := s.reg(); err != nil {
			t.Fatalf("attach event %d: %v", i, err)
		}
	}
	return tr
}

func newTestAgent(t *testing.T, model llm.ChatModel, ts ToolSet, scope *kernel.Context) *Agent {
	t.Helper()
	a, err := NewAgent(model, WithToolSet(ts), WithEventScope(scope), WithSystemPrompt("you are a test"))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return a
}

// 验收 1：无工具纯对话一步完成；Messages 含 system+user+assistant。
func TestRunCompletesWithoutTools(t *testing.T) {
	scope := kernel.New()
	a := newTestAgent(t, llm.NewScripted(llm.Resp("hi there")), nil, scope)

	res, err := a.Run(context.Background(), nil, llm.UserText("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final == nil || res.Final.Text() != "hi there" {
		t.Fatalf("final = %v", res.Final)
	}
	if res.Steps != 1 || res.StoppedBy != StopCompleted {
		t.Fatalf("steps=%d stoppedBy=%s", res.Steps, res.StoppedBy)
	}
	if len(res.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(res.Messages))
	}
	roles := []llm.Role{res.Messages[0].Role, res.Messages[1].Role, res.Messages[2].Role}
	if roles[0] != llm.RoleSystem || roles[1] != llm.RoleUser || roles[2] != llm.RoleAssistant {
		t.Fatalf("message roles wrong: %v", roles)
	}
}

// 验收 1+4：两步工具循环；工具结果进入 Messages；Final 为最终回复。
func TestTwoStepToolLoop(t *testing.T) {
	scope := kernel.New()
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: []byte(`{"text":"ping"}`)}),
		respWith(llm.TokenUsage{InputTokens: 3, OutputTokens: 2}, llm.Text("done: ping")),
	)
	tools := &fakeTools{fn: func(call llm.ToolCall) (string, error) { return "pong", nil }}
	a := newTestAgent(t, model, tools, scope)

	res, err := a.Run(context.Background(), []*llm.Message{llm.UserText("earlier")}, llm.UserText("call echo"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final == nil || res.Final.Text() != "done: ping" {
		t.Fatalf("final = %v", res.Final)
	}
	if res.Steps != 2 || res.StoppedBy != StopCompleted {
		t.Fatalf("steps=%d stoppedBy=%s", res.Steps, res.StoppedBy)
	}
	if got := tools.callCount(); got != 1 {
		t.Fatalf("tool calls = %d, want 1", got)
	}
	if res.Usage.OutputTokens != 2 {
		t.Fatalf("usage not accumulated: %+v", res.Usage)
	}
	// 本回合新增消息里应有一条 echo 的成功结果。
	var sawPong bool
	for _, m := range res.Messages {
		for _, part := range m.Parts {
			if p := part.ToolResultValue; p != nil && !p.IsError {
				for _, c := range p.Content {
					if c.Text == "pong" && p.ToolCallID == "c1" {
						sawPong = true
					}
				}
			}
		}
	}
	if !sawPong {
		t.Fatal("tool result (pong) missing from messages")
	}
}

// 验收 2：事件序列完整还原执行轨迹。
func TestEventTraceRestoresExecution(t *testing.T) {
	scope := kernel.New()
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: []byte(`{}`)}),
		llm.Resp("finished"),
	)
	tools := &fakeTools{fn: func(call llm.ToolCall) (string, error) { return "ok", nil }}
	a := newTestAgent(t, model, tools, scope)
	tr := attachTrace(t, scope)

	if _, err := a.Run(context.Background(), nil, llm.UserText("go")); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"turn_start",
		"step_start(1)",
		"after_model()",
		"before_tool(echo)",
		`after_tool(echo="ok" err=false rej=false)`,
		"step_start(2)",
		"after_model(finished)",
		"turn_end(completed,steps=2,in=0,out=0)",
	}, " | ")
	if got := tr.joined(); got != want {
		t.Fatalf("trace mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// 验收 3：before_tool_call 短路拒绝——工具未执行、模型收到 IsError 结果、循环继续。
func TestBeforeToolCallRejectsExecution(t *testing.T) {
	scope := kernel.New()
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c9", Name: "danger", Arguments: []byte(`{}`)}),
		llm.Resp("understood"),
	)
	tools := &fakeTools{fn: func(call llm.ToolCall) (string, error) {
		return "", errors.New("should never run")
	}}
	a := newTestAgent(t, model, tools, scope)

	unsub, err := kernel.OnWaterfall(scope, EventBeforeToolCall,
		func(p *BeforeToolCall, next func(*BeforeToolCall) *BeforeToolCall) *BeforeToolCall {
			p.Rejected = true
			p.RejectReason = "needs human approval"
			return p // 不调 next => 短路
		})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	res, err := a.Run(context.Background(), nil, llm.UserText("do it"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tools.callCount(); got != 0 {
		t.Fatalf("tool executed %d times despite rejection", got)
	}
	var found bool
	for _, m := range res.Messages {
		for _, part := range m.Parts {
			if p := part.ToolResultValue; p != nil && p.IsError {
				found = true
				txt := ""
				for _, c := range p.Content {
					txt += c.Text
				}
				if !strings.Contains(txt, "needs human approval") {
					t.Fatalf("rejection reason not propagated: %q", txt)
				}
			}
		}
	}
	if !found {
		t.Fatal("no IsError tool result found")
	}
	if res.Final == nil || res.Final.Text() != "understood" {
		t.Fatalf("loop did not continue after rejection: %v", res.Final)
	}
}

// 验收 4：MaxSteps 安全阀——不是错误，如实返回中间态。
func TestMaxStepsSafetyValve(t *testing.T) {
	scope := kernel.New()
	// 脚本耗尽后重复最后一个条目：模型永远要求调用工具。
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c", Name: "echo", Arguments: []byte(`{}`)}),
	)
	tools := &fakeTools{fn: func(call llm.ToolCall) (string, error) { return "tick", nil }}
	a, err := NewAgent(model, WithToolSet(tools), WithEventScope(scope), WithMaxSteps(2))
	if err != nil {
		t.Fatal(err)
	}

	res, rerr := a.Run(context.Background(), nil, llm.UserText("loop forever"))
	if rerr != nil {
		t.Fatalf("max steps should not be an error, got %v", rerr)
	}
	if res.StoppedBy != StopMaxSteps || res.Steps != 2 {
		t.Fatalf("stoppedBy=%s steps=%d", res.StoppedBy, res.Steps)
	}
	if got := tools.callCount(); got != 2 {
		t.Fatalf("tool calls = %d, want 2", got)
	}
}

// MaxSteps 默认不限制；负值归一为不限制。
func TestMaxStepsUnlimitedByDefault(t *testing.T) {
	scope := kernel.New()
	a, err := NewAgent(llm.NewScripted(llm.Resp("x")), WithMaxSteps(-5), WithEventScope(scope))
	if err != nil {
		t.Fatal(err)
	}
	if a.maxSteps != 0 {
		t.Fatalf("negative max steps must normalize to unlimited(0), got %d", a.maxSteps)
	}
	b, err := NewAgent(llm.NewScripted(llm.Resp("y")))
	if err != nil {
		t.Fatal(err)
	}
	if b.maxSteps != 0 {
		t.Fatalf("default must be unlimited(0), got %d", b.maxSteps)
	}
	res, rerr := b.Run(context.Background(), nil, llm.UserText("q"))
	if rerr != nil || res.StoppedBy != StopCompleted || res.Steps != 1 {
		t.Fatalf("unlimited default broken: %v %s %d", rerr, res.StoppedBy, res.Steps)
	}
}

// 验收 6：工具 panic 恢复为失败结果，回合继续。
func TestToolPanicRecovered(t *testing.T) {
	scope := kernel.New()
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c", Name: "boom", Arguments: []byte(`{}`)}),
		llm.Resp("recovered"),
	)
	tools := &fakeTools{fn: func(call llm.ToolCall) (string, error) { panic("exploded") }}
	a := newTestAgent(t, model, tools, scope)

	res, err := a.Run(context.Background(), nil, llm.UserText("trigger"))
	if err != nil {
		t.Fatalf("tool panic must not fail the turn: %v", err)
	}
	if res.Final == nil || res.Final.Text() != "recovered" {
		t.Fatalf("final = %v", res.Final)
	}
	var sawErrResult bool
	for _, m := range res.Messages {
		for _, part := range m.Parts {
			if p := part.ToolResultValue; p != nil && p.IsError {
				sawErrResult = true
			}
		}
	}
	if !sawErrResult {
		t.Fatal("panic was not converted to an IsError result")
	}
}

// ctx 取消以错误返回且终止原因明确。
func TestContextCancelFailsRun(t *testing.T) {
	scope := kernel.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := newTestAgent(t, llm.NewScripted(llm.Resp("x")), nil, scope)

	res, err := a.Run(ctx, nil, llm.UserText("q"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.StoppedBy != StopCanceled {
		t.Fatalf("stoppedBy = %s", res.StoppedBy)
	}
}

// onDelta 收到文本增量。
func TestOnDeltaReceivesText(t *testing.T) {
	scope := kernel.New()
	a := newTestAgent(t, llm.NewScripted(llm.Resp("hello")), nil, scope)

	var got strings.Builder
	res, err := a.RunStream(context.Background(), func(text string) { got.WriteString(text) }, nil, llm.UserText("q"))
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "hello" {
		t.Fatalf("delta = %q", got.String())
	}
	if res.Final == nil || res.Final.Text() != "hello" {
		t.Fatalf("final = %v", res.Final)
	}
}

// 并发 Run 共享同一 Agent 实例（无状态验证）。
func TestConcurrentRunsShareAgent(t *testing.T) {
	scope := kernel.New()
	model := llm.NewScripted(llm.Resp("ok"))
	tools := &fakeTools{fn: func(call llm.ToolCall) (string, error) { return "", nil }}
	a := newTestAgent(t, model, tools, scope)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = a.Run(context.Background(), nil, llm.UserText(fmt.Sprintf("q%d", i)))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

// 未知名工具：错误回传模型，回合继续（走 MemToolSet 的真实校验路径）。
func TestUnknownToolName(t *testing.T) {
	scope := kernel.New()
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c", Name: "nonexistent", Arguments: []byte(`{}`)}),
		llm.Resp("got it"),
	)
	ts := NewMemToolSet()
	if err := ts.Register(llm.ToolDef{Name: "known"}, func(_ context.Context, _ json.RawMessage) (string, error) {
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t, model, ts, scope)

	res, err := a.Run(context.Background(), nil, llm.UserText("q"))
	if err != nil {
		t.Fatal(err)
	}
	var txt string
	for _, m := range res.Messages {
		for _, part := range m.Parts {
			if p := part.ToolResultValue; p != nil && p.IsError {
				for _, c := range p.Content {
					txt += c.Text
				}
			}
		}
	}
	if !strings.Contains(txt, "unknown tool") {
		t.Fatalf("unknown-tool error not surfaced to model: %q", txt)
	}
	if res.Final == nil || res.Final.Text() != "got it" {
		t.Fatalf("final = %v", res.Final)
	}
}

// MemToolSet 基本行为：登记 / 重名拒绝 / 执行 / 未知名。
func TestMemToolSetBasics(t *testing.T) {
	ts := NewMemToolSet()
	err := ts.Register(llm.ToolDef{Name: "a"}, func(_ context.Context, _ json.RawMessage) (string, error) {
		return "A", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = ts.Register(llm.ToolDef{Name: "a"}, func(_ context.Context, _ json.RawMessage) (string, error) {
		return "dup", nil
	})
	if err == nil {
		t.Fatal("expected duplicate registration error")
	}
	if err := ts.Register(llm.ToolDef{Name: ""}, nil); err == nil {
		t.Fatal("expected empty-name error")
	}

	out, err := ts.Execute(context.Background(), llm.ToolCall{Name: "a"})
	if err != nil || out != "A" {
		t.Fatalf("execute: %v %q", err, out)
	}
	if _, err := ts.Execute(context.Background(), llm.ToolCall{Name: "missing"}); err == nil {
		t.Fatal("expected unknown tool error")
	}

	defs := ts.Definitions()
	if len(defs) != 1 || defs[0].Name != "a" {
		t.Fatalf("defs = %+v", defs)
	}

	// 空参 Agent 必须报错。
	if _, err := NewAgent(nil); err == nil {
		t.Fatal("expected nil-model error")
	}
}
