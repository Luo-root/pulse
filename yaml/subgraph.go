package yaml

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Luo-root/pulse"
)

// 声明式装配里的「一步 = 一张子图」（#295）。
//
// 这一层只把引擎已有的能力（`pulse.Sub`）**写出来**：拓扑归 YAML，`Run` 还是归
// 注册的工厂；子图是「拓扑的复用单元」，不该逼人为了复用一段三步流程去写 Go 工厂。
//
// 三条判据，与引擎侧同一套口径：
//
//   - **边界写在接线处**：`in` / `out` 就是 `pulse.In` / `pulse.Out`，一律
//     「**子键: 父键**」——读 YAML 一眼看清这一步吃什么、吐什么；
//   - **类型在 Load 期比**：两端各按 `{name, type}` 查注册表，两端用**同一个
//     type 记号**，对不上就在装图期报（YAML 是运行期装配，这已是最早的时机）；
//   - **子图是一次性的**：每次运行都按 spec 装一张新图（`pulse.Sub` 的工厂形态），
//     所以同一张子图被引用两次 = 两个独立实例。

// GraphSpec 是一张可复用子图的声明：形状与顶层文档一样（`seeds` + `nodes`），
// 但**没有**自己的 `graphs`——子图引用的是文档顶层那一张**扁平**的表（名字在
// 文档内唯一），递归靠「图引用图」表达，环在 `Load` 期拦掉。
type GraphSpec struct {
	Seeds []SeedSpec `yaml:"seeds"`
	Nodes []NodeSpec `yaml:"nodes"`
}

