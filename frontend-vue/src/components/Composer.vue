<script setup>
// 输入区：多行输入 + 图片附件 + 模式/模型切换 + ACP agent 选择 + 发送/停止 + 排队提示
// ACP agent 选择器：绑定外部编码 agent（Claude Code / Gemini CLI 等）后，
// 当前会话任务经 ACP 协议派发（空 = 内置引擎）。
// Claude Code 对齐增强：
//   @ 文件补全 —— 输入 "@" 触发工作区文件候选，↑↓ 选择 / Enter/Tab 补全 / ESC 关闭；
//   双 ESC 回溯 —— 800ms 内连按两次 ESC：清空当前输入草稿（再按可恢复，栈式回溯）。
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from "vue";
import { IconSend, IconImage, IconDelete, IconPauseCircle, IconClockCircle, IconRobot, IconThunderbolt } from "@arco-design/web-vue/es/icon";
import { Message } from "@arco-design/web-vue";
import { bridgeCall } from "../bridge/wails.js";
import { store, sendCurrent, stopSession, loadACPAgents, switchACP, loadEmployees, switchEmployee, loadRemoteState } from "../stores/app.js";

onMounted(() => {
  loadACPAgents();
  loadEmployees();
  loadRemoteState();
  autosize(); // 恢复草稿后按内容定高
  // 图片粘贴（Ctrl+V 截图/复制的图片）：挂 window 级，焦点不在输入框也能粘；
  // 卸载时移除，避免重复注册。
  window.addEventListener("paste", onWindowPaste);
});
onUnmounted(() => window.removeEventListener("paste", onWindowPaste));

const inputEl = ref(null);
const isRunning = computed(() => !!(store.currentId && store.running[store.currentId]));

// ── textarea 自适应增高（规划板 2026-10）─────────────────────────
// 高度随内容增长，超过 CSS max-height 上限后转内部滚动（overflow-y:auto）；
// 输入/草稿恢复/双 ESC 恢复均经 watch 触发重算。scrollHeight 收缩时回退
// min-height 再测量，保证删除内容后高度能收回。
function autosize() {
  const el = inputEl.value;
  if (!el) return;
  el.style.height = "auto";
  el.style.height = el.scrollHeight + "px";
  const overflow = el.scrollHeight > el.clientHeight + 1;
  el.style.overflowY = overflow ? "auto" : "hidden";
}
watch(() => store.inputText, () => nextTick(autosize));
const queuedCount = computed(() => {
  const sv = store.sessionViews.get(store.currentId);
  return sv ? sv.queuedCount : 0;
});

// ── @ 文件补全（Claude Code 对齐）───────────────────────────────
const atOpen = ref(false);        // 补全弹层开关
const atQuery = ref("");          // @ 后的过滤词
const atStart = ref(-1);          // @ 触发位置（textarea 光标下标）
const atItems = ref([]);          // 候选清单（相对路径）
const atIdx = ref(0);             // 当前高亮下标
let filesCache = null;            // 工作区文件清单缓存（切项目失效）

function refreshFilesCache() {
  filesCache = null;
  bridgeCall("ListWorkspaceFiles").then((l) => { filesCache = l || []; }).catch(() => {});
}

function filterAt(q) {
  const all = filesCache || [];
  const needle = (q || "").toLowerCase();
  const starts = [], contains = [];
  for (const p of all) {
    const lp = p.toLowerCase();
    const base = lp.split("/").pop();
    if (base.startsWith(needle)) starts.push(p);
    else if (lp.includes(needle)) contains.push(p);
    if (starts.length >= 50) break;
  }
  return starts.concat(contains).slice(0, 50);
}

function onInput() {
  const el = inputEl.value;
  if (!el) return;
  const pos = el.selectionStart;
  const text = store.inputText.slice(0, pos);
  const m = text.match(/(^|\s)@([^\s@]*)$/); // 行首或空白后的 @ 开始扫描
  if (m) {
    if (!filesCache) refreshFilesCache();
    atStart.value = pos - m[2].length - 1;
    atQuery.value = m[2];
    atItems.value = filterAt(m[2]);
    atIdx.value = 0;
    atOpen.value = atItems.value.length > 0;
  } else {
    atOpen.value = false;
  }
}

function applyAt(p) {
  if (atStart.value < 0) return;
  const el = inputEl.value;
  const pos = el ? el.selectionStart : store.inputText.length;
  const before = store.inputText.slice(0, atStart.value);
  const after = store.inputText.slice(pos);
  store.inputText = before + "@" + p + (after.startsWith(" ") || after === "" ? " " : " ") + after.replace(/^\s+/, "");
  atOpen.value = false;
  atStart.value = -1;
  requestAnimationFrame(() => { el && el.focus(); });
}

