// conn.go ACP agent 连接：spawn 子进程 + JSON-RPC 双向通道。
//
// 与 internal/mcp 的单请求串行模型不同，ACP agent 会主动发通知（session/update
// 流式增量）与反向请求（fs 读写/权限审批），因此：
//   - 读泵逐行解析后统一进 lines 通道，由常驻消费 goroutine 分发（dispatch）：
//     响应帧 → pending 表唤醒等待者；请求帧 → 异步 handler 回帧；通知 → updateFn。
//   - call() 串行化出站请求（mutex），等待同 ID 响应，全程取消感知。
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"kaelvoris/internal/sysproc"
)

// lineBuf 读泵通道缓冲（session/update 洪水缓冲；满载丢弃仅防御异常 agent）。
const lineBuf = 512

// Client 单个 ACP agent 连接（进程生命周期 = 一次任务轮次，对齐 MCP 语义）。
type Client struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *capBuf
	nextID int64
	mu     sync.Mutex // 保护 stdin 写与 nextID（出站串行）

	pendingMu sync.Mutex
	pending   map[int64]chan *rpcResponse

	// updateFn session/update 回调（Prompt 前经 SetUpdateHandler 注册；nil = 丢弃）。
	// 由消费 goroutine 串行调用（单线程语义，消费方无需加锁）。
	updateFn UpdateCallback
	// hooks 反向请求处理器（fs 读写 / 权限审批）。
	hooks FsHooks

	sessionID string
	// AgentInfo 握手获得的 agent 元信息（TestAgent 面板展示）。
	AgentInfo *AgentInfo
	// ProtocolVersion agent 侧协议版本。
	ProtocolVersion int

	lines chan rpcMessage

	// doneCh 消费循环退出信号：Close 后 stdout 结束 → readLoop 关闭 lines →
	// consumeLoop 退出 → close(doneCh)；call 侧 select 即时感知 agent 退出。
	doneCh chan struct{}
	// closeOnce Close 幂等（ctx 取消与显式 Close 并发时可重复调用不双杀）。
	closeOnce sync.Once
}

// UpdateCallback session/update 通知回调（update 为解析后载荷，raw 为原始参数）。
type UpdateCallback func(update SessionUpdate, raw []byte)

// FsHooks 反向请求处理器（agent 请求 client 读写文件/审批时回调；nil = 该能力拒绝）。
type FsHooks struct {
	// ReadTextFile 读取工作区文件（line/limit 从 1 计，0 = 不限）。
	ReadTextFile func(path string, line, limit int) (string, error)
	// WriteTextFile 写工作区文件。
	WriteTextFile func(path, content string) error
	// RequestPermission 权限审批：返回选中 optionId（reject 类 = 拒绝动作）。
	// 阻塞等待用户应答由实现方负责（桥接 run.askCh）。
	RequestPermission func(sessionID string, p *PermissionRequestPayload) (string, error)
}

