package pulse

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// 阶段一第二步的验收：形状、度数、来源、顺序、毛病一次给全，以及「只读副本」。
// 判据都按**声明序稳定**写成字面量，所以哪一处顺序变了都当场红。

func nodeIDs(ns []NodeShape) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.ID)
	}
	return out
}

// TestTopologyNodesEdgesDegrees 钉住：度数按**相邻节点**数（不是 Key 数）、
// 同一对节点只留一条边并列出全部 Key、拓扑序满足边的方向。
func TestTopologyNodesEdgesDegrees(t *testing.T) {
	a1 := NewKey[string]("tp.a1")
	a2 := NewKey[string]("tp.a2")
	a3 := NewKey[string]("tp.a3")

	g := mustNew(t, context.Background(), "shape")
	mustAdd(t, g, NewNode("A", "上游甲", nil, Provides(a1, a2), func(rc *RunCtx) error {
		if err := Set(rc, a1, "x"); err != nil {
			return err
		}
		return Set(rc, a2, "y")
	}))
	mustAdd(t, g, NewNode("C", "", nil, Provides(a3), func(rc *RunCtx) error {
		return Set(rc, a3, "z")
	}))
	mustAdd(t, g, NewNode("B", "汇合", Requires(a1, a2, a3), nil, func(rc *RunCtx) error {
		for _, k := range []Key[string]{a1, a2, a3} {
			if _, err := Get(rc, k); err != nil {
				return err
			}
		}
		return nil
	}))

	top := g.Topology()

	if top.Graph.ID != "shape" {
		t.Fatalf("Graph.ID = %q", top.Graph.ID)
	}
	if got := nodeIDs(top.Nodes); !reflect.DeepEqual(got, []string{"A", "C", "B"}) {
		t.Fatalf("Nodes 按声明序：%v", got)
	}
	wantEdges := []EdgeShape{
		{From: "A", To: "B", Keys: []string{"tp.a1", "tp.a2"}},
		{From: "C", To: "B", Keys: []string{"tp.a3"}},
	}
	if !reflect.DeepEqual(top.Edges, wantEdges) {
		t.Fatalf("Edges = %+v\nwant %+v", top.Edges, wantEdges)
	}
	// A 喂了 B 两条键：仍只是一条边、B 的上游仍是两个节点
	if top.Nodes[0].OutDegree != 1 || top.Nodes[0].InDegree != 0 {
		t.Fatalf("A 度数 = in %d / out %d", top.Nodes[0].InDegree, top.Nodes[0].OutDegree)
	}
	if top.Nodes[2].InDegree != 2 || top.Nodes[2].OutDegree != 0 {
		t.Fatalf("B 度数 = in %d / out %d", top.Nodes[2].InDegree, top.Nodes[2].OutDegree)
	}
	if !reflect.DeepEqual(top.Order, []string{"A", "C", "B"}) {
		t.Fatalf("Order = %v", top.Order)
	}
	if top.Nodes[0].Description != "上游甲" || top.Nodes[2].Description != "汇合" {
		t.Fatalf("描述没进形状：%+v", top.Nodes)
	}

	// 键形状：类型记号是 reflect.Type.String()（与 YAML 的 type 记号同一套）；
	// Requires 上带来源，Provides 上恒为零值（一条输出的来源就是它自己）。
	b := top.Nodes[2].Requires[0]
	if b.Name != "tp.a1" || b.Type != "string" || b.From.Kind != SourceNode || b.From.NodeID != "A" {
		t.Fatalf("Requires[0] = %+v", b)
	}
	if p := top.Nodes[0].Provides[0]; p.From.Kind != SourceNone || p.From.NodeID != "" {
		t.Fatalf("Provides[0].From 必须是零值：%+v", p)
	}
	if top.Problems != nil {
		t.Fatalf("这张图没毛病：%v", top.Problems)
	}

	// 形状与运行无关：跑一遍不该改动它（这条同时是「跑完再问也一样」的一半）。
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
}

