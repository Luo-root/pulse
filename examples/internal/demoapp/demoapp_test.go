package demoapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/observability"
)

func TestInputMessageMultimodal(t *testing.T) {
	in := Input{
		Text:     "describe this",
		ImageURL: "https://example.com/cat.png",
		ImageType: "image/png",
	}
	msg, err := in.Message()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != llm.RoleUser {
		t.Fatalf("role = %s", msg.Role)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("parts = %d", len(msg.Parts))
	}
	if msg.Parts[0].Kind != llm.PartText || msg.Parts[1].Kind != llm.PartImage {
		t.Fatalf("kinds = %s %s", msg.Parts[0].Kind, msg.Parts[1].Kind)
	}
}

func TestOpenScriptedGenerate(t *testing.T) {
	h, err := Open(Flags{Scripted: true}, llm.Resp("hello from scripted"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	resp, err := h.Model.Generate(context.Background(), llm.NewRequest(llm.UserText("hi")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != "hello from scripted" {
		t.Fatalf("got %q", resp.Message.Text())
	}
}

// host.Close 后 Effect 全部回收：Registry 服务绑定应从仓库消失；
// Sink 记录数不再增长（Dispose 零残留）。
func TestHostCloseReclaimsServices(t *testing.T) {
	h, err := Open(Flags{Scripted: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := GetRegistry(h); !ok {
		t.Fatal("registry should be provided before close")
	}
	nBefore := h.Sink.Len()
	h.Close()
	if _, ok := GetRegistry(h); ok {
		t.Fatal("registry service binding must be gone after Close")
	}
	if h.Sink.Len() != nBefore {
		t.Fatalf("records leaked after close: %d -> %d", nBefore, h.Sink.Len())
	}
}

// D3 两层标识：同宿主 hostID 稳定；每次请求 trace_id 独立且不同，
// 并携带宿主前缀便于日志聚合分组。
func TestHostAndTraceIDSeparation(t *testing.T) {
	h, err := Open(Flags{Scripted: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	t1 := h.NewTraceID()
	t2 := h.NewTraceID()
	hostID := h.HostID()
	if t1 == t2 {
		t.Fatalf("trace ids must differ per request: %q", t1)
	}
	if !strings.HasPrefix(t1, hostID) {
		t.Fatalf("trace id %q should carry host prefix %q", t1, hostID)
	}
	// 同宿主跨 NewTraceID 调用 hostID 必须稳定。
	if h.HostID() != hostID {
		t.Fatalf("host id drifted: %q -> %q", hostID, h.HostID())
	}
	h2, err := Open(Flags{Scripted: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	if h2.HostID() == hostID {
		t.Fatalf("two Open() hosts must have different host ids: %q", hostID)
	}
}

// 桥运行期事实写入统一 Sink：generate_finished 同时携带 HostID 与桥的
// 请求级 TraceID（桥创建时生成；host 前缀保证日志聚合可分组）。
func TestBridgeWritesRuntimeRecords(t *testing.T) {
	h, err := Open(Flags{Scripted: true}, llm.Resp("bridge ok"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	reqScope, err := h.Ctx.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer reqScope.Dispose()
	bridge, err := h.NewBridge(reqScope)
	if err != nil {
		t.Fatal(err)
	}
	if bridge.TraceID == "" {
		t.Fatal("bridge trace id should be set at creation")
	}
	before := len(h.Sink.Snapshot())
	_, genErr := h.Model.Generate(llm.WithEventScope(context.Background(), reqScope), llm.NewRequest(llm.UserText("hi")))
	time.Sleep(50 * time.Millisecond)
	if genErr != nil {
		t.Fatal(genErr)
	}

	found := false
	for _, rec := range h.Sink.Snapshot()[before:] {
		if rec.Event != "llm.generate_finished" {
			continue
		}
		found = true
		if rec.Source != observability.SourceBridge {
			t.Fatalf("source = %s", rec.Source)
		}
		if rec.HostID != h.HostID() {
			t.Fatalf("host mismatch: %q vs %q", rec.HostID, h.HostID())
		}
		if rec.TraceID != bridge.TraceID {
			t.Fatalf("trace mismatch: %q vs bridge %q", rec.TraceID, bridge.TraceID)
		}
		if rec.Status == "" {
			t.Fatal("status should be set")
		}
		if !strings.HasPrefix(rec.TraceID, rec.HostID) {
			t.Fatalf("trace %q should carry host prefix %q", rec.TraceID, rec.HostID)
		}
	}
	if !found {
		t.Fatalf("no generate_finished record among %d new records", len(h.Sink.Snapshot())-before)
	}
}

// 请求级 Bridge 隔离：scopeA 上的 Agent 工具事件只能写 bridgeA 的 trace；
// 即使 scopeB/bridgeB 同时存活，也绝不能复制为 B 的记录。
func TestRequestBridgesDoNotCrossTalk(t *testing.T) {
	h, err := Open(Flags{Scripted: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	scopeA, err := h.Ctx.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer scopeA.Dispose()
	scopeB, err := h.Ctx.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer scopeB.Dispose()

	bridgeA, err := h.NewBridge(scopeA)
	if err != nil {
		t.Fatal(err)
	}
	bridgeB, err := h.NewBridge(scopeB)
	if err != nil {
		t.Fatal(err)
	}
	if bridgeA.TraceID == bridgeB.TraceID {
		t.Fatal("two request bridges must have different trace IDs")
	}

	tools := loop.NewMemToolSet()
	if err := tools.Register(llm.ToolDef{
		Name:       "lookup",
		Parameters: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	// 经 Registry 打开，确保 after_response Local 事件真正发出。
	if _, err := h.Registry.RegisterProvider(h.Ctx, "iso-mock", func(llm.Config) (llm.ChatModel, error) {
		return llm.NewScripted(
			llm.RespToolCalls(llm.ToolCall{ID: "tool-a", Name: "lookup", Arguments: json.RawMessage(`{}`)}),
			llm.Resp("done"),
		), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Registry.Declare("iso-main", llm.Config{Provider: "iso-mock", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	model, err := h.Registry.Open("iso-main")
	if err != nil {
		t.Fatal(err)
	}
	agentA, err := loop.NewAgent(model, loop.WithToolSet(tools), loop.WithEventScope(scopeA))
	if err != nil {
		t.Fatal(err)
	}

	before := len(h.Sink.Snapshot())
	if _, err := agentA.Run(context.Background(), nil, llm.UserText("call lookup")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	var aTools, bTools, aLLM, bLLM int
	for _, rec := range h.Sink.Snapshot()[before:] {
		switch rec.Event {
		case "loop.tool_finished":
			switch rec.TraceID {
			case bridgeA.TraceID:
				aTools++
			case bridgeB.TraceID:
				bTools++
			default:
				t.Fatalf("tool event with foreign trace %q", rec.TraceID)
			}
		case "llm.generate_finished":
			switch rec.TraceID {
			case bridgeA.TraceID:
				aLLM++
			case bridgeB.TraceID:
				bLLM++
			}
		}
	}
	if aTools != 1 || bTools != 0 {
		t.Fatalf("tool cross-talk: A=%d B=%d", aTools, bTools)
	}
	if aLLM == 0 || bLLM != 0 {
		t.Fatalf("llm cross-talk: A=%d B=%d", aLLM, bLLM)
	}
}


func TestAnthropicMaxTokensDefaultInstalled(t *testing.T) {
	h, err := Open(Flags{Scripted: true}, llm.Resp("ok"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	reqScope, err := h.Ctx.Derive()
	if err != nil {
		t.Fatal(err)
	}
	defer reqScope.Dispose()
	if _, err := h.NewBridge(reqScope); err != nil {
		t.Fatal(err)
	}

	var seen *int
	_, err = kernel.OnWaterfall(reqScope, llm.EventBeforeGenerate,
		func(req *llm.GenerateRequest, next func(*llm.GenerateRequest) *llm.GenerateRequest) *llm.GenerateRequest {
			out := next(req) // 内层含 NewBridge 安装的默认 MaxTokens 注入
			seen = out.MaxTokens
			return out
		})
	if err != nil {
		t.Fatal(err)
	}
	_, genErr := h.Model.Generate(llm.WithEventScope(context.Background(), reqScope), llm.NewRequest(llm.UserText("hi")))
	if genErr != nil {
		t.Fatal(genErr)
	}
	if seen == nil || *seen != defaultAnthropicMaxTokens {
		t.Fatalf("MaxTokens after default inject = %v, want %d", seen, defaultAnthropicMaxTokens)
	}
}

// 01-chat 路径：不 NewBridge、不 WithEventScope，observed 回退 Registry.EventScope()。
// 必须由 Open 时挂在该 scope 上的默认注入补 MaxTokens，否则 anthropic 会 ErrBadRequest。
func TestAnthropicMaxTokensDefaultOnRegistryFallback(t *testing.T) {
	h, err := Open(Flags{Scripted: true}, llm.Resp("ok"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	scope := h.Registry.EventScope()
	if scope == nil {
		t.Fatal("registry event scope nil")
	}

	var seen *int
	_, err = kernel.OnWaterfall(scope, llm.EventBeforeGenerate,
		func(req *llm.GenerateRequest, next func(*llm.GenerateRequest) *llm.GenerateRequest) *llm.GenerateRequest {
			out := next(req) // 内层含 Open 安装的 Registry-scope 默认注入
			seen = out.MaxTokens
			return out
		})
	if err != nil {
		t.Fatal(err)
	}
	// 刻意不带 EventScope：走 Registry 回退 Local（01-chat）。
	_, genErr := h.Model.Generate(context.Background(), llm.NewRequest(llm.UserText("hi")))
	if genErr != nil {
		t.Fatal(genErr)
	}
	if seen == nil || *seen != defaultAnthropicMaxTokens {
		t.Fatalf("registry-fallback MaxTokens = %v, want %d", seen, defaultAnthropicMaxTokens)
	}

	// 已有值不被覆盖
	preset := 128
	var seen2 *int
	_, err = kernel.OnWaterfall(scope, llm.EventBeforeGenerate,
		func(req *llm.GenerateRequest, next func(*llm.GenerateRequest) *llm.GenerateRequest) *llm.GenerateRequest {
			req.MaxTokens = &preset
			out := next(req)
			seen2 = out.MaxTokens
			return out
		})
	if err != nil {
		t.Fatal(err)
	}
	_, genErr = h.Model.Generate(context.Background(), llm.NewRequest(llm.UserText("hi2")))
	if genErr != nil {
		t.Fatal(genErr)
	}
	if seen2 == nil || *seen2 != preset {
		t.Fatalf("preset MaxTokens overwritten: %v, want %d", seen2, preset)
	}
}