// Start 启动 agent 子进程并完成 initialize 握手。
// cwd 为任务工作区（子进程 Dir + 后续 session/new 的 cwd）。
// 按 cfg.Protocol 分流："codex" 走 app-server 适配器（codexbridge.go），默认 ACP。
func Start(ctx context.Context, cfg AgentConfig, cwd string, hooks FsHooks) (*Client, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("acp agent %q: 启动命令为空", cfg.Name)
	}
	cmd := agentCommand(cfg)
	if cmd == nil {
		return nil, fmt.Errorf("acp agent %q: 启动命令为空", cfg.Name)
	}
	if wd := firstNonEmpty(cwd, cfg.Cwd); wd != "" {
		if fi, err := os.Stat(wd); err == nil && fi.IsDir() {
			cmd.Dir = wd
		}
	}
	if len(cfg.Env) > 0 {
		cmd.Env = os.Environ()
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
	// stderr 捕获到尾部环形缓冲：spawn/握手失败时把 agent 报错带给调用方。
	errBuf := &capBuf{max: 8 * 1024}
	cmd.Stderr = errBuf
	sysproc.Hide(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp agent %q 启动失败: %w%s", cfg.Name, err, stderrHint(errBuf))
	}
	bufDone := make(chan struct{})
	c := &Client{
		name:    cfg.Name,
		cmd:     cmd,
		stdin:   stdin,
		stderr:  errBuf,
		pending: map[int64]chan *rpcResponse{},
		lines:   make(chan rpcMessage, lineBuf),
		doneCh:  bufDone,
		hooks:   hooks,
	}
	go c.readLoop(stdout)
	go func() {
		defer close(bufDone)
		c.consumeLoop()
	}()

	// initialize 握手（30s：npx 冷启动 + agent 自身初始化可能较慢）。
	hsTimeout := 30 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 && d < hsTimeout {
			hsTimeout = d
		}
	}
	hsCtx, cancel := context.WithTimeout(ctx, hsTimeout)
	defer cancel()
	var initRes InitializeResult
	if err := c.call(hsCtx, "initialize", map[string]interface{}{
		"protocolVersion": protocolVersion,
		"clientCapabilities": map[string]interface{}{
			"fs": map[string]interface{}{
				"readTextFile":  true,
				"writeTextFile": true,
			},
		},
	}, &initRes); err != nil {
		c.Close()
		return nil, fmt.Errorf("acp agent %q initialize: %w%s", cfg.Name, err, stderrHint(errBuf))
	}
	c.AgentInfo = initRes.AgentInfo
	c.ProtocolVersion = initRes.ProtocolVersion
	return c, nil
}

// NewSession 创建 ACP 会话（session/new）。cwd 空时回落子进程 Dir。
func (c *Client) NewSession(ctx context.Context, cwd string) (string, error) {
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "session/new", map[string]interface{}{
		"cwd":        firstNonEmpty(cwd, c.processDir()),
		"mcpServers": []interface{}{},
	}, &res); err != nil {
		return "", fmt.Errorf("acp session/new: %w", err)
	}
	if res.SessionID == "" {
		return "", fmt.Errorf("acp session/new: agent 未返回 sessionId")
	}
	c.sessionID = res.SessionID
	return res.SessionID, nil
}

// processDir 子进程工作目录（cmd.Dir；未设置 = ""）。
func (c *Client) processDir() string {
	if c.cmd == nil {
		return ""
	}
	return c.cmd.Dir
}

// SetUpdateHandler 注册 session/update 回调（Prompt 前调用）。
func (c *Client) SetUpdateHandler(fn UpdateCallback) { c.updateFn = fn }

// SessionID 当前 ACP 会话 ID。
func (c *Client) SessionID() string { return c.sessionID }

// Prompt 发送用户消息并阻塞至 turn 结束（session/prompt）。
func (c *Client) Prompt(ctx context.Context, sessionID, text string) (PromptResult, error) {
	var res PromptResult
	blocks := []ContentBlock{{Type: "text", Text: text}}
	if err := c.call(ctx, "session/prompt", map[string]interface{}{
		"sessionId": sessionID,
		"prompt":    blocks,
	}, &res); err != nil {
		return PromptResult{}, err
	}
	return res, nil
}

// Cancel 中断当前 turn（session/cancel 通知，无响应帧；agent 已退出时静默）。
func (c *Client) Cancel(sessionID string) {
	if sessionID == "" {
		sessionID = c.sessionID
	}
	req := rpcRequest{JSONRPC: "2.0", Method: "session/cancel", Params: map[string]interface{}{"sessionId": sessionID}}
	if b, err := json.Marshal(req); err == nil {
		c.mu.Lock()
		_, _ = io.WriteString(c.stdin, string(b)+"\n")
		c.mu.Unlock()
	}
}

// Close 终止 agent 子进程。
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.cmd != nil && c.cmd.Process != nil {
			_ = killTree(c.cmd)
			_ = c.cmd.Wait()
		}
	})
}

// ---------- 命令启动（Windows 兼容，对齐 internal/mcp stdioCommand） ----------

