package desktop

// zed_fake_test.go：临时目录放假 zed.cmd（把参数写入文件）+ 临时改 PATH，
// 验证 openInZedAt 以 `zed <file>:<line>` 形态 spawn（Windows 经 cmd /C）。
// 仅 Windows；其他平台跳过。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOpenInZedAtSpawnsWithPathLine(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd /C 包装验证仅 Windows")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "args.txt")
	// 假 zed.cmd：参数（path:line）写入 args.txt
	fake := "@echo off\r\necho %* > \"" + log + "\"\r\n"
	if err := os.WriteFile(filepath.Join(dir, "zed.cmd"), []byte(fake), 0o644); err != nil {
		t.Fatal(err)
	}
	old := os.Getenv("PATH")
	_ = os.Setenv("PATH", dir+";"+old)
	defer os.Setenv("PATH", old)

	target := filepath.Join(dir, "a.go")
	if err := os.WriteFile(target, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := openInZedAt(dir, target, 42); err != nil {
		t.Fatalf("openInZedAt: %v", err)
	}
	// 等待异步进程写文件（Start 不等待）。注意 cmd 重定向会先建空文件
	// 再写入，故读到内容不含目标时继续轮询，超时才判失败（防满载竞态）。
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(log); err == nil {
			last = string(b)
			if strings.Contains(last, "a.go:42") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("假 zed 未在期限内写入正确 args, last=%q", last)
}
