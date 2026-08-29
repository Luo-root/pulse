package toolset

import (
	"fmt"
	"sync"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
)

// ServiceKey 是工具注册中心的 kernel 服务键。
// 消费方通过 kernel.Get(ctx, toolset.ServiceKey) 获取 Registry。
var ServiceKey = kernel.NewServiceKey[*Registry]("pulse.tools")

// Risk 是宿主侧工具风险分级，不进入 llm.ToolDef。
// 零值 RiskUnspecified 非法：Register 必须拒绝，禁止漏填变成只读（fail-open）。
type Risk int

const (
	RiskUnspecified Risk = iota // 非法；Register 拒绝
	RiskReadonly
	RiskReadWrite
	RiskDangerous
)

// String 便于日志与测试断言。
func (r Risk) String() string {
	switch r {
	case RiskUnspecified:
		return "unspecified"
	case RiskReadonly:
		return "readonly"
	case RiskReadWrite:
		return "readwrite"
	case RiskDangerous:
		return "dangerous"
	default:
		return fmt.Sprintf("Risk(%d)", int(r))
	}
}

// Registration 是一次工具登记的完整宿主侧描述。
type Registration struct {
	Def       llm.ToolDef
	Fn        loop.ToolFunc
	Source    string    // 来源稳定名，如 "local.lookup" / "mcp.filesystem"；元数据，非 Name 主键
	Risk      Risk      // 必填；不得为 RiskUnspecified
	PreviewFn PreviewFn // 可选；执行前只读卡片。nil = 该条目无预览
}

type entry struct {
	def     llm.ToolDef
	fn      loop.ToolFunc
	source  string
	risk    Risk
	preview PreviewFn
	token   uint64 // 区分同名先后登记，避免旧 dispose 误删新条目
}

// Registry 是并发安全的工具注册中心：主键 = Def.Name（全局扁平唯一）。
type Registry struct {
	mu      sync.RWMutex
	tools   map[string]*entry
	bySrc   map[string]map[string]struct{} // source → set of names
	nextTok uint64
	closed  bool
}

// NewRegistry 创建空注册中心。通常由 [Plugin] Provide；测试可直接构造。
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]*entry),
		bySrc: make(map[string]map[string]struct{}),
	}
}

// Plugin 提供 pulse.tools 服务：Apply 时 Provide Registry，卸载时 Close。
func Plugin() kernel.Plugin {
	return kernel.Func(func(c *kernel.Context) error {
		reg := NewRegistry()
		if _, err := kernel.Provide(c, ServiceKey, reg); err != nil {
			return err
		}
		_, err := c.Effect(func() (func(), error) {
			return func() { reg.Close() }, nil
		})
		return err
	})
}

// Close 清空全部登记。幂等。通常由 Plugin Effect 在宿主 Dispose 时调用。
func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.tools = make(map[string]*entry)
	r.bySrc = make(map[string]map[string]struct{})
}

// Register 在 scope 上登记可逆工具。
//
// 校验失败（空名 / nil Fn / 空 Source / RiskUnspecified / 同名冲突 / 已 Close）
// 时立即返回 error，不登记 Effect。
//
// 成功时返回的 dispose 撤销「这一次」登记（幂等）；scope.Dispose 也会
// 通过 Effect 栈触发同一撤销。MCP 掉线应优先 [DisposeSource]，或由
// 来源插件自持 dispose 列表。
func (r *Registry) Register(scope *kernel.Context, reg Registration) (func(), error) {
	if r == nil {
		return nil, fmt.Errorf("toolset: nil registry")
	}
	if scope == nil {
		return nil, fmt.Errorf("toolset: nil scope")
	}
	if err := validateRegistration(reg); err != nil {
		return nil, err
	}

	return scope.Effect(func() (func(), error) {
		tok, err := r.install(reg)
		if err != nil {
			return nil, err
		}
		name := reg.Def.Name
		return func() { r.remove(name, tok) }, nil
	})
}

func validateRegistration(reg Registration) error {
	if reg.Def.Name == "" {
		return fmt.Errorf("toolset: tool name is required")
	}
	if reg.Fn == nil {
		return fmt.Errorf("toolset: tool %q: nil handler", reg.Def.Name)
	}
	if reg.Source == "" {
		return fmt.Errorf("toolset: tool %q: source is required", reg.Def.Name)
	}
	if reg.Risk == RiskUnspecified {
		return fmt.Errorf("toolset: tool %q: risk is required (got unspecified)", reg.Def.Name)
	}
	switch reg.Risk {
	case RiskReadonly, RiskReadWrite, RiskDangerous:
	default:
		return fmt.Errorf("toolset: tool %q: unknown risk %v", reg.Def.Name, reg.Risk)
	}
	return nil
}

func (r *Registry) install(reg Registration) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, fmt.Errorf("toolset: registry closed")
	}
	if _, dup := r.tools[reg.Def.Name]; dup {
		return 0, fmt.Errorf("toolset: tool %q already registered", reg.Def.Name)
	}
	r.nextTok++
	tok := r.nextTok
	r.tools[reg.Def.Name] = &entry{
		def:     reg.Def,
		fn:      reg.Fn,
		source:  reg.Source,
		risk:    reg.Risk,
		preview: reg.PreviewFn,
		token:   tok,
	}
	set := r.bySrc[reg.Source]
	if set == nil {
		set = make(map[string]struct{})
		r.bySrc[reg.Source] = set
	}
	set[reg.Def.Name] = struct{}{}
	return tok, nil
}

func (r *Registry) remove(name string, tok uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.tools[name]
	if !ok || e.token != tok {
		return // 已被替换/撤过/Close；幂等
	}
	delete(r.tools, name)
	if set := r.bySrc[e.source]; set != nil {
		delete(set, name)
		if len(set) == 0 {
			delete(r.bySrc, e.source)
		}
	}
}

// DisposeSource 按 Source 元数据批量撤销该来源下全部登记。
// 供 MCP 掉线使用。禁止调用方用 Def.Name 前缀匹配猜测来源。
// 未知 source 或空串是空操作。幂等。
func (r *Registry) DisposeSource(source string) {
	if r == nil || source == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.bySrc[source]
	if set == nil {
		return
	}
	for name := range set {
		delete(r.tools, name)
	}
	delete(r.bySrc, source)
}

// LookupMeta 查询宿主侧元数据。查不到返回 ok=false（策略应 fail-closed）。
func (r *Registry) LookupMeta(name string) (source string, risk Risk, ok bool) {
	if r == nil || name == "" {
		return "", RiskUnspecified, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.tools[name]
	if !ok {
		return "", RiskUnspecified, false
	}
	return e.source, e.risk, true
}

// AsToolSet 返回 live 适配视图。回合内 Definitions 快照由 loop 在
// Run 开始时取一次；本视图本身随 Register / dispose / DisposeSource 即时变化。
func (r *Registry) AsToolSet() loop.ToolSet {
	return &registryToolSet{r: r}
}
