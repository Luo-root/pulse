package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Configurable 是插件可选实现的配置接收接口。
//
// Loader 装载条目时，在 Use 之前把 Entry.Config 交给实现了本接口
// 的插件实例（对齐 Cordis 把 entry.config 绑定进组件 apply 的做法）。
// 配置是实例私有的——每个条目拿到自己那份，不经过全局服务仓库，
// 因此多个条目之间不会互相覆盖、卸载互不影响：
//
//	type MyPlugin struct{ cfg map[string]any }
//	func (p *MyPlugin) Configure(cfg map[string]any) error { p.cfg = cfg; return nil }
type Configurable interface {
	Configure(cfg map[string]any) error
}

// Entry 是 Loader 配置树中的一个条目：一个待装载插件的声明式描述。
// 整个系统的装配形态 = 条目列表，它是"系统加载了什么"的权威记录。
type Entry struct {
	// ID 是稳定标识，作为增量调和的 key。
	ID string `json:"id" yaml:"id"`
	// Name 是插件工厂名（须先经 Loader.Register 注册）。
	Name string `json:"name" yaml:"name"`
	// Disabled 为 true 时该条目保持卸载状态（保留配置便于恢复）。
	Disabled bool `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	// Config 经由 Configurable 接口传给该条目的插件实例。
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

	// reconcileMu 串行化整个调和过程；mu 只保护下面的数据结构
	// （短临界区）。因此装载期间插件代码可以安全调用 Fiber /
	// Snapshot 做查询——但不得反向调用 Reconcile（插件不应知道
	// Loader 的存在）。
	reconcileMu sync.Mutex

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

// mountPlan 是一次待执行装载的完整描述（阶段一产出、阶段二消费）。
type mountPlan struct {
	id      string
	cfg     map[string]any
	factory Factory
}

// Reconcile 将条目列表调和为期望的运行形态。单个条目的失败不阻断
// 其余条目；返回聚合错误（errors.Join），所有失败条目都可见。
//
// 执行分三阶段：持锁 diff 出计划 → 解锁执行回收与装载（插件 Apply
// 运行期间不持有任何 Loader 锁）→ 持锁提交结果。并发 Reconcile 由
// reconcileMu 串行排队。
func (l *Loader) Reconcile(entries []Entry) error {
	l.reconcileMu.Lock()
	defer l.reconcileMu.Unlock()

	// ---- 阶段一：diff 出回收与装载计划 ----
	l.mu.Lock()
	want := make(map[string]*Entry, len(entries))
	for i := range entries {
		e := entries[i]
		if e.ID == "" {
			continue
		}
		want[e.ID] = &entries[i]
	}

	var errs []error
	var toClose []*Fiber
	closing := make(map[string]struct{})
	var plans []mountPlan
	newEntries := make(map[string]*Entry, len(want))

	for id, f := range l.fibers {
		if _, keep := want[id]; !keep {
			toClose = append(toClose, f)
			closing[id] = struct{}{}
		}
	}
	for id, e := range want {
		cur := l.entries[id]
		if cur != nil && cur.Name == e.Name &&
			sameConfig(cur.Config, e.Config) && cur.Disabled == e.Disabled {
			newEntries[id] = cloneEntry(e)
			continue // 无变化
		}
		if old, ok := l.fibers[id]; ok {
			toClose = append(toClose, old)
			closing[id] = struct{}{}
		}
		if e.Disabled {
			newEntries[id] = cloneEntry(e)
			continue // 保留记录但不装载
		}
		factory, ok := l.factories[e.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("kernel: entry %q references unknown plugin %q", id, e.Name))
			newEntries[id] = cloneEntry(e)
			continue
		}
		plans = append(plans, mountPlan{id: id, cfg: cloneConfig(e.Config), factory: factory})
		newEntries[id] = cloneEntry(e)
	}
	l.mu.Unlock()

	// ---- 阶段二：解锁执行回收与装载 ----
	for _, f := range toClose {
		f.Close()
	}

	type mountedFiber struct {
		id string
		f  *Fiber
	}
	var mountedList []mountedFiber
	for _, m := range plans {
		f, err := l.mount(m.cfg, m.factory)
		if err != nil {
			errs = append(errs, fmt.Errorf("kernel: entry %q (%s): %w", m.id, newEntries[m.id].Name, err))
			continue
		}
		mountedList = append(mountedList, mountedFiber{id: m.id, f: f})
	}

	// ---- 阶段三：持锁提交结果 ----
	l.mu.Lock()
	for id := range closing {
		delete(l.fibers, id)
	}
	for _, m := range mountedList {
		l.fibers[m.id] = m.f
	}
	l.entries = newEntries
	l.mu.Unlock()

	return errors.Join(errs...)
}

// mount 装载一个条目：工厂建实例 => 可选的 Configure 交接私有配置
// （失败视为该条目装载失败）=> 同步 Use。
func (l *Loader) mount(cfg map[string]any, factory Factory) (*Fiber, error) {
	p := factory()
	if cf, ok := p.(Configurable); ok {
		if err := cf.Configure(cloneConfig(cfg)); err != nil {
			return nil, fmt.Errorf("configure: %w", err)
		}
	}
	return Use(l.host, p)
}

// Fiber 返回某条目当前对应的实例；不存在（含 Disabled）返回 nil。
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

func sameConfig(a, b map[string]any) bool {
	// 深比较走 JSON 归一化，避免 map 元素顺序干扰。
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func cloneConfig(cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out
}

func cloneEntry(e *Entry) *Entry {
	cp := *e
	cp.Config = cloneConfig(e.Config)
	return &cp
}
