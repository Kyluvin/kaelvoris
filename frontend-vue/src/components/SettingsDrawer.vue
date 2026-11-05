<script setup>
// ═══...（设置面板重设计 2026-10/11）═══
//   布局：860px 抽屉 · 左侧目录导航 + 右侧滚动内容区
//   页面：通用 / 模型 / MCP / 消息渠道 / ACP / 远程节点 / 关于
//   视觉：全部引用 tokens.css 令牌（Claude 暖色系）
//   渠道：状态概览 + 渠道卡片（品牌徽章/三态/企微三选一模式分段）
// ═════════════════════════════════════════════════════════════════════
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from "vue";
import { Message } from "@arco-design/web-vue";
	import {
	  	  IconDesktop, IconThunderbolt, IconTool, IconCommon, IconCode,
	  	  IconCloud, IconInfoCircle, IconSafe, IconFolder, IconPlus,
	  	  IconApps, IconUpload, IconEdit, IconDownload, IconCompass, IconCopy,
		} from "@arco-design/web-vue/es/icon";
import { bridgeCall, onEvent } from "../bridge/wails.js";
import { store, loadProjects } from "../stores/app.js";
import BindEditor from "./BindEditor.vue";

const visible = computed({
  get: () => store.settingsVisible,
  set: (v) => (store.settingsVisible = v),
});

	const cfg = ref(null);
	// 沙箱配置宽松访问（wailsjs 生成物类型可能不含 sandbox 新字段，不手改生成物）。
	const sandboxCfg = computed(() => (cfg.value && typeof cfg.value.sandbox === "object" && cfg.value.sandbox !== null) ? cfg.value.sandbox : {});
	const sandboxOn = computed(() => sandboxCfg.value.mode !== "off");
	function setSandboxOn(on) {
	  if (!cfg.value) return;
	  const sb = (typeof cfg.value.sandbox === "object" && cfg.value.sandbox !== null) ? cfg.value.sandbox : {};
	  sb.mode = on ? "job" : "off";
	  cfg.value = { ...cfg.value, sandbox: sb };
	}
	function setSandbox(key, val) {
	  if (!cfg.value) return;
	  const sb = (typeof cfg.value.sandbox === "object" && cfg.value.sandbox !== null) ? cfg.value.sandbox : {};
	  sb[key] = val == null ? 0 : Number(val);
	  cfg.value = { ...cfg.value, sandbox: sb };
	}
const saving = ref(false);
const page = computed(() => store.settingsPage);

// ── ACP agents 配置（列表编辑 + 测试连接）──
const acpAgents = ref([]);
const acpTesting = ref(""); // 正在测试的 agent 名
const acpPresets = [
  { name: "Claude Code", command: "npx -y @zed-industries/claude-code-acp" },
  { name: "Gemini CLI", command: "npx -y @google/gemini-cli --experimental-acp" },
  { name: "Codex", command: "codex app-server", protocol: "codex" },
];

async function loadACP() {
  try { acpAgents.value = (await bridgeCall("ListACPAgents")) || []; }
  catch { acpAgents.value = []; }
}
function addACP() {
  acpAgents.value.push({ name: "", command: "", args: [], env: {}, enabled: true });
}
function removeACP(i) {
  acpAgents.value.splice(i, 1);
}

// ── ACP 配置导出（复制为 JSON，粘贴到其他设备/软件即可直接使用）──
// 序列化单条 agent，与快速添加 parseACPJSON 的导入格式同构（空字段省略）
function acpExportJSON(ag) {
  return JSON.stringify({
    name: ag.name,
    command: ag.command,
    ...(ag.protocol ? { protocol: ag.protocol } : {}),
    ...(ag.args && ag.args.length ? { args: ag.args } : {}),
    ...(ag.env && Object.keys(ag.env).length ? { env: ag.env } : {}),
    ...(ag.cwd ? { cwd: ag.cwd } : {}),
    enabled: ag.enabled !== false,
  }, null, 2);
}
// 复制单条 agent 配置到剪贴板
async function copyACPJSON(ag) {
  try {
    await navigator.clipboard.writeText(acpExportJSON(ag));
    Message.success(`已复制「${ag.name || "未命名"}」配置，粘贴到其他设备/软件的 ACP 快速添加即可使用`);
  } catch { Message.error("复制失败"); }
}
// 复制全部 agent 配置（{ agents: [...] } 包装格式，快速添加同样可识别）
async function copyAllACPJSON() {
  const list = acpAgents.value.filter((a) => a.name && a.command);
  if (!list.length) { Message.warning("暂无可复制的 ACP agent 配置"); return; }
  try {
    await navigator.clipboard.writeText(JSON.stringify({ agents: list.map(acpExportJSON) }, null, 2));
    Message.success(`已复制 ${list.length} 个 ACP agent 配置，粘贴到其他设备/软件即可使用`);
  } catch { Message.error("复制失败"); }
}

// ── ACP 快速添加（弹窗 + JSON 识别，与 MCP 快速添加同构）──
// 状态：visible 控制弹窗显隐；text 用户输入的 JSON 文本；loading 识别/合并进行中
const acpQuickAdd = ref({ visible: false, text: "", loading: false });
// 弹窗提示文案（JS 侧模板字符串定义：含 JSON 双引号示例，避免写在 HTML 属性里的转义问题）
const acpPlaceholder = `在此粘贴 ACP agent 配置 JSON，支持以下格式：

示例 1（单 agent）：
{
  "name": "Claude Code",
  "command": "npx -y @zed-industries/claude-code-acp"
}

示例 2（数组，多个 agent）：
[
  { "name": "Claude Code", "command": "npx -y @zed-industries/claude-code-acp" },
  { "name": "Gemini CLI", "command": "npx -y @google/gemini-cli --experimental-acp" }
]

示例 3（包装格式）：
{
  "agents": {
    "Codex": { "command": "codex app-server", "protocol": "codex" }
  }
}

字段说明：
- name*     agent 名称（必填，对话输入区选择器显示名）
- command*  启动命令（必填，如 npx -y … / codex app-server）
- protocol  协议变体（如 codex，普通 ACP 留空）
- args      附加参数（数组或空格分隔字符串）
- env       环境变量对象
- cwd       默认工作目录（可选，空 = 会话工作区）
- enabled   是否启用（默认 true）`;

/**
 * 打开 ACP 快速添加弹窗（每次重置输入）
 */
function openACPQuickAdd() {
  acpQuickAdd.value = { visible: true, text: "", loading: false };
}

/**
 * 预置快捷填充：把内置 preset 序列化为 JSON 写入输入框（可继续手改）
 * @param {{name:string,command:string,protocol?:string}} p 预置项
 */
function fillACPPresetJSON(p) {
  acpQuickAdd.value.text = JSON.stringify(p, null, 2);
}

/**
 * 解析用户输入的 JSON 文本，识别多种 ACP 配置格式，归一化为 acpAgents 数组项
 * 支持格式：
 *   1) 单对象  { name, command, args, env, ... }
 *   2) 数组    [ {...}, {...} ]
 *   3) 包装    { agents: { <name>: {...} } } 或 { agents: [ {...} ] }
 * @param {string} input 用户输入的 JSON 文本
 * @returns {Array} 归一化后的 ACP agent 配置数组
 */
function parseACPJSON(input) {
  const trimmed = String(input || "").trim();
  if (!trimmed) throw new Error("输入为空");
  let parsed;
  try {
    parsed = JSON.parse(trimmed);
  } catch (e) {
    throw new Error("JSON 解析失败：" + (e && e.message ? e.message : String(e)));
  }

  let list = [];
  // 包装格式：{ agents: {...} }（对象映射）或 { agents: [...] }（数组）
  const wrapper = parsed && parsed.agents;
  if (wrapper && typeof wrapper === "object") {
    if (Array.isArray(wrapper)) {
      for (const item of wrapper) list.push(normalizeACPEntry((item || {}).name || "", item || {}));
      return list;
    }
    for (const [name, cfg] of Object.entries(wrapper)) {
      list.push(normalizeACPEntry(name, cfg || {}));
    }
    return list;
  }
  // 数组
  if (Array.isArray(parsed)) {
    for (const item of parsed) {
      const it = item || {};
      list.push(normalizeACPEntry(it.name || "", it));
    }
    return list;
  }
  // 单对象
  if (parsed && typeof parsed === "object") {
    list.push(normalizeACPEntry(parsed.name || "", parsed));
    return list;
  }
  throw new Error("无法识别的 JSON 格式，期望对象/数组或 { agents: {...} }");
}

/**
 * 将单个 ACP 配置对象归一化为 acpAgents 数组项格式
 * 字段兼容：command/cmd、args(数组或空格分隔字符串)、env、protocol、cwd、enabled/disabled
 * @param {string} name agent 名（来自外层 key 或对象自身 name）
 * @param {object} cfg 单个 ACP 配置对象
 * @returns {object} 归一化后的配置项
 */
function normalizeACPEntry(name, cfg) {
  let args = cfg.args;
  if (typeof args === "string") args = args.split(/\s+/).filter(Boolean);
  else if (!Array.isArray(args)) args = [];
  return {
    name: String(name || cfg.name || "").trim(),
    command: cfg.command || cfg.cmd || "",
    protocol: String(cfg.protocol || "").trim(),
    args,
    env: cfg.env && typeof cfg.env === "object" ? cfg.env : {},
    cwd: String(cfg.cwd || "").trim(),
    enabled: cfg.enabled !== false && cfg.disabled !== true,
  };
}

/**
 * 确认快速添加：解析 JSON 并合并到 acpAgents 数组
 * 同名跳过；解析失败提示但不关闭弹窗
 * @param {(close:boolean)=>void} done Arco Modal before-ok 回调，done(true) 关闭，done(false) 保持
 */
async function confirmACPQuickAdd(done) {
  acpQuickAdd.value.loading = true;
  try {
    let list;
    try {
      list = parseACPJSON(acpQuickAdd.value.text);
    } catch (e) {
      Message.error(String((e && e.message) || e));
      done(false);
      return;
    }
    if (!list.length) {
      Message.warning("未识别到任何 ACP agent 配置");
      done(false);
      return;
    }
    const invalid = list.filter((a) => !a.name || !a.command);
    if (invalid.length) {
      Message.warning(`有 ${invalid.length} 个配置缺少 name 或 command 字段，请补充后重试`);
      done(false);
      return;
    }
    // 合并到现有列表（同名跳过）
    const existing = new Set(acpAgents.value.map((a) => a.name));
    let added = 0;
    const skipped = [];
    for (const ag of list) {
      if (existing.has(ag.name)) {
        skipped.push(ag.name);
        continue;
      }
      acpAgents.value.push(ag);
      existing.add(ag.name);
      added++;
    }
    if (added > 0) {
      Message.success(`已添加 ${added} 个 ACP agent，请点击「保存 ACP」使其生效${skipped.length ? `（同名跳过：${skipped.join("、")}）` : ""}`);
      done(true);
    } else {
      Message.warning("无新增条目（与现有同名）");
      done(false);
    }
  } finally {
    acpQuickAdd.value.loading = false;
  }
}
async function saveACP() {
  try {
    await bridgeCall("SaveACPAgents", JSON.parse(JSON.stringify(acpAgents.value)));
    Message.success("ACP agent 配置已保存");
  } catch (e) {
    Message.error("保存失败：" + String(e));
  }
}
async function testACP(ag) {
  acpTesting.value = ag.name || "?";
  try {
    const r = await bridgeCall("TestACPAgent", JSON.parse(JSON.stringify(ag)));
    if (r && r.ok) {
      Message.success(`连接成功：${r.agentName || ag.name}${r.version ? " v" + r.version : ""}（${r.durationMS}ms）`);
    } else {
      Message.error(`连接失败：${(r && r.error) || "未知错误"}`);
    }
  } catch (e) {
    Message.error("测试失败：" + String(e));
  } finally {
    acpTesting.value = "";
  }
}

// ── MCP 服务器配置 ──
const mcpServers = ref([]);
const mcpTesting = ref("");
const mcpSaving = ref(false);
const mcpPublishing = ref("");
async function loadMCP() {
  try { mcpServers.value = (await bridgeCall("ListMCPServers")) || []; }
  catch { mcpServers.value = []; }
}
function addMCP() {
  mcpServers.value.push({ name: "", transport: "stdio", command: "", args: [], url: "", env: {}, headers: {}, enabled: true });
}
// ── MCP 快速添加（弹窗 + JSON 识别）──
// 状态：visible 控制弹窗显隐；text 用户输入的 JSON 文本；loading 识别/合并进行中
const mcpQuickAdd = ref({ visible: false, text: "", loading: false });

/**
 * 打开 MCP 快速添加弹窗（每次重置输入）
 */
function openMCPQuickAdd() {
  mcpQuickAdd.value = { visible: true, text: "", loading: false };
}

/**
 * 解析用户输入的 JSON 文本，识别多种 MCP 配置格式，归一化为 mcpServers 数组项
 * 支持格式：
 *   1) 单对象  { name, command, args, env, url, ... }
 *   2) 数组    [ {...}, {...} ]
 *   3) Claude Desktop / mcp 标准：{ mcpServers: { <name>: { command, args, ... } } }
 *   4) 兼容别名：servers / cmd / serverUrl / disabled
 * @param {string} input 用户输入的 JSON 文本
 * @returns {Array} 归一化后的 MCP 服务器配置数组
 */
function parseMCPJSON(input) {
  const trimmed = String(input || "").trim();
  if (!trimmed) throw new Error("输入为空");
  let parsed;
  try {
    parsed = JSON.parse(trimmed);
  } catch (e) {
    throw new Error("JSON 解析失败：" + (e && e.message ? e.message : String(e)));
  }

  let list = [];
  // Claude Desktop / mcp 标准格式：{ mcpServers: { <name>: {...} } } 或 { servers: {...} }
  const wrapper = parsed && (parsed.mcpServers || parsed.servers);
  if (wrapper && typeof wrapper === "object" && !Array.isArray(wrapper)) {
    for (const [name, cfg] of Object.entries(wrapper)) {
      list.push(normalizeMCPEntry(name, cfg || {}));
    }
    return list;
  }
  // 数组
  if (Array.isArray(parsed)) {
    for (const item of parsed) {
      const it = item || {};
      list.push(normalizeMCPEntry(it.name || "", it));
    }
    return list;
  }
  // 单对象
  if (parsed && typeof parsed === "object") {
    list.push(normalizeMCPEntry(parsed.name || "", parsed));
    return list;
  }
  throw new Error("无法识别的 JSON 格式，期望对象/数组或 { mcpServers: {...} }");
}

/**
 * 将单个 MCP 配置对象归一化为 mcpServers 数组项格式
 * 字段兼容：command/cmd、args(数组或空格分隔字符串)、transport、url/serverUrl、env、headers、enabled/disabled
 * 自动判定 transport：有 url 默认 http（除非显式 sse）；有 command 默认 stdio
 * @param {string} name 服务器名（来自外层 key 或对象自身 name）
 * @param {object} cfg 单个 MCP 配置对象
 * @returns {object} 归一化后的配置项
 */
function normalizeMCPEntry(name, cfg) {
  const url = cfg.url || cfg.serverUrl || "";
  const transport = cfg.transport || (url ? "http" : "stdio");
  const cmd = cfg.command || cfg.cmd || "";
  // args 兼容数组 / 字符串空格分隔
  let args = cfg.args;
  if (typeof args === "string") args = args.split(/\s+/).filter(Boolean);
  else if (!Array.isArray(args)) args = [];
  const env = cfg.env && typeof cfg.env === "object" ? cfg.env : {};
  const headers = cfg.headers && typeof cfg.headers === "object" ? cfg.headers : {};
  const enabled = cfg.enabled !== false && cfg.disabled !== true;
  return {
    name: String(name || cfg.name || "").trim(),
    transport,
    command: cmd,
    args,
    url,
    env,
    headers,
    enabled,
  };
}

/**
 * 确认快速添加：解析 JSON 并合并到 mcpServers 数组
 * 同名跳过；解析失败提示但不关闭弹窗
 * @param {(close:boolean)=>void} done Arco Modal before-ok 回调，done(true) 关闭，done(false) 保持
 */
async function confirmMCPQuickAdd(done) {
  mcpQuickAdd.value.loading = true;
  try {
    let list;
    try {
      list = parseMCPJSON(mcpQuickAdd.value.text);
    } catch (e) {
      Message.error(String((e && e.message) || e));
      done(false);
      return;
    }
    if (!list.length) {
      Message.warning("未识别到任何 MCP 服务器配置");
      done(false);
      return;
    }
    const invalid = list.filter((s) => !s.name);
    if (invalid.length) {
      Message.warning(`有 ${invalid.length} 个配置缺少 name 字段，请补充后重试`);
      done(false);
      return;
    }
    // 合并到现有列表（同名跳过）
    const existing = new Set(mcpServers.value.map((s) => s.name));
    let added = 0;
    const skipped = [];
    for (const sv of list) {
      if (existing.has(sv.name)) {
        skipped.push(sv.name);
        continue;
      }
      mcpServers.value.push(sv);
      existing.add(sv.name);
      added++;
    }
    if (added > 0) {
      Message.success(`已添加 ${added} 个 MCP 服务器，请点击「保存 MCP」使其下一任务生效${skipped.length ? `（同名跳过：${skipped.join("、")}）` : ""}`);
      done(true);
    } else {
      Message.warning("无新增条目（与现有同名）");
      done(false);
    }
  } finally {
    mcpQuickAdd.value.loading = false;
  }
}
function removeMCP(i) {
  mcpServers.value.splice(i, 1);
}
async function saveMCP() {
  mcpSaving.value = true;
  try {
    await bridgeCall("SaveMCPServers", JSON.parse(JSON.stringify(mcpServers.value)));
    Message.success("MCP 配置已保存（下一任务生效）");
  } catch (e) {
    Message.error("保存失败：" + String(e));
  } finally {
    mcpSaving.value = false;
  }
}
async function publishMCP(sv) {
	  if (!sv.name || !sv.name.trim()) {
	    Message.warning("请先填写服务器名称再发布");
	    return;
	  }
	  mcpPublishing.value = sv.name;
	  try {
	    await bridgeCall("PublishMCPToMarket", { name: sv.name });
	    Message.success(`已发布到市场：${sv.name}`);
	  } catch (e) {
	    Message.error("发布失败：" + String(e));
	  } finally {
	    mcpPublishing.value = "";
	  }
	}
	async function testMCP(sv) {
		  mcpTesting.value = sv.name || "?";
	  try {
	    const r = await bridgeCall("TestMCPServer", JSON.parse(JSON.stringify(sv)));
	    if (r && r.ok) {
	      Message.success(`连接成功：${sv.name}（${r.tools} 个工具）`);
	    } else {
	      Message.error(`连接失败：${(r && r.error) || "未知错误"}`);
	    }
	  } catch (e) {
	    Message.error("测试失败：" + String(e));
	  } finally {
	    mcpTesting.value = "";
	  }
	}

	// ── 粘贴导入（快速添加：后端 ImportMCPServers 三格式解析 + 合并落盘）──
	const mcpImportVisible = ref(false);
	const mcpImportText = ref("");
	const mcpImportLoading = ref(false);
	async function importMCP() {
	  if (!mcpImportText.value.trim()) {
	    Message.warning("请先粘贴 MCP 配置 JSON");
	    return;
	  }
	  mcpImportLoading.value = true;
	  try {
	    // 后端解析+合并落盘后返回完整列表，直接刷新面板（同名覆盖、异名保留）。
	    const list = await bridgeCall("ImportMCPServers", mcpImportText.value);
	    mcpServers.value = list || [];
	    mcpImportVisible.value = false;
	    mcpImportText.value = "";
	    Message.success("MCP 配置已导入（下一任务生效）");
	  } catch (e) {
	    Message.error("导入失败：" + String(e));
	  } finally {
	    mcpImportLoading.value = false;
	  }
	}

	// ── 工具列表查看（后端 MCPServerTools：握手 → tools/list，含名称/描述/参数）──
	const toolsVisible = ref(false);
	const toolsLoading = ref(false);
	const toolsResult = ref(null);
	const toolsFor = ref("");
	async function showTools(sv) {
	  toolsFor.value = sv.name || sv.url || "?";
	  toolsResult.value = null;
	  toolsVisible.value = true;
	  toolsLoading.value = true;
	  try {
	    toolsResult.value = await bridgeCall("MCPServerTools", JSON.parse(JSON.stringify(sv)));
	  } catch (e) {
	    toolsResult.value = { name: toolsFor.value, ok: false, error: String(e) };
	  } finally {
	    toolsLoading.value = false;
	  }
	}
	// 参数 schema 摘要（一行文案：text* (string), limit (number)）。
	function schemaSummary(t) {
	  if (!t || !t.inputSchema) return "";
	  try {
	    const s = typeof t.inputSchema === "string" ? JSON.parse(t.inputSchema) : t.inputSchema;
	    const props = s.properties || {};
	    const req = s.required || [];
	    const parts = Object.entries(props).map(([k, v]) => k + (req.includes(k) ? "*" : "") + "（" + (v.type || "any") + "）");
	    return parts.length ? parts.join("，") : "无参数";
	  } catch { return ""; }
	}

