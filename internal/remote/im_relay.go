package remote

// im_relay.go 通用 IM 渠道公网中继（接收侧公网托管）：
//
//	本地 PC 节点无公网入站能力，IM 平台回调打不到本地；服务端（公网）代持
//	回调入口，经 /ws/agent 既有长连接以 server.im_inbound 帧转发给「在线且
//	注册了该渠道」的节点。回复侧（pubsendV2/Webhook）由节点出站直连 IM 平台
//	（节点出站网络可达，与 LLM API 调用同源），服务端不代理发送。
//
//	生命周期：节点注册中继渠道（agent.im_state，Enabled=true）→ 服务端落
//	im_channels.json 持久化（服务重启回调入口不失效）→ 平台回调按渠道指纹
//	路由转发；节点下线 → 摘除自身在线路由（同指纹其他在线节点自动接管：
//	同机多开读同一 channels.json，渠道等价，多主故障转移——修复单节点映射
//	「最后注册者独占路由，其退出后同指纹渠道整体 503」）；全部离线后回调回
//	503（平台超时重发语义兜底，不静默丢消息，落盘态保留待重连恢复）。
//
//	回调 URL（平台侧配置）：
//	  - 公共号：  /im/c/<fp>          （fp = 渠道指纹）
//	  - 群消息：  /im/g/<fp>/<hookID> （hookID 与群组一一对应）
//
//	渠道指纹 = 五元组（Name+PublicID+EID+WebhookToken+GroupHookIDs）SHA1 前
//	12 位：URL 安全、不含敏感明文；节点侧 imRelayChannelFP（channel_im.go）
//	必须与本函数一致（跨进程契约，注释互指）。

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 回调 URL 前缀（与 RegisterIMRelayRoutes 的 mux 挂载一致）。
const (
	imRelayPathPub   = "/im/c/"
	imRelayPathGroup = "/im/g/"
)

