// Package remote kaelvoris 远程服务体系（PC 节点 ⇆ 服务端 ⇆ 移动端/管理端）：
//
//	架构对齐 plans/kaelvoris-architecture.md 渠道层设计（channel_lark/wecom 同款内核复用思路）：
//	  PC 端（Wails 桌面）作为「节点」通过 WebSocket 长连接注册到服务端；
//	  手机 H5 / 管理后台作为「客户端」连接服务端，查看在线节点与任务，
//	  发布新任务 / 续聊会话 / 审批 ask / 取消任务；
//	  服务端作为中心 hub 做注册、路由、持久化与事件广播。
//
//	协议：JSON 信封 over WebSocket（gorilla/websocket，复用 go.mod 既有依赖）：
//	  {"id":"可选关联ID","type":"agent.task_event","payload":{...},"ts":1690000000}
package remote

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// 消息类型常量（Type 常量命名 <方向>.<动作>）。
const (
	// ---- agent（PC 节点）→ server ----
	TypeAgentRegister     = "agent.register"      // 注册节点（连接后第一条）
	TypeAgentHeartbeat    = "agent.heartbeat"     // 心跳（含运行中任务数）
	TypeAgentTaskEvent    = "agent.task_event"    // 任务事件流（started/text/tool/error/ask/completion）
	TypeAgentTaskDone     = "agent.task_done"     // 任务终态（ok/result/error）
	TypeAgentMessages     = "agent.messages"      // get.messages 响应（会话历史回传）
	TypeAgentConfig       = "agent.config"        // 节点配置快照回传（set.config 确认 / get.config 响应）
	TypeAgentAnalyzeDone  = "agent.analyze_done"  // 会话 AI 分析结果回传（节点 LLM 生成）
	TypeAgentCleanupDone  = "agent.cleanup_done"  // 批量清理会话结果回传（节点分析+归档删除）
	TypeAgentVerifyReport = "agent.verify_report" // 任务状态核实回报（节点本地会话真实状态）
	TypeAgentImage        = "agent.image"         // 节点侧生成图片事件回传（generate_image → 上传服务端 → 广播 task.event kind=image）
	TypeAgentSubtask      = "agent.subtask"       // 子会话上报（new_task/spawn_agent 平行子会话：创建/结束，服务端会话列表投影用）
	TypeAgentOffline      = "agent.offline"       // 主动下线（关开关/退出账号时主动通知服务端：立即从节点列表移除并拒绝后续派发）

	// ---- server → agent ----
	TypeServerRegisterOK    = "server.register_ok"      // 注册确认
	TypeServerTaskDispatch  = "server.task_dispatch"    // 派发新任务
	TypeServerTaskCancel    = "server.task_cancel"      // 取消任务
	TypeServerChat          = "server.chat"             // 续聊/入队消息
	TypeServerAskReply      = "server.ask_reply"        // ask 审批应答
	TypeServerGetMessages   = "server.get_messages"     // 拉取会话历史
	TypeServerSetMode       = "server.set_mode"         // 切换节点执行模式
	TypeServerGetConfig     = "server.get_config"       // 拉取节点配置（远端「PC 端设置」）
	TypeServerSetConfig     = "server.set_config"       // 修改节点配置（远端下发，PC 端合并保存）
	TypeServerDeleteSession = "server.delete_session"   // 删除节点本地会话存档（会话删除联动 PC 端）
	TypeServerAnalyze       = "server.analyze_session"  // 会话 AI 分析（节点侧 LLM 生成报告）
	TypeServerCleanup       = "server.cleanup_sessions" // 批量清理会话（节点侧逐会话分析+归档删除；Mode 分流 archive/clear/summarize）
	TypeServerVerifyTasks   = "server.verify_tasks"     // 核实任务状态（远端发起，节点按本地会话真实状态回报）
	TypeServerClearSessions = "server.clear_sessions"   // 清空节点本地会话存档（clear.tasks 联动：服务端数据已清，节点清本地）

	// ---- server → agent（技能库）----
	TypeServerSkillsList   = "server.skills_list"   // 拉取节点技能列表（触发 agent.skills_list 回传）
	TypeServerSkillsGet    = "server.skills_get"    // 拉取技能全文（触发 agent.skills_data 回传）
	TypeServerSkillsSave   = "server.skills_save"   // 保存/新建技能（节点落 project 层）
	TypeServerSkillsDelete = "server.skills_delete" // 删除技能（builtin 禁删）

	// ---- agent（PC 节点）→ server（技能库）----
	TypeAgentSkillsList = "agent.skills_list" // 技能元数据全量回传（list 响应 + save/delete 后刷新）
	TypeAgentSkillsData = "agent.skills_data" // 技能全文回传（get 响应，ID 关联）

	// ---- client（手机/管理后台）→ server（技能库）----
	TypeSkillsList   = "skills.list"   // 拉取节点技能列表（缓存立即应答 + 在线节点刷新）
	TypeSkillsGet    = "skills.get"    // 拉取技能全文（靠 skills.item 回传）
	TypeSkillsSave   = "skills.save"   // 保存/新建技能（受理即回 op.result，真实结果由列表广播确认）
	TypeSkillsDelete = "skills.delete" // 删除技能（同上）

	// ---- client（手机/管理后台）→ server（数字员工）----
	TypeEmployeeList = "employee.list" // 拉取节点数字员工列表（群聊建群选成员数据源）

	// ---- agent（PC 节点）→ server（数字员工）----
	TypeAgentEmployeeList = "agent.employee_list" // 员工元数据全量回传（list 响应）

	// ---- server → agent（数字员工）----
	TypeServerEmployeeList = "server.employee_list" // 拉取节点员工列表（触发 agent.employee_list 回传）

	// ---- client（手机/管理后台）→ server ----
	TypeAuth            = "auth"             // 认证（token 或 用户名+密码）
	TypeListNodes       = "list.nodes"       // 拉取节点列表
	TypeListTasks       = "list.tasks"       // 拉取任务列表
	TypeCreateTask      = "create.task"      // 发布新任务
	TypeChatSend        = "chat.send"        // 会话续聊（运行中入队 / 空闲新轮次）
	TypeTaskCancel      = "task.cancel"      // 取消任务
	TypeGetMessages     = "get.messages"     // 拉取会话消息
	TypeAskReply        = "ask.reply"        // 审批应答
	TypeSetMode         = "set.mode"         // 切换节点执行模式（PC 端执行，远端仅指令）
	TypeDeleteTask      = "delete.task"      // 删除任务记录（仅终态；连带清会话镜像/事件）
	TypeDeleteSession   = "delete.session"   // 删除会话（移动端；服务端记录+镜像+节点本地存档一并清除）
	TypeAnalyzeSession  = "analyze.session"  // 会话 AI 分析（节点侧 LLM 生成报告）
	TypeCleanupSessions = "cleanup.sessions" // 批量清理会话（移动端；路由节点：分析→文档→归档删除）
	TypeVerifyTasks     = "verify.tasks"     // 核实任务状态（移动端发起：节点本地会话真实状态核对服务端任务）
	TypeClearTasks      = "clear.tasks"      // 清空全部任务与会话（移动端；Mode=clear 直接清空 / summarize 总结并清理）
	TypeRenameSession   = "rename.session"   // 会话重命名（PC 端会话卡右键；服务端写任务 Title + 广播全端同步）
	TypeGetConfig       = "get.config"       // 读取节点配置（nodeId 必填；走节点实时快照）
	TypeSetConfig       = "set.config"       // 修改节点配置（部分字段更新；APIKey 非空才覆盖）
	TypeClientPing      = "client.ping"      // 心跳保活（H5/管理端定期发送，服务端回 pong 判活）
	TypePinSession      = "pin.session"      // 会话置顶/取消置顶（移动端长按菜单；服务端写任务 Pinned + 广播全端同步）
	TypeAccountPasswd   = "account.passwd"   // 修改密码（自己改密需验旧密码；admin 可重置他人；宿主持久化 accounts.json）
	TypeUploadImage     = "upload.image"     // 节点上传生成图片（multipart binary；agentToken 鉴权）

	// ---- server → client ----
	TypeAuthOK      = "auth.ok"      // 认证成功 {role}
	TypeAuthErr     = "auth.err"     // 认证失败 {message}
	TypeNodes       = "nodes"        // 节点列表推送
	TypeTasks       = "tasks"        // 任务列表推送
	TypeTaskCreated = "task.created" // 任务已受理 {task}
	TypeTaskEvent   = "task.event"   // 任务事件（转发 agent.task_event）
	TypeTaskResult  = "task.result"  // 任务终态 {taskID, ok, result, error}
	TypeMessages    = "messages"     // 会话历史 {sessionID, messages}
	TypeOpResult    = "op.result"    // 通用操作结果（请求-响应关联 id）
	TypeNodeConfig  = "node.config"  // 节点配置（get.config 应答 / agent.config 广播）
	TypeSkills      = "skills"       // 节点技能列表（缓存立即应答 / agent.skills_list 广播）
	TypeSkillsItem  = "skills.item"  // 单技能全文（skills.get 响应，ID 关联请求）
	TypeAnalyzeDone = "analyze.done" // 会话分析完成 {sessionID, analysis}
	TypeCleanupDone = "cleanup.done" // 批量清理完成 {nodeId, deleted, skipped, total, reports}
	TypeClearDone   = "clear.done"   // 清空完成 {mode, canceled, deleted, newTaskId, newSessionId}（清空/总结并清理共用）
	TypeVerifyDone  = "verify.done"  // 任务核实完成 {checked, fixed, detail}（修正后附全量 tasks 广播）
	TypeServerError = "server.error" // 服务端错误 {message}
	TypeClientPong  = "client.pong"  // 心跳应答 {ts}（客户端判活；旧服务端不识别会回 op.result 错误，可据此判假活）

	// 上行可靠投递（P-Reliable：PC 端消息不丢失）。
	// 语义：agent 帧带 MsgID → 服务端 worker 线程处理完成后回 server.ack(msgId)；
	// 处理失败/未知类型回 server.send_failed(msgId)；连接中断时未确认帧由 PC 端
	// outbox 重连补发，服务端按 msgId 去重（幂等）。旧服务端不识别 MsgID（无 ack）
	// → PC 端重连后重发同一 MsgID，服务端无去重则容忍重放（事件写入覆盖式，安全）。
	TypeServerAck        = "server.ack"         // 上行确认 {msgId}（处理完成）
	TypeServerSendFailed = "server.send_failed" // 上行失败 {msgId, error}（PC 端立即重发）
)

