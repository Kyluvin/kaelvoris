// Package mcp MCP 工具适配器：把远程 MCP 工具桥接为 tools.Tool 注册到任务
// Registry（模型经 function calling 直接调用，工具名 mcp_<server>_<tool>，
// 对齐 codex 的 McpToolCall 运行时最小语义）。
//
// 传输分流（RegisterAll）：URL 非空 → 远程传输（Streamable HTTP / 旧版
// HTTP+SSE 自动探测，见 http.go）；否则 stdio 子进程（见 mcp.go）。
// 两种传输经统一接口 Conn 适配，工具注册逻辑完全一致。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// osEnviron 当前进程环境（MCP 子进程继承 + 追加配置 Env）。
func osEnviron() []string { return os.Environ() }

// sanitizeToolName 生成 LLM function name：mcp_<server>_<tool>（非法字符
// 替换下划线，防注入 function name；对齐 codex mcp tool 命名简版）。
func sanitizeToolName(server, tool string) string {
	clean := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		return b.String()
	}
	return "mcp_" + clean(server) + "_" + clean(tool)
}

// Conn 单个 MCP 服务器连接的统一接口（stdio 子进程 / 远程 HTTP 双实现同构）。
type Conn interface {
	ListTools(ctx context.Context) ([]ToolInfo, error)
	CallTool(ctx context.Context, tool string, args json.RawMessage) (string, error)
	Close()
}

// 编译期断言：两种实现均满足 Conn。
var (
	_ Conn = (*Client)(nil)       // stdio
	_ Conn = (*remoteClient)(nil) // Streamable HTTP / 旧版 HTTP+SSE
)

// RemoteTool tools.Tool 适配器：调用转发到 MCP 服务器 tools/call。
type RemoteTool struct {
	Server     Conn
	FullName   string          // mcp_<server>_<tool> 规范化全名（LLM function name）
	ServerTool string          // 服务器侧真实工具名
	Desc       string          // 描述
	Schema     json.RawMessage // 服务端 inputSchema 原样透传（nil = 宽松 schema）
}

func (t *RemoteTool) Name() string { return t.FullName }

func (t *RemoteTool) Description() string { return t.Desc }

func (t *RemoteTool) Parameters() map[string]interface{} {
	if len(t.Schema) > 0 {
		var raw map[string]interface{}
		if err := json.Unmarshal(t.Schema, &raw); err == nil && len(raw) > 0 {
			return raw
		}
	}
	// 兜底：宽松对象 schema（服务端未提供 inputSchema 时）。
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
		"required":   []string{},
	}
}

func (t *RemoteTool) Execute(ctx context.Context, args string) (string, error) {
	raw := json.RawMessage(args)
	if args == "" {
		raw = json.RawMessage("{}")
	}
	out, err := t.Server.CallTool(ctx, t.ServerTool, raw)
	if err != nil {
		return "", fmt.Errorf("mcp %s: %w", t.FullName, err)
	}
	return out, nil
}

// RegisterAll 连接 server → 枚举工具 → 转成 RemoteTool 调用 register 注册。
// 返回创建的连接（调用方在任务结束时 Close）与注册的工具数。
// 连接或枚举失败返回错误（调用方跳过该 server 并记录，不影响其余 server）。
// 传输选择：cfg.URL 非空 → 远程（Streamable HTTP，405 自动降级旧版 SSE）；
// 否则 stdio spawn。
func RegisterAll(ctx context.Context, cfg ServerConfig, register func(name string, t *RemoteTool) error) (Conn, int, error) {
	var cli Conn
	var err error
	if strings.TrimSpace(cfg.URL) != "" {
		cli, err = StartRemote(ctx, cfg)
	} else {
		cli, err = Start(ctx, cfg)
	}
	if err != nil {
		return nil, 0, err
	}
	toolsList, err := cli.ListTools(ctx)
	if err != nil {
		cli.Close()
		return cli, 0, fmt.Errorf("mcp server %q tools/list: %w", cfg.Name, err)
	}
	n := 0
	for _, ti := range toolsList {
		name := sanitizeToolName(cfg.Name, ti.Name)
		if err := register(name, &RemoteTool{
			Server:     cli,
			FullName:   name,
			ServerTool: ti.Name,
			Desc:       ti.Description,
			Schema:     ti.InputSchema,
		}); err != nil {
			continue // 重名等注册失败：跳过该工具
		}
		n++
	}
	return cli, n, nil
}
