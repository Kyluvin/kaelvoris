// Package tools 浏览器自动化工具：browser_open / browser_eval / browser_screenshot。
//
// 架构：复用项目既有 gorilla/websocket 依赖（go.mod 已含），走 Chrome DevTools
// Protocol（CDP）over WebSocket——无需 Playwright/Puppeteer/Selenium，零新增依赖。
//
// 工作方式（Codex BrowserUse / In-App-Browser 能力面对齐，2026-10 深度嵌入）：
//  1. browser_open：headless 启动本机 Edge/Chrome（自动探测，浏览器自选端口），
//     经 HTTP /json/new?url= 创建页面 target，并连接页面级 CDP WebSocket；
//     轮询 readyState 等加载后返回页面标题 + 正文文本。
//  2. browser_eval：对已连接页面执行 JS（Runtime.evaluate，returnByValue），
//     返回表达式结果（提取数据的通用原语）。
//  3. browser_screenshot：Page.captureScreenshot → PNG 落盘
//     ~/.kaelvoris/media/screenshots/ → 复用 KAELVORIS_MEDIA 统一媒体载荷推送聊天流
//     （支持 fullPage 整页截图，captureBeyondViewport）。
//  4. browser_act：结构化交互原语 click/type/press/scroll/wait（点击/填表用
//     原生 click + React 兼容 native setter，按键走 CDP Input 真实键盘事件，
//     等待用 awaitPromise 轮询）——对标 Codex 真实输入语义，避免裸 eval JS hack。
//  5. browser_snapshot：可交互元素引用树（role/name/ref），ref 经 data-kv-ref
//     属性回查，可直接喂给 browser_act 的 ref 参数（Codex snapshot→act 工作流）。
//  6. browser_tabs：标签页 list/new/select/close（HTTP /json + Target 生命周期）。
//
// 审批语义：browser_* 系列与 desktop_* 系列同属 command 组，随组审批；
// 参数级审批（approval.Rule）可按 action/selector 正则细分放行或拦截。
//
// 进程生命周期：浏览器进程随任务结束 kill（App.runTask cleanup 链注册 close）。
// 单例：进程与 CDP 连接为 App 级单例（多会话并发罕见；ponytail：进程级锁）。
package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ==== CDP 客户端（最小子集）====

// cdpConn CDP WebSocket 连接封装（gorilla/websocket）。
// 常驻读泵（readLoop）按 id 分发响应：调用侧支持 ctx 取消立即返回、
// 连接断开即时广播错误（触发上层自愈重建），并保证并发调用读安全
// （gorilla 不允许并发 Read；旧实现在 ctx 取消后重入会双 reader 竞争）。
type cdpConn struct {
	ws      *websocket.Conn
	wmu     sync.Mutex // 写串行
	mu      sync.Mutex
	nextID  int
	pending map[int]chan cdpCallResult
	broken  bool // 读泵探明连接已断（进程被杀/CDP 关闭）
}

// cdpCallResult CDP 调用结果（响应分发信封）。
type cdpCallResult struct {
	raw json.RawMessage
	err error
}

// newCdpConn 建立连接封装并启动读泵。
func newCdpConn(ws *websocket.Conn) *cdpConn {
	c := &cdpConn{ws: ws, pending: make(map[int]chan cdpCallResult)}
	go c.readLoop()
	return c
}

