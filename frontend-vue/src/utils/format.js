// ═════════════════════════════════════════════════════════════════
// 通用格式化工具
// ═════════════════════════════════════════════════════════════════

// fmtMsgStamp 消息时间戳（桌面端消息流）：入参 ns（ClineMessage.TS）或 ms（TaskEvent.TS），
// >1e15 视为纳秒自动归一。同日 HH:MM，跨日 M/D HH:MM；无值返回空串。
export function fmtMsgStamp(ts) {
  if (!ts) return "";
  let n = Number(ts);
  if (!isFinite(n) || n <= 0) return "";
  if (n > 1e14) n = Math.round(n / 1e6); // ns → ms
  const d = new Date(n);
  if (isNaN(d.getTime())) return "";
  const now = new Date();
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  if (d.toDateString() === now.toDateString()) return `${hh}:${mm}`;
  return `${d.getMonth() + 1}/${d.getDate()} ${hh}:${mm}`;
}

export function fmtTime(ts) {
  if (!ts) return "";
  const d = ts instanceof Date ? ts : new Date(ts);
  if (isNaN(d.getTime())) return "";
  const now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  if (sameDay) return `${hh}:${mm}`;
  const M = d.getMonth() + 1, D = d.getDate();
  return `${M}/${D} ${hh}:${mm}`;
}

export function fmtClockNs(ns) {
  let n = Number(ns || 0);
  if (!n) return "";
  if (n > 1e14) n = Math.round(n / 1e6); // ns → ms（ClineMessage.TS 为纳秒，TaskEvent.TS 为毫秒）
  const d = new Date(n);
  if (isNaN(d.getTime())) return "";
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  const ss = String(d.getSeconds()).padStart(2, "0");
  return `${hh}:${mm}:${ss}`;
}

export function fmtDurSec(sec) {
  const s = Number(sec || 0);
  if (s < 1) return Math.round(s * 1000) + "ms";
  if (s < 60) return s.toFixed(1) + "s";
  return Math.floor(s / 60) + "m" + Math.round(s % 60) + "s";
}

export function fmtTok(n) {
  n = Number(n || 0);
  return n >= 1000 ? (n / 1000).toFixed(1) + "k" : String(n);
}

// 截断路径中间段：D:/code/very/long/path → D:/code/…/path
export function shortPath(p, max = 42) {
  const s = String(p || "");
  if (s.length <= max) return s;
  const head = s.slice(0, Math.max(0, max - 12));
  return head + "…" + s.slice(-10);
}

// ═══ 会话卡片信息升级（规划板 2026-10）═══

// relTime 相对时间（会话卡 meta 行）：<1min 刚刚；<1h N 分钟前；今日 HH:MM；
// 昨日 昨天 HH:MM；7 天内 N 天前；更早 YYYY/M/D。入参兼容 ISO 字符串/Date/毫秒。
export function relTime(ts) {
  if (!ts) return "";
  const d = ts instanceof Date ? ts : new Date(ts);
  if (isNaN(d.getTime())) return "";
  const now = new Date();
  const diffMs = now - d;
  const min = Math.floor(diffMs / 60000);
  if (min < 1) return "刚刚";
  if (min < 60) return min + " 分钟前";
  const dayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const dStart = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const dayDiff = Math.round((dayStart - dStart) / 86400000);
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  if (dayDiff <= 0) return `${hh}:${mm}`;        // 同日（含时钟回拨兑底）
  if (dayDiff === 1) return `昨天 ${hh}:${mm}`;
  if (dayDiff < 7) return `${dayDiff} 天前`;
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`;
}

// dateGroup 日期分组名（会话列表分组标题）：今天 / 昨天 / 7 天内 / 更早。
// key 为稳定分组键（同组会话归入同一渲染组，滚动不抖）。
export function dateGroup(ts) {
  const empty = { name: "", key: "" };
  if (!ts) return empty;
  const d = ts instanceof Date ? ts : new Date(ts);
  if (isNaN(d.getTime())) return empty;
  const now = new Date();
  const dayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const dStart = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const dayDiff = Math.round((dayStart - dStart) / 86400000);
  if (dayDiff <= 0) return { name: "今天", key: "today" };
  if (dayDiff === 1) return { name: "昨天", key: "yesterday" };
  if (dayDiff < 7) return { name: "7 天内", key: "week" };
  return { name: "更早", key: "earlier" };
}

// clipSummary 摘要单行显示截断（rune 安全；CSS ellipsis 为主，超长
// payload 双重保险截断，防超长 base64/日志撑爆 DOM）。
export function clipSummary(s, max = 120) {
  const str = String(s || "").replace(/\s+/g, " ").trim();
  const r = Array.from(str);
  if (r.length <= max) return str;
  return r.slice(0, max).join("") + "…";
}
