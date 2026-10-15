package pulse

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

func (e *Execution) fail(err error) {
	if err == nil || isSkipped(err) {
		return
	}
	e.mu.Lock()
	if e.err == nil {
		e.err = err
		e.cancel()
	}
	e.mu.Unlock()
}

// acquire 占用这次运行的**有效**名额表里的一个名额（nil = 无限，直接放行）。
// 取消语义见 limiter.acquire。
func (e *Execution) acquire(ctx context.Context) error { return e.lim.acquire(ctx) }

// release 归还名额。没占过、或名额表无限，都安全。
func (e *Execution) release() { e.lim.release() }

func (e *Execution) runNode(n *Node) {
	defer e.wg.Done()
	if r, ok := e.restored[n]; ok {
		var err error
		if r.Reason == NodeSkipped {
			err = NoValue()
		}
		e.finishNode(n, r.Reason, err)
		return
	}

	rc := newRunCtx(e, n, e.ctx)
	defer rc.Cancel()

	// 每节点生命周期事件至多一次（防 Retry 双打点）。
	var waited, ran atomic.Bool
	emitWaiting := func() {
		if waited.CompareAndSwap(false, true) {
			e.recordNodeState(n, NodeStatusWaiting, nil)
			e.notify(func(o Observer) { o.OnNodeWaiting(e.site, n.id) })
		}
	}
	emitRunning := func() {
		if ran.CompareAndSwap(false, true) {
			e.recordNodeState(n, NodeStatusRunning, nil)
			e.notify(func(o Observer) { o.OnNodeRunning(e.site, n.id) })
		}
	}

	// 切面覆盖「等输入 + 执行」整段，这样 Timeout 能打断 Wait。
	// MaxRunning 只在即将执行用户 Run 时占用。
	// 生命周期埋点在 innermost core：Retry 重入时靠门闩只发一次。
	chain := buildChain(append(append([]Aspect{}, e.aspects...), n.aspects...), func(rc *RunCtx) (err error) {
		// 推进闸：同一个节点执行上下文里 `next()` 只允许成功推进一次——`Retry` 每试都开
		// 新的尝试边界，所以它的重入放行；用户自己写的重试切面开不了边界，第二次推进
		// 当场拒（见 beginAdvance）。判据在最内层，所以**所有**节点（含挂子图那一步）
		// 共用同一处，不各写一套。
		if err := rc.beginAdvance(); err != nil {
			return err
		}
		defer func() { rc.endAdvance(err) }()
		emitWaiting()
		skipped, err := awaitAll(rc, n.requires)
		if err != nil {
			return err
		}
		if rc.ctx.Err() != nil {
			return rc.ctx.Err()
		}
		// 到几个收几个：**只要有一条输入真的到了值**，就带着到了的那些进入
		// Run；一条值都没到才轮到本节点自己跳过。判据是「有没有值」，不是
		// 「有没有跳过」——上游某一路没有值，不该让手里还有数据的下游跟着停，
		// 也不该让已经到达的值作废。一条 Requires 都没有的源节点不适用此
		// 判据，照常执行。
		if len(n.requires) > 0 && len(skipped) == len(n.requires) {
			return skipErr(skipped...)
		}
		// 挂子图那一步**不吃名额**：它整段（取值 + 子图 + 桥回）都在等子图跑完，
		// 占着名额就等于把父侧额度锁在「等」上——名额共享之后那就是死锁
		// （限额 ≤ 嵌套深度时必现，实测见 .workbase/probe-sharedsem）。
		if !n.sub {
			if err := rc.lease.acquire(rc.ctx); err != nil {
				return err
			}
			defer rc.lease.release()
		}
		emitRunning()
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("pulse: panic in node %s: %v", n.id, rec)
			}
		}()
		if n.run != nil {
			return n.run(rc)
		}
		return nil
	})

	var err error
	if e.checkpoint != nil {
		err = e.runCheckpointChain(n, rc, chain)
	} else {
		err = chain(rc)
	}
	if isSkipped(err) {
		e.settleUnwritten(n, rc)
		e.finishNode(n, NodeSkipped, err)
		return
	}
	if err != nil {
		e.fail(err)
		e.settleUnwritten(n, rc)
		e.finishNode(n, finishReason(err), err)
		return
	}
	e.settleUnwritten(n, rc)
	e.finishNode(n, NodeCompleted, nil)
}

