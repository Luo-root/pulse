package yaml

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Luo-root/pulse/kernel/flow"
	goyaml "gopkg.in/yaml.v3"
)

// Document 是声明式流程图的解码形态。
type Document struct {
	// Version 缺省或 1 接受；其它值拒绝。
	Version int `yaml:"version"`
	Seeds   []SeedSpec `yaml:"seeds"`
	Nodes   []NodeSpec `yaml:"nodes"`
	// Observer 仅文档提示位：Load 忽略。观察者走 LoadOptions.Graph / WithObserver。
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
type NodeSpec struct {
	ID       string        `yaml:"id"`
	Uses     string        `yaml:"uses"`
	Requires []KeySpec     `yaml:"requires"`
	Provides []KeySpec     `yaml:"provides"`
	Timeout  time.Duration `yaml:"timeout"`
	Retry    *RetrySpec    `yaml:"retry"`
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
	reg     *flow.Registry
}

// Apply 由宿主调用。resolve 负责 literal 以外的取值。
func (p *SeedPlan) Apply(g *flow.Graph, resolve func(SeedFrom) (any, error)) error {
	if p == nil {
		return nil
	}
	for _, e := range p.Entries {
		if e.Skip {
			if err := flow.SkipSeedByName(g, p.reg, e.Name, e.Type); err != nil {
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
				return fmt.Errorf("flow/yaml: seed kind %q needs resolve func", e.From.Kind)
			}
			got, err := resolve(e.From)
			if err != nil {
				return err
			}
			v = got
		}
		if err := flow.SeedByName(g, p.reg, e.Name, e.Type, v); err != nil {
			return err
		}
	}
	return nil
}

// LoadOptions 装图选项。
type LoadOptions struct {
	Context context.Context
	Graph   []flow.Option
}

// Load 把 YAML 文档装成 Graph + SeedPlan。
func Load(data []byte, reg *flow.Registry, opts LoadOptions) (*flow.Graph, *SeedPlan, error) {
	if reg == nil {
		return nil, nil, fmt.Errorf("flow/yaml: nil registry")
	}
	var doc Document
	if err := goyaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("flow/yaml: decode: %w", err)
	}
	if doc.Version != 0 && doc.Version != 1 {
		return nil, nil, fmt.Errorf("flow/yaml: unsupported version %d (want 0 or 1)", doc.Version)
	}
	if len(doc.Nodes) == 0 {
		return nil, nil, fmt.Errorf("flow/yaml: document has no nodes")
	}
	_ = doc.Observer // 明确忽略；宿主用 LoadOptions.Graph 挂 Observer

	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	g := flow.New(ctx, opts.Graph...)

	for i, n := range doc.Nodes {
		if n.ID == "" {
			return nil, nil, fmt.Errorf("flow/yaml: nodes[%d] missing id", i)
		}
		if n.Uses == "" {
			return nil, nil, fmt.Errorf("flow/yaml: node %q missing uses", n.ID)
		}
		run, ok := reg.Lookup(n.Uses)
		if !ok {
			return nil, nil, fmt.Errorf("flow/yaml: node %q uses unknown factory %q", n.ID, n.Uses)
		}
		requires, err := reg.KeyRefs(toNameTypes(n.Requires))
		if err != nil {
			return nil, nil, fmt.Errorf("flow/yaml: node %q requires: %w", n.ID, err)
		}
		provides, err := reg.KeyRefs(toNameTypes(n.Provides))
		if err != nil {
			return nil, nil, fmt.Errorf("flow/yaml: node %q provides: %w", n.ID, err)
		}
		// NewNode 切面：先列的更靠外 → Timeout 在外、Retry 在内。
		var aspects []flow.Aspect
		if n.Timeout > 0 {
			aspects = append(aspects, flow.Timeout(n.Timeout))
		}
		if n.Retry != nil {
			aspects = append(aspects, flow.Retry(n.Retry.Attempts, n.Retry.Delay))
		}
		node := flow.NewNode(n.ID, requires, provides, run, aspects...)
		if err := g.Add(node); err != nil {
			return nil, nil, fmt.Errorf("flow/yaml: add node %q: %w", n.ID, err)
		}
	}

	plan := &SeedPlan{reg: reg}
	for i, s := range doc.Seeds {
		if _, err := reg.ResolveKey(s.Key.Name, s.Key.Type); err != nil {
			return nil, nil, fmt.Errorf("flow/yaml: seeds[%d]: %w", i, err)
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

// LoadFile 读路径再 Load。
func LoadFile(path string, reg *flow.Registry, opts LoadOptions) (*flow.Graph, *SeedPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return Load(data, reg, opts)
}

func toNameTypes(specs []KeySpec) []flow.NameType {
	out := make([]flow.NameType, len(specs))
	for i, s := range specs {
		out[i] = flow.NameType{Name: s.Name, Type: s.Type}
	}
	return out
}
