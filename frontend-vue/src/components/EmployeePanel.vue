<script setup>
// 数字员工面板（EHR）：员工档案 / 组织架构 / 群聊协作 三视图。
// 桥接：employee_panel.go（ListEmployees/SaveEmployee/GetOrg...）与 remote_group.go（Group*）。
// 事件：kaelvoris:employees（员工变更）、kaelvoris:groups（群列表）、kaelvoris:cron（影响岗位关联资源计数）。
import { computed, h, onBeforeUnmount, ref } from "vue";
import { Message, Modal } from "@arco-design/web-vue";
import {
  IconPlus, IconEdit, IconDelete, IconCopy, IconUser,
  IconRefresh, IconRelation, IconUserGroup, IconStorage, IconSafe, IconThunderbolt,
} from "@arco-design/web-vue/es/icon";
import { bridgeCall, onEvent } from "../bridge/wails.js";
import { store } from "../stores/app.js";
import GroupChatPanel from "./GroupChatPanel.vue";
import ResourcePanel from "./ResourcePanel.vue";

/* ── 运行状态字典（引擎态：enabled/disabled/archived）────────────── */
const statusMap = {
  enabled: { label: "启用", color: "green" },
  disabled: { label: "停用", color: "orange" },
  archived: { label: "归档", color: "gray" },
};

/* ── 人事状态字典（EHR 态，优先于引擎态展示）────────────────────── */
// 人事状态字典：与后端枚举对齐（employee.go EmpActive/EmpProbation/EmpSuspended/
// EmpResigned）。后端 Validate 仅认这四值，"leave"/"retired" 保存即报错，删除。
const empStatusMap = {
  active: { label: "在职", color: "green" },
  probation: { label: "试用期", color: "orange" },
  suspended: { label: "停职", color: "orange" },
  resigned: { label: "离职", color: "gray" },
};

// 卡片状态徽标（轻量文字点样式；人事状态优先于引擎状态）
function empStatusLabel(e) {
  const m = empStatusMap[e.empStatus] || statusMap[e.status] || {};
  return m.label || (e.status === "draft" ? "草稿" : e.status) || "";
}
const empStatusCls = (e) => {
  const st = e.empStatus || e.status;
  if (st === "active" || st === "enabled") return "ok";
  if (st === "probation" || st === "disabled") return "warn";
  if (st === "suspended") return "warn";
  return "mute";
};
const empStatusOptions = Object.entries(empStatusMap).map(([value, m]) => ({ label: m.label, value }));

const genderOptions = [
  { label: "保密", value: "" },
  { label: "男", value: "male" },
  { label: "女", value: "female" },
];

// MCP 权限模式：与后端枚举对齐（employee.go MCPUnset/MCPWhitelist/MCPBlacklist）。
// 后端空串 = 未配置（全部可用）；此前误传 "unset" 会被后端 Validate 拒绝（保存失败）。
const roleMCPOptions = [
  { label: "未设置", value: "" },
  { label: "白名单", value: "whitelist" },
  { label: "黑名单", value: "blacklist" },
];

const employees = ref([]);
const loading = ref(false);
const activeTab = ref("emp");
const editVisible = ref(false);
const form = ref(null);
const empSearch = ref("");

/* ── 部门过滤："" = 全部，"none" = 未分配，其余为部门 id ────────── */
const curDeptId = ref("");
const deptTree = ref([]);
const positions = ref([]);
const roles = ref([]);

const deptEmployees = computed(() => {
  if (curDeptId.value === "") return employees.value;
  if (curDeptId.value === "none") return employees.value.filter((e) => !e.deptId);
  const ids = [];
  for (const d of deptTree.value) if (d.id === curDeptId.value) collectSubDepts(d, ids);
  return employees.value.filter((e) => e.deptId && ids.includes(e.deptId));
});
function collectSubDepts(node, out) {
  out.push(node.id);
  for (const c of node.children || []) collectSubDepts(c, out);
}
function deptTotal(id) {
  const ids = [];
  for (const d of deptTree.value) if (d.id === id) collectSubDepts(d, ids);
  return employees.value.filter((e) => e.deptId && ids.includes(e.deptId)).length;
}
/* ── 部门过滤："" = 全部，"none" = 未分配，其余为部门 id ────────── */

// 顶部统计条数据
const activeCount = computed(() => employees.value.filter((e) => e.status === "enabled").length);

// 搜索过滤：名称/职责/工号/岗位/部门
const searchedEmployees = computed(() => {
  const q = empSearch.value.trim().toLowerCase();
  if (!q) return deptEmployees.value;
  return deptEmployees.value.filter((e) =>
    [e.name, e.description, e.empNo, e.positionName, e.deptName]
      .some((s) => s && String(s).toLowerCase().includes(q))
  );
});

// 组织统计（组织架构 Tab 头部）
const orgStats = computed(() => {
  const countDepts = (list) => list.reduce((n, d) => n + 1 + countDepts(d.children || []), 0);
  return {
    depts: countDepts(deptTree.value),
    positions: positions.value.length,
    roles: roles.value.length,
    bound: employees.value.filter((e) => e.positionId).length,
  };
});

/* ── 加载与 CRUD ──────────────────────────────────────────────── */
async function load() {
  loading.value = true;
  try {
    employees.value = (await bridgeCall("ListEmployees")) || [];
  } finally {
    loading.value = false;
  }
}

function emptyForm() {
  return {
    name: "", avatar: "", description: "", rolePrompt: "",
    model: "", evolutionStrength: 50,
    status: "enabled", readOnly: false,
    workspaceRoot: "", workspacePath: "",
    empNo: "", gender: "", birthday: "", phone: "", email: "",
    hireDate: "", deptId: "", positionId: "", roleId: "", roleIds: [], managerId: "",
    empStatus: "active",
    mcpDisabled: false, mcpServers: [], mcpMode: "", mcpTools: [],
    skillsDisabled: false, skillsDeny: [], dirs: [],
  };
}
function openNew() {
  form.value = emptyForm();
  editVisible.value = true;
  ensureOrgForEdit();
}
async function openEdit(id) {
  try {
    const v = await bridgeCall("GetEmployee", id);
    if (v) {
      v.roleIds = (v.roleIds && v.roleIds.length) ? v.roleIds : (v.roleId ? [v.roleId] : []);
      form.value = v;
      editVisible.value = true;
      ensureOrgForEdit();
    }
  } catch (e) {
    Message.error("读取员工失败：" + String(e));
  }
}
async function save() {
  const v = form.value;
  if (!v || !v.name || !v.name.trim()) {
    Message.warning("请填写员工名称");
    return false;
  }
  try {
    await bridgeCall("SaveEmployee", JSON.parse(JSON.stringify(v)));
    Message.success("已保存");
    await load();
    return true;
  } catch (e) {
    Message.error("保存失败：" + String(e));
    return false;
  }
}
async function toggleStatus(e) {
  try {
    await bridgeCall("SetEmployeeStatus", e.id, e.status === "enabled" ? "disabled" : "enabled");
    await load();
  } catch (err) {
    Message.error("操作失败：" + String(err));
  }
}
async function toggleEmpStatus(e) {
  try {
    await bridgeCall("SetEmpStatus", e.id, e.empStatus === "active" ? "resigned" : "active");
    await load();
  } catch (err) {
    Message.error("操作失败：" + String(err));
  }
}
function removeEmp(ev, e) {
  ev.stopPropagation();
  Modal.confirm({
    title: `删除员工「${e.name}」？`,
    content: "工作空间文件保留；如需彻底清理请到设置目录手动删除。",
    okButtonProps: { status: "danger" },
    onOk: async () => {
      try {
        await bridgeCall("DeleteEmployee", e.id);
        Message.success("已删除");
        await load();
      } catch (err) {
        Message.error("删除失败：" + String(err));
      }
    },
  });
}
async function duplicateEmp(ev, e) {
  ev.stopPropagation();
  try {
    // 走后端 DuplicateEmployee（§2.3：配置复制 + 工作空间文件复制 + 新员工置草稿态）。
    // 此前手工 GetEmployee→SaveEmployee 绕过后端，丢工作空间复制且副本保持 enabled。
    await bridgeCall("DuplicateEmployee", e.id);
    Message.success("已复制（副本为草稿态）");
    await load();
  } catch (err) {
    Message.error("复制失败：" + String(err));
  }
}
async function bindToSession(e) {
  try {
    const { switchEmployee } = await import("../stores/app.js");
    await switchEmployee(e.id);
    Message.success("已绑定到当前会话");
  } catch (err) {
    Message.error("绑定失败：" + String(err));
  }
}

/* ── 组织架构（部门/岗位/角色）────────────────────────────────── */
let orgLoadedForEdit = false;
async function ensureOrgForEdit() {
  if (orgLoadedForEdit) return;
  orgLoadedForEdit = true;
  await loadOrg();
}
const orgLoading = ref(false);
async function loadOrg() {
  orgLoading.value = true;
  try {
    const [tree, org] = await Promise.all([bridgeCall("OrgTree"), bridgeCall("GetOrg")]);
    // OrgTree 后端返回数组（[]*DeptTree），与 ResourcePanel.loadTree 同口径。
    deptTree.value = Array.isArray(tree) ? tree : [];
    positions.value = (org && org.positions) || [];
    roles.value = (org && org.roles) || [];
  } catch {
    deptTree.value = [];
    positions.value = [];
    roles.value = [];
  } finally {
    orgLoading.value = false;
  }
}
function onTabChange(tab) {
  if (tab === "org") loadOrg();
  if (tab === "emp") load();
}

// 部门扁平选项（下拉用，带层级路径）
const deptFlatOptions = computed(() => {
  const out = [];
  const walk = (list, prefix) => {
    for (const d of list || []) {
      out.push({ id: d.id, path: prefix ? prefix + " / " + d.name : d.name });
      walk(d.children, prefix ? prefix + " / " + d.name : d.name);
    }
  };
  walk(deptTree.value, "");
  return out;
});
// 员工下拉选项（直属上级/部门负责人选择用）
const empOptions = computed(() => employees.value.map((e) => ({ id: e.id, name: e.name })));

