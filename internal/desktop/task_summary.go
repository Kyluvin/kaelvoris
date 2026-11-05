// task_summary.go —— 任务结束后会话总结（会话 hook 能力，2026-10 新增）。
//
// 用户需求：任务结束后自动进行会话总结。本文件实现「会话级 hook」的核心：
//   - 引擎 OnTaskFinished（internal/agent）在 RunInMode 所有收尾路径触发 →
//     maybeTaskSummary 按配置开关后台执行 taskSummaryRun；
//   - taskSummaryRun 把任务文本 + 对话转录 + 待办快照发给主模型（非流式
//     Generate，单次调用），产出 JSON 协议 {goal,status,outcome,next} →
//     解析后落会话存储（SessionData.TaskSummary，随会话 JSON 持久化）；
//   - 前端在聊天流尾部渲染「会话总结」卡片 + 会话列表卡角标（有总结标记）。
//
// 与 selflearn.go（记忆收尾 MEMORY.md）的差异：
//   - selflearn 面向「模型续接上下文」（记忆文档注入系统提示词，会话回路）；
//   - 本文件面向「用户阅读」（任务收尾的可读总结，展示在聊天流/会话卡，
//     不参与模型上下文注入——续聊仍走 ResumeDigest）。
//
// 约束（对齐 selflearn 的防失控方针）：
//   - 同一会话至多 1 个总结调用在跑（taskSummaryMu 按会话互斥）；失败静默跳过；
//   - 仅顶层会话（非子任务）总结；子任务由各自会话的宿主路径处理；
//   - 总结超时 5 分钟；输出钳制（目标/产出/下一步各 ≤ 600 rune）。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"kaelvoris/internal/llm"
	"kaelvoris/internal/session"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// TaskSummaryConfig 任务结束会话总结配置（desktop.json 的 taskSummary 字段）。
// nil = 未配置 = 关闭（对齐 SelfReview 的可选配置族风格）。
type TaskSummaryConfig struct {
	Enabled bool `json:"enabled"`
	// MaxLen 单字段输出上限（rune；0 = 默认 600）。
	MaxLen int `json:"maxLen,omitempty"`
	// Silent 静默模式：总结成功后不推送 toast（true = 仅落盘/推送消息）。
	Silent bool `json:"silent,omitempty"`
}

// taskSummary 总结参数上限（防提示词膨胀与失控写入）。
const (
	taskSummaryMaxTranscript = 8000 // 对话转录上限（rune，超出保留尾部）
	taskSummaryMaxField      = 600  // 单字段输出上限（rune，0 由配置覆盖）
	taskSummaryTimeout       = 5 * time.Minute
)

// taskSummaryRunCache 会话级总结互斥/计数（防并发 LLM 调用与重复总结）。
// key = 会话 ID；仅内存态（重启后会话总结不可恢复为「进行中」——自然丢弃）。
var taskSummaryMu = map[string]bool{}
var taskSummaryMuLock sync.Mutex

// taskSummaryConfig 加锁读取配置。
func (a *App) taskSummaryConfig() *TaskSummaryConfig {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	return a.cfg.TaskSummary
}

// maybeTaskSummary 任务结束后的会话总结入口（不阻塞任务收尾）。
// 仅在配置开启、非子任务会话、无并发总结进行时触发。
func (a *App) maybeTaskSummary(sessionID, task string, run *taskRun, taskErr error, elapsed time.Duration) {
	ts := a.taskSummaryConfig()
	if ts == nil || !ts.Enabled {
		return
	}
	// 会话级互斥：同一会话只允许一个总结在跑（不同会话可并行）。
	taskSummaryMuLock.Lock()
	if taskSummaryMu[sessionID] {
		taskSummaryMuLock.Unlock()
		return
	}
	taskSummaryMu[sessionID] = true
	taskSummaryMuLock.Unlock()

	// 快照对话历史与待办清单（hist 由 say 桥在任务 goroutine 持续追加，
	// todo 由引擎并发更新，都必须拷贝快照）。
	run.histMu.Lock()
	hist := append([]message.ClineMessage(nil), run.hist...)
	run.histMu.Unlock()
	todos := run.todo.Items()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("会话总结 panic（已拦截）", "session", sessionID, "panic", r)
			}
			taskSummaryMuLock.Lock()
			delete(taskSummaryMu, sessionID)
			taskSummaryMuLock.Unlock()
		}()
		a.taskSummaryRun(sessionID, task, hist, todos, taskErr, elapsed, ts)
	}()
}

