//go:build windows

package builtins

import (
	"context"
	"os/exec"
)

// buildShellCommand 在 Windows 上用 PowerShell 执行命令字符串。
func buildShellCommand(ctx context.Context, command string) *exec.Cmd {
	// -NoProfile 减少噪声；-NonInteractive 避免提示；命令作单独参数避免二次展开。
	return exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		command,
	)
}
