// Package roadmap 自主迭代规划板（autonomous roadmap）：
//
// 对齐业界自主 Agent 项目的 plan-and-execute 闭环（AutoGPT 目标分解 /
// MetaGPT 规划先行 / OpenDevin 任务队列 + OpenHands microagents 沉淀），
// 针对 kaelvoris 的落地语义：
//
//	规划（plan）→ 提案（proposed）→ 审批（approved，人工勾选或 AI 审核）
//	  → 开发（developing → done/failed，并行 N 路 / 依次）→ 复盘沉淀
//
// 审批路径（用户需求 2026-10 增强）：
//  1. 人工审核：面板勾选 → 「执行」→ approved（并行 N 路 / 依次）；
//  2. AI 审核：LLM 按审核提示词逐条 verdict——approved 自动纳入排期
//     并立即开发（ApprovedBy=ai_review）；rejected 驳回留痕；
//  3. 自动模式（desktop.json autoDev=true）：完全自主——自动规划、AI 审核、
//     自动排期开发、自动复盘，无需用户审核（带每日上限熔断）。
//
// 持久化：~/.kaelvoris/roadmap/items.json（原子写盘，损坏跳过不致命，
// 对齐 evolve/cron store 的存储约定）。
package roadmap

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 条目状态（单向流转为主，允许人工回拨）。
const (
	StatusProposed   = "proposed"   // 待审核（自主规划默认落此状态，等用户勾选）
	StatusApproved   = "approved"   // 已批准（勾选提交后；排队等待开发）
	StatusDeveloping = "developing" // 开发中（会话任务运行中）
	StatusDone       = "done"       // 已完成
	StatusFailed     = "failed"     // 开发失败（错误信息见 Error）
	StatusBlocked    = "blocked"    // 连续失败暂停（尝试超上限；人工重跑恢复）
	StatusRejected   = "rejected"   // 用户拒绝（留痕可回溯）
)

// 优先级与类别（规划产出标注，面板筛选用）。
const (
	PriorityHigh   = "high"
	PriorityMedium = "medium"
	PriorityLow    = "low"
)

// 限制常量（防提示词膨胀与列表失控）。
const (
	maxItems       = 200  // 条目总数上限（超出淘汰最旧的终态条目）
	maxTitleRunes  = 80   // 标题上限（rune）
	maxDetailRunes = 1200 // 实现要点上限（rune）
	maxResultRunes = 2000 // 完成摘要上限（rune）
)

