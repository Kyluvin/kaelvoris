<script setup>
// ═════════════════════════════════════════════════════════════════
// 会话技能面板（用户需求 2026-12：聊天页右侧「技能」TAB）
//   与左侧「技能库」（SkillPanel，全量货架）不同：本面板只显示
//   用户显式「添加」进当前会话的技能：
//     1. 添加：从技能库弹窗挑选（搜索过滤，内置技能同样可选）；
//     2. 配置：按技能 frontmatter params 声明动态渲染参数表单，
//        保存随会话持久化（~/.kaelvoris/sessions/<id>.json）；
//     3. 运行：「运行」按钮把技能指令全文 + 参数拼为任务发进当前会话；
//     4. 生效：未点运行时，会话内每次任务也会自动注入已挂载技能
//        （后端 runTask → sessionSkillsSnippet）。
// 后端：skill_session_panel.go（GetSessionSkills/AddSessionSkill/
//   SetSessionSkillParams/RemoveSessionSkill/RunSessionSkill）。
// ═════════════════════════════════════════════════════════════════
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { Message, Modal } from "@arco-design/web-vue";
import {
  IconPlus, IconDelete, IconPlayArrow, IconRefresh, IconSearch,
  IconSettings, IconThunderbolt, IconBook,
} from "@arco-design/web-vue/es/icon";
import { store } from "../stores/app.js";
import { bridgeCall } from "../bridge/wails.js";

const props = defineProps({ compact: { type: Boolean, default: false } });

const skills = ref([]);       // 已挂载技能（[]SessionSkillView）
const loading = ref(false);
const runningSkill = ref(""); // 正在运行的技能名（按钮 loading）

const sid = computed(() => store.currentId || "");

// ── 数据加载 ─────────────────────────────────────────────────────
async function load() {
  if (!sid.value) { skills.value = []; return; }
  loading.value = true;
  try {
    skills.value = (await bridgeCall("GetSessionSkills", sid.value)) || [];
  } catch { skills.value = []; }
  finally { loading.value = false; }
}

// 会话切换 / 挂载变化（保存参数/增删后由本组件自行刷新；kaelvoris:sessions
// 推送时也拉一次，覆盖远程/多窗口改动）。
watch(sid, () => load());
const offSess = (window.runtime && window.runtime.EventsOn)
  ? window.runtime.EventsOn("kaelvoris:sessions", () => load())
  : null;
onMounted(load);
onBeforeUnmount(() => { if (typeof offSess === "function") offSess(); });

// ── 添加技能（从技能库挑选）────────────────────────────────────────
const addVisible = ref(false);
const libSkills = ref([]);    // 技能库全量
const libKeyword = ref("");

async function openAdd() {
  addVisible.value = true;
  try {
    libSkills.value = (await bridgeCall("ListSkills")) || [];
  } catch (e) { Message.error(String(e)); }
}

// 库内过滤：排除已挂载 + 关键词（名称/描述）。
const libFiltered = computed(() => {
  const kw = (libKeyword.value || "").trim().toLowerCase();
  const mounted = new Set(skills.value.map((s) => s.name));
  return (libSkills.value || []).filter((s) =>
    !mounted.has(s.name) &&
    (!kw || (s.name || "").toLowerCase().includes(kw) ||
      (s.description || "").toLowerCase().includes(kw))
  );
});

async function addSkill(s) {
  try {
    await bridgeCall("AddSessionSkill", sid.value, s.name);
    Message.success(`已添加技能「${s.name}」`);
    addVisible.value = false;
    await load();
    openCfg(s.name); // 新添加直接进入参数配置（有参数即可填）
  } catch (e) { Message.error(String(e)); }
}

// ── 参数配置抽屉 ─────────────────────────────────────────────────
const cfgVisible = ref(false);
const cfgSaving = ref(false);
const cfgForm = ref(null); // { name, params[], values{} }

function defaultOf(p, values) {
  if (values && values[p.name] !== undefined && values[p.name] !== "") return values[p.name];
  return p.default || "";
}

