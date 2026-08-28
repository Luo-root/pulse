package skills

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// maxListedResources 限制激活时枚举的资源数量，避免巨大 skill 目录刷爆上下文。
const maxListedResources = 64

// Loader 是 Skills 装载面。
type Loader interface {
	List(ctx context.Context) ([]Meta, error)
	// Load 返回结构化激活结果（正文 + 目录 + 资源首页），不执行脚本。
	Load(ctx context.Context, name string) (Content, error)
	// ListResources 按字典序分页列出 skill 内资源相对路径。
	// after 为空从首页开始；非空则返回严格大于 after 的下一页。
	// limit<=0 时使用默认页大小。
	ListResources(ctx context.Context, name, after string, limit int) (ResourcePage, error)
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
		meta.Location = skillFile
		next[meta.Name] = indexed{meta: meta, body: body}
	}
	l.mu.Lock()
	l.index = next
	l.mu.Unlock()
	return nil
}

// List 实现 Loader：按 name 字典序返回快照（宿主完整 Meta）。
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

// Load 实现 Loader：返回结构化激活结果。
// Body 来自扫描快照；Resources 为排序后的首页（不预读文件内容）。
func (l *FSLoader) Load(ctx context.Context, name string) (Content, error) {
	if err := ctx.Err(); err != nil {
		return Content{}, err
	}
	e, err := l.lookup(name)
	if err != nil {
		return Content{}, err
	}
	page, err := l.listResourcePage(e.meta.Dir, "", maxListedResources)
	if err != nil {
		return Content{}, fmt.Errorf("skills: list resources %q: %w", name, err)
	}
	return Content{
		Name:          e.meta.Name,
		Body:          e.body,
		Directory:     e.meta.Dir,
		Location:      e.meta.Location,
		Resources:     page.Resources,
		ResourcesNext: page.Next,
	}, nil
}

// ListResources 实现 Loader：资源相对路径分页。
func (l *FSLoader) ListResources(ctx context.Context, name, after string, limit int) (ResourcePage, error) {
	if err := ctx.Err(); err != nil {
		return ResourcePage{}, err
	}
	e, err := l.lookup(name)
	if err != nil {
		return ResourcePage{}, err
	}
	if limit <= 0 {
		limit = maxListedResources
	}
	page, err := l.listResourcePage(e.meta.Dir, after, limit)
	if err != nil {
		return ResourcePage{}, fmt.Errorf("skills: list resources %q: %w", name, err)
	}
	return page, nil
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

func (l *FSLoader) listResourcePage(skillDir, after string, limit int) (ResourcePage, error) {
	all, err := collectResources(skillDir)
	if err != nil {
		return ResourcePage{}, err
	}
	return pageResources(all, after, limit), nil
}

func collectResources(skillDir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(skillDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(skillDir, path)
		if err != nil {
			return err
		}
		if rel == "SKILL.md" {
			return nil
		}
		// 统一用斜杠，方便进模型上下文 / 稳定排序
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// pageResources 在已排序全集上切片；Next 为当前页最后一项（供 after 游标）。
func pageResources(all []string, after string, limit int) ResourcePage {
	if limit <= 0 {
		limit = maxListedResources
	}
	start := 0
	if after != "" {
		start = sort.Search(len(all), func(i int) bool { return all[i] > after })
	}
	if start >= len(all) {
		return ResourcePage{}
	}
	end := start + limit
	if end >= len(all) {
		return ResourcePage{Resources: append([]string(nil), all[start:]...)}
	}
	page := append([]string(nil), all[start:end]...)
	return ResourcePage{Resources: page, Next: page[len(page)-1]}
}

func safeJoin(skillDir, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("skills: empty relative path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("skills: absolute path rejected")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("skills: path escapes skill directory")
	}
	if strings.Contains(rel, "\x00") {
		return "", fmt.Errorf("skills: invalid path")
	}
	joined := filepath.Join(skillDir, clean)
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
