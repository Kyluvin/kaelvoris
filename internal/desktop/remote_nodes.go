package desktop

// remote_nodes.go 桌面端「远程会话」能力（桌面 ⇆ kaelvoris-server ⇆ 其他 PC 节点）：
//
//	此前 P-Remote 的方向是「本机作为节点被远端指挥」（remote_panel.go：/ws/agent 通道）；
//	本文件补齐反向链路：桌面端以「客户端」身份连接服务中心（/ws/client 通道，对齐
//	remote_account.go 的账号认证路径），拉取在线节点列表，把任务发布到远端 PC 节点执行，
//	事件流/审批/续聊/取消在桌面端实时渲染 —— 即「PC 端当手机用」：远程指挥另一台 PC。
//
//	复用服务端既有协议（internal/remote/protocol.go），服务端零改动：
//	  - 发布任务：create.task → task.created {task}（Task.SessionID = remote-<id>）
//	  - 事件流：task.event {taskId, sessionId, kind, text, partial}（kind=text/tool/error/ask）
//	  - 终态：task.result {taskId, sessionId, ok, result, error, canceled?}
//	  - 续聊：chat.send {nodeId?, sessionId, text}（运行中由节点侧入队）
//	  - 审批：ask.reply {sessionId, response, text} → 节点 run.askCh（与本地审批同源）
//	  - 取消：task.cancel {taskId}（服务端标记 canceled + 转发节点停止执行）
//	  - 历史：get.messages {sessionId} → messages {sessionID, messages, todos}
//
//	事件路由：kaelvoris:remote_task（sessionId/kind/text/partial/ok/canceled）、
//	kaelvoris:remote_nodes（connected/nodes）。前端按 remote:<sessionID> 维度渲染
//	远程会话流（main.js remote_task_ui 段），复用本地会话同款消息渲染管线。

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"kaelvoris/internal/remote"
)

// remoteTaskEvent 推送前端的事件（kaelvoris:remote_task 载荷）。
type remoteTaskEvent struct {
	SessionID string `json:"sessionId"`
	TaskID    string `json:"taskId,omitempty"`
	NodeID    string `json:"nodeId,omitempty"`
	Kind      string `json:"kind"` // text/tool/error/ask/started/result
	Text      string `json:"text,omitempty"`
	Partial   bool   `json:"partial,omitempty"`
	OK        bool   `json:"ok,omitempty"` // kind=result：是否成功
	// Own 本机相关标记（经 rnOwnTask 过滤放行 = 本机执行/本机发起；前端据此
	// 清理历史残留时不会误删「本机主动发布到其他节点」的会话，2026-11）。
	Own      bool `json:"own,omitempty"`
	Canceled bool `json:"canceled,omitempty"` // kind=result：是否被取消
}

// remoteNodesEvent 推送前端的事件（kaelvoris:remote_nodes 载荷）。
type remoteNodesEvent struct {
	Connected bool          `json:"connected"`
	Online    bool          `json:"online"` // 远程功能开启且账号已登录
	Nodes     []remote.Node `json:"nodes,omitempty"`
	// SelfNodeID 本机（执行端）节点身份：前端据此清理「他机会话」残留
	//（执行端仅显示本机执行/本机发起的远程任务，2026-11 用户需求）。
	SelfNodeID string `json:"selfNodeId,omitempty"`
}

// rnState 远程会话连接状态（App.rn 惰性初始化；生命周期对齐账号登录态）。
type rnState struct {
	a *App // 反向引用（推送前端事件；登录态读取）

	mu       sync.Mutex
	ws       *websocket.Conn
	online   bool
	user     string // 当前连接凭据（配置变化判定用）
	pass     string
	connURL  string
	nodes    []remote.Node     // 最近一次节点列表（ListRemoteNodes 兜底数据源）
	sessNode map[string]string // sessionID → nodeID（chat.send 补路由用）
	sessTask map[string]string // sessionID → taskID（取消按钮用）

	// writeMu gorilla 连接单写者保护：请求-响应帧（request）与主动拉取帧
	//（send）可能并发写；不加锁会帧交错损坏协议（对齐 remote.Client.writeMu）。
	writeMu sync.Mutex

	// pending 请求-响应配对：envelope.ID → 应答通道（request/resolve）。
	pendMu sync.Mutex
	pend   map[string]chan remote.Envelope

	stop    chan struct{} // 关闭 = 断开（读循环退出 + 重连循环退出）
	running bool          // 连接循环是否存活
}