/* ── 组织架构 Tab 选中态（主从布局）：orgSelId "" = 全部部门概览 ── */
const orgSelId = ref("");
const orgSub = ref("pos"); // 右侧子页签：pos 岗位 | role 角色
// 部门树深度查找（orgSelDept / 子树集合用）
function findDeptNode(list, id) {
  for (const d of list || []) {
    if (d.id === id) return d;
    const hit = findDeptNode(d.children, id);
    if (hit) return hit;
  }
  return null;
}
const orgSelDept = computed(() => (orgSelId.value ? findDeptNode(deptTree.value, orgSelId.value) : null));
// 当前部门（含子树）范围内岗位；「全部」= 所有岗位
const orgPositions = computed(() => {
  if (!orgSelId.value) return positions.value;
  const n = findDeptNode(deptTree.value, orgSelId.value);
  if (!n) return [];
  const ids = [];
  collectSubDepts(n, ids);
  return positions.value.filter((p) => p.deptId && ids.includes(p.deptId));
});
// 部门详情头统计：直属员工 / 子部门数
function deptDirectCount(id) {
  return employees.value.filter((e) => e.deptId === id).length;
}
function deptChildCount(id) {
  const n = findDeptNode(deptTree.value, id);
  return n && n.children ? n.children.length : 0;
}
// 员工名反查（部门负责人展示）
function empName(id) {
  if (!id) return "";
  const e = employees.value.find((x) => x.id === id);
  return e ? e.name : "";
}
// 角色是否完全未配置权限限制（占位提示用）
function roleNoRestrict(r) {
  return r && !r.readOnly && !r.mcpDisabled && !r.mcpMode && !r.skillsDisabled && !(r.skillsDeny && r.skillsDeny.length);
}

/* ── 部门 CRUD ────────────────────────────────────────────────── */
const deptVisible = ref(false);
const deptForm = ref(null);
function openDeptNew(parentId) {
  deptForm.value = { id: "", parentId: parentId || "", name: "", code: "", sortOrder: 0, status: "active" };
  deptVisible.value = true;
}
function openDeptEdit(d) {
  deptForm.value = JSON.parse(JSON.stringify(d));
  deptVisible.value = true;
}
async function saveDept() {
  const v = deptForm.value;
  if (!v.name || !v.name.trim()) {
    Message.warning("请填写部门名称");
    return false;
  }
  try {
    await bridgeCall("SaveDepartment", JSON.parse(JSON.stringify(v)));
    Message.success("已保存");
    await loadOrg();
    await load();
    return true;
  } catch (e) {
    Message.error("保存失败：" + String(e));
    return false;
  }
}
	function removeDept(d) {
	  Modal.confirm({
	    title: `删除部门「${d.name}」？`,
	    content: "有子部门、岗位或员工归属时将拒绝删除（引用完整性校验，不级联清理）。",
	    okButtonProps: { status: "danger" },
    onOk: async () => {
      try {
await bridgeCall("DeleteDepartment", d.id);
if (curDeptId.value === d.id) curDeptId.value = "";
if (orgSelId.value === d.id) orgSelId.value = "";
        Message.success("已删除");
        await loadOrg();
        await load();
      } catch (err) {
        Message.error("删除失败：" + String(err));
      }
    },
  });
}

/* ── 部门树右键菜单（上下文操作全量入口：新建/编辑/停用启用/删除）── */
// node = null 表示「全部部门」根行（仅含新建顶级部门）。
const ctxMenu = ref(null); // { x, y, items }；items 项：{ label, danger?, fn } | { sep: true }
const CTX_MENU_W = 168;    // 菜单估宽（.ctx-menu min-width + padding）
// 通用右键菜单入口：组织架构 Tab 全量增删改查收敛于此（部门节点/根行/树空白/部门卡/
// 概览卡/岗位卡/角色卡/列表空白区均构建 items 后调用；视口边界内收防溢出首帧）
function openCtxMenu(pos, items) {
  const mh = items.reduce((n, it) => n + (it.sep ? 9 : 32), 0) + 8;
  ctxMenu.value = {
    x: Math.max(8, Math.min(pos.x, window.innerWidth - CTX_MENU_W - 8)),
    y: Math.max(8, Math.min(pos.y, window.innerHeight - mh - 8)),
    items,
  };
}
// 部门菜单（node = null 表示「全部部门」根行/树空白/概览卡，仅含新建顶级部门）
function deptMenuItems(node) {
  if (!node) return [{ label: "新建顶级部门", fn: () => openDeptNew("") }];
  return [
    { label: "新建子部门", fn: () => openDeptNew(node.id) },
    { sep: true },
    { label: "编辑部门", fn: () => openDeptEdit(node) },
    { label: node.status === "disabled" ? "启用部门" : "停用部门", fn: () => toggleDeptStatus(node) },
    { sep: true },
    { label: "删除部门", danger: true, fn: () => removeDept(node) },
  ];
}
const openDeptCtxMenu = (pos, node) => openCtxMenu(pos, deptMenuItems(node));
// 岗位卡菜单（关联资源 = 查询入口，跳资源页签岗位视图）
const openPosCtxMenu = (pos, p) => openCtxMenu(pos, [
  { label: "编辑岗位", fn: () => openPosEdit(p) },
  { label: "关联资源", fn: () => openResourcePositions() },
  { sep: true },
  { label: "删除岗位", danger: true, fn: () => removePos(p) },
]);
// 角色卡菜单
const openRoleCtxMenu = (pos, r) => openCtxMenu(pos, [
  { label: "编辑角色", fn: () => openRoleEdit(r) },
  { sep: true },
  { label: "删除角色", danger: true, fn: () => removeRole(r) },
]);
// 列表空白区菜单（新建入口，跟随当前子页签：pos → 新建岗位 / role → 新建角色；
// 模板侧用 @contextmenu.prevent.stop 绑定阻断 document 守卫，仅空白区/空态触发——
// 卡片右键自身 stopPropagation 后不会穿透到此处）
const onListCtxMenu = (e) => {
  const at = { x: e.clientX, y: e.clientY };
  if (orgSub.value === "pos") openCtxMenu(at, [{ label: "新建岗位", fn: () => openPosNew() }]);
  else openCtxMenu(at, [{ label: "新建角色", fn: () => openRoleNew() }]);
};
// 停用/启用：走 SaveDepartment 全量更新（后端无独立状态接口；
// DeptTree 节点剥掉 children 后透传，status 翻转）。
async function toggleDeptStatus(d) {
  try {
    const { children, ...dept } = d;
    const next = d.status === "disabled" ? "active" : "disabled";
    await bridgeCall("SaveDepartment", JSON.parse(JSON.stringify({ ...dept, status: next })));
    Message.success(next === "disabled" ? "已停用" : "已启用");
    await loadOrg();
    await load();
  } catch (err) {
    Message.error("操作失败：" + String(err));
  }
}
function onCtxItemClick(it) {
  const fn = it.fn;
  ctxMenu.value = null;
  if (fn) fn();
}
function closeCtxMenu() {
  ctxMenu.value = null;
}
// 菜单关闭守卫：任意左键 / 其他区域右键 / Escape / 滚轮（返回解绑函数供卸载清理）
function bindCtxMenuGuards() {
  const onDocCtx = () => closeCtxMenu();
  const onKey = (e) => { if (e.key === "Escape") closeCtxMenu(); };
  const onWheel = () => closeCtxMenu();
  document.addEventListener("click", closeCtxMenu);
  document.addEventListener("contextmenu", onDocCtx);
  document.addEventListener("keydown", onKey);
  document.addEventListener("wheel", onWheel, { passive: true });
  return () => {
    document.removeEventListener("click", closeCtxMenu);
    document.removeEventListener("contextmenu", onDocCtx);
    document.removeEventListener("keydown", onKey);
    document.removeEventListener("wheel", onWheel);
  };
}
const offCtxMenuGuards = bindCtxMenuGuards();

/* ── 岗位 CRUD ────────────────────────────────────────────────── */
const posVisible = ref(false);
const posForm = ref(null);
// 岗位范围模式（业界 RBAC 岗位数据/业务范围）：dept=仅归属部门（默认）/
// all=全公司 / custom=指定部门多选；保存时映射回 scopeAll/scopeDeptIds
//（后端 SavePosition→OrgStore.checkPositionScopeLocked 校验存在性+去重）。
const posScopeMode = ref("dept");
function openPosNew() {
  posForm.value = { id: "", name: "", code: "", level: "", deptId: "", description: "", status: "active", scopeAll: false, scopeDeptIds: [] };
  posScopeMode.value = "dept";
  posVisible.value = true;
}
function openPosEdit(p) {
  posForm.value = JSON.parse(JSON.stringify(p));
  if (!Array.isArray(posForm.value.scopeDeptIds)) posForm.value.scopeDeptIds = [];
  // 由存量字段反推范围模式（scopeAll 优先；无指定范围 = 仅归属部门）。
  posScopeMode.value = p.scopeAll ? "all" : (p.scopeDeptIds || []).length ? "custom" : "dept";
  posVisible.value = true;
}
async function savePos() {
  const v = posForm.value;
  if (!v.name || !v.name.trim()) {
    Message.warning("请填写岗位名称");
    return false;
  }
  // 范围模式 → 字段映射（全公司清空指定范围；指定部门至少选 1 个）。
  v.scopeAll = posScopeMode.value === "all";
  v.scopeDeptIds = posScopeMode.value === "custom" ? (v.scopeDeptIds || []) : [];
  if (posScopeMode.value === "custom" && !v.scopeDeptIds.length) {
    Message.warning("指定部门范围需至少选择 1 个部门（或改选「仅归属部门」）");
    return false;
  }
  try {
    await bridgeCall("SavePosition", JSON.parse(JSON.stringify(v)));
    Message.success("已保存");
    await loadOrg();
    return true;
  } catch (e) {
    Message.error("保存失败：" + String(e));
    return false;
  }
}
	function removePos(p) {
	  Modal.confirm({
	    title: `删除岗位「${p.name}」？`,
	    content: "被员工/渠道/定时任务引用时将拒绝删除，需先解除引用。",
	    okButtonProps: { status: "danger" },
    onOk: async () => {
      try {
        await bridgeCall("DeletePosition", p.id);
        Message.success("已删除");
        await loadOrg();
        await load();
      } catch (err) {
        Message.error("删除失败：" + String(err));
      }
    },
  });
}

/* ── 角色 CRUD（MCP/技能权限网关配置载体）──────────────────────── */
const roleVisible = ref(false);
const roleForm = ref(null);
function openRoleNew() {
  roleForm.value = {
    id: "", name: "", code: "", description: "",
    mcpMode: "", mcpDisabled: false, mcpServers: [], mcpTools: [],
    skillsDisabled: false, skillsDeny: [], readOnly: false,
  };
  roleVisible.value = true;
  loadMcpCatalog();
}
function openRoleEdit(r) {
  roleForm.value = JSON.parse(JSON.stringify(r));
  roleVisible.value = true;
  loadMcpCatalog();
}
async function saveRole() {
  const v = roleForm.value;
  if (!v.name || !v.name.trim()) {
    Message.warning("请填写角色名称");
    return false;
  }
  try {
    await bridgeCall("SaveRole", JSON.parse(JSON.stringify(v)));
    Message.success("已保存");
    await loadOrg();
    return true;
  } catch (e) {
    Message.error("保存失败：" + String(e));
    return false;
  }
}
	function removeRole(r) {
	  Modal.confirm({
	    title: `删除角色「${r.name}」？`,
	    content: "被员工直绑或岗位映射引用时将拒绝删除，需先解除引用。",
	    okButtonProps: { status: "danger" },
    onOk: async () => {
      try {
        await bridgeCall("DeleteRole", r.id);
        Message.success("已删除");
        await loadOrg();
        await load();
      } catch (err) {
        Message.error("删除失败：" + String(err));
      }
    },
  });
}

