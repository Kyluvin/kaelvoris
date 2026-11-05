<script setup>
// ═════════════════════════════════════════════════════════════════
// 资源管理面板（doc/09-设计方案 §5 · RBAC 优化版）：
//   左侧部门组织树（点击部门 = 过滤岗位与资源；「全部」看全部）+ 右侧 Tab：
//   会话（session）/ 渠道（channel）/ 定时任务（cron）。
//   每条资源行内直接改绑定（BindEditor：员工/岗位/分配规则），会话行运行中禁改。
//   岗位视图：按岗位聚合资源（部门范围 ScopeAll/ScopeDeptIDs 过滤）。
//   权限链 Tab（业界 RBAC）：用户 → 岗位 → 角色 → 权限包，直绑角色仅兜底：
//     - 岗位卡：映射角色（岗位↔角色主路径）+ 成员（成员卡可直改直绑角色）
//       + 关联资源行（资源→岗位→角色→有效权限链提示）。
//     - 角色视图：权限包结构化展示 + 引用反查（岗位/员工引用计数，删除保护数据源同语义）。
// 后端：ListResourceBindings / SaveResourceBinding / SetSessionBinding / GetOrg /
//       SavePosition / SaveEmployee（成员直绑角色）。
// ═════════════════════════════════════════════════════════════════
import { computed, h, onBeforeUnmount, onMounted, ref } from "vue";
import { Message, Modal } from "@arco-design/web-vue";
import {
	  IconRefresh, IconSend, IconClockCircle, IconUserGroup, IconMessage, IconSafe, IconStorage,
	} from "@arco-design/web-vue/es/icon";
import { bridgeCall, onEvent } from "../bridge/wails.js";
import { store, openSession, loadEmployees } from "../stores/app.js";
import BindEditor from "./BindEditor.vue";

// embedded=true 时作为「数字员工」面板的内嵌 Tab：隐藏标题栏，「管理」跳转改为页签内切换
const props = defineProps({ embedded: { type: Boolean, default: false } });
const emit = defineEmits(["nav-employee"]);
function goEmployeeOrg() {
  if (props.embedded) emit("nav-employee", "org");
  else store.activeMain = "employee";
}

const activeTab = ref("channel"); // channel | cron | session | positions | perms
const bindings = ref([]);         // ResourceBinding[]
const loading = ref(false);

// ══ 岗位-角色权限链（业界 RBAC：用户→岗位→角色→权限；直绑角色仅兑底）══
const roles = ref([]);            // Role[]（org.json 角色字典）
const roleName = computed(() => {
  const m = {};
  for (const r of roles.value) m[r.id] = r;
  return m;
});

// 角色权限包摘要文案（角色卡展示）。
function roleSummary(r) {
  if (!r) return "";
  const parts = [];
  parts.push(r.mcpDisabled ? "MCP禁用" : r.mcpMode === "whitelist" ? "MCP白名单" : r.mcpMode === "blacklist" ? "MCP黑名单" : "MCP未设置");
  if ((r.mcpServers || []).length) parts.push("服务器" + r.mcpServers.length);
  if (r.skillsDisabled) parts.push("技能禁用");
  else if ((r.skillsDeny || []).length) parts.push("技能黑名单" + r.skillsDeny.length);
  if (r.readOnly) parts.push("只读");
  if ((r.dirs || []).length) parts.push("目录" + r.dirs.length);
  return parts.join(" · ");
}

// 岗位角色编辑弹窗（直接改岗位 RoleIDs 映射；保存经 SavePosition 校验存在性）。
const posRoleVisible = ref(false);
const posRoleForm = ref(null); // { position, roleIds: [] }
function openPosRole(p) {
  posRoleForm.value = { position: p, roleIds: [...(p.roleIds || [])] };
  posRoleVisible.value = true;
}
async function savePosRole() {
  const f = posRoleForm.value || {};
  const p = f.position || {};
  try {
    await bridgeCall("SavePosition", JSON.parse(JSON.stringify({ ...p, roleIds: f.roleIds })));
    Message.success("岗位角色映射已保存");
    posRoleVisible.value = false;
    await loadOrg();
    return true;
  } catch (e) {
    Message.error("保存失败：" + String(e));
    return false;
  }
}

// 岗位下员工（部门导航联动：岗位卡展示该岗位在职员工）。
const posEmployees = computed(() => {
  const m = {};
  for (const e of store.employees || []) {
    if (!e.positionId) continue;
    (m[e.positionId] = m[e.positionId] || []).push(e);
  }
  return m;
});

// 该岗位关联的资源条目（三源聚合；positionId 优先，仅绑员工时按员工岗位归组）。
function posResources(posId) {
  return bindings.value.filter((b) => {
    if (b.positionId === posId) return true;
    if (!b.positionId && b.employeeId) {
      const e = empName.value[b.employeeId];
      return !!(e && e.positionId === posId);
    }
    return false;
  });
}

// ── 字典（员工/岗位名映射）───────────────────────────────────────
const empName = computed(() => {
  const m = {};
  for (const e of store.employees || []) m[e.id] = e;
  return m;
});
const positions = ref([]);
const posName = computed(() => {
  const m = {};
  for (const p of positions.value) m[p.id] = p;
  return m;
});

async function load() {
  loading.value = true;
  try {
    bindings.value = (await bridgeCall("ListResourceBindings")) || [];
  } catch (e) {
    console.warn("ListResourceBindings failed:", e);
  } finally {
    loading.value = false;
  }
}

async function loadOrg() {
  try {
    const view = await bridgeCall("GetOrg");
    positions.value = (view && view.positions) || [];
    roles.value = (view && view.roles) || [];
  } catch { positions.value = []; roles.value = []; }
}

