// cron_tools.go —— 定时任务的聊天工具集成（内部 MCP 风格：工具级暴露给 Agent）。
//
// 用户在聊天中即可让 Agent 管理应用内定时任务：
//   - list   查看全部任务（id/表达式/中文描述/下次触发）
//   - add    新增（name/expr/task 必填；workspace 缺省 = 当前会话工作区）
//   - update 按 id 更新（未提供的字段保持不变）
//   - delete 按 id 删除
//   - toggle 启停（enabled 缺省 = 取反）
//   - run    立即执行一次（复用调度器 RunNow，受防重入约束）
//
// 数据面复用 internal/cron（Store 持久化 + Scheduler 调度），与设置面板
// 「定时任务」页共享同一份 jobs.json；任何变更经 Push 回调推送 kaelvoris:cron
// 事件，前端列表实时刷新。注册进任务级 Registry 后由模式系统按 command
// 组放行（code/debug 等含 command 组的模式可用）。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kaelvoris/internal/cron"
)

// cronToolArgs cron_job 工具入参（Enabled 用指针区分「未提供」与 false）。
type cronToolArgs struct {
	Action     string `json:"action"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	Expr       string `json:"expr"`
	Workspace  string `json:"workspace"`
	Mode       string `json:"mode"`
	Model      string `json:"model"`
	Task       string `json:"task"`
	Enabled    *bool  `json:"enabled"`
	TimeoutMin int    `json:"timeoutMin"`
	RetryLimit int    `json:"retryLimit"`
	// 运行时间窗口（可选）：开启后仅允许窗口内自动触发（跨零点支持）。
	TimeWindowEnabled *bool  `json:"timeWindowEnabled"` // 指针区分「未提供」与 false
	StartTime         string `json:"startTime"`         // "HH:MM"，默认 18:00
	EndTime           string `json:"endTime"`           // "HH:MM"，默认 06:00
}

// CronTool 定时任务管理工具（实现 tools.Tool 接口）。
type CronTool struct {
	Store *cron.Store
	Sched *cron.Scheduler
	// DefaultWorkspace 新增任务未指定工作区时的默认值（注册处传入会话工作区）。
	DefaultWorkspace string
	// Push 变更后刷新前端任务列表（可空；桌面端为 App.pushCron）。
	Push func()
}

func (t *CronTool) Name() string { return "cron_job" }

func (t *CronTool) Description() string {
	return "Manage the app's built-in scheduled tasks (定时任务). Actions: " +
		"list (view all jobs with id/expr/description/next-run), add (create: " +
		"name + expr + task required), update (modify fields by id), delete " +
		"(remove by id), toggle (enable/disable by id), run (trigger once now). " +
		"`expr` is a 5-field cron expression \"minute hour day month weekday\", " +
		"e.g. \"30 9 * * *\" = daily 09:30, \"*/10 * * * *\" = every 10 minutes. " +
		"Descriptor syntax also supported: @hourly/@daily/@weekly/@monthly/@yearly/" +
		"@every <duration> (e.g. \"@every 2h\"). " +
		"`task` is the prompt text sent to the agent engine each time the job " +
		"fires. Call list first to get job ids before update/delete/toggle/run. " +
		"The settings panel (定时任务) refreshes automatically after every change."
}

func (t *CronTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"description": "Operation to perform on scheduled tasks",
				"enum":        []string{"list", "add", "update", "delete", "toggle", "run"},
			},
			"id":                map[string]interface{}{"type": "string", "description": "Job id (required for update/delete/toggle/run; get ids from list)"},
			"name":              map[string]interface{}{"type": "string", "description": "Job display name (add; update to rename)"},
			"expr":              map[string]interface{}{"type": "string", "description": "5-field cron expression \"min hour day month weekday\", e.g. \"30 9 * * *\" (add; update to reschedule)"},
			"workspace":         map[string]interface{}{"type": "string", "description": "Working directory the job runs in (add/update; empty = current session workspace)"},
			"mode":              map[string]interface{}{"type": "string", "description": "Mode slug the job runs in (add/update; empty = default mode)"},
			"model":             map[string]interface{}{"type": "string", "description": "LLM model id the job runs with (add/update; empty = global configured model). Get available ids via ListModels panel or ask the user"},
			"task":              map[string]interface{}{"type": "string", "description": "Prompt text sent to the agent engine each time the job fires (add; update to change)"},
			"enabled":           map[string]interface{}{"type": "boolean", "description": "Enable state (toggle: omit to flip current state)"},
			"timeoutMin":        map[string]interface{}{"type": "integer", "description": "Per-run timeout in minutes (add/update; 0 = default 30; timed-out runs release the busy lock automatically)"},
			"retryLimit":        map[string]interface{}{"type": "integer", "description": "Auto-retry count on failure (add/update; 0-" + fmt.Sprint(cron.MaxRetryLimit) + ", default 0; backoff " + cron.RetryBackoff.String() + ")"},
			"timeWindowEnabled": map[string]interface{}{"type": "boolean", "description": "Run-time-window switch (add/update; when on, auto-fire is allowed only inside [startTime, endTime], supports crossing midnight; manual run is not restricted)"},
			"startTime":         map[string]interface{}{"type": "string", "description": "Window start \"HH:MM\" (default 18:00; effective when timeWindowEnabled=true)"},
			"endTime":           map[string]interface{}{"type": "string", "description": "Window end \"HH:MM\" (default 06:00; effective when timeWindowEnabled=true)"},
		},
		"required": []string{"action"},
	}
}

func (t *CronTool) Execute(ctx context.Context, args string) (string, error) {
	var p cronToolArgs
	if strings.TrimSpace(args) == "" {
		p.Action = "list" // 无参 = 列表（对齐 design_system 的浏览语义）
	} else if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(p.Action)) {
	case "list", "":
		return t.list()
	case "add":
		return t.add(p)
	case "update":
		return t.update(p)
	case "delete":
		return t.delete(p.ID)
	case "toggle":
		return t.toggle(p)
	case "run":
		return t.run(p.ID)
	default:
		return "", fmt.Errorf("unknown action %q (list/add/update/delete/toggle/run)", p.Action)
	}
}

// list 全部任务概览（附 id 供后续操作引用）。
func (t *CronTool) list() (string, error) {
	jobs := t.Store.List()
	if len(jobs) == 0 {
		return "定时任务：暂无。用 add 操作创建（name/expr/task 必填）。", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "定时任务共 %d 个（id 用于 update/delete/toggle/run）：\n", len(jobs))
	for i, j := range jobs {
		fmt.Fprintf(&b, "\n%d. %s（id: %s）— %s，%s\n", i+1, j.Name, j.ID, cron.Describe(j.Expr), cronEnabledText(j.Enabled))
		fmt.Fprintf(&b, "   表达式: %s\n", j.Expr)
		if j.Enabled && !j.NextRun.IsZero() {
			fmt.Fprintf(&b, "   下次触发: %s\n", j.NextRun.Format("2006-01-02 15:04"))
		}
		if !j.LastRun.IsZero() {
			fmt.Fprintf(&b, "   上次触发: %s\n", j.LastRun.Format("2006-01-02 15:04"))
		}
		if j.Workspace != "" {
			fmt.Fprintf(&b, "   工作区: %s\n", j.Workspace)
		}
		if j.Mode != "" {
			fmt.Fprintf(&b, "   模式: %s\n", j.Mode)
		}
		if j.Model != "" {
			fmt.Fprintf(&b, "   模型: %s\n", j.Model)
		}
		if j.TimeoutMin > 0 || j.RetryLimit > 0 {
			fmt.Fprintf(&b, "   可靠性: 超时 %d 分钟，重试 %d 次\n", timeoutMinOr(j.TimeoutMin), j.RetryLimit)
		}
		if j.TimeWindowEnabled {
			fmt.Fprintf(&b, "   运行时间窗口: %s～%s\n", j.StartTime, j.EndTime)
		}
		if j.FailStreak > 0 {
			fmt.Fprintf(&b, "   连败: %d 次\n", j.FailStreak)
		}
		if j.DisabledReason != "" {
			fmt.Fprintf(&b, "   熔断: %s\n", j.DisabledReason)
		}
		fmt.Fprintf(&b, "   任务: %s\n", t.snippet(j.Task, 120))
	}
	return b.String(), nil
}

// timeoutMinOr 显示用超时值（0 = 默认）。
func timeoutMinOr(v int) int {
	if v > 0 {
		return v
	}
	return cron.DefaultTimeoutMin
}

// add 新增任务（workspace 缺省 = 当前会话工作区；enabled 缺省 = true）。
func (t *CronTool) add(p cronToolArgs) (string, error) {
	if strings.TrimSpace(p.Name) == "" {
		return "", fmt.Errorf("add 需要 name（任务名）")
	}
	if strings.TrimSpace(p.Expr) == "" {
		return "", fmt.Errorf("add 需要 expr（5 段 cron 表达式，如 \"30 9 * * *\"）")
	}
	if strings.TrimSpace(p.Task) == "" {
		return "", fmt.Errorf("add 需要 task（定时发送给 Agent 的提示词）")
	}
	ws := strings.TrimSpace(p.Workspace)
	if ws == "" {
		ws = t.DefaultWorkspace
	}
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	j, err := t.Store.Add(cron.Job{
		Name:              strings.TrimSpace(p.Name),
		Expr:              strings.TrimSpace(p.Expr),
		Workspace:         ws,
		Mode:              strings.TrimSpace(p.Mode),
		Model:             strings.TrimSpace(p.Model),
		Task:              p.Task,
		Enabled:           enabled,
		TimeoutMin:        p.TimeoutMin,
		RetryLimit:        p.RetryLimit,
		TimeWindowEnabled: p.TimeWindowEnabled != nil && *p.TimeWindowEnabled,
		StartTime:         strings.TrimSpace(p.StartTime),
		EndTime:           strings.TrimSpace(p.EndTime),
	})
	if err != nil {
		return "", fmt.Errorf("创建失败: %w", err)
	}
	t.notify()
	var b strings.Builder
	fmt.Fprintf(&b, "已创建定时任务：%s\nid: %s\n表达式: %s（%s）\n状态: %s\n任务: %s\n",
		j.Name, j.ID, j.Expr, cron.Describe(j.Expr), cronEnabledText(j.Enabled), t.snippet(j.Task, 120))
	if j.TimeWindowEnabled {
		fmt.Fprintf(&b, "运行时间窗口: %s～%s（窗口外自动触发跳过）\n", j.StartTime, j.EndTime)
	}
	if j.Workspace != "" {
		fmt.Fprintf(&b, "工作区: %s\n", j.Workspace)
	}
	if j.Mode != "" {
		fmt.Fprintf(&b, "模式: %s\n", j.Mode)
	}
	if j.Model != "" {
		fmt.Fprintf(&b, "模型: %s\n", j.Model)
	}
	if sched, err := cron.Parse(j.Expr); err == nil {
		if next := sched.Next(time.Now()); !next.IsZero() {
			fmt.Fprintf(&b, "下次触发: %s\n", next.Format("2006-01-02 15:04"))
		}
	}
	b.WriteString("设置面板「定时任务」列表已同步。")
	return b.String(), nil
}

// update 按 id 更新（未提供的字段保持不变）。
func (t *CronTool) update(p cronToolArgs) (string, error) {
	id := strings.TrimSpace(p.ID)
	if id == "" {
		return "", fmt.Errorf("update 需要 id（先 list 获取）")
	}
	cur, ok := t.Store.Get(id)
	if !ok {
		return "", cron.ErrJobNotFound
	}
	j := cur
	if v := strings.TrimSpace(p.Name); v != "" {
		j.Name = v
	}
	if v := strings.TrimSpace(p.Expr); v != "" {
		j.Expr = v
	}
	if v := strings.TrimSpace(p.Workspace); v != "" {
		j.Workspace = v
	}
	if v := strings.TrimSpace(p.Mode); v != "" {
		j.Mode = v
	}
	if p.Model != "" {
		j.Model = strings.TrimSpace(p.Model)
	}
	if p.Task != "" {
		j.Task = p.Task
	}
	if p.Enabled != nil {
		j.Enabled = *p.Enabled
	}
	if p.TimeoutMin > 0 {
		j.TimeoutMin = p.TimeoutMin
	}
	if p.RetryLimit > 0 {
		j.RetryLimit = p.RetryLimit
	}
	if p.TimeWindowEnabled != nil {
		j.TimeWindowEnabled = *p.TimeWindowEnabled
	}
	if v := strings.TrimSpace(p.StartTime); v != "" {
		j.StartTime = v
	}
	if v := strings.TrimSpace(p.EndTime); v != "" {
		j.EndTime = v
	}
	updated, err := t.Store.Update(j)
	if err != nil {
		return "", fmt.Errorf("更新失败: %w", err)
	}
	t.notify()
	var b strings.Builder
	fmt.Fprintf(&b, "已更新定时任务：%s\nid: %s\n表达式: %s（%s）\n状态: %s\n任务: %s\n",
		updated.Name, updated.ID, updated.Expr, cron.Describe(updated.Expr),
		cronEnabledText(updated.Enabled), t.snippet(updated.Task, 120))
	if sched, err := cron.Parse(updated.Expr); err == nil {
		if next := sched.Next(time.Now()); !next.IsZero() {
			fmt.Fprintf(&b, "下次触发: %s\n", next.Format("2006-01-02 15:04"))
		}
	}
	return b.String(), nil
}

// delete 删除任务。
func (t *CronTool) delete(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("delete 需要 id（先 list 获取）")
	}
	j, ok := t.Store.Get(id)
	name := id
	if ok {
		name = j.Name
	}
	if err := t.Store.Delete(id); err != nil {
		return "", err
	}
	t.notify()
	return fmt.Sprintf("已删除定时任务「%s」（id: %s）。", name, id), nil
}

// toggle 启停（enabled 缺省 = 取反当前状态）。
func (t *CronTool) toggle(p cronToolArgs) (string, error) {
	id := strings.TrimSpace(p.ID)
	if id == "" {
		return "", fmt.Errorf("toggle 需要 id（先 list 获取）")
	}
	enabled := false
	if p.Enabled != nil {
		enabled = *p.Enabled
	} else {
		cur, ok := t.Store.Get(id)
		if !ok {
			return "", cron.ErrJobNotFound
		}
		enabled = !cur.Enabled
	}
	j, err := t.Store.SetEnabled(id, enabled)
	if err != nil {
		return "", err
	}
	t.notify()
	return fmt.Sprintf("定时任务「%s」（id: %s）%s。", j.Name, j.ID, cronEnabledText(j.Enabled)), nil
}

// run 立即执行一次（异步触发；防重入由调度器约束，忙时返回 ErrBusy）。
func (t *CronTool) run(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("run 需要 id（先 list 获取）")
	}
	j, ok := t.Store.Get(id)
	if !ok {
		return "", cron.ErrJobNotFound
	}
	if _, err := t.Sched.RunNow(id); err != nil {
		return "", err
	}
	return fmt.Sprintf("已触发定时任务「%s」（id: %s）立即执行（异步运行；执行记录见设置面板「定时任务」→ 执行记录）。", j.Name, j.ID), nil
}

// notify 变更后刷新前端列表（Push 可空——CLI 场景无事件桥）。
func (t *CronTool) notify() {
	if t.Push != nil {
		t.Push()
	}
}

// snippet 任务提示词单行摘要（换行折叠 + 按 rune 截断）。
func (t *CronTool) snippet(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", "␤")
	return truncateRunes(s, max)
}

func cronEnabledText(v bool) string {
	if v {
		return "已启用"
	}
	return "已停用"
}
