// Package tools unified_exec 持久 shell 会话（codex 对齐补全 ⑤）。
//
// 对齐 codex-rs unified_exec / shell_snapshot 的核心思想：多次 shell 调用
// 复用同一个持久 shell 进程——环境变量导出、cd、venv 激活等跨调用状态
// 自然延续（普通 ShellTool 每次 exec 冷启动，cd/export 不保留）。
//
// 实现方式（零 PTY 依赖，Windows 兼容）：
//
//	Windows：常驻 cmd.exe /K 子进程，stdin 逐条投递命令；
//	Unix：常驻 bash --noprofile --norc 子进程。
//	每条命令用唯一哨兵标记回显边界，收集到哨兵即认为该命令结束，
//	哨兵行携带退出码。单命令超时后回退：进程未响应时整个会话标记为
//	失效（下次调用自动重启新会话），防卡死。
//
// Windows 编码修复（2026-09-27 复盘）：cmd.exe 管道代码页默认跟随系统
// GBK，中文命令（如 git commit -m "中文"）经 stdin 投递后按 GBK 解码，
// UTF-8 字节流被误读成乱码后落盘。会话启动即投递 chcp 65001 切换到
// UTF-8，使 stdin 字节（Go 端恒为 UTF-8）与解码端一致。
package tools

	import (
		"bufio"
		"context"
		"encoding/json"
		"fmt"
		"io"
		"log/slog"
		"os/exec"
		"runtime"
		"runtime/debug"
		"strings"
		"sync"
		"time"
		"unicode/utf8"

		"golang.org/x/text/encoding/simplifiedchinese"

		"kaelvoris/internal/sysproc"
	)

// sessionDefaultTimeout 单命令默认超时（秒）。
const sessionDefaultTimeout = 120

// shellSession 一个持久 shell 会话（常驻子进程 + stdin 投递 + 哨兵同步）。
type shellSession struct {
	mu        sync.Mutex
	workspace string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	alived    bool
	idSeq     int
	// 读泵竞争修复：单一常驻读泵独占 stdout（bufio.Reader 不可并发读）。
	// 旧实现每条命令临时起一个读泵 goroutine，命令结束后泵仍阻塞在
	// ReadString 上不被回收，下一条命令再起一个泵 → 两个泵并发读同一个
	// bufio.Reader = bufio 内部 slice 越界 panic（app.log 大量"读泵 panic"
	// 刷屏的根因），且输出错乱导致哨兵解析失败。runCommand 只消费 lines。
	done    chan struct{} // close() 时关闭：解除读泵阻塞 + 广播会话失效
	lines   chan string   // 读泵产出的原始行（含行尾 \r\n；关闭 = 流结束）
	readErr chan error    // 读泵遇到的 IO 错误（缓冲 1，幂等投递）
		// 命令级串行化：同一会话的命令必须排队执行——并发写 stdin 会让两条
		// 命令的输出/哨兵交错错位。
		cmdMu sync.Mutex
		// sandbox 会话沙箱执行器（nil = 裸跑）：close 时整树终止+关句柄。
		sandbox SandboxExecutor
	}

	// PersistentShellManager 任务级持久会话管理器（每个任务一个，会话随任务
	// 结束销毁；导出供 app 层 buildTools 构造与任务结束时 CloseAll）。
	type PersistentShellManager struct {
		mu        sync.Mutex
		sessions  map[string]*shellSession // sessionID → session
		workspace string
		// Sandbox 沙箱工厂（nil = 裸跑）：每个会话 New() 一个执行器，会话
		// shell 进程入 job；超时会话重置 / CloseAll 时整树终止。
		Sandbox SandboxFactory
	}

	// NewPersistentShellManager 创建持久会话管理器。
	func NewPersistentShellManager(workspace string) *PersistentShellManager {
		return &PersistentShellManager{sessions: map[string]*shellSession{}, workspace: workspace}
	}

// exec 在指定会话中执行命令（session 为空 = "default"）；会话不存在或已死
// 则自动重建（对齐 unified_exec 的会话生命周期语义）。
func (m *PersistentShellManager) exec(ctx context.Context, sessionID, command string, timeoutSec int) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		sessionID = "default"
	}
	if timeoutSec <= 0 {
		timeoutSec = sessionDefaultTimeout
	}
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	if !ok || !s.alive() {
		// 重建会话（旧进程已死或首次调用）。
			if s != nil {
				s.close()
			}
			ns, err := newShellSession(m.workspace, m.Sandbox)
			if err != nil {
				delete(m.sessions, sessionID)
				m.mu.Unlock()
				return "", fmt.Errorf("failed to start shell session: %w", err)
			}
		m.sessions[sessionID] = ns
		s = ns
	}
	m.mu.Unlock()
	return s.runCommand(ctx, command, timeoutSec)
}

