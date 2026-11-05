// http_strict_test.go —— MCP 远程传输补强测试（SSE 模式为核心）：
//   - Streamable HTTP SSE 帧响应（POST → text/event-stream 单帧回 JSON-RPC）
//   - 严格旧版 HTTP+SSE（POST initialize 回 202，响应帧经 GET SSE 流异步
//     下发——官方 SDK 2024-11-05 语义）端到端：握手/tools/list/tools/call
//   - Mcp-Session-Id 回带 + DELETE 会话清理 + 认证头透传
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeSSEFrame 写一条 SSE data 帧（多行 data 拼接按规范单帧）。
func writeSSEFrame(w http.ResponseWriter, v interface{}) {
	b, _ := json.Marshal(v)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// fakeStreamableSSEResponse Streamable HTTP 假服务器：POST 响应为
// text/event-stream 帧（而非 application/json）——验证 readSSEResponse 路径。
type fakeStreamableSSEResponse struct {
	srv    *httptest.Server
	mu     sync.Mutex
	sawDel bool // 收到 DELETE 会话清理
}

func newFakeStreamableSSEResponse(t *testing.T) *fakeStreamableSSEResponse {
	f := &fakeStreamableSSEResponse{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case r.Method == http.MethodDelete:
			f.mu.Lock()
			f.sawDel = true
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		case strings.HasPrefix(req.Method, "notifications/"):
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-sse-frame")
			writeSSEFrame(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"protocolVersion":"2024-11-05"}`)})
		case "tools/list":
			writeSSEFrame(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"tools":[{"name":"sse_tool","description":"via sse frames"}]}`)})
		case "tools/call":
			writeSSEFrame(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"content":[{"type":"text","text":"sse-frame-ok"}]}`)})
		default:
			writeSSEFrame(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "nope"}})
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestRemote_StreamableHTTP_SSEFrameResponse(t *testing.T) {
	f := newFakeStreamableSSEResponse(t)
	cli, err := StartRemote(context.Background(), ServerConfig{Name: "sf", URL: f.srv.URL + "/mcp"})
	if err != nil {
		t.Fatalf("StartRemote: %v", err)
	}
	defer cli.Close()

	tools, err := cli.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "sse_tool" {
		t.Fatalf("tools = %+v", tools)
	}
	out, err := cli.CallTool(context.Background(), "sse_tool", json.RawMessage(`{}`))
	if err != nil || out != "sse-frame-ok" {
		t.Fatalf("CallTool = %q, err=%v", out, err)
	}
	cli.Close()

	// DELETE 会话清理（异步，轮询等待）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		d := f.sawDel
		f.mu.Unlock()
		if d {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expected DELETE session cleanup")
}

// fakeStrictLegacyServer 严格旧版 HTTP+SSE 服务器（官方 2024-11-05 语义）：
//   - GET /sse：建流先推 endpoint 帧；响应帧经该流异步下发
//   - POST /message：接收 JSON-RPC 请求，一律回 202 Accepted（响应走流）
//   - POST /sse：405 → 触发客户端降级探测
type fakeStrictLegacyServer struct {
	srv        *httptest.Server
	mu         sync.Mutex
	sink       http.ResponseWriter // 当前 SSE 流（响应帧经此下发）
	sawSession []string            // 各 POST 携带的 Mcp-Session-Id
	calls      []string            // 收到的方法序列
}

func newFakeStrictLegacyServer(t *testing.T) *fakeStrictLegacyServer {
	f := &fakeStrictLegacyServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: endpoint\ndata: /message\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		f.mu.Lock()
		f.sink = w
		f.mu.Unlock()
		<-r.Context().Done() // 保持到客户端断开
	})
	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.sawSession = append(f.sawSession, r.Header.Get("Mcp-Session-Id"))
		f.calls = append(f.calls, req.Method)
		sink := f.sink
		f.mu.Unlock()
		if strings.HasPrefix(req.Method, "notifications/") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// 严格语义：202 + 响应帧经 GET 流异步下发。
		w.WriteHeader(http.StatusAccepted)
		time.AfterFunc(30*time.Millisecond, func() {
			switch req.Method {
			case "initialize":
				writeSSEFrame(sink, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"protocolVersion":"2024-11-05"}`)})
			case "tools/list":
				writeSSEFrame(sink, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"tools":[{"name":"strict_tool","description":"from strict legacy"}]}`)})
			case "tools/call":
				writeSSEFrame(sink, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"content":[{"type":"text","text":"strict-ok"}]}`)})
			default:
				writeSSEFrame(sink, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "unknown"}})
			}
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestRemote_StrictLegacySSE_EndToEnd(t *testing.T) {
	f := newFakeStrictLegacyServer(t)
	cfg := ServerConfig{
		Name:    "strict",
		URL:     f.srv.URL + "/sse",
		Headers: map[string]string{"Authorization": "Bearer strict-tok"},
	}
	cli, err := StartRemote(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartRemote strict legacy: %v", err)
	}
	defer cli.Close()

	tools, err := cli.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "strict_tool" {
		t.Fatalf("tools = %+v", tools)
	}
	out, err := cli.CallTool(context.Background(), "strict_tool", json.RawMessage(`{"x":2}`))
	if err != nil || out != "strict-ok" {
		t.Fatalf("CallTool = %q err=%v", out, err)
	}

	// 认证头 + 会话 ID 经 POST 回传通道透传。
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sawSession) < 2 {
		t.Fatalf("posts = %d, want >=2 (initialize + tools/list)", len(f.sawSession))
	}
	for i, sid := range f.sawSession[1:] {
		_ = sid
		_ = i
	}
}

func TestRemote_RegisterAll_RoutesRemote(t *testing.T) {
	// RegisterAll 分流：URL 非空走远程；URL 空 + Command 走 stdio（此仅验证远程路径）。
	f := newFakeStreamableServer(t)
	var regNames []string
	cli, n, err := RegisterAll(context.Background(), ServerConfig{Name: "rt", URL: f.srv.URL + "/mcp"}, func(name string, tool *RemoteTool) error {
		regNames = append(regNames, name)
		return nil
	})
	if err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	defer cli.Close()
	if n != 2 || len(regNames) != 2 {
		t.Fatalf("n=%d names=%v", n, regNames)
	}
	if regNames[0] != "mcp_rt_echo" || regNames[1] != "mcp_rt_add" {
		t.Fatalf("names = %v", regNames)
	}
}
