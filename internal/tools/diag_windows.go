//go:build windows

// Package tools —— Windows 平台诊断实现：进程/端口快照 + 文件占用者检测。
//
// 实现选型（零 CGO、零新增第三方依赖，golang.org/x/sys 已在依赖树）：
//   - 进程枚举：CreateToolhelp32Snapshot 进程快照（PID/PPID/EXE 名）；
//     ExePath 经 NtQuerySystemInformation(SystemProcessInformation) 的
//     ImageName 字段（x64 偏移 0x38，PID 在 0x50）补齐；
//     命令行不在该结构体内，走 OpenProcess + NtQueryInformationProcess
//     读 PEB→ProcessParameters→CommandLine（同用户进程无需管理员）。
//   - 端口归属：GetExtendedTcpTable（owner-pid 表，iphlpapi.dll）拿
//     (PID ↔ 本地端口) 映射，等效 netstat -ano，纯 API 无子进程、无转义。
//   - 文件占用者：NtQuerySystemInformation(SystemHandleInformation) 枚举
//     全系统句柄；先用"自建临时文件句柄反查自身条目"拿到本系统 File 对象
//     的类型索引，再只对 File 类型句柄做 Duplicate + GetFinalPathNameByHandleW
//     路径比对（避免全表 NtQueryObject，规避命名管道句柄查询挂起）。
package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---------- 常量 ----------

const (
	th32csSnapProcess        = 0x00000002
	tcpTableOwnerPIDListener = 3
	tcpTableOwnerPIDAll      = 5
	afInet                   = 2
	processDupHandle         = 0x0040
	processQueryLimitedInfo  = 0x1000
	processVmRead            = 0x0010
	processBasicInformation  = 0
	fileShareRead            = 0x00000001
	fileShareWrite           = 0x00000002
	fileShareDelete          = 0x00000004
	genericRead              = 0x80000000
	fileTypeDisk             = 0x0001 // GetFileType：磁盘文件句柄
	genericWrite             = 0x40000000
	openExisting             = 3
	statusInfoLengthMismatch = 0xC0000004
	systemProcessInformation = 5
	systemHandleInformation  = 16
)

// ---------- Win32 结构 ----------

type processEntry32W struct {
	Size            uint32
	Usage           uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	Threads         uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [windows.MAX_PATH]uint16
}

type mibTcpRowOwnerPid struct {
	State      uint32
	LocalAddr  uint32
	LocalPort  uint32
	RemoteAddr uint32
	RemotePort uint32
	OWningPid  uint32
}

// sysUniStr UNICODE_STRING（x64 与 x86 自然对齐布局均适用）。
type sysUniStr struct {
	Len    uint16
	MaxLen uint16
	Buf    uintptr
}

// sysHandleEntry SYSTEM_HANDLE_TABLE_ENTRY_INFO（SystemHandleInformation=16）。
// 实测 Win10 x64 字节布局（字节级扫描 + 自句柄回查确认，头部 8B + 每条 24B）：
//
//	+0x00 ProcessId     uint16（PID；CreatorBackTrace 紧随其後）
//	+0x02 CreatorBackTrace uint16
//	+0x04 ObjectTypeIndex uint8（实测 File=0x25/37，随系统版本变化）
//	+0x05 HandleAttributes uint8
//	+0x06 HandleValue   uint16（win32 句柄值，低 2 位为标志位）
//	+0x08 Object        uintptr（内核对象地址）
//	+0x10 GrantedAccess uint32
//	+0x14 pad           uint32
//
// 即 PHNT 经典紧凑布局；头部为 8 字节（NumberOfHandles u32 + Reserved u32），
// 初稿误按 4 字节切头导致全表偏移错位（PID 全为 0、自句柄永不命中）。
type sysHandleEntry struct {
	ProcessID        uint16
	CreatorBackTrace uint16
	ObjectTypeIndex  uint8
	HandleAttributes uint8
	Handle           uint16
	Object           uintptr
	GrdAccess        uint32
	_                uint32
}

// 头部字节数：8（NumberOfHandles u32 + Reserved u32）。
const sysHandleInfoHeaderSize = 8

// ---------- API 绑定 ----------

