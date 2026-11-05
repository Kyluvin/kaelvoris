package desktop

// channel_lark.go 飞书通道运行时桥（doc/01-需求文档 FR-7.1）：
//
//	接收指令（WebSocket 长连接，免公网回调）→ 会话任务执行 → 消息回传 →
//	ask 文本协议审批闭环。
//
// 设计要点：
//   - 内核完全复用桌面端路径：buildKernel / buildRunEngine / makeSayBridge /
//     ApproveAsk（run.askCh 应答通道），会话 ChannelID = "lark"（对齐
//     internal/session 的通道语义）。
//   - 长连接：larksuite/oapi-sdk-go v3 ws.Client（自动重连）；事件
//     im.message.receive_v1 → OnP2MessageReceiveV1。应用需在飞书开放平台
//     开启机器人能力 + 「接收消息 im.message.receive_v1」事件 + 读消息权限。
//   - ask 审批闭环（长连接不承载卡片回调，采用文本协议）：
//     引擎 ask 挂起时回复飞书提问 + 操作提示；用户回复
//     "y"=同意 / "n"=拒绝 / 其他文本=messageResponse（对齐 ApproveAsk 三态）。
//     ask:completion_result 自动通过（结果由任务完成回执推送）。
//   - 事件双路：定型 say/ask 事件同时 EventsEmit 前端（桌面端实时旁观
//     飞书会话）+ 回传飞书聊天（text/error，partial 流式帧不推送）。
//   - AllowedUsers 白名单（open_id，逗号分隔，空 = 不限制）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"kaelvoris/internal/session"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// ChannelStatus 渠道运行状态（侧栏 ● 在线 / ○ 离线 / 未启用灰显）。
type ChannelStatus struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Online  bool   `json:"online"` // 运行时通道已连接（仅 lark）
}

// LarkChannel 飞书长连接通道实例。
type LarkChannel struct {
	app *App
	cfg ChannelConfig

	// 生命周期字段由 mu 保护：Start 在 goroutine 中赋值，Stop/reply/sendChat
	// 可能与赋值并发（修复：裸读写数据竞争）。
	api    *lark.Client
	ws     *larkws.Client
	online atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	running  map[string]bool        // chatID → 任务运行中（防重入）
	pending  map[string]*pendingAsk // chatID → 等待用户应答的 ask
	lastPush map[string]time.Time   // chatID → 上次回执时间（回执节流）
}

// larkAPI 读取 API 客户端（持锁快照，避免与 Start 赋值竞争）。
func (ch *LarkChannel) larkAPI() *lark.Client {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.api
}

// pendingAsk 一次挂起中的 ask（会话路由信息）。
type pendingAsk struct {
	sessionID string
	chatID    string
}

// ---------- 生命周期：App 侧管理 ----------

// startChannels 启动全部已启用的消息通道（startup 后台调用，不阻塞窗口）：
// lark 长连接 + wecom 本地回调服务器。
func (a *App) startChannels() {
	for _, cfg := range a.ListChannels() {
		if !cfg.Enabled {
			continue
		}
		switch cfg.Type {
		case "lark":
			a.startLarkChannel(cfg)
		case "wecom":
			a.startWecomChannel(cfg)
		case "im":
			a.startIMChannel(cfg)
		}
	}
	a.pushChannelStatus()
}

// reloadChannels 配置变更后重启通道（先停旧连接，再按新配置拉起）。
func (a *App) reloadChannels() {
	a.chanMu.Lock()
	olds := a.larkChannels
	oldWecom := a.wecomChannels
	oldIM := a.imChannels
	a.larkChannels = nil
	a.wecomChannels = nil
	a.imChannels = nil
	a.chanMu.Unlock()
	for _, ch := range olds {
		ch.Stop()
	}
	for _, ch := range oldWecom {
		ch.Stop()
	}
	for _, ch := range oldIM {
		ch.Stop()
	}
	a.startChannels()
}

// startLarkChannel 启动单个飞书通道实例。
func (a *App) startLarkChannel(cfg ChannelConfig) {
	ch := &LarkChannel{
		app:      a,
		cfg:      cfg,
		running:  map[string]bool{},
		pending:  map[string]*pendingAsk{},
		lastPush: map[string]time.Time{},
	}
	a.chanMu.Lock()
	a.larkChannels = append(a.larkChannels, ch)
	a.chanMu.Unlock()
	// panic 防护（崩溃修复）：Start 是常驻长连接循环，内部（含 ws 回调链）
	// panic 会杀死整个进程；拦截后仅记日志（渠道状态自然显示离线）。
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("飞书长连接 panic（已拦截）", "name", cfg.Name, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		ch.Start()
	}()
}

