package tools

// 诊断三件套单测：http_request（httptest 本地回环）/ file_locker（自锁探测）。
// process_list / 端口归属依赖系统状态，仅做冒烟（不校验具体条目）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v map[string]interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return string(b)
}

func TestHTTPRequestTool_Get(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tl := &HTTPRequestTool{}
	out, err := tl.Execute(context.Background(), mustJSON(t, map[string]interface{}{"url": srv.URL + "/health"}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"status: 200", "latency:", `"ok":true`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// 404 路径：状态行可见，不算 error（探测语义）。
	out, err = tl.Execute(context.Background(), mustJSON(t, map[string]interface{}{"url": srv.URL + "/missing"}))
	if err != nil {
		t.Fatalf("execute 404: %v", err)
	}
	if !strings.Contains(out, "status: 404") {
		t.Errorf("expected 404 status line, got:\n%s", out)
	}

	// 非法 URL：返回错误。
	if _, err := tl.Execute(context.Background(), mustJSON(t, map[string]interface{}{"url": "ftp://x"})); err == nil {
		t.Error("expected error for non-http url")
	}
}

func TestHTTPRequestTool_HeadNoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD, got %s", r.Method)
		}
		w.Header().Set("Content-Length", "12345")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tl := &HTTPRequestTool{}
	out, err := tl.Execute(context.Background(), mustJSON(t, map[string]interface{}{
		"url": srv.URL, "method": "HEAD",
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "status: 200") || !strings.Contains(out, "content-length: 12345") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestFileLockerTool_NotLocked(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "free.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := &FileLockerTool{}
	out, err := tl.Execute(context.Background(), mustJSON(t, map[string]interface{}{"path": p}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "not locked") {
		t.Errorf("expected not-locked verdict, got:\n%s", out)
	}
}

func TestFileLockerTool_SelfLocked(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "busy.txt")
	if err := os.WriteFile(p, []byte("busy"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 直接以无共享语义独占打开（模拟外部进程持锁）；
	// 注意：不能先用 os.OpenFile 打开（Go 默认共享模式会与零共享打开自冲突）。
	if err := lockExclusive(p); err != nil {
		t.Skipf("cannot establish exclusive lock on this platform: %v", err)
	}
	defer unlockFile(p)

	tl := &FileLockerTool{}
	out, err := tl.Execute(context.Background(), mustJSON(t, map[string]interface{}{"path": p}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Windows：应识别出锁者（至少报告无法写打开）；非 Windows：降级探测也应报无法写打开。
	if !strings.Contains(out, "locked by") && !strings.Contains(out, "cannot be opened for write") {
		t.Errorf("expected locker/lock verdict, got:\n%s", out)
	}
}

func TestFileLockerTool_MissingFile(t *testing.T) {
	tl := &FileLockerTool{}
	if _, err := tl.Execute(context.Background(), mustJSON(t, map[string]interface{}{"path": filepath.Join(t.TempDir(), "nope.txt")})); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestProcessListTool_Smoke(t *testing.T) {
	tl := &ProcessListTool{}
	out, err := tl.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out == "" {
		t.Error("empty output")
	}
}

func TestProcessListTool_FilterByName(t *testing.T) {
	tl := &ProcessListTool{}
	// 用自身测试进程名过滤（go test 编译出的二进制名）。
	out, err := tl.Execute(context.Background(), mustJSON(t, map[string]interface{}{"name": "go"}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// 不校验命中（CI 环境进程名不定），仅验证不 panic、输出格式正确。
	if out == "" {
		t.Error("empty output")
	}
}