function atKeydown(e) {
  if (!atOpen.value) return false;
  if (e.key === "ArrowDown") { e.preventDefault(); atIdx.value = (atIdx.value + 1) % atItems.value.length; return true; }
  if (e.key === "ArrowUp") { e.preventDefault(); atIdx.value = (atIdx.value - 1 + atItems.value.length) % atItems.value.length; return true; }
  if (e.key === "Enter" || e.key === "Tab") {
    e.preventDefault();
    applyAt(atItems.value[atIdx.value]);
    return true;
  }
  if (e.key === "Escape") { atOpen.value = false; return true; }
  return false;
}

// ── 双 ESC 回溯（Claude Code 双 ESC 回退对话对齐，GUI 形态：草稿栈）────
const draftStack = ref([]);   // 被回溯清掉的草稿（后进先出，可恢复）
let lastEscTs = 0;

function onKeydown(e) {
  // @ 补全弹层打开时，↑↓/Enter/Tab 优先服务补全
  if (atKeydown(e)) {
    if (e.key === "Escape") return;
    return;
  }
  // 双 ESC 回溯：800ms 内第二次 ESC 恢复草稿，第一次入栈清空
  if (e.key === "Escape") {
    const now = Date.now();
    if (now - lastEscTs < 800) {
      // 第二次 ESC：弹出上一份草稿（如有）
      if (draftStack.value.length) {
        store.inputText = draftStack.value.pop();
        e.preventDefault();
      }
      lastEscTs = 0;
    } else {
      // 第一次 ESC：当前草稿入栈并清空（空输入不动作）
      if (store.inputText.trim()) {
        draftStack.value.push(store.inputText);
        if (draftStack.value.length > 20) draftStack.value.shift();
        store.inputText = "";
        e.preventDefault();
        Message.info({ content: "已清空（双 ESC 可恢复草稿）", duration: 1800 });
      }
      lastEscTs = now;
    }
    return;
  }
  // Enter 发送 / Shift+Enter 换行（IME 组合输入期间不触发）；
  // 发送中（sending）禁止重复提交（需求：发送期间不可重复提交）
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    if (!store.sending) sendCurrent();
  }
}

// 单张图片入待发队列：>8MB 提示丢弃（与原 pickImages 同一限制）；返回是否为图片文件
function addImageFile(f) {
  if (!f || !f.type || !f.type.startsWith("image/")) return false;
  if (f.size > 8 * 1024 * 1024) {
    Message.warning("图片过大（>8MB）：" + (f.name || "剪贴板图片"));
    return true;
  }
  const reader = new FileReader();
  reader.onload = () => store.pendingImages.push(String(reader.result || ""));
  reader.readAsDataURL(f);
  return true;
}

async function pickImages() {
  const input = document.createElement("input");
  input.type = "file";
  input.accept = "image/png,image/jpeg,image/webp,image/gif";
  input.multiple = true;
  input.onchange = () => {
    for (const f of [...(input.files || [])]) addImageFile(f);
  };
  input.click();
}

	// 粘贴图片 → 加入待发附件（截图工具 / 网页复制图片均可）。
	// window 级监听：焦点不在输入框也能粘；但排除其他文本输入框/可编辑区
	//（设置抽屉、表单等处保持原生粘贴语义，不抢事件）。
	// 图文混合（用户需求 2026-12）：剪贴板同时含文字与图片时二者都保留——
	// 文字进输入框（追加到光标处/末尾），图片进待发附件队列。
	function onWindowPaste(e) {
	  const t = e.target;
	  if (t && t !== inputEl.value && (t.tagName === "TEXTAREA" || t.tagName === "INPUT" || t.isContentEditable)) return;
	  const cd = (e.clipboardData && e.clipboardData.items) ? e.clipboardData : null;
	  const items = cd ? cd.items : [];
	  const files = [];
	  for (const it of items) {
	    if (it.kind === "file" && it.type && it.type.startsWith("image/")) {
	      const f = it.getAsFile();
	      if (f) files.push(f);
	    }
	  }
	  if (!files.length) return; // 纯文本/无图片：走默认粘贴
	  e.preventDefault();
	  let added = 0;
	  for (const f of files) { if (addImageFile(f)) added++; }
	  // 图文混合：文字部分追加到输入框光标处（纯图粘贴无文字时跳过）
	  const text = cd ? (cd.getData("text/plain") || "") : "";
	  if (text) {
	    const el = inputEl.value;
	    const pos = el ? el.selectionStart : store.inputText.length;
	    const end = el ? el.selectionEnd : pos;
	    store.inputText = store.inputText.slice(0, pos) + text + store.inputText.slice(end);
	    requestAnimationFrame(() => {
	      if (el) { el.selectionStart = el.selectionEnd = pos + text.length; el.focus(); }
	    });
	  }
	  if (added > 0) {
	    Message.info({ content: "已添加 " + added + " 张图片附件" + (text ? "和文字" : ""), duration: 1500 });
	    inputEl.value && inputEl.value.focus(); // 粘贴后聚焦输入框，可直接继续输入/发送
	  }
	}

