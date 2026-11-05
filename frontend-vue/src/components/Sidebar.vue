<script setup>
// 左侧栏：项目切换 + 会话列表 + 状态条
// 2026-10 体验升级（用户需求：会话列表仅选中时显示）：
//   - RailNav 新增「会话」开关，默认收起（v-show 逻辑在 App.vue class 绑定）；
//   - 选中/新建/发送后自动收起（autoCollapseSidebar），列表仅唤起时占位；
//   - 会话卡视觉升级：相对时间、运行呼吸灯、删除/重命名悬停显隐、搜索过滤。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from "vue";
	import { Message, Modal } from "@arco-design/web-vue";
	import { IconPlus, IconFolder, IconDelete, IconLaunch, IconCode, IconCheck, IconSearch, IconRight, IconPushpin, IconStop, IconEraser, IconBulb, IconEmpty } from "@arco-design/web-vue/es/icon";
	import { store, openSession, newSession, stopAll, loadProjects, autoCollapseSidebar, renameSession, setSessionWorkspace, pinSession, manualPreviewSession, clearAllSessions, summarizeClearSessions } from "../stores/app.js";
import { relTime, dateGroup, clipSummary } from "../utils/format.js";
import RunningTasks from "./RunningTasks.vue";
import { bridgeCall, onEvent } from "../bridge/wails.js";
import { IconDown as IconDownDoc } from "@arco-design/web-vue/es/icon";

defineEmits(["open-settings"]);

const projMenuVisible = ref(false);
const projects = computed(() => store.projects);
const curProj = computed(() => projects.value.find((p) => p.current) || { path: "选择项目" });

// ── 会话搜索过滤（列表长时快速定位）──
// 工作区过滤（用户需求 2026-11）：切换工作区后，列表仅显示当前工作区的会话。
//   - 绑定工作区 = 当前工作区 → 显示；路径比较经规范化（分隔符统一/小写/去尾分隔符）。
//   - 未绑定工作区（空 = 跟随全局，如未跑过任务的空会话/旧数据）→ 显示。
//   - 子会话（parentId 非空）豁免：跟随父会话显隐（sessionTree 仅从父行展开子行），
//     避免旧数据「父显示/子被过滤」导致折叠树断裂。
const searchKey = ref("");
function normWs(p) {
  return (p || "").replace(/\\/g, "/").replace(/\/+$/, "").toLowerCase();
}
const filteredSessions = computed(() => {
  const ws = normWs(store.workspace);
  const inWs = store.sessions.filter((s) => s.parentId || !ws || !s.workspace || normWs(s.workspace) === ws);
  const q = searchKey.value.trim().toLowerCase();
  if (!q) return inWs;
  return inWs.filter((s) => (s.title || "").toLowerCase().includes(q) || (s.workspace || "").toLowerCase().includes(q));
});
// 空态归因：无搜索关键词但被工作区过滤清空 → 提示「当前工作区暂无会话」（防「会话丢了」误解）。
const workspaceFilteredEmpty = computed(() => !searchKey.value && store.sessions.length > 0 && !filteredSessions.value.length);

// ── 打开当前项目（文件夹/Zed）：按钮弹菜单二选一，选择即执行并记住为默认方式 ──
// 后端 OpenProjectInEditor("folder"|"zed") 执行打开；SetEditorPreference 持久化偏好。
const openProjVisible = ref(false);
const openWith = ref("folder"); // 当前默认打开方式（启动时从 GetConfig 读入）
onMounted(async () => {
  try {
    const cfg = await bridgeCall("GetConfig");
    if (cfg && cfg.editorPreference) openWith.value = cfg.editorPreference;
  } catch { /* 浏览器预览/读失败保持默认 folder */ }
});
async function openProjectWith(mode) {
  openProjVisible.value = false;
  try {
    await bridgeCall("OpenProjectInEditor", mode);
    openWith.value = mode;
    bridgeCall("SetEditorPreference", mode).catch(() => {}); // 记住偏好，失败不阻断
  } catch (e) { Message.error(String(e)); }
}

// 打开目录：调 OpenProject（选完目录后端直接切换为当前项目并写入最近
// 列表）。旧实现只调 PickWorkspace 丢弃返回值 → 切换不生效、下拉无项。
async function pickProject() {
  try {
    await bridgeCall("OpenProject");
    await loadProjects();
  } catch (e) { Message.error(String(e)); }
}

async function switchProject(p) {
  try {
    await bridgeCall("SwitchProject", p.path);
    projMenuVisible.value = false;
    await loadProjects();
  } catch (e) { Message.error(String(e)); }
}

async function removeSession(e, id) {
  e.stopPropagation();
  try {
    await bridgeCall("DeleteSession", id);
    store.sessions = store.sessions.filter((s) => s.id !== id);
  } catch (err) { Message.error(String(err)); }
}

// ── 会话卡右键菜单 → 重命名（手动命名；改名后自动命名不再覆盖）──
const ctxMenu = ref({ visible: false, x: 0, y: 0, id: "" });
const renamingId = ref("");
const renameValue = ref("");
const renameInput = ref(null);

function onSessContext(e, id) {
  e.preventDefault();
  ctxMenu.value = {
    visible: true,
    x: Math.min(e.clientX, window.innerWidth - 150),
    y: Math.min(e.clientY, window.innerHeight - 80),
    id,
  };
  document.addEventListener("click", closeCtxMenu, { once: true, capture: true });
  document.addEventListener("contextmenu", closeCtxOnOther, { once: true, capture: true });
}

