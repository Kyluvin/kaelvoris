<script setup>
// 右侧工作面板：待办清单 / AI 上下文 / 会话技能 / 会话设置（可整体折叠）
// 会话技能（用户需求 2026-12）：只显用户显式添加进本会话的技能，
// 可配置参数（frontmatter params 驱动动态表单）、可一键运行；
// 全量技能货架仍在左侧入口「技能库」（SkillPanel.vue）。
// 会话设置（用户需求 2026-11）：工作区（会话级绑定/清除）+ 是否禁用 MCP。
// 语义与后端一致：工作区空 = 跟随全局；MCP 禁用随会话持久化，下个任务生效。
// 岗位分配 Phase 3（doc/09-设计方案 §6.1/§8-4）：会话岗位只读徽标展示
//（快照随会话创建/切换员工固化；桌面会话不新增直接绑岗位），员工绑定入口
// 指引到顶栏员工选择器（Composer）。
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { IconRight, IconPushpin } from "@arco-design/web-vue/es/icon";
import { Message } from "@arco-design/web-vue";
import { store, currentTodos, currentSession, setSessionWorkspace, setSessionMCPDisabled } from "../stores/app.js";
import { bridgeCall } from "../bridge/wails.js";
import { shortPath } from "../utils/format.js";
import SessionSkillPanel from "./SessionSkillPanel.vue";

// ── 记忆（用户需求 2026-10-31：双层记忆面板）──
// 会话记忆：当前会话的 MEMORY.md（后端 sessions/<id>.memory.md）；
// 工作空间记忆：项目级 WORKSPACE 跨会话共享（后端 workspace-memory/），
// 每轮交互自动更新「最近交互」节。切到记忆 TAB 懒加载；会话切换刷新。
const memTab = ref("session"); // 记忆 TAB 内子页签：session / workspace
const activeTab = ref("todos"); // 右面板主页签：todos / context / memory / settings（声明须先于下方 watch 引用）
const memContent = ref("");
const memPath = ref("");
const memLoading = ref(false);
const memSaving = ref(false);
const wsMemContent = ref("");
const wsMemPath = ref("");
const wsMemWorkspace = ref("");
const wsMemLoading = ref(false);
const wsMemSaving = ref(false);

async function loadMemory() {
  if (!store.currentId || memLoading.value) return;
  memLoading.value = true;
  try {
    const r = await bridgeCall("GetMemory", store.currentId);
    memContent.value = (r && r.content) || "";
    memPath.value = (r && r.path) || "";
  } catch (e) { Message.error(String(e)); }
  memLoading.value = false;
}

async function saveMemory() {
  if (!store.currentId || memSaving.value) return;
  memSaving.value = true;
  try {
    await bridgeCall("SaveMemory", store.currentId, memContent.value);
    Message.success("会话记忆已保存");
  } catch (e) { Message.error(String(e)); }
  memSaving.value = false;
}

async function loadWsMemory() {
  if (wsMemLoading.value) return;
  wsMemLoading.value = true;
  try {
    const r = await bridgeCall("GetWorkspaceMemory", wsPath.value || "");
    wsMemContent.value = (r && r.content) || "";
    wsMemPath.value = (r && r.path) || "";
    wsMemWorkspace.value = (r && r.workspace) || "";
  } catch (e) { Message.error(String(e)); }
  wsMemLoading.value = false;
}

async function saveWsMemory() {
  if (wsMemSaving.value) return;
  wsMemSaving.value = true;
  try {
    await bridgeCall("SaveWorkspaceMemory", wsMemWorkspace.value || wsPath.value || "", wsMemContent.value);
    Message.success("工作空间记忆已保存（下个任务生效）");
  } catch (e) { Message.error(String(e)); }
  wsMemSaving.value = false;
}

