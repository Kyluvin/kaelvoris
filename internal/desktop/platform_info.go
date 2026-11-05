package desktop

import (
	"os"
	"runtime"
	"strings"
)

// 平台小工具（原 machine_id.go 共享辅助；机器指纹已随 node.token 去令牌化移除，
// 这两个函数被 local_ip.go / version_panel.go 等继续使用，独立落盘）。

// goos 当前平台（独立小函数：便于测试桩替换）。
func goos() string { return runtime.GOOS }

// hostnameSafe 主机名（失败返回空，不 panic）。
func hostnameSafe() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(h)
}
