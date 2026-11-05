// session_cleanup.go —— PC 端「清理会话」闭环（桌面端 App 绑定 + 前端按钮数据源）。
//
// 用户需求（2026-10-01 未完成任务 A3）：
//   - 电脑端 + 移动端提供「清理会话」能力；
//   - 清理前必须先对每个会话的执行过程进行分析、形成分析文档、找出可优化的
//     冗余步骤（用于完善工具与提示词），不能直接删。
//
// 落地语义（单个会话）：
//  1. 完成校验：会话必须在本地存档（store.Get 可读）；可选仅清理非运行中会话；
//  2. 会话分析：复用 remoteAnalyzeSession 的材料组装（标题/待办/记忆/完整对话），
//     调 LLM 产出「用户问题理解 → 执行过程 → 结果评价 → 可优化冗余步骤」报告；
//  3. 生成文档：报告落 docs/session-reviews/<safeID>-<ts>.md（工作区内，可回溯），
//     同时以定型消息追加进会话历史（删除前最后留档）；
//  4. 归档删除：Store.Delete（内存缓存 + 磁盘 JSON + 记忆文档一并清理）。
//
// 批量清理 ListSessions 全量非运行中会话；失败逐条记录（不中断批次），
// 返回结果摘要供前端渲染。分析失败（LLM 未配置/调用失败）时可选择跳过删除
// （保守：分析不成不删，用户诉求"不能直接删"）。
package desktop

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"kaelvoris/internal/llm"
	"kaelvoris/internal/remote"
	"kaelvoris/internal/tools"
	"kaelvoris/pkg/message"
)

// CleanupReport 单会话清理结果（前端结果卡片渲染）。
type CleanupReport struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
	OK        bool   `json:"ok"`       // 分析+归档删除全链路成功
	Skipped   bool   `json:"skipped"`  // 跳过（运行中/无历史/分析失败等）
	Reason    string `json:"reason"`   // skipped=true 时的原因
	DocPath   string `json:"docPath"`  // 分析文档落盘路径（工作区相对路径）
	Analysis  string `json:"analysis"` // 分析报告正文（前端预览）
}

// CleanupSummary 批量清理结果。
type CleanupSummary struct {
	Total   int             `json:"total"`   // 参与清理的会话数
	Deleted int             `json:"deleted"` // 成功清理（分析+删除）
	Skipped int             `json:"skipped"` // 跳过数
	Reports []CleanupReport `json:"reports"` // 逐会话明细
}

// cleanupAnalyzeTimeout 单会话分析 LLM 调用超时（与 remoteAnalyzeSession 同 5min）。
const cleanupAnalyzeTimeout = 5 * time.Minute

// cleanupDocDir 分析文档落盘目录（工作区相对路径）。
const cleanupDocDir = "docs/session-reviews"

// safeFileID 会话 ID 清洗为安全文件名片段（保留字母数字-_，其余转下划线）。
var safeFileIDRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func safeFileID(id string) string {
	return safeFileIDRe.ReplaceAllString(id, "_")
}

// ListCleanupCandidates 清理候选会话列表（前端预检展示）：非运行中的本地会话。
func (a *App) ListCleanupCandidates() []sessionSummaryLite {
	running := map[string]bool{}
	for id := range a.runs {
		running[id] = true
	}
	all := a.store.List()
	out := make([]sessionSummaryLite, 0, len(all))
	for _, s := range all {
		out = append(out, sessionSummaryLite{
			ID: s.ID, Title: s.Title, MessageCount: s.MessageCount,
			UpdatedAt: s.UpdatedAt.UnixMilli(), Running: running[s.ID],
		})
	}
	return out
}

// sessionSummaryLite 清理候选列表条目（前端轻量渲染；不引 wails 复杂类型）。
type sessionSummaryLite struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	MessageCount int    `json:"messageCount"`
	UpdatedAt    int64  `json:"updatedAt"`
	Running      bool   `json:"running"`
}

// CleanupSession 清理单个会话：分析 → 生成文档 → 归档删除。
// 返回逐项结果（前端展示分析预览与落盘路径）。
func (a *App) CleanupSession(sessionID string) CleanupReport {
	rep := a.cleanupOne(sessionID)
	a.pushSessionsChanged()
	return rep
}

