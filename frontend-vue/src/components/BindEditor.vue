<script setup>
// ═════════════════════════════════════════════════════════════════
// 共用资源绑定编辑器（doc/09-设计方案 §5.1/§5.6 · Phase 3）
//   员工下拉（含岗位显示）/ 岗位下拉 / 分配规则（random | round_robin |
//   least_loaded，后端策略注册表 employee/strategy.go 已落地三策略）/ 清空绑定。
//   保存经 SaveResourceBinding → 内部转发 SaveChannels / UpdateCronJob
//   既有校验（§11-4：不另起炉灶、不新增平行校验）。
// 绑定语义（cron.ValidateBinding / SaveChannels §4.4 同源）：
//   - 员工与岗位二选一起填；选员工自动带出其当前岗位（回填快照，§3）；
//   - 仅绑岗位 = 按分配规则从岗位池选取；
//   - 全空 = 解绑（启用渠道被后端拒绝，cron 允许 = 存量裸跑语义）。
// 数据源：ListEmployees / GetOrg（岗位字典）经 store 缓存 + 组件内拉取。
// ═════════════════════════════════════════════════════════════════
import { computed, ref, watch } from "vue";
import { Message } from "@arco-design/web-vue";
import { bridgeCall } from "../bridge/wails.js";
import { store } from "../stores/app.js";

const props = defineProps({
  // 资源类型：channel | cron | session（与后端 ResourceBinding.Type 对齐）
  resourceType: { type: String, required: true },
  // 资源标识：渠道 type:name 复合 / cron Job ID / 会话 ID
  resourceId: { type: String, required: true },
  // 回显初值（ListResourceBindings 行数据；watch 变化时重置编辑态）
  employeeId: { type: String, default: "" },
  positionId: { type: String, default: "" },
  assignRule: { type: String, default: "" },
  // 紧凑模式（行内/弹窗窄区）
  compact: { type: Boolean, default: false },
});
const emit = defineEmits(["saved"]);

// ── 编辑态 ───────────────────────────────────────────────────────
const empId = ref(props.employeeId || "");
const posId = ref(props.positionId || "");
const rule = ref(props.assignRule || "");
const saving = ref(false);
// 显式清空过岗位（员工清空后岗位快照应一并清除，避免残留陈旧快照）
const posCleared = ref(false);

watch(
  () => [props.resourceId, props.employeeId, props.positionId, props.assignRule],
  () => {
    empId.value = props.employeeId || "";
    posId.value = props.positionId || "";
    rule.value = props.assignRule || "";
    posCleared.value = false;
  }
);

// ── 字典：员工（含岗位名）/ 岗位 ─────────────────────────────────
const employees = computed(() => store.employees || []);
const positions = ref([]);
const posLoaded = ref(false);

async function ensurePositions() {
  if (posLoaded.value) return;
  posLoaded.value = true;
  try {
    const view = await bridgeCall("GetOrg");
    positions.value = (view && view.positions) || [];
  } catch { /* 静默：下拉为空不阻断 */ }
}

// 岗位下可运行员工数（岗位模式下拉提示，§8-2）
function runnableCountOf(posId) {
  return (store.employees || []).filter(
    (e) => e.positionId === posId && e.runnable
  ).length;
}

// ── 分配规则字典（与后端枚举 employee/resolve.go 对齐；单一来源避免三处散写）──
const assignRuleOptions = [
  { label: "随机 random", value: "random" },
  { label: "轮转 round_robin", value: "round_robin" },
  { label: "最闲 least_loaded", value: "least_loaded" },
];
// 展示名（下拉选中回显用；空 = random 默认）。
function ruleLabel(rule) {
  const o = assignRuleOptions.find((x) => x.value === rule);
  return o ? o.label.split(" ")[0] : (rule ? rule : "随机");
}

// 选员工 → 自动带出岗位（快照回填语义，§5.3「选员工自动带出岗位」）
function onEmpChange(v) {
  posCleared.value = false;
  if (v) {
    const e = (store.employees || []).find((x) => x.id === v);
    if (e && e.positionId) posId.value = e.positionId;
  }
}
function onEmpClear() {
  empId.value = "";
  posCleared.value = true; // 清员工连带清岗位快照（用户可改为纯岗位绑定）
  posId.value = "";
}
function onPosChange(v) {
  if (!v) posCleared.value = true;
}

