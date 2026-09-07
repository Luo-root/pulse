package flow

import (
	"sync"
	"time"

	"github.com/Luo-root/pulse/observability"
)

// 观测记录事件名（<组件>.<事实> 点分约定）。
const (
	// EventNodeWaitFinished 是节点等待完成（进入执行或以 skip/失败
	// 终结）的观测记录事件名。
	EventNodeWaitFinished = "flow.node_wait_finished"
	// EventNodeRunFinished 是节点执行完成的观测记录事件名。
	EventNodeRunFinished = "flow.node_run_finished"
)

// NewRecordObserver 返回写 observability.Record 的节点分段计时观察者：
// 等待完成与执行完成各一条记录（Duration 分别为等待段/执行段耗时），
// nodeID 进 Attrs（AttrNode 契约），不占用 Record 的装配专用具名字段；
// Status 为 running（等待段）或 finish reason（执行段）。跳过节点只有
// 一条 skipped 等待记录，无运行记录。
//
// 单次图运行使用一个适配器实例：内部按 nodeID 记账，节点异常路径
// （Finished 未达，如图被取消）的残留条目随实例丢弃，实例复用会残留。
// 挂载：WithObserver(NewRecordObserver(cfg))；与宿主自有 Observer 经
// MultiObserver 组合。观察者 panic / error 不升格为节点失败（notify
// 已吞，只读 seam 契约）。cfg.Sink 为 nil 返回哨兵错误。
func NewRecordObserver(cfg observability.ObserveConfig) (Observer, error) {
	if cfg.Sink == nil {
		return nil, observability.ErrNilSink
	}
	type nodeState struct {
		waitStart time.Time
		runStart  time.Time
		ran       bool
		waitDone  bool
	}
	var mu sync.Mutex
	states := make(map[string]*nodeState)

	seg := func(nodeID, event, status string, d time.Duration, err error) {
		rec := observability.Record{
			HostID:   cfg.HostID,
			TraceID:  cfg.TraceID,
			Source:   observability.SourceAdapter,
			Event:    event,
			Status:   status,
			Duration: d,
			Err:      err,
		}
		observability.Set(&rec.Attrs, AttrNode, nodeID)
		cfg.Sink.Write(rec)
	}

	return ObserverFunc{
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
		Finished: func(nodeID string, reason NodeFinishReason, err error) {
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
	}, nil
}
