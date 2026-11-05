<script setup>
// ═════════════════════════════════════════════════════════════════
// 流程画布主壳（Dify/Coze 风格多文件架构）
//   组合：FlowPalette（节点库）· FlowNode（节点渲染）· FlowEdge（连线）
//        · FlowDrawer（配置抽屉 + FlowDebugger）· 轨迹面板 · 变量弹窗
//   后端：GetFlow / SaveFlow / RunFlow / FlowVars / RunNode / ListFlowTools
//   交互（对标 Dify/n8n）：
//     · 连线落空 → 就地「继续添加」节点菜单
//     · 边悬停 → 线中点「+」插入节点并自动重连（保留 branch 条件）
//     · 节点 4 向锚点（上入下出 + 左入右出）
//     · 托盘分组 + 搜索 + 单击/拖拽落到画布
//     · 撤销/重做（≤50 步）· JSON 导入/导出 · 键盘 Delete 删除选中
//     · 运行高亮：节点描边 + 连线流光；双击节点进入抽屉
// ═════════════════════════════════════════════════════════════════
import { computed, nextTick, onMounted, onUnmounted, reactive, ref } from "vue";
import { Message } from "@arco-design/web-vue";
import {
    IconBackward, IconSave, IconPlayArrow, IconThunderbolt,
    IconEye, IconUndo, IconRedo, IconLayout, IconImport,
} from "@arco-design/web-vue/es/icon";
import { VueFlow, useVueFlow } from "@vue-flow/core";
import { Background } from "@vue-flow/background";
import { Controls } from "@vue-flow/controls";
import { MiniMap } from "@vue-flow/minimap";
import "@vue-flow/core/dist/style.css";
import "@vue-flow/core/dist/theme-default.css";
import { bridgeCall, onEvent } from "../bridge/wails.js";

// ── 子组件 ──
import FlowPalette from "./flow/FlowPalette.vue";
import FlowNode from "./flow/FlowNode.vue";
import FlowEdge from "./flow/FlowEdge.vue";
import FlowDrawer from "./flow/FlowDrawer.vue";

// ── 状态管理（单例 composable）──
import {
    nodes, edges, meta,
    traces, runOutput, running, traceOpen,
    drawer, canUndo, canRedo,
    pushHistory, undo, redo,
    setStatus, clearStatus, resetRunningStatus,
    nodeById, nodeType, canConnect, addNodeAt, ensureStruct,
    buildCanvasPayload, legacyCanvas, loadFromCanvas,
    genEdgeId, openDrawer, closeDrawer,
    debugActive, debugTraces, enterDebugMode, exitDebugMode,
} from "./flow/useFlowCanvas.js";

const props = defineProps({ flow: { type: Object, required: true } });
const emit = defineEmits(["close"]);

// ── Vue Flow 工具方法 ──
const {
    addEdges, removeNodes, removeEdges, findNode, updateEdge,
    fitView, project, screenToFlowCoordinate,
    onConnect, onConnectStart, onConnectEnd,
} = useVueFlow();

const canvasEl = ref(null);
const fileInput = ref(null);
const showMiniMap = ref(true);

// ═════════════════════════════════════════════════════════════════
// 载入流程
// ═════════════════════════════════════════════════════════════════
let offStep = null;
onMounted(async () => {
    try {
        const f = await bridgeCall("GetFlow", props.flow.id);
        meta.name = f.name || "";
        meta.description = f.description || "";
        const c = f.canvas || legacyCanvas(f.steps || []);
        loadFromCanvas(c);
        await nextTick();
        fitView({ padding: 0.15, maxZoom: 1.2 });
    } catch (e) {
        Message.error(String((e && e.message) || e));
    }
    // 订阅运行步骤事件（优先按 NodeID 高亮；旧事件按 Index → chainOrder 兜底）
    offStep = onEvent("kaelvoris:flow:step", (t) => {
        if (debugActive.value) {
            debugTraces.value.push(t);
            return;
        }
        traces.value.push(t);
        const id = t.nodeId || chainOrder.value[t.index] || "";
        if (id) setStatus(id, t.err ? "error" : "success");
        const nx = chainOrder.value[t.index + 1];
        if (nx && !t.err) setStatus(nx, "running");
    });
});
onUnmounted(() => { if (offStep) offStep(); });

