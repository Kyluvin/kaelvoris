<template>
  <div class="ws-page">
    <div class="ws-toolbar">
      <button class="ws-dir-btn" :title="root || '选择工作区文件夹'" @click="pickDir">
        <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>
        <span class="ws-dir-name">{{ rootName }}</span>
      </button>
      <span v-if="linkedSession" class="ws-session-badge" :title="'关联会话：' + linkedSession">已关联会话</span>
      <div class="ws-spacer"></div>
      <button class="ws-run-btn" :disabled="!root || running" @click="openRun">
        <svg viewBox="0 0 24 24" width="13" height="13" fill="currentColor"><path d="M8 5v14l11-7z"/></svg>
        执行
      </button>
    </div>

    <div v-if="!root" class="ws-empty">
      <div class="ws-empty-logo">📂</div>
      <div class="ws-empty-title">工作区</div>
      <div class="ws-empty-desc">选择一个文件夹作为工作区，浏览与编辑其中文件；点击「执行」创建关联会话运行任务，完成后工作内容将总结回写该文件夹的 README.md。</div>
      <button class="ws-empty-btn" @click="pickDir">选择工作区文件夹</button>
    </div>

    <div v-else class="ws-body">
      <!-- 右键菜单（用户需求 2026-12）：文件/目录上右键 = 针对该节点；空白处右键 = 工作区根新建 -->
      <div class="ws-tree" @contextmenu.prevent.stop="openCtx($event, null)">
        <div class="ws-tree-head">
          <span>文件</span>
          <button class="ws-tree-btn" title="新建文件" @click="startCreate(false)">＋文件</button>
          <button class="ws-tree-btn" title="新建文件夹" @click="startCreate(true)">＋目录</button>
          <button class="ws-tree-btn" title="刷新" @click="load">⟳</button>
        </div>
        <div v-if="creating" class="ws-create-box">
          <input ref="newInput" v-model="newPath" class="ws-create-input" placeholder="名称（可含子目录）" @keyup.enter="create" @keyup.escape="creating = false" />
          <button class="ws-create-ok" @click="create">✓</button>
          <button class="ws-tree-btn" @click="creating = false">×</button>
        </div>
        <template v-if="flat.length">
          <div
            v-for="row in flat"
            :key="row.node.path"
            class="ws-item"
            :class="{ dir: row.node.isDir, active: !row.node.isDir && row.node.path === currentPath }"
            :style="{ paddingLeft: 8 + row.depth * 12 + 'px' }"
            :title="row.node.path"
            @click="row.node.isDir ? toggleDir(row.node) : openFile(row.node)"
            @contextmenu.prevent.stop="openCtx($event, row.node)"
          >
            <span v-if="row.node.isDir" class="ws-caret" :class="{ open: expanded.has(row.node.path) }">▸</span>
            <span v-else class="ws-caret-sp"></span>
            <span class="ws-item-ico">{{ row.node.isDir ? '📁' : '📄' }}</span>
            <span class="ws-item-name">{{ row.node.displayName || row.node.name }}</span>
            <span class="ws-spacer"></span>
            <button class="ws-node-btn" title="重命名" @click.stop="rename(row.node)">✎</button>
            <button class="ws-node-btn danger" title="删除" @click.stop="del(row.node)">🗑</button>
          </div>
        </template>
        <div v-else class="ws-tree-empty">目录为空</div>
      </div>

      <!-- 右键菜单浮层：fixed 定位，点空白/再次右键/滚动时关闭 -->
      <div
        v-if="ctx.show"
        class="ws-ctx"
        :style="{ left: ctx.x + 'px', top: ctx.y + 'px' }"
        @contextmenu.prevent
        @mousedown.stop
      >
        <button class="ws-ctx-item" @click="ctxNew(false)">新建文件</button>
        <button class="ws-ctx-item" @click="ctxNew(true)">新建文件夹</button>
        <template v-if="ctx.node">
          <div class="ws-ctx-sep"></div>
          <button class="ws-ctx-item" @click="ctxRename">重命名</button>
          <button class="ws-ctx-item danger" @click="ctxDelete">删除</button>
        </template>
      </div>

      <div class="ws-content">
        <template v-if="current">
          <div class="ws-file-head">
            <span class="ws-file-name" :title="current.path">{{ current.path }}</span>
            <span class="ws-file-meta">{{ fmtSize(current.size) }}</span>
            <div class="ws-spacer"></div>
              <button v-if="!editing" class="ws-act" :disabled="current.isBinary || current.truncated" :title="current.isBinary ? '二进制文件不支持编辑' : (current.truncated ? '文件超过 2MB 读取上限，暂不支持编辑' : '')" @click="startEdit">编辑</button>
            <template v-else>
              <button class="ws-act primary" :disabled="saving" @click="save">{{ saving ? '保存中…' : '保存' }}</button>
              <button class="ws-act" @click="editing = false">取消</button>
            </template>
          </div>
          <textarea v-if="editing" v-model="editContent" class="ws-editor" spellcheck="false"></textarea>
          <div v-else-if="current.isBinary" class="ws-binary">二进制文件，暂不支持预览</div>
          <pre v-else class="ws-source">{{ current.content }}</pre>
        </template>
        <div v-else class="ws-empty-sm">
          <div class="ws-empty-title">选择左侧文件查看 / 编辑</div>
          <div class="ws-empty-desc">支持文本编辑、新建文件 / 文件夹、重命名与删除。</div>
        </div>
      </div>
    </div>

    <!-- 执行任务弹窗（用户需求 2026-12：window.prompt 在 WebView2 中静默返回 null，
         执行按钮点击后无任何反应；改用 Arco a-modal 承载任务输入，对齐 CronPanel 惯例） -->
    <a-modal
      v-model:visible="runVisible"
      title="执行任务"
      :width="560"
      :ok-loading="running"
      ok-text="执行"
      @before-ok="onRun"
      unmount-on-close
    >
      <div class="ws-run-meta" v-if="runTargetSession">目标会话：{{ runTargetSession }}（复用）</div>
      <div class="ws-run-meta" v-else>将创建新会话（首次执行）</div>
      <div class="ws-run-meta ws-run-file" v-if="runTargetFile">开发对象：{{ runTargetFile }}（文件内容将随任务注入）</div>
      <a-textarea
        v-model="runTask"
        :auto-size="{ minRows: 3, maxRows: 8 }"
        placeholder="描述要在本工作区执行的任务，如：修复登录页崩溃并补齐测试…"
      />
    </a-modal>

    <!-- 重命名弹窗（WebView2 下 window.prompt 静默返回 null → 右键「重命名」无反应，
         改用 Arco a-modal，对齐 CronPanel/执行弹窗惯例） -->
    <a-modal
      v-model:visible="renameVisible"
      title="重命名"
      :width="480"
      :ok-loading="renaming"
      ok-text="确定"
      @before-ok="submitRename"
      unmount-on-close
    >
      <a-input ref="renInput" v-model="renameTo" placeholder="新路径（可含子目录实现移动）" />
    </a-modal>
  </div>