// ── 插件（MCP 插件 · 能力扩展）──
const plugins = ref([]);
const plugLoading = ref(false);
const plugInstallDir = ref("");

async function loadPlugins() {
  try { plugins.value = (await bridgeCall("ListPlugins")) || []; }
  catch { plugins.value = []; }
}
async function installPlugin() {
  if (!plugInstallDir.value.trim()) {
    Message.warning("请填写插件目录路径（含 plugin.json 的目录）");
    return;
  }
  try {
    const r = await bridgeCall("InstallPluginFromDir", { dir: plugInstallDir.value.trim() });
    Message.success(`插件已安装：${r.name || r.id}`);
    plugInstallDir.value = "";
    await loadPlugins();
  } catch (e) {
    Message.error("安装失败：" + String(e));
  }
}
async function uninstallPlugin(p) {
  try {
    await bridgeCall("UninstallPlugin", p.id);
    Message.success(`已卸载：${p.name || p.id}`);
    await loadPlugins();
  } catch (e) {
    Message.error("卸载失败：" + String(e));
  }
}
async function togglePlugin(p, enabled) {
  try {
    await bridgeCall("TogglePlugin", p.id, enabled);
    p.enabled = enabled;
    Message.success(`${p.name || p.id} 已${enabled ? "启用" : "停用"}（下一任务生效）`);
  } catch (e) {
    Message.error("切换失败：" + String(e));
  }
}

// ── 消息渠道配置 ──
	const channels = ref([]);
	const localIP = ref(""); // 本机局域网出口 IP（GetLocalIP：群接收回调地址拼接展示）
const channelStatus = ref([]); // 运行时状态（ListChannelStatus：enabled/online）
const empOptions = ref([]);
const chSaving = ref(false);
const channelTypes = [
  { type: "lark", label: "飞书", hint: "长连接，免公网回调" },
  { type: "wecom", label: "企业微信", hint: "智能机器人 WS / 应用消息" },
  { type: "wechat", label: "个人微信", hint: "预留位，开发中" },
	  { type: "im", label: "通用 IM", hint: "自定义平台 · 公共号 / 群机器人 / 群消息接收" },
	];

	// 通用 IM 连接模式：公共号双向 / 「群机器人 + 群消息接收」组合。
	// 群机器人（Webhook 推送）与群消息接收（HTTP 端点）同区块配置，可任意
	// 配置其一或全配置（全配置时群消息进入 → 任务回复经群机器人推回群）。
	const imModes = [
	  { key: "pub", label: "公共号", desc: "回调接收 · 双向交互" },
	  { key: "group", label: "群机器人 + 群消息接收", desc: "推送 + 接收 · 任配其一或全配" },
	];

// 企微连接模式：三选一（互斥接入方式，切换只改展示不清字段）
const wecomModes = [
  { key: "ws", label: "智能机器人", desc: "WS 长连接 · 免公网回调" },
  { key: "webhook", label: "群机器人", desc: "Webhook 推送 · 仅单向" },
  { key: "app", label: "应用消息", desc: "双向交互 · 本地回调" },
];

// 状态缓存：type + name → ChannelStatus（后端按名称匹配）
const chStateMap = computed(() => {
  const m = {};
  for (const s of channelStatus.value) m[s.type + "\u0000" + (s.name || "")] = s;
  return m;
});
// 渠道三态：未启用 / 在线 / 已启用未连接
function chState(c) {
  const st = chStateMap.value[c.type + "\u0000" + (c.name || "")];
  if (!c.enabled) return { key: "off", label: "未启用", hint: "保存并开启开关后接入" };
  if (st && st.online) return { key: "on", label: "已连接", hint: "通道在线，可收发消息" };
  return { key: "idle", label: "待连接", hint: "已启用，等待建立连接" };
}
// 概览统计
const chStats = computed(() => {
  const all = channels.value;
  let online = 0, unbound = 0;
  for (const c of all) {
    if (chState(c).key === "on") online += 1;
    if (c.enabled && !c.employeeId) unbound += 1;
  }
  return { total: all.length, enabled: all.filter((c) => c.enabled).length, online, unbound };
});

// 企微当前模式：优先跟随用户显式选择（_wm），否则按已填字段推断
function wecomModeOf(c) {
  if (c._wm) return c._wm;
  if (c.botId || c.botSecret) return "ws";
  if (c.webhookKey) return "webhook";
  if (c.corpId || c.corpSecret || c.agentId || c.token || c.encodingAesKey) return "app";
  return "ws";
}
	function switchWecomMode(c, key) { c._wm = key; }

				// 通用 IM 当前模式：优先跟随用户显式选择（_bm），否则按已填字段推断。
				// 群机器人与群消息接收已合并为同一组合模式（两者可任意配置其一或全配置，
				// 运行时共存见 channel_im.go Start/sendText）；仅公共号凭据走 pub。
				function imModeOf(c) {
				  if (c._bm) return c._bm;
				  if (c.publicId || c.pubSecret || c.eid) return "pub";
				  return "group";
				}
				function switchIMMode(c, key) { c._bm = key; }

				// ── IM 公网中继回调 URL 实时生成（c.remoteRelay 开启时展示）──
				// 服务中心地址取「远程节点」面板 remoteCfg.serverURL；指纹 = 渠道五元组
				// （Name/PublicID/EID/WebhookToken/GroupHookIDs，\x00 连接）SHA1 前 12 位，
				// 与节点侧 channel_im.go / 服务端 internal/remote/im_relay.go 的
				// imRelayChannelFP 逐字节一致 —— 跨进程契约（三处互指，改一处必改三处）。
				function sha1HexFP(u8) {
				  const ml = u8.length;
				  const buf = new Uint8Array(((ml + 8) >> 6 << 6) + 64);
				  buf.set(u8); buf[ml] = 0x80;
				  const dv = new DataView(buf.buffer);
				  dv.setUint32(buf.length - 8, Math.floor(ml / 0x20000000)); // 64 位大端长度高 32 位（消息 < 2^29 字节恒 0）
				  dv.setUint32(buf.length - 4, (ml << 3) >>> 0);
				  let h0 = 0x67452301, h1 = 0xEFCDAB89, h2 = 0x98BADCFE, h3 = 0x10325476, h4 = 0xC3D2E1F0;
				  const rol = (x, n) => (x << n) | (x >>> (32 - n));
				  const w = new Int32Array(80);
				  for (let off = 0; off < buf.length; off += 64) {
				    for (let i = 0; i < 16; i++) w[i] = dv.getInt32(off + i * 4);
				    for (let i = 16; i < 80; i++) w[i] = rol(w[i - 3] ^ w[i - 8] ^ w[i - 14] ^ w[i - 16], 1);
				    let a = h0, b = h1, c = h2, d = h3, e = h4;
				    for (let i = 0; i < 80; i++) {
				      let f, k;
				      if (i < 20) { f = (b & c) | (~b & d); k = 0x5A827999; }
				      else if (i < 40) { f = b ^ c ^ d; k = 0x6ED9EBA1; }
				      else if (i < 60) { f = (b & c) | (b & d) | (c & d); k = 0x8F1BBCDC; }
				      else { f = b ^ c ^ d; k = 0xCA62C1D6; }
				      const t = (rol(a, 5) + f + e + k + w[i]) | 0;
				      e = d; d = c; c = rol(b, 30); b = a; a = t;
				    }
				    h0 = (h0 + a) | 0; h1 = (h1 + b) | 0; h2 = (h2 + c) | 0; h3 = (h3 + d) | 0; h4 = (h4 + e) | 0;
				  }
				  return [h0, h1, h2, h3, h4].map((x) => (x >>> 0).toString(16).padStart(8, "0")).join("").slice(0, 12);
				}
				function imRelayFP(c) {
				  return sha1HexFP(new TextEncoder().encode(
				    [c.name || "", c.publicId || "", c.eid || "", c.webhookToken || "", c.groupHookIds || ""].join("\x00")
				  ));
				}
				// 服务中心 base（远程节点面板 serverURL，去尾斜杠归一为可直接拼接形态）
				function imRelayBase() {
				  return String((remoteCfg.value && remoteCfg.value.serverURL) || "").trim().replace(/\/+$/, "");
				}
					// 群 hook 列表（去空去重保序，对齐节点侧 parseGroupHooks）
					function imRelayHooks(c) {
					  const seen = new Set(); const out = [];
					  for (const h of String(c.groupHookIds || "").split(",")) {
					    const t = h.trim();
					    if (t && !seen.has(t)) { seen.add(t); out.push(t); }
				  }
					  return out;
					}
					// 指纹是否已在服务端注册：仅当「已保存（持久化）且启用」的渠道快照里
					// 存在同指纹渠道时成立。修复：URL 按编辑框草稿实时计算并展示，渠道
					// 未保存/未启用时用户把 URL 配到 IM 平台必然回调 404（指纹从未注册）。
					const savedChannels = ref([]);
					function refreshSavedSnapshot() {
					  savedChannels.value = JSON.parse(JSON.stringify(channels.value));
					}
					function imRelayRegistered(c) {
					  return savedChannels.value.some((s) => s.type === "im" && s.enabled && imRelayFP(s) === imRelayFP(c));
					}

			// IM 入参映射增删行（groupInMap: {目标字段: 源路径}；_imNewRow 输入缓冲）
			function imInMapRows(c) {
			  const rows = Object.entries(c.groupInMap || {});
			  return rows.length ? rows : [["", ""]];
			}
			function imInMapKey(c, i) { return imInMapRows(c)[i]?.[0] ?? ""; }
			function imInMapVal(c, i) { return imInMapRows(c)[i]?.[1] ?? ""; }
			function imInMapSet(c, i, k, v) {
			  const entries = Object.entries(c.groupInMap || {});
			  while (entries.length <= i) entries.push(["", ""]);
			  entries[i] = [k ?? entries[i][0], v ?? entries[i][1]];
			  const obj = {};
			  for (const [ek, ev] of entries) { if (String(ek).trim()) obj[String(ek).trim()] = ev; }
			  c.groupInMap = obj;
			}
			function addImInMapRow(c) {
			  const entries = Object.entries(c.groupInMap || {});
			  entries.push(["", ""]);
			  const obj = {};
			  for (const [ek, ev] of entries) obj[ek] = ev;
			  obj["_new" + entries.length] = ""; // 占位空行（保存时空键自动剔除）
			  c.groupInMap = obj;
			}
			function delImInMapRow(c, key) {
			  const obj = { ...(c.groupInMap || {}) };
			  delete obj[key];
			  c.groupInMap = obj;
			}

	async function loadChannels() {
  try { channels.value = (await bridgeCall("ListChannels")) || []; }
  catch { channels.value = []; }
		  try { localIP.value = (await bridgeCall("GetLocalIP")) || ""; } catch { localIP.value = ""; }
		  try { channelStatus.value = (await bridgeCall("ListChannelStatus")) || []; }
  catch { channelStatus.value = []; }
	  try { empOptions.value = (await bridgeCall("ListEmployees")) || []; }
	  catch { empOptions.value = []; }
	  refreshSavedSnapshot(); // 已保存态快照（中继 URL 注册与否的判定基准）
	}
	function addChannel(type) {
	  channels.value.push({
	    type, name: "", enabled: false, employeeId: "",
	    appId: "", appSecret: "", webhookKey: "", botId: "", botSecret: "",
	    corpId: "", agentId: "", corpSecret: "", token: "", encodingAesKey: "",
		    callbackPort: type === "im" ? 9881 : 9880, deviceId: "", allowedUsers: "",
				    apiBase: "", webhookUrl: "", publicId: "", pubSecret: "", eid: "", webhookToken: "", groupHookIds: "", groupInMap: null, outTemplate: "", remoteRelay: false,
	  });
	}
	function removeChannel(i) {
	  channels.value.splice(i, 1);
	}
	// 渠道绑定编辑器保存后：刷新渠道列表与运行态（后端已整表校验 + 热重载）
	async function onChannelBindSaved() {
	  await loadChannels();
	}

	async function saveChannels() {
	  chSaving.value = true;
	  try {
	    // 剔除 UI 态临时字段（_wm/_bm 连接模式选择）与入参映射空键占位行，避免随配置持久化
	    const clean = channels.value.map((c) => {
	      const { _wm, _bm, ...rest } = c;
	      if (rest.groupInMap) {
		      const obj = {};
		      for (const [k, v] of Object.entries(rest.groupInMap)) {
			      if (String(k).trim() && !String(k).startsWith("_new")) obj[k] = v;
			      }
			      rest.groupInMap = Object.keys(obj).length ? obj : null;
		      }
		      return rest;
	    });
	    await bridgeCall("SaveChannels", JSON.parse(JSON.stringify(clean)));
	    Message.success("渠道配置已保存（启用中的渠道热重载生效）");
	    await loadChannels(); // 刷新运行时状态（保存后热重载）
	  } catch (e) {
	    Message.error("保存失败：" + String(e));
	  } finally {
	    chSaving.value = false;
	  }
	}

// ── 服务中心连接（合并节点配置与账号登录）──
const remoteCfg = ref(null);
const remoteSaving = ref(false);
const logging = ref(false);

async function loadRemote() {
  try { remoteCfg.value = await bridgeCall("GetRemoteConfig"); }
  // 后端 omitempty 字段未返回时补缺省，避免 v-model 绑定 undefined
  catch { remoteCfg.value = { enabled: false, serverURL: "", user: "", pass: "", nodeID: "", name: "", loggedIn: false }; }
}
function remotePlain() {
  return JSON.parse(JSON.stringify({
    enabled: false, serverURL: "", user: "", pass: "", nodeID: "", name: "", loggedIn: false,
    ...remoteCfg.value,
  }));
}
async function saveRemote() {
  remoteSaving.value = true;
  try {
    await bridgeCall("SaveRemoteConfig", remotePlain()); // 仅保存数据，不连接
    Message.success("已保存（未连接）");
  } catch (e) {
    Message.error("保存失败：" + String(e));
  } finally {
    remoteSaving.value = false;
  }
}
async function remoteLogin() {
  const c = remoteCfg.value || {};
  if (!String(c.serverURL || "").trim() || !String(c.user || "").trim() || !String(c.pass || "").trim()) {
    Message.warning("请先填写服务中心地址、登录账号与密码");
    return;
  }
	  logging.value = true;
	  try {
	    await bridgeCall("SaveRemoteConfig", remotePlain()); // 先落盘再登录，保证服务端地址与账号一致
	    const st = await bridgeCall("RemoteLogin", String(c.user || ""), String(c.pass || "")); // Go 端签名 RemoteLogin(user, pass)，逐参传递
	    if (st) accState.value = st; // 登录态即时生效（kaelvoris:remote_account 推送为冗余兜底）
	    await loadRemote();
	    Message.success("已登录并连接服务中心");
	  } catch (e) {
	    Message.error(String(e));
	  } finally {
	    logging.value = false;
	  }
	}
	async function remoteLogout() {
	  try {
	    const st = await bridgeCall("RemoteLogout");
	    if (st) accState.value = st;
	    await loadRemote();
	    Message.success("已退出登录，节点已下线");
	  } catch (e) { Message.error(String(e)); }
	}

// ── 关于 / 版本 / 升级（version_panel.go：GetVersionInfo / SaveUpdateConfig / CheckUpdate / ApplyUpdate）──
const ver = ref(null);
const upChecking = ref(false);
const upApplying = ref(false);
const upResult = ref(null);

async function loadVersion() {
  try { ver.value = await bridgeCall("GetVersionInfo"); }
  catch { ver.value = { number: "", changelog: [], autoCheck: true, source: "" }; }
}
async function saveUpdateCfg() {
  if (!ver.value) return;
  try {
    await bridgeCall("SaveUpdateConfig", ver.value.source || "", !!ver.value.autoCheck);
    Message.success("升级源配置已保存");
  } catch (e) {
    Message.error("保存失败：" + String(e));
  }
}
async function checkUpdate() {
  upChecking.value = true;
  upResult.value = null;
  try {
    const r = await bridgeCall("CheckUpdate");
    upResult.value = r || {};
    if (r && r.update) Message.success(`发现新版本 v${r.latest}`);
    else Message.info("已是最新版本");
  } catch (e) {
    upResult.value = { error: String(e) };
  } finally {
    upChecking.value = false;
  }
}
async function applyUpdate() {
  upApplying.value = true;
  try {
    await bridgeCall("ApplyUpdate", (upResult.value && upResult.value.latest) || "");
    Message.success("升级包校验通过，应用即将重启…");
  } catch (e) {
    Message.error("升级失败：" + String(e));
  } finally {
    upApplying.value = false;
  }
}

