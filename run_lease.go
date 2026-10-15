package pulse

import (
	"context"
	"fmt"
	"sync"
)

// runLease 属于一个节点执行，Fork 共享。普通 Run 持有额度，只有显式的
// 受管等待暂时归还；恢复业务（包括取消后的 defer）之前必须取回额度。
// idle 允许同一 Retry 的下一次 advance 重新取额，不允许并发或嵌套等待。
type runLease struct {
	mu    sync.Mutex
	pool  *limiter
	state leaseState
}

type leaseState uint8

const (
	leaseIdle leaseState = iota
	leaseAcquiring
	leaseHeld
	leaseParked
)

func (l *runLease) acquire(ctx context.Context) error {
	l.mu.Lock()
	if l.state != leaseIdle {
		l.mu.Unlock()
		return fmt.Errorf("pulse: concurrent node quota acquisition")
	}
	l.state = leaseAcquiring
	l.mu.Unlock()
	err := l.pool.acquire(ctx)
	l.mu.Lock()
	if err != nil {
		l.state = leaseIdle
	} else {
		l.state = leaseHeld
	}
	l.mu.Unlock()
	return err
}

func (l *runLease) release() {
	l.mu.Lock()
	if l.state != leaseHeld {
		l.mu.Unlock()
		return
	}
	l.state = leaseIdle
	// 归还与状态切换一起完成，不能把别的 advance 的额度扣掉。
	l.pool.release()
	l.mu.Unlock()
}

func (l *runLease) park() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.state {
	case leaseIdle:
		// 尚未进入 Run 或已从 Run 返回的切面没有持有名额。
		return false, nil
	case leaseHeld:
		l.state = leaseParked
		l.pool.release()
		return true, nil
	default:
		return false, fmt.Errorf("pulse: concurrent or nested managed stream wait on one RunCtx; use separate nodes for concurrent business work")
	}
}

func (l *runLease) resume(ctx context.Context) error {
	l.mu.Lock()
	if l.state != leaseParked {
		l.mu.Unlock()
		return fmt.Errorf("pulse: managed stream wait lost its quota lease")
	}
	l.state = leaseAcquiring
	l.mu.Unlock()
	err := l.pool.acquire(ctx)
	l.mu.Lock()
	if err != nil {
		l.state = leaseParked
	} else {
		l.state = leaseHeld
	}
	l.mu.Unlock()
	return err
}

// WaitStream 仅在原生 select 等阻塞等待期间暂时归还本节点的业务额度。
// wait 只能等待与记录结果，不能执行业务、重入本方法或同时使用同一 RunCtx；
// 返回后（含 panic 展开）才恢复业务。Stream 的受管 Send/Receive 已自动使用它。
// Go 无法自动感知任意 I/O，也无法强制 wait 回调只做等待。
//
// 取消时仍先取回额度再返回；因此取消返回可能要等其它不协作的持额节点退出。
// 已经完成的交付保留原结果，不会因重新排队期间取消而被报告成“没有发送”。
func (rc *RunCtx) WaitStream(wait func(context.Context) error) error {
	if rc == nil || wait == nil {
		return fmt.Errorf("pulse: WaitStream requires a RunCtx and wait function")
	}
	return rc.waitStream(wait)
}

func (rc *RunCtx) waitStream(wait func(context.Context) error) error {
	if rc.lease == nil {
		return wait(rc.ctx)
	}
	parked, err := rc.lease.park()
	if err != nil {
		return err
	}
	if !parked {
		return wait(rc.ctx)
	}
	defer func() {
		if err := rc.lease.resume(rc.ctx); err != nil {
			// 取消不许可业务在没有额度时继续。WithoutCancel 保留值，
			// 仅用于恢复已经释放的额度，不延长真正的流操作。
			if err := rc.lease.resume(context.WithoutCancel(rc.ctx)); err != nil {
				panic(err)
			}
		}
	}()
	return wait(rc.ctx)
}
