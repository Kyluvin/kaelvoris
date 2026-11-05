package remote

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"kaelvoris/internal/version"
)

// Server 远程服务 HTTP/WebSocket 服务器。
type Server struct {
	hub  *Hub
	up   websocket.Upgrader
	mux  *http.ServeMux
	mu   sync.Mutex
	srv  *http.Server
	addr string

	// 全局中间件链（use 注册，Start 时包在 mux 外层；先注册者最外层）。
	// 现为空 —— 五项中间件待办（访问日志/panic 恢复/Host 白名单/Bearer/IP 限流）
	// 落地时经 use 按位插入，见 middleware.go。
	globalMW []Middleware

	// REST 凭据（SetRESTCredentials 注入）：/api/nodes /api/tasks 等敏感接口
	// 要求 Basic 认证（统一账号）或 Bearer/查询参数 token（clientToken）。
	// 空值 = 不校验（本地纯内网部署可显式不配置以保持旧行为）。
	restUser  string
	restPass  string
	restToken string

	// 知识库根目录（SetDocsDir 注入，服务端部署目录下 doc/）。
	// 空串 = 未启用：/api/docs* 返回 404。见 docs.go。
	docsDir string

	// 生成图片托管（绘画链路，见 images.go）：
	imageDir    string            // 图片存储目录（空 = /api/images 404；/img/ 回落 hub 目录 images/）
	agentTokens map[string]string // 节点令牌快照（X-Node-Token 上传鉴权；SetAgentTokens 注入）

	// 插件市场（market.go）：技能/插件/MCP 包发布与分发存储目录（空 = 未启用）。
	marketDir string
}

// NewServer 创建服务器（hub 提供全部业务能力；webDir 静态托管 web/ 目录，可空）。
func NewServer(hub *Hub, webDir string) *Server {
	s := &Server{hub: hub}
	s.up = websocket.Upgrader{
		CheckOrigin:     func(r *http.Request) bool { return true }, // 移动端/管理端跨域直连
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
	}
	mux := http.NewServeMux()
	// 全部路由统一经 s.route 挂载（middleware.go）：per-route 鉴权以中间件
	// 实参接入，后续全局中间件经 use 注册、Start 时包在 mux 外层。
	s.route(mux, "/ws/agent", s.handleAgentWS)
	s.route(mux, "/ws/client", s.handleClientWS)
	// REST 敏感接口一律经 restAuth（凭据每请求动态读取；
	// SetRESTCredentials 全空 = 直通，保持旧行为）。
	s.route(mux, "/api/health", s.handleHealth)
	s.route(mux, "/api/nodes", s.handleNodes, s.restAuth)
	s.route(mux, "/api/tasks", s.handleTasks, s.restAuth)
	// 知识库（doc/ 管理，APK H5「知识」页数据源；鉴权见 docs.go docAuth）。
	// 未 SetDocsDir 时 docAuth 直接 404，注册无害。
	s.route(mux, "/api/docs", s.handleDocsList, s.docAuth(false))
	s.route(mux, "/api/docs/read", s.handleDocsRead, s.docAuth(false))
	s.route(mux, "/api/docs/write", s.handleDocsWrite, s.docAuth(true))
	s.route(mux, "/api/docs/delete", s.handleDocsDelete, s.docAuth(true))
	// 生成图片托管（绘画链路）：/api/images 节点上传；/img/ 公开只读。见 images.go。
	s.registerImageRoutes(mux, hub)
	// IM 中继公网回调入口（/im/c/<fp>、/im/g/<fp>/<hookID>；im_relay.go）。
	s.RegisterIMRelayRoutes(mux)
	// 插件市场（技能/插件/MCP 包发布与分发；market.go。未 SetMarketDir 时 404）。
	s.registerMarketRoutes(mux)
	if webDir != "" {
		// 各入口独立根目录：StripPrefix 去掉前缀（含尾斜杠）后为空路径，
		// FileServer 根目录下存在 index.html 时自动返回，避免回退到 web 根列表。
		s.route(mux, "/admin/", http.StripPrefix("/admin", noStoreFS(filepath.Join(webDir, "admin"))).ServeHTTP)
		s.route(mux, "/m/", http.StripPrefix("/m", noStoreFS(filepath.Join(webDir, "mobile"))).ServeHTTP)
		s.route(mux, "/", s.handleWebRoot(webDir))
	}
	s.mux = mux
	return s
}

// noStore 禁止浏览器缓存静态资源，确保 H5/管理端更新后不回退旧版。
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

// noStoreFS 包装静态文件服务，所有响应附加 no-store 头。
func noStoreFS(root string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		http.FileServer(http.Dir(root)).ServeHTTP(w, r)
	})
}

// handleWebRoot web 根目录静态托管：根路径重定向到管理后台，其余 no-store 直出。
func (s *Server) handleWebRoot(webDir string) http.HandlerFunc {
	fs := http.FileServer(http.Dir(webDir))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/admin/", http.StatusFound)
			return
		}
		noStore(w)
		fs.ServeHTTP(w, r)
	}
}

// SetRESTCredentials 配置 REST 敏感接口（/api/nodes /api/tasks）鉴权凭据。
// 在 NewServer 之后、Start 之前调用；user/pass 为统一账号（与 WS auth 同一账号表），
// token 为 clientToken。三者全空 = 关闭 REST 鉴权（保持旧行为）。
func (s *Server) SetRESTCredentials(user, pass, token string) {
	s.mu.Lock()
	s.restUser, s.restPass, s.restToken = user, pass, token
	s.mu.Unlock()
}

// restAuth REST 接口鉴权中间件：支持
//   - Basic Auth（统一账号 user/pass，与 WS auth 同一账号表）；
//   - Bearer <token> / X-Access-Token / ?token=<token>（clientToken）。
//
// 校验失败 → 401（WWW-Authenticate: Basic 提示），响应体脱敏不含账号信息。
func (s *Server) restAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		user, pass, token := s.restUser, s.restPass, s.restToken
		s.mu.Unlock()
		if user == "" && pass == "" && token == "" {
			next.ServeHTTP(w, r) // 未配置凭据：开放（旧行为）
			return
		}
		if u, p, ok := r.BasicAuth(); ok && u != "" && p != "" &&
			subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1 &&
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		tok := r.Header.Get("X-Access-Token")
		if tok == "" {
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				tok = strings.TrimPrefix(auth, "Bearer ")
			}
		}
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if tok != "" && token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(token)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		noStore(w)
		w.Header().Set("WWW-Authenticate", "Basic realm=\"kaelvoris\"")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": false, "error": "unauthorized: 需要 Basic 认证（统一账号）或访问令牌",
		})
	})
}

// Start 启动 HTTP 服务（阻塞）。
func (s *Server) Start(addr string) error {
	s.mu.Lock()
	s.addr = addr
	handler := Chain(s.mux, s.globalMW...) // 全局中间件包在全部路由外层（先注册者最外层）
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}
	srv := s.srv
	s.mu.Unlock()
	slog.Info("远程服务启动", "addr", addr)
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Stop 优雅关闭。
func (s *Server) Stop() {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// ---------- REST ----------

// handleHealth 健康探活（服务端可达性；负载均衡/监控定时探测）。
// 加固（2026-10 事故）：此前直接调 s.hub.ListNodes()（内部 h.mu.RLock），
// h.mu 被异常持有时探活无限阻塞 → 监控判定服务死。改为带 2s 超时取锁：
// 超时返回 503（探活失败），探活路径永不挂死。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	nodes, ok := s.hub.ListNodesTimeout(healthProbeMuTimeout)
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "hub 主锁超时（疑似死锁/锁泄漏），探活失败")
		return
	}
	writeJSONOK(w, map[string]interface{}{
		"ok":      true,
		"version": version.Number,
		"nodes":   len(nodes),
		"time":    time.Now().UnixMilli(),
	})
}

// healthProbeMuTimeout /api/health 取 h.mu 的最大等待时长（超过回 503）。
const healthProbeMuTimeout = 2 * time.Second

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	writeJSONOK(w, s.hub.ListNodes())
}

// handleTasks 任务列表查询：支持过滤参数（全部可选，向后兼容——不带参数时
// 返回最新 200 条，与旧行为一致）：
//
//	GET /api/tasks?nodeId=node-default&status=failed&search=构建&since=1690000000000&limit=50
//
// 非法 status 返回 400（响应体不含敏感信息）。
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	f := &TaskFilter{
		NodeID: r.URL.Query().Get("nodeId"),
		Status: r.URL.Query().Get("status"),
		Search: r.URL.Query().Get("search"),
	}
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "since 参数无效：需要毫秒时间戳")
			return
		}
		f.Since = n
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSONError(w, http.StatusBadRequest, "limit 参数无效：需要正整数")
			return
		}
		f.Limit = n
	}
	if err := f.Validate(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONOK(w, s.hub.QueryTasks(f))
}

