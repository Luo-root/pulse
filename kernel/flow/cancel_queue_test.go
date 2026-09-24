package flow

// #253-2 的回归用例：取消/超时要能打断「等名额」的节点。

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestAcquireHonorsCancel 直接钉 acquire 与取消的关系。
//
// 为什么要有这条：「某节点此刻是否已停在 acquire 里」从图的外部不可观测，所以纯行为
// 用例（TestCancelWakesQueuedNode）杀不死缺陷——取消完全可能落在节点跑到 acquire 之前，
// 而那条路径下修复前后行为相同（节点被 runNode 的 ctx 复查拦下、同样不进入 Run）。
// 这里由测试自己占住/让出名额，把三种时序都摆成确定的。
func TestAcquireHonorsCancel(t *testing.T) {
	g := mustNew(t, context.Background(), "test", WithMaxRunning(1))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	// 有界等待：缺陷形态是「永久挂住」，失败要是断言，不是把整包拖到超时。
	waitAcquire := func(ctx context.Context) (error, bool) {
		errCh := make(chan error, 1)
		go func() { errCh <- g.acquire(ctx) }()
		select {
		case err := <-errCh:
			return err, true
		case <-time.After(2 * time.Second):
			return nil, false
		}
	}

	// 占满唯一名额（等价于某个节点正在 Run）。
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// 一、名额满 + ctx 已取消：立即返回取消，不许排队。
	err, ok := waitAcquire(canceled)
	if !ok {
		t.Fatal("名额满 + ctx 已取消：acquire 不返回（等名额时取消唤不醒排队者）")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("名额满 + ctx 已取消：err = %v, want context.Canceled", err)
	}

	// 二、名额满 + ctx 未取消：先排队，取消后再被唤醒（前半是对照组：证明 acquire
	// 没被改成「早退」）。
	live, liveCancel := context.WithCancel(context.Background())
	liveErr := make(chan error, 1)
	go func() { liveErr <- g.acquire(live) }()
	select {
	case err := <-liveErr:
		t.Fatalf("名额满但 ctx 未取消：acquire 不该返回（err = %v）", err)
	case <-time.After(25 * time.Millisecond):
	}
	liveCancel()
	select {
	case err := <-liveErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消后 err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消后排队者没被唤醒")
	}
	g.release() // 让出名额，给下面第三档用

	// 三、名额空闲 + ctx 已取消：select 两路都就绪，无论选中哪一路都不得拿走名额。
	// 反复跑——随机取路意味着漏掉这一档的概率是 1/2，跑 20 次蒙不过去。
	for i := 0; i < 20; i++ {
		err, ok := waitAcquire(canceled)
		if !ok {
			t.Fatalf("第 %d 次：名额空闲 + ctx 已取消：acquire 不返回", i)
		}
		if err == nil {
			g.release() // 先把误拿的名额还回去，再报错
			t.Fatalf("第 %d 次：ctx 已取消，acquire 仍拿到了名额", i)
		}
	}
}

// TestCancelWakesQueuedNode 是图这一层的契约用例：取消后名额一释放，排队节点一个都
// 不进入 Run（终态 canceled）。修复前 acquire 是裸 `g.sem <- struct{}{}`：取消唤不醒
// 排队者，名额一释放它就照常执行并发起副作用，还被记成 completed。
//
// 确定性杀缺陷的是上面那条；这里钉的是「图里观察到的结果」——排队者不得执行、
// 终态为 canceled、占名额者照常 completed。
func TestCancelWakesQueuedNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	obs := &recordingObserver{}
	g := mustNew(t, ctx, "test", WithMaxRunning(1), WithObserver(obs))

	var entered atomic.Int32
	first := make(chan struct{}, 1)
	gate := make(chan struct{})
	body := func(*RunCtx) error {
		if entered.Add(1) == 1 {
			first <- struct{}{} // 拿到名额的那个在此报到
		}
		<-gate // 占住名额不放
		return nil
	}
	mustAdd(t, g, NewNode("a", nil, nil, body))
	mustAdd(t, g, NewNode("b", nil, nil, body))

	// 必须用 Start：Run 会阻塞到全图结束，而名额要等到 close(gate) 才释放。
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	<-first  // 名额已被占住：另一个节点要么在 acquire 里排队，要么还没跑到 ctx 复查
	cancel() // 取消必须把排队者唤醒
	close(gate)

	if err := g.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
	if n := entered.Load(); n != 1 {
		t.Fatalf("进入 Run 的节点数 = %d，want 1（排队者不得执行）", n)
	}
	log := obs.snapshot()
	if got := countPref(log, "F:"); got != 2 {
		t.Fatalf("finish 事件 = %d 条，want 2: %v", got, log)
	}
	canceled := countPref(log, "F:a:"+string(NodeCanceled)) + countPref(log, "F:b:"+string(NodeCanceled))
	if canceled != 1 {
		t.Fatalf("恰好一个节点应以 canceled 收尾，实得 %d: %v", canceled, log)
	}
}
