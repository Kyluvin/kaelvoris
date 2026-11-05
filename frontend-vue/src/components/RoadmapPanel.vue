<script setup>
// ═════════════════════════════════════════════════════════════════
// 自主迭代规划板（企业级看板重设计 2026-10）
// 布局（对标企业开发管理面板）：
//   ┌ KPI 统计条（待审核/排期/进行中/完成/失败/暂停/总计）────────┐
//   ├ 工具条：搜索 · 状态 Tab · 优先级/类别筛选 · 批量操作 ────────┤
//   ├──────────────────────────────┬─────────────────────────────┤
	//   │ 数据表格（勾选/标题/状态/优先级│ 右：条目详情（选中行）        │
	//   │ /来源/更新时间/操作）          │   + AI 规划 · AI 审核        │
	//   │ 顶部：批量操作栏（全选/清空/   │   · 完全自主              │
	//   │   批量删除 · 并行/依次执行）   │                           │
//   └──────────────────────────────┴─────────────────────────────┘
// 流程：AI 规划（引导意见）→ 表格勾选/搜索过滤 → 执行（并行/依次）
//   → 状态实时流转（kaelvoris:roadmap 推送）；或 AI 审核 → 自动排期执行。
// 后端：roadmap_panel.go（RoadmapData/PlanRoadmapWithGuidance/
//   ReviewRoadmapProposals/StartRoadmapDev/DeleteRoadmapItems/...）。
// 2026-10 重设计要点：
//   - KPI 统计卡：状态计数一眼可读，点击即筛选；
//   - 搜索 + 状态 Tab + 优先级/类别筛选：长列表可快速定位；
	//   - 批选体验：表格上方批量操作栏（全选/清空/批量删除（终态）/执行）
	//     + 行内勾选 + 表头全选 + 已选计数；
//   - 表格行点击 = 右侧详情（实现要点/审核意见/结果/执行日志）。
// ═════════════════════════════════════════════════════════════════
import { computed, onMounted, ref } from "vue";
import { Message, Modal } from "@arco-design/web-vue";
import {
  IconPlus, IconRefresh, IconDelete, IconThunderbolt, IconSend, IconUndo,
  IconSearch, IconSettings, IconStorage, IconCheckCircle, IconCloseCircle,
  IconClockCircle, IconExclamationCircle,
} from "@arco-design/web-vue/es/icon";
import { bridgeCall, onEvent } from "../bridge/wails.js";
import { store, openSession as openSessionById } from "../stores/app.js";

const items = ref([]);
const stats = ref({ total: 0, proposed: 0, approved: 0, developing: 0, done: 0, failed: 0, rejected: 0, blocked: 0 });
const autoCfg = ref({
  enabled: false, intervalMin: 0, dailyLimit: 0, maxPlanItems: 0,
  reviewPrompt: "", devMode: "parallel", parallel: 0,
  autoReview: false, rejectDelete: false, minDeveloping: 0, reviewMode: "",
});
const checked = ref(new Set());     // 勾选的提案 ID（批量操作集）
const selectedId = ref("");         // 当前查看详情的条目
const loading = ref(false);
const planning = ref(false);
const reviewing = ref(false);
const executing = ref(false);
const deleting = ref(false);
const guidance = ref("");           // 用户引导意见（规划方向约束，可空）
const reviewPrompt = ref("");       // AI 审核提示词（用户可指定审核数据）
const showReviewCfg = ref(false);   // 审核提示词输入区展开态
const adding = ref(false);
const newTitle = ref("");
const newDetail = ref("");

// 编辑弹窗状态（选中任务可编辑标题/要点/优先级/类别）。
const editVisible = ref(false);
const editId = ref("");
const editTitle = ref("");
const editDetail = ref("");
const editPriority = ref("medium");
const editCategory = ref("");

// 视图状态（搜索 / 状态 Tab / 优先级 / 类别筛选）
const query = ref("");
const statusTab = ref("all");       // all | proposed | approved | developing | done | failed | blocked | rejected
const prioFilter = ref("all");
const catFilter = ref("all");

// 执行方式本地视图（并行/依次 + 并行数；保存到 autoDev 配置生效）。
const devModeView = ref("parallel");
const parallelView = ref(2);

// 完全自主模式设置视图（循环间隔/每日上限/规划任务数/最低并行数/审核开关；
// 保存到 autoDev 配置生效；用户需求 2026-10：规划任务数、自动审核、驳回删除、
// 最低并行开发数均可设置）。
const intervalView = ref(120);      // 循环间隔（分钟）
const dailyLimitView = ref(12);     // 每日自动开发上限（条）
const maxPlanItemsView = ref(5);    // 单次规划任务数上限
const minDevelopingView = ref(1);   // 最低并行开发数（低于则持续规划补货）
const autoReviewView = ref(false);  // 规划后自动 AI 审核
const rejectDeleteView = ref(false);// 审核驳回直接删除
const reviewModeView = ref("");     // 审核宽松度：宽松（默认，红线一票否决）| 严审（7维全过）
const verifyCmdsView = ref("");     // 独立验证命令（每行一条；验证权分离通道，收口 done 前规划板亲自执行）

// buildAutoCfg 汇总当前设置为 autoDev 配置对象（所有 SetAutoDevConfig 调用同源，
// 防各入口字段漂移丢配置）。
function buildAutoCfg() {
  return {
    enabled: !!autoCfg.value.enabled,
    intervalMin: Math.max(0, Number(intervalView.value) || 0),
    dailyLimit: Math.max(0, Number(dailyLimitView.value) || 0),
    maxPlanItems: Math.max(0, Number(maxPlanItemsView.value) || 0),
    reviewPrompt: reviewPrompt.value.trim(),
    devMode: devModeView.value === "serial" ? "serial" : "parallel",
    parallel: Math.min(8, Math.max(1, Number(parallelView.value) || 2)),
    autoReview: !!autoReviewView.value,
    rejectDelete: !!rejectDeleteView.value,
    minDeveloping: Math.max(0, Number(minDevelopingView.value) || 0),
    reviewMode: reviewModeView.value,
    verifyCommands: verifyCmdsView.value.split("\n").map((s) => s.trim()).filter(Boolean),
  };
}

// 状态展示映射（文案 + 颜色，暖调状态色对齐 tokens.css）。
const statusMeta = {
  proposed:   { label: "待审核", color: "orangered", tone: "proposed" },
  approved:   { label: "已排期", color: "arcoblue",  tone: "approved" },
  developing: { label: "开发中", color: "orange",    tone: "developing" },
  done:       { label: "已完成", color: "green",     tone: "done" },
  failed:     { label: "失败",   color: "red",       tone: "failed" },
  blocked:    { label: "已暂停", color: "orange",    tone: "blocked" },
  rejected:   { label: "已驳回", color: "gray",      tone: "rejected" },
};
const priorityMeta = { high: "高", medium: "中", low: "低" };
const categoryMeta = { feature: "功能", bug: "缺陷", refactor: "重构", docs: "文档", tooling: "工程", ui: "界面" };

