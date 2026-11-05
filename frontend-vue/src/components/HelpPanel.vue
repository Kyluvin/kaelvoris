<script setup>
// ════════════════════════════════════════════════════════════════
// 帮助助手（用户需求：点击查看功能使用说明）
//   弹层/抽屉：左分类导航 + 右内容区（搜索 · 条目列表 · 详情）
//   数据源：src/data/help.js（内置内容，分类/搜索/快捷键说明）
//   入口：RailNav 底部「帮助」按钮 + MainChat 顶栏「帮助」按钮
//   关闭：右上 X / 点击遮罩 / Esc
// ════════════════════════════════════════════════════════════════
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { IconClose, IconSearch } from "@arco-design/web-vue/es/icon";
import { HELP_CATEGORIES, HELP_ITEMS, searchHelp } from "../data/help.js";

const props = defineProps({
  visible: { type: Boolean, default: false },
  // 当前使用场景（store.activeMain）：打开时自动定位到对应分类/条目
  scene: { type: String, default: "chat" },
});
const emit = defineEmits(["close", "update:visible"]);

// 场景 → 帮助定位（chat/设置类落到入门，各功能面板落到「功能面板入口」）
const SCENE_MAP = {
  chat: { cat: "start", item: "layout" },
  git: { cat: "panels", item: "panels-overview" },
  roadmap: { cat: "panels", item: "panels-overview" },
  employee: { cat: "panels", item: "panels-overview" },
  skills: { cat: "panels", item: "panels-overview" },
  flow: { cat: "panels", item: "panels-overview" },
  doc: { cat: "panels", item: "panels-overview" },
};

const activeCat = ref("start");
const keyword = ref("");
const activeItem = ref(HELP_ITEMS[0] ? HELP_ITEMS[0].id : "");

// 打开时按当前场景定位（重置搜索词）
watch(
  () => props.visible,
  (v) => {
    if (!v) return;
    const sc = SCENE_MAP[props.scene] || SCENE_MAP.chat;
    activeCat.value = sc.cat;
    activeItem.value = sc.item;
    keyword.value = "";
  }
);

// 当前分类条目（无搜索词时按分类过滤）
const catItems = computed(() => {
  const items = keyword.value ? searchHelp(keyword.value) : HELP_ITEMS;
  return items.filter((it) => it.cat === activeCat.value);
});

// 当前条目（数据驱动，始终有内容）
const curItem = computed(() => {
  return (
    HELP_ITEMS.find((it) => it.id === activeItem.value && it.cat === activeCat.value) ||
    catItems.value[0] ||
    null
  );
});

function selectItem(it) {
  activeItem.value = it.id;
}

function changeCat(cat) {
  activeCat.value = cat;
  activeItem.value = "";
}

function close() {
  emit("update:visible", false);
  emit("close");
}

// Esc 关闭 + 焦点管理（弹层打开时聚焦搜索框）
function onKey(e) {
  if (e.key === "Escape") close();
}
onMounted(() => {
  window.addEventListener("keydown", onKey);
});
onBeforeUnmount(() => {
  window.removeEventListener("keydown", onKey);
});
</script>

<template>
  <!-- 弹层遮罩 -->
  <Transition name="help-fade">
    <div v-if="visible" class="help-mask" @click.self="close">
      <!-- 面板容器 -->
      <div class="help-panel" role="dialog" aria-modal="true" aria-label="帮助">
        <!-- 头部 -->
        <div class="help-head">
          <div class="help-title">帮助</div>
          <div class="help-sub">功能使用说明 · Kaelvoris</div>
          <button class="help-close" aria-label="关闭帮助" title="关闭" @click="close">
            <IconClose />
          </button>
        </div>

        <!-- 搜索 -->
        <div class="help-search">
          <span class="help-search-ico"><IconSearch /></span>
          <input
            v-model="keyword"
            class="help-search-input"
            type="text"
            placeholder="搜索功能 / 快捷键（如：@ 引用文件）"
            aria-label="搜索帮助"
          />
        </div>

        <div class="help-body">
          <!-- 左：分类 -->
          <nav class="help-cats">
            <button
              v-for="c in HELP_CATEGORIES"
              :key="c.key"
              class="help-cat"
              :class="{ active: activeCat === c.key }"
              @click="changeCat(c.key)"
            >
              <span class="help-cat-ico">{{ c.icon }}</span>
              <span class="help-cat-text">
                <span class="help-cat-label">{{ c.label }}</span>
                <span class="help-cat-desc">{{ c.desc }}</span>
              </span>
            </button>
          </nav>

          <!-- 右：内容 -->
          <div class="help-content">
            <!-- 无搜索结果 -->
            <div v-if="!catItems.length" class="help-empty">
              <div class="help-empty-ico">🔍</div>
              <div>没有匹配的结果</div>
              <div class="help-empty-sub">换个关键词，或切换分类试试</div>
            </div>

            <!-- 有内容 -->
            <template v-else>
              <!-- 条目列表 -->
              <div class="help-list">
                <button
                  v-for="it in catItems"
                  :key="it.id"
                  class="help-list-item"
                  :class="{ active: curItem && curItem.id === it.id }"
                  @click="selectItem(it)"
                >
                  <span class="help-list-dot"></span>
                  <span class="help-list-title">{{ it.title }}</span>
                </button>
              </div>

              <!-- 详情 -->
              <div v-if="curItem" class="help-detail">
                <div class="help-detail-title">{{ curItem.title }}</div>
                <div class="help-detail-body">
                  <template v-for="(b, i) in curItem.body" :key="i">
                    <div v-if="b.h" class="help-detail-h">{{ b.h }}</div>
                    <p v-if="b.p" class="help-detail-p">{{ b.p }}</p>
                    <ul v-if="b.list" class="help-detail-list">
                      <li v-for="(l, j) in b.list" :key="j">{{ l }}</li>
                    </ul>
                  </template>
                </div>
              </div>
            </template>
          </div>
        </div>
      </div>
    </div>
  </Transition>
