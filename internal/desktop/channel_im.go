package desktop

// channel_im.go 通用 IM 接入通道运行时桥：
//
//	平台无关的通用 IM 接入：平台地址（APIBase / WebhookURL）一律来自设置
//	面板配置，代码不写死任何平台地址；接收侧字段映射（InMap）与发送侧
//	出参模板（OutTemplate）可配置，适配任意 JSON 推送格式。
//
//	1. 群组机器人（单向推送）：
//	   群设置中复制 Webhook（完整地址含授权码，形如
//	   https://<平台域名>/gateway/robot/webhook/send?yzjtype=0&yzjtoken=xxx），
//	   POST 出参模板渲染的 JSON body（默认 {"content":"...@ALL"}，可配其他平台格式）。
//
//	2. 公共号（双向交互，pubSend 协议）：
//	   - 接收：公共号菜单配置的回调 URL 指向本机监听端口 /im/callback，用户 @/进入后
//	     平台 POST JSON（pubRespon 协议：ServiceType/PubAccId/FromUserName/ToUserName/
//	     MsgType/MsgId/Eid/CreateTime/PubToken/Payload.Content），须 10 秒内响应
//	     {"success":true,"data":{"type":2,"content":"..."}}；
//	   - 回传：POST <APIBase>/pubacc/pubsendV2（pubSend 协议，载荷同文档
//	     接口路径 <APIBase>/pubacc/pubsendV2；from 携带
//	     pubtoken=sha1(sort(no,pub,pubkey,nonce,time)) 签名，to 按企业 eid +
//	     用户 openid 定向回传；type=2 纯文本。成功响应 {"code":0,"success":true}
//	     （兼容 v1 形态 {"pubId","sourceMsgId"}）；失败为 code/message JSON 或
//	     自定义 HTTP 状态 5000-5005 + 纯文本 body（v1 残留网关）。
//
//	3. 群消息 HTTP 接收（群组回调，hookID 分配群组）：
//	   本机启动 /im/group/<hookID> 接收端点，每个 hookID 对应一个群组
//	   （独立会话 im-group-<hookID>，回复经群机器人 Webhook 推回群）。
//	   入参格式：InMap 显式字段映射（目标=源路径）优先，未覆盖字段回退宽松解析
//	   （多候选字段 + 嵌套提取，格式无公开文档时的兜底），全量原始报文落日志。
//
// 设计要点（与 channel_lark.go / channel_wecom.go 对齐）：
//   - 内核完全复用桌面端路径：buildKernel / buildRunEngine / makeSayBridge /
//     ApproveAsk（run.askCh 应答通道），会话 ChannelID = "im"，会话 ID =
//     "im-" + 用户 openid（公共号按用户维度隔离；群机器人模式为单向推送，
//     无会话语义）。会话 ID 统一 im- 前缀。
//   - 回调解签：PubToken=sha1(sort(PubAccId,FromUserName,CreateTime,MsgId,pubkey))；
//     签名不一致拒绝（防仿冒，对齐企微 msg_signature 校验语义）。
//   - MsgId 去重（平台超时重发），taskErrCh 收异常（defer 兜底不吞错）。
//   - ask 审批闭环：文本协议回复 y/n/补充内容（复用 normalizeAnswer 三态）。
//   - AllowedUsers 白名单（用户 openid，逗号分隔，空 = 不限制）。
//   - 群接收端点：GroupHookIDs 逗号分隔，hookID 与群一一对应；会话 ID =
//     "im-group-" + hookID（与公共号 openid 命名空间隔离），任务回复
//     固定走群机器人 Webhook（群目标 sendText 前缀分流）。
//
// 发送双路径：公共号模式 pubsend（实际接口 /pubacc/pubsendV2，定向，署名）；
// 群机器人模式 webhook（群内推送，出参模板可配）。频率限制：公共号同一 from
// 内容 1 分钟内不可重复（5005）——发送侧每次生成新的 nonce/time，且本轮任务
// 结束才发下一条，天然规避。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
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

	"kaelvoris/internal/remote"
	"kaelvoris/internal/session"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// imDefaultPort 本地回调监听默认端口（9880 已被企微占用）。
const imDefaultPort = 9881

// imHTTP IM 开放 API 调用客户端（统一超时，防止发送阻塞任务 goroutine）。
var imHTTP = &http.Client{Timeout: 15 * time.Second}

// IMChannel 通用 IM 通道实例（公共号双向 / 群机器人单向 / 群消息 HTTP 接收）。
type IMChannel struct {
	app *App
	cfg ChannelConfig

	online atomic.Bool
	cancel context.CancelFunc

	// MsgId 去重簿（公共号回调平台超时重发防重复受理）。
	seenMu sync.Mutex
	seen   map[string]time.Time

	// senderEids 发送者 openid → 来源企业 ID（回调报文 Eid 簿记）：跨企业用户
	// 发消息时 pubsend 回复 to.no 须投其来源企业——固定投渠道配置 EID 企业会
	// 被网关拒「无权限发送给公司 to.no=...」（2026-10 线上实测）。与 seen 同锁。
	senderEids map[string]string

	mu      sync.Mutex
	running map[string]bool        // 会话目标（openid / group-<hookID>）→ 任务运行中（防重入）
	pending map[string]*pendingAsk // 会话目标 → 等待用户应答的 ask

	// 公共号发送节流（跨任务共享：任务收尾连发 ack+结果时防 5102 频率过快）。
	sendMu   sync.Mutex
	lastSend time.Time
}

// imPubCallback 公共号回调 POST 载荷（pubRespon 协议）。
type imPubCallback struct {
	ServiceType  string `json:"ServiceType"`
	PubAccID     string `json:"PubAccId"`
	FromUserName string `json:"FromUserName"`
	ToUserName   string `json:"ToUserName"`
	MsgType      string `json:"MsgType"`
	MsgID        string `json:"MsgId"`
	Eid          string `json:"Eid"`
	CreateTime   string `json:"CreateTime"` // 毫秒时间戳字符串
	PubToken     string `json:"PubToken"`
	Payload      struct {
		Content string `json:"Content"`
	} `json:"Payload"`
}

