//go:build windows

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func setupProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	cmd.Cancel = func() error {
		killProcessTree(cmd.Process)
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 3 * time.Second
}

// killProcessTree 在 Windows 上用 taskkill 杀掉整个进程树
func killProcessTree(p *os.Process) {
	if p == nil {
		return
	}
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", p.Pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Run() // 忽略错误, 进程可能已退出
}
