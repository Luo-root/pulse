package yaml

import (
	"fmt"
	"sort"

	"github.com/Luo-root/pulse"
)

// Analyze 不用宿主注册表，把 YAML 的声明装配成静态拓扑。
//
// GraphID 必填；Graph / MaxRunning 的静态选项与 Load 相同。Analyze 与 Load 共用
// 解码、接线、子图装配和引擎的静态判据；根层的静态问题保留在 Topology.Problems，
// 子图和装配错误仍返回 error。它不创建执行、不运行工厂或切面、不取外部 seed 的值，
// 也不调用 ObserverFor 或观察回调。
//
// Type 是文档内的类型标签：同名键的标签必须一致，in/out 两端有已知标签时也必须一致。
// 标签沿绑定传播；仅出现在接线里、且不能由任何声明推导的键，其 Type 为空串。
// 任意标签（如 example.Item）原样保留；这不是实际 Go 类型的校验。显式 KeySpec 仍须
// 写 type，但工厂名是否登记、标签是否对应宿主的 Go 类型、literal 值能否赋给该类型，
// 都要由 Load / SeedPlan.Inputs 对账。Analyze 不从 literal 值猜类型。
func Analyze(data []byte, opts LoadOptions) (*pulse.Topology, error) {
	doc, err := decodeDocument(data, opts)
	if err != nil {
		return nil, err
	}
	reg, types, err := prepareAnalysis(&doc)
	if err != nil {
		return nil, err
	}
	// Load 的真图装配仍可复用，但分析不为任何挂载点建观察出口。
	opts.ObserverFor = nil
	g, _, err := loadDocument(doc, reg, opts)
	if err != nil {
		return nil, err
	}
	top := g.Topology()
	types.restore(top)
	return top, nil
}

// analysisTypes 只处理类型标签的等式，不推导来源、端口或依赖图。
// 同名键共用一个符号，与 Load 的单张 Registry 一致；绑定再把两个符号合并。
// 装图用 any 占位，返回前恢复标签，绝不把占位的 reflect 类型当成文档声明。
type analysisTypes struct {
	parent map[string]string
	tags   map[string]string
}

func (t *analysisTypes) root(name string) string {
	p, ok := t.parent[name]
	if !ok {
		t.parent[name] = name
		return name
	}
	if p != name {
		t.parent[name] = t.root(p)
	}
	return t.parent[name]
}

func (t *analysisTypes) declare(k KeySpec) error {
	if k.Name == "" {
		return fmt.Errorf("pulse: empty key name in document")
	}
	if k.Type == "" {
		return fmt.Errorf("pulse: key %q missing type tag", k.Name)
	}
	r := t.root(k.Name)
	if old := t.tags[r]; old != "" && old != k.Type {
		return fmt.Errorf("key %q has conflicting type tags %q and %q", k.Name, old, k.Type)
	}
	t.tags[r] = k.Type
	return nil
}

func (t *analysisTypes) bind(child, parent string) error {
	c, p := t.root(child), t.root(parent)
	if c == p {
		return nil
	}
	ct, pt := t.tags[c], t.tags[p]
	if ct != "" && pt != "" && ct != pt {
		return fmt.Errorf("key %q type %q does not match key %q type %q", child, ct, parent, pt)
	}
	t.parent[p] = c
	if ct == "" {
		t.tags[c] = pt
	}
	return nil
}

func (t *analysisTypes) tag(name string) string { return t.tags[t.root(name)] }

