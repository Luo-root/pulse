package tools

import (
	"context"
	"os"
	"path/filepath"
	"time"
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

// getWorkDirParams get_work_dir 参数定义
var getWorkDirParams = map[string]any{
	"type":       "object",
	"properties": map[string]any{},
}

// RegisterEnvTools 注册环境工具
func RegisterEnvTools(registry *ToolRegistry) {
	registry.MustRegister(ToolMetadata{
		Name:        "get_work_dir",
		Description: "获取当前工作目录和操作系统信息",
		Parameters:  getWorkDirParams,
		Permission:  PermReadOnly,
		Category:    "env",
		Version:     "1.0.0",
		Tags:        []string{"env", "info", "safe"},
		Timeout:     5 * time.Second,
	}, GetWorkDirTool)
}
