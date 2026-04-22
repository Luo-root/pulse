package tools

import (
	"context"
	"os"
	"path/filepath"

	"github.com/Luo-root/pulse/components/schema"
)

// GetWorkDir 获取当前工作目录
func GetWorkDir() string {
	dir, _ := os.Getwd()
	abs, _ := filepath.Abs(dir)
	return abs
}

// GetWorkDirTool 工具：返回当前工作目录
func GetWorkDirTool(ctx context.Context, args map[string]any) (any, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(dir)
	return map[string]string{
		"work_dir": abs,
		"os":       os.Getenv("OS"),
	}, nil
}

// RegisterEnvTools 注册环境相关工具
func RegisterEnvTools(executor *schema.ToolExecutor) {
	executor.MustRegister(schema.Tool{
		Name:        "get_work_dir",
		Description: "获取当前工作目录和操作系统信息",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, GetWorkDirTool)
}
