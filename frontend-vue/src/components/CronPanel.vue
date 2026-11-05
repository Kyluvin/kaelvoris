<script setup>
// ═════════════════════════════════════════════════════════════════
// 定时任务管控面板（用户需求 2026-12：PC 端左侧入口）
//   - 头部：标题 + 运行统计 + 刷新 + 新建定时任务
//   - 列表：任务卡（名称/状态/表达式中文描述/下次触发/上次运行/工作区/模式/
//           任务摘要/连败熔断）＋ 操作（立即运行/启停/编辑/删除）
//   - 执行记录：最近运行（倒序，状态徽标/时长/会话回放/重试/错误）
//   - 快捷创建：代码自检·经验沉淀（CreateSelfReviewJob）、每小时会话复盘
//     （CreateHourlyReviewJob）
//   - 编辑/新建弹窗：名称/表达式（实时中文描述预览）/任务提示词/工作区/
//     模式/超时/重试/启用
// 后端：cron_panel.go（ListCronJobs/AddCronJob/UpdateCronJob/DeleteCronJob/
//   ToggleCronJob/RunCronJobNow/ListCronRuns/DescribeCron/CreateSelfReviewJob/
//   CreateHourlyReviewJob）；事件 kaelvoris:cron 推送列表刷新；
//   运行态经 store.running["cron_<jobId>"] 推导（定时会话 ID 前缀 cron_）。
// ═════════════════════════════════════════════════════════════════
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { Message, Modal } from "@arco-design/web-vue";
import {
  IconClockCircle, IconPlus, IconRefresh, IconPlayArrow, IconDelete,
  IconEdit, IconHistory, IconPoweroff, IconFile, IconCode,
} from "@arco-design/web-vue/es/icon";
import { bridgeCall, onEvent } from "../bridge/wails.js";
import { store, openSession } from "../stores/app.js";
import { fmtTime } from "../utils/format.js";
import BindEditor from "./BindEditor.vue";

const jobs = ref([]);
const runs = ref([]);
const runningIds = ref(new Set());
const loading = ref(false);
const showRuns = ref(true); // 执行记录区展开态

// ── 编辑/新建弹窗 ─────────────────────────────────────────────────
const editVisible = ref(false);
const saving = ref(false);
const form = ref(null); // {id,name,expr,workspace,mode,model,task,enabled,timeoutMin,retryLimit,timeWindowEnabled,startTime,endTime}
const descPreview = ref(""); // 表达式实时中文描述

function emptyForm() {
  return {
    id: "", name: "", expr: "0 9 * * *", workspace: "", mode: "", model: "",
    task: "", enabled: true, timeoutMin: 0, retryLimit: 0,
    timeWindowEnabled: false, startTime: "18:00", endTime: "06:00",
  };
}

function openNew() {
  form.value = emptyForm();
  descPreview.value = "";
  editVisible.value = true;
}

function openEdit(j) {
  form.value = {
    id: j.id, name: j.name || "", expr: j.expr || "", workspace: j.workspace || "",
    mode: j.mode || "", model: j.model || "", task: j.task || "", enabled: !!j.enabled,
    timeoutMin: j.timeoutMin || 0, retryLimit: j.retryLimit || 0,
    timeWindowEnabled: !!j.timeWindowEnabled, startTime: j.startTime || "18:00", endTime: j.endTime || "06:00",
    employeeId: j.employeeId || "", positionId: j.positionId || "", assignRule: j.assignRule || "",
  };
  descPreview.value = j.desc || "";
  editVisible.value = true;
}

// 编辑弹窗内嵌绑定编辑器保存（doc/09-设计方案 §8-3）：写回任务绑定三字段，
// 后端经 UpdateCronJob → cron.ValidateBinding 校验；保存后刷新任务列表。
async function onBindSaved() {
  await load();
}

// 表达式实时预览：非法表达式显示错误（不阻断保存，后端也会校验）。
async function onExprChange() {
  const expr = (form.value.expr || "").trim();
  if (!expr) { descPreview.value = ""; return; }
  try {
    descPreview.value = (await bridgeCall("DescribeCron", expr)) || "";
  } catch (e) {
    descPreview.value = "表达式无效：" + String(e);
  }
}