var (
	envelopeSeq     atomic.Uint64
	envelopeStartMS = time.Now().UnixMilli() // 进程启动毫秒（重启后整体偏移更大，不冲突）
)

// MsgID 跨进程单调递增的上行消息 ID（去重/补发关联键）：
// 时间戳段（进程启动毫秒，重启后整体偏移更大）+ 进程内序号 —— 同一 PC 端进程
// 内唯一，重启后不会与旧进程遗留 ID 冲突。
func NewMsgID() string {
	return fmt.Sprintf("m%d-%04d", envelopeStartMS, envelopeSeq.Add(1))
}

// Envelope WebSocket 消息信封。
type Envelope struct {
	ID      string          `json:"id,omitempty"`      // 请求关联 ID（请求-响应配对）
	Type    string          `json:"type"`              // 消息类型（Type* 常量）
	Payload json.RawMessage `json:"payload,omitempty"` // 载荷（各类型结构体）
	TS      int64           `json:"ts"`                // 毫秒时间戳
	MsgID   string          `json:"msgId,omitempty"`   // 上行可靠投递消息 ID（agent 帧可选；服务端 ack/去重用）
}

// NewEnvelope 构造信封（payload 序列化失败时忽略载荷）。
func NewEnvelope(id, typ string, payload interface{}) Envelope {
	e := Envelope{ID: id, Type: typ, TS: time.Now().UnixMilli()}
	if payload != nil {
		if b, err := json.Marshal(payload); err == nil {
			e.Payload = b
		}
	}
	return e
}

// Decode 解析载荷到 out（out 为 nil 或空载荷时跳过）。
func (e Envelope) Decode(out interface{}) error {
	if len(e.Payload) == 0 || out == nil {
		return nil
	}
	return json.Unmarshal(e.Payload, out)
}

// ---------- 节点 ----------

// Node 已注册的 PC 节点。
type Node struct {
	ID           string     `json:"id"`                 // 节点 ID（token 关联 / 稳定标识）
	Name         string     `json:"name"`               // 显示名（默认机器名）
	Owner        string     `json:"owner,omitempty"`    // 归属账号（node.token 开通者；账号隔离展示）
	Platform     string     `json:"platform,omitempty"` // windows/darwin/linux
	Arch         string     `json:"arch,omitempty"`
	Version      string     `json:"version,omitempty"` // 桌面端版本号
	Hostname     string     `json:"hostname,omitempty"`
	Online       bool       `json:"online"`
	CurrentTask  string     `json:"currentTask,omitempty"`  // 运行中任务 ID（并发派发下为最近派发任务的徽标）
	RunningTasks int        `json:"runningTasks,omitempty"` // 节点并发运行中任务数（心跳回传，服务端各状态迁移点同步增减）
	Mode         string     `json:"mode,omitempty"`         // 节点当前执行模式（PC 端唯一执行面）
	Modes        []ModeInfo `json:"modes,omitempty"`        // 节点支持的执行模式（远端发布任务时可指定）
	FirstSeen    int64      `json:"firstSeen"`
	LastSeen     int64      `json:"lastSeen"`
}

// ModeInfo 执行模式摘要（PC 端 modes.Manager 转换）。
type ModeInfo struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// AgentRegister agent.register 载荷（mode/modes：PC 端执行模式与可选清单，远端展示/切换用）。
// User/Pass 为 PC 端登录账号凭据（P-Account，去令牌化 2026-10）：服务端经统一账号表
// （AuthAgentForRegister，与 /ws/client auth 同一 accounts.json 验证源）校验后绑定
// 节点归属（Node.Owner），客户端指挥权按账号隔离（相同账号才能指挥）。
// User/Pass 均空 = 匿名注册：节点可上线展示但不可被指挥。
type AgentRegister struct {
	NodeID   string     `json:"nodeId"`
	Name     string     `json:"name"`
	Platform string     `json:"platform"`
	Arch     string     `json:"arch"`
	Version  string     `json:"version"`
	Hostname string     `json:"hostname"`
	User     string     `json:"user,omitempty"` // 登录账号（空 = 匿名；指挥权归属依据）
	Pass     string     `json:"pass,omitempty"` // 登录密码（与统一账号同源验证）
	Mode     string     `json:"mode,omitempty"`
	Modes    []ModeInfo `json:"modes,omitempty"`
}

// Heartbeat agent.heartbeat 载荷。
type Heartbeat struct {
	RunningTasks int `json:"runningTasks"`
}

// ---------- 任务 ----------

// 任务状态。
const (
	TaskStatusPending  = "pending"  // 已受理待派发（目标节点离线时暂存）
	TaskStatusRunning  = "running"  // 执行中
	TaskStatusDone     = "done"     // 完成
	TaskStatusFailed   = "failed"   // 失败
	TaskStatusCanceled = "canceled" // 已取消
)

// 任务类别（kind）：general = 普通远程任务（默认）；paint = 绘画作品（移动端绘画 tab 数据源）。
const (
	TaskKindGeneral = "general"
	TaskKindPaint   = "paint"
)

// Task 一条远程任务（任务 = 一次远程会话执行；续聊挂同一会话）。
// kind=paint 时任务即「绘画作品」：Text=画面描述（提示词）、Spot=景点/主题、
// Images=生成结果（服务端 /img/ 相对 URL 列表，随图片事件与终态回填、随任务持久化）。
// Kind 缺省按 general 解读（旧服务端数据/旧客户端兼容）。
// Title 手动命名（PC 端右键改名写入）：非空时会话列表/聊天页标题优先用它，
// 空 = 回退任务文本首行（firstLineText）；rename.session 广播后全端同步。
// Pinned 置顶标记（pin.session 写入，会话级生效）：非零时会话卡排列表最前，
// 列表侧 pinned 投影取该会话任一任务的最大值（同会话多任务记录）。
type Task struct {
	ID         string   `json:"id"`
	SessionID  string   `json:"sessionId"` // agent 侧会话 ID（remote-<taskID>）
	NodeID     string   `json:"nodeId"`
	Text       string   `json:"text"`               // 任务文本（paint = 画面描述）
	Title      string   `json:"title,omitempty"`    // 手动命名（会话显示名；空 = 自动推导首行）
	Pinned     int64    `json:"pinnedAt,omitempty"` // 置顶标记（毫秒时间戳；0 = 未置顶；非零 = 置顶顺序依据）
	Mode       string   `json:"mode,omitempty"`     // 执行模式（PC 端切换后执行）
	Kind       string   `json:"kind,omitempty"`     // 任务类别：general（默认）/ paint
	Spot       string   `json:"spot,omitempty"`     // paint 专属：景点/主题分组标签
	Status     string   `json:"status"`
	Result     string   `json:"result,omitempty"`
	Error      string   `json:"error,omitempty"`
	Images     []string `json:"images,omitempty"`    // 生成图片 URL 列表（/img/<file>；paint 任务终态回填）
	CreatedBy  string   `json:"createdBy,omitempty"` // 发布来源（mobile:<user> / admin:<user>）
	CreatedAt  int64    `json:"createdAt"`
	StartedAt  int64    `json:"startedAt,omitempty"`
	FinishedAt int64    `json:"finishedAt,omitempty"`
}

