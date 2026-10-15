package pulse

import (
	"context"
	"sync"
)

// streamOperation 串行化会作废交付或可靠提交的操作。它与状态锁不同：
// 持此门时允许锁外同步调用可靠钩子；等待门时让位，避免持额等正在恢复额度的提交。
// 单写端与固定锁序保证消息事务不会与另一事务反向持门。
type streamOperation struct {
	once sync.Once
	gate chan struct{}
}

func (o *streamOperation) lock(rc *RunCtx) error {
	o.once.Do(func() { o.gate = make(chan struct{}, 1) })
	ctx := context.Background()
	if rc != nil {
		ctx = rc.Context()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case o.gate <- struct{}{}:
		return nil
	default:
	}
	wait := func(ctx context.Context) error {
		select {
		case o.gate <- struct{}{}:
			return nil // 取得门后保留已完成结果，由调用方在提交前复查取消。
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if rc != nil {
		return rc.waitStream(wait)
	}
	return wait(ctx)
}

func (o *streamOperation) unlock() { <-o.gate }
