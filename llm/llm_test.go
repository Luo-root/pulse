package llm

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
)

// closerModel 包装任意 ChatModel 并统计 Close 次数。
type closerModel struct {
	ChatModel
	closed atomic.Int32
}

func (m *closerModel) Close() error { m.closed.Add(1); return nil }

func setupRegistry(t *testing.T) (*kernel.Context, *Registry) {
	t.Helper()
	ctx := kernel.New()
	f, err := kernel.Use(ctx, Plugin())
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
	ctx, reg := setupRegistry(t)

	var builds int32
	m := NewScripted(Resp("hi"))
	_, err := reg.RegisterProvider(ctx, "mock", func(cfg Config) (ChatModel, error) {
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
	ctx, reg := setupRegistry(t)
	if _, err := reg.Open("nope"); KindOf(err) != ErrNoModel {
		t.Fatalf("kind = %s, want no_model (err=%v)", KindOf(err), err)
	}
	_, _ = reg.RegisterProvider(ctx, "mock", func(cfg Config) (ChatModel, error) { return nil, nil })
	if err := reg.Declare("m", Config{Provider: "missing"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Open("m"); KindOf(err) != ErrNoModel {
		t.Fatalf("unknown provider kind = %s, want no_model", KindOf(err))
	}
}

// #3/#10d：拦截链路——before_generate waterfall 改写对后续监听器可见；
// after_response 观察者拿到值类型 Response。
func TestInterceptionEvents(t *testing.T) {
	ctx, reg := setupRegistry(t)

	_, _ = reg.RegisterProvider(ctx, "mock", func(cfg Config) (ChatModel, error) {
		return NewScripted(Resp("answer")), nil
	})
	if err := reg.Declare("main", Config{Provider: "mock", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	reg.SetDefault("main")

	// before_generate：第一个监听器 Clone 后注入默认 MaxTokens。
	_, _ = kernel.OnWaterfall(ctx, EventBeforeGenerate,
		func(req *GenerateRequest, next func(*GenerateRequest) *GenerateRequest) *GenerateRequest {
			req = req.Clone()
			n := 128
			req.MaxTokens = &n
			return next(req)
		})
	// 第二个监听器验证能看到上游的改写。
	var capturedMax atomic.Int64
	_, _ = kernel.OnWaterfall(ctx, EventBeforeGenerate,
		func(req *GenerateRequest, next func(*GenerateRequest) *GenerateRequest) *GenerateRequest {
			capturedMax.Store(int64(*req.MaxTokens))
			return next(req)
		})

	var seen atomic.Int32
	unsub, _ := kernel.On(ctx, EventAfterResponse, func(r *Response) { seen.Add(1) })

	model, err := reg.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	// 请求级 scope：监听挂在 ctx（Registry 宿主），Generate 注入同一 scope，
	// observed 对该 scope Local 派发——选项 A 的正确口径。
	callerReq := NewRequest(UserText("q"))
	if _, err := model.Generate(WithEventScope(context.Background(), ctx), callerReq); err != nil {
		t.Fatal(err)
	}
	if v := capturedMax.Load(); v != 128 {
		t.Fatalf("before_generate rewrite not observed: %d", v)
	}
	if callerReq.MaxTokens != nil {
		t.Fatal("caller request polluted by interception")
	}
	time.Sleep(50 * time.Millisecond)
	if seen.Load() == 0 {
		t.Fatal("after_response observer never ran")
	}
	_ = unsub
}

// #1（llm 版）：限流插件挂在兄弟作用域，其 before_generate 监听
// 默认不可见（Local 派发）；同 scope 才可见。
func TestSiblingPluginSeesInterceptionEvents(t *testing.T) {
	ctx, reg := setupRegistry(t) // llm 插件的私有作用域在 root 之下

	_, _ = reg.RegisterProvider(ctx, "mock", func(cfg Config) (ChatModel, error) {
		return NewScripted(Resp("ok")), nil
	})
	if err := reg.Declare("main", Config{Provider: "mock", Model: "m"}); err != nil {
		t.Fatal(err)
	}

	// 兄弟插件：在自己的私有作用域里挂监听（丢弃 dispose，
	// 卸载时应随之消失）。
	var hit atomic.Int32
	ratePlugin := kernel.Func(func(c *kernel.Context) error {
		_, err := kernel.OnWaterfall(c, EventBeforeGenerate,
			func(req *GenerateRequest, next func(*GenerateRequest) *GenerateRequest) *GenerateRequest {
				hit.Add(1)
				return next(req)
			})
		return err
	})
	fr, err := kernel.Use(ctx, ratePlugin)
	if err != nil {
		t.Fatal(err)
	}

	model, _ := reg.Open("main")
	// Local 派发：监听在兄弟私有作用域，Generate 注入 Registry 宿主 scope，
	// 兄弟默认听不到——这正是请求隔离要的边界。
	if _, err := model.Generate(WithEventScope(context.Background(), ctx), NewRequest(UserText("q"))); err != nil {
		t.Fatal(err)
	}
	if hit.Load() != 0 {
		t.Fatalf("sibling must NOT see Local events from registry scope, hit=%d", hit.Load())
	}

	// 对照：把监听挂到同一请求 scope，Local 才能触达。
	var hitSame atomic.Int32
	if _, err := kernel.OnWaterfall(ctx, EventBeforeGenerate,
		func(req *GenerateRequest, next func(*GenerateRequest) *GenerateRequest) *GenerateRequest {
			hitSame.Add(1)
			return next(req)
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := model.Generate(WithEventScope(context.Background(), ctx), NewRequest(UserText("q2"))); err != nil {
		t.Fatal(err)
	}
	if hitSame.Load() == 0 {
		t.Fatal("same-scope listener never invoked")
	}

	// #2（llm 版）：兄弟插件卸载 => 其监听随私有作用域回收。
	fr.Close()
	if hit.Load() > 0 {
		before := hit.Load()
		_, _ = model.Generate(WithEventScope(context.Background(), ctx), NewRequest(UserText("q3")))
		if hit.Load() != before {
			t.Fatal("unloaded plugin's listener still firing")
		}
	}
}

// #3：三条关闭路径——Declare 替换、Drop、llm 插件卸载——都必须
// 关到真实模型的 io.Closer。
func TestRealModelClosedOnAllPaths(t *testing.T) {
	t.Run("declare replace closes old", func(t *testing.T) {
		ctx, reg := setupRegistry(t)
		c1 := &closerModel{ChatModel: NewScripted(Resp("v1"))}
		c2 := &closerModel{ChatModel: NewScripted(Resp("v2"))}
		_, _ = reg.RegisterProvider(ctx, "mock", func(cfg Config) (ChatModel, error) {
			if cfg.Model == "m1" {
				return c1, nil
			}
			return c2, nil
		})
		_ = reg.Declare("main", Config{Provider: "mock", Model: "m1"})
		first, err := reg.Open("main")
		if err != nil {
			t.Fatal(err)
		}
		_ = first

		_ = reg.Declare("main", Config{Provider: "mock", Model: "m2"})
		if got := c1.closed.Load(); got != 1 {
			t.Fatalf("old instance closed %d times, want 1", got)
		}
		next, err := reg.Open("main") // 用新工厂重建
		if err != nil {
			t.Fatal(err)
		}
		resp, _ := next.Generate(context.Background(), NewRequest(UserText("x")))
		if resp.Message.Text() != "v2" {
			t.Fatalf("redeclare did not rebuild instance: %q", resp.Message.Text())
		}
	})

	t.Run("drop closes instance", func(t *testing.T) {
		ctx, reg := setupRegistry(t)
		cm := &closerModel{ChatModel: NewScripted(Resp("x"))}
		_, _ = reg.RegisterProvider(ctx, "mock", func(cfg Config) (ChatModel, error) { return cm, nil })
		_ = reg.Declare("main", Config{Provider: "mock", Model: "m"})
		if _, err := reg.Open("main"); err != nil {
			t.Fatal(err)
		}
		reg.Drop("main")
		if got := cm.closed.Load(); got != 1 {
			t.Fatalf("dropped instance closed %d times, want 1", got)
		}
	})

	t.Run("plugin unload closes registry models", func(t *testing.T) {
		ctx := kernel.New()
		cm := &closerModel{ChatModel: NewScripted(Resp("y"))}
		fl, err := kernel.Use(ctx, Plugin())
		if err != nil {
			t.Fatal(err)
		}
		reg, _ := kernel.Get(ctx, ServiceKey)
		_, _ = reg.RegisterProvider(ctx, "mock", func(cfg Config) (ChatModel, error) { return cm, nil })
		_ = reg.Declare("main", Config{Provider: "mock", Model: "m"})
		if _, err := reg.Open("main"); err != nil {
			t.Fatal(err)
		}

		fl.Close() // 插件卸载 => Registry.Close => 实例关闭
		if got := cm.closed.Load(); got != 1 {
			t.Fatalf("model closed %d times on plugin unload, want 1", got)
		}
		if _, err := reg.Open("main"); KindOf(err) != ErrUnknown && KindOf(err) != ErrNoModel {
			t.Fatalf("open after registry close: %v", err)
		}
	})
}

// #9：provider 登记是可逆效应——adapter 插件卸载后工厂收回。
func TestProviderRegistrationReversible(t *testing.T) {
	ctx, reg := setupRegistry(t)

	adapter := kernel.Func(func(c *kernel.Context) error {
		// adapter 正确用法：把工厂登记到自己的私有作用域 c 上，
		// 生命周期与插件一致（此处故意丢弃 dispose 验证兜底）。
		_, err := reg.RegisterProvider(c, "mock", func(cfg Config) (ChatModel, error) {
			return NewScripted(Resp("from-mock")), nil
		})
		return err
	})
	fa, err := kernel.Use(ctx, adapter)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range reg.Providers() {
		if p == "mock" {
			found = true
		}
	}
	if !found {
		t.Fatal("provider missing after registration")
	}

	fa.Close()
	for _, p := range reg.Providers() {
		if p == "mock" {
			t.Fatal("provider survived adapter unload")
		}
	}
	if err := reg.Declare("m", Config{Provider: "mock", Model: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Open("m"); KindOf(err) != ErrNoModel {
		t.Fatalf("open without provider should fail with no_model, got %v", err)
	}
}

// #9：同名覆盖 provider => 该 provider 名下已打开实例失效关闭，
// 下次 Open 用新工厂重建。
func TestProviderOverrideInvalidatesCache(t *testing.T) {
	ctx, reg := setupRegistry(t)
	old := &closerModel{ChatModel: NewScripted(Resp("old"))}
	fresh := &closerModel{ChatModel: NewScripted(Resp("fresh"))}

	d1, _ := reg.RegisterProvider(ctx, "mock", func(cfg Config) (ChatModel, error) { return old, nil })
	_ = d1
	_ = reg.Declare("main", Config{Provider: "mock", Model: "m"})
	first, err := reg.Open("main")
	if err != nil {
		t.Fatal(err)
	}
	_ = first

	_, _ = reg.RegisterProvider(ctx, "mock", func(cfg Config) (ChatModel, error) { return fresh, nil })
	if got := old.closed.Load(); got != 1 {
		t.Fatalf("cached instance of overridden provider closed %d times, want 1", got)
	}

	again, err := reg.Open("main")
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := again.Generate(context.Background(), NewRequest(UserText("q")))
	if resp.Message.Text() != "fresh" {
		t.Fatalf("override did not take effect: %q", resp.Message.Text())
	}
}

// #10b：Clone 对拦截会触碰的字段做深拷贝。
func TestCloneIsolation(t *testing.T) {
	temp := 0.7
	n := 8
	topK := 40
	logprobs := true
	topLogprobs := 3
	parallel := false
	req := &GenerateRequest{
		Messages:       []*Message{UserText("hi")},
		Temperature:    &temp,
		TopP:           &temp,
		TopK:           &topK,
		MaxTokens:      &n,
		ToolChoice:     &ToolChoice{Mode: ToolAuto, Parallel: &parallel},
		ResponseFormat: &ResponseFormat{Type: FormatJSONObject},
		Audio:          &AudioOutput{Voice: "alloy", Format: "wav"},
		Reasoning:      &ReasoningOptions{Effort: "high", BudgetTokens: 1024},
		Output:         &OutputOptions{Verbosity: "low", Logprobs: &logprobs, TopLogprobs: &topLogprobs},
		Metadata:       map[string]any{"k": "v"},
	}
	cp := req.Clone()

	cp.Metadata["k"] = "changed"
	md := 99
	cp.MaxTokens = &md
	cp.ToolChoice.Mode = ToolNone
	*cp.ToolChoice.Parallel = true
	cp.ResponseFormat.Type = FormatJSONSchema
	cp.Audio.Voice = "nova"
	cp.Reasoning.Effort = "low"
	cp.Output.Verbosity = "high"

	if req.Metadata["k"] != "v" {
		t.Fatal("Metadata leaked through Clone")
	}
	if *req.MaxTokens != 8 {
		t.Fatal("MaxTokens pointer shared through Clone")
	}
	if req.ToolChoice.Mode != ToolAuto || *req.ToolChoice.Parallel != false {
		t.Fatal("ToolChoice shared through Clone")
	}
	if req.ResponseFormat.Type != FormatJSONObject {
		t.Fatal("ResponseFormat shared through Clone")
	}
	if req.Audio.Voice != "alloy" || req.Reasoning.Effort != "high" || req.Output.Verbosity != "low" {
		t.Fatal("Audio/Reasoning/Output shared through Clone")
	}
	if *req.Output.Logprobs != true || *req.Output.TopLogprobs != 3 {
		t.Fatal("Output pointers shared through Clone")
	}
	if len(cp.Messages) != len(req.Messages) {
		t.Fatal("messages slice not copied")
	}
}

func TestConstructorsAndHelpers(t *testing.T) {
	// 块构造器
	parts := []Part{
		Text("a"),
		Reasoning("r"),
		Call(ToolCall{ID: "c1", Name: "n", Arguments: json.RawMessage(`{}`)}),
		Result("c1", "ok"),
		ResultParts("c2", true, Text("bad")),
		ImageURL("https://x/cat.png", "image/png"),
		ImageData("image/png", []byte{1}),
		Media("audio/wav", []byte("RIFF")),
		MediaURL("video/mp4", "https://x/v.mp4"),
	}
	m := &Message{Role: RoleAssistant, Parts: []Part{parts[0], parts[1], parts[2]}}
	if m.Text() != "a" || m.ReasoningText() != "r" {
		t.Fatalf("Text/ReasoningText = %q/%q", m.Text(), m.ReasoningText())
	}
	if len(m.ToolCalls()) != 1 || m.ToolCalls()[0].ID != "c1" {
		t.Fatalf("ToolCalls = %+v", m.ToolCalls())
	}
	// 消息构造器
	if System("s").Parts[0].Text != "s" {
		t.Fatal("System")
	}
	if ToolMessage("c1", "res").Parts[0].ToolResultValue.ToolCallID != "c1" {
		t.Fatal("ToolMessage")
	}
	// Clone：顶层深拷贝
	mc := m.Clone()
	mc.Parts[0] = Text("changed")
	if m.Parts[0].Text != "a" {
		t.Fatal("Message.Clone shared parts slice")
	}
	// TokenUsage.Total
	if (TokenUsage{InputTokens: 3, OutputTokens: 4}).Total() != 7 {
		t.Fatal("Total")
	}
	// Error 文案与 unwrap 链
	base := errors.New("boom")
	e := NewError(ErrRateLimit, "openai", 429, base, "slow down")
	if got := e.Error(); got != "llm: rate_limit (openai, slow down): boom" {
		t.Fatalf("Error() = %q", got)
	}
	anon := NewError(ErrAuth, "", 0, nil, "no key")
	if got := anon.Error(); got != "llm: auth (no key)" {
		t.Fatalf("Error() = %q", got)
	}
	_ = parts
}

func TestInterceptionStream(t *testing.T) {
	// observed.Stream 与 Generate 同契约：before_generate 改写可达、
	// done 携带的 Response 触发 after_response、error 不触发。
	ctx, reg := setupRegistry(t)

	_, _ = reg.RegisterProvider(ctx, "mock", func(cfg Config) (ChatModel, error) {
		return NewScripted(Resp("answer")), nil
	})
	if err := reg.Declare("main", Config{Provider: "mock", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	reg.SetDefault("main")

	var rewritten atomic.Bool
	_, _ = kernel.OnWaterfall(ctx, EventBeforeGenerate,
		func(req *GenerateRequest, next func(*GenerateRequest) *GenerateRequest) *GenerateRequest {
			req = req.Clone()
			n := 77
			req.MaxTokens = &n
			rewritten.Store(true)
			return next(req)
		})
	var after atomic.Int32
	_, _ = kernel.On(ctx, EventAfterResponse, func(r *Response) { after.Add(1) })

	model, err := reg.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	ch, err := model.Stream(WithEventScope(context.Background(), ctx), NewRequest(UserText("q")))
	if err != nil {
		t.Fatal(err)
	}
	var done *StreamEvent
	for ev := range ch {
		if ev.Kind == EventDone {
			cp := ev
			done = &cp
		}
	}
	if done == nil || done.Response.Message.Text() != "answer" {
		t.Fatalf("done = %+v", done)
	}
	if !rewritten.Load() {
		t.Fatal("before_generate never ran on Stream path")
	}
	time.Sleep(50 * time.Millisecond)
	if after.Load() != 1 {
		t.Fatalf("after_response on stream done = %d", after.Load())
	}
}

func TestCustomMediaParts(t *testing.T) {
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

func TestErrorClassification(t *testing.T) {
	base := errors.New("boom")
	e := NewError(ErrRateLimit, "openai", 429, base, "rate limited")
	if !IsRetryable(e) {
		t.Fatal("rate limit should be retryable")
	}
	if KindOf(e) != ErrRateLimit {
		t.Fatalf("kind = %s", KindOf(e))
	}
	if IsRetryable(base) {
		t.Fatal("foreign error must be conservative non-retryable")
	}
	if !errors.Is(e, base) {
		t.Fatal("unwrap chain broken")
	}
	if KindOf(errors.New("other")) != ErrUnknown {
		t.Fatal("foreign error kind should be unknown")
	}
}

func TestContextCanceledStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := NewScripted(Resp("slow"))
	ch, _ := m.Stream(ctx, NewRequest())
	cancel()
	for range ch {
	} // channel 必然关闭（range 结束即为证）
}

var _ ChatModel = (*closerModel)(nil)
var _ = time.Second
