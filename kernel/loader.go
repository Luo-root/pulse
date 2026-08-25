package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// ConfigKey 是 Loader 向插件私有作用域注入条目配置的服务键。
// 插件在 Apply 中按需读取（不声明为依赖——没有配置也允许装载）：
//
//	cfg, _ := kernel.Get(c, kernel.ConfigKey)
var ConfigKey = NewServiceKey[map[string]any]("kernel.plugin.config")

// Entry 是 Loader 配置树中的一个条目：一个待装载插件的声明式描述。
// 整个系统的装配形态 = 条目列表，它是"系统加载了什么"的权威记录。
type Entry struct {
	// ID 是稳定标识，作为增量调和的 key。
	ID string `json:"id" yaml:"id"`
	// Name 是插件工厂名（须先经 Loader.Register 注册）。
	Name string `json:"name" yaml:"name"`
	// Disabled 为 true 时该条目保持卸载状态（保留配置便于恢复）。
	Disabled bool `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	// Config 传给插件：装载前被 Provide 到插件私有作用域的
	// ConfigKey 上。
	Config map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
}

// Factory 创建插件实例。每次装载调用一次，保证实例间零共享状态。
type Factory func() Plugin

// Loader 把声明式条目列表翻译成 Fiber 的增删改，并在条目变化时
// 做最小破坏的增量调和。
//
// 调和规则：
//   - 新增 ID => 装载；
//   - 移除 ID => 卸载并注销；
//   - Name 或 Config 变化 => 重建（旧实例整体回收、新实例全新装载；
//     Go 插件普遍轻状态，重建的正确性远比原地 diff 重要）；
//   - 仅 Disabled 翻转 => 卸载 / 恢复，不动其他字段。
type Loader struct {
	host *Context

	mu        sync.Mutex
	factories map[string]Factory
	fibers    map[string]*Fiber // ID -> 当前实例
	entries   map[string]*Entry // ID -> 最近一次应用的条目
}

// NewLoader 在 host 上创建装配器。条目产生的所有 Fiber 都挂在
// host 下，host 销毁即全部回收。
func NewLoader(host *Context) *Loader {
	return &Loader{
		host:      host,
		factories: make(map[string]Factory),
		fibers:    make(map[string]*Fiber),
		entries:   make(map[string]*Entry),
	}
}

// Register 登记插件工厂。同名重复注册返回错误——装配期冲突应当
// 尽早暴露，而不是静默覆盖。
func (l *Loader) Register(name string, f Factory) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.factories[name]; dup {
		return fmt.Errorf("kernel: plugin factory %q already registered", name)
	}
	l.factories[name] = f
	return nil
}

// MustRegister 同 Register，冲突时 panic。用于包级 init 装配断言。
func (l *Loader) MustRegister(name string, f Factory) {
	if err := l.Register(name, f); err != nil {
		panic(err)
	}
}

// Reconcile 将条目列表调和为期望的运行形态，返回聚合错误
// （单个条目的失败不阻断其余条目）。
func (l *Loader) Reconcile(entries []Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	want := make(map[string]*Entry, len(entries))
	for i := range entries {
		e := entries[i]
		if e.ID == "" {
			continue
		}
		want[e.ID] = &entries[i]
	}

	var errs []error

	// 移除消失的条目。
	for id, f := range l.fibers {
		if _, keep := want[id]; !keep {
			f.Close()
			delete(l.fibers, id)
			delete(l.entries, id)
		}
	}

	// 新增或重建变化的条目。
	for id, e := range want {
		cur := l.entries[id]
		if cur != nil && cur.Name == e.Name &&
			sameConfig(cur.Config, e.Config) && cur.Disabled == e.Disabled {
			continue // 无变化
		}

		if old := l.fibers[id]; old != nil {
			old.Close()
			delete(l.fibers, id)
		}

		if e.Disabled {
			l.entries[id] = cloneEntry(e)
			continue // 保留记录但不装载
		}

		factory, ok := l.factories[e.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("kernel: entry %q references unknown plugin %q", id, e.Name))
			l.entries[id] = cloneEntry(e)
			continue
		}

		f, err := l.mount(id, e.Config, factory)
		if err != nil {
			errs = append(errs, fmt.Errorf("kernel: entry %q (%s): %w", id, e.Name, err))
		} else {
			l.fibers[id] = f
		}
		l.entries[id] = cloneEntry(e)
	}

	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}

// mount 装载一个条目：把条目配置包装进插件（Apply 前注入其私有
// 作用域的 ConfigKey），然后同步 Use。
func (l *Loader) mount(id string, cfg map[string]any, factory Factory) (*Fiber, error) {
	_ = id // 预留：条目级诊断信息
	inner := factory()
	return Use(l.host, &configured{config: cfg, inner: inner})
}

// configured 在 inner 的 Apply 之前向其私有作用域提供条目配置。
type configured struct {
	config map[string]any
	inner  Plugin
}

func (p *configured) Inject() []Dependency { return p.inner.Inject() }

func (p *configured) Apply(c *Context) error {
	if p.config == nil {
		p.config = map[string]any{}
	}
	if _, err := Provide(c, ConfigKey, p.config); err != nil {
		return err
	}
	return p.inner.Apply(c)
}

// Fiber 返回某条目当前对应的实例；不存在返回 nil。
func (l *Loader) Fiber(id string) *Fiber {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fibers[id]
}

// Snapshot 返回 ID -> 状态 的快照，供诊断与测试。
func (l *Loader) Snapshot() map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]string, len(l.fibers))
	for id, f := range l.fibers {
		out[id] = f.State().String()
	}
	return out
}

// LoadFile 从 JSON 文件读取条目并调和。
func (l *Loader) LoadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return l.LoadJSON(data)
}

// LoadJSON 从字节流读取条目并调和。
func (l *Loader) LoadJSON(data []byte) error {
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("kernel: parse loader config: %w", err)
	}
	return l.Reconcile(entries)
}

func sameConfig(a, b map[string]any) bool {
	// 深比较走 JSON 归一化，避免 map 元素顺序干扰。
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func cloneEntry(e *Entry) *Entry {
	cp := *e
	cp.Config = make(map[string]any, len(e.Config))
	for k, v := range e.Config {
		cp.Config[k] = v
	}
	return &cp
}
