package skills

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Meta 是扫描后的 skill 记录（宿主侧完整信息）。
// Catalog/披露给模型时通常只取 Name + Description；
// Location / Dir 供激活与相对路径解析（见 agentskills.io client guide）。
type Meta struct {
	Name        string
	Description string
	// Location 是 SKILL.md 的绝对路径。
	Location string
	// Dir 是 skill 根目录（Location 的父目录），用于解析相对路径。
	Dir string
	// Compatibility 原样保留；v1 不强制过滤。
	Compatibility string
	// License 原样保留。
	License string
	// Metadata 仅含 frontmatter metadata 映射（string→string）。
	Metadata map[string]string
	// AllowedTools 不透明声明；本包不解析。
	AllowedTools string
}

// Content 是激活结果（tier 2）：正文 + 目录 + 可选资源清单。
// 目录给模型/命令行工具解析 SKILL.md 内相对路径；不必再造专用 script 工具。
type Content struct {
	Name      string   `json:"name"`
	Body      string   `json:"body"`                // 已剥 frontmatter 的 Markdown
	Directory string   `json:"directory"`           // skill 根目录（绝对路径）
	Location  string   `json:"location"`            // SKILL.md 绝对路径
	Resources []string `json:"resources,omitempty"` // 相对 skill 根的资源路径（不预读内容）
}

// CatalogEntry 是披露给模型的最小集（tier 1）。
type CatalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Catalog 从 Meta 列表抽出 name+description，供 system 短表 / list_skills 使用。
func Catalog(metas []Meta) []CatalogEntry {
	out := make([]CatalogEntry, 0, len(metas))
	for _, m := range metas {
		out = append(out, CatalogEntry{Name: m.Name, Description: m.Description})
	}
	return out
}

var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("skills: name is required")
	}
	if utf8.RuneCountInString(name) > 64 {
		return fmt.Errorf("skills: name %q exceeds 64 characters", name)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("skills: name %q must not start or end with a hyphen", name)
	}
	if strings.Contains(name, "--") {
		return fmt.Errorf("skills: name %q must not contain consecutive hyphens", name)
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("skills: name %q must be lowercase alphanumeric with single hyphens", name)
	}
	return nil
}

func validateDescription(desc string) error {
	if strings.TrimSpace(desc) == "" {
		return fmt.Errorf("skills: description is required")
	}
	if utf8.RuneCountInString(desc) > 1024 {
		return fmt.Errorf("skills: description exceeds 1024 characters")
	}
	return nil
}
