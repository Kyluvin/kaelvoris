package autocommit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo 建临时 git 仓库（配置本地身份，不依赖全局 git config）。
func initRepo(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init")
	run("config", "user.name", "test")
	run("config", "user.email", "test@test.local")
	run("config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(ws, "base.txt"), "base\n")
	run("add", ".")
	run("commit", "-m", "init")
	return ws
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitLog(t *testing.T, ws string, n int) []string {
	t.Helper()
	cmd := exec.Command("git", "log", "--format=%s", "-n", itoa(n))
	cmd.Dir = ws
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func itoa(n int) string {
	return string(rune('0' + n))
}

// TestCommitMessage 纯函数：首行摘要截断 + 文件清单 + 归因尾注。
func TestCommitMessage(t *testing.T) {
	long := strings.Repeat("啊", 100) // 100 rune 中文任务
	msg := CommitMessage(long, []string{"a.go", "b/c.go"})
	lines := strings.Split(msg, "\n")
	first := []rune(lines[0])
	if len(first) > 61 { // 60 + 省略号
		t.Fatalf("首行超 60 rune: %d", len(first))
	}
	if !strings.Contains(msg, "- a.go") || !strings.Contains(msg, "- b/c.go") {
		t.Fatalf("缺文件清单: %q", msg)
	}
	if !strings.Contains(msg, "🤖 Generated with Kaelvoris") {
		t.Fatalf("缺归因尾注: %q", msg)
	}
	// 换行压平：多行任务摘要进首行不含换行。
	msg2 := CommitMessage("line1\nline2\n\nline3", []string{"x.go"})
	if strings.Contains(strings.Split(msg2, "\n")[0], "\n") {
		t.Fatal("首行应压平换行")
	}
	if !strings.HasPrefix(msg2, "line1 line2 line3") {
		t.Fatalf("换行未压平为空格: %q", msg2)
	}
}

// TestRunCommitsTaskFiles 端到端：AI 变更文件被提交，hash 返回。
func TestRunCommitsTaskFiles(t *testing.T) {
	ws := initRepo(t)
	writeFile(t, filepath.Join(ws, "src", "new.go"), "package src\n")
	hash, err := Run(ws, []string{filepath.ToSlash(filepath.Join("src", "new.go"))}, "新增 src 包")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(hash) < 7 {
		t.Fatalf("hash 异常: %q", hash)
	}
	logs := gitLog(t, ws, 1)
	if !strings.HasPrefix(logs[0], "新增 src 包") {
		t.Fatalf("commit 首行不符: %q", logs[0])
	}
}

// TestRunDirtyProtection 端到端：用户既有未提交变更先打 wip commit，与 AI 提交分离。
func TestRunDirtyProtection(t *testing.T) {
	ws := initRepo(t)
	// 用户 dirty：改 base.txt（不属于本次 AI 变更清单）。
	writeFile(t, filepath.Join(ws, "base.txt"), "user edit\n")
	// AI 变更：新文件。
	writeFile(t, filepath.Join(ws, "ai.go"), "package ai\n")
	if _, err := Run(ws, []string{"ai.go"}, "AI 改动"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	logs := gitLog(t, ws, 2)
	if logs[0] != "AI 改动" {
		t.Fatalf("最新提交应为 AI 改动: %q", logs[0])
	}
	if logs[1] != "wip: user changes before kaelvoris task" {
		t.Fatalf("次新提交应为 wip 保护: %q", logs[1])
	}
}

// TestRunNotARepo 非 git 目录报错（调用方静默）。
func TestRunNotARepo(t *testing.T) {
	ws := t.TempDir()
	if _, err := Run(ws, []string{"a.txt"}, "task"); err == nil {
		t.Fatal("非 git 仓库应报错")
	}
}

// TestRunNoFiles 空清单报错。
func TestRunNoFiles(t *testing.T) {
	ws := initRepo(t)
	if _, err := Run(ws, nil, "task"); err == nil {
		t.Fatal("空清单应报错")
	}
}

// TestIsGitRepo 仓库/非仓库判定。
func TestIsGitRepo(t *testing.T) {
	ws := initRepo(t)
	if !IsGitRepo(ws) {
		t.Fatal("git 仓库应判定为 true")
	}
	if IsGitRepo(t.TempDir()) {
		t.Fatal("普通目录应判定为 false")
	}
}
