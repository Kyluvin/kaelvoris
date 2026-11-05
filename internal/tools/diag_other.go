//go:build !windows

// Package tools —— 非 Windows 平台诊断降级实现（优雅降级，Windows 为主战场）：
//
//   - listProcsGeneric：优先读 /proc（Linux）；无 /proc（macOS 等）回退
//     `ps -axo pid=,ppid=,comm=` 一次子进程调用。命令行仅 /proc 路径可补齐。
//   - collectPortsGeneric：无零依赖的跨平台端口归属 API（/proc/net/tcp → inode
//     → /proc/<pid>/fd 反查链路过重），降级为空集——端口过滤在非 Windows 上
//     返回"no matching processes"，不影响 process_list 其余能力。
//   - findFileLockers：句柄表为 Windows 专属机制（见 diag_windows.go），
//     非 Windows 返回空，由 FileLockerTool 降级为写打开探测（probeWriteOpen）。
package tools

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// listProcs 非 Windows 进程枚举入口：/proc 优先，ps 命令兜底。
func listProcs(_ string) ([]ProcInfo, error) {
	if procs, ok := listProcsProcFS(); ok {
		return procs, nil
	}
	return listProcsPS()
}

// listProcsProcFS Linux /proc 枚举（PID/PPID/名称/命令行）。
func listProcsProcFS() ([]ProcInfo, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	var procs []ProcInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		pr := ProcInfo{PID: pid, Name: "unknown"}
		if b, err := os.ReadFile("/proc/" + e.Name() + "/comm"); err == nil {
			pr.Name = strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile("/proc/" + e.Name() + "/stat"); err == nil {
			// stat 格式：pid (comm) state ppid ...——comm 可能含空格/括号，取最后一个 ')' 之后解析。
			if i := strings.LastIndexByte(string(b), ')'); i >= 0 && i+2 < len(b) {
				fields := strings.Fields(string(b)[i+2:])
				if len(fields) > 1 {
					if ppid, err := strconv.Atoi(fields[1]); err == nil {
						pr.PPID = ppid
					}
				}
			}
		}
		if b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline"); err == nil && len(b) > 0 {
			// cmdline 以 NUL 分隔；替换为空格得到可读命令行。
			pr.CmdLine = strings.ReplaceAll(strings.TrimSuffix(string(b), "\x00"), "\x00", " ")
		}
		procs = append(procs, pr)
	}
	return procs, procs != nil
}

// listProcsPS ps 命令兜底（macOS/BSD 等）：单次调用，零转义风险。
func listProcsPS() ([]ProcInfo, error) {
	out, err := runPS()
	if err != nil {
		return nil, fmt.Errorf("process enumeration unavailable on %s: %w", osType(), err)
	}
	var procs []ProcInfo
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		// comm 可能含空格：剩余字段全归入名称。
		procs = append(procs, ProcInfo{PID: pid, PPID: ppid, Name: strings.Join(fields[2:], " ")})
	}
	return procs, nil
}

func osType() string { return runtime.GOOS }

// runPS 执行 ps -axo pid=,ppid=,comm=（隐藏窗口由 exec 层保障）。
func runPS() (string, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// collectPorts 非 Windows 端口归属：零依赖实现过重，降级为空集。
// （Linux 可用 ss -lptnpi，但需额外子进程与输出解析；Windows 为主战场，
// 保留接口一致性即可。）
func collectPorts(_ int, _ map[int]bool, _ map[int][]string) {}

// findFileLockers 句柄表为 Windows 专属机制，非 Windows 返回空集，
// 由 FileLockerTool 降级为写打开探测（probeWriteOpen）。
func findFileLockers(_ string) []Locker { return nil }
