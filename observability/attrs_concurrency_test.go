package observability

import (
	"sync"
	"testing"
)

// MultiSink 把同一个带 Attrs 的 Record 扇出给多个 Sink，多 goroutine
// 并发 Write + 并发 Snapshot：验证引用语义契约（产出方 Write 后不再
// 修改、Sink 只读消费）在 -race 下无竞争（#125 审核修订 3 的验收锚）。
func TestMultiSinkConcurrentAttrsFanout(t *testing.T) {
	const sinks, writers = 4, 8
	var sinksList []Sink
	mems := make([]*MemorySink, sinks)
	for i := range mems {
		mems[i] = &MemorySink{}
		sinksList = append(sinksList, mems[i])
	}
	multi := MultiSink(sinksList)

	rec := Record{HostID: "h", TraceID: "tr-fanout", Source: SourceAdapter, Event: "llm.after_response"}
	Set(&rec.Attrs, "llm.model", "m1")
	Set(&rec.Attrs, "llm.tokens_in", int64(10))

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 同一条 Record（同一 Attrs map）被并发扇出：只读竞争。
			for i := 0; i < 25; i++ {
				multi.Write(rec)
			}
		}()
	}
	// 并发读者与写入者同时跑：模拟 Sink 侧的异步消费视角。
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
				for _, m := range mems {
					for _, r := range m.Snapshot() {
						_, _ = Get[string](r.Attrs, "llm.model")
					}
				}
			}
		}
	}()

	wg.Wait()
	close(stop)
	<-readerDone

	for _, m := range mems {
		if got := m.Len(); got != writers*25 {
			t.Fatalf("sink collected %d records, want %d", got, writers*25)
		}
	}
}
