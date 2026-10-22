package pulse

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// Aspect 包裹节点的「等输入 + 执行」整段；不调 next 即短路。
//
// 是函数类型而非接口：切面没有隐藏状态，唯一的实现就是这层函数签名。
type Aspect func(rc *RunCtx, next func(*RunCtx) error) error

func buildChain(aspects []Aspect, core func(*RunCtx) error) func(*RunCtx) error {
	invoker := core
	for i := len(aspects) - 1; i >= 0; i-- {
		a := aspects[i]
		next := invoker
		index := i
		invoker = func(rc *RunCtx) error {
			previous := rc.aspectIndex
			rc.aspectIndex = index
			defer func() { rc.aspectIndex = previous }()
			// 门闩：单节点的体不得**并发**进入（两个 goroutine 同时跑同一
			// 节点会抢同一批槽位，语义上必然错）。顺序重入的判据不在这里——
			// 「有没有新的尝试边界」由最内层的推进闸判（见 RunCtx.beginAdvance），
			// 所以 `Retry` 与用户自写的重试切面在那里被分开。
			var depth atomic.Int32
			return invokeNode(rc, func(rc *RunCtx) error {
				return a(rc, func(nextRC *RunCtx) error {
					if depth.Add(1) > 1 {
						depth.Add(-1)
						return ErrNextCalledTwice
					}
					defer depth.Add(-1)
					return next(nextRC)
				})
			})
		}
	}
	return invoker
}

// invokeNode 把业务体与每层切面的 panic 就地转成失败。边界随调用进入
// Timeout 的工作 goroutine，外层 Retry / checkpoint 才能收到同一条错误。
func invokeNode(rc *RunCtx, run func(*RunCtx) error) (err error) {
	returned := false
	defer func() {
		if rec := recover(); !returned {
			// returned 也覆盖 GODEBUG=panicnil=1 下 recover 返回 nil 的 panic。
			err = &nodePanicError{nodeID: rc.NodeID(), value: rec}
		}
	}()
	err = run(rc)
	returned = true
	return err
}

// inspectNodeError 只包围引擎实际执行的错误检查与控制收尾。业务 next 已有
// invokeNode 保护；这里的异常不能携带故障 error，也不能被外层 Retry 当作业务失败重跑。
func inspectNodeError(inspect func() error) (err error) {
	returned := false
	defer func() {
		if rec := recover(); !returned {
			err = &nodePanicError{value: fmt.Sprintf("%T", rec), inspection: true}
		}
	}()
	err = inspect()
	returned = true
	return err
}

func isErrorInspectionFailure(err error) bool {
	if err == nil {
		return false
	}
	var failure *nodePanicError
	return errors.As(err, &failure) && failure != nil && failure.inspection
}

type nodePanicError struct {
	nodeID     string
	value      any
	inspection bool // 错误方法异常只保存 panic 值的类型，不再遍历故障错误链。
}

func (e *nodePanicError) Error() string {
	if e.inspection {
		return fmt.Sprintf("pulse: panic while inspecting node error (%s)", e.value)
	}
	return fmt.Sprintf("pulse: panic in node %s: %v", e.nodeID, e.value)
}

func (e *nodePanicError) Unwrap() error {
	if e.inspection {
		return nil
	}
	cause, _ := e.value.(error)
	return cause
}

func isPanicFailure(err error) bool {
	if err == nil {
		return false
	}
	var failure *nodePanicError
	return errors.As(err, &failure)
}

