package cron

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Job 一个定时任务定义。
type Job struct {
	ID        string `json:"id"`
	Name      string `json:"name"`      // 任务名（列表展示）
	Expr      string `json:"expr"`      // 5 段 cron 表达式或 @ 描述符
		// Action 任务动作（空 = 提示词任务，存量语义不变；run_flow = 到点触发
		// 指定流程）。经 FlowLauncher 回调启动（flow 包不反向依赖 cron，内核零耦合）。
		Action string `json:"action,omitempty"`
		// ---- 本批 F1/F3/F4：脚本 / no-agent / 技能挂载（对标 Hermes --script / no_agent / --skill）----
		// Script 前置脚本路径（空 = 无脚本）。非空且非 NoAgent 时，每次执行前先跑脚本，
		// 其 stdout/stderr 注入为任务上下文（[script output]/[script stderr] 段），供模型推理。
		Script string `json:"script,omitempty"`
		// ScriptTimeoutSec 脚本超时（秒；0 = 默认 30；超时按脚本失败处理但不阻断 agent 任务）。
		ScriptTimeoutSec int `json:"scriptTimeoutSec,omitempty"`
		// ScriptArgs 脚本命令行参数（可选，空格分隔；随脚本路径传给执行器）。
		ScriptArgs string `json:"scriptArgs,omitempty"`
		// NoAgent 纯脚本模式（对齐 Hermes no_agent）：true 时任务只运行 Script，
		// 脚本 stdout verbatim 作为结果，不构造 agent 会话、零 LLM。
		NoAgent bool `json:"noAgent,omitempty"`
		// Skills 任务挂载的技能名列表（对齐 Hermes --skill；空 = 不挂载）。
		// cron 会话构建时把绑定的技能预置为可发现（available skills 段）。
		Skills []string `json:"skills,omitempty"`
	// FlowID Action=run_flow 时要触发的流程 ID（~/.kaelvoris/flows/<id>.json）。
	FlowID string `json:"flowId,omitempty"`
	Workspace string `json:"workspace"` // 执行工作区（空 = 当前项目）
	Mode      string `json:"mode"`      // 执行模式 slug（空 = 默认）
	Task      string `json:"task"`      // 提示词（送入引擎的任务内容）
	Enabled   bool   `json:"enabled"`
	// TimeoutMin 单次运行超时（分钟；0 = 默认 30；对齐 GitHub Actions
	// timeout-minutes：超时后解除重入占用，允许下次到期再触发）。
	TimeoutMin int `json:"timeoutMin,omitempty"`
	// RetryLimit 失败自动重试次数（0 = 不重试；上限 MaxRetryLimit；
	// 对齐 GitHub Actions retry：按 RetryBackoff 退避自动重试）。
	RetryLimit int `json:"retryLimit,omitempty"`
	// Model 任务级大模型（触发时的 LLM 覆盖；空 = 跟随全局配置，
	// 与 Workspace/Mode 同语义）。触发时同步到 cron_<jobID> 会话，
	// 经会话自有模型覆盖链路生效（runTask: run.model → client.Model）。
	Model string `json:"model,omitempty"`
	// ---- 员工岗位绑定（岗位分配×双区权限，doc/09-设计方案 §3）----
	// EmployeeID 绑定的数字员工（空 = 未指定员工；与 PositionID 至少
	// 其一组成绑定；全空 = 存量裸跑语义，不强制迁移，见 §2 分支④）。
	EmployeeID string `json:"employeeId,omitempty"`
	// PositionID 绑定岗位快照：绑定时回填员工当前岗位（§3 调岗不自动
	// 跟随）；仅绑岗位时执行按 AssignRule 从岗位池选取员工。
	PositionID string `json:"positionId,omitempty"`
	// AssignRule 岗位池分配规则：random（默认）/ round_robin /
	// least_loaded（二期枚举位）；空值 = random（§2.1）。存资源项
	// 配置侧，解析结果随快照写入会话。
	AssignRule string `json:"assignRule,omitempty"`
	// LastAssignedIndex round_robin 轮转游标（§2.1：存资源项配置内，
	// 随 jobs.json 原子落盘；重启丢内存态可接受）。0 = 尚未分配（零值
	// 兼容旧数据）；round_robin 分配时取 (last+1)%len(池) 并回写。
	LastAssignedIndex int `json:"lastAssignedIndex,omitempty"`
	// ---- 运行时间窗口（用户需求：指定时间范围内才可正常运行）----
	// TimeWindowEnabled 开启后仅允许 StartHHMM～EndHHMM（支持跨零点）时段
	// 内触发；到期但不在窗口内则记 skipped 不执行（RunNow 手动触发不受限）。
	TimeWindowEnabled bool   `json:"timeWindowEnabled,omitempty"`
	StartTime         string `json:"startTime,omitempty"` // "HH:MM"，默认 18:00
	EndTime           string `json:"endTime,omitempty"`   // "HH:MM"，默认 06:00
	// ---- 以下为运行时状态（调度器维护，随 jobs.json 持久化）----
	// FailStreak 连续失败计数（>= MaxFailStreak 自动熔断置 DisabledReason）。
	FailStreak int `json:"failStreak,omitempty"`
	// DisabledReason 熔断原因（非空 = 因连续失败被自动停用）。
	DisabledReason string    `json:"disabledReason,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	LastRun        time.Time `json:"lastRun,omitempty"` // 最近一次触发（零值 = 未运行过）
	NextRun        time.Time `json:"nextRun,omitempty"` // 下次触发（调度器维护）
	// NextRetry 下次重试时间（上次失败且配置了重试时；零值 = 无待重试）。
	NextRetry time.Time `json:"nextRetry,omitempty"`
}

	// Run 一次执行记录。
	type Run struct {
		JobID     string    `json:"jobId"`
		JobName   string    `json:"jobName"`
		SessionID string    `json:"sessionId"` // 触发产生的会话（点击可回放）
		StartedAt time.Time `json:"startedAt"`
		Status    string    `json:"status"` // running / done / failed / skipped / timeout / circuit_open / silent
	Error     string    `json:"error,omitempty"`
	// Attempt 重试轮次（1 = 首次；>1 = 失败重试）。
	Attempt int `json:"attempt,omitempty"`
	// DurationMs 运行时长（毫秒；成功/失败/超时均记录）。
	DurationMs int64 `json:"durationMs,omitempty"`
}

	// 任务动作枚举（Action 字段；空值 = 提示词任务，存量语义）。
	const ActionRunFlow = "run_flow" // 到点触发指定 flow（经 FlowLauncher 回调启动）

	// 运行状态枚举（Run.Status 的补充值）。
	const StatusSilent = "silent" // 成功但仅输出 [SILENT]：不推送完成/失败通知（对标 Hermes [SILENT]）

// Store 任务定义与执行记录的持久化（jobs.json / runs.json，直接落盘）。
type Store struct {
	dir string
	mu  sync.Mutex

	jobs []Job
	runs []Run // 追加；读取时倒序截断
}

const maxRuns = 200 // 执行记录上限（超出丢弃最旧）

// NewStore 创建存储并加载既有数据。
func NewStore(dir string) (*Store, error) {
	s := &Store{dir: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s.load(&s.jobs, s.jobsPath())
	s.load(&s.runs, s.runsPath())
	return s, nil
}

func (s *Store) jobsPath() string { return filepath.Join(s.dir, "jobs.json") }
func (s *Store) runsPath() string { return filepath.Join(s.dir, "runs.json") }

func (s *Store) load(v any, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, v)
}

func (s *Store) saveLocked(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// newID 生成随机任务 ID。
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "cron_" + hex.EncodeToString(b)
}

// List 全部任务（按创建时间正序）。
func (s *Store) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, len(s.jobs))
	copy(out, s.jobs)
	return out
}

// Add 新增任务（自动生成 ID）。
func (s *Store) Add(j Job) (Job, error) {
	now := time.Now()
	j.ID = newID()
	j.CreatedAt = now
	j.UpdatedAt = now
	if err := validateJob(j); err != nil {
		return j, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, j)
	return j, s.saveLocked(s.jobsPath(), s.jobs)
}

// Update 更新任务（按 ID 覆盖可变字段）。
func (s *Store) Update(j Job) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.jobs {
		if s.jobs[i].ID == j.ID {
			if err := validateJob(j); err != nil {
				return j, err
			}
			kept := s.jobs[i]
			j.CreatedAt = kept.CreatedAt
			j.UpdatedAt = time.Now()
			j.LastRun = kept.LastRun
			j.NextRun = kept.NextRun
			s.jobs[i] = j
			return j, s.saveLocked(s.jobsPath(), s.jobs)
		}
	}
	return j, os.ErrNotExist
}

// Delete 删除任务（同时清除该任务的执行记录：前端删除确认与验收均要求
// 删除后 cron store 无残留，runs.json 不再保留已被删除任务的记录）。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.jobs {
		if s.jobs[i].ID == id {
			s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
			// 过滤掉该任务的执行记录（复用底层数组避免二次分配）。
			kept := s.runs[:0]
			runsChanged := false
			for _, r := range s.runs {
				if r.JobID == id {
					runsChanged = true
					continue
				}
				kept = append(kept, r)
			}
			s.runs = kept
			if err := s.saveLocked(s.jobsPath(), s.jobs); err != nil {
				return err
			}
			if runsChanged {
				return s.saveLocked(s.runsPath(), s.runs)
			}
			return nil
		}
	}
	return os.ErrNotExist
}

// SetEnabled 启停任务。
func (s *Store) SetEnabled(id string, enabled bool) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.jobs {
		if s.jobs[i].ID == id {
			s.jobs[i].Enabled = enabled
			s.jobs[i].UpdatedAt = time.Now()
			// 重新启用时清零熔断状态（用户已排查，给任务重新开始的机会）。
			if enabled {
				s.jobs[i].FailStreak = 0
				s.jobs[i].DisabledReason = ""
				s.jobs[i].NextRetry = time.Time{}
			}
			return s.jobs[i], s.saveLocked(s.jobsPath(), s.jobs)
		}
	}
	return Job{}, os.ErrNotExist
}

// RecordFailure 记录一次失败：累加连败计数；达熔断阈值自动停用任务。
// 返回更新后的任务与是否触发熔断。
func (s *Store) RecordFailure(jobID string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.jobs {
		if s.jobs[i].ID != jobID {
			continue
		}
		s.jobs[i].FailStreak++
		if s.jobs[i].FailStreak >= MaxFailStreak {
			s.jobs[i].Enabled = false
			s.jobs[i].DisabledReason = fmt.Sprintf("连续失败 %d 次已自动熔断停用，请排查后重新启用", s.jobs[i].FailStreak)
			s.jobs[i].UpdatedAt = time.Now()
			_ = s.saveLocked(s.jobsPath(), s.jobs)
			return s.jobs[i], true
		}
		_ = s.saveLocked(s.jobsPath(), s.jobs)
		return s.jobs[i], false
	}
	return Job{}, false
}

// ClearFailure 任务成功后清零连败计数与待重试时间。
func (s *Store) ClearFailure(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.jobs {
		if s.jobs[i].ID == jobID {
			if s.jobs[i].FailStreak != 0 || !s.jobs[i].NextRetry.IsZero() {
				s.jobs[i].FailStreak = 0
				s.jobs[i].NextRetry = time.Time{}
				s.jobs[i].UpdatedAt = time.Now()
				_ = s.saveLocked(s.jobsPath(), s.jobs)
			}
			return
		}
	}
}

// SetLastRunForTest 测试辅助：直接设置 LastRun（@every 到期推算用）。
// 生产代码不经此路径（LastRun 由 MarkRun 维护）。
func (s *Store) SetLastRunForTest(jobID string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.jobs {
		if s.jobs[i].ID == jobID {
			s.jobs[i].LastRun = at
			_ = s.saveLocked(s.jobsPath(), s.jobs)
			return
		}
	}
}

// SetNextRetry 设置下次重试时间（调度器失败重试用；标准式重试到期时
// 由调度器按 NextRetry 触发一次并清除）。
func (s *Store) SetNextRetry(jobID string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.jobs {
		if s.jobs[i].ID == jobID {
			s.jobs[i].NextRetry = at
			_ = s.saveLocked(s.jobsPath(), s.jobs)
			return
		}
	}
}

// MarkRun 更新任务运行标记并追加执行记录。
// 任务已不存在（被删除）时不追加记录：runJob 是异步收尾，删除后完成时
// 不应再向 runs.json 写回已被删除任务的残留记录。
func (s *Store) MarkRun(jobID string, at time.Time, run Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for i := range s.jobs {
		if s.jobs[i].ID == jobID {
			s.jobs[i].LastRun = at
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	s.runs = append(s.runs, run)
	if len(s.runs) > maxRuns {
		s.runs = s.runs[len(s.runs)-maxRuns:]
	}
	if err := s.saveLocked(s.jobsPath(), s.jobs); err != nil {
		return err
	}
	return s.saveLocked(s.runsPath(), s.runs)
}

// UpdateNextRun 批量更新下次触发时间（调度器 tick 维护，不追加记录）。
func (s *Store) UpdateNextRun(next map[string]time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for i := range s.jobs {
		if t, ok := next[s.jobs[i].ID]; ok && !s.jobs[i].NextRun.Equal(t) {
			s.jobs[i].NextRun = t
			changed = true
		}
	}
	if changed {
		_ = s.saveLocked(s.jobsPath(), s.jobs)
	}
}

// Get 读取单个任务。
func (s *Store) Get(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			return j, true
		}
	}
	return Job{}, false
}

// RecentRuns 最近 n 条执行记录（倒序）。
func (s *Store) RecentRuns(n int) []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Run, 0, n)
	for i := len(s.runs) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, s.runs[i])
	}
	return out
}

// validateJob 任务定义合法性（表达式可解析 + 提示词非空 + 参数界内 +
// 绑定组合校验）。
func validateJob(j Job) error {
	if _, err := Parse(j.Expr); err != nil {
		return err
	}
	switch j.Action {
	case "", ActionRunFlow:
	default:
		return simpleErr(fmt.Sprintf("未知任务动作 %q（允许空 = 提示词任务 / %s）", j.Action, ActionRunFlow))
	}
	if j.Action == ActionRunFlow {
		// run_flow 动作：流程 ID 必填，提示词不适用（启动入口按 FlowID 加载流程）。
		if strings.TrimSpace(j.FlowID) == "" {
			return errFlowIDEmpty
		}
	} else if j.NoAgent {
		// no-agent 纯脚本模式：必须有脚本（否则无语义——不构造 agent 会话又无脚本可跑）。
		if strings.TrimSpace(j.Script) == "" {
			return errNoAgentNoScript
		}
	} else if j.Task == "" {
		return errTaskEmpty
	}
	if j.TimeoutMin < 0 || j.TimeoutMin > 24*60 {
		return simpleErr("超时时间需在 0～1440 分钟之间（0 = 默认 30 分钟）")
	}
	if j.RetryLimit < 0 || j.RetryLimit > MaxRetryLimit {
		return simpleErr(fmt.Sprintf("重试次数需在 0～%d 之间", MaxRetryLimit))
	}
	if err := ValidateBinding(j.EmployeeID, j.PositionID, j.AssignRule); err != nil {
		return err
	}
	return validateTimeWindow(j)
}

// InTimeWindow 判断 t 是否落在任务允许的运行时间窗口内。
// 未开启窗口恒为 true；start==end 视为全天；start>end 视为跨零点区间
// （如 18:00～06:00：从当天 18:00 到次日 06:00）。
func (j Job) InTimeWindow(t time.Time) bool {
	if !j.TimeWindowEnabled {
		return true
	}
	s, e, ok := parseWindowBounds(j.StartTime, j.EndTime)
	if !ok {
		return true
	}
	m := t.Hour()*60 + t.Minute()
	if s == e {
		return true
	}
	if s < e {
		return m >= s && m < e
	}
	// 跨零点：22:00～06:00 = m>=22:00 或 m<06:00
	return m >= s || m < e
}

// parseWindowBounds 解析 "HH:MM" 为分钟数；任一非法时回退默认边界
// （默认 18:00～06:00，与前端表单默认一致）。
func parseWindowBounds(start, end string) (int, int, bool) {
	p := func(v string, def int) (int, bool) {
		parts := strings.Split(strings.TrimSpace(v), ":")
		if len(parts) != 2 {
			return def, false
		}
		h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return def, false
		}
		return h*60 + m, true
	}
	s, ok1 := p(start, 18*60)
	e, ok2 := p(end, 6*60)
	return s, e, ok1 || ok2
}

// validateTimeWindow 运行时间窗口合法性（开启时起止必须为合法 HH:MM）。
func validateTimeWindow(j Job) error {
	if !j.TimeWindowEnabled {
		return nil
	}
	if _, _, ok := parseWindowBounds(j.StartTime, j.EndTime); !ok {
		return simpleErr("运行时间窗口格式非法，应为 HH:MM（如 18:00 / 06:00）")
	}
	return nil
}

// 分配规则枚举（doc/09-设计方案 §2.1：默认 random；round_robin 本期实现；
// least_loaded 二期仅留枚举位）。空值语义等价 random，由解析器兜底。
const (
	AssignRuleRandom      = "random"
	AssignRuleRoundRobin  = "round_robin"
	AssignRuleLeastLoaded = "least_loaded"
)

// ValidateBinding 员工岗位绑定组合校验（doc/09-设计方案 §4.4：
// AddCronJob/UpdateCronJob「员工与岗位组合校验；指定员工须有岗位」中
// 不依赖员工档案的数据组合判定部分，供上层 cron_panel.go 保存链路接线
// 复用；岗位存在性/员工可运行性等档案侧校验由上层负责，包内不引依赖）。
//
// 规则（对齐 §2 ResolveAssignment 入参形态）：
//   - 全空 = 存量裸跑（§2 分支④），合法不拦；但不得携带分配规则；
//   - assignRule 仅允许空 / random / round_robin / least_loaded；
//   - 指定员工时岗位必填（§3：绑定时回填员工当前岗位快照，必随绑定落库）。
//
// 形参化纯函数：不读存储不依赖运行态，Add/Update 经 validateJob 自动生效。
func ValidateBinding(employeeID, positionID, assignRule string) error {
	if employeeID == "" && positionID == "" {
		if assignRule != "" {
			return simpleErr("未绑定员工与岗位时不能指定分配规则")
		}
		return nil
	}
	switch assignRule {
	case "", AssignRuleRandom, AssignRuleRoundRobin, AssignRuleLeastLoaded:
	default:
		return simpleErr(fmt.Sprintf("非法分配规则 %q（允许 random / round_robin / least_loaded）", assignRule))
	}
	if employeeID != "" && positionID == "" {
		return simpleErr("绑定员工时必须同时指定岗位（保存时回填员工当前岗位快照）")
	}
	return nil
}

type simpleErr string

func (e simpleErr) Error() string { return string(e) }

const errTaskEmpty = simpleErr("任务提示词不能为空")

	const errFlowIDEmpty = simpleErr("run_flow 动作需指定流程 ID（flowId）")

	const errNoAgentNoScript = simpleErr("no-agent 纯脚本任务必须配置 script（脚本路径）")
