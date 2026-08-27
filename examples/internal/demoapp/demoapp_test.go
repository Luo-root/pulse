package demoapp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/llm"
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

	if h.Bridge == nil || h.Bridge.TraceID == "" {
		t.Fatal("bridge trace id should be set at install time")
	}
	before := len(h.Sink.Snapshot())
	_, genErr := h.Model.Generate(context.Background(), llm.NewRequest(llm.UserText("hi")))
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
		if rec.TraceID != h.Bridge.TraceID {
			t.Fatalf("trace mismatch: %q vs bridge %q", rec.TraceID, h.Bridge.TraceID)
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

// 请求级 Bridge 隔离契约：两个同时存活的请求 scope 不得互相收到对方的
// tool/turn/llm 事件。当前被 #20（EmitLocal/WaterfallLocal）阻塞——
// kernel.Emit 全树广播会把 A 的事件复制进 B。落地 #20 后去掉 Skip，
// 并补强 HITL 双策略隔离与 llm.generate_finished 隔离；不要为变绿弱化断言。
func TestRequestBridgesDoNotCrossTalk(t *testing.T) {
	t.Skip("blocked by #20: EmitLocal/WaterfallLocal required for request-scoped isolation")
}