// cdpMessage CDP 消息信封。
type cdpMessage struct {
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// cdpCall 发送 CDP 命令并等待响应。
// 超时语义：ctx 截止 / 默认 45s，先到者生效；ctx 取消立即返回（连接不报废，
// 晚到响应按 id 丢弃）。修复点：旧实现 ReadJSON 阻塞读无视 ctx，任务停止后
// 工具调用仍要等满读截止才返回，表现为「卡住」。
func (c *cdpConn) cdpCall(ctx context.Context, method string, params map[string]interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan cdpCallResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := map[string]interface{}{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	c.wmu.Lock()
	err := c.ws.WriteJSON(msg)
	c.wmu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	deadline := 45 * time.Second
	if d, ok := ctx.Deadline(); ok {
		if remaining := time.Until(d); remaining > 0 && remaining < deadline {
			deadline = remaining
		} else if remaining <= 0 {
			deadline = 10 * time.Second
		}
	}
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.raw, r.err
	case <-timer.C:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("cdp %s: timeout after %s", method, deadline)
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// readLoop 常驻读泵：响应按 id 分发给等待者；事件帧忽略；连接断开时向
// 全部等待者广播错误后退出（等待者立即返回，上层自愈清理重建连接）。
func (c *cdpConn) readLoop() {
	for {
		var m cdpMessage
		if err := c.ws.ReadJSON(&m); err != nil {
			c.mu.Lock()
			c.broken = true
			for id, ch := range c.pending {
				ch <- cdpCallResult{err: fmt.Errorf("cdp connection lost: %w", err)}
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		if m.ID == 0 {
			continue // 事件帧。
		}
		c.mu.Lock()
		ch, ok := c.pending[m.ID]
		if ok {
			delete(c.pending, m.ID)
		}
		c.mu.Unlock()
		if !ok {
			continue // 已超时/取消的调用，晚到响应丢弃。
		}
		if m.Error != nil {
			ch <- cdpCallResult{err: fmt.Errorf("cdp: %s (code %d)", m.Error.Message, m.Error.Code)}
		} else {
			ch <- cdpCallResult{raw: m.Result}
		}
	}
}

// browserState 浏览器单例（进程 + HTTP 端口 + CDP 页面连接）。
// 包级单例：多个会话任务共享同一浏览器进程（每进程一个浏览器可接受；
// 若需多会话并发独立浏览器，改为按会话 id 存储——ponytail：进程级单例锁）。
var browserGlobal = &browserState{}

type browserState struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	embedded bool     // 内嵌 WebView2 窗口模式（cmd 恒 nil；cleanup 只断 CDP 不杀进程）
	httpPort int      // --remote-debugging-port 端口（0=自选）
	cdp      *cdpConn // 页面级 CDP WebSocket
	pageWS   string   // 页面级 ws URL（重连用）
	lastURL  string
}

// BrowserTool 浏览器自动化入口（browser_open）。
type BrowserTool struct {
	// BrowserPath 浏览器可执行文件路径（空 = 自动探测 Edge/Chrome）。
	BrowserPath string
	// ScreenshotDir 截图保存目录（空 = ~/.kaelvoris/media/screenshots）。
	ScreenshotDir string
	// EmbeddedEnabled 内嵌浏览器显示开关（2026-10 用户需求）：桌面端热读设置
	//「内嵌浏览器（WebView2）」；nil = 恒 headless（CLI / 服务端宿主不注入）。
	EmbeddedEnabled func() bool
	// EmbeddedLauncher 内嵌浏览器启动器：desktop 层注入，创建/复用可见
	// WebView2 窗口并返回其 CDP HTTP 调试端口，工具层经 /json/list 接管页面
	// 连接（导航仍走 CDP Page.navigate，窗口与工具层完全解耦）。
	// 钩子注入而非直接 import：internal/tools 不得反向依赖 internal/desktop。
	EmbeddedLauncher func() (cdpHTTPPort int, err error)
}

// stateOf 返回包级单例。
func (t *BrowserTool) stateOf() *browserState {
	_ = t // 保持方法签名兼容（BrowserPath/ScreenshotDir 从 t 读取）
	return browserGlobal
}

func (t *BrowserTool) Name() string { return "browser_open" }

func (t *BrowserTool) Description() string {
	return "Open a URL in a headless browser (Edge/Chrome via CDP) and return the page title and readable text. " +
		"Use for browser automation: checking live websites, rendering JS-heavy pages, " +
		"or as the entry point to browser_eval / browser_screenshot. No API keys needed — uses the local browser."
}

func (t *BrowserTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"url": strProp("URL to open (http/https)"),
	}, []string{"url"})
}

func (t *BrowserTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		URL    string `json:"url"`
		WaitMs int    `json:"waitMs"` // readyState complete 后的额外等待（懒加载页）
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	if strings.TrimSpace(p.URL) == "" || !urlIsValid(p.URL) {
		return "", fmt.Errorf("url is required (http/https)")
	}

	st := t.stateOf()
	st.mu.Lock()
	defer st.mu.Unlock()

	// 首次 / 连接脏时重建（融合 2026-10 两侧修复）：
	//   - cdp 活着直接复用（内嵌模式 cmd==nil，不得误判重启——本地判据）；
	//   - cdp 死且 headless 进程仍在：优先 reattach 复用进程（browser_tabs
	//     关页后进程与标签页不白白重建——远程判据）；
	//   - reattach 失败 / 无进程：清脏态走 launch 双路径重建。
	if st.cdp != nil && !st.cdp.dead() {
		// 连接健康：直接复用。
	} else if st.cmd != nil {
		if err := t.reattach(st); err != nil {
			t.cleanupLocked(st)
			if err := t.launch(st); err != nil {
				return "", err
			}
		}
	} else {
		t.cleanupLocked(st)
		if err := t.launch(st); err != nil {
			return "", err
		}
	}
	// 导航到目标 URL。
	if _, err := st.cdp.cdpCall(ctx, "Page.navigate", map[string]interface{}{"url": p.URL}); err != nil {
		t.cleanupLocked(st) // 导航失败多半是连接已死：清脏态，下次调用重建。
		return "", fmt.Errorf("navigate: %w", err)
	}
	st.lastURL = p.URL
	// 等待页面加载（轮询 document.readyState：快页立即返回，慢页最多等 10s，
	// 比固定 1.8s sleep 稳）+ 300ms 渲染沉淀；可选 waitMs 额外等待（懒加载页）。
	st.cdp.waitLoad(ctx, 10*time.Second)
	if p.WaitMs > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(p.WaitMs) * time.Millisecond):
		}
	}

	title, err := st.cdp.evalString(ctx, "document.title")
	if err != nil {
		title = "(unknown)"
	}
	text, _ := st.cdp.evalString(ctx, `document.body ? document.body.innerText.slice(0, 6000) : ""`)
	return fmt.Sprintf("browser opened: %s\npage title: %s\n\n%s", p.URL, title, text), nil
}

