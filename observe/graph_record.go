package observe

import (
	"sync"
	"time"

	"github.com/Luo-root/pulse"
)

// 图观测记录事件名（<组件>.<事实> 点分约定）。
//
// 归属规则：**谁生产记录谁拥有事件名**——这些记录由本包折叠产出，
// 故归本包。相对地，attrs key 的契约由**事实归属包**定义
// （pulse.AttrGraph / pulse.AttrNode），本包只消费不定义。
const (
	// EventNodeWaitFinished 是节点等待完成（进入执行或以 skip/失败
	// 终结）的观测记录事件名。
	EventNodeWaitFinished = "pulse.node_wait_finished"
	// EventNodeRunFinished 是节点执行完成的观测记录事件名。
	EventNodeRunFinished = "pulse.node_run_finished"
	// EventNodeFinished 是只收到 Finished 的失败或取消终态记录事件名。
	// 未观测到等待或执行段，Duration 为 0，不编造分段耗时。
	EventNodeFinished = "pulse.node_finished"
	// EventGraphStarted 是「这一轮开始了」的运行级记录事件名：引擎在提交
	// 任何节点之前发出，所以它排在本轮全部节点记录之前。
	EventGraphStarted = "pulse.graph_started"
	// EventGraphFinished 是「这一轮结束了」的运行级记录事件名：Duration 为
	// 整轮耗时（两回调之间），Status 为运行终态（completed / failed / canceled）。
	EventGraphFinished = "pulse.graph_finished"
	// EventAspect 承接 Core 的可选切面事实，阶段/边界/尝试都经 pulse.Attr* 进入 Attrs。
	EventAspect = "pulse.aspect"
)

