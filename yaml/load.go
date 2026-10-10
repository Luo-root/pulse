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
	Context context.Context
	GraphID string
	// Graph 是根图的选项（WithObserver / WithMaxRunning / WithAspects…）。
	// 子图也拿同一批选项 —— 所以挂在这里的观察者会到每一层。
	Graph []pulse.Option
	// ObserverFor 按层建出口：声明式装配里每一层是另一张图，而一个出口实例
	// 只能带一条 `pulse.path`，所以「按层各建一个」得由宿主决定。path 是这一层
	// 的路径（引擎给：一层子图是它的节点 id，再深一层是 `outer/inner`），
	// 宿主拿它填 observe.ObserveConfig.Path 就能把层级写进记录。
	//
	// 返回 nil = 这一层按 Graph 里挂的那条走（子图会继承父图的观察者）。
	ObserverFor func(path string) pulse.Observer
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
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}

	// 每一张 spec 的**节点**也在这一刻查完。落进图的只有根层那批，子图要等运行到
	// 才装——只查根层的话，第 2 层往后写错工厂名或绑定的毛病会「装图全过、跑到
	// 一半才炸」，谁也引用不到的 spec 更是永远不炸。
	//
	// 做法是给每张 spec 起一个**只装不跑**的空图，把同一段装图代码走一遍：`Sub` /
	// `Add` 在装配期做的检查（id、重复、来源、绑定）一个不少，子图却不会被真的
	// 建起来（build 只是被存进节点，没有人调用它）。
	for _, name := range sortedNames(doc.Graphs) {
		check, err := pulse.New(ctx, "load-check:"+name)
		if err != nil {
			return nil, nil, err
		}
		if err := addNodes(check, doc.Graphs[name].Nodes, name, doc.Graphs, reg, opts); err != nil {
			return nil, nil, err
		}
	}

	g, err := pulse.New(ctx, opts.GraphID, opts.Graph...)
	if err != nil {
		return nil, nil, err
	}
	if err := addNodes(g, doc.Nodes, "", doc.Graphs, reg, opts); err != nil {
		return nil, nil, err
	}

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
	return g, plan, nil
}

// addNodes 把一批节点声明装进 g。工厂节点与子图节点在这里分流：子图节点被
// 翻成一个 `pulse.Sub`——父侧的 Requires / Provides 由 `in` / `out` 推出来，
// 子图本身交给 build 工厂在**每次运行时**新装一张（一次性契约天然满足）。
//
// 子图里的子图走的是同一个函数（`buildSub` 递归调它），所以嵌套不设深度上限；
// 引用环已经在 `checkGraphRefs` 拦掉了，这里不会转圈。
//
// `where` 是这批声明属于哪张子图（根层传空串），只用于把报错点到位——子图里的
// 毛病不写清是哪一张，读的人还得自己找。
func addNodes(g *pulse.Graph, specs []NodeSpec, where string,
	graphs map[string]GraphSpec, reg *pulse.Registry, opts LoadOptions) error {

	for i, n := range specs {
		if n.ID == "" {
			return fmt.Errorf("pulse/yaml: %snodes[%d] missing id", at(where), i)
		}
		aspects := nodeAspects(n)
		switch {
		case n.Graph != "" && n.Uses != "":
			return fmt.Errorf("pulse/yaml: %snode %q: uses and graph are mutually exclusive", at(where), n.ID)

		case n.Graph != "":
			spec, ok := graphs[n.Graph]
			if !ok {
				return fmt.Errorf("pulse/yaml: %snode %q references unknown graph %q", at(where), n.ID, n.Graph)
			}
			binds, err := subBinds(n, n.Graph, spec, reg)
			if err != nil {
				// subBinds 的报错自己就带 `node %q:` 那句，这里只补「哪张 spec」——
				// 再加一层 node id 会打成 `node "x": node "x": …`。
				return fmt.Errorf("pulse/yaml: %s%w", at(where), err)
			}
			graphName := n.Graph
			err = pulse.Sub(g, n.ID, binds, func(sc *pulse.SubCtx) (*pulse.Graph, error) {
				return buildSub(sc, graphName, spec, graphs, reg, opts)
			}, aspects...)
			if err != nil {
				// `pulse.Sub` 的报错也都点着节点 id，不用再补一层。
				return fmt.Errorf("pulse/yaml: %s%w", at(where), err)
			}

		case n.Uses != "":
			run, ok := reg.Lookup(n.Uses)
			if !ok {
				return fmt.Errorf("pulse/yaml: %snode %q uses unknown factory %q", at(where), n.ID, n.Uses)
			}
			requires, err := reg.KeyRefs(toNameTypes(n.Requires))
			if err != nil {
				return fmt.Errorf("pulse/yaml: %snode %q requires: %w", at(where), n.ID, err)
			}
			provides, err := reg.KeyRefs(toNameTypes(n.Provides))
			if err != nil {
				return fmt.Errorf("pulse/yaml: %snode %q provides: %w", at(where), n.ID, err)
			}
			if err := g.Add(pulse.NewNode(n.ID, requires, provides, run, aspects...)); err != nil {
				return fmt.Errorf("pulse/yaml: %snode %q: %w", at(where), n.ID, err)
			}

		default:
			return fmt.Errorf("pulse/yaml: %snode %q missing uses or graph", at(where), n.ID)
		}
	}
	return nil
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