// CloseAll 关闭全部会话（任务结束时调用）。
func (m *PersistentShellManager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.close()
		delete(m.sessions, id)
	}
}

	// newShellSession 启动常驻 shell 子进程（sandbox 工厂可空 = 裸跑）。
	func newShellSession(workspace string, factory SandboxFactory) (*shellSession, error) {
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd.exe", "/K")
		} else {
			cmd = exec.Command("bash", "--noprofile", "--norc", "-i")
		}
	if workspace != "" {
		cmd.Dir = workspace
	}
	sysproc.Hide(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout // 合并 stderr
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		// 沙箱接线：会话 shell 进程入 job（每会话一个实例）；Attach 失败
		// 仅记日志降级裸跑——绝不因沙箱挂掉而让会话起不来。
		var sb SandboxExecutor
		if factory != nil {
			sb = factory.New()
			if err := sb.Attach(cmd); err != nil {
				slog.Warn("sandbox attach failed for shell session, fallback to unsandboxed", "err", err)
				sb.Close()
				sb = nil
			}
		}
		s := &shellSession{
			cmd:       cmd,
			stdin:     stdin,
			stdout:    bufio.NewReaderSize(stdout, 64*1024),
			workspace: workspace,
			alived:    true,
			done:      make(chan struct{}),
			lines:     make(chan string, 256),
			readErr:   make(chan error, 1),
			sandbox:   sb,
		}
	// 常驻读泵：会话生命周期内唯一的 stdout 读者（竞争修复核心）。
	go s.readPump()
	// Windows 编码修复：首条命令前置 chcp 65001，把控制台切到 UTF-8。
	// 用同步方式确保在用户命令前生效；失败仅记日志（回退 GBK 现状）。
	if runtime.GOOS == "windows" {
		_, _ = io.WriteString(s.stdin, "chcp 65001>nul\r\n")
	}
	return s, nil
}

// readPump 常驻读泵：逐行读取子进程输出投递 lines；会话关闭（done）或流
// EOF 时退出。panic 防护保留：任何异常退出都必须让等待方感知（readErr
// 报错或 lines 关闭），否则 runCommand 的 select 永久阻塞（命令永不返回）。
func (s *shellSession) readPump() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("shell 读泵 panic（已拦截）", "panic", r, "stack", string(debug.Stack()))
			select {
			case s.readErr <- fmt.Errorf("reader panic: %v", r):
			default:
			}
		}
		close(s.lines)
	}()
	for {
		line, err := s.stdout.ReadString('\n')
		if line != "" {
			// GBK 兑底（任务 3 加固）：chcp 65001 未生效/部分程序仍按系统
			// 代码页输出时，逐行检测非法 UTF-8 并用 GBK 解码重试。
			if !utf8.ValidString(line) {
				if decoded, derr := simplifiedchinese.GBK.NewDecoder().Bytes([]byte(line)); derr == nil && utf8.Valid(decoded) {
					line = string(decoded)
				}
			}
			select {
			case s.lines <- line:
			case <-s.done:
				return
			}
		}
		if err != nil {
			select {
			case s.readErr <- err:
			default:
			}
			return
		}
	}
}

// alive 会话进程是否存活。
func (s *shellSession) alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alived
}

// close 终止会话进程（幂等）。
func (s *shellSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.alived {
		return
	}
	s.alived = false
		close(s.done) // 解除读泵阻塞并广播会话失效（仅首次关闭执行）
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		// 沙箱优先：Kill 整树终止（cmd.exe + 全部孙进程）；裸跑时退回
		// Process.Kill（只杀直接子进程，孙进程泄漏为既有已知限制）。
		killed := false
		if s.sandbox != nil {
			if err := s.sandbox.Kill(); err == nil {
				killed = true
			}
		}
		if !killed && s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		if s.sandbox != nil {
			_ = s.sandbox.Close()
			s.sandbox = nil
		}
	}

