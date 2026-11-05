// ═════════════════════════════════════════════════════════════════
// 全局应用状态（Vue reactive 单例 store，消息协议对齐原版 main.js）
//
// 事件路由（与后端 app.go 事件桥一一对应）：
//   cline:message      say/ask 消息（含 sessionId）
//   kaelvoris:sessions    会话列表推送
//   kaelvoris:todos       待办推送（sessionId）
//   kaelvoris:status      运行状态（sessionId/running）
//   kaelvoris:done        任务完成（sessionId）
//   kaelvoris:ai_context  最近一次 LLM 请求上下文
//   kaelvoris:toast       后端轻提示
//   kaelvoris:cron-failed 定时任务失败（失焦系统通知，用户可开关）
//   kaelvoris:queued      消息排队数
//   kaelvoris:usage       会话级 token/cost 计量（Claude Code /cost 对齐，实时展示不落历史）
// ═════════════════════════════════════════════════════════════════
import { reactive, computed, watch } from "vue";
import { Message } from "@arco-design/web-vue";
import { onEvent, bridgeCall, wailsAvailable } from "../bridge/wails.js";
import { parseToolMessage } from "../utils/markdown.js";
import { getDraft, setDraft, clearDraft } from "../utils/drafts.js";
import { notifyIfBlurred } from "../utils/desktopNotify.js";
import { fmtClockNs, fmtDurSec, fmtTok } from "../utils/format.js";

export const store = reactive({
  // 会话
  sessions: [],            // 会话摘要列表
  sessionsLoading: false,  // 首次拉取中（侧栏骨架态；kaelvoris:sessions 推送后复位）
  currentId: null,         // 当前聚焦会话
  sessionViews: new Map(), // sessionId -> { msgs, todos, running, queuedCount, title }
  running: {},             // sessionId -> bool
  anyRunning: false,

  // 模式 / 模型
  modes: [],
  mode: "code",
  models: [],
  model: "",

  // 项目
  projects: [],
  workspace: "",

	  // ACP agents（外部编码 agent，会话级绑定）
	  acpAgents: [],        // [{name, command, enabled, ...}]
	  currentACP: "",       // 当前会话绑定的 agent 名（空 = 内置引擎）

	  // 数字员工（用户需求 2026-10 之 5：会话级绑定，经 Composer 选择器/员工面板）
	  employees: [],          // [{id,name,avatar,status,runnable,...}]
	  currentEmployee: "",    // 当前会话绑定的员工 ID（空 = 普通会话）

	  // 远程节点（用户需求 2026-10 之 1：配置/切换远程后所有功能可操作）
	  remoteNodes: [],        // 在线节点列表（Composer 远程选择器数据源）
	  remoteConnected: false, // /ws/client 通道状态
	  remoteOnline: false,    // 账号已登录且远程开启
	  currentRemoteNode: "",  // 当前会话发布目标节点（空 = 本机执行；"auto"=任意在线节点）
	  remoteTasks: {},        // sessionID → { taskId, nodeId }（取消/审批路由用）
	  selfNodeId: "",         // 本机（执行端）节点身份（kaelvoris:remote_nodes.selfNodeId；过滤他机会话依据）
	  sessionMeta: {},        // sessionID → { mode, model, employeeId, acpAgent, nodeId }（会话自有配置快照，避免未知字段告警）

  // 输入
  inputText: "",
  pendingImages: [],
  sending: false,

  // 审批卡
  pendingAsk: null,        // { ask, text, ts, sessionId }
  lastAnsweredAskTs: 0,    // 最近一次已应答 ask 的 TS（纳秒）：防后端列表陈旧推送把已答卡片恢复回来

  // 清空/总结并清空 busy 态（侧栏按钮禁用 + 后端异步完成复位）
  cleanupBusy: false,

  // 侧栏文档区 → DocPanel 打开请求（2026-12 用户需求：文档入口放会话列表下方）。
  // 侧栏点击文件时写入，DocPanel 挂载/watch 到后打开并清空（一次性信号）。
  docOpenPath: "",

	// AI 上下文（P2.7-0b 热路径优化：仅 context 面板激活时接收快照帧，
	// activeMain 布局里 context 面板仅在 rightbar 展开 + context tab 激活时展示）
	aiContext: "",
	contextTabActive: false,   // 右栏「AI 上下文」tab 是否激活（RightPanel 维护）

  // 会话级用量（usage.Totals；Claude Code /cost 对齐，kaelvoris:usage 实时推送）
  usage: null,

  // 布局
  railCollapsed: false,
  sidebarCollapsed: false,
  rightbarCollapsed: false,
  rightbarPinned: false,   // 右栏固定钉住（2026-12 用户需求）：true 时浮层抽屉变占位停靠
  activeMain: "chat",      // chat | git | roadmap | employee | skills | cozeflow | settings-*
                           // （flow「流程编排」入口已移除，2026-10-25：左侧只保留 AI Flow）
  theme: "dark",

	  // Git 面板
	  git: null,
	  gitDiffText: "",
	  gitDiffFile: "",

	  // 自主迭代规划板（kaelvoris:roadmap 推送缓存；RoadmapPanel 挂载时也会主动拉取）
	  roadmap: null,

  // PR/CI 面板（gh CLI 桥接；ghAvailable=false 时隐藏入口）
  ghAvailable: false,
  prs: [],           // [{number,title,state,headRefName,author,isDraft,checkState,url}]
  prDetail: null,
  prLoading: false,

  // 设置抽屉
  settingsVisible: false,
  settingsPage: "general",
  config: null,
});

// ── 会话视图存取 ────────────────────────────────────────────────
export function sess(id) {
  if (!store.sessionViews.has(id)) {
    store.sessionViews.set(id, {
      id,
      msgs: [],        // 渲染消息数组（已合并流式）
      todos: [],
	      running: false,
	      queuedCount: 0,
	      streamingIdx: -1,   // 当前流式节点下标
	      reasoningIdx: -1,   // 当前思考卡片下标
	      loaded: false,      // 已加载历史
	      remote: false,      // 远程会话（事件经 kaelvoris:remote_task 路由）
    });
  }
  return store.sessionViews.get(id);
}

export const currentSession = computed(() => {
  if (!store.currentId) return null;
  return sess(store.currentId);
});

export const currentMessages = computed(() => {
  const s = store.currentId ? sess(store.currentId) : null;
  return s ? s.msgs : [];
});