function closeCtxOnOther(e) {
  // 点在菜单本身上时不关闭（@click.prevent 已阻止默认行为）
  if (e.target.closest && e.target.closest(".sess-ctx-menu")) {
    document.addEventListener("click", closeCtxMenu, { once: true, capture: true });
    return;
  }
  closeCtxMenu();
}

function closeCtxMenu() {
  ctxMenu.value.visible = false;
  document.removeEventListener("click", closeCtxMenu, true);
  document.removeEventListener("contextmenu", closeCtxOnOther, true);
}

function startRename(e) {
  const id = ctxMenu.value.id;
  closeCtxMenu();
  const s = store.sessions.find((x) => x.id === id);
  if (!s) return;
  renamingId.value = id;
  renameValue.value = s.title || "";
  nextTick(() => {
    const el = document.getElementById("rename-input-" + id);
    if (el) { el.focus(); el.select(); }
  });
}

async function commitRename() {
  const id = renamingId.value;
  if (!id) return;
  const t = renameValue.value.trim();
  renamingId.value = "";
  if (!t) return; // 空名 = 取消
  const s = store.sessions.find((x) => x.id === id);
  if (s && s.title === t) return; // 未变化不提交
  const ok = await renameSession(id, t);
  if (ok) Message.success("已重命名");
}

function cancelRename() {
  renamingId.value = "";
}

function onRenameKeydown(e) {
  if (e.key === "Enter") { e.preventDefault(); commitRename(); }
  else if (e.key === "Escape") { e.preventDefault(); cancelRename(); }
}

	// 窄窗（<1100px）左侧栏为浮层抽屉：选中会话后自动退场，让出中间区
			function onSessClick(id) {
			  if (renamingId.value === id) return; // 编辑中不触发打开
			  openSession(id);
			  autoCollapseSidebar();
			}

			// 新对话虚拟卡点击：清空当前会话（回到未命名新会话态）
			function onNewCardClick() {
			  newSession();
			  autoCollapseSidebar();
			}

			// ── 会话独立工作区：右键菜单「设置工作区…」→ 选目录 → 写入会话 ──
			async function pickSessionWorkspace(id) {
			  closeCtxMenu();
			  try {
			    const dir = await bridgeCall("PickWorkspace");
			    if (!dir) return; // 用户取消
			    await setSessionWorkspace(id, dir);
			  } catch (e) { Message.error(String(e)); }
			}

			// 置顶/取消置顶（会话卡右键菜单；规划板 2026-10）
			async function togglePin(id) {
			  closeCtxMenu();
			  const s = store.sessions.find((x) => x.id === id);
			  await pinSession(id, !isPinned(s));
			}

			// 设置摘要（会话卡右键菜单）：非空 = 手动摘要覆盖自动提取；空 = 恢复。
			// （后端 SetSessionManualPreview；ManualPreview 随任务注入提示词）。
			async function setPreview(id) {
			  closeCtxMenu();
			  const s = store.sessions.find((x) => x.id === id);
			  if (!s) return;
			  const cur = (s.lastMessage || "").trim();
			  const v = window.prompt("设置会话卡摘要（留空恢复自动摘要）", cur);
			  if (v === null) return; // 用户取消
			  await manualPreviewSession(id, v.trim());
			}

// ── 会话卡片信息升级（规划板 2026-10）：日期分组 + 摘要/错误态/置顶 ──
// 会话树仍保持「父→子」层级展开语义；分组仅在顶层插入标题行（子会话
// 跟随父行所在分组，不单独成组），分组键取自各会话 updatedAt。
// 渲染模型：{ type:'header', key, name } | { type:'session', ...sess }
// 稳定 key（type+id / type+分组键）保证 v-for 复用 DOM，滚动无抖动。
const groupedTree = computed(() => {
  const out = [];
  let cur = "";
  for (const s of sessionTree.value) {
    if (s.isChild) { out.push({ type: "session", ...s }); continue; }
    const g = dateGroup(s.updatedAt);
    if (g.key && g.key !== cur) {
      cur = g.key;
      out.push({ type: "header", key: "h-" + g.key, name: g.name });
    }
    out.push({ type: "session", ...s });
  }
  return out;
});

// 卡片预览行：lastMessage 优先，退化为工作区徽标提示（旧数据无摘要时）。
function sessPreview(s) {
  return clipSummary(s.lastMessage, 120);
}

// 置顶态：pinnedAt 非零即置顶（后端毫秒时间戳）。
function isPinned(s) {
  return !!(s && s.pinnedAt);
}

// 会话卡角标文案：D:/code/gocoder → gocoder（短目录名）
function wsBadge(s) {
		  const ws = s && s.workspace;
		  if (!ws) return "";
		  return "· " + ws.replace(/[\\/]+$/, "").split(/[\\/]/).pop();
		}

			// 右键菜单上下文：目标会话的工作区与运行态（菜单项显隐用）
			const ctxWorkspace = computed(() => {
			  const s = store.sessions.find((x) => x.id === ctxMenu.value.id);
			  return (s && s.workspace) || "";
			});
			const ctxRunning = computed(() => !!store.running[ctxMenu.value.id]);
			const ctxPinned = computed(() => {
			  const s = store.sessions.find((x) => x.id === ctxMenu.value.id);
			  return isPinned(s);
			});

		// 清除会话工作区 → 恢复「跟随全局」：后端置空会话绑定，下轮任务
		// runTask 自动补记当时的全局工作区。
		async function clearSessionWorkspace(id) {
		  closeCtxMenu();
		  try {
		    await bridgeCall("SetSessionWorkspace", id, "");
		    const s = store.sessions.find((x) => x.id === id);
		    if (s) s.workspace = "";
		  } catch (e) { Message.error(String(e)); }
		}

		onBeforeUnmount(closeCtxMenu);

