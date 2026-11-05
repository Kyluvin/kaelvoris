<script setup>
// ═════════════════════════════════════════════════════════════════
// 技能库面板（PC 端左侧入口「技能库」）— 电商式挑选体验
//   三级横向导航（像逛商品分类一样逐层收敛）：
//     L1 分类页签：全部 / 各自定义分类 / 未分类（数量徽标）+ 分类管理
//     L2 筛选条：来源（内置/全局/项目）+ 模式（modeSlugs 汇总）横向 chips
//     L3 工具行：搜索（名称/描述实时过滤）+ 计数 + 新建技能
//   卡片网格：名称/描述/来源徽标/模式标签/所属分类，hover 显编辑/改分类/删除
//   分类管理：新增 / 重命名 / 删除（删除后技能自动回落未分类）/ 上下移排序
//   - builtin 内置技能只读（无删除/保存按钮，可查看全文）
//   - 编辑 SKILL.md 全文（frontmatter+body）+ 分类选择
//   - 保存/删除/改分类后经 kaelvoris:skills 事件刷新（emitSkillsChanged 广播）
// 后端：skill_panel.go（ListSkillCategories/CategoryAdd/CategoryRename/
//   CategoryDelete/SetSkillCategory + ListSkills/SkillRaw/SaveSkill/DeleteSkill）
// 分类持久化：~/.kaelvoris/skill-categories.json（internal/skills/categories.go）
// ═════════════════════════════════════════════════════════════════
import { computed, onBeforeUnmount, ref } from "vue";
import { Message, Modal } from "@arco-design/web-vue";
import {
  IconPlus, IconEdit, IconDelete, IconRefresh, IconEye, IconSearch,
  IconFolderAdd, IconDragDot, IconSwap, IconUpload, IconBook, IconThunderbolt,
} from "@arco-design/web-vue/es/icon";
import { bridgeCall, onEvent } from "../bridge/wails.js";

const skills = ref([]);          // 全量技能（[]skills.Metadata）
const categories = ref([]);      // 分类导航（[]SkillCategoryView，含未分类虚拟区）
const loading = ref(false);
const keyword = ref("");
// compact 模式（2026-12 用户需求）：右栏 TAB 嵌入时收紧布局（父组件传 prop）
defineProps({ compact: { type: Boolean, default: false } });

// ── 三级导航选中态 ────────────────────────────────────────────────
const activeCat = ref("");       // "" = 全部；"__uncat" = 未分类虚拟区；其他 = 分类 ID
const activeSource = ref("");    // "" = 全部来源；builtin/global/project
const activeMode = ref("");      // "" = 全部模式；否则 modeSlug

const sourceMap = {
  builtin: { label: "内置", color: "gray" },
  global: { label: "全局", color: "blue" },
  project: { label: "项目", color: "green" },
};

const NEW_TEMPLATE = `---
name: my-skill
description: 技能一句话描述
modeSlugs:
  - code
---

# 技能正文

分步说明、命令、注意事项……`;

// ── 数据加载 ─────────────────────────────────────────────────────
async function load() {
  loading.value = true;
  try {
    const [list, cats] = await Promise.all([
      bridgeCall("ListSkills").catch(() => []),
      bridgeCall("ListSkillCategories").catch(() => []),
    ]);
    skills.value = list || [];
    categories.value = cats || [];
    // 当前选中分类被删除后回退到「全部」。
    if (activeCat.value && activeCat.value !== "__uncat" &&
        !categories.value.some((c) => c.id === activeCat.value)) {
      activeCat.value = "";
    }
  } finally {
    loading.value = false;
  }
}

// 分类名映射（技能卡片显示所属分类用；已删除分类显示为未分类）。
const catNameMap = computed(() => {
  const m = {};
  for (const c of categories.value) m[c.id] = c.name;
  return m;
});

// 当前技能的分类归属：从后端分类视图推导不可行（列表不带分配明细），
// 这里维护本地 assignment 缓存：进入面板/事件刷新时拉一次。
const assignment = ref({}); // skillName -> catID（"" 缺省 = 未分类）

async function loadAssignments() {
  try {
    assignment.value = (await bridgeCall("GetSkillAssignments")) || {};
  } catch {
    assignment.value = {};
  }
}

function catOf(name) {
  const cid = assignment.value[name] || "";
  // 指向已删除分类 → 未分类（与后端 ListSkillCategories 口径一致）。
  if (cid && !categories.value.some((c) => c.id === cid)) return "";
  return cid;
}

