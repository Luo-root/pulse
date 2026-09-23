package host

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/memory"
	"github.com/Luo-root/pulse/memory/assemble"
	"github.com/Luo-root/pulse/memory/session"
	"github.com/Luo-root/pulse/observability"
	"github.com/Luo-root/pulse/toolset"
)

func scriptedProvider(model llm.ChatModel) Provider {
	return func(c *kernel.Context, reg *llm.Registry) error {
		_, err := reg.RegisterProvider(c, "stub", func(cfg llm.Config) (llm.ChatModel, error) {
			return model, nil
		})
		return err
	}
}

func newTestHost(t *testing.T, model llm.ChatModel, opt func(*Options)) *Host {
	t.Helper()
	k := kernel.New()
	t.Cleanup(k.Dispose) // kernel 归调用方所有：生命周期随测试清理
	o := Options{
		Kernel:    k,
		Providers: []Provider{scriptedProvider(model)},
		Models:    []ModelDecl{{Name: "stub", Config: llm.Config{Provider: "stub", Model: "test-model"}}},
	}
	if opt != nil {
		opt(&o)
	}
	h, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestHostStatelessRound：无会话的便捷实例化（DefaultAgent）。
func TestHostStatelessRound(t *testing.T) {
	ctx := context.Background()
	h := newTestHost(t, llm.NewScripted(llm.Resp("hi there")), nil)
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "t1", Model: "stub", System: "be brief"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Session() != nil {
		t.Fatal("session-less host must produce session-less agent")
	}
	res, err := a.Run(ctx, llm.User(llm.Text("hello")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final == nil || res.Final.Parts[0].Text != "hi there" {
		t.Fatalf("final = %+v", res.Final)
	}
	// 显式 history 通道可用（无会话时生效）。
	hist := []*llm.Message{llm.User(llm.Text("older")), llm.Assistant(llm.Text("older reply"))}
	if _, err := a.RunHistory(ctx, hist, llm.User(llm.Text("again"))); err != nil {
		t.Fatal(err)
	}
}

// TestHostInputRoleValidation：回合输入只接受 user 消息——assistant /
// tool 注入显式拒绝，不再静默归档。
func TestHostInputRoleValidation(t *testing.T) {
	ctx := context.Background()
	h := newTestHost(t, llm.NewScripted(llm.Resp("x")), nil)
	a, _ := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "t", Model: "stub"})
	if _, err := a.Run(ctx, llm.Assistant(llm.Text("bogus"))); err == nil || !strings.Contains(err.Error(), "user messages") {
		t.Fatalf("assistant input err = %v", err)
	}
	if _, err := a.Run(ctx, nil); err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("nil input err = %v", err)
	}
}