const runningIds = computed(() => new Set(
  Object.entries(store.running).filter(([, v]) => v).map(([k]) => k),
));
const anyRunning = computed(() => runningIds.value.size > 0);

// ── PC 端会话列表「清空 / 总结并清空」（用户需求 2026-11，对齐移动端）──
// 清空：确认后同步执行（后端秒级删除全部本地会话；当前会话被清空回新建态）。
// 总结并清空：确认后后端异步执行（LLM 汇总未完成工作 → 清空 → 新建续作会话
// 并自动运行），完成经 kaelvoris:cleanup-done 通知复位忙碌态；按钮忙态防重复点击。
const clearBusy = ref(false);
function onClearSessions() {
  if (clearBusy.value || store.cleanupBusy) return;
  if (!store.sessions.length) { Message.info("暂无可清空的会话"); return; }
  Modal.confirm({
    title: "清空会话",
    content: `将删除全部 ${store.sessions.length} 个会话（运行中会先停止，不可恢复）。已连接远程服务时，移动端记录将同步清空。`,
    okButtonProps: { status: "danger" },
    onOk: async () => {
      clearBusy.value = true;
      try { await clearAllSessions(); } finally { clearBusy.value = false; }
    },
  });
}
function onSummarizeClear() {
  if (clearBusy.value || store.cleanupBusy) return;
  if (!store.sessions.length) { Message.info("暂无可总结的会话"); return; }
  Modal.confirm({
    title: "总结并清空",
    content: `PC 端将汇总全部未完成工作为一条续作任务并自动运行，随后清空全部 ${store.sessions.length} 个会话（不可恢复）。`,
    okButtonProps: { status: "danger" },
    onOk: () => {
      store.cleanupBusy = true;
      summarizeClearSessions();
    },
  });
}

// ── 会话折叠树（用户需求 2026-10：并行开发需有主任务，子任务挂主任务下面）──
// 后端语义：主任务会话（roadmap_batch_<ts>）/ new_task 委派子会话（<父ID>-s<N>）
// 以 parentId 字段标记层级（旧存档由后端 deriveParentID 按 ID 形态兑底推导）。
// 渲染：父会话行可点展开/收起；子会话缩进展示，父收起时隐藏。
const expanded = ref(new Set()); // 已展开的父会话 ID
function toggleExpand(id) {
  const next = new Set(expanded.value);
  next.has(id) ? next.delete(id) : next.add(id);
  expanded.value = next;
}
// 顶层会话（无父）+ 其子会话（按 updatedAt 旧→新，开发批次清单阅读顺序）。
// 默认展开含子会话的父行（2026-11 修复：子任务会话在列表中直接可见，
// 不再依赖“子任务在跑”这一时点条件）；仅自动展开一次，用户手动收起后
// 不强行拉回。
const autoExpandedOnce = ref(new Set());
const sessionTree = computed(() => {
  const list = filteredSessions.value;
  const roots = list.filter((s) => !s.parentId);
  const out = [];
  for (const r of roots) {
    const kids = list.filter((s) => s.parentId === r.id)
      .sort((a, b) => new Date(a.updatedAt) - new Date(b.updatedAt));
    // 有子会话且从未手动展开过 → 自动展开（收起状态尊重用户手动操作）。
    if (kids.length && !autoExpandedOnce.value.has(r.id)) {
      const next = new Set(expanded.value);
      next.add(r.id);
      expanded.value = next;
      autoExpandedOnce.value = new Set([...autoExpandedOnce.value, r.id]);
    }
    out.push({ ...r, isChild: false, kidCount: kids.length });
    if (expanded.value.has(r.id)) {
      for (const k of kids) out.push({ ...k, isChild: true });
    }
  }
  return out;
});
// 子会话运行态汇总：父会话行右侧显示「N 个子任务运行中」徽标。
function childRunningCount(id) {
  return store.sessions.filter((s) => s.parentId === id && (s.running || store.running[s.id])).length;
}
function childCount(id) {
  return store.sessions.filter((s) => s.parentId === id).length;
}

// ── 侧栏文档区（2026-12 用户需求：文档入口放会话列表下方）──
// 只读快捷导航：展示当前项目 doc/ 文件树，点击文件切 docs 主视图打开
//（编辑/新建/删除等完整操作仍在 DocPanel）。树数据随项目切换刷新。
import { IconFolder as IconFolderDoc, IconFile as IconFileDoc } from "@arco-design/web-vue/es/icon";

const docsInfo = ref(null);        // GetDocInfo（exists/fileCount，用于空态文案）
const docsTree = ref([]);          // ListDocTree 嵌套树
const docsExpanded = ref(new Set()); // 已展开目录 path 集合
const docsOpen = ref(false);       // 折叠面板展开态（默认收起，不占侧栏空间）
const docActivePath = ref("");     // 当前在看的文件（树高亮；DocPanel 删除/重命名后由刷新兑底）

async function loadDocsTree() {
  try {
    const [i, t] = await Promise.all([
      bridgeCall("GetDocInfo").catch(() => null),
      bridgeCall("ListDocTree").catch(() => []),
    ]);
    docsInfo.value = i;
    docsTree.value = t || [];
  } catch { docsTree.value = []; }
}

