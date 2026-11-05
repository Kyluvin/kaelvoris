//go:build !windows

package agent

import "runtime"

// osInfo 返回操作系统描述（对齐 system-info.ts 的 os-name()；非 Windows 走 runtime 信息）。
func osInfo() string {
	return runtime.GOOS + " " + runtime.GOARCH
}