// TestTopologySourceKinds 钉住来源语义：外部 Seed 与节点来源分得清，而且
// **一个真叫 `seed` 的节点**照样分得清、也不会凭空多出一条边（阶段零修的那
// 两类静默错误，在公开面上就是这个形状）。
func TestTopologySourceKinds(t *testing.T) {
	seeded := NewKey[string]("tp.seeded")
	fromSeed := NewKey[string]("tp.fromseed")
	out := NewKey[string]("tp.out")

	g := mustNew(t, context.Background(), "srcs")
	if err := Seed(g, seeded, "v"); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("seed", "", nil, Provides(fromSeed), func(rc *RunCtx) error {
		return Set(rc, fromSeed, "n")
	}))
	mustAdd(t, g, NewNode("reader", "", Requires(seeded, fromSeed), Provides(out), func(rc *RunCtx) error {
		if _, err := Get(rc, seeded); err != nil {
			return err
		}
		if _, err := Get(rc, fromSeed); err != nil {
			return err
		}
		return Set(rc, out, "ok")
	}))

	top := g.Topology()
	req := top.Nodes[1].Requires
	if req[0].Name != "tp.seeded" || req[0].From.Kind != SourceSeed || req[0].From.NodeID != "" {
		t.Fatalf("外部 Seed 那一条 = %+v", req[0])
	}
	if req[1].From.Kind != SourceNode || req[1].From.NodeID != "seed" {
		t.Fatalf("节点来源那一条 = %+v", req[1])
	}
	if req[0].From.String() != "seed" || req[1].From.String() != "seed" {
		t.Fatalf("两条都叫 seed，但 Kind 分得开：%q / %q", req[0].From, req[1].From)
	}
	// Seed 不建边：只有那条节点来源的边，且它来自**那个节点**。
	want := []EdgeShape{{From: "seed", To: "reader", Keys: []string{"tp.fromseed"}}}
	if !reflect.DeepEqual(top.Edges, want) {
		t.Fatalf("Edges = %+v\nwant %+v", top.Edges, want)
	}
	if len(top.Problems) != 0 {
		t.Fatalf("没毛病：%v", top.Problems)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
}

// TestTopologyProblemsAllAtOnce 钉住「一次给全」：缺来源与依赖环同时存在时
// 两条都在 `Problems` 里（顺序固定：来源 → 环），而 `Validate` / `Start` 报的
// 仍是**首条**、文案与旧实现逐字相同。
func TestTopologyProblemsAllAtOnce(t *testing.T) {
	x := NewKey[string]("tp.x")
	y := NewKey[string]("tp.y")
	missing := NewKey[string]("tp.missing")
	missing2 := NewKey[string]("tp.missing2")
	done := func(rc *RunCtx) error { return nil }

	g := mustNew(t, context.Background(), "problems")
	mustAdd(t, g, NewNode("A", "", Requires(y), Provides(x), done))
	mustAdd(t, g, NewNode("B", "", Requires(x), Provides(y), done))
	mustAdd(t, g, NewNode("C", "", Requires(missing), nil, done))
	mustAdd(t, g, NewNode("D", "", Requires(missing2), nil, done))

	top := g.Topology()
	wantSrc := `pulse: node "C" requires "tp.missing" but nothing provides or seeds it`
	wantSrc2 := `pulse: node "D" requires "tp.missing2" but nothing provides or seeds it`
	wantCycle := `pulse: dependency cycle: A -> B -> A (A requires "tp.y", B requires "tp.x")`
	// 每个缺来源的 Requires 各一条（不是只报第一个），环那条排在它们后面。
	if len(top.Problems) != 3 {
		t.Fatalf("Problems = %v", top.Problems)
	}
	if got := top.Problems[0].Error(); got != wantSrc {
		t.Fatalf("Problems[0] = %q\nwant %q", got, wantSrc)
	}
	if got := top.Problems[1].Error(); got != wantSrc2 {
		t.Fatalf("Problems[1] = %q\nwant %q", got, wantSrc2)
	}
	if got := top.Problems[2].Error(); got != wantCycle {
		t.Fatalf("Problems[2] = %q\nwant %q", got, wantCycle)
	}

	// 有环照样返回结构（供绘图与排查）：节点、边都在，Order 只含剥得掉的那批。
	if got := nodeIDs(top.Nodes); !reflect.DeepEqual(got, []string{"A", "B", "C", "D"}) {
		t.Fatalf("Nodes = %v", got)
	}
	if !reflect.DeepEqual(top.Order, []string{"C", "D"}) {
		t.Fatalf("Order = %v，有环时只含剥得掉的那批", top.Order)
	}
	if len(top.Edges) != 2 {
		t.Fatalf("Edges = %+v", top.Edges)
	}

	// 首条不变：两个入口都报它，逐字相同。
	if err := g.Validate(); err == nil || err.Error() != wantSrc {
		t.Fatalf("Validate = %v", err)
	}
	if err := g.Start(); err == nil || err.Error() != wantSrc {
		t.Fatalf("Start = %v", err)
	}
}

