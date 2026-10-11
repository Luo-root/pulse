package yaml_test

import (
	"errors"
	"testing"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

// YAML 那条路也看得见子图（#303 决策 I / #309 阶段二）：根图快照里那一步带着
// `Child`，它的 path 与图 id 就是 YAML 装配给的那套（节点 id / 父图 id + 节点 id），
// 子图自己的节点在子图快照里。手装那条路在 `pulse` 包的 `child_test.go`。
func TestLoadSubgraphChildHandle(t *testing.T) {
	var got string
	reg := subReg(t, &got)

	g, plan, err := pulseyaml.Load([]byte(subDoc), reg, pulseyaml.LoadOptions{GraphID: "P"})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if got != "about slot contract" {
		t.Fatalf("子图产物没桥回父图：%q", got)
	}

	root := g.Snapshot()
	if root.Graph.Path != "" {
		t.Errorf("根图 path = %q，期望空串", root.Graph.Path)
	}
	var step *pulse.NodeState
	for i := range root.Nodes {
		if root.Nodes[i].ID == "step1" {
			step = &root.Nodes[i]
		}
	}
	if step == nil {
		t.Fatal("根图快照里没有 step1")
	}
	if step.Status != pulse.NodeStatusCompleted {
		t.Fatalf("那一步状态 %q，期望 completed", step.Status)
	}
	if step.Child == nil {
		t.Fatal("YAML 装配的那一步也该留着子图句柄")
	}
	if got := step.Child.ID(); got != "P/step1" {
		t.Errorf("子图图 id = %q，期望 P/step1（父图 id + 节点 id）", got)
	}
	childSnap := step.Child.Snapshot()
	if childSnap.Graph.Path != "step1" {
		t.Errorf("子图 path = %q，期望 step1", childSnap.Graph.Path)
	}
	if childSnap.Graph.Status != pulse.GraphCompleted {
		t.Errorf("子图状态 %q，期望 completed", childSnap.Graph.Status)
	}
	if len(childSnap.Nodes) != 1 || childSnap.Nodes[0].ID != "work" {
		t.Fatalf("子图快照里应当是那个 work 节点，实际 %+v", childSnap.Nodes)
	}
	if childSnap.Nodes[0].Status != pulse.NodeStatusCompleted {
		t.Errorf("子图里 work 状态 %q，期望 completed", childSnap.Nodes[0].Status)
	}

	// 认领即封条：YAML 那条路交出来的句柄同样是只读的。
	if err := step.Child.Add(pulse.NewNode("late", "", nil, nil,
		func(rc *pulse.RunCtx) error { return nil })); !errors.Is(err, pulse.ErrGraphStarted) {
		t.Errorf("认领之后 Add = %v，期望 ErrGraphStarted", err)
	}
	if err := step.Child.Start(); !errors.Is(err, pulse.ErrGraphStarted) {
		t.Errorf("认领之后 Start = %v，期望 ErrGraphStarted", err)
	}
}
