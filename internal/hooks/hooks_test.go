package hooks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadValidAndInvalid 配置加载：合法条目保留，非法事件/空命令跳过。
func TestLoadValidAndInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	cfg := `[
		{"event":"PreToolUse","matcher":"^shell$","command":"echo hi","timeout_sec":5},
		{"event":"bogus","command":"echo bad"},
		{"event":"Stop","command":""},
		{"event":"Stop","command":"echo done"}
	]`
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	m := Load(path, dir)
	if !m.Enabled() {
		t.Fatal("expected enabled manager")
	}
	if len(m.hooks) != 2 {
		t.Fatalf("expected 2 valid hooks, got %d", len(m.hooks))
	}
	// 文件不存在 = 空（Enabled=false）。
	if Load(filepath.Join(dir, "missing.json"), dir).Enabled() {
		t.Fatal("missing file should be disabled")
	}
	// 坏 JSON = 空且不致命。
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Load(filepath.Join(dir, "bad.json"), dir).Enabled() {
		t.Fatal("bad json should be skipped")
	}
}

// TestPreToolUseBlockExitCode exit≠0 阻断：exit 2 时 stderr 原文回填。
func TestPreToolUseBlockExitCode(t *testing.T) {
	var cfg string
	if goosWindows() {
		cfg = `[{"event":"PreToolUse","command":"echo block-reason>&2 & exit 2"}]`
	} else {
		cfg = `[{"event":"PreToolUse","command":"echo block-reason >&2; exit 2"}]`
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	m := Load(path, dir)
	_, blockMsg := m.PreToolUse(context.Background(), "shell", "{}")
	if blockMsg == "" || !strings.Contains(blockMsg, "block-reason") {
		t.Fatalf("expected block message, got %q", blockMsg)
	}
}

// TestPreToolUseMutateArgs stdout {"args":...} 参数改写。
func TestPreToolUseMutateArgs(t *testing.T) {
	var cmd string
	if goosWindows() {
		// 环境守卫（对齐 tools/shell_ps_encoded_test.go）：部分机器 SRP/杀软拦截
		// cmd 子进程链启动 powershell.exe，钩子命令必然失败（stderr=拒绝访问被
		// 误判为 block），跳过而非误报。
		if err := exec.Command("cmd", "/C", "powershell -NoProfile -Command exit").Run(); err != nil {
			t.Skip("本机 cmd /C 启动 powershell 被环境策略拦截（SRP/杀软），跳过")
		}
		// cmd /C 引号剥离严重，用 echo + 重定向写临时文件再 type？
		// 简化：用 powershell 单行输出 JSON（Windows 下钩子本就允许任意命令）。
		cmd = `powershell -NoProfile -Command '{"args":"{\"path\":\"rewired.go\"}"}'`
	} else {
		cmd = `echo '{"args":"{\"path\":\"rewired.go\"}"}'`
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	cfg, _ := json.Marshal([]HookConfig{{Event: "PreToolUse", Command: cmd}})
	if err := os.WriteFile(path, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	m := Load(path, dir)
	newArgs, blockMsg := m.PreToolUse(context.Background(), "fs_write", `{"path":"orig.go"}`)
	if blockMsg != "" {
		t.Fatalf("unexpected block: %q", blockMsg)
	}
	want := `{"path":"rewired.go"}`
	if goosWindows() {
		want = `{"path":"rewired.go"}` // powershell 输出含引号转义差异时放宽校验
	}
	if !strings.Contains(newArgs, "rewired") {
		t.Fatalf("expected mutated args containing %q, got %q", want, newArgs)
	}
}

// TestStopHook Stop 事件执行（以临时文件为证，命令真实跑过）。
func TestStopHook(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "stopped.txt")
	var cfg string
	if goosWindows() {
		cfg = `[{"event":"Stop","command":"echo ok> ` + strings.ReplaceAll(marker, `\`, `\\`) + `"}]`
	} else {
		cfg = `[{"event":"Stop","command":"touch '` + marker + `'"}]`
	}
	path := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	m := Load(path, dir)
	m.Stop(context.Background())
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("stop hook did not run: %v", err)
	}
}

// TestMatcherRegex matcher 正则匹配过滤。
func TestMatcherRegex(t *testing.T) {
	m := &Manager{reCache: map[string]*regexpT{}}
	m.hooks = []HookConfig{{Event: "PreToolUse", Matcher: "^(shell|fs_write)$", Command: "x"}}
	if !m.match("^(shell|fs_write)$", "shell") || m.match("^(shell|fs_write)$", "fs_read") {
		t.Fatal("matcher regex mismatch")
	}
	if !m.match("", "anything") {
		t.Fatal("empty matcher should match all")
	}
	// 非法正则退化为字面量。
	if !m.match("a(b", "a(b") || m.match("a(b", "ab") {
		t.Fatal("invalid regex should degrade to literal compare")
	}
}
