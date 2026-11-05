package remote

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Hub 服务端中心枢纽：节点连接、客户端连接、任务持久化与路由。
//
// 线程模型：
//   - mu 保护节点/客户端/任务内存态；
//   - agentConn / clientConn 的写操作各自持连接级锁（gorilla 并发写不安全）；
//   - 持久化：nodes.json / tasks.json 落盘（dir 为空 = 不持久化，仅内存）。
//
// 账号体系（P-Account）：users 为统一账号表（PC/移动/管理端同一套），
// role 决定管理权限（admin 可用管理后台全部能力；user 仅移动端+聊天）。
// 同一账号任意端登录 → 同一身份，任务/会话全部互通。
type Hub struct {
	mu      sync.RWMutex
	nodes   map[string]*nodeState // nodeID → 状态
	clients map[*clientConn]bool
	tasks   map[string]*Task       // taskID → 任务（全量，含历史）
	events  map[string][]TaskEvent // taskID → 事件环形历史（客户端断线重连补发）

	// subtasks 子会话投影（sessionID → SubtaskEvent）：节点 new_task/spawn_agent
	// 创建的平行子会话（agent.subtask 上报）。仅会话列表展示用（带 parentId 缩进），
	// 不进入任务派发/取消/核实链路（子会话无服务端任务记录，纯投影缓存不持久化）。
	subtasks map[string]SubtaskEvent

	dir    string // 持久化目录（空 = 仅内存）
	authMu sync.Mutex
	users  map[string]Account // user → 统一账号（PC 节点注册 / 客户端登录 同一验证源；兼容旧 admins/mobiles 双表，见 NewHub）

	// PendingTasks 离线节点任务暂存上限（防止无界增长）。
	maxPendingPerNode    int
	maxEventsPerTask     int
	maxSessionMirrorMsgs int // 会话镜像追加模式单会话上限（task.event 同步追加用）

	clientToken string                       // 移动端/管理端共享访问令牌（可空）
	sessions    map[string][]json.RawMessage // sessionID → 镜像历史
	sessTodos   map[string][]json.RawMessage // sessionID → 镜像任务清单（移动端 Todo 进度，离线/重启后仍可见）
	sessMu      sync.Mutex

	// nodeCfgs 节点配置缓存（get.config 立即应答 + 离线节点展示；掩码存储）。
	// 来源唯一：节点回传 agent.config（SetConfig 成功回报 / get.config 拉取），
	// 服务端不自造配置 —— PC 端 desktop.json 才是权威源。
	nodeCfgs map[string]NodeConfig // nodeID → 配置快照（APIKey 已掩码）

	// nodeSkills 技能列表缓存（进程内存，不持久化）：skills.list 缓存立即应答；
	// 节点 OnOnline 主动上报兑底刷新。来源唯一：节点回传 agent.skills_list。
	nodeSkills map[string][]SkillMeta // nodeID → 技能元数据全量

	// nodeRoadmaps 自主开发规划板缓存（进程内存，不持久化）：roadmap.list 缓存
	// 立即应答；节点上线主动上报兑底刷新。来源唯一：节点回传 agent.roadmap_data
	//（PC 端 ~/.kaelvoris/roadmap/items.json 才是权威源，服务端仅投影）。
	nodeRoadmaps map[string]RoadmapDataPayload // nodeID → 规划板快照

	// nodeGroups A2A 群聊投影缓存（进程内存，不持久化）：group.list / group.sync
	// 缓存立即应答；节点上线主动上报兑底刷新。来源唯一：节点回传 agent.group_state
	//（PC 端 ~/.kaelvoris/groups/ 才是权威源，服务端仅投影）。
	nodeGroups map[string][]Group // nodeID → 群列表投影

	// imRelay IM 中继渠道注册表（公网回调入口路由；im_channels.json 持久化）。
	// 来源唯一：节点回传 agent.im_state（桌面端 channels.json 才是权威源，服务端
	// 仅存转发路由投影）。见 im_relay.go。
	imRelay *imRelayRegistry

	// onUsersChanged 账号表变更回调（account.passwd 修改密码等写操作后，宿主
	// 经 UsersSnapshot 重新持久化 accounts.json 用；可空。回调内禁止调回 Hub 方法，
	// 避免锁序倒置）。
	onUsersChanged func()
	usersChangedMu sync.Mutex

	// 上行消息去重（P-Reliable）：msgID → 已处理（LRU 环形）；PC 端补发重放时
	// 幂等跳过。增量落盘（agent_msg_ids.json，JSONL）保服务重启后补发仍不重复入账。
	seenMu   sync.Mutex
	seenIDs  map[string]struct{}
	seenRing []string
	seenPos  int
	seenNew  []string // 自上次落盘以来的新增 msgID（增量追加落盘）

	// 失联巡检生命周期（StartWatchdog/StopWatchdog）。
	watchdogStop chan struct{}
	watchdogDone chan struct{}

	// 消息存储总量上限（P-Storage，storage_limit.go）：超限按日期从旧到新清理。
	// 默认 512MB（defaultStoreLimitBytes），main.go -store-limit 可调，<=0 关闭。
	storeLimitBytes atomic.Int64
	storePruneMu    sync.Mutex
	storePrunerStop chan struct{}

	// 异步持久化（防死锁核心改造 2026-10-03）：历史版本在 h.mu 写锁内同步
	// os.WriteFile（saveNodes/saveTasks，RegisterAgent/TaskDone/CancelTask 等
	// 全部热点路径）。sessions.json 达 MB 级后磁盘一紧张（曾 96% 满）写入耗时
	// 暴涨，锁内 IO 卡住 → 所有等 h.mu 的 goroutine（曾 95 个）集体挂起，
	// /api/health 超时（10-02 死锁 48 分钟事故根因）。
	// 改造后锁内仅置脏 + 非阻塞唤醒；唯一落盘者 persistLoop 在锁外快照并原子写
	//（tmp + rename），单写者消除并发写撕裂；Stop 时最终 flush 保证退出不丢数据。
	persistMu   sync.Mutex
	dirtyCfgs   bool
	dirtyNodes  bool
	dirtyTasks  bool
	dirtySess   bool
	dirtyTodos  bool
	persistWake chan struct{}
	persistStop chan struct{}
	persistDone chan struct{}
	persistOnce sync.Once

	// FlushPersistence 同步等待通道（persistLoop 每轮 flush 完成后回发；
	// persistSyncMu 保护交换，防止并发 Flush 丢失回发）。
	persistSync   chan struct{}
	persistSyncMu sync.Mutex
}

// nodeState 节点运行态。
type nodeState struct {
	info     Node
	conn     *agentConn // 在线连接（nil = 离线）
	pending  []*Task    // 离线暂存任务（上线后补发）
	disabled bool       // 主动下线标记（agent.offline 关开关/退出账号）：ListNodes 过滤、EnqueueChat 拒绝；RegisterAgent 自动清除（节点重连恢复服务）
}

// clientConn 已认证客户端连接（手机 H5 / 管理后台）。
type clientConn struct {
	role   string // client | admin
	user   string
	ws     *websocket.Conn
	sendMu sync.Mutex
}

// agentConn 已注册节点连接。
type agentConn struct {
	nodeID string
	ws     *websocket.Conn
	sendMu sync.Mutex
}

// writeJSON 线程安全写消息。
func (c *clientConn) writeJSON(e Envelope) error {
	if c == nil || c.ws == nil {
		return errStopped
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteJSON(e)
}

// writeJSON 线程安全写消息（agent 侧）。
func (ac *agentConn) writeJSON(e Envelope) error {
	if ac == nil || ac.ws == nil {
		return errStopped
	}
	ac.sendMu.Lock()
	defer ac.sendMu.Unlock()
	ac.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ac.ws.WriteJSON(e)
}

// NewHub 创建 hub 并加载持久化数据。clientToken 为移动端/管理端共享访问令牌（可空）。
// adminUsers/mobileUsers 为旧双表账号（admins/mobiles）；users 非空时优先使用统一账号表，
// 旧双表自动并入（admins→admin 角色，mobiles→user 角色），实现向后兼容。
// agentTokens 参数已废弃（去令牌化 2026-10：节点注册改统一账号 User/Pass 鉴权），
// 仅保留签名兼容 cmd/kaelvoris-server 装配与既有测试；传入值不再使用。
func NewHub(dir string, agentTokens map[string]string, clientToken string, adminUsers, mobileUsers map[string]string, users []Account) *Hub {
	merged := map[string]Account{}
	for u, p := range adminUsers {
		merged[u] = Account{User: u, Pass: p, Role: "admin", DisplayName: u}
	}
	for u, p := range mobileUsers {
		if _, exists := merged[u]; !exists { // admin 优先
			merged[u] = Account{User: u, Pass: p, Role: "user", DisplayName: u}
		}
	}
	for _, a := range users {
		if a.User == "" {
			continue
		}
		aa := a
		if aa.Role == "" {
			aa.Role = "user"
		}
		if aa.DisplayName == "" {
			aa.DisplayName = aa.User
		}
		merged[a.User] = aa
	}
	h := &Hub{
		nodes:                map[string]*nodeState{},
		clients:              map[*clientConn]bool{},
		tasks:                map[string]*Task{},
		events:               map[string][]TaskEvent{},
		dir:                  dir,
		users:                merged,
		clientToken:          clientToken,
		maxPendingPerNode:    50,
		maxEventsPerTask:     500,
		maxSessionMirrorMsgs: 800,
		sessions:             map[string][]json.RawMessage{},
		sessTodos:            map[string][]json.RawMessage{},
		nodeCfgs:             map[string]NodeConfig{},
		nodeSkills:           map[string][]SkillMeta{},
		nodeRoadmaps:         map[string]RoadmapDataPayload{},
		nodeGroups:           map[string][]Group{},
		subtasks:             map[string]SubtaskEvent{},
		imRelay:              newIMRelayRegistry(dir),
		seenIDs:              map[string]struct{}{},
		seenRing:             make([]string, msgIDCapacity),
	}
	if dir != "" {
		h.loadSeen()
		h.load()
		h.loadFileMeta() // 附件归属元数据（file_meta.json；缺失/损坏静默跳过）
		// 恢复后立即规范化落盘：未完成任务重置为 pending 的状态持久化，
		// 避免再次重启时 tasks.json 与内存态不一致（重复恢复日志/状态漂移）。
		if len(h.tasks) > 0 {
			h.saveTasks()
		}
		h.loadSessions()
		// 启动清理：历史遗留超上限的会话任务（自动删最旧终态会话）+
		// 孤儿镜像（无任务关联的 sessions.json 残留），防数据文件无界膨胀。
		h.pruneFinishedSessions()
		h.pruneOrphanMirrors()
		// 存储总量上限巡检（P-Storage）：超限按日期从旧到新清理（storage_limit.go）。
		h.SetStoreLimit(defaultStoreLimitBytes)
		h.startStorePruner()
		// 启动即全量置脏：persistLoop 首轮把 nodes/tasks/sessions/todos
		// 全部落一次盘（内存态与磁盘对齐，含 prune 后的收敛结果）。
		h.markPersistAll()
		h.startPersistLoop()
	}
	return h
}

// Accounts 统一账号列表（脱敏：不含密码，管理端账号管理页展示）。
func (h *Hub) Accounts() []Account {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	out := make([]Account, 0, len(h.users))
	for _, a := range h.users {
		out = append(out, Account{User: a.User, Role: a.Role, DisplayName: a.DisplayName})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role // admin 在前
		}
		return out[i].User < out[j].User
	})
	return out
}

// OnUsersChanged 注入统一账号表变更回调（宿主把 UsersSnapshot 重新落
// accounts.json 用；NewHub 后、任何 ChangePassword 前设置）。
func (h *Hub) OnUsersChanged(cb func()) {
	h.usersChangedMu.Lock()
	h.onUsersChanged = cb
	h.usersChangedMu.Unlock()
}

// notifyUsersChanged 异步触发账号表变更回调（回调内禁止调回 Hub 方法，
// 避免锁序倒置）。
func (h *Hub) notifyUsersChanged() {
	h.usersChangedMu.Lock()
	cb := h.onUsersChanged
	h.usersChangedMu.Unlock()
	if cb != nil {
		go cb()
	}
}

// UsersSnapshot 统一账号表快照（宿主持久化 accounts.json 用；含密码明文，
// 仅在宿主进程内写 accounts.json 0600，严禁外发）。
func (h *Hub) UsersSnapshot() []Account {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	out := make([]Account, 0, len(h.users))
	for _, a := range h.users {
		out = append(out, a)
	}
	return out
}

