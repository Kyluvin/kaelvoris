<script setup>
// ═════════════════════════════════════════════════════════════════
// 节点配置抽屉：对标 Dify/Coze 右侧节点配置面板
//   职责：
//     1) AST 字段渲染器（同一套渲染逻辑覆盖所有节点类型）
//     2) 分支节点：列出全部出边，逐条编辑条件表达式
//     3) 节点操作：复制 / 删除
//     4) 内嵌单节点调试器（FlowDebugger.vue）
//   emit：
//     - close  关闭抽屉
//     - remove-node  删除当前节点
//     - duplicate-node  复制当前节点
// ═════════════════════════════════════════════════════════════════
import { computed, ref, watch } from "vue";
import { Message } from "@arco-design/web-vue";
import { IconCopy, IconDelete } from "@arco-design/web-vue/es/icon";
import { bridgeCall } from "../../bridge/wails.js";
import { getNode, getFields } from "./nodeRegistry.js";
import {
    drawer, nodeById, nodes, edges, pushHistory,
} from "./useFlowCanvas.js";
import FlowDebugger from "./FlowDebugger.vue";

const props = defineProps({
    flowId: { type: String, default: "" },   // 透传给内嵌 FlowDebugger
});
const emit = defineEmits(["close", "remove-node", "duplicate-node"]);

// ── 当前节点（响应式从 drawer.nodeId 解析）──
const curNode = computed(() => nodeById(drawer.value.nodeId));
// ── 节点类型 / step ──
const curType = computed(() => (curNode.value && curNode.value.data.nodeType) || "");
const curStep = computed(() => (curNode.value && curNode.value.data.step) || {});
// ── 节点元数据 ──
const curDef = computed(() => getNode(curType.value) || { label: curType.value, color: "#87867F" });
// ── 字段 AST ──
const fields = computed(() => getFields(curType.value));

// ── 工具名列表（tools 节点用）──
const toolNames = ref([]);
async function loadTools() {
    try { toolNames.value = (await bridgeCall("ListFlowTools")) || []; } catch { toolNames.value = []; }
}

// ── llm 节点引用数据源：MCP 服务器名 + 技能列表（multi 多选控件用）──
// mcp 来自 ListFlowMCPServers（手动配置 Enabled 条目 + 已启用插件端点）；
// 技能来自 ListSkills（[]skills.Metadata，展示 name + description）。
const mcpNames = ref([]);
const skillItems = ref([]);
async function loadRefData() {
    try { mcpNames.value = (await bridgeCall("ListFlowMCPServers")) || []; } catch { mcpNames.value = []; }
    try { skillItems.value = (await bridgeCall("ListSkills")) || []; } catch { skillItems.value = []; }
}
// 抽屉打开时预加载（抽屉按需渲染，收起不加载）。
watch(() => drawer.value.visible, (v) => { if (v) loadRefData(); });

/** multi 多选切换：从数组中增删一项（值均为 string）。 */
function toggleInArray(key, val) {
    const cur = Array.isArray(curStep.value[key]) ? curStep.value[key] : [];
    const next = cur.includes(val) ? cur.filter((x) => x !== val) : [...cur, val];
    setField(key, next);
}
/**
 * multi 选项展示名：技能列表展示 description，MCP 直接用服务器名。
 * @param {object} f 字段 AST
 * @param {string} val 选项值
 */
function multiLabel(f, val) {
    if (f.source === "skills") {
        const s = skillItems.value.find((x) => x && x.name === val);
        return s && s.description ? `${val} — ${s.description}` : val;
    }
    return val;
}
/**
 * multi 选项清单：按 f.source 解析为 [{value,label}]；未引用的旧值也保留显示
 * （服务器已删/技能已卸载时仍可见可取消勾选，避免幽灵勾选）。
 * @param {object} f 字段 AST
 */
function multiOptions(f) {
    if (f.source === "mcp") {
        const names = [...mcpNames.value];
        for (const v of (Array.isArray(curStep.value[f.key]) ? curStep.value[f.key] : [])) {
            if (v && !names.includes(v)) names.push(v);
        }
        return names.map((n) => ({ value: n, label: n }));
    }
    const items = [...skillItems.value];
    for (const v of (Array.isArray(curStep.value[f.key]) ? curStep.value[f.key] : [])) {
        if (v && !items.some((s) => s && s.name === v)) items.push({ name: v, description: "" });
    }
    return items.map((s) => ({ value: s.name, label: multiLabel(f, s.name) }));
}
/** 当前字段勾选态（容错：step 值非数组按空处理）。 */
function isChecked(key, val) {
    return Array.isArray(curStep.value[key]) && curStep.value[key].includes(val);
}

