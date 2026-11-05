// task_log.go —— 任务运行日志导出（B2 对标增强：对标 OpenHands
// Event Stream 回放 / Goose 会话诊断导出）。
//
// 任务结束后（OnTaskFinished 链）把会话消息序列化为单文件 Markdown 报告，
// 落 ~/.kaelvoris/tasklogs/，滚动保留最近 30 份。纯本地序列化（无 LLM 调用），
// 失败静默（仅日志），不影响主流程。
package desktop

import (
	"log/slog"
	"os"
	"time"

	"kaelvoris/internal/tasklog"
	"kaelvoris/pkg/message"
)

// maybeTaskLog 任务结束后异步导出运行报告（不阻塞收尾）。
func (a *App) maybeTaskLog(sessionID, task, result string, taskErr error, elapsed time.Duration) {
	a.cfgMu.Lock()
	disabled := a.cfg.TaskLogDisabled
	a.cfgMu.Unlock()
	if disabled {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("tasklog panic", "session", sessionID, "panic", r)
			}
		}()
		// 会话快照（内存缓存读 + 拷贝，无磁盘 IO；Message/Mode/Model 需锁保护）。
		var msgs []message.ClineMessage
		mode, model := "", ""
		if sess, err := a.store.Get(sessionID); err == nil && sess != nil {
			msgs = sess.MessagesSnapshot()
			sess.Mu().RLock()
			mode, model = sess.Mode, sess.Model
			sess.Mu().RUnlock()
		}
		status, errText := "done", ""
		if taskErr != nil {
			if taskErr.Error() == "context canceled" {
				status = "stopped"
			} else {
				status = "failed"
			}
			errText = taskErr.Error()
		}
		meta := tasklog.Meta{
			SessionID: sessionID,
			Mode:      mode,
			Model:     model,
			Task:      task,
			Status:    status,
			Elapsed:   elapsed.Round(time.Second).String(),
			ErrText:   errText,
		}
		// 文件变更摘要：取会话消息中最近一条 turn_diff（引擎收尾产出）。
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Type == message.TypeSay && msgs[i].Say == message.SayTurnDiff {
				meta.ChangeSummary = msgs[i].Text
				break
			}
		}
		if _, err := tasklog.WriteReport(tasklogDir(), meta, msgs, 30); err != nil {
			slog.Warn("tasklog write failed", "session", sessionID, "err", err)
		}
		_ = os.Getpid() // 保持 os 导入可用（后续可扩展磁盘配额检查）
	}()
}
