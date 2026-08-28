package llm

import (
	"context"
	"io"
	"sync"

	"github.com/Luo-root/pulse/kernel"
)

// ServiceKey 是模型注册中心的 kernel 服务键。
// 消费方通过 kernel.Get(ctx, llm.ServiceKey) 获取注册中心，
// 而不是 import 具体实现。
var ServiceKey = kernel.NewServiceKey[*Registry]("pulse.llm")

// 拦截事件：能力挂载点（对标 DSH 的 llm/stream waterfall）。
//
//	before_generate（waterfall）：请求发出前经过所有监听器，可就地
//	  改写请求字段（路由、注入默认参数、脱敏、限流检查……）；
//	after_response（emit）：拿到响应后通知观察者（计量、审计、缓存）。
//	  载荷是值类型 Response：观察者拿到 *Response 只读，改写不了
//	  调用方的结果——与 before_generate 的指针改写语义形成对照。
var (
	EventBeforeGenerate = kernel.NewEventKey[*GenerateRequest]("pulse.llm.before_generate")
	EventAfterResponse  = kernel.NewEventKey[Response]("pulse.llm.after_response")
)

// Config 是一个命名模型实例的声明。
type Config struct {
	Provider string `json:"provider" yaml:"provider"` // 已注册的 provider 名
	Model    string `json:"model" yaml:"model"`       // provider 侧的模型名
	BaseURL  string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	APIKey   string `json:"api_key,omitempty" yaml:"api_key,omitempty"`
	// Options 是 provider 特有参数，由 adapter 自行解释
	// （例如 organization、project、自定义超时）。
	Options map[string]any `json:"options,omitempty" yaml:"options,omitempty"`
}

// Factory 由配置构造模型实例。每个 adapter 提供一个工厂。
type Factory func(cfg Config) (ChatModel, error)

// factoryEntry 包装工厂以获得稳定的登记身份：撤销时按指针比较，
// 避免两个闭包包裹同一函数时被误判为同一条目。
type factoryEntry struct{ f Factory }

// Registry 是 provider 工厂与命名实例的注册中心。
//
// 两级结构：
//   - RegisterProvider(scope, "openai", factory)：登记 adapter——本身是
//     一条内核效应，adapter 插件卸载时工厂随之收回；
//   - Declare(id, cfg) + Open(id)：声明并打开命名实例。
//
// Open 返回的是被拦截包装的实例：每次调用都会先过
// EventBeforeGenerate（waterfall），成功返回后发 EventAfterResponse
// （emit）。计量、限流、路由因此都是普通的事件监听插件。
type Registry struct {
	ctx *kernel.Context // 拦截事件的派发作用域

	mu        sync.Mutex
	factories map[string]*factoryEntry
	decls     map[string]Config
	opened    map[string]ChatModel
	defaultID string
	closed    bool
}

// NewRegistry 创建注册中心。调用方负责将其 Provide 到 ServiceKey
// （或直接使用 [Plugin] 完成装配）。
func NewRegistry(c *kernel.Context) *Registry {
	return &Registry{
		ctx:       c,
		factories: make(map[string]*factoryEntry),
		decls:     make(map[string]Config),
		opened:    make(map[string]ChatModel),
	}
}

// EventScope 返回拦截事件的 Local 派发作用域（通常是 llm.Plugin Apply
// 的私有子 ctx）。无请求级 WithEventScope 时，observed 回退到这里。
// 装配层若要在「01-chat 直连 Generate」路径上挂 before_generate，
// 必须装到本 scope，而不是 host 根（EmitLocal 不向父冒泡）。
func (r *Registry) EventScope() *kernel.Context {
	if r == nil {
		return nil
	}
	return r.ctx
}

