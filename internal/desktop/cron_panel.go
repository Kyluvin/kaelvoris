// cron_panel.go —— 记忆面板 + 定时任务面板 + 总任务面板（Wails 绑定）。
//
// 三块数据面：
//   - 记忆：GetMemory/SaveMemory（会话 MEMORY.md 的查看与编辑保存）
//   - 定时任务：cron 任务 CRUD / 启停 / 立即运行 / 执行记录（internal/cron 调度器）
//   - 总任务：ListAllTasks 聚合全部会话（标题/运行态/待办进度）供侧栏总面板
//
// 定时任务触发复用现有执行链：RunCronJob → SendMessage（经 TaskScheduler 门控，
// 事件以 sessionId 路由到前端对应会话）。
package desktop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"kaelvoris/internal/cron"
	"kaelvoris/internal/employee"
	"kaelvoris/internal/evolve"
	"kaelvoris/pkg/message"
)

// hourlyReviewJobName 一键创建的每小时复盘任务名（防重复创建的判定依据）。
const hourlyReviewJobName = "每小时会话复盘"

// ---------- 记忆面板 ----------

// GetMemory 读取会话记忆文档（MEMORY.md 全文）。
func (a *App) GetMemory(sessionID string) (map[string]any, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("sessionID 不能为空")
	}
	return map[string]any{
		"sessionId": sessionID,
		"content":   a.memory.Load(sessionID),
		"path":      a.memory.Path(sessionID),
	}, nil
}

// SaveMemory 覆盖保存会话记忆文档（面板编辑保存；原子写盘，下任务生效）。
func (a *App) SaveMemory(sessionID, content string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sessionID 不能为空")
	}
	if err := a.memory.Save(sessionID, content); err != nil {
		return fmt.Errorf("保存记忆失败: %w", err)
	}
	return nil
}

// GetWorkspaceMemory 读取项目工作空间记忆（跨会话共享；workspace 为空 =
// 跟随全局配置工作区）。返回 {workspace, content, path}。
func (a *App) GetWorkspaceMemory(workspace string) (map[string]any, error) {
	ws := strings.TrimSpace(workspace)
	if ws == "" {
		ws = a.cfgWorkspace()
	}
	if ws == "" {
		return nil, errors.New("工作区未设置，无法读取工作空间记忆")
	}
	return map[string]any{
		"workspace": ws,
		"content":   a.wsMemory.Load(ws),
		"path":      a.wsMemory.Path(ws),
	}, nil
}

// SaveWorkspaceMemory 覆盖保存项目工作空间记忆（面板编辑保存；原子写盘，
// 下任务生效；同一项目下所有会话共享）。
func (a *App) SaveWorkspaceMemory(workspace, content string) error {
	ws := strings.TrimSpace(workspace)
	if ws == "" {
		ws = a.cfgWorkspace()
	}
	if ws == "" {
		return errors.New("工作区未设置，无法保存工作空间记忆")
	}
	if err := a.wsMemory.Save(ws, content); err != nil {
		return fmt.Errorf("保存工作空间记忆失败: %w", err)
	}
	return nil
}

// ---------- 定时任务面板 ----------

// cronJobView 定时任务列表条目（附中文描述与下次触发）。
type cronJobView struct {
	cron.Job
	Desc string `json:"desc"` // 表达式中文描述
}

// pushCron 推送任务列表到前端（kaelvoris:cron）。
func (a *App) pushCron() {
	if a.ctx == nil {
		return
	}
	uiEmit(a.ctx, "kaelvoris:cron", a.cronViews())
}

// cronViews 内部：任务列表 + 描述。
func (a *App) cronViews() []cronJobView {
	jobs := a.cronStore.List()
	out := make([]cronJobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, cronJobView{Job: j, Desc: cron.Describe(j.Expr)})
	}
	return out
}

// ListCronJobs 定时任务列表。
func (a *App) ListCronJobs() []cronJobView { return a.cronViews() }

// AddCronJob 新增定时任务。
func (a *App) AddCronJob(name, expr, workspace, mode, task string, enabled bool) (cron.Job, error) {
	j, err := a.cronStore.Add(cron.Job{
		Name:      strings.TrimSpace(name),
		Expr:      strings.TrimSpace(expr),
		Workspace: strings.TrimSpace(workspace),
		Mode:      strings.TrimSpace(mode),
		Task:      task,
		Enabled:   enabled,
	})
	if err == nil {
		a.pushCron()
	}
	return j, err
}