// reattach 浏览器进程仍在但页面连接丢失时重连（browser_tabs 关闭当前页后；
// /json/list 取第一个 page 型 target，跳过 background_page/iframe）。
// 仅 headless 路径使用（内嵌模式无 cmd 进程，走窗口复用）。
func (t *BrowserTool) reattach(st *browserState) error {
	if st.httpPort == 0 || st.cmd == nil {
		return fmt.Errorf("no live browser process")
	}
	httpc := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpc.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", st.httpPort))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var targets []struct {
		Type                 string `json:"type"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerURL"`
		URL                  string `json:"url"`
	}
	if err := json.Unmarshal(body, &targets); err != nil {
		return err
	}
	for _, tg := range targets {
		if tg.Type != "page" || tg.WebSocketDebuggerURL == "" {
			continue
		}
		dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
		ws, _, err := dialer.Dial(tg.WebSocketDebuggerURL, nil)
		if err != nil {
			continue
		}
		st.cdp = newCdpConn(ws)
		st.pageWS = tg.WebSocketDebuggerURL
		st.lastURL = tg.URL
		_, _ = st.cdp.cdpCall(context.Background(), "Page.enable", nil)
		return nil
	}
	return fmt.Errorf("no page target to reattach")
}

// launch 启动浏览器并建立页面级 CDP 连接（双路径分流入口，2026-10 内嵌浏览器）：
//   - 内嵌路径：EmbeddedEnabled() 为真且宿主注入 EmbeddedLauncher 时，经
//     WebView2 可见窗口执行（用户可实时观看 Agent 的浏览器操作）；内嵌
//     失败（无 WebView2 Runtime 等）静默回落 headless，不中断任务。
//   - headless 路径：本机 Edge/Chrome 后台无头执行（原有行为，CLI/服务端）。
func (t *BrowserTool) launch(st *browserState) error {
	if t.EmbeddedEnabled != nil && t.EmbeddedLauncher != nil && t.EmbeddedEnabled() {
		if err := t.launchEmbedded(st); err == nil {
			return nil
		}
		// 回落前清 embedded 标志（launchEmbedded 可能已置位半途状态），
		// 避免 cleanupLocked 误走窗口分支漏杀 headless 进程。
		st.embedded = false
		st.httpPort = 0
	}
	return t.launchHeadless(st)
}