// TaskFilter 任务过滤条件（list.tasks / GET /api/tasks 共用）。
// 零值字段 = 不过滤（向后兼容：旧行为 ListTasks 等价于 QueryTasks{Limit: n}）。
type TaskFilter struct {
	NodeID string `json:"nodeId,omitempty"` // 按节点过滤（精确匹配）
	Status string `json:"status,omitempty"` // 按状态过滤（pending/running/done/failed/canceled）
	Search string `json:"search,omitempty"` // 文本包含匹配（任务文本/发布者，大小写不敏感）
	Since  int64  `json:"since,omitempty"`  // 只返回 createdAt >= since（毫秒）
	Limit  int    `json:"limit,omitempty"`  // 最多返回条数（0 = 默认 100，上限 1000）
}

// Validate 规范化过滤参数：状态白名单校验、Limit 夹取，非法参数返回错误。
func (f *TaskFilter) Validate() error {
	if f.Status != "" {
		switch f.Status {
		case TaskStatusPending, TaskStatusRunning, TaskStatusDone, TaskStatusFailed, TaskStatusCanceled:
		default:
			return fmt.Errorf("无效的任务状态: %s", f.Status)
		}
	}
	if f.Limit <= 0 {
		f.Limit = 100
	}
	if f.Limit > 1000 {
		f.Limit = 1000
	}
	return nil
}

// Matches 任务是否命中过滤条件（调用方保证 f 已 Validate）。
func (f *TaskFilter) Matches(t *Task) bool {
	if f == nil {
		return true
	}
	if f.NodeID != "" && t.NodeID != f.NodeID {
		return false
	}
	if f.Status != "" && t.Status != f.Status {
		return false
	}
	if f.Since > 0 && t.CreatedAt < f.Since {
		return false
	}
	if f.Search != "" {
		needle := strings.ToLower(f.Search)
		if !strings.Contains(strings.ToLower(t.Text), needle) &&
			!strings.Contains(strings.ToLower(t.CreatedBy), needle) {
			return false
		}
	}
	return true
}

// TaskEvent 任务事件（agent.task_event → 广播 task.event）。
// Kind=image 时 Text 携带 JSON 载荷 ImageEventPayload（图片 URL/提示词/景点），
// 服务端按会话回填 Task.Images 并随事件原样广播（移动端聊天流/绘画列表双渲染）。
type TaskEvent struct {
	TaskID    string `json:"taskId"`
	SessionID string `json:"sessionId"`
	// NodeID 归属节点（服务端 AddTaskEvent 广播前按 taskID 反查填充；
	// PC 端客户端据此过滤：仅渲染本机执行/本机发起的任务，不渲染他机广播）。
	NodeID  string   `json:"nodeId,omitempty"`
	Kind    string   `json:"kind"` // started/text/tool/error/ask/completion/image
	Text    string   `json:"text,omitempty"`
	Partial bool     `json:"partial,omitempty"`
	Images  []string `json:"images,omitempty"` // image 事件：图片 URL 列表（/img/<file>）
	TS      int64    `json:"ts"`
}

// ImageEventPayload task.event(kind=image) 的 Text JSON 载荷（移动端解析渲染）。
type ImageEventPayload struct {
	Images []string `json:"images"`           // 图片 URL 列表（相对 /img/<file>）
	Prompt string   `json:"prompt,omitempty"` // 生成提示词（绘画卡展示）
	Spot   string   `json:"spot,omitempty"`   // 景点/主题（绘画卡分组）
}