// UpdateCronJob 更新定时任务。
func (a *App) UpdateCronJob(job cron.Job) (cron.Job, error) {
	if strings.TrimSpace(job.ID) == "" {
		return job, errors.New("任务 ID 不能为空")
	}
	j, err := a.cronStore.Update(job)
	if err == nil {
		a.pushCron()
	}
	return j, err
}

// DeleteCronJob 删除定时任务。
func (a *App) DeleteCronJob(id string) error {
	err := a.cronStore.Delete(id)
	if err == nil {
		a.pushCron()
	}
	return err
}

// ToggleCronJob 启停定时任务。
func (a *App) ToggleCronJob(id string, enabled bool) (cron.Job, error) {
	j, err := a.cronStore.SetEnabled(id, enabled)
	if err == nil {
		a.pushCron()
	}
	return j, err
}

// RunCronJobNow 立即触发一次（防重入；记录执行）。
func (a *App) RunCronJobNow(id string) error {
	if _, err := a.cronSched.RunNow(id); err != nil {
		return err
	}
	a.pushCron()
	return nil
}

// ListCronRuns 最近执行记录（默认 30 条，倒序）。
func (a *App) ListCronRuns() []cron.Run {
	runs := a.cronStore.RecentRuns(30)
	if runs == nil {
		runs = []cron.Run{}
	}
	return runs
}

// DescribeCron 解析表达式并返回中文描述（编辑器实时预览；非法表达式返回错误）。
func (a *App) DescribeCron(expr string) (string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", nil
	}
	if _, err := cron.Parse(expr); err != nil {
		return "", err
	}
	return cron.Describe(expr), nil
}

// ---------- 自进化：定时自检任务 ----------

// selfReviewTaskPrompt 定时自检任务的内置提示词模板：要求 Agent 复盘近期
// 会话失误/纠偏，调用 evolve_record 沉淀经验卡（自进化闭环第 1 步）。
const selfReviewTaskPrompt = `【定时自检】复盘本工作区近期任务，把可复用的经验沉淀为经验卡：
1. 查看项目近期改动（git log/diff）与当前代码质量薄弱点；
2. 提炼 1～3 条可复用规则（编码规范/易犯错误/工具用法/项目约定），每条用 evolve_record 工具的 add 操作沉淀（title 一句话规则；body 具体做法含正反示例；keywords 给出触发词）；
3. 已有同主题经验卡（先 list 查看）直接更新正文，不要重复堆叠；
	4. 无值得沉淀的新经验时输出「本次无新增经验」即可，不强行编造。`

// selfCheckTaskPrompt 定时自检任务提示词（promptconfig 配置覆盖；空 = 内置默认）。
func (a *App) selfCheckTaskPrompt() string {
	if s := strings.TrimSpace(a.promptStore.Get("selfCheckTask")); s != "" {
		return s
	}
	return selfReviewTaskPrompt
}

// CreateSelfReviewJob 一键创建「代码自检」定时任务（自进化闭环入口）。
// expr 示例："0 21 * * *"（每天 21:00）或 "@daily"；workspace 空 = 当前工作区。
func (a *App) CreateSelfReviewJob(expr, workspace string) (cron.Job, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		expr = "0 21 * * *"
	}
	j, err := a.cronStore.Add(cron.Job{
		Name:       "代码自检·经验沉淀",
		Expr:       expr,
		Workspace:  strings.TrimSpace(workspace),
		Mode:       "code",
		Task:       a.selfCheckTaskPrompt(),
		Enabled:    true,
		TimeoutMin: cron.DefaultTimeoutMin,
		RetryLimit: 1,
	})
	if err == nil {
		a.pushCron()
	}
	return j, err
}

// ListEvolveCards 经验卡列表（设置面板展示）。
func (a *App) ListEvolveCards() []evolve.Card { return a.evolveStore.List() }

// DeleteEvolveCard 删除经验卡。
func (a *App) DeleteEvolveCard(id string) error { return a.evolveStore.Delete(id) }

// EvolveStats 经验卡统计（面板徽标）。
func (a *App) EvolveStats() map[string]int {
	cards := a.evolveStore.List()
	hits := 0
	for _, c := range cards {
		hits += c.Hits
	}
	return map[string]int{"cards": len(cards), "hits": hits}
}

