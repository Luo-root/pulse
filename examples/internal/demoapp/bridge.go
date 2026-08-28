// bridge.go 是装配层观测桥：订阅 llm/loop 的公开事件并把运行期
// 事实折进 observability.Record 写同一 Sink。
//
// 边界（docs/design/observability-v1-design.md + kernel-local-events.md）：
//   - 允许 import llm/loop/flow——本文件属于装配层；
//   - D3 两层标识：HostID 宿主稳定，TraceID 由 Host.NewTraceID 注入
//     （单一生成源，禁止桥自己另造序号）；
//   - 监听挂在请求 scope 上；配合 EmitLocal/WaterfallLocal，只有本请求听得到；
//   - 官方 Record 的装配专用字段保持零值；token 数走 slog 附加键。
package demoapp

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/observability"
)

// Bridge 聚合一次请求的运行期观测状态。
// 生命周期 = 该请求：由 Host.NewBridge 创建，监听随请求 scope 销毁摘除。
type Bridge struct {
	Sink    observability.Sink
	HostID  string
	TraceID string

	mu       sync.Mutex
	genStart time.Time
}

// install 把本请求的事件监听挂到 scope（要求非 nil）。
func (b *Bridge) install(scope *kernel.Context) error {
	if scope == nil {
		return fmt.Errorf("demo: bridge requires a request scope")
	}
	if _, err := kernel.OnWaterfall(scope, llm.EventBeforeGenerate,
		func(req *llm.GenerateRequest, next func(*llm.GenerateRequest) *llm.GenerateRequest) *llm.GenerateRequest {
			b.mu.Lock()
			b.genStart = time.Now()
			b.mu.Unlock()
			return next(req)
		}); err != nil {
		return err
	}
	if _, err := kernel.On(scope, llm.EventAfterResponse, func(resp *llm.Response) {
		b.mu.Lock()
		started := b.genStart
		b.mu.Unlock()
		b.Sink.Write(observability.Record{
			HostID:   b.HostID,
			TraceID:  b.TraceID,
			Source:   observability.SourceBridge,
			Event:    "llm.generate_finished",
			Duration: time.Since(started),
			Status:   string(resp.FinishReason),
		})
		slog.Debug("token usage",
			"trace_id", b.TraceID,
			"input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens,
			"cached_input_tokens", resp.Usage.CachedInputTokens,
		)
	}); err != nil {
		return err
	}
	if _, err := kernel.On(scope, loop.EventAfterToolCall, func(after *loop.AfterToolCall) {
		status := "completed"
		switch {
		case after.Rejected:
			status = "rejected"
		case after.Err != nil:
			status = "failed"
		}
		b.Sink.Write(observability.Record{
			HostID:   b.HostID,
			TraceID:  b.TraceID,
			Source:   observability.SourceBridge,
			Event:    "loop.tool_finished",
			Status:   status,
			Duration: after.Duration,
			Err:      after.Err,
		})
	}); err != nil {
		return err
	}
	if _, err := kernel.On(scope, loop.EventTurnEnd, func(end *loop.TurnEnd) {
		slog.Info("turn summary",
			"host_id", b.HostID,
			"trace_id", b.TraceID,
			"steps", end.Steps,
			"stopped_by", end.StoppedBy,
			"input_tokens", end.Usage.InputTokens,
			"output_tokens", end.Usage.OutputTokens,
		)
	}); err != nil {
		return err
	}
	return nil
}

// FlowPeak 记录 flow observer 观测到的并发存活峰值。
// alive = 当前仍在 Waiting..Finished 之间的节点数；peak = 历史最大值。
type FlowPeak struct {
	alive atomic.Int32
	peak  atomic.Int32
}

// Peak 返回历史并发存活峰值。
func (p *FlowPeak) Peak() int32 { return p.peak.Load() }

func (p *FlowPeak) enter() {
	if p == nil {
		return
	}
	cur := p.alive.Add(1)
	for {
		old := p.peak.Load()
		if cur <= old || p.peak.CompareAndSwap(old, cur) {
			return
		}
	}
}

func (p *FlowPeak) leave() {
	if p != nil {
		p.alive.Add(-1)
	}
}

// 桥事件名（官方 Record 不扩字段；wait/run 分两条 Duration）。
const (
	EventFlowNodeWaitFinished = "flow.node_wait_finished"
	EventFlowNodeRunFinished  = "flow.node_run_finished"
)

// FlowObserver 订阅 flow E1 生命周期，写出 wait/run 两条 Record，并更新峰值。
// 不再用 Aspect.Around 整段耗时冒充分段。
func (b *Bridge) FlowObserver(peak *FlowPeak) flow.Observer {
	type nodeState struct {
		waitStart time.Time
		runStart  time.Time
		ran       bool
		waitDone  bool
	}
	var mu sync.Mutex
	states := make(map[string]*nodeState)

	// FiberName 借官方信封的实例诊断名槽位填 nodeID，不扩 Record 字段。
	write := func(nodeID, event, status string, d time.Duration, err error) {
		b.Sink.Write(observability.Record{
			HostID:    b.HostID,
			TraceID:   b.TraceID,
			Source:    observability.SourceBridge,
			Event:     event,
			Status:    status,
			Duration:  d,
			Err:       err,
			FiberName: nodeID,
		})
	}

	return flow.ObserverFunc{
		Waiting: func(nodeID string) {
			mu.Lock()
			states[nodeID] = &nodeState{waitStart: time.Now()}
			mu.Unlock()
			peak.enter()
		},
		Running: func(nodeID string) {
			mu.Lock()
			st := states[nodeID]
			if st == nil {
				st = &nodeState{waitStart: time.Now()}
				states[nodeID] = st
			}
			if !st.waitDone {
				d := time.Since(st.waitStart)
				st.waitDone = true
				st.ran = true
				st.runStart = time.Now()
				mu.Unlock()
				write(nodeID, EventFlowNodeWaitFinished, "running", d, nil)
				return
			}
			mu.Unlock()
		},
		Finished: func(nodeID string, reason flow.NodeFinishReason, err error) {
			defer peak.leave()
			status := string(reason)
			mu.Lock()
			st := states[nodeID]
			delete(states, nodeID)
			mu.Unlock()
			if st == nil {
				return
			}
			if !st.waitDone {
				write(nodeID, EventFlowNodeWaitFinished, status, time.Since(st.waitStart), err)
				return
			}
			if st.ran {
				write(nodeID, EventFlowNodeRunFinished, status, time.Since(st.runStart), err)
			}
		},
	}
}

// Write 让桥直接把自定义事实写进同一出口。
func (b *Bridge) Write(event, status string) {
	b.Sink.Write(observability.Record{
		HostID:  b.HostID,
		TraceID: b.TraceID,
		Source:  observability.SourceBridge,
		Event:   event,
		Status:  status,
	})
}