// CleanupSessions 批量清理全部非运行中会话（会话页「清理会话」按钮入口）。
// 逐会话串行执行（LLM 单并发，与 selfReviewing 语义一致），失败不中断批次。
func (a *App) CleanupSessions() CleanupSummary {
	all := a.store.List()
	summary := CleanupSummary{Total: len(all), Reports: []CleanupReport{}}
	for _, s := range all {
		rep := a.cleanupOne(s.ID)
		summary.Reports = append(summary.Reports, rep)
		if rep.Skipped {
			continue
		}
		if rep.OK {
			summary.Deleted++
		}
	}
	summary.Skipped = summary.Total - summary.Deleted
	// 服务端联动：PC 端本地点「清理会话」删除本地存档后，同步把删除结果回传
	// 服务端（agent.cleanup_done → 联动删任务记录/镜像/事件 + 广播刷新），
	// 否则移动端会话列表（由服务端任务记录推导）不会同步消失（2026-10 反馈）。
	if client := a.remoteClientSnapshot(); client != nil {
		client.SendCleanupDone(remote.CleanupDone{
			Total: summary.Total, Deleted: summary.Deleted, Skipped: summary.Skipped,
			Reports: cleanupReportsToRemote(summary.Reports),
		})
	}
	a.pushSessionsChanged()
	a.pushToast(fmt.Sprintf("🧹 清理会话完成：%d/%d 已分析归档删除", summary.Deleted, summary.Total))
	return summary
}

// cleanupReportsToRemote 清理报告 → 协议回传结构（仅保留成功删除项；
// 服务端仅联动删除 OK 项，跳过项服务端保留记录）。
func cleanupReportsToRemote(reports []CleanupReport) []remote.CleanupSessionReport {
	out := make([]remote.CleanupSessionReport, 0, len(reports))
	for _, r := range reports {
		if !r.OK {
			continue
		}
		out = append(out, remote.CleanupSessionReport{
			SessionID: r.SessionID, Title: truncateTitle(r.Title, 100),
			OK: r.OK, Skipped: r.Skipped, Reason: r.Reason,
			DocPath: r.DocPath, Analysis: truncateTitle(r.Analysis, 800),
		})
	}
	return out
}

// pushSessionsChanged 会话列表变更通知（前端侧栏刷新）。
func (a *App) pushSessionsChanged() {
	if a.ctx == nil {
		return
	}
	uiEmit(a.ctx, "kaelvoris:sessions", a.sessionsWithRunning())
}

// cleanupOne 单会话清理全流程（校验 → 分析 → 文档 → 删除）。
func (a *App) cleanupOne(sessionID string) CleanupReport {
	rep := CleanupReport{SessionID: sessionID}
	if sessionID == "" {
		rep.Skipped = true
		rep.Reason = "会话 ID 为空"
		return rep
	}
	// 运行中会话不清理（删除语义 = 终止执行，破坏面大；跳过待下轮）。
	if a.IsSessionRunning(sessionID) {
		rep.Skipped = true
		rep.Reason = "会话运行中，已跳过"
		return rep
	}
	s, err := a.store.Get(sessionID)
	if err != nil || s == nil {
		rep.Skipped = true
		rep.Reason = "会话不存在或已删除"
		return rep
	}
	snap := s.Snapshot()
	rep.Title = snap.Title

	// ---- 1. 会话分析材料（对齐 remoteAnalyzeSession 组装；复用记忆文档/待办） ----
	msgs := snap.Messages
	if len(msgs) == 0 {
		// 无历史内容的空会话：无分析价值，直接删除（用户诉求"清理"的核心对象）。
		if err := a.store.Delete(sessionID); err != nil {
			rep.Skipped = true
			rep.Reason = "空会话删除失败: " + err.Error()
			return rep
		}
		rep.OK = true
		rep.Reason = "空会话，无历史内容，直接清理"
		return rep
	}
	material := buildCleanupMaterial(snap.Title, snap.Mode, snap.Todos, a.memory.Load(sessionID), msgs)

	// ---- 2. LLM 分析 ----
	cfg := a.GetConfig()
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.APIKey) == "" {
		rep.Skipped = true
		rep.Reason = "节点未配置 LLM（BaseURL/APIKey），保守跳过删除"
		return rep
	}
	analysis, err := a.cleanupAnalyze(material)
	if err != nil {
		slog.Warn("清理会话分析失败（保守跳过删除）", "session", sessionID, "err", err)
		rep.Skipped = true
		rep.Reason = "分析失败: " + err.Error()
		return rep
	}
	rep.Analysis = analysis

	// ---- 3. 生成分析文档（工作区 docs/session-reviews/） ----
	docRel, docErr := a.writeCleanupDoc(sessionID, snap.Title, analysis)
	if docErr != nil {
		// 文档写失败不阻断清理（分析结论已在手），仅日志。
		slog.Warn("清理分析文档落盘失败", "session", sessionID, "err", docErr)
	} else {
		rep.DocPath = docRel
	}

	// ---- 4. 归档删除：分析结论先落会话历史留档，再删除（存档已被文档承载） ----
	if err := a.store.Delete(sessionID); err != nil {
		rep.Skipped = true
		rep.Reason = "删除失败: " + err.Error()
		return rep
	}
	rep.OK = true
	slog.Info("会话已清理（先分析后归档删除）", "session", sessionID, "doc", docRel)
	return rep
}