// startWecomChannel 启动单个企微通道实例（panic 防护对齐 startLarkChannel）。
func (a *App) startWecomChannel(cfg ChannelConfig) {
	ch := &WeComChannel{
		app:     a,
		cfg:     cfg,
		running: map[string]bool{},
		pending: map[string]*pendingAsk{},
	}
	a.chanMu.Lock()
	a.wecomChannels = append(a.wecomChannels, ch)
	a.chanMu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("企业微信通道 panic（已拦截）", "name", cfg.Name, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		ch.Start()
	}()
}

// startIMChannel 启动单个通用 IM 通道实例（panic 防护对齐 startLarkChannel）。
// 凭据分流：公共号三要素（PublicID+PubSecret+EID）→ 本地回调服务器（双向）；
// 仅 WebhookToken/WebhookURL → 群机器人单向推送（无接收能力，标记可推送后挂起）。
func (a *App) startIMChannel(cfg ChannelConfig) {
	ch := &IMChannel{
		app:     a,
		cfg:     cfg,
		running: map[string]bool{},
		pending: map[string]*pendingAsk{},
	}
	a.chanMu.Lock()
	a.imChannels = append(a.imChannels, ch)
	a.chanMu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("IM 通道 panic（已拦截）", "name", cfg.Name, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		ch.Start()
	}()
}

// ListChannelStatus 渠道运行状态列表（前端侧栏展示）。
func (a *App) ListChannelStatus() []ChannelStatus {
	out := make([]ChannelStatus, 0)
	for _, c := range a.ListChannels() {
		st := ChannelStatus{Type: c.Type, Name: c.Name, Enabled: c.Enabled}
		if c.Enabled && (c.Type == "lark" || c.Type == "wecom" || c.Type == "im") {
			a.chanMu.Lock()
			for _, ch := range a.larkChannels {
				if ch.cfg.Name == c.Name && ch.cfg.AppID == c.AppID {
					st.Online = ch.online.Load()
				}
			}
			for _, ch := range a.wecomChannels {
				if ch.cfg.Name == c.Name && ch.cfg.WebhookKey == c.WebhookKey {
					st.Online = ch.online.Load()
				}
			}
			for _, ch := range a.imChannels {
				if ch.cfg.Name == c.Name && ch.cfg.PublicID == c.PublicID && ch.cfg.WebhookToken == c.WebhookToken {
					st.Online = ch.online.Load()
				}
			}
			a.chanMu.Unlock()
		}
		out = append(out, st)
	}
	return out
}

// pushChannelStatus 推送渠道状态到前端（kaelvoris:channels）。
func (a *App) pushChannelStatus() {
	if a.ctx == nil {
		return
	}
	uiEmit(a.ctx, "kaelvoris:channels", a.ListChannelStatus())
}

// ---------- 生命周期：通道实例 ----------

// Start 建立飞书连接（阻塞；内部自动重连。goroutine 中运行）。
func (ch *LarkChannel) Start() {
	ctx, cancel := context.WithCancel(context.Background())

	// 生命周期字段赋值持锁（修复：与 Stop/reply/sendChat 并发裸读写竞争）。
	ch.mu.Lock()
	ch.ctx = ctx
	ch.cancel = cancel
	ch.api = lark.NewClient(ch.cfg.AppID, ch.cfg.AppSecret)
	ch.mu.Unlock()

	dispatcher := larkdispatcher.NewEventDispatcher("", "")
	dispatcher.OnP2MessageReceiveV1(ch.onMessageReceive)

	ch.mu.Lock()
	ch.ws = larkws.NewClient(ch.cfg.AppID, ch.cfg.AppSecret,
		larkws.WithEventHandler(dispatcher),
		larkws.WithLogLevel(larkcore.LogLevelError),
		larkws.WithOnReady(func() {
			ch.online.Store(true)
			ch.app.pushChannelStatus()
		}),
		larkws.WithOnDisconnected(func() {
			ch.online.Store(false)
			ch.app.pushChannelStatus()
		}),
		larkws.WithOnError(func(err error) {
			ch.online.Store(false)
			ch.app.pushChannelStatus()
		}),
	)
	ws := ch.ws
	ch.mu.Unlock()
	// Start 阻塞运行；Cancel 后返回（返回值仅含关闭错误，忽略）。
	// 注意：阻塞调用不持锁（Stop 需要先拿到锁调用 cancel）。
	_ = ws.Start(ctx)
}

