// mentions 解析单元测试：@/file @/folder @url @git-changes 与边界语义
// （email 不误判、尾部标点留存、转义空格、路径穿越拒绝、去重、截断）。
package mentions

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFileMention(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Parse("请看 @/hello.txt 并总结", dir)
	if res.Text != "请看 'hello.txt' 并总结" {
		t.Fatalf("text = %q", res.Text)
	}
	if len(res.Blocks) != 1 {
		t.Fatalf("blocks = %d", len(res.Blocks))
	}
	b := res.Blocks[0]
	if b.Type != "file" || b.Path != "hello.txt" {
		t.Fatalf("block = %+v", b)
	}
	if !strings.Contains(b.Content, "[read_file for 'hello.txt']") ||
		!strings.Contains(b.Content, "line one") {
		t.Fatalf("content = %q", b.Content)
	}
	if !strings.Contains(b.Content, "1|\tline one") {
		t.Fatalf("missing line numbers: %q", b.Content)
	}
}

func TestParseDedupSamePath(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	res := Parse("@/a.txt 和 @/a.txt 再看一遍 @/a.txt", dir)
	if len(res.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(res.Blocks))
	}
}

func TestParseFolderMention(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "x.go"), []byte("package pkg\n"), 0o644)
	res := Parse("浏览 @/pkg 目录", dir)
	if len(res.Blocks) != 1 || res.Blocks[0].Type != "folder" {
		t.Fatalf("blocks = %+v", res.Blocks)
	}
	if !strings.Contains(res.Blocks[0].Content, "pkg/x.go") {
		t.Fatalf("content = %q", res.Blocks[0].Content)
	}
}

func TestParseURLMention(t *testing.T) {
	res := Parse("参考 @https://example.com/docs 说明", t.TempDir())
	if res.Text != "参考 'https://example.com/docs' 说明" {
		t.Fatalf("text = %q", res.Text)
	}
	if len(res.Blocks) != 0 {
		t.Fatalf("blocks = %d", len(res.Blocks))
	}
}

func TestParseNoMention(t *testing.T) {
	in := "普通任务，没有 mention"
	if got := Parse(in, t.TempDir()).Text; got != in {
		t.Fatalf("text = %q", got)
	}
}

func TestParseEmailNotMention(t *testing.T) {
	// @ 前是字母（email 场景）→ 非 mention，原样保留。
	in := "联系 someone@example.com 确认"
	if got := Parse(in, t.TempDir()).Text; got != in {
		t.Fatalf("text = %q", got)
	}
}

func TestParseMissingFileErrorBlock(t *testing.T) {
	res := Parse("看 @/nope.txt", t.TempDir())
	if len(res.Blocks) != 1 {
		t.Fatalf("blocks = %d", len(res.Blocks))
	}
	if !strings.Contains(res.Blocks[0].Content, "Error:") {
		t.Fatalf("content = %q", res.Blocks[0].Content)
	}
}

func TestParsePathEscapeRejected(t *testing.T) {
	res := Parse("看 @/../../etc/passwd", t.TempDir())
	if len(res.Blocks) != 1 {
		t.Fatalf("blocks = %d", len(res.Blocks))
	}
	if !strings.Contains(res.Blocks[0].Content, "escapes workspace") {
		t.Fatalf("content = %q", res.Blocks[0].Content)
	}
}

func TestParseTrailingPunctuationStays(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	res := Parse("先看 @/a.txt, 再看 @/b.txt.", dir)
	if res.Text != "先看 'a.txt', 再看 'b.txt'." {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestParseEscapedSpacePath(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "my file.txt"), []byte("spaced"), 0o644)
	res := Parse(`读取 @/my\ file.txt 内容`, dir)
	if res.Text != "读取 'my file.txt' 内容" {
		t.Fatalf("text = %q", res.Text)
	}
	if len(res.Blocks) != 1 || res.Blocks[0].Path != "my file.txt" {
		t.Fatalf("blocks = %+v", res.Blocks)
	}
}

func TestParseFileTruncation(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 0, 2100*2)
	for i := 0; i < 2100; i++ {
		data = append(data, "l\n"...)
	}
	os.WriteFile(filepath.Join(dir, "big.txt"), data, 0o644)
	res := Parse("@/big.txt", dir)
	c := res.Blocks[0].Content
	if !strings.Contains(c, "Showing lines 1-2000 of 2100 total lines") ||
		!strings.Contains(c, "read_file tool with offset=2001") {
		t.Fatalf("truncation hint missing:\n%s", c)
	}
}

func TestParseBinaryFileSkipped(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bin.dat"), append([]byte("ok"), make([]byte, 32)...), 0o644)
	os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{'a', 0, 'b'}, 0o644)
	res := Parse("@/bin.dat", dir)
	if !strings.Contains(res.Blocks[0].Content, "(binary file, content omitted)") {
		t.Fatalf("content = %q", res.Blocks[0].Content)
	}
}

func TestParseGitChanges(t *testing.T) {
	dir := t.TempDir()
	if err := exec.Command("git", "init", "-q", dir).Run(); err != nil {
		t.Skip("git unavailable:", err)
	}
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644)
	res := Parse("检查 @git-changes", dir)
	if !strings.Contains(res.Text, "Workspace Changes (see below)") {
		t.Fatalf("text = %q", res.Text)
	}
	found := false
	for _, b := range res.Blocks {
		if b.Type == "git_changes" && strings.Contains(b.Content, "new.txt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("git block missing: %+v", res.Blocks)
	}
}

func TestParseGitChangesNonRepoKept(t *testing.T) {
	// 非 git 仓库：mention 保留原文，不注入假引用。
	in := "检查 @git-changes"
	if got := Parse(in, t.TempDir()).Text; got != in {
		t.Fatalf("text = %q", got)
	}
}

func TestExpandAppendsBlocks(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644)
	got := Expand("总结 @/a.txt", dir)
	want := "总结 'a.txt'\n\n[read_file for 'a.txt']\n     1|\thello"
	if got != want {
		t.Fatalf("Expand = %q, want %q", got, want)
	}
	if got := Expand("无 mention", dir); got != "无 mention" {
		t.Fatalf("no-mention expand = %q", got)
	}
}
