<script>
// 模块级跨实例状态：未读数/最后消息缓存/已见消息计数在组件卸载后仍保留（「关闭后重开仍有未读」）。
// 未读三表为普通对象（非响应式），经组件内响应式 ref 包装后驱动 UI。
const unreadByGroup = {};   // {gid: number}
const lastMsgCache = {};    // {gid: {text, ts, authorName}}
const lastCount = {};       // {gid: number} 已见权威消息数（计算未读增量）
</script>

<script setup>
// ═════════════════════════════════════════════════════════════════
// A2A 群聊独立视图（PC 端，2026-10 用户需求：群聊从 640px 弹窗升级为独立聊天视图）
//   - 左：群列表（名称/最近消息预览/未读 Badge/成员数/时间）
//   - 右：聊天区（时间分组·头像昵称·类型标签·发送中状态·自动滚底）+ 成员管理 + 派任务多选
//   - 后端绑定：GroupList/GroupCreate/GroupMsg/GroupTask/GroupJoin/GroupLeave/
//     GroupMessages/ListEmployees；事件：kaelvoris:groups/kaelvoris:group_msg/
//     kaelvoris:group_task_done（员工产出本地推送由并行后端任务负责，此处仅消费事件）。
// ═════════════════════════════════════════════════════════════════
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { Message, Modal } from "@arco-design/web-vue";
import {
  IconArrowLeft, IconUserGroup, IconPlus, IconRefresh, IconDelete,
  IconThunderbolt, IconSend,
} from "@arco-design/web-vue/es/icon";
import { bridgeCall, onEvent } from "../bridge/wails.js";

const props = defineProps({
  // 初始打开的群 ID（从数字员工面板「群聊」入口进入时携带）
  initialGroupId: { type: String, default: "" },
});
const emit = defineEmits(["back", "create"]);

// ── 状态 ──────────────────────────────────────────────────────
const groups = ref([]);          // Group[]
const curGroup = ref(null);      // 当前群
const groupMsgs = ref([]);       // 当前群消息（权威历史 + 本地 pending）
const taskCards = ref([]);       // 任务卡（乐观 running + 权威完成态）
const employees = ref([]);       // 数字员工（添加成员/状态展示数据源）
const input = ref("");           // 输入框
const taskInput = ref("");       // 派任务指令
const taskSel = ref([]);         // 派任务选中的员工 ID
const taskMode = ref("chain");   // 协作模式：chain 接力 / parallel 并行 / review 评审
const taskModal = ref(false);    // 派任务弹窗
const memberModal = ref(false);  // 成员管理弹窗
const addSel = ref([]);          // 添加成员勾选
const taskBusy = ref(false);
const joinBusy = ref(false);
const loadingMsgs = ref(false);
const msgListEl = ref(null);

// 模块级未读三表 → 响应式视图
const unread = ref({ ...unreadByGroup });
const lastMsg = ref({ ...lastMsgCache });
const EMPTY_TIME = 0;

// 协作模式选项（后端 GroupTask mode 参数：chain=接力 / parallel=并行 / review=评审）
const TASK_MODES = [
  { value: "chain", label: "接力协作", short: "接力", color: "arcoblue",
    desc: "员工按序接力，后位在前位产出基础上继续（适合流水线：写初稿→润色→核查）" },
  { value: "parallel", label: "并行分工", short: "并行", color: "orangered",
    desc: "所选员工各自独立完成，完成后群助手汇总（适合多方案对比/多角度分析）" },
  { value: "review", label: "评审打磨", short: "评审", color: "purple",
    desc: "第 1 位员工产出、第 2 位员工评审并给出修订版（适合质量把关）" },
];
function taskModeMeta(mode) {
  return TASK_MODES.find((m) => m.value === mode) || null;
}

// ── 派生 ──────────────────────────────────────────────────────
const curMembers = computed(() => (curGroup.value && curGroup.value.members) || []);
const curEmpIds = computed(() => curMembers.value.filter((m) => m.kind === "employee").map((m) => m.id));
const empById = computed(() => {
  const m = {};
  for (const e of employees.value) m[e.id] = e;
  return m;
});

// 可添加成员：非归档 + 不在群内；不可运行项灰显禁用
const addCandidates = computed(() =>
  employees.value
    .filter((e) => e.status !== "archived" && !curEmpIds.value.includes(e.id))
    .map((e) => {
      const runnable = e.status === "enabled" && e.runnable !== false;
      return { ...e, runnable };
    })
);

// 群任务默认指派 = 群内全部可用员工（用户可增减；清空 = 后端默认第一个）
const taskDefaultSel = computed(() =>
  curEmpIds.value.filter((id) => {
    const e = empById.value[id];
    return e ? e.status === "enabled" && e.runnable !== false : false;
  })
);

// 群列表排序：最近消息时间（无消息用 updatedAt/createdAt）倒序
const sortedGroups = computed(() =>
  [...groups.value].sort((a, b) => {
    const ta = (lastMsg.value[a.id] && lastMsg.value[a.id].ts) || a.updatedAt || a.createdAt || EMPTY_TIME;
    const tb = (lastMsg.value[b.id] && lastMsg.value[b.id].ts) || b.updatedAt || b.createdAt || EMPTY_TIME;
    return tb - ta;
  })
);

// 消息 + 任务卡合并渲染序列（按时间序，供时间分组）
const feed = computed(() => {
  const items = [];
  for (const m of groupMsgs.value) items.push({ kind: "msg", ts: m.ts || 0, msg: m });
  for (const t of taskCards.value) items.push({ kind: "task", ts: t.ts || 0, task: t });
  return items.sort((a, b) => a.ts - b.ts);
});