</template>

<script setup>
import { ref, computed, reactive, watch, nextTick, onMounted, onBeforeUnmount } from "vue";
import { bridgeCall } from "../bridge/wails.js";
import { store, openSession } from "../stores/app.js";
import { Message, Modal } from "@arco-design/web-vue";
const root = ref("");
const nodes = ref([]);
const linkedSession = ref("");
const running = ref(false);

// 新建文件/文件夹状态
const creating = ref(false);
const creatingIsDir = ref(false);
const newPath = ref("");
const newInput = ref(null);
watch(creating, (v) => {
  if (v) nextTick(() => newInput.value && newInput.value.focus());
});

// 当前打开的文件
const current = ref(null);
const currentPath = ref("");
const editing = ref(false);
const editContent = ref("");
const saving = ref(false);

const expanded = reactive(new Set());

// 右键菜单状态（用户需求 2026-12）：node=null 表示空白处右键（新建落在工作区根）
const ctx = reactive({ show: false, x: 0, y: 0, node: null });

// 执行任务弹窗状态（用户需求 2026-12：替代 WebView2 下静默失效的 window.prompt）
const runVisible = ref(false);
const runTask = ref("");
const runTargetSession = ref("");
const runTargetFile = ref(""); // 当前打开文件路径（非空 = 执行时注入文件内容作为开发对象）

