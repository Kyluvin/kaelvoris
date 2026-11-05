package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeCdpServer 假 CDP 端点：升级 WebSocket，收到命令回写同 id 响应
// （result.value="pong"），用于回归验证 cdpConn 读泵分发语义。
func fakeCdpServer(t *testing.T) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
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
			_ = ws.WriteJSON(map[string]interface{}{"id": m.ID, "result": map[string]interface{}{"value": "pong"}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCdpConnRoundTrip 读泵按 id 分发：cdpCall 必须在超时内拿到响应，且连续
// 多发不错配。回归背景：launch 曾用裸 &cdpConn{ws} 绕过 newCdpConn，读泵
// 未启动，所有 CDP 调用等满 45s 超时，browser_open 表现为「卡住没反应」。
func TestCdpConnRoundTrip(t *testing.T) {
	srv := fakeCdpServer(t)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	c := newCdpConn(ws)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		raw, err := c.cdpCall(ctx, "Page.enable", nil)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		var out struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.Value != "pong" {
			t.Fatalf("call %d: bad result %s (err %v)", i, raw, err)
		}
	}
}

// TestBrowserOpenE2E 真机端到端：真实启动 headless Edge 打开本地页面，
// 验证修复后 browser_open 秒级返回（修复前读泵未启动，要卡 90s 超时）。
// 无浏览器环境（CI）自动跳过。
func TestBrowserOpenE2E(t *testing.T) {
	bt := &BrowserTool{}
	if _, err := bt.findBrowser(); err != nil {
		t.Skip("no Edge/Chrome found, skip E2E")
	}
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>E2E-ok-8271</title></head><body><p>hello e2e</p></body></html>"))
	}))
	t.Cleanup(htmlSrv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	out, err := bt.Execute(ctx, mustJSON(t, map[string]interface{}{"url": htmlSrv.URL + "/page1"}))
	elapsed := time.Since(start)
	bt.Close()
	if err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	if !strings.Contains(out, "E2E-ok-8271") {
		t.Fatalf("title missing in output: %s", clipStr(out, 300))
	}
	if elapsed > 15*time.Second {
		t.Fatalf("browser_open took %s (>15s, pump likely not running)", elapsed)
	}
}

// TestEvalStringAwaitPromiseAndException eval 语义回归（假 CDP 端点）：
//  1. 表达式返回 Promise 且 awaitPromise=true 时等待落定（fetch 链可用）；
//  2. 页面抛错 / Promise reject → exceptionDetails 上抛为明确错误，
//     不再吞成空串（旧实现表现为 eval 静默返回空值）。
func TestEvalStringAwaitPromiseAndException(t *testing.T) {
	// 可编程响应的假 CDP：按请求序号吐不同 Runtime.evaluate 结果。
	up := websocket.Upgrader{}
	var mu sync.Mutex
	calls := 0
	responses := []string{
		// awaitPromise：result 为已落定 Promise 的值。
		`{"result":{"type":"string","value":"promise-resolved-6161"}}`,
		// 页面异常：exceptionDetails 带描述。
		`{"result":{"type":"object","subtype":"error"},"exceptionDetails":{"text":"Uncaught","exception":{"description":"TypeError: boom-7471"}}}`,
		// Promise reject：同走 exceptionDetails。
		`{"result":{},"exceptionDetails":{"text":"Uncaught (in promise)","exception":{"description":"Error: rejected-8282"}}}`,
	}
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
			if m.Method == "Runtime.evaluate" {
				mu.Lock()
				idx := calls
				calls++
				mu.Unlock()
				var params struct {
					AwaitPromise bool `json:"awaitPromise"`
				}
				_ = json.Unmarshal(m.Params, &params)
				if !params.AwaitPromise {
					_ = ws.WriteJSON(map[string]interface{}{"id": m.ID, "error": map[string]interface{}{"code": -32602, "message": "awaitPromise not set"}})
					continue
				}
				res := responses[idx%len(responses)]
				_ = ws.WriteJSON(map[string]interface{}{"id": m.ID, "result": json.RawMessage(res)})
				continue
			}
			_ = ws.WriteJSON(map[string]interface{}{"id": m.ID, "result": map[string]interface{}{}})
		}
	}))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	c := newCdpConn(ws)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 1) Promise 落定值直接返回。
	s, err := c.evalString(ctx, "Promise.resolve('promise-resolved-6161')")
	if err != nil || s != "promise-resolved-6161" {
		t.Fatalf("awaitPromise: %q %v", s, err)
	}
	// 2) 页面异常上抛（含描述，取 exception.description，空则退 text），非空串。
	if _, err := c.evalString(ctx, "throw new Error('boom-7471')"); err == nil || (!strings.Contains(err.Error(), "boom-7471") && !strings.Contains(err.Error(), "Uncaught")) {
		t.Fatalf("exception should propagate with description, got %v", err)
	}
	// 3) Promise reject 上抛。
	if _, err := c.evalString(ctx, "Promise.reject(new Error('rejected-8282'))"); err == nil || (!strings.Contains(err.Error(), "rejected-8282") && !strings.Contains(err.Error(), "in promise")) {
		t.Fatalf("promise rejection should propagate, got %v", err)
	}
}

// TestEvalJSONUndefinedNull evalJSON 对 undefined / null 结果归一化空串
// （browser_eval 层转「evaluated (result: null/undefined)」提示语）。
func TestEvalJSONUndefinedNull(t *testing.T) {
	up := websocket.Upgrader{}
	var mu sync.Mutex
	calls := 0
	responses := []string{
		`{"result":{"type":"undefined"}}`,
		`{"result":{"type":"object","subtype":"null"}}`,
		`{"result":{"type":"number","value":42,"description":"42"}}`,
	}
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
			idx := calls
			calls++
			mu.Unlock()
			res := responses[idx%len(responses)]
			_ = ws.WriteJSON(map[string]interface{}{"id": m.ID, "result": json.RawMessage(res)})
		}
	}))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	c := newCdpConn(ws)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if s, err := c.evalJSON(ctx, "undefined"); err != nil || s != "" {
		t.Fatalf("undefined should be empty: %q %v", s, err)
	}
	if s, err := c.evalJSON(ctx, "null"); err != nil || s != "" {
		t.Fatalf("null should be empty: %q %v", s, err)
	}
	if s, err := c.evalJSON(ctx, "42"); err != nil || !strings.Contains(s, "42") {
		t.Fatalf("number passthrough: %q %v", s, err)
	}
}

// TestLaunchMustUseCdpPump 源码守卫：&cdpConn{ 字面量只允许出现在
// newCdpConn 构造器内部——launch 等处裸构造无读泵，是 browser_open
// 卡死根因，禁止回归。
func TestLaunchMustUseCdpPump(t *testing.T) {
	data, err := os.ReadFile("browser_tools.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "&cdpConn{"); n != 1 {
		t.Fatalf("browser_tools.go: &cdpConn{ 出现 %d 次（应为 1，仅 newCdpConn 构造器内）；必须经 newCdpConn 启动读泵", n)
	}
}
