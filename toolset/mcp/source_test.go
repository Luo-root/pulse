package mcp_test

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
	mcpsrc "github.com/Luo-root/pulse/toolset/mcp"
)

func TestSyncRegistersAndDetach(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, toolset.Plugin()); err != nil {
		t.Fatal(err)
	}
	reg, _ := kernel.Get(host, toolset.ServiceKey)

	client := &mockClient{}
	client.setTools(
		mcpsrc.Tool{Name: "read", Description: "r", Parameters: json.RawMessage(`{"type":"object"}`)},
		mcpsrc.Tool{Name: "write", Description: "w"},
	)
	src, err := mcpsrc.NewSource(reg, mcpsrc.Config{
		ID:          "fs",
		Client:      client,
		DefaultRisk: toolset.RiskReadonly,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Sync(host, context.Background()); err != nil {
		t.Fatal(err)
	}
	defs := reg.AsToolSet().Definitions()
	if len(defs) != 2 || defs[0].Name != "read" || defs[1].Name != "write" {
		t.Fatalf("defs=%v", defs)
	}
	if key := src.SourceKey(); key != "mcp.fs" {
		t.Fatalf("SourceKey=%q", key)
	}
	srcMeta, risk, ok := reg.LookupMeta("read")
	if !ok || srcMeta != "mcp.fs" || risk != toolset.RiskReadonly {
		t.Fatalf("meta=%q %v %v", srcMeta, risk, ok)
	}

	src.Detach()
	if len(reg.AsToolSet().Definitions()) != 0 {
		t.Fatal("detach should clear mcp.fs tools")
	}
}

func TestNamePrefixAndConflict(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	reg := toolset.NewRegistry()

	// 先占住最终名 fs_read，逼出「先挂上一条再撞名」的半截路径
	if _, err := reg.Register(host, toolset.Registration{
		Def:    llm.ToolDef{Name: "fs_read"},
		Fn:     func(context.Context, json.RawMessage) (string, error) { return "local", nil },
		Source: "local",
		Risk:   toolset.RiskReadonly,
	}); err != nil {
		t.Fatal(err)
	}

	client := &mockClient{}
	// ok → fs_ok 先成功；read → fs_read 再撞名；回滚后 fs_ok 也必须消失
	client.setTools(
		mcpsrc.Tool{Name: "ok", Description: "ok"},
		mcpsrc.Tool{Name: "read", Description: "r"},
	)
	src, err := mcpsrc.NewSource(reg, mcpsrc.Config{
		ID:          "fs",
		Client:      client,
		NamePrefix:  "fs",
		DefaultRisk: toolset.RiskReadonly,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = src.Sync(host, context.Background())
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("want conflict, got %v", err)
	}
	// 冲突整源回滚：不得留下半截 mcp 登记（含已成功的 fs_ok）；local 仍在
	defs := reg.AsToolSet().Definitions()
	if len(defs) != 1 || defs[0].Name != "fs_read" {
		t.Fatalf("after conflict defs=%v want only local fs_read", defs)
	}
	if _, _, ok := reg.LookupMeta("fs_ok"); ok {
		t.Fatal("fs_ok must be rolled back with DisposeSource")
	}
	srcMeta, _, ok := reg.LookupMeta("fs_read")
	if !ok || srcMeta != "local" {
		t.Fatalf("local fs_read must remain, meta=%q ok=%v", srcMeta, ok)
	}
}

func TestReconnectSameNames(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	reg := toolset.NewRegistry()

	client := &mockClient{}
	client.setTools(mcpsrc.Tool{Name: "a1"}, mcpsrc.Tool{Name: "a2"})
	src, err := mcpsrc.NewSource(reg, mcpsrc.Config{
		ID: "fs", Client: client, DefaultRisk: toolset.RiskReadWrite,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Sync(host, context.Background()); err != nil {
		t.Fatal(err)
	}
	// 本地工具不受影响
	if _, err := reg.Register(host, toolset.Registration{
		Def: llm.ToolDef{Name: "b1"}, Fn: func(context.Context, json.RawMessage) (string, error) { return "b1", nil },
		Source: "local", Risk: toolset.RiskReadonly,
	}); err != nil {
		t.Fatal(err)
	}

	src.Detach()
	client.setTools(mcpsrc.Tool{Name: "a1"}) // 重连后少一个
	if err := src.Sync(host, context.Background()); err != nil {
		t.Fatal(err)
	}
	defs := reg.AsToolSet().Definitions()
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	if !names["a1"] || names["a2"] || !names["b1"] {
		t.Fatalf("names=%v", names)
	}
	out, err := reg.AsToolSet().Execute(context.Background(), llm.ToolCall{
		Name: "a1", Arguments: json.RawMessage(`{"x":1}`),
	})
	if err != nil || !strings.Contains(out, `"tool":"a1"`) {
		t.Fatalf("call=%q err=%v", out, err)
	}
}

func TestCallRespectsCancel(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	reg := toolset.NewRegistry()
	client := &mockClient{}
	client.setTools(mcpsrc.Tool{Name: "slow"})
	src, err := mcpsrc.NewSource(reg, mcpsrc.Config{
		ID: "x", Client: client, DefaultRisk: toolset.RiskReadonly,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Sync(host, context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = reg.AsToolSet().Execute(ctx, llm.ToolCall{Name: "slow", Arguments: json.RawMessage(`{}`)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestPluginLifecycle(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, toolset.Plugin()); err != nil {
		t.Fatal(err)
	}
	reg, _ := kernel.Get(host, toolset.ServiceKey)

	client := &mockClient{}
	client.setTools(mcpsrc.Tool{Name: "ping"})
	plug, err := mcpsrc.Plugin(reg, mcpsrc.Config{
		ID: "demo", Client: client, DefaultRisk: toolset.RiskReadonly,
	})
	if err != nil {
		t.Fatal(err)
	}
	fiber, err := kernel.Use(host, plug)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.AsToolSet().Definitions()) != 1 {
		t.Fatal("expected ping registered")
	}
	fiber.Close()
	if len(reg.AsToolSet().Definitions()) != 0 {
		t.Fatal("unload should detach")
	}
	if !client.closed {
		t.Fatal("client should close on unload")
	}
}

func TestRejectsBadConfig(t *testing.T) {
	reg := toolset.NewRegistry()
	_, err := mcpsrc.NewSource(reg, mcpsrc.Config{Client: &mockClient{}, DefaultRisk: toolset.RiskReadonly})
	if err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Fatalf("%v", err)
	}
	_, err = mcpsrc.NewSource(reg, mcpsrc.Config{ID: "x", DefaultRisk: toolset.RiskReadonly})
	if err == nil || !strings.Contains(err.Error(), "client is required") {
		t.Fatalf("%v", err)
	}
	_, err = mcpsrc.NewSource(reg, mcpsrc.Config{ID: "x", Client: &mockClient{}})
	if err == nil || !strings.Contains(err.Error(), "default risk") {
		t.Fatalf("%v", err)
	}
}

func TestAsToolSetThroughAgent(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	reg := toolset.NewRegistry()
	client := &mockClient{}
	client.setTools(mcpsrc.Tool{Name: "lookup", Description: "q"})
	src, err := mcpsrc.NewSource(reg, mcpsrc.Config{
		ID: "kb", Client: client, DefaultRisk: toolset.RiskReadonly,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Sync(host, context.Background()); err != nil {
		t.Fatal(err)
	}

	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"q":"pulse"}`)}),
		llm.Resp("ok"),
	)
	agent, err := loop.NewAgent(model, loop.WithToolSet(reg.AsToolSet()), loop.WithEventScope(host))
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Run(context.Background(), nil, llm.UserText("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final == nil || res.Final.Text() != "ok" {
		t.Fatalf("final=%v", res.Final)
	}
}
