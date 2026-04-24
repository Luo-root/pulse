package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Luo-root/pulse/components/schema"
)

// 安全路径检查：限制在工作目录内
func safePath(baseDir, userPath string) (string, error) {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	absUser, err := filepath.Abs(userPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absBase, absUser)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path access denied: %s", userPath)
	}
	return absUser, nil
}

// FileRead 读取文件（带安全限制+大小限制）
func FileRead(ctx context.Context, args map[string]any) (any, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return nil, fmt.Errorf("path must be a non-empty string")
	}

	// 安全路径检查
	safePath, err := safePath(".", path)
	if err != nil {
		return nil, err
	}

	// 检查文件大小
	info, err := os.Stat(safePath)
	if err != nil {
		return nil, err
	}
	const maxSize = 10 * 1024 * 1024 // 10MB
	if info.Size() > maxSize {
		return nil, fmt.Errorf("file too large: %d bytes (max %d bytes)", info.Size(), maxSize)
	}

	data, err := os.ReadFile(safePath)
	if err != nil {
		return nil, err
	}

	return map[string]string{
		"content": string(data),
		"path":    safePath,
	}, nil
}

// FileWrite 写入文件（带安全限制+自动创建父目录）
func FileWrite(ctx context.Context, args map[string]any) (any, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return nil, fmt.Errorf("path must be a non-empty string")
	}
	content, ok := args["content"].(string)
	if !ok {
		return nil, fmt.Errorf("content must be a string")
	}

	// 安全路径检查
	safePath, err := safePath(".", path)
	if err != nil {
		return nil, err
	}

	// 自动创建父目录
	dir := filepath.Dir(safePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create parent dir failed: %v", err)
	}

	if err := os.WriteFile(safePath, []byte(content), 0644); err != nil {
		return nil, err
	}

	return map[string]string{"status": "written", "path": safePath}, nil
}

// FileList 列出目录（带安全限制+错误处理）
func FileList(ctx context.Context, args map[string]any) (any, error) {
	dir, ok := args["path"].(string)
	if !ok || dir == "" {
		dir = "."
	}

	// 安全路径检查
	safeDir, err := safePath(".", dir)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(safeDir)
	if err != nil {
		return nil, err
	}

	var files []map[string]string
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue // 跳过获取信息失败的文件
		}
		files = append(files, map[string]string{
			"name":     entry.Name(),
			"type":     map[bool]string{true: "dir", false: "file"}[entry.IsDir()],
			"size":     fmt.Sprintf("%d", info.Size()),
			"mod_time": info.ModTime().Format("2006-01-02 15:04:05"),
		})
	}

	return map[string]any{"files": files, "path": safeDir}, nil
}

func RegisterFileTools(executor *schema.ToolExecutor) {
	executor.MustRegister(schema.Tool{
		Name:        "file_read",
		Description: "读取文件内容（最大10MB），返回文本。路径限制在当前工作目录内。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "文件路径（必填）"},
			},
			"required": []string{"path"},
		},
	}, FileRead)

	executor.MustRegister(schema.Tool{
		Name:        "file_write",
		Description: "写入内容到文件，自动创建父目录。路径限制在当前工作目录内。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "文件路径（必填）"},
				"content": map[string]any{"type": "string", "description": "文件内容（必填）"},
			},
			"required": []string{"path", "content"},
		},
	}, FileWrite)

	executor.MustRegister(schema.Tool{
		Name:        "file_list",
		Description: "列出目录下的文件和文件夹。路径限制在当前工作目录内。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "目录路径（可选，默认为当前目录）"},
			},
		},
	}, FileList)
}
