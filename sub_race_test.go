package pulse

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// hookupRace 是这条 race 用例的**共用形状**：子图在装配期建好之后，宿主直接把指针交给
// 另一个 goroutine（完全可以这么干），那头在父图跑的同时不停调只读入口。
//
// **这条用例守的命题在阶段 2 变了**，写清楚免得后来人把它当看门人用错地方：
//
//   - 阶段 1 撞的是运行期挂接字段（`child.adopt` 那批：observer / 名额 / 图级切面）——
//     那时候子图的定义字段会在**运行期**被写，与只读入口是真竞态，且去掉 `adopt` 的锁
//     就能让它变红（M1）。
//   - 阶段 2 把那批字段搬进了子执行，**运行期只剩封口一处会写定义**（`sealTreeLocked`
//     沿挂载边递归写每一层的 `sealed`）。而封口与改定义入口共用全局装配锁，两者本就互斥；
//     只读入口又不读 `sealed`——所以今天**没有**一条变异能把这它变成红的（实测：把封口那
//     一层改成直接写 `n.child.sealed`，这条用例照样绿，因为读路读的是节点 / 端口 / 名额
//     那批字段，与 `sealed` 不是同一个地址）。
//   - 那它还有什么用：**回归底网**。谁再往运行期加一处「写自己的定义」（这正是阶段 2 拆掉
//     的那类形状），只要读路读得到那个字段，这里就会在 `-race` 下响。
//
// 子图是**自足的**（没有 `in:` 绑定、每条 `Requires` 都有来源），否则 `Validate()`
// 报的就不是 nil，读路在「缺来源」那一步就返回，走不到递归进子图的那几段，用例就白跑了
// （第一版就是这么白跑的：变异对照 M1 存活）。
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

	err := runOn(g)
	close(done)
	wg.Wait()

	mu.Lock()
	n, verr := reads, lastErr
	mu.Unlock()
	// 读必须真的发生过——否则这条用例是空转的。
	if n < 10 {
		t.Fatalf("只读入口只被调了 %d 次，没盖住封口那个窗口", n)
	}
	if verr != nil {
		t.Fatalf("只读入口在运行期间报错（说明它没走到递归读子图那一步）：%v", verr)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// TestSubHookupRaceWithReadOnlyEntry 走 `Validate()` 这条读路（它递归读子图、并算声明名额）。
func TestSubHookupRaceWithReadOnlyEntry(t *testing.T) {
	hookupRace(t, func(child *Graph) error { return child.Validate() })
}

// TestSubHookupRaceWithTopology 走 `Topology()` 这条读路——#308 新加的「任何时候可调」
// 那个承诺同样递归读整棵子树，评审在 #308 的行内意见点名的就是它。
func TestSubHookupRaceWithTopology(t *testing.T) {
	hookupRace(t, func(child *Graph) error {
		_ = child.Topology()
		return nil
	})
}

// TestSubChildAssemblyVsParentReadOnly 走的是另一条缝：**父图的只读入口要递归读子图**，
// 而子图在装配期是宿主手里的对象。那头一边 `Add`（往子图里接着装），这头一边
// `parent.Topology()`——两边必须共用**子图**那把锁。父图那把锁管不到子图：`Topology()`
// 先锁父图，再沿挂载边往下走，**每往下走一层都要取那一层的锁**；少了任何一层，-race 就会
// 在这里报 DATA RACE（父图锁着自己、读别人正在写的字段）。
//
// 读路只走 `Topology()`：`Validate()` 通过会封口，子图一封就没法再 `Add`，竞态也就没得跑了。
//
// 变异对照：把递归那一层（`child.problemsAt` / `child.topologyAt` 的自加锁）换回直接读字段
// → 本用例在 `-race` 下必红。
func TestSubChildAssemblyVsParentReadOnly(t *testing.T) {
	in := NewKey[string]("cra.in")
	out := NewKey[string]("cra.out")
	pin := NewKey[string]("cra.pin")
	pout := NewKey[string]("cra.pout")

	child := mustNew(t, context.Background(), "cra.child")
	if err := child.DeclareInput(NewPort(in)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := child.DeclareOutput(NewPort(out)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	mustAdd(t, child, NewNode("make", "", Requires(in), Provides(out), func(rc *RunCtx) error { return nil }))

	parent := mustNew(t, context.Background(), "cra.parent")
	if err := parent.DeclareInput(NewPort(pin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := Sub(parent, "mount", "", []SubBind{In(pin, in), Out(out, pout)}, child); err != nil {
		t.Fatalf("Sub = %v", err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // 拿到子图指针的那一头：接着往子图里装
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			k := NewKey[string](fmt.Sprintf("cra.late%d", i))
			_ = child.Add(NewNode(fmt.Sprintf("late%d", i), "", nil, Provides(k), func(*RunCtx) error { return nil }))
		}
	}()

	deadline := time.Now().Add(300 * time.Millisecond)
	reads := 0
	for time.Now().Before(deadline) {
		_ = parent.Topology()
		reads++
	}
	close(done)
	wg.Wait()
	// 读必须真的发生过，否则这条用例是空转的
	if reads < 100 {
		t.Fatalf("只读入口只被调了 %d 次，没盖住装配窗口", reads)
	}
}

// checkBinds 读的是**子图**的端口表，而那张表在子图封口 / 启动之前一直受理写入
// （`DeclareInput` / `DeclareOutput`）。所以那几次查表必须在**子图那把锁**里做完：
// 解锁之后再去查 `g.inPorts`，就是读一张别人正在写的 map——实测 `-race` 报的正是
// `declarePorts`（写）↔ `checkBinds`（读）这一处，且可能直接落到 runtime 的
// concurrent map read and map write（那是 fatal，不是可恢复的 panic）。
//
// 读侧走 `Topology()`（它递归到 `problemsSubAtLocked` → `checkBinds`），写侧不停往
// 子图端口表里加新端口：两条路都不建立同步，所以没加锁时竞态必然现形。
func TestCheckBindsReadsChildPortsUnderChildLock(t *testing.T) {
	child := mustNew(t, context.Background(), "cbr.child")
	ck := NewKey[string]("cbr.child.k")
	if err := child.DeclareInput(NewPort(ck)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	parent := mustNew(t, context.Background(), "cbr")
	in := NewKey[string]("cbr.in")
	if err := Sub(parent, "step", "", []SubBind{In(in, ck)}, child); err != nil {
		t.Fatalf("Sub = %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50000; i++ {
			select {
			case <-stop:
				return
			default:
			}
			name := fmt.Sprintf("cbr.extra%d", i)
			if err := child.DeclareInput(NewPort(NewKey[string](name))); err != nil {
				return
			}
		}
	}()

	reads := 0
	for i := 0; i < 5000; i++ {
		_ = parent.Topology()
		reads++
	}
	close(stop)
	wg.Wait()
	// 读必须真的发生过，否则这条用例是空转的
	if reads < 100 {
		t.Fatalf("递归校验只被调了 %d 次，没盖住写窗口", reads)
	}
}
