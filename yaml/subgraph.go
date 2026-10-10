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
		if provided[childKey] {
			// 子图**自己**就有节点提供这条键：再让父侧 `in:` 喂一次，这条槽
			// 就有了两个来源。引擎的纪律是「一条键恰好一个来源」，所以跑到
			// 这个节点时会撞来源冲突——而这在装图期读一遍声明就能定。
			//
			// 除非这条键**同时**还挂在 `out:` 里：那种形状先让 `pulse.Sub` 报
			// 「同一个子键绑了两次」——错在绑定的形状，不在来源，那句话更贴。
			if _, both := n.Out[childKey]; !both {
				return nil, fmt.Errorf("node %q: graph %q provides %q inside, so the binding cannot also feed it with in: — "+
					"a key has exactly one source, and the parent's seed would hit %s the first time this step runs",
					n.ID, graphName, childKey, pulse.ErrDuplicateSource)
			}
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
	if err := addNodes(child, spec.Nodes, graphName, graphs, reg, opts); err != nil {
		return nil, err
	}
	// 子图的 seeds：只允许 literal（C3）——env / file / context 那几种要靠宿主
	// IO，而 SeedPlan 是**父图**的产物；把子图的计划向上冒泡是另一件事，
	// 等真有需要再说。校验在 Load 期做（checkSubSeeds）。
	for _, s := range spec.Seeds {
		if err := seedOne(child, graphName, s, reg); err != nil {
			return nil, err
		}
	}
	return child, nil
}

// seedOne 把子图声明里的一条 seed 种进 g，供**装配期**（Load 的校验图）与
// **运行期**（buildSub 建出来的那张图）共用：同一个函数 ⇒ 两边拒的东西永远
// 一致（键没登记、类型写错、值是 nil——这些都是静态可判定的，装配期报出来
// 比跑到那一层才报好）。
func seedOne(g *pulse.Graph, graphName string, s SeedSpec, reg *pulse.Registry) error {
	if s.Skip {
		if err := pulse.SkipSeedByName(g, reg, s.Key.Name, s.Key.Type); err != nil {
			return fmt.Errorf("pulse/yaml: graph %q seed %q: %w", graphName, s.Key.Name, err)
		}
		return nil
	}
	if err := pulse.SeedByName(g, reg, s.Key.Name, s.Key.Type, s.From.Value); err != nil {
		return fmt.Errorf("pulse/yaml: graph %q seed %q: %w", graphName, s.Key.Name, err)
	}
	return nil
}

// checkSite 是「装配期要静态校验的一张图」，按**引用点**列出来。
type checkSite struct {
	graph string   // 被校验的 spec 名
	owner string   // 引用它的那张图（"" = 根层）
	node  string   // 引用它的节点 id（谁也引用不到时为空）
	bound []string // 这个引用点用 `in:` 喂进来的子侧键（按名排序）
}

// via 说明这张图是**哪个引用点**在看它，附在校验报错后面：同一张 spec 被两处
// 引用时两边的 `in:` 可以不一样，同一句「这条键没来源」在两处看到的含义不同
// ——不说清是哪一处，读的人还得自己找。无人引用时为空串。
func (s checkSite) via() string {
	if s.node == "" {
		return ""
	}
	if s.owner == "" {
		return fmt.Sprintf(" (as referenced by node %q of the root graph)", s.node)
	}
	return fmt.Sprintf(" (as referenced by node %q of graph %q)", s.node, s.owner)
}

// checkSites 列出所有要静态校验的图：**每个引用点各一条**，加上谁也引用不到
// 的 spec 各一条。
//
// 为什么按引用点而不是按 spec：同一张 spec 被两处引用时，两边的 `in:` 可以不
// 一样，而「子图里这条键有没有来源」正是**按引用点**成立的——有一处没喂，
// 那张图跑到就一定会挂。按并集校验会把这种图放过去。
//
// 谁也引用不到的 spec 也过一遍（没有任何外部来源那种形状）：它是死配置，
// 但「写错了没人报」比「报了」难查得多。
func checkSites(doc Document) []checkSite {
	var sites []checkSite
	referenced := make(map[string]bool, len(doc.Graphs))
	add := func(owner string, specs []NodeSpec) {
		for _, n := range specs {
			if n.Graph == "" {
				continue
			}
			referenced[n.Graph] = true
			sites = append(sites, checkSite{
				graph: n.Graph,
				owner: owner,
				node:  n.ID,
				bound: sortedMapKeys(n.In),
			})
		}
	}
	add("", doc.Nodes) // 根图的节点先过：报错顺序跟着读文档的顺序走
	for _, name := range sortedNames(doc.Graphs) {
		add(name, doc.Graphs[name].Nodes)
	}
	for _, name := range sortedNames(doc.Graphs) {
		if !referenced[name] {
			sites = append(sites, checkSite{graph: name})
		}
	}
	return sites
}

// checkSubSeeds 在 Load 期拦掉子图 seed 本身的毛病（C3 + 重复声明）：
//
//   - 要靠宿主 IO 的 seed（`env` / `file` / `context`）——子图的 `SeedPlan`
//     不存在，只有 `literal` 能种；
//   - 同一张图里同一条键声明**两次** seed——第二次种值会被幂等首写静默忽略，
//     到底哪个值生效取决于声明顺序，两行看着都像是对的。
//
// 键名 / 类型 / 值能不能对上登记表，由装配期的校验图**种一遍**来判
// （seedOne 与运行期同一个函数）。
func checkSubSeeds(graphs map[string]GraphSpec) error {
	for _, name := range sortedNames(graphs) {
		seen := make(map[string]bool, len(graphs[name].Seeds))
		for _, s := range graphs[name].Seeds {
			switch s.From.Kind {
			case "", "literal":
			default:
				return fmt.Errorf("pulse/yaml: graph %q seed %q: kind %q is not supported in a subgraph "+
					"(only literal: env/file/context need host IO, and SeedPlan belongs to the parent graph)",
					name, s.Key.Name, s.From.Kind)
			}
			if seen[s.Key.Name] {
				return fmt.Errorf("pulse/yaml: graph %q: key %q is seeded twice — the second one would be "+
					"silently ignored (idempotent first write), so which value wins depends on declaration order",
					name, s.Key.Name)
			}
			seen[s.Key.Name] = true
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
