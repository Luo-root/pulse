//go:build !windows && !unix

package lsp

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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

// setupProcessGroup：无进程组 API；空操作。
func setupProcessGroup(cmd *exec.Cmd) {}

// killTree 在无进程组 API 的平台不可用：清理走 cancel 兜底，只杀包装进程。
func killTree(pid int) error {
	return fmt.Errorf("lsp: process-group kill unsupported on this platform (pid %d)", pid)
}