// KPI 统计卡（点击即切换状态筛选）。
const kpis = computed(() => [
  { key: "proposed",   label: "待审核", value: stats.value.proposed,   icon: "✍", color: "var(--danger)" },
  { key: "approved",   label: "已排期", value: stats.value.approved,   icon: "◷", color: "var(--primary)" },
  { key: "developing", label: "开发中", value: stats.value.developing, icon: "⚙", color: "var(--warning)" },
  { key: "done",       label: "已完成", value: stats.value.done,       icon: "✓", color: "var(--success)" },
  { key: "failed",     label: "失败",   value: stats.value.failed,     icon: "✕", color: "var(--danger)" },
  { key: "blocked",    label: "已暂停", value: stats.value.blocked,    icon: "⛔", color: "var(--warning)" },
  { key: "total",      label: "总计",   value: stats.value.total,      icon: "∑", color: "var(--text-3)" },
]);

// 视图 Tab（状态筛选）。
const statusTabs = computed(() => [
  { key: "all", label: "全部", count: items.value.length },
  { key: "proposed", label: "待审核", count: stats.value.proposed },
  { key: "approved", label: "已排期", count: stats.value.approved },
  { key: "developing", label: "开发中", count: stats.value.developing },
  { key: "done", label: "已完成", count: stats.value.done },
  { key: "failed", label: "失败", count: stats.value.failed },
  { key: "blocked", label: "已暂停", count: stats.value.blocked },
  { key: "rejected", label: "已驳回", count: stats.value.rejected },
]);

// 过滤后的表格数据（搜索 + 状态 + 优先级 + 类别）。
const filtered = computed(() => {
  const q = query.value.trim().toLowerCase();
  return items.value.filter((it) => {
    if (statusTab.value !== "all" && it.status !== statusTab.value) return false;
    if (prioFilter.value !== "all" && (it.priority || "medium") !== prioFilter.value) return false;
    if (catFilter.value !== "all" && (it.category || "") !== catFilter.value) return false;
    if (q) {
      const hay = ((it.title || "") + " " + (it.detail || "") + " " + (it.review || "") + " " + (it.result || "")).toLowerCase();
      if (!hay.includes(q)) return false;
    }
    return true;
  });
});

// 当前筛选结果内可勾选的条目（全部状态均可进入批选/执行/删除）。
const filterIds = computed(() => filtered.value.map((x) => x.id));

// 全部已勾选判定（表头全选语义）。
const allCheckedInView = computed(() =>
  filterIds.value.length > 0 && filterIds.value.every((id) => checked.value.has(id))
);

// 已勾选的条目对象（供批量操作/计数）。
const checkedItems = computed(() => items.value.filter((x) => checked.value.has(x.id)));

// 右侧详情：选中条目（被删除时回落第一条）。
const selected = computed(() =>
  items.value.find((x) => x.id === selectedId.value) || items.value[0] || null
);

function selectItem(it) {
  selectedId.value = it.id;
}

// 终态条目标记（批量删除候选：done/failed/blocked/rejected）。
const isTerminal = (st) => ["done", "failed", "blocked", "rejected"].includes(st);

function isDevMode(m) {
  return (autoCfg.value.devMode || "parallel") === m;
}

	async function load() {
	  loading.value = true;
	  try {
	    let d = null;
	    try {
	      d = await bridgeCall("RoadmapData");
	    } catch {
	      const [itemsRaw, statsRaw, autoRaw] = await Promise.all([
	        bridgeCall("ListRoadmapItems").catch(() => []),
	        bridgeCall("RoadmapStats").catch(() => null),
	        bridgeCall("GetAutoDevConfig").catch(() => null),
	      ]);
	      d = { items: itemsRaw || [], stats: statsRaw, auto: autoRaw };
	    }
	    applyData(d);
	  } catch (e) {
	    console.warn("Roadmap load failed:", e);
	  } finally {
	    loading.value = false;
	  }
	}

function applyData(p) {
  if (!p) return;
  if (Array.isArray(p.items)) {
    items.value = p.items;
    if (selectedId.value && !p.items.some((x) => x.id === selectedId.value)) {
      selectedId.value = p.items.length ? p.items[0].id : "";
    }
    if (!selectedId.value && p.items.length) selectedId.value = p.items[0].id;
    // 勾选清理：已不存在的条目移出勾选集（防残留勾选提交被拒）。
    const ids = new Set(p.items.map((x) => x.id));
    checked.value = new Set([...checked.value].filter((id) => ids.has(id)));
  }
  if (p.stats) stats.value = p.stats;
  if (p.auto) {
    autoCfg.value = {
      enabled: false, intervalMin: 0, dailyLimit: 0, maxPlanItems: 0,
      reviewPrompt: "", devMode: "parallel", parallel: 0,
      autoReview: false, rejectDelete: false, minDeveloping: 0, verifyCommands: [], ...p.auto,
    };
    devModeView.value = autoCfg.value.devMode === "serial" ? "serial" : "parallel";
    parallelView.value = Math.min(4, Math.max(1, Number(autoCfg.value.parallel) || 2));
    intervalView.value = Math.max(1, Number(autoCfg.value.intervalMin) || 120);
    dailyLimitView.value = Math.max(1, Number(autoCfg.value.dailyLimit) || 12);
    maxPlanItemsView.value = Math.min(20, Math.max(1, Number(autoCfg.value.maxPlanItems) || 5));
    minDevelopingView.value = Math.min(8, Math.max(1, Number(autoCfg.value.minDeveloping) || 1));
    autoReviewView.value = !!autoCfg.value.autoReview;
    rejectDeleteView.value = !!autoCfg.value.rejectDelete;
    reviewModeView.value = autoCfg.value.reviewMode === "standard" ? "standard" : "";
    verifyCmdsView.value = (autoCfg.value.verifyCommands || []).join("\n");
    if (autoCfg.value.reviewPrompt && !reviewPrompt.value) reviewPrompt.value = autoCfg.value.reviewPrompt;
  }
}

function toggle(id) {
  const next = new Set(checked.value);
  next.has(id) ? next.delete(id) : next.add(id);
  checked.value = next;
}

// 表头全选/反选：当前筛选结果内全部勾选（或全部取消）。
function toggleAllInView() {
  const next = new Set(checked.value);
  if (allCheckedInView.value) {
    filterIds.value.forEach((id) => next.delete(id));
  } else {
    filterIds.value.forEach((id) => next.add(id));
  }
  checked.value = next;
}

// 按状态组全选（KPI 卡点击的增强：该状态全部勾选）。
function checkAllByStatus(status) {
  const next = new Set(checked.value);
  items.value.forEach((it) => {
    if (it.status === status) next.add(it.id);
  });
  checked.value = next;
}

// 清空勾选。
function clearChecked() {
  checked.value = new Set();
}

// 让 AI 规划（聚合项目现状 → LLM 产出提案 → 待审核；
// 有引导意见时走 PlanRoadmapWithGuidance，意见作为最高优先级方向注入）。
async function plan() {
  planning.value = true;
  Message.info("AI 正在分析项目并规划…");
  try {
    const g = guidance.value.trim();
    if (g) {
      await bridgeCall("PlanRoadmapWithGuidance", g);
    } else {
      await bridgeCall("PlanRoadmap");
    }
    setTimeout(load, 800);
  } catch (e) {
    Message.error(String(e));
  } finally {
    planning.value = false;
  }
}