// TaskDone agent.task_done 载荷。
// Outbox 非空 = PC 端 outbox 补发（重连后重放未确认终态）：服务端仅当本地任务
// 仍处于 pending/running 活动态时才接受校正（终态幂等保护不变：不覆盖
// done/failed/canceled —— 手机端 ABORT 后迟到的正常终态不得复活已取消任务）；
// 且仅当校正值与当前状态不同才落盘+广播（节点断线期间被 watchdog 收敛为
// failed 的任务回传真实终态 → 就地校正，任务卡不再永远卡失败/运行中）。
type TaskDone struct {
	TaskID string `json:"taskId"`
	OK     bool   `json:"ok"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
	// Outbox=true：本帧来自 PC 端离线补发（断线期间执行完成/失败的迟到终态）。
	// 服务端处理语义差异见上；运行中任务不覆盖（节点重连后 running 语义仍有效）。
	Outbox bool `json:"outbox,omitempty"`
}

// reapErrorMark 服务端收敛断线任务的错误文案标记（节点回放判断用）。
const reapErrorMark = "节点连接中断"

// SubtaskEvent agent.subtask 载荷（子会话生命周期上报）。
// 节点侧 new_task / spawn_agent 委派创建平行子会话（ID 形态 <父ID>-s<N>）时
// 上报：start 帧标记开始（status=running），end 帧标记结束（status=done/failed
// 或 canceled）。服务端仅以此维护会话列表投影（h.subtasks），不纳入任务派发链；
// 子会话对移动端/管理端只读展示（会话列表带 parentId 缩进），不可发布/取消/续聊。
type SubtaskEvent struct {
	SessionID    string `json:"sessionId"`              // 子会话 ID（<父ID>-s<N>）
	ParentID     string `json:"parentId,omitempty"`     // 父会话 ID
	ParentTaskID string `json:"parentTaskId,omitempty"` // 父会话关联的服务端任务 ID（父任务为远程任务时；空 = 本地会话）
	Title        string `json:"title,omitempty"`        // 子会话标题（已含 ↳ 前缀）
	Status       string `json:"status"`                 // running / done / failed / canceled（对齐 TaskStatus*）
	Result       string `json:"result,omitempty"`       // end 帧：完成结果摘要
	Error        string `json:"error,omitempty"`        // end 帧：失败原因
	Mode         string `json:"mode,omitempty"`         // 子会话执行模式（列表徽标展示）
	TS           int64  `json:"ts"`                     // 毫秒时间戳
}

// VerifyTaskState 节点回报的单任务真实状态（以节点本地会话档案为权威事实）。
type VerifyTaskState struct {
	TaskID string `json:"taskId"`
	Status string `json:"status"`           // running/done/failed/canceled（节点能判定的子集）
	Result string `json:"result,omitempty"` // done：完成结果摘要（截断）
	Error  string `json:"error,omitempty"`  // failed：失败原因
}

// VerifyReport agent.verify_report 载荷（节点对本节点待核实任务的逐条真实状态回报）。
// RequestedBy 为服务端发起 server.verify_tasks 时携带的发起端标记，节点
// 原样透传回服务端（仅用于 verify.done 广播时定位发起端提示文案）。
type VerifyReport struct {
	NodeID      string            `json:"nodeId,omitempty"`
	Items       []VerifyTaskState `json:"items"`
	RequestedBy string            `json:"requestedBy,omitempty"`
}

// VerifyDone verify.done 载荷（服务端核实完毕广播：移动端提示/刷新用）。
// RequestedBy 为发起端标记（服务端透传自 server.verify_tasks 载荷），
// 移动端据此区分「本端发起的核实」与「其他端/服务端巡检发起的核实」。
type VerifyDone struct {
	OK          bool   `json:"ok"`
	Checked     int    `json:"checked"`           // 节点回报并核对的任务数
	Fixed       int    `json:"fixed"`             // 修正状态的任务数
	Detail      string `json:"detail,omitempty"`  // 修正明细（逐条 "t-xx running→done"，换行分隔）
	Offline     int    `json:"offline,omitempty"` // 离线节点上无法核实的 pending/running 任务数
	RequestedBy string `json:"requestedBy,omitempty"`
	Error       string `json:"error,omitempty"`
}

// ---------- 派发与续聊 ----------

// TaskDispatch server.task_dispatch 载荷（Kind/Spot 随任务透传节点，节点据此在
// 提示词注入生图指令；Kind 缺省 = general，旧服务端不携带时行为不变）。
type TaskDispatch struct {
	TaskID    string   `json:"taskId"`
	SessionID string   `json:"sessionId"`
	Text      string   `json:"text"`
	Mode      string   `json:"mode,omitempty"` // 执行模式（发布时指定；PC 端派发时切换）
	Kind      string   `json:"kind,omitempty"` // 任务类别：general / paint
	Spot      string   `json:"spot,omitempty"` // paint：景点/主题标签
	Images    []string `json:"images,omitempty"`
}

// ChatSend chat.send / server.chat 载荷。
type ChatSend struct {
	NodeID    string   `json:"nodeId,omitempty"` // client→server 方向需要
	SessionID string   `json:"sessionId"`
	TaskID    string   `json:"taskId,omitempty"`
	Text      string   `json:"text"`
	Images    []string `json:"images,omitempty"`
}

// AskReply ask.reply / server.ask_reply 载荷（response: yes/no/messageResponse）。
type AskReply struct {
	SessionID string `json:"sessionId"`
	Response  string `json:"response"`
	Text      string `json:"text,omitempty"`
}

// GetMessages get.messages / server.get_messages 载荷。
type GetMessages struct {
	NodeID    string `json:"nodeId,omitempty"`
	SessionID string `json:"sessionId"`
}

// MessagesResponse messages / agent.messages 载荷（todos = 会话任务清单，随历史回传）。
type MessagesResponse struct {
	SessionID string        `json:"sessionID"`
	Messages  []interface{} `json:"messages"`
	Todos     []interface{} `json:"todos,omitempty"`
}

// ---------- 认证与通用 ----------

// Auth auth 载荷：token（设备/节点令牌）或 user+pass（统一账号登录，PC/移动互通）。
type Auth struct {
	Token string `json:"token,omitempty"`
	User  string `json:"user,omitempty"`
	Pass  string `json:"pass,omitempty"`
}

// AuthOK auth.ok 载荷。
type AuthOK struct {
	Role        string `json:"role"` // admin | client
	User        string `json:"user,omitempty"`
	DisplayName string `json:"displayName,omitempty"` // 统一账号显示名
}

// Account 统一账号（PC/移动/管理端同一账号体系；role 决定管理权限）。
type Account struct {
	User        string `json:"user"`
	Pass        string `json:"pass"`
	Role        string `json:"role"` // admin | user
	DisplayName string `json:"displayName,omitempty"`
}

// SetMode set.mode / server.set_mode 载荷（远端切换 PC 节点执行模式）。
type SetMode struct {
	NodeID string `json:"nodeId,omitempty"` // 空 = 自动（当前会话归属节点）
	Mode   string `json:"mode"`
}

// AccountPasswd account.passwd 载荷（管理端/移动端修改密码）。
// Self 改密必须验 OldPass；admin 重置他人时 OldPass 忽略（不要求知道旧密码）。
type AccountPasswd struct {
	User    string `json:"user,omitempty"`    // 目标账号（空 = 当前登录账号自己）
	OldPass string `json:"oldPass,omitempty"` // 旧密码（改自己的密码必填）
	NewPass string `json:"newPass"`           // 新密码（4~64 字符）
}

// AccountPasswdOK account.passwd_ok 应答载荷。
type AccountPasswdOK struct {
	User    string `json:"user"` // 实际修改的账号
	Self    bool   `json:"self"` // 是否本人修改（true = 已同时刷新本地凭据提示）
	Message string `json:"message,omitempty"`
}

// NodeConfig 节点配置投影（桌面端 desktop.json Config 的远端可见子集）。
// 对齐桌面端「通用设置」页字段：API Base URL / 模型 / API Key / 自动批准 /
// 上下文窗口；Workspace/RecentProjects 等本机路径字段不同步（无远端操作意义，
// 且避免泄漏目录结构）。APIKey 出站统一掩码（MaskAPIKey），入站空值 = 不修改。
type NodeConfig struct {
	BaseURL             string `json:"baseURL"`
	Model               string `json:"model"`
	APIKey              string `json:"apiKey,omitempty"` // 出站掩码（sk-****abcd）；set.config 时非空 = 覆盖
	AutoApprove         bool   `json:"autoApprove"`
	ContextWindow       int    `json:"contextWindow,omitempty"`
	AutoCondensePercent int    `json:"autoCondensePercent,omitempty"`
	// 媒体生成端点（generate_image / generate_video / generate_speech 工具；
	// 移动端/管理端「节点设置」可直接配置）。
	ImageGenBaseURL string `json:"imageGenBaseURL,omitempty"`
	ImageGenModel   string `json:"imageGenModel,omitempty"`
	ImageGenAPIKey  string `json:"imageGenAPIKey,omitempty"` // 出站掩码；set.config 非空才覆盖
	VideoGenBaseURL string `json:"videoGenBaseURL,omitempty"`
	VideoGenModel   string `json:"videoGenModel,omitempty"`
	VideoGenAPIKey  string `json:"videoGenAPIKey,omitempty"` // 出站掩码；set.config 非空才覆盖
	SpeechBaseURL   string `json:"speechBaseURL,omitempty"`
	SpeechModel     string `json:"speechModel,omitempty"`
	SpeechAPIKey    string `json:"speechAPIKey,omitempty"` // 出站掩码；set.config 非空才覆盖
	SpeechVoice     string `json:"speechVoice,omitempty"`
}

// GetConfig get.config / server.get_config 载荷（NodeID 必填：远端按节点定向拉取）。
type GetConfig struct {
	NodeID string `json:"nodeId"`
}

// SetConfig set.config / server.set_config 载荷（部分更新语义：
// 指针字段区分「未下发」与「显式置空」；APIKey 非空才覆盖）。
type SetConfig struct {
	NodeID              string  `json:"nodeId"`
	BaseURL             *string `json:"baseURL,omitempty"`
	Model               *string `json:"model,omitempty"`
	APIKey              string  `json:"apiKey,omitempty"` // 空 = 保持不变（远端只见掩码）
	AutoApprove         *bool   `json:"autoApprove,omitempty"`
	ContextWindow       *int    `json:"contextWindow,omitempty"`
	AutoCondensePercent *int    `json:"autoCondensePercent,omitempty"`
	// 媒体生成端点（部分更新语义同上；APIKey 非空才覆盖，掩码形态 = 未修改）。
	ImageGenBaseURL *string `json:"imageGenBaseURL,omitempty"`
	ImageGenModel   *string `json:"imageGenModel,omitempty"`
	ImageGenAPIKey  string  `json:"imageGenAPIKey,omitempty"`
	VideoGenBaseURL *string `json:"videoGenBaseURL,omitempty"`
	VideoGenModel   *string `json:"videoGenModel,omitempty"`
	VideoGenAPIKey  string  `json:"videoGenAPIKey,omitempty"`
	SpeechBaseURL   *string `json:"speechBaseURL,omitempty"`
	SpeechModel     *string `json:"speechModel,omitempty"`
	SpeechAPIKey    string  `json:"speechAPIKey,omitempty"`
	SpeechVoice     *string `json:"speechVoice,omitempty"`
}

// ErrorPayload server.error / auth.err 载荷。
type ErrorPayload struct {
	Message string `json:"message"`
}

// AckPayload server.ack 载荷：上行消息处理完成确认（P-Reliable；PC 端据此
// 从 outbox 删除已投递消息）。
type AckPayload struct {
	MsgID string `json:"msgId"`
}

// RegisterOKPayload server.register_ok 载荷：注册确认 + 服务端裁定节点身份。
// NodeID 为裁定后的生效 ID（自报 ID 与令牌不同族时回落基础 ID）；
// BaseNodeID 为令牌绑定的基础 ID（供 PC 端对齐派生规则，消除 WARN 刷屏：
// 桌面端首次学习后把自生成 NodeID 改写为令牌家族基础 ID，此后派生
// "基础ID-槽位号" 与服务端同族，不再触发回落 WARN）。旧服务端无此字段
// （空值），PC 端保持自报身份不变（向后兼容）。
type RegisterOKPayload struct {
	NodeID        string `json:"nodeId"`
	BaseNodeID    string `json:"baseNodeId,omitempty"`
	ServerVersion string `json:"serverVersion,omitempty"`
}

// SendFailedPayload server.send_failed 载荷：上行消息处理失败（PC 端立即重发；
// 重连时服务端也会补发未确认帧的失败回执，PC 端据此重发）。
type SendFailedPayload struct {
	MsgID string `json:"msgId"`
	Error string `json:"error,omitempty"`
}

// OpResult op.result 载荷（请求-响应通用确认）。
type OpResult struct {
	OK      bool        `json:"ok"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

// CreateTask create.task 载荷（Kind=paint = 绘画任务：移动端绘画 tab 发布，
// 服务端落 Kind/Spot 并在派发前注入生图指令；旧客户端不带 Kind 行为不变）。
type CreateTask struct {
	NodeID string `json:"nodeId,omitempty"` // 空 = 自动选择在线节点
	Text   string `json:"text"`
	Mode   string `json:"mode,omitempty"` // 执行模式（PC 端执行前切换；空 = 节点当前模式）
	Kind   string `json:"kind,omitempty"` // 任务类别：general（默认）/ paint
	Spot   string `json:"spot,omitempty"` // paint：景点/主题分组标签
}

// DeleteTask delete.task 载荷（删除任务记录，仅终态任务可删）。
type DeleteTask struct {
	TaskID string `json:"taskId"`
}

// DeleteSession delete.session / server.delete_session 载荷。
// 会话删除联动：服务端删任务记录/镜像/事件（按 sessionID 反查全部任务）+ 广播，
// 同时路由到归属节点清除本地会话存档（~/.kaelvoris/sessions/<id>.json），
// 避免 PC 端会话列表/回放仍残留已删会话。
type DeleteSession struct {
	SessionID string `json:"sessionId"`
}

// RenameSession rename.session 载荷（会话重命名）。
// PC 端会话卡右键改名 → 上行本消息；服务端把手动名称写入该会话全部任务
// 记录的 Title（移动端/管理端标题数据源），落盘 + 广播 tasks/sessions。
// 名称一经写入不再被自动命名覆盖（显示侧优先 t.Title，回退任务首行）。
type RenameSession struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
}

