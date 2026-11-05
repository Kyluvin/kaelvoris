// MCP stdio JSON-RPC 服务器框架。
//
// 协议对齐 MCP 2024-11-05（最小可用子集）：
//   - initialize / notifications/initialized（握手）；
//   - tools/list（枚举工具与 JSON Schema）；
//   - tools/call（执行工具，返回 content 文本 JSON）；
//   - ping（存活探测）。
//
// stdin 是按行分隔的 JSON（JSON-RPC 2.0），stdout 只输出响应行；
// 任何诊断日志必须走 stderr（GOFFICE_DEBUG=1 时打开）。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// mcpVersion 协议版本（对齐 kaelvoris internal/mcp 客户端声明的 2024-11-05）。
const mcpVersion = "2024-11-05"

// Server MCP 服务器（单连接、顺序处理）。
type Server struct {
	mu     sync.Mutex
	tools  map[string]*Tool
	closed bool
}

// NewServer 创建服务器并注册全部领域工具。
func NewServer() *Server {
	s := &Server{tools: map[string]*Tool{}}
	registerTools(s)
	return s
}

// rpcRequest / rpcResponse / rpcError MCP 线格式（JSON-RPC 2.0）。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcErrorf 构造 JSON-RPC 错误对象。
func rpcErrorf(code int, format string, args ...interface{}) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Serve 读取 stdin 并按行分发请求，直到 EOF（客户端关闭进程）。
func (s *Server) Serve() error {
	sc := bufio.NewScanner(os.Stdin)
	// MCP 工具结果可能是大 JSON；提高行缓冲上限。
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// 请求响应后立即 flush（客户端按行读）。
		resp := s.handleRaw(line)
		if resp == nil {
			continue // notification（无响应）
		}
		b, err := json.Marshal(resp)
		if err != nil {
			b, _ = json.Marshal(rpcResponse{
				JSONRPC: "2.0",
				Error:   rpcErrorf(-32603, "marshal response: %v", err),
			})
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// handleRaw 解析并处理一行请求；notification 返回 nil。
func (s *Server) handleRaw(line string) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return &rpcResponse{JSONRPC: "2.0", Error: rpcErrorf(-32700, "parse error: %v", err)}
	}
	// notifications/initialized 等无 id 通知：处理但不响应。
	if len(req.ID) == 0 {
		s.handleNotification(req.Method)
		return nil
	}
	resp := s.dispatch(req)
	resp.ID = req.ID
	return resp
}

// handleNotification 处理无需响应的通知（当前全部忽略）。
func (s *Server) handleNotification(method string) {
	// 预留：notifications/initialized 之后可做初始化收尾（当前无状态）。
}

// dispatch 按方法分发请求。
func (s *Server) dispatch(req rpcRequest) *rpcResponse {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion,omitempty"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo,omitempty"`
		}
		_ = json.Unmarshal(req.Params, &params)
		return &rpcResponse{JSONRPC: "2.0", Result: map[string]interface{}{
			"protocolVersion": mcpVersion,
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{"listChanged": false},
			},
			"serverInfo": map[string]interface{}{"name": "goffice", "version": "0.1.0"},
		}}
	case "tools/list":
		return s.listTools()
	case "tools/call":
		return s.callTool(req.Params)
	case "ping":
		return &rpcResponse{JSONRPC: "2.0", Result: map[string]interface{}{}}
	default:
		return &rpcResponse{JSONRPC: "2.0", Error: rpcErrorf(-32601, "method not found: %s", req.Method)}
	}
}

// listTools 返回全部工具 schema。
func (s *Server) listTools() *rpcResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.tools))
	for n := range s.tools {
		names = append(names, n)
	}
	sortStrings(names)
	out := make([]map[string]interface{}, 0, len(names))
	for _, n := range names {
		t := s.tools[n]
		out = append(out, map[string]interface{}{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.Parameters,
		})
	}
	return &rpcResponse{JSONRPC: "2.0", Result: map[string]interface{}{"tools": out}}
}

// callTool 执行工具调用。
func (s *Server) callTool(raw json.RawMessage) *rpcResponse {
	var req struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return &rpcResponse{JSONRPC: "2.0", Error: rpcErrorf(-32602, "invalid params: %v", err)}
	}
	s.mu.Lock()
	t, ok := s.tools[req.Name]
	s.mu.Unlock()
	if !ok || t == nil {
		return &rpcResponse{JSONRPC: "2.0", Error: rpcErrorf(-32602, "unknown tool: %s", req.Name)}
	}
	result, err := t.Execute(req.Arguments)
	if err != nil {
		// 领域错误以稳定 code 进文本（模型无需解析自然语言即可路由恢复路径）。
		return &rpcResponse{JSONRPC: "2.0", Result: map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": fmt.Sprintf("Error [%s]: %s", errCode(err), err.Error())},
			},
			"isError": true,
		}}
	}
	return &rpcResponse{JSONRPC: "2.0", Result: map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": result},
		},
	}}
}

// sortStrings 就地升序排序（不引入额外依赖，保持零第三方通用库）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
