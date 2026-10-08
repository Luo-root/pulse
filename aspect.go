package pulse

import (
	"context"
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
		invoker = func(rc *RunCtx) error {
			// 门闩：单节点的 Run 不得并发进入（两个 goroutine 同时跑同一
			// 节点会抢同一批槽位，语义上必然错）；顺序重入合法——Retry
			// 正依赖它（1→0→1）。
			var depth atomic.Int32
			return a(rc, func(nextRC *RunCtx) error {
				if depth.Add(1) > 1 {
					depth.Add(-1)
					return ErrNextCalledTwice
				}
				defer depth.Add(-1)
				return next(nextRC)
			})
		}
	}
	return invoker
}

// Timeout 限制节点（含等数据）的总时长；超时取消本层 ctx。
//
// 超时是**协作式**的：到期后先取消本层 ctx，再等内层返回（内层不看 ctx 时
// 只能等它自己结束）。不能拿到 `ctx.Done()` 就返回——`Fork` 与父 RunCtx
// 共享 `wrote`，提前返回会让收尾路径（`skipAllOrUnwritten`）和还在跑的
// `Run` 同时碰同一批槽，既有数据竞态，也让 `Run()` 的「阻塞到全部终止」
// 失真。已发布的槽不会撤回：超时是失败，不是回滚。
func Timeout(d time.Duration) Aspect {
	return func(rc *RunCtx, next func(*RunCtx) error) error {
		child := rc.Fork()
		ctx, cancel := context.WithTimeout(child.ctx, d)
		defer cancel()
		child.ctx = ctx
		errCh := make(chan error, 1)
		go func() { errCh <- next(child) }()
		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			child.Cancel()
			<-errCh // 等内层收干净：收尾不与它并发
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("pulse: node %s timeout after %s", rc.NodeID(), d)
			}
			return ctx.Err()
		}
	}
}

// Retry 在节点 Run（含其内层切面）失败时重试。等数据阶段的取消不重试；
// 跳过也不重试——跳过是「到达」，不是失败（判据见 isSkipped）。
//
// **重试安全的前提**：失败前没有写过任何 Provide，也没有不可重入的副作用。
// 槽位是「到达即发布、幂等首写」的：前一次 attempt 一旦 Set/Skip 过，下游
// 已经被唤醒，后续 attempt 的写会被静默忽略（对已就绪的槽 resolveValue 返回
// nil），槽位也不会回滚——回滚等于重开槽位，与「一次性」契约冲突，所以这条
// 只能由调用方守。需要事务性重试时，把副作用与输出挪到最后一次成功的
// attempt 上（例如先算完再 Set），别指望引擎撤前一次。
func Retry(attempts int, delay time.Duration) Aspect {
	if attempts <= 0 {
		attempts = 1
	}
	return func(rc *RunCtx, next func(*RunCtx) error) error {
		var err error
		for i := 0; i < attempts; i++ {
			err = next(rc)
			// 跳过不是失败：WaitAll 的跳过返回 *SkipError（带被跳过的
			// Key 名），只有 errors.Is 成立——用 == 比较会让整段
			// 「等输入 + 执行」被重跑 attempts-1 次并逐次等待。
			if err == nil || isSkipped(err) {
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