// agentCommand 构建 agent 启动命令：整行命令支持（Command 含空白且 Args 为空时
// 引号感知拆分，引导用户整行粘贴 "npx -y @zed-industries/claude-code-acp"）；
// Windows 上经 cmd /C（npx/node 实为 .cmd 批处理包装，直接 spawn 报
// "executable file not found"——对齐 shell/MCP 的 Windows 路径）。
func agentCommand(cfg AgentConfig) *exec.Cmd {
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

// joinCommandLine 重组命令行：含空白片段加引号（已带引号原样保留）。
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

// firstNonEmpty 返回第一个非空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// capBuf 只保留末尾 max 字节的 stderr 捕获器（对齐 mcp.capBuf）。
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
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *capBuf) tail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

// stderrHint agent stderr 尾部摘要（空 = ""，拼接到错误信息末尾）。
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

// ---------- 读泵、消费分发与请求应答 ----------

// readLoop 常驻读泵：逐行读 agent stdout 解析为 rpcMessage 推入 lines。
// 非 JSON 行（启动 banner 等）静默丢弃。
func (c *Client) readLoop(stdout io.Reader) {
	// panic 防护（崩溃修复惯例）：读泵异常不得拖垮 GUI 进程。
	defer func() {
		if r := recover(); r != nil {
			slog.Error("ACP 读泵 panic（已拦截）", "agent", c.name, "panic", r, "stack", string(debug.Stack()))
		}
		close(c.lines)
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg rpcMessage
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		select {
		case c.lines <- msg:
		default:
		}
	}
}

// consumeLoop 消费 lines 中的帧并串行分发（由 Start 内包裹 goroutine 启动，
// 退出时关闭 doneCh = c.closed() 信源）。
func (c *Client) consumeLoop() {
	for msg := range c.lines {
		c.dispatch(msg)
	}
}

// dispatch 分发一条入站消息。
func (c *Client) dispatch(msg rpcMessage) {
	switch {
	case msg.ID != nil && msg.Method == "":
		// 响应帧 → pending 表。
		c.resolve(*msg.ID, &rpcResponse{JSONRPC: msg.JSONRPC, ID: *msg.ID, Result: msg.Result, Error: msg.Error})
	case msg.ID != nil && msg.Method != "":
		// agent → client 反向请求：异步处理（审批可能阻塞分钟级，不阻塞读泵）。
		go c.handleAgentRequest(*msg.ID, msg.Method, msg.Params)
	default:
		// 通知帧：session/update → updateFn（消费 goroutine 串行调用）。
		if msg.Method == "session/update" && c.updateFn != nil {
			var p struct {
				SessionID string        `json:"sessionId"`
				Update    SessionUpdate `json:"update"`
			}
			if json.Unmarshal(msg.Params, &p) == nil {
				c.updateFn(p.Update, msg.Params)
			}
		}
	}
}

// register/unregister/resolve pending 等待表。
func (c *Client) register(id int64) chan *rpcResponse {
	ch := make(chan *rpcResponse, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	return ch
}

func (c *Client) unregister(id int64) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *Client) resolve(id int64, resp *rpcResponse) {
	c.pendingMu.Lock()
	ch, ok := c.pending[id]
	c.pendingMu.Unlock()
	if ok {
		select {
		case ch <- resp:
		default:
		}
	}
}

