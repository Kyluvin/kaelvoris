package desktop

// open_in_explorer_test.go：openInZedAt 行号归一 + OpenFileAtLine 路径边界验证
// + resolveOpenableFile 白名单（工作区 / ~/.kaelvoris media·images / 越权拒绝）。
// 场景：zed 未安装时走可读错误分支（不 panic）；路径越权被拒绝。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenInZedAtLineClamp(t *testing.T) {
	err := openInZedAt(".", "app.go", 0)
	if err == nil {
		t.Skip("zed 已安装（命令已 Start），无输出可断言")
	}
	if !strings.Contains(err.Error(), "未找到 zed 命令") {
		t.Fatalf("want 可读 zed 错误, got: %v", err)
	}
}

func TestOpenFileAtLinePathBoundary(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "a.go")
	if err := os.WriteFile(inside, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{Workspace: dir}}

	// 工作区内文件：应有可读错误（关联程序缺失等）且不再报「不在工作区」。
	err := app.OpenFileAtLine(inside, 62)
	if err != nil && strings.Contains(err.Error(), "不在当前工作区内") {
		t.Fatalf("工作区内文件被误拒: %v", err)
	}
	// 工作区外：必须拒绝。
	outside := filepath.Join(filepath.Dir(dir), "outside.go")
	if err := os.WriteFile(outside, []byte("x\n"), 0o644); err == nil {
		if e := app.OpenFileAtLine(outside, 1); e == nil || !strings.Contains(e.Error(), "不在当前工作区内") {
			t.Fatalf("越权文件未被拒: %v", e)
		}
	}
}

func TestResolveOpenableFileMediaWhitelist(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "报告.pptx")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{Workspace: dir}}

	// 工作区内 pptx：应通过校验（返回绝对路径，不报越权）。
	got, err := app.resolveOpenableFile(inside)
	if err != nil {
		t.Fatalf("工作区内 pptx 被误拒: %v", err)
	}
	if got != inside {
		t.Fatalf("want %q, got %q", inside, got)
	}
	// 相对路径按工作区拼接等价。
	if got2, err := app.resolveOpenableFile("报告.pptx"); err != nil || got2 != inside {
		t.Fatalf("相对路径解析: %v, %q", err, got2)
	}

	// 媒体回放目录（~/.kaelvoris/media）：白名单内，允许。
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("无 home 目录")
	}
	mediaDir := filepath.Join(home, ".kaelvoris", "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mediaFile := filepath.Join(mediaDir, "t_open_def_test.mp4")
	if err := os.WriteFile(mediaFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := app.resolveOpenableFile(mediaFile); err != nil {
		t.Fatalf("media 白名单文件被误拒: %v", err)
	}
	_ = os.Remove(mediaFile)

	// 越权：工作区外且非媒体目录 → 拒绝。
	outside := filepath.Join(filepath.Dir(dir), "evil.pptx")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err == nil {
		if _, e := app.resolveOpenableFile(outside); e == nil || !strings.Contains(e.Error(), "不在当前工作区内") {
			t.Fatalf("越权文件未被拒: %v", e)
		}
	}
	// 不存在的文件：可读错误。
	if _, e := app.resolveOpenableFile(filepath.Join(dir, "nope.png")); e == nil || !strings.Contains(e.Error(), "文件不存在") {
		t.Fatalf("不存在文件未报可读错误: %v", e)
	}
}