var (
	kernel32Proc = windows.NewLazySystemDLL("kernel32.dll")
	iphlpapiProc = windows.NewLazyDLL("iphlpapi.dll") // GetExtendedTcpTable 在 iphlpapi，非 kernel32
	ntdllProc    = windows.NewLazySystemDLL("ntdll.dll")

	procCreateToolhelp32SnapshotP = kernel32Proc.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW           = kernel32Proc.NewProc("Process32FirstW")
	procProcess32NextW            = kernel32Proc.NewProc("Process32NextW")
	procGetExtendedTcpTableP      = iphlpapiProc.NewProc("GetExtendedTcpTable")
	procCreateFileW               = kernel32Proc.NewProc("CreateFileW")
	procGetFileTypeP              = kernel32Proc.NewProc("GetFileType")
	procGetFinalPathNameByHandleP = kernel32Proc.NewProc("GetFinalPathNameByHandleW")
	procQueryDosDeviceP           = kernel32Proc.NewProc("QueryDosDeviceW")

	procNtQuerySystemInformationP  = ntdllProc.NewProc("NtQuerySystemInformation")
	procNtQueryInformationProcessP = ntdllProc.NewProc("NtQueryInformationProcess")
	procNtDuplicateObjectP         = ntdllProc.NewProc("NtDuplicateObject")
	procReadProcessMemoryP         = kernel32Proc.NewProc("ReadProcessMemory")
)

// ---------- 平台入口（diag_tools.go 依赖的平台契约） ----------

// listProcs 枚举进程（PID/PPID/名称），ExePath/命令行尽力补齐。
// 签名与 diag_other.go 平台契约一致（第二参为预留过滤词，Windows 全量枚举忽略）。
func listProcs(_ string) ([]ProcInfo, error) {
	snap, _, err := procCreateToolhelp32SnapshotP.Call(th32csSnapProcess, 0)
	if snap == uintptr(windows.InvalidHandle) || snap == 0 {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot failed: %v", err)
	}
	defer windows.CloseHandle(windows.Handle(snap))

	var procs []ProcInfo
	entry := processEntry32W{Size: uint32(unsafe.Sizeof(processEntry32W{}))}
	r, _, _ := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&entry)))
	for r != 0 {
		procs = append(procs, ProcInfo{
			PID:  int(entry.ProcessID),
			PPID: int(entry.ParentProcessID),
			Name: windows.UTF16ToString(entry.ExeFile[:]),
		})
		r, _, _ = procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&entry)))
	}
	fillImagePaths(procs)
	fillCmdlines(procs)
	return procs, nil
}

// utf16SliceAt 把 NT 内核缓冲内的 UTF-16 字符串读为切片。
// addr 为缓冲区内绝对地址（UNICODE_STRING.Buf），base 为缓冲首地址：
// 两者同属 NtQuerySystemInformation 本次调用返回的 Go 字节缓冲，相对偏移
// 差值 + unsafe.Add 转换（vet 白名单模式）避免 uintptr→unsafe.Pointer
// 直转的 unsafeptr 误报；生命周期覆盖读取期。
func utf16SliceAt(base unsafe.Pointer, addr uintptr, n int) []uint16 {
	rel := addr - uintptr(base)
	return unsafe.Slice((*uint16)(unsafe.Add(base, rel)), n)
}

// collectPorts 枚举占用指定端口的进程（监听 + 连接）。
func collectPorts(port int, pidSet map[int]bool, lines map[int][]string) {
	for pid, l := range collectTCPTable(tcpTableOwnerPIDListener, port) {
		pidSet[pid] = true
		lines[pid] = append(lines[pid], l...)
	}
	for pid, l := range collectTCPTable(tcpTableOwnerPIDAll, port) {
		pidSet[pid] = true
		lines[pid] = append(lines[pid], l...)
	}
}

// findFileLockers 句柄级锁定者枚举（Windows 实现）。
func findFileLockers(path string) []Locker {
	return findFileLockersWindows(path)
}

// ---------- 映像路径 / 命令行补齐 ----------

