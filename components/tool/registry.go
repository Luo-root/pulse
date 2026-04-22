package tools

import "github.com/Luo-root/pulse/components/schema"

// RegisterAll 注册所有基础工具
func RegisterAll(executor *schema.ToolExecutor) {
	executor.MustRegister(schema.Tool{
		Name:        "file_read",
		Description: "读取文件内容，返回文本",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "文件路径"},
			},
			"required": []string{"path"},
		},
	}, FileRead)

	executor.MustRegister(schema.Tool{
		Name:        "file_write",
		Description: "写入内容到文件",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
	}, FileWrite)

	executor.MustRegister(schema.Tool{
		Name:        "file_list",
		Description: "列出目录下的文件",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "目录路径，默认为当前目录"},
			},
		},
	}, FileList)

	executor.MustRegister(schema.Tool{
		Name:        "command_exec",
		Description: "执行系统命令，返回输出结果",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "要执行的命令"},
				"timeout": map[string]any{"type": "number", "description": "超时时间（秒），默认30"},
			},
			"required": []string{"command"},
		},
	}, CommandExec)
}