// AI 审核：勾选（空 = 全部待审核）交 LLM 审查——通过自动纳入排期并执行。
async function review() {
  reviewing.value = true;
  Message.info("AI 审核中：通过的提案将自动纳入排期并开始开发…");
  try {
    const ids = checked.value.size ? Array.from(checked.value) : [];
    await bridgeCall("ReviewRoadmapProposals", ids, reviewPrompt.value.trim());
    setTimeout(load, 1500);
  } catch (e) {
    Message.error(String(e));
  } finally {
    reviewing.value = false;
  }
}

// 执行：勾选项批准 → 按并行/依次启动开发（并行数 1~4，默认并行）。
async function execDev() {
  if (!checked.value.size) { Message.warning("请先勾选要开发的条目"); return; }
  executing.value = true;
  try {
    const serial = devModeView.value === "serial";
    const parallel = serial ? 1 : Math.min(4, Math.max(1, Number(parallelView.value) || 2));
    await persistDevMode(serial ? "serial" : "parallel", parallel);
    await bridgeCall("StartRoadmapDev", Array.from(checked.value), serial ? "serial" : "parallel", parallel);
    Message.success(`已提交 ${checked.value.size} 条进入开发（${serial ? "依次执行" : `并行 ${parallel} 路`}）`);
    checked.value = new Set();
    await load();
  } catch (e) {
    Message.error(String(e));
  } finally {
    executing.value = false;
  }
}

// persistDevMode 把执行方式写入 autoDev 配置（开发循环读取同一配置）。
async function persistDevMode(devMode, parallel) {
  try {
    const cfg = buildAutoCfg();
    cfg.devMode = devMode;
    cfg.parallel = parallel;
    await bridgeCall("SetAutoDevConfig", cfg);
  } catch (e) {
    console.warn("persistDevMode failed:", e);
  }
}

// 保存自动模式设置（审核提示词/规划任务数/自动审核/驳回删除/并行数/
// 每日上限/循环间隔/最低并行数）。
async function saveAutoCfg() {
  try {
    await bridgeCall("SetAutoDevConfig", buildAutoCfg());
    Message.success("设置已保存");
    await load();
  } catch (e) {
    Message.error(String(e));
  }
}

// 完全自主开关：开启后系统自动规划 + AI 审核 + 并行开发（无需人工审核）。
async function toggleAuto(val) {
  try {
    const cfg = buildAutoCfg();
    cfg.enabled = !!val;
    await bridgeCall("SetAutoDevConfig", cfg);
    Message.success(val ? "自主迭代已开启：开发中不足将持续规划补货，规划 → AI 审核 → 排期 → 自动开发" : "自主迭代已关闭：提案需人工审核");
  } catch (e) {
    Message.error(String(e));
    autoCfg.value = { ...autoCfg.value, enabled: !val };
  }
  await load();
}

async function retryItem(id) {
  try { await bridgeCall("RetryRoadmapItem", id); await load(); }
  catch (e) { Message.error(String(e)); }
}

async function rejectItem(id) {
  try { await bridgeCall("RejectRoadmapItem", id); await load(); }
  catch (e) { Message.error(String(e)); }
}

function removeItem(id) {
  Modal.confirm({
    title: "删除规划条目",
    content: "删除后不可恢复，确定删除该条目？",
    onOk: async () => {
      try { await bridgeCall("DeleteRoadmapItem", id); await load(); }
      catch (e) { Message.error(String(e)); }
    },
  });
}

// 批量删除（仅终态条目；开发中条目后端自动跳过）。
async function removeChecked() {
  const targets = checkedItems.value.filter((x) => isTerminal(x.status));
  const developing = checkedItems.value.length - targets.length;
  if (!targets.length) {
    Message.warning(developing ? "勾选中没有终态条目（开发中条目不可删除）" : "请先勾选要删除的条目");
    return;
  }
  Modal.confirm({
    title: "批量删除规划条目",
    content: `将删除已勾选的 ${targets.length} 条条目${developing ? `（另有 ${developing} 条开发中已跳过）` : ""}，删除后不可恢复。确认删除？`,
    onOk: async () => {
      deleting.value = true;
      try {
        await bridgeCall("DeleteRoadmapItems", targets.map((x) => x.id));
        checked.value = new Set();
        Message.success(`已删除 ${targets.length} 条`);
        await load();
      } catch (e) {
        Message.error(String(e));
      } finally {
        deleting.value = false;
      }
    },
  });
}

async function addItem() {
  if (!newTitle.value.trim()) { Message.warning("请填写条目标题"); return; }
  adding.value = true;
  try {
    await bridgeCall("AddRoadmapItem", newTitle.value.trim(), newDetail.value.trim(), "medium");
    newTitle.value = ""; newDetail.value = "";
    adding.value = false;
    await load();
  } catch (e) {
    Message.error(String(e));
  } finally {
    adding.value = false;
  }
}

// 跳转开发会话回放（侧栏聚焦该会话）。
async function openSession(sid) {
  if (!sid) return;
  try { await openSessionById(sid); } catch (e) { console.warn(e); }
  store.activeMain = "chat";
}

function statusOf(s) {
  return statusMeta[s] || { label: s, color: "gray", tone: "muted" };
}

// 执行日志格式化（最近一次：耗时 + 结论 + 改动文件数；无日志返回空）。
function latestRun(it) {
  return Array.isArray(it.runLog) && it.runLog.length ? it.runLog[0] : null;
}

function historyRuns(it) {
  return Array.isArray(it.runLog) ? it.runLog.slice(1, 4) : [];
}

function runFiles(it) {
  const r = Array.isArray(it.runLog) && it.runLog[0];
  return r && Array.isArray(r.files) ? r.files.slice(0, 6) : [];
}

function fmtDuration(ms) {
  if (!ms || ms <= 0) return "-";
  if (ms < 60000) return Math.round(ms / 1000) + "s";
  return Math.floor(ms / 60000) + "m" + Math.round((ms % 60000) / 1000) + "s";
}

function fmtTime(iso) {
  if (!iso) return "-";
  try {
    const d = new Date(iso);
    if (isNaN(d.getTime())) return "-";
    return d.toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
  } catch { return "-"; }
}

// 工具函数：取文本首行并截断（表格副行展示用）。
function firstLineText(s, n) {
  const line = String(s || "").split("\n")[0];
  return line.length > n ? line.slice(0, n) + "…" : line;
}

// 打开编辑弹窗（预填当前条目字段）。
function openEdit(it) {
  editId.value = it.id;
  editTitle.value = it.title || "";
  editDetail.value = it.detail || "";
  editPriority.value = it.priority || "medium";
  editCategory.value = it.category || "";
  editVisible.value = true;
}