// ── 保存 / 清空 ─────────────────────────────────────────────────
const emitPayload = () => ({
  employeeId: empId.value,
  positionId: posId.value,
  assignRule: rule.value,
});

async function save() {
  const e = String(empId.value || "").trim();
  const p = String(posId.value || "").trim();
  if (!e && !p) {
    Message.warning("请选择数字员工或岗位（启用资源必须至少绑定其一）");
    return false;
  }
  if (e && !p) {
    Message.warning("绑定员工时必须同时指定岗位（保存时自动回填员工当前岗位快照）");
    return false;
  }
  saving.value = true;
  try {
    await bridgeCall("SaveResourceBinding", props.resourceType, props.resourceId, e, p, String(rule.value || ""));
    Message.success("绑定已保存");
    emit("saved", emitPayload());
    return true;
  } catch (err) {
    Message.error("保存失败：" + String(err));
    return false;
  } finally {
    saving.value = false;
  }
}

// 清空绑定 = 解绑回裸跑（启用渠道被 SaveChannels 拒绝；cron 合法 = 存量语义）
async function clearBinding() {
  saving.value = true;
  try {
    await bridgeCall("SaveResourceBinding", props.resourceType, props.resourceId, "", "", "");
    Message.success("绑定已清空");
    empId.value = "";
    posId.value = "";
    rule.value = "";
    posCleared.value = false;
    emit("saved", { employeeId: "", positionId: "", assignRule: "" });
    return true;
  } catch (err) {
    Message.error(String(err));
    return false;
  } finally {
    saving.value = false;
  }
}

// 挂载即加载岗位字典（懒加载一次；失败静默 = 下拉为空不阻断）
ensurePositions();
</script>

<template>
  <div class="bind-editor" :class="{ compact }">
    <div class="be-grid">
      <div>
        <div class="be-label">数字员工</div>
        <a-select
          v-model="empId"
          size="small"
          allow-clear
          placeholder="不指定（按岗位分配）"
          @change="onEmpChange"
          @clear="onEmpClear"
        >
          <a-option v-for="e in employees" :key="e.id" :value="e.id" :disabled="!e.runnable">
            {{ (e.avatar || "🤖") + " " + e.name }}{{ e.runnable ? "" : "（未启用）" }}
          </a-option>
        </a-select>
      </div>
      <div>
        <div class="be-label">岗位</div>
        <a-select
          v-model="posId"
          size="small"
          allow-clear
          placeholder="选择岗位（池内分配）"
          @change="onPosChange"
        >
          <a-option v-for="p in positions" :key="p.id" :value="p.id">
            {{ p.name }}<template v-if="runnableCountOf(p.id)">（{{ runnableCountOf(p.id) }} 人可运行）</template>
          </a-option>
        </a-select>
      </div>
      <div>
        <div class="be-label">分配规则</div>
        <a-select v-model="rule" size="small" allow-clear placeholder="随机（默认）">
          <a-option v-for="o in assignRuleOptions" :key="o.value" :value="o.value">{{ o.label }}</a-option>
        </a-select>
      </div>
    </div>
    <div class="be-hint">
      {{ resourceType === "session"
        ? "会话绑定岗位后，首条消息从岗位池分配员工接管；选员工则直接固定（快照随会话固化，调岗不漂移）。"
        : "选员工自动带出其当前岗位（快照回填）；仅绑岗位 = 按分配规则从岗位池选取。" }}
    </div>
    <div class="be-actions">
      <a-button size="small" type="primary" :loading="saving" @click="save">保存绑定</a-button>
      <a-button size="small" :disabled="saving" @click="clearBinding">清空绑定</a-button>
    </div>
  </div>
</template>

<style scoped>
.bind-editor {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 10px;
  background: var(--bg-inset);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
}
.be-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px;
}
.bind-editor.compact .be-grid { grid-template-columns: 1fr; }
.be-label {
  font-size: var(--fs-micro);
  color: var(--text-2);
  margin-bottom: 5px;
  display: block;
}
.be-hint {
  font-size: var(--fs-micro);
  color: var(--text-3);
  line-height: 1.6;
}
.be-actions {
  display: flex;
  gap: 8px;
}
</style>