// ═════════════════════════════════════════════════════════════════
// 链序计算（编译序；分支画布返回 []）
// ═════════════════════════════════════════════════════════════════
const chainOrder = computed(() => {
    const hasBranch = nodes.value.some((n) => n.data.nodeType === "branch");
    if (hasBranch || !nodes.value.length) return [];
    const nextOf = {};
    edges.value.forEach((e) => { nextOf[e.source] = e.target; });
    const indeg = {};
    nodes.value.forEach((n) => { indeg[n.id] = 0; });
    edges.value.forEach((e) => { indeg[e.target] = (indeg[e.target] || 0) + 1; });
    const start = nodes.value.find((n) => !indeg[n.id]);
    if (!start) return [];
    const order = [];
    const seen = new Set();
    for (let cur = start.id; cur && !seen.has(cur); ) {
        seen.add(cur); order.push(cur); cur = nextOf[cur];
    }
    return order.length === nodes.value.length ? order : [];
});

const isEmptyCanvas = computed(() => nodes.value.length === 0);

// ═════════════════════════════════════════════════════════════════
// 连线交互：落空菜单 + 边中点插入 + edgesUpdatable
// ═════════════════════════════════════════════════════════════════
let connectFrom = null;
let connectedThisGesture = false;
onConnectStart(({ handleType, nodeId }) => {
    connectFrom = handleType === "source" ? nodeId : null;
    connectedThisGesture = false;
});
onConnect((conn) => {
    connectedThisGesture = true;
    const v = canConnect(conn.source, conn.target);
    if (!v.ok) { Message.warning(v.why); return; }
    pushHistory();
    addEdges([{ ...conn, id: genEdgeId(), data: { expr: "" } }]);
});
onConnectEnd((evt) => {
    const from = connectFrom;
    connectFrom = null;
    if (!from || connectedThisGesture) return;
    const t = evt && evt.target;
    const el = t && t.closest ? t.closest(".vue-flow__node") : null;
    if (el) {
        // 落在节点上（非锚点）：直接连线
        const id = el.getAttribute("data-id");
        if (id && id !== from) {
            const v = canConnect(from, id);
            if (v.ok) {
                pushHistory();
                addEdges([{ id: genEdgeId(), source: from, target: id, data: { expr: "" } }]);
            } else Message.warning(v.why);
        }
        return;
    }
    openConnectMenu(from, evt.clientX, evt.clientY);
});

// 连线落空菜单（就地继续添加节点）
const connectMenu = reactive({ visible: false, x: 0, y: 0, cx: 0, cy: 0, from: "" });
function openConnectMenu(from, cx, cy) {
    const rect = canvasEl.value.getBoundingClientRect();
    connectMenu.from = from;
    connectMenu.cx = cx; connectMenu.cy = cy;
    connectMenu.x = cx - rect.left;
    connectMenu.y = cy - rect.top;
    connectMenu.visible = true;
}
/**
 * 菜单添加节点 + 自动连线
 * @param {string} t 节点类型
 */
function menuAdd(t) {
    const pos = screenToFlowCoordinate({ x: connectMenu.cx, y: connectMenu.cy });
    pushHistory();
    const nid = addNodeAt(t, { x: pos.x - 80, y: pos.y - 24 }, true);
    const v = canConnect(connectMenu.from, nid);
    if (v.ok) addEdges([{ id: genEdgeId(), source: connectMenu.from, target: nid, data: { expr: "" } }]);
    else Message.warning(v.why);
    connectMenu.visible = false;
}

// 边中点 + 菜单（插入节点并自动重连，保留 branch 条件）
const edgeMenu = reactive({ visible: false, x: 0, y: 0, cx: 0, cy: 0, edgeId: "" });
function openEdgeMenu(e, edge) {
    const rect = canvasEl.value.getBoundingClientRect();
    edgeMenu.edgeId = edge.id;
    edgeMenu.cx = e.clientX; edgeMenu.cy = e.clientY;
    edgeMenu.x = e.clientX - rect.left;
    edgeMenu.y = e.clientY - rect.top;
    edgeMenu.visible = true;
}
/**
 * 边中点插入节点：删旧边 → 新节点 → 重连两端（保留原 expr 到 src→new 边）
 * @param {string} t 节点类型
 */