// imRelayChannelFP 渠道指纹（五元组 SHA1 前 12 位；与节点侧 channel_im.go
// imRelayChannelFP 保持逐字节一致 —— 跨进程契约）。
func imRelayChannelFP(name, publicID, eid, webhookToken, groupHooks string) string {
	sum := sha1.Sum([]byte(strings.Join([]string{name, publicID, eid, webhookToken, groupHooks}, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}

// imRelayEntry 已注册的中继渠道（服务端视角）。
type imRelayEntry struct {
	NodeID  string // 归属节点（转发目标首选；离线时多主故障转移到其他在线节点）
	FP      string // 渠道指纹（回调 URL 路由键）
	Channel IMRelayChannel
	TS      int64 // 注册时间（毫秒）
}

// imRelayRegistry IM 中继渠道注册表（内存 + im_channels.json 持久化）。
// 节点渠道注册是全量幂等替换（SetChannels），指纹即主键；routes 为在线路由
// 子表：fp → 在线节点 ID 集合（多主路由）。同机多开实例读同一 channels.json，
// 会以相同指纹注册同一渠道——任一在线即可服务，节点下线仅摘除自身，其余在线
// 节点自动接管（修复单节点映射「最后注册者独占路由，其退出后渠道整体 503」
// 的线上事故，2026-10-11 /im/g/28c87ec3734d/BD0001 → node offline）。entries
// 为全量注册态（持久化）。
type imRelayRegistry struct {
	mu      sync.Mutex
	entries map[string]*imRelayEntry   // fp → entry（全量注册，含离线节点）
	routes  map[string]map[string]bool // fp → 在线节点 ID 集合（多主路由）
	dir     string                     // 持久化目录（空 = 仅内存，测试用）
}

func newIMRelayRegistry(dir string) *imRelayRegistry {
	r := &imRelayRegistry{
		entries: map[string]*imRelayEntry{},
		routes:  map[string]map[string]bool{},
		dir:     dir,
	}
	r.load()
	return r
}

func (r *imRelayRegistry) file() string { return filepath.Join(r.dir, "im_channels.json") }

// load 加载持久化注册（缺失/损坏静默跳过，对齐 hub 数据文件容错规范）。
// 仅恢复 entries；routes 留空，等节点重连 RegisterAgent 后 AttachNode 恢复路由。
func (r *imRelayRegistry) load() {
	if r.dir == "" {
		return
	}
	b, err := os.ReadFile(r.file())
	if err != nil {
		return
	}
	var p struct {
		Entries []imRelayEntry `json:"entries"`
	}
	if json.Unmarshal(b, &p) != nil {
		return
	}
	for _, e := range p.Entries {
		if e.FP == "" || e.NodeID == "" {
			continue
		}
		ee := e
		r.entries[e.FP] = &ee
	}
	if len(r.entries) > 0 {
		slog.Info("IM 中继渠道注册表已恢复（待节点上线恢复路由）", "count", len(r.entries))
	}
}

// save 落盘（调用方持锁；tmp + rename 原子写，防写撕裂）。
func (r *imRelayRegistry) save() {
	if r.dir == "" {
		return
	}
	p := struct {
		Entries []imRelayEntry `json:"entries"`
	}{Entries: make([]imRelayEntry, 0, len(r.entries))}
	for _, e := range r.entries {
		p.Entries = append(p.Entries, *e)
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(r.dir, 0o755)
	tmp := r.file() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		slog.Warn("im_channels.json 写入失败", "err", err)
		return
	}
	if err := os.Rename(tmp, r.file()); err != nil {
		slog.Warn("im_channels.json 原子替换失败", "err", err)
	}
}

// SetChannels 节点全量替换渠道注册（agent.im_state；Enabled=false 时传空表）。
// 返回登记的指纹列表（诊断）。多主：同指纹可被多个节点同时注册（各自建路由）。
func (r *imRelayRegistry) SetChannels(nodeID string, channels []IMRelayChannel) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 幂等全量替换：先摘该节点旧注册（entry + 自身路由位）。
	for fp, e := range r.entries {
		if e.NodeID == nodeID {
			delete(r.entries, fp)
			r.dropRoute(fp, nodeID)
		}
	}
	var fps []string
	now := time.Now().UnixMilli()
	for _, ch := range channels {
		ch.Name = strings.TrimSpace(ch.Name)
		if ch.Name == "" {
			continue
		}
		fp := imRelayChannelFP(ch.Name, ch.PublicID, ch.EID, ch.WebhookToken, ch.GroupHookIDs)
		// 多主：同指纹已被其他节点注册时保留其 entry（渠道凭据等价），仅
		// 更新自身时间戳归属；本节点路由在 addRoute 单独登记（摘除互不牵连）。
		if _, exist := r.entries[fp]; !exist {
			r.entries[fp] = &imRelayEntry{NodeID: nodeID, FP: fp, Channel: ch, TS: now}
		} else {
			r.entries[fp].TS = now
		}
		r.addRoute(fp, nodeID)
		fps = append(fps, fp)
	}
	r.save()
	if len(fps) > 0 {
		slog.Info("IM 中继渠道已注册", "node", nodeID, "count", len(fps), "fps", fps)
	} else {
		slog.Info("IM 中继渠道已注销", "node", nodeID)
	}
	return fps
}

// dropRoute 摘除单节点在线路由（调用方持锁；集合空则删整个 fp 键，防 map 泄漏）。
func (r *imRelayRegistry) dropRoute(fp, nodeID string) {
	if set := r.routes[fp]; set != nil {
		delete(set, nodeID)
		if len(set) == 0 {
			delete(r.routes, fp)
		}
	}
}

// addRoute 登记在线路由位（调用方持锁；同指纹多节点并存 = 多主故障转移）。
func (r *imRelayRegistry) addRoute(fp, nodeID string) {
	if r.routes[fp] == nil {
		r.routes[fp] = map[string]bool{}
	}
	r.routes[fp][nodeID] = true
}

// DetachNode 节点下线：仅摘除自身在线路由（落盘态保留，重连注册恢复）。
// 同指纹其余在线节点自动接管（多主故障转移；全部离线才 503）。
func (r *imRelayRegistry) DetachNode(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for fp := range r.entries {
		r.dropRoute(fp, nodeID)
	}
}

// AttachNode 节点上线：恢复其在线路由（注册表在落盘/内存中已存在；节点注册
// 全量幂等，落盘 entries 与该节点持有渠道一致）。
func (r *imRelayRegistry) AttachNode(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for fp, e := range r.entries {
		if e.NodeID == nodeID {
			r.addRoute(fp, nodeID)
		}
	}
}

// Lookup 按指纹查路由（任一在线节点返回 ok=true）。多主故障转移：entry 归属
// 节点在线时优先（路由稳定可预期）；归属节点离线而同指纹其他节点在线（同机
// 多开读同一 channels.json，渠道等价）时取在线集合中字典序最小节点 ID（确定性
// 选取：同一集合恒定同一目标，不逐回调轮换转发目标）。
func (r *imRelayRegistry) Lookup(fp string) (nodeID string, entry *imRelayEntry, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, exist := r.entries[fp]
	if !exist || e == nil || e.FP == "" {
		return "", nil, false
	}
	set := r.routes[fp]
	if len(set) == 0 {
		return "", e, false
	}
	if set[e.NodeID] {
		return e.NodeID, e, true
	}
	best := ""
	for nid := range set {
		if best == "" || nid < best {
			best = nid
		}
	}
	return best, e, true
}

// List 渠道注册投影（管理端展示）。nodeOnline = 该指纹任一注册节点在线。
func (r *imRelayRegistry) List() []map[string]interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]interface{}, 0, len(r.entries))
	for _, e := range r.entries {
		online := len(r.routes[e.FP]) > 0
		out = append(out, map[string]interface{}{
			"nodeId":       e.NodeID,
			"name":         e.Channel.Name,
			"publicId":     e.Channel.PublicID,
			"eid":          e.Channel.EID,
			"hookIds":      e.Channel.GroupHookIDs,
			"fp":           e.FP,
			"nodeOnline":   online,
			"registeredAt": e.TS,
		})
	}
	return out
}