// fillImagePaths 用 NtQuerySystemInformation(SystemProcessInformation) 的
// ImageName 补齐 ExePath。x64 SYSTEM_PROCESS_INFORMATION 布局（实测 Win10）
// 与 PHNT 头文件一致：NextEntryOffset(0x00) NumberOfThreads(0x04) /
// WorkingSetPrivateSize(0x08) ... ImageName(0x38) UniqueProcessId(0x50) /
// InheritedFromUniqueProcessId(0x58)；线程数组紧跟其后，我们只读头部。
func fillImagePaths(procs []ProcInfo) {
	defer func() { _ = recover() }() // 布局/权限差异兑底：绝不影响主流程
	buf, ok := ntQuerySystemBuffer(systemProcessInformation)
	if !ok {
		return
	}
	base := unsafe.Pointer(&buf[0])
	off := uintptr(0)
	imgOf := map[uint32]string{}
	for {
		if off+0x60 > uintptr(len(buf)) {
			break
		}
		next := *(*uint32)(unsafe.Add(base, off))
		pid := *(*uint32)(unsafe.Add(base, off+0x50))
		if pid != 0 {
			img := (*sysUniStr)(unsafe.Add(base, off+0x38))
			if img.Buf != 0 && img.Len > 0 && int(img.Len) <= 1024 {
				imgOf[pid] = windows.UTF16ToString(utf16SliceAt(base, img.Buf, int(img.Len)/2))
			}
		}
		if next == 0 {
			break
		}
		off += uintptr(next)
	}
	for i := range procs {
		if e, ok := imgOf[uint32(procs[i].PID)]; ok {
			procs[i].ExePath = e
		}
	}
}

// fillCmdlines 补齐命令行：命令行在 PEB→ProcessParameters→CommandLine
// （NtQuerySystemInformation 不含它）。同用户进程用
// PROCESS_QUERY_LIMITED_INFORMATION|PROCESS_VM_READ 即可读取。
func fillCmdlines(procs []ProcInfo) {
	defer func() { _ = recover() }()
	for i := range procs {
		if c := cmdlineOfPID(uint32(procs[i].PID)); c != "" {
			procs[i].CmdLine = c
		}
	}
}

// cmdlineOfPID 读单个进程的 PEB 命令行（失败返回空）。
func cmdlineOfPID(pid uint32) string {
	defer func() { _ = recover() }()
	h, err := windows.OpenProcess(processQueryLimitedInfo|processVmRead, false, pid)
	if err != nil || h == 0 {
		return ""
	}
	defer windows.CloseHandle(h)

	// PROCESS_BASIC_INFORMATION：x64 = PebBaseData(0)+Affinity(8)+BasePriority(16)+
	// UniqueProcessId(24)+InheritedFrom(32)+PebBaseAddress(40)；x86 = 20 字节尾。
	type pbi struct {
		ExitStatus       uintptr
		PebBaseAddress   uintptr
		AffinityMask     uintptr
		BasePriority     uintptr
		UniqueProcessId  uintptr
		InheritedFromPID uintptr
	}
	var info pbi
	var ret uint32
	st, _, _ := procNtQueryInformationProcessP.Call(
		uintptr(h), processBasicInformation,
		uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Sizeof(info)), uintptr(unsafe.Pointer(&ret)))
	if st != 0 || info.PebBaseAddress == 0 {
		return ""
	}

	// PEB x64: Reserved(0x00..0x10) ProcessParameters@0x20；x86: @0x10。
	paramsOff := uintptr(0x10)
	ptrSize := uintptr(4)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		paramsOff = 0x20
		ptrSize = 8
	}
	var params uintptr
	if !readRemote(info.PebBaseAddress+paramsOff, uintptr(unsafe.Pointer(&params)), ptrSize) {
		return ""
	}
	if params == 0 {
		return ""
	}
	// RTL_USER_PROCESS_PARAMETERS：CommandLine 是第 6 个 UNICODE_STRING。
	// x64 头部：MaximumLength(0) Length(2) Flags(4) DebugFlags(6) ConsoleHandle(8)
	// ConsoleFlags(0x10) InputHandle(0x18) OutputHandle(0x20) ErrorHandle(0x28)
	// CurrentDirectory(0x28..0x38) DllPath(0x38 UNICODE_STRING) ImagePathName(0x48)
	// CommandLine(0x58)。x86 对应 CommandLine@0x40。
	cmdOff := uintptr(0x40)
	if ptrSize == 8 {
		cmdOff = 0x58
	}
	var cmd sysUniStr
	if !readRemote(params+cmdOff, uintptr(unsafe.Pointer(&cmd)), unsafe.Sizeof(cmd)) {
		return ""
	}
	if cmd.Buf == 0 || cmd.Len == 0 || int(cmd.Len) > 32768 {
		return ""
	}
	buf := make([]byte, cmd.Len)
	if !readRemote(cmd.Buf, uintptr(unsafe.Pointer(&buf[0])), uintptr(cmd.Len)) {
		return ""
	}
	return windows.UTF16ToString((*[1 << 15]uint16)(unsafe.Pointer(&buf[0]))[:int(cmd.Len)/2])
}

