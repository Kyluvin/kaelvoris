//go:build !windows

// hideConsole —— 非 Windows 平台无控制台窗口概念，空操作。
package main

import "os/exec"

// hideConsole 空操作（POSIX 无需隐藏控制台）。
func hideConsole(cmd *exec.Cmd) {}
