// ═════════════════════════════════════════════════════════════════
// 节点注册表（插件化）：对标 Dify/Coze 的节点扩展机制
//   设计原则：
//     1) 每个节点类型由一份 NodeDefinition 描述，可被 defineNode 注册
//     2) NodeDefinition 描述：元数据（label/group/color/icon）、输入输出端口、
//        字段 AST（驱动配置抽屉）、默认 step、节点副标题摘要函数
//     3) 业务节点对齐 internal/flow/flow.go 的 StepType（9 官方 + 6 Dify 融入）+ 结构 3 类
//     4) 注册表为 Map，运行时可动态新增（自定义节点插件）
//   扩展点：
//     - 第三方插件 import { defineNode } from "./nodeRegistry.js" 后调用即可
//     - 字段 widget 类型可扩展：input/textarea/select/number/tool/multi/hint/checkbox/code
//       （multi = 复选多选组，options 由 FlowDrawer 按 f.source 从绑定接口动态加载）
// ═════════════════════════════════════════════════════════════════
import {
    IconMessage, IconCode, IconShareInternal, IconBranch, IconFile,
    IconTool, IconSearch, IconThunderbolt, IconStop, IconPlayCircle,
    IconSwap, IconStorage, IconLink, IconEdit,
    IconLoop, IconBook, IconCloud, IconExperiment, IconApps,
} from "@arco-design/web-vue/es/icon";

// ── 节点分组定义（与 FlowPalette 一一对应）──
export const NODE_GROUPS = [
    { key: "struct", title: "结构", desc: "流程入口出口与控制" },
    { key: "ai", title: "AI 能力", desc: "模型 · 模板 · 提示" },
    { key: "data", title: "数据与知识", desc: "加载 · 转换 · 向量 · 检索" },
    { key: "logic", title: "流程控制", desc: "分支 · 透传 · 聚合" },
    { key: "io", title: "输入输出", desc: "HTTP · 代码 · 变量" },
];

// ── 注册表存储（Map<type, NodeDefinition>）──
const registry = new Map();

/**
 * 注册一个节点定义到注册表
 * @param {NodeDefinition} def 节点定义对象
 *   - type {string}       唯一类型标识（与后端 Step.Type 对齐）
 *   - label {string}      显示名
 *   - group {string}      分组 key（struct/ai/data/logic/io）
 *   - color {string}     节点主色（CSS 颜色）
 *   - icon {Component}   Arco 图标组件
 *   - inputs {string[]}   输入端口名数组（[] 表示无入边约束）
 *   - outputs {string[]}  输出端口名数组
 *   - fields {FieldAST[]} 配置字段 AST
 *   - defaults {object}   默认 step 字段值
 *   - summary {(step:object)=>string} 节点副标题摘要
 *   - structural {boolean} 是否结构节点（不可删除）
 *   - paletteHidden {boolean} 是否在托盘隐藏（start/end 自动补齐）
 */
export function defineNode(def) {
    if (!def || !def.type) throw new Error("defineNode 缺少 type");
    registry.set(def.type, def);
}

/**
 * 获取节点定义
 * @param {string} type 节点类型
 * @returns {NodeDefinition|undefined}
 */
export function getNode(type) {
    return registry.get(type);
}

/**
 * 列出全部节点定义（按注册顺序）
 * @returns {NodeDefinition[]}
 */
export function allNodes() {
    return Array.from(registry.values());
}

/**
 * 按分组返回托盘分组结构（过滤 paletteHidden）
 * @returns {Array<{key,title,desc,types:string[]}>}
 */
export function paletteGroups() {
    const all = allNodes().filter((n) => !n.paletteHidden);
    return NODE_GROUPS.map((g) => ({
        ...g,
        types: all.filter((n) => n.group === g.key).map((n) => n.type),
    })).filter((g) => g.types.length);
}

/**
 * 取节点的字段 AST（fallback 到空数组）
 * @param {string} type 节点类型
 * @returns {FieldAST[]}
 */
export function getFields(type) {
    const def = registry.get(type);
    return (def && def.fields) || [];
}

/**
 * 取节点默认 step（深拷贝避免污染）
 * @param {string} type 节点类型
 * @returns {object}
 */
export function defaultStep(type) {
    const def = registry.get(type);
    const base = { type, name: def ? def.label : type };
    return { ...base, ...JSON.parse(JSON.stringify((def && def.defaults) || {})) };
}

/**
 * 节点副标题摘要：从 step 抽取关键信息
 * @param {string} type 节点类型
 * @param {object} step 节点 step 配置
 * @returns {string}
 */
export function nodeSummary(type, step) {
    const def = registry.get(type);
    if (!def || !def.summary) return "";
    try { return def.summary(step || {}) || ""; } catch { return ""; }
}

