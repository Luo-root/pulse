package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/toolset"
	mcpsrc "github.com/Luo-root/pulse/toolset/mcp"
)

type echoIn struct {
	Msg string `json:"msg"`
}

func startInMemoryPair(t *testing.T, addTools func(*sdkmcp.Server)) (client mcpsrc.Client, cleanup func()) {
	t.Helper()
	ctx := context.Background()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "pulse-test-server", Version: "v0.0.1"}, nil)
	if addTools != nil {
		addTools(server)
	}
	t1, t2 := sdkmcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, t1, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	c, err := mcpsrc.ConnectSDK(ctx, t2)
	if err != nil {
		_ = ss.Close()
		t.Fatalf("client connect: %v", err)
	}
	return c, func() {
		_ = c.Close()
		_ = ss.Close()
	}
}

func TestSDKClientListAndCallViaSource(t *testing.T) {
	client, cleanup := startInMemoryPair(t, func(s *sdkmcp.Server) {
		sdkmcp.AddTool(s, &sdkmcp.Tool{
			Name:        "echo",
			Description: "echo message",
		}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in echoIn) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "echo:" + in.Msg}},
			}, nil, nil
		})
	})
	defer cleanup()

	host := kernel.New()
	defer host.Dispose()
	reg := toolset.NewRegistry()
	src, err := mcpsrc.NewSource(reg, mcpsrc.Config{
		ID:          "mem",
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
	if len(defs) != 1 || defs[0].Name != "echo" {
		t.Fatalf("defs=%v", defs)
	}
	if defs[0].Description != "echo message" {
		t.Fatalf("desc=%q", defs[0].Description)
	}
	if len(defs[0].Parameters) == 0 {
		t.Fatal("expected inferred input schema in Parameters")
	}

	out, err := reg.AsToolSet().Execute(context.Background(), llm.ToolCall{
		Name:      "echo",
		Arguments: json.RawMessage(`{"msg":"hi"}`),
	})
	if err != nil || out != "echo:hi" {
		t.Fatalf("execute: %q %v", out, err)
	}

	src.Detach()
	if len(reg.AsToolSet().Definitions()) != 0 {
		t.Fatal("detach should clear")
	}
}

func TestSDKClientToolBusinessErrorAsError(t *testing.T) {
	client, cleanup := startInMemoryPair(t, func(s *sdkmcp.Server) {
		sdkmcp.AddTool(s, &sdkmcp.Tool{Name: "fail"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
			return nil, nil, errors.New("boom")
		})
	})
	defer cleanup()

	out, err := client.CallTool(context.Background(), "fail", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want err containing boom, got out=%q err=%v", out, err)
	}
	if strings.Contains(err.Error(), "tool error:") {
		t.Fatalf("adapter must not double-prefix; err=%v", err)
	}
	if out != "" {
		t.Fatalf("out should be empty on business error, got %q", out)
	}
}

func TestSDKClientBusinessErrorSetsLoopIsError(t *testing.T) {
	client, cleanup := startInMemoryPair(t, func(s *sdkmcp.Server) {
		sdkmcp.AddTool(s, &sdkmcp.Tool{Name: "fail"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
			return nil, nil, errors.New("boom")
		})
	})
	defer cleanup()

	host := kernel.New()
	defer host.Dispose()
	reg := toolset.NewRegistry()
	src, err := mcpsrc.NewSource(reg, mcpsrc.Config{
		ID: "err", Client: client, DefaultRisk: toolset.RiskReadonly,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Sync(host, context.Background()); err != nil {
		t.Fatal(err)
	}

	var after *loop.AfterToolCall
	if _, err := kernel.On(host, loop.EventAfterToolCall, func(p *loop.AfterToolCall) {
		cp := *p
		after = &cp
	}); err != nil {
		t.Fatal(err)
	}

	model := llm.NewScripted(
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "fail", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("acked"),
	)
	agent, err := loop.NewAgent(model, loop.WithToolSet(reg.AsToolSet()), loop.WithEventScope(host))
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Run(context.Background(), nil, llm.UserText("go"))
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, m := range res.Messages {
		if m == nil {
			continue
		}
		for _, p := range m.Parts {
			if p.Kind == llm.PartToolResult && p.ToolResultValue != nil && p.ToolResultValue.IsError {
				found = true
				text := ""
				for _, c := range p.ToolResultValue.Content {
					if c.Kind == llm.PartText {
						text += c.Text
					}
				}
				if !strings.Contains(text, "boom") {
					t.Fatalf("tool result text=%q", text)
				}
				if strings.Count(text, "tool error:") != 1 {
					t.Fatalf("want single loop prefix, got %q", text)
				}
			}
		}
	}
	if !found {
		t.Fatal("expected ToolResult IsError=true in Messages")
	}
	if after == nil || after.Err == nil {
		t.Fatalf("AfterToolCall.Err should be set, after=%v", after)
	}
}

func TestSDKClientCloseIdempotent(t *testing.T) {
	client, cleanup := startInMemoryPair(t, nil)
	defer cleanup()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := client.ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("err=%v", err)
	}
}

func TestSDKClientCallRespectsCancel(t *testing.T) {
	client, cleanup := startInMemoryPair(t, func(s *sdkmcp.Server) {
		sdkmcp.AddTool(s, &sdkmcp.Tool{Name: "x"}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		})
	})
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.CallTool(ctx, "x", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("want cancel error")
	}
}

func TestSDKClientThroughAgent(t *testing.T) {
	client, cleanup := startInMemoryPair(t, func(s *sdkmcp.Server) {
		sdkmcp.AddTool(s, &sdkmcp.Tool{Name: "lookup"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `{"ok":true}`}},
			}, nil, nil
		})
	})
	defer cleanup()

	host := kernel.New()
	defer host.Dispose()
	reg := toolset.NewRegistry()
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
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)}),
		llm.Resp("done"),
	)
	agent, err := loop.NewAgent(model, loop.WithToolSet(reg.AsToolSet()), loop.WithEventScope(host))
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Run(context.Background(), nil, llm.UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final == nil || res.Final.Text() != "done" {
		t.Fatalf("final=%v", res.Final)
	}
}
