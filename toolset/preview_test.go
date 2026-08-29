package toolset_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

func TestPreviewOptionalAndDispose(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()

	_, ok := r.LookupPreview("write")
	if ok {
		t.Fatal("empty registry must not report preview")
	}
	p, ok, err := r.Preview(context.Background(), "write", nil)
	if err != nil || ok || p.Tool != "" {
		t.Fatalf("empty Preview: %+v ok=%v err=%v", p, ok, err)
	}

	dispose := mustReg(t, r, host, "echo", "local.echo", toolset.RiskReadonly, "ok")
	if _, ok := r.LookupPreview("echo"); ok {
		t.Fatal("nil PreviewFn must be absent")
	}
	dispose()

	fn := func(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
		return toolset.Preview{
			Kind:    toolset.KindOpaque,
			Subject: "echo",
			Opaque:  &toolset.OpaqueChange{Summary: "echo", ArgsExcerpt: string(args)},
		}, nil
	}
	d, err := r.Register(host, toolset.Registration{
		Def:       llm.ToolDef{Name: "echo"},
		Fn:        echoFn("ok"),
		Source:    "local.echo",
		Risk:      toolset.RiskReadonly,
		PreviewFn: fn,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := r.Preview(context.Background(), "echo", json.RawMessage(`{"x":1}`))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.Tool != "echo" || got.Source != "local.echo" || got.Risk != toolset.RiskReadonly {
		t.Fatalf("identity not filled: %+v", got)
	}
	if got.Action != toolset.ActionRead {
		t.Fatalf("action=%s", got.Action)
	}
	if got.Opaque == nil || !strings.Contains(got.Opaque.ArgsExcerpt, `"x":1`) {
		t.Fatalf("opaque=%+v", got.Opaque)
	}
	d()
	if _, ok := r.LookupPreview("echo"); ok {
		t.Fatal("dispose must drop PreviewFn")
	}
}

func TestPreviewFnErrorStillOk(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()
	boom := errors.New("preview boom")
	_, err := r.Register(host, toolset.Registration{
		Def:    llm.ToolDef{Name: "w"},
		Fn:     echoFn("x"),
		Source: "local",
		Risk:   toolset.RiskReadWrite,
		PreviewFn: func(context.Context, json.RawMessage) (toolset.Preview, error) {
			return toolset.Preview{}, boom
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ok, err := r.Preview(context.Background(), "w", nil)
	if !ok || err != boom {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestPreviewRender(t *testing.T) {
	p := toolset.Preview{
		Kind:   toolset.KindFile,
		Action: toolset.ActionWrite,
		Risk:   toolset.RiskReadWrite,
		Source: "builtins.write",
		File: &toolset.FileChange{
			Op: "modify", Path: "a.txt", Added: 1, Removed: 1,
			Diff: "--- a\n+++ b\n",
		},
	}
	s := p.Render()
	if !strings.Contains(s, "+1/-1") || !strings.Contains(s, "a.txt") {
		t.Fatalf("%s", s)
	}
}
