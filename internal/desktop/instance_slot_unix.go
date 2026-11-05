//go:build !windows

package desktop

import "syscall"

// pidAlive 检测进程是否存活（Unix：kill(pid, 0) 探测；ESRCH=不存在，EPERM=存在但无权限）。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