function sendOrQueue() {
  atOpen.value = false;
  sendCurrent();
}

	// 模式切换（原顶栏下拉迁移至此，语义不变）。
	// 会话级联动（2026-11 会话自有配置）：已绑定会话时写会话自有模式
	//（SetSessionMode，后端回写 s.Mode，选择器随会话切换恢复）；
	// 新会话/写失败仅改全局当前模式。运行中会话后端拒绝写入时选择器
	// 回滚旧值（本轮已按旧模式运行，避免假反馈）。
	async function switchModeSafe(v) {
	  const prev = store.mode;
	  store.mode = v;
	  try { await bridgeCall("SetMode", v); } catch (e) { console.warn(e); }
	  if (store.currentId) {
	    try {
	      await bridgeCall("SetSessionMode", store.currentId, v || "");
	    } catch (e) {
	      store.mode = prev;
	      Message.error(String(e));
	    }
	  }
	}
	// 模型切换（原顶栏下拉迁移至此，语义不变）。
	// 会话级联动（2026-11 会话自有配置）：已绑定会话时写会话自有模型
	//（SetSessionModel，选择器随会话切换恢复）；新会话/写失败回落全局配置。
	// 运行中会话后端拒绝写入（模型本轮已定）：选择器回滚旧值，避免
	//「显示已切换、实际仍按旧模型执行」的假反馈。
	async function bridgeSetModel(v) {
	  const prev = store.model;
	  store.model = v;
	  try { await bridgeCall("SetModel", v); } catch (e) { console.warn(e); }
	  if (store.currentId) {
	    try {
	      await bridgeCall("SetSessionModel", store.currentId, v || "");
	    } catch (e) {
	      store.model = prev;
	      Message.error(String(e));
	    }
	  }
	}

	// 远程节点切换（执行目标）：新会话仅暂存（发送时随首条消息发布）；
	// 已绑定会话写会话自有 NodeID（SetSessionNodeID，切回联动恢复，重启不丢）。
	// 运行中会话后端拒绝：选择器回滚旧值（本轮发布目标已定，改动不生效）。
	async function switchRemoteNodeSafe(v) {
	  const prev = store.currentRemoteNode;
	  store.currentRemoteNode = v || "";
	  if (!store.currentId) return;
	  try {
	    await bridgeCall("SetSessionNodeID", store.currentId, v || "");
	  } catch (e) {
	    store.currentRemoteNode = prev;
	    Message.error(String(e));
	  }
	}
</script>

