<script setup>
// ═════════════════════════════════════════════════════════════════
// 单节点调试器：对标 Dify 单节点独立运行 + 输入输出预览
//   职责：
//     1) 试运行 / 真实调试（dryRun 切换模型/工具是否实际调用）
//     2) 调试结果结构化展示：输入（节点配置 JSON）· 输出（render 文本）·
//        耗时 · 错误 · 上游消息历史
//     3) 变量填写弹窗（流程模板中的 {var} 占位符需要填值）
//   设计要点：
//     - emit save-before 让父组件先保存画布（后端需最新画布定位节点）
//     - 调用 RunNode bridgeCall，结果写入 useFlowCanvas.debugTraces
// ═════════════════════════════════════════════════════════════════
import { computed, ref } from "vue";
import { Message } from "@arco-design/web-vue";
import { IconBug, IconPlayArrow } from "@arco-design/web-vue/es/icon";
import { bridgeCall } from "../../bridge/wails.js";
import {
    debugTraces, debugRunning, enterDebugMode, exitDebugMode,
} from "./useFlowCanvas.js";

const props = defineProps({
    node: { type: Object, required: true },   // 当前节点（节点对象）
    flowId: { type: String, required: true }, // 流程 ID
});
const emit = defineEmits(["save-before"]);

// ── 调试变量填写弹窗 ──
const varsModal = ref({ visible: false, names: [], values: {}, _dry: false });

/**
 * 启动调试（试运行 or 真实）
 *   流程：先 emit save-before 保存画布 → 拉取变量列表 → 无变量直接运行
 * @param {boolean} dryRun 是否试运行
 */
async function debugNode(dryRun) {
    // 1) 先保存画布
    const ok = await emitSave();
    if (!ok) return;
    // 2) 拉取变量列表
    let names = [];
    try { names = (await bridgeCall("FlowVars", props.flowId)) || []; } catch { names = []; }
    if (names.length) {
        varsModal.value = { visible: true, names, values: {}, _dry: dryRun };
        return;
    }
    await doDebug({}, dryRun);
}

/**
 * 把 emit save-before 包装成 Promise
 * @returns {Promise<boolean>} 保存是否成功
 */
function emitSave() {
    return new Promise((resolve) => {
        let done = false;
        const timer = setTimeout(() => { done = true; resolve(false); }, 30000);
        emit("save-before", {
            ok: () => {
                if (done) return;
                done = true; clearTimeout(timer); resolve(true);
            },
            fail: () => {
                if (done) return;
                done = true; clearTimeout(timer); resolve(false);
            },
        });
    });
}

/**
 * 确认变量填写弹窗 → 执行调试
 */
async function confirmVars() {
    const m = varsModal.value;
    m.visible = false;
    await doDebug({ ...m.values }, m._dry);
}

/**
 * 执行调试：调用 RunNode，结果写入 debugTraces
 * @param {object} vars 变量值
 * @param {boolean} dryRun 是否试运行
 */
async function doDebug(vars, dryRun) {
    enterDebugMode();
    debugRunning.value = true;
    debugTraces.value = [];
    const startedAt = Date.now();
    try {
        const res = await bridgeCall("RunNode", {
            flowId: props.flowId,
            nodeId: props.node.id,
            vars, dryRun: !!dryRun,
        });
        if (res && Array.isArray(res.steps)) debugTraces.value.push(...res.steps);
        if (res && res.output && !debugTraces.value.length) {
            debugTraces.value.push({
                index: 0, type: props.node.data.nodeType,
                name: props.node.data.step.name,
                render: res.output,
            });
        }
        Message.success(dryRun ? "试运行完成" : "调试完成");
    } catch (e) {
        debugTraces.value.push({
            index: 0, type: props.node.data.nodeType,
            name: props.node.data.step.name,
            err: String((e && e.message) || e),
            render: "",
        });
        Message.error("调试失败：" + String((e && e.message) || e));
    } finally {
        debugRunning.value = false;
        exitDebugMode();
        lastDuration.value = Date.now() - startedAt;
    }
}

// ── 调试结果结构化字段 ──
const lastDuration = ref(0);
const lastTrace = computed(() => debugTraces.value[debugTraces.value.length - 1] || null);
const debugText = computed(() => {
    if (!lastTrace.value) return "";
    return lastTrace.value.err ? `✗ ${lastTrace.value.err}` : (lastTrace.value.render || "（无输出）");
});
const hasError = computed(() => !!(lastTrace.value && lastTrace.value.err));
// ── 节点配置 JSON（输入侧） ──
const inputJSON = computed(() => {
    if (!props.node || !props.node.data) return "{}";
    try { return JSON.stringify(props.node.data.step || {}, null, 2); } catch { return "{}"; }
});

