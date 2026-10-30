package pulse

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMermaidStaticGolden(t *testing.T) {
	in := NewKey[string]("input")
	unused := NewKey[string]("unused")
	first := NewKey[string]("first")
	second := NewKey[string]("second")
	g, err := New("checkout", "结算")
	if err != nil {
		t.Fatal(err)
	}
	declareIn(t, g, NewPort(in), NewPort(unused))
	if err := g.DeclareOutput(NewPort(second)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("build", "建单", Requires(in), Provides(first, second), func(rc *RunCtx) error {
		if err := Set(rc, first, "FIRST_VALUE_MUST_NOT_APPEAR"); err != nil {
			return err
		}
		return Set(rc, second, "SECOND_VALUE_MUST_NOT_APPEAR")
	}))
	mustAdd(t, g, NewNode("audit", "", Requires(second, first), nil, func(rc *RunCtx) error { return nil }))
	want := "flowchart TD\n" +
		"  subgraph m0[\"checkout · 结算\"]\n" +
		"    m1([\"in · input#58; string\"])\n" +
		"    m2([\"in · unused#58; string\"])\n" +
		"    m3([\"out · second#58; string\"])\n" +
		"    m4[\"build · 建单\"]\n" +
		"    m5[\"audit\"]\n" +
		"  end\n" +
		"  m1 -->|\"input\"| m4\n" +
		"  m4 -->|\"second\"| m5\n" +
		"  m4 -->|\"first\"| m5\n" +
		"  m4 -->|\"second\"| m3\n"
	if got := g.Topology().Mermaid(); got != want {
		t.Fatalf("Mermaid =\n%s\nwant\n%s", got, want)
	}
	var inputs Inputs
	if err := Seed(&inputs, in, "INPUT_VALUE_MUST_NOT_APPEAR"); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(context.Background(), inputs); err != nil {
		t.Fatal(err)
	}
	if got := g.Topology().Mermaid(nil, WithMermaidSnapshot(nil)); got != want {
		t.Fatalf("纯静态输出不应受运行或 nil 选项影响：\n%s", got)
	}
}

func TestMermaidEmpty(t *testing.T) {
	var absent *Topology
	if got := absent.Mermaid(WithMermaidSnapshot(&Snapshot{})); got != "flowchart TD\n" {
		t.Fatalf("nil Topology = %q", got)
	}
	if got := (&Topology{}).Mermaid(); got != "flowchart TD\n  subgraph m0[\"graph\"]\n  end\n" {
		t.Fatalf("零视图 = %q", got)
	}
}