// 保存（@before-ok 语义：返回 false / 抛错时弹窗保持打开）
async function save() {
  const f = form.value || {};
  if (!String(f.name || "").trim()) { Message.warning("请填写任务名称"); return false; }
  if (!String(f.expr || "").trim()) { Message.warning("请填写 cron 表达式"); return false; }
  if (!String(f.task || "").trim()) { Message.warning("请填写任务提示词"); return false; }
  saving.value = true;
  try {
    if (f.id) {
      await bridgeCall("UpdateCronJob", {
        id: f.id, name: f.name.trim(), expr: f.expr.trim(),
        workspace: f.workspace.trim(), mode: f.mode.trim(), model: (f.model || "").trim(), task: f.task,
        enabled: !!f.enabled, timeoutMin: Number(f.timeoutMin) || 0,
        retryLimit: Number(f.retryLimit) || 0,
        timeWindowEnabled: !!f.timeWindowEnabled,
        startTime: f.timeWindowEnabled ? (f.startTime || "18:00") : "",
        endTime: f.timeWindowEnabled ? (f.endTime || "06:00") : "",
      });
      Message.success("定时任务已更新");
    } else {
      // 新建：AddCronJob 仅基础字段 → 创建后统一经 UpdateCronJob 补写全部扩展字段
      //（模型/超时/重试）；全为默认值时也补写一次，保证 model:"" 能显式清空。
      const created = await bridgeCall("AddCronJob", f.name.trim(), f.expr.trim(), f.workspace.trim(), f.mode.trim(), f.task, !!f.enabled);
      if (created && created.id) {
        await bridgeCall("UpdateCronJob", {
          id: created.id, name: f.name.trim(), expr: f.expr.trim(),
          workspace: f.workspace.trim(), mode: f.mode.trim(), model: (f.model || "").trim(), task: f.task,
          enabled: !!f.enabled, timeoutMin: Number(f.timeoutMin) || 0,
          retryLimit: Number(f.retryLimit) || 0,
          timeWindowEnabled: !!f.timeWindowEnabled,
          startTime: f.timeWindowEnabled ? (f.startTime || "18:00") : "",
          endTime: f.timeWindowEnabled ? (f.endTime || "06:00") : "",
        });
      }
      Message.success("定时任务已创建");
    }
    editVisible.value = false;
    await load();
    return true;
  } catch (e) {
    Message.error("保存失败：" + String(e));
    return false;
  } finally {
    saving.value = false;
  }
}

// ── 加载 ─────────────────────────────────────────────────────────
async function load() {
  loading.value = true;
  try {
    const [j, r] = await Promise.all([
      bridgeCall("ListCronJobs").catch(() => []),
      bridgeCall("ListCronRuns").catch(() => []),
    ]);
    jobs.value = j || [];
    runs.value = r || [];
    refreshRunning();
  } catch (e) {
    console.warn("cron load failed:", e);
  } finally {
    loading.value = false;
  }
}

// 运行态：调度器占用中 = 任务会话运行中（cron_<jobId> 会话）
function refreshRunning() {
  const ids = new Set(jobs.value.map((j) => j.id));
  const set = new Set();
  for (const [sid, running] of Object.entries(store.running)) {
    if (running && sid.startsWith("cron_")) {
      const id = sid.slice(5);
      if (ids.has(id)) set.add(id);
    }
  }
  runningIds.value = set;
}

// ── 操作 ─────────────────────────────────────────────────────────
async function runJob(j) {
  try {
    await bridgeCall("RunCronJobNow", j.id);
    Message.success(`已触发「${j.name}」立即执行`);
    setTimeout(load, 1200);
  } catch (e) {
    Message.error(String(e));
  }
}

async function toggleJob(j) {
  try {
    await bridgeCall("ToggleCronJob", j.id, !j.enabled);
    Message.success(j.enabled ? "已停用" : "已启用");
    await load();
  } catch (e) {
    Message.error(String(e));
  }
}

function removeJob(j) {
  Modal.confirm({
    title: "删除定时任务",
    content: `确定删除「${j.name}」？删除后不可恢复，其执行记录同时清理。`,
    okText: "删除",
    okButtonProps: { status: "danger" },
    onOk: async () => {
      try {
        await bridgeCall("DeleteCronJob", j.id);
        Message.success("已删除");
        await load();
      } catch (e) {
        Message.error(String(e));
      }
    },
  });
}

// 立即运行且已有会话运行中 → 按钮禁用（忙态防重入）
function isRunning(j) {
  return runningIds.value.has(j.id);
}

// 执行记录 → 打开对应会话回放（cron_<jobId> 会话）
async function openRunSession(run) {
  if (!run.sessionId) return;
  try {
    await openSession(run.sessionId);
    store.activeMain = "chat";
  } catch (e) {
    Message.error(String(e));
  }
}

