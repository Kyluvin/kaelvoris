// codexbridge.go Codex app-server 协议适配器。
//
// Codex（openai/codex）主分支不实现 ACP（Agent Client Protocol），提供的是
// 自研 app-server 协议（JSON-RPC over stdio/websocket，见 codex-rs/app-server-protocol）。
// 本文件把它桥接到 kaelvoris 的 ACP 客户端接口（acp_run.go 复用同一套
// Start/NewSession/Prompt/Cancel/Close + UpdateMapper 流式映射 + 审批桥），
// 使「Codex」可作为一个外部编码 agent 从 Composer 选择器接入。
//
// 协议差异（相对 ACP，详见 plans/codex-bridge.md）：
//   - 帧无 jsonrpc 字段（rpc.rs 明示非标准 JSON-RPC 2.0），ID 可为 string/int。
//   - 会话创建 = thread/start（响应含 thread.id）；turn 提交 = turn/start（异步，
//     响应立即返回 turn.id，实际执行经通知流推送）。
//   - 完成信号 = turn/completed 通知（turn.status）；中断 = turn/interrupt。
//   - 流式正文 = agent_message.delta；思考 = reasoning.text.delta 等；
//     工具卡 = item/started + item/completed（item.type 区分）。
//   - 反向请求（审批）= item/commandExecution/requestApproval、
//     item/fileChange/requestApproval（v2 新 API）与旧版 applyPatch/execCommand。
//
// 边界（明确不做）：dynamic tool call（item/tool/call）、MCP elicitation、
// 终端命令交互（commandExec/*）——一律回 method not found（agent 侧降级）。
// 沙箱/审批 reviewer 等安全字段不设置（保持默认）。
package acp

import (
	"bufio"
	"context"
	"encoding/base64"
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

// CodexProtocol 适配器标识（AgentConfig.Protocol 字段取值 "codex"）。
const CodexProtocol = "codex"

// ---------- app-server 帧（非标准 JSON-RPC：无 jsonrpc 字段） ----------

// codexRequest 客户端 → 服务器请求。
type codexRequest struct {
	ID     int64       `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params,omitempty"`
}

// codexRequestID 支持 string/int 的请求 ID（服务端回响应时原样回传）。
type codexRequestID struct {
	Str *string `json:"-"`
	Int *int64  `json:"-"`
}

// responseID 响应帧的 id（string 或 int，用 RawMessage 分流）。
// 我们只用 int64 出站 ID，服务端回传 int；仍兼容 string 以便测试假服务器。
type codexResponseFrame struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

// CodexNotification 服务端 → 客户端通知/反向请求（无 id 字段为通知）。
type CodexNotification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// codexMessage 通用入站帧。（无 jsonrpc 字段；无 method = 响应帧）
type codexMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

// parseResponseID 解析响应帧 ID（int64 或字符串，容错测试假服务器）。
func parseResponseID(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		var v int64
		if _, err := fmt.Sscanf(s, "%d", &v); err == nil {
			return v, true
		}
	}
	return 0, false
}

// ---------- app-server 载荷类型 ----------

// codexUserInput UserInput 变体（仅文本/图片路径子集；对齐 schema v2 UserInput）。
type codexUserInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Path string `json:"path,omitempty"`
	URL  string `json:"url,omitempty"`
}

// codexTurnStatus turn.status 枚举。
type codexTurnStatus string

const (
	codexTurnCompleted   codexTurnStatus = "completed"
	codexTurnInterrupted codexTurnStatus = "interrupted"
	codexTurnFailed      codexTurnStatus = "failed"
	codexTurnInProgress  codexTurnStatus = "inProgress"
)

// codexThread thread 对象（thread/start 响应中的 thread 字段，最小子集）。
type codexThread struct {
	ID string `json:"id"`
}

