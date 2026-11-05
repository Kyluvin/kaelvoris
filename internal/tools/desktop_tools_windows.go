//go:build windows

// Package tools 桌面自动化工具（Codex ComputerUse Windows 能力面对齐）。
//
// 2026-10 深度嵌入：分析 openai/codex 后移植。Codex 侧 ComputerUse = 窗口枚举 +
// 屏幕截图（AppScreenshot）+ 真实键鼠输入，管控面为 Windows exe/AUMID 白名单 +
// 审批生命周期；执行在其闭源桌面客户端。本移植取能力面（Windows 优先，对齐项目
// "Windows 优先" 约定），管控由既有 approval 体系承接（desktop_* 属 command 组）。
//
// 实现：纯 syscall（user32/gdi32/gdiplus），零新增依赖，无 CGO。
//   - desktop_windows：枚举顶层可见窗口（标题/PID/类名/坐标）→ ComputerUse 窗口定位
//   - desktop_screenshot：全屏或指定窗口截图 → PNG 落盘 + KAELVORIS_MEDIA 推送聊天流
//   - desktop_act：真实键鼠输入（SendInput click/type/key/scroll/hotkey）
//
// 其余平台（linux/darwin）本组工具不注册：宿主注册前经 desktopSupported() 探测。
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// desktopSupported Windows 平台恒 true（init 覆盖 other 文件的 false 默认；
// 注册处据此决定是否注册 desktop_* 工具）。
var desktopSupported = func() bool { return true }

// ── Win32 API 声明（仅本文件使用的最小子集）─────────────────────────────────

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procGetDesktopWindow         = user32.NewProc("GetDesktopWindow")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW     = user32.NewProc("GetWindowTextLengthW")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procShowWindow               = user32.NewProc("ShowWindow")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procSendInput                = user32.NewProc("SendInput")
	procGetWindowDC              = user32.NewProc("GetWindowDC")
	procReleaseDC                = user32.NewProc("ReleaseDC")

	gdi32                      = windows.NewLazySystemDLL("gdi32.dll")
	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procGetDIBits              = gdi32.NewProc("GetDIBits")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procGetDeviceCaps          = gdi32.NewProc("GetDeviceCaps")

	user32PrintWindow = user32.NewProc("PrintWindow")
)

// Windows 常量（虚拟屏度量 / ShowWindow / PrintWindow）。
const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
	swRestore         = 9
	// pwRenderTarget PrintWindow flag（PW_RENDERFULLCONTENT，Win 8.1+ 硬件加速窗口）。
	pwRenderTarget = 2
)

// ── desktop_windows ──────────────────────────────────────────────────────────

// DesktopWindowsTool 枚举桌面顶层可见窗口。
type DesktopWindowsTool struct{}

func (t *DesktopWindowsTool) Name() string { return "desktop_windows" }

func (t *DesktopWindowsTool) Description() string {
	return "List top-level visible desktop windows (title, PID, class name, position) — Windows desktop automation. " +
		"Use to find a window before desktop_act(focus) or desktop_screenshot(window)."
}

func (t *DesktopWindowsTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"filter": strProp("Case-insensitive substring filter on window title (optional)"),
	}, nil)
}

// winInfo 枚举到的窗口信息。
type winInfo struct {
	hwnd       uintptr
	title      string
	class      string
	pid        uint32
	x, y, w, h int
}

// enumWindowsInfo 枚举全部顶层可见窗口（带 128 字符标题截断）。
func enumWindowsInfo() []winInfo {
	var out []winInfo
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		vis, _, _ := procIsWindowVisible.Call(hwnd)
		if vis == 0 {
			return 1 // 继续枚举
		}
		var wi winInfo
		wi.hwnd = hwnd
		// 标题。
		n, _, _ := procGetWindowTextLengthW.Call(hwnd)
		if n > 0 {
			buf := make([]uint16, n+1)
			procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), n+1)
			wi.title = windows.UTF16ToString(buf)
		}
		// 类名。
		cls := make([]uint16, 256)
		procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&cls[0])), 256)
		wi.class = windows.UTF16ToString(cls)
		// PID。
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&wi.pid)))
		// 坐标。
		type rect struct{ l, t, r, b int32 }
		var rc rect
		procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
		wi.x, wi.y, wi.w, wi.h = int(rc.l), int(rc.t), int(rc.r-rc.l), int(rc.b-rc.t)
		out = append(out, wi)
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return out
}

