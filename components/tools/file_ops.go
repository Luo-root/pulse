package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ============================================================================
// file_read — 读取文件内容
// ============================================================================

func FileRead(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("file not found: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("path is a directory, use file_list instead")
	}

	// 默认限制 10MB
	if info.Size() > 10*1024*1024 {
		return nil, fmt.Errorf("file too large (%d bytes), max 10MB", info.Size())
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	return map[string]any{
		"path":     absPath,
		"content":  string(data),
		"size":     info.Size(),
		"mode":     info.Mode().String(),
		"mod_time": info.ModTime().Format(time.RFC3339),
	}, nil
}

// ============================================================================
// file_write — 写入文件（自动创建父目录）
// ============================================================================

func FileWrite(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}

	content, _ := args["content"].(string)

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	// 自动创建父目录
	parentDir := filepath.Dir(absPath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return nil, fmt.Errorf("create parent directory: %w", err)
	}

	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}

	info, _ := os.Stat(absPath)
	result := map[string]any{
		"path":    absPath,
		"size":    len(content),
		"created": true,
	}
	if info != nil {
		result["mode"] = info.Mode().String()
	}

	return result, nil
}

// ============================================================================
// file_list — 列出目录内容
// ============================================================================

func FileList(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("path not found: %w", err)
	}
	if !info.IsDir() {
		// 单文件信息
		return map[string]any{
			"path":   absPath,
			"is_dir": false,
			"size":   info.Size(),
			"mode":   info.Mode().String(),
		}, nil
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}

	// 默认最多 200 条
	maxEntries := 200
	if m, ok := args["max_entries"].(float64); ok && m > 0 {
		maxEntries = int(m)
	} else if m, ok := args["max_entries"].(int); ok && m > 0 {
		maxEntries = m
	}

	var items []map[string]any
	for i, entry := range entries {
		if i >= maxEntries {
			break
		}

		item := map[string]any{
			"name":   entry.Name(),
			"is_dir": entry.IsDir(),
		}

		if info, err := entry.Info(); err == nil {
			item["size"] = info.Size()
			item["mode"] = info.Mode().String()
			item["mod_time"] = info.ModTime().Format(time.RFC3339)
		}

		items = append(items, item)
	}

	return map[string]any{
		"path":      absPath,
		"count":     len(items),
		"truncated": len(entries) > maxEntries,
		"entries":   items,
	}, nil
}

// ============================================================================
// 注册
// ============================================================================

var fileReadParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"path": map[string]any{
			"type":        "string",
			"description": "文件路径（绝对或相对路径）",
		},
	},
	"required": []string{"path"},
}

var fileWriteParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"path": map[string]any{
			"type":        "string",
			"description": "文件路径（绝对或相对路径，父目录不存在时自动创建）",
		},
		"content": map[string]any{
			"type":        "string",
			"description": "要写入的内容",
		},
	},
	"required": []string{"path", "content"},
}

var fileListParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"path": map[string]any{
			"type":        "string",
			"description": "目录路径（默认当前目录）",
		},
		"max_entries": map[string]any{
			"type":        "number",
			"description": "最大返回条目数（默认 200）",
		},
	},
}

func RegisterFileTools(registry *ToolRegistry) {
	registry.MustRegister(ToolMetadata{
		Name:        "file_read",
		Description: "读取文件内容。返回文件的文本内容、大小、权限和修改时间。最大支持 10MB 文件。",
		Parameters:  fileReadParams,
		Permission:  PermReadOnly,
		Category:    "file",
		Version:     "1.0.0",
		Tags:        []string{"file", "read", "safe"},
		Timeout:     10 * time.Second,
	}, FileRead)

	registry.MustRegister(ToolMetadata{
		Name:        "file_write",
		Description: "写入文件内容。自动创建父目录。如果文件已存在则覆盖。",
		Parameters:  fileWriteParams,
		Permission:  PermReadWrite,
		Category:    "file",
		Version:     "1.0.0",
		Tags:        []string{"file", "write"},
		Timeout:     10 * time.Second,
	}, FileWrite)

	registry.MustRegister(ToolMetadata{
		Name:        "file_list",
		Description: "列出目录内容。返回每个文件/目录的名称、大小、权限和修改时间。",
		Parameters:  fileListParams,
		Permission:  PermReadOnly,
		Category:    "file",
		Version:     "1.0.0",
		Tags:        []string{"file", "list", "safe"},
		Timeout:     10 * time.Second,
	}, FileList)
}