// runCommand 向会话投递一条命令并等待哨兵回显；返回合并输出与退出码。
// 哨兵协议：先 echo 开始标记 → 命令 → echo 结束标记（携带退出码）。
// 读取侧逐行消费常驻读泵的 lines 通道直到看到结束哨兵（或超时/流关闭）。
// 命令级串行化（cmdMu）：同一会话的命令排队执行——并发写 stdin 会让两条
// 命令的输出与哨兵交错错位（竞争修复的另一面）。
func (s *shellSession) runCommand(ctx context.Context, command string, timeoutSec int) (string, error) {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	s.mu.Lock()
	if !s.alived {
		s.mu.Unlock()
		return "", fmt.Errorf("shell session is not alive")
	}
	s.idSeq++
	sentinel := fmt.Sprintf("KAELVORIS_EOF_%d", s.idSeq)
	// PowerShell $ 保真（2026-10-06 复盘补齐）：cmd /K 常驻会话 stdin
	// 投递直通 cmd 解析层，powershell -Command "...$变量..." 会被提前展开/
	// 吞失。纯 PowerShell 调用先转 -EncodedCommand 形态再投递（Base64
	// 只含安全字符，cmd 原样放行，$ 与 Agent 提交时逐字节一致）。
	if runtime.GOOS == "windows" {
		command = psSafeCommand(command)
	}
	// 投递：echo 开始哨兵 + 命令 + echo 结束哨兵（退出码）。
	var script string
	if runtime.GOOS == "windows" {
		script = fmt.Sprintf("@echo %s_START\r\n%s\r\n@echo %s_END_%%ERRORLEVEL%%\r\n", sentinel, command, sentinel)
	} else {
		script = fmt.Sprintf("echo %s_START\n%s\necho %s_END_$?\n", sentinel, command, sentinel)
	}
	if _, err := io.WriteString(s.stdin, script); err != nil {
		s.alived = false
		s.mu.Unlock()
		return "", fmt.Errorf("shell session broken (stdin write): %w", err)
	}
	s.mu.Unlock()

	// 定时读取（单命令超时；ctx 取消优先）。
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	var out strings.Builder
	endMark := sentinel + "_END_"
		for {
			select {
			case <-cctx.Done():
				s.close() // 进程未响应：整个会话标记失效，下次调用自动重建
				return out.String(), fmt.Errorf(
					"shell session command timed out after %ds — session was RESET: cwd/env state is LOST (re-run cd/export as needed). For long-running commands use run_in_background=true and poll with shell_output", timeoutSec)
		case err := <-s.readErr:
			s.close()
			return out.String(), fmt.Errorf("shell session closed: %w", err)
		case line, ok := <-s.lines:
			if !ok {
				s.close() // 读泵退出（进程死亡/会话关闭）→ 下次调用自动重建
				return out.String(), fmt.Errorf("shell session closed (reader exited)")
			}
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.Contains(trimmed, sentinel+"_START") {
				continue // 开始哨兵：丢弃
			}
					if idx := strings.Index(trimmed, endMark); idx >= 0 {
						code := strings.TrimSpace(trimmed[idx+len(endMark):])
						res := ensureUTF8(out.String()) // GBK 兑底（任务 3 加固）
						if len(res) > shellOutLimit {
							res = truncateMiddle(res, shellOutLimit)
						}
				if res == "" {
					res = "(no output)"
				}
				return fmt.Sprintf("exit code %s\n\n%s", code, res), nil
			}
			out.WriteString(line)
		}
	}
}

// ---------- unified_exec 工具 ----------

// UnifiedExecTool 持久 shell 会话工具：多次调用共享 cwd/env 状态；
// run_in_background=true 时走 BackgroundManager 后台启动（防长驻进程阻塞）。
type UnifiedExecTool struct {
	Mgr *PersistentShellManager
	Bg  *BackgroundManager
}

func (t *UnifiedExecTool) Name() string { return "unified_exec" }

func (t *UnifiedExecTool) Description() string {
	return "Execute a command inside a persistent shell session. State such as the current " +
		"working directory (cd) and exported environment variables persists across calls " +
		"within the same session id. Use a distinct session id to isolate state; omit it " +
		"to use the default session. Output is truncated to 10000 characters."
}

func (t *UnifiedExecTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"command":           strProp("The shell command to execute inside the session"),
		"session":           strProp("Session id for persistent state (default: \"default\")"),
		"timeout_sec":       numProp("Per-command timeout in seconds (default 120)"),
		"run_in_background": map[string]interface{}{"type": "boolean", "description": "Start the command in background and return a bg_id immediately instead of waiting. Use for long-running processes (dev servers, uvicorn, watch tasks); read output later with shell_output."},
	}, []string{"command"})
}

func (t *UnifiedExecTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Command         string `json:"command"`
		Session         string `json:"session"`
		TimeoutSec      int    `json:"timeout_sec"`
		RunInBackground bool   `json:"run_in_background"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(p.Command) == "" {
		return "", fmt.Errorf("command is required")
	}
	// 危险命令检测与 shell 工具共享同一清单（P2.7-2 DCG 黑名单）。
	// 2026-10-10：改用 denyListHit 统一判定（PS -Format 参数形态豁免，
	// 修复 Get-Date -Format 被 "format " 黑名单项误拦）。
	if denyListHit(p.Command) {
		return "", fmt.Errorf("command blocked by destructive command guard: %q", p.Command)
	}
	// 后台模式：立即返回 bg_id，输出落盘；用 shell_output 轮询（防长驻进程
	// 阻塞前台会话直至超时——uvicorn/dev server 类命令必须走此模式）。
	if p.RunInBackground {
		if t.Bg == nil {
			return "", fmt.Errorf("background execution not available in this context")
		}
		id, _, err := t.Bg.Start(p.Command)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("started in background: %s\nread output with shell_output (bg_id=%s, offset=0).",
			id, id), nil
	}
	return t.Mgr.exec(ctx, p.Session, p.Command, p.TimeoutSec)
}
