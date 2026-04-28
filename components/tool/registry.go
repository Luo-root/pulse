package tools

import "github.com/Luo-root/pulse/components/schema"

// RegisterAll 注册所有基础工具到 ToolRegistry
func RegisterAll(registry *schema.ToolRegistry) {
	RegisterFileTools(registry)
	RegisterCommandTools(registry)
	RegisterEnvTools(registry)
}
