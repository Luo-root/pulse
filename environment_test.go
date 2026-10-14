package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rec 是一枚「记名切面」：它只把名字与进出事件记进共享日志，不做别的事。
// 图级切面的继承/叠加/切断都只能从「谁被套过、按什么顺序被套」上看出来，
// 所以用例全靠它取证。并发安全：子图里的节点可能并行。
type rec struct {
	mu   sync.Mutex
	logs []string
}

func (r *rec) aspect(name string) Aspect {
	return func(rc *RunCtx, next func(*RunCtx) error) error {
		r.append("in " + name)
		defer r.append("out " + name)
		return next(rc)
	}
}

func (r *rec) append(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, s)
}

func (r *rec) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.logs {
		if l == "in "+name {
			n++
		}
	}
	return n
}

func (r *rec) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.logs...)
}

// childWith 造一个子图：n 个节点都吃 `cin`，各自跑 body，可带构造选项。
//
// 静态形态：子图在**装配期**就建好，所以 `cin` 先声明成入口端口（接线只能接端口），
// 图 id 走 `<父图 id>/<节点 id>` 那套口径。
func childWith(t *testing.T, ctx context.Context, graphID, nodePrefix string, n int, cin Key[string],
	body func(*RunCtx) error, opts ...Option) *Graph {
	t.Helper()
	child := mustNew(t, ctx, graphID, opts...)
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	for i := 0; i < n; i++ {
		mustAdd(t, child, NewNode(fmt.Sprintf("%s-node%d", nodePrefix, i), "", Requires(cin), nil, body))
	}
	return child
}

// 图级切面**默认继承**：父图 WithAspects(A)、子图不声明 ⇒ 子图里的节点也被 A 包住。
// 旧行为（不继承）下这条必红：A 只在父侧那一步出现 1 次，子图两个节点各自裸跑。
func TestAspectInheritedBySubGraph(t *testing.T) {
	in := NewKey[string]("asp.inherit.in")
	cin := NewKey[string]("asp.inherit.cin")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var r rec
	var ran atomic.Int32
	g := mustNew(t, ctx, "P", WithAspects(r.aspect("A")))
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	child := childWith(t, ctx, "P/step", "c", 2, cin, func(*RunCtx) error { ran.Add(1); return nil })
	if err := Sub(g, "step", "", []SubBind{In(in, cin)}, child); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if got := ran.Load(); got != 2 {
		t.Fatalf("子图两个节点都该跑过，实得 %d 个", got)
	}
	// 父侧那一步 1 次 + 子图两个节点各 1 次。
	if got := r.count("A"); got != 3 {
		t.Fatalf("A 被套了 %d 次，want 3（父侧那一步 + 子图两个节点）：%v", got, r.all())
	}
}

// 叠加的**顺序**：继承来的在**外**、子图自己声明的在**内**（与「先写的更靠外」同一条
// 规则）。单节点 → 事件序确定，可以逐条断言。
func TestAspectInheritOrderOuterFirst(t *testing.T) {
	in := NewKey[string]("asp.order.in")
	cin := NewKey[string]("asp.order.cin")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var r rec
	g := mustNew(t, ctx, "P", WithAspects(r.aspect("outer")))
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	child := childWith(t, ctx, "P/step", "c", 1, cin, func(*RunCtx) error { return nil },
		WithAspects(r.aspect("inner")))
	if err := Sub(g, "step", "", []SubBind{In(in, cin)}, child); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	// 父侧那一步的 outer 包着**整段子图**（所以子图那个节点的进出在它里面）；
	// 子图那个节点自己的链 = outer（继承，在外）→ inner（自己声明的，在内）。
	want := "in outer, in outer, in inner, out inner, out outer, out outer"
	if got := strings.Join(r.all(), ", "); got != want {
		t.Fatalf("切面链顺序不对：\n got %s\nwant %s", got, want)
	}
}

// WithoutAspects 切断继承：子图节点不再被父图那批包住，但**子图自己声明的照挂**；
// 同时钉住那条边界——父图那一步 `Sub` **仍然**被父图的图级切面包着（那是父图对自己
// 节点做的事，切断解不开它）。
func TestAspectCutInheritanceKeepsOwnAndParentStep(t *testing.T) {
	in := NewKey[string]("asp.cut.in")
	cin := NewKey[string]("asp.cut.cin")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var r rec
	var ran atomic.Int32
	g := mustNew(t, ctx, "P", WithAspects(r.aspect("parent")))
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	child := childWith(t, ctx, "P/step", "c", 2, cin, func(*RunCtx) error { ran.Add(1); return nil },
		WithoutAspects(), WithAspects(r.aspect("own")))
	if err := Sub(g, "step", "", []SubBind{In(in, cin)}, child); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if got := ran.Load(); got != 2 {
		t.Fatalf("子图两个节点都该跑过，实得 %d 个", got)
	}
	// 父图那批只剩「父侧那一步」这一次——子图两个节点上都没有它。
	if got := r.count("parent"); got != 1 {
		t.Fatalf("切断之后父图那批仍被套了 %d 次，want 1（只有父侧那一步）：%v", got, r.all())
	}
	// 自己那批照挂，两个子图节点各一次。
	if got := r.count("own"); got != 2 {
		t.Fatalf("子图自己声明的那批被套了 %d 次，want 2（两个子图节点）：%v", got, r.all())
	}
}

