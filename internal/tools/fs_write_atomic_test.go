package tools

// fs_write / fs_fs_apply_diff 原子落盘 + fs_search 纯 Go 路径上限对齐的回归测试
//（2026-11 工具层可靠性加固）。
//
// writeFileAtomic：正常写入 / 覆盖已存在文件 / rename 失败后无临时文件残留；
// walkSearch：per-file 命中上限（对齐 rg --max-count）、全局上限 + 截断提示、
// filesize 上限 2M（对齐 rg --max-filesize）。直调 walkSearch 绕开 rg 分流，
// 不依赖本机是否安装 ripgrep（与 TestSearchFilesTool_IgnoreDirs 同法）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// leftoverTmps 返回目录内残留的 writeFileAtomic 临时文件路径。
func leftoverTmps(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".kaelvoris-*.tmp"))
	if err != nil {
		t.Fatalf("glob tmp: %v", err)
	}
	return matches
}

// TestWriteFileAtomicBasic 正常写入与覆盖已存在文件：内容精确落盘、无临时残留。
func TestWriteFileAtomicBasic(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")

	// ① 新文件写入。
	if err := writeFileAtomic(target, []byte("hello atomic"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic new file: %v", err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "hello atomic" {
		t.Fatalf("new file content mismatch: %q err=%v", b, err)
	}
	if n := leftoverTmps(t, dir); len(n) != 0 {
		t.Errorf("tmp files leaked after success: %v", n)
	}

	// ② 覆盖已存在文件（同卷 Rename 原子替换，Windows 上 MoveFileEx REPLACE_EXISTING）。
	if err := writeFileAtomic(target, []byte("replaced"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic overwrite: %v", err)
	}
	b, err = os.ReadFile(target)
	if err != nil || string(b) != "replaced" {
		t.Fatalf("overwrite content mismatch: %q err=%v", b, err)
	}
	if n := leftoverTmps(t, dir); len(n) != 0 {
		t.Errorf("tmp files leaked after overwrite: %v", n)
	}

	// ③ fs_write 工具链路端到端：落盘走 writeFileAtomic 后无临时残留。
	ws := t.TempDir()
	tool := &WriteFileTool{Workspace: ws}
	if _, err := tool.Execute(context.Background(), `{"path":"a/b.txt","content":"x"}`); err != nil {
		t.Fatalf("fs_write: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "a", "b.txt")); err != nil || string(b) != "x" {
		t.Fatalf("fs_write content mismatch: %q err=%v", b, err)
	}
	if n := leftoverTmps(t, filepath.Join(ws, "a")); len(n) != 0 {
		t.Errorf("tmp files leaked after fs_write: %v", n)
	}
}

// TestWriteFileAtomicRenameFailCleansTmp rename 失败场景（目标为已存在目录，
// Windows 上文件无法替换目录，必然失败）：报错且临时文件被清理，不留残留。
// 崩溃窗口防护的核心保证：失败路径绝不遗留半成品临时文件。
func TestWriteFileAtomicRenameFailCleansTmp(t *testing.T) {
	dir := t.TempDir()
	// 目标路径是已存在目录 → os.Rename(file, dir) 必败（确定性失败注入）。
	dirTarget := filepath.Join(dir, "targetdir")
	if err := os.MkdirAll(dirTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(dirTarget, []byte("should fail"), 0o644); err == nil {
		t.Fatal("expected rename-to-directory to fail")
	}
	if n := leftoverTmps(t, dir); len(n) != 0 {
		t.Errorf("tmp files leaked after rename failure: %v", n)
	}
}

// TestWalkSearchPerFileAndGlobalCaps 纯 Go 路径上限回归：
// ① 单文件 30 次命中只输出 maxHitsPerFile 行（对齐 rg --max-count），无截断提示；
// ② 多文件累计超 maxSearchHits → 全局 200 行封顶 + "(truncated)" 提示（与 rg 路径文案一致）。
func TestWalkSearchPerFileAndGlobalCaps(t *testing.T) {
	// ① per-file 上限。
	dir := t.TempDir()
	var rep strings.Builder
	for i := 0; i < 30; i++ {
		rep.WriteString("needle line\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "repetitive.log"), []byte(rep.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := walkSearch(dir, dir, "needle", "", nil)
	if err != nil {
		t.Fatalf("walkSearch: %v", err)
	}
	if n := strings.Count(out, "needle line"); n != maxHitsPerFile {
		t.Errorf("per-file cap: expected %d hits, got %d: %q", maxHitsPerFile, n, out)
	}
	if strings.Contains(out, "truncated") {
		t.Errorf("per-file cap alone must not set global truncation marker: %q", out)
	}

	// ② 全局上限 + 截断提示：per-file 上限 10 先生效，每个文件只贡献 10 条，
	// 需 21 文件 × 10 = 210 > 200 才能触发全局截断。
	dir2 := t.TempDir()
	for i := 0; i < 21; i++ {
		name := filepath.Join(dir2, fmt.Sprintf("r%02d.log", i))
		if err := os.WriteFile(name, []byte(rep.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err = walkSearch(dir2, dir2, "needle", "", nil)
	if err != nil {
		t.Fatalf("walkSearch multi: %v", err)
	}
	if n := strings.Count(out, "needle line"); n != maxSearchHits {
		t.Errorf("global cap: expected %d hits, got %d", maxSearchHits, n)
	}
	if !strings.Contains(out, "(truncated)") {
		t.Errorf("global cap should append truncation hint, got: %q", out)
	}
}

// TestCapSearchOutput rg 路径全局截断提示（纯函数，不依赖 rg 安装）：
// 行数达到 maxSearchHits → 末尾补 "... (truncated)"（与 Go 路径文案一致）；
// 未达上限 → 原样返回。
func TestCapSearchOutput(t *testing.T) {
	// 恰为 maxSearchHits 行：无法确认 rg 是否还有更多命中，应标截断。
	lines := make([]string, maxSearchHits)
	for i := range lines {
		lines[i] = "file.go:1: hit"
	}
	s := strings.Join(lines, "\n") + "\n"
	got := capSearchOutput(s)
	if !strings.HasSuffix(got, "... (truncated)\n") {
		t.Errorf("%d lines should get truncation hint, got suffix: %q", maxSearchHits, got[max(0, len(got)-40):])
	}

	// 少于上限：原样返回。
	short := "file.go:1: hit\nfile.go:2: hit\n"
	if got := capSearchOutput(short); got != short {
		t.Errorf("short output must pass through unchanged, got: %q", got)
	}
}

// TestWalkSearchFileSizeLimit2M filesize 上限对齐 rg --max-filesize 2M：
// 刚好低于 2M 的文件应被检索；超过 2M 的跳过（不整读内存）。
// 注：不用 Seek 稀疏文件——稀疏洞在二进制探测（读头 8KB）中读出 NUL 字节，
// 会被 isBinary 误判为二进制文件跳过，与 filesize 语义混清；用真实字节填充。
func TestWalkSearchFileSizeLimit2M(t *testing.T) {
	dir := t.TempDir()

	// under.bin：总大小 = 7 + (2M-100) + 1 = 2M-92 字节 < maxSearchFileSize。
	under := filepath.Join(dir, "under.bin")
	underContent := "needle\n" + strings.Repeat("x", maxSearchFileSize-100) + "\n"
	if err := os.WriteFile(under, []byte(underContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// over.bin：总大小 = 7 + (2M-6) = 2M+1 字节 > maxSearchFileSize → 跳过。
	over := filepath.Join(dir, "over.bin")
	overContent := "needle\n" + strings.Repeat("x", maxSearchFileSize-6)
	if err := os.WriteFile(over, []byte(overContent), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := walkSearch(dir, dir, "needle", "", nil)
	if err != nil {
		t.Fatalf("walkSearch: %v", err)
	}
	if !strings.Contains(out, "under.bin") {
		t.Errorf("file under 2M should be searched, got: %q", out)
	}
	if strings.Contains(out, "over.bin") {
		t.Errorf("file over 2M should be skipped, got: %q", out)
	}
}
