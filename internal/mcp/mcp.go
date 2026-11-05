// Package mcp MCP (Model Context Protocol) 运行时（codex 对齐补全 ⑦）。
//
// 对齐 codex-rs mcp-client / rmcp 的核心语义（Go 简版，stdio + 远程双传输）：
//   - stdio：spawn server 子进程，JSON-RPC 2.0 over stdin/stdout；
//   - 远程：Streamable HTTP（单端点 POST，JSON/SSE 响应自适应）+ 旧版
//     HTTP+SSE（GET 建流 → endpoint POST 回传，405 自动降级），见 http.go；
//   - initialize 握手 → tools/list 枚举 → tools/call 调用；
//   - 每个启用的 server 生成 mcp_<server>_<tool> 形式的 tools.Tool 注册到
//     任务级 Registry（模型经 function calling 直接调用）；
//   - 协议版本 "2024-11-05"；客户端信息 name=kaelvoris。
//
// 不实现（边界）：resources/prompts/sampling、服务端热重载。
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"kaelvoris/internal/sysproc"
)

// ServerConfig MCP 服务器配置（与 app 层 MCPServer 对齐；stdio 与远程双传输）。
// URL 非空 → 远程传输（Streamable HTTP，405 自动降级旧版 HTTP+SSE，见 http.go）；
// 否则 Command 生效（stdio spawn）。Headers 仅远程传输使用（认证头注入）。
type ServerConfig struct {
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// rpcRequest JSON-RPC 2.0 请求。
type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// rpcResponse JSON-RPC 2.0 响应。
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError JSON-RPC 错误对象。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ToolInfo tools/list 返回的工具条目。
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// lineBuf 读泵行通道缓冲（正常请求-响应 1:1 消费，仅防御异常服务器输出洪水）。
const lineBuf = 64

// Client 单个 stdio MCP 服务器连接（进程生命周期 = 一次任务运行）。
type Client struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string // 常驻读泵输出行（取消感知读取，见 readLoop）
	stderr *capBuf     // 子进程 stderr 尾部（spawn/握手失败诊断用）
	nextID int
	mu     sync.Mutex
}

// Start 启动 stdio server 子进程并完成 initialize 握手。
func Start(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("mcp server %q: empty command", cfg.Name)
	}
	cmd := stdioCommand(cfg)
	if cmd == nil {
		return nil, fmt.Errorf("mcp server %q: empty command", cfg.Name)
	}
	if len(cfg.Env) > 0 {
		cmd.Env = osEnviron()
		for k, v := range cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// stderr 捕获到尾部环形缓冲：spawn/握手失败时把服务端报错带给调用方
	// （此前 io.Discard，「executable file not found」之类失败对用户完全不可见）。
	errBuf := &capBuf{max: 8 * 1024}
	cmd.Stderr = errBuf
	sysproc.Hide(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp server %q spawn: %w%s", cfg.Name, err, stderrHint(errBuf))
	}
	c := &Client{
		name:   cfg.Name,
		cmd:    cmd,
		stdin:  stdin,
		stderr: errBuf,
		lines:  make(chan string, lineBuf),
		nextID: 1,
	}
	// 常驻读泵：阻塞式 Scan 与 ctx 取消解耦（call() 侧 select 可即时感知
	// 取消/超时——此前 scanner.Scan() 阻塞期间取消不唤醒，MCP 工具执行中
	// 点停止完全无效）。
	go c.readLoop(stdout)

	// initialize 握手（对齐 MCP initialize；15s 超时防服务器僵死——npx 冷启动
	// 需解析甚至下载包，5s 常不够；调用方 ctx 更紧时取其剩余额度）。
	hsTimeout := 15 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 && d < hsTimeout {
			hsTimeout = d
		}
	}
	hsCtx, cancel := context.WithTimeout(ctx, hsTimeout)
	defer cancel()
	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.call(hsCtx, "initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "kaelvoris", "version": "1.0"},
	}, &initRes); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp server %q initialize: %w%s", cfg.Name, err, stderrHint(errBuf))
	}
	// initialized 通知（无响应）。
	_, _ = io.WriteString(c.stdin, mustJSON(rpcRequest{
		JSONRPC: "2.0", Method: "notifications/initialized",
	})+"\n")
	return c, nil
}

// ---------- 命令启动（Windows 兼容） ----------