// 主题：dark（默认）/ light，持久化到 localStorage
const theme = ref(localStorage.getItem("kaelvoris-theme") || "dark");
watch(theme, (t) => {
  document.documentElement.classList.toggle("light", t === "light");
  document.documentElement.classList.toggle("dark", t === "dark");
  localStorage.setItem("kaelvoris-theme", t);
}, { immediate: true });

// ── 失焦系统通知（共用开关 blurNotifyDisabled，取反语义：false = 通知开；
// 权限状态 + 授权入口）──
import { ensureNotifyPermission } from "../utils/desktopNotify.js";
const notifyPermission = ref(typeof Notification !== "undefined" ? Notification.permission : "denied");
async function grantNotify() {
  notifyPermission.value = await ensureNotifyPermission();
}
// 开关打开（=通知开启）时顺带请求权限（弹一次系统授权框；已授权则无感）。
async function onBlurNotifyToggle(disabled) {
  if (!disabled && notifyPermission.value !== "granted") await grantNotify();
}

	// 渠道运行状态实时订阅（后端 pushChannelStatus → kaelvoris:channels）
	onMounted(() => onEvent("kaelvoris:channels", (list) => {
	  if (Array.isArray(list)) channelStatus.value = list;
	}));
	// onUnmounted 无需显式清理：onEvent 为追加式注册，且本组件常驻挂载。

	// 账号登录态实时订阅（后端 pushRemoteAccount → kaelvoris:remote_account）。
	// 修复「登录后按钮不变/无退出按钮」：remoteCfg（GetRemoteConfig 返回的
	// RemoteConfig）没有 loggedIn 字段，此前退出按钮 v-if="remoteCfg.loggedIn"
	// 恒为假。账号态真实来源是 RemoteAccountState（GetRemoteAccount），
	// 这里以 accState 合并渲染（登录成功/退出/自动重登均由后端推送）。
	const accState = ref(null);
	onMounted(() => onEvent("kaelvoris:remote_account", (st) => {
	  if (st) accState.value = st;
	}));
	// 远程页打开时拉一次账号态（抽屉常驻，事件错过也有兜底）。
	watch(page, (p) => { if (p === "remote") bridgeCall("GetRemoteAccount").then((st) => { accState.value = st; }).catch(() => {}); }, { immediate: true });
	const remoteLoggedIn = computed(() => !!(accState.value && accState.value.loggedIn));
	const remoteAccountName = computed(() => (accState.value && (accState.value.displayName || accState.value.user)) || "");

watch(visible, async (v) => {
  if (v && !cfg.value) {
    try {
	const c = await bridgeCall("GetConfig");
	      // 后端 omitempty 字段未返回时补缺省，避免 v-model 绑定 undefined
	      cfg.value = {
	        contextWindow: 0, autoCondensePercent: 90, condenseDisabled: false,
        taskSummary: { enabled: false },
	        costPerMPrompt: 0, costPerMCompletion: 0,
	        maxParallel: 10, blurNotifyDisabled: false,
		        imageGenBaseURL: "", imageGenModel: "", imageGenAPIKey: "",
	        videoGenBaseURL: "", videoGenModel: "", videoGenAPIKey: "", videoGenMode: "", videoGenSize: "",
	        speechBaseURL: "", speechModel: "", speechAPIKey: "", speechVoice: "",
	        smtpHost: "", smtpPort: 465, smtpUser: "", smtpPass: "", smtpFrom: "",
	        sandbox: { mode: "job", memoryLimitMB: 0, cpuPercent: 0, maxProcesses: 0 },
	        notionAPIKey: "", browserPath: "", internalBrowser: false, browserDisabled: false,
	        ...c,
	      };
    } catch { cfg.value = {}; }
  }
	  if (v) {
	    loadACP();
	    loadMCP();
	    loadChannels();
	    loadRemote();
	    loadVersion();
	    loadPlugins();
	    loadPrompts();
	  }
});

async function save() {
  saving.value = true;
  try {
    await bridgeCall("SaveConfig", JSON.parse(JSON.stringify(cfg.value)));
    // 同步全局 config 缓存（失焦通知开关等实时读取 store.config 的消费方立即生效）。
    store.config = JSON.parse(JSON.stringify(cfg.value));
    Message.success("配置已保存");
  } catch (e) {
    Message.error("保存失败：" + String(e));
  } finally {
    saving.value = false;
  }
}

async function pickWorkspace() {
  try {
    // OpenProject：选完目录后端直接切换为当前工作区并写入最近项目列表
    // （旧实现只调 PickWorkspace 丢弃返回值，保存前不生效且下拉无项）。
    const dir = await bridgeCall("OpenProject");
    if (dir) {
      cfg.value = await bridgeCall("GetConfig");
      await loadProjects();
    }
  } catch { /* 用户取消 */ }
}

async function switchProject(p) {
  try {
    await bridgeCall("SwitchProject", p.path || p);
    cfg.value = await bridgeCall("GetConfig");
    await loadProjects();
    Message.success("工作区已切换");
  } catch (e) { Message.error(String(e)); }
}

		// 常用模型快捷填充（点击写入输入框，仍可自由编辑）
			const modelPresets = ["deepseek-chat", "gpt-4o", "claude-sonnet-4-5", "gemini-2.5-pro"];
			// 功能型模型预设（图像/视频/语音；点击写入输入框，仍可自由编辑）
			const imageModelPresets = ["gpt-image-1", "dall-e-3", "gemini-2.5-flash-image", "agnes-image-2.5-flash"];
			const videoModelPresets = ["sora-2", "veo-3", "kling-v2", "agnes-video-2.5-flash"];
			// Agnes 系网关视频必填参数（mode 必填、size 仅支持 720P；Sora 兼容网关留空）
			const videoModePresets = ["", "text", "keyframe", "reference"];
			const videoSizePresets = ["", "720P", "1280x720", "720x1280"];
			const speechModelPresets = ["tts-1", "tts-1-hd", "gpt-4o-mini-tts"];

		// ── 模型配置条目化（同一类型多个 + 启停开关；后端 Config.ModelProfiles）──
		// 每类第一个未停用条目由后端投影到平铺字段（baseURL/apiKey/model 等）
		// 生效；前端不再直接编辑平铺字段，全部经条目视图操作。
		const modelGroups = [
		  { kind: "chat",   title: "对话模型", desc: "主循环引擎（OpenAI Chat Completions 兼容）；多个条目时靠前的未停用条目生效。", modelPh: "gpt-4o / deepseek-chat / …",   presets: modelPresets },
		  { kind: "image",  title: "图像生成", desc: "generate_image 工具端点；留空不启用。",                          modelPh: "gpt-image-1 / dall-e-3 / …",    presets: imageModelPresets },
		  { kind: "video",  title: "视频生成", desc: "generate_video 工具端点；mode/size 仍为下方全局参数（视频区块单独设置）。", modelPh: "sora-2 / veo-3 / …", presets: videoModelPresets },
		  { kind: "speech", title: "语音合成", desc: "generate_speech / 语音转文字工具端点；音色 voice 仍为全局参数。",  modelPh: "tts-1 / gpt-4o-mini-tts",         presets: speechModelPresets },
		];
		// 空条目工厂（新添加时用；kind 决定归属类）
		const newProfile = (kind) => ({ kind, name: "", baseURL: "", apiKey: "", model: "", disabled: false });
		// 某类条目列表（懒创建：首次读取时若该类无条目，从旧平铺字段迁移一条）。
		// 迁移只发生在内存视图层，落盘仍由「保存设置」走 SaveConfig。
		function profilesOf(kind) {
		  const arr = cfg.value.modelProfiles || (cfg.value.modelProfiles = []);
		  if (!arr.some((p) => p.kind === kind)) {
		    const flat = {
		      chat:   [cfg.value.baseURL, cfg.value.model, cfg.value.apiKey],
		      image:  [cfg.value.imageGenBaseURL, cfg.value.imageGenModel, cfg.value.imageGenAPIKey],
		      video:  [cfg.value.videoGenBaseURL, cfg.value.videoGenModel, cfg.value.videoGenAPIKey],
		      speech: [cfg.value.speechBaseURL, cfg.value.speechModel, cfg.value.speechAPIKey],
		    }[kind];
		    if (flat && (String(flat[0] || "").trim() || String(flat[1] || "").trim())) {
		      arr.push({ kind, name: "", baseURL: flat[0] || "", model: flat[1] || "", apiKey: flat[2] || "", disabled: false });
		    }
		  }
		  return arr.filter((p) => p.kind === kind);
		}
		// 该条目是否为当前生效配置（同类第一个未停用条目）
		function isActiveProfile(kind, p) {
		  const list = profilesOf(kind).filter((x) => !x.disabled);
		  return list.length > 0 && list[0] === p;
		}
		function addProfile(kind) { cfg.value.modelProfiles.push(newProfile(kind)); }
		function removeProfile(kind, idx) {
		  const list = profilesOf(kind);
		  const target = list[idx];
		  cfg.value.modelProfiles = cfg.value.modelProfiles.filter((p) => p !== target);
		}
		function moveProfile(kind, idx, dir) {
		  const arr = cfg.value.modelProfiles;
		  const list = profilesOf(kind);
		  const gi = arr.indexOf(list[idx]);
		  const gj = arr.indexOf(list[idx + dir]);
		  if (gi < 0 || gj < 0) return;
		  [arr[gi], arr[gj]] = [arr[gj], arr[gi]];
		}

	// ── 提示词设置（promptconfig 单源：~/.kaelvoris/prompts.json，内存缓存 + 落盘）──
	const promptCfg = ref(null);
	const promptSaving = ref(false);
	// 内置默认提示词（GetPromptDefaults：与引擎实际使用的内置文本同源），
	// key → 默认文本；单编辑框下方可展开只读查看 + 一键填入编辑。
	const promptDefaults = ref({});
	const showDefault = ref(false);        // 「内置默认」只读区展开态（跟随当前选中段）
	const activePromptKey = ref("system"); // 当前编辑的提示词段 key（页面上方下拉选择）
const promptSegments = [
  { key: "system", label: "系统提示词", desc: "追加在系统提示词末尾的高优先级指令，全局生效（每个任务注入）" },
  { key: "summarize", label: "历史压缩摘要", desc: "会话上下文压缩时的摘要指令（上下文接近上限时触发）" },
  { key: "taskSummary", label: "会话总结", desc: "任务结束后自动生成会话总结的指令" },
  { key: "selfReview", label: "任务级复盘", desc: "每次任务结束自学习复盘沉淀经验卡的指令" },
  { key: "hourlyReview", label: "每小时复盘", desc: "每小时全量会话复盘的指令" },
  { key: "roadmapReview", label: "规划板 AI 审核", desc: "规划板提案 AI 审核的补充判据（审核准则之后追加）" },
  { key: "selfCheckTask", label: "定时自检任务", desc: "定时自检会话的任务提示词（内置模板创建的任务，下一次触发即生效）" },
  { key: "roadmapSystem", label: "自主开发会话", desc: "自主迭代规划板开发会话的系统提示词（每次拉起开发会话时前置注入）" },
  { key: "autonomousDev", label: "自主开发守则", desc: "系统提示词中的自主开发循环纪律段（长任务/无人值守守则，非空时整段替换内置默认）" },
];
 // 当前选中段对象（供描述文案 / 操作按钮取值；布局：上方下拉选中 → 下方唯一编辑框带出该段内容）
 const activePromptSeg = computed(() => promptSegments.find((s) => s.key === activePromptKey.value) || promptSegments[0]);
 // 编辑框显示值 = 实际生效文本：配置非空显示配置，为空显示内置默认（系统提示词完整文本等）。
 // 保存规则：若编辑值 == 该段生效文本且原本配置为空（未做任何修改），落盘空串（不把内置默认固化成自定义配置）；
 // 否则落盘编辑值。
 const activePromptDefault = computed(() => promptDefaults.value[activePromptKey.value] || "");
 const activePromptCfgEmpty = computed(() => !String(promptCfg.value?.[activePromptKey.value] || ""));
 const activePromptDisplay = computed(() =>
   String(promptCfg.value?.[activePromptKey.value] || "") || activePromptDefault.value
 );
 // 编辑框 ↔ 配置双向绑定：带出生效文本，写回时区分「未修改的默认文本」与「用户修改」
 function setPromptDisplay(v) {
   const text = String(v ?? "");
   if (activePromptCfgEmpty.value && text === activePromptDefault.value) {
     // 仍是内置默认原文（未修改）：配置保持空串
     promptCfg.value[activePromptKey.value] = "";
     return;
   }
   promptCfg.value[activePromptKey.value] = text;
 }
 async function loadPrompts() {
	  const loadCfg = bridgeCall("GetPromptConfig")
	    .then((c) => (promptCfg.value = { ...c }))
	    .catch(() => (promptCfg.value = {}));
	  const loadDefs = bridgeCall("GetPromptDefaults")
	    .then((list) => {
	      const m = {};
	      for (const d of list || []) m[d.key] = d.default || "";
	      promptDefaults.value = m;
	    })
	    .catch(() => (promptDefaults.value = {}));
	  await Promise.all([loadCfg, loadDefs]);
	  for (const s of promptSegments) {
	    if (promptCfg.value[s.key] == null) promptCfg.value[s.key] = "";
	  }
	}
async function savePrompts() {
  promptSaving.value = true;
  try {
    await bridgeCall("SavePromptConfig", JSON.parse(JSON.stringify(promptCfg.value)));
    Message.success("提示词已保存（内存缓存即时更新，下一任务生效）");
  } catch (e) {
    Message.error("提示词保存失败：" + String(e));
  } finally {
    promptSaving.value = false;
  }
}
	// 清空当前段：恢复为「留空 = 使用内置默认」（仍需保存生效）。
	function resetActivePrompt() {
	  promptCfg.value[activePromptKey.value] = "";
	  Message.success(`已清空「${activePromptSeg.value.label}」，保存后使用内置默认`);
	}
	// 填入内置默认：把当前段内置默认文本写入编辑框（覆盖当前配置，仍需保存生效）。
	function fillDefaultPrompt() {
	  const d = promptDefaults.value[activePromptKey.value] || "";
	  if (!d) { Message.warning("该段没有内置默认提示词"); return; }
	  promptCfg.value[activePromptKey.value] = d;
	  Message.success("已填入内置默认提示词，保存后生效");
	}

const pages = [
  { key: "general", label: "通用", desc: "工作区 · 外观 · 行为", icon: IconDesktop },
	  { key: "model", label: "模型", desc: "API 连接与上下文", icon: IconThunderbolt },
	  { key: "prompts", label: "提示词", desc: "系统 · 功能提示词", icon: IconEdit },
	  { key: "browser", label: "浏览器", desc: "内嵌显示 · 自动化", icon: IconCompass },
	  { key: "mcp", label: "MCP", desc: "外部工具服务器", icon: IconTool },
	  { key: "plugins", label: "插件", desc: "MCP 插件 · 能力扩展", icon: IconApps },
	  { key: "databases", label: "数据源", desc: "数据库表 · SQL 节点", icon: IconCommon },
		  { key: "channels", label: "消息渠道", desc: "飞书 · 企微 · 通用 IM", icon: IconCommon },
  { key: "acp", label: "ACP", desc: "外部编码 Agent", icon: IconCode },
  { key: "remote", label: "远程节点", desc: "多机组网协同", icon: IconCloud },
  { key: "version", label: "关于", desc: "版本 · 更新日志", icon: IconInfoCircle },
];

		const chTypeBadge = { lark: "飞书", wecom: "企微", wechat: "微信", im: "IM" };

// ═══ 左导航分组（codex 设置页套路：条目多时分组降低扫读成本）═══
const pageGroups = [
  { title: "基础", keys: ["general", "model", "prompts"] },
	  { title: "扩展", keys: ["browser", "mcp", "plugins", "databases"] },
  { title: "连接", keys: ["channels", "acp", "remote"] },
  { title: "系统", keys: ["version"] },
];
const groupOf = (key) => (pageGroups.find((g) => g.keys.includes(key)) || {}).title || "";

// ═══ 未保存修改 dirty 指示（编辑即标记 · 保存后清除）═══
// 深 watch 主配置对象；独立保存页数据由 bridge 异步灌入，watch(visible/page)
// 置 dirtyWatchReady=false 并 nextTick 后放行，避免「打开面板就亮点」。
const dirty = ref(false);
let dirtyWatchReady = false;
function markDirty() { if (dirtyWatchReady) dirty.value = true; }
watch(cfg, markDirty, { deep: true });
watch(promptCfg, markDirty, { deep: true });
watch(remoteCfg, markDirty, { deep: true });
watch(ver, markDirty, { deep: true });
watch([acpAgents, mcpServers, channels, plugins], markDirty, { deep: true });
watch([visible, page], () => {
  dirtyWatchReady = false;
  dirty.value = false;
  nextTick(() => { dirtyWatchReady = true; });
}, { immediate: true });
watch(saving, (s, os) => { if (os && !s) dirty.value = false; });
watch(chSaving, (s, os) => { if (os && !s) dirty.value = false; });
watch(mcpSaving, (s, os) => { if (os && !s) dirty.value = false; });
watch(promptSaving, (s, os) => { if (os && !s) dirty.value = false; });
watch(remoteSaving, (s, os) => { if (os && !s) dirty.value = false; });

// ═══ 数据源（外部 MySQL 连接；工作流 SQL 自定义节点直连执行）═══
const dsList = ref([]);          // [{id, name, host, port, user, password, dbName, jdbcURL}]
const dsLoading = ref(false);
const dsSaving = ref(false);
const dsTesting = ref(false);
const dsShowForm = ref(false);
const dsForm = ref({ id: "", name: "", host: "", port: 3306, user: "", password: "", dbName: "", tableName: "" });

