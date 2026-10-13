package yaml

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Luo-root/pulse"
)

// 声明式装配里的「一步 = 一张子图」（#295、#313 阶段 1）。
//
// 分工没变：**拓扑归 YAML**（`in` / `out` 写接线、`graphs` 写可复用的子图），`Run` 还是
// 归注册的工厂；子图是「拓扑的复用单元」，不该逼人为了复用一段三步流程去写 Go 工厂。
//
// 四条判据，与引擎侧同一套口径：
//
//   - **边界写在接线处**：`in` / `out` 就是 `pulse.In` / `pulse.Out`，一律
//     「**子键: 父键**」——读 YAML 一眼看清这一步吃什么、吐什么；
//   - **子图在 Load 期装一次**：一格 `graph:` 节点 = 一张**装配期就建好**的子图
//     （`pulse.Sub`），所以父图在运行之前就能递归看完整个子树
//     （`Topology().Nodes[i].Sub`），子图的静态毛病也在 `Load` 那一刻报出来，而不是
//     等跑到那一层才炸；
//   - **按引用点**：同一张 spec 挂几处就是几张**独立实例**，每处的 `in` / `out` 各写各的
//     ——端口按那一处的接线声明，校验也按那一处各来一条（同一张图两处引用、只有一处没喂
//     那条输入，报的就是没喂的那一处，见 #298）；
//   - **类型在 Load 期比**：两端各按 `{name, type}` 查注册表，两端用**同一个 type 记号**，
//     对不上就在装图期报（YAML 是运行期装配，这已是最早的时机）；
//   - **透传**：同一条子侧键同时挂在 `in:` 与 `out:` —— 与引擎「同名入口 + 出口是透传」
//     同一条规则（见 `problemsOutputsLocked`），那条键的值来自父侧，所以不要求子图内部有人
//     读它 / 有人产出它；但**内部有人产出它**仍要拒：那与父侧那次喂值是同一个槽的两个来源。
//
// `retry:` / `timeout:` 落在子图节点上照装（见 `nodeAspects` 与 addSubNode）：先列的
// 更靠外，所以 `timeout: 30s` 是「整张子图限时」，而 `retry:` 让这一步**整段**（含子图
// 那一次运行）按尝试重来——每次尝试各建一份新的子执行，失败那一次的桥回随它那条写暂存
// 一起作废（引擎的 attempt 写集，见 `pulse.Retry`）。Go 侧同一个形状同一条语义。

// GraphSpec 是一张可复用子图的声明：形状与顶层文档一样（`seeds` + `nodes`），
// 但**没有**自己的 `graphs`——子图引用的是文档顶层那一张**扁平**的表（名字在
// 文档内唯一），递归靠「图引用图」表达，环在 `Load` 期拦掉。
type GraphSpec struct {
	// Description 落到**这张 spec 装出来的每张子图**的 `Description()`（一处挂载
	// 一张）；`""` = 没写。根图的那条在文档顶层。
	Description string     `yaml:"description"`
	Seeds       []SeedSpec `yaml:"seeds"`
	Nodes       []NodeSpec `yaml:"nodes"`
}

// checkGraphRefs 在 Load 期拦掉两类装不出图的问题（C4 / C5）：
// **引用一张不存在的子图**、**子图引用环**（含自引用）。两者都会装出一张
// 永远装不完或跑不起来的图，且都是「静态可判定」的——与引擎在 `Start` 拦
// 死图的口径一致。
//
// 引用环这一条还挡着装配器的递归：装配是**按挂载位置递归**建图的（一格
// `graph:` 节点就当场建一张子图），环会让它转不完。引擎里那道组合环判据
// （`Sub` 挂载时沿子树查祖先）拦的是「已经建好的图被挂进它自己的子树」，
// 到不了「装配期还在往下建」这一步，所以这一条留在装配器这边。
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