// cronRunner 调度器触发回调：送入现有会话执行链。
// 会话 ID 固定前缀（cron_<jobID>），同一任务的多次触发复用同一会话（历史连续）；
// 会话已有任务运行时 SendMessage 返回错误（计入执行记录 failed）。
// 任务级配置同步：触发时把 job 的模型/模式/工作区写入 cron_<jobID> 会话
// （模型空 = 恢复跟随全局），runTask 经「会话自有模型/模式/工作区」覆盖链生效；
// 旧会话无记录时 Get 失败即跳过（首次触发由 runTask 按全局默认建会话）。
// 例外：每小时复盘哨兵任务不进引擎（系统级单次 LLM 复盘，无需 Agent 轮次），
// 直接后台执行 hourlyReviewRun；同步返回（成功）避免调度器误判失败重试。
func (a *App) cronRunner(j cron.Job) (string, error) {
	sessID := "cron_" + j.ID
	// run_flow 动作：到点触发指定流程（复用 RunFlow 同一启动链路 runFlowCore，
	// 轨迹仅留结果不推前端事件——cron 触发无前端会话上下文）。异步执行后同
	// 步返回，调度器据返回 error 走既有重试/熔断语义（执行记录 SessionID 存
	// 触发标识）。dry-run 不适用于定时产出场景（耗时长任务就是要真实跑）。
	if j.Action == cron.ActionRunFlow {
		return a.cronRunFlow(j)
	}
	// 提示词热生效：自检任务创建时固化了任务文本快照——按内置默认模板
	// 创建的 job，触发时重读 promptconfig（设置面板「提示词」页修改
	// selfCheckTask 段后，下一次触发即按新提示词执行，无需重建任务）。
	if j.Task == selfReviewTaskPrompt {
		j.Task = a.selfCheckTaskPrompt()
	}
	if isHourlyReviewTask(j.Task) {
		go a.hourlyReviewRun(sessID)
		return sessID, nil
	}
	// 员工解析（doc/09-设计方案 Phase 2-② cron 链路）：按 job 绑定三元组
	// （EmployeeID/PositionID/AssignRule）经 ResolveAssignment 四分支解析本次
	// 执行员工。解析失败（池空 / 员工不存在 / 未分配岗位）→ slog 留痕 + 返回
	// 错误：调度器记 failed 执行记录并按重试/熔断语义处理（§10 池空不静默降级，
	// 不静默裸跑）。绑定全空 → 裸跑存量语义（§2-④，行为与旧版完全一致）。
	empID, degraded, err := a.resolveCronEmployee(j)
	if err != nil {
		slog.Error("cron 任务员工解析失败（任务不执行）", "job", j.ID,
			"name", j.Name, "employeeId", j.EmployeeID, "positionId", j.PositionID, "err", err)
		a.notifyCronFailed(j, err) // 同步失败：调度器记 failed 前先推失焦通知
		return sessID, err
	}
	if degraded {
		// §10 降级留痕：绑定员工不可运行 → 按分配规则从岗位池降级。
		// 日志 + 会话双留痕（§2-②：降级事件记入会话，回放可见）；会话尚未
		// 存在（首次触发 runTask 未建会话）时仅日志留痕，下轮触发补写。
		slog.Warn("cron 任务降级：绑定员工不可运行，已按分配规则从岗位池选取",
			"job", j.ID, "name", j.Name, "employeeId", j.EmployeeID,
			"positionId", j.PositionID)
		if s, gerr := a.store.Get(sessID); gerr == nil && s != nil {
			s.AppendMessages([]message.ClineMessage{message.NewSay(message.SayText,
				"⚠️ 降级分配：绑定员工不可运行，本次按分配规则从岗位池选取执行者（doc/09-设计方案 §2-②/§10 留痕）")})
			a.store.Save(s)
		}
	}
	a.syncCronSession(sessID, j, empID)
	_, err = a.SendMessageWithEmployee(sessID, j.Task, nil, empID)
	if err != nil {
		a.notifyCronFailed(j, err)
	}
	return sessID, err
}

