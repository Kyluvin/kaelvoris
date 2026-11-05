package tools

// 临时调试：detectFileObjectTypeIndex 逐层排查（验证后删除）。

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestDebugDetectProbe(t *testing.T) {
	// 复刻 detectFileObjectTypeIndex 的逻辑，逐步打日志
	dir := os.TempDir()
	probe := filepath.Join(dir, ".kaelvoris-filelocker-probe")
	f, err := os.Create(probe)
	if err != nil {
		t.Fatalf("create probe: %v", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(probe)
	}()

	raw := uint32(f.Fd())
	t.Logf("raw fd=%#x", raw)
	target := uint16(raw & 0xFFFC)
	t.Logf("target handle u16=%#x", target)

		// 直接读原始 buffer（不走 walkHandleTableCollect，排除其 panic/recover）；
		// 缓冲区不足 STATUS_INFO_LENGTH_MISMATCH 时倍增重试（与正式实现同语义，
		// 否则句柄数多时误报失败）。
		var size uint32 = 1 << 22
		var buf []byte
		var ret uint32
		var st uintptr
		for attempt := 0; attempt < 3; attempt++ {
			buf = make([]byte, size)
			st, _, _ = procNtQuerySystemInformationP.Call(
				systemHandleInformation,
				uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&ret)))
			if st == statusInfoLengthMismatch {
				size *= 2
				continue
			}
			break
		}
		t.Logf("st=%#x ret=%d size=%d", st, ret, size)
		if st != 0 {
			t.Fatal("handle query failed")
		}
	count := *(*uint32)(unsafe.Pointer(&buf[0]))
	t.Logf("count=%d", count)

	myPID := uint16(windows.GetCurrentProcessId())
	n := 0
	base := unsafe.Pointer(&buf[sysHandleInfoHeaderSize])
	entrySize := unsafe.Sizeof(sysHandleEntry{})
	for i := uint32(0); i < count; i++ {
		e := (*sysHandleEntry)(unsafe.Add(base, uintptr(i)*entrySize))
		if e.ProcessID == myPID {
			n++
			if n <= 5 {
				t.Logf("self e[%d] pid=%d handle=%#x type=%d", i, e.ProcessID, e.Handle, e.ObjectTypeIndex)
			}
			if e.Handle&0xFFFC == target {
				t.Logf("FOUND target handle: type=%d", e.ObjectTypeIndex)
				return
			}
		}
	}
	t.Logf("self handles=%d, target NOT found (probe file may have been closed/reopened)", n)
}