// imPubResp 公共号回调响应（10 秒内返回；仅支持文本 type=2）。
// 统一应答包装格式：{"data":{...},"error":null,"errorCode":0,"success":true}。
type imPubResp struct {
	Data struct {
		Type    int    `json:"type"`
		Content string `json:"content"`
	} `json:"data"`
	Error     any  `json:"error"`
	ErrorCode int  `json:"errorCode"`
	Success   bool `json:"success"`
}

// port 本地回调监听端口（默认 9881）。
func (ch *IMChannel) port() int {
	if ch.cfg.CallbackPort > 0 && ch.cfg.CallbackPort < 65536 {
		return ch.cfg.CallbackPort
	}
	return imDefaultPort
}

// ---------- 公网中继（本地 PC 无外网：服务端代持回调入口 → WS 下发节点） ----------
//
// 渠道配置了 RemoteRelay=true 时接收侧走公网中继：不监听本地回调端口，消息
// 经服务端 server.im_inbound 帧下发到 handleRelayed（协议见 internal/remote/
// im_relay.go）；发送侧不变（节点出站直连 IM 平台）。回调 URL 指纹五元组与
// 服务端 imRelayChannelFP 逐字节一致（跨进程契约，注释互指）。

// imRelayChannelFP 渠道指纹（五元组 SHA1 前 12 位）：与服务端 im_relay.go
// imRelayChannelFP 保持逐字节一致 —— 跨进程契约。
func imRelayChannelFP(name, publicID, eid, webhookToken, groupHooks string) string {
	sum := sha1.Sum([]byte(strings.Join([]string{name, publicID, eid, webhookToken, groupHooks}, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}

// relayFP 本渠道的中继指纹（服务端回调 URL 寻址键；五元组即渠道身份）。
func (ch *IMChannel) relayFP() string {
	return imRelayChannelFP(ch.cfg.Name, ch.cfg.PublicID, ch.cfg.EID, ch.cfg.WebhookToken, ch.cfg.GroupHookIDs)
}

// handleRelayed 处理公网中继下发的回调载荷：按 Kind 分流到公共号/群消息
// 处理链（复用既有验签/去重/白名单/会话路由；渠道定位指纹已在
// remoteIMInbound 匹配，此处直接复用本地回调语义）。
func (ch *IMChannel) handleRelayed(msg remote.IMRelayInbound) {
	defer func() { _ = recover() }() // 单帧异常不拖垮连接
	switch msg.Kind {
	case "pub":
		var cb imPubCallback
		if err := json.Unmarshal([]byte(msg.Body), &cb); err != nil {
			slog.Warn("IM 中继公共号回调解析失败", "name", ch.cfg.Name, "err", err)
			return
		}
		ch.onCallback(&cb) // 与本地回调同路（验签/去重/白名单/路由内聚）
	case "group":
		m := ch.parseGroupPayload([]byte(msg.Body))
		ch.onGroupMsg(msg.GroupHookID, m)
	default:
		slog.Warn("IM 中继载荷类型未知", "name", ch.cfg.Name, "kind", msg.Kind)
	}
}

// ---------- 生命周期 ----------

// relayed 中继接收模式（RemoteRelay=true：本地 PC 无外网，接收侧走公网服务端
// 中继，不监听本地回调端口；发送侧不变）。
func (ch *IMChannel) relayed() bool { return ch.cfg.RemoteRelay }

// Start 启动通道（阻塞；goroutine 中运行）。凭据分流：
//   - 公共号模式（PublicID+PubSecret+EID）：启动本地回调 HTTP 服务器接收用户消息；
//   - 群消息接收模式（GroupHookIDs）：同一本地 HTTP 服务器挂 /im/group/<hookID>
//     端点（与公共号可共存，同一 mux/端口）；
//   - 群机器人模式（仅 Webhook）：平台无接收能力，标记可推送后挂起等取消。
//   - 中继模式（RemoteRelay=true）：接收侧不监听本地端口，消息经公网服务端
//     server.im_inbound 帧下发（handleRelayed）；上线后上报 agent.im_state。
func (ch *IMChannel) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	ch.cancel = cancel

	hasPub := ch.cfg.PublicID != "" && ch.cfg.PubSecret != "" && ch.cfg.EID != ""
	hooks := ch.parseGroupHooks()
	if ch.relayed() {
		// 公网中继接收模式：无本地监听；在线状态 + 渠道注册表上报（服务端
		// 路由投影建立后平台回调才能路由到本节点）。
		ch.online.Store(true)
		ch.app.pushChannelStatus()
		ch.app.remoteSendIMRelayState()
		slog.Info("IM 通道已接入公网中继（接收侧）", "name", ch.cfg.Name,
			"pub", hasPub, "groupHooks", hooks)
		<-ctx.Done()
		ch.online.Store(false)
		ch.app.pushChannelStatus()
		ch.app.remoteSendIMRelayState() // 注销同步（Enabled=false）
		return
	}
	if hasPub || len(hooks) > 0 {
		addr := fmt.Sprintf(":%d", ch.port())
		mux := http.NewServeMux()
		if hasPub {
			mux.HandleFunc("/im/callback", ch.handleCallback)
		}
		if len(hooks) > 0 {
			mux.HandleFunc("/im/group/", ch.handleGroupMsg)
		}
		srv := &http.Server{Addr: addr, Handler: mux}
		errCh := make(chan error, 1)
		go func() { errCh <- srv.ListenAndServe() }()
		ch.online.Store(true)
		ch.app.pushChannelStatus()
		slog.Info("IM 回调服务已启动", "name", ch.cfg.Name, "addr", addr,
			"pub", hasPub, "groupHooks", hooks)
		select {
		case err := <-errCh:
			if err != nil && err != http.ErrServerClosed {
				slog.Error("IM 回调服务异常退出", "name", ch.cfg.Name, "err", err)
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

	// 群机器人 Webhook 单向模式。
	ch.online.Store(true)
	ch.app.pushChannelStatus()
	<-ctx.Done()
	ch.online.Store(false)
	ch.app.pushChannelStatus()
}

// Stop 停止通道（关闭回调服务器；进行中任务随会话 cancel 语义由用户停止）。
func (ch *IMChannel) Stop() {
	ch.online.Store(false)
	ch.mu.Lock()
	cancel := ch.cancel
	ch.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// 中继模式：停用即注销服务端路由（im.state Enabled=false；非中继模式无注册）。
	if ch.relayed() {
		ch.app.remoteSendIMRelayState()
	}
}

// ---------- 回调 HTTP 处理（公共号菜单回调，pubRespon 协议） ----------

// handleCallback 公共号回调入口：验签（PubToken）→ 解析 → 10 秒内应答。
// 平台匹配菜单规则后向配置 URL POST；超时 10 秒中断（先应答再异步处理任务）。
func (ch *IMChannel) handleCallback(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = recover() }() // 单请求异常不拖垮服务器
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cb, err := ch.parseCallback(body)
	if err != nil {
		slog.Warn("IM 回调解析失败", "name", ch.cfg.Name, "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// 验签：PubToken=sha1(sort(PubAccId,FromUserName,CreateTime,MsgId,pubkey))，
	// 与公共号密钥不一致拒绝（防仿冒）。
	if ch.cfg.PubSecret != "" {
		want := imPubToken(ch.cfg.PubSecret, cb.PubAccID, cb.FromUserName, cb.CreateTime, cb.MsgID)
		if !strings.EqualFold(want, cb.PubToken) {
			slog.Warn("IM 回调验签失败", "name", ch.cfg.Name, "cb", cb.PubAccID)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	// 先应答后异步处理（平台 10 秒超时；空文本 = 立即返回）。
	// 同步响应仅作受理确认（短文本），任务进展/结果经 pubsend（实际接口
	// /pubacc/pubsendV2）异步回传，避免与 pubSend 推送重复消息；回显用户原文
	// 会造成机器人「复读」观感。
	resp := imPubResp{Success: true}
	if cb.MsgType == "text" {
		resp.Data = struct {
			Type    int    `json:"type"`
			Content string `json:"content"`
		}{Type: 2, Content: "🕐 已收到，正在处理…"}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
	go ch.onCallback(cb)
}

// parseCallback 解析公共号回调 JSON 载荷。
func (ch *IMChannel) parseCallback(body []byte) (*imPubCallback, error) {
	var cb imPubCallback
	if err := json.Unmarshal(body, &cb); err != nil {
		return nil, fmt.Errorf("回调 JSON 解析失败: %w", err)
	}
	if cb.FromUserName == "" {
		return nil, errors.New("回调缺少 FromUserName")
	}
	if cb.MsgID == "" {
		return nil, errors.New("回调缺少 MsgId")
	}
	return &cb, nil
}

// ---------- 群消息 HTTP 接收（hookID 分配群组） ----------

// parseGroupHooks 解析群组 hook ID 列表（逗号分隔，去空去重，保序）。
// hookID 与群组一一对应：会话 ID = "im-group-"+hookID，URL = /im/group/<hookID>。
func (ch *IMChannel) parseGroupHooks() []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range strings.Split(ch.cfg.GroupHookIDs, ",") {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// handleGroupMsg 群消息接收端点：POST /im/group/<hookID>。
// 路由规则：URL 末段必须命中已配置 hookID（未命中 = 404，防未分配群组乱投）；
// GET 返回 200 纯文本（平台/浏览器探活）；POST 宽松解析（InMap 显式映射优先，
// 全量原始报文落日志便于按真实样本精确化），先应答后异步处理任务。
// 受理应答对齐云之家 chatbot 文档（data.type=2+content 必填，3 秒超时窗；
// 与公网中继服务端 im_relay.go writeIMChatbotAck 同格式 —— 改一处必改另一处）；
// 创建机器人时平台主动发探活测试请求（robotId=test-robotId），直接回文档
// 示例文案、不触发任务。
func (ch *IMChannel) handleGroupMsg(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = recover() }() // 单请求异常不拖垮服务器
	hookID := strings.TrimPrefix(r.URL.Path, "/im/group/")
	// 允许末尾斜杠变体；进一步防路径注入（仅允许字母数字-_）。
	hookID = strings.Trim(hookID, "/")
	if hookID == "" {
		http.Error(w, "missing hook id", http.StatusBadRequest)
		return
	}
	valid := false
	for _, h := range ch.parseGroupHooks() {
		if h == hookID {
			valid = true
			break
		}
	}
	if !valid {
		http.Error(w, "unknown hook id", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		// 探活/浏览器验证：返回端点就绪信息。
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("IM group hook ready: " + hookID + "\n"))
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	msg := ch.parseGroupPayload(body)
	// 全量原始报文落日志（格式未知平台按真实推送样本逐步精确化；截断 4KB 防日志膨胀）。
	slog.Info("IM 群消息接收", "name", ch.cfg.Name, "hookID", hookID,
		"remote", r.RemoteAddr, "ct", r.Header.Get("Content-Type"),
		"raw", truncateRunes(string(body), 4096),
		"parsed", map[string]string{"groupID": msg.GroupID, "sender": msg.Sender, "senderName": msg.SenderName, "msgID": msg.MsgID, "text": msg.Text})
	// 云之家 chatbot 平台探活测试请求（robotId=test-robotId，创建机器人
	// 时主动发起）：直接回文档示例文案，不建任务、不触发去重簿。
	var probe struct {
		RobotID string `json:"robotId"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.RobotID == "test-robotId" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(imChatbotAck("好风凭借力，送我上青云"))
		return
	}
	// 先应答后异步处理（chatbot 3 秒超时窗对齐平台约束；空文本也确认受理）。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(imChatbotAck("🕐 已收到，正在处理…"))
	if strings.TrimSpace(msg.Text) == "" {
		return
	}
	go ch.onGroupMsg(hookID, msg)
}

// imChatbotAck chatbot 协议受理应答（云之家 chatbot 文档格式）：
// {"success":true,"data":{"type":2,"content":"..."}} —— data.type=2 与 content
// 均必填，缺省即被平台判「返回的数据格式错误」。与公网中继服务端 im_relay.go
// writeIMChatbotAck 同格式 —— 跨进程契约，改一处必改另一处。
func imChatbotAck(content string) imPubResp {
	var r imPubResp
	r.Success = true
	r.Data.Type = 2
	r.Data.Content = content
	return r
}

// imGroupMsg 群消息解析结果（字段尽力提取，全量原始报文另落日志）。
type imGroupMsg struct {
	GroupID    string // 群组标识（载荷内若携带则优先，否则回退 hookID）
	Sender     string // 发送者标识（openid/userId 等）
	SenderName string // 发送者显示名（昵称/姓名）
	MsgID      string // 消息唯一 ID（去重用；空则 hookID+内容哈希退去重）
	Text       string // 消息文本（嵌套提取）
}

// parseGroupPayload 群消息推送载荷解析。
// 策略：InMap 显式字段映射（目标=源路径，入参格式配置转换）优先 → 未覆盖字段
// 回退宽松解析（JSON 多候选字段名逐层探测 + 嵌套文本递归提取）→ 非 JSON 退纯
// 文本。hookID 作 GroupID/MsgID 兑底。
func (ch *IMChannel) parseGroupPayload(body []byte) imGroupMsg {
	var m imGroupMsg
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var obj map[string]any
		if json.Unmarshal(trimmed, &obj) == nil {
			flat := map[string]string{}
			imFlattenJSON("", obj, flat)
			// InMap 显式映射优先：目标=源路径（group=Payload.GroupId 形态），
			// 未配置字段回退候选键探测（格式无公开文档的兜底）。
			pick := func(target string, fallbacks ...string) string {
				if src := strings.TrimSpace(ch.cfg.GroupInMap[target]); src != "" {
					if v := imLookupPath(flat, src); v != "" {
						return v
					}
				}
				return imFirstKey(flat, fallbacks...)
			}
			// 群组标识：hookID 命名空间独立，但载荷若携带更精确群 ID 则记录（日志/排查用）。
			m.GroupID = pick("group", "groupId", "group_id", "chatId", "chat_id", "roomId", "room_id", "groupID", "GroupID", "gid")
			// chatbot 载荷：operatorOpenid/operatorName（发送者加密 id/显示名）。
			m.Sender = pick("sender", "fromUserId", "from", "senderId", "sender_id", "userId", "user_id", "openid", "FromUserName", "openId", "operatorOpenid")
			m.SenderName = pick("senderName", "fromUserName", "senderName", "sender_name", "nickName", "nickname", "userName", "displayName", "operatorName")
			m.MsgID = pick("msgId", "msgId", "msg_id", "messageId", "message_id", "MsgId", "MsgID", "id")
			m.Text = imMappedText(ch.cfg.GroupInMap, flat, obj)
			return m
		}
	}
	// 非 JSON：退纯文本（平台可能推 form-urlencoded / 纯文本）。
	m.Text = string(trimmed)
	return m
}

// imLookupPath 按 InMap 源路径取值（点分嵌套，如 Payload.Content；数组下标
// msgList[0].Text）。先精确匹配展平键，再大小写不敏感匹配（平台字段大小写
// 风格未知）。空路径或未命中返回空。
func imLookupPath(flat map[string]string, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if v, ok := flat[path]; ok && v != "" {
		return v
	}
	// 大小写不敏感：展平键逐个比对（小规模展平表，线性扫描足够）。
	lp := strings.ToLower(path)
	for k, v := range flat {
		if strings.ToLower(k) == lp && v != "" {
			return v
		}
	}
	return ""
}

// imMappedText 文本提取：InMap 显式 text 映射优先，未配置回退宽松提取。
func imMappedText(inMap map[string]string, flat map[string]string, obj any) string {
	if src := strings.TrimSpace(inMap["text"]); src != "" {
		if v := imLookupPath(flat, src); v != "" {
			return v
		}
	}
	return imExtractText(obj)
}

// imFlattenJSON 展平嵌套 JSON（key 用点连接；数组取 [i] 后缀）。
func imFlattenJSON(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			imFlattenJSON(key, sub, out)
		}
	case []any:
		for i, sub := range t {
			imFlattenJSON(fmt.Sprintf("%s[%d]", prefix, i), sub, out)
		}
	case string:
		out[prefix] = t
	case float64:
		out[prefix] = strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		out[prefix] = strconv.FormatBool(t)
	case nil:
		// 空值不记录
	default:
		out[prefix] = fmt.Sprint(t)
	}
}

// imFirstKey 按候选键序列取首个非空值（不区分大小写：先精确后后缀匹配，
// 如 payload.msgId；平台字段大小写风格未知）。
func imFirstKey(flat map[string]string, keys ...string) string {
	for _, k := range keys {
		for fk, fv := range flat {
			if fv != "" && strings.EqualFold(fk, k) {
				return fv
			}
		}
	}
	for _, k := range keys {
		lk := strings.ToLower(k)
		for fk, fv := range flat {
			if fv != "" && strings.HasSuffix(strings.ToLower(fk), "."+lk) {
				return fv
			}
		}
	}
	return ""
}

// imExtractText 从 JSON 对象提取消息文本：已知文本字段优先（不区分大小写，
// 含嵌套后缀），其次任意 string 字段中最长的（嵌套递归已在展平时覆盖）；
// 过滤明显非文本字段（URL/token/时间戳/ID 类）。
func imExtractText(v any) string {
	flat := map[string]string{}
	imFlattenJSON("", v, flat)
	known := []string{"content", "text", "msg", "message", "body", "msgContent", "msg_content", "contentText"}
	for _, k := range known {
		lk := strings.ToLower(k)
		for fk, fv := range flat {
			fl := strings.ToLower(fk)
			if strings.TrimSpace(fv) == "" {
				continue
			}
			if fl == lk || strings.HasSuffix(fl, "."+lk) {
				return fv
			}
		}
	}
	// 兜底：取最长 string 值，排除 URL/时间戳/token 类字段。
	best := ""
	for k, v := range flat {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "url") || strings.Contains(lk, "token") || strings.Contains(lk, "time") || strings.Contains(lk, "id") {
			continue
		}
		if len(v) > len(best) {
			best = v
		}
	}
	return best
}

// ---------- 消息接收与路由 ----------

// onCallback 回调消息入口：应答路由 或 新任务（对齐 LarkChannel.onMessageReceive）。
func (ch *IMChannel) onCallback(cb *imPubCallback) {
	defer func() { _ = recover() }()
	if cb == nil {
		return
	}
	// 簿记发送者来源企业（回调报文 Eid）：回复 pubsend 的 to.no 定向依据
	//（跨企业用户发消息时回复须投其来源企业，见 pubsendOnce 注释）。
	if cb.Eid != "" {
		ch.rememberSenderEid(cb.FromUserName, cb.Eid)
	}
	// 单位测试（httptest）app 为 nil：仅验证回调协议字段，不触发任务引擎。
	if ch.app == nil {
		return
	}
	if cb.MsgType != "text" {
		ch.sendText(cb.FromUserName, "当前仅支持文本消息（图片/文件解析在后续版本开放）。")
		return
	}
	text := strings.TrimSpace(cb.Payload.Content)
	if text == "" {
		return
	}
	// MsgId 去重（平台超时重发防重复受理）。
	if ch.seenMsg(cb.MsgID) {
		return
	}
	// 白名单（AllowedUsers 逗号分隔用户 openid；空 = 不限制）。
	if allow := strings.TrimSpace(ch.cfg.AllowedUsers); allow != "" {
		ok := false
		for _, u := range strings.Split(allow, ",") {
			if strings.TrimSpace(u) == cb.FromUserName {
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
	p := ch.pending[cb.FromUserName]
	busy := ch.running[cb.FromUserName]
	ch.mu.Unlock()
	if p != nil {
		ch.deliverAnswer(p, text)
		return
	}
	if busy {
		// 运行中不停止：消息入队（FIFO），本轮任务结束后自动继续对话。
		if ch.app.enqueueSessionMsg("im-"+cb.FromUserName, text, nil) {
			ch.sendText(cb.FromUserName, "⏳ 已加入队列（当前任务完成后自动继续）："+truncateRunes(firstLine(text), 60))
			return
		}
		// 竞态：会话恰在本检查后结束运行 → 回落正常发送。
	}
	go ch.runIMTaskSafe(cb.FromUserName, text)
}

// onGroupMsg 群消息入口：hookID → 独立会话 im-group-<hookID>（hookID 与群组
// 一一对应）；任务回复经群机器人 Webhook 推回群（sendText 群前缀分流）。路由
// 语义对齐 onCallback（去重/ask 应答/运行中入队）。
func (ch *IMChannel) onGroupMsg(hookID string, msg imGroupMsg) {
	defer func() { _ = recover() }()
	// httptest（app 为 nil）：仅验证解析与路由字段，不触发任务引擎。
	if ch.app == nil {
		return
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}
	chatID := "group-" + hookID
	// 去重：优先 MsgID；缺失时退内容哈希（平台重发防重复受理；同内容
	// 10 分钟内去重为已知简化）。
	dedupeKey := msg.MsgID
	if dedupeKey == "" {
		sum := sha1.Sum([]byte(hookID + "|" + text))
		dedupeKey = "hash-" + hex.EncodeToString(sum[:8])
	}
	if ch.seenMsg("grp|" + hookID + "|" + dedupeKey) {
		return
	}
	// 群上下文：携带发送者署名（Agent 能分辨群内发言人；对齐群聊转录形态）。
	if msg.SenderName != "" || msg.Sender != "" {
		who := msg.SenderName
		if who == "" {
			who = msg.Sender
		}
		text = "[" + who + "]: " + text
	}

	// 运行中会话：路由为 ask 应答 / 入队（对齐 onCallback）。
	ch.mu.Lock()
	p := ch.pending[chatID]
	busy := ch.running[chatID]
	ch.mu.Unlock()
	if p != nil {
		ch.deliverAnswer(p, text)
		return
	}
	if busy {
		if ch.app.enqueueSessionMsg("im-"+chatID, text, nil) {
			ch.sendText(chatID, "⏳ 已加入队列（当前任务完成后自动继续）："+truncateRunes(firstLine(text), 60))
			return
		}
	}
	go ch.runIMTaskSafe(chatID, text)
}

// seenMsg MsgId 去重；簿记超 1024 条时清理 10 分钟前的旧记录。
func (ch *IMChannel) seenMsg(id string) bool {
	ch.seenMu.Lock()
	defer ch.seenMu.Unlock()
	if ch.seen == nil {
		ch.seen = map[string]time.Time{}
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
func (ch *IMChannel) deliverAnswer(p *pendingAsk, text string) {
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

// runIMTaskSafe 任务 goroutine 的 panic 防护（对齐 runLarkTaskSafe）：
// 独立 goroutine 未捕获 panic 会杀死整个进程，拦截后回执 IM 错误提示。
func (ch *IMChannel) runIMTaskSafe(openid, text string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("IM 任务 panic（已拦截，进程保持运行）",
				"user", openid, "panic", r, "stack", string(debug.Stack()))
			ch.sendText(openid, "❌ 任务内部异常终止（已恢复）："+truncateRunes(fmt.Sprint(r), 500))
		}
	}()
	ch.runIMTask(openid, text)
}

// runIMTask 以会话（ChannelID="im"）运行一条指令；事件回传 IM + 推送前端。
func (ch *IMChannel) runIMTask(openid, text string) {
	a := ch.app
	sessionID := "im-" + openid

	// 内核组件惰性构建（与 runTask / runLarkTask 一致）。
	a.kmu.Lock()
	needBuild := a.modeMgr == nil || a.skillMgr == nil
	a.kmu.Unlock()
	if needBuild {
		if err := a.buildKernel(); err != nil {
			ch.sendText(openid, "❌ "+err.Error())
			return
		}
	}
	a.kmu.Lock()
	modeMgr, skillMgr := a.modeMgr, a.skillMgr
	a.kmu.Unlock()

	// 岗位分配收口（doc/09-设计方案 Phase 2-①，§4.1）：建会话前统一
	// ResolveAssignment 解析绑定（固定/降级/岗位池/裸跑），结果固化进会话快照；
	// 解析报错（员工不存在/池空）回执 IM 并终止本轮（§2 池空不静默降级）。
	assign, ok := ch.resolveChannelAssignment()
	if !ok {
		ch.sendText(openid, "❌ 岗位分配失败："+assign.errMsg)
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
			ChannelID:  "im",
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
	run.mcpProbe = text // MCP 惰性加载：IM 任务文本作为激活信号
	// §10 降级留痕（会话可见）：以 say text 写入会话历史 + 前端推送（run 已建）。
	emitAssignNotice(a, sess, run, sessionID, assign.notice)
	if snap := sess.Snapshot(); len(snap.Todos) > 0 {
		run.todo.Set(snap.Todos)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	ch.mu.Lock()
	ch.running[openid] = true
	ch.mu.Unlock()
	defer func() {
		if run.cleanup != nil {
			run.cleanup()
		}
		cancel()
		ch.mu.Lock()
		delete(ch.pending, openid)
		delete(ch.running, openid)
		ch.mu.Unlock()
		a.runsMu.Lock()
		delete(a.runs, sessionID)
		a.runsMu.Unlock()
		a.registry.Remove(sessionID)
		a.pushChannelStatus()
		// 会话级状态修复（同 runTask）：任务结束恒报本会话 false，不报全局
		// IsRunning()——否则并发任务会把已完成的 IM 会话卡在「运行中」。
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
		ch.sendText(openid, "🚀 已开始处理："+truncateRunes(firstLine(curText), 60))

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

		// say 双路桥：桌面端事件桥（历史留痕 + 前端推送）+ IM 回传（定型 text/error）。
		desktopSay := a.makeSayBridge(sess, run, sessionID)
		say := func(m message.ClineMessage) {
			desktopSay(m)
			ch.pushToIM(openid, m)
		}

		result, runErr := engine.RunInMode(ctx, a.GetMode(), curText, false, 0, say, ch.wrapAsk(run, ctx, openid))
		flushMCP()
		if runErr != nil {
			ch.sendText(openid, "❌ 任务失败："+truncateRunes(runErr.Error(), 500))
		} else {
			ch.sendText(openid, "✅ 任务完成\n\n"+truncateRunes(result, 3000))
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
		ch.sendText(openid, "⏭️ 上一轮已完成，继续处理："+truncateRunes(firstLine(curText), 60))
	}
}

// wrapAsk 引擎 ask 桥：completion_result 自动通过；其余推送提问 + 文本协议应答
// （对齐 LarkChannel.wrapAsk）。
func (ch *IMChannel) wrapAsk(run *taskRun, ctx context.Context, openid string) func(message.ClineMessage) (message.AskResponse, string) {
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
			ch.sendText(openid, prompt)
		}
		ch.mu.Lock()
		ch.pending[openid] = &pendingAsk{sessionID: run.id, chatID: openid}
		ch.mu.Unlock()
		defer func() {
			ch.mu.Lock()
			delete(ch.pending, openid)
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

// ---------- IM 消息发送 ----------

// apiBase IM 平台 API 根：完全来自渠道配置（设置面板可配，代码不写死
// 任何平台地址）；未配置返回空串，由发送侧前置校验拦截并提示。
func (ch *IMChannel) apiBase() string {
	return strings.TrimRight(strings.TrimSpace(ch.cfg.APIBase), "/")
}

// sendText 发送文本：群会话（"group-<hookID>" 前缀）固定走群机器人 Webhook 推回
// 群（群消息无法经公共号定向，出参模板渲染请求体）；公共号模式（PublicID+
// PubSecret+EID）走 pubsend 定向回传（实际接口 /pubacc/pubsendV2，openid 定向）；
// 否则走群机器人 Webhook。
func (ch *IMChannel) sendText(toUser, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if strings.HasPrefix(toUser, "group-") {
		ch.sendWebhook(text)
		return
	}
	hasPub := ch.cfg.PublicID != "" && ch.cfg.PubSecret != "" && ch.cfg.EID != ""
	if hasPub {
		ch.sendPubMessage(toUser, text)
		return
	}
	ch.sendWebhook(text)
}

// sendWebhook 群机器人推送：完整 Webhook URL 优先（WebhookURL 配置，任意平台
// 通用）；否则回退 APIBase + 群机器人路径 + WebhookToken（需平台兼容该路径形态）。
// 请求体经出参模板渲染（OutTemplate 含 {{text}} 占位符；空则默认
// {"content":"<text>"}）。
func (ch *IMChannel) sendWebhook(text string) {
	u := strings.TrimSpace(ch.cfg.WebhookURL)
	if u == "" {
		base := ch.apiBase()
		if base == "" {
			slog.Error("IM webhook 发送失败：未配置 Webhook URL / API 根（设置面板填写）", "name", ch.cfg.Name)
			return
		}
		u = base + "/gateway/robot/webhook/send?yzjtype=0&yzjtoken=" + url.QueryEscape(ch.cfg.WebhookToken)
	}
	payload := ch.cfg.OutTemplate
	if strings.TrimSpace(payload) == "" {
		// 默认出参：{"content":...} 形态（其他平台配 OutTemplate 覆盖）。
		payload = `{"content":"{{text}}"}`
	}
	body := imRenderTemplate(payload, text)
	resp, err := imHTTP.Post(u, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		slog.Error("IM webhook 发送失败", "name", ch.cfg.Name, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Error("IM webhook 推送被拒", "name", ch.cfg.Name, "status", resp.StatusCode)
	}
}

// imRenderTemplate 出参模板渲染：{{text}} → JSON 字符串安全的回复文本。
// 模板是 JSON 报文形态，插入前对文本做 JSON 字符串转义（引号/反斜杠/
// 控制符，含换行），否则多行/含引号文本会产出非法 JSON。无占位符且模板
// 非空 → 原样返回（平台要求固定报文时使用）。
func imRenderTemplate(tpl, text string) string {
	if !strings.Contains(tpl, "{{text}}") {
		return tpl
	}
	b, _ := json.Marshal(text) // JSON 字符串字面量（含引号）
	if len(b) >= 2 {
		text = string(b[1 : len(b)-1]) // 去掉外层引号，仅取转义后内容
	}
	return strings.ReplaceAll(tpl, "{{text}}", text)
}

// sendPubMessage 公共号定向回传（pubsendV2 接口 POST /pubacc/pubsendV2，type=2
// 纯文本，pubSend 协议（接口 <APIBase>/pubacc/pubsendV2）；
// 载荷协议同 v1）——详见 pubsendOnce 注释。
// 每次发送生成新 nonce/time（规避平台 1 分钟同 from 内容频率限制 5005）；
// 业务失败（code 5005 内容重复 / 5102 频率过快）退避重试一次，重试时文本加
// 空格后缀使内容变化（5005 判据是「同一 from 同一内容」，内容变则绕开）。
// 全量响应检查（HTTP 状态码 / 非 JSON 响应体 / 业务 code），失败落日志带
// openid + 响应摘要（线上取证依赖：工作 PC app.log grep "IM pubsend"）。
func (ch *IMChannel) sendPubMessage(openid, text string) {
	if strings.TrimSpace(openid) == "" {
		slog.Error("IM 公共号发送失败：目标用户 openid 为空", "name", ch.cfg.Name)
		return
	}
	// 渠道级节流：距上次发送不足 600ms 先等待（平台 5102 频率过快兜底；
	// 任务收尾连发 ack+结果多条时生效，单发场景零影响）。
	ch.sendMu.Lock()
	if !ch.lastSend.IsZero() {
		if d := time.Since(ch.lastSend); d < imSendGap {
			time.Sleep(imSendGap - d)
		}
	}
	ch.lastSend = time.Now()
	ch.sendMu.Unlock()

	for attempt := 0; ; attempt++ {
		_, fail := ch.pubsendOnce(openid, text)
		if fail == nil {
			return
		}
		// 仅频率限制类失败重试一次（5005 同内容重复 → 加空格变内容规避；
		// 5102 频率过快 → 退避后重发）；密钥错（5004）/用户错（5003）重试无意义。
		retryable := fail.code == 5005 || fail.code == 5102
		if !retryable || attempt >= 1 {
			slog.Error("IM pubsend 发送失败", "name", ch.cfg.Name, "openid", openid,
				"toNo", fail.toNo, "code", fail.code, "message", fail.message,
				"status", fail.status, "detail", fail.detail)
			return
		}
		slog.Warn("IM pubsend 频控退避重试", "name", ch.cfg.Name, "openid", openid, "code", fail.code)
		time.Sleep(1100 * time.Millisecond) // 跨过平台 1 秒级频控窗口
		if fail.code == 5005 {
			text += " " // 内容变化规避「同一内容 1 分钟重复」判定
		}
	}
}

// imSendGap 公共号发送最小间隔（渠道级节流，防 5102 频率过快）。
const imSendGap = 600 * time.Millisecond

// imSendFail pubsend 失败详情（日志取证用）。
type imSendFail struct {
	code    int    // 业务 code（HTTP 层失败为 0）
	message string // 平台 message 字段
	status  int    // HTTP 状态码（业务层失败为 0）
	detail  string // 响应体摘要（非 JSON / 异常状态时取证）
	toNo    string // 目标企业 no（to.no；跨企业权限拒绝时取证）
}

// rememberSenderEid 簿记发送者 openid → 来源企业 ID（回调报文 Eid）：pubsend
// 回复 to.no 的定向依据（跨企业用户回复须投其来源企业，见 pubsendOnce 注释）。
// 与 seen 去重簿同锁（seenMu），容量控制随 seen 清理路径一并过期——不单独
// 淘汰，openid 数量级 = 活跃用户数，内存占用可忽略。
func (ch *IMChannel) rememberSenderEid(openid, eid string) {
	ch.seenMu.Lock()
	defer ch.seenMu.Unlock()
	if ch.senderEids == nil {
		ch.senderEids = map[string]string{}
	}
	ch.senderEids[openid] = eid
}

// senderEidOf 取发送者来源企业 ID（未记录返回空）。
func (ch *IMChannel) senderEidOf(openid string) string {
	ch.seenMu.Lock()
	defer ch.seenMu.Unlock()
	return ch.senderEids[openid]
}

// pubsendOnce 单次 pubsend 请求（pubsendV2 接口 POST /pubacc/pubsendV2）：成功返回
// (true, nil)；失败返回 (false, fail)。
//
// 线上实测记录：文档接口 /pubacc/pubsend
// 在该私有云网关上返回 openresty 404（路由未部署）；而 /pubacc/pubsendV2
// 存在且活着，载荷协议与 pubsend v1 相同（from/to/type/msg）。因此发送侧
// 默认走 pubsendV2；响应双形态兼容（平台新旧版本共存）：
//   - pubsendV2 成功：HTTP 200 + {"code":0,"success":true}（可能附带 data）；
//   - 文档 pubsend 成功形态兼容：{"pubId","sourceMsgId"}（无 success 字段）
//     ——pubId/sourceMsgId 任一非空即成功；
//   - pubsendV2 失败：HTTP 200 + {"code":500,"message":"...","success":false}
//     （频控类 code 5005/5102 同空间）；
//   - 文档 pubsend 失败（v1 残留网关）：自定义 HTTP 状态码 5000-5005 + 纯文本。
//
// to.no 目标企业：发送者来源企业（回调报文 Eid 簿记，senderEidOf）优先——
// 跨企业用户给公共号发消息时，回复仍投渠道配置 EID 企业会被网关以
// 「无权限发送给公司 to.no=...」拒绝（2026-10 线上实测：to.no 必须与
// 发消息用户所在企业一致）；未记录（无 Eid 载荷/进程重启后）回退渠道配置 EID。
// from/pubtoken 恒用渠道配置（公共号自身身份，不随目标企业变）。
// 网络 / HTTP >= 300 / 非 JSON / success=false 均视为失败。
func (ch *IMChannel) pubsendOnce(openid, text string) (bool, *imSendFail) {
	nonce := imNonce()
	now := strconv.FormatInt(time.Now().UnixMilli(), 10)
	token := imPubToken(ch.cfg.PubSecret, ch.cfg.EID, ch.cfg.PublicID, nonce, now)
	toNo := ch.cfg.EID
	if e := ch.senderEidOf(openid); e != "" {
		toNo = e
	}
	payload := map[string]any{
		"from": map[string]string{
			"no":       ch.cfg.EID,
			"pub":      ch.cfg.PublicID,
			"time":     now,
			"nonce":    nonce,
			"pubtoken": token,
		},
		"to": []map[string]any{
			{
				"no":   toNo,
				"user": []string{openid},
			},
		},
		"type": 2,
		"msg":  map[string]string{"text": truncateRunes(text, 3000)},
	}
	body, _ := json.Marshal(payload)
	// pubsendV2：私有云网关实际部署的接口（v1 /pubacc/pubsend 404，见
	// 函数注释的探测记录）。路径不再写死 v1 文档形态。
	req, err := http.NewRequest(http.MethodPost, ch.apiBase()+"/pubacc/pubsendV2", bytes.NewReader(body))
	if err != nil {
		return false, &imSendFail{detail: "请求构建失败: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := imHTTP.Do(req)
	if err != nil {
		// 文档 pubsend 失败响应使用非标准 4 位 HTTP 状态码（HTTP/1.1 5000-5005），
		// Go net/http 解析失败直接断连（malformed HTTP status code）——从错误文本
		// 提取业务码，5005 频控重试判定才生效。
		if code, ok := imParseMalformedStatus(err.Error()); ok {
			return false, &imSendFail{code: code, status: code, toNo: toNo, detail: truncateRunes(err.Error(), 200)}
		}
		return false, &imSendFail{detail: "网络错误: " + err.Error(), toNo: toNo}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		// 文档 pubsend 失败形态：自定义 HTTP 状态码 5000-5005（如 5000 参数错误、
		// 5003 用户错、5004 密钥错、5005 频率限制）+ 纯文本 body——status 值即
		// 业务错误码（同一数字空间），供上层频控重试判定。
		return false, &imSendFail{code: resp.StatusCode, status: resp.StatusCode, toNo: toNo,
			detail: truncateRunes(strings.TrimSpace(string(raw)), 200)}
	}
	var out struct {
		Code        int    `json:"code"`
		Message     string `json:"message"`
		Success     bool   `json:"success"`
		PubID       string `json:"pubId"`
		SourceMsgID string `json:"sourceMsgId"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		// 平台异常时可能返回 HTML 错误页等非 JSON 响应，无 Success 字段判定——
		// 旧实现此处静默通过（用户端收不到回复且无日志），现按失败处理。
		return false, &imSendFail{status: resp.StatusCode, toNo: toNo, detail: truncateRunes(strings.TrimSpace(string(raw)), 200)}
	}
	// 成功判定兼容双形态：文档 pubsend 成功返回 {"pubId","sourceMsgId"}
	//（无 success 字段，有此即成功）；pubsendV2 形态 success=true。两者皆无 →
	// 业务失败（code/message 落日志）。
	if !out.Success && out.PubID == "" && out.SourceMsgID == "" {
		return false, &imSendFail{code: out.Code, message: out.Message, status: resp.StatusCode, toNo: toNo}
	}
	return true, nil
}

// ---------- IM 签名工具 ----------

// imPubToken 公共号签名：pubtoken=sha1(sort(no,pub,pubkey,nonce,time))。
func imPubToken(pubkey string, args ...string) string {
	parts := append([]string{pubkey}, args...)
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

// imNonce 随机数（UUID v4 格式：32 hex 分段；仅需唯一性，crypto/rand）。
func imNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// imParseMalformedStatus 从 net/http 断连错误中提取平台自定义状态码：
// pubsend 失败响应状态行为 `HTTP/1.1 5005 ...`（4 位非 HTTP 规范），Go 客户端
// 报 `net/http: HTTP/1.x transport connection broken: malformed HTTP status
// code "5005"`——提取出 5005 供上层频控重试判定。未命中返回 (0, false)。
func imParseMalformedStatus(errText string) (int, bool) {
	const marker = `malformed HTTP status code "`
	i := strings.Index(errText, marker)
	if i < 0 {
		return 0, false
	}
	rest := errText[i+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end <= 0 {
		return 0, false
	}
	code, err := strconv.Atoi(rest[:end])
	if err != nil || code < 4000 || code > 5999 {
		return 0, false
	}
	return code, true
}

// pushToIM 引擎 say 事件推送 IM：定型 text/error 帧转发（partial 流式帧不入）。
func (ch *IMChannel) pushToIM(openid string, m message.ClineMessage) {
	if m.Partial {
		return
	}
	switch m.Say {
	case message.SayText:
		if t := strings.TrimSpace(m.Text); t != "" {
			ch.sendText(openid, truncateRunes(t, 3000))
		}
	case message.SayError:
		ch.sendText(openid, "⚠️ "+truncateRunes(m.Text, 800))
	}
}
