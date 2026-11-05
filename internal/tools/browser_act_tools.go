// Package tools 浏览器结构化交互工具：browser_act / browser_snapshot / browser_tabs。
//
// 2026-10 深度嵌入（分析开源 openai/codex 后移植）：
//   - Codex 侧 BrowserUse 能力面 = 连接浏览器 + snapshot 引用树 + 真实输入 act +
//     多标签页生命周期 + origin 级管控（access/downloads/uploads/full_cdp_access）；
//     实际执行在其闭源桌面客户端（Chrome 扩展 + In-App-Browser），开源仓库仅协议层。
//   - 本移植取其能力面：snapshot → act 工作流（快照产出稳定 ref，交互按 ref 定位），
//     真实输入语义（CDP Input.dispatch 原生键盘事件，非裸 JS 赋值），多标签页管理
//     （HTTP /json + /json/new + /json/close，无需 browser 级 CDP 连接）。
//   - 管控语义由既有 approval 体系承接：browser_* 属 command 组随组审批，
//     参数级 Rule 可按 action/selector/url 正则细分放行（对标 BrowserUseOriginPolicy）。
//
// ref 约定：browser_snapshot 给每个可交互元素标注 data-kv-ref="eN"（会话内唯一，
// 存 browserState.refSeq 递增），browser_act(ref=...) 经属性选择器回查定位。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// DesktopSupported 当前平台是否支持桌面自动化（宿主注册处探测：Windows = true，
// 非 Windows = false 跳过 desktop_* 注册，模型不可见）。
func DesktopSupported() bool { return desktopSupported() }

// ── browser_act ──────────────────────────────────────────────────────────────

// BrowserActTool 结构化交互原语（click/type/press/scroll/wait/hover）。
type BrowserActTool struct {
	Browser *BrowserTool
}

func (t *BrowserActTool) Name() string { return "browser_act" }

func (t *BrowserActTool) Description() string {
	return "Perform a structured interaction on the page opened by browser_open: " +
		"click (element by ref from browser_snapshot or CSS selector), type text (with replace/clear + Enter), " +
		"press a key (Enter/Tab/Escape/ArrowDown...), scroll (up/down px), hover, or wait (ms/selector/text). " +
		"Prefer browser_act over browser_eval for interactions — it uses real input events and waits. " +
		"Typical flow: browser_snapshot → browser_act(click ref) → browser_screenshot to verify."
}

func (t *BrowserActTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"action": map[string]interface{}{
			"type":        "string",
			"enum":        []string{"click", "type", "press", "scroll", "hover", "wait"},
			"description": "Interaction primitive",
		},
		"ref":       strProp("Element ref from browser_snapshot (data-kv-ref id); preferred over selector"),
		"selector":  strProp("CSS selector fallback when ref is unknown"),
		"text":      strProp("Text for action=type"),
		"key":       strProp("Key for action=press (Enter/Tab/Escape/ArrowDown/PageDown...)"),
		"px":        numProp("Scroll pixels for action=scroll (positive = down, negative = up)"),
		"ms":        numProp("Milliseconds for action=wait"),
		"selector2": strProp("wait until selector exists (action=wait)"),
		"text2":     strProp("wait until body text contains this string (action=wait)"),
		"replace":   map[string]interface{}{"type": "boolean", "description": "action=type: clear existing value first (default true)"},
		"submit":    map[string]interface{}{"type": "boolean", "description": "action=type: press Enter after typing"},
	}, []string{"action"})
}

// actParams browser_act 参数。
type actParams struct {
	Action    string `json:"action"`
	Ref       string `json:"ref"`
	Selector  string `json:"selector"`
	Text      string `json:"text"`
	Key       string `json:"key"`
	Px        int    `json:"px"`
	Ms        int    `json:"ms"`
	Selector2 string `json:"selector2"`
	Text2     string `json:"text2"`
	Replace   *bool  `json:"replace"`
	Submit    bool   `json:"submit"`
}