// TestTopologyEachCycleReported 钉住「每条环各一条」：两个互不重叠的环
// 各报一条见证，第一条仍是旧实现那条；**不枚举**同一个环里的所有简单环。
func TestTopologyEachCycleReported(t *testing.T) {
	x := NewKey[string]("tp.c2.x")
	y := NewKey[string]("tp.c2.y")
	z := NewKey[string]("tp.c2.z")
	w := NewKey[string]("tp.c2.w")
	done := func(rc *RunCtx) error { return nil }

	g := mustNew(t, context.Background(), "cycles")
	mustAdd(t, g, NewNode("A", "", Requires(y), Provides(x), done))
	mustAdd(t, g, NewNode("B", "", Requires(x), Provides(y), done))
	mustAdd(t, g, NewNode("C", "", Requires(w), Provides(z), done))
	mustAdd(t, g, NewNode("D", "", Requires(z), Provides(w), done))

	top := g.Topology()
	if len(top.Problems) != 2 {
		t.Fatalf("两个环各一条：%v", top.Problems)
	}
	want0 := `pulse: dependency cycle: A -> B -> A (A requires "tp.c2.y", B requires "tp.c2.x")`
	want1 := `pulse: dependency cycle: C -> D -> C (C requires "tp.c2.w", D requires "tp.c2.z")`
	if got := top.Problems[0].Error(); got != want0 {
		t.Fatalf("Problems[0] = %q\nwant %q", got, want0)
	}
	if got := top.Problems[1].Error(); got != want1 {
		t.Fatalf("Problems[1] = %q\nwant %q", got, want1)
	}
	if len(top.Order) != 0 {
		t.Fatalf("全是环，剥得掉的一批为空：%v", top.Order)
	}
	if err := g.Validate(); err == nil || err.Error() != want0 {
		t.Fatalf("Validate 报首条：%v", err)
	}
}

// TestTopologyStreamProblems 钉住流式那一类也走同一个出口：每条出口一条
// （没人读 / 被两个各写各的读了），`Validate` 仍报首条。
func TestTopologyStreamProblems(t *testing.T) {
	src := NewKey[<-chan int]("tp.s.src")
	o1 := NewKey[<-chan int]("tp.s.o1")
	o2 := NewKey[<-chan int]("tp.s.o2")

	// 容量 4：装配期那三道快速检查都过得去（Tee 要 3），所以毛病只在 Start 这一层。
	g := mustNew(t, context.Background(), "stream", WithMaxRunning(4))
	send := func(rc *RunCtx, send func(int) error) error { return nil }
	sink := func(rc *RunCtx, v int) error { return nil }
	if err := Produce(g, "p", src, send); err != nil {
		t.Fatal(err)
	}
	if err := Tee(g, "t", src, Keys(o1, o2)); err != nil {
		t.Fatal(err)
	}
	if err := Consume(g, "c1", o1, sink); err != nil {
		t.Fatal(err)
	}
	if err := Consume(g, "c2", o1, sink); err != nil {
		t.Fatal(err)
	}

	top := g.Topology()
	if len(top.Problems) != 2 {
		t.Fatalf("每条出口一条：%v", top.Problems)
	}
	if got := top.Problems[0].Error(); !strings.Contains(got, "tp.s.o1") ||
		!strings.Contains(got, "read by 2 nodes (c1, c2)") {
		t.Fatalf("Problems[0] = %q", got)
	}
	wantNoConsumer := `pulse: node "t" provides "tp.s.o2" but nothing consumes it: ` +
		`a stream output nobody reads blocks the sender forever`
	if got := top.Problems[1].Error(); got != wantNoConsumer {
		t.Fatalf("Problems[1] = %q\nwant %q", got, wantNoConsumer)
	}
	if err := g.Validate(); err == nil || err.Error() != top.Problems[0].Error() {
		t.Fatalf("Validate 报首条：%v", err)
	}
	if err := g.Start(); err == nil || err.Error() != top.Problems[0].Error() {
		t.Fatalf("Start 报首条：%v", err)
	}
}