func (t *DesktopWindowsTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Filter string `json:"filter"`
	}
	if strings.TrimSpace(args) != "" {
		_ = json.Unmarshal([]byte(args), &p)
	}
	wins := enumWindowsInfo()
	var b strings.Builder
	count := 0
	for _, w := range wins {
		if p.Filter != "" && !strings.Contains(strings.ToLower(w.title), strings.ToLower(p.Filter)) {
			continue
		}
		// 无标题窗口多为隐藏辅助窗，过滤噪音。
		if strings.TrimSpace(w.title) == "" {
			continue
		}
		fmt.Fprintf(&b, "  hwnd=0x%x pid=%d class=%q rect=(%d,%d %dx%d) %q\n",
			w.hwnd, w.pid, w.class, w.x, w.y, w.w, w.h, clipStr(w.title, 80))
		count++
	}
	if count == 0 {
		return "no matching visible windows", nil
	}
	return fmt.Sprintf("visible windows (%d):\n%s", count, b.String()), nil
}

// ── desktop_screenshot ───────────────────────────────────────────────────────

// DesktopScreenshotTool 截取全屏或指定窗口 → PNG 推送聊天流。
type DesktopScreenshotTool struct {
	// ScreenshotDir 截图保存目录（空 = ~/.kaelvoris/media/screenshots）。
	ScreenshotDir string
}

func (t *DesktopScreenshotTool) Name() string { return "desktop_screenshot" }

func (t *DesktopScreenshotTool) Description() string {
	return "Capture the entire screen or a specific window (by hwnd from desktop_windows) as PNG, save to disk, and show it in chat. " +
		"Windows desktop automation — use to see what the user sees before desktop_act."
}

func (t *DesktopScreenshotTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"hwnd": numProp("Window handle (0x... from desktop_windows) to capture instead of full screen"),
	}, nil)
}

func (t *DesktopScreenshotTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Hwnd int64 `json:"hwnd"`
	}
	if strings.TrimSpace(args) != "" {
		_ = json.Unmarshal([]byte(args), &p)
	}
	raw, err := captureWindowPNG(uintptr(p.Hwnd))
	if err != nil {
		return "", err
	}
	dir := t.ScreenshotDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			dir = filepath.Join(os.TempDir(), "kaelvoris-media", "screenshots")
		} else {
			dir = filepath.Join(home, ".kaelvoris", "media", "screenshots")
		}
	}
	_ = os.MkdirAll(dir, 0o755)
	name := fmt.Sprintf("desktop-%s.png", time.Now().Format("20060102-150405"))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Desktop screenshot saved: " + path + "\n")
	b.WriteString("The screenshot has been shown to the user. Briefly confirm; do not embed markdown image links.")
	b.WriteString(BuildMediaPayload([]MediaPayload{{
		Kind:     "image",
		Path:     path,
		DataURL:  MustDataURL(raw),
		FileName: name,
	}}))
	return b.String(), nil
}

// ── desktop_act ──────────────────────────────────────────────────────────────

// DesktopActTool 真实键鼠输入（SendInput）。
type DesktopActTool struct{}

func (t *DesktopActTool) Name() string { return "desktop_act" }

func (t *DesktopActTool) Description() string {
	return "Perform real keyboard/mouse input on the Windows desktop: click (at x,y), double_click, right_click, " +
		"type text, press key (Enter/Tab/...), hotkey (ctrl+c), scroll, or focus a window (by hwnd from desktop_windows). " +
		"Use desktop_screenshot first to see the screen, then desktop_act to interact — like a human would."
}

func (t *DesktopActTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"action": map[string]interface{}{
			"type":        "string",
			"enum":        []string{"click", "double_click", "right_click", "type", "press", "hotkey", "scroll", "focus", "move"},
			"description": "Input primitive",
		},
		"x":    numProp("Screen X for click/move"),
		"y":    numProp("Screen Y for click/move"),
		"text": strProp("Text for action=type"),
		"key":  strProp("Key for action=press (Enter/Tab/Escape/...) or single key in hotkey"),
		"keys": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Keys for action=hotkey, e.g. [\"ctrl\",\"c\"]"},
		"px":   numProp("Scroll pixels (positive = down) for action=scroll"),
		"hwnd": numProp("Window handle for action=focus (from desktop_windows)"),
	}, []string{"action"})
}