// codexTurn turn 对象（turn/start 响应与 turn/completed 通知共用，最小子集）。
type codexTurn struct {
	ID     string          `json:"id"`
	Status codexTurnStatus `json:"status"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// codexThreadItem item 对象（item/started、item/completed 通知，tag=type 双形态）。
type codexThreadItem struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	// agentMessage
	Text string `json:"text,omitempty"`
	// commandExecution / fileChange / mcpToolCall
	Command          string          `json:"command,omitempty"`
	AggregatedOutput string          `json:"aggregatedOutput,omitempty"`
	ExitCode         *int            `json:"exitCode,omitempty"`
	Status           string          `json:"status,omitempty"`
	Changes          json.RawMessage `json:"changes,omitempty"`
	Server           string          `json:"server,omitempty"`
	Tool             string          `json:"tool,omitempty"`
	Arguments        json.RawMessage `json:"arguments,omitempty"`
	Name             string          `json:"name,omitempty"`
	// plan
	Text2 string `json:"-"`
}

// ---------- CodexClient：app-server 连接（实现与 Client 同构的接口面） ----------

// CodexClient codex app-server 连接（一个进程 = 一次 turn 会话生命周期，
// 对齐 ACP Client 语义：进程内可多轮续聊，但 kaelvoris 每轮 spawn 新进程）。
type CodexClient struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *capBuf
	nextID int64
	mu     sync.Mutex // 保护 stdin 写与 nextID（出站串行）

	pendingMu sync.Mutex
	pending   map[int64]chan *codexResponseFrame

	// updateFn 服务端通知回调（Prompt 前经 SetUpdateHandler 注册；nil = 丢弃）。
	// 由消费 goroutine 串行调用（单线程语义，消费方无需加锁）。
	updateFn func(n CodexNotification)

	threadID string
	turnID   string

	// hooks 反向请求处理器（保留：后续审批扩展接 askCh 桥）。
	hooks FsHooks

	// AgentInfo 握手获得的 agent 元信息（TestAgent 面板展示；app-server 无该字段，
	// 用协议名兜底）。
	AgentInfo       *AgentInfo
	ProtocolVersion int

	lines chan codexMessage

	doneCh chan struct{}

	// turnDone 当前 turn 完成信号（turn/completed 通知触发；一次 turn 一次）。
	turnDoneCh chan codexTurn
	turnDoneMu sync.Mutex

	// closeOnce Close 幂等（ctx 取消与显式 Close 并发时可重复调用不双杀）。
	closeOnce sync.Once
}

// codexAgentCommand 构建 codex app-server 启动命令。
// 配置格式与 ACP 一致（Command 可为整行），默认补 `app-server --listen stdio://`。
func codexAgentCommand(cfg AgentConfig) *exec.Cmd {
	parts := []string{}
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
	// codex app-server 默认即 stdio://（main.rs listen 默认 stdio://），无需显式参数。
	return commandFor(parts)
}

// commandFor 按平台构造命令（Windows 经 cmd /C，对齐 agentCommand）。
// 抽出来供测试假服务器复用同一平台路径。
func commandFor(parts []string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/C", joinCommandLine(parts))
	}
	return exec.Command(parts[0], parts[1:]...)
}