// ── 左侧部门树导航（点击 = 圈定岗位范围；「全部」清空）────────────
const deptTree = ref([]);
const curDeptId = ref(""); // "" = 全部；"none" = 通用岗位/未归属
async function loadTree() {
  try {
    deptTree.value = (await bridgeCall("OrgTree")) || [];
  } catch { deptTree.value = []; }
}
// 部门子树 ID 集合（选中部门 + 全部子孙）。
function subtreeIds(id) {
  const ids = new Set([id]);
  const walk = (nodes) => {
    for (const n of nodes || []) {
      if (ids.has(n.parentId) || ids.has(n.id)) {
        ids.add(n.id);
        walk(n.children);
      }
    }
  };
  // 顶层展开一次：命中即收集其子树
  const collect = (nodes) => {
    for (const n of nodes || []) {
      if (n.id === id) { ids.add(n.id); walkDeep(n); }
      else collect(n.children);
    }
  };
  const walkDeep = (n) => { for (const c of n.children || []) { ids.add(c.id); walkDeep(c); } };
  collect(deptTree.value);
  return ids;
}
const curScopeIds = computed(() => (curDeptId.value && curDeptId.value !== "none" ? subtreeIds(curDeptId.value) : null));

// 岗位是否落在当前选中部门范围（Position.InDeptScope 同语义前端版）。
function posInScope(p) {
  if (!curScopeIds.value) return true;
  if (p.scopeAll) return true;
  if (p.deptId && curScopeIds.value.has(p.deptId)) return true;
  for (const d of p.scopeDeptIds || []) {
    if (curScopeIds.value.has(d)) return true;
  }
  return false;
}
// 可见岗位（选中「通用/未归属」时仅显示无归属部门的岗位）。
const visiblePositions = computed(() => {
  if (curDeptId.value === "none") return positions.value.filter((p) => !p.deptId);
  return positions.value.filter(posInScope);
});

// ── 交叉筛选（员工/岗位下拉，Tab 间共享）─────────────────────────
const filterEmp = ref("");
const filterPos = ref("");
function matchFilter(b) {
  if (filterEmp.value && b.employeeId !== filterEmp.value) return false;
  if (filterPos.value && b.positionId !== filterPos.value) return false;
  // 部门导航联动：资源按岗位快照 / 员工当前岗位归入可见范围。
  if (curDeptId.value) {
    const pid = b.positionId || (empName.value[b.employeeId] || {}).positionId || "";
    if (curDeptId.value === "none") {
      if (pid) return false;
      return true;
    }
    const p = posName.value[pid];
    if (!p || !posInScope(p)) return false;
  }
  return true;
}
const channelRows = computed(() => bindings.value.filter((b) => b.type === "channel" && matchFilter(b)));
const cronRows = computed(() => bindings.value.filter((b) => b.type === "cron" && matchFilter(b)));
const sessionRows = computed(() => bindings.value.filter((b) => b.type === "session" && matchFilter(b)));

// ── 岗位视图（按岗位聚合资源；行内改绑定）────────────────────────
const positionRows = computed(() => {
  const byPos = new Map();
  const put = (posId, b) => {
    if (!byPos.has(posId)) byPos.set(posId, []);
    byPos.get(posId).push(b);
  };
  for (const b of bindings.value) {
    if (!matchFilter(b)) continue;
    let pos = b.positionId;
    if (!pos && b.employeeId) {
      const e = empName.value[b.employeeId];
      if (e && e.positionId) pos = e.positionId;
    }
    if (pos) put(pos, b);
  }
  return visiblePositions.value
    .map((p) => ({ position: p, rows: byPos.get(p.id) || [] }))
    .filter((r) => r.rows.length);
});

// 行内编辑展开态（type+id 定位唯一行）
const editingKey = ref("");
function toggleEdit(b) {
  const k = b.type + "\u0000" + b.id;
  editingKey.value = editingKey.value === k ? "" : k;
}

// 绑定展示徽标（保存后刷新整表）
function bindLabel(b) {
  const parts = [];
  if (b.employeeId) {
    const e = empName.value[b.employeeId];
    parts.push(e ? (e.avatar || "🤖") + " " + e.name : b.employeeId);
  }
  if (b.positionId) parts.push("岗位：" + ((posName.value[b.positionId] || {}).name || b.positionId));
  if (!parts.length) return b.type === "session" ? "普通会话（未绑定）" : "未绑定（裸跑）";
  return parts.join(" · ") + ruleTextOf(b);
}
function ruleTextOf(b) {
  if (!b.assignRule) return "";
  switch (b.assignRule) {
    case "round_robin": return " · 轮转";
    case "least_loaded": return " · 最闲";
    default: return "";
  }
}

async function onSaved() {
  await load();
}

// 渠道启停走渠道页（绑定编辑器不复制启停语义——SaveChannels 整表校验）
function goChannelSettings() {
  store.settingsVisible = true;
  store.settingsPage = "channels";
}

// 会话跳转聊天（openSession 对齐侧栏点开同路：选择器/员工/节点联动恢复）
async function goSession(id) {
  store.activeMain = "chat";
  if (id) await openSession(id);
}

// 绑定是否落在部门子树范围（左侧导航计数；scopeAll 全公司岗位每部门都计）。
function posOfBinding(b, deptId) {
  const ids = subtreeIds(deptId);
  const pid = b.positionId || (empName.value[b.employeeId] || {}).positionId || "";
  const p = posName.value[pid];
  if (!p) return false;
  if (p.scopeAll) return true;
  if (p.deptId && ids.has(p.deptId)) return true;
  for (const d of p.scopeDeptIds || []) if (ids.has(d)) return true;
  return false;
}

		// 权限 Tab：可见岗位全量（含未关联资源的岗位——权限链主视图，不按资源过滤）。
		const permRows = computed(() => visiblePositions.value);

		// 角色视图（权限 Tab 下半区）：权限包结构化展示 + 引用反查。
		// 引用统计与后端 DeleteRole 门禁同语义：岗位 RoleIDs 映射 + 员工直绑 RoleIDs。
		const roleRefs = computed(() => {
		  const m = {};
		  const ensure = (rid) => {
		    if (!m[rid]) m[rid] = { positions: [], employees: [] };
		    return m[rid];
		  };
			  for (const p of positions.value) {
			    for (const rid of p.roleIds || []) ensure(rid).positions.push(p.name);
			  }
			  for (const e of store.employees || []) {
			    for (const rid of e.roleIds || []) ensure(rid).employees.push(e.name);
			  }
			  return m;
			});

	// 成员直绑保存后刷新：saveEmpRole 成功路径主动 loadEmployees() 兜底
	// （后端 SaveEmployee 同时推 kaelvoris:employees，store 全局订阅双保险；
	// posEmployees/roleRefs 为 computed 自动跟随，组件内无需额外订阅）。