// PinSession pin.session 载荷（会话置顶/取消置顶）。
// 移动端会话卡长按菜单（或左滑按钮）→ 上行本消息；服务端把 Pinned 时间戳写入
// 该会话全部任务记录，落盘 + 广播 tasks/sessions；各端列表按 pinned 优先排序。
// Pinned=false 时字段零值下发（Go 端 Unmarshal 后 Pinned=false → 服务端写 0 取消置顶）。
type PinSession struct {
	SessionID string `json:"sessionId"`
	Pinned    bool   `json:"pinned"`
}

// ClearAll clear.tasks 载荷（移动端「清空任务与会话」入口）。
// Mode 空/"clear" = 直接清空全部；"summarize" = 节点先把未完成任务汇总为
// 续接任务再清空（服务端收尾时自动创建新会话任务并派发运行）。
type ClearAll struct {
	Mode string `json:"mode,omitempty"`
}

// CleanupAll server.cleanup_sessions 载荷（清理模式透传节点）：
//   - "archive"（空）：逐会话「AI 分析→报告落盘→归档删除」（原 cleanup.sessions 行为）；
//   - "clear"：直接清空本地会话存档（配合移动端「清空任务与会话」；不分析不保留）；
//   - "summarize"：先汇总未完成任务为续接任务（LLM），回传 CleanupDone{Mode,
//     Summary} 后由服务端创建新会话任务，再统一清空两端记录。
type CleanupAll struct {
	Mode string `json:"mode,omitempty"`
}

// AnalyzeSession analyze.session / server.analyze_session 载荷。
// 节点侧拉取整个会话历史（含 todos/记忆文档）调 LLM 生成分析报告：
// 问题理解 → 执行过程 → 结果评价 → 后续建议。结果经 analyze.done 广播回传，
// 同时落节点本地会话历史（移动端下次拉历史仍可见）。
type AnalyzeSession struct {
	SessionID string `json:"sessionId"`
}

// AnalyzeDone analyze.done 载荷（AI 分析结果回传）。
type AnalyzeDone struct {
	SessionID string `json:"sessionId"`
	TaskID    string `json:"taskId,omitempty"`
	OK        bool   `json:"ok"`
	Analysis  string `json:"analysis,omitempty"`
	Error     string `json:"error,omitempty"`
}

// CleanupSessionReport 单会话清理结果（批量清理回传逐项明细）。
type CleanupSessionReport struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title,omitempty"`
	OK        bool   `json:"ok"`      // 分析+归档删除全链路成功
	Skipped   bool   `json:"skipped"` // 跳过（运行中/无历史/分析失败）
	Reason    string `json:"reason,omitempty"`
	DocPath   string `json:"docPath,omitempty"`  // 分析文档落盘路径（节点工作区相对）
	Analysis  string `json:"analysis,omitempty"` // 分析报告预览
}

// CleanupDone cleanup.done 载荷（批量清理结果广播：移动端结果卡片渲染）。
// Mode 对齐 server.cleanup_sessions 的清理模式（空 = archive 逐会话分析归档，
// 旧服务端/旧节点不带此字段行为不变）；summarize 模式额外携带 Summary
// （未完成任务汇总文本，服务端据此创建续接任务后广播）。
type CleanupDone struct {
	NodeID  string                 `json:"nodeId,omitempty"`
	Mode    string                 `json:"mode,omitempty"` // archive（空）/ clear / summarize
	Total   int                    `json:"total"`
	Deleted int                    `json:"deleted"`
	Skipped int                    `json:"skipped"`
	Summary string                 `json:"summary,omitempty"` // summarize：汇总文本（新任务正文）
	Reports []CleanupSessionReport `json:"reports,omitempty"`
}

// ClearDone clear.done 载荷（批量清空 / 总结并清理完成广播：移动端提示与列表刷新）。
// summarize 模式下 NewTaskID/NewSessionID 指向汇总后新建的续接任务/会话。
type ClearDone struct {
	Mode         string `json:"mode,omitempty"` // clear | summarize
	OK           bool   `json:"ok"`
	Canceled     int    `json:"canceled,omitempty"`     // 取消的活动任务数
	Deleted      int    `json:"deleted,omitempty"`      // 删除的任务记录数
	NewTaskID    string `json:"newTaskId,omitempty"`    // summarize：新任务 ID
	NewSessionID string `json:"newSessionId,omitempty"` // summarize：新会话 ID
	Summary      string `json:"summary,omitempty"`      // summarize：汇总预览（截断）
	Error        string `json:"error,omitempty"`
}

// ---------- 技能库 ----------

// SkillMeta 技能元数据投影（对齐 skills.Metadata；remote 包自定义类型，
// 不 import internal/skills 保持解耦，PC 端装配层做转换）。
type SkillMeta struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Path        string   `json:"path,omitempty"`
	Source      string   `json:"source,omitempty"` // builtin/global/project
	ModeSlugs   []string `json:"modeSlugs,omitempty"`
}

// SkillsListReq skills.list 载荷（client→server；NodeID 必填）。
type SkillsListReq struct {
	NodeID string `json:"nodeId"`
}

// SkillsGetReq skills.get 载荷（client→server）。
type SkillsGetReq struct {
	NodeID string `json:"nodeId"`
	Name   string `json:"name"`
}

// SkillsGet server.skills_get / server.skills_delete 载荷（server→agent）。
type SkillsGet struct {
	Name string `json:"name"`
}

// ---------- 数字员工 ----------

// EmployeeMeta 员工元数据投影（对齐 employee.Employee 的子集；remote 包自定义类型，
// 不 import internal/employee 保持解耦，PC 端装配层做转换）。供移动端群聊建群选成员。
type EmployeeMeta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"role,omitempty"` // 员工角色（rolePrompt 摘要）
	Description string `json:"description,omitempty"`
	Status      string `json:"status,omitempty"` // enabled/disabled/draft
	Avatar      string `json:"avatar,omitempty"`
}

// EmployeeListReq employee.list 载荷（client→server；NodeID 可空 = 全部在线节点）。
type EmployeeListReq struct {
	NodeID string `json:"nodeId,omitempty"`
}

// SkillsSaveReq skills.save 载荷（client→server）。
type SkillsSaveReq struct {
	NodeID  string `json:"nodeId"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// SkillsSave server.skills_save 载荷（server→agent）。
