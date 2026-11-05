<script setup>
// ═════════════════════════════════════════════════════════════════
// 流程编排面板（P1）：左侧应用列表 → 右侧该应用下工作流列表 → 打开画布
// 后端：flow_app_panel.go（ListApps/CreateApp/UpdateApp/DeleteApp/
//   ListFlowsByApp/ListFlowTools/RunNode）；事件：kaelvoris:flowapps。
// 兼容：旧平铺工作流（Flow.AppID 为空）归入「默认应用」虚拟分组。
// ═════════════════════════════════════════════════════════════════
import { onMounted, onUnmounted, ref, watch } from "vue";
import { Message, Modal } from "@arco-design/web-vue";
import {
  IconPlus, IconDelete, IconEdit, IconApps,
} from "@arco-design/web-vue/es/icon";
import { bridgeCall, onEvent } from "../bridge/wails.js";
import { fmtTime } from "../utils/format.js";
import FlowCanvas from "./FlowCanvas.vue";

const apps = ref([]);            // 应用列表（AppSummary[]）
const flows = ref([]);           // 当前应用下工作流（Summary[]）
const activeApp = ref("");       // "" = 默认应用
const activeFlow = ref(null);    // 打开中的流程（传给画布）
const loading = ref(false);

// 应用新建/重命名弹窗
const appModal = ref({ visible: false, mode: "create", id: "", name: "", desc: "", icon: "" });
// 工作流新建弹窗
const flowModal = ref({ visible: false, name: "" });

async function loadApps() {
  try {
    const d = await bridgeCall("ListApps");
    apps.value = Array.isArray(d) ? d : [];
  } catch (e) {
    console.warn("ListApps failed:", e);
  }
}

async function loadFlows() {
  loading.value = true;
  try {
    const d = await bridgeCall("ListFlowsByApp", activeApp.value);
    flows.value = Array.isArray(d) ? d : [];
  } catch (e) {
    console.warn("ListFlowsByApp failed:", e);
    flows.value = [];
  } finally {
    loading.value = false;
  }
}

function selectApp(id) {
  activeApp.value = id;
  activeFlow.value = null;
}

watch(activeApp, loadFlows);

function onAppsEvent(list) {
  if (!Array.isArray(list)) return;
  apps.value = list;
}

let offApps = null;
onMounted(async () => {
  offApps = onEvent("kaelvoris:flowapps", onAppsEvent);
  await loadApps();
  await loadFlows();
});
onUnmounted(() => { if (offApps) offApps(); });

// ── 应用 CRUD ──
function openCreateApp() {
  appModal.value = { visible: true, mode: "create", id: "", name: "", desc: "", icon: "" };
}
function openRenameApp(ap) {
  appModal.value = { visible: true, mode: "rename", id: ap.id, name: ap.name, desc: ap.desc || "", icon: ap.icon || "" };
}
async function submitAppModal() {
  const m = appModal.value;
  if (!m.name.trim()) { Message.warning("应用名称不能为空"); return; }
  try {
    if (m.mode === "create") {
      await bridgeCall("CreateApp", m.name.trim(), m.desc.trim(), m.icon.trim());
      Message.success("应用已创建");
    } else {
      await bridgeCall("UpdateApp", m.id, m.name.trim(), m.desc.trim(), m.icon.trim());
      Message.success("应用已更新");
    }
    m.visible = false;
    await loadApps();
  } catch (e) {
    Message.error(String(e && e.message || e));
  }
}
function removeApp(ap) {
  Modal.confirm({
    title: "删除应用",
    content: `确定删除「${ap.name}」？其下 ${ap.flowCount} 个工作流将归入「默认应用」，不会丢失。`,
    okButtonProps: { status: "danger" },
    onOk: async () => {
      try {
        if (activeApp.value === ap.id) activeApp.value = "";
        await bridgeCall("DeleteApp", ap.id);
        Message.success("应用已删除");
        await loadApps();
        await loadFlows();
      } catch (e) {
        Message.error(String(e && e.message || e));
      }
    },
  });
}

