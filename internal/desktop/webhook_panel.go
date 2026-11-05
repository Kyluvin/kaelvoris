// webhook_panel.go —— B3 webhook 触发宿主接线（对标 OpenHands Automation
// Webhook）：启动时按配置监听 127.0.0.1:port；命中触发经 SendMessageUnattended
// 同一引擎执行路径（无人值守语义，ask 自动放行）。
package desktop

import (
	"log/slog"
	"strconv"

	"kaelvoris/internal/webhook"
)

// hookStore 惰性创建 hook 定义存储。
func (a *App) hookStore() *webhook.Store {
	a.hookMu.Lock()
	defer a.hookMu.Unlock()
	if a.hookStoreRef == nil {
		a.hookStoreRef = webhook.LoadOrInit(hooksTriggerPath())
	}
	return a.hookStoreRef
}

// hookLog 惰性创建触发记录。
func (a *App) hookLog() *webhook.Log {
	a.hookMu.Lock()
	defer a.hookMu.Unlock()
	if a.hookLogRef == nil {
		a.hookLogRef = webhook.NewLog(hooksTriggerLogPath())
	}
	return a.hookLogRef
}

// startWebhookServer 按配置启动触发监听（缺省关；启动失败仅日志不 crash）。
// 在 startup 流程调用。
func (a *App) startWebhookServer() {
	a.cfgMu.Lock()
	enabled := a.cfg.WebhookEnabled
	port := a.cfg.WebhookPort
	a.cfgMu.Unlock()
	if !enabled {
		return
	}
	if port <= 0 {
		port = 18923
	}
	srv := webhook.NewServer(a.hookStore(), a.hookLog(), func(h webhook.Hook) {
		// 触发 = 无人值守任务：复用 SendMessageUnattended（引擎/审批/事件桥全复用，
		// ask 自动放行语义与 cron 触发路径一致）。
		sess := a.defaultSessionID()
		if sess == "" {
			slog.Warn("webhook trigger skipped: no session", "hook", h.ID)
			return
		}
		if _, err := a.SendMessageUnattended(sess, h.Task, nil); err != nil {
			slog.Warn("webhook trigger task failed", "hook", h.ID, "err", err)
		}
	})
	srv.MaxPerMinute = 10
	if err := srv.Start(port); err != nil {
		slog.Warn("webhook server start failed", "port", port, "err", err)
		return
	}
	a.hookSrv = srv
	slog.Info("webhook server listening", "addr", "127.0.0.1:"+strconv.Itoa(port))
}

// stopWebhookServer 优雅关闭（退出流程调用；幂等）。
func (a *App) stopWebhookServer() {
	a.hookMu.Lock()
	srv := a.hookSrv
	a.hookMu.Unlock()
	if srv != nil {
		srv.Stop()
	}
}

// ListHookTriggers 面板绑定：hook 列表（后续前端设置面板迭代用）。
func (a *App) ListHookTriggers() ([]webhook.Hook, error) {
	return a.hookStore().List()
}

// SaveHookTriggers 面板绑定：全量保存。
func (a *App) SaveHookTriggers(hooks []webhook.Hook) error {
	return a.hookStore().Save(hooks)
}

// HookTriggerHistory 面板绑定：最近触发记录。
func (a *App) HookTriggerHistory() []webhook.TriggerLog {
	return a.hookLog().History()
}

// defaultSessionID 当前活跃（或最近）会话 ID：webhook/cron 类无人触发落点。
// 取会话列表第一个（与前端默认选中一致；无会话返回空）。
func (a *App) defaultSessionID() string {
	sessions := a.ListSessions()
	if len(sessions) == 0 {
		return ""
	}
	return sessions[0].ID
}
