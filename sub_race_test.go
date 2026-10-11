package pulse

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestSubHookupRaceWithReadOnlyEntry 盯住「挂接期间读只读入口」这条路。
//
// 场景就是评审点出来的那个用法：`Sub` 的 build 把子图指针交给**另一个 goroutine**
// （宿主完全可以这么干），那个 goroutine 在父图跑的同时不停调只读入口；而 runSub
// 此刻正在写挂接字段（path / observer / 名额）。没有共同互斥时 `-race` 会抓到一起
// DATA RACE——本轮把这三个字段收进 `child.adopt` 一把锁里写完。
//
// 子图是**自足的**（没有 `in:` 绑定、每条 `Requires` 都有来源），否则 `Validate()`
// 会在「缺来源」那一步就返回，根本走不到读**名额**的流式校验——那正是被写的字段，
// 用例就白跑了（第一版就是这么白跑的：变异对照 M1 存活）。
//
// 变异对照：把 `adopt` 里的 `g.mu.Lock()/Unlock()` 去掉 → 本用例在 `-race` 下必红
// （`.workbase/mutation_review2.py` 的 M1）。
func TestSubHookupRaceWithReadOnlyEntry(t *testing.T) {
	childOut := NewKey[int]("hk.cout")
	out := NewKey[int]("hk.out")
	stream := NewKey[<-chan int]("hk.stream")

	g := mustNew(t, context.Background(), "hookup", WithMaxRunning(2))

	children := make(chan *Graph) // 无缓冲：build 在这里等读者接过去，读者先拿到指针
	build := func(sc *SubCtx) (*Graph, error) {
		child, err := New(sc.Context(), sc.GraphID())
		if err != nil {
			return nil, err
		}
		if err := Produce(child, "p", stream, func(rc *RunCtx, send func(int) error) error {
			return send(1)
		}); err != nil {
			return nil, err
		}
		if err := Consume(child, "c", stream, func(rc *RunCtx, v int) error { return nil }); err != nil {
			return nil, err
		}
		if err := child.Add(NewNode("pass", nil, Provides(childOut), func(rc *RunCtx) error {
			return Set(rc, childOut, 1)
		})); err != nil {
			return nil, err
		}
		children <- child // 交给别的 goroutine —— 就是评审说的那种用法
		// 关键的一点：**发完指针先睡一下**，让读者真的跑起来（它要读到「写之前」
		// 的那几次才行）。写成「等读者读完了再返回」会把这几次读与写**同步**起来，
		// 竞态反而消失——所以这里只能靠时间，不能靠信号。
		time.Sleep(20 * time.Millisecond)
		return child, nil
	}
	if err := Sub(g, "step", []SubBind{Out(childOut, out)}, build); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex // 保护 readers / lastErr（测试自己的账）
	reads, lastErr := 0, error(nil)
	wg.Add(1)
	go func() {
		defer wg.Done()
		child := <-children
		for {
			select {
			case <-done:
				return
			default:
				err := child.Validate() // 读 child.mu 下的 path / 名额
				mu.Lock()
				reads++
				lastErr = err
				mu.Unlock()
			}
		}
	}()

	err := g.Run()
	close(done)
	wg.Wait()

	mu.Lock()
	n, verr := reads, lastErr
	mu.Unlock()
	// 读必须真的发生过，而且真的走到了读名额那一步——否则这条用例是空转的
	// （第一版就是这么白跑的：子图里缺来源，Validate 在第一道判据就返回了）。
	if n < 10 {
		t.Fatalf("只读入口只被调了 %d 次，没盖住挂接窗口", n)
	}
	if verr != nil {
		t.Fatalf("只读入口在挂接期间报错（说明它没走到名额那一步）：%v", verr)
	}
	if err != nil {
		t.Fatal(err)
	}
}
