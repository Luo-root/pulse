package pulse

import (
	"fmt"
	"testing"
)

func streamSkipWorkMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func streamSkipWorkGraph(t *testing.T, id string) *Graph {
	t.Helper()
	g, err := New(id, "")
	streamSkipWorkMust(t, err)
	return g
}

func streamSkipWorkTree(g *Graph) *streamTree {
	g.mu.Lock()
	defer g.mu.Unlock()
	tree := &streamTree{}
	pool := &streamPool{}
	tree.collectLocked(g, "", mountChain{g}, pool, nil, len(g.aspects) != 0)
	return tree
}

func streamSkipWorkBound(t *testing.T, tree *streamTree, work streamSkipWork) {
	t.Helper()
	requires, provides, binds, children := 0, 0, 0, 0
	for _, n := range tree.nodes {
		requires += len(n.def.requires)
		provides += len(n.def.provides)
		if n.child != nil {
			binds += len(n.def.binds)
			children += len(n.child.nodes)
		}
	}
	// 同时要求已统计建索引的访问，避免「漏计工作」让退化悄悄通过。
	if work.keys < len(tree.keys) || work.keys > 2*len(tree.keys) ||
		work.nodes < len(tree.nodes) || work.nodes > 2*len(tree.nodes) ||
		work.requires < requires || work.requires > 2*requires ||
		work.binds < binds || work.binds > 2*binds ||
		work.provides > provides || work.mounts > children {
		t.Fatalf("skip work is not bounded by declarations: work=%+v, keys=%d nodes=%d requires=%d provides=%d binds=%d children=%d",
			work, len(tree.keys), len(tree.nodes), requires, provides, binds, children)
	}
}

func TestStreamSkipWorkDeclarationOrderLinear(t *testing.T) {
	for _, size := range []int{1024, 4096} {
		var forward streamSkipWork
		haveForward := false
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("size=%d/reverse=%t", size, reverse), func(t *testing.T) {
				g := streamSkipWorkGraph(t, "chain")
				keys := make([]Key[bool], size+1)
				for i := range keys {
					keys[i] = NewKey[bool](fmt.Sprint("k", i))
				}
				src, out := NewKey[<-chan int]("src"), NewKey[<-chan int]("out")
				streamSkipWorkMust(t, g.DeclareInput(NewPort(keys[0])))
				terminal := func() {
					streamSkipWorkMust(t, g.Add(NewNode("channel", "", Requires(keys[size]), Provides(src), func(*RunCtx) error { return nil })))
					streamSkipWorkMust(t, Tee(g, "tee", src, Keys(out)))
					streamSkipWorkMust(t, Consume(g, "consume", out, func(*RunCtx, int) error { return nil }))
				}
				if reverse {
					terminal()
				}
				for j := 0; j < size; j++ {
					i := j
					if reverse {
						i = size - 1 - j
					}
					streamSkipWorkMust(t, g.Add(NewNode(fmt.Sprint("node", i), "", Requires(keys[i]), Provides(keys[i+1]), func(*RunCtx) error { return nil })))
				}
				if !reverse {
					terminal()
				}
				tree := streamSkipWorkTree(g)
				// 检查真实形状：每条链边都由前一个节点产出，倒序确实倒着声明。
				if len(tree.nodes) != size+3 || len(tree.keys) != size+3 {
					t.Fatalf("unexpected chain size: nodes=%d keys=%d", len(tree.nodes), len(tree.keys))
				}
				for i := 0; i < size; i++ {
					at := i
					if reverse {
						at = size + 2 - i
					}
					n := tree.nodes[at]
					if len(n.def.requires) != 1 || len(n.def.provides) != 1 ||
						n.def.requires[0].name != keys[i].Name() || n.def.provides[0].name != keys[i+1].Name() ||
						tree.sites[0].keys[keys[i+1].Name()].writer != n {
						t.Fatalf("chain link %d is not the declared dependency at %d", i, at)
					}
				}
				work := tree.propagateSkips(map[string]bool{keys[0].Name(): true})
				streamSkipWorkBound(t, tree, work)
				for _, n := range tree.nodes {
					if !n.bodySkipped {
						t.Fatalf("skip did not reach %q", n.at)
					}
				}
				for _, k := range tree.keys {
					if !k.skipped {
						t.Fatalf("skip did not reach key %q", k.ref.name)
					}
				}
				if work.inputs != 1 || work.provides != size+2 {
					t.Fatalf("source/output visits were not counted: %+v", work)
				}
				if !reverse {
					forward, haveForward = work, true
				} else if haveForward && work != forward {
					t.Fatalf("declaration order changed work: forward=%+v reverse=%+v", forward, work)
				}
				t.Logf("size=%d reverse=%t work=%+v", size, reverse, work)
			})
		}
	}
}

