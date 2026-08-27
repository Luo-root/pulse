package flow

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// Aspect 与 kernel.Waterfall 同构：不调 next 即短路。
type Aspect interface {
	Around(rc *RunCtx, next func(*RunCtx) error) error
}

// AspectFunc 把函数适配为切面。
type AspectFunc func(rc *RunCtx, next func(*RunCtx) error) error

func (f AspectFunc) Around(rc *RunCtx, next func(*RunCtx) error) error { return f(rc, next) }

func buildChain(aspects []Aspect, core func(*RunCtx) error) func(*RunCtx) error {
	invoker := core
	for i := len(aspects) - 1; i >= 0; i-- {
		a := aspects[i]
		next := invoker
		invoker = func(rc *RunCtx) error {
			var called atomic.Bool
			return a.Around(rc, func(nextRC *RunCtx) error {
				if !called.CompareAndSwap(false, true) {
					return ErrNextCalledTwice
				}
				return next(nextRC)
			})
		}
	}
	return invoker
}

// Timeout 限制节点（含等数据）的总时长；超时取消本层 ctx。
func Timeout(d time.Duration) Aspect {
	return AspectFunc(func(rc *RunCtx, next func(*RunCtx) error) error {
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
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("flow: node %s timeout after %s", rc.NodeID(), d)
			}
			return ctx.Err()
		}
	})
}

// Retry 在节点 Run（含其内层切面）失败时重试。等数据阶段的取消不重试。
func Retry(attempts int, delay time.Duration) Aspect {
	if attempts <= 0 {
		attempts = 1
	}
	return AspectFunc(func(rc *RunCtx, next func(*RunCtx) error) error {
		var err error
		for i := 0; i < attempts; i++ {
			err = next(rc)
			if err == nil || err == ErrSkipped {
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
	})
}
