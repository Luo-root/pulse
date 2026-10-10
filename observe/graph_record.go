package observe

import (
	"sync"
	"time"

	"github.com/Luo-root/pulse"
)

// 图观测记录事件名（<组件>.<事实> 点分约定）。
//
// 归属规则：**谁生产记录谁拥有事件名**——这两条记录由本包折叠产出，
// 故归本包。相对地，attrs key 的契约由**事实归属包**定义
// （pulse.AttrGraph / pulse.AttrNode），本包只消费不定义。
const (
	// EventNodeWaitFinished 是节点等待完成（进入执行或以 skip/失败
	// 终结）的观测记录事件名。
	EventNodeWaitFinished = "pulse.node_wait_finished"
	// EventNodeRunFinished 是节点执行完成的观测记录事件名。
	EventNodeRunFinished = "pulse.node_run_finished"
	// EventGraphStarted 是「这一轮开始了」的运行级记录事件名：引擎在提交
	// 任何节点之前发出，所以它排在本轮全部节点记录之前。
	EventGraphStarted = "pulse.graph_started"
	// EventGraphFinished 是「这一轮结束了」的运行级记录事件名：Duration 为
	// 整轮耗时（两回调之间），Status 为运行终态（completed / failed / canceled）。
	EventGraphFinished = "pulse.graph_finished"
)

// NewRecordObserver 返回写 Record 的观察者：**一次运行两条运行级记录**
// （started / finished）+ **每个节点两条分段计时记录**（等待完成与执行完成
// 各一条，Duration 分别为等待段/执行段耗时）。
//
// 归因走 Attrs：节点记录带 pulse.AttrGraph + pulse.AttrNode，运行级记录只带
// pulse.AttrGraph（节点维度对它没有意义）——key 契约由**事实归属包** pulse
// 定义，本包只消费。`cfg.Path` 非空时**每条**记录再带一个 pulse.AttrPath
// （嵌套层级，引擎给值；空则不写，见 ObserveConfig.Path）。Status 为
// running（等待段 / 运行级 started）或终态
// （finish reason）。跳过节点只有一条 skipped 等待记录，无运行记录。
//
// 记录顺序即引擎事件的顺序：运行级 started 排在本轮全部节点记录之前，
// finished 排在它们之后。
//
// graphID 由 pulse 随回调发出（Graph 构造时的 graphID）——多图复用同一
// Observer 实现时归因不漂移。
//
// 单实例可复用于多图并发：内部按（graphID, nodeID）与 graphID 记账，同名
// 节点跨图互不串扰。同图同节点 / 同 graphID 的残留条目（Finished 未达，如
// 进程退出）会影响该键的下一次记账；**同 graphID 的两轮运行并发复用同一
// 实例**时，整轮耗时会按后一次 Started 起算（拿不到 Started 时 Duration 为
// 恰好 0，记录照发）。长期复用建议按图运行周期换实例。
// 挂载：pulse.WithObserver(NewRecordObserver(cfg))；与宿主自有 Observer
// 经 pulse.MultiObserver 组合。观察者 panic / error 不升格为节点失败
// （pulse 侧 notify 已吞掉，只读 seam 契约）。cfg.Sink 为 nil 返回哨兵错误。
func NewRecordObserver(cfg ObserveConfig) (pulse.Observer, error) {
	if cfg.Sink == nil {
		return nil, ErrNilSink
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
	graphStart := make(map[string]time.Time)

	newRec := func(event, status string, d time.Duration, err error) Record {
		return Record{
			HostID:   cfg.HostID,
			TraceID:  cfg.TraceID,
			Source:   SourceObserver,
			Event:    event,
			Status:   status,
			Duration: d,
			Err:      err,
		}
	}

	// withPath 补上嵌套层级归因（#294）：三段归因挨着写，读起来才是「同一组的
	// 三个维度」。`cfg.Path` 为空**不写**这个 key——根图的记录本来就没有层级，
	// 写空串会让「根」与「忘了传」长得一样（`Attrs` 是有才有）。
	withPath := func(rec *Record) {
		if cfg.Path != "" {
			Set(&rec.Attrs, pulse.AttrPath, cfg.Path)
		}
	}

	seg := func(graphID, nodeID, event, status string, d time.Duration, err error) {
		rec := newRec(event, status, d, err)
		Set(&rec.Attrs, pulse.AttrGraph, graphID)
		Set(&rec.Attrs, pulse.AttrNode, nodeID)
		withPath(&rec)
		cfg.Sink.Write(rec)
	}

	// 运行级记录只带图 ID：节点维度对它没有意义。
	graphSeg := func(graphID, event, status string, d time.Duration, err error) {
		rec := newRec(event, status, d, err)
		Set(&rec.Attrs, pulse.AttrGraph, graphID)
		withPath(&rec)
		cfg.Sink.Write(rec)
	}

	return pulse.ObserverFunc{
		GraphStarted: func(graphID string) {
			mu.Lock()
			graphStart[graphID] = time.Now()
			mu.Unlock()
			graphSeg(graphID, EventGraphStarted, "running", 0, nil)
		},
		GraphFinished: func(graphID string, reason pulse.NodeFinishReason, err error) {
			mu.Lock()
			start, ok := graphStart[graphID]
			delete(graphStart, graphID)
			mu.Unlock()
			// 没见过 Started：不编造整轮耗时，**留 0**——它是「不知道」，不是
			// 「耗时为零」。不留 `time.Since(time.Now())` 那种几十纳秒的残值：
			// 那会把「没量到」渲染成一个看着像真数字的值（实测 Linux 上 90ns）。
			var d time.Duration
			if ok {
				d = time.Since(start)
			}
			graphSeg(graphID, EventGraphFinished, string(reason), d, err)
		},
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
		Finished: func(graphID, nodeID string, reason pulse.NodeFinishReason, err error) {
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
