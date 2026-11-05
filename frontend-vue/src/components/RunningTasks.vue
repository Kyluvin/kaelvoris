<script setup>
// ═════════════════════════════════════════════════════════════════
// 运行任务徽标 + 弹出任务列表（用户需求：点击「N 运行中」徽标弹出运行中
// 任务列表；点列表项切换到该会话绑定的项目工作区并定位到该运行会话）。
//
// 用法：组件空态自隐藏（无运行任务不渲染徽标），可直接挂两处——
//   - Sidebar 底部状态条（替换原静态 run-badge span）；
//   - MainChat 顶栏（侧栏默认收起时仍可从主区顶栏进入）。
// 定位链路：SwitchProject 切工作区（会话绑定且与当前不同时）→ openSession
// （内部处理草稿/审批卡清零、历史回放、会话绑定工作区联动）。
// ═════════════════════════════════════════════════════════════════
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { store, openSession, loadProjects } from "../stores/app.js";
import { relTime } from "../utils/format.js";
import { bridgeCall } from "../bridge/wails.js";

const open = ref(false);
const badgeEl = ref(null);
const popPos = ref({ left: 0 });

// 运行中任务列表：以 store.running（kaelvoris:status/sessions/done 维护）为源，
// 会话摘要补标题/工作区；不在列表且非远程的残留 ID（删除会话竞态）过滤掉。
const runningList = computed(() => {
  const byId = new Map(store.sessions.map((s) => [s.id, s]));
  return Object.entries(store.running)
    .filter(([, v]) => v)
    .map(([id]) => id)
    .filter((id) => byId.has(id) || store.remoteTasks[id])
    .map((id) => {
      const s = byId.get(id) || {};
      return {
        id,
        title: s.title || "运行中会话",
        workspace: s.workspace || "",
        updatedAt: s.updatedAt || "",
        remote: !!store.remoteTasks[id],
      };
    })
    .sort((a, b) => new Date(b.updatedAt || 0) - new Date(a.updatedAt || 0));
});

// 徽标点击：按徽标在屏幕位置决定弹层向上（侧栏底部）/向下（顶栏）展开，
// 右缘钳制防溢出。
function toggle() {
  if (!open.value && badgeEl.value) {
    const r = badgeEl.value.getBoundingClientRect();
    const openUp = r.top > window.innerHeight / 2;
    popPos.value = {
      left: Math.max(8, Math.min(r.left, window.innerWidth - 348)),
      ...(openUp
        ? { bottom: window.innerHeight - r.top + 8 }
        : { top: r.bottom + 8 }),
    };
  }
  open.value = !open.value;
}

// 点击组件外关闭（弹层 teleport 到 body，须经 ref 判断归属）
function onDocClick(e) {
  if (!open.value) return;
  const pop = document.querySelector(".run-tasks-pop");
  if (badgeEl.value && (badgeEl.value.contains(e.target) || (pop && pop.contains(e.target)))) return;
  open.value = false;
}
onMounted(() => document.addEventListener("click", onDocClick, true));
onBeforeUnmount(() => document.removeEventListener("click", onDocClick, true));

const popStyle = computed(() => {
  const s = { left: popPos.value.left + "px" };
  if (popPos.value.bottom !== undefined) s.bottom = popPos.value.bottom + "px";
  if (popPos.value.top !== undefined) s.top = popPos.value.top + "px";
  return s;
});

function normWs(p) {
  return (p || "").replace(/\\/g, "/").replace(/\/+$/, "").toLowerCase();
}
// 工作区短名（会话卡 wsBadge 同语义）：D:/code/gocoder → gocoder
function wsName(p) {
  return (p || "").replace(/[\\/]+$/, "").split(/[\\/]/).pop();
}

// 点任务项：切工作区（绑定且与当前不同）→ 定位会话。目录已失效时保持当前
// 项目继续定位会话（openSession 内部对失效目录同样兑底）。
async function goTask(t) {
  open.value = false;
  store.activeMain = "chat";
  if (t.workspace && normWs(t.workspace) !== normWs(store.workspace)) {
    try {
      await bridgeCall("SwitchProject", t.workspace);
      await loadProjects(); // 刷新项目列表与 store.workspace（侧栏项目名联动）
    } catch { /* 目录可能已不存在：继续定位会话 */ }
  }
  openSession(t.id);
}
</script>