// writeJSONError REST 错误响应（统一脱敏：不含账号/凭据信息）。
func writeJSONError(w http.ResponseWriter, code int, msg string) {
	noStore(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": msg})
}

func writeJSONOK(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": data})
}

// ---------- WebSocket：PC 节点（agent） ----------
func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	ws, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	// panic 兜底：WS 处理器 panic 必须拦截（http handler goroutine 崩溃会带崩整个 HTTP 服务）。
	defer func() {
		if r := recover(); r != nil {
			slog.Error("节点 WS 连接处理 panic（已拦截）", "panic", r)
			_ = ws.WriteJSON(NewEnvelope("", TypeServerError, ErrorPayload{Message: "服务异常，请重试"}))
		}
	}()
	var conn *agentConn
	defer func() {
		if conn != nil {
			s.hub.UnregisterAgent(conn.nodeID, conn)
		}
	}()

	// 首条必须是 agent.register。
	ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	var e Envelope
	if err := ws.ReadJSON(&e); err != nil {
		return
	}
	if e.Type != TypeAgentRegister {
		_ = ws.WriteJSON(NewEnvelope("", TypeServerError, ErrorPayload{Message: "首条消息必须为 agent.register"}))
		return
	}
	var reg AgentRegister
	if err := e.Decode(&reg); err != nil {
		_ = ws.WriteJSON(NewEnvelope("", TypeServerError, ErrorPayload{Message: "注册参数无效"}))
		return
	}
	// 去令牌化（2026-10）：注册凭据改统一账号（User/Pass，与 /ws/client auth 同一
	// accounts.json 验证源）。User/Pass 均空 = 匿名注册（节点可上线展示但不可被指挥）。
	// 凭据非空但校验失败 → 拒绝注册（防冒名顶替他账号节点）。
	role, ok := s.hub.AuthAgentForRegister(reg.User, reg.Pass)
	if !ok {
		_ = ws.WriteJSON(NewEnvelope("", TypeServerError, ErrorPayload{Message: "注册凭据无效（账号或密码错误）"}))
		return
	}
	if role == "" {
		reg.User = "" // 匿名注册：清空自报账号，RegisterAgent 不绑定 Owner
	}
	// 多开实例采纳派生节点 ID：同账号的多个桌面实例按槽位派生身份
	//（"nodeID-N"，见 PC 端 instance_slot.go），同开时不互相顶替。
	// 防冒充：仅接受基础 ID 自身或严格 "基础ID-数字" 派生形式（同账号
	// 家族内）；其他任意 ID 一律回落基础 ID，无法占用他节点身份。
	// 去令牌化：无令牌表绑定基础 ID，注册自报 ID 即基础 ID（派生规则仍生效）。
	baseNodeID := reg.NodeID
	nodeID := deriveAgentNodeID(baseNodeID, reg.NodeID)

	ws.SetReadDeadline(time.Time{})
	conn = &agentConn{nodeID: nodeID, ws: ws}
	agentQueue := make(chan Envelope, agentWorkerQueueSize)
	// RegisterAgent 内部已在解锁后广播 nodes + tasks（上线/重连收敛，
	// 含断联期间卡死任务的 status 恢复推送），此处无需重复广播。
	s.hub.RegisterAgent(conn, reg)
	// 回传服务端裁定身份 + 令牌家族基础 ID：PC 端据此对齐派生规则
	//（自生成 NodeID 与令牌不同族时此前每 66s 重连必触发回落 WARN 刷屏）。
	// 2026-10-09 修复：此处必须在 conn 登记后走带锁写。RegisterAgent 已把 conn
	// 登记为广播目标（并发 goroutine 会经 conn.writeJSON 持 sendMu 写同一底层
	// ws），裸写 ws.WriteJSON 不持锁 → gorilla "concurrent write to websocket
	// connection" panic 带崩整个进程（线上 0.7.9 崩溃-重启循环 restart counter
	// 104 的根因，节点/手机 WS 频繁断连，群聊/任务消息全部丢发）。
	_ = conn.writeJSON(NewEnvelope(e.ID, TypeServerRegisterOK, RegisterOKPayload{
		NodeID:        nodeID,
		BaseNodeID:    baseNodeID,
		ServerVersion: version.Number,
	}))

	// 读循环。
	//
	// P-Reliable 重构：读循环只收不处理（网络 IO 专用线程），消息投递到独立
	// worker goroutine（agentWorker，有界队列背压）串行处理 —— 之前落盘/广播
	// 在读循环内同步执行，事件风暴/慢盘/慢客户端把读循环拖到超过心跳超时，
	// 服务端主动断链（即用户反馈的「PC 端打着打着不打了」的断线路径之一），
	// 在途未处理消息随连接丢失。worker 处理完成后回 server.ack(msgId)，
	// 未确认帧由 PC 端 outbox 重连补发（去重幂等）。
	go s.agentWorker(conn, agentQueue)
	defer close(agentQueue) // 读循环退出关闭队列 → worker 排空后退出（不丢尾部消息）
	for {
		ws.SetReadDeadline(time.Now().Add(agentReadIdleTimeout)) // 心跳超时
		var e Envelope
		if err := ws.ReadJSON(&e); err != nil {
			return
		}
		// 压制到有界队列（满了丢弃最旧，保心跳/注册等控制帧优先；worker 慢于
		// 生产时背压丢弃事件帧可接受 —— PC 端 outbox 未收 ack 会补发）。
		select {
		case agentQueue <- e:
		default:
			// 队列满：丢弃最旧一条（若为未处理消息，PC 端未收 ack 会补发，不丢）。
			select {
			case <-agentQueue:
			default:
			}
			select {
			case agentQueue <- e:
			default:
			}
		}
	}
}

