package pulse

import (
	"context"
	"sync"
	"testing"
	"time"
)

// hookupRace 是这条 race 用例的**共用形状**：子图在装配期建好之后，宿主直接把指针交给
// 另一个 goroutine（完全可以这么干），那头在父图跑的同时不停调只读入口；而 runStaticSub
// 此刻正在写挂接字段（observer / 名额 / 图级切面）——挂接发生在**运行期**
// （`child.adopt`），所以两边确实会撞上。没有共同互斥时 `-race` 会抓到
// 一起 DATA RACE——那些字段现在收进 `child.adopt` 一把锁里写完。
//
// 子图是**自足的**（没有 `in:` 绑定、每条 `Requires` 都有来源），否则 `Validate()`
// 报的就不是 nil，读路也在「缺来源」那一步就返回，根本走不到读**名额**的流式校验
// ——那正是被写的字段，用例就白跑了（第一版就是这么白跑的：变异对照 M1 存活）。
//
// 指针交接之后那次 `time.Sleep` 也是必要的：要让读者的读与写**时间上重叠**而又**不建立
// 同步**（写成「等读者读完再开始跑」会把这两者同步起来，竞态反而消失）。
func hookupRace(t *testing.T, read func(*Graph) error) {
	t.Helper()

	childOut := NewKey[int]("hk.cout")
	out := NewKey[int]("hk.out")
	stream := NewKey[<-chan int]("hk.stream")

	g := mustNew(t, context.Background(), "hookup", WithMaxRunning(2))

	// 子图在装配期就建好——静态形态挂的是现成的图；出口要先声明成端口。
	child := mustNew(t, context.Background(), "hookup/step")
	if err := child.DeclareOutput(NewPort(childOut)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	if err := Produce(child, "p", stream, func(rc *RunCtx, send func(int) error) error {
		return send(1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := Consume(child, "c", stream, func(rc *RunCtx, v int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, child, NewNode("pass", "", nil, Provides(childOut), func(rc *RunCtx) error {
		return Set(rc, childOut, 1)
	}))
	if err := Sub(g, "step", "", []SubBind{Out(childOut, out)}, child); err != nil {
		t.Fatal(err)
	}

	children := make(chan *Graph) // 无缓冲：把子图指针交出去，读者先拿到指针
	done := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex // 保护 reads / lastErr（测试自己的账）
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
				err := read(child)
				mu.Lock()
				reads++
				lastErr = err
				mu.Unlock()
			}
		}
	}()
	children <- child // 交给别的 goroutine —— 就是评审说的那种用法
	// 让读者先跑起来；读与写要时间上重叠，这两步之间又不建立同步。
	time.Sleep(20 * time.Millisecond)

	err := g.Run()
	close(done)
	wg.Wait()

	mu.Lock()
	n, verr := reads, lastErr
	mu.Unlock()
	// 读必须真的发生过，而且真的走到了读挂接字段那一步——否则这条用例是空转的。
	if n < 10 {
		t.Fatalf("只读入口只被调了 %d 次，没盖住挂接窗口", n)
	}
	if verr != nil {
		t.Fatalf("只读入口在挂接期间报错（说明它没走到挂接字段那一步）：%v", verr)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// TestSubHookupRaceWithReadOnlyEntry 走 `Validate()` 这条读路（它读名额）。
//
// 变异对照：把 `adopt` 里的 `g.mu.Lock()/Unlock()` 去掉 → 本用例在 `-race` 下必红
// （`.workbase/mutation_review2.py` 的 M1）。
func TestSubHookupRaceWithReadOnlyEntry(t *testing.T) {
	hookupRace(t, func(child *Graph) error { return child.Validate() })
}

// TestSubHookupRaceWithTopology 走 `Topology()` 这条读路——#308 新加的「任何时候可调」
// 那个承诺同样读挂接字段（observer / 名额），评审在 #308 的行内意见点名的就是它。
func TestSubHookupRaceWithTopology(t *testing.T) {
	hookupRace(t, func(child *Graph) error {
		_ = child.Topology()
		return nil
	})
}
