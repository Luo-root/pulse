package tools

import (
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================================
// 选项模式 - 用于动态工具注册
// ============================================================================

// ToolOption 工具配置选项
type ToolOption func(*schema.ToolMetadata)

// WithPermission 设置权限
func WithPermission(perm schema.ToolPermission) ToolOption {
	return func(m *schema.ToolMetadata) {
		m.Permission = perm
	}
}

// WithCategory 设置分类
func WithCategory(cat string) ToolOption {
	return func(m *schema.ToolMetadata) {
		m.Category = cat
	}
}

// WithTimeout 设置超时
func WithTimeout(timeout time.Duration) ToolOption {
	return func(m *schema.ToolMetadata) {
		m.Timeout = timeout
	}
}

// WithTags 设置标签
func WithTags(tags ...string) ToolOption {
	return func(m *schema.ToolMetadata) {
		m.Tags = tags
	}
}

// WithVersion 设置版本
func WithVersion(version string) ToolOption {
	return func(m *schema.ToolMetadata) {
		m.Version = version
	}
}

// WithAuthor 设置作者
func WithAuthor(author string) ToolOption {
	return func(m *schema.ToolMetadata) {
		m.Author = author
	}
}

// WithParameters 设置参数定义
func WithParameters(params map[string]any) ToolOption {
	return func(m *schema.ToolMetadata) {
		m.Parameters = params
	}
}

// ============================================================================
// 动态工具加载器 - 用于 MCP / Skill 动态加载
// ============================================================================

// DynamicToolLoader 动态工具加载器
type DynamicToolLoader struct {
	registry *schema.ToolRegistry
}

// NewDynamicToolLoader 创建动态加载器
func NewDynamicToolLoader(registry *schema.ToolRegistry) *DynamicToolLoader {
	return &DynamicToolLoader{registry: registry}
}

// Load 动态注册工具（完整版）
func (d *DynamicToolLoader) Load(
	name string,
	description string,
	handler schema.ToolHandler,
	parameters map[string]any,
	opts ...ToolOption,
) error {
	meta := schema.ToolMetadata{
		Name:        name,
		Description: description,
		Parameters:  parameters,
		Permission:  schema.PermReadOnly,
		Category:    "dynamic",
		Version:     "1.0.0",
		Tags:        []string{"dynamic"},
		Timeout:     30 * time.Second,
	}

	for _, opt := range opts {
		opt(&meta)
	}

	return d.registry.Register(meta, handler)
}

// LoadSimple 动态注册工具（简化版，无参数）
func (d *DynamicToolLoader) LoadSimple(
	name string,
	description string,
	handler schema.ToolHandler,
	opts ...ToolOption,
) error {
	return d.Load(name, description, handler, map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}, opts...)
}