/**
 * 设置 step 字段值（入历史 + 响应式更新）
 * @param {string} key 字段名
 * @param {*} v 值
 */
function setField(key, v) {
    const n = curNode.value;
    if (!n) return;
    pushHistory();
    n.data.step = { ...n.data.step, [key]: v };
}

// ── 分支节点：当前出边列表 ──
const curBranchEdges = computed(() => {
    if (!curNode.value || curType.value !== "branch") return [];
    return edges.value.filter((e) => e.source === curNode.value.id);
});
/**
 * 分支出边显示标签：目标节点的显示名 / 类型 label / ID
 * @param {object} e 边
 * @returns {string}
 */
function branchEdgeLabel(e) {
    const t = nodeById(e.target);
    if (!t) return e.target;
    const def = getNode(t.data.nodeType);
    return t.data.step.name || (def && def.label) || e.target;
}
/**
 * 设置边条件表达式
 * @param {object} e 边
 * @param {string} v 表达式
 */
function setEdgeExpr(e, v) {
    pushHistory();
    e.data = { ...(e.data || {}), expr: v };
}

// ── 是否禁止操作的结构/分支节点 ──
const isStructural = computed(() => ["start", "end", "branch"].includes(curType.value));
const isStruct = computed(() => ["start", "end"].includes(curType.value));

// ── 抽屉关闭 ──
function close() { emit("close"); }
</script>

