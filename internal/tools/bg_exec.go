// Package tools 后台命令执行：unified_exec 的 run_in_background 模式。
//
// 解决「启动长驻服务（uvicorn / dev server / watch 进程）导致 Agent 卡住」：
//   - 前台模式（默认）：行为不变，等命令退出或超时；
//   - 后台模式（run_in_background=true）：立即返回 bg_id，进程输出实时落盘，
//     Agent 随后用 shell_output 工具按 bg_id 轮询增量输出与退出码。
//
// 与 codex 对齐：codex-rs 的 exec 命令即采用 start + poll 后台语义。
package tools

	import (
		"context"
		"encoding/json"
		"fmt"
		"log/slog"
		"os"
		"os/exec"
		"path/filepath"
		"runtime"
		"strings"
		"sync"
		"time"

		"kaelvoris/internal/sysproc"
	)

	// bgProc 一个后台进程的生命周期记录。
	type bgProc struct {
		id      string
		cmd     *exec.Cmd
		logPath string
		// mu 保护 done / exitCode / exited。
		mu       sync.Mutex
		done     chan struct{}
		exitCode int
		exited   bool
		// sandbox 后台任务沙箱执行器（nil = 裸跑）：kill/CloseAll 时整树终止。
		sandbox SandboxExecutor
	}

	// BackgroundManager 任务级后台进程管理器（每任务一个，任务结束 CloseAll）。
	type BackgroundManager struct {
		mu        sync.Mutex
		procs     map[string]*bgProc
		seq       int
		workspace string
		logDir    string
		// Sandbox 沙箱工厂（nil = 裸跑）：每个后台任务 New() 一个执行器，
		// 进程入 job；CloseAll/单进程 kill 时整树终止。
		Sandbox SandboxFactory
	}

	// NewBackgroundManager 创建后台进程管理器；日志写入 <workspace>/.kaelvoris-bg/。
	func NewBackgroundManager(workspace string) *BackgroundManager {
		return &BackgroundManager{
			procs:     map[string]*bgProc{},
			workspace: workspace,
			logDir:    filepath.Join(workspace, ".kaelvoris-bg"),
		}
	}

// Start 后台启动命令：立即返回 bg_id；输出重定向到日志文件。
func (m *BackgroundManager) Start(command string) (string, *bgProc, error) {
	if strings.TrimSpace(command) == "" {
		return "", nil, fmt.Errorf("command is required")
	}
	if err := os.MkdirAll(m.logDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create bg log dir: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("bg_%d_%d", time.Now().Unix(), m.seq)
	logPath := filepath.Join(m.logDir, id+".log")

	logFile, err := os.Create(logPath)
	if err != nil {
		return "", nil, fmt.Errorf("create bg log file: %w", err)
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// PowerShell $ 保真（2026-10-06）：cmd /C 会先把内联脚本串里的
		// $变量 展开/吞失（shell 工具已用 argv 化修复，bg 后台路径此前漏网）。
		// 纯 PowerShell 调用在投递前转成 -EncodedCommand 形态（Base64 只含
		// 安全字符），cmd 解析层原样放行，PowerShell 端还原 $ 变量。
		cmd = exec.Command("cmd", "/C", psSafeCommand(command))
	} else {
		cmd = exec.Command("bash", "-c", command)
	}
	if m.workspace != "" {
		cmd.Dir = m.workspace
	}
	sysproc.Hide(cmd)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

		p := &bgProc{id: id, logPath: logPath, done: make(chan struct{})}
		if err := cmd.Start(); err != nil {
			logFile.Close()
			return "", nil, fmt.Errorf("start bg command: %w", err)
		}
		// 沙箱接线：Start 后入 job（每后台任务一个实例）；Attach 失败仅记
		// 日志降级裸跑。job 实例由 bgProc 持有，kill/CloseAll 时 Kill+Close。
		if m.Sandbox != nil {
			sb := m.Sandbox.New()
			if err := sb.Attach(cmd); err != nil {
				slog.Warn("sandbox attach failed for bg task, fallback to unsandboxed", "err", err)
				sb.Close()
				sb = nil
			}
			p.sandbox = sb
		}
		p.cmd = cmd
		m.procs[id] = p

	// 等待器：进程退出后记录退出码并关闭 done（日志文件随之不再写入）。
	go func() {
		code := 0
		if err := cmd.Wait(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		logFile.Close()
		p.mu.Lock()
		p.exitCode = code
		p.exited = true
		p.mu.Unlock()
		close(p.done)
	}()
	return id, p, nil
}

// Get 按 bg_id 取后台进程。
func (m *BackgroundManager) Get(id string) (*bgProc, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[id]
	return p, ok
}

// ReadOutput 增量读取后台进程输出：offset 为字节偏移（从 ReadOutput 返回的
// nextOffset 传入即取增量）。返回新输出、当前文件长度（作为下次 offset）、
// 是否已退出与退出码。
func (m *BackgroundManager) ReadOutput(id string, offset int64) (string, int64, bool, int, error) {
	p, ok := m.Get(id)
	if !ok {
		return "", 0, false, 0, fmt.Errorf("unknown background id %q", id)
	}
	return p.read(offset)
}

// WaitOutput 带等待的增量读取：进程运行中且 offset 处无新输出时，阻塞轮询
// 至多 waitMs 毫秒，期间一旦出现新输出或进程退出立即返回（shell_output 的
// wait_ms 参数后端，根治空轮询连续同参调用被防重复检测拦截的问题）。
// waitMs<=0 等价 ReadOutput。轮询间隔 300ms，退出后立即返回。
func (m *BackgroundManager) WaitOutput(id string, offset int64, waitMs int64) (string, int64, bool, int, error) {
	p, ok := m.Get(id)
	if !ok {
		return "", 0, false, 0, fmt.Errorf("unknown background id %q", id)
	}
	if waitMs <= 0 {
		return p.read(offset)
	}
	deadline := time.Now().Add(time.Duration(waitMs) * time.Millisecond)
	const tick = 300 * time.Millisecond
	for {
		out, next, finished, code, err := p.read(offset)
		if err != nil {
			return out, next, finished, code, err
		}
		// 有新输出 / 已退出 / 超时 → 返回。
		if out != "" || finished || !time.Now().Before(deadline) {
			return out, next, finished, code, err
		}
		d := time.Until(deadline)
		if d > tick {
			d = tick
		}
		select {
		case <-p.done: // 进程退出：最后读一次确保不丢单尾输出
			return p.read(offset)
		case <-time.After(d):
		}
	}
}

// read 实现增量读取（见 ReadOutput）。
func (p *bgProc) read(offset int64) (string, int64, bool, int, error) {
	if offset < 0 {
		offset = 0
	}
	data, err := os.ReadFile(p.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", offset, p.finished(), p.code(), nil
		}
		return "", offset, p.finished(), p.code(), err
	}
	total := int64(len(data))
	if offset > total {
		offset = total
	}
		chunk := data[offset:]
		// 单次读取上限，防超长日志撑爆上下文（与引擎 clampToolResult(40k) 对齐）。
		const maxChunk = shellOutLimit
		truncated := false
		if int64(len(chunk)) > maxChunk {
			chunk = chunk[:maxChunk]
			truncated = true
		}
		out := ensureUTF8(string(chunk)) // GBK 兑底（任务 3 加固）
		if truncated {
			out += "\n... (truncated at 40000 bytes; raise offset to read more)"
		}
	return out, total, p.finished(), p.code(), nil
}

// finished 进程是否已退出。
func (p *bgProc) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// code 退出码（未退出返回 0）。
func (p *bgProc) code() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode
}

	// CloseAll 终止全部后台进程（任务结束时调用；对齐会话生命周期）。
	// 沙箱优先：Kill 整树终止（含孙进程），失败退回 Process.Kill 兜底；
	// 随后 Close 释放 job 句柄。
	func (m *BackgroundManager) CloseAll() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, p := range m.procs {
			killed := false
			if p.sandbox != nil {
				if err := p.sandbox.Kill(); err == nil {
					killed = true
				}
				_ = p.sandbox.Close()
				p.sandbox = nil
			}
			if !killed && p.cmd != nil && p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
		}
		m.procs = map[string]*bgProc{}
	}