// 保存编辑（调用后端 UpdateRoadmapItem；标题必填）。
async function saveEdit() {
  if (!editTitle.value.trim()) { Message.warning("请填写条目标题"); return false; }
  try {
    await bridgeCall("UpdateRoadmapItem", editId.value, editTitle.value.trim(), editDetail.value.trim(), editPriority.value, editCategory.value);
    editVisible.value = false;
    Message.success("条目已更新");
    await load();
    return true;
  } catch (e) {
    Message.error(String(e));
    return false;
  }
}

// 操作按钮设置（行内胶囊）。
function rowActions(it) {
  const acts = [];
  if (it.status === "proposed") acts.push({ key: "choose", label: checked.value.has(it.id) ? "取消选择" : "选择", onClick: () => toggle(it.id) });
  if (it.status === "failed" || it.status === "blocked") acts.push({ key: "retry", label: "重跑", onClick: () => retryItem(it.id) });
  if (it.status === "proposed") acts.push({ key: "reject", label: "驳回", danger: true, onClick: () => rejectItem(it.id) });
  if (it.sessionId) acts.push({ key: "session", label: "会话", onClick: () => openSession(it.sessionId) });
  acts.push({ key: "edit", label: "编辑", onClick: () => openEdit(it) });
  acts.push({ key: "del", label: "删除", danger: true, onClick: () => removeItem(it.id) });
  return acts;
}

onMounted(() => {
  load();
  onEvent("kaelvoris:roadmap", applyData);
});
</script>

