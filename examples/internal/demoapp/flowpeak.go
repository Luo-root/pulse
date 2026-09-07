package demoapp

import (
	"sync/atomic"

	"github.com/Luo-root/pulse/kernel/flow"
)

// FlowPeak 记录 flow observer 观测到的并发存活峰值（demo 业务统计）。
// alive = 当前仍在 Waiting..Finished 之间的节点数；peak = 历史最大值。
//
// 节点分段计时（wait/run 记录）由官方 observability/bridge 的
// FlowObserver 负责；本类型只做峰值统计，二者经 flow.MultiObserver
// 组合挂图（见 examples/04-flow）。
type FlowPeak struct {
	alive atomic.Int32
	peak  atomic.Int32
}

// Peak 返回历史并发存活峰值。
func (p *FlowPeak) Peak() int32 { return p.peak.Load() }

// Observer 返回只做峰值统计的 flow.Observer：Waiting 进、Finished 出，
// 与官方桥的 FlowObserver 组合使用。
func (p *FlowPeak) Observer() flow.Observer {
	return flow.ObserverFunc{
		Waiting:  func(string) { p.enter() },
		Finished: func(string, flow.NodeFinishReason, error) { p.leave() },
	}
}

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
