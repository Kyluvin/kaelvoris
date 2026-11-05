// http.go —— MCP 远程传输客户端（Streamable HTTP + 旧版 HTTP+SSE）。
//
// 对齐 MCP 规范「Streamable HTTP」传输（2025-03-26 引入，向后兼容 2024-11-05
// 旧版 HTTP+SSE）：
//
//   - Streamable HTTP：单端点 URL，JSON-RPC 请求经 HTTP POST 上行；响应可
//     为 application/json（单响应）或 text/event-stream（SSE 帧流，最终帧
//     为该请求的响应）。202 Accepted = 已接受无响应体（通知语义）。
//   - 旧版 HTTP+SSE：GET 端点建立 text/event-stream 长流，先推 endpoint
//     帧（data: <POST 回传 URL>），后续请求 POST 到该 URL；响应/通知经流
//     异步下发。规范规定旧 SSE-only 服务器对 POST 返回 405 —— 客户端据此
//     自动降级协议，用户配置 sse 或 http 均无需区分。
//     严格旧版服务器（官方 SDK 2024-11-05 语义）对 POST 回 202 Accepted，
//     响应帧经 GET SSE 流异步下发——客户端 register-await 等待同 ID 帧；
//     宽松服务器 POST 内联回 JSON 亦兼容（响应路径三分支自适应）。
//     帧（data: <POST 回传 URL>），后续请求 POST 到该 URL；响应/通知经流
//     异步下发。规范规定旧 SSE-only 服务器对 POST 返回 405 —— 客户端据此
//     自动降级协议，用户配置 sse 或 http 均无需区分。
//   - 会话：initialize 响应的 Mcp-Session-Id 头保存后回带；Close 时 DELETE
//     会话（尽力而为，不阻塞调用方）。
//   - 认证：配置 Headers 原样注入每个请求（Authorization: Bearer xxx 等）。
//
// 实现边界（与 stdio Client 一致）：仅 initialize / tools/list / tools/call /
// notifications；无 resources/prompts/sampling；单请求串行（互斥保护）。
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// httpTimeout 单次 HTTP 请求超时（覆盖握手与工具调用；工具长任务由调用方
// ctx 控制取消，此处仅防连接僵死）。
const httpTimeout = 120 * time.Second

// errProbeMethodNotAllowed 内部探测标记：旧版 SSE-only 服务器对 POST 返回
// 405（规范语义）→ StartRemote 据此降级 GET+SSE 握手。
const errProbeMethodNotAllowed = "mcp: post method not allowed (legacy sse server)"

// remoteClient MCP 远程服务器客户端（Streamable HTTP / 旧版 HTTP+SSE 双协议，
// 连接生命周期 = 一次任务运行，与 stdio Client 对齐）。
type remoteClient struct {
	name     string
	url      string            // 端点（Streamable HTTP 唯一端点；legacy 为 SSE 建流地址）
	headers  map[string]string // 用户配置头（认证等）
	legacy   bool              // 旧版 HTTP+SSE 模式
	postURL  string            // legacy：endpoint 帧给出的 POST 回传地址
	sessID   string            // Mcp-Session-Id（initialize 响应头，后续回带）
	http     *http.Client
	longHTTP *http.Client // GET SSE 长流专用（无 client 级超时，超时由 ctx 控制）
	nextID   int
	mu       sync.Mutex
	closed   bool

	// ---- legacy SSE 长流消费（严格旧版服务器把响应经 GET 流异步下发） ----
	endpointCh chan string // endpoint 帧 → connectLegacy（cap 1，主流程未等待时缓存）
	streamMu   sync.Mutex
	streamBody io.Closer                 // SSE 流 body（Close 时关闭以解除后台读阻塞）
	streamDone chan struct{}             // 流结束时关闭（等待者退出信号）
	sseWaiters map[int]chan *rpcResponse // 等待同 ID 响应帧的调用方
	sseEarly   []*rpcResponse            // 早到帧（响应先于等待者注册到达）
}