// ensureRn 惰性初始化远程会话状态（读路径与写路径统一收口）。
func (a *App) ensureRn() *rnState {
	a.rnMu.Lock()
	defer a.rnMu.Unlock()
	if a.rn == nil {
		a.rn = &rnState{a: a, pend: map[string]chan remote.Envelope{}}
	}
	return a.rn
}

// ---------- 前端绑定 API ----------

// ListRemoteNodes 在线节点列表（新建远程会话的节点选择器数据源）。
// 连接在线时同步请求一次刷新（响应经 kaelvoris:remote_nodes 推送），并返回缓存快照。
func (a *App) ListRemoteNodes() []remote.Node {
	st := a.ensureRn()
	if st.isConnected() {
		st.send("", remote.TypeListNodes, nil)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.nodes == nil {
		return []remote.Node{}
	}
	return st.nodes
}

// RemoteSendTaskResult 远程任务发布结果（RemoteSendTask 返回值，json 键对齐前端/rnRemember）。
type RemoteSendTaskResult struct {
	TaskID    string `json:"taskId"`
	SessionID string `json:"sessionId"`
	NodeID    string `json:"nodeId,omitempty"`
}

// RemoteSendTask 发布任务到远程节点（nodeID 空 = 服务端自动选在线节点；mode 空 = 节点当前模式）。
// 返回 (result, error)：Wails 绑定运行时（boundMethod.go Call）只支持 ≤2 个输出值，
// 旧形态 (taskID, sessionID, error) 三个返回值会静默返回 null（前端报「服务端未返回会话 ID」），
// 故收拢为单结构体 + error；前端经 r.taskId / r.sessionId 读取，与 mock 桩一致。
func (a *App) RemoteSendTask(nodeID, text, mode string) (*RemoteSendTaskResult, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("任务内容不能为空")
	}
	st := a.ensureRn()
	if !a.rnAccountReady() {
		return nil, fmt.Errorf("请先登录远程账号（顶栏右上角）")
	}
	if !st.isConnected() {
		return nil, fmt.Errorf("未连接远程服务中心，请稍候重试")
	}
	ct := remote.CreateTask{Text: text, Mode: mode}
	if nodeID != "" {
		ct.NodeID = nodeID
	}
	e, err := st.request("", remote.TypeCreateTask, ct, 10*time.Second)
	if err != nil {
		return nil, err
	}
	switch e.Type {
	case remote.TypeTaskCreated:
		var p struct {
			Task remote.Task `json:"task"`
		}
		_ = e.Decode(&p)
		if p.Task.ID == "" || p.Task.SessionID == "" {
			return nil, fmt.Errorf("服务端响应异常（任务信息不完整）")
		}
		// 登记映射：事件流/终态/续聊/取消都需要 sessionID → taskID/nodeID。
		a.rnRemember(p.Task.SessionID, p.Task.NodeID, p.Task.ID)
		// 会话记录补记执行节点/模式（会话自有配置持久化）：RemoteSendTask 建的
		// 会话在服务端（remote-<taskID>），本机 store 无记录——补一条最小记录，
		// 重启后切回该会话 OpenSession 仍能读到 NodeID/Mode，选择器跟随会话
		// 而不是残留上一会话的值（Get 失败不致命：映射内存侧已登记）。
		if s, err := a.store.Get(p.Task.SessionID); err == nil && s != nil {
			s.SetNodeID(p.Task.NodeID)
			s.SetMode(p.Task.Mode)
			a.store.Save(s)
		}
		a.pushRemoteTask(remoteTaskEvent{
			SessionID: p.Task.SessionID, TaskID: p.Task.ID, NodeID: p.Task.NodeID,
			Kind: "started", Text: p.Task.Text, Own: true,
		})
		slog.Info("远程任务已发布", "task", p.Task.ID, "session", p.Task.SessionID, "node", p.Task.NodeID)
		return &RemoteSendTaskResult{TaskID: p.Task.ID, SessionID: p.Task.SessionID, NodeID: p.Task.NodeID}, nil
	case remote.TypeOpResult:
		var op remote.OpResult
		_ = e.Decode(&op)
		if op.Message != "" {
			return nil, fmt.Errorf("%s", op.Message)
		}
		return nil, fmt.Errorf("任务发布失败")
	default:
		return nil, fmt.Errorf("服务端响应异常（%s）", e.Type)
	}
}