// Item 一条功能规划条目（规划的最小执行单元）。
type Item struct {
	ID       string `json:"id"`
	Title    string `json:"title"`              // 一句话功能点（列表展示）
	Detail   string `json:"detail,omitempty"`   // 实现要点（开发任务的上下文）
	Priority string `json:"priority,omitempty"` // high | medium | low（空 = medium）
	Category string `json:"category,omitempty"` // feature | bug | refactor | docs | tooling
	Status   string `json:"status"`             // proposed | approved | developing | done | failed | rejected
	Source   string `json:"source,omitempty"`   // manual | plan:<session> | agent:<session> | autodev
	// Workspace 开发该条目时使用的工作区（规划时当前项目；开发会话固定在此目录执行）。
	Workspace string `json:"workspace,omitempty"`
	// SessionID 开发会话 ID（roadmap_<itemID>；面板点击跳转会话回放）。
	SessionID string `json:"sessionId,omitempty"`
	// BatchID 所属主任务批次（batch_<UnixMilli>；一次「执行」/一轮 AI 审核 =
	// 一个批次，批次内条目并行开发——用户需求 2026-10：并行开发需有主任务，
	// 子任务挂主任务下面展示。空 = 无批次（历史数据/单条重试））。
	BatchID string `json:"batchId,omitempty"`
	// ParentSessionID 主任务会话 ID（roadmap_batch_<ts>；开发子会话的 ParentID
	// 指向它，侧栏折叠树按此归组；空 = 平级独立会话）。
	ParentSessionID string `json:"parentSessionId,omitempty"`
	// Result 完成摘要（attempt_completion 结果截断）；Error 失败原因。
	// Review AI 审核意见（✅通过/✖驳回 + 理由；AI 审核留痕，面板展示）。
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
	Review string `json:"review,omitempty"`
	// Verification 验收方式（可执行验证命令或可核验检查点；开发任务的
	// 完成判据，收口时据此做机器验证声明核验——对齐 harness engineering
	// 「feature list 是 harness 原语」：条目必须有独立可核验的完成标准，
	// 生成者不给自己打分）。空 = 无显式验收（历史条目/纯文档类）。
	Verification string `json:"verification,omitempty"`
	// ApprovedBy 批准来源：user（人工勾选）| ai_review（AI 审核通过自动纳入排期）。
	ApprovedBy string `json:"approvedBy,omitempty"`
	// Attempts 开发尝试次数（每次认领开发 +1，含失败自动重试；重跑不清零——
	// 失败原因可追溯，超上限置 blocked 的判定依据，见 finishDevelopRun）。
	Attempts int `json:"attempts,omitempty"`
	// BlockedReason 连续失败暂停原因（StatusBlocked 时写入；重跑清除）。
	BlockedReason string `json:"blockedReason,omitempty"`
	// RunLog 执行轨迹（最新在前；单条开发一次流转记录一条：耗时/结论/摘要/文件）。
	RunLog     []RunEntry `json:"runLog,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	StartedAt  time.Time  `json:"startedAt,omitempty"`
	FinishedAt time.Time  `json:"finishedAt,omitempty"`
}

// RunEntry 单次开发执行日志（执行轨迹：耗时 + 结论 + 摘要 + 改动文件）。
type RunEntry struct {
	Outcome    string    `json:"outcome"`           // done | failed | blocked
	Summary    string    `json:"summary,omitempty"` // 结果摘要 / 失败原因（截断）
	Files      []string  `json:"files,omitempty"`   // 本次改动文件（会话写操作解析，相对路径）
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt"`
	DurationMS int64     `json:"durationMS,omitempty"`
}

// LatestRun 最近一次执行日志（空返回 nil；面板「最近执行」展示入口）。
func (it Item) LatestRun() *RunEntry {
	if len(it.RunLog) == 0 {
		return nil
	}
	return &it.RunLog[0]
}

// 执行日志限制（防 items.json 膨胀：单条 50 条日志 / 20 文件 / 路径 120 rune）。
const (
	maxRunLogEntries = 50
	maxRunFiles      = 20
	maxRunFileRunes  = 260
)

// Store 条目持久化（items.json + rejections.json，原子写盘；互斥锁保护内存态）。
type Store struct {
	dir        string
	mu         sync.Mutex
	items      []Item
	rejections []Rejection // 驳回环形日志（rejectDelete 删除后反馈仍可回注规划器）
}

// NewStore 创建存储并加载既有数据（目录不存在自动创建）。
func NewStore(dir string) (*Store, error) {
	s := &Store{dir: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s.load()
	return s, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, "items.json") }

func (s *Store) load() {
	if b, err := os.ReadFile(s.path()); err == nil {
		_ = json.Unmarshal(b, &s.items)
	}
	if b, err := os.ReadFile(s.rejectionsPath()); err == nil {
		_ = json.Unmarshal(b, &s.rejections)
	}
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "rm_" + hex.EncodeToString(b)
}

// List 全部条目（创建时间正序：老需求在前，符合"排队开发"直觉）。
func (s *Store) List() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Item, len(s.items))
	copy(out, s.items)
	return out
}

// Get 按 ID 读取条目。
func (s *Store) Get(id string) (Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		if it.ID == id {
			return it, true
		}
	}
	return Item{}, false
}

// Add 新增条目（规范化字段；ID/时间戳自动生成；状态默认 proposed）。
func (s *Store) Add(it Item) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	it.ID = newID()
	it.Title = clampRunes(strings.TrimSpace(it.Title), maxTitleRunes)
	it.Detail = clampRunes(strings.TrimSpace(it.Detail), maxDetailRunes)
	it.Verification = clampRunes(strings.TrimSpace(it.Verification), maxDetailRunes)
	it.Priority = normalizePriority(it.Priority)
	it.Category = strings.TrimSpace(it.Category)
	if it.Title == "" {
		return it, errItemEmpty
	}
	if it.Status == "" {
		it.Status = StatusProposed
	}
	it.Source = strings.TrimSpace(it.Source)
	it.CreatedAt = now
	it.UpdatedAt = now
	s.items = append(s.items, it)
	err := s.evictIfNeededLocked()
	if err == nil {
		err = s.saveLocked()
	}
	return it, err
}