// settleUnwritten 把这次运行里**还没有落地**的 Provide 收成跳过（隐式跳过）。
//
// 判据是**落地状态**，不是「这个节点写过谁」。这个差别是实的：一次失败的尝试写过 K、
// 而整份暂存被丢掉了，K 就仍然没有落地——它该以跳过收尾，而不是把下游永远吊在那条槽上
// （一句看不出病因的超时）。没有尝试边界时两种判据一一对应（写过 ⟺ 已就绪 / 已跳过），
// 所以不装 `Retry` 的节点行为逐条不变。
//
// 写入走**当前写入目标**（见 `RunCtx.writeTarget`）：有暂存就落暂存，没有就直写执行槽位。
// 于是 #307 的 Durable 模式下，「完整 Aspect 链结束、收敛全部 Provides 状态」这一步同样
// 落在节点暂存里，由检查点提交之后才发布——两层暂存是**按顺序组合**，不是互相替代
// （见 #317「与 Durable 输出提交的边界」）。
func (e *Execution) settleUnwritten(n *Node, rc *RunCtx) {
	for _, k := range n.provides {
		if rc.landed(k) {
			continue
		}
		_ = rc.writeTarget(k).resolveSkip()
	}
}

// limiter 是一份并发名额表：同时在 Run 里干活的节点至多 cap(sem) 个。
// nil 表示无限（默认）。
//
// **名额是跨图共享的**：子图默认继承父图那一份（见 runSub 的继承点），所以
// 「一棵图树里同时有几个节点在跑」由根图声明的那一次 WithMaxRunning 决定，
// 而不是每张子图各算各的。挂子图那一步**不吃名额**——它整段都在等子图跑完，
// 占着就等于把父侧的额度锁在「等」上（实测：父子共用一份名额又不让位时，
// 限额 ≤ 嵌套深度必死锁）。
type limiter struct {
	sem chan struct{}
}

func newLimiter(n int) *limiter {
	if n <= 0 {
		return nil
	}
	return &limiter{sem: make(chan struct{}, n)}
}

// acquire 占用一个名额。nil 名额表直接放行。
//
// ctx 取消时不再排队，**拿到名额后再看一次**：「名额空出」与「取消」同时就绪时
// select 会随机挑一个分支，少了这一眼就会在取消之后仍进入 Run（实测这条路径
// 并不罕见）。留给调用方的名额不该用在一个已取消的节点上。
func (l *limiter) acquire(ctx context.Context) error {
	if l == nil {
		return nil
	}
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-l.sem // 放回名额
		return err
	}
	return nil
}

// release 归还一个名额。nil 名额表无需操作。
func (l *limiter) release() {
	if l == nil {
		return
	}
	<-l.sem
}

// capacity 是这份名额表的上限，0 表示无限。
func (l *limiter) capacity() int {
	if l == nil {
		return 0
	}
	return cap(l.sem)
}

