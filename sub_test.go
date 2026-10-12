package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// 图即节点（#293）：建图 / 桥接 / 取消域 / 观察者四件事都由 `Sub` 收齐。
//
// 这些用例对着探针实测的结论写（`.workbase/probe-subgraph`，五组实测已抄进票面）：
// 手工嵌套能做到的，糖必须做到，且**观测不能断链**——那是手工搭最容易漏、
// 又最没声音的一处。
//
// 形态：子图组合只有 `Sub` 一条路——子图提前到装配期建、接线只能接子图**声明过的
// 端口**、path / 图 id 由 `Topology()` 按挂载位置推导。
func TestSubBridgesValues(t *testing.T) {
	topic := NewKey[string]("s.topic")
	summary := NewKey[string]("s.summary")
	childIn := NewKey[string]("s.child.in")
	childOut := NewKey[string]("s.child.out")
	obs := &recordingObserver{}

	// 子图提前到装配期建好：id 沿用「父图 id + 本节点 id」这套写法；
	// 接线两端都必须是声明过的端口。
	child := mustNew(t, context.Background(), "parent/step1")
	if err := child.DeclareInput(NewPort(childIn)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := child.DeclareOutput(NewPort(childOut)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	mustAdd(t, child, NewNode("work", "", Requires(childIn), Provides(childOut),
		func(rc *RunCtx) error {
			v, err := Get(rc, childIn)
			if err != nil {
				return err
			}
			return Set(rc, childOut, "about "+v)
		}))

	g := mustNew(t, context.Background(), "parent", WithObserver(obs))
	declareIn(t, g, NewPort(topic))
	if err := seedOn(g, topic, "slot contract"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step1", "",
		[]SubBind{In(topic, childIn), Out(childOut, summary)}, child); err != nil {
		t.Fatal(err)
	}

	// 边界写在接线处：父侧那个节点声明什么，完全由 In / Out 的父侧端推出来。
	var sub *Node
	for _, n := range g.nodes {
		if n.id == "step1" {
			sub = n
		}
	}
	if sub == nil {
		t.Fatal("step1 没装上")
	}
	if len(sub.requires) != 1 || sub.requires[0].name != topic.Name() {
		t.Fatalf("Requires = %v，want 只有 %s", sub.requires, topic.Name())
	}
	if len(sub.provides) != 1 || sub.provides[0].name != summary.Name() {
		t.Fatalf("Provides = %v，want 只有 %s", sub.provides, summary.Name())
	}

	// path / 图 id 改由静态形状给：path 按**挂载位置**推（`step1`），图 id 是子图
	// 自己的（挂载不改写它）——不必先跑。
	top := g.Topology()
	if len(top.Nodes) != 1 {
		t.Fatalf("装完一步时父图该只有一个节点，got %+v", top.Nodes)
	}
	subTop := top.Nodes[0].Sub
	if subTop == nil {
		t.Fatal("挂载之后 Nodes[0].Sub 该是子图定义——不必先跑")
	}
	if subTop.Graph.ID != "parent/step1" {
		t.Fatalf("静态形状里的图 id = %q，want parent/step1", subTop.Graph.ID)
	}
	if subTop.Graph.Path != "step1" {
		t.Fatalf("一层子图的 path = %q，want step1", subTop.Graph.Path)
	}

	var bridged string
	if err := g.Add(NewNode("sink", "", Requires(summary), nil, func(rc *RunCtx) error {
		v, err := Get(rc, summary)
		if err != nil {
			return err
		}
		bridged = v
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if bridged != "about slot contract" {
		t.Fatalf("子图产物没有桥回父侧：%q", bridged)
	}
	// 观察者的继承发生在**建子执行那一刻**（有效配置算在 Execution 上，不回写子图），
	// 所以这里验「子图没挂观察者 → 本次运行继承了父图那份」：父图那份收到了带子位置的事件。
	if !obs.pathSeen("step1") {
		t.Fatal("子图没挂观察者，跑起来该继承到父图那份（建子执行时没接上）")
	}
}

// graphLog 按「哪张图 / 哪个节点 / 什么事件」留痕——嵌套观测要验归因，图 id 必须
// 一起记下来（包内那个 recordingObserver 只记节点 id）。
type graphLog struct {
	mu   sync.Mutex
	rows []string
}

func (l *graphLog) add(s string) {
	l.mu.Lock()
	l.rows = append(l.rows, s)
	l.mu.Unlock()
}

func (l *graphLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.rows...)
}

func (l *graphLog) observer() Observer {
	return ObserverFunc{
		GraphStarted:  func(s Site) { l.add("start:" + pos(s)) },
		GraphFinished: func(s Site, r NodeFinishReason, _ error) { l.add("finish:" + pos(s) + "=" + string(r)) },
		Waiting:       func(s Site, n string) { l.add("wait:" + posNode(s, n)) },
		Running:       func(s Site, n string) { l.add("run:" + posNode(s, n)) },
		Finished:      func(s Site, n string, r NodeFinishReason, _ error) { l.add("done:" + posNode(s, n) + "=" + string(r)) },
	}
}

// pos 是这条身份在运行树里的**位置**：根执行以图 id 表示、子执行用它自己的挂载路径
// （阶段 2 起身份是 `Site`，「哪一次运行里的哪个位置」由 `Path` 承担）。留痕按它拼，
// 嵌套观测的括号结构一眼看得出。
func pos(s Site) string {
	if s.Path == "" {
		return s.Graph
	}
	return s.Path
}

// posNode 在 pos 之上补节点 id——挂载点那一步的节点属于**父**执行，它的子图节点
// 属于子执行，两边因此自动分开。
func posNode(s Site, nodeID string) string {
	if s.Path == "" {
		return nodeID
	}
	return s.Path + "/" + nodeID
}

// 观察者自动下传：子图**故意不挂**观察者，挂载那一步照样把父图那份接给它，
// 且两条流靠图 id 分得开——嵌套是一对括号（父图的两条把子图的两条夹在中间）。
func TestSubKeepsObservationChain(t *testing.T) {
	in := NewKey[string]("s.obs.in")
	cin := NewKey[string]("s.obs.cin")
	cout := NewKey[string]("s.obs.cout")
	out := NewKey[string]("s.obs.out")
	log := &graphLog{}
	g := mustNew(t, context.Background(), "P", WithObserver(log.observer()))

	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	child := mustNew(t, context.Background(), "P/step1")
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := child.DeclareOutput(NewPort(cout)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	mustAdd(t, child, NewNode("inner", "", Requires(cin), Provides(cout),
		func(rc *RunCtx) error {
			v, err := Get(rc, cin)
			if err != nil {
				return err
			}
			return Set(rc, cout, v+"→内层")
		}))
	if err := Sub(g, "step1", "", []SubBind{In(in, cin), Out(cout, out)}, child); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"start:P",
		"wait:step1", "run:step1",
		"start:step1",
		"wait:step1/inner", "run:step1/inner", "done:step1/inner=completed",
		"finish:step1=completed",
		"done:step1=completed",
		"finish:P=completed",
	}
	if got := log.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("嵌套观测的序列不对：\n got %v\nwant %v", got, want)
	}
}

// 子图自己挂了观察者就用它自己的——自动继承只填 nil，不覆盖。
func TestSubDoesNotClobberChildObserver(t *testing.T) {
	in := NewKey[string]("s.co.in")
	cin := NewKey[string]("s.co.cin")
	cout := NewKey[string]("s.co.cout")
	parentLog, childLog := &graphLog{}, &graphLog{}
	g := mustNew(t, context.Background(), "P", WithObserver(parentLog.observer()))

	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	child := mustNew(t, context.Background(), "P/step1", WithObserver(childLog.observer()))
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := child.DeclareOutput(NewPort(cout)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	mustAdd(t, child, NewNode("inner", "", Requires(cin), Provides(cout),
		func(rc *RunCtx) error { return Set(rc, cout, "y") }))
	if err := Sub(g, "step1", "", []SubBind{In(in, cin), Out(cout, NewKey[string]("s.co.out"))},
		child); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	// 父图的出口只看得见父图自己那几条（挂载点那一步属于父执行，子图里那条节点的
	// id 是 inner，图级两条是 start:step1 / finish:step1=completed）。两侧都是单节点图，
	// 序列完全确定。
	wantParent := []string{
		"start:P",
		"wait:step1", "run:step1", "done:step1=completed",
		"finish:P=completed",
	}
	if got := parentLog.snapshot(); !reflect.DeepEqual(got, wantParent) {
		t.Fatalf("父图的出口收到了不属于它的记录：\n got %v\nwant %v", got, wantParent)
	}
	wantChild := []string{
		"start:step1",
		"wait:step1/inner", "run:step1/inner", "done:step1/inner=completed",
		"finish:step1=completed",
	}
	if got := childLog.snapshot(); !reflect.DeepEqual(got, wantChild) {
		t.Fatalf("子图自己的出口内容不对：\n got %v\nwant %v", got, wantChild)
	}
}

// 子图失败 → 本节点失败，首错原样冒泡。
func TestSubChildFailureBubbles(t *testing.T) {
	boom := errors.New("子图内部失败")
	cout := NewKey[string]("s.bf.cout")
	out := NewKey[string]("s.bf.out")
	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "P", WithObserver(obs))

	child := mustNew(t, context.Background(), "P/step1")
	if err := child.DeclareOutput(NewPort(cout)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	mustAdd(t, child, NewNode("inner", "", nil, Provides(cout),
		func(rc *RunCtx) error { return boom }))
	if err := Sub(g, "step1", "", []SubBind{Out(cout, out)}, child); err != nil {
		t.Fatal(err)
	}
	err := runOn(g)
	if !errors.Is(err, boom) {
		t.Fatalf("Run = %v，want 子图的首错", err)
	}
	if n := countPref(obs.snapshot(), "F:step1:failed"); n != 1 {
		t.Fatalf("父侧那个节点应当是 failed：%v", obs.snapshot())
	}
}

// 父图取消 → 子图看得见。静态形态下子图那份 ctx 是它**建图时**带进来的，
// 靠挂载那一步把父节点这一层的取消转发进去（见 runStaticSub 的第 5 步）。
func TestSubChildSeesParentCancel(t *testing.T) {
	in := NewKey[string]("s.cx.in")
	cin := NewKey[string]("s.cx.cin")
	ctx, cancel := context.WithCancel(context.Background())
	g := mustNew(t, ctx, "P")
	seenc := make(chan error, 1)

	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	// 子图预建：ctx 与父图无关，取消只能靠转发。
	child := mustNew(t, context.Background(), "P/step1")
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	mustAdd(t, child, NewNode("slow", "", Requires(cin), nil, func(crc *RunCtx) error {
		select {
		case <-crc.Context().Done():
			seenc <- crc.Context().Err()
			return crc.Context().Err()
		case <-time.After(2 * time.Second):
			seenc <- nil
			return nil
		}
	}))
	if err := Sub(g, "step1", "", []SubBind{In(in, cin)}, child); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	err := runOn(g)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v，want context.Canceled", err)
	}
	select {
	case got := <-seenc:
		if !errors.Is(got, context.Canceled) {
			t.Fatalf("子图看到的是 %v，want context.Canceled（取消域没接上）", got)
		}
	case <-time.After(time.Second):
		t.Fatal("子图始终没退出")
	}
}

// 输出桥接的三种形态：就绪 → 父侧拿到值；部分跳过 → 父侧那条键跳过、节点 completed；
// 全部跳过 → 本节点以 skipped 收尾，整轮仍是成功。
func TestSubOutputSkipMapping(t *testing.T) {
	in := NewKey[string]("s.os.in")
	ready := NewKey[string]("s.os.ready")
	skipped := NewKey[string]("s.os.skipped")
	all := NewKey[string]("s.os.all")
	cReady := NewKey[string]("s.os.c.ready")
	cSkipped := NewKey[string]("s.os.c.skipped")
	cAll := NewKey[string]("s.os.c.all")

	// 每个挂载点各建一份子图（静态组合里一张子图只能跑一处）：结构相同，
	// 只是各自声明自己要拿出手的出口。
	buildChild := func(gid string, outs ...Key[string]) *Graph {
		child := mustNew(t, context.Background(), gid)
		if err := child.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		for _, k := range outs {
			if err := child.DeclareOutput(NewPort(k)); err != nil {
				t.Fatalf("DeclareOutput = %v", err)
			}
		}
		mustAdd(t, child, NewNode("w1", "", Requires(in), Provides(cReady, cSkipped),
			func(rc *RunCtx) error {
				if err := Set(rc, cReady, "值"); err != nil {
					return err
				}
				return Skip(rc, cSkipped)
			}))
		mustAdd(t, child, NewNode("w2", "", Requires(in), Provides(cAll),
			func(rc *RunCtx) error { return Skip(rc, cAll) }))
		return child
	}

	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "P", WithObserver(obs))
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "part", "", []SubBind{
		In(in, in), Out(cReady, ready), Out(cSkipped, skipped),
	}, buildChild("P/part", cReady, cSkipped)); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "none", "", []SubBind{In(in, in), Out(cAll, all)},
		buildChild("P/none", cAll)); err != nil {
		t.Fatal(err)
	}

	sawReady, sawSkipped, sawAllDownstream := false, false, false
	if err := g.Add(NewNode("useReady", "", Requires(ready), nil, func(rc *RunCtx) error {
		sawReady = true
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	// 跳过那条的下游要**带着另一条有值的输入**才会进 Run（门是「有没有值」）——
	// 进去之后再读那条被跳过的键，才是 *SkipError。
	if err := g.Add(NewNode("useSkipped", "", Deps(Requires(ready), Requires(skipped)), nil,
		func(rc *RunCtx) error {
			sawSkipped = true
			if _, err := Get(rc, skipped); !errors.Is(err, ErrSkipped) {
				return fmt.Errorf("父侧那条键应当是跳过到达，got %v", err)
			}
			return nil
		})); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("useAll", "", Requires(all), nil, func(rc *RunCtx) error {
		sawAllDownstream = true
		return nil
	})); err != nil {
		t.Fatal(err)
	}

	if err := runOn(g); err != nil {
		t.Fatalf("跳过是到达、不是失败：%v", err)
	}
	if !sawReady || !sawSkipped {
		t.Fatalf("部分跳过：就绪那条的下游要跑（%v）、带着跳过那条的下游也要跑（%v）",
			sawReady, sawSkipped)
	}
	if sawAllDownstream {
		t.Fatal("全部输出跳过的子图，下游不该执行")
	}
	log := obs.snapshot()
	if n := countPref(log, "F:part:completed"); n != 1 {
		t.Fatalf("部分跳过的那一步应 completed：%v", log)
	}
	if n := countPref(log, "F:none:skipped"); n != 1 {
		t.Fatalf("全部跳过的那一步应以 skipped 收尾：%v", log)
	}
}

// 输入侧的跳过：全部跳过 → 本节点根本不进入 Run（引擎的门判「有没有值」）；
// 一部分有值、一部分跳过 → 照常进 Run，那条跳过**照跳种**进子图。
func TestSubSkippedInputStaysSkipped(t *testing.T) {
	giver := NewKey[string]("s.si.give")
	cin := NewKey[string]("s.si.cin")
	cout := NewKey[string]("s.si.cout")
	pout := NewKey[string]("s.si.out")

	child := mustNew(t, context.Background(), "P/step1")
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := child.DeclareOutput(NewPort(cout)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	mustAdd(t, child, NewNode("inner", "", Requires(cin), Provides(cout),
		func(rc *RunCtx) error { return Set(rc, cout, "x") }))

	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "P", WithObserver(obs))
	if err := g.Add(NewNode("maybe", "", nil, Provides(giver),
		func(rc *RunCtx) error { return Skip(rc, giver) })); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step1", "", []SubBind{In(giver, cin), Out(cout, pout)}, child); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatalf("跳过是到达、不是失败：%v", err)
	}
	// 全部输入跳过时本节点不该进入 Run——静态形态下子图是**提前建好**的，所以这里
	// 钉「这一步根本没碰过它」：没为它建过执行（跑了就一定有一对带 step1 的图级事件）。
	if obs.pathSeen("step1") {
		t.Fatal("全部输入跳过时本节点不该进入 Run，子图也就不该被建执行 / 启动")
	}

	// —— 混合：一条有值、一条跳过 ——
	okIn := NewKey[string]("s.si2.ok")
	skipIn := NewKey[string]("s.si2.skip")
	cin2 := NewKey[string]("s.si2.cin")
	cSkip := NewKey[string]("s.si2.cskip")
	cout2 := NewKey[string]("s.si2.cout")
	pout2 := NewKey[string]("s.si2.out")

	var sawValue string
	var sawSkip error
	child2 := mustNew(t, context.Background(), "P2/step1")
	if err := child2.DeclareInput(NewPort(cin2)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := child2.DeclareInput(NewPort(cSkip)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := child2.DeclareOutput(NewPort(cout2)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	mustAdd(t, child2, NewNode("inner", "", Deps(Requires(cin2), Requires(cSkip)),
		Provides(cout2), func(rc *RunCtx) error {
			v, err := Get(rc, cin2)
			if err != nil {
				return err
			}
			sawValue = v
			_, sawSkip = Get(rc, cSkip)
			return Set(rc, cout2, v)
		}))

	obs2 := &recordingObserver{}
	g2 := mustNew(t, context.Background(), "P2", WithObserver(obs2))
	declareIn(t, g2, NewPort(okIn))
	if err := seedOn(g2, okIn, "有值"); err != nil {
		t.Fatal(err)
	}
	declareIn(t, g2, NewPort(skipIn))
	if err := skipSeedOn(g2, skipIn); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g2, "step1", "", []SubBind{
		In(okIn, cin2), In(skipIn, cSkip), Out(cout2, pout2),
	}, child2); err != nil {
		t.Fatal(err)
	}
	var bridged string
	if err := g2.Add(NewNode("sink", "", Requires(pout2), nil, func(rc *RunCtx) error {
		v, err := Get(rc, pout2)
		if err != nil {
			return err
		}
		bridged = v
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g2); err != nil {
		t.Fatalf("一条有值就该照常跑：%v", err)
	}
	if sawValue != "有值" {
		t.Fatalf("子图读到的是 %q，want 有值", sawValue)
	}
	if !errors.Is(sawSkip, ErrSkipped) {
		t.Fatalf("父侧跳过的那条键在子图里应当也是跳过到达，got %v", sawSkip)
	}
	if bridged != "有值" {
		t.Fatalf("桥回父侧的是 %q，want 有值", bridged)
	}
	// 对照组：这一侧确实建了子执行并跑起来（上面那句「没碰过它」才不是空话）。
	if !obs2.pathSeen("step1") {
		t.Fatal("一条有值一条跳过时，子图该照常跑")
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

// 并发的两个挂载点复用同一张子图：阶段 2 起这不是「抢认领」，而是**两条独立的运行**
// ——每个挂载位置各建一份子执行，所以两边都要跑成，各读自己的输入、各桥回自己的父侧
// 输出，且**不许有数据竞争**（定义只被读；槽位 / 取消域 / 首错全在各自的 Execution 上）。
//
// 起跑线那段是刻意留的：两个挂载点的切面先对齐再一起进 Run，让两边尽量同时碰到
// 「建子执行 + 跑子图」那一刻——旧实现在这里抢的是 `claimed` 那份共享状态，
// 现在两边碰的只是同一份**只读**定义，`-race` 就是判据。
//
// 跑多轮是因为窗口窄：一轮里两边可能恰好错开，多轮让「两边同时读到同一份定义」
// 这条路径有机会出现。
func TestSubConcurrentTwoMountsBothRun(t *testing.T) {
	inA := NewKey[string]("s.race.inA")
	inB := NewKey[string]("s.race.inB")
	cin := NewKey[string]("s.race.cin")
	cout := NewKey[string]("s.race.cout")
	outA := NewKey[string]("s.race.outA")
	outB := NewKey[string]("s.race.outB")

	for round := range 20 {
		// 共享子图：建一次，两个挂载点都把它交出去。
		shared := mustNew(t, context.Background(), "SHARED")
		if err := shared.DeclareInput(NewPort(cin)); err != nil {
			t.Fatal(err)
		}
		if err := shared.DeclareOutput(NewPort(cout)); err != nil {
			t.Fatal(err)
		}
		mustAdd(t, shared, NewNode("w", "", Requires(cin), Provides(cout),
			// 把入口原样吐到出口：父侧两个输出键各自拿到**自己那一份**才说明
			// 「两处各有一份状态」，而不是「碰巧都跑完了」。
			func(rc *RunCtx) error {
				v, err := Get(rc, cin)
				if err != nil {
					return err
				}
				return Set(rc, cout, v)
			}))

		// 起跑线：两个挂载点的切面都先到场再一起放行。带超时是为了万一只有一边跑到，
		// 也别把整个测试挂死（对齐只需「同时到」）。
		var arrive sync.WaitGroup
		arrive.Add(2)
		release := make(chan struct{})
		go func() {
			arrive.Wait()
			close(release)
		}()
		gate := Aspect(func(rc *RunCtx, next func(*RunCtx) error) error {
			arrive.Done()
			select {
			case <-release:
			case <-time.After(2 * time.Second):
			}
			return next(rc)
		})

		g := mustNew(t, context.Background(), "P")
		declareIn(t, g, NewPort(inA))
		if err := seedOn(g, inA, "a"); err != nil {
			t.Fatal(err)
		}
		declareIn(t, g, NewPort(inB))
		if err := seedOn(g, inB, "b"); err != nil {
			t.Fatal(err)
		}
		if err := Sub(g, "sA", "", []SubBind{In(inA, cin), Out(cout, outA)}, shared, gate); err != nil {
			t.Fatal(err)
		}
		if err := Sub(g, "sB", "", []SubBind{In(inB, cin), Out(cout, outB)}, shared, gate); err != nil {
			t.Fatal(err)
		}

		if err := runOn(g); err != nil {
			t.Fatalf("第 %d 轮：两处该都跑成（每个挂载位置一份子执行），got %v", round, err)
		}
		if _, v := slotOn(g, outA.asRef()); v != "a" {
			t.Fatalf("第 %d 轮：第一处 = %v，want a（各读自己的输入）", round, v)
		}
		if _, v := slotOn(g, outB.asRef()); v != "b" {
			t.Fatalf("第 %d 轮：第二处 = %v，want b（各读自己的输入）", round, v)
		}
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

// 两层嵌套：path 是节点 id 链、图 id 一层层接；这两条事实现在由 `Topology()`
// 按**挂载位置**推导（根图为空串，一层子图是挂它的那个节点 id）。
func TestSubNestedPath(t *testing.T) {
	in := NewKey[string]("s.np.in")
	cin := NewKey[string]("s.np.cin")
	cout := NewKey[string]("s.np.cout")
	pout := NewKey[string]("s.np.out")

	// 自底向上装配：grand 挂进 mid 的 "inner" 一步，mid 再挂进 g 的 "outer" 一步。
	grand := mustNew(t, context.Background(), "P/outer/inner")
	if err := grand.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := grand.DeclareOutput(NewPort(cout)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	mustAdd(t, grand, NewNode("leaf", "", Requires(cin), Provides(cout),
		func(rc *RunCtx) error {
			v, err := Get(rc, cin)
			if err != nil {
				return err
			}
			return Set(rc, cout, v+"→更深一层")
		}))

	mid := mustNew(t, context.Background(), "P/outer")
	if err := mid.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := mid.DeclareOutput(NewPort(cout)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	if err := Sub(mid, "inner", "", []SubBind{In(cin, cin), Out(cout, cout)}, grand); err != nil {
		t.Fatal(err)
	}

	g := mustNew(t, context.Background(), "P")
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "叶子"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "outer", "", []SubBind{In(in, cin), Out(cout, pout)}, mid); err != nil {
		t.Fatal(err)
	}

	var bridged string
	if err := g.Add(NewNode("sink", "", Requires(pout), nil, func(rc *RunCtx) error {
		v, err := Get(rc, pout)
		if err != nil {
			return err
		}
		bridged = v
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if bridged != "叶子→更深一层" {
		t.Fatalf("两层之后桥回父侧的是 %q", bridged)
	}

	// 第一层 path = outer；第二层 = outer/inner，图 id 一层层接（装配序：outer 在前）。
	top := g.Topology()
	outer := top.Nodes[0].Sub
	if outer == nil {
		t.Fatal("outer 那一步没有静态子树")
	}
	if outer.Graph.Path != "outer" {
		t.Fatalf("第一层 path = %q，want outer", outer.Graph.Path)
	}
	inner := outer.Nodes[0].Sub
	if inner == nil {
		t.Fatal("inner 那一步没有静态子树")
	}
	if inner.Graph.Path != "outer/inner" {
		t.Fatalf("第二层 path = %q，want outer/inner", inner.Graph.Path)
	}
	if inner.Graph.ID != "P/outer/inner" {
		t.Fatalf("第二层的图 id = %q，want P/outer/inner", inner.Graph.ID)
	}
}

// 自己声明就用自己的（覆盖继承）：父图 WithMaxRunning(1)，子图声明 WithMaxRunning(2)
// → 子图那一层换成它自己那份，两个节点必须能重叠。
// 「整棵树共享」是**默认**行为（见 limiter_test.go 的继承用例）；声明是显式覆盖。
func TestSubDeclaredLimitOverridesInheritance(t *testing.T) {
	in := NewKey[string]("s.slot.in")
	cin := NewKey[string]("s.slot.cin")

	g := mustNew(t, context.Background(), "P", WithMaxRunning(1))
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	inside, peak := 0, 0
	both := make(chan struct{})
	enter := func() {
		mu.Lock()
		inside++
		if inside == 2 {
			close(both)
		}
		mu.Unlock()
	}

	child := mustNew(t, context.Background(), "P/step1", WithMaxRunning(2))
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	for _, id := range []string{"w1", "w2"} {
		id := id
		mustAdd(t, child, NewNode(id, "", Requires(cin), nil, func(rc *RunCtx) error {
			enter()
			select {
			case <-both:
			case <-time.After(2 * time.Second):
				return errors.New("子图的两个节点没能重叠：声明了 WithMaxRunning(2) 就该用它自己那份")
			}
			mu.Lock()
			if inside > peak {
				peak = inside
			}
			mu.Unlock()
			return nil
		}))
	}
	if err := Sub(g, "step1", "", []SubBind{In(in, cin)}, child); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatalf("子图声明了自己的名额，嵌套应当跑得通：%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak != 2 {
		t.Fatalf("子图内部同时进 Run 的峰值 = %d，want 2（子图声明了 2，覆盖继承）", peak)
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
func TestValidateCatchesLateChildProvider(t *testing.T) {
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
	// 挂上之后再让子图内部长出「产出那条进口」的节点——`Sub` 那一刻看不见它。
	mustAdd(t, child, NewNode("prod", "", nil, Provides(cin), func(rc *RunCtx) error {
		return Set(rc, cin, "子图自己的默认值")
	}))
	err := g.Validate()
	if err == nil || !strings.Contains(err.Error(), `is provided inside the child by node "prod"`) {
		t.Fatalf("Validate = %v，want 装配期判据（子图内部有人产出它），不该拖到运行期", err)
	}
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("Validate = %v，want 那句点明撞的是来源冲突（%s）", err, ErrDuplicateSource)
	}
	// 同一份判据在静态毛病清单里也看得见（`problemsSubAtLocked` → `checkBinds`，判据同源）。
	probs := g.Topology().Problems
	if len(probs) == 0 || !strings.Contains(probs[0].Error(), cin.Name()) ||
		!strings.Contains(probs[0].Error(), "provided inside the child") {
		t.Fatalf("Topology().Problems = %v，want 点到 %s 的那条", probs, cin.Name())
	}
}

// 旧版本这里还有一条运行期兜底用例：`Seed` 落在 `Start` 之后、子图真跑起来之前，
// 装配期判据已经过完了，只剩 `runStaticSub` 那一步能拦。
//
// 阶段 2 起这个场景**不存在**，所以用例一并删掉，而不是改写成别的东西：
//   - 「往子图种值」这条入口没了（值属于**一次运行**，子图的输入由挂载点的绑定给）；
//   - 挂载之后能给那条进口添来源的只剩子图自己的 `Add`，而 `Start` 成功会把**整棵
//     子树**封口（含挂着的子图），所以「跑起来之后子图又长出新的来源」也进不来。
//
// 判据一条没少，只是全落在装配期：见 TestSubRejectsDoubleFedInlet 与
// TestValidateCatchesLateChildProvider。

// deepNest 造「图套图套图」的静态链：自底向上装配，每层把下一层的图挂成一步，
// 最深一层挂叶子节点。每层的 path 与图 id 由 `Topology()` 按挂载位置推导——
// 「深度不设限」得有用例钉着，不能只有两层。
type deepNest struct {
	in, out   Key[string]
	cin, cout Key[string]
}

func newDeepNest(suffix string) *deepNest {
	return &deepNest{
		in:   NewKey[string]("s.deep" + suffix + ".in"),
		out:  NewKey[string]("s.deep" + suffix + ".out"),
		cin:  NewKey[string]("s.deep" + suffix + ".cin"),
		cout: NewKey[string]("s.deep" + suffix + ".cout"),
	}
}

// build 自底向上装配 max 层嵌套，返回最外层那张图（挂到根图的一步上）。
// 第 i 层的图 id 沿用「父图 id + /Li」这套写法：
// `P` + `/L1` + … + `/Li`。
func (d *deepNest) build(t *testing.T, max int, leaf func(*RunCtx) error) *Graph {
	t.Helper()
	ctx := context.Background()
	var child *Graph
	for level := max; level >= 1; level-- {
		gid := "P"
		for i := 1; i <= level; i++ {
			gid += fmt.Sprintf("/L%d", i)
		}
		g := mustNew(t, ctx, gid)
		if err := g.DeclareInput(NewPort(d.cin)); err != nil {
			t.Fatalf("%s.DeclareInput = %v", gid, err)
		}
		if err := g.DeclareOutput(NewPort(d.cout)); err != nil {
			t.Fatalf("%s.DeclareOutput = %v", gid, err)
		}
		if level == max {
			mustAdd(t, g, NewNode("leaf", "", Requires(d.cin), Provides(d.cout), leaf))
		} else if err := Sub(g, fmt.Sprintf("L%d", level+1), "",
			[]SubBind{In(d.cin, d.cin), Out(d.cout, d.cout)}, child); err != nil {
			t.Fatalf("第 %d 层挂载 = %v", level, err)
		}
		child = g
	}
	return child
}

// levels 沿静态挂载边下钻（L1 挂进根图、L2 挂进 L1、…），逐层给一条
// "<path>|<graphID>"——两个事实都由 `Topology()` 按挂载位置推导。
func (d *deepNest) levels(root *Topology) []string {
	var out []string
	var sub *Topology
	for i := range root.Nodes {
		if root.Nodes[i].ID == "L1" {
			sub = root.Nodes[i].Sub
		}
	}
	for sub != nil {
		out = append(out, sub.Graph.Path+"|"+sub.Graph.ID)
		if len(sub.Nodes) == 0 {
			break
		}
		sub = sub.Nodes[0].Sub
	}
	return out
}

// 三层：每层的 path / 图 id 逐层接上，最深的产物一层层桥回最外层，
// 观测是一对**三层**的括号。
func TestSubDeepNesting(t *testing.T) {
	d := newDeepNest("a")
	log := &graphLog{}
	g := mustNew(t, context.Background(), "P", WithObserver(log.observer()))
	declareIn(t, g, NewPort(d.in))
	if err := seedOn(g, d.in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "L1", "", []SubBind{In(d.in, d.cin), Out(d.cout, d.out)},
		d.build(t, 3, func(rc *RunCtx) error {
			v, err := Get(rc, d.cin)
			if err != nil {
				return err
			}
			return Set(rc, d.cout, v+"|最深")
		})); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := g.Add(NewNode("sink", "", Requires(d.out), nil, func(rc *RunCtx) error {
		v, err := Get(rc, d.out)
		if err != nil {
			return err
		}
		got = v
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"L1|P/L1",
		"L1/L2|P/L1/L2",
		"L1/L2/L3|P/L1/L2/L3",
	}
	if levels := d.levels(g.Topology()); !reflect.DeepEqual(levels, want) {
		t.Fatalf("逐层的 path|图 id：\n got %v\nwant %v", levels, want)
	}
	if got != "v|最深" {
		t.Fatalf("最深层的产物没桥回最外层：%q", got)
	}
	// 观测：四张图各一对括号，由内向外逐层收。行里的位置是**挂载路径**
	// （阶段 2 起身份是 `Site`；根执行以图 id 表示，子执行用它自己的挂载位置）。
	// 只看**图级**记录——父图里那两个节点是并发的，节点级记录的先后不保证，
	// 混在一起比整条序列会飘。
	wantOrder := []string{
		"start:P",
		"start:L1",
		"start:L1/L2",
		"start:L1/L2/L3",
		"finish:L1/L2/L3=completed",
		"finish:L1/L2=completed",
		"finish:L1=completed",
		"finish:P=completed",
	}
	var graphRows []string
	for _, row := range log.snapshot() {
		if strings.HasPrefix(row, "start:") || strings.HasPrefix(row, "finish:") {
			graphRows = append(graphRows, row)
		}
	}
	if !reflect.DeepEqual(graphRows, wantOrder) {
		t.Fatalf("三层嵌套的观测括号不对：\n got %v\nwant %v", graphRows, wantOrder)
	}
}

// 最深那一层失败：首错要原样冒到最外层（中途三层不许改写它）。
func TestSubDeepFailureBubblesFromDeepest(t *testing.T) {
	d := newDeepNest("b")
	boom := errors.New("最深层的失败")
	g := mustNew(t, context.Background(), "P")
	declareIn(t, g, NewPort(d.in))
	if err := seedOn(g, d.in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "L1", "", []SubBind{In(d.in, d.cin), Out(d.cout, d.out)},
		d.build(t, 3, func(rc *RunCtx) error { return boom })); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); !errors.Is(err, boom) {
		t.Fatalf("Run = %v，want 最深层的原错（errors.Is）", err)
	}
}

// 最外层取消：最深那一层的节点要看得见（静态形态靠挂载那一步一层层把取消转发下去）。
func TestSubDeepCancelReachesDeepest(t *testing.T) {
	d := newDeepNest("c")
	ctx, cancel := context.WithCancel(context.Background())
	g := mustNew(t, ctx, "P")
	deepest := make(chan error, 1)
	declareIn(t, g, NewPort(d.in))
	if err := seedOn(g, d.in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "L1", "", []SubBind{In(d.in, d.cin), Out(d.cout, d.out)},
		d.build(t, 3, func(rc *RunCtx) error {
			select {
			case <-rc.Context().Done():
				deepest <- rc.Context().Err()
				return rc.Context().Err()
			case <-time.After(2 * time.Second):
				deepest <- nil
				return nil
			}
		})); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if err := runOn(g); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v，want context.Canceled", err)
	}
	select {
	case got := <-deepest:
		if !errors.Is(got, context.Canceled) {
			t.Fatalf("最深三层之下的节点看到的是 %v，want context.Canceled", got)
		}
	case <-time.After(time.Second):
		t.Fatal("最深层的节点始终没退出")
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