func (t *BrowserActTool) Execute(ctx context.Context, args string) (string, error) {
	var p actParams
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
	c := st.cdp

	// 定位目标元素（ref 优先 → selector 回退）；wait 动作无需目标。
	var target string
	switch p.Action {
	case "click", "type", "hover":
		if p.Ref != "" {
			target = fmt.Sprintf("[data-kv-ref=%q]", p.Ref)
		} else if p.Selector != "" {
			target = p.Selector
		} else {
			return "", fmt.Errorf("%s requires ref (from browser_snapshot) or selector", p.Action)
		}
	case "press", "scroll", "wait":
	default:
		return "", fmt.Errorf("unknown action %q (click/type/press/scroll/hover/wait)", p.Action)
	}

	switch p.Action {
	case "click":
		// 滚动可见 → 原生 click（React 合成事件全兼容）；focus 跟随。
		js := fmt.Sprintf(`(() => {
			const el = document.querySelector("%s");
			if (!el) return "NOT_FOUND";
			el.scrollIntoView({block: "center"});
			el.click();
			el.focus && el.focus();
			return "OK";
		})()`, jsEscape(target))
		return actResult(c, ctx, js, "clicked")
	case "hover":
		js := fmt.Sprintf(`(() => {
			const el = document.querySelector("%s");
			if (!el) return "NOT_FOUND";
			el.scrollIntoView({block: "center"});
			for (const ev of ["mouseover","mouseenter"]) {
				el.dispatchEvent(new MouseEvent(ev, {bubbles: true}));
			}
			return "OK";
		})()`, jsEscape(target))
		return actResult(c, ctx, js, "hovered")
	case "type":
		if p.Text == "" {
			return "", fmt.Errorf("type requires text")
		}
		// Codex 真实输入语义的 JS 等价：focus → 可选清空 → native setter 赋值
		//（React 受控组件兼容，绕过 value setter 拦截）→ input 事件 → 可选 Enter。
		replace := p.Replace == nil || *p.Replace // 缺省 true
		js := fmt.Sprintf(`(() => {
			const el = document.querySelector("%s");
			if (!el) return "NOT_FOUND";
			el.scrollIntoView({block: "center"});
			el.focus();
			const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype
				: HTMLInputElement.prototype;
			const setter = Object.getOwnPropertyDescriptor(proto, "value").set;
			if (%t) setter.call(el, "");
			setter.call(el, el.value + "%s");
			el.dispatchEvent(new Event("input", {bubbles: true}));
			el.dispatchEvent(new Event("change", {bubbles: true}));
			return "OK";
		})()`, jsEscape(target), replace, jsEscape(p.Text))
		out, err := actResult(c, ctx, js, "typed")
		if err != nil {
			return "", err
		}
		if p.Submit {
			_, _ = c.cdpCall(ctx, "Input.dispatchKeyEvent", keyParams("Enter"))
			out += "\nEnter pressed (submit)"
		}
		return out, nil
	case "press":
		if p.Key == "" {
			return "", fmt.Errorf("press requires key")
		}
		// CDP Input.dispatchKeyEvent 原生键盘事件（页面无需焦点元素也能收）。
		if _, err := c.cdpCall(ctx, "Input.dispatchKeyEvent", keyParams(p.Key)); err != nil {
			return "", fmt.Errorf("press %s: %w", p.Key, err)
		}
		return "pressed " + p.Key, nil
	case "scroll":
		if p.Px == 0 {
			p.Px = 600
		}
		js := fmt.Sprintf(`window.scrollBy(0, %d); "OK"`, p.Px)
		return actResult(c, ctx, js, fmt.Sprintf("scrolled %dpx", p.Px))
	case "wait":
		return waitByCondition(c, ctx, p)
	}
	return "", fmt.Errorf("unreachable")
}

// actResult 执行交互 JS 并归一化输出（NOT_FOUND → 明确错误引导 snapshot）。
func actResult(c *cdpConn, ctx context.Context, js, okMsg string) (string, error) {
	s, err := c.evalString(ctx, js)
	if err != nil {
		return "", err
	}
	switch s {
	case "OK":
		return okMsg, nil
	case "NOT_FOUND":
		return "", fmt.Errorf("element not found: run browser_snapshot first and use a ref, or check the selector")
	default:
		return s, nil
	}
}

