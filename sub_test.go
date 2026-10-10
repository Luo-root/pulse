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

// 图即节点（#293）：建图 / 桥接 / 取消域 / 观察者四件事都由 Sub 收齐。
//
// 这些用例对着探针实测的结论写（`.workbase/probe-subgraph`，五组实测已抄进票面）：
// 手工嵌套能做到的，糖必须做到，且**观测不能断链**——那是手工搭最容易漏、
// 又最没声音的一处。
func TestSubBridgesValues(t *testing.T) {
	topic := NewKey[string]("s.topic")
	summary := NewKey[string]("s.summary")
	childIn := NewKey[string]("s.child.in")
	childOut := NewKey[string]("s.child.out")

	var gotID, gotPath string
	var seenObserver bool
	g := mustNew(t, context.Background(), "parent", WithObserver(ObserverFunc{}))

	if err := Seed(g, topic, "slot contract"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step1",
		[]SubBind{In(topic, childIn), Out(childOut, summary)},
		func(sc *SubCtx) (*Graph, error) {
			gotID, gotPath = sc.GraphID(), sc.Path()
			seenObserver = sc.Observer() != nil
			child, err := New(sc.Context(), sc.GraphID())
			if err != nil {
				return nil, err
			}
			return child, child.Add(NewNode("work", Requires(childIn), Provides(childOut),
				func(rc *RunCtx) error {
					v, err := Get(rc, childIn)
					if err != nil {
						return err
					}
					return Set(rc, childOut, "about "+v)
				}))
		}); err != nil {
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

	var bridged string
	if err := g.Add(NewNode("sink", Requires(summary), nil, func(rc *RunCtx) error {
		v, err := Get(rc, summary)
		if err != nil {
			return err
		}
		bridged = v
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if bridged != "about slot contract" {
		t.Fatalf("子图产物没有桥回父侧：%q", bridged)
	}
	if gotID != "parent/step1" {
		t.Fatalf("建议的图 id = %q，want parent/step1", gotID)
	}
	if gotPath != "step1" {
		t.Fatalf("一层子图的 path = %q，want step1", gotPath)
	}
	if !seenObserver {
		t.Fatal("SubCtx.Observer() 不该是 nil（父图挂了观察者）")
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
		GraphStarted:  func(g string) { l.add("start:" + g) },
		GraphFinished: func(g string, r NodeFinishReason, _ error) { l.add("finish:" + g + "=" + string(r)) },
		Waiting:       func(g, n string) { l.add("wait:" + g + "/" + n) },
		Running:       func(g, n string) { l.add("run:" + g + "/" + n) },
		Finished:      func(g, n string, r NodeFinishReason, _ error) { l.add("done:" + g + "/" + n + "=" + string(r)) },
	}
}

// 观察者自动下传：build 里**故意不挂**观察者，子图的记录照样落到同一条流里，
// 且两条流靠图 id 分得开——嵌套是一对括号（父图的两条把子图的两条夹在中间）。
func TestSubKeepsObservationChain(t *testing.T) {
	in := NewKey[string]("s.obs.in")
	cin := NewKey[string]("s.obs.cin")
	cout := NewKey[string]("s.obs.cout")
	out := NewKey[string]("s.obs.out")
	log := &graphLog{}
	g := mustNew(t, context.Background(), "P", WithObserver(log.observer()))

	if err := Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step1", []SubBind{In(in, cin), Out(cout, out)},
		func(sc *SubCtx) (*Graph, error) {
			child, err := New(sc.Context(), sc.GraphID())
			if err != nil {
				return nil, err
			}
			return child, child.Add(NewNode("inner", Requires(cin), Provides(cout),
				func(rc *RunCtx) error {
					v, err := Get(rc, cin)
					if err != nil {
						return err
					}
					return Set(rc, cout, v+"→内层")
				}))
		}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"start:P",
		"wait:P/step1", "run:P/step1",
		"start:P/step1",
		"wait:P/step1/inner", "run:P/step1/inner", "done:P/step1/inner=completed",
		"finish:P/step1=completed",
		"done:P/step1=completed",
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

	if err := Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step1", []SubBind{In(in, cin), Out(cout, NewKey[string]("s.co.out"))},
		func(sc *SubCtx) (*Graph, error) {
			child, err := New(sc.Context(), sc.GraphID(), WithObserver(childLog.observer()))
			if err != nil {
				return nil, err
			}
			return child, child.Add(NewNode("inner", Requires(cin), Provides(cout),
				func(rc *RunCtx) error { return Set(rc, cout, "y") }))
		}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	// 父图的出口只看得见父图自己那几条（子图那条节点的 id 是 P/step1/inner，
	// 图级两条是 start:P/step1 / finish:P/step1=completed）。两侧都是单节点图，
	// 序列完全确定。
	wantParent := []string{
		"start:P",
		"wait:P/step1", "run:P/step1", "done:P/step1=completed",
		"finish:P=completed",
	}
	if got := parentLog.snapshot(); !reflect.DeepEqual(got, wantParent) {
		t.Fatalf("父图的出口收到了不属于它的记录：\n got %v\nwant %v", got, wantParent)
	}
	wantChild := []string{
		"start:P/step1",
		"wait:P/step1/inner", "run:P/step1/inner", "done:P/step1/inner=completed",
		"finish:P/step1=completed",
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

	if err := Sub(g, "step1", []SubBind{Out(cout, out)},
		func(sc *SubCtx) (*Graph, error) {
			child, err := New(sc.Context(), sc.GraphID())
			if err != nil {
				return nil, err
			}
			return child, child.Add(NewNode("inner", nil, Provides(cout),
				func(rc *RunCtx) error { return boom }))
		}); err != nil {
		t.Fatal(err)
	}
	err := g.Run()
	if !errors.Is(err, boom) {
		t.Fatalf("Run = %v，want 子图的首错", err)
	}
	if n := countPref(obs.snapshot(), "F:step1:failed"); n != 1 {
		t.Fatalf("父侧那个节点应当是 failed：%v", obs.snapshot())
	}
}

// 父图取消 → 子图看得见（子 ctx 派生自本节点）。
func TestSubChildSeesParentCancel(t *testing.T) {
	in := NewKey[string]("s.cx.in")
	cin := NewKey[string]("s.cx.cin")
	ctx, cancel := context.WithCancel(context.Background())
	g := mustNew(t, ctx, "P")
	seenc := make(chan error, 1)

	if err := Seed(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step1", []SubBind{In(in, cin)},
		func(sc *SubCtx) (*Graph, error) {
			child, err := New(sc.Context(), sc.GraphID())
			if err != nil {
				return nil, err
			}
			return child, child.Add(NewNode("slow", Requires(cin), nil, func(crc *RunCtx) error {
				select {
				case <-crc.Context().Done():
					seenc <- crc.Context().Err()
					return crc.Context().Err()
				case <-time.After(2 * time.Second):
					seenc <- nil
					return nil
				}
			}))
		}); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	err := g.Run()
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

	build := func(sc *SubCtx) (*Graph, error) {
		child, err := New(sc.Context(), sc.GraphID())
		if err != nil {
			return nil, err
		}
		if err := child.Add(NewNode("w1", Requires(in), Provides(cReady, cSkipped),
			func(rc *RunCtx) error {
				if err := Set(rc, cReady, "值"); err != nil {
					return err
				}
				return Skip(rc, cSkipped)
			})); err != nil {
			return nil, err
		}
		return child, child.Add(NewNode("w2", Requires(in), Provides(cAll),
			func(rc *RunCtx) error { return Skip(rc, cAll) }))
	}

	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "P", WithObserver(obs))
	if err := Seed(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "part", []SubBind{
		In(in, in), Out(cReady, ready), Out(cSkipped, skipped),
	}, build); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "none", []SubBind{In(in, in), Out(cAll, all)}, build); err != nil {
		t.Fatal(err)
	}

	sawReady, sawSkipped, sawAllDownstream := false, false, false
	if err := g.Add(NewNode("useReady", Requires(ready), nil, func(rc *RunCtx) error {
		sawReady = true
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	// 跳过那条的下游要**带着另一条有值的输入**才会进 Run（门是「有没有值」）——
	// 进去之后再读那条被跳过的键，才是 *SkipError。
	if err := g.Add(NewNode("useSkipped", Deps(Requires(ready), Requires(skipped)), nil,
		func(rc *RunCtx) error {
			sawSkipped = true
			if _, err := Get(rc, skipped); !errors.Is(err, ErrSkipped) {
				return fmt.Errorf("父侧那条键应当是跳过到达，got %v", err)
			}
			return nil
		})); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("useAll", Requires(all), nil, func(rc *RunCtx) error {
		sawAllDownstream = true
		return nil
	})); err != nil {
		t.Fatal(err)
	}

	if err := g.Run(); err != nil {
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

	g := mustNew(t, context.Background(), "P")
	if err := g.Add(NewNode("maybe", nil, Provides(giver),
		func(rc *RunCtx) error { return Skip(rc, giver) })); err != nil {
		t.Fatal(err)
	}
	built := false
	if err := Sub(g, "step1", []SubBind{In(giver, cin), Out(cout, pout)},
		func(sc *SubCtx) (*Graph, error) {
			built = true
			child, err := New(sc.Context(), sc.GraphID())
			if err != nil {
				return nil, err
			}
			return child, child.Add(NewNode("inner", Requires(cin), Provides(cout),
				func(rc *RunCtx) error { return Set(rc, cout, "x") }))
		}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("跳过是到达、不是失败：%v", err)
	}
	if built {
		t.Fatal("全部输入跳过时本节点不该进入 Run，子图也就不该被建")
	}

	// —— 混合：一条有值、一条跳过 ——
	okIn := NewKey[string]("s.si2.ok")
	skipIn := NewKey[string]("s.si2.skip")
	cin2 := NewKey[string]("s.si2.cin")
	cSkip := NewKey[string]("s.si2.cskip")
	cout2 := NewKey[string]("s.si2.cout")
	pout2 := NewKey[string]("s.si2.out")

	g2 := mustNew(t, context.Background(), "P2")
	if err := Seed(g2, okIn, "有值"); err != nil {
		t.Fatal(err)
	}
	if err := SkipSeed(g2, skipIn); err != nil {
		t.Fatal(err)
	}
	var sawValue string
	var sawSkip error
	if err := Sub(g2, "step1", []SubBind{
		In(okIn, cin2), In(skipIn, cSkip), Out(cout2, pout2),
	}, func(sc *SubCtx) (*Graph, error) {
		child, err := New(sc.Context(), sc.GraphID())
		if err != nil {
			return nil, err
		}
		return child, child.Add(NewNode("inner", Deps(Requires(cin2), Requires(cSkip)),
			Provides(cout2), func(rc *RunCtx) error {
				v, err := Get(rc, cin2)
				if err != nil {
					return err
				}
				sawValue = v
				_, sawSkip = Get(rc, cSkip)
				return Set(rc, cout2, v)
			}))
	}); err != nil {
		t.Fatal(err)
	}
	var bridged string
	if err := g2.Add(NewNode("sink", Requires(pout2), nil, func(rc *RunCtx) error {
		v, err := Get(rc, pout2)
		if err != nil {
			return err
		}
		bridged = v
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := g2.Run(); err != nil {
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
}

// 绑定写错的三种，都要在跑到一半之前报出来，而不是静默什么都不发生。
func TestSubBindMistakesAreLoud(t *testing.T) {
	ok := func(sc *SubCtx) (*Graph, error) {
		child, err := New(sc.Context(), sc.GraphID())
		if err != nil {
			return nil, err
		}
		return child, child.Add(NewNode("n", nil, nil, func(rc *RunCtx) error { return nil }))
	}

	// 1) 子图没声明这条键
	in := NewKey[string]("s.bm.in")
	missing := NewKey[string]("s.bm.missing")
	g := mustNew(t, context.Background(), "P")
	if err := Seed(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "missing", []SubBind{In(in, missing)}, ok); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("Run = %v，want 「子图没声明这条键」", err)
	}

	// 2) 名字对上了、T 没对上：绑定说这一端是 string，子图里那条同名键是 int。
	//    （父侧与子侧写同一个 T 时编译期就红，所以这里要**同名不同 T**才到得了运行期
	//    ——YAML 那条路只有运行期能比，兜底是同一条。）
	same := NewKey[string]("s.bm.same")
	childInt := NewKey[int]("s.bm.same")
	typed := func(sc *SubCtx) (*Graph, error) {
		child, err := New(sc.Context(), sc.GraphID())
		if err != nil {
			return nil, err
		}
		if err := Seed(child, childInt, 1); err != nil {
			return nil, err
		}
		return child, child.Add(NewNode("n", Requires(childInt), nil, func(rc *RunCtx) error { return nil }))
	}
	g2 := mustNew(t, context.Background(), "P2")
	if err := Seed(g2, same, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g2, "typed", []SubBind{In(same, NewKey[string]("s.bm.same"))}, typed); err != nil {
		t.Fatal(err)
	}
	if err := g2.Run(); err == nil || !strings.Contains(err.Error(), "type mismatch") {
		t.Fatalf("Run = %v，want 「类型对不上」", err)
	}

	// 3) 箭头写反：把 Out 当成「父侧在前」写（`Out(父, 子)`），子侧那一端就指向了
	//    子图不认识的键——与 1) 同一个兜底接住，不会静默接反。
	cout := NewKey[string]("s.bm.cout")
	pout := NewKey[string]("s.bm.out")
	g3 := mustNew(t, context.Background(), "P3")
	if err := Seed(g3, in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g3, "reversed", []SubBind{In(in, in), Out(pout, cout)},
		func(sc *SubCtx) (*Graph, error) {
			child, err := New(sc.Context(), sc.GraphID())
			if err != nil {
				return nil, err
			}
			if err := Seed(child, in, "v"); err != nil {
				return nil, err
			}
			return child, child.Add(NewNode("n", Requires(in), Provides(cout),
				func(rc *RunCtx) error { return Set(rc, cout, "x") }))
		}); err != nil {
		t.Fatal(err)
	}
	if err := g3.Run(); err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("Run = %v，want 「箭头写反了」被兜住", err)
	}
}

// 复用同一张子图 → 一句能照着改的话（子图是一次性的）。
func TestSubRejectsReusedChildGraph(t *testing.T) {
	in := NewKey[string]("s.reuse.in")
	cin := NewKey[string]("s.reuse.cin")
	cout := NewKey[string]("s.reuse.cout")
	outA := NewKey[string]("s.reuse.outA")
	outB := NewKey[string]("s.reuse.outB")

	g := mustNew(t, context.Background(), "P")
	if err := Seed(g, in, "v"); err != nil {
		t.Fatal(err)
	}

	// 典型的写错：子图建在闭包外面，于是第二步拿到的是同一张（已经跑过的）图。
	var cached *Graph
	build := func(sc *SubCtx) (*Graph, error) {
		if cached != nil {
			return cached, nil
		}
		child, err := New(sc.Context(), sc.GraphID())
		if err != nil {
			return nil, err
		}
		if err := child.Add(NewNode("n", Requires(cin), Provides(cout),
			func(rc *RunCtx) error { return Set(rc, cout, "x") })); err != nil {
			return nil, err
		}
		cached = child
		return child, nil
	}
	if err := Sub(g, "a", []SubBind{In(in, cin), Out(cout, outA)}, build); err != nil {
		t.Fatal(err)
	}
	// b 排在 a 之后（它读 a 的产物），于是 b 一定拿到那张已经启动的图。
	if err := Sub(g, "b", []SubBind{In(outA, cin), Out(cout, outB)}, build); err != nil {
		t.Fatal(err)
	}
	err := g.Run()
	if err == nil {
		t.Fatal("复用同一张子图应当报错")
	}
	// 断言要盯**翻译过的那句**：`ErrGraphStarted` 的原文自己也含 "already started"，
	// 只断言这个短语的话，把翻译去掉照样绿（变异对照实测过）。
	if !strings.Contains(err.Error(), "build must return a new one on every run") {
		t.Fatalf("要报出一句能照着改的话，got %v", err)
	}
	if !errors.Is(err, ErrGraphStarted) {
		t.Fatalf("翻译不该吃掉哨兵：errors.Is(err, ErrGraphStarted) 要成立，got %v", err)
	}
}

// 并发的两次 Sub 复用同一张**尚未启动**的子图：认领必须是**原子**的，所以只有
// 一方拿到它，另一方当场拿到那句翻译——而且**不许有数据竞争**。
//
// 这是变异对照抓出来的一处洞：修复前这里没有认领，两边都会看到「还没启动」，
// 然后一起写子图的 path（`-race` 实测就是 `child.path = path` 那一行报 DATA RACE）。
//
// 跑多轮是因为赢面太窄：把认领的锁去掉（读与写不在同一临界区）时，一轮里两边
// 也可能恰好错开——实测单轮抓不到、多轮才抓到。多轮让「两边同时读到旧值」这条
// 路径有机会出现，出现了就是一处并发写。
func TestSubConcurrentReuseIsClaimedAtomically(t *testing.T) {
	inA := NewKey[string]("s.race.inA")
	inB := NewKey[string]("s.race.inB")
	cin := NewKey[string]("s.race.cin")
	cout := NewKey[string]("s.race.cout")
	outA := NewKey[string]("s.race.outA")
	outB := NewKey[string]("s.race.outB")

	for round := range 20 {
		// 共享子图：建一次，两个 Sub 的 build 都把它交出去（正是引擎要拦的复用）。
		// 两个 Sub 绑**同一条**子键，种值幂等，所以两边都走得到「认领」那一刻。
		shared, err := New(context.Background(), "SHARED")
		if err != nil {
			t.Fatal(err)
		}
		if err := shared.Add(NewNode("w", Requires(cin), Provides(cout),
			func(rc *RunCtx) error { return Set(rc, cout, "x") })); err != nil {
			t.Fatal(err)
		}

		// 让两边在 build 里对齐再一起碰共享图；带超时是为了万一只有一个节点跑到，
		// 也别把整个测试挂死（认领失败的一方**不碰**这张图，对齐只需「同时到」）。
		var arrive sync.WaitGroup
		arrive.Add(2)
		release := make(chan struct{})
		go func() {
			arrive.Wait()
			close(release)
		}()
		build := func(sc *SubCtx) (*Graph, error) {
			arrive.Done()
			select {
			case <-release:
			case <-time.After(2 * time.Second):
			}
			return shared, nil
		}

		g := mustNew(t, context.Background(), "P")
		if err := Seed(g, inA, "a"); err != nil {
			t.Fatal(err)
		}
		if err := Seed(g, inB, "b"); err != nil {
			t.Fatal(err)
		}
		if err := Sub(g, "sA", []SubBind{In(inA, cin), Out(cout, outA)}, build); err != nil {
			t.Fatal(err)
		}
		if err := Sub(g, "sB", []SubBind{In(inB, cin), Out(cout, outB)}, build); err != nil {
			t.Fatal(err)
		}

		err = g.Run()
		if err == nil {
			t.Fatalf("第 %d 轮：并发复用同一张子图应当报错", round)
		}
		if !strings.Contains(err.Error(), "build must return a new one on every run") {
			t.Fatalf("第 %d 轮：抢不到认领的一方要拿到那句翻译，got %v", round, err)
		}
		if !errors.Is(err, ErrGraphStarted) {
			t.Fatalf("第 %d 轮：翻译不该吃掉哨兵，got %v", round, err)
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

	build := func(sc *SubCtx) (*Graph, error) {
		child, err := New(sc.Context(), sc.GraphID())
		if err != nil {
			return nil, err
		}
		return child, child.Add(NewNode("n", Requires(cin), Provides(cout),
			func(rc *RunCtx) error { return Set(rc, cout, "x") }))
	}

	g := mustNew(t, context.Background(), "P")
	err := Sub(g, "a/b", []SubBind{In(in, cin), Out(cout, out)}, build)
	if err == nil {
		t.Fatal("节点 id 里的 / 会让 path 的层级拆不开，装配期就该拒")
	}
	if !strings.Contains(err.Error(), "must not contain '/'") {
		t.Fatalf("要说明为什么拒，got %v", err)
	}
	// 想表达「更深一层」的话，嵌套本来就能写；别的节点 id 含 `/` 不受影响。
	if err := Sub(g, "a", []SubBind{In(in, cin), Out(cout, out)}, build); err != nil {
		t.Fatalf("正常的 id 不该被拒：%v", err)
	}
}

// 翻译函数本身：只翻「已经启动过」这一类，别的错误原样过。
func TestSubStartedErrTranslation(t *testing.T) {
	got := subStartedErr("b", ErrGraphStarted)
	if !strings.Contains(got.Error(), `Sub "b"`) ||
		!strings.Contains(got.Error(), "build must return a new one on every run") {
		t.Fatalf("子图复用要报出「哪一步」与「怎么改」，got %v", got)
	}
	if !errors.Is(got, ErrGraphStarted) {
		t.Fatalf("哨兵被吃掉了：%v", got)
	}
	other := errors.New("别的东西")
	if subStartedErr("b", other) != other {
		t.Fatalf("不该翻别的错误，got %v", subStartedErr("b", other))
	}
}

// 两层嵌套：path 是节点 id 链，父图的 path 自动盖到子图上；图 id 也一层层接。
func TestSubNestedPath(t *testing.T) {
	in := NewKey[string]("s.np.in")
	cin := NewKey[string]("s.np.cin")
	cout := NewKey[string]("s.np.cout")
	pout := NewKey[string]("s.np.out")

	var outerPath, innerPath, innerID string
	g := mustNew(t, context.Background(), "P")
	if err := Seed(g, in, "叶子"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "outer", []SubBind{In(in, cin), Out(cout, pout)},
		func(sc *SubCtx) (*Graph, error) {
			outerPath = sc.Path()
			child, err := New(sc.Context(), sc.GraphID())
			if err != nil {
				return nil, err
			}
			if err := Sub(child, "inner", []SubBind{In(cin, cin), Out(cout, cout)},
				func(sc *SubCtx) (*Graph, error) {
					innerPath, innerID = sc.Path(), sc.GraphID()
					grand, err := New(sc.Context(), sc.GraphID())
					if err != nil {
						return nil, err
					}
					return grand, grand.Add(NewNode("leaf", Requires(cin), Provides(cout),
						func(rc *RunCtx) error {
							v, err := Get(rc, cin)
							if err != nil {
								return err
							}
							return Set(rc, cout, v+"→更深一层")
						}))
				}); err != nil {
				return nil, err
			}
			return child, nil
		}); err != nil {
		t.Fatal(err)
	}

	var bridged string
	if err := g.Add(NewNode("sink", Requires(pout), nil, func(rc *RunCtx) error {
		v, err := Get(rc, pout)
		if err != nil {
			return err
		}
		bridged = v
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if outerPath != "outer" {
		t.Fatalf("第一层 path = %q，want outer", outerPath)
	}
	if innerPath != "outer/inner" {
		t.Fatalf("第二层 path = %q，want outer/inner", innerPath)
	}
	if innerID != "P/outer/inner" {
		t.Fatalf("第二层的图 id = %q，want P/outer/inner", innerID)
	}
	if bridged != "叶子→更深一层" {
		t.Fatalf("两层之后桥回父侧的是 %q", bridged)
	}
}

// 名额各管各的（票面 A2）：父图 WithMaxRunning(1) 管的是「同时有几张子图在跑」，
// 子图内部的并发由子图自己的 WithMaxRunning 管——子图里两个节点必须能重叠。
func TestSubSlotsArePerGraph(t *testing.T) {
	in := NewKey[string]("s.slot.in")
	cin := NewKey[string]("s.slot.cin")

	g := mustNew(t, context.Background(), "P", WithMaxRunning(1))
	if err := Seed(g, in, "v"); err != nil {
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

	if err := Sub(g, "step1", []SubBind{In(in, cin)},
		func(sc *SubCtx) (*Graph, error) {
			child, err := New(sc.Context(), sc.GraphID(), WithMaxRunning(2))
			if err != nil {
				return nil, err
			}
			for _, id := range []string{"w1", "w2"} {
				if err := child.Add(NewNode(id, Requires(cin), nil, func(rc *RunCtx) error {
					enter()
					select {
					case <-both:
					case <-time.After(2 * time.Second):
						return errors.New("子图的两个节点没能重叠：名额被父图的 WithMaxRunning 卡住了")
					}
					mu.Lock()
					if inside > peak {
						peak = inside
					}
					mu.Unlock()
					return nil
				})); err != nil {
					return nil, err
				}
			}
			return child, nil
		}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("父子名额各管各的，嵌套应当跑得通：%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak != 2 {
		t.Fatalf("子图内部同时进 Run 的峰值 = %d，want 2（父图 WithMaxRunning(1) 不该管到子图内部）", peak)
	}
}

// 子图在 build 里自己种了某条**被绑定**的键 → 父侧传进来的值会被幂等首写静默
// 忽略（「值不见了」而没有报错），所以这里当场吵出来。
func TestSubRejectsChildSeedingBoundKey(t *testing.T) {
	in := NewKey[string]("s.cs.in")
	cin := NewKey[string]("s.cs.cin")
	cout := NewKey[string]("s.cs.cout")
	out := NewKey[string]("s.cs.out")

	for _, tc := range []struct {
		name string
		seed func(*Graph) error
	}{
		{"Seed", func(child *Graph) error { return Seed(child, cin, "子图自己的默认值") }},
		{"SkipSeed", func(child *Graph) error { return SkipSeed(child, cin) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := mustNew(t, context.Background(), "P")
			if err := Seed(g, in, "父侧的值"); err != nil {
				t.Fatal(err)
			}
			if err := Sub(g, "step1", []SubBind{In(in, cin), Out(cout, out)},
				func(sc *SubCtx) (*Graph, error) {
					child, err := New(sc.Context(), sc.GraphID())
					if err != nil {
						return nil, err
					}
					if err := tc.seed(child); err != nil {
						return nil, err
					}
					return child, child.Add(NewNode("n", Requires(cin), Provides(cout),
						func(rc *RunCtx) error {
							v, err := Get(rc, cin)
							if err != nil {
								return err
							}
							return Set(rc, cout, v)
						}))
				}); err != nil {
				t.Fatal(err)
			}
			err := g.Run()
			if err == nil || !strings.Contains(err.Error(), "silently ignored") {
				t.Fatalf("Run = %v，want 「父侧的值会被静默忽略」", err)
			}
		})
	}
}

// deepNest 造「图套图套图」：每层子图里再嵌一层，最后一层挂叶子节点。
// 每层的 SubCtx 事实（path / 图 id）都留痕——「深度不设限」得有用例钉着，
// 不能只有两层。
type deepNest struct {
	in, out   Key[string]
	cin, cout Key[string]

	mu   sync.Mutex
	seen []string // 每层一条 "<path>|<graphID>"
}

func (d *deepNest) build(cur, max int, leaf func(*RunCtx) error) func(*SubCtx) (*Graph, error) {
	return func(sc *SubCtx) (*Graph, error) {
		d.mu.Lock()
		d.seen = append(d.seen, sc.Path()+"|"+sc.GraphID())
		d.mu.Unlock()

		child, err := New(sc.Context(), sc.GraphID())
		if err != nil {
			return nil, err
		}
		if cur == max {
			return child, child.Add(NewNode("leaf", Requires(d.cin), Provides(d.cout), leaf))
		}
		return child, Sub(child, fmt.Sprintf("L%d", cur+1),
			[]SubBind{In(d.cin, d.cin), Out(d.cout, d.cout)},
			d.build(cur+1, max, leaf))
	}
}

func (d *deepNest) levels() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

func newDeepNest(suffix string) *deepNest {
	return &deepNest{
		in:   NewKey[string]("s.deep" + suffix + ".in"),
		out:  NewKey[string]("s.deep" + suffix + ".out"),
		cin:  NewKey[string]("s.deep" + suffix + ".cin"),
		cout: NewKey[string]("s.deep" + suffix + ".cout"),
	}
}

// 三层：每层的 path / 图 id 逐层接上，最深的产物一层层桥回最外层，
// 观测是一对**三层**的括号。
func TestSubDeepNesting(t *testing.T) {
	d := newDeepNest("a")
	log := &graphLog{}
	g := mustNew(t, context.Background(), "P", WithObserver(log.observer()))
	if err := Seed(g, d.in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "L1", []SubBind{In(d.in, d.cin), Out(d.cout, d.out)},
		d.build(1, 3, func(rc *RunCtx) error {
			v, err := Get(rc, d.cin)
			if err != nil {
				return err
			}
			return Set(rc, d.cout, v+"|最深")
		})); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := g.Add(NewNode("sink", Requires(d.out), nil, func(rc *RunCtx) error {
		v, err := Get(rc, d.out)
		if err != nil {
			return err
		}
		got = v
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"L1|P/L1",
		"L1/L2|P/L1/L2",
		"L1/L2/L3|P/L1/L2/L3",
	}
	if levels := d.levels(); !reflect.DeepEqual(levels, want) {
		t.Fatalf("逐层的 path|图 id：\n got %v\nwant %v", levels, want)
	}
	if got != "v|最深" {
		t.Fatalf("最深层的产物没桥回最外层：%q", got)
	}
	// 观测：四张图各一对括号，由内向外逐层收。只看**图级**记录——父图里那两
	// 个节点是并发的，节点级记录的先后不保证，混在一起比整条序列会飘。
	wantOrder := []string{
		"start:P",
		"start:P/L1",
		"start:P/L1/L2",
		"start:P/L1/L2/L3",
		"finish:P/L1/L2/L3=completed",
		"finish:P/L1/L2=completed",
		"finish:P/L1=completed",
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
	if err := Seed(g, d.in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "L1", []SubBind{In(d.in, d.cin), Out(d.cout, d.out)},
		d.build(1, 3, func(rc *RunCtx) error { return boom })); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); !errors.Is(err, boom) {
		t.Fatalf("Run = %v，want 最深层的原错（errors.Is）", err)
	}
}

// 最外层取消：最深那一层的节点要看得见（取消域一层层派生下去）。
func TestSubDeepCancelReachesDeepest(t *testing.T) {
	d := newDeepNest("c")
	ctx, cancel := context.WithCancel(context.Background())
	g := mustNew(t, ctx, "P")
	deepest := make(chan error, 1)
	if err := Seed(g, d.in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "L1", []SubBind{In(d.in, d.cin), Out(d.cout, d.out)},
		d.build(1, 3, func(rc *RunCtx) error {
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
	if err := g.Run(); !errors.Is(err, context.Canceled) {
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
	ok := func(sc *SubCtx) (*Graph, error) { return New(sc.Context(), sc.GraphID()) }

	if err := Sub(nil, "x", []SubBind{In(in, cin)}, ok); err == nil {
		t.Fatal("nil graph 应当报错")
	}
	if err := Sub(g, "", []SubBind{In(in, cin)}, ok); err == nil {
		t.Fatal("空 id 应当报错")
	}
	if err := Sub(g, "x", []SubBind{In(in, cin)}, nil); err == nil {
		t.Fatal("nil build 应当报错")
	}
	if err := Sub(g, "x", nil, ok); err == nil {
		t.Fatal("没有绑定应当报错")
	}
	// 同一条子侧键绑两次：第二次种值会被静默忽略，装配期就拒。
	if err := Sub(g, "x", []SubBind{In(in, cin), In(in, cin)}, ok); err == nil {
		t.Fatal("子侧键绑两次应当报错")
	}
	// 零值 SubBind（手写 SubBind{} 而不是用 In / Out）
	if err := Sub(g, "x", []SubBind{{}}, ok); err == nil {
		t.Fatal("零值 bind 应当报错")
	}
	if n := len(g.nodes); n != 0 {
		t.Fatalf("装配失败不该往图上留节点，got %d", n)
	}
}