func TestMermaidNestedPortsAndBinds(t *testing.T) {
	pass := NewKey[string]("pass")
	unused := NewKey[string]("unused")
	fixed := NewKey[string]("fixed")
	fixedSkip := NewKey[string]("fixedSkip")
	answer := NewKey[string]("answer")
	leaf := mustNew(t, context.Background(), "leaf")
	declareIn(t, leaf, NewPort(pass), NewPort(unused), NewPort(fixed), NewPort(fixedSkip))
	if err := leaf.DeclareOutput(NewPort(answer), NewPort(pass), NewPort(unused)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, leaf, NewNode("work", "", Requires(fixed), Provides(answer), func(rc *RunCtx) error {
		return Set(rc, answer, "ANSWER_MUST_NOT_APPEAR")
	}))
	midIn := NewKey[string]("source")
	midOut := NewKey[string]("result")
	midPass := NewKey[string]("passthrough")
	mid := mustNew(t, context.Background(), "mid")
	declareIn(t, mid, NewPort(midIn))
	if err := mid.DeclareOutput(NewPort(midPass), NewPort(midOut)); err != nil {
		t.Fatal(err)
	}
	if err := Sub(mid, "inner", "", []SubBind{
		Out(answer, midOut), In(midIn, pass), Fix(fixed, "FIXED_SECRET_MUST_NOT_APPEAR"),
		Out(pass, midPass), FixSkip(fixedSkip),
	}, leaf); err != nil {
		t.Fatal(err)
	}
	rootIn := NewKey[string]("incoming")
	root := mustNew(t, context.Background(), "root")
	declareIn(t, root, NewPort(rootIn))
	for _, side := range []string{"left", "right"} {
		out := NewKey[string](side + ".out")
		through := NewKey[string](side + ".through")
		if err := Sub(root, side, "", []SubBind{In(rootIn, midIn), Out(midOut, out), Out(midPass, through)}, mid); err != nil {
			t.Fatal(err)
		}
	}
	top := root.Topology()
	got := top.Mermaid()
	doc := parseMermaid(t, got)
	if len(doc.graphs) != 5 {
		t.Fatalf("根 + 两处中间层 + 两处叶子 = 5 张图，got %v", doc.graphs)
	}
	for _, side := range []string{"left", "right"} {
		midScope, leafScope := "mid @ "+side, "leaf @ "+side+"/inner"
		rootPort := doc.vertex(t, "root", "in · incoming: string")
		mountIn := doc.vertex(t, "root", side+" · in · incoming: string")
		midPort := doc.vertex(t, midScope, "in · source: string")
		innerIn := doc.vertex(t, midScope, "inner · in · source: string")
		leafIn := doc.vertex(t, leafScope, "in · pass: string")
		leafOut := doc.vertex(t, leafScope, "out · pass: string")
		innerOut := doc.vertex(t, midScope, "inner · out · passthrough: string")
		doc.edge(t, rootPort, mountIn, "incoming")
		doc.edge(t, mountIn, midPort, "incoming → source")
		doc.edge(t, midPort, innerIn, "source")
		doc.edge(t, innerIn, leafIn, "source → pass")
		doc.edge(t, leafIn, leafOut, "pass")
		doc.edge(t, leafOut, innerOut, "pass → passthrough")
		_ = doc.vertex(t, leafScope, "in · unused: string")
		_ = doc.vertex(t, leafScope, "out · unused: string")
		for _, name := range []string{"fixed", "fixedSkip"} {
			port := doc.vertex(t, leafScope, "in · "+name+": string")
			found := false
			for _, edge := range doc.edges {
				if edge.to == port && edge.label == "Fix / FixSkip" && doc.vertices[edge.from].label == "fixed" {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s 的 %s 没有固定绑定", side, name)
			}
		}
		// Bind 输出次序必须沿装配序，不能把入口/出口/固定输入各自重新分组。
		var binds []string
		for _, e := range doc.edges {
			if (doc.vertices[e.from].scope == midScope && doc.vertices[e.to].scope == leafScope) ||
				(doc.vertices[e.from].scope == leafScope && doc.vertices[e.to].scope == midScope) {
				binds = append(binds, e.label)
			}
		}
		if strings.Join(binds, "|") != "answer → result|source → pass|Fix / FixSkip|pass → passthrough|Fix / FixSkip" {
			t.Fatalf("%s 的接线声明序 = %v", side, binds)
		}
	}
	if strings.Contains(got, "MUST_NOT_APPEAR") || strings.Contains(got, "classDef") {
		t.Fatalf("静态图泄露值或猜测状态：\n%s", got)
	}
	for i := 0; i < 16; i++ {
		if next := root.Topology().Mermaid(); next != got {
			t.Fatal("同一份定义应逐字节稳定")
		}
	}
}

func TestMermaidSharedParentKeepsAllBindingEndpoints(t *testing.T) {
	input, a, b := NewKey[string]("incoming"), NewKey[string]("a"), NewKey[string]("b")
	child := mustNew(t, t.Context(), "shared")
	declareIn(t, child, NewPort(a), NewPort(b))
	if err := child.DeclareOutput(NewPort(a), NewPort(b)); err != nil {
		t.Fatal(err)
	}
	root := mustNew(t, t.Context(), "root")
	declareIn(t, root, NewPort(input))
	for _, side := range []string{"left", "right"} {
		outA, outB := NewKey[string](side+".a"), NewKey[string](side+".b")
		if err := Sub(root, side, "", []SubBind{In(input, a), Out(a, outA), In(input, b), Out(b, outB)}, child); err != nil {
			t.Fatal(err)
		}
		if err := root.DeclareOutput(NewPort(outA), NewPort(outB)); err != nil {
			t.Fatal(err)
		}
	}
	top := root.Topology()
	got := top.Mermaid()
	doc := parseMermaid(t, got)
	rootIn := doc.vertex(t, "root", "in · incoming: string")
	for _, side := range []string{"left", "right"} {
		var inputEndpoints []string
		for _, edge := range doc.edges {
			if edge.from == rootIn && edge.label == input.Name() && doc.vertices[edge.to].label == side+" · in · incoming: string" {
				inputEndpoints = append(inputEndpoints, edge.to)
			}
		}
		if len(inputEndpoints) != 2 || inputEndpoints[0] == inputEndpoints[1] {
			t.Fatalf("%s lost distinct child inlets: %v", side, inputEndpoints)
		}
		for i, name := range []string{"a", "b"} {
			childIn := doc.vertex(t, "shared @ "+side, "in · "+name+": string")
			childOut := doc.vertex(t, "shared @ "+side, "out · "+name+": string")
			mountOut := doc.vertex(t, "root", side+" · out · "+side+"."+name+": string")
			rootOut := doc.vertex(t, "root", "out · "+side+"."+name+": string")
			doc.edge(t, inputEndpoints[i], childIn, "incoming → "+name)
			doc.edge(t, childIn, childOut, name)
			doc.edge(t, childOut, mountOut, name+" → "+side+"."+name)
			doc.edge(t, mountOut, rootOut, side+"."+name)
		}
	}
	if next := top.Mermaid(); next != got {
		t.Fatal("binding indexes changed deterministic output")
	}
}

func TestMermaidIDsDoNotCollideAcrossDomains(t *testing.T) {
	leafIn := NewKey[string]("m0")
	leafOut := NewKey[string]("wire")
	leaf := mustNew(t, context.Background(), "m0")
	declareIn(t, leaf, NewPort(leafIn))
	if err := leaf.DeclareOutput(NewPort(leafOut)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, leaf, NewNode("m0", "", Requires(leafIn), Provides(leafOut), func(rc *RunCtx) error { return nil }))
	root := mustNew(t, context.Background(), "m0")
	declareIn(t, root, NewPort(leafIn))
	if err := Sub(root, "m0", "", []SubBind{In(leafIn, leafIn), Out(leafOut, leafOut)}, leaf); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"m1", "input", "end", "g0", "n0", "a-b", "a_b", "a b", "1"} {
		declareIn(t, root, NewPort(NewKey[string](name)))
		mustAdd(t, root, NewNode(name, "", Requires(leafOut), nil, func(rc *RunCtx) error { return nil }))
	}
	doc := parseMermaid(t, root.Topology().Mermaid()) // 解析器会检查所有图/节点/端口 ID 全域唯一。
	rootNode := doc.vertex(t, "m0 @ m0", "m0")
	rootPort := doc.vertex(t, "m0", "in · m0: string")
	childPort := doc.vertex(t, "m0 @ m0", "in · m0: string")
	if rootNode == rootPort || childPort == rootPort || childPort == rootNode {
		t.Fatal("跨图/节点/端口标识碰撞")
	}
	for _, name := range []string{"a-b", "a_b", "a b", "end"} {
		_ = doc.vertex(t, "m0", name)
	}
}