// subBinds 把一条 `graph:` 节点的 `in` / `out` 翻成引擎的绑定（`pulse.InRef` /
// `pulse.OutRef`）。
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
	// **透传**：同一条子侧键既挂在 `in:` 又挂在 `out:`。它是一条声明——父侧喂进去的值
	// 就是这条出口的值（引擎的出口判据同样认它：同名入口 + 出口是透传，见
	// problemsOutputsLocked）。所以下面两条「内部得有人读 / 有人产出」对透传不适用：
	// 那条键的值来自父侧，不在子图内部产生。
	passThrough := func(childKey string) bool {
		_, ok := n.Out[childKey]
		return ok
	}
	for _, childKey := range sortedMapKeys(n.In) {
		if !required[childKey] && !passThrough(childKey) {
			return nil, fmt.Errorf("node %q: graph %q reads %q in the binding but no node of it requires that key",
				n.ID, graphName, childKey)
		}
		if provided[childKey] {
			// 子图**自己**就有节点提供这条键：再让父侧 `in:` 喂一次，这条槽就有了两个
			// 来源。引擎的纪律是「一条键恰好一个来源」，所以跑到这个节点时会撞来源冲突
			// ——而这在装图期读一遍声明就能定。透传也救不了它：喂进来的值与内部那次产出
			// 是同一个槽的两个来源，照样撞（`checkBinds` 在引擎那侧判同一件事）。
			return nil, fmt.Errorf("node %q: graph %q provides %q inside, so the binding cannot also feed it with in: — "+
				"a key has exactly one source, and the parent's seed would hit %s the first time this step runs",
				n.ID, graphName, childKey, pulse.ErrDuplicateSource)
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
		if !provided[childKey] && !passThrough(childKey) {
			return nil, fmt.Errorf("node %q: graph %q produces %q in the binding but no node of it provides that key",
				n.ID, graphName, childKey)
		}
		b, err := bind(childKey, n.Out[childKey], false)
		if err != nil {
			return nil, err
		}
		binds = append(binds, b)
	}
	// 子图自己声明的 `seeds:` **不再写进子图定义**（值属于一次运行）：它翻成这个挂载点上
	// 的一组 `pulse.Fix` 绑定——「这一步给子图那条进口一个常量」。声明那一半（端口）由
	// `declarePorts` 在子图那侧补上（见那里）。顺序按键名排序，报文才稳定。
	for _, s := range sortedSeeds(child.Seeds) {
		var (
			b   pulse.SubBind
			err error
		)
		if s.Skip {
			b, err = pulse.FixSkipByName(reg, s.Key.Name, s.Key.Type)
		} else {
			b, err = pulse.FixByName(reg, s.Key.Name, s.Key.Type, s.From.Value)
		}
		if err != nil {
			return nil, fmt.Errorf("node %q: graph %q seed %q: %w", n.ID, graphName, s.Key.Name, err)
		}
		binds = append(binds, b)
	}
	return binds, nil
}

// sortedSeeds 按键名排序，装图与报文顺序才稳定（同一份文档每次报同一句）。
func sortedSeeds(seeds []SeedSpec) []SeedSpec {
	out := append([]SeedSpec(nil), seeds...)
	sort.Slice(out, func(i, j int) bool { return out[i].Key.Name < out[j].Key.Name })
	return out
}

// mount 是「这一批声明装在哪一层」的两条坐标 + 一句「这是哪张 spec」。
//
//   - `id` 是这一层的**图身份**：根 = `LoadOptions.GraphID`，一层子图 =
//     `<父 id>/<节点 id>`，它进观测的 `pulse.graph`；
//   - `path` 是这一层的**挂载位置**：根 = `""`，一层 = 挂载它的那个节点 id，再深 =
//     `outer/inner`（与引擎的 `joinPath` 同一套写法），它进观测的 `pulse.path`；
//   - `where` 是这批声明属于哪张 spec（根层为空串），只用于把报错点到位。
//
// 根那一层 id 有值、path 为空，所以两者得分开存：「身份」与「位置」揉进一个字段的话，
// 根这一层会把图 id 当成路径写进观测。
type mount struct {
	id    string
	path  string
	where string
}