// ── 快捷创建（一键任务）────────────────────────────────────────────
async function quickSelfReview() {
  try {
    await bridgeCall("CreateSelfReviewJob", "", "");
    Message.success("已创建「代码自检·经验沉淀」（默认每天 21:00，可编辑调整）");
    await load();
  } catch (e) {
    Message.error(String(e));
  }
}
async function quickHourlyReview() {
  try {
    await bridgeCall("CreateHourlyReviewJob", "0 * * * *");
    Message.success("已创建「每小时会话复盘」（每小时整点）");
    await load();
  } catch (e) {
    Message.error(String(e));
  }
}

// ── 派生展示 ─────────────────────────────────────────────────────
// ── 执行者徽标字典（员工/岗位名；懒加载，失败静默 = 显示 ID）─────
const empDict = ref({});
const posDict = ref({});
async function loadBindDicts() {
  try {
    const emps = (await bridgeCall("ListEmployees").catch(() => [])) || [];
    const m = {};
    for (const e of emps) m[e.id] = (e.avatar || "🤖") + " " + e.name;
    empDict.value = m;
  } catch { /* 静默 */ }
  try {
    const view = await bridgeCall("GetOrg");
    const m = {};
    for (const p of ((view && view.positions) || [])) m[p.id] = p.name;
    posDict.value = m;
  } catch { /* 静默 */ }
}
function empName(id) { return empDict.value[id] || ""; }
function positionName(id) { return posDict.value[id] || ""; }

const runningCount = computed(() => runningIds.value.size);
const enabledCount = computed(() => jobs.value.filter((j) => j.enabled).length);

// 状态徽标（对齐 cron 工具 cronEnabledText + 熔断提示）
function statusOf(j) {
  if (isRunning(j)) return { label: "运行中", tone: "run" };
  if (j.disabledReason) return { label: "已熔断", tone: "tripped" };
  return j.enabled ? { label: "已启用", tone: "on" } : { label: "已停用", tone: "off" };
}

function fmtTs(v) {
  return v ? fmtTime(v) : "";
}

// 下次触发相对时间（codex 调度列表套路：主显「x 分钟后」相对量，title 留绝对时间）
function nextIn(v) {
  if (!v) return "—";
  const d = v instanceof Date ? v : new Date(v);
  if (isNaN(d.getTime())) return "—";
  const ms = d.getTime() - Date.now();
  if (ms <= 0) return "即将触发";
  const min = Math.round(ms / 60000);
  if (min < 1) return "1 分钟内";
  if (min < 60) return min + " 分钟后";
  const h = Math.floor(min / 60);
  if (h < 24) return h + " 小时后";
  return Math.floor(h / 24) + " 天后";
}

function shortWs(ws) {
  if (!ws) return "";
  const parts = String(ws).replace(/[\\/]+$/, "").split(/[\\/]/).filter(Boolean);
  return parts.length ? parts[parts.length - 1] : ws;
}

function shortModel(m) {
  const s = String(m || "");
  return s.length > 28 ? s.slice(0, 28) + "…" : s;
}

// 时间窗口展示辅助：判断是否跨零点（start > end，如 18:00～06:00）
function isCrossMidnight(s, e) {
  const p = (v) => String(v || "").split(":").map(Number);
  const [sh, sm] = p(s || "18:00");
  const [eh, em] = p(e || "06:00");
  if ([sh, sm, eh, em].some((x) => Number.isNaN(x))) return false;
  return sh * 60 + sm > eh * 60 + em;
}

function snippet(s, n = 72) {
  const str = String(s || "").replace(/\s+/g, " ").trim();
  const arr = Array.from(str);
  return arr.length > n ? arr.slice(0, n).join("") + "…" : str;
}

// 执行记录状态徽标文案/样式
const runStatusMeta = {
  running:  { label: "运行中", tone: "run" },
  done:     { label: "成功",   tone: "ok" },
  failed:   { label: "失败",   tone: "err" },
  skipped:  { label: "跳过",   tone: "skip" },
  timeout:  { label: "超时",   tone: "skip" },
  circuit_open: { label: "熔断", tone: "err" },
  retry:    { label: "重试",   tone: "skip" },
};
function runTone(s) {
  return (runStatusMeta[s] || {}).tone || "muted";
}
function runLabel(s) {
  return (runStatusMeta[s] || {}).label || s;
}

// 执行记录时长（毫秒 → 可读）
function fmtDurMs(ms) {
  const n = Number(ms || 0);
  if (!n) return "";
  if (n < 60000) return Math.round(n / 1000) + "s";
  return Math.floor(n / 60000) + "m" + Math.round((n % 60000) / 1000) + "s";
}