func TestMermaidUntrustedLabels(t *testing.T) {
	payload := "end\"\r\n%%{init: {'securityLevel':'loose'}}%%\n<script>alert(1)</script>&#34;[#quot;](x)|\\n\x00\t"
	key := KeyShape{Name: payload, Type: payload, From: Source{Kind: SourceInput}}
	child := &Topology{Graph: GraphShape{ID: payload, Description: payload, Path: payload, InPorts: []KeyShape{key}}}
	top := &Topology{
		Graph: GraphShape{ID: payload, Description: payload, Path: payload,
			InPorts: []KeyShape{key}, OutPorts: []KeyShape{{Name: "out" + payload, Type: payload}}},
		Nodes: []NodeShape{
			{ID: payload, Description: payload, Requires: []KeyShape{key}, Provides: []KeyShape{{Name: "out" + payload}}},
			{ID: "sink", Description: payload},
			{ID: "sub" + payload, Description: payload, Requires: []KeyShape{key}, Sub: child,
				Binds: []BindShape{{Parent: payload, Child: payload, In: true}}},
		},
		Edges: []EdgeShape{{From: payload, To: "sink", Keys: []string{"out" + payload}}},
	}
	got := top.Mermaid()
	doc := parseMermaid(t, got)
	if len(doc.graphs) != 2 || len(doc.vertices) != 7 || len(doc.edges) != 5 {
		t.Fatalf("用户内容改变了语句结构：graphs=%d vertices=%d edges=%d\n%s", len(doc.graphs), len(doc.vertices), len(doc.edges), got)
	}
	labelSource := strings.ReplaceAll(got, "-->", "")
	for _, unsafe := range []string{"%%", "<", ">", "&", "\r", "\x00", "\t", "`", "\\n"} {
		if strings.Contains(labelSource, unsafe) {
			t.Fatalf("输出含未编码的 %q：\n%s", unsafe, got)
		}
	}
	for _, entity := range []string{"#34;", "#35;", "#37;", "#60;", "#62;", "#38;", "#91;", "#40;", "#124;", "#92;u0000"} {
		if !strings.Contains(got, entity) {
			t.Fatalf("输出缺少安全编码 %s", entity)
		}
	}
	display := strings.NewReplacer("\r", "\\r", "\n", "\\n", "\t", "\\t", "\x00", "\\u0000").Replace(payload)
	found := false
	for _, v := range doc.vertices {
		if v.label == display+" · "+display {
			found = true
		}
	}
	if !found {
		t.Fatal("特殊标识与描述应作为文字保留，控制字符显示为转义文字")
	}
}