// 文本 ⇄ 数组 双向 computed（textarea/input 编辑逗号分隔列表）
const mcpServerText = computed({
  get: () => (form.value && form.value.mcpServers || []).join(", "),
  set: (v) => {
    if (!form.value) return;
    form.value.mcpServers = String(v || "").split(/[,,\n]/).map((s) => s.trim()).filter(Boolean);
  },
});
const skillsDenyText = computed({
  get: () => (form.value && form.value.skillsDeny || []).join(", "),
  set: (v) => {
    if (!form.value) return;
    form.value.skillsDeny = String(v || "").split(/[,,\n]/).map((s) => s.trim()).filter(Boolean);
  },
});
// 员工级 MCP 工具白/黑名单（mcpMode + mcpTools；Gateway 三层判定之一，
// 角色仅收紧不替代：运行期以员工自身配置为基准叠加角色限制）。
const empMcpToolsText = computed({
  get: () => (form.value && form.value.mcpTools || []).join(", "),
  set: (v) => {
    if (!form.value) return;
    form.value.mcpTools = String(v || "").split(/[,,\n]/).map((s) => s.trim()).filter(Boolean);
  },
});
// 目录白名单（§7）：角色不覆盖 dirs（收紧式合并不含目录），员工自身是唯一配置入口。
const empDirsText = computed({
  get: () => (form.value && form.value.dirs || []).join("\n"),
  set: (v) => {
    if (!form.value) return;
    form.value.dirs = String(v || "").split(/\n/).map((s) => s.trim()).filter(Boolean);
  },
});
const roleMcpToolsText = computed({
  get: () => (roleForm.value && roleForm.value.mcpTools || []).join(", "),
  set: (v) => {
    if (!roleForm.value) return;
    roleForm.value.mcpTools = String(v || "").split(/[,\n]/).map((s) => s.trim()).filter(Boolean);
  },
});

/* ── MCP 工具目录树（角色编辑弹窗；勾选写回 mcpTools）──────────── */
const mcpCatalog = ref([]);
const catalogLoaded = ref(false);
const catalogLoading = ref(false);
async function loadMcpCatalog() {
  if (catalogLoaded.value || catalogLoading.value) return;
  catalogLoading.value = true;
  try {
    mcpCatalog.value = (await bridgeCall("ListMCPToolCatalog")) || [];
    catalogLoaded.value = true;
  } catch {
    mcpCatalog.value = [];
  } finally {
    catalogLoading.value = false;
  }
}
// 目录树数据：server 节点（插件来源加标记）→ tools 子节点；勾选 server 节点跳过（不入 mcpTools）
const mcpCatalogTree = computed(() =>
  mcpCatalog.value.map((s) => ({
    key: "srv:" + s.server,
    title: s.server + (s.source === "plugin" ? "（插件）" : ""),
    children: (s.tools || []).map((t) => ({ key: t.fullName, title: t.name })),
  }))
);
// 勾选回写：保留手动填写的、不在目录中的条目（mcp_server_* 通配等），再并入勾选的工具全名
function onCatalogCheck(list) {
  if (!roleForm.value) return;
  const manual = (roleForm.value.mcpTools || []).filter((t) => !mcpCatalog.value.some((s) => (s.tools || []).some((x) => x.fullName === t)));
  const picked = [];
  for (const k of list || []) {
    if (String(k).startsWith("srv:")) continue;
    picked.push(String(k));
  }
  roleForm.value.mcpTools = [...new Set([...manual, ...picked])];
}
const catalogChecked = computed(() => {
  const set = new Set(roleForm.value && roleForm.value.mcpTools || []);
  const out = [];
  for (const s of mcpCatalog.value) {
    for (const t of s.tools || []) if (set.has(t.fullName)) out.push(t.fullName);
  }
  return out;
});
// 树勾选可写代理：读 = 目录内已勾选；写 = onCatalogCheck 回写（保留目录外手动条目）
const catalogCheckedProxy = computed({
  get: () => catalogChecked.value,
  set: (v) => onCatalogCheck(v),
});
	// 角色级 MCP 服务器白名单（逗号分隔）
	const roleMcpServersText = computed({
  get: () => (roleForm.value && roleForm.value.mcpServers || []).join(", "),
  set: (v) => {
    if (!roleForm.value) return;
    roleForm.value.mcpServers = String(v || "").split(/[,,\n]/).map((s) => s.trim()).filter(Boolean);
  },
});
const roleSkillsDenyText = computed({
  get: () => (roleForm.value && roleForm.value.skillsDeny || []).join(", "),
  set: (v) => {
    if (!roleForm.value) return;
    roleForm.value.skillsDeny = String(v || "").split(/[,,\n]/).map((s) => s.trim()).filter(Boolean);
  },
});

/* ── 群聊（发起群聊弹窗；聊天视图在 GroupChatPanel）────────────── */
const groups = ref([]);          // Group[]
const groupCreateVisible = ref(false);
const groupNewName = ref("");
const groupNewMembers = ref([]); // 已勾选的员工 ID
const groupGoTo = ref("");       // 跳转到群聊 Tab 时定位的群 ID

async function loadGroups() {
  try {
    groups.value = (await bridgeCall("GroupList")) || [];
  } catch {
    groups.value = [];
  }
}

function openGroupCreate() {
  groupNewName.value = "";
  groupNewMembers.value = [];
  groupCreateVisible.value = true;
}

function toggleGroupMember(id) {
  const i = groupNewMembers.value.indexOf(id);
  if (i >= 0) groupNewMembers.value.splice(i, 1);
  else groupNewMembers.value.push(id);
}

async function createGroup() {
  const name = groupNewName.value.trim();
  if (!name) {
    Message.warning("请填写群名称");
    return false;
  }
  try {
    const g = await bridgeCall("GroupCreate", name, groupNewMembers.value);
    Message.success("群聊已创建");
    groupCreateVisible.value = false;
    await loadGroups();
    if (g && g.id) { await openGroup(g); }
    return true;
  } catch (e) {
    Message.error("创建群聊失败：" + String(e));
    return false;
  }
}

// 跳转到群聊 Tab 并定位指定群（GroupChatPanel 监听 initial-group-id 变化）
function openGroup(g) {
  if (!g || !g.id) return;
  groupGoTo.value = g.id;
  activeTab.value = "group";
}

// kaelvoris:groups 事件：群列表变更时同步本地缓存（含新建/解散/成员变更）
const offGroups = onEvent("kaelvoris:groups", (list) => {
  if (Array.isArray(list)) groups.value = list;
});

/* ── 岗位关联资源计数（岗位卡上的「关联资源 N」入口）────────── */
// 岗位归属摘要（岗位卡组织行）：归属部门路径；无归属 = 通用岗位。
function posOrgText(p) {
  if (!p || !p.deptId) return "";
  return (deptFlatOptions.value.find((o) => o.id === p.deptId) || {}).path || "";
}
const posBindCounts = ref({});
async function loadPosBindCounts() {
  try {
    const binds = (await bridgeCall("ListResourceBindings")) || [];
    const counts = {};
    for (const b of binds) {
      // positionId 优先；仅绑员工时按员工当前岗位归组（ListResourceBindings
      // 返回的 employeeId 为 ID，须查员工字典取 positionId）。
      const pid = b.positionId || (employees.value.find((x) => x.id === b.employeeId) || {}).positionId;
      if (pid) counts[pid] = (counts[pid] || 0) + 1;
    }
    posBindCounts.value = counts;
  } catch {
    posBindCounts.value = {};
  }
}
// 跳转到资源管理页签（岗位视图；2026-10-25 融合：资源管理并入本面板，内切页签）
function openResourcePositions() {
  activeTab.value = "resources";
}
// 资源页签内「管理 / 管理角色 / 组织管理」跳转（内嵌模式 emit 回传）→ 切到组织架构页签
function switchEmpTab(t) {
  activeTab.value = t;
}
const offCronForBinds = onEvent("kaelvoris:cron", () => loadPosBindCounts());

// 表单工作空间提示（优先用户指定根，否则显示后端实际路径）
const formWorkspacePath = computed(() => form.value && (form.value.workspaceRoot || form.value.workspacePath) || "");

/* ── 部门导航树节点（h() 渲染器；Vue SFC 内以变量引用，勿删）───── */
const DeptNavNode = {
  name: "DeptNavNode",
  props: { node: { type: Object, required: true }, cur: { type: String, default: "" } },
  emits: ["select"],
  setup(props, { emit }) {
    const expanded = ref(true);
    return () => {
      const n = props.node;
      const kids = n && n.children && n.children.length
        ? h("div", { style: { paddingLeft: "12px" } },
            n.children.map((c) => h(DeptNavNode, {
              key: c.id, node: c, cur: props.cur,
              onSelect: (id) => emit("select", id),
            })))
        : null;
      return h("div", { class: "dept-nav-node" }, [
        h("div", {
          class: ["dept-nav-row", { on: props.cur === n.id }],
          onClick: () => emit("select", n.id),
        }, [
          h("span", {
            class: "dept-nav-caret",
            onClick: (e) => { e.stopPropagation(); expanded.value = !expanded.value; },
          }, expanded.value ? "▾" : "▸"),
          h("span", { class: "dept-nav-name", title: n.name }, n.name),
          h("span", { class: "dept-nav-count" }, deptTotal(n.id) || ""),
        ]),
        expanded.value && kids ? kids : null,
      ]);
    };
  },
};

