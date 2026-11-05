<script setup>
// ═════════════════════════════════════════════════════════════════
// 根组件：三栏布局壳 + 全局事件装配
//   左：图标轨 rail + 会话导航 sidebar（均可折叠）
//   中：主内容区 main（对话 / Git / 设置抽屉内嵌页）
//   右：工作面板 rightbar（待办 / 上下文，可折叠）
// 2026-10 体验升级（用户需求）：
//   - 会话列表仅唤起时显示：默认收起，Rail「会话」开关或顶栏按钮唤起；
//     选中会话 / 发送首条消息后自动退场（autoCollapseSidebar），聚焦对话区。
//   - 非对话模式（Git/自主迭代/员工/技能库/流程）隐藏会话列表栏：
//     侧栏仅在对话视图存在（v-if），切出对话即退场，不给非对话页占位。
// 2026-12（用户需求：文档放到会话列表下方）：docs 视图侧栏保留 —— 侧栏底部
//     新增「文档」快捷区（文件树导航），点文件切 docs 主视图打开，列表不退场。
// ═════════════════════════════════════════════════════════════════
import { onMounted, ref } from "vue";
import { Message } from "@arco-design/web-vue";
import { onEvent } from "./bridge/wails.js";
	import { store, bindEvents, loadSessions, loadProjects, loadModes, loadModels, loadEmployees, loadRemoteState, loadConfig } from "./stores/app.js";
import RailNav from "./components/RailNav.vue";
import Sidebar from "./components/Sidebar.vue";
import MainChat from "./components/MainChat.vue";
import RightPanel from "./components/RightPanel.vue";
import SettingsDrawer from "./components/SettingsDrawer.vue";

const settingsRef = ref(null);

onMounted(async () => {
  bindEvents();
	  await Promise.all([loadSessions(), loadProjects(), loadModes(), loadModels(), loadEmployees(), loadRemoteState(), loadConfig()]);
  // 全局 toast 消费（后端 kaelvoris:toast 事件；onEvent 为追加式，多订阅共存）
  onEvent("kaelvoris:toast", (msg) => {
    if (msg) Message.info(String(msg));
  });
});

// 会话列表「仅选中时显示」：任务启动后（运行中会话存在）默认收起左栏，
// 避免常驻占宽；Rail/顶栏开关可随时唤起。
if (window.innerWidth >= 1100) store.sidebarCollapsed = true;

function openSettings(page) {
  store.settingsPage = page || "general";
  store.settingsVisible = true;
}
</script>

<template>
  <div
    class="shell"
    :class="{
      'sidebar-collapsed': store.sidebarCollapsed,
      'rightbar-collapsed': store.rightbarCollapsed,
    }"
  >
    <RailNav @open-settings="openSettings" />
    <Sidebar v-if="store.activeMain === 'chat' || store.activeMain === 'docs'" @open-settings="openSettings" />
    <MainChat />
    <!-- 右侧抽屉遮罩：未固定时打开点空白处关闭；固定（pin）后停靠占位无遮罩 -->
    <div
      v-if="!store.rightbarCollapsed && !store.rightbarPinned"
      class="rightbar-mask"
      @click="store.rightbarCollapsed = true"
    ></div>
    <RightPanel />
    <SettingsDrawer ref="settingsRef" />
  </div>
</template>
