//go:build !windows

package builtins

import (
	"context"
	"os/exec"
	"syscall"
)

// buildShellCommand 在 Unix 上用 sh -c 执行命令字符串。
func buildShellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}

// setupBackgroundProcess 让后台 job 独立进程组，kill 时可整组 SIGKILL。
func setupBackgroundProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree 对进程组整组 SIGKILL（background 进程已 Setpgid）。
func killTree(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
