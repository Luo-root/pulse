package bridge

import (
	"sync"
	"time"

	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/observability"
)

// FlowObserver 返回 flow.Observer 适配器：把节点生命周期折成分段计时
// 记录——等待完成（EventNodeWaitFinished）与执行完成
// （EventNodeRunFinished）各一条，Duration 分别为等待段/执行段耗时。
//
// nodeID 进 Attrs（flow.AttrNode 契约），不占用 Record 的装配专用
// 字段。跳过节点只有 Finished（skipped）一条等待记录；图级 panic /
// error 不升格为节点失败（flow.Observer 契约，notify 已吞）。
//
// 挂载：flow 图构建时 WithObserver(b.FlowObserver())；同一 Bridge 的
// 适配器可安全并发（内部按 nodeID 记账）。
func (b *Bridge) FlowObserver() flow.Observer {
	type nodeState struct {
		waitStart time.Time
		runStart  time.Time
		ran       bool
		waitDone  bool
	}
	var mu sync.Mutex
	states := make(map[string]*nodeState)

	seg := func(nodeID, event, status string, d time.Duration, err error) {
		b.write(event, status, d, err, func(a *observability.Attrs) {
			observability.Set(a, flow.AttrNode, nodeID)
		})
	}

	return flow.ObserverFunc{
		Waiting: func(nodeID string) {
			mu.Lock()
			states[nodeID] = &nodeState{waitStart: time.Now()}
			mu.Unlock()
		},
		Running: func(nodeID string) {
			mu.Lock()
			st := states[nodeID]
			if st == nil {
				st = &nodeState{waitStart: time.Now()}
				states[nodeID] = st
			}
			if st.waitDone {
				mu.Unlock()
				return
			}
			d := time.Since(st.waitStart)
			st.waitDone = true
			st.ran = true
			st.runStart = time.Now()
			mu.Unlock()
			seg(nodeID, EventNodeWaitFinished, "running", d, nil)
		},
		Finished: func(nodeID string, reason flow.NodeFinishReason, err error) {
			mu.Lock()
			st := states[nodeID]
			delete(states, nodeID)
			mu.Unlock()
			if st == nil {
				return
			}
			// 未进入执行（skip / 直接失败）：只补等待段。
			if !st.waitDone {
				seg(nodeID, EventNodeWaitFinished, string(reason), time.Since(st.waitStart), err)
				return
			}
			if st.ran {
				seg(nodeID, EventNodeRunFinished, string(reason), time.Since(st.runStart), err)
			}
		},
	}
}
