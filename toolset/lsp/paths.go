package lsp

import (
	"fmt"
	"path/filepath"
	"strings"
)

// resolveUnderRoot 将相对路径解析到 Root 下（绝对路径必须已在 Root 内），
// 并检查 symlink 最终落点，防「Root 内的 link」实际指向 Root 外。
func resolveUnderRoot(root, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("lsp: empty path")
	}
	var joined string
	if filepath.IsAbs(p) {
		joined = filepath.Clean(p)
	} else {
		joined = filepath.Join(root, filepath.Clean(p))
	}
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	if !withinRoot(root, abs) {
		return "", fmt.Errorf("lsp: path escapes Root: %s", abs)
	}
	if eval, err := filepath.EvalSymlinks(abs); err == nil && eval != abs {
		if !withinRoot(root, eval) {
			return "", fmt.Errorf("lsp: path escapes Root via symlink: %s -> %s", abs, eval)
		}
		abs = eval
	}
	return abs, nil
}

func withinRoot(root, abs string) bool {
	sep := string(filepath.Separator)
	if abs == root {
		return true
	}
	return strings.HasPrefix(abs, root+sep)
}