// 懒加载：切到记忆 TAB 时拉取；会话切换/子页签切换时刷新对应层。
watch(activeTab, (tab) => {
  if (tab === "memory") {
    loadMemory();
    loadWsMemory();
  }
});
watch(() => store.currentId, () => {
  if (activeTab.value === "memory") loadMemory();
});
watch(memTab, (t) => { if (activeTab.value === "memory" && t === "workspace") loadWsMemory(); });

// AI 上下文面板可见性上报（P2.7-0b 热路径优化）：面板打开/激活时告知后端，
// 引擎才生成 SayAIContext 全载荷快照帧；关闭/切走时停发（长会话每轮 O(载荷)
// JSON 序列化不再白付）。后端无运行会话时静默忽略。
watch(activeTab, (tab) => {
  store.contextTabActive = tab === "context";
  if (store.currentId) bridgeCall("SetAIContextVisible", store.currentId, tab === "context").catch(() => {});
});
watch(() => store.currentId, (id) => {
  if (id && activeTab.value === "context") bridgeCall("SetAIContextVisible", id, true).catch(() => {});
});
onBeforeUnmount(() => {
  store.contextTabActive = false;
  if (store.currentId) bridgeCall("SetAIContextVisible", store.currentId, false).catch(() => {});
});

const wsPath = computed(() => {
  const s = store.sessionViews.get(store.currentId);
  return (s && s.workspace) || store.workspace || "";
});

// ── 会话设置 ──
// 数据源：store.sessions（后端 kaelvoris:sessions 推送，含 workspace/mcpDisabled）。
// 会话未持久化工作区时展示全局兜底值（与后端 buildRunEngine 的空值回退语义一致）。
const curSess = computed(() => store.sessions.find((x) => x.id === store.currentId) || null);
const sessWorkspace = computed(() => (curSess.value && curSess.value.workspace) || "");
const sessMCPDisabled = computed(() => !!(curSess.value && curSess.value.mcpDisabled));
const sessRunning = computed(() => !!store.running[store.currentId]);
// 岗位快照（Summary.positionId，设计方案 §5.2/§6.1）：只读展示；
// 岗位名字典懒加载（GetOrg 失败静默 = 显示 ID）。
const sessPositionId = computed(() => (curSess.value && curSess.value.positionId) || "");
const posDict = ref({});
let posLoaded = false;
onMounted(async () => {
  if (posLoaded) return;
  posLoaded = true;
  try {
    const view = await bridgeCall("GetOrg");
    const m = {};
    for (const p of ((view && view.positions) || [])) m[p.id] = p.name;
    posDict.value = m;
  } catch { /* 静默：显示 ID 兑底 */ }
});
const sessPosName = computed(() => posDict.value[sessPositionId.value] || "");

async function pickWorkspace() {
  if (!store.currentId) return;
  try {
    const dir = await bridgeCall("PickWorkspace");
    if (!dir) return; // 用户取消
    await setSessionWorkspace(store.currentId, dir);
  } catch (e) { Message.error(String(e)); }
}

async function clearWorkspace() {
  if (!store.currentId) return;
  try { await setSessionWorkspace(store.currentId, ""); } catch (e) { Message.error(String(e)); }
}

async function toggleMCP(val) {
  if (!store.currentId) return;
  try { await setSessionMCPDisabled(store.currentId, val); } catch (e) {
    Message.error(String(e));
    curSess.value.mcpDisabled = !val; // 后端拒绝（如运行中）：回滚开关显示
  }
}
</script>

