package toolset_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
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

// TestPreviewIdentityUnderConcurrentDispose：#215-3——卡片的三样身份事实
// （Tool / Source / Risk）必须与 PreviewFn 出自**同一份快照**。
//
// 分两次查（先 LookupPreview 再 LookupMeta）会在中间被撤销撕开，产出
// 「ok=true 但 Source 空、Risk 零值」的卡片，再被 ActionFromRisk 兜成
// execute：把零值当低风险放行的策略（switch risk { case ReadWrite,
// Dangerous: 问人; default: 放行 }）因此被绕过，LookupMeta 的 fail-closed
// 语义也在卡片路径上丢掉。
func TestPreviewIdentityUnderConcurrentDispose(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	r := toolset.NewRegistry()

	register := func() (func(), error) {
		d, err := r.Register(host, toolset.Registration{
			Def:    llm.ToolDef{Name: "q", Description: "q"},
			Fn:     echoFn("ok"),
			Source: "local.q",
			Risk:   toolset.RiskDangerous,
			PreviewFn: func(context.Context, json.RawMessage) (toolset.Preview, error) {
				return toolset.Preview{
					Kind:    toolset.KindOpaque,
					Subject: "q",
					Opaque:  &toolset.OpaqueChange{Summary: "q"},
				}, nil
			},
		})
		return d, err
	}

	// 撤销方的注册失败只能回传主 goroutine——t.Fatal / FailNow 的契约只
	// 适用于测试 goroutine（FailNow 只终止当前 goroutine，失败会静默漏掉，
	// `go vet` 也抓不到这种经闭包的间接调用）。
	stop := make(chan struct{})
	fail := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			d, err := register()
			if err != nil {
				select {
				case fail <- err:
				default:
				}
				return
			}
			d()
		}
	}()

	drain := func() {
		select {
		case err := <-fail:
			close(stop)
			wg.Wait()
			t.Fatalf("register 在并发撤销中失败: %v", err)
		default:
		}
	}

	var bad int
	var sample toolset.Preview
	for i := 0; i < 50000; i++ {
		drain()
		p, ok, err := r.Preview(context.Background(), "q", nil)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("Preview: %v", err)
		}
		if !ok {
			continue
		}
		if p.Source == "" || p.Risk != toolset.RiskDangerous {
			bad++
			if bad == 1 {
				sample = p
			}
		}
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-fail:
		t.Fatalf("register 在并发撤销中失败: %v", err)
	default:
	}

	if bad != 0 {
		t.Fatalf("ok=true 的卡片带着零值身份字段 %d 次；样本 source=%q risk=%v action=%s",
			bad, sample.Source, sample.Risk, sample.Action)
	}
}
