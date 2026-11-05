package desktop

// channel_wecom.go 企业微信通道运行时桥（doc/01-需求文档 FR-7.2）：
//
//	智能机器人模式（BotID+Secret → channel_wecom_ws.go）：WS 长连接免公网
//	回调接收消息（aibot_msg_callback），回复经同 req_id respond stream 帧
//	ACK、后续回执经 aibot_send_msg markdown 下发；三种凭据中优先级最高。
//	应用消息模式：本地 HTTP 回调接收用户指令（企微后台「接收消息」回调 URL
//	指向本机监听端口）→ 会话任务执行 → message/send 回传 → ask 文本协议
//	审批闭环。
//	群机器人模式：Webhook 单向推送（受理/完成/失败/ask 提问推送到群）。企微
//	平台限制群机器人无法接收消息，双向交互请使用智能机器人/应用消息模式。
//
// 设计要点（与 channel_lark.go 对齐）：
//   - 内核完全复用桌面端路径：buildKernel / buildRunEngine / makeSayBridge /
//     ApproveAsk（run.askCh 应答通道），会话 ChannelID = "wecom"，会话
//     ID = "wecom-" + 成员 UserID（按用户维度隔离会话）。
//   - 回调加解密：WXBizMsgCrypt（AES-256-CBC + SHA1 签名，标准库实现），
//     GET 验证 URL（echostr 解密回显）+ POST 消息推送（XML 载荷）。
//   - 发送双路径：群机器人 webhook/send（免鉴权）；应用消息 gettoken +
//     message/send（token 缓存 + 过期自动刷新，42001/40014 失效重试一次）。
//   - ask 审批闭环：文本协议回复 y/n/补充内容（复用 normalizeAnswer 三态）。
//   - MsgId 去重：企微 5 秒未收到 200 响应会重推回调，去重防任务重复受理。
//   - AllowedUsers 白名单（成员 UserID，逗号分隔，空 = 不限制）。

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kaelvoris/internal/session"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

const wecomAPIBase = "https://qyapi.weixin.qq.com/cgi-bin"

// wecomHTTP 企微开放 API 调用客户端（统一超时，防止发送阻塞任务 goroutine）。
var wecomHTTP = &http.Client{Timeout: 15 * time.Second}

// wecomDefaultPort 应用消息模式本地回调默认监听端口。
const wecomDefaultPort = 9880

// WeComChannel 企业微信通道实例。
type WeComChannel struct {
	app *App
	cfg ChannelConfig

	online atomic.Bool
	cancel context.CancelFunc

	// access_token 缓存（应用消息模式；gettoken 有频率限制，缓存 + 过期刷新）。
	tokMu      sync.Mutex
	token      string
	tokenUntil time.Time

	// MsgId 去重簿（企微 5 秒无 200 响应会重推回调）。
	seenMu sync.Mutex
	seen   map[int64]time.Time

	// 智能机器人 WS 长连接运行时（BotID+Secret 模式；startAIBot 赋值，
	// nil = 未启用智能机器人模式，sendText 回落应用消息/Webhook）。
	ai *wecomAIBotConn

	// 智能机器人 msgid 去重簿（字符串 ID；企微约 5s 未 ACK 会重推同一回调）。
	seenStrMu sync.Mutex
	seenStr   map[string]time.Time

	mu      sync.Mutex
	running map[string]bool        // userID → 任务运行中（防重入）
	pending map[string]*pendingAsk // userID → 等待用户应答的 ask
}

// port 本地回调监听端口（默认 9880）。
func (ch *WeComChannel) port() int {
	if ch.cfg.CallbackPort > 0 && ch.cfg.CallbackPort < 65536 {
		return ch.cfg.CallbackPort
	}
	return wecomDefaultPort
}

// ---------- 生命周期 ----------