function edgeMenuAdd(t) {
    const edge = edges.value.find((x) => x.id === edgeMenu.edgeId);
    if (!edge) { edgeMenu.visible = false; return; }
    const expr = (edge.data && edge.data.expr) || "";
    const src = edge.source, tgt = edge.target;
    const pos = screenToFlowCoordinate({ x: edgeMenu.cx, y: edgeMenu.cy });
    pushHistory();
    const nid = addNodeAt(t, { x: pos.x - 80, y: pos.y - 24 }, true);
    removeEdges([edge.id]);
    const v1 = canConnect(src, nid);
    const v2 = v1.ok ? canConnect(nid, tgt) : { ok: false };
    if (v1.ok && v2.ok) {
        addEdges([
            { id: genEdgeId(), source: src, target: nid, data: { expr } },
            { id: genEdgeId(), source: nid, target: tgt, data: { expr: "" } },
        ]);
    } else {
        // 回滚重连（节点保留，undo 可整体撤销）
        addEdges([{ id: genEdgeId(), source: src, target: tgt, data: { expr } }]);
        Message.warning("此处无法插入该类型节点");
    }
    edgeMenu.visible = false;
}

// 拖拽中途把已有连线接到别的节点
function onEdgeUpdate({ edge, connection }) {
    const v = canConnect(connection.source, connection.target);
    if (!v.ok) { Message.warning(v.why); return; }
    const expr = (edge.data && edge.data.expr) || "";
    pushHistory();
    updateEdge(edge, connection);
    if (expr) {
        const ne = edges.value.find((e) => e.id === edge.id);
        if (ne) ne.data = { ...(ne.data || {}), expr };
    }
}

function validateConn(conn) { return canConnect(conn.source, conn.target).ok; }

// ═════════════════════════════════════════════════════════════════
// 托盘事件：单击落到画布中心 / 拖拽落到鼠标位置
// ═════════════════════════════════════════════════════════════════
function quickAdd(t) {
    const el = canvasEl.value;
    let pos = { x: 120, y: 120 };
    if (el && typeof screenToFlowCoordinate === "function") {
        const r = el.getBoundingClientRect();
        pos = screenToFlowCoordinate({ x: r.left + r.width / 2, y: r.top + r.height / 2 });
    }
    const k = nodes.value.length % 5;
    pushHistory();
    addNodeAt(t, { x: pos.x - 80 + k * 18, y: pos.y - 24 + k * 14 }, true);
}
function onDrop(e) {
    const t = e.dataTransfer.getData("application/x-flow-node");
    if (!t) return;
    const pos = typeof screenToFlowCoordinate === "function"
        ? screenToFlowCoordinate({ x: e.clientX, y: e.clientY })
        : project({ x: e.clientX, y: e.clientY });
    pushHistory();
    addNodeAt(t, { x: pos.x - 80, y: pos.y - 24 }, true);
}

// ═════════════════════════════════════════════════════════════════
// 键盘删除选中（禁用内置删除以便入历史）
// ═════════════════════════════════════════════════════════════════
function deleteSelection() {
    const selN = nodes.value.filter((n) => n.selected && !["start", "end"].includes(n.data.nodeType));
    const selE = edges.value.filter((e) => e.selected);
    if (!selN.length && !selE.length) return;
    pushHistory();
    if (selE.length) removeEdges(selE.map((e) => e.id));
    if (selN.length) {
        const ids = selN.map((n) => n.id);
        removeEdges(edges.value.filter((e) => ids.includes(e.source) || ids.includes(e.target)));
        removeNodes(ids);
    }
}

// ═════════════════════════════════════════════════════════════════
// 节点抽屉相关：双击节点 → 打开抽屉
// ═════════════════════════════════════════════════════════════════
function onNodeDbl(id) { openDrawer(id); }

