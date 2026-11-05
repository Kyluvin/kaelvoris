<script setup>
// ═════════════════════════════════════════════════════════════════
// 文档面板（用户需求 2026-10：左侧文档图标，可查看 doc 对应的文档）
//   左：doc/ 目录扁平化文件树（目录点击展开/收起，文件点击查看）
//   右：Markdown 渲染/源码切换查看 + 在线编辑保存
//   操作：新建文件/目录、重命名、删除（删除有确认）
// 后端：doc_panel.go（GetDocInfo/ListDocTree/ReadDocFile/WriteDocFile/
//   CreateDocFile/DeleteDocFile/RenameDocFile）；kaelvoris:doc_changed 事件
//   广播后自动刷新（AI/外部改动文档时同步）。
// 安全边界（后端硬约束）：所有操作仅限 workspace/doc 内；前端只传相对路径。
// ═════════════════════════════════════════════════════════════════
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { Message, Modal } from "@arco-design/web-vue";

defineOptions({ name: "DocPanel" });
import {
  IconPlus, IconDelete, IconEdit, IconFile, IconFolder,
  IconFolderAdd, IconRefresh, IconSave, IconEye, IconCode, IconDown, IconRight,
} from "@arco-design/web-vue/es/icon";
import { bridgeCall, onEvent } from "../bridge/wails.js";
		import { renderMarkdownHtml } from "../utils/markdown.js";
import { store } from "../stores/app.js";

const info = ref(null);          // GetDocInfo 概况（root/exists/fileCount...）
const tree = ref([]);            // ListDocTree 嵌套文件树
const loading = ref(false);
const expanded = ref(new Set()); // 已展开目录 path 集合
const current = ref(null);       // 当前查看文件（DocFileContent）
const currentPath = ref("");     // 当前文件相对路径（树高亮）
const editing = ref(false);      // 编辑态
const editContent = ref("");     // 编辑缓冲
const saving = ref(false);
const viewMode = ref("render");  // render（Markdown 渲染）| source（源码）

// ── 数据加载 ─────────────────────────────────────────────────────
async function load() {
  loading.value = true;
  try {
    const [i, t] = await Promise.all([
      bridgeCall("GetDocInfo").catch(() => null),
      bridgeCall("ListDocTree").catch(() => []),
    ]);
    info.value = i;
    tree.value = t || [];
    // 当前文件被删除后清空视图（含目录被删：前缀匹配）。
    if (currentPath.value && !pathExists(tree.value, currentPath.value)) {
      current.value = null;
      currentPath.value = "";
      editing.value = false;
    }
  } finally {
    loading.value = false;
  }
}

// pathDepth 树中查找路径是否存在（文件被删/重命名后校验用）。
function pathExists(nodes, path) {
  for (const n of nodes || []) {
    if (n.path === path) return true;
    if (n.isDir && n.children && pathExists(n.children, path)) return true;
  }
  return false;
}

// ── 扁平化文件树（展开状态 → 可见节点列表，附缩进深度）───────────
const flatTree = computed(() => {
  const out = [];
  const walk = (nodes, depth) => {
    for (const n of nodes || []) {
      out.push({ node: n, depth });
      if (n.isDir && expanded.value.has(n.path) && n.children?.length) {
        walk(n.children, depth + 1);
      }
    }
  };
  walk(tree.value, 0);
  return out;
});

function toggleDir(node) {
  const next = new Set(expanded.value);
  next.has(node.path) ? next.delete(node.path) : next.add(node.path);
  expanded.value = next;
}

async function openFile(node) {
  try {
    const data = await bridgeCall("ReadDocFile", node.path);
    current.value = data;
    currentPath.value = node.path;
    editing.value = false;
    viewMode.value = "render";
  } catch (e) {
    Message.error(String(e));
  }
}

function onNodeClick(node) {
  node.isDir ? toggleDir(node) : openFile(node);
}

// ── 新建 ────────────────────────────────────────────────────────
const creating = ref(false);
const newPath = ref("");
const newIsDir = ref(false);

