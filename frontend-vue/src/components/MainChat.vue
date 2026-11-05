<script setup>
// 中间主区：顶栏（标题/用量/布局折叠）+ 消息流 + 审批卡 + 输入区
// 模式/模型下拉已移至输入区 Composer（停止/发送按钮左侧）
import { computed, nextTick, ref, watch } from "vue";
import { IconMenuFold, IconMenuUnfold, IconUp, IconDown, IconUser, IconDoubleUp, IconDoubleDown, IconFolder, IconBook, IconCode } from "@arco-design/web-vue/es/icon";
import { answerAsk, store, currentMessages, expandSidebar } from "../stores/app.js";
import { fmtTok } from "../utils/format.js";
import RunningTasks from "./RunningTasks.vue";
import MessageItem from "./MessageItem.vue";
import Composer from "./Composer.vue";
import GitPanel from "./GitPanel.vue";
import EmployeePanel from "./EmployeePanel.vue";
// FlowPanel（旧「流程编排」P1）入口已移除（2026-10-25 用户需求：左侧只保留 AI Flow），
// 组件文件保留但不再挂载；AI Flow（cozeflow）由 CozeFlowPanel 承接。
import CozeFlowPanel from "./CozeFlowPanel.vue";
import SkillPanel from "./SkillPanel.vue";
import RoadmapPanel from "./RoadmapPanel.vue";
import DocPanel from "./DocPanel.vue";
	import CronPanel from "./CronPanel.vue";
		import PluginPanel from "./PluginPanel.vue";
		import WorkspacePanel from "./WorkspacePanel.vue";

const messagesEl = ref(null);
const askInputText = ref("");

const curTitle = computed(() => {
  const s = store.sessions.find((x) => x.id === store.currentId);
  return s ? (s.title || "未命名会话") : "新对话";
});

const askBadgeMap = {
  tool: "工具审批", command: "命令审批", followup: "提问",
  api_req_failed: "请求失败", mistake_limit_reached: "错误上限",
  auto_approval_max_req_reached: "审批上限", resume_task: "恢复任务",
};
	const askBadgeText = computed(() => askBadgeMap[(store.pendingAsk || {}).ask] || "确认");
	const askBadgeCls = computed(() =>
	  ["api_req_failed", "mistake_limit_reached", "auto_approval_max_req_reached"]
	    .includes((store.pendingAsk || {}).ask) ? "danger" : "info");
// usage 徽标：会话级 token/cost 计量（Claude Code /cost 对齐；估算部分标注 ~）
const usageLabel = computed(() => {
  const u = store.usage;
  if (!u || (!u.requests && !u.total_tokens)) return "";
  let s = `↑${fmtTok(u.prompt_tokens)} ↓${fmtTok(u.completion_tokens)}`;
  if (u.total_tokens) s += ` Σ${fmtTok(u.total_tokens)}`;
  if (u.requests) s += ` · ${u.requests}轮`;
  if (u.cost_usd) s += ` · $${u.cost_usd < 0.01 ? u.cost_usd.toFixed(4) : u.cost_usd.toFixed(2)}`;
  if (u.estimated_tokens > 0) s += "（部分估算）";
  return s;
});

const needTextInput = computed(() => (store.pendingAsk || {}).ask === "followup");

function answer(resp) {
  answerAsk(resp, askInputText.value);
  askInputText.value = "";
}

// 模式/模型切换已移至 Composer 输入区；此处仅保留顶栏布局控制

// 自动贴底滚动（用户上滚超过一屏则暂停跟随）
let stickBottom = true;
function onScroll() {
  const el = messagesEl.value;
  if (!el) return;
  stickBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
}

// 悬浮功能（2026-10 用户需求）：聊天区右上/右下圆形箭头，一键滚到历史最上 / 最新最下
function scrollToTopOfChat() {
  const el = messagesEl.value;
  if (!el) return;
  stickBottom = false; // 主动上滚即退出贴底跟随
  el.scrollTop = 0;
}
function scrollToBottomOfChat() {
  const el = messagesEl.value;
  if (!el) return;
  stickBottom = true; // 回到底部，恢复贴底跟随
  el.scrollTop = el.scrollHeight;
}
// 悬浮功能（2026-10-25 用户需求）：上/下按钮中间的「定位用户会话」按钮——
// 一键滚动到当前会话最后一条用户消息（长会话中快速找回自己的提问位置）。
// 定位算法与 offsetParent 无关：用 getBoundingClientRect 相对差值换算滚动内容中的绝对位置，
// 让目标消息落在可视区上 1/3 处（上下文前瞻更自然）。
function scrollToUserMessage() {
  const el = messagesEl.value;
  if (!el) return;
  const users = el.querySelectorAll(".msg-user");
  const target = users[users.length - 1];
  if (!target) return;
  const elRect = el.getBoundingClientRect();
  const tRect = target.getBoundingClientRect();
  const absTop = el.scrollTop + (tRect.top - elRect.top); // 目标在滚动内容中的绝对位置
  el.scrollTop = Math.max(0, absTop - el.clientHeight / 3);
  // 定位后是否保持贴底跟随：已接近底部则跟随，否则退出（与手动上滚同语义）
  stickBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
}