// keyParams 构造 Input.dispatchKeyEvent 参数（key/code/windowsKeyCode 映射常用键，
// 未知键单字符透传）。press/submit 键程用。
func keyParams(key string) map[string]interface{} {
	type kd struct {
		key, code string
		vk        int
	}
	m := map[string]kd{
		"enter":      {"Enter", "Enter", 13},
		"tab":        {"Tab", "Tab", 9},
		"escape":     {"Escape", "Escape", 27},
		"backspace":  {"Backspace", "Backspace", 8},
		"delete":     {"Delete", "Delete", 46},
		"space":      {" ", "Space", 32},
		"arrowup":    {"ArrowUp", "ArrowUp", 38},
		"arrowdown":  {"ArrowDown", "ArrowDown", 40},
		"arrowleft":  {"ArrowLeft", "ArrowLeft", 37},
		"arrowright": {"ArrowRight", "ArrowRight", 39},
		"home":       {"Home", "Home", 36},
		"end":        {"End", "End", 35},
		"pageup":     {"PageUp", "PageUp", 33},
		"pagedown":   {"PageDown", "PageDown", 34},
	}
	k, ok := m[strings.ToLower(strings.TrimSpace(key))]
	if !ok {
		k = kd{key: key, code: key, vk: 0} // 单字符透传
	}
	return map[string]interface{}{
		"type": "keyDown", "key": k.key, "code": k.code,
		"windowsVirtualKeyCode": k.vk, "text": keyText(k.key),
	}
}

// keyText 可打印键的 text 字段（Enter → \r；其余空）。
func keyText(key string) string {
	switch key {
	case "Enter":
		return "\r"
	default:
		return ""
	}
}

// waitByCondition wait 动作三形态：ms 定时 / selector 出现 / 文本出现（轮询 150ms）。
func waitByCondition(c *cdpConn, ctx context.Context, p actParams) (string, error) {
	if p.Selector2 != "" {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
			s, err := c.evalString(ctx, fmt.Sprintf(`document.querySelector("%s") ? "OK" : "WAIT"`, jsEscape(p.Selector2)))
			if err == nil && s == "OK" {
				return "selector appeared: " + p.Selector2, nil
			}
			time.Sleep(150 * time.Millisecond)
		}
		return "", fmt.Errorf("selector %q not found within 10s", p.Selector2)
	}
	if p.Text2 != "" {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
			s, err := c.evalString(ctx, fmt.Sprintf(`(document.body && document.body.innerText.includes("%s")) ? "OK" : "WAIT"`, jsEscape(p.Text2)))
			if err == nil && s == "OK" {
				return "text appeared: " + p.Text2, nil
			}
			time.Sleep(150 * time.Millisecond)
		}
		return "", fmt.Errorf("text %q not found within 10s", p.Text2)
	}
	ms := p.Ms
	if ms <= 0 {
		ms = 1000
	}
	if ms > 30000 {
		ms = 30000
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(time.Duration(ms) * time.Millisecond):
	}
	return fmt.Sprintf("waited %dms", ms), nil
}

// jsEscape Go 字符串 → JS 字符串字面量内容（%q 留给外层 fmt，这里转义引号反斜杠换行）。
func jsEscape(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	)
	return r.Replace(s)
}

// ── browser_snapshot ─────────────────────────────────────────────────────────

// BrowserSnapshotTool 可交互元素引用树（Codex snapshot→act 工作流）。
type BrowserSnapshotTool struct {
	Browser *BrowserTool
}

func (t *BrowserSnapshotTool) Name() string { return "browser_snapshot" }

func (t *BrowserSnapshotTool) Description() string {
	return "Take a structured snapshot of the current page: interactive elements (links/buttons/inputs/selects/textareas) " +
		"listed with role, name, and a stable ref id. Pass the ref to browser_act to interact. " +
		"Much cheaper than full screenshots for deciding what to click."
}

func (t *BrowserSnapshotTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"max": numProp("Max elements to list (default 60)"),
	}, nil)
}

