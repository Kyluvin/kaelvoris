package remote

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// clipStr 截断字符串（错误信息防超长；与 tools 包 clipStr 同语义）。
func clipStr(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// ClientCallbacks PC 节点（桌面端）对接回调：把远程指令桥接到本地内核。
//
// 与 channel_lark.go 的内核复用思路一致：桌面端提供
// runTaskText（新任务/续聊）、CancelTask（取消）、AnswerAsk（审批）、
// SessionMessages（历史），远程层只做协议与重连。
type ClientCallbacks struct {
	// RunTask 执行任务（sessionID 恒定；运行中调用 = 续聊入队）。
	// mode 非空时先切换执行模式（远端指定；PC 端唯一执行面）。
	// 返回是否受理。taskID 为服务端任务 ID（事件/终态回传需引用；空 = 续聊
	// server.chat 无新任务 ID，保留既有映射）。
	RunTask func(sessionID string, text string, images []string, mode string, taskID string) bool
	// Cancel 取消会话任务。sessionID 可能为空（旧服务端不下发）→ 由桌面端
	// 按 taskID 反查兑底；taskID 亦可能为空（仅按会话取消）。
	Cancel func(sessionID string, taskID string)
	// ApproveAsk 审批应答。
	ApproveAsk func(sessionID string, response string, text string) error
	// IsRunning 会话是否运行中。
	IsRunning func(sessionID string) bool
	// SessionMessages 会话消息（ClineMessage 结构原样序列化）。
	SessionMessages func(sessionID string) []interface{}
	// SessionTodos 会话任务清单（随历史回传，远端渲染进度）。
	SessionTodos func(sessionID string) []interface{}
	// DeleteSession 删除节点本地会话存档（会话删除联动 PC 端；异步执行）。
	// 空实现 = 旧桌面端不支持，服务端记录删除不受影响。
	DeleteSession func(sessionID string)
	// AnalyzeSession 会话 AI 分析（节点侧 LLM 生成报告；异步，结果经
	// SendAnalyzeDone 回传；空实现 = 旧桌面端不支持，服务端回 op.result 失败）。
	Analyze func(sessionID string)
	// CleanupSessions 批量清理会话（节点侧；mode 分流：archive（空）= 逐会话
	// 「分析→生成文档→归档删除」；clear = 直接清空本地会话存档；summarize = 先
	// 汇总未完成任务为续接任务再清空。异步执行，结果经 SendCleanupDone 回传；
	// 空实现 = 旧桌面端不支持，服务端不动，移动端无结果卡片）。
	CleanupSessions func(mode string)
	// VerifyTasks 核实本节点任务真实状态（远端发起；节点按本地会话档案判定
	// 每个任务的真实状态后经 SendVerifyReport 回传；异步执行不阻塞读循环；
	// 空实现 = 旧桌面端不支持，服务端广播 verify.done(0 fixed) 兑底）。
	// requestedBy 为发起端标记（server.verify_tasks 载荷携带，服务端注入），
	// verifyAll 为全量核实哨兵（TaskIDs 为空时服务端下发）：节点核实
	// remoteTaskOf 中全部会话的真实状态（覆盖被 watchdog 误标 failed 的任务）。
	VerifyTasks func(taskIDs []string, requestedBy string, verifyAll bool)
	// GroupCreate 创建群聊（GroupServerReq{Name, Members}；节点侧创建并落盘
	// ~/.kaelvoris/groups/<gid>.json，回传 agent.group_state；空实现 = 旧桌面端不支持）。
	GroupCreate func(req GroupServerReq)
	// GroupMsg 群消息（GroupServerReq{GroupID, Text}；节点广播给成员/群助手，
	// 消息历史节点持久化；空实现 = 旧桌面端不支持）。
	GroupMsg func(req GroupServerReq)
	// GroupTask 群写任务（GroupServerReq{GroupID, Instruction, AssignedTo}；
	// 节点侧群助手协调派发给成员员工执行；空实现 = 旧桌面端不支持）。
	GroupTask func(req GroupServerReq)
	// GroupJoin / GroupLeave 添加/移出群成员（GroupServerReq{GroupID, AgentID}）。
	GroupJoin  func(req GroupServerReq)
	GroupLeave func(req GroupServerReq)
	// GroupSync 拉取群状态（GroupServerReq{GroupID}；节点回传 agent.group_state
	// 附带消息历史；空实现 = 旧桌面端不支持）。
	GroupSync func(req GroupServerReq)
	// ClearSessions 清空节点本地全部会话存档（clear.tasks Mode=clear 联动；
	// 异步执行不回执 —— 服务端受理时已清空记录并广播，节点回执会重复广播；
	// 空实现 = 旧桌面端不支持，节点本地存档残留，下次清理入口再清）。
	ClearSessions func()
	// Modes 当前全部可选执行模式（注册时上报；空 = 节点未提供）。
	Modes func() []ModeInfo
	// CurrentMode 当前执行模式 slug。
	CurrentMode func() string
	// Account/Pass 当前登录账号凭据（P-Account 指挥权隔离，去令牌化 2026-10）：
	// 注册帧携带上报，服务端经统一账号表校验后绑定节点归属（Node.Owner）并
	// 据此校验客户端指挥权（同账号才能指挥）。Account/Pass 空/nil = 未登录：
	// 节点仍可注册上线（只读状态展示），但不可被任何客户端指挥。
	Account func() string
	// Pass 登录密码快照（注册帧与 Account 同源上报；空 = 匿名注册）。
	Pass func() string
	// SetMode 远端切换执行模式（立即生效，影响下一次任务）。
	SetMode func(slug string) error
	// GetConfig 读取节点配置投影（远端「PC 端设置」页展示；APIKey 掩码）。
	GetConfig func() NodeConfig
	// SetConfig 合并保存节点配置（远端下发；仅更新非 nil 字段，
	// APIKey 非空才覆盖；保存后按新配置重建内核，下一任务生效）。
	SetConfig func(sc SetConfig) error
	// SkillList 返回技能元数据全量（服务端 skills.list / save/delete 后刷新；
	// 空 = 旧桌面端不支持技能库，服务端回空列表）。
	SkillList func() []SkillMeta
	// SkillGet 读取技能 SKILL.md 原始全文（编辑器回显；builtin 可读只读）。
	SkillGet func(name string) (string, error)
	// EmployeeList 返回节点数字员工元数据全量（群聊建群选成员数据源；
	// 空 = 旧桌面端不支持，服务端回空列表）。
	EmployeeList func() []EmployeeMeta
	// SkillSave 保存/新建技能（统一落 project 层；成功后自动刷新列表回传）。
	SkillSave func(name, content string) error
	// SkillDelete 删除技能（builtin 禁删；成功后自动刷新列表回传）。
	SkillDelete func(name string) error
	// Roadmap 自主开发规划板回调（移动端「AR 自主开发」页数据链路；
	// 空实现 = 旧桌面端不支持，服务端回空快照）。
	// RoadmapData 返回规划板全量投影（roadmap.list 拉取 / 变更后回传）。
	RoadmapData func() RoadmapDataPayload
	// RoadmapPlan 触发 AI 规划（guidance 用户引导意见可空；异步 LLM 调用，
	// 提案产出后节点推 agent.roadmap_data 广播刷新；返回错误则 server.error 回报）。
	RoadmapPlan func(guidance string) error
	// RoadmapDev 执行自主开发（IDs 必填 = 用户勾选的提案；服务端已校验非空，
	// 节点侧二次校验兜底）：批准勾选项后启动开发。返回错误 = server.error 回报。
	RoadmapDev func(ids []string, mode string, parallel int) error
	// RoadmapDelete 删除规划条目。
	RoadmapDelete func(id string) error
	// RoadmapDeleteBatch 批量删除规划条目（已完成/失败清理；开发中条目跳过）。
	RoadmapDeleteBatch func(ids []string) error
	// RoadmapRetry 一键重跑失败/暂停条目（failed/blocked → approved 回队；
	// 空 = 旧节点不支持，服务端回 server.error）。
	RoadmapRetry func(id string) error
	// RoadmapApprove 批量审核通过（IDs 必填；只置 approved 入排期，不立即开发）。
	// 空 = 旧节点不支持，服务端回 server.error。
	RoadmapApprove func(ids []string) error
	// RoadmapAuto 开启完全自主开发（guidance 提示词可空；parallel 并发线程数；
	// minDeveloping 最低并行开发数，0 = PC 端配置/默认）。
	// 空 = 旧节点不支持，服务端回 server.error。
	RoadmapAuto func(guidance string, parallel int, minDeveloping int) error
	// OnNodeIDAligned 节点 ID 对齐回调（注册应答学习到令牌家族基础 ID 时触发；
	// 宿主应持久化新 ID 到 remote.json，重启后仍对齐。空实现 = 仅运行时对齐）。
	OnNodeIDAligned func(baseNodeID string)
	// IMInbound IM 中继回调下发（公网服务端代持 IM 平台回调，经本节点执行；
	// 载荷含渠道定位五元组与原始请求体。空实现 = 旧节点不支持，服务端转发仅日志）。
	IMInbound func(msg IMRelayInbound)
	// OnStatus 状态回调（连接状态变化；供前端推送）。
	OnStatus func(online bool, err string)
	// OnOnline 注册成功回调（每次上线触发一次；节点可在此补传离线期间
	// 的会话历史到服务端镜像，修复：断线期间执行的任务回手机端点开空白）。
	// 可空 = 旧桌面端不支持补传。
	OnOnline func()
}

// Client PC 节点连接器：连接服务端 /ws/agent，注册、心跳、断线重连、指令处理。
type Client struct {
	mu sync.Mutex
	// writeMu conn 写锁：gorilla 连接仅允许单写者，而本结构存在多个并发写方
	//（读循环心跳/注册应答、任务 goroutine 事件流、SetMode 回报 SendModeUpdate），
	// 不加锁则帧交错损坏协议（服务端 hub 侧 clientConn/agentConn 均有 sendMu 同款保护）。
	writeMu sync.Mutex

	url      string // ws://host/ws/agent
	token    string
	nodeID   string
	name     string
	platform string
	arch     string
	hostname string
	httpBase string // 服务端 HTTP 根地址（http://host；上传生成图片用，SetHTTPBase 注入）

	ws      *websocket.Conn
	cancel  chan struct{}
	running bool
	online  bool

	cb ClientCallbacks

	// 运行中任务计数（心跳上报）。
	taskMu     sync.Mutex
	runningIDs map[string]bool // sessionID → running

	// 挂起中的 ask（sessionID → 有 ask 等待；ask_reply 路由用）。
	askSessions map[string]bool

	// 上行 outbox（P-Reliable）：未确认消息队列（内存 + journal 落盘），
	// 发送线程独立消费；ack 删除、断线保留、重连补发 —— 消息永不静默丢弃。
	outboxMu    sync.Mutex
	outbox      []*outboxMsg
	journalPath string
	journalMu   sync.Mutex
	flushStop   chan struct{}
	senderWake  chan struct{} // 唤醒发送线程（新消息入队/重连成功）
	ackNotify   chan struct{} // ack/nack 到达通知（等待方重查队列；容量 1 合并）

	reconnectDelay time.Duration

	// 计时参数（测试可注入；生产默认值见 NewClient）：
	// heartbeatInterval agent.heartbeat 上行周期（默认 15s）。
	heartbeatInterval time.Duration
}

// NewClient 创建连接器（url 形如 ws://host:port/ws/agent）。
func NewClient(url, token, nodeID, name, versionStr, hostname, platform, arch string, cb ClientCallbacks) *Client {
	return &Client{
		url:            url,
		token:          token,
		nodeID:         nodeID,
		name:           name,
		platform:       platform,
		arch:           arch,
		hostname:       hostname,
		cb:             cb,
		cancel:         make(chan struct{}),
		runningIDs:     map[string]bool{},
		askSessions:    map[string]bool{},
		reconnectDelay: 3 * time.Second,
		// 心跳 15s（服务端 agentReadIdleTimeout=75s 的 5 倍余量）；
		// 读循环不设超时：下行帧无周期（无指令/ack 时静默），不可作判活
		// 依据；断线闭环由心跳写失败感知（ws.Close → 读错误 → 重连）。
		heartbeatInterval: 15 * time.Second,
	}
}

// SetJournalPath 注入上行 journal 文件路径（Start 之前调用；空 = 仅内存队列不落盘）。
// 桌面端按节点派生身份命名（agent_outbox_<nodeID>.jsonl），多开实例互不串扰。
func (c *Client) SetJournalPath(path string) {
	c.mu.Lock()
	c.journalPath = strings.TrimSpace(path)
	c.mu.Unlock()
}

// HTTPBase 读取服务端 HTTP 基地址（nil 安全；空 = 未连接）。
// 图生视频工作流用：本地图上传 /api/images 后与基地址拼接成公网 URL
// （视频网关要求媒体公网可访问，见 internal/tools/media_tools.go Upload）。
func (c *Client) HTTPBase() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.httpBase
}