// Timeout 限制节点（含等数据）的总时长；超时取消本层 ctx。
//
// 超时是**协作式**的：到期后先取消本层 ctx，再等内层返回（内层不看 ctx 时
// 只能等它自己结束）。不能拿到 `ctx.Done()` 就返回——`Fork` 与父 RunCtx
// 共享写入目标与推进账，提前返回会让收尾路径（`settleUnwritten`）和还在跑的
// `Run` 同时碰同一批槽，既有数据竞态，也让 `Run()` 的「阻塞到全部终止」
// 失真。已经落地的输出不会撤回：超时是失败，不是回滚。
func Timeout(d time.Duration) Aspect {
	return func(rc *RunCtx, next func(*RunCtx) error) error {
		o, rc := beginAspect(rc, AspectTimeout, "", 0)
		o.base.Timeout = d
		o.emit(AspectEvent{Phase: AspectStarted, Timeout: d})
		return o.runControl(rc, func(rc *RunCtx) error {
			child := rc.Fork()
			defer child.Cancel() // deadline 子层结束不等于释放 Fork 在父 ctx 上的注册
			timeoutErr := fmt.Errorf("pulse: node %s timeout after %s", rc.NodeID(), d)
			// 两条链共用同一截止时刻：业务 fail-fast 不影响可靠链，真实 Timeout 影响两者。
			deadline := time.Now().Add(d)
			ctx, cancel := context.WithDeadlineCause(child.ctx, deadline, timeoutErr)
			defer cancel()
			control := ctx
			if rc.exec.checkpoint != nil {
				var cancelControl context.CancelFunc
				control, cancelControl = context.WithDeadlineCause(child.controlCtx, deadline, timeoutErr)
				defer cancelControl()
			}
			child.ctx, child.controlCtx = ctx, control
			errCh := make(chan error, 1)
			go func() { errCh <- next(child) }()
			var err error
			expired := false
			reportDeadline := func() {
				if !expired && context.Cause(ctx) == timeoutErr {
					expired = true
					o.emit(AspectEvent{Phase: AspectDeadline, Timeout: d, Duration: time.Since(o.start)})
				}
			}
			select {
			case err = <-errCh:
			case <-ctx.Done():
				reportDeadline() // 自己的计时器确实触发，且内层可能还没有退出。
				// ctx 已取消；不能在这里把内部 fail-fast 再转成可靠控制链的显式取消。
				err = <-errCh // 等内层收干净，并保留期间捕获的真实失败
			}
			reportDeadline() // 两个 channel 同时就绪时，仍以独立 cause 判定本层事实。
			// 清理取消不能覆盖已经捕获的 panic / 可靠失败，两条 select 路径共用此判据。
			if isPanicFailure(err) || isCheckpointFailure(err) {
				return err
			}
			// 每次 Timeout 的 cause 独立：父域截止或外层 Timeout 不能冒充本层计时器。
			// 即使等待业务退出期间父域也到期，本层先触发的事实仍然保留。
			if context.Cause(ctx) == timeoutErr {
				return timeoutErr
			}
			if parentErr := rc.Context().Err(); parentErr != nil {
				return &executionCancellation{cause: parentErr}
			}
			// Fork 的局部 Cancel 也用于清理。它既不是本层计时器到期，
			// 也不是父域中断，不能覆盖内层已经取得的成功、跳过或业务错误。
			return err
		})
	}
}