// Plugin 返回提供本服务的内核插件：
//   - Apply 时向所在作用域 Provide 一个绑定该作用域的 Registry；
//   - 注册中心的生命周期与插件一致——卸载时关闭全部打开的实例。
func Plugin() kernel.Plugin {
	return kernel.Func(func(c *kernel.Context) error {
		reg := NewRegistry(c)
		if _, err := kernel.Provide(c, ServiceKey, reg); err != nil {
			return err // Provide 自动登记为效应，随插件卸载撤除
		}
		_, err := c.Effect(func() (func(), error) {
			return func() { reg.Close() }, nil
		})
		return err
	})
}

// RegisterProvider 登记一个 provider 工厂。scope 是登记所属的
// 作用域——adapter 插件应传入自己 Apply 收到的 Context，使工厂的
// 生命周期与插件一致（Apply 中丢弃返回值也不会泄漏，插件卸载时
// 工厂与其打开的实例一并收回）。
//
// 同名覆盖 = 撤旧装新，旧 provider 名下已打开的实例立即失效关闭
// （下次 Open 用新工厂重建）。返回的 dispose 可提前手动撤销（幂等）。
func (r *Registry) RegisterProvider(scope *kernel.Context, name string, f Factory) (func(), error) {
	entry := &factoryEntry{f: f}
	return scope.Effect(func() (func(), error) {
		r.installFactory(name, entry)
		return func() { r.removeFactory(name, entry) }, nil
	})
}

// installFactory 替换工厂并失效该 provider 名下全部已开实例
// （须不在 r.mu 内调用）。
func (r *Registry) installFactory(name string, entry *factoryEntry) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.factories[name] = entry
	stale := r.evictProviderLocked(name)
	r.mu.Unlock()
	for _, m := range stale {
		closeModel(m)
	}
}

// removeFactory 撤销自己的登记；若同名已被后来者覆盖则不动。
func (r *Registry) removeFactory(name string, entry *factoryEntry) {
	r.mu.Lock()
	cur, ok := r.factories[name]
	if !ok || cur != entry {
		r.mu.Unlock()
		return
	}
	delete(r.factories, name)
	stale := r.evictProviderLocked(name)
	r.mu.Unlock()
	for _, m := range stale {
		closeModel(m)
	}
}

// evictProviderLocked 取出该 provider 名下全部已开实例并出缓存。
// 须持有 r.mu。
func (r *Registry) evictProviderLocked(name string) []ChatModel {
	var stale []ChatModel
	for id, cfg := range r.decls {
		if cfg.Provider != name {
			continue
		}
		if m, ok := r.opened[id]; ok {
			stale = append(stale, m)
			delete(r.opened, id)
		}
	}
	return stale
}

// Declare 声明一个命名实例。重复 Declare 同名 id 会替换声明并
// 关闭已打开的旧实例（下次 Open 用新配置重建）。
func (r *Registry) Declare(id string, cfg Config) error {
	if cfg.Provider == "" {
		return NewError(ErrBadRequest, "", 0, nil, "declare %q: provider is required", id)
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return NewError(ErrUnknown, "", 0, nil, "registry closed")
	}
	r.decls[id] = cfg
	old, hadOld := r.opened[id]
	if hadOld {
		delete(r.opened, id)
	}
	r.mu.Unlock()
	if hadOld {
		closeModel(old)
	}
	return nil
}

// SetDefault 设置默认实例 ID。
func (r *Registry) SetDefault(id string) {
	r.mu.Lock()
	r.defaultID = id
	r.mu.Unlock()
}

// DefaultID 返回默认实例 ID（可能为空串）。
func (r *Registry) DefaultID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.defaultID
}