<template>
  <aside
	    class="rightbar"
	    :class="{ 'is-open': !store.rightbarCollapsed, pinned: store.rightbarPinned }"
	  >
    <div class="rightbar-tabs">
      <button
        class="rightbar-tab"
        :class="{ active: activeTab === 'todos' }"
        @click="activeTab = 'todos'"
      >待办</button>
      <button
        class="rightbar-tab"
        :class="{ active: activeTab === 'context' }"
        @click="activeTab = 'context'"
      >AI 上下文</button>
      <button
	        class="rightbar-tab"
	        :class="{ active: activeTab === 'session' }"
	        @click="activeTab = 'session'"
	      >设置</button>
	      <button
		        class="rightbar-tab"
		        :class="{ active: activeTab === 'skills' }"
		        @click="activeTab = 'skills'"
		      >技能</button>
	      <button
		        class="rightbar-tab"
		        :class="{ active: activeTab === 'memory' }"
		        @click="activeTab = 'memory'"
		      >记忆</button>
	      <span class="spacer"></span>
	      <button
	        class="icon-btn"
	        :title="store.rightbarPinned ? '取消固定（恢复浮层抽屉）' : '固定面板（停靠占位，不遮内容）'"
	        :class="{ 'pin-active': store.rightbarPinned }"
	        @click="store.rightbarPinned = !store.rightbarPinned"
	      >
	        <IconPushpin :class="{ 'pin-rot': store.rightbarPinned }" />
	      </button>
	      <button class="icon-btn" title="折叠面板" @click="store.rightbarCollapsed = true">
	        <IconRight />
	      </button>
    </div>

    <div class="rightbar-body">
      <!-- 待办 -->
      <template v-if="activeTab === 'todos'">
        <div v-if="currentTodos.length">
          <div v-for="(t, i) in currentTodos" :key="i" class="todo-item" :class="{ done: t.status === 'completed' }">
            <span
              class="todo-status-dot"
              :class="t.status === 'completed' ? 'done' : t.status === 'in_progress' ? 'doing' : ''"
            ></span>
            <span class="todo-text">{{ t.content }}</span>
          </div>
        </div>
        <div v-else class="rightbar-empty">
          {{ store.currentId ? "当前会话暂无待办" : "选择或开始一个会话后，这里显示实时待办清单" }}
        </div>
      </template>

      <!-- AI 上下文 -->
      <template v-else-if="activeTab === 'context'">
        <div v-if="store.aiContext" class="ai-context">
          <div class="ctx-hint">最近一次 LLM 请求的完整输入上下文（只读）</div>
          <pre class="ctx-pre">{{ store.aiContext }}</pre>
        </div>
        <div v-else class="rightbar-empty">暂无上下文快照（发起一次任务后生成）</div>
      </template>

	      <!-- 会话技能（2026-12 用户需求：只显已添加进会话的技能，可配置参数、可一键运行） -->
		      <template v-else-if="activeTab === 'skills'">
		        <SessionSkillPanel compact />
		      </template>

		      <!-- 记忆（2026-10-31 双层记忆：会话级 + 项目工作空间级，可查看/编辑） -->
		      <template v-else-if="activeTab === 'memory'">
		        <div class="mem-tabs">
		          <button class="mem-tab" :class="{ active: memTab === 'session' }" @click="memTab = 'session'">会话记忆</button>
		          <button class="mem-tab" :class="{ active: memTab === 'workspace' }" @click="memTab = 'workspace'">工作空间记忆</button>
		        </div>

		        <!-- 会话记忆层：当前会话的 MEMORY.md -->
		        <div v-if="memTab === 'session'" class="mem-layer">
		          <div v-if="!store.currentId" class="rightbar-empty">
		            选择或开始一个会话后，这里显示当前会话的记忆文档
		          </div>
		          <div v-else class="ai-context">
		            <div class="mem-hint">
		              <span>会话级 MEMORY.md（里程碑累积 + 任务收尾；每轮交互引擎自动维护）</span>
		              <span v-if="memPath" class="mem-path" :title="memPath">{{ shortPath(memPath) }}</span>
		            </div>
		            <a-spin v-if="memLoading" class="mem-spin" />
		            <template v-else>
		              <a-textarea
		                v-model="memContent"
		                class="mem-area"
		                :auto-size="{ minRows: 10, maxRows: 24 }"
		                :disabled="sessRunning"
		                spellcheck="false"
		              />
		              <div class="mem-actions">
		                <a-button size="small" type="primary" :loading="memSaving" :disabled="sessRunning" @click="saveMemory">保存</a-button>
		                <span v-if="sessRunning" class="set-state">任务运行中，引擎可能正在更新记忆</span>
		              </div>
		            </template>
		          </div>
		        </div>

		        <!-- 工作空间记忆层：项目级跨会话共享 -->
		        <div v-else class="mem-layer">
		          <div v-if="!wsPath && !wsMemWorkspace" class="rightbar-empty">
		            未设置工作区：先在全局或会话设置中选择项目目录
		          </div>
		          <div v-else class="ai-context">
		            <div class="mem-hint">
		              <span>项目级跨会话记忆（同项目所有会话共享；每轮交互自动更新「最近交互」）</span>
		              <span v-if="wsMemPath" class="mem-path" :title="wsMemPath">{{ shortPath(wsMemWorkspace || wsPath) }}</span>
		            </div>
		            <a-spin v-if="wsMemLoading" class="mem-spin" />
		            <template v-else>
		              <a-textarea
		                v-model="wsMemContent"
		                class="mem-area"
		                :auto-size="{ minRows: 10, maxRows: 24 }"
		                :disabled="sessRunning"
		                spellcheck="false"
		              />
		              <div class="mem-actions">
		                <a-button size="small" type="primary" :loading="wsMemSaving" :disabled="sessRunning" @click="saveWsMemory">保存</a-button>
		                <span class="set-state">同项目所有会话共享，保存后下个任务生效</span>
		              </div>
		            </template>
		          </div>
		        </div>
		      </template>

	      <!-- 会话设置（当前会话独立配置） -->
	      <template v-else>
        <div v-if="!store.currentId" class="rightbar-empty">
          选择或开始一个会话后，这里可设置该会话的工作区与 MCP 开关
        </div>
        <div v-else class="sess-settings">
          <!-- 工作区 -->
          <div class="set-block">
            <div class="set-title">工作区</div>
            <div class="set-desc">本会话固定运行在该目录，多会话并行互不干扰；留空跟随全局当前项目。</div>
            <div class="set-row mono" :title="sessMCPDisabled ? '' : (sessWorkspace || wsPath)">
              {{ shortPath(sessWorkspace || wsPath) || "（未设置）" }}
            </div>
            <div class="set-actions">
              <a-button size="mini" :disabled="sessRunning" @click="pickWorkspace">选择目录…</a-button>
              <a-button v-if="sessWorkspace" size="mini" :disabled="sessRunning" @click="clearWorkspace">清除（跟随全局）</a-button>
            </div>
          </div>

          <!-- 岗位快照（只读；doc/09-设计方案 §6.1：员工岗位自动推导，不新增直绑岗位） -->
          <div v-if="sessPositionId" class="set-block">
            <div class="set-title">岗位分配</div>
            <div class="set-desc">随会话创建/切换员工固化的岗位快照（员工调岗后不漂移）。</div>
            <div class="set-row">
              <a-tag size="small" color="arcoblue">{{ sessPosName || sessPositionId }}</a-tag>
              <span class="set-state">员工绑定经对话输入区选择器切换</span>
            </div>
          </div>

          <!-- MCP 禁用 -->
          <div class="set-block">
            <div class="set-title">禁用 MCP</div>
            <div class="set-desc">开启后本会话不再连接/注册任何 MCP 工具（模型不可见，调用被拒绝）；不影响其他会话与全局配置。变更自下个任务起生效。</div>
            <div class="set-row">
              <a-switch
                size="small"
                :model-value="sessMCPDisabled"
                :disabled="sessRunning"
                @change="toggleMCP"
              />
              <span class="set-state">{{ sessMCPDisabled ? "已禁用（本会话）" : "启用中（跟随全局）" }}</span>
            </div>
          </div>

          <div v-if="sessRunning" class="set-hint">任务运行中，设置暂不可更改。</div>
        </div>
      </template>
    </div>

    <!-- 底部工作区路径 -->
    <div v-if="wsPath" class="rightbar-foot" :title="wsPath">
      <span class="ws-label">工作区</span>
      <span class="ws-path">{{ shortPath(wsPath) }}</span>
    </div>
  </aside>