// child 是「挂在 n 这个位置上的那一层」：id 与 path 各加一段。
func (m mount) child(n NodeSpec) mount {
	return mount{id: m.id + "/" + n.ID, path: joinPath(m.path, n.ID), where: n.Graph}
}

// joinPath 拼嵌套路径：根为空串，一层是挂载它的那个节点 id，再深是 `outer/inner`。
// 与引擎的 `joinPath` 同一套写法（节点 id 不许含 `/` 就是为它）。
func joinPath(at, id string) string {
	if at == "" {
		return id
	}
	return at + "/" + id
}

// mountedAt 是校验报错里那句「挂在哪」：同一张 spec 挂两处时，两处看到的含义不同
// （见 assembler.buildChild），报文里带上路径，读的人照着就能找到该改的那一处。
func mountedAt(path string) string {
	return fmt.Sprintf(" (mounted at %q)", path)
}

// assembler 是这一遍装配的全部状态。同一段装配代码走**两遍**：先装一棵只装不跑的
// **校验树**，等它整棵过完，再用同一段代码装真图（见 Load）。
//
// 两遍的差别只有两处，都是「校验树只装不跑」的直接后果：
//
//   - 子图**在 `Load` 期装一次**（`pulse.Sub`）：父图在运行之前就递归看得见整棵子树，
//     子图的静态毛病也在这一刻报出来——校验树要的就是这个形状；
//   - **不建出口**：`ObserverFor` 不调——校验树一个记录也不会产生，宿主的回调次数不会
//     因为「多校验一遍」而变。
//
// 其余一切（接线检查、端口声明、递归、报错文案）都是同一份，所以「校验期报的那句」与
// 「真图装到一半报的那句」逐字相同。
type assembler struct {
	doc   Document
	reg   *pulse.Registry
	opts  LoadOptions
	check bool
}

// addNodes 把一批节点声明装进 g。工厂节点与子图节点在这里分流：子图节点被翻成一个
// **静态挂载**（`pulse.Sub`）。`retry` / `timeout` 都由 nodeAspects 交给引擎；Retry
// 每次尝试的子执行与桥回写集彼此隔离（见 addSubNode）。
//
// 子图里的子图走的是同一个函数（`buildChild` 递归调它），所以嵌套不设深度上限；
// 引用环已经在 `checkGraphRefs` 拦掉了，这里不会转圈。
//
// `m` 说这批声明装在哪一层（根层传 `{id: GraphID}`），报错与观测都按它定位。
func (a *assembler) addNodes(g *pulse.Graph, specs []NodeSpec, m mount) error {
	for i, n := range specs {
		if n.ID == "" {
			return fmt.Errorf("pulse/yaml: %snodes[%d] missing id", at(m.where), i)
		}
		aspects := nodeAspects(n)
		switch {
		case n.Graph != "" && n.Uses != "":
			return fmt.Errorf("pulse/yaml: %snode %q: uses and graph are mutually exclusive", at(m.where), n.ID)

		case n.Graph != "":
			if err := a.addSubNode(g, n, m, aspects); err != nil {
				return err
			}

		case n.Uses != "":
			// 工厂节点的边界只有 `requires` / `provides` 一处说法。写了 `in` /
			// `out` 不报的话，配置看着接好了、实际一个字都没生效（YAML 会照常
			// 解码这两个字段）——与子图节点拒 `requires` / `provides` 对称。
			if len(n.In) > 0 || len(n.Out) > 0 {
				return fmt.Errorf("pulse/yaml: %snode %q: a factory node declares its boundary with requires/provides, not in/out",
					at(m.where), n.ID)
			}
			run, ok := a.reg.Lookup(n.Uses)
			if !ok {
				return fmt.Errorf("pulse/yaml: %snode %q uses unknown factory %q", at(m.where), n.ID, n.Uses)
			}
			requires, err := a.reg.KeyRefs(toNameTypes(n.Requires))
			if err != nil {
				return fmt.Errorf("pulse/yaml: %snode %q requires: %w", at(m.where), n.ID, err)
			}
			provides, err := a.reg.KeyRefs(toNameTypes(n.Provides))
			if err != nil {
				return fmt.Errorf("pulse/yaml: %snode %q provides: %w", at(m.where), n.ID, err)
			}
			if err := g.Add(pulse.NewNode(n.ID, n.Description, requires, provides, run, aspects...)); err != nil {
				return fmt.Errorf("pulse/yaml: %snode %q: %w", at(m.where), n.ID, err)
			}

		default:
			return fmt.Errorf("pulse/yaml: %snode %q missing uses or graph", at(m.where), n.ID)
		}
	}
	return nil
}

