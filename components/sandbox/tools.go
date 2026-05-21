package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/tools"
)

// RegisterSandboxTools 将沙箱能力注册为 Agent 可调用的工具
func RegisterSandboxTools(registry *tools.ToolRegistry, sb Sandbox) error {

	// ---- 工具：执行代码 ----
	executeSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"language": map[string]any{
				"type":        "string",
				"enum":        sb.ListLangs(),
				"description": "编程语言",
			},
			"code": map[string]any{
				"type":        "string",
				"description": "要执行的代码。Python/Node/Go 需要完整可运行的代码，Shell 直接写命令",
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
		Description: buildExecuteDesc(sb),
		Parameters:  executeSchema,
		Category:    "sandbox",
		Tags:        []string{"sandbox", "execution"},
	}, makeExecuteHandler(sb)); err != nil {
		return fmt.Errorf("注册 execute_code: %w", err)
	}

	// ---- 工具：执行 Shell 命令 ----
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

	if err := registry.Register(tools.ToolMetadata{
		Name: "sandbox_run_command",
		Description: "在沙箱中执行 shell 命令。" +
			"适合运行系统命令、安装依赖、文件操作等。有超时和输出大小限制。",
		Parameters: commandSchema,
		Category:   "sandbox",
		Tags:       []string{"sandbox", "shell"},
	}, makeCommandHandler(sb)); err != nil {
		return fmt.Errorf("注册 run_command: %w", err)
	}

	return nil
}

// makeExecuteHandler 生成 execute_code 的 handler
func makeExecuteHandler(sb Sandbox) func(ctx context.Context, args map[string]any) (any, error) {
	return func(ctx context.Context, args map[string]any) (any, error) {
		lang, _ := args["language"].(string)
		code, _ := args["code"].(string)
		if lang == "" || code == "" {
			return nil, fmt.Errorf("language 和 code 不能为空")
		}

		var timeout time.Duration
		if t, ok := args["timeout"].(float64); ok && t > 0 {
			timeout = time.Duration(t * float64(time.Second))
		}

		result, err := sb.Execute(ctx, ExecRequest{
			Language: lang,
			Code:     code,
			Timeout:  timeout,
		})
		if err != nil {
			return nil, err
		}
		return FormatResult(result), nil
	}
}

// makeCommandHandler 生成 run_command 的 handler
func makeCommandHandler(sb Sandbox) func(ctx context.Context, args map[string]any) (any, error) {
	return func(ctx context.Context, args map[string]any) (any, error) {
		command, _ := args["command"].(string)
		if command == "" {
			return nil, fmt.Errorf("command 不能为空")
		}

		var timeout time.Duration
		if t, ok := args["timeout"].(float64); ok && t > 0 {
			timeout = time.Duration(t * float64(time.Second))
		}

		result, err := sb.Execute(ctx, ExecRequest{
			Language: "shell",
			Code:     command,
			Timeout:  timeout,
		})
		if err != nil {
			return nil, err
		}
		return FormatResult(result), nil
	}
}

// FormatResult 格式化执行结果为 LLM 可读文本
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

// buildExecuteDesc 构建 execute_code 工具描述
func buildExecuteDesc(sb Sandbox) string {
	langs := sb.ListLangs()
	var tips []string
	for _, lang := range langs {
		switch lang {
		case "python":
			tips = append(tips, "Python: 完整脚本，可 import 标准库")
		case "node":
			tips = append(tips, "Node.js: 完整脚本，可 require 标准模块")
		case "go":
			tips = append(tips, "Go: 需要 package main + func main()")
		case "shell":
			tips = append(tips, "Shell: 单条命令或多条用 && 连接")
		}
	}
	return fmt.Sprintf(
		"在沙箱中执行代码。支持语言: %s。有超时限制（默认30秒），输出限制1MB。\n%s",
		strings.Join(langs, ", "),
		strings.Join(tips, "；"),
	)
}