// 重命名弹窗状态（替代 WebView2 下静默失效的 window.prompt）
const renameVisible = ref(false);
const renameTo = ref("");
const renameNode = ref(null);
const renaming = ref(false);
const renInput = ref(null);
watch(renameVisible, (v) => {
  if (v) nextTick(() => renInput.value && renInput.value.focus());
});

function openCtx(e, node) {
  // .stop 阻断冒泡：window 上的 contextmenu 监听（onGlobalClose）在同一事件里
  // 会立即关闭刚打开的菜单（空白处右键闪开即关的根因）；文件行的 .stop 只
  // 保护节点右键，树空白处这条链路同样需要阻断。
  e.stopPropagation();
  ctx.show = true;
  ctx.node = node;
  // 菜单约 150x120，防止贴边溢出视口
  ctx.x = Math.min(e.clientX, window.innerWidth - 160);
  ctx.y = Math.min(e.clientY, window.innerHeight - 140);
}
function closeCtx() {
  ctx.show = false;
}
function ctxNew(isDir) {
  closeCtx();
  creatingIsDir.value = isDir;
  creating.value = true;
  // 针对目录右键新建 → 该目录内；文件/空白右键 → 其父目录（文件无子级）
  const base = ctx.node ? (ctx.node.isDir ? ctx.node.path : parentOf(ctx.node.path)) : "";
  newPath.value = base ? base + "/" : "";
}
function ctxRename() {
  closeCtx();
  if (ctx.node) rename(ctx.node);
}
function ctxDelete() {
  closeCtx();
  if (ctx.node) del(ctx.node);
}
function parentOf(p) {
  const i = p.lastIndexOf("/");
  return i < 0 ? "" : p.slice(0, i);
}

const rootName = computed(() => {
  if (!root.value) return "选择工作区文件夹";
  const parts = root.value.replace(/[\\/]+$/, "").split(/[\\/]/);
  return parts[parts.length - 1] || root.value;
});

function fmtSize(n) {
  if (n == null) return "";
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}

async function pickDir() {
  try {
    const dir = await bridgeCall("PickWorkspace");
    if (!dir) return;
    await bridgeCall("SetWSPanelDir", dir);
    expanded.clear();
    current.value = null;
    currentPath.value = "";
    editing.value = false;
    await load();
  } catch (e) {
    Message.error("选择目录失败：" + (e && e.message || e));
  }
}

async function load() {
  try {
    const res = await bridgeCall("GetWSTree");
    root.value = res.root || "";
    nodes.value = res.nodes || [];
    if (res.exists === false) root.value = "";
  } catch (e) {
    nodes.value = [];
  }
  try {
    const s = await bridgeCall("WSSessionFor");
    linkedSession.value = s && s.sessionId || "";
  } catch (e) {
    /* 忽略 */
  }
}

// 展开集：仅保留有效路径，防节点删除后残留
function toggleDir(node) {
  if (expanded.has(node.path)) expanded.delete(node.path);
  else expanded.add(node.path);
}

// 深度优先铺平树；父目录折叠时子树不输出
const flat = computed(() => {
  const out = [];
  const walk = (list, depth) => {
    for (const n of list) {
      out.push({ node: n, depth });
      if (n.isDir && n.children && expanded.has(n.path)) walk(n.children, depth + 1);
    }
  };
  walk(nodes.value, 0);
  return out;
});

