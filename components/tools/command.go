package tools

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"time"
)

// 预编译危险命令正则
// 策略：只拦截明确的破坏性操作，不拦截 shell 操作符
var dangerousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+(-[a-zA-Z]*f[a-zA-Z]*\s+|.*\s+)(/|[a-zA-Z]:\\)`), // rm -f / 或 rm C:\
	regexp.MustCompile(`(?i)\bmkfs\b`),
	regexp.MustCompile(`(?i)\bdd\s+if=/dev/(zero|random)\s+of=/dev/`),
	regexp.MustCompile(`(?i)\brd\s+/s\s+/q\b`), // rd /s /q (Windows 强制删除目录)
	regexp.MustCompile(`(?i)\bdel\s+/[sfq]\b`), // del /s /f /q
	regexp.MustCompile(`(?i)shutdown\b`),
	regexp.MustCompile(`(?i)format\s+[a-zA-Z]:`), // format C:
}

func CommandExec(ctx context.Context, args map[string]any) (any, error) {
	command, ok := args["command"].(string)
	if !ok || command == "" {
		return nil, fmt.Errorf("command must be a non-empty string")
	}

	// 危险命令检查
	for _, pat := range dangerousPatterns {
		if pat.MatchString(command) {
			return nil, fmt.Errorf("dangerous command blocked: %s", command)
		}
	}

	timeoutSec := 30.0
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeoutSec = t
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	shell := "sh"
	shellArg := "-c"
	if runtime.GOOS == "windows" {
		shell = "cmd"
		shellArg = "/c"
	}
	cmd := exec.CommandContext(ctx, shell, shellArg, command)

	// 安全检查 cwd
	if cwd, ok := args["cwd"].(string); ok && cwd != "" {
		safeDir, err := safePath(".", cwd)
		if err != nil {
			return nil, fmt.Errorf("cwd access denied: %w", err)
		}
		cmd.Dir = safeDir
	}

	output, err := cmd.CombinedOutput()

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

	return result, nil
}

var commandExecParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"command": map[string]any{"type": "string", "description": "要执行的命令（必填）"},
		"timeout": map[string]any{"type": "number", "description": "超时时间（秒），默认30"},
		"cwd":     map[string]any{"type": "string", "description": "命令执行的工作目录（可选，需在安全目录内）"},
	},
	"required": []string{"command"},
}

func RegisterCommandTools(registry *ToolRegistry) {
	registry.MustRegister(ToolMetadata{
		Name:        "command_exec",
		Description: "执行系统命令（支持 Windows/Linux/macOS），返回输出结果。危险操作会被拦截",
		Parameters:  commandExecParams,
		Permission:  PermDangerous,
		Category:    "system",
		Version:     "1.0.0",
		Tags:        []string{"system", "command", "dangerous"},
		Timeout:     30 * time.Second,
	}, CommandExec)
}