// Start 启动通道（阻塞；goroutine 中运行）。凭据分流（优先级从高到低）：
//   - 智能机器人模式（BotID+BotSecret）：WS 长连接免公网回调，双向交互；
//   - 应用消息模式（CorpID+CorpSecret）：启动本地回调 HTTP 服务器接收消息；
//   - 群机器人模式（仅 WebhookKey）：企微平台无接收能力，标记可推送后挂起，
//     等待 Stop 取消。
func (ch *WeComChannel) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	ch.cancel = cancel

	// 智能机器人 WS 长连接模式（startAIBot 阻塞运行；断线自动重连，Stop 取消退出）。
	if ch.cfg.BotID != "" && ch.cfg.BotSecret != "" {
		ch.startAIBot(ctx)
		return
	}

	if ch.cfg.CorpID != "" && ch.cfg.CorpSecret != "" {
		addr := fmt.Sprintf(":%d", ch.port())
		mux := http.NewServeMux()
		mux.HandleFunc("/wecom/callback", ch.handleCallback)
		srv := &http.Server{Addr: addr, Handler: mux}
		errCh := make(chan error, 1)
		go func() { errCh <- srv.ListenAndServe() }()
		ch.online.Store(true)
		ch.app.pushChannelStatus()
		slog.Info("企业微信回调服务已启动", "name", ch.cfg.Name, "addr", addr)
		select {
		case err := <-errCh:
			if err != nil && err != http.ErrServerClosed {
				slog.Error("企业微信回调服务异常退出", "name", ch.cfg.Name, "err", err)
			}
		case <-ctx.Done():
		}
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelShutdown()
		_ = srv.Shutdown(shutdownCtx)
		ch.online.Store(false)
		ch.app.pushChannelStatus()
		return
	}

	// 群机器人 Webhook 模式：单向推送。
	ch.online.Store(true)
	ch.app.pushChannelStatus()
	<-ctx.Done()
	ch.online.Store(false)
	ch.app.pushChannelStatus()
}