// addSubNode 装一格 `graph:` 节点：边界先由 `in` / `out` 翻成绑定，再把这一处的
// 子图（或它的工厂）挂上去。
func (a *assembler) addSubNode(g *pulse.Graph, n NodeSpec, m mount, aspects []pulse.Aspect) error {
	spec, ok := a.doc.Graphs[n.Graph]
	if !ok {
		return fmt.Errorf("pulse/yaml: %snode %q references unknown graph %q", at(m.where), n.ID, n.Graph)
	}
	// `retry:` / `timeout:` 落在子图节点上：由调用方按 `nodeAspects` 一起传进来
	// （先列的更靠外 → Timeout 在外、Retry 在内），这里不再单独判。
	//
	// 阶段 1 / 2 这里曾经**当场拒** `retry:`——那时缺一次尝试一份写暂存的机制，重试会让
	// 第一次已经桥回父侧的输出被幂等首写静默吞掉。阶段 3 把 attempt 写集补上之后，那条
	// 限制的理由整个消失，于是**照装**：Go 侧与 YAML 侧同一个语义、同一处判据（在引擎
	// 最内层的推进闸与 `Retry` 的写暂存上），不再有两套口径。
	binds, err := subBinds(n, n.Graph, spec, a.reg)
	if err != nil {
		// subBinds 的报错自己就带 `node %q:` 那句，这里只补「哪张 spec」——
		// 再加一层 node id 会打成 `node "x": node "x": …`。
		return fmt.Errorf("pulse/yaml: %s%w", at(m.where), err)
	}

	// 装配期就有一张现成的子图（`pulse.Sub`）：父图在运行之前递归看得见整个子树，
	// 子图的静态毛病也在 `Load` 那一刻报出来。
	cm := m.child(n)
	// 子图是定义，不创建 ctx；Sub 在每次运行 / 尝试时建子执行，ctx 从挂载点的
	// 节点执行派生（见 runStaticSub），取消只影响这一次运行。
	child, err := a.buildChild(n, cm)
	if err != nil {
		return err
	}
	// 顺序：接线形状的毛病先报（它们更贴使用者刚写下的那一行，也不必先钻进子图里
	// 找），再让这一张子图过静态判据。
	if err := pulse.Sub(g, n.ID, n.Description, binds, child, aspects...); err != nil {
		return fmt.Errorf("pulse/yaml: %s%w", at(m.where), err)
	}
	// 校验树这一遍：这一张子图实例**连同它的子树**当场过一遍静态判据（`Validate`
	// 与 `Start` 共用同一套判据，所以报文与它跑到那一层被拒时逐字相同）。
	//
	// 为什么是「每张实例各自校验」而不是「最后校验一次根图」：根图那一层**不能**
	// 在这一刻查——它的外部输入是宿主在 `Load` 之后种进去的（`SeedPlan.Inputs`），
	// 装图期看它当然是「没人提供、没人 seed」，任何一份正常文档都会被误报。
	// 「声明归端口、值归运行输入」是阶段 2 随 `Execution` 落地的事，所以根层自己那
	// 几条判据仍在 `Start` 那一刻报（与今天一致）。子图这一层没有这个问题：它们的
	// 输入面就是引用点的 `in:`，装配期已经声明成端口了（declarePorts）。
	//
	// 报文带上**绝对挂载路径**：同一张 spec 挂两处，两处看到的含义不同，照着路径
	// 才找得到该改的那一处。子图里再嵌一层时，内层先报（内层在装配时就校验过了，
	// 错误一路往上返回，不再包第二层）。
	if a.check {
		if err := child.Validate(); err != nil {
			return fmt.Errorf("pulse/yaml: graph %q%s: %w", n.Graph, mountedAt(cm.path), err)
		}
	}
	return nil
}

