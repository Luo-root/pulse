package pulse

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestWaitLatchesCancellation 钉住 #327：没有节点读到的真实取消也必须进入终态。
// 变异对照：退回先 done 后 Err，空图与可选透传会从 canceled 变成 completed。
func TestWaitLatchesCancellation(t *testing.T) {
	for _, passThrough := range []bool{false, true} {
		name := "empty"
		if passThrough {
			name = "optional pass-through"
		}
		t.Run(name, func(t *testing.T) {
			g, err := New("wait-cancel", "收尾前真实取消")
			if err != nil {
				t.Fatal(err)
			}
			if passThrough {
				k := NewKey[int]("optional")
				if err := g.DeclareInput(NewPort(k)); err != nil {
					t.Fatal(err)
				}
				if err := g.DeclareOutput(NewPort(k)); err != nil {
					t.Fatal(err)
				}
			}
			var e *Execution
			callbacks := 0
			observer := ObserverFunc{GraphFinished: func(_ Site, reason NodeFinishReason, err error) {
				callbacks++
				if reason != NodeCanceled || !errors.Is(err, context.Canceled) {
					t.Errorf("finished = (%s, %v)，want canceled", reason, err)
				}
				if !errors.Is(e.Err(), context.Canceled) || e.Snapshot().Graph.Status != GraphStatusCanceled {
					t.Error("finished 回调中的 Err / Snapshot 没有同源取消")
				}
			}}
			e, err = g.NewExecution(context.Background(), Inputs{}, WithExecObserver(observer))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			e.Cancel()
			if !errors.Is(e.Err(), context.Canceled) {
				t.Fatal("探针前提：收尾前取消尚不可见")
			}
			for i := 0; i < 2; i++ {
				if err := e.Wait(); !errors.Is(err, context.Canceled) {
					t.Fatalf("第 %d 次 Wait = %v，取消不得变成成功", i, err)
				}
			}
			if callbacks != 1 || !errors.Is(e.Err(), context.Canceled) {
				t.Fatalf("收尾 callbacks=%d err=%v", callbacks, e.Err())
			}
		})
	}
}

// TestWaitLatchesCancellationAfterNodesExit 用会合点明确取消发生在节点退出之后、收尾之前。
func TestWaitLatchesCancellationAfterNodesExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	g, err := New("finished-before-cancel", "节点退出后取消")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("done", "完成节点", nil, nil, func(*RunCtx) error { return nil })); err != nil {
		t.Fatal(err)
	}
	e, err := g.NewExecution(ctx, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { e.wg.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-ctx.Done():
		t.Fatal("节点未按时退出")
	}
	if got := e.Snapshot().Nodes[0].Status; got != NodeStatusCompleted {
		t.Fatalf("取消前节点应已完成，got %s", got)
	}
	e.Cancel()
	if err := e.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("节点退出后的真实取消被抹掉：%v", err)
	}
}

// TestWaitFinalResultIsStable 同一终态给所有 Wait；不能把自己收尾的 cancel 反抓成错误。
func TestWaitFinalResultIsStable(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "completed"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			g, err := New("stable-wait", "并发收尾结果")
			if err != nil {
				t.Fatal(err)
			}
			var count int
			e, err := g.NewExecution(context.Background(), Inputs{}, WithExecObserver(ObserverFunc{
				GraphFinished: func(Site, NodeFinishReason, error) { count++ },
			}))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			if canceled {
				e.Cancel()
			}
			const callers = 8
			results := make(chan error, callers)
			var wg sync.WaitGroup
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); results <- e.Wait() }()
			}
			wg.Wait()
			close(results)
			for err := range results {
				if canceled && !errors.Is(err, context.Canceled) || !canceled && err != nil {
					t.Fatalf("终态漂移 canceled=%t err=%v", canceled, err)
				}
			}
			if count != 1 {
				t.Fatalf("finished 次数 = %d", count)
			}
			if e.Context().Err() == nil {
				t.Fatal("前提：Wait 应已释放 ctx")
			}
			if err := e.Wait(); canceled && !errors.Is(err, context.Canceled) || !canceled && err != nil {
				t.Fatalf("重复 Wait 反抓收尾 cancel：%v", err)
			}
		})
	}
}

func TestWaitKeepsFirstFailure(t *testing.T) {
	cause := errors.New("original node failure")
	g, err := New("first-failure", "首错优先")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("fail", "失败节点", nil, nil, func(*RunCtx) error { return cause })); err != nil {
		t.Fatal(err)
	}
	e, err := g.NewExecution(context.Background(), Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); !errors.Is(err, cause) {
		t.Fatalf("首错被取消覆盖：%v", err)
	}
	if !errors.Is(e.Err(), cause) || e.Snapshot().Graph.Status != GraphStatusFailed {
		t.Fatal("失败终态与首错不一致")
	}
}