// Stop 停止通道（关闭回调服务器；进行中任务随会话 cancel 语义由用户停止）。
func (ch *WeComChannel) Stop() {
	ch.online.Store(false)
	ch.mu.Lock()
	cancel := ch.cancel
	ch.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ---------- 回调 HTTP 处理（企微 WXBizMsgCrypt） ----------

// handleCallback 企微回调入口：GET 验证 URL（echostr 解密回显）/ POST 消息推送。
// 用户在企微后台配置的回调 URL 形如 http://<host>:<port>/wecom/callback。
func (ch *WeComChannel) handleCallback(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = recover() }() // 单请求异常不拖垮服务器
	q := r.URL.Query()
	sig, ts, nonce := q.Get("msg_signature"), q.Get("timestamp"), q.Get("nonce")
	switch r.Method {
	case http.MethodGet:
		// URL 有效性验证：解密 echostr 并原样返回明文。
		plain, err := ch.decryptMsg(sig, ts, nonce, q.Get("echostr"))
		if err != nil {
			slog.Warn("企微回调验签失败", "name", ch.cfg.Name, "err", err)
			http.Error(w, "verify failed", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, plain)
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		msg, err := ch.decryptCallback(sig, ts, nonce, body)
		if err != nil {
			slog.Warn("企微回调解密失败", "name", ch.cfg.Name, "err", err)
			http.Error(w, "decrypt failed", http.StatusBadRequest)
			return
		}
		// 企微要求 5 秒内响应；先应答后异步处理任务。
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "success")
		ch.onMessage(msg)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// wecomEncXML 回调加密载荷 XML（POST body）。
type wecomEncXML struct {
	Encrypt    string `xml:"Encrypt"`
	ToUserName string `xml:"ToUserName"`
}

// wecomMsgXML 解密后的消息 XML。
type wecomMsgXML struct {
	ToUserName   string `xml:"ToUserName"`
	FromUserName string `xml:"FromUserName"`
	CreateTime   int64  `xml:"CreateTime"`
	MsgType      string `xml:"MsgType"`
	Content      string `xml:"Content"`
	MsgID        int64  `xml:"MsgId"`
	AgentID      int64  `xml:"AgentID"`
}

// decryptCallback 解密回调 POST 载荷：验签 → 解密 → XML 解析。
func (ch *WeComChannel) decryptCallback(sig, ts, nonce string, body []byte) (*wecomMsgXML, error) {
	var enc wecomEncXML
	if err := xml.Unmarshal(body, &enc); err != nil {
		return nil, fmt.Errorf("回调 XML 解析失败: %w", err)
	}
	if strings.TrimSpace(enc.Encrypt) == "" {
		return nil, errors.New("回调载荷缺少 Encrypt 字段")
	}
	plain, err := ch.decryptMsg(sig, ts, nonce, enc.Encrypt)
	if err != nil {
		return nil, err
	}
	var m wecomMsgXML
	if err := xml.Unmarshal([]byte(plain), &m); err != nil {
		return nil, fmt.Errorf("消息 XML 解析失败: %w", err)
	}
	return &m, nil
}

// decryptMsg 验签 + 解密单条密文（echostr / Encrypt 通用）。
func (ch *WeComChannel) decryptMsg(sig, ts, nonce, encryptB64 string) (string, error) {
	if wecomSignature(ch.cfg.Token, ts, nonce, encryptB64) != sig {
		return "", errors.New("msg_signature 校验失败")
	}
	return wecomDecrypt(ch.cfg.EncodingAESKey, encryptB64, ch.cfg.CorpID)
}

// ---------- 消息接收与路由 ----------

// onMessage 解密后的消息入口：应答路由 或 新任务（对齐 LarkChannel.onMessageReceive）。
func (ch *WeComChannel) onMessage(m *wecomMsgXML) {
	if m == nil || m.FromUserName == "" {
		return
	}
	text := strings.TrimSpace(m.Content)
	if text == "" {
		return
	}
	// MsgId 去重（企微 5 秒无 200 响应会重推同一消息）。
	if m.MsgID > 0 && ch.seenMsg(m.MsgID) {
		return
	}
	if m.MsgType != "text" {
		ch.sendText(m.FromUserName, "当前仅支持文本消息（图片/文件解析在后续版本开放）。")
		return
	}
	// 白名单（AllowedUsers 逗号分隔成员 UserID；空 = 不限制）。
	if allow := strings.TrimSpace(ch.cfg.AllowedUsers); allow != "" {
		ok := false
		for _, u := range strings.Split(allow, ",") {
			if strings.TrimSpace(u) == m.FromUserName {
				ok = true
				break
			}
		}
		if !ok {
			return
		}
	}

	// 运行中会话：路由为 ask 应答（y/n/补充文本，对齐 ApproveAsk 三态）。
	ch.mu.Lock()
	p := ch.pending[m.FromUserName]
	busy := ch.running[m.FromUserName]
	ch.mu.Unlock()
	if p != nil {
		ch.deliverAnswer(p, text)
		return
	}
	if busy {
		// 运行中不停止：消息入队（FIFO），本轮任务结束后 runWecomTask 自动继续对话。
		if ch.app.enqueueSessionMsg("wecom-"+m.FromUserName, text, nil) {
			ch.sendText(m.FromUserName, "⏳ 已加入队列（当前任务完成后自动继续）："+truncateRunes(firstLine(text), 60))
			return
		}
		// 竞态：会话恰在本检查后结束运行 → 回落正常发送。
	}
	go ch.runWecomTaskSafe(m.FromUserName, text, false)
}

// seenMsg MsgId 去重；簿记超 1024 条时清理 10 分钟前的旧记录。
func (ch *WeComChannel) seenMsg(id int64) bool {
	ch.seenMu.Lock()
	defer ch.seenMu.Unlock()
	if ch.seen == nil {
		ch.seen = map[int64]time.Time{}
	}
	if t, ok := ch.seen[id]; ok && time.Since(t) < 10*time.Minute {
		return true
	}
	if len(ch.seen) > 1024 {
		now := time.Now()
		for k, t := range ch.seen {
			if now.Sub(t) >= 10*time.Minute {
				delete(ch.seen, k)
			}
		}
	}
	ch.seen[id] = time.Now()
	return false
}

// deliverAnswer 把文本应答路由到挂起中的 ask（复用 ApproveAsk 多会话路由）。
func (ch *WeComChannel) deliverAnswer(p *pendingAsk, text string) {
	var resp message.AskResponse
	switch normalizeAnswer(text) {
	case "yes":
		resp, text = message.ResponseYes, ""
	case "no":
		resp, text = message.ResponseNo, ""
	default:
		resp = message.ResponseMessageResponse
	}
	_ = ch.app.ApproveAsk(p.sessionID, string(resp), text)
}

// ---------- 任务执行：复用桌面端内核路径 ----------

// runWecomTaskSafe 任务 goroutine 的 panic 防护（对齐 runLarkTaskSafe）：
// 独立 goroutine 未捕获 panic 会杀死整个进程，拦截后回执企微错误提示。
// skipAckReceipt：智能机器人路径受理回执已随 respond ACK 内联下发（onAIBotMessage），
// 传 true 跳过轮次循环开头的「已开始处理」回执，避免重复消息。
func (ch *WeComChannel) runWecomTaskSafe(userID, text string, skipAckReceipt bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("企业微信任务 panic（已拦截，进程保持运行）",
				"user", userID, "panic", r, "stack", string(debug.Stack()))
			ch.sendText(userID, "❌ 任务内部异常终止（已恢复）："+truncateRunes(fmt.Sprint(r), 500))
		}
	}()
	ch.runWecomTask(userID, text, skipAckReceipt)
}

