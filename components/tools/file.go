package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var SafeBaseDir = "." // 可在 init 时修改，比如 tools.SafeBaseDir = "/home/user/workspace"

// safePath 安全路径检查：限制在工作目录内
//
// 安全策略：
//  1. 解析 baseDir 和 userPath 为绝对路径
//  2. 检查 userPath 是否在 baseDir 之下（通过路径前缀匹配）
//  3. 处理符号链接：解析所有符号链接后再次检查
//  4. 清理路径中的 .. 和 . 后再比较
//
// Windows 特殊处理：
//   - 统一使用 filepath.Clean 规范化路径
//   - 比较时添加路径分隔符后缀，防止前缀误判（如 /foo 匹配 /foobar）
//   - 支持不同盘符（C: vs D:）的检测
func safePath(baseDir, userPath string) (string, error) {
	// 1. 获取 baseDir 的绝对路径
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve base dir failed: %w", err)
	}
	absBase = filepath.Clean(absBase)

	// 2. 处理 userPath
	// 如果是相对路径（不以 / 或 盘符开头），先将其与 baseDir 拼接
	// 这样 "./file.txt" 或 "." 会被解析为 baseDir 下的路径
	var absUser string
	if !filepath.IsAbs(userPath) {
		// 相对路径：基于 baseDir 解析
		absUser = filepath.Join(absBase, userPath)
	} else {
		// 绝对路径：直接使用
		absUser = userPath
	}
	absUser = filepath.Clean(absUser)

	// 3. 解析 userPath 中的所有符号链接，获取真实路径
	realUser, err := filepath.EvalSymlinks(absUser)
	if err != nil {
		// 如果文件不存在，EvalSymlinks 会报错
		// 这种情况下我们使用 absUser，但在创建前会再次检查
		realUser = absUser
	} else {
		realUser = filepath.Clean(realUser)
	}

	// 4. 解析 baseDir 中的符号链接
	realBase, err := filepath.EvalSymlinks(absBase)
	if err != nil {
		realBase = absBase
	} else {
		realBase = filepath.Clean(realBase)
	}

	// 5. 检查路径是否在 baseDir 之下
	// 方法：确保目标路径以 baseDir + 分隔符 开头，或者是 baseDir 本身
	if !isSubPath(realBase, realUser) {
		return "", fmt.Errorf("path access denied: %s (must be under %s)", userPath, realBase)
	}

	// 6. 额外检查：防止通过 .. 绕过（虽然 Clean 已经处理，但双重保险）
	// 检查原始路径中是否包含 .. 序列
	if strings.Contains(userPath, "..") {
		// 允许正常的相对路径如 "subdir/file.txt"
		// 但拒绝 "../../../etc/passwd" 这种明显的越界尝试
		// 由于已经做了绝对路径检查，这里主要是防御性的
		cleaned := filepath.Clean(userPath)
		if strings.HasPrefix(cleaned, "..") {
			return "", fmt.Errorf("path access denied: relative path escapes base dir: %s", userPath)
		}
	}

	return realUser, nil
}

// isSubPath 检查 child 是否是 parent 的子路径（或等于 parent）
// 使用路径分隔符确保精确匹配，防止前缀误判
func isSubPath(parent, child string) bool {
	// 如果两者相等，允许访问
	if child == parent {
		return true
	}

	// Windows 下不区分大小写
	if strings.EqualFold(child, parent) {
		return true
	}

	// 统一添加尾部路径分隔符，确保 /foo 不会匹配 /foobar
	parentWithSep := parent + string(filepath.Separator)
	childWithSep := child + string(filepath.Separator)

	// 检查 child 是否以 parent+sep 开头
	// Windows 下不区分大小写
	if strings.HasPrefix(strings.ToLower(childWithSep), strings.ToLower(parentWithSep)) {
		return true
	}

	return false
}

// FileRead 读取文件（带安全限制+大小限制）
func FileRead(ctx context.Context, args map[string]any) (any, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return nil, fmt.Errorf("path must be a non-empty string")
	}

	// 安全路径检查
	safePath, err := safePath(SafeBaseDir, path)
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
	safePath, err := safePath(SafeBaseDir, path)
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
	safeDir, err := safePath(SafeBaseDir, dir)
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

// fileReadParams file_read 参数定义
var fileReadParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"path": map[string]any{"type": "string", "description": "文件路径（必填）"},
	},
	"required": []string{"path"},
}

// fileWriteParams file_write 参数定义
var fileWriteParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"path":    map[string]any{"type": "string", "description": "文件路径（必填）"},
		"content": map[string]any{"type": "string", "description": "文件内容（必填）"},
	},
	"required": []string{"path", "content"},
}

// fileListParams file_list 参数定义
var fileListParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"path": map[string]any{"type": "string", "description": "目录路径（可选，默认为当前目录）"},
	},
}

// RegisterFileTools 注册文件工具
func RegisterFileTools(registry *ToolRegistry) {
	registry.MustRegister(ToolMetadata{
		Name:        "file_read",
		Description: "读取文件内容，最大10MB",
		Parameters:  fileReadParams,
		Permission:  PermReadOnly,
		Category:    "file",
		Version:     "1.0.0",
		Tags:        []string{"file", "read", "safe"},
		Timeout:     30 * time.Second,
	}, FileRead)

	registry.MustRegister(ToolMetadata{
		Name:        "file_write",
		Description: "写入内容到文件，自动创建父目录",
		Parameters:  fileWriteParams,
		Permission:  PermReadWrite,
		Category:    "file",
		Version:     "1.0.0",
		Tags:        []string{"file", "write"},
		Timeout:     30 * time.Second,
	}, FileWrite)

	registry.MustRegister(ToolMetadata{
		Name:        "file_list",
		Description: "列出目录下的文件和文件夹",
		Parameters:  fileListParams,
		Permission:  PermReadOnly,
		Category:    "file",
		Version:     "1.0.0",
		Tags:        []string{"file", "list", "safe"},
		Timeout:     30 * time.Second,
	}, FileList)
}
