// ═════════════════════════════════════════════════════════════════
// 流程画布状态管理 composable（单实例 reactive）
//   职责：
//     1) 节点 / 边 / 元信息状态（响应式）
//     2) 撤销 / 重做（快照 ≤ 50 步；结构性变更前 pushHistory）
//     3) 节点运行状态高亮（idle/running/success/error）
//     4) 运行轨迹日志（traces）与最终输出（runOutput）
//     5) 单节点调试独立状态（debugTraces / debugRunning / debugActive）
//   设计要点：
//     - 用 reactive 单例 store，所有组件共享同一份状态
//     - 快照采用深拷贝（JSON.parse(JSON.stringify)）避免引用污染
//     - 运行状态 push 前先深拷贝,运行中变化不入历史
// ═════════════════════════════════════════════════════════════════
import { reactive, ref, computed } from "vue";
import { defaultStep } from "./nodeRegistry.js";

// ── 单例状态（全局共享）──
const nodes = ref([]);
const edges = ref([]);
const meta = reactive({ name: "", description: "" });

// ── 运行态：状态高亮 / 轨迹 / 输出 ──
const nodeStatus = ref({});          // id → running|success|error
const traces = ref([]);               // 全流程运行轨迹
const runOutput = ref("");            // 最终输出
const running = ref(false);           // 全流程运行中
const traceOpen = ref(false);         // 轨迹面板是否展开

// ── 调试态：单节点独立运行 ──
const debugTraces = ref([]);          // 调试轨迹
const debugRunning = ref(false);      // 调试进行中
const debugActive = ref(false);       // 调试激活（事件只进调试不进全局高亮）

// ── 抽屉态 ──
const drawer = reactive({ visible: false, nodeId: "" });

// ── 节点 ID / 边 ID 生成器 ──
let seq = 0;
/**
 * 生成节点 ID（前缀 n + 时间戳 + 自增 seq，避免冲突）
 * @returns {string}
 */
function genNodeId() { return `n${Date.now()}_${++seq}`; }
/**
 * 生成边 ID（前缀 e + 时间戳 + 自增 seq）
 * @returns {string}
 */
function genEdgeId() { return `e${Date.now()}_${++seq}`; }

// ═════════════════════════════════════════════════════════════════
// 撤销 / 重做（≤ 50 步快照）
// ═════════════════════════════════════════════════════════════════
const history = reactive({ past: [], future: [] });
const canUndo = computed(() => history.past.length > 0);
const canRedo = computed(() => history.future.length > 0);

/**
 * 生成当前画布快照（深拷贝）
 * @returns {{nodes:Array,edges:Array}}
 */
function snap() {
    return {
        nodes: nodes.value.map((n) => ({
            id: n.id, type: n.type,
            position: { ...n.position },
            data: JSON.parse(JSON.stringify(n.data)),
            selected: !!n.selected,
        })),
        edges: edges.value.map((e) => ({
            id: e.id, source: e.source, target: e.target,
            sourceHandle: e.sourceHandle ?? null,
            targetHandle: e.targetHandle ?? null,
            data: JSON.parse(JSON.stringify(e.data || {})),
            selected: !!e.selected,
        })),
    };
}

/**
 * 推入历史快照（结构性变更前调用）
 */
function pushHistory() {
    history.past.push(snap());
    if (history.past.length > 50) history.past.shift();
    history.future.length = 0;
}

/**
 * 恢复快照到画布
 * @param {{nodes:Array,edges:Array}} s 快照
 */
function restore(s) {
    nodes.value = s.nodes;
    edges.value = s.edges;
}

/**
 * 撤销一步
 */
function undo() {
    if (!history.past.length) return;
    history.future.push(snap());
    restore(history.past.pop());
}

/**
 * 重做一步
 */
function redo() {
    if (!history.future.length) return;
    history.past.push(snap());
    restore(history.future.pop());
}

// ═════════════════════════════════════════════════════════════════
// 节点状态高亮
// ═════════════════════════════════════════════════════════════════
/**
 * 设置某节点状态
 * @param {string} id 节点 ID
 * @param {"running"|"success"|"error"|"idle"} s 状态
 */