// 拉取连接列表（打开页面时）
async function loadDatasources() {
  dsLoading.value = true;
  try { dsList.value = (await bridgeCall("CozeListDatasources")) || []; }
  catch (e) { dsList.value = []; Message.error("读取数据源失败：" + String(e)); }
  dsLoading.value = false;
}
function dsNewForm() {
  dsForm.value = { id: "", name: "", host: "", port: 3306, user: "", password: "", dbName: "", tableName: "" };
  dsShowForm.value = true;
}
function dsEditForm(d) {
  dsForm.value = { id: d.id, name: d.name, host: d.host, port: d.port || 3306, user: d.user, password: d.password, dbName: d.dbName, tableName: d.tableName || "" };
  dsShowForm.value = true;
}
// 测试连接（不保存）
async function testDatasource() {
  if (!dsForm.value.host || !dsForm.value.dbName) { Message.error("请填写主机与库名"); return; }
  dsTesting.value = true;
  try {
    await bridgeCall("CozeTestDatasource", JSON.parse(JSON.stringify(dsForm.value)));
    Message.success("连接成功");
  } catch (e) { Message.error(String(e)); }
  finally { dsTesting.value = false; }
}
// 保存（后端先测连接，失败不落盘）
async function saveDatasource() {
  if (!dsForm.value.name.trim()) { Message.error("请填写连接名称"); return; }
  if (!dsForm.value.host || !dsForm.value.dbName) { Message.error("请填写主机与库名"); return; }
  dsSaving.value = true;
  try {
    await bridgeCall("CozeSaveDatasource", JSON.parse(JSON.stringify(dsForm.value)));
    Message.success(`数据源「${dsForm.value.name.trim()}」已保存`);
    dsShowForm.value = false;
    await loadDatasources();
  } catch (e) { Message.error(String(e)); }
  finally { dsSaving.value = false; }
}
// 删除连接（二次确认）
async function removeDatasource(d) {
  if (!window.confirm(`确认删除数据源「${d.name}」（${d.dbName}@${d.host}）？`)) return;
  try {
    await bridgeCall("CozeDeleteDatasource", d.id);
    Message.success(`数据源「${d.name}」已删除`);
    await loadDatasources();
  } catch (e) { Message.error("删除失败：" + String(e)); }
}
// 打开数据源页时拉列表
watch(visible, (v) => { if (v && page.value === "databases") loadDatasources(); });
watch(page, (p) => { if (p === "databases") loadDatasources(); });
</script>

<template>
  <a-drawer
    v-model:visible="visible"
    :width="860"
    :footer="true"
    unmount-on-close
    class="settings-drawer"
  >
    <template #title>设置</template>

    <div class="settings-layout">
      <!-- 左：目录导航 -->
      <nav class="settings-nav">
        <template v-for="(grp, gi) in pageGroups" :key="grp.title">
          <div class="settings-nav-group" :class="{ first: gi === 0 }">{{ grp.title }}</div>
          <button
            v-for="pg in pages.filter((p) => grp.keys.includes(p.key))"
            :key="pg.key"
            class="settings-nav-item"
            :class="{ active: page === pg.key }"
            @click="store.settingsPage = pg.key"
          >
            <span class="settings-nav-icochip"><component :is="pg.icon" class="settings-nav-icon" /></span>
            <span class="settings-nav-text">
              <span class="settings-nav-label">{{ pg.label }}</span>
              <span class="settings-nav-desc">{{ pg.desc }}</span>
            </span>
          </button>
        </template>
        <div class="settings-nav-foot">Kaelvoris · 工程控制台</div>
      </nav>

      <!-- 右：内容区 -->
      <div class="settings-body">
        <Transition name="setpage" mode="out-in">
        <div :key="page" class="settings-content">
        <!-- ══════════ 通用 ══════════ -->
        <template v-if="page === 'general' && cfg">
          <div class="page-head">
            <div class="page-title">通用</div>
            <div class="page-desc">工作区、外观与任务执行行为。修改后点击抽屉右下角「保存设置」生效。</div>
          </div>

          <section class="set-block">
            <div class="set-block-head">
              <div class="set-block-title">工作区</div>
              <div class="set-block-desc">任务执行、文件读写与 Git 操作的根目录。</div>
            </div>
            <div class="set-card">
              <div class="ws-path" :title="cfg.workspace">
                <IconFolder class="ws-icon" />
                <span class="ws-text">{{ cfg.workspace || "未设置工作区" }}</span>
              </div>
              <div class="btn-toolbar" style="margin-top: 10px">
                <a-button size="small" @click="pickWorkspace">选择目录…</a-button>
              </div>
              <div v-if="store.projects && store.projects.length > 1" class="ws-recent">
                <div class="set-field-label">最近项目</div>
                <div class="chip-row">
                  <button
                    v-for="p in store.projects"
                    :key="p.path"
                    class="pick-chip"
                    :class="{ active: p.current }"
                    :title="p.path"
                    @click="switchProject(p)"
                  >{{ p.path.split(/[\\/]/).filter(Boolean).pop() || p.path }}</button>
                </div>
              </div>
            </div>
          </section>

          <section class="set-block">
            <div class="set-block-head">
              <div class="set-block-title">外观</div>
              <div class="set-block-desc">界面主题，即时生效并持久化到本机。</div>
            </div>
            <div class="theme-cards">
              <button class="theme-card" :class="{ active: theme === 'dark' }" @click="theme = 'dark'">
                <span class="theme-swatch dk"><span class="b1"></span><span class="b2"></span></span>
                <span class="theme-name">深色<span class="theme-default">默认</span></span>
              </button>
              <button class="theme-card" :class="{ active: theme === 'light' }" @click="theme = 'light'">
                <span class="theme-swatch lt"><span class="b1"></span><span class="b2"></span></span>
                <span class="theme-name">浅色</span>
              </button>
            </div>
          </section>

          <section class="set-block">
            <div class="set-block-head">
              <div class="set-block-title">任务执行</div>
              <div class="set-block-desc">工具调用的审批策略。红线约束（敏感凭据文件写入）始终人工确认。</div>
            </div>
            <div class="set-card">
              <div class="set-row">
                <div class="set-row-main">
                  <div class="set-row-label">自动审批工具调用</div>
                  <div class="set-row-hint">跳过普通工具审批卡；危险命令仍强制人工确认</div>
                </div>
                <a-switch v-model="cfg.autoApprove" />
              </div>
	              <div class="set-row">
	                <div class="set-row-main">
	                  <div class="set-row-label"><IconSafe class="row-warn" /> 危险操作自动确认（YOLO）</div>
	                  <div class="set-row-hint">删除 / 关机等危险命令不再弹审批卡，自动选择最优方案执行并留痕。敏感凭据文件（.env / .git 等）写入不受此开关影响。</div>
	                </div>
	                <a-switch v-model="cfg.dangerAutoApprove" />
	              </div>
	              <div class="set-row">
	                <div class="set-row-main">
	                  <div class="set-row-label">命令沙箱（Job Object 进程树围栏）</div>
	                  <div class="set-row-hint">Job Object 进程树围栏：超时/结束时整树终止、可限资源；不隔离文件与网络访问。关闭后仅按既有方式终止直接子进程，孙进程可能泄漏。</div>
	                </div>
	                <a-switch :model-value="sandboxOn" @update:model-value="setSandboxOn" />
	              </div>
	              <div v-if="sandboxOn" class="set-grid2" style="margin-top: 8px">
	                <div>
	                  <div class="set-field-label">内存上限 MB（0 = 不限）</div>
	                  <a-input-number :model-value="sandboxCfg.memoryLimitMB" :min="0" :step="256" size="small" style="width: 100%" @update:model-value="v => setSandbox('memoryLimitMB', v)" />
	                </div>
	                <div>
	                  <div class="set-field-label">CPU 配额 %（10-100）</div>
	                  <a-input-number :model-value="sandboxCfg.cpuPercent" :min="0" :max="100" :step="10" size="small" style="width: 100%" @update:model-value="v => setSandbox('cpuPercent', v)" />
	                </div>
	                <div>
	                  <div class="set-field-label">最大进程数（0 = 不限）</div>
	                  <a-input-number :model-value="sandboxCfg.maxProcesses" :min="0" :step="10" size="small" style="width: 100%" @update:model-value="v => setSandbox('maxProcesses', v)" />
	                </div>
	              </div>
	            </div>
	          </section>

	          <section class="set-block">
	            <div class="set-block-head">
	              <div class="set-block-title">失焦系统通知</div>
	              <div class="set-block-desc">窗口失焦时经系统通知提醒关键事件（定时任务失败、回复完成等共用开关）。</div>
	            </div>
	            <div class="set-card">
	              <div class="set-row">
	                <div class="set-row-main">
	                  <div class="set-row-label">定时任务失败时通知</div>
	                  <div class="set-row-hint">仅任务失败且窗口失焦时弹系统通知，成功不弹；关闭后不弹（共用开关，回复完成通知后续同源）
	                    <a v-if="notifyPermission !== 'granted'" @click.prevent="grantNotify" style="color: var(--primary); cursor: pointer">（点击授权）</a>
	                    <span v-else style="color: var(--success, #00b42a)">（已授权）</span>
	                  </div>
	                </div>
	                <a-switch v-model="cfg.blurNotifyDisabled" @change="onBlurNotifyToggle" />
	              </div>
	            </div>
	          </section>

	          <section class="set-block">
	            <div class="set-block-head">
	              <div class="set-block-title">并行会话</div>
	              <div class="set-block-desc">PC 端可同时并行处理的主任务会话数；超过上限的会话任务自动排队，不阻塞界面。</div>
	            </div>
	            <div class="set-card">
	              <div class="set-row">
	                <div class="set-row-main">
	                  <div class="set-row-label">最大并行会话数</div>
	                  <div class="set-row-hint">默认 10；调高后排队中的会话自动开始运行，调低后运行中的任务不中断，新任务动态排队</div>
	                </div>
	                <a-input-number v-model="cfg.maxParallel" :min="1" :max="64" :step="1" size="small" style="width: 160px" />
	              </div>
	            </div>
	          </section>

	          <section class="set-block">
	            <div class="set-block-head">
	              <div class="set-block-title">成本统计</div>
              <div class="set-block-desc">每百万 token 的美元单价，用于会话用量折算（0 = 仅统计 token 不计价）。</div>
            </div>
            <div class="set-card">
              <div class="set-grid2">
                <div>
                  <div class="set-field-label">输入单价 $/M token</div>
                  <a-input-number v-model="cfg.costPerMPrompt" :min="0" :step="0.1" style="width: 100%" size="small" placeholder="0" />
                </div>
                <div>
                  <div class="set-field-label">输出单价 $/M token</div>
                  <a-input-number v-model="cfg.costPerMCompletion" :min="0" :step="0.1" style="width: 100%" size="small" placeholder="0" />
                </div>
              </div>
            </div>
          </section>

	        </template>

		        <!-- ══════════ 模型 ══════════ -->
	        <template v-else-if="page === 'model' && cfg">
	          <div class="page-head">
	            <div class="page-title">模型</div>
	            <div class="page-desc">OpenAI 兼容端点。同一类型可添加多个配置条目，开关控制是否参与；每类靠前的未停用条目为生效配置。修改后点击抽屉右下角「保存设置」生效。</div>
	          </div>

	          <section
	            v-for="g in modelGroups"
	            :key="g.kind"
	            class="set-block"
	          >
	            <div class="set-block-head">
	              <div class="set-block-title">{{ g.title }}</div>
	              <div class="set-block-desc">{{ g.desc }}</div>
	            </div>
	            <div class="set-card">
	              <div
	                v-for="(p, idx) in profilesOf(g.kind)"
	                :key="idx"
	                class="mp-item"
	                :class="{ 'mp-disabled': p.disabled }"
	              >
	                <div class="mp-head">
	                  <a-switch
	                    :model-value="!p.disabled"
	                    size="small"
	                    @change="(v) => (p.disabled = !v)"
	                  />
	                  <span class="mp-badge" :class="{ on: isActiveProfile(g.kind, p) }">{{ isActiveProfile(g.kind, p) ? '生效中' : (p.name || p.model || '未命名') }}</span>
	                  <div class="mp-ops">
	                    <a-link :disabled="idx === 0" @click="moveProfile(g.kind, idx, -1)">上移</a-link>
	                    <a-link :disabled="idx === profilesOf(g.kind).length - 1" @click="moveProfile(g.kind, idx, 1)">下移</a-link>
	                    <a-link status="danger" @click="removeProfile(g.kind, idx)">删除</a-link>
	                  </div>
	                </div>
	                <div class="set-grid2" style="margin-top: 8px">
	                  <div>
	                    <div class="set-field-label">备注名</div>
	                    <a-input v-model="p.name" placeholder="选填，如 生产 / 备用" size="small" :disabled="p.disabled" />
	                  </div>
	                  <div>
	                    <div class="set-field-label">模型</div>
	                    <a-input v-model="p.model" :placeholder="g.modelPh" size="small" class="mono-input" :disabled="p.disabled" />
	                  </div>
	                </div>
	                <div class="set-grid2" style="margin-top: 8px">
	                  <div>
	                    <div class="set-field-label">API Base URL</div>
	                    <a-input v-model="p.baseURL" placeholder="https://api.example.com/v1" size="small" class="mono-input" :disabled="p.disabled" />
	                  </div>
	                  <div>
	                    <div class="set-field-label">API Key</div>
	                    <a-input-password v-model="p.apiKey" placeholder="sk-…（空 = 不带认证头）" size="small" :disabled="p.disabled" />
	                  </div>
	                </div>
	                <div v-if="g.presets.length" class="chip-row" style="margin-top: 8px">
	                  <button
	                    v-for="m in g.presets"
	                    :key="m"
	                    class="pick-chip"
	                    :class="{ active: p.model === m }"
	                    :disabled="p.disabled"
	                    @click="p.model = m"
	                  >{{ m }}</button>
	                </div>
	              </div>
	              <a-button type="text" size="small" style="margin-top: 8px" @click="addProfile(g.kind)">+ 添加{{ g.title }}配置</a-button>
	            </div>
	          </section>
	
	          <section class="set-block">
	            <div class="set-block-head">
	              <div class="set-block-title">上下文治理</div>
	              <div class="set-block-desc">接近窗口上限时自动压缩历史消息，保证长任务不丢上下文。</div>
	            </div>
	            <div class="set-card">
	<div class="set-field">
	                <div class="set-field-label">上下文窗口（token）</div>
	                <a-input-number v-model="cfg.contextWindow" :min="0" :step="1000" style="width: 100%" size="small" />
	                <div class="set-field-hint">0 = 不启用自动压缩（窗口 × 0.9 触发）</div>
	              </div>
	              <div class="set-row" style="margin-top: 10px">
	                <div class="set-row-main">
	                  <div class="set-row-label">关闭上下文自动压缩</div>
	                  <div class="set-row-hint">开启后超阈值不再摘要/截断历史消息（保留完整历史，不推荐长会话开启；413 兑底裁剪仍生效）</div>
	                </div>
	                <a-switch v-model="cfg.condenseDisabled" />
	              </div>
	            </div>
	          </section>

	          		          <section class="set-block">
		            <div class="set-block-head">
		              <div class="set-block-title">会话总结</div>
		              <div class="set-block-desc">任务结束后自动生成「会话总结」（目标/状态/产出/下一步），展示在聊天流与会话卡。</div>
		            </div>
		            <div class="set-card">
		              <div class="set-row">
		                <div class="set-row-main">
		                  <div class="set-row-label">任务结束生成会话总结</div>
		                  <div class="set-row-hint">每轮任务结束后台调用一次 LLM 生成总结，落会话存储；关闭后不生成</div>
		                </div>
		                <a-switch v-model="cfg.taskSummary.enabled" />
		              </div>
		            </div>
		          </section>