export const currentTodos = computed(() => {
  const s = store.currentId ? sess(store.currentId) : null;
  return s ? s.todos : [];
});

// ── 消息渲染模型（把 ClineMessage 映射为视图消息）────────────────
// 视图消息形态：
//   { kind: 'user'|'assistant'|'reasoning'|'tool'|'tag'|'error'|'image'|'diff'|'done',
//     text, name, summary, diff, args, badge, ... }
let msgSeq = 0;
// pushView 组装视图消息：view.ts 可选透传消息时间戳（历史回放 ClineMessage.TS 纳秒 /
// 远程 TaskEvent.TS 毫秒 / 实时推送同 TS），供 MessageItem 时间戳行展示；无 ts 不显示。
function pushView(sv, view) {
  view._id = ++msgSeq;
  sv.msgs.push(view);
  return view;
}

// 事件：say/ask 消息消费（流式合并逻辑对齐原版 onClineMessage）
export function onClineMessage(payload) {
  if (!payload || !payload.message) return;
  const sid = payload.sessionId || store.currentId || "default";
  const m = payload.message;
  const sv = sess(sid);
  sv.loaded = true;

  // 1) reasoning 流式（累计全文覆盖最后一张思考卡）
  if (m.type === "say" && m.say === "reasoning") {
    if (sv.reasoningIdx >= 0 && sv.msgs[sv.reasoningIdx]) {
      sv.msgs[sv.reasoningIdx].text = m.text || "";
      if (!m.partial) sv.reasoningIdx = -1;
    } else {
      const v = pushView(sv, { kind: "reasoning", text: m.text || "", ts: m.ts, streaming: !!m.partial });
      if (m.partial) sv.reasoningIdx = sv.msgs.indexOf(v);
    }
    return;
  }

  // 2) text partial 流式（覆盖最后流式节点）
  if (m.type === "say" && m.say === "text" && m.partial) {
    let node = sv.streamingIdx >= 0 ? sv.msgs[sv.streamingIdx] : null;
    // 防御：索引残留指向已定型节点（新流开始 api_req_started 已清，此处兜底）→ 重建
    if (node && !node.streaming) node = null;
    if (!node) {
      node = pushView(sv, { kind: "assistant", text: m.text || "", ts: m.ts, streaming: true });
      sv.streamingIdx = sv.msgs.indexOf(node);
    } else {
      node.text = m.text || "";
    }
    return;
  }

  // 注意：此处不再无条件 sv.streamingIdx = -1——引擎定型顺序为 partials →
  // api_req_finished → 定型消息（对齐 session.AppendMerged 修复语义），中间帧清空
  // 索引会导致定型 text 无法复用流式节点，同一段回复渲染两条（重复根因）。
  // 流式周期清理：text 定型分支复用完成后自清；新周期 api_req_started 重置；
  // 任务结束 kaelvoris:status/running=false 重置。

  // 3) ask（实时审批卡片）
  if (m.type === "ask") {
    if (m.ask === "completion_result") {
      // 完成结果：渲染 done 卡（任务完成）；清除本会话挂起审批（若有）。
      if (m.text) pushView(sv, { kind: "done", text: m.text, ts: m.ts });
      if (store.pendingAsk && (store.pendingAsk.sessionId || store.currentId) === sid) store.pendingAsk = null;
      return;
    }
    // 带会话归属：pendingAsk 恢复/清理时按会话路由（多会话并行不串卡）。
    if (m.type === "ask") m.sessionId = sid;
    if (sid === store.currentId) {
      store.pendingAsk = m;
    } else {
      // 后台会话：记为 tag 提示
      pushView(sv, { kind: "tag", badge: "待审批", text: m.text || "" });
    }
    return;
  }

  // 4) say 分发
  switch (m.say) {
    case "text": {
      // 定型全文：优先复用流式节点（去重）；无流式节点时新建。
      // 注意：api_req_finished 等中间帧不清 streamingIdx，保证这里能复用。
      const node = sv.streamingIdx >= 0 ? sv.msgs[sv.streamingIdx] : null;
      if (node && node.streaming) {
        node.text = m.text || "";
        node.streaming = false;
        node.ts = m.ts;
      } else {
        pushView(sv, { kind: "assistant", text: m.text || "", ts: m.ts });
      }
      sv.streamingIdx = -1;
      break;
    }
				    case "user_feedback": {
				      // 回显去重（聊天重复根因 2）：
				      //   - 本地发送（sendCurrent）已渲染用户气泡且无 ts；引擎随后回显同文本
				      //     user_feedback（历史照记）→ 向上扫描最近未配对气泡（含被 tag 卡隔开的情形）
				      //   - 审批补充指令（ask 应答 messageResponse）未本地渲染，正常展示。
				      //   - 历史回放（OpenSession）user_feedback 带 ts，正常展示。
					      const t = m.text || "";
					      let paired = false;
					      for (let i = sv.msgs.length - 1; i >= 0; i--) {
					        const g = sv.msgs[i];
					        if (g.kind === "user") {
					          // 纯图无文字发送：本地气泡 text 为空，引擎回显为「[图片附件]」，同样配对
					          if (!g.ts && (g.text === t || (g.text === "" && t === "[图片附件]"))) {
				            g.ts = m.ts;
				            if (m.images && m.images.length) g.images = m.images;
				            paired = true;
				          }
				          break; // 遇到最近一个用户气泡即停（一帧只配一个）
				        }
				        // 非 user 帧跨过（允许 tag 等隔开）
				      }
				      if (!paired) {
				        pushView(sv, { kind: "user", text: t, ts: m.ts, images: m.images || [] });
				      }
				      break;
				    }
    case "tool": {
      const info = parseToolMessage(m.text);
      pushView(sv, { kind: "tool", name: info.name, summary: info.summary, diff: info.diff, args: info.args });
      break;
    }
    case "new_task_started":
      pushView(sv, { kind: "tag", badge: "子任务", text: m.text || "" });
      break;
    case "new_task_finished":
      pushView(sv, { kind: "tag", badge: "子任务完成", text: m.text || "" });
      break;
    case "mode_switched":
      pushView(sv, { kind: "tag", badge: "模式切换", text: "已切换到 " + (m.text || "") });
      break;
	case "context_truncated":
	      pushView(sv, { kind: "tag", badge: "上下文截断", text: m.text || "" });
	      break;
	    case "context_condensed":
	      // 长会话上下文自动压缩（规划板 2026-10）：早期历史已替换为 LLM 摘要，
	      // 续聊仍可凭摘要引用早期要点；正文经 OnCondense 落会话存储。
	      pushView(sv, { kind: "tag", badge: "已压缩", text: m.text || "" });
	      break;
    case "api_req_started":
      // 新请求开始 = 新的流式周期：上一段文本/思考的节点索引失效
      //（api_req_finished 不清——定型消息紧随其后，见上方注释）。
      sv.streamingIdx = -1;
      sv.reasoningIdx = -1;
      pushView(sv, { kind: "tag", badge: "API", text: "请求中…" });
      break;
    case "api_req_finished": {
      // 回填：把最近一张「请求中」卡片替换为用量摘要（含调用时刻 HH:MM:SS + 耗时）
      for (let i = sv.msgs.length - 1; i >= 0; i--) {
        if (sv.msgs[i].kind === "tag" && sv.msgs[i].badge === "API") {
          let label = "";
          try {
            const u = m.text ? JSON.parse(m.text) : null;
            if (u && (u.prompt_tokens || u.completion_tokens)) {
              label = "tokens ↑" + fmtTok(u.prompt_tokens) + " ↓" + fmtTok(u.completion_tokens) + (u.total_tokens ? " Σ" + fmtTok(u.total_tokens) : "");
            }
          } catch { /* 忽略 */ }
          // 调用时刻（时:分:秒）：优先「开始」时间戳（api_req_started），保底结束时间；
          // 若流期间整卡未换页（同会话连续调用），以结束 ts 兜底避免误用旧开始 ts。
          const startTs = Number(sv.msgs[i].ts || 0);
          const endTs = Number(m.ts || 0);
          const startedAt = endTs && startTs && endTs >= startTs ? startTs : (endTs || startTs);
          const clock = fmtClockNs(startedAt);
          const d = fmtDurSec(Math.max(0, (Number(endTs) - Number(startedAt)) / 1e9));
          label = (label ? label + " · " : "") + clock + " 调用" + (d ? " · " + d : "");
          sv.msgs[i] = { ...sv.msgs[i], text: label || "完成", done: true };
          break;
        }
      }
      break;
    }
	    case "error":
	      pushView(sv, { kind: "error", text: m.text || "" });
	      break;
	    case "fallback":
	      // 降级续跑通知（备用模型切换 / 第三级兑底降级指引）：tag 卡展示。
	      pushView(sv, { kind: "tag", badge: "降级续跑", text: m.text || "" });
	      break;
		    case "image":
		      pushView(sv, { kind: "image", src: m.images && m.images[0] ? m.images[0] : "", path: m.text || "" });
		      break;
		    case "video":
		      pushView(sv, { kind: "video", text: m.text || "", src: m.images && m.images[0] ? m.images[0] : "" });
		      break;
		    case "audio":
		      pushView(sv, { kind: "audio", text: m.text || "", src: m.images && m.images[0] ? m.images[0] : "" });
		      break;
	    case "turn_diff":
	      pushView(sv, { kind: "diff", text: m.text || "" });
	      break;
	    case "progress":
	      // 长任务心跳（R2）：每 N 轮请求一条进度提示，替代长时间静默。
	      pushView(sv, { kind: "tag", badge: "进行中", text: m.text || "" });
	      break;
	    case "auto_decision":
	      // 全AI自动决策（用户需求 2026-10）：ask_followup 未等待用户，AI 自主决策留痕卡。
	      pushView(sv, { kind: "tag", badge: "AI自主决策", text: m.text || "" });
	      break;
    case "task_summary":
      // 会话总结卡（会话 hook 产出）：渲染摘要正文（Markdown）。
      pushView(sv, { kind: "summary", text: m.text || "", ts: m.ts });
      break;
    default:
      break;
  }
}

	// 挂起审批卡恢复（用户需求 2026-12「不显示审批就卡住」修复）：后端把
	// 阻塞等人的 ask 随列表下发（sessionsWithRunning → Summary.pendingAsk）。
	// 当前会话有挂起卡而前端丢失（切换会话/刷新/事件漏收）→ 恢复展示，
	// 治「任务在等应答但无卡可点」的假死。已应答时间戳防护：恢复仅接受比
	// 最近一次已应答更新（TS 更大）的 ask，防陈旧列表推送把已答卡片拉回来。
	// 事件推送（kaelvoris:sessions）与主动拉取（openSession 兑底）共用。
	function onSessionsList(list) {
	  for (const s of list || []) {
	    if (!s.pendingAsk || !s.pendingAsk.ts) continue;
	    if (s.pendingAsk.ts <= store.lastAnsweredAskTs) continue;
	    if (s.id === store.currentId) {
	      if (!store.pendingAsk || (store.pendingAsk.ts || 0) < s.pendingAsk.ts) {
	        store.pendingAsk = { ...s.pendingAsk, sessionId: s.id };
	      }
	    }
	  }
	}

	// ── 事件订阅（App 启动时调用一次）─────────────────────────
	export function bindEvents() {
	  onEvent("cline:message", (p) => {
	    if (p && p.sessionId) onClineMessage(p);
	  });

	  onEvent("kaelvoris:sessions", (list) => {
	    store.sessions = list || [];
	    store.sessionsLoading = false; // 首次拉取完成（推送后续正常刷新）
	    for (const s of list || []) {
	      if (s.running !== undefined) store.running[s.id] = !!s.running;
	    }
	    store.anyRunning = Object.values(store.running).some(Boolean);
	    onSessionsList(list);
	  });

  onEvent("kaelvoris:status", (s) => {
    if (!s) return;
    if (s.sessionId) {
      store.running[s.sessionId] = !!s.running;
      const sv = sess(s.sessionId);
      sv.running = !!s.running;
      if (!s.running) { sv.streamingIdx = -1; sv.reasoningIdx = -1; }
    }
    store.anyRunning = Object.values(store.running).some(Boolean);
  });

  onEvent("kaelvoris:todos", (p) => {
    if (!p || !p.sessionId) return;
    sess(p.sessionId).todos = p.items || [];
  });

  onEvent("kaelvoris:done", (p) => {
    const sid = p && p.sessionId;
    if (sid) {
      store.running[sid] = false;
      sess(sid).running = false;
      sess(sid).streamingIdx = -1;
      sess(sid).reasoningIdx = -1;
      store.anyRunning = Object.values(store.running).some(Boolean);
    }
    store.pendingAsk = null;
  });

	onEvent("kaelvoris:ai_context", (p) => {
		if (!p) return;
		// 面板未激活时丢弃快照：引擎侧已按可见性门控，此处双保险避免长 JSON
		// 字符串反复进响应式系统（面板打开时后端自动恢复推送，无 staleness）。
		if (!store.contextTabActive || store.rightbarCollapsed) return;
		store.aiContext = typeof p.messages === "string" ? p.messages : JSON.stringify(p.messages, null, 2);
	});

  onEvent("kaelvoris:usage", (p) => {
    if (!p || !p.sessionId) return;
    // 仅更新当前聚焦会话的用量展示（后台会话的推送下次聚焦时由后端补发）。
    if (p.sessionId === store.currentId) {
      store.usage = p.totals || null;
    }
  });

  onEvent("kaelvoris:toast", (msg) => {
    if (msg) store.toast = String(msg);
    // 实际 toast 展示在 App.vue 中监听
  });

  // 定时任务失败 → 失焦系统通知（共用通道 desktopNotify.notifyIfBlurred；
  // 开关 blurNotifyDisabled 缺省 false = 通知开启，对齐 CondenseDisabled 等 "xxxDisabled" 惯例）。
  onEvent("kaelvoris:cron-failed", (p) => {
    if (!p || !p.jobName) return;
    notifyIfBlurred(!(store.config && store.config.blurNotifyDisabled),
      "定时任务失败：" + p.jobName,
      (p.error || "执行失败").slice(0, 160));
  });

  onEvent("kaelvoris:queued", (p) => {
    if (!p || !p.sessionId) return;
    sess(p.sessionId).queuedCount = p.count || 0;
  });

  onEvent("kaelvoris:queued-continue", (p) => {
    if (p && p.sessionId && p.sessionId === store.currentId) {
      sess(p.sessionId).msgs.push({ kind: "tag", badge: "继续", text: "上一轮已完成，继续处理：" + (p.task || ""), _id: ++msgSeq });
    }
  });

	  onEvent("kaelvoris:project-changed", async () => {
	    await loadProjects();
	  });

	  // 自主迭代规划板推送（kaelvoris:roadmap：items + stats + auto 配置；
	  // 后端提案落库/开发状态变更后全量刷新）。
	  onEvent("kaelvoris:roadmap", (p) => {
	    if (p && Array.isArray(p.items)) store.roadmap = p;
	  });

	  // 数字员工列表推送（后端增删改后 kaelvoris:employees 全量刷新）
	  onEvent("kaelvoris:employees", (list) => {
	    if (Array.isArray(list)) store.employees = list;
	  });

	  // ── PC 端清空/总结并清空完成（用户需求 2026-11）────────────────────
	  // 后端 SummarizeClearSessions 异步完成后的收尾：复位忙碌态 + 刷新会话列表 +
	  // 当前会话被清空则回新对话态（清空按钮是同步 await，无需此事件）。
	  onEvent("kaelvoris:cleanup-done", (ev) => {
	    store.cleanupBusy = false;
	    refreshAfterCleanup();
	    if (ev && !ev.ok) Message.error(String(ev.error || "总结并清空失败"));
	  });
	  // ── 远程节点事件路由（用户需求 2026-10 之 1）───────────────────
	  // kaelvoris:remote_nodes：连接状态 + 在线节点列表（Composer 远程选择器）
	  onEvent("kaelvoris:remote_nodes", (ev) => {
	    if (!ev) return;
	    store.remoteConnected = !!ev.connected;
	    store.remoteOnline = !!ev.online;
	    if (Array.isArray(ev.nodes)) store.remoteNodes = ev.nodes;
	    // 本机节点身份（执行端过滤依据）：收到即清掉「非本机」的远程会话残留
	    //（2026-11 用户需求：PC 执行端只显示本机执行/本机发起的任务，他机任务的
	    //  广播会话（含历史残留）从列表/视图移除；移动端/管理端不受影响）。
	    if (ev.selfNodeId) {
	      store.selfNodeId = ev.selfNodeId;
	      for (const sid of Object.keys(store.remoteTasks)) {
	        const rt = store.remoteTasks[sid];
	        if (!rt) continue;
	        // own=true（本机执行/本机发起）→ 保留；仅清理非本机相关残留
	        if (rt.own) continue;
	        if (rt.nodeId && rt.nodeId !== ev.selfNodeId) {
	          store.sessionViews.delete(sid);
	          if (store.currentId === sid) store.currentId = null;
	          store.sessions = store.sessions.filter((x) => x.id !== sid);
	          delete store.remoteTasks[sid];
	        }
	      }
	    }
	  });

	  // kaelvoris:remote_task：远程任务事件流 → 复用本地会话渲染管线
	  //（started/text/tool/error/ask/result，kind 语义对齐 remote_nodes.go）
	  onEvent("kaelvoris:remote_task", (ev) => {
	    if (!ev || !ev.sessionId) return;
	    const sid = ev.sessionId;
	    if (ev.taskId || ev.nodeId) {
	      store.remoteTasks[sid] = { taskId: ev.taskId || "", nodeId: ev.nodeId || "", own: !!ev.own };
	    }
	    const sv = sess(sid);
	    sv.loaded = true;
	    sv.remote = true;
	    switch (ev.kind) {
	      case "started": {
	        sv.running = true;
	        store.running[sid] = true;
	        store.anyRunning = true;
	        if (store.currentId !== sid) {
	          // 新发布的远程会话自动聚焦（用户视角：发布即进入会话流）
	          store.currentId = sid;
if (!store.sessions.some((s) => s.id === sid)) {
	            store.sessions.unshift({ id: sid, title: (ev.text || "远程任务").slice(0, 40), updatedAt: new Date().toISOString(), remote: true });
	          }
	        }
	        // 去重：sendCurrent 远程分支已本地渲染该任务文本（无 ts），服务端
	        // started 事件回带同一文本 → 相同且紧邻时跳过（补 ts），防用户气泡双份。
	        if (ev.text) {
	          const lst = sv.msgs[sv.msgs.length - 1];
	          if (lst && lst.kind === "user" && lst.text === ev.text) {
	            if (!lst.ts) lst.ts = ev.ts;
	          } else {
	            pushView(sv, { kind: "user", text: ev.text, ts: ev.ts });
	          }
	        }
	        break;
	      }
	      case "text":
	        if (ev.partial) {
	          let node = sv.streamingIdx >= 0 ? sv.msgs[sv.streamingIdx] : null;
	          if (node && !node.streaming) node = null;
	          if (!node) {
	            node = pushView(sv, { kind: "assistant", text: ev.text || "", ts: ev.ts, streaming: true });
	            sv.streamingIdx = sv.msgs.indexOf(node);
	          } else {
	            node.text = ev.text || "";
	          }
	        } else {
	          const node = sv.streamingIdx >= 0 ? sv.msgs[sv.streamingIdx] : null;
	          if (node && node.streaming) {
	            node.text = ev.text || "";
	            node.streaming = false;
	            node.ts = ev.ts;
	          } else {
	            pushView(sv, { kind: "assistant", text: ev.text || "", ts: ev.ts });
	          }
	          sv.streamingIdx = -1;
	        }
	        break;
	      case "tool":
	        pushView(sv, { kind: "tool", ts: ev.ts, ...parseToolMessage(ev.text || "") });
	        break;
	      case "error":
	        pushView(sv, { kind: "error", text: ev.text || "", ts: ev.ts });
	        break;
	      case "ask":
	        if (sid === store.currentId) store.pendingAsk = { text: ev.text || "" };
	        else pushView(sv, { kind: "tag", badge: "远程待审批", text: ev.text || "" });
	        break;
	      case "result":
	        sv.running = false;
	        store.running[sid] = false;
	        store.anyRunning = Object.values(store.running).some(Boolean);
	        sv.streamingIdx = -1;
	        sv.reasoningIdx = -1;
	        if (ev.canceled) pushView(sv, { kind: "tag", badge: "已取消", text: "任务已取消" });
	        else if (ev.text) pushView(sv, { kind: ev.ok ? "done" : "error", text: ev.text, ts: ev.ts });
	        store.pendingAsk = null;
	        break;
	      default:
	        break;
	    }
	  });
}

