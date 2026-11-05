<script setup>
// ═════════════════════════════════════════════════════════════════
// 节点托盘：对标 Dify/Coze 左侧节点库
//   职责：
//     1) 分组展示节点（折叠展开）
//     2) 搜索过滤（按 label 或 type 模糊匹配）
//     3) 拖拽到画布（HTML5 dataTransfer） / 单击落到画布中心
//   emit：
//     - quick-add(type)  单击托盘项
//     - drag-start(type) 拖拽开始
// ═════════════════════════════════════════════════════════════════
import { computed, reactive, ref } from "vue";
import { IconSearch } from "@arco-design/web-vue/es/icon";
import { paletteGroups, getNode } from "./nodeRegistry.js";
import { nodes } from "./useFlowCanvas.js";

const emit = defineEmits(["quick-add", "drag-start"]);

// ── 搜索关键字 ──
const paletteQ = ref("");
// ── 分组折叠状态（默认全展开）──
const paletteOpen = reactive({ struct: true, ai: true, data: true, logic: true, io: true });

// ── 过滤后的分组列表（按搜索词过滤）──
const groups = computed(() => {
    const q = paletteQ.value.trim().toLowerCase();
    return paletteGroups()
        .map((g) => ({
            ...g,
            types: g.types.filter((t) => {
                if (!q) return true;
                const def = getNode(t) || {};
                return (def.label || "").toLowerCase().includes(q) || t.toLowerCase().includes(q);
            }),
        }))
        .filter((g) => g.types.length);
});

/**
 * 拖拽开始：写入 dataTransfer
 * @param {DragEvent} e
 * @param {string} t 节点类型
 */
function onDragStart(e, t) {
    e.dataTransfer.setData("application/x-flow-node", t);
    e.dataTransfer.effectAllowed = "move";
    emit("drag-start", t);
}

/**
 * 单击托盘项：落到画布中心
 * @param {string} t 节点类型
 */
function onClick(t) { emit("quick-add", t); }

/**
 * 折叠/展开分组
 * @param {string} key 分组 key
 */
function toggleGroup(key) { paletteOpen[key] = !paletteOpen[key]; }
</script>

<template>
    <aside class="fc-palette">
        <div class="fc-palette-head">
            <span>节点库</span>
            <span class="fc-palette-count">{{ nodes.length }}</span>
        </div>
        <div class="fc-pal-search">
            <IconSearch />
            <input v-model="paletteQ" placeholder="搜索节点…" />
        </div>
        <div v-for="g in groups" :key="g.key" class="fc-pal-group">
            <button class="fc-pal-group-head" @click="toggleGroup(g.key)">
                <span class="fc-pal-fold">{{ paletteOpen[g.key] ? "▾" : "▸" }}</span>
                <span>{{ g.title }}</span>
                <span class="fc-pal-group-n">{{ g.types.length }}</span>
            </button>
            <template v-if="paletteOpen[g.key]">
                <div
                    v-for="t in g.types" :key="t"
                    class="fc-pal-card" draggable="true"
                    @dragstart="(e) => onDragStart(e, t)"
                    @click="onClick(t)"
                >
                    <span class="fc-pal-dot" :style="{ background: (getNode(t) || {}).color || '#87867F' }"></span>
                    <component :is="(getNode(t) || {}).icon" class="fc-pal-ico" />
                    <span class="fc-pal-name">{{ (getNode(t) || {}).label || t }}</span>
                    <span class="fc-pal-plus">＋</span>
                </div>
            </template>
        </div>
        <div class="fc-palette-hint">拖入或单击添加 · 从节点底部圆点拖线连接 · 线上悬停可插入节点</div>
    </aside>
</template>

<style scoped>
.fc-palette {
    width: 168px; flex: none; padding: var(--gap-sm); overflow-y: auto;
    border-right: 1px solid var(--border-weak); display: flex; flex-direction: column; gap: 6px;
}
.fc-palette-head {
    display: flex; align-items: center; justify-content: space-between;
    font-size: var(--fs-caption); color: var(--text-2); padding-bottom: 2px;
}
.fc-palette-count {
    font-size: var(--fs-micro); color: var(--text-3);
    background: var(--bg-inset); border-radius: 8px; padding: 1px 7px;
}
.fc-pal-search {
    display: flex; align-items: center; gap: 6px;
    background: var(--bg-inset); border: 1px solid transparent; border-radius: var(--radius-sm);
    padding: 4px 8px; color: var(--text-3); margin-bottom: 2px;
    transition: var(--transition);
}
.fc-pal-search:focus-within { border-color: var(--primary); }
.fc-pal-search input {
    flex: 1; min-width: 0; background: transparent; border: none; outline: none;
    color: var(--text-1); font-size: var(--fs-caption);
}
.fc-pal-group { display: flex; flex-direction: column; gap: 4px; }
.fc-pal-group-head {
    display: flex; align-items: center; gap: 5px;
    background: transparent; border: none; cursor: pointer; padding: 3px 2px;
    font-size: var(--fs-micro); color: var(--text-3); text-align: left;
}
.fc-pal-group-head:hover { color: var(--text-1); }
.fc-pal-fold { width: 10px; }
.fc-pal-group-n { margin-left: auto; opacity: 0.7; }
.fc-pal-card {
    display: flex; align-items: center; gap: 8px;
    padding: 7px 10px; border-radius: var(--radius-sm); cursor: grab;
    background: var(--bg-card); border: 1px solid var(--border-weak);
    font-size: var(--fs-caption); color: var(--text-1); transition: var(--transition);
    user-select: none;
}
.fc-pal-card:hover { border-color: var(--primary); background: var(--bg-elevated); transform: translateX(2px); }
.fc-pal-dot { width: 8px; height: 8px; border-radius: 50%; flex: none; }
.fc-pal-ico { color: var(--text-3); flex: none; }
.fc-pal-card:hover .fc-pal-ico { color: var(--primary); }
.fc-pal-name { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.fc-pal-plus { opacity: 0; color: var(--primary); font-size: 12px; transition: var(--transition); }
.fc-pal-card:hover .fc-pal-plus { opacity: 1; }
.fc-palette-hint { margin-top: auto; font-size: var(--fs-micro); color: var(--text-3); line-height: 1.6; padding-top: 8px; }
</style>
