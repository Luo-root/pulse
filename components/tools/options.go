package tools

import (
	"time"
)

// ============================================================================
// 选项模式 - 用于动态工具注册
// ============================================================================

// ToolOption 工具配置选项
type ToolOption func(*ToolMetadata)

// WithPermission 设置权限
func WithPermission(perm ToolPermission) ToolOption {
	return func(m *ToolMetadata) {
		m.Permission = perm
	}
}

// WithCategory 设置分类
func WithCategory(cat string) ToolOption {
	return func(m *ToolMetadata) {
		m.Category = cat
	}
}

// WithTimeout 设置超时
func WithTimeout(timeout time.Duration) ToolOption {
	return func(m *ToolMetadata) {
		m.Timeout = timeout
	}
}

// WithTags 设置标签
func WithTags(tags ...string) ToolOption {
	return func(m *ToolMetadata) {
		m.Tags = tags
	}
}

// WithVersion 设置版本
func WithVersion(version string) ToolOption {
	return func(m *ToolMetadata) {
		m.Version = version
	}
}

// WithAuthor 设置作者
func WithAuthor(author string) ToolOption {
	return func(m *ToolMetadata) {
		m.Author = author
	}
}

// WithParameters 设置参数定义
func WithParameters(params map[string]any) ToolOption {
	return func(m *ToolMetadata) {
		m.Parameters = params
	}
}
