package yaml

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Luo-root/pulse"
	goyaml "gopkg.in/yaml.v3"
)

// Document 是声明式流程图的解码形态。
type Document struct {
	// Version 缺省或 1 接受；其它值拒绝。
	Version int `yaml:"version"`
	// Description 是人可读的说明，落到**根图**的 `Description()`（`""` = 没写）。
	// 它不参与任何语义、校验与拓扑结果，只出现在 `Topology()` / `Snapshot()` 这类
	// 给人看的出口上。子图各写各的（`graphs.<名>.description`）。
	Description string `yaml:"description"`
	// Graphs 是可复用子图（名字在文档内唯一）：`nodes[].graph` 指向它。
	// 子图引用的是这张**扁平**的表，递归靠「图引用图」表达，环在 Load 期拦。
	Graphs map[string]GraphSpec `yaml:"graphs"`
	Seeds  []SeedSpec           `yaml:"seeds"`
	Nodes  []NodeSpec           `yaml:"nodes"`
	// Observer 仅文档提示位：Load 忽略。观察者走 LoadOptions.Graph / ObserverFor。
	Observer string `yaml:"observer"`
}

// KeySpec 是 YAML 中的 {name, type}。
type KeySpec struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
}

// SeedFrom 描述 Seed 引用（宿主执行 IO）。
type SeedFrom struct {
	Kind  string `yaml:"kind"` // literal | env | file | context
	Value any    `yaml:"value,omitempty"`
	Env   string `yaml:"env,omitempty"`
	Path  string `yaml:"path,omitempty"`
	Key   string `yaml:"key,omitempty"`
}

// SeedSpec 一条 Seed / SkipSeed 计划。
type SeedSpec struct {
	Key  KeySpec  `yaml:"key"`
	From SeedFrom `yaml:"from"`
	Skip bool     `yaml:"skip"`
}

// RetrySpec 内建 Retry。
type RetrySpec struct {
	Attempts int           `yaml:"attempts"`
	Delay    time.Duration `yaml:"delay"`
}

// NodeSpec 一个声明式节点（拓扑归 YAML）。
//
// 两种形态二选一：**工厂节点**（`uses`：Run 来自注册表）或**子图节点**
// （`graph`：这张图就是这一步的拓扑，边界由 `in` / `out` 声明）。
type NodeSpec struct {
	ID string `yaml:"id"`
	// Description 落到这个节点（子图节点则落在**父侧那个挂载节点**上）的
	// `Description()`；`""` = 没写。不参与任何语义与校验。
	Description string `yaml:"description"`
	// Uses 是注册表里的工厂名（工厂节点）。
	Uses string `yaml:"uses"`
	// Graph 是 Document.Graphs 里的子图名（子图节点）：这个节点 = 装一张子图。
	Graph string `yaml:"graph"`
	// In 是父 → 子的绑定，一律 `子键: 父键`（子图要的键 ← 父图的键）。
	In map[string]string `yaml:"in"`
	// Out 是子 → 父的绑定，同样 `子键: 父键`（子图产出的键 → 父图的键）。
	Out      map[string]string `yaml:"out"`
	Requires []KeySpec         `yaml:"requires"`
	Provides []KeySpec         `yaml:"provides"`
	Timeout  time.Duration     `yaml:"timeout"`
	Retry    *RetrySpec        `yaml:"retry"`
}

// SeedPlanEntry 是装图返回给宿主的 Seed 计划项。
type SeedPlanEntry struct {
	Name string
	Type string
	Skip bool
	From SeedFrom
}

// SeedPlan 装图产物：宿主按条目执行 Seed/SkipSeed。
type SeedPlan struct {
	Entries []SeedPlanEntry
	reg     *pulse.Registry
}

// Apply 由宿主调用。resolve 负责 literal 以外的取值。
func (p *SeedPlan) Apply(g *pulse.Graph, resolve func(SeedFrom) (any, error)) error {
	if p == nil {
		return nil
	}
	for _, e := range p.Entries {
		if e.Skip {
			if err := pulse.SkipSeedByName(g, p.reg, e.Name, e.Type); err != nil {
				return err
			}
			continue
		}
		var v any
		switch e.From.Kind {
		case "literal", "":
			v = e.From.Value
		default:
			if resolve == nil {
				return fmt.Errorf("pulse/yaml: seed kind %q needs resolve func", e.From.Kind)
			}
			got, err := resolve(e.From)
			if err != nil {
				return err
			}
			v = got
		}
		if err := pulse.SeedByName(g, p.reg, e.Name, e.Type, v); err != nil {
			return err
		}
	}
	return nil
}