// ═════════════════════════════════════════════════════════════════
// 内置节点定义：9 业务 + 3 结构（对齐 internal/flow/flow.go StepType）
//   颜色沿用既有 Claude 暖色系（与 SettingsDrawer 一致）
// ═════════════════════════════════════════════════════════════════

// ── 结构节点：start / end（不进托盘，由画布自动补齐）──
defineNode({
    type: "start",
    label: "开始",
    group: "struct",
    color: "#3FB968",
    icon: IconPlayCircle,
    inputs: [],
    outputs: ["start"],
    fields: [],
    defaults: { name: "开始" },
    summary: () => "流程入口",
    structural: true,
    paletteHidden: true,
});
defineNode({
    type: "end",
    label: "结束",
    group: "struct",
    color: "#E06553",
    icon: IconStop,
    inputs: ["in"],
    outputs: [],
    fields: [],
    defaults: { name: "结束" },
    summary: () => "流程出口",
    structural: true,
    paletteHidden: true,
});

// ── AI 能力组 ──
defineNode({
    type: "llm",
    label: "模型",
    group: "ai",
    color: "#D97757",
    icon: IconMessage,
    inputs: ["messages"],
    outputs: ["messages"],
    fields: [
        {
            key: "hint", widget: "hint",
            text: "调用全局配置的对话模型（设置 → 模型服务）。输入为上游消息历史，输出写回上下文；若为流程末端 LLM 节点则作为最终输出。可引用 MCP 服务器（模型经 function calling 调用其工具）与技能（正文注入上下文）。",
        },
        {
            // MCPServers 引用的 MCP 服务器名清单（flow.Step.MCPServers）：
            // 执行时连接所选服务器，其全部工具以 function calling 暴露给模型。
            key: "mcpServers", label: "MCP 服务器", widget: "multi", source: "mcp",
        },
        {
            // Skills 引用的技能名清单（flow.Step.Skills）：执行时加载技能
            // 正文注入 system 消息（排在模板 system 之后）。
            key: "skills", label: "引用技能", widget: "multi", source: "skills",
        },
    ],
    defaults: { mcpServers: [], skills: [] },
    summary: (s) => {
        const mcp = Array.isArray(s.mcpServers) ? s.mcpServers.length : 0;
        const sk = Array.isArray(s.skills) ? s.skills.length : 0;
        const parts = [];
        if (mcp) parts.push(`MCP×${mcp}`);
        if (sk) parts.push(`技能×${sk}`);
        return parts.length ? `对话模型 · ${parts.join(" + ")}` : "对话模型（全局配置）";
    },
});
defineNode({
    type: "template",
    label: "模板",
    group: "ai",
    color: "#D9963E",
    icon: IconEdit,
    inputs: [],
    outputs: ["message"],
    fields: [
        {
            key: "role", label: "角色", widget: "select", required: true,
            options: [
                { value: "system", label: "system" },
                { value: "user", label: "user" },
                { value: "assistant", label: "assistant" },
            ],
        },
        {
            key: "template", label: "模板内容（支持 {var} 变量）", widget: "textarea", required: true,
            ph: "你是 {role}，请回答：{question}",
        },
    ],
    defaults: { role: "user", template: "" },
    summary: (s) => s.template ? s.template.slice(0, 46) : "未配置模板",
});

// ── 数据与知识组 ──
defineNode({
    type: "tools",
    label: "工具",
    group: "data",
    color: "#3FB968",
    icon: IconTool,
    inputs: [],
    outputs: ["result"],
    fields: [
        { key: "name", label: "工具", widget: "tool", required: true },
        {
            key: "template", label: "参数 JSON（支持 {var}）", widget: "textarea",
            ph: '{"path": "{file}"}',
        },
    ],
    defaults: {},
    summary: (s) => s.name ? (s.template ? `参数 ${String(s.template).slice(0, 30)}` : "参数未配置") : "未选择工具",
});
defineNode({
    type: "loader",
    label: "加载",
    group: "data",
    color: "#86A5C5",
    icon: IconFile,
    inputs: [],
    outputs: ["docs"],
    fields: [
        {
            key: "template", label: "文件路径（相对工作区，支持 {var}）", widget: "input", required: true,
            ph: "docs/README.md",
        },
    ],
    defaults: { template: "" },
    summary: (s) => s.template || "未配置路径",
});
defineNode({
    type: "transformer",
    label: "转换",
    group: "data",
    color: "#C5A986",
    icon: IconSwap,
    inputs: ["docs"],
    outputs: ["docs"],
    fields: [
        {
            key: "template", label: "模式", widget: "select", required: true,
            options: [
                { value: "split", label: "split · 按空行拆分" },
                { value: "join", label: "join · 合并文档" },
            ],
        },
    ],
    defaults: { template: "split" },
    summary: (s) => s.template === "join" ? "合并文档" : "按空行拆分",
});
defineNode({
    type: "embedding",
    label: "向量",
    group: "data",
    color: "#86C5B1",
    icon: IconShareInternal,
    inputs: ["docs"],
    outputs: ["vectors"],
    fields: [
        { widget: "hint", text: "将上游文本向量化（本地哈希向量，dim=256）。" },
    ],
    defaults: {},
    summary: () => "文本向量化",
});
defineNode({
    type: "indexer",
    label: "索引",
    group: "data",
    color: "#5FA88F",
    icon: IconStorage,
    inputs: ["vectors"],
    outputs: [],
    fields: [
        { widget: "hint", text: "把上游文档写入本次运行的内存向量索引，供「检索」节点使用。" },
    ],
    defaults: {},
    summary: () => "写入内存索引",
});
defineNode({
    type: "retriever",
    label: "检索",
    group: "data",
    color: "#9C86C5",
    icon: IconSearch,
    inputs: [],
    outputs: ["docs"],
    fields: [
        {
            key: "template", label: "查询语句（支持 {var}）", widget: "input", required: true,
            ph: "{question}",
        },
        { key: "topK", label: "TopK", widget: "number", min: 1, max: 20 },
    ],
    defaults: { template: "", topK: 3 },
    summary: (s) => s.template ? `查询 ${String(s.template).slice(0, 30)}` : "按查询检索",
});

