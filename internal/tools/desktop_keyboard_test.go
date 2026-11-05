//go:build windows

package tools

// desktop_act 键盘卡键防护回归测试（2026-11 工具层可靠性加固）：
// hotkey 中途失败（未知键）时，已按下的修饰键必须被 defer 路径补 keyup——
// 用 GetAsyncKeyState 在真机（本机为 Windows）验证物理按键状态，
// 杜绝 ctrl/alt/win 卡在按下态导致用户键鼠体感失灵。
//
// 用例执行真实 SendInput/GetAsyncKeyState（对齐任务要求：//go:build windows
// 用例在 Windows 会跑），但注入面刻意最小化：只做修饰键 down→up 的瞬时
// 配对注入与单键键程，不移动鼠标、不产生字符输入、不依赖焦点窗口。

import (
	"context"
	"testing"
	"time"
)

var procGetAsyncKeyState = user32.NewProc("GetAsyncKeyState")

// keyIsPhysicallyDown 用 GetAsyncKeyState 查询虚拟键当前物理按下态。
func keyIsPhysicallyDown(vk uint16) bool {
	r, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	return r&0x8000 != 0 // 最高位 = 当前按下
}

// TestDesktopActHotkeyStuckKeysReleased hotkey 错误路径的卡键防护核心回归：
// keys=[ctrl, <无法映射的键名>] → ctrl down 成功后遇未知键报错退出，
// defer 必须补 ctrl keyup，物理态回到释放（修复前此路径 ctrl 永久卡住）。
func TestDesktopActHotkeyStuckKeysReleased(t *testing.T) {
	tool := &DesktopActTool{}
	ctx := context.Background()

	// 收尾清场兜底：即使实现回退导致卡键，测试结束后也把修饰键补 keyup，
	// 不把卡键遗留给用户桌面。
	defer func() {
		for _, vk := range []uint16{0x11, 0x12, 0x10} { // ctrl/alt/shift
			_ = keybdInput(vk, 0, keyeventfKeyup)
		}
	}()

	// 起点清场：对全部修饰键补 keyup（对已释放键无副作用），
	// 防其他进程残留按下态造成误判。
	for _, vk := range []uint16{0x11, 0x12, 0x10} { // ctrl/alt/shift
		_ = keybdInput(vk, 0, keyeventfKeyup)
	}
	time.Sleep(30 * time.Millisecond)

	// 未知键错误路径：ctrl 已真实按下 → 报 unknown key 退出 → defer 释放。
	if _, err := tool.Execute(ctx, `{"action":"hotkey","keys":["ctrl","unknown-key-name"]}`); err == nil {
		t.Fatal("expected unknown-key error")
	}
	time.Sleep(50 * time.Millisecond) // 等 defer 注入的 keyup 生效
	if keyIsPhysicallyDown(0x11) {
		t.Error("ctrl stuck down after unknown-key error path (defer keyup missed)")
	}
}

// TestDesktopActHotkeySuccessReleasesModifiers hotkey 成功路径（纯修饰键组合，
// 不产生字符输入）：downVKs 为空、defer 正常执行，两个修饰键均回到释放态。
func TestDesktopActHotkeySuccessReleasesModifiers(t *testing.T) {
	tool := &DesktopActTool{}
	if _, err := tool.Execute(context.Background(), `{"action":"hotkey","keys":["shift","ctrl"]}`); err != nil {
		t.Fatalf("hotkey shift+ctrl: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if keyIsPhysicallyDown(0x10) {
		t.Error("shift stuck down after successful modifier-only hotkey")
	}
	if keyIsPhysicallyDown(0x11) {
		t.Error("ctrl stuck down after successful modifier-only hotkey")
	}
}

// TestDesktopActPressKeyFullStroke press 单键键程完整性：down→up 后物理态回到
// 释放（修复后 up 失败不再阻断返回，键程必达）。
func TestDesktopActPressKeyFullStroke(t *testing.T) {
	tool := &DesktopActTool{}
	if _, err := tool.Execute(context.Background(), `{"action":"press","key":"F8"}`); err != nil {
		t.Fatalf("press F8: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if keyIsPhysicallyDown(0x77) { // VK_F8
		t.Error("F8 stuck down after press (down without matching up)")
	}
}

// TestDesktopActTypeUnicode type 路径 Unicode 注入（含中文）正常返回。
// type 不涉及修饰键（每字符自带配对 up，best-effort 语义），无卡键面。
func TestDesktopActTypeUnicode(t *testing.T) {
	tool := &DesktopActTool{}
	if _, err := tool.Execute(context.Background(), `{"action":"type","text":"ab中"}`); err != nil {
		t.Fatalf("type unicode: %v", err)
	}
}