// ---------- shell_output 工具 ----------

// ShellOutputTool 读取后台命令的增量输出与状态（配合 unified_exec 的
// run_in_background 使用）。Long-running 服务启动类命令必须后台化，否则
// 前台模式会阻塞到超时。
type ShellOutputTool struct {
	Mgr *BackgroundManager
}

func (t *ShellOutputTool) Name() string { return "shell_output" }

func (t *ShellOutputTool) Description() string {
	return "Read incremental output of a background command started with unified_exec " +
		"(run_in_background=true). Pass the bg_id returned earlier and the offset from " +
		"the previous call (0 for first read) to fetch new output, running state and " +
		"exit code. Poll it to follow logs of long-running processes. " +
		"Use wait_ms (default 0, max 60000) to block until NEW output arrives or the " +
		"process exits — this avoids empty-poll loops and replaces `sleep`-style waiting."
}

func (t *ShellOutputTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"bg_id":   strProp("Background id returned by unified_exec (run_in_background=true)"),
		"offset":  numProp("Byte offset from previous shell_output response (0 for first read)"),
		"wait_ms": numProp("Optional: block up to this many ms waiting for new output or exit (default 0 = return immediately; max 60000). Use instead of repeated empty polls or shell sleep commands."),
	}, []string{"bg_id"})
}

func (t *ShellOutputTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		BgID   string `json:"bg_id"`
		Offset int64  `json:"offset"`
		WaitMs int64  `json:"wait_ms"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(p.BgID) == "" {
		return "", fmt.Errorf("bg_id is required")
	}
	// wait_ms 等待语义（2026-10-01 会话复盘：shell_output 空轮询连续同参调用
	// 9 次被「Repeated identical tool call blocked」拦截，另有 5 次 Start-Sleep
	// 空等阻塞会话）：给 Manager 加带超时的等待读取——进程运行中且无新输出时
	// 阻塞至多 wait_ms 毫秒，等新输出写入或进程退出后立即返回。上限 60s
	// 防止单次调用占用过久；解析失败/非正数按 0 处理（行为不变）。
	waitMs := p.WaitMs
	if waitMs < 0 {
		waitMs = 0
	}
	if waitMs > 60000 {
		waitMs = 60000
	}
	out, next, finished, code, err := t.Mgr.WaitOutput(p.BgID, p.Offset, waitMs)
	if err != nil {
		return "", err
	}
	status := "RUNNING"
	if finished {
		status = fmt.Sprintf("EXITED (exit code %d)", code)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\nnext_offset: %d\n\n", status, next)
	if out == "" {
		b.WriteString("(no new output)")
	} else {
		b.WriteString(out)
	}
	return b.String(), nil
}