// 成员直绑角色编辑弹窗（用户设计图「用户也可直接绑角色（兑底）」）：
// 仅改 roleIds，其余字段原样回传 SaveEmployee（后端整存整取）。
const empRoleVisible = ref(false);
const empRoleForm = ref(null); // { employee, roleIds: [] }
function openEmpRole(e) {
  empRoleForm.value = { employee: e, roleIds: [...(e.roleIds || [])] };
  empRoleVisible.value = true;
}
async function saveEmpRole() {
  const f = empRoleForm.value || {};
  const e = f.employee || {};
  try {
    await bridgeCall("SaveEmployee", JSON.parse(JSON.stringify({ ...e, roleIds: f.roleIds })));
	    Message.success("直绑角色已保存");
	    empRoleVisible.value = false;
	    await loadEmployees(); // 主动刷新 store.employees（事件推送的兜底；mock 预览环境无事件）
	    return true;
  } catch (err) {
    Message.error("保存失败：" + String(err));
    return false;
  }
}

// 有效角色链摘要（资源行权限链徽标：资源→岗位→角色→权限包）。
// 岗位映射角色 ∪ 员工直绑角色（员工仅绑无岗位快照时）；与 applyEmpRole 同语义。
function effRoleChips(b) {
  const posId = b.positionId || (empName.value[b.employeeId] || {}).positionId || "";
  const p = posName.value[posId];
  const ids = new Set(p && p.roleIds || []);
  const e = empName.value[b.employeeId];
  for (const rid of (e && e.roleIds) || []) ids.add(rid);
  return [...ids].map((rid) => roleName.value[rid]).filter(Boolean);
}

	// 角色权限包结构化标签（角色字典卡；比 roleSummary 摘要更直观的枚举形态）。
	function rolePermTags(r) {
	  if (!r) return [];
	  const tags = [];
	  if (r.mcpDisabled) tags.push("MCP 禁用");
	  else if (r.mcpMode === "whitelist") tags.push((r.mcpTools || []).length ? "MCP 白名单×" + r.mcpTools.length : "MCP 白名单");
	  else if (r.mcpMode === "blacklist") tags.push("MCP 黑名单");
	  if ((r.mcpServers || []).length) tags.push("服务器×" + r.mcpServers.length);
	  if (r.skillsDisabled) tags.push("技能禁用");
	  else if ((r.skillsDeny || []).length) tags.push("技能黑名单×" + r.skillsDeny.length);
	  if (r.readOnly) tags.push("只读");
	  if ((r.dirs || []).length) tags.push("目录×" + r.dirs.length);
	  return tags;
	}

// 岗位全部角色包合并摘要（岗位卡权限行；收紧式合并语义提示）。
function permPackText(roleIds) {
  const parts = [];
  for (const rid of roleIds || []) {
    const r = roleName.value[rid];
    if (!r) continue;
    const s = roleSummary(r);
    if (s) parts.push("「" + r.name + "」" + s);
  }
  return parts.length ? parts.join("；") + " —— 任一角色禁用即生效（收紧式合并）" : "角色未配置权限包";
}

	// 岗位范围摘要（范围徽标 title：全公司/指定多部门名明细）。
	function scopeTitle(p) {
	  if (!p) return "";
	  if (p.scopeAll) return "岗位范围：全公司（左侧任意部门下均可见）";
	  const ids = p.scopeDeptIds || [];
	  if (!ids.length) return "岗位范围：仅归属部门（含子树）";
	  const names = ids.map((d) => (deptFlatName.value[d] || d));
	  return "岗位范围：" + names.join("、") + "（含子树）";
	}
	// 部门扁平名映射（范围明细展示）。
	const deptFlatName = computed(() => {
	  const m = {};
	  const walk = (list) => {
	    for (const d of list || []) { m[d.id] = d.name; walk(d.children); }
	  };
	  walk(deptTree.value);
	  return m;
	});

	// ResNavNode 递归部门节点（渲染函数自引用；计数 = 范围内岗位关联资源数）。
const ResNavNode = {
  name: "ResNavNode",
  props: { node: { type: Object, required: true }, cur: { type: String, default: "" } },
  emits: ["select"],
  setup(props, { emit }) {
    const expanded = ref(true);
    return () => {
      const n = props.node;
      const kids = n && n.children && n.children.length
        ? h("div", { style: { paddingLeft: "12px" } },
            n.children.map((c) => h(ResNavNode, {
              key: c.id, node: c, cur: props.cur,
              onSelect: (id) => emit("select", id),
            })))
        : null;
      const cnt = bindings.value.filter((b) => posOfBinding(b, n.id)).length;
      // 岗位计数：范围内可见岗位数（ScopeAll/ScopeDeptIDs 语义，含子树展开）。
      const posCnt = positions.value.filter((p) => {
        if (p.scopeAll) return true;
        const ids = new Set([n.id]);
        const walk = (nodes) => {
          for (const c of nodes || []) { ids.add(c.id); walk(c.children); }
        };
        walk(n.children);
        if (ids.has(p.deptId)) return true;
        return (p.scopeDeptIds || []).some((d) => ids.has(d));
      }).length;
      const label = [posCnt ? posCnt + " 岗" : "", cnt ? cnt + " 资源" : ""].filter(Boolean).join(" · ");
      return h("div", { class: "res-nav-node" }, [
        h("div", {
          class: ["res-nav-row", { on: props.cur === n.id }],
          onClick: () => emit("select", n.id),
        }, [
          h("span", {
            class: "res-nav-caret",
            onClick: (e) => { e.stopPropagation(); expanded.value = !expanded.value; },
          }, expanded.value ? "▾" : "▸"),
          h("span", { class: "res-nav-name", title: n.name }, n.name),
          h("span", { class: "res-nav-count", title: posCnt + " 个岗位 · " + cnt + " 项资源" }, label),
        ]),
        expanded.value && kids ? kids : null,
      ]);
    };
  },
};