async function createDoc() {
  const p = newPath.value.trim();
  if (!p) { Message.warning("请输入名称（可含子目录，如 sub/notes.md）"); return; }
  try {
    await bridgeCall("CreateDocFile", { path: p, isDir: !!newIsDir.value });
    newPath.value = "";
    creating.value = false;
    Message.success("已创建");
    await load();
    // 新建文件直接打开进入编辑（目录只刷新树）。
    if (!newIsDir.value) openFile({ path: p, isDir: false });
  } catch (e) {
    Message.error(String(e));
  }
}

// doc 目录不存在时一键创建（CreateDocFile 支持逐级建父目录）。
async function createRootDir() {
  try {
    await bridgeCall("CreateDocFile", { path: ".keep", isDir: false });
    Message.success("doc 目录已创建");
    await load();
  } catch (e) {
    // .keep 可能已存在或后端拒绝：直接尝试空目录语义（创建子目录失败则报错）
    try {
      await bridgeCall("CreateDocFile", { path: "notes", isDir: true });
      Message.success("doc 目录已创建");
      await load();
    } catch (e2) { Message.error(String(e2)); }
  }
}

// ── 重命名 / 删除 ────────────────────────────────────────────────
// renameDoc：桌面 WebView 支持原生 prompt（轻量；改名含路径可同时实现移动）。
function renameDoc(node) {
  const to = window.prompt("新路径（可含子目录实现移动）", node.path);
  if (!to || to === node.path) return;
  bridgeCall("RenameDocFile", { from: node.path, to })
    .then(() => { Message.success("已重命名"); load(); })
    .catch((e) => Message.error(String(e)));
}

function deleteDoc(node) {
  Modal.confirm({
    title: "删除" + (node.isDir ? "目录" : "文件"),
    content: `确定删除「${node.displayName || node.name}」？${node.isDir ? "目录内全部内容将一并删除，" : ""}不可恢复。`,
    onOk: async () => {
      try {
        await bridgeCall("DeleteDocFile", node.path);
        // 当前查看文件在删除范围内则清空视图
        if (currentPath.value === node.path || (currentPath.value || "").startsWith(node.path + "/")) {
          current.value = null;
          currentPath.value = "";
          editing.value = false;
        }
        Message.success("已删除");
        await load();
      } catch (e) { Message.error(String(e)); }
    },
  });
}

// ── 编辑保存 ─────────────────────────────────────────────────────
function startEdit() {
  if (!current.value || current.value.isBinary) return;
  editContent.value = current.value.content || "";
  editing.value = true;
}

async function saveEdit() {
  if (!current.value) return;
  saving.value = true;
  try {
    await bridgeCall("WriteDocFile", { path: current.value.path, content: editContent.value });
    current.value = { ...current.value, content: editContent.value };
    editing.value = false;
    Message.success("已保存");
    load(); // 刷新树（大小/时间变化）
  } catch (e) {
    Message.error(String(e));
  } finally {
    saving.value = false;
  }
}

// 编辑器 Ctrl+S 快捷保存
function onEditorKeydown(e) {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") {
    e.preventDefault();
    saveEdit();
  }
}

// ── 展示辅助 ─────────────────────────────────────────────────────
function fmtSize(n) {
  const v = Number(n) || 0;
  if (v < 1024) return v + " B";
  if (v < 1024 * 1024) return (v / 1024).toFixed(1) + " KB";
  return (v / 1024 / 1024).toFixed(1) + " MB";
}

	const currentName = computed(() => (current.value?.name || "").replace(/\.md$/i, ""));
	const rendered = computed(() => renderMarkdownHtml(current.value?.content || ""));
	const isMarkdown = computed(() => /\.md$/i.test(current.value?.path || ""));

			// 代码块复制 + 文件链接委托：与 MessageItem.onMdClick 同一约定；
			// 统一用系统默认程序打开（2026-11 用户反馈已移除自动拉起 Zed 的行为）。
			async function onDocMdClick(e) {
			  const fl = e.target.closest && e.target.closest("[data-file-link]");
			  if (fl) {
			    e.preventDefault();
			    const path = fl.getAttribute("data-file-link") || "";
			    try {
			      await bridgeCall("OpenFileWithDefault", path);
			      Message.success("已用系统程序打开: " + path);
			    } catch (err) { Message.error(String((err && err.message) || err)); }
			    return;
			  }
		  const btn = e.target.closest && e.target.closest("[data-code-copy]");
		  if (!btn) return;
		  const wrap = btn.closest(".md-code");
		  const code = wrap ? wrap.querySelector("pre code") : null;
		  if (!code) return;
		  try {
		    await navigator.clipboard.writeText(code.textContent || "");
		    Message.success("已复制");
		  } catch { Message.error("复制失败"); }
		}

