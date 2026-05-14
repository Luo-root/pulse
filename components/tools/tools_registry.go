package tools

// RegisterAll 注册所有基础工具到 ToolRegistry
func RegisterAll(registry *ToolRegistry) {
	RegisterFileTools(registry)
	RegisterCommandTools(registry)
	RegisterEnvTools(registry)
	RegisterWebTools(registry)
	RegisterUserConfigTools(registry)
}