// launchEmbedded 接管内嵌 WebView2 窗口：宿主启动器返回 CDP HTTP 端口后，
// 经 /json/list 找页面 target 并连接（无既有页面时 /json/new 兜底创建）。
// 调用方持 st.mu。窗口销毁归 desktop 层（用户 X 关闭 / 应用 Shutdown）。
func (t *BrowserTool) launchEmbedded(st *browserState) error {
	port, err := t.EmbeddedLauncher()
	if err != nil {
		return fmt.Errorf("embedded browser: %w", err)
	}
	st.httpPort = port
	st.embedded = true
	wsURL, err := pickPageTarget(port)
	if err != nil {
		return err
	}
	st.pageWS = wsURL
	// 连接页面级 CDP（5s 握手超时，与 headless 路径一致）。
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, _, err := dialer.Dial(st.pageWS, nil)
	if err != nil {
		return fmt.Errorf("connect embedded page cdp: %w", err)
	}
	// 必须经 newCdpConn 启动读泵（回归守卫见 TestLaunchMustUseCdpPump）。
	st.cdp = newCdpConn(ws)
	_, _ = st.cdp.cdpCall(context.Background(), "Page.enable", nil)
	return nil
}

// pickPageTarget 从 CDP HTTP 端点挑一个页面级 target：优先 /json/list 既有
// 页面（WebView2 启动即带 about:blank），无则 /json/new 创建（PUT 优先 GET 回退）。
func pickPageTarget(port int) (string, error) {
	httpc := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpc.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err == nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		var targets []struct {
			Type                 string `json:"type"`
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		if json.Unmarshal(body, &targets) == nil {
			for _, tg := range targets {
				if tg.Type == "page" && tg.WebSocketDebuggerURL != "" {
					return tg.WebSocketDebuggerURL, nil
				}
			}
		}
	}
	newURL := fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", port, urlEncode("about:blank"))
	req, _ := http.NewRequest(http.MethodPut, newURL, nil)
	resp2, err := httpc.Do(req)
	if err != nil {
		req2, _ := http.NewRequest(http.MethodGet, newURL, nil)
		resp2, err = httpc.Do(req2)
	}
	if err != nil {
		return "", fmt.Errorf("create embedded target: %w", err)
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp2.Body, 1<<20))
	var target struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &target); err != nil || target.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("parse embedded target: %s", clipStr(string(body), 300))
	}
	return target.WebSocketDebuggerURL, nil
}