<!-- 图像/视频/语音的 BaseURL/模型/Key 已并入上方条目视图（图像生成/视频生成/语音合成组）；此处仅留各类全局参数 -->

		          <section class="set-block">
		            <div class="set-block-head">
		              <div class="set-block-title">视频请求参数</div>
		              <div class="set-block-desc">generate_video 工具的全局请求参数（Agnes 系网关必填；Sora 兼容网关留空）。</div>
		            </div>
		            <div class="set-card">
		              <div class="set-grid2">
		                <div>
		                  <div class="set-field-label">请求模式（mode）</div>
		                  <a-input v-model="cfg.videoGenMode" placeholder="空 = 不携带（Sora 语义）；Agnes 必填 text" size="small" class="mono-input" />
		                  <div class="chip-row" style="margin-top: 8px">
		                    <button
		                      v-for="m in videoModePresets"
		                      :key="m || 'none'"
		                      class="pick-chip"
		                      :class="{ active: (cfg.videoGenMode || '') === m }"
		                      @click="cfg.videoGenMode = m"
		                    >{{ m || '不携带' }}</button>
		                  </div>
		                </div>
		                <div>
		                  <div class="set-field-label">分辨率（size）</div>
		                  <a-input v-model="cfg.videoGenSize" placeholder="空 = 不携带；Agnes 仅支持 720P" size="small" class="mono-input" />
		                  <div class="chip-row" style="margin-top: 8px">
		                    <button
		                      v-for="s in videoSizePresets"
		                      :key="s || 'none'"
		                      class="pick-chip"
		                      :class="{ active: (cfg.videoGenSize || '') === s }"
		                      @click="cfg.videoGenSize = s"
		                    >{{ s || '不携带' }}</button>
		                  </div>
		                </div>
		              </div>
	            </div>
	          </section>

		          <section class="set-block">
		            <div class="set-block-head">
		              <div class="set-block-title">语音全局参数</div>
		              <div class="set-block-desc">generate_speech / 语音转文字共用的默认音色（端点与模型在上方条目视图配置）。</div>
		            </div>
		            <div class="set-card">
		              <div class="set-field">
		                <div class="set-field-label">默认音色（voice）</div>
		                <a-input v-model="cfg.speechVoice" placeholder="alloy / nova / echo …（空 = 服务端默认）" size="small" class="mono-input" />
		              </div>
		            </div>
		          </section>

	          <section class="set-block">
	            <div class="set-block-head">
	              <div class="set-block-title">邮件 / 笔记</div>
	              <div class="set-block-desc">email_send（SMTP 发信）、notion（Notion 笔记）。浏览器自动化设置已移至「浏览器」页。</div>
	            </div>
	            <div class="set-card">
	              <div class="set-field">
	                <div class="set-field-label">SMTP 服务器</div>
	                <a-input v-model="cfg.smtpHost" placeholder="smtp.example.com（空 = 不启用 email_send）" size="small" class="mono-input" />
	              </div>
	              <div class="set-grid2" style="margin-top: 10px">
	                <div>
	                  <div class="set-field-label">SMTP 端口</div>
	                  <a-input-number v-model="cfg.smtpPort" :min="1" :max="65535" size="small" style="width: 100%" />
	                </div>
	                <div>
	                  <div class="set-field-label">发件人地址</div>
	                  <a-input v-model="cfg.smtpFrom" placeholder="me@example.com" size="small" class="mono-input" />
	                </div>
	              </div>
	              <div class="set-field" style="margin-top: 10px">
	                <div class="set-field-label">SMTP 账号</div>
	                <a-input v-model="cfg.smtpUser" placeholder="me@example.com" size="small" class="mono-input" />
	              </div>
	              <div class="set-field" style="margin-top: 10px">
	                <div class="set-field-label">SMTP 密码 / 授权码</div>
	                <a-input-password v-model="cfg.smtpPass" placeholder="授权码（非登录密码）" size="small" />
	              </div>
	              <div class="set-field" style="margin-top: 10px">
	                <div class="set-field-label">Notion API Key</div>
	                <a-input-password v-model="cfg.notionAPIKey" placeholder="ntn_xxx / secret_xxx（空 = 不启用 notion）" size="small" />
	              </div>
	            </div>
	          </section>
	        </template>

	        <!-- ══════════ 浏览器 ══════════ -->
	        <template v-else-if="page === 'browser' && cfg">
	          <div class="page-head">
	            <div class="page-title">浏览器</div>
	            <div class="page-desc">browser_open / eval / act / snapshot / tabs / screenshot（CDP 自动化，自动探测本机 Edge/Chrome）。显示模式与路径保存后下个任务生效。</div>
	          </div>

	          <section class="set-block">
	            <div class="set-block-head">
	              <div class="set-block-title">浏览器显示</div>
	              <div class="set-block-desc">控制 Agent 执行浏览器自动化时是否展示可见窗口。</div>
	            </div>
	            <div class="set-card">
	              <div class="set-row">
	                <div class="set-row-main">
	                  <div class="set-row-label">内嵌浏览器（WebView2 可见窗口）</div>
	                  <div class="set-row-hint">开启后 browser_open 等工具在可见的 WebView2 内嵌窗口执行，Agent 操作实时可见、登录态跨任务保留；关闭 = 后台 headless 无痕执行。内嵌失败（无 WebView2 Runtime 等）自动回落 headless。仅 Windows 生效。</div>
	                </div>
	                <a-switch v-model="cfg.internalBrowser" />
	              </div>
	              <div class="set-field" style="margin-top: 12px">
	                <div class="set-field-label">浏览器路径（可选）</div>
	                <a-input v-model="cfg.browserPath" placeholder="空 = 自动探测 Edge / Chrome（内嵌浏览器模式下仅 headless 回落时使用）" size="small" class="mono-input" />
	              </div>
	            </div>
	          </section>

	          <section class="set-block">
	            <div class="set-block-head">
	              <div class="set-block-title">工具可见性</div>
	              <div class="set-block-desc">browser_* 工具组对模型的可见性开关。</div>
	            </div>
	            <div class="set-card">
	              <div class="set-row">
	                <div class="set-row-main">
	                  <div class="set-row-label">隐藏内置浏览器</div>
	                  <div class="set-row-hint">开启后本应用不再注册 browser_open/act/snapshot/tabs/screenshot/eval 工具（模型不可见，不弹浏览器自动化审批）；不影响 Windows 桌面自动化。保存后下个任务生效。</div>
	                </div>
	                <a-switch v-model="cfg.browserDisabled" />
	              </div>
	            </div>
	          </section>
	        </template>

	        <!-- ══════════ MCP ══════════ -->
        <template v-else-if="page === 'mcp'">
          <div class="page-head">
            <div class="page-title">MCP 服务器</div>
		            <div class="page-desc">
		              MCP（Model Context Protocol）为 Agent 扩展外部工具：stdio = 本地子进程；http / sse = 远程服务。
		              工具注册名为 <span class="code-chip">mcp_&lt;服务器&gt;_&lt;工具&gt;</span>，任务开始时连接、结束后关闭。开关保存后<b>下一次任务</b>生效：正在运行的任务继续使用已连接的工具，关闭的服务器不会在任务中连接。修改后请点击上方「保存 MCP」并保存（本页独立保存）。
		            </div>
          </div>

          <div class="btn-toolbar" style="margin-bottom: 12px">
            <a-button size="small" type="primary" @click="addMCP">
              <template #icon><IconPlus /></template>添加服务器
            </a-button>
            <a-button size="small" type="outline" @click="openMCPQuickAdd">
              <template #icon><IconThunderbolt /></template>
              快速添加
            </a-button>
	            <!-- 粘贴导入：后端 ImportMCPServers 直接解析落盘，支持数组 /
	                 mcpServers 映射 / 顶层单服务器映射（Claude/Cline 直贴 fragment） -->
	            <a-button size="small" type="outline" status="warning" @click="mcpImportVisible = true">
	              <template #icon><IconDownload /></template>粘贴导入
	            </a-button>
            <span class="spacer"></span>
            <a-button size="small" status="success" :loading="mcpSaving" @click="saveMCP">保存 MCP</a-button>
          </div>

	          <div v-for="(sv, i) in mcpServers" :key="i" class="srv-card panel-card card-lift">
            <div class="srv-head">
              <a-input v-model="sv.name" placeholder="名称（如 filesystem）" size="small" class="mono-input" style="flex: 1" />
              <a-select v-model="sv.transport" size="mini" style="width: 92px">
                <a-option value="stdio">stdio</a-option>
                <a-option value="http">http</a-option>
                <a-option value="sse">sse</a-option>
              </a-select>
	              <a-switch v-model="sv.enabled" size="small" />
	              <a-button size="mini" @click="showTools(sv)">工具</a-button>
	              <a-button size="mini" :loading="mcpTesting === sv.name" @click="testMCP(sv)">测试</a-button>
              <a-button size="mini" :loading="mcpPublishing === sv.name" title="发布到市场（导出为 MCP 插件包）" @click="publishMCP(sv)">
                <template #icon><IconUpload /></template>
              </a-button>
              <a-button size="mini" status="danger" @click="removeMCP(i)">删除</a-button>
            </div>
            <template v-if="sv.transport === 'stdio'">
              <a-input v-model="sv.command" placeholder="启动命令：npx -y @modelcontextprotocol/server-xxx" size="small" class="mono-input" />
              <a-input
                size="small" class="mono-input" placeholder="启动参数（空格分隔，可选）"
                :model-value="(sv.args || []).join(' ')"
                @update:model-value="(v) => (sv.args = String(v || '').split(/\s+/).filter(Boolean))"
              />
            </template>
            <template v-else>
              <a-input v-model="sv.url" placeholder="服务端 URL：https://host:port/mcp" size="small" class="mono-input" />
              <a-input
                size="small" placeholder="认证头（格式 Header: value，多条换行，可选）"
                :model-value="Object.entries(sv.headers || {}).map(([k, v]) => k + ': ' + v).join('\n')"
                @update:model-value="(v) => {
                  const h = {};
                  for (const line of String(v || '').split('\n')) {
                    const idx = line.indexOf(':');
                    if (idx > 0) h[line.slice(0, idx).trim()] = line.slice(idx + 1).trim();
                  }
                  sv.headers = h;
                }"
              />
            </template>
            <a-input
              size="small" placeholder="环境变量（格式 KEY=value，每行一条，可选）"
              :model-value="Object.entries(sv.env || {}).map(([k, v]) => k + '=' + v).join('\n')"
              @update:model-value="(v) => {
                const e = {};
                for (const line of String(v || '').split('\n')) {
                  const idx = line.indexOf('=');
                  if (idx > 0) e[line.slice(0, idx).trim()] = line.slice(idx + 1);
                }
                sv.env = e;
              }"
            />
          </div>

          <div v-if="!mcpServers.length" class="empty-block">
            <div class="empty-block-title">暂无 MCP 服务器</div>
            <div class="empty-block-hint">点击「快速添加」粘贴 JSON 一键导入，或「添加服务器」手动配置</div>
          </div>

          <!-- 快速添加 MCP 弹窗：粘贴 JSON 识别添加 -->
          <a-modal
            v-model:visible="mcpQuickAdd.visible"
            title="快速添加 MCP（粘贴 JSON）"
            :ok-loading="mcpQuickAdd.loading"
            :mask-closable="false"
            ok-text="识别并添加"
            @before-ok="confirmMCPQuickAdd"
          >
            <a-textarea
              v-model="mcpQuickAdd.text"
              :auto-size="{ minRows: 12, maxRows: 24 }"
              placeholder="在此粘贴 MCP 配置 JSON，支持以下格式：

示例 1（单服务器）：
{
  &quot;name&quot;: &quot;filesystem&quot;,
  &quot;transport&quot;: &quot;stdio&quot;,
  &quot;command&quot;: &quot;npx -y @modelcontextprotocol/server-filesystem D:/&quot;,
  &quot;args&quot;: [],
  &quot;env&quot;: {}
}

示例 2（Claude Desktop / mcp 标准格式）：
{
  &quot;mcpServers&quot;: {
    &quot;filesystem&quot;: { &quot;command&quot;: &quot;npx&quot;, &quot;args&quot;: [&quot;-y&quot;, &quot;@modelcontextprotocol/server-filesystem&quot;, &quot;D:/&quot;] },
    &quot;fetch&quot;: { &quot;command&quot;: &quot;npx&quot;, &quot;args&quot;: [&quot;-y&quot;, &quot;@modelcontextprotocol/server-fetch&quot;] }
  }
}

示例 3（数组）：
[
  { &quot;name&quot;: &quot;filesystem&quot;, &quot;command&quot;: &quot;npx -y @modelcontextprotocol/server-filesystem D:/&quot; }
]