// StartCodex 启动 codex app-server 并完成 initialize 握手。
// cwd 为任务工作区（子进程 Dir + thread/start 的 cwd）。
func StartCodex(ctx context.Context, cfg AgentConfig, cwd string, hooks FsHooks) (*CodexClient, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("codex agent %q: 启动命令为空", cfg.Name)
	}
	cmd := codexAgentCommand(cfg)
	if cmd == nil {
		return nil, fmt.Errorf("codex agent %q: 启动命令为空", cfg.Name)
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
	errBuf := &capBuf{max: 8 * 1024}
	cmd.Stderr = errBuf
	sysproc.Hide(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex agent %q 启动失败: %w%s", cfg.Name, err, stderrHint(errBuf))
	}
	c := &CodexClient{
		name:       cfg.Name,
		cmd:        cmd,
		stdin:      stdin,
		stderr:     errBuf,
		pending:    map[int64]chan *codexResponseFrame{},
		lines:      make(chan codexMessage, lineBuf),
		doneCh:     make(chan struct{}),
		turnDoneCh: make(chan codexTurn, 1),
		hooks:      hooks,
	}
	go c.readLoop(stdout)
	go func() {
		defer close(c.doneCh)
		c.consumeLoop()
	}()

	// initialize 握手（30s：codex 冷启动较慢）。
	hsTimeout := 30 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 && d < hsTimeout {
			hsTimeout = d
		}
	}
	hsCtx, cancel := context.WithTimeout(ctx, hsTimeout)
	defer cancel()
	var initRes struct {
		UserAgent string `json:"userAgent"`
	}
	if err := c.call(hsCtx, "initialize", map[string]interface{}{
		"clientInfo": map[string]string{
			"name":    "kaelvoris",
			"version": "1.0",
		},
		"capabilities": map[string]interface{}{
			"optOutNotificationMethods": []string{},
		},
	}, &initRes); err != nil {
		c.Close()
		return nil, fmt.Errorf("codex agent %q initialize: %w%s", cfg.Name, err, stderrHint(errBuf))
	}
	// app-server 无 agentInfo；用协议名兜底展示（TestACPAgent 面板）。
	c.AgentInfo = &AgentInfo{Name: "Codex app-server", Version: initRes.UserAgent}
	c.ProtocolVersion = 2
	return c, nil
}

// NewThread 创建 app-server 会话（thread/start）。
func (c *CodexClient) NewThread(ctx context.Context, cwd string) (string, error) {
	var res struct {
		Thread codexThread `json:"thread"`
	}
	params := map[string]interface{}{
		"cwd": firstNonEmpty(cwd, c.processDir()),
	}
	if err := c.call(ctx, "thread/start", params, &res); err != nil {
		return "", fmt.Errorf("codex thread/start: %w", err)
	}
	if res.Thread.ID == "" {
		return "", fmt.Errorf("codex thread/start: 未返回 thread.id")
	}
	c.threadID = res.Thread.ID
	return res.Thread.ID, nil
}

// StartTurn 提交用户输入（turn/start，异步；返回 turnId，完成经 turnDoneCh）。
func (c *CodexClient) StartTurn(ctx context.Context, threadID, text string) (string, error) {
	var res struct {
		Turn codexTurn `json:"turn"`
	}
	params := map[string]interface{}{
		"threadId": threadID,
		"input": []codexUserInput{
			{Type: "text", Text: text},
		},
	}
	if err := c.call(ctx, "turn/start", params, &res); err != nil {
		return "", fmt.Errorf("codex turn/start: %w", err)
	}
	if res.Turn.ID == "" {
		return "", fmt.Errorf("codex turn/start: 未返回 turn.id")
	}
	c.turnID = res.Turn.ID
	return res.Turn.ID, nil
}

// WaitTurn 阻塞至 turn/completed 通知（返回最终 turn 状态）。
// ctx 取消 = 上层停止（调用方先 Interrupt 再取消）。
func (c *CodexClient) WaitTurn(ctx context.Context) (codexTurn, error) {
	select {
	case <-ctx.Done():
		return codexTurn{}, ctx.Err()
	case <-c.doneCh:
		return codexTurn{}, fmt.Errorf("codex agent %q closed stream%s", c.name, stderrHint(c.stderr))
	case t := <-c.turnDoneCh:
		return t, nil
	}
}

// Interrupt 中断当前 turn（turn/interrupt 通知，无响应帧）。
func (c *CodexClient) Interrupt(threadID, turnID string) {
	if threadID == "" {
		threadID = c.threadID
	}
	if turnID == "" {
		turnID = c.turnID
	}
	req := codexRequest{ID: -1, Method: "turn/interrupt", Params: map[string]interface{}{
		"threadId": threadID, "turnId": turnID,
	}}
	if b, err := json.Marshal(req); err == nil {
		c.mu.Lock()
		_, _ = io.WriteString(c.stdin, string(b)+"\n")
		c.mu.Unlock()
	}
}

