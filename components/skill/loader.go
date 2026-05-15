package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/sandbox"
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
	AllowedTools  string         `yaml:"allowed-tools"` // YAML 中可写空格分隔
	Metadata      map[string]any `yaml:"metadata"`
	Parameters    map[string]any `yaml:"parameters"`
	// 自定义扩展
	Category string   `yaml:"category"`
	Tags     []string `yaml:"tags"`
	Timeout  int      `yaml:"timeout"`  // 秒
	EnvVars  string   `yaml:"env_vars"` // 新增：空格分隔的环境变量名
	// 新增：独立代码文件路径（相对于 Skill 目录）
	Script   string `yaml:"script"`
	Language string `yaml:"language"` // python, go, node, shell（默认 go）
}

// CodeBlock 代码块
type CodeBlock struct {
	Language string
	Code     string
}

// ParseSkillMarkdown 解析 Skill Markdown 文档
// skillDir 为 Skill 所在目录，用于定位 script 文件（可为空）
func ParseSkillMarkdown(content string, sb sandbox.Sandbox, skillDir string) (*Skill, error) {
	fm, body, err := parseFrontmatter(content)
	if err != nil {
		return nil, fmt.Errorf("parse frontmatter failed: %w", err)
	}

	// 确定语言：frontmatter 指定 > 默认 go
	lang := fm.Language
	if lang == "" {
		lang = "go"
	}

	var handler tools.ToolHandler

	// 优先级1：从独立文件加载代码（script 字段）
	if fm.Script != "" && skillDir != "" {
		scriptPath := filepath.Join(skillDir, fm.Script)
		codeBytes, err := os.ReadFile(scriptPath)
		if err != nil {
			return nil, fmt.Errorf("read script file %s: %w", fm.Script, err)
		}
		// 从文件扩展名推断语言，覆盖 frontmatter 的 language
		scriptLang := langFromExt(filepath.Ext(fm.Script))
		if scriptLang != "" {
			lang = scriptLang
		}
		if sb != nil {
			handler, err = compileHandler(sb, lang, string(codeBytes))
			if err != nil {
				return nil, fmt.Errorf("compile script %s: %w", fm.Script, err)
			}
		}
	}

	// 优先级2：从内嵌代码块加载（向后兼容）
	if handler == nil {
		codeBlocks := extractCodeBlocks(body)
		if len(codeBlocks) > 0 && sb != nil {
			for _, cb := range codeBlocks {
				blockLang := cb.Language
				if blockLang == "golang" {
					blockLang = "go"
				}
				// frontmatter 的 language 优先，否则用代码块标记的语言
				if blockLang == "" {
					blockLang = lang
				}
				handler, err = compileHandler(sb, blockLang, cb.Code)
				if err != nil {
					return nil, fmt.Errorf("compile handler failed: %w", err)
				}
				break
			}
		}
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

	var envVars []string
	if fm.EnvVars != "" {
		envVars = strings.Fields(fm.EnvVars)
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
		Timeout:       time.Duration(fm.Timeout) * time.Second,
		Handler:       handler,
		EnvVars:       envVars,
		Language:      lang,
		Script:        fm.Script, // 新增
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

// langFromExt 从文件扩展名推断语言
func langFromExt(ext string) string {
	switch ext {
	case ".py":
		return "python"
	case ".js":
		return "node"
	case ".go":
		return "go"
	case ".sh", ".bat":
		return "shell"
	default:
		return ""
	}
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

	startMarker := "```"
	endMarker := "```"

	pos := 0
	for {
		// 找开始的 ```
		startIdx := strings.Index(content[pos:], startMarker)
		if startIdx == -1 {
			break
		}
		startIdx += pos

		// 跳过开始标记
		afterStart := startIdx + len(startMarker)

		// 提取语言标识（同一行，在 ``` 之后到换行之前）
		lineEnd := strings.IndexAny(content[afterStart:], "\r\n")
		if lineEnd == -1 {
			break
		}

		lang := strings.TrimSpace(content[afterStart : afterStart+lineEnd])

		// 跳过语言标识行末的换行符（\r\n 或 \n）
		codeStart := afterStart + lineEnd
		for codeStart < len(content) && (content[codeStart] == '\r' || content[codeStart] == '\n') {
			codeStart++
		}

		// 找结束的 ```
		// 从 codeStart 开始搜索，找独占一行的 ```
		remaining := content[codeStart:]
		endIdx := -1
		searchPos := 0
		for searchPos < len(remaining) {
			nextClose := strings.Index(remaining[searchPos:], endMarker)
			if nextClose == -1 {
				break
			}
			// 验证 ``` 独占一行（前面是行首或换行，后面是行末或换行）
			absIdx := searchPos + nextClose
			atLineStart := absIdx == 0 || remaining[absIdx-1] == '\n' || remaining[absIdx-1] == '\r'
			closeEnd := absIdx + len(endMarker)
			atLineEnd := closeEnd >= len(remaining) || remaining[closeEnd] == '\n' || remaining[closeEnd] == '\r'

			if atLineStart && atLineEnd {
				endIdx = absIdx
				break
			}
			searchPos = absIdx + 1
		}

		if endIdx == -1 {
			break
		}

		code := strings.TrimSpace(remaining[:endIdx])
		if lang != "" || code != "" {
			blocks = append(blocks, CodeBlock{
				Language: lang,
				Code:     code,
			})
		}

		// 继续搜索下一个代码块
		pos = codeStart + endIdx + len(endMarker)
	}

	return blocks
}

// ============================================================================
// Handler 编译
// ============================================================================

// compileHandler 使用 ProcessSandbox 执行 Skill 代码
func compileHandler(sb sandbox.Sandbox, lang, code string) (tools.ToolHandler, error) {
	// 先验证语言可用
	if err := sb.CheckLang(lang); err != nil {
		return nil, fmt.Errorf("skill language not available: %w", err)
	}

	// 自动包装：确保代码可执行
	execCode, err := wrapCode(lang, code)
	if err != nil {
		return nil, fmt.Errorf("wrap code: %w", err)
	}

	return func(ctx context.Context, args map[string]any) (any, error) {
		result, err := sb.Execute(ctx, sandbox.ExecRequest{
			Language: lang,
			Code:     execCode,
			Env: map[string]string{
				"SKILL_ARGS": marshalArgs(args),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("skill execution error: %w", err)
		}
		if result.TimedOut {
			return nil, fmt.Errorf("skill execution timed out after %s", result.Duration)
		}
		if result.ExitCode != 0 {
			return nil, fmt.Errorf("skill execution failed (exit %d): %s", result.ExitCode, result.Stderr)
		}
		return parseSkillOutput(result.Stdout), nil
	}, nil
}

// marshalArgs 将 args 序列化为 JSON 字符串，通过环境变量传入
func marshalArgs(args map[string]any) string {
	if args == nil {
		return "{}"
	}
	data, _ := json.Marshal(args)
	return string(data)
}

// parseSkillOutput 尝试解析输出为结构化数据
func parseSkillOutput(stdout string) any {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return ""
	}

	// 按行分割，逐行尝试 JSON 解析
	lines := strings.Split(stdout, "\n")

	// 只取第一个非空行尝试 JSON 解析
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(line), &parsed); err == nil {
			return parsed
		}
		// 第一个非空行不是 JSON，说明是纯文本，返回全部内容
		return stdout
	}

	return stdout
}

// wrapCode 根据语言和代码内容，自动包装成可执行代码
func wrapCode(lang, code string) (string, error) {
	switch lang {
	case "go":
		return wrapGoCode(code)
	case "python":
		return wrapPythonCode(code), nil
	case "node":
		return wrapNodeCode(code), nil
	case "shell":
		return code, nil // shell 不需要包装
	default:
		return "", fmt.Errorf("unsupported skill language: %s", lang)
	}
}

// wrapGoCode 处理三种 Go 代码输入模式
//
// 模式1: 完整 package main + func main → 直接用
// 模式2: package skill + func Handler → 改包名，加 main 入口
// 模式3: 裸函数体 → 包装成完整文件
func wrapGoCode(code string) (string, error) {
	trimmed := strings.TrimSpace(code)

	// 模式1: 已经是完整 main 包
	if strings.Contains(trimmed, "package main") {
		return code, nil
	}

	// 模式2: package skill + func Handler
	if strings.Contains(trimmed, "package skill") && strings.Contains(trimmed, "func Handler") {
		wrapped := strings.Replace(trimmed, "package skill", "package main", 1)
		// 在代码末尾追加 main 函数
		wrapped += `

func main() {
	os.Setenv("__SKILL_CALL", "1")
	// 从环境变量获取 args
	argsJSON := os.Getenv("SKILL_ARGS")
	args := map[string]any{}
	if argsJSON != "" {
		json.Unmarshal([]byte(argsJSON), &args)
	}
	result, err := Handler(context.Background(), args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}
	output, _ := json.Marshal(result)
	fmt.Println(string(output))
}
`
		// 自动补全 import
		requiredImports := []string{`"os"`, `"encoding/json"`, `"fmt"`, `"context"`}
		for _, pkg := range requiredImports {
			if !strings.Contains(wrapped, pkg) {
				wrapped = addGoImport(wrapped, pkg)
			}
		}
		return wrapped, nil
	}

	// 模式3: 裸函数体
	wrapped := fmt.Sprintf(`package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

func Handler(ctx context.Context, args map[string]any) (any, error) {
%s
}

func main() {
	argsJSON := os.Getenv("SKILL_ARGS")
	args := map[string]any{}
	if argsJSON != "" {
		json.Unmarshal([]byte(argsJSON), &args)
	}
	result, err := Handler(context.Background(), args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%%v", err)
		os.Exit(1)
	}
	output, _ := json.Marshal(result)
	fmt.Println(string(output))
}
`, code)
	return wrapped, nil
}

// addGoImport 在 import 块中添加包
func addGoImport(code, pkg string) string {
	// 情况1：有 import 块
	if strings.Contains(code, "import (") {
		return strings.Replace(code, "import (", "import (\n\t"+pkg, 1)
	}

	// 情况2：单行 import（如 import "context"）→ 转成块
	re := regexp.MustCompile(`(?m)^import\s+"([^"]+)"`)
	match := re.FindString(code)
	if match != "" {
		existing := re.FindStringSubmatch(code)[1]
		replacement := fmt.Sprintf("import (\n\t%q\n\t%s\n)", existing, pkg)
		return strings.Replace(code, match, replacement, 1)
	}

	// 情况3：没有 import 声明 → 在 package 后插入
	lines := strings.SplitN(code, "\n", 2)
	if len(lines) == 2 {
		return lines[0] + "\n\nimport (\n\t" + pkg + "\n)\n" + lines[1]
	}
	return code
}

func wrapPythonCode(code string) string {
	trimmed := strings.TrimSpace(code)

	// 模式1: 已有 if __name__ == "__main__" → 直接用
	if strings.Contains(trimmed, `if __name__`) {
		return code
	}

	// 模式2: 自包含代码（自己读 SKILL_ARGS 并 print 结果）→ 直接用
	if strings.Contains(trimmed, "SKILL_ARGS") {
		return code
	}

	// 模式3: 有 def handler(args) → 加 main 入口
	if strings.Contains(trimmed, "def handler") {
		return trimmed + `

import json, os
if __name__ == "__main__":
    args = json.loads(os.environ.get("SKILL_ARGS", "{}"))
    result = handler(args)
    if result is not None:
        print(json.dumps(result) if not isinstance(result, str) else result)
`
	}

	// 模式4: 裸代码 → 包装成 handler + main
	return fmt.Sprintf(`import json, os
def handler(args):
%s

if __name__ == "__main__":
    args = json.loads(os.environ.get("SKILL_ARGS", "{}"))
    result = handler(args)
    if result is not None:
        print(json.dumps(result) if not isinstance(result, str) else result)
`, indentPython(code))
}

func wrapNodeCode(code string) string {
	trimmed := strings.TrimSpace(code)

	// 模式1: 已有 process 相关代码 → 直接用
	if strings.Contains(trimmed, "process.argv") || strings.Contains(trimmed, "process.env") {
		return code
	}

	// 模式2: 自包含代码（自己读 SKILL_ARGS）→ 直接用
	if strings.Contains(trimmed, "SKILL_ARGS") {
		return code
	}

	// 模式3: 有 function handler → 加 main 入口
	if strings.Contains(trimmed, "function handler") || strings.Contains(trimmed, "const handler") {
		return trimmed + `

const args = JSON.parse(process.env.SKILL_ARGS || '{}');
const result = handler(args);
if (result instanceof Promise) {
    result.then(r => { if (r != null) console.log(typeof r === 'string' ? r : JSON.stringify(r)); });
} else if (result != null) {
    console.log(typeof result === 'string' ? result : JSON.stringify(result));
}
`
	}

	// 模式4: 裸代码 → 包装成 handler + main
	return fmt.Sprintf(`function handler(args) {
%s
}
const args = JSON.parse(process.env.SKILL_ARGS || '{}');
const result = handler(args);
if (result instanceof Promise) {
    result.then(r => { if (r != null) console.log(typeof r === 'string' ? r : JSON.stringify(r)); });
} else if (result != null) {
    console.log(typeof result === 'string' ? result : JSON.stringify(result));
}
`, code)
}

func indentPython(code string) string {
	lines := strings.Split(code, "\n")
	var result []string
	for _, line := range lines {
		if line == "" {
			result = append(result, "")
		} else {
			result = append(result, "    "+line)
		}
	}
	return strings.Join(result, "\n")
}

// ============================================================================
// SkillLoader Skill 加载器
// ============================================================================

// skillCallKey 用于在 context 中标记当前调用来源的 Skill 名称
type skillCallKey string

const SkillCallerKey skillCallKey = "skill_caller"

// SkillLoader Skill 加载器
type SkillLoader struct {
	loader       *tools.DynamicToolLoader
	registry     *SkillRegistry
	toolRegistry *tools.ToolRegistry
	sandbox      sandbox.Sandbox
	// 记录每个 Skill 注册的钩子 ID，便于 Unload 时清理
	hookIDs map[string][]tools.HookID
}

func NewSkillLoader(registry *SkillRegistry, toolRegistry *tools.ToolRegistry, sb sandbox.Sandbox) *SkillLoader {
	return &SkillLoader{
		loader:       tools.NewDynamicToolLoader(toolRegistry),
		registry:     registry,
		toolRegistry: toolRegistry,
		sandbox:      sb,
		hookIDs:      make(map[string][]tools.HookID),
	}
}

func (sl *SkillLoader) Register(skill Skill) error {
	if err := skill.IsValid(); err != nil {
		return fmt.Errorf("invalid skill %s: %w", skill.Name, err)
	}
	sl.registry.Register(&skill)

	var handler tools.ToolHandler
	if skill.Type == SkillTypeCode {
		handler = skill.Handler
	} else {
		body := skill.Body
		handler = func(ctx context.Context, args map[string]any) (any, error) {
			return body, nil
		}
	}

	if handler != nil {
		callerName := skill.Name
		requiredEnv := skill.EnvVars // 捕获当前值

		wrappedHandler := func(ctx context.Context, args map[string]any) (any, error) {
			// 注入调用者身份（AllowedTools 钩子需要）
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

		if err := sl.loader.Load(
			skill.Name,
			skill.Description,
			wrappedHandler,
			skill.Parameters,
			tools.WithCategory(skill.Category),
			tools.WithTimeout(skill.Timeout),
			tools.WithTags(skill.Tags...),
		); err != nil {
			return err
		}

		if len(skill.AllowedTools) > 0 {
			sl.registerAllowedToolsHook(skill.Name, skill.AllowedTools)
		}
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

func (sl *SkillLoader) Unload(name string) error {
	sl.registry.Remove(name)

	// 清理钩子
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

	// 传入 Skill 所在目录，用于定位 script 文件
	skillDir := filepath.Dir(path)
	return sl.LoadFromStringWithDir(string(content), skillDir)
}

// LoadFromString 从字符串加载 Skill（无目录上下文，script 字段将被忽略）
func (sl *SkillLoader) LoadFromString(content string) error {
	return sl.LoadFromStringWithDir(content, "")
}

// LoadFromStringWithDir 从字符串加载 Skill，指定目录上下文
func (sl *SkillLoader) LoadFromStringWithDir(content string, skillDir string) error {
	skill, err := ParseSkillMarkdown(content, sl.sandbox, skillDir)
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