// 组织架构 Tab 的部门树节点（行选中查看 + 右键上下文菜单全量管理；
// 行内「+」与图标按钮已移除，增删改查统一走右键命令，对齐钉钉/企微组织树惯例）
const DeptNode = {
  name: "DeptNode",
  props: {
    node: { type: Object, required: true },
    cur: { type: String, default: "" }, // 当前选中部门 id（"" = 概览）
  },
  emits: ["select", "menu"],
  setup(props, { emit }) {
    const expanded = ref(true);
    const hasKids = (n) => !!(n.children && n.children.length);
    return () => {
      const n = props.node;
      const kids = hasKids(n)
        ? h("div", { class: "dtree-kids" },
            n.children.map((c) => h(DeptNode, {
              key: c.id, node: c, cur: props.cur,
              onSelect: (id) => emit("select", id),
              onMenu: (p) => emit("menu", p),
            })))
        : null;
      return h("div", { class: "dtree-node" }, [
        h("div", {
          class: ["dtree-row", { on: props.cur === n.id, off: n.status === "disabled" }],
          onClick: () => emit("select", n.id),
          onContextmenu: (e) => {
            e.preventDefault();
            e.stopPropagation(); // 阻断到 document 的冒泡（document 右键监听负责关闭菜单）
            emit("menu", { node: n, event: e });
          },
        }, [
          h("span", {
            class: ["dtree-caret", { leaf: !hasKids(n) }],
            onClick: (e) => { e.stopPropagation(); expanded.value = !expanded.value; },
          }, hasKids(n) ? (expanded.value ? "▾" : "▸") : ""),
          h("span", { class: "dtree-name", title: n.code ? n.name + "（" + n.code + "）" : n.name }, n.name),
          n.status === "disabled" ? h("span", { class: "dtree-flag" }, "停用") : null,
          deptTotal(n.id) ? h("span", { class: "dtree-count" }, deptTotal(n.id)) : null,
        ]),
        expanded.value && kids ? kids : null,
      ]);
    };
  },
};

onBeforeUnmount(() => {
  offEmployees();
  offGroups();
  offCronForBinds();
  offCtxMenuGuards();
});

const offEmployees = onEvent("kaelvoris:employees", (list) => {
  if (Array.isArray(list)) employees.value = list;
});

load();
loadGroups();
loadOrg();
loadPosBindCounts();
</script>

