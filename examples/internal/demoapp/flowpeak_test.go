package demoapp

import (
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/observability"
)

// FlowPeak 必须记住历史最大值：Waiting..Finished 之间 defer leave 后 Peak 仍保持。
func TestFlowPeakRemembersHistoricalMax(t *testing.T) {
	peak := &FlowPeak{}
	bridge := &Bridge{Sink: &observability.MemorySink{}, HostID: "h", TraceID: "t"}
	obs := bridge.FlowObserver(peak)

	var wg sync.WaitGroup
	runOne := func(id string) {
		defer wg.Done()
		obs.OnNodeWaiting(id)
		time.Sleep(20 * time.Millisecond)
		obs.OnNodeRunning(id)
		time.Sleep(20 * time.Millisecond)
		obs.OnNodeFinished(id, flow.NodeCompleted, nil)
	}
	wg.Add(2)
	go runOne("a")
	go runOne("b")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if peak.Peak() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := peak.Peak(); got < 2 {
		t.Fatalf("alive peak while both running = %d, want >= 2", got)
	}
	wg.Wait()
	if got := peak.Peak(); got < 2 {
		t.Fatalf("historical peak collapsed to %d after nodes finished", got)
	}
}