// buildChild 装这一处的子图：建图 → 递归装它的节点 → 种它自己的 seeds → 按这一处的
// 接线**声明端口**。
//
// 校验树与真图共用它（同一段代码、同一批判据）。三件事在这里定：
//
//   - 定义不持有 ctx：每个挂载点在运行 / 尝试时各建子执行，取消域从那个节点的 ctx
//     派生（见 runStaticSub）；装配不创建执行、也不挂宿主的生命周期；
//   - 出口：`ObserverFor` 按这一层的**挂载路径**建（声明式装配里每一层是另一张图，
//     一个出口实例只能带一条 `pulse.path`，所以「按层各建一个」得由宿主决定）。
//     校验树不调它：它只装不跑，一个记录也不会产生；
//   - 图级选项：子图**不拿** `opts.Graph`——那批里能影响子图的三件（观察者 / 名额 /
//     图级切面）都靠**继承**往下走（见各自的 godoc），逐层再声明一遍就成了「每层各一份」
//     ——切面更糟，会一层叠一遍（两层两份、三层三份）。`Option` 是不透明闭包、没法挑着
//     传，所以这里只传「该按层不同」的那一条：出口。
func (a *assembler) buildChild(n NodeSpec, cm mount) (*pulse.Graph, error) {
	spec := a.doc.Graphs[n.Graph]

	var opts []pulse.Option
	if !a.check && a.opts.ObserverFor != nil {
		if o := a.opts.ObserverFor(cm.path); o != nil {
			opts = append(opts, pulse.WithObserver(o))
		}
	}
	var (
		child *pulse.Graph
		err   error
	)
	if a.check {
		// 校验图与真图**同额度**（理由见 checkGraph）；它同样带 opts.Graph 那批选项。
		child, err = checkGraph(cm.id, a.opts)
	} else {
		child, err = pulse.New(cm.id, spec.Description, opts...)
	}
	if err != nil {
		return nil, err
	}
	if err := a.addNodes(child, spec.Nodes, cm); err != nil {
		return nil, err
	}
	// 子图自己声明的 `seeds:` 不在这里落：值属于一次运行，所以它们变成了**挂载点上**的
	// `pulse.Fix` 绑定（见 subBinds）。这一层只按它们补上端口声明（见 declarePorts）。
	//
	// 值本身仍然在这里对账（键有没有登记、类型记不记得上、容器要不要 coerce）——
	// 由 `subBinds` 那两次查表 + `FixByName` 完成，报错点在节点那一行。
	if err := a.declarePorts(child, n, spec); err != nil {
		return nil, err
	}
	return child, nil
}

