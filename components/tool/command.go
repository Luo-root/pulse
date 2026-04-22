package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// CommandExec 执行命令（带超时）
func CommandExec(ctx context.Context, args map[string]any) (any, error) {
	command, _ := args["command"].(string)
	timeoutSec, _ := args["timeout"].(float64)
	if timeoutSec == 0 {
		timeoutSec = 30
	}

	// 安全：限制危险命令
	dangerous := []string{"rm -rf /", "mkfs", "dd if=/dev/zero"}
	for _, d := range dangerous {
		if strings.Contains(command, d) {
			return nil, fmt.Errorf("dangerous command blocked: %s", command)
		}
	}

	// 创建带超时的 context
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	output, err := cmd.CombinedOutput()

	result := map[string]string{
		"command": command,
		"output":  string(output),
	}

	if err != nil {
		result["error"] = err.Error()
		if ctx.Err() == context.DeadlineExceeded {
			result["error"] = "timeout"
		}
		return result, nil // 返回错误但不抛异常，让模型看到输出
	}

	result["status"] = "success"
	return result, nil
}

func RegisterCommandExecTools(executor *schema.ToolExecutor) {
	executor.MustRegister(schema.Tool{
		Name:        "command_exec",
		Description: "执行系统命令，返回输出结果",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "要执行的命令"},
				"timeout": map[string]any{"type": "number", "description": "超时时间（秒），默认30"},
			},
			"required": []string{"command"},
		},
	}, CommandExec)
}
