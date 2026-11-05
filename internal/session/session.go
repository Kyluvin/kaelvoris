// Package session 会话管理。
//
// 对齐 Zoo-Code task-persistence 的三类数据：ClineMessages、API 消息、任务元数据。
// P2.6 扩展 JSON 文件持久化（~/.kaelvoris/sessions/<id>.json），支持会话列表 /
// 打开回放 / 删除（P3 可平滑升级 SQLite 与会话恢复）。
//
// P3.1 并发安全改造（修复停止/切换卡顿）：
//   - Session 内置 RWMutex：引擎 say 桥（多 goroutine 并发 Append）与
//     Store 刷盘（后台 goroutine Snapshot 序列化）互不阻塞 UI 推送。
//   - AppendMerged：流式 partial 消息合并（同类型尾部覆盖，不堆积），
//     会话文件不再随流式 token 线性膨胀。
//   - Todos 持久化：待办清单随会话落盘，停止后"继续"可恢复（P3.1-B）。
package session

import (
	"strings"
	"sync"
	"time"

	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// CurrentSchemaVersion 当前会话文件 schema 版本（写入时由 Store.write 统一打标）。
// v1（2026-10）起会话 JSON 顶层携带 schemaVersion 字段；读取时 SchemaVersion==0
// 即旧格式（历史会话无此字段），走读取期缺省分支（当前结构与 v1 兼容，无需迁移）。
// 版本只属于文件格式（落盘/加载），不进运行态 Session——运行态永远是最新代码。
// 为旧格式归一化与后续 schema 迁移提供显式判别依据，替代逐字段嗅探。
const CurrentSchemaVersion = 1

// SessionData 会话纯数据快照（不含锁，可安全跨 goroutine 复制/序列化/落盘）。
type SessionData struct {
	ID          string `json:"id"`
	ChannelID   string `json:"channelId"` // desktop | lark | wechat | cli
	Title       string `json:"title,omitempty"`
	ManualTitle bool   `json:"manualTitle,omitempty"` // 用户手动命名（右键重命名）；此后自动命名（任务首行）不再覆盖
	Mode        string `json:"mode,omitempty"`
	Model       string `json:"model,omitempty"`       // 会话自有模型（切换会话联动恢复；空 = 跟随全局配置）
	NodeID      string `json:"nodeId,omitempty"`      // 会话自有执行节点（远程会话发布目标；空 = 本机执行）
	Workspace   string `json:"workspace,omitempty"`   // 会话关联工作区（切换会话联动切换）
	MCPDisabled bool   `json:"mcpDisabled,omitempty"` // 会话禁用 MCP（右侧会话设置面板；定义侧不连接注册 + 执行侧网关拒绝，双保险）
	EmployeeID  string `json:"employeeId,omitempty"`  // 数字员工绑定（§9.1：会话记录生效员工；空 = 普通会话）
	PositionID  string `json:"positionId,omitempty"`  // 岗位分配快照（设计方案 §3/§6.1：会话创建/切换员工时固化，员工调岗后不漂移；空 = 无岗位绑定）
	AssignRule  string `json:"assignRule,omitempty"`  // 分配规则快照（设计方案 §2.1：random|round_robin|least_loaded；空 = random；解析结果随快照写入会话，会话生命周期内不重选）
	ACPAgent    string `json:"acpAgent,omitempty"`    // ACP agent 绑定（会话级：任务固定派发给外部 agent；空 = 内置引擎）
	ParentID    string `json:"parentId,omitempty"`    // 父会话 ID（new_task 委派子任务；空 = 独立会话）
	// SessionSkills 会话技能挂载（用户需求 2026-12：聊天页右侧技能 TAB）。
	// key = 技能名，value = 用户配置参数的 JSON 字符串（空 = 无参数）。
	// 仅显式添加进会话的技能参与提示词注入与一键运行。nil = 未挂载。
	SessionSkills map[string]string `json:"sessionSkills,omitempty"`
	// 规划板 2026-10 会话卡片信息升级：手动摘要 / 错误态 / 置顶标记随会话持久化。
	ManualPreview string    `json:"manualPreview,omitempty"` // 用户手动摘要（提示词 manual_preview；非空时 Summary 覆盖自动提取）
	HasError      bool      `json:"hasError,omitempty"`      // 最近一段错误态（say error / 任务失败落置；新任务开始清除）
	Done          bool      `json:"done,omitempty"`          // 最近一段成功完成（任务正常结束置位；新任务开始/出错清除）
	PinnedAt      time.Time `json:"pinnedAt,omitempty"`      // 置顶时间（零值 = 未置顶；Summary 导出毫秒时间戳）
	// SchemaVersion 会话文件 schema 版本（只读打标字段，SessionData 是落盘
	// 直接序列化结构）：写入时由 Store.write 统一置 CurrentSchemaVersion；
	// 读取时 0 = 旧格式（历史会话无此字段），loadLocked 按缺省分支归一为当前
	// 版本（归一化结果随 IntoSession 进入运行态，消费方可显式判别）。
	SchemaVersion int                    `json:"schemaVersion,omitempty"`
	Messages      []message.ClineMessage `json:"messages,omitempty"`
	Todos         []tools.TodoItem       `json:"todos,omitempty"` // 待办清单持久化（会话恢复用）
	// Condense 最近一次上下文摘要压缩记录（规划板 2026-10 长会话上下文自动压缩）。
	// Condensed = true 时摘要正文（Summary）随会话持久化：续聊经 ResumeDigest
	// 注入模型（早期要点可引用），前端可回放压缩标注。
	Condense *CondenseRecord `json:"condense,omitempty"`
	// TaskSummary 最近一次任务结束的会话总结（会话 hook：任务结束后 LLM 生成）。
	// 与 Condense（上下文压缩）不同：TaskSummary 是「任务收尾总结」——面向用户
	// 阅读的目标/产出/进度快照，不参与模型上下文注入（续聊仍走 ResumeDigest）。
	// nil = 尚未产生总结（任务未结束 / 用户关闭总结 / 模型未配置）。
	TaskSummary *TaskSummary `json:"taskSummary,omitempty"`
	CreatedAt   time.Time    `json:"createdAt"`
	UpdatedAt   time.Time    `json:"updatedAt"`
}

// CondenseRecord 上下文摘要压缩记录（会话级，每次压缩覆盖——模型续接只需
// 最近一次摘要即可覆盖全部早期历史；保留累计统计供前端展示）。
type CondenseRecord struct {
	Condensed       bool      `json:"condensed"`       // 是否发生过摘要压缩
	RemovedMessages int       `json:"removedMessages"` // 被摘要替换的历史消息条数
	KeptMessages    int       `json:"keptMessages"`    // 保留原文的最近消息条数
	Summary         string    `json:"summary"`         // LLM 生成的摘要正文
	CondensedAt     time.Time `json:"condensedAt"`     // 压缩时间
}

// TaskSummary 任务结束会话总结（会话 hook 产出，2026-10 新增）。
// 字段语义（对齐 selflearn.go 记忆收尾的指针式精简方针）：
//   - Goal 任务目标一句话；Status 完成态（done/failed/stopped）；
//   - Outcome 做了什么 / 产出物路径指针（正文不入库，按需 fs_read）；
//   - Next 未完成项的续接切入点（空 = 全部完成）；
//   - Elapsed 任务耗时（秒）；GeneratedAt 生成时间（前端展示用）。
type TaskSummary struct {
	Goal        string    `json:"goal,omitempty"`
	Status      string    `json:"status,omitempty"` // done | failed | stopped
	Outcome     string    `json:"outcome,omitempty"`
	Next        string    `json:"next,omitempty"`
	ElapsedSecs int64     `json:"elapsedSecs,omitempty"`
	GeneratedAt time.Time `json:"generatedAt,omitempty"`
}

// Session 一次任务会话（并发安全；快照用 Snapshot()）。
type Session struct {
	mu sync.RWMutex

	ID          string
	ChannelID   string
	Title       string
	ManualTitle bool // 用户手动命名：置位后 SetTitle 自动命名不再覆盖
	// 规划板 2026-10 会话卡片信息升级（与 SessionData 同名字段镜像）：
	ManualPreview string    // 用户手动摘要（提示词 manual_preview；非空时 Summary 覆盖自动提取）
	HasError      bool      // 最近一段错误态（say error / 任务失败落置；新任务开始清除）
	Done          bool      // 最近一段成功完成（任务正常结束置位；新任务开始/出错清除）
	PinnedAt      time.Time // 置顶时间（零值 = 未置顶；Summary 导出毫秒时间戳）
	Mode          string
	Model         string // 会话自有模型（空 = 跟随全局配置）
	NodeID        string // 会话自有执行节点（远程会话发布目标；空 = 本机执行）
	Workspace     string // 会话关联工作区（打开时联动切换）
	MCPDisabled   bool   // 会话禁用 MCP（右侧会话设置面板；运行期双保险：定义侧不连接 + 执行侧网关拒绝）
	EmployeeID    string // 数字员工绑定（§9.1 首次消息固定生效员工；空 = 普通会话）
	PositionID    string // 岗位分配快照（设计方案 §3/§6.1：固化员工当前岗位，调岗不漂移；空 = 无岗位绑定）
	AssignRule    string // 分配规则快照（设计方案 §2.1：random|round_robin|least_loaded；空 = random）
	ACPAgent      string // ACP agent 绑定（任务派发给外部 agent；空 = 内置引擎）
	ParentID      string // 父会话 ID（new_task 委派子任务；空 = 独立会话）
	// SessionSkills 会话技能挂载（右侧技能 TAB；技能名 → 参数 JSON）。
	SessionSkills map[string]string
	SchemaVersion int // 会话文件 schema 版本（从盘加载时携带，旧格式已归一为当前版；仅持久化层使用）
	Messages      []message.ClineMessage
	Todos         []tools.TodoItem // 待办清单持久化（会话恢复用）
	Condense      *CondenseRecord  // 最近一次上下文摘要压缩记录（规划板 2026-10）
	TaskSummary   *TaskSummary     // 最近一次任务结束会话总结（会话 hook 产出）
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Message 访问锁（B2 tasklog 需安全读取会话字段）。
func (s *Session) Mu() *sync.RWMutex { return &s.mu }

// SetCondense 写入上下文摘要压缩记录（引擎 OnCondense 回调路径触发，
// 随会话持久化；nil = 清除记录）。
func (s *Session) SetCondense(r *CondenseRecord) {
	s.mu.Lock()
	s.Condense = r
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetTaskSummary 写入任务结束会话总结（会话 hook 回调路径触发，随会话持久化；
// nil = 清除）。
func (s *Session) SetTaskSummary(r *TaskSummary) {
	s.mu.Lock()
	s.TaskSummary = r
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// Append 追加消息。
func (s *Session) Append(m message.ClineMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, m)
	s.UpdatedAt = time.Now()
}

// AppendMerged 追加消息，流式 partial 合并：
// 尾部为同 say/ask 类型的 partial 消息时覆盖（引擎每 chunk 发累计全文，
// 不合并会导致会话文件随流式 chunk 数线性膨胀——P3.1 卡顿根因之一）。
// 定型消息（Partial=false）到达时还需清理历史中同类型的流式中间帧：
// 引擎定型顺序为 partials → SayAPIReqFinished → 定型消息，partial 帧被
// api_req_finished 隔断后旧实现只认「尾部相邻」，定型版无法替换 partial，
// 同段回复在会话历史中存两份 → 回放/实时重复渲染（「回复重复」根因）。
// 清理规则：定型消息向前跳过其他类型，移除所有同类型 partial，遇到同类型
// 定型消息即停（已闭合的流式周期不动；不会误删更早轮次的定型历史）。
func (s *Session) AppendMerged(m message.ClineMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !m.Partial {
		for i := len(s.Messages) - 1; i >= 0; i-- {
			p := s.Messages[i]
			if p.Type != m.Type || p.Say != m.Say || p.Ask != m.Ask {
				continue
			}
			if !p.Partial {
				break // 同类型定型消息：流式周期已闭合，停止向前
			}
			s.Messages = append(s.Messages[:i], s.Messages[i+1:]...)
		}
	}
	if n := len(s.Messages); n > 0 {
		last := s.Messages[n-1]
		if last.Partial && m.Partial && last.Type == m.Type && last.Say == m.Say && last.Ask == m.Ask {
			s.Messages[n-1] = m
			s.UpdatedAt = time.Now()
			return
		}
	}
	s.Messages = append(s.Messages, m)
	s.UpdatedAt = time.Now()
}

// SetTitle 更新会话标题（自动命名路径）：手动命名（ManualTitle）后不再覆盖，
// 保证用户改的名称在后续任务/续聊中保持不变。
func (s *Session) SetTitle(t string) {
	s.mu.Lock()
	if s.ManualTitle {
		s.mu.Unlock()
		return
	}
	s.Title = t
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetPinned 置顶/取消置顶（会话卡右键菜单）：写 PinnedAt 时间戳，列表
// 排序置顶优先（语义对齐服务端 Hub.PinSession / Task.Pinned 毫秒时间戳）。
func (s *Session) SetPinned(pinned bool) {
	s.mu.Lock()
	if pinned {
		s.PinnedAt = time.Now()
	} else {
		s.PinnedAt = time.Time{}
	}
	s.mu.Unlock()
}

// SetManualPreview 写入手动摘要（提示词 manual_preview；空串 = 恢复自动提取）。
func (s *Session) SetManualPreview(p string) {
	s.mu.Lock()
	s.ManualPreview = strings.TrimSpace(p)
	s.mu.Unlock()
}

// SetError 更新最近一段错误态（引擎 error 事件/任务失败落置；新任务开始清除）。
// SetError(true) 同时清除完成态（错误 > 完成的终态语义：最近一段以错误收尾）。
func (s *Session) SetError(v bool) {
	s.mu.Lock()
	s.HasError = v
	if v {
		s.Done = false
	}
	s.mu.Unlock()
}

// SetDone 更新最近一段成功完成态（任务正常结束置位；新任务开始清除）。
// SetDone(true) 同时清除错误态（成功完成 = 最近一段以完成收尾，覆盖中途已恢复的报错）。
func (s *Session) SetDone(v bool) {
	s.mu.Lock()
	s.Done = v
	if v {
		s.HasError = false
	}
	s.mu.Unlock()
}

// Rename 手动重命名会话（右键改名）：写入手动名称并置 ManualTitle，
// 此后所有自动命名（任务首行等）不再覆盖。空名视为取消。
func (s *Session) Rename(t string) {
	s.mu.Lock()
	if strings.TrimSpace(t) == "" {
		s.mu.Unlock()
		return
	}
	s.Title = strings.TrimSpace(t)
	s.ManualTitle = true
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetWorkspace 关联会话工作区（任务运行前记录；切换会话时前端据此联动切换项目）。
func (s *Session) SetWorkspace(w string) {
	s.mu.Lock()
	s.Workspace = w
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetTodos 覆盖待办清单（update_todo_list 工具触发时回写会话）。
func (s *Session) SetTodos(items []tools.TodoItem) {
	s.mu.Lock()
	s.Todos = items
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// TodosSnapshot 返回待办清单快照。
func (s *Session) TodosSnapshot() []tools.TodoItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]tools.TodoItem, len(s.Todos))
	copy(out, s.Todos)
	return out
}

// MessagesSnapshot 返回消息历史快照（B2 任务运行日志导出用；跨 goroutine 安全）。
func (s *Session) MessagesSnapshot() []message.ClineMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]message.ClineMessage, len(s.Messages))
	copy(out, s.Messages)
	return out
}

// Snapshot 返回会话数据快照（纯数据，不含锁；跨 goroutine 安全）。
func (s *Session) Snapshot() SessionData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := SessionData{
		ID:            s.ID,
		ChannelID:     s.ChannelID,
		Title:         s.Title,
		ManualTitle:   s.ManualTitle,
		ManualPreview: s.ManualPreview,
		HasError:      s.HasError,
		Done:          s.Done,
		PinnedAt:      s.PinnedAt,
		Mode:          s.Mode,
		Workspace:     s.Workspace,
		EmployeeID:    s.EmployeeID,
		PositionID:    s.PositionID,
		AssignRule:    s.AssignRule,
		Model:         s.Model,
		NodeID:        s.NodeID,
		ACPAgent:      s.ACPAgent,
		MCPDisabled:   s.MCPDisabled,
		ParentID:      s.ParentID,
		SessionSkills: cloneStringMap(s.SessionSkills),
		SchemaVersion: s.SchemaVersion,
		TaskSummary:   s.TaskSummary,
		Condense:      s.Condense,
		CreatedAt:     s.CreatedAt,
		UpdatedAt:     s.UpdatedAt,
	}
	cp.Messages = make([]message.ClineMessage, len(s.Messages))
	copy(cp.Messages, s.Messages)
	cp.Todos = make([]tools.TodoItem, len(s.Todos))
	copy(cp.Todos, s.Todos)
	return cp
}