// RemoteContinueTask 远程会话续聊（chat.send；节点运行中会入队，空闲则新轮次）。
func (a *App) RemoteContinueTask(sessionID, text string) error {
	text = strings.TrimSpace(text)
	if sessionID == "" || text == "" {
		return fmt.Errorf("会话与消息内容不能为空")
	}
	st := a.ensureRn()
	if !st.isConnected() {
		return fmt.Errorf("未连接远程服务中心")
	}
	cs := remote.ChatSend{SessionID: sessionID, Text: text, NodeID: a.rnNodeOf(sessionID)}
	e, err := st.request("", remote.TypeChatSend, cs, 10*time.Second)
	if err != nil {
		return err
	}
	// chat.send 受理无应答（服务端直接路由节点），仅参数错误/无归属节点时回 op.result；
	// 空 id 响应不会进 pending，此处超时即视为受理成功 —— 改用 send + 超时容忍语义。
	_ = e
	return nil
}

// RemoteApproveTaskAsk 远程会话审批应答（response: yesButtonClicked/noButtonClicked/messageResponse）。
// 服务端按 sessionId 反查任务归属节点 → server.ask_reply → 节点 run.askCh。
func (a *App) RemoteApproveTaskAsk(sessionID, response, text string) error {
	if sessionID == "" {
		return fmt.Errorf("会话不能为空")
	}
	st := a.ensureRn()
	if !st.isConnected() {
		return fmt.Errorf("未连接远程服务中心")
	}
	ar := remote.AskReply{SessionID: sessionID, Response: response, Text: text}
	// ask.reply 路由成功无应答（fire-and-forget）；失败回 op.result（错误消息）。
	// 用短超时 send：路由失败场景由服务端 op.result 广播兜底（前端 toast）。
	st.send("", remote.TypeAskReply, ar)
	return nil
}

// RemoteCancelTask 取消远程任务（服务端立即标记 canceled + 转发节点停止执行；
// 终态以 task.result 广播为准，前端据 kind=result&canceled 收敛 UI）。
func (a *App) RemoteCancelTask(taskID string) error {
	if taskID == "" {
		return fmt.Errorf("缺少任务 ID")
	}
	st := a.ensureRn()
	if !st.isConnected() {
		return fmt.Errorf("未连接远程服务中心")
	}
	// task.cancel 受理即回 op.result（服务端已同步标记）。
	e, err := st.request("", remote.TypeTaskCancel, remote.TaskCancel{TaskID: taskID}, 10*time.Second)
	if err != nil {
		return err
	}
	var op remote.OpResult
	_ = e.Decode(&op)
	return nil
}

// RemoteSessionMessages 拉取远程会话历史（服务端镜像立即应答；节点在线时服务端
// 同时路由 get.messages → agent.messages 覆盖广播，前端下次打开拿到最新历史）。
// 返回 {sessionID, messages, todos?}；messages 为 ClineMessage JSON 数组。
func (a *App) RemoteSessionMessages(sessionID string) (map[string]interface{}, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("会话不能为空")
	}
	st := a.ensureRn()
	if !st.isConnected() {
		return nil, fmt.Errorf("未连接远程服务中心")
	}
	e, err := st.request("", remote.TypeGetMessages, remote.GetMessages{SessionID: sessionID}, 12*time.Second)
	if err != nil {
		return nil, err
	}
	if e.Type != remote.TypeMessages {
		var op remote.OpResult
		_ = e.Decode(&op)
		if op.Message != "" {
			return nil, fmt.Errorf("%s", op.Message)
		}
		return nil, fmt.Errorf("拉取历史失败")
	}
	var mr remote.MessagesResponse
	if err := e.Decode(&mr); err != nil {
		return nil, fmt.Errorf("解析历史失败: %w", err)
	}
	out := map[string]interface{}{
		"sessionID": mr.SessionID,
		"messages":  mr.Messages,
	}
	if mr.Todos != nil {
		out["todos"] = mr.Todos
	}
	return out, nil
}

// ---------- 连接器生命周期（登录态联动） ----------

