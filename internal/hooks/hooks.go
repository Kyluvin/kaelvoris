// Package hooks 生命周期钩子（对齐 Claude Code Hooks 生命周期 + codex
// hook_runtime.rs 简版的用户可配置层）：
//
//	PreToolUse   —— 工具执行前：可阻断（exit≠0 且 stderr 非空）/ 改参（stdout JSON {"args":...}）
//	PostToolUse  —— 工具执行成功后：观察点（结果旁路，不修改结果）
//	Stop         —— 任务收尾（attempt_completion / 上限终止）：审计/通知
//
// 配置文件 ~/.kaelvoris/hooks.json（数组，全部启用；非法条目跳过不致命）：
//
//	[
//	  {"event":"PreToolUse","matcher":"^(shell|fs_write)$","command":"python guard.py","timeout_sec":10},
//	  {"event":"Stop","command":"notify-send \"task done\""}
//	]
//
// 钩子进程经 shell 执行（Windows cmd /C，Unix sh -c），工作目录 = 会话工作区；
// stdin 传 JSON 载荷（event/tool/args/result/workspace），环境注入 KAELVORIS_HOOK_EVENT。
// 执行失败不致命：返回安全缺省（allow=true / 忽略输出）。
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"kaelvoris/internal/sysproc"
)

// Event 钩子事件类型。
type Event string

const (
	EventPreToolUse  Event = "PreToolUse"
	EventPostToolUse Event = "PostToolUse"
	EventStop        Event = "Stop"
)

// HookConfig 单条钩子配置（hooks.json 数组条目）。
type HookConfig struct {
	Event      string `json:"event"`                 // PreToolUse | PostToolUse | Stop
	Matcher    string `json:"matcher,omitempty"`     // 工具名正则（Stop 事件忽略）；空 = 全部工具
	Command    string `json:"command"`               // 经 shell 执行的命令行
	TimeoutSec int    `json:"timeout_sec,omitempty"` // 超时秒数（默认 15，上限 120）
}

// Manager 钩子管理器：持有配置与工作区，按事件分发执行。
type Manager struct {
	Workspace string // 钩子进程工作目录（会话工作区）
	Timeout   int    // 全局默认超时秒数（0 = 15）

	hooks   []HookConfig
	reCache map[string]*regexp.Regexp
}