// 窄窗（<1100px）左栏为浮层抽屉：会话聚焦/新建后自动退场，让出中间区
export function autoCollapseSidebar() {
  if (window.innerWidth < 1100) store.sidebarCollapsed = true;
}

// 非对话模式（Git/自主迭代/员工/技能库/流程/文档）自动收起会话列表：
// App.vue 中 Sidebar 在 chat / docs 视图渲染（v-if）——docs 侧栏底部有文档
// 快捷区（2026-12 用户需求），列表保持可见；这里兜底强制收起其余非对话
// 开关态，避免「开关显示已展开但列表不可见」的矛盾高亮；切回 chat 保持用户原状态。
watch(
  () => store.activeMain,
  (v) => {
    if (v !== "chat" && v !== "docs") store.sidebarCollapsed = true;
  },
);

// ── 草稿实时落盘（防抖 400ms）──────────────────────────────
// 需求（规划板 2026-10）：未发送草稿按会话 ID 暂存 localStorage，
// 切回会话自动恢复。inputText 变化即排程写入（debounce）；会话切换
//（openSession/newSession）另有同步 setDraft 兜底，防抖窗口内按键不丢。
let draftTimer = null;
function cancelDraftTimer() {
  if (draftTimer) { clearTimeout(draftTimer); draftTimer = null; }
}
watch(
  () => store.inputText,
  (text) => {
    const key = store.currentId || "new";
    if (draftTimer) clearTimeout(draftTimer);
    draftTimer = setTimeout(() => setDraft(key, text), 400);
  },
);

