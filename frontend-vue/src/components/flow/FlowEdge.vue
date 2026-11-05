<script setup>
// ═════════════════════════════════════════════════════════════════
// 自定义连线：对标 Dify 悬停 + 节点插入 + 运行流光
//   职责：
//     1) SVG 贝塞尔曲线（按 source/target position 自动弯向）
//     2) 悬停显示中点「+」按钮，点击 emit open-edge-menu
//     3) 运行高亮：data.highlight 控制 running/error/idle 状态色 + 流光动画
//     4) 起止端点圆点（悬停显现）
//   设计要点：
//     - 颜色继承 source 节点主色（注册表查询）
//     - 命中区 fc-edge-hit 拓宽点击区，避免连线难以点击
// ═════════════════════════════════════════════════════════════════
import { computed } from "vue";
import { getNode } from "./nodeRegistry.js";

const props = defineProps({
    id: { type: String, required: true },
    source: { type: String, required: true },
    target: { type: String, required: true },
    sourceX: { type: Number, required: true },
    sourceY: { type: Number, required: true },
    targetX: { type: Number, required: true },
    targetY: { type: Number, required: true },
    sourcePosition: { type: String, default: "bottom" },
    targetPosition: { type: String, default: "top" },
    data: { type: Object, default: () => ({}) },
});

const emit = defineEmits(["open-edge-menu"]);

// ── 路径 d 计算：按 source/target position 自动决定控制点偏移 ──
const d = computed(() => {
    const { sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition } = props;
    let sx = 0, sy = 0, tx = 0, ty = 0;
    if (sourcePosition === "left") { sx = -Math.abs(targetX - sourceX) / 2 - 20; sy = 0; }
    else if (sourcePosition === "right") { sx = Math.abs(targetX - sourceX) / 2 + 20; sy = 0; }
    else { sx = 0; sy = Math.abs(targetY - sourceY) / 2 + 20; }
    if (targetPosition === "left") { tx = -Math.abs(targetX - sourceX) / 2 - 20; ty = 0; }
    else if (targetPosition === "right") { tx = Math.abs(targetX - sourceX) / 2 + 20; ty = 0; }
    else { tx = 0; ty = Math.abs(targetY - sourceY) / 2 + 20; }
    return `M ${sourceX},${sourceY} C ${sourceX + sx},${sourceY + sy} ${targetX + tx},${targetY + ty} ${targetX},${targetY}`;
});

// ── 中点坐标（+ 按钮位置）──
const cx = computed(() => (props.sourceX + props.targetX) / 2);
const cy = computed(() => (props.sourceY + props.targetY) / 2);

// ── 颜色：运行高亮优先，否则继承 source 节点主色 ──
const color = computed(() => {
    const hl = (props.data && props.data.highlight) || "";
    if (hl === "error") return "#E06553";
    if (hl === "running") return "#5B8DEF";
    if (hl === "success") return "#3FB968";
    const m = getNode("");  // 不需要查 source 类型，直接返回默认色
    return "#5A5A55";
});

// ── 颜色（继承 source 节点主色；运行时覆盖）──
const lineColor = computed(() => {
    const hl = (props.data && props.data.highlight) || "";
    if (hl === "error") return "#E06553";
    if (hl === "running") return "#5B8DEF";
    if (hl === "success") return "#3FB968";
    return "#5A5A55";
});

/**
 * 中点 + 按钮点击：emit 打开插入节点菜单
 * @param {MouseEvent} e
 */
function onPlus(e) {
    e.stopPropagation();
    emit("open-edge-menu", e, { id: props.id, source: props.source, target: props.target, data: props.data });
}
</script>

<template>
    <g class="fc-edge-g" :class="['hl-' + ((data && data.highlight) || 'idle')]">
        <!-- 命中区（不可见，拓宽点击） -->
        <path class="fc-edge-hit" :d="d" />
        <!-- 可见线 -->
        <path class="fc-edge-path" :d="d" :style="{ stroke: lineColor }" />
        <!-- 起止端点圆点（悬停显现） -->
        <circle class="fc-edge-anchor" :cx="sourceX" :cy="sourceY" r="3.5" :style="{ fill: lineColor }" />
        <circle class="fc-edge-anchor" :cx="targetX" :cy="targetY" r="3.5" :style="{ fill: lineColor }" />
        <!-- 中点 + 按钮（悬停显现） -->
        <g class="fc-edge-plus" :transform="'translate(' + cx + ',' + cy + ')'">
            <circle r="9" @click="onPlus" />
            <path d="M -4.5 0 H 4.5 M 0 -4.5 V 4.5" />
        </g>
    </g>
</template>

<style scoped>
.fc-edge-g:hover .fc-edge-path { stroke-width: 2; }
.fc-edge-hit { fill: none; stroke: transparent; stroke-width: 16; cursor: pointer; }
.fc-edge-path { fill: none; stroke-width: 1.5; transition: stroke-width 0.15s ease; }
.fc-edge-anchor { opacity: 0; transition: opacity 0.15s ease; }
.fc-edge-g:hover .fc-edge-anchor { opacity: 1; }
.fc-edge-plus { opacity: 0; cursor: pointer; transition: opacity 0.15s ease; }
.fc-edge-g:hover .fc-edge-plus { opacity: 1; }
.fc-edge-plus circle { fill: var(--bg-card); stroke: var(--border-weak); }
.fc-edge-plus path { stroke: var(--text-2); stroke-width: 1.4; }
.fc-edge-plus:hover circle { stroke: var(--primary); }
.fc-edge-plus:hover path { stroke: var(--primary); }
.hl-running .fc-edge-path { stroke-dasharray: 6 5; animation: fc-dash 0.6s linear infinite; }
.hl-error .fc-edge-path { stroke-dasharray: 3 3; }
@keyframes fc-dash { to { stroke-dashoffset: -22; } }
</style>