// LoadOptions 装图选项。GraphID 必填（图身份，空串 Load 报错）——
// 观测记录靠它区分「运行的是哪张图」。
type LoadOptions struct {
	// Context 是**根图**的 ctx：宿主取消它 = 取消这次运行（子图跟着停）。
	//
	// 子图**定义不挂**宿主的 ctx：静态挂载的子图是装配期建好的定义，取消是运行期从
	// **挂载点**转发进去的（`pulse.Sub` 把父节点这一层的取消转发进子图，见
	// runStaticSub）；挂在宿主 ctx 上只会让每一次 `Load` 都在宿主 ctx 上多挂几层
	// （热重载反复装图 = 越攒越多）。`""` = `context.Background()`。
	Context context.Context
	GraphID string
	// Graph 是**根图**的选项（WithObserver / WithAspects / WithMaxRunning…）。
	// 子图**不再拿这批**——三个图级选项都靠**继承**往下走（观察者、名额、图级切面），
	// 所以「挂在这里的观察者会到每一层」这件事仍然成立，只是走的是继承而不是逐层声明。
	//
	// 之所以不再逐层传：`Option` 是不透明闭包，没法挑着传；而逐层再声明一遍会把
	// **图级切面**叠成每层一份（两层两份、三层三份，见 pulse.WithAspects）。
	//
	// **别在这里放 pulse.WithMaxRunning**：额度只走下面的 MaxRunning 字段——两个都
	// 写时按 MaxRunning 那段写的规则合（`>0` 才压过这一份，`<=0` 不动它）。
	Graph []pulse.Option
	// MaxRunning 是**整棵声明树**的并发额度（<=0 = 无限，默认）：根图拿它建名额，
	// 子图在运行期**继承同一份**（引擎的默认继承，挂子图那一步自己不吃名额），
	// 所以这里写的是「整棵树同时最多几个节点在干活」，不是「每层各几个」。
	//
	// 与 `Graph` 里可能写的 `WithMaxRunning` 的关系（**只在 `>0` 时**它才拼在那批的
	// 最后、压过那一份）：`int` 的零值分不出「没写」与「写了 0」，所以
	// `MaxRunning: 0`（含默认）**不会**把 `Graph` 里那份改回「不限」——想让整棵声明树
	// 不限，就别在 `Graph` 里放 `WithMaxRunning`。七档组合由包内用例
	// `TestLoadOptionsQuotaPrecedence` 钉住；推荐把额度只写在 `MaxRunning` 上，
	// `Graph` 留给观察者与切面。
	MaxRunning int
	// ObserverFor 按层建出口：声明式装配里每一层是另一张图，而一个出口实例
	// 只能带一条 `pulse.path`，所以「按层各建一个」得由宿主决定。path 是这一层的
	// 路径（一层子图是它的节点 id，再深一层是 `outer/inner`），宿主拿它填
	// observe.ObserveConfig.Path 就能把层级写进记录。
	//
	// **调用时机**：子图在 `Load` 装到那一层时各调一次（每处挂载一次——同一张 spec
	// 挂两处就是两个出口实例，path 各是各自的挂载路径）。只装不跑的校验树**不调**它：
	// 宿主收到的回调次数不会因为「多校验一遍」而变。
	//
	// 返回 nil = 这一层按继承走（子图会继承父图的观察者）。
	ObserverFor func(path string) pulse.Observer
}

// checkGraph 起一张**只装不跑**的校验图，并且带上与真图**同一份额度**。
//
// 额度必须一起给：`Start` / `Validate` 的流式判据按**有效容量**判
// （`problemsStreamLocked` 比的是容量与这一层同时活着的流节点数），校验图没有额度时那道
// 检查形同虚设——于是「额度养不起这条流」会装图全过、跑到那一层 `Start` 才爆。校验图
// 与真图的选项必须是**同一次推导**出来的（与函数末尾那份 `rootOpts` 一致），两处各写
// 一遍就会漂。
//
// 今天 YAML 还表达不了流式节点（`Produce` / `Consume` / `Tee` 是 Go 糖，跑在工厂里面），
// 所以这条差异在公开面上暂时不可观测；它是给「YAML 也能声明流」那天留的闸门，也顺手
// 钉住「校验图 = 真图的选项」这件事（包内用例 `TestCheckGraphCarriesCapacity`）。
func checkGraph(id string, opts LoadOptions) (*pulse.Graph, error) {
	o := append([]pulse.Option(nil), opts.Graph...)
	if opts.MaxRunning > 0 {
		o = append(o, pulse.WithMaxRunning(opts.MaxRunning))
	}
	// 描述给空串：校验图只装不跑，装完就丢，任何出口都不会拿它去见人。
	return pulse.New(context.Background(), id, "", o...)
}

