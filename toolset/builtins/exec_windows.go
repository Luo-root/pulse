//go:build windows

package builtins

import (
	"context"
	"os/exec"
	"strconv"
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

// setupBackgroundProcess：Windows 不设进程组；整树杀走 taskkill /T。
func setupBackgroundProcess(cmd *exec.Cmd) {}

// killTree 杀整个进程树（含包装 shell 起的子进程）。
func killTree(pid int) error {
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
