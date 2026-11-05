//go:build !windows

// 非 Windows 锁辅助：无独占打开语义的零依赖模拟，直接跳过自锁用例。
package tools

import "os"

// lockExclusive 非 Windows 不模拟独占锁（flock/fcntl 语义与 Windows 共享冲突模型不同）。
func lockExclusive(_ string) error { return os.ErrPermission }

// unlockFile 非 Windows 无操作。
func unlockFile(_ string) {}