func (t *BrowserSnapshotTool) Execute(ctx context.Context, args string) (string, error) {
	max := 60
	if strings.TrimSpace(args) != "" {
		var p struct {
			Max int `json:"max"`
		}
		if err := json.Unmarshal([]byte(args), &p); err == nil && p.Max > 0 {
			max = p.Max
		}
	}
	st := t.Browser.stateOf()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cdp == nil {
		return "", fmt.Errorf("no browser open: call browser_open first")
	}
	// 标注 + 收集一气呵成：可交互元素打 data-kv-ref（复用旧 ref 不重复递增），
	// 返回 [ref, role, name, extra] 数组，Go 侧拼人类可读树。
	js := `(() => {
		const sel = 'a[href], button, input, select, textarea, [role="button"], [role="link"], [role="tab"], [onclick]';
		const els = Array.from(document.querySelectorAll(sel));
		const out = [];
		for (const el of els) {
			if (out.length >= ` + fmt.Sprint(max) + `) break;
			if (!(el.offsetWidth || el.offsetHeight || el.getClientRects().length)) continue; // 隐藏元素跳过
			let ref = el.getAttribute("data-kv-ref");
			if (!ref) {
				ref = "e" + (++window.__kvRefSeq || (window.__kvRefSeq = 1));
				el.setAttribute("data-kv-ref", ref);
			}
			const role = el.tagName.toLowerCase();
			const name = (el.getAttribute("aria-label") || el.innerText || el.value || el.placeholder || el.getAttribute("title") || el.alt || el.name || "").trim().slice(0, 60);
			let extra = "";
			if (role === "input" || role === "textarea") extra = "value=" + JSON.stringify((el.value || "").slice(0, 40));
			if (role === "a") extra = "href=" + (el.getAttribute("href") || "").slice(0, 60);
			out.push([ref, role, name, extra]);
		}
		return JSON.stringify({title: document.title, url: location.href, elements: out});
	})()`
	raw, err := st.cdp.evalString(ctx, js)
	if err != nil {
		return "", err
	}
	var snap struct {
		Title    string     `json:"title"`
		URL      string     `json:"url"`
		Elements [][]string `json:"elements"`
	}
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return "", fmt.Errorf("parse snapshot: %w (raw: %s)", err, clipStr(raw, 200))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "page: %s\nurl: %s\nelements (%d):\n", snap.Title, snap.URL, len(snap.Elements))
	for _, e := range snap.Elements {
		if len(e) < 4 {
			continue
		}
		line := fmt.Sprintf("  [%s] %s %q", e[0], e[1], e[2])
		if e[3] != "" {
			line += " " + e[3]
		}
		b.WriteString(line + "\n")
	}
	if len(snap.Elements) == 0 {
		b.WriteString("  (no interactive elements found)\n")
	}
	b.WriteString("\nPass a ref like [e3] to browser_act (action=click, ref=e3).")
	return b.String(), nil
}

// ── browser_tabs ─────────────────────────────────────────────────────────────

// BrowserTabsTool 标签页生命周期（list/new/select/close）。
type BrowserTabsTool struct {
	Browser *BrowserTool
}

func (t *BrowserTabsTool) Name() string { return "browser_tabs" }

func (t *BrowserTabsTool) Description() string {
	return "Manage browser tabs: list open tabs (index/url/title), action=new to open a URL in a new tab, " +
		"action=select to switch the automation target to a tab by index, action=close to close by index. " +
		"Requires browser_open to have been called first in this session."
}

func (t *BrowserTabsTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"action": map[string]interface{}{
			"type":        "string",
			"enum":        []string{"list", "new", "select", "close"},
			"description": "Tab operation (default list)",
		},
		"url":   strProp("URL for action=new"),
		"index": numProp("Tab index (from list output) for select/close"),
	}, []string{})
}

// cdpTarget /json/list 单条目。
type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerURL"`
}

// listPageTargets 枚举 page 型 target（调试 HTTP 端点，进程须已启动）。
func listPageTargets(port int) ([]cdpTarget, error) {
	httpc := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpc.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var targets []cdpTarget
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, err
	}
	pages := make([]cdpTarget, 0, len(targets))
	for _, tg := range targets {
		if tg.Type == "page" {
			pages = append(pages, tg)
		}
	}
	return pages, nil
}

