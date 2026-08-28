package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Loader 是 Skills 装载面（设计示意接口的实现契约）。
type Loader interface {
	List(ctx context.Context) ([]Meta, error)
	Load(ctx context.Context, name string) (body string, err error)
	ReadFile(ctx context.Context, name, rel string) ([]byte, error)
}

// FSLoader 从本地目录树装载 skills。root 下每个子目录若含 SKILL.md 则视为一个 skill。
type FSLoader struct {
	root string

	mu    sync.RWMutex
	index map[string]indexed // name → entry
}

type indexed struct {
	meta Meta
	body string // Open/rescan 时缓存的正文；Load 只返回快照
	file string // SKILL.md 绝对路径（ReadFile 锚定 Dir 用）
}

// Open 扫描 root 并返回 Loader。
// 非法 skill 目录（有 SKILL.md 但 frontmatter 不合法）会导致整个 Open 失败——
// 装配期尽早暴露；无 SKILL.md 的子目录会被跳过。
// 技能文件变更默认下次 Open（或未来显式 Rescan）才生效：List 与 Load 共用扫描快照。
func Open(root string) (*FSLoader, error) {
	if root == "" {
		return nil, fmt.Errorf("skills: root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("skills: resolve root: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("skills: root: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("skills: root is not a directory: %s", abs)
	}
	l := &FSLoader{root: abs, index: make(map[string]indexed)}
	if err := l.rescan(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *FSLoader) rescan() error {
	entries, err := os.ReadDir(l.root)
	if err != nil {
		return fmt.Errorf("skills: read root: %w", err)
	}
	next := make(map[string]indexed)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dirName := e.Name()
		skillDir := filepath.Join(l.root, dirName)
		skillFile := filepath.Join(skillDir, "SKILL.md")
		raw, err := os.ReadFile(skillFile)
		if err != nil {
			if os.IsNotExist(err) {
				continue // 无 SKILL.md 的子目录跳过
			}
			return fmt.Errorf("skills: read %s: %w", skillFile, err)
		}
		meta, body, err := parseSkillFile(raw, dirName)
		if err != nil {
			return fmt.Errorf("skills: %s: %w", dirName, err)
		}
		if _, dup := next[meta.Name]; dup {
			return fmt.Errorf("skills: duplicate name %q", meta.Name)
		}
		meta.Dir = skillDir
		next[meta.Name] = indexed{meta: meta, body: body, file: skillFile}
	}
	l.mu.Lock()
	l.index = next
	l.mu.Unlock()
	return nil
}

// List 实现 Loader：按 name 字典序返回快照。
func (l *FSLoader) List(ctx context.Context) ([]Meta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Meta, 0, len(l.index))
	for _, e := range l.index {
		out = append(out, e.meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Load 实现 Loader：返回 Open/rescan 时缓存的 Markdown 正文（已剥 frontmatter）。
// 不重读磁盘，与 List 共用同一快照——符合「下次扫描生效」。
func (l *FSLoader) Load(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	e, err := l.lookup(name)
	if err != nil {
		return "", err
	}
	return e.body, nil
}

// ReadFile 实现 Loader：读取 skill 目录内相对路径。
func (l *FSLoader) ReadFile(ctx context.Context, name, rel string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e, err := l.lookup(name)
	if err != nil {
		return nil, err
	}
	target, err := safeJoin(e.meta.Dir, rel)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(target)
	if err != nil {
		return nil, fmt.Errorf("skills: read %s/%s: %w", name, rel, err)
	}
	return b, nil
}

func (l *FSLoader) lookup(name string) (indexed, error) {
	if name == "" {
		return indexed{}, fmt.Errorf("skills: empty skill name")
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, ok := l.index[name]
	if !ok {
		return indexed{}, fmt.Errorf("skills: unknown skill %q", name)
	}
	return e, nil
}

func safeJoin(skillDir, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("skills: empty relative path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("skills: absolute path rejected")
	}
	// Normalize and reject parent traversal.
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("skills: path escapes skill directory")
	}
	if strings.Contains(rel, "\x00") {
		return "", fmt.Errorf("skills: invalid path")
	}
	joined := filepath.Join(skillDir, clean)
	// Ensure joined is still under skillDir.
	skillAbs, err := filepath.Abs(skillDir)
	if err != nil {
		return "", err
	}
	joinedAbs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	sep := string(filepath.Separator)
	if joinedAbs != skillAbs && !strings.HasPrefix(joinedAbs, skillAbs+sep) {
		return "", fmt.Errorf("skills: path escapes skill directory")
	}
	return joinedAbs, nil
}

var _ Loader = (*FSLoader)(nil)