function setStatus(id, s) {
    nodeStatus.value = { ...nodeStatus.value, [id]: s };
}
/**
 * 查询节点状态
 * @param {string} id 节点 ID
 * @returns {string}
 */
function statusOf(id) { return nodeStatus.value[id] || "idle"; }
/**
 * 清空全部节点状态
 */
function clearStatus() { nodeStatus.value = {}; }
/**
 * 清理运行中残留状态（运行结束后把 running 改回 idle）
 */
function resetRunningStatus() {
    Object.keys(nodeStatus.value).forEach((k) => {
        if (nodeStatus.value[k] === "running") setStatus(k, "idle");
    });
}

// ═════════════════════════════════════════════════════════════════
// 节点 / 边操作工具
// ═════════════════════════════════════════════════════════════════
/**
 * 按节点 ID 查找
 * @param {string} id
 * @returns {object|undefined}
 */
function nodeById(id) { return nodes.value.find((n) => n.id === id); }
/**
 * 取节点类型
 * @param {string} id 节点 ID
 * @returns {string}
 */
function nodeType(id) { const n = nodeById(id); return n ? n.data.nodeType : ""; }

/**
 * 连线校验（镜像后端 ValidateCanvas 规则）
 *   - 单入单出 + branch 多出（≤4）+ start/end 度约束 + 防环
 * @param {string} source 源节点 ID
 * @param {string} target 目标节点 ID
 * @returns {{ok:boolean,why:string}}
 */
function canConnect(source, target) {
    const sType = nodeType(source), tType = nodeType(target);
    if (!source || !target) return { ok: false, why: "无效连线" };
    if (source === target) return { ok: false, why: "不能连接自身" };
    if (sType === "end") return { ok: false, why: "「结束」节点不能有出边" };
    if (tType === "start") return { ok: false, why: "「开始」节点不能有入边" };
    const outCnt = edges.value.filter((e) => e.source === source).length;
    const inCnt = edges.value.filter((e) => e.target === target).length;
    if (inCnt >= 1) return { ok: false, why: "目标节点已有一条入线（单入）" };
    if (sType !== "branch" && outCnt >= 1) return { ok: false, why: "该节点已有一条出线（仅条件分支支持多出边）" };
    if (sType === "branch" && outCnt >= 4) return { ok: false, why: "分支节点最多 4 条出边" };
    // 防环：从 target 沿出边能否走回 source（BFS）
    const nextOf = {};
    edges.value.forEach((e) => { (nextOf[e.source] = nextOf[e.source] || []).push(e.target); });
    const seen = new Set();
    const queue = [target];
    while (queue.length) {
        const cur = queue.shift();
        if (cur === source) return { ok: false, why: "该连线会形成环" };
        if (seen.has(cur)) continue;
        seen.add(cur);
        queue.push(...(nextOf[cur] || []));
    }
    return { ok: true, why: "" };
}

/**
 * 在画布添加节点
 * @param {string} t 节点类型
 * @param {{x:number,y:number}} pos 坐标
 * @param {boolean} silent 是否跳过入历史（拖拽过程中插入由调用方推历史）
 * @returns {string} 新节点 ID
 */
function addNodeAt(t, pos, silent) {
    const id = genNodeId();
    nodes.value.push({
        id, type: "flow",
        position: { x: pos.x, y: pos.y },
        data: { nodeType: t, step: defaultStep(t) },
    });
    return id;
}

/**
 * 画布缺 start/end 时自动补齐
 */
function ensureStruct() {
    const has = (t) => nodes.value.some((n) => n.data.nodeType === t);
    let minX = Infinity, maxX = -Infinity, minY = 0;
    nodes.value.forEach((n) => {
        if (minX === Infinity || n.position.x < minX) minX = n.position.x;
        if (maxX === -Infinity || n.position.x > maxX) maxX = n.position.x;
        if (!minY || n.position.y < minY) minY = n.position.y;
    });
    if (minX === Infinity) { minX = 0; maxX = 200; }
    if (!has("start")) {
        nodes.value.push({
            id: "n_start_" + Date.now(), type: "flow",
            position: { x: minX - 220, y: minY },
            data: { nodeType: "start", step: defaultStep("start") },
        });
    }
    if (!has("end")) {
        nodes.value.push({
            id: "n_end_" + Date.now(), type: "flow",
            position: { x: maxX + 220, y: minY },
            data: { nodeType: "end", step: defaultStep("end") },
        });
    }
}