<template>
  <div class="rm-page">
    <!-- ⓪ hero 页头（对齐其他面板 panel-head 规范） -->
    <div class="rm-head panel-head">
      <span class="panel-icon"><IconThunderbolt /></span>
      <span class="panel-title">自主迭代</span>
      <span class="panel-sub">AI 规划 → 审核 → 排期开发闭环 · {{ stats.total }} 条提案</span>
      <span class="flex-sp"></span>
      <a-tooltip content="刷新">
        <button class="icon-btn" :disabled="loading" @click="load"><IconRefresh /></button>
      </a-tooltip>
    </div>

    <!-- ① KPI 统计条（点击 KPI 卡切换状态筛选） -->
    <div class="rm-kpis">
      <button
        v-for="k in kpis"
        :key="k.key"
        class="rm-kpi"
        :class="{ on: (k.key === 'all' ? statusTab === 'all' : statusTab === k.key) }"
        type="button"
        @click="statusTab = k.key"
      >
        <span class="rm-kpi-ico" :style="{ color: k.color }">{{ k.icon }}</span>
        <span class="rm-kpi-num" :style="{ color: k.color }">{{ k.value }}</span>
        <span class="rm-kpi-label">{{ k.label }}</span>
      </button>
    </div>

    <!-- ② 工具条：搜索 + 状态 Tab + 筛选 + 批量操作 -->
    <div class="rm-toolbar">
      <div class="rm-search">
        <IconSearch class="rm-search-ico" />
        <input
          v-model="query"
          class="rm-search-inp"
          placeholder="搜索标题 / 要点 / 结果…"
          autocapitalize="off"
          autocorrect="off"
        />
        <button v-if="query" class="rm-search-clear" type="button" @click="query = ''">✕</button>
      </div>
      <a-select v-model="prioFilter" size="small" class="rm-filt" placeholder="优先级">
        <a-option value="all">全部优先级</a-option>
        <a-option value="high">高</a-option>
        <a-option value="medium">中</a-option>
        <a-option value="low">低</a-option>
      </a-select>
      <a-select v-model="catFilter" size="small" class="rm-filt" placeholder="类别">
        <a-option value="all">全部类别</a-option>
        <a-option v-for="(label, key) in categoryMeta" :key="key" :value="key">{{ label }}</a-option>
      </a-select>
      <div class="spacer"></div>
      <a-button size="small" @click="adding = !adding">
        <template #icon><IconPlus /></template>
        手动添加
      </a-button>
      <a-button size="small" @click="load">
        <template #icon><IconRefresh /></template>
        刷新
      </a-button>
    </div>

    <!-- 状态 Tab（快速切换筛选用） -->
    <div class="rm-tabs">
      <button
        v-for="t in statusTabs"
        :key="t.key"
        class="rm-tab"
        :class="{ on: statusTab === t.key }"
        type="button"
        @click="statusTab = t.key"
      >{{ t.label }}<span class="rm-tab-n">{{ t.count }}</span></button>
    </div>

    <!-- 手动添加表单 -->
    <div v-if="adding" class="rm-add-box">
      <a-input v-model="newTitle" size="small" placeholder="功能点标题（一句话）" @keyup.enter="addItem" />
      <a-textarea v-model="newDetail" :auto-size="{ minRows: 2, maxRows: 4 }" placeholder="实现要点（可选：涉及模块 / 验收标准）" />
      <div class="rm-add-actions">
        <a-button size="small" type="primary" :loading="adding" @click="addItem">加入规划</a-button>
        <a-button size="small" @click="adding = false">取消</a-button>
      </div>
    </div>

    <div class="rm-body">
      <!-- 左：数据表格（顶部批量操作栏 + 表格） -->
      <div class="rm-table-wrap">
        <!-- 批量操作栏（表格上方：全选/清空/批量删除 · 并行/依次执行） -->
        <div class="rm-batchbar">
          <div class="rm-batchbar-left">
            <span class="rm-sel-count">已选 <b>{{ checked.size }}</b> 条</span>
            <a-button size="mini" :disabled="!filterIds.length" @click="toggleAllInView">
              {{ allCheckedInView ? "取消全选" : "全选" }}
            </a-button>
            <a-button size="mini" :disabled="!checked.size" @click="clearChecked">清空</a-button>
            <a-button
              size="mini"
              :disabled="!checkedItems.some((x) => isTerminal(x.status))"
              :loading="deleting"
              status="danger"
              @click="removeChecked"
            >
              批量删除{{ checkedItems.some((x) => isTerminal(x.status)) ? `（${checkedItems.filter((x) => isTerminal(x.status)).length}）` : "" }}
            </a-button>
          </div>
          <div class="rm-batchbar-right">
            <a-radio-group v-model="devModeView" size="mini" type="button">
              <a-radio value="parallel">并行</a-radio>
              <a-radio value="serial">依次</a-radio>
            </a-radio-group>
            <a-input-number
              v-if="devModeView === 'parallel'"
              v-model="parallelView"
              size="mini"
              :min="1"
              :max="4"
              style="width: 100px"
            >
              <template #prefix>并行</template>
              <template #suffix>路</template>
            </a-input-number>
            <a-button type="primary" size="small" :loading="executing" :disabled="!checked.size" @click="execDev">
              <template #icon><IconSend /></template>
              执行{{ checked.size ? `（${checked.size}）` : "" }}
            </a-button>
          </div>
        </div>
		        <div class="rm-table-scroll" v-show="filtered.length || loading">
        <table class="rm-table">
          <thead>
            <tr>
              <th class="rm-col-check">
                <a-checkbox
                  :model-value="allCheckedInView"
                  :indeterminate="checked.size > 0 && !allCheckedInView"
                  @change="toggleAllInView"
                />
              </th>
              <th class="rm-col-title">条目</th>
              <th class="rm-col-status">状态</th>
              <th class="rm-col-prio">优先级</th>
              <th class="rm-col-cat">类别</th>
              <th class="rm-col-src">来源</th>
              <th class="rm-col-time">更新时间</th>
              <th class="rm-col-act">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="it in filtered"
              :key="it.id"
              class="rm-row"
              :class="{ checked: checked.has(it.id), active: selectedId === it.id }"
              @click="selectItem(it)"
            >
              <td class="rm-col-check" @click.stop>
                <a-checkbox :model-value="checked.has(it.id)" @change="toggle(it.id)" />
              </td>
              <td class="rm-col-title">
                <div class="rm-cell-title">
                  {{ it.title }}
                  <span v-if="it.status === 'developing'" class="rm-dev-dot"></span>
                </div>
                <div v-if="it.review || it.result || it.error" class="rm-cell-sub">
                  <template v-if="it.review">{{ firstLineText(it.review, 60) }}</template>
                  <template v-else-if="it.status === 'done'">{{ firstLineText(it.result, 60) }}</template>
                  <template v-else-if="it.status === 'failed' || it.status === 'blocked'">{{ firstLineText(it.blockedReason || it.error, 60) }}</template>
                </div>
              </td>
              <td class="rm-col-status">
                <span class="rm-badge" :class="'b-' + statusOf(it.status).tone">{{ statusOf(it.status).label }}</span>
                <span v-if="it.attempts && it.attempts > 1" class="rm-attempt">×{{ it.attempts }}</span>
              </td>
              <td class="rm-col-prio">
                <span v-if="it.priority && it.priority !== 'medium'" class="rm-prio" :class="'p-' + it.priority">{{ priorityMeta[it.priority] }}</span>
                <span v-else class="rm-prio p-medium">中</span>
              </td>
              <td class="rm-col-cat">{{ categoryMeta[it.category] || "-" }}</td>
              <td class="rm-col-src">{{ it.source === "manual" ? "手动" : it.source === "autodev" ? "自动" : it.source === "remote" ? "远端" : "AI" }}</td>
              <td class="rm-col-time">{{ fmtTime(it.updatedAt) }}</td>
              <td class="rm-col-act" @click.stop>
                <div class="rm-row-acts">
                  <template v-for="act in rowActions(it)" :key="act.key">
                    <button
                      class="rm-act"
                      :class="{ danger: act.danger }"
                      type="button"
                      @click="act.onClick"
                    >{{ act.label }}</button>
                  </template>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        </div>
        <div v-if="!filtered.length && !loading" class="rm-table-empty">
          <div class="rm-empty-logo">📋</div>
          <div class="rm-empty-title">{{ query || statusTab !== 'all' ? "无匹配条目" : "让系统开始自主迭代" }}</div>
          <div class="rm-empty-desc">
            {{ query || statusTab !== 'all' ? "调整搜索或筛选条件后重试" : "点右上「AI 规划」分析项目产出提案；勾选后「执行」开发；或开启「完全自主」交给系统持续迭代。" }}
          </div>
          <a-button v-if="query || statusTab !== 'all'" size="small" @click="query = ''; statusTab = 'all'">清除筛选</a-button>
        </div>

      </div>

      <!-- 右：详情面板 + 操作（AI 规划/审核/完全自主） -->
      <div class="rm-side">
        <!-- 详情（选中行） -->
        <div class="rm-detail">
          <div class="rm-detail-panel">
            <div v-if="selected" class="rm-detail-inner">
              <div class="rm-detail-head">
                <span class="rm-detail-title">{{ selected.title }}</span>
                <span class="rm-badge" :class="'b-' + statusOf(selected.status).tone">{{ statusOf(selected.status).label }}</span>
              </div>
              <div class="rm-detail-meta">
                <span v-if="selected.priority" class="rm-prio" :class="'p-' + selected.priority">{{ priorityMeta[selected.priority] }}优先</span>
                <span v-if="selected.category" class="rm-meta-chip">{{ categoryMeta[selected.category] }}</span>
                <span class="rm-meta-chip">{{ selected.source === "manual" ? "手动添加" : selected.source === "autodev" ? "自动规划" : "AI 规划" }}</span>
                <span v-if="selected.approvedBy === 'ai_review'" class="rm-meta-chip ai">AI 审核通过</span>
              </div>
              <div v-if="selected.detail" class="rm-detail-block">
                <div class="rm-detail-label">实现要点</div>
                <div class="rm-detail-text">{{ selected.detail }}</div>
              </div>
              <div v-if="selected.review" class="rm-detail-block">
                <div class="rm-detail-label">AI 审核意见</div>
                <div class="rm-detail-text review">{{ selected.review }}</div>
              </div>
              <div v-if="selected.result" class="rm-detail-block">
                <div class="rm-detail-label">完成摘要</div>
                <div class="rm-detail-text ok">{{ selected.result }}</div>
              </div>
              <div v-if="selected.error" class="rm-detail-block">
                <div class="rm-detail-label">失败原因</div>
                <div class="rm-detail-text err">{{ selected.blockedReason || selected.error }}</div>
              </div>
              <div v-if="latestRun(selected)" class="rm-detail-block">
                <div class="rm-detail-label">
                  最近执行（{{ latestRun(selected).outcome === 'done' ? '成功' : latestRun(selected).outcome === 'blocked' ? '重试超限暂停' : '失败' }}<template v-if="selected.attempts"> · 尝试 {{ selected.attempts }} 次</template>）
                </div>
                <div class="rm-detail-text run">
                  耗时 {{ fmtDuration(latestRun(selected).durationMS) }}<template v-if="runFiles(selected).length"> · 改动：{{ runFiles(selected).join("、") }}</template>
                </div>
              </div>
              <div v-for="(r, i) in historyRuns(selected)" :key="i" class="rm-runlog-hist">
                <span class="rm-runlog-outcome" :class="r.outcome">{{ r.outcome === 'done' ? '✓' : r.outcome === 'blocked' ? '⛔' : '✗' }} {{ r.outcome === 'done' ? '完成' : r.outcome === 'blocked' ? '暂停' : '失败' }}</span>
                <span class="rm-runlog-dur">{{ fmtDuration(r.durationMS) }}</span>
                <span class="rm-runlog-sum">{{ r.summary }}</span>
              </div>
              <div class="rm-detail-foot">
                <a-button v-if="selected.status === 'proposed'" size="mini" type="primary" @click="toggle(selected.id)">
                  {{ checked.has(selected.id) ? "取消选择" : "选择开发" }}
                </a-button>
                <a-button v-if="selected.status === 'failed' || selected.status === 'blocked'" size="mini" @click="retryItem(selected.id)">重跑</a-button>
                <a-button size="mini" @click="openEdit(selected)">编辑</a-button>
                <a-button v-if="selected.status === 'proposed'" size="mini" status="danger" @click="rejectItem(selected.id)">驳回</a-button>
                <a-button size="mini" status="danger" @click="removeItem(selected.id)">删除</a-button>
                <a-button v-if="selected.sessionId" size="mini" @click="openSession(selected.sessionId)">查看会话</a-button>
              </div>
            </div>
            <div v-else class="rm-detail-empty">点击表格行查看具体内容</div>
          </div>
        </div>

	        <!-- ① AI 规划（引导意见） -->
	        <div class="rm-panel">
	          <div class="rm-panel-title">AI 规划</div>
	          <a-textarea
	            v-model="guidance"
	            :auto-size="{ minRows: 2, maxRows: 4 }"
	            placeholder="引导意见（选填）：本次规划方向，如「优先优化移动端体验」"
	          />
	          <a-button size="small" type="primary" :loading="planning" long @click="plan">
	            <template #icon><IconThunderbolt /></template>
	            生成规划
	          </a-button>
	          <div class="rm-mini-row">
	            <a-input-number v-model="maxPlanItemsView" size="mini" :min="1" :max="20" style="width: 108px">
	              <template #prefix>规划</template>
	              <template #suffix>条</template>
	            </a-input-number>
	            <a-tooltip content="单次 AI 规划最多产出的任务数；手动与自动规划同源生效">
	              <span class="rm-mini-hint">单次规划任务数</span>
	            </a-tooltip>
	          </div>
	          <div class="rm-check-row">
	            <a-checkbox v-model="autoReviewView">规划后自动 AI 审核</a-checkbox>
	          </div>
	          <div class="rm-check-row" v-if="autoReviewView">
	            <a-checkbox v-model="rejectDeleteView">审核驳回直接删除（不留痕）</a-checkbox>
	          </div>
	          <a-button size="mini" @click="saveAutoCfg">保存设置</a-button>
	        </div>

        <!-- ② AI 审核（用户可指定审核提示词） -->
        <div class="rm-panel">
          <div class="rm-panel-head" @click="showReviewCfg = !showReviewCfg">
            <span class="rm-panel-title">AI 审核</span>
            <span class="rm-arrow">{{ showReviewCfg ? "▾" : "▸" }}</span>
          </div>
          <div v-if="showReviewCfg" class="rm-panel-body">
            <a-textarea
              v-model="reviewPrompt"
              :auto-size="{ minRows: 2, maxRows: 4 }"
              placeholder="审核提示词（选填）：作为审核数据的补充判据，如「仅通过与性能/稳定性相关且不改动数据库结构的提案」"
            />
            <a-button size="mini" long @click="saveAutoCfg">保存为默认审核提示词</a-button>
            <div class="rm-mini-row" style="margin-top: 6px">
              <a-radio-group v-model="reviewModeView" size="mini" type="button">
                <a-radio value="">宽松</a-radio>
                <a-radio value="standard">严审</a-radio>
              </a-radio-group>
              <a-tooltip content="宽松：仅红线（无据/风险/重复）一票否决，其余存疑交给开发验证；严审：7 项准则全部达标才通过。审核全驳回时自动换宽松模式重审一次">
                <span class="rm-mini-hint">审核宽松度</span>
              </a-tooltip>
            </div>
          </div>
          <a-button size="small" :loading="reviewing" long @click="review">
            <template #icon><IconSearch /></template>
            AI 审核{{ checked.size ? `（${checked.size}）` : "（全部待审核）" }}
          </a-button>
          <div class="rm-panel-hint">审核通过的提案自动纳入排期并立即开发</div>
        </div>

	        <!-- ③ 完全自主模式 -->
	        <div class="rm-panel">
	          <div class="rm-auto-row">
	            <span class="rm-panel-title">完全自主</span>
	            <a-switch size="small" :model-value="autoCfg.enabled" @change="toggleAuto" />
	          </div>
	          <div class="rm-panel-hint">开启后：自动规划 → AI 审核 → 自动排期 → 并行开发；开发中不足最低并行数时持续规划补货，无需人工审核。</div>
	          <div v-if="autoCfg.enabled" class="rm-auto-cfg">
	            <div class="rm-mini-row">
	              <span class="rm-mini-label">循环间隔</span>
	              <a-input-number v-model="intervalView" size="mini" :min="1" :max="1440" style="width: 104px">
	                <template #suffix>分</template>
	              </a-input-number>
	            </div>
	            <div class="rm-mini-row">
	              <span class="rm-mini-label">每日上限</span>
	              <a-input-number v-model="dailyLimitView" size="mini" :min="1" :max="100" style="width: 104px">
	                <template #suffix>条</template>
	              </a-input-number>
	            </div>
	            <div class="rm-mini-row">
	              <span class="rm-mini-label">单次规划</span>
	              <a-input-number v-model="maxPlanItemsView" size="mini" :min="1" :max="20" style="width: 104px">
	                <template #suffix>条</template>
	              </a-input-number>
	            </div>
	            <div class="rm-mini-row">
	              <span class="rm-mini-label">并行开发</span>
	              <a-input-number v-model="parallelView" size="mini" :min="1" :max="8" style="width: 104px">
	                <template #suffix>路</template>
	              </a-input-number>
	            </div>
	            <div class="rm-mini-row">
	              <span class="rm-mini-label">最低并行</span>
	              <a-input-number v-model="minDevelopingView" size="mini" :min="1" :max="8" style="width: 104px">
	                <template #suffix>条</template>
	              </a-input-number>
	            </div>
	            <div class="rm-check-row">
	              <a-checkbox v-model="autoReviewView">规划后自动 AI 审核</a-checkbox>
	              <a-checkbox v-model="rejectDeleteView">驳回直接删除</a-checkbox>
	            </div>
	            <!-- 独立验证通道（验证权分离，对标 MetaGPT X Verifier）：收口 done 前规划板亲自重跑验证命令，
	              不采信会话自报；留空按工作区类型自动推断（go.mod/package.json），填 "-" 显式关闭 -->
	            <div class="rm-verify-row">
	              <span class="rm-mini-label">独立验证命令（每行一条，收口时规划板亲自复核）</span>
	              <a-textarea
	                v-model="verifyCmdsView"
	                size="mini"
	                :auto-size="{ minRows: 2, maxRows: 5 }"
	                placeholder="每行一条，如：go build ./...&#10;留空 = 自动识别（go.mod → go build/test；package.json → npm run build）&#10;填 - 关闭复核"
	              />
	            </div>
	            <a-button size="mini" type="primary" long @click="saveAutoCfg">保存设置</a-button>
	            <div class="rm-panel-hint">开发中 + 排期 < 最低并行时，立即规划并 AI 审核；驳回{{ rejectDeleteView ? "直接删除" : "留痕回溯" }}。条目收口 done 前会先核验会话是否跑过验证命令，再亲自重跑独立验证命令复核，未过一律降级失败重试（验收测试先行：开发任务要求先写验收测试再实现）。</div>
	          </div>
	          <div v-if="autoCfg.enabled" class="rm-auto-status">
	            <IconClockCircle /> 间隔 {{ intervalView }} 分 · 每日上限 {{ dailyLimitView }} 条 · 单次规划 {{ maxPlanItemsView }} 条 · 最低并行 {{ minDevelopingView }} 路
	          </div>
	        </div>
      </div>
    </div>
    <!-- 编辑条目弹窗（AI 规划的任务，用户可修改内容后审批开发） -->
    <a-modal
      v-model:visible="editVisible"
      title="编辑规划条目"
      :ok-loading="false"
      @before-ok="saveEdit"
    >
      <a-form layout="vertical">
        <a-form-item label="标题" required>
          <a-input v-model="editTitle" placeholder="功能点标题（一句话）" :max-length="80" />
        </a-form-item>
        <a-form-item label="实现要点">
          <a-textarea v-model="editDetail" :auto-size="{ minRows: 3, maxRows: 6 }" placeholder="涉及模块 / 验收标准（可空）" />
        </a-form-item>
        <a-form-item label="优先级">
          <a-radio-group v-model="editPriority" type="button">
            <a-radio value="high">高</a-radio>
            <a-radio value="medium">中</a-radio>
            <a-radio value="low">低</a-radio>
          </a-radio-group>
        </a-form-item>
        <a-form-item label="类别">
          <a-select v-model="editCategory" allow-clear placeholder="选择类别">
            <a-option v-for="(label, key) in categoryMeta" :key="key" :value="key">{{ label }}</a-option>
          </a-select>
        </a-form-item>
      </a-form>
    </a-modal>

  </div>
