package observability

import (
	"testing"

	"github.com/Luo-root/pulse/kernel"
)

// Collector 服务化直写：注册 → kernel.Get → Write/WriteAttrs 自动携带
// HostID/TraceID/Source；scope 销毁后服务撤除。
func TestAttachCollectorService(t *testing.T) {
	scope := kernel.New()
	sink := &MemorySink{}
	c, err := AttachCollector(scope, ObserveConfig{Sink: sink, HostID: "h1", TraceID: "tr-1"})
	if err != nil {
		t.Fatal(err)
	}

	got, ok := kernel.Get(scope, CollectorKey)
	if !ok || got != c {
		t.Fatalf("collector service missing or mismatched: ok=%v", ok)
	}

	c.Write("app.evt", "done")
	c.WriteAttrs("app.custom", "ok", func(a *Attrs) {
		Set(a, "app.k", "v")
	})

	recs := sink.Snapshot()
	if len(recs) != 2 {
		t.Fatalf("sink has %d records, want 2", len(recs))
	}
	for _, rec := range recs {
		if rec.HostID != "h1" || rec.TraceID != "tr-1" || rec.Source != SourceAdapter {
			t.Fatalf("envelope mismatch: %+v", rec)
		}
		if rec.Event == "app.evt" && rec.Status != "done" {
			t.Fatalf("write record: %+v", rec)
		}
	}
	if recs[0].Event != "app.evt" || recs[0].Status != "done" {
		t.Fatalf("write record: %+v", recs[0])
	}
	if recs[1].Event != "app.custom" || recs[1].Status != "ok" {
		t.Fatalf("writeattrs record: %+v", recs[1])
	}
	if v, _ := Get[string](recs[1].Attrs, "app.k"); v != "v" {
		t.Fatalf("attr = %q", v)
	}

	// scope 销毁 → 服务撤除（kernel.Provide 的作用域归属语义）。
	scope.Dispose()
	if _, ok := kernel.Get(scope, CollectorKey); ok {
		t.Fatal("collector service must be withdrawn after scope dispose")
	}
}

// 装配校验：nil scope / nil Sink 哨兵。
func TestAttachCollectorValidation(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	if _, err := AttachCollector(nil, ObserveConfig{Sink: &MemorySink{}}); err != ErrNilScope {
		t.Fatalf("nil scope err = %v", err)
	}
	if _, err := AttachCollector(scope, ObserveConfig{TraceID: "t"}); err != ErrNilSink {
		t.Fatalf("nil sink err = %v", err)
	}
}

// 同一 scope 重复 Attach：Provide 覆盖语义，以最后一次为准。
func TestAttachCollectorOverwrite(t *testing.T) {
	scope := kernel.New()
	t.Cleanup(scope.Dispose)
	sink := &MemorySink{}
	if _, err := AttachCollector(scope, ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-1"}); err != nil {
		t.Fatal(err)
	}
	c2, err := AttachCollector(scope, ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-2"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := kernel.Get(scope, CollectorKey)
	if !ok || got != c2 {
		t.Fatal("second attach must win (Provide overwrite semantics)")
	}
}

// 并发隔离（#170）：两个并列请求 scope 各自 Attach，互不可见；父作用域
// 也读不到——这正是「请求级直写」此前串台的修复锚点（修复前两个请求
// 互相覆盖，从任何 scope 都读到最后一个 Attach 的 Collector）。
func TestAttachCollectorIsolation(t *testing.T) {
	root := kernel.New()
	t.Cleanup(root.Dispose)
	sink := &MemorySink{}

	reqA, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	reqB, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	subA, err := reqA.Derive() // 请求内的后代作用域（如插件私有 scope）
	if err != nil {
		t.Fatal(err)
	}

	cA, err := AttachCollector(reqA, ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-A"})
	if err != nil {
		t.Fatal(err)
	}
	cB, err := AttachCollector(reqB, ObserveConfig{Sink: sink, HostID: "h", TraceID: "tr-B"})
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := kernel.Get(reqA, CollectorKey); !ok || got != cA {
		t.Fatalf("reqA read = %v, want its own collector", ok)
	}
	if got, ok := kernel.Get(reqB, CollectorKey); !ok || got != cB {
		t.Fatalf("reqB read = %v, want its own collector", ok)
	}
	if got, ok := kernel.Get(subA, CollectorKey); !ok || got != cA {
		t.Fatal("request descendant must see the request's collector")
	}
	if _, ok := kernel.Get(root, CollectorKey); ok {
		t.Fatal("parent scope must not see a request-scoped collector")
	}

	// 各写各的：TraceID 不串台。
	cA.Write("req.a", "ok")
	cB.Write("req.b", "ok")
	recs := sink.Snapshot()
	if len(recs) != 2 {
		t.Fatalf("sink has %d records, want 2", len(recs))
	}
	if recs[0].Event != "req.a" || recs[0].TraceID != "tr-A" {
		t.Fatalf("reqA record = %+v（TraceID 不得串台）", recs[0])
	}
	if recs[1].Event != "req.b" || recs[1].TraceID != "tr-B" {
		t.Fatalf("reqB record = %+v（TraceID 不得串台）", recs[1])
	}
}
