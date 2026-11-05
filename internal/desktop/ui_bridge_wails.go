//go:build !headless

package desktop

// ui_bridge_wails.go UI 桥（桌面形态，默认构建）：Wails 运行时直连。
//
// 无头改造收口层（plans/headless-pc-node-plan.md S1）：根包全部 UI 依赖
// （wruntime.EventsEmit / OpenDirectoryDialog）统一走本文件的两个桥函数，
// headless 构建标签下由 ui_bridge_headless.go 提供同签名替换实现，
// 桌面/无头共用一份业务代码（remote_panel/flows/channels/... 零感知）。

import (
	"context"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// uiEmit 事件推送（桌面：Wails EventsEmit 广播到前端）。
// 注意 data... 必须展开：EventsEmit 为变参（optionalData ...interface{}），
// JS 端回调按 data 数组展开接收（events.js: callback.apply(null, data)）。
// 误传 data（切片整体作单个元素）会让前端收到 [[payload]] 双层包裹，
// payload.sessionId/message 全部变 undefined → 消息事件被整体丢弃（无回复 BUG）。
func uiEmit(ctx context.Context, name string, data ...interface{}) {
	if ctx == nil {
		return
	}
	wruntime.EventsEmit(ctx, name, data...)
}

// uiPickDir 图形目录选择（桌面：系统目录选择对话框）。
func uiPickDir(ctx context.Context, title string) (string, error) {
	return wruntime.OpenDirectoryDialog(ctx, wruntime.OpenDialogOptions{
		Title: title,
	})
}

// uiSaveFile 图形保存文件对话框（桌面：系统「另存为」）。
// defaultName 为缺省文件名，ext 为缺省扩展名（无需点号；空 = 任意类型）。
// 用户取消返回 ("", nil)（调用方静默处理）。
func uiSaveFile(ctx context.Context, title, defaultName, ext string) (string, error) {
	opts := wruntime.SaveDialogOptions{
		Title:           title,
		DefaultFilename: defaultName,
	}
	if ext != "" {
		opts.Filters = []wruntime.FileFilter{{
			DisplayName: ext + " 文件",
			Pattern:     "*." + ext,
		}}
	}
	return wruntime.SaveFileDialog(ctx, opts)
}