// ── 事件 ─────────────────────────────────────────────────────────
const offCron = onEvent("kaelvoris:cron", (list) => {
  if (Array.isArray(list)) {
    jobs.value = list;
    refreshRunning();
  }
});
// 会话运行态变化→任务运行徽标联动（会话结束推送 kaelvoris:status）
const offStatus = onEvent("kaelvoris:status", () => refreshRunning());
const offDone = onEvent("kaelvoris:done", () => refreshRunning());

let refreshTimer = null;
onMounted(() => {
  load();
  loadBindDicts();
  // 定时刷新下次触发时间展示（分钟级）；句柄随卸载清理（修复：面板随
  // activeMain 反复挂载/卸载时定时器累积泄漏，同一分钟内多次 load）。
  refreshTimer = setInterval(load, 60000);
});

onBeforeUnmount(() => {
  if (refreshTimer) { clearInterval(refreshTimer); refreshTimer = null; }
  if (typeof offCron === "function") offCron();
  if (typeof offStatus === "function") offStatus();
  if (typeof offDone === "function") offDone();
});
</script>

<template>
  <div class="cron-panel">
  <!-- 头部 -->
	    <div class="cron-head panel-head">
	      <span class="panel-icon"><IconClockCircle /></span>
	      <span class="cron-title">定时任务</span>
	      <span class="panel-sub">cron 自动触发 Agent · 快捷模板一键创建</span>
	      <span v-if="runningCount" class="cron-run-badge pulse-soft"> {{ runningCount }} 个运行中</span>
      <span class="flex-sp"></span>
      <a-tooltip content="刷新">
        <button class="icon-btn" @click="load"><IconRefresh /></button>
      </a-tooltip>
      <a-button size="small" type="primary" @click="openNew">
        <template #icon><IconPlus /></template>
        新建任务
      </a-button>
    </div>

    <!-- 概览统计 -->
	    <div class="panel-stats">
	      <div class="stat-item">
	        <div class="stat-num">{{ jobs.length }}</div>
	        <div class="stat-label">任务总数</div>
	      </div>
	      <div class="stat-div"></div>
	      <div class="stat-item stat-accent">
	        <div class="stat-num">{{ enabledCount }}</div>
	        <div class="stat-label">已启用</div>
	      </div>
	      <div class="stat-div"></div>
	      <div class="stat-item" :class="{ 'stat-on': runningCount > 0 }">
	        <div class="stat-num">{{ runningCount }}</div>
	        <div class="stat-label">运行中</div>
	      </div>
	      <div class="stat-div"></div>
	      <div class="stat-item" :class="{ 'stat-warn': runs.some((r) => r.status === 'failed' || r.status === 'timeout') }">
	        <div class="stat-num">{{ runs.filter((r) => r.status === "failed" || r.status === "timeout").length }}</div>
	        <div class="stat-label">近期异常</div>
	      </div>
	    </div>

	    <!-- 快捷创建 -->
	    <div class="cron-quick">
      <button class="cron-quick-btn" @click="quickSelfReview">
        <IconCode class="cron-quick-ico" />
        <span class="cron-quick-name">代码自检·经验沉淀</span>
        <span class="cron-quick-hint">默认每天 21:00 · 复盘并沉淀经验卡</span>
      </button>
      <button class="cron-quick-btn" @click="quickHourlyReview">
        <IconClockCircle class="cron-quick-ico" />
        <span class="cron-quick-name">每小时会话复盘</span>
        <span class="cron-quick-hint">每小时整点 · 系统级会话复盘</span>
      </button>
    </div>

    <!-- spin 包裹层本身作为滚动约束层（对齐 ResourcePanel 套路）：flex 子项默认
         min-height:auto，任务多时被撑开、被外层 overflow:hidden 裁掉且内部
         overflow-y:auto 不生效；挂 flex:1+min-height:0 后列表区才真正可滚 -->
    <a-spin :loading="loading" style="display: flex; flex-direction: column; width: 100%; flex: 1; min-height: 0; overflow: hidden">
      <!-- 任务列表 -->
      <div v-if="jobs.length" class="cron-list">
	          <div v-for="(j, i) in jobs" :key="j.id" class="cron-card panel-card card-lift card-in" :class="{ disabled: !j.enabled && !j.disabledReason, running: isRunning(j) }" :style="{ '--i': i }">
          <div class="cron-row1">
            <span class="cron-dot" :class="'cron-dot-' + statusOf(j).tone" :title="statusOf(j).label"></span>
            <span class="cron-name" :title="j.name">{{ j.name }}</span>
            <span class="cron-badge" :class="'cron-badge-' + statusOf(j).tone">{{ statusOf(j).label }}</span>
            <span v-if="j.enabled" class="cron-next" :title="'下次触发：' + (j.nextRun ? fmtTs(j.nextRun) : '—')">
	              下次 {{ j.nextRun ? nextIn(j.nextRun) : "—" }}
	            </span>
            <span class="flex-sp"></span>
            <a-tooltip :content="isRunning(j) ? '运行中，不可重复触发' : '立即执行一次'">
              <a-button size="mini" type="text" :disabled="isRunning(j)" @click="runJob(j)">
                <template #icon><IconPlayArrow /></template>
              </a-button>
            </a-tooltip>
            <span class="cron-ops">
              <a-tooltip :content="j.enabled ? '停用' : '启用'">
                <a-button size="mini" type="text" @click="toggleJob(j)">
                  <template #icon><IconPoweroff /></template>
                </a-button>
              </a-tooltip>
              <a-tooltip content="编辑">
                <a-button size="mini" type="text" @click="openEdit(j)">
                  <template #icon><IconEdit /></template>
                </a-button>
              </a-tooltip>
              <a-tooltip content="删除">
                <a-button size="mini" type="text" status="danger" @click="removeJob(j)">
                  <template #icon><IconDelete /></template>
                </a-button>
              </a-tooltip>
            </span>
          </div>
          <div class="cron-row2">
            <span class="cron-expr mono" title="cron 表达式">{{ j.expr }}</span>
            <span class="cron-desc" :title="j.desc">{{ j.desc }}</span>
          </div>
          <div v-if="j.disabledReason" class="cron-tripped">已熔断：{{ j.disabledReason }}</div>
          <div class="cron-row3">
            <span v-if="j.employeeId || j.positionId" class="cron-meta cron-bind" :title="(j.employeeId ? '员工 ' + j.employeeId : '') + (j.positionId ? (j.employeeId ? ' · ' : '') + '岗位 ' + (positionName(j.positionId) || j.positionId) : '')">
              执行 {{ j.employeeId ? (empName(j.employeeId) || j.employeeId) : "岗位池" }}{{ j.positionId ? " · " + (positionName(j.positionId) || j.positionId) : "" }}{{ j.assignRule === "round_robin" ? " · 轮转" : (j.assignRule === "least_loaded" ? " · 最闲" : "") }}
            </span>
            <span v-if="j.task" class="cron-task" :title="j.task">{{ snippet(j.task) }}</span>
	            <span v-if="j.workspace" class="cron-meta" :title="j.workspace">工作区 {{ shortWs(j.workspace) }}</span>
	            <span v-if="j.mode" class="cron-meta">模式 {{ j.mode }}</span>
	            <span v-if="j.model" class="cron-meta" :title="j.model">模型 {{ shortModel(j.model) }}</span>
	            <span v-if="j.timeWindowEnabled" class="cron-meta cron-win-badge" :title="'仅在 ' + (j.startTime || '18:00') + '～' + (j.endTime || '06:00') + ' 时段内自动触发，手动执行不受限'">时段 {{ j.startTime || "18:00" }}～{{ j.endTime || "06:00" }}</span>
	            <span v-if="j.timeoutMin || j.retryLimit" class="cron-meta">超时{{ j.timeoutMin || 30 }}m · 重试{{ j.retryLimit || 0 }}</span>
            <span v-if="j.lastRun" class="cron-meta">上次 {{ fmtTs(j.lastRun) }}</span>
            <span v-if="j.failStreak" class="cron-meta fail">连败 {{ j.failStreak }}</span>
            <span class="flex-sp"></span>
            <span v-if="j.lastRun && runningIds.size" class="cron-last-run"></span>
          </div>
        </div>
      </div>
      <div v-else class="cron-empty empty-block">
	        <div class="empty-ico-chip"><IconClockCircle /></div>
	        <div class="empty-block-title">暂无定时任务</div>
	        <div class="empty-block-hint">定时任务按 cron 表达式自动触发 Agent 执行；可点击下方快捷卡片或「新建任务」创建。</div>
	        <a-button size="small" type="primary" @click="openNew">新建第一个任务</a-button>
	      </div>
    </a-spin>

    <!-- 执行记录 -->
    <div v-if="runs.length" class="cron-runs">
      <div class="cron-runs-head" role="button" tabindex="0" @click="showRuns = !showRuns" @keydown.enter.prevent="showRuns = !showRuns" @keydown.space.prevent="showRuns = !showRuns">
        <IconHistory class="cron-runs-ico" />
        <span class="cron-runs-title">执行记录（最近 {{ runs.length }} 条）</span>
        <span class="flex-sp"></span>
        <span class="cron-runs-toggle">{{ showRuns ? "收起" : "展开" }}</span>
      </div>
      <div v-if="showRuns" class="cron-runs-body">
        <div v-for="(r, i) in runs.slice(0, 12)" :key="i" class="cron-run">
          <span class="cron-run-badge" :class="'cron-badge-' + runTone(r.status)">{{ runLabel(r.status) }}</span>
          <span class="cron-run-name" :title="r.jobName">{{ r.jobName }}</span>
          <span v-if="r.attempt && r.attempt > 1" class="cron-run-attempt">×{{ r.attempt }}</span>
          <span v-if="r.error" class="cron-run-err" :title="r.error">{{ snippet(r.error, 48) }}</span>
          <span v-if="r.durationMs" class="cron-run-dur">{{ fmtDurMs(r.durationMs) }}</span>
          <span class="flex-sp"></span>
          <span class="cron-run-time">{{ fmtTs(r.startedAt) }}</span>
          <a-button v-if="r.sessionId" size="mini" type="text" @click="openRunSession(r)">
            <template #icon><IconFile /></template>
          </a-button>
        </div>
      </div>
    </div>

    <!-- 编辑/新建弹窗 -->
    <a-modal
      v-model:visible="editVisible"
      :title="form && form.id ? '编辑定时任务' : '新建定时任务'"
      :width="560"
      :ok-loading="saving"
      @before-ok="save"
      unmount-on-close
    >
      <template v-if="form">
        <div class="set-section">
          <label class="set-label">任务名称 *</label>
          <a-input v-model="form.name" placeholder="如：每日 9 点代码自检" max-length="60" />
        </div>
        <div class="set-section">
          <label class="set-label">cron 表达式 *（5 段：分 时 日 月 周；支持 @hourly / @daily / @every 2h）</label>
          <a-input v-model="form.expr" class="mono-input" placeholder="0 9 * * *" @input="onExprChange" @change="onExprChange" />
          <div v-if="descPreview" class="set-hint" :class="{ err: descPreview.startsWith('表达式无效') }">{{ descPreview }}</div>
        </div>
        <div class="set-section">
          <label class="set-label">任务提示词 *（每次触发时发送给 Agent 的内容）</label>
          <a-textarea v-model="form.task" :auto-size="{ minRows: 3, maxRows: 8 }" placeholder="描述每次触发要完成的工作…" />
        </div>
        <div class="set-section set-grid2">
          <div>
            <label class="set-label">工作区（空 = 当前会话工作区）</label>
            <a-input v-model="form.workspace" placeholder="D:/code/project" class="mono-input" />
          </div>
          <div>
            <label class="set-label">模式（空 = 默认）</label>
            <a-select v-model="form.mode" allow-clear placeholder="默认模式">
              <a-option v-for="m in store.modes" :key="m.Slug || m.slug" :value="m.Slug || m.slug">
                {{ m.Name || m.name }}
              </a-option>
            </a-select>
          </div>
        </div>
        <div class="set-section set-grid2">
          <div>
            <label class="set-label">超时（分钟，0 = 默认 30）</label>
            <a-input-number v-model="form.timeoutMin" :min="0" :max="1440" style="width: 100%" />
          </div>
          <div>
            <label class="set-label">失败重试（0 = 不重试）</label>
            <a-input-number v-model="form.retryLimit" :min="0" :max="3" style="width: 100%" />
          </div>
        </div>
        <div class="set-section">
          <label class="set-label">指定时间范围内运行（开启后仅在该时段内自动触发；手动“立即执行”不受限）</label>
          <div class="cron-win-row">
            <a-switch v-model="form.timeWindowEnabled" size="small" />
            <template v-if="form.timeWindowEnabled">
              <a-time-picker
                v-model="form.startTime"
                format="HH:mm"
                value-format="HH:mm"
                :allow-clear="false"
                size="mini"
                class="cron-win-time"
              />
              <span class="cron-win-sep">～</span>
              <a-time-picker
                v-model="form.endTime"
                format="HH:mm"
                value-format="HH:mm"
                :allow-clear="false"
                size="mini"
                class="cron-win-time"
              />
              <span v-if="isCrossMidnight(form.startTime, form.endTime)" class="cron-win-hint">跨零点</span>
            </template>
            <span v-else class="set-hint" style="margin-left: 8px">默认 18:00～06:00（支持跨零点）</span>
          </div>
        </div>
        <div class="set-section">
          <label class="set-label">大模型（触发任务时使用的 LLM；空 = 跟随全局配置）</label>
          <a-select
            v-model="form.model"
            allow-clear
            allow-create
            placeholder="跟随全局配置"
          >
            <a-option v-for="mm in store.models" :key="mm.Id || mm.id || mm" :value="mm.Id || mm.id || mm">
              {{ mm.Id || mm.id || mm }}
            </a-option>
          </a-select>
        </div>
        <div class="set-section">
          <label class="set-label">启用</label>
          <a-switch v-model="form.enabled" size="small" />
        </div>
        <!-- 岗位/员工/分配规则绑定编辑器（doc/09-设计方案 §5.3/§8-3） -->
        <div class="set-section" v-if="form.id">
          <label class="set-label">执行者绑定（员工/岗位二选一；选员工自动带出岗位）</label>
          <BindEditor
            resource-type="cron"
            :resource-id="form.id"
            :employee-id="form.employeeId || ''"
            :position-id="form.positionId || ''"
            :assign-rule="form.assignRule || ''"
            compact
            @saved="onBindSaved"
          />
          <div class="set-hint">新建任务先保存，再点列表「绑定」配置执行者；不绑定 = 普通会话裸跑（存量语义）。</div>
        </div>
      </template>
    </a-modal>
  </div>