async function openFile(node) {
  try {
    const res = await bridgeCall("ReadWSFile", node.path);
    current.value = { ...node, ...res };
    currentPath.value = node.path;
    editing.value = false;
    editContent.value = res.content || "";
  } catch (e) {
    Message.error("读取失败：" + (e && e.message || e));
  }
}

function startEdit() {
  editContent.value = current.value.content || "";
  editing.value = true;
}

async function save() {
  if (!current.value || saving.value) return;
  saving.value = true;
  // 超时兜底：Wails 绑定调用若因宿主异常永挂起（Promise 永不 settle），
  // 按钮将永久卡在「保存中…」且无任何提示——用 race 保证 10s 后按失败处理。
  const timeout = new Promise((_, rej) => setTimeout(() => rej(new Error("保存超时（后端无响应）")), 10000));
  try {
    await Promise.race([bridgeCall("WriteWSFile", { path: current.value.path, content: editContent.value }), timeout]);
    current.value.content = editContent.value;
    current.value.size = new Blob([editContent.value]).size; // 字节数（含中文时 length 是字符数会偏小）
    editing.value = false;
    Message.success("已保存");
  } catch (e) {
    Message.error("保存失败：" + (e && e.message || e));
  } finally {
    saving.value = false;
  }
}

// Ctrl/Cmd+S 快捷保存（全局监听：编辑态下焦点不在 textarea 也能保存，Ctrl+S 触发浏览器/宿主默认行为的场景一并拦截）。
// 用 e.code 判定（e.key 在 CapsLock/部分输入法下为大写 "S"，字符串比较会把
// Ctrl+S 静默丢掉——用户报告「快捷保存不可用」的根因之一）。
function onKey(e) {
  if (!(e.ctrlKey || e.metaKey) || e.altKey) return;
  if (e.code !== "KeyS" && e.key !== "s" && e.key !== "S") return;
  if (!editing.value) return; // 非编辑态不拦截（不 preventDefault）
  e.preventDefault();
  save();
}

function startCreate(isDir) {
  creatingIsDir.value = isDir;
  creating.value = true;
  newPath.value = "";
}

// 全局点击/右键/滚轮关闭右键菜单（mousedown.stop 避免点菜单项先触发关闭）
function onGlobalClose() {
  closeCtx();
}

// 打开执行弹窗：预查关联会话给用户明确预期（新会话 or 复用）；
// 当前面板打开了文件时，任务预填「完成此文件开发」并注入文件内容（用户需求：
// 点击执行 = 以当前打开文件为开发对象，第一次创建新会话跑完文件待办开发）。
const WS_FILE_INJECT_LIMIT = 8000; // 文件内容注入上限（字符），防止超大文件撑爆任务文本
async function openRun() {
  try {
    const s = await bridgeCall("WSSessionFor");
    runTargetSession.value = s && s.sessionId || "";
  } catch (e) {
    runTargetSession.value = "";
  }
  runTargetFile.value = current.value && !current.value.isBinary ? current.value.path : "";
  if (runTargetFile.value) {
    runTask.value = `请完成文件 ${runTargetFile.value} 的开发内容：阅读文件中已有的说明/待办（TODO/注释/结构），把文件开发到完整可用状态。`;
  } else {
    runTask.value = "";
  }
  runVisible.value = true;
}

async function create() {
  const p = (newPath.value || "").trim();
  if (!p) return;
  try {
    await bridgeCall("CreateWSFile", { path: p, isDir: creatingIsDir.value });
    creating.value = false;
    // 自动展开父目录链
    const parts = p.split("/");
    let acc = "";
    for (let i = 0; i < parts.length - 1; i++) {
      acc = acc ? acc + "/" + parts[i] : parts[i];
      expanded.add(acc);
    }
    await load();
    if (!creatingIsDir.value) {
      const found = findByPath(p);
      if (found) {
        await openFile(found);
        editing.value = true; // 新建文件直接进入编辑态，省一次「编辑」点击
      }
    }
  } catch (e) {
    Message.error("创建失败：" + (e && e.message || e));
  }
}