// kaelvoris:cron 推送联动（任务增删改后绑定表同步刷新）
const offCron = onEvent("kaelvoris:cron", () => load());

onMounted(async () => {
  await Promise.all([load(), loadOrg(), loadTree()]);
});
	onBeforeUnmount(() => {
	  if (typeof offCron === "function") offCron();
	});

const typeBadge = { channel: "渠道", cron: "定时任务", session: "会话" };
</script>

<template>
  <div class="res-panel" :class="{ 'res-embedded': embedded }">
 <!-- 头部：标题 + 交叉筛选 + 刷新（内嵌模式隐藏标题组，仅留筛选 + 刷新） -->
     <div class="res-head panel-head">
       <span v-if="!embedded" class="panel-icon"><IconStorage /></span>
       <span v-if="!embedded" class="res-title">资源管理</span>
       <span v-if="!embedded" class="panel-sub">会话 · 渠道 · 定时任务 → 岗位 / 员工绑定</span>
      <span class="flex-sp"></span>
      <a-select v-model="filterEmp" size="mini" allow-clear placeholder="按员工筛选" style="width: 150px">
        <a-option v-for="e in store.employees" :key="e.id" :value="e.id">
          {{ (e.avatar || "🤖") + " " + e.name }}
        </a-option>
      </a-select>
      <a-select v-model="filterPos" size="mini" allow-clear placeholder="按岗位筛选" style="width: 150px">
        <a-option v-for="p in positions" :key="p.id" :value="p.id">{{ p.name }}</a-option>
      </a-select>
      <a-tooltip content="刷新">
        <button class="icon-btn" @click="load"><IconRefresh /></button>
      </a-tooltip>
    </div>

    <div class="res-main">
      <!-- 左侧：部门组织树（点击查看该部门范围内的岗位与资源） -->
      <aside class="res-nav">
        <div class="res-nav-head">
          <span>组织</span>
          <a-button size="mini" type="text" @click="goEmployeeOrg">管理</a-button>
        </div>
        <div class="res-nav-list">
          <div class="res-nav-row" :class="{ on: curDeptId === '' }" @click="curDeptId = ''">
            <span class="res-nav-caret">•</span>
            <span class="res-nav-name">全部资源</span>
            <span class="res-nav-count">{{ bindings.length }}</span>
          </div>
          <div class="res-nav-row" :class="{ on: curDeptId === 'none' }" @click="curDeptId = 'none'">
            <span class="res-nav-caret">•</span>
            <span class="res-nav-name">通用 / 未归属</span>
            <span class="res-nav-count">{{ positions.filter((p) => !p.deptId).length }}</span>
          </div>
          <div v-if="!deptTree.length" class="res-nav-empty">暂无部门<br /><span class="res-nav-hint">在「数字员工 → 组织架构」中创建</span></div>
          <ResNavNode
            v-for="d in deptTree"
            :key="d.id"
            :node="d"
            :cur="curDeptId"
            @select="(id) => { curDeptId = id; }"
          />
        </div>
      </aside>

      <!-- 右侧：三 Tab -->
      <div class="res-right">
        <div class="res-tabs">
          <button :class="{ active: activeTab === 'channel' }" @click="activeTab = 'channel'">
            <IconSend /> 渠道<span v-if="channelRows.length">（{{ channelRows.length }}）</span>
          </button>
          <button :class="{ active: activeTab === 'cron' }" @click="activeTab = 'cron'">
            <IconClockCircle /> 定时任务<span v-if="cronRows.length">（{{ cronRows.length }}）</span>
          </button>
          <button :class="{ active: activeTab === 'session' }" @click="activeTab = 'session'">
            <IconMessage /> 会话<span v-if="sessionRows.length">（{{ sessionRows.length }}）</span>
          </button>
          <button :class="{ active: activeTab === 'positions' }" @click="activeTab = 'positions'">
            <IconUserGroup /> 岗位视图<span v-if="positionRows.length">（{{ positionRows.length }}）</span>
          </button>
          <button :class="{ active: activeTab === 'perms' }" @click="activeTab = 'perms'">
            <IconSafe /> 岗位权限<span v-if="permRows.length">（{{ permRows.length }}）</span>
          </button>
        </div>

        <a-spin :loading="loading" style="display: block; width: 100%; flex: 1; min-height: 0; overflow: hidden">
          <div class="res-body">
            <!-- Tab1 消息渠道 -->
            <template v-if="activeTab === 'channel'">
              <div v-if="channelRows.length" class="res-list">
                <div v-for="b in channelRows" :key="b.id" class="res-card">
                  <div class="res-row1">
                    <span class="res-name" :title="b.name">{{ b.name }}</span>
                    <span class="res-badge" :class="b.enabled ? 'on' : 'off'">{{ b.enabled ? "已启用" : "未启用" }}</span>
                    <span class="flex-sp"></span>
                    <a-button size="mini" type="text" @click="goChannelSettings">渠道设置</a-button>
                    <a-button size="mini" type="text" @click="toggleEdit(b)">绑定</a-button>
                  </div>
                  <div class="res-meta">
                    <span class="res-bind" :class="{ none: !b.employeeId && !b.positionId }">{{ bindLabel(b) }}</span>
                  </div>
                  <BindEditor
                    v-if="editingKey === b.type + '\u0000' + b.id"
                    resource-type="channel"
                    :resource-id="b.id"
                    :employee-id="b.employeeId || ''"
                    :position-id="b.positionId || ''"
                    :assign-rule="b.assignRule || ''"
                    compact
                    @saved="onSaved"
                  />
                </div>
              </div>
              <div v-else class="res-empty empty-block">
                <div class="empty-ico-chip"><IconSend /></div>
                <div class="empty-block-title">{{ filterEmp || filterPos || curDeptId ? "当前筛选下无渠道" : "暂无消息渠道" }}</div>
                <div class="empty-block-hint">可在设置 → 消息渠道中添加</div>
              </div>
            </template>

            <!-- Tab2 定时任务 -->
            <template v-else-if="activeTab === 'cron'">
              <div v-if="cronRows.length" class="res-list">
                <div v-for="b in cronRows" :key="b.id" class="res-card">
                  <div class="res-row1">
                    <span class="res-name" :title="b.name">{{ b.name }}</span>
                    <span class="res-badge" :class="b.enabled ? 'on' : 'off'">{{ b.enabled ? "已启用" : "已停用" }}</span>
                    <span class="flex-sp"></span>
                    <a-button size="mini" type="text" @click="store.activeMain = 'cron'">任务管理</a-button>
                    <a-button size="mini" type="text" @click="toggleEdit(b)">绑定</a-button>
                  </div>
                  <div class="res-meta">
                    <span class="res-bind" :class="{ none: !b.employeeId && !b.positionId }">{{ bindLabel(b) }}</span>
                  </div>
                  <BindEditor
                    v-if="editingKey === b.type + '\u0000' + b.id"
                    resource-type="cron"
                    :resource-id="b.id"
                    :employee-id="b.employeeId || ''"
                    :position-id="b.positionId || ''"
                    :assign-rule="b.assignRule || ''"
                    compact
                    @saved="onSaved"
                  />
                </div>
              </div>
              <div v-else class="res-empty empty-block">
                <div class="empty-ico-chip"><IconClockCircle /></div>
                <div class="empty-block-title">{{ filterEmp || filterPos || curDeptId ? "当前筛选下无定时任务" : "暂无定时任务" }}</div>
                <div class="empty-block-hint">可在「定时任务」面板创建</div>
              </div>
            </template>

            <!-- Tab3 会话（可直接绑岗位/员工；运行中禁改） -->
            <template v-else-if="activeTab === 'session'">
              <div v-if="sessionRows.length" class="res-list">
                <div v-for="b in sessionRows" :key="b.id" class="res-card">
                  <div class="res-row1">
                    <span class="res-name" :title="b.name">{{ b.name || "未命名会话" }}</span>
                    <span v-if="b.running" class="res-badge run">运行中</span>
                    <span class="flex-sp"></span>
                    <a-button size="mini" type="text" @click="goSession(b.id)">打开</a-button>
                    <a-button size="mini" type="text" :disabled="b.running" @click="toggleEdit(b)">绑定</a-button>
                  </div>
                  <div class="res-meta">
                    <span class="res-bind" :class="{ none: !b.employeeId && !b.positionId }">{{ bindLabel(b) }}</span>
                    <span v-if="b.running" class="res-bind dim">（运行中不可改）</span>
                  </div>
                  <BindEditor
                    v-if="editingKey === b.type + '\u0000' + b.id"
                    resource-type="session"
                    :resource-id="b.id"
                    :employee-id="b.employeeId || ''"
                    :position-id="b.positionId || ''"
                    :assign-rule="b.assignRule || ''"
                    compact
                    @saved="onSaved"
                  />
                </div>
              </div>
              <div v-else class="res-empty empty-block">
                <div class="empty-ico-chip"><IconMessage /></div>
                <div class="empty-block-title">{{ filterEmp || filterPos || curDeptId ? "当前筛选下无会话" : "暂无会话" }}</div>
                <div class="empty-block-hint">会话可在左侧聊天创建；绑定岗位后首条消息从岗位池分配员工</div>
              </div>
            </template>

            <!-- 岗位权限（用户→岗位→角色→权限主路径；部门导航联动） -->
            <template v-else-if="activeTab === 'perms'">
              <div v-if="permRows.length" class="res-list">
                <div v-for="p in permRows" :key="p.id" class="res-card">
                  <div class="res-row1">
                    <span class="res-name" :title="p.description">{{ p.name }}</span>
                    <span v-if="p.level" class="res-pos-level">{{ p.level }}</span>
                    <span v-if="p.scopeAll" class="res-badge pos" :title="scopeTitle(p)">全公司</span>
                    <span v-else-if="(p.scopeDeptIds || []).length" class="res-badge pos" :title="scopeTitle(p)">范围 {{ p.scopeDeptIds.length }} 部门</span>
                    <span class="flex-sp"></span>
                    <a-button size="mini" type="primary" @click="openPosRole(p)">配置角色</a-button>
                  </div>
                  <!-- 角色链：岗位映射的角色（运行期与员工直绑角色收紧式合并） -->
                  <div class="res-role-chain">
                    <span class="res-chain-label">角色</span>
                    <template v-if="(p.roleIds || []).length">
                      <span v-for="rid in p.roleIds" :key="rid" class="res-role-chip" :title="roleSummary(roleName[rid])">
                        {{ (roleName[rid] || {}).name || rid }}
                        <span class="res-role-sum">{{ roleSummary(roleName[rid]) || '未配置权限' }}</span>
                      </span>
                    </template>
                    <span v-else class="res-bind none">未映射角色（员工仅直绑角色或无权限包）</span>
                  </div>
                  <!-- 岗位下员工（有效角色 = 直绑 ∪ 岗位映射；成员卡可直改直绑角色兑底） -->
                  <div class="res-role-chain">
                    <span class="res-chain-label">成员</span>
                    <template v-if="(posEmployees[p.id] || []).length">
                      <span
                        v-for="e in posEmployees[p.id]"
                        :key="e.id"
                        class="res-emp-chip"
                        :title="'点击编辑直绑角色（兑底临时授权）；有效角色 = 直绑 ∪ 岗位映射，运行期收紧式合并'"
                        @click="openEmpRole(e)"
                      >
                        {{ (e.avatar || "🤖") + " " + e.name }}
                        <span v-if="(e.roleIds || []).length" class="res-role-sum">直绑 {{ e.roleIds.length }}</span>
                        <span v-else class="res-role-sum">+兑底</span>
                      </span>
                    </template>
                    <span v-else class="res-bind none">暂无在职员工</span>
                  </div>
                  <!-- 关联资源（行内改绑 + 有效权限链：资源→岗位→角色→权限包） -->
                  <div v-if="posResources(p.id).length" class="res-pos-rows">
                    <div v-for="b in posResources(p.id)" :key="b.type + '\u0000' + b.id" class="res-pos-row">
                      <span class="res-badge mini" :class="b.enabled ? 'on' : 'off'">{{ typeBadge[b.type] || b.type }}</span>
                      <span class="res-pos-name" :title="b.name">{{ b.name }}</span>
                      <span class="res-bind" :class="{ none: !b.employeeId && !b.positionId }">{{ bindLabel(b) }}</span>
                      <span class="flex-sp"></span>
                      <a-button size="mini" type="text" :disabled="b.type === 'session' && b.running" @click="toggleEdit(b)">改绑定</a-button>
                      <BindEditor
                        v-if="editingKey === b.type + '\u0000' + b.id"
                        :resource-type="b.type"
                        :resource-id="b.id"
                        :employee-id="b.employeeId || ''"
                        :position-id="b.positionId || ''"
                        :assign-rule="b.assignRule || ''"
                        compact
                        @saved="onSaved"
                      />
                    </div>
                  </div>
                  <!-- 有效权限链行（该岗位全部角色包合并摘要） -->
                  <div v-if="p.roleIds && p.roleIds.length" class="res-perm-line">
                    <span class="res-chain-label">权限</span>
                    <span class="res-perm-sum">{{ permPackText(p.roleIds) }}</span>
                  </div>
                </div>
              </div>
              <div v-else class="res-empty empty-block">
                <div class="empty-ico-chip"><IconSafe /></div>
                <div class="empty-block-title">{{ curDeptId ? "当前部门范围内无岗位" : "暂无岗位" }}</div>
                <div class="empty-block-hint">在「数字员工 → 组织架构」中创建岗位；此处维护岗位↔角色映射与关联资源</div>
              </div>

              <!-- 角色视图：权限包结构化展示 + 引用反查（删除保护同源数据） -->
              <div class="res-roles-head">
                <span class="res-roles-title">角色字典</span>
                <span class="panel-sub">权限包快照 · 引用反查（岗位映射 = 主路径 / 员工直绑 = 兑底）</span>
                <span class="flex-sp"></span>
                <a-button size="mini" type="text" @click="goEmployeeOrg">管理角色</a-button>
              </div>
              <div v-if="roles.length" class="res-list">
                <div v-for="r in roles" :key="r.id" class="res-card res-role-card">
                  <div class="res-row1">
                    <span class="res-name">{{ r.name }}</span>
                    <span v-if="r.code" class="res-pos-level">{{ r.code }}</span>
                    <span class="res-badge" :class="r.status === 'disabled' ? 'off' : 'on'">{{ r.status === 'disabled' ? '已停用' : '启用' }}</span>
                    <span class="flex-sp"></span>
                  </div>
                  <div class="res-perm-line">
                    <span class="res-chain-label">权限包</span>
                    <span class="res-perm-chips">
                      <span v-for="tag in rolePermTags(r)" :key="tag" class="res-perm-tag">{{ tag }}</span>
                      <span v-if="!rolePermTags(r).length" class="res-bind none">未配置（套用后无额外限制）</span>
                    </span>
                  </div>
                  <div class="res-perm-line">
                    <span class="res-chain-label">引用</span>
                    <span class="res-bind">
                      <template v-if="(roleRefs[r.id] || {}).positions || (roleRefs[r.id] || {}).employees">
                        {{ (roleRefs[r.id].positions || []).length }} 岗位映射{{ (roleRefs[r.id].employees || []).length ? ' · ' + roleRefs[r.id].employees.length + ' 员工直绑' : '' }}
                        <span v-if="(roleRefs[r.id].positions || []).length" class="res-ref-names">（{{ roleRefs[r.id].positions.join("、") }}）</span>
                      </template>
                      <template v-else>未被引用（可删除）</template>
                    </span>
                  </div>
                </div>
              </div>
              <div v-else class="res-empty empty-block">
                <div class="empty-block-title">暂无角色</div>
                <div class="empty-block-hint">在「数字员工 → 组织架构」中创建角色并配置权限包（MCP/技能/只读/目录）</div>
              </div>
            </template>

            <!-- 岗位视图 -->
            <template v-else>
              <div v-if="positionRows.length" class="res-list">
                <div v-for="row in positionRows" :key="row.position.id" class="res-card">
                  <div class="res-row1">
                    <span class="res-name">{{ row.position.name }}</span>
                    <span v-if="row.position.level" class="res-pos-level">{{ row.position.level }}</span>
                    <span v-if="row.position.scopeAll" class="res-badge pos">全公司</span>
                    <span v-else-if="(row.position.scopeDeptIds || []).length" class="res-badge pos">范围 {{ row.position.scopeDeptIds.length }} 部门</span>
                    <span class="res-badge pos">{{ row.rows.length }} 项资源</span>
                    <span class="flex-sp"></span>
                    <a-button size="mini" type="text" @click="goEmployeeOrg">组织管理</a-button>
                  </div>
                  <div class="res-pos-rows">
                    <div v-for="b in row.rows" :key="b.type + '\u0000' + b.id" class="res-pos-row">
                      <span class="res-badge mini" :class="b.enabled ? 'on' : 'off'">{{ typeBadge[b.type] || b.type }}</span>
                      <span class="res-pos-name" :title="b.name">{{ b.name }}</span>
                      <span class="res-bind" :class="{ none: !b.employeeId && !b.positionId }">{{ bindLabel(b) }}</span>
                      <span class="flex-sp"></span>
                      <a-button size="mini" type="text" :disabled="b.type === 'session' && b.running" @click="toggleEdit(b)">改员工</a-button>
                      <BindEditor
                        v-if="editingKey === b.type + '\u0000' + b.id"
                        :resource-type="b.type"
                        :resource-id="b.id"
                        :employee-id="b.employeeId || ''"
                        :position-id="b.positionId || ''"
                        :assign-rule="b.assignRule || ''"
                        compact
                        @saved="onSaved"
                      />
                    </div>
                  </div>
                  <!-- 有效权限链（资源→岗位→角色→权限包；收紧式合并后任一禁用即生效） -->
                  <div class="res-perm-line">
                    <span class="res-chain-label">权限</span>
                    <span class="res-perm-chips">
                      <span
                        v-for="r in effRoleChips(row.rows[0])"
                        :key="r.id"
                        class="res-perm-tag"
                        :title="r.name + '：' + (roleSummary(r) || '未配置权限')"
                      >{{ r.name }}</span>
                      <span v-if="!effRoleChips(row.rows[0]).length" class="res-bind none">无角色包（裸跑全权）</span>
                    </span>
                  </div>
                </div>
              </div>
              <div v-else class="res-empty empty-block">
                <div class="empty-ico-chip"><IconUserGroup /></div>
                <div class="empty-block-title">{{ filterEmp || filterPos || curDeptId ? "当前筛选下无岗位资源" : "暂无岗位绑定资源" }}</div>
                <div class="empty-block-hint">在渠道 / 定时任务 / 会话 Tab 设置绑定后在此聚合展示</div>
              </div>
            </template>
          </div>
        </a-spin>
      </div>
    </div>

    <!-- 岗位角色映射弹窗（业界 RBAC：岗位→角色多对多；保存经后端存在性校验） -->
    <a-modal
      v-model:visible="posRoleVisible"
      :title="'岗位角色映射 · ' + ((posRoleForm && posRoleForm.position || {}).name || '')"
      :width="560"
      unmount-on-close
      @before-ok="savePosRole"
    >
      <template v-if="posRoleForm">
        <div class="set-section">
          <label class="set-label">映射角色（可多选；运行期与员工直绑角色收紧式合并，任一角色禁用即生效）</label>
          <a-select
            v-model="posRoleForm.roleIds"
            size="small"
            multiple
            allow-clear
            placeholder="选择角色（空 = 该岗位无角色权限包）"
            :max-tag-count="4"
          >
            <a-option v-for="r in roles" :key="r.id" :value="r.id" :disabled="r.status === 'disabled'">
              {{ r.name }}{{ r.status === 'disabled' ? '（停用）' : '' }}
            </a-option>
          </a-select>
        </div>
        <div class="set-hint">权限链：用户 → 岗位（此处）→ 角色 → 权限包（MCP/技能/只读/目录）；日常仅需维护岗位↔角色，员工直绑角色仅兑底临时授权。角色字典在「数字员工 → 组织架构」维护。</div>
      </template>
    </a-modal>

    <!-- 成员直绑角色编辑弹窗（用户设计图：用户也可直接绑角色，兑底临时授权） -->
    <a-modal
      v-model:visible="empRoleVisible"
      :title="'直绑角色 · ' + ((empRoleForm && empRoleForm.employee || {}).name || '')"
      :width="520"
      unmount-on-close
      @before-ok="saveEmpRole"
    >
      <template v-if="empRoleForm">
        <div class="set-section">
          <label class="set-label">直绑角色（可多选；与岗位映射角色收紧式合并，任一角色禁用即生效）</label>
          <a-select
            v-model="empRoleForm.roleIds"
            size="small"
            multiple
            allow-clear
            placeholder="选择角色（空 = 仅岗位映射角色）"
            :max-tag-count="4"
          >
            <a-option v-for="r in roles" :key="r.id" :value="r.id" :disabled="r.status === 'disabled'">
              {{ r.name }}{{ r.status === 'disabled' ? '（停用）' : '' }}
            </a-option>
          </a-select>
        </div>
        <div class="set-hint">兑底授权语义：日常权限随岗位自动继承（岗位↔角色主路径），此处仅应对临时授权（如临时开放某个 MCP 工具）。角色字典在「数字员工 → 组织架构」维护。</div>
      </template>
    </a-modal>
  </div>
