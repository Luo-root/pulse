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
