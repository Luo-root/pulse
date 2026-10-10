package pulse

import "context"

// limiter 是一份并发名额表：同时在 Run 里干活的节点至多 cap(sem) 个。
// nil 表示无限（默认）。
//
// **名额是跨图共享的**：子图默认继承父图那一份（见 runSub 的继承点），所以
// 「一棵图树里同时有几个节点在跑」由根图声明的那一次 WithMaxRunning 决定，
// 而不是每张子图各算各的。`Sub` 那一步**不吃名额**——它整段都在等子图跑完，
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