func TestMermaidSnapshotMatching(t *testing.T) {
	statuses := []NodeStatus{NodeStatusPending, NodeStatusWaiting, NodeStatusRunning, NodeStatusCompleted,
		NodeStatusSkipped, NodeStatusFailed, NodeStatusCanceled, NodeStatus("future")}
	top := &Topology{Graph: GraphShape{ID: "shape", Path: "mount"}}
	s := &Snapshot{Graph: GraphState{ID: "shape", Path: "mount", Description: "SNAPSHOT_MUST_NOT_APPEAR", Err: errors.New("ERR_MUST_NOT_APPEAR")}}
	for _, status := range statuses {
		top.Nodes = append(top.Nodes, NodeShape{ID: string(status), Description: "declared"})
		s.Nodes = append(s.Nodes, NodeState{ID: string(status), Status: status, Description: "SNAPSHOT_MUST_NOT_APPEAR",
			Inputs: []KeyState{{Name: "INPUT_MUST_NOT_APPEAR"}}, Outputs: []KeyState{{Name: "OUTPUT_MUST_NOT_APPEAR"}},
			Err: errors.New("ERR_MUST_NOT_APPEAR")})
	}
	s.Nodes = append(s.Nodes, NodeState{ID: "GHOST_MUST_NOT_APPEAR", Status: NodeStatusCompleted})
	got := top.Mermaid(WithMermaidSnapshot(s))
	doc := parseMermaid(t, got)
	if len(doc.classes) != 7 || strings.Count(got, "classDef ") != 7 || strings.Contains(got, "MUST_NOT_APPEAR") {
		t.Fatalf("只允许实际匹配的已知状态，got\n%s", got)
	}
	for _, status := range statuses[:7] {
		id := doc.vertex(t, "shape @ mount", string(status)+" · declared · "+string(status))
		if doc.classes[id] != string(status) {
			t.Fatalf("%s 未着色：%v", status, doc.classes)
		}
	}
	id := doc.vertex(t, "shape @ mount", "future · declared")
	if _, ok := doc.classes[id]; ok {
		t.Fatal("未知状态不应着色")
	}
	static := top.Mermaid()
	for _, graph := range []GraphState{{ID: "other", Path: "mount"}, {ID: "shape", Path: "other"}} {
		wrong := *s
		wrong.Graph = graph
		if got := top.Mermaid(WithMermaidSnapshot(&wrong)); got != static {
			t.Fatal("图 ID 或挂载路径不符时不应误染")
		}
	}
	unknown := &Snapshot{Graph: s.Graph, Nodes: []NodeState{{ID: "future", Status: NodeStatus("future")}, {ID: "ghost", Status: NodeStatusFailed}}}
	if got := top.Mermaid(WithMermaidSnapshot(unknown)); got != static {
		t.Fatal("只有未知状态或不存在的节点时，不应生成任何样式")
	}
}

