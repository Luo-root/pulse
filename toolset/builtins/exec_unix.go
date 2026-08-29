//go:build !windows

package builtins

import (
	"context"
	"os/exec"
)

// buildShellCommand 在 Unix 上用 sh -c 执行命令字符串。
func buildShellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}
