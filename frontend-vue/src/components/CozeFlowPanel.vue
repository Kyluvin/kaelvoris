<script setup>
// ═════════════════════════════════════════════════════════════════
// AI Flow（嵌入式内核）面板：coze-studio 源码融合引擎的前端接入。
//   - 内核状态徽章：就绪 / 模型可用 / 装配失败原因
//   - 工作流列表（搜索过滤）+ 新建（内置 start→end 模板）
//   - 可视化画布（CozeCanvas：拖拽节点/连线/删除）+ JSON 模式切换
//   - 保存草稿 + 发布版本 + 同步运行 + 结果展示
// ═════════════════════════════════════════════════════════════════
import { onMounted, ref, computed } from "vue";
import { Message } from "@arco-design/web-vue";
import { IconPlus, IconSearch, IconRefresh, IconDelete, IconPlayCircle, IconUpload } from "@arco-design/web-vue/es/icon";
import { bridgeCall } from "../bridge/wails.js";
import CozeCanvas from "./coze/CozeCanvas.vue";

const status = ref({ ready: false, modelConfigured: false, dataDir: "", error: "" });
const loading = ref(false);
const list = ref([]);
const keyword = ref("");

const selected = ref(null);      // 当前选中工作流详情
const selectedMeta = ref(null);  // 列表元信息
const name = ref("");
const desc = ref("");
const canvasText = ref("");
const canvasMode = ref("visual"); // visual=可视化画布 / json=JSON 编辑
const canvasRef = ref(null);      // CozeCanvas 组件实例（exportCanvas 导出画布）
const saving = ref(false);
const publishing = ref(false);
const running = ref(false);
const runInput = ref('{\n  "query": "你好，工作流"\n}');
const runResult = ref(null);

const filtered = computed(() => {
  const kw = keyword.value.trim().toLowerCase();
  if (!kw) return list.value;
  return list.value.filter((w) => (w.name + " " + w.desc).toLowerCase().includes(kw));
});

async function init() {
  loading.value = true;
  try {
    status.value = await bridgeCall("CozeInit");
    await refresh();
  } catch (e) {
    status.value = { ready: false, error: String(e) };
  } finally {
    loading.value = false;
  }
}

async function refresh() {
  try {
    list.value = (await bridgeCall("CozeListWorkflows", keyword.value)) || [];
  } catch (e) {
    Message.error("加载列表失败: " + e);
  }
}

async function createFlow() {
  const name2 = prompt("工作流名称", "未命名工作流");
  if (!name2) return;
  try {
    const m = await bridgeCall("CozeCreateWorkflow", name2, "");
    Message.success("已创建");
    await refresh();
    await open(m);
  } catch (e) {
    Message.error(String(e));
  }
}

async function open(meta) {
  selectedMeta.value = meta;
  runResult.value = null;
  try {
    const d = await bridgeCall("CozeGetWorkflow", meta.id);
    selected.value = d;
    name.value = d.name;
    desc.value = d.desc;
    canvasText.value = pretty(d.canvas);
  } catch (e) {
    Message.error(String(e));
  }
}

// 可视化 → JSON 模式切换时，把画布最新内容同步进 JSON 文本
function switchCanvasMode(m) {
  if (canvasMode.value === "visual" && m === "json" && canvasRef.value) {
    canvasText.value = pretty(canvasRef.value.exportCanvas());
  }
  canvasMode.value = m;
}

// 获取当前画布 JSON（按编辑模式取值）
function currentCanvas() {
  if (canvasMode.value === "visual") {
    return canvasRef.value ? canvasRef.value.exportCanvas() : canvasText.value;
  }
  return JSON.stringify(JSON.parse(canvasText.value)); // JSON 模式先校验合法
}

// 单节点试运行（画布抽屉调用）：先静默保存草稿，再发 NodeDebug 执行
async function testNode(workflowId, nodeId, input) {
  if (canvasMode.value === "visual" && canvasRef.value) {
    canvasText.value = JSON.stringify(JSON.parse(canvasRef.value.exportCanvas()));
  }
  await bridgeCall("CozeSaveWorkflow", String(workflowId), currentCanvas());
  return await bridgeCall("CozeTestNode", String(workflowId), String(nodeId), input || {});
}

// 在线模型列表（大模型节点表单下拉数据源）
async function loadModels() {
  return await bridgeCall("CozeListModels");
}