func (t *BrowserTabsTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Action string `json:"action"`
		URL    string `json:"url"`
		Index  int    `json:"index"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	if p.Action == "" {
		p.Action = "list"
	}
	st := t.Browser.stateOf()
	st.mu.Lock()
	defer st.mu.Unlock()
	// 「浏览器已打开」判据用 httpPort（两条启动路径 headless/内嵌均写入、
	// cleanup 清零），不用 st.cmd——内嵌 WebView2 模式 cmd 恒 nil，旧判据会
	// 误报 no browser open（tabs 四动作只依赖 /json HTTP 端点，与进程句柄无关）。
	if st.httpPort == 0 {
		return "", fmt.Errorf("no browser open: call browser_open first")
	}

	switch p.Action {
	case "list":
		pages, err := listPageTargets(st.httpPort)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("tabs (%d):\n", len(pages)))
		for i, tg := range pages {
			fmt.Fprintf(&b, "  %d: %s %q\n", i, tg.URL, clipStr(tg.Title, 60))
		}
		return b.String(), nil
	case "new":
		if !urlIsValid(p.URL) {
			return "", fmt.Errorf("url is required for action=new (http/https)")
		}
		httpc := &http.Client{Timeout: 5 * time.Second}
		req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", st.httpPort, urlEncode(p.URL)), nil)
		resp, err := httpc.Do(req)
		if err != nil {
			// 旧版 Chrome 仅支持 GET /json/new。
			req2, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", st.httpPort, urlEncode(p.URL)), nil)
			resp, err = httpc.Do(req2)
		}
		if err != nil {
			return "", fmt.Errorf("create tab: %w", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var tg struct {
			WebSocketDebuggerURL string `json:"webSocketDebuggerURL"`
		}
		_ = json.Unmarshal(body, &tg)
		return fmt.Sprintf("tab opened: %s\n(use browser_tabs action=select to switch to it)", p.URL), nil
	case "select":
		pages, err := listPageTargets(st.httpPort)
		if err != nil {
			return "", err
		}
		if p.Index < 0 || p.Index >= len(pages) {
			return "", fmt.Errorf("index %d out of range (0-%d)", p.Index, len(pages)-1)
		}
		tg := pages[p.Index]
		if tg.WebSocketDebuggerURL == "" {
			return "", fmt.Errorf("tab %d has no debugger url", p.Index)
		}
		// 断旧连接，连新页面级 CDP。
		if st.cdp != nil {
			_ = st.cdp.ws.Close()
		}
		dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
		ws, _, err := dialer.Dial(tg.WebSocketDebuggerURL, nil)
		if err != nil {
			return "", fmt.Errorf("connect tab cdp: %w", err)
		}
		st.cdp = newCdpConn(ws)
		st.pageWS = tg.WebSocketDebuggerURL
		st.lastURL = tg.URL
		_, _ = st.cdp.cdpCall(context.Background(), "Page.enable", nil)
		return fmt.Sprintf("switched to tab %d: %s", p.Index, tg.URL), nil
	case "close":
		pages, err := listPageTargets(st.httpPort)
		if err != nil {
			return "", err
		}
		if p.Index < 0 || p.Index >= len(pages) {
			return "", fmt.Errorf("index %d out of range (0-%d)", p.Index, len(pages)-1)
		}
		httpc := &http.Client{Timeout: 5 * time.Second}
		if _, err := httpc.Get(fmt.Sprintf("http://127.0.0.1:%d/json/close/%s", st.httpPort, pages[p.Index].ID)); err != nil {
			return "", fmt.Errorf("close tab: %w", err)
		}
		// 关的是当前连接的页：断连接（下次 browser_open/eval 触发 reattach 自愈）。
		if st.cdp != nil && pages[p.Index].WebSocketDebuggerURL == st.pageWS {
			_ = st.cdp.ws.Close()
			st.cdp = nil
		}
		return fmt.Sprintf("tab %d closed", p.Index), nil
	}
	return "", fmt.Errorf("unknown action %q (list/new/select/close)", p.Action)
}
