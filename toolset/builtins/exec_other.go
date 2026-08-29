//go:build !windows && !unix

package builtins

import (
	"context"
	"fmt"
	"os/exec"
)

// buildShellCommand 在其他平台（plan9 / js-wasm）按 sh -c 兜底。
func buildShellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}

// setupBackgroundProcess：无进程组 API；空操作。
func setupBackgroundProcess(cmd *exec.Cmd) {}

// killTree 在无进程组 API 的平台不可用：job_kill 走 cancel() 兜底，
// 只杀包装 shell，孙子进程不保证（README 已写明该边界）。
func killTree(pid int) error {
	return fmt.Errorf("builtins/exec: process-group kill unsupported on this platform (pid %d)", pid)
}
