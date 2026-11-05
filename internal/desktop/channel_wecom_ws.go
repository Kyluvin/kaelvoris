package desktop

// channel_wecom_ws.go 企业微信「智能机器人」（AI Bot）WebSocket 长连接模式：
//
//	免公网回调：本地直连 wss://openws.work.weixin.qq.com，凭据 = BotID + Secret
//	（企微管理后台创建「智能机器人」后获取），接入体验对齐飞书长连接（无需
//	公网 IP / 备案 / 回调配置）。协议按官方智能机器人 WS 协议实现（帧结构、
//	认证、心跳、回复 ACK 均与 wecom-aibot-go 参考实现对齐）。
//
// 协议要点：
//   - 帧：JSON 文本帧 {cmd, headers:{req_id}, body, errcode, errmsg}；
//   - 认证：{cmd:"aibot_subscribe", body:{bot_id, secret}}，服务端以同 req_id 回帧，
//     errcode==0 即认证成功 → 启动 30s ping 心跳（连续 2 次无 ACK 判定断线重连）；
//   - 消息回调：cmd=aibot_msg_callback / aibot_event_callback，须以同 req_id 回
//     aibot_respond_msg（约 5s ACK 窗口）；respond 不支持 msgtype="text"
//     （errcode 40008），纯文本回复以 stream finish=true 表达，content 为空 =
//     静默占位 ACK（回执统一走 aibot_send_msg，避免重复消息）；
//   - 主动发送：aibot_send_msg {chatid, msgtype:"markdown", markdown:{content}}；
//   - 群聊仅推送 @机器人 的消息（服务端过滤，无需本地 @ 检测）；
//   - 回调 5s 内未确认会重推：msgid（字符串）去重 + 同 req_id respond 占位；
//   - 断线重连：1s 起步退避（上限 30s）无限重试；认证被拒不重试（凭据错误）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// errWecomAuth 认证被服务端拒绝（凭据错误；不可重试，避免无效凭据无限重连）。
var errWecomAuth = errors.New("企微智能机器人认证被拒绝")

const (
	// wecomAIBotWSURL 企微智能机器人长连接接入点。
	wecomAIBotWSURL = "wss://openws.work.weixin.qq.com"

	wecomWsCmdSubscribe     = "aibot_subscribe"      // 认证/订阅
	wecomWsCmdHeartbeat     = "ping"                 // 心跳
	wecomWsCmdResponse      = "aibot_respond_msg"    // 回调回复（同 req_id）
	wecomWsCmdSendMsg       = "aibot_send_msg"       // 主动发送（按 chatid）
	wecomWsCmdMsgCallback   = "aibot_msg_callback"   // 消息回调
	wecomWsCmdEventCallback = "aibot_event_callback" // 事件回调

	wecomHeartbeatInterval = 30 * time.Second // 心跳间隔
	wecomMaxMissedPong     = 2                // 连续无 ACK 容忍次数
	wecomAuthAckTimeout    = 10 * time.Second // 认证应答超时
	wecomReplyAckTimeout   = 5 * time.Second  // 回复/发送 ACK 等待（对齐官方 SDK）
	wecomMaxReconnectDelay = 30 * time.Second // 重连退避上限
)

// ---------- 协议帧 ----------

// wecomWsHeaders 帧头（req_id 用于应答匹配与回调回复）。
type wecomWsHeaders struct {
	ReqID string `json:"req_id"`
}

// wecomWsFrame 出站帧。
type wecomWsFrame struct {
	Cmd     string         `json:"cmd,omitempty"`
	Headers wecomWsHeaders `json:"headers"`
	Body    any            `json:"body,omitempty"`
}