// launchHeadless 启动 headless 浏览器并建立页面级 CDP 连接。
// 2026-10 全面修复（用户反馈「内置浏览器功能有问题」，Edge 139 真机取证）：
//  1. 启动参数不带位置 URL：--headless=new 模式带命令行 URL（about:blank）
//     直接报 "Multiple targets are not supported in headless mode." 秒退，
//     browser_open 永远等不到 CDP。页面 target 本就经 HTTP /json/new 创建。
//  2. 不再从 stderr 解析 "DevTools listening"：Edge/Chrome 在 Windows 上的
//     launcher 进程会立即退出（真浏览器是它派生的子进程），cmd.Wait() 秒回
//     nil，stderr 生命周期与浏览器实际生命周期脱钩——该解析模式天生不可靠。
//     改用固定空闲端口 + HTTP /json/version 就绪轮询（与内嵌 WebView2 路径
//     embedded_browser_windows.go waitCDPReady 同款语义），对 launcher/子进程
//     拓扑完全无感。
func (t *BrowserTool) launchHeadless(st *browserState) error {
	path, err := t.findBrowser()
	if err != nil {
		return err
	}
	profile := filepath.Join(os.TempDir(), fmt.Sprintf("kaelvoris-browser-profile-%d", time.Now().UnixNano()))
	_ = os.MkdirAll(profile, 0o755)
	port, err := freeLocalPort()
	if err != nil {
		return fmt.Errorf("probe free port: %w", err)
	}
	cmd := exec.Command(path,
		"--headless=new", "--disable-gpu", "--no-sandbox", "--disable-dev-shm-usage",
		"--remote-debugging-port="+strconv.Itoa(port), "--user-data-dir="+profile)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser: %w", err)
	}
	st.cmd = cmd

	// CDP HTTP 端点就绪轮询（浏览器进程冷启动 1~3s 属正常；launcher 秒退
	// 不影响子浏览器进程持续拉起 CDP）。
	if err := waitCDPHTTPReady(port, 20*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("browser CDP endpoint (port %d): %w", port, err)
	}
	st.httpPort = port

	// 经 HTTP /json/new?url=about:blank 创建页面 target（返回页面级 ws URL）。
	// 修复点：旧用 http.DefaultClient（无超时）——浏览器半死时无限阻塞；
	// 5s 超时足够本机回环（实测 <100ms）。
	// 新版 Chrome 用 PUT，旧版 GET 回退。
	httpc := &http.Client{Timeout: 5 * time.Second}
	newURL := fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", port, urlEncode("about:blank"))
	req, _ := http.NewRequest(http.MethodPut, newURL, nil)
	resp, err := httpc.Do(req)
	if err != nil {
		req2, _ := http.NewRequest(http.MethodGet, newURL, nil)
		resp, err = httpc.Do(req2)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("create target: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var target struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &target); err != nil || target.WebSocketDebuggerURL == "" {
		_ = cmd.Process.Kill()
		return fmt.Errorf("parse target: %s", clipStr(string(body), 300))
	}
	st.pageWS = target.WebSocketDebuggerURL

	// 连接页面级 CDP（5s 握手超时；旧 DefaultDialer 无超时，端口异常时无限阻塞）。
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, _, err := dialer.Dial(st.pageWS, nil)
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("connect page cdp: %w", err)
	}
	// 必须经 newCdpConn 启动读泵：裸字面量构造无读泵，cdpCall 等响应永远
	// 超时（45s），browser_open 表现为「卡住没反应」——回归测试锁定此约定。
	st.cdp = newCdpConn(ws)
	// 启用 Page 域（截图/导航需要；失败不致命）。
	_, _ = st.cdp.cdpCall(context.Background(), "Page.enable", nil)
	return nil
}

// freeLocalPort 探测一个空闲回环端口（listen 后即关，存在竞窗但可接受——
// CDP 端点就绪探测会兜底验证）。
func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitCDPHTTPReady 轮询 CDP /json/version 直至 200（浏览器进程冷启动等待；
// 与 desktop 内嵌浏览器 waitCDPReady 同语义，此处独立实现避免跨包依赖）。
func waitCDPHTTPReady(port int, timeout time.Duration) error {
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
	return fmt.Errorf("endpoint not ready within %s", timeout)
}

// cleanupLocked 清理浏览器运行态（调用方持 st.mu；进程可能已被杀，错误忽略）。
func (t *BrowserTool) cleanupLocked(st *browserState) {
	if st.embedded {
		// 内嵌 WebView2 窗口是用户可见 UI，生命周期归 desktop 层（跨任务
		// 存活：任务收尾 CloseBrowser 不销毁窗口；窗口 X 关闭时 desktop 层
		// 回调 CloseBrowser 清态）。此处只断开 CDP 页面连接，下次调用重连。
		if st.cdp != nil {
			_ = st.cdp.ws.Close()
			st.cdp = nil
		}
		st.embedded = false
		st.httpPort = 0
		return
	}
	if st.cdp != nil {
		_ = st.cdp.ws.Close()
		st.cdp = nil
	}
	if st.cmd != nil {
		_ = st.cmd.Process.Kill()
		go func(c *exec.Cmd) { _ = c.Wait() }(st.cmd) // Wait 与 kill 异步收割，避免锁内阻塞
		st.cmd = nil
	}
}

// CloseBrowser 包级收尾：宿主（app.runTask 每轮 / CLI main）在任务结束后
// 调用，结束浏览器进程。修复点：注释声称「随任务结束 kill」但从未接线，
// 导致 headless Edge 孤儿进程残留（每次 browser_open 都新起一个）。
func CloseBrowser() {
	browserToolSingleton.Close()
}

