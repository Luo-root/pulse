//go:build unix

package lsp

import (
	"context"
	"os/exec"
	"strings"
	"syscall"
)

// buildServerCommand 解析 server 启动命令（strings.Fields 分词；不支持引号路径）。
func buildServerCommand(ctx context.Context, command string) *exec.Cmd {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return exec.Command("")
	}
	if len(parts) == 1 {
		return exec.CommandContext(ctx, parts[0])
	}
	return exec.CommandContext(ctx, parts[0], parts[1:]...)
}

// setupProcessGroup 让语言服务器独立进程组，kill 时整组 SIGKILL。
func setupProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree 对进程组整组 SIGKILL。
func killTree(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
