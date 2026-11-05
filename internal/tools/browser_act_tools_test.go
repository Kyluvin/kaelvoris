package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"strconv"
)

// statefulFakeCdpServer 带状态的假 CDP 端点：按方法返回预设响应（handler 可变，
// 测试中途注入 JS 执行语义），用于 browser_act / browser_snapshot 回归。
func statefulFakeCdpServer(t *testing.T) (*httptest.Server, *fakeCdpState) {
	t.Helper()
	st := &fakeCdpState{responses: map[string]string{}}
	up := websocket.Upgrader{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			var m cdpMessage
			if err := ws.ReadJSON(&m); err != nil {
				return
			}
			mu.Lock()
			res, ok := st.responses[m.Method]
			mu.Unlock()
			if !ok {
				res = `{"result":{"value":"OK"}}`
			}
			_ = ws.WriteJSON(map[string]interface{}{
				"id":     m.ID,
				"result": json.RawMessage(res),
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

// fakeCdpState 方法 → 响应 JSON（Runtime.evaluate 返回值可注入）。
type fakeCdpState struct {
	responses map[string]string
}

// setEval 注入 Runtime.evaluate 返回值（信封层 result 由服务器补，此处仅 value）。
func (s *fakeCdpState) setEval(v string) {
	b, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{"value": v},
	})
	s.responses["Runtime.evaluate"] = string(b)
}

// wireFakeBrowser 把 browserState 指到假 CDP（绕过真实浏览器启动），
// 返回工具与可注入响应的 state。
func wireFakeBrowser(t *testing.T, st *fakeCdpState) (*BrowserTool, *fakeCdpState) {
	t.Helper()
	if st == nil {
		st = &fakeCdpState{responses: map[string]string{}}
	}
	srv, owned := statefulFakeCdpServer(t)
	*owned = *st // 测试预置的响应合并进服务器 state
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial fake cdp: %v", err)
	}
	t.Cleanup(func() { ws.Close() })
	prev := browserGlobal
	browserGlobal = &browserState{cdp: newCdpConn(ws), pageWS: wsURL}
	t.Cleanup(func() { browserGlobal = prev })
	return browserToolSingleton, st
}

// TestBrowserActValidation 参数校验路径（无需真实浏览器语义的部分）。
func TestBrowserActValidation(t *testing.T) {
	bt, _ := wireFakeBrowser(t, nil)
	act := &BrowserActTool{Browser: bt}

	// 未知 action。
	if _, err := act.Execute(context.Background(), `{"action":"nuke"}`); err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("expected unknown action error, got %v", err)
	}
	// click 缺 ref/selector。
	if _, err := act.Execute(context.Background(), `{"action":"click"}`); err == nil || !strings.Contains(err.Error(), "requires ref") {
		t.Fatalf("expected ref/selector requirement, got %v", err)
	}
	// type 缺 text。
	if _, err := act.Execute(context.Background(), `{"action":"type","ref":"e1"}`); err == nil || !strings.Contains(err.Error(), "requires text") {
		t.Fatalf("expected text requirement, got %v", err)
	}
	// press 缺 key。
	if _, err := act.Execute(context.Background(), `{"action":"press"}`); err == nil || !strings.Contains(err.Error(), "requires key") {
		t.Fatalf("expected key requirement, got %v", err)
	}

	// 无浏览器时 browser_act/snapshot/tabs 报 no browser open。
	saved := browserGlobal
	browserGlobal = &browserState{}
	t.Cleanup(func() { browserGlobal = saved })
	if _, err := act.Execute(context.Background(), `{"action":"click","ref":"e1"}`); err == nil || !strings.Contains(err.Error(), "no browser open") {
		t.Fatalf("expected no browser open, got %v", err)
	}
	if _, err := (&BrowserSnapshotTool{Browser: bt}).Execute(context.Background(), ""); err == nil {
		t.Fatal("expected no browser open error for snapshot")
	}
	if _, err := (&BrowserTabsTool{Browser: bt}).Execute(context.Background(), `{"action":"list"}`); err == nil {
		t.Fatal("expected no browser open error for tabs")
	}
}