// ChangePassword 修改统一账号密码（account.passwd 服务端实现）。
//   - user 为空 = 当前登录账号自己改密：必须验旧密码（AuthClient 同表校验）；
//   - user 非空 = 指定账号改密：仅 admin 角色可操作（重置语义，无需旧密码）；
//   - 新密码 4~64 字符；成功后异步通知宿主持久化 accounts.json（重启后生效）。
//
// 内存表即改即生效（AuthClient 下次登录即用新密码）；已在线连接不受影响。
func (h *Hub) ChangePassword(actorUser, actorRole, targetUser, oldPass, newPass string) (string, error) {
	newPass = strings.TrimSpace(newPass)
	if len(newPass) < 4 || len(newPass) > 64 {
		return "", fmt.Errorf("新密码长度需为 4~64 字符")
	}
	h.authMu.Lock()
	defer h.authMu.Unlock()
	user := targetUser
	if user == "" {
		user = actorUser // 空 = 自己
	}
	a, ok := h.users[user]
	if !ok {
		return "", fmt.Errorf("账号不存在: %s", user)
	}
	if targetUser != "" && targetUser != actorUser {
		if actorRole != "admin" {
			return "", fmt.Errorf("仅管理员可修改他人密码")
		}
	} else {
		// 自己改密：验旧密码（AuthClient 同规则：账号存在且密码非空匹配）。
		if a.Pass != oldPass || oldPass == "" {
			return "", fmt.Errorf("旧密码错误")
		}
	}
	a.Pass = newPass
	h.users[user] = a
	go h.notifyUsersChanged()
	slog.Info("账号密码已修改", "user", user, "by", actorUser)
	return user, nil
}

// ---------- 持久化 ----------

type persistedNodes struct {
	Nodes map[string]Node `json:"nodes"`
}

func (h *Hub) nodesFile() string    { return filepath.Join(h.dir, "nodes.json") }
func (h *Hub) tasksFile() string    { return filepath.Join(h.dir, "tasks.json") }
func (h *Hub) nodeCfgsFile() string { return filepath.Join(h.dir, "node_configs.json") }

// msgIDSeen 置位 msgID 已处理（LRU 环形去重表，P-Reliable）。
// 容量 msgIDCapacity：远超单节点在途/补发窗口（几十条量级），溢出淘汰最旧条目
// 安全（补发窗口远小于容量，淘汰后重放概率可忽略；且受益载荷多数覆盖式幂等）。
const msgIDCapacity = 4096

func (h *Hub) msgIDSeen(id string) {
	if id == "" {
		return
	}
	h.seenMu.Lock()
	if _, ok := h.seenIDs[id]; ok {
		h.seenMu.Unlock()
		return
	}
	h.seenIDs[id] = struct{}{}
	old := h.seenRing[h.seenPos]
	if old != "" {
		delete(h.seenIDs, old)
	}
	h.seenRing[h.seenPos] = id
	h.seenPos = (h.seenPos + 1) % len(h.seenRing)
	h.seenNew = append(h.seenNew, id)
	pending := h.seenNew
	h.seenNew = nil
	h.seenMu.Unlock()
	if h.dir != "" {
		h.appendSeen(pending)
	}
}

// msgIDIsDup msgID 是否已处理过（补发重放判定；调用方据以跳过重复入账）。
func (h *Hub) msgIDIsDup(id string) bool {
	if id == "" {
		return false
	}
	h.seenMu.Lock()
	defer h.seenMu.Unlock()
	_, ok := h.seenIDs[id]
	return ok
}

// msgIDsFile 去重表落盘文件（JSONL 每行一个 JSON 字符串；服务重启后补发仍不重复）。
func (h *Hub) msgIDsFile() string { return filepath.Join(h.dir, "agent_msg_ids.json") }

// loadSeen 加载历史 msgID（JSONL 逐行解析；损坏行跳过不致命，对齐编码规范）。
func (h *Hub) loadSeen() {
	b, err := os.ReadFile(h.msgIDsFile())
	if err != nil {
		return
	}
	h.seenMu.Lock()
	defer h.seenMu.Unlock()
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var id string
		if json.Unmarshal([]byte(line), &id) != nil || id == "" {
			continue
		}
		if _, ok := h.seenIDs[id]; ok {
			continue
		}
		h.seenIDs[id] = struct{}{}
		if len(h.seenRing) > 0 {
			if old := h.seenRing[h.seenPos]; old != "" {
				delete(h.seenIDs, old)
			}
			h.seenRing[h.seenPos] = id
			h.seenPos = (h.seenPos + 1) % len(h.seenRing)
		}
	}
}

// appendSeen 增量追加新 msgID（调用方传入待写批次，解锁后调用）。
func (h *Hub) appendSeen(ids []string) {
	if h.dir == "" || len(ids) == 0 {
		return
	}
	_ = os.MkdirAll(h.dir, 0o755)
	f, err := os.OpenFile(h.msgIDsFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	bw := bufio.NewWriter(f)
	for _, id := range ids {
		b, err := json.Marshal(id)
		if err != nil {
			continue
		}
		bw.Write(append(b, '\n'))
	}
	_ = bw.Flush()
}
func (h *Hub) load() {
	if b, err := os.ReadFile(h.nodesFile()); err == nil {
		var p persistedNodes
		if json.Unmarshal(b, &p) == nil && p.Nodes != nil {
			for id, n := range p.Nodes {
				n.Online = false
				n.CurrentTask = ""
				n.RunningTasks = 0 // 落盘值为心跳快照，重启后以首跳心跳回传校正
				h.nodes[id] = &nodeState{info: n}
			}
		}
	}
	if b, err := os.ReadFile(h.tasksFile()); err == nil {
		var list []*Task
		if json.Unmarshal(b, &list) == nil {
			// 未完成任务恢复：进程重启后 running/pending 的执行态已失真，
			// 重置为 pending 并重建离线暂存队列（ns.pending），节点重新上线
			// 后经 RegisterAgent 补发自动续跑（服务重启不再丢任务）。
			// 节点端语义：补发复用 server.task_dispatch，节点按会话空闲/运行
			// 决定新轮次或入队（remoteRunTask），重复恢复最多追加一轮执行，可接受。
			var unfin []*Task
			for _, t := range list {
				if t == nil {
					continue
				}
				if t.Status == TaskStatusRunning || t.Status == TaskStatusPending {
					t.Status = TaskStatusPending
					t.StartedAt = 0
					t.FinishedAt = 0
					t.Error = ""
					unfin = append(unfin, t)
				}
				h.tasks[t.ID] = t
			}
			// 按创建时间升序入队（先创建先执行，对齐派发 FIFO 语义）。
			sort.Slice(unfin, func(i, j int) bool { return unfin[i].CreatedAt < unfin[j].CreatedAt })
			for _, t := range unfin {
				ns, ok := h.nodes[t.NodeID]
				if !ok { // 节点从未落盘（异常情况）兜底建卡，避免任务永久滞留
					ns = &nodeState{}
					ns.info.ID = t.NodeID
					h.nodes[t.NodeID] = ns
				}
				ns.pending = append(ns.pending, t)
			}
			if len(unfin) > 0 {
				slog.Info("服务重启：恢复未完成任务，待节点上线补发", "count", len(unfin))
			}
		}
	}
	// 节点配置缓存（node_configs.json；旧版无此文件时静默跳过）。
	if b, err := os.ReadFile(h.nodeCfgsFile()); err == nil {
		var m map[string]NodeConfig
		if json.Unmarshal(b, &m) == nil && m != nil {
			h.nodeCfgs = m
		}
	}
}

// saveNodes 置脏节点持久化（调用点均在 h.mu 锁内，此处绝不碰磁盘/数据 map——
// 真正落盘由 persistLoop 锁外完成，见 persist*.go 同文件尾部）。
func (h *Hub) saveNodes() { h.markPersist(&h.dirtyNodes) }

// saveTasks 置脏任务持久化（语义同 saveNodes）。
func (h *Hub) saveTasks() { h.markPersist(&h.dirtyTasks) }

// saveNodeCfgs 置脏节点配置快照持久化（node_configs.json；persistLoop 落盘）。
func (h *Hub) saveNodeCfgs() { h.markPersist(&h.dirtyCfgs) }

// StoreNodeConfig 缓存节点配置快照（agent.config 回传时调用）。
// APIKey 统一掩码落盘（节点上行即掩码，双保险）；随 get.config 应答/广播复用。
func (h *Hub) StoreNodeConfig(nodeID string, cfg NodeConfig) {
	cfg.APIKey = MaskAPIKey(cfg.APIKey)
	h.mu.Lock()
	h.nodeCfgs[nodeID] = cfg
	h.mu.Unlock()
	h.saveNodeCfgs() // 置脏异步落盘（原实现在锁外直接读 h.nodeCfgs 序列化，存在数据竞争）
}

// ---------- 技能库 ----------

// StoreSkills 缓存节点技能列表（agent.skills_list 回传时调用；进程内存缓存，
// 不持久化 —— 节点重连即主动上报，进程重启后由节点上线兜底）。
func (h *Hub) StoreSkills(nodeID string, list []SkillMeta) {
	h.mu.Lock()
	h.nodeSkills[nodeID] = list
	h.mu.Unlock()
}

// SkillsOf 节点技能列表缓存（无缓存返回 nil；离线节点返回最后已知列表）。
func (h *Hub) SkillsOf(nodeID string) []SkillMeta {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.nodeSkills[nodeID]
}

// ---------- 自主开发规划板（roadmap 缓存） ----------

// StoreRoadmap 缓存节点规划板快照（agent.roadmap_data 回传时调用；进程内存，
// 不持久化 —— 节点重连即主动上报，进程重启后由节点上线兑底）。
func (h *Hub) StoreRoadmap(nodeID string, data RoadmapDataPayload) {
	data.NodeID = nodeID // 以连接身份为准（载荷不带节点 ID）
	h.mu.Lock()
	h.nodeRoadmaps[nodeID] = data
	h.mu.Unlock()
}

// RoadmapOf 节点规划板快照（无缓存返回零值 false；离线节点返回最后已知快照）。
func (h *Hub) RoadmapOf(nodeID string) (RoadmapDataPayload, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.nodeRoadmaps[nodeID]
	return c, ok
}

// NodeConfigOf 节点配置快照（无缓存返回零值 false；离线节点返回最后已知配置）。
func (h *Hub) NodeConfigOf(nodeID string) (NodeConfig, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.nodeCfgs[nodeID]
	return c, ok
}

// taskList 任务快照（调用方需持读锁或写锁）。
func (h *Hub) taskList() []*Task {
	out := make([]*Task, 0, len(h.tasks))
	for _, t := range h.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// ---------- 节点管理 ----------

// AuthAgentForRegister 节点注册鉴权（去令牌化 2026-10）：统一账号表校验
// User/Pass（与 /ws/client auth 同一验证源）。校验通过返回账号的 role，失败返回 false。
// User/Pass 均空 = 匿名注册（只读在线展示，RegisterAgent 不绑定 Owner，不可被指挥）。
func (h *Hub) AuthAgentForRegister(user, pass string) (string, bool) {
	if user == "" && pass == "" {
		return "", true // 匿名：节点可上线展示，但无指挥权
	}
	h.authMu.Lock()
	defer h.authMu.Unlock()
	a, ok := h.users[user]
	if !ok || a.Pass != pass || pass == "" {
		return "", false
	}
	return a.Role, true
}

// RegisterAgent 节点上线注册（幂等：重复注册刷新信息；同步上报执行模式与可选清单）。
// 上线/重连统一广播 nodes + tasks：修复断联窗口内客户端状态滞留 —— 节点卡灰点
// 不恢复、断联期间卡死的「运行中」任务收敛后不推送（此前仅补发路径广播，
// 收敛与上线均静默，各端只能等下拉刷新才收敛）。
func (h *Hub) RegisterAgent(conn *agentConn, reg AgentRegister) {
	var (
		reaped  int
		pending []*Task
	)
	// 锁内临界区：defer Unlock 保证任何 panic 都回收 h.mu（防锁泄漏导致
	// AddClient//api/health/后续注册全部永久阻塞——线上 10-09 事故形态）。
	func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		ns, ok := h.nodes[conn.nodeID]
		if !ok {
			ns = &nodeState{}
			ns.info.ID = conn.nodeID
			ns.info.FirstSeen = time.Now().UnixMilli()
			h.nodes[conn.nodeID] = ns
		}
		if reg.Name != "" {
			ns.info.Name = reg.Name
		}
		ns.info.Platform = reg.Platform
		ns.info.Arch = reg.Arch
		ns.info.Version = reg.Version
		ns.info.Hostname = reg.Hostname
		ns.info.Online = true
		ns.info.LastSeen = time.Now().UnixMilli()
		ns.info.Mode = reg.Mode
		ns.info.Modes = reg.Modes
		ns.info.RunningTasks = 0 // 重连清零陈旧计数，首跳心跳回传真实并发数校正
		ns.conn = conn
		ns.disabled = false // 节点重连自动恢复服务（清除 agent.offline 标记）
		// 账号绑定（P-Account 指挥权隔离）：注册帧携带登录账号时刷新归属，
		// 空账号不清除既有 Owner（旧客户端兼容 + 退出登录前保留归属展示）。
		if reg.User != "" && ns.info.Owner != reg.User {
			ns.info.Owner = reg.User
			slog.Info("节点归属账号更新（注册上报）", "node", conn.nodeID, "owner", reg.User)
		}
		// IM 中继：节点上线恢复其渠道在线路由（im_channels.json 落盘态 → nodeByFP）。
		if h.imRelay != nil {
			h.imRelay.AttachNode(conn.nodeID)
		}
		// 僵尸 running 收敛（修复：节点断连期间，运行中任务的终态回传随连接丢失，
		// 重连后卡片永远卡死「运行中」，右上角徽标/计时无法清零——观感即“运行中
		// 状态不管用”）。断连 = 执行进程已不可达，本节点全部 running 任务不可能
		// 还在真实执行；转 failed（错误文案说明原因）并广播 tasks 让所有端立即归零。
		// 注：服务重启恢复链路在 load() 已重置为 pending 并入暂存队列，与此处无交叉。
		reaped = h.reapStaleRunningLocked(conn.nodeID)
		// 补发离线暂存任务（含服务重启恢复的未完成任务，见 load）。
		pending = ns.pending
		ns.pending = nil
		h.saveNodes()
		slog.Info("远程节点注册", "node", conn.nodeID, "name", reg.Name, "version", reg.Version, "reaped", reaped)
	}()
	// 解锁后广播 nodes：上线/重连立即让全部端收敛在线状态（绿点/灰点不再滞留）。
	h.Broadcast(TypeNodes, h.ListNodes())
	// 僵尸 running 收敛后广播 tasks：无补发任务时这是唯一推送时机，否则移动端
	// 任务卡「运行中」徽标/计时永久滞留（收敛结果仅落盘），需手动刷新才收敛。
	if reaped > 0 {
		h.Broadcast(TypeTasks, h.ListTasks(100))
	}
	if len(pending) > 0 {
		// 逐个派发（dispatchTask 自带锁管理与锁外 WS 写：补发遇半死连接
		// 不再持 h.mu 阻塞，写失败自动回退暂存；已终态任务自动跳过）。
		go func() {
			for _, t := range pending {
				h.dispatchTask(t)
			}
			h.Broadcast(TypeTasks, h.ListTasks(100)) // 补发后刷新客户端任务列表
		}()
	}
}

// UnregisterAgent 节点下线：状态落盘后广播 nodes（修复：此前下线静默，
// 移动端/管理端节点卡在线状态滞留，直到任意其他触发才刷新）。
func (h *Hub) UnregisterAgent(nodeID string, conn *agentConn) {
	offline := false
	// 锁内临界区：defer Unlock 回收（防锁泄漏；与 RegisterAgent/AddClient 同模式）。
	func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if ns, ok := h.nodes[nodeID]; ok && ns.conn == conn {
			ns.conn = nil
			ns.info.Online = false
			ns.info.LastSeen = time.Now().UnixMilli()
			ns.info.RunningTasks = 0 // 下线清零陈旧负载（重连后心跳回传恢复真实值）
			// IM 中继：节点下线摘除渠道在线路由（重连注册恢复；离线期间回调 503）。
			if h.imRelay != nil {
				h.imRelay.DetachNode(nodeID)
			}
			h.saveNodes()
			offline = true
		}
	}()
	if offline {
		h.Broadcast(TypeNodes, h.ListNodes())
	}
}

