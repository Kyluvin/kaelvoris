//go:build headless

package desktop

// ui_bridge_headless.go UI 桥（无头形态，-tags headless）：事件日志化 + 目录选择禁用。
//
// 与 ui_bridge_wails.go 同签名（构建标签互斥，业务代码零感知）：
//   - uiEmit：事件写结构化日志（kaelvoris:* 事件是前端刷新信号，无头端无前端，
//     仅留痕供排查；数据载荷不落日志——含 APIKey 掩码前配置等敏感内容）。
//   - uiPickDir：无 GUI 拒绝并提示走配置文件（desktop.json 的 workspace 等）。

import (
	"context"
	"fmt"
	"log/slog"
)

// uiEmit 事件推送（无头：日志留痕）。
func uiEmit(ctx context.Context, name string, data ...interface{}) {
	slog.Info("ui-event", "name", name)
}

// uiPickDir 目录选择（无头：不支持，提示走配置文件）。
func uiPickDir(ctx context.Context, title string) (string, error) {
	return "", fmt.Errorf("无头模式不支持图形目录选择（%s），请在配置文件中指定路径", title)
}

// uiSaveFile 保存文件对话框（无头：不支持，提示用文件系统复制）。
func uiSaveFile(ctx context.Context, title, defaultName, ext string) (string, error) {
	return "", fmt.Errorf("无头模式不支持图形保存对话框（%s），产物已落盘 ~/.kaelvoris/", title)
}