// declarePorts 按这一处的接线给子图**声明端口**：`in:` 的子侧键进输入端口、`out:` 的
// 子侧键进输出端口、**子图自己 `seeds:` 的键也进输入端口**。
//
// 为什么必须有：子图里那些节点读 `in:` 喂进来的键，而值要等运行期才送得进去——静态校验
// 子图时，这条键就是「没人提供、也不是入口」。端口把「这本来就是外部输入」说清楚
// （静态来源是 `pulse.SourceInput`），于是「忘了接线」与「本来就是这张图的接口」分得开；
// 端口同时也是**父侧唯一能接的接线面**（`Sub` 只认声明过的端口，接到子图内部的中间 Key
// 是静态错）。
//
// `seeds:` 那几条同理：它们的值来自挂载点上的 `Fix` 绑定，对子图而言仍然是「外部输入」。
//
// 按**引用点**各声明各的：同一张 spec 挂两处、两边的 `in:` 不一样时，两张实例的接口面
// 本来就不同——这正是「按引用点校验」（#298）在静态装配里的落点。
func (a *assembler) declarePorts(child *pulse.Graph, n NodeSpec, spec GraphSpec) error {
	in, err := a.ports(n.ID, n.In, "in")
	if err != nil {
		return err
	}
	seeded, err := a.seedPorts(n.ID, spec)
	if err != nil {
		return err
	}
	if err := child.DeclareInput(append(in, seeded...)...); err != nil {
		return fmt.Errorf("pulse/yaml: node %q in: %w", n.ID, err)
	}
	out, err := a.ports(n.ID, n.Out, "out")
	if err != nil {
		return err
	}
	if err := child.DeclareOutput(out...); err != nil {
		return fmt.Errorf("pulse/yaml: node %q out: %w", n.ID, err)
	}
	return nil
}

// seedPorts 把子图 `seeds:` 里那几条键翻成端口声明（顺序按键名排序，与 Fix 绑定同序）。
func (a *assembler) seedPorts(nodeID string, spec GraphSpec) ([]pulse.Port, error) {
	out := make([]pulse.Port, 0, len(spec.Seeds))
	for _, s := range sortedSeeds(spec.Seeds) {
		tag, ok := a.reg.TypeTagOf(s.Key.Name)
		if !ok {
			return nil, fmt.Errorf("pulse/yaml: node %q seed: key %q is not registered", nodeID, s.Key.Name)
		}
		ref, err := a.reg.ResolveKey(s.Key.Name, tag)
		if err != nil {
			return nil, fmt.Errorf("pulse/yaml: node %q seed: %w", nodeID, err)
		}
		out = append(out, pulse.PortRef(ref))
	}
	return out, nil
}

// ports 把一侧接线里的**子侧**键翻成端口声明，顺序按键名排序（声明顺序确定，
// 校验与报文才稳定）。
//
// 空集返回空切片：`DeclareInput()` 本身把零个端口当无事发生，所以两侧都可以无条件调。
func (a *assembler) ports(nodeID string, keys map[string]string, side string) ([]pulse.Port, error) {
	out := make([]pulse.Port, 0, len(keys))
	for _, name := range sortedMapKeys(keys) {
		tag, ok := a.reg.TypeTagOf(name)
		if !ok {
			return nil, fmt.Errorf("pulse/yaml: node %q %s: key %q is not registered", nodeID, side, name)
		}
		ref, err := a.reg.ResolveKey(name, tag)
		if err != nil {
			return nil, fmt.Errorf("pulse/yaml: node %q %s: %w", nodeID, side, err)
		}
		out = append(out, pulse.PortRef(ref))
	}
	return out, nil
}

// declareSpecInputs 给「谁也引用不到」的那张 spec 补上它**可能**的接口面：把解析不到来源的
// 每条 `Requires` 声明成入口，好让那张只装不跑的校验图过得了来源那条判据（见 Load 里的说明：
// 没有挂载点就没有接口面，来源判据在这张图上本来就不成立）。
//
// 判据不在 yaml 这边重写：谁是「没有来源」由引擎自己的出口给——`Topology()` 里
// `Requires[].From` 就是 `sourceOfLocked` 写出来的那个来源，`SourceNone` 即没有来源。所以
// 这里既不必分节点类型（工厂节点 / 子图节点一视同仁），也不重算一遍来源表；名与类型都从
// 那条键自己来（`KeyShape.Type` 就是登记表的类型记号），形状与真被引用时由 `in:` 声明的一致。
//
// 收集顺序是确定的（节点声明序 × Requires 顺序 × 首次出现），所以端口表与报文顺序都稳定。
func (a *assembler) declareSpecInputs(g *pulse.Graph, name string) error {
	var ports []pulse.Port
	seen := make(map[string]struct{})
	for _, ns := range g.Topology().Nodes {
		for _, ks := range ns.Requires {
			if ks.From.Kind != pulse.SourceNone {
				continue
			}
			if _, dup := seen[ks.Name]; dup {
				continue
			}
			seen[ks.Name] = struct{}{}
			ref, err := a.reg.ResolveKey(ks.Name, ks.Type)
			if err != nil {
				return fmt.Errorf("%s%w", at(name), err)
			}
			ports = append(ports, pulse.PortRef(ref))
		}
	}
	if err := g.DeclareInput(ports...); err != nil {
		return fmt.Errorf("%s%w", at(name), err)
	}
	return nil
}

