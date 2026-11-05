package tools

// 临时调试：句柄枚举/类型索引/命令行读取内部状态（验证后删除）。

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestDebugFileLockInternal(t *testing.T) {
	dir, _ := os.MkdirTemp("", "dbglock")
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "locked.txt")
	_ = os.WriteFile(p, []byte("x"), 0o644)

	// 独占打开
	h, err := windows.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	hh, _, callErr := procCreateFileW.Call(
		uintptr(unsafe.Pointer(h)), genericRead, 0, 0, openExisting, 0, 0)
	if hh == 0 || hh == uintptr(windows.InvalidHandle) {
		t.Fatalf("exclusive open failed: %v", callErr)
	}
	defer func() { _ = windows.CloseHandle(windows.Handle(hh)) }()

	t.Logf("fileObjectTypeID()=%d", fileObjectTypeID())
	t.Logf("fileObjectTypeIndex=%d", fileObjectTypeIndex)

	// 调试：直接调 NtQuerySystemInformation(SystemHandleInformation) 看状态
	var size uint32 = 1 << 22
	buf := make([]byte, size)
	var ret uint32
	st, _, _ := procNtQuerySystemInformationP.Call(
		systemHandleInformation,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&ret)))
	t.Logf("SystemHandleInformation st=%#x ret=%d bufSize=%d", st, ret, size)
	if st == 0 {
		count := *(*uint32)(unsafe.Pointer(&buf[0]))
		t.Logf("handle count=%d entrySize=%d", count, unsafe.Sizeof(sysHandleEntry{}))

		// 自身进程的句柄匹配：找独占打开的那个句柄值，验证 type 索引提取
		myPID := uint16(windows.GetCurrentProcessId())
		handleVal := uint16(uint32(hh) & 0xFFFC) // 句柄值低 2 位是标志位
		t.Logf("myPID=%d target handle=%#x (raw %#x)", myPID, handleVal, uint32(hh))
		base := unsafe.Pointer(&buf[sysHandleInfoHeaderSize])
		entrySize := unsafe.Sizeof(sysHandleEntry{})
		for i := uint32(0); i < count; i++ {
			e := (*sysHandleEntry)(unsafe.Add(base, uintptr(i)*entrySize))
			if e.ProcessID == myPID {
				if e.Handle&0xFFFC == handleVal {
					t.Logf("MATCH pid=%d handle=%#x typeIdx=%d attr=%d access=%#x",
						e.ProcessID, e.Handle, e.ObjectTypeIndex, e.HandleAttributes, e.GrdAccess)
				}
			}
		}
	}

	lockers := findFileLockersWindows(p)
	t.Logf("lockers=%+v", lockers)
}