// ── 过滤管线（L1 分类 → L2 来源/模式 → L3 搜索）───────────────────
const filtered = ref([]);

function applyFilter() {
  const kw = (keyword.value || "").trim().toLowerCase();
  let out = skills.value;
  // L1 分类
  if (activeCat.value === "__uncat") {
    out = out.filter((s) => !catOf(s.name));
  } else if (activeCat.value) {
    out = out.filter((s) => catOf(s.name) === activeCat.value);
  }
  // L2 来源 + 模式
  if (activeSource.value) {
    out = out.filter((s) => (s.source || "") === activeSource.value);
  }
  if (activeMode.value) {
    out = out.filter((s) => (s.modeSlugs || []).includes(activeMode.value));
  }
  // L3 搜索
  if (kw) {
    out = out.filter(
      (s) => (s.name || "").toLowerCase().includes(kw) ||
        (s.description || "").toLowerCase().includes(kw)
    );
  }
  // 排序：名称升序（稳定可预期的"货架"顺序）
  out = [...out].sort((a, b) => (a.name || "").localeCompare(b.name || ""));
  filtered.value = out;
}

// L2 模式选项：当前全量技能 modeSlugs 汇总（动态货架，不写死内置模式）。
const modeOptions = computed(() => {
  const set = new Set();
  for (const s of skills.value) for (const m of s.modeSlugs || []) set.add(m);
  return [...set].sort();
});

// 全部计数（L1 页签徽标）。
const total = computed(() => skills.value.length);
function catCount(c) {
  return (c && c.count) || 0;
}

function selectCat(id) {
  activeCat.value = id;
  applyFilter();
}

// ── 编辑/查看抽屉 ────────────────────────────────────────────────
const editVisible = ref(false);
const viewOnly = ref(false); // builtin 只读
const saving = ref(false);
const form = ref(null); // { name, content, isNew, category }

async function openView(s) {
  try {
    const content = (await bridgeCall("SkillRaw", s.name)) || "";
    form.value = { name: s.name, content, isNew: false, category: catOf(s.name) };
    viewOnly.value = s.source === "builtin";
    editVisible.value = true;
  } catch (e) {
    Message.error(String(e));
  }
}

function openNew() {
  form.value = {
    name: "", content: NEW_TEMPLATE, isNew: true,
    category: activeCat.value === "__uncat" || !activeCat.value ? "" : activeCat.value,
  };
  viewOnly.value = false;
  editVisible.value = true;
}

async function save() {
  const f = form.value || {};
  const name = String(f.name || "").trim();
  if (!name) {
    Message.warning("请填写技能名（小写字母/数字/连字符）");
    return;
  }
  saving.value = true;
  try {
    if (viewOnly.value) {
      // builtin 只读技能：正文不可改，仅保存分类归属。
      await bridgeCall("SetSkillCategory", name, f.category || "");
      Message.success("分类已保存");
    } else {
      await bridgeCall("SaveSkill", name, f.content || "");
      await bridgeCall("SetSkillCategory", name, f.category || "").catch(() => {});
      Message.success("技能已保存");
    }
    editVisible.value = false;
    await load();
    applyFilter();
  } catch (e) {
    Message.error("保存失败：" + String(e));
  } finally {
    saving.value = false;
  }
}

function removeSkill(ev, s) {
  ev.stopPropagation();
  Modal.confirm({
    title: "删除技能",
    content: `确定删除「${s.name}」？将删除其技能目录（SKILL.md 及附带文件）。`,
    okText: "删除",
    cancelText: "取消",
    onOk: async () => {
      try {
        await bridgeCall("DeleteSkill", s.name);
        Message.success("已删除");
        await load();
        applyFilter();
      } catch (err) {
        Message.error(String(err));
      }
    },
  });
}

// ── 发布到市场（kind=skill；内置技能不可发布，后端同样拒绝）─────
const publishing = ref("");
async function publishSkill(s) {
  if (s.source === "builtin") {
    Message.warning("内置技能不可发布，请先另存为自定义技能");
    return;
  }
  publishing.value = s.name;
  try {
    await bridgeCall("PublishSkillToMarket", s.name);
    Message.success(`已发布到市场：${s.name}`);
  } catch (e) {
    Message.error("发布失败：" + String(e));
  } finally {
    publishing.value = "";
  }
}

