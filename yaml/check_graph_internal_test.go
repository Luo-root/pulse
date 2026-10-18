package yaml

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Luo-root/pulse"
)

// 完整树检查仍拒绝已挂载子树的额度问题；校验子图自身不重复声明根选项。
// YAML 工厂不携带原生流糖元数据，故用同一装配器建子图后补上已知 Produce/Consume。
func TestCheckMountedSubgraphsKeepsInheritedQuota(t *testing.T) {
	for _, max := range []int{1, 2} {
		t.Run(fmt.Sprintf("max=%d", max), func(t *testing.T) {
			reg := pulse.NewRegistry()
			reg.MustRegister("noop", func(*pulse.RunCtx) error { t.Error("check must not run"); return nil })
			spec := GraphSpec{Nodes: []NodeSpec{{ID: "noop", Uses: "noop"}}}
			a := &assembler{
				doc: Document{Graphs: map[string]GraphSpec{"child": spec}},
				reg: reg, opts: LoadOptions{GraphID: "quota-context", MaxRunning: max}, check: true,
			}
			n := NodeSpec{ID: "mount", Graph: "child"}
			child, err := a.buildChild(n, mount{id: "quota-context/mount", path: "mount", where: "child"})
			if err != nil {
				t.Fatal(err)
			}
			stream := pulse.NewKey[<-chan int]("quota-stream")
			if err := pulse.Produce(child, "writer", "", stream, func(*pulse.RunCtx, func(int) error) error { return nil }); err != nil {
				t.Fatalf("unmounted child inherits quota only at its mount, rather than redeclaring root options: %v", err)
			}
			if err := pulse.Consume(child, "reader", "", stream, func(*pulse.RunCtx, int) error { return nil }); err != nil {
				t.Fatal(err)
			}
			spec.Nodes = append(spec.Nodes, NodeSpec{ID: "writer"}, NodeSpec{ID: "reader"})
			a.doc.Graphs["child"] = spec
			root, err := newRootGraph("quota-context", "", a.opts)
			if err != nil {
				t.Fatal(err)
			}
			entry := pulse.NewKey[int]("entry")
			if err := child.DeclareInput(pulse.NewPort(entry)); err != nil {
				t.Fatal(err)
			}
			if err := pulse.Sub(root, n.ID, "", []pulse.SubBind{pulse.Fix(entry, 1)}, child); err != nil {
				t.Fatal(err)
			}
			err = a.checkSubgraphs(root.Topology(), []NodeSpec{n})
			if max == 1 {
				if err == nil || !strings.Contains(err.Error(), `graph "child" (mounted at "mount")`) || !strings.Contains(err.Error(), "stream needs 2 slots") {
					t.Fatalf("mounted quota failure must remain static and path-specific: %v", err)
				}
			} else if err != nil {
				t.Fatalf("sufficient inherited capacity must pass: %v", err)
			}
		})
	}
}

// 校验图必须与真图**同额度**，否则流式那道判据在装配期是空的。
//
// 这条只能在包内测：`newRootGraph` 未导出，而公开面上也**看不见**这件事——`Produce` /
// `Consume` / `Tee` 是 Go 糖，而 YAML 的工厂拿到的是 `*RunCtx`，装配期连 `*Graph` 都
// 摸不到，所以声明式那条路今天表达不了流。它是给「YAML 也能声明流」那天留的闸门，
// 也钉住「校验图 = 真图的选项」这件事。
//
// 额度不足时拒在**装配那一刻**（`Produce` 自己的快速检查就按容量判），不是等 `Validate`。
func TestCheckGraphCarriesCapacity(t *testing.T) {
	build := func(maxRunning int) (*pulse.Graph, error) {
		g, err := newRootGraph("chk", "", LoadOptions{MaxRunning: maxRunning})
		if err != nil {
			return nil, err
		}
		out := pulse.NewKey[<-chan int]("chk.out")
		if err := pulse.Produce(g, "prod", "", out,
			func(rc *pulse.RunCtx, send func(int) error) error { return nil }); err != nil {
			return nil, err
		}
		if err := pulse.Consume(g, "cons", "", out,
			func(rc *pulse.RunCtx, v int) error { return nil }); err != nil {
			return nil, err
		}
		return g, nil
	}

	// 这条路的两端必须同时活着（2 个名额）；额度 1 养不起 → 装配期就该拒。
	// 校验图没有额度时的旧行为：这里什么都不报，`Validate` 也过——一路装到最后交出去。
	if _, err := build(1); err == nil || !strings.Contains(err.Error(), "cannot host this stream") {
		t.Fatalf("额度 1 养不起两端同时活着的流，装配期必须拒：%v", err)
	}

	// 对照一：额度够 → 装配与 `Validate` 都通过（否则上面那条可能只是「什么都拒」）。
	roomy, err := build(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := roomy.Validate(); err != nil {
		t.Fatalf("额度 2 装得下这条流，不该拒：%v", err)
	}

	// 对照二：不给额度（<=0 = 不限）时也不该拒。
	free, err := build(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := free.Validate(); err != nil {
		t.Fatalf("不限额度时不该拒：%v", err)
	}
}
