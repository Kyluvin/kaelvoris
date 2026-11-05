//go:build windows

// procgroup_windows.go —— Windows 进程树终止：taskkill /T 按父子树整链杀。
// ACP agent 经 cmd /C 启动（npx/node 为 .cmd 包装），直接 TerminateProcess
// 只杀 cmd.exe，npx/node 孙进程残留——必须 taskkill /T 才能杀整棵树。
package acp

import (
	"os/exec"
	"strconv"

	"kaelvoris/internal/sysproc"
)

// setupProcessGroup Windows 无 POSIX 进程组语义，树终止由 taskkill /T 实现（空操作）。
func setupProcessGroup(cmd *exec.Cmd) {}

// killTree 终止整棵进程树：taskkill /T /F /PID（cmd /C → npx → node 全链）。
// 进程已退出时 taskkill 报错（调用方忽略）；Hide 防 GUI 宿主下闪控制台黑框。
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	sysproc.Hide(k)
	return k.Run()
}
