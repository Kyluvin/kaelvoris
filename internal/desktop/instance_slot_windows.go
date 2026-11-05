//go:build windows

package desktop

import "golang.org/x/sys/windows"

// pidAlive 检测进程是否存活（Windows：OpenProcess 查询，进程不存在时返回 false）。
// PROCESS_QUERY_LIMITED_INFORMATION 权限足够判断存活，且对同用户进程必然放行。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = windows.CloseHandle(h)
	return true
}
