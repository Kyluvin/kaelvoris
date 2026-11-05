//go:build windows

// 内嵌浏览器端到端测试（真实 WebView2 窗口 + CDP 全链路）：
// 首启 → CDP 就绪 → 复用同端口 → BrowserTool 钩子注入经 browser_open 导航
// httptest 页面并提取标题 → headless 回落开关关闭路径。CI 无 GUI 自动跳过。
package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"kaelvoris/internal/tools"
)

func TestEmbeddedBrowserE2E(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("CI 无 GUI/WebView2 Runtime，跳过")
	}

	// ---- 1. 首启：窗口创建 + CDP 就绪 ----
	port, err := ensureEmbeddedBrowser()
	if err != nil {
		t.Fatalf("ensureEmbeddedBrowser 首启: %v", err)
	}
	t.Logf("内嵌浏览器已启动，CDP 端口 %d", port)

	// ---- 2. 复用：同端口（不重复建窗）----
	port2, err := ensureEmbeddedBrowser()
	if err != nil {
		t.Fatalf("ensureEmbeddedBrowser 复用: %v", err)
	}
	if port2 != port {
		t.Fatalf("复用端口变了: %d != %d（应复用既有窗口）", port2, port)
	}

	// ---- 3. 全链路：BrowserTool 钩子注入 → browser_open 导航提取标题 ----
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>EmbeddedE2E-ok-7391</title></head><body><p>hello embedded</p></body></html>"))
	}))
	defer htmlSrv.Close()

	bt := &tools.BrowserTool{
		EmbeddedEnabled:  func() bool { return true },
		EmbeddedLauncher: func() (int, error) { return ensureEmbeddedBrowser() },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	out, err := bt.Execute(ctx, `{"url":"`+htmlSrv.URL+`/page1"}`)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("browser_open(内嵌): %v", err)
	}
	if !strings.Contains(out, "EmbeddedE2E-ok-7391") {
		t.Fatalf("标题缺失: %s", out[:min(len(out), 300)])
	}
	t.Logf("browser_open(内嵌) 耗时 %s", elapsed)

	// ---- 4. 收尾：清工具态 + 销毁窗口 ----
	bt.Close()
	shutdownEmbeddedBrowser()
	time.Sleep(500 * time.Millisecond) // 窗口 goroutine 收尾
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