type SkillsSave struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// SkillsData agent.skills_data 载荷（agent.skills_get 响应；ID 关联请求信封 ID）。
type SkillsData struct {
	ID      string `json:"id,omitempty"` // 请求关联 ID（Envelope.ID 原样回传）
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Content string `json:"content,omitempty"` // SKILL.md 原始全文（frontmatter+body）
	Error   string `json:"error,omitempty"`
}

// MaskAPIKey APIKey 出站掩码：保留前 3 后 4 字符，中间 ****；
// 过短（≤8）全掩码。服务端 agent.config 缓存落盘/推客户端前统一调用，
// 确保明文密钥不出节点（服务端与移动端只见掩码形态）。
func MaskAPIKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	r := []rune(k)
	if len(r) <= 8 {
		return "****"
	}
	return string(r[:3]) + "****" + string(r[len(r)-4:])
}

// VerifyTasks verify.tasks / server.verify_tasks 载荷（任务状态核实）。
// 移动端→服务端为空载荷（核对全部在线节点可核任务）；服务端→节点按节点过滤后的
// taskId 清单（pending 暂存链状态节点无法判定，不参与核对）。
// RequestedBy 仅 server.verify_tasks 使用：服务端注入发起端标记（"mobile"
// / "admin"），节点回传 agent.verify_report 时原样透传，verify.done 广播
// 携带该标记 → 移动端区分「本端发起的核实」与「其他端发起的核实」。
// VerifyAll 仅 server.verify_tasks 使用：TaskIDs 为空（全部任务已终态）时
// 置 true，节点核实其 remoteTaskOf 中全部会话的真实状态 —— 覆盖节点断线
// 期间被 watchdog「自动收敛」误标 failed 的任务（节点本地会话实际已完成）。
type VerifyTasks struct {
	TaskIDs     []string `json:"taskIds,omitempty"`
	RequestedBy string   `json:"requestedBy,omitempty"`
	VerifyAll   bool     `json:"verifyAll,omitempty"`
}

// TaskCancel task.cancel / server.task_cancel 载荷。
// SessionID：取消在节点侧按会话停止（taskRun 以 sessionID 为隔离单元），
// 服务端 CancelTask 已知映射故顺带下发；旧服务端不带此字段 → 节点侧
// 用本地 remoteTaskOf 反查兑底。
type TaskCancel struct {
	TaskID    string `json:"taskId"`
	SessionID string `json:"sessionId,omitempty"` // 节点侧停止执行用（见上）
}

// ---------- 自主开发规划板（roadmap；移动端「AR 自主开发」页数据链路） ----------
//
// 对齐技能库 skills.* 的转发语义：客户端（移动端）发 roadmap.* → 服务端路由
// 到 PC 节点（server.roadmap_*）→ 节点操作本地规划板（roadmap_panel.go）→
// 变更后回传 agent.roadmap_data → 服务端落缓存 + roadmap 广播（全端同步）。
// 执行强制选择：roadmap.dev 服务端校验 IDs 非空（移动端按钮同样禁用兑底）。
const (
	TypeRoadmapList        = "roadmap.list"         // 拉取节点规划板（缓存立即应答 + 在线节点刷新）
	TypeRoadmapPlan        = "roadmap.plan"         // 触发 AI 规划（guidance 用户引导意见可空）
	TypeRoadmapDev         = "roadmap.dev"          // 执行自主开发（IDs 必填 = 用户勾选的提案）
	TypeRoadmapDelete      = "roadmap.delete"       // 删除规划条目
	TypeRoadmapDeleteBatch = "roadmap.delete_batch" // 批量删除规划条目（已完成/失败清理；IDs 必填）
	TypeRoadmapRetry       = "roadmap.retry"        // 一键重跑（failed/blocked 条目回队重开发）
	TypeRoadmapApprove     = "roadmap.approve"      // 批量审核通过（IDs 必填；只置 approved 入排期，不立即开发）
	TypeRoadmapAuto        = "roadmap.auto"         // 完全自主开发（guidance 可空；parallel 并发线程数）
	TypeRoadmap            = "roadmap"              // 规划板数据广播（roadmap.list 应答 / agent.roadmap_data 广播）

	TypeServerRoadmapList        = "server.roadmap_list"         // 拉取节点规划板（触发 agent.roadmap_data 回传）
	TypeServerRoadmapPlan        = "server.roadmap_plan"         // 触发节点 AI 规划
	TypeServerRoadmapDev         = "server.roadmap_dev"          // 触发节点批准+开发（勾选条目）
	TypeServerRoadmapDelete      = "server.roadmap_delete"       // 删除节点规划条目
	TypeServerRoadmapDeleteBatch = "server.roadmap_delete_batch" // 批量删除节点规划条目
	TypeServerRoadmapRetry       = "server.roadmap_retry"        // 一键重跑失败/暂停条目（failed/blocked → approved 回队）
	TypeServerRoadmapApprove     = "server.roadmap_approve"      // 批量审核通过（IDs → approved 入排期）
	TypeServerRoadmapAuto        = "server.roadmap_auto"         // 完全自主开发（自动规划 → AI 审核 → 并行开发）

	TypeAgentRoadmapData = "agent.roadmap_data" // 节点规划板全量回传（列表/状态变更后推送）
)

// RoadmapItem 规划条目投影（remote 包自定义类型，不 import internal/roadmap
// 保持解耦，PC 端装配层做转换；字段对齐 roadmap.Item 远端可见子集）。
type RoadmapItem struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Detail     string `json:"detail,omitempty"`
	Priority   string `json:"priority,omitempty"`   // high | medium | low
	Category   string `json:"category,omitempty"`   // feature | bug | refactor | docs | tooling
	Status     string `json:"status"`               // proposed | approved | developing | done | failed | blocked | rejected
	Source     string `json:"source,omitempty"`     // manual | plan:<session> | agent:<session> | autodev
	Result     string `json:"result,omitempty"`     // 完成摘要
	Error      string `json:"error,omitempty"`      // 失败原因
	Review     string `json:"review,omitempty"`     // AI 审核意见
	ApprovedBy string `json:"approvedBy,omitempty"` // user | ai_review
	// Attempts 开发尝试次数（含失败自动重试）；BlockedReason 重试超限封存原因。
	Attempts      int    `json:"attempts,omitempty"`
	BlockedReason string `json:"blockedReason,omitempty"`
	// RunLog 最近执行日志（执行轨迹：耗时/结论/摘要/改动文件；最新在前）。
	RunLog    []RoadmapRunEntry `json:"runLog,omitempty"`
	CreatedAt int64             `json:"createdAt,omitempty"`
	UpdatedAt int64             `json:"updatedAt,omitempty"`
}

// RoadmapRunEntry 执行日志投影（finishedAt 毫秒时间戳，对齐移动端 fmtTime）。
type RoadmapRunEntry struct {
	Outcome    string   `json:"outcome"`           // done | failed | blocked
	Summary    string   `json:"summary,omitempty"` // 结果摘要 / 失败原因
	Files      []string `json:"files,omitempty"`   // 改动文件清单
	DurationMS int64    `json:"durationMS,omitempty"`
	FinishedAt int64    `json:"finishedAt,omitempty"`
}

// RoadmapStats 规划板统计（移动端统计行）。
type RoadmapStats struct {
	Total      int `json:"total"`
	Proposed   int `json:"proposed"`
	Approved   int `json:"approved"`
	Developing int `json:"developing"`
	Done       int `json:"done"`
	Failed     int `json:"failed"`
	Blocked    int `json:"blocked"`
	Rejected   int `json:"rejected"`
}