// rnConnect 启动远程会话连接（登录成功/自动重登成功后调用；幂等；
// 凭据或服务端地址变化时先断旧连接再重启）。
func (a *App) rnConnect() {
	st := a.ensureRn()
	cfg := a.loadRemoteConfig()
	if !cfg.Enabled || cfg.ServerURL == "" {
		return
	}
	a.accMu.Lock()
	loggedIn := a.accLoggedIn
	a.accMu.Unlock()
	if !loggedIn {
		return
	}
	sa := loadStoredAccount()
	if sa.User == "" || sa.Pass == "" {
		return
	}
	user, pass := sa.User, sa.Pass

	st.mu.Lock()
	if st.running {
		same := st.user == user && st.pass == pass && st.connURL == cfg.ServerURL
		st.mu.Unlock()
		if same {
			return // 已在运行且配置未变
		}
	} else {
		st.mu.Unlock()
	}

	a.rnDisconnect() // 幂等断开（未运行时无操作；配置变化时断旧连接）
	st = a.ensureRn()
	st.mu.Lock()
	st.running = true
	st.user = user
	st.pass = pass
	st.connURL = cfg.ServerURL
	st.stop = make(chan struct{})
	stop := st.stop
	st.mu.Unlock()
	go st.loop(stop)
	slog.Info("远程会话通道启动", "server", cfg.ServerURL, "user", user)
}

// rnDisconnect 停止远程会话连接（退出登录时调用；幂等）。
func (a *App) rnDisconnect() {
	st := a.ensureRn()
	st.mu.Lock()
	running := st.running
	if running {
		st.running = false
		close(st.stop)
		if st.ws != nil {
			_ = st.ws.Close()
		}
		st.ws = nil
		st.online = false
	}
	st.mu.Unlock()
}

// rnAccountReady 账号是否已登录（发布任务前置校验）。
func (a *App) rnAccountReady() bool {
	a.accMu.Lock()
	defer a.accMu.Unlock()
	return a.accLoggedIn
}

// rnNotifyStatusChanged 登录态变化钩子（remote_account.go 登录/退出/自动重登后调用）：
// 登录 → 启动通道并推送节点列表；退出 → 断开 + 清空会话映射 + 推送空列表。
func (a *App) rnNotifyStatusChanged(loggedIn bool) {
	if loggedIn {
		go a.rnConnect()
	} else {
		a.rnDisconnect()
		st := a.ensureRn()
		st.mu.Lock()
		st.sessNode = nil
		st.sessTask = nil
		st.nodes = nil
		st.mu.Unlock()
	}
	a.pushRemoteNodesEvent()
}

// ---------- 连接主循环 ----------