// openSession 回填令牌（竞态防护）：openSessionSeq 单调递增，每次切换取
// 最新值；晚到的旧回填结果比对失败即丢弃（见 openSession 内写回前校验）。
let openSessionSeq = 0;

// 唤起会话列表（Rail「会话列表」开关 / 顶栏展开按钮共用）：
// 当前停在新对话态（未选中任何会话）且存在历史会话时，自动选中最近一条
//（ListSessions 按 UpdatedAt 倒序，sessions[0] 即最近会话），避免唤起后
// 停在空态还须再点一次；收起动作不走此函数。
export function expandSidebar() {
  store.sidebarCollapsed = false;
  if (!store.currentId && store.sessions.length) {
    openSession(store.sessions[0].id);
  }
}

// ── 动作 ───────────────────────────────────────────────────────
export async function loadSessions() {
  if (!wailsAvailable()) return;
  store.sessionsLoading = true;
  try {
    const list = await bridgeCall("ListSessions");
    store.sessions = list || [];
    for (const s of list || []) {
      if (s.running !== undefined) store.running[s.id] = !!s.running;
    }
    store.anyRunning = Object.values(store.running).some(Boolean);
  } catch (e) { console.warn(e); }
  finally { store.sessionsLoading = false; }
}

// ── 数字员工（用户需求 2026-10 之 5）──────────────────────
export async function loadEmployees() {
  try { store.employees = (await bridgeCall("ListEmployees")) || []; }
  catch { store.employees = []; }
}