// stdioCommand 构建 stdio server 启动命令：
//   - 整行命令支持：Args 为空且 Command 含空白时按引号感知拆分（前端引导
//     用户把 "npx -y @modelcontextprotocol/server-filesystem" 整行填进命令框）；
//   - Windows 上 npx / node 实为 .cmd / .bat 批处理包装，exec.Command 无法
//     直接 spawn（报 "executable file not found"），须如 shell 工具一样经
//     cmd /C 执行（对齐 shell_tools.go 的 Windows 路径）。
func stdioCommand(cfg ServerConfig) *exec.Cmd {
	var parts []string
	if s := strings.TrimSpace(cfg.Command); s != "" {
		if len(cfg.Args) == 0 && strings.ContainsAny(s, " \t") {
			parts = append(parts, splitCommandLine(s)...)
		} else {
			parts = append(parts, s)
		}
	}
	parts = append(parts, cfg.Args...)
	if len(parts) == 0 {
		return nil
	}
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/C", joinCommandLine(parts))
	}
	return exec.Command(parts[0], parts[1:]...)
}

// splitCommandLine 引号感知的命令行拆分（双引号内空白不拆分）。
func splitCommandLine(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote, hasTok := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			hasTok = true
		case (r == ' ' || r == '\t') && !inQuote:
			if hasTok {
				out = append(out, cur.String())
				cur.Reset()
				hasTok = false
			}
		default:
			cur.WriteRune(r)
			hasTok = true
		}
	}
	if hasTok {
		out = append(out, cur.String())
	}
	return out
}

// joinCommandLine 重组命令行：含空白的片段加引号（已带引号的原样保留）。
func joinCommandLine(parts []string) string {
	out := make([]string, len(parts))
	for i, p := range parts {
		if n := len(p); n > 1 && strings.HasPrefix(p, `"`) && strings.HasSuffix(p, `"`) {
			out[i] = p
		} else if strings.ContainsAny(p, " \t") {
			out[i] = `"` + p + `"`
		} else {
			out[i] = p
		}
	}
	return strings.Join(out, " ")
}

// capBuf 只保留末尾 max 字节的 stderr 捕获器（环形尾部，防服务器日志刷爆内存）。
type capBuf struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (b *capBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	limit := b.max
	if limit <= 0 {
		limit = 8 * 1024
	}
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - limit; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...) // memmove 安全（copy 语义）
	}
	return len(p), nil
}

func (b *capBuf) tail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

// stderrHint 服务端 stderr 尾部摘要（空 = ""，拼接到错误信息末尾）。
func stderrHint(b *capBuf) string {
	if b == nil {
		return ""
	}
	t := b.tail()
	if t == "" {
		return ""
	}
	if len(t) > 600 {
		t = "…(截断)" + t[len(t)-600:]
	}
	return "；stderr: " + t
}

// readLoop 常驻读泵：逐行扫描子进程 stdout 推入 lines 通道。
// goroutine 独立于调用方 ctx——call() 等响应时经 select 即时感知取消；
// 通道关闭（Close 杀进程后 Scan 失败）即退出。缓冲满时丢弃溢出行
// （正常请求-响应 1:1 串行消费，仅防御异常服务器输出洪水）。
func (c *Client) readLoop(stdout io.Reader) {
	// panic 防护（崩溃修复）：读泵是常驻 goroutine，异常 panic 会拖垮整个
	// 进程；拦截后正常走 close(c.lines)，call() 侧收到"closed stream"错误。
	defer func() {
		if r := recover(); r != nil {
			slog.Error("MCP 读泵 panic（已拦截）", "server", c.name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	defer close(c.lines)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		select {
		case c.lines <- line:
		default: // 缓冲满：丢弃（仅防御性路径）
		}
	}
}

// call 发送 JSON-RPC 请求并等待同 ID 响应（MCP stdio 单请求串行；互斥保护）。
// 取消感知：等待响应全程 select ctx.Done，立即返回（读泵负责消费流）。
func (c *Client) call(ctx context.Context, method string, params interface{}, out interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID
	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(c.stdin, string(b)+"\n"); err != nil {
		return fmt.Errorf("mcp write: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-c.lines:
			if !ok {
				return fmt.Errorf("mcp server %q closed stream%s", c.name, stderrHint(c.stderr))
			}
			var resp rpcResponse
			if json.Unmarshal([]byte(line), &resp) != nil || resp.ID != id {
				continue // 通知/其他请求的响应：跳过
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
	}
}

// ListTools 枚举服务器工具（对齐 tools/list）。
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var res struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", map[string]interface{}{}, &res); err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool 调用服务器工具（对齐 tools/call；返回文本内容拼接）。
func (c *Client) CallTool(ctx context.Context, tool string, args json.RawMessage) (string, error) {
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
	if err := c.call(ctx, "tools/call", map[string]interface{}{
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

// Close 终止服务器子进程。
func (c *Client) Close() {
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
}

// mustJSON 序列化失败返回空对象（通知帧无 ID，不会阻塞对端）。
func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