// ── 折叠状态 ──
const showInput = ref(false);
function toggleInput() { showInput.value = !showInput.value; }
</script>

<template>
    <div class="fc-debug">
        <div class="fc-debug-title">
            <span>调试</span>
            <span v-if="lastDuration" class="fc-debug-dur">{{ lastDuration }}ms</span>
        </div>
        <div class="fc-debug-actions">
            <button class="fc-dbtn" :disabled="debugRunning" @click="debugNode(true)">
                <IconBug /> 试运行
            </button>
            <button class="fc-dbtn primary" :disabled="debugRunning" @click="debugNode(false)">
                <IconPlayArrow /> 真实调试
            </button>
        </div>

        <!-- 输入侧：节点配置（可折叠） -->
        <button class="fc-debug-toggle" @click="toggleInput">
            {{ showInput ? "▾" : "▸" }} 输入（节点配置）
        </button>
        <pre v-if="showInput" class="fc-debug-pre input">{{ inputJSON }}</pre>

        <!-- 输出侧：调试结果 -->
        <div v-if="lastTrace" class="fc-debug-out">
            <div class="fc-debug-label">输出</div>
            <pre class="fc-debug-pre" :class="{ err: hasError }">{{ debugText }}</pre>
        </div>
        <div v-else-if="!debugRunning" class="fc-debug-empty">点击上方按钮开始调试</div>
        <div v-else class="fc-debug-empty loading">调试中…</div>

        <!-- 变量填写弹窗 -->
        <a-modal
            v-model:visible="varsModal.visible" title="调试变量"
            ok-text="运行" :mask-closable="false" @before-ok="confirmVars"
        >
            <a-form layout="vertical">
                <a-form-item v-for="n in varsModal.names" :key="n" :label="`{${n}}`" required>
                    <a-input v-model="varsModal.values[n]" placeholder="变量值" />
                </a-form-item>
            </a-form>
            <div class="fc-debug-hint">模板中的 {'{var}'} 占位符需要填值。</div>
        </a-modal>
    </div>
</template>

<style scoped>
.fc-debug {
    margin-top: 8px; padding: 10px 12px; border: 1px solid var(--border-weak);
    border-radius: var(--radius-sm); background: var(--bg-inset);
    display: flex; flex-direction: column; gap: 8px;
}
.fc-debug-title {
    display: flex; align-items: center; justify-content: space-between;
    font-size: var(--fs-caption); color: var(--text-2); font-weight: 500;
}
.fc-debug-dur {
    font-size: var(--fs-micro); color: var(--text-3);
    background: var(--bg-card); padding: 1px 7px; border-radius: 8px;
}
.fc-debug-actions { display: flex; gap: 6px; }
.fc-debug-toggle {
    background: transparent; border: none; cursor: pointer;
    color: var(--text-3); font-size: var(--fs-caption); text-align: left; padding: 2px 0;
}
.fc-debug-toggle:hover { color: var(--text-1); }
.fc-debug-pre {
    margin: 0; padding: 8px 10px; border-radius: var(--radius-sm);
    background: var(--bg-card); color: var(--text-1);
    font-size: 11px; font-family: var(--font-mono, monospace);
    white-space: pre-wrap; word-break: break-word; max-height: 200px; overflow-y: auto;
}
.fc-debug-pre.input { color: var(--text-2); }
.fc-debug-pre.err { color: var(--danger); }
.fc-debug-out { display: flex; flex-direction: column; gap: 4px; }
.fc-debug-label { font-size: var(--fs-micro); color: var(--text-3); }
.fc-debug-empty {
    font-size: var(--fs-caption); color: var(--text-3); text-align: center; padding: 8px;
}
.fc-debug-empty.loading { color: var(--primary); }
.fc-dbtn {
    display: inline-flex; align-items: center; gap: 5px;
    background: var(--bg-card); border: 1px solid var(--border-weak); border-radius: var(--radius-sm);
    color: var(--text-2); font-size: var(--fs-caption); padding: 6px 10px; cursor: pointer;
    transition: var(--transition);
}
.fc-dbtn:hover:not(:disabled) { color: var(--primary); border-color: var(--primary); }
.fc-dbtn.primary { color: var(--primary); }
.fc-dbtn:disabled { opacity: 0.45; cursor: not-allowed; }
.fc-debug-hint { font-size: var(--fs-caption); color: var(--text-3); }
</style>
