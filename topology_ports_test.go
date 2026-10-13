package pulse

import (
	"context"
	"reflect"
	"testing"
)

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