</template>

<style scoped>
.cron-panel {
  height: 100%;
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px 20px;
  overflow: hidden;
}
/* 头部基底（display/align 由公共 .panel-head 提供，此处保留差异） */
.cron-head {
  gap: 10px;
  flex-shrink: 0;
}
.cron-title { font-size: var(--fs-title); color: var(--text-1); font-weight: 500; }
.cron-run-badge {
  font-size: var(--fs-micro);
  color: var(--success);
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: 999px;
  padding: 1px 8px;
}
.flex-sp { flex: 1; }

/* 快捷创建 */
.cron-quick {
  display: flex;
  gap: 10px;
  flex-shrink: 0;
}
	.cron-quick-btn {
	  flex: 1;
	  display: flex;
	  flex-direction: column;
	  align-items: flex-start;
	  gap: 2px;
	  padding: 10px 12px;
	  background: var(--bg-card);
	  border: 1px dashed var(--border);
	  border-radius: var(--radius-lg);
	  cursor: pointer;
	  text-align: left;
	  transition: border-color var(--transition), background var(--transition), transform var(--transition), box-shadow var(--transition);
	}
	.cron-quick-ico {
	  width: 26px; height: 26px;
	  display: flex; align-items: center; justify-content: center;
	  border-radius: var(--radius-sm);
	  background: var(--primary-tint);
	  color: var(--primary);
	  font-size: 14px;
	  margin-bottom: 3px;
	}