// actReq desktop_act 参数。
type actReq struct {
	Action string   `json:"action"`
	X      int      `json:"x"`
	Y      int      `json:"y"`
	Text   string   `json:"text"`
	Key    string   `json:"key"`
	Keys   []string `json:"keys"`
	Px     int      `json:"px"`
	Hwnd   int64    `json:"hwnd"`
}

// sendInputWin SendInput 调用（键盘鼠标统一入口；INPUT 结构按 Win64 布局手排：
// type(4)+pad(4)+MOUSEINPUT/KEYBDINPUT union(32) = 40 字节）。
func sendInputWin(kind uintptr, flags uintptr, dx, dy int32, data uintptr) error {
	// INPUT { type(4) + pad(4) + union(32) } = 40 字节（Win64）。
	var buf [40]byte
	*(*uint32)(unsafe.Pointer(&buf[0])) = uint32(kind) // 0=MOUSE 1=KEYBOARD
	// MOUSEINPUT 布局（union 首选）：dx(4) dy(4) mouseData(4) dwFlags(4) time(4) pad(4) extra(8)
	off := 8
	*(*int32)(unsafe.Pointer(&buf[off])) = dx
	*(*int32)(unsafe.Pointer(&buf[off+4])) = dy
	*(*uint32)(unsafe.Pointer(&buf[off+8])) = uint32(data)
	*(*uint32)(unsafe.Pointer(&buf[off+12])) = uint32(flags)
	r, _, err := procSendInput.Call(1, uintptr(unsafe.Pointer(&buf[0])), 40)
	if r == 0 {
		// UIPI 指引（2026-11 复盘：目标窗口以管理员运行时低完整性进程的
		// SendInput 被静默拦截，模型只能反复盲试）：附可操作指引。
		return fmt.Errorf("SendInput failed: %v (目标窗口可能以管理员运行，UIPI 拦截注入；"+
			"建议以同等权限运行本程序，或改用 desktop_windows+focus 切到普通权限窗口)", err)
	}
	return nil
}

// keybdInput Win64 KEYBDINPUT 布局的 SendInput。
func keybdInput(vk, scan, flags uint16) error {
	var buf [40]byte
	*(*uint32)(unsafe.Pointer(&buf[0])) = 1 // INPUT_KEYBOARD
	off := 8
	*(*uint16)(unsafe.Pointer(&buf[off])) = vk
	*(*uint16)(unsafe.Pointer(&buf[off+2])) = scan
	*(*uint16)(unsafe.Pointer(&buf[off+4])) = flags
	r, _, err := procSendInput.Call(1, uintptr(unsafe.Pointer(&buf[0])), 40)
	if r == 0 {
		// UIPI 指引：与 sendInputWin 同源（管理员目标窗口拦截键鼠注入）。
		return fmt.Errorf("SendInput(key) failed: %v (目标窗口可能以管理员运行，UIPI 拦截注入；"+
			"建议以同等权限运行本程序，或换普通权限窗口)", err)
	}
	return nil
}

// 鼠标 flags。
const (
	mouseeventfMove        = 0x0001
	mouseeventfLeftdown    = 0x0002
	mouseeventfLeftup      = 0x0004
	mouseeventfRightdown   = 0x0008
	mouseeventfRightup     = 0x0010
	mouseeventfMidDown     = 0x0020
	mouseeventfMidUp       = 0x0040
	mouseeventfWheel       = 0x0800
	mouseeventfAbsolute    = 0x8000
	mouseeventfVirtualdesk = 0x4000
)

// 键盘 flags。
const (
	keyeventfExtendedkey = 0x0001
	keyeventfKeyup       = 0x0002
	keyeventfUnicode     = 0x0004
)

