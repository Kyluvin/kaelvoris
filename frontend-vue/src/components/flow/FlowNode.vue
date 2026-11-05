<script setup>
// ═════════════════════════════════════════════════════════════════
// 节点渲染器（Vue Flow 自定义节点）：对标 Dify/Coze 节点卡片
//   职责：
//     1) 4 向连接锚点（top/left 入 · bottom/right 出），自动按节点类型显隐
//     2) 节点卡片视觉：图标 + 类型标签 + 名称 + 副标题摘要
//     3) 运行状态高亮（idle/running/success/error）
//     4) 双击进入抽屉配置（emit open-drawer）
//   设计要点：
//     - 节点类型由 data.nodeType 决定，注册表元数据驱动渲染
//     - 结构节点 start/end 为圆角胶囊样式；业务节点为矩形
// ═════════════════════════════════════════════════════════════════
import { computed } from "vue";
import { Handle, Position } from "@vue-flow/core";
import { getNode, nodeSummary } from "./nodeRegistry.js";
import { statusOf } from "./useFlowCanvas.js";

const props = defineProps({
    id: { type: String, required: true },
    data: { type: Object, required: true },   // { nodeType, step }
    selected: { type: Boolean, default: false },
});

const emit = defineEmits(["open-drawer"]);

// ── 节点元数据（label/color/icon/inputs/outputs）──
const def = computed(() => getNode(props.data.nodeType) || { label: props.data.nodeType, color: "#87867F" });
// ── 状态高亮 class ──
const status = computed(() => statusOf(props.id));
// ── 副标题摘要 ──
const summary = computed(() => nodeSummary(props.data.nodeType, props.data.step || {}));
// ── 是否结构节点 ──
const isStruct = computed(() => ["start", "end"].includes(props.data.nodeType));
// ── 是否分支节点 ──
const isBranch = computed(() => props.data.nodeType === "branch");
// ── 节点 CSS 变量（颜色）──
const accent = computed(() => def.value.color || "#87867F");
// ── 显示名 ──
const displayName = computed(() => {
    const s = props.data.step || {};
    return s.name || def.value.label || props.data.nodeType;
});

/**
 * 双击节点：打开配置抽屉
 */
function onDbl() { emit("open-drawer", props.id); }
</script>

<template>
    <div
        class="fc-node"
        :class="[status, 't-' + data.nodeType, { selected, struct: isStruct, branch: isBranch }]"
        :style="{ '--node-accent': accent }"
        @dblclick="onDbl"
    >
        <!-- 4 向锚点：上入下出 + 左入右出 -->
        <Handle v-if="data.nodeType !== 'start'" type="target" :position="Position.Top" class="fc-handle" />
        <Handle
            v-if="!isStruct && data.nodeType !== 'start'"
            type="target" :position="Position.Left" class="fc-handle"
        />
        <Handle v-if="data.nodeType !== 'end'" type="source" :position="Position.Bottom" class="fc-handle" />
        <Handle
            v-if="!isStruct && data.nodeType !== 'end'"
            type="source" :position="Position.Right" class="fc-handle"
        />

        <!-- 结构节点：胶囊式 -->
        <div v-if="isStruct" class="fc-node-struct">
            <component :is="def.icon" />
            <span>{{ displayName }}</span>
        </div>

        <!-- 业务节点：图标 + 类型 + 名称 + 摘要 -->
        <div v-else class="fc-node-body">
            <div class="fc-node-head">
                <component :is="def.icon" class="fc-node-ico" />
                <div class="fc-node-tt">
                    <div class="fc-node-type">{{ def.label }}</div>
                    <div class="fc-node-name">{{ displayName }}</div>
                </div>
            </div>
            <div v-if="summary" class="fc-node-sub">{{ summary }}</div>
            <div v-if="isBranch" class="fc-node-sub fc-node-branch-hint">按出边条件路由（双击编辑）</div>

            <!-- 输入/输出端口徽标（让用户一眼看到 IO 约束） -->
            <div v-if="def.inputs && def.inputs.length" class="fc-node-ports">
                <span class="fc-port in">in: {{ def.inputs.join(", ") }}</span>
                <span v-if="def.outputs && def.outputs.length" class="fc-port out">out: {{ def.outputs.join(", ") }}</span>
            </div>
        </div>
    </div>
</template>

<style scoped>
.fc-node {
    --node-accent: #87867F;
    width: 160px; min-height: 48px; padding: 9px 11px 10px;
    background: var(--bg-card); border: 1px solid var(--border-weak);
    border-left: 3px solid var(--node-accent);
    border-radius: 10px; cursor: pointer;
    box-shadow: 0 1px 3px rgba(0, 0, 0, 0.18);
    transition: box-shadow 0.18s ease, border-color 0.18s ease, transform 0.18s ease;
}
.fc-node:hover { border-color: var(--node-accent); box-shadow: 0 4px 14px rgba(0, 0, 0, 0.28); transform: translateY(-1px); }
.fc-node.selected { border-color: var(--primary); box-shadow: 0 0 0 2px rgba(91, 141, 239, 0.25); }
.fc-node.running { border-color: #5B8DEF; box-shadow: 0 0 0 2px rgba(91, 141, 239, 0.35); }
.fc-node.success { border-color: #3FB968; }
.fc-node.error { border-color: #E06553; box-shadow: 0 0 0 2px rgba(224, 101, 83, 0.3); }

.fc-node-head { display: flex; align-items: center; gap: 8px; min-width: 0; }
.fc-node-ico { color: var(--node-accent); flex: none; }
.fc-node-tt { flex: 1; min-width: 0; }
.fc-node-type {
    font-size: 10px; letter-spacing: 0.05em; color: var(--text-3);
    text-transform: uppercase;
}
.fc-node-name {
    font-size: var(--fs-caption); font-weight: 500; color: var(--text-1);
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.fc-node-sub {
    margin-top: 5px; font-size: 10px; color: var(--text-3); line-height: 1.45;
    overflow: hidden; display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical;
    word-break: break-all;
}
.fc-node-branch-hint { color: #5B8DEF; opacity: 0.85; }

.fc-node-ports {
    display: flex; flex-wrap: wrap; gap: 4px; margin-top: 5px;
    font-size: 9px; color: var(--text-3);
}
.fc-port { padding: 1px 5px; border-radius: 3px; background: var(--bg-inset); }
.fc-port.in { color: #5B8DEF; }
.fc-port.out { color: #3FB968; }

.fc-node-struct {
    display: flex; align-items: center; justify-content: center; gap: 6px;
    color: var(--text-2); font-size: var(--fs-caption);
}
.fc-node.struct { border-radius: 999px; text-align: center; }
.fc-node.t-start { border-left-color: #3FB968; }
.fc-node.t-end { border-left-color: #E06553; }

/* 连接锚点 */
.fc-handle {
    width: 9px; height: 9px; background: var(--bg-card);
    border: 2px solid var(--node-accent, #87867F);
    transition: transform 0.15s ease, box-shadow 0.15s ease;
}
.fc-handle:hover { transform: scale(1.5); box-shadow: 0 0 0 4px rgba(91, 141, 239, 0.18); }
</style>