// browserToolSingleton 包级工具实例（CloseBrowser 收尾用；注册处复用）。
var browserToolSingleton = &BrowserTool{}

// Close 结束浏览器（宿主在任务收尾调用，经 CloseBrowser 包级入口）。
func (t *BrowserTool) Close() {
	st := t.stateOf()
	st.mu.Lock()
	defer st.mu.Unlock()
	t.cleanupLocked(st)
}

// findBrowser 自动探测浏览器路径（Edge 优先，然后 Chrome；win/mac/linux）。
func (t *BrowserTool) findBrowser() (string, error) {
	if p := strings.TrimSpace(t.BrowserPath); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	candidates := []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		"/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	if p, err := exec.LookPath("msedge"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("chrome"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("no Edge/Chrome browser found (set BrowserPath in 设置)")
}

// ---- browser_eval ----

// BrowserEvalTool 对已打开页面执行 JS。
type BrowserEvalTool struct {
	Browser *BrowserTool
}

func (t *BrowserEvalTool) Name() string { return "browser_eval" }

func (t *BrowserEvalTool) Description() string {
	return "Execute JavaScript in the browser page opened by browser_open and return the result. " +
		"Use for clicking buttons, filling forms, extracting structured data: " +
		"e.g. document.querySelector('input[name=q]').value='x' — follow with browser_screenshot to verify. " +
		"Requires browser_open to have been called first in this session."
}

func (t *BrowserEvalTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"expression": strProp("JavaScript expression to evaluate (must return a JSON-serializable value)"),
	}, []string{"expression"})
}

func (t *BrowserEvalTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Expression string `json:"expression"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	if strings.TrimSpace(p.Expression) == "" {
		return "", fmt.Errorf("expression is required")
	}
	st := t.Browser.stateOf()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cdp == nil {
		return "", fmt.Errorf("no browser open: call browser_open first")
	}
	out, err := st.cdp.evalJSON(ctx, p.Expression)
	if err != nil {
		return "", err
	}
	if out == "" || out == "null" {
		return "evaluated (result: null/undefined)", nil
	}
	return "result: " + out, nil
}

// ---- browser_screenshot ----

// BrowserScreenshotTool 截图。
type BrowserScreenshotTool struct {
	Browser *BrowserTool
}

func (t *BrowserScreenshotTool) Name() string { return "browser_screenshot" }

func (t *BrowserScreenshotTool) Description() string {
	return "Capture a screenshot of the browser page opened by browser_open, save to disk, and show it in chat. " +
		"Use to verify page state after browser_eval actions (form fill, click)."
}

func (t *BrowserScreenshotTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"fullPage": map[string]interface{}{"type": "boolean", "description": "Capture the entire scrollable page instead of just the viewport (default false)"},
	}, nil)
}

func (t *BrowserScreenshotTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		FullPage bool `json:"fullPage"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	st := t.Browser.stateOf()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cdp == nil {
		return "", fmt.Errorf("no browser open: call browser_open first")
	}
	// fullPage（Codex In-App-Browser 整页截图语义）：captureBeyondViewport 让
	// Chromium 把滚动区域外内容也渲染进截图；先取 scrollHeight 拼进 clip。
	params := map[string]interface{}{"format": "png"}
	if p.FullPage {
		var dim struct {
			Content struct {
				Height int `json:"scrollHeight"`
				Width  int `json:"scrollWidth"`
			} `json:"contentSize"`
		}
		if raw, err := st.cdp.cdpCall(ctx, "Page.getLayoutMetrics", nil); err == nil {
			_ = json.Unmarshal(raw, &dim)
		}
		w, h := dim.Content.Width, dim.Content.Height
		if h <= 0 {
			h = 20000 // 度量失败兜底：超大 clip，Chromium 按实际内容裁剪
		}
		if w <= 0 {
			w = 1280
		}
		params["captureBeyondViewport"] = true
		params["clip"] = map[string]interface{}{"x": 0, "y": 0, "width": float64(w), "height": float64(h), "scale": 1}
	}
	res, err := st.cdp.cdpCall(ctx, "Page.captureScreenshot", params)
	if err != nil {
		return "", fmt.Errorf("screenshot: %w", err)
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(out.Data)
	if err != nil {
		return "", err
	}
	dir := t.Browser.ScreenshotDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			dir = filepath.Join(os.TempDir(), "kaelvoris-media", "screenshots")
		} else {
			dir = filepath.Join(home, ".kaelvoris", "media", "screenshots")
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("browser-%s.png", time.Now().Format("20060102-150405"))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Browser screenshot saved: " + path + "\n")
	b.WriteString("The screenshot has been shown to the user. Briefly confirm; do not embed markdown image links.")
	b.WriteString(BuildMediaPayload([]MediaPayload{{
		Kind:     "image",
		Path:     path,
		DataURL:  MustDataURL(raw),
		FileName: name,
	}}))
	return b.String(), nil
}

// dead 连接是否已断（读泵广播过错误；供上层自愈判断）。
func (c *cdpConn) dead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.broken
}