// vkMap 常用键 VK 码。
var vkMap = map[string]uint16{
	"enter": 0x0D, "return": 0x0D, "tab": 0x09, "escape": 0x1B, "esc": 0x1B,
	"backspace": 0x08, "delete": 0x2E, "del": 0x2E, "space": 0x20,
	"arrowup": 0x26, "up": 0x26, "arrowdown": 0x28, "down": 0x28,
	"arrowleft": 0x25, "left": 0x25, "arrowright": 0x27, "right": 0x27,
	"home": 0x24, "end": 0x23, "pageup": 0x21, "pagedown": 0x22,
	"f1": 0x70, "f2": 0x71, "f3": 0x72, "f4": 0x73, "f5": 0x74, "f6": 0x75,
	"f7": 0x76, "f8": 0x77, "f9": 0x78, "f10": 0x79, "f11": 0x7A, "f12": 0x7B,
	"ctrl": 0x11, "control": 0x11, "alt": 0x12, "shift": 0x10, "win": 0x5B,
	"insert": 0x2D, "capslock": 0x14, "printscreen": 0x2C,
}

// vkFor 键名 → VK 码（单字符 a-z0-9 直算，其余查表）。
func vkFor(name string) (uint16, bool) {
	k := strings.ToLower(strings.TrimSpace(name))
	if v, ok := vkMap[k]; ok {
		return v, true
	}
	if len(k) == 1 {
		c := k[0]
		if c >= 'a' && c <= 'z' {
			return uint16(c - 'a' + 0x41), true
		}
		if c >= '0' && c <= '9' {
			return uint16(c - '0' + 0x30), true
		}
	}
	return 0, false
}

