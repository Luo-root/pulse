package pulse

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// conc 数「此刻有几条节点体在跑」，并记峰值。名额语义是「整棵图树里同时有几个
// 节点真正在 Run」，所以峰值就是它的可观测形状。
type conc struct {
	mu   sync.Mutex
	live int
	top  int
}

func (c *conc) enter() func() {
	c.mu.Lock()
	c.live++
	if c.live > c.top {
		c.top = c.live
	}
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.live--
		c.mu.Unlock()
	}
}

func (c *conc) peak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.top
}

// 名额整棵树共享：父图声明 1、子图**不声明** → 子图内部两个节点不能重叠。
// 旧行为（每张图各一份）下这条必红：子图拿到的是无限名额，两个节点会重叠。
func TestSubLimitInheritedByParent(t *testing.T) {
	in := NewKey[string]("lim.inherit.in")
	cin := NewKey[string]("lim.inherit.cin")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := mustNew(t, ctx, "P", WithMaxRunning(1))
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}

	var c conc
	var done atomic.Int32
	child := mustNew(t, ctx, "P/step") // 不声明名额 → 运行期继承父图那份
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	for _, id := range []string{"w1", "w2"} {
		mustAdd(t, child, NewNode(id, "", Requires(cin), nil, func(rc *RunCtx) error {
			leave := c.enter()
			defer leave()
			done.Add(1)
			time.Sleep(60 * time.Millisecond)
			return nil
		}))
	}
	if err := Sub(g, "step", "", []SubBind{In(in, cin)}, child); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatalf("继承父图名额之后嵌套应当跑得通：%v", err)
	}
	if got := done.Load(); got != 2 {
		t.Fatalf("子图两个节点都该跑过，实得 %d 个", got)
	}
	if got := c.peak(); got != 1 {
		t.Fatalf("整棵树同时进 Run 的峰值 = %d，want 1（子图继承父图的 WithMaxRunning(1)）", got)
	}
}

// 今天会死锁的那条路径：限额 1 + 三层嵌套。若挂子图那一步占着名额等子图，
// 内层永远拿不到名额 → 整轮超时（同形实测：探针 probe-sharedsem 限额 1 → 超前收尾）。
func TestSubLimitThreeDeepUnderOneSlot(t *testing.T) {
	in := NewKey[string]("lim.deep.in")
	l1 := NewKey[string]("lim.deep.l1")
	l2 := NewKey[string]("lim.deep.l2")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g := mustNew(t, ctx, "P", WithMaxRunning(1))
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}

	var c conc
	var leafRan atomic.Int32

	// 三层子图全部在装配期建好：最内层吃 l2，中间层把 l1 转给 l2，顶层挂 step1。
	leaf := mustNew(t, ctx, "P/step1/step2")
	if err := leaf.DeclareInput(NewPort(l2)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	mustAdd(t, leaf, NewNode("leaf", "", Requires(l2), nil, func(rc *RunCtx) error {
		leave := c.enter()
		defer leave()
		leafRan.Add(1)
		time.Sleep(30 * time.Millisecond)
		return nil
	}))

	mid := mustNew(t, ctx, "P/step1")
	if err := mid.DeclareInput(NewPort(l1)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := Sub(mid, "step2", "", []SubBind{In(l1, l2)}, leaf); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step1", "", []SubBind{In(in, l1)}, mid); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatalf("三层嵌套 + 限额 1 应当跑得通（占着名额等子图的话这里会是超时）：%v", err)
	}
	if got := leafRan.Load(); got != 1 {
		t.Fatalf("最内层节点跑了 %d 次，want 1", got)
	}
	if got := c.peak(); got != 1 {
		t.Fatalf("整棵树同时进 Run 的峰值 = %d，want 1", got)
	}
}

// 同层两个子图共享同一份额度：合计并发 ≤ 2（不是两个子图各 2）。
func TestSubLimitSiblingsShareQuota(t *testing.T) {
	in1 := NewKey[string]("lim.sib.in1")
	in2 := NewKey[string]("lim.sib.in2")
	c1 := NewKey[string]("lim.sib.c1")
	c2 := NewKey[string]("lim.sib.c2")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := mustNew(t, ctx, "P", WithMaxRunning(2))
	declareIn(t, g, NewPort(in1))
	if err := seedOn(g, in1, "v"); err != nil {
		t.Fatal(err)
	}
	declareIn(t, g, NewPort(in2))
	if err := seedOn(g, in2, "v"); err != nil {
		t.Fatal(err)
	}

	var c conc
	var done atomic.Int32
	// 装配期按同一段代码建两张子图（两个实例），各自的入口端口先声明。
	build := func(graphID string, key Key[string]) *Graph {
		child := mustNew(t, ctx, graphID)
		if err := child.DeclareInput(NewPort(key)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		for _, id := range []string{"w1", "w2"} {
			mustAdd(t, child, NewNode(id, "", Requires(key), nil, func(rc *RunCtx) error {
				leave := c.enter()
				defer leave()
				done.Add(1)
				time.Sleep(60 * time.Millisecond)
				return nil
			}))
		}
		return child
	}
	if err := Sub(g, "a", "", []SubBind{In(in1, c1)}, build("P/a", c1)); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "b", "", []SubBind{In(in2, c2)}, build("P/b", c2)); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if got := done.Load(); got != 4 {
		t.Fatalf("四个节点都该跑过，实得 %d 个", got)
	}
	if got := c.peak(); got != 2 {
		t.Fatalf("整棵树同时进 Run 的峰值 = %d，want 2（两个子图共享父图的 2 个名额）", got)
	}
}

// 子图声明 WithMaxRunning(0) = 「这一子树不限」：父图的 1 不约束它。
// （若实现把「声明」判成「值 != 0」，这条会红。）
func TestSubLimitZeroOptsOutForSubtree(t *testing.T) {
	in := NewKey[string]("lim.zero.in")
	cin := NewKey[string]("lim.zero.cin")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := mustNew(t, ctx, "P", WithMaxRunning(1))
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}

	var c conc
	child := mustNew(t, ctx, "P/step", WithMaxRunning(0)) // 声明：这一子树不限
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	for _, id := range []string{"w1", "w2"} {
		mustAdd(t, child, NewNode(id, "", Requires(cin), nil, func(rc *RunCtx) error {
			leave := c.enter()
			defer leave()
			time.Sleep(60 * time.Millisecond)
			return nil
		}))
	}
	if err := Sub(g, "step", "", []SubBind{In(in, cin)}, child); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if got := c.peak(); got != 2 {
		t.Fatalf("峰值 = %d，want 2（子图自己声明了不限，父图的 1 不该管到它）", got)
	}
}
