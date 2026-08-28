package skills

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// frontmatterFile 只解码业界字段；其余键由 yaml 忽略（不进结构体）。
type frontmatterFile struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license"`
	Compatibility string            `yaml:"compatibility"`
	Metadata      map[string]string `yaml:"metadata"`
	AllowedTools  string            `yaml:"allowed-tools"`
}

func splitFrontmatter(raw []byte) (yamlPart, body []byte, err error) {
	text := string(raw)
	// Accept UTF-8 BOM
	text = strings.TrimPrefix(text, "\ufeff")
	if !strings.HasPrefix(text, "---") {
		return nil, nil, fmt.Errorf("skills: missing YAML frontmatter opener")
	}
	rest := text[3:]
	// optional newline after opener
	if strings.HasPrefix(rest, "\r\n") {
		rest = rest[2:]
	} else if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	} else {
		return nil, nil, fmt.Errorf("skills: frontmatter opener must be on its own line")
	}
	// find closing ---
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return nil, nil, fmt.Errorf("skills: missing YAML frontmatter closer")
	}
	yamlPart = []byte(rest[:idx])
	after := rest[idx+len("\n---"):]
	after = strings.TrimPrefix(after, "\r")
	after = strings.TrimPrefix(after, "\n")
	body = []byte(after)
	return yamlPart, body, nil
}

func parseSkillFile(raw []byte, dirName string) (meta Meta, body string, err error) {
	yp, bd, err := splitFrontmatter(raw)
	if err != nil {
		return Meta{}, "", err
	}
	var fm frontmatterFile
	dec := yaml.NewDecoder(bytes.NewReader(yp))
	dec.KnownFields(false) // 私有/未知键忽略
	if err := dec.Decode(&fm); err != nil {
		return Meta{}, "", fmt.Errorf("skills: invalid frontmatter: %w", err)
	}
	if err := validateName(fm.Name); err != nil {
		return Meta{}, "", err
	}
	if err := validateDescription(fm.Description); err != nil {
		return Meta{}, "", err
	}
	if fm.Name != dirName {
		return Meta{}, "", fmt.Errorf("skills: name %q must match directory name %q", fm.Name, dirName)
	}
	if fm.Compatibility != "" && len(fm.Compatibility) > 500 {
		return Meta{}, "", fmt.Errorf("skills: compatibility exceeds 500 characters")
	}
	meta = Meta{
		Name:          fm.Name,
		Description:   fm.Description,
		License:       fm.License,
		Compatibility: fm.Compatibility,
		Metadata:      fm.Metadata,
		AllowedTools:  fm.AllowedTools,
	}
	return meta, string(bd), nil
}