// kaelvoris:doc_changed：文档变更广播后防抖刷新（合并高频连续事件）。
let docChangedTimer = 0;
const onDocChanged = () => {
  clearTimeout(docChangedTimer);
  docChangedTimer = setTimeout(load, 500);
};

onMounted(() => {
  load();
  onEvent("kaelvoris:doc_changed", onDocChanged);
});
onBeforeUnmount(() => clearTimeout(docChangedTimer));

// 侧栏文档区 → 主视图打开请求（2026-12：文档入口放会话列表下方）：
// 侧栏点文件写入 store.docOpenPath 并切 activeMain=docs，本面板挂载/
// 激活后 watch 到即打开该文件并清空信号（一次性）；侧栏文件删除后打开失败仅报错。
async function consumeDocOpen() {
  const p = store.docOpenPath;
  if (!p) return;
  store.docOpenPath = "";
  try {
    const data = await bridgeCall("ReadDocFile", p);
    current.value = data;
    currentPath.value = data.path || p;
    editing.value = false;
    viewMode.value = "render";
  } catch (e) {
    Message.error(String(e));
    load(); // 树可能已过期（文件被删/重命名），刷新
  }
}
watch(() => store.docOpenPath, consumeDocOpen, { immediate: true });
</script>

<template>
  <div class="doc-page">
    <div class="doc-toolbar">
      <span class="doc-title">文档</span>
      <span v-if="info && info.exists" class="doc-stat">
        {{ info.fileCount }} 个文件{{ info.dirCount ? ` · ${info.dirCount} 个目录` : "" }}
      </span>
      <div class="spacer"></div>
      <a-button size="small" :loading="loading" @click="load"><template #icon><IconRefresh /></template></a-button>
      <a-button size="small" @click="creating = !creating">
        <template #icon><IconPlus /></template>新建
      </a-button>
    </div>

    <!-- doc 目录不存在：引导创建 -->
    <div v-if="info && !info.exists" class="doc-missing">
      <div class="doc-missing-logo">📄</div>
      <div class="doc-missing-title">doc 目录不存在</div>
      <div class="doc-missing-desc">当前项目根目录下没有 doc 目录。可点击下方按钮创建，或让 Agent 在任务中创建。</div>
      <a-button size="small" type="primary" @click="createRootDir">创建 doc 目录</a-button>
    </div>

    <!-- 新建输入区 -->
    <div v-if="creating" class="doc-create-box">
      <a-input v-model="newPath" size="small" placeholder="名称（可含子目录，如 sub/plan.md）" @keyup.enter="createDoc" />
      <label class="doc-create-dir"><a-checkbox v-model="newIsDir" size="small" />目录</label>
      <a-button size="small" type="primary" @click="createDoc">创建</a-button>
      <a-button size="small" @click="creating = false">取消</a-button>
    </div>

    <div v-if="info && info.exists" class="doc-body">
      <!-- 左：文件树（扁平化渲染，目录缩进） -->
      <div class="doc-tree">
        <template v-if="flatTree.length">
          <div
            v-for="row in flatTree"
            :key="row.node.path"
            class="doc-tree-item"
            :class="{ active: row.node.path === currentPath && !row.node.isDir }"
            :style="{ paddingLeft: 8 + row.depth * 14 + 'px' }"
            :title="row.node.path"
            @click="onNodeClick(row.node)"
          >
            <IconDown v-if="row.node.isDir && expanded.has(row.node.path)" class="doc-caret" />
            <IconRight v-else-if="row.node.isDir" class="doc-caret" />
            <span v-else class="doc-caret-spacer"></span>
            <IconFolder v-if="row.node.isDir" class="doc-icon" />
            <IconFile v-else class="doc-icon" />
            <span class="doc-tree-name">{{ row.node.displayName || row.node.name }}</span>
            <span class="flex-sp"></span>
            <button class="doc-node-btn" title="重命名" @click.stop="renameDoc(row.node)"><IconEdit /></button>
            <button class="doc-node-btn danger" title="删除" @click.stop="deleteDoc(row.node)"><IconDelete /></button>
          </div>
        </template>
        <div v-else-if="!loading" class="doc-tree-empty">doc 目录为空</div>
      </div>

      <!-- 右：内容区 -->
      <div class="doc-content">
        <template v-if="current">
          <div class="doc-file-head">
            <span class="doc-file-name" :title="current.path">{{ currentName }}</span>
            <span class="doc-file-meta">{{ fmtSize(current.size) }}</span>
            <div class="spacer"></div>
            <a-radio-group v-if="isMarkdown && !editing" v-model="viewMode" size="mini" type="button">
              <a-radio value="render"><template #icon><IconEye /></template>渲染</a-radio>
              <a-radio value="source"><template #icon><IconCode /></template>源码</a-radio>
            </a-radio-group>
            <a-button v-if="!editing" size="mini" type="primary" @click="startEdit">
              <template #icon><IconEdit /></template>编辑
            </a-button>
            <template v-else>
              <a-button size="mini" type="primary" :loading="saving" @click="saveEdit">
                <template #icon><IconSave /></template>保存
              </a-button>
              <a-button size="mini" @click="editing = false">取消</a-button>
            </template>
          </div>
          <div v-if="current.truncated" class="doc-truncated">文件超过 2MB，仅显示前 2MB 内容</div>

          <div v-if="editing" class="doc-editor-wrap">
            <textarea
              v-model="editContent"
              class="doc-editor"
              spellcheck="false"
              placeholder="Ctrl+S 保存"
              @keydown="onEditorKeydown"
            ></textarea>
          </div>
          <div v-else-if="current.isBinary" class="doc-binary">二进制文件，暂不支持预览</div>
	          <div v-else-if="isMarkdown && viewMode === 'render'" class="doc-md" v-html="rendered" @click="onDocMdClick"></div>
          <pre v-else class="doc-source">{{ current.content }}</pre>
        </template>
        <div v-else class="doc-empty-main">
          <div class="doc-empty-logo">📖</div>
          <div class="doc-empty-title">项目文档</div>
          <div class="doc-empty-desc">左侧选择 doc/ 目录下的文档查看；支持 Markdown 渲染、源码切换与在线编辑。</div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.doc-page {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-height: 0;
  padding: var(--pad-block);
  gap: var(--gap);
}
.doc-toolbar { display: flex; align-items: center; gap: var(--gap-sm); flex-shrink: 0; }
.doc-title { font-size: var(--fs-title); font-weight: 600; color: var(--text-1); }
.doc-stat { font-size: var(--fs-caption); color: var(--text-3); }
.spacer { flex: 1; }