// ---- CDP 连接辅助方法 ----

// evalString 执行 JS 表达式并返回字符串结果。
// awaitPromise=true：表达式返回 Promise 时等待其落定（fetch().then() 等
// 异步表达式可用）；exceptionDetails 上抛：页面抛错 / Promise reject 时
// 返回明确错误而非空串（旧实现两者都吞——表现为 eval 静默返回空值）。
func (c *cdpConn) evalString(ctx context.Context, expr string) (string, error) {
	return c.evalValue(ctx, expr, func(raw json.RawMessage) (string, error) {
		var out struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", err
		}
		return out.Value, nil
	})
}

// waitLoad 轮询 document.readyState 到 complete（Codex 导航语义：返回即加载完成，
// 而非固定 sleep）。到 deadline 仍未 complete 也返回（SPA 长转页不阻塞调用），
// 由调用方继续取标题/正文。单次表达式同轮判定，消除旧实现「先 eval 再判串」
// 每轮两次 RTT 的窗口（导航切页瞬间旧页值被误判为新页状态）。
func (c *cdpConn) waitLoad(ctx context.Context, deadline time.Duration) {
	start := time.Now()
	for time.Since(start) < deadline {
		select {
		case <-ctx.Done():
			return
		default:
		}
		s, err := c.evalString(ctx, `document.readyState === "complete" ? "OK" : "WAIT"`)
		if err == nil && s == "OK" {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// evalJSON 执行 JS 表达式并返回 JSON 值串（browser_eval 用）。
// awaitPromise + exceptionDetails 语义同 evalString；undefined/null 结果返回空串。
func (c *cdpConn) evalJSON(ctx context.Context, expr string) (string, error) {
	return c.evalValue(ctx, expr, func(raw json.RawMessage) (string, error) {
		var probe struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		_ = json.Unmarshal(raw, &probe)
		if probe.Type == "undefined" || probe.Subtype == "null" {
			return "", nil
		}
		return string(raw), nil
	})
}

// evalValue Runtime.evaluate 公共封装：awaitPromise 等待 Promise 落定（fetch 等
// 异步表达式可用）、exceptionDetails 上抛页面异常（描述截断 300 字符），
// extract 从 result 远程对象提取业务值（形态各异由调用方定义）。
func (c *cdpConn) evalValue(ctx context.Context, expr string, extract func(json.RawMessage) (string, error)) (string, error) {
	res, err := c.cdpCall(ctx, "Runtime.evaluate", map[string]interface{}{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Result    json.RawMessage `json:"result"`
		Exception *struct {
			Text        string `json:"text"`
			Description string `json:"description"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	if out.Exception != nil {
		d := out.Exception.Description
		if d == "" {
			d = out.Exception.Text
		}
		return "", fmt.Errorf("js eval: %s", clipStr(d, 300))
	}
	return extract(out.Result)
}

// ---- 辅助 ----

func urlIsValid(u string) bool {
	parsed, err := url.Parse(u)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func urlParse(u string) (*url.URL, error) {
	return url.Parse(u)
}

func urlEncode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