.cron-quick-name { font-size: var(--fs-caption); color: var(--text-1); line-height: 1.4; }
.cron-quick-hint { font-size: var(--fs-micro); color: var(--text-3); line-height: 1.4; }

/* 任务列表 */
/* 任务列表：spin 包裹层（flex:1+min-height:0）为滚动约束层，列表占满并可滚 */
.cron-list {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding-right: 2px;
}
	/* 卡片基底走公共 .panel-card；此处保留状态差异 */
	.cron-card {
	  gap: 6px;
	}
.cron-card.disabled { opacity: 0.65; }
.cron-card.running { border-color: rgba(63, 185, 104, 0.35); }
.cron-row1 { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; /* 小屏（375px）行内字段/按钮换行防溢出 */ }
/* 行内次要操作（启停/编辑/删除）hover 显隐：降低默认视觉密度，触达即浮现 */
.cron-ops { display: inline-flex; align-items: center; gap: 2px; opacity: 0; transition: opacity var(--transition); }
.cron-card:hover .cron-ops, .cron-card:focus-within .cron-ops { opacity: 1; }
@media (hover: none) { .cron-ops { opacity: 1; } }
.cron-dot {
  width: 7px; height: 7px; border-radius: 50%;
  flex-shrink: 0;
}
.cron-dot-run { background: var(--success); animation: cron-breath 1.4s ease-in-out infinite; }
.cron-dot-on { background: var(--success); }
.cron-dot-off { background: var(--text-3); }
.cron-dot-tripped { background: var(--danger); }
@keyframes cron-breath { 0%, 100% { opacity: 1; } 50% { opacity: 0.35; } }
.cron-name {
  font-size: var(--fs-body);
  color: var(--text-1);
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
}
.cron-badge {
  font-size: var(--fs-micro);
  padding: 0 8px;
  border-radius: 999px;
  flex-shrink: 0;
  line-height: 18px;
}
.cron-badge-on { background: var(--primary-tint); color: var(--primary); }
.cron-badge-off { background: var(--bg-hover); color: var(--text-3); }
	.cron-badge-run { background: var(--success-tint); color: var(--success); }
