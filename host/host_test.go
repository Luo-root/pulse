package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/memory"
	"github.com/Luo-root/pulse/memory/session"
	"github.com/Luo-root/pulse/observability"
	"github.com/Luo-root/pulse/toolset"
)

func scriptedProvider(model *llm.ScriptedModel) Provider {
	return func(c *kernel.Context, reg *llm.Registry) error {
		_, err := reg.RegisterProvider(c, "stub", func(cfg llm.Config) (llm.ChatModel, error) {
			return model, nil
		})
		return err
	}
}

func newTestHost(t *testing.T, model *llm.ScriptedModel, opt func(*Options)) *Host {
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
		ToolGate: func(call llm.ToolCall) (bool, string) { return false, "not allowed" },
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