// Update 整体更新（按 ID 定位；保留不可变字段 ID/CreatedAt）。
func (s *Store) Update(it Item) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == it.ID {
			kept := s.items[i] // 不可变字段保护
			it.ID = kept.ID
			if it.CreatedAt.IsZero() {
				it.CreatedAt = kept.CreatedAt
			}
			it.Title = clampRunes(strings.TrimSpace(it.Title), maxTitleRunes)
			it.Detail = clampRunes(strings.TrimSpace(it.Detail), maxDetailRunes)
			it.Verification = clampRunes(strings.TrimSpace(it.Verification), maxDetailRunes)
			it.Result = clampRunes(strings.TrimSpace(it.Result), maxResultRunes)
			it.Error = clampRunes(strings.TrimSpace(it.Error), 600)
			it.UpdatedAt = time.Now()
			s.items[i] = it
			return it, s.saveLocked()
		}
	}
	return it, os.ErrNotExist
}

// SetSession 写入开发会话 ID（roadmapDevelopLoop 拉起会话时调用；只改 SessionID
// 不触碰其他字段 —— Store.Update 是整体替换，用部分更新防字段被清空）。
func (s *Store) SetSession(id, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].SessionID = strings.TrimSpace(sessionID)
			s.items[i].UpdatedAt = time.Now()
			return s.saveLocked()
		}
	}
	return os.ErrNotExist
}

// AttachBatch 把条目归入开发批次（部分更新：只写 BatchID/ParentSessionID，
// 其余字段不动）。StartRoadmapDev 批量批准 / AI 审核批量通过时调用——同一批
// 条目共享一个主任务会话，并行子会话挂其下（侧栏树层级依据）。
func (s *Store) AttachBatch(id, batchID, parentSessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].BatchID = strings.TrimSpace(batchID)
			s.items[i].ParentSessionID = strings.TrimSpace(parentSessionID)
			s.items[i].UpdatedAt = time.Now()
			return s.saveLocked()
		}
	}
	return os.ErrNotExist
}

// SetReview 写入 AI 审核意见与批准来源（审核留痕；非状态变更不触发状态机校验）。
// status 非空时同步流转（仅 proposed→approved/rejected 合法路径）。
func (s *Store) SetReview(id, review, status, approvedBy string) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID != id {
			continue
		}
		it := &s.items[i]
		it.Review = clampRunes(strings.TrimSpace(review), maxDetailRunes)
		if status != "" {
			if !CanTransition(it.Status, status) {
				return *it, simpleErr("非法状态流转: " + it.Status + " → " + status)
			}
			it.Status = status
			it.ApprovedBy = approvedBy
		}
		it.UpdatedAt = time.Now()
		return *it, s.saveLocked()
	}
	return Item{}, os.ErrNotExist
}

// SetStatus 状态流转（ approved/done/failed 等变更即落盘；附带结果/错误）。
func (s *Store) SetStatus(id, status, result, errMsg string) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID != id {
			continue
		}
		it := &s.items[i]
		now := time.Now()
		switch status {
		case StatusDeveloping:
			// 认领防重（同一临界区内校验）：仅 approved 可进入 developing。
			// 并发调度方（手动执行/AI 审核/autoDev 心跳/完成钩子）同时认领同
			// 一条目时，第一个流转成功后其余立即被拒 —— 同一条目不被并发
			// 重复开发（配合面板层 roadmapDevMu 双保险）。
			if it.Status != StatusApproved {
				return *it, simpleErr("非法状态流转: " + it.Status + " → " + status)
			}
			it.StartedAt = now
			it.Error = ""
			it.Attempts++ // 每次认领开发计入尝试次数（含失败自动重试）
		case StatusDone:
			it.FinishedAt = now
			it.Result = clampRunes(strings.TrimSpace(result), maxResultRunes)
			s.appendRunLocked(it, "done", result, nil)
		case StatusFailed:
			it.FinishedAt = now
			it.Error = clampRunes(strings.TrimSpace(errMsgOr(result, errMsg)), 600)
			s.appendRunLocked(it, "failed", errMsgOr(result, errMsg), nil)
		case StatusBlocked:
			if it.Status != StatusDeveloping {
				return *it, simpleErr("非法状态流转: " + it.Status + " → " + status)
			}
			it.FinishedAt = now
			it.Error = clampRunes(strings.TrimSpace(errMsgOr(result, errMsg)), 600)
			it.BlockedReason = it.Error
			s.appendRunLocked(it, "blocked", errMsgOr(result, errMsg), nil)
		default:
			// proposed/approved/rejected：仅状态变更（人工审核/回拨路径），
			// 校验状态机合法性防非法跳变。
			if !CanTransition(it.Status, status) {
				return *it, simpleErr("非法状态流转: " + it.Status + " → " + status)
			}
		}
		it.Status = status
		it.UpdatedAt = now
		return *it, s.saveLocked()
	}
	return Item{}, os.ErrNotExist
}

