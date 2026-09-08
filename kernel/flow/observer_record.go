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
// graphID 与 nodeID 进 Attrs（AttrGraph / AttrNode 契约），不占用
// Record 的装配专用具名字段；Status 为 running（等待段）或 finish
// reason（执行段）。跳过节点只有一条 skipped 等待记录，无运行记录。
//
// graphID 由图随回调发出（New 的 graphID）——多图复用同一 Observer
// 实现时归因不漂移。
//
// 单实例可复用于多图并发：内部按（graphID, nodeID）记账，同名节点
// 跨图互不串扰。同图同节点的残留条目（Finished 未达，如进程退出）
// 会影响该键的下一次记账，长期复用建议按图运行周期换实例。
// 挂载：WithObserver(NewRecordObserver(cfg))；与宿主自有 Observer 经
// MultiObserver 组合。观察者 panic / error 不升格为节点失败（notify
// 已吞，只读 seam 契约）。cfg.Sink 为 nil 返回哨兵错误。
func NewRecordObserver(cfg observability.ObserveConfig) (Observer, error) {
	if cfg.Sink == nil {
		return nil, observability.ErrNilSink
	}
	type nodeKey struct{ graph, node string }
	type nodeState struct {
		waitStart time.Time
		runStart  time.Time
		ran       bool
		waitDone  bool
	}
	var mu sync.Mutex
	states := make(map[nodeKey]*nodeState)

	seg := func(graphID, nodeID, event, status string, d time.Duration, err error) {
		rec := observability.Record{
			HostID:   cfg.HostID,
			TraceID:  cfg.TraceID,
			Source:   observability.SourceAdapter,
			Event:    event,
			Status:   status,
			Duration: d,
			Err:      err,
		}
		observability.Set(&rec.Attrs, AttrGraph, graphID)
		observability.Set(&rec.Attrs, AttrNode, nodeID)
		cfg.Sink.Write(rec)
	}

	return ObserverFunc{
		Waiting: func(graphID, nodeID string) {
			mu.Lock()
			states[nodeKey{graphID, nodeID}] = &nodeState{waitStart: time.Now()}
			mu.Unlock()
		},
		Running: func(graphID, nodeID string) {
			mu.Lock()
			k := nodeKey{graphID, nodeID}
			st := states[k]
			if st == nil {
				st = &nodeState{waitStart: time.Now()}
				states[k] = st
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
			seg(graphID, nodeID, EventNodeWaitFinished, "running", d, nil)
		},
		Finished: func(graphID, nodeID string, reason NodeFinishReason, err error) {
			mu.Lock()
			k := nodeKey{graphID, nodeID}
			st := states[k]
			delete(states, k)
			mu.Unlock()
			if st == nil {
				return
			}
			// 未进入执行（skip / 直接失败）：只补等待段。
			if !st.waitDone {
				seg(graphID, nodeID, EventNodeWaitFinished, string(reason), time.Since(st.waitStart), err)
				return
			}
			if st.ran {
				seg(graphID, nodeID, EventNodeRunFinished, string(reason), time.Since(st.runStart), err)
			}
		},
	}, nil
}
