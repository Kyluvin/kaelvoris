// FileContextTracker 简版单测：mtime stale 检测语义边界。
package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileTrackerStaleDetected(t *testing.T) {
	dir := t.TempDir()
	p := "cfg.txt"
	abs := filepath.Join(dir, p)
	os.WriteFile(abs, []byte("v1"), 0o644)

	ft := newFileContextTracker()
	ft.markRead(dir, p) // 模型读取

	// 外部修改（shell 命令/用户）→ mtime 变化。
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(abs, future, future)

	warn := ft.checkStale(dir, p)
	if warn == "" || !strings.Contains(warn, "stale content") || !strings.Contains(warn, p) {
		t.Fatalf("want stale warning, got %q", warn)
	}
}

func TestFileTrackerNoStaleWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := "cfg.txt"
	os.WriteFile(filepath.Join(dir, p), []byte("v1"), 0o644)

	ft := newFileContextTracker()
	ft.markRead(dir, p)
	if warn := ft.checkStale(dir, p); warn != "" {
		t.Fatalf("unexpected warning: %q", warn)
	}
}

func TestFileTrackerNeverReadNoWarning(t *testing.T) {
	dir := t.TempDir()
	p := "fresh.txt"
	os.WriteFile(filepath.Join(dir, p), []byte("v1"), 0o644)

	ft := newFileContextTracker()
	// 从未读过：不判定 stale（外部基线无法推断）。
	if warn := ft.checkStale(dir, p); warn != "" {
		t.Fatalf("unexpected warning: %q", warn)
	}
}

func TestFileTrackerMarkWrittenClearsStale(t *testing.T) {
	dir := t.TempDir()
	p := "cfg.txt"
	abs := filepath.Join(dir, p)
	os.WriteFile(abs, []byte("v1"), 0o644)

	ft := newFileContextTracker()
	ft.markRead(dir, p)

	// 外部修改后模型写入 → markWritten 刷新基线，下次写入不再误报。
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(abs, future, future)
	if warn := ft.checkStale(dir, p); warn == "" {
		t.Fatal("expected stale before write")
	}
	ft.markWritten(dir, p)
	if warn := ft.checkStale(dir, p); warn != "" {
		t.Fatalf("unexpected warning after own write: %q", warn)
	}
}

func TestFileTrackerSelfWriteThenWriteOK(t *testing.T) {
	dir := t.TempDir()
	p := "app.go"
	abs := filepath.Join(dir, p)
	os.WriteFile(abs, []byte("v1"), 0o644)

	ft := newFileContextTracker()
	ft.markRead(dir, p)
	ft.markWritten(dir, p) // 自己刚写
	if warn := ft.checkStale(dir, p); warn != "" {
		t.Fatalf("self write flagged stale: %q", warn)
	}
}

func TestFileTrackerPathEscapeSafe(t *testing.T) {
	dir := t.TempDir()
	ft := newFileContextTracker()
	// 越界路径：statWorkspace 拒绝 → 不记录、不告警。
	ft.markRead(dir, "../../outside.txt")
	if warn := ft.checkStale(dir, "../../outside.txt"); warn != "" {
		t.Fatalf("unexpected warning: %q", warn)
	}
}

func TestFileTrackerNilSafe(t *testing.T) {
	var ft *fileContextTracker
	if warn := ft.checkStale(t.TempDir(), "x.txt"); warn != "" {
		t.Fatalf("nil tracker warning: %q", warn)
	}
	ft.markRead(t.TempDir(), "x.txt")
	ft.markWritten(t.TempDir(), "x.txt")
}

func TestStaleNotice(t *testing.T) {
	if got := staleNotice("", "res"); got != "res" {
		t.Fatalf("got %q", got)
	}
	got := staleNotice("WARN", "res")
	if !strings.HasPrefix(got, "res\n\nWARN") {
		t.Fatalf("got %q", got)
	}
}
