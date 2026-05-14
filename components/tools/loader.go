package tools

import (
	"time"
)

// DynamicToolLoader 动态工具加载器
type DynamicToolLoader struct {
	registry *ToolRegistry
}

func NewDynamicToolLoader(registry *ToolRegistry) *DynamicToolLoader {
	return &DynamicToolLoader{registry: registry}
}

func (d *DynamicToolLoader) Load(
	name string,
	description string,
	handler ToolHandler,
	parameters map[string]any,
	opts ...ToolOption,
) error {
	meta := ToolMetadata{
		Name:        name,
		Description: description,
		Parameters:  parameters,
		Permission:  PermReadOnly,
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

func (d *DynamicToolLoader) LoadSimple(
	name string,
	description string,
	handler ToolHandler,
	opts ...ToolOption,
) error {
	return d.Load(name, description, handler, map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}, opts...)
}
