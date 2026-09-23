//go:build unix

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

// setupProcessTree 让命令自成进程组，killTree 才能整组 SIGKILL（前台与后台
// 命令同此处理：包装 shell 拉起的子进程都在这个组里）。
func setupProcessTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree 对进程组整组 SIGKILL（命令已由 setupProcessTree 设 Setpgid）。
func killTree(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