// Open 打开（或复用缓存的）命名实例。
func (r *Registry) Open(id string) (ChatModel, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, NewError(ErrUnknown, "", 0, nil, "registry closed")
	}
	if m, ok := r.opened[id]; ok {
		r.mu.Unlock()
		return m, nil
	}
	cfg, ok := r.decls[id]
	if !ok {
		r.mu.Unlock()
		return nil, NewError(ErrNoModel, "", 0, nil, "model %q not declared", id)
	}
	fe, ok := r.factories[cfg.Provider]
	if !ok {
		r.mu.Unlock()
		return nil, NewError(ErrNoModel, cfg.Provider, 0, nil,
			"provider %q not registered (model %q)", cfg.Provider, id)
	}
	r.mu.Unlock()

	m, err := fe.f(cfg)
	if err != nil {
		return nil, NewError(ErrUnknown, cfg.Provider, 0, err, "build model %q", id)
	}

	wrapped := &observed{inner: m, reg: r}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		closeModel(m)
		return nil, NewError(ErrUnknown, "", 0, nil, "registry closed")
	}
	// 双检：并发 Open 时后到者让位。
	if cur, ok := r.opened[id]; ok {
		r.mu.Unlock()
		closeModel(m)
		return cur, nil
	}
	r.opened[id] = wrapped
	r.mu.Unlock()
	return wrapped, nil
}

// OpenDefault 打开默认实例。
func (r *Registry) OpenDefault() (ChatModel, error) {
	id := r.DefaultID()
	if id == "" {
		return nil, NewError(ErrNoModel, "", 0, nil, "no default model set")
	}
	return r.Open(id)
}

// Drop 关闭并移除实例与声明。
func (r *Registry) Drop(id string) {
	r.mu.Lock()
	old := r.opened[id]
	delete(r.opened, id)
	delete(r.decls, id)
	if r.defaultID == id {
		r.defaultID = ""
	}
	r.mu.Unlock()
	closeModel(old)
}

// Close 关闭注册中心：关闭全部打开的实例并作废一切登记。
// 由 llm.Plugin 在插件卸载时自动调用；重复调用幂等。
func (r *Registry) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	all := r.opened
	r.opened = make(map[string]ChatModel)
	r.decls = make(map[string]Config)
	r.factories = make(map[string]*factoryEntry)
	r.defaultID = ""
	r.mu.Unlock()
	for _, m := range all {
		closeModel(m)
	}
}

// Providers 返回已登记的 provider 名列表（诊断用）。
func (r *Registry) Providers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.factories))
	for name := range r.factories {
		out = append(out, name)
	}
	return out
}

func closeModel(m ChatModel) {
	if c, ok := m.(io.Closer); ok {
		_ = c.Close()
	}
}

// observed 是拦截包装：所有调用经过 before/after 事件。
type observed struct {
	inner ChatModel
	reg   *Registry
}

// eventScope 解析本次调用的派发作用域：
//   - ctx 带请求级 scope（loop 注入）→ 用它做 Local 派发，Bridge 才能只听本请求；
//   - 否则回退到 Registry 宿主 scope，仍走 Local（不再全树广播）。
//
// 禁止「EmitLocal(reg.ctx)」却指望挂在 reqScope 的监听收到——那两条不等价。
func (o *observed) eventScope(ctx context.Context) *kernel.Context {
	if s := EventScopeFrom(ctx); s != nil {
		return s
	}
	return o.reg.ctx
}

func (o *observed) Generate(ctx context.Context, req *GenerateRequest) (*Response, error) {
	scope := o.eventScope(ctx)
	req = kernel.WaterfallLocal(scope, EventBeforeGenerate, req)
	resp, err := o.inner.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	kernel.EmitLocal(scope, EventAfterResponse, *resp)
	return resp, nil
}

func (o *observed) Stream(ctx context.Context, req *GenerateRequest) (<-chan StreamEvent, error) {
	scope := o.eventScope(ctx)
	req = kernel.WaterfallLocal(scope, EventBeforeGenerate, req)
	src, err := o.inner.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(chan StreamEvent, 8)
	go func() {
		defer close(out)
		for ev := range src {
			out <- ev
			if ev.Kind == EventDone {
				if ev.Response != nil {
					kernel.EmitLocal(scope, EventAfterResponse, *ev.Response)
				}
				return
			}
			if ev.Kind == EventError {
				return
			}
		}
	}()
	return out, nil
}

// Close 转发给内层模型（内层实现 io.Closer 才真正关闭）——保证
// Registry.Drop / Declare 替换 / Registry.Close 能关到真实资源。
func (o *observed) Close() error {
	if c, ok := o.inner.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
