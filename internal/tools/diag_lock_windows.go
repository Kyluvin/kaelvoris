//go:build windows

// 诊断工具测试辅助：无共享语义独占打开文件（模拟第三方进程持锁），
// 供 TestFileLockerTool_SelfLocked 验证 file_locker 的锁定识别路径。
package tools

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// testLockHolder 持锁句柄（测试内可释放，避免 TempDir 清理失败）。
var testLockHolder windows.Handle

// lockExclusive 以无共享（0 share mode）打开文件，制造共享冲突。
// 成功后调用方必须 defer unlockFile() 释放，否则 TempDir 清理会失败。
func lockExclusive(path string) error {
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, _, callErr := procCreateFileW.Call(
		uintptr(unsafe.Pointer(p16)),
		genericRead, // 仅读访问 + 零共享 → 他人 GENERIC_WRITE 打开必然失败
		0,           // 无任何共享
		0, openExisting, 0, 0)
	if h == uintptr(windows.InvalidHandle) || h == 0 {
		return callErr
	}
	testLockHolder = windows.Handle(h)
	return nil
}

// unlockFile 释放 lockExclusive 持有的句柄。
func unlockFile(_ string) {
	if testLockHolder != 0 {
		_ = windows.CloseHandle(testLockHolder)
		testLockHolder = 0
	}
}