<template>
    <transition name="fc-slide">
        <div v-if="drawer.visible" class="fc-drawer-mask" @click.self="close">
            <aside class="fc-drawer" @keydown.stop>
                <div v-if="curNode" class="fc-drawer-inner">
                    <!-- 抽屉头：色点 + 类型名 + 节点 ID + 关闭 -->
                    <div class="fc-drawer-head" :style="{ '--node-accent': curDef.color }">
                        <span class="fc-drawer-dot"></span>
                        <div class="fc-drawer-tt">
                            <div class="fc-drawer-name">{{ curDef.label || curType }}</div>
                            <div class="fc-drawer-sub">{{ curNode.id }}</div>
                        </div>
                        <button class="fc-drawer-x" title="关闭" @click="close">✕</button>
                    </div>

                    <div class="fc-drawer-body">
                        <!-- 分支节点：出边条件编辑 -->
                        <template v-if="curType === 'branch'">
                            <div class="fc-drawer-tip">
                                按顺序求值各出边条件，首个命中的分支执行；全不命中走「否则」分支（条件留空）。
                                条件可引用变量，如 {'{score}'} &gt; 80。
                            </div>
                            <div v-for="e in curBranchEdges" :key="e.id" class="fc-branch-row">
                                <div class="fc-branch-label">→ {{ branchEdgeLabel(e) }}</div>
                                <input
                                    class="fc-input mono" :value="(e.data && e.data.expr) || ''"
                                    placeholder="条件，如 {score} > 80（留空 = 否则）"
                                    @change="setEdgeExpr(e, $event.target.value)"
                                />
                            </div>
                            <div v-if="!curBranchEdges.length" class="fc-drawer-tip">
                                该分支节点尚无出边：从节点底部圆点拖出连线到各分支（至少 2 条）。
                            </div>
                        </template>

                        <!-- 开始节点：输入变量定义（Dify start 对齐，逗号分隔变量名，运行时即无需从模板反推） -->
                        <template v-else-if="curType === 'start'">
                            <div class="fc-field">
                                <label class="fc-field-label">输入变量名（逗号分隔）</label>
                                <input
                                    class="fc-input mono" :value="curStep.name || ''"
                                    placeholder="如：topic, score（运行时需填这些变量）"
                                    @change="setField('name', $event.target.value)"
                                />
                            </div>
                            <div class="fc-drawer-tip">
                                定义的变量运行时需填值，下游模板/分支条件用 {'{'}变量名{'}'} 引用；
                                留空时由引擎从下游模板反扫（TraceVars 兼容语义）。
                            </div>
                        </template>

                        <!-- 结束节点：输出模板（Dify answer/end 对齐，渲染写回最终输出） -->
                        <template v-else-if="curType === 'end'">
                            <div class="fc-field">
                                <label class="fc-field-label">输出模板（支持 {'{var}'} 与 {'{节点ID.output}'}，留空 = 最后模型/工具输出）</label>
                                <textarea
                                    class="fc-input fc-area" rows="3" :value="curStep.template || ''"
                                    placeholder="如：最终答案：{TR.output}（{name}）"
                                    @change="setField('template', $event.target.value)"
                                ></textarea>
                            </div>
                        </template>

                        <!-- 业务节点：AST 字段渲染 -->
                        <template v-else>
                            <div class="fc-field">
                                <label class="fc-field-label">步骤显示名</label>
                                <input
                                    class="fc-input" :value="curStep.name || ''" :max-length="30"
                                    placeholder="节点显示名" @change="setField('name', $event.target.value)"
                                />
                            </div>
                            <template v-for="f in fields" :key="f.key">
                                <div v-if="f.widget === 'hint'" class="fc-drawer-tip">{{ f.text }}</div>
                                <div v-else class="fc-field">
                                    <label class="fc-field-label">
                                        {{ f.label }}<span v-if="f.required" class="fc-req">*</span>
                                    </label>
                                    <!-- select 渲染 -->
                                    <select
                                        v-if="f.widget === 'select'"
                                        class="fc-input" :value="curStep[f.key] || ''"
                                        @change="setField(f.key, $event.target.value)"
                                    >
                                        <option value="" disabled>请选择…</option>
                                        <option v-for="o in f.options" :key="o.value" :value="o.value">{{ o.label }}</option>
                                    </select>
                                    <!-- 工具选择 -->
                                    <select
                                        v-else-if="f.widget === 'tool'"
                                        class="fc-input" :value="curStep[f.key] || ''"
                                        @change="setField(f.key, $event.target.value)" @focus="loadTools"
                                    >
                                        <option value="" disabled>选择工具…</option>
                                        <option v-for="n in toolNames" :key="n" :value="n">{{ n }}</option>
                                    </select>
                                    <!-- 多选组（llm 节点 MCP 服务器 / 引用技能） -->
                                    <div v-else-if="f.widget === 'multi'" class="fc-multi">
                                        <div v-if="!multiOptions(f).length" class="fc-multi-empty">
                                            {{ f.source === 'mcp' ? '暂无可引用的 MCP 服务器（设置 → MCP 面板添加）' : '暂无可引用的技能（技能库为空）' }}
                                        </div>
                                        <label v-for="o in multiOptions(f)" :key="o.value" class="fc-multi-item">
                                            <input
                                                type="checkbox" class="fc-multi-check"
                                                :checked="isChecked(f.key, o.value)"
                                                @change="toggleInArray(f.key, o.value)"
                                            />
                                            <span class="fc-multi-name" :title="o.label">{{ o.label }}</span>
                                        </label>
                                    </div>
                                    <!-- 单行输入 -->
                                    <input
                                        v-else-if="f.widget === 'input'"
                                        class="fc-input" :value="curStep[f.key] || ''" :placeholder="f.ph"
                                        @change="setField(f.key, $event.target.value)"
                                    />
                                    <!-- 数字输入 -->
                                    <input
                                        v-else-if="f.widget === 'number'"
                                        type="number" class="fc-input" :value="curStep[f.key] ?? 3"
                                        :min="f.min" :max="f.max" :step="f.step || 1"
                                        @change="setField(f.key, Number($event.target.value))"
                                    />
                                    <!-- JSON 代码编辑（transform/http/extractor/iteration 定义体，等宽字体） -->
                                    <textarea
                                        v-else-if="f.widget === 'code'"
                                        class="fc-input fc-area mono" rows="6" spellcheck="false"
                                        :value="curStep[f.key] || ''" :placeholder="f.ph"
                                        @change="setField(f.key, $event.target.value)"
                                    ></textarea>
                                    <!-- 多行文本 -->
                                    <textarea
                                        v-else
                                        class="fc-input fc-area" rows="4" :value="curStep[f.key] || ''" :placeholder="f.ph"
                                        @change="setField(f.key, $event.target.value)"
                                    ></textarea>
                                </div>
                            </template>

                            <!-- 内嵌调试器（仅非结构/分支节点显示） -->
                            <FlowDebugger v-if="!isStructural" :node="curNode" :flow-id="props.flowId" />
                        </template>
                    </div>

                    <!-- 抽屉底栏：操作按钮 -->
                    <div class="fc-drawer-foot">
                        <span class="fc-flex"></span>
                        <button v-if="!isStruct" class="fc-dbtn" @click="emit('duplicate-node')">
                            <IconCopy /> 复制
                        </button>
                        <button v-if="!isStruct" class="fc-dbtn danger" @click="emit('remove-node')">
                            <IconDelete /> 删除
                        </button>
                    </div>
                </div>
            </aside>
        </div>
    </transition>
</template>

