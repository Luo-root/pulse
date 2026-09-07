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