// checkGraphRefs 在 Load 期拦掉两类装不出图的问题（C4 / C5）：
// **引用一张不存在的子图**、**子图引用环**（含自引用）。两者都会装出一张
// 永远装不完或跑不起来的图，且都是「静态可判定」的——与引擎在 `Start` 拦
// 死图的口径一致。
//
// 环的报法照抄引擎的依赖环：给出一条**具体**的路径（`a -> b -> a`），
// 遍历按名字排序取第一条，所以同一份文档每次报同一句。
func checkGraphRefs(graphs map[string]GraphSpec) error {
	refs := make(map[string][]string, len(graphs))
	for _, name := range sortedNames(graphs) {
		spec := graphs[name]
		if len(spec.Nodes) == 0 {
			return fmt.Errorf("pulse/yaml: graph %q has no nodes", name)
		}
		for _, n := range spec.Nodes {
			if n.Graph == "" {
				continue
			}
			if _, ok := graphs[n.Graph]; !ok {
				return fmt.Errorf("pulse/yaml: graph %q node %q references unknown graph %q",
					name, n.ID, n.Graph)
			}
			refs[name] = append(refs[name], n.Graph)
		}
	}

	const (
		white = iota // 没进过
		grey         // 在当前这条路径上
		black        // 收过尾了
	)
	color := make(map[string]int, len(graphs))
	var stack []string
	var visit func(name string) error
	visit = func(name string) error {
		color[name] = grey
		stack = append(stack, name)
		kids := append([]string(nil), refs[name]...)
		sort.Strings(kids)
		for _, k := range kids {
			switch color[k] {
			case grey:
				i := 0
				for j, s := range stack {
					if s == k {
						i = j
						break
					}
				}
				path := append(append([]string(nil), stack[i:]...), k)
				return fmt.Errorf("pulse/yaml: graph reference cycle: %s", strings.Join(path, " -> "))
			case white:
				if err := visit(k); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[name] = black
		return nil
	}
	for _, name := range sortedNames(graphs) {
		if color[name] == white {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// declaredKeys 扫出一张图**声明过**的键名，分「有人读」与「有人写」两组
// （不记类型——类型按名字去注册表查，注册表本来就是 name → type）。
//
// 图里的子图节点也算它的边界：那个节点在**本图**里的 `Requires` / `Provides`
// 正是它 `in` / `out` 的父侧端。
func declaredKeys(spec GraphSpec) (required, provided map[string]bool) {
	required, provided = map[string]bool{}, map[string]bool{}
	for _, n := range spec.Nodes {
		for _, k := range n.Requires {
			required[k.Name] = true
		}
		for _, k := range n.Provides {
			provided[k.Name] = true
		}
		for _, parentKey := range n.In {
			required[parentKey] = true
		}
		for _, parentKey := range n.Out {
			provided[parentKey] = true
		}
	}
	return required, provided
}

// subBinds 把一条 `graph:` 节点的 `in` / `out` 翻成 `pulse.Sub` 的绑定。
//
// 顺序**确定**（键名排序）：父侧节点的 Requires / Provides 顺序跟着它走，
// 报错也才稳定。
func subBinds(n NodeSpec, graphName string, child GraphSpec, reg *pulse.Registry) ([]pulse.SubBind, error) {
	if len(n.In)+len(n.Out) == 0 {
		return nil, fmt.Errorf("node %q: a graph node needs in/out to say what this step eats and produces", n.ID)
	}
	if len(n.Requires)+len(n.Provides) > 0 {
		return nil, fmt.Errorf("node %q: a graph node declares its boundary with in/out, not requires/provides", n.ID)
	}
	required, provided := declaredKeys(child)
	// 子图自己给这条键声明了 seed、父侧又用 in 传值：两次种同一条槽，第二次
	// 会被幂等首写**静默忽略**（谁赢取决于先种的那次），而两处声明看着都有道理。
	// 在装图期吵出来，点名哪张图、哪个节点、哪条键。
	seeded := map[string]bool{}
	for _, s := range child.Seeds {
		seeded[s.Key.Name] = true
	}

	// 解析一对「子侧键 ↔ 父侧键」。**类型记号取自子侧那条键**，父侧用同一个记号
	// 去查——所以「两端类型不一致」在这里就报（报的是父侧那条对不上），不需要
	// 另写一条比对逻辑；键没登记、type 写错也都在这两次查表里出结果。
	//
	// 写成闭包而不是函数：`keyRef` 是 pulse 包内部类型，跨包只能以**推断**的
	// 形式拿着用（与 `KeyRefs` / `NewNode` 同一个玩法），写不进函数签名。
	bind := func(childKey, parentKey string, in bool) (pulse.SubBind, error) {
		tag, ok := reg.TypeTagOf(childKey)
		if !ok {
			return pulse.SubBind{}, fmt.Errorf("node %q: graph %q binds key %q, which is not registered",
				n.ID, graphName, childKey)
		}
		childRef, err := reg.ResolveKey(childKey, tag)
		if err != nil {
			return pulse.SubBind{}, fmt.Errorf("node %q: %w", n.ID, err)
		}
		parentRef, err := reg.ResolveKey(parentKey, tag)
		if err != nil {
			return pulse.SubBind{}, fmt.Errorf("node %q binds %q: %w", n.ID, parentKey, err)
		}
		if in {
			return pulse.InRef(parentRef, childRef), nil
		}
		return pulse.OutRef(childRef, parentRef), nil
	}

	binds := make([]pulse.SubBind, 0, len(n.In)+len(n.Out))
	for _, childKey := range sortedMapKeys(n.In) {
		if !required[childKey] {
			return nil, fmt.Errorf("node %q: graph %q reads %q in the binding but no node of it requires that key",
				n.ID, graphName, childKey)
		}
		if seeded[childKey] {
			return nil, fmt.Errorf("node %q: graph %q seeds %q and the binding also feeds it with in: — "+
				"one of the two is redundant, and the one from the parent would be silently ignored",
				n.ID, graphName, childKey)
		}
		b, err := bind(childKey, n.In[childKey], true)
		if err != nil {
			return nil, err
		}
		binds = append(binds, b)
	}
	for _, childKey := range sortedMapKeys(n.Out) {
		if !provided[childKey] {
			return nil, fmt.Errorf("node %q: graph %q produces %q in the binding but no node of it provides that key",
				n.ID, graphName, childKey)
		}
		b, err := bind(childKey, n.Out[childKey], false)
		if err != nil {
			return nil, err
		}
		binds = append(binds, b)
	}
	return binds, nil
}

// buildSub 是 `pulse.Sub` 的工厂：**每次运行**都按 spec 装一张新图，
// 一次性契约天然满足。
//
// 出口按层给（`LoadOptions.ObserverFor`）：声明式装配里每一层是另一张图，
// 一个出口实例只能带一条 `pulse.path`，所以「按层各建一个」这件事得由宿主
// 决定——这里把这一层的路径交给它。
func buildSub(sc *pulse.SubCtx, graphName string, spec GraphSpec,
	graphs map[string]GraphSpec, reg *pulse.Registry, opts LoadOptions) (*pulse.Graph, error) {

	childOpts := append([]pulse.Option(nil), opts.Graph...)
	if opts.ObserverFor != nil {
		if o := opts.ObserverFor(sc.Path()); o != nil {
			childOpts = append(childOpts, pulse.WithObserver(o)) // 后写覆盖 opts.Graph 里的那条
		}
	}
	child, err := pulse.New(sc.Context(), sc.GraphID(), childOpts...)
	if err != nil {
		return nil, err
	}
	if err := addNodes(child, spec.Nodes, graphs, reg, opts); err != nil {
		return nil, err
	}
	// 子图的 seeds：只允许 literal（C3）——env / file / context 那几种要靠宿主
	// IO，而 SeedPlan 是**父图**的产物；把子图的计划向上冒泡是另一件事，
	// 等真有需要再说。校验在 Load 期做（checkSubSeeds）。
	for _, s := range spec.Seeds {
		if s.Skip {
			if err := pulse.SkipSeedByName(child, reg, s.Key.Name, s.Key.Type); err != nil {
				return nil, fmt.Errorf("pulse/yaml: graph %q seed %q: %w", graphName, s.Key.Name, err)
			}
			continue
		}
		if err := pulse.SeedByName(child, reg, s.Key.Name, s.Key.Type, s.From.Value); err != nil {
			return nil, fmt.Errorf("pulse/yaml: graph %q seed %q: %w", graphName, s.Key.Name, err)
		}
	}
	return child, nil
}

// checkSubSeeds 在 Load 期拦掉子图里那些「要靠宿主 IO」的 seed（C3）。
// 留在运行期报也行，但那是「装图全过、跑到一半才炸」，而这是静态可判定的。
func checkSubSeeds(graphs map[string]GraphSpec) error {
	for _, name := range sortedNames(graphs) {
		for _, s := range graphs[name].Seeds {
			switch s.From.Kind {
			case "", "literal":
			default:
				return fmt.Errorf("pulse/yaml: graph %q seed %q: kind %q is not supported in a subgraph "+
					"(only literal: env/file/context need host IO, and SeedPlan belongs to the parent graph)",
					name, s.Key.Name, s.From.Kind)
			}
		}
	}
	return nil
}

func sortedNames(m map[string]GraphSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