// loop 连接主循环（拨号 → auth → 读循环；断线指数退避重连直到 stop）。
func (st *rnState) loop(stop chan struct{}) {
	delay := 3 * time.Second
	for {
		select {
		case <-stop:
			return
		default:
		}
		err := st.connectOnce(stop)
		if err == errRnStopped {
			return
		}
		if err != nil {
			slog.Warn("远程会话通道断开（将重连）", "err", err)
		}
		st.setOnline(false, rnErrString(err))
		select {
		case <-stop:
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > 30*time.Second {
			delay = 3 * time.Second
		}
	}
}

// errRnStopped 主动停止标志（loop 识别后退出，不再重连）。
var errRnStopped = fmt.Errorf("remote session client stopped")

// rnErrString 错误转字符串（nil → 空；与 internal/remote.errString 同构，
// 主包不能引用其未导出符号故此处独立实现）。
func rnErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// connectOnce 单次连接：拨号 → auth（user+pass）→ 读循环。
// 认证失败（密码被改/账号被删）返回错误，由 loop 退避重试（账号侧修复后自愈）。
func (st *rnState) connectOnce(stop chan struct{}) error {
	cfg := st.a.loadRemoteConfig()
	sa := loadStoredAccount()
	user, pass := sa.User, sa.Pass
	if user == "" || pass == "" {
		return fmt.Errorf("凭据缺失（请重新登录账号）")
	}

	dialer := websocket.Dialer{HandshakeTimeout: 8 * time.Second}
	ws, _, err := dialer.Dial(wsURLFromClient(cfg.ServerURL), nil)
	if err != nil {
		return fmt.Errorf("连接失败: %w", err)
	}
	// 认证（首条 auth；对齐 web/admin、web/mobile 登录路径）。
	_ = ws.SetWriteDeadline(time.Now().Add(8 * time.Second))
	if err := ws.WriteJSON(remote.NewEnvelope("", remote.TypeAuth, remote.Auth{User: user, Pass: pass})); err != nil {
		ws.Close()
		return fmt.Errorf("发送认证失败: %w", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(8 * time.Second))
	var e remote.Envelope
	if err := ws.ReadJSON(&e); err != nil {
		ws.Close()
		return fmt.Errorf("认证响应超时: %w", err)
	}
	switch e.Type {
	case remote.TypeAuthOK:
	case remote.TypeAuthErr:
		var ep remote.ErrorPayload
		_ = e.Decode(&ep)
		ws.Close()
		if ep.Message != "" {
			return fmt.Errorf("%s", ep.Message)
		}
		return fmt.Errorf("认证失败")
	default:
		ws.Close()
		return fmt.Errorf("服务端响应异常（%s）", e.Type)
	}
	_ = ws.SetReadDeadline(time.Time{})

	st.mu.Lock()
	st.ws = ws
	st.mu.Unlock()
	st.setOnline(true, "")

	// 读循环：分发服务端消息；断开时清连接句柄（send/request 立即失败）。
	for {
		select {
		case <-stop:
			ws.Close()
			return errRnStopped
		default:
		}
		_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		var env remote.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			ws.Close()
			st.mu.Lock()
			if st.ws == ws {
				st.ws = nil
			}
			st.mu.Unlock()
			st.setOnline(false, rnErrString(err))
			return fmt.Errorf("连接断开: %w", err)
		}
		st.handleEnvelope(env)
	}
}

// ---------- 信封处理 ----------

// handleEnvelope 分发服务端消息：请求-响应配对优先，其余按类型广播处理。
func (st *rnState) handleEnvelope(e remote.Envelope) {
	// 带 id 帧优先送 pending 通道（request 等待方消费）。
	if e.ID != "" {
		st.pendMu.Lock()
		ch, ok := st.pend[e.ID]
		st.pendMu.Unlock()
		if ok {
			select {
			case ch <- e:
			default:
			}
			return
		}
	}
	switch e.Type {
	case remote.TypeNodes:
		var nodes []remote.Node
		_ = e.Decode(&nodes)
		st.mu.Lock()
		st.nodes = nodes
		st.mu.Unlock()
		st.a.pushRemoteNodesEvent()
	case remote.TypeTaskEvent:
		var ev remote.TaskEvent
		if err := e.Decode(&ev); err == nil && ev.SessionID != "" {
			// 归属过滤（2026-11 用户需求：执行端仅看本机执行/本机发起的任务）：
			// 服务端 task.event 已带 nodeId（AddTaskEvent 反查任务归属填充），
			// 用它 + 本机会话映射双路判定；他机执行的任务不推送前端（仍是广播，
			// 移动端/管理端照常接收全部）。
			if !st.a.rnOwnTask(ev.SessionID, ev.NodeID) {
				return
			}
			re := remoteTaskEvent{
				SessionID: ev.SessionID,
				TaskID:    ev.TaskID,
				NodeID:    st.a.rnNodeOf(ev.SessionID),
				Kind:      ev.Kind,
				Text:      ev.Text,
				Partial:   ev.Partial,
				Own:       true,
			}
			if re.TaskID == "" {
				re.TaskID = ev.TaskID
			}
			// 节点绑定回填（会话自有执行节点随事件流恢复）：sessNode 是内存态，
			// 桌面端重启后映射清空——切回远程会话节点选择器会错回本机、续聊
			// 路由丢失。服务端 task.event 携带归属 nodeId，到达即回填映射。
			if ev.NodeID != "" {
				st.a.rnRemember(ev.SessionID, ev.NodeID, ev.TaskID)
			}
			st.a.pushRemoteTask(re)
		}
	case remote.TypeTaskResult:
		// 终态：{taskId, sessionId, ok, result, error, canceled?, task?}。
		var p struct {
			TaskID    string      `json:"taskId"`
			SessionID string      `json:"sessionId"`
			OK        bool        `json:"ok"`
			Result    string      `json:"result"`
			Error     string      `json:"error"`
			Canceled  bool        `json:"canceled"`
			Task      remote.Task `json:"task"`
		}
		_ = e.Decode(&p)
		if p.SessionID == "" {
			p.SessionID = st.a.rnSessionOfTask(p.TaskID)
		}
		if p.SessionID == "" {
			return
		}
		// 归属过滤（与 task.event 同语义）：仅本机执行/本机发起的任务才推前端。
		// nodeId 优先读 task 对象（终态广播携带），旧广播回退会话映射。
		nodeID := p.Task.NodeID
		if nodeID == "" {
			nodeID = st.a.rnNodeOf(p.SessionID)
		}
		// 节点绑定回写（同 task.event：终态广播携带的归属 nodeId 回填映射，
		// 覆盖「重启后 sessNode 内存态丢失」的恢复盲区）。
		if nodeID != "" {
			st.a.rnRemember(p.SessionID, nodeID, p.TaskID)
		}
		if !st.a.rnOwnTask(p.SessionID, nodeID) {
			return
		}
		st.a.pushRemoteTask(remoteTaskEvent{
			SessionID: p.SessionID,
			TaskID:    p.TaskID,
			NodeID:    nodeID,
			Kind:      "result",
			Text:      p.Result,
			OK:        p.OK,
			Canceled:  p.Canceled,
			Own:       true,
		})
	case remote.TypeOpResult:
		// 无 id 的 op.result（路由失败广播）→ toast 提示；带 id 的已被 pending 消费。
		var op remote.OpResult
		_ = e.Decode(&op)
		if e.ID == "" && !op.OK && op.Message != "" {
			st.a.pushToast("远程操作失败: " + op.Message)
		}
	case remote.TypeServerError:
		var ep remote.ErrorPayload
		_ = e.Decode(&ep)
		if ep.Message != "" {
			st.a.pushToast("远程服务错误: " + ep.Message)
		}
	}
}

// ---------- 连接状态与收发 ----------

// isConnected 通道是否在线。
func (st *rnState) isConnected() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.online && st.ws != nil
}

