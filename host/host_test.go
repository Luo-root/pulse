package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/memory"
	"github.com/Luo-root/pulse/memory/session"
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
		Models:    map[string]llm.Config{"stub": {Provider: "stub", Model: "test-model"}},
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
	a, err := h.NewAgent(ctx, AgentOptions{
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
// 审计在位。
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

	// request.header 审计（system + tool 声明 + model 声明名在位）。
	envs, err := a.Session().Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var header *session.RequestHeaderPayload
	for _, env := range envs {
		if env.Type == session.EventRequestHeader {
			var p session.RequestHeaderPayload
			if err := json.Unmarshal(env.Data, &p); err != nil {
				t.Fatal(err)
			}
			header = &p
		}
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