// runWecomTask 以会话（ChannelID="wecom"）运行一条指令；事件回传企微 + 推送前端。
// skipAckReceipt=true 时跳过本轮开头的「已开始处理」回执（智能机器人路径
// 受理回执已随 respond ACK 下发；队列续发轮仍正常回执）。
func (ch *WeComChannel) runWecomTask(userID, text string, skipAckReceipt bool) {
	a := ch.app
	sessionID := "wecom-" + userID

	// 内核组件惰性构建（与 runTask / runLarkTask 一致）。
	a.kmu.Lock()
	needBuild := a.modeMgr == nil || a.skillMgr == nil
	a.kmu.Unlock()
	if needBuild {
		if err := a.buildKernel(); err != nil {
			ch.sendText(userID, "❌ "+err.Error())
			return
		}
	}
	a.kmu.Lock()
	modeMgr, skillMgr := a.modeMgr, a.skillMgr
	a.kmu.Unlock()

	// 岗位分配收口（doc/09-设计方案 Phase 2-①，§4.1）：建会话前统一
	// ResolveAssignment 解析绑定（固定/降级/岗位池/裸跑），结果固化进会话快照；
	// 解析报错（员工不存在/池空）回执企微并终止本轮（§2 池空不静默降级）。
	assign, ok := ch.resolveChannelAssignment()
	if !ok {
		ch.sendText(userID, "❌ 岗位分配失败："+assign.errMsg)
		return
	}

	// 会话创建/复用（含恢复摘要，与桌面端一致）。
	now := time.Now()
	var sess *session.Session
	var resumeDigest string
	if old, err := a.store.Get(sessionID); err == nil && old != nil {
		sess = old
		sess.SetTitle(truncateTitle(text, 40))
		if snap := sess.Snapshot(); len(snap.Messages) > 0 || len(snap.Todos) > 0 {
			resumeDigest = ResumeDigestFor(snap, a.memory.Load(sessionID), text)
		}
	} else {
		sess = &session.Session{
			ID:         sessionID,
			ChannelID:  "wecom",
			Title:      truncateTitle(text, 40),
			Mode:       a.GetMode(),
			Workspace:  a.cfgWorkspace(),
			EmployeeID: ch.cfg.EmployeeID, // §10.3 渠道绑定唯一数字员工
			CreatedAt:  now,
		}
		// §4.1 收口：解析结果固化进新会话（EmployeeID + 岗位快照，生命周期不重选）。
		applyChannelAssignSnapshot(sess, assign)
	}
	if sess.Snapshot().Workspace == "" {
		sess.SetWorkspace(a.cfgWorkspace())
	}
	// 错误/完成态清除（与桌面端 runTask 一致）：新任务开始重置上一轮标记。
	sess.SetError(false)
	sess.SetDone(false)
	a.store.Save(sess)
	uiEmit(a.ctx, "kaelvoris:sessions", a.store.List())

	run := &taskRun{id: sessionID, workspace: sess.Snapshot().Workspace, employeeID: sess.Snapshot().EmployeeID, todo: tools.NewTodoStore(), askCh: make(chan askReply, 1)}
	run.mcpProbe = text // MCP 惰性加载：企微任务文本作为激活信号
	// §10 降级留痕（会话可见）：以 say text 写入会话历史 + 前端推送（run 已建）。
	emitAssignNotice(a, sess, run, sessionID, assign.notice)
	if snap := sess.Snapshot(); len(snap.Todos) > 0 {
		run.todo.Set(snap.Todos)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	ch.mu.Lock()
	ch.running[userID] = true
	ch.mu.Unlock()
	defer func() {
		if run.cleanup != nil {
			run.cleanup()
		}
		cancel()
		ch.mu.Lock()
		delete(ch.pending, userID)
		delete(ch.running, userID)
		ch.mu.Unlock()
		a.runsMu.Lock()
		delete(a.runs, sessionID)
		a.runsMu.Unlock()
		a.registry.Remove(sessionID)
		a.pushChannelStatus()
		// 会话级状态修复（同 runTask）：任务结束恒报本会话 false，不报全局
		// IsRunning()——否则并发任务会把已完成的企微会话卡在「运行中」。
		uiEmit(a.ctx, "kaelvoris:status", map[string]any{"running": false, "sessionId": sessionID})
	}()
	a.runsMu.Lock()
	a.runs[sessionID] = run
	a.runsMu.Unlock()
	a.registry.Push(run)
	uiEmit(a.ctx, "kaelvoris:status", map[string]any{"running": true, "sessionId": sessionID})

	// 轮次循环（运行中不停止，对齐桌面端 runTask）：curText 为当前轮任务；
	// 每轮结束检查队列，有待续消息且本轮非中止 → 以队首消息续发新引擎轮次
	// （会话不退出运行态；MCP 子进程生命周期 = 每轮）。
	curText := text
	for {
		// 回执：本轮任务已受理（智能机器人首轮已随 respond ACK 下发则跳过）。
		if !skipAckReceipt {
			ch.sendText(userID, "🚀 已开始处理："+truncateRunes(firstLine(curText), 60))
		}
		skipAckReceipt = false

		engine, mcpClients := a.buildRunEngine(ctx, modeMgr, skillMgr, run, nil)
		// codex 对齐补全 ⑦：任务结束关闭全部 MCP 服务器子进程（生命周期 = 本轮）。
		flushMCP := func() {
			for _, cli := range mcpClients {
				if cli != nil {
					cli.Close()
				}
			}
		}
		engine.ResumeDigest = resumeDigest
		engine.MemoryDoc = a.memory.Load(sessionID)
		engine.MemoryPath = a.memory.Path(sessionID)

		// say 双路桥：桌面端事件桥 + 企微回传（定型 text/error）。
		desktopSay := a.makeSayBridge(sess, run, sessionID)
		say := func(m message.ClineMessage) {
			desktopSay(m)
			ch.pushToWecom(userID, m)
		}

		result, runErr := engine.RunInMode(ctx, a.GetMode(), curText, false, 0, say, ch.wrapAsk(run, ctx, userID))
		flushMCP()
		if runErr != nil {
			ch.sendText(userID, "❌ 任务失败："+truncateRunes(runErr.Error(), 500))
		} else {
			ch.sendText(userID, "✅ 任务完成\n\n"+truncateRunes(result, 3000))
		}
		// P3.1-A：任务结束（完成/停止/出错）强制刷盘——关键节点数据不丢。
		a.store.FlushAll()

		// 用户中止/上下文取消 = 退出整条轮次链（排队消息丢弃，对齐 Stop 语义）。
		if run.IsAborted() || ctx.Err() != nil {
			return
		}
		// 队列有消息 → 继续对话（本轮结束后引入）：取出队首作为下一轮任务。
		next, ok := a.dequeueSessionMsg(sessionID)
		if !ok {
			return
		}
		if next.task == "" && len(next.images) == 0 {
			continue
		}
		curText = next.task
		resumeDigest = ResumeDigestFor(sess.Snapshot(), a.memory.Load(sessionID), curText)
		uiEmit(a.ctx, "kaelvoris:queued-continue", map[string]any{
			"sessionId": sessionID, "task": curText,
		})
		ch.sendText(userID, "⏭️ 上一轮已完成，继续处理："+truncateRunes(firstLine(curText), 60))
	}
}

// wrapAsk 引擎 ask 桥：completion_result 自动通过；其余推送提问 + 文本协议应答
// （对齐 LarkChannel.wrapAsk；企微应用消息回复即触发回调路由 deliverAnswer）。
func (ch *WeComChannel) wrapAsk(run *taskRun, ctx context.Context, userID string) func(message.ClineMessage) (message.AskResponse, string) {
	return func(m message.ClineMessage) (message.AskResponse, string) {
		if m.Ask == message.AskCompletionResult {
			return message.ResponseYes, ""
		}
		// 清理残留应答（防上一轮超时/取消串扰）。
		select {
		case <-run.askCh:
		default:
		}
		if !m.Partial {
			prompt := "❓ " + truncateRunes(firstLine(m.Text), 500) + "\n\n回复 y 同意 / n 拒绝；也可直接回复内容作为补充信息。"
			ch.sendText(userID, prompt)
		}
		ch.mu.Lock()
		ch.pending[userID] = &pendingAsk{sessionID: run.id, chatID: userID}
		ch.mu.Unlock()
		defer func() {
			ch.mu.Lock()
			delete(ch.pending, userID)
			ch.mu.Unlock()
		}()
		select {
		case r := <-run.askCh:
			return r.response, r.text
		case <-ctx.Done():
			return message.ResponseNo, ""
		}
	}
}

// pushToWecom 定型 say 事件回传企微（partial 流式帧不推送，避免刷屏）。
func (ch *WeComChannel) pushToWecom(userID string, m message.ClineMessage) {
	if m.Partial {
		return
	}
	switch m.Say {
	case message.SayText:
		if t := strings.TrimSpace(m.Text); t != "" {
			ch.sendText(userID, truncateRunes(t, 3000))
		}
	case message.SayError:
		ch.sendText(userID, "⚠️ "+truncateRunes(m.Text, 800))
	}
}

// ---------- 企微消息发送 ----------

// sendText 发送文本：智能机器人 WS 长连接在线时走 aibot_send_msg（chatid
// 投递）；CorpID+CorpSecret 配置时走应用消息（touser 精确回传）；
// 否则走群机器人 Webhook（推送到配置的群，忽略 toUser）。
func (ch *WeComChannel) sendText(toUser, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// 智能机器人 WS 模式：回执/推送统一经 aibot_send_msg（toUser 实为 chatid）。
	if ai := ch.getAI(); ai != nil && ai.connected() {
		ai.sendText(toUser, text)
		return
	}
	if ch.cfg.CorpID != "" && ch.cfg.CorpSecret != "" {
		ch.sendAppMessage(toUser, text)
		return
	}
	ch.sendWebhook(text)
}

// sendWebhook 群机器人推送（免鉴权；webhook/send errcode=0 即成功）。
func (ch *WeComChannel) sendWebhook(text string) {
	body, _ := json.Marshal(map[string]interface{}{
		"msgtype": "text",
		"text":    map[string]string{"content": truncateRunes(text, 2048)},
	})
	resp, err := wecomHTTP.Post(wecomAPIBase+"/webhook/send?key="+url.QueryEscape(ch.cfg.WebhookKey),
		"application/json", bytes.NewReader(body))
	if err != nil {
		slog.Error("企微 webhook 发送失败", "name", ch.cfg.Name, "err", err)
		return
	}
	defer resp.Body.Close()
	var out struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err == nil && out.ErrCode != 0 {
		slog.Error("企微 webhook 发送失败", "name", ch.cfg.Name, "errcode", out.ErrCode, "errmsg", out.ErrMsg)
	}
}

// sendAppMessage 应用消息（message/send）：touser=成员 UserID；42001/40014
// token 失效时刷新重试一次。
func (ch *WeComChannel) sendAppMessage(toUser, text string) {
	agentID, _ := strconv.Atoi(strings.TrimSpace(ch.cfg.AgentID))
	if agentID <= 0 {
		slog.Error("企微应用消息发送失败：AgentID 未配置", "name", ch.cfg.Name)
		return
	}
	if strings.TrimSpace(toUser) == "" {
		toUser = "@all"
	}
	send := func(token string) (int, string, error) {
		body, _ := json.Marshal(map[string]any{
			"touser":  toUser,
			"msgtype": "text",
			"agentid": agentID,
			"text":    map[string]string{"content": truncateRunes(text, 2048)},
		})
		resp, err := wecomHTTP.Post(wecomAPIBase+"/message/send?access_token="+url.QueryEscape(token),
			"application/json", bytes.NewReader(body))
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		var out struct {
			ErrCode int    `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return 0, "", err
		}
		return out.ErrCode, out.ErrMsg, nil
	}
	token, err := ch.accessToken()
	if err != nil {
		slog.Error("企微 gettoken 失败", "name", ch.cfg.Name, "err", err)
		return
	}
	code, msg, err := send(token)
	// token 失效：清缓存重试一次。
	if err == nil && (code == 42001 || code == 40014) {
		ch.invalidateToken()
		if token2, err2 := ch.accessToken(); err2 == nil {
			code2, msg2, _ := send(token2)
			code, msg = code2, msg2
		}
	}
	if err == nil && code != 0 {
		slog.Error("企微应用消息发送失败", "name", ch.cfg.Name, "errcode", code, "errmsg", msg)
	}
}

// accessToken 获取/缓存企微应用 access_token（过期前 1 分钟失效，自动刷新）。
func (ch *WeComChannel) accessToken() (string, error) {
	ch.tokMu.Lock()
	defer ch.tokMu.Unlock()
	if ch.token != "" && time.Now().Before(ch.tokenUntil) {
		return ch.token, nil
	}
	req := wecomAPIBase + "/gettoken?corpid=" + url.QueryEscape(ch.cfg.CorpID) +
		"&corpsecret=" + url.QueryEscape(ch.cfg.CorpSecret)
	resp, err := wecomHTTP.Get(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Token   string `json:"access_token"`
		Expires int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ErrCode != 0 || out.Token == "" {
		return "", fmt.Errorf("gettoken 失败: %d %s", out.ErrCode, out.ErrMsg)
	}
	ttl := time.Duration(out.Expires) * time.Second
	if ttl <= 0 {
		ttl = 7200 * time.Second
	}
	ch.token = out.Token
	ch.tokenUntil = time.Now().Add(ttl - time.Minute)
	return ch.token, nil
}

// invalidateToken 清空缓存的 access_token（失效重试用）。
func (ch *WeComChannel) invalidateToken() {
	ch.tokMu.Lock()
	ch.token = ""
	ch.tokenUntil = time.Time{}
	ch.tokMu.Unlock()
}

// ---------- WXBizMsgCrypt（企微回调加解密，标准库实现） ----------

// wecomSignature 企微回调签名：sha1(sort(token, timestamp, nonce, encrypt))。
func wecomSignature(token, timestamp, nonce, encrypt string) string {
	parts := []string{token, timestamp, nonce, encrypt}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

// wecomDecrypt 解密回调密文（Base64 → AES-256-CBC 解密 → 剥离 16B 随机串
// + 4B 长度头 → 校验 receiveid）。IV = AESKey 前 16 字节，PKCS7 填充块 32。
func wecomDecrypt(encodingAESKey, b64Cipher, receiveID string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimRight(encodingAESKey, "=") + "=")
	if err != nil || len(key) != 32 {
		return "", errors.New("EncodingAESKey 非法（应为 43 位字符串）")
	}
	data, err := base64.StdEncoding.DecodeString(b64Cipher)
	if err != nil {
		return "", fmt.Errorf("密文 base64 解码失败: %w", err)
	}
	if len(data) < 32 || len(data)%aes.BlockSize != 0 {
		return "", errors.New("密文长度非法")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, key[:16]).CryptBlocks(plain, data)
	pad := int(plain[len(plain)-1])
	if pad <= 0 || pad > 32 || pad > len(plain) {
		return "", errors.New("PKCS7 填充校验失败")
	}
	plain = plain[:len(plain)-pad]
	if len(plain) < 20 {
		return "", errors.New("解密数据过短")
	}
	msgLen := int(binary.BigEndian.Uint32(plain[16:20]))
	if msgLen < 0 || 20+msgLen > len(plain) {
		return "", errors.New("消息长度非法")
	}
	msg := string(plain[20 : 20+msgLen])
	recv := string(plain[20+msgLen:])
	if receiveID != "" && recv != receiveID {
		return "", fmt.Errorf("receiveid 校验失败: got %q want %q", recv, receiveID)
	}
	return msg, nil
}

// wecomEncrypt 加密（回调应答/测试 round-trip 用）：16B 随机 + 4B 长度 +
// 消息 + receiveid，PKCS7 填充后 AES-256-CBC 加密。
func wecomEncrypt(encodingAESKey, msg, receiveID string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimRight(encodingAESKey, "=") + "=")
	if err != nil || len(key) != 32 {
		return "", errors.New("EncodingAESKey 非法（应为 43 位字符串）")
	}
	buf := make([]byte, 0, 16+4+len(msg)+len(receiveID)+32)
	buf = append(buf, make([]byte, 16)...)
	if _, err := rand.Read(buf[:16]); err != nil {
		return "", err
	}
	lens := make([]byte, 4)
	binary.BigEndian.PutUint32(lens, uint32(len(msg)))
	buf = append(buf, lens...)
	buf = append(buf, msg...)
	buf = append(buf, receiveID...)
	pad := 32 - len(buf)%32
	buf = append(buf, bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	out := make([]byte, len(buf))
	cipher.NewCBCEncrypter(block, key[:16]).CryptBlocks(out, buf)
	return base64.StdEncoding.EncodeToString(out), nil
}