function findByPath(path) {
  const stack = [...nodes.value];
  while (stack.length) {
    const n = stack.shift();
    if (n.path === path) return n;
    if (n.children) stack.push(...n.children);
  }
  return null;
}

function rename(node) {
  renameNode.value = node;
  renameTo.value = node.path;
  renameVisible.value = true;
}

async function submitRename() {
  const node = renameNode.value;
  const to = (renameTo.value || "").trim();
  if (!node || !to || to === node.path) return false;
  renaming.value = true;
  try {
    await bridgeCall("RenameWSFile", { from: node.path, to });
    if (currentPath.value === node.path || (currentPath.value + "/").startsWith(node.path + "/")) {
      current.value = null;
      currentPath.value = "";
      editing.value = false;
    }
    await load();
    return true;
  } catch (e) {
    Message.error("重命名失败：" + (e && e.message || e));
    return false;
  } finally {
    renaming.value = false;
  }
}

function del(node) {
  // WebView2 下 window.confirm 静默返回 false → 右键「删除」无反应，改用 Modal.confirm（对齐 CronPanel 惯例）
  Modal.confirm({
    title: node.isDir ? "删除目录" : "删除文件",
    content: `确认删除「${node.path}」？${node.isDir ? "目录内全部内容将一并删除。" : ""}`,
    okText: "删除",
    okButtonProps: { status: "danger" },
    onOk: async () => {
      try {
        await bridgeCall("DeleteWSFile", node.path);
        if (currentPath.value === node.path || (currentPath.value + "/").startsWith(node.path + "/")) {
          current.value = null;
          currentPath.value = "";
          editing.value = false;
        }
        await load();
      } catch (e) {
        Message.error("删除失败：" + (e && e.message || e));
      }
    },
  });
}

async function onRun() {
  const task = (runTask.value || "").trim();
  if (!task) {
    Message.warning("请输入任务内容");
    return false; // a-modal before-ok 返回 false 阻止关闭
  }
  running.value = true;
  try {
    // 组装任务：当前打开文件存在时注入内容段（对齐后端 [workspace-readme] 注入惯例）
    let prompt = task;
    if (runTargetFile.value) {
      const c = (editContent.value && editing.value) ? editContent.value : (current.value && current.value.content) || "";
      let body = c;
      if (body.length > WS_FILE_INJECT_LIMIT) body = body.slice(0, WS_FILE_INJECT_LIMIT) + "\n…（文件过长已截断）";
      prompt = task + `\n\n[workspace-file] 以下是目标文件 ${runTargetFile.value} 当前内容（请在此基础上开发完成）：\n\n` + body;
    }
    // 后端 WSRunProject 返回纯字符串会话 ID（非 {sessionId,isNew} 对象）。
    // isNew 用 openRun 预查的 runTargetSession 判定（执行前为空 = 本次新建）。
    const sid = await bridgeCall("WSRunProject", prompt);
    if (sid) {
      const isNew = !runTargetSession.value;
      Message.success(isNew ? "已创建关联会话，任务执行中" : "已发送至关联会话");
      store.activeMain = "chat";
      await openSession(sid);
    }
    return true;
  } catch (e) {
    Message.error("执行失败：" + (e && e.message || e));
    return false;
  } finally {
    running.value = false;
  }
}

// 目录变更事件防抖刷新
let reloadTimer = null;
function onWsChanged() {
  if (reloadTimer) clearTimeout(reloadTimer);
  reloadTimer = setTimeout(load, 500);
}

onMounted(() => {
  load();
  window.addEventListener("kaelvoris:ws_changed", onWsChanged);
  window.addEventListener("mousedown", onGlobalClose);
  window.addEventListener("contextmenu", onGlobalClose);
  window.addEventListener("wheel", onGlobalClose, { passive: true });
  window.addEventListener("keydown", onKey);
});
onBeforeUnmount(() => {
  window.removeEventListener("kaelvoris:ws_changed", onWsChanged);
  window.removeEventListener("mousedown", onGlobalClose);
  window.removeEventListener("contextmenu", onGlobalClose);
  window.removeEventListener("wheel", onGlobalClose);
  window.removeEventListener("keydown", onKey);
  if (reloadTimer) clearTimeout(reloadTimer);
});
</script>