/* doc 目录不存在引导 */
.doc-missing {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 10px;
  color: var(--text-3);
}
.doc-missing-logo { font-size: 40px; }
.doc-missing-title { font-size: var(--fs-title); color: var(--text-1); }
.doc-missing-desc { font-size: var(--fs-caption); max-width: 380px; text-align: center; line-height: 1.7; }

/* 新建输入区 */
.doc-create-box {
  display: flex;
  align-items: center;
  gap: var(--gap-sm);
  padding: var(--gap-sm) var(--gap);
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  flex-shrink: 0;
}
.doc-create-dir {
  display: flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-caption);
  color: var(--text-2);
  white-space: nowrap;
  cursor: pointer;
}

.doc-body {
  flex: 1;
  display: flex;
  gap: var(--gap);
  min-height: 0;
}

/* ── 左：文件树 ─────────────────────────────────────────────── */
.doc-tree {
  width: 232px;
  flex-shrink: 0;
  overflow-y: auto;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  padding: 6px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.doc-tree-empty { color: var(--text-3); font-size: var(--fs-caption); text-align: center; padding: 20px 0; }
.doc-tree-item {
  display: flex;
  align-items: center;
  gap: 5px;
  padding: 5px 8px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  font-size: var(--fs-caption);
  color: var(--text-2);
  transition: background var(--transition);
  white-space: nowrap;
  overflow: hidden;
}
.doc-tree-item:hover { background: var(--bg-hover); }
.doc-tree-item.active { background: var(--bg-active); color: var(--primary); }
.doc-caret { font-size: 10px; color: var(--text-3); flex-shrink: 0; }
.doc-caret-spacer { width: 12px; flex-shrink: 0; }
.doc-icon { color: var(--text-3); flex-shrink: 0; }
.doc-tree-name { overflow: hidden; text-overflow: ellipsis; }
.flex-sp { flex: 1; }
.doc-node-btn {
  border: none;
  background: none;
  color: var(--text-3);
  cursor: pointer;
  padding: 2px;
  font-size: 12px;
  border-radius: 4px;
  opacity: 0;
  transition: opacity var(--transition), color var(--transition);
  flex-shrink: 0;
  display: flex;
  align-items: center;
}
.doc-tree-item:hover .doc-node-btn { opacity: 0.7; }
.doc-node-btn:hover { color: var(--text-1); opacity: 1; }
.doc-node-btn.danger:hover { color: var(--danger, #e5534b); opacity: 1; }

/* ── 右：内容区 ─────────────────────────────────────────────── */
.doc-content {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: var(--gap-sm);
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  padding: var(--gap);
  overflow: hidden;
}
.doc-file-head { display: flex; align-items: center; gap: var(--gap-sm); flex-shrink: 0; }
.doc-file-name {
  font-size: var(--fs-body);
  font-weight: 600;
  color: var(--text-1);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.doc-file-meta { font-size: var(--fs-micro); color: var(--text-3); flex-shrink: 0; }
.doc-truncated { font-size: var(--fs-micro); color: var(--warning, #f7a500); flex-shrink: 0; }

.doc-editor-wrap { flex: 1; min-height: 0; display: flex; }
.doc-editor {
  flex: 1;
  width: 100%;
  background: var(--bg-page);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text-1);
  font-family: var(--font-mono);
  font-size: var(--fs-caption);
  line-height: 1.7;
  padding: 10px 12px;
  outline: none;
  resize: none;
}
.doc-editor:focus { border-color: var(--primary); }
.doc-binary { color: var(--text-3); font-size: var(--fs-caption); text-align: center; padding: 48px 0; }

.doc-source {
  flex: 1;
  overflow: auto;
  margin: 0;
  background: var(--bg-page);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  padding: var(--gap);
  color: var(--text-2);
  font-family: var(--font-mono);
  font-size: var(--fs-caption);
  line-height: 1.7;
  white-space: pre-wrap;
  word-break: break-word;
}

/* Markdown 渲染排版（对齐 MessageItem 的 md-* 视觉语言） */
.doc-md { flex: 1; overflow-y: auto; color: var(--text-2); font-size: var(--fs-body); line-height: 1.75; min-height: 0; }
.doc-md :deep(h1), .doc-md :deep(h2), .doc-md :deep(h3),
.doc-md :deep(h4), .doc-md :deep(h5), .doc-md :deep(h6) { color: var(--text-1); margin: 14px 0 8px; line-height: 1.4; }
.doc-md :deep(h1) { font-size: 20px; }
.doc-md :deep(h2) { font-size: 17px; }
.doc-md :deep(h3) { font-size: 15px; }
.doc-md :deep(h4) { font-size: 14px; }
.doc-md :deep(p) { margin: 8px 0; }
.doc-md :deep(ul), .doc-md :deep(ol) { padding-left: 22px; margin: 8px 0; }
.doc-md :deep(li) { margin: 3px 0; }
.doc-md :deep(blockquote) {
  border-left: 3px solid var(--primary);
  background: var(--bg-hover);
  padding: 6px 12px;
  margin: 8px 0;
  border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
}
.doc-md :deep(code.md-code-inline) {
  background: var(--bg-active);
  padding: 1px 5px;
  border-radius: 4px;
  font-family: var(--font-mono);
  font-size: 0.9em;
  color: var(--primary);
}
.doc-md :deep(pre.md-pre) {
  background: var(--bg-page);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  padding: 10px 12px;
  overflow-x: auto;
  margin: 8px 0;
}
	.doc-md :deep(pre.md-pre code) { font-family: var(--font-mono); font-size: var(--fs-caption); color: var(--text-2); }
	/* 代码块卡片（renderMarkdownHtml 统一输出）：深色底 + 语言标签 + 复制按钮，与消息流同构 */
	.doc-md :deep(.md-code) {
	  background: #16181D;
	  border: 1px solid var(--border-weak);
	  border-radius: var(--radius-sm);
	  overflow: hidden;
	  margin: 8px 0;
	}
	.doc-md :deep(.md-code-head) {
	  display: flex; align-items: center;
	  padding: 3px 10px;
	  background: rgba(255, 255, 255, 0.04);
	  border-bottom: 1px solid rgba(255, 255, 255, 0.07);
	}
	.doc-md :deep(.md-code-lang) {
	  font-family: var(--font-mono); font-size: var(--fs-micro); color: #8B93A7;
	  letter-spacing: 0.4px; text-transform: uppercase;
	}
	.doc-md :deep(.md-code-copy) {
	  margin-left: auto; border: none; background: none; cursor: pointer;
	  font-size: var(--fs-micro); color: #A9B1C3; padding: 2px 8px; border-radius: 4px;
	  font-family: var(--font-ui);
	}
	.doc-md :deep(.md-code-copy:hover) { background: rgba(255, 255, 255, 0.10); color: #E8EAF0; }
	.doc-md :deep(.md-code .md-pre) { margin: 0; border: none; border-radius: 0; background: transparent; }
	.doc-md :deep(.md-pre code) { color: #D8DEE9; }
	/* 高亮 token（Nord 系，与消息流一致） */
	.doc-md :deep(.hl-kw) { color: #81A1C1; }
	.doc-md :deep(.hl-str) { color: #A3BE8C; }
	.doc-md :deep(.hl-cmt) { color: #6A7387; font-style: italic; }
	.doc-md :deep(.hl-num) { color: #B48EAD; }
	.doc-md :deep(.hl-add) { color: #A3BE8C; }
	.doc-md :deep(.hl-del) { color: #BF616A; }
	.doc-md :deep(.hl-hunk) { color: #88C0D0; }
	/* 表格：自适应不溢出 */
	.doc-md :deep(.md-table-wrap) { overflow-x: auto; }
	.doc-md :deep(table) { border-collapse: collapse; margin: 8px 0; width: 100%; table-layout: fixed; font-size: var(--fs-caption); }
	.doc-md :deep(th), .doc-md :deep(td) { border: 1px solid var(--border-weak); padding: 5px 10px; text-align: left; word-break: break-word; color: var(--text-2); }
	.doc-md :deep(th) { background: var(--bg-page); color: var(--text-1); font-weight: 600; }
	.doc-md :deep(img) { max-width: 100%; border-radius: var(--radius-sm); }
.doc-md :deep(a) { color: var(--primary); }
.doc-md :deep(hr) { border: none; border-top: 1px solid var(--border-weak); margin: 12px 0; }

/* 空态 */
.doc-empty-main {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  color: var(--text-3);
}
.doc-empty-logo { font-size: 36px; }
.doc-empty-title { font-size: var(--fs-title); color: var(--text-1); }
.doc-empty-desc { font-size: var(--fs-caption); max-width: 380px; text-align: center; line-height: 1.7; }
</style>