func TestStreamSkipWorkBindingsLinear(t *testing.T) {
	for _, size := range []int{128, 512} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			g, pass := streamSkipWorkGraph(t, "root"), streamSkipWorkGraph(t, "pass")
			port, warm := NewKey[bool]("port"), NewKey[bool]("warm")
			streamSkipWorkMust(t, pass.DeclareInput(NewPort(port), NewPort(warm)))
			streamSkipWorkMust(t, pass.DeclareOutput(NewPort(port)))
			streamSkipWorkMust(t, pass.Add(NewNode("warm", "", Requires(warm), nil, func(*RunCtx) error { return nil })))
			keys := make([]Key[bool], size+1)
			for i := range keys {
				keys[i] = NewKey[bool](fmt.Sprint("k", i))
			}
			streamSkipWorkMust(t, g.DeclareInput(NewPort(keys[0]), NewPort(warm)))
			// 第二个未知/ready 入口使每处 Sub body 保持可能执行，必须实际沿 In/Out
			// 推导 skip，不能用整步跳过绕过绑定链。挂载和节点均倒序声明。
			src, out := NewKey[<-chan int]("src"), NewKey[<-chan int]("out")
			streamSkipWorkMust(t, Consume(g, "consume", out, func(*RunCtx, int) error { return nil }))
			streamSkipWorkMust(t, Tee(g, "tee", src, Keys(out)))
			streamSkipWorkMust(t, g.Add(NewNode("channel", "", Requires(keys[size]), Provides(src), func(*RunCtx) error { return nil })))
			for i := size - 1; i >= 0; i-- {
				streamSkipWorkMust(t, Sub(g, fmt.Sprint("pass", i), "", []SubBind{In(keys[i], port), In(warm, warm), Out(port, keys[i+1])}, pass))
			}
			tree := streamSkipWorkTree(g)
			if len(tree.sites) != size+1 || len(tree.nodes) != 2*size+3 || len(tree.keys) != 3*size+4 {
				t.Fatalf("unexpected mounted chain: sites=%d nodes=%d keys=%d", len(tree.sites), len(tree.nodes), len(tree.keys))
			}
			work := tree.propagateSkips(map[string]bool{keys[0].Name(): true})
			streamSkipWorkBound(t, tree, work)
			if !tree.sites[0].keys[out.Name()].skipped || work.binds != 5*size || work.mounts != 0 {
				t.Fatalf("did not traverse the real In/Out chain once: %+v", work)
			}
			seen := make(map[*streamKey]bool)
			for _, n := range tree.sites[0].nodes {
				if n.child == nil {
					continue
				}
				k := n.child.keys[port.Name()]
				if n.bodySkipped || n.def.child != pass || len(n.def.requires) != 2 || len(n.def.binds) != 3 ||
					!k.skipped || seen[k] || n.child.keys[warm.Name()].skipped || n.child.nodes[0].bodySkipped {
					t.Fatalf("mount %q lost its live gate or isolated facts", n.at)
				}
				seen[k] = true
			}
			t.Logf("mounts=%d work=%+v", size, work)
		})
	}
}

func TestStreamSkipWorkAncestorSkipsEachBodyOnce(t *testing.T) {
	root, middle, leaf := streamSkipWorkGraph(t, "root"), streamSkipWorkGraph(t, "middle"), streamSkipWorkGraph(t, "leaf")
	trigger, fixed := NewKey[bool]("trigger"), NewKey[bool]("fixed")
	streamSkipWorkMust(t, root.DeclareInput(NewPort(trigger)))
	streamSkipWorkMust(t, middle.DeclareInput(NewPort(trigger)))
	streamSkipWorkMust(t, leaf.DeclareInput(NewPort(trigger), NewPort(fixed)))
	streamSkipWorkMust(t, leaf.Add(NewNode("gate", "", Requires(trigger), nil, func(*RunCtx) error { return nil })))
	for i := 0; i < 128; i++ {
		out := NewKey[bool](fmt.Sprint("out", i))
		streamSkipWorkMust(t, leaf.Add(NewNode(fmt.Sprint("node", i), "", Requires(fixed), Provides(out), func(*RunCtx) error { return nil })))
	}
	for i := 0; i < 16; i++ {
		streamSkipWorkMust(t, Sub(middle, fmt.Sprint("leaf", i), "", []SubBind{In(trigger, trigger), FixSkip(fixed)}, leaf))
	}
	streamSkipWorkMust(t, Sub(root, "middle", "", []SubBind{In(trigger, trigger)}, middle))
	tree := streamSkipWorkTree(root)
	work := tree.propagateSkips(map[string]bool{trigger.Name(): true})
	streamSkipWorkBound(t, tree, work)
	if work.mounts != len(tree.nodes)-1 || work.provides != 0 {
		t.Fatalf("ancestor skip did not visit each descendant once without inventing arrival: %+v", work)
	}
	for _, n := range tree.nodes {
		if !n.bodySkipped {
			t.Fatalf("ancestor skip did not reach %q", n.at)
		}
		for _, ref := range n.def.provides {
			if n.site.keys[ref.name].skipped {
				t.Fatalf("uncreated child published output %q", ref.name)
			}
		}
	}
	// 同一定义的下一次检查没有本次 root skip；FixSkip 仍逐挂载生效，
	// 但它不能反过来污染根入口或其它未知入口。
	next := streamSkipWorkTree(root)
	streamSkipWorkBound(t, next, next.propagateSkips(nil))
	if next.sites[0].keys[trigger.Name()].skipped || next.nodes[0].bodySkipped {
		t.Fatal("execution skip facts leaked into the shared definition")
	}
	for _, n := range next.nodes {
		if len(n.def.provides) != 0 && !n.site.keys[n.def.provides[0].name].skipped {
			t.Fatalf("mount-local FixSkip fact was lost at %q", n.at)
		}
	}
}