<style scoped>
.ws-page {
  display: flex;
  flex-direction: column;
  height: 100%;
  background: var(--bg);
  color: var(--fg);
}
.ws-toolbar {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 12px;
  border-bottom: 1px solid var(--border-weak);
  flex-shrink: 0;
}
.ws-dir-btn {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 4px 10px;
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm, 6px);
  background: var(--bg-card);
  color: var(--fg);
  cursor: pointer;
  max-width: 280px;
}
.ws-dir-btn:hover {
  border-color: var(--primary);
}
.ws-dir-name {
  font-size: var(--fs-sm, 12px);
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ws-session-badge {
  font-size: var(--fs-xs, 11px);
  color: var(--primary);
  border: 1px solid var(--primary);
  border-radius: 999px;
  padding: 1px 8px;
  white-space: nowrap;
}
.ws-spacer {
  flex: 1;
}
.ws-run-btn {
  display: flex;
  align-items: center;
  gap: 5px;
  padding: 5px 14px;
  border: none;
  border-radius: var(--radius-sm, 6px);
  background: var(--primary);
  color: #fff;
  font-size: var(--fs-sm, 12px);
  font-weight: 600;
  cursor: pointer;
}
.ws-run-btn:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.ws-run-meta {
  font-size: var(--fs-xs, 11px);
  color: var(--fg-weak, #888);
  margin-bottom: 8px;
}
.ws-run-file {
  color: var(--primary);
  font-weight: 600;
}
.ws-empty {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding: 40px;
  text-align: center;
}
.ws-empty-logo {
  font-size: 40px;
}
.ws-empty-title {
  font-size: var(--fs-lg, 16px);
  font-weight: 700;
}
.ws-empty-desc {
  font-size: var(--fs-sm, 12px);
  color: var(--fg-weak, #888);
  max-width: 420px;
  line-height: 1.6;
}
.ws-empty-btn {
  margin-top: 8px;
  padding: 7px 18px;
  border: none;
  border-radius: var(--radius-sm, 6px);
  background: var(--primary);
  color: #fff;
  font-size: var(--fs-sm, 12px);
  cursor: pointer;
}
.ws-body {
  flex: 1;
  display: flex;
  min-height: 0;
}
.ws-tree {
  width: 232px;
  flex-shrink: 0;
  border-right: 1px solid var(--border-weak);
  display: flex;
  flex-direction: column;
  overflow-y: auto;
  background: var(--bg-card);
}
.ws-tree-head {
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 6px 8px;
  border-bottom: 1px solid var(--border-weak);
  font-size: var(--fs-xs, 11px);
  font-weight: 600;
  color: var(--fg-weak, #888);
  position: sticky;
  top: 0;
  background: var(--bg-card);
}
.ws-tree-btn {
  border: none;
  background: transparent;
  color: var(--fg-weak, #888);
  cursor: pointer;
  font-size: var(--fs-xs, 11px);
  padding: 2px 4px;
  border-radius: 4px;
}
.ws-tree-btn:hover {
  background: var(--bg-hover, rgba(0, 0, 0, 0.06));
  color: var(--fg);
}
.ws-create-box {
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 6px 8px;
  border-bottom: 1px solid var(--border-weak);
}
.ws-create-input {
  flex: 1;
  min-width: 0;
  border: 1px solid var(--border-weak);
  border-radius: 4px;
  background: var(--bg);
  color: var(--fg);
  font-size: var(--fs-xs, 11px);
  padding: 3px 6px;
}
.ws-create-ok {
  border: none;
  background: var(--primary);
  color: #fff;
  border-radius: 4px;
  cursor: pointer;
  padding: 2px 7px;
}
.ws-item {
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 3px 8px;
  padding-right: 6px;
  cursor: pointer;
  font-size: var(--fs-sm, 12px);
  white-space: nowrap;
  overflow: hidden;
}
.ws-item:hover {
  background: var(--bg-hover, rgba(0, 0, 0, 0.06));
}
.ws-item.active {
  background: var(--bg-active, rgba(0, 0, 0, 0.1));
  color: var(--primary);
  font-weight: 600;
}
.ws-item.dir {
  font-weight: 500;
}
.ws-caret {
  width: 12px;
  flex-shrink: 0;
  display: inline-block;
  transition: transform 0.15s;
  font-size: 10px;
  color: var(--fg-weak, #888);
}
.ws-caret.open {
  transform: rotate(90deg);
}
.ws-caret-sp {
  width: 12px;
  flex-shrink: 0;
}
.ws-item-ico {
  flex-shrink: 0;
}
.ws-item-name {
  overflow: hidden;
  text-overflow: ellipsis;
}
.ws-node-btn {
  border: none;
  background: transparent;
  cursor: pointer;
  font-size: 11px;
  padding: 1px 3px;
  border-radius: 3px;
  color: var(--fg-weak, #888);
  opacity: 0;
  flex-shrink: 0;
}
.ws-item:hover .ws-node-btn {
  opacity: 1;
}
.ws-node-btn.danger:hover {
  color: #d5423e;
}
.ws-tree-empty {
  padding: 16px;
  text-align: center;
  font-size: var(--fs-xs, 11px);
  color: var(--fg-weak, #888);
}
/* 右键菜单浮层（fixed 相对视口，z-index 高于面板内容） */
.ws-ctx {
  position: fixed;
  z-index: 1000;
  min-width: 132px;
  padding: 4px;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm, 6px);
  box-shadow: 0 4px 16px rgba(0, 0, 0, 0.15);
  display: flex;
  flex-direction: column;
}
.ws-ctx-item {
  border: none;
  background: transparent;
  color: var(--fg);
  font-size: var(--fs-sm, 12px);
  text-align: left;
  padding: 5px 10px;
  border-radius: 4px;
  cursor: pointer;
  white-space: nowrap;
}
.ws-ctx-item:hover {
  background: var(--bg-hover, rgba(0, 0, 0, 0.06));
}
.ws-ctx-item.danger {
  color: #d5423e;
}
.ws-ctx-sep {
  height: 1px;
  margin: 3px 6px;
  background: var(--border-weak);
}
.ws-content {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.ws-file-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 12px;
  border-bottom: 1px solid var(--border-weak);
  flex-shrink: 0;
}
.ws-file-name {
  font-size: var(--fs-sm, 12px);
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ws-file-meta {
  font-size: var(--fs-xs, 11px);
  color: var(--fg-weak, #888);
  white-space: nowrap;
}
.ws-act {
  padding: 3px 12px;
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm, 6px);
  background: var(--bg-card);
  color: var(--fg);
  font-size: var(--fs-xs, 11px);
  cursor: pointer;
}
.ws-act.primary {
  background: var(--primary);
  color: #fff;
  border-color: var(--primary);
}
.ws-editor {
  flex: 1;
  width: 100%;
  border: none;
  outline: none;
  resize: none;
  padding: 12px 16px;
  background: var(--bg);
  color: var(--fg);
  font-family: var(--font-mono, Consolas, monospace);
  font-size: var(--fs-sm, 12px);
  line-height: 1.6;
}
.ws-binary,
.ws-source {
  flex: 1;
  overflow: auto;
  padding: 12px 16px;
  margin: 0;
}
.ws-source {
  font-family: var(--font-mono, Consolas, monospace);
  font-size: var(--fs-sm, 12px);
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-all;
}
.ws-binary {
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--fg-weak, #888);
  font-size: var(--fs-sm, 12px);
}
.ws-empty-sm {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 6px;
}
</style>