// Load 读取并解析 hooks.json；文件不存在/解析失败 = 空管理器（跳过不致命）。
func Load(path, workspace string) *Manager {
	m := &Manager{Workspace: workspace, reCache: map[string]*regexp.Regexp{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	// 剥 BOM（Windows PowerShell 写出的 UTF-8 文件默认带 BOM）。
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	var list []HookConfig
	if err := json.Unmarshal(b, &list); err != nil {
		return m
	}
	for _, h := range list {
		if strings.TrimSpace(h.Command) == "" {
			continue
		}
		switch Event(strings.TrimSpace(h.Event)) {
		case EventPreToolUse, EventPostToolUse, EventStop:
			m.hooks = append(m.hooks, h)
		}
	}
	return m
}

// Enabled 是否配置了任何钩子。
func (m *Manager) Enabled() bool { return m != nil && len(m.hooks) > 0 }

// match 工具名是否命中 matcher（空 = 全部；非法正则按字面量兜底）。
func (m *Manager) match(matcher, tool string) bool {
	if strings.TrimSpace(matcher) == "" {
		return true
	}
	re, ok := m.reCache[matcher]
	if !ok {
		re, _ = regexp.Compile(matcher) // 失败时 re 为 nil，缓存避免反复编译
		m.reCache[matcher] = re
	}
	if re == nil {
		return matcher == tool
	}
	return re.MatchString(tool)
}

// hookPayload 传给钩子进程的 stdin JSON 载荷。
type hookPayload struct {
	Event     string          `json:"event"`
	Tool      string          `json:"tool,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Result    string          `json:"result,omitempty"`
	Workspace string          `json:"workspace,omitempty"`
}

// hookOutput 钩子 stdout 可选 JSON 输出（PreToolUse 改参/显式阻断）。
type hookOutput struct {
	Args   string `json:"args,omitempty"`
	Block  bool   `json:"block,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// PreToolUse 执行全部 PreToolUse 钩子，返回 (生效参数, blockMsg)。
// 语义（对齐 Claude Code PreToolUse + codex hook_runtime 简版）：
//   - 任一钩子 exit 非零 → 阻断（exit 2 = stderr 原文回填模型；其他码摘要）；
//   - stdout JSON {"block":true,"reason":...} → 同样阻断；
//   - stdout JSON {"args":...}（合法 JSON 字符串）→ 参数改写，后续钩子收到改写后参数；
//   - 全部通过 → (原始/改写参数, "")。
func (m *Manager) PreToolUse(ctx context.Context, tool, args string) (string, string) {
	if !m.Enabled() {
		return args, ""
	}
	cur := args
	for _, h := range m.hooks {
		if Event(strings.TrimSpace(h.Event)) != EventPreToolUse || !m.match(h.Matcher, tool) {
			continue
		}
		code, stdout, stderr := m.run(ctx, h, hookPayload{
			Event: string(EventPreToolUse), Tool: tool,
			Args: json.RawMessage(orEmptyJSON(cur)), Workspace: m.Workspace,
		})
		if code != 0 {
			reason := strings.TrimSpace(stderr)
			if reason == "" {
				reason = fmt.Sprintf("hook exit code %d", code)
			}
			return cur, reason
		}
		if out := strings.TrimSpace(stdout); out != "" {
			var o hookOutput
			if json.Unmarshal([]byte(out), &o) == nil {
				if o.Block {
					reason := o.Reason
					if reason == "" {
						reason = "blocked by hook"
					}
					return cur, reason
				}
				if o.Args != "" && json.Valid([]byte(o.Args)) {
					cur = o.Args
				}
			}
		}
	}
	return cur, ""
}

// PostToolUse 执行全部 PostToolUse 钩子（结果旁路观察点；失败忽略）。
func (m *Manager) PostToolUse(ctx context.Context, tool, args, result string) {
	if !m.Enabled() {
		return
	}
	res := result
	if len(res) > 16*1024 { // 结果截断后进 stdin（防管道撑爆）
		res = res[:16*1024]
	}
	for _, h := range m.hooks {
		if Event(strings.TrimSpace(h.Event)) != EventPostToolUse || !m.match(h.Matcher, tool) {
			continue
		}
		_, _, _ = m.run(ctx, h, hookPayload{
			Event: string(EventPostToolUse), Tool: tool,
			Args: json.RawMessage(orEmptyJSON(args)), Result: res, Workspace: m.Workspace,
		})
	}
}

// Stop 执行全部 Stop 钩子（任务收尾审计/通知；失败忽略）。
func (m *Manager) Stop(ctx context.Context) {
	if !m.Enabled() {
		return
	}
	for _, h := range m.hooks {
		if Event(strings.TrimSpace(h.Event)) != EventStop {
			continue
		}
		_, _, _ = m.run(ctx, h, hookPayload{Event: string(EventStop), Workspace: m.Workspace})
	}
}

// run 执行单条钩子：shell 包装 + stdin 载荷 + 超时控制。
// 返回 (exitCode, stdout, stderr)；启动失败 exitCode=-1（按放行处理，不致命）。
func (m *Manager) run(ctx context.Context, h HookConfig, p hookPayload) (int, string, string) {
	timeout := h.TimeoutSec
	if timeout <= 0 {
		timeout = m.Timeout
	}
	if timeout <= 0 {
		timeout = 15
	}
	if timeout > 120 {
		timeout = 120
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	stdinBytes, _ := json.Marshal(p)
	var cmd *exec.Cmd
	if goosWindows() {
		cmd = exec.CommandContext(cctx, "cmd", "/C", h.Command)
	} else {
		cmd = exec.CommandContext(cctx, "sh", "-c", h.Command)
	}
	// 仅隐藏窗口，不走 sysproc.Hide：Hide 附带 preserveCmdLine 会改变 cmd /C
	// 引号解析语义，钩子 stdout JSON 的引号被剥除后 json.Unmarshal 失败
	// （2026-10-29 TestPreToolUseMutateArgs 回归实锤）。钩子命令由用户配置、
	// 原样执行语义，只需不弹黑框。
	sysproc.HideWindowOnly(cmd)
	cmd.Dir = m.Workspace
	cmd.Stdin = bytes.NewReader(stdinBytes)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	cmd.Env = append(os.Environ(), "KAELVORIS_HOOK_EVENT="+string(p.Event))

	code := 0
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return code, outBuf.String(), errBuf.String()
}

func orEmptyJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}