// TestTopologyStreamCapacityProblem 钉住名额那一条：装配期全过、`Start` 才拒
// 的那张图（实测它跑到运行期就是死锁），在 `Problems` 里也看得见。
func TestTopologyStreamCapacityProblem(t *testing.T) {
	in := NewKey[<-chan int]("tp.cap.src")
	out := NewKey[<-chan int]("tp.cap.out")

	g := mustNew(t, context.Background(), "cap", WithMaxRunning(2))
	send := func(rc *RunCtx, send func(int) error) error { return nil }
	sink := func(rc *RunCtx, v int) error { return nil }
	if err := Produce(g, "src", in, send); err != nil {
		t.Fatal(err)
	}
	if err := Tee(g, "tee", in, Keys(out)); err != nil {
		t.Fatal(err)
	}
	if err := Consume(g, "sink", out, sink); err != nil {
		t.Fatal(err)
	}

	want := `pulse: stream needs 3 slots at the same time, but WithMaxRunning(2): src, tee, sink`
	top := g.Topology()
	if len(top.Problems) != 1 || top.Problems[0].Error() != want {
		t.Fatalf("Problems = %v\nwant %q", top.Problems, want)
	}
	// 拒在 Start：不是跑到一半卡死。
	if err := g.Run(); err == nil || err.Error() != want {
		t.Fatalf("Run = %v", err)
	}
}

// TestTopologyStableAndIsolated 钉住两条只读契约：同一张图反复问逐字段相同
// （跑完再问也相同），而且拿到的是**副本**——改它不影响图。
func TestTopologyStableAndIsolated(t *testing.T) {
	a1 := NewKey[string]("tp.st.a1")
	a2 := NewKey[string]("tp.st.a2")
	g := mustNew(t, context.Background(), "stable")
	mustAdd(t, g, NewNode("A", "写", nil, Provides(a1, a2), func(rc *RunCtx) error {
		if err := Set(rc, a1, "x"); err != nil {
			return err
		}
		return Set(rc, a2, "y")
	}))
	mustAdd(t, g, NewNode("B", "读", Requires(a1, a2), nil, func(rc *RunCtx) error {
		for _, k := range []Key[string]{a1, a2} {
			if _, err := Get(rc, k); err != nil {
				return err
			}
		}
		return nil
	}))

	first := g.Topology()
	second := g.Topology()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("两次问不一样：\n%+v\n%+v", first, second)
	}

	// 改副本：图与下一次问都不受影响。
	first.Graph.ID = "MUTATED"
	first.Order[0] = "MUTATED"
	first.Nodes[0].ID = "MUTATED"
	first.Nodes[0].Requires = nil
	first.Nodes[1].Requires[0].Name = "MUTATED"
	first.Edges[0].Keys[0] = "MUTATED"
	third := g.Topology()
	if !reflect.DeepEqual(second, third) {
		t.Fatalf("改副本回头影响图：\n%+v\n%+v", second, third)
	}
	if third.Graph.ID != "stable" || third.Edges[0].Keys[0] != "tp.st.a1" {
		t.Fatalf("副本没隔离：%+v", third)
	}

	// 跑完再问：形状不随运行变。
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	fourth := g.Topology()
	if !reflect.DeepEqual(second, fourth) {
		t.Fatalf("运行改变了形状：\n%+v\n%+v", second, fourth)
	}
}

// TestTopologyIgnoresDescription 钉住描述**不参与**拓扑：除描述外逐字段相同。
func TestTopologyIgnoresDescription(t *testing.T) {
	build := func(id, graphDesc string, withDesc bool) *Graph {
		a := NewKey[string]("tp.d.a")
		b := NewKey[string]("tp.d.b")
		g, err := New(context.Background(), id, graphDesc)
		if err != nil {
			t.Fatal(err)
		}
		descA, descB := "", ""
		if withDesc {
			descA, descB = "上游", "下游"
		}
		mustAdd(t, g, NewNode("A", descA, nil, Provides(a), func(rc *RunCtx) error {
			return Set(rc, a, "x")
		}))
		mustAdd(t, g, NewNode("B", descB, Requires(a), Provides(b), func(rc *RunCtx) error {
			if _, err := Get(rc, a); err != nil {
				return err
			}
			return Set(rc, b, "y")
		}))
		return g
	}

	with := build("plain", "图说明", true).Topology()
	without := build("plain", "", false).Topology()
	with.Graph.Description = ""
	for i := range with.Nodes {
		with.Nodes[i].Description = ""
	}
	if !reflect.DeepEqual(with, without) {
		t.Fatalf("描述影响了拓扑：\n%+v\n%+v", with, without)
	}
}
