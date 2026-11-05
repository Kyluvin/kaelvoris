// fake_test.go —— 测试辅助：JSON-RPC 响应写出与序列化工具。
package mcp

import (
	"encoding/json"
	"net/http"
)

// mustJSONBytes 序列化失败 panic（测试数据固定可序列化）。
func mustJSONBytes(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// writeJSONRPC application/json 单响应。
func writeJSONRPC(w http.ResponseWriter, id int, result interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: mustJSONBytes(result)})
}

// writeRPCError JSON-RPC 错误响应。
func writeRPCError(w http.ResponseWriter, id, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}