// ── 输入输出组（Dify 融入 6 类：transform/http/assigner/extractor/iteration/knowledge）──
//   字段 key 与 internal/flow 各执行器（transform.go/http.go/assigner.go/
//   extractor.go/iteration.go）存储语义逐一对齐。
defineNode({
    type: "transform",
    label: "代码转换",
    group: "io",
    color: "#B98A5E",
    icon: IconCode,
    inputs: ["in"],
    outputs: ["output"],
    fields: [
        {
            key: "role", label: "语言风格", widget: "select", required: true,
            options: [
                { value: "python", label: "python 风格（if x > 10: / endif）" },
                { value: "javascript", label: "javascript 风格（if (x > 10) { / }）" },
            ],
        },
        {
            key: "template", label: "脚本（{{x}} 变量 · {{x + 1}} 算术 · upper/lower/trim/len）", widget: "textarea", required: true,
            ph: "{{upper name}} 得分 {{score + 1}}",
        },
    ],
    defaults: { role: "python", template: "" },
    summary: (s) => s.template ? String(s.template).slice(0, 46) : "未配置脚本",
});
defineNode({
    type: "http",
    label: "HTTP 请求",
    group: "io",
    color: "#5B8DEF",
    icon: IconCloud,
    inputs: ["in"],
    outputs: ["output"],
    fields: [
        {
            key: "role", label: "方法", widget: "select", required: true,
            options: ["GET", "POST", "PUT", "DELETE", "PATCH", "HEAD"].map((m) => ({ value: m, label: m })),
        },
        {
            key: "template", label: "请求定义 JSON（url/headers/body/timeoutSec 均支持 {var}）", widget: "code", required: true,
            ph: '{"url": "https://api.example.com/v1?q={q}", "headers": {"Authorization": "Bearer {token}"}, "body": "", "timeoutSec": 30}',
        },
        { widget: "hint", text: "响应写回变量池：{节点ID.status} 状态码、{节点ID.output} 响应体，供下游模板/分支条件引用。" },
    ],
    defaults: { role: "GET", template: '{"url": "", "timeoutSec": 30}' },
    summary: (s) => `${s.role || "GET"} ${parseURLHost(s.template)}`,
});
defineNode({
    type: "assigner",
    label: "变量赋值",
    group: "io",
    color: "#9C86C5",
    icon: IconEdit,
    inputs: ["in"],
    outputs: ["output"],
    fields: [
        { key: "name", label: "目标变量名（下游 {变量名} 直引）", widget: "input", required: true, ph: "result" },
        { key: "template", label: "值表达式（{var} / {节点ID.output} / 字面量）", widget: "textarea", required: true, ph: "{LLM.output}" },
    ],
    defaults: { name: "", template: "" },
    summary: (s) => s.name ? `${s.name} = ${String(s.template || "").slice(0, 30)}` : "未配置赋值",
});
defineNode({
    type: "extractor",
    label: "参数提取",
    group: "io",
    color: "#D9963E",
    icon: IconFilter,
    inputs: ["in"],
    outputs: ["output"],
    fields: [
        {
            key: "template", label: "提取指令 JSON（text 支持引用，params 定义参数清单）", widget: "code", required: true,
            ph: '{"text": "{input}", "params": [{"name": "city", "desc": "城市名", "required": true}]}',
        },
        { widget: "hint", text: "LLM 按参数清单从文本提取结构化 JSON；逐参数写回 {节点ID.参数名}，结果 JSON 写 {节点ID.output}。必填参数缺失时报错。" },
    ],
    defaults: { template: '{"text": "", "params": []}' },
    summary: (s) => summaryExtractorParams(s.template),
});
defineNode({
    type: "iteration",
    label: "迭代",
    group: "io",
    color: "#5FA88F",
    icon: IconLoop,
    inputs: ["in"],
    outputs: ["output"],
    fields: [
        {
            key: "template", label: "迭代定义 JSON（list / itemVar / steps 子步骤链）", widget: "code", required: true,
            ph: '{"list": "{names}", "itemVar": "item", "steps": [{"type": "transform", "role": "python", "template": "{{upper item}}"}]}',
        },
        { widget: "hint", text: "列表逐项跑子步骤（上限 100 项，不支持嵌套迭代）；输出 {节点ID.output} 按行聚合、{节点ID.items} 为 JSON 数组。子步骤 type 可用 template/transform/tools。" },
    ],
    defaults: { template: '{"list": "", "itemVar": "item", "steps": []}' },
    summary: (s) => summaryIteration(s.template),
});
defineNode({
    type: "subflow",
    label: "子流程",
    group: "logic",
    color: "#3FA8A8",
    icon: IconApps,
    inputs: ["in"],
    outputs: ["output"],
    fields: [
        {
            key: "template", label: "调用定义 JSON（flow = 目标流程 ID，vars = 输入变量映射）", widget: "code", required: true,
            ph: '{"flow": "flow_1730000000", "vars": {"who": "{name}"}}',
        },
        { widget: "hint", text: "执行另一条已保存流程（流程 ID 见流程列表），输出写 {节点ID.output}，子流程逐步轨迹并入本流程。嵌套上限 5 层。" },
    ],
    defaults: { template: '{"flow": "", "vars": {}}' },
    summary: (s) => summarySubflow(s.template),
});
defineNode({
    type: "knowledge",
    label: "知识库检索",
    group: "data",
    color: "#86A5C5",
    icon: IconBook,
    inputs: ["in"],
    outputs: ["docs"],
    fields: [
        { key: "template", label: "查询语句（支持 {var} / {节点ID.output}）", widget: "textarea", required: true, ph: "{question}" },
        { key: "topK", label: "TopK（默认 8）", widget: "number", min: 1, max: 50 },
        { key: "minScore", label: "最小相似度（0 = 库默认 0.35）", widget: "number", min: 0, max: 1, step: 0.05 },
    ],
    defaults: { template: "", topK: 8, minScore: 0 },
    summary: (s) => s.template ? `查 ${String(s.template).slice(0, 30)}` : "未配置查询",
});