// Stop 停止通道（断开长连接；进行中的任务随会话 cancel 语义由用户停止）。
func (ch *LarkChannel) Stop() {
	ch.online.Store(false)
	ch.mu.Lock()
	cancel := ch.cancel
	ch.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ---------- 事件接收 ----------

// atMentionRe text 消息中的 @ 占位（@机器人/@成员）。
var atMentionRe = regexp.MustCompile(`@_user_\d+`)

// onMessageReceive 飞书消息入口：应答路由 或 新任务。
func (ch *LarkChannel) onMessageReceive(_ context.Context, event *larkim.P2MessageReceiveV1) error {
	defer func() { _ = recover() }() // 单事件异常不拖垮长连接
	if event == nil || event.Event == nil || event.Event.Message == nil || event.Event.Sender == nil {
		return nil
	}
	ev := event.Event
	msg := ev.Message
	if msg.MessageId == nil || msg.ChatId == nil || msg.MessageType == nil || msg.Content == nil {
		return nil
	}
	// 仅真人消息（过滤机器人回环）。
	if ev.Sender.SenderType == nil || *ev.Sender.SenderType != "user" {
		return nil
	}
	// 白名单（AllowedUsers 逗号分隔 open_id；空 = 不限制）。
	if allow := strings.TrimSpace(ch.cfg.AllowedUsers); allow != "" {
		openID := ""
		if ev.Sender.SenderId != nil && ev.Sender.SenderId.OpenId != nil {
			openID = *ev.Sender.SenderId.OpenId
		}
		ok := false
		for _, u := range strings.Split(allow, ",") {
			if strings.TrimSpace(u) == openID {
				ok = true
				break
			}
		}
		if !ok {
			return nil
		}
	}

	messageID := *msg.MessageId
	chatID := *msg.ChatId
	text := larkTextFromContent(*msg.Content)
	if *msg.MessageType != "text" {
		ch.replyText(messageID, chatID, "当前仅支持文本消息（图片/文件解析在后续版本开放）。")
		return nil
	}
	if text == "" {
		return nil
	}

	// 运行中会话：路由为 ask 应答（y/n/补充文本，对齐 ApproveAsk 三态）。
	ch.mu.Lock()
	p := ch.pending[chatID]
	busy := ch.running[chatID]
	ch.mu.Unlock()
	if p != nil {
		ch.deliverAnswer(p, text)
		return nil
	}
	if busy {
		// 运行中不停止：消息入队（FIFO），本轮任务结束后 runLarkTask 自动继续对话。
		if ch.app.enqueueSessionMsg("lark-"+chatID, text, nil) {
			ch.replyText(messageID, chatID, "⏳ 已加入队列（当前任务完成后自动继续）："+truncateRunes(firstLine(text), 60))
			return nil
		}
		// 竞态：会话恰在本检查后结束运行 → 回落正常发送。
	}
	go ch.runLarkTaskSafe(chatID, messageID, text)
	return nil
}

// deliverAnswer 把文本应答路由到挂起中的 ask（复用 ApproveAsk 多会话路由）。
func (ch *LarkChannel) deliverAnswer(p *pendingAsk, text string) {
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

// normalizeAnswer 应答文本归一：y/yes/同意 → yes；n/no/拒绝 → no；其他 → 原文小写（走 messageResponse）。
func normalizeAnswer(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "y", "yes", "同意", "是", "ok", "批准":
		return "yes"
	case "n", "no", "拒绝", "否", "不":
		return "no"
	default:
		return strings.ToLower(strings.TrimSpace(t))
	}
}

// larkTextFromContent 解析 text 消息 content JSON 并清理 @ 占位。
func larkTextFromContent(content string) string {
	var c struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &c); err != nil {
		return ""
	}
	t := atMentionRe.ReplaceAllString(c.Text, "")
	return strings.TrimSpace(t)
}