// AgentOffline 节点主动下线（用户关闭远程节点开关 / 退出账号时由客户端发送
// agent.offline 通知服务端）。与 UnregisterAgent 的差异：
//   - 标记 ns.disabled=true：ListNodes 不再返回该节点（移动端/管理端立即从
//     列表消失，区别于断线时的"灰点"在线状态），EnqueueChat/RouteToAgent
//     直接拒绝后续派发与续聊（断线暂存 + 重连补发链路不再适用，因为用户
//     主动表明不再服务；防止"关开关后向原有会话发消息仍能执行"）。
//   - 清空 pending 暂存队列：抛弃断线期间暂存的任务（用户主动下线不应保留
//     待补发任务；与断线被动失联语义不同）。
//   - 关闭 conn + Online=false（同 UnregisterAgent 路径，但 disabled=true
//     使节点从列表彻底消失而非灰点滞留）。
//   - RegisterAgent 自动清除 disabled 标记（用户重新开启开关/重登账号时
//     节点重连自动恢复服务，无需手动复位）。
//
// 调用方负责在锁外广播 nodes/tasks（同 RegisterAgent/UnregisterAgent 模式）。
func (h *Hub) AgentOffline(nodeID string, conn *agentConn) (disabled bool) {
	offline := false
	func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		ns, ok := h.nodes[nodeID]
		if !ok || ns.conn != conn {
			return // 节点已下线 / 连接已替换（迟到通知忽略）
		}
		ns.conn = nil
		ns.info.Online = false
		ns.info.LastSeen = time.Now().UnixMilli()
		ns.info.RunningTasks = 0
		ns.disabled = true // 主动下线标记（ListNodes 过滤 + EnqueueChat 拒绝）
		// 清空暂存队列：主动下线后不再补发任何任务，旧暂存任务直接抛弃。
		dropped := len(ns.pending)
		ns.pending = nil
		// IM 中继摘除（同 UnregisterAgent）。
		if h.imRelay != nil {
			h.imRelay.DetachNode(nodeID)
		}
		h.saveNodes()
		disabled = true
		offline = true
		slog.Info("节点主动下线：从列表移除并拒绝后续派发", "node", nodeID, "dropped_pending", dropped)
	}()
	if offline {
		h.Broadcast(TypeNodes, h.ListNodes())
		h.Broadcast(TypeTasks, h.ListTasks(100))
	}
	return disabled
}

// reapStaleRunningLocked 收敛节点断连期间遗留的 running 僵尸任务（调用方持写锁）。
// 无 IsRunning 权威源可用（连接已断），全部转 failed；返回收敛条数（0 = 无），
// 调用方据以决定是否广播 tasks（RegisterAgent 解锁后推送，见上）。
func (h *Hub) reapStaleRunningLocked(nodeID string) int {
	reaped := 0
	for _, t := range h.tasks {
		if t.NodeID != nodeID || t.Status != TaskStatusRunning {
			continue
		}
		t.Status = TaskStatusFailed
		// 文案须避免「真失败」误导：节点本地会话可能实际已完成，仅连接中断
		// 导致状态失联；核实链路（移动端刷新小按钮 → verify.tasks）可按节点
		// 本地会话档案回核校正回真实状态。
		t.Error = "节点连接中断，状态暂标失败（可点任务面板刷新核实真实状态）"
		t.FinishedAt = time.Now().UnixMilli()
		reaped++
	}
	if reaped > 0 {
		// CurrentTask 徽标与并发计数同步清零（TouchAgent 仅在 running<=0 时清，
		// 断连期间不会收到心跳，此处手动清避免节点卡显示陈旧任务号/负载）。
		if ns := h.nodes[nodeID]; ns != nil {
			ns.info.CurrentTask = ""
			ns.info.RunningTasks = 0
		}
		h.saveNodes()
		h.saveTasks()
		slog.Info("节点重连：收敛断连期间遗留的运行中任务", "node", nodeID, "count", reaped)
	}
	return reaped
}

// StartWatchdog 节点失联巡检（修复：断线只标 Online=false，running 任务
// 永远滞留「运行中」，收敛仅挂在节点重连时机 —— 节点失联后一直不回来
// （关机/换网/卸载），任务卡/会话徽标/ABORT 按钮全部永久卡死，手动
// 刷新也无效，因为权威源本身是脏的）。
//
// 每 30s 扫一轮：对 LastSeen 超过宽限期（grace，<=0 用默认 150s ≈ 10 个
// 心跳周期，容忍心跳抖动与慢网）且无在线连接的节点，把 running 任务收敛
// failed（复用 reapStaleRunningLocked 语义），并广播 nodes + tasks 让在线端
// 立即收敛 UI（绿点变灰、徽标归零、ABORT 消失）。正常在线节点（心跳 15s
// 周期）LastSeen 持续刷新，不会误伤；优雅下线（正常退出主动断连）走
// UnregisterAgent 即时广播，巡检只兜底异常失联（断网/休眠/关机）。
func (h *Hub) StartWatchdog(grace time.Duration) {
	if grace <= 0 {
		grace = 150 * time.Second
	}
	// Stop 先于 Start：避免旧 goroutine 残留导致双巡检（重启场景）。
	if h.watchdogStop != nil {
		close(h.watchdogStop)
		h.watchdogStop, h.watchdogDone = nil, nil
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	h.watchdogStop = stop
	h.watchdogDone = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				h.sweepDisconnected(grace)
			case <-stop:
				return
			}
		}
	}()
}

// StopWatchdog 停止失联巡检（幂等；服务关闭时调用）。同步等待巡检
// goroutine 退出，返回后保证无残留定时器（避免关闭期尾焰广播）。
func (h *Hub) StopWatchdog() {
	if h.watchdogStop == nil {
		return
	}
	stop, done := h.watchdogStop, h.watchdogDone
	h.watchdogStop, h.watchdogDone = nil, nil
	close(stop)
	if done != nil {
		<-done // goroutine 退出后才返回（sweep 进行中也等它扫完本轮）
	}
}

// sweepDisconnected 一轮失联巡检（调用方持锁范围见内）：找出断连超宽限期的
// 节点，收敛其 running 任务；返回发生变化的节点数（0 = 无事发生不广播）。
func (h *Hub) sweepDisconnected(grace time.Duration) int {
	cutoff := time.Now().UnixMilli() - grace.Milliseconds()
	// defer 兜底解锁：巡检为后台 goroutine（watchdog），锁内 panic（如将来 map
	// 结构变更引入的竞态）若泄漏 h.mu 将复现 10-09 全服务挂死形态；同时支持
	// 显式提前解锁（广播在锁外）——unlocked 标志保证解锁仅发生一次。
	h.mu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			h.mu.Unlock()
		}
	}()
	dirty := map[string]bool{}
	for id, ns := range h.nodes {
		if ns.conn != nil {
			continue // 在线（或连接刚建）：跳过
		}
		if ns.info.LastSeen == 0 || ns.info.LastSeen > cutoff {
			continue // 从未上线的空卡 / 宽限期内（含 LastSeen 为零值的新节点）：跳过
		}
		if reaped := h.reapStaleRunningLocked(id); reaped > 0 {
			dirty[id] = true
			slog.Warn("节点失联巡检：断连超宽限期，运行中任务自动收敛", "node", id, "count", reaped, "lastSeenAgo", time.Since(time.UnixMilli(ns.info.LastSeen)).Round(time.Second))
		}
	}
	if len(dirty) == 0 {
		return 0
	}
	// 锁内工作完成，先解锁再广播（Broadcast/ListNodes 内部会再取 h.mu 读锁，
	// 持有写锁调用即自死锁）。
	h.mu.Unlock()
	unlocked = true
	// 解锁后广播：节点在线状态（灰点）+ 任务列表（徽标/计时/ABORT 收敛）一次推平。
	h.Broadcast(TypeNodes, h.ListNodes())
	h.Broadcast(TypeTasks, h.ListTasks(100))
	return len(dirty)
}

// TouchAgent 心跳刷新：LastSeen + 节点并发计数同步。running 为节点本地真实
// 运行中任务数（权威源），负值容错归零；计数变化时置脏落盘。节点卡并发展示
// 与 PickNode 负载感知共用此值：心跳间隔内的瞬时变化由 dispatchLocked 增量
// 维护，心跳到达后以节点回传覆盖校正（最终一致）。
func (h *Hub) TouchAgent(nodeID string, running int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ns, ok := h.nodes[nodeID]; ok {
		ns.info.LastSeen = time.Now().UnixMilli()
		if running < 0 {
			running = 0
		}
		if running <= 0 {
			ns.info.CurrentTask = ""
		}
		if ns.info.RunningTasks != running {
			ns.info.RunningTasks = running
			h.saveNodes()
		}
	}
}

