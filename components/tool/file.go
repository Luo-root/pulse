package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// FileRead 读取文件
func FileRead(ctx context.Context, args map[string]any) (any, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, fmt.Errorf("path is required")
	}

	// 安全检查：限制路径
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}

	return map[string]string{
		"content": string(data),
		"path":    absPath,
	}, nil
}

// FileWrite 写入文件
func FileWrite(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return nil, err
	}

	return map[string]string{"status": "written", "path": path}, nil
}

// FileList 列出目录
func FileList(ctx context.Context, args map[string]any) (any, error) {
	dir, _ := args["path"].(string)
	if dir == "" {
		dir = "."
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var files []map[string]string
	for _, entry := range entries {
		info, _ := entry.Info()
		files = append(files, map[string]string{
			"name":     entry.Name(),
			"type":     map[bool]string{true: "dir", false: "file"}[entry.IsDir()],
			"size":     fmt.Sprintf("%d", info.Size()),
			"mod_time": info.ModTime().Format("2006-01-02 15:04:05"),
		})
	}

	return map[string]any{"files": files, "path": dir}, nil
}
