package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ============================================================================
// file_edit — 精确字符串替换（增量修改）
// ============================================================================

func FileEdit(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}

	oldString, _ := args["old_string"].(string)
	if oldString == "" {
		return nil, fmt.Errorf("old_string is required")
	}

	newString, _ := args["new_string"].(string)

	replaceAll := false
	if v, ok := args["replace_all"].(bool); ok {
		replaceAll = v
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	content := string(data)

	if !strings.Contains(content, oldString) {
		return nil, fmt.Errorf("old_string not found in file")
	}

	var newContent string
	var replaceCount int

	if replaceAll {
		newContent = strings.ReplaceAll(content, oldString, newString)
		replaceCount = strings.Count(content, oldString)
	} else {
		// 只替换第一个匹配
		idx := strings.Index(content, oldString)
		newContent = content[:idx] + newString + content[idx+len(oldString):]
		replaceCount = 1
	}

	if err := os.WriteFile(absPath, []byte(newContent), 0644); err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}

	return map[string]any{
		"path":        absPath,
		"replaced":    replaceCount,
		"replace_all": replaceAll,
		"new_size":    len(newContent),
	}, nil
}

// ============================================================================
// file_search — 文件名匹配（glob）+ 内容搜索（grep）
// ============================================================================

func FileSearch(ctx context.Context, args map[string]any) (any, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	searchPath, _ := args["path"].(string)
	if searchPath == "" {
		searchPath = "."
	}

	absPath, err := filepath.Abs(searchPath)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	mode := "files" // 默认按文件名匹配
	if m, ok := args["mode"].(string); ok {
		mode = m
	}

	maxResults := 50
	if m, ok := args["max_results"].(float64); ok && m > 0 {
		maxResults = int(m)
	} else if m, ok := args["max_results"].(int); ok && m > 0 {
		maxResults = m
	}

	switch mode {
	case "content":
		return searchContent(absPath, pattern, maxResults)
	default:
		return searchFiles(absPath, pattern, maxResults)
	}
}

// searchFiles glob 模式匹配文件名（递归搜索）
func searchFiles(root, pattern string, maxResults int) (any, error) {
	var matches []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(matches) >= maxResults {
			return filepath.SkipAll
		}

		matched, _ := filepath.Match(pattern, info.Name())
		if matched {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(matches) > maxResults {
		matches = matches[:maxResults]
	}

	// 收集文件信息
	var results []map[string]any
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		results = append(results, map[string]any{
			"path":     m,
			"name":     info.Name(),
			"is_dir":   info.IsDir(),
			"size":     info.Size(),
			"mod_time": info.ModTime().Format(time.RFC3339),
		})
	}

	return map[string]any{
		"pattern":   pattern,
		"path":      root,
		"count":     len(results),
		"truncated": len(matches) >= maxResults,
		"results":   results,
	}, nil
}

// searchContent 在文件内容中搜索关键词
func searchContent(root, keyword string, maxResults int) (any, error) {
	var results []map[string]any

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			// 跳过隐藏目录和常见不需要搜索的目录
			name := info.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(results) >= maxResults {
			return filepath.SkipAll
		}

		// 跳过二进制文件（按扩展名判断）
		ext := strings.ToLower(filepath.Ext(path))
		skipExts := map[string]bool{
			".exe": true, ".dll": true, ".so": true, ".dylib": true,
			".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
			".mp3": true, ".mp4": true, ".wav": true, ".avi": true,
			".zip": true, ".tar": true, ".gz": true, ".rar": true,
			".pdf": true, ".doc": true, ".docx": true,
			".db": true, ".sqlite": true,
		}
		if skipExts[ext] {
			return nil
		}

		// 限制文件大小（最大 5MB）
		if info.Size() > 5*1024*1024 {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		lines := strings.Split(string(data), "\n")
		for lineNum, line := range lines {
			if len(results) >= maxResults {
				return filepath.SkipAll
			}
			if strings.Contains(line, keyword) {
				matchText := strings.TrimSpace(line)
				if len(matchText) > 200 {
					matchText = matchText[:200] + "..."
				}
				results = append(results, map[string]any{
					"path":    path,
					"line":    lineNum + 1,
					"content": matchText,
				})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// 按路径排序保证确定性
	sort.Slice(results, func(i, j int) bool {
		pi, _ := results[i]["path"].(string)
		pj, _ := results[j]["path"].(string)
		if pi != pj {
			return pi < pj
		}
		li, _ := results[i]["line"].(int)
		lj, _ := results[j]["line"].(int)
		return li < lj
	})

	return map[string]any{
		"keyword":   keyword,
		"path":      root,
		"count":     len(results),
		"truncated": len(results) >= maxResults,
		"results":   results,
	}, nil
}

// ============================================================================
// 注册
// ============================================================================

var fileEditParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"path": map[string]any{
			"type":        "string",
			"description": "文件路径",
		},
		"old_string": map[string]any{
			"type":        "string",
			"description": "要替换的原始文本（必须精确匹配）",
		},
		"new_string": map[string]any{
			"type":        "string",
			"description": "替换后的文本",
		},
		"replace_all": map[string]any{
			"type":        "boolean",
			"description": "是否替换所有匹配（默认 false，只替换第一个）",
		},
	},
	"required": []string{"path", "old_string", "new_string"},
}

var fileSearchParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"pattern": map[string]any{
			"type":        "string",
			"description": "搜索模式：文件名 glob（*.go）或内容关键词",
		},
		"path": map[string]any{
			"type":        "string",
			"description": "搜索根目录（默认当前目录）",
		},
		"mode": map[string]any{
			"type":        "string",
			"enum":        []string{"files", "content"},
			"description": "搜索模式：files=按文件名匹配（默认），content=按文件内容搜索",
		},
		"max_results": map[string]any{
			"type":        "number",
			"description": "最大返回结果数（默认 50）",
		},
	},
	"required": []string{"pattern"},
}

func RegisterFileEditTools(registry *ToolRegistry) {
	registry.MustRegister(ToolMetadata{
		Name: "file_edit",
		Description: "精确替换文件中的文本片段。" +
			"old_string 必须与文件中的内容完全匹配（包括缩进和换行）。" +
			"适合对文件做小范围修改，无需重写整个文件。",
		Parameters: fileEditParams,
		Permission: PermReadWrite,
		Category:   "file",
		Version:    "1.0.0",
		Tags:       []string{"file", "edit", "modify"},
		Timeout:    10 * time.Second,
	}, FileEdit)

	registry.MustRegister(ToolMetadata{
		Name: "file_search",
		Description: "搜索文件。支持两种模式：" +
			"files 模式按文件名 glob 匹配（如 *.go、**/*.md）；" +
			"content 模式按文件内容搜索关键词（跳过二进制文件和隐藏目录）。",
		Parameters: fileSearchParams,
		Permission: PermReadOnly,
		Category:   "file",
		Version:    "1.0.0",
		Tags:       []string{"file", "search", "grep", "glob", "safe"},
		Timeout:    30 * time.Second,
	}, FileSearch)
}
