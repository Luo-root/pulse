package pulse

import (
	"context"
	"time"
)

// AspectObserver 是可选的切面事实出口。通过 WithObserver / WithExecObserver 安装的
// Observer 实现它即可接收；不增加原五回调的次数。与 Observer 一样须并发安全、快速返回，
// 不得在回调里等待本次执行结束；panic 被隔离。回调在引擎状态锁外执行。
type AspectObserver interface {
	OnAspectEvent(AspectEvent)
}

// AspectKind 区分内置控制事实与显式登记的自定义切面。
type AspectKind string

const (
	AspectRetry   AspectKind = "retry"
	AspectTimeout AspectKind = "timeout"
	AspectCustom  AspectKind = "custom"
)

// AspectPhase 是实际控制点，不从节点最终结果反推内部历史。
type AspectPhase string

const (
	AspectStarted         AspectPhase = "started"
	AspectFinished        AspectPhase = "finished"
	AspectAttemptStarted  AspectPhase = "attempt_started"
	AspectAttemptFinished AspectPhase = "attempt_finished"
	// AspectRetrying 只在实际分配下一次尝试后报告；首次尝试或认回活跃尝试不发。
	AspectRetrying        AspectPhase = "retry"
	AspectBackoffStarted  AspectPhase = "backoff_started"
	AspectBackoffFinished AspectPhase = "backoff_finished"
	// AspectDeadline 表示本层 Timeout 的计时器确实到期，不表示内层已经退出。
	AspectDeadline AspectPhase = "deadline"
)

// AttemptOrigin 是本次 OpenAttempt 的事实；unknown 表示旧桥没有提供标记。
// 编号大于 1 不等于本进程刚分配了重试，不能据此把 resumed 当成 new。
type AttemptOrigin string

const (
	AttemptNew     AttemptOrigin = "new"
	AttemptResumed AttemptOrigin = "resumed"
	AttemptUnknown AttemptOrigin = "unknown"
)

// AspectEvent 是一次切面控制事实的值视图，不含业务值。
// Boundary 在 (Site.Execution, NodeID) 内唯一；每次进入切面分配一次，嵌套重入不复用。
// ParentBoundary + ParentAttempt 关联直接外层切面及它的当前尝试，0 表示没有该维度。
// Scope / ParentScope 仅检查点 Retry 非空，直接来自桥的持久游标；Boundary 不是持久身份。
// 完整恢复节点不进入切面，故不补发历史事件。消息级尝试不使用本事件。
type AspectEvent struct {
	Site           Site
	NodeID         string
	Kind           AspectKind
	Name           string // 仅 NamedAspect 的显式名称；内置切面留空。
	Boundary       uint64
	ParentBoundary uint64
	ParentAttempt  int
	Attempt        int // Retry 的真实编号（包括首次），其他切面为 0。
	Limit          int // Retry 的总上限，包括首次。
	Phase          AspectPhase
	Reason         NodeFinishReason // 仅 finished、attempt_finished、backoff_finished 有值。
	Err            error            // 正常 skip 为 nil，可靠失败和 panic 保留错误。
	Time           time.Time
	Duration       time.Duration // 本次调用/尝试/等待已经观测到的耗时，不包含恢复前历史。
	Delay          time.Duration // 配置的退避时间；仅 backoff 事件。
	Timeout        time.Duration // 本层 Timeout 配置值。
	Scope          string
	ParentScope    string
	Origin         AttemptOrigin // 仅 Retry 的尝试/重试/退避事件。
}

// 切面事实的 Attrs key 归属 Core；observe 仅消费这些事实，不扩 Record。
const (
	AttrAspect         = "pulse.aspect"
	AttrAspectName     = "pulse.aspect.name"
	AttrBoundary       = "pulse.aspect.boundary"
	AttrParentBoundary = "pulse.aspect.parent_boundary"
	AttrParentAttempt  = "pulse.aspect.parent_attempt"
	AttrAttempt        = "pulse.attempt"
	AttrAttemptLimit   = "pulse.attempt.limit"
	AttrAttemptOrigin  = "pulse.attempt.origin"
	AttrAttemptScope   = "pulse.attempt.scope"
	AttrParentScope    = "pulse.attempt.parent_scope"
	AttrAspectPhase    = "pulse.aspect.phase"
	AttrRetryDelay     = "pulse.retry.delay_ns"
	AttrTimeout        = "pulse.timeout.duration_ns"
)

type aspectObservation struct {
	rc      *RunCtx
	obs     AspectObserver
	base    AspectEvent
	start   time.Time
	pending *aspectPendingAttempt
}

// 只有已报告进入、尚未报告结束的尝试需要异常收尾。每个 Retry 边界独有。
type aspectPendingAttempt struct {
	attempt CheckpointAttempt
	parent  string
	start   time.Time
	active  bool
}