// cronRunFlow run_flow 动作执行：加载流程 → runFlowCore 启动（异步，避免
// 长耗时流程阻塞调度器 goroutine 长期占用；runJob 的执行记录收尾在流程启动
// 后由 cron 包自行完成）。失败错误返回调度器（重试/熔断语义与提示词任务
// 一致），会话 ID 固定前缀 cron_flow_ 防与提示词任务会话混淆。
func (a *App) cronRunFlow(j cron.Job) (string, error) {
	flowID := strings.TrimSpace(j.FlowID)
	if flowID == "" {
		return "cron_flow_" + j.ID, errors.New("run_flow 任务未指定流程 ID")
	}
	f, err := a.GetFlow(flowID)
	if err != nil {
		return "cron_flow_" + j.ID, fmt.Errorf("加载流程 %s 失败: %w", flowID, err)
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("cron run_flow panic", "job", j.ID, "flow", flowID, "recover", r,
					"stack", string(debug.Stack()))
			}
		}()
		if _, err := a.runFlowCore(context.Background(), f, nil, false, nil); err != nil {
			slog.Error("cron run_flow 执行失败", "job", j.ID, "name", j.Name, "flow", flowID, "err", err)
		}
	}()
	return "cron_flow_" + j.ID, nil
}

// notifyCronFailed 定时任务失败通知钩子：推送 kaelvoris:cron-failed 事件，
// 前端在窗口失焦时弹系统通知（共用通道 desktopNotify.notifyIfBlurred，
// 与回复完成通知条目共用同一通道与开关；成功不推，防打扰）。
// 覆盖两类失败：① 调度同步失败（员工解析失败/会话忙，cronRunner 返回 err 处）；
// ② 引擎运行失败（如无效模型，runTask 收尾按会话 ID 前缀 cron_ 判定后转发）。
func (a *App) notifyCronFailed(j cron.Job, execErr error) {
	a.emitEvent("kaelvoris:cron-failed", cronFailedPayload(j, execErr))
}

// cronFailedPayload 事件载荷（纯函数可测：任务名 + 截断后的错误首行）。
func cronFailedPayload(j cron.Job, execErr error) map[string]any {
	msg := ""
	if execErr != nil {
		msg = execErr.Error()
	}
	return map[string]any{
		"jobId": j.ID, "jobName": j.Name, "error": truncateRunes(msg, 200),
	}
}

// cronJobName 按 ID 读任务名（任务已删除时回退 ID 本身；runTask 失败转发用，
// 那里只有会话 ID 前缀衍生出的 jobID，无完整 Job 对象）。
func cronJobName(store *cron.Store, jobID string) string {
	if j, ok := store.Get(jobID); ok && j.Name != "" {
		return j.Name
	}
	return jobID
}

// resolveCronEmployee 触发时解析本次执行员工（cron 链路唯一分配入口，§2）。
// 返回（员工ID, 是否降级, 错误）：绑定三元组全空 → 裸跑（恒空串，无错误，
// §2-④ 存量语义）；round_robin 解析成功时把下一游标回写 Job.LastAssignedIndex
// 并经 cron Store Update 原子落盘（§2.1：游标存资源项配置内，随 jobs.json
// 原子写；并发互斥以调度器 tryOccupy 同任务防重入 + Store 内部锁为准）。
func (a *App) resolveCronEmployee(j cron.Job) (string, bool, error) {
	if j.EmployeeID == "" && j.PositionID == "" {
		return "", false, nil
	}
	employees := a.empStore.List() // Store 内部锁快照（与渠道侧同语义）
	r, next, err := employee.ResolveAssignment(employees, employee.Assignment{
		EmployeeID: j.EmployeeID,
		PositionID: j.PositionID,
		AssignRule: j.AssignRule,
		LastIndex:  j.LastAssignedIndex,
		Load:       a.employeeLoad, // least_loaded 运行计数（策略层按需调用）
	})
	if err != nil {
		return "", false, err
	}
	if j.AssignRule == employee.AssignRoundRobin {
		j.LastAssignedIndex = next
		if _, err := a.cronStore.Update(j); err != nil {
			return "", false, fmt.Errorf("回写轮转游标失败: %w", err)
		}
	}
	return r.EmployeeID, r.Degraded, nil
}

