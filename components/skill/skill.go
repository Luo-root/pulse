package skill

import (
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// Skill 定义
type Skill struct {
	Name        string             `yaml:"name"`
	Description string             `yaml:"description"`
	Category    string             `yaml:"category"`
	Timeout     time.Duration      `yaml:"timeout"`
	Tags        []string           `yaml:"tags"`
	Parameters  map[string]any     `yaml:"parameters"`
	Handler     schema.ToolHandler `yaml:"-"` // 从代码块解析
}

// ToToolMetadata 转换为 ToolMetadata
func (s *Skill) ToToolMetadata() schema.ToolMetadata {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return schema.ToolMetadata{
		Name:        s.Name,
		Description: s.Description,
		Parameters:  s.Parameters,
		Category:    s.Category,
		Version:     "1.0.0",
		Tags:        s.Tags,
		Timeout:     timeout,
	}
}
