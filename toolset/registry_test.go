package toolset_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/toolset"
)

func echoFn(want string) loop.ToolFunc {
	return func(_ context.Context, _ json.RawMessage) (string, error) {
		return want, nil
	}
}

func mustReg(t *testing.T, r *toolset.Registry, scope *kernel.Context, name, source string, risk toolset.Risk, out string) func() {
	t.Helper()
	dispose, err := r.Register(scope, toolset.Registration{
		Def:    llm.ToolDef{Name: name, Description: name},
		Fn:     echoFn(out),
		Source: source,
		Risk:   risk,
	})
	if err != nil {
		t.Fatalf("Register %q: %v", name, err)
	}
	return dispose
}

func TestRegisterRejectsInvalid(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()

	cases := []struct {
		name string
		reg  toolset.Registration
		sub  string
	}{
		{
			name: "empty name",
			reg:  toolset.Registration{Fn: echoFn("x"), Source: "local", Risk: toolset.RiskReadonly},
			sub:  "name is required",
		},
		{
			name: "nil fn",
			reg: toolset.Registration{
				Def: llm.ToolDef{Name: "a"}, Source: "local", Risk: toolset.RiskReadonly,
			},
			sub: "nil handler",
		},
		{
			name: "empty source",
			reg: toolset.Registration{
				Def: llm.ToolDef{Name: "a"}, Fn: echoFn("x"), Risk: toolset.RiskReadonly,
			},
			sub: "source is required",
		},
		{
			name: "unspecified risk",
			reg: toolset.Registration{
				Def: llm.ToolDef{Name: "a"}, Fn: echoFn("x"), Source: "local", Risk: toolset.RiskUnspecified,
			},
			sub: "risk is required",
		},
		{
			name: "unknown risk",
			reg: toolset.Registration{
				Def: llm.ToolDef{Name: "a"}, Fn: echoFn("x"), Source: "local", Risk: toolset.Risk(99),
			},
			sub: "unknown risk",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Register(host, tc.reg)
			if err == nil || !strings.Contains(err.Error(), tc.sub) {
				t.Fatalf("err=%v want substring %q", err, tc.sub)
			}
			if len(r.AsToolSet().Definitions()) != 0 {
				t.Fatal("invalid register must not leave tools")
			}
		})
	}
}

func TestRegisterConflictAndDispose(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()

	d1 := mustReg(t, r, host, "echo", "local.a", toolset.RiskReadonly, "one")
	_, err := r.Register(host, toolset.Registration{
		Def:    llm.ToolDef{Name: "echo"},
		Fn:     echoFn("two"),
		Source: "local.b",
		Risk:   toolset.RiskReadonly,
	})
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("dup err=%v", err)
	}

	ts := r.AsToolSet()
	out, err := ts.Execute(context.Background(), llm.ToolCall{Name: "echo"})
	if err != nil || out != "one" {
		t.Fatalf("got %q %v want one", out, err)
	}

	d1()
	d1() // 幂等
	if len(ts.Definitions()) != 0 {
		t.Fatal("dispose should remove tool from live view")
	}
	_, err = ts.Execute(context.Background(), llm.ToolCall{Name: "echo"})
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("execute after dispose: %v", err)
	}
}

func TestDisposeSource(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()

	mustReg(t, r, host, "a1", "mcp.fs", toolset.RiskReadonly, "a1")
	mustReg(t, r, host, "a2", "mcp.fs", toolset.RiskReadWrite, "a2")
	mustReg(t, r, host, "b1", "local", toolset.RiskReadonly, "b1")

	r.DisposeSource("mcp.fs")
	r.DisposeSource("mcp.fs") // 幂等
	r.DisposeSource("")       // 空操作
	r.DisposeSource("nope")

	defs := r.AsToolSet().Definitions()
	if len(defs) != 1 || defs[0].Name != "b1" {
		t.Fatalf("defs=%v want only b1", defs)
	}
	if _, _, ok := r.LookupMeta("a1"); ok {
		t.Fatal("a1 should be gone")
	}
	src, risk, ok := r.LookupMeta("b1")
	if !ok || src != "local" || risk != toolset.RiskReadonly {
		t.Fatalf("meta b1: %q %v %v", src, risk, ok)
	}
}

func TestDefinitionsSortedAndExecute(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()
	mustReg(t, r, host, "zeta", "s", toolset.RiskReadonly, "z")
	mustReg(t, r, host, "alpha", "s", toolset.RiskDangerous, "a")

	defs := r.AsToolSet().Definitions()
	if len(defs) != 2 || defs[0].Name != "alpha" || defs[1].Name != "zeta" {
		t.Fatalf("order=%v", defs)
	}
	out, err := r.AsToolSet().Execute(context.Background(), llm.ToolCall{
		Name: "alpha", Arguments: json.RawMessage(`{}`),
	})
	if err != nil || out != "a" {
		t.Fatalf("execute: %q %v", out, err)
	}
}