// setOnline 状态切换（记日志 + 状态变化推送前端节点列表）。
func (st *rnState) setOnline(v bool, errStr string) {
	st.mu.Lock()
	changed := st.online != v
	st.online = v
	st.mu.Unlock()
	if changed {
		if v {
			slog.Info("远程会话通道已连接")
		} else if errStr != "" && errStr != errRnStopped.Error() {
			slog.Info("远程会话通道离线", "err", errStr)
		}
		st.a.pushRemoteNodesEvent()
	}
}

// send 发送信封（线程安全；离线静默丢弃；写锁序列化）。
func (st *rnState) send(id, typ string, payload interface{}) {
	st.mu.Lock()
	ws := st.ws
	st.mu.Unlock()
	if ws == nil {
		return
	}
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteJSON(remote.NewEnvelope(id, typ, payload)); err != nil {
		slog.Warn("远程会话通道发送失败", "type", typ, "err", err)
	}
}

// request 发送请求并等待同 id 应答（超时/离线返回错误）。
// 注意：服务端对 chat.send / ask.reply 不回帧（直接路由节点），此类消息
// 调用方应使用 send（超时容忍语义），request 仅用于有应答的操作。
func (st *rnState) request(id, typ string, payload interface{}, timeout time.Duration) (remote.Envelope, error) {
	if id == "" {
		id = fmt.Sprintf("rn-%d-%d", time.Now().UnixNano(), time.Now().UnixNano()%1000)
	}
	if !st.isConnected() {
		return remote.Envelope{}, fmt.Errorf("未连接远程服务中心")
	}
	ch := make(chan remote.Envelope, 1)
	st.pendMu.Lock()
	st.pend[id] = ch
	st.pendMu.Unlock()
	defer func() {
		st.pendMu.Lock()
		delete(st.pend, id)
		st.pendMu.Unlock()
	}()
	st.send(id, typ, payload)
	select {
	case e := <-ch:
		return e, nil
	case <-time.After(timeout):
		return remote.Envelope{}, fmt.Errorf("请求超时：%s", typ)
	}
}

// ---------- 会话映射（sessionID ⇆ taskID/nodeID） ----------

