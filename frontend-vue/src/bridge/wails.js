// ═════════════════════════════════════════════════════════════════
// 桥接层：Wails 绑定统一入口
//   - Wails 环境下直接调用 window.go.desktop.App.*（绑定 key = Go 包名 desktop）
//   - 非桌面环境（浏览器预览）走 mock-install.js 注入的桩
// ═════════════════════════════════════ resolve.kaelvoris.app
export function wailsAvailable() {
  return !!(window.go && window.go.desktop && window.go.desktop.App);
}

export function bridgeCall(fn, ...args) {
  const app = window.go && window.go.desktop && window.go.desktop.App;
  if (!app) return Promise.reject(new Error("Wails 绑定不可用（桌面端外运行）"));
  // 绑定调用统一 null 数组兜底层（规划板 2026-10，承接被驳的 nil 切片后端口径条目）：
  // Go nil 切片经 Wails 序列化为 JS null，顶层列表调用方已有 || [] 兜底，漏网的是
  // 嵌套对象内的数组字段（Employee.roleIds、OrgView.positions、DeptTree.children
  // 等）→ .filter/.map/.length 直接抛错（998f79e 连修三处的同类根因）。统一在分发
  // 点对已知数组键白名单归一 null→[]，消费方免逐点打补丁。
  return app[fn](...args).then(nullArraysToEmpty);
}

// 数组类字段白名单：仅这些键的 null 归一为 []。不做「有 length 就当数组」的
// 通用猜测——RoadmapData.stats、GetFlow.canvas、GhPRView 等 null 判空语义
// 依赖保持 null 不变（避免把「无数据」误判为「空列表」）。
const ARRAY_KEYS = new Set([
  // 列表型数据（列表调用返回 { items/sessions/... } 或嵌套结构）
  "sessions", "employees", "plugins", "items", "nodes", "channels",
  "modes", "models", "projects", "skills", "categories", "apps", "flows",
  // EHR 组织架构（GetOrg / OrgTree）
  "departments", "positions", "roles", "children",
  // 数字员工（Employee：表单 roleIds/mcpServers/mcpTools/skillsDeny/dirs）
  "roleIds", "mcpServers", "mcpTools", "skillsDeny", "dirs",
  // 会话 / 消息 / Git / A2A / 更新
  "messages", "images", "staged", "unstaged", "members", "notes",
]);

// 深度遍历归一：null→[] 仅作用于白名单键；数组与对象递归下钻。
// Wails 返回值为 JSON 反序列化产物（无循环引用），depth 上限 4 兜底防御。
export function nullArraysToEmpty(v, depth = 0) {
  if (!v || typeof v !== "object" || depth > 4) return v;
  if (Array.isArray(v)) {
    for (let i = 0; i < v.length; i++) v[i] = nullArraysToEmpty(v[i], depth + 1);
    return v;
  }
  for (const k of Object.keys(v)) {
    if (v[k] === null && ARRAY_KEYS.has(k)) v[k] = [];
    else v[k] = nullArraysToEmpty(v[k], depth + 1);
  }
  return v;
}

// 事件订阅：优先 Wails runtime（窗口打开前 EventsOn 可能未就绪，
// Wails 会在 window.runtime 就绪前缓存事件，无需特殊处理）
export function onEvent(name, handler) {
  if (window.runtime && window.runtime.EventsOn) {
    return window.runtime.EventsOn(name, (data) => handler(data));
  }
  // 浏览器预览：支持 dev 时手动 window.__emit("kaelvoris:toast", "...")
  window.__kaelvoris_listeners = window.__kaelvoris_listeners || {};
  (window.__kaelvoris_listeners[name] = window.__kaelvoris_listeners[name] || []).push(handler);
  return () => {
    const list = window.__kaelvoris_listeners[name] || [];
    const i = list.indexOf(handler);
    if (i >= 0) list.splice(i, 1);
  };
}

export function emitLocal(name, data) {
  if (window.__kaelvoris_listeners && window.__kaelvoris_listeners[name]) {
    for (const fn of window.__kaelvoris_listeners[name]) fn(data);
  }
}
