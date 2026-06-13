package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/tools"
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
	AllowedTools  string         `yaml:"allowed-tools"`
	Metadata      map[string]any `yaml:"metadata"`
	Parameters    map[string]any `yaml:"parameters"`
	// 自定义扩展
	Category string   `yaml:"category"`
	Tags     []string `yaml:"tags"`
	Timeout  int      `yaml:"timeout"`  // 秒
	EnvVars  string   `yaml:"env_vars"` // 空格分隔的环境变量名
}

// ParseSkillMarkdown 解析 Skill Markdown 文档
// Skill 现在只支持指令型：frontmatter 定义元数据，正文作为指令内容返回
func ParseSkillMarkdown(content string) (*Skill, error) {
	fm, body, err := parseFrontmatter(content)
	if err != nil {
		return nil, fmt.Errorf("parse frontmatter failed: %w", err)
	}

	// 解析 allowed-tools
	var allowedTools []string
	if fm.AllowedTools != "" {
		allowedTools = strings.Fields(fm.AllowedTools)
	}

	// 默认 parameters
	params := fm.Parameters
	if params == nil {
		params = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}

	var envVars []string
	if fm.EnvVars != "" {
		envVars = strings.Fields(fm.EnvVars)
	}

	s := &Skill{
		Name:          fm.Name,
		Description:   fm.Description,
		License:       fm.License,
		Compatibility: fm.Compatibility,
		AllowedTools:  allowedTools,
		Metadata:      fm.Metadata,
		Parameters:    params,
		Category:      fm.Category,
		Tags:          fm.Tags,
		Timeout:       time.Duration(fm.Timeout) * time.Second,
		EnvVars:       envVars,
		Body:          body,
	}

	if err := s.IsValid(); err != nil {
		return nil, err
	}

	return s, nil
}

// parseFrontmatter 解析 YAML Frontmatter
func parseFrontmatter(content string) (*SkillFrontmatter, string, error) {
	if !strings.HasPrefix(content, "---") {
		return nil, content, fmt.Errorf("no frontmatter found")
	}

	endIdx := strings.Index(content[3:], "---")
	if endIdx == -1 {
		return nil, content, fmt.Errorf("frontmatter not closed")
	}
	endIdx += 3

	var fm SkillFrontmatter
	if err := yaml.Unmarshal([]byte(content[3:endIdx]), &fm); err != nil {
		return nil, content, fmt.Errorf("parse yaml failed: %w", err)
	}

	body := strings.TrimSpace(content[endIdx+3:])
	return &fm, body, nil
}

// ============================================================================
// SkillLoader
// ============================================================================

type skillCallKey string

const SkillCallerKey skillCallKey = "skill_caller"

// SkillLoader Skill 加载器
type SkillLoader struct {
	registry     *SkillRegistry
	toolRegistry *tools.ToolRegistry
	hookIDs      map[string][]tools.HookID
}

// NewSkillLoader 创建 Skill 加载器
func NewSkillLoader(registry *SkillRegistry, toolRegistry *tools.ToolRegistry) *SkillLoader {
	return &SkillLoader{
		registry:     registry,
		toolRegistry: toolRegistry,
		hookIDs:      make(map[string][]tools.HookID),
	}
}

// Register 注册 Skill 为 Agent 可调用的工具
func (sl *SkillLoader) Register(skill Skill) error {
	if err := skill.IsValid(); err != nil {
		return fmt.Errorf("invalid skill %s: %w", skill.Name, err)
	}
	sl.registry.Register(&skill)

	// 创建 handler：返回 Skill 正文内容
	body := skill.Body
	handler := func(ctx context.Context, args map[string]any) (any, error) {
		return body, nil
	}

	callerName := skill.Name
	requiredEnv := skill.EnvVars

	wrappedHandler := func(ctx context.Context, args map[string]any) (any, error) {
		ctx = context.WithValue(ctx, SkillCallerKey, callerName)

		// 检查必需的环境变量
		if len(requiredEnv) > 0 {
			var missing []string
			for _, key := range requiredEnv {
				if os.Getenv(key) == "" {
					missing = append(missing, key)
				}
			}
			if len(missing) > 0 {
				return nil, fmt.Errorf("skill %q requires environment variables: %s",
					callerName, strings.Join(missing, ", "))
			}
		}

		return handler(ctx, args)
	}

	if err := sl.toolRegistry.RegisterSimple(
		skill.Name,
		skill.Description,
		wrappedHandler,
		tools.WithCategory(skill.Category),
		tools.WithTimeout(skill.Timeout),
		tools.WithTags(skill.Tags...),
		tools.WithParameters(skill.Parameters),
	); err != nil {
		return err
	}

	// 注册 AllowedTools 钩子
	if len(skill.AllowedTools) > 0 {
		sl.registerAllowedToolsHook(skill.Name, skill.AllowedTools)
	}

	return nil
}

// registerAllowedToolsHook 注册 beforeExecute 钩子，限制 Skill 可调用的工具
func (sl *SkillLoader) registerAllowedToolsHook(skillName string, allowedTools []string) {
	allowed := make(map[string]bool, len(allowedTools))
	for _, t := range allowedTools {
		allowed[t] = true
	}

	hookID := sl.toolRegistry.AddBeforeExecuteHook(func(ctx context.Context, toolName string, args map[string]any) error {
		caller, ok := ctx.Value(SkillCallerKey).(string)
		if !ok || caller != skillName {
			return nil
		}
		if toolName == skillName {
			return nil
		}
		if !allowed[toolName] {
			return fmt.Errorf("skill %q is not allowed to call tool %q (allowed: %s)",
				skillName, toolName, strings.Join(allowedTools, ", "))
		}
		return nil
	})

	sl.hookIDs[skillName] = append(sl.hookIDs[skillName], hookID)
}

// Unload 注销 Skill
func (sl *SkillLoader) Unload(name string) error {
	sl.registry.Remove(name)

	if ids, ok := sl.hookIDs[name]; ok {
		for _, id := range ids {
			sl.toolRegistry.RemoveBeforeExecuteHook(id)
		}
		delete(sl.hookIDs, name)
	}

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
	s, err := ParseSkillMarkdown(content)
	if err != nil {
		return err
	}
	return sl.Register(*s)
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
			// 兼容旧格式：查找目录下任意 .md 文件
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