// appendRunLocked 追加一条执行日志（最新在前；超出上限裁剪；空摘要也记，
// 耗时/结论本身即有效轨迹。持久化在调用方 saveLocked 统一落盘）。
func (s *Store) appendRunLocked(it *Item, outcome, summary string, files []string) {
	e := RunEntry{
		Outcome:    outcome,
		Summary:    clampRunes(strings.TrimSpace(summary), 600),
		Files:      normalizeRunFiles(files),
		StartedAt:  it.StartedAt,
		FinishedAt: time.Now(),
	}
	if !e.StartedAt.IsZero() {
		e.DurationMS = e.FinishedAt.Sub(e.StartedAt).Milliseconds()
	}
	it.RunLog = append([]RunEntry{e}, it.RunLog...)
	if len(it.RunLog) > maxRunLogEntries {
		it.RunLog = it.RunLog[:maxRunLogEntries]
	}
}

// normalizeRunFiles 改动文件清单清洗：去空、去重、截断路径、限制条数。
func normalizeRunFiles(files []string) []string {
	if len(files) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(files))
	out := make([]string, 0, len(files))
	for _, f := range files {
		f = clampRunes(strings.TrimSpace(f), maxRunFileRunes)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
		if len(out) >= maxRunFiles {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AppendRunFiles 给最新一条执行日志补写改动文件清单（部分更新：仅碰
// RunLog[0]；异步收口路径先 SetStatus 后补文件，两次落盘均原子）。
func (s *Store) AppendRunFiles(id string, files []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID != id {
			continue
		}
		it := &s.items[i]
		if len(it.RunLog) == 0 || len(files) == 0 {
			return nil // 无日志可补 / 无文件可记：幂等不报错
		}
		it.RunLog[0].Files = normalizeRunFiles(append(it.RunLog[0].Files, files...))
		it.UpdatedAt = time.Now()
		return s.saveLocked()
	}
	return os.ErrNotExist
}

// MarkRetry 人工重跑回队（failed/blocked → approved）：清错误与 blocked 封存
// 原因（执行日志保留，尝试次数不清零——失败原因可追溯，重跑后继续累计）。
func (s *Store) MarkRetry(id string) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID != id {
			continue
		}
		it := &s.items[i]
		if it.Status != StatusFailed && it.Status != StatusBlocked {
			return *it, simpleErr("仅失败/暂停条目可重跑: " + it.Status)
		}
		it.Status = StatusApproved
		it.Error = ""
		it.BlockedReason = ""
		it.FinishedAt = time.Time{}
		it.UpdatedAt = time.Now()
		return *it, s.saveLocked()
	}
	return Item{}, os.ErrNotExist
}

// Delete 删除条目。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			s.items = append(s.items[:i], s.items[i+1:]...)
			return s.saveLocked()
		}
	}
	return os.ErrNotExist
}