// 零参 `WithAspects()` / `WithAspects(list...)` 传进空切片 = **没声明**，不是切断。
// 想「这一层什么都不要」只能靠 `WithoutAspects()`。
func TestAspectEmptyDeclIsNotCut(t *testing.T) {
	in := NewKey[string]("asp.empty.in")
	cin := NewKey[string]("asp.empty.cin")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var r rec
	var empty []Aspect // 传进 WithAspects(empty...) 的一个空批次
	g := mustNew(t, ctx, "P", WithAspects(r.aspect("A")))
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	child := childWith(t, ctx, "P/step", "c", 2, cin, func(*RunCtx) error { return nil },
		WithAspects(empty...))
	if err := Sub(g, "step", "", []SubBind{In(in, cin)}, child); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if got := r.count("A"); got != 3 {
		t.Fatalf("空切片声明必须仍是「继承」（A 该被套 3 次），实得 %d：%v", got, r.all())
	}
}

// 继承**不随嵌套深度重复叠加**：三层嵌套、每层都不声明，每个节点身上的链长都是 1。
// 事件序是确定的：三个节点依次进出。
func TestAspectInheritNoDuplicatePerDepth(t *testing.T) {
	in := NewKey[string]("asp.deep.in")
	l1 := NewKey[string]("asp.deep.l1")
	l2 := NewKey[string]("asp.deep.l2")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var r rec
	g := mustNew(t, ctx, "P", WithAspects(r.aspect("A")))
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}

	// 静态形态：三层子图都在装配期建好，端口先声明，再逐层挂上去。
	leaf := mustNew(t, ctx, "P/step1/step2")
	if err := leaf.DeclareInput(NewPort(l2)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, leaf, NewNode("leaf", "", Requires(l2), nil, func(rc *RunCtx) error {
		r.append("BODY")
		return nil
	}))

	mid := mustNew(t, ctx, "P/step1")
	if err := mid.DeclareInput(NewPort(l1)); err != nil {
		t.Fatal(err)
	}
	if err := Sub(mid, "step2", "", []SubBind{In(l1, l2)}, leaf); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step1", "", []SubBind{In(in, l1)}, mid); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	want := "in A, in A, in A, BODY, out A, out A, out A"
	if got := strings.Join(r.all(), ", "); got != want {
		t.Fatalf("每个节点只该带一层 A（最内层也是 1 层）：\n got %s\nwant %s", got, want)
	}
}

// 中间层切断之后，**孙子看不见祖父**：继承读的是父图那一层的**有效**列表，不是它继承
// 之前的那份。所以「祖父 A → 中间层切断 + 声明 B → 孙子不声明」时，孙子身上的链是
// `[B]`，A 不会顺着血缘漏下去。
func TestAspectCutAtMiddleLayerHidesGrandparent(t *testing.T) {
	in := NewKey[string]("asp.mid.in")
	l1 := NewKey[string]("asp.mid.l1")
	l2 := NewKey[string]("asp.mid.l2")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var r rec
	g := mustNew(t, ctx, "P", WithAspects(r.aspect("A")))
	declareIn(t, g, NewPort(in))
	if err := seedOn(g, in, "v"); err != nil {
		t.Fatal(err)
	}

	// 最内层：不声明任何切面 → 只能拿到中间层那层有效列表 = [B]。
	leaf := mustNew(t, ctx, "P/step1/step2")
	if err := leaf.DeclareInput(NewPort(l2)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, leaf, NewNode("leaf", "", Requires(l2), nil, func(rc *RunCtx) error {
		r.append("BODY")
		return nil
	}))
	// 中间层：切断继承（A 不从这一层往下走），自己声明 B。
	mid := mustNew(t, ctx, "P/step1", WithoutAspects(), WithAspects(r.aspect("B")))
	if err := mid.DeclareInput(NewPort(l1)); err != nil {
		t.Fatal(err)
	}
	if err := Sub(mid, "step2", "", []SubBind{In(l1, l2)}, leaf); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step1", "", []SubBind{In(in, l1)}, mid); err != nil {
		t.Fatal(err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}

	// 父图那一步：A（父图的图级切面，与继承无关）；中间层那一步：B；
	// 最内层那个节点：B（从中间层继承来的有效列表），**没有 A**。
	want := "in A, in B, in B, BODY, out B, out B, out A"
	if got := strings.Join(r.all(), ", "); got != want {
		t.Fatalf("中间层切断之后 A 不该出现在孙子身上：\n got %s\nwant %s", got, want)
	}
}

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