func TestMermaidRealExecutionChildStates(t *testing.T) {
	in, out := NewKey[string]("child.in"), NewKey[string]("child.out")
	leaf := mustNew(t, context.Background(), "leaf")
	declareIn(t, leaf, NewPort(in))
	if err := leaf.DeclareOutput(NewPort(out)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, leaf, NewNode("work", "", Requires(in), Provides(out), func(rc *RunCtx) error {
		v, err := Get(rc, in)
		if err != nil {
			return err
		}
		if v == "skip" {
			return NoValue()
		}
		return Set(rc, out, "OUTPUT_SECRET_MUST_NOT_APPEAR")
	}))
	leftIn, rightIn := NewKey[string]("left.in"), NewKey[string]("right.in")
	root := mustNew(t, context.Background(), "root")
	declareIn(t, root, NewPort(leftIn), NewPort(rightIn))
	for i, name := range []string{"left", "right"} {
		parentIn := []Key[string]{leftIn, rightIn}[i]
		if err := Sub(root, name, "", []SubBind{In(parentIn, in), Out(out, NewKey[string](name+".out"))}, leaf); err != nil {
			t.Fatal(err)
		}
	}
	top := root.Topology()
	static := top.Mermaid()
	var inputs Inputs
	if err := Seed(&inputs, leftIn, "INPUT_SECRET_MUST_NOT_APPEAR"); err != nil {
		t.Fatal(err)
	}
	if err := Seed(&inputs, rightIn, "skip"); err != nil {
		t.Fatal(err)
	}
	e := mermaidRun(t, root, inputs)
	s := e.Snapshot()
	doc := parseMermaid(t, top.Mermaid(WithMermaidSnapshot(s)))
	for _, side := range []string{"left", "right"} {
		status := "completed"
		if side == "right" {
			status = "skipped"
		}
		for _, name := range []string{"Sub · " + side, "work"} {
			id := doc.vertex(t, "leaf @ "+side, name+" · "+status)
			if doc.classes[id] != status {
				t.Fatalf("%s/%s 状态误染：%v", side, name, doc.classes)
			}
		}
	}
	if got := root.Topology().Mermaid(); got != static {
		t.Fatal("状态叠加不应修改静态视图或定义")
	}
	// 换错两处 Child 活句柄：节点同名、图 ID 相同，仍必须按 Path 拒绝。
	wrong := *s
	wrong.Nodes = append([]NodeState(nil), s.Nodes...)
	wrong.Nodes[0].Child, wrong.Nodes[1].Child = wrong.Nodes[1].Child, wrong.Nodes[0].Child
	misplaced := parseMermaid(t, top.Mermaid(WithMermaidSnapshot(&wrong)))
	if len(misplaced.classes) != 2 {
		t.Fatalf("错挂的子执行不应给内部 work 着色：%v", misplaced.classes)
	}
	// 另一轮的全部入口跳过：仍创建 child，各内部节点按自己的输入门跳过。
	var skipped Inputs
	if err := Seed(&skipped, leftIn, "go"); err != nil {
		t.Fatal(err)
	}
	if err := SkipSeed(&skipped, rightIn); err != nil {
		t.Fatal(err)
	}
	s2 := mermaidRun(t, root, skipped).Snapshot()
	if s2.Nodes[1].Child == nil {
		t.Fatal("跳过入口不应阻止创建子执行")
	}
	doc2 := parseMermaid(t, top.Mermaid(WithMermaidSnapshot(s2)))
	work := doc2.vertex(t, "leaf @ right", "work · skipped")
	if doc2.classes[work] != "skipped" || len(doc2.graphs) != 3 {
		t.Fatal("子节点应保留真实跳过状态和完整静态子树")
	}
}

