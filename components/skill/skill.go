package skill

import (
	"fmt"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// SkillType 技能类型
type SkillType int

const (
	SkillTypeCode        SkillType = iota // 可执行代码型（默认）
	SkillTypeInstruction                  // 纯指令型
)

// Skill 标准 Agent Skill 定义（对齐 Agent Skills Open Specification）
type Skill struct {
	// 标准 frontmatter 字段
	Name          string         `yaml:"name"`          // 必填，max 64 字符，小写+连字符
	Description   string         `yaml:"description"`   // 必填，max 1024 字符
	License       string         `yaml:"license"`       // 可选
	Compatibility string         `yaml:"compatibility"` // 可选，max 500 字符
	AllowedTools  []string       `yaml:"allowed-tools"` // 可选，空格分隔
	Metadata      map[string]any `yaml:"metadata"`      // 可选，扩展字段

	// 参数定义（JSON Schema）
	Parameters map[string]any `yaml:"parameters"`

	// 自定义扩展
	Category string        `yaml:"category"`
	Tags     []string      `yaml:"tags"`
	Timeout  time.Duration `yaml:"timeout"`

	// 运行时字段（不从 frontmatter 解析）
	Path    string             `yaml:"-"` // Skill 目录路径
	Handler schema.ToolHandler `yaml:"-"` // 编译后的处理函数

	// Skill 类型（代码型 / 指令型）
	Type SkillType `yaml:"-"`

	// 纯指令 Skill 的正文内容（不含 frontmatter）
	Body    string   `yaml:"-"`
	EnvVars []string `yaml:"-"` // 新增：Skill 运行时需要的环境变量列表
}

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

// IsValid 校验标准规范必填字段
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