// handleAgentMessage 分发节点消息；返回 error 时（worker 层）向 PC 端回
// server.send_failed（触发立即重发）：仅未识别类型返回错误，业务处理器
// 内部已容错的返回 nil（避免误重发导致重复执行）。
func (s *Server) handleAgentMessage(conn *agentConn, e Envelope) error {
	switch e.Type {
	case TypeAgentHeartbeat:
		var hb Heartbeat
		_ = e.Decode(&hb)
		s.hub.TouchAgent(conn.nodeID, hb.RunningTasks)
	case TypeAgentTaskEvent:
		var ev TaskEvent
		if err := e.Decode(&ev); err == nil {
			s.hub.AddTaskEvent(ev)
		}
	case TypeAgentTaskDone:
		var d TaskDone
		if err := e.Decode(&d); err == nil {
			s.hub.TaskDone(d)
		}
	case TypeAgentSubtask:
		// 子会话生命周期上报（new_task/spawn_agent 平行子会话）：仅维护会话列表
		// 投影（含 parentId 缩进），不纳入任务链（无派发/取消/终态交互语义）。
		var st SubtaskEvent
		if err := e.Decode(&st); err == nil {
			s.hub.AddSubtask(st)
		}
	case TypeAgentMessages:
		var mr MessagesResponse
		if err := e.Decode(&mr); err == nil {
			s.hub.SaveSessionMessages(mr.SessionID, mr.Messages)
			// Todo 清单随历史镜像落盘（空清单不动旧值，见 SaveSessionTodos）。
			s.hub.SaveSessionTodos(mr.SessionID, mr.Todos)
			// 转发给请求来源客户端（拉历史后的响应）。
			s.hub.Broadcast(TypeMessages, mr)
		}
	case TypeAgentAnalyzeDone:
		// 会话 AI 分析结果回传：直接广播 analyze.done，移动端聊天页/会话列表
		// 渲染分析卡片（成功含 analysis 文本；失败含 error 提示）。
		var ad AnalyzeDone
		if err := e.Decode(&ad); err == nil {
			s.hub.Broadcast(TypeAnalyzeDone, ad)
		}
	case TypeAgentCleanupDone:
		// 批量清理会话结果回传：节点侧已完成「分析→文档→本地存档删除」，
		// 服务端联动删除这些会话的全部任务记录+镜像+事件（否则移动端会话列表
		// 由任务记录推导，任务不删则清理后手机端会话卡纹丝不动 —— 2026-10
		// 「PC 端清理后移动端不更新」根因）。失败（如任务仍标执行中）仅日志
		// 不阻断；随后广播 cleanup.done + 刷新 tasks/sessions 供移动端同步。
		// Mode=summarize（总结并清理）：节点已汇总未完成任务并清空本地 —— 本端
		// 先创建新会话任务（自动选节点并派发运行），再统一清空服务端旧记录，
		// 广播 clear.done（创建失败仍照常清空：不影响数据治理，仅丢续接任务）。
		var cd CleanupDone
		if err := e.Decode(&cd); err == nil {
			cd.NodeID = conn.nodeID
			// summarize（clear.tasks 收尾）：节点已汇总未完成任务并清空本地 —— 服务端
			// 必须先清空全部旧记录再创建新会话任务（顺序不能反：ClearAllTasks 会取消
			// 全部活动任务，若 CreateTask 在前，新建续接任务会被误删）。汇总缺失/
			// 创建失败仍照常清空（数据治理优先，仅丢续接任务，移动端可重发）。
			cdDone := ClearDone{Mode: cd.Mode}
			if cd.Mode == "summarize" {
				cdDone.Canceled, cdDone.Deleted, _ = s.hub.ClearAllTasks()
				if strings.TrimSpace(cd.Summary) == "" {
					cdDone.Error = "节点未返回汇总结果"
				} else if t, err := s.hub.CreateTask("", cd.Summary, "", "summarize-clear"); err != nil {
					cdDone.Error = "新任务创建失败: " + err.Error()
				} else {
					cdDone.NewTaskID = t.ID
					cdDone.NewSessionID = t.SessionID
					cdDone.Summary = firstLineText(cd.Summary, 120)
				}
			}
			if cd.Mode == "clear" {
				// PC 端本地「清空/总结并清空」回传（节点已完成本地存档清空）：服务端
				// 整表清空任务/事件/镜像并广播 clear.done（移动端列表同步收敛；
				// 对齐移动端 clear.tasks 收尾语义）。
				cdDone.Canceled, cdDone.Deleted, _ = s.hub.ClearAllTasks()
			}
			purged := 0
			for _, r := range cd.Reports {
				if !r.OK || r.SessionID == "" {
					continue // 跳过/失败项服务端保留记录（与节点侧语义一致）
				}
				if _, err := s.hub.DeleteSession(r.SessionID); err != nil {
					slog.Warn("清理联动删除服务端会话记录失败", "session", r.SessionID, "err", err)
					continue
				}
				purged++
			}
			slog.Info("远程批量清理服务端联动完成", "node", conn.nodeID, "deleted", cd.Deleted, "purged", purged)
			if cd.Mode == "summarize" || cd.Mode == "clear" {
				s.hub.Broadcast(TypeClearDone, cdDone)
			} else {
				s.hub.Broadcast(TypeCleanupDone, cd)
			}
			s.hub.Broadcast(TypeTasks, s.hub.ListTasks(100))
			s.hub.Broadcast("sessions", map[string]interface{}{"sessions": s.hub.ListSessionSummaries()})
		}
	case TypeAgentVerifyReport:
		// 节点任务状态核实回报：按节点本地会话真实状态校正服务端任务并
		// 广播 verify.done + tasks（移动端「刷新」按钮发起的全链路收尾）。
		var rep VerifyReport
		if err := e.Decode(&rep); err == nil {
			s.hub.ApplyVerifyReport(rep)
		}
	case TypeAgentRegister:
		// 节点重发注册（SendModeUpdate 模式回报复用 register，轻量方案）：
		// 幂等刷新节点 Mode/Modes 并广播 nodes（服务端 RegisterAgent 已做同连接幂等）。
		var reg AgentRegister
		if err := e.Decode(&reg); err == nil {
			s.hub.RegisterAgent(conn, reg)
			s.hub.Broadcast(TypeNodes, s.hub.ListNodes())
		}
	case TypeAgentConfig:
		// 节点配置快照回传：set.config 成功回报 / get.config 拉取响应。
		// 落缓存（掩码存储）后广播给全部在线客户端（移动端/管理端同步刷新）。
		var nc NodeConfig
		if err := e.Decode(&nc); err == nil {
			s.hub.StoreNodeConfig(conn.nodeID, nc)
			s.hub.Broadcast(TypeNodeConfig, map[string]interface{}{
				"nodeId": conn.nodeID, "config": nc,
			})
		}
	case TypeAgentEmployeeList:
		// 数字员工列表回传：广播 employees（移动端群聊建群页数据源；对齐技能库广播语义）。
		var list []EmployeeMeta
		if err := e.Decode(&list); err == nil {
			s.hub.Broadcast("employees", map[string]interface{}{"employees": list, "nodeId": conn.nodeID})
		}
	case TypeAgentSkillsList:
		// 技能元数据全量回传：落缓存 + skills 广播（移动端技能库列表数据源）。
		// 载荷带 nodeId（技能库按节点展示；广播对全部在线客户端）。
		var list []SkillMeta
		if err := e.Decode(&list); err == nil {
			s.hub.StoreSkills(conn.nodeID, list)
			s.hub.Broadcast(TypeSkills, map[string]interface{}{
				"nodeId": conn.nodeID, "skills": list,
			})
		}
	case TypeAgentSkillsData:
		// 技能全文回传：广播 skills.item（ID 关联请求，移动端按 id 填充编辑器）。
		var sd SkillsData
		if err := e.Decode(&sd); err == nil {
			s.hub.Broadcast(TypeSkillsItem, sd)
		}
	case TypeAgentRoadmapData:
		// 自主开发规划板回传：落缓存 + roadmap 广播（移动端「AR 自主开发」
		// 页数据源；对齐技能库 agent.skills_list 同款广播语义）。
		var rd RoadmapDataPayload
		if err := e.Decode(&rd); err == nil {
			s.hub.StoreRoadmap(conn.nodeID, rd)
			s.hub.Broadcast(TypeRoadmap, rd)
		}
	case TypeAgentGroupState:
		// A2A 群聊节点状态上报：落节点投影缓存 + groups 广播（群列表全端同步）；
		// 带消息历史时按 group.state 广播（群聊页打开 / sync 拉取后覆盖刷新）。
		var gsp GroupStatePayload
		if err := e.Decode(&gsp); err == nil {
			s.hub.StoreGroups(conn.nodeID, gsp.Groups)
			s.hub.Broadcast(TypeGroups, map[string]interface{}{"groups": s.hub.AllGroups()})
			if len(gsp.Messages) > 0 {
				gsp.NodeID = conn.nodeID
				s.hub.Broadcast(TypeGroupState, gsp)
			}
		}
	case TypeAgentGroupMsg:
		// 群消息回传（用户留言/员工产出/群助手通知）：广播 group.message，
		// 全部在线客户端（含移动端群聊页）实时追加。
		var gm GroupMessage
		if err := e.Decode(&gm); err == nil && gm.GroupID != "" {
			s.hub.Broadcast(TypeGroupMessage, gm)
		}
	case TypeAgentGroupDone:
		// 群写任务完成回传：广播 group.task_done（移动端群聊页渲染完成卡片）。
		var gt GroupTask
		if err := e.Decode(&gt); err == nil {
			s.hub.Broadcast(TypeGroupTaskDone, gt)
		}
	case TypeAgentIMState:
		// IM 中继渠道注册表上报：落服务端路由投影 + im.state 广播（渠道公网
		// 回调入口同步全端展示；im_relay.go）。
		var st IMRelayState
		if err := e.Decode(&st); err == nil {
			s.hub.SetIMRelayChannels(conn.nodeID, st)
		}
	case TypeAgentOffline:
		// 节点主动下线（用户关远程节点开关 / 退出账号）：标记 disabled=true
		// 后从节点列表移除并拒绝后续派发/续聊；与被动断线（UnregisterAgent）
		// 区分：被动断线保留灰点状态待重连，主动下线彻底消失。
		// 见 hub.AgentOffline 完整语义说明。
		s.hub.AgentOffline(conn.nodeID, conn)
	case TypeServerError:
		// 节点侧错误回报（如 set.config 合并保存失败，见 client.go handleSetConfig）。
		// 此前落 default 仅打日志，远端下发后失败无感知（server.go set.config 注释
		// 承诺「失败 → 节点 server.error」，此处补最后一跳）：转发给全部在线客户端，
		// 移动端/管理端 toast 提示并复位保存中状态。
		var ep ErrorPayload
		if err := e.Decode(&ep); err == nil && ep.Message != "" {
			s.hub.Broadcast(TypeServerError, ep)
		}
	default:
		slog.Warn("未知节点消息", "type", e.Type)
		return fmt.Errorf("未知消息类型: %s", e.Type)
	}
	return nil
}

// agentReadIdleTimeout 节点读空闲超时：心跳周期 15s，超时阈值放宽到 75s
// （5 个心跳周期）：PC 端休眠/系统挂起/GC 停顿/事件风暴时单心跳延迟可达数十秒，
// 90s → 75s 无本质差异；核心是 worker 化后读循环不再被业务处理拖慢，
// 心跳帧 3 次内必达，75s 已含足量冗余（服务端视野内最后一次心跳后 5 周期无任何帧）。
const agentReadIdleTimeout = 75 * time.Second

// agentWorkerQueueSize 节点消息处理队列容量（有界背压；满时丢最旧保新，
// 被丢帧未收 ack → PC 端补发，不丢消息）。
const agentWorkerQueueSize = 256

// agentWorker 节点消息处理线程（P-Reliable 核心）：从队列串行消费，处理完成后
// 回 server.ack；未识别/处理失败回 server.send_failed（PC 立即重发）。带 msgID
// 的帧按 msgID 去重（补发重放幂等跳过，直接补 ack）。
func (s *Server) agentWorker(conn *agentConn, q <-chan Envelope) {
	for e := range q {
		s.processAgentMessage(conn, e)
	}
}