func TestMaxRunningSerializes(t *testing.T) {
	g := mustNew(t, context.Background(), "test", WithMaxRunning(1))
	var current, max atomic.Int32
	run := func(rc *RunCtx) error {
		n := current.Add(1)
		for {
			old := max.Load()
			if n <= old || max.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		current.Add(-1)
		// 无 Provides：漏写自动 skip，无所谓
		return nil
	}
	mustAdd(t, g, NewNode("x", "", nil, nil, run))
	mustAdd(t, g, NewNode("y", "", nil, nil, run))
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if max.Load() != 1 {
		t.Fatalf("max concurrent Run = %d, want 1", max.Load())
	}
}

// TestCancelInterruptsQueuedNode ctx 取消要能打断「等名额」：在 acquire 里
// 排队的节点不进入 Run，终态 canceled——与「等数据」一致（New 的 godoc
// 承诺「ctx 取消会打断所有等待」）。
//
// 顺序必须确定，不能让「谁先抢到名额」交给调度器：a 先 Set 出 k，b 才能
// 通过 WaitAll 走到 acquire。于是 a 必然先占住名额，b 必然排在后面——b 想
// 进 Run 只能等 a 释放名额，而 a 只在 cancel 之后才被放行。sleep 只是让
// 「b 已在 acquire 里」成为常态路径，删掉它结论仍成立（cancel 早于 acquire
// 时，select 只有 ctx 分支可走，同样 canceled）。
func TestCancelInterruptsQueuedNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var bReason NodeFinishReason
	bDone := make(chan struct{})
	obs := ObserverFunc{Finished: func(_ Site, nodeID string, reason NodeFinishReason, _ error) {
		if nodeID == "b" {
			bReason = reason
			close(bDone)
		}
	}}
	g := mustNew(t, ctx, "test", WithMaxRunning(1), WithObserver(obs))

	k := NewKey[string]("queue.k")
	kReady := make(chan struct{})
	releaseA := make(chan struct{})
	mustAdd(t, g, NewNode("a", "", nil, Provides(k), func(rc *RunCtx) error {
		if err := Set(rc, k, "v"); err != nil {
			return err
		}
		close(kReady)
		<-releaseA // 占住唯一名额，直到 b 已离队
		return nil
	}))

	var entered atomic.Int32
	mustAdd(t, g, NewNode("b", "", Requires(k), nil, func(rc *RunCtx) error {
		entered.Add(1)
		return nil
	}))

	go func() {
		<-kReady
		time.Sleep(20 * time.Millisecond) // 让 b 走到 acquire（非必要：见上方注释）
		cancel()
		select {
		case <-bDone:
			// 期望路径：b 立刻以 canceled 离队。
		case <-time.After(2 * time.Second):
			// 旧行为：b 仍卡在 acquire 里，放行 a 让断言报告原因而不是挂死。
		}
		close(releaseA)
	}()

	err := runOn(g)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	if n := entered.Load(); n != 0 {
		t.Fatalf("取消后排队节点仍进入 Run %d 次，want 0", n)
	}
	select {
	case <-bDone:
		if bReason != NodeCanceled {
			t.Fatalf("b 终态 = %q, want canceled", bReason)
		}
	default:
		t.Fatal("b 没有终态回调")
	}
}

// TestAcquireReturnsSlotWhenCanceled 「名额空着」与「ctx 已取消」同时成立时，
// acquire 必须以取消为准并把名额放回去。这是 `select` 唯一会漏的形态：
// 两个分支都就绪时它随机挑（若 goroutine 已经阻塞在 select 里，runtime 会把它
// 提交给先就绪的那个分支，那种情况反而是确定的——所以直接打 acquire 本身，
// 不靠调度碰运气）。run it with -count 才说明问题：漏了复查约有一半概率失败。
func TestAcquireReturnsSlotWhenCanceled(t *testing.T) {
	g := mustNew(t, context.Background(), "test", WithMaxRunning(1))
	// 名额与取消域都在 `Execution` 上（一次运行自己的），所以先建一份执行再打 acquire。
	e := mustExec(t, g, context.Background(), Inputs{})
	ctx, cancel := context.WithCancel(e.ctx)
	cancel() // 已取消；同时名额是空的

	if err := e.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire = %v, want context.Canceled", err)
	}
	if n := len(e.lim.sem); n != 0 {
		t.Fatalf("取消后名额没放回去：len(lim.sem) = %d", n)
	}
	// 名额确实还能被正常用掉
	if err := e.acquire(e.ctx); err != nil {
		t.Fatalf("放回的名额不可用：%v", err)
	}
	e.release()
}
