//go:build !windows

// procgroup_unix.go —— 非 Windows 进程组：子进程入独立进程组，终止时整树 SIGKILL。
// npx 等启动器会派生孙进程；仅 Kill 直接子进程会残留孤儿（与 Windows taskkill /T 对齐）。
package acp

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup 子进程入新进程组（setpgid），killTree 可按组灭全树。
func setupProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree 终止整进程组（负 PID = 组内全部进程，含启动器派生的孙进程）。
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