// 用户消息导航（2026-12 用户需求）：双箭头上/下，定位上一条/下一条用户消息气泡。
// 以视口顶部为参照（对齐编辑器「上一处/下一处」语义）：已定位在顶部的用户消息
// 不再命中，继续找它前/后一条；用 getBoundingClientRect 计算视差，不依赖 offsetParent。
// 容差必须 > 定位偏移 8px：否则向上定位到 elTop+8 的消息在向下方向仍满足
// 「> elTop+容差」，被重复命中 → 增量恒为 0，向下按钮卡死无反应（实测 BUG）。
function scrollUserMsg(dir) {
  const el = messagesEl.value;
  if (!el) return;
  const elTop = el.getBoundingClientRect().top;
  const nodes = [...el.querySelectorAll(".msg-user")];
  if (!nodes.length) return;
  const target = dir < 0
    ? nodes.filter((n) => n.getBoundingClientRect().top < elTop - 10).pop()
    : nodes.find((n) => n.getBoundingClientRect().top > elTop + 10);
  if (!target) return;
  stickBottom = false; // 主动导航即退出贴底跟随（滚到真底部时 onScroll 自动恢复）
  el.scrollTop += target.getBoundingClientRect().top - elTop - 8;
}
// 自动贴底触发器（O(1) 化）：旧实现每条 partial 都重建全量消息指纹串
// join（流式长会话每 chunk O(全部消息)）；实际贴底只需感知「新消息追加」
// 与「末条消息增长」两个信号——末条 id + 文本长度即足够（中间消息不再
// 变化；工具/思考卡追加也会改变末条）。
watch(
  () => {
    const list = currentMessages.value;
    if (!list.length) return "";
    const last = list[list.length - 1];
    return list.length + ":" + last._id + ":" + (last.text || "").length;
  },
  async () => {
    await nextTick();
    const el = messagesEl.value;
    if (el && stickBottom) el.scrollTop = el.scrollHeight;
  },
);
</script>

