package yaml

import (
	"fmt"
	"sort"

	"github.com/Luo-root/pulse"
)

// Analyze 不用宿主注册表，把 YAML 的声明装配成静态拓扑。
//
// GraphID 必填；Graph / MaxRunning 先按 Load 的规则应用。Analyze 与 Load 共用
// 解码、接线、子图装配及不依赖真实键类型的静态判据；这部分根层问题保留在
// Topology.Problems，子图和装配错误仍返回 error。它不创建执行、不运行工厂或切面，
// 不取外部 seed 的值，也不调用 ObserverFor 或观察回调。
//
// Type 是文档内的类型标签：同名键的标签必须一致，in/out 两端有已知标签时也必须一致。
// 标签沿包括宿主 Sub 在内的非固定绑定传播，宿主内部键按挂载点区分；
// 仅出现在接线里、且不能由任何声明推导的键，其 Type 为空串。
// 任意标签（如 example.Item）原样保留；这不是实际 Go 类型的校验。显式 KeySpec 仍须
// 写 type，但工厂名是否登记、标签是否对应宿主的 Go 类型、literal 值能否赋给该类型，
// 都要由 Load / SeedPlan.Inputs 对账。Analyze 不从 literal 值猜类型。
//
// Graph 选项仍会应用，预置节点、端口和 Sub 接线与 YAML 一起以 any 占位装配；
// 宿主 Go 类型不参与标签校验。恢复标签不会重新执行依赖真实类型的检查。因此即使
// 标签显式写成 pulse.Stream[int] 或 channel，Analyze 也不证明流消费者、共享额度及
// 生命周期约束。须用有 Registry 的 Load 后的 Topology / Validate / Start 检查这些约束。
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
	inspect := func(top *pulse.Topology, spec GraphSpec) error {
		return newAnalysisTypes(types).connect(top, &spec, doc.Graphs)
	}
	g, _, err := loadDocument(doc, reg, opts, newAnalysisRoot, inspect)
	if err != nil {
		return nil, err
	}
	top := g.Topology()
	root := GraphSpec{Seeds: doc.Seeds, Nodes: doc.Nodes}
	scoped := newAnalysisTypes(types)
	if err := scoped.connect(top, &root, doc.Graphs); err != nil {
		return nil, err
	}
	scoped.restore(top)
	return top, nil
}

// newAnalysisRoot 先应用宿主选项，再把静态声明也归一为 any，避免预置的 typed key
// 与 YAML 占位键冲突。不复制执行体、切面或观察者，也不把宿主 Go 类型当作文档标签。
func newAnalysisRoot(id, description string, opts LoadOptions) (*pulse.Graph, error) {
	g, err := newRootGraph(id, description, opts)
	if err != nil {
		return nil, err
	}
	return analysisGraph(g.Topology())
}

// analysisGraph 只复制装配所需的形状；来源、依赖和 Problems 仍由核心重新计算。
func analysisGraph(top *pulse.Topology) (*pulse.Graph, error) {
	g, err := pulse.New(top.Graph.ID, top.Graph.Description)
	if err != nil {
		return nil, err
	}
	keys := func(shapes []pulse.KeyShape) []pulse.Key[any] {
		out := make([]pulse.Key[any], len(shapes))
		for i, k := range shapes {
			out[i] = pulse.NewKey[any](k.Name)
		}
		return out
	}
	ports := func(shapes []pulse.KeyShape) []pulse.Port {
		out := make([]pulse.Port, len(shapes))
		for i, k := range keys(shapes) {
			out[i] = pulse.NewPort(k)
		}
		return out
	}
	if err := g.DeclareInput(ports(top.Graph.InPorts)...); err != nil {
		return nil, err
	}
	if err := g.DeclareOutput(ports(top.Graph.OutPorts)...); err != nil {
		return nil, err
	}
	for _, n := range top.Nodes {
		if n.Sub == nil {
			err = g.Add(pulse.NewNode(n.ID, n.Description, pulse.Requires(keys(n.Requires)...), pulse.Provides(keys(n.Provides)...), func(*pulse.RunCtx) error { return nil }))
		} else {
			child, buildErr := analysisGraph(n.Sub)
			if buildErr != nil {
				return nil, buildErr
			}
			binds := make([]pulse.SubBind, 0, len(n.Binds))
			for _, b := range n.Binds {
				parent, port := pulse.NewKey[any](b.Parent), pulse.NewKey[any](b.Child)
				switch {
				case b.Fixed:
					// 固定值不在 Topology 中；与 YAML seed 一样只保留固定入口的形状。
					binds = append(binds, pulse.Fix(port, any(struct{}{})))
				case b.In:
					binds = append(binds, pulse.In(parent, port))
				default:
					binds = append(binds, pulse.Out(port, parent))
				}
			}
			err = pulse.Sub(g, n.ID, n.Description, binds, child)
		}
		if err != nil {
			return nil, err
		}
	}
	return g, nil
}

// 文档键沿用单张 Registry 的名字空间；实际图里的键按挂载位置区分。
// 同一宿主定义挂两处时，内部同名键只有经显式绑定才会建立标签关系。
type analysisKey struct {
	name, path string
	document   bool
}

// analysisTypes 只处理类型标签的等式，不推导来源、端口或依赖图。
// 装图用 any 占位，返回前恢复标签，绝不把占位的 reflect 类型当成文档声明。
type analysisTypes struct {
	parent map[analysisKey]analysisKey
	tags   map[analysisKey]string
	basis  *analysisTypes
}

