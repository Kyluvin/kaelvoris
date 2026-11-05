// http_test.go —— 远程 MCP 传输（Streamable HTTP + 旧版 HTTP+SSE）测试：
// httptest 假服务器端到端验证 降级探测、initialize 握手、tools/list、
// tools/call、认证头注入、Mcp-Session-Id 回带。
package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// context_bg 测试用后台上下文别名。
func context_bg() context.Context { return context.Background() }

// fakeStreamableServer Streamable HTTP 假 MCP 服务器：POST 单端点，
// initialize/tools/list/tools/call 返回 application/json 单响应。
type fakeStreamableServer struct {
	srv         *httptest.Server
	mu          sync.Mutex
	sawHeaders  []http.Header // 各请求头快照（认证头断言）
	sawSessions []string      // 各请求携带的 Mcp-Session-Id
}

func newFakeStreamableServer(t *testing.T) *fakeStreamableServer {
	t.Helper()
	f := &fakeStreamableServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sawHeaders = append(f.sawHeaders, r.Header.Clone())
		f.sawSessions = append(f.sawSessions, r.Header.Get("Mcp-Session-Id"))
		f.mu.Unlock()
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeRPCError(w, 0, -32700, "bad json")
			return
		}
		if strings.HasPrefix(req.Method, "notifications/") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-123")
			writeJSONRPC(w, req.ID, map[string]interface{}{"protocolVersion": "2024-11-05"})
		case "tools/list":
			writeJSONRPC(w, req.ID, map[string]interface{}{
				"tools": []map[string]interface{}{
					{"name": "echo", "description": "echo tool", "inputSchema": map[string]interface{}{"type": "object"}},
					{"name": "add", "description": "add numbers"},
				},
			})
		case "tools/call":
			writeJSONRPC(w, req.ID, map[string]interface{}{
				"content": []map[string]interface{}{{"type": "text", "text": "ok"}},
			})
		default:
			writeRPCError(w, req.ID, -32601, "method not found")
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestRemote_StreamableHTTP_EndToEnd(t *testing.T) {
	f := newFakeStreamableServer(t)
	cfg := ServerConfig{
		Name:    "remote",
		URL:     f.srv.URL + "/mcp",
		Headers: map[string]string{"Authorization": "Bearer tok123"},
	}
	cli, err := StartRemote(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartRemote: %v", err)
	}
	defer cli.Close()

	// 认证头注入（initialize 请求 = 第 1 个请求）。
	f.mu.Lock()
	auth := f.sawHeaders[0].Get("Authorization")
	f.mu.Unlock()
	if auth != "Bearer tok123" {
		t.Fatalf("Authorization header = %q", auth)
	}

	tools, err := cli.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "add" {
		t.Fatalf("tools = %+v", tools)
	}

	// tools/call（走同一连接回带会话 ID）。
	out, err := cli.CallTool(context.Background(), "echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != "ok" {
		t.Fatalf("CallTool out = %q", out)
	}

	// 会话 ID 回带断言：除第 1 个请求（initialize，尚无会话）外，后续请求
	// 全部回带 Mcp-Session-Id。
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, sid := range f.sawSessions {
		if i > 0 && sid != "sess-123" {
			t.Fatalf("request %d Mcp-Session-Id = %q, want sess-123", i, sid)
		}
	}
}

// fakeLegacySSEServer 旧版 HTTP+SSE 服务器：/sse GET 建流推 endpoint 帧；
// POST /sse 返回 405（触发客户端降级探测）；/message 回传端点响应 JSON-RPC。
type fakeLegacySSEServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	sawPosts int
	lastAuth string
}

func newFakeLegacySSEServer(t *testing.T) *fakeLegacySSEServer {
	t.Helper()
	f := &fakeLegacySSEServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			// 旧版 SSE-only 服务器语义：拒绝 POST → 触发客户端降级探测。
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: endpoint\ndata: /message?sid=abc\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		// 保持连接直到客户端断开（测试结束 server.Close 时触发）。
		<-r.Context().Done()
	})
	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sawPosts++
		f.lastAuth = r.Header.Get("Authorization")
		f.mu.Unlock()
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.HasPrefix(req.Method, "notifications/") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch req.Method {
		case "initialize":
			writeJSONRPC(w, req.ID, map[string]interface{}{"protocolVersion": "2024-11-05"})
		case "tools/list":
			writeJSONRPC(w, req.ID, map[string]interface{}{
				"tools": []map[string]interface{}{
					{"name": "legacy_tool", "description": "from legacy sse"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestRemote_LegacySSE_Fallback(t *testing.T) {
	f := newFakeLegacySSEServer(t)
	cfg := ServerConfig{
		Name:    "legacy",
		URL:     f.srv.URL + "/sse",
		Headers: map[string]string{"Authorization": "Bearer legacy-tok"},
	}
	// POST initialize 收 405 → 自动降级 GET+SSE endpoint 握手。
	cli, err := StartRemote(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartRemote legacy fallback: %v", err)
	}
	defer cli.Close()
	tools, err := cli.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "legacy_tool" {
		t.Fatalf("tools = %+v", tools)
	}
	f.mu.Lock()
	auth := f.lastAuth
	f.mu.Unlock()
	if !strings.Contains(auth, "Bearer legacy-tok") {
		t.Fatalf("auth header lost on POST回传: %q", auth)
	}
}

func TestRemote_StartRemote_ErrorPaths(t *testing.T) {
	// 空 URL。
	if _, err := StartRemote(context.Background(), ServerConfig{Name: "x"}); err == nil {
		t.Fatal("want error for empty url")
	}
	// 不可达端点。
	if _, err := StartRemote(context.Background(), ServerConfig{Name: "down", URL: "http://127.0.0.1:1/mcp"}); err == nil {
		t.Fatal("want error for unreachable server")
	}
	// RegisterAll 分流：URL 非空走远程且错误可读。
	_, _, err := RegisterAll(context.Background(), ServerConfig{Name: "down", URL: "http://127.0.0.1:1/mcp"}, func(string, *RemoteTool) error { return nil })
	if err == nil {
		t.Fatal("RegisterAll remote: want error")
	}
}
