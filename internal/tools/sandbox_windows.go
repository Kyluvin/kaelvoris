//go:build windows

// Package tools 沙箱 Windows 实现：kernel32 Job Object。
//
// 全部经 golang.org/x/sys/windows 绑定（CreateJobObject /
// SetInformationJobObject / AssignProcessToJobObject / TerminateJobObject，
// v0.46.0 齐备）；仅 JOBOBJECT_CPU_RATE_CONTROL_INFORMATION 与
// JobObjectInfoClass 的 CPU rate 常量 x/sys 未导出，按本包惯例自定义
//（对齐 diag_windows.go NewLazySystemDLL 思路，但此处直接用 x/sys 的
// zsyscall 绑定，无需再绑 proc）。
package tools

import (
	"fmt"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows API 常量（x/sys v0.46.0 未导出部分的补充定义）。
const (
	// jobObjectCpuRateControlInformation JobObjectInfoClass = 15（x/sys 已有
	// windows.JobObjectCpuRateControlInformation 常量，直接用）。
	// JOB_OBJECT_CPU_RATE_CONTROL_ENABLE / HARD_CAP（winbase.h）。
	jobObjectCPURateEnable  = 0x00000001
	jobObjectCPURateHardCap = 0x00000002
	// JobObjectCpuRateControlInformation 在 x/sys 中为
	// windows.JobObjectCpuRateControlInformation（= 15），不重复定义。
)

// jobCPURateControl JOBOBJECT_CPU_RATE_CONTROL_INFORMATION（winbase.h）：
// ControlFlags + CpuRate（百分比 × 100，即 100% = 10000）。
// x/sys 未导出该结构体，本地定义（内存布局两 uint32）。
type jobCPURateControl struct {
	ControlFlags uint32
	CpuRate      uint32
}

// jobSandbox Windows Job Object 执行器（每命令/会话/后台任务一个实例）。
type jobSandbox struct {
	mu   sync.Mutex
	job  windows.Handle
	kill bool // Kill 已调用（Terminate 幂等保护）
}

// newPlatformSandbox Windows job 实现：创建 job object 并按配置施加 limits。
// CreateJobObject 失败仅在此返回 error，调用方降级裸跑。
func newPlatformSandbox(cfg SandboxConfig) (SandboxExecutor, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil || job == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	sb := &jobSandbox{job: job}

	// limits：全 0 = 不限（不调用 SetInformation，job 默认无资源约束，
	// 仅提供整树终止能力）。
	if cfg.MemoryLimitMB <= 0 && cfg.CPUPercent <= 0 && cfg.MaxProcesses <= 0 {
		return sb, nil
	}

	var ext windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limitFlags := uint32(windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY | windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS)
	if cfg.MemoryLimitMB > 0 {
		ext.ProcessMemoryLimit = uintptr(cfg.MemoryLimitMB) << 20
	}
	if cfg.MaxProcesses > 0 {
		ext.BasicLimitInformation.ActiveProcessLimit = uint32(cfg.MaxProcesses)
	}
	// 单一 limit 时清掉另一个 flag，避免把 0 值 limit 一并设进去
	//（0 值 ActiveProcessLimit 携带 ACTIVE_PROCESS flag 行为未定义）。
	if cfg.MemoryLimitMB <= 0 {
		limitFlags &^= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
	}
	if cfg.MaxProcesses <= 0 {
		limitFlags &^= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
	}
	ext.BasicLimitInformation.LimitFlags = limitFlags

	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject(limits): %w", err)
	}

	// CPU 硬配额：10-100 才生效（<10 视为误配，跳过；100 = 全核上限也可不设，
	// 但保持显式硬上限语义与任务书一致）。
	if cfg.CPUPercent >= 10 && cfg.CPUPercent <= 100 {
		cpu := jobCPURateControl{
			ControlFlags: jobObjectCPURateEnable | jobObjectCPURateHardCap,
			// CpuRate 以百分比 × 100 计（100% = 10000）。
			CpuRate: uint32(cfg.CPUPercent) * 100,
		}
		if _, err := windows.SetInformationJobObject(job, windows.JobObjectCpuRateControlInformation,
			uintptr(unsafe.Pointer(&cpu)), uint32(unsafe.Sizeof(cpu))); err != nil {
			windows.CloseHandle(job)
			return nil, fmt.Errorf("SetInformationJobObject(cpu): %w", err)
		}
	}
	return sb, nil
}

// Attach 把进程加入 job（Start 成功后调用）。
//
// 失败场景：进程已在系统兼容性 job 中（ERROR_ACCESS_DENIED 等）——
// 返回 error，调用方 warn 一次并继续裸跑。进程已退出时同样报错，无害。
// 句柄经 OpenProcess(SET_QUOTA|TERMINATE) 获取（AssignProcessToJobObject
// 的文档要求权限）；不用 os.Process.WithHandle（需 go1.26，模块声明 1.25）。
func (s *jobSandbox) Attach(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("sandbox attach: process not started")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == 0 {
		return fmt.Errorf("sandbox attach: job closed")
	}
	const procSetQuota = 0x0100
	const procTerminate = 0x0001
	h, err := windows.OpenProcess(procSetQuota|procTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(pid=%d): %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(h)
	return windows.AssignProcessToJobObject(s.job, h)
}

// Kill 整树终止（TerminateJobObject，幂等）：杀 job 内全部进程——
// cmd.exe 与其派生的全部孙进程，修复既有 Process.Kill() 只杀直接子进程
// 的进程泄漏。仅在既有 kill 点（超时/取消/会话关闭）调用。
func (s *jobSandbox) Kill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == 0 || s.kill {
		return nil
	}
	s.kill = true
	return windows.TerminateJobObject(s.job, 1)
}

// Close 关闭 job 句柄（不杀进程，幂等）。
// 不设 KILL_ON_JOB_CLOSE：保留「start /B 拉起常驻进程后命令返回、
// 进程继续存活」的既有语义。
func (s *jobSandbox) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == 0 {
		return nil
	}
	job := s.job
	s.job = 0
	return windows.CloseHandle(job)
}