// runStaticSub 是挂子图那一步的运行体：取值 → 建一份子执行 → 跑 → 桥回。
//
// **每个挂载位置一份子执行**：子图是**定义**，挂在这里的这一次运行要一份自己的
// ctx / 槽位 / 状态（`newChildExecution`）。于是同一张子图挂在两处时两处都能跑，
// 各自的输入输出互不干扰——旧版本是「一次性认领 + 运行期回写子图字段」，同一张图
// 只能跑一处、而且两处会互相踩配置。
//
// 三样东西跟着消失：
//
//   - **认领**：不再有「这张图用过没有」这回事，定义可以跑很多次；
//   - **回写子图字段**（旧 `adopt`）：有效观察者 / 名额 / 图级切面在**创建子执行**时
//     按「父执行环境 + 子图自己的声明」算出来，定义保持只读；
//   - **转发取消的那条 goroutine**：子执行的 ctx 从父节点这一层的 ctx 派生，
//     父侧取消天然传下去（实测过旧版本不转发时父图会跑满整段并报成功）。
func runStaticSub(rc *RunCtx, id string, binds []SubBind, child *Graph) error {
	// 1) 先取父侧的全部输入：跳过是**到达**，照跳（用 SkipSeed 表达）；其它错误
	//    （取消、未声明）直接返回。`Fix` / `FixSkip` 那条不读父侧，直接写进这一次的输入。
	var in Inputs
	for _, b := range binds {
		if b.fix != nil {
			if b.fix.bad != nil {
				return fmt.Errorf("pulse: Sub %q: %w", id, b.fix.bad)
			}
			for _, ref := range b.fixKeys() {
				en := b.fix.in.entries[ref.name]
				if err := in.set(en.ref, en.value, en.skip); err != nil {
					return fmt.Errorf("pulse: Sub %q: %w", id, err)
				}
			}
			continue
		}
		if !b.in {
			continue
		}
		v, skip, err := readParentInput(rc, b)
		if err != nil {
			return err
		}
		if err := in.setArrived(b.child, v, skip); err != nil {
			return fmt.Errorf("pulse: Sub %q: %w", id, err)
		}
	}

	// 2) 为这个挂载位置建一份子执行。父节点这一层的 ctx 是它的取消域：父侧被取消
	//    （含这一步上的 `Timeout` 切面）子图当场停。
	//
	//    这一步还会判子图那侧的启动条件（每条被 `Requires` 的进口都要有交代）：
	//    父图没绑、子图里也没人产出的进口在这里报，而不是让子图跑起来之后挂死。
	sub, err := child.newChildExecution(rc.exec, rc.Context(), rc.node.id, in)
	if err != nil {
		return fmt.Errorf("pulse: Sub %q: %w", id, err)
	}
	rc.exec.recordChild(rc.node, sub)
	if err := rc.prepareChildCheckpoint(sub); err != nil {
		sub.Cancel()
		return err
	}

	// 3) 跑子执行：Start + Wait。失败原样冒泡（首错就是子图的首错）。
	if err := sub.Start(); err != nil {
		return err
	}
	if err := sub.Wait(); err != nil {
		return err
	}

	// 4) 桥回父侧：就绪 → Set，跳过 → Skip；**全部**输出都跳 → 本节点以跳过收尾。
	return bridgeOutputs(rc, id, binds, sub)
}

// readParentInput 取父侧一条绑定的输入：**跳过是到达**，照跳种（skip=true）；
// 其它错误（取消、未声明）原样返回。
func readParentInput(rc *RunCtx, b SubBind) (any, bool, error) {
	v, err := b.pull(rc)
	if err != nil {
		if errors.Is(err, ErrSkipped) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return v, false, nil
}

// bridgeOutputs 把子执行跑完之后的结果桥回父侧：就绪 → `Set`，跳过 → `Skip`；
// **全部**输出都以跳过收尾 → 这一步以节点级跳过收尾（`NoValue()`，未发布的输出由引擎自动跳过）。
//
// 读的是**子执行**的槽位（这一次挂载位置那一份），不是子图定义——同一张子图挂在两处时，
// 两处各读各的那一份。
func bridgeOutputs(rc *RunCtx, id string, binds []SubBind, sub *Execution) error {
	hasOut, allSkipped := false, true
	for _, b := range binds {
		if b.intoChild() {
			continue // `In` 与 `Fix` / `FixSkip` 都是「往子图送」，不是输出
		}
		hasOut = true
		st, _ := sub.slotOf(b.child).snapshot()
		switch st {
		case slotReady:
			allSkipped = false
		case slotSkipped:
		default:
			return fmt.Errorf("pulse: Sub %q: the child finished but its output key %s "+
				"never arrived (neither written nor skipped)", id, b.child)
		}
	}
	if hasOut && allSkipped {
		return NoValue() // 节点级跳过；未发布的输出由引擎自动跳过
	}
	for _, b := range binds {
		if b.intoChild() {
			continue
		}
		st, v := sub.slotOf(b.child).snapshot()
		if st == slotSkipped {
			if err := skipRef(rc, b.parent); err != nil {
				return err
			}
			continue
		}
		if err := b.push(rc, v); err != nil {
			return err
		}
	}
	return nil
}
