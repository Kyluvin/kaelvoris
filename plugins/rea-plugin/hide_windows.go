//go:build windows

// hideConsole —— Windows 实现：CREATE_NO_WINDOW 隐藏控制台黑框。
//
// 背景：本桥接进程由桌面端经 MCP stdio spawn（自身无控制台），但其
// spawn 的 npx/node 是控制台程序，系统会新建可见控制台窗口。
// 与主工程 internal/sysproc 同语义的独立最小实现（零主工程 import）。
package main

import (
	"os/exec"
	"syscall"
)

// createNoWindow 对齐 Windows CREATE_NO_WINDOW（0x08000000）。
const createNoWindow = 0x08000000

// hideConsole 隐藏子进程控制台窗口。
func hideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