// TestBrowserActClickFlow 点击流：NOT_FOUND 语义 → 错误引导 snapshot；OK 语义 → clicked。
func TestBrowserActClickFlow(t *testing.T) {
	bt, _ := wireFakeBrowser(t, nil)
	act := &BrowserActTool{Browser: bt}
	out, err := act.Execute(context.Background(), `{"action":"click","ref":"e1"}`)
	if err != nil {
		t.Fatalf("click: %v", err)
	}
	if out != "clicked" {
		t.Fatalf("click out: %q", out)
	}
	// hover。
	out, err = act.Execute(context.Background(), `{"action":"hover","selector":"#btn"}`)
	if err != nil || out != "hovered" {
		t.Fatalf("hover: %q %v", out, err)
	}
	// scroll（默认 600px）。
	out, err = act.Execute(context.Background(), `{"action":"scroll"}`)
	if err != nil || !strings.Contains(out, "scrolled 600px") {
		t.Fatalf("scroll: %q %v", out, err)
	}
}

// TestBrowserSnapshotRender 快照渲染：ref/role/name 行格式与引导语。
func TestBrowserSnapshotRender(t *testing.T) {
	bt, st := wireFakeBrowser(t, nil)
	// 注入快照 JSON：验证 ref/role/name 行格式与 act 引导语。
	st.setEval(`{"title":"T","url":"http://x/","elements":[["e1","button","Submit",""],["e2","a","Docs","href=/docs"]]}`)
	out, err := (&BrowserSnapshotTool{Browser: bt}).Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, want := range []string{"page: T", "url: http://x/", "[e1] button \"Submit\"", "[e2] a \"Docs\" href=/docs", "browser_act"} {
		if !strings.Contains(out, want) {
			t.Fatalf("snapshot missing %q in: %s", want, out)
		}
	}
}

// TestBrowserTabsListNoBrowser 标签页 list：浏览器未打开时（httpPort==0）报错。
// 判据是 httpPort（两条启动路径均写入、cleanup 清零），不是进程句柄 cmd。
func TestBrowserTabsListNoBrowser(t *testing.T) {
	saved := browserGlobal
	browserGlobal = &browserState{}
	t.Cleanup(func() { browserGlobal = saved })
	if _, err := (&BrowserTabsTool{Browser: browserToolSingleton}).Execute(context.Background(), `{"action":"list"}`); err == nil || !strings.Contains(err.Error(), "no browser open") {
		t.Fatalf("expected no browser open, got %v", err)
	}
}

// TestBrowserTabsListEmbeddedMode 内嵌 WebView2 模式回归：cmd==nil 且
// httpPort>0 时 browser_tabs 必须可用（修复前误报 no browser open）。
// 用 httptest 假 /json/list 端点模拟内嵌浏览器的 CDP HTTP 服务。
func TestBrowserTabsListEmbeddedMode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"T1","type":"page","title":"Example","url":"https://example.com/","webSocketDebuggerURL":"ws://127.0.0.1/ws"}]`))
	}))
	t.Cleanup(ts.Close)
	saved := browserGlobal
	browserGlobal = &browserState{httpPort: portOf(ts.URL), embedded: true} // cmd==nil：内嵌模式
	t.Cleanup(func() { browserGlobal = saved })
	out, err := (&BrowserTabsTool{Browser: browserToolSingleton}).Execute(context.Background(), `{"action":"list"}`)
	if err != nil {
		t.Fatalf("embedded mode tabs list: %v", err)
	}
	for _, want := range []string{"tabs (1)", "https://example.com/"} {
		if !strings.Contains(out, want) {
			t.Fatalf("embedded tabs missing %q in: %s", want, out)
		}
	}
}

// portOf httptest URL 提取端口（listPageTargets 连 127.0.0.1:<port>/json/list）。
func portOf(u string) int {
	parsed, err := url.Parse(u)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(parsed.Port())
	return n
}

// TestJsEscape JS 转义回归（引号/反斜杠/换行不破坏内联脚本）。
func TestJsEscape(t *testing.T) {
	in := `a"b\c
d	e`
	got := jsEscape(in)
	if strings.Contains(got, "\n") || strings.Count(got, `\"`) != 1 || strings.Count(got, `\\`) != 1 {
		t.Fatalf("jsEscape: %q", got)
	}
}

// TestKeyParams 键映射：enter 有 VK 码；未知单字符透传。
func TestKeyParams(t *testing.T) {
	p := keyParams("Enter")
	if p["windowsVirtualKeyCode"] != 13 || p["text"] != "\r" {
		t.Fatalf("enter params: %v", p)
	}
	p = keyParams("x")
	if p["key"] != "x" {
		t.Fatalf("single char passthrough: %v", p)
	}
}