// 每棵装配树独立保存等式，只借用已收敛的文档符号，避免校验树与主图串用挂载身份。
func newAnalysisTypes(basis *analysisTypes) *analysisTypes {
	return &analysisTypes{parent: map[analysisKey]analysisKey{}, tags: map[analysisKey]string{}, basis: basis}
}

func (t *analysisTypes) root(key analysisKey) analysisKey {
	if t.basis != nil && key.document {
		key = t.basis.root(key)
	}
	p, ok := t.parent[key]
	if !ok {
		t.parent[key] = key
		if t.basis != nil && key.document {
			t.tags[key] = t.basis.tags[key]
		}
		return key
	}
	if p != key {
		t.parent[key] = t.root(p)
	}
	return t.parent[key]
}

func (t *analysisTypes) declare(k KeySpec) error {
	if k.Name == "" {
		return fmt.Errorf("pulse: empty key name in document")
	}
	if k.Type == "" {
		return fmt.Errorf("pulse: key %q missing type tag", k.Name)
	}
	r := t.root(analysisKey{name: k.Name, document: true})
	if old := t.tags[r]; old != "" && old != k.Type {
		return fmt.Errorf("key %q has conflicting type tags %q and %q", k.Name, old, k.Type)
	}
	t.tags[r] = k.Type
	return nil
}

func (t *analysisTypes) bind(child, parent string) error {
	return t.bindKeys(analysisKey{name: child, document: true}, analysisKey{name: parent, document: true})
}

func (t *analysisTypes) bindKeys(child, parent analysisKey) error {
	c, p := t.root(child), t.root(parent)
	if c == p {
		return nil
	}
	ct, pt := t.tags[c], t.tags[p]
	if ct != "" && pt != "" && ct != pt {
		return fmt.Errorf("key %q type %q does not match key %q type %q", child.name, ct, parent.name, pt)
	}
	t.parent[p] = c
	if ct == "" {
		t.tags[c] = pt
	}
	return nil
}

func (t *analysisTypes) tag(key analysisKey) string { return t.tags[t.root(key)] }

// prepareAnalysis 用文档标签约束临时键，再归一为 assembler 能接受的占位声明。
// 所有 seed 值换成非 nil 占位：只保留 Fix / FixSkip 的接线形状，不对原值做 coerce。
func prepareAnalysis(doc *Document) (*pulse.Registry, *analysisTypes, error) {
	types := newAnalysisTypes(nil)
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
	for key := range types.parent {
		if key.name != "" {
			names = append(names, key.name)
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

// connect 把文档声明接到实际所在层，再让整树的非固定绑定共用同一套标签等式。
// spec 为 nil 的层来自宿主，只经显式绑定取得标签，不能按同名键套用文档标签。
func (t *analysisTypes) connect(top *pulse.Topology, spec *GraphSpec, graphs map[string]GraphSpec) error {
	yamlChildren := make(map[string]string)
	if spec != nil {
		link := func(name string) error {
			if err := t.bindKeys(analysisKey{name: name, path: top.Graph.Path}, analysisKey{name: name, document: true}); err != nil {
				return fmt.Errorf("pulse/yaml: graph at %q key %q: %w", top.Graph.Path, name, err)
			}
			return nil
		}
		// YAML 子图的接口由挂载点声明，包含只出现在 in/out 的透传键。
		// 根图接口还可能来自宿主；它们只能由本层文档实际提到的键接入。
		if top.Graph.Path != "" {
			for _, ports := range [][]pulse.KeyShape{top.Graph.InPorts, top.Graph.OutPorts} {
				for _, k := range ports {
					if err := link(k.Name); err != nil {
						return err
					}
				}
			}
		}
		for _, s := range spec.Seeds {
			if err := link(s.Key.Name); err != nil {
				return err
			}
		}
		for _, n := range spec.Nodes {
			for _, side := range [][]KeySpec{n.Requires, n.Provides} {
				for _, k := range side {
					if err := link(k.Name); err != nil {
						return err
					}
				}
			}
			for _, side := range []map[string]string{n.In, n.Out} {
				for _, child := range sortedMapKeys(side) {
					if err := link(side[child]); err != nil {
						return err
					}
				}
			}
			if n.Graph != "" {
				yamlChildren[n.ID] = n.Graph
			}
		}
	}
	for _, n := range top.Nodes {
		if n.Sub == nil {
			continue
		}
		for _, b := range n.Binds {
			if b.Fixed {
				continue
			}
			child := analysisKey{name: b.Child, path: n.Sub.Graph.Path}
			parent := analysisKey{name: b.Parent, path: top.Graph.Path}
			if err := t.bindKeys(child, parent); err != nil {
				return fmt.Errorf("pulse/yaml: node %q binds %q: %w", n.Sub.Graph.Path, b.Parent, err)
			}
		}
		var childSpec *GraphSpec
		if name, ok := yamlChildren[n.ID]; ok {
			s := graphs[name]
			childSpec = &s
		}
		if err := t.connect(n.Sub, childSpec, graphs); err != nil {
			return err
		}
	}
	return nil
}

// restore 只改类型标签，静态来源、接线、端口声明序和 Problems 都由引擎产出。
func (t *analysisTypes) restore(top *pulse.Topology) {
	keys := func(shapes []pulse.KeyShape) {
		for i := range shapes {
			shapes[i].Type = t.tag(analysisKey{name: shapes[i].Name, path: top.Graph.Path})
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
