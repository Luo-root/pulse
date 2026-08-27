package demoapp

import (
	"context"
	"testing"

	"github.com/Luo-root/pulse/llm"
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
	h, err := Open(Flags{Scripted: true, TraceID: "t-scripted"}, llm.Resp("hello from scripted"))
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
	h, err := Open(Flags{Scripted: true, TraceID: "t-close"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := GetRegistry(h); !ok {
		t.Fatal("registry should be provided before close")
	}
	observ := ObservabilityReporter(h)
	if observ == nil {
		t.Fatal("reporter should be provided before close")
	}
	h.Close()
	if _, ok := GetRegistry(h); ok {
		t.Fatal("llm registry binding should be gone after Close")
	}
	if ObservabilityReporter(h) != nil {
		t.Fatal("observability reporter should be gone after Close")
	}
}