// ListNodes 节点列表（在线在前，按名称排序）。
// 过滤 disabled=true 的节点：主动下线（agent.offline）的节点立即从移动端/
// 管理端列表消失，区别于断线时的"灰点"在线状态。RegisterAgent 清除 disabled
// 后节点重新出现。
func (h *Hub) ListNodes() []Node {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Node, 0, len(h.nodes))
	for _, ns := range h.nodes {
		if ns.disabled {
			continue // 主动下线：从列表彻底移除
		}
		out = append(out, ns.info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// NodeOnline 查询节点是否在线。
func (h *Hub) NodeOnline(nodeID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ns, ok := h.nodes[nodeID]
	return ok && ns.conn != nil
}

// NodeDisabled 查询节点是否被主动下线（agent.offline 标记，EnqueueChat /
// RouteToAgent 拒绝派发的判据；与 NodeOnline 互补：disabled=true 节点即便
// 内存中 Online=false 也不再接受任务派发与续聊）。
func (h *Hub) NodeDisabled(nodeID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ns, ok := h.nodes[nodeID]
	return ok && ns.disabled
}

// CanControl 指挥权校验（P-Account 账号隔离核心）：登录账号能否指挥指定节点。
// 规则：
//   - admin 角色：全权（运维账号可指挥任意节点，含未绑定账号的历史节点）；
//   - 节点 Owner 为空：拒绝（未登录账号的 PC 节点不可被指挥 —— "仅连接不登录"
//     状态下服务端/客户端均无账号可用，必须 PC 端登录后才可被指挥）；
//   - 节点 Owner != 登录账号：拒绝（不同账号不可互相指挥）。
//
// 返回错误（非 nil = 拒绝）供服务端直接回 op.result 提示。
func (h *Hub) CanControl(nodeID, user, role string) error {
	if role == "admin" {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	ns, ok := h.nodes[nodeID]
	if !ok {
		return fmt.Errorf("节点 %s 不存在", nodeID)
	}
	if ns.info.Owner == "" {
		return fmt.Errorf("节点 %s 未绑定账号（PC 端未登录），不可指挥", nodeID)
	}
	if ns.info.Owner != user {
		return fmt.Errorf("无权指挥节点 %s：该节点归属账号 %s", nodeID, ns.info.Owner)
	}
	return nil
}

// NodeOwner 查询节点归属账号（不存在返回空）。
func (h *Hub) NodeOwner(nodeID string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if ns, ok := h.nodes[nodeID]; ok {
		return ns.info.Owner
	}
	return ""
}

// SetNodeMode 更新节点当前执行模式（set.mode 路由后由服务端/节点上报回写）。
// 节点离线时仅更新内存展示值（上线注册时会重新上报真实值）。
func (h *Hub) SetNodeMode(nodeID, mode string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ns, ok := h.nodes[nodeID]; ok {
		ns.info.Mode = mode
		h.saveNodes()
	}
}

// NodeMode 节点当前执行模式。
func (h *Hub) NodeMode(nodeID string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if ns, ok := h.nodes[nodeID]; ok {
		return ns.info.Mode
	}
	return ""
}

// PickNode 选一个在线节点：指定 ID 优先；否则负载感知 —— 在线节点中选
// RunningTasks 最少者（并发数相同按名称序稳定决胜，消除 map 遍历随机性）。
// 计数来源：节点心跳回传（权威）+ dispatchLocked 派发增量（心跳间隔内即时
// 感知）+ 终态各同步点递减；未指定节点时按最空闲选择，避免全部任务压向
// 单一节点（串行根因之一）。
func (h *Hub) PickNode(preferred string) (string, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if preferred != "" {
		if ns, ok := h.nodes[preferred]; ok {
			if ns.disabled {
				return "", fmt.Errorf("节点 %s 已下线，无法接收消息", preferred)
			}
			if ns.conn != nil {
				return preferred, nil
			}
			return "", fmt.Errorf("节点 %s 不在线", preferred)
		}
		return "", fmt.Errorf("节点 %s 不在线", preferred)
	}
	best := ""
	bestLoad := 0
	for _, ns := range h.nodes {
		if ns.conn == nil || ns.disabled {
			continue
		}
		load := ns.info.RunningTasks
		if load < 0 {
			load = 0
		}
		if best == "" || load < bestLoad ||
			(load == bestLoad && ns.info.Name < h.nodes[best].info.Name) {
			best, bestLoad = ns.info.ID, load
		}
	}
	if best == "" {
		return "", fmt.Errorf("没有在线节点")
	}
	return best, nil
}

// PickNodeFor 账号感知自动选节点（P-Account 指挥权隔离配套）：admin 退化为
// PickNode 全量负载感知；非 admin 仅在「在线且归属自己（Owner==user）」的节点
// 中做同样负载感知选择 —— 手机端自动选节点（nodeId 空）不再命中他人/未绑定
// 节点，天然过 CanControl 守卫。无归属在线节点时报错文案区分「无自己的节点」。
// preferred 非空时同 PickNode 定向语义（守卫仍由调用方 CanControl 把关）。
func (h *Hub) PickNodeFor(preferred, user, role string) (string, error) {
	if role == "admin" {
		return h.PickNode(preferred)
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if preferred != "" {
		if ns, ok := h.nodes[preferred]; ok && !ns.disabled && ns.conn != nil {
			return preferred, nil
		}
		return "", fmt.Errorf("节点 %s 不在线", preferred)
	}
	best := ""
	bestLoad := 0
	for _, ns := range h.nodes {
		if ns.conn == nil || ns.disabled || ns.info.Owner == "" || ns.info.Owner != user {
			continue
		}
		load := ns.info.RunningTasks
		if load < 0 {
			load = 0
		}
		if best == "" || load < bestLoad ||
			(load == bestLoad && ns.info.Name < h.nodes[best].info.Name) {
			best, bestLoad = ns.info.ID, load
		}
	}
	if best == "" {
		return "", fmt.Errorf("没有属于账号 %s 的在线节点（PC 端需登录同一账号）", user)
	}
	return best, nil
}

// ---------- 客户端管理 ----------

// AuthClient 校验客户端凭据：token 匹配共享访问令牌，或 user+pass（统一账号表）。
// 返回 role（admin | client）与用户名。同一账号在管理后台/移动端均可登录（互通）。
func (h *Hub) AuthClient(user, pass, token string) (string, string, bool) {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	// token 直接匹配 agent token 不允许（防止移动端复用节点令牌）。
	if token != "" {
		// 简单方案：token 亦可为共享访问令牌（server 启动参数 -client-token）。
		if h.clientToken != "" && token == h.clientToken {
			return "client", "token", true
		}
	}
	a, ok := h.users[user]
	if !ok || a.Pass != pass || pass == "" {
		return "", "", false
	}
	if a.Role == "admin" {
		return "admin", a.User, true
	}
	return "client", a.User, true
}

// lockMuWithTimeout 在 timeout 内轮询尝试获取 h.mu 写锁（Go 1.18+ TryLock）。
// 用于对外部探活/认证等「不能无限等待」的路径：h.mu 被异常持有（panic 未回收 /
// 极端情况）时调用方拿到 false 回错误、主动失败关闭，而不是永久阻塞拖垮服务
// （10-09 事故形态：h.mu 永久持有 → AddClient / health / agent 注册全部挂死）。
// 说明：正常路径不经过此函数（直接用 Lock，无额外开销），仅兜底路径使用。
func (h *Hub) lockMuWithTimeout(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if h.mu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// rlockMuWithTimeout 在 timeout 内轮询尝试获取 h.mu 读锁（TryRLock）。
// 语义同 lockMuWithTimeout，供 /api/health 等读快照探活路径用。
func (h *Hub) rlockMuWithTimeout(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if h.mu.TryRLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// clientAuthMuTimeout 客户端认证登记的最大等待时长（超过视为服务异常，
// 回 auth.err 关闭连接，避免前端永久悬挂）。
const clientAuthMuTimeout = 5 * time.Second

// AddClient / RemoveClient 连接登记。
func (h *Hub) AddClient(c *clientConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = true
}

// AddClientTimeout 带超时登记客户端（handleClientWS 认证路径用）：h.mu 短时
// 不可得直接返回 false（调用方回 auth.err + 关闭连接，前端不再永久等待）。
func (h *Hub) AddClientTimeout(c *clientConn, timeout time.Duration) bool {
	if !h.lockMuWithTimeout(timeout) {
		return false
	}
	defer h.mu.Unlock()
	h.clients[c] = true
	return true
}

func (h *Hub) RemoveClient(c *clientConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
}

// ListNodesTimeout 带超时的节点列表快照（/api/health 探活用）：锁不可得返回
// false，调用方回 503 —— 探活永不挂死（防负载均衡/监控判定服务失联）。
func (h *Hub) ListNodesTimeout(timeout time.Duration) ([]Node, bool) {
	if !h.rlockMuWithTimeout(timeout) {
		return nil, false
	}
	defer h.mu.RUnlock()
	out := make([]Node, 0, len(h.nodes))
	for _, ns := range h.nodes {
		out = append(out, ns.info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].Name < out[j].Name
	})
	return out, true
}

// Broadcast 向全部已认证客户端广播。
// 并行写（每连接独立 goroutine，5s 写超时由 writeJSON deadline 保证）：修复某个
// 客户端连接假死（半开，写阻塞直至超时）时串行写拖慢事件分发到其他客户端，
// 事件风暴期间节点读循环心跳也被拖慢而误判超时断线。
func (h *Hub) Broadcast(typ string, payload interface{}) {
	e := NewEnvelope("", typ, payload)
	h.mu.RLock()
	targets := make([]*clientConn, 0, len(h.clients))
	for c := range h.clients {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	var wg sync.WaitGroup
	for _, c := range targets {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 兕底：任何绕过 sendMu 的直接写（历史/未来新增路径）触发 gorilla
			// concurrent-write panic 时，仅断开该连接而非带崩整个进程（10-09
			// 线上 restart counter=104 崩溃循环教训：一次裸写 = 全体用户断连）。
			defer func() {
				if r := recover(); r != nil {
					slog.Error("广播写连接 panic（已拦截，断开该连接）", "err", r)
					h.RemoveClient(c)
				}
			}()
			if err := c.writeJSON(e); err != nil {
				h.RemoveClient(c)
			}
		}()
	}
	wg.Wait()
}

// ---------- 任务管理 ----------

// CreateTask 受理新任务：生成 ID、持久化、派发或暂存（mode 空 = 节点当前模式）。
func (h *Hub) CreateTask(nodeID, text, mode, createdBy string) (*Task, error) {
	return h.CreateTaskKinded(nodeID, text, mode, createdBy, "", "")
}

// CreateTaskKinded 带类别受理新任务（paint = 绘画任务）。
// kind=paint 时在任务文本尾部注入生图指令（模型侧经 generate_image 工具出图，
// 图片经节点上传服务端 → image 事件 → Task.Images 回填 → 移动端绘画 tab 展示）。
func (h *Hub) CreateTaskKinded(nodeID, text, mode, createdBy, kind, spot string) (*Task, error) {
	if text == "" {
		return nil, fmt.Errorf("任务内容不能为空")
	}
	if kind == "" {
		kind = TaskKindGeneral
	}
	if kind != TaskKindGeneral && kind != TaskKindPaint {
		return nil, fmt.Errorf("无效的任务类别: %s", kind)
	}
	if kind == TaskKindPaint {
		spot = strings.TrimSpace(spot)
		prefix := "【绘画任务】请用 generate_image 工具生成图片（不要只用文字描述）："
		if spot != "" {
			prefix += "主题「" + spot + "」，"
		}
		text = prefix + strings.TrimSpace(text) +
			"。生成后用一两句话确认即可，不要重复描述画面。"
	}
	// 选节点。
	nodeID, err := h.PickNode(nodeID)
	if err != nil {
		return nil, err
	}
	t := &Task{
		ID:        newID("t"),
		SessionID: "remote-" + newID("s"),
		NodeID:    nodeID,
		Text:      text,
		Mode:      mode,
		Kind:      kind,
		Spot:      strings.TrimSpace(spot),
		Status:    TaskStatusPending,
		CreatedBy: createdBy,
		CreatedAt: time.Now().UnixMilli(),
	}
	h.mu.Lock()
	ns := h.nodes[nodeID] // 在线判定 + 离线暂存（同锁内，竞态安全）
	h.tasks[t.ID] = t
	h.saveTasks()
	online := ns != nil && ns.conn != nil
	if !online {
		// 节点离线暂存。
		if ns == nil {
			ns = &nodeState{}
			ns.info.ID = nodeID
			ns.info.FirstSeen = time.Now().UnixMilli()
			h.nodes[nodeID] = ns
		}
		ns.pending = append(ns.pending, t)
		if len(ns.pending) > h.maxPendingPerNode {
			ns.pending = ns.pending[1:]
		}
	}
	h.mu.Unlock()
	// 应答时序（2026-10「移动端发布任务请求超时」根因修复）：派发与列表
	// 广播全部异步 —— 半死节点连接的 WS 写最长 10s、弱网客户端的广播写
	// 同样可拖满 10s，同步执行会把 task.created 应答拖到移动端 8s 超时
	// 之后（前端伪造「请求超时」但任务实际已入库 → 老师重发 → 重复任务）。
	// 受理（入库+落盘置脏）完成后立即应答，派发/列表广播在后台收敛。
	go func() {
		if online {
			h.dispatchTask(t)
		}
		h.Broadcast(TypeTasks, h.ListTasks(100))
	}()
	return t, nil
}

// dispatchTask 派发单个任务（线程安全，WS 写在锁外）。
// 锁内仅做终态检查、状态迁移（pending → running）与连接快照；真正的 WS 写在
// 锁外执行 —— 节点连接半死时写超时（10s）不再持 h.mu 阻塞全局（原
// dispatchLocked 锁内写帧，CreateTaskKinded 派发可拖住全部客户端消息达 10s）。
// 写失败重新加锁回退暂存队列，节点重连后经 RegisterAgent 补发重试（连接
// 抖动容错）。返回 true = 已派发或已回退暂存；false = 终态/无连接跳过。
func (h *Hub) dispatchTask(t *Task) bool {
	h.mu.Lock()
	if t.Status != TaskStatusPending {
		h.mu.Unlock()
		return false // 终态（done/failed/canceled）不重派：防节点离线期间被取消/
		// 已完成的暂存任务在补发时被重复执行（服务重启恢复链路依赖此保护）。
	}
	ns := h.nodes[t.NodeID]
	if ns == nil || ns.conn == nil || ns.disabled {
		h.mu.Unlock()
		return false // 节点离线 / 主动下线（agent.offline）：不派发，由后续重连/补发链路处理（disabled 节点不补发）
	}
	conn := ns.conn
	t.Status = TaskStatusRunning
	t.StartedAt = time.Now().UnixMilli()
	ns.info.CurrentTask = t.ID
	h.mu.Unlock()

	e := NewEnvelope("", TypeServerTaskDispatch, TaskDispatch{
		TaskID:    t.ID,
		SessionID: t.SessionID,
		Text:      t.Text,
		Mode:      t.Mode,
		Kind:      t.Kind,
		Spot:      t.Spot,
	})
	if err := conn.writeJSON(e); err != nil {
		slog.Error("派发任务失败，任务回退暂存待补发", "task", t.ID, "err", err)
		h.mu.Lock()
		// 窗口内未被并发改态（如迟到 TaskDone 终态）才回退，终态幂等保护。
		if t.Status == TaskStatusRunning {
			t.Status = TaskStatusPending
			t.StartedAt = 0
			ns2 := h.nodes[t.NodeID]
			if ns2 == nil { // 写期间节点被注销：重建占位，回退暂存待重连补发
				ns2 = &nodeState{}
				ns2.info.ID = t.NodeID
				ns2.info.FirstSeen = time.Now().UnixMilli()
				h.nodes[t.NodeID] = ns2
			}
			ns2.info.CurrentTask = ""
			ns2.pending = append(ns2.pending, t)
			if len(ns2.pending) > h.maxPendingPerNode {
				ns2.pending = ns2.pending[1:]
			}
		}
		h.mu.Unlock()
		return true
	}
	// 写成功才计并发（失败回退路径天然不计数）：心跳周期内 PickNode 立即
	// 感知负载变化；节点心跳回传随后覆盖校正（权威源）。写完连接已被替换
	//（节点重连）则不计，重连链路 reapStaleRunning + 心跳回传会自行校正。
	h.mu.Lock()
	if ns3 := h.nodes[t.NodeID]; ns3 != nil && ns3.conn == conn {
		ns3.info.RunningTasks++
	}
	h.mu.Unlock()
	return true
}

// TaskDone 处理任务终态。
// 终态保护（修复：手机端 ABORT 后 PC 端迟到终态回传把任务覆盖回 done/failed，
// 卡片状态跳变「取消→完成」，观感即“取消/切换不管用”）：
//   - 已终态（done/failed/canceled）的任务不再被迟到回报覆盖（幂等收敛）；
//   - canceled 特例：允许空的重复取消回报（ok=false canceled=true）幂等通过，
//     但不带 canceled 标记的迟到正常结果不得复活已取消任务。
func (h *Hub) TaskDone(d TaskDone) {
	h.mu.Lock()
	t := h.tasks[d.TaskID]
	if t == nil {
		h.mu.Unlock()
		return
	}
	if t.Status != TaskStatusPending && t.Status != TaskStatusRunning {
		// 断线终态回填（P-Reliable）：PC 端补发的迟到终态（Outbox）vs 服务端已收敛
		//（failed 且错误文案含 reapErrorMark —— 断线时 reapStaleRunningLocked 标记）。
		// 节点本地会话真实执行完成（回传 ok=true）→ 校正回 done：修复断线期间完成的
		// 任务在重连后被永久误标「失败」（文案还提示用户去刷新核实，多此一举）。
		if d.Outbox && d.OK && t.Status == TaskStatusFailed && strings.Contains(t.Error, reapErrorMark) {
			t.Status = TaskStatusDone
			t.Result = d.Result
			t.Error = ""
			t.FinishedAt = time.Now().UnixMilli()
			if ns := h.nodes[t.NodeID]; ns != nil && ns.info.CurrentTask == t.ID {
				ns.info.CurrentTask = ""
				h.saveNodes()
			}
			h.saveTasks()
			slog.Info("节点补发终态回填：断线收敛任务校正回完成", "task", t.ID)
			h.mu.Unlock()
			h.Broadcast(TypeTaskResult, map[string]interface{}{
				"taskId": d.TaskID, "sessionId": t.SessionID, "ok": true,
				"result": d.Result, "task": t, "outbox": true,
			})
			return
		}
		// 已终态：迟到回报仅幂等重放广播（保底让未收到的客户端补一次终态），不改动状态。
		snapshot := *t
		h.mu.Unlock()
		h.Broadcast(TypeTaskResult, map[string]interface{}{
			"taskId": d.TaskID, "sessionId": snapshot.SessionID,
			"ok": d.OK, "result": d.Result, "error": d.Error, "task": &snapshot,
		})
		return
	}
	wasRunning := t.Status == TaskStatusRunning
	if d.OK {
		t.Status = TaskStatusDone
		t.Result = d.Result
	} else {
		t.Status = TaskStatusFailed
		t.Error = d.Error
	}
	t.FinishedAt = time.Now().UnixMilli()
	dec := false
	if ns := h.nodes[t.NodeID]; ns != nil {
		if ns.info.CurrentTask == t.ID {
			ns.info.CurrentTask = ""
			dec = true
		}
		// running 任务终态 → 并发计数递减（pending 未派发从未计数，不减；
		// 下限 0 防护心跳覆盖窗口内的竞态误减）。
		if wasRunning && ns.info.RunningTasks > 0 {
			ns.info.RunningTasks--
			dec = true
		}
		if dec {
			h.saveNodes()
		}
	}
	h.saveTasks()
	h.mu.Unlock()
	h.Broadcast(TypeTaskResult, map[string]interface{}{
		"taskId": d.TaskID, "sessionId": t.SessionID, "ok": d.OK,
		"result": d.Result, "error": d.Error, "task": t,
	})
	// 会话任务自动清理：会话数超上限（20）时删最旧终态会话；
	// 删除后广播 tasks/sessions 让全部在线客户端同步收敛。
	if h.pruneFinishedSessions() > 0 {
		h.Broadcast(TypeTasks, h.ListTasks(100))
		h.Broadcast("sessions", map[string]interface{}{"sessions": h.ListSessionSummaries()})
	}
}

// ApplyVerifyReport 节点任务状态核实回报落地（VerifyTasks 全链路第 3 步）。
// 节点侧已按本地会话档案判定真实状态（service 端口径：RunningSessions=运行中，
// 定型 completion_result=done，定型 error=失败），此处以回报为准校正服务端任务：
//   - 仅校正 pending/running 两态（节点可判定范围）；终态任务不被覆盖（幂等，
//     与 TaskDone 终态保护同语义，防迟到回报回跳）；
//   - 校正时同步 Result/Error/FinishedAt，并清节点 CurrentTask 徽标；
//   - 被校正为终态的任务同时回播 task.result（供各端补一次终态通知）；
//
// 全部核对完毕后广播 verify.done + 全量 tasks（移动端提示 + 列表收敛）。
// RequestedBy 透传自 server.verify_tasks 发起端标记，verify.done 广播携带，
// 移动端据此区分本端发起（结果提示文案）与他端发起（静默列表刷新）。
func (h *Hub) ApplyVerifyReport(rep VerifyReport) {
	h.mu.Lock()
	fixed, checked := 0, 0
	var detail []string
	changed := false
	for _, it := range rep.Items {
		t := h.tasks[it.TaskID]
		if t == nil || it.Status == "" {
			continue
		}
		checked++
		switch it.Status {
		case TaskStatusRunning:
			if t.Status == TaskStatusPending {
				t.Status = TaskStatusRunning
				if t.StartedAt == 0 {
					t.StartedAt = time.Now().UnixMilli()
				}
				fixed++
				detail = append(detail, t.ID+": pending→running（节点本地会话执行中）")
				changed = true
			}
		case TaskStatusDone, TaskStatusFailed:
			// 节点判定终态且服务端仍是活动态 → 以节点事实为准校正。
			if t.Status == TaskStatusPending || t.Status == TaskStatusRunning {
				wasRunning := t.Status == TaskStatusRunning
				if it.Status == TaskStatusDone {
					t.Status = TaskStatusDone
					t.Result = it.Result
					t.Error = ""
				} else {
					t.Status = TaskStatusFailed
					t.Error = it.Error
					if t.Error == "" {
						t.Error = "节点核实：本地会话已失败（无错误详情）"
					}
				}
				t.FinishedAt = time.Now().UnixMilli()
				// 校正为终态 → CurrentTask 清零 + running 并发计数递减（pending
				// 未派发从未计数，仅 wasRunning 时递减；下限 0 防护竞态）。
				if ns := h.nodes[t.NodeID]; ns != nil {
					if ns.info.CurrentTask == t.ID {
						ns.info.CurrentTask = ""
					}
					if wasRunning && ns.info.RunningTasks > 0 {
						ns.info.RunningTasks--
					}
					h.saveNodes()
				}
				fixed++
				detail = append(detail, t.ID+": "+t.Status+"→"+it.Status)
				changed = true
				// 校正为终态 → 按节点事实补发终态广播（各端收尾通知/徽标收敛）。
				snap := *t
				h.mu.Unlock()
				h.Broadcast(TypeTaskResult, map[string]interface{}{
					"taskId": snap.ID, "sessionId": snap.SessionID,
					"ok": it.Status == TaskStatusDone, "result": snap.Result,
					"error": snap.Error, "task": &snap, "verify": true,
				})
				h.mu.Lock()
			}
		}
	}
	if changed {
		h.saveTasks()
	}
	h.mu.Unlock()
	if changed {
		h.Broadcast(TypeTasks, h.ListTasks(100))
	}
	// 会话任务自动清理：会话数超上限（20）时删最旧终态会话（核实校正为
	// 终态同样可能触发超限）；删除后广播 tasks/sessions 收敛各端。
	if h.pruneFinishedSessions() > 0 {
		h.Broadcast(TypeTasks, h.ListTasks(100))
		h.Broadcast("sessions", map[string]interface{}{"sessions": h.ListSessionSummaries()})
	}
	h.Broadcast(TypeVerifyDone, VerifyDone{
		OK: true, Checked: checked, Fixed: fixed,
		Detail: strings.Join(detail, "\n"), RequestedBy: rep.RequestedBy,
	})
	slog.Info("任务状态核实回报落地", "node", rep.NodeID, "checked", checked, "fixed", fixed)
}

// AddSubtask 子会话投影更新（agent.subtask 处理）。
// 语义：子会话是节点本地平行会话（无服务端任务记录），服务端仅投影到会话列表
// （带 parentId 缩进展示）；start（running）→ end（done/failed/canceled）两帧幂等
// 覆盖同一 sessionID 槽位，不持久化（节点重连补传 = 最新帧覆盖旧槽位）。
func (h *Hub) AddSubtask(ev SubtaskEvent) {
	if ev.SessionID == "" {
		return
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	h.mu.Lock()
	h.subtasks[ev.SessionID] = ev
	h.mu.Unlock()
	h.Broadcast("sessions", map[string]interface{}{"sessions": h.ListSessionSummaries()})
}

// AddTaskEvent 记录并广播任务事件。
// 会话镜像同步追加（定型帧，say 语义）：task.event 只在内存 events 环形缓冲
// （500 条上限）且不落盘，节点断线/服务重启后丢失；镜像（sessions.json）才是
// 手机端 get.messages 的数据源。此前镜像唯一写入源是节点回传 agent.messages
// （依赖在线拉取），断线期间执行的任务镜像空白 → 手机端点开任务卡无聊天记录。
// 此处从事件流直接落镜像：get.messages 立即应答即有内容（节点回传到达后再覆盖为完整结构）。
// 请求帧带请求 id（客户端 pending）时同时回写同 id 的 messages 响应，
// 避免广播（无 id）被前端 pending 通道吞掉、页面首次加载空白。
func (h *Hub) AddTaskEvent(ev TaskEvent) {
	saveMirror := false
	h.mu.Lock()
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	// 广播前按 taskID 反查归属节点：PC 端客户端据此按本机身份过滤（2026-11
	// 用户需求：执行端仅看本机执行/本机发起的任务，不渲染他机广播会话）。
	t := h.tasks[ev.TaskID]
	if t != nil && ev.NodeID == "" {
		ev.NodeID = t.NodeID
	}
	evs := h.events[ev.TaskID]
	evs = append(evs, ev)
	if len(evs) > h.maxEventsPerTask {
		evs = evs[len(evs)-h.maxEventsPerTask:]
	}
	h.events[ev.TaskID] = evs
	syncMirror := ev.SessionID != "" && !ev.Partial &&
		ev.Kind != "ask" && ev.Kind != "started" && ev.Text != ""
	if syncMirror {
		raw, err := json.Marshal(map[string]string{
			"type": "say", "say": ev.Kind, "text": ev.Text,
		})
		if err == nil {
			raws := append(h.sessions[ev.SessionID], raw)
			if len(raws) > h.maxSessionMirrorMsgs {
				raws = raws[len(raws)-h.maxSessionMirrorMsgs:]
			}
			h.sessions[ev.SessionID] = raws
			saveMirror = true
		}
	}
	h.mu.Unlock()
	if saveMirror {
		h.saveSessions()
	}
	h.Broadcast(TypeTaskEvent, ev)
}

// AttachTaskImages 图片事件回填：把节点上传后的图片 URL 归档到任务记录并广播
// task.event(kind=image)。paint 任务靠它把生成图写入 Task.Images（随 tasks 推送、
// tasks.json 持久化，移动端绘画 tab 直接拿到 URL）；普通任务同样兼容（聊天流内嵌图）。
// 返回回填后的 URL 列表（去重追加；终态后仍可回填，兜底迟到图片不丢）。
func (h *Hub) AttachTaskImages(taskID string, urls []string) []string {
	if taskID == "" || len(urls) == 0 {
		return nil
	}
	h.mu.Lock()
	t, ok := h.tasks[taskID]
	if !ok {
		h.mu.Unlock()
		return nil
	}
	merged := t.Images
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		dupe := false
		for _, old := range merged {
			if old == u {
				dupe = true
				break
			}
		}
		if !dupe {
			merged = append(merged, u)
		}
	}
	t.Images = merged
	h.saveTasks()
	images := append([]string(nil), t.Images...)
	h.mu.Unlock()
	return images
}

// Dir 图片根目录（服务端 /img/ 静态托管源；SetImageDir 注入，空 = dir/images）。
func (h *Hub) Dir() string { return h.dir }

// Broadcast 带请求 id 透传的可选变体：镜像同步后回写同 id 的 messages 帧，
// 保证发起方（get.messages）的 pending 能拿到响应。
func (h *Hub) BroadcastWithID(typ string, payload interface{}, reqID string) {
	if reqID == "" {
		h.Broadcast(typ, payload)
		return
	}
	h.mu.RLock()
	conns := make([]*clientConn, 0, len(h.clients))
	for c2 := range h.clients {
		conns = append(conns, c2)
	}
	h.mu.RUnlock()
	for _, c2 := range conns {
		_ = c2.writeJSON(NewEnvelope(reqID, typ, payload))
	}
}

// TaskEvents 拉取任务事件历史（重连补发）。
func (h *Hub) TaskEvents(taskID string) []TaskEvent {
	h.mu.RLock()
	defer h.mu.RUnlock()
	evs := h.events[taskID]
	out := make([]TaskEvent, len(evs))
	copy(out, evs)
	return out
}

// GetTask 查询任务。
func (h *Hub) GetTask(id string) (*Task, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	t := h.tasks[id]
	return t, t != nil
}

// ListTasks 任务列表（max 条，最新在前）。
func (h *Hub) ListTasks(max int) []*Task {
	h.mu.RLock()
	defer h.mu.RUnlock()
	list := h.taskList()
	if max > 0 && len(list) > max {
		list = list[:max]
	}
	return list
}

// QueryTasks 过滤查询任务历史（nodeId/status/search/since/limit，全部可选，
// 条件之间 AND 语义；filter 为 nil 或零值字段不过滤）。
// 非法状态值返回错误（与 TaskFilter.Validate 白名单一致）。
func (h *Hub) QueryTasks(f *TaskFilter) []*Task {
	if f == nil {
		return h.ListTasks(100)
	}
	if err := f.Validate(); err != nil {
		return []*Task{}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	list := h.taskList()
	out := make([]*Task, 0, len(list))
	for _, t := range list {
		if f.Matches(t) {
			out = append(out, t)
			if len(out) >= f.Limit {
				break
			}
		}
	}
	return out
}

// DeleteTask 删除任务记录（仅终态任务可删）：任务、事件环形历史、会话镜像、
// Todo 清单镜像一并清除，避免移动端会话列表/聊天页残留殓尸数据。
// 运行中/待执行任务拒绝删除（先取消再删）；节点侧不通知 —— PC 端任务
// 注册表是执行态权威源，远程记录仅是投影（执行完成后节点侧亦已清理，见 client.go）。
// 返回任务副本与归属节点 ID（server.go 据此路由节点侧会话存档清理）。
func (h *Hub) DeleteTask(taskID string) (Task, string, error) {
	h.mu.Lock()
	t, ok := h.tasks[taskID]
	if !ok {
		h.mu.Unlock()
		return Task{}, "", fmt.Errorf("任务不存在")
	}
	if t.Status == TaskStatusPending || t.Status == TaskStatusRunning {
		h.mu.Unlock()
		return *t, t.NodeID, fmt.Errorf("任务仍在 %s，请先取消再删除", statusTextCN(t.Status))
	}
	sessionID := t.SessionID
	nodeID := t.NodeID
	delete(h.tasks, taskID)
	delete(h.events, taskID)
	h.saveTasks()
	h.mu.Unlock()
	// 会话镜像与 Todo 清单同源清理（sessMu 独立锁）。
	// 该会话仍有关联任务时保留镜像（多任务可共用一个会话，删除单个任务不应清会话）。
	if _, still := h.SessionTaskID(sessionID); !still {
		h.clearSessionMirror(sessionID)
	}
	slog.Info("任务记录已删除", "task", taskID, "session", sessionID)
	return *t, nodeID, nil
}

// DeleteSession 删除整个会话（移动端会话卡入口）：按 sessionID 反查全部关联
// 任务记录逐一删除（终态才可删，与 DeleteTask 同语义）+ 清镜像/事件 + 返回
// 归属节点 ID（server.go 据此路由节点本地会话存档清理，实现「会话删除联动 PC 端」）。
// 部分任务仍在运行时拒绝整体删除（先取消）。
// RenameSession 会话重命名（PC 端右键改名）：按 sessionID 找到全部任务，
// 将手动名称写入 Task.Title（移动端/管理端会话列表与聊天页标题数据源），
// 落盘并返回改写的任务数。空会话/空名返回错误；无关联任务时返回 0（无副作用）。
// 显示侧规则：title 优先 Task.Title，空则回退 firstLineText(Text)。
// PinSession 会话置顶/取消置顶（移动端长按菜单）：按 sessionID 找到全部任务，
// 把 Pinned 时间戳统一写入（置顶 = now，取消 = 0），落盘并返回改写任务数。
// 与 RenameSession 同链路同语义：会话级操作落到会话全部任务记录上，
// 广播后各端列表按 pinned 优先排序（ListSessionSummaries）。
func (h *Hub) PinSession(sessionID string, pinned bool) (int, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return 0, fmt.Errorf("会话不能为空")
	}
	var ts int64
	if pinned {
		ts = time.Now().UnixMilli()
	}
	h.mu.Lock()
	n := 0
	for _, t := range h.tasks {
		if t.SessionID == sessionID && t.Pinned != ts {
			t.Pinned = ts
			n++
		}
	}
	h.mu.Unlock()
	if n > 0 {
		h.saveTasks()
	}
	if pinned {
		slog.Info("会话已置顶", "session", sessionID, "tasks", n)
	} else {
		slog.Info("会话已取消置顶", "session", sessionID, "tasks", n)
	}
	return n, nil
}

func (h *Hub) RenameSession(sessionID, title string) (int, error) {
	sessionID = strings.TrimSpace(sessionID)
	title = strings.TrimSpace(title)
	if sessionID == "" {
		return 0, fmt.Errorf("会话不能为空")
	}
	if title == "" {
		return 0, fmt.Errorf("名称不能为空")
	}
	if r := []rune(title); len(r) > 80 {
		title = string(r[:80])
	}
	h.mu.Lock()
	n := 0
	for _, t := range h.tasks {
		if t.SessionID == sessionID && t.Title != title {
			t.Title = title
			n++
		}
	}
	h.mu.Unlock()
	if n > 0 {
		h.saveTasks()
	}
	slog.Info("会话已重命名", "session", sessionID, "title", title, "tasks", n)
	return n, nil
}

func (h *Hub) DeleteSession(sessionID string) (string, error) {
	h.mu.RLock()
	var taskIDs []string
	nodeID := ""
	for _, t := range h.tasks {
		if t.SessionID != sessionID {
			continue
		}
		if nodeID == "" {
			nodeID = t.NodeID
		}
		if t.Status == TaskStatusPending || t.Status == TaskStatusRunning {
			h.mu.RUnlock()
			return nodeID, fmt.Errorf("会话仍有任务在 %s，请先取消再删除", statusTextCN(t.Status))
		}
		taskIDs = append(taskIDs, t.ID)
	}
	h.mu.RUnlock()
	for _, id := range taskIDs {
		if _, _, err := h.DeleteTask(id); err != nil {
			return nodeID, err
		}
	}
	// 子会话投影联动清：删除的是子会话本身，或删除父会话（其全部子会话投影一并除名）。
	// 投影是纯内存缓存（不持久化），此处顺手清理避免删除后列表残影。
	h.mu.Lock()
	for sid, st := range h.subtasks {
		if sid == sessionID || st.ParentID == sessionID {
			delete(h.subtasks, sid)
		}
	}
	h.mu.Unlock()
	// 兜底：无任务关联的孤儿会话镜像直接清（节点侧仍需清存档）。
	h.clearSessionMirror(sessionID)
	slog.Info("会话记录已删除", "session", sessionID, "tasks", len(taskIDs), "node", nodeID)
	return nodeID, nil
}

// clearSessionMirror 会话镜像 + Todo 清单镜像同源清理。
// 锁拆分：h.sessions 由 h.mu 保护（与 SaveSessionMessages/AddTaskEvent/writeSessionsFile
// 同锁，避免 map 并发读写数据竞争）；h.sessTodos 由 h.sessMu 保护。
func (h *Hub) clearSessionMirror(sessionID string) {
	h.mu.Lock()
	_, hadMsgs := h.sessions[sessionID]
	delete(h.sessions, sessionID)
	h.mu.Unlock()
	h.sessMu.Lock()
	_, hadTodos := h.sessTodos[sessionID]
	delete(h.sessTodos, sessionID)
	h.sessMu.Unlock()
	if hadMsgs {
		h.saveSessions()
	}
	if hadTodos {
		h.saveSessionTodos()
	}
}

// ---------- 会话任务自动清理 ----------

// maxFinishedSessions 服务端会话保留上限：会话数超过后自动按最旧优先清理
// 终态会话（运行中/待执行不受影响）。触发点：任务终态回报 / 任务核实回报 /
// 服务启动。清理走 DeleteSession 同链路（任务记录 + 事件 + 镜像 + Todo 镜像），
// 与移动端手动删除行为完全一致。
const maxFinishedSessions = 20

// pruneFinishedSessions 会话任务自动清理：会话数（从任务记录推导，与
// ListSessionSummaries 同口径）超过 maxFinishedSessions 时，把最旧的终态
// 会话逐个删除，直到回到上限或无可删候选。运行中/待执行会话绝不强删
// （与 DeleteTask/DeleteSession 的终态保护同语义）。返回本次清理的会话数。
func (h *Hub) pruneFinishedSessions() int {
	h.mu.RLock()
	list := h.taskList() // 创建时间倒序
	h.mu.RUnlock()
	// 会话 → 最近创建时间 + 是否有活动任务（与 ListSessionSummaries 同口径）。
	latest := map[string]int64{}
	active := map[string]bool{}
	for _, t := range list {
		if _, ok := latest[t.SessionID]; !ok {
			latest[t.SessionID] = t.CreatedAt
		}
		if t.Status == TaskStatusPending || t.Status == TaskStatusRunning {
			active[t.SessionID] = true
		}
	}
	need := len(latest) - maxFinishedSessions
	if need <= 0 {
		return 0
	}
	// 候选：非活动会话，按最近创建时间升序（最旧优先）。
	type sessAge struct {
		id string
		at int64
	}
	cands := make([]sessAge, 0, len(latest))
	for sid, at := range latest {
		if !active[sid] {
			cands = append(cands, sessAge{id: sid, at: at})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].at < cands[j].at })
	removed := 0
	for _, c := range cands {
		if removed >= need {
			break
		}
		if _, err := h.DeleteSession(c.id); err != nil {
			// 竞态（删除前一刻任务复活）或部分失败：跳过该会话继续下一个。
			slog.Warn("会话自动清理跳过", "session", c.id, "err", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		slog.Info("会话任务自动清理完成", "removed", removed, "total", len(latest), "limit", maxFinishedSessions)
	}
	return removed
}

// pruneOrphanMirrors 孤儿会话镜像清理（服务启动调用一次）：sessions.json 中
// 已无任务关联的镜像条目（历史遗留/手工删库残留）一并清除。运行期不产生
// 新孤儿（DeleteTask/DeleteSession 联动清镜像）。返回清理的条目数。
func (h *Hub) pruneOrphanMirrors() int {
	h.mu.RLock()
	ids := make([]string, 0, len(h.sessions))
	for sid := range h.sessions {
		ids = append(ids, sid)
	}
	h.mu.RUnlock()
	removed := 0
	for _, sid := range ids {
		if _, ok := h.SessionTaskID(sid); ok {
			continue // 仍有关联任务，保留
		}
		h.clearSessionMirror(sid)
		removed++
	}
	if removed > 0 {
		slog.Info("孤儿会话镜像已清理", "count", removed)
	}
	return removed
}

// statusTextCN 任务状态中文（删除拒绝提示用）。
func statusTextCN(st string) string {
	switch st {
	case TaskStatusPending:
		return "待执行"
	case TaskStatusRunning:
		return "执行中"
	case TaskStatusDone:
		return "已完成"
	case TaskStatusFailed:
		return "已失败"
	case TaskStatusCanceled:
		return "已取消"
	}
	return st
}

// CancelTask 取消任务：在线则转发取消；否则直接标记。
// 转发携带 sessionID（节点侧按会话停止执行；旧节点忽略此字段，仅 taskID）。
// 修复：取消时同步清节点 CurrentTask（否则节点页「● EXECUTING」常亮）并
// 广播 tasks（所有端立即从「运行中」筛选消失，不再等下次全量推送）。
func (h *Hub) CancelTask(taskID, sessionID string) {
	h.mu.RLock()
	t := h.tasks[taskID]
	var conn *agentConn
	if t != nil {
		if sessionID == "" {
			sessionID = t.SessionID
		}
		if ns := h.nodes[t.NodeID]; ns != nil {
			conn = ns.conn
		}
	}
	h.mu.RUnlock()
	if conn != nil {
		_ = conn.writeJSON(NewEnvelope("", TypeServerTaskCancel, TaskCancel{TaskID: taskID, SessionID: sessionID}))
	}
	h.mu.Lock()
	canceled := false
	if t != nil && (t.Status == TaskStatusPending || t.Status == TaskStatusRunning) {
		wasRunning := t.Status == TaskStatusRunning
		t.Status = TaskStatusCanceled
		t.FinishedAt = time.Now().UnixMilli()
		canceled = true
		if ns := h.nodes[t.NodeID]; ns != nil {
			if ns.info.CurrentTask == taskID {
				ns.info.CurrentTask = ""
			}
			// 已派发（running）任务取消 → 并发计数递减；pending 从未计数不动。
			if wasRunning && ns.info.RunningTasks > 0 {
				ns.info.RunningTasks--
			}
			h.saveNodes()
		}
		h.saveTasks()
	}
	h.mu.Unlock()
	h.Broadcast(TypeTaskResult, map[string]interface{}{
		"taskId": taskID, "sessionId": sessionID, "ok": false,
		"error": "任务已取消", "canceled": true,
	})
	if canceled {
		h.Broadcast(TypeTasks, h.ListTasks(100)) // 状态快照同步（徽标/筛选/计时全部收敛）
	}
}

// SessionTaskID 反查会话关联的任务（ask_reply / chat 路由用）。
func (h *Hub) SessionTaskID(sessionID string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, t := range h.tasks {
		if t.SessionID == sessionID {
			return t.ID, true
		}
	}
	return "", false
}

// RouteToAgent 把消息路由到指定节点的 agent 连接（chat 续聊 / ask_reply / get_messages）。
// 主动下线保护（agent.offline）：disabled=true 的节点拒绝路由（用户已表明不再
// 服务，ask_reply 等会话内应答不应再发往死节点；移动端会收到明确错误提示）。
func (h *Hub) RouteToAgent(nodeID, typ string, payload interface{}) error {
	h.mu.RLock()
	var conn *agentConn
	disabled := false
	if ns := h.nodes[nodeID]; ns != nil {
		conn = ns.conn
		disabled = ns.disabled
	}
	h.mu.RUnlock()
	if disabled {
		return fmt.Errorf("节点 %s 已下线，无法接收消息", nodeID)
	}
	if conn == nil {
		return fmt.Errorf("节点 %s 不在线", nodeID)
	}
	return conn.writeJSON(NewEnvelope("", typ, payload))
}

// ClearAllTasks 清空全部任务与会话（clear.tasks 链路第 2 步，服务端侧权威数据）。
// 与 DeleteSession 逐会话删除不同：批量入口一次性清空（先全部取消活动任务，
// 再整表清除任务/事件/镜像），避免逐条删除在多任务下的 O(n²) 持久化与广播风暴。
// 流程：
//  1. 活动任务（pending/running）逐个取消：在线节点转发取消（节点停止执行），
//     服务端同步置 canceled（与单任务取消同语义）；
//  2. 全部任务记录、事件环形历史、会话镜像/Todo 镜像、节点离线暂存队列一次性清空；
//  3. 返回曾涉任务记录的节点集合（server.go 据此路由节点本地会话存档清理，
//     避免服务端已空但 PC 端会话列表/存档残留）。调用方负责删除后广播。
//
// 调用方负责删除后广播 tasks/sessions/clear.done（本函数只管数据）。
func (h *Hub) ClearAllTasks() (canceled, deleted int, nodes []string) {
	h.mu.Lock()
	// 1) 活动任务取消：直接置 canceled（转发在锁外逐个补发，与 CancelTask 同语义）。
	activeNodes := map[string]bool{}
	nodeSeen := map[string]bool{}
	var cancelList []Task
	for _, t := range h.tasks {
		if t.NodeID != "" {
			nodeSeen[t.NodeID] = true
		}
		if t.Status != TaskStatusPending && t.Status != TaskStatusRunning {
			continue
		}
		activeNodes[t.NodeID] = true
		wasRunning := t.Status == TaskStatusRunning
		t.Status = TaskStatusCanceled
		t.FinishedAt = time.Now().UnixMilli()
		canceled++
		if ns := h.nodes[t.NodeID]; ns != nil {
			if ns.info.CurrentTask == t.ID {
				ns.info.CurrentTask = ""
			}
			if wasRunning && ns.info.RunningTasks > 0 {
				ns.info.RunningTasks--
			}
		}
		cancelList = append(cancelList, *t)
	}
	// 2) 整表清除：任务记录 + 事件历史 + 离线暂存队列（防节点重连补发已清任务）。
	deleted = len(h.tasks)
	h.tasks = make(map[string]*Task)
	h.events = make(map[string][]TaskEvent)
	h.subtasks = make(map[string]SubtaskEvent)
	for _, ns := range h.nodes {
		ns.pending = nil
	}
	h.saveTasks()
	if len(activeNodes) > 0 {
		h.saveNodes() // CurrentTask/RunningTasks 变更置脏（全量保存一次）
	}
	h.mu.Unlock()
	// 3) 会话镜像/Todo 镜像全清（锁拆分：sessions 用 h.mu，sessTodos 用 h.sessMu）。
	h.mu.Lock()
	hadMsgs := len(h.sessions) > 0
	h.sessions = make(map[string][]json.RawMessage)
	h.mu.Unlock()
	h.sessMu.Lock()
	hadTodos := len(h.sessTodos) > 0
	h.sessTodos = make(map[string][]json.RawMessage)
	h.sessMu.Unlock()
	if hadMsgs {
		h.saveSessions()
	}
	if hadTodos {
		h.saveSessionTodos()
	}
	// 4) 锁外逐个转发取消 + 广播取消终态（与 CancelTask 单任务语义一致，
	// 让各端会话页/徽标立即收敛为「已取消」）。
	for _, t := range cancelList {
		h.Broadcast(TypeTaskResult, map[string]interface{}{
			"taskId": t.ID, "sessionId": t.SessionID, "ok": false,
			"error": "批量清空：任务已取消", "canceled": true,
		})
	}
	for nid := range nodeSeen {
		nodes = append(nodes, nid)
	}
	// 活动取消转发到在线节点（节点侧按会话停止执行）。
	for _, t := range cancelList {
		if err := h.RouteToAgent(t.NodeID, TypeServerTaskCancel, TaskCancel{TaskID: t.ID, SessionID: t.SessionID}); err != nil {
			slog.Debug("批量清空：取消转发节点失败（节点离线，已直接置 canceled）", "task", t.ID, "err", err)
		}
	}
	slog.Info("批量清空任务与会话完成", "canceled", canceled, "deleted", deleted, "nodes", len(nodeSeen))
	return canceled, deleted, nodes
}

// EnqueueChat 续聊消息离线暂存（P-Reliable）：节点不在线时 chat 不再丢弃，
// 存入节点 pending 暂存队列（复用任务补发链路），节点上线后经 dispatchLocked
// 自动补发为完整任务（text 非空即任务，服务重启 load() 恢复后仍补发）。
// 上线即时派发：在先等待与 CreateTask 同款派发路径（含写失败回退暂存）。
//
// 主动下线保护（agent.offline）：disabled=true 的节点直接拒绝续聊（返回错误
// "节点已下线，无法接收消息"），不再暂存与补发——区别于断线失联的暂存语义，
// 用户主动关开关/退出账号后，原有会话发消息不应再被节点执行。
func (h *Hub) EnqueueChat(nodeID string, cs ChatSend) error {
	text := strings.TrimSpace(cs.Text)
	if text == "" {
		return fmt.Errorf("消息内容为空")
	}
	h.mu.Lock()
	ns := h.nodes[nodeID]
	if ns != nil && ns.disabled {
		// 主动下线：拒绝续聊，不暂存不补发。
		h.mu.Unlock()
		return fmt.Errorf("节点已下线，无法接收消息")
	}
	if ns == nil {
		ns = &nodeState{}
		ns.info.ID = nodeID
		ns.info.FirstSeen = time.Now().UnixMilli()
		h.nodes[nodeID] = ns
	}
	if ns.conn != nil {
		// 在线（含读写竞态窗口内的刚上线）：直接路由。
		conn := ns.conn
		h.mu.Unlock()
		return conn.writeJSON(NewEnvelope("", TypeServerChat, cs))
	}
	// 离线：暂存为 pending 任务（续聊文本即任务文本），上线补发。
	createdBy := "chat-queued"
	t := &Task{
		ID:        newID("t"),
		SessionID: cs.SessionID,
		NodeID:    nodeID,
		Text:      text,
		Mode:      "",
		Kind:      TaskKindGeneral,
		Status:    TaskStatusPending,
		CreatedBy: createdBy,
		CreatedAt: time.Now().UnixMilli(),
	}
	h.tasks[t.ID] = t
	// 注意：此处绝不调用 h.SessionTaskID()（内部 h.mu.RLock）——本函数持 h.mu 写锁，
	// sync.RWMutex 不可重入，同 goroutine 写锁内再取读锁 = 永久自死锁（h.mu 冻结，
	// 线上事故形态：移动端 auth/health/节点注册全挂、nodes.json 停止更新、TCP accept
	// 仍工作）。原实现此处调用 SessionTaskID 且仅丢弃查询结果（零副作用），已整体
	// 移除；续聊路由/ask_reply 的会话→任务反查由调用方在锁外完成，语义不变。
	ns.pending = append(ns.pending, t)
	if len(ns.pending) > h.maxPendingPerNode {
		ns.pending = ns.pending[1:]
	}
	h.saveTasks()
	h.mu.Unlock()
	slog.Info("续聊消息离线暂存，节点上线后自动补发", "node", nodeID, "session", cs.SessionID, "task", t.ID)
	h.Broadcast(TypeTasks, h.ListTasks(100))
	return nil
}

// ---------- 会话历史（服务端镜像，供客户端拉取） ----------

// loadSessions 加载会话镜像历史。
// 锁拆分：h.sessions 由 h.mu 保护（与其余读写同锁）；h.sessTodos 由 h.sessMu。
func (h *Hub) loadSessions() {
	h.mu.Lock()
	if b, err := os.ReadFile(filepath.Join(h.dir, "sessions.json")); err == nil {
		var m map[string][]json.RawMessage
		if json.Unmarshal(b, &m) == nil && m != nil {
			h.sessions = m
		}
	}
	h.mu.Unlock()
	h.sessMu.Lock()
	defer h.sessMu.Unlock()
	// 会话任务清单镜像（session_todos.json 独立文件：旧版本无此文件时静默跳过，向后兼容）。
	if b, err := os.ReadFile(filepath.Join(h.dir, "session_todos.json")); err == nil {
		var m map[string][]json.RawMessage
		if json.Unmarshal(b, &m) == nil && m != nil {
			h.sessTodos = m
		}
	}
}

// saveSessions 置脏会话镜像持久化（调用点均已不在 sessMu 锁内；
// 真正落盘由 persistLoop 完成）。
func (h *Hub) saveSessions() { h.markPersist(&h.dirtySess) }

// SaveSessionMessages agent 回传历史落盘（覆盖式）。
func (h *Hub) SaveSessionMessages(sessionID string, msgs []interface{}) {
	// 序列化保留原始结构。
	raws := make([]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		raws = append(raws, b)
	}
	h.mu.Lock()
	h.sessions[sessionID] = raws
	h.mu.Unlock()
	h.saveSessions()
}

// SessionMessages 拉取会话历史。
func (h *Hub) SessionMessages(sessionID string) []json.RawMessage {
	h.mu.RLock()
	defer h.mu.RUnlock()
	raws := h.sessions[sessionID]
	out := make([]json.RawMessage, len(raws))
	copy(out, raws)
	return out
}

// SaveSessionTodos 会话任务清单镜像落盘（agent.messages 回传时调用，覆盖式；
// 空/缺省清单不动旧值：节点未开启 todo 回传时避免误清已有进度）。
func (h *Hub) SaveSessionTodos(sessionID string, todos []interface{}) {
	if len(todos) == 0 {
		return
	}
	raws := make([]json.RawMessage, 0, len(todos))
	for _, t := range todos {
		b, err := json.Marshal(t)
		if err != nil {
			continue
		}
		raws = append(raws, b)
	}
	h.sessMu.Lock()
	h.sessTodos[sessionID] = raws
	h.sessMu.Unlock()
	h.saveSessionTodos()
}

// SessionTodos 拉取会话任务清单镜像（get.messages 立即响应携带，
// 节点离线/服务重启后移动端仍可渲染 Todo 进度）。
func (h *Hub) SessionTodos(sessionID string) []json.RawMessage {
	h.sessMu.Lock()
	defer h.sessMu.Unlock()
	raws := h.sessTodos[sessionID]
	out := make([]json.RawMessage, len(raws))
	copy(out, raws)
	return out
}

// saveSessionTodos 置脏会话任务清单镜像持久化（session_todos.json）。
func (h *Hub) saveSessionTodos() { h.markPersist(&h.dirtyTodos) }

// ---------- 异步持久化：置脏 + 单写者落盘（防锁内 IO 死锁） ----------

// markPersist 置脏并非阻塞唤醒 persistLoop（任意锁内均可安全调用：
// 仅操作独立 persistMu 与容量 1 的唤醒通道，不触碰 h.mu/sessMu/磁盘）。
func (h *Hub) markPersist(flag *bool) {
	if h.dir == "" {
		return
	}
	h.persistMu.Lock()
	*flag = true
	h.persistMu.Unlock()
	select {
	case h.persistWake <- struct{}{}:
	default: // 已有待处理唤醒，合并
	}
}

// markPersistAll 全部文件置脏（启动首轮全量落盘用）。
func (h *Hub) markPersistAll() {
	if h.dir == "" {
		return
	}
	h.persistMu.Lock()
	h.dirtyCfgs, h.dirtyNodes, h.dirtyTasks, h.dirtySess, h.dirtyTodos = true, true, true, true, true
	h.persistMu.Unlock()
	select {
	case h.persistWake <- struct{}{}:
	default:
	}
}

// startPersistLoop 启动唯一落盘 goroutine（NewHub 内 dir 非空时调用一次）。
func (h *Hub) startPersistLoop() {
	h.persistWake = make(chan struct{}, 1)
	h.persistStop = make(chan struct{})
	h.persistDone = make(chan struct{})
	go h.persistLoop()
}

// persistLoop 唯一落盘者：醒来后取走全部脏标记，逐文件锁外快照 + 原子写。
// 退出前最终 flush（stop 关闭后仍补一轮），保证服务关闭不丢最后一批状态。
func (h *Hub) persistLoop() {
	defer close(h.persistDone)
	for {
		select {
		case <-h.persistWake:
			h.flushDirty()
			h.notifyFlushWait()
		case <-h.persistStop:
			h.flushDirty()
			h.notifyFlushWait()
			return
		}
	}
}

// notifyFlushWait 回发一轮 flush 完成信号（FlushPersistence 同步等待用；
// 取走后置空，避免旧信号被下一轮误用）。
func (h *Hub) notifyFlushWait() {
	h.persistSyncMu.Lock()
	done := h.persistSync
	h.persistSync = nil
	h.persistSyncMu.Unlock()
	if done != nil {
		done <- struct{}{}
	}
}

// flushDirty 取走当前全部脏标记并落盘对应文件（persistLoop 专用，锁外调用）。
func (h *Hub) flushDirty() {
	h.persistMu.Lock()
	dc, dn, dt, ds, dtd := h.dirtyCfgs, h.dirtyNodes, h.dirtyTasks, h.dirtySess, h.dirtyTodos
	h.dirtyCfgs, h.dirtyNodes, h.dirtyTasks, h.dirtySess, h.dirtyTodos = false, false, false, false, false
	h.persistMu.Unlock()
	_ = os.MkdirAll(h.dir, 0o755)
	if dc {
		h.writeNodeCfgsFile()
	}
	if dn {
		h.writeNodesFile()
	}
	if dt {
		h.writeTasksFile()
	}
	if ds {
		h.writeSessionsFile()
	}
	if dtd {
		h.writeSessionTodosFile()
	}
}

// FlushPersistence 同步等待已置脏数据全部落盘（测试/优雅退出辅助）。
// 实现：注册同步信号 → 请求一轮落盘 → 等待 persistLoop 完成回发 →
// 复查脏标记（若等待期间又有新置脏且未被本轮涵盖，再来一轮，有限次防御）。
// 不能用「无脏即返回」：flushDirty 取走标记后写入仍在进行，提前返回会
// 读到旧文件。Stop 后无后台落盘者 → 直接同步刷一轮。
func (h *Hub) FlushPersistence() {
	if h.dir == "" || h.persistStop == nil {
		return
	}
	for i := 0; i < 100; i++ {
		select {
		case <-h.persistStop: // 已停止：无后台落盘者，同步刷
			h.flushDirty()
			return
		default:
		}
		done := make(chan struct{}, 1)
		h.persistSyncMu.Lock()
		h.persistSync = done
		h.persistSyncMu.Unlock()
		select {
		case h.persistWake <- struct{}{}:
		default:
		}
		select {
		case <-done: // persistLoop 本轮 flush 完成后回发
		case <-h.persistStop:
			h.flushDirty()
			return
		}
		h.persistMu.Lock()
		dirty := h.dirtyCfgs || h.dirtyNodes || h.dirtyTasks || h.dirtySess || h.dirtyTodos
		h.persistMu.Unlock()
		if !dirty {
			return
		}
	}
}

// Stop 停止持久化 goroutine 与存储巡检并最终 flush（幂等；服务关闭时调用）。
func (h *Hub) Stop() {
	if stop := h.storePrunerStop; stop != nil {
		close(stop)
		h.storePrunerStop = nil
	}
	if h.persistStop == nil {
		return
	}
	h.persistOnce.Do(func() {
		close(h.persistStop)
		<-h.persistDone
	})
}

// writeNodeCfgsFile 节点配置快照落盘（含掩码 APIKey，仍限制权限 0600）。
func (h *Hub) writeNodeCfgsFile() {
	h.mu.RLock()
	b, err := json.MarshalIndent(h.nodeCfgs, "", "  ")
	h.mu.RUnlock()
	if err != nil {
		return
	}
	atomicWriteFile(h.nodeCfgsFile(), b, 0o600)
}

// writeNodesFile 节点表快照落盘（读锁仅包住快照拷贝，写盘在锁外）。
func (h *Hub) writeNodesFile() {
	h.mu.RLock()
	p := persistedNodes{Nodes: map[string]Node{}}
	for id, ns := range h.nodes {
		p.Nodes[id] = ns.info
	}
	b, err := json.MarshalIndent(p, "", "  ")
	h.mu.RUnlock()
	if err != nil {
		return
	}
	atomicWriteFile(h.nodesFile(), b, 0o644)
}

// writeTasksFile 任务表快照落盘（倒序、上限 1000 条防膨胀）。
func (h *Hub) writeTasksFile() {
	h.mu.RLock()
	list := h.taskList()
	if len(list) > 1000 {
		list = list[:1000]
	}
	b, err := json.MarshalIndent(list, "", "  ")
	h.mu.RUnlock()
	if err != nil {
		return
	}
	atomicWriteFile(h.tasksFile(), b, 0o644)
}

// writeSessionsFile 会话镜像快照落盘（h.mu 包住快照+序列化，写盘锁外）。
// 注意：h.sessions 的写入方（SaveSessionMessages/AddTaskEvent）均持 h.mu，
// 快照读取必须同锁，否则 map 并发读写数据竞争（曾致落盘只见部分会话）。
func (h *Hub) writeSessionsFile() {
	h.mu.RLock()
	b, err := json.MarshalIndent(h.sessions, "", "  ")
	h.mu.RUnlock()
	if err != nil {
		return
	}
	atomicWriteFile(filepath.Join(h.dir, "sessions.json"), b, 0o644)
}

// writeSessionTodosFile 会话任务清单镜像落盘（session_todos.json）。
func (h *Hub) writeSessionTodosFile() {
	h.sessMu.Lock()
	b, err := json.MarshalIndent(h.sessTodos, "", "  ")
	h.sessMu.Unlock()
	if err != nil {
		return
	}
	atomicWriteFile(filepath.Join(h.dir, "session_todos.json"), b, 0o644)
}

// atomicWriteFile 原子写：先写临时文件再 rename 覆盖，避免写入中途被读到
// 半截 JSON（原 os.WriteFile 直写存在断电/磁盘满截断风险）。
// Windows 兼容：rename 目标被占用（杀毒扫描/读句柄未放）时会报
// Access is denied / being used by another process，带指数退避重试，
// 仍失败则回退直写目标文件（数据不丢，仅牺牲一次原子性）。
func atomicWriteFile(path string, b []byte, perm os.FileMode) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		slog.Warn("持久化写临时文件失败，回退直写", "file", filepath.Base(path), "err", err)
		_ = os.WriteFile(path, b, perm)
		return
	}
	base := filepath.Base(path)
	for i := 0; ; i++ {
		err := os.Rename(tmp, path)
		if err == nil {
			return
		}
		if i < 4 {
			time.Sleep(time.Duration(10<<i) * time.Millisecond) // 10/20/40ms
			continue
		}
		slog.Warn("持久化原子替换失败，回退直写", "file", base, "err", err)
		if werr := os.WriteFile(path, b, perm); werr != nil {
			slog.Error("持久化直写也失败", "file", base, "err", werr)
		}
		_ = os.Remove(tmp)
		return
	}
}

// ListSessionSummaries 会话摘要（管理端/移动端会话列表）。
// 排序：置顶会话优先（Pinned 毫秒新→旧），其余按创建时间新→旧。
func (h *Hub) ListSessionSummaries() []map[string]interface{} {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]map[string]interface{}, 0)
	// 从任务推导会话（一个会话一个任务主键 + 续聊）。
	seen := map[string]bool{}
	// 会话级 pinned 投影：同会话多任务记录取最大 Pinned（pin.session 写入
	// 该会话全部任务，正常一致；这里聚合兑底，任一置顶即会话置顶）。
	pinnedAtBySess := map[string]int64{}
	for _, t := range h.tasks {
		if t.Pinned > pinnedAtBySess[t.SessionID] {
			pinnedAtBySess[t.SessionID] = t.Pinned
		}
	}
	list := h.taskList()
	for _, t := range list {
		if seen[t.SessionID] {
			continue
		}
		seen[t.SessionID] = true
		// 手动命名优先：PC 端右键改名的会话标题随 rename.session 落到 Task.Title，
		// 此后列表/详情均显示手动名称（空 = 回退任务首行，原行为不变）。
		title := t.Title
		if title == "" {
			title = firstLineText(t.Text, 40)
		}
		// pinned 投影（会话级）：同会话多任务记录取最大 Pinned（任一置顶即会话置顶）。
		pinned := pinnedAtBySess[t.SessionID] > 0
		out = append(out, map[string]interface{}{
			"sessionID": t.SessionID,
			"taskId":    t.ID,
			"nodeId":    t.NodeID,
			"title":     title,
			"status":    t.Status,
			"createdAt": t.CreatedAt,
			"pinned":    pinned,
			"pinnedAt":  pinnedAtBySess[t.SessionID],
		})
	}
	// 子会话投影追加（agent.subtask）：仅会话列表展示，带 parentId 供前端缩进；
	// 与主任务会话同列排序（createdAt 新→旧），置顶不生效（子会话无置顶操作）。
	for _, st := range h.subtasks {
		out = append(out, map[string]interface{}{
			"sessionID":    st.SessionID,
			"taskId":       "",
			"nodeId":       "",
			"title":        st.Title,
			"status":       st.Status,
			"createdAt":    st.TS,
			"parentId":     st.ParentID,
			"parentTaskId": st.ParentTaskID,
			"mode":         st.Mode,
			"result":       st.Result,
			"error":        st.Error,
			"subtask":      true,
		})
	}
	// 置顶优先排序：置顶会话排最前（按置顶时间新→旧），其余按创建时间新→旧。
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := out[i]["pinned"].(bool), out[j]["pinned"].(bool)
		if pi != pj {
			return pi
		}
		return out[i]["createdAt"].(int64) > out[j]["createdAt"].(int64)
	})
	return out
}