// ---------- Hub 集成 ----------

// SetIMRelayChannels 节点注册/注销 IM 中继渠道（agent.im_state 处理入口）。
// Enabled=true 全量替换注册，false 注销；随后广播 im.state 全端同步。
func (h *Hub) SetIMRelayChannels(nodeID string, st IMRelayState) {
	if st.Enabled {
		h.imRelay.SetChannels(nodeID, st.Channels)
	} else {
		h.imRelay.SetChannels(nodeID, nil)
	}
	h.Broadcast(TypeIMState, map[string]interface{}{"channels": h.imRelay.List()})
}

// IMRelayChannelList 渠道注册表投影（管理端 /api/im/relays 展示）。
func (h *Hub) IMRelayChannelList() []map[string]interface{} {
	return h.imRelay.List()
}

// handleIMRelays GET /api/im/relays：中继渠道注册表查询（restAuth 保护）。
// 排查「回调 404」用：可直接核对 IM 平台侧 URL 里的指纹是否已在服务端注册、
// 归属节点是否在线（404 = 未注册，503 = 已注册但节点离线）。
func (s *Server) handleIMRelays(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "channels": s.hub.IMRelayChannelList()})
}

// HandleIMRelayHTTP IM 中继回调 HTTP 入口（Server mux 适配后调用）：
//   - POST /im/c/<fp>           公共号回调（pubRespon JSON）
//   - GET|POST /im/g/<fp>/<hookID>  群消息接收
//
// 语义对齐节点侧 IMChannel.handleCallback / handleGroupMsg：立即应答平台
// （公共号 10 秒超时约束），转发在后台 goroutine（任务处理绝不阻塞应答）。
func (h *Hub) HandleIMRelayHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = recover() }() // 单请求异常不拖垮服务器
	switch {
	case strings.HasPrefix(r.URL.Path, imRelayPathPub):
		h.handleIMRelayPub(w, r)
	case strings.HasPrefix(r.URL.Path, imRelayPathGroup):
		h.handleIMRelayGroup(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleIMRelayPub 公共号回调：指纹定位渠道 → 在线校验 → 应答 → 后台转发。
func (h *Hub) handleIMRelayPub(w http.ResponseWriter, r *http.Request) {
	fp := strings.Trim(strings.TrimPrefix(r.URL.Path, imRelayPathPub), "/")
	if fp == "" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	nodeID, entry, online := h.imRelay.Lookup(fp)
	if entry == nil {
		// 渠道未注册（节点未启用中继/配置已变）：404 让平台侧可感知配置漂移。
		slog.Warn("IM 中继回调：渠道未注册", "fp", fp)
		http.Error(w, "unknown channel", http.StatusNotFound)
		return
	}
	if !online {
		// 节点离线：503（平台超时重发语义兜底）。
		slog.Warn("IM 中继回调：节点离线", "fp", fp, "node", entry.NodeID, "name", entry.Channel.Name)
		http.Error(w, "node offline", http.StatusServiceUnavailable)
		return
	}
	// 立即应答平台（对齐节点本地 handleCallback 受理确认形态；同步响应仅受理
	// 确认，任务进展经节点 pubsendV2 异步回传）。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data":      map[string]any{"type": 2, "content": "🕐 已收到，正在处理…"},
		"error":     nil,
		"errorCode": 0,
		"success":   true,
	})
	h.forwardIMInbound(nodeID, IMRelayInbound{
		Kind:         "pub",
		Name:         entry.Channel.Name,
		PublicID:     entry.Channel.PublicID,
		EID:          entry.Channel.EID,
		WebhookToken: entry.Channel.WebhookToken,
		GroupHooks:   entry.Channel.GroupHookIDs,
		Body:         string(body),
		Path:         r.URL.Path,
	})
}

