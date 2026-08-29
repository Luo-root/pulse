package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/toolset"
)

// Config 描述一个 MCP 来源插件的装配参数。
type Config struct {
	// ID 非空；Source 元数据固定为 "mcp." + ID（不是 Name 前缀协议）。
	ID string
	// Client 必填。由调用方注入 mock 或真实 SDK 适配器。
	Client Client
	// NamePrefix 可空。非空时 Def.Name = NamePrefix + "_" + 上游名，
	// 在 Register 之前定名；撞名失败，禁止冲突后改名。
	NamePrefix string
	// DefaultRisk 必填；Register 时写入每条工具。Unspecified 拒绝。
	DefaultRisk toolset.Risk
	// PreviewFn 覆盖本源全部工具的预览。nil 则用 DefaultPreview（opaque）。
	PreviewFn toolset.PreviewFn
}

// Source 持有一次 MCP 来源的装载状态：Sync 登记，Detach 整源撤销。
type Source struct {
	cfg Config
	reg *toolset.Registry

	mu     sync.Mutex
	synced bool
}

// sourceKey 返回 Registry 条目的 Source 元数据。
func (c Config) sourceKey() string { return "mcp." + c.ID }

func validateConfig(cfg Config) error {
	if cfg.ID == "" {
		return fmt.Errorf("toolset/mcp: id is required")
	}
	if cfg.Client == nil {
		return fmt.Errorf("toolset/mcp: client is required")
	}
	if cfg.DefaultRisk == toolset.RiskUnspecified {
		return fmt.Errorf("toolset/mcp: default risk is required (got unspecified)")
	}
	switch cfg.DefaultRisk {
	case toolset.RiskReadonly, toolset.RiskReadWrite, toolset.RiskDangerous:
	default:
		return fmt.Errorf("toolset/mcp: unknown default risk %v", cfg.DefaultRisk)
	}
	return nil
}

func modelName(prefix, upstream string) string {
	if prefix == "" {
		return upstream
	}
	return prefix + "_" + upstream
}

// NewSource 绑定 Registry 与 Client，不立即 List/Register。
// 调用 Sync 完成上线；Detach 或 scope Dispose 完成掉线。
func NewSource(reg *toolset.Registry, cfg Config) (*Source, error) {
	if reg == nil {
		return nil, fmt.Errorf("toolset/mcp: nil registry")
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	return &Source{cfg: cfg, reg: reg}, nil
}

// Plugin 返回可装入 kernel 的来源插件：Apply 时 Sync，卸载时 Detach + Close Client。
func Plugin(reg *toolset.Registry, cfg Config) (kernel.Plugin, error) {
	src, err := NewSource(reg, cfg)
	if err != nil {
		return nil, err
	}
	return kernel.Func(func(c *kernel.Context) error {
		if err := src.Sync(c, context.Background()); err != nil {
			return err
		}
		_, err := c.Effect(func() (func(), error) {
			return func() {
				src.Detach()
				_ = cfg.Client.Close()
			}, nil
		})
		return err
	}), nil
}

// Sync 拉取 ListTools 并 Register。已 synced 时先 Detach 再挂（重连语义）。
// scope 用于 Register 的可逆 Effect；通常传入来源插件的 Apply ctx，
// 不要挂到单次请求 reqScope。
func (s *Source) Sync(scope *kernel.Context, ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("toolset/mcp: nil source")
	}
	if scope == nil {
		return fmt.Errorf("toolset/mcp: nil scope")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.synced {
		s.reg.DisposeSource(s.cfg.sourceKey())
		s.synced = false
	}

	tools, err := s.cfg.Client.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("toolset/mcp: list tools: %w", err)
	}

	srcKey := s.cfg.sourceKey()
	for _, t := range tools {
		if t.Name == "" {
			s.reg.DisposeSource(srcKey)
			return fmt.Errorf("toolset/mcp: upstream tool name is required")
		}
		final := modelName(s.cfg.NamePrefix, t.Name)
		upstream := t.Name
		client := s.cfg.Client
		preview := s.cfg.PreviewFn
		if preview == nil {
			preview = DefaultPreview(srcKey, upstream, s.cfg.DefaultRisk)
		}
		_, err := s.reg.Register(scope, toolset.Registration{
			Def:       DefFromTool(final, t),
			Source:    srcKey,
			Risk: s.cfg.DefaultRisk,
			Fn: func(callCtx context.Context, args json.RawMessage) (string, error) {
				return client.CallTool(callCtx, upstream, args)
			},
			PreviewFn: preview,
		})
		if err != nil {
			s.reg.DisposeSource(srcKey)
			return fmt.Errorf("toolset/mcp: register %q: %w", final, err)
		}
	}
	s.synced = true
	return nil
}

// Detach 撤销本 Source 下全部登记（DisposeSource）。幂等。
// 不关闭 Client；关闭由 Plugin Effect 或调用方负责。
func (s *Source) Detach() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reg.DisposeSource(s.cfg.sourceKey())
	s.synced = false
}

// SourceKey 返回 Registry 元数据键（"mcp." + ID）。
func (s *Source) SourceKey() string {
	if s == nil {
		return ""
	}
	return s.cfg.sourceKey()
}