// 切换当前会话绑定的数字员工（"" = 普通会话；新会话先暂存，发送时随首条绑定）
export async function switchEmployee(empID) {
  const prev = store.currentEmployee;
  store.currentEmployee = empID || "";
  if (!store.currentId) return; // 新会话：暂存，发送时随首条消息绑定
  try {
    await bridgeCall("SetSessionEmployee", store.currentId, empID || "");
  } catch (e) {
    store.currentEmployee = prev;
    Message.error(String(e));
  }
}

// ── 远程节点（用户需求 2026-10 之 1）────────────────────
export async function loadRemoteState() {
  try {
    const st = await bridgeCall("GetRemoteStatus");
    if (st) store.remoteOnline = !!st.online;
  } catch { /* 忽略 */ }
  bridgeCall("ListRemoteNodes").then((nodes) => {
    if (Array.isArray(nodes)) store.remoteNodes = nodes;
  }).catch(() => {});
  try {
    const acc = await bridgeCall("GetRemoteAccount");
    if (acc) store.remoteOnline = store.remoteOnline || !!acc.loggedIn;
  } catch { /* 忽略 */ }
}

// 切换远程节点（""=本机；"auto"=服务端自动选在线节点；否则指定节点 ID）。
// 会话级持久化（BUG 修复）：已绑定会话时写会话自有 NodeID（SetSessionNodeID），
// 切回会话选择器联动恢复（此前仅改前端内存，切走再切回丢回「本机执行」）。
// 保留同步版供外部引用；Composer 实际入口为 switchRemoteNodeSafe（带回滚）。
export function switchRemoteNode(nodeId) {
  store.currentRemoteNode = nodeId || "";
  if (store.currentId) {
    bridgeCall("SetSessionNodeID", store.currentId, nodeId || "").catch(() => {});
  }
}