// Close 终止 agent 子进程（killTree 整树终止：Windows cmd/npx/node 全链，
// 对齐 conn.go Client.Close 的进程树回收语义）。
func (c *CodexClient) Close() {
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

// SetUpdateHandler 注册通知回调（StartTurn 前调用）。
func (c *CodexClient) SetUpdateHandler(fn func(n CodexNotification)) { c.updateFn = fn }

// ThreadID 当前 app-server 线程 ID。
func (c *CodexClient) ThreadID() string { return c.threadID }

// processDir 子进程工作目录。
func (c *CodexClient) processDir() string {
	if c.cmd == nil {
		return ""
	}
	return c.cmd.Dir
}

// ---------- 读泵、消费分发与请求应答（对齐 ACP Client 语义） ----------

func (c *CodexClient) readLoop(stdout io.Reader) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Codex 读泵 panic（已拦截）", "agent", c.name, "panic", r, "stack", string(debug.Stack()))
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
		var msg codexMessage
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		select {
		case c.lines <- msg:
		default:
		}
	}
}

func (c *CodexClient) consumeLoop() {
	for msg := range c.lines {
		c.dispatch(msg)
	}
}

func (c *CodexClient) dispatch(msg codexMessage) {
	if len(msg.Method) == 0 {
		// 响应帧 → pending 表。
		if id, ok := parseResponseID(msg.ID); ok {
			c.resolve(id, &codexResponseFrame{ID: msg.ID, Result: msg.Result, Error: msg.Error})
		}
		return
	}
	// 有 id 的请求帧 = 服务器 → 客户端反向请求（审批等）：异步回帧（不阻塞读泵）。
	if len(msg.ID) > 0 {
		if id, ok := parseResponseID(msg.ID); ok {
			go c.handleServerRequest(id, msg.Method, msg.Params)
		}
		return
	}
	switch msg.Method {
	case "turn/completed":
		var p struct {
			ThreadID string    `json:"threadId"`
			Turn     codexTurn `json:"turn"`
		}
		if json.Unmarshal(msg.Params, &p) == nil {
			select {
			case c.turnDoneCh <- p.Turn:
			default:
			}
		}
	default:
		// 其余通知 → updateFn（消费 goroutine 串行调用）。
		if c.updateFn != nil {
			c.updateFn(CodexNotification{Method: msg.Method, Params: msg.Params})
		}
	}
}