// wecomWsFrameRaw 入站帧（Body 保留原始 JSON，按 cmd 二次解码）。
type wecomWsFrameRaw struct {
	Cmd     string          `json:"cmd,omitempty"`
	Headers wecomWsHeaders  `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
	ErrCode int             `json:"errcode,omitempty"`
	ErrMsg  string          `json:"errmsg,omitempty"`
}

// wecomAIBotFrom 消息发送者。
type wecomAIBotFrom struct {
	UserID string `json:"userid"`
}

// wecomAIBotMsg 消息回调体（aibot_msg_callback.body）。
type wecomAIBotMsg struct {
	MsgID      string         `json:"msgid"`
	AIBotID    string         `json:"aibotid,omitempty"`
	ChatID     string         `json:"chatid,omitempty"`
	ChatType   string         `json:"chattype,omitempty"` // group | single
	From       wecomAIBotFrom `json:"from"`
	CreateTime int64          `json:"create_time,omitempty"`
	MsgType    string         `json:"msgtype,omitempty"`
	Text       struct {
		Content string `json:"content"`
	} `json:"text"`
}

// wecomStreamBody 回复体：aibot_respond_msg 不支持 msgtype="text"（errcode 40008），
// 纯文本以 stream finish=true 表达；content 为空 = 静默占位 ACK。
type wecomStreamBody struct {
	MsgType string `json:"msgtype"`
	Stream  struct {
		ID      string `json:"id"`
		Finish  bool   `json:"finish,omitempty"`
		Content string `json:"content,omitempty"`
	} `json:"stream"`
}

// wecomSendBody 主动发送体（aibot_send_msg，markdown 文本，按 chatid 投递）。
type wecomSendBody struct {
	ChatID   string `json:"chatid"`
	MsgType  string `json:"msgtype"`
	Markdown struct {
		Content string `json:"content"`
	} `json:"markdown"`
}

// wecomGenReqID 生成 "<前缀>_<随机hex>" 请求 ID（服务端按前缀识别认证/心跳应答）。
func wecomGenReqID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// ---------- 长连接运行时 ----------

// wecomAIBotConn 智能机器人长连接运行时（挂在 WeComChannel 上）。
// 读循环独占读；写经 wmu 串行化（gorilla/websocket 并发写不安全）。
type wecomAIBotConn struct {
	ch  *WeComChannel
	url string

	mu   sync.Mutex
	conn *websocket.Conn
	wmu  sync.Mutex

	ackMu sync.Mutex
	acks  map[string]chan wecomAckResult

	heartbeatInterval time.Duration
	maxMissedPong     int
	replyAckTimeout   time.Duration
}

// wecomAckResult 服务端应答（帧或错误；连接断开时以 err 唤醒全部等待者）。
type wecomAckResult struct {
	frame wecomWsFrameRaw
	err   error
}

func newAIBotConn(ch *WeComChannel) *wecomAIBotConn {
	return &wecomAIBotConn{
		ch:                ch,
		url:               wecomAIBotWSURL,
		acks:              map[string]chan wecomAckResult{},
		heartbeatInterval: wecomHeartbeatInterval,
		maxMissedPong:     wecomMaxMissedPong,
		replyAckTimeout:   wecomReplyAckTimeout,
	}
}

func (w *wecomAIBotConn) setConn(c *websocket.Conn) {
	w.mu.Lock()
	w.conn = c
	w.mu.Unlock()
}

func (w *wecomAIBotConn) getConn() *websocket.Conn {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn
}

// connected 连接是否就绪。
func (w *wecomAIBotConn) connected() bool { return w.getConn() != nil }

// run 长连接主循环：拨号 → 认证 → 心跳 + 读帧；断线退避重连（ctx 取消即退出）。
func (w *wecomAIBotConn) run(ctx context.Context) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if attempt > 0 {
			delay := time.Duration(attempt) * time.Second
			if delay > wecomMaxReconnectDelay {
				delay = wecomMaxReconnectDelay
			}
			slog.Info("企微智能机器人等待重连", "name", w.ch.cfg.Name, "attempt", attempt, "delay", delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		authed, err := w.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errWecomAuth) {
			slog.Error("企微智能机器人认证被拒（停止重连，请检查 Bot ID/Secret）", "name", w.ch.cfg.Name, "err", err)
			return
		}
		if err != nil {
			slog.Warn("企微智能机器人连接断开", "name", w.ch.cfg.Name, "err", err)
		}
		w.ch.online.Store(false)
		w.ch.app.pushChannelStatus()
		if authed {
			attempt = 1 // 上轮认证成功过：退避重新起步
		} else {
			attempt++
		}
	}
}

// session 单轮连接：拨号 → 认证 → 心跳 + 读循环，直至断开或 ctx 取消。
// 返回 authed 表示本轮是否完成过认证（重连退避是否重新起步）。
func (w *wecomAIBotConn) session(ctx context.Context) (bool, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, w.url, nil)
	if err != nil {
		return false, fmt.Errorf("连接 %s 失败: %w", w.url, err)
	}
	w.setConn(conn)
	defer func() {
		_ = conn.Close()
		w.setConn(nil)
	}()

	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				select {
				case readErr <- err:
				default:
				}
				return
			}
			w.handleFrame(data)
		}
	}()

	// 认证：aibot_subscribe + bot_id/secret；服务端以同 req_id 回帧，errcode==0 即成功。
	authID := wecomGenReqID(wecomWsCmdSubscribe)
	ack := w.registerAck(authID)
	if err := w.sendFrame(wecomWsFrame{
		Cmd:     wecomWsCmdSubscribe,
		Headers: wecomWsHeaders{ReqID: authID},
		Body:    map[string]string{"bot_id": w.ch.cfg.BotID, "secret": w.ch.cfg.BotSecret},
	}); err != nil {
		w.unregisterAck(authID)
		return false, err
	}
	select {
	case r := <-ack:
		if r.err != nil {
			return false, r.err
		}
		if r.frame.ErrCode != 0 {
			return false, fmt.Errorf("%w: errcode=%d errmsg=%s", errWecomAuth, r.frame.ErrCode, r.frame.ErrMsg)
		}
	case <-time.After(wecomAuthAckTimeout):
		w.unregisterAck(authID)
		return false, errors.New("认证应答超时")
	case <-ctx.Done():
		return false, ctx.Err()
	}

	w.ch.online.Store(true)
	w.ch.app.pushChannelStatus()
	slog.Info("企微智能机器人长连接已认证", "name", w.ch.cfg.Name)

	// 心跳：30s ping；连续 maxMissedPong 次无应答主动断开触发重连。
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go w.heartbeat(hbCtx)

	select {
	case err := <-readErr:
		w.failAcks(fmt.Errorf("连接断开: %w", err))
		return true, err
	case <-ctx.Done():
		w.failAcks(errors.New("通道已停止"))
		return true, nil
	}
}

// heartbeat 心跳循环：每 interval 发一帧 ping（req_id 前缀 ping_），连续
// maxMissedPong 次无应答则关闭连接（读循环退出 → session 返回 → 重连）。
func (w *wecomAIBotConn) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(w.heartbeatInterval)
	defer ticker.Stop()
	missed := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if missed >= w.maxMissedPong {
				slog.Warn("企微智能机器人连续心跳无应答，主动断开重连", "name", w.ch.cfg.Name, "missed", missed)
				if conn := w.getConn(); conn != nil {
					_ = conn.Close()
				}
				return
			}
			missed++
			id := wecomGenReqID(wecomWsCmdHeartbeat)
			ack := w.registerAck(id)
			if err := w.sendFrame(wecomWsFrame{Cmd: wecomWsCmdHeartbeat, Headers: wecomWsHeaders{ReqID: id}}); err != nil {
				return // 写失败 = 连接已坏，读循环很快退出
			}
			select {
			case r := <-ack:
				if r.err == nil && r.frame.ErrCode == 0 {
					missed = 0
				}
			case <-time.After(w.heartbeatInterval):
				// 本轮无应答，missed 保留，下轮复核
			case <-ctx.Done():
				return
			}
		}
	}
}

// handleFrame 入站帧分发：回调帧 → 消息路由；其余按 req_id 唤醒等待者
// （认证/心跳/回复/发送的 ACK 均回显原 req_id）。
func (w *wecomAIBotConn) handleFrame(data []byte) {
	var f wecomWsFrameRaw
	if err := json.Unmarshal(data, &f); err != nil {
		slog.Warn("企微智能机器人帧解析失败", "name", w.ch.cfg.Name, "err", err)
		return
	}
	switch f.Cmd {
	case wecomWsCmdMsgCallback:
		w.handleMsgCallback(f)
	case wecomWsCmdEventCallback:
		w.handleEventCallback(f)
	default:
		if id := f.Headers.ReqID; id != "" {
			w.resolveAck(id, f)
		}
	}
}

// handleMsgCallback 消息回调 → 渠道路由（去重/白名单/ask/队列/新任务）。
func (w *wecomAIBotConn) handleMsgCallback(f wecomWsFrameRaw) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("企微智能机器人消息处理 panic（已拦截，连接保持）", "name", w.ch.cfg.Name, "panic", r)
		}
	}()
	var msg wecomAIBotMsg
	if err := json.Unmarshal(f.Body, &msg); err != nil {
		slog.Warn("企微智能机器人消息解析失败", "name", w.ch.cfg.Name, "err", err)
		return
	}
	w.ch.onAIBotMessage(f.Headers.ReqID, msg)
}

// handleEventCallback 事件回调（enter_chat / template_card_event / feedback_event）。
// 暂不进入任务链；不回 ACK（重推无害，对齐参考实现）。
func (w *wecomAIBotConn) handleEventCallback(f wecomWsFrameRaw) {
	var ev struct {
		From  wecomAIBotFrom `json:"from"`
		Event struct {
			EventType string `json:"eventtype"`
			EventKey  string `json:"event_key,omitempty"`
		} `json:"event"`
	}
	_ = json.Unmarshal(f.Body, &ev)
	slog.Info("企微智能机器人事件", "name", w.ch.cfg.Name,
		"eventtype", ev.Event.EventType, "event_key", ev.Event.EventKey, "user", ev.From.UserID)
}

// ---------- 帧收发与应答等待 ----------

// sendFrame 序列化并发送一帧（写串行化）。
func (w *wecomAIBotConn) sendFrame(f wecomWsFrame) error {
	conn := w.getConn()
	if conn == nil {
		return errors.New("长连接未建立")
	}
	payload, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("帧序列化失败: %w", err)
	}
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return fmt.Errorf("帧发送失败: %w", err)
	}
	return nil
}

func (w *wecomAIBotConn) registerAck(id string) chan wecomAckResult {
	c := make(chan wecomAckResult, 1)
	w.ackMu.Lock()
	w.acks[id] = c
	w.ackMu.Unlock()
	return c
}

func (w *wecomAIBotConn) unregisterAck(id string) {
	w.ackMu.Lock()
	delete(w.acks, id)
	w.ackMu.Unlock()
}

// resolveAck 服务端应答帧到达时唤醒等待者（无等待者则忽略）。
func (w *wecomAIBotConn) resolveAck(id string, f wecomWsFrameRaw) bool {
	w.ackMu.Lock()
	ch, ok := w.acks[id]
	if ok {
		delete(w.acks, id)
	}
	w.ackMu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- wecomAckResult{frame: f}:
	default:
	}
	return true
}

// failAcks 连接断开：以 err 唤醒全部等待者（防挂死）。
func (w *wecomAIBotConn) failAcks(err error) {
	w.ackMu.Lock()
	chs := make([]chan wecomAckResult, 0, len(w.acks))
	for id, ch := range w.acks {
		chs = append(chs, ch)
		delete(w.acks, id)
	}
	w.ackMu.Unlock()
	for _, ch := range chs {
		select {
		case ch <- wecomAckResult{err: err}:
		default:
		}
	}
}

// request 发送一帧并等待同 req_id 的服务端应答（认证/回复/发送通用）。
func (w *wecomAIBotConn) request(cmd, reqID string, body any, timeout time.Duration) error {
	if w.getConn() == nil {
		return errors.New("长连接未建立")
	}
	ack := w.registerAck(reqID)
	if err := w.sendFrame(wecomWsFrame{Cmd: cmd, Headers: wecomWsHeaders{ReqID: reqID}, Body: body}); err != nil {
		w.unregisterAck(reqID)
		return err
	}
	select {
	case r := <-ack:
		if r.err != nil {
			return r.err
		}
		if r.frame.ErrCode != 0 {
			return fmt.Errorf("errcode=%d errmsg=%s", r.frame.ErrCode, r.frame.ErrMsg)
		}
		return nil
	case <-time.After(timeout):
		w.unregisterAck(reqID)
		return errors.New("等待应答超时")
	}
}

// respond 以同 req_id 回复回调帧（stream finish=true；respond 不支持 msgtype=text）。
func (w *wecomAIBotConn) respond(reqID, content string) error {
	var body wecomStreamBody
	body.MsgType = "stream"
	body.Stream.ID = wecomGenReqID("stream")
	body.Stream.Finish = true
	body.Stream.Content = content
	return w.request(wecomWsCmdResponse, reqID, body, wecomReplyAckTimeout)
}

// ackRespond 异步占位应答（回调须在约 5s 内 respond 同 req_id，否则企微重推；
// 异步化避免阻塞消息路由 goroutine）。
func (w *wecomAIBotConn) ackRespond(reqID, content string) {
	if reqID == "" {
		return
	}
	go func() {
		defer func() { _ = recover() }()
		if err := w.respond(reqID, content); err != nil {
			slog.Warn("企微智能机器人回调应答失败", "name", w.ch.cfg.Name, "reqID", reqID, "err", err)
		}
	}()
}

// sendText 主动发送文本（aibot_send_msg，markdown，按 chatid 投递；单聊/群聊
// 回调均携带 chatid）。
func (w *wecomAIBotConn) sendText(chatID, text string) {
	var body wecomSendBody
	body.ChatID = chatID
	body.MsgType = "markdown"
	body.Markdown.Content = strings.TrimSpace(text)
	if err := w.request(wecomWsCmdSendMsg, wecomGenReqID(wecomWsCmdSendMsg), body, w.replyAckTimeout); err != nil {
		slog.Error("企微智能机器人发送失败", "name", w.ch.cfg.Name, "chatID", chatID, "err", err)
	}
}

// ---------- WeComChannel 集成 ----------

// startAIBot 智能机器人长连接模式主循环（阻塞；断线自动重连，Stop 取消退出）。
func (ch *WeComChannel) startAIBot(ctx context.Context) {
	ai := newAIBotConn(ch)
	ch.mu.Lock()
	ch.ai = ai
	ch.mu.Unlock()
	ai.run(ctx)
	ch.online.Store(false)
	ch.app.pushChannelStatus()
}

// getAI 读取长连接运行时（nil = 未启用智能机器人模式）。
func (ch *WeComChannel) getAI() *wecomAIBotConn {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.ai
}

// seenAIBotMsg 智能机器人 msgid（字符串）去重；簿记超 1024 条时清理 10 分钟前
// 旧记录（respond 未在 5s 内确认时企微会重推同一回调）。
func (ch *WeComChannel) seenAIBotMsg(id string) bool {
	if id == "" {
		return false
	}
	ch.seenStrMu.Lock()
	defer ch.seenStrMu.Unlock()
	if ch.seenStr == nil {
		ch.seenStr = map[string]time.Time{}
	}
	if t, ok := ch.seenStr[id]; ok && time.Since(t) < 10*time.Minute {
		return true
	}
	if len(ch.seenStr) > 1024 {
		now := time.Now()
		for k, t := range ch.seenStr {
			if now.Sub(t) >= 10*time.Minute {
				delete(ch.seenStr, k)
			}
		}
	}
	ch.seenStr[id] = time.Now()
	return false
}

// onAIBotMessage 智能机器人消息入口（对齐 onMessage 的路由语义）：
// msgid 去重 → 白名单 → ask 应答 / 运行中队列 / 新任务。
// 首帧回执经 respond ACK（5s 窗口内必达）；后续回执统一走 sendText（aibot_send_msg）。
func (ch *WeComChannel) onAIBotMessage(reqID string, msg wecomAIBotMsg) {
	userID := strings.TrimSpace(msg.From.UserID)
	if userID == "" {
		return
	}
	// msgid 去重（企微重推防护；先于任何可见回复，避免重复提示）。
	if ch.seenAIBotMsg(msg.MsgID) {
		return
	}
	text := strings.TrimSpace(msg.Text.Content)
	if msg.MsgType != "text" || text == "" {
		ch.ai.ackRespond(reqID, "当前仅支持文本消息（图片/文件解析在后续版本开放）。")
		return
	}
	// 白名单（成员 UserID，逗号分隔；空 = 不限制）。
	if allow := strings.TrimSpace(ch.cfg.AllowedUsers); allow != "" {
		ok := false
		for _, u := range strings.Split(allow, ",") {
			if strings.TrimSpace(u) == userID {
				ok = true
				break
			}
		}
		if !ok {
			return
		}
	}
	// 会话路由键：群聊按 chatid 隔离（多人共享会话，对齐 lark），单聊回退 userID。
	routeKey := strings.TrimSpace(msg.ChatID)
	if routeKey == "" {
		routeKey = userID
	}

	ch.mu.Lock()
	p := ch.pending[routeKey]
	busy := ch.running[routeKey]
	ch.mu.Unlock()
	if p != nil {
		// ask 应答路由（y/n/补充文本）；静默 ACK，结果随引擎输出回传。
		ch.ai.ackRespond(reqID, "")
		ch.deliverAnswer(p, text)
		return
	}
	if busy {
		// 运行中不停止：消息入队（FIFO），本轮任务结束后自动继续对话。
		if ch.app.enqueueSessionMsg("wecom-"+routeKey, text, nil) {
			ch.ai.ackRespond(reqID, "⏳ 已加入队列（当前任务完成后自动继续）：\n"+truncateRunes(firstLine(text), 60))
			return
		}
		// 竞态：会话恰在本检查后结束运行 → 回落新任务。
	}
	// 新任务：respond ACK 内联受理回执（后续回执经 sendText 下发，故首轮回执跳过）。
	ch.ai.ackRespond(reqID, "🚀 已开始处理：\n"+truncateRunes(firstLine(text), 60))
	go ch.runWecomTaskSafe(routeKey, text, false)
}
