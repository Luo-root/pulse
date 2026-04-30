package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PromptLoader Markdown 配置加载器
type PromptLoader struct {
	configDir string
}

func NewPromptLoader(configDir string) *PromptLoader {
	return &PromptLoader{configDir: configDir}
}

// LoadRules 加载运行规则
func (pl *PromptLoader) LoadRules() (string, error) {
	return pl.loadFile("rules.md")
}

// LoadSystemPrompt 加载系统提示词
func (pl *PromptLoader) LoadSystemPrompt() (string, error) {
	return pl.loadFile("system_prompt.md")
}

// LoadSafetyRules 加载安全策略
func (pl *PromptLoader) LoadSafetyRules() (string, error) {
	return pl.loadFile("safety.md")
}

func (pl *PromptLoader) loadFile(filename string) (string, error) {
	path := filepath.Join(pl.configDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ReplaceVariables 替换配置中的变量（如 {{work_dir}}）
func (pl *PromptLoader) ReplaceVariables(content string, vars map[string]string) string {
	for k, v := range vars {
		content = strings.ReplaceAll(content, "{{"+k+"}}", v)
	}
	return content
}

func (pl *PromptLoader) LoadAllDefaultPrompt() (string, error) {
	rules, err := pl.LoadRules()
	if err != nil {
		return "", err
	}

	workDir := GetWorkDir()
	rules = pl.ReplaceVariables(rules, map[string]string{
		"work_dir": workDir,
	})

	systemPrompt, err := pl.LoadSystemPrompt()
	if err != nil {
		return "", err
	}
	safetyRules, err := pl.LoadSafetyRules()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s\n\n# 运行规则\n%s\n%s", systemPrompt, rules, safetyRules), nil
}