// handleAgentRequest 处理 agent → client 反向请求并回帧。
// 写失败 = agent 已退出，静默。
func (c *Client) handleAgentRequest(id int64, method string, params json.RawMessage) {
	var result interface{}
	var rpcErr *rpcError
	switch method {
	case "fs/read_text_file":
		var p struct {
			SessionID string `json:"sessionId"`
			Path      string `json:"path"`
			Line      *int   `json:"line,omitempty"`
			Limit     *int   `json:"limit,omitempty"`
		}
		if json.Unmarshal(params, &p) != nil {
			rpcErr = &rpcError{Code: codeInvalidParams, Message: "invalid params"}
			break
		}
		if c.hooks.ReadTextFile == nil {
			rpcErr = &rpcError{Code: codeMethodNotFound, Message: "fs/read_text_file 不可用"}
			break
		}
		content, err := c.hooks.ReadTextFile(p.Path, derefInt(p.Line), derefInt(p.Limit))
		if err != nil {
			rpcErr = &rpcError{Code: codeInternal, Message: err.Error()}
			break
		}
		result = map[string]string{"content": content}
	case "fs/write_text_file":
		var p struct {
			SessionID string `json:"sessionId"`
			Path      string `json:"path"`
			Content   string `json:"content"`
		}
		if json.Unmarshal(params, &p) != nil {
			rpcErr = &rpcError{Code: codeInvalidParams, Message: "invalid params"}
			break
		}
		if c.hooks.WriteTextFile == nil {
			rpcErr = &rpcError{Code: codeMethodNotFound, Message: "fs/write_text_file 不可用"}
			break
		}
		if err := c.hooks.WriteTextFile(p.Path, p.Content); err != nil {
			rpcErr = &rpcError{Code: codeInternal, Message: err.Error()}
			break
		}
		result = map[string]interface{}{}
	case "session/request_permission":
		var p PermissionRequestPayload
		if json.Unmarshal(params, &p) != nil {
			rpcErr = &rpcError{Code: codeInvalidParams, Message: "invalid params"}
			break
		}
		if c.hooks.RequestPermission == nil {
			// 无审批桥（无人值守场景）：自动放行第一个 allow 类选项。
			if oid := firstAllowOption(p.Options); oid != "" {
				result = map[string]interface{}{
					"outcome": map[string]interface{}{"outcome": "selected", "optionId": oid},
				}
				break
			}
			rpcErr = &rpcError{Code: codeMethodNotFound, Message: "session/request_permission 不可用"}
			break
		}
		oid, err := c.hooks.RequestPermission(p.SessionID, &p)
		if err != nil {
			rpcErr = &rpcError{Code: codeInternal, Message: err.Error()}
			break
		}
		result = map[string]interface{}{
			"outcome": map[string]interface{}{"outcome": "selected", "optionId": oid},
		}
	default:
		rpcErr = &rpcError{Code: codeMethodNotFound, Message: "method not supported: " + method}
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: id}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result, _ = json.Marshal(result)
	}
	b, err := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: &id, Result: resp.Result, Error: resp.Error})
	if err != nil {
		return
	}
	c.mu.Lock()
	_, _ = io.WriteString(c.stdin, string(b)+"\n")
	c.mu.Unlock()
}

// JSON-RPC 错误码（标准段）。
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeInternal2      = -32000 // 服务端自定义（文件读写失败等）
)

// derefInt 指针取值（nil = 0）。
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// firstAllowOption 取第一个 allow 类 optionId（无人值守自动放行）。
func firstAllowOption(opts []PermissionOption) string {
	for _, o := range opts {
		if strings.HasPrefix(o.Kind, "allow") {
			return o.OptionID
		}
	}
	return ""
}

// ---------- 出站请求 ----------

// call 发送 JSON-RPC 请求并等待同 ID 响应（互斥串行出站；取消感知：等待全程
// select ctx.Done 与 doneCh——agent 僵死时 Stop/cancel 路径即时返回）。
func (c *Client) call(ctx context.Context, method string, params interface{}, out interface{}) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	b, err := json.Marshal(req)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if _, err := io.WriteString(c.stdin, string(b)+"\n"); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("acp write: %w", err)
	}
	c.mu.Unlock()

	ch := c.register(id)
	defer c.unregister(id)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed():
		return fmt.Errorf("acp agent %q closed stream%s", c.name, stderrHint(c.stderr))
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("acp %s error %d: %s", method, resp.Error.Code, resp.Error.Message)
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("acp %s decode: %w", method, err)
			}
		}
		return nil
	}
}

// closed 读泵关闭信号（lines 通道关闭 = agent stdout 结束）。
func (c *Client) closed() <-chan struct{} { return c.doneCh }