// processAgentMessage 处理单条节点消息（worker 线程内执行；含去重与 ack 回执）。
func (s *Server) processAgentMessage(conn *agentConn, e Envelope) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("节点消息处理 panic（已拦截，连接保持）", "type", e.Type, "panic", r)
			if e.MsgID != "" {
				_ = conn.writeJSON(NewEnvelope("", TypeServerSendFailed, SendFailedPayload{MsgID: e.MsgID, Error: fmt.Sprintf("处理异常: %v", r)}))
			}
		}
	}()
	// 去重（P-Reliable）：带 msgID 的帧若已处理过，直接补发 ack（PC 端可能
	// 未收到首次 ack 就断线重发）；不带 msgID 的旧协议帧不参与去重。
	if e.MsgID != "" {
		if s.hub.msgIDIsDup(e.MsgID) {
			slog.Debug("节点补发消息重复，幂等跳过", "type", e.Type, "msgId", e.MsgID)
			_ = conn.writeJSON(NewEnvelope("", TypeServerAck, AckPayload{MsgID: e.MsgID}))
			return
		}
	}
	err := s.handleAgentMessage(conn, e)
	if e.MsgID != "" {
		if err == nil {
			s.hub.msgIDSeen(e.MsgID) // 处理成功才置位去重表 + 落盘（失败不置位，等待补发）
			_ = conn.writeJSON(NewEnvelope("", TypeServerAck, AckPayload{MsgID: e.MsgID}))
		} else {
			_ = conn.writeJSON(NewEnvelope("", TypeServerSendFailed, SendFailedPayload{MsgID: e.MsgID, Error: err.Error()}))
		}
	}
}

// deriveAgentNodeID 校验并采纳节点自报 ID（多开派生身份）：
//   - 空 / 等于基础 ID → 基础 ID（旧客户端兼容）;
//   - "基础ID-数字" 派生形式（PC 端多开槽位递增，instance_slot.go）→ 原样采纳；
//   - 其他任意值（试图冒充他节点）→ 回落基础 ID。
//
// 同令牌家族内多实例各自持独立派生 ID 注册为多个节点；跨令牌冒充因前缀
// 不匹配被拒（派生段必须为纯数字，与 PC 端槽位号约定一致）。
func deriveAgentNodeID(baseID, claimed string) string {
	claimed = strings.TrimSpace(claimed)
	if claimed == "" || claimed == baseID {
		return baseID
	}
	suffix, ok := strings.CutPrefix(claimed, baseID+"-")
	if ok && suffix != "" && isAllDigits(suffix) {
		return claimed
	}
	slog.Warn("节点自报 ID 与令牌不匹配，已回落基础 ID",
		"baseNodeID", baseID, "claimed", claimed)
	return baseID
}

// isAllDigits 判断字符串是否全为数字（非空）。
func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// routeGroupMessage 按群归属节点路由群操作（群消息/任务/成员管理）；
// 群不在任何节点投影中（服务重启缓存空）时回退路由到第一个在线节点拉同步。
func (s *Server) routeGroupMessage(groupID, typ string, req GroupServerReq) error {
	if nid, ok := s.hub.GroupNodeOf(groupID); ok {
		if err := s.hub.RouteToAgent(nid, typ, req); err == nil {
			return nil
		}
		// 投影有新群但节点已离线：回退到在线节点（群状态权威跟随节点重来）。
	}
	nid, err := s.hub.PickNode("")
	if err != nil {
		return fmt.Errorf("群归属节点不在线：%w", err)
	}
	return s.hub.RouteToAgent(nid, typ, req)
}

// todosToIface json.RawMessage 清单转 interface{} 清单（Envelope 序列化时保留原始结构）。
func todosToIface(raws []json.RawMessage) []interface{} {
	if len(raws) == 0 {
		return nil
	}
	out := make([]interface{}, 0, len(raws))
	for _, r := range raws {
		out = append(out, r)
	}
	return out
}

// ---------- WebSocket：客户端（手机 H5 / 管理后台） ----------
func (s *Server) handleClientWS(w http.ResponseWriter, r *http.Request) {
	ws, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	// panic 兜底：WS 处理器 panic 必须拦截（http handler goroutine 崩溃会带崩整个 HTTP 服务）。
	defer func() {
		if r := recover(); r != nil {
			slog.Error("客户端 WS 处理 panic（已拦截）", "panic", r)
			_ = ws.WriteJSON(NewEnvelope("", TypeAuthErr, ErrorPayload{Message: "服务异常，请重试"}))
		}
	}()

	// 首条必须是 auth。
	ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	var e Envelope
	if err := ws.ReadJSON(&e); err != nil {
		// 连接层失败（读超时/对端断开）无法回执；记日志便于排查认证阶段异常
		//（如客户端发空帧后挂断，修复前完全静默）。
		slog.Debug("客户端首帧读取失败（连接关闭或超时）", "err", err)
		return
	}
	if e.Type != TypeAuth {
		_ = ws.WriteJSON(NewEnvelope("", TypeAuthErr, ErrorPayload{Message: "请先认证"}))
		return
	}
	var a Auth
	if err := e.Decode(&a); err != nil {
		// 修复：首帧 Decode 失败此前静默 return（连接无响应，前端永久等 auth 回执），
		// 改日志 + 回 auth.err（响应体脱敏：不含载荷内容，仅提示格式错误）。
		slog.Warn("客户端认证载荷解析失败，已回 auth.err", "err", err)
		_ = ws.WriteJSON(NewEnvelope(e.ID, TypeAuthErr, ErrorPayload{Message: "认证载荷格式错误"}))
		return
	}
	// token 通道保留（去令牌化仅移除 /ws/agent 注册令牌；移动端 H5 仍支持
	// 共享令牌 URL 免密登录，AuthClient 内部校验 clientToken）。
	role, user, ok := s.hub.AuthClient(a.User, a.Pass, a.Token)
	if !ok {
		_ = ws.WriteJSON(NewEnvelope(e.ID, TypeAuthErr, ErrorPayload{Message: "认证失败：用户名或密码错误"}))
		return
	}
	ws.SetReadDeadline(time.Time{})
	c := &clientConn{role: role, user: user, ws: ws}
	// 认证通过但 h.mu 不可得（锁泄漏/异常持有兜底）：带 5s 超时，超时回
	// auth.err 并关闭连接 —— 修复前端永久挂起（10-09 事故中 AddClient 永久阻塞）。
	if !s.hub.AddClientTimeout(c, clientAuthMuTimeout) {
		slog.Error("客户端认证后登记超时（h.mu 长时间不可得，疑似锁泄漏），已拒绝连接", "user", user)
		_ = ws.WriteJSON(NewEnvelope(e.ID, TypeAuthErr, ErrorPayload{Message: "服务繁忙，请稍后重试"}))
		return
	}
	defer s.hub.RemoveClient(c)
	// 2026-10-09 修复：AddClient 已登记本连接为 Broadcast 目标，此后对同一底层
	// ws 的所有写必须走 c.writeJSON（持 sendMu），否则与并发 Broadcast 裸写撞车
	// → gorilla concurrent write panic 带崩进程（同 agent 注册路径根因）。
	_ = c.writeJSON(NewEnvelope(e.ID, TypeAuthOK, AuthOK{Role: role, User: user}))
	// 认证成功后推送初始数据。
	_ = c.writeJSON(NewEnvelope("", TypeNodes, s.hub.ListNodes()))
	_ = c.writeJSON(NewEnvelope("", TypeTasks, s.hub.ListTasks(100)))

	// 读循环（客户端消息带并发处理；写统一经 hub.Broadcast / clientConn.writeJSON）。
	for {
		ws.SetReadDeadline(time.Time{})
		var e Envelope
		if err := ws.ReadJSON(&e); err != nil {
			return
		}
		s.handleClientMessage(c, e)
	}
}

// denyNoControl 指挥权校验（P-Account 账号隔离）：非 admin 客户端对指定节点的
// 全部指挥操作（发布任务/续聊/取消/删除/改配置/切模式/技能/群聊/规划板等）
// 必须先过此关 —— 节点 Owner 为空（PC 端仅连接未登录）或 Owner != 当前登录
// 账号时拒绝。校验失败回 op.result(false) 并返回 true（调用方直接 return）。
func (s *Server) denyNoControl(c *clientConn, e Envelope, nodeID string) bool {
	if err := s.hub.CanControl(nodeID, c.user, c.role); err != nil {
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
		return true
	}
	return false
}