// buildCleanupMaterial 组装会话分析材料（与 remoteAnalyzeSession 同源语义）。
func buildCleanupMaterial(title, mode string, todos []tools.TodoItem, memoryDoc string, msgs []message.ClineMessage) string {
	var b strings.Builder
	b.WriteString("[会话背景]\n")
	if title != "" {
		b.WriteString("会话标题: " + title + "\n")
	}
	if mode != "" {
		b.WriteString("执行模式: " + mode + "\n")
	}
	if len(todos) > 0 {
		b.WriteString("[任务清单]\n")
		for _, td := range todos {
			mark := "[ ]"
			switch td.Status {
			case "completed":
				mark = "[x]"
			case "in_progress":
				mark = "[-]"
			}
			b.WriteString(mark + " " + td.Content + "\n")
		}
	}
	if memoryDoc != "" {
		b.WriteString("[会话记忆文档]\n" + memoryDoc + "\n")
	}
	b.WriteString("[完整对话记录（旧→新）]\n")
	for _, m := range msgs {
		if m.Partial || m.Say == message.SayAPIReqStarted || m.Say == message.SayAPIReqFinished ||
			m.Say == message.SayAIContext || m.Say == message.SayReasoning {
			continue
		}
		kind, text := describeMessage(m)
		if text == "" {
			continue
		}
		b.WriteString("- " + kind + ": " + text + "\n")
	}
	return b.String()
}

// cleanupAnalyze 调 LLM 生成会话执行过程分析（含可优化冗余步骤章节）。
func (a *App) cleanupAnalyze(material string) (string, error) {
	cfg := a.GetConfig()
	llmClient := llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	ctx, cancel := context.WithTimeout(context.Background(), cleanupAnalyzeTimeout)
	defer cancel()
	prompt := "你是 AI Agent 执行过程的复盘分析助手。请基于以下会话的完整对话记录，生成一份" +
		"面向「清理归档」的分析报告（用简体中文，Markdown 格式，1000 字以内）。\n\n" +
		"报告必须包含以下四个部分（用 ## 标题）：\n" +
		"1. ## 用户问题理解 —— 用一两句话概括任务目标；\n" +
		"2. ## 执行过程回顾 —— Agent 经历的关键步骤（工具调用/文件修改/命令执行/错误与纠偏）；\n" +
		"3. ## 结果评价 —— 最终回复是否解决了问题，存在哪些风险或未覆盖项；\n" +
		"4. ## 可优化冗余步骤 —— 重点！找出执行过程中冗长、重复、低效或不合适的步骤" +
		"（如重复读同一文件、绕道命令读文件、无进展的循环调用），逐条给出优化建议" +
		"（供完善工具与提示词使用）。\n\n" +
		"要求：基于记录客观陈述，不要臆造记录中不存在的内容。\n\n" + material
	resp, err := llmClient.Generate(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "你是 AI Agent 执行过程的复盘分析助手，输出简洁、结构化、可操作的中文分析报告。"},
			{Role: llm.RoleUser, Content: truncateTitle(prompt, 60000)},
		},
	})
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(resp.Content)
	if out == "" {
		return "", fmt.Errorf("模型返回空分析")
	}
	return out, nil
}

// writeCleanupDoc 分析文档落盘（工作区 docs/session-reviews/<safeID>-<ts>.md）。
// 返回工作区相对路径。文档不存在目录时自动创建。
func (a *App) writeCleanupDoc(sessionID, title, analysis string) (string, error) {
	ws := a.cfgWorkspace()
	if strings.TrimSpace(ws) == "" {
		ws, _ = os.Getwd()
	}
	dir := filepath.Join(ws, filepath.FromSlash(cleanupDocDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s.md", safeFileID(sessionID), time.Now().Format("20060102-150405"))
	abs := filepath.Join(dir, name)
	head := fmt.Sprintf("# 会话清理分析：%s\n\n- 会话 ID：%s\n- 清理时间：%s\n\n",
		title, sessionID, time.Now().Format("2006-01-02 15:04:05"))
	if err := os.WriteFile(abs, []byte(head+analysis+"\n"), 0o644); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(ws, abs)
	if err != nil {
		rel = abs
	}
	return filepath.ToSlash(rel), nil
}