// 扁平化（同 DocPanel.flatTree：展开目录下钻 + 缩进深度）
const docsFlat = computed(() => {
  const out = [];
  const walk = (nodes, depth) => {
    for (const n of nodes || []) {
      out.push({ node: n, depth });
      if (n.isDir && docsExpanded.value.has(n.path) && n.children?.length) walk(n.children, depth + 1);
    }
  };
  walk(docsTree.value, 0);
  return out;
});

function toggleDocsDir(node) {
  const next = new Set(docsExpanded.value);
  next.has(node.path) ? next.delete(node.path) : next.add(node.path);
  docsExpanded.value = next;
}

// 点文件：切 docs 主视图 + 一次性打开请求（DocPanel watch store.docOpenPath）
function openDocFile(node) {
  docActivePath.value = node.path;
  store.docOpenPath = node.path;
  store.activeMain = "docs";
}

// 项目切换（workspace 变化）后重载树；doc_changed 广播后防抖刷新（AI 改文档同步）
watch(() => store.workspace, loadDocsTree, { immediate: true });
let docChgTimer = 0;
const onDocsChanged = () => {
  clearTimeout(docChgTimer);
  docChgTimer = setTimeout(loadDocsTree, 500);
};
onMounted(() => onEvent("kaelvoris:doc_changed", onDocsChanged));
onBeforeUnmount(() => clearTimeout(docChgTimer));
</script>