// ═════════════════════════════════════════════════════════════════
// 节点复制 / 删除（抽屉触发）
// ═════════════════════════════════════════════════════════════════
function duplicateCurNode() {
    const n = nodeById(drawer.nodeId);
    if (!n || ["start", "end"].includes(n.data.nodeType)) return;
    pushHistory();
    nodes.value.push({
        id: `n${Date.now()}_${Math.random().toString(36).slice(2, 6)}`, type: "flow",
        position: { x: n.position.x + 48, y: n.position.y + 48 },
        data: { nodeType: n.data.nodeType, step: JSON.parse(JSON.stringify(n.data.step)) },
    });
    Message.success("已复制");
}
function removeCurNode() {
    const n = nodeById(drawer.nodeId);
    if (!n) return;
    if (["start", "end"].includes(n.data.nodeType)) {
        Message.warning("开始/结束节点为结构节点，不可删除");
        return;
    }
    pushHistory();
    removeEdges(edges.value.filter((e) => e.source === n.id || e.target === n.id));
    removeNodes([n.id]);
    closeDrawer();
    Message.info("节点已删除");
}

// ═════════════════════════════════════════════════════════════════
// 运行 / 试运行（全流程）
// ═════════════════════════════════════════════════════════════════
const varsModal = ref({ visible: false, names: [], values: {}, dryRun: false });
async function startRun(dryRun) {
    if (!nodes.value.length) { Message.warning("画布为空，请先拖入节点"); return; }
    let names = [];
    try { names = (await bridgeCall("FlowVars", props.flow.id)) || []; } catch { names = []; }
    if (names.length) {
        varsModal.value = { visible: true, names, values: {}, dryRun };
        running.value = false;
        return;
    }
    await doRun({}, dryRun);
}
async function confirmVars() {
    const m = varsModal.value;
    m.visible = false;
    await doRun({ ...m.values }, m.dryRun);
}
async function doRun(vars, dryRun) {
    running.value = true;
    traces.value = []; runOutput.value = ""; clearStatus();
    traceOpen.value = true;
    // 先保存画布（后端编译校验），失败即拦截
    if (!(await save(true))) { running.value = false; return; }
    try {
        const res = await bridgeCall("RunFlow", { id: props.flow.id, vars, dryRun });
        runOutput.value = (res && res.output) || "";
        if (res && Array.isArray(res.steps)) traces.value = res.steps;
        Message.success("运行完成");
    } catch (e) {
        Message.error(String((e && e.message) || e));
    } finally {
        running.value = false;
        resetRunningStatus();
    }
}

// ═════════════════════════════════════════════════════════════════
// 保存（前端快速校验 → SaveFlow 后端编译校验）
// ═════════════════════════════════════════════════════════════════
const saving = ref(false);
async function save(silent = false) {
    const hasStart = nodes.value.some((n) => n.data.nodeType === "start");
    const hasEnd = nodes.value.some((n) => n.data.nodeType === "end");
    if (!hasStart || !hasEnd) {
        Message.warning("画布缺少开始/结束节点（保存时自动补齐，请检查连线）");
    }
    const steps = nodes.value
        .filter((n) => !["start", "end", "branch"].includes(n.data.nodeType))
        .map((n) => ({ ...n.data.step, type: n.data.nodeType }));
    saving.value = true;
    try {
        const saved = await bridgeCall("SaveFlow", {
            id: props.flow.id, name: meta.name || props.flow.name,
            description: meta.description || "", appId: props.flow.appId || "",
            steps, canvas: buildCanvasPayload(),
        });
        if (saved && saved.id) props.flow.id = saved.id;
        if (!silent) Message.success("已保存");
        return true;
    } catch (e) {
        Message.error(String((e && e.message) || e));
        return false;
    } finally {
        saving.value = false;
    }
}

// 抽屉调试器回调：保存画布的同步钩子（debugger 通过 emit 触发）
function onSaveBefore({ ok, fail }) {
    save(true).then((r) => r ? ok() : fail()).catch(fail);
}