// MCP 级联数据源（工作流 MCP 节点：设置中的服务器 → 该服务器工具清单）
async function loadMCPServers() {
  return await bridgeCall("ListMCPServers");
}
async function loadMCPTools(serverName) {
  return await bridgeCall("CozeListMCPTools", String(serverName || ""));
}
// 图片生成模型配置（图片生成节点数据源：设置 → 图片生成模型）
async function loadImageGenConfig() {
  return await bridgeCall("CozeGetImageGenConfig");
}
// 知识库数据源（检索/写入节点：内核知识库列表 + 快速创建文本库）
async function loadKnowledgeBases() {
  return await bridgeCall("CozeListKnowledgeBases");
}
async function createKnowledge(name, content) {
  return await bridgeCall("CozeCreateKnowledge", String(name || ""), String(content || ""));
}
// 数据源（SQL 自定义节点：设置 → 数据源中管理的 MySQL 连接）
async function loadDatabases() {
  return await bridgeCall("CozeListDatabases");
}
// 工作流（子流程节点 9：可选流程清单，ID 字符串化避免 JS Number 精度丢失）
async function loadWorkflowRefs() {
  return await bridgeCall("CozeListWorkflows", "");
}

function pretty(s) {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

async function save() {
  if (!selected.value) return;
  saving.value = true;
  try {
    // 按编辑模式取画布 JSON（后端 sonic 解析接受任意格式）
    const canvas = currentCanvas();
    await bridgeCall("CozeSaveWorkflow", selected.value.id, canvas);
    Message.success("草稿已保存");
    await refresh();
  } catch (e) {
    Message.error("保存失败（画布 JSON 是否合法？）: " + e);
  } finally {
    saving.value = false;
  }
}

async function publish() {
  if (!selected.value) return;
  publishing.value = true;
  try {
    await bridgeCall("CozePublishWorkflow", selected.value.id, "", "面板发布", true);
    Message.success("已发布新版本");
    await refresh();
    await open(selectedMeta.value);
  } catch (e) {
    Message.error("发布失败: " + e);
  } finally {
    publishing.value = false;
  }
}

async function run() {
  if (!selected.value) return;
  running.value = true;
  runResult.value = null;
  try {
    let input = {};
    try {
      input = JSON.parse(runInput.value || "{}");
    } catch (e2) {
      Message.warning("运行输入 JSON 不合法，按空对象执行");
    }
    // Kaelvoris 补丁（2026-10）：先保存草稿再跑草稿（fromDraft=true）。
    // 原实现跑「最新发布版本」且不保存画布——画布改动（如新拖入的节点）全部
    // 不生效，发布版还是旧结构图时内核报 720701013 no tasks to execute。
    if (canvasMode.value === "visual" && canvasRef.value) {
      canvasText.value = JSON.stringify(JSON.parse(canvasRef.value.exportCanvas()));
    }
    await bridgeCall("CozeSaveWorkflow", String(selected.value.id), currentCanvas());
    runResult.value = await bridgeCall("CozeRunWorkflow", selected.value.id, input, true);
  } catch (e) {
    runResult.value = { status: "failed", failReason: String(e) };
  } finally {
    running.value = false;
  }
}

async function remove() {
  if (!selected.value) return;
  if (!confirm(`确认删除工作流「${selectedMeta.value?.name || selected.value.name}」？`)) return;
  try {
    await bridgeCall("CozeDeleteWorkflow", selected.value.id);
    Message.success("已删除");
    selected.value = null;
    selectedMeta.value = null;
    await refresh();
  } catch (e) {
    Message.error(String(e));
  }
}

// 列表卡片删除（强制硬删，不经详情面板）：id 字符串化避免 JS Number 精度丢失
async function removeById(w) {
  if (!confirm(`确认删除工作流「${w.name}」？该操作不可恢复。`)) return;
  try {
    await bridgeCall("CozeDeleteWorkflow", String(w.id));
    Message.success("已删除");
    if (selectedMeta.value && String(selectedMeta.value.id) === String(w.id)) {
      selected.value = null;
      selectedMeta.value = null;
    }
    await refresh();
  } catch (e) {
    Message.error(String(e));
  }
}

onMounted(init);
</script>

<template>
  <div class="cozeflow">
    <!-- 头部：标题 + 内核状态 -->
    <header class="cozeflow-head">
      <div class="head-title">
        <h2>AI Flow（嵌入式内核）</h2>
        <a-tag v-if="status.ready && status.modelConfigured" color="green">内核就绪 · 模型可用</a-tag>
        <a-tag v-else-if="status.ready" color="orange">内核就绪 · 模型未配置</a-tag>
        <a-tag v-else-if="status.error" color="red">装配失败</a-tag>
        <a-tag v-else color="gray">装配中…</a-tag>
      </div>
      <div class="head-actions">
        <a-input-search
          v-model="keyword"
          placeholder="搜索工作流"
          size="small"
          style="width: 200px"
          allow-clear
          @search="refresh"
          @clear="refresh"
        >
          <template #prefix><IconSearch /></template>
        </a-input-search>
        <a-button size="small" @click="init"><IconRefresh /> 重新装配</a-button>
        <a-button type="primary" size="small" :disabled="!status.ready" @click="createFlow">
          <IconPlus /> 新建工作流
        </a-button>
      </div>
    </header>
    <div v-if="status.error" class="cozeflow-error">{{ status.error }}</div>

    <!-- 主体两栏 -->
    <div class="cozeflow-body">
      <!-- 左：列表 -->
      <aside class="wf-list">
        <div v-if="!filtered.length" class="wf-empty">
          <a-empty :description="status.ready ? '暂无工作流，点击「新建」开始' : '内核装配中…'" />
        </div>
        <div
          v-for="w in filtered"
          :key="w.id"
          class="wf-item"
          :class="{ active: selectedMeta && selectedMeta.id === w.id }"
          @click="open(w)"
        >
          <div class="wf-name">
            {{ w.name }}
            <a-tag v-if="w.hasPublished" size="small" color="arcoblue">v{{ w.latestVersion }}</a-tag>
            <a-tag v-else size="small" color="gray">未发布</a-tag>
            <button class="wf-del" title="强制删除" @click.stop="removeById(w)"><IconDelete /></button>
          </div>
          <div class="wf-desc">{{ w.desc || "（无描述）" }}</div>
        </div>
      </aside>

      <!-- 右：详情 -->
      <section v-if="selected" class="wf-detail">
        <div class="detail-toolbar">
          <a-input v-model="name" size="small" style="width: 220px" placeholder="名称" />
          <a-input v-model="desc" size="small" style="flex: 1" placeholder="描述" />
          <a-button size="small" type="primary" :loading="saving" @click="save"><IconUpload /> 保存草稿</a-button>
          <a-button size="small" :loading="publishing" @click="publish">发布版本</a-button>
          <a-button size="small" status="danger" @click="remove"><IconDelete /> 删除</a-button>
        </div>

        <div class="detail-section detail-canvas">
          <div class="section-label canvas-label">
            <span>画布（开始/结束节点唯一；节点连线从下锚点拖到目标节点）</span>
            <span class="mode-switch">
              <a-radio-group type="button" size="mini" :model-value="canvasMode" @change="switchCanvasMode">
                <a-radio value="visual">可视化</a-radio>
                <a-radio value="json">JSON</a-radio>
              </a-radio-group>
            </span>
          </div>
          <!-- 可视化画布（Vue Flow：拖拽/连线/删除/移动，坐标存 meta.position） -->
          <CozeCanvas
            v-if="canvasMode === 'visual'"
            ref="canvasRef"
            :canvas="canvasText"
            :workflow-id="selected ? String(selected.id) : ''"
            :test-run-handler="testNode"
            :models-loader="loadModels"
            :mcp-servers-loader="loadMCPServers"
            :mcp-tools-loader="loadMCPTools"
            :image-gen-loader="loadImageGenConfig"
            :kb-loader="loadKnowledgeBases"
            :kb-create-handler="createKnowledge"
            :db-loader="loadDatabases"
            :wf-loader="loadWorkflowRefs"
            class="canvas-host"
          />
          <!-- JSON 兜底编辑 -->
          <textarea v-else v-model="canvasText" class="canvas-editor" spellcheck="false" />
        </div>

        <div class="detail-section">
          <div class="section-label">运行输入（对应 start 节点输出变量）</div>
          <div class="run-row">
            <textarea v-model="runInput" class="run-input" spellcheck="false" />
            <a-button type="primary" :loading="running" @click="run">
              <IconPlayCircle /> 运行
            </a-button>
          </div>
        </div>

        <div v-if="runResult" class="detail-section">
          <div class="section-label">
            运行结果
            <a-tag v-if="runResult.status === 'success'" color="green">{{ runResult.durationMs }}ms</a-tag>
            <a-tag v-else color="red">{{ runResult.status }}</a-tag>
          </div>
          <pre class="run-output">{{ runResult.failReason || JSON.stringify(runResult.output ?? runResult.outputRaw, null, 2) }}</pre>
        </div>
      </section>

      <section v-else class="wf-detail wf-detail-empty">
        <a-empty description="选择左侧工作流，或新建一个" />
      </section>
    </div>
  </div>
</template>

<style scoped>
.cozeflow {
  display: flex;
  flex-direction: column;
  height: 100%;
  padding: var(--gap);
  gap: var(--gap-sm);
  overflow: hidden;
}
.cozeflow-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--gap);
}
.head-title {
  display: flex;
  align-items: center;
  gap: var(--gap-sm);
}
.head-title h2 {
  margin: 0;
  font-size: 16px;
}
.head-actions {
  display: flex;
  align-items: center;
  gap: var(--gap-sm);
}
.cozeflow-error {
  padding: 8px 12px;
  border-radius: var(--radius-sm);
  background: rgba(245, 63, 63, 0.08);
  color: #f53f3f;
  font-size: 12px;
  white-space: pre-wrap;
}
.cozeflow-body {
  display: flex;
  flex: 1;
  min-height: 0;
  gap: var(--gap);
}
.wf-list {
  width: 260px;
  flex-shrink: 0;
  overflow-y: auto;
  border-right: 1px solid var(--border-weak);
  padding-right: var(--gap-sm);
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.wf-empty {
  padding: 32px 0;
}
.wf-item {
  padding: 10px 12px;
  border-radius: var(--radius-sm);
  border: 1px solid transparent;
  cursor: pointer;
  transition: background var(--transition);
}
.wf-item:hover {
  background: var(--bg-hover);
}
.wf-item.active {
  background: var(--bg-active);
  border-color: var(--border-weak);
}
.wf-name {
  font-weight: 600;
  font-size: 13px;
  display: flex;
  align-items: center;
  gap: 6px;
}
.wf-desc {
  margin-top: 2px;
  font-size: 12px;
  color: var(--text-3);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.wf-del {
  margin-left: auto;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 22px;
  height: 22px;
  border: none;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text-3);
  cursor: pointer;
  opacity: 0;
  transition: opacity var(--transition);
}
.wf-item:hover .wf-del { opacity: 1; }
.wf-del:hover { color: #f53f3f; background: var(--bg-hover); }
.wf-detail {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: var(--gap-sm);
  overflow: hidden; /* 画布撑满视口高度（flex:1），运行结果区内滚动 */
}
.wf-detail-empty {
  align-items: center;
  justify-content: center;
}
.detail-toolbar {
  display: flex;
  gap: var(--gap-sm);
  align-items: center;
}
.detail-section {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
/* 画布区：撑满详情剩余高度（可视化画布需要固定高度容器） */
.detail-canvas {
  flex: 1;
  min-height: 0;
}
.canvas-label {
  justify-content: space-between;
}
.canvas-host {
  flex: 1;
  min-height: 380px;
}
.section-label {
  font-size: 12px;
  color: var(--text-3);
  display: flex;
  align-items: center;
  gap: 6px;
}
.canvas-editor {
  width: 100%;
  height: 260px;
  resize: vertical;
  padding: 10px;
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  background: var(--bg-page);
  color: var(--text-1);
  font-family: var(--font-mono);
  font-size: 12px;
  line-height: 1.5;
}
.run-row {
  display: flex;
  gap: var(--gap-sm);
  align-items: stretch;
}
.run-input {
  flex: 1;
  height: 90px;
  resize: vertical;
  padding: 10px;
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  background: var(--bg-page);
  color: var(--text-1);
  font-family: var(--font-mono);
  font-size: 12px;
}
.run-output {
  margin: 0;
  padding: 10px;
  border-radius: var(--radius-sm);
  background: var(--bg-page);
  border: 1px solid var(--border-weak);
  font-family: var(--font-mono);
  font-size: 12px;
  max-height: 200px;
  overflow: auto;
  white-space: pre-wrap;
}
</style>