// taskSummaryPrompt 会话总结提示词（标题/目标/进度/产出/卡点 → JSON 协议）。
// userPrompt 为用户配置覆盖（promptconfig）：非空时以用户文本替换内置模板；空 = 内置默认。
func taskSummaryPrompt(existing, userPrompt string) string {
	var b strings.Builder
	if s := strings.TrimSpace(userPrompt); s != "" {
		b.WriteString(s + "\n")
	} else {
		b.WriteString(`你是任务收尾总结器。根据上面的任务对话，为本会话生成一段简洁的任务总结（面向用户阅读，Markdown）。

	必须覆盖（按此顺序输出，每项一行标签 + 内容）：
	【目标】任务目标一句话（用户想解决什么）。
	【状态】任务完成情况：✅ 已完成 / ⚠️ 部分完成 / ❌ 未完成或失败（含原因一句话）。
	【产出】关键产出物：做了什么（文件/功能/结论），路径按需给出（指针式，不复制正文）。
	【下一步】未完成项：下次接着干从哪里开始；全部完成则写「无」。

	约束：
	- 只记录本会话任务的事实与进度，不写经验教训；不空话、不夸大；
	- 中文，总长不超过 600 字；产出物路径尽量给相对路径；
	- 若对话很短且任务即问即答（纯问答无产出），状态写✅并简要说明即可。

	输出协议：仅输出一个 JSON 对象，不要任何解释、注释或代码围栏：
	{"goal": "目标一句话", "status": "done|failed|stopped", "outcome": "产出摘要", "next": "下一步或 无"}
	`)
	}
	if existing != "" {
		b.WriteString("\n本会话上一次总结（若任务延续，可参考：不要重复同样的内容）：\n" + existing + "\n")
	}
	return b.String()
}

// taskSummaryRun 执行一次会话总结：转录 + 待办 → 主模型 → 解析 → 落会话存储。
func (a *App) taskSummaryRun(sessionID, task string, hist []message.ClineMessage, todos []tools.TodoItem, taskErr error, elapsed time.Duration, cfg *TaskSummaryConfig) {
	transcript := buildTaskSummaryTranscript(hist, task, todos, taskErr)
	if strings.TrimSpace(transcript) == "" {
		return
	}
	// 上一次总结（若有）：供提示词参考（避免同任务重复总结）。
	var existing string
	if s, err := a.store.Get(sessionID); err == nil && s != nil {
		if snap := s.Snapshot(); snap.TaskSummary != nil {
			existing = snap.TaskSummary.Outcome
			if snap.TaskSummary.Next != "" && snap.TaskSummary.Next != "无" {
				existing += "\n下一步: " + snap.TaskSummary.Next
			}
		}
	}

	// E2 辅助模型分流（对标 Plandex summarizer 角色）：总结属辅助环节，
	// 配置了辅助模型时走便宜小模型，未配置回落主模型（零破坏）。
	client := a.auxClient(nil)
	ctx, cancel := context.WithTimeout(context.Background(), taskSummaryTimeout)
	defer cancel()
	resp, err := client.Generate(ctx, llm.ChatRequest{Messages: []llm.Message{{
		Role:    llm.RoleUser,
		Content: transcript + "\n\n" + taskSummaryPrompt(existing, a.promptStore.Get("taskSummary")),
	}}})
	if err != nil {
		// 总结是尽力而为的后台增强：失败静默（不干扰用户），仅留日志。
		slog.Warn("会话总结调用失败（跳过）", "session", sessionID, "err", err)
		return
	}
	ts := parseTaskSummaryOutput(resp.Content)
	if ts == nil {
		slog.Warn("会话总结输出解析失败（跳过）", "session", sessionID, "content", truncateRunes(resp.Content, 200))
		return
	}
	// 字段钳制（rune 上限，防失控超长落盘）。
	maxLen := cfg.MaxLen
	if maxLen <= 0 {
		maxLen = taskSummaryMaxField
	}
	ts.Goal = clampRunes(ts.Goal, maxLen)
	ts.Outcome = clampRunes(ts.Outcome, maxLen)
	ts.Next = clampRunes(ts.Next, maxLen)
	// 状态枚举兜底（模型输出不合法时按错误/停止归类）。
	switch ts.Status {
	case "done", "failed", "stopped":
	default:
		if taskErr != nil {
			ts.Status = "failed"
		} else {
			ts.Status = "done"
		}
	}
	ts.ElapsedSecs = int64(elapsed.Seconds())
	ts.GeneratedAt = time.Now()

	// 落会话存储（异步刷盘；总结正文独立字段，不污染消息流历史）。
	if s, err := a.store.Get(sessionID); err == nil && s != nil {
		s.SetTaskSummary(ts)
		a.store.Save(s)
		// 聊天流尾部推送总结卡片（前端 kind=summary 渲染；不落历史消息数组）。
		// 消息正文 = 结构化 Markdown（前端直接展示）。
		msg := buildSummaryMessage(ts)
		uiEmit(a.ctx, "cline:message", map[string]any{
			"sessionId": sessionID,
			"message":   message.NewSay(message.SayTaskSummary, msg),
		})
		// 会话列表卡角标刷新（有总结标记；同时覆盖卡片摘要显示最新总结）。
		uiEmit(a.ctx, "kaelvoris:sessions", a.store.List())
		if !cfg.Silent {
			a.pushToast("📋 会话总结已生成：" + truncateRunes(ts.Goal, 40))
		}
	}
}

