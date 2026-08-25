package llm

import (
	"context"
	"fmt"
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
var (
	EventBeforeGenerate = kernel.NewEventKey[*GenerateRequest]("pulse.llm.before_generate")
	EventAfterResponse  = kernel.NewEventKey[*Response]("pulse.llm.after_response")
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

// Registry 是 provider 工厂与命名实例的注册中心。
//
// 两级结构：
//   - RegisterProvider("openai", factory)：登记 adapter；
//   - Declare("main", cfg) + Open("main")：声明并打开命名实例。
//
// Open 返回的是被拦截包装的实例：每次调用都会先过
// EventBeforeGenerate（waterfall），成功返回后发 EventAfterResponse
// （emit）。计量、限流、路由因此都是普通的事件监听插件。
type Registry struct {
	ctx *kernel.Context // 拦截事件的派发作用域

	mu        sync.Mutex
	factories map[string]Factory
	decls     map[string]Config
	opened    map[string]ChatModel
	defaultID string
}

// NewRegistry 创建注册中心。调用方负责将其 Provide 到 ServiceKey
// （或直接使用 [Plugin] 完成装配）。
func NewRegistry(c *kernel.Context) *Registry {
	return &Registry{
		ctx:       c,
		factories: make(map[string]Factory),
		decls:     make(map[string]Config),
		opened:    make(map[string]ChatModel),
	}
}

// Plugin 返回提供本服务的内核插件：Apply 时向所在作用域
// Provide 一个绑定该作用域的 Registry。
func Plugin() kernel.Plugin {
	return kernel.Func(func(c *kernel.Context) error {
		_, err := kernel.Provide(c, ServiceKey, NewRegistry(c))
		return err
	})
}

// RegisterProvider 登记一个 provider 工厂；同名覆盖旧登记
// （撤旧装新语义）。返回撤销函数。
func (r *Registry) RegisterProvider(name string, f Factory) (func(), error) {
	r.mu.Lock()
	r.factories[name] = f
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if cur, ok := r.factories[name]; ok && sameFactory(cur, f) {
				delete(r.factories, name)
			}
		})
	}, nil
}

func sameFactory(a, b Factory) bool { return fmt.Sprintf("%p", a) == fmt.Sprintf("%p", b) }

// Declare 声明一个命名实例。重复 Declare 同名 id 会替换声明并
// 关闭已打开的旧实例（下次 Open 用新配置重建）。
func (r *Registry) Declare(id string, cfg Config) error {
	if cfg.Provider == "" {
		return NewError(ErrBadRequest, "", 0, nil, "declare %q: provider is required", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decls[id] = cfg
	if old, ok := r.opened[id]; ok {
		closeModel(old)
		delete(r.opened, id)
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
	if m, ok := r.opened[id]; ok {
		r.mu.Unlock()
		return m, nil
	}
	cfg, ok := r.decls[id]
	if !ok {
		r.mu.Unlock()
		return nil, NewError(ErrNoModel, "", 0, nil, "model %q not declared", id)
	}
	factory, ok := r.factories[cfg.Provider]
	if !ok {
		r.mu.Unlock()
		return nil, NewError(ErrNoModel, cfg.Provider, 0, nil,
			"provider %q not registered (model %q)", cfg.Provider, id)
	}
	r.mu.Unlock()

	m, err := factory(cfg)
	if err != nil {
		return nil, NewError(ErrUnknown, cfg.Provider, 0, err, "build model %q", id)
	}

	wrapped := &observed{inner: m, reg: r}
	r.mu.Lock()
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

func (o *observed) Generate(ctx context.Context, req *GenerateRequest) (*Response, error) {
	req = kernel.Waterfall(o.reg.ctx, EventBeforeGenerate, req)
	resp, err := o.inner.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	kernel.Emit(o.reg.ctx, EventAfterResponse, resp)
	return resp, nil
}

func (o *observed) Stream(ctx context.Context, req *GenerateRequest) (<-chan StreamEvent, error) {
	req = kernel.Waterfall(o.reg.ctx, EventBeforeGenerate, req)
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
					kernel.Emit(o.reg.ctx, EventAfterResponse, ev.Response)
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