// Retry 在节点 Run（含其内层切面）返回普通业务错误时重试。正常跳过不重试——
// 跳过是「到达」，不是失败（判据见 isSkipped）。本层 ctx 已取消或到期时也不继续重试。
// 错误链包含 CheckpointError 时立即停止，即使本图未启用检查点；可靠操作可能已经完成
// 却没有返回确认，不能把这种不确定结果当作普通业务失败重复执行。
//
// **它给每一次尝试开一条独立的写暂存**（attempt 写集）：`Retry` 里面（`next` 之内、
// 含更内层的切面）的 `Set` / `Skip` 先落在这条暂存上，**不碰执行槽位**。于是：
//
//   - 尝试**失败**且还要重试 → 整条暂存**丢掉**：失败那一次的输出版本，下游从未见过；
//     后续尝试的写也不会被幂等首写静默吞掉。旧的「重试安全的前提由调用方自己守」从
//     这一版起由引擎接管。
//   - 尝试**成功**（或以跳过收尾）→ 整条暂存交出去：外层还有一层暂存就交给外层
//     （嵌套 `Retry` 时内层的结果属于外层那一次尝试），否则逐条写进执行槽位。
//     多 Key 是**逐条**发布的，不承诺严格原子可见（提交窗口里可能「一部分输出已就绪、
//     其余仍 pending」，等这些输出的下游可能被先到的那一条先唤醒）。
//
// 三条要记住的边界：
//
//  1. **只回滚引擎管理的输出**。用户 `Run` 里已经发生的任意外部副作用（数据库提交、
//     HTTP 请求、邮件、写文件）**不可能**被自动撤销。即使放在函数末尾，也可能在「操作成功
//     但确认未收到」后再次执行。防重需要稳定的业务幂等键或可靠去重；单独一笔事务不能
//     关闭这个窗口。outbox 须可靠地共同提交业务变更与消息，保持事件身份稳定，并配合
//     消费幂等或去重；补偿是事后修复，不能保证不重复。
//  2. **事务边界就是 `Retry` 套的那一层**。`Retry` 外面（更外层切面）的写不属于任何
//     attempt，也不参与回滚。
//  3. **跨尝试读不回被丢弃的写**。那条键在这一次尝试里从来没有过值，所以读它就是等一个
//     没人会写的槽（与「一条键没人产出」同一个形状，直到本层 ctx 取消）；尝试**内部**
//     读回自己刚写的值是自洽的（写暂存优先）。
//
// **它可以装在 `pulse.Sub` 装出来的那一步上**：每一次尝试各建一份**新的子执行**，
// 第一次尝试的桥回随它的暂存一起作废，父侧只拿得到成功那一次的输出版本。
//
// 总上限 `attempts` 包含首次调用，非正值按一次处理；继续尝试时，每次之间等待 `delay`。
// 进 `Retry` 这一层拿到的 ctx 已经取消 / 到期时**不重试**（外层 `Timeout` 或父取消
// 就是这样），而内层自己包的超时只是**这一次尝试**的普通失败，未触及上述停止条件时照常重试。
//
// 与推进闸的关系：一个节点在**同一次执行**里只允许成功推进一次，而 `Retry` 每次尝试都
// 换一条新的写暂存——所以它的重入是「有边界的重入」、放行；用户自己写的重试切面开不了
// 边界，第二次推进会当场被拒（见 `RunCtx.beginAdvance` 与 `ErrNextCalledTwice`）。
func Retry(attempts int, delay time.Duration) Aspect {
	if attempts <= 0 {
		attempts = 1
	}
	return func(rc *RunCtx, next func(*RunCtx) error) error {
		o, rc := beginAspect(rc, AspectRetry, "", attempts)
		o.emit(AspectEvent{Phase: AspectStarted})
		return o.runControl(rc, func(rc *RunCtx) error {
			if rc.exec.checkpoint != nil {
				return retryCheckpoint(rc, next, attempts, delay, o)
			}
			var err error
			resumed := false
			for i := 1; i <= attempts; i++ {
				// 实际分配写集后才报告进入，首次不是重试。失败写集整份丢弃。
				prev := rc.writes
				ws := &writeSet{outer: prev}
				rc.writes = ws
				if o.obs != nil {
					rc.aspectAttempt = i
				}
				a := CheckpointAttempt{Index: i, Resumed: &resumed}
				if i > 1 {
					o.attempt(AspectRetrying, a, "", nil, time.Time{})
				}
				start := time.Now()
				o.attempt(AspectAttemptStarted, a, "", nil, time.Time{})
				err = next(rc)
				rc.writes = prev

				// 正常 skip 照常提交；可靠失败和 panic 不能降格成 skip。
				if err == nil || isSkipped(err) {
					if cerr := ws.commit(rc.exec); cerr != nil {
						err = cerr
					}
					return o.attempt(AspectAttemptFinished, a, "", err, start)
				}
				err = o.attempt(AspectAttemptFinished, a, "", err, start)
				if isCheckpointFailure(err) || isExecutionCancellation(err) || isErrorInspectionFailure(err) || rc.ctx.Err() != nil {
					return err
				}
				if i < attempts {
					if werr := o.backoff(rc, a, "", delay); werr != nil {
						return werr
					}
				}
			}
			return err
		})
	}
}