/**
 * 构建保存到后端的画布 payload（与后端 Canvas 结构对齐）
 * @returns {{nodes:Array,edges:Array}}
 */
function buildCanvasPayload() {
    return {
        nodes: nodes.value.map((n) => ({
            id: n.id, type: n.data.nodeType,
            pos: { x: n.position.x, y: n.position.y },
            step: n.data.step,
        })),
        edges: edges.value.map((e) => ({
            id: e.id, source: e.source, target: e.target,
            expr: (e.data && e.data.expr) || e.expr || "",
        })),
    };
}

/**
 * 旧 Steps 数据兜底转画布
 * @param {Array} steps 旧 Steps
 * @returns {{nodes:Array,edges:Array}}
 */
function legacyCanvas(steps) {
    const c = { nodes: [], edges: [] };
    const ts = Date.now();
    const startId = `n_start_${ts}`;
    c.nodes.push({
        id: startId, type: "flow",
        position: { x: -220, y: 0 },
        data: { nodeType: "start", step: defaultStep("start") },
    });
    let prev = startId;
    steps.forEach((s, i) => {
        const id = `n${i}_${ts}`;
        c.nodes.push({
            id, type: "flow",
            position: { x: i * 180, y: 0 },
            data: { nodeType: s.type, step: { ...s } },
        });
        c.edges.push({ id: `e${i}_${ts}`, source: prev, target: id, data: { expr: "" } });
        prev = id;
    });
    c.nodes.push({
        id: `n_end_${ts}`, type: "flow",
        position: { x: steps.length * 180, y: 0 },
        data: { nodeType: "end", step: defaultStep("end") },
    });
    c.edges.push({ id: `e_end_${ts}`, source: prev, target: `n_end_${ts}`, data: { expr: "" } });
    return c;
}

/**
 * 从后端画布格式载入到内部状态
 * @param {{nodes?:Array,edges?:Array}} c 后端画布
 */
function loadFromCanvas(c) {
    nodes.value = (c.nodes || []).map((n) => ({
        id: n.id, type: "flow",
        position: { x: (n.pos && n.pos.x) || 0, y: (n.pos && n.pos.y) || 0 },
        data: { nodeType: n.type, step: n.step || defaultStep(n.type) },
    }));
    edges.value = (c.edges || []).map((e) => ({
        id: e.id || `${e.source}-${e.target}`,
        source: e.source, target: e.target,
        data: { expr: e.expr || "" },
    }));
    ensureStruct();
}

// ── 抽屉操作 ──
/**
 * 打开节点配置抽屉
 * @param {string} id 节点 ID
 */
function openDrawer(id) {
    drawer.visible = true;
    drawer.nodeId = id;
    debugTraces.value = [];
}
/**
 * 关闭抽屉
 */
function closeDrawer() { drawer.visible = false; }

// ── 调试态操作 ──
/**
 * 进入调试模式（事件只进调试不进全局高亮）
 */
function enterDebugMode() {
    debugActive.value = true;
    debugTraces.value = [];
}
/**
 * 退出调试模式
 */
function exitDebugMode() {
    debugActive.value = false;
}

export {
    // 状态
    nodes, edges, meta,
    nodeStatus, traces, runOutput, running, traceOpen,
    debugTraces, debugRunning, debugActive,
    drawer,
    // 历史
    history, canUndo, canRedo,
    pushHistory, snap, restore, undo, redo,
    // 状态高亮
    setStatus, statusOf, clearStatus, resetRunningStatus,
    // 节点操作
    nodeById, nodeType, canConnect, addNodeAt, ensureStruct,
    buildCanvasPayload, legacyCanvas, loadFromCanvas,
    genNodeId, genEdgeId,
    // 抽屉
    openDrawer, closeDrawer,
    // 调试
    enterDebugMode, exitDebugMode,
};