// StartRemote 连接远程 MCP 服务器并完成 initialize 握手。
// 协议自动探测：先按 Streamable HTTP（POST initialize）；POST 返回 405 时
// 降级旧版 HTTP+SSE（GET 建流 → endpoint 帧 → 经 POST 回传通道握手）。
// transport 参数仅日志语义（sse/http 配置均走同一探测）。
func StartRemote(ctx context.Context, cfg ServerConfig) (*remoteClient, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, fmt.Errorf("mcp server %q: empty url", cfg.Name)
	}
	initParams := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "kaelvoris", "version": "1.0"},
	}
	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
	}

	tryStreamable := func() (*remoteClient, error) {
		c := &remoteClient{
			name:    cfg.Name,
			url:     strings.TrimSpace(cfg.URL),
			headers: cfg.Headers,
			http:    &http.Client{Timeout: httpTimeout},
			nextID:  1,
		}
		if _, err := c.rpc(ctx, "initialize", initParams, &initRes); err != nil {
			c.Close()
			return nil, err
		}
		_, _ = c.notify("notifications/initialized", nil)
		return c, nil
	}
	tryLegacy := func() (*remoteClient, error) {
		c := &remoteClient{
			name:       cfg.Name,
			url:        strings.TrimSpace(cfg.URL),
			headers:    cfg.Headers,
			legacy:     true,
			http:       &http.Client{Timeout: httpTimeout},
			longHTTP:   &http.Client{},
			nextID:     1,
			endpointCh: make(chan string, 1),
			sseWaiters: map[int]chan *rpcResponse{},
		}
		// GET 建流拿 endpoint（POST 回传地址）。
		if err := c.connectLegacy(ctx); err != nil {
			c.Close()
			return nil, err
		}
		if _, err := c.rpc(ctx, "initialize", initParams, &initRes); err != nil {
			c.Close()
			return nil, err
		}
		_, _ = c.notify("notifications/initialized", nil)
		return c, nil
	}

	c, err := tryStreamable()
	if err == nil {
		return c, nil
	}
	if !strings.Contains(err.Error(), errProbeMethodNotAllowed) {
		return nil, fmt.Errorf("mcp server %q initialize: %w", cfg.Name, err)
	}
	// 405 → 旧版 HTTP+SSE 降级。
	c2, err2 := tryLegacy()
	if err2 != nil {
		return nil, fmt.Errorf("mcp server %q initialize (streamable 405 → legacy sse): %w", cfg.Name, err2)
	}
	return c2, nil
}

// connectLegacy 旧版 HTTP+SSE：GET 端点建 text/event-stream 长流，等待
// endpoint 帧（data: <POST URL>）拿回传地址；随后长流交给后台 pump 持续
// 消费——严格旧版服务器把响应帧经该流异步下发，等待者在 sseWaiters 表中
// register-await 同 ID 帧（早期类型测试实现的「读完 endpoint 即 return」
// 会把响应帧锁死在未消费的流里，严格模式下 initialize 永远收不到响应）。
func (c *remoteClient) connectLegacy(ctx context.Context) error {
	resp, err := c.doLong(ctx, http.MethodGet, c.url, nil, "text/event-stream")
	if err != nil {
		return fmt.Errorf("sse connect: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		resp.Body.Close()
		return fmt.Errorf("sse connect: status %d: %s", resp.StatusCode, truncateForErr(b))
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessID = sid
	}

	c.streamMu.Lock()
	if c.streamDone == nil {
		c.streamDone = make(chan struct{})
	}
	done := c.streamDone
	c.streamMu.Unlock()

	// 后台消费 goroutine：读流至 endpoint 帧后注册 streamBody，继续把
	// 后续帧分发到 sseWaiters（响应）/丢弃（通知），流断开时收尾唤醒。
	go func(body io.ReadCloser) {
		defer close(done)
		defer body.Close()
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		var pending []string
		flush := func() *string {
			if len(pending) == 0 {
				return nil
			}
			data := strings.Join(pending, "\n")
			pending = pending[:0]
			return &data
		}
		epPosted := false
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "data:"):
				pending = append(pending, strings.TrimSpace(line[5:]))
			case line == "":
				d := flush()
				if d == nil || strings.TrimSpace(*d) == "" {
					continue
				}
				ep := strings.TrimSpace(*d)
				if abs, err := resolveURL(c.url, ep); err == nil {
					ep = abs
				}
				if !epPosted {
					epPosted = true
					c.streamMu.Lock()
					c.postURL = ep
					c.streamBody = body
					c.streamMu.Unlock()
					select {
					case c.endpointCh <- ep:
					default:
					}
					continue // endpoint 帧已消耗，后续帧按响应分发
				}
				// endpoint 之后：JSON-RPC 帧 → 分发；其它帧（注释等）→ 忽略。
				var rm rpcResponse
				if json.Unmarshal([]byte(ep), &rm) == nil && (rm.ID != 0 || rm.Result != nil || rm.Error != nil) {
					c.deliverFrame(&rm)
				}
			}
		}
		// 流结束（服务器断开/超时）：streamDone 关闭后等待者经 select 退出。
	}(resp.Body)

	// 主流程等待 endpoint 帧（服务端通常立即推；10s 兜底）。
	select {
	case ep := <-c.endpointCh:
		c.postURL = ep
		return nil
	case <-time.After(10 * time.Second):
		c.streamMu.Lock()
		if c.streamBody != nil {
			_ = c.streamBody.Close()
		}
		c.streamMu.Unlock()
		return fmt.Errorf("sse endpoint event timeout (10s)")
	case <-ctx.Done():
		return fmt.Errorf("sse connect: %w", ctx.Err())
	}
}

