package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// 变异对照：把递归那一层（`child.problemsWithStreamsAt` / `child.topologyAt` 的自加锁）换回直接读字段
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

// 绑定写错的三种，都要在**装配期**（`Sub` 调用那一刻）报出来，而不是静默什么都不发生。
func TestSubBindMistakesAreLoud(t *testing.T) {
	// 1) 子图没声明这条键 → 它不是这张图的端口。
	in := NewKey[string]("s.bm.in")
	missing := NewKey[string]("s.bm.missing")
	g := mustNew(t, context.Background(), "P")
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	bare := mustNew(t, context.Background(), "P/missing")
	mustAdd(t, bare, NewNode("n", "", nil, nil, func(rc *RunCtx) error { return nil }))
	err := Sub(g, "missing", "", []SubBind{In(in, missing)}, bare)
	if err == nil {
		t.Fatal("子图没声明这条键，装配期就该拒")
	}
	if !strings.Contains(err.Error(), "is not a input port") ||
		!strings.Contains(err.Error(), "s.bm.missing") {
		t.Fatalf("报文要点明「%s 不是这张图的输入端口」，got %v", missing.Name(), err)
	}

	// 2) 名字对上了、T 没对上：绑定说这一端是 string，子图报名的端口是 int。
	//    （父侧与子侧写同一个 T 时编译期就红，所以这里要**同名不同 T**才到得了检查
	//    那一刻——YAML 那条路只有装配期能比，兜底是同一条。）
	same := NewKey[string]("s.bm.same")
	childInt := NewKey[int]("s.bm.same")
	typed := mustNew(t, context.Background(), "P2/typed")
	if err := typed.DeclareInput(NewPort(childInt)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	declareIn(t, typed, NewPort(childInt))
	if err := seedOn(typed, childInt, 1); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, typed, NewNode("n", "", Requires(childInt), nil, func(rc *RunCtx) error { return nil }))
	g2 := mustNew(t, context.Background(), "P2")
	declareIn(t, g2, NewPort(same))
	if err := seedOn(g2, same, "v"); err != nil {
		t.Fatal(err)
	}
	err = Sub(g2, "typed", "", []SubBind{In(same, NewKey[string]("s.bm.same"))}, typed)
	if err == nil || !strings.Contains(err.Error(), "type mismatch") {
		t.Fatalf("Sub = %v，want 「类型对不上」", err)
	}

	// 3) 箭头写反：把 Out 当成「父侧在前」写（`Out(父, 子)`），子侧那一端就指向了
	//    子图不认识的键——与 1) 同一个兜底接住，不会静默接反。
	cout := NewKey[string]("s.bm.cout")
	pout := NewKey[string]("s.bm.out")
	rev := mustNew(t, context.Background(), "P3/reversed")
	if err := rev.DeclareInput(NewPort(in)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := rev.DeclareOutput(NewPort(cout)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	mustAdd(t, rev, NewNode("n", "", Requires(in), Provides(cout),
		func(rc *RunCtx) error { return Set(rc, cout, "x") }))
	g3 := mustNew(t, context.Background(), "P3")
	err = Sub(g3, "reversed", "", []SubBind{In(in, in), Out(pout, cout)}, rev)
	if err == nil || !strings.Contains(err.Error(), "is not a output port") ||
		!strings.Contains(err.Error(), "s.bm.out") {
		t.Fatalf("Sub = %v，want 「箭头写反了」被兜住，且报文点到写反的那个键", err)
	}
}

// 节点 id 里的 `/` 是 path 的层级分隔符：装配期就拒——否则 `a/b` 与「a 里嵌 b」
// 拼出**同一条** path（实测两条都是 `"a/b"`，连 GraphID 也一样），层级再也拆不回来。
func TestSubRejectsSlashInNodeID(t *testing.T) {
	in := NewKey[string]("s.slash.in")
	cin := NewKey[string]("s.slash.cin")
	cout := NewKey[string]("s.slash.cout")
	out := NewKey[string]("s.slash.out")

	child := mustNew(t, context.Background(), "P/a")
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := child.DeclareOutput(NewPort(cout)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	mustAdd(t, child, NewNode("n", "", Requires(cin), Provides(cout),
		func(rc *RunCtx) error { return Set(rc, cout, "x") }))

	g := mustNew(t, context.Background(), "P")
	err := Sub(g, "a/b", "", []SubBind{In(in, cin), Out(cout, out)}, child)
	if err == nil {
		t.Fatal("节点 id 里的 / 会让 path 的层级拆不开，装配期就该拒")
	}
	if !strings.Contains(err.Error(), "must not contain '/'") {
		t.Fatalf("要说明为什么拒，got %v", err)
	}
	// 想表达「更深一层」的话，嵌套本来就能写；别的节点 id 含 `/` 不受影响。
	// （被拒的那次没碰过子图，所以这张子图照样能挂。）
	if err := Sub(g, "a", "", []SubBind{In(in, cin), Out(cout, out)}, child); err != nil {
		t.Fatalf("正常的 id 不该被拒：%v", err)
	}
}

// 同一个进口被**喂两次**：第二次那笔会被幂等首写静默吃掉（「值不见了」而没有报错）。
// 这是**读一遍声明就能定**的事，所以装配期就吵出来，三个入口判据同源：`Sub`、
// `Validate`、`Topology().Problems`（共用 checkBinds）。
//
// 阶段 2 之前这里还有第二种形状——「子图自己 `Seed` 过这条键」——那条随 `Seed` 搬家
// 消失了（输入属于**一次运行**，不再能往定义里垫默认值）；现在给子图一条默认值的两个
// 合法出口是：子图内部放一个产出它的节点，或挂载点上用 `Fix` 给一个**属于这一步**的
// 常量（后者与 `In` 撞在同一条进口上时，就是下面第一条用例）。
func TestSubRejectsDoubleFedInlet(t *testing.T) {
	in := NewKey[string]("s.cs.in")
	cin := NewKey[string]("s.cs.cin")
	cout := NewKey[string]("s.cs.cout")
	out := NewKey[string]("s.cs.out")

	child := func(t *testing.T) *Graph {
		t.Helper()
		c := mustNew(t, context.Background(), "P/step1")
		if err := c.DeclareInput(NewPort(cin)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := c.DeclareOutput(NewPort(cout)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		mustAdd(t, c, NewNode("n", "", Requires(cin), Provides(cout),
			func(rc *RunCtx) error {
				v, err := Get(rc, cin)
				if err != nil {
					return err
				}
				return Set(rc, cout, v)
			}))
		return c
	}

	for _, tc := range []struct {
		name  string
		binds func() []SubBind
		want  string
	}{
		{
			name:  "Fix 与 In 打在同一条进口上",
			binds: func() []SubBind { return []SubBind{In(in, cin), Fix(cin, "这一步的常量"), Out(cout, out)} },
			want:  "bound twice",
		},
		{
			name:  "同一个进口被 In 绑两次",
			binds: func() []SubBind { return []SubBind{In(in, cin), In(in, cin), Out(cout, out)} },
			want:  "bound twice",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := mustNew(t, context.Background(), "P")
			declareIn(t, g, NewPort(in))
			if err := seedOn(g, in, "父侧的值"); err != nil {
				t.Fatal(err)
			}
			err := Sub(g, "step1", "", tc.binds(), child(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), cin.Name()) {
				t.Fatalf("Sub = %v，want 装配期就报「%s」且点到 %s", err, tc.want, cin.Name())
			}
		})
	}
}

// 装配期那条判据看不见的一种：`Sub` 之后子图**自己长出了**产出那条进口的节点。
// `Sub` 那一刻放行（子图里还没有这个来源），轮到 `Validate` 判——装完之后同一份判据
// 照样跑，不该拖到运行期。
//
// （旧版本这一条用的是「`Sub` 之后才 `Seed` 子图那条键」；`Seed` 不再碰定义之后，
// 能改变「子图内部有没有来源」的只剩 `Add` 这一个入口。）
// 挂上之后再让子图内部长出「产出那条进口」的节点：这一次 `Add` 本身就被拒——子图的进口面
// （`DeclareInput`）与产出面（节点的 `Provides`）互斥，那条键已经有来源了。
//
// 这条以前守的是「父图 `Validate` 能抓到挂载之后子图才变的情况」（`problemsSubAtLocked`
// 把接线重新核一遍）：那个形状现在建不出来，判据提前到了声明那一刻（见
// `TestInputPortAndProducerConflict`）。父侧那遍重核仍在（子图的端口在挂载之后、它自己封口
// 之前还能改），但它今天已经没有可构造的失败形状了——**这一段没有变异对照**，如实记在这里，
// 不当作已覆盖。
func TestLateChildProviderIsRejectedAtAssembly(t *testing.T) {
	in := NewKey[string]("s.vs.in")
	cin := NewKey[string]("s.vs.cin")
	cout := NewKey[string]("s.vs.cout")
	out := NewKey[string]("s.vs.out")

	g := mustNew(t, context.Background(), "P")
	child := mustNew(t, context.Background(), "P/step")
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatal(err)
	}
	if err := child.DeclareOutput(NewPort(cout)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, child, NewNode("n", "", Requires(cin), Provides(cout),
		func(rc *RunCtx) error {
			v, err := Get(rc, cin)
			if err != nil {
				return err
			}
			return Set(rc, cout, v)
		}))
	if err := Sub(g, "step", "", []SubBind{In(in, cin), Out(cout, out)}, child); err != nil {
		t.Fatal(err)
	}
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "父侧的值"); err != nil {
		t.Fatal(err)
	}
	// 挂上之后再让子图内部长出「产出那条进口」的节点——`Sub` 那一刻看不见它，但 `Add` 看得见
	// 那条进口声明：当场拒。
	err := child.Add(NewNode("prod", "", nil, Provides(cin), func(rc *RunCtx) error {
		return Set(rc, cin, "子图自己的默认值")
	}))
	if err == nil {
		t.Fatal("子图内部长出「产出已声明进口」的节点：装配期就该拒")
	}
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("Add = %v，want 撞来源冲突（%s）", err, ErrDuplicateSource)
	}
	for _, want := range []string{cin.Name(), `"P/step"`, `"prod"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("报文该含 %q，got %v", want, err)
		}
	}
	// 失败的那次 Add 不留痕：整棵树照旧校验通过、跑得通，父侧那条值照旧桥回。
	if err := g.Validate(); err != nil {
		t.Fatalf("失败的 Add 写脏了定义：%v", err)
	}
	if err := runOn(g); err != nil {
		t.Fatalf("跑：%v", err)
	}
	if st, v := slotOn(g, out.asRef()); st != slotReady || v != "父侧的值" {
		t.Fatalf("out = (%v, %v)，want (ready, 父侧的值)", st, v)
	}
}

// 装配期参数校验（与 Join / FanOut 同形）。
func TestSubAssemblyErrors(t *testing.T) {
	in := NewKey[string]("s.ae.in")
	cin := NewKey[string]("s.ae.cin")
	g := mustNew(t, context.Background(), "P")
	child := mustNew(t, context.Background(), "P/x")
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}

	if err := Sub(nil, "x", "", []SubBind{In(in, cin)}, child); err == nil {
		t.Fatal("nil graph 应当报错")
	}
	if err := Sub(g, "", "", []SubBind{In(in, cin)}, child); err == nil {
		t.Fatal("空 id 应当报错")
	}
	if err := Sub(g, "x", "", []SubBind{In(in, cin)}, nil); err == nil {
		t.Fatal("nil child 应当报错")
	}
	if err := Sub(g, "x", "", nil, child); err == nil {
		t.Fatal("没有绑定应当报错")
	}
	// 同一条子侧键绑两次：第二次种值会被静默忽略，装配期就拒（所以子图先把这条
	// 声明成端口，否则先撞上的是「不是端口」那条）。
	if err := Sub(g, "x", "", []SubBind{In(in, cin), In(in, cin)}, child); err == nil {
		t.Fatal("子侧键绑两次应当报错")
	}
	// 零值 SubBind（手写 SubBind{} 而不是用 In / Out）
	if err := Sub(g, "x", "", []SubBind{{}}, child); err == nil {
		t.Fatal("零值 bind 应当报错")
	}
	if n := len(g.nodes); n != 0 {
		t.Fatalf("装配失败不该往图上留节点，got %d", n)
	}
}