// ── 工作流 CRUD ──
async function submitFlowModal() {
  const name = flowModal.value.name.trim();
  if (!name) { Message.warning("工作流名称不能为空"); return; }
  try {
    // 新建工作流默认一个 passthrough 节点；保存时后端 EnsureStartEnd 自动补齐 start/end 结构节点。
    const saved = await bridgeCall("SaveFlow", { name, steps: [{ type: "passthrough" }], appId: activeApp.value || "" });
    flowModal.value.visible = false;
    flowModal.value.name = "";
    Message.success("工作流已创建");
    await loadFlows();
    if (saved && saved.id) activeFlow.value = saved; // 直接进入画布
  } catch (e) {
    Message.error(String(e && e.message || e));
  }
}
function removeFlow(f) {
  Modal.confirm({
    title: "删除工作流",
    content: `确定删除「${f.name}」？该操作不可恢复。`,
    okButtonProps: { status: "danger" },
    onOk: async () => {
      try {
        await bridgeCall("DeleteFlow", f.id);
        Message.success("工作流已删除");
        if (activeFlow.value && activeFlow.value.id === f.id) activeFlow.value = null;
        await loadFlows();
      } catch (e) {
        Message.error(String(e && e.message || e));
      }
    },
  });
}

function onCanvasClose() {
  activeFlow.value = null;
  loadFlows();
}

function flowCountOf(id) {
  if (id === "") {
    // 默认应用计数：总减去各应用之和（ListApps 的 FlowCount 仅统计有主流程）
    const known = apps.value.reduce((s, a) => s + (a.flowCount || 0), 0);
    return Math.max(0, flowsAllCount.value - known);
  }
  const a = apps.value.find((x) => x.id === id);
  return a ? (a.flowCount || 0) : 0;
}
// 全量工作流数（供默认应用计数；轻量：仅取数量）
const flowsAllCount = ref(0);
async function refreshAllCount() {
  try {
    const d = await bridgeCall("ListFlows");
    flowsAllCount.value = Array.isArray(d) ? d.length : 0;
  } catch { flowsAllCount.value = 0; }
}
watch(activeApp, refreshAllCount, { immediate: true });
</script>

<template>
  <div class="flowpanel">
    <!-- 左：应用列表 -->
    <aside class="fp-apps">
      <div class="fp-pane-head">
        <span class="fp-pane-title"><IconApps /> 应用</span>
        <button class="fp-add" title="新建应用" @click="openCreateApp"><IconPlus /></button>
      </div>
      <div class="fp-app-item" :class="{ active: activeApp === '' }" @click="selectApp('')">
        <span class="fp-app-name">默认应用</span>
        <span class="fp-app-count">{{ flowCountOf("") }}</span>
      </div>
      <div
        v-for="ap in apps" :key="ap.id"
        class="fp-app-item" :class="{ active: activeApp === ap.id }"
        @click="selectApp(ap.id)"
      >
        <span class="fp-app-name" :title="ap.desc || ap.name">{{ ap.name }}</span>
        <span class="fp-app-meta">
          <span class="fp-app-count">{{ ap.flowCount || 0 }}</span>
          <button class="fp-icon-btn" title="重命名" @click.stop="openRenameApp(ap)"><IconEdit /></button>
          <button class="fp-icon-btn danger" title="删除" @click.stop="removeApp(ap)"><IconDelete /></button>
        </span>
      </div>
      <div v-if="!apps.length" class="fp-empty-hint">暂无应用，点击 + 新建</div>
    </aside>

    <!-- 右：工作流列表 或 画布 -->
    <section class="fp-main">
      <FlowCanvas v-if="activeFlow" :flow="activeFlow" @close="onCanvasClose" />
      <template v-else>
        <div class="fp-pane-head">
          <span class="fp-pane-title">工作流 · {{ activeApp === "" ? "默认应用" : (apps.find(a => a.id === activeApp) || {}).name || "" }}</span>
          <button class="fp-add" title="新建工作流" @click="flowModal.visible = true"><IconPlus /></button>
        </div>
        <div class="fp-flows">
          <div v-for="f in flows" :key="f.id" class="fp-flow-card" @click="activeFlow = { id: f.id }">
            <div class="fp-flow-name">{{ f.name }}</div>
            <div class="fp-flow-desc">{{ f.description || "（无描述）" }}</div>
            <div class="fp-flow-meta">
              <span>{{ f.stepCount }} 个节点</span>
              <span>{{ fmtTime(f.updatedAt) }}</span>
              <button class="fp-icon-btn danger" title="删除" @click.stop="removeFlow(f)"><IconDelete /></button>
            </div>
          </div>
          <div v-if="!flows.length && !loading" class="fp-empty-hint">该应用暂无工作流，点击 + 新建</div>
        </div>
      </template>
    </section>

    <!-- 应用弹窗（新建/重命名） -->
    <a-modal
      v-model:visible="appModal.visible"
      :title="appModal.mode === 'create' ? '新建应用' : '重命名应用'"
      :ok-loading="false" @before-ok="submitAppModal"
    >
      <a-form layout="vertical">
        <a-form-item label="名称" required>
          <a-input v-model="appModal.name" placeholder="如：客服助手" :max-length="40" />
        </a-form-item>
        <a-form-item label="描述">
          <a-input v-model="appModal.desc" placeholder="一句话说明用途（可空）" :max-length="120" />
        </a-form-item>
        <a-form-item label="图标（Arco 图标名，可空）">
          <a-input v-model="appModal.icon" placeholder="如：IconRobot（可空）" :max-length="40" />
        </a-form-item>
      </a-form>
    </a-modal>

    <!-- 工作流新建弹窗 -->
    <a-modal v-model:visible="flowModal.visible" title="新建工作流" @before-ok="submitFlowModal">
      <a-form layout="vertical">
        <a-form-item label="名称" required>
          <a-input v-model="flowModal.name" placeholder="如：日报生成" :max-length="40" @keyup.enter="submitFlowModal" />
        </a-form-item>
        <div class="fp-modal-hint">创建后进入画布：开始/结束节点自动就位，从左侧托盘拖入节点编排，支持条件分支。</div>
      </a-form>
    </a-modal>
  </div>