function memberLabel(m) {
  if (m.kind === "bot") return "群助手";
  if (m.kind === "user") return "用户";
  return "员工";
}
function memberTagCls(m) {
  return m.kind === "bot" ? "bot" : m.kind === "user" ? "user" : "emp";
}
function isOwner(m) {
  return curGroup.value && m.id && m.id === curGroup.value.owner;
}
// 员工在群内可见状态（enabled/disabled + 人事状态）
function empState(m) {
  const e = empById.value[m.id];
  if (!e) return "";
  const st = e.status === "enabled" ? "启用" : e.status === "disabled" ? "停用" : e.status === "draft" ? "草稿" : (e.status || "");
  const es = e.empStatus ? (e.empStatus === "active" ? "在职" : e.empStatus === "probation" ? "试用期" : e.empStatus === "suspended" ? "人事停用" : e.empStatus === "resigned" ? "离职" : e.empStatus) : "";
  return st + (es && es !== st ? " · " + es : "");
}
function empStateColor(m) {
  const e = empById.value[m.id];
  if (!e) return "mute";
  if (e.status === "enabled") return "ok";
  if (e.status === "archived") return "mute";
  return "warn";
}

// ── 时间分组（今天/昨天/日期）─────────────────────────────────
function dayLabel(ts) {
  if (!ts) return "";
  const d = new Date(Number(ts));
  const now = new Date();
  const startOf = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const diff = Math.floor((startOf(now) - startOf(d)) / 86400000);
  if (diff === 0) return "今天";
  if (diff === 1) return "昨天";
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}
function groupTs(ts) {
  if (!ts) return "";
  const d = new Date(Number(ts));
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}`;
}
function fmtTs(ts) { return groupTs(ts); }

// ── 群列表 / 消息加载 ─────────────────────────────────────────
async function loadGroups() {
  try {
    groups.value = (await bridgeCall("GroupList")) || [];
  } catch (e) {
    groups.value = [];
  }
}

async function loadMessages(gid, opts = {}) {
  loadingMsgs.value = true;
  try {
    const msgs = (await bridgeCall("GroupMessages", gid)) || [];
    mergeAuthorityMsgs(gid, msgs, !!opts.pending);
    onSeen(gid, msgs);
  } catch (e) {
    /* 保持现有消息 */
  } finally {
    loadingMsgs.value = false;
  }
}

// 权威消息合并：覆盖历史 + 保留本地 pending（发送中）帧
function mergeAuthorityMsgs(gid, msgs, keepPending) {
  if (gid !== (curGroup.value && curGroup.value.id)) {
    // 非当前群：只更新 lastMsg 缓存与计数
    updateLastMsg(gid, msgs);
    return;
  }
  const pending = keepPending ? groupMsgs.value.filter((m) => m.id && String(m.id).indexOf("local-") === 0) : [];
  groupMsgs.value = [...msgs, ...pending];
  updateLastMsg(gid, msgs);
  scrollToBottomSoon();
}

function updateLastMsg(gid, msgs) {
  const last = msgs[msgs.length - 1];
  if (!last) return;
  lastMsgCache[gid] = { text: last.text || "", ts: last.ts || 0, authorName: last.authorName || last.author || "" };
  lastMsg.value = { ...lastMsgCache };
}

// 未读增量：记录已见消息数，事件覆盖时 diff
function onSeen(gid, msgs) {
  lastCount[gid] = msgs.length;
}

// ── 打开群 ────────────────────────────────────────────────────
async function openGroup(g) {
  if (!g || !g.id) return;
  curGroup.value = g;
  groupMsgs.value = [];
  taskCards.value = [];
  input.value = "";
  clearUnread(g.id);
  await loadMessages(g.id, { pending: false });
}

function clearUnread(gid) {
  if (unreadByGroup[gid]) delete unreadByGroup[gid];
  unread.value = { ...unreadByGroup };
}

function selectGroupById(gid) {
  const g = groups.value.find((x) => x.id === gid);
  if (g) openGroup(g);
  else if (gid) {
    // 初始群未在列表（列表未加载完）→ 等列表到达后再开
    const stop = watch(
      () => groups.value,
      (list) => {
        const gg = list.find((x) => x.id === gid);
        if (gg) {
          stop();
          openGroup(gg);
        }
      },
      { deep: false }
    );
  }
}

// ── 发送消息（Enter 发送 / Shift+Enter 换行）────────────────────
function onKeydown(e) {
  if (e.key === "Enter" && !e.shiftKey) {
    e.preventDefault();
    sendMsg();
  }
}

async function sendMsg() {
  const text = input.value.trim();
  const gid = curGroup.value && curGroup.value.id;
  if (!text || !gid || !curGroup.value) return;
  // 乐观上屏（pending 帧）
  const local = {
    id: "local-" + Date.now() + "-" + Math.random().toString(36).slice(2, 6),
    groupId: gid, author: "user:remote", authorName: "用户", authorKind: "user",
    text, ts: Date.now(), pending: true,
  };
  groupMsgs.value.push(local);
  input.value = "";
  scrollToBottomSoon();
  try {
    await bridgeCall("GroupMsg", gid, text);
    // 等待权威覆盖（事件异步推送；此处主动拉取保证一致）
    await loadMessages(gid, { pending: true });
  } catch (e) {
    const i = groupMsgs.value.findIndex((m) => m.id === local.id);
    if (i >= 0) groupMsgs.value.splice(i, 1);
    Message.error(String(e));
  }
}

// 派任务弹窗底部说明（随协作模式联动）
const taskModeHint = computed(() => {
  if (taskMode.value === "parallel") return "并行分工：所选员工各自独立完成任务，完成后由群助手汇总回群。";
  if (taskMode.value === "review") return "评审打磨：第 1 位员工产出，第 2 位员工评审并给出修订版（仅取前 2 位）。";
  return "接力协作：任务按所选顺序依次派发，后位员工在前位产出基础上继续，产出汇总回群。";
});

// ── 派任务（多员工接力：assignedTo 逗号分隔员工 ID；空 = 后端默认第一个）─
function openTaskModal() {
  taskInput.value = input.value;
  taskSel.value = [...taskDefaultSel.value];
  taskMode.value = "chain";
  input.value = "";
  taskModal.value = true;
}
function toggleTaskSel(id) {
  const i = taskSel.value.indexOf(id);
  if (i >= 0) taskSel.value.splice(i, 1);
  else taskSel.value.push(id);
}
async function dispatchTask() {
  const text = taskInput.value.trim();
  const gid = curGroup.value && curGroup.value.id;
  if (!text || !gid || taskBusy.value) return;
  // 乐观任务卡（running）；task_done 到达后按 instruction 匹配替换为权威卡
  const localTask = {
    id: "local-task-" + Date.now(),
    groupId: gid, instruction: text, assignedTo: taskSel.value.join(","),
    assigned: [...taskSel.value], mode: taskMode.value,
    status: "running", ts: Date.now(),
  };
  taskCards.value.push(localTask);
  taskBusy.value = true;
  try {
    await bridgeCall("GroupTask", gid, text, taskSel.value.join(","), taskMode.value);
    taskModal.value = false;
    Message.success("任务已派发，员工协作中…");
  } catch (e) {
    const i = taskCards.value.findIndex((t) => t.id === localTask.id);
    if (i >= 0) taskCards.value.splice(i, 1);
    Message.error(String(e));
  } finally {
    taskBusy.value = false;
    scrollToBottomSoon();
  }
}
// task_done → 排除乐观卡，追加权威卡（按 id 去重）
function applyTaskDone(t) {
  if (!t || !t.groupId) return;
  taskCards.value = taskCards.value
    .filter((c) => !(String(c.id).indexOf("local-task-") === 0 && c.instruction === t.instruction))
    .filter((c) => c.id !== t.id);
  taskCards.value.push({
    id: t.id || "task-" + Date.now(),
    groupId: t.groupId, instruction: t.instruction || "", assignedTo: t.assignedTo || "",
    assigned: String(t.assignedTo || "").split(",").map((s) => s.trim()).filter(Boolean),
    mode: t.mode || "",
    status: t.status || "done", result: t.result || "",
    results: Array.isArray(t.results) ? t.results : null,
    ts: t.ts || Date.now(),
  });
  scrollToBottomSoon();
}

// ── 成员管理 ──────────────────────────────────────────────────
function openMembers() {
  addSel.value = [];
  memberModal.value = true;
}
async function addMembers() {
  const gid = curGroup.value && curGroup.value.id;
  const ids = addSel.value;
  if (!gid || !ids.length || joinBusy.value) return;
  joinBusy.value = true;
  try {
    for (const id of ids) {
      await bridgeCall("GroupJoin", gid, id);
    }
    Message.success(`已添加 ${ids.length} 位成员`);
    addSel.value = [];
    await refreshGroup(gid);
  } catch (e) {
    Message.error(String(e));
  } finally {
    joinBusy.value = false;
  }
}
function removeMember(m) {
  const gid = curGroup.value && curGroup.value.id;
  if (!gid || !m || m.kind !== "employee") return;
  Modal.confirm({
    title: "移出成员",
    content: `确定将「${m.name || m.id}」移出群聊？`,
    okText: "移出",
    cancelText: "取消",
    onOk: async () => {
      try {
        await bridgeCall("GroupLeave", gid, m.id);
        Message.success("已移出");
        await refreshGroup(gid);
      } catch (err) {
        Message.error(String(err));
      }
    },
  });
}
// 群变更后刷新：列表 + 当前群快照 + 消息（群助手会写入加入/移出消息）
async function refreshGroup(gid) {
  await loadGroups();
  const g = groups.value.find((x) => x.id === gid);
  if (g && curGroup.value && curGroup.value.id === gid) {
    curGroup.value = g;
  }
  if (curGroup.value && curGroup.value.id === gid) await loadMessages(gid, { pending: true });
}
function toggleAddSel(id) {
  const i = addSel.value.indexOf(id);
  if (i >= 0) addSel.value.splice(i, 1);
  else addSel.value.push(id);
}

// ── 事件订阅 ──────────────────────────────────────────────────
const offGroups = onEvent("kaelvoris:groups", (list) => {
  if (Array.isArray(list)) {
    groups.value = list;
    // 当前群快照同步（成员被其他地方修改时）
    if (curGroup.value) {
      const g = list.find((x) => x.id === curGroup.value.id);
      if (g) curGroup.value = g;
    }
  }
});
const offGroupMsg = onEvent("kaelvoris:group_msg", (ev) => {
  if (!ev || !ev.groupId || !Array.isArray(ev.messages)) return;
  const gid = ev.groupId;
  const msgs = ev.messages;
  if (curGroup.value && curGroup.value.id === gid) {
    mergeAuthorityMsgs(gid, msgs, true);
    onSeen(gid, msgs);
  } else {
    // 非当前群：未读 = 本次新增条数（与已见计数 diff）
    const prev = lastCount[gid] || 0;
    if (msgs.length > prev) {
      unreadByGroup[gid] = (unreadByGroup[gid] || 0) + (msgs.length - prev);
      unread.value = { ...unreadByGroup };
    }
    updateLastMsg(gid, msgs);
    lastCount[gid] = msgs.length;
  }
});
const offTaskDone = onEvent("kaelvoris:group_task_done", (t) => {
  applyTaskDone(t);
  // 任务完成消息会经 group_msg 推送；此处补拉当前群消息保持一致
  if (t && t.groupId && curGroup.value && curGroup.value.id === t.groupId) {
    loadMessages(t.groupId, { pending: true });
  }
});
onMounted(async () => {
  await loadGroups();
  await loadEmployees();
  if (props.initialGroupId) selectGroupById(props.initialGroupId);
  else if (groups.value.length) openGroup(sortedGroups.value[0]);
});

async function loadEmployees() {
  try {
    employees.value = (await bridgeCall("ListEmployees")) || [];
  } catch (e) {
    employees.value = [];
  }
}

onBeforeUnmount(() => {
  if (typeof offGroups === "function") offGroups();
  if (typeof offGroupMsg === "function") offGroupMsg();
  if (typeof offTaskDone === "function") offTaskDone();
});

// ── 自动滚底（用户上滚超过一屏则暂停跟随）─────────────────────
let stickBottom = true;
let scrollTimer = 0;
function scrollToBottomSoon() {
  clearTimeout(scrollTimer);
  scrollTimer = setTimeout(() => {
    nextTick(() => {
      const el = msgListEl.value;
      if (el && stickBottom) el.scrollTop = el.scrollHeight;
    });
  }, 30);
}
function onScroll() {
  const el = msgListEl.value;
  if (!el) return;
  stickBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
}
</script>

<template>
  <div class="gchat">
    <!-- ═══ 左侧：群列表 ═══ -->
    <aside class="gchat-list">
      <div class="gchat-list-head panel-head">
        <span class="panel-icon"><IconUserGroup /></span>
        <span class="gchat-list-title">群聊</span>
        <span class="panel-sub">A2A 协作 · {{ groups.length }} 个群</span>
        <span class="flex-sp"></span>
        <a-tooltip content="刷新">
          <button class="icon-btn" title="刷新群列表" @click="loadGroups"><IconRefresh /></button>
        </a-tooltip>
        <button class="gbtn gbtn-primary" @click="$emit('create')">
          <IconPlus />建群
        </button>
      </div>
      <div class="gchat-list-body">
        <div v-if="!groups.length" class="gchat-empty empty-block">
          <div class="empty-ico-chip"><IconUserGroup /></div>
          <div class="empty-block-title">暂无群聊</div>
          <div class="empty-block-hint">点击右上角「建群」，选数字员工即可发起协作</div>
        </div>
        <div
          v-for="g in sortedGroups"
          :key="g.id"
          class="gchat-item"
          :class="{ on: curGroup && curGroup.id === g.id }"
          @click="openGroup(g)"
        >
          <span class="gchat-item-avatar">{{ (g.name || "群").slice(0, 1) }}</span>
          <div class="gchat-item-main">
            <div class="gchat-item-row1">
              <span class="gchat-item-name" :title="g.name">{{ g.name || "未命名群" }}</span>
              <span v-if="unread[g.id]" class="gchat-unread">{{ unread[g.id] > 99 ? "99+" : unread[g.id] }}</span>
            </div>
            <div class="gchat-item-preview">
              {{ (lastMsg[g.id] && lastMsg[g.id].text) || "暂无消息" }}
            </div>
            <div class="gchat-item-meta">
              {{ (g.members || []).filter((m) => m.kind === "employee").length }} 员工 ·
              {{ lastMsg[g.id] ? fmtTs(lastMsg[g.id].ts) : fmtTs(g.updatedAt || g.createdAt) }}
            </div>
          </div>
        </div>
      </div>
    </aside>

    <!-- ═══ 右侧：聊天区 ═══ -->
    <section class="gchat-main">
      <template v-if="curGroup">
        <div class="gchat-head panel-head">
          <button class="icon-btn" title="返回数字员工" @click="$emit('back')">
            <IconArrowLeft />
          </button>
          <div class="gchat-head-title" title="点击管理成员" @click="openMembers">
            <span class="gchat-title">{{ curGroup.name }}</span>
            <span class="gchat-sub">
              {{ curMembers.length }} 位成员 · {{ curEmpIds.length }} 员工
              <template v-if="curGroup.nodeId"> · @{{ String(curGroup.nodeId).slice(0, 10) }}</template>
            </span>
          </div>
          <span class="flex-sp"></span>
          <button class="gbtn" title="管理成员" @click="openMembers">
            <IconUserGroup />{{ curMembers.length }}
          </button>
          <button class="gbtn gbtn-primary" :disabled="taskBusy" @click="openTaskModal">
            <IconThunderbolt />{{ taskBusy ? "派发中…" : "派任务" }}
          </button>
        </div>

        <!-- 消息流 -->
        <div ref="msgListEl" class="gchat-msgs" @scroll="onScroll">
          <template v-if="feed.length">
            <template v-for="(it, idx) in feed" :key="it.kind === 'msg' ? it.msg.id || idx : it.task.id || idx">
              <div v-if="idx === 0 || dayLabel(feed[idx - 1].ts) !== dayLabel(it.ts)" class="g-day-sep">
                {{ dayLabel(it.ts) }} {{ groupTs(it.ts) }}
              </div>

              <!-- 任务卡 -->
              <div v-if="it.kind === 'task'" class="gtask-card" :class="it.task.status">
                <div class="gtask-head">
                  <IconThunderbolt class="gtask-icon" />
                  <span class="gtask-label">群任务</span>
                  <span v-if="taskModeMeta(it.task.mode)" class="gtask-mode">{{ taskModeMeta(it.task.mode).short }}</span>
                  <span class="gtask-status" :class="it.task.status === 'done' ? 'ok' : it.task.status === 'failed' ? 'err' : 'run'">
                    {{ it.task.status === "done" ? "已完成" : it.task.status === "failed" ? "失败" : "执行中…" }}
                  </span>
                </div>
                <div class="gtask-instr">{{ it.task.instruction }}</div>
                <div class="gtask-assigned">
                  <span class="gtask-assigned-label">指派：</span>
                  <span v-if="it.task.assigned && it.task.assigned.length" class="gtask-chip-row">
                    <span v-for="id in it.task.assigned" :key="id" class="gtask-chip">
                      {{ (empById[id] && empById[id].name) || id }}
                    </span>
                  </span>
                  <span v-else class="gtask-chip">默认（群助手派发）</span>
                </div>
                <div v-if="it.task.status !== 'running'" class="gtask-result">
                  <!-- results 逐员工展示（后端有此字段时） -->
                  <template v-if="Array.isArray(it.task.results) && it.task.results.length">
                    <div v-for="(r, ri) in it.task.results" :key="ri" class="gtask-result-item">
                      <span class="gtask-result-emp">{{ r.employee || r.agentId || r.name || `员工 ${ri + 1}` }}</span>
                      <span class="gtask-result-text">{{ r.result || r.text || String(r) }}</span>
                    </div>
                  </template>
                  <template v-else>
                    <div class="gtask-result-text">{{ it.task.result || "（无结果文本）" }}</div>
                  </template>
                </div>
              </div>

              <!-- 消息气泡 -->
              <div v-else class="gmsg" :class="it.msg.authorKind === 'user' ? 'gmsg-user' : ''">
                <template v-if="it.msg.authorKind !== 'user'">
                  <span class="gmsg-avatar" :class="memberTagCls(it.msg)">{{ (it.msg.authorName || "?")[0] }}</span>
                  <div class="gmsg-body">
                    <div class="gmsg-name">
                      {{ it.msg.authorName || it.msg.author }}
                      <span class="gmsg-tag" :class="memberTagCls(it.msg)">{{ memberLabel(it.msg) }}</span>
                      <span class="gmsg-time">{{ groupTs(it.msg.ts) }}</span>
                    </div>
                    <div class="gmsg-bubble" :class="memberTagCls(it.msg)">{{ it.msg.text }}</div>
                  </div>
                </template>
                <template v-else>
                  <div class="gmsg-body user">
                    <div class="gmsg-name">
                      <span v-if="it.msg.pending" class="gmsg-sending">发送中…</span>
                    </div>
                    <div class="gmsg-bubble user">{{ it.msg.text }}</div>
                    <span class="gmsg-time">{{ groupTs(it.msg.ts) }}</span>
                  </div>
                </template>
              </div>
            </template>
          </template>
          <div v-else class="gchat-empty" style="margin-top: 0">
            <div>暂无消息</div>
            <div class="gchat-empty-hint">输入内容向群里发言，或点「派任务」让员工协作</div>
          </div>
          <div style="height: 12px"></div>
        </div>

        <!-- 输入区 -->
        <div class="gchat-inputbar">
          <textarea
            v-model="input"
            class="gchat-textarea"
            rows="2"
            placeholder="发送消息（群成员实时可见），Enter 发送 / Shift+Enter 换行"
            @keydown="onKeydown"
          ></textarea>
          <button class="gbtn gbtn-primary gbtn-send" :disabled="!input.trim()" @click="sendMsg">
            <IconSend />发送
          </button>
        </div>
      </template>

      <!-- 无选中群（空态） -->
      <div v-else class="gchat-main-empty">
        <IconUserGroup class="gchat-empty-icon" />
        <div>选择左侧群聊开始协作</div>
        <div class="gchat-empty-hint">群聊 = 你 + 数字员工 + 群助手；派任务后员工产出实时回传群内</div>
      </div>
    </section>

    <!-- ═══ 成员管理弹窗 ═══ -->
    <a-modal
      v-model:visible="memberModal"
      :title="curGroup ? curGroup.name + ' · 成员' : '成员'"
      :width="520"
      unmount-on-close
      :footer="false"
    >
      <div class="gmember">
        <div class="gmember-list">
          <div v-for="m in curMembers" :key="m.id" class="gmember-row">
            <span class="gmsg-avatar" :class="memberTagCls(m)">{{ (m.name || "?")[0] }}</span>
            <div class="gmember-info">
              <div class="gmember-name">
                {{ m.name || m.id }}
                <span v-if="m.kind === 'bot'" class="gtag bot">群助手</span>
                <span v-else-if="isOwner(m)" class="gtag owner">群主</span>
                <span v-else class="gtag emp">{{ memberLabel(m) }}</span>
              </div>
              <div v-if="m.description" class="gmember-desc">{{ m.description }}</div>
              <div v-if="m.kind === 'employee'" class="gmember-state">
                <span class="gstate" :class="empStateColor(m)">{{ empState(m) || "未知状态" }}</span>
              </div>
            </div>
            <span class="flex-sp"></span>
            <a-button
              v-if="m.kind === 'employee'"
              size="mini"
              type="text"
              status="danger"
              @click="removeMember(m)"
            >
              <template #icon><IconDelete /></template>移除
            </a-button>
          </div>
        </div>

        <div class="gmember-add">
          <div class="gmember-add-title">+ 添加成员（数字员工）</div>
          <div v-if="!addCandidates.length" class="gmember-add-empty">没有可添加的员工（非归档且不在群内）</div>
          <div
            v-for="e in addCandidates"
            :key="e.id"
            class="gsheet-member"
            :class="{ off: !e.runnable }"
            @click="e.runnable && toggleAddSel(e.id)"
          >
            <span class="gcheck" :class="{ on: addSel.indexOf(e.id) >= 0 }"></span>
            <span class="gsheet-avatar">{{ e.avatar || "🤖" }}</span>
            <span class="gsheet-name">{{ e.name }}</span>
            <span class="gsheet-dept">{{ e.description || e.rolePrompt || "" }}</span>
            <span v-if="!e.runnable" class="gtag warn">不可运行</span>
          </div>
          <div class="gmember-add-actions">
            <span class="set-hint">已选 {{ addSel.length }} 位</span>
            <a-button size="small" :disabled="!addSel.length || joinBusy" :loading="joinBusy" type="primary" @click="addMembers">
              添加选中
            </a-button>
          </div>
        </div>
      </div>
    </a-modal>

    <!-- ═══ 派任务弹窗（多员工接力）═══ -->
    <a-modal
      v-model:visible="taskModal"
      title="⚡ 派任务"
      :width="560"
      unmount-on-close
      :footer="false"
    >
      <div class="set-section">
        <label class="set-label">任务指令 *</label>
        <a-textarea v-model="taskInput" :auto-size="{ minRows: 3, maxRows: 8 }" placeholder="描述要员工完成的任务，如：整理今天的群聊纪要并输出 markdown" />
      </div>
      <div class="set-section">
        <label class="set-label">协作模式</label>
        <div class="gmode-row">
          <div
            v-for="m in TASK_MODES"
            :key="m.value"
            class="gmode-card"
            :class="{ on: taskMode === m.value }"
            @click="taskMode = m.value"
          >
            <div class="gmode-name">{{ m.label }}</div>
            <div class="gmode-desc">{{ m.desc }}</div>
          </div>
        </div>
        <div v-if="taskMode === 'review'" class="set-hint" style="margin-top: 6px">
          评审模式取前 2 位员工：第 1 位产出、第 2 位评审{{ taskSel.length > 2 ? '（已选 ' + taskSel.length + ' 位，将截取前 2 位）' : '' }}。
        </div>
      </div>
      <div class="set-section">
        <label class="set-label">指派员工（默认群内全部可用员工，清空 = 群助手派第一个）</label>
        <div v-if="!taskDefaultSel.length && !taskSel.length" class="set-hint">群内暂无可用员工，任务将失败——请先添加员工</div>
        <div
          v-for="e in employees.filter((x) => curEmpIds.includes(x.id))"
          :key="e.id"
          class="gsheet-member"
          @click="toggleTaskSel(e.id)"
        >
          <span class="gcheck" :class="{ on: taskSel.indexOf(e.id) >= 0 }"></span>
          <span class="gsheet-avatar">{{ e.avatar || "🤖" }}</span>
          <span class="gsheet-name">{{ e.name }}</span>
          <span class="gsheet-dept">{{ e.description || "" }}</span>
            <span v-if="e.status !== 'enabled'" class="gtag warn">停用</span>
        </div>
      </div>
      <div class="group-task-hint" style="margin-top: 4px">
        {{ taskModeHint }}
      </div>
      <div style="display: flex; justify-content: flex-end; gap: 8px; margin-top: 10px">
        <a-button size="small" @click="taskModal = false">取消</a-button>
        <a-button size="small" type="primary" :loading="taskBusy" :disabled="!taskInput.trim()" @click="dispatchTask">
          <template #icon><IconThunderbolt /></template>派发
        </a-button>
      </div>
    </a-modal>
  </div>
</template>

<style scoped>
/* ═══ 群聊独立视图：沿用 tokens（暖黑/暖纸 + 陶土强调），无渐变无玻璃卡 ═══ */
.gchat {
  flex: 1;
  min-height: 0;
  display: flex;
  gap: 12px;
  overflow: hidden;
}

/* ── 左：群列表 ── */
.gchat-list {
  width: clamp(180px, 28%, 264px);
  flex-shrink: 0;
  min-width: 0;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  display: flex;
  flex-direction: column;
  min-height: 0;
}
.gchat-list-head {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 10px;
  border-bottom: 1px solid var(--border-weak);
}
.gchat-list-title {
  font-size: var(--fs-title);
  color: var(--text-1);
  font-weight: 500;
}

/* 面板级按钮（与 EmployeePanel emp-btn 同语言） */
.gbtn {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  height: 28px;
  padding: 0 11px;
  border-radius: var(--radius-sm);
  border: 1px solid var(--border);
  background: var(--bg-elevated);
  color: var(--text-2);
  font-size: var(--fs-caption);
  font-family: var(--font-ui);
  cursor: pointer;
  transition: var(--transition);
  white-space: nowrap;
}
.gbtn svg { font-size: 13px; }
.gbtn:hover { border-color: var(--border); color: var(--text-1); background: var(--bg-hover); }
.gbtn:disabled { opacity: 0.45; cursor: not-allowed; }
.gbtn-primary {
  background: var(--primary);
  border-color: var(--primary);
  color: var(--on-primary);
}
.gbtn-primary:hover {
  background: var(--primary-strong);
  border-color: var(--primary-strong);
  color: var(--on-primary);
}
.gbtn-primary:disabled { background: var(--primary); opacity: 0.45; }
.gchat-list-body {
  flex: 1;
  overflow-y: auto;
  min-height: 0;
  padding: 6px;
}
.gchat-item {
  display: flex;
  gap: 8px;
  padding: 8px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: var(--transition);
}
.gchat-item:hover { background: var(--bg-hover); }
/* 选中态：与部门导航一致的滑动亮条 */
.gchat-item.on {
  background: var(--bg-active);
  box-shadow: inset 2px 0 0 var(--primary);
}
.gchat-item-avatar {
  width: 36px;
  height: 36px;
  border-radius: 10px;
  background: var(--bg-elevated);
  border: 1px solid var(--border-weak);
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 15px;
  font-weight: 600;
  color: var(--primary);
  flex-shrink: 0;
}
.gchat-item-main { flex: 1; min-width: 0; }
.gchat-item-row1 { display: flex; align-items: center; gap: 6px; }
.gchat-item-name {
  color: var(--text-1);
  font-size: var(--fs-body);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  flex: 1;
}
.gchat-unread {
  flex-shrink: 0;
  min-width: 16px;
  height: 16px;
  padding: 0 5px;
  border-radius: 8px;
  background: var(--danger);
  color: var(--on-primary);
  font-size: 10px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}
.gchat-item-preview {
  color: var(--text-3);
  /* 预览行最大两行内省略由单行省略保持，行高收紧 */
  font-size: var(--fs-caption);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  margin-top: 2px;
}
.gchat-item-meta {
  color: var(--text-3);
  font-size: var(--fs-micro);
  margin-top: 2px;
}
.gchat-empty {
  margin-top: 32px;
}

/* ── 右：聊天区 ── */
.gchat-main {
  flex: 1;
  min-width: 0;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  display: flex;
  flex-direction: column;
  min-height: 0;
  overflow: hidden;
}
.gchat-main-empty {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  color: var(--text-2);
}
.gchat-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 12px;
  border-bottom: 1px solid var(--border-weak);
}
.gchat-head-title {
  display: flex;
  flex-direction: column;
  min-width: 0;
  cursor: pointer;
}
.gchat-title {
  color: var(--text-1);
  font-size: var(--fs-body);
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.gchat-sub {
  color: var(--text-3);
  font-size: var(--fs-micro);
  margin-top: 1px;
}

/* 消息流 */
.gchat-msgs {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 10px 14px 0;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
/* 时间分隔：无边框纯文字胶囊 */
.g-day-sep {
  align-self: center;
  font-size: var(--fs-micro);
  color: var(--text-3);
  background: var(--bg-inset);
  padding: 2px 12px;
  border-radius: 10px;
  margin: 4px 0;
  letter-spacing: 0.02em;
}

/* 消息气泡：去边框，靠底色分层；头像圆形更符合 IM 语境 */
.gmsg { display: flex; gap: 8px; max-width: 86%; }
.gmsg-user { align-self: flex-end; flex-direction: row-reverse; }
.gmsg-avatar {
  width: 30px;
  height: 30px;
  border-radius: 50%;
  margin-top: 18px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 13px;
  flex-shrink: 0;
  background: var(--bg-elevated);
  border: 1px solid var(--border-weak);
  color: var(--text-1);
}
.gmsg-avatar.emp { background: var(--primary-tint); border-color: var(--primary-tint); color: var(--primary); }
.gmsg-avatar.bot { background: var(--success-tint); border-color: var(--success-tint); color: var(--success); }
.gmsg-avatar.user { background: var(--bg-elevated); }
.gmsg-body { min-width: 0; display: flex; flex-direction: column; gap: 3px; }
.gmsg-body.user { align-items: flex-end; }
.gmsg-name { font-size: var(--fs-micro); color: var(--text-3); display: flex; align-items: center; gap: 6px; }
.gmsg-tag {
  padding: 0 5px;
  font-size: 10px;
  color: var(--primary);
  background: var(--primary-tint);
  border-radius: 3px;
}
.gmsg-tag.bot { color: var(--success); background: var(--success-tint); }
.gmsg-tag.user { color: var(--text-2); background: var(--bg-inset); }
.gmsg-time { color: var(--text-3); font-size: 10px; }
.gmsg-sending { color: var(--warning); font-size: 10px; }
.gmsg-bubble {
  background: var(--bg-inset);
  border-radius: 14px 14px 14px 4px;
  padding: 8px 12px;
  font-size: var(--fs-body);
  color: var(--text-1);
  white-space: pre-wrap;
  word-break: break-word;
  line-height: var(--lh);
}
/* 用户气泡：陶土实底 + 深色文字，收尾角在右下 */
.gmsg-bubble.user {
  background: var(--primary);
  color: var(--on-primary);
  border-radius: 14px 14px 4px 14px;
}
.gmsg-bubble.bot { background: var(--bg-inset); }
.gmsg-bubble.emp { background: var(--bg-inset); }

/* 任务卡：执行中左侧亮条标识，完成/失败回落弱边框 */
.gtask-card {
  align-self: stretch;
  border: 1px solid var(--border-weak);
  background: var(--bg-inset);
  border-radius: var(--radius);
  padding: 10px 12px;
  display: flex;
  flex-direction: column;
  gap: 6px;
  transition: var(--transition);
}
.gtask-card.running { box-shadow: inset 3px 0 0 var(--primary); border-color: var(--primary-tint); }
.gtask-card.failed { box-shadow: inset 3px 0 0 var(--danger); }
.gtask-head { display: flex; align-items: center; gap: 8px; }
.gtask-icon { color: var(--warning); font-size: 13px; }
/* 模式/状态：轻量文字徽标（替代 a-tag） */
.gtask-mode {
  font-size: var(--fs-micro);
  color: var(--primary);
  background: var(--primary-tint);
  padding: 1px 8px;
  border-radius: 8px;
}
.gtask-status {
  font-size: var(--fs-micro);
  display: inline-flex;
  align-items: center;
  gap: 5px;
  color: var(--text-3);
}
.gtask-status::before { content: ""; width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.gtask-status.ok { color: var(--success); }
.gtask-status.err { color: var(--danger); }
.gtask-status.run { color: var(--primary); }
.gtask-status.run::before { animation: gtask-pulse 1.2s ease-in-out infinite; }
@keyframes gtask-pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.3; } }
.gtask-label { font-size: var(--fs-caption); color: var(--text-1); font-weight: 600; }
.gtask-instr { font-size: var(--fs-body); color: var(--text-1); white-space: pre-wrap; word-break: break-word; }
.gtask-assigned { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.gtask-assigned-label { font-size: var(--fs-micro); color: var(--text-3); }
.gtask-chip-row { display: flex; gap: 4px; flex-wrap: wrap; }
.gtask-chip {
  font-size: var(--fs-micro);
  color: var(--text-2);
  background: var(--bg-elevated);
  border: 1px solid var(--border-weak);
  border-radius: 8px;
  padding: 1px 8px;
}
.gtask-result {
  border-top: 1px solid var(--border-weak);
  padding-top: 6px;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.gtask-result-item { display: flex; gap: 8px; font-size: var(--fs-caption); }
.gtask-result-emp { color: var(--primary); flex-shrink: 0; }
.gtask-result-text { color: var(--text-2); white-space: pre-wrap; word-break: break-word; }

/* 输入区 */
.gchat-inputbar {
  display: flex;
  gap: 8px;
  align-items: flex-end;
  padding: 10px 12px;
  border-top: 1px solid var(--border-weak);
}
.gchat-textarea {
  flex: 1;
  resize: none;
  background: var(--bg-inset);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  color: var(--text-1);
  font-size: var(--fs-body);
  font-family: var(--font-ui);
  line-height: var(--lh);
  padding: 8px 10px;
  outline: none;
  transition: var(--transition);
}
.gchat-textarea:focus {
  border-color: var(--primary);
  box-shadow: 0 0 0 3px var(--primary-tint);
}

/* 成员弹窗 */
.gmember { display: flex; flex-direction: column; gap: 12px; max-height: 60vh; overflow-y: auto; }
.gmember-list { display: flex; flex-direction: column; gap: 4px; }
.gmember-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 4px;
  border-bottom: 1px solid var(--border-weak);
}
.gmember-info { min-width: 0; }
.gmember-name { display: flex; align-items: center; gap: 4px; color: var(--text-1); font-size: var(--fs-body); }
.gmember-desc { font-size: var(--fs-micro); color: var(--text-3); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.gmember-state { margin-top: 2px; }
/* 成员轻量标签 / 状态点（替代 a-tag） */
.gtag {
  font-size: 10px;
  padding: 1px 6px;
  border-radius: 7px;
  line-height: 1.5;
  flex-shrink: 0;
}
.gtag.bot { color: var(--success); background: var(--success-tint); }
.gtag.emp { color: var(--primary); background: var(--primary-tint); }
.gtag.owner { color: var(--text-2); background: var(--bg-inset); }
.gtag.warn { color: var(--warning); background: color-mix(in srgb, var(--warning) 14%, transparent); }
.gstate {
  font-size: var(--fs-micro);
  color: var(--text-3);
  display: inline-flex;
  align-items: center;
  gap: 5px;
}
.gstate::before { content: ""; width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.gstate.ok { color: var(--success); }
.gstate.warn { color: var(--warning); }
.gstate.mute { color: var(--text-3); }
.gmember-add {
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  padding: 10px;
  background: var(--bg-inset);
}
.gmember-add-title { font-size: var(--fs-caption); color: var(--primary); font-weight: 500; margin-bottom: 8px; }
.gmember-add-empty { font-size: var(--fs-caption); color: var(--text-3); padding: 8px 0; }
.gmember-add-actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 8px;
}
.gsheet-member {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 7px 4px;
  border-bottom: 1px solid var(--border-weak);
  cursor: pointer;
  transition: var(--transition);
}
.gsheet-member:last-child { border-bottom: none; }
.gsheet-member:hover { background: var(--bg-hover); }
.gsheet-member.off { opacity: 0.5; cursor: not-allowed; }
.gcheck {
  width: 18px;
  height: 18px;
  border-radius: 4px;
  border: 1.5px solid var(--border);
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 12px;
  color: var(--on-primary);
}
.gcheck.on { background: var(--primary-strong); border-color: var(--primary-strong); }
.gcheck.on::after { content: '✓'; font-weight: 700; }
.gsheet-avatar { font-size: 16px; }
.gsheet-name { flex: 1; color: var(--text-1); font-size: var(--fs-body); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.gsheet-dept { font-size: var(--fs-micro); color: var(--text-3); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 160px; }
.flex-sp { flex: 1; }
.set-section { margin-bottom: 14px; }
.set-label { display: block; font-size: var(--fs-caption); color: var(--text-2); margin-bottom: 6px; }
.set-hint { font-size: var(--fs-micro); color: var(--text-3); }
.group-task-hint { font-size: var(--fs-micro); color: var(--text-3); line-height: 1.6; margin-top: 6px; }
/* 协作模式选择卡（派任务弹窗）：选中态同「滑动亮条」语言 */
.gmode-row { display: flex; gap: 8px; }
.gmode-card {
  flex: 1;
  min-width: 0;
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  background: var(--bg-inset);
  padding: 8px 10px;
  cursor: pointer;
  transition: var(--transition);
}
.gmode-card:hover { border-color: var(--primary); }
.gmode-card.on {
  border-color: var(--primary);
  background: var(--primary-tint);
  box-shadow: inset 2px 0 0 var(--primary);
}
.gmode-name { font-size: var(--fs-caption); color: var(--text-1); font-weight: 600; margin-bottom: 2px; }
.gmode-card.on .gmode-name { color: var(--primary); }
.gmode-desc { font-size: var(--fs-micro); color: var(--text-3); line-height: 1.6; }
</style>
