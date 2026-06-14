package sandbox

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/tools"
)

// LangDef 工具层的语言定义
type LangDef struct {
	Command   string
	Args      []string
	Ext       string
	InitFiles map[string]string
}

// defaultLangs 返回平台适配的语言配置
func defaultLangs() map[string]LangDef {
	pythonCmd := "python3"
	shellCmd := "sh"
	shellArgs := []string{"-c"}

	if runtime.GOOS == "windows" {
		pythonCmd = "python"
		shellCmd = "cmd"
		shellArgs = []string{"/C"}
	}

	return map[string]LangDef{
		"python": {Command: pythonCmd, Ext: ".py"},
		"node":   {Command: "node", Ext: ".js"},
		"go":     {Command: "go", Args: []string{"run"}, Ext: ".go", InitFiles: map[string]string{"go.mod": "module sandbox\ngo 1.21\n"}},
		"shell":  {Command: shellCmd, Args: shellArgs},
	}
}

// RegisterSandboxTools 将沙箱能力注册为 Agent 可调用的工具
func RegisterSandboxTools(registry *tools.ToolRegistry, sb Sandbox, langs map[string]LangDef) error {
	if langs == nil {
		langs = defaultLangs()
	}

	langNames := make([]string, 0, len(langs))
	for name := range langs {
		langNames = append(langNames, name)
	}

	executeSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"language": map[string]any{
				"type":        "string",
				"enum":        langNames,
				"description": "编程语言",
			},
			"code": map[string]any{
				"type":        "string",
				"description": "要执行的代码",
			},
			"timeout": map[string]any{
				"type":        "number",
				"description": "超时秒数，默认 30",
			},
		},
		"required": []string{"language", "code"},
	}

	if err := registry.Register(tools.ToolMetadata{
		Name:        "sandbox_execute_code",
		Description: fmt.Sprintf("在沙箱中执行代码。支持: %s。超时30秒，输出限制1MB。", strings.Join(langNames, ", ")),
		Parameters:  executeSchema,
		Category:    "sandbox",
		Tags:        []string{"sandbox", "execution"},
	}, makeExecuteHandler(sb, langs)); err != nil {
		return err
	}

	commandSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "要执行的 shell 命令",
			},
			"timeout": map[string]any{
				"type":        "number",
				"description": "超时秒数，默认 30",
			},
		},
		"required": []string{"command"},
	}

	return registry.Register(tools.ToolMetadata{
		Name:        "sandbox_run_command",
		Description: "在沙箱中执行 shell 命令。有超时和输出大小限制。",
		Parameters:  commandSchema,
		Category:    "sandbox",
		Tags:        []string{"sandbox", "shell"},
	}, makeCommandHandler(sb, langs))
}

func makeExecuteHandler(sb Sandbox, langs map[string]LangDef) func(ctx context.Context, args map[string]any) (any, error) {
	return func(ctx context.Context, args map[string]any) (any, error) {
		langName, _ := args["language"].(string)
		code, _ := args["code"].(string)
		if langName == "" || code == "" {
			return nil, fmt.Errorf("language 和 code 不能为空")
		}

		lang, ok := langs[langName]
		if !ok {
			return nil, fmt.Errorf("不支持的语言: %s", langName)
		}

		var timeout time.Duration
		if t, ok := args["timeout"].(float64); ok && t > 0 {
			timeout = time.Duration(t * float64(time.Second))
		}

		// 构建命令 + 文件注入
		req := ExecRequest{Timeout: timeout}

		for name, content := range lang.InitFiles {
			req.Files = append(req.Files, InputFile{Path: name, Content: content})
		}

		if lang.Ext != "" {
			// 有扩展名：写代码到文件，运行 <command> <args...> <file>
			req.Files = append(req.Files, InputFile{Path: "main" + lang.Ext, Content: code})
			req.Command = lang.Command
			req.Args = append(append([]string{}, lang.Args...), "main"+lang.Ext)
		} else {
			// 无扩展名：shell 模式，代码直接作为命令参数
			req.Command = lang.Command
			req.Args = append(append([]string{}, lang.Args...), code)
		}

		result, err := sb.Execute(ctx, req)
		if err != nil {
			return nil, err
		}
		return FormatResult(result), nil
	}
}

func makeCommandHandler(sb Sandbox, langs map[string]LangDef) func(ctx context.Context, args map[string]any) (any, error) {
	return func(ctx context.Context, args map[string]any) (any, error) {
		command, _ := args["command"].(string)
		if command == "" {
			return nil, fmt.Errorf("command 不能为空")
		}

		var timeout time.Duration
		if t, ok := args["timeout"].(float64); ok && t > 0 {
			timeout = time.Duration(t * float64(time.Second))
		}

		// shell 模式：用配置的 shell 命令
		shell := langs["shell"]
		result, err := sb.Execute(ctx, ExecRequest{
			Command: shell.Command,
			Args:    append(append([]string{}, shell.Args...), command),
			Timeout: timeout,
		})
		if err != nil {
			return nil, err
		}
		return FormatResult(result), nil
	}
}

func FormatResult(r *ExecResult) string {
	var sb strings.Builder
	if r.TimedOut {
		sb.WriteString("[TIMEOUT] 执行超时\n")
	}
	sb.WriteString(fmt.Sprintf("Exit Code: %d\n", r.ExitCode))
	sb.WriteString(fmt.Sprintf("Duration: %s\n", r.Duration.Round(time.Millisecond)))
	if r.Truncated {
		sb.WriteString("(output truncated)\n")
	}
	if r.Stdout != "" {
		sb.WriteString("\n--- STDOUT ---\n")
		sb.WriteString(r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			sb.WriteString("\n")
		}
	}
	if r.Stderr != "" {
		sb.WriteString("\n--- STDERR ---\n")
		sb.WriteString(r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			sb.WriteString("\n")
		}
	}
	if r.Stdout == "" && r.Stderr == "" {
		sb.WriteString("\n(no output)\n")
	}
	return sb.String()
}