// deliverFrame 分发一条响应帧给等待者（无等待者时暂存，供稍后 register 消费）。
func (c *remoteClient) deliverFrame(rm *rpcResponse) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if ch, ok := c.sseWaiters[rm.ID]; ok {
		select {
		case ch <- rm:
			delete(c.sseWaiters, rm.ID)
			return
		default:
		}
	}
	// 早到帧：等待者尚未 register（如 initialize 响应先于等待）。保留供
	// register 时立刻消费（仅保留少量，防异常服务器洪水）。
	if len(c.sseEarly) < 8 {
		c.sseEarly = append(c.sseEarly, rm)
	}
}

// registerSSE 注册同 ID 响应等待者（优先消费早到帧）。
func (c *remoteClient) registerSSE(id int) chan *rpcResponse {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	for i, rm := range c.sseEarly {
		if rm.ID == id {
			c.sseEarly = append(c.sseEarly[:i], c.sseEarly[i+1:]...)
			ch := make(chan *rpcResponse, 1)
			ch <- rm
			return ch
		}
	}
	ch := make(chan *rpcResponse, 1)
	c.sseWaiters[id] = ch
	return ch
}

// unregisterSSE 撤销等待者（超时/关闭路径）。
func (c *remoteClient) unregisterSSE(id int) {
	c.streamMu.Lock()
	delete(c.sseWaiters, id)
	c.streamMu.Unlock()
}