// buildSummaryMessage 把 TaskSummary 渲染为 Markdown 消息正文（前端 summary 卡片）。
func buildSummaryMessage(ts *session.TaskSummary) string {
	var b strings.Builder
	b.WriteString("**会话总结 · " + summaryStatusLabel(ts.Status) + "**\n\n")
	if ts.Goal != "" {
		b.WriteString("**目标**：" + ts.Goal + "\n\n")
	}
	if ts.Outcome != "" {
		b.WriteString("**产出**：" + ts.Outcome + "\n\n")
	}
	if ts.Next != "" && ts.Next != "无" {
		b.WriteString("**下一步**：" + ts.Next + "\n")
	}
	if ts.ElapsedSecs > 0 {
		b.WriteString(fmt.Sprintf("\n> ⏱ 耗时 %s · %s\n", fmtDurShort(ts.ElapsedSecs), ts.GeneratedAt.Format("01-02 15:04")))
	}
	return strings.TrimSpace(b.String())
}

// summaryStatusLabel 状态中文标签。
func summaryStatusLabel(s string) string {
	switch s {
	case "done":
		return "✅ 已完成"
	case "failed":
		return "❌ 未完成"
	case "stopped":
		return "⏹ 已停止"
	}
	return "—"
}

// taskSummaryOutput 总结 JSON 协议（模型输出）。
type taskSummaryOutput struct {
	Goal    string `json:"goal"`
	Status  string `json:"status"`
	Outcome string `json:"outcome"`
	Next    string `json:"next"`
}

// parseTaskSummaryOutput 解析模型输出（容错：剥离代码围栏，取首个 JSON 对象）。
func parseTaskSummaryOutput(s string) *session.TaskSummary {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// 截取首个 { 到最后一个 }（剥解释文本/围栏）。
	i := strings.Index(s, "{")
	if i < 0 {
		return nil
	}
	j := strings.LastIndex(s, "}")
	if j <= i {
		return nil
	}
	var out taskSummaryOutput
	if err := json.Unmarshal([]byte(s[i:j+1]), &out); err != nil {
		return nil
	}
	ts := &session.TaskSummary{
		Goal:    strings.TrimSpace(out.Goal),
		Status:  strings.TrimSpace(out.Status),
		Outcome: strings.TrimSpace(out.Outcome),
		Next:    strings.TrimSpace(out.Next),
	}
	// 全空 = 无效输出。
	if ts.Goal == "" && ts.Outcome == "" && ts.Next == "" {
		return nil
	}
	return ts
}

// buildTaskSummaryTranscript 把会话消息历史转成紧凑对话文本（总结素材）。
// 跳过流式 partial 中间帧与 ask 事件；保留用户/助手/工具/错误四类信号。
func buildTaskSummaryTranscript(hist []message.ClineMessage, task string, todos []tools.TodoItem, taskErr error) string {
	var b strings.Builder
	b.WriteString("---- 本会话任务对话转录（总结素材） ----\n")
	if s := strings.TrimSpace(task); s != "" {
		fmt.Fprintf(&b, "用户任务: %s\n", truncateRunes(s, 500))
	}
	if len(todos) > 0 {
		b.WriteString("[任务清单]\n")
		for _, it := range todos {
			mark := "[ ]"
			switch it.Status {
			case "completed":
				mark = "[x]"
			case "in_progress":
				mark = "[-]"
			}
			fmt.Fprintf(&b, "%s %s\n", mark, it.Content)
		}
	}
	for _, m := range hist {
		if m.Partial || m.Type != message.TypeSay {
			continue
		}
		switch m.Say {
		case message.SayUserFeedback:
			fmt.Fprintf(&b, "用户: %s\n", truncateRunes(oneLine(m.Text), 400))
		case message.SayText:
			fmt.Fprintf(&b, "助手: %s\n", truncateRunes(oneLine(m.Text), 500))
		case message.SayTool:
			var info message.ToolCallInfo
			if json.Unmarshal([]byte(m.Text), &info) == nil {
				label := info.Tool
				if info.Path != "" {
					label += " → " + info.Path
				} else if info.Cmd != "" {
					label += " → " + truncateRunes(info.Cmd, 100)
				}
				fmt.Fprintf(&b, "工具: %s\n", label)
			}
		case message.SayError:
			fmt.Fprintf(&b, "错误: %s\n", truncateRunes(oneLine(m.Text), 250))
		}
	}
	if taskErr != nil {
		fmt.Fprintf(&b, "任务结果: 异常结束（%s）\n", truncateRunes(taskErr.Error(), 200))
	}
	out := b.String()
	if r := []rune(out); len(r) > taskSummaryMaxTranscript {
		out = string(r[len(r)-taskSummaryMaxTranscript:])
	}
	return out
}

// clampRunes 按 rune 截断（无省略号，硬上限钳制）。
func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// oneLine 压缩为单行。
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// fmtDurShort 耗时秒数 → 人类可读（1m30s / 45s）。
func fmtDurShort(secs int64) string {
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm%ds", secs/60, secs%60)
}