<template>
  <aside class="sidebar">
    <div class="sidebar-head">
      <a-dropdown trigger="click" position="bl">
        <button class="proj-btn" :title="curProj.path">
          <IconFolder style="color: var(--text-2)" />
          <span class="proj-name">{{ curProj.path }}</span>
        </button>
        <template #content>
          <div class="proj-menu">
            <div class="proj-menu-item" @click="pickProject">打开目录…</div>
            <div class="proj-menu-sep"></div>
            <div
              v-for="p in projects"
              :key="p.path"
              class="proj-menu-item"
              :class="{ current: p.current }"
              @click="switchProject(p)"
            >{{ p.path }}</div>
          </div>
        </template>
      </a-dropdown>
      <a-dropdown trigger="click" position="br" v-model:popup-visible="openProjVisible">
        <button class="icon-btn" title="用文件夹 / Zed 打开当前项目">
          <IconLaunch />
        </button>
        <template #content>
          <div class="proj-menu open-proj-menu">
            <div
              class="proj-menu-item open-proj-item"
              :class="{ current: openWith === 'folder' }"
              @click="openProjectWith('folder')"
            >
              <IconFolder /> 文件夹窗口打开
              <IconCheck v-if="openWith === 'folder'" class="open-proj-check" />
            </div>
            <div
              class="proj-menu-item open-proj-item"
              :class="{ current: openWith === 'zed' }"
              @click="openProjectWith('zed')"
            >
              <IconCode /> 用 Zed 打开
              <IconCheck v-if="openWith === 'zed'" class="open-proj-check" />
            </div>
          </div>
        </template>
      </a-dropdown>
      <a-tooltip content="新建会话">
        <button class="icon-btn" @click="newSession"><IconPlus /></button>
      </a-tooltip>
    </div>

    <!-- 会话搜索（列表长时快速过滤） -->
    <div class="sidebar-search">
      <IconSearch class="sidebar-search-icon" />
      <input v-model="searchKey" class="sidebar-search-input" placeholder="搜索会话…" />
      <button v-if="searchKey" class="sidebar-search-clear" @click="searchKey = ''">×</button>
    </div>

    <div class="sidebar-scroll">
      <!-- 骨架占位：首次列表拉取中（会话数未知，先给 4 行占位防白屏抖动） -->
      <div v-if="store.sessionsLoading" class="sess-skeleton" aria-hidden="true">
        <div v-for="i in 4" :key="i" class="sess-skeleton-card">
          <div class="sk-line sk-w-60"></div>
          <div class="sk-line sk-w-90"></div>
          <div class="sk-line sk-w-30"></div>
        </div>
      </div>
      <!-- 空态：图标 + 引导文案（搜索无结果与列表无会话分开提示） -->
      <div v-else-if="!filteredSessions.length" class="sidebar-empty">
        <IconEmpty class="sidebar-empty-icon" />
        <div class="sidebar-empty-title">{{ searchKey ? "无匹配会话" : (workspaceFilteredEmpty ? "当前工作区暂无会话" : "暂无会话") }}</div>
        <div class="sidebar-empty-sub">{{ searchKey ? "换个关键词试试" : (workspaceFilteredEmpty ? "列表仅显示当前工作区的会话" : "点击上方 + 新建一个任务会话") }}</div>
      </div>
      <!-- 新对话虚拟卡：新建会话后高亮（2026-10 修复：点新增无反馈时给可见落点） -->
      <div class="sess-item new-card" :class="{ active: !store.currentId }" @click="onNewCardClick">
        <div class="sess-title">
          <span class="sess-title-text">新对话</span>
        </div>
        <div class="sess-meta">描述任务即可开始</div>
      </div>
      <!-- 日期分组标题（今天/昨天/7天内/更早；规划板 2026-10 会话卡片信息升级） -->
      <template v-for="row in groupedTree" :key="row.type === 'header' ? row.key : row.id">
        <div v-if="row.type === 'header'" class="sess-group-header">{{ row.name }}</div>
        <div
          v-else
          class="sess-item"
	          :class="{ active: row.id === store.currentId, child: row.isChild, pinned: isPinned(row), error: row.hasError, done: row.done }"
          @click="onSessClick(row.id)"
          @contextmenu="onSessContext($event, row.id)"
        >
        <div class="sess-title">
          <!-- 父会话（含并行子任务）：展开/收起箭头（用户需求 2026-10 会话树） -->
          <button
            v-if="childCount(row.id)"
            class="sess-expand"
            type="button"
            :title="expanded.has(row.id) ? '收起子任务' : '展开子任务（' + childCount(row.id) + '）'"
            @click.stop="toggleExpand(row.id)"
          >
            <IconRight class="sess-expand-ico" :class="{ 'is-open': expanded.has(row.id) }" style="width: 12px; height: 12px" />
          </button>
          <span class="sess-indent" v-else-if="row.isChild"></span>
          <span v-if="row.running || store.running[row.id]" class="sess-run-dot" title="任务执行中"></span>
          <span v-else-if="row.hasError" class="sess-run-dot sess-error-dot" title="最近一次任务出错"></span>
          <input
            v-if="renamingId === row.id"
            :id="'rename-input-' + row.id"
            v-model="renameValue"
            class="rename-input"
            maxlength="80"
            @keydown="onRenameKeydown"
            @blur="commitRename"
            @click.stop
          />
          <span v-else class="sess-title-text">{{ row.title || "未命名会话" }}</span>
          <span v-if="row.hasSummary" class="sess-summary-badge" title="已有会话总结">总</span>
          <IconPushpin v-if="isPinned(row)" class="sess-pin-icon" title="已置顶" />
          <span v-if="childCount(row.id) && !expanded.has(row.id)"
            class="sess-child-badge" :class="{ 'sess-child-count': !childRunningCount(row.id) }"
            :title="childRunningCount(row.id) ? childRunningCount(row.id) + ' 个子任务运行中' : childCount(row.id) + ' 个子任务'">{{ childRunningCount(row.id) ? childRunningCount(row.id) + ' 运行中' : childCount(row.id) + ' 个子任务' }}</span>
        </div>
        <!-- 最后一条消息摘要（单行截断；规划板 2026-10 会话卡片信息升级） -->
        <div v-if="sessPreview(row)" class="sess-preview" :title="row.lastMessage">{{ sessPreview(row) }}</div>
        <div class="sess-meta">
          <!-- 信息层级：时间最弱置底；未读/标签为可选字段（后端 Summary 暂无 unread/tags，样式与条件渲染已就绪，字段出现即显示——不创建数据，仅支持展示） -->
          <span v-if="row.unreadCount > 0" class="sess-unread-badge" title="未读消息">{{ row.unreadCount > 99 ? '99+' : row.unreadCount }}</span>
          <span v-if="row.tags && row.tags.length" class="sess-cat-chips">
            <span v-for="tg in row.tags" :key="tg" class="sess-cat-chip">{{ tg }}</span>
          </span>
          <span :title="row.updatedAt">{{ relTime(row.updatedAt) }}</span>
          <span v-if="row.messageCount">{{ row.messageCount }} 条</span>
          <span v-if="row.workspace" class="sess-ws-badge" :title="row.workspace">{{ wsBadge(row) }}</span>
          <span class="flex-sp"></span>
          <a-button size="mini" type="text" class="sess-del-btn sess-pin-btn"
            :title="isPinned(row) ? '取消置顶' : '置顶'"
            @click.stop="togglePin(row.id)">
            <template #icon><IconPushpin :class="{ filled: isPinned(row) }" /></template>
          </a-button>
          <a-button size="mini" type="text" status="danger" class="sess-del-btn"
            @click.stop="removeSession($event, row.id)">
            <template #icon><IconDelete /></template>
          </a-button>
        </div>
        </div>
      </template>

      <!-- 会话卡右键菜单（重命名 / 删除） -->
      <teleport to="body">
        <div
          v-if="ctxMenu.visible"
          class="sess-ctx-menu"
          :style="{ left: ctxMenu.x + 'px', top: ctxMenu.y + 'px' }"
          @contextmenu.prevent
        >
          <div class="sess-ctx-item" @click.stop="startRename($event)">重命名</div>
          <div class="sess-ctx-item" @click.stop="togglePin(ctxMenu.id)">{{ ctxPinned ? '取消置顶' : '置顶' }}</div>
          <div class="sess-ctx-item" @click.stop="setPreview(ctxMenu.id)">设置摘要…</div>
          <div class="sess-ctx-item" @click.stop="pickSessionWorkspace(ctxMenu.id)">设置工作区…</div>
          <div v-if="ctxWorkspace && !ctxRunning" class="sess-ctx-item danger" @click.stop="clearSessionWorkspace(ctxMenu.id)">清除（跟随全局）</div>
          <div v-if="ctxRunning" class="sess-ctx-hint">任务运行中，暂不可改</div>
        </div>
      </teleport>
    </div>

    <!-- 文档快捷区（2026-12 用户需求：文档入口放会话列表下方）：
         折叠面板，展开后为只读文件树；点文件切 docs 主视图打开（完整编辑在 DocPanel） -->
    <div class="docs-sec">
      <button class="docs-sec-head" type="button" @click="docsOpen = !docsOpen">
        <IconDownDoc class="docs-sec-caret" :class="{ open: docsOpen }" />
        <IconFolderDoc class="docs-sec-ico" />
        <span class="docs-sec-title">文档</span>
        <span v-if="docsInfo && docsInfo.fileCount" class="docs-sec-count">{{ docsInfo.fileCount }}</span>
      </button>
      <div v-show="docsOpen" class="docs-sec-body">
        <template v-if="docsFlat.length">
          <div
            v-for="row in docsFlat"
            :key="row.node.path"
            class="docs-item"
            :class="{ dir: row.node.isDir, active: !row.node.isDir && row.node.path === docActivePath }"
            :style="{ paddingLeft: 8 + row.depth * 12 + 'px' }"
            :title="row.node.path"
            @click="row.node.isDir ? toggleDocsDir(row.node) : openDocFile(row.node)"
          >
            <IconDownDoc v-if="row.node.isDir && docsExpanded.has(row.node.path)" class="docs-item-caret" />
            <IconRight v-else-if="row.node.isDir" class="docs-item-caret" />
            <IconFolderDoc v-if="row.node.isDir" class="docs-item-ico" />
            <IconFileDoc v-else class="docs-item-ico" />
            <span class="docs-item-name">{{ row.node.displayName || row.node.name }}</span>
          </div>
        </template>
        <div v-else class="docs-sec-empty">{{ docsInfo && !docsInfo.exists ? "doc 目录不存在" : "doc 目录为空" }}</div>
      </div>
    </div>

    <div class="sidebar-foot">
      <!-- 运行徽标（用户需求：点击弹出运行任务列表，切换工作区并定位会话）；无运行任务时组件自隐藏 -->
      <RunningTasks v-if="anyRunning" />
      <span v-else class="status-text">空闲</span>
      <span class="spacer"></span>
      <a-tooltip content="全部停止">
        <a-button v-if="anyRunning" size="mini" type="text" class="foot-icon-btn foot-danger" @click="stopAll">
          <template #icon><IconStop /></template>
        </a-button>
      </a-tooltip>
      <a-tooltip content="清空全部会话">
        <a-button size="mini" type="text" class="foot-icon-btn foot-danger" :disabled="clearBusy || store.cleanupBusy" @click="onClearSessions">
          <template #icon><IconEraser /></template>
        </a-button>
      </a-tooltip>
      <a-tooltip content="总结未完成工作并清空（自动新建续作会话运行）">
        <a-button size="mini" type="text" class="foot-icon-btn foot-warning" :disabled="clearBusy || store.cleanupBusy" @click="onSummarizeClear">
          <template #icon><IconBulb /></template>
        </a-button>
      </a-tooltip>
    </div>
  </aside>