// handleClientMessage 分发客户端消息（认证已通过）。
func (s *Server) handleClientMessage(c *clientConn, e Envelope) {
	// panic 兜底：客户端消息处理器内部异常不得带崩读循环（handleClientWS 在主
	// goroutine 串行调用本函数），回 server.error 告知前端（op.result 风格）。
	defer func() {
		if r := recover(); r != nil {
			slog.Error("客户端消息处理 panic（已拦截）", "type", e.Type, "panic", r)
			_ = c.writeJSON(NewEnvelope(e.ID, TypeServerError, ErrorPayload{Message: "处理异常: " + fmt.Sprint(r)}))
		}
	}()
	switch e.Type {
	case TypeListNodes:
		_ = c.writeJSON(NewEnvelope(e.ID, TypeNodes, s.hub.ListNodes()))
	case TypeListTasks:
		// 支持可选过滤载荷（TaskFilter；空载荷/解析失败 = 旧行为最新 100 条，向后兼容）。
		var f TaskFilter
		_ = e.Decode(&f)
		if err := f.Validate(); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeTasks, s.hub.QueryTasks(&f)))
	case TypeCreateTask:
		var ct CreateTask
		if err := e.Decode(&ct); err != nil || ct.Text == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "任务内容不能为空"}))
			return
		}
		createdBy := c.user + "（" + c.role + "）"
		// P-Account：手机端 nodeId 空 = 自动选节点，必须账号感知（只在自己/全部
		// [admin] 在线节点中选），否则 denyNoControl("") 必拒（e2e 主链路回归点）。
		nodeID := ct.NodeID
		if nodeID == "" {
			var perr error
			nodeID, perr = s.hub.PickNodeFor("", c.user, c.role)
			if perr != nil {
				_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: perr.Error()}))
				return
			}
		}
		if s.denyNoControl(c, e, nodeID) {
			return
		}
		t, err := s.hub.CreateTask(nodeID, ct.Text, ct.Mode, createdBy)
		if err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeTaskCreated, map[string]interface{}{"task": t}))
	case TypeSetMode:
		// 远端切换 PC 节点执行模式（PC 端唯一执行面，远端仅指令）。
		var sm SetMode
		if err := e.Decode(&sm); err != nil || sm.Mode == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "模式参数无效"}))
			return
		}
		nodeID := sm.NodeID
		if nodeID == "" {
			// 自动：选第一个在线节点（优先当前登录账号自己的节点，无则降级）。
			for _, n := range s.hub.ListNodes() {
				if n.Online && (c.role == "admin" || (n.Owner != "" && n.Owner == c.user)) {
					nodeID = n.ID
					break
				}
			}
		}
		if nodeID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "没有在线节点"}))
			return
		}
		if s.denyNoControl(c, e, nodeID) {
			return
		}
		if err := s.hub.RouteToAgent(nodeID, TypeServerSetMode, SetMode{Mode: sm.Mode}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		s.hub.SetNodeMode(nodeID, sm.Mode) // 立即回写展示（节点真实状态经下一次注册/心跳同步）
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "模式已切换: " + sm.Mode}))
		_ = c.writeJSON(NewEnvelope("", TypeNodes, s.hub.ListNodes()))
	case TypeChatSend:
		var cs ChatSend
		if err := e.Decode(&cs); err != nil || cs.Text == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "消息不能为空"}))
			return
		}
		// 查任务归属节点。
		taskID := cs.TaskID
		if taskID == "" {
			taskID, _ = s.hub.SessionTaskID(cs.SessionID)
		}
		var nodeID string
		if t, ok := s.hub.GetTask(taskID); ok {
			nodeID = t.NodeID
		}
		if nodeID == "" && cs.NodeID != "" {
			nodeID = cs.NodeID
		}
		if nodeID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "找不到会话归属节点"}))
			return
		}
		// P-Reliable：在线直发；离线暂存（节点上线后自动补发为续聊任务），
		// 不再直接丢弃（旧版路由失败 = 消息丢失）。
		if s.denyNoControl(c, e, nodeID) {
			return
		}
		cs2 := ChatSend{SessionID: cs.SessionID, TaskID: taskID, Text: cs.Text, Images: cs.Images}
		err := s.hub.EnqueueChat(nodeID, cs2)
		if err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true}))
	case TypeAskReply:
		var ar AskReply
		if err := e.Decode(&ar); err != nil || ar.SessionID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效"}))
			return
		}
		taskID, _ := s.hub.SessionTaskID(ar.SessionID)
		nodeID := ""
		if t, ok := s.hub.GetTask(taskID); ok {
			nodeID = t.NodeID
		}
		if nodeID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "找不到会话归属节点"}))
			return
		}
		if s.denyNoControl(c, e, nodeID) {
			return
		}
		err := s.hub.RouteToAgent(nodeID, TypeServerAskReply, ar)
		if err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
		}
	case TypeTaskCancel:
		// 取消受理即回 op.result（修复：此前无应答，移动端 cancelCurrentTask
		// 只能干等 8s 请求超时后误报「取消失败」）。取消是否成功落终态以
		// task.result（canceled=true）广播为准，转发节点失败不阻塞状态收敛
		//（离线节点任务由服务端直接标记 canceled）。
		var tc TaskCancel
		if err := e.Decode(&tc); err != nil || tc.TaskID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 taskId"}))
			return
		}
		sessionID := ""
		if t, ok := s.hub.GetTask(tc.TaskID); ok {
			sessionID = t.SessionID
			if s.denyNoControl(c, e, t.NodeID) {
				return
			}
		}
		s.hub.CancelTask(tc.TaskID, sessionID)
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "已请求取消"}))
	case TypeDeleteTask:
		// 删除任务记录（仅终态）：连带清会话镜像/Todo 镜像/事件历史，
		// 广播 tasks + sessions 让全部在线客户端同步消失（会话列表由任务推导，见
		// ListSessionSummaries —— 任务删了会话卡自然不再出现）。
		// 节点联动：会话无其它关联任务时路由节点删除本地会话存档
		//（~/.kaelvoris/sessions/<id>.json），PC 端会话列表不再残留。
		var dt DeleteTask
		if err := e.Decode(&dt); err != nil || dt.TaskID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效"}))
			return
		}
		// 指挥权校验在删除前（任务仍存在时按任务归属节点判定）。
		if t0, ok := s.hub.GetTask(dt.TaskID); ok {
			if s.denyNoControl(c, e, t0.NodeID) {
				return
			}
		}
		t, nodeID, err := s.hub.DeleteTask(dt.TaskID)
		if err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "记录已删除"}))
		// 节点侧本地会话存档清理（会话已无关联任务时才发；失败不影响本次删除结果）。
		if _, still := s.hub.SessionTaskID(t.SessionID); !still && t.SessionID != "" {
			if err := s.hub.RouteToAgent(nodeID, TypeServerDeleteSession, DeleteSession{SessionID: t.SessionID}); err != nil {
				slog.Warn("任务删除后节点侧会话存档清理失败", "task", dt.TaskID, "err", err)
			}
		}
		s.hub.Broadcast(TypeTasks, s.hub.ListTasks(100))
		s.hub.Broadcast("sessions", map[string]interface{}{"sessions": s.hub.ListSessionSummaries()})
	case TypeDeleteSession:
		// 会话删除（移动端会话卡左滑/按钮）：服务端记录+镜像全清 + 节点本地
		// 会话存档联动删除（「PC 端会话要一起删」）。路由失败不回滚（节点离线时
		// 下次节点上线旧存档仅本地可见，服务端记录已除名）。
		var ds DeleteSession
		if err := e.Decode(&ds); err != nil || ds.SessionID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 sessionId"}))
			return
		}
		// 指挥权校验在删除前（按会话关联任务归属节点判定）。
		if taskID0, ok0 := s.hub.SessionTaskID(ds.SessionID); ok0 {
			if t0, ok1 := s.hub.GetTask(taskID0); ok1 {
				if s.denyNoControl(c, e, t0.NodeID) {
					return
				}
			}
		}
		nodeID, err := s.hub.DeleteSession(ds.SessionID)
		if err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "会话已删除"}))
		if nodeID != "" {
			if rerr := s.hub.RouteToAgent(nodeID, TypeServerDeleteSession, ds); rerr != nil {
				slog.Warn("节点侧会话存档删除路由失败（服务端已删）", "session", ds.SessionID, "err", rerr)
			}
		}
		s.hub.Broadcast(TypeTasks, s.hub.ListTasks(100))
		s.hub.Broadcast("sessions", map[string]interface{}{"sessions": s.hub.ListSessionSummaries()})
	case TypeRenameSession:
		// 会话重命名（PC 端会话卡右键改名同步服务端）：手动名称写入该会话
		// 全部任务记录的 Title，落盘 + 广播 tasks/sessions，移动端/管理端
		// 会话列表与聊天页标题实时同步（显示优先手动名，不再回退任务首行）。
		var rs RenameSession
		if err := e.Decode(&rs); err != nil || rs.SessionID == "" || strings.TrimSpace(rs.Title) == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 sessionId 与 title"}))
			return
		}
		if _, err := s.hub.RenameSession(rs.SessionID, rs.Title); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "会话已重命名"}))
		s.hub.Broadcast(TypeTasks, s.hub.ListTasks(100))
		s.hub.Broadcast("sessions", map[string]interface{}{"sessions": s.hub.ListSessionSummaries()})
	case TypePinSession:
		// 会话置顶/取消置顶（移动端长按菜单）：Pinned 时间戳写入该会话全部任务
		// 记录，落盘 + 广播 tasks/sessions，全部在线端列表同步置顶排序
		//（会话卡片列表按 ListSessionSummaries 的 pinned 优先排序渲染）。
		var ps PinSession
		if err := e.Decode(&ps); err != nil || ps.SessionID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 sessionId"}))
			return
		}
		if _, err := s.hub.PinSession(ps.SessionID, ps.Pinned); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		// 提示语按动作区分：置顶与取消置顶共用此协议入口，固定文案会误导操作者。
		pinMsg := "已置顶"
		if !ps.Pinned {
			pinMsg = "已取消置顶"
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: pinMsg}))
		s.hub.Broadcast(TypeTasks, s.hub.ListTasks(100))
		s.hub.Broadcast("sessions", map[string]interface{}{"sessions": s.hub.ListSessionSummaries()})
	case TypeAnalyzeSession:
		// AI 会话分析：路由到会话归属节点（分析在 PC 端执行 —— 节点持有完整
		// 会话历史与 LLM 配置；服务端不存明文密钥）。受理即回 op.result，
		// 结果经 analyze.done 广播（异步，LLM 耗时不可预估）。
		var as AnalyzeSession
		if err := e.Decode(&as); err != nil || as.SessionID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 sessionId"}))
			return
		}
		taskID, _ := s.hub.SessionTaskID(as.SessionID)
		nodeID := ""
		if t, ok := s.hub.GetTask(taskID); ok {
			nodeID = t.NodeID
		}
		if nodeID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "找不到会话归属节点"}))
			return
		}
		if s.denyNoControl(c, e, nodeID) {
			return
		}
		if err := s.hub.RouteToAgent(nodeID, TypeServerAnalyze, as); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "AI 分析已受理，PC 端生成中…"}))
	case TypeCleanupSessions:
		// 批量清理会话：路由到第一个在线节点（清理在 PC 端执行 —— 节点持有
		// 会话存档与 LLM 配置；逐会话「分析→文档→归档删除」，完成后经
		// agent.cleanup_done → cleanup.done 广播结果）。受理即回 op.result。
		nodeID, err := s.hub.PickNodeFor("", c.user, c.role) // P-Account：账号感知选节点
		if err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		if s.denyNoControl(c, e, nodeID) {
			return
		}
		if err := s.hub.RouteToAgent(nodeID, TypeServerCleanup, struct{}{}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "清理已受理，PC 端逐会话分析中…"}))
	case TypeVerifyTasks:
		// 任务状态核实（移动端「刷新」按钮发起）：把服务端侧 pending/running
		// 任务按节点分组，路由到各在线节点（server.verify_tasks 携带该节点的
		// taskId 清单）→ 节点按本地会话档案回传真实状态（agent.verify_report）
		// → ApplyVerifyReport 校正 + 广播 verify.done / tasks。
		// pending 暂存任务也一并发核（节点本地可能有派发丢失的会话事实）。
		//
		// 无活动任务时同样路由在线节点（空清单「空核实」）：节点回空报告 →
		// verify.done(ok) 收尾广播。修复：此前仅按活动任务分组，全部任务已
		// 终态时 byNode 为空走 routed==0 分支，节点明明在线却误报「没有在线
		// 节点」—— 移动端核实小圆圈永远提示失败，busy 态只能等 15s 超时复位。
		var vt VerifyTasks
		_ = e.Decode(&vt)
		byNode := map[string][]string{}
		for _, t := range s.hub.ListTasks(0) {
			if t.Status != TaskStatusPending && t.Status != TaskStatusRunning {
				continue
			}
			if t.NodeID != "" {
				byNode[t.NodeID] = append(byNode[t.NodeID], t.ID)
			}
		}
		// 发起端标记：随 server.verify_tasks 下发 → 节点透传回 agent.verify_report
		// → verify.done 广播携带，移动端据此区分本端发起（full 文案）与他端发起。
		requestedBy := c.role
		routed := 0
		if len(byNode) == 0 {
			// 无活动任务（全部终态）：向全部在线节点发「全量核实」哨兵请求。
			// 节点核实其 remoteTaskOf 中全部会话的真实状态 —— 覆盖节点断线
			// 期间被 watchdog「自动收敛」误标 failed 的任务（节点本地会话档案
			// 实际已完成 → verify 回报校正回 done）。无会话则回空报告收尾。
			for _, n := range s.hub.ListNodes() {
				if !n.Online {
					continue
				}
				if err := s.hub.RouteToAgent(n.ID, TypeServerVerifyTasks, VerifyTasks{RequestedBy: requestedBy, VerifyAll: true}); err == nil {
					routed++
				}
			}
		} else {
			for nid, ids := range byNode {
				if err := s.hub.RouteToAgent(nid, TypeServerVerifyTasks, VerifyTasks{TaskIDs: ids, RequestedBy: requestedBy}); err == nil {
					routed++
				}
			}
		}
		if routed == 0 {
			// 确实无可用节点（节点全离线 / 路由写失败）：广播 verify.done 兜底
			//（离线任务数 = 全部活动任务）。
			off := 0
			for _, ts := range byNode {
				off += len(ts)
			}
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "没有在线节点，无法核实"}))
			s.hub.Broadcast(TypeVerifyDone, VerifyDone{OK: false, Offline: off, Error: "没有在线节点"})
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "核实已受理，PC 端核对本地会话中…"}))
	case TypeClearTasks:
		// 清空全部任务与会话（移动端「清空」按钮）或「总结并清理」（Mode=summarize，
		// 节点先汇总未完成任务为续接任务，回传后由本端创建新会话任务再统一清空）。
		// 路由到第一个在线节点（清空在 PC 端执行 —— 节点持有本地会话存档与运行态；
		// 服务端侧记录清空在 agent.cleanup_done 收尾时统一执行，见 handleAgentMessage）。
		// 直接清空（Mode=clear）：服务端立即 ClearAllTasks（权威数据先收敛，
		// 移动端列表即刻消失），再路由节点清本地存档；节点不回帧。
		// 总结并清理：异步等待节点汇总，期间不预清（新任务创建后才清，防丢上下文）。
		var ca ClearAll
		_ = e.Decode(&ca)
		mode := strings.TrimSpace(ca.Mode)
		if mode != "clear" && mode != "summarize" {
			mode = "clear"
		}
		nodeID, err := s.hub.PickNodeFor("", c.user, c.role) // P-Account：账号感知选节点
		if err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		if s.denyNoControl(c, e, nodeID) {
			return
		}
		if mode == "summarize" {
			// 总结并清理：路由节点走 cleanup_sessions 链路（Mode 透传，节点先汇总
			// 未完成任务，回传 CleanupDone{Mode=summarize, Summary} 后由服务端
			// ClearAllTasks 清旧记录 → CreateTask 新续接任务 → clear.done 广播）。
			if err := s.hub.RouteToAgent(nodeID, TypeServerCleanup, CleanupAll{Mode: mode}); err != nil {
				_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
				return
			}
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "已受理：PC 端正在汇总未完成任务…"}))
			return
		}
		// 直接清空（mode=clear）：服务端记录/镜像先清 + 广播（移动端列表立即收敛），
		// 节点本地存档清理异步补发（server.clear_sessions；节点离线时残留仅本地
		// 可见，服务端已除名，下次清理入口再清）。目标节点与曾涉节点去重补发。
		canceled, deleted, nodes := s.hub.ClearAllTasks()
		routedClear := map[string]bool{nodeID: true}
		for _, nid := range nodes {
			if routedClear[nid] {
				continue
			}
			routedClear[nid] = true
			if rerr := s.hub.RouteToAgent(nid, TypeServerClearSessions, struct{}{}); rerr != nil {
				slog.Warn("清空联动：节点本地会话存档清理路由失败", "node", nid, "err", rerr)
			}
		}
		if err := s.hub.RouteToAgent(nodeID, TypeServerClearSessions, struct{}{}); err != nil {
			slog.Warn("清空联动：目标节点本地会话存档清理路由失败", "node", nodeID, "err", err)
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "已清空 " + strconv.Itoa(deleted) + " 条任务记录"}))
		s.hub.Broadcast(TypeClearDone, ClearDone{Mode: "clear", OK: true, Canceled: canceled, Deleted: deleted})
		s.hub.Broadcast(TypeTasks, s.hub.ListTasks(100))
		s.hub.Broadcast("sessions", map[string]interface{}{"sessions": s.hub.ListSessionSummaries()})
	case TypeGetConfig:
		// 远端读取节点配置：先以服务端缓存立即应答（离线节点返回最后已知快照，
		// 保证请求-响应配对），在线节点同时路由 server.get_config 拉实时快照
		//（节点回传 agent.config 后经 node.config 广播覆盖刷新；同 get.messages
		// "本地镜像立即应答 + 节点回传覆盖" 同源语义）。
		var gc GetConfig
		if err := e.Decode(&gc); err != nil || gc.NodeID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId"}))
			return
		}
		cached, _ := s.hub.NodeConfigOf(gc.NodeID)
		_ = c.writeJSON(NewEnvelope(e.ID, TypeNodeConfig, map[string]interface{}{
			"nodeId": gc.NodeID, "config": cached,
		}))
		_ = s.hub.RouteToAgent(gc.NodeID, TypeServerGetConfig, GetConfig{NodeID: gc.NodeID})
	case TypeSetConfig:
		// 远端修改节点配置：部分字段更新（指针 = 显式下发；APIKey 非空才覆盖）。
		// 路由到节点后不立即 op.result：真实确认来自节点 agent.config 回报
		//（成功 → node.config 广播 + op.result ok；失败 → 节点 server.error）。
		var sc SetConfig
		if err := e.Decode(&sc); err != nil || sc.NodeID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId"}))
			return
		}
		if s.denyNoControl(c, e, sc.NodeID) {
			return
		}
		if sc.BaseURL == nil && sc.Model == nil && sc.APIKey == "" && sc.AutoApprove == nil &&
			sc.ContextWindow == nil && sc.AutoCondensePercent == nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "无任何待更新字段"}))
			return
		}
		if err := s.hub.RouteToAgent(sc.NodeID, TypeServerSetConfig, sc); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
		}
	case TypeGroupList:
		// A2A 群聊列表：服务端节点投影合并立即应答（groups 广播格式），同时路由
		// 全部在线节点拉权威快照（server.group_sync → agent.group_state → groups 广播覆盖）。
		_ = c.writeJSON(NewEnvelope(e.ID, TypeGroups, map[string]interface{}{"groups": s.hub.AllGroups()}))
		for _, n := range s.hub.ListNodes() {
			if n.Online {
				_ = s.hub.RouteToAgent(n.ID, TypeServerGroupSync, GroupServerReq{})
			}
		}
	case TypeGroupCreate:
		// 创建群聊：路由到节点（群状态权威在节点：节点建群落盘 + 群状态上报广播）。
		var gc GroupCreateReq
		if err := e.Decode(&gc); err != nil || strings.TrimSpace(gc.Name) == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要群名称"}))
			return
		}
		nodeID, err := s.hub.PickNodeFor("", c.user, c.role) // P-Account：账号感知选节点
		if err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		if s.denyNoControl(c, e, nodeID) {
			return
		}
		if err := s.hub.RouteToAgent(nodeID, TypeServerGroupCreate, GroupServerReq{Name: gc.Name, Members: gc.Members}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "群聊创建已受理，节点侧建群中…"}))
	case TypeGroupMsg:
		// 群消息（移动端发送）：按群归属节点路由（节点记录历史 + 回传 agent.group_msg
		// → 服务端广播 group.message，全部在线客户端消息流实时同步）。
		var gm GroupMsgReq
		if err := e.Decode(&gm); err != nil || gm.GroupID == "" || strings.TrimSpace(gm.Text) == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "群消息不能为空"}))
			return
		}
		if nid, ok := s.hub.GroupNodeOf(gm.GroupID); ok {
			if s.denyNoControl(c, e, nid) {
				return
			}
		}
		if err := s.routeGroupMessage(gm.GroupID, TypeServerGroupMsg, GroupServerReq{GroupID: gm.GroupID, Text: gm.Text}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "已发送到群"}))
	case TypeGroupTask:
		// 群写任务：群助手协调派发（AssignedTo 指定员工；空 = 节点侧自动选）。
		var gt GroupTaskReq
		if err := e.Decode(&gt); err != nil || gt.GroupID == "" || strings.TrimSpace(gt.Instruction) == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "任务指令不能为空"}))
			return
		}
		// Mode 随路透传（协作模式 chain/parallel/review；空 = chain 向后兼容）。
		if nid, ok := s.hub.GroupNodeOf(gt.GroupID); ok {
			if s.denyNoControl(c, e, nid) {
				return
			}
		}
		if err := s.routeGroupMessage(gt.GroupID, TypeServerGroupTask, GroupServerReq{GroupID: gt.GroupID, Instruction: gt.Instruction, AssignedTo: gt.AssignedTo, Mode: gt.Mode}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "任务已受理，群助手协调中…"}))
	case TypeGroupJoin:
		var gj GroupMemberReq
		if err := e.Decode(&gj); err != nil || gj.GroupID == "" || gj.AgentID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 groupId 与 agentId"}))
			return
		}
		if nid, ok := s.hub.GroupNodeOf(gj.GroupID); ok {
			if s.denyNoControl(c, e, nid) {
				return
			}
		}
		if err := s.routeGroupMessage(gj.GroupID, TypeServerGroupJoin, GroupServerReq{GroupID: gj.GroupID, AgentID: gj.AgentID}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "已添加成员"}))
	case TypeGroupLeave:
		var gl GroupMemberReq
		if err := e.Decode(&gl); err != nil || gl.GroupID == "" || gl.AgentID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 groupId 与 agentId"}))
			return
		}
		if nid, ok := s.hub.GroupNodeOf(gl.GroupID); ok {
			if s.denyNoControl(c, e, nid) {
				return
			}
		}
		if err := s.routeGroupMessage(gl.GroupID, TypeServerGroupLeave, GroupServerReq{GroupID: gl.GroupID, AgentID: gl.AgentID}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "已移出成员"}))
	case TypeGroupSync:
		// 群状态拉取（群聊页/群列表打开时）：缓存投影立即应答（保证请求-响应配对），
		// 同时路由节点拉权威快照（含消息历史；agent.group_state → group.state 广播覆盖）。
		var gs GroupListReq
		_ = e.Decode(&gs)
		snap := s.hub.AllGroups()
		if gs.GroupID != "" {
			var one []Group
			for _, g := range snap {
				if g.ID == gs.GroupID {
					one = append(one, g)
				}
			}
			snap = one
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeGroupState, GroupStatePayload{Groups: snap}))
		routed := 0
		if gs.GroupID != "" {
			if nid, ok := s.hub.GroupNodeOf(gs.GroupID); ok {
				if s.hub.RouteToAgent(nid, TypeServerGroupSync, GroupServerReq{GroupID: gs.GroupID}) == nil {
					routed++
				}
			}
		} else {
			for _, n := range s.hub.ListNodes() {
				if n.Online {
					if s.hub.RouteToAgent(n.ID, TypeServerGroupSync, GroupServerReq{}) == nil {
						routed++
					}
				}
			}
		}
		if routed == 0 && gs.GroupID != "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "群归属节点不在线，仅可查看缓存快照"}))
		}
	case TypeEmployeeList:
		// 数字员工列表：缓存立即应答语义对标 skills.list —— 路由到首个在线节点
		// 拉实时列表（agent.employee_list → 广播刷新），无节点回空列表。
		var er EmployeeListReq
		_ = e.Decode(&er)
		nodeID := er.NodeID
		if nodeID == "" {
			for _, n := range s.hub.ListNodes() {
				if n.Online {
					nodeID = n.ID
					break
				}
			}
		}
		if nodeID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, "employees", map[string]interface{}{"employees": []EmployeeMeta{}}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, "employees", map[string]interface{}{"employees": []EmployeeMeta{}, "nodeId": nodeID}))
		_ = s.hub.RouteToAgent(nodeID, TypeServerEmployeeList, struct{}{})
	case TypeSkillsList:
		// 技能列表：缓存立即应答（同 get.config "镜像立即应答 + 节点回传覆盖" 语义），
		// 在线节点同时路由 server.skills_list 拉实时列表（agent.skills_list 广播覆盖刷新）。
		var lr SkillsListReq
		if err := e.Decode(&lr); err != nil || lr.NodeID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId"}))
			return
		}
		cached := s.hub.SkillsOf(lr.NodeID)
		if cached == nil {
			cached = []SkillMeta{}
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeSkills, map[string]interface{}{
			"nodeId": lr.NodeID, "skills": cached,
		}))
		_ = s.hub.RouteToAgent(lr.NodeID, TypeServerSkillsList, struct{}{})
	case TypeSkillsGet:
		// 技能全文：路由到节点（skills.item 广播回传，ID 关联请求）。
		var gr SkillsGetReq
		if err := e.Decode(&gr); err != nil || gr.NodeID == "" || gr.Name == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId 与 name"}))
			return
		}
		if err := s.hub.RouteToAgent(gr.NodeID, TypeServerSkillsGet, SkillsGet{Name: gr.Name}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		// 帧无 id 时生成关联 id（skills.item 广播不带定向字段，移动端按 id 匹配）。
		if e.ID == "" {
			e.ID = "sk-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "已受理，等待节点回传"}))
	case TypeSkillsSave:
		// 保存/新建技能：校验后路由（受理即回 op.result；真实结果由
		// agent.skills_list 广播确认；保存失败由节点 server.error 回报）。
		var sr SkillsSaveReq
		if err := e.Decode(&sr); err != nil || sr.NodeID == "" || sr.Name == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId 与 name"}))
			return
		}
		if s.denyNoControl(c, e, sr.NodeID) {
			return
		}
		if err := s.hub.RouteToAgent(sr.NodeID, TypeServerSkillsSave, SkillsSave{Name: sr.Name, Content: sr.Content}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "保存已受理"}))
	case TypeSkillsDelete:
		// 删除技能：同保存语义（builtin 禁删由节点拒绝 + server.error 回报）。
		var dr SkillsGetReq
		if err := e.Decode(&dr); err != nil || dr.NodeID == "" || dr.Name == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId 与 name"}))
			return
		}
		if s.denyNoControl(c, e, dr.NodeID) {
			return
		}
		if err := s.hub.RouteToAgent(dr.NodeID, TypeServerSkillsDelete, SkillsGet{Name: dr.Name}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "删除已受理"}))
	case TypeRoadmapList:
		// 自主开发规划板：缓存立即应答（同 skills.list "镜像立即应答 + 节点回传覆盖"
		// 语义），在线节点同时路由 server.roadmap_list 拉实时数据（agent.roadmap_data
		// 广播覆盖刷新）。
		var lr RoadmapListReq
		if err := e.Decode(&lr); err != nil || lr.NodeID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId"}))
			return
		}
		snap, ok := s.hub.RoadmapOf(lr.NodeID)
		if !ok {
			snap = RoadmapDataPayload{Items: []RoadmapItem{}}
		}
		snap.NodeID = lr.NodeID
		_ = c.writeJSON(NewEnvelope(e.ID, TypeRoadmap, snap))
		_ = s.hub.RouteToAgent(lr.NodeID, TypeServerRoadmapList, struct{}{})
	case TypeRoadmapPlan:
		// 触发 AI 规划：路由到节点（受理即回 op.result；提案产出后节点推
		// agent.roadmap_data 广播刷新；失败由节点 server.error 回报）。
		var pr RoadmapPlanReq
		if err := e.Decode(&pr); err != nil || pr.NodeID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId"}))
			return
		}
		if s.denyNoControl(c, e, pr.NodeID) {
			return
		}
		if err := s.hub.RouteToAgent(pr.NodeID, TypeServerRoadmapPlan, RoadmapPlanReq{Guidance: pr.Guidance}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "规划已受理，PC 端分析中"}))
	case TypeRoadmapDev:
		// 执行自主开发：执行前必须选择（IDs 必填，服务端强制校验——未勾选
		// 提案不允许点执行；与移动端按钮禁用双保险）。
		var dr RoadmapDevReq
		if err := e.Decode(&dr); err != nil || dr.NodeID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId"}))
			return
		}
		if len(dr.IDs) == 0 {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "请先选择要开发的条目（未选择不能执行）"}))
			return
		}
		if s.denyNoControl(c, e, dr.NodeID) {
			return
		}
		if err := s.hub.RouteToAgent(dr.NodeID, TypeServerRoadmapDev, dr); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: fmt.Sprintf("已受理 %d 条进入开发", len(dr.IDs))}))
	case TypeRoadmapDelete:
		// 删除规划条目：路由到节点（结果由 agent.roadmap_data 广播确认）。
		var xr RoadmapDeleteReq
		if err := e.Decode(&xr); err != nil || xr.NodeID == "" || xr.ID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId 与 id"}))
			return
		}
		if err := s.hub.RouteToAgent(xr.NodeID, TypeServerRoadmapDelete, RoadmapDeleteReq{ID: xr.ID}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "删除已受理"}))
	case TypeRoadmapDeleteBatch:
		// 批量删除规划条目：路由到节点（结果由 agent.roadmap_data 广播确认）。
		var xb RoadmapDeleteBatchReq
		if err := e.Decode(&xb); err != nil || xb.NodeID == "" || len(xb.IDs) == 0 {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId 与 ids"}))
			return
		}
		if err := s.hub.RouteToAgent(xb.NodeID, TypeServerRoadmapDeleteBatch, RoadmapDeleteBatchReq{IDs: xb.IDs}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "批量删除已受理"}))
	case TypeRoadmapApprove:
		// 批量审核通过：只置 approved 入排期不开发（IDs 必填；结果由广播确认）。
		var ar RoadmapDeleteBatchReq
		if err := e.Decode(&ar); err != nil || ar.NodeID == "" || len(ar.IDs) == 0 {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId 与 ids"}))
			return
		}
		if s.denyNoControl(c, e, ar.NodeID) {
			return
		}
		if err := s.hub.RouteToAgent(ar.NodeID, TypeServerRoadmapApprove, RoadmapDeleteBatchReq{IDs: ar.IDs}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "已受理审核"}))
	case TypeRoadmapAuto:
		// 完全自主开发：路由到节点（guidance 可空；parallel 并发线程数）。
		var au RoadmapAutoReq
		if err := e.Decode(&au); err != nil || au.NodeID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId"}))
			return
		}
		if s.denyNoControl(c, e, au.NodeID) {
			return
		}
		if err := s.hub.RouteToAgent(au.NodeID, TypeServerRoadmapAuto, RoadmapAutoReq{Guidance: au.Guidance, Parallel: au.Parallel, MinDeveloping: au.MinDeveloping}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "已受理"}))
	case TypeRoadmapRetry:
		// 一键重跑失败/暂停条目：路由到节点（failed/blocked → approved 回队，
		// 状态变更由节点 agent.roadmap_data 广播确认）。
		var rr RoadmapDeleteReq
		if err := e.Decode(&rr); err != nil || rr.NodeID == "" || rr.ID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效：需要 nodeId 与 id"}))
			return
		}
		if s.denyNoControl(c, e, rr.NodeID) {
			return
		}
		if err := s.hub.RouteToAgent(rr.NodeID, TypeServerRoadmapRetry, RoadmapDeleteReq{ID: rr.ID}); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: true, Message: "已重新入队"}))
	case TypeGetMessages:
		var gm GetMessages
		if err := e.Decode(&gm); err != nil || gm.SessionID == "" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效"}))
			return
		}
		// 修复点：此前只查服务端本地镜像（SessionMessages），而镜像唯一写入来源是
		// 节点回传的 agent.messages，但全链路无 server.get_messages 发送方 ——
		// 镜像恒为空 → 手机端/管理端点开聊天卡永远空白（E2E
		// TestE2E_MobileOpensChatHistory 复现）。
		// 现语义：先以服务端本地镜像立即应答（可能为空/过期，但保证请求-响应配对，
		// 前端 pending 不悬空超时）；同时路由到会话归属节点拉真实历史（server.get_messages
		// → 节点回传 agent.messages → 服务端 SaveSessionMessages 落镜像 + Broadcast 覆盖），
		// 节点不在线或路由失败时本地镜像即最终答案（sessions.json，服务重启后仍可看历史）。
		// Todo 进度随镜像立即回传（节点回传 agent.messages 后经 Broadcast 覆盖刷新；
		// 节点离线/服务重启后移动端点开聊天卡仍可渲染历史进度）。
		// 请求帧无 id 时也回写同 type 响应（id 置空）：移动端 request() 仅把带 id
		// 帧送 pending resolve，但空历史时靠后续 agent.messages 广播覆盖 —— 无 id
		// 帧若被 dispatch 吞掉，首次加载页将空白直到节点回传。统一写响应保证
		// dispatch 分支总能收到至少一帧 messages（镜像有则立即渲染）。
		rspID := e.ID
		if rspID == "" {
			rspID = "gm-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		}
		_ = c.writeJSON(NewEnvelope(rspID, TypeMessages, map[string]interface{}{
			"sessionID": gm.SessionID, "messages": s.hub.SessionMessages(gm.SessionID),
			"todos": todosToIface(s.hub.SessionTodos(gm.SessionID)),
		}))
		if taskID, ok := s.hub.SessionTaskID(gm.SessionID); ok {
			if t, ok := s.hub.GetTask(taskID); ok && t.NodeID != "" {
				_ = s.hub.RouteToAgent(t.NodeID, TypeServerGetMessages, gm)
			}
		}
	case TypeAccountPasswd:
		// 修改密码（P-Account）：自己改密验旧密码；user 字段非空且非本人时仅
		// admin 可重置。改密后宿主经 OnUsersChanged 回调重新持久化 accounts.json。
		var ap AccountPasswd
		if err := e.Decode(&ap); err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "参数无效"}))
			return
		}
		target, err := s.hub.ChangePassword(c.user, c.role, ap.User, ap.OldPass, ap.NewPass)
		if err != nil {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: err.Error()}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, "account.passwd_ok", AccountPasswdOK{
			User:    target,
			Self:    target == c.user,
			Message: "密码已更新",
		}))
	case "list.accounts":
		// 统一账号列表（脱敏）：管理端账号管理页展示。
		if c.role != "admin" {
			_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "仅管理员可查看账号列表"}))
			return
		}
		_ = c.writeJSON(NewEnvelope(e.ID, "accounts", map[string]interface{}{
			"accounts": s.hub.Accounts(),
		}))
	case "list.sessions":
		_ = c.writeJSON(NewEnvelope(e.ID, "sessions", map[string]interface{}{
			"sessions": s.hub.ListSessionSummaries(),
		}))
	case "get.events":
		var ge struct {
			TaskID string `json:"taskId"`
		}
		if err := e.Decode(&ge); err == nil {
			for _, ev := range s.hub.TaskEvents(ge.TaskID) {
				_ = c.writeJSON(NewEnvelope(e.ID, TypeTaskEvent, ev))
			}
		}
	case "client.ping":
		// H5/管理端心跳保活应答：客户端定期发 client.ping，服务端立即回 pong
		//（载荷回传 ts 供 RTT 计算）。修复移动端「PC 端执行完了状态不更新」：
		// 手机休眠/弱网致 WS 半开时 onclose 不触发，服务端广播的 task.result
		// 永远发不到死连接。旧客户端无 ping 不受影响（读循环零超时兼容）；
		// 新客户端对旧服务端：按未知类型回 op.result(false)，客户端据此判假活。
		_ = c.writeJSON(NewEnvelope(e.ID, "client.pong", map[string]interface{}{"ts": time.Now().UnixMilli()}))
	default:
		_ = c.writeJSON(NewEnvelope(e.ID, TypeOpResult, OpResult{OK: false, Message: "未知消息类型: " + e.Type}))
	}
}