// TestHostNewAgentFullyInjected：最泛化构造——stub 模型不经 Registry、
// 工具集与会话全注入。
func TestHostNewAgentFullyInjected(t *testing.T) {
	ctx := context.Background()
	model := llm.NewScripted(llm.Resp("injected"))
	tools := loop.NewMemToolSet()
	_ = tools.Register(llm.ToolDef{Name: "noop", Description: "no-op", Parameters: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, args json.RawMessage) (string, error) { return "", nil })
	memStack := memory.NewMemorySessionStack()
	sess, err := memStack.Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	// 宿主不声明任何模型/工具——NewAgent 全注入照样可用。
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models, o.Tools = nil, nil, nil
	})
	a, err := h.NewAgent(AgentOptions{
		Name:      "injected",
		Model:     model,
		ModelName: "stub-model",
		ToolSet:   tools,
		Session:   sess,
		System:    "injected system",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(ctx, llm.User(llm.Text("go")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final.Parts[0].Text != "injected" {
		t.Fatalf("final = %+v", res.Final)
	}
	// request.header 记录注入的 modelName 与工具声明。
	envs, err := sess.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range envs {
		if env.Type == session.EventRequestHeader {
			var p session.RequestHeaderPayload
			if err := json.Unmarshal(env.Data, &p); err != nil {
				t.Fatal(err)
			}
			if p.Model != "stub-model" || len(p.ToolDefs) != 1 || p.ToolDefs[0].Name != "noop" {
				t.Fatalf("header = %+v", p)
			}
		}
	}
}

// TestHostSessionWiring：三向接线——工具回合落盘后，第二轮 Surface 含
// 第一轮完整历史（user → assistant(toolcall) → tool.result），request.header
// 审计在位，turn/step 生命周期事件闭合。
func TestHostSessionWiring(t *testing.T) {
	ctx := context.Background()
	calls := 0
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"ping"}`)}),
		llm.Resp("done"),
	)

	echo := func(ctx context.Context, args json.RawMessage) (string, error) {
		calls++
		return "pong:ping", nil
	}
	h := newTestHost(t, model, func(o *Options) {
		o.Session, _ = memory.NewJSONLSessionStack(t.TempDir()) // JSONL 会话栈
		o.Tools = []ToolSource{func(c *kernel.Context, reg *toolset.Registry) error {
			_, err := reg.Register(c, toolset.Registration{
				Def: llm.ToolDef{
					Name:        "echo",
					Description: "echoes text back",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
				},
				Fn:     echo,
				Source: "test.echo",
				Risk:   toolset.RiskReadWrite,
			})
			return err
		}}
	})
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "wired", Model: "stub", System: "use tools"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Windows TempDir 清理依赖句柄释放：JSONL 会话经类型断言 Close。
		if c, ok := a.Session().(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	if a.Session() == nil {
		t.Fatal("session host must produce session agent")
	}

	// 第一轮：模型请求工具 → loop 执行 → 模型收结果 → 回文本（一个 Run 内完成）。
	res, err := a.Run(ctx, llm.User(llm.Text("please ping")))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("tool calls = %d, want 1", calls)
	}
	if res.Final.Parts[0].Text != "done" {
		t.Fatalf("final = %+v", res.Final)
	}

	// 三向接线验收：Surface 已含完整历史（含工具往返）。
	surface, err := a.Session().Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range surface {
		roles = append(roles, string(m.Role))
	}
	got := strings.Join(roles, ",")
	want := "user,assistant,tool,assistant"
	if got != want {
		t.Fatalf("surface roles = %q, want %q", got, want)
	}
	if surface[1].Parts[0].Kind != llm.PartToolCall || surface[1].Parts[0].ToolCallValue.ID != "c1" {
		t.Fatalf("assistant tool-call part = %+v", surface[1].Parts[0])
	}
	if surface[2].Parts[0].Kind != llm.PartToolResult || surface[2].Parts[0].ToolResultValue.ToolCallID != "c1" {
		t.Fatalf("tool result part = %+v", surface[2].Parts[0])
	}

	// 事件级验收：turn/step 生命周期闭合（官方路径产出的日志对冷恢复
	// 无未决；lifecycle 是 log-only，不进 surface）。
	envs, err := a.Session().Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lifecycle []session.EventType
	var header *session.RequestHeaderPayload
	for _, env := range envs {
		switch env.Type {
		case session.EventTurnStarted, session.EventTurnEnded,
			session.EventStepStarted, session.EventStepEnded:
			lifecycle = append(lifecycle, env.Type)
		case session.EventRequestHeader:
			var p session.RequestHeaderPayload
			if err := json.Unmarshal(env.Data, &p); err != nil {
				t.Fatal(err)
			}
			header = &p
		}
	}
	wantLC := "turn.started,step.started,step.ended,step.started,step.ended,turn.ended"
	var lcStr []string
	for _, t := range lifecycle {
		lcStr = append(lcStr, string(t))
	}
	if strings.Join(lcStr, ",") != wantLC {
		t.Fatalf("lifecycle = %v, want %s", lcStr, wantLC)
	}
	if header == nil {
		t.Fatal("request.header must be recorded")
	}
	if header.System == nil || *header.System != "use tools" || header.Model != "stub" || len(header.ToolDefs) != 1 || header.ToolDefs[0].Name != "echo" {
		t.Fatalf("header = %+v", header)
	}

	// 第二轮：上一轮历史经 Surface 注入 loop（脚本模型继续回放，跑通即证
	// 消息序列合法——非法历史会让 loop/provider 层报错）。
	if _, err := a.Run(ctx, llm.User(llm.Text("and again"))); err != nil {
		t.Fatalf("second round with session history: %v", err)
	}
}

// TestHostToolCallLoggedBeforeExecution：assistant 的 tool_call 在工具
// 执行**之前**已在日志里——进程死在工具执行/HITL 等待点，现场可裁决。
func TestHostToolCallLoggedBeforeExecution(t *testing.T) {
	ctx := context.Background()
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("done"),
	)
	var loggedDuringExec bool
	var sess session.Session
	h := newTestHost(t, model, func(o *Options) {
		o.Session = memory.NewMemorySessionStack()
		o.Tools = []ToolSource{func(c *kernel.Context, reg *toolset.Registry) error {
			_, err := reg.Register(c, toolset.Registration{
				Def: llm.ToolDef{Name: "echo", Parameters: json.RawMessage(`{"type":"object"}`)},
				Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
					envs, err := sess.Events(ctx, 0)
					if err != nil {
						return "", err
					}
					for _, env := range envs {
						if env.Type != session.EventMessageAssistant {
							continue
						}
						var p session.MessagePayload
						if err := json.Unmarshal(env.Data, &p); err != nil {
							return "", err
						}
						for _, part := range p.Parts {
							if part.Kind == llm.PartToolCall && part.ToolCallValue != nil && part.ToolCallValue.ID == "c1" {
								loggedDuringExec = true
							}
						}
					}
					return "pong", nil
				},
				Source: "test.echo",
				Risk:   toolset.RiskReadonly,
			})
			return err
		}}
	})
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "prelog", Model: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	sess = a.Session()
	if _, err := a.Run(ctx, llm.User(llm.Text("ping"))); err != nil {
		t.Fatal(err)
	}
	if !loggedDuringExec {
		t.Fatal("tool_call must be logged before tool execution")
	}
}

// flushCountingSession 统计 Flush 调用：HITL 检查点验收用。
type flushCountingSession struct {
	session.Session
	flushes int
}

func (s *flushCountingSession) Flush(ctx context.Context) error {
	s.flushes++
	return s.Session.Flush(ctx)
}

// TestHostHITLCheckpointFlush：assistant 落盘后立即 Flush（HITL 检查点）
// ——每次 after_model 一次，其余事件不刷；进程死在审批等待时裁决现场
// 必须已在磁盘上（JSONL 的 Append 只 write 不 fsync）。
func TestHostHITLCheckpointFlush(t *testing.T) {
	ctx := context.Background()
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("done"),
	)
	h := newTestHost(t, model, func(o *Options) {
		o.Tools = []ToolSource{func(c *kernel.Context, reg *toolset.Registry) error {
			_, err := reg.Register(c, toolset.Registration{
				Def:    llm.ToolDef{Name: "echo", Parameters: json.RawMessage(`{"type":"object"}`)},
				Fn:     func(ctx context.Context, args json.RawMessage) (string, error) { return "pong", nil },
				Source: "test.echo",
				Risk:   toolset.RiskReadonly,
			})
			return err
		}}
	})
	stack := memory.NewMemorySessionStack()
	sess, err := stack.Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	fc := &flushCountingSession{Session: sess}
	a, err := h.NewAgent(AgentOptions{
		Name: "ckpt", ModelName: "stub", Model: model, ToolSet: h.Tools().AsToolSet(), Session: fc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("ping"))); err != nil {
		t.Fatal(err)
	}
	if fc.flushes != 2 {
		t.Fatalf("flushes = %d, want 2 (one per after_model HITL checkpoint)", fc.flushes)
	}
}

// stepFailModel 是错误路径测试模型：按序回放 steps，耗尽后以 err 失败。
type stepFailModel struct {
	steps []*llm.Response
	err   error
	idx   int
}

func (m *stepFailModel) Generate(_ context.Context, _ *llm.GenerateRequest) (*llm.Response, error) {
	if m.idx < len(m.steps) {
		r := *m.steps[m.idx]
		m.idx++
		return &r, nil
	}
	return nil, m.err
}

func (m *stepFailModel) Stream(ctx context.Context, req *llm.GenerateRequest) (<-chan llm.StreamEvent, error) {
	out := make(chan llm.StreamEvent, 2)
	go func() {
		defer close(out)
		if m.idx >= len(m.steps) {
			out <- llm.StreamEvent{Kind: llm.EventError, Err: m.err}
			return
		}
		resp := *m.steps[m.idx]
		m.idx++
		if txt := resp.Message.Text(); txt != "" {
			out <- llm.StreamEvent{Kind: llm.EventTextDelta, Text: txt}
		}
		out <- llm.StreamEvent{Kind: llm.EventDone, Response: &resp}
	}()
	return out, nil
}

// TestHostErrorPathPersists：模型在第二步失败——已发生的产出与输入
// **必须已落盘**，且日志由 turn_end 监听闭合（reason=interrupted）；
// 重开后无未决（默认档零合成）、Surface 可用。副作用已发生就不能当
// 没发生。
func TestHostErrorPathPersists(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	model := &stepFailModel{
		steps: []*llm.Response{
			llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}),
		},
		err: errors.New("boom"),
	}
	h := newTestHost(t, nil, func(o *Options) {
		o.Providers, o.Models = nil, nil
		o.Tools = []ToolSource{func(c *kernel.Context, reg *toolset.Registry) error {
			_, err := reg.Register(c, toolset.Registration{
				Def:    llm.ToolDef{Name: "echo", Parameters: json.RawMessage(`{"type":"object"}`)},
				Fn:     func(ctx context.Context, args json.RawMessage) (string, error) { return "pong", nil },
				Source: "test.echo",
				Risk:   toolset.RiskReadonly,
			})
			return err
		}}
	})
	stack, err := memory.NewJSONLSessionStack(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := stack.Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Windows TempDir 清理依赖句柄释放：测试中途失败也要释放 JSONL 句柄。
		if c, ok := sess.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	id := sess.Header().SessionID
	a, err := h.NewAgent(AgentOptions{
		Name: "failpath", ModelName: "stub", Model: model, ToolSet: h.Tools().AsToolSet(), Session: sess,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, rerr := a.Run(ctx, llm.User(llm.Text("go")))
	if rerr == nil || !strings.Contains(rerr.Error(), "boom") {
		t.Fatalf("err = %v, want boom", rerr)
	}
	// 部分产出如实返回（canceled/error 不丢已发生的事实）。
	if res == nil || res.StoppedBy != loop.StopError || len(res.Messages) == 0 {
		t.Fatalf("partial result = %+v", res)
	}
	if c, ok := sess.(interface{ Close() error }); ok {
		_ = c.Close()
	}

	// 重开（默认档）：日志已闭合 → 零合成；surface 恰好 user/assistant/tool
	// ——任何 unpaired call 或悬空生命周期都会多出合成事件。
	st2, err := memory.NewJSONLSessionStack(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := st2.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := s2.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	surface, err := s2.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range surface {
		roles = append(roles, string(m.Role))
	}
	if strings.Join(roles, ",") != "user,assistant,tool" {
		t.Fatalf("reopened surface roles = %v (len %d), want user,assistant,tool", roles, len(surface))
	}
	// 中断痕迹在位：turn.ended(reason=interrupted)。
	envs, err := s2.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := false
	for _, env := range envs {
		if env.Type != session.EventTurnEnded {
			continue
		}
		var p session.LifecyclePayload
		if err := json.Unmarshal(env.Data, &p); err != nil {
			t.Fatal(err)
		}
		if p.Reason == session.ReasonInterrupted {
			interrupted = true
		}
	}
	if !interrupted {
		t.Fatal("turn.ended(interrupted) must be recorded on the error path")
	}
}

// TestHostDefaultAgentOpenSession：SessionID 非空 = 打开既有会话续跑；
// Surface 历史跨越两次构造仍然完整。
func TestHostDefaultAgentOpenSession(t *testing.T) {
	ctx := context.Background()
	h := newTestHost(t, llm.NewScripted(llm.Resp("first"), llm.Resp("second")), func(o *Options) {
		o.Session, _ = memory.NewJSONLSessionStack(t.TempDir())
	})
	a1, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "r1", Model: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	id := a1.Session().Header().SessionID
	if _, err := a1.Run(ctx, llm.User(llm.Text("hello"))); err != nil {
		t.Fatal(err)
	}
	if c, ok := a1.Session().(interface{ Close() error }); ok {
		_ = c.Close()
	}

	// 同 ID 重开：冷恢复（默认档合成闭环）+ 续跑。
	a2, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "r2", Model: "stub", SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := a2.Session().(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	surface, err := a2.Session().Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) != 2 || surface[0].Parts[0].Text != "hello" || surface[1].Parts[0].Text != "first" {
		t.Fatalf("reopened surface = %+v", surface)
	}
	if _, err := a2.Run(ctx, llm.User(llm.Text("more"))); err != nil {
		t.Fatal(err)
	}

	// 宿主无 SessionStack 时 SessionID 非空报错。
	h2 := newTestHost(t, llm.NewScripted(llm.Resp("x")), nil)
	if _, err := h2.DefaultAgent(ctx, DefaultAgentOptions{Name: "r3", Model: "stub", SessionID: "nope"}); err == nil {
		t.Fatal("session id without session stack must fail")
	}
}

// TestHostToolGateRejects：闸门拒绝 → 工具不执行、模型收到 IsError 结果、
// 日志 tool.result(IsError) 在位。
func TestHostToolGateRejects(t *testing.T) {
	ctx := context.Background()
	calls := 0
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("gave up"),
	)
	h := newTestHost(t, model, func(o *Options) {
		o.Session = memory.NewMemorySessionStack()
		o.Tools = []ToolSource{func(c *kernel.Context, reg *toolset.Registry) error {
			_, err := reg.Register(c, toolset.Registration{
				Def:    llm.ToolDef{Name: "echo", Parameters: json.RawMessage(`{"type":"object"}`)},
				Fn:     func(ctx context.Context, args json.RawMessage) (string, error) { calls++; return "pong", nil },
				Source: "test.echo",
				Risk:   toolset.RiskReadonly,
			})
			return err
		}}
	})
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{
		Name: "gated", Model: "stub",
		ToolGate: func(_ context.Context, call llm.ToolCall) (bool, string) { return false, "not allowed" },
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(ctx, llm.User(llm.Text("ping")))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("tool executed %d times, want 0", calls)
	}
	if res.Final == nil || res.Final.Parts[0].Text != "gave up" {
		t.Fatalf("final = %+v", res.Final)
	}
	surface, err := a.Session().Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var rejected bool
	for _, m := range surface {
		if m.Role == llm.RoleTool && m.Parts[0].Kind == llm.PartToolResult &&
			m.Parts[0].ToolResultValue.ToolCallID == "c1" && m.Parts[0].ToolResultValue.IsError {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("rejected call must surface as IsError tool result")
	}
}

// TestHostScopeHook：请求级 scope 挂点——应用在 hook 里订阅 loop 事件，
// 回合事实可见。
func TestHostScopeHook(t *testing.T) {
	ctx := context.Background()
	turns := 0
	h := newTestHost(t, llm.NewScripted(llm.Resp("ok")), nil)
	a, err := h.NewAgent(AgentOptions{
		Name: "hooked", Model: llm.NewScripted(llm.Resp("ok")),
		ScopeHook: func(scope *kernel.Context) error {
			_, err := kernel.On(scope, loop.EventTurnEnd, func(p *loop.TurnEnd) {
				turns++
			})
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("hi"))); err != nil {
		t.Fatal(err)
	}
	if turns != 1 {
		t.Fatalf("scope hook observed %d turn_end events, want 1", turns)
	}
}

// TestHostObservePerRequest：装配了 Sink 的宿主——每回合的 loop/llm 事实
// 进 Sink，每请求 TraceID 独立、HostID 承接宿主。
func TestHostObservePerRequest(t *testing.T) {
	ctx := context.Background()
	sink := &observability.MemorySink{}
	h := newTestHost(t, llm.NewScripted(llm.Resp("a"), llm.Resp("b")), func(o *Options) {
		o.Observe = ObserveConfig{HostID: "obs-host", Sink: sink}
	})
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "obs", Model: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("one"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("two"))); err != nil {
		t.Fatal(err)
	}
	recs := sink.Snapshot()
	turnFinished := 0
	traces := map[string]bool{}
	for _, r := range recs {
		if r.Event == loop.EventTurnFinished {
			turnFinished++
			traces[r.TraceID] = true
		}
		if r.HostID != "obs-host" {
			t.Fatalf("record host id = %q", r.HostID)
		}
	}
	if turnFinished != 2 {
		t.Fatalf("turn_finished records = %d, want 2", turnFinished)
	}
	if len(traces) != 2 {
		t.Fatalf("trace ids = %v, want 2 distinct (per-request)", traces)
	}
}

// TestSkillToolsSource：SkillTools 注册 list_skills / load_skill 两个只读
// 工具并可用（stub 模型依次调用）。
func TestSkillToolsSource(t *testing.T) {
	ctx := context.Background()
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "s1", Name: "list_skills", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("listed"),
	)
	h := newTestHost(t, model, func(o *Options) {
		o.Session = memory.NewMemorySessionStack()
		o.Tools = []ToolSource{SkillTools(newStubLoader())}
	})
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "skills", Model: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("list skills"))); err != nil {
		t.Fatal(err)
	}
	// 工具声明经 ToolSet 聚合在位（Definitions 含两个 skill 工具）。
	envs, err := a.Session().Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var defs []llm.ToolDef
	for _, env := range envs {
		if env.Type == session.EventRequestHeader {
			var p session.RequestHeaderPayload
			if err := json.Unmarshal(env.Data, &p); err != nil {
				t.Fatal(err)
			}
			defs = p.ToolDefs
		}
	}
	if len(defs) != 2 || defs[0].Name != "list_skills" || defs[1].Name != "load_skill" {
		t.Fatalf("skill tool defs = %+v", defs)
	}
}

// deltaModel 把一次响应拆成多段文本增量发出——ScriptedModel 只发一段
// （llm/mock.go），验「逐段到达」需要多段。
type deltaModel struct {
	deltas []string
}

func (m *deltaModel) Generate(_ context.Context, _ *llm.GenerateRequest) (*llm.Response, error) {
	return llm.Resp(strings.Join(m.deltas, "")), nil
}

func (m *deltaModel) Stream(ctx context.Context, _ *llm.GenerateRequest) (<-chan llm.StreamEvent, error) {
	out := make(chan llm.StreamEvent, len(m.deltas)+1)
	go func() {
		defer close(out)
		for _, d := range m.deltas {
			select {
			case out <- llm.StreamEvent{Kind: llm.EventTextDelta, Text: d}:
			case <-ctx.Done():
				out <- llm.StreamEvent{Kind: llm.EventError, Err: ctx.Err()}
				return
			}
		}
		out <- llm.StreamEvent{Kind: llm.EventDone, Response: llm.Resp(strings.Join(m.deltas, ""))}
	}()
	return out, nil
}

// TestHostAgentStreamsDeltas：#211——loop 的 onDelta 经 AgentOptions 透传：
// 多段增量按序到达、拼接与 Result.Final 一致；且流式不是旁路，会话落盘的
// 三向接线照旧（assistant 进 surface，回合闭合）。
func TestHostAgentStreamsDeltas(t *testing.T) {
	ctx := context.Background()
	model := &deltaModel{deltas: []string{"你", "好", "呀"}}
	sess, err := memory.NewMemorySessionStack().Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil // 全注入：不依赖宿主声明的模型
	})
	var got []string
	a, err := h.NewAgent(AgentOptions{
		Name: "streamer", Model: model, ModelName: "delta-model", Session: sess,
		OnDelta: func(text string) { got = append(got, text) },
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(ctx, llm.User(llm.Text("打个招呼")))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "你" || got[1] != "好" || got[2] != "呀" {
		t.Fatalf("deltas = %q, want 三段按序", got)
	}
	if want := strings.Join(got, ""); res.Final == nil || res.Final.Text() != want {
		t.Fatalf("final = %+v, want %q", res.Final, want)
	}
	surface, err := sess.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) == 0 {
		t.Fatal("surface empty: streaming must not bypass session persistence")
	}
	last := surface[len(surface)-1]
	if last.Role != llm.RoleAssistant || last.Text() != "你好呀" {
		t.Fatalf("surface tail = %+v", last)
	}
}

// TestHostDefaultAgentStreamsDeltas：便捷路径同样带 onDelta——否则
// 「高级旋钮在便捷路径丢失」这个问题会被原样复制一份。
func TestHostDefaultAgentStreamsDeltas(t *testing.T) {
	ctx := context.Background()
	h := newTestHost(t, &deltaModel{deltas: []string{"a", "b"}}, nil)
	var got []string
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{
		Name: "conv", Model: "stub",
		OnDelta: func(text string) { got = append(got, text) },
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(ctx, llm.User(llm.Text("hi")))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "") != "ab" || res.Final.Text() != "ab" {
		t.Fatalf("deltas = %q, final = %q", got, res.Final.Text())
	}
}

// TestHostOnDeltaUnsetRoundOK：不设 OnDelta = 正常回合仍然跑通（不回调、
// 不报错、结果照常返回）。名字不承诺「逐字节与透传前一致」——那条测不到，
// 行为不变由 nil 分支的读码保证。
func TestHostOnDeltaUnsetRoundOK(t *testing.T) {
	ctx := context.Background()
	h := newTestHost(t, llm.NewScripted(llm.Resp("plain")), nil)
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "plain", Model: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(ctx, llm.User(llm.Text("hi")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final.Text() != "plain" {
		t.Fatalf("final = %q", res.Final.Text())
	}
}

// TestHostOnDeltaPanicPropagates：回调 panic 原样上抛——host 只把
// appendFail 转成 error，其余 panic 不吞不标（README「安全默认」）。
func TestHostOnDeltaPanicPropagates(t *testing.T) {
	ctx := context.Background()
	h := newTestHost(t, llm.NewScripted(llm.Resp("hi")), nil)
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{
		Name: "boom", Model: "stub",
		OnDelta: func(string) { panic("delta exploded") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("onDelta panic must propagate out of Run")
		}
		if s, ok := r.(string); !ok || s != "delta exploded" {
			t.Fatalf("panic payload = %#v", r)
		}
	}()
	if _, err := a.Run(ctx, llm.User(llm.Text("hi"))); err != nil {
		t.Fatalf("Run returned error instead of panicking: %v", err)
	}
}

// TestHostMaxStepsStopsTurn：#2——单回合步数上限经 AgentOptions 透传给
// loop（此前 host 造出来的 Agent 无法设上限）。超限不是错误：Result 以
// StoppedBy=max_steps 如实返回，**回合照常落盘闭合、会话照常可续跑**——
// 这正是 godoc 承诺、也最容易被误信的一条，所以在带会话的真实装配上钉。
func TestHostMaxStepsStopsTurn(t *testing.T) {
	ctx := context.Background()
	tools := loop.NewMemToolSet()
	if err := tools.Register(
		llm.ToolDef{Name: "noop", Description: "no-op", Parameters: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	// 脚本恒回工具调用：没有上限会一直转下去。
	model := llm.NewScripted(llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "noop", Arguments: json.RawMessage(`{}`)}))
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	sess, err := memory.NewMemorySessionStack().Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.NewAgent(AgentOptions{
		Name: "bounded", Model: model, ModelName: "stub", ToolSet: tools, Session: sess, MaxSteps: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(ctx, llm.User(llm.Text("go")))
	if err != nil {
		t.Fatal(err)
	}
	if res.StoppedBy != loop.StopMaxSteps || res.Steps != 2 {
		t.Fatalf("stopped_by=%v steps=%d, want max_steps / 2", res.StoppedBy, res.Steps)
	}
	// 落盘闭合：这一轮的消息真的进了 surface（不是空日志）。
	surface, err := sess.Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) == 0 {
		t.Fatal("a max_steps turn must still be persisted (surface is empty)")
	}
	// 会话可续跑：换一个直接收尾的模型走同一会话，下一轮正常完成。
	h2 := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	a2, err := h2.NewAgent(AgentOptions{
		Name: "bounded", Model: llm.NewScripted(llm.Resp("second")), ModelName: "stub",
		ToolSet: tools, Session: sess, MaxSteps: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	res2, err := a2.Run(ctx, llm.User(llm.Text("again")))
	if err != nil {
		t.Fatal(err)
	}
	if res2.StoppedBy != loop.StopCompleted || res2.Final.Text() != "second" {
		t.Fatalf("second turn stopped_by=%v final=%q, want completed / second", res2.StoppedBy, res2.Final.Text())
	}
}

// TestHostDefaultAgentScopeHook：#3——便捷路径也能装 ScopeHook（此前只有
// NewAgent 有）：请求 scope 上的自订阅 / 自挂 waterfall 从此两条路都可达。
func TestHostDefaultAgentScopeHook(t *testing.T) {
	ctx := context.Background()
	h := newTestHost(t, llm.NewScripted(llm.Resp("hooked")), nil)
	var scopes []*kernel.Context
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{
		Name: "hooked", Model: "stub",
		ScopeHook: func(scope *kernel.Context) error {
			scopes = append(scopes, scope)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("go"))); err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 {
		t.Fatalf("scope hook calls = %d, want 1 (per Run)", len(scopes))
	}
	if scopes[0] == h.Kernel() {
		t.Fatal("hook must receive the derived request scope, not the kernel root")
	}
}

// TestHostContextBuilderInjects：#5——ContextBuilder 是长期记忆的官方落点：
// 拿到会话 surface 与本轮 input，返回的组装产物真的进入发给模型的消息序列
// （用 captureModel 字面断言，不只从 Surface 间接推断）。
func TestHostContextBuilderInjects(t *testing.T) {
	ctx := context.Background()
	sess, err := memory.NewMemorySessionStack().Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	// 第一轮：让 surface 里有一条真实历史（user + assistant）。
	seed := &captureModel{inner: llm.NewScripted(llm.Resp("first"))}
	h1 := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	a1, err := h1.NewAgent(AgentOptions{Name: "seed", Model: seed, ModelName: "stub", Session: sess})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a1.Run(ctx, llm.User(llm.Text("older"))); err != nil {
		t.Fatal(err)
	}

	// 第二轮：同一会话 + 组装缝。
	cap2 := &captureModel{inner: llm.NewScripted(llm.Resp("second"))}
	h2 := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	var surfaceLen, inputLen int
	a2, err := h2.NewAgent(AgentOptions{
		Name: "assembling", Model: cap2, ModelName: "stub", Session: sess,
		ContextBuilder: func(_ context.Context, surface, input []*llm.Message) ([]*llm.Message, error) {
			surfaceLen, inputLen = len(surface), len(input)
			out := append([]*llm.Message{}, surface...)
			out = append(out, llm.Assistant(llm.Text("recalled fact"))) // 模拟检索到的记忆
			return out, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a2.Run(ctx, llm.User(llm.Text("now"))); err != nil {
		t.Fatal(err)
	}
	if surfaceLen != 2 || inputLen != 1 {
		t.Fatalf("builder args: surface=%d input=%d, want 2/1", surfaceLen, inputLen)
	}
	req := cap2.last
	if req == nil {
		t.Fatal("model was not called")
	}
	if len(req.Messages) != 4 {
		t.Fatalf("model messages = %d, want 4 (surface 2 + recalled 1 + input 1)", len(req.Messages))
	}
	if got := req.Messages[2].Text(); got != "recalled fact" {
		t.Fatalf("assembled message = %q", got)
	}
	if last := req.Messages[3]; last.Role != llm.RoleUser || last.Text() != "now" {
		t.Fatalf("turn input must stay last: role=%v text=%q", last.Role, last.Text())
	}
}

// TestHostContextBuilderErrorAborts：#5——组装失败中止回合，且发生在任何
// 模型调用之前（不把半成品发给模型）。
func TestHostContextBuilderErrorAborts(t *testing.T) {
	ctx := context.Background()
	cap := &captureModel{inner: llm.NewScripted(llm.Resp("unused"))}
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	a, err := h.NewAgent(AgentOptions{
		Name: "failing", Model: cap, ModelName: "stub",
		ContextBuilder: func(context.Context, []*llm.Message, []*llm.Message) ([]*llm.Message, error) {
			return nil, errors.New("budget exhausted")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("go"))); err == nil || !strings.Contains(err.Error(), "context builder") {
		t.Fatalf("err = %v, want context builder error", err)
	}
	if cap.last != nil {
		t.Fatal("model must not be called when assembly fails")
	}
}

// TestHostScopeHookRewritesToolCall：#6 的官方配方——ScopeHook 在请求 scope
// 上挂 before_tool_call waterfall，可现场改写调用参数（参数净化）；这是
// ToolGate 之外唯一能改写调用的路径，钉住它真的生效。
func TestHostScopeHookRewritesToolCall(t *testing.T) {
	ctx := context.Background()
	var got string
	tools := loop.NewMemToolSet()
	if err := tools.Register(
		llm.ToolDef{Name: "echo", Description: "echo", Parameters: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			got = in.Text
			return "ok", nil
		}); err != nil {
		t.Fatal(err)
	}
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"raw"}`)}),
		llm.Resp("done"),
	)
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	a, err := h.NewAgent(AgentOptions{
		Name: "sanitizer", Model: model, ModelName: "stub", ToolSet: tools,
		ScopeHook: func(scope *kernel.Context) error {
			_, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
				func(p *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
					if p.Call.Name == "echo" {
						p.Call.Arguments = json.RawMessage(`{"text":"sanitized"}`)
					}
					return next(p)
				})
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("go"))); err != nil {
		t.Fatal(err)
	}
	if got != "sanitized" {
		t.Fatalf("tool received %q, want the rewritten %q", got, "sanitized")
	}
}

