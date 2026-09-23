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

// maxSymlinkDepth 限制手工解析链接的层数（与 filepath.EvalSymlinks 的上限
// 同量级）：链接成环时在此中止并报错，而不是无限解析。
const maxSymlinkDepth = 255

// resolveSymlinks 把 path 解析到 symlink 最终落点。
// 目标尚不存在时：对最深已存在祖先 EvalSymlinks，再拼回缺失后缀，
// 避免「Root 内的 link/new.txt」实际写出 Root 外。
//
// 最深已存在祖先是**悬空链接**时（EvalSymlinks 失败但 Lstat 报链接），
// 按链接文本继续解析——不能原样返回未解析路径：那样 confine* 判定的是
// 链接路径本身，而 os.WriteFile 会跟随链接写到 Root 外（#213）。
func resolveSymlinks(abs string) (string, error) {
	for i := 0; i < maxSymlinkDepth; i++ {
		if eval, err := filepath.EvalSymlinks(abs); err == nil {
			return eval, nil
		}
		exist := deepestExisting(abs)
		if fi, err := os.Lstat(exist); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(exist)
			if err != nil {
				return "", fmt.Errorf("builtins: read symlink %s: %w", exist, err)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(exist), target)
			}
			rel, err := filepath.Rel(exist, abs)
			if err != nil {
				return "", fmt.Errorf("builtins: resolve symlink %s: %w", exist, err)
			}
			abs = filepath.Join(target, rel)
			continue // 链接指向的路径本身可能还有链接
		}
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
	return "", fmt.Errorf("builtins: too many levels of symbolic links: %s", abs)
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
//
// 用 os.Lstat（**不跟随**链接）：悬空链接自身算「已存在」，交给
// resolveSymlinks 按链接文本继续解析；若改用 os.Stat，悬空链接会被跳过，
// 从而漏掉「链接指向 Root 外」这一档。
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
