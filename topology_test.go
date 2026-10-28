package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
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
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
}

// TestTopologySourceKinds 钉住来源语义：声明过的入口与节点来源分得清，而且
// **一个真叫 `seed` 的节点**照样分得清、也不会凭空多出一条边（阶段零修的那
// 两类静默错误，在公开面上就是这个形状）。
func TestTopologySourceKinds(t *testing.T) {
	seeded := NewKey[string]("tp.seeded")
	fromSeed := NewKey[string]("tp.fromseed")
	out := NewKey[string]("tp.out")

	g := mustNew(t, context.Background(), "srcs")
	declareIn(t, g, NewPort(seeded))
	if err := seedOn(g, seeded, "v"); err != nil {
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
	if req[0].Name != "tp.seeded" || req[0].From.Kind != SourceInput || req[0].From.NodeID != "" {
		t.Fatalf("声明过的入口那一条 = %+v", req[0])
	}
	if req[1].From.Kind != SourceNode || req[1].From.NodeID != "seed" {
		t.Fatalf("节点来源那一条 = %+v", req[1])
	}
	// 两种来源的**公开文本**也要分得开，否则这个歧义会从日志 / 格式化输出绕回来
	// （评审在 #308 上点的这条）。
	if req[0].From.String() != "input" || req[1].From.String() != `node "seed"` {
		t.Fatalf("两种来源在公开出口上必须分得开：%q / %q", req[0].From, req[1].From)
	}
	// 入口不建边：只有那条节点来源的边，且它来自**那个节点**。
	want := []EdgeShape{{From: "seed", To: "reader", Keys: []string{"tp.fromseed"}}}
	if !reflect.DeepEqual(top.Edges, want) {
		t.Fatalf("Edges = %+v\nwant %+v", top.Edges, want)
	}
	if len(top.Problems) != 0 {
		t.Fatalf("没毛病：%v", top.Problems)
	}
	if err := runOn(g); err != nil {
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
	wantSrc := `pulse: node "C" requires "tp.missing" but nothing provides it ` +
		`(declare it with DeclareInput if the value comes from outside)`
	wantSrc2 := `pulse: node "D" requires "tp.missing2" but nothing provides it ` +
		`(declare it with DeclareInput if the value comes from outside)`
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
	if err := startOn(g); err == nil || err.Error() != wantSrc {
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

// TestTopologyCycleWitnessStaysInCycle 钉住评审给的那个反例：真环是 A↔B，另有一个
// A→C 的旁支（C 没有出边）。
//
// 旧实现顺着「第一条出边」走出了环、走到 C 这个死胡同，还把路径首尾相接报成
// `A -> C -> A ()`——那是个**不存在**的环（括号里是空的）；更糟的是 `Problems` 随后
// 会把 A、C 当成「已报过」，真正的环 A↔B 一条都不出现。现在只在**强连通分量内部**
// 走，报的一定是真环。
func TestTopologyCycleWitnessStaysInCycle(t *testing.T) {
	x := NewKey[string]("tp.w.x")
	y := NewKey[string]("tp.w.y")
	done := func(rc *RunCtx) error { return nil }

	g := mustNew(t, context.Background(), "witness")
	// 声明序故意是 A、C、B：让 A 的「第一条出边」指向旁支 C（旧走法就是这么出错的）
	mustAdd(t, g, NewNode("A", "", Requires(y), Provides(x), done))
	mustAdd(t, g, NewNode("C", "", Requires(x), nil, done))
	mustAdd(t, g, NewNode("B", "", Requires(x), Provides(y), done))

	want := `pulse: dependency cycle: A -> B -> A (A requires "tp.w.y", B requires "tp.w.x")`
	top := g.Topology()
	if len(top.Problems) != 1 {
		t.Fatalf("Problems = %v\nwant 只有一条（真环那条）", top.Problems)
	}
	if got := top.Problems[0].Error(); got != want {
		t.Fatalf("Problems[0] = %q\nwant %q", got, want)
	}
	// 两个入口逐字相同；旁支 C 不被误报成环的一部分。
	if err := g.Validate(); err == nil || err.Error() != want {
		t.Fatalf("Validate = %v\nwant %q", err, want)
	}
	if err := startOn(g); err == nil || err.Error() != want {
		t.Fatalf("Start = %v\nwant %q", err, want)
	}
	// 结构照样给全：节点与边都在（B→A、A→C、A→B 三条），但**剥得掉的一批是空的**
	// ——C 等的是环里的输出，它自己也没法先跑（这正是「有来源不等于能满足」）。
	if len(top.Nodes) != 3 || len(top.Edges) != 3 {
		t.Fatalf("Nodes/Edges = %d/%d，有环也要给全", len(top.Nodes), len(top.Edges))
	}
	if len(top.Order) != 0 {
		t.Fatalf("Order = %v，这张图没有能先跑的节点", top.Order)
	}
}

// TestTopologyOneWitnessPerSCC 钉住「一个强连通分量一条见证」：A↔B 与 C↔D 两个顶点
// 不相交的环，再加 B→C、D→A 之后四个节点同属**一个**分量——只报一条，不是两条
// （#303 设计稿的口径；旧实现按「逐轮摘点」会报两条，也把每轮都重跑一遍 Kahn）。
func TestTopologyOneWitnessPerSCC(t *testing.T) {
	ka := NewKey[string]("tp.scc.ka")
	kb := NewKey[string]("tp.scc.kb")
	kc := NewKey[string]("tp.scc.kc")
	kd := NewKey[string]("tp.scc.kd")
	done := func(rc *RunCtx) error { return nil }

	g := mustNew(t, context.Background(), "onescc")
	mustAdd(t, g, NewNode("A", "", Requires(kb, kc), Provides(ka), done)) // B→A、C→A
	mustAdd(t, g, NewNode("B", "", Requires(ka), Provides(kb), done))     // A→B
	mustAdd(t, g, NewNode("C", "", Requires(kd, kb), Provides(kc), done)) // D→C、B→C
	mustAdd(t, g, NewNode("D", "", Requires(kc), Provides(kd), done))     // C→D

	want := `pulse: dependency cycle: A -> B -> A (A requires "tp.scc.kb", B requires "tp.scc.ka")`
	top := g.Topology()
	if len(top.Problems) != 1 {
		t.Fatalf("一个分量一条：%v", top.Problems)
	}
	if got := top.Problems[0].Error(); got != want {
		t.Fatalf("Problems[0] = %q\nwant %q", got, want)
	}
	if err := g.Validate(); err == nil || err.Error() != want {
		t.Fatalf("Validate = %v", err)
	}
}

// TestTopologySourceStringDistinguishesKinds 钉住公开出口上的来源名**两两可分**：节点 id
// 是任意用户字符串，只给 `seed` 开小灶挡不住另外两条路——节点叫 `none` 的会与「没有来源」
// 撞，字面量 id `node "seed"` 会与节点 `seed` 的输出撞（两条都实测撞上了，评审在 #308 上
// 点的这条）。内部报错走同一份规则（`sourceName`），那边由 `source_test.go` 钉住。
func TestTopologySourceStringDistinguishesKinds(t *testing.T) {
	cases := []struct {
		name string
		src  Source
		want string
	}{
		{"声明过的入口", Source{Kind: SourceInput}, "input"},
		{"普通节点", Source{Kind: SourceNode, NodeID: "load"}, `node "load"`},
		{"节点 id 恰好是 seed", Source{Kind: SourceNode, NodeID: "seed"}, `node "seed"`},
		{"节点 id 恰好是 none", Source{Kind: SourceNode, NodeID: "none"}, `node "none"`},
		{"节点 id 本身长成 node \"seed\"", Source{Kind: SourceNode, NodeID: `node "seed"`}, `node "node \"seed\""`},
		{"没有来源（零值）", Source{}, "none"},
	}
	seen := map[string]string{}
	for _, c := range cases {
		got := c.src.String()
		if got != c.want {
			t.Fatalf("%s：String() = %q，want %q", c.name, got, c.want)
		}
		if prev, dup := seen[got]; dup {
			t.Fatalf("%s 与 %s 都写成 %q——公开出口上的来源名必须两两可分", c.name, prev, got)
		}
		seen[got] = c.name
	}
}

// costGraphs 造两张**节点数相同**（V = 2k）的含环图：一张是 k 个互不相连的 2 节点环
// （分量数 = k），一张是一个 2k 节点的大环（分量数 = 1）。
func costGraphs(t *testing.T, k int) (many, one *Graph) {
	t.Helper()
	done := func(*RunCtx) error { return nil }
	many = mustNew(t, context.Background(), "cost-many")
	for i := 0; i < k; i++ {
		a := NewKey[string](fmt.Sprintf("cost.many.%d.a", i))
		b := NewKey[string](fmt.Sprintf("cost.many.%d.b", i))
		mustAdd(t, many, NewNode(fmt.Sprintf("A%d", i), "", Requires(b), Provides(a), done))
		mustAdd(t, many, NewNode(fmt.Sprintf("B%d", i), "", Requires(a), Provides(b), done))
	}
	one = mustNew(t, context.Background(), "cost-one")
	ring := make([]Key[string], 2*k)
	for i := range ring {
		ring[i] = NewKey[string](fmt.Sprintf("cost.one.%d", i))
	}
	for i := 0; i < 2*k; i++ {
		mustAdd(t, one, NewNode(fmt.Sprintf("N%d", i), "",
			Requires(ring[(i-1+2*k)%(2*k)]), Provides(ring[i]), done))
	}
	return many, one
}

// TestTopologyWitnessCostScalesWithComponent 钉住「每个环分量的额外开销按**该分量**的大小
// 增长，不按全图大小」：每个分量都开一份 `len(g.nodes)` 的成员数组时，k 个互不相连的小环
// 就是 O(k·V)（评审在 #308 上点的这条；实测 V=4000、k=2000 时多分配 7.4 MB，改成按分量
// 规模开之后同一对图差 −0.6 MB）。
//
// 判据用**分配字节数**而不是墙钟时间——时间在本机与 CI 之间不是一个可比的量（既有教训：
// 同一句 `time.Since(time.Now())` 本机 0ns、CI 90ns），而分配字节数跨平台确定。两张图
// **V 相同**、只有分量数不同，所以「建图 + 建形状」那部分开销两边一致，差值就是见证那一步
// 的额外开销（外加：分量多的一方要多造 k 条报错文案，那部分本身是 O(k)、已经算在下面这个
// 上限里）。
func TestTopologyWitnessCostScalesWithComponent(t *testing.T) {
	const (
		k      = 2000    // 两张图都是 V = 4000
		budget = 2 << 20 // 2 MiB：修复后实测 −0.6 MB，退回每个分量一份全图数组是 +7.8 MB
	)
	many, one := costGraphs(t, k)
	// 先确认两张图真的是「k 个分量」与「1 个分量」——否则下面的差值不说明任何事。
	if got := len(many.Topology().Problems); got != k {
		t.Fatalf("k 个小环应当报 %d 条，got %d", k, got)
	}
	if got := len(one.Topology().Problems); got != 1 {
		t.Fatalf("一个大环应当报 1 条，got %d", got)
	}

	alloc := func(g *Graph) uint64 {
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		_ = g.Topology()
		runtime.ReadMemStats(&m1)
		return m1.TotalAlloc - m0.TotalAlloc
	}
	diff := int64(alloc(many)) - int64(alloc(one))
	if diff > budget {
		t.Fatalf("%d 个小环比一个大环（同 V=%d）多分配 %d B，上限 %d——"+
			"环见证的额外开销按全图大小长了", k, 2*k, diff, budget)
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
	if err := Produce(g, "p", "", src, send); err != nil {
		t.Fatal(err)
	}
	if err := Tee(g, "t", "", src, Keys(o1, o2)); err != nil {
		t.Fatal(err)
	}
	if err := Consume(g, "c1", "", o1, sink); err != nil {
		t.Fatal(err)
	}
	if err := Consume(g, "c2", "", o1, sink); err != nil {
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
	if err := startOn(g); err == nil || err.Error() != top.Problems[0].Error() {
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
	if err := Produce(g, "src", "", in, send); err != nil {
		t.Fatal(err)
	}
	if err := Tee(g, "tee", "", in, Keys(out)); err != nil {
		t.Fatal(err)
	}
	if err := Consume(g, "sink", "", out, sink); err != nil {
		t.Fatal(err)
	}

	want := `pulse: stream needs 3 slots at the same time, but WithMaxRunning(2): src, tee, sink`
	top := g.Topology()
	if len(top.Problems) != 1 || top.Problems[0].Error() != want {
		t.Fatalf("Problems = %v\nwant %q", top.Problems, want)
	}
	// 拒在 Start：不是跑到一半卡死。
	if err := runOn(g); err == nil || err.Error() != want {
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
	if err := runOn(g); err != nil {
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
		g, err := New(id, graphDesc)
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

func topologyPortNames(ports []KeyShape) []string {
	names := make([]string, len(ports))
	for i, p := range ports {
		names[i] = p.Name
	}
	return names
}

func TestTopologyPortsIncludeUnusedAndPassThrough(t *testing.T) {
	unused, cin, pass := NewKey[string]("ports.unused"), NewKey[string]("ports.cin"), NewKey[string]("ports.pass")
	cout, internal := NewKey[string]("ports.cout"), NewKey[int]("ports.internal")
	child := mustNew(t, context.Background(), "ports.child")
	declareIn(t, child, NewPort(unused), NewPort(cin), NewPort(pass))
	declareIn(t, child, NewPort(cin), NewPort(unused)) // 幂等重声明不改变顺序。
	if err := child.DeclareOutput(NewPort(pass), NewPort(cout)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, child, NewNode("work", "", Requires(cin), Deps(Provides(cout), Provides(internal)), nil))

	parentIn, parentPass, parentOut := NewKey[string]("ports.pin"), NewKey[string]("ports.ppass"), NewKey[string]("ports.pout")
	parent := mustNew(t, context.Background(), "ports.parent")
	declareIn(t, parent, NewPort(parentIn), NewPort(parentPass))
	if err := Sub(parent, "step", "", []SubBind{In(parentIn, cin), In(parentPass, pass), Out(pass, parentOut)}, child); err != nil {
		t.Fatal(err)
	}
	top := parent.Topology()
	shape := top.Nodes[0].Sub.Graph
	if got := topologyPortNames(shape.InPorts); !reflect.DeepEqual(got, []string{unused.Name(), cin.Name(), pass.Name()}) {
		t.Fatalf("进口完整且按声明序: %v", got)
	}
	if got := topologyPortNames(shape.OutPorts); !reflect.DeepEqual(got, []string{pass.Name(), cout.Name()}) {
		t.Fatalf("出口完整且按声明序: %v", got)
	}
	for _, p := range shape.InPorts {
		if p.Type != "string" || p.From.Kind != SourceInput {
			t.Fatalf("进口来源词表: %+v", p)
		}
	}
	for _, p := range shape.OutPorts {
		if p.From.Kind != SourceNone {
			t.Fatalf("出口不重复来源: %+v", p)
		}
		if p.Name == internal.Name() {
			t.Fatal("内部键不是公开端口")
		}
	}
	for _, b := range top.Nodes[0].Binds {
		ports := shape.OutPorts
		if b.In {
			ports = shape.InPorts
		}
		found := false
		for _, p := range ports {
			if p.Name == b.Child {
				found = true
			}
		}
		if !found {
			t.Fatalf("绑定端口在子树不可见: %+v", b)
		}
	}
	if len(top.Graph.OutPorts) != 0 {
		t.Fatal("父侧 Provides 不自动变为声明出口")
	}
	// 返回的端口切片同样是副本。
	top.Nodes[0].Sub.Graph.InPorts[0].Name = "changed"
	if got := child.Topology().Graph.InPorts[0].Name; got != unused.Name() {
		t.Fatalf("端口副本污染定义: %q", got)
	}
}

func TestTopologyPortOrderIsTransactional(t *testing.T) {
	g := mustNew(t, context.Background(), "ports.atomic")
	z, a := NewKey[string]("z"), NewKey[string]("a")
	declareIn(t, g, NewPort(z), NewPort(z))
	if err := g.DeclareInput(NewPort(a), NewPort(NewKey[int]("z"))); err == nil {
		t.Fatal("跨型声明应拒绝")
	}
	if got := topologyPortNames(g.Topology().Graph.InPorts); !reflect.DeepEqual(got, []string{"z"}) {
		t.Fatalf("失败声明不能留下顺序记录: %v", got)
	}
	declareIn(t, g, NewPort(a), NewPort(z))
	if got := topologyPortNames(g.Topology().Graph.InPorts); !reflect.DeepEqual(got, []string{"z", "a"}) {
		t.Fatalf("声明序不是字典序/重复顺序: %v", got)
	}
}

// 静态子树的**静态可见性**：装配完（不运行）就能递归看全树、按挂载点报毛病，
// 而且读这些入口不会改动任何定义。
//
// 这一组是票面验收里「A/B/C 各自构建、D 组合后**无运行**即可递归看全树并报全部静态错」
// 与「同一子图多处挂载不改定义」两条的落地用例。
func TestStaticSubtreeVisibility(t *testing.T) {
	ctx := context.Background()

	// leafWithPorts 造一张声明了入口 / 出口、且**有一个死节点**的子图（hungry 读的键
	// 没人提供、也没声明成入口）。
	leafWithPorts := func(t *testing.T, id string, dead bool) (*Graph, Key[string], Key[string]) {
		t.Helper()
		in := NewKey[string](id + ".in")
		out := NewKey[string](id + ".out")
		g := mustNew(t, ctx, id)
		if err := g.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := g.DeclareOutput(NewPort(out)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		mustAdd(t, g, NewNode("step", "", Requires(in), Provides(out), func(rc *RunCtx) error {
			v, err := Get(rc, in)
			if err != nil {
				return err
			}
			return Set(rc, out, v)
		}))
		if dead {
			ghost := NewKey[string](id + ".ghost")
			mustAdd(t, g, NewNode("hungry", "", Requires(ghost), nil, func(rc *RunCtx) error { return nil }))
		}
		return g, in, out
	}

	t.Run("无运行即可递归看全树：节点、路径、绑定", func(t *testing.T) {
		leaf, lin, lout := leafWithPorts(t, "tv.leaf", false)
		dIn := NewKey[string]("tv.in")
		dOut := NewKey[string]("tv.out")
		d := mustNew(t, ctx, "tv.parent")
		if err := Sub(d, "step", "", []SubBind{In(dIn, lin), Out(lout, dOut)}, leaf); err != nil {
			t.Fatalf("Sub = %v", err)
		}

		top := d.Topology()
		if top.Graph.Path != "" {
			t.Fatalf("根图的 Path 该是空串，got %q", top.Graph.Path)
		}
		if len(top.Nodes) != 1 {
			t.Fatalf("父图该有一个节点，got %+v", top.Nodes)
		}
		sub := top.Nodes[0].Sub
		if sub == nil {
			t.Fatal("装配完之后 Nodes[0].Sub 该是子图的静态形状——不用先跑")
		}
		if sub.Graph.ID != "tv.leaf" || sub.Graph.Path != "step" {
			t.Fatalf("子图静态形状：id=%q path=%q，want id=tv.leaf path=step", sub.Graph.ID, sub.Graph.Path)
		}
		if len(sub.Nodes) != 1 || sub.Nodes[0].ID != "step" {
			t.Fatalf("子树的节点没递归下来：%+v", sub.Nodes)
		}
		binds := top.Nodes[0].Binds
		if len(binds) != 2 || binds[0].In != true || binds[1].In != false {
			t.Fatalf("父子接线不对：%+v", binds)
		}
		if binds[0].Parent != "tv.in" || binds[0].Child != "tv.leaf.in" {
			t.Fatalf("In 那头的两端不对：%+v", binds[0])
		}
		if binds[1].Parent != "tv.out" || binds[1].Child != "tv.leaf.out" {
			t.Fatalf("Out 那头的两端不对：%+v", binds[1])
		}
	})

	t.Run("两级嵌套的路径按挂载位置推出来", func(t *testing.T) {
		leaf, lin, lout := leafWithPorts(t, "n2.leaf", false)
		midIn := NewKey[string]("n2.mid.in")
		midOut := NewKey[string]("n2.mid.out")
		mid := mustNew(t, ctx, "n2.mid")
		// 中间层自己也是「被别人挂的子图」，所以它同样要把两头声明成端口
		if err := mid.DeclareInput(NewPort(midIn)); err != nil {
			t.Fatalf("mid.DeclareInput = %v", err)
		}
		if err := mid.DeclareOutput(NewPort(midOut)); err != nil {
			t.Fatalf("mid.DeclareOutput = %v", err)
		}
		if err := Sub(mid, "inner", "", []SubBind{In(midIn, lin), Out(lout, midOut)}, leaf); err != nil {
			t.Fatalf("内层挂载 = %v", err)
		}
		dIn := NewKey[string]("n2.in")
		dOut := NewKey[string]("n2.out")
		d := mustNew(t, ctx, "n2.parent")
		if err := d.DeclareInput(NewPort(dIn)); err != nil {
			t.Fatalf("parent.DeclareInput = %v", err)
		}
		if err := Sub(d, "outer", "", []SubBind{In(dIn, midIn), Out(midOut, dOut)}, mid); err != nil {
			t.Fatalf("外层挂载 = %v", err)
		}

		top := d.Topology()
		inner := top.Nodes[0].Sub
		if inner == nil || inner.Graph.Path != "outer" {
			t.Fatalf("中间层路径该是 outer，got %+v", inner.Graph)
		}
		leafTop := inner.Nodes[0].Sub
		if leafTop == nil || leafTop.Graph.Path != "outer/inner" {
			t.Fatalf("最内层路径该是 outer/inner，got %+v", leafTop.Graph)
		}
	})

	t.Run("无运行即可报子图的静态毛病，报文点明是哪一处挂载", func(t *testing.T) {
		leaf, lin, lout := leafWithPorts(t, "bad.leaf", true)
		dIn := NewKey[string]("bad.in")
		dOut := NewKey[string]("bad.out")
		d := mustNew(t, ctx, "bad.parent")
		// 父侧那条输入也要有来源（这里是「声明的入口」），否则先报的是父图自己的毛病
		if err := d.DeclareInput(NewPort(dIn)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := Sub(d, "mount", "", []SubBind{In(dIn, lin), Out(lout, dOut)}, leaf); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		err := d.Validate()
		if err == nil {
			t.Fatal("子图里那个死节点该在父图上就被报出来——不必先跑")
		}
		if !strings.Contains(err.Error(), `mounted at "mount"`) {
			t.Fatalf("报文该点明是哪一处挂载，got %v", err)
		}
		if !strings.Contains(err.Error(), "bad.leaf.ghost") {
			t.Fatalf("报文该带着子图那条键的病根，got %v", err)
		}
		if got := len(d.Topology().Problems); got == 0 {
			t.Fatal("Topology().Problems 也该有这条")
		}
	})

	t.Run("同一子图挂两处：两处各一条，各自点明位置", func(t *testing.T) {
		leaf, lin, lout := leafWithPorts(t, "tw2.leaf", true)
		dIn := NewKey[string]("tw2.in")
		o1 := NewKey[string]("tw2.out1")
		o2 := NewKey[string]("tw2.out2")
		d := mustNew(t, ctx, "tw2.parent")
		if err := Sub(d, "m1", "", []SubBind{In(dIn, lin), Out(lout, o1)}, leaf); err != nil {
			t.Fatalf("m1 = %v", err)
		}
		if err := Sub(d, "m2", "", []SubBind{In(dIn, lin), Out(lout, o2)}, leaf); err != nil {
			t.Fatalf("m2 = %v", err)
		}

		probs := d.Topology().Problems
		var atM1, atM2 int
		for _, p := range probs {
			switch {
			case strings.Contains(p.Error(), `mounted at "m1"`):
				atM1++
			case strings.Contains(p.Error(), `mounted at "m2"`):
				atM2++
			}
		}
		if atM1 != 1 || atM2 != 1 {
			t.Fatalf("同一子图挂两处该各报一条（m1=%d m2=%d），got %d 条：%v", atM1, atM2, len(probs), probs)
		}
	})

	t.Run("读静态入口不改动定义", func(t *testing.T) {
		leaf, lin, lout := leafWithPorts(t, "ro.leaf", false)
		dIn := NewKey[string]("ro.in")
		dOut := NewKey[string]("ro.out")
		d := mustNew(t, ctx, "ro.parent")
		if err := Sub(d, "mount", "", []SubBind{In(dIn, lin), Out(lout, dOut)}, leaf); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		before := leaf.Topology()
		_ = d.Topology()
		_ = d.Validate()
		after := leaf.Topology()
		// 子图自己那份视图里的 path 恒为空：路径是按**挂载位置**在每次视图里现推的。
		// 定义上已经没有可被盖写的 path 字段了（那正是「读一遍就盖章」那条旧路的根源），
		// 所以这里钉的是**语义**：从它自己看永远是根，从父图看才是挂载位置。
		if before.Graph.Path != "" || after.Graph.Path != "" {
			t.Fatalf("子图定义自己的 path 不该被读取动作盖章：before=%q after=%q",
				before.Graph.Path, after.Graph.Path)
		}
		if len(before.Nodes) != len(after.Nodes) || before.Graph.ID != after.Graph.ID {
			t.Fatalf("读一遍把定义改了：before=%+v after=%+v", before.Graph, after.Graph)
		}
		// 父图那条视图里，子图这一层按挂载位置推成 "mount"——两处视图互不干扰。
		if top := d.Topology(); top.Nodes[0].Sub == nil || top.Nodes[0].Sub.Graph.Path != "mount" {
			t.Fatalf("父图视图里的挂载路径 = %+v，want mount", top.Nodes[0].Sub)
		}
		if got := leaf.Topology().Graph.Path; got != "" {
			t.Fatalf("读过父图之后，子图自己那份的 path 变成了 %q——定义被改了", got)
		}
	})

	t.Run("观察者照旧继承给静态子图（子执行建得出来就有有效值）", func(t *testing.T) {
		leaf, lin, lout := leafWithPorts(t, "obs.leaf", false)
		dIn := NewKey[string]("obs.in")
		dOut := NewKey[string]("obs.out")

		var seen []string
		obs := ObserverFunc{
			Running: func(_ Site, nodeID string) { seen = append(seen, "run:"+nodeID) },
			Finished: func(_ Site, nodeID string, reason NodeFinishReason, err error) {
				seen = append(seen, "fin:"+nodeID)
			},
		}
		d := mustNew(t, ctx, "obs.parent", WithObserver(obs))
		if err := Sub(d, "mount", "", []SubBind{In(dIn, lin), Out(lout, dOut)}, leaf); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		declareIn(t, d, NewPort(dIn))
		if err := seedOn(d, dIn, "v"); err != nil {
			t.Fatalf("Seed = %v", err)
		}
		if err := runOn(d); err != nil {
			t.Fatalf("Run = %v", err)
		}
		var sawChild bool
		for _, s := range seen {
			// 节点事件的回调只带 nodeID，所以认的是子图那个节点的 id（父图只有 mount 一步）
			if s == "run:step" || s == "fin:step" {
				sawChild = true
			}
		}
		if !sawChild {
			t.Fatalf("静态子图的节点事件没出现在父图的出口里（子执行没继承到有效出口）：%v", seen)
		}
	})
}

// 同一份子视图的诊断只构造一次；父层按声明序包装它，不再重新扫描整个子树。
func TestTopologyNestedProblemOrder(t *testing.T) {
	optional := NewKey[int]("nested.optional")
	layer := func(id string) *Graph {
		g := mustNew(t, context.Background(), id)
		declareIn(t, g, NewPort(optional))
		if err := g.DeclareOutput(NewPort(NewKey[int](id + ".out"))); err != nil {
			t.Fatal(err)
		}
		mustAdd(t, g, NewNode("hungry", "", Requires(NewKey[int](id+".missing")), nil, nil))
		if err := Produce(g, "producer", "", NewKey[<-chan int](id+".stream"),
			func(*RunCtx, func(int) error) error { return nil }); err != nil {
			t.Fatal(err)
		}
		return g
	}
	leaf, mid, root := layer("nested.leaf"), layer("nested.mid"), layer("nested.root")
	x, y := NewKey[int]("nested.x"), NewKey[int]("nested.y")
	mustAdd(t, leaf, NewNode("A", "", Requires(y), Provides(x), nil))
	mustAdd(t, leaf, NewNode("B", "", Requires(x), Provides(y), nil))
	if err := Sub(mid, "inner", "", []SubBind{Fix(optional, 1)}, leaf); err != nil {
		t.Fatal(err)
	}
	if err := Sub(root, "outer", "", []SubBind{Fix(optional, 1)}, mid); err != nil {
		t.Fatal(err)
	}

	local := func(id, at string) []string {
		return []string{
			fmt.Sprintf(`pulse: node "hungry" requires "%s.missing" but nothing provides it `+
				`(declare it with DeclareInput if the value comes from outside)`, id),
			fmt.Sprintf(`pulse: declared output "%s.out" is never produced: `+
				`nothing in this graph provides it, and it is not a declared input of the same name `+
				`(a pass-through), so a parent binding it with Out could only ever report `+
				`"never arrived" (provide it with a node, or drop the DeclareOutput)`, id),
			fmt.Sprintf(`pulse: node "%s" provides "%s.stream" but nothing consumes it: `+
				`a stream output nobody reads blocks the sender forever`, joinPath(at, "producer"), id),
		}
	}
	wrap := func(id, at string, problems []string) []string {
		out := make([]string, 0, len(problems))
		for _, problem := range problems {
			out = append(out, fmt.Sprintf(`pulse: subgraph "%s" (mounted at "%s"): %s`, id, at, problem))
		}
		return out
	}
	wantLeaf := local("nested.leaf", "outer/inner")
	wantLeaf = []string{wantLeaf[0], wantLeaf[1],
		`pulse: dependency cycle: A -> B -> A (A requires "nested.y", B requires "nested.x")`, wantLeaf[2]}
	wantMid := append(local("nested.mid", "outer"), wrap("nested.leaf", "outer/inner", wantLeaf)...)
	wantRoot := append(local("nested.root", ""), wrap("nested.mid", "outer", wantMid)...)
	top := root.Topology()
	for _, view := range []struct {
		top  *Topology
		want []string
	}{
		{top, wantRoot},
		{top.Nodes[2].Sub, wantMid},
		{top.Nodes[2].Sub.Nodes[2].Sub, wantLeaf},
	} {
		if len(view.top.Problems) != len(view.want) {
			t.Fatalf("%q Problems = %v; want %v", view.top.Graph.Path, view.top.Problems, view.want)
		}
		for i, problem := range view.top.Problems {
			if got := problem.Error(); got != view.want[i] {
				t.Fatalf("%q Problems[%d] = %q; want %q", view.top.Graph.Path, i, got, view.want[i])
			}
		}
	}
	if err := errors.Unwrap(errors.Unwrap(top.Problems[6])); err == nil || err.Error() != wantLeaf[0] {
		t.Fatalf("两层包装没有保留子错误：%v", err)
	}
}

func TestTopologyBindFailureKeepsChildView(t *testing.T) {
	optional, late := NewKey[int]("bind.optional"), NewKey[int]("bind.late")
	leaf := mustNew(t, context.Background(), "bind.leaf")
	declareIn(t, leaf, NewPort(optional))
	mustAdd(t, leaf, NewNode("hungry", "", Requires(NewKey[int]("bind.missing")), nil, nil))
	child := mustNew(t, context.Background(), "bind.child")
	declareIn(t, child, NewPort(optional))
	if err := Sub(child, "inner", "", []SubBind{Fix(optional, 1)}, leaf); err != nil {
		t.Fatal(err)
	}
	root := mustNew(t, context.Background(), "bind.root")
	if err := Sub(root, "mount", "", []SubBind{Fix(optional, 1)}, child); err != nil {
		t.Fatal(err)
	}
	// 挂载时绑定合法，开放子图之后新增了这一处尚未供给的必需入口。
	declareIn(t, child, NewPort(late))
	mustAdd(t, child, NewNode("late", "", Requires(late), nil, nil))
	top := root.Topology()
	want := `pulse: subgraph mounted at "mount": pulse: Sub "mount": child inlet bind.late:int ` +
		`has no value for this mount: node "late" inside the child requires it, and it is a declared ` +
		`input of graph "bind.child" - bind it with In, or give this mount site a value with Fix / FixSkip`
	if len(top.Problems) != 1 || top.Problems[0].Error() != want {
		t.Fatalf("父层只报绑定问题：%v; want %q", top.Problems, want)
	}
	childTop := top.Nodes[0].Sub
	wantChild := `pulse: subgraph "bind.leaf" (mounted at "mount/inner"): ` +
		`pulse: node "hungry" requires "bind.missing" but nothing provides it ` +
		`(declare it with DeclareInput if the value comes from outside)`
	if childTop == nil || len(childTop.Problems) != 1 || childTop.Problems[0].Error() != wantChild {
		t.Fatalf("绑定失败不能隐藏子视图：%+v", childTop)
	}
	if childTop.Nodes[0].Sub == nil || len(childTop.Nodes[0].Sub.Problems) != 1 {
		t.Fatal("绑定失败不能截断后代视图")
	}
}

func TestTopologySharedChildProblemsAreIsolatedBySite(t *testing.T) {
	in, value := NewKey[int]("shared.in"), NewKey[int]("shared.value")
	stream := NewKey[Stream[int]]("shared.stream")
	child := mustNew(t, context.Background(), "shared.child")
	declareIn(t, child, NewPort(in))
	if err := child.DeclareOutput(NewPort(in)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, child, NewNode("writer", "", Requires(in), Deps(Provides(stream), Provides(value)), nil))
	mustAdd(t, child, NewNode("reader", "", Requires(value), nil, nil))
	root := mustNew(t, context.Background(), "shared.root")
	if err := Sub(root, "live", "", []SubBind{Fix(in, 1), Out(in, NewKey[int]("shared.live"))}, child); err != nil {
		t.Fatal(err)
	}
	if err := Sub(root, "skipped", "", []SubBind{FixSkip(in), Out(in, NewKey[int]("shared.skipped"))}, child); err != nil {
		t.Fatal(err)
	}
	top, unchanged := root.Topology(), root.Topology()
	live, skipped := top.Nodes[0].Sub, top.Nodes[1].Sub
	want := `pulse: node "live/writer" provides "shared.stream" but nothing consumes it: ` +
		`a stream output nobody reads blocks the sender forever`
	if live.Graph.Path != "live" || skipped.Graph.Path != "skipped" ||
		len(live.Problems) != 1 || live.Problems[0].Error() != want || skipped.Problems != nil {
		t.Fatalf("同一定义的诊断必须按挂载点区分：live=%+v skipped=%+v", live, skipped)
	}
	if len(top.Problems) != 1 || top.Problems[0].Error() !=
		`pulse: subgraph "shared.child" (mounted at "live"): `+want {
		t.Fatalf("父层挂载诊断：%v", top.Problems)
	}
	live.Problems[0] = errors.New("changed")
	if top.Problems[0].Error() != unchanged.Problems[0].Error() {
		t.Fatal("子 Problems 切片与父层共享")
	}
	top.Problems[0] = errors.New("changed")
	top.Nodes[0].Binds[0].Child = "changed"
	top.Nodes[0].Provides[0].Name = "changed"
	live.Graph.ID, live.Graph.Path = "changed", "changed"
	live.Graph.InPorts[0].Name, live.Graph.OutPorts[0].Name = "changed", "changed"
	live.Order[0], live.Nodes[0].ID = "changed", "changed"
	live.Nodes[0].Requires[0].Name, live.Nodes[0].Provides[0].Name = "changed", "changed"
	live.Edges[0].Keys[0] = "changed"
	if !reflect.DeepEqual(skipped, unchanged.Nodes[1].Sub) {
		t.Fatal("修改一个挂载点的视图影响了兄弟挂载点")
	}
	if next := root.Topology(); !reflect.DeepEqual(next, unchanged) {
		t.Fatal("修改返回副本影响了后续视图")
	}
}

func topologyDeepGraph(tb testing.TB, depth int) *Graph {
	tb.Helper()
	optional := NewKey[int]("depth.optional")
	newLayer := func(id string) *Graph {
		g, err := New(id, "")
		if err != nil {
			tb.Fatal(err)
		}
		if err := g.DeclareInput(NewPort(optional)); err != nil {
			tb.Fatal(err)
		}
		return g
	}
	g := newLayer("depth.leaf")
	if err := g.Add(NewNode("leaf", "", nil, nil, nil)); err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < depth; i++ {
		parent := newLayer(fmt.Sprintf("depth.%d", i))
		if err := Sub(parent, "m", "", []SubBind{In(optional, optional)}, g); err != nil {
			tb.Fatal(err)
		}
		g = parent
	}
	if err := g.Validate(); err != nil {
		tb.Fatal(err)
	}
	return g
}

func TestTopologyDeepChain(t *testing.T) {
	const depth = 512
	top := topologyDeepGraph(t, depth).Topology()
	at := ""
	for i := 0; i <= depth; i++ {
		if top.Graph.Path != at || top.Problems != nil || len(top.Nodes) != 1 {
			t.Fatalf("第 %d 层形状或诊断不符：%+v", i, top)
		}
		if i == depth {
			if top.Nodes[0].ID != "leaf" || top.Nodes[0].Sub != nil {
				t.Fatalf("最内层不是叶子：%+v", top.Nodes)
			}
			break
		}
		if top.Nodes[0].ID != "m" || top.Nodes[0].Sub == nil {
			t.Fatalf("第 %d 层丢了子树", i)
		}
		at = joinPath(at, "m")
		top = top.Nodes[0].Sub
	}
}

func TestTopologyDeepChainAllocationGrowth(t *testing.T) {
	small, large := topologyDeepGraph(t, 32), topologyDeepGraph(t, 128)
	allocs := func(g *Graph) float64 {
		return testing.AllocsPerRun(1, func() { _ = g.Topology() })
	}
	smallAllocs, largeAllocs := allocs(small), allocs(large)
	// 深度增为 4 倍，允许 6 倍分配次数；重扫每层子树会超过此界。
	// 路径和祖先链随深度变长，因此不对总分配字节数作线性承诺。
	if largeAllocs > 6*smallAllocs {
		t.Fatalf("深度 32→128 分配次数 %.0f→%.0f，仍在重复检查子树", smallAllocs, largeAllocs)
	}
	t.Logf("深度 32→128 分配次数 %.0f→%.0f", smallAllocs, largeAllocs)
}

// TestTopologyReportsForgedSubgraphCycle 钉的是递归的**兜底判环**。
//
// 组合环在装配期就被 `Sub` 拒了，但递归展开碰上环的代价不是一句错话：单线程里是**爆栈**，
// 并发里是**两层互等对方的锁**（父锁着自己再要子图、子锁着自己再要父图）。所以沿挂载边
// 每往下走一层，都要自己数一遍「这张子图是不是已经在这条链上」——不能只靠装配期那一次检查。
//
// 这条兜底判据没有别的路进得去（`Sub` 那条路进不去环），所以这里包内直接把环焊上：
// 先让 c 正常挂 a（这一刻装配期检查合法），再给 a 手造一个挂着 c 的节点——于是
// a 的子树里有 c、c 的子树里有 a。
func TestTopologyReportsForgedSubgraphCycle(t *testing.T) {
	ctx := context.Background()
	ain := NewKey[string]("fc.a_in")
	aout := NewKey[string]("fc.a_out")
	cin := NewKey[string]("fc.c_in")
	cout := NewKey[string]("fc.c_out")

	// a 是一张正常子图：入口 → 出口
	a := mustNew(t, ctx, "fc.a")
	if err := a.DeclareInput(NewPort(ain)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := a.DeclareOutput(NewPort(aout)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	mustAdd(t, a, NewNode("ax", "", Requires(ain), Provides(aout), func(rc *RunCtx) error {
		return Set(rc, aout, "v")
	}))

	// c 正常挂 a（这一刻 a 的子树里没有 c，装配期检查当然过）
	c := mustNew(t, ctx, "fc.c")
	if err := c.DeclareInput(NewPort(cin)); err != nil {
		t.Fatalf("DeclareInput = %v", err)
	}
	if err := c.DeclareOutput(NewPort(cout)); err != nil {
		t.Fatalf("DeclareOutput = %v", err)
	}
	if err := Sub(c, "toA", "", []SubBind{In(cin, ain), Out(aout, cout)}, a); err != nil {
		t.Fatalf("Sub = %v", err)
	}

	// 把 c 焊回 a：包内直接造那个挂载节点，绕开 Sub 的装配期检查（环在装配期就拒，只有
	// 伪造才造得出来）。伪造的这一步也要**接线成完整形状**——`c` 需要 `cin`，不喂它的话
	// `checkBinds` 会先报「漏绑必需进口」，那一支就不再往下展开，环反而报不出来。
	back := NewNode("back", "", nil, nil, func(rc *RunCtx) error { return nil })
	back.child = c
	back.binds = []SubBind{In(cin, cin)}
	mustAdd(t, a, back)

	// 递归展开要**报环**，不是爆栈
	top := a.Topology()
	var sawCycle bool
	for _, p := range top.Problems {
		if strings.Contains(p.Error(), "subgraph cycle") {
			sawCycle = true
		}
	}
	if !sawCycle {
		t.Fatalf("环要报进 Problems（它的代价是爆栈 / 互等锁，不能就这么走下去）：%v", top.Problems)
	}
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "subgraph cycle") {
		t.Fatalf("Validate 该报同一句，got %v", err)
	}
	// 环那一处**就地收住**：这一层的其它形状照旧（a 的两个节点都在，挂到 c 的那一步照样展开），
	// 但不往回绕——绕回去就是无限递归。
	if top.Nodes[1].ID != "back" || top.Nodes[1].Sub == nil {
		t.Fatalf("非环的那一层该照常展开：%+v", top.Nodes)
	}
	if got := top.Nodes[1].Sub; len(got.Nodes) != 1 || got.Nodes[0].Sub != nil {
		t.Fatalf("环上的边该就地收住（不再往下展开），got %+v", got.Nodes)
	}
	// 环从链上任何一张图看都是环：c 自己那份视图同样报得出来（而不是走到一半就卡住）
	var fromC bool
	for _, p := range c.Topology().Problems {
		if strings.Contains(p.Error(), "subgraph cycle") {
			fromC = true
		}
	}
	if !fromC {
		t.Fatalf("从环上另一张图看也该报环：%v", c.Topology().Problems)
	}
}