// handleServerRequest 处理服务器 → 客户端反向请求（审批 / fs 读写 / 未知拒绝）。
// 写失败 = agent 已退出，静默。
func (c *CodexClient) handleServerRequest(id int64, method string, params json.RawMessage) {
	var result interface{}
	var rpcErr *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	}
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		// Codex 审批：参数含 kind/command；决策 accept / decline。
		var p struct {
			Kind    string `json:"kind"`
			Command string `json:"command"`
			Reason  string `json:"reason"`
		}
		_ = json.Unmarshal(params, &p) // 参数容错：字段缺失不致命
		title := p.Command
		if title == "" {
			title = p.Reason
		}
		if c.hooks.RequestPermission == nil {
			// 无人值守：自动放行（对齐 ACP 行为）。
			result = map[string]interface{}{"decision": "accept"}
			break
		}
		// 适配到现有审批桥：造一个 PermissionRequestPayload（kind/options 透传），
		// 返回 optionId 语义：非空 = 放行。
		permPayload := &PermissionRequestPayload{
			ToolCall: mustJSONRaw(map[string]string{"title": title, "kind": kindOfApproval(p.Kind)}),
		}
		if oid, err := c.hooks.RequestPermission("", permPayload); err == nil && oid != "" {
			result = map[string]interface{}{"decision": "accept"}
		} else {
			result = map[string]interface{}{"decision": "decline"}
		}
	case "fs/readFile":
		var p struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(params, &p) != nil || c.hooks.ReadTextFile == nil {
			rpcErr = &struct {
				Code    int             `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data,omitempty"`
			}{Code: codeMethodNotFound, Message: "fs/readFile 不可用"}
			break
		}
		content, err := c.hooks.ReadTextFile(p.Path, 0, 0)
		if err != nil {
			rpcErr = &struct {
				Code    int             `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data,omitempty"`
			}{Code: codeInternal, Message: err.Error()}
			break
		}
		result = map[string]interface{}{"dataBase64": base64Std(content)}
	case "fs/writeFile":
		var p struct {
			Path       string `json:"path"`
			DataBase64 string `json:"dataBase64"`
		}
		if json.Unmarshal(params, &p) != nil || c.hooks.WriteTextFile == nil {
			rpcErr = &struct {
				Code    int             `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data,omitempty"`
			}{Code: codeMethodNotFound, Message: "fs/writeFile 不可用"}
			break
		}
		content, err := base64Decode(p.DataBase64)
		if err != nil {
			rpcErr = &struct {
				Code    int             `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data,omitempty"`
			}{Code: codeInternal, Message: err.Error()}
			break
		}
		if err := c.hooks.WriteTextFile(p.Path, string(content)); err != nil {
			rpcErr = &struct {
				Code    int             `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data,omitempty"`
			}{Code: codeInternal, Message: err.Error()}
			break
		}
		result = map[string]interface{}{}
	default:
		rpcErr = &struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data,omitempty"`
		}{Code: codeMethodNotFound, Message: "method not supported: " + method}
	}
	resp := codexResponseFrame{ID: int64JSON(id)}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result, _ = json.Marshal(result)
	}
	b, err := json.Marshal(codexMessage{ID: int64JSON(id), Result: resp.Result, Error: resp.Error})
	if err != nil {
		return
	}
	c.mu.Lock()
	_, _ = io.WriteString(c.stdin, string(b)+"\n")
	c.mu.Unlock()
}

// kindOfApproval codex 审批 kind → ACP 工具 kind（审批卡展示）。
func kindOfApproval(kind string) string {
	switch kind {
	case "command":
		return "execute"
	case "file_change":
		return "edit"
	default:
		return "other"
	}
}

// mustJSONRaw 序列化为 RawMessage（失败 = "{}"）。
func mustJSONRaw(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// base64Std Base64 标准编码（无换行）。
func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// base64Decode Base64 解码（容错 URL 变体）。
func base64Decode(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// int64JSON int64 → json.RawMessage（响应帧 id）。
func int64JSON(id int64) json.RawMessage {
	b, _ := json.Marshal(id)
	return b
}

// register/unregister/resolve pending 等待表（对齐 ACP Client）。
func (c *CodexClient) register(id int64) chan *codexResponseFrame {
	ch := make(chan *codexResponseFrame, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	return ch
}

func (c *CodexClient) unregister(id int64) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *CodexClient) resolve(id int64, resp *codexResponseFrame) {
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

// call 发送请求并等待同 ID 响应（互斥串行出站；取消感知）。
func (c *CodexClient) call(ctx context.Context, method string, params interface{}, out interface{}) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	req := codexRequest{ID: id, Method: method, Params: params}
	b, err := json.Marshal(req)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if _, err := io.WriteString(c.stdin, string(b)+"\n"); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("codex write: %w", err)
	}
	c.mu.Unlock()

	ch := c.register(id)
	defer c.unregister(id)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.doneCh:
		return fmt.Errorf("codex agent %q closed stream%s", c.name, stderrHint(c.stderr))
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("codex %s error %d: %s", method, resp.Error.Code, resp.Error.Message)
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("codex %s decode: %w", method, err)
			}
		}
		return nil
	}
}

// closed 读泵关闭信号。
func (c *CodexClient) closed() <-chan struct{} { return c.doneCh }
