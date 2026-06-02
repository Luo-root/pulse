//go:build linux || darwin

package sandbox

import (
	"os"
	"os/exec"
)

func setupProcess(cmd *exec.Cmd) {
	// Unix 系统不需要特殊设置
}

func killProcessTree(p *os.Process) {
	// Unix 系统使用 Signal 即可
	if p != nil {
		p.Kill()
	}
}
