package tools

import "github.com/Luo-root/pulse/components/schema"

// RegisterAll 注册所有基础工具
func RegisterAll(executor *schema.ToolExecutor) {
	RegisterFileTools(executor)
	RegisterCommandExecTools(executor)
	RegisterEnvTools(executor)
}
