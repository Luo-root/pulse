package tools

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// 预编译危险命令正则（提升性能）
var dangerousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^rm\s+-rf`),
	regexp.MustCompile(`(?i)mkfs\s+`),
	regexp.MustCompile(`(?i)dd\s+if=/dev/zero`),
	regexp.MustCompile(`(?i)^rd\s+/s`),
	regexp.MustCompile(`(?i)^del\s+/s`),
	regexp.MustCompile(`;`),
	regexp.MustCompile(`&&`),
	regexp.MustCompile(`\|\|`),
}

// CommandExec 执行命令（带超时+跨平台+安全检查）
func CommandExec(ctx context.Context, args map[string]any) (any, error) {
	// 1. 安全校验参数
	command, ok := args["command"].(string)
	if !ok || command == "" {
		return nil, fmt.Errorf("command must be a non-empty string")
	}

	// 2. 危险命令检查
	for _, pat := range dangerousPatterns {
		if pat.MatchString(command) {
			return nil, fmt.Errorf("dangerous command blocked: %s", command)
		}
	}

	// 3. 解析超时
	timeoutSec := 30.0
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeoutSec = t
	}

	// 4. 创建带超时的 context
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	// 5. 跨平台选择 shell
	shell := "sh"
	shellArg := "-c"
	if runtime.GOOS == "windows" {
		shell = "cmd"
		shellArg = "/c"
	}
	cmd := exec.CommandContext(ctx, shell, shellArg, command)

	// 6. 可选：设置工作目录
	if cwd, ok := args["cwd"].(string); ok && cwd != "" {
		cmd.Dir = cwd
	}

	// 7. 执行命令
	output, err := cmd.CombinedOutput()

	// 8. 封装结果
	result := map[string]any{
		"command": command,
		"output":  string(output),
	}

	if err != nil {
		result["status"] = "failed"
		if ctx.Err() == context.DeadlineExceeded {
			result["error"] = "command timeout"
		} else {
			result["error"] = err.Error()
		}
	} else {
		result["status"] = "success"
	}

	// 始终返回结果，让模型决定如何处理错误
	return result, nil
}

func RegisterCommandExecTools(executor *schema.ToolExecutor) {
	executor.MustRegister(schema.Tool{
		Name:        "command_exec",
		Description: "执行系统命令（支持 Windows/Linux/macOS），返回输出结果。禁止执行危险命令（如 rm -rf、mkfs 等）。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "要执行的命令（必填）"},
				"timeout": map[string]any{"type": "number", "description": "超时时间（秒），默认30"},
				"cwd":     map[string]any{"type": "string", "description": "命令执行的工作目录（可选）"},
			},
			"required": []string{"command"},
		},
	}, CommandExec)
}