// seedOne 把子图声明里的一条 seed 在**装图期**对一次账：键有没有登记、类型记号对不对、
// 值能不能对齐到那个类型、是不是 nil。
//
// 它**不碰任何图**（值属于一次运行，见 subBinds 那段）：真正把值送到子图那一步的是
// 挂载点上的 `pulse.Fix` 绑定。这里只留「这条 seed 站不站得住」这一半，供**谁也引用不到**
// 的那些 spec 用——那种 spec 没有挂载点，`subBinds` 没有机会跑，而「写错了没人报」
// 比「报了」难查得多。
func seedOne(reg *pulse.Registry, graphName string, s SeedSpec) error {
	var in pulse.Inputs
	if s.Skip {
		if err := pulse.SkipSeedByName(&in, reg, s.Key.Name, s.Key.Type); err != nil {
			return fmt.Errorf("pulse/yaml: graph %q seed %q: %w", graphName, s.Key.Name, err)
		}
		return nil
	}
	if err := pulse.SeedByName(&in, reg, s.Key.Name, s.Key.Type, s.From.Value); err != nil {
		return fmt.Errorf("pulse/yaml: graph %q seed %q: %w", graphName, s.Key.Name, err)
	}
	return nil
}

// checkSubSeeds 在 Load 期拦掉子图 seed 本身的毛病（C3 + 重复声明）：
//
//   - **非 skip** 的 seed 要靠宿主 IO（`env` / `file` / `context`）——子图的
//     `SeedPlan` 不存在，只有 `literal` 能种。`skip: true` 那条不算：它只把
//     槽标成跳过，`from` 没有语义（`seedOne` 与顶层 `SeedPlan.Inputs` 都是
//     直接 `SkipSeed` 并忽略 `from`），所以照顶层的样子放行；
//   - 同一张图里同一条键声明**两次** seed——第二次种值会被幂等首写静默忽略，
//     到底哪个值生效取决于声明顺序，两行看着都像是对的。
//
// 键名 / 类型 / 值能不能对上登记表，由装配期的校验树**种一遍**来判
// （seedOne 与真图同一个函数）。
func checkSubSeeds(graphs map[string]GraphSpec) error {
	for _, name := range sortedNames(graphs) {
		seen := make(map[string]bool, len(graphs[name].Seeds))
		for _, s := range graphs[name].Seeds {
			if !s.Skip {
				switch s.From.Kind {
				case "", "literal":
				default:
					return fmt.Errorf("pulse/yaml: graph %q seed %q: kind %q is not supported in a subgraph "+
						"(only literal: env/file/context need host IO, and SeedPlan belongs to the parent graph)",
						name, s.Key.Name, s.From.Kind)
				}
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

// referencedGraphs 是「至少有一处引用了它」的 spec 名（根与各 spec 的 `graph:`
// 都算）。谁的引用都没有的那些要单独校验：它们是死配置，但「写错了没人报」比「报了」
// 难查得多。
func referencedGraphs(doc Document) map[string]bool {
	out := make(map[string]bool, len(doc.Graphs))
	add := func(specs []NodeSpec) {
		for _, n := range specs {
			if n.Graph != "" {
				out[n.Graph] = true
			}
		}
	}
	add(doc.Nodes)
	for _, name := range sortedNames(doc.Graphs) {
		add(doc.Graphs[name].Nodes)
	}
	return out
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