</template>

<style scoped>
.rm-page {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-height: 0;
  padding: var(--pad-block);
  gap: var(--gap);
}

/* ── hero 页头（挂公共 panel-head，去内建 padding/边框适配 rm-page 已有内边距）── */
.rm-head { gap: 8px; flex-shrink: 0; padding-left: 0; padding-right: 0; border-bottom: none; margin-bottom: 2px; }

/* ── KPI 统计条 ─────────────────────────────── */
.rm-kpis {
  display: grid;
  grid-template-columns: repeat(7, 1fr);
  gap: var(--gap-sm);
  flex-shrink: 0;
}
.rm-kpi {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 2px;
  padding: 10px 4px;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: border-color var(--transition), background var(--transition);
}
.rm-kpi:hover { background: var(--bg-hover); }
.rm-kpi.on { border-color: var(--primary); background: var(--bg-active); }
.rm-kpi-ico { font-size: 15px; line-height: 1; }
.rm-kpi-num { font-size: 20px; font-weight: 700; font-family: var(--font-mono, monospace); line-height: 1.2; }
.rm-kpi-label { font-size: var(--fs-micro); color: var(--text-3); }

/* ── 工具条 ─────────────────────────────────── */
.rm-toolbar {
  display: flex;
  align-items: center;
  gap: var(--gap-sm);
  flex-shrink: 0;
  flex-wrap: wrap;
}
.rm-search {
  position: relative;
  flex: 1;
  min-width: 180px;
  display: flex;
  align-items: center;
}
.rm-search-ico { position: absolute; left: 10px; color: var(--text-3); font-size: 14px; pointer-events: none; }
.rm-search-inp {
  width: 100%;
  padding: 7px 28px 7px 30px;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  color: var(--text-1);
  font-size: var(--fs-caption);
  outline: none;
  transition: border-color var(--transition);
}
.rm-search-inp:focus { border-color: var(--primary); }
.rm-search-clear {
  position: absolute; right: 8px;
  background: none; border: none; color: var(--text-3); cursor: pointer; font-size: 12px; padding: 2px;
}
.rm-search-clear:hover { color: var(--text-1); }
.rm-filt { width: 110px; flex-shrink: 0; }
.spacer { flex: 1; }

