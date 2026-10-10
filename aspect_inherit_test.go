package pulse

import (
	"context"
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
func childWith(t *testing.T, id string, n int, cin Key[string], body func(*RunCtx) error,
	opts ...Option) func(*SubCtx) (*Graph, error) {
	t.Helper()
	return func(sc *SubCtx) (*Graph, error) {
		child, err := New(sc.Context(), sc.GraphID(), opts...)
		if err != nil {
			return nil, err
		}
		for i := 0; i < n; i++ {
			if err := child.Add(NewNode(fmt.Sprintf("%s-node%d", id, i), Requires(cin), nil, body)); err != nil {
				return nil, err
			}
		}
		return child, nil
	}
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
	if err := Seed(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step", []SubBind{In(in, cin)},
		childWith(t, "c", 2, cin, func(*RunCtx) error { ran.Add(1); return nil })); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
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
	if err := Seed(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step", []SubBind{In(in, cin)},
		childWith(t, "c", 1, cin, func(*RunCtx) error { return nil },
			WithAspects(r.aspect("inner")))); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
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
	if err := Seed(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step", []SubBind{In(in, cin)},
		childWith(t, "c", 2, cin, func(*RunCtx) error { ran.Add(1); return nil },
			WithoutAspects(), WithAspects(r.aspect("own")))); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
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
	if err := Seed(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step", []SubBind{In(in, cin)},
		childWith(t, "c", 2, cin, func(*RunCtx) error { return nil },
			WithAspects(empty...))); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
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
	if err := Seed(g, in, "v"); err != nil {
		t.Fatal(err)
	}

	leaf := func(sc *SubCtx) (*Graph, error) {
		child, err := New(sc.Context(), sc.GraphID())
		if err != nil {
			return nil, err
		}
		return child, child.Add(NewNode("leaf", Requires(l2), nil, func(rc *RunCtx) error {
			r.append("BODY")
			return nil
		}))
	}
	mid := func(sc *SubCtx) (*Graph, error) {
		child, err := New(sc.Context(), sc.GraphID())
		if err != nil {
			return nil, err
		}
		if err := Sub(child, "step2", []SubBind{In(l1, l2)}, leaf); err != nil {
			return nil, err
		}
		return child, nil
	}
	if err := Sub(g, "step1", []SubBind{In(in, l1)}, mid); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	want := "in A, in A, in A, BODY, out A, out A, out A"
	if got := strings.Join(r.all(), ", "); got != want {
		t.Fatalf("每个节点只该带一层 A（最内层也是 1 层）：\n got %s\nwant %s", got, want)
	}
}