<style scoped>
.fc-drawer-mask { position: fixed; inset: 0; z-index: 200; background: rgba(0, 0, 0, 0.32); }
.fc-drawer {
    position: absolute; top: 0; right: 0; bottom: 0; width: 460px; max-width: 92vw;
    background: var(--bg-card); border-left: 1px solid var(--border-weak);
    box-shadow: -12px 0 32px rgba(0, 0, 0, 0.35);
    display: flex; flex-direction: column;
}
.fc-slide-enter-active, .fc-slide-leave-active { transition: opacity 0.18s ease; }
.fc-slide-enter-active .fc-drawer, .fc-slide-leave-active .fc-drawer { transition: transform 0.18s ease; }
.fc-slide-enter-from, .fc-slide-leave-to { opacity: 0; }
.fc-slide-enter-from .fc-drawer, .fc-slide-leave-to .fc-drawer { transform: translateX(30px); }
.fc-drawer-inner { display: flex; flex-direction: column; height: 100%; min-height: 0; }
.fc-drawer-head {
    --node-accent: #87867F;
    display: flex; align-items: center; gap: 10px;
    padding: 14px 16px; border-bottom: 1px solid var(--border-weak); flex: none;
}
.fc-drawer-dot { width: 10px; height: 10px; border-radius: 50%; background: var(--node-accent); flex: none; }
.fc-drawer-tt { flex: 1; min-width: 0; }
.fc-drawer-name { font-size: var(--fs-title); color: var(--text-1); font-weight: 500; }
.fc-drawer-sub { font-size: var(--fs-micro); color: var(--text-3); margin-top: 1px; }
.fc-drawer-x {
    width: 26px; height: 26px; border-radius: var(--radius-sm);
    background: transparent; border: none; color: var(--text-3); cursor: pointer; transition: var(--transition);
}
.fc-drawer-x:hover { color: var(--text-1); background: var(--bg-hover); }
.fc-drawer-body { flex: 1; min-height: 0; overflow-y: auto; padding: 14px 16px; display: flex; flex-direction: column; gap: 12px; }
.fc-drawer-tip { font-size: var(--fs-caption); color: var(--text-3); line-height: 1.6; }
.fc-field { display: flex; flex-direction: column; gap: 5px; }
.fc-field-label { font-size: var(--fs-caption); color: var(--text-2); }
.fc-req { color: var(--danger); margin-left: 3px; }
.fc-input {
    width: 100%; box-sizing: border-box;
    background: var(--bg-inset); border: 1px solid transparent; border-radius: var(--radius-sm);
    color: var(--text-1); font-size: var(--fs-caption); padding: 7px 9px; outline: none;
    transition: var(--transition);
}
.fc-input:focus { border-color: var(--primary); background: var(--bg-card); }
.fc-input.mono { font-family: var(--font-mono, monospace); }
.fc-area { resize: vertical; min-height: 84px; line-height: 1.55; }
textarea.mono { font-family: var(--font-mono, monospace); font-size: var(--fs-caption); }
select.fc-input { appearance: auto; }
/* multi 多选组（checkbox 网格，两列自适应） */
.fc-multi {
    display: grid; grid-template-columns: repeat(auto-fill, minmax(190px, 1fr));
    gap: 6px 10px; padding: 8px 9px;
    background: var(--bg-inset); border-radius: var(--radius-sm);
}
.fc-multi-empty { grid-column: 1 / -1; font-size: var(--fs-micro); color: var(--text-3); padding: 2px 0; }
.fc-multi-item { display: flex; align-items: center; gap: 6px; cursor: pointer; min-width: 0; }
.fc-multi-check { accent-color: var(--primary); flex: none; cursor: pointer; }
.fc-multi-name {
    font-size: var(--fs-caption); color: var(--text-1); line-height: 1.4;
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.fc-branch-row { display: flex; flex-direction: column; gap: 5px; }
.fc-branch-label { font-size: var(--fs-caption); color: var(--text-2); }
.fc-drawer-foot {
    display: flex; align-items: center; gap: 8px; flex-wrap: wrap;
    padding: 12px 16px; border-top: 1px solid var(--border-weak); flex: none;
}
.fc-dbtn {
    display: inline-flex; align-items: center; gap: 5px;
    background: var(--bg-inset); border: 1px solid var(--border-weak); border-radius: var(--radius-sm);
    color: var(--text-2); font-size: var(--fs-caption); padding: 6px 10px; cursor: pointer;
    transition: var(--transition);
}
.fc-dbtn:hover:not(:disabled) { color: var(--primary); border-color: var(--primary); }
.fc-dbtn.danger:hover:not(:disabled) { color: var(--danger); border-color: var(--danger); }
.fc-dbtn:disabled { opacity: 0.45; cursor: not-allowed; }
.fc-flex { flex: 1; }
</style>