func beginAspect(rc *RunCtx, kind AspectKind, name string, limit int) (aspectObservation, *RunCtx) {
	obs, ok := rc.exec.observer.(AspectObserver)
	if !ok {
		return aspectObservation{}, rc
	}
	cp := *rc
	id := rc.advance.aspectSeq.Add(1)
	cp.aspectBoundary, cp.aspectAttempt = id, 0
	o := aspectObservation{rc: &cp, obs: obs, start: time.Now(), base: AspectEvent{
		Site: rc.Site(), NodeID: rc.NodeID(), Kind: kind, Name: name, Limit: limit,
		Boundary: id, ParentBoundary: rc.aspectBoundary, ParentAttempt: rc.aspectAttempt,
	}}
	if kind == AspectRetry {
		o.pending = &aspectPendingAttempt{}
	}
	return o, &cp
}

// run 先经过节点原有的异常保护，再报告终态。切面自己的错误归类也可能 panic，
// 不能在尚未赋值的命名返回值上 defer 一条 completed；观察开关不改变业务结果。
func (o aspectObservation) run(rc *RunCtx, run func(*RunCtx) error) error {
	err := invokeNode(rc, run)
	if p := o.pending; p != nil && p.active {
		o.attempt(AspectAttemptFinished, p.attempt, p.parent, err, p.start)
	}
	o.finish(err)
	return err
}

func (o aspectObservation) emit(event AspectEvent) {
	if o.obs == nil {
		return
	}
	// 分类可能遍历用户 error；它与观察回调一样只能影响观测，不能改变业务结果。
	callSafely(func() {
		e := o.base
		e.Phase, e.Attempt = event.Phase, event.Attempt
		e.Scope, e.ParentScope, e.Origin = event.Scope, event.ParentScope, event.Origin
		e.Duration, e.Delay, e.Timeout = event.Duration, event.Delay, event.Timeout
		e.Time = time.Now()
		switch e.Phase {
		case AspectFinished, AspectAttemptFinished, AspectBackoffFinished:
			e.Reason = finishReason(event.Err)
			// 本层 Cancel 也可能只是业务返回后的清理，不能据此把独立 RPC 失败降为取消。
			// 执行域取消、外层控制时限实际到期或已锁存的取消来源才足以保留 canceled；
			// DeadlineExceeded 不会被后续清理 Cancel 覆盖。
			if e.Reason == NodeCanceled && o.rc.exec.ctx.Err() == nil &&
				o.rc.ctx.Err() != context.DeadlineExceeded && !isExecutionCancellation(event.Err) {
				e.Reason = NodeFailed
			}
		}
		e.Err = event.Err
		if e.Reason == NodeSkipped {
			e.Err = nil
		}
		o.obs.OnAspectEvent(e)
	})
}

func (o aspectObservation) finish(err error) {
	o.emit(AspectEvent{Phase: AspectFinished, Err: err, Duration: time.Since(o.start), Timeout: o.base.Timeout})
}

func (o aspectObservation) attempt(phase AspectPhase, a CheckpointAttempt, parent string, err error, start time.Time) {
	if p := o.pending; p != nil {
		switch phase {
		case AspectAttemptStarted:
			*p = aspectPendingAttempt{attempt: a, parent: parent, start: time.Now(), active: true}
		case AspectAttemptFinished:
			p.active = false
		}
	}
	origin := AttemptUnknown
	if a.Resumed != nil {
		origin = AttemptNew
		if *a.Resumed {
			origin = AttemptResumed
		}
	}
	e := AspectEvent{Phase: phase, Attempt: a.Index, Scope: a.Scope, ParentScope: parent, Origin: origin, Err: err}
	if !start.IsZero() {
		e.Duration = time.Since(start)
	}
	o.emit(e)
}

func (o aspectObservation) backoff(rc *RunCtx, a CheckpointAttempt, parent string, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	start := time.Now()
	// 延迟与尝试身份同源，但等待结束不是下一次分配成功的承诺。
	origin := AttemptUnknown
	if a.Resumed != nil {
		origin = AttemptNew
		if *a.Resumed {
			origin = AttemptResumed
		}
	}
	e := AspectEvent{Phase: AspectBackoffStarted, Attempt: a.Index, Scope: a.Scope, ParentScope: parent, Origin: origin, Delay: delay}
	o.emit(e)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	var err error
	select {
	case <-timer.C:
	case <-rc.ctx.Done():
		err = rc.ctx.Err()
	}
	e.Phase, e.Err, e.Duration = AspectBackoffFinished, err, time.Since(start)
	o.emit(e)
	return err
}

// NamedAspect 显式登记自定义切面的名字并记录进出。它不猜函数名，也不声称知道
// 自定义切面内部的重试/审批原因；这些事实仍由拥有它们的业务代码报告。
// name 必须非空，aspect 必须非 nil，否则构造时 panic。
func NamedAspect(name string, aspect Aspect) Aspect {
	if name == "" || aspect == nil {
		panic("pulse: NamedAspect requires a name and an aspect")
	}
	return func(rc *RunCtx, next func(*RunCtx) error) error {
		o, inner := beginAspect(rc, AspectCustom, name, 0)
		o.emit(AspectEvent{Phase: AspectStarted})
		return o.run(inner, func(rc *RunCtx) error { return aspect(rc, next) })
	}
}
