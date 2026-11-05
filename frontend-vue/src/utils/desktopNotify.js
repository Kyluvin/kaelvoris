// ═════════════════════════════════════════════════════════════════
// 失焦系统通知通道（共用入口）：窗口失焦时弹系统 Notification。
// 使用方：定时任务失败（kaelvoris:cron-failed）、回复完成通知（后续条目）。
//
// 语义：
//   - document.hasFocus() = true（窗口在前台）→ 不弹，防打扰；
//   - Notification 权限未授予 → 静默跳过（首次需用户在设置页授权）；
//   - 浏览器预览环境（无 Wails）同样生效（标准 Web API）。
// ═════════════════════════════════════════════════════════════════

// ensureNotifyPermission 请求系统通知权限（设置页开关打开时调用；幂等）。
// 返回 "granted" | "denied" | "default"。
export async function ensureNotifyPermission() {
  if (typeof Notification === "undefined") return "denied";
  if (Notification.permission === "default") {
    try { return await Notification.requestPermission(); } catch { return "denied"; }
  }
  return Notification.permission;
}

// notifyIfBlurred 失焦时弹系统通知（前台焦点不弹，防打扰）。
// enabled 为 false 时整体关断（设置开关）。title/body 为通知文案。
export function notifyIfBlurred(enabled, title, body) {
  if (!enabled) return;
  if (typeof document !== "undefined" && document.hasFocus()) return; // 前台不弹
  if (typeof Notification === "undefined" || Notification.permission !== "granted") return;
  try {
    const n = new Notification(title, { body, silent: false });
    n.onclick = () => { try { window.focus(); n.close(); } catch { /* 忽略 */ } };
  } catch { /* 通知失败静默（尽力而为） */ }
}
