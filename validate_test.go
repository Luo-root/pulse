package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 装配期可以先问一句「这张图跑得起来吗」：Validate 跑的是 Start 那套只读判据
// （每个 Requires 都有来源 / 依赖无环 / 流式名额与出口），但**不改图的状态**
// ——不置 started、不建 goroutine，所以问完还能接着装、接着跑。
//
// 它存在的理由在装配器那一侧：声明式装配（pulse/yaml）里的子图是运行到才建的，
// 没有这个入口，「子图里这条键没人提供」与「子图里依赖成环」就只能等它第一次
// 运行才由引擎拒掉——而这两条读一遍声明就能定。
func TestValidateReportsWhatStartWouldReject(t *testing.T) {
	x := NewKey[int]("val.x")
	y := NewKey[int]("val.y")

	t.Run("依赖成环：与 Start 报同一句话", func(t *testing.T) {
		g := mustNew(t, context.Background(), "test")
		mustAdd(t, g, NewNode("A", "", Requires(y), Provides(x), func(rc *RunCtx) error { return nil }))
		mustAdd(t, g, NewNode("B", "", Requires(x), Provides(y), func(rc *RunCtx) error { return nil }))

		verr := g.Validate()
		if verr == nil {
			t.Fatal("Validate 放行了一张含环的图")
		}
		if !strings.Contains(verr.Error(), "dependency cycle") {
			t.Fatalf("环要报出成环的那条路径，got %v", verr)
		}
		// 一份判据、两个入口：两边的报文逐字相同，装图期提前报的那句才不需要翻译
		serr := startOn(g)
		if serr == nil || serr.Error() != verr.Error() {
			t.Fatalf("Validate 与 Start 的判据不一致：\nValidate %v\nStart    %v", verr, serr)
		}
		// 校验不改状态：图仍是「没启动」，Wait 照报 ErrGraphNotStarted
		if werr := waitOn(g); !errors.Is(werr, ErrGraphNotStarted) {
			t.Fatalf("Validate 把图启动起来了？Wait() = %v", werr)
		}
	})

	t.Run("无来源的 Requires", func(t *testing.T) {
		g := mustNew(t, context.Background(), "test")
		mustAdd(t, g, NewNode("waiter", "", Requires(kA), nil, func(rc *RunCtx) error { return nil }))
		err := g.Validate()
		if err == nil {
			t.Fatal("Validate 放行了一张等不到值的图")
		}
		if !strings.Contains(err.Error(), `"waiter"`) || !strings.Contains(err.Error(), `nothing provides it`) {
			t.Fatalf("错误里要能读到节点 id 与病因，got %v", err)
		}
		// 补上来源（这正是装配器要留出的余地：问一次不该把图问死）
		declareIn(t, g, NewPort(kA))
		if err := seedOn(g, kA, "seeded"); err != nil {
			t.Fatalf("Validate 之后补 Seed 被拒：%v（说明它改了状态）", err)
		}
		if err := g.Validate(); err != nil {
			t.Fatalf("补上来源后应通过：%v", err)
		}
	})

	t.Run("通过即封口：定义不再变，但运行照旧", func(t *testing.T) {
		g := mustNew(t, context.Background(), "test")
		// 这条输入是**声明的入口**：它让「没人在图里提供 kA」不再算毛病
		if err := g.DeclareInput(NewPort(kA)); err != nil {
			t.Fatalf("声明入口失败：%v", err)
		}
		mustAdd(t, g, NewNode("n", "", Requires(kA), Provides(kB), func(rc *RunCtx) error {
			v, err := Get(rc, kA)
			if err != nil {
				return err
			}
			return Set(rc, kB, v)
		}))
		if err := g.Validate(); err != nil {
			t.Fatalf("干净的图不该被 Validate 拒：%v", err)
		}
		// 封口 = 定义不再变化：补节点被拒，而且是 ErrGraphStarted 这个哨兵
		err := g.Add(NewNode("m", "", Requires(kB), nil, func(rc *RunCtx) error { return nil }))
		if !errors.Is(err, ErrGraphStarted) {
			t.Fatalf("封口后 Add 的错应是 ErrGraphStarted，got %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "sealed") {
			t.Fatalf("封口后 Add 的报错该说清是定义封口，got %v", err)
		}
		// Seed 是**本次运行**的值，不属于定义：封口后照样能喂、能跑（`kA` 在开头就
		// 声明成入口了，封口不会再拦它）
		if err := seedOn(g, kA, "from-host"); err != nil {
			t.Fatalf("封口后 Seed 被拒：%v（Seed 是本次运行的值，不属于定义）", err)
		}
		if err := runOn(g); err != nil {
			t.Fatalf("封口之后照常跑：%v", err)
		}
		// 已经启动的图直接通过：它在 Start 那一刻就过了同一套判据
		if err := g.Validate(); err != nil {
			t.Fatalf("已跑完的图 Validate = %v, want nil", err)
		}
	})

	t.Run("校验失败不封口：补上缺的那块再校验正是要支持的用法", func(t *testing.T) {
		g := mustNew(t, context.Background(), "test")
		mustAdd(t, g, NewNode("waiter", "", Requires(kA), nil, func(rc *RunCtx) error { return nil }))
		if err := g.Validate(); err == nil {
			t.Fatal("缺来源的图不该过校验")
		}
		// 失败之后**没有**封口：补一个提供者就能接着装、接着校验
		mustAdd(t, g, NewNode("feeder", "", nil, Provides(kA), func(rc *RunCtx) error {
			return Set(rc, kA, "v")
		}))
		if err := g.Validate(); err != nil {
			t.Fatalf("补上提供者后应通过：%v", err)
		}
	})

	// passThrough 是三层嵌套的一棵树：root → mid → leaf，每层都是「入口 → 出口」。
	// 挂上去的子图是**父图这张定义的一部分**，所以封口封的是整棵子树（见 Graph.Validate）。
	passThrough := func(t *testing.T) (root, mid, leaf *Graph, keys map[string]Key[string]) {
		t.Helper()
		keys = map[string]Key[string]{}
		k := func(name string) Key[string] {
			if _, ok := keys[name]; !ok {
				keys[name] = NewKey[string](name)
			}
			return keys[name]
		}
		ports := func(id string) *Graph {
			t.Helper()
			g := mustNew(t, context.Background(), id)
			if err := g.DeclareInput(NewPort(k(id + ".in"))); err != nil {
				t.Fatalf("%s.DeclareInput = %v", id, err)
			}
			if err := g.DeclareOutput(NewPort(k(id + ".out"))); err != nil {
				t.Fatalf("%s.DeclareOutput = %v", id, err)
			}
			return g
		}
		// 只有最内层有真正干活的节点，中间两层各只有「挂子图」那一步——
		// 于是每层那条出口的来源都是它自己的挂载节点，不会跟别的节点撞来源。
		leaf = ports("seal.leaf")
		mustAdd(t, leaf, NewNode("step", "", Requires(k("seal.leaf.in")), Provides(k("seal.leaf.out")), func(rc *RunCtx) error {
			v, err := Get(rc, k("seal.leaf.in"))
			if err != nil {
				return err
			}
			return Set(rc, k("seal.leaf.out"), v)
		}))
		mid = ports("seal.mid")
		if err := Sub(mid, "inner", "", []SubBind{
			In(k("seal.mid.in"), k("seal.leaf.in")),
			Out(k("seal.leaf.out"), k("seal.mid.out")),
		}, leaf); err != nil {
			t.Fatalf("mid 挂 leaf = %v", err)
		}
		root = ports("seal.root")
		if err := Sub(root, "outer", "", []SubBind{
			In(k("seal.root.in"), k("seal.mid.in")),
			Out(k("seal.mid.out"), k("seal.root.out")),
		}, mid); err != nil {
			t.Fatalf("root 挂 mid = %v", err)
		}
		return root, mid, leaf, keys
	}

	t.Run("通过即封口：整棵子树一起封（挂上去的子图也是这张定义的一部分）", func(t *testing.T) {
		root, mid, leaf, keys := passThrough(t)
		if err := root.Validate(); err != nil {
			t.Fatalf("干净的图树不该被 Validate 拒：%v", err)
		}
		for _, g := range []*Graph{root, mid, leaf} {
			err := g.Add(NewNode("late", "", nil, nil, func(rc *RunCtx) error { return nil }))
			if !errors.Is(err, ErrGraphStarted) {
				t.Fatalf("%s：挂上去的子图也在这次校验里，该一起封，got %v", g.ID(), err)
			}
			if err == nil || !strings.Contains(err.Error(), "sealed") {
				t.Fatalf("%s：报错该点明是定义封口，got %v", g.ID(), err)
			}
			if err := g.DeclareInput(NewPort(NewKey[string](g.ID() + ".late_in"))); !errors.Is(err, ErrGraphStarted) {
				t.Fatalf("%s：声明也该被拒，got %v", g.ID(), err)
			}
		}
		// 封口只管**定义**：这一次运行的值不属于定义，所以封口之后照样喂得进去、
		// 整棵树照样能跑。（旧版本这里还有半句「封口后子图上照样能 seed」——阶段 2
		// 起「往子图种值」这条入口整个消失：子图的输入由挂载点的绑定给，那是装配期的
		// 事，不是某一次运行的值。）
		if err := seedOn(root, keys["seal.root.in"], "from-host"); err != nil {
			t.Fatalf("封口后根图 Seed 被拒：%v（Seed 是本次运行的值，不属于定义）", err)
		}
		if err := runOn(root); err != nil {
			t.Fatalf("封口之后照常跑：%v", err)
		}
		// 同一张子图挂两处：两处都封得上（重复走到已经封住的那张上不报错、也不重复递归）
		other := mustNew(t, context.Background(), "seal.other")
		if err := other.DeclareInput(NewPort(keys["seal.leaf.in"])); err != nil {
			t.Fatalf("other.DeclareInput = %v", err)
		}
		if err := Sub(other, "again", "", []SubBind{
			In(keys["seal.leaf.in"], keys["seal.leaf.in"]),
			Out(keys["seal.leaf.out"], keys["seal.leaf.out"]),
		}, leaf); err != nil {
			t.Fatalf("已封口的子图照样能被别处挂：%v", err)
		}
		if err := other.Validate(); err != nil {
			t.Fatalf("other.Validate = %v", err)
		}
	})

	// 与上一条是**两个入口、同一条规则**：`Start()` 成功同样封整棵子树。少了它，持有子图
	// 指针的调用方能在「父图已经跑起来、子图还没轮到」之间接着改子图（实测 `Add` 与
	// `DeclareInput` 都进得去），于是真正跑的就不是观察与拓扑看到的那张图。
	t.Run("启动成功也封整棵子树：父图跑起来之后、子图还没轮到也改不了", func(t *testing.T) {
		root, mid, leaf, keys := passThrough(t)
		// 根侧那条入口由一个等信号的节点提供：子图这一步在 Start 之后不会马上跑起来，
		// 于是「父图已启动、子图还没开始」这个窗口是确定的，不用抢时序。
		release := make(chan struct{})
		mustAdd(t, root, NewNode("gate", "", nil, Provides(keys["seal.root.in"]),
			func(rc *RunCtx) error {
				<-release
				return Set(rc, keys["seal.root.in"], "v")
			}))
		if err := startOn(root); err != nil {
			t.Fatalf("Start = %v", err)
		}
		for _, g := range []*Graph{root, mid, leaf} {
			err := g.Add(NewNode("late", "", nil, nil, func(rc *RunCtx) error { return nil }))
			if !errors.Is(err, ErrGraphStarted) {
				t.Fatalf("%s：父图 Start 成功该连子图一起封（此刻它还没跑），got %v", g.ID(), err)
			}
		}
		if err := mid.DeclareInput(NewPort(NewKey[string]("seal.mid.late"))); !errors.Is(err, ErrGraphStarted) {
			t.Fatalf("mid.DeclareInput after Start = %v, want ErrGraphStarted", err)
		}
		close(release)
		if err := waitOn(root); err != nil {
			t.Fatalf("封了并不妨碍跑：Wait = %v", err)
		}
	})

	t.Run("校验失败时整棵都不封（补上缺的那块再校验正是要支持的用法）", func(t *testing.T) {
		ghost := NewKey[string]("sealfail.ghost")
		in := NewKey[string]("sealfail.in")
		out := NewKey[string]("sealfail.out")
		rootIn := NewKey[string]("sealfail.root_in")

		child := mustNew(t, context.Background(), "sealfail.child")
		if err := child.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := child.DeclareOutput(NewPort(out)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		mustAdd(t, child, NewNode("hungry", "", Requires(ghost), Provides(out), func(rc *RunCtx) error {
			return Set(rc, out, "v")
		}))

		root := mustNew(t, context.Background(), "sealfail.root")
		if err := root.DeclareInput(NewPort(rootIn)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := Sub(root, "mount", "", []SubBind{In(rootIn, in), Out(out, out)}, child); err != nil {
			t.Fatalf("Sub = %v", err)
		}

		err := root.Validate()
		if err == nil || !strings.Contains(err.Error(), "sealfail.ghost") {
			t.Fatalf("子图里那条缺来源该从根图报出来，got %v", err)
		}
		// 失败的校验**不留封口**——两层都还该能接着装（子图被顺手封住是这里最容易犯的错）
		for _, g := range []*Graph{root, child} {
			if err := g.Add(NewNode("after", "", nil, nil, func(rc *RunCtx) error { return nil })); err != nil {
				t.Fatalf("%s：校验失败不该封口，got %v", g.ID(), err)
			}
		}
		// 补上病根，再校验一次：这次整棵一起封
		declareIn(t, child, NewPort(ghost))
		if err := seedOn(child, ghost, "v"); err != nil {
			t.Fatalf("Seed = %v", err)
		}
		if err := root.Validate(); err != nil {
			t.Fatalf("补上来源之后该通过：%v", err)
		}
		for _, g := range []*Graph{root, child} {
			err := g.Add(NewNode("late", "", nil, nil, func(rc *RunCtx) error { return nil }))
			if !errors.Is(err, ErrGraphStarted) {
				t.Fatalf("%s：通过之后该封住，got %v", g.ID(), err)
			}
		}
	})
}

// `Validate()` 的两趟（判完整棵树 → 逐层封）必须在**同一个装配临界区**里：改定义的入口
// （`Add` / `DeclareInput` / `DeclareOutput` / `Sub`）与这里共用 `assemblyMu`，所以判过的
// 那棵树在封口那一刻不可能已经被改过。
//
// 少了这把锁会怎样（实测）：窗口里给挂着的子图加一个「要一条没人提供的输入」的节点，
// `Validate()` 返回 nil，却把**没校验过**的那份封上了——此后既改不了（坏节点留在里面），
// `Start()` 又重新报错。
//
// 判据是那条不变式：**`Validate()` 返回 nil ⇒ 刚封住的那棵树没有静态毛病**。
// 窗口靠 C2 那段大子图撑开：第一趟走到 C2 要花掉一整个校验的时间，而 C1 的锁在这段时间里
// 是空的，动刀几乎必然落进窗口（实测：改前第 0 轮就中；改后全部轮次干净）。
func TestValidateSealsOnlyWhatItChecked(t *testing.T) {
	const (
		trials = 10
		chain  = 3000
		gap    = 300 * time.Microsecond
	)
	for trial := 0; trial < trials; trial++ {
		// C1 是动刀的目标，C2 很大（第一趟在 C1 之后还要走很久）。
		c1 := mustNew(t, context.Background(), fmt.Sprintf("atomic.c1.%d", trial))
		k1 := NewKey[string](fmt.Sprintf("atomic.c1.k%d", trial))
		if err := c1.DeclareInput(NewPort(k1)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}

		c2 := mustNew(t, context.Background(), fmt.Sprintf("atomic.c2.%d", trial))
		head := NewKey[int](fmt.Sprintf("atomic.c2.k%d.0", trial))
		if err := c2.DeclareInput(NewPort(head)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		prev := head
		for i := 1; i <= chain; i++ {
			next := NewKey[int](fmt.Sprintf("atomic.c2.k%d.%d", trial, i))
			mustAdd(t, c2, NewNode(fmt.Sprintf("n%d", i), "",
				Requires(prev), Provides(next), func(rc *RunCtx) error { return nil }))
			prev = next
		}

		root := mustNew(t, context.Background(), fmt.Sprintf("atomic.root.%d", trial))
		pin1 := NewKey[string](fmt.Sprintf("atomic.p1.%d", trial))
		pin2 := NewKey[int](fmt.Sprintf("atomic.p2.%d", trial))
		if err := Sub(root, "s1", "", []SubBind{In(pin1, k1)}, c1); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		if err := Sub(root, "s2", "", []SubBind{In(pin2, head)}, c2); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		declareIn(t, root, NewPort(pin1))
		if err := seedOn(root, pin1, "v"); err != nil {
			t.Fatalf("Seed = %v", err)
		}
		declareIn(t, root, NewPort(pin2))
		if err := seedOn(root, pin2, 1); err != nil {
			t.Fatalf("Seed = %v", err)
		}

		// 动刀：给 C1 加一个「要一条没人提供的输入」的节点——加完 C1 就有静态毛病。
		bad := NewNode("bad", "", Requires(NewKey[string](fmt.Sprintf("atomic.missing.%d", trial))),
			nil, func(rc *RunCtx) error { return nil })
		attacked := make(chan error, 1)
		go func() {
			time.Sleep(gap)
			attacked <- c1.Add(bad)
		}()

		err := root.Validate()
		aerr := <-attacked // 先等动刀落地：下面那句读的是它的结果
		if err != nil {
			continue // 这轮动刀抢在了校验之前（判的时候子图已经有毛病）——窗口没被碰到
		}
		if aerr == nil {
			if probs := c1.Topology().Problems; len(probs) > 0 {
				t.Fatalf("第 %d 轮：Validate() 返回 nil，却把带毛病的子图封上了（%v）", trial, probs[0])
			}
		}
	}
}