字段说明：
- name*        服务器名称（必填）
- transport   stdio | http | sse（缺省按 url/command 自动判定）
- command     启动命令（stdio）
- args        启动参数（数组或空格分隔字符串）
- url         远程服务端 URL（http/sse）
- env         环境变量对象
- headers     认证头对象
- enabled     是否启用（默认 true）"
              class="mono-input"
              style="font-family: var(--font-mono, ui-monospace, monospace); font-size: 12px;"
            />
            <div class="mcp-quick-tip">同名服务器将自动跳过；添加后请点击上方「保存 MCP」使下一任务生效。</div>
          </a-modal>
           <div v-if="!mcpServers.length" class="empty-block">
	            <div class="empty-block-title">暂无 MCP 服务器</div>
	            <div class="empty-block-hint">可从 Claude/Cline 等工具复制配置后「粘贴导入」，或「添加服务器」手动配置</div>
	          </div>

	          <!-- 粘贴导入（快速添加）：支持数组 / mcpServers 映射 / 顶层单服务器映射 fragment -->
	          <a-modal v-model:visible="mcpImportVisible" title="粘贴导入 MCP 配置" :width="560" unmount-on-close>
	            <div class="mcp-import-hint">
	              粘贴 MCP 配置 JSON（支持 kaelvoris 数组、Claude/Cline 的 {"mcpServers":{...}} 映射，以及
	              顶层单服务器格式如 {"名字":{"type":"streamable-http","url":...}}）。导入直接合并落盘：同名覆盖、异名保留。
	            </div>
	            <a-textarea
	              v-model="mcpImportText"
	              class="mono-input"
	              :auto-size="{ minRows: 10, maxRows: 18 }"
	              placeholder='{ "b1-pro": { "type": "streamable-http", "url": "http://…:8091/mcp/stream", "headers": { "Authorization": "Bearer …" }, "disabled": false } }'
	            />
	            <template #footer>
	              <a-button size="small" @click="mcpImportVisible = false">取消</a-button>
	              <a-button size="small" type="primary" :loading="mcpImportLoading" @click="importMCP">导入</a-button>
	            </template>
	          </a-modal>

	          <!-- 工具列表查看：连接 → tools/list，展示名称/方法描述/参数 -->
	          <a-modal v-model:visible="toolsVisible" :title="`工具列表 · ${toolsFor}`" :width="640" :footer="true" unmount-on-close>
	            <a-spin :loading="toolsLoading" style="display: block; width: 100%">
	              <div v-if="toolsResult && !toolsResult.ok" class="mcp-tools-error">{{ toolsResult.error || "连接失败" }}</div>
	              <template v-else-if="toolsResult">
	                <div v-if="!(toolsResult.tools || []).length" class="mcp-tools-empty">服务器未暴露任何工具</div>
	                <div v-for="t in toolsResult.tools || []" :key="t.fullName || t.name" class="mcp-tool-item">
	                  <div class="mcp-tool-head">
	                    <span class="mcp-tool-name">{{ t.name }}</span>
	                    <span class="mcp-tool-full mono-input">{{ t.fullName }}</span>
	                  </div>
	                  <div v-if="t.desc" class="mcp-tool-desc">{{ t.desc }}</div>
	                  <div v-if="schemaSummary(t)" class="mcp-tool-schema">参数：{{ schemaSummary(t) }}</div>
	                </div>
	              </template>
	            </a-spin>
	            <template #footer>
	              <a-button size="small" type="primary" @click="toolsVisible = false">关闭</a-button>
	            </template>
	          </a-modal>
	        </template>

	        <!-- ══════════ 插件 ══════════ -->
	        <template v-else-if="page === 'plugins'">
	          <div class="page-head">
	            <div class="page-title">插件</div>
	            <div class="page-desc">
	              插件以独立进程通过 MCP 协议接入（实现语言不限），完全隔离于主工程。
	              启用后自动在任务中注册 mcp_&lt;插件&gt;_&lt;工具&gt; 工具；开关下一任务生效。
	            </div>
	          </div>

	          <section class="set-block">
	            <div class="set-block-head">
	              <div class="set-block-title">安装插件</div>
	              <div class="set-block-desc">填写含 plugin.json 清单的插件目录（本地路径），安装到 ~/.kaelvoris/plugins/。</div>
	            </div>
	            <div class="set-card">
	              <div class="set-field">
	                <div class="set-field-label">插件目录</div>
	                <a-input v-model="plugInstallDir" placeholder="例如 D:\code\gocoder\plugins\office-plugin" size="small" class="mono-input" />
	              </div>
	              <div class="btn-toolbar" style="margin-top: 12px">
	                <a-button type="primary" size="small" @click="installPlugin">安装</a-button>
	              </div>
	            </div>
	          </section>

	          <section class="set-block">
	            <div class="set-block-head">
	              <div class="set-block-title">已安装插件</div>
	              <div class="set-block-desc">停用后下一任务不再注册该插件工具（保留安装）。</div>
	            </div>
	            <div v-if="!plugins.length" class="empty-block">
	              <div class="empty-block-title">暂无插件</div>
	              <div class="empty-block-hint">安装 office 等 MCP 插件后在此管理</div>
	            </div>
	            <div v-for="p in plugins" :key="p.id" class="srv-card panel-card card-lift">
	              <div class="srv-head">
	                <div class="srv-head-title">
	                  <span class="plg-name">{{ p.name || p.id }}</span>
	                  <span v-if="p.version" class="ver-pill">v{{ p.version }}</span>
	                  <span v-if="p.mcpInfo" class="plg-badge">{{ p.mcpInfo }}</span>
	                </div>
	                <a-switch v-model="p.enabled" size="small" @change="(v) => togglePlugin(p, v)" />
	                <a-button size="mini" status="danger" @click="uninstallPlugin(p)">卸载</a-button>
	              </div>
	              <div class="srv-desc">{{ p.description || "（无描述）" }}</div>
	              <div class="srv-meta">ID: {{ p.id }}<span v-if="p.skillCount"> · {{ p.skillCount }} 个技能</span></div>
	            </div>
	          </section>
	        </template>

	        <!-- ══════════ 数据源（外部 MySQL 连接）══════════ -->
	        <template v-else-if="page === 'databases'">
	          <div class="page-head">
	            <div class="page-title">数据源</div>
	            <div class="page-desc">
	              外部 MySQL 数据库连接（JDBC 式配置），工作流「SQL 自定义」节点选择连接后直连执行。
	              保存前会自动测试连接。
	            </div>
	          </div>

	          <section class="set-block">
	            <div class="set-block-head">
	              <div class="set-block-title">连接列表</div>
	              <div class="set-block-desc">SQL 节点中按连接名称选择。</div>
	              <a-button size="mini" type="primary" @click="dsNewForm">+ 新建连接</a-button>
	            </div>

	            <!-- 新建/编辑表单 -->
	            <div v-if="dsShowForm" class="set-card" style="margin-bottom: 14px">
	              <div class="set-grid2">
	                <div class="set-field">
	                  <div class="set-field-label">连接名称</div>
	                  <a-input v-model="dsForm.name" placeholder="例如：生产库订单库" size="small" />
	                </div>
	                <div class="set-field">
	                  <div class="set-field-label">数据库名</div>
	                  <a-input v-model="dsForm.dbName" placeholder="例如：user_orders" size="small" class="mono-input" />
	                </div>
	              </div>
	              <div class="set-grid2" style="margin-top: 10px">
	                <div class="set-field">
	                  <div class="set-field-label">主机</div>
	                  <a-input v-model="dsForm.host" placeholder="例如：192.168.1.100" size="small" class="mono-input" />
	                </div>
	                <div class="set-field">
	                  <div class="set-field-label">端口</div>
	                  <a-input-number v-model="dsForm.port" size="small" :min="1" :max="65535" style="width: 100%" />
	                </div>
	              </div>
	              <div class="set-grid2" style="margin-top: 10px">
	                <div class="set-field">
	                  <div class="set-field-label">用户名</div>
	                  <a-input v-model="dsForm.user" placeholder="root" size="small" class="mono-input" />
	                </div>
	                <div class="set-field">
                  <div class="set-field-label">密码</div>
                  <a-input-password v-model="dsForm.password" placeholder="" size="small" />
                </div>
              </div>
              <div class="set-field" style="margin-top: 10px">
                <div class="set-field-label">绑定表名（可选，供「查询/新增/更新/删除数据」节点使用；SQL 自定义节点无需填写）</div>
                <a-input v-model="dsForm.tableName" placeholder="例如 user_orders（英文/数字/下划线）" size="small" class="mono-input" />
              </div>
              <div class="btn-toolbar" style="margin-top: 12px">
	                <a-button size="small" :loading="dsTesting" @click="testDatasource">测试连接</a-button>
	                <a-button type="primary" size="small" :loading="dsSaving" @click="saveDatasource">保存</a-button>
	                <a-button size="small" @click="dsShowForm = false">取消</a-button>
	              </div>
	            </div>

	            <div v-if="!dsList.length && !dsLoading" class="empty-block">
	              <div class="empty-block-title">暂无数据源</div>
	              <div class="empty-block-hint">点击「新建连接」添加第一个 MySQL 数据库连接</div>
	            </div>
	            <div v-for="d in dsList" :key="d.id" class="srv-card panel-card card-lift">
	              <div class="srv-head">
	                <div class="srv-head-title">
	                  <span class="plg-name">{{ d.name }}</span>
	                  <span class="ver-pill">MySQL</span>
	                </div>
	                <a-button size="mini" @click="dsEditForm(d)">编辑</a-button>
	                <a-button size="mini" status="danger" @click="removeDatasource(d)">删除</a-button>
	              </div>
	              <div class="srv-desc">{{ d.jdbcURL || `jdbc:mysql://${d.host}:${d.port}/${d.dbName}` }}</div>
	              <div class="srv-meta">用户 {{ d.user }} · 库 {{ d.dbName }} @ {{ d.host }}:{{ d.port }}{{ d.tableName ? ` · 表 ${d.tableName}` : "" }}</div>
	            </div>
	          </section>
	        </template>

	        <!-- ══════════ 消息渠道 ══════════ -->
        <template v-else-if="page === 'channels'">
          <div class="page-head">
            <div class="page-title">消息渠道</div>
            <div class="page-desc">
              接入 IM 渠道后，对应平台的会话由绑定的数字员工接管（启用渠道必须绑定一个已启用的员工）。
              飞书走长连接、企微智能机器人 WS 免公网回调。本页独立保存。
            </div>
          </div>

          <div class="ch-summary panel-stats">
	            <div class="ch-sum-item stat-item">
	              <div class="ch-sum-num stat-num">{{ chStats.total }}</div>
	              <div class="ch-sum-label stat-label">渠道总数</div>
	            </div>
	            <div class="ch-sum-div stat-div"></div>
	            <div class="ch-sum-item stat-item">
	              <div class="ch-sum-num stat-num">{{ chStats.enabled }}</div>
	              <div class="ch-sum-label stat-label">已启用</div>
	            </div>
	            <div class="ch-sum-div stat-div"></div>
	            <div class="ch-sum-item ch-sum-on stat-item stat-on">
	              <div class="ch-sum-num stat-num">{{ chStats.online }}</div>
	              <div class="ch-sum-label stat-label">在线连接</div>
	            </div>
	            <div class="ch-sum-div stat-div"></div>
	            <div v-if="chStats.unbound" class="ch-sum-item ch-sum-warn stat-item stat-warn">
	              <div class="ch-sum-num stat-num">{{ chStats.unbound }}</div>
	              <div class="ch-sum-label stat-label">未绑定员工</div>
	            </div>
            <span class="spacer"></span>
            <a-button size="small" status="success" :loading="chSaving" @click="saveChannels">保存渠道</a-button>
          </div>

          <div class="ch-add-row">
            <span class="ch-add-title">添加渠道</span>
            <button
              v-for="t in channelTypes"
              :key="t.type"
              class="add-tile"
              :disabled="t.type === 'wechat'"
              :title="t.type === 'wechat' ? '开发中，可先保存配置' : ''"
              @click="addChannel(t.type)"
            >
              <span class="add-tile-plus">+</span>
              <span class="add-tile-name">{{ t.label }}</span>
              <span class="add-tile-hint">{{ t.hint }}</span>
            </button>
          </div>

	          <div v-for="(c, i) in channels" :key="i" class="ch-card panel-card card-lift" :style="{ '--i': i }">
            <!-- 卡头：品牌徽章 · 名称 · 状态 · 开关/删除 -->
            <div class="ch-head">
              <div class="ch-id">
                <span class="ch-badge" :class="'ch-badge-' + c.type">{{ chTypeBadge[c.type] || c.type }}</span>
                <a-input v-model="c.name" placeholder="渠道名称" size="small" class="ch-name" />
              </div>
              <span class="ch-state" :class="'ch-state-' + chState(c).key">
                <span class="ch-state-dot"></span>{{ chState(c).label }}
              </span>
              <a-switch v-model="c.enabled" size="small" />
              <a-button size="mini" status="danger" class="ch-del" @click="removeChannel(i)">删除</a-button>
            </div>

            <!-- 未启用：轻提示 -->
            <div v-if="!c.enabled" class="ch-hint-off">未启用状态仅保存配置，不建立连接</div>

            <!-- 绑定与白名单 -->
            <div class="set-grid2 ch-bind">
              <div>
                <div class="set-field-label">绑定数字员工（启用须绑定员工或岗位）</div>
                <a-select v-model="c.employeeId" size="small" placeholder="不指定（按岗位分配）" allow-clear>
                  <a-option v-for="e in empOptions" :key="e.id" :value="e.id" :disabled="!e.runnable">
                    {{ (e.avatar || "🤖") + " " + e.name }}{{ e.runnable ? "" : "（未启用）" }}
                  </a-option>
                </a-select>
              </div>
              <div>
                <div class="set-field-label">允许的用户标识</div>
                <a-input v-model="c.allowedUsers" placeholder="空 = 不限制；逗号分隔" size="small" />
              </div>
            </div>

            <!-- 岗位绑定编辑器（doc/09-设计方案 §8-2：指定员工 / 按岗位分配二选一；
                 保存时后端回填员工当前岗位快照） -->
            <BindEditor
              resource-type="channel"
              :resource-id="c.type + ':' + (c.name || '')"
              :employee-id="c.employeeId || ''"
              :position-id="c.positionId || ''"
              :assign-rule="c.assignRule || ''"
              compact
              @saved="onChannelBindSaved"
            />

            <!-- 连接方式：飞书=长连接；企微=三选一模式分段 -->
            <div v-if="c.type === 'lark'" class="ch-conn">
              <div class="ch-conn-title">连接参数 <span class="ch-conn-badge">长连接 · 免公网回调</span></div>
              <div class="set-grid2">
                <div>
                  <div class="set-field-label">App ID</div>
                  <a-input v-model="c.appId" placeholder="飞书开放平台创建应用" size="small" class="mono-input" />
                </div>
                <div>
                  <div class="set-field-label">App Secret</div>
                  <a-input-password v-model="c.appSecret" placeholder="••••••" size="small" />
                </div>
              </div>
            </div>

            <div v-else-if="c.type === 'wecom'" class="ch-conn">
              <div class="ch-conn-title">连接方式 <span class="ch-conn-badge">三选一</span></div>
              <div class="ch-mode-tabs">
                <button
                  v-for="m in wecomModes"
                  :key="m.key"
                  class="ch-mode-tab"
                  :class="{ active: wecomModeOf(c) === m.key }"
                  @click="switchWecomMode(c, m.key)"
                >
                  <span class="ch-mode-name">{{ m.label }}</span>
                  <span class="ch-mode-desc">{{ m.desc }}</span>
                </button>
              </div>

              <div v-if="wecomModeOf(c) === 'ws'" class="ch-mode-pane">
                <div class="set-grid2">
                  <div>
                    <div class="set-field-label">Bot ID</div>
                    <a-input v-model="c.botId" placeholder="智能机器人 Bot ID" size="small" class="mono-input" />
                  </div>
                  <div>
                    <div class="set-field-label">Bot Secret</div>
                    <a-input-password v-model="c.botSecret" size="small" />
                  </div>
                </div>
                <div class="set-field-hint">推荐方式：企微后台创建智能机器人，WS 长连接免公网回调，支持双向交互。</div>
              </div>

              <div v-else-if="wecomModeOf(c) === 'webhook'" class="ch-mode-pane">
                <div class="set-field">
                  <div class="set-field-label">Webhook Key</div>
                  <a-input v-model="c.webhookKey" placeholder="群机器人 Webhook Key" size="small" class="mono-input" />
                </div>
                <div class="set-field-hint">仅向群内推送消息（单向），不支持接收用户消息。</div>
              </div>

              <div v-else-if="wecomModeOf(c) === 'app'" class="ch-mode-pane">
                <div class="set-grid2">
                  <div>
                    <div class="set-field-label">CorpID</div>
                    <a-input v-model="c.corpId" placeholder="企业 ID" size="small" class="mono-input" />
                  </div>
                  <div>
                    <div class="set-field-label">Corp Secret</div>
                    <a-input-password v-model="c.corpSecret" size="small" />
                  </div>
                </div>
                <div class="set-grid2" style="margin-top: 10px">
                  <div>
                    <div class="set-field-label">Agent ID</div>
                    <a-input v-model="c.agentId" placeholder="应用 AgentID" size="small" class="mono-input" />
                  </div>
                  <div>
                    <div class="set-field-label">回调 Token</div>
                    <a-input v-model="c.token" size="small" class="mono-input" />
                  </div>
                </div>
                <div class="set-field" style="margin-top: 10px">
                  <div class="set-field-label">EncodingAESKey（43 位）</div>
                  <a-input v-model="c.encodingAesKey" size="small" class="mono-input" />
                </div>
                <div class="set-field-hint">本地回调接收推送：企微后台「接收消息」需配置 Token / EncodingAESKey，端口 9880。</div>
              </div>
            </div>

            <div v-else-if="c.type === 'wechat'" class="ch-conn">
              <div class="ch-conn-title">连接参数 <span class="ch-conn-badge warn">开发中</span></div>
              <div class="set-field">
                <div class="set-field-label">设备标识 DeviceID（wechat-robot-go）</div>
                <a-input v-model="c.deviceId" size="small" class="mono-input" />
              </div>
	              <div class="set-field-hint">个人微信渠道运行时接入开发中，可先保存配置。</div>
	            </div>

		            <div v-else-if="c.type === 'im'" class="ch-conn">
		              <div class="ch-conn-title">平台地址（通用，任意 IM 平台可配）</div>
		              <div class="set-field">
		                <div class="set-field-label">API 根地址</div>
		                <a-input v-model="c.apiBase" placeholder="必填，如 https://im.example.com" size="small" class="mono-input" />
		              </div>
		              <div class="set-field">
		                <div class="set-field-label">
		                  公网中继接收（本地 PC 无外网时开启；服务端代持回调入口，经 WS 下发本节点执行）
		                </div>
		                <a-switch v-model="c.remoteRelay" size="small">
		                  <template #checked>开启中继</template>
		                  <template #unchecked>本地直连</template>
		                </a-switch>
		              </div>
	              <div v-if="c.remoteRelay" class="set-field-hint" style="margin-bottom: 8px">
	                <template v-if="imModeOf(c) === 'pub'">
	                  <div class="set-field-label">公网中继回调 URL（配到 IM 平台公共号菜单；需 PC 节点在线，服务中心地址取自「远程节点」面板）</div>
	                <div v-if="imRelayBase() && imRelayRegistered(c)" class="mono-input ch-callback-url">{{ imRelayBase() }}/im/c/{{ imRelayFP(c) }}</div>
	                <div v-else-if="imRelayBase()" class="set-field-hint">保存并启用该渠道后，此处生成已在服务端注册的公共号回调 URL（未注册的指纹回调 404）。</div>
	                  <div v-else>请先在「远程节点」面板填写并保存服务中心地址，此处自动生成回调 URL。</div>
	                </template>
	                <template v-else>已开启公网中继：群消息接收地址见下方「群消息接收」区块的公网 URL（指纹由渠道五元组实时生成，与本节点注册指纹一致；群机器人 Webhook 为本节点出站推送，无需回调 URL）。</template>
	              </div>
		              <div class="ch-mode-tabs">
		                <button
		                  v-for="m in imModes"
		                  :key="m.key"
		                  class="ch-mode-tab"
		                  :class="{ active: imModeOf(c) === m.key }"
		                  @click="switchIMMode(c, m.key)"
		                >
	                  <span class="ch-mode-name">{{ m.label }}</span>
	                  <span class="ch-mode-desc">{{ m.desc }}</span>
	                </button>
	              </div>

		              <div v-if="imModeOf(c) === 'pub'" class="ch-mode-pane">
	                <div class="set-grid2">
	                  <div>
	                    <div class="set-field-label">公共号 ID（pub）</div>
	                    <a-input v-model="c.publicId" placeholder="如 XT-xxxx-xxxx" size="small" class="mono-input" />
	                  </div>
	                  <div>
	                    <div class="set-field-label">公共号密钥（pubsecret）</div>
	                    <a-input-password v-model="c.pubSecret" size="small" />
	                  </div>
	                </div>
	                <div class="set-grid2" style="margin-top: 10px">
	                  <div>
	                    <div class="set-field-label">企业注册号（EID）</div>
	                    <a-input v-model="c.eid" placeholder="企业注册号 no" size="small" class="mono-input" />
	                  </div>
	                  <div>
	                    <div class="set-field-label">回调端口</div>
	                    <a-input-number v-model="c.callbackPort" size="small" :min="1024" :max="65535" style="width: 100%" />
	                  </div>
	                </div>
		                <div class="set-field-hint">公共号菜单配置的回调 URL 指向本机 http://&lt;host&gt;:端口/im/callback；用户消息经回调接收，回执经 pubsendV2 定向推送（PubToken 验签；pubSend 协议）。</div>
		              </div>

			              <div v-else class="ch-mode-pane">
			                <div class="ch-conn-title" style="margin-top: 2px">群机器人 Webhook 推送（单向）</div>
			                <div class="set-field">
		                  <div class="set-field-label">完整 Webhook URL（任意平台通用；填了则忽略下方 Token）</div>
		                  <a-input v-model="c.webhookUrl" placeholder="如 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx" size="small" class="mono-input" />
		                </div>
		                <div class="set-field">
		                  <div class="set-field-label">或 Webhook Token（平台授权码，与上方 API 根拼成地址）</div>
		                  <a-input v-model="c.webhookToken" placeholder="群机器人 Webhook 中的 yzjtoken" size="small" class="mono-input" />
		                </div>
		                <div class="set-field">
		                  <div class="set-field-label">发送出参模板（JSON，{{text}} 为回复文本占位符；空 = 默认 {"content":"..."}）</div>
		                  <a-input v-model="c.outTemplate" placeholder='{"msgtype":"text","text":{"content":"{{text}}"}}' size="small" class="mono-input" />
		                </div>
			                <div class="set-field-hint">群机器人（Webhook）向群内单向推送消息，出参模板适配任意平台报文格式。与下方「群消息接收」可任意配置其一或全配置——全配置时群消息进入后任务回复经此 Webhook 推回群；仅填群机器人则纯推送，仅填群消息接收则回复用默认出参推回。</div>

		              <div class="ch-conn-title" style="margin-top: 12px">群消息接收（HTTP 端点 · hookID 分配群组）</div>
		                <div class="set-field">
		                  <div class="set-field-label">群接收 Hook ID（逗号分隔，每个 ID 对应一个群组）</div>
		                  <a-input v-model="c.groupHookIds" placeholder="如 group-a, group-b" size="small" class="mono-input" />
		                </div>
	                <div class="set-field">
	                  <div class="set-field-label">{{ c.remoteRelay ? "接收地址（公网中继，配到 IM 平台群机器人/回调设置；需 PC 节点在线）" : "接收地址（本机回调服务启动后生效，配到 IM 平台群机器人/回调设置）" }}</div>
	                  <template v-if="c.remoteRelay">
	                    <div v-if="imRelayBase() && imRelayRegistered(c)">
	                      <div v-for="h in imRelayHooks(c)" :key="h" class="mono-input ch-callback-url" style="margin-bottom: 4px">{{ imRelayBase() }}/im/g/{{ imRelayFP(c) }}/{{ h }}</div>
	                    </div>
	                    <div v-else-if="imRelayBase()" class="set-field-hint">当前指纹尚未在服务端注册（渠道未保存/未启用或字段已改动）——保存并启用渠道后此处才会生成可用的公网回调 URL，直接配置现在这个地址到 IM 平台会回调 404。</div>
	                    <div v-else>请先在「远程节点」面板填写并保存服务中心地址，此处自动生成公网回调 URL。</div>
	                  </template>
	                  <div v-else class="mono-input ch-callback-url">http://{{ localIP || "&lt;本机IP&gt;" }}:{{ c.callbackPort || 9881 }}/im/group/&lt;hookID&gt;</div>
	                </div>
		                <div class="set-field" style="margin-top: 10px">
		                  <div class="set-field-label">入参字段映射（目标 = 推送报文中的字段路径；点分嵌套如 Payload.Content，大小写不敏感）</div>
		                  <div v-for="(row, i) in imInMapRows(c)" :key="i" style="display: flex; gap: 6px; margin-bottom: 6px">
		                    <a-select v-model="row[0]" size="small" style="width: 160px" @change="(v) => imInMapSet(c, i, v, undefined)">
		                      <a-option value="text">text（消息文本）</a-option>
		                      <a-option value="sender">sender（发送者 ID）</a-option>
		                      <a-option value="senderName">senderName（发送者昵称）</a-option>
		                      <a-option value="msgId">msgId（消息去重 ID）</a-option>
		                      <a-option value="group">group（群组标识）</a-option>
		                    </a-select>
		                    <a-input
		                      :model-value="row[1]"
		                      placeholder="源字段路径，如 Payload.Content"
		                      size="small"
		                      class="mono-input"
		                      @update:model-value="(v) => imInMapSet(c, i, undefined, v)"
		                    />
		                    <a-button size="small" type="text" status="danger" @click="delImInMapRow(c, row[0])">删除</a-button>
		                  </div>
		                  <a-button size="small" type="text" @click="addImInMapRow(c)">+ 添加字段映射</a-button>
		                </div>
		                <div class="set-field-hint">入参映射：左侧选目标字段、右侧填平台推送报文中的字段路径（未配置字段自动回退宽松解析；全量原始报文始终写入日志便于精确适配）。任务回复经上方「群机器人」Webhook 推回群——两者可任意配置其一或全配置，建议一并配置 Webhook（含出参模板）。</div>
		              </div>
		            </div>
	          </div>

          <div v-if="!channels.length" class="empty-block">
            <div class="empty-block-title">暂无消息渠道</div>
            <div class="empty-block-hint">点击上方渠道卡片添加，启用前需在「数字员工」面板创建员工</div>
          </div>
        </template>

        <!-- ══════════ ACP ══════════ -->
        <template v-else-if="page === 'acp'">
          <div class="page-head">
            <div class="page-title">ACP Agents</div>
            <div class="page-desc">
              ACP（Agent Client Protocol）：把任务派发给外部编码 agent（Claude Code / Gemini CLI / Codex…）执行。
              在对话输入区选择 agent 即可绑定当前会话。本页独立保存。
              本地配置可一键<b>复制为 JSON</b>——粘贴到其他设备的本页「快速添加」，或任何支持 ACP 的软件即可直接使用。
            </div>
          </div>

	          <div class="btn-toolbar" style="margin-bottom: 12px">
            <a-button size="small" type="primary" @click="addACP">
              <template #icon><IconPlus /></template>添加 Agent
            </a-button>
            <a-button size="small" type="outline" @click="openACPQuickAdd">
              <template #icon><IconThunderbolt /></template>
              快速添加
            </a-button>
            <a-button size="small" type="outline" @click="copyAllACPJSON">
              <template #icon><IconCopy /></template>
              复制配置
            </a-button>
            <span class="spacer"></span>
            <a-button size="small" status="success" @click="saveACP">保存 ACP</a-button>
          </div>

	          <div v-for="(ag, i) in acpAgents" :key="i" class="srv-card panel-card card-lift">
            <div class="srv-head">
              <a-input v-model="ag.name" placeholder="名称（如 Claude Code）" size="small" style="flex: 1" />
              <a-switch v-model="ag.enabled" size="small" />
              <a-button size="mini" @click="copyACPJSON(ag)">复制</a-button>
              <a-button size="mini" :loading="acpTesting === ag.name" @click="testACP(ag)">测试</a-button>
              <a-button size="mini" status="danger" @click="removeACP(i)">删除</a-button>
            </div>
            <a-input v-model="ag.command" placeholder="启动命令：npx -y @zed-industries/claude-code-acp" size="small" class="mono-input" />
            <a-input
              size="small" class="mono-input" placeholder="附加参数（空格分隔，可选）"
              :model-value="(ag.args || []).join(' ')"
              @update:model-value="(v) => (ag.args = String(v || '').split(/\s+/).filter(Boolean))"
            />
            <a-input v-model="ag.cwd" placeholder="默认工作目录（可选，空 = 会话工作区）" size="small" class="mono-input" />
          </div>

          <div v-if="!acpAgents.length" class="empty-block">
           <div class="empty-block-title">暂无 ACP agent</div>
             <div class="empty-block-hint">点击「快速添加」粘贴 JSON 一键导入，或选预置 Claude Code / Gemini CLI / Codex</div>
          </div>

          <!-- 快速添加 ACP 弹窗：粘贴 JSON 识别添加（与 MCP 快速添加同构） -->
          <a-modal
            v-model:visible="acpQuickAdd.visible"
            title="快速添加 ACP（粘贴 JSON）"
            :ok-loading="acpQuickAdd.loading"
            :mask-closable="false"
            ok-text="识别并添加"
            @before-ok="confirmACPQuickAdd"
          >
            <a-textarea
              v-model="acpQuickAdd.text"
              :auto-size="{ minRows: 12, maxRows: 24 }"
              :placeholder="acpPlaceholder"
              class="mono-input"
              style="font-family: var(--font-mono, ui-monospace, monospace); font-size: 12px;"
            />
            <div class="mcp-quick-tip">
              <span>同名将自动跳过；添加后请点击「保存 ACP」使其生效。从其他设备复制来的配置粘贴到上方输入框即可一键导入。</span>
              <span class="acp-preset-row">
                预置填充：
                <a
                  v-for="p in acpPresets"
                  :key="p.name"
                  class="acp-preset-link"
                  @click="fillACPPresetJSON(p)"
                >{{ p.name }}</a>
              </span>
            </div>
          </a-modal>
        </template>

        <!-- ══════════ 服务中心连接 ══════════ -->
        <template v-else-if="page === 'remote' && remoteCfg">
          <div class="page-head">
            <div class="page-title">服务中心连接</div>
            <div class="page-desc">
              两层能力：① 本机作为<b>节点</b>接入服务中心，手机 / 管理后台可派任务到本机；
              ② 登录账号后可把任务<b>发布到其他 PC 节点</b>执行（对话输入区「远程节点」选择器）。
            </div>
          </div>

          <section class="set-block">
            <div class="set-block-head">
              <div class="set-block-title">服务中心连接</div>
            </div>
            <div class="set-card">
              <div class="set-row">
                <div class="set-row-main">
                  <div class="set-row-label">启用远程节点（本机作为被指挥节点）</div>
                  <div class="set-row-hint">开启后本机注册到服务中心，接收手机端与管理后台派发的任务</div>
                </div>
                <a-switch v-model="remoteCfg.enabled" />
              </div>
              <div class="set-field" style="margin-top: 12px">
                <div class="set-field-label">服务中心地址</div>
                <a-input v-model="remoteCfg.serverURL" placeholder="http://host:8900" size="small" class="mono-input" />
              </div>
              <div class="set-grid2" style="margin-top: 12px">
                <div>
                  <div class="set-field-label">登录账号</div>
                  <a-input v-model="remoteCfg.user" placeholder="用户名" size="small" @keyup.enter="remoteLogin" />
                </div>
                <div>
                  <div class="set-field-label">登录密码</div>
                  <a-input-password v-model="remoteCfg.pass" placeholder="密码" size="small" @keyup.enter="remoteLogin" />
                </div>
              </div>
              <div class="set-field" style="margin-top: 12px">
                <div class="set-field-label">节点显示名（默认主机名）</div>
                <a-input v-model="remoteCfg.name" placeholder="我的工作 PC" size="small" />
              </div>
	              <div class="btn-toolbar" style="margin-top: 12px">
	                <a-button size="small" :loading="remoteSaving" @click="saveRemote">保存</a-button>
	                <a-button v-if="!remoteLoggedIn" type="primary" size="small" :loading="logging" @click="remoteLogin">登录连接</a-button>
	                <a-button v-if="remoteLoggedIn" size="small" @click="remoteLogout">退出登录</a-button>
	              </div>
	              <div class="set-row-hint" style="margin-top: 8px">
	                当前账号：<template v-if="remoteLoggedIn"><b>{{ remoteAccountName }}</b>（已登录）</template><template v-else>未登录</template>
	                · 连接状态：{{ store.remoteConnected ? "已连接服务中心" : "未连接" }}<template v-if="store.remoteNodes.length"> · 在线节点 {{ store.remoteNodes.filter((x) => x.online).length }} 个</template>
	              </div>
            </div>
          </section>
        </template>

        <!-- ══════════ 关于 ══════════ -->
        <template v-else-if="page === 'version' && ver">
          <div class="page-head">
            <div class="page-title">关于</div>
            <div class="page-desc">版本信息、更新日志与升级源配置。</div>
          </div>

          <div class="about-hero">
            <div class="about-logo"><img src="/logo.png" alt="Kaelvoris" /></div>
            <div class="about-name">Kaelvoris</div>
            <div class="about-sub">极简工程控制台 · 深色优先 · 三栏布局</div>
            <div class="about-meta">
              <span class="ver-pill">v{{ ver.number || "0.0.0" }}</span>
              <span v-if="ver.buildDate">构建于 {{ ver.buildDate }}</span>
              <span v-if="ver.os">{{ ver.os }}</span>
              <span v-if="ver.commit" class="mono">{{ String(ver.commit).slice(0, 7) }}</span>
            </div>
          </div>

          <section class="set-block">
            <div class="set-block-head">
              <div class="set-block-title">检查更新</div>
            </div>
            <div class="set-card">
              <div class="set-row">
                <div class="set-row-main">
                  <div class="set-row-label">启动时自动检查更新</div>
                  <div class="set-row-hint">发现新版本时在界面提示，不会自动安装</div>
                </div>
                <a-switch v-model="ver.autoCheck" @change="saveUpdateCfg" />
              </div>
              <div class="set-field" style="margin-top: 10px">
                <div class="set-field-label">升级源（清单地址，空 = 不检查更新）</div>
                <div style="display: flex; gap: 8px">
                  <a-input v-model="ver.source" placeholder="http://…/update.json" size="small" class="mono-input" style="flex: 1" @blur="saveUpdateCfg" />
                  <a-button size="small" :loading="upChecking" @click="checkUpdate">检查更新</a-button>
                </div>
              </div>
              <div v-if="upResult" class="up-result">
                <template v-if="upResult.error">
                  <span class="up-err">检查失败：{{ upResult.error }}</span>
                </template>
                <template v-else-if="upResult.update">
                  <span class="up-ok">新版本 v{{ upResult.latest }} 可用</span>
                  <span v-if="upResult.date" class="up-meta">{{ upResult.date }}</span>
                  <a-button size="mini" type="primary" :loading="upApplying" style="margin-left: 8px" @click="applyUpdate">一键升级</a-button>
                </template>
                <template v-else>
                  <span class="up-ok">当前已是最新版本（v{{ upResult.current || (ver && ver.number) }}）</span>
                </template>
              </div>
              <div v-if="ver.pending" class="set-row-hint" style="color: var(--warning); margin-top: 8px">存在待完成升级：重启应用后自动替换为新版本。</div>
            </div>
          </section>

          <section v-if="ver.changelog && ver.changelog.length" class="set-block">
            <div class="set-block-head">
              <div class="set-block-title">更新日志</div>
            </div>
            <div class="changelog">
              <div v-for="(en, i) in ver.changelog.slice(0, 6)" :key="en.version" class="log-entry">
                <div class="log-ver" :class="{ latest: i === 0 }">v{{ en.version }}</div>
                <div v-if="en.date" class="log-date">{{ en.date }}</div>
                <ul class="log-notes">
                  <li v-for="(n, j) in en.notes" :key="j">{{ n }}</li>
                </ul>
              </div>
            </div>
          </section>
        </template>

        <!-- ══════════ 提示词 ══════════ -->
        <template v-else-if="page === 'prompts' && promptCfg">
          <div class="page-head">
            <div class="page-title">提示词</div>
            <div class="page-desc">上方下拉选择提示词段，编辑框带出该段实际生效文本（未自定义时为内置默认全文），修改后保存写入内存缓存并落盘 ~/.kaelvoris/prompts.json；未改动的内置文本不会写入配置，运行中任务不受影响，下一任务生效。</div>
          </div>

	          <!-- 上方：下拉选择提示词段（选中即带出该段配置到下方唯一编辑框）+ 重新加载 -->
	          <div class="prompt-toolbar">
	            <a-select v-model="activePromptKey" size="small" class="prompt-select">
	              <a-option v-for="seg in promptSegments" :key="seg.key" :value="seg.key">
	                {{ seg.label }}{{ promptCfg[seg.key] ? "（已自定义）" : "" }}
	              </a-option>
	            </a-select>
	            <a-button size="small" :loading="promptSaving" @click="loadPrompts">重新加载</a-button>
	            <span class="spacer"></span>
	            <span class="set-row-hint">已配置 {{ promptSegments.filter((s) => promptCfg[s.key]).length }} / {{ promptSegments.length }} 段（其余使用内置默认）</span>
	          </div>
	          <div class="prompt-desc-row">
	            <span class="prompt-desc-text">{{ activePromptSeg.desc }}</span>
	            <span v-if="promptCfg[activePromptKey]" class="prompt-state-tag">已自定义</span>
	            <span v-else class="prompt-state-tag default">内置默认（编辑框已带出全文，改动后保存生效）</span>
	          </div>

	          <!-- 下方：唯一编辑框（占满剩余可视高度，最大化编辑区）；
	               显示实际生效文本：未自定义时带出内置默认全文，用户在其上修改保存 -->
	          <div class="set-card prompt-editor-card">
	            <a-textarea
	              :model-value="activePromptDisplay"
	              @update:model-value="setPromptDisplay"
	              class="prompt-editor"
	              :placeholder="'（加载中…）'"
	              :maxlength="20000"
	              show-word-limit
	            />
	          </div>

	         <div class="btn-toolbar" style="margin-top: 10px; margin-bottom: 24px">
	           <a-button type="primary" size="small" :loading="promptSaving" @click="savePrompts">保存提示词</a-button>
	           <a-button
	             v-if="promptCfg[activePromptKey] && promptDefaults[activePromptKey]"
	             size="small"
	             @click="fillDefaultPrompt"
	           >恢复为默认</a-button>
	           <a-button v-if="promptCfg[activePromptKey]" size="small" @click="resetActivePrompt">清空</a-button>
	           <a-button
	             size="small"
	             type="text"
	             :class="{ 'prompt-default-btn': true, active: showDefault }"
	             @click="showDefault = !showDefault"
	           >查看内置默认</a-button>
	         </div>

	          <!-- 内置默认提示词（只读折叠区）：与引擎实际使用的内置文本同源 -->
	          <div v-if="showDefault" class="prompt-default-card" style="margin-bottom: 24px">
	            <div class="prompt-default-head">
	              <span>{{ activePromptSeg.label }} · 内置默认（只读，用户配置为空时生效）</span>
	              <a-button
	                size="mini"
	                type="text"
	                :disabled="!promptDefaults[activePromptKey] || promptCfg[activePromptKey] === promptDefaults[activePromptKey]"
	                @click="fillDefaultPrompt"
	              >填入编辑</a-button>
	            </div>
	            <pre class="prompt-default-body">{{ promptDefaults[activePromptKey] || '（加载中…）' }}</pre>
	          </div>
        </template>

        <!-- 数据未就绪兜底 -->
        <div v-else class="empty-block" style="margin-top: 40px">
          <div class="empty-block-title">正在加载配置…</div>
        </div>
        </div>
        </Transition>
      </div>
    </div>

    <template #footer>
      <div class="settings-foot">
        <span class="settings-foot-hint">通用 / 模型页修改需保存；MCP、渠道、ACP、远程节点、提示词在各页内独立保存</span>
        <span v-if="dirty" class="settings-foot-dirty pulse-soft">●</span>
        <span v-if="dirty" class="settings-foot-dirty-hint">有未保存修改</span>
        <span class="spacer"></span>
        <a-button size="small" @click="visible = false">关闭</a-button>
        <a-button type="primary" :loading="saving" style="min-width: 96px" @click="save">保存设置</a-button>
      </div>
    </template>
  </a-drawer>