// ═════════════════════════════════════════════════════════════════
// JSON 导入 / 导出
// ═════════════════════════════════════════════════════════════════
function exportJSON() {
    const payload = {
        name: meta.name || props.flow.name || "flow",
        description: meta.description || "",
        canvas: buildCanvasPayload(),
    };
    const blob = new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `${payload.name}.json`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
    Message.success("已导出流程 JSON");
}
function importJSON() { if (fileInput.value) fileInput.value.click(); }
async function onImportFile(ev) {
    const file = ev.target.files && ev.target.files[0];
    ev.target.value = "";
    if (!file) return;
    try {
        const data = JSON.parse(await file.text());
        const c = data.canvas || data;
        if (!c || !Array.isArray(c.nodes) || !c.nodes.length) throw new Error("文件中没有画布节点");
        pushHistory();
        if (data.name) meta.name = data.name;
        if (data.description != null) meta.description = data.description;
        loadFromCanvas(c);
        await nextTick();
        fitView({ padding: 0.15, maxZoom: 1.2 });
        Message.success("已导入（保存后生效）");
    } catch (e) {
        Message.error("导入失败: " + String((e && e.message) || e));
    }
}
</script>

<template>
    <div class="fc">
        <!-- 工具条 -->
        <div class="fc-toolbar">
            <button class="fc-btn" title="返回列表" @click="emit('close')"><IconBackward /></button>
            <input v-model="meta.name" class="fc-title" placeholder="流程名" :max-length="40" />
            <span v-if="chainOrder.length" class="fc-chain-hint">单链 · {{ chainOrder.length }} 节点</span>
            <span v-else-if="nodes.length" class="fc-chain-hint branch">分支图</span>
            <span class="fc-flex"></span>
            <button class="fc-btn" :disabled="!canUndo" title="撤销" @click="undo"><IconUndo /></button>
            <button class="fc-btn" :disabled="!canRedo" title="重做" @click="redo"><IconRedo /></button>
            <button class="fc-btn" title="导入 JSON" @click="importJSON"><IconImport /></button>
            <button class="fc-btn" title="导出 JSON" @click="exportJSON"><IconLayout /></button>
            <button class="fc-btn" :disabled="running" title="试运行（跳过模型/工具）" @click="startRun(true)"><IconThunderbolt /></button>
            <button class="fc-btn primary" :disabled="running" title="运行（调用模型/工具）" @click="startRun(false)"><IconPlayArrow /></button>
            <button class="fc-btn" :class="{ on: showMiniMap }" :title="showMiniMap ? '隐藏小地图' : '显示小地图'" @click="showMiniMap = !showMiniMap">
                <IconEye />
            </button>
            <button class="fc-btn" :disabled="saving" title="保存（后端校验并编译）" @click="save(false)"><IconSave /></button>
        </div>

        <div class="fc-body">
            <!-- 节点托盘 -->
            <FlowPalette @quick-add="quickAdd" />

            <!-- 画布 -->
            <div
                class="fc-canvas" ref="canvasEl" tabindex="0"
                @drop="onDrop" @dragover.prevent
                @keydown.delete.prevent="deleteSelection"
            >
                <div v-if="isEmptyCanvas" class="fc-empty-guide">
                    <div class="fc-empty-logo">⌘</div>
                    <div class="fc-empty-title">从左侧拖入节点开始编排</div>
                    <div class="fc-empty-desc">开始 / 结束节点已自动就位 · 拖动节点边缘圆点连线 · 条件分支节点可按变量路由</div>
                </div>
                <VueFlow
                    v-model:nodes="nodes"
                    v-model:edges="edges"
                    :is-valid-connection="validateConn"
                    :delete-key-code="null"
                    :edges-updatable="true"
                    :connection-radius="30"
                    :default-edge-options="{ type: 'flowEdge' }"
                    fit-view-on-init
                    @edge-update="onEdgeUpdate"
                    @pane-click="connectMenu.visible = false; edgeMenu.visible = false"
                >
                    <Background :gap="18" pattern-color="rgba(245,244,237,0.05)" />
                    <Controls position="bottom-left" />
                    <MiniMap v-if="showMiniMap" position="bottom-right" pannable zoomable />
                    <template #node-flow="{ id, data, selected }">
                        <FlowNode :id="id" :data="data" :selected="selected" @open-drawer="onNodeDbl" />
                    </template>
                    <template #edge-flowEdge="ep">
                        <FlowEdge v-bind="ep" @open-edge-menu="openEdgeMenu" />
                    </template>
                </VueFlow>

                <!-- 连线落空菜单（继续添加） -->
                <div v-if="connectMenu.visible" class="fc-pop" :style="{ left: connectMenu.x + 'px', top: connectMenu.y + 'px' }">
                    <div class="fc-pop-title">继续添加</div>
                    <button v-for="t in ['llm', 'template', 'tools', 'branch', 'passthrough']" :key="t" class="fc-pop-item" @click="menuAdd(t)">
                        {{ t }}
                    </button>
                </div>

                <!-- 边上加号菜单（插入节点） -->
                <div v-if="edgeMenu.visible" class="fc-pop" :style="{ left: edgeMenu.x + 'px', top: edgeMenu.y + 'px' }">
                    <div class="fc-pop-title">在此插入</div>
                    <button v-for="t in ['llm', 'template', 'tools', 'branch', 'passthrough']" :key="t" class="fc-pop-item" @click="edgeMenuAdd(t)">
                        {{ t }}
                    </button>
                </div>
            </div>
        </div>

        <!-- 轨迹日志 -->
        <div class="fc-trace" :class="{ open: traceOpen }">
            <button class="fc-trace-head" @click="traceOpen = !traceOpen">
                <span>运行轨迹（{{ traces.length }}）</span>
                <span v-if="runOutput" class="fc-trace-out" :title="runOutput">输出：{{ runOutput.slice(0, 60) }}</span>
                <span class="fc-flex"></span>
                <span class="fc-trace-arrow">{{ traceOpen ? "▾" : "▸" }}</span>
            </button>
            <div v-if="traceOpen" class="fc-trace-list">
                <div v-if="!traces.length && !running" class="fc-trace-empty">尚无运行记录</div>
                <div v-for="(t, i) in traces" :key="i" class="fc-trace-item" :class="{ err: !!t.err }">
                    <span class="fc-trace-idx">{{ t.index + 1 }}</span>
                    <span class="fc-trace-name">{{ t.name || t.type }}</span>
                    <span class="fc-trace-text" :title="t.render">{{ t.err ? t.err : t.render }}</span>
                </div>
                <div v-if="runOutput" class="fc-trace-output">
                    <div class="fc-trace-label">最终输出</div>
                    <pre class="fc-trace-pre">{{ runOutput }}</pre>
                </div>
            </div>
        </div>

        <!-- 节点配置抽屉（含调试器） -->
        <FlowDrawer
            :flow-id="props.flow.id"
            @close="closeDrawer"
            @remove-node="removeCurNode"
            @duplicate-node="duplicateCurNode"
            @save-before="onSaveBefore"
        />

        <!-- 变量填写弹窗（全流程运行） -->
        <a-modal v-model:visible="varsModal.visible" title="填写运行变量" @before-ok="confirmVars">
            <a-form layout="vertical">
                <a-form-item v-for="n in varsModal.names" :key="n" :label="`{${n}}`" required>
                    <a-input v-model="varsModal.values[n]" placeholder="变量值" />
                </a-form-item>
                <div class="fc-modal-hint">模板中的 {'{var}'} 占位符需要填值。</div>
            </a-form>
        </a-modal>

        <!-- 隐藏文件选择（JSON 导入） -->
        <input ref="fileInput" type="file" accept="application/json,.json" style="display: none" @change="onImportFile" />
    </div>