function openCfg(name) {
  const s = skills.value.find((x) => x.name === name);
  if (!s) return;
  const values = {};
  for (const p of s.params || []) {
    values[p.name] = defaultOf(p, s.values);
  }
  cfgForm.value = { name: s.name, description: s.description, params: s.params || [], values };
  cfgVisible.value = true;
}

async function saveCfg() {
  const f = cfgForm.value;
  if (!f) return;
  for (const p of f.params) {
    if (p.required && String(f.values[p.name] || "").trim() === "") {
      Message.warning(`参数「${p.label}」必填`);
      return;
    }
  }
  cfgSaving.value = true;
  try {
    await bridgeCall("SetSessionSkillParams", sid.value, f.name, f.values);
    Message.success("参数已保存");
    cfgVisible.value = false;
    await load();
  } catch (e) { Message.error(String(e)); }
  finally { cfgSaving.value = false; }
}

// ── 运行 / 移除 ──────────────────────────────────────────────────
async function runSkill(s) {
  if (!sid.value) return;
  // 必填参数未配置 → 先弹配置。
  for (const p of s.params || []) {
    const v = (s.values || {})[p.name];
    if (p.required && (!v || !String(v).trim())) {
      Message.warning(`请先配置「${s.name}」的必填参数`);
      openCfg(s.name);
      return;
    }
  }
  runningSkill.value = s.name;
  try {
    const newSid = await bridgeCall(
      "RunSessionSkill", sid.value, s.name, JSON.stringify(s.values || {})
    );
    // 空会话首次运行：后端返回新会话 ID，前端接管（与 sendCurrent 同语义）。
    if (newSid && newSid !== sid.value) store.currentId = newSid;
    Message.success(`技能「${s.name}」已开始运行`);
  } catch (e) { Message.error(String(e)); }
  finally { runningSkill.value = ""; }
}

function removeSkill(s) {
  Modal.confirm({
    title: "移除技能",
    content: `从当前会话移除「${s.name}」？技能库本体不受影响，已保存的参数将被清除。`,
    okText: "移除",
    cancelText: "取消",
    onOk: async () => {
      try {
        await bridgeCall("RemoveSessionSkill", sid.value, s.name);
        Message.success("已移除");
        await load();
      } catch (e) { Message.error(String(e)); }
    },
  });
}

const sourceLabel = { builtin: "内置", global: "全局", project: "项目", plugin: "插件" };
</script>