// Load 把 YAML 文档装成 Graph + SeedPlan。
func Load(data []byte, reg *pulse.Registry, opts LoadOptions) (*pulse.Graph, *SeedPlan, error) {
	if reg == nil {
		return nil, nil, fmt.Errorf("pulse/yaml: nil registry")
	}
	var doc Document
	if err := goyaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("pulse/yaml: decode: %w", err)
	}
	if doc.Version != 0 && doc.Version != 1 {
		return nil, nil, fmt.Errorf("pulse/yaml: unsupported version %d (want 0 or 1)", doc.Version)
	}
	if len(doc.Nodes) == 0 {
		return nil, nil, fmt.Errorf("pulse/yaml: document has no nodes")
	}
	if opts.GraphID == "" {
		return nil, nil, fmt.Errorf("pulse/yaml: graph id is required (LoadOptions.GraphID, observe instance identity)")
	}
	_ = doc.Observer // 明确忽略；宿主用 LoadOptions.Graph / ObserverFor 挂 Observer

	// 子图先做静态校验：引用、环、seed 取值方式三类都是「装不出图」或
	// 「跑到一半才炸」的病，而都能在这一刻判定（C4 / C5）。
	if err := checkGraphRefs(doc.Graphs); err != nil {
		return nil, nil, err
	}
	if err := checkSubSeeds(doc.Graphs); err != nil {
		return nil, nil, err
	}
	referenced := referencedGraphs(doc)
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}

	// 装图分两遍，判据只写一份：先装一棵**只装不跑的校验树**（子图一律静态挂、
	// 整棵建在 `context.Background()` 上、不建出口），等它整棵过完，再用同一段装配
	// 代码装真图。
	//
	// 为什么是这个顺序：真图要从**宿主 ctx** 派生，而 `Load` 失败时返回值只有
	// `(nil, nil, err)`——调用方连取消它的把手都没有，同一份坏文档反复加载
	// （热重载）就会在宿主 ctx 上越攒越多。所以「所有可能失败的校验」必须排在
	// 「动宿主 ctx 建根图」之前，而校验树整棵都不碰宿主的那份 ctx，可以在前面随便建。
	//
	// 校验树里**每个引用点各是一张实例**：同一张 spec 挂两处，两边的 `in:` 可以不一样，
	// 而「子图里这条键有没有来源」正是按引用点成立的——有一处没喂，那张图跑到就一定会挂。
	// 按并集校验会把这种图放过去（#298 的口径）。每张实例的子图端口也按**那一处**的
	// `in:` / `out:` 声明（见 assembler.declarePorts），所以两处看到的接口面本来就是
	// 两份，报错点的是各自那条挂载路径（`step1/inner`）。
	a := &assembler{doc: doc, reg: reg, opts: opts, check: true}
	checkRoot, err := checkGraph(opts.GraphID, opts)
	if err != nil {
		return nil, nil, err
	}
	// 这一遍装的是**根层**的装配：根侧那些接线处的毛病（绑了一条没登记的键、`in` 喂的键
	// 子图根本不读、工厂节点写了 `in` / `out`……）由 `subBinds` / `Sub` / `Add` 点着节点
	// id 报出来。子图那一层在它自己的 `buildChild` 里当场校验（连同子树）。
	//
	// 根图**自己**那几条静态判据（每个 `Requires` 有来源 / 依赖无环 / 流式）不在这里查：
	// 根层的外部输入是宿主在 `Load` 之后种进去的（`SeedPlan.Apply`），装图期看它当然是
	// 「没人提供」——任何一份 `seeds:` 正常的文档都会被误报。「声明归端口、值归运行输入」
	// 是阶段 2 随 `Execution` 落地的事，所以根层那几条仍在 `Start` 那一刻报（与今天一致）。
	if err := a.addNodes(checkRoot, doc.Nodes, mount{id: opts.GraphID}); err != nil {
		return nil, nil, err
	}
	// 谁也引用不到的 spec：它**没有挂载点**，因此**没有接口面**——里面那些「没人提供」的键
	// 究竟是「等将来挂上去时由那一处的 `in:` 喂」还是「名字写错了」，装配期判不了。按闭合图
	// 去校验会把前者一律报成后者（正常的模板库只要有一张暂时没接上，整份文档就 `Load` 不过）。
	//
	// 所以这里只留**真判得了**的那几条：工厂名 / 重复 id / 来源冲突 / 依赖成环 / 流式那批
	// 照旧，**来源那条不作数**——它的接口面按「读到的都当入口」补上（declareSpecInputs，
	// 补在一张只装不跑的校验图上）。等它被引用时，接口面由**那一处**的 `in:` / `out:` 定，
	// 来源判据在那时按引用点生效（见 addSubNode 与 #298 的口径）。
	for _, name := range sortedNames(doc.Graphs) {
		if referenced[name] {
			continue
		}
		spec := doc.Graphs[name]
		g, err := checkGraph(name, opts)
		if err != nil {
			return nil, nil, err
		}
		if err := a.addNodes(g, spec.Nodes, mount{id: name, where: name}); err != nil {
			return nil, nil, err
		}
		for _, s := range spec.Seeds {
			if err := seedOne(g, name, s, reg); err != nil {
				return nil, nil, err
			}
		}
		if err := a.declareSpecInputs(g, name); err != nil {
			return nil, nil, err
		}
		if err := g.Validate(); err != nil {
			return nil, nil, fmt.Errorf("pulse/yaml: graph %q: %w", name, err)
		}
	}

	// 宿主的 seeds 也先在装图之前对完账：这条检查会失败，而它原来排在根图
	// 后面——失败返回 `nil, nil, err` 时，那张刚建好的根图连同它派生出去的
	// ctx 就没人认领了（同一份坏文档反复加载 = 宿主 ctx 越挂越多）。
	plan := &SeedPlan{reg: reg}
	for i, s := range doc.Seeds {
		if _, err := reg.ResolveKey(s.Key.Name, s.Key.Type); err != nil {
			return nil, nil, fmt.Errorf("pulse/yaml: seeds[%d]: %w", i, err)
		}
		plan.Entries = append(plan.Entries, SeedPlanEntry{
			Name: s.Key.Name,
			Type: s.Key.Type,
			Skip: s.Skip,
			From: s.From,
		})
	}

	// 所有**可能失败**的校验都过了，才动宿主的 ctx：从这里往下一路成功，
	// `Load` 要么把图交出去（调用方接管它的生死），要么在 `New` 这一步就
	// 因为 graphID 空拒掉（那个检查在函数开头做过）。
	//
	// 真图用的是**同一段**装配代码（`a.check` 关掉即可）：装的形状、接线检查、
	// 端口声明与校验树逐字一致。两遍真正的差别只有两处，都写在这里免得再推一遍：
	//
	//  1. **根图那份 ctx**：校验树的根图建在 `context.Background()` 上（见 checkGraph），
	//     真图的根图继承宿主 ctx。子图**两边都不挂**宿主 ctx——静态挂载的子图定义没有要
	//     取消的东西，取消是运行期从挂载点转发进去的（见 runStaticSub），所以 buildChild
	//     两遍都用 `context.Background()`；
	//  2. **出口**：只有真图按层建 `ObserverFor`（见 buildChild），校验树一个记录都不产生。
	//
	// 另外校验树那遍会逐张挂载点调 `child.Validate()`（见 addSubNode），真图那遍不调——
	// 校验树已经把整棵子树判过并**封口**了（Validate 封的是整棵子树），
	// 而 `Load` 交出去的真图要留给宿主 `Seed` / `Start`（那两者不受封口影响）。
	a.check = false
	rootOpts := append([]pulse.Option(nil), opts.Graph...)
	if opts.MaxRunning > 0 {
		rootOpts = append(rootOpts, pulse.WithMaxRunning(opts.MaxRunning)) // 放最后：整棵树的额度压过 Graph 里的每层一份
	}
	g, err := pulse.New(ctx, opts.GraphID, doc.Description, rootOpts...)
	if err != nil {
		return nil, nil, err
	}
	if err := a.addNodes(g, doc.Nodes, mount{id: opts.GraphID}); err != nil {
		return nil, nil, err
	}
	return g, plan, nil
}

// at 给一条装图期报错标出「这是哪张 spec 里的」：根层为空串（不写），子图写
// `graph "name": `。
func at(where string) string {
	if where == "" {
		return ""
	}
	return fmt.Sprintf("graph %q: ", where)
}

// nodeAspects 是节点切面：先列的更靠外 → Timeout 在外、Retry 在内。
// 子图节点同样适用——那里的 `timeout: 30s` 意思是「整张子图限时」。
func nodeAspects(n NodeSpec) []pulse.Aspect {
	var aspects []pulse.Aspect
	if n.Timeout > 0 {
		aspects = append(aspects, pulse.Timeout(n.Timeout))
	}
	if n.Retry != nil {
		aspects = append(aspects, pulse.Retry(n.Retry.Attempts, n.Retry.Delay))
	}
	return aspects
}

// LoadFile 读路径再 Load。
func LoadFile(path string, reg *pulse.Registry, opts LoadOptions) (*pulse.Graph, *SeedPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return Load(data, reg, opts)
}

func toNameTypes(specs []KeySpec) []pulse.NameType {
	out := make([]pulse.NameType, len(specs))
	for i, s := range specs {
		out[i] = pulse.NameType{Name: s.Name, Type: s.Type}
	}
	return out
}
