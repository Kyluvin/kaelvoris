<script setup>
// 最左图标导航轨：主区切换 + 设置入口（极简、克制，Arco 线性图标）
// 用户需求 2026-10 之 5：新增「数字员工」入口（员工管理面板，中栏切换）。
// 用户需求 2026-10：新增「自主迭代」入口（roadmap 规划板，中栏切换）。
// 2026-10 定时任务：新增「定时任务」入口（cron 面板，中栏切换）。
// 用户需求 2026-11：恢复「会话列表」图标按钮 —— 点击时展开默认隐藏的会话列表
//（已展开则再次点击收起；非对话视图点击 = 切回对话 + 展开列表，一次动作到位）。
// 用户需求 2026-12：去除「对话」图标（对话为默认主视图，由「会话列表」图标兼任回切入口）。
// 用户需求 2026-12 二次调整：① 导航排序固定为 会话列表→工作区→文档→Git→数字员工→技能库
//   →AI Flow→定时任务→插件；② 「自主迭代」从导航移除（融合进数字员工面板，
//   由 EmployeePanel 头部「自主迭代」按钮进入，roadmap 主视图挂载保留）。
	import { IconBranch, IconSettings, IconUser, IconBook, IconNav, IconFile, IconClockCircle, IconApps, IconHighlight, IconFolder } from "@arco-design/web-vue/es/icon";
import { store, expandSidebar } from "../stores/app.js";

const emit = defineEmits(["open-settings"]);

const nav = [
  { key: "__sessions", label: "会话列表", icon: IconNav, toggle: true },
  // 用户需求 2026-12 二次调整：按指定顺序排列（工作区第 2、文档第 3）。
  { key: "workspace", label: "工作区", icon: IconFolder },
  { key: "docs", label: "文档", icon: IconFile },
  { key: "git", label: "Git", icon: IconBranch },
  { key: "employee", label: "数字员工", icon: IconUser },
  // 「资源管理」入口已并入「数字员工」面板（2026-10-25 融合：独立导航项移除，资源以页签形式内嵌）。
  // 「自主迭代」入口已并入「数字员工」面板（2026-12 融合：导航项移除，由面板头部按钮进入）。
  { key: "skills", label: "技能库", icon: IconBook },
  // 「流程编排」（flow/P1）入口已移除（2026-10-25 用户需求：左侧只保留 AI Flow），
  // FlowPanel/FlowCanvas 等组件文件保留但不再从导航可达。
  { key: "cozeflow", label: "AI Flow（内核）", icon: IconHighlight },
  { key: "cron", label: "定时任务", icon: IconClockCircle },
  { key: "plugins", label: "插件", icon: IconApps },
];

function onClick(it) {
  if (it.toggle) {
    // 会话栏开关：列表仅在对话视图存在（非对话模式侧栏已被隐藏且强制收起），
    // 从其他视图点击时：固定切回对话并展开列表（一次动作到位：回对话 + 看列表）；
    // 对话视图内点击：正常翻转折叠状态（点击左侧会话图标 → 展开隐藏的会话列表）。
    // 注：「对话」图标已去除（2026-12），此 toggle 即为非对话视图回对话的入口。
    if (store.activeMain !== "chat") {
      store.activeMain = "chat";
      expandSidebar();
      return;
    }
    if (store.sidebarCollapsed) expandSidebar();
    else store.sidebarCollapsed = true;
    return;
  }
  store.activeMain = it.key;
}
</script>

<template>
  <nav class="rail">
    <div class="rail-logo"><img src="/logo.png" alt="Kaelvoris" /></div>
    <div class="rail-group">
      <button
        v-for="it in nav"
        :key="it.key"
        class="rail-item"
        :class="{ active: it.toggle ? !store.sidebarCollapsed : store.activeMain === it.key }"
        :title="it.label"
        @click="onClick(it)"
      >
        <component :is="it.icon" />
      </button>
    </div>
    <!-- 设置：固定在最左导轨最下方（常驻入口，不随会话栏显隐） -->
    <button class="rail-item rail-settings" title="设置" @click="emit('open-settings', 'general')">
      <IconSettings />
    </button>
  </nav>
</template>

<style scoped>
.rail {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  width: var(--rail-w);
  padding: 10px 0;
  background: var(--bg-page);
  border-right: 1px solid var(--border-weak);
}

		.rail-logo {
		  display: flex;
		  align-items: center;
		  justify-content: center;
		  width: 32px;
		  height: 32px;
		  margin-bottom: 8px;
		  border-radius: var(--radius);
		  overflow: hidden;
			  /* 覆盖 app.css 全局 .rail-logo 的 primary-strong 底色：logo 为透明底 PNG，深色导轨上需白底衬托 */
			  background: #ffffff;
			  box-shadow: var(--ring);
			}
	.rail-logo img {
	  width: 100%; height: 100%;
	  object-fit: cover;
	  display: block;
	}

.rail-item {
  position: relative; /* 激活指示条定位锚点 */
  display: flex;
  align-items: center;
  justify-content: center;
  width: 34px;
  height: 34px;
  border: none;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text-2);
  cursor: pointer;
  transition: background var(--transition), color var(--transition);
}

/* 激活指示条：贴轨道左缘的主色竖条（对齐会话卡 active 的左侧指示） */
.rail-item::before {
  content: "";
  position: absolute;
  /* 按钮在 52px 轨道内水平居中（(52-34)/2=9px），负向偏移回轨道左缘 */
  left: calc((var(--rail-w) - 34px) / -2);
  top: 50%;
  transform: translateY(-50%);
  width: 3px;
  height: 16px;
  border-radius: 2px;
  background: var(--primary-strong);
  opacity: 0;
  transition: opacity var(--transition), height 200ms var(--ease-out);
}

/* 悬浮原生提示太慢且不可样式化：rail 图标改用 CSS tooltip（右向气泡，
   与 Arco Tooltip 体系解耦，无 JS 开销） */
.rail-item::after {
  content: attr(title);
  position: absolute;
  left: calc(100% + 10px);
  top: 50%;
  transform: translateY(-50%) translateX(-4px);
  padding: 4px 10px;
  border-radius: var(--radius-sm);
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  box-shadow: var(--shadow-overlay);
  color: var(--text-1);
  font-size: var(--fs-caption);
  white-space: nowrap;
  opacity: 0;
  pointer-events: none;
  transition: opacity 160ms var(--ease-out), transform 160ms var(--ease-out);
  z-index: 60;
}
.rail-item:hover::after {
  opacity: 1;
  transform: translateY(-50%) translateX(0);
  transition-delay: 320ms;  /* 快扫过轨道时不闪 tooltip（VSCode 同款节奏） */
}
.rail-item.rail-settings:hover::after { transition-delay: 0ms; }

.rail-item:hover {
  background: var(--bg-hover);
  color: var(--text-1);
}

.rail-item.active {
  background: var(--bg-active);
  color: var(--primary);
}
.rail-item.active::before {
  opacity: 1;
  height: 20px;
}

/* 分组（52px 图标轨内用分隔线表达分组，不占导航空间） */
.rail-group {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  width: 100%;
}
.rail-group::before {
  content: "";
  width: 20px;
  height: 1px;
  margin: 2px 0 4px;
  background: var(--border-weak);
}
.rail-settings {
  margin-top: auto;
}
</style>
