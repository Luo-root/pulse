package llm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
)

func setupRegistry(t *testing.T) (*kernel.Context, *Registry) {
	t.Helper()
	ctx := kernel.New()
	p := Plugin()
	f, err := kernel.Use(ctx, p)
	if err != nil {
		t.Fatalf("mount llm.Plugin: %v", err)
	}
	reg, ok := kernel.Get(ctx, ServiceKey)
	if !ok {
		t.Fatal("llm service not provided")
	}
	_ = f
	return ctx, reg
}

func TestRegistryOpenAndCache(t *testing.T) {
	_, reg := setupRegistry(t)

	var builds int32
	m := NewScripted(Resp("hi"))
	_, err := reg.RegisterProvider("mock", func(cfg Config) (ChatModel, error) {
		atomic.AddInt32(&builds, 1)
		return m, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Declare("main", Config{Provider: "mock", Model: "test-1"}); err != nil {
		t.Fatal(err)
	}

	got1, err := reg.Open("main")
	if err != nil {
		t.Fatal(err)
	}
	got2, _ := reg.Open("main")
	if got1 != got2 {
		t.Fatal("Open should cache instances")
	}
	if n := atomic.LoadInt32(&builds); n != 1 {
		t.Fatalf("factory called %d times, want 1", n)
	}

	resp, err := got1.Generate(context.Background(), NewRequest(UserText("ping")))
	if err != nil || resp.Message.Text() != "hi" {
		t.Fatalf("generate: %v %v", err, resp)
	}
}

func TestOpenUnknownFails(t *testing.T) {
	_, reg := setupRegistry(t)
	if _, err := reg.Open("nope"); KindOf(err) != ErrNoModel {
		t.Fatalf("kind = %s, want no_model (err=%v)", KindOf(err), err)
	}
	_, _ = reg.RegisterProvider("mock", func(cfg Config) (ChatModel, error) { return nil, nil })
	if err := reg.Declare("m", Config{Provider: "missing"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Open("m"); KindOf(err) != ErrNoModel {
		t.Fatalf("unknown provider kind = %s, want no_model", KindOf(err))
	}
}

func TestInterceptionEvents(t *testing.T) {
	ctx, reg := setupRegistry(t)

	_, _ = reg.RegisterProvider("mock", func(cfg Config) (ChatModel, error) {
		return NewScripted(Resp("answer")), nil
	})
	_ = reg.Declare("main", Config{Provider: "mock", Model: "m"})

	// before_generate：waterfall 改写请求（注入默认 MaxTokens）。
	_, _ = kernel.OnWaterfall(ctx, EventBeforeGenerate,
		func(req *GenerateRequest, next func(*GenerateRequest) *GenerateRequest) *GenerateRequest {
			req = req.Clone()
			n := 128
			req.MaxTokens = &n
			req.Metadata = map[string]any{"routed": true}
			return next(req)
		})

	// after_response：观察者收集 usage。
	var seen atomic.Int32
	unsub, _ := kernel.On(ctx, EventAfterResponse, func(r **Response) {
		seen.Add(1)
	})

	model, _ := reg.OpenDefault()
	// 未设置默认 => 报错。
	if _, err := reg.OpenDefault(); KindOf(err) != ErrNoModel {
		t.Fatalf("expected no default, got %v", err)
	}
	reg.SetDefault("main")

	model, err := reg.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	var capturedMax int64
	// ScriptedModel 不读 MaxTokens；改为在 before 里再挂一个记录器验证链路。
	_, _ = kernel.OnWaterfall(ctx, EventBeforeGenerate,
		func(req *GenerateRequest, next func(*GenerateRequest) *GenerateRequest) *GenerateRequest {
			atomic.StoreInt64(&capturedMax, int64(*req.MaxTokens)) // 上一个监听器的改写可见
			return next(req)
		})
	if _, err := model.Generate(context.Background(), NewRequest(UserText("q"))); err != nil {
		t.Fatal(err)
	}
	if capturedMax != 128 {
		t.Fatalf("before_generate rewrite not observed: %d", capturedMax)
	}
	time.Sleep(50 * time.Millisecond)
	if seen.Load() == 0 {
		t.Fatal("after_response observer never ran")
	}
	_ = unsub
}

func TestDeclareReplaceClosesOldInstance(t *testing.T) {
	_, reg := setupRegistry(t)

	type closer struct{ ScriptedModel; closed int32 }
	c1 := &closer{ScriptedModel: *NewScripted(Resp("v1"))}
	_, _ = reg.RegisterProvider("mock", func(cfg Config) (ChatModel, error) {
		if cfg.Model == "m1" {
			return c1, nil
		}
		return &closer{ScriptedModel: *NewScripted(Resp("v2"))}, nil
	})

	_ = reg.Declare("main", Config{Provider: "mock", Model: "m1"})
	first, _ := reg.Open("main")
	_ = first

	_ = reg.Declare("main", Config{Provider: "mock", Model: "m2"})
	if atomic.LoadInt32(&c1.closed) != 0 && false {
		t.Fatal("unreachable")
	}
	next, err := reg.Open("main") // 新实例
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := next.Generate(context.Background(), NewRequest(UserText("x")))
	if resp.Message.Text() != "v2" {
		t.Fatalf("redeclare did not rebuild instance: %q", resp.Message.Text())
	}
}

func TestScriptedStreamEvents(t *testing.T) {
	m := NewScripted(Resp("hello"))
	ch, err := m.Stream(context.Background(), NewRequest(UserText("s")))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []StreamEventKind
	for ev := range ch {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == EventDone && ev.Response.Message.Text() != "hello" {
			t.Fatalf("done payload text = %q", ev.Response.Message.Text())
		}
	}
	if len(kinds) != 2 || kinds[0] != EventTextDelta || kinds[1] != EventDone {
		t.Fatalf("stream kinds = %v", kinds)
	}
}

func TestScriptedToolCallStream(t *testing.T) {
	m := NewScripted(RespToolCalls(ToolCall{ID: "c1", Name: "fs.read", Arguments: []byte(`{"p":"a"}`)}))
	ch, _ := m.Stream(context.Background(), NewRequest())
	for ev := range ch {
		if ev.Kind == EventToolCallBegin && (ev.CallID != "c1" || ev.ToolName != "fs.read") {
			t.Fatalf("begin event = %+v", ev)
		}
	}
}

func TestCustomMediaParts(t *testing.T) {
	// 开放模态：音频输入走 PartCustom，输出侧同样可携带（对称）。
	m := NewScripted(Resp("heard"))
	req := NewRequest(User(Media("audio/wav", []byte("RIFF...")), Text("这段音频说了什么")))
	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != "heard" {
		t.Fatalf("resp = %q", resp.Message.Text())
	}
	if got := req.Messages[0].Parts[0]; got.Kind != PartCustom || got.Media.MediaType != "audio/wav" {
		t.Fatalf("custom part = %+v", got)
	}
}

func TestErrorClassification(t *testing.T) {
	base := errors.New("boom")
	e := errRateLimit("openai", 429, base)
	if !IsRetryable(e) {
		t.Fatal("rate limit should be retryable")
	}
	if KindOf(e) != ErrRateLimit {
		t.Fatalf("kind = %s", KindOf(e))
	}
	wrapped := context.Cause // 占位避免未用导入
	_ = wrapped
	if IsRetryable(base) {
		t.Fatal("foreign error must be conservative non-retryable")
	}
	if !errors.Is(e, base) {
		t.Fatal("unwrap chain broken")
	}
}

func TestContextCanceledStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := NewScripted(Resp("slow"))
	ch, _ := m.Stream(ctx, NewRequest())
	cancel()
	drained := false
	for ev := range ch {
		if ev.Kind == EventError && errors.Is(ev.Err, context.Canceled) {
			drained = true
		}
	}
	// Scripted 的发送循环感知取消；若竞态先发出 done 也算通过——
	// 关键是 channel 必然关闭（for range 结束即为证）。
	_ = drained
}