// ---------- 任务执行：复用桌面端内核路径 ----------

// runLarkTaskSafe 任务 goroutine 的 panic 防护包装（崩溃修复）：事件入口
// onMessageReceive 已有单事件 recover，但 go 出去的任务执行体是独立
// goroutine——其未捕获 panic 仍会杀死整个进程。拦截后回执飞书错误提示。
func (ch *LarkChannel) runLarkTaskSafe(chatID, messageID, text string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("飞书任务 panic（已拦截，进程保持运行）",
				"chat", chatID, "panic", r, "stack", string(debug.Stack()))
			ch.sendChat(chatID, "❌ 任务内部异常终止（已恢复）："+truncateRunes(fmt.Sprint(r), 500))
		}
	}()
	ch.runLarkTask(chatID, messageID, text)
}

// runLarkTask 以会话（ChannelID="lark"）运行一条指令；事件回传飞书 + 推送前端。
func (ch *LarkChannel) runLarkTask(chatID, messageID, text string) {
	a := ch.app
	sessionID := "lark-" + chatID

	// 内核组件惰性构建（与 runTask 一致；未配置凭据时直接回执错误）。
	a.kmu.Lock()
	needBuild := a.modeMgr == nil || a.skillMgr == nil
	a.kmu.Unlock()
	if needBuild {
		if err := a.buildKernel(); err != nil {
			ch.replyText(messageID, chatID, "❌ "+err.Error())
			return
		}
	}
	a.kmu.Lock()
	modeMgr, skillMgr := a.modeMgr, a.skillMgr
	a.kmu.Unlock()

	// 岗位分配收口（doc/09-设计方案 Phase 2-①，§4.1）：四渠道建会话前统一
	// ResolveAssignment 解析绑定（固定/降级/岗位池/裸跑），结果固化进会话快照；
	// 解析报错（员工不存在/池空）回执飞书并终止本轮（§2 池空不静默降级）。
	assign, ok := ch.resolveChannelAssignment()
	if !ok {
		ch.replyText(messageID, chatID, "❌ 岗位分配失败："+assign.errMsg)
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
			ChannelID:  "lark",
			Title:      truncateTitle(text, 40),
			Mode:       a.GetMode(),
			Workspace:  a.cfgWorkspace(),  // 会话关联工作区（渠道会话同样隔离项目目录）
			EmployeeID: ch.cfg.EmployeeID, // §10.3 渠道绑定唯一数字员工（新会话首次绑定后固定）
			CreatedAt:  now,
		}
		// §4.1 收口：解析结果固化进新会话（EmployeeID + 岗位快照，生命周期不重选）。
		applyChannelAssignSnapshot(sess, assign)
	}
	if sess.Snapshot().Workspace == "" {
		sess.SetWorkspace(a.cfgWorkspace()) // 旧会话升级兼容
	}
	// 错误/完成态清除（与桌面端 runTask 一致）：新任务开始重置上一轮标记。
	sess.SetError(false)
	sess.SetDone(false)
	a.store.Save(sess)
	uiEmit(a.ctx, "kaelvoris:sessions", a.store.List())

	run := &taskRun{id: sessionID, workspace: sess.Snapshot().Workspace, employeeID: sess.Snapshot().EmployeeID, todo: tools.NewTodoStore(), askCh: make(chan askReply, 1)}
	run.mcpProbe = text // MCP 惰性加载：飞书任务文本作为激活信号
	// §10 降级留痕（会话可见）：以 say text 写入会话历史 + 前端推送（run 已建）。
	emitAssignNotice(a, sess, run, sessionID, assign.notice)
	if snap := sess.Snapshot(); len(snap.Todos) > 0 {
		run.todo.Set(snap.Todos)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	ch.mu.Lock()
	ch.running[chatID] = true
	ch.mu.Unlock()
	defer func() {
		if run.cleanup != nil {
			run.cleanup() // 数字员工网关审计文件关闭等（后进先出）
		}
		cancel()
		ch.mu.Lock()
		delete(ch.pending, chatID)
		delete(ch.running, chatID)
		ch.mu.Unlock()
		a.runsMu.Lock()
		delete(a.runs, sessionID)
		a.runsMu.Unlock()
		a.registry.Remove(sessionID)
		a.pushChannelStatus()
		// 会话级状态修复（同 runTask）：任务结束恒报本会话 false，不报全局
		// IsRunning()——否则并发任务会把已完成的飞书会话卡在「运行中」。
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
		// 回执：本轮任务已受理。
		ch.replyText(messageID, chatID, "🚀 已开始处理："+truncateRunes(firstLine(curText), 60))

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

		// say 双路桥：桌面端事件桥（历史留痕 + 前端推送）+ 飞书回传（定型 text/error）。
		desktopSay := a.makeSayBridge(sess, run, sessionID)
		say := func(m message.ClineMessage) {
			desktopSay(m)
			ch.pushToLark(chatID, m)
		}

		result, runErr := engine.RunInMode(ctx, a.GetMode(), curText, false, 0, say, ch.wrapAsk(run, ctx, chatID))
		flushMCP()
		if runErr != nil {
			ch.sendChat(chatID, "❌ 任务失败："+truncateRunes(runErr.Error(), 500))
		} else {
			ch.sendChat(chatID, "✅ 任务完成\n\n"+truncateRunes(result, 3000))
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
		ch.sendChat(chatID, "⏭️ 上一轮已完成，继续处理："+truncateRunes(firstLine(curText), 60))
	}
}

// wrapAsk 引擎 ask 桥：completion_result 自动通过；其余推送提问 + 文本协议应答。
func (ch *LarkChannel) wrapAsk(run *taskRun, ctx context.Context, chatID string) func(message.ClineMessage) (message.AskResponse, string) {
	return func(m message.ClineMessage) (message.AskResponse, string) {
		// 任务完成确认：自动通过（结果回执由 runLarkTask 发送）。
		if m.Ask == message.AskCompletionResult {
			return message.ResponseYes, ""
		}
		// 清理残留应答（防上一轮超时/取消串扰）。
		select {
		case <-run.askCh:
		default:
		}
		// 提示用户可应答（非 partial ask 才提示；partial 流式帧不应出现于 ask）。
		if !m.Partial {
			prompt := "❓ " + truncateRunes(firstLine(m.Text), 500) + "\n\n回复 y 同意 / n 拒绝；也可直接回复内容作为补充信息。"
			ch.sendChat(chatID, prompt)
		}
		ch.mu.Lock()
		ch.pending[chatID] = &pendingAsk{sessionID: run.id, chatID: chatID}
		ch.mu.Unlock()
		defer func() {
			ch.mu.Lock()
			delete(ch.pending, chatID)
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

// pushToLark 定型 say 事件回传飞书（partial 流式帧不推送，避免刷屏）。
func (ch *LarkChannel) pushToLark(chatID string, m message.ClineMessage) {
	if m.Partial {
		return
	}
	switch {
	case m.Type == message.TypeAsk:
		// ask 提示由 wrapAsk 统一发送（带操作提示）。
	case m.Say == message.SayText:
		if t := strings.TrimSpace(m.Text); t != "" {
			ch.sendChat(chatID, truncateRunes(t, 3000))
		}
	case m.Say == message.SayError:
		ch.sendChat(chatID, "⚠️ "+truncateRunes(m.Text, 800))
	}
}

// ---------- 飞书消息发送 ----------

// replyText 引用回复；失败降级为普通会话消息。
func (ch *LarkChannel) replyText(messageID, chatID, text string) {
	api := ch.larkAPI()
	if api == nil {
		return
	}
	content, _ := json.Marshal(map[string]string{"text": truncateRunes(text, 3000)})
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()
	resp, err := api.Im.V1.Message.Reply(context.Background(), req)
	if err == nil && resp != nil && resp.Success() {
		return
	}
	ch.sendChat(chatID, text)
}

// sendChat 发送文本到会话（chat_id 维度；P2P/群聊通用）。
func (ch *LarkChannel) sendChat(chatID, text string) {
	api := ch.larkAPI()
	if api == nil || strings.TrimSpace(text) == "" {
		return
	}
	content, _ := json.Marshal(map[string]string{"text": truncateRunes(text, 3000)})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()
	resp, err := api.Im.V1.Message.Create(context.Background(), req)
	_ = resp
	if err != nil {
		// 发送失败仅静默（无 UI 通道可上报；事件桥侧已留痕）。
		return
	}
}

// ---------- 小工具 ----------

// firstLine 取首行非空文本。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// truncateRunes 按 rune 截断（结果回执/工具输出限长）。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