</template>

<style scoped>
.res-panel {
  height: 100%;
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px 20px;
  overflow: hidden;
}
/* 内嵌于「数字员工」页签时去掉外层留白，由宿主 .emp-body 控制布局 */
.res-panel.res-embedded { padding: 0; }
.res-head { gap: 8px; flex-shrink: 0; }
.res-title { font-size: var(--fs-title); color: var(--text-1); font-weight: 500; }
.flex-sp { flex: 1; }

/* 主体：左组织导航 + 右资源区 */
.res-main {
  flex: 1;
  display: flex;
  gap: 12px;
  min-height: 0;
}
.res-nav {
  width: clamp(160px, 22%, 210px);
  flex-shrink: 0;
  min-width: 0;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  padding: 10px;
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-height: 0;
}
.res-nav-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  color: var(--text-1);
  font-size: var(--fs-body);
  font-weight: 500;
  padding: 0 4px;
}
.res-nav-list {
  flex: 1;
  overflow-y: auto;
  min-height: 0;
  font-size: var(--fs-body);
}
.res-nav-row {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 4px 6px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  color: var(--text-2);
  transition: var(--transition);
}
	.res-nav-row:hover { background: var(--bg-hover); color: var(--text-1); }
	/* 选中态：滑动亮条（对齐 EmployeePanel 部门导航 / Sidebar 会话卡同语言） */
	.res-nav-row.on {
	  background: var(--bg-active);
	  color: var(--primary);
	  font-weight: 500;
	  box-shadow: inset 2px 0 0 var(--primary);
	}
	.res-nav-row.on .res-nav-count { color: var(--primary); background: var(--primary-tint); }
