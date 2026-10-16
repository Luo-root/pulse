package observe

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// #398：在途 Write 固定住 worker，取消首次 Close 后检查实际环槽，
// 不靠 GC 时机证明已丢弃的 Attrs/Err 引用被释放；也覆盖 head 跨环尾的情况。
func TestAsyncSinkCanceledCloseReleasesQueuedRecords(t *testing.T) {
	for _, tc := range []struct {
		name     string
		capacity int
		warmup   int
		queued   int
	}{
		{"single", 1, 0, 1},
		{"wrapped-partial", 4, 2, 2},
		{"wrapped-full", 4, 2, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := newAsyncFlushBarrierSink()
			gate.flushGate.release()
			gate.writeGates["in-flight"] = newAsyncTestBarrier()
			as := newAsyncFlushTestSink(t, gate, gate, WithCapacity(tc.capacity))
			wantEvents := make([]string, 0, tc.warmup+1)
			for i := 0; i < tc.warmup; i++ {
				event := fmt.Sprintf("warmup-%d", i)
				as.Write(Record{Event: event})
				waitAsyncTestValue(t, gate.writeEntered)
				wantEvents = append(wantEvents, event)
			}
			if err := as.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, priorFlushes, _ := gate.snapshot()
			as.Write(Record{Event: "in-flight"})
			waitAsyncTestValue(t, gate.writeEntered)
			wantEvents = append(wantEvents, "in-flight")
			payload := strings.Repeat("x", 2<<20)
			for i := 0; i < tc.queued; i++ {
				rec := Record{Event: fmt.Sprintf("queued-%d", i), Err: errors.New("queued error")}
				Set(&rec.Attrs, "app.payload", payload)
				as.Write(rec)
			}
			flushed := runWaitingAsyncFlush(t, as, context.Background())
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := as.Close(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Close = %v, want context.Canceled", err)
			}
			// 首次终态锁存后，重复/并发 Close 与 Flush 都应及时取得同一结果。
			var repeated []<-chan error
			for i := 0; i < 4; i++ {
				repeated = append(repeated, runAsyncTestCall(func() error { return as.Close(context.Background()) }))
			}
			repeated = append(repeated, runAsyncTestCall(func() error { return as.Flush(context.Background()) }))
			for _, done := range repeated {
				if err := waitAsyncTestValue(t, done); !errors.Is(err, context.Canceled) {
					t.Fatalf("repeated close result = %v, want context.Canceled", err)
				}
			}
			gate.writeGates["in-flight"].release()
			waitAsyncTestValue(t, as.workerDone)
			if err := waitAsyncTestValue(t, flushed); !errors.Is(err, context.Canceled) {
				t.Fatalf("pending Flush = %v, want first Close cancellation", err)
			}
			if got := as.Dropped(); got != uint64(tc.queued) {
				t.Errorf("Dropped = %d, want %d", got, tc.queued)
			}
			events, calls, overlap := gate.snapshot()
			if !reflect.DeepEqual(events, wantEvents) || calls != priorFlushes || overlap {
				t.Errorf("events=%v flushes=%d overlap=%v, want %v, %d, false", events, calls, overlap, wantEvents, priorFlushes)
			}
			as.mu.Lock()
			defer as.mu.Unlock()
			retainedAttrs, retainedErrors, retainedBytes := 0, 0, 0
			for _, rec := range as.ring {
				if rec.Attrs.Len() != 0 {
					retainedAttrs++
				}
				if rec.Err != nil {
					retainedErrors++
				}
				if value, ok := Get[string](rec.Attrs, "app.payload"); ok {
					retainedBytes += len(value)
				}
			}
			if as.count != 0 || as.head != 0 || retainedAttrs != 0 || retainedErrors != 0 {
				t.Errorf("worker exited: count=%d head=%d retained_attrs=%d retained_errors=%d retained_payload_bytes=%d; want no queued references", as.count, as.head, retainedAttrs, retainedErrors, retainedBytes)
			}
		})
	}
}

// 反向用例：取消 Flush 不等于取消 Close，不能提前释放或丢弃尚待送达的队列。
func TestAsyncSinkCanceledFlushRetainsQueuedPayload(t *testing.T) {
	gate := newAsyncFlushBarrierSink()
	gate.writeGates["in-flight"] = newAsyncTestBarrier()
	as := newAsyncFlushTestSink(t, gate, gate, WithCapacity(1))
	as.Write(Record{Event: "in-flight"})
	waitAsyncTestValue(t, gate.writeEntered)
	rec := Record{Event: "queued", Err: errors.New("retained error")}
	Set(&rec.Attrs, "app.payload", "keep until delivered")
	as.Write(rec)
	ctx, cancel := context.WithCancel(context.Background())
	done := runWaitingAsyncFlush(t, as, ctx)
	cancel()
	if err := waitAsyncTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Flush = %v, want context.Canceled", err)
	}
	as.mu.Lock()
	queued := as.ring[as.head]
	count := as.count
	as.mu.Unlock()
	if value, ok := Get[string](queued.Attrs, "app.payload"); !ok || value != "keep until delivered" || queued.Err != rec.Err || count != 1 {
		t.Fatal("canceled Flush released or changed a queued record")
	}
	gate.writeGates["in-flight"].release()
	gate.flushGate.release()
	if err := as.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitAsyncTestValue(t, as.workerDone)
	if events, _, overlap := gate.snapshot(); !reflect.DeepEqual(events, []string{"in-flight", "queued"}) || overlap || as.Dropped() != 0 {
		t.Fatalf("events=%v overlap=%v Dropped=%d; want both records, false, 0", events, overlap, as.Dropped())
	}
}
