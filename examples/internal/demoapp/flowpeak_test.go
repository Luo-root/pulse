package demoapp

import (
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/observability"
)

// FlowPeak 必须记住历史最大值：切面 defer 回落 alive 后 Peak 仍保持。
func TestFlowPeakRemembersHistoricalMax(t *testing.T) {
	peak := &FlowPeak{}
	bridge := &Bridge{Sink: &observability.MemorySink{}, HostID: "h", TraceID: "t"}
	aspect := bridge.FlowAspect(peak)

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	runOne := func() {
		defer wg.Done()
		_ = aspect.Around(nil, func(*flow.RunCtx) error {
			started <- struct{}{}
			<-release
			return nil
		})
	}
	wg.Add(2)
	go runOne()
	go runOne()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("nodes did not enter aspect")
		}
	}
	if got := peak.Peak(); got < 2 {
		t.Fatalf("alive peak while both running = %d, want >= 2", got)
	}
	close(release)
	wg.Wait()
	if got := peak.Peak(); got < 2 {
		t.Fatalf("historical peak collapsed to %d after nodes finished", got)
	}
}