.res-nav-caret { width: 12px; color: var(--text-3); font-size: 10px; flex-shrink: 0; }
.res-nav-name {
  flex: 1;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.res-nav-count {
  font-size: var(--fs-micro);
  color: var(--text-3);
  background: var(--bg-inset);
  padding: 0 6px;
  border-radius: 8px;
}
.res-nav-empty {
  color: var(--text-3);
  font-size: var(--fs-caption);
  padding: 12px 6px;
  text-align: center;
  line-height: 1.7;
}
.res-nav-hint { font-size: var(--fs-micro); color: var(--text-3); }

.res-right {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 10px;
  min-height: 0;
}
.res-right > .res-tabs { flex-shrink: 0; }

/* Tab 分段（对齐 EmployeePanel .emp-tabs 视觉） */
.res-tabs {
  display: flex;
  gap: 4px;
  padding: 3px;
  background: var(--bg-inset);
  border-radius: var(--radius-sm);
  width: fit-content;
  flex-shrink: 0;
}
.res-tabs button {
  border: none;
  background: transparent;
  color: var(--text-2);
  font-size: var(--fs-caption);
  font-family: var(--font-ui);
  padding: 5px 14px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  gap: 5px;
  transition: var(--transition);
}
.res-tabs button:hover { color: var(--text-1); background: var(--bg-hover); }
.res-tabs button.active { color: var(--on-primary); background: var(--primary-strong); }

.res-list {
  height: 100%;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding-right: 2px;
}
.res-card {
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  padding: 12px 14px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  transition: border-color var(--transition);
}
.res-card:hover { border-color: var(--border); }
.res-row1 { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.res-name {
  font-size: var(--fs-body);
  color: var(--text-1);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
  max-width: 60%;
}
.res-badge {
  font-size: var(--fs-micro);
  padding: 0 8px;
  border-radius: 999px;
  flex-shrink: 0;
  line-height: 18px;
}
.res-badge.on { background: var(--primary-tint); color: var(--primary); }
.res-badge.off { background: var(--bg-hover); color: var(--text-3); }
.res-badge.pos { background: var(--success-tint); color: var(--success); }
.res-badge.run { background: var(--warning-tint, rgba(255, 153, 0, 0.15)); color: var(--warning, #ff9a2e); }
.res-badge.mini { padding: 0 6px; }
.res-pos-level {
  font-size: var(--fs-micro);
  color: var(--text-3);
  background: var(--bg-inset);
  padding: 1px 6px;
  border-radius: 4px;
}
.res-meta { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.res-bind.dim { color: var(--text-3); font-style: italic; }

/* 岗位视图资源行 */
.res-pos-rows {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.res-pos-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 8px;
  background: var(--bg-inset);
  border-radius: var(--radius-sm);
  flex-wrap: wrap;
}
.res-pos-name {
  font-size: var(--fs-caption);
  color: var(--text-1);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 30%;
}
.res-bind {
  font-size: var(--fs-micro);
  color: var(--text-2);
}
.res-bind.none { color: var(--text-3); font-style: italic; }

.res-empty.empty-block {
  padding: 32px 16px;
}
.res-empty.empty-block .empty-ico-chip { margin: 0 auto 10px; }

/* 岗位权限链（角色/成员 chip 行） */
.res-role-chain {
  display: flex;
  align-items: flex-start;
  gap: 6px;
  flex-wrap: wrap;
}
.res-chain-label {
  font-size: var(--fs-micro);
  color: var(--text-3);
  width: 30px;
  flex-shrink: 0;
  line-height: 22px;
}
.res-role-chip,
.res-emp-chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: var(--fs-caption);
  color: var(--text-1);
  background: var(--bg-inset);
  border: 1px solid var(--border-weak);
  padding: 2px 8px;
  border-radius: 999px;
  line-height: 18px;
}
.res-role-sum {
  font-size: var(--fs-micro);
  color: var(--text-3);
}
.res-emp-chip { cursor: pointer; transition: var(--transition); }
.res-emp-chip:hover { border-color: var(--primary); color: var(--primary); }

/* 角色视图（权限 Tab 下半区）与权限链行 */
.res-roles-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 2px 0;
}
.res-roles-title {
  font-size: var(--fs-body);
  color: var(--text-1);
  font-weight: 500;
}
.res-role-card { background: var(--bg-inset); }
.res-perm-line {
  display: flex;
  align-items: flex-start;
  gap: 6px;
}
.res-perm-sum {
  font-size: var(--fs-micro);
  color: var(--text-2);
  line-height: 1.7;
}
.res-perm-chips {
  display: inline-flex;
  flex-wrap: wrap;
  gap: 4px;
}
.res-perm-tag {
  font-size: var(--fs-micro);
  color: var(--text-2);
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  padding: 1px 8px;
  border-radius: 999px;
  line-height: 17px;
}
.res-ref-names { color: var(--text-3); }
</style>