// rnRemember 登记会话归属（task.created/事件流到达时）。
func (a *App) rnRemember(sessionID, nodeID, taskID string) {
	if sessionID == "" {
		return
	}
	st := a.ensureRn()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessNode == nil {
		st.sessNode = map[string]string{}
	}
	if st.sessTask == nil {
		st.sessTask = map[string]string{}
	}
	if nodeID != "" {
		st.sessNode[sessionID] = nodeID
	}
	if taskID != "" {
		st.sessTask[sessionID] = taskID
	}
}

// rnNodeOf 会话归属节点（chat.send 路由兜底）。
func (a *App) rnNodeOf(sessionID string) string {
	st := a.ensureRn()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessNode == nil {
		return ""
	}
	return st.sessNode[sessionID]
}

// rnTaskOf 会话关联任务 ID（取消按钮）。
func (a *App) rnTaskOf(sessionID string) string {
	st := a.ensureRn()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessTask == nil {
		return ""
	}
	return st.sessTask[sessionID]
}

// rnSessionOfTask 任务 → 会话 ID（终态广播缺 sessionId 时反查）。
func (a *App) rnSessionOfTask(taskID string) string {
	st := a.ensureRn()
	st.mu.Lock()
	defer st.mu.Unlock()
	for sid, tid := range st.sessTask {
		if tid == taskID {
			return sid
		}
	}
	return ""
}

// rnOwnTask 判定某任务会话是否属于本机视角（执行端过滤规则，2026-11）：
//   - 服务端广播携带的 nodeId == 本机节点身份（effectiveNodeIdentity）→ 本机执行；
//   - 会话已在本机映射（rnRemember 登记，本机发起/本机曾参与的会话）→ 本机相关；
//   - 其余（他机执行、他端发布的无关会话）→ 不渲染（仍是广播，移动端照常全量）。
//
// 本机身份读 remote.json 派生（与连接器注册同源，多开槽位带后缀也一致）。
func (a *App) rnOwnTask(sessionID, nodeID string) bool {
	if sessionID == "" {
		return false
	}
	// 1) 本机会话映射命中（本机发起/本机登记的会话）。
	st := a.ensureRn()
	st.mu.Lock()
	_, known := st.sessTask[sessionID]
	st.mu.Unlock()
	if known {
		return true
	}
	// 2) 服务端广播 nodeId 与本机身份一致。
	if nodeID != "" {
		self, _, _ := a.effectiveNodeIdentity()
		if self != "" && nodeID == self {
			return true
		}
	}
	// 3) 会话记录持久化的执行节点与本机身份一致（重启后恢复盲区：
	//    sessNode 内存映射已空，但本机曾发布到某节点且事件流未再到达的
	//    历史会话，按会话记录 NodeID 判归本机相关）。
	// 零值 App（store 未初始化）防御：测试/极端场景下 a.store 可为 nil，
	// 判归兜底失败直接回 false，不 panic。
	if a.store != nil {
		if s, err := a.store.Get(sessionID); err == nil && s != nil {
			n := strings.TrimSpace(s.Snapshot().NodeID)
			if n != "" {
				st.mu.Lock()
				st.sessNode[sessionID] = n
				st.mu.Unlock()
				return true
			}
		}
	}
	return false
}

// ---------- 前端推送 ----------

// pushRemoteNodesEvent 推送节点列表与连接状态（kaelvoris:remote_nodes）。
func (a *App) pushRemoteNodesEvent() {
	if a.ctx == nil {
		return
	}
	st := a.ensureRn()
	st.mu.Lock()
	ev := remoteNodesEvent{
		Connected: st.online && st.ws != nil,
		Nodes:     st.nodes,
	}
	st.mu.Unlock()
	a.accMu.Lock()
	ev.Online = a.accLoggedIn
	a.accMu.Unlock()
	// 本机节点身份（执行端过滤/清理他机会话的依据；未配置远程时为空）。
	selfID, _, _ := a.effectiveNodeIdentity()
	ev.SelfNodeID = selfID
	uiEmit(a.ctx, "kaelvoris:remote_nodes", ev)
}

// pushRemoteTask 推送远程任务事件（kaelvoris:remote_task；前端按 sessionId 路由渲染）。
func (a *App) pushRemoteTask(ev remoteTaskEvent) {
	if a.ctx == nil {
		return
	}
	uiEmit(a.ctx, "kaelvoris:remote_task", ev)
}
