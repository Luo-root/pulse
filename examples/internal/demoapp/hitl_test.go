package demoapp

import (
	"bytes"
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

// waterfallOnce 手动派发一次 before_tool_call，返回最终裁决。
func decide(t *testing.T, scope *kernel.Context, name string) (bool, string) {
	t.Helper()
	btc := &loop.BeforeToolCall{Call: llm.ToolCall{ID: "t1", Name: name,
		Arguments: json.RawMessage(`{"path":"/tmp/x"}`)}}
	got := kernel.WaterfallLocal(scope, loop.EventBeforeToolCall, btc)
	return got.Rejected, got.RejectReason
}

func TestParseHITLMode(t *testing.T) {
	cases := map[string]HITLMode{
		"":            HITLDenylist,
		"denylist":    HITLDenylist,
		"interactive": HITLInteractive,
		"allowlist":   HITLAllowlist,
		"off":         HITLOff,
	}
	for in, want := range cases {
		got, err := ParseHITLMode(in)
		if err != nil || got != want {
			t.Fatalf("ParseHITLMode(%q)=%v,%v want %v", in, got, err, want)
		}
	}
	if _, err := ParseHITLMode("bogus"); err == nil {
		t.Fatal("bogus mode should error")
	}
}

func TestHITLDenylist(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	if _, err := InstallHITL(host, HITLDenylist, "delete_file", "", "x", nil, nil); err != nil {
		t.Fatal(err)
	}
	rejected, reason := decide(t, host, "delete_file")
	if !rejected || !strings.Contains(reason, "demo HITL policy") {
		t.Fatalf("delete_file should be rejected: %v %q", rejected, reason)
	}
	rejected, _ = decide(t, host, "lookup")
	if rejected {
		t.Fatal("lookup should pass")
	}
}

func TestHITLAllowlistDefaultDeny(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	if _, err := InstallHITL(host, HITLAllowlist, "", "lookup", "x", nil, nil); err != nil {
		t.Fatal(err)
	}
	rejected, reason := decide(t, host, "lookup")
	if rejected {
		t.Fatal("whitelisted lookup should pass")
	}
	rejected, reason = decide(t, host, "delete_file")
	if !rejected || !strings.Contains(reason, "default-deny") {
		t.Fatalf("default-deny expected, got %v %q", rejected, reason)
	}
}

func TestHITLInteractiveApprovals(t *testing.T) {
	cases := []struct {
		input       string
		call        string
		wantReject  bool
		wantTrusted bool
	}{
		{"y\n", "delete_file", false, false},
		{"n\n", "delete_file", true, false},
		{"a\n", "delete_file", false, true},
	}
	for _, c := range cases {
		host := kernel.New()
		trust := newSessionTrust()
		in := strings.NewReader(c.input)
		var out bytes.Buffer
		approver := newConsoleApprover("删除文件（模拟）", NewLineSource(in), &out, trust, nil)
		if _, err := kernel.OnWaterfall(host, loop.EventBeforeToolCall, approver.approve); err != nil {
			t.Fatal(err)
		}
		rejected, reason := decide(t, host, c.call)
		if rejected != c.wantReject {
			t.Fatalf("input=%q call=%s rejected=%v (%s); out=%s", c.input, c.call, rejected, reason, out.String())
		}
		if trust.Trusted(c.call) != c.wantTrusted {
			t.Fatalf("input=%q trusted=%v want %v; out=%s", c.input, trust.Trusted(c.call), c.wantTrusted, out.String())
		}
		host.Dispose()
	}
}

func TestHITLInteractiveAlwaysRemembers(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	trust := newSessionTrust()
	in := strings.NewReader("a\n")
	var out bytes.Buffer
	approver := newConsoleApprover("删除文件（模拟）", NewLineSource(in), &out, trust, nil)
	if _, err := kernel.OnWaterfall(host, loop.EventBeforeToolCall, approver.approve); err != nil {
		t.Fatal(err)
	}
	if rejected, _ := decide(t, host, "delete_file"); rejected {
		t.Fatal("first a should approve")
	}
	// 第二次调用：会话信任生效，不再询问（输入流已耗尽也不会被读）。
	if rejected, reason := decide(t, host, "delete_file"); rejected {
		t.Fatalf("trusted tool must auto-pass, got %q", reason)
	}
	if !trust.Trusted("delete_file") {
		t.Fatal("trust should persist")
	}
}

func TestHITLInteractiveFailClosedOnEOF(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	trust := newSessionTrust()
	in := strings.NewReader("") // EOF
	var out bytes.Buffer
	approver := newConsoleApprover("x", NewLineSource(in), &out, trust, nil)
	kernel.OnWaterfall(host, loop.EventBeforeToolCall, approver.approve)
	rejected, reason := decide(t, host, "delete_file")
	if !rejected || !strings.Contains(reason, "fail-closed") {
		t.Fatalf("EOF must fail closed, got %v %q", rejected, reason)
	}
}

func TestHITLInteractivePrintsPreview(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	reg := toolset.NewRegistry()
	_, err := reg.Register(host, toolset.Registration{
		Def:    llm.ToolDef{Name: "delete_file"},
		Fn:     func(context.Context, json.RawMessage) (string, error) { return "x", nil },
		Source: "local.delete_file",
		Risk:   toolset.RiskDangerous,
		PreviewFn: func(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
			return toolset.Preview{
				Kind:    toolset.KindOpaque,
				Subject: "delete",
				Opaque:  &toolset.OpaqueChange{Summary: "will delete", ArgsExcerpt: string(args)},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	trust := newSessionTrust()
	in := strings.NewReader("y\n")
	var out bytes.Buffer
	approver := newConsoleApprover("hint", NewLineSource(in), &out, trust, reg)
	if _, err := kernel.OnWaterfall(host, loop.EventBeforeToolCall, approver.approve); err != nil {
		t.Fatal(err)
	}
	rejected, _ := decide(t, host, "delete_file")
	if rejected {
		t.Fatal("y should approve")
	}
	got := out.String()
	if !strings.Contains(got, "preview kind=opaque") || !strings.Contains(got, "will delete") {
		t.Fatalf("missing preview card: %q", got)
	}
}

func TestHITLInteractivePreviewErrorStillAsks(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	reg := toolset.NewRegistry()
	_, err := reg.Register(host, toolset.Registration{
		Def:    llm.ToolDef{Name: "delete_file"},
		Fn:     func(context.Context, json.RawMessage) (string, error) { return "x", nil },
		Source: "local.delete_file",
		Risk:   toolset.RiskDangerous,
		PreviewFn: func(context.Context, json.RawMessage) (toolset.Preview, error) {
			return toolset.Preview{}, errors.New("boom")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	trust := newSessionTrust()
	in := strings.NewReader("n\n")
	var out bytes.Buffer
	approver := newConsoleApprover("hint", NewLineSource(in), &out, trust, reg)
	if _, err := kernel.OnWaterfall(host, loop.EventBeforeToolCall, approver.approve); err != nil {
		t.Fatal(err)
	}
	rejected, _ := decide(t, host, "delete_file")
	if !rejected {
		t.Fatal("n should reject; preview error must not auto-approve")
	}
	got := out.String()
	if !strings.Contains(got, "预览失败") || !strings.Contains(got, "boom") {
		t.Fatalf("want preview error line, got %q", got)
	}
	if !strings.Contains(got, "args=") {
		t.Fatalf("should fall back to args JSON: %q", got)
	}
}