/* ── 状态 Tab ───────────────────────────────── */
.rm-tabs {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
  flex-wrap: wrap;
  border-bottom: 1px solid var(--border-weak);
  padding-bottom: 6px;
}
.rm-tab {
  padding: 5px 12px;
  font-size: var(--fs-caption);
  color: var(--text-2);
  background: none;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: background var(--transition), color var(--transition);
}
.rm-tab:hover { background: var(--bg-hover); color: var(--text-1); }
.rm-tab.on { background: var(--bg-active); color: var(--primary); border-color: var(--primary); }
.rm-tab-n { margin-left: 5px; font-family: var(--font-mono, monospace); font-size: var(--fs-micro); color: var(--text-3); }
.rm-tab.on .rm-tab-n { color: var(--primary); }

/* ── 手动添加 ───────────────────────────────── */
.rm-add-box {
  display: flex;
  flex-direction: column;
  gap: var(--gap-sm);
  padding: var(--gap);
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  flex-shrink: 0;
}
.rm-add-actions { display: flex; justify-content: flex-end; gap: var(--gap-sm); }

/* ── 主体：表格 + 详情 ──────────────────────── */
.rm-body {
  flex: 1;
  display: flex;
  gap: var(--gap);
  min-height: 0;
}

/* 左：表格区 */
.rm-table-wrap {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  min-height: 0;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  overflow: hidden;
}
.rm-table-scroll {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  overflow-x: hidden;
}
.rm-table {
  width: 100%;
  border-collapse: collapse;
  table-layout: fixed;
}
.rm-table thead th {
  position: sticky;
  top: 0;
  z-index: 2;
  text-align: left;
  padding: 8px 10px;
  font-size: var(--fs-micro);
  font-weight: 600;
  color: var(--text-3);
  background: var(--bg-page);
  border-bottom: 1px solid var(--border-weak);
  white-space: nowrap;
  user-select: none;
}
	.rm-row {
  border-bottom: 1px solid var(--border-weak);
  cursor: pointer;
  transition: background var(--transition);
}
.rm-row:hover { background: var(--bg-hover); }
.rm-row.active { background: var(--bg-active); }
.rm-row.checked { box-shadow: inset 3px 0 0 var(--primary); }
.rm-row td { padding: 8px 10px; vertical-align: middle; }
.rm-col-check { width: 36px; text-align: center; }
.rm-col-title { width: 38%; }
.rm-col-status { width: 90px; }
.rm-col-prio { width: 56px; text-align: center; }
.rm-col-cat { width: 64px; text-align: center; }
.rm-col-src { width: 52px; text-align: center; }
.rm-col-time { width: 96px; }
.rm-col-act { width: 172px; }

.rm-cell-title {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: var(--fs-caption);
  color: var(--text-1);
  line-height: 1.5;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.rm-cell-sub {
  font-size: var(--fs-micro);
  color: var(--text-3);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  margin-top: 2px;
}
.rm-dev-dot {
  width: 7px; height: 7px; border-radius: 50%;
  background: var(--warning);
  flex-shrink: 0;
  animation: rm-pulse 1.6s ease-in-out infinite;
}
@keyframes rm-pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.35; } }