<template>
  <div class="emp-panel">

    <!-- 顶部分段页签：员工 / 组织架构 / 群聊 / 资源（胶囊滑块式，样式 .emp-tabs） -->
    <div class="emp-tabs" role="tablist">
      <button :class="{ active: activeTab === 'emp' }" role="tab" @click="activeTab = 'emp'"><IconUser />员工</button>
      <button :class="{ active: activeTab === 'org' }" role="tab" @click="activeTab = 'org'"><IconRelation />组织架构</button>
      <button :class="{ active: activeTab === 'group' }" role="tab" @click="activeTab = 'group'"><IconUserGroup />群聊</button>
      <button :class="{ active: activeTab === 'resources' }" role="tab" @click="activeTab = 'resources'"><IconStorage />资源</button>
    </div>

    <!-- ══ 员工 Tab ══ -->
    <div v-if="activeTab === 'emp'" class="emp-body">
      <!-- 头部：图标 + 标题组 + 操作按钮（群聊入口合并为「发起群聊」单入口） -->
      <div class="emp-head">
        <span class="panel-icon"><IconUserGroup /></span>
        <div class="emp-head-titles">
          <div class="emp-title">数字员工</div>
          <div class="panel-sub">部门 · 岗位 · 角色 · 权限网关，一体化管理</div>
        </div>
        <div class="flex-sp"></div>
        <button class="icon-btn" title="刷新列表" @click="load(); loadPosBindCounts()"><IconRefresh /></button>
        <!-- 自主迭代入口（2026-12 融合：导航项移除，由本按钮进入 roadmap 规划板视图） -->
        <button class="emp-btn-ghost" title="AI 规划 → 审核 → 排期开发闭环" @click="store.activeMain = 'roadmap'"><IconThunderbolt />自主迭代</button>
        <button class="emp-btn-ghost" @click="openGroupCreate">发起群聊</button>
        <button class="emp-btn-primary" @click="openNew"><IconPlus />新建员工</button>
      </div>

      <!-- 统计条：纯数字组 -->
      <div class="emp-stats-row">
        <div class="emp-stat">
          <span class="emp-stat-num">{{ employees.length }}</span>
          <span class="emp-stat-label">员工总数</span>
        </div>
        <div class="emp-stat">
          <span class="emp-stat-num ok">{{ activeCount }}</span>
          <span class="emp-stat-label">在职启用</span>
        </div>
        <div class="emp-stat">
          <span class="emp-stat-num">{{ deptTree.length }}</span>
          <span class="emp-stat-label">部门</span>
        </div>
        <div class="emp-stat">
          <span class="emp-stat-num">{{ groups.length }}</span>
          <span class="emp-stat-label">群聊</span>
        </div>
      </div>

      <!-- 主体：部门导航 + 员工列表 -->
      <div class="emp-main">
        <div class="dept-nav">
          <div class="dept-nav-head"><span>部门</span></div>
          <div class="dept-nav-list">
            <div class="dept-nav-row" :class="{ on: curDeptId === '' }" @click="curDeptId = ''">
              <span class="dept-nav-caret dept-nav-caret-root"></span>
              <span class="dept-nav-name">全部员工</span>
              <span class="dept-nav-count">{{ employees.length }}</span>
            </div>
            <div class="dept-nav-row" :class="{ on: curDeptId === 'none' }" @click="curDeptId = 'none'">
              <span class="dept-nav-caret dept-nav-caret-root"></span>
              <span class="dept-nav-name">未分配部门</span>
              <span class="dept-nav-count">{{ employees.filter((e) => !e.deptId).length }}</span>
            </div>
            <template v-for="n in deptTree" :key="n.id">
              <DeptNavNode :node="n" :cur="curDeptId" @select="(id) => curDeptId = id" />
            </template>
          </div>
        </div>

        <div class="emp-list-wrap">
          <div class="emp-search">
            <input v-model="empSearch" class="emp-search-inp" type="text" placeholder="搜索姓名 / 工号 / 岗位 / 职责…" />
            <button v-if="empSearch" class="emp-search-clear" title="清空" @click="empSearch = ''">✕</button>
          </div>
          <div v-if="!loading && searchedEmployees.length === 0" class="empty-block emp-empty">
            <div class="empty-ico-chip"><IconUserGroup /></div>
            <div class="empty-block-title">暂无员工</div>
            <div class="empty-block-hint">点击右上角「新建员工」创建第一位数字员工</div>
          </div>
          <div v-else class="emp-list">
            <div v-for="(e, i) in searchedEmployees" :key="e.id" class="emp-card panel-card card-lift card-in" :style="{ '--i': i }">
              <div class="emp-card-main">
                <div class="emp-avatar" :class="{ off: e.status !== 'enabled' }">{{ e.avatar || '👤' }}</div>
                <div class="emp-card-body">
                  <div class="emp-row1">
                    <span class="emp-name" :title="e.name">{{ e.name }}</span>
                    <span v-if="e.positionName" class="emp-pos">{{ e.positionName }}</span>
                    <span class="emp-status" :class="empStatusCls(e)">{{ empStatusLabel(e) }}</span>
                    <div class="flex-sp"></div>
                    <div class="emp-row-actions">
                      <button class="icon-btn" title="编辑" @click="openEdit(e.id)"><IconEdit /></button>
                      <button class="icon-btn" title="复制" @click="duplicateEmp($event, e)"><IconCopy /></button>
                      <button class="icon-btn" title="删除" @click="removeEmp($event, e)"><IconDelete /></button>
                    </div>
                  </div>
                  <div v-if="e.description" class="emp-desc" :title="e.description">{{ e.description }}</div>
                  <div class="emp-org-meta">
                    <span v-if="e.deptName" class="emp-org-chip">{{ e.deptName }}</span>
                    <span v-if="e.empNo" class="emp-org-item">工号 {{ e.empNo }}</span>
                    <span v-if="e.managerName" class="emp-org-item">直属上级 {{ e.managerName }}</span>
                  </div>
                  <div class="emp-meta">
                    <span v-if="e.readOnly" class="emp-readonly">只读模式</span>
                    <span v-if="e.model" class="emp-model">{{ e.model }}</span>
                    <span v-if="e.workspaceRoot || e.workspacePath" class="emp-ws" :title="e.workspaceRoot || e.workspacePath">{{ e.workspaceRoot || e.workspacePath }}</span>
                  </div>
                </div>
              </div>
              <div class="emp-actions">
                <button class="emp-btn-mini" @click="toggleStatus(e)">{{ e.status === 'enabled' ? '停用' : '启用' }}</button>
                <button class="emp-btn-mini" @click="bindToSession(e)">绑定到当前会话</button>
                <span v-if="store.activeEmployeeId === e.id" class="emp-bound">● 当前会话</span>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- ══ 组织架构 Tab ══（主从布局：左部门树管理位 + 右详情列） -->
    <div v-else-if="activeTab === 'org'" class="org-body">
      <div class="org-main">
        <!-- 左：部门树（本 Tab 管理位：行选中 + 行内增删改 + 底部新建） -->
        <aside class="org-tree panel-card">
          <div class="org-tree-head">
            <span class="org-tree-title">部门</span>
            <span class="org-tree-sub">{{ orgStats.depts }}</span>
            <span class="flex-sp"></span>
            <span class="org-tree-tip">右键管理</span>
          </div>
          <div class="org-tree-body" @contextmenu.prevent.stop="openDeptCtxMenu($event, null)">
            <div class="dtree-row dtree-all" :class="{ on: orgSelId === '' }" @click="orgSelId = ''" @contextmenu.prevent.stop="openDeptCtxMenu($event, null)">
              <span class="dtree-caret dtree-caret-root"></span>
              <span class="dtree-name">全部部门</span>
              <span class="dtree-count">{{ orgStats.depts }}</span>
            </div>
            <div v-if="!deptTree.length" class="org-tree-empty">
              <div class="org-tree-empty-title">还没有部门</div>
              <div class="org-tree-empty-hint">右键此处新建第一个部门，支持多级子部门</div>
            </div>
            <template v-for="n in deptTree" :key="n.id">
              <DeptNode
                :node="n" :cur="orgSelId"
                @select="(id) => orgSelId = id"
                @menu="(p) => openDeptCtxMenu({ x: p.event.clientX, y: p.event.clientY }, p.node)"
              />
            </template>
          </div>
        </aside>

        <!-- 右：详情列（部门卡 / 全部概览 + 岗位·角色子页签） -->
        <div class="org-detail">
          <!-- 详情头：选中部门卡 或 全部概览 -->
          <div v-if="orgSelDept" class="org-deptcard panel-card" @contextmenu.prevent.stop="openDeptCtxMenu($event, orgSelDept)">
            <div class="org-deptcard-row1">
              <span class="org-deptcard-name">{{ orgSelDept.name }}</span>
              <span v-if="orgSelDept.code" class="org-item-code">{{ orgSelDept.code }}</span>
              <span v-if="orgSelDept.status === 'disabled'" class="org-item-warn">已停用</span>
            </div>
            <div class="org-deptcard-meta">
              <span class="org-deptcard-stat"><b>{{ deptDirectCount(orgSelDept.id) }}</b>直属员工</span>
              <span class="org-deptcard-stat"><b>{{ deptChildCount(orgSelDept.id) }}</b>子部门</span>
              <span v-if="empName(orgSelDept.leaderId)" class="org-deptcard-stat"><b>{{ empName(orgSelDept.leaderId) }}</b>负责人</span>
            </div>
            <div v-if="!orgSelDept.children || !orgSelDept.children.length" class="org-deptcard-hint">
              该部门暂无子部门，右键左侧部门行可添加
            </div>
          </div>
          <div v-else class="org-deptcard org-deptcard-all panel-card" @contextmenu.prevent.stop="openDeptCtxMenu($event, null)">
            <div class="org-deptcard-row1">
              <span class="org-deptcard-name">组织概览</span>
            </div>
            <div class="org-deptcard-meta">
              <span class="org-deptcard-stat"><b>{{ orgStats.depts }}</b>部门</span>
              <span class="org-deptcard-stat"><b>{{ orgStats.positions }}</b>岗位</span>
              <span class="org-deptcard-stat"><b>{{ orgStats.roles }}</b>角色</span>
              <span class="org-deptcard-stat"><b>{{ orgStats.bound }}</b>已配岗员工</span>
            </div>
          </div>

          <!-- 右侧子页签：岗位 / 角色 -->
          <div class="org-subtabs">
            <button :class="{ active: orgSub === 'pos' }" @click="orgSub = 'pos'">岗位<span v-if="orgPositions.length">（{{ orgPositions.length }}）</span></button>
            <button :class="{ active: orgSub === 'role' }" @click="orgSub = 'role'">角色<span v-if="roles.length">（{{ roles.length }}）</span></button>
          </div>

          <!-- 岗位列表（右键卡片：编辑/关联资源/删除；右键空白：新建） -->
          <div v-if="orgSub === 'pos'" class="org-detail-list" @contextmenu.prevent.stop="onListCtxMenu">
            <div v-if="!orgPositions.length" class="empty-block org-empty-block">
              <div class="empty-ico-chip"><IconRelation /></div>
              <div class="empty-block-title">{{ orgSelId ? "该部门下暂无岗位" : "暂无岗位" }}</div>
              <div class="empty-block-hint">岗位 = 职责编制 + 权限包载体，右键此处新建</div>
            </div>
            <template v-else>
              <div v-for="(p, i) in orgPositions" :key="p.id" class="org-poscard panel-card card-lift card-in" :style="{ '--i': i }"
                @contextmenu.prevent.stop="openPosCtxMenu($event, p)">
                <div class="org-poscard-row1">
                  <span class="org-poscard-name">{{ p.name }}</span>
                  <span v-if="p.code" class="org-item-code">{{ p.code }}</span>
                  <span v-if="p.level" class="org-item-level">{{ p.level }}</span>
                </div>
                <div v-if="posOrgText(p) || p.scopeAll || (p.scopeDeptIds || []).length || p.roleIds?.length" class="org-poscard-meta">
                  <span v-if="posOrgText(p)" class="org-poscard-chip">{{ posOrgText(p) }}</span>
                  <span v-if="p.scopeAll" class="org-item-scope">全公司范围</span>
                  <span v-else-if="(p.scopeDeptIds || []).length" class="org-item-scope">范围 {{ p.scopeDeptIds.length }} 部门</span>
                  <span v-if="p.roleIds?.length" class="org-item-scope role">{{ p.roleIds.length }} 角色</span>
                </div>
                <div v-if="p.description" class="org-poscard-desc">{{ p.description }}</div>
                <button class="org-item-res" @click="openResourcePositions">关联资源 {{ posBindCounts[p.id] || 0 }}</button>
              </div>
            </template>
          </div>

          <!-- 角色列表（右键卡片：编辑/删除；右键空白：新建） -->
          <div v-else class="org-detail-list" @contextmenu.prevent.stop="onListCtxMenu">
            <div v-if="!roles.length" class="empty-block org-empty-block">
              <div class="empty-ico-chip"><IconSafe /></div>
              <div class="empty-block-title">暂无角色</div>
              <div class="empty-block-hint">角色 = 权限包（MCP/技能/只读），供岗位映射或员工直绑；右键此处新建</div>
            </div>
            <template v-else>
              <div v-for="(r, i) in roles" :key="r.id" class="org-poscard panel-card card-lift card-in" :style="{ '--i': i }"
                @contextmenu.prevent.stop="openRoleCtxMenu($event, r)">
                <div class="org-poscard-row1">
                  <span class="org-poscard-name">{{ r.name }}</span>
                  <span v-if="r.code" class="org-item-code">{{ r.code }}</span>
                </div>
                <div class="org-poscard-meta">
                  <span v-if="r.readOnly" class="org-item-warn">只读</span>
                  <span v-if="r.mcpDisabled" class="org-item-warn">MCP 禁用</span>
                  <span v-else-if="r.mcpMode === 'whitelist'" class="org-item-scope role">MCP 白名单</span>
                  <span v-else-if="r.mcpMode === 'blacklist'" class="org-item-scope warn">MCP 黑名单</span>
                  <!-- mcpMode 空串/其他值不展示（未限制） -->
                  <span v-if="r.skillsDisabled" class="org-item-warn">技能禁用</span>
                  <span v-if="r.skillsDeny && r.skillsDeny.length" class="org-item-warn">技能拒绝 {{ r.skillsDeny.length }}</span>
                  <span v-if="roleNoRestrict(r)" class="org-poscard-none">未配置权限限制</span>
                </div>
                <div v-if="r.description" class="org-poscard-desc">{{ r.description }}</div>
              </div>
            </template>
          </div>
        </div>
      </div>
    </div>

    <!-- ══ 资源 Tab ══（2026-10-25 融合：原独立「资源管理」面板内嵌为页签，
         面板内「管理」类跳转经 nav-employee 回传 → switchEmpTab 切页签） -->
    <div v-else-if="activeTab === 'resources'" class="emp-body emp-res-body">
      <ResourcePanel embedded @nav-employee="switchEmpTab" />
    </div>

    <!-- ══ 群聊 Tab ══ -->
    <GroupChatPanel
      v-else
      :initial-group-id="groupGoTo"
      @back="activeTab = 'emp'"
      @create="openGroupCreate"
    />

    <!-- ══ 部门树右键菜单（Teleport 到 body，避免面板 overflow 裁剪；放在页签链之后避免打断 v-else-if）══ -->
    <teleport to="body">
      <div
        v-if="ctxMenu"
        class="ctx-menu"
        :style="{ left: ctxMenu.x + 'px', top: ctxMenu.y + 'px' }"
        @contextmenu.prevent
      >
        <template v-for="(it, i) in ctxMenu.items" :key="i">
          <div v-if="it.sep" class="ctx-sep"></div>
          <button v-else class="ctx-item" :class="{ danger: it.danger }" @click="onCtxItemClick(it)">{{ it.label }}</button>
        </template>
      </div>
    </teleport>

    <!-- ══ 员工编辑弹窗 ══ -->
    <a-modal
      v-model:visible="editVisible"
      :width="760"
      :title="form && form.id ? '编辑员工' : '新建员工'"
      :mask-closable="false"
      @before-ok="save"
    >
      <div v-if="form" class="emp-form">
        <!-- 基础信息 -->
        <div class="org-section">
          <div class="org-section-title">基础信息</div>
          <div class="emp-grid2">
            <div class="set-section">
              <label class="set-label">名称 *</label>
              <a-input v-model="form.name" placeholder="如：小张 / 客服专员" />
            </div>
            <div class="set-section">
              <label class="set-label">头像（emoji）</label>
              <a-input v-model="form.avatar" placeholder="如：👩‍💼 🤖 🧑‍💻" />
            </div>
          </div>
          <div class="set-section">
            <label class="set-label">职责描述</label>
            <a-textarea v-model="form.description" :auto-size="{ minRows: 2, maxRows: 4 }" placeholder="一句话描述该员工的职责边界与擅长领域" />
          </div>
          <div class="set-section">
            <label class="set-label">角色提示词（rolePrompt）</label>
            <a-textarea v-model="form.rolePrompt" :auto-size="{ minRows: 3, maxRows: 8 }" placeholder="该员工的系统级人设与工作方式说明，会注入对话系统提示词" />
          </div>
          <div class="emp-grid2">
            <div class="set-section">
              <label class="set-label">模型</label>
              <a-input v-model="form.model" placeholder="留空使用全局默认模型" />
            </div>
            <div class="set-section">
              <label class="set-label">进化强度 {{ form.evolutionStrength }}</label>
              <a-slider v-model="form.evolutionStrength" :min="0" :max="100" :step="5" />
            </div>
          </div>
          <div class="emp-grid2">
            <div class="set-section">
              <label class="set-label">工作空间根目录</label>
              <a-input v-model="form.workspaceRoot" placeholder="如 D:/work/proj-a（留空用全局默认）" />
            </div>
            <div class="set-section">
              <label class="set-label">当前工作空间</label>
              <div class="set-hint ws-hint">{{ formWorkspacePath || '未设置（使用全局默认）' }}</div>
            </div>
          </div>
        </div>

        <!-- 人事信息（EHR） -->
        <div class="org-section">
          <div class="org-section-title">人事信息</div>
          <div class="emp-grid2">
            <div class="set-section">
              <label class="set-label">工号</label>
              <a-input v-model="form.empNo" placeholder="如 GH-0001" />
            </div>
            <div class="set-section">
              <label class="set-label">人事状态</label>
              <a-select v-model="form.empStatus" :options="empStatusOptions" />
            </div>
          </div>
          <div class="emp-grid2">
            <div class="set-section">
              <label class="set-label">性别</label>
              <a-select v-model="form.gender" :options="genderOptions" placeholder="保密" allow-clear />
            </div>
            <div class="set-section">
              <label class="set-label">出生日期</label>
              <a-input v-model="form.birthday" placeholder="如 1995-06-01" />
            </div>
          </div>
          <div class="emp-grid2">
            <div class="set-section">
              <label class="set-label">手机</label>
              <a-input v-model="form.phone" placeholder="手机号" />
            </div>
            <div class="set-section">
              <label class="set-label">邮箱</label>
              <a-input v-model="form.email" placeholder="name@company.com" />
            </div>
          </div>
          <div class="emp-grid2">
            <div class="set-section">
              <label class="set-label">入职日期</label>
              <a-input v-model="form.hireDate" placeholder="如 2026-01-01" />
            </div>
            <div class="set-section">
              <label class="set-label">直属上级</label>
              <a-select v-model="form.managerId" allow-clear placeholder="无">
                <a-option v-for="o in empOptions.filter((x) => x.id !== form.id)" :key="o.id" :value="o.id">{{ o.name }}</a-option>
              </a-select>
            </div>
          </div>
          <div class="emp-grid2">
            <div class="set-section">
              <label class="set-label">部门</label>
              <a-select v-model="form.deptId" allow-clear placeholder="未分配">
                <a-option v-for="o in deptFlatOptions" :key="o.id" :value="o.id">{{ o.path }}</a-option>
              </a-select>
            </div>
            <div class="set-section">
              <label class="set-label">岗位</label>
              <a-select v-model="form.positionId" allow-clear placeholder="未分配">
                <a-option v-for="p in positions" :key="p.id" :value="p.id">{{ p.name }}</a-option>
              </a-select>
            </div>
          </div>
          <div class="set-section">
            <label class="set-label">角色（可多选）</label>
            <a-select v-model="form.roleIds" multiple allow-clear placeholder="未绑定角色">
              <a-option v-for="r in roles" :key="r.id" :value="r.id">{{ r.name }}</a-option>
            </a-select>
            <div class="set-hint">直绑角色仅作兑底临时授权（如临时开放某个 MCP 工具）；日常权限随岗位自动继承（岗位→角色映射，在「资源管理 → 岗位权限」维护）。有效角色 = 直绑 ∪ 岗位映射，收紧式合并。</div>
          </div>
        </div>

        <!-- 权限与工具 -->
        <div class="org-section">
          <div class="org-section-title">权限与工具</div>
          <div class="emp-grid2">
            <div class="set-section">
              <a-checkbox v-model="form.readOnly">只读模式（禁止写操作与 shell 执行）</a-checkbox>
            </div>
            <div class="set-section">
              <a-checkbox v-model="form.mcpDisabled">禁用 MCP 工具</a-checkbox>
            </div>
          </div>
          <div class="emp-grid2">
            <div class="set-section">
              <label class="set-label">MCP 模式</label>
              <a-select v-model="form.mcpMode" :options="roleMCPOptions" />
            </div>
            <div class="set-section">
              <label class="set-label">MCP 工具清单</label>
              <a-textarea v-model="empMcpToolsText" :auto-size="{ minRows: 1, maxRows: 3 }" placeholder="支持 * 通配，如 mcp_github_*；模式为白/黑名单时生效" />
            </div>
          </div>
          <div class="set-section">
            <label class="set-label">MCP 服务器白名单</label>
            <a-textarea v-model="mcpServerText" :auto-size="{ minRows: 1, maxRows: 3 }" placeholder="留空 = 不限制；填服务器名，逗号或换行分隔" />
            <div class="set-hint">仅在「禁用 MCP 工具」未勾选时生效</div>
          </div>
          <div class="set-section">
            <a-checkbox v-model="form.skillsDisabled">禁用技能（SKILL.md）</a-checkbox>
          </div>
          <div class="set-section">
            <label class="set-label">技能黑名单</label>
            <a-textarea v-model="skillsDenyText" :auto-size="{ minRows: 1, maxRows: 3 }" placeholder="支持 * 通配，如 pptx* , git-*" />
            <div class="set-hint">仅在「禁用技能」未勾选时生效</div>
          </div>
          <div class="set-section">
            <label class="set-label">目录白名单（每行一个绝对路径）</label>
            <a-textarea v-model="empDirsText" :auto-size="{ minRows: 2, maxRows: 5 }" placeholder="D:/work/proj-a" />
            <div class="set-hint">额外允许访问的目录（支持 dir\* 前缀通配）；工作空间与当前会话目录始终可访问</div>
          </div>
        </div>
      </div>
    </a-modal>

    <!-- ══ 部门编辑弹窗 ══ -->
    <a-modal
      v-model:visible="deptVisible"
      :width="480"
      :title="deptForm && deptForm.id ? '编辑部门' : '新建部门'"
      :mask-closable="false"
      @before-ok="saveDept"
    >
      <div v-if="deptForm">
        <div class="set-section">
          <label class="set-label">名称 *</label>
          <a-input v-model="deptForm.name" placeholder="如：研发部" />
        </div>
        <div class="emp-grid2">
          <div class="set-section">
            <label class="set-label">编码</label>
            <a-input v-model="deptForm.code" placeholder="如 RD" />
          </div>
          <div class="set-section">
            <label class="set-label">排序</label>
            <a-input-number v-model="deptForm.sortOrder" :min="0" style="width: 100%" />
          </div>
        </div>
        <div class="set-section">
          <label class="set-label">上级部门</label>
          <a-select v-model="deptForm.parentId" allow-clear placeholder="无（作为顶级部门）">
            <a-option v-for="o in deptFlatOptions.filter((x) => x.id !== deptForm.id)" :key="o.id" :value="o.id">{{ o.path }}</a-option>
          </a-select>
        </div>
        <div class="emp-grid2">
          <div class="set-section">
            <label class="set-label">负责人</label>
            <a-select v-model="deptForm.leaderId" allow-clear placeholder="无">
              <a-option v-for="e in employees" :key="e.id" :value="e.id">{{ e.name }}</a-option>
            </a-select>
          </div>
          <div class="set-section">
            <label class="set-label">状态</label>
            <a-select v-model="deptForm.status">
              <a-option value="active">启用</a-option>
              <a-option value="disabled">停用（新员工不可选）</a-option>
            </a-select>
          </div>
        </div>
      </div>
    </a-modal>

    <!-- ══ 岗位编辑弹窗 ══ -->
    <a-modal
      v-model:visible="posVisible"
      :width="480"
      :title="posForm && posForm.id ? '编辑岗位' : '新建岗位'"
      :mask-closable="false"
      @before-ok="savePos"
    >
      <div v-if="posForm">
        <div class="emp-grid2">
          <div class="set-section">
            <label class="set-label">名称 *</label>
            <a-input v-model="posForm.name" placeholder="如：前端工程师" />
          </div>
          <div class="set-section">
            <label class="set-label">编码</label>
            <a-input v-model="posForm.code" placeholder="如 FE" />
          </div>
        </div>
        <div class="emp-grid2">
          <div class="set-section">
            <label class="set-label">职级</label>
            <a-input v-model="posForm.level" placeholder="如 P5 / L3" />
          </div>
          <div class="set-section">
            <label class="set-label">所属部门</label>
            <a-select v-model="posForm.deptId" allow-clear placeholder="未分配">
              <a-option v-for="o in deptFlatOptions" :key="o.id" :value="o.id">{{ o.path }}</a-option>
            </a-select>
          </div>
        </div>
        <!-- 岗位范围（业界 RBAC：左侧导航点部门时按范围展示岗位；
             全公司岗位在任意部门下可见） -->
        <div class="set-section">
          <label class="set-label">岗位范围</label>
          <a-radio-group v-model="posScopeMode" size="small">
            <a-radio value="dept">仅归属部门</a-radio>
            <a-radio value="all">全公司</a-radio>
            <a-radio value="custom">指定部门（多选）</a-radio>
          </a-radio-group>
          <a-select
            v-if="posScopeMode === 'custom'"
            v-model="posForm.scopeDeptIds"
            multiple
            allow-clear
            size="small"
            placeholder="选择岗位可见的部门（含子树）"
            :max-tag-count="4"
            style="margin-top: 8px"
          >
            <a-option v-for="o in deptFlatOptions" :key="o.id" :value="o.id">{{ o.path }}</a-option>
          </a-select>
          <div class="set-hint">岗位范围决定其在左侧部门导航的展示范围：全公司岗位每个部门下都可见；默认仅归属部门（含子树）。</div>
        </div>
        <div class="set-section">
          <label class="set-label">描述</label>
          <a-textarea v-model="posForm.description" :auto-size="{ minRows: 2, maxRows: 4 }" placeholder="岗位职责说明" />
        </div>
      </div>
    </a-modal>

    <!-- ══ 角色编辑弹窗 ══ -->
    <a-modal
      v-model:visible="roleVisible"
      :width="680"
      :title="roleForm && roleForm.id ? '编辑角色' : '新建角色'"
      :mask-closable="false"
      @before-ok="saveRole"
    >
      <div v-if="roleForm">
        <div class="emp-grid2">
          <div class="set-section">
            <label class="set-label">名称 *</label>
            <a-input v-model="roleForm.name" placeholder="如：后端开发" />
          </div>
          <div class="set-section">
            <label class="set-label">编码</label>
            <a-input v-model="roleForm.code" placeholder="如 BE" />
          </div>
        </div>
        <div class="set-section">
          <label class="set-label">描述</label>
          <a-textarea v-model="roleForm.description" :auto-size="{ minRows: 2, maxRows: 4 }" placeholder="角色职责说明" />
        </div>

        <div class="org-section">
          <div class="org-section-title">权限网关</div>
          <div class="emp-grid2">
            <div class="set-section">
              <label class="set-label">MCP 模式</label>
              <a-select v-model="roleForm.mcpMode" :options="roleMCPOptions" />
            </div>
            <div class="set-section">
              <label class="set-label">开关</label>
              <div class="role-checks">
                <a-checkbox v-model="roleForm.readOnly">只读</a-checkbox>
                <a-checkbox v-model="roleForm.mcpDisabled">禁用 MCP</a-checkbox>
                <a-checkbox v-model="roleForm.skillsDisabled">禁用技能</a-checkbox>
              </div>
            </div>
          </div>
          <div class="set-section">
            <label class="set-label">MCP 服务器白名单</label>
            <a-textarea v-model="roleMcpServersText" :auto-size="{ minRows: 1, maxRows: 3 }" placeholder="服务器名，逗号或换行分隔" />
          </div>
          <div class="set-section">
            <label class="set-label">MCP 工具白/黑名单</label>
            <a-tree
              v-if="mcpCatalogTree.length"
              :checkable="true"
              :data="mcpCatalogTree"
              v-model:checked-keys="catalogCheckedProxy"
              size="small"
            />
            <div v-else class="set-hint">{{ catalogLoading ? '目录加载中…' : '暂无目录数据' }}</div>
          </div>
	          <div class="set-section">
	            <label class="set-label">技能黑名单</label>
	            <a-textarea v-model="roleSkillsDenyText" :auto-size="{ minRows: 1, maxRows: 3 }" placeholder="支持 * 通配" />
	          </div>
	        </div>
      </div>
    </a-modal>

    <!-- ══ 发起群聊弹窗 ══ -->
    <a-modal
      v-model:visible="groupCreateVisible"
      :width="440"
      title="发起群聊"
      :mask-closable="false"
      @before-ok="createGroup"
    >
      <div>
        <div class="set-section">
          <label class="set-label">群名称 *</label>
          <a-input v-model="groupNewName" placeholder="如：本周发布准备" />
        </div>
        <div class="set-section">
          <label class="set-label">选择成员（可多选）</label>
          <div class="gsheet-list">
            <div
              v-for="e in employees.filter((x) => x.status !== 'archived')"
              :key="e.id"
              class="gsheet-member"
              @click="toggleGroupMember(e.id)"
            >
              <span class="gcheck" :class="{ on: groupNewMembers.includes(e.id) }"></span>
              <span class="gsheet-avatar">{{ e.avatar || '👤' }}</span>
              <span class="gsheet-name">{{ e.name }}</span>
              <span v-if="e.deptName" class="gsheet-dept">{{ e.deptName }}</span>
            </div>
          </div>
        </div>
      </div>
    </a-modal>
  </div>
