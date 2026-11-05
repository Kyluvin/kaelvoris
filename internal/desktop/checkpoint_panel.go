package desktop

// checkpoint_panel.go 检查点回滚 Wails 绑定（对标增强 D1，doc/10-对标增强 第三批）。
// 复用 B1 休眠的 checkpoint.Store.Restore/List API，产品化为前端可调用的绑定：
//   - ListCheckpoints：会话全部检查点（新→旧），供前端「检查点」列表展示；
//   - RestoreCheckpoint：回滚单条目（恢复前自动记 tool="restore" 安全快照，
//     可再次回滚），成功后推送 checkpoint_frontend 事件 + 恢复通知入队
//     （下次任务注入 [checkpoint-restore] 前缀通知，对齐 Gemini CLI rewind
//     "inform the model" 语义）。
// 本批仅落内核机制 + Wails 绑定，前端接入后续批次（对齐 C2 审批矩阵模式）。

import (
	"fmt"
	"path/filepath"

	"kaelvoris/internal/checkpoint"
	"kaelvoris/pkg/message"
)

// CheckpointEntry 前端可见条目（从 checkpoint.Entry 投影，隐藏内部字段）。
type CheckpointEntry struct {
	ID    string `json:"id"`    // 条目 ID（RestoreCheckpoint 用）
	Seq   int    `json:"seq"`   // 会话内序号（1 起）
	Time  int64  `json:"time"`  // unix 毫秒
	Tool  string `json:"tool"`  // fs_write | fs_apply_diff | restore
	Path  string `json:"path"`  // 规范化相对路径（正斜杠）
	IsNew bool   `json:"isNew"` // 记录时文件不存在（恢复 = 删除该文件）
}

// ListCheckpoints 返回会话的可回滚检查点（新→旧；无记录 = 空数组）。
func (a *App) ListCheckpoints(sessionID string) []CheckpointEntry {
	s := checkpoint.NewStore(checkpointDir(), sessionID, 0)
	entries := s.List()
	out := make([]CheckpointEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, CheckpointEntry{
			ID:    e.ID,
			Seq:   e.Seq,
			Time:  e.Time,
			Tool:  e.Tool,
			Path:  e.Path,
			IsNew: e.IsNew,
		})
	}
	// List 按 Seq 升序（旧→新）；前端展示新→旧，就地反转。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// RestoreCheckpoint 回滚单条目：恢复前自动记当前内容为安全快照（可再次回滚）→
// 写回/删除 → 推送 SayCheckpointFrontend 事件（前端即时反馈）→ 恢复通知入队
// （下次任务启动时注入模型）。失败（条目不存在/磁盘错误）直接返回 err。
func (a *App) RestoreCheckpoint(sessionID, entryID string) error {
	s := checkpoint.NewStore(checkpointDir(), sessionID, 0)
	// 恢复写盘需要工作区根（与 buildRunEngine 的 cps.SetWorkspaceRoot 一致）：
	// 取会话关联工作区，旧会话无记录时退回全局配置工作区。
	ws := a.cfgWorkspace()
	if sess, err := a.store.Get(sessionID); err == nil && sess != nil {
		if w := sess.Snapshot().Workspace; w != "" {
			ws = w
		}
	}
	if abs, err := filepath.Abs(ws); err == nil {
		s.SetWorkspaceRoot(abs)
	}
	e, err := s.Restore(entryID)
	if err != nil {
		return err
	}
	note := fmt.Sprintf("已回滚 %s（安全快照 %s）", e.Path, entryID)
	uiEmit(a.ctx, "cline:message", map[string]any{
		"sessionId": sessionID,
		"message":   message.NewSay(message.SayCheckpointFrontend, note),
	})
	// 恢复通知入队：下次任务注入（run 不存在也记录，runTask 启动时 drain）。
	a.enqueueRestoreNote(sessionID, fmt.Sprintf("%s 已回滚到检查点 %s 时的状态（基于磁盘当前内容继续，勿假设旧内容仍存在）", e.Path, entryID))
	return nil
}

// enqueueRestoreNote 追加会话恢复通知（D1；App 级运行态，会话未运行也记录）。
func (a *App) enqueueRestoreNote(sessionID, note string) {
	a.restoreMu.Lock()
	if a.restoreNotes == nil {
		a.restoreNotes = map[string][]string{}
	}
	a.restoreNotes[sessionID] = append(a.restoreNotes[sessionID], note)
	a.restoreMu.Unlock()
}

// drainRestoreNotes 一次性取走会话全部恢复通知（FIFO；runTask 任务启动时拼接
// 为 [checkpoint-restore] 前缀注入首个任务帧——模型必然看到，不入系统提示词）。
func (a *App) drainRestoreNotes(sessionID string) string {
	a.restoreMu.Lock()
	notes := a.restoreNotes[sessionID]
	delete(a.restoreNotes, sessionID)
	a.restoreMu.Unlock()
	if len(notes) == 0 {
		return ""
	}
	out := notes[0]
	for _, n := range notes[1:] {
		out += "\n" + n
	}
	return out
}

// grantSkillsToRuns 把技能当轮授权写入全部活跃 run 的引擎（D3；SkillTool.
// OnAuthorize 回调入口）。ponytail 简化：同一时刻多 run 并行时广播所有活跃
// 引擎（授权粒度 = 当轮工具调用，误授面为同轮其他会话的同名工具调用；红线
// 兜底 + 授权仅当轮有效，风险可接受；精确路由需引擎持有 runID，本批不做）。
func (a *App) grantSkillsToRuns(patterns []string) {
	a.runsMu.Lock()
	defer a.runsMu.Unlock()
	for _, run := range a.runs {
		if run != nil && run.engine != nil {
			run.engine.GrantSkills(patterns)
		}
	}
}
