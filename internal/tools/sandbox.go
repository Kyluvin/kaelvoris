// Package tools 沙箱（Windows Job Object 进程树围栏）。
//
// 移植对齐 codex-rs exec 的 job object 沙箱思路：把每条命令（或每个持久
// shell 会话 / 后台任务）放进一个独立 Windows Job Object——
//   - 超时/取消/任务结束时 TerminateJobObject 整树终止（修复既有
//     Process.Kill() 只杀 cmd.exe、孙进程存活泄漏的问题）；
//   - 可选资源上限（内存 / CPU 配额 / 进程数），默认全 0 = 不限。
//
// 边界（如实）：
//   - 只管"怎么跑"不管"跑什么"——危险命令拦截/审批链（danger.go/denylist/
//     引擎审批）完全不变；
//   - 不隔离文件与网络访问（job object 无此能力，任务书已明确）；
//   - Start → Assign 之间存在微秒级逃逸窗口（进程已创建但尚未入 job），
//     已知 MVP 限制；彻底关闭需 Windows 8+ 的 CREATE_SUSPENDED +
//     Assign + ResumeThread 三步法，留待后续（见 ponytail 注）。
//
// 生命周期约定：每条命令 / 每个会话 / 每个后台任务各自 New() 一个 job 实例
// （勿全局单例），Start 后 Attach，用完 Close。不设 KILL_ON_JOB_CLOSE——
// 保留「start /B 拉起常驻 dev server 后命令返回、进程继续存活」的既有语义；
// Kill() 只在超时/取消/会话关闭等既有 kill 点调用。
package tools

import (
	"os/exec"
	"sync"
)

// SandboxConfig 沙箱配置（desktop.json 的 Config.Sandbox / CLI 环境变量注入）。
//
// Mode："job" = 启用 Job Object 围栏（默认）；"off" = 关闭（裸跑）。
// limits：全 0 = 不限（默认只保证整树终止，不施加资源约束）。
type SandboxConfig struct {
	Mode          string `json:"mode"`
	MemoryLimitMB int    `json:"memoryLimitMB"`
	CPUPercent    int    `json:"cpuPercent"`
	MaxProcesses  int    `json:"maxProcesses"`
}

// SandboxModeJob / SandboxModeOff 沙箱模式常量（配置与前端共用）。
const (
	SandboxModeJob = "job"
	SandboxModeOff = "off"
)

// SandboxFactory 执行器工厂：每个执行点调用 New() 取一个专属执行器，
// Attach 后使用，用完 Close（幂等）。
type SandboxFactory interface {
	New() SandboxExecutor
}

// SandboxExecutor 单个 job 实例的执行器。
//
// Attach 在 cmd.Start() 成功后调用，把进程加入 job；失败返回 error——
// 调用方 say/warn 一次后继续裸跑（优雅降级，绝不因沙箱挂掉而让命令失败）。
// Kill 整树终止（超时/取消/会话关闭等既有 kill 点调用）；Close 关句柄
// （不杀进程，幂等，Attach 失败时也安全）。
type SandboxExecutor interface {
	Attach(cmd *exec.Cmd) error
	Kill() error
	Close() error
}

// sandboxFactory 全平台共享的工厂壳：持有配置，New() 分发到平台实现。
type sandboxFactory struct {
	cfg SandboxConfig
}

// NewSandboxFactory 构造沙箱工厂。
//
// Mode=="off" 或非 Windows 平台 → noop 工厂（New 返回 noop 执行器，
// 全部操作零副作用）；Mode=="job" 且 Windows → Job Object 实现。
// 未知 Mode 按 job 处理（与配置兜底一致：默认开）。
func NewSandboxFactory(cfg SandboxConfig) SandboxFactory {
	return &sandboxFactory{cfg: cfg}
}

// noopSandbox 无沙箱执行器（Mode=off / 非 Windows）：全部零副作用。
// 单例复用即可——没有任何状态，Close/Kill 幂等无操作。
type noopSandbox struct{}

func (noopSandbox) Attach(*exec.Cmd) error { return nil }
func (noopSandbox) Kill() error            { return nil }
func (noopSandbox) Close() error           { return nil }

// noopSandboxOnce noop 单例（只读无状态，锁仅为初始化竞态）。
var (
	noopOnce      sync.Once
	noopSandboxSt noopSandbox
)

func noopSandboxInstance() noopSandbox {
	noopOnce.Do(func() { noopSandboxSt = noopSandbox{} })
	return noopSandboxSt
}

// New 按 Mode 分发平台实现（非 Windows / off → noop）。
func (f *sandboxFactory) New() SandboxExecutor {
	if f.cfg.Mode == SandboxModeOff {
		return noopSandboxInstance()
	}
	// job 模式：平台实现创建失败（CreateJobObject/SetInformation 报错）时
	// 降级裸跑（noop），调用方零感知——绝不因沙箱挂掉而让命令失败。
	if sb, err := newPlatformSandbox(f.cfg); err == nil && sb != nil {
		return sb
	}
	return noopSandboxInstance()
}

// jobSandboxWarn 单次告警去重：Attach 失败时调用方 warn 一次并降级裸跑，
// 用这里的辅助避免每次命令都刷告警（首次 true，之后 false）。
type jobSandboxWarn struct {
	mu    sync.Mutex
	shown bool
}

// Warn 返回是否应当发出告警（进程生命周期内只报一次）。
func (w *jobSandboxWarn) Warn() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.shown {
		return false
	}
	w.shown = true
	return true
}