<template>
  <button
    v-if="runningList.length"
    ref="badgeEl"
    class="run-tasks-badge"
    :title="'点击查看运行中的任务（' + runningList.length + '）'"
    @click="toggle"
  >
    <span class="rt-dot"></span>
    {{ runningList.length }} 运行中
  </button>
  <teleport to="body">
    <div v-if="open" class="run-tasks-pop" :style="popStyle">
      <div class="rtp-head">运行中的任务（{{ runningList.length }}）</div>
      <div class="rtp-list">
        <div v-for="t in runningList" :key="t.id" class="rtp-item" :title="t.workspace || ''" @click="goTask(t)">
          <span class="rt-dot"></span>
          <div class="rtp-main">
            <div class="rtp-title">{{ t.title }}</div>
            <div class="rtp-meta">
              <span v-if="t.remote" class="rtp-tag">远程</span>
              <span v-if="t.workspace" class="rtp-ws">{{ wsName(t.workspace) }}</span>
              <span v-if="t.updatedAt">{{ relTime(t.updatedAt) }}</span>
            </div>
          </div>
        </div>
      </div>
    </div>
  </teleport>
</template>

<style scoped>
/* 徽标：复用侧栏 run-badge 视觉（绿色胶囊 + 呼吸点），改为可点按钮 */
.run-tasks-badge {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 1px 8px;
  border-radius: 999px;
  font-size: var(--fs-micro);
  font-weight: 500;
  line-height: 1.5;
  color: var(--success);
  background: color-mix(in srgb, var(--success) 14%, transparent);
  border: 1px solid color-mix(in srgb, var(--success) 30%, transparent);
  white-space: nowrap;
  flex-shrink: 0;
  cursor: pointer;
  transition: background var(--transition), border-color var(--transition);
}
.run-tasks-badge:hover {
  background: color-mix(in srgb, var(--success) 22%, transparent);
  border-color: color-mix(in srgb, var(--success) 45%, transparent);
}
.rt-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--success);
  flex-shrink: 0;
  animation: rt-breath 1.4s ease-in-out infinite;
}
@keyframes rt-breath { 0%, 100% { opacity: 1; } 50% { opacity: 0.35; } }

/* 弹出任务列表：固定定位（位置由徽标矩形计算，向上/向下自适应） */
.run-tasks-pop {
  position: fixed;
  box-sizing: border-box; /* 全局无 border-box：把 padding/border 计入 340px 宽，钳制预留 348 恰好吻合 */
  z-index: 3000;
  width: 340px;
  max-height: 320px;
  overflow-y: auto;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  box-shadow: var(--shadow-overlay);
  padding: 4px;
  animation: rt-pop-in 150ms var(--ease-out) both;
}
@keyframes rt-pop-in { from { opacity: 0; transform: translateY(4px); } to { opacity: 1; transform: none; } }
.rtp-head {
  font-size: var(--fs-micro);
  color: var(--text-3);
  letter-spacing: 0.5px;
  padding: 6px 8px 4px;
  border-bottom: 1px solid var(--border-weak);
  user-select: none;
}
.rtp-list { padding-top: 4px; }
.rtp-item {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 7px 8px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: background var(--transition);
}
.rtp-item:hover { background: var(--bg-hover); }
.rtp-item .rt-dot { margin-top: 5px; }
.rtp-main { min-width: 0; flex: 1; }
.rtp-title {
  font-size: var(--fs-caption);
  color: var(--text-1);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.rtp-meta {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: 2px;
  font-size: var(--fs-micro);
  color: var(--text-3);
}
.rtp-ws {
  max-width: 140px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--text-2);
}
.rtp-tag {
  padding: 0 5px;
  border-radius: 999px;
  font-size: 10px;
  line-height: 1.5;
  color: var(--primary);
  background: var(--primary-tint);
  border: 1px solid var(--primary);
  flex-shrink: 0;
}
</style>
