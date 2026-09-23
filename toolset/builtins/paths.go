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

// maxSymlinkDepth 限制手工解析链接的层数：取 Linux ELOOP（40）的量级——
// 比内核更有耐心只会让成环/超长链在用户态多烧时间，而内核本来也解析不了。
const maxSymlinkDepth = 40

// resolveSymlinks 把 path 解析到 symlink 最终落点。
// 目标尚不存在时：对最深已存在祖先 EvalSymlinks，再拼回缺失后缀，
// 避免「Root 内的 link/new.txt」实际写出 Root 外。
//
// 最深已存在祖先是链接时（悬空、成环都算——Lstat 不跟随，所以链接自身
// 就是「已存在」），按链接文本自己解析：不能原样返回未解析路径（那样
// confine* 判的是链接路径本身，os.WriteFile 会跟随链接写到 Root 外，
// #213）。
//
// **不在这里调 filepath.EvalSymlinks(abs)**：成环路径上它会退化到自己的
// 255 步上限才失败（实测单次约 150ms），把它放进按链接数循环的热路径，
// 会把一次系统调用级快失败放大成几十秒（#213 review 实测 37.98s）。
// 链接用 Readlink 自己走（`seen` 记已解析的链接，重复即环；自指链接还会
// 被「解析结果没前进」当场拦下），只有「最深已存在祖先不是链接」时才把
// 祖先链交给 EvalSymlinks 一次解析完——那条路上不可能有成环（否则其下
// 的路径根本不存在）。
func resolveSymlinks(abs string) (string, error) {
	seen := make(map[string]bool)
	for i := 0; i < maxSymlinkDepth; i++ {
		exist := deepestExisting(abs)
		if fi, err := os.Lstat(exist); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if seen[exist] {
				return "", fmt.Errorf("builtins: symlink cycle at %s", exist)
			}
			seen[exist] = true
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
			next := filepath.Join(target, rel)
			if next == abs { // 自指：解析回自身，再转也不前进
				return "", fmt.Errorf("builtins: symlink cycle at %s", exist)
			}
			abs = next
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

// canonRoot 规范化配置里的根路径：先 Abs，再解析链接**自身**。
//
// Root / WriteRoots / ForbidRead 只做 Abs 是不够的：resolveSymlinks 交给
// confine* 的是解析后的真实路径，若前缀还留着未解析的链接，withinRoot 的
// 前缀比较必然失败——Root 是 symlink 时读写全被判越界（macOS 的 /tmp、
// /var 都是链接，t.TempDir() 正落在 /var/folders/…；CI 只跑 ubuntu，看不见
// 这一档）。不存在的路径由 resolveSymlinks 解析到「最深已存在祖先 + 缺失
// 后缀」，与运行时同一套口径。
func canonRoot(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return resolveSymlinks(abs)
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
