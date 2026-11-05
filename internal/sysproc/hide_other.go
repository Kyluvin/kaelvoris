//go:build !windows

// Package sysproc —— 非 Windows 平台无控制台窗口概念，空操作。
package sysproc

import "os/exec"

// Hide 空操作（POSIX 无需隐藏控制台）。
func Hide(cmd *exec.Cmd) {}

// HideWindowOnly POSIX 无控制台窗口概念，空操作（与 Windows 版语义对齐）。
func HideWindowOnly(cmd *exec.Cmd) {}