// ── ACP agent（外部编码 agent）──────────────────────────────
export async function loadACPAgents() {
  try { store.acpAgents = (await bridgeCall("ListACPAgents")) || []; }
  catch { store.acpAgents = []; }
}

// 切换当前会话的 ACP agent（"" = 内置引擎；新会话先暂存，首条消息绑定时带上）。
export async function switchACP(name) {
  const prev = store.currentACP;
  store.currentACP = name || "";
  if (!store.currentId) return; // 新会话：暂存，发送时随首条消息绑定
  try {
    await bridgeCall("SetSessionACP", store.currentId, name || "");
  } catch (e) {
    store.currentACP = prev;
    Message.error(String(e));
  }
}

// 会话置顶/取消置顶（会话卡右键菜单；规划板 2026-10 会话卡片信息升级）：
// 后端写 PinnedAt 并广播（列表排序置顶优先，远程在线时同步服务端移动端链路）；
// 这里同步更新本地列表避免等推送闪烁。
export async function pinSession(id, pinned) {
  try {
    await bridgeCall("SetSessionPinned", id, !!pinned);
    const s = store.sessions.find((x) => x.id === id);
    if (s) s.pinnedAt = pinned ? Date.now() : 0;
    // 立即重排：置顶会话恒排前（与后端 Store.List 同语义），无需等 kaelvoris:sessions 推送
    // 卡片位置即时变化，避免“点了没反应”的错觉。
    store.sessions.sort((a, b) => {
      const pa = !!(a && a.pinnedAt), pb = !!(b && b.pinnedAt);
      if (pa !== pb) return pa ? -1 : 1;
      if (pa && pb) return (b.pinnedAt || 0) - (a.pinnedAt || 0);
      return (new Date(b.updatedAt) - new Date(a.updatedAt));
    });
    Message.success(pinned ? "已置顶" : "已取消置顶");
    return true;
  } catch (e) {
    Message.error(String(e));
    return false;
  }
}

// 会话手动摘要（会话卡右键「设置摘要」；提示词 manual_preview）：非空时列表
// 卡预览行用它覆盖自动提取的“最后一条消息摘要”，空串 = 恢复自动提取。
export async function manualPreviewSession(id, preview) {
  try {
    await bridgeCall("SetSessionManualPreview", id, preview || "");
    const s = store.sessions.find((x) => x.id === id);
    if (s) s.lastMessage = (preview || "").trim();
    return true;
  } catch (e) {
    Message.error(String(e));
    return false;
  }
}

// 会话重命名（会话卡右键）：后端写入手动名（ManualTitle 置位后自动命名不再
// 覆盖）+ 广播 kaelvoris:sessions 全端同步；这里同步更新本地列表避免等推送闪烁。
export async function renameSession(id, title) {
  const t = String(title || "").trim();
  if (!id || !t) return false;
  try {
    await bridgeCall("RenameSession", id, t);
    const s = store.sessions.find((x) => x.id === id);
    if (s) s.title = t;
    return true;
  } catch (e) {
    console.warn("重命名失败", e);
    return false;
  }
}

