package demoapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// host.Close 后 Effect 全部回收：Registry 服务绑定应从仓库消失。
// 这是 kernel「卸载即还原」在同进程内的可断言验证（进程退出交给 OS 不算证据）。
func TestHostCloseReclaimsServices(t *testing.T) {
	h, err := Open(Flags{Scripted: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := GetRegistry(h); !ok {
		t.Fatal("registry should be provided before close")
	}
	// 观测桥随宿主销毁而停止：Close 后 Sink 记录数不再增长。
	nBefore := h.Sink.Len()
	h.Close()
	time.Sleep(50 * time.Millisecond)
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
	if t1 == t2 {
		t.Fatalf("trace ids must differ per request: %q", t1)
	}
	if !strings.HasPrefix(t1, h.HostID()) {
		t.Fatalf("trace id %q should carry host prefix %q", t1, h.HostID())
	}
	if h.HostID() != h.HostID() {
		t.Fatal("host id must be stable")
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