// doLong GET SSE 长流专用请求（无 client 级超时；认证/会话头同 do）。
func (c *remoteClient) doLong(ctx context.Context, method, rawURL string, body []byte, accept string) (*http.Response, error) {
	target := strings.TrimSpace(rawURL)
	if target == "" {
		target = c.url
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.headers {
		if k = strings.TrimSpace(k); k != "" {
			req.Header.Set(k, v)
		}
	}
	c.mu.Lock()
	sid := c.sessID
	c.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	return c.longHTTP.Do(req)
}

// do 执行一次 HTTP 请求（认证头 + 会话头 + Accept 语义）。
func (c *remoteClient) do(ctx context.Context, method, rawURL string, body []byte, accept string) (*http.Response, error) {
	target := strings.TrimSpace(rawURL)
	if target == "" {
		target = c.url
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	} else if body != nil {
		// Streamable HTTP 规范：POST 必须同时声明 JSON 与 SSE 两种可接受响应。
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.headers {
		if k = strings.TrimSpace(k); k != "" {
			req.Header.Set(k, v)
		}
	}
	c.mu.Lock()
	sid := c.sessID
	c.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	return c.http.Do(req)
}

// rpc 发送 JSON-RPC 请求并解析响应。返回原始响应字节（测试/调试用）；
// 202 无响应体时返回 (nil, nil)。
func (c *remoteClient) rpc(ctx context.Context, method string, params interface{}, out interface{}) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp server %q: client closed", c.name)
	}
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	postURL := c.postURL
	c.mu.Unlock()
	if postURL == "" {
		postURL = c.url
	}
	resp, err := c.do(ctx, http.MethodPost, postURL, b, "")
	if err != nil {
		return nil, fmt.Errorf("mcp %s request: %w", method, err)
	}
	defer resp.Body.Close()

	// 旧版 SSE-only 服务器对 POST 返回 405 → 上层降级 GET+SSE。
	if resp.StatusCode == http.StatusMethodNotAllowed && !c.legacy {
		return nil, fmt.Errorf("%s", errProbeMethodNotAllowed)
	}
	// 会话 ID 捕获（initialize 响应头；后续请求回带）。
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessID = sid
		c.mu.Unlock()
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		// 202：已接受无响应体。Streamable HTTP 通知语义直接返回；legacy
		// 严格服务器则把响应帧经 GET SSE 流异步下发，register-await。
		if c.legacy {
			res, err := c.awaitLegacyResponse(ctx, method, id)
			if err != nil {
				return nil, err
			}
			if out != nil && len(res.Result) > 0 {
				if err := json.Unmarshal(res.Result, out); err != nil {
					return nil, fmt.Errorf("mcp %s decode: %w", method, err)
				}
			}
			b, _ := json.Marshal(res.Result)
			return b, nil
		}
		return nil, nil // 202：已接受（通知语义），无响应体
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("mcp %s: status %d: %s", method, resp.StatusCode, truncateForErr(b))
	case c.legacy:
		// 宽松旧版服务器 POST 内联 JSON 响应（非 202）：直接解码。
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
		if err != nil {
			return nil, fmt.Errorf("mcp %s read body: %w", method, err)
		}
		if err := decodeRPC(body, method, out); err != nil {
			return nil, err
		}
		return body, nil
	}

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		// SSE 帧响应：读流至该请求 ID 的响应帧。
		res, err := readSSEResponse(resp.Body, id)
		if err != nil {
			return nil, fmt.Errorf("mcp %s sse: %w", method, err)
		}
		if res.Error != nil {
			return nil, fmt.Errorf("mcp %s error %d: %s", method, res.Error.Code, res.Error.Message)
		}
		if out != nil && len(res.Result) > 0 {
			if err := json.Unmarshal(res.Result, out); err != nil {
				return nil, fmt.Errorf("mcp %s decode: %w", method, err)
			}
		}
		b, _ := json.Marshal(res.Result)
		return b, nil
	}
	// application/json 单响应。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("mcp %s read body: %w", method, err)
	}
	if err := decodeRPC(body, method, out); err != nil {
		return nil, err
	}
	return body, nil
}

// awaitLegacyResponse 严格旧版模式下等待经 GET SSE 流下发的同 ID 响应帧。
// 超时预算 60s（工具调用可能较慢；agent 僵死由上层 ctx/任务超时控制）；
// 流断开（streamDone）即时失败，不干等。
func (c *remoteClient) awaitLegacyResponse(ctx context.Context, method string, id int) (*rpcResponse, error) {
	ch := c.registerSSE(id)
	defer c.unregisterSSE(id)
	c.streamMu.Lock()
	done := c.streamDone
	c.streamMu.Unlock()
	if done == nil {
		return nil, fmt.Errorf("mcp %s: legacy sse stream not established", method)
	}
	select {
	case res := <-ch:
		if res.Error != nil {
			return nil, fmt.Errorf("mcp %s error %d: %s", method, res.Error.Code, res.Error.Message)
		}
		return res, nil
	case <-done:
		return nil, fmt.Errorf("mcp %s: legacy sse stream closed while awaiting response", method)
	case <-ctx.Done():
		return nil, fmt.Errorf("mcp %s: %w", method, ctx.Err())
	case <-time.After(60 * time.Second):
		return nil, fmt.Errorf("mcp %s: legacy sse response timeout (60s)", method)
	}
}