<template>
  <div class="composer">
    <div class="composer-inner">
      <!-- @ 文件补全弹层（Claude Code 对齐） -->
      <div v-if="atOpen" class="at-pop">
        <div
          v-for="(p, i) in atItems"
          :key="p"
          class="at-item"
          :class="{ active: i === atIdx }"
          @mousedown.prevent="applyAt(p)"
          @mousemove="atIdx = i"
        >
          <span class="at-icon">{{ p.endsWith("/") ? "📁" : "📄" }}</span>
          <span class="at-path">{{ p }}</span>
        </div>
        <div class="at-hint">↑↓ 选择 · Enter/Tab 补全 · ESC 关闭</div>
      </div>

      <textarea
        ref="inputEl"
        v-model="store.inputText"
        class="composer-textarea"
        rows="2"
        :disabled="store.sending"
        :placeholder="isRunning
          ? '任务运行中，继续输入将排队（本轮结束后自动继续）…'
          : '描述任务，@ 引用文件，Enter 发送，Shift+Enter 换行'"
        @keydown="onKeydown"
        @input="onInput"
        @blur="atOpen = false"
      ></textarea>

      <div class="composer-actions">
        <!-- 图片附件 -->
        <div v-for="(src, i) in store.pendingImages" :key="i" class="img-chip">
          <img :src="src" />
          <span class="remove" @click="store.pendingImages.splice(i, 1)">×</span>
        </div>
        <a-tooltip content="添加图片">
          <button class="icon-btn" @click="pickImages"><IconImage /></button>
        </a-tooltip>

        <span v-if="queuedCount > 0" class="queued-hint"><IconClockCircle /> 排队 {{ queuedCount }} 条</span>
        <span class="spacer"></span>

        <!-- ACP agent 选择（外部编码 agent；空 = 内置引擎） -->
        <a-select
          v-if="store.acpAgents.length"
          class="composer-select composer-select-acp"
          :model-value="store.currentACP"
          size="mini"
          placeholder="内置引擎"
          @change="(v) => switchACP(v || '')"
          allow-clear
        >
          <a-option value="">内置引擎</a-option>
          <a-option v-for="ag in store.acpAgents.filter((x) => x.enabled)" :key="ag.name" :value="ag.name">
            {{ ag.name }}
          </a-option>
        </a-select>

	        <!-- 数字员工选择（内置引擎 / 绑定员工；远程模式下隐藏） -->
	        <a-select
	          v-if="store.employees.length && !store.currentRemoteNode"
	          class="composer-select composer-select-emp"
	          :model-value="store.currentEmployee"
	          size="mini"
	          placeholder="内置引擎"
	          @change="(v) => switchEmployee(v || '')"
	          allow-clear
	        >
	          <a-option value="">内置引擎</a-option>
	          <a-option v-for="e in store.employees.filter((x) => x.runnable)" :key="e.id" :value="e.id">
	            {{ (e.avatar || "🤖") + " " + e.name }}
	          </a-option>
	        </a-select>

		        <!-- 远程节点选择（用户需求 2026-10 之 1：切换远程后任务发布到远端 PC） -->
		        <a-select
		          class="composer-select composer-select-remote"
		          :model-value="store.currentRemoteNode"
		          size="mini"
		          placeholder="本机执行"
		          @change="(v) => switchRemoteNodeSafe(v || '')"
		          allow-clear
		        >
	          <a-option value="">本机执行</a-option>
	          <a-option v-if="store.remoteOnline" value="auto">自动选节点</a-option>
	          <a-option v-for="n in store.remoteNodes.filter((x) => x.online)" :key="n.id" :value="n.id">
	            {{ n.name || n.id }}{{ n.runningTasks ? "（运行中 " + n.runningTasks + "）" : "" }}
	          </a-option>
	        </a-select>

	        <!-- 模式/模型切换（停止/发送按钮左侧） -->
        <a-select
          class="composer-select"
          :model-value="store.mode"
          size="mini"
          placeholder="模式"
          @change="(v) => switchModeSafe(v)"
        >
          <a-option v-for="m in store.modes" :key="m.Name || m.name" :value="m.Name || m.name">
            {{ m.Name || m.name }}
          </a-option>
        </a-select>
        <a-select
          v-if="store.models.length"
          class="composer-select composer-select-model"
          :model-value="store.model"
          size="mini"
          placeholder="模型"
          @change="(v) => bridgeSetModel(v)"
        >
          <a-option v-for="mm in store.models" :key="mm.Id || mm.id || mm" :value="mm.Id || mm.id || mm">
            {{ mm.Id || mm.id || mm }}
          </a-option>
        </a-select>

        <!-- 运行中：停止；空闲：发送 -->
        <a-button
          v-if="isRunning"
          size="small"
          status="danger"
          @click="stopSession"
        >
          <template #icon><IconPauseCircle /></template>
          停止
        </a-button>
        <a-button
          v-else
          size="small"
          type="primary"
          :disabled="!store.inputText.trim() && !store.pendingImages.length"
          :loading="store.sending"
          @click="sendOrQueue"
        >
          <template #icon><IconSend /></template>
          发送
        </a-button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.queued-hint {
  font-size: 12px;
  color: var(--warning);
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
/* @ 文件补全弹层：浮于 textarea 上方（Claude Code 补全观感）；
   锚定 composer 顶沿之上，输入框自适应增高后仍不遮挡输入区 */
.at-pop {
  position: absolute;
  bottom: calc(100% + 6px);
  left: 16px;
  right: 16px;
  max-height: 280px;
  overflow-y: auto;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  box-shadow: var(--shadow-overlay);
  z-index: 30;
  padding: 4px;
}
.at-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 5px 8px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  font-size: var(--fs-caption);
  color: var(--text-1);
}
.at-item.active { background: var(--bg-active); }
.at-pop { animation: at-in 150ms var(--ease-out) both; }
@keyframes at-in { from { opacity: 0; transform: translateY(4px); } to { opacity: 1; transform: none; } }
.at-icon { flex-shrink: 0; font-size: 12px; }
.at-path {
  font-family: var(--font-mono);
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.at-hint {
  font-size: 11px;
  color: var(--text-3);
  padding: 4px 8px;
  border-top: 1px solid var(--border-weak);
  margin-top: 2px;
}
/* 输入区内联下拉：紧凑小控件，弱化边框以贴合 composer 背景 */
.composer-select {
  width: 108px;
  flex: 0 0 auto;
  transition: opacity var(--transition);
}
.composer-select:hover { opacity: 0.88; }
.composer-select-model {
  width: 148px;
}
	.composer-select-acp {
	  width: 128px;
	}
	.composer-select-emp {
	  width: 128px;
	}
	.composer-select-remote {
	  width: 128px;
	}
</style>