<template>
  <div class="ss-panel" :class="{ compact }">
    <template v-if="sid">
      <!-- 工具行 -->
      <div class="ss-toolbar">
        <span class="ss-count">{{ skills.length }} 个已添加技能</span>
        <span class="flex-sp"></span>
        <a-tooltip content="刷新">
          <button class="icon-btn" @click="load"><IconRefresh /></button>
        </a-tooltip>
        <a-button size="small" type="primary" @click="openAdd">
          <template #icon><IconPlus /></template>
          添加技能
        </a-button>
      </div>

      <a-spin :loading="loading" style="display: block; width: 100%">
        <div v-if="skills.length" class="ss-list">
          <div v-for="s in skills" :key="s.name" class="ss-card" :class="{ missing: s.source === 'missing' }">
            <div class="ss-row1">
              <span class="ss-name" :title="s.name">{{ s.name }}</span>
              <a-tag v-if="s.source === 'missing'" size="small" color="red">已删除</a-tag>
              <a-tag v-else size="small" color="arcoblue">{{ sourceLabel[s.source] || s.source }}</a-tag>
            </div>
            <div class="ss-desc" :title="s.description">{{ s.description || "（无描述）" }}</div>

            <!-- 参数概览（已配置值） -->
            <div v-if="s.params && s.params.length" class="ss-params">
              <span v-for="p in s.params" :key="p.name" class="ss-param"
                :title="p.label + ' = ' + ((s.values || {})[p.name] || p.default || '未配置')">
                {{ p.label }}: {{ (s.values || {})[p.name] || p.default || "未配置" }}
              </span>
            </div>

            <div class="ss-actions">
              <a-button size="mini" type="primary" :loading="runningSkill === s.name" @click="runSkill(s)">
                <template #icon><IconPlayArrow /></template>
                运行
              </a-button>
              <a-button v-if="s.params && s.params.length" size="mini" @click="openCfg(s.name)">
                <template #icon><IconSettings /></template>
                参数
              </a-button>
              <span class="flex-sp"></span>
              <a-button size="mini" type="text" status="danger" title="移除" @click="removeSkill(s)">
                <template #icon><IconDelete /></template>
              </a-button>
            </div>
          </div>
        </div>

        <div v-else class="ss-empty">
          <div class="ss-empty-ico"><IconThunderbolt /></div>
          <div class="ss-empty-title">未添加任何技能</div>
          <div class="ss-empty-hint">
            点上方「添加技能」从技能库挑选；<br />
            已添加的技能随本会话每次任务自动生效，<br />
            也可点「运行」单独执行。
          </div>
          <a-button size="small" type="primary" @click="openAdd">
            <template #icon><IconPlus /></template>
            从技能库添加
          </a-button>
        </div>
      </a-spin>
    </template>

    <!-- 无会话空态 -->
    <div v-else class="ss-empty">
      <div class="ss-empty-ico"><IconBook /></div>
      <div class="ss-empty-title">选择或开始一个会话</div>
      <div class="ss-empty-hint">会话技能随会话保存，每个会话独立配置。</div>
    </div>

    <!-- 添加技能弹窗（从技能库挑选） -->
    <a-modal
      v-model:visible="addVisible"
      title="从技能库添加技能"
      :width="480"
      :footer="false"
      unmount-on-close
    >
      <a-input-search
        v-model="libKeyword"
        placeholder="搜索技能名称/描述…"
        size="small"
        allow-clear
        style="margin-bottom: 10px"
      >
        <template #prefix><IconSearch /></template>
      </a-input-search>
      <div class="ss-lib-list">
        <div v-for="s in libFiltered" :key="s.name" class="ss-lib-item" @click="addSkill(s)">
          <span class="ss-name">{{ s.name }}</span>
          <a-tag size="small" color="arcoblue">{{ sourceLabel[s.source] || s.source }}</a-tag>
          <div class="ss-lib-desc">{{ s.description || "（无描述）" }}</div>
        </div>
        <div v-if="!libFiltered.length" class="ss-lib-empty">
          {{ libSkills.length ? "没有匹配或全部已添加" : "技能库为空（左侧技能库可新建）" }}
        </div>
      </div>
    </a-modal>

    <!-- 参数配置抽屉（动态表单） -->
    <a-drawer
      v-model:visible="cfgVisible"
      :title="'技能参数：' + (cfgForm && cfgForm.name || '')"
      :width="440"
      :footer="true"
      unmount-on-close
    >
      <template v-if="cfgForm">
        <div v-if="cfgForm.description" class="ss-cfg-desc">{{ cfgForm.description }}</div>
        <div v-if="cfgForm.params.length" class="ss-cfg-form">
          <div v-for="p in cfgForm.params" :key="p.name" class="ss-cfg-item">
            <div class="ss-cfg-label">
              {{ p.label }}
              <span v-if="p.required" class="ss-req">*</span>
              <span class="ss-cfg-key">{{ p.name }}</span>
            </div>
            <div v-if="p.description" class="ss-cfg-hint">{{ p.description }}</div>
            <a-select
              v-if="p.type === 'select'"
              v-model="cfgForm.values[p.name]"
              size="small"
              :placeholder="'默认: ' + (p.default || '无')"
              allow-clear
              style="width: 100%"
            >
              <a-option v-for="o in p.options || []" :key="o" :value="o">{{ o }}</a-option>
            </a-select>
            <a-textarea
              v-else-if="p.type === 'text'"
              v-model="cfgForm.values[p.name]"
              size="small"
              :auto-size="{ minRows: 3, maxRows: 8 }"
              :placeholder="'默认: ' + (p.default || '无')"
            />
            <a-input-number
              v-else-if="p.type === 'number'"
              v-model="cfgForm.values[p.name]"
              size="small"
              hide-button
              :placeholder="'默认: ' + (p.default || '无')"
              style="width: 100%"
            />
            <a-switch
              v-else-if="p.type === 'boolean'"
              :model-value="cfgForm.values[p.name] === true || cfgForm.values[p.name] === 'true'"
              @change="(v) => (cfgForm.values[p.name] = v)"
            />
            <a-input
              v-else
              v-model="cfgForm.values[p.name]"
              size="small"
              allow-clear
              :placeholder="'默认: ' + (p.default || '无')"
            />
          </div>
        </div>
        <a-alert v-else type="info">该技能无可配置参数，添加后即可随任务生效。</a-alert>
      </template>
      <template #footer>
        <a-space>
          <a-button @click="cfgVisible = false">取消</a-button>
          <a-button type="primary" :loading="cfgSaving" @click="saveCfg">保存参数</a-button>
        </a-space>
      </template>
    </a-drawer>
  </div>