func TestMermaidRealFailedChild(t *testing.T) {
	in, out := NewKey[string]("in"), NewKey[string]("out")
	failure := errors.New("FAILURE_SECRET_MUST_NOT_APPEAR")
	leaf := mustNew(t, context.Background(), "failed-child")
	declareIn(t, leaf, NewPort(in))
	if err := leaf.DeclareOutput(NewPort(out)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, leaf, NewNode("work", "", Requires(in), Provides(out), func(rc *RunCtx) error { return failure }))
	root := mustNew(t, context.Background(), "root")
	if err := Sub(root, "mount", "", []SubBind{Fix(in, "FIXED_SECRET_MUST_NOT_APPEAR"), Out(out, NewKey[string]("result"))}, leaf); err != nil {
		t.Fatal(err)
	}
	e, err := root.NewExecution(context.Background(), Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); !errors.Is(err, failure) {
		t.Fatalf("Wait = %v", err)
	}
	got := root.Topology().Mermaid(WithMermaidSnapshot(e.Snapshot()))
	doc := parseMermaid(t, got)
	if len(doc.classes) != 2 || strings.Contains(got, "MUST_NOT_APPEAR") {
		t.Fatalf("失败父节点和失败子节点都应可见而不泄露载荷：\n%s", got)
	}
	for _, name := range []string{"Sub · mount", "work"} {
		id := doc.vertex(t, "failed-child @ mount", name+" · failed")
		if doc.classes[id] != "failed" {
			t.Fatal("失败子树未着色")
		}
	}
	// 同一路径、同节点 ID 的其他子图也必须按图 ID 排除。
	foreign := mustNew(t, context.Background(), "foreign")
	declareIn(t, foreign, NewPort(in))
	if err := foreign.DeclareOutput(NewPort(out)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, foreign, NewNode("work", "", Requires(in), Provides(out), func(rc *RunCtx) error { return nil }))
	otherRoot := mustNew(t, context.Background(), "other-root")
	if err := Sub(otherRoot, "mount", "", []SubBind{Fix(in, "go"), Out(out, NewKey[string]("other.result"))}, foreign); err != nil {
		t.Fatal(err)
	}
	other := mermaidRun(t, otherRoot, Inputs{}).Snapshot()
	s := e.Snapshot()
	s.Nodes[0].Child = other.Nodes[0].Child
	wrong := parseMermaid(t, root.Topology().Mermaid(WithMermaidSnapshot(s)))
	if len(wrong.classes) != 1 {
		t.Fatalf("同路径的其他定义误染了 work：%v", wrong.classes)
	}
}