// readRemote 跨进程读内存（ReadProcessMemory，目标为当前进程时也成立）。
// 返回 true 表示读满 size 字节。
func readRemote(addr, dst, size uintptr) bool {
	var read uintptr
	r, _, _ := procReadProcessMemoryP.Call(
		uintptr(windows.CurrentProcess()), addr, dst, size,
		uintptr(unsafe.Pointer(&read)))
	return r != 0 && read == size
}

// ntQuerySystemBuffer 统一的 NtQuerySystemInformation 调用（带指数扩容）。
func ntQuerySystemBuffer(class uint32) ([]byte, bool) {
	var size uint32 = 1 << 20
	for attempt := 0; attempt < 3; attempt++ {
		buf := make([]byte, size)
		var ret uint32
		st, _, _ := procNtQuerySystemInformationP.Call(
			uintptr(class),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(size),
			uintptr(unsafe.Pointer(&ret)))
		if st == statusInfoLengthMismatch {
			size *= 2
			continue
		}
		if st != 0 {
			return nil, false
		}
		return buf, true
	}
	return nil, false
}

// ---------- 端口归属 ----------

// collectTCPTable 读一张 owner-pid TCP 表并筛出目标端口行。
func collectTCPTable(tableClass uint32, port int) map[int][]string {
	out := map[int][]string{}
	var size uint32
	_, _, _ = procGetExtendedTcpTableP.Call(0, uintptr(unsafe.Pointer(&size)), 0, afInet, uintptr(tableClass), 0)
	if size == 0 {
		return out
	}
	buf := make([]byte, size)
	st, _, _ := procGetExtendedTcpTableP.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		0, afInet, uintptr(tableClass), 0)
	if st != 0 {
		return out
	}
	numEntries := *(*uint32)(unsafe.Pointer(&buf[0]))
	rowSize := unsafe.Sizeof(mibTcpRowOwnerPid{})
	if max := uint32((len(buf) - 4) / int(rowSize)); numEntries > max {
		numEntries = max
	}
	base := unsafe.Pointer(&buf[4])
	for i := uint32(0); i < numEntries; i++ {
		row := (*mibTcpRowOwnerPid)(unsafe.Add(base, uintptr(i)*rowSize))
		localPort := swapPort(row.LocalPort)
		if int(localPort) != port {
			continue
		}
		s := fmt.Sprintf(":%d", localPort)
		if row.RemoteAddr != 0 || row.RemotePort != 0 {
			s += fmt.Sprintf(" <-> %s:%d", ipv4String(row.RemoteAddr), swapPort(row.RemotePort))
		}
		s += " (" + tcpStateString(row.State) + ")"
		out[int(row.OWningPid)] = append(out[int(row.OWningPid)], s)
	}
	return out
}

// swapPort GetExtendedTcpTable 端口字段：网络序 DWORD，低 16 位按字节交换即主机序。
func swapPort(v uint32) uint16 {
	return uint16(v>>8&0xff) | uint16(v&0xff)<<8
}

func ipv4String(v uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", v&0xff, (v>>8)&0xff, (v>>16)&0xff, (v>>24)&0xff)
}

func tcpStateString(s uint32) string {
	switch s {
	case 1:
		return "LISTEN"
	case 2:
		return "SYN_SENT"
	case 3:
		return "SYN_RCVD"
	case 4:
		return "ESTABLISHED"
	case 8:
		return "CLOSE_WAIT"
	case 12:
		return "TIME_WAIT"
	default:
		return fmt.Sprintf("state=%d", s)
	}
}

// ---------- 文件占用者 ----------

// fileObjectTypeIndex 探测出的本系统 File 对象类型索引（惰性缓存：
// 包初始化阶段调用 NT API 时机不稳，首次使用时再探测）。
var (
	fileObjectTypeIndexOnce sync.Once
	fileObjectTypeIndex     uint8
)

// fileObjectTypeID 返回 File 类型索引（首次调用时探测，失败重试到成功为止）。
func fileObjectTypeID() uint8 {
	fileObjectTypeIndexOnce.Do(func() {
		fileObjectTypeIndex = detectFileObjectTypeIndex()
	})
	return fileObjectTypeIndex
}