// RoadmapAutoCfg 自动迭代配置投影（移动端展示开发模式；简化子集）。
// 2026-10 增强：规划任务数 / 自动审核 / 驳回删除 / 最低并行数随投影同步
// （移动端「AR 自主开发」页与 PC 端规划板设置同源可见）。
type RoadmapAutoCfg struct {
	Enabled       bool   `json:"enabled"`
	DevMode       string `json:"devMode,omitempty"`       // parallel（默认）| serial
	Parallel      int    `json:"parallel,omitempty"`      // 并行开发数（1~4）
	MaxPlanItems  int    `json:"maxPlanItems,omitempty"`  // 单次规划提案上限
	MinDeveloping int    `json:"minDeveloping,omitempty"` // 最低并行开发数（自主模式补货阈值）
	AutoReview    bool   `json:"autoReview,omitempty"`    // 规划后自动 AI 审核
	RejectDelete  bool   `json:"rejectDelete,omitempty"`  // 审核驳回直接删除
}

// RoadmapDataPayload 规划板全量数据（agent.roadmap_data 上行 / roadmap 广播载荷）。
// NodeID 由服务端回填（agent 上行不携带，以连接身份为准）。
type RoadmapDataPayload struct {
	NodeID string         `json:"nodeId,omitempty"`
	Items  []RoadmapItem  `json:"items"`
	Stats  RoadmapStats   `json:"stats"`
	Auto   RoadmapAutoCfg `json:"auto,omitempty"`
}

// RoadmapListReq roadmap.list 载荷（client→server；NodeID 必填）。
type RoadmapListReq struct {
	NodeID string `json:"nodeId"`
}

// RoadmapPlanReq roadmap.plan / server.roadmap_plan 载荷（guidance 用户引导意见，可空）。
type RoadmapPlanReq struct {
	NodeID   string `json:"nodeId,omitempty"` // client→server 方向需要
	Guidance string `json:"guidance,omitempty"`
}

// RoadmapDevReq roadmap.dev / server.roadmap_dev 载荷。
// IDs 必填（用户勾选的提案；空 = 未选择，服务端直接拒绝——执行前必须选择）。
type RoadmapDevReq struct {
	NodeID   string   `json:"nodeId,omitempty"`
	IDs      []string `json:"ids"`
	Mode     string   `json:"mode,omitempty"`     // parallel（默认）| serial
	Parallel int      `json:"parallel,omitempty"` // 并行数（1~4；仅 parallel 生效）
}

// RoadmapDeleteReq roadmap.delete / server.roadmap_delete 载荷。
type RoadmapDeleteReq struct {
	NodeID string `json:"nodeId,omitempty"`
	ID     string `json:"id"`
}

// RoadmapDeleteBatchReq roadmap.delete_batch / server.roadmap_delete_batch 载荷。
type RoadmapDeleteBatchReq struct {
	NodeID string   `json:"nodeId,omitempty"`
	IDs    []string `json:"ids"`
}

// RoadmapAutoReq roadmap.auto / server.roadmap_auto 载荷（guidance 提示词可空；
// parallel 并发线程数；minDeveloping 最低并行开发数——开发中低于该值时持续规划补货）。
type RoadmapAutoReq struct {
	NodeID        string `json:"nodeId,omitempty"`
	Guidance      string `json:"guidance,omitempty"`
	Parallel      int    `json:"parallel,omitempty"`
	MinDeveloping int    `json:"minDeveloping,omitempty"` // 最低并行开发数（0 = PC 端配置/默认）
}

// ---------- A2A 群聊（group；Agent-to-Agent 群组协作） ----------
//
// 语义对齐 A2A（Google/Linux Foundation Agent2Agent Protocol）概念映射：
//
//	Agent Card → AgentProfile（员工/用户/群助手的 profile）；
//	Message/Part → GroupMessage{Author, Role, Text, Images}；
//	Task → GroupTask（群内写任务，由群助手协调派发给成员员工）；
//	Artifact → 共享空间文件（.kaelvoris/groups/<gid>/shared/，P2）；
//	Transport → 既有 WS 信封（group.* 类型族），不引入 HTTP JSON-RPC 新端点。
//
// 权威状态在 PC 节点（员工主场：员工进程/工具/工作空间都在节点），服务端只做
// 路由/广播/离线暂存；群消息不落服务端（归属节点会话持久化）。
const (
	// ---- client（手机/管理后台）→ server ----
	TypeGroupCreate = "group.create" // 创建群聊 {Name, Members[]}
	TypeGroupList   = "group.list"   // 拉取群列表
	TypeGroupMsg    = "group.msg"    // 发送群消息 {GroupID, Author, Text}
	TypeGroupTask   = "group.task"   // 派发群写任务 {GroupID, Instruction}（群助手协调）
	TypeGroupJoin   = "group.join"   // 添加员工/成员到群 {GroupID, AgentID}
	TypeGroupLeave  = "group.leave"  // 移出员工/成员 {GroupID, AgentID}
	TypeGroupSync   = "group.sync"   // 拉取群状态快照（群聊页打开时）

	// ---- server → client ----
	TypeGroups        = "groups"          // 群列表广播 {groups: []}
	TypeGroupCreated  = "group.created"   // 群已创建 {group}
	TypeGroupMessage  = "group.message"   // 群消息广播 {group, message}
	TypeGroupState    = "group.state"     // 群状态快照（group.sync 应答 / 变更推送）
	TypeGroupTaskDone = "group.task_done" // 群写任务完成 {group, task, result}

	// ---- server → agent（节点）----
	TypeServerGroupCreate = "server.group_create" // 创建群（节点侧落盘群状态）
	TypeServerGroupMsg    = "server.group_msg"    // 群消息（节点广播给成员/群助手）
	TypeServerGroupTask   = "server.group_task"   // 派发群写任务（节点侧群助手协调）
	TypeServerGroupJoin   = "server.group_join"   // 添加成员
	TypeServerGroupLeave  = "server.group_leave"  // 移出成员
	TypeServerGroupSync   = "server.group_sync"   // 拉取群状态（节点回传 agent.group_state）

	// ---- agent（PC 节点）→ server ----
	TypeAgentGroupState = "agent.group_state"     // 节点群状态上报 {groups: []}
	TypeAgentGroupMsg   = "agent.group_msg"       // 成员员工产出消息回传（广播到群）
	TypeAgentGroupDone  = "agent.group_task_done" // 群写任务完成回传
)

// AgentProfile 群成员画像（A2A Agent Card 语义映射）：
// 员工/用户/群助手均是一份 profile。
type AgentProfile struct {
	ID          string   `json:"id"`                    // 稳定 ID（员工 ID / user:<name> / bot）
	Name        string   `json:"name"`                  // 显示名
	Kind        string   `json:"kind,omitempty"`        // user | employee | bot
	Description string   `json:"description,omitempty"` // 一句话职责（A2A description）
	Avatar      string   `json:"avatar,omitempty"`      // emoji 头像
	Skills      []string `json:"skills,omitempty"`      // 能力标签（A2A skills）
}

// Group 群聊元数据。
type Group struct {
	ID        string         `json:"id"`                  // 群 ID（g<rand>）
	Name      string         `json:"name"`                // 群名
	Owner     string         `json:"owner,omitempty"`     // 创建者（user:<name>）
	Members   []AgentProfile `json:"members"`             // 成员（用户 + 员工）
	Workspace string         `json:"workspace,omitempty"` // 群工作目录（P1 群助手用；P2 各员工共享）
	NodeID    string         `json:"nodeId,omitempty"`    // 归属节点（服务端回填）
	CreatedAt int64          `json:"createdAt,omitempty"`
	UpdatedAt int64          `json:"updatedAt,omitempty"`
}

// GroupMessage 群消息（A2A Message/Part 语义映射；图片后续扩展）。
type GroupMessage struct {
	ID         string `json:"id,omitempty"`
	GroupID    string `json:"groupId"`
	Author     string `json:"author"`
	AuthorName string `json:"authorName,omitempty"`
	AuthorKind string `json:"authorKind,omitempty"` // user | employee | bot
	Text       string `json:"text"`
	TS         int64  `json:"ts,omitempty"`
}