// SetHTTPBase 注入服务端 HTTP 基地址（http://host:port，节点上传生成图片用；
// WS 地址 ws(s)://host:port/ws/agent 反推同源 http(s) 根地址）。
func (c *Client) SetHTTPBase(base string) {
	c.mu.Lock()
	c.httpBase = strings.TrimRight(strings.TrimSpace(base), "/")
	c.mu.Unlock()
}

// UploadImage 上传生成图片到服务端（POST /api/images，multipart；节点令牌鉴权）。
// 返回服务端相对 URL（/img/<file>）；失败返回空串（不阻塞任务主链路，仅日志）。
func (c *Client) UploadImage(path, taskID, sessionID string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取图片失败: %w", err)
	}
	return c.UploadImageBytes(data, filepath.Base(path), taskID, sessionID)
}

// UploadImageBytes 上传图片字节流（multipart 表单；节点令牌 X-Node-Token 鉴权）。
// 返回服务端相对 URL（/img/<file>）。name 仅作扩展名提示（服务端重生成安全文件名）。
func (c *Client) UploadImageBytes(data []byte, name, taskID, sessionID string) (string, error) {
	return c.uploadBytes("/api/images", 60*time.Second, data, name, taskID, sessionID)
}

// UploadMedia 上传生成媒体（视频/音频）到服务端（POST /api/media，multipart；节点令牌鉴权）。
// 返回服务端相对 URL（/img/<file>）；失败返回错误（不阻塞任务主链路，仅日志）。
func (c *Client) UploadMedia(path, taskID, sessionID string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取媒体失败: %w", err)
	}
	return c.UploadMediaBytes(data, filepath.Base(path), taskID, sessionID)
}