// ── 改分类（卡片 hover「分类」下拉）───────────────────────────────
async function moveSkill(s, catID) {
  try {
    await bridgeCall("SetSkillCategory", s.name, catID || "");
    Message.success(catID ? `已移入「${catNameMap.value[catID] || catID}」` : "已移入未分类");
    await load();
    applyFilter();
  } catch (e) {
    Message.error(String(e));
  }
}

// ── 分类管理（新增/重命名/删除/排序）────────────────────────────
const mgrVisible = ref(false);
const newCatName = ref("");
const mgrSaving = ref(false);

// ── AI 分类（LLM 读取技能自动划分；autoCreate 开关控制是否允许新增分类）──
const aiCatLoading = ref(false);
const aiAutoCreate = ref(true);

async function loadAICatSwitch() {
  try {
    aiAutoCreate.value = (await bridgeCall("GetSkillAICategorizeEnabled")) !== false;
  } catch {
    aiAutoCreate.value = true; // 后端不可达时默认开（后端缺省即开）
  }
}

async function toggleAIAutoCreate(v) {
  try {
    await bridgeCall("SetSkillAICategorizeEnabled", !!v);
  } catch (e) {
    Message.error(String(e));
    aiAutoCreate.value = !v; // 回滚 UI
  }
}

async function runAICategorize() {
  if (aiCatLoading.value) return;
  aiCatLoading.value = true;
  try {
    await bridgeCall("AICategorizeSkills");
    Message.info("AI 分类已开始，读取全部技能中…完成后将自动刷新");
    mgrVisible.value = false;
  } catch (e) {
    Message.error(String(e));
  } finally {
    aiCatLoading.value = false;
  }
}

// 自定义分类列表（未分类虚拟区不参与管理操作）。
const userCats = computed(() => categories.value.filter((x) => x.id !== ""));

// 重命名弹窗（受控表单，替代 Modal innerHTML hack）。
const renameVisible = ref(false);
const renameForm = ref({ id: "", name: "" });

function openRename(c) {
  renameForm.value = { id: c.id, name: c.name };
  renameVisible.value = true;
}

// 重命名（@before-ok 语义：失败/校验不过时 resolve false，弹窗保持打开可修改后重试）
async function doRename() {
  const { id, name } = renameForm.value;
  const trimmed = (name || "").trim();
  if (!id || !trimmed) {
    Message.warning("请填写分类名");
    return false;
  }
  try {
    await bridgeCall("CategoryRename", id, trimmed);
    renameVisible.value = false;
    await load();
    return true;
  } catch (e) {
    Message.error("重命名失败：" + String(e));
    return false;
  }
}

function openCatMgr() {
  newCatName.value = "";
  mgrVisible.value = true;
  loadAICatSwitch();
}

async function addCategory() {
  const name = (newCatName.value || "").trim();
  if (!name) {
    Message.warning("请输入分类名");
    return;
  }
  mgrSaving.value = true;
  try {
    await bridgeCall("CategoryAdd", name);
    newCatName.value = "";
    await load();
  } catch (e) {
    Message.error(String(e));
  } finally {
    mgrSaving.value = false;
  }
}

const renameCategory = openRename;

function deleteCategory(c) {
  Modal.confirm({
    title: "删除分类",
    content: `确定删除分类「${c.name}」？其中 ${c.count} 个技能将自动移入「未分类」（技能本身不受影响）。`,
    okText: "删除",
    cancelText: "取消",
    onOk: async () => {
      try {
        await bridgeCall("CategoryDelete", c.id);
        if (activeCat.value === c.id) activeCat.value = "";
        await load();
        applyFilter();
      } catch (e) {
        Message.error(String(e));
      }
    },
  });
}

// 上下移排序：读全量定义 → 交换 Sort → 批量保存（未分类虚拟区不参与）。
async function moveCategory(c, dir) {
  const cats = userCats.value;
  const i = cats.findIndex((x) => x.id === c.id);
  const j = i + dir;
  if (i < 0 || j < 0 || j >= cats.length) return;
  const order = [...cats];
  [order[i], order[j]] = [order[j], order[i]];
  try {
    await bridgeCall(
      "SaveSkillCategories",
      order.map((x, idx) => ({ id: x.id, name: x.name, sort: idx + 1 }))
    );
    await load();
  } catch (e) {
    Message.error(String(e));
  }
}