// ── 辅助摘要函数 ──
/** http 请求定义 JSON 中提 host（异常输入返回空串不抛）。 */
function parseURLHost(tpl) {
    try {
        const u = JSON.parse(tpl || "{}").url || "";
        return u.replace(/^https?:\/\//, "").split("/")[0] || "";
    } catch { return ""; }
}
/** extractor 参数清单摘要：city*, days（* = 必填）。 */
function summaryExtractorParams(tpl) {
    try {
        const ps = JSON.parse(tpl || "{}").params || [];
        if (!ps.length) return "未定义参数";
        return ps.map((p) => (p && p.name) + (p && p.required ? "*" : "")).join(", ");
    } catch { return "定义 JSON 解析失败"; }
}
/** iteration 定义摘要：list 表达式 + 子步骤类型数。 */
function summaryIteration(tpl) {
    try {
        const d = JSON.parse(tpl || "{}");
        const n = Array.isArray(d.steps) ? d.steps.length : 0;
        return `${d.list || "未配置列表"} · ${n} 子步骤`;
    } catch { return "定义 JSON 解析失败"; }
}
/** subflow 调用摘要：目标流程 ID + 变量映射数。 */
function summarySubflow(tpl) {
    try {
        const d = JSON.parse(tpl || "{}");
        const n = d.vars ? Object.keys(d.vars).length : 0;
        return `${d.flow || "未选择流程"} · ${n} 输入`;
    } catch { return "定义 JSON 解析失败"; }
}

// ── 流程控制组 ──
defineNode({
    type: "branch",
    label: "条件分支",
    group: "logic",
    color: "#5B8DEF",
    icon: IconBranch,
    inputs: ["in"],
    outputs: ["branch1", "branch2", "branch3", "branch4"],
    fields: [],
    defaults: { name: "条件分支" },
    summary: () => "按出边条件路由",
});
defineNode({
    type: "passthrough",
    label: "透传",
    group: "logic",
    color: "#87867F",
    icon: IconThunderbolt,
    inputs: ["in"],
    outputs: ["out"],
    fields: [
        { widget: "hint", text: "原样透传上游数据，常用于占位或调试连线。" },
    ],
    defaults: {},
    summary: () => "原样透传",
});