// detectFileObjectTypeIndex 自建一个临时文件句柄，在句柄表里反查自身条目
// 得到本系统的 File 对象类型索引（比逐条 NtQueryObject 快且不挂起）。
func detectFileObjectTypeIndex() uint8 {
	defer func() { _ = recover() }()
	dir := os.TempDir()
	probe := filepath.Join(dir, ".kaelvoris-filelocker-probe")
	f, err := os.Create(probe)
	if err != nil {
		return 0
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(probe)
	}()

	// 目标句柄值（os.File 底层 NT 句柄；条目 Handle 字段为 u16，去掉低 2 位标志）。
	raw := uint32(f.Fd())
	target := uint16(raw & 0xFFFC)
	var idx uint8
	walkHandleTableCollect(func(e *sysHandleEntry) {
		if e.ProcessID != uint16(windows.GetCurrentProcessId()) {
			return
		}
		if e.Handle&0xFFFC != target {
			return
		}
		idx = e.ObjectTypeIndex
	})
	return idx
}

// findFileLockersWindows 句柄级锁定者枚举：File 类型句柄 → Duplicate →
// GetFinalPathNameByHandleW → 路径比对目标文件。
func findFileLockersWindows(path string) []Locker {
	defer func() { _ = recover() }()
	fileIdx := fileObjectTypeID()
	if fileIdx == 0 {
		// 类型索引探测失败：退回逐条 NtQueryObject 太慢/易挂起，直接放弃
		// 句柄级识别，交由调用方写打开探测兜底。
		return nil
	}
	target := strings.ToLower(filepath.Clean(path))

	seen := map[int]bool{}
	seenPIDs := map[uint16]bool{} // File 句柄去重缓存：同名进程只 OpenProcess 一次
	procHandles := map[uint16]windows.Handle{}
	defer func() {
		for _, h := range procHandles {
			_ = windows.CloseHandle(h)
		}
	}()
	var out []Locker
	walkHandleTableCollect(func(e *sysHandleEntry) {
		pid := e.ProcessID
		if pid <= 4 || seen[int(pid)] || e.ObjectTypeIndex != fileIdx {
			return
		}
		// 进程句柄缓存：每个 PID 只 OpenProcess 一次（避免 8 万次系统调用）。
		hProc, ok := procHandles[pid]
		if !ok {
			h, err := windows.OpenProcess(processDupHandle, false, uint32(pid))
			if err != nil || h == 0 {
				seenPIDs[pid] = true // 标记不可打开，后续条目直接跳过
				seen[int(pid)] = true
				return
			}
			procHandles[pid] = h
			hProc = h
		}
		var dup windows.Handle
		st, _, _ := procNtDuplicateObjectP.Call(
			uintptr(hProc), uintptr(e.Handle), uintptr(windows.CurrentProcess()),
			uintptr(unsafe.Pointer(&dup)), 0, 0, 0)
		if st != 0 {
			return
		}
		// GetFileType 预过滤：只对磁盘文件句柄做路径解析。同步命名管道/设备
		// 句柄调用 GetFinalPathNameByHandleW 会等待对端而无限挂起（全表遍历
		// 必然撞到），非 DISK 类型直接跳过——锁定目标必是磁盘文件。
		if ft, _, _ := procGetFileTypeP.Call(uintptr(dup)); ft != fileTypeDisk {
			_ = windows.CloseHandle(dup)
			return
		}
		name := finalPathOfHandle(dup)
		_ = windows.CloseHandle(dup)
		if name == "" {
			return
		}
		if matchesTarget(name, target) {
			seen[int(pid)] = true
			out = append(out, Locker{
				PID:     int(pid),
				Name:    procNameOfPID(uint32(pid)),
				CmdLine: cmdlineOfPID(uint32(pid)),
			})
		}
	})
	for pid, h := range procHandles {
		_ = windows.CloseHandle(h)
		delete(procHandles, pid)
	}
	return out
}