// typeText Unicode 文本输入（KEYEVENTF_UNICODE，任意字符含中文）。
// 最佳努力语义：单字符 down/up 失败不中断剩余文本（否则长文本中途卡键且
// 后续全部丢失）；函数末尾返回首个错误供调用方提示（错误已带 UIPI 指引）。
func typeText(text string) error {
	var firstErr error
	for _, r := range text {
		if r == '\n' {
			if err := keybdInput(0x0D, 0, 0); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if err := keybdInput(0x0D, 0, keyeventfKeyup); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := keybdInput(0, uint16(r), keyeventfUnicode); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue // down 失败则本字符无 up 可配对，跳过。
		}
		if err := keybdInput(0, uint16(r), keyeventfUnicode|keyeventfKeyup); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// pressKey 单键完整键程 down→up（10ms 间隔）。
// 卡键防护：down 成功后 up 必达——up 失败不掩盖 down 成功的事实，
// 返回 nil（键已完整注入，仅抬起环节异常时系统自动弹起兜底，无卡键风险）。
func pressKey(vk uint16) error {
	if err := keybdInput(vk, 0, 0); err != nil {
		return err
	}
	time.Sleep(10 * time.Millisecond)
	_ = keybdInput(vk, 0, keyeventfKeyup)
	return nil
}

func (t *DesktopActTool) Execute(ctx context.Context, args string) (string, error) {
	var p actReq
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	switch p.Action {
	case "focus":
		hwnd := uintptr(p.Hwnd)
		if hwnd == 0 {
			return "", fmt.Errorf("focus requires hwnd (from desktop_windows)")
		}
		// 先还原再前置（最小化窗口 SetForegroundWindow 无效）。
		procShowWindow.Call(hwnd, swRestore)
		procSetForegroundWindow.Call(hwnd)
		time.Sleep(150 * time.Millisecond)
		return "window focused", nil
	case "move":
		if p.X == 0 && p.Y == 0 {
			return "", fmt.Errorf("move requires x/y")
		}
		sendMouseAbs(mouseeventfMove, int32(p.X), int32(p.Y), 0)
		return fmt.Sprintf("moved to (%d,%d)", p.X, p.Y), nil
	case "click", "double_click", "right_click":
		if p.X != 0 || p.Y != 0 {
			sendMouseAbs(mouseeventfMove, int32(p.X), int32(p.Y), 0)
			time.Sleep(30 * time.Millisecond)
		}
		down, up := uintptr(mouseeventfLeftdown), uintptr(mouseeventfLeftup)
		if p.Action == "right_click" {
			down, up = uintptr(mouseeventfRightdown), uintptr(mouseeventfRightup)
		}
		if err := sendMouseWin(down); err != nil {
			return "", err
		}
		if err := sendMouseWin(up); err != nil {
			return "", err
		}
		if p.Action == "double_click" {
			time.Sleep(40 * time.Millisecond)
			_ = sendMouseWin(down)
			_ = sendMouseWin(up)
		}
		pos := ""
		if p.X != 0 || p.Y != 0 {
			pos = fmt.Sprintf(" at (%d,%d)", p.X, p.Y)
		}
		return p.Action + pos + " done", nil
	case "type":
		if p.Text == "" {
			return "", fmt.Errorf("type requires text")
		}
		if err := typeText(p.Text); err != nil {
			return "", err
		}
		return fmt.Sprintf("typed %d chars", len([]rune(p.Text))), nil
	case "press":
		vk, ok := vkFor(p.Key)
		if !ok {
			return "", fmt.Errorf("unknown key %q", p.Key)
		}
		if err := pressKey(vk); err != nil {
			return "", err
		}
		return "pressed " + p.Key, nil
	case "hotkey":
		keys := p.Keys
		if len(keys) == 0 && p.Key != "" {
			keys = strings.Split(p.Key, "+") // "ctrl+c" 快捷写法
		}
		if len(keys) == 0 {
			return "", fmt.Errorf("hotkey requires keys")
		}
		// 修正序：修饰键先 down，最后普通键；逆序 up。
		// 卡键防护（2026-11 复盘：hotkey 中途失败直接 return，ctrl/alt/win
		// 卡在按下态，用户键鼠体感失灵）：defer 必达释放——任何错误路径
		//（未知键/SendInput 失败）退出前都对全部修饰键补 keyup；对未按下
		// 的修饰键发 keyup 系统忽略，无副作用。
		defer func() {
			for i := len(keys) - 1; i >= 0; i-- {
				if !isModifier(keys[i]) {
					continue
				}
				if vk, ok := vkFor(keys[i]); ok {
					_ = keybdInput(vk, 0, keyeventfKeyup)
				}
			}
		}()
		var downVKs []uint16
		for _, k := range keys {
			vk, ok := vkFor(k)
			if !ok {
				return "", fmt.Errorf("unknown key %q", k)
			}
			if isModifier(k) {
				if err := keybdInput(vk, 0, 0); err != nil {
					return "", err
				}
			} else {
				downVKs = append(downVKs, vk)
			}
		}
		for _, vk := range downVKs {
			if err := pressKey(vk); err != nil {
				return "", err
			}
		}
		return "hotkey " + strings.Join(keys, "+") + " done", nil
	case "scroll":
		if p.Px == 0 {
			p.Px = 400
		}
		// WHEEL_DELTA=120；px→格数换算。
		notches := p.Px / 120
		if notches == 0 {
			notches = sign(p.Px)
		}
		sendMouseAbs(mouseeventfWheel, 0, 0, uintptr(int32(-notches*120))) // 正值=上滚，取反
		return fmt.Sprintf("scrolled %dpx", p.Px), nil
	default:
		return "", fmt.Errorf("unknown action %q (click/double_click/right_click/type/press/hotkey/scroll/focus/move)", p.Action)
	}
}

// isModifier 是否修饰键。
func isModifier(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "ctrl", "control", "alt", "shift", "win":
		return true
	}
	return false
}

// sign 符号函数。
func sign(v int) int {
	if v < 0 {
		return -1
	}
	return 1
}

// sendMouseAbs 绝对坐标鼠标事件（虚拟屏坐标归一化 0-65535）。
// wheel 事件（x=y=0 且带 wheel flag）无需坐标归一化，直接透传 data。
func sendMouseAbs(flags uintptr, x, y int32, data uintptr) error {
	if x != 0 || y != 0 {
		// 多显示器虚拟屏原点偏移归一化。
		vx, vy, vw, vh := virtScreen()
		nx := (x - vx) * 65535 / maxI32(1, vw-1)
		ny := (y - vy) * 65535 / maxI32(1, vh-1)
		flags |= mouseeventfAbsolute | mouseeventfVirtualdesk
		return sendInputWin(0, flags, nx, ny, data)
	}
	return sendInputWin(0, flags, x, y, data)
}

// sendMouseWin 原始鼠标 flags（click down/up，无坐标）。
func sendMouseWin(flags uintptr) error {
	return sendInputWin(0, flags, 0, 0, 0)
}

// virtScreen 虚拟屏度量（x/y 原点，w/h 尺寸）。
func virtScreen() (int32, int32, int32, int32) {
	x, _, _ := procGetSystemMetrics.Call(smXVirtualScreen)
	y, _, _ := procGetSystemMetrics.Call(smYVirtualScreen)
	w, _, _ := procGetSystemMetrics.Call(smCXVirtualScreen)
	h, _, _ := procGetSystemMetrics.Call(smCYVirtualScreen)
	return int32(x), int32(y), int32(w), int32(h)
}

// maxI32。
func maxI32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// ── 截图实现（GDI BitBlt → PNG 编码）────────────────────────────────────────

// captureWindowPNG 截取窗口/全屏 → PNG 字节。
// hwnd=0 截虚拟屏全桌面（BitBlt）；指定 hwnd 用 PrintWindow（后台窗口也能截）。
func captureWindowPNG(hwnd uintptr) ([]byte, error) {
	var w, h int32
	var srcDC uintptr
	releaseSrc := func() {}
	if hwnd == 0 {
		_, _, cw, ch := virtScreen()
		w, h = cw, ch
		d, _, _ := procGetWindowDC.Call(0)
		srcDC = d
		releaseSrc = func() { procReleaseDC.Call(0, d) }
	} else {
		type rect struct{ l, t, r, b int32 }
		var rc rect
		procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
		w, h = rc.r-rc.l, rc.b-rc.t
		if w <= 0 || h <= 0 {
			return nil, fmt.Errorf("window has zero size (minimized?)")
		}
		d, _, _ := procGetWindowDC.Call(hwnd)
		srcDC = d
		releaseSrc = func() { procReleaseDC.Call(hwnd, d) }
	}
	defer releaseSrc()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("screen size unknown")
	}

	hdcMem, _, _ := procCreateCompatibleDC.Call(srcDC)
	hbm, _, _ := procCreateCompatibleBitmap.Call(srcDC, uintptr(w), uintptr(h))
	defer procDeleteObject.Call(hbm)
	defer procDeleteDC.Call(hdcMem)
	old, _, _ := procSelectObject.Call(hdcMem, hbm)
	defer procSelectObject.Call(hdcMem, old)

	if hwnd == 0 {
		procBitBlt.Call(hdcMem, 0, 0, uintptr(w), uintptr(h), srcDC, 0, 0, 0x00CC0020 /*SRCCOPY*/)
	} else {
		// PrintWindow(PW_RENDERFULLCONTENT)：后台/硬件加速窗口也能截。
		user32PrintWindow.Call(hwnd, hdcMem, pwRenderTarget)
	}

	// GetDIBits 读像素（32bpp BGRA 顶向下）。
	type bmiHeader struct {
		size                  uint32
		width, height         int32
		planes, bitcount      uint16
		compression           uint32
		sizeimage             uint32
		xppm, yppm            int32
		clrused, clrimportant uint32
	}
	type bmi struct {
		hdr bmiHeader
		pal [1]uint32
	}
	var bi bmi
	bi.hdr.size = 40
	bi.hdr.width = int32(w)
	bi.hdr.height = -int32(h) // 负高 = 顶向下
	bi.hdr.planes = 1
	bi.hdr.bitcount = 32
	pix := make([]byte, int(w)*int(h)*4)
	procGetDIBits.Call(hdcMem, hbm, 0, uintptr(h), uintptr(unsafe.Pointer(&pix[0])), uintptr(unsafe.Pointer(&bi)), 0 /*DIB_RGB_COLORS*/)
	return encodePNGFromBGRA(pix, int(w), int(h))
}

// encodePNGFromBGRA BGRA 像素 → PNG（标准库 image/png）。
func encodePNGFromBGRA(pix []byte, w, h int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// BGRA → RGBA 像素序转换。
	for y := 0; y < h; y++ {
		row := pix[y*w*4 : (y+1)*w*4]
		for x := 0; x < w; x++ {
			off := x * 4
			img.SetRGBA(x, y, color.RGBA{row[off+2], row[off+1], row[off], row[off+3]})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
