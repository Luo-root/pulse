package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/bufutil"
)

// ============================================================================
// command_exec — 执行 shell 命令
// ============================================================================

func CommandExec(ctx context.Context, args map[string]any) (any, error) {
	command, _ := args["command"].(string)
	if command == "" {
		return nil, fmt.Errorf("command is required")
	}

	// 危险命令拦截
	if err := checkDangerousCommand(command); err != nil {
		return nil, err
	}

	timeout := 30 * time.Second
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t * float64(time.Second))
	}

	workDir, _ := args["work_dir"].(string)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}

	if workDir != "" {
		absDir, err := absPath(workDir)
		if err != nil {
			return nil, fmt.Errorf("invalid work_dir: %w", err)
		}
		cmd.Dir = absDir
	}

	// 继承 PATH 等必要环境变量
	cmd.Env = buildCommandEnv()

	stdout := bufutil.NewCappedBuffer(1024 * 1024) // 1MB 限制
	stderr := bufutil.NewCappedBuffer(1024 * 1024)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	result := map[string]any{
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
		"duration":  duration.Round(time.Millisecond).String(),
		"truncated": stdout.Truncated() || stderr.Truncated(),
	}

	if ctx.Err() == context.DeadlineExceeded {
		result["timed_out"] = true
		result["exit_code"] = -1
	} else if runErr == nil {
		result["exit_code"] = 0
	} else {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			result["exit_code"] = exitErr.ExitCode()
		} else {
			result["exit_code"] = -1
			result["error"] = runErr.Error()
		}
	}

	return result, nil
}

// ============================================================================
// 危险命令拦截
// ============================================================================

// blockedCommands 直接拦截的命令片段（全小写比较）
var blockedCommands = []string{
	"rm -rf /",
	"rm -rf /*",
	"mkfs.",
	"dd if=",
	"format ",
	"del /f /s /q",
	"rmdir /s /q",
}

// checkDangerousCommand 检查命令是否危险
func checkDangerousCommand(command string) error {
	lower := strings.ToLower(strings.TrimSpace(command))

	for _, blocked := range blockedCommands {
		if strings.Contains(lower, blocked) {
			return fmt.Errorf("dangerous command blocked: contains %q", blocked)
		}
	}

	return nil
}

// buildCommandEnv 构建命令执行的环境变量
func buildCommandEnv() []string {
	safeKeys := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "SHELL": true,
		"LANG": true, "LC_ALL": true, "LC_CTYPE": true,
		"TMPDIR": true, "TEMP": true, "TMP": true,
		"TERM": true, "XDG_RUNTIME_DIR": true,
		"SYSTEMROOT": true, "WINDIR": true, "COMSPEC": true,
		"PATHEXT": true, "OS": true, "PROGRAMFILES": true,
		"LOCALAPPDATA": true, "APPDATA": true, "USERPROFILE": true,
	}

	var env []string
	for _, entry := range os.Environ() {
		idx := strings.IndexByte(entry, '=')
		if idx < 0 {
			continue
		}
		key := entry[:idx]
		if safeKeys[key] || safeKeys[strings.ToUpper(key)] {
			env = append(env, entry)
		}
	}
	return env
}

// absPath 获取绝对路径
func absPath(path string) (string, error) {
	return filepath.Abs(path)
}

// ============================================================================
// 注册
// ============================================================================

var commandExecParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"command": map[string]any{
			"type":        "string",
			"description": "要执行的 shell 命令",
		},
		"work_dir": map[string]any{
			"type":        "string",
			"description": "工作目录（可选，默认当前目录）",
		},
		"timeout": map[string]any{
			"type":        "number",
			"description": "超时秒数（默认 30）",
		},
	},
	"required": []string{"command"},
}

func RegisterCommandTools(registry *ToolRegistry) {
	registry.MustRegister(ToolMetadata{
		Name: "command_exec",
		Description: "执行 shell 命令并返回 stdout/stderr。" +
			"支持超时控制（默认 30 秒），输出限制 1MB。" +
			"危险命令（rm -rf /、mkfs、dd 等会被自动拦截）。",
		Parameters: commandExecParams,
		Permission: PermDangerous,
		Category:   "system",
		Version:    "1.0.0",
		Tags:       []string{"system", "shell", "command", "dangerous"},
		Timeout:    30 * time.Second,
	}, CommandExec)
}