</template>

<style scoped>
.ss-panel {
  height: 100%;
  overflow-y: auto;
  padding: 12px 4px 12px 2px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.ss-panel.compact { padding: 0; }
.flex-sp { flex: 1; }

.ss-toolbar {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-shrink: 0;
}
.ss-count {
  font-size: var(--fs-caption);
  color: var(--text-3);
}

/* 技能卡列表（单列，右栏窄幅） */
.ss-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.ss-card {
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm, 6px);
  background: var(--bg-card);
  padding: 10px 12px;
  display: flex;
  flex-direction: column;
  gap: 6px;
  transition: border-color 0.15s;
}
.ss-card:hover { border-color: var(--primary); }
.ss-card.missing { opacity: 0.7; border-style: dashed; }
.ss-row1 {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.ss-name {
  font-weight: 600;
  font-family: var(--font-mono, monospace);
  font-size: var(--fs-caption);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ss-desc {
  font-size: 12px;
  color: var(--text-3);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-height: 17px;
}

/* 参数概览 chips */
.ss-params {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
.ss-param {
  font-size: 11px;
  color: var(--text-2);
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: 3px;
  padding: 1px 6px;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.ss-actions {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: 2px;
}

/* 空态 */
.ss-empty {
  text-align: center;
  padding: 32px 12px;
  color: var(--text-3);
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
}
.ss-empty-ico { font-size: 28px; opacity: 0.5; }
.ss-empty-title { font-size: var(--fs-caption); color: var(--text-2); font-weight: 600; }
.ss-empty-hint { font-size: var(--fs-micro); line-height: 1.8; }

/* 添加弹窗列表 */
.ss-lib-list {
  max-height: 380px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.ss-lib-item {
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm, 6px);
  padding: 8px 10px;
  cursor: pointer;
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  transition: border-color 0.15s;
}
.ss-lib-item:hover { border-color: var(--primary); background: var(--bg-card); }
.ss-lib-desc {
  width: 100%;
  font-size: 12px;
  color: var(--text-3);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ss-lib-empty {
  text-align: center;
  color: var(--text-3);
  font-size: var(--fs-caption);
  padding: 24px 0;
}

/* 参数配置抽屉 */
.ss-cfg-desc {
  font-size: var(--fs-caption);
  color: var(--text-3);
  margin-bottom: 12px;
  line-height: 1.7;
}
.ss-cfg-form { display: flex; flex-direction: column; gap: 14px; }
.ss-cfg-item { display: flex; flex-direction: column; gap: 4px; }
.ss-cfg-label {
  font-size: var(--fs-caption);
  font-weight: 600;
  color: var(--text-1);
  display: flex;
  align-items: baseline;
  gap: 6px;
}
.ss-req { color: #f53f3f; }
.ss-cfg-key {
  font-family: var(--font-mono, monospace);
  font-size: 11px;
  color: var(--text-3);
  font-weight: 400;
}
.ss-cfg-hint { font-size: var(--fs-micro); color: var(--text-3); }
</style>
