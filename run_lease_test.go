package pulse

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRunLeaseWaitRestoresQuotaBeforeCanceledReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := make(chan struct{})
	rc := &RunCtx{ctx: &leaseObservedContext{Context: ctx, observed: observed}, lease: &runLease{pool: newLimiter(1)}}
	if err := rc.lease.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer rc.lease.release()
	result := errors.New("operation already completed")
	otherHeld := make(chan struct{})
	returned := make(chan error, 1)
	go func() {
		returned <- rc.WaitStream(func(context.Context) error {
			// 等待期间另一个业务节点取得同一个池的唯一额度。
			if err := rc.lease.pool.acquire(context.Background()); err != nil {
				return err
			}
			cancel()
			close(otherHeld)
			return result
		})
	}()
	select {
	case <-otherHeld:
	case <-time.After(time.Second):
		t.Fatal("wait did not yield quota")
	}
	select {
	case <-observed: // resume 已经进入取消的 acquire；不是靠 sleep 猜它在排队。
	case <-time.After(time.Second):
		t.Fatal("resume did not try to reacquire")
	}
	select {
	case err := <-returned:
		t.Fatalf("business resumed without quota: %v", err)
	default:
	}
	rc.lease.pool.release()
	select {
	case err := <-returned:
		if !errors.Is(err, result) {
			t.Fatalf("completed result changed during reacquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not return after quota became available")
	}
	rc.lease.release()
	if got := len(rc.lease.pool.sem); got != 0 {
		t.Fatalf("quota leaked: %d", got)
	}
	// Retry 的下一次 advance 可以再次取额，重复 release 不能扣掉别人额度。
	if err := rc.lease.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	rc.lease.release()
	rc.lease.release()
}

type leaseObservedContext struct {
	context.Context
	once     sync.Once
	observed chan struct{}
}

func (c *leaseObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestRunLeaseRejectsNestedWaitAndRestoresOnPanic(t *testing.T) {
	rc := &RunCtx{ctx: context.Background(), lease: &runLease{pool: newLimiter(1)}}
	if err := rc.lease.acquire(rc.ctx); err != nil {
		t.Fatal(err)
	}
	defer rc.lease.release()
	var innerRan bool
	if err := rc.WaitStream(func(context.Context) error {
		if err := rc.WaitStream(func(context.Context) error { innerRan = true; return nil }); err == nil {
			return errors.New("nested wait was accepted")
		}
		return nil
	}); err != nil || innerRan {
		t.Fatalf("nested wait: ran=%v err=%v", innerRan, err)
	}
	func() {
		defer func() {
			if recover() != "wait panic" {
				t.Error("panic did not propagate")
			}
			if got := len(rc.lease.pool.sem); got != 1 {
				t.Errorf("panic resumed without quota: %d", got)
			}
		}()
		_ = rc.WaitStream(func(context.Context) error { panic("wait panic") })
	}()
}

func TestRunLeaseUnlimitedAndAspectWait(t *testing.T) {
	for _, pool := range []*limiter{nil, newLimiter(1)} {
		rc := &RunCtx{ctx: context.Background(), lease: &runLease{pool: pool}}
		var calls int
		if err := rc.WaitStream(func(context.Context) error { calls++; return nil }); err != nil {
			t.Fatal(err)
		}
		if err := rc.lease.acquire(rc.ctx); err != nil {
			t.Fatal(err)
		}
		if err := rc.WaitStream(func(context.Context) error { calls++; return nil }); err != nil {
			t.Fatal(err)
		}
		rc.lease.release()
		if calls != 2 || (pool != nil && len(pool.sem) != 0) {
			t.Fatalf("calls=%d pool=%v", calls, pool)
		}
	}
}