</template>

<style scoped>
/* ═══ 数字员工面板 · 2026 重设计 ═══
   语言：暖黑画布上的「档案抽屉」——卡片即员工档案；陶土色仅作焦点色。
   原则：信息分层靠留白与字重，不靠边框堆叠；状态一律轻量圆点文字。 */

.emp-panel {
  height: 100%;
  display: flex;
  flex-direction: column;
  padding: var(--pad-block) var(--pad-page) 20px;
  gap: var(--gap-lg);
  overflow: hidden;
}

/* ── 顶部分段页签：胶囊滑块式 ── */
.emp-tabs {
  position: relative;
  display: flex;
  gap: 2px;
  padding: 3px;
  background: var(--bg-inset);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  width: fit-content;
}
.emp-tabs button {
  border: none;
  background: transparent;
  color: var(--text-2);
  font-size: var(--fs-caption);
  font-family: var(--font-ui);
  padding: 5px 16px;
  border-radius: calc(var(--radius) - 3px);
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  transition: color var(--transition), background var(--transition);
}
.emp-tabs button svg { transition: color var(--transition); }
.emp-tabs button:hover, .emp-tabs button:focus-visible { color: var(--text-1); background: var(--bg-hover); }
.emp-tabs button.active {
  color: var(--on-primary);
  background: var(--primary-strong);
  box-shadow: 0 1px 4px rgba(201, 100, 66, 0.28);
}
/* 非激活页签图标随文字色（避免 SVG 默认色残留） */
.emp-tabs button:not(.active) svg { color: var(--text-3); }

