package builtins

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveUnderRoot 将用户路径解析为绝对路径（相对则相对 Root）。
// 不在此处做越界拒绝——读/写各自用 confine*。
func resolveUnderRoot(root, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("builtins: empty path")
	}
	if strings.Contains(p, "\x00") {
		return "", fmt.Errorf("builtins: invalid path")
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
	return resolveSymlinks(abs)
}

// resolveSymlinks 把 path 解析到 symlink 最终落点。
// 目标尚不存在时：对最深已存在祖先 EvalSymlinks，再拼回缺失后缀，
// 避免「Root 内的 link/new.txt」实际写出 Root 外。
func resolveSymlinks(abs string) (string, error) {
	if eval, err := filepath.EvalSymlinks(abs); err == nil {
		return eval, nil
	}
	exist := deepestExisting(abs)
	evalExist, err := filepath.EvalSymlinks(exist)
	if err != nil {
		return abs, nil
	}
	rel, err := filepath.Rel(exist, abs)
	if err != nil {
		return abs, nil
	}
	return filepath.Join(evalExist, rel), nil
}

func withinRoot(root, abs string) bool {
	sep := string(filepath.Separator)
	if abs == root {
		return true
	}
	return strings.HasPrefix(abs, root+sep)
}

func withinAny(roots []string, abs string) bool {
	for _, r := range roots {
		if withinRoot(r, abs) {
			return true
		}
	}
	return false
}

func confineRead(root string, forbid []string, abs string) error {
	if !withinRoot(root, abs) {
		// 读允许绝对路径落在 Root 外？P0：默认必须在 Root 内。
		return fmt.Errorf("builtins: path escapes Root: %s", abs)
	}
	for _, f := range forbid {
		if withinRoot(f, abs) {
			return fmt.Errorf("builtins: path is forbid-read: %s", abs)
		}
	}
	return nil
}

func confineWrite(writeRoots []string, abs string) error {
	if !withinAny(writeRoots, abs) {
		return fmt.Errorf("builtins: path outside WriteRoots: %s", abs)
	}
	return nil
}

// deepestExisting 返回 path 上最深的已存在祖先（含自身），用于 EvalSymlinks。
func deepestExisting(path string) string {
	cur := path
	for {
		if _, err := os.Lstat(cur); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur
		}
		cur = parent
	}
}
