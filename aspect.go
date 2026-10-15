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

type nodePanicError struct {
	nodeID string
	value  any
}

func (e *nodePanicError) Error() string {
	return fmt.Sprintf("pulse: panic in node %s: %v", e.nodeID, e.value)
}

func (e *nodePanicError) Unwrap() error {
	cause, _ := e.value.(error)
	return cause
}

func isPanicFailure(err error) bool {
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
		child := rc.Fork()
		defer child.Cancel() // deadline 子层结束不等于释放 Fork 在父 ctx 上的注册
		ctx, cancel := context.WithTimeout(child.ctx, d)
		defer cancel()
		child.ctx = ctx
		errCh := make(chan error, 1)
		go func() { errCh <- next(child) }()
		var err error
		select {
		case err = <-errCh:
		case <-ctx.Done():
			child.Cancel()
			<-errCh // 等内层收干净：收尾不与它并发
		}
		// 内层可能已观察到 deadline 却返回 nil；两个分支同时就绪时，
		// select 选中结果不代表没有超时。两条退出路径共用最终取消判据。
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("pulse: node %s timeout after %s", rc.NodeID(), d)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
}

// Retry 在节点 Run（含其内层切面）失败时重试。等数据阶段的取消不重试；
// 跳过也不重试——跳过是「到达」，不是失败（判据见 isSkipped）。
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
//     HTTP 请求、邮件、写文件）**不可能**被自动撤销——把副作用挪到最后一次成功的尝试上，
//     或者自己用幂等键 / outbox / 去重 / 补偿。内存里暂存过，不代表外部世界能回滚。
//  2. **事务边界就是 `Retry` 套的那一层**。`Retry` 外面（更外层切面）的写不属于任何
//     attempt，也不参与回滚。
//  3. **跨尝试读不回被丢弃的写**。那条键在这一次尝试里从来没有过值，所以读它就是等一个
//     没人会写的槽（与「一条键没人产出」同一个形状，直到本层 ctx 取消）；尝试**内部**
//     读回自己刚写的值是自洽的（写暂存优先）。
//
// **它可以装在 `pulse.Sub` 装出来的那一步上**：每一次尝试各建一份**新的子执行**，
// 第一次尝试的桥回随它的暂存一起作废，父侧只拿得到成功那一次的输出版本。
//
// 重试的**次数与时机**不变：最多 `attempts` 次、每次之间 `delay`；进 `Retry` 这一层
// 拿到的 ctx 已经取消 / 到期时**不重试**（外层 `Timeout` 或父取消就是这样），而 `Retry`
// 内层自己包的超时只是**这一次尝试**的失败，照常重试。
//
// 与推进闸的关系：一个节点在**同一次执行**里只允许成功推进一次，而 `Retry` 每次尝试都
// 换一条新的写暂存——所以它的重入是「有边界的重入」、放行；用户自己写的重试切面开不了
// 边界，第二次推进会当场被拒（见 `RunCtx.beginAdvance` 与 `ErrNextCalledTwice`）。
func Retry(attempts int, delay time.Duration) Aspect {
	if attempts <= 0 {
		attempts = 1
	}
	return func(rc *RunCtx, next func(*RunCtx) error) error {
		if rc.exec.checkpoint != nil {
			return retryCheckpoint(rc, next, attempts, delay)
		}
		var err error
		for i := 0; i < attempts; i++ {
			// 开边界 → 跑 → 关边界。暂存是**这一次尝试**的：失败就整份丢掉，而「丢掉」
			// 什么都不用做——没有东西被写进执行槽位，下游也就从未被唤醒过。
			prev := rc.writes
			ws := &writeSet{outer: prev}
			rc.writes = ws
			err = next(rc)
			rc.writes = prev

			// 跳过不是失败：WaitAll 的跳过返回 *SkipError（带被跳过的 Key 名），
			// 只有 errors.Is 成立——用 == 比较会让整段「等输入 + 执行」被重跑
			// attempts-1 次并逐次等待。以跳过收尾的尝试**照常提交**它的写集：
			// 那些写是这一次运行真真切切产出的东西。
			if err == nil || isSkipped(err) {
				if cerr := ws.commit(rc.exec); cerr != nil {
					return cerr
				}
				return err
			}
			if rc.ctx.Err() != nil {
				return err
			}
			if i < attempts-1 && delay > 0 {
				t := time.NewTimer(delay)
				select {
				case <-t.C:
				case <-rc.ctx.Done():
					t.Stop()
					return rc.ctx.Err()
				}
			}
		}
		return err
	}
}