</template>

<style scoped>
.fc { display: flex; flex-direction: column; height: 100%; min-height: 0; flex: 1; }
.fc-toolbar {
    display: flex; align-items: center; gap: var(--gap-sm);
    padding: var(--gap-sm) var(--gap); flex: none;
}
.fc-title {
    background: transparent; border: none; outline: none;
    color: var(--text-1); font-size: var(--fs-title); font-weight: 500;
    padding: 4px 8px; border-radius: var(--radius-sm); min-width: 120px; max-width: 320px;
}
.fc-title:focus { background: var(--bg-inset); }
.fc-chain-hint {
    font-size: var(--fs-micro); color: var(--text-3);
    background: var(--bg-inset); border-radius: 4px; padding: 1px 8px;
}
.fc-chain-hint.branch { color: var(--warning); }
.fc-flex { flex: 1; }
.fc-btn {
    display: inline-flex; align-items: center; justify-content: center;
    width: 30px; height: 30px; border-radius: var(--radius-sm);
    color: var(--text-2); background: transparent; border: 1px solid transparent; cursor: pointer;
    transition: var(--transition);
}
.fc-btn:hover:not(:disabled) { color: var(--primary); background: var(--bg-hover); }
.fc-btn.primary, .fc-btn.on { color: var(--primary); }
.fc-btn:disabled { opacity: 0.4; cursor: not-allowed; }
.fc-body { flex: 1; min-height: 0; display: flex; }