// NewRecordObserver 返回写 Record 的观察者：**一次运行两条运行级记录**
// （started / finished）；观测到等待与执行的节点各有两条分段计时记录
// （等待完成与执行完成各一条，Duration 分别为等待段/执行段耗时）。
// 同时实现可选 pulse.AspectObserver：Retry / Timeout / NamedAspect 的真实控制点
// 直接转为 EventAspect，身份与阶段经 Attrs 承接，不复制节点生命周期或推造历史。
//
// 归因走 Attrs：节点记录带 pulse.AttrGraph + pulse.AttrNode，运行级记录只带
// pulse.AttrGraph（节点维度对它没有意义）——key 契约由**事实归属包** pulse
// 定义，本包只消费。此外每条记录都带 `pulse.AttrExecution`（**这一次运行**的身份）与
// `pulse.AttrPath`（挂载位置，根图为空、不写）：两者都从引擎随回调发出来的
// `pulse.Site` 里拿，所以**同一张图跑两次的记录自动分得开**——只按 graph + path 记账
// 会把并发两次运行折叠成一份。`cfg.Path` 只在 site 给不出位置时兜底（旧装配的用法）。
// Status 为
// running（等待段 / 运行级 started）或终态
// （finish reason）。等待后跳过而未运行的节点只有一条 skipped 等待记录；
// 已运行后返回 NoValue 的节点则以 skipped 执行记录收尾。正常跳过的 Err 为 nil。
// 只收到 Finished 的 failed / canceled 节点补一条 EventNodeFinished，Duration 为 0；
// completed / skipped 仍不产节点记录，包括恢复节点和成功或跳过的切面短路。
//
// 记录顺序即引擎事件的顺序：运行级 started 排在本轮全部节点记录之前，
// finished 排在它们之后。
//
// 身份由 pulse 随回调发出（定义 id + 本次运行 id + 挂载位置）——多图复用同一
// Observer 实现时归因不漂移。
//
// 单实例可复用于多图并发：内部按（**运行 id**, nodeID）与运行 id 记账，同名
// 节点跨图、同一张图跨运行都互不串扰。同运行同节点的残留条目（Finished 未达，如
// 进程退出）会影响该键的下一次记账——运行 id 每个执行实例都不同，所以那一次运行
// 之后键不会再被复用。长期复用建议按运行周期换实例。
// 挂载：pulse.WithObserver(NewRecordObserver(cfg))；与宿主自有 Observer
// 经 pulse.MultiObserver 组合。观察者 panic / error 不升格为节点失败
// （pulse 侧 notify 已吞掉，只读 seam 契约）。cfg.Sink 为 nil 返回哨兵错误。
func NewRecordObserver(cfg ObserveConfig) (pulse.Observer, error) {
	if cfg.Sink == nil {
		return nil, ErrNilSink
	}
	type nodeKey struct{ exec, node string }
	type nodeState struct {
		waitStart time.Time
		runStart  time.Time
		ran       bool
		waitDone  bool
	}
	var mu sync.Mutex
	states := make(map[nodeKey]*nodeState)
	execStart := make(map[string]time.Time)

	newRec := func(event, status string, d time.Duration, err error) Record {
		return Record{
			Time:     time.Now(), // 在入队前记录事件时间，不让异步叶子出口补成投递时间。
			HostID:   cfg.HostID,
			TraceID:  cfg.TraceID,
			Source:   SourceObserver,
			Event:    event,
			Status:   status,
			Duration: d,
			Err:      err,
		}
	}

	// withSite 补上位置归因：定义 id / 这次运行 id / 挂载位置。三个维度挨着写，
	// 读起来才是「同一组的三个维度」。
	//
	// 位置**空则不写**：根图的记录本来就没有层级，写空串会让「根」与「忘了传」
	// 长得一样（`Attrs` 是有才有）。`cfg.Path` 只在 site 没给位置时兜底——
	// 引擎现在把挂载位置随每条回调发出来了，宿主不必再按层建出口。
	withSite := func(rec *Record, site pulse.Site) {
		if site.Execution != "" {
			Set(&rec.Attrs, pulse.AttrExecution, site.Execution)
		}
		switch {
		case site.Path != "":
			Set(&rec.Attrs, pulse.AttrPath, site.Path)
		case cfg.Path != "":
			Set(&rec.Attrs, pulse.AttrPath, cfg.Path)
		}
	}

	seg := func(site pulse.Site, nodeID, event, status string, d time.Duration, err error) {
		rec := newRec(event, status, d, err)
		Set(&rec.Attrs, pulse.AttrGraph, site.Graph)
		Set(&rec.Attrs, pulse.AttrNode, nodeID)
		withSite(&rec, site)
		cfg.Sink.Write(rec)
	}

	// 运行级记录只带图 ID：节点维度对它没有意义。
	graphSeg := func(site pulse.Site, event, status string, d time.Duration, err error) {
		rec := newRec(event, status, d, err)
		Set(&rec.Attrs, pulse.AttrGraph, site.Graph)
		withSite(&rec, site)
		cfg.Sink.Write(rec)
	}

	return pulse.ObserverFunc{
		Aspect: func(event pulse.AspectEvent) {
			status := "running"
			if event.Reason != "" {
				status = string(event.Reason)
			} else if event.Phase == pulse.AspectDeadline {
				status = "expired" // 到期事实，不声称内层已经退出。
			}
			rec := newRec(EventAspect, status, event.Duration, event.Err)
			rec.Time = event.Time
			Set(&rec.Attrs, pulse.AttrGraph, event.Site.Graph)
			Set(&rec.Attrs, pulse.AttrNode, event.NodeID)
			withSite(&rec, event.Site)
			Set(&rec.Attrs, pulse.AttrAspect, string(event.Kind))
			Set(&rec.Attrs, pulse.AttrAspectPhase, string(event.Phase))
			Set(&rec.Attrs, pulse.AttrBoundary, int64(event.Boundary))
			if event.Name != "" {
				Set(&rec.Attrs, pulse.AttrAspectName, event.Name)
			}
			if event.ParentBoundary != 0 {
				Set(&rec.Attrs, pulse.AttrParentBoundary, int64(event.ParentBoundary))
			}
			if event.ParentAttempt != 0 {
				Set(&rec.Attrs, pulse.AttrParentAttempt, int64(event.ParentAttempt))
			}
			if event.Limit != 0 {
				Set(&rec.Attrs, pulse.AttrAttemptLimit, int64(event.Limit))
			}
			if event.Attempt != 0 {
				Set(&rec.Attrs, pulse.AttrAttempt, int64(event.Attempt))
			}
			if event.Origin != "" {
				Set(&rec.Attrs, pulse.AttrAttemptOrigin, string(event.Origin))
			}
			if event.Scope != "" {
				Set(&rec.Attrs, pulse.AttrAttemptScope, event.Scope)
			}
			if event.ParentScope != "" {
				Set(&rec.Attrs, pulse.AttrParentScope, event.ParentScope)
			}
			if event.Phase == pulse.AspectBackoffStarted || event.Phase == pulse.AspectBackoffFinished {
				Set(&rec.Attrs, pulse.AttrRetryDelay, int64(event.Delay))
			}
			if event.Kind == pulse.AspectTimeout {
				Set(&rec.Attrs, pulse.AttrTimeout, int64(event.Timeout))
			}
			cfg.Sink.Write(rec) // 无状态锁；Sink panic 由 Core 只读边界隔离。
		},
		GraphStarted: func(site pulse.Site) {
			mu.Lock()
			execStart[site.Execution] = time.Now()
			mu.Unlock()
			graphSeg(site, EventGraphStarted, "running", 0, nil)
		},
		GraphFinished: func(site pulse.Site, reason pulse.NodeFinishReason, err error) {
			mu.Lock()
			start, ok := execStart[site.Execution]
			delete(execStart, site.Execution)
			mu.Unlock()
			// 没见过 Started：不编造整轮耗时，**留 0**——它是「不知道」，不是
			// 「耗时为零」。不留 `time.Since(time.Now())` 那种几十纳秒的残值：
			// 那会把「没量到」渲染成一个看着像真数字的值（实测 Linux 上 90ns）。
			var d time.Duration
			if ok {
				d = time.Since(start)
			}
			graphSeg(site, EventGraphFinished, string(reason), d, err)
		},
		Waiting: func(site pulse.Site, nodeID string) {
			mu.Lock()
			states[nodeKey{site.Execution, nodeID}] = &nodeState{waitStart: time.Now()}
			mu.Unlock()
		},
		Running: func(site pulse.Site, nodeID string) {
			mu.Lock()
			k := nodeKey{site.Execution, nodeID}
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
			seg(site, nodeID, EventNodeWaitFinished, "running", d, nil)
		},
		Finished: func(site pulse.Site, nodeID string, reason pulse.NodeFinishReason, err error) {
			// 终态原因由引擎判定：正常跳过不是失败。不能按 ErrSkipped 过滤，
			// 因为可靠失败的错误链也可能包含它。
			if reason == pulse.NodeSkipped {
				err = nil
			}
			mu.Lock()
			k := nodeKey{site.Execution, nodeID}
			st := states[k]
			delete(states, k)
			mu.Unlock()
			if st == nil {
				if reason == pulse.NodeFailed || reason == pulse.NodeCanceled {
					seg(site, nodeID, EventNodeFinished, string(reason), 0, err)
				}
				return
			}
			// 未进入执行（skip / 直接失败）：只补等待段。
			if !st.waitDone {
				seg(site, nodeID, EventNodeWaitFinished, string(reason), time.Since(st.waitStart), err)
				return
			}
			if st.ran {
				seg(site, nodeID, EventNodeRunFinished, string(reason), time.Since(st.runStart), err)
			}
		},
	}, nil
}