// kaelvoris:skills 推送（后端保存/删除/改分类/远端同步后刷新：技能 + 分类计数 + 归属）
const offSkills = onEvent("kaelvoris:skills", () => {
  load()
    .then(() => loadAssignments())
    .then(applyFilter);
});
onBeforeUnmount(() => { if (typeof offSkills === "function") offSkills(); });

load()
  .then(() => loadAssignments())
  .then(applyFilter);
</script>

<template>
  <div class="skill-panel" :class="{ compact }">
    <!-- L1 分类页签（横向，数量徽标；像商品分类入口） -->
    <div class="skill-head panel-head">
      <span class="panel-icon"><IconBook /></span>
      <span class="panel-title">技能库</span>
      <span class="panel-sub">{{ total }} 个技能 · 内置/全局/项目</span>
      <span class="flex-sp"></span>
      <a-tooltip content="刷新">
        <button class="icon-btn" @click="load"><IconRefresh /></button>
      </a-tooltip>
      <a-button size="small" type="primary" @click="openNew">
        <template #icon><IconPlus /></template>
        新建技能
      </a-button>
    </div>

    <div class="cat-bar">
      <div class="cat-tabs">
        <button class="cat-tab" :class="{ active: activeCat === '' }" @click="selectCat('')">
          全部<span class="cnt">{{ total }}</span>
        </button>
        <button
          v-for="c in categories.filter((x) => x.id !== '')"
          :key="c.id"
          class="cat-tab"
          :class="{ active: activeCat === c.id }"
          @click="selectCat(c.id)"
        >
          {{ c.name }}<span class="cnt">{{ catCount(c) }}</span>
        </button>
        <button class="cat-tab" :class="{ active: activeCat === '__uncat' }" @click="selectCat('__uncat')">
          未分类<span class="cnt">{{ catCount(categories.find((x) => x.id === '')) }}</span>
        </button>
      </div>
      <a-button size="mini" type="text" class="cat-manage" @click="openCatMgr">
        <template #icon><IconFolderAdd /></template>
        管理分类
      </a-button>
    </div>

    <!-- L2 来源/模式筛选 chips（横向） -->
    <div class="filter-row">
      <div class="chip-group">
        <button class="chip" :class="{ active: activeSource === '' }" @click="activeSource = ''; applyFilter()">全部来源</button>
        <button v-for="(v, k) in sourceMap" :key="k" class="chip" :class="{ active: activeSource === k }" @click="activeSource = k; applyFilter()">
          {{ v.label }}
        </button>
      </div>
      <template v-if="modeOptions.length">
        <span class="filter-sep"></span>
        <div class="chip-group">
          <button class="chip" :class="{ active: activeMode === '' }" @click="activeMode = ''; applyFilter()">全模式</button>
          <button v-for="m in modeOptions" :key="m" class="chip" :class="{ active: activeMode === m }" @click="activeMode = m; applyFilter()">
            {{ m }}
          </button>
        </div>
      </template>
    </div>

    <!-- L3 搜索 + 计数 -->
    <div class="tool-row">
      <a-input-search
        v-model="keyword"
        placeholder="搜索技能名称/描述…"
        size="small"
        allow-clear
        style="width: 260px"
        @input="applyFilter"
        @clear="applyFilter"
        @search="applyFilter"
      >
        <template #prefix><IconSearch /></template>
      </a-input-search>
      <span class="result-hint">{{ filtered.length }} / {{ total }} 个技能</span>
      <span class="flex-sp"></span>
    </div>

    <a-spin :loading="loading" style="display: block; width: 100%">
      <div v-if="filtered.length" class="skill-grid">
        <div v-for="s in filtered" :key="s.name + (s.path || '')" class="skill-card" @click="openView(s)">
          <div class="skill-row1">
            <span class="skill-name">{{ s.name }}</span>
            <a-tag size="small" :color="(sourceMap[s.source] || {}).color || 'gray'">
              {{ (sourceMap[s.source] || {}).label || s.source || "未知" }}
            </a-tag>
            <span class="flex-sp"></span>
            <!-- 操作区：hover 显现（改分类/编辑/删除） -->
            <span class="skill-ops">
              <a-dropdown v-if="s.source !== 'builtin'" trigger="click" @select="(v) => moveSkill(s, v)">
                <a-button size="mini" type="text" title="移动到分类" @click.stop>
                  <template #icon><IconSwap /></template>
                </a-button>
                <template #content>
                  <a-doption v-for="c in categories" :key="c.id" :value="c.id" :class="{ 'op-current': catOf(s.name) === c.id }">
                    {{ c.name }}
                  </a-doption>
                </template>
              </a-dropdown>
              <a-button size="mini" type="text" :title="s.source === 'builtin' ? '查看' : '编辑'" @click.stop="openView(s)">
                <template #icon><IconEye v-if="s.source === 'builtin'" /><IconEdit v-else /></template>
              </a-button>
              <a-button
                v-if="s.source !== 'builtin'"
                size="mini"
                type="text"
                title="发布到市场"
                :loading="publishing === s.name"
                @click.stop="publishSkill(s)"
              >
                <template #icon><IconUpload /></template>
              </a-button>
              <a-button v-if="s.source !== 'builtin'" size="mini" type="text" status="danger" title="删除" @click.stop="removeSkill($event, s)">
                <template #icon><IconDelete /></template>
              </a-button>
            </span>
          </div>
          <div class="skill-desc" :title="s.description">{{ s.description || "（无描述）" }}</div>
          <div class="skill-foot">
            <span class="skill-cat" :title="'所属分类：' + (catNameMap[catOf(s.name)] || '未分类')">
              {{ catNameMap[catOf(s.name)] || "未分类" }}
            </span>
            <span v-if="s.modeSlugs && s.modeSlugs.length" class="skill-modes">
              <a-tag v-for="m in s.modeSlugs" :key="m" size="small" color="arcoblue">{{ m }}</a-tag>
            </span>
          </div>
        </div>
      </div>
      <div v-else class="skill-empty empty-block">
	        <div class="empty-ico-chip"><IconBook /></div>
	        <div class="empty-block-title">{{ keyword ? "没有匹配的技能" : "暂无技能" }}</div>
	        <div class="empty-block-hint">{{ keyword ? "换个关键词试试" : "点右上角「新建技能」创建，或将 SKILL.md 目录放入 ~/.agents/skills、工作区 .kaelvoris/skills" }}</div>
      </div>
    </a-spin>

    <!-- 查看/编辑抽屉 -->
    <a-drawer
      v-model:visible="editVisible"
      :title="(form && form.isNew ? '新建技能' : '技能：' + (form && form.name || '')) + (viewOnly ? '（内置只读）' : '')"
      :width="560"
      :footer="true"
      unmount-on-close
    >
      <template v-if="form">
        <div v-if="form.isNew" class="form-line">
          <a-input v-model="form.name" placeholder="技能名：小写字母/数字/连字符，如 my-skill" allow-clear />
        </div>
        <div class="form-line form-2col">
          <div>
            <div class="form-label">所属分类</div>
            <a-select v-model="form.category" size="small" allow-clear placeholder="未分类" style="width: 100%">
              <a-option v-for="c in categories" :key="c.id" :value="c.id">{{ c.name }}</a-option>
            </a-select>
          </div>
        </div>
        <a-alert v-if="viewOnly" type="info" style="margin: 10px 0">
          内置技能随发行版分发，正文只读（同名放入用户技能目录即可覆盖）；所属分类可调整，点「保存分类」生效。
        </a-alert>
        <a-textarea
          v-model="form.content"
          :auto-size="{ minRows: 16, maxRows: 30 }"
          :readonly="viewOnly"
          class="skill-editor"
          placeholder="SKILL.md 全文（frontmatter + 正文）"
        />
      </template>
      <template #footer>
        <a-space>
          <a-button @click="editVisible = false">关闭</a-button>
          <a-button type="primary" :loading="saving" @click="save">{{ viewOnly ? "保存分类" : "保存" }}</a-button>
        </a-space>
      </template>
    </a-drawer>

    <!-- 重命名分类弹窗 -->
    <a-modal
      v-model:visible="renameVisible"
      title="重命名分类"
      :width="360"
      ok-text="确定"
      cancel-text="取消"
      @before-ok="doRename"
    >
      <a-input v-model="renameForm.name" placeholder="分类名" allow-clear @keyup.enter="doRename" />
    </a-modal>

    <!-- 分类管理抽屉 -->
    <a-drawer v-model:visible="mgrVisible" title="分类管理" :width="420" unmount-on-close>
      <div class="cat-mgr-tip">
        分类仅用于技能库面板的挑选与导航，不影响 Agent 可用技能；删除分类后其下技能自动移入「未分类」。
      </div>

      <!-- AI 分类：LLM 读取全部技能自动划分；开关控制是否允许自动新增分类 -->
      <div class="cat-ai-card">
        <div class="cat-ai-row">
          <span class="cat-ai-title"><IconThunderbolt /> AI 智能分类</span>
          <a-button size="mini" type="primary" :loading="aiCatLoading" @click="runAICategorize">
            {{ aiCatLoading ? "分类中…" : "开始分类" }}
          </a-button>
        </div>
        <div class="cat-ai-desc">AI 读取全部技能后自动生成分类名称并划分；已有分类将优先复用。</div>
        <div class="cat-ai-row cat-ai-switch">
          <span>允许 AI 自动新增分类</span>
          <a-switch size="small" v-model="aiAutoCreate" @change="toggleAIAutoCreate" />
        </div>
        <div class="cat-ai-hint">关闭后：AI 仅归入既有分类，需要新建分类的技能保持「未分类」。</div>
      </div>

      <div class="cat-mgr-list">
        <div v-for="(c, i) in userCats" :key="c.id" class="cat-mgr-item">
          <IconDragDot class="cat-mgr-drag" />
          <span class="cat-mgr-name">{{ c.name }}</span>
          <span class="cat-mgr-cnt">{{ c.count }} 技能</span>
          <a-space size="mini">
            <a-button size="mini" type="text" :disabled="i === 0" @click="moveCategory(c, -1)">↑</a-button>
            <a-button size="mini" type="text" :disabled="i === userCats.length - 1" @click="moveCategory(c, 1)">↓</a-button>
            <a-button size="mini" type="text" @click="renameCategory(c)">重命名</a-button>
            <a-button size="mini" type="text" status="danger" @click="deleteCategory(c)">删除</a-button>
          </a-space>
        </div>
        <div v-if="!userCats.length" class="cat-mgr-empty">
          暂无自定义分类，所有技能都在「未分类」区。
        </div>
      </div>
      <div class="cat-mgr-add">
        <a-input v-model="newCatName" size="small" placeholder="新分类名，如：前端开发" allow-clear @keyup.enter="addCategory" />
        <a-button size="small" type="primary" :loading="mgrSaving" @click="addCategory">
          <template #icon><IconPlus /></template>
          新增
        </a-button>
      </div>
    </a-drawer>
  </div>
