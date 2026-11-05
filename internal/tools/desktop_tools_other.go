//go:build !windows

// Package tools 桌面自动化工具（非 Windows 平台桩）。
//
// desktop_* 工具集仅实现 Windows（user32/gdi32 syscall，对齐项目 "Windows 优先"
// 约定）；本文件保证 linux/darwin 交叉编译通过，注册处经 desktopSupported() 探测
// 后跳过注册（非 Windows 宿主模型不可见这些工具），Execute 兜底返回不支持提示。
package tools

import (
	"context"
	"fmt"
)

// desktopSupported 非 Windows 平台恒 false（注册处据此跳过注册）。
var desktopSupported = func() bool { return false }

// notSupportedErr 通用不支持错误。
func notSupportedErr() error {
	return fmt.Errorf("desktop automation is only supported on Windows")
}

// DesktopWindowsTool 桩。
type DesktopWindowsTool struct{}

func (t *DesktopWindowsTool) Name() string { return "desktop_windows" }
func (t *DesktopWindowsTool) Description() string {
	return "List top-level visible desktop windows (Windows only)."
}
func (t *DesktopWindowsTool) Parameters() map[string]interface{} {
	return objectSchema(nil, nil)
}
func (t *DesktopWindowsTool) Execute(ctx context.Context, args string) (string, error) {
	return "", notSupportedErr()
}

// DesktopScreenshotTool 桩。
type DesktopScreenshotTool struct{ ScreenshotDir string }

func (t *DesktopScreenshotTool) Name() string { return "desktop_screenshot" }
func (t *DesktopScreenshotTool) Description() string {
	return "Capture the screen or a window as PNG (Windows only)."
}
func (t *DesktopScreenshotTool) Parameters() map[string]interface{} {
	return objectSchema(nil, nil)
}
func (t *DesktopScreenshotTool) Execute(ctx context.Context, args string) (string, error) {
	return "", notSupportedErr()
}

// DesktopActTool 桩。
type DesktopActTool struct{}

func (t *DesktopActTool) Name() string { return "desktop_act" }
func (t *DesktopActTool) Description() string {
	return "Perform real keyboard/mouse input on the desktop (Windows only)."
}
func (t *DesktopActTool) Parameters() map[string]interface{} {
	return objectSchema(nil, nil)
}
func (t *DesktopActTool) Execute(ctx context.Context, args string) (string, error) {
	return "", notSupportedErr()
}
