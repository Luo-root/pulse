//go:build windows

package lsp

import (
	"context"
	"os/exec"
	"strconv"
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

// setupProcessGroup：Windows 不设进程组；整树杀走 taskkill /T。
func setupProcessGroup(cmd *exec.Cmd) {}

// killTree 杀整个进程树（语言服务器可能 spawn 子进程）。
func killTree(pid int) error {
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