<template>
  <main class="main">
    <div class="main-topbar">
      <button
        v-if="store.activeMain === 'chat'"
        class="icon-btn"
        :title="store.sidebarCollapsed ? '展开侧栏' : '折叠侧栏'"
        @click="store.sidebarCollapsed ? expandSidebar() : (store.sidebarCollapsed = true)"
      >
        <IconMenuUnfold v-if="store.sidebarCollapsed" />
        <IconMenuFold v-else />
      </button>
      <span class="main-title">{{ curTitle }}</span>
	      <!-- 运行任务徽标（用户需求：点击弹出运行任务列表，切工作区并定位会话；无运行任务自隐藏） -->
	      <RunningTasks />
	      <span v-if="usageLabel" class="usage-badge" :title="'会话累计 token 用量（Claude Code /cost 对齐）'">{{ usageLabel }}</span>
      <div class="topbar-ctl">
        <button
          class="icon-btn"
          :title="store.rightbarCollapsed ? '展开工作面板' : '折叠工作面板'"
          @click="store.rightbarCollapsed = !store.rightbarCollapsed"
        >
          <IconMenuFold v-if="!store.rightbarCollapsed" />
          <IconMenuUnfold v-else />
        </button>
      </div>
    </div>

    <div class="main-body">
      <template v-if="store.activeMain === 'chat'">
        <div class="chat-scroll-wrap">
        <div ref="messagesEl" class="messages" @scroll="onScroll">
          <div class="messages-inner">
            <template v-if="currentMessages.length">
              <MessageItem v-for="m in currentMessages" :key="m._id" :msg="m" />
            </template>
            <div v-else class="empty-state">
              <div class="empty-logo"><img src="/logo.png" alt="Kaelvoris" /></div>
              <div class="empty-title">今天要做点什么？</div>
              <div class="empty-sub">描述你的任务，Kaelvoris 来完成</div>
              <div class="empty-suggest">
                <button class="empty-chip" @click="store.inputText = '总结当前项目的目录结构'"><IconFolder class="chip-icon" />总结当前项目的目录结构</button>
                <button class="empty-chip" @click="store.inputText = '阅读 README.md 并给出改进建议'"><IconBook class="chip-icon" />阅读 README.md 并给出改进建议</button>
                <button class="empty-chip" @click="store.inputText = '帮我把这个模块重构为更清晰的分层'"><IconCode class="chip-icon" />帮我把这个模块重构为更清晰的分层</button>
              </div>
              <div class="empty-hint-row">
                <span class="kbd">Enter</span> 发送 · <span class="kbd">@</span> 引用文件 · <span class="kbd">双 ESC</span> 回溯草稿
              </div>
            </div>
          </div>
        </div>
        <!-- 悬浮滚动按钮：顶部“回到最上”，中部“定位用户会话”，底部“回到最下”（仅消息流可滚动时显示）-->
        <button
          v-if="currentMessages.length"
          class="chat-nav chat-nav-top"
          title="回到最上"
          aria-label="回到最上"
          @click="scrollToTopOfChat"
        >
          <IconUp />
        </button>
        <!-- 定位用户会话（2026-10-25 用户需求）：一键跳到当前会话最后一条用户消息 -->
        <button
          v-if="currentMessages.length"
          class="chat-nav chat-nav-user"
          title="定位用户消息（最近一条提问）"
          aria-label="定位用户消息"
          @click="scrollToUserMessage"
        >
          <IconUser />
        </button>
        <button
          v-if="currentMessages.length"
          class="chat-nav chat-nav-user-up"
          title="上一条我的消息"
          aria-label="上一条我的消息"
          @click="scrollUserMsg(-1)"
        >
          <IconDoubleUp />
        </button>
        <button
          v-if="currentMessages.length"
          class="chat-nav chat-nav-user-down"
          title="下一条我的消息"
          aria-label="下一条我的消息"
          @click="scrollUserMsg(1)"
        >
          <IconDoubleDown />
        </button>
        <button
          v-if="currentMessages.length"
          class="chat-nav chat-nav-bottom"
          title="回到最下"
          aria-label="回到最下"
          @click="scrollToBottomOfChat"
        >
          <IconDown />
        </button>
        </div>

        <!-- 审批卡（仅当前会话有挂起 ask 时显示） -->
        <div v-if="store.pendingAsk" class="ask-card">
          <span class="ask-badge" :class="askBadgeCls">{{ askBadgeText }}</span>
          <div class="ask-text">{{ store.pendingAsk.text }}</div>
          <div class="ask-actions">
            <template v-if="needTextInput">
              <a-input
                v-model="askInputText"
                size="small"
                placeholder="输入回答，Enter 发送…"
                @keyup.enter="answer('yes')"
              />
            </template>
            <template v-else>
              <a-button size="small" type="primary" @click="answer('yes')">批准</a-button>
              <a-button size="small" @click="answer('no')">拒绝</a-button>
            </template>
          </div>
        </div>

        <Composer />
      </template>

      <GitPanel v-else-if="store.activeMain === 'git'" />

      <!-- 自主迭代规划板（用户需求 2026-10：自主开发/自主迭代/自主规划） -->
      <RoadmapPanel v-else-if="store.activeMain === 'roadmap'" />

      <!-- 数字员工管理（用户需求 2026-10 之 5：PC 端左侧入口） -->
      <EmployeePanel v-else-if="store.activeMain === 'employee'" />

      <!-- 技能库（列表/查看/编辑/新建/删除；与移动端技能库同源同链路） -->
      <SkillPanel v-else-if="store.activeMain === 'skills'" />

      <!-- AI Flow（嵌入式 coze 内核：工作流创建/发布/同步执行）；
           旧「流程编排」flow 入口已移除（2026-10-25 用户需求），仅保留 cozeflow -->
      <CozeFlowPanel v-else-if="store.activeMain === 'cozeflow'" />

      <!-- 文档面板（用户需求 2026-10：左侧文档图标，查看 doc 目录文档） -->
      <DocPanel v-else-if="store.activeMain === 'docs'" />

	      <!-- 定时任务管控面板（用户需求 2026-12：左侧定时任务入口） -->
	      <CronPanel v-else-if="store.activeMain === 'cron'" />

				      <!-- 插件面板（用户需求 2026-12：标准插件 + 独立管理面板） -->
				      <PluginPanel v-else-if="store.activeMain === 'plugins'" />

				      <!-- 工作区面板（用户需求 2026-11：任意文件夹浏览/编辑 + 执行关联会话 + README 注入与日志回写） -->
				      <WorkspacePanel v-else-if="store.activeMain === 'workspace'" />
		      <!-- 资源管理已并入 EmployeePanel 资源页签（2026-10-25 融合，resources 主视图移除） -->
		   </div>
  </main>
</template>

<style scoped>
.usage-badge {
  font-family: var(--font-mono);
  font-size: var(--fs-micro);
  color: var(--text-2);
  margin-left: var(--gap-sm);
  padding: 2px 10px;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: 999px;
  flex-shrink: 0;
  white-space: nowrap;
  font-variant-numeric: tabular-nums;
  letter-spacing: 0.01em;
}
.chip-icon {
  width: 14px;
  height: 14px;
  color: var(--text-3);
  transition: color var(--transition);
}
.empty-chip:hover .chip-icon { color: var(--primary); }
	.ask-actions {
	  display: flex;
	  gap: var(--gap-sm);
	  margin-top: var(--gap);
	  align-items: center;
	}
</style>