// TestHostToolGatePreviewRecipe：#4 的官方配方——闸门闭包持 Host.Tools()
// 调 Registry.Preview 取执行前权限卡片（身份 / 主体 / 效果），据卡片拒绝；
// 被拒绝的调用不执行，模型收到带 reason 的 IsError 结果。
func TestHostToolGatePreviewRecipe(t *testing.T) {
	ctx := context.Background()
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	executed := false
	disp, err := h.Tools().Register(h.Kernel(), toolset.Registration{
		Def:    llm.ToolDef{Name: "write_file", Description: "write", Parameters: json.RawMessage(`{"type":"object"}`)},
		Source: "test.local",
		Risk:   toolset.RiskReadWrite,
		Fn: func(context.Context, json.RawMessage) (string, error) {
			executed = true
			return "wrote", nil
		},
		PreviewFn: func(context.Context, json.RawMessage) (toolset.Preview, error) {
			return toolset.Preview{Action: toolset.ActionWrite, Subject: "/etc/hosts", Kind: "file"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disp()

	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "w1", Name: "write_file", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("ack"),
	)
	var card toolset.Preview
	var hadCard bool
	a, err := h.NewAgent(AgentOptions{
		Name: "previewed", Model: model, ModelName: "stub", ToolSet: h.Tools().AsToolSet(),
		ToolGate: func(_ context.Context, call llm.ToolCall) (bool, string) {
			p, ok, err := h.Tools().Preview(ctx, call.Name, call.Arguments)
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			card, hadCard = p, ok
			return false, "needs approval for " + p.Subject
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(ctx, llm.User(llm.Text("write it")))
	if err != nil {
		t.Fatal(err)
	}
	if !hadCard || card.Subject != "/etc/hosts" || card.Action != toolset.ActionWrite {
		t.Fatalf("card = %+v ok=%v", card, hadCard)
	}
	if executed {
		t.Fatal("rejected call must not execute")
	}
	// 模型收到的工具结果是 IsError 且文本带拒绝原因——工具结果的文本在
	// PartToolResult.Content 里，不在 Message.Text()（后者只取文本块）。
	var seen bool
	for _, m := range res.Messages {
		for _, p := range m.Parts {
			if p.Kind != llm.PartToolResult || p.ToolResultValue == nil || !p.ToolResultValue.IsError {
				continue
			}
			for _, c := range p.ToolResultValue.Content {
				if strings.Contains(c.Text, "needs approval for /etc/hosts") {
					seen = true
				}
			}
		}
	}
	if !seen {
		t.Fatal("model must receive the rejection reason as an IsError result")
	}
}

// TestHostToolGateSeesRewrittenCall：闸门的**顺序**契约——取后序，审批的是
// 改写后的最终调用（「批准的 = 执行的」由构造保证），而不是改写前的原始
// 参数；闸门拒绝时，那条被改写过的调用同样不得执行。
//
// 顺序写错正是「批准 A、执行 B」的来源：卡片展示闸门看到的参数，工具跑
// 链尾的参数，两者必须同一份。
func TestHostToolGateSeesRewrittenCall(t *testing.T) {
	ctx := context.Background()
	executed := false
	tools := loop.NewMemToolSet()
	if err := tools.Register(
		llm.ToolDef{Name: "echo", Description: "echo", Parameters: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, json.RawMessage) (string, error) {
			executed = true
			return "ok", nil
		}); err != nil {
		t.Fatal(err)
	}
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"raw"}`)}),
		llm.Resp("done"),
	)
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	var gated string
	a, err := h.NewAgent(AgentOptions{
		Name: "ordered", Model: model, ModelName: "stub", ToolSet: tools,
		ScopeHook: func(scope *kernel.Context) error {
			_, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
				func(p *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
					p.Call.Arguments = json.RawMessage(`{"text":"sanitized"}`)
					return next(p)
				})
			return err
		},
		ToolGate: func(_ context.Context, call llm.ToolCall) (bool, string) {
			gated = string(call.Arguments)
			return false, "needs approval"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("go"))); err != nil {
		t.Fatal(err)
	}
	if gated != `{"text":"sanitized"}` {
		t.Fatalf("gate saw %s, want the rewritten call (post-order)", gated)
	}
	if executed {
		t.Fatal("rejected call must not execute")
	}
}

// TestHostToolGateSkippedWhenInnerRejected：闸门后序的**短路分支**契约——
// 内层钩子已经把这次调用拒掉时，不再打扰人（审批 UI 不该弹卡片），也不该
// 用 host 的兜底文案覆盖内层给的 reason。
//
// 这一支若被改回「无条件问人」，闸门会被调用（gateCalls>0）且工具会执行，
// 两条断言都会红。
func TestHostToolGateSkippedWhenInnerRejected(t *testing.T) {
	ctx := context.Background()
	executed := false
	tools := loop.NewMemToolSet()
	if err := tools.Register(
		llm.ToolDef{Name: "echo", Description: "echo", Parameters: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, json.RawMessage) (string, error) {
			executed = true
			return "ok", nil
		}); err != nil {
		t.Fatal(err)
	}
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("done"),
	)
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	gateCalls := 0
	a, err := h.NewAgent(AgentOptions{
		Name: "inner-reject", Model: model, ModelName: "stub", ToolSet: tools,
		ScopeHook: func(scope *kernel.Context) error {
			_, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
				func(p *loop.BeforeToolCall, _ func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
					p.Rejected = true
					p.RejectReason = "inner policy says no"
					return p // 不委托 next：直接短路（loop 的 waterfall 契约）
				})
			return err
		},
		ToolGate: func(_ context.Context, _ llm.ToolCall) (bool, string) {
			gateCalls++
			return true, ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(ctx, llm.User(llm.Text("go")))
	if err != nil {
		t.Fatal(err)
	}
	if gateCalls != 0 {
		t.Fatalf("an inner rejection must not bother the human, gate called %d time(s)", gateCalls)
	}
	if executed {
		t.Fatal("a rejected call must not execute")
	}
	var result string
	for _, m := range res.Messages {
		for _, p := range m.Parts {
			if p.Kind != llm.PartToolResult || p.ToolResultValue == nil {
				continue
			}
			for _, c := range p.ToolResultValue.Content {
				result = c.Text
			}
		}
	}
	if !strings.Contains(result, "inner policy says no") {
		t.Fatalf("model must receive the inner reason, got %q", result)
	}
	if strings.Contains(result, "rejected by policy") {
		t.Fatalf("host fallback text must not override the inner reason, got %q", result)
	}
}

// TestHostContextBuilderRecipe：README「上下文组装缝」那段官方配方逐字
// 落到可编译、可运行的用例上（没人编译的文档片段正是字段名写错还能躺在
// 文档里的原因），并覆盖「空 input」这一档——`Run(ctx)` 不带输入是合法
// 调用，配方里那句判空就是为它写的。
func TestHostContextBuilderRecipe(t *testing.T) {
	ctx := context.Background()
	items := memory.NewMemoryItemStack(assemble.Budget{StableMemoryTokens: 800, RetrievedTokens: 1200})
	recipe := func(ctx context.Context, surface, input []*llm.Message) ([]*llm.Message, error) {
		in := assemble.AssembleInput{
			Namespace: []string{"user-42"},
			Surface:   surface,
		}
		if len(input) > 0 {
			in.Query = input[len(input)-1].Text()
		}
		out, err := items.Assemble(ctx, in)
		if err != nil {
			return nil, err
		}
		return out.Messages, nil
	}

	// 空 input：直接取 input[len(input)-1] 会 index out of range。
	if _, err := recipe(ctx, nil, nil); err != nil {
		t.Fatalf("empty input must be legal for the recipe: %v", err)
	}

	// 真跑一回合：组装产物进请求，本轮 input 仍是最后一条。
	cap := &captureModel{inner: llm.NewScripted(llm.Resp("ok"))}
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	a, err := h.NewAgent(AgentOptions{
		Name: "recipe", Model: cap, ModelName: "stub", ContextBuilder: recipe,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("hello"))); err != nil {
		t.Fatal(err)
	}
	if cap.last == nil {
		t.Fatal("model was not called")
	}
	if n := len(cap.last.Messages); n != 1 || cap.last.Messages[0].Text() != "hello" {
		t.Fatalf("model request = %d messages, first = %q", n, cap.last.Messages[0].Text())
	}
}

// TestHostOptionsKnobParity：两条构造路径的旋钮必须**同名同型**——往
// AgentOptions 加旋钮却忘了 DefaultAgentOptions，就是 #211 里 #3 的复刻，
// 而且不会有任何测试变红（那 4 个字段当初正是靠手工转写补上的）。这里把
// 「来源类字段」以外的集合机械化比对，让这类遗漏由测试拦住而不是靠人记得。
func TestHostOptionsKnobParity(t *testing.T) {
	// 两端本来就不同的「来源」解析项：模型 / 工具集 / 会话的来源不同。
	onlyAgent := map[string]bool{"Model": true, "ModelName": true, "ToolSet": true, "Session": true}
	onlyDefault := map[string]bool{"Model": true, "SessionID": true}

	knobs := func(v any, exclude map[string]bool) map[string]string {
		tp := reflect.TypeOf(v)
		out := make(map[string]string, tp.NumField())
		for i := range tp.NumField() {
			f := tp.Field(i)
			if exclude[f.Name] {
				continue
			}
			out[f.Name] = f.Type.String()
		}
		return out
	}

	fromAgent := knobs(AgentOptions{}, onlyAgent)
	fromDefault := knobs(DefaultAgentOptions{}, onlyDefault)
	// 反射扫空 = 护栏本身失效（放它过去等于没有护栏）。
	if len(fromAgent) == 0 || len(fromDefault) == 0 {
		t.Fatal("knob scan produced nothing — this guard would be vacuous")
	}
	for _, name := range []string{"Name", "System", "ToolGate", "ScopeHook", "OnDelta", "MaxSteps", "ContextBuilder"} {
		if _, ok := fromAgent[name]; !ok {
			t.Errorf("AgentOptions.%s missing from the scan — exclusion sets drifted", name)
		}
	}
	for name, typ := range fromAgent {
		got, ok := fromDefault[name]
		if !ok {
			t.Errorf("AgentOptions.%s has no same-named knob on DefaultAgentOptions (the convenience path silently loses it)", name)
			continue
		}
		if got != typ {
			t.Errorf("%s type differs across paths: AgentOptions=%s DefaultAgentOptions=%s", name, typ, got)
		}
	}
	for name := range fromDefault {
		if _, ok := fromAgent[name]; !ok {
			t.Errorf("DefaultAgentOptions.%s has no same-named knob on AgentOptions", name)
		}
	}
}

// TestHostDefaultAgentSessionHeaderAgentID：DefaultAgent 建会话时把 agent 名
// 写进 header.AgentID——同一会话目录被多个 agent 共用时可从 header 区分归属
// （Workspace / AgentPreset 没有自动生产者，需要时用 NewAgent + 宿主自建
// 会话显式传 header）。
func TestHostDefaultAgentSessionHeaderAgentID(t *testing.T) {
	ctx := context.Background()
	h := newTestHost(t, llm.NewScripted(llm.Resp("hi")), func(o *Options) {
		o.Session, _ = memory.NewJSONLSessionStack(t.TempDir())
	})
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "writer-A", Model: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := a.Session().(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	if a.Session() == nil {
		t.Fatal("session host must produce session agent")
	}
	if got := a.Session().Header().AgentID; got != "writer-A" {
		t.Fatalf("header.AgentID = %q, want %q", got, "writer-A")
	}
}

// TestHostRegistryServiceKeysOnKernel：#223——host 的模型/工具注册中心走
// 官方插件路径（kernel.Use + kernel.Get）：README 教的
// kernel.Get(c, llm.ServiceKey) / kernel.Get(c, toolset.ServiceKey) 在 host
// 装配下取得到，取到的就是 Host.Models()/Tools()；注册中心的生命周期归内核
// ——Dispose 之后 closed 守卫生效（裸构造时它永不触发）。
func TestHostRegistryServiceKeysOnKernel(t *testing.T) {
	k := kernel.New()
	h, err := New(Options{
		Kernel:    k,
		Providers: []Provider{scriptedProvider(llm.NewScripted(llm.Resp("unused")))},
		Models:    []ModelDecl{{Name: "stub", Config: llm.Config{Provider: "stub", Model: "test-model"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	models, ok := kernel.Get(k, llm.ServiceKey)
	if !ok || models != h.Models() {
		t.Fatalf("kernel.Get(llm.ServiceKey) = %v, ok=%v; want Host.Models()", models, ok)
	}
	tools, ok := kernel.Get(k, toolset.ServiceKey)
	if !ok || tools != h.Tools() {
		t.Fatalf("kernel.Get(toolset.ServiceKey) = %v, ok=%v; want Host.Tools()", tools, ok)
	}
	// 外部插件经同一服务键登记工具（toolset README 的写法）：host 的聚合
	// 视图随之可见——这正是「宿主与应用插件共享服务仓库」的验收点。
	if _, err := tools.Register(k, toolset.Registration{
		Def:    llm.ToolDef{Name: "late", Description: "registered through the kernel service"},
		Fn:     func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
		Source: "test.late",
		Risk:   toolset.RiskReadonly,
	}); err != nil {
		t.Fatal(err)
	}
	if defs := h.Tools().AsToolSet().Definitions(); len(defs) != 1 || defs[0].Name != "late" {
		t.Fatalf("definitions = %+v, want the tool registered via the service key", defs)
	}
	// 内核销毁 = 插件卸载：注册中心关闭。closed 守卫在换一个活 scope 后
	// 仍要拦下登记（证明关闭是注册中心自身的状态，不只是作用域没了）。
	k.Dispose()
	k2 := kernel.New()
	t.Cleanup(k2.Dispose)
	if _, err := tools.Register(k2, toolset.Registration{
		Def:    llm.ToolDef{Name: "after-dispose", Description: "x"},
		Fn:     func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
		Source: "test.after",
		Risk:   toolset.RiskReadonly,
	}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("err = %v, want a closed-registry error after kernel Dispose", err)
	}
}

// TestHostAttachCollectorBusinessWrite：#223——host 装观测桥时把 Collector
// 局部绑到请求 scope：业务插件 / ScopeHook 经 CollectorKey 取到它直写观测
// （D10 / #125 的官方业务入口），写得进宿主 Sink，且与 loop/llm 记录同一条
// TraceID（同一请求一条 trace）。
func TestHostAttachCollectorBusinessWrite(t *testing.T) {
	ctx := context.Background()
	sink := &observability.MemorySink{}
	h := newTestHost(t, llm.NewScripted(llm.Resp("ok")), func(o *Options) {
		o.Observe = ObserveConfig{HostID: "collector-host", Sink: sink}
	})
	var gotCollector bool
	var writeErr error
	a, err := h.NewAgent(AgentOptions{
		Name: "biz", Model: llm.NewScripted(llm.Resp("ok")), ModelName: "stub",
		ScopeHook: func(scope *kernel.Context) error {
			c, ok := kernel.Get(scope, observability.CollectorKey)
			if !ok || c == nil {
				return nil // 断言放外面：hook 里只记事实
			}
			gotCollector = true
			c.WriteAttrs("order.created", "ok", func(attr *observability.Attrs) {
				observability.Set(attr, "order.id", "o-1")
			})
			return writeErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("go"))); err != nil {
		t.Fatal(err)
	}
	if !gotCollector {
		t.Fatal("CollectorKey must be attached to the request scope when a Sink is configured")
	}
	var bizTrace string
	turnTrace := map[string]bool{}
	for _, r := range sink.Snapshot() {
		switch r.Event {
		case "order.created":
			bizTrace = r.TraceID
		case loop.EventTurnFinished:
			turnTrace[r.TraceID] = true
		}
	}
	if bizTrace == "" {
		t.Fatal("business write must land in the host Sink")
	}
	if !turnTrace[bizTrace] {
		t.Fatalf("business write trace %q must be the request trace (loop records: %v)", bizTrace, turnTrace)
	}
}

// TestHostToolCalledBeforeGate：#224——tool.called 在**闸门（人批）之前**落盘：
// 闸门被问到时日志里已经有这条「调用已发生」锚点（崩溃/强杀现场靠它区分
// 「没调用」与「调用了但没结果」），且被拒绝的调用同样有 called——拒绝本身
// 由 tool.result 的 IsError 呈现。
func TestHostToolCalledBeforeGate(t *testing.T) {
	ctx := context.Background()
	stack, err := memory.NewJSONLSessionStack(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := stack.Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := sess.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	tools := loop.NewMemToolSet()
	if err := tools.Register(
		llm.ToolDef{Name: "echo", Description: "echo"},
		func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)}),
		llm.Resp("done"),
	)
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	var calledSeenByGate bool
	a, err := h.NewAgent(AgentOptions{
		Name: "hitl", Model: model, ModelName: "stub", ToolSet: tools, Session: sess,
		ToolGate: func(_ context.Context, _ llm.ToolCall) (bool, string) {
			envs, err := sess.Events(ctx, 0)
			if err != nil {
				return false, "needs approval"
			}
			for _, e := range envs {
				if e.Type == session.EventToolCalled {
					calledSeenByGate = true
				}
			}
			return false, "needs approval"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("go"))); err != nil {
		t.Fatal(err)
	}
	if !calledSeenByGate {
		t.Fatal("tool.called must already be on disk when the gate asks the human")
	}
	envs, err := sess.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var called *session.ToolCalledPayload
	var rejected bool
	for _, e := range envs {
		switch e.Type {
		case session.EventToolCalled:
			var p session.ToolCalledPayload
			if err := json.Unmarshal(e.Data, &p); err != nil {
				t.Fatal(err)
			}
			called = &p
		case session.EventToolResult:
			var p session.ToolResultPayload
			if err := json.Unmarshal(e.Data, &p); err != nil {
				t.Fatal(err)
			}
			if p.ToolCallID == "c1" && p.IsError {
				rejected = true
			}
		}
	}
	if called == nil || called.ToolCallID != "c1" || called.Name != "echo" || string(called.Arguments) != `{"text":"hi"}` {
		t.Fatalf("tool.called = %+v", called)
	}
	if !rejected {
		t.Fatal("a rejected call must still show up as an IsError tool.result")
	}
}

// TestHostRequestUsageAndRoute：#224——装配层补齐两个「Ignorable 但必须发」
// 的审计事件：request.usage（全回合累计 token，§13.2 缓存命中率归因的唯一
// 数据源）与 request.route（本回合实际服务的模型：adapter 回填则记它，未回填
// 退声明名）。两条都排在闭合事件之前。
func TestHostRequestUsageAndRoute(t *testing.T) {
	ctx := context.Background()
	served := llm.Resp("done")
	served.Model = "stub-2026-01-01" // adapter 回填的实际路由
	served.Usage = llm.TokenUsage{InputTokens: 11, OutputTokens: 5, CachedInputTokens: 3}
	nodecl := llm.Resp("again") // 不填 Model：退回声明名
	h := newTestHost(t, llm.NewScripted(served, nodecl), func(o *Options) {
		o.Session, _ = memory.NewJSONLSessionStack(t.TempDir())
	})
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "audit", Model: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := a.Session().(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	if _, err := a.Run(ctx, llm.User(llm.Text("one"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("two"))); err != nil {
		t.Fatal(err)
	}
	envs, err := a.Session().Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	var usage []session.RequestUsagePayload
	var routeIdx, usageIdx []int
	ended := -1
	for i, e := range envs {
		switch e.Type {
		case session.EventRequestRoute:
			var p session.RequestRoutePayload
			if err := json.Unmarshal(e.Data, &p); err != nil {
				t.Fatal(err)
			}
			routes = append(routes, p.Model)
			routeIdx = append(routeIdx, i)
		case session.EventRequestUsage:
			var p session.RequestUsagePayload
			if err := json.Unmarshal(e.Data, &p); err != nil {
				t.Fatal(err)
			}
			usage = append(usage, p)
			usageIdx = append(usageIdx, i)
		case session.EventTurnEnded:
			if ended < 0 {
				ended = i
			}
		}
	}
	if len(routes) != 2 || routes[0] != "stub-2026-01-01" || routes[1] != "stub" {
		t.Fatalf("routes = %v, want [stub-2026-01-01 stub] (served model, then declared fallback)", routes)
	}
	if len(usage) != 2 {
		t.Fatalf("usage events = %d, want one per turn", len(usage))
	}
	if usage[0].Model != "stub-2026-01-01" || usage[0].InputTokens != 11 ||
		usage[0].OutputTokens != 5 || usage[0].CachedInputTokens != 3 {
		t.Fatalf("usage[0] = %+v, want the accumulated turn usage with the served model", usage[0])
	}
	if usage[1].InputTokens != 0 || usage[1].Model != "stub" {
		t.Fatalf("usage[1] = %+v, want zero tokens under the declared model", usage[1])
	}
	if ended < 0 {
		t.Fatal("turn.ended must be recorded")
	}
	if len(routeIdx) != 2 || len(usageIdx) != 2 ||
		!(routeIdx[0] < usageIdx[0] && usageIdx[0] < ended) {
		t.Fatalf("first-turn order: route@%v usage@%v turn.ended@%d (审计事件必须排在闭合事件之前)",
			routeIdx, usageIdx, ended)
	}
}

// TestHostGateEmptyReasonFallsBackToLoopText：#223——空 reason 的兜底文案只有
// 一个所有者（loop 的 "rejected by policy"）：闸门拒绝但不给理由时，模型收到
// 的就是那一套，host 不再自造第二套默认文本。
func TestHostGateEmptyReasonFallsBackToLoopText(t *testing.T) {
	ctx := context.Background()
	tools := loop.NewMemToolSet()
	if err := tools.Register(
		llm.ToolDef{Name: "echo", Description: "echo"},
		func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("done"),
	)
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	a, err := h.NewAgent(AgentOptions{
		Name: "no-reason", Model: model, ModelName: "stub", ToolSet: tools,
		ToolGate: func(_ context.Context, _ llm.ToolCall) (bool, string) { return false, "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(ctx, llm.User(llm.Text("go")))
	if err != nil {
		t.Fatal(err)
	}
	var result string
	for _, m := range res.Messages {
		for _, p := range m.Parts {
			if p.Kind != llm.PartToolResult || p.ToolResultValue == nil {
				continue
			}
			for _, c := range p.ToolResultValue.Content {
				result = c.Text
			}
		}
	}
	if !strings.Contains(result, "rejected by policy") {
		t.Fatalf("model must receive the loop fallback text, got %q", result)
	}
	if strings.Contains(result, "rejected by tool gate") {
		t.Fatalf("host must not mint a second fallback wording, got %q", result)
	}
}

// TestHostRequestRouteLastWinsAcrossSteps：#224——多步回合里 request.route 取
// **最后一次** adapter 回填的服务模型（回合级审计，与 request.header 对称；
// 网关中途换模型只留最后一次），`request.usage` 仍是全回合累计。
func TestHostRequestRouteLastWinsAcrossSteps(t *testing.T) {
	ctx := context.Background()
	first := llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)})
	first.Model = "gateway-model-a"
	first.Usage = llm.TokenUsage{InputTokens: 3, OutputTokens: 1}
	second := llm.Resp("done")
	second.Model = "gateway-model-b"
	second.Usage = llm.TokenUsage{InputTokens: 5, OutputTokens: 2, CachedInputTokens: 1}
	tools := loop.NewMemToolSet()
	if err := tools.Register(
		llm.ToolDef{Name: "echo", Description: "echo"},
		func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})
	sess, err := memory.NewMemorySessionStack().Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.NewAgent(AgentOptions{
		Name: "routes", Model: llm.NewScripted(first, second), ModelName: "declared", ToolSet: tools, Session: sess,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("go"))); err != nil {
		t.Fatal(err)
	}
	envs, err := sess.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var route *session.RequestRoutePayload
	var usage *session.RequestUsagePayload
	steps := 0
	for _, e := range envs {
		switch e.Type {
		case session.EventStepStarted:
			steps++
		case session.EventRequestRoute:
			var p session.RequestRoutePayload
			if err := json.Unmarshal(e.Data, &p); err != nil {
				t.Fatal(err)
			}
			route = &p
		case session.EventRequestUsage:
			var p session.RequestUsagePayload
			if err := json.Unmarshal(e.Data, &p); err != nil {
				t.Fatal(err)
			}
			usage = &p
		}
	}
	if steps != 2 {
		t.Fatalf("steps = %d, want 2 (multi-step turn so last-wins is observable)", steps)
	}
	if route == nil || route.Model != "gateway-model-b" {
		t.Fatalf("route = %+v, want the last adapter-reported model (gateway-model-b)", route)
	}
	if usage == nil || usage.InputTokens != 8 || usage.OutputTokens != 3 || usage.CachedInputTokens != 1 {
		t.Fatalf("usage = %+v, want the whole-turn accumulation (8/3/1)", usage)
	}
}

// TestTurnRecorderDropsMalformedToolArguments：#224——tool.called 的载荷守卫：
// 参数不是合法 JSON 时留空（omitempty），不让 codec 拒绝、把落盘打成 panic。
//
// 注意这是**纵深防御**：host 的完整路径上，非法参数会先卡在 assistant 消息
// 落盘（`message.assistant` 原样序列化 Part，`json.RawMessage` 拒非法 JSON），
// 轮不到这里——根因（适配器把模型给的参数串原样透传，不保证是合法 JSON）
// 单独开票跟踪。本用例直接跑请求 scope 的 before_tool_call 链，把守卫本身钉住。
func TestTurnRecorderDropsMalformedToolArguments(t *testing.T) {
	ctx := context.Background()
	sess, err := memory.NewMemorySessionStack().Create(ctx, session.SessionHeader{})
	if err != nil {
		t.Fatal(err)
	}
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	r := &turnRecorder{a: &Agent{}, sess: sess, ctx: ctx}
	if err := r.mount(scope); err != nil {
		t.Fatal(err)
	}
	out := kernel.WaterfallLocal(scope, loop.EventBeforeToolCall, &loop.BeforeToolCall{
		Call: llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":`)},
	})
	if out == nil || out.Call.ID != "c1" || out.Call.Name != "echo" {
		t.Fatalf("chain result = %+v, want the payload passed through", out)
	}
	envs, err := sess.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var called *session.ToolCalledPayload
	for _, e := range envs {
		if e.Type != session.EventToolCalled {
			continue
		}
		var p session.ToolCalledPayload
		if err := json.Unmarshal(e.Data, &p); err != nil {
			t.Fatal(err)
		}
		called = &p
	}
	if called == nil || called.ToolCallID != "c1" || called.Name != "echo" {
		t.Fatalf("tool.called = %+v, want it recorded despite malformed arguments", called)
	}
	if len(called.Arguments) != 0 {
		t.Fatalf("arguments = %s, want them dropped (the codec rejects invalid JSON)", called.Arguments)
	}
}

// TestHostToolGateReceivesRunContext：#227——闸门是**带 ctx 的一等形态**：
// 拿到的就是调用方传给 Run 的那个 ctx（值 / 取消 / 超时随宿主），等人工裁决
// 就地等，不必再自挂一条 waterfall 只为拿 ctx。
func TestHostToolGateReceivesRunContext(t *testing.T) {
	type gateKey struct{}
	ctx := context.WithValue(context.Background(), gateKey{}, "round-42")
	tools := loop.NewMemToolSet()
	if err := tools.Register(
		llm.ToolDef{Name: "echo", Description: "echo", Parameters: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	h := newTestHost(t, llm.NewScripted(llm.Resp("unused")), func(o *Options) {
		o.Providers, o.Models = nil, nil
	})

	// ① 值随宿主：闸门拿到的 ctx 就是传给 Run 的那个。
	var seen context.Context
	a, err := h.NewAgent(AgentOptions{
		Name: "ctx", ModelName: "stub", ToolSet: tools,
		Model: llm.NewScripted(
			llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}),
			llm.Resp("done"),
		),
		ToolGate: func(gctx context.Context, call llm.ToolCall) (bool, string) {
			seen = gctx
			return false, "needs approval"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, llm.User(llm.Text("go"))); err != nil {
		t.Fatal(err)
	}
	if seen == nil {
		t.Fatal("the gate never ran")
	}
	if v, _ := seen.Value(gateKey{}).(string); v != "round-42" {
		t.Fatalf("gate ctx value = %q, want the ctx passed to Run", v)
	}

	// ② 超时随宿主：审批人坐在对面就地等裁决，Run 的 ctx 一过期闸门立刻可用
	// （闸门里不 sleep、不自己造 ctx；5s 兜底只为不让变异探针挂死 CI）。
	var toolText string
	a2, err := h.NewAgent(AgentOptions{
		Name: "ctx-deadline", ModelName: "stub", ToolSet: tools,
		Model: llm.NewScripted(
			llm.RespToolCalls(llm.ToolCall{ID: "c2", Name: "echo", Arguments: json.RawMessage(`{}`)}),
			llm.Resp("late"),
		),
		ToolGate: func(gctx context.Context, call llm.ToolCall) (bool, string) {
			select {
			case <-gctx.Done():
				return false, "gate saw " + gctx.Err().Error()
			case <-time.After(5 * time.Second):
				return false, "gate ctx never closed"
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res, err := a2.Run(runCtx, llm.User(llm.Text("go")))
	if err == nil {
		t.Fatal("a round whose ctx expired while the gate waited must surface as an error")
	}
	for _, m := range res.Messages {
		for _, p := range m.Parts {
			if p.ToolResultValue == nil {
				continue
			}
			for _, c := range p.ToolResultValue.Content {
				toolText += c.Text
			}
		}
	}
	if !strings.Contains(toolText, "gate saw context deadline exceeded") {
		t.Fatalf("tool result = %q, want the gate to have observed the host ctx expiring", toolText)
	}
}
