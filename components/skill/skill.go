package skill

import (
	"fmt"
	"time"

	"github.com/Luo-root/pulse/components/tools"
)

// Skill Agent Skill 定义
type Skill struct {
	// frontmatter 字段
	Name          string         `yaml:"name"`        // 必填，max 64 字符
	Description   string         `yaml:"description"` // 必填，max 1024 字符
	License       string         `yaml:"license"`
	Compatibility string         `yaml:"compatibility"`
	AllowedTools  []string       `yaml:"allowed-tools"`
	Metadata      map[string]any `yaml:"metadata"`
	Parameters    map[string]any `yaml:"parameters"`
	Category      string         `yaml:"category"`
	Tags          []string       `yaml:"tags"`
	Timeout       time.Duration  `yaml:"timeout"`

	// 指令正文（不含 frontmatter）
	Body string `yaml:"-"`

	// 运行时字段
	Path    string `yaml:"-"`
	EnvVars []string
}

// ToToolMetadata 转换为工具元数据
func (s *Skill) ToToolMetadata() tools.ToolMetadata {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return tools.ToolMetadata{
		Name:        s.Name,
		Description: s.Description,
		Parameters:  s.Parameters,
		Category:    s.Category,
		Version:     "1.0.0",
		Tags:        s.Tags,
		Timeout:     timeout,
	}
}

// IsValid 校验必填字段
func (s *Skill) IsValid() error {
	if s.Name == "" {
		return fmt.Errorf("skill name is required")
	}
	if s.Description == "" {
		return fmt.Errorf("skill description is required")
	}
	if len(s.Name) > 64 {
		return fmt.Errorf("skill name exceeds 64 characters")
	}
	if len(s.Description) > 1024 {
		return fmt.Errorf("skill description exceeds 1024 characters")
	}
	return nil
}
