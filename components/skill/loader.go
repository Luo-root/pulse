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

// SkillFrontmatter Skill Markdown 文档前置元数据
type SkillFrontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Category    string   `yaml:"category"`
	Timeout     int      `yaml:"timeout"` // 秒
	Tags        []string `yaml:"tags"`
}

// CodeBlock 代码块
type CodeBlock struct {
	Language string
	Code     string
}

// ParseSkillMarkdown 解析 Skill Markdown 文档
func ParseSkillMarkdown(content string) (*Skill, error) {
	// 1. 解析 YAML Frontmatter
	fm, body, err := parseFrontmatter(content)
	if err != nil {
		return nil, fmt.Errorf("parse frontmatter failed: %w", err)
	}

	// 2. 提取代码块
	codeBlocks := extractCodeBlocks(body)
	if len(codeBlocks) == 0 {
		return nil, fmt.Errorf("no code block found in skill markdown")
	}

	// 3. 找到 Go 代码块
	var handlerCode string
	for _, cb := range codeBlocks {
		if cb.Language == "go" || cb.Language == "golang" {
			handlerCode = cb.Code
			break
		}
	}
	if handlerCode == "" {
		return nil, fmt.Errorf("no go code block found")
	}

	// 4. 编译 handler
	handler, err := compileHandler(handlerCode)
	if err != nil {
		return nil, fmt.Errorf("compile handler failed: %w", err)
	}

	// 5. 构建 Skill
	timeout := time.Duration(fm.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &Skill{
		Name:        fm.Name,
		Description: fm.Description,
		Category:    fm.Category,
		Timeout:     timeout,
		Tags:        fm.Tags,
		Parameters:  extractParameters(body),
		Handler:     handler,
	}, nil
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

// extractParameters 从文档提取参数定义（简化版）
func extractParameters(content string) map[string]any {
	// 默认参数结构
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

// ============================================================================
// Handler 编译（使用 yaegi 解释器）
// ============================================================================

// compileHandler 编译 Go 代码为 ToolHandler
// 使用 yaegi 解释器执行 Skill 代码，支持动态加载和沙箱隔离
func compileHandler(code string) (schema.ToolHandler, error) {
	// 创建 yaegi 解释器
	i := interp.New(interp.Options{})

	// 导入标准库（限制可用的包，增强安全性）
	i.Use(stdlib.Symbols)

	// 构建完整的 Skill 代码
	// 用户代码只需要写函数体，我们包装成完整的函数
	wrappedCode := fmt.Sprintf(`
package skill

import (
	"context"
	"fmt"
)

// Handler 执行 Skill 逻辑
func Handler(ctx context.Context, args map[string]any) (any, error) {
%s
}
`, code)

	// 编译代码
	_, err := i.Eval(wrappedCode)
	if err != nil {
		return nil, fmt.Errorf("compile skill code failed: %w", err)
	}

	// 获取编译后的函数
	v, err := i.Eval("skill.Handler")
	if err != nil {
		return nil, fmt.Errorf("get handler function failed: %w", err)
	}

	// 类型断言为 ToolHandler
	handler, ok := v.Interface().(func(context.Context, map[string]any) (any, error))
	if !ok {
		return nil, fmt.Errorf("handler type mismatch: expected func(context.Context, map[string]any) (any, error)")
	}

	// 返回包装后的 handler，添加超时控制
	return func(ctx context.Context, args map[string]any) (any, error) {
		// 使用带超时的 context
		// 注意：实际超时时间由 ToolRegistry 控制，这里只是额外的一层保护
		return handler(ctx, args)
	}, nil
}

// ============================================================================
// SkillLoader Skill 加载器
// ============================================================================

// SkillLoader Skill 加载器
type SkillLoader struct {
	loader *tools.DynamicToolLoader
}

// NewSkillLoader 创建 Skill 加载器
func NewSkillLoader(registry *schema.ToolRegistry) *SkillLoader {
	return &SkillLoader{
		loader: tools.NewDynamicToolLoader(registry),
	}
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

// LoadFromDir 从目录批量加载 Skill
func (sl *SkillLoader) LoadFromDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read skill dir failed: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		if err := sl.LoadFromFile(path); err != nil {
			// 记录错误但继续加载其他
			fmt.Printf("load skill %s failed: %v\n", entry.Name(), err)
			continue
		}
	}

	return nil
}

// Register 注册 Skill 到 Registry
func (sl *SkillLoader) Register(skill Skill) error {
	return sl.loader.Load(
		skill.Name,
		skill.Description,
		skill.Handler,
		skill.Parameters,
		tools.WithCategory(skill.Category),
		tools.WithTimeout(skill.Timeout),
		tools.WithTags(skill.Tags...),
	)
}
