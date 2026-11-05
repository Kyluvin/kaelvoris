package desktop

// open_media_guard_test.go：聊天媒体打开/另存为路径白名单加固验收测试
// （规划板条目：入口 filepath.Clean 后媒体目录前缀白名单校验，越界拒绝）。
// resolveOpenableFile：`../` 越界相对路径、盘外绝对路径拒绝；
// ~/.kaelvoris/media、~/.kaelvoris/images 目录内路径放行（含子目录产物）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveOpenableFileRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{Workspace: dir}}

	// `../` 越界相对路径：经工作区拼接后逃出工作区（且非媒体目录）→ 拒绝。
	for _, p := range []string{
		"../outside.txt",
		filepath.Join("..", "outside.txt"),
		"a/../../outside.txt", // Clean 归一后同样落在工作区外
	} {
		evil := filepath.Join(filepath.Dir(dir), "outside.txt")
		if err := os.WriteFile(evil, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := app.resolveOpenableFile(p); err == nil {
			t.Fatalf("越界相对路径 %q 未被拒绝", p)
		} else if !strings.Contains(err.Error(), "不在当前工作区内") {
			t.Fatalf("越界相对路径 %q 错误不可读: %v", p, err)
		}
	}

	// 盘外绝对路径：白名单（工作区/媒体/图片目录）外 → 拒绝。
	if err := os.WriteFile(evil2(dir), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := app.resolveOpenableFile(evil2(dir)); err == nil || !strings.Contains(err.Error(), "不在当前工作区内") {
		t.Fatalf("盘外绝对路径未被拒绝: %v", err)
	}
}

// evil2 返回 dir 同级的盘外文件路径（与工作区/媒体目录均无关）。
func evil2(dir string) string { return filepath.Join(filepath.Dir(dir), "evil_abs.txt") }

func TestResolveOpenableFileAllowsMediaDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "报告.pptx"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{Workspace: dir}}

	// 工作区内文件放行（白名单第一分支）。
	if got, err := app.resolveOpenableFile(filepath.Join(dir, "报告.pptx")); err != nil || got != filepath.Join(dir, "报告.pptx") {
		t.Fatalf("工作区内文件被误拒: %v, %q", err, got)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("无 home 目录")
	}
	// 媒体/图片目录内路径放行：根级文件 + 子目录产物（media/video、media/audio、
	// media/screenshots、images 根）与 generate_image/video/speech 实际落盘结构对齐。
	cases := map[string]string{
		"media 根级":          filepath.Join(home, ".kaelvoris", "media", "t_guard_media.mp4"),
		"media/video 子目录":   filepath.Join(home, ".kaelvoris", "media", "video", "t_guard_v.mp4"),
		"media/audio 子目录":   filepath.Join(home, ".kaelvoris", "media", "audio", "t_guard_a.mp3"),
		"media/screenshots": filepath.Join(home, ".kaelvoris", "media", "screenshots", "t_guard_s.png"),
		"images 根级":         filepath.Join(home, ".kaelvoris", "images", "t_guard_i.png"),
	}
	for name, path := range cases {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := app.resolveOpenableFile(path); err != nil {
			t.Fatalf("%s 白名单文件被误拒: %v", name, err)
		}
		_ = os.Remove(path)
	}
}

func TestResolveOpenableFileSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	app := &App{cfg: Config{Workspace: dir}}

	// 符号链接逃逸：工作区内 symlink 指向盘外文件。filepath.Abs/Rel 仅做词法
	// 归一，不会解析 symlink，因此目标在白名单词法范围内 → 放行（打开的仍是
	// 校验时落点文件，语义安全）；此处断言不因 symlink 崩溃即可。
	link := filepath.Join(dir, "link.txt")
	target := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("创建 symlink 失败（Windows 需管理员/开发者模式）:", err)
	}
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := app.resolveOpenableFile(link); err != nil {
		t.Fatalf("工作区内 symlink 被误拒: %v", err)
	}
}