</template>

<style scoped>
/* ══════════ 布局骨架：左目录 + 右内容 ══════════ */
.settings-layout {
  display: flex;
  height: 100%;
  margin: 0 calc(-1 * var(--pad-block));
}

/* ── 左：目录导航 ── */
.settings-nav {
  flex: 0 0 200px;
  width: 200px;
  display: flex;
  flex-direction: column;
  padding: 4px 12px 4px 0;
  border-right: 1px solid var(--border-weak);
}
.settings-nav-item {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 7px 10px;
  border: none;
  background: none;
  cursor: pointer;
  border-radius: var(--radius);
  text-align: left;
  transition: background var(--transition);
}

	.settings-nav-item:hover,
	.settings-nav-item:focus-visible { background: var(--bg-hover); }
	.settings-nav-item:focus-visible { outline: 1px solid var(--primary); outline-offset: -1px; }

	.settings-nav-item.active {
	  background: var(--bg-active);
	  box-shadow: inset 2px 0 0 var(--primary);
	}
.settings-nav-item.active .settings-nav-label { color: var(--primary); }
.settings-nav-icochip {
  width: 28px;
  height: 28px;
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: var(--radius-sm);
  background: var(--bg-inset);
  border: 1px solid var(--border-weak);
  transition: background var(--transition), border-color var(--transition);
}
.settings-nav-item:hover .settings-nav-icochip { border-color: var(--border); }
.settings-nav-item.active .settings-nav-icochip {
  background: var(--primary-tint);
  border-color: transparent;
}
.settings-nav-icon {
  font-size: 15px;
  color: var(--text-2);
  flex-shrink: 0;
  transition: color var(--transition);
}
.settings-nav-item:hover .settings-nav-icon,
.settings-nav-item.active .settings-nav-icon { color: var(--primary); }
.settings-nav-text { display: flex; flex-direction: column; gap: 1px; min-width: 0; }
.settings-nav-label {
  font-size: var(--fs-body);
  color: var(--text-1);
  line-height: 1.4;
}
.settings-nav-desc {
  font-size: var(--fs-micro);
  color: var(--text-3);
  line-height: 1.4;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.settings-nav-foot {
  margin-top: auto;
  padding: 10px 12px 6px;
  font-size: var(--fs-micro);
  color: var(--text-3);
  border-top: 1px solid var(--border-weak);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* 导航分组标题（codex 套路：小号灰色微标题，首组不额外留白） */
.settings-nav-group {
  font-size: 10px;
  color: var(--text-3);
  letter-spacing: 1px;
  padding: 10px 10px 3px;
  user-select: none;
}
.settings-nav-group.first { padding-top: 2px; }

/* footer 未保存修改指示（主色脉冲点 + 提示文案） */
.settings-foot-dirty {
  color: var(--primary);
  font-size: var(--fs-micro);
  flex-shrink: 0;
}
.settings-foot-dirty-hint {
  font-size: var(--fs-micro);
  color: var(--primary);
  flex-shrink: 0;
}

/* ── 右：内容区 ── */
.settings-body {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  padding-left: var(--pad-block);
}
.settings-content {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 2px 14px 16px 2px;
  scroll-behavior: smooth;
}

/* 页头：标题 + 描述（长页滚动吸顶，底缘弱分割线锚定层级） */
.page-head {
  position: sticky;
  top: 0;
  z-index: 3;
  margin: 0 -14px 14px -2px;
  padding: 2px 14px 10px 2px;
  background: var(--bg-page);
  border-bottom: 1px solid var(--border-weak);
}
.page-title {
  font-size: 18px;
  font-weight: 600;
  letter-spacing: -0.01em;
  color: var(--text-1);
}
.page-desc {
  font-size: var(--fs-caption);
  color: var(--text-2);
  margin-top: 4px;
  line-height: 1.6;
}

/* ── 分节（标题/描述 + 卡片）── */
.set-block { margin-bottom: var(--gap-lg); }
.set-block-head { margin-bottom: 8px; display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.set-block-title {
  font-size: var(--fs-body);
  font-weight: 600;
  color: var(--text-1);
}
.set-block-desc {
  font-size: var(--fs-micro);
  color: var(--text-3);
  margin-top: 2px;
  line-height: 1.6;
}
	.set-card {
	  background: var(--bg-card);
	  border: 1px solid var(--border-weak);
	  border-radius: var(--radius-lg);
	  padding: 12px 14px;
	}
	/* ── 行式设置项（左标签+说明，右控件）：codex 设置页套路，整行淡底 ── */
	.set-row {
	  display: flex;
	  align-items: center;
	  gap: 12px;
	  margin: 0 -8px;
	  padding: 6px 8px;
	  border-radius: var(--radius-sm);
	  transition: background var(--transition);
	}
	.set-row:hover, .set-row:focus-visible { background: var(--bg-hover); }
	.set-row:focus-visible { outline: 1px solid var(--primary); outline-offset: -1px; }
	.set-row + .set-row { margin-top: 6px; }
.set-row-main { flex: 1; min-width: 0; }
.set-row-label {
  font-size: var(--fs-body);
  color: var(--text-1);
  display: flex;
  align-items: center;
  gap: 6px;
}
.set-row-hint {
  font-size: var(--fs-micro);
  color: var(--text-3);
  margin-top: 2px;
  line-height: 1.6;
}

/* 字段网格 */
.set-grid2 {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px;
}
.set-field-label {
  font-size: var(--fs-micro);
  color: var(--text-2);
  margin-bottom: 5px;
  display: block;
}
.set-field-hint {
  font-size: var(--fs-micro);
  color: var(--text-3);
  margin-top: 4px;
}
.set-divider {
  height: 1px;
  background: var(--border-weak);
  margin: 12px 0;
}

/* ── 通用工具 ── */
.btn-toolbar { display: flex; align-items: center; gap: 8px; }
.code-chip {
  font-family: var(--font-mono);
  font-size: var(--fs-mono);
  background: var(--bg-inset);
  border: 1px solid var(--border-weak);
  border-radius: 4px;
  padding: 0 5px;
}
	.mono-input :deep(input) { font-family: var(--font-mono); font-size: var(--fs-mono); }
	.ch-callback-url {
	  padding: 6px 10px;
	  border: 1px solid var(--border-weak);
	  border-radius: 4px;
	  background: var(--bg-card);
	  color: var(--text-2);
	  font-family: var(--font-mono);
	  font-size: var(--fs-mono);
	  word-break: break-all;
	  user-select: all;
	}
.chip-row { display: flex; flex-wrap: wrap; gap: 8px; }
.pick-chip {
  border: 1px solid var(--border-weak);
  background: var(--bg-card);
  color: var(--text-2);
  border-radius: 999px;
  padding: 2px 10px;
  font-size: var(--fs-micro);
  font-family: var(--font-mono);
  cursor: pointer;
  transition: color var(--transition), border-color var(--transition), background var(--transition);
}

	.pick-chip.active {
	  color: var(--primary);
	  border-color: var(--primary);
	  background: var(--primary-tint);
	}

	/* ── 模型配置条目卡（同一类型多条目 + 启停开关）── */
	.mp-item {
	  border: 1px solid var(--border-weak);
	  border-radius: var(--radius-sm);
	  padding: 10px 12px;
	  background: var(--bg-inset);
	}
	.mp-item + .mp-item { margin-top: 10px; }
	.mp-item.mp-disabled { opacity: 0.55; }
	.mp-head {
	  display: flex;
	  align-items: center;
	  gap: 10px;
	}
	.mp-badge {
	  font-size: var(--fs-micro);
	  font-family: var(--font-mono);
	  color: var(--text-2);
	  flex: 1;
	  min-width: 0;
	  overflow: hidden;
	  text-overflow: ellipsis;
	  white-space: nowrap;
	}
	.mp-badge.on { color: var(--primary); font-weight: 600; }
	.mp-ops { display: flex; align-items: center; gap: 10px; flex-shrink: 0; }

/* 工作区路径展示 */
.ws-path {
  display: flex;
  align-items: center;
  gap: 8px;
  background: var(--bg-inset);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  padding: 8px 12px;
  overflow: hidden;
}
.ws-icon { color: var(--text-2); flex-shrink: 0; }
.ws-text {
  font-family: var(--font-mono);
  font-size: var(--fs-mono);
  color: var(--text-1);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.ws-recent { margin-top: 10px; }

/* 主题选择卡 */
.theme-cards { display: flex; gap: 10px; }
.theme-card {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 12px;
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  cursor: pointer;
  transition: border-color var(--transition), box-shadow var(--transition), transform var(--transition);
}

.theme-card.active {
  border-color: var(--primary);
  box-shadow: 0 0 0 1px var(--primary);
}
.theme-swatch {
  height: 52px;
  border-radius: var(--radius-sm);
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: 4px;
  padding: 0 12px;
  border: 1px solid var(--border-weak);
}
.theme-swatch.dk { background: #141413; }
.theme-swatch.lt { background: #F5F4ED; }
.theme-swatch .b1 { width: 70%; height: 5px; border-radius: 3px; background: #D97757; }
.theme-swatch .b2 { width: 45%; height: 4px; border-radius: 3px; background: rgba(245, 244, 237, 0.28); }
.theme-swatch.lt .b2 { background: rgba(20, 20, 19, 0.18); }
.theme-name {
  font-size: var(--fs-caption);
  color: var(--text-1);
  display: flex;
  align-items: center;
  gap: 6px;
}
.theme-default {
  font-size: var(--fs-micro);
  color: var(--primary);
  background: var(--primary-tint);
  border-radius: 4px;
  padding: 0 6px;
  line-height: 16px;
}

/* ── 服务器卡片（MCP / 渠道 / ACP 共用）── */
	/* 卡片基底走公共 .panel-card；此处保留布局差异（间距/悬停上浮） */
	.srv-card {
	  margin-bottom: 10px;
	}
	.srv-card.panel-card { gap: 8px; }
.srv-head { display: flex; align-items: center; gap: 8px; }
.srv-badge {
  flex-shrink: 0;
  font-size: var(--fs-micro);
  padding: 1px 8px;
  border-radius: 4px;
  background: var(--primary-tint);
  color: var(--primary);
  font-weight: 600;
}
.dopt-name { display: block; font-size: var(--fs-caption); color: var(--text-1); }
.dopt-desc {
  display: block;
  font-size: var(--fs-micro);
  color: var(--text-3);
  font-family: var(--font-mono);
  max-width: 320px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
/* MCP / ACP 快速添加弹窗：底部提示 */
.mcp-quick-tip {
  margin-top: 10px;
  padding: 8px 10px;
  font-size: var(--fs-micro);
  color: var(--text-3);
  background: var(--bg-fill);
  border-radius: 6px;
  line-height: 1.6;
}
/* ACP 快速添加弹窗：预置快捷填充链接（点击把 preset JSON 写入输入框） */
.acp-preset-row { display: inline-flex; align-items: center; gap: 6px; margin-left: 8px; }
.acp-preset-link {
  color: var(--primary);
  cursor: pointer;
  text-decoration: none;
}
.acp-preset-link:hover { text-decoration: underline; }

  /* 添加渠道磁贴 */
  .add-tile {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 1px;
    min-width: 128px;
    padding: 8px 12px;
    background: var(--bg-card);
    border: 1px dashed var(--border);
    border-radius: var(--radius);
    cursor: pointer;
    transition: border-color var(--transition), background var(--transition), transform var(--transition);
  }
	  .add-tile-plus { transition: transform var(--transition); }
	  .add-tile:hover:not(:disabled), .add-tile:focus-visible:not(:disabled) { border-color: var(--primary); }
	  .add-tile:focus-visible:not(:disabled) { outline: 1px solid var(--primary); outline-offset: -1px; }
	  .add-tile:hover:not(:disabled) .add-tile-plus { transform: scale(1.25); }
  .add-tile:active:not(:disabled) { transform: none; }
  .add-tile:disabled { opacity: 0.5; cursor: not-allowed; }
  .add-tile-plus { font-size: 15px; color: var(--primary); line-height: 1.2; }
  .add-tile-name { font-size: var(--fs-caption); color: var(--text-1); line-height: 1.4; }
  .add-tile-hint { font-size: var(--fs-micro); color: var(--text-3); line-height: 1.4; }

  /* ══════════ 消息渠道页（重设计 2026-11）══════════ */
	  /* 添加行 */
	  .ch-summary.panel-stats { margin-bottom: 12px; }
	  .ch-add-row {
	    display: flex;
	    align-items: center;
	    gap: 8px;
	    margin-bottom: 12px;
	  }
	  .ch-add-title {
	    font-size: var(--fs-caption);
	    color: var(--text-3);
	    margin-right: 2px;
	    flex-shrink: 0;
	  }

	  /* 渠道卡片：基底走公共 .panel-card，此处只留间距 */
	  .ch-card {
	    margin-bottom: 12px;
	  }
  .ch-head {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-bottom: 10px;
  }
  .ch-id { display: flex; align-items: center; gap: 8px; flex: 1; min-width: 0; }
  .ch-name { flex: 1; min-width: 120px; }
  .ch-name :deep(input) { font-size: var(--fs-caption); }
  .ch-badge {
    flex-shrink: 0;
    font-size: var(--fs-micro);
    padding: 2px 8px;
    border-radius: 4px;
    font-weight: 600;
    letter-spacing: 0.2px;
  }
  .ch-badge-lark { background: rgba(57, 138, 212, 0.14); color: #6FB3F2; }
  .ch-badge-wecom { background: rgba(62, 172, 101, 0.14); color: #5FC98B; }
	  .ch-badge-wechat { background: rgba(122, 213, 164, 0.12); color: #7FD6A8; }
	  .ch-badge-im { background: rgba(157, 108, 232, 0.14); color: #B18CF2; }
  .ch-state {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-size: var(--fs-micro);
    color: var(--text-3);
    flex-shrink: 0;
  }
  .ch-state-dot {
    width: 7px; height: 7px;
    border-radius: 50%;
    background: var(--text-3);
    flex-shrink: 0;
  }
  .ch-state-on { color: var(--success); }
  .ch-state-on .ch-state-dot { background: var(--success); box-shadow: 0 0 0 3px rgba(63, 185, 104, 0.12); }
  .ch-state-idle { color: var(--warning); }
  .ch-state-idle .ch-state-dot { background: var(--warning); }
  .ch-del { flex-shrink: 0; }
  .ch-hint-off {
    font-size: var(--fs-micro);
    color: var(--text-3);
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
    padding: 4px 10px;
    margin-bottom: 10px;
  }
  .ch-bind { margin-bottom: 10px; }
  .req { color: var(--danger); margin-left: 2px; }

  /* 连接区（类型专属） */
  .ch-conn {
    border-top: 1px solid var(--border-weak);
    padding-top: 10px;
  }
  .ch-conn-title {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: var(--fs-caption);
    color: var(--text-2);
    margin-bottom: 8px;
  }
  .ch-conn-badge {
    font-size: var(--fs-micro);
    color: var(--primary);
    background: var(--primary-tint);
    border-radius: 4px;
    padding: 0 6px;
    line-height: 16px;
  }
  .ch-conn-badge.warn { color: var(--warning); background: rgba(217, 150, 62, 0.12); }

  /* 企微三选一模式分段 */
  .ch-mode-tabs {
    display: flex;
    gap: 6px;
    margin-bottom: 10px;
  }
  .ch-mode-tab {
    flex: 1;
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 1px;
    padding: 6px 10px;
    background: var(--bg-inset);
    border: 1px solid var(--border-weak);
    border-radius: var(--radius-sm);
    cursor: pointer;
    text-align: left;
    transition: border-color var(--transition), background var(--transition);
  }
	  .ch-mode-tab:hover, .ch-mode-tab:focus-visible { border-color: var(--border); }
	  .ch-mode-tab:focus-visible { outline: 1px solid var(--primary); outline-offset: -1px; }
  .ch-mode-tab.active {
    border-color: var(--primary);
    background: var(--primary-tint);
  }
  .ch-mode-name { font-size: var(--fs-caption); color: var(--text-1); line-height: 1.4; }
  .ch-mode-desc { font-size: var(--fs-micro); color: var(--text-3); line-height: 1.4; }
  .ch-mode-tab.active .ch-mode-name { color: var(--primary); }
  .ch-mode-pane { padding: 2px 0 0; }
  .ch-mode-pane .set-field-hint { margin-top: 8px; }

/* 账号行 */
	.acc-row { display: flex; align-items: center; gap: 10px; }
.acc-avatar {
  width: 34px; height: 34px;
  border-radius: 50%;
  background: var(--primary-strong);
  color: var(--on-primary);
  display: flex; align-items: center; justify-content: center;
  font-weight: 600;
  font-size: var(--fs-body);
  flex-shrink: 0;
  user-select: none;
}
.acc-info { min-width: 0; }
.acc-name { font-size: var(--fs-body); color: var(--text-1); }
.acc-role { font-size: var(--fs-micro); color: var(--text-3); }

/* ── 关于页 ── */
.about-hero {
  display: flex;
  flex-direction: column;
  align-items: center;
  padding: 18px 0 6px;
  margin-bottom: var(--gap-lg);
}
	.about-logo {
	  width: 56px; height: 56px;
	  border-radius: 14px;
	  overflow: hidden;
	  display: flex; align-items: center; justify-content: center;
	  animation: pulse-soft 3.2s ease-in-out infinite;
	}
	.about-logo img {
	  width: 100%; height: 100%;
	  object-fit: cover;
	  display: block;
	}
.about-name {
  font-size: 20px;
  color: var(--text-1);
  font-weight: 600;
  margin-top: 10px;
}
.about-sub { font-size: var(--fs-caption); color: var(--text-3); margin-top: 2px; }
.about-meta {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 10px;
  font-size: var(--fs-micro);
  color: var(--text-3);
}
.ver-pill {
  background: var(--primary-tint);
  color: var(--primary);
  border-radius: 999px;
  padding: 1px 10px;
  font-size: var(--fs-caption);
  font-weight: 600;
}

/* 升级结果行 */
.up-result {
  margin-top: 10px;
  font-size: var(--fs-caption);
  display: flex;
  align-items: center;
}
.up-ok { color: var(--success); }
.up-err { color: var(--danger); }
.up-meta { color: var(--text-3); margin-left: 8px; font-size: var(--fs-micro); }

/* 更新日志时间线 */
.changelog { display: flex; flex-direction: column; }
.log-entry {
  position: relative;
  padding: 0 0 16px 18px;
  border-left: 1px solid var(--border-weak);
  margin-left: 4px;
}
.log-entry:last-child { padding-bottom: 0; }
.log-entry::before {
  content: "";
  position: absolute;
  left: -4px;
  top: 5px;
  width: 7px; height: 7px;
  border-radius: 50%;
  background: var(--text-3);
}
.log-entry:first-child::before { background: var(--primary); }
.log-ver { font-size: var(--fs-caption); font-weight: 600; color: var(--text-1); }
.log-ver.latest { color: var(--primary); }
.log-date { font-size: var(--fs-micro); color: var(--text-3); margin-top: 1px; }
.log-notes {
  margin: 6px 0 0;
  padding-left: 16px;
  font-size: var(--fs-micro);
  color: var(--text-2);
  line-height: 1.7;
}
.log-notes li { margin: 2px 0; }

	/* 页面切换动画（低速轻量）*/
	.setpage-enter-active,
	.setpage-leave-active { transition: opacity 140ms ease, transform 140ms ease; }
	.setpage-enter-from { opacity: 0; transform: translateY(4px); }
	.setpage-leave-to { opacity: 0; transform: translateY(-2px); }

	/* ── 提示词页（单编辑框布局 2026-10-24 重构）：上方下拉选段 + 下方编辑框最大化 ── */
	.prompt-toolbar { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; }
	.prompt-select { width: 220px; flex-shrink: 0; }
	.prompt-desc-row { display: flex; align-items: center; gap: 8px; margin: 0 2px 8px; }
	.prompt-desc-text { font-size: var(--fs-caption); color: var(--text-3); line-height: 1.5; }
	.prompt-state-tag {
	  flex-shrink: 0;
	  font-size: var(--fs-micro);
	  padding: 1px 8px;
	  border-radius: 999px;
	  color: var(--primary);
	  background: var(--bg-active);
	}
	.prompt-state-tag.default { color: var(--text-3); }
	/* 编辑框最大化：占满抽屉剩余可视高度（min-height 兜底矮窗口，超出走内容区滚动） */
	.prompt-editor { height: calc(100vh - 360px); min-height: 300px; }
	.prompt-editor :deep(textarea) {
	  height: 100%;
	  resize: vertical;
	  font-family: var(--font-mono);
	  font-size: var(--fs-mono, 12px);
	  line-height: 1.6;
	}
	.prompt-default-btn { color: var(--text-3); }
	.prompt-default-btn.active { color: var(--primary); }
	.prompt-default-card {
	  margin-top: 8px;
	  border: 1px dashed var(--border-weak);
	  border-radius: var(--radius);
	  background: var(--bg-hover);
	  overflow: hidden;
	}
	.prompt-default-head {
	  display: flex;
	  align-items: center;
	  justify-content: space-between;
	  gap: 8px;
	  padding: 6px 10px;
	  font-size: var(--fs-micro);
	  color: var(--text-3);
	  border-bottom: 1px dashed var(--border-weak);
	}
	.prompt-default-body {
	  margin: 0;
	  padding: 10px 12px;
	  max-height: 320px;
	  overflow: auto;
	  font-family: var(--font-mono);
	  font-size: var(--fs-mono, 12px);
	  line-height: 1.6;
	  color: var(--text-2);
	  white-space: pre-wrap;
	  word-break: break-word;
	}

	/* ── MCP 页：粘贴导入 + 工具列表查看（modal 渲染于 body，样式见非 scoped 块） ── */
	.mcp-import-hint { margin-bottom: 10px; font-size: var(--fs-caption); color: var(--text-3); line-height: 1.6; }

	/* 页脚 */
	.settings-foot {
	  display: flex;
	  align-items: center;
	  gap: 8px;
	  width: 100%;
	}
.settings-foot-hint {
  font-size: var(--fs-micro);
  color: var(--text-3);
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
}
</style>

<!-- 非 scoped：抽屉骨架（popup 渲染于 body，scoped 不可达）与下拉双行选项 -->
<style>
.arco-drawer.settings-drawer { background: var(--bg-page); }
.arco-drawer.settings-drawer .arco-drawer-header {
  height: 56px;
  padding: 0 20px;
}
.arco-drawer.settings-drawer .arco-drawer-title {
  font-size: var(--fs-title);
  font-weight: 600;
  color: var(--text-1);
}
.arco-drawer.settings-drawer .arco-drawer-body { padding: 16px 20px; }
.arco-drawer.settings-drawer .arco-drawer-footer { padding: 12px 20px; }
	.arco-dropdown .dopt-desc { padding: 0 0 4px 14px; margin-top: -2px; }

	/* MCP 工具列表查看 modal（popup 渲染于 body，scoped 不可达） */
	.mcp-tools-error {
	  padding: 12px;
	  border: 1px solid var(--danger, #f53f3f);
	  border-radius: var(--radius, 8px);
	  color: var(--danger, #f53f3f);
	  font-size: var(--fs-caption, 13px);
	  line-height: 1.6;
	  word-break: break-word;
	}
	.mcp-tools-empty { padding: 24px 0; text-align: center; color: var(--text-3, #86909c); font-size: var(--fs-caption, 13px); }
	.mcp-tool-item {
	  padding: 10px 12px;
	  border: 1px solid var(--border-weak, #e5e6eb);
	  border-radius: var(--radius, 8px);
	  margin-bottom: 8px;
	}
	.mcp-tool-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
	.mcp-tool-name { font-weight: 600; font-size: var(--fs-body, 14px); color: var(--text-1, #1d2129); }
	.mcp-tool-full { font-size: var(--fs-micro, 11px); color: var(--text-3, #86909c); }
	.mcp-tool-desc { margin-top: 6px; font-size: var(--fs-caption, 13px); color: var(--text-2, #4e5969); line-height: 1.6; white-space: pre-wrap; word-break: break-word; }
	.mcp-tool-schema { margin-top: 4px; font-size: var(--fs-micro, 11px); color: var(--text-3, #86909c); }
	</style>