// IntoSession 把纯数据快照转为活跃会话对象（Store 从盘加载用）。
func (d SessionData) IntoSession() *Session {
	return &Session{
		ID:            d.ID,
		ChannelID:     d.ChannelID,
		Title:         d.Title,
		ManualTitle:   d.ManualTitle,
		ManualPreview: d.ManualPreview,
		HasError:      d.HasError,
		Done:          d.Done,
		PinnedAt:      d.PinnedAt,
		Mode:          d.Mode,
		Workspace:     d.Workspace,
		EmployeeID:    d.EmployeeID,
		PositionID:    d.PositionID,
		AssignRule:    d.AssignRule,
		Model:         d.Model,
		NodeID:        d.NodeID,
		ACPAgent:      d.ACPAgent,
		MCPDisabled:   d.MCPDisabled,
		ParentID:      d.ParentID,
		SessionSkills: d.SessionSkills,
		SchemaVersion: d.SchemaVersion,
		TaskSummary:   d.TaskSummary,
		Condense:      d.Condense,
		Messages:      d.Messages,
		Todos:         d.Todos,
		CreatedAt:     d.CreatedAt,
		UpdatedAt:     d.UpdatedAt,
	}
}

// SetEmployeeID 绑定数字员工（§9.1：首次消息固定生效员工，后续不变）。
// 空 = 解绑（普通会话）。
func (s *Session) SetEmployeeID(id string) {
	s.mu.Lock()
	s.EmployeeID = id
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetPositionSnapshot 写入岗位分配快照（设计方案 §3/§6.1：会话创建/切换员工时
// 由调用方固化 ResolveAssignment 解析结果，员工后续调岗不漂移；空对 = 解除
// 岗位快照）。分配规则枚举见设计方案 §2.1（空 = random 默认）。
func (s *Session) SetPositionSnapshot(positionID, assignRule string) {
	s.mu.Lock()
	s.PositionID = strings.TrimSpace(positionID)
	s.AssignRule = strings.TrimSpace(assignRule)
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetSessionSkills 覆盖会话技能挂载表（右侧技能 TAB 的添加/配置/移除统一入口）。
// nil 或空 map = 清空挂载；调用方传完整副本（内部不增量合并）。
func (s *Session) SetSessionSkills(m map[string]string) {
	s.mu.Lock()
	s.SessionSkills = cloneStringMap(m)
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// cloneStringMap map 拷贝（nil 安全；SessionSkills 快照/写入防共享底层）。
func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SetACPAgent 绑定 ACP agent（会话级：任务派发给外部 agent 执行；
// 空 = 内置引擎）。首次绑定后固定生效（续聊沿用）。
func (s *Session) SetACPAgent(name string) {
	s.mu.Lock()
	s.ACPAgent = strings.TrimSpace(name)
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetModel 绑定会话自有模型（会话级：切换会话联动恢复选择器；
// 空 = 恢复跟随全局配置）。同 SetSessionWorkspace 语义。
func (s *Session) SetModel(m string) {
	s.mu.Lock()
	s.Model = strings.TrimSpace(m)
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetMode 写会话自有模式（执行策略；空 = 跟随全局当前模式）。
// 加锁写入（与 Snapshot 并发安全）。
func (s *Session) SetMode(m string) {
	s.mu.Lock()
	s.Mode = strings.TrimSpace(m)
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetNodeID 绑定会话自有执行节点（远程会话发布目标；空 = 本机执行）。
// 会话级持久化：切换会话联动恢复选择器，重启后不丢（rnState.sessNode
// 内存映射生命周期仅限登录态，会话记录是恢复源头）。
func (s *Session) SetNodeID(n string) {
	s.mu.Lock()
	s.NodeID = strings.TrimSpace(n)
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetMCPDisabled 会话级禁用/启用 MCP（右侧会话设置面板开关）：true = 本会话
// 不连接不注册任何 mcp_* 工具（定义侧），并拒绝残留调用（执行侧网关兜底）。
func (s *Session) SetMCPDisabled(v bool) {
	s.mu.Lock()
	s.MCPDisabled = v
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// AppendMessages 快照追加（供回放路径批量灌入历史）。
func (s *Session) AppendMessages(msgs []message.ClineMessage) {
	s.mu.Lock()
	s.Messages = append(s.Messages, msgs...)
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// Summary 会话列表条目（不含消息体，避免大 payload）。
// 规划板 2026-10 会话卡片信息升级（用户需求：摘要/错误态/置顶）：
// LastMessage 最后一条消息摘要（会话卡预览行）；HasError 错误态标识；
// PinnedAt 置顶毫秒时间戳（列表排序置顶优先，语义对齐服务端 Task.Pinned）。
type Summary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	ParentID     string    `json:"parentId,omitempty"`   // 父会话 ID（new_task 委派子任务；侧栏折叠树层级依据；旧会话由 App.sessionsWithRunning 按 ID 形态推导兑底）
	SkillCount   int       `json:"skillCount,omitempty"` // 会话技能挂载数（右侧技能 TAB 徽标；0 = 未挂载）
	Mode         string    `json:"mode,omitempty"`
	Model        string    `json:"model,omitempty"`       // 会话自有模型（切换联动恢复；空 = 跟随全局）
	NodeID       string    `json:"nodeId,omitempty"`      // 会话自有执行节点（远程会话；空 = 本机）
	Workspace    string    `json:"workspace,omitempty"`   // 会话关联工作区（切换联动）
	EmployeeID   string    `json:"employeeId,omitempty"`  // 数字员工绑定（前端员工角标）
	PositionID   string    `json:"positionId,omitempty"`  // 岗位分配快照（设计方案 §3/§5.2：资源管理面板会话 Tab 岗位/员工列展示）
	AssignRule   string    `json:"assignRule,omitempty"`  // 分配规则快照（设计方案 §2.1；空 = random）
	ACPAgent     string    `json:"acpAgent,omitempty"`    // ACP agent 绑定（前端 agent 角标）
	MCPDisabled  bool      `json:"mcpDisabled,omitempty"` // 会话禁用 MCP（右侧会话设置面板开关回显）
	MessageCount int       `json:"messageCount"`
	LastMessage  string    `json:"lastMessage,omitempty"` // 最后一条消息摘要（单行截断；会话卡预览）
	HasError     bool      `json:"hasError,omitempty"`    // 最近一段错误态（会话卡红点）
	Done         bool      `json:"done,omitempty"`        // 最近一段成功完成态（会话卡绿标题）
	PinnedAt     int64     `json:"pinnedAt,omitempty"`    // 置顶毫秒时间戳（0 = 未置顶；非零 = 置顶顺序依据）
	HasSummary   bool      `json:"hasSummary,omitempty"`  // 是否存在任务结束会话总结（会话 hook 产出；会话卡角标）
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	// Running 任务执行中（绿点标识，用户需求 2026-10）：不在持久化层填值——
	// 运行态在 App.runs 注册表，由 App.sessionsWithRunning 统一回填；
	// Storage.List 直列持久化数据时恒为 false。
	Running bool `json:"running,omitempty"`
	// PendingAsk 挂起审批卡（运行态回填，非持久化；用户需求 2026-12「不显示
	// 审批就卡住」修复）：会话正在阻塞等待应答的 ask 消息，由
	// App.sessionsWithRunning 从运行期事件提取随列表下发。引擎阻塞等人时
	// 前端必须可见可点——切回会话/列表刷新后据此恢复审批卡，治审批卡丢失后
	// 「任务卡住且无卡可点」的假死。
	PendingAsk *message.ClineMessage `json:"pendingAsk,omitempty"`
}

// Summary 生成列表条目。
func (s *Session) Summary() Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// UpdatedAt 零值兜底（升级兼容）：历史会话可能只落了 CreatedAt，
	// 零值 UpdatedAt 会使其在「按更新时间倒序」中沉底，回退用 CreatedAt。
	up := s.UpdatedAt
	if up.IsZero() {
		up = s.CreatedAt
	}
	// 手动摘要（提示词 manual_preview）优先于自动提取；置顶时间戳一并输出
	//（列表排序置顶优先，与移动端 pin.session 链路语义一致）。
	preview := s.ManualPreview
	if preview == "" {
		preview = lastMessageSummary(s.Messages)
	}
	return Summary{
		ID:           s.ID,
		Title:        s.Title,
		Mode:         s.Mode,
		Model:        s.Model,
		NodeID:       s.NodeID,
		Workspace:    s.Workspace,
		EmployeeID:   s.EmployeeID,
		PositionID:   s.PositionID,
		AssignRule:   s.AssignRule,
		ACPAgent:     s.ACPAgent,
		MCPDisabled:  s.MCPDisabled,
		ParentID:     s.ParentID,
		SkillCount:   len(s.SessionSkills),
		MessageCount: len(s.Messages),
		LastMessage:  preview,
		HasError:     s.HasError,
		Done:         s.Done,
		HasSummary:   s.TaskSummary != nil,
		// 零值时间不导出负数 UnixMilli（time.Time 零值 UnixMilli 为负）：
		// 前端/协议侧 0 = 未置顶的语义保持一致。
		PinnedAt:  pinnedAtMillis(s.PinnedAt),
		CreatedAt: s.CreatedAt,
		UpdatedAt: up,
	}
}

// pinnedAtMillis 置顶时间 → 毫秒时间戳（零值归 0 = 未置顶；time.Time 零值
// UnixMilli 为负数，直接导出会破坏「0 = 未置顶」的协议语义）。
func pinnedAtMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// summaryMaxRunes 摘要单行截断上限（rune 数，防超长文本入列表 payload；
// 前端 CSS 再按容器宽度二次截断，两层截断互为兑底）。
const summaryMaxRunes = 160

// truncateSummary 摘要单行化（换行/缩进折为单空格）+ rune 截断 + 省略号。
func truncateSummary(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > summaryMaxRunes {
		return string(r[:summaryMaxRunes]) + "…"
	}
	return s
}

// lastMessageSummary 倒扫会话消息提取最后一条可读摘要（会话卡预览行）。
// 跳过：partial 流式中间帧（定型版随后到达）、api_req_started/finished/retried
// （用量帧）、ai_context/usage（不落历史快照帧）；媒体卡返回占位文案。
// 取首条命中项（已单行化截断）；无可读消息返回空串（前端隐藏预览行）。
func lastMessageSummary(msgs []message.ClineMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Partial {
			continue
		}
		if m.Type == message.TypeSay {
			switch m.Say {
			case message.SayAPIReqStarted, message.SayAPIReqFinished, message.SayAPIReqRetried,
				message.SayAIContext, message.SayUsage:
				continue
			case message.SayImage:
				return "🖼 [图片]"
			case message.SayVideo:
				return "🎬 [视频]"
			case message.SayAudio:
				return "🔊 [音频]"
			}
		}
		if text := truncateSummary(m.Text); text != "" {
			return text
		}
	}
	return ""
}

// Manager 会话管理器（ChannelID+SessionID 聚合，内存版）。
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewManager 创建管理器。
func NewManager() *Manager {
	return &Manager{sessions: map[string]*Session{}}
}

// Get 获取或创建会话。
func (m *Manager) Get(channelID, sessionID string) *Session {
	key := channelID + ":" + sessionID
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[key]; ok {
		return s
	}
	s := &Session{
		ID:        sessionID,
		ChannelID: channelID,
		CreatedAt: time.Now(),
	}
	m.sessions[key] = s
	return s
}