</template>

<style scoped>
.skill-panel {
  padding: 16px 20px;
  overflow-y: auto;
  height: 100%;
}
.skill-head { gap: 8px; flex-shrink: 0; padding-left: 0; padding-right: 0; border-bottom: none; margin-bottom: 2px; }
.flex-sp { flex: 1; }

/* L1 分类页签：横向滚动条（电商分类页签） */
.cat-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.cat-tabs {
  display: flex;
  align-items: center;
  gap: 6px;
  overflow-x: auto;
  flex: 1;
  padding-bottom: 2px;
  scrollbar-width: thin;
}
	.cat-tab {
	  flex-shrink: 0;
	  display: inline-flex;
	  align-items: center;
	  gap: 5px;
	  border: 1px solid var(--color-border-2, #e5e6eb);
	  background: transparent;
	  color: var(--color-text-2, #4e5969);
	  border-radius: 999px;
	  padding: 3px 12px;
	  font-size: var(--fs-caption);
	  cursor: pointer;
	  transition: all 0.15s;
	  white-space: nowrap;
	}
.cat-tab:hover {
  border-color: rgb(var(--primary-6));
  color: rgb(var(--primary-6));
}
.cat-tab.active {
  background: rgb(var(--primary-6));
  border-color: rgb(var(--primary-6));
  color: #fff;
}
.cat-tab .cnt {
  font-size: 11px;
  opacity: 0.75;
  background: rgba(0, 0, 0, 0.06);
  border-radius: 999px;
  padding: 0 6px;
  line-height: 16px;
}
.cat-tab.active .cnt { background: rgba(255, 255, 255, 0.22); }
.cat-manage { flex-shrink: 0; }

/* L2 筛选 chips */
.filter-row {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 8px;
  flex-wrap: wrap;
}
.chip-group {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}
.chip {
  border: none;
  background: var(--color-fill-2, #f2f3f5);
  color: var(--color-text-3, #86909c);
  border-radius: 4px;
  padding: 2px 9px;
  font-size: 12px;
  cursor: pointer;
  transition: all 0.15s;
}
.chip:hover { color: rgb(var(--primary-6)); }
.chip.active {
  background: rgba(var(--primary-6), 0.12);
  color: rgb(var(--primary-6));
  font-weight: 500;
}
.filter-sep {
  width: 1px;
  height: 14px;
  background: var(--color-border-2, #e5e6eb);
}

/* L3 工具行 */
.tool-row {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
}
.result-hint {
  font-size: 12px;
  color: var(--color-text-3, #86909c);
}

/* 卡片网格（货架） */
.skill-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(250px, 1fr));
  gap: 10px;
}
.skill-card {
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  background: var(--bg-card);
  padding: 10px 12px;
  cursor: pointer;
  transition: var(--transition);
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.skill-card:hover {
  background: var(--bg-card);
  border-color: var(--primary);
  transform: translateY(-1px);
  box-shadow: var(--shadow-sm);
}
.skill-card:active { transform: translateY(0); }
.skill-row1 {
  display: flex;
  align-items: center;
  gap: 8px;
}
	.skill-name {
	  font-weight: 600;
	  font-family: var(--font-mono, monospace);
	  font-size: var(--fs-body);
	  overflow: hidden;
	  text-overflow: ellipsis;
	  white-space: nowrap;
	}
.skill-ops {
  display: inline-flex;
  gap: 0;
  opacity: 0;
  transition: opacity 0.15s;
}
.skill-card:hover .skill-ops { opacity: 1; }
.skill-desc {
  font-size: 12px;
  color: var(--color-text-3, #86909c);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-height: 17px;
}
.skill-foot {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: 2px;
}
.skill-cat {
  font-size: 11px;
  color: var(--color-text-3, #86909c);
  background: var(--color-fill-2, #f2f3f5);
  border-radius: 3px;
  padding: 1px 6px;
  max-width: 90px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.skill-modes {
  display: flex;
  gap: 4px;
  flex-wrap: wrap;
  overflow: hidden;
}
.op-current { color: rgb(var(--primary-6)); font-weight: 500; }

/* 编辑抽屉 */
.form-line { margin: 10px 0; }
.form-2col { display: flex; gap: 10px; }
.form-label {
  font-size: 12px;
  color: var(--color-text-3, #86909c);
  margin-bottom: 4px;
}
.skill-editor {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  margin-top: 4px;
}

/* 分类管理抽屉 */
/* AI 分类卡片 */
.cat-ai-card {
  border: 1px solid var(--color-border-2, #e5e6eb);
  border-radius: 8px;
  padding: 10px 12px;
  margin-bottom: 12px;
  background: var(--color-fill-1, #f7f8fa);
}
.cat-ai-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}
	.cat-ai-title {
	  display: inline-flex;
	  align-items: center;
	  gap: 5px;
	  font-weight: 600;
	  font-size: var(--fs-body);
	}
.cat-ai-desc {
  font-size: 12px;
  color: var(--color-text-3, #86909c);
  margin: 6px 0;
  line-height: 1.5;
}
	.cat-ai-switch {
	  font-size: var(--fs-caption);
	  color: var(--color-text-2, #4e5969);
	}
	.cat-ai-hint {
	  font-size: var(--fs-micro);
	  color: var(--color-text-4, #c9cdd4);
	  margin-top: 5px;
	  line-height: 1.5;
	}
.cat-mgr-tip {
  font-size: 12px;
  color: var(--color-text-3, #86909c);
  background: var(--color-fill-1, #f7f8fa);
  border-radius: 6px;
  padding: 8px 10px;
  margin-bottom: 12px;
  line-height: 1.6;
}
.cat-mgr-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-bottom: 12px;
}
.cat-mgr-item {
  display: flex;
  align-items: center;
  gap: 6px;
  border: 1px solid var(--color-border-2, #e5e6eb);
  border-radius: 6px;
  padding: 6px 10px;
}
.cat-mgr-drag { color: var(--color-text-4, #c9cdd4); }
.cat-mgr-name {
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.cat-mgr-cnt {
  font-size: 11px;
  color: var(--color-text-3, #86909c);
  margin-left: 2px;
  white-space: nowrap;
}
.cat-mgr-item .arco-space { margin-left: auto; }
.cat-mgr-empty {
  font-size: 12px;
  color: var(--color-text-3, #86909c);
  text-align: center;
  padding: 16px 0;
}
.cat-mgr-add {
  display: flex;
  gap: 8px;
  align-items: center;
}
</style>