</template>

<style scoped>
.flowpanel { display: flex; height: 100%; min-height: 0; overflow: hidden; }
.fp-apps {
  width: 216px; flex: none; display: flex; flex-direction: column;
  border-right: 1px solid var(--border-weak); padding: var(--gap) 0; overflow-y: auto;
}
.fp-pane-head {
  display: flex; align-items: center; justify-content: space-between;
  padding: 0 var(--gap) var(--gap-sm); flex: none;
}
.fp-pane-title { display: inline-flex; align-items: center; gap: 6px; font-size: var(--fs-caption); color: var(--text-2); }
.fp-add {
  display: inline-flex; align-items: center; justify-content: center;
  width: 26px; height: 26px; border-radius: var(--radius-sm);
  color: var(--text-2); background: transparent; border: 1px solid transparent; cursor: pointer;
  transition: var(--transition);
}
.fp-add:hover { color: var(--primary); background: var(--bg-hover); }
.fp-app-item {
  display: flex; align-items: center; justify-content: space-between; gap: var(--gap-sm);
  padding: 7px var(--gap); margin: 1px var(--gap-sm); border-radius: var(--radius-sm);
  cursor: pointer; transition: var(--transition); font-size: var(--fs-body); color: var(--text-1);
}
.fp-app-item:hover { background: var(--bg-hover); }
.fp-app-item.active { background: var(--bg-active); color: var(--primary); }
.fp-app-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.fp-app-meta { display: inline-flex; align-items: center; gap: 4px; flex: none; }
.fp-app-count {
  font-size: var(--fs-micro); color: var(--text-3);
  background: var(--bg-inset); border-radius: 8px; padding: 1px 7px; min-width: 20px; text-align: center;
}
.fp-icon-btn {
  display: inline-flex; align-items: center; justify-content: center;
  width: 22px; height: 22px; border-radius: var(--radius-sm);
  color: var(--text-3); background: transparent; border: none; cursor: pointer; opacity: 0;
  transition: var(--transition);
}
.fp-app-item:hover .fp-icon-btn, .fp-flow-card:hover .fp-icon-btn { opacity: 1; }
.fp-icon-btn:hover { color: var(--text-1); background: var(--bg-hover); }
.fp-icon-btn.danger:hover { color: var(--danger); }
.fp-empty-hint { padding: var(--gap-lg); font-size: var(--fs-caption); color: var(--text-3); text-align: center; }
.fp-main { flex: 1; min-width: 0; display: flex; flex-direction: column; padding: var(--gap) var(--gap-lg) var(--gap) 0; }
.fp-flows {
  flex: 1; min-height: 0; overflow-y: auto;
  display: grid; grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
  gap: var(--gap); align-content: start; padding: 0 var(--gap-lg) var(--gap);
}
.fp-flow-card {
  background: var(--bg-card); border: 1px solid var(--border-weak); border-radius: var(--radius);
  padding: var(--gap) var(--gap-lg); cursor: pointer; transition: var(--transition);
}
.fp-flow-card:hover { border-color: var(--primary); background: var(--bg-elevated); }
.fp-flow-name { font-size: var(--fs-body); color: var(--text-1); font-weight: 500; }
.fp-flow-desc {
  font-size: var(--fs-caption); color: var(--text-3); margin-top: 2px;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.fp-flow-meta {
  display: flex; align-items: center; gap: var(--gap);
  margin-top: var(--gap-sm); font-size: var(--fs-micro); color: var(--text-3);
}
.fp-modal-hint { font-size: var(--fs-caption); color: var(--text-3); }
</style>