// GroupTask 群写任务（A2A Task 语义映射：群助手协调 → 成员员工执行 → 汇总产出）。
type GroupTask struct {
	ID          string `json:"id,omitempty"`
	GroupID     string `json:"groupId"`
	Instruction string `json:"instruction"`
	// AssignedTo 逗号分隔的员工 ID 列表（接力对话：按序执行，后员工输入含前序产出）；
	// 空 = 群助手协调派发（取第一个 employee 成员），保持旧行为。
	AssignedTo string `json:"assignedTo,omitempty"`
	// Mode 协作模式：chain 接力 / parallel 并行分工 / review 评审打磨；
	// 空 = chain（旧客户端不发 mode 字段，向后兼容）。
	Mode      string `json:"mode,omitempty"`
	CreatedBy string `json:"createdBy,omitempty"`
	Status    string `json:"status,omitempty"` // running | done | failed
	// Result 汇总摘要（兼容旧消费方，如移动端任务卡）；详细分员工产出见 Results。
	Result  string            `json:"result,omitempty"`
	Results []GroupStepResult `json:"results,omitempty"` // 各员工执行产出（接力链按序记录）
	TS      int64             `json:"ts,omitempty"`
}

// GroupStepResult 接力链中单个员工的执行产出（GroupTask.Results 条目）。
type GroupStepResult struct {
	EmployeeID string `json:"employeeId,omitempty"`
	Name       string `json:"name,omitempty"`
	Result     string `json:"result,omitempty"`
}

// GroupCreateReq group.create 载荷（client→server）。
type GroupCreateReq struct {
	Name    string   `json:"name"`
	Members []string `json:"members,omitempty"` // 初始成员（员工 ID 列表；用户自动加入）
}

// GroupListReq group.list / group.sync 载荷（client→server）。
type GroupListReq struct {
	NodeID  string `json:"nodeId,omitempty"` // 空 = 全部在线节点
	GroupID string `json:"groupId,omitempty"`
}

// GroupMsgReq group.msg 载荷（client→server）。
type GroupMsgReq struct {
	GroupID string `json:"groupId"`
	Text    string `json:"text"`
}

// GroupTaskReq group.task 载荷（client→server）。
type GroupTaskReq struct {
	GroupID     string `json:"groupId"`
	Instruction string `json:"instruction"`
	AssignedTo  string `json:"assignedTo,omitempty"`
	// Mode 协作模式：chain 接力 / parallel 并行分工 / review 评审打磨；
	// 空 = chain（旧客户端不发 mode 字段，向后兼容）。
	Mode string `json:"mode,omitempty"`
}

// GroupMemberReq group.join / group.leave 载荷（client→server）。
type GroupMemberReq struct {
	GroupID string `json:"groupId"`
	AgentID string `json:"agentId"`
}

// GroupServerReq server.group_* 载荷（server→agent）：
// 转发 client 侧请求（GroupID + Text/AgentID 等；节点按自身群状态执行）。
type GroupServerReq struct {
	GroupID     string   `json:"groupId"`
	Name        string   `json:"name,omitempty"`
	Members     []string `json:"members,omitempty"`
	Text        string   `json:"text,omitempty"`
	Instruction string   `json:"instruction,omitempty"`
	AssignedTo  string   `json:"assignedTo,omitempty"`
	// Mode 协作模式：chain 接力 / parallel 并行分工 / review 评审打磨；
	// 空 = chain（旧客户端不发 mode 字段，向后兼容）。
	Mode    string `json:"mode,omitempty"`
	AgentID string `json:"agentId,omitempty"`
}

// GroupStatePayload 群状态快照（group.sync 应答 / agent.group_state 上报 / 广播）。
// Messages 为群消息历史（节点侧持久化；sync 应答时回传，广播时可省）。
type GroupStatePayload struct {
	NodeID   string         `json:"nodeId,omitempty"`
	Groups   []Group        `json:"groups"`
	Messages []GroupMessage `json:"messages,omitempty"` // 群消息历史（group.sync 应答 / agent.group_state 回传）
	Message  *GroupMessage  `json:"message,omitempty"`  // group.message 广播附带（可选）
	Task     *GroupTask     `json:"task,omitempty"`     // group.task_done 广播附带（可选）
}

// ---------- 通用 IM 渠道公网中继（IMRelay；本地 PC 无外网时的接收通道） ----------
//
// 背景：IM 平台的回调必须打到公网可达 URL，而 PC 节点在
// 内网（无公网入站）。复用 /ws/agent 既有节点长连接做转发：
//
//	IM 平台 ──HTTP POST──▶ kaelvoris-server（公网）/im/callback、/im/group/<hookID>
//			│ 立即应答（平台 10 秒超时；转发在后台 goroutine）
//			▼ server.im_inbound（WS 下发节点）
//		PC 节点 IMChannel.onRelayedCallback / onRelayedGroupMsg
//		（复用验签/去重/白名单/会话路由，回复经节点出站直连 IM 平台）
//
// 路由：服务端按「节点上报的渠道匹配」转发——节点注册中继渠道时上报
// Name+PublicID+EID+WebhookToken+GroupHookIDs 五元组（IMRelayRegister），
// 服务端以五元组精确匹配回调归属渠道 → 目标节点（多节点多渠道共存互不串扰；
// NodeID 为空 = 全部在线节点广播）。回调路径携带渠道指纹（im/c/<fp>、
// im/g/<fp>/<hook>），服务端按指纹查表路由，URL 不含敏感凭据。
const (
	// ---- client（IM 平台 HTTP）→ server → agent ----
	TypeServerIMInbound = "server.im_inbound" // IM 回调转发（公共号 pubRespon / 群消息载荷）

	// ---- agent（PC 节点）→ server ----
	TypeAgentIMState = "agent.im_state" // IM 中继渠道注册表上报（节点启用中继时）

	// ---- client（移动端/管理端）→ server：仅渠道状态查询，无下行控制 ----
	TypeIMState = "im.state" // IM 中继渠道注册表广播（渠道列表投影，全端展示）
)

// IMRelayChannel 单个 IM 渠道的中继注册条目（节点 → 服务端上报）。
// 五元组（Name+PublicID+EID+WebhookToken+GroupHookIDs）即渠道指纹：
// 服务端匹配回调 URL 的 fp → 渠道 → 节点。与桌面端 ChannelConfig 对应
// 字段同构（remote 包不 import 宿主包，保持解耦）。
type IMRelayChannel struct {
	Name         string `json:"name"`
	PublicID     string `json:"publicId,omitempty"`
	EID          string `json:"eid,omitempty"`
	WebhookToken string `json:"webhookToken,omitempty"`
	GroupHookIDs string `json:"groupHookIds,omitempty"` // 逗号分隔
}

// IMRelayState agent.im_state / im.state 载荷（节点 IM 中继渠道注册表）。
// Enabled = 渠道启用且配置了中继（公网回调入口）；指纹列表服务端用于回调
// URL 路由。服务端仅投影（节点 channels.json 才是权威源），不持久化 ——
// 节点重连/渠道保存后经 agent.im_state 刷新（含 OnOnline 全量上报兑底）。
type IMRelayState struct {
	NodeID   string           `json:"nodeId,omitempty"` // 服务端回填（连接身份）
	Enabled  bool             `json:"enabled"`          // 渠道启用且存在中继渠道
	Channels []IMRelayChannel `json:"channels"`
}

// IMRelayInbound server.im_inbound 载荷：IM 平台回调转发（原样透传）。
// Kind 分流：pub = 公共号回调（IMPubCallback 同构 JSON 原样转发）；
// group = 群消息（原始 JSON/文本转发，节点侧经 InMap/宽松解析）。
// Path 保留原始路径（诊断）；Body 为原始请求体（仅 JSON，二进制不支持——
// IM 平台推送均为 JSON/表单文本，表单场景节点侧宽松解析兜底）。
type IMRelayInbound struct {
	Kind string `json:"kind"` // pub | group
	// 渠道定位（服务端按回调 URL 指纹反查后填入）：节点据此精确路由到对应
	// IMChannel 实例（同节点多渠道共存）。
	Name         string `json:"name,omitempty"`
	PublicID     string `json:"publicId,omitempty"`
	EID          string `json:"eid,omitempty"`
	WebhookToken string `json:"webhookToken,omitempty"`
	GroupHookID  string `json:"groupHookId,omitempty"` // kind=group：hookID（URL 段，单群路由）
	// GroupHooks 渠道完整 hook 列表（服务端从注册表透传）：注册指纹按完整
	// 列表计算，转发载荷必须携带同一五元组，否则群消息模式指纹不匹配无法
	// 定位渠道（节点侧 imRelayChannelFP 契约）。
	GroupHooks string `json:"groupHooks,omitempty"`
	Body       string `json:"body"`           // 原始请求体（UTF-8 文本）
	Path       string `json:"path,omitempty"` // 原始回调路径（诊断）
}
