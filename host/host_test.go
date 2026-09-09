package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
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
	o := Options{
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
	t.Cleanup(h.Close)
	return h
}

// TestHostStatelessRound：无会话宿主的纯透传回合。
func TestHostStatelessRound(t *testing.T) {
	ctx := context.Background()
	h := newTestHost(t, llm.NewScripted(llm.Resp("hi there")), nil)
	a, err := h.Agent(ctx, AgentOptions{Name: "t1", Model: "stub", System: "be brief"})
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
		o.Session, _ = memory.NewSessionStack(memory.SessionOptions{}) // 内存会话栈
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
	a, err := h.Agent(ctx, AgentOptions{Name: "wired", Model: "stub", System: "use tools"})
	if err != nil {
		t.Fatal(err)
	}
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

	// request.header 审计（system + tool 声明 + model 三样在位）。
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
