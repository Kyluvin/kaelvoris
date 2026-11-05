//go:build !windows

// 内嵌浏览器非 Windows stub：WebView2 仅存在于 Windows，其他平台（服务端
// 交叉编译 GOOS=linux 等）恒返回失败，工具层静默回落 headless Edge/Chrome。
package desktop

import "errors"

// ensureEmbeddedBrowser 非 Windows 恒失败（见 embedded_browser_windows.go）。
func ensureEmbeddedBrowser() (int, error) {
	return 0, errors.New("embedded browser requires windows (webview2)")
}

// shutdownEmbeddedBrowser 非 Windows 无窗口，空实现。
func shutdownEmbeddedBrowser() {}