// walkHandleTableCollect 遍历句柄表并对每条目执行回调（收集式）。
func walkHandleTableCollect(visit func(*sysHandleEntry)) {
	defer func() { _ = recover() }()
	var size uint32 = 1 << 22
	var buf []byte
	var st uintptr
	for attempt := 0; attempt < 3; attempt++ {
		buf = make([]byte, size)
		var ret uint32
		st, _, _ = procNtQuerySystemInformationP.Call(
			systemHandleInformation,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(size),
			uintptr(unsafe.Pointer(&ret)))
		if st == statusInfoLengthMismatch {
			size *= 2
			continue
		}
		break
	}
	if st != 0 {
		return
	}
	count := *(*uint32)(unsafe.Pointer(&buf[0]))
	entrySize := unsafe.Sizeof(sysHandleEntry{})
	// 头部为 8 字节（NumberOfHandles u32 + Reserved u32），条目从 +8 开始；
	// 初稿按 +4 切头导致条目边界错位 4 字节（PID/句柄全部错读）。
	if max := uint32((len(buf) - sysHandleInfoHeaderSize) / int(entrySize)); count > max {
		count = max
	}
	base := unsafe.Pointer(&buf[sysHandleInfoHeaderSize])
	for i := uint32(0); i < count; i++ {
		visit((*sysHandleEntry)(unsafe.Add(base, uintptr(i)*entrySize)))
	}
}

// matchesTarget 句柄路径与目标路径比对（NT 设备路径与 DOS 路径双形态 + \\?\ 前缀）。
func matchesTarget(handlePath, targetLower string) bool {
	candidates := []string{strings.ToLower(handlePath)}
	if d := dosPathOf(handlePath); d != handlePath {
		candidates = append(candidates, strings.ToLower(d))
	}
	trimmed := strings.TrimPrefix(strings.ToLower(handlePath), `\\?\`)
	if trimmed != strings.ToLower(handlePath) {
		candidates = append(candidates, trimmed)
	}
	for _, c := range candidates {
		if c == targetLower {
			return true
		}
	}
	return false
}

// finalPathOfHandle GetFinalPathNameByHandleW（带超时防挂兑底：经 GetFileType
// 磁盘过滤后挂起概率极低，但极端文件系统对象仍可能阻塞系统调用——
// 200ms 未返回即放弃该句柄；挂起的 goroutine 无法终止，量级极小可接受）。
func finalPathOfHandle(h windows.Handle) string {
	defer func() { _ = recover() }()
	ch := make(chan string, 1)
	go func() {
		defer func() { _ = recover() }()
		var buf [1024]uint16
		r, _, _ := procGetFinalPathNameByHandleP.Call(
			uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
		if r == 0 || int(r) > len(buf) {
			ch <- ""
			return
		}
		ch <- windows.UTF16ToString(buf[:r])
	}()
	select {
	case name := <-ch:
		return name
	case <-time.After(200 * time.Millisecond):
		return ""
	}
}

// dosPathOf NT 设备路径（\Device\HarddiskVolumeN\...）→ 盘符路径。
func dosPathOf(ntPath string) string {
	var vol [512]uint16
	for c := 'A'; c <= 'Z'; c++ {
		root := string(c) + ":"
		n, _, _ := procQueryDosDeviceP.Call(
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(root))),
			uintptr(unsafe.Pointer(&vol[0])), uintptr(len(vol)))
		if n == 0 {
			continue
		}
		// QueryDosDeviceW 返回多串（NUL 分隔），取第一段。
		device := windows.UTF16ToString(vol[:n-1])
		if strings.HasPrefix(ntPath, device) {
			return root + strings.TrimPrefix(ntPath, device)
		}
	}
	return ntPath
}

// probeWriteOpenWindows GENERIC_WRITE + 全共享打开探测（返回共享冲突/只读等错误）。
func probeWriteOpenWindows(path string) error {
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, _, callErr := procCreateFileW.Call(
		uintptr(unsafe.Pointer(p16)),
		genericWrite,
		fileShareRead|fileShareWrite|fileShareDelete,
		0, openExisting, 0, 0)
	if h == uintptr(windows.InvalidHandle) || h == 0 {
		return callErr
	}
	_ = windows.CloseHandle(windows.Handle(h))
	return nil
}

// procNameOfPID Toolhelp 快照按 PID 找进程名。
func procNameOfPID(pid uint32) string {
	snap, _, _ := procCreateToolhelp32SnapshotP.Call(th32csSnapProcess, 0)
	if snap == uintptr(windows.InvalidHandle) || snap == 0 {
		return fmt.Sprintf("pid-%d", pid)
	}
	defer windows.CloseHandle(windows.Handle(snap))
	var e processEntry32W
	e.Size = uint32(unsafe.Sizeof(e))
	r, _, _ := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&e)))
	for r != 0 {
		if e.ProcessID == pid {
			return windows.UTF16ToString(e.ExeFile[:])
		}
		r, _, _ = procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&e)))
	}
	return fmt.Sprintf("pid-%d", pid)
}