/* 画布 */
.fc-canvas { flex: 1; min-width: 0; position: relative; outline: none; }
.fc-empty-guide {
    position: absolute; inset: 0; z-index: 1; pointer-events: none;
    display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 8px;
    color: var(--text-3);
}
.fc-empty-logo {
    width: 56px; height: 56px; border-radius: 16px; display: flex; align-items: center; justify-content: center;
    font-size: 26px; color: var(--primary); background: var(--bg-card);
    border: 1px dashed var(--border-weak);
}
.fc-empty-title { font-size: var(--fs-body); color: var(--text-2); }
.fc-empty-desc { font-size: var(--fs-caption); }

/* 就地弹出菜单 */
.fc-pop {
    position: absolute; z-index: 30; min-width: 132px;
    background: var(--bg-elevated); border: 1px solid var(--border-weak);
    border-radius: var(--radius); box-shadow: 0 8px 24px rgba(0, 0, 0, 0.4);
    padding: 6px; display: flex; flex-direction: column; gap: 2px;
}
.fc-pop-title { font-size: var(--fs-micro); color: var(--text-3); padding: 2px 8px 4px; }
.fc-pop-item {
    display: flex; align-items: center; gap: 8px;
    background: transparent; border: none; cursor: pointer;
    padding: 6px 8px; border-radius: var(--radius-sm);
    color: var(--text-1); font-size: var(--fs-caption); text-align: left;
    transition: var(--transition);
}
.fc-pop-item:hover { background: var(--bg-hover); color: var(--primary); }

/* 轨迹日志 */
.fc-trace { flex: none; border-top: 1px solid var(--border-weak); background: var(--bg-card); }
.fc-trace-head {
    display: flex; align-items: center; gap: var(--gap); width: 100%;
    padding: 8px var(--gap); background: transparent; border: none; cursor: pointer;
    color: var(--text-2); font-size: var(--fs-caption);
}
.fc-trace-head:hover { color: var(--text-1); }
.fc-trace-out { color: var(--text-3); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 46%; }
.fc-trace-list { max-height: 180px; overflow-y: auto; padding: 0 var(--gap) var(--gap-sm); }
.fc-trace-empty { padding: var(--gap); text-align: center; color: var(--text-3); font-size: var(--fs-caption); }
.fc-trace-item {
    display: flex; align-items: baseline; gap: var(--gap-sm);
    padding: 5px 8px; border-radius: var(--radius-sm); font-size: var(--fs-caption);
}
.fc-trace-item:hover { background: var(--bg-hover); }
.fc-trace-item.err .fc-trace-text { color: var(--danger); }
.fc-trace-idx {
    flex: none; width: 20px; text-align: center; font-size: var(--fs-micro);
    color: var(--text-3); background: var(--bg-inset); border-radius: 8px;
}
.fc-trace-name { flex: none; color: var(--text-2); min-width: 64px; }
.fc-trace-text { color: var(--text-3); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.fc-trace-output { margin-top: 6px; }
.fc-trace-label { font-size: var(--fs-micro); color: var(--text-3); margin-bottom: 4px; }
.fc-trace-pre {
    margin: 0; padding: 10px; border-radius: var(--radius-sm);
    background: var(--bg-inset); color: var(--text-1);
    font-size: var(--fs-caption); font-family: var(--font-mono, monospace);
    white-space: pre-wrap; word-break: break-word; max-height: 160px; overflow-y: auto;
}
.fc-modal-hint { font-size: var(--fs-caption); color: var(--text-3); }
</style>
