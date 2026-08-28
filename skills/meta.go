package skills

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Meta 是 Discovery 阶段暴露给上下文的最小集。
type Meta struct {
	Name        string
	Description string
	Dir         string // 绝对路径：该 skill 根目录
	// Compatibility 原样保留；v1 不强制过滤。
	Compatibility string
	// License 原样保留。
	License string
	// Metadata 仅含 frontmatter metadata 映射（string→string）。
	Metadata map[string]string
	// AllowedTools 不透明声明；本包不解析。
	AllowedTools string
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