func TestMermaidRunningChildSnapshot(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	in, out := NewKey[string]("in"), NewKey[string]("out")
	leaf := mustNew(t, context.Background(), "leaf")
	declareIn(t, leaf, NewPort(in))
	if err := leaf.DeclareOutput(NewPort(out)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, leaf, NewNode("work", "", Requires(in), Provides(out), func(rc *RunCtx) error {
		close(started)
		select {
		case <-release:
			return Set(rc, out, "VALUE_MUST_NOT_APPEAR")
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	}))
	root := mustNew(t, context.Background(), "root")
	if err := Sub(root, "mount", "", []SubBind{Fix(in, "INPUT_MUST_NOT_APPEAR"), Out(out, NewKey[string]("result"))}, leaf); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e, err := root.NewExecution(ctx, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer e.Wait()
	defer close(release)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("子节点未进入运行体")
	}
	top := root.Topology()
	for i := 0; i < 8; i++ {
		got := top.Mermaid(WithMermaidSnapshot(e.Snapshot()))
		doc := parseMermaid(t, got)
		if len(doc.classes) != 2 || strings.Contains(got, "MUST_NOT_APPEAR") {
			t.Fatalf("运行中的挂载和子节点应各有状态：\n%s", got)
		}
		for _, status := range doc.classes {
			if status != "running" {
				t.Fatalf("阻塞中的状态 = %s", status)
			}
		}
	}
}

func mermaidRun(t *testing.T, g *Graph, in Inputs) *Execution {
	t.Helper()
	e, err := g.NewExecution(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
	return e
}

// 从公开图文本检查语句与连线；不调用渲染器内部的 ID/编码/状态函数。
type mermaidTestVertex struct {
	label, scope string
}

type mermaidTestEdge struct {
	from, to, label string
}

type mermaidTestDoc struct {
	graphs   map[string]string
	vertices map[string]mermaidTestVertex
	edges    []mermaidTestEdge
	classes  map[string]string
}

func parseMermaid(t *testing.T, source string) mermaidTestDoc {
	t.Helper()
	graph := regexp.MustCompile(`^subgraph (m[0-9]+)\["([^"]*)"\]$`)
	vertex := regexp.MustCompile(`^(m[0-9]+)(?:\(\[|\[|\{)"([^"]*)"(?:\]\)|\]|\})$`)
	edge := regexp.MustCompile(`^(m[0-9]+) -->\|"([^"]*)"\| (m[0-9]+)$`)
	class := regexp.MustCompile(`^class (m[0-9]+) pulse_([a-z]+)$`)
	entity := regexp.MustCompile(`#([0-9]+);`)
	decode := func(s string) string {
		return entity.ReplaceAllStringFunc(s, func(e string) string {
			n, err := strconv.Atoi(e[1 : len(e)-1])
			if err != nil {
				t.Fatal(err)
			}
			return string(rune(n))
		})
	}
	doc := mermaidTestDoc{graphs: make(map[string]string), vertices: make(map[string]mermaidTestVertex), classes: make(map[string]string)}
	seen := make(map[string]bool)
	claim := func(id string) {
		if seen[id] {
			t.Fatalf("重复的内部 ID %s", id)
		}
		seen[id] = true
	}
	var scopes []string
	for i, line := range strings.Split(strings.TrimSuffix(source, "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case i == 0:
			if line != "flowchart TD" {
				t.Fatalf("非法头部 %q", line)
			}
		case graph.MatchString(line):
			m := graph.FindStringSubmatch(line)
			claim(m[1])
			doc.graphs[m[1]] = decode(m[2])
			scopes = append(scopes, doc.graphs[m[1]])
		case line == "end":
			if len(scopes) == 0 {
				t.Fatal("意外的子图结束语句")
			}
			scopes = scopes[:len(scopes)-1]
		case vertex.MatchString(line):
			m := vertex.FindStringSubmatch(line)
			claim(m[1])
			if len(scopes) == 0 {
				t.Fatal("节点不在静态图内")
			}
			doc.vertices[m[1]] = mermaidTestVertex{decode(m[2]), scopes[len(scopes)-1]}
		case edge.MatchString(line):
			m := edge.FindStringSubmatch(line)
			doc.edges = append(doc.edges, mermaidTestEdge{m[1], m[3], decode(m[2])})
		case class.MatchString(line):
			m := class.FindStringSubmatch(line)
			doc.classes[m[1]] = m[2]
		case strings.HasPrefix(line, "classDef pulse_"):
		default:
			t.Fatalf("非预期 Mermaid 语句 %q", line)
		}
	}
	if len(scopes) != 0 {
		t.Fatal("未闭合的子图")
	}
	for _, e := range doc.edges {
		if _, ok := doc.vertices[e.from]; !ok {
			t.Fatalf("边起点 %s 未声明", e.from)
		}
		if _, ok := doc.vertices[e.to]; !ok {
			t.Fatalf("边终点 %s 未声明", e.to)
		}
	}
	return doc
}

func (d mermaidTestDoc) vertex(t *testing.T, scope, label string) string {
	t.Helper()
	var id string
	for candidate, v := range d.vertices {
		if v.scope == scope && v.label == label {
			if id != "" {
				t.Fatalf("%q 中 label %q 对应多个端点", scope, label)
			}
			id = candidate
		}
	}
	if id == "" {
		t.Fatalf("%q 中缺少 %q；vertices = %v", scope, label, d.vertices)
	}
	return id
}

func (d mermaidTestDoc) edge(t *testing.T, from, to, label string) {
	t.Helper()
	for _, e := range d.edges {
		if e.from == from && e.to == to && e.label == label {
			return
		}
	}
	t.Fatalf("缺少连线 %s -- %q --> %s", from, label, to)
}