func TestScopeDisposeReversesRegister(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()

	child, err := host.Derive()
	if err != nil {
		t.Fatal(err)
	}
	mustReg(t, r, child, "tmp", "src", toolset.RiskReadonly, "x")
	if len(r.AsToolSet().Definitions()) != 1 {
		t.Fatal("expected one tool")
	}
	child.Dispose()
	if len(r.AsToolSet().Definitions()) != 0 {
		t.Fatal("child dispose should reverse Register effect")
	}
}

func TestPluginProvidesService(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, toolset.Plugin()); err != nil {
		t.Fatal(err)
	}
	reg, ok := kernel.Get(host, toolset.ServiceKey)
	if !ok || reg == nil {
		t.Fatal("pulse.tools not provided")
	}
	mustReg(t, reg, host, "echo", "local", toolset.RiskReadonly, "ok")
	host.Dispose()
	// Close 清空；再 Register 应失败
	_, err := reg.Register(kernel.New(), toolset.Registration{
		Def: llm.ToolDef{Name: "x"}, Fn: echoFn("x"), Source: "s", Risk: toolset.RiskReadonly,
	})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("after host dispose want closed, got %v", err)
	}
}

func TestAsToolSetSatisfiesLoop(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()
	mustReg(t, r, host, "ping", "local", toolset.RiskReadonly, "pong")

	var ts loop.ToolSet = r.AsToolSet()
	out, err := ts.Execute(context.Background(), llm.ToolCall{Name: "ping"})
	if err != nil || out != "pong" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestDisposeDoesNotClobberReregister(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()

	d1 := mustReg(t, r, host, "echo", "s1", toolset.RiskReadonly, "v1")
	d1()
	mustReg(t, r, host, "echo", "s2", toolset.RiskReadWrite, "v2")
	d1() // 旧 dispose 不得删掉新条目
	out, err := r.AsToolSet().Execute(context.Background(), llm.ToolCall{Name: "echo"})
	if err != nil || out != "v2" {
		t.Fatalf("got %q %v", out, err)
	}
}

func TestExecuteRespectsContextCancel(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()

	_, err := r.Register(host, toolset.Registration{
		Def:    llm.ToolDef{Name: "wait"},
		Source: "local",
		Risk:   toolset.RiskReadonly,
		Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.AsToolSet().Execute(ctx, llm.ToolCall{Name: "wait", Arguments: json.RawMessage(`{}`)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled", err)
	}
}

func TestDisposeSourceThenReregister(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()

	mustReg(t, r, host, "a1", "mcp.fs", toolset.RiskReadonly, "a1-old")
	mustReg(t, r, host, "a2", "mcp.fs", toolset.RiskReadWrite, "a2")
	mustReg(t, r, host, "b1", "local", toolset.RiskReadonly, "b1")

	r.DisposeSource("mcp.fs")
	mustReg(t, r, host, "a1", "mcp.fs", toolset.RiskReadonly, "a1-new")

	out, err := r.AsToolSet().Execute(context.Background(), llm.ToolCall{Name: "a1"})
	if err != nil || out != "a1-new" {
		t.Fatalf("a1 after reconnect: %q %v", out, err)
	}
	src, risk, ok := r.LookupMeta("a1")
	if !ok || src != "mcp.fs" || risk != toolset.RiskReadonly {
		t.Fatalf("LookupMeta a1: %q %v %v", src, risk, ok)
	}
	if _, _, ok := r.LookupMeta("a2"); ok {
		t.Fatal("a2 should stay gone after DisposeSource")
	}
	out, err = r.AsToolSet().Execute(context.Background(), llm.ToolCall{Name: "b1"})
	if err != nil || out != "b1" {
		t.Fatalf("local b1 must remain: %q %v", out, err)
	}
}

func TestAsToolSetConsumedByAgent(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()
	mustReg(t, r, host, "lookup", "local.lookup", toolset.RiskReadonly, `{"ok":true}`)

	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"topic":"pulse"}`)}),
		llm.Resp("done"),
	)
	agent, err := loop.NewAgent(model,
		loop.WithToolSet(r.AsToolSet()),
		loop.WithEventScope(host),
		loop.WithSystemPrompt("test"),
	)
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Run(context.Background(), nil, llm.UserText("call lookup"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final == nil || res.Final.Text() != "done" {
		t.Fatalf("Final=%v", res.Final)
	}
	foundToolResult := false
	for _, m := range res.Messages {
		if m == nil {
			continue
		}
		for _, p := range m.Parts {
			if p.Kind == llm.PartToolResult && p.ToolResultValue != nil && !p.ToolResultValue.IsError {
				foundToolResult = true
			}
		}
	}
	if !foundToolResult {
		t.Fatalf("expected successful tool result in Messages: %+v", res.Messages)
	}
}