export async function openSession(id) {
					  // 切换前同步落盘旧会话草稿（防抖窗口内的按键不丢；空内容则清槽位）
					  setDraft(store.currentId || "new", store.inputText);
					  store.currentId = id;
					  const sv = sess(id);
					  // 恢复该会话未发送草稿（规划板 2026-10：切回会话草稿自动回填）
					  store.inputText = getDraft(id);
					  // 跨会话状态立即清空（BUG 修复：切换后残留上一会话的审批卡/用量/AI 上下文）
					  store.pendingAsk = null;
					  store.usage = null;
					  store.aiContext = "";
					  // 回填令牌：每次 openSession 递增。多段 await 间隙用户可能连点切换，
					  // 晚到的上一会话回填结果（模型/模式/员工/节点）不得串写到当前会话
					  // 选择器上（竞态根因：先切 currentId 再多段 await 回填）。
					  const token = ++openSessionSeq;
					  // 立即回退到全局当前值（旧会话未记录自有值时展示与实际执行一致）。
					  // 后端 OpenSession 现随 meta 一并返回 globalMode/globalModel，
					  // 单次 IPC 即可拿到；失败再兑底逐个调用。
					  let data0 = null;
					  try {
					    data0 = await bridgeCall("OpenSession", id);
					  } catch { /* 保持 */ }
					  const snap = data0 || {};
					  // 会话自有值优先；空（旧会话未记录）跟随全局当前值（globalMode/
					  // globalModel 由后端 OpenSession 返回，与后端执行语义一致）。
					  let gMode = snap.globalMode || "";
					  let gModel = snap.globalModel || "";
					  if (!gMode) { try { gMode = (await bridgeCall("GetMode")) || ""; } catch { /* 忽略 */ } }
					  if (!gModel) { try { gModel = ((await bridgeCall("GetConfig")) || {}).model || ""; } catch { /* 忽略 */ } }
					  // 写回前校验 token：期间已切走（再次 openSession/newSession）则放弃，
					  // 防止 A 会话的回填覆盖 B 会话选择器（快速连点串值根因）。
					  if (store.currentId !== id || token !== openSessionSeq) return;
					  store.currentACP = snap.acpAgent || "";
					  store.currentEmployee = snap.employeeId || "";
					  store.model = snap.model || gModel || store.model || "";
					  store.mode = snap.mode || gMode || store.mode || "code";
					  // 执行节点：会话记录持久化的 NodeID 优先（重启后 remoteTasks 内存
					  // 映射丢失也不丢绑定）；无记录回退 remoteTasks；均无 = 本机执行。
					  const rt = store.remoteTasks[id];
					  store.currentRemoteNode = snap.nodeId || (rt && rt.nodeId) || "";
						store.sessionMeta[id] = { mode: snap.mode || "", model: snap.model || "", employeeId: snap.employeeId || "", acpAgent: snap.acpAgent || "", nodeId: snap.nodeId || "", mcpDisabled: !!snap.mcpDisabled };
		  if (!sv.loaded) {
		    try {
		      const data = data0 || (await bridgeCall("OpenSession", id));
		      const msgs = (data && data.messages) || [];
	      for (const m of msgs) onClineMessage({ sessionId: id, message: m });
	      sv.loaded = true;
	      // 会话关联工作区联动：打开会话即切到该会话绑定的项目目录（
	      // 后端 OpenSession 返回 workspace；空 = 旧会话未记录，不动全局）。
	      const ws = data && data.workspace;
	      if (ws && ws !== store.workspace) {
	        try {
	          await bridgeCall("SwitchProject", ws);
	        } catch { /* 目录可能已不存在：保持当前项目 */ }
	      }
		    } catch (e) {
		      console.warn("打开会话失败", e);
		      sv.loaded = true;
		    }
		  }
		  // 切回恢复挂起审批卡（用户需求 2026-12「不显示审批就卡住」修复）：
		  // 已加载会话（loaded=true）不回放历史，实时 ask 事件若在切走期间到达则
		  // 审批卡丢失——主动拉会话列表，后端 sessionsWithRunning 携带 pendingAsk，
		  // bindEvents 的恢复逻辑据此补卡（任务仍在等应答时必须可见可点）。
		  if (sv.loaded) {
		    try { const list = await bridgeCall("ListSessions"); if (list) onSessionsList(list); } catch { /* 忽略 */ }
		  }
		}

	// 会话独立设置工作区（右键会话卡「设置工作区…」）：后端写入会话记录并
	// 广播刷新；这里同步本地列表，避免等推送闪烁。dir 为空 = 清除绑定。
		export async function setSessionWorkspace(id, dir) {
		  await bridgeCall("SetSessionWorkspace", id, dir);
		  const s = store.sessions.find((x) => x.id === id);
		  if (s) s.workspace = dir;
		  if (dir) Message.success("工作区已设置：" + dir);
		  else Message.success("已清除会话工作区，恢复跟随全局");
		}

		// 会话级禁用/启用 MCP（右侧会话设置面板开关）：后端写入会话记录并广播刷新；
		// 这里同步本地列表避免等推送闪烁。disabled=false = 恢复跟随全局 MCP 配置。
		export async function setSessionMCPDisabled(id, disabled) {
		  await bridgeCall("SetSessionMCPDisabled", id, disabled);
		  const s = store.sessions.find((x) => x.id === id);
		  if (s) s.mcpDisabled = disabled;
		  Message.success(disabled ? "本会话已禁用 MCP（下个任务生效）" : "已恢复本会话 MCP（跟随全局配置）");
		}

			export async function newSession() {
		  // 切换前同步落盘旧会话草稿（与 openSession 同语义）
		  setDraft(store.currentId || "new", store.inputText);
		  store.currentId = null;
		  store.currentACP = "";       // 新会话：agent 选择重置（发送时按暂存值绑定）
		  store.currentEmployee = "";  // 新会话：员工选择重置（发送时按暂存值绑定）
		  store.currentRemoteNode = ""; // 新会话：执行节点重置本机（发送时按暂存值发布）
		  store.pendingAsk = null;     // 新会话：跨会话状态清零（审批卡/用量/AI 上下文）
		  const bumpedSeq = ++openSessionSeq; // 使 in-flight 旧回填失效（竞态防护）
		  void bumpedSeq;
			  // 模型/模式回全局当前值（新会话跟随全局；创建后按会话隔离）。
			  // 取后端实时值（GetMode/GetConfig）而非启动快照 store.config：
			  // 中途改过全局配置后新建会话应跟随最新全局选择。
			  try { store.model = ((await bridgeCall("GetConfig")) || {}).model || ""; } catch { /* 保持 */ }
			  try { store.mode = (await bridgeCall("GetMode")) || store.mode || "code"; } catch { /* 保持 */ }
		  store.usage = null;    // 新会话用量清零
	  // 恢复新会话草稿槽位（"new"；发送成功建会话后 clearDraft 已同步移除）
	  store.inputText = getDraft("new");
  const k = "new";
  store.sessionViews.delete(k); // 复用空态
  // 2026-10 修复（用户反馈：点新增会话看不到任何变化，须手动切会话图标）：
  // 1) 切回对话主区（若正停在 Git/规划板等视图，新建动作不可见）；
  // 2) 展开会话列表（侧栏收起时新建无反馈，展开后虚拟「新对话」高亮卡可见）。
  store.activeMain = "chat";
  store.sidebarCollapsed = false;
}