</template>

<style scoped>
/* ── 遮罩 ── */
.help-mask {
  position: fixed;
  inset: 0;
  z-index: 2000;
  background: rgba(0, 0, 0, 0.55);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
}

/* ── 面板 ── */
.help-panel {
  width: min(780px, 100%);
  max-height: min(600px, 90vh);
  display: flex;
  flex-direction: column;
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius-lg);
  box-shadow: var(--shadow-overlay);
  overflow: hidden;
}

/* ── 头部 ── */
.help-head {
  position: relative;
  padding: var(--pad-block) var(--pad-block) 10px;
  border-bottom: 1px solid var(--border-weak);
}
.help-title {
  font-size: var(--fs-title);
  font-weight: 600;
  color: var(--text-1);
}
.help-sub {
  font-size: var(--fs-micro);
  color: var(--text-3);
  margin-top: 2px;
}
.help-close {
  position: absolute;
  top: 14px;
  right: 14px;
  width: 28px;
  height: 28px;
  display: flex;
  align-items: center;
  justify-content: center;
  border: none;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text-2);
  cursor: pointer;
  transition: background var(--transition), color var(--transition);
}
.help-close:hover {
  background: var(--bg-hover);
  color: var(--text-1);
}

/* ── 搜索 ── */
.help-search {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: var(--gap) var(--pad-block);
  padding: 0 10px;
  height: 34px;
  background: var(--bg-inset);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
}
.help-search-ico {
  color: var(--text-3);
  display: flex;
}
.help-search-input {
  flex: 1;
  border: none;
  outline: none;
  background: transparent;
  color: var(--text-1);
  font-size: var(--fs-body);
}
.help-search-input::placeholder {
  color: var(--text-3);
}

/* ── 主体 ── */
.help-body {
  flex: 1;
  min-height: 0;
  display: flex;
  border-top: 1px solid var(--border-weak);
}

/* 分类（左） */
.help-cats {
  width: 200px;
  flex-shrink: 0;
  padding: var(--gap) var(--gap-sm);
  border-right: 1px solid var(--border-weak);
  display: flex;
  flex-direction: column;
  gap: 4px;
  overflow-y: auto;
}
.help-cat {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 10px;
  border: none;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text-2);
  cursor: pointer;
  text-align: left;
  transition: background var(--transition), color var(--transition);
}
.help-cat:hover {
  background: var(--bg-hover);
  color: var(--text-1);
}
.help-cat.active {
  background: var(--bg-active);
  color: var(--primary);
}
.help-cat-ico {
  font-size: 16px;
  flex-shrink: 0;
}
.help-cat-text {
  display: flex;
  flex-direction: column;
}
.help-cat-label {
  font-size: var(--fs-body);
  font-weight: 500;
}
.help-cat-desc {
  font-size: var(--fs-micro);
  color: var(--text-3);
}

/* 内容（右） */
.help-content {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  padding: var(--gap) var(--pad-block);
  overflow-y: auto;
}

/* 条目列表 */
.help-list {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin-bottom: var(--gap);
}
.help-list-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 7px 10px;
  border: none;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text-2);
  cursor: pointer;
  text-align: left;
  transition: background var(--transition), color var(--transition);
}
.help-list-item:hover {
  background: var(--bg-hover);
  color: var(--text-1);
}
.help-list-item.active {
  background: var(--bg-active);
  color: var(--primary);
}
.help-list-dot {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: currentColor;
  opacity: 0.55;
  flex-shrink: 0;
}
.help-list-title {
  font-size: var(--fs-body);
}

/* 详情 */
.help-detail {
  border-top: 1px solid var(--border-weak);
  padding-top: var(--gap);
}
.help-detail-title {
  font-size: var(--fs-title);
  font-weight: 600;
  color: var(--text-1);
  margin-bottom: var(--gap-sm);
}
.help-detail-body {
  font-size: var(--fs-body);
  color: var(--text-2);
  line-height: var(--lh);
}
.help-detail-h {
  font-size: var(--fs-caption);
  color: var(--text-3);
  margin: 8px 0 4px;
}
.help-detail-p {
  margin: 0 0 8px;
}
.help-detail-list {
  margin: 0 0 8px;
  padding-left: 18px;
}
.help-detail-list li {
  margin-bottom: 4px;
}

/* 空态 */
.help-empty {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  color: var(--text-2);
  font-size: var(--fs-body);
  padding: 40px 0;
}
.help-empty-ico {
  font-size: 32px;
  margin-bottom: 10px;
}
.help-empty-sub {
  font-size: var(--fs-caption);
  color: var(--text-3);
  margin-top: 4px;
}

/* 过渡 */
.help-fade-enter-active,
.help-fade-leave-active {
  transition: opacity var(--transition);
}
.help-fade-enter-from,
.help-fade-leave-to {
  opacity: 0;
}

/* 移动端适配（窄窗：分类收窄、面板全屏） */
@media (max-width: 640px) {
  .help-mask {
    padding: 0;
  }
  .help-panel {
    width: 100%;
    max-height: 100vh;
    height: 100vh;
    border-radius: 0;
  }
  .help-cats {
    width: 120px;
  }
  .help-cat-desc {
    display: none;
  }
}
</style>