// syncCronSession 任务级配置 → cron 会话（面板/工具编辑后下次触发即生效）：
// 把 job 的模型/模式/工作区写入 cron_<jobID> 会话（模型空 = 恢复跟随全局），
// runTask 经「会话自有模型/模式/工作区」覆盖链生效。旧会话无记录时
// Get 失败即跳过（首次触发由 runTask 按全局默认建会话，下轮再同步）。
// empID 非空时固化员工岗位快照（§6.1：Session.EmployeeID + PositionID +
// AssignRule，会话生命周期不重选，员工调岗不漂移；cron_<jobID> 会话跨触发
// 复用，本轮解析出的员工覆盖写入——SendMessageWithEmployee 对老会话忽略
// employeeID 入参，必须走会话快照才能换绑，§4.2 cron 链路语义）。
func (a *App) syncCronSession(sessID string, j cron.Job, empID string) {
	s, err := a.store.Get(sessID)
	if err != nil || s == nil {
		return
	}
	s.SetModel(strings.TrimSpace(j.Model))
	s.SetMode(strings.TrimSpace(j.Mode))
	s.SetWorkspace(strings.TrimSpace(j.Workspace))
	s.SetEmployeeID(empID)
	s.SetPositionSnapshot(strings.TrimSpace(j.PositionID), strings.TrimSpace(j.AssignRule))
	a.store.Save(s)
}

// ---------- 每小时会话复盘：一键创建 ----------

// CreateHourlyReviewJob 一键创建「每小时会话复盘」定时任务（系统级自进化入口）。
// 同名启用任务已存在时不重复创建（防重复叠加）；存在同名停用任务则重新启用。
// expr 示例："0 * * * *"（每小时整点）或 "15 * * * *"（每小时第 15 分）。
func (a *App) CreateHourlyReviewJob(expr string) (cron.Job, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		expr = "0 * * * *"
	}
	// 防重复：已有同名任务时复用（停用 → 重新启用；表达式同步更新）。
	for _, old := range a.cronStore.List() {
		if old.Name != hourlyReviewJobName {
			continue
		}
		old.Expr = expr
		old.Task = hourlyReviewSentinel
		old.Mode = ""
		if !old.Enabled {
			old.Enabled = true
			old.FailStreak = 0
			old.DisabledReason = ""
			old.NextRetry = time.Time{}
		}
		updated, err := a.cronStore.Update(old)
		if err == nil {
			a.pushCron()
			return updated, nil
		}
		break
	}
	j, err := a.cronStore.Add(cron.Job{
		Name:       hourlyReviewJobName,
		Expr:       expr,
		Mode:       "ask",
		Task:       hourlyReviewSentinel,
		Enabled:    true,
		TimeoutMin: cron.DefaultTimeoutMin,
		RetryLimit: 1,
	})
	if err == nil {
		a.pushCron()
	}
	return j, err
}

// ---------- 总任务面板 ----------

// TaskItem 总任务面板条目（一个会话一条；含定时任务来源标记）。
type TaskItem struct {
	SessionID string    `json:"sessionId"`
	Title     string    `json:"title"`
	Workspace string    `json:"workspace,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Running   bool      `json:"running"`
	IsCron    bool      `json:"isCron"` // 定时任务来源（ID 前缀 cron_）
	TodoTotal int       `json:"todoTotal"`
	TodoDone  int       `json:"todoDone"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ListAllTasks 总任务聚合：全部会话 + 运行状态 + 待办完成度。
// 供左侧"任务"总面板展示（跨会话全景，点击跳转会话）。
func (a *App) ListAllTasks() []TaskItem {
	summaries := a.store.List()
	out := make([]TaskItem, 0, len(summaries))
	for _, s := range summaries {
		item := TaskItem{
			SessionID: s.ID,
			Title:     s.Title,
			Workspace: s.Workspace,
			Mode:      s.Mode,
			IsCron:    strings.HasPrefix(s.ID, "cron_"),
			UpdatedAt: s.UpdatedAt,
		}
		// 运行状态（session 侧 running 集合）。
		item.Running = a.IsSessionRunning(s.ID)
		// 待办完成度（持久化快照）。
		if sess, err := a.store.Get(s.ID); err == nil {
			snap := sess.Snapshot()
			item.TodoTotal = len(snap.Todos)
			for _, t := range snap.Todos {
				if t.Status == "completed" {
					item.TodoDone++
				}
			}
		}
		out = append(out, item)
	}
	return out
}