/* 状态徽章（企业 tabular badge） */
.rm-badge {
  display: inline-flex;
  align-items: center;
  padding: 2px 8px;
  font-size: var(--fs-micro);
  font-weight: 600;
  border-radius: 999px;
  border: 1px solid transparent;
  white-space: nowrap;
}
.b-proposed   { color: var(--danger);  background: rgba(224, 101, 83, 0.12);  border-color: rgba(224, 101, 83, 0.28); }
.b-approved   { color: var(--primary); background: var(--primary-tint);          border-color: rgba(217, 119, 87, 0.3); }
.b-developing { color: var(--warning); background: rgba(217, 150, 62, 0.12);    border-color: rgba(217, 150, 62, 0.3); }
.b-done       { color: var(--success); background: rgba(63, 185, 104, 0.12);    border-color: rgba(63, 185, 104, 0.3); }
.b-failed     { color: var(--danger);  background: rgba(224, 101, 83, 0.12);    border-color: rgba(224, 101, 83, 0.3); }
.b-blocked    { color: var(--warning); background: rgba(217, 150, 62, 0.12);    border-color: rgba(217, 150, 62, 0.3); }
.b-rejected   { color: var(--text-3);  background: rgba(135, 134, 127, 0.12);   border-color: rgba(135, 134, 127, 0.3); }
.rm-attempt { margin-left: 4px; font-size: var(--fs-micro); color: var(--text-3); font-family: var(--font-mono, monospace); }

.rm-prio { font-size: var(--fs-micro); font-weight: 600; }
.p-high   { color: var(--danger); }
.p-medium { color: var(--text-3); }
.p-low    { color: var(--text-2); }
.rm-meta-chip {
  font-size: var(--fs-micro);
  color: var(--text-3);
  border: 1px solid var(--border-weak);
  border-radius: 4px;
  padding: 1px 6px;
}
.rm-meta-chip.ai { color: var(--primary); border-color: rgba(217, 119, 87, 0.3); }

.rm-table-empty {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  color: var(--text-3);
  padding: 36px 0;
}
.rm-empty-logo { font-size: 34px; }
.rm-empty-title { font-size: var(--fs-title); color: var(--text-1); }
.rm-empty-desc { font-size: var(--fs-caption); max-width: 380px; text-align: center; line-height: 1.7; }

/* 批量操作栏（表格上方：全选/清空/批量删除 · 并行/依次执行） */
.rm-batchbar {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--gap);
  padding: 8px var(--gap);
  border-bottom: 1px solid var(--border-weak);
  background: var(--bg-page);
  flex-wrap: wrap;
}
.rm-batchbar-left, .rm-batchbar-right { display: flex; align-items: center; gap: var(--gap-sm); flex-wrap: wrap; }
.rm-sel-count { font-size: var(--fs-caption); color: var(--text-2); }
.rm-sel-count b { color: var(--primary); font-family: var(--font-mono, monospace); }
.rm-row-acts { display: flex; gap: 4px; flex-wrap: wrap; }
.rm-act {
  padding: 2px 7px;
  font-size: var(--fs-micro);
  color: var(--text-2);
  background: none;
  border: 1px solid var(--border-weak);
  border-radius: 4px;
  cursor: pointer;
  transition: background var(--transition), color var(--transition);
  white-space: nowrap;
}
.rm-act:hover { background: var(--bg-hover); color: var(--text-1); }
.rm-act.danger { color: var(--danger); border-color: rgba(224, 101, 83, 0.3); }
.rm-act.danger:hover { background: rgba(224, 101, 83, 0.1); }

/* ── 右侧：详情 + 操作 ─────────────────────── */
.rm-side {
  width: 300px;
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  gap: var(--gap);
  overflow-y: auto;
  min-height: 0;
}
.rm-detail { flex-shrink: 0; }
.rm-detail-panel {
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  padding: var(--gap);
  display: flex;
  flex-direction: column;
  max-height: 42vh;
  overflow-y: auto;
}
.rm-detail-inner { display: flex; flex-direction: column; gap: var(--gap-sm); }
.rm-detail-empty {
  color: var(--text-3);
  font-size: var(--fs-caption);
  text-align: center;
  padding: 20px 0;
}
.rm-detail-head { display: flex; align-items: center; gap: var(--gap-sm); }
.rm-detail-title { font-size: var(--fs-body); font-weight: 600; color: var(--text-1); line-height: 1.5; flex: 1; }
.rm-detail-meta { display: flex; align-items: center; gap: 6px; font-size: var(--fs-micro); color: var(--text-3); flex-wrap: wrap; }
.rm-detail-block { margin-top: 2px; }
.rm-detail-label { font-size: var(--fs-micro); color: var(--text-3); margin-bottom: 2px; }
.rm-detail-text {
  font-size: var(--fs-caption);
  color: var(--text-2);
  line-height: 1.65;
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 160px;
  overflow-y: auto;
}
.rm-detail-text.review { color: var(--primary); }
.rm-detail-text.ok { color: var(--success); }
.rm-detail-text.err { color: var(--danger); }
.rm-detail-text.run { color: var(--text-3); font-family: var(--font-mono, monospace); font-size: var(--fs-micro); }
.rm-runlog-hist {
  display: flex; align-items: baseline; gap: 8px;
  font-size: var(--fs-micro); color: var(--text-3); line-height: 1.6;
}
.rm-runlog-outcome { flex-shrink: 0; }
.rm-runlog-outcome.done { color: var(--success); }
.rm-runlog-outcome.failed { color: var(--danger); }
.rm-runlog-outcome.blocked { color: var(--warning); }
.rm-runlog-dur { flex-shrink: 0; color: var(--text-3); font-family: var(--font-mono, monospace); }
.rm-runlog-sum { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.rm-detail-foot { display: flex; gap: var(--gap-sm); flex-wrap: wrap; margin-top: 4px; }

.rm-panel {
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  padding: var(--gap);
  display: flex;
  flex-direction: column;
  gap: var(--gap-sm);
  flex-shrink: 0;
}
.rm-panel-head { display: flex; align-items: center; justify-content: space-between; cursor: pointer; user-select: none; }
.rm-panel-title { font-size: var(--fs-caption); font-weight: 600; color: var(--text-1); }
.rm-panel-body { display: flex; flex-direction: column; gap: var(--gap-sm); }
.rm-panel-hint { font-size: var(--fs-micro); color: var(--text-3); line-height: 1.5; }
.rm-arrow { color: var(--text-3); flex-shrink: 0; font-size: var(--fs-micro); }
	.rm-auto-row { display: flex; align-items: center; justify-content: space-between; gap: var(--gap-sm); }
	.rm-auto-cfg { display: flex; flex-direction: column; gap: 6px; }
	.rm-mini-row { display: flex; align-items: center; justify-content: space-between; gap: var(--gap-sm); }
	.rm-mini-label { font-size: var(--fs-micro); color: var(--text-3); flex-shrink: 0; }
	.rm-check-row { display: flex; align-items: center; gap: var(--gap-sm); }
	.rm-mini-row + .rm-check-row { margin-top: 2px; }
	/* 独立验证命令（竖排：标签在上，textarea 在下占满宽度） */
	.rm-verify-row { display: flex; flex-direction: column; gap: 4px; margin-top: 2px; }
	.rm-auto-status {
  display: flex; align-items: center; gap: 6px;
  font-size: var(--fs-micro); color: var(--primary);
  padding: 6px 8px; background: var(--primary-tint); border-radius: var(--radius-sm);
}
</style>
