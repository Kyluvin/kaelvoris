// Kaelvoris 桌面端 Vue3 入口
import { createApp } from "vue";
import ArcoVue from "@arco-design/web-vue";
import ArcoVueIcon from "@arco-design/web-vue/es/icon";
// Arco 基础样式（必须先于自定义覆盖加载，否则组件裸奔：下拉/按钮/输入框无样式）
import "@arco-design/web-vue/dist/arco.css";
import App from "./App.vue";
import { store } from "./stores/app.js";

// 设计系统：令牌优先于组件库覆盖
import "./styles/tokens.css";
import "./styles/arco-overrides.css";
import "./styles/app.css";

import "./bridge/mock-install";

// 布局自适应：DPI 缩放/小窗口下默认收起侧栏，保证中间对话区永远可见不被遮挡。
// 规则：窗口 <1100px 左栏抽屉化；<1500px 右侧浮层抽屉默认收起（需要时再呼出）。
(function applyInitialLayout() {
  const w = window.innerWidth || 0;
  if (w < 1100) store.sidebarCollapsed = true;   // 窄窗：左栏抽屉化，不占位
  if (w < 1500) store.rightbarCollapsed = true;  // 浮层抽屉默认收起
})();

createApp(App).use(ArcoVue).use(ArcoVueIcon).mount("#app");