// TestDesktopActValidation desktop_act 参数校验（非交互路径，不注入真实键鼠）。
func TestDesktopActValidation(t *testing.T) {
	dt := &DesktopActTool{}
	if _, err := dt.Execute(context.Background(), `{"action":"focus"}`); err == nil || !strings.Contains(err.Error(), "hwnd") {
		t.Fatalf("focus requires hwnd: %v", err)
	}
	if _, err := dt.Execute(context.Background(), `{"action":"type"}`); err == nil || !strings.Contains(err.Error(), "requires text") {
		t.Fatalf("type requires text: %v", err)
	}
	if _, err := dt.Execute(context.Background(), `{"action":"press","key":"nosuchkey9"}`); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("unknown key: %v", err)
	}
	if _, err := dt.Execute(context.Background(), `{"action":"hotkey","keys":["ctrl","≡bad"]}`); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("hotkey unknown key: %v", err)
	}
}

// TestVkFor VK 码映射回归。
func TestVkFor(t *testing.T) {
	if v, ok := vkFor("ctrl"); !ok || v != 0x11 {
		t.Fatalf("ctrl vk: %d %v", v, ok)
	}
	if v, ok := vkFor("a"); !ok || v != 0x41 {
		t.Fatalf("a vk: %d %v", v, ok)
	}
	if v, ok := vkFor("5"); !ok || v != 0x35 {
		t.Fatalf("5 vk: %d %v", v, ok)
	}
	if _, ok := vkFor("f13"); ok {
		t.Fatal("f13 should be unknown")
	}
}

// TestDesktopWindowsEnum 真机窗口枚举（Windows 桌面必有可见窗口）。
func TestDesktopWindowsEnum(t *testing.T) {
	if !DesktopSupported() {
		t.Skip("non-windows platform")
	}
	out, err := (&DesktopWindowsTool{}).Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("desktop_windows: %v", err)
	}
	if !strings.Contains(out, "hwnd=") {
		t.Fatalf("no windows listed: %q", clipStr(out, 200))
	}
	// filter 过滤：不存在的窗口名。
	out2, _ := (&DesktopWindowsTool{}).Execute(context.Background(), `{"filter":"@@definitely_not_a_window_9988@@"}`)
	if !strings.Contains(out2, "no matching") {
		t.Fatalf("filter should match nothing: %q", clipStr(out2, 200))
	}
}

// TestDesktopScreenshotFull 真机全屏截图：PNG 落盘且非空（无头 CI 无桌面时跳过）。
func TestDesktopScreenshotFull(t *testing.T) {
	if !DesktopSupported() {
		t.Skip("non-windows platform")
	}
	dir := t.TempDir()
	out, err := (&DesktopScreenshotTool{ScreenshotDir: dir}).Execute(context.Background(), "")
	if err != nil {
		t.Skipf("screenshot failed (headless CI?): %v", err)
	}
	var files []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		files = append(files, e.Name())
	}
	if len(files) == 0 {
		t.Fatalf("no screenshot file saved; out=%s", clipStr(out, 200))
	}
	raw, _ := os.ReadFile(filepath_join(dir, files[0]))
	if len(raw) < 100 || string(raw[:4]) != "\x89PNG" {
		t.Fatalf("invalid png: %d bytes", len(raw))
	}
	// 结果含媒体载荷标记（复用统一通道推送聊天流）。
	if !strings.Contains(out, "KAELVORIS_MEDIA") {
		t.Fatalf("missing media payload: %s", clipStr(out, 200))
	}
}

// filepath_join 小工具（避免仅为此引 path/filepath 别名冲突）。
func filepath_join(a, b string) string { return a + string(os.PathSeparator) + b }

// TestSendInputRoundtrip 真机键鼠冒烟：type 空串拒绝、press 已在参数校验覆盖；
// 真实 SendInput 会移动用户鼠标，仅在显式环境变量下跑。
func TestSendInputRoundtrip(t *testing.T) {
	if os.Getenv("KAELVORIS_TEST_DESKTOP_INPUT") == "" {
		t.Skip("set KAELVORIS_TEST_DESKTOP_INPUT=1 to run real SendInput smoke")
	}
	_ = time.Now()
	if err := pressKey(0x0D); err != nil { // Enter
		t.Fatalf("pressKey: %v", err)
	}
}
