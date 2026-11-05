// ── 会话草稿暂存（localStorage，按会话 ID 隔离）──────────────────
// 需求（规划板 2026-10）：未发送草稿按会话 ID 暂存，切回会话自动恢复；
// 新会话（无 ID）草稿存 "new" 槽位。存取均做 JSON 容错，损坏即弃不致命。
const DRAFT_KEY = "kaelvoris.composer.drafts";
// 存储上限：草稿总量超出即丢弃最旧条目，防止长会话积累撑爆 localStorage
const MAX_DRAFTS = 100;

function readAll() {
  try {
    const raw = localStorage.getItem(DRAFT_KEY);
    const obj = raw ? JSON.parse(raw) : null;
    return obj && typeof obj === "object" && !Array.isArray(obj) ? obj : {};
  } catch {
    return {}; // JSON 损坏/隐私模式：按无草稿处理
  }
}

function writeAll(all) {
  try {
    // 超限裁剪：保留最近 MAX_DRAFTS 条（无时间戳字段，按键插入序即可近似）
    const keys = Object.keys(all);
    if (keys.length > MAX_DRAFTS) {
      for (const k of keys.slice(0, keys.length - MAX_DRAFTS)) delete all[k];
    }
    localStorage.setItem(DRAFT_KEY, JSON.stringify(all));
  } catch {
    // localStorage 满/不可用：静默降级（草稿仅存内存态）
  }
}

// 读取指定会话草稿（无则返回空串）
export function getDraft(sessionId) {
  if (!sessionId) return "";
  const all = readAll();
  const d = all[sessionId];
  return d && typeof d.text === "string" ? d.text : "";
}

// 写入指定会话草稿（text 为空串时删除该条目）
export function setDraft(sessionId, text) {
  const all = readAll();
  if (text) {
    all[sessionId] = { text, ts: Date.now() };
  } else {
    delete all[sessionId];
  }
  writeAll(all);
}

// 删除指定会话草稿（发送成功后调用）
export function clearDraft(sessionId) {
  setDraft(sessionId, "");
}
