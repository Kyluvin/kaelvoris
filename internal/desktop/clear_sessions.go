// clear_sessions.go —— PC 端会话列表「清空 / 总结并清空」按钮（2026 用户需求）。
//
// 对齐移动端会话列表页的「清空 / 总结并清理」语义（clear.tasks 链路），但由于
// PC 端本地就是执行节点，无需经服务端路由：核心直接复用 remote_panel.go 的
// summarizeClearCore / clearLocalSessions（收集未完成事实 → LLM 汇总 → 清空本地
// 存档），收尾时再做两处本链路特有动作：
//   - 总结并清空：新建本地会话并自动运行续作任务（移动端由服务端 CreateTask
//     派发回节点；PC 本地发起则直接本地建会话，避免双端重复执行）；
//   - 服务端联动：远程在线时回传 CleanupDone{Mode:"clear"}（节点已清空的权威
//     声明），服务端据此整表清空任务/事件/镜像并广播 clear.done（移动端列表
//     同步收敛 —— 2026-10「PC 清理后移动端不更新」根因的对齐举措）。
package desktop

import (
	"fmt"
	"log/slog"

	"kaelvoris/internal/remote"
)

// ClearAllSessions 清空 PC 端全部本地会话存档（侧栏「清空」按钮，同步执行，
// 秒级无 LLM）：运行中先停止、队列丢弃、remote 映射清理，再逐个删除（内存 +
// 磁盘 + 记忆文档）；远程在线时同步服务端整表清空。返回删除数。
func (a *App) ClearAllSessions() (int, error) {
	deleted, total := a.clearLocalSessions()
	if total == 0 {
		return 0, nil
	}
	// 服务端联动：节点本地已清空的权威声明 → 服务端 ClearAllTasks + clear.done
	// 广播（移动端列表由服务端任务记录推导，不同步则手机端会话卡残留）。
	if client := a.remoteClientSnapshot(); client != nil {
		client.Send(remote.TypeAgentCleanupDone, remote.CleanupDone{
			Mode: "clear", Total: total, Deleted: deleted,
		})
	}
	a.pushSessionsChanged()
	a.pushToast(fmt.Sprintf("🧹 已清空本地会话 %d/%d", deleted, total))
	return deleted, nil
}

// SummarizeClearSessions 总结并清空（侧栏「总结并清空」按钮，异步执行）：
// 先收集未完成事实（运行中/失败/无终态会话 + 未完成 todos）经 LLM 汇总为一条
// 「续作任务指令」，清空全部本地会话后新建本地会话自动运行续作任务；远程在线
// 时同步服务端清空。受理立即返回（LLM 汇总耗时 5 分钟内），完成经
// kaelvoris:cleanup-done 事件告知前端（复位忙碌态 + 当前会话被删时回空态）。
func (a *App) SummarizeClearSessions() {
	total := len(a.store.List())
	if total == 0 {
		a.pushToast("暂无可总结的会话")
		return
	}
	a.pushToast("📝 已受理：正在汇总未完成任务，完成后自动清空并新建续作会话…")
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("总结并清空 panic（已拦截）", "panic", r)
				uiEmit(a.ctx, "kaelvoris:cleanup-done", map[string]any{"ok": false, "error": "总结并清空异常结束"})
			}
		}()
		summary, deleted, _, unfinished := a.summarizeClearCore()
		// 续作任务：汇总成功 → 新建本地会话并自动运行（PC 本地为执行面，
		// 不上行服务端创建 —— 避免服务端再派发一次造成双端重复执行）。
		if summary != "" {
			if _, err := a.sendMessageOpts("", summary, nil, "", nil); err != nil {
				slog.Warn("总结并清空：续作任务创建失败", "err", err)
				a.pushToast("⚠️ 续作任务创建失败：" + err.Error())
			} else {
				a.pushToast(fmt.Sprintf("🧹 已汇总 %d 项未完成工作并清空 %d 个会话，续作任务已开始运行", unfinished, deleted))
			}
		} else if unfinished > 0 {
			a.pushToast("⚠️ 未能汇总未完成任务（LLM 未配置或调用失败），本地会话已清空")
		} else {
			a.pushToast(fmt.Sprintf("🧹 全部会话均已完成，已清空 %d 个会话", deleted))
		}
		// 服务端联动（与清空按钮同路径）：清服务端记录 + clear.done 广播。
		if client := a.remoteClientSnapshot(); client != nil {
			client.Send(remote.TypeAgentCleanupDone, remote.CleanupDone{
				Mode: "clear", Total: total, Deleted: deleted,
			})
		}
		a.pushSessionsChanged()
		// 完成事件：前端复位忙碌态 + 当前会话被删则回到新对话态。
		uiEmit(a.ctx, "kaelvoris:cleanup-done", map[string]any{
			"ok": true, "deleted": deleted, "summarized": summary != "",
		})
	}()
}