func firstLineText(s string, max int) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			s = s[:i]
			break
		}
	}
	r := []rune(s)
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}

// newID 简易 ID 生成（时间戳 + 随机后缀）。
// 注意：不能仅靠 UnixNano —— Windows 墙钟 tick 粒度可达 ~15ms，同一 tick 内
// 两次调用会生成相同 ID，导致并发创建的任务/会话互相覆盖（E2E
// TestE2E_DeleteTaskRecord 偶现两任务同 ID 复现）。追加进程内原子计数器 +
// 随机后缀，保证进程内严格唯一。
var idSeq atomic.Uint64

func newID(prefix string) string {
	seq := idSeq.Add(1)
	var rnd [2]byte
	_, _ = randRead(rnd[:])
	return fmt.Sprintf("%s-%d-%04d-%d-%02x%02x", prefix, time.Now().UnixNano(), os.Getpid()%10000, seq, rnd[0], rnd[1])
}

// ---------- A2A 群聊（group）----------
//
// 服务端只做路由/广播/投影缓存（节点权威），与 skills/roadmap 转发语义一致：
//   - group.create / group.msg / group.task 等来自移动端 → 路由到目标节点
//     （server.group_*）→ 节点执行 → 回传 agent.group_state / agent.group_msg
//     / agent.group_task_done → 服务端广播 groups/group.message/group.task_done。
//   - 节点上线/群状态变更 → agent.group_state 上报 → 服务端投影缓存 + 广播。

// StoreGroups 节点群列表投影缓存（agent.group_state 上报；同一节点覆盖写）。
func (h *Hub) StoreGroups(nodeID string, groups []Group) {
	h.mu.Lock()
	h.nodeGroups[nodeID] = groups
	h.mu.Unlock()
}

// GroupsOf 节点群列表投影（缓存读取；无则返回 nil）。
func (h *Hub) GroupsOf(nodeID string) []Group {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.nodeGroups[nodeID]
}

// AllGroups 全部节点的群列表合并投影（group.list 应答）。
func (h *Hub) AllGroups() []Group {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []Group
	for _, gs := range h.nodeGroups {
		out = append(out, gs...)
	}
	return out
}

// GroupNodeOf 群归属节点（按群 ID 反查投影）。
func (h *Hub) GroupNodeOf(groupID string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for nid, gs := range h.nodeGroups {
		for _, g := range gs {
			if g.ID == groupID {
				return nid, true
			}
		}
	}
	return "", false
}