// 发送任务：本机 / 远程节点 / 数字员工三路分发（用户需求 2026-10 之 1/5）
//   - 选择远程节点（currentRemoteNode 非空）→ RemoteSendTask 发布到远端 PC 执行，
//     事件流经 kaelvoris:remote_task 复用本地渲染管线；发布前须已登录远程账号。
//   - 本机执行：员工绑定（currentEmployee）与 ACP agent（currentACP）互斥，
//     员工优先（SendMessageWithEmployee）；均空走 SendMessageWithACP（内置引擎）。
export async function sendCurrent() {
  const text = store.inputText.trim();
  const images = store.pendingImages.slice();
  if ((!text && !images.length) || store.sending) return;
  store.sending = true;
  // 发送前记下草稿归属槽位：发送成功后清除（失败保留，输入不清空可重试）
  const draftKey = store.currentId || "new";
  try {
	    // 远程分支：任务发布到远端节点执行（本机不建会话，事件流异步到达）
	    if (store.currentRemoteNode) {
	      const nodeId = store.currentRemoteNode === "auto" ? "" : store.currentRemoteNode;
	      const r = await bridgeCall("RemoteSendTask", nodeId, text, store.mode || "");
	      const sid = r && r.sessionId;
	      if (!sid) throw new Error("服务端未返回会话 ID");
		    store.currentId = sid;
		      clearDraft(draftKey); // remote
			      cancelDraftTimer(); // 取消在途防抖，防旧草稿复活已清空槽位
			      store.inputText = "";
		      store.pendingImages = [];
		      const sv = sess(sid);
	      sv.remote = true;
	      sv.running = true;
	      sv.loaded = true;
	      store.remoteTasks[sid] = { taskId: r.taskId || "", nodeId: nodeId, own: true };
	      pushView(sv, { kind: "user", text });
	      store.running[sid] = true;
	      store.anyRunning = true;
	      if (!store.sessions.some((s) => s.id === sid)) {
	        store.sessions.unshift({ id: sid, title: text.slice(0, 40) || "远程任务", updatedAt: new Date().toISOString(), remote: true });
	      }
	      autoCollapseSidebar();
	      return;
	    }
	    // 本机分支：员工绑定优先；选择器暂存值随首条消息一并传递（避免竞态）
	    let sid;
	    if (store.currentEmployee) {
	      sid = await bridgeCall("SendMessageWithEmployee", store.currentId || "", text, images, store.currentEmployee);
	    } else {
	      // ACP 绑定：选择器暂存的 agent 名随首条消息一并传递（后端建会话时
	      // 直接写入，避免先建会话再绑定与任务启动的竞态）。
	      sid = await bridgeCall("SendMessageWithACP", store.currentId || "", text, images, store.currentACP || "");
	    }
	    store.currentId = sid;
    store.inputText = "";
    store.pendingImages = [];
	    const sv = sess(sid);
	    sv.running = true;
	    sv.loaded = true;
		    // 本地渲染带图用户气泡（图片发送不显示修复）：引擎首条 user_feedback
		    // 回显被 suppressFeedback 抑制（消息去重），图片只能在这里带上；
		    // 历史回放走引擎落盘的 user_feedback（自带 Images），不受影响。
		    if (text || images.length) pushView(sv, { kind: "user", text, images });
	    store.running[sid] = true;
	    store.anyRunning = true;
	    // 「继续」重定向：提交时无当前会话（draftKey === "new"）且后端返回的
	    // sid 已存在于会话列表 = 空会话「继续」被重定向到最近既有会话。
	    // 用 openSession 对齐选择器/员工/节点（与侧栏点开同路）；openSession
	    // 内部 loaded 守卫决定是否回放，不额外改 sv.loaded。
	    if (draftKey === "new" && store.sessions.some((s) => s.id === sid)) {
	      await openSession(sid);
	    }
	    autoCollapseSidebar(); // 窄窗：首条消息创建新会话后，左栏抽屉自动退场
	    clearDraft(draftKey); // 发送成功：移除草稿（远程分支同理见上方 clearDraft）
	  cancelDraftTimer();
  } catch (e) {
    store.error = String(e);
    Message.error(String(e));
  } finally {
    store.sending = false;
  }
}

export async function stopSession() {
  if (!store.currentId) return;
  // 远程会话：走 task.cancel（服务端转发节点停止执行）
  const rt = store.remoteTasks[store.currentId];
  if (rt && rt.taskId) {
    try { await bridgeCall("RemoteCancelTask", rt.taskId); } catch (e) { Message.error(String(e)); }
    return;
  }
  try {
    await bridgeCall("Stop", store.currentId);
  } catch (e) { console.warn(e); }
}

	export async function stopAll() {
	  // 远程任务逐个取消（本地 StopAll 不覆盖远端节点）
	  for (const [sid, rt] of Object.entries(store.remoteTasks)) {
	    if (rt.taskId && store.running[sid]) {
	      bridgeCall("RemoteCancelTask", rt.taskId).catch(() => {});
	    }
	  }
	  try { await bridgeCall("StopAll"); } catch (e) { console.warn(e); }
	}

// ── PC 端会话列表「清空 / 总结并清空」（用户需求 2026-11：对齐移动端）──
// 清空：后端同步执行（秒级无 LLM），删除全部本地会话存档 + 服务端联动；
// 总结并清空：后端先受理（LLM 汇总耗时最长 5 分钟），完成经 kaelvoris:cleanup-done
// 通知（复位忙碌态；当前会话被清空则回新建会话态）。
function refreshAfterCleanup() {
  loadSessions();
  // 当前会话已被清空：回到新对话态（侧栏自动高亮「新对话」卡）
  if (store.currentId && !store.sessions.some((s) => s.id === store.currentId)) {
    newSession();
  }
}

export async function clearAllSessions() {
  try {
    const n = await bridgeCall("ClearAllSessions");
    if (n > 0) Message.success("已清空 " + n + " 个会话");
    else Message.info("暂无可清空的会话");
    refreshAfterCleanup();
  } catch (e) { Message.error(String(e)); }
}

export function summarizeClearSessions() {
  // 后端受理即返回（异步完成），完成经 kaelvoris:cleanup-done / kaelvoris:toast 通知
  bridgeCall("SummarizeClearSessions").catch((e) => Message.error(String(e)));
}

	export async function answerAsk(response, text) {
  if (!store.pendingAsk) return;
  // 应答时间戳：后端 sessions 推送据此判旧（陈旧恢复防护，见 bindEvents）。
  if (store.pendingAsk.ts) store.lastAnsweredAskTs = store.pendingAsk.ts;
  // 远程会话审批：ask.reply 路由到归属节点（response 映射对齐 remote_nodes.go）
  if (store.currentId && store.remoteTasks[store.currentId]) {
    const map = { yes: "yesButtonClicked", no: "noButtonClicked" };
    try {
      await bridgeCall("RemoteApproveTaskAsk", store.currentId, map[response] || "messageResponse", text || "");
      store.pendingAsk = null;
    } catch (e) { Message.error(String(e)); }
    return;
  }
  try {
    await bridgeCall("ApproveAsk", store.currentId || "", response, text || "");
    store.pendingAsk = null;
  } catch (e) { console.warn(e); }
}

export async function loadProjects() {
  if (!wailsAvailable()) return;
  try {
    store.projects = (await bridgeCall("ListProjects")) || [];
    const cur = store.projects.find((p) => p.current);
    store.workspace = cur ? cur.path : "";
  } catch (e) { console.warn(e); }
}

export async function loadModes() {
  try {
    store.modes = (await bridgeCall("ListModes")) || [];
  } catch { store.modes = []; }
  try { store.mode = (await bridgeCall("GetMode")) || "code"; } catch { /* 保持 */ }
}

export async function loadModels() {
  try { store.models = (await bridgeCall("ListModels")) || []; } catch { store.models = []; }
}

export async function switchMode(name) {
  store.mode = name;
  try { await bridgeCall("SetMode", name); } catch (e) { console.warn(e); }
}

export async function loadGit() {
  try {
    store.git = await bridgeCall("GitStatus");
  } catch { store.git = null; }
}

export async function loadConfig() {
  try {
    store.config = await bridgeCall("GetConfig");
  } catch { store.config = null; }
}