.cron-badge-tripped { background: var(--danger-tint-strong); color: var(--danger); }
.cron-next {
  font-size: var(--fs-micro);
  color: var(--text-3);
  flex-shrink: 0;
  white-space: nowrap;
  font-variant-numeric: tabular-nums;
}
.cron-row2 { display: flex; align-items: center; gap: 8px; min-width: 0; }
.mono { font-family: var(--font-mono); font-size: var(--fs-mono); }
.cron-expr {
  color: var(--text-2);
  background: var(--bg-inset);
  border: 1px solid var(--border-weak);
  border-radius: 4px;
  padding: 1px 6px;
  flex-shrink: 0;
  white-space: nowrap;
}
.cron-desc {
  font-size: var(--fs-caption);
  color: var(--text-2);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
}
.cron-tripped {
  font-size: var(--fs-micro);
  color: var(--danger);
  background: var(--danger-tint);
  border-radius: 4px;
  padding: 2px 8px;
}
.cron-row3 {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  font-size: var(--fs-micro);
  color: var(--text-3);
  min-width: 0;
}
.cron-task {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
  max-width: 100%;
}
.cron-meta { white-space: nowrap; }
.cron-meta.fail { color: var(--warning); }
.cron-bind { color: var(--primary); flex-shrink: 0; }

	/* 空态：挂公共 empty-block 卡片，垂直居中维持 */
	.cron-empty {
	  flex: 1;
	  min-height: 0;
	  display: flex;
	  flex-direction: column;
	  align-items: center;
	  justify-content: center;
	  gap: 8px;
	  padding: 0 24px;
	}