// notify 发送 JSON-RPC 通知（无 ID；5s 预算，尽力而为）。
func (c *remoteClient) notify(method string, params interface{}) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	b, err := json.Marshal(req)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	postURL := c.postURL
	c.mu.Unlock()
	resp, err := c.do(ctx, http.MethodPost, postURL, b, "")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
	return true, nil
}

// decodeRPC 解析 application/json 单响应（错误语义与 SSE 帧路径一致）。
func decodeRPC(b []byte, method string, out interface{}) error {
	var resp rpcResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return fmt.Errorf("mcp %s decode: %w (%s)", method, err, truncateForErr(b))
	}
	if resp.Error != nil {
		return fmt.Errorf("mcp %s error %d: %s", method, resp.Error.Code, resp.Error.Message)
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("mcp %s decode: %w", method, err)
		}
	}
	return nil
}

// readSSEResponse 从 SSE 流读取匹配 id 的 JSON-RPC 响应（跳过通知帧与
// 其他 ID 的帧）。多行 data: 帧按规范拼接。
func readSSEResponse(r io.Reader, id int) (*rpcResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var pending []string
	flush := func() *rpcResponse {
		if len(pending) == 0 {
			return nil
		}
		data := strings.Join(pending, "\n")
		pending = pending[:0]
		var resp rpcResponse
		if json.Unmarshal([]byte(data), &resp) != nil {
			return nil
		}
		// 通知帧（无 ID 无 result 无 error）与其余请求的响应：跳过。
		if resp.ID == 0 && resp.Result == nil && resp.Error == nil {
			return nil
		}
		if resp.ID != id {
			return nil
		}
		return &resp
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			pending = append(pending, strings.TrimSpace(line[5:]))
		case line == "":
			if r := flush(); r != nil {
				return r, nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// 流结束时仍滞留一帧未刷出的情况。
	if r := flush(); r != nil {
		return r, nil
	}
	return nil, fmt.Errorf("stream ended without response for id %d", id)
}

// ---------- ListTools / CallTool / Close：与 stdio Client 同签名 ----------

// ListTools 枚举服务器工具（对齐 tools/list）。
func (c *remoteClient) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var res struct {
		Tools []ToolInfo `json:"tools"`
	}
	if _, err := c.rpc(ctx, "tools/list", map[string]interface{}{}, &res); err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool 调用服务器工具（对齐 tools/call；文本内容拼接，IsError → error）。
func (c *remoteClient) CallTool(ctx context.Context, tool string, args json.RawMessage) (string, error) {
	if args == nil {
		args = json.RawMessage("{}")
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if _, err := c.rpc(ctx, "tools/call", map[string]interface{}{
		"name":      tool,
		"arguments": args,
	}, &res); err != nil {
		return "", err
	}
	var parts []string
	for _, c := range res.Content {
		if c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	out := strings.Join(parts, "\n")
	if res.IsError {
		return out, fmt.Errorf("mcp tool %s failed: %s", tool, out)
	}
	return out, nil
}

// Close 关闭客户端并删除远程会话（DELETE，尽力而为不阻塞调用方）；
// legacy SSE 长流同步关闭以解除后台 pump 读阻塞。
func (c *remoteClient) Close() {
	c.mu.Lock()
	wasClosed := c.closed
	c.closed = true
	sess := c.sessID
	c.mu.Unlock()
	// 关闭 GET SSE 长流（后台 pump 阻塞在读上，关 body 即退出）。
	c.streamMu.Lock()
	if c.streamBody != nil {
		_ = c.streamBody.Close()
	}
	c.streamMu.Unlock()
	if wasClosed {
		return
	}
	if sess == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		resp, err := c.do(ctx, http.MethodDelete, c.url, nil, "")
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
	}()
}

// truncateForErr 错误信息中的响应体摘要（防刷屏）。
func truncateForErr(b []byte) string {
	t := strings.TrimSpace(string(b))
	if len(t) > 600 {
		t = t[:600] + "…(截断)"
	}
	return t
}

// resolveURL 相对 endpoint 补全为绝对 URL（基于 SSE 服务器的基准地址）。
func resolveURL(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}
