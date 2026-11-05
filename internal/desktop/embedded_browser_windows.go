//go:build windows

// 内嵌浏览器宿主（2026-10 用户需求）：WebView2 内核可见窗口 + CDP 工具接管。
//
// 设计：
//   - browser_open/browser_eval/browser_screenshot 三件套经 tools.BrowserTool
//     的 EmbeddedLauncher 钩子进入本文件（依赖注入避免 internal/tools 反向依赖
//     internal/desktop）；本文件创建/复用可见 WebView2 窗口，返回其 CDP HTTP
//     调试端口，工具层自行经 /json/list 接管页面连接并 Page.navigate 导航。
//   - CDP 暴露机制：WebView2 环境创建前设 WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS
//     =--remote-debugging-port=N（环境变量在环境创建时读取，创建完成后即还原，
//     不影响 Wails 主窗口——主窗口环境在应用启动时已创建完毕）。
//   - 独立浏览器进程：DataPath 指向 ~/.kaelvoris/browser-profile（与 Wails 主
//     窗口 user-data-folder 不同源），WebView2 由此拉起专属 msedgewebview2
//     进程，附加调试参数才会生效；用户登录态跨任务保留。
//   - 生命周期：窗口跨任务存活（用户可见 UI，任务收尾 CloseBrowser 只断工具
//     侧 CDP 连接不销毁窗口）；用户 X 关闭 → Run 返回 → 清单例 + 清工具态，
//     下次 browser_open 按需重建（复用前经 CDP 存活校验，死窗口自动重建）；
//     应用 Shutdown 主动 Destroy。
//   - 线程模型：COM STA（库内 CoInitializeEx）+ Win32 消息循环必须锁同一 OS
//     线程——创建与 Run 全程 runtime.LockOSThread；跨线程仅 Destroy（PostMessage，
//     线程安全），不跨线程直调 COM。
//   - 锁序约定：本文件 ebMu 与工具侧 browserState.mu 互不嵌套持有（窗口
//     goroutine 收尾先放 ebMu 再调 tools.CloseBrowser），无死锁环。
package desktop

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/webviewloader"

	"kaelvoris/internal/tools"
)

// ebWindow 一个内嵌浏览器窗口实例（webview + 其 CDP 端口）。
type ebWindow struct {
	wv   webview2.WebView
	port int
}

var (
	ebMu sync.Mutex
	eb   *ebWindow // 进程级单例；nil = 无窗口
)

// ensureEmbeddedBrowser 创建或复用内嵌 WebView2 窗口，返回其 CDP HTTP 端口。
// 由 tools.BrowserTool.EmbeddedLauncher 注入调用（调用方持工具侧状态锁，本函数
// 不触碰工具侧锁）。失败返回 error，工具层静默回落 headless Edge/Chrome。
func ensureEmbeddedBrowser() (int, error) {
	// ---- 复用路径：旧窗口存活（CDP 可达）直接返回 ----
	ebMu.Lock()
	old := eb
	ebMu.Unlock()
	if old != nil {
		if waitCDPReady(old.port, 2*time.Second) == nil {
			return old.port, nil
		}
		// 窗口已死（X 关闭残留 / 浏览器进程崩溃）：摘除并异步销毁后重建。
		ebMu.Lock()
		if eb == old {
			eb = nil
		}
		ebMu.Unlock()
		go old.wv.Destroy() // PostMessage 线程安全；窗口 goroutine 随之自清
	}

	// ---- 创建路径 ----
	// WebView2 Runtime 预检：缺失时明确报错（工具层回落 headless），避免
	// 深入 NewWithOptions 后 log.Fatal 拖垮整个应用。
	ver, err := webviewloader.GetInstalledVersion()
	if err != nil {
		return 0, fmt.Errorf("webview2 runtime probe: %w", err)
	}
	if ver == "" {
		return 0, errors.New("webview2 runtime not installed")
	}

	port, err := freeTCPPort()
	if err != nil {
		return 0, fmt.Errorf("probe free port: %w", err)
	}
	// 独立 user-data-folder → 专属浏览器进程（附加调试参数生效的前提）；
	// 持久化目录使用户登录态跨任务保留。
	profile := filepath.Join(homeDir(), ".kaelvoris", "browser-profile")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		return 0, fmt.Errorf("create browser profile dir: %w", err)
	}

	ready := make(chan error, 1)
	go func() {
		// COM STA + 消息循环必须固定同一 OS 线程（库在环境创建时
		// CoInitializeEx(STA)；goroutine 可能被调度迁移线程）。
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// 环境变量在 WebView2 环境创建时读取；NewWithOptions 返回即环境
		// 已创建（Embed 内部泵消息等待 inited），defer 立即还原，把对
		// 进程全局环境的暴露窗口压到最短（Wails 主窗口环境早已创建，不受影响）。
		args := fmt.Sprintf("--remote-debugging-port=%d", port)
		_ = os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", args)
		defer os.Unsetenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS")

		w := webview2.NewWithOptions(webview2.WebViewOptions{
			Debug:    false,
			DataPath: profile,
			WindowOptions: webview2.WindowOptions{
				Title:  "Kaelvoris 内嵌浏览器",
				Width:  1100,
				Height: 760,
				Center: true,
			},
		})
		if w == nil {
			ready <- errors.New("create webview2 window failed")
			return
		}
		// 发布单例（短临界区；此时 ensureEmbeddedBrowser 主流程未持 ebMu）。
		ebMu.Lock()
		eb = &ebWindow{wv: w, port: port}
		ebMu.Unlock()
		ready <- nil

		w.Run() // 阻塞直到窗口关闭（WM_CLOSE → WM_DESTROY → WM_QUIT）

		// 收尾：身份比对摘单例（不误伤并发重建的新窗口），再清工具侧
		// CDP 连接态（先放 ebMu 再取 browserState.mu，维持锁序约定）。
		ebMu.Lock()
		if eb != nil && eb.wv == w {
			eb = nil
		}
		ebMu.Unlock()
		tools.CloseBrowser()
	}()

	select {
	case err := <-ready:
		if err != nil {
			return 0, err
		}
	case <-time.After(15 * time.Second):
		return 0, errors.New("create webview2 window timeout")
	}

	// 等浏览器进程拉起 CDP HTTP 端点（冷启动 1~3s 属正常）。
	if err := waitCDPReady(port, 15*time.Second); err != nil {
		// 窗口已建但 CDP 未就绪：摘单例并销毁，报错回落 headless。
		ebMu.Lock()
		w := eb
		eb = nil
		ebMu.Unlock()
		if w != nil {
			w.wv.Destroy()
		}
		return 0, err
	}
	return port, nil
}

// shutdownEmbeddedBrowser 应用退出时销毁内嵌窗口（App.Shutdown 调用）。
// Destroy 投递 WM_CLOSE（线程安全），窗口 goroutine 的 Run 随之返回并自清。
func shutdownEmbeddedBrowser() {
	ebMu.Lock()
	w := eb
	eb = nil
	ebMu.Unlock()
	if w != nil {
		w.wv.Destroy()
	}
}

// freeTCPPort 探测一个空闲回环端口（listen 后即关，存在竞窗但可接受——
// CDP 端点就绪探测会兜底验证）。
func freeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitCDPReady 轮询 CDP /json/version 直至 200（浏览器进程冷启动等待）。
func waitCDPReady(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	httpc := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := httpc.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("embedded browser CDP endpoint not ready within %s", timeout)
}