.emp-body, .org-body { flex: 1; display: flex; flex-direction: column; gap: 14px; min-height: 0; }
/* 资源页签：内嵌 ResourcePanel 铺满页签体（res-panel 自带 height:100%，此处补 flex 撑满） */
.emp-res-body { gap: 0; }
.emp-res-body :deep(.res-panel) { flex: 1; min-height: 0; }

/* ── 面板头部：图标 + 标题组 ── */
.emp-head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  flex-shrink: 0;
}
.emp-head-titles { display: flex; flex-direction: column; gap: 1px; min-width: 0; }
.emp-title {
  font-size: var(--fs-title);
  color: var(--text-1);
  font-weight: 650;
  letter-spacing: -0.015em;
}
.flex-sp { flex: 1; }

/* ── 统计条：纯数字组，无卡片容器 ── */
.emp-stats-row {
  display: flex;
  align-items: center;
  gap: 28px;
  padding: 2px 2px 0;
  flex-shrink: 0;
}
.emp-stat { display: flex; align-items: baseline; gap: 8px; }
.emp-stat-num {
  font-size: 22px;
  font-weight: 650;
  color: var(--text-1);
  line-height: 1.1;
  font-variant-numeric: tabular-nums;
  letter-spacing: -0.02em;
}
.emp-stat-num.ok { color: var(--success); }
.emp-stat-label { font-size: var(--fs-caption); color: var(--text-3); letter-spacing: 0.02em; }

/* ── 主体两栏 ── */
.emp-main { flex: 1; display: flex; gap: 14px; min-height: 0; }