// writeIMChatbotAck chatbot 协议受理应答（云之家 chatbot 文档格式）：
// {"success":true,"data":{"type":2,"content":"..."}} —— data.type=2 与 content
// 均必填，缺省即被平台判「返回的数据格式错误」（3 秒超时窗）。节点侧
// channel_im.go handleGroupMsg 使用同一格式 —— 改一处必改另一处。
func writeIMChatbotAck(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"data":    map[string]any{"type": 2, "content": content},
	})
}

// handleIMRelayGroup 群消息回调：hookID 白名单校验（对齐节点本地路由规则）
// → GET 探活 200 / POST 应答+转发；未配置 hookID 404（防未分配群组乱投）。
// 应答格式对齐云之家 chatbot 文档（data.type=2+content；平台把该 content 作为
// 机器人回复即时发进群，任务结果由节点异步经群机器人 Webhook 推送）。
func (h *Hub) handleIMRelayGroup(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, imRelayPathGroup), "/")
	parts := strings.SplitN(rest, "/", 2)
	fp := ""
	if len(parts) >= 1 {
		fp = strings.TrimSpace(parts[0])
	}
	if fp == "" {
		http.NotFound(w, r)
		return
	}
	hookID := ""
	if len(parts) == 2 {
		hookID = strings.Trim(parts[1], "/")
	}
	nodeID, entry, online := h.imRelay.Lookup(fp)
	if entry == nil {
		// 渠道未注册（配置未保存/未启用/指纹已漂移）：日志落Server端取证，
		// 404 让平台侧可感知配置漂移（对齐公共号路径 handleIMRelayPub）。
		slog.Warn("IM 中继群回调：渠道未注册", "fp", fp, "hook", hookID)
		http.Error(w, "unknown channel (not registered)", http.StatusNotFound)
		return
	}
	// hookID 白名单（未配置 = 该群组未分配给本渠道）。
	validHook := false
	for _, hid := range strings.Split(entry.Channel.GroupHookIDs, ",") {
		if strings.TrimSpace(hid) == hookID && hookID != "" {
			validHook = true
			break
		}
	}
	if !validHook {
		slog.Warn("IM 中继群回调：hookID 不在白名单", "fp", fp, "hook", hookID,
			"name", entry.Channel.Name, "hooks", entry.Channel.GroupHookIDs)
		http.Error(w, "unknown hook for this channel", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		// 探活/浏览器验证（对齐节点本地端点语义）。
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("IM group hook ready: " + hookID + "\n"))
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !online {
		slog.Warn("IM 中继群消息：节点离线", "fp", fp, "hook", hookID, "node", entry.NodeID)
		http.Error(w, "node offline", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// 云之家 chatbot 协议（opendocs #/server-api/im/chatbot）：
	// 创建机器人时平台主动发探活测试请求（robotId=test-robotId，sign 以
	// test-secret 计算），直接回文档示例文案、不转发节点。sign（HmacSHA1+
	// Base64，机器人 appSecret）不在服务端校验：appSecret 在节点侧配置，
	// 安全边界 = 指纹 URL 不可枚举 + hookID 白名单 + 节点侧白名单。
	var probe struct {
		RobotID string `json:"robotId"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.RobotID == "test-robotId" {
		slog.Info("IM 中继群回调：平台机器人探活测试请求", "fp", fp, "hook", hookID)
		writeIMChatbotAck(w, "好风凭借力，送我上青云")
		return
	}
	// 受理应答（chatbot 文档格式）：同步回执仅受理确认文案，任务结果由节点
	// 异步经群机器人 Webhook 推送（不在 3 秒窗内同步等待任务）。
	writeIMChatbotAck(w, "🕐 已收到，正在处理…")
	h.forwardIMInbound(nodeID, IMRelayInbound{
		Kind:         "group",
		Name:         entry.Channel.Name,
		PublicID:     entry.Channel.PublicID,
		EID:          entry.Channel.EID,
		WebhookToken: entry.Channel.WebhookToken,
		GroupHookID:  hookID,
		GroupHooks:   entry.Channel.GroupHookIDs,
		Body:         string(body),
		Path:         r.URL.Path,
	})
}

// forwardIMInbound 后台转发回调载荷到目标节点（应答平台之后的异步一跳；
// 转发失败仅日志 —— 平台超时重发语义兜底，PC 端在帧级有 outbox 补发）。
func (h *Hub) forwardIMInbound(nodeID string, payload IMRelayInbound) {
	go func() {
		defer func() { _ = recover() }()
		if err := h.RouteToAgent(nodeID, TypeServerIMInbound, payload); err != nil {
			slog.Warn("IM 中继转发失败", "node", nodeID, "name", payload.Name,
				"kind", payload.Kind, "hook", payload.GroupHookID, "err", err)
		}
	}()
}

// ---------- Server HTTP 挂载 ----------

// RegisterIMRelayRoutes 挂载 IM 中继回调路由（NewServer 调用；无需鉴权 ——
// 端点由渠道指纹寻址，指纹含渠道五元组哈希不可枚举，载荷在节点侧验签/白名单）。
func (s *Server) RegisterIMRelayRoutes(mux *http.ServeMux) {
	s.route(mux, imRelayPathPub, s.hub.HandleIMRelayHTTP)
	s.route(mux, imRelayPathGroup, s.hub.HandleIMRelayHTTP)
	// 注册表查询（管理端排查回调 404；restAuth 与 /api/nodes 同一凭据体系）。
	s.route(mux, "/api/im/relays", s.handleIMRelays, s.restAuth)
}
