package tools

import (
	"os"
	"path/filepath"
	"strings"
)

// PromptLoader 通用 Markdown 配置加载器
// 支持从目录加载任意 .md 文件，以及业界标准的 AGENTS.md
type PromptLoader struct {
	configDir string
}

func NewPromptLoader(configDir string) *PromptLoader {
	return &PromptLoader{configDir: configDir}
}

// LoadFile 加载指定文件名的内容
func (pl *PromptLoader) LoadFile(filename string) (string, error) {
	path := filepath.Join(pl.configDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// LoadByName 按名称加载（自动加 .md 后缀）
func (pl *PromptLoader) LoadByName(name string) (string, error) {
	if !strings.HasSuffix(name, ".md") {
		name = name + ".md"
	}
	return pl.LoadFile(name)
}

// ReplaceVariables 替换模板变量（如 {{work_dir}}）
func (pl *PromptLoader) ReplaceVariables(content string, vars map[string]string) string {
	for k, v := range vars {
		content = strings.ReplaceAll(content, "{{"+k+"}}", v)
	}
	return content
}

// ============================================================================
// 业界标准文件加载
// ============================================================================

// LoadAGENTS 加载 AGENTS.md（业界标准的 Agent 指令文件）
// 搜索顺序：configDir/AGENTS.md
func (pl *PromptLoader) LoadAGENTS() (string, error) {
	return pl.LoadFile("AGENTS.md")
}

// LoadCLAUDE 加载 CLAUDE.md（Claude Code 标准指令文件）
func (pl *PromptLoader) LoadCLAUDE() (string, error) {
	return pl.LoadFile("CLAUDE.md")
}

// LoadCursorRules 加载 .cursor/rules/ 目录下的规则文件
func (pl *PromptLoader) LoadCursorRules() (string, error) {
	rulesDir := filepath.Join(pl.configDir, ".cursor", "rules")
	entries, err := os.ReadDir(rulesDir)
	if err != nil {
		return "", err
	}

	var parts []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		content, err := pl.LoadFile(filepath.Join(".cursor", "rules", entry.Name()))
		if err != nil {
			continue
		}
		parts = append(parts, strings.TrimSpace(content))
	}

	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, "\n\n"), nil
}

// LoadAll 加载所有可用的指令文件，合并为一个 prompt
// 搜索优先级：AGENTS.md > CLAUDE.md > .cursor/rules/
// 返回合并后的内容和实际加载到的文件名列表
func (pl *PromptLoader) LoadAll() (string, []string, error) {
	var parts []string
	var loaded []string

	// AGENTS.md（最高优先级）
	if content, err := pl.LoadAGENTS(); err == nil && content != "" {
		parts = append(parts, content)
		loaded = append(loaded, "AGENTS.md")
	}

	// CLAUDE.md
	if content, err := pl.LoadCLAUDE(); err == nil && content != "" {
		parts = append(parts, content)
		loaded = append(loaded, "CLAUDE.md")
	}

	// .cursor/rules/
	if content, err := pl.LoadCursorRules(); err == nil && content != "" {
		parts = append(parts, content)
		loaded = append(loaded, ".cursor/rules/")
	}

	if len(parts) == 0 {
		return "", nil, nil
	}
	return strings.Join(parts, "\n\n---\n\n"), loaded, nil
}

// LoadWithDefaults 加载所有可用指令文件，附加工作目录变量替换
func (pl *PromptLoader) LoadWithDefaults() (string, []string, error) {
	content, loaded, err := pl.LoadAll()
	if err != nil {
		return "", nil, err
	}
	if content == "" {
		return "", nil, nil
	}

	workDir := GetWorkDir()
	content = pl.ReplaceVariables(content, map[string]string{
		"work_dir": workDir,
	})

	return content, loaded, nil
}
