package llm_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/observability"
)

// newObservedModel 经 Registry 装配 scripted 模型：事件派发在 observed
// 包装层，裸 ScriptedModel 不发 llm 事件。slowModel 包一层延迟——
// Windows 时钟粒度下瞬时 Generate 的 Duration 可能同为 0，而 Duration>0
// 正是「计时起点生效」的语义锚。
type slowModel struct{ llm.ChatModel }

func (m slowModel) Generate(ctx context.Context, req *llm.GenerateRequest) (*llm.Response, error) {
	time.Sleep(2 * time.Millisecond)
	return m.ChatModel.Generate(ctx, req)
}

func newObservedModel(t *testing.T, scope *kernel.Context, steps ...*llm.Response) llm.ChatModel {
	t.Helper()
	reg := llm.NewRegistry(scope)
	if _, err := reg.RegisterProvider(scope, "mock", func(llm.Config) (llm.ChatModel, error) {
		return slowModel{llm.NewScripted(steps...)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Declare("main", llm.Config{Provider: "mock"}); err != nil {
		t.Fatal(err)
	}
	reg.SetDefault("main")
	m, err := reg.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func respWithUsage(text string) *llm.Response {
	return &llm.Response{
		Message:      llm.AssistantText(text),
		FinishReason: llm.FinishStop,
		Model:        "test-model",
		Usage:        llm.TokenUsage{InputTokens: 10, OutputTokens: 5, CachedInputTokens: 2},
	}
}

// 折叠锚：Generate 产生一条 generate_finished，信封与 attrs 逐项断言。
func TestObserveFoldsGenerateFinished(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}
	if err := llm.Observe(scope, observability.ObserveConfig{Sink: sink, HostID: "h1", TraceID: "tr-1"}); err != nil {
		t.Fatal(err)
	}

	model := newObservedModel(t, scope, respWithUsage("done"))
	if _, err := model.Generate(llm.WithEventScope(context.Background(), scope), llm.NewRequest(llm.UserText("q"))); err != nil {
		t.Fatal(err)
	}

	recs := sink.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.HostID != "h1" || rec.TraceID != "tr-1" || rec.Source != observability.SourceAdapter {
		t.Fatalf("envelope mismatch: %+v", rec)
	}
	if rec.Event != llm.EventGenerateFinished || rec.Status != string(llm.FinishStop) {
		t.Fatalf("event/status wrong: %+v", rec)
	}
	if rec.Duration <= 0 {
		t.Fatal("duration should be > 0")
	}
	if rec.FiberName != "" {
		t.Fatalf("named fields must stay zero for adapter records: %+v", rec)
	}
	if v, ok := observability.Get[string](rec.Attrs, llm.AttrModel); !ok || v != "test-model" {
		t.Fatalf("model attr: %q %v", v, ok)
	}
	if v, ok := observability.Get[int64](rec.Attrs, llm.AttrTokensIn); !ok || v != 10 {
		t.Fatalf("tokens_in: %v %v", v, ok)
	}
	if v, ok := observability.Get[int64](rec.Attrs, llm.AttrTokensOut); !ok || v != 5 {
		t.Fatalf("tokens_out: %v %v", v, ok)
	}
	if v, ok := observability.Get[int64](rec.Attrs, llm.AttrTokensCached); !ok || v != 2 {
		t.Fatalf("tokens_cached: %v %v", v, ok)
	}
}

// 透传锚：Observe 的计时监听不吞不改请求，后续监听与调用方均不受影响。
func TestObserveWaterfallPassthrough(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}
	if err := llm.Observe(scope, observability.ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr"}); err != nil {
		t.Fatal(err)
	}
	model := newObservedModel(t, scope, llm.Resp("ok"))

	var seen *int
	if _, err := kernel.OnWaterfall(scope, llm.EventBeforeGenerate,
		func(req *llm.GenerateRequest, next func(*llm.GenerateRequest) *llm.GenerateRequest) *llm.GenerateRequest {
			if req.MaxTokens != nil {
				seen = req.MaxTokens
			}
			return next(req)
		}); err != nil {
		t.Fatal(err)
	}

	caller := llm.NewRequest(llm.UserText("q"))
	if _, err := model.Generate(llm.WithEventScope(context.Background(), scope), caller); err != nil {
		t.Fatal(err)
	}
	if seen != nil {
		t.Fatalf("request was rewritten by observe listener: %v", *seen)
	}
	if caller.MaxTokens != nil {
		t.Fatal("caller request polluted by interception")
	}
	if len(sink.Snapshot()) != 1 {
		t.Fatalf("records = %d, want 1 (folding still works)", len(sink.Snapshot()))
	}
}

// 可选注册：不调 Observe 即零观测足迹。
func TestObserveOptional(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}

	model := newObservedModel(t, scope, llm.Resp("silent"))
	if _, err := model.Generate(llm.WithEventScope(context.Background(), scope), llm.NewRequest(llm.UserText("q"))); err != nil {
		t.Fatal(err)
	}
	if len(sink.Snapshot()) != 0 {
		t.Fatalf("no observe attached but sink has %d records", len(sink.Snapshot()))
	}
}

// 隔离与摘除锚（#128 验收 4）：双 scope 共用同一 Sink——A 的记录不进
// B 的断言集；A 的 scope Dispose 后再跑 B，A 记录数零增长。
func TestObserveIsolationAndDispose(t *testing.T) {
	sink := &observability.MemorySink{}
	countTrace := func(id string) int {
		n := 0
		for _, rec := range sink.Snapshot() {
			if rec.TraceID == id {
				n++
			}
		}
		return n
	}

	scopeA := kernel.New()
	if err := llm.Observe(scopeA, observability.ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-a"}); err != nil {
		t.Fatal(err)
	}
	modelA := newObservedModel(t, scopeA, llm.Resp("a done"))
	if _, err := modelA.Generate(llm.WithEventScope(context.Background(), scopeA), llm.NewRequest(llm.UserText("a"))); err != nil {
		t.Fatal(err)
	}
	countA := countTrace("tr-a")
	if countA == 0 {
		t.Fatal("request A produced no records")
	}
	scopeA.Dispose()

	scopeB := kernel.New()
	t.Cleanup(scopeB.Dispose)
	if err := llm.Observe(scopeB, observability.ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-b"}); err != nil {
		t.Fatal(err)
	}
	modelB := newObservedModel(t, scopeB, llm.Resp("b done"))
	if _, err := modelB.Generate(llm.WithEventScope(context.Background(), scopeB), llm.NewRequest(llm.UserText("b"))); err != nil {
		t.Fatal(err)
	}
	if got := countTrace("tr-a"); got != countA {
		t.Fatalf("request A grew after dispose: %d -> %d", countA, got)
	}
	if countTrace("tr-b") == 0 {
		t.Fatal("request B produced no records")
	}
}

// 装配校验：nil scope / nil Sink 哨兵。
func TestObserveValidation(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	if err := llm.Observe(nil, observability.ObserveConfig{Sink: &observability.MemorySink{}}); err != observability.ErrNilScope {
		t.Fatalf("nil scope err = %v", err)
	}
	if err := llm.Observe(scope, observability.ObserveConfig{TraceID: "t"}); err != observability.ErrNilSink {
		t.Fatalf("nil sink err = %v", err)
	}
}

// 归因锚（#144）：同 scope 并发双实例——两条 Declare 各自 Open，
// 记录以 llm.instance 区分归属；Started 锚点随事件携带，并发下
// Duration 仍各自 >0（无共享闭包计时）。
func TestObserveConcurrentInstanceAttribution(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &observability.MemorySink{}
	if err := llm.Observe(scope, observability.ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-multi"}); err != nil {
		t.Fatal(err)
	}

	reg := llm.NewRegistry(scope)
	if _, err := reg.RegisterProvider(scope, "mock", func(llm.Config) (llm.ChatModel, error) {
		return slowModel{llm.NewScripted(respWithUsage("done"))}, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := reg.Declare(id, llm.Config{Provider: "mock"}); err != nil {
			t.Fatal(err)
		}
	}
	open := func(id string) llm.ChatModel {
		t.Helper()
		m, err := reg.Open(id)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	const perInstance = 8
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		m := open(id)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perInstance; i++ {
				if _, err := m.Generate(llm.WithEventScope(context.Background(), scope), llm.NewRequest(llm.UserText("q"))); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	recs := sink.Snapshot()
	if len(recs) != 2*perInstance {
		t.Fatalf("records = %d, want %d", len(recs), 2*perInstance)
	}
	counts := map[string]int{}
	for _, rec := range recs {
		if rec.Event != llm.EventGenerateFinished || rec.Duration <= 0 {
			t.Fatalf("event/duration wrong: %+v", rec)
		}
		v, ok := observability.Get[string](rec.Attrs, llm.AttrInstance)
		if !ok {
			t.Fatalf("record missing %s attr: %+v", llm.AttrInstance, rec)
		}
		counts[v]++
	}
	if counts["a"] != perInstance || counts["b"] != perInstance {
		t.Fatalf("instance attribution = %v, want a=%d b=%d", counts, perInstance, perInstance)
	}
}

// recordChan 是「写入即信号」的 Sink：断言与折叠完成同步，不轮询、不 sleep。
type recordChan chan observability.Record

func (c recordChan) Write(r observability.Record) { c <- r }

// firstByteDelayModel 把「首字节前的往返」建模为 inner 调用自身的耗时：
// Generate 直接睡在调用里；Stream 睡在建流阶段（连接 + 请求发送 + 等
// 首个增量）——这正是流式观测原先漏计的那一段。
type firstByteDelayModel struct {
	llm.ChatModel
	delay time.Duration
}

func (m firstByteDelayModel) Generate(ctx context.Context, req *llm.GenerateRequest) (*llm.Response, error) {
	time.Sleep(m.delay)
	return m.ChatModel.Generate(ctx, req)
}

func (m firstByteDelayModel) Stream(ctx context.Context, req *llm.GenerateRequest) (<-chan llm.StreamEvent, error) {
	time.Sleep(m.delay)
	return m.ChatModel.Stream(ctx, req)
}

// Duration 口径对称锚：Stream 的 Started 必须落在 inner.Stream **之前**，
// 否则首字节前那一段不计入，同一个模型走 Generate 与走 Stream 观测出的
// Duration 不可比（流式短、非流式长）。
func TestDurationCoversPreFirstByteOnBothPaths(t *testing.T) {
	const delay = 30 * time.Millisecond

	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	recs := make(recordChan, 4)
	if err := llm.Observe(scope, observability.ObserveConfig{Sink: recs, HostID: "h", TraceID: "tr"}); err != nil {
		t.Fatal(err)
	}
	reg := llm.NewRegistry(scope)
	if _, err := reg.RegisterProvider(scope, "mock", func(llm.Config) (llm.ChatModel, error) {
		return firstByteDelayModel{ChatModel: llm.NewScripted(respWithUsage("done")), delay: delay}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Declare("main", llm.Config{Provider: "mock"}); err != nil {
		t.Fatal(err)
	}
	model, err := reg.Open("main")
	if err != nil {
		t.Fatal(err)
	}

	wait := func(what string) observability.Record {
		t.Helper()
		select {
		case r := <-recs:
			if r.Event != llm.EventGenerateFinished {
				t.Fatalf("%s: event = %q", what, r.Event)
			}
			return r
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no %s record", what, llm.EventGenerateFinished)
			return observability.Record{}
		}
	}

	if _, err := model.Generate(llm.WithEventScope(context.Background(), scope), llm.NewRequest(llm.UserText("q"))); err != nil {
		t.Fatal(err)
	}
	if got := wait("generate").Duration; got < delay {
		t.Fatalf("generate duration = %v, want >= %v", got, delay)
	}

	ch, err := model.Stream(llm.WithEventScope(context.Background(), scope), llm.NewRequest(llm.UserText("q")))
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if got := wait("stream").Duration; got < delay {
		t.Fatalf("stream duration = %v, want >= %v（锚点须在建流之前，首字节前的往返才算得进去）", got, delay)
	}
}