/* 部门导航：无边框浮层列，选中项滑入亮条 */
.dept-nav {
  width: clamp(170px, 23%, 224px);
  flex-shrink: 0;
  min-width: 0;
  padding: 4px 8px 8px 4px;
  min-height: 0;
  gap: 6px;
  background: transparent;
  border: none;
}
.dept-nav-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  color: var(--text-3);
  font-size: var(--fs-caption);
  font-weight: 500;
  letter-spacing: 0.06em;
  padding: 0 8px 6px;
}
.dept-nav-list { flex: 1; overflow-y: auto; min-height: 0; }
.dept-nav-node { font-size: var(--fs-body); }
.dept-nav-row {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 6px 8px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  color: var(--text-2);
  transition: background var(--transition), color var(--transition);
}
.dept-nav-row:hover { background: var(--bg-hover); color: var(--text-1); }
.dept-nav-row.on {
  background: var(--bg-active);
  color: var(--primary);
  font-weight: 500;
  box-shadow: inset 2px 0 0 var(--primary);
}
.dept-nav-row.on .dept-nav-count { color: var(--primary); background: var(--primary-tint); }
.dept-nav-caret { width: 12px; color: var(--text-3); font-size: 10px; flex-shrink: 0; transition: transform var(--transition); }
.dept-nav-caret-root { visibility: hidden; }
.dept-nav-name {
  flex: 1;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.dept-nav-count {
  font-size: var(--fs-micro);
  font-variant-numeric: tabular-nums;
  color: var(--text-3);
  background: var(--bg-inset);
  padding: 0 6px;
  border-radius: 8px;
  min-width: 18px;
  text-align: center;
}
.dept-nav-empty {
  color: var(--text-3);
  font-size: var(--fs-caption);
  padding: 12px 8px;
  text-align: center;
}

.emp-list-wrap { flex: 1; min-width: 0; overflow-y: auto; padding-right: 2px; }

/* 搜索框：inset 底 + 聚焦陶土 ring */
.emp-search { position: relative; margin-bottom: 12px; flex-shrink: 0; }
.emp-search-inp {
  width: 100%;
  box-sizing: border-box;
  padding: 8px 30px 8px 12px;
  font-size: var(--fs-caption);
  font-family: var(--font-ui);
  color: var(--text-1);
  background: var(--bg-inset);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  outline: none;
  transition: border-color var(--transition), box-shadow var(--transition);
}
.emp-search-inp::placeholder { color: var(--text-3); }
.emp-search-inp:focus {
  border-color: var(--primary);
  box-shadow: 0 0 0 3px var(--primary-tint);
}
.emp-search-clear {
  position: absolute;
  right: 6px; top: 50%;
  transform: translateY(-50%);
  width: 20px; height: 20px;
  border: none; background: none;
  color: var(--text-3);
  cursor: pointer;
  border-radius: var(--radius-sm);
  font-size: var(--fs-micro);
  line-height: 1;
  transition: color var(--transition), background var(--transition);
}
.emp-search-clear:hover, .emp-search-clear:focus-visible { color: var(--text-1); background: var(--bg-hover); }

/* ── 员工卡：档案行（头像列 + 内容列）── */
.emp-list { display: flex; flex-direction: column; gap: 10px; }
.emp-card { padding: 13px 16px; gap: 8px; }
.emp-card-main { display: flex; gap: 12px; min-width: 0; }
.emp-avatar {
  width: 40px; height: 40px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 20px;
  flex-shrink: 0;
  background: var(--primary-tint);
  border-radius: var(--radius);
  user-select: none;
  transition: filter var(--transition), transform var(--transition);
}
.emp-avatar.off { filter: grayscale(0.9); opacity: 0.55; }
.emp-card:hover .emp-avatar { transform: scale(1.06); }
.emp-card-body { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 3px; }
.emp-row1 { display: flex; align-items: center; gap: 8px; min-height: 22px; }
.emp-row-actions { display: flex; align-items: center; gap: 2px; opacity: 0; transition: opacity var(--transition); }
.emp-card:hover .emp-row-actions, .emp-card:focus-within .emp-row-actions { opacity: 1; }
.emp-name {
  color: var(--text-1);
  font-size: var(--fs-body);
  font-weight: 650;
  letter-spacing: 0.01em;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.emp-pos { font-size: var(--fs-caption); color: var(--text-2); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; flex-shrink: 0; }

/* 状态徽标：轻量圆点 + 文字（替代 Arco tag）；点带同色柔光提升辨识 */
.emp-status {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: var(--fs-micro);
  color: var(--text-2);
  flex-shrink: 0;
}
.emp-status::before {
  content: "";
  width: 6px; height: 6px;
  border-radius: 50%;
  background: var(--text-3);
  flex-shrink: 0;
  box-shadow: 0 0 0 2px var(--bg-card);
}
.emp-status.ok::before { background: var(--success); }
.emp-status.ok { color: var(--success); }
.emp-status.warn::before { background: var(--warning); }
.emp-status.warn { color: var(--warning); }
.emp-status.mute { color: var(--text-3); }

/* 组织徽标行：低干扰分隔点统一 3px（与全局状态点语言一致） */
.emp-org-meta { row-gap: 4px; }

.emp-desc {
  font-size: var(--fs-caption);
  color: var(--text-2);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.emp-org-meta {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  font-size: var(--fs-micro);
  color: var(--text-3);
}
.emp-org-chip {
  color: var(--primary);
  background: var(--primary-tint);
  border-radius: 4px;
  padding: 0 6px;
  line-height: 1.7;
}
.emp-org-item { position: relative; padding-left: 9px; }
.emp-org-item + .emp-org-item::before {
  content: "";
  position: absolute;
  left: 0; top: 50%;
  transform: translateY(-50%);
  width: 3px; height: 3px;
  border-radius: 50%;
  background: var(--border);
}
.emp-meta {
  display: flex;
  gap: 10px;
  font-size: var(--fs-micro);
  color: var(--text-3);
  flex-wrap: wrap;
}
.emp-proposals { color: var(--warning); }
.emp-readonly { color: var(--warning); }
.emp-ws {
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  direction: rtl;
  text-align: left;
  font-family: var(--font-mono);
}
.emp-actions {
  display: flex;
  gap: 8px;
  align-items: center;
  padding-left: 52px;   /* 与头像列对齐（40px 头像 + 12px 间距） */
}
.emp-bound {
  font-size: var(--fs-micro);
  color: var(--success);
  letter-spacing: 0.02em;
}
.emp-empty { margin-top: 48px; }

/* ── 表单（编辑弹窗）：分节线代替内嵌边框盒 ── */
.emp-grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.emp-grid3 { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; margin-top: 12px; }
.set-section { margin-bottom: 16px; }
.set-label {
  display: block;
  font-size: var(--fs-caption);
  color: var(--text-2);
  margin-bottom: 6px;
}
.set-hint { font-size: var(--fs-micro); color: var(--text-3); line-height: 1.6; }
.org-section {
  border: none;
  border-top: 1px solid var(--border-weak);
  border-radius: 0;
  padding: 14px 0 2px;
  background: transparent;
}
.org-section-title {
  font-size: var(--fs-caption);
  color: var(--text-1);
  font-weight: 600;
  margin-bottom: 12px;
  display: flex;
  align-items: center;
  gap: 8px;
}
.org-section-title::before {
  content: "";
  width: 3px; height: 12px;
  border-radius: 2px;
  background: var(--primary);
}
.org-hint { margin-top: 10px; line-height: 1.6; }

	/* ── 组织 Tab：主从布局（左部门树管理位 + 右详情列）── */
	.org-main { flex: 1; display: flex; gap: 14px; min-height: 0; }

	/* 左：部门树 */
	.org-tree { width: clamp(210px, 26%, 270px); flex-shrink: 0; min-height: 0; padding: 12px 10px 10px; }
	.org-tree-head {
	  display: flex;
	  align-items: center;
	  gap: 8px;
	  padding: 0 6px 10px;
	  border-bottom: 1px solid var(--border-weak);
	}
	.org-tree-title { color: var(--text-1); font-size: var(--fs-body); font-weight: 600; }
	.org-tree-sub {
	  font-size: var(--fs-micro);
	  font-variant-numeric: tabular-nums;
	  color: var(--text-3);
	  background: var(--bg-inset);
	  padding: 0 6px;
	  border-radius: 8px;
	  line-height: 1.7;
	}
	.org-tree-body { flex: 1; overflow-y: auto; min-height: 0; padding-top: 6px; font-size: var(--fs-body); }
		.org-tree-tip { font-size: var(--fs-micro); color: var(--text-3); }
		.org-tree-empty { padding: 22px 12px; text-align: center; }
	.org-tree-empty-title { color: var(--text-2); font-size: var(--fs-caption); font-weight: 500; }
	.org-tree-empty-hint { color: var(--text-3); font-size: var(--fs-micro); margin-top: 4px; line-height: 1.7; }

		/* 部门树行（DeptNode h() 渲染；干净行 + 右键全量操作，对齐钉钉/企微组织树惯例） */
	.dtree-row {
	  display: flex;
	  align-items: center;
	  gap: 6px;
	  padding: 6px 8px;
	  border-radius: var(--radius-sm);
	  cursor: pointer;
	  color: var(--text-2);
	  user-select: none;
	  transition: background var(--transition), color var(--transition);
	}
	.dtree-row:hover { background: var(--bg-hover); color: var(--text-1); }
	.dtree-row.on {
	  background: var(--bg-active);
	  color: var(--primary);
	  font-weight: 500;
	  box-shadow: inset 2px 0 0 var(--primary);
	}
	.dtree-row.on .dtree-count { color: var(--primary); background: var(--primary-tint); }
	.dtree-row.off .dtree-name { opacity: 0.55; }
	.dtree-caret { width: 12px; color: var(--text-3); font-size: 10px; flex-shrink: 0; cursor: pointer; transition: transform var(--transition); }
	.dtree-caret.leaf { visibility: hidden; }
	.dtree-caret-root { visibility: hidden; }
	.dtree-name { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.dtree-flag {
	  font-size: var(--fs-micro);
	  color: var(--warning);
	  flex-shrink: 0;
	}
	.dtree-count {
	  font-size: var(--fs-micro);
	  font-variant-numeric: tabular-nums;
	  color: var(--text-3);
	  background: var(--bg-inset);
	  padding: 0 6px;
	  border-radius: 8px;
	  min-width: 18px;
	  text-align: center;
	}
		/* 子级缩进 + 引导线（树形可读性，替代纯 padding 盲缩进） */
	.dtree-kids { position: relative; margin-left: 17px; }
	.dtree-kids::before {
	  content: "";
	  position: absolute;
	  left: 0; top: 0; bottom: 6px;
	  width: 1px;
	  background: var(--border-weak);
	}

	/* 右键上下文菜单（Teleport 至 body；浮层单层柔和投影 + ring，对齐 --shadow-overlay 语言） */
	.ctx-menu {
	  position: fixed;
	  z-index: 1000;
	  min-width: 168px;
	  padding: 5px;
	  background: var(--bg-elevated);
	  border: 1px solid var(--border-weak);
	  border-radius: var(--radius);
	  box-shadow: var(--shadow-overlay);
	  animation: ctx-in 120ms var(--ease-out);
	}
	@keyframes ctx-in {
	  from { opacity: 0; transform: scale(0.97) translateY(-2px); }
	  to { opacity: 1; transform: scale(1) translateY(0); }
	}
	.ctx-item {
	  display: block;
	  width: 100%;
	  border: none;
	  background: transparent;
	  color: var(--text-1);
	  font-size: var(--fs-caption);
	  font-family: var(--font-ui);
	  text-align: left;
	  padding: 6px 10px;
	  border-radius: var(--radius-sm);
	  cursor: pointer;
	  transition: background var(--transition), color var(--transition);
	}
	.ctx-item:hover, .ctx-item:focus-visible { background: var(--bg-hover); }
	.ctx-item.danger { color: var(--danger); }
	.ctx-item.danger:hover { background: var(--danger-tint); }
	.ctx-sep { height: 1px; margin: 4px 6px; background: var(--border-weak); }

	/* 右：详情列 */
	.org-detail { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 12px; min-height: 0; position: relative; }

	/* 详情头：部门卡 / 概览卡 */
	.org-deptcard { padding: 14px 16px; gap: 8px; flex-shrink: 0; }
	.org-deptcard-row1 { display: flex; align-items: center; gap: 8px; }
	.org-deptcard-name {
	  font-family: var(--font-display);
	  font-size: var(--fs-display-sm);
	  font-weight: 650;
	  letter-spacing: -0.015em;
	  color: var(--text-1);
	}
	.org-deptcard-meta { display: flex; align-items: center; gap: 18px; flex-wrap: wrap; }
	.org-deptcard-stat {
	  display: inline-flex;
	  align-items: baseline;
	  gap: 6px;
	  font-size: var(--fs-micro);
	  color: var(--text-3);
	  letter-spacing: 0.02em;
	}
	.org-deptcard-stat b {
	  font-family: var(--font-display);
	  font-size: 17px;
	  font-weight: 650;
	  color: var(--text-1);
	  font-variant-numeric: tabular-nums;
	  letter-spacing: -0.02em;
	}
	.org-deptcard-hint { font-size: var(--fs-micro); color: var(--text-3); line-height: 1.6; }

	/* 右侧子页签（岗位/角色）：同 .emp-tabs 胶囊语言缩小版 */
	.org-subtabs {
	  display: flex;
	  gap: 2px;
	  padding: 3px;
	  background: var(--bg-inset);
	  border: 1px solid var(--border-weak);
	  border-radius: var(--radius);
	  width: fit-content;
	  flex-shrink: 0;
	}
	.org-subtabs button {
	  border: none;
	  background: transparent;
	  color: var(--text-2);
	  font-size: var(--fs-caption);
	  font-family: var(--font-ui);
	  padding: 4px 14px;
	  border-radius: calc(var(--radius) - 3px);
	  cursor: pointer;
	  transition: color var(--transition), background var(--transition);
	}
	.org-subtabs button:hover, .org-subtabs button:focus-visible { color: var(--text-1); background: var(--bg-hover); }
	.org-subtabs button.active {
	  color: var(--on-primary);
	  background: var(--primary-strong);
	  box-shadow: 0 1px 4px rgba(201, 100, 66, 0.28);
	}

	/* 岗位/角色卡列表（档案行语言，对齐 .emp-card） */
		.org-detail-list { flex: 1; overflow-y: auto; min-height: 0; display: flex; flex-direction: column; gap: 10px; padding-right: 2px; }
	.org-empty-block { margin-top: 10px; }
	.org-poscard { padding: 12px 16px; gap: 6px; }
	.org-poscard-row1 { display: flex; align-items: center; gap: 8px; min-height: 22px; }
	.org-poscard-name { color: var(--text-1); font-size: var(--fs-body); font-weight: 650; letter-spacing: 0.01em; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.org-poscard-meta { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; row-gap: 4px; }
	.org-poscard-chip {
	  font-size: var(--fs-micro);
	  color: var(--primary);
	  background: var(--primary-tint);
	  border-radius: 4px;
	  padding: 0 6px;
	  line-height: 1.7;
	}
	.org-poscard-desc {
	  font-size: var(--fs-caption);
	  color: var(--text-2);
	  overflow: hidden;
	  text-overflow: ellipsis;
	  white-space: nowrap;
	}
	.org-poscard-none { font-size: var(--fs-micro); color: var(--text-3); }

		/* 徽标原子（部门卡/岗位卡/角色卡共用）：code 芯片 / 范围 / 警示 / 资源入口 */
	.org-item-code, .org-item-level {
	  font-size: var(--fs-micro);
	  font-variant-numeric: tabular-nums;
	  color: var(--text-3);
	  background: var(--bg-inset);
	  padding: 1px 6px;
	  border-radius: 4px;
	  flex-shrink: 0;
	}
	.org-item-scope {
	  font-size: var(--fs-micro);
	  color: var(--success);
	  background: var(--success-tint);
	  padding: 0 6px;
	  border-radius: 4px;
	  line-height: 1.7;
	}
	.org-item-scope.role { color: var(--primary); background: var(--primary-tint); }
	.org-item-scope.warn { color: var(--warning); background: var(--warning-tint); }
	.org-item-warn { font-size: var(--fs-micro); color: var(--warning); }
	.org-item-res {
	  align-self: flex-start;
	  border: none;
	  background: var(--primary-tint);
	  color: var(--primary);
	  font-size: var(--fs-micro);
	  font-family: var(--font-ui);
	  padding: 1px 8px;
	  border-radius: 999px;
	  cursor: pointer;
	  margin-top: 2px;
	  transition: background var(--transition), color var(--transition);
	}
	.org-item-res:hover { background: var(--primary); color: var(--on-primary); }

/* ── 建群弹窗成员选择 ── */
.gsheet-member {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 6px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: background var(--transition);
}
.gsheet-member:hover { background: var(--bg-hover); }
.gcheck {
  width: 18px;
  height: 18px;
  border-radius: 5px;
  border: 1.5px solid var(--border);
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 12px;
  color: var(--on-primary);
  transition: background var(--transition), border-color var(--transition);
}
.gcheck.on { background: var(--primary-strong); border-color: var(--primary-strong); }
.gcheck.on::after { content: '✓'; font-weight: 700; }
.gsheet-avatar { font-size: 16px; }
.gsheet-name { flex: 1; color: var(--text-1); font-size: var(--fs-body); }
.gsheet-dept { font-size: var(--fs-micro); color: var(--text-3); max-width: 200px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