// UploadMediaBytes 上传媒体字节流（multipart 表单；节点令牌 X-Node-Token 鉴权）。
// 返回服务端相对 URL（/img/<file>）。name 仅作扩展名提示（服务端重生成安全文件名）。
// 音视频体积大（视频可达百 MB），上传超时放宽到 10 分钟。
func (c *Client) UploadMediaBytes(data []byte, name, taskID, sessionID string) (string, error) {
	c.mu.Lock()
	base := c.httpBase
	c.mu.Unlock()
	return c.uploadBytes(c.mediaEndpoint(base), 10*time.Minute, data, name, taskID, sessionID)
}

// mediaEndpoint 媒体上传端点（独立于图片 /api/images；旧服务端无此路由时
// 调用方收到 404 → 调用方降级推文件名卡，不阻塞任务主链路）。
func (c *Client) mediaEndpoint(base string) string {
	return base + "/api/media"
}

// uploadBytes 通用 multipart 上传（图片/媒体共用；endpoint 决定服务端路由）。
func (c *Client) uploadBytes(endpoint string, timeout time.Duration, data []byte, name, taskID, sessionID string) (string, error) {
	c.mu.Lock()
	base, tok := c.httpBase, c.token
	c.mu.Unlock()
	if base == "" {
		return "", fmt.Errorf("未配置服务端 HTTP 地址")
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	mw.WriteField("taskId", taskID)
	mw.WriteField("sessionID", sessionID)
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Node-Token", tok)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("上传失败 HTTP %d: %s", resp.StatusCode, clipStr(string(b), 200))
	}
	var out struct {
		OK       bool   `json:"ok"`
		URL      string `json:"url"`
		FileName string `json:"fileName"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil || !out.OK || out.URL == "" {
		return "", fmt.Errorf("上传响应异常: %s", clipStr(string(b), 200))
	}
	return out.URL, nil
}

// ---------- 生命周期 ----------

// Start 启动连接循环（goroutine 中运行；自动重连直到 Stop）。
func (c *Client) Start() {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	c.running = true
	c.mu.Unlock()
	// 初始化上行 outbox（P-Reliable）：回放 journal 恢复未确认消息 + 启动发送线程。
	c.initOutbox()
	go c.loop()
}

// Stop 停止连接（自死锁修复：原实现持 c.mu 期间调 setOnline 再次加锁，
// 调用方永久卡死 —— 桌面端关远程开关/改配置重启连接器即触发，e2e 验证发现）。
func (c *Client) Stop() {
	c.mu.Lock()
	running := c.running
	if running {
		c.running = false
		close(c.cancel)
		if c.ws != nil {
			_ = c.ws.Close()
		}
	}
	c.mu.Unlock()
	if running {
		c.setOnline(false, "")
	}
	c.closeOutbox() // 停发送线程（journal 保留，下次 Start 回放续发）
}

// StopGraceful 主动下线并停止连接。
// 与 Stop 的差异：先尝试同步发送 agent.offline 通知服务端标记节点 disabled=true
// （ListNodes 过滤 + EnqueueChat/RouteToAgent 拒绝），再调 Stop 关闭 WS 连接。
// 用于用户主动操作场景（关远程节点开关 / 退出账号 / 实例槽位切换 / 重启连接器）
// —— 服务端立即把节点从移动端/管理端列表移除，并拒绝后续任务派发与续聊。
// offline 帧发送 best-effort：发不出去（连接已断/服务端不可达）不阻塞，
// 退化到被动下线（UnregisterAgent，灰点状态待重连）。
func (c *Client) StopGraceful() {
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws != nil {
		// 短超时同步写：避免 Stop 路径阻塞过久；写失败由后续 ws.Close 触发
		// 服务端 UnregisterAgent 兜底（无 disabled 标记的被动下线）。
		_ = ws.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = ws.WriteJSON(NewEnvelope("", TypeAgentOffline, struct{}{}))
	}
	c.Stop()
}

// Online 连接状态。
func (c *Client) Online() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.online
}

func (c *Client) setOnline(v bool, err string) {
	c.mu.Lock()
	changed := c.online != v
	c.online = v
	cb := c.cb.OnStatus
	c.mu.Unlock()
	if changed && cb != nil {
		cb(v, err)
	}
}

// loop 连接主循环（连接 → 注册 → 读写；断开后延迟重连）。
func (c *Client) loop() {
	for {
		c.mu.Lock()
		running := c.running
		cancel := c.cancel
		c.mu.Unlock()
		if !running {
			return
		}

		err := c.connectOnce(cancel)
		if err == errClosed {
			return // 主动停止
		}
		c.setOnline(false, errString(err))
		// 指数退避重连（3s → 30s 封顶）。
		c.mu.Lock()
		c.reconnectDelay *= 2
		if c.reconnectDelay > 30*time.Second {
			c.reconnectDelay = 3 * time.Second
		}
		d := c.reconnectDelay
		c.mu.Unlock()
		select {
		case <-cancel:
			return
		case <-time.After(d):
		}
	}
}

// errStopped 连接循环主动停止标志。
var errStopped = fmt.Errorf("client stopped")

// errClosed 连接循环退出信号（同 errStopped；别名保留语义）。
var errClosed = errStopped

// heartbeatLoop 心跳上行 goroutine（与读循环解耦）：每 heartbeatInterval
// 周期发送 agent.heartbeat（含运行中任务数）。
//
// 解耦原因（移动端节点状态抖动根因）：原实现心跳 ticker 与 ReadJSON 在
// 同一循环 —— ReadJSON 阻塞等待服务端下行帧期间心跳无法触发。多会话任务
// 执行中（LLM 思考/长工具调用/服务端无 ack 下行）读阻塞可远超心跳周期，
// 服务端 75s 读空闲超时判定断线 → UnregisterAgent 广播离线；PC 端读循环
// 60s 超时又主动断开 → 3s 后重建 → 移动端节点状态「掉线↔连接」反复抖动。
//
// 写失败（连接已断/半开）→ close ws 让读循环感知（ReadJSON 返回错误走
// 正常重连路径）；stop 关闭（连接会话结束）自动退出。
func (c *Client) heartbeatLoop(ws *websocket.Conn, cancel, stop chan struct{}) {
	interval := c.heartbeatInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-cancel:
			return
		case <-ticker.C:
			if err := c.writeWS(ws, NewEnvelope("", TypeAgentHeartbeat, Heartbeat{RunningTasks: c.RunningCount()})); err != nil {
				// 写失败 = 连接已死：关闭底层连接，读循环 ReadJSON 立即返回
				// 错误 → connectOnce 正常退出并走重连（而非 goroutine 内静默死掉）。
				_ = ws.Close()
				return
			}
		}
	}
}

// connectOnce 单次连接会话（注册 + 心跳 + 读循环）。
func (c *Client) connectOnce(cancel chan struct{}) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.Dial(c.url, nil)
	if err != nil {
		return fmt.Errorf("连接失败: %w", err)
	}
	c.mu.Lock()
	c.ws = ws
	c.reconnectDelay = 3 * time.Second
	c.mu.Unlock()

	// 注册（同步上报当前执行模式与可选清单，远端展示/切换；写锁序列化）。
	// User/Pass 上报当前登录账号凭据（P-Account 指挥权隔离，去令牌化后注册
	// 鉴权即账号密码）：服务端经统一账号表校验后绑定节点归属，相同账号的
	// 客户端才能指挥本节点；未登录（空）时匿名注册、节点不可被指挥。
	reg := AgentRegister{
		NodeID:   c.nodeID,
		Name:     c.name,
		Platform: c.platform,
		Arch:     c.arch,
		Version:  c.version(),
		Hostname: c.hostname,
		User:     c.account(),
		Pass:     c.accountPass(),
		Mode:     c.currentMode(),
		Modes:    c.modes(),
	}
	if err := c.writeWS(ws, NewEnvelope("", TypeAgentRegister, reg)); err != nil {
		ws.Close()
		return fmt.Errorf("注册失败: %w", err)
	}

	// 心跳 goroutine（与读循环解耦，见 heartbeatLoop）：15s 周期稳定上行。
	// 原实现心跳 ticker 与 ReadJSON 同循环 —— ReadJSON 阻塞期间心跳无法
	// 触发，多会话任务执行中 LLM 思考/长工具调用阶段服务端无下行帧（无
	// ack/指令），读超时 60s 主动断开 → 服务端 UnregisterAgent 广播离线 →
	// 3s 重连又广播上线 → 移动端节点状态抖动（本问题根因）。
	hbStop := make(chan struct{})
	defer close(hbStop)
	go c.heartbeatLoop(ws, cancel, hbStop)
	online := false
	for {
		select {
		case <-cancel:
			ws.Close()
			return errStopped
		default:
		}
		// 读空闲超时：在线时超时仅续命（重新等待下行帧），不再自杀断开 ——
		// 服务端静默期（无不需应答的下行帧）不是断线信号；心跳 goroutine
		// 持续上行喂服务端 LastSeen，服务端 75s 读空闲永不触发。真断线由
		// 非超时读错误（FIN/RST）或心跳写失败（close ws）感知。
		// 注意：gorilla 的 ReadJSON 超时后连接进入 failed 状态，再次读取会
		// panic（repeated read on failed websocket connection）—— 因此读循环
		// 不设读超时（下行帧不是判活依据），断线完全由心跳写失败闭环感知：
		//   心跳写失败 → ws.Close() → 本循环 ReadJSON 返回错误 → 走重连。
		// 读循环只等下行帧（指令/ack），无限等待直至错误（FIN/RST）或关闭。
		ws.SetReadDeadline(time.Time{}) // 无限：下行帧无周期性，不可作超时判据
		var e Envelope
		if err := ws.ReadJSON(&e); err != nil {
			ws.Close()
			if !online {
				return fmt.Errorf("注册未完成: %w", err)
			}
			return fmt.Errorf("连接断开: %w", err)
		}
		switch e.Type {
		case TypeServerAck:
			// 上行消息处理完成确认：从 outbox 删除（journal 记账终止重发）。
			var ap AckPayload
			if err := e.Decode(&ap); err == nil {
				c.ackMsg(ap.MsgID)
			}
		case TypeServerSendFailed:
			// 服务端明确拒绝（未知类型/处理 panic）：出队终止重发（重发无意义）。
			var sf SendFailedPayload
			if err := e.Decode(&sf); err == nil {
				c.nackMsg(sf.MsgID, sf.Error)
			}
		case TypeServerRegisterOK:
			online = true
			c.setOnline(true, "")
			// 服务端身份对齐（节点令牌 WARN 修复）：服务端裁定 ID 与自报不一致时，
			// 说明自报 ID 不在令牌家族（如自生成 node-xxxx vs 令牌绑定的
			// node-default）。这里对齐的是服务端裁定后的生效 ID（rok.NodeID），
			// 而非基础 ID（rok.BaseNodeID）：多开槽位派生 ID（基础ID-N）服务端
			// 原样采纳并回传为 rok.NodeID，与自报一致时不做修改（防多开互顶：
			// 此前无条件对齐 BaseNodeID，槽位 2/3 重连后自报 ID 被改回基础 ID，
			// 与服务端节点交错、互相顶替）。仅当服务端回落（自报 ID 被拒换基础 ID）
			// 时 cur != rok.NodeID，此时对齐到裁定 ID 并异步通知宿主持久化。
			var rok RegisterOKPayload
			_ = e.Decode(&rok)
			c.mu.Lock()
			cur := c.nodeID
			c.mu.Unlock()
			if rok.NodeID != "" && cur != rok.NodeID {
				aligned := rok.NodeID
				slog.Info("节点 ID 对齐服务端令牌家族（修复不同族派生导致的回落 WARN）",
					"old", cur, "aligned", aligned, "base", rok.BaseNodeID)
				c.mu.Lock()
				c.nodeID = aligned
				c.mu.Unlock()
				if c.cb.OnNodeIDAligned != nil {
					go c.cb.OnNodeIDAligned(aligned)
				}
			}
			// 首跳心跳回传真实并发数：服务重启/节点重连后，服务端运行计数以此收敛
			_ = c.writeWS(ws, NewEnvelope("", TypeAgentHeartbeat, Heartbeat{RunningTasks: c.RunningCount()}))
			// 上线即异步补传离线期间缺失的会话历史（OnOnline 回调；不阻塞读循环）。
			if c.cb.OnOnline != nil {
				go c.cb.OnOnline()
			}
			// 唤醒发送线程：补发 outbox 中未确认的上行消息（服务端按 MsgID 去重）。
			c.wakeSender()
		case TypeServerError:
			var ep ErrorPayload
			_ = e.Decode(&ep)
			ws.Close()
			return fmt.Errorf("服务端拒绝: %s", ep.Message)
		case TypeServerTaskDispatch:
			var d TaskDispatch
			if err := e.Decode(&d); err == nil {
				c.handleDispatch(d)
			}
		case TypeServerSetMode:
			var sm SetMode
			if err := e.Decode(&sm); err == nil {
				c.handleSetMode(sm)
			}
		case TypeServerTaskCancel:
			var tc TaskCancel
			if err := e.Decode(&tc); err == nil {
				c.handleCancel(tc)
			}
		case TypeServerChat:
			var cs ChatSend
			if err := e.Decode(&cs); err == nil {
				c.handleChat(cs)
			}
		case TypeServerAskReply:
			var ar AskReply
			if err := e.Decode(&ar); err == nil {
				c.handleAskReply(ar)
			}
		case TypeServerGetMessages:
			var gm GetMessages
			if err := e.Decode(&gm); err == nil {
				c.handleGetMessages(gm)
			}
		case TypeServerGetConfig:
			// 拉取节点配置：回传 agent.config（服务端落缓存 + node.config 广播）。
			c.handleGetConfig()
		case TypeServerSetConfig:
			var sc SetConfig
			if err := e.Decode(&sc); err == nil {
				c.handleSetConfig(sc)
			}
		case TypeServerDeleteSession:
			// 会话删除联动：删除节点本地会话存档（回调异步执行）。
			var ds DeleteSession
			if err := e.Decode(&ds); err == nil && c.cb.DeleteSession != nil {
				c.cb.DeleteSession(ds.SessionID)
			}
		case TypeServerAnalyze:
			// 会话 AI 分析：异步执行（LLM 耗时不可预估，不阻塞读循环），
			// 完成后 SendAnalyzeDone 回传。
			var as AnalyzeSession
			if err := e.Decode(&as); err == nil && c.cb.Analyze != nil {
				go c.cb.Analyze(as.SessionID)
			}
		case TypeServerCleanup:
			// 批量清理会话：异步执行（逐会话 LLM 分析，耗时不可预估），
			// 完成后 SendCleanupDone 回传（服务端广播 cleanup.done）。
			// Mode 化（2026-10 移动端「清空/总结并清理」）：archive（空）= 原逐会话分析归档；
			// clear = 直接清空本地会话存档；summarize = 先汇总未完成任务为续接任务。
			var ca CleanupAll
			if err := e.Decode(&ca); err == nil && c.cb.CleanupSessions != nil {
				mode := ca.Mode
				go c.cb.CleanupSessions(mode)
			}
		case TypeServerVerifyTasks:
			// 核实任务状态：异步执行（需读会话档案/运行态），完成后 SendVerifyReport
			// 回传（服务端 ApplyVerifyReport 校正 + 广播 verify.done/tasks）。
			var vt VerifyTasks
			if err := e.Decode(&vt); err == nil && c.cb.VerifyTasks != nil {
				go c.cb.VerifyTasks(vt.TaskIDs, vt.RequestedBy, vt.VerifyAll)
			}
		case TypeServerClearSessions:
			// 清空节点本地会话存档（clear.tasks Mode=clear 联动）：异步执行
			//（逐会话 Stop+Delete，秒级），不回执 —— 服务端受理时已清空记录
			// 并广播 clear.done，节点回执会重复广播。
			if c.cb.ClearSessions != nil {
				go c.cb.ClearSessions()
			}
		case TypeServerEmployeeList:
			// 拉取数字员工列表：回传 agent.employee_list（服务端广播应答）。
			c.handleEmployeeList()
		case TypeServerSkillsList:
			// 拉取技能列表：回传 agent.skills_list（服务端缓存 + skills 广播）。
			c.handleSkillsList()
		case TypeServerSkillsGet:
			// 拉取技能全文：e.ID 关联请求（服务端 skills.item 回传时保留）。
			var sg SkillsGet
			if err := e.Decode(&sg); err == nil {
				c.handleSkillsGet(e.ID, sg)
			}
		case TypeServerSkillsSave:
			// 保存/新建技能：成功后补发 agent.skills_list（服务端广播刷新全端）。
			var ss SkillsSave
			if err := e.Decode(&ss); err == nil {
				c.handleSkillsSave(ss)
			}
		case TypeServerSkillsDelete:
			// 删除技能：成功后补发 agent.skills_list（同上）。
			var sd SkillsGet
			if err := e.Decode(&sd); err == nil {
				c.handleSkillsDelete(sd.Name)
			}
		case TypeServerRoadmapList:
			// 拉取规划板：回传 agent.roadmap_data（服务端缓存 + roadmap 广播）。
			c.handleRoadmapList()
		case TypeServerRoadmapPlan:
			// 触发 AI 规划：异步执行（LLM 耗时不可预估，不阻塞读循环），
			// 完成后节点自动推 agent.roadmap_data；失败回报 server.error。
			var pr RoadmapPlanReq
			if err := e.Decode(&pr); err == nil {
				c.handleRoadmapPlan(pr.Guidance)
			}
		case TypeServerRoadmapDev:
			// 执行自主开发：异步执行（开发会话启动即返回；状态变更推
			// agent.roadmap_data）。
			var dr RoadmapDevReq
			if err := e.Decode(&dr); err == nil {
				c.handleRoadmapDev(dr)
			}
		case TypeServerRoadmapDelete:
			// 删除规划条目：同步执行（秒级），成功后补发规划板回传。
			var xr RoadmapDeleteReq
			if err := e.Decode(&xr); err == nil {
				c.handleRoadmapDelete(xr.ID)
			}
		case TypeServerRoadmapDeleteBatch:
			// 批量删除规划条目：同步执行；成功后补发规划板回传。
			var xb RoadmapDeleteBatchReq
			if err := e.Decode(&xb); err == nil {
				c.handleRoadmapDeleteBatch(xb.IDs)
			}
		case TypeServerRoadmapRetry:
			// 一键重跑：同步执行（秒级），成功后补发规划板回传（状态已回队）。
			var rr RoadmapDeleteReq
			if err := e.Decode(&rr); err == nil {
				c.handleRoadmapRetry(rr.ID)
			}
		case TypeServerRoadmapApprove:
			// 批量审核通过：同步执行（只置 approved 入排期，不开发），成功后回传规划板。
			var ap RoadmapDeleteBatchReq
			if err := e.Decode(&ap); err == nil {
				c.handleRoadmapApprove(ap.IDs)
			}
		case TypeServerRoadmapAuto:
			// 完全自主开发：异步执行（自动规划 → AI 审核 → 并行开发），结果由广播回传。
			var au RoadmapAutoReq
			if err := e.Decode(&au); err == nil {
				c.handleRoadmapAuto(au)
			}
		case TypeServerGroupCreate:
			// A2A 群聊创建：回调节点侧（异步执行，完成后回传 agent.group_state）。
			var gcr GroupServerReq
			if err := e.Decode(&gcr); err == nil && c.cb.GroupCreate != nil {
				c.cb.GroupCreate(gcr)
			}
		case TypeServerGroupMsg:
			// 群消息：回调节点侧（消息历史节点持久化 + 广播成员员工）。
			var gm GroupServerReq
			if err := e.Decode(&gm); err == nil && c.cb.GroupMsg != nil {
				c.cb.GroupMsg(gm)
			}
		case TypeServerGroupTask:
			// 群写任务：回调节点侧（群助手协调派发给成员员工执行）。
			var gt GroupServerReq
			if err := e.Decode(&gt); err == nil && c.cb.GroupTask != nil {
				c.cb.GroupTask(gt)
			}
		case TypeServerGroupJoin:
			var gj GroupServerReq
			if err := e.Decode(&gj); err == nil && c.cb.GroupJoin != nil {
				c.cb.GroupJoin(gj)
			}
		case TypeServerGroupLeave:
			var gl GroupServerReq
			if err := e.Decode(&gl); err == nil && c.cb.GroupLeave != nil {
				c.cb.GroupLeave(gl)
			}
		case TypeServerGroupSync:
			// 群状态拉取：回调节点侧（回传 agent.group_state 附消息历史）。
			var gs GroupServerReq
			if err := e.Decode(&gs); err == nil && c.cb.GroupSync != nil {
				c.cb.GroupSync(gs)
			}
		case TypeServerIMInbound:
			// IM 中继回调下发（公网服务端代持 IM 平台回调；im_relay.go）：
			// 回调节点侧 IMChannel 复用既有内核执行（验签/去重/会话路由）。
			var im IMRelayInbound
			if err := e.Decode(&im); err == nil && c.cb.IMInbound != nil {
				c.cb.IMInbound(im)
			}
		}
	}
}

// version 版本号（延迟注入避免循环依赖；桌面端 setVersionInfo）。
var clientVersion = ""

// SetVersionInfo 注入版本号（桌面端启动时调用）。
func SetVersionInfo(v string) { clientVersion = v }

func (c *Client) version() string {
	if clientVersion != "" {
		return clientVersion
	}
	return "dev"
}

// ---------- 指令处理 ----------

// handleDispatch 派发新任务：转发到桌面端执行链（mode 非空先切换执行模式）。
// taskID 经回调透传 → 桌面端 remoteTaskOf 登记，事件/终态回传才能命中服务端任务。
func (c *Client) handleDispatch(d TaskDispatch) {
	if c.cb.RunTask == nil {
		return
	}
	if d.Mode != "" && c.cb.SetMode != nil {
		_ = c.cb.SetMode(d.Mode) // PC 端唯一执行面：远端指定模式，本地立即切换
	}
	c.taskMu.Lock()
	c.runningIDs[d.SessionID] = true
	c.taskMu.Unlock()
	ok := c.cb.RunTask(d.SessionID, d.Text, d.Images, d.Mode, d.TaskID)
	if !ok {
		c.taskMu.Lock()
		delete(c.runningIDs, d.SessionID)
		c.taskMu.Unlock()
	}
}

// handleSetMode 远端切换执行模式。
func (c *Client) handleSetMode(sm SetMode) {
	if c.cb.SetMode == nil {
		return
	}
	if err := c.cb.SetMode(sm.Mode); err != nil {
		c.Send(TypeServerError, ErrorPayload{Message: "切换模式失败: " + err.Error()})
		return
	}
	// 回报最新模式（刷新服务端 nodes 列表；RegisterAgent 更新 Mode 字段）。
	c.SendModeUpdate()
}

// SendIMRelayState 上报 IM 中继渠道注册表（agent.im_state；节点渠道启用/变更时
// 推送，服务端落路由投影 + im.state 广播；im_relay.go）。
func (c *Client) SendIMRelayState(st IMRelayState) {
	c.Send(TypeAgentIMState, st)
}

// SendGroups 上报节点群列表全量（agent.group_state；服务端落投影 + groups 广播）。
func (c *Client) SendGroups(groups []Group) {
	c.Send(TypeAgentGroupState, GroupStatePayload{Groups: groups})
}

// SendGroupState 上报群状态 + 消息历史（agent.group_state；GroupID 为空 = 全量）。
func (c *Client) SendGroupState(gsp GroupStatePayload) {
	c.Send(TypeAgentGroupState, gsp)
}

// SendGroupMessage 上报成员员工产出消息（agent.group_msg → 服务端广播 group.message）。
func (c *Client) SendGroupMessage(msg GroupMessage) {
	c.Send(TypeAgentGroupMsg, msg)
}

// SendGroupTaskDone 上报群写任务完成（agent.group_task_done）。
func (c *Client) SendGroupTaskDone(task GroupTask) {
	c.Send(TypeAgentGroupDone, task)
}

// SendModeUpdate 上报当前模式与可选清单（切换后调用；轻量方案 = 重发 register，
// 服务端 RegisterAgent 幂等刷新 info.Mode/Modes 并广播 nodes 列表）。
func (c *Client) SendModeUpdate() {
	c.Send(TypeAgentRegister, AgentRegister{
		NodeID:   c.nodeID,
		Name:     c.nameSnapshot(),
		Platform: c.platform,
		Arch:     c.arch,
		Version:  c.version(),
		Hostname: c.hostnameSnapshot(),
		User:     c.account(),
		Pass:     c.accountPass(),
		Mode:     c.currentMode(),
		Modes:    c.modes(),
	})
}

// account 当前登录账号快照（注册帧携带；回调空/未设置 = 未登录）。
func (c *Client) account() string {
	if c.cb.Account == nil {
		return ""
	}
	return c.cb.Account()
}

// accountPass 当前登录密码快照（注册帧携带；回调空/未设置 = 匿名注册）。
func (c *Client) accountPass() string {
	if c.cb.Pass == nil {
		return ""
	}
	return c.cb.Pass()
}

// modes 可选执行模式（回调空时返回 nil）。
func (c *Client) modes() []ModeInfo {
	if c.cb.Modes == nil {
		return nil
	}
	return c.cb.Modes()
}

// currentMode 当前执行模式。
func (c *Client) currentMode() string {
	if c.cb.CurrentMode == nil {
		return ""
	}
	return c.cb.CurrentMode()
}

// nameSnapshot / hostnameSnapshot 连接字段快照（SendModeUpdate 用）。
func (c *Client) nameSnapshot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.name
}

func (c *Client) hostnameSnapshot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hostname
}

// handleCancel 取消任务：sessionID/taskID 原样透传（服务端新协议带 sessionId；
// 旧服务端为空时由桌面端 Cancel 回调反向兑底，见 remote_panel.go）。
func (c *Client) handleCancel(tc TaskCancel) {
	if c.cb.Cancel == nil {
		return
	}
	c.cb.Cancel(tc.SessionID, tc.TaskID)
}

// handleChat 续聊：运行中入队（RunTask 内部处理；mode 透传空 = 当前模式）。
// taskID 原样透传：服务端 server.chat 路由携带会话关联任务 ID（SessionTaskID
// 反查），桌面端 remoteRunTask 沿用既有 remoteTaskOf 映射回传事件/终态。
// 修复点：此前硬编码空 taskID，续聊终态回传时服务端 TaskDone 查无任务、
// 移动端收不到第二轮结果（E2E TestE2E_MobileChatContinuesOnPC 复现）。
// 旧服务端 chat 不携带 taskID（恒空），行为不变，向后兼容。
func (c *Client) handleChat(cs ChatSend) {
	if c.cb.RunTask == nil {
		return
	}
	c.cb.RunTask(cs.SessionID, cs.Text, cs.Images, "", cs.TaskID)
}

// handleAskReply 审批应答。
func (c *Client) handleAskReply(ar AskReply) {
	if c.cb.ApproveAsk == nil {
		return
	}
	_ = c.cb.ApproveAsk(ar.SessionID, ar.Response, ar.Text)
}

// handleGetConfig 回传节点配置投影（GetConfig 回调；APIKey 由桌面端回调侧掩码）。
func (c *Client) handleGetConfig() {
	if c.cb.GetConfig == nil {
		return
	}
	c.Send(TypeAgentConfig, c.cb.GetConfig())
}

// handleSetConfig 远端修改节点配置：合并保存后回传 agent.config 确认
// （服务端以此广播 node.config + op.result，移动端刷新；失败回报 server.error）。
func (c *Client) handleSetConfig(sc SetConfig) {
	if c.cb.SetConfig == nil {
		return
	}
	if err := c.cb.SetConfig(sc); err != nil {
		c.Send(TypeServerError, ErrorPayload{Message: "保存配置失败: " + err.Error()})
		return
	}
	if c.cb.GetConfig != nil {
		c.Send(TypeAgentConfig, c.cb.GetConfig())
	}
}

// ---------- 数字员工 ----------

// handleEmployeeList 回传数字员工元数据全量（EmployeeList 回调；空回调回空列表）。
func (c *Client) handleEmployeeList() {
	list := []EmployeeMeta{}
	if c.cb.EmployeeList != nil {
		if l := c.cb.EmployeeList(); l != nil {
			list = l
		}
	}
	c.Send(TypeAgentEmployeeList, list)
}

// ---------- 技能库 ----------

// handleSkillsList 回传技能元数据全量（SkillList 回调；空回调回空列表）。
func (c *Client) handleSkillsList() {
	list := []SkillMeta{}
	if c.cb.SkillList != nil {
		if l := c.cb.SkillList(); l != nil {
			list = l
		}
	}
	c.Send(TypeAgentSkillsList, list)
}

// handleSkillsGet 回传技能全文（SkillGet 回调；失败 OK:false + Error）。
// id 为请求信封 ID（服务端 skills.item 回传时保留关联）。
func (c *Client) handleSkillsGet(id string, sg SkillsGet) {
	if c.cb.SkillGet == nil {
		return
	}
	data := SkillsData{ID: id, Name: sg.Name}
	if content, err := c.cb.SkillGet(sg.Name); err != nil {
		data.OK = false
		data.Error = clipStr(err.Error(), 300)
	} else {
		data.OK = true
		data.Content = content
	}
	c.Send(TypeAgentSkillsData, data)
}

// handleSkillsSave 保存/新建技能：成功后补发 agent.skills_list 刷新（服务端
// 广播 skills 列表确认；失败回报 server.error，移动端 op.result 已受理）。
func (c *Client) handleSkillsSave(ss SkillsSave) {
	if c.cb.SkillSave == nil {
		c.Send(TypeServerError, ErrorPayload{Message: "节点不支持技能保存"})
		return
	}
	if err := c.cb.SkillSave(ss.Name, ss.Content); err != nil {
		c.Send(TypeServerError, ErrorPayload{Message: "保存技能失败: " + err.Error()})
		return
	}
	c.handleSkillsList()
}

// handleSkillsDelete 删除技能（builtin 禁删由回调拒绝）：成功后补发列表刷新。
func (c *Client) handleSkillsDelete(name string) {
	if c.cb.SkillDelete == nil {
		c.Send(TypeServerError, ErrorPayload{Message: "节点不支持技能删除"})
		return
	}
	if err := c.cb.SkillDelete(name); err != nil {
		c.Send(TypeServerError, ErrorPayload{Message: "删除技能失败: " + err.Error()})
		return
	}
	c.handleSkillsList()
}

// SendSkillsList 主动上报技能列表（节点上线时调用；对齐 OnOnline 补传语义）。
func (c *Client) SendSkillsList() {
	c.handleSkillsList()
}

// ---------- 自主开发规划板（roadmap） ----------

// handleRoadmapList 回传规划板全量（RoadmapData 回调；空回调回空数据）。
func (c *Client) handleRoadmapList() {
	data := RoadmapDataPayload{Items: []RoadmapItem{}}
	if c.cb.RoadmapData != nil {
		if d := c.cb.RoadmapData(); d.Items != nil {
			data = d
		}
	}
	c.Send(TypeAgentRoadmapData, data)
}

// handleRoadmapPlan 触发 AI 规划（异步：LLM 调用耗时不可预估，不阻塞读循环；
// 结果由节点侧 pushRoadmapAfterRemote 经 agent.roadmap_data 回传；失败 server.error）。
func (c *Client) handleRoadmapPlan(guidance string) {
	if c.cb.RoadmapPlan == nil {
		c.Send(TypeServerError, ErrorPayload{Message: "节点不支持自主开发规划"})
		return
	}
	go func() {
		if err := c.cb.RoadmapPlan(guidance); err != nil {
			c.Send(TypeServerError, ErrorPayload{Message: "AI 规划失败: " + clipStr(err.Error(), 200)})
		}
	}()
}

// handleRoadmapDev 执行自主开发（异步：批准+启动开发会话；IDs 必填由服务端
// 已校验，此处兑底再校验一次 —— 未选择不允许执行）。
func (c *Client) handleRoadmapDev(dr RoadmapDevReq) {
	if c.cb.RoadmapDev == nil {
		c.Send(TypeServerError, ErrorPayload{Message: "节点不支持自主开发执行"})
		return
	}
	if len(dr.IDs) == 0 {
		c.Send(TypeServerError, ErrorPayload{Message: "请先选择要开发的条目（未选择不能执行）"})
		return
	}
	go func() {
		if err := c.cb.RoadmapDev(dr.IDs, dr.Mode, dr.Parallel); err != nil {
			c.Send(TypeServerError, ErrorPayload{Message: "执行失败: " + clipStr(err.Error(), 200)})
			return
		}
		c.handleRoadmapList() // 执行受理后立即回传规划板（状态同步：approved/developing）
	}()
}

// handleRoadmapDelete 删除规划条目（同步执行；成功后回传规划板刷新）。
func (c *Client) handleRoadmapDelete(id string) {
	if c.cb.RoadmapDelete == nil {
		c.Send(TypeServerError, ErrorPayload{Message: "节点不支持规划板删除"})
		return
	}
	if err := c.cb.RoadmapDelete(id); err != nil {
		c.Send(TypeServerError, ErrorPayload{Message: "删除失败: " + clipStr(err.Error(), 200)})
		return
	}
	c.handleRoadmapList()
}

// handleRoadmapDeleteBatch 批量删除规划条目（同步执行；成功后回传规划板刷新）。
// 开发中条目由节点侧跳过（客户端先 Stop 再删）。
func (c *Client) handleRoadmapDeleteBatch(ids []string) {
	if c.cb.RoadmapDeleteBatch == nil {
		c.Send(TypeServerError, ErrorPayload{Message: "节点不支持规划板批量删除"})
		return
	}
	if err := c.cb.RoadmapDeleteBatch(ids); err != nil {
		c.Send(TypeServerError, ErrorPayload{Message: "批量删除失败: " + clipStr(err.Error(), 200)})
		return
	}
	c.handleRoadmapList()
}

// handleRoadmapRetry 一键重跑失败/暂停条目（同步执行；成功后回传规划板刷新）。
// 旧节点未实现时回 server.error（移动端提示重跑失败，不影响其余操作）。
func (c *Client) handleRoadmapRetry(id string) {
	if c.cb.RoadmapRetry == nil {
		c.Send(TypeServerError, ErrorPayload{Message: "节点不支持规划板重跑"})
		return
	}
	if err := c.cb.RoadmapRetry(id); err != nil {
		c.Send(TypeServerError, ErrorPayload{Message: "重跑失败: " + clipStr(err.Error(), 200)})
		return
	}
	c.handleRoadmapList()
}

// handleRoadmapApprove 批量审核通过（同步执行；只置 approved 入排期，成功后回传规划板）。
func (c *Client) handleRoadmapApprove(ids []string) {
	if c.cb.RoadmapApprove == nil {
		c.Send(TypeServerError, ErrorPayload{Message: "节点不支持批量审核"})
		return
	}
	if err := c.cb.RoadmapApprove(ids); err != nil {
		c.Send(TypeServerError, ErrorPayload{Message: "审核失败: " + clipStr(err.Error(), 200)})
		return
	}
	c.handleRoadmapList()
}

// handleRoadmapAuto 完全自主开发（异步：自动规划 → AI 审核 → 并行开发；结果广播回传）。
func (c *Client) handleRoadmapAuto(au RoadmapAutoReq) {
	if c.cb.RoadmapAuto == nil {
		c.Send(TypeServerError, ErrorPayload{Message: "节点不支持完全自主开发"})
		return
	}
	go func() {
		if err := c.cb.RoadmapAuto(au.Guidance, au.Parallel, au.MinDeveloping); err != nil {
			c.Send(TypeServerError, ErrorPayload{Message: "开启失败: " + clipStr(err.Error(), 200)})
			return
		}
		c.handleRoadmapList() // 状态变更后立即回传规划板
	}()
}

// SendRoadmapData 主动上报规划板（节点上线时调用；对齐 SendSkillsList 兑底语义）。
func (c *Client) SendRoadmapData() {
	c.handleRoadmapList()
}

// handleGetMessages 回传会话历史（含 todos 任务清单，远端渲染进度）。
func (c *Client) handleGetMessages(gm GetMessages) {
	if c.cb.SessionMessages == nil {
		return
	}
	msgs := c.cb.SessionMessages(gm.SessionID)
	if msgs == nil {
		msgs = []interface{}{}
	}
	resp := MessagesResponse{SessionID: gm.SessionID, Messages: msgs}
	if c.cb.SessionTodos != nil {
		if todos := c.cb.SessionTodos(gm.SessionID); len(todos) > 0 {
			resp.Todos = todos
		}
	}
	c.Send(TypeAgentMessages, resp)
}

// SendAnalyzeDone 回传会话 AI 分析结果（节点侧 LLM 生成后调用；服务端广播）。
func (c *Client) SendAnalyzeDone(ad AnalyzeDone) {
	c.Send(TypeAgentAnalyzeDone, ad)
}

// SendCleanupDone 回传批量清理会话结果（节点逐会话分析+归档删除后调用；服务端广播）。
func (c *Client) SendCleanupDone(cd CleanupDone) {
	c.Send(TypeAgentCleanupDone, cd)
}

// SendVerifyReport 回传任务状态核实结果（节点按本地会话档案判定真实状态后
// 调用；服务端 ApplyVerifyReport 校正任务并广播 verify.done/tasks）。
// requestedBy 为发起端标记（server.verify_tasks 注入），透传给服务端用于
// verify.done 广播定位发起端。
func (c *Client) SendVerifyReport(rep VerifyReport, requestedBy string) {
	rep.RequestedBy = requestedBy
	c.Send(TypeAgentVerifyReport, rep)
}

// ---------- 发送 ----------

// Send 上行消息（P-Reliable）：一律入 outbox 由发送线程投递（未连接时保留队列，
// 重连后补发；服务端处理完成回 server.ack 才删除）—— 修复旧实现「未连接静默
// 丢弃」导致断线期间的事件/终态永久丢失。心跳/注册不走此路径（连接级控制帧）。
func (c *Client) Send(typ string, payload interface{}) {
	c.enqueue(typ, payload)
}

// writeWS 单连接写入口（writeMu 序列化 + 写超时；对齐 hub 侧 writeJSON 语义）。
func (c *Client) writeWS(ws *websocket.Conn, e Envelope) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ws.WriteJSON(e)
}

// SendTaskEvent 推送任务事件。
func (c *Client) SendTaskEvent(taskID, sessionID, kind, text string, partial bool) {
	c.Send(TypeAgentTaskEvent, TaskEvent{
		TaskID: taskID, SessionID: sessionID, Kind: kind,
		Text: text, Partial: partial, TS: time.Now().UnixMilli(),
	})
}

// SendTaskImageEvent 推送生成图片事件（kind=image）：Text 为 ImageEventPayload JSON，
// Images 为图片 URL 列表（服务端 AttachTaskImages 回填 Task.Images + 广播移动端）。
func (c *Client) SendTaskImageEvent(taskID, sessionID, text string, images []string) {
	c.Send(TypeAgentTaskEvent, TaskEvent{
		TaskID: taskID, SessionID: sessionID, Kind: "image",
		Text: text, Images: images, TS: time.Now().UnixMilli(),
	})
}

// SendSubtask 子会话生命周期上报（new_task/spawn_agent 平行子会话）：
// start（status=running）/ end（done/failed/canceled）两帧，服务端仅投影会话列表。
func (c *Client) SendSubtask(ev SubtaskEvent) {
	if ev.SessionID == "" {
		return
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	c.Send(TypeAgentSubtask, ev)
}

// SendTaskDone 推送任务终态并清运行计数。
func (c *Client) SendTaskDone(taskID, sessionID string, ok bool, result, errMsg string) {
	c.taskMu.Lock()
	delete(c.runningIDs, sessionID)
	c.taskMu.Unlock()
	c.Send(TypeAgentTaskDone, TaskDone{
		TaskID: taskID, OK: ok, Result: result, Error: errMsg,
	})
}

// RunningCount 运行中任务数。
func (c *Client) RunningCount() int {
	c.taskMu.Lock()
	defer c.taskMu.Unlock()
	return len(c.runningIDs)
}

// NotifyNodeInfo 节点信息更新（重连时刷新名字等）。
func (c *Client) NotifyInfo(name, hostname string) {
	c.mu.Lock()
	c.name = name
	c.hostname = hostname
	c.mu.Unlock()
}

// errString 错误转字符串（nil → 空）。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// NewNodeID 生成节点 ID（机器维度稳定标识；首次生成后持久化）。
func NewNodeID() string {
	b := make([]byte, 6)
	_, _ = randRead(b)
	return "node-" + hex.EncodeToString(b)
}