</template>

<style scoped>
.proj-btn {
  flex: 1;
  display: flex;
  align-items: center;
  gap: 6px;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  padding: 6px 10px;
  cursor: pointer;
  color: var(--text-1);
  min-width: 0;
  transition: border-color var(--transition);
}
.proj-btn:hover { border-color: var(--border); }
.proj-name {
  font-size: var(--fs-caption);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.proj-menu {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 4px;
  min-width: 240px;
  max-width: 360px;
  box-shadow: var(--shadow-overlay);
}
.proj-menu-item {
  padding: 6px 10px;
  font-size: var(--fs-caption);
  color: var(--text-1);
  border-radius: 4px;
  cursor: pointer;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.proj-menu-item:hover { background: var(--bg-hover); }
.proj-menu-item.current { color: var(--primary); }
.proj-menu-sep {
  height: 1px;
  background: var(--border-weak);
  margin: 4px 0;
}
/* 打开项目菜单项：图标 + 文案 + 当前默认勾选 */
.open-proj-menu { min-width: 160px; }
.open-proj-item {
  display: flex;
  align-items: center;
  gap: 6px;
}
.open-proj-check { margin-left: auto; font-size: 12px; }
.flex-sp { flex: 1; }
	.sess-del-btn { opacity: 0; transition: opacity var(--transition); }
	.sess-item:hover .sess-del-btn { opacity: 0.7; }
	/* 新对话虚拟卡：虚线边框区分实体会话，active 时高亮主色 */
	.sess-item.new-card {
	  border: 1px dashed var(--border);
	  margin-bottom: 6px;
	}
	.sess-item.new-card .sess-title-text { color: var(--text-2); }
	.sess-item.new-card.active .sess-title-text { color: var(--primary); }
	.sess-item.new-card:hover { border-color: var(--primary); }
	.sess-item.new-card.active {
	  box-shadow: inset 2px 0 0 var(--primary);
	  border-style: solid;
	  border-color: var(--primary);
	}
	.sess-title-text {
	  overflow: hidden;
	  text-overflow: ellipsis;
	  white-space: nowrap;
	  flex: 1;
	}
	/* 会话折叠树：子会话缩进 + 展开/收起箭头 + 子任务运行徽标 */
	.sess-item.child {
	  margin-left: 16px;
	  border-left: 2px solid var(--border-weak);
	}
	.sess-item.child.active { border-left-color: var(--primary); }
	.sess-expand {
	  display: inline-flex;
	  align-items: center;
	  justify-content: center;
	  width: 16px; height: 16px;
	  padding: 0;
	  border: none;
	  background: transparent;
	  color: var(--text-3);
	  cursor: pointer;
	  flex-shrink: 0;
	  border-radius: 3px;
	}
	.sess-expand:hover { color: var(--primary); background: var(--bg-hover); }
	.sess-expand:active { background: var(--bg-active); }
	/* 展开箭头：收起=朝右（默认），展开= rotate 90° 朝下，过渡顺滑（单图标旋转，避免 IconDown 切换跳动） */
	.sess-expand-ico { transition: transform var(--transition); }
	.sess-expand-ico.is-open { transform: rotate(90deg); }
	.sess-indent { width: 16px; flex-shrink: 0; }
	.sess-child-badge {
	  font-size: 10px;
	  color: var(--success);
	  flex-shrink: 0;
	  white-space: nowrap;
	}
	/* 收起且无运行中：子任务数量徽标用中性色（区别于绿色“运行中”） */
	.sess-child-badge.sess-child-count { color: var(--text-3); }
	.sess-run-dot {
	  width: 6px; height: 6px;
	  border-radius: 50%;
	  background: var(--success);
	  flex-shrink: 0;
	  animation: sess-breath 1.4s ease-in-out infinite;
	}
	@keyframes sess-breath { 0%, 100% { opacity: 1; } 50% { opacity: 0.35; } }

	/* ── 会话卡片信息升级（规划板 2026-10）─────────────────── */
	/* 日期分组标题：吸顶可辨（sticky 不占布局额外高度，滚动时分组名常驻） */
	.sess-group-header {
	  position: sticky;
	  top: 0;
	  z-index: 2;
	  background: var(--bg-page);
	  font-size: var(--fs-micro);
	  color: var(--text-3);
	  letter-spacing: 0.5px;
	  padding: 6px 2px 4px;
	  margin-top: 4px;
	  user-select: none;
	  border-bottom: 1px solid var(--border-weak);
	}
	/* 摘要预览行：单行 ellipsis + 固定行高（摘要有无/截断均不引起布局跳动） */
	.sess-preview {
	  font-size: var(--fs-micro);
	  color: var(--text-2);
	  line-height: 1.4;
	  height: 15px;
	  white-space: nowrap;
	  overflow: hidden;
	  text-overflow: ellipsis;
	}
/* 置顶态：左侧琥珀边条（对齐移动端 .tcard.pinned 语义）+ 标题行置顶钉 */
.sess-item.pinned { box-shadow: inset 3px 0 0 var(--warning); }
.sess-item.pinned.active { box-shadow: inset 3px 0 0 var(--primary); }

/* 会话卡 hover 微交互：卡片轻浮 + 删除/置顶按钮渐显节奏更顺 */
.sess-item:hover { transform: translateY(-1px); }
	.sess-summary-badge {
	  display: inline-flex; flex-shrink: 0; width: 16px; height: 16px; align-items: center; justify-content: center;
	  margin: 0 4px; font-size: 10px; line-height: 1; color: var(--primary);
	  background: var(--primary-tint); border: 1px solid var(--primary); border-radius: 4px;
	}
	.sess-pin-icon {
	  font-size: 12px;
	  color: var(--warning);
	  flex-shrink: 0;
	}
	.sess-pin-btn { color: var(--text-3); }
	/* 未读徽标：主色实心圆点+数字（零不渲染，超 99 显 99+） */
	.sess-unread-badge {
		display: inline-flex; align-items: center; justify-content: center;
		min-width: 16px; height: 16px; padding: 0 5px;
		border-radius: 999px;
		background: var(--primary-strong);
		color: var(--on-primary);
		font-size: var(--fs-micro); line-height: 1; font-weight: 600;
		flex-shrink: 0;
	}
	/* 标签 chips（可选字段，随后端数据出现而显示） */
	.sess-cat-chips { display: inline-flex; gap: 4px; flex-shrink: 0; overflow: hidden; }
	.sess-cat-chip {
		padding: 1px 7px;
		border-radius: 999px;
		font-size: var(--fs-micro); line-height: 1.6;
		color: var(--text-2);
		background: var(--bg-elevated);
		border: 1px solid var(--border-weak);
		white-space: nowrap;
	}
	.sess-pin-btn:hover { color: var(--warning); }
		/* 成功完成：标题显绿（最近一段任务正常完成；会话列表用户需求 2026-11） */
		.sess-item.done:not(.error) .sess-title-text { color: var(--success); }
			/* 错误态：标题前置红点（会话最近一段执行出错） */
			.sess-item.error .sess-title-text { color: var(--danger); }
			.sess-error-dot {
		  width: 6px; height: 6px;
	  border-radius: 50%;
	  background: var(--danger);
	  flex-shrink: 0;
	}
		/* 置顶按钮常驻可见（用户需求 2026-11「钉一下」：入目即可点，无需悬停发现）；
		已置顶/未置顶状态均可直接切换，删除按钮仍悬停显隐。 */
		.sess-pin-btn { opacity: 0.7; }
		.sess-item:hover .sess-pin-btn { opacity: 1; }
		.sess-item.pinned .sess-pin-btn { opacity: 1; color: var(--warning); }
.rename-input {
  flex: 1;
  min-width: 0;
  background: var(--bg-elevated);
  border: 1px solid var(--primary);
  border-radius: 4px;
  color: var(--text-1);
  font-size: var(--fs-caption);
  padding: 2px 6px;
  outline: none;
}
.sess-ctx-menu {
  position: fixed;
  z-index: 3000;
  min-width: 120px;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  box-shadow: var(--shadow-overlay);
  padding: 4px;
}
.sess-ctx-item {
  padding: 6px 12px;
  font-size: var(--fs-caption);
  color: var(--text-1);
  border-radius: 4px;
  cursor: pointer;
  white-space: nowrap;
}
.sess-ctx-item:hover { background: var(--bg-hover); }
.sess-ctx-item:active { background: var(--bg-active); }
	.sess-ctx-item.danger { color: var(--danger, #e5534b); }
	.sess-ctx-hint {
	  padding: 6px 12px;
	  font-size: var(--fs-caption);
	  color: var(--text-3);
	  white-space: nowrap;
	}
	.sess-ws-badge {
	  font-size: var(--fs-caption);
	  color: var(--text-3);
	  max-width: 88px;
	  overflow: hidden;
	  text-overflow: ellipsis;
	  white-space: nowrap;
	}
.sidebar-empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  color: var(--text-3);
  font-size: var(--fs-caption);
  text-align: center;
  margin-top: 32px;
}
.sidebar-empty-icon { font-size: 30px; color: var(--text-3); opacity: 0.7; }
.sidebar-empty-title { color: var(--text-2); font-size: var(--fs-body); font-weight: 500; }
.sidebar-empty-sub { color: var(--text-3); }
/* 骨架占位：左栏首次加载会话列表期间的低调脉冲占位卡（设计语言禁渐变，用 opacity 呼吸） */
.sess-skeleton { display: flex; flex-direction: column; gap: 8px; padding: 4px 0; }
.sess-skeleton-card {
  padding: 10px 12px;
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  background: var(--bg-card);
}
.sk-line {
  height: 9px;
  margin: 0 0 7px;
  border-radius: 4px;
  background: var(--bg-inset);
  animation: sk-pulse 1.6s ease-in-out infinite;
}
.sk-line:last-child { margin-bottom: 0; }
.sk-w-60 { width: 60%; }
.sk-w-90 { width: 90%; }
.sk-w-30 { width: 30%; }
@keyframes sk-pulse { 0%, 100% { opacity: 0.45; } 50% { opacity: 1; } }
	.status-text { font-size: var(--fs-caption); color: var(--text-2); }
.sidebar-foot .spacer { flex: 1; }
	/* 底部状态条：运行徽标（可点，弹出运行任务列表 RunningTasks 组件）+ 图标按钮 */
.foot-icon-btn { color: var(--text-3); margin-left: 2px; }
.foot-icon-btn:hover { color: var(--text-1); }
.foot-icon-btn.foot-danger:hover { color: var(--danger); }
.foot-icon-btn.foot-warning:hover { color: var(--warning); }

/* ── 会话搜索框 ───────────────────────────────────────────── */
.sidebar-search {
  display: flex;
  align-items: center;
  gap: 6px;
  margin: 0 10px 4px;
  padding: 4px 8px;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  transition: border-color var(--transition);
}
.sidebar-search:focus-within { border-color: var(--primary); box-shadow: 0 0 0 3px var(--primary-tint); }
.sidebar-search-icon { color: var(--text-3); font-size: 13px; flex-shrink: 0; }
.sidebar-search-input {
  flex: 1;
  min-width: 0;
  background: transparent;
  border: none;
  outline: none;
  color: var(--text-1);
  font-size: var(--fs-caption);
}
.sidebar-search-input::placeholder { color: var(--text-3); }
.sidebar-search-clear {
  border: none;
  background: none;
  color: var(--text-3);
  cursor: pointer;
  font-size: 12px;
  padding: 0 2px;
}
	.sidebar-search-clear:hover { color: var(--text-1); }
	.sidebar-search-clear:active { color: var(--primary); }

/* ── 文档快捷区（2026-12：会话列表下方）── */
.docs-sec {
  flex-shrink: 0;
  margin: 0 8px 6px;
  border-top: 1px solid var(--border-weak);
  padding-top: 6px;
}
.docs-sec-head {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  padding: 4px 6px;
  border: none;
  background: transparent;
  border-radius: var(--radius-sm);
  color: var(--text-2);
  cursor: pointer;
  font-size: var(--fs-caption);
  transition: background var(--transition), color var(--transition);
}
.docs-sec-head:hover { background: var(--bg-hover); color: var(--text-1); }
.docs-sec-caret { font-size: 10px; color: var(--text-3); transition: transform var(--transition); flex-shrink: 0; }
.docs-sec-caret.open { transform: rotate(0deg); }
.docs-sec-caret:not(.open) { transform: rotate(-90deg); }
.docs-sec-ico { font-size: 13px; color: var(--text-3); flex-shrink: 0; }
.docs-sec-title { font-weight: 600; letter-spacing: 0.3px; }
.docs-sec-count {
  margin-left: auto;
  font-size: var(--fs-micro);
  color: var(--text-3);
  font-variant-numeric: tabular-nums;
}
.docs-sec-body {
  margin-top: 2px;
  max-height: 180px;
  overflow-y: auto;
  padding: 0 2px;
}
.docs-item {
  display: flex;
  align-items: center;
  gap: 5px;
  padding: 4px 6px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  font-size: var(--fs-caption);
  color: var(--text-2);
  white-space: nowrap;
  overflow: hidden;
  transition: background var(--transition), color var(--transition);
}
.docs-item:hover { background: var(--bg-hover); color: var(--text-1); }
.docs-item.active { background: var(--bg-active); color: var(--primary); }
.docs-item-caret { font-size: 10px; color: var(--text-3); flex-shrink: 0; }
.docs-item-ico { font-size: 12px; color: var(--text-3); flex-shrink: 0; }
.docs-item-name { overflow: hidden; text-overflow: ellipsis; }
.docs-item.dir .docs-item-name { color: var(--text-1); font-weight: 500; }
.docs-sec-empty {
  padding: 8px 6px;
  font-size: var(--fs-micro);
  color: var(--text-3);
  text-align: center;
}
	</style>