// prepareAnalysis 用文档标签约束临时键，再归一为 assembler 能接受的占位声明。
// 所有 seed 值换成非 nil 占位：只保留 Fix / FixSkip 的接线形状，不对原值做 coerce。
func prepareAnalysis(doc *Document) (*pulse.Registry, *analysisTypes, error) {
	types := &analysisTypes{parent: map[string]string{}, tags: map[string]string{}}
	collect := func(spec GraphSpec, where string) error {
		for _, n := range spec.Nodes {
			for _, side := range []struct {
				name string
				keys []KeySpec
			}{{"requires", n.Requires}, {"provides", n.Provides}} {
				for _, k := range side.keys {
					if err := types.declare(k); err != nil {
						return fmt.Errorf("pulse/yaml: %snode %q %s: %w", at(where), n.ID, side.name, err)
					}
				}
			}
		}
		for i, s := range spec.Seeds {
			if err := types.declare(s.Key); err != nil {
				return fmt.Errorf("pulse/yaml: %sseeds[%d]: %w", at(where), i, err)
			}
		}
		return nil
	}
	root := GraphSpec{Seeds: doc.Seeds, Nodes: doc.Nodes}
	if err := collect(root, ""); err != nil {
		return nil, nil, err
	}
	for _, name := range sortedNames(doc.Graphs) {
		if err := collect(doc.Graphs[name], name); err != nil {
			return nil, nil, err
		}
	}
	connect := func(nodes []NodeSpec, where string) error {
		for _, n := range nodes {
			if n.Graph == "" {
				continue
			}
			for _, side := range []map[string]string{n.In, n.Out} {
				for _, child := range sortedMapKeys(side) {
					if err := types.bind(child, side[child]); err != nil {
						return fmt.Errorf("pulse/yaml: %snode %q binds %q: %w", at(where), n.ID, side[child], err)
					}
				}
			}
		}
		return nil
	}
	if err := connect(doc.Nodes, ""); err != nil {
		return nil, nil, err
	}
	for _, name := range sortedNames(doc.Graphs) {
		if err := connect(doc.Graphs[name].Nodes, name); err != nil {
			return nil, nil, err
		}
	}

	reg := pulse.NewRegistry()
	names := make([]string, 0, len(types.parent))
	for name := range types.parent {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	placeholderTag := ""
	for _, name := range names {
		if err := pulse.RegisterKey(reg, pulse.NewKey[any](name)); err != nil {
			return nil, nil, err
		}
		placeholderTag, _ = reg.TypeTagOf(name)
	}
	factories := make(map[string]bool)
	normalize := func(spec GraphSpec) (GraphSpec, error) {
		for i := range spec.Nodes {
			n := &spec.Nodes[i]
			if n.Uses != "" && !factories[n.Uses] {
				// 占位执行体只满足 NewNode 的形状要求；分析从不启动这张图。
				if err := reg.Register(n.Uses, func(*pulse.RunCtx) error { return nil }); err != nil {
					return GraphSpec{}, err
				}
				factories[n.Uses] = true
			}
			for j := range n.Requires {
				n.Requires[j].Type = placeholderTag
			}
			for j := range n.Provides {
				n.Provides[j].Type = placeholderTag
			}
		}
		for i := range spec.Seeds {
			spec.Seeds[i].Key.Type = placeholderTag
			spec.Seeds[i].From.Value = struct{}{}
		}
		return spec, nil
	}
	root, err := normalize(root)
	if err != nil {
		return nil, nil, err
	}
	doc.Seeds, doc.Nodes = root.Seeds, root.Nodes
	for _, name := range sortedNames(doc.Graphs) {
		spec, err := normalize(doc.Graphs[name])
		if err != nil {
			return nil, nil, err
		}
		doc.Graphs[name] = spec
	}
	return reg, types, nil
}

// restore 只改类型标签，静态来源、接线、端口声明序和 Problems 都由引擎产出。
func (t *analysisTypes) restore(top *pulse.Topology) {
	keys := func(shapes []pulse.KeyShape) {
		for i := range shapes {
			shapes[i].Type = t.tag(shapes[i].Name)
		}
	}
	keys(top.Graph.InPorts)
	keys(top.Graph.OutPorts)
	for i := range top.Nodes {
		n := &top.Nodes[i]
		keys(n.Requires)
		keys(n.Provides)
		if n.Sub != nil {
			t.restore(n.Sub)
		}
	}
}