</template>

<style scoped>
.rightbar-empty {
  color: var(--text-3);
  font-size: var(--fs-caption);
  text-align: center;
  margin-top: 48px;
  padding: 0 16px;
  line-height: 1.8;
}
.ai-context {
  display: flex;
  flex-direction: column;
  gap: 8px;
  height: 100%;
}
.ctx-hint {
  font-size: var(--fs-micro);
  color: var(--text-3);
}
.ctx-pre {
  flex: 1;
  overflow: auto;
  font-family: var(--font-mono);
  font-size: var(--fs-mono);
  color: var(--text-2);
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  padding: 10px;
  margin: 0;
  white-space: pre-wrap;
  word-break: break-all;
}
.rightbar-foot {
  border-top: 1px solid var(--border-weak);
  padding: 8px 16px;
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: var(--fs-micro);
  color: var(--text-3);
}
.ws-label { flex-shrink: 0; }
.ws-path {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* ── 会话设置 ── */
.sess-settings {
  display: flex;
  flex-direction: column;
  gap: 18px;
  padding: 4px 2px;
}
.set-block {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.set-title {
  font-size: var(--fs-caption);
  font-weight: 600;
  color: var(--text-1);
}
.set-desc {
  font-size: var(--fs-micro);
  color: var(--text-3);
  line-height: 1.7;
}
.set-row {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: var(--fs-caption);
  color: var(--text-2);
}
.set-row.mono {
  font-family: var(--font-mono);
  font-size: var(--fs-mono);
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  padding: 6px 10px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.set-state {
  font-size: var(--fs-micro);
  color: var(--text-3);
}
.set-actions {
  display: flex;
  gap: 8px;
}
.set-hint {
  font-size: var(--fs-micro);
  color: var(--warning, #d48806);
  line-height: 1.6;
}

	/* ── 记忆 TAB（2026-10-31 双层记忆）：子页签 + 编辑器 ── */
	.mem-tabs {
	  display: flex;
	  gap: 6px;
	  margin-bottom: 10px;
	}
	.mem-tab {
	  border: 1px solid var(--border-weak);
	  background: transparent;
	  color: var(--text-3);
	  font-size: var(--fs-caption);
	  border-radius: var(--radius-sm);
	  padding: 3px 10px;
	  cursor: pointer;
	}
	.mem-tab.active {
	  color: var(--primary);
	  border-color: var(--primary);
	}
	.mem-layer {
	  height: 100%;
	}
	.mem-hint {
	  display: flex;
	  flex-direction: column;
	  gap: 2px;
	  font-size: var(--fs-micro);
	  color: var(--text-3);
	  margin-bottom: 6px;
	}
	.mem-path {
	  font-family: var(--font-mono);
	  overflow: hidden;
	  text-overflow: ellipsis;
	  white-space: nowrap;
	}
	.mem-spin {
	  display: block;
	  margin: 40px auto;
	}
	.mem-area {
	  font-family: var(--font-mono);
	  font-size: var(--fs-mono);
	}
	.mem-area :deep(textarea) {
	  font-family: var(--font-mono);
	  font-size: var(--fs-mono);
	  background: var(--bg-card);
	}
	.mem-actions {
	  display: flex;
	  align-items: center;
	  gap: 10px;
	  margin-top: 8px;
	}

	/* ── pin 固定按钮：激活态主色 + 斜置 45°（无 Fill 变体图标，用色与形表达状态） ── */
.pin-active {
  color: var(--primary);
}
.pin-active:hover {
  color: var(--primary);
}
.pin-rot {
  transform: rotate(45deg);
}

/* ── 技能 TAB：右栏宽度有限，SessionSkillPanel 加 compact 模式收紧内边距 ── */
	.rightbar-body :deep(.ss-panel.compact) {
	  padding: 0;
	}
</style>