// DeleteMany 批量删除条目（用户需求 2026-10：「已完成」批量清理入口）。
// 开发中条目跳过（先 Stop 再删由面板层负责），其余存在即删；返回实际删除数。
func (s *Store) DeleteMany(ids []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	del := make(map[string]bool, len(ids))
	for _, id := range ids {
		del[id] = true
	}
	kept := s.items[:0]
	n := 0
	for _, it := range s.items {
		if del[it.ID] && it.Status != StatusDeveloping {
			n++
			continue
		}
		kept = append(kept, it)
	}
	if n == 0 {
		return 0
	}
	s.items = kept
	_ = s.saveLocked()
	return n
}

// Rejection 一条驳回反馈（环形日志条目）。RejectDelete 直接删除条目后
// store 里已无 rejected 条目可扫——驳回理由必须独立持久化，规划器才有
// 负反馈可用（「反复提同类提案被反复驳回」的根治点，Reflexion 闭环）。
type Rejection struct {
	Title  string    `json:"title"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

const maxRejections = 20 // 环形上限（防 rejections.json 膨胀）

func (s *Store) rejectionsPath() string { return filepath.Join(s.dir, "rejections.json") }

// AppendRejection 追加一条驳回反馈（持久化；超上限淘汰最旧。尽力而为：
// 写失败不影响审核主流程）。
func (s *Store) AppendRejection(title, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejections = append(s.rejections, Rejection{
		Title: clampRunes(title, maxTitleRunes), Reason: clampRunes(reason, 200),
		At: time.Now(),
	})
	if len(s.rejections) > maxRejections {
		s.rejections = s.rejections[len(s.rejections)-maxRejections:]
	}
	if b, err := json.MarshalIndent(s.rejections, "", "  "); err == nil {
		_ = os.WriteFile(s.rejectionsPath(), b, 0o600)
	}
}

// RejectionFeedback 最近 n 条驳回反馈文案（最新在前：标题——理由）。
func (s *Store) RejectionFeedback(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for i := len(s.rejections) - 1; i >= 0 && len(out) < n; i-- {
		r := s.rejections[i]
		out = append(out, "「"+r.Title+"」——"+r.Reason)
	}
	return out
}

// RequeueDeveloping 把遗留 developing 条目置回 approved 重新入队
// （重启恢复用；不走 SetStatus 状态机校验——developing→approved 在
// CanTransition 中非法，旧实现恢复永远失败，遗留僵尸条目）。
func (s *Store) RequeueDeveloping() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.items {
		if s.items[i].Status == StatusDeveloping {
			s.items[i].Status = StatusApproved
			s.items[i].UpdatedAt = time.Now()
			n++
		}
	}
	if n > 0 {
		_ = s.saveLocked()
	}
	return n
}

// Count 统计各状态条目数（面板徽标）。
func (s *Store) Count() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{"total": len(s.items)}
	for _, it := range s.items {
		out[it.Status]++
	}
	return out
}

// evictIfNeededLocked 条目超限时淘汰最旧的终态条目（done/failed/blocked/rejected），
// 活跃条目（proposed/approved/developing）不淘汰。
func (s *Store) evictIfNeededLocked() error {
	for len(s.items) > maxItems {
		victim := -1
		for i := range s.items {
			switch s.items[i].Status {
			case StatusDone, StatusFailed, StatusBlocked, StatusRejected:
				if victim < 0 || s.items[i].CreatedAt.Before(s.items[victim].CreatedAt) {
					victim = i
				}
			}
		}
		if victim < 0 {
			return nil // 无终态可淘汰（理论上不可能：活跃条目远少于上限）
		}
		s.items = append(s.items[:victim], s.items[victim+1:]...)
	}
	return nil
}

// normalizePriority 优先级归一（非法值回退 medium）。
func normalizePriority(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case PriorityHigh:
		return PriorityHigh
	case PriorityLow:
		return PriorityLow
	default:
		return PriorityMedium
	}
}

// errMsgOr 优先 errMsg，其次 result，都为空时给默认错误文案。
func errMsgOr(result, errMsg string) string {
	if s := strings.TrimSpace(errMsg); s != "" {
		return s
	}
	if s := strings.TrimSpace(result); s != "" {
		return s
	}
	return "开发任务异常结束（无错误详情）"
}

func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type simpleErr string

func (e simpleErr) Error() string { return string(e) }

const errItemEmpty = simpleErr("规划条目标题不能为空")
