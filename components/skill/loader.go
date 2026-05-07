package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	tools "github.com/Luo-root/pulse/components/tools"
	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
	"gopkg.in/yaml.v3"
)

// ============================================================================
// Markdown 解析
// ============================================================================

// SkillFrontmatter 对应 SKILL.md 的 YAML frontmatter
type SkillFrontmatter struct {
	Name          string         `yaml:"name"`
	Description   string         `yaml:"description"`
	License       string         `yaml:"license"`
	Compatibility string         `yaml:"compatibility"`
	AllowedTools  string         `yaml:"allowed-tools"` // YAML 中可写空格分隔
	Metadata      map[string]any `yaml:"metadata"`
	Parameters    map[string]any `yaml:"parameters"`
	// 自定义扩展
	Category string   `yaml:"category"`
	Tags     []string `yaml:"tags"`
	Timeout  int      `yaml:"timeout"` // 秒
}

// CodeBlock 代码块
type CodeBlock struct {
	Language string
	Code     string
}

// ParseSkillMarkdown 解析 Skill Markdown 文档
func ParseSkillMarkdown(content string) (*Skill, error) {
	fm, body, err := parseFrontmatter(content)
	if err != nil {
		return nil, fmt.Errorf("parse frontmatter failed: %w", err)
	}

	codeBlocks := extractCodeBlocks(body)
	var handler schema.ToolHandler

	if len(codeBlocks) > 0 {
		for _, cb := range codeBlocks {
			if cb.Language == "go" || cb.Language == "golang" {
				handler, err = compileHandler(cb.Code)
				if err != nil {
					return nil, fmt.Errorf("compile handler failed: %w", err)
				}
				break
			}
		}
	}

	timeout := time.Duration(fm.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	// 解析 allowed-tools
	var allowedTools []string
	if fm.AllowedTools != "" {
		allowedTools = strings.Fields(fm.AllowedTools)
	}

	// 如果 frontmatter 没写 parameters，给默认值
	params := fm.Parameters
	if params == nil {
		params = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}

	skill := &Skill{
		Name:          fm.Name,
		Description:   fm.Description,
		License:       fm.License,
		Compatibility: fm.Compatibility,
		AllowedTools:  allowedTools,
		Metadata:      fm.Metadata,
		Parameters:    params,
		Category:      fm.Category,
		Tags:          fm.Tags,
		Timeout:       timeout,
		Handler:       handler,
	}

	// 判断类型
	if skill.Handler != nil {
		skill.Type = SkillTypeCode
	} else {
		skill.Type = SkillTypeInstruction
		skill.Body = body
	}

	if err := skill.IsValid(); err != nil {
		return nil, err
	}

	return skill, nil
}

// parseFrontmatter 解析 YAML Frontmatter
func parseFrontmatter(content string) (*SkillFrontmatter, string, error) {
	if !strings.HasPrefix(content, "---") {
		return nil, content, fmt.Errorf("no frontmatter found")
	}

	// 找到第二个 ---
	endIdx := strings.Index(content[3:], "---")
	if endIdx == -1 {
		return nil, content, fmt.Errorf("frontmatter not closed")
	}
	endIdx += 3 // 加上前面的 ---

	// 解析 YAML
	var fm SkillFrontmatter
	if err := yaml.Unmarshal([]byte(content[3:endIdx]), &fm); err != nil {
		return nil, content, fmt.Errorf("parse yaml failed: %w", err)
	}

	body := strings.TrimSpace(content[endIdx+3:])
	return &fm, body, nil
}

// extractCodeBlocks 提取 Markdown 代码块
func extractCodeBlocks(content string) []CodeBlock {
	var blocks []CodeBlock

	// 正则匹配 ```lang\ncode\n```
	// 使用 (?s) 标志使 . 匹配换行符
	re := regexp.MustCompile("(?s)```(\\w+)?\\n(.*?)\\n```")
	matches := re.FindAllStringSubmatch(content, -1)

	for _, match := range matches {
		lang := ""
		if len(match) > 1 {
			lang = match[1]
		}
		code := ""
		if len(match) > 2 {
			code = strings.TrimSpace(match[2])
		}
		blocks = append(blocks, CodeBlock{
			Language: lang,
			Code:     code,
		})
	}

	return blocks
}

// ============================================================================
// Handler 编译（使用 yaegi 解释器）
// ============================================================================

// compileHandler 编译 Go 代码为 ToolHandler
// 使用 yaegi 解释器执行 Skill 代码，支持动态加载和沙箱隔离
func compileHandler(code string) (schema.ToolHandler, error) {
	i := interp.New(interp.Options{})
	i.Use(stdlib.Symbols)

	var evalCode string
	if strings.HasPrefix(strings.TrimSpace(code), "package ") {
		// 完整文件模式：用户自己写 package skill 等
		evalCode = code
	} else {
		// 函数体模式：自动包装
		evalCode = fmt.Sprintf(`
package skill

import (
    "context"
    "fmt"
)

func Handler(ctx context.Context, args map[string]any) (any, error) {
%s
}
`, code)
	}

	_, err := i.Eval(evalCode)
	if err != nil {
		return nil, fmt.Errorf("compile skill code failed: %w", err)
	}

	v, err := i.Eval("skill.Handler")
	if err != nil {
		return nil, fmt.Errorf("get handler function failed: %w", err)
	}

	handler, ok := v.Interface().(func(context.Context, map[string]any) (any, error))
	if !ok {
		return nil, fmt.Errorf("handler type mismatch")
	}

	return func(ctx context.Context, args map[string]any) (any, error) {
		return handler(ctx, args)
	}, nil
}

// ============================================================================
// SkillLoader Skill 加载器
// ============================================================================

// SkillLoader Skill 加载器
type SkillLoader struct {
	loader       *tools.DynamicToolLoader
	registry     *SkillRegistry
	toolRegistry *schema.ToolRegistry
}

func NewSkillLoader(registry *SkillRegistry, toolRegistry *schema.ToolRegistry) *SkillLoader {
	return &SkillLoader{
		loader:       tools.NewDynamicToolLoader(toolRegistry),
		registry:     registry,
		toolRegistry: toolRegistry,
	}
}

func (sl *SkillLoader) Register(skill Skill) error {
	if err := skill.IsValid(); err != nil {
		return fmt.Errorf("invalid skill %s: %w", skill.Name, err)
	}
	sl.registry.Register(&skill)

	var handler schema.ToolHandler
	if skill.Type == SkillTypeCode {
		handler = skill.Handler
	} else {
		// 指令型 Skill：调用时返回完整的 Markdown 正文
		body := skill.Body
		handler = func(ctx context.Context, args map[string]any) (any, error) {
			return body, nil
		}
	}

	if handler != nil {
		return sl.loader.Load(
			skill.Name,
			skill.Description,
			handler,
			skill.Parameters,
			tools.WithCategory(skill.Category),
			tools.WithTimeout(skill.Timeout),
			tools.WithTags(skill.Tags...),
		)
	}
	return nil
}

func (sl *SkillLoader) Unload(name string) error {
	// 从 registry 移除
	sl.registry.Remove(name)
	// 从 ToolRegistry 注销
	return sl.toolRegistry.Unregister(name)
}

// LoadFromFile 从 Markdown 文件加载 Skill
func (sl *SkillLoader) LoadFromFile(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read skill file failed: %w", err)
	}

	return sl.LoadFromString(string(content))
}

// LoadFromString 从字符串加载 Skill
func (sl *SkillLoader) LoadFromString(content string) error {
	skill, err := ParseSkillMarkdown(content)
	if err != nil {
		return err
	}

	return sl.Register(*skill)
}

// LoadFromDir 从目录加载标准 Skill 目录结构
func (sl *SkillLoader) LoadFromDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read skill dir failed: %w", err)
	}

	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillDir := filepath.Join(dir, entry.Name())
		skillFile := filepath.Join(skillDir, "SKILL.md")
		if _, err := os.Stat(skillFile); os.IsNotExist(err) {
			// 兼容旧格式：检查目录下有没有 .md 文件
			subEntries, _ := os.ReadDir(skillDir)
			for _, se := range subEntries {
				if !se.IsDir() && strings.HasSuffix(se.Name(), ".md") {
					skillFile = filepath.Join(skillDir, se.Name())
					break
				}
			}
		}

		if err := sl.LoadFromFile(skillFile); err != nil {
			errs = append(errs, fmt.Errorf("load %s failed: %w", entry.Name(), err))
			continue
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("skill loading errors: %v", errs)
	}
	return nil
}