/* 执行记录 */
.cron-runs {
  flex-shrink: 0;
  border-top: 1px solid var(--border-weak);
  padding-top: 8px;
  max-height: 240px;
  display: flex;
  flex-direction: column;
}
.cron-runs-head {
  display: flex;
  align-items: center;
  gap: 8px;
  cursor: pointer;
  padding: 4px 0;
  user-select: none;
  border-radius: var(--radius-sm);
}
.cron-runs-head:hover, .cron-runs-head:focus-visible { background: var(--bg-hover); }
.cron-runs-head:focus-visible { outline: 1px solid var(--primary); outline-offset: -1px; }
.cron-runs-body {
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-height: 0;
}
.cron-run {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 6px;
  border-radius: var(--radius-sm);
  font-size: var(--fs-micro);
  color: var(--text-2);
  min-width: 0;
}
.cron-run:hover { background: var(--bg-hover); }
.cron-run-badge {
  flex-shrink: 0;
  font-size: var(--fs-micro);
  padding: 0 6px;
  border-radius: 999px;
  line-height: 16px;
}
.cron-badge-ok { background: var(--success-tint); color: var(--success); }
.cron-badge-err { background: var(--danger-tint); color: var(--danger); }
.cron-badge-skip { background: var(--bg-hover); color: var(--text-3); }
.cron-badge-muted { background: var(--bg-hover); color: var(--text-3); }
.cron-run-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
  flex-shrink: 1;
}
.cron-run-attempt { color: var(--warning); flex-shrink: 0; }
.cron-run-err {
  color: var(--danger);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 40%;
  flex-shrink: 1;
}
.cron-run-dur { color: var(--text-3); flex-shrink: 0; }
.cron-run-time { color: var(--text-3); flex-shrink: 0; white-space: nowrap; font-variant-numeric: tabular-nums; }

/* 时间窗口（弹窗行内控件 + 卡片徽标） */
.cron-win-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.cron-win-time { width: 108px; }
.cron-win-sep { color: var(--text-2); }
.cron-win-hint { font-size: var(--fs-micro); color: var(--warning, #d29922); }
.cron-win-badge { color: var(--text-2); }

/* 弹窗表单（复用设置抽屉 set-* 类名只能在全局引用？——本地定义对齐） */
.set-section { margin-bottom: 14px; }
.set-label {
  display: block;
  font-size: var(--fs-micro);
  color: var(--text-2);
  margin-bottom: 6px;
}
.set-hint {
  font-size: var(--fs-micro);
  color: var(--text-3);
  margin-top: 4px;
}
.set-hint.err { color: var(--danger); }
.set-grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.mono-input :deep(input) { font-family: var(--font-mono); font-size: var(--fs-mono); }
</style>
