<script setup>
// ═════════════════════════════════════════════════════════════════
// AI Flow 可视化画布（coze-studio 嵌入式内核专属，Vue Flow 实现）。
//   - 左侧节点库拖拽落画布（HTML5 drag + screenToFlowCoordinate）
//   - 节点拖动 / 连线 / 选中删除（Delete 键）/ 双击编辑配置
//   - coze 画布 JSON ↔ Vue Flow nodes/edges 双向转换：
//       节点 {id, type:"1|2|3...", meta:{position}, data} ↔ 节点卡
//       连线 {sourceNodeID, targetNodeID} ↔ vue-flow edge
//   - 画布顶层未知字段透传保留（rest），坐标存 node.meta.position
//   - 配置抽屉：节点标题 + 原始 data JSON（兜底一切复杂配置）
// 注意：useVueFlow({ id }) 独立 store，避免与 FlowPanel 的画布互相污染。
// ═════════════════════════════════════════════════════════════════
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from "vue";
import { Message } from "@arco-design/web-vue";
import { BaseEdge, EdgeLabelRenderer, VueFlow, useVueFlow, getBezierPath, Handle } from "@vue-flow/core";
import { Background } from "@vue-flow/background";
import { Controls } from "@vue-flow/controls";
import { MiniMap } from "@vue-flow/minimap";
import "@vue-flow/core/dist/style.css";
import "@vue-flow/core/dist/theme-default.css";
import "@vue-flow/controls/dist/style.css";
import "@vue-flow/minimap/dist/style.css";
import CozeNodeDrawer from "./CozeNodeDrawer.vue";

const props = defineProps({
    canvas: { type: String, default: "" }, // coze 画布 JSON（外部载入）
    workflowId: { type: String, default: "" }, // 所属工作流（试运行用）
    testRunHandler: { type: Function, default: null }, // async (nodeId, input) => CozeRunResult
    modelsLoader: { type: Function, default: null },   // async () => [{id, name}]（CozeListModels）
    mcpServersLoader: { type: Function, default: null }, // async () => [{name, transport, url, headers, enabled}]
    mcpToolsLoader: { type: Function, default: null },   // async (serverName) => [{name, description}]
    imageGenLoader: { type: Function, default: null },   // async () => {configured, baseURL, model, apiKey}
    kbLoader: { type: Function, default: null },         // async () => [{id, name, description}]（CozeListKnowledgeBases）
    kbCreateHandler: { type: Function, default: null },  // async (name, content) => {id, name}（CozeCreateKnowledge）
    wfLoader: { type: Function, default: null },         // async () => [{id, name, desc}]（CozeListWorkflows，子流程节点）
});
const emit = defineEmits(["change"]);

// ── coze 节点类型注册表（ID 与后端 NodeTypeMetas 对齐）──
const NODE_TYPES = [
    { key: "start",     id: "1",  name: "开始",       color: "#5C62FF", draggable: false },
    { key: "end",       id: "2",  name: "结束",       color: "#5C62FF", draggable: false },
    { key: "llm",       id: "3",  name: "大模型",     color: "#5C62FF" },
    { key: "plugin",    id: "4",  name: "插件",       color: "#CA61FF" },
    { key: "code",      id: "5",  name: "代码",       color: "#00B2B2" },
    { key: "knowledge", id: "6",  name: "知识库检索", color: "#FF811A" },
    { key: "selector",  id: "8",  name: "分支",       color: "#00B2B2" },
    { key: "sql",       id: "12", name: "SQL自定义",  color: "#FF811A" },
    { key: "output",    id: "13", name: "输出",       color: "#5C62FF" },
    { key: "text",      id: "15", name: "文本处理",   color: "#3071F2" },
    { key: "agg",       id: "32", name: "聚合",       color: "#00B2B2" },
    { key: "http",      id: "45", name: "HTTP 请求",  color: "#3071F2" },
    { key: "mcp",       id: "45", name: "MCP",        color: "#7B61FF", preset: "mcp" },
    { key: "imagegen",  id: "45", name: "图片生成",   color: "#FF5FA2", preset: "imagegen" },
    { key: "extractor", id: "45", name: "参数提取器", color: "#9F68FF", preset: "extractor" },
    { key: "intent",    id: "22", name: "意图识别",   color: "#00B2B2" },
    { key: "jsonstr",   id: "58", name: "JSON 序列化", color: "#F2B600" },
    { key: "jsonparse", id: "59", name: "JSON 反序列化", color: "#F2B600" },
    { key: "kbwrite",   id: "27", name: "知识库写入", color: "#FF811A" },
    { key: "dbquery",   id: "43", name: "查询数据", color: "#4E5BA6" },
    { key: "dbinsert",  id: "46", name: "新增数据", color: "#4E5BA6" },
    { key: "dbupdate",  id: "42", name: "更新数据", color: "#4E5BA6" },
    { key: "dbdelete",  id: "44", name: "删除数据", color: "#4E5BA6" },
    { key: "loop",      id: "21", name: "循环", color: "#722ED1" },
    { key: "batch",     id: "28", name: "批处理", color: "#9F44D3" },
    { key: "loopbreak", id: "19", name: "终止循环", color: "#F53F3F" },
    { key: "loopcontinue", id: "29", name: "继续循环", color: "#F77234" },
    { key: "loopsetvar", id: "20", name: "设置变量", color: "#722ED1" },
];
// ── 循环/批处理容器（嵌套画布）常量 ──
const CONTAINER_TYPES = ["21", "28"];        // 容器节点类型（循环/批处理）
const LOOP_ONLY_TYPES = ["19", "29", "20"];  // 仅能放入容器的体内专用节点（终止/继续/设置变量）
const CONTAINER_W = 660, CONTAINER_H = 460;  // 容器默认尺寸
// 同一内核类型（HTTP=45）的多个画布预设：palette key → 专属模板。
// MCP 节点 = MCP Streamable HTTP JSON-RPC 预设；图片生成 = 图像 API POST 预设。
const PRESET_BY_KEY = { mcp: "mcp", imagegen: "imagegen" };
const UNKNOWN_COLOR = "#8B4A12";

// Kaelvoris 补丁（2026-10）：HTTP/MCP/图片生成 同属内核类型 45，byID 按 id 建表会互相覆盖
//（后者胜出导致拖 HTTP/MCP 显示成「图片生成」）——改为按 key 建表 + id→默认 key 映射。
const byKey = Object.fromEntries(NODE_TYPES.map((t) => [t.key, t]));
const defaultKeyByID = {};
for (const t of NODE_TYPES) { if (!(t.id in defaultKeyByID)) defaultKeyByID[t.id] = t.key; }

function typeName(typeID) { const k = defaultKeyByID[typeID]; return (byKey[k] || {}).name || `节点(${typeID})`; }
// typeColor 支持预设覆盖（MCP/图片生成与 HTTP 同类型但配色不同）
function typeColor(typeID, presetKey) {
    const t = presetKey ? byKey[presetKey] : byKey[defaultKeyByID[typeID]];
    return (t || {}).color || UNKNOWN_COLOR;
}

// 节点卡副标题（类型 + 简要配置摘要）
function nodeSubTitle(nodeProps) {
    const raw = nodeProps.data && nodeProps.data.raw;
    const t = raw && raw.type;
    const d = (raw && raw.data) || {};
    if (t === "45") {
        const u = d.apiInfo && d.apiInfo.url;
        return u ? `${(d.apiInfo.method || "GET")} ${u.slice(0, 24)}` : typeName(t);
    }
    if (t === "6") {
        const dp = (d.inputs && d.inputs.datasetParam) || [];
        const ids = (dp.find((x) => x.name === "datasetList") || {}).input;
        const n = ids && ids.value && Array.isArray(ids.value.content) ? ids.value.content.length : 0;
        return n ? `${n} 个知识库` : typeName(t);
    }
    if (t === "22") {
        const n = (d.inputs && d.inputs.intents || []).length;
        return n ? `${n} 个意图` : typeName(t);
    }
    if (t === "27") {
        const dp = (d.inputs && d.inputs.datasetParam) || [];
        const ids = (dp.find((x) => x.name === "datasetList") || {}).input;
        const n = ids && ids.value && Array.isArray(ids.value.content) ? ids.value.content.length : 0;
        return n ? `写入 1 个知识库` : typeName(t);
    }
    if (t === "8") {
        const n = (d.branches || []).length;
        return `分支×${n} + else`;
    }
    if (t === "3") {
        const p = d.llmParam || [];
        const m = p.find((x) => x.name === "modleName");
        return m ? String(m.input.value.content) : typeName(t);
    }
    if (t === "9") {
        // 子流程：显示引用的流程 ID（名称需查 wfList，抽屉里维护；画布摘要先显示 ID）
        const wid = (d.inputs && d.inputs.workflowId) || "";
        return wid ? `子流程 #${wid}` : typeName(t);
    }
    if (t === "60") {
        const dp = (d.inputs && d.inputs.datasetParam) || [];
        const ids = (dp.find((x) => x.name === "datasetList") || {}).input;
        const n = ids && ids.value && Array.isArray(ids.value.content) ? ids.value.content.length : 0;
        return n ? `删除 1 个知识库文档` : typeName(t);
    }
    if (t === "31") {
        const note = (d.inputs && d.inputs.comment && d.inputs.comment.note) || "";
        return note ? String(note).slice(0, 18) : "双击编辑注释";
    }
    // 数据库四件套：显示所选外部表（负 ID 需查名称，画布摘要先显示 ID；42/43/44 附条件行数）
    if (t === "43" || t === "42" || t === "44" || t === "46") {
        const ds = ((d.inputs && d.inputs.databaseInfoList) || [])[0];
        const dsID = ds && ds.databaseInfoID ? ` #${ds.databaseInfoID}` : "";
        if (t === "43") {
            const sp = d.inputs && d.inputs.selectParam;
            return sp ? `查询${dsID} · limit ${sp.limit || "-"}` : typeName(t);
        }
        if (t === "42") {
            const up = d.inputs && d.inputs.updateParam;
            const n = up && up.condition && up.condition.conditionList ? up.condition.conditionList.length : 0;
            return `更新${dsID} · ${n} 条件`;
        }
        if (t === "44") {
            const dp = d.inputs && d.inputs.deleteParam;
            const n = dp && dp.condition && dp.condition.conditionList ? dp.condition.conditionList.length : 0;
            return `删除${dsID} · ${n} 条件`;
        }
        const ip = d.inputs && d.inputs.insertParam;
        const n = ip && ip.fieldInfo ? ip.fieldInfo.length : 0;
        return `新增${dsID} · ${n} 字段`;
    }
    // 循环族摘要
    if (t === "21") {
        const lt = (d.inputs && d.inputs.loopType) || "array";
        const arrs = ((d.inputs && d.inputs.inputParameters) || []).filter((p) => p && p.input && p.input.type === "list");
        if (lt === "count") {
            const lc = d.inputs && d.inputs.loopCount && d.inputs.loopCount.value;
            return `按次数 ×${(lc && lc.content) || "?"} · 体内 ${(d.blocks || []).length} 节点`;
        }
        if (lt === "infinite") return `无限循环 · 体内 ${(d.blocks || []).length} 节点`;
        return arrs.length ? `按数组 ${arrs[0].name} · 体内 ${(d.blocks || []).length} 节点` : `体内 ${(d.blocks || []).length} 节点`;
    }
    if (t === "28") {
        const bs = d.inputs && d.inputs.batchSize && d.inputs.batchSize.value;
        const cs = d.inputs && d.inputs.concurrentSize && d.inputs.concurrentSize.value;
        return `批量 ${(bs && bs.content) || "?"} × 并发 ${(cs && cs.content) || "?"} · 体内 ${(d.blocks || []).length} 节点`;
    }
    if (t === "19") return "跳出整个循环";
    if (t === "29") return "跳过本轮剩余步骤";
    if (t === "20") {
        const n = ((d.inputs && d.inputs.inputParameters) || []).length;
        return `${n} 个赋值`;
    }
    return typeName(t);
}

// 分支节点出口端口列表（底边均匀分布）：true（分支1）→ true_i → false（else）
function isContainer(nodeProps) {
    return CONTAINER_TYPES.includes(String(nodeProps.data && nodeProps.data.raw && nodeProps.data.raw.type));
}

function selectorPorts(nodeProps) {
    const d = (nodeProps.data && nodeProps.data.raw && nodeProps.data.raw.data) || {};
    const n = Math.max(1, (d.branches || []).length);
    const total = n + 1; // 分支数 + else
    const ports = [];
    for (let i = 0; i < n; i++) {
        ports.push({ id: i === 0 ? "true" : `true_${i}`, left: `${((i + 1) * 100) / (total + 1)}%`, color: "#00B2B2" });
    }
    ports.push({ id: "false", left: `${(total) * 100 / (total + 1)}%`, color: "#86909c" });
    return ports;
}

// 意图识别出口端口（官方语义）：branch_0..N（每个意图一个）→ default（都不命中）
function intentPorts(nodeProps) {
    const d = (nodeProps.data && nodeProps.data.raw && nodeProps.data.raw.data) || {};
    const n = Math.max(1, (d.inputs && d.inputs.intents || []).length);
    const total = n + 1;
    const ports = [];
    for (let i = 0; i < n; i++) {
        ports.push({ id: `branch_${i}`, left: `${((i + 1) * 100) / (total + 1)}%`, color: "#00B2B2" });
    }
    ports.push({ id: "default", left: `${total * 100 / (total + 1)}%`, color: "#86909c" });
    return ports;
}

// ── 新增节点的 data 预置模板（与后端 nodes/* 节点 adaptor 期望的画布结构对齐；
//     LLM 模板已经过 workflow_llm_test.go 端到端验证。复杂配置在抽屉 raw JSON 里调）──
// 关键约定：
//   - llmParam 是 []*Param（Param.Name 分发字段；"modleName" 为 coze 源码固有拼写），
//     Param.Input.Value.Content 强转 string，数值也必须传 JSON 字符串
//   - modelType=69010 为 Bootstrap 探测可用的模型主键（agnes-2.5-flash），
//     环境变化时改这里（见 Bootstrap 日志 "sync old model id 69010"）
//   - 专属配置字段（code/branches/content…）经 vo.Inputs 内嵌结构提升到 inputs 顶层
const LLM_PARAM = (system, user) => [
    { name: "modelType", input: { type: "literal", value: { type: "literal", content: "69010" } } },
    { name: "modleName", input: { type: "literal", value: { type: "literal", content: "agnes-2.5-flash" } } },
    { name: "systemPrompt", input: { type: "literal", value: { type: "literal", content: system } } },
    { name: "prompt", input: { type: "literal", value: { type: "literal", content: user } } },
    { name: "responseFormat", input: { type: "literal", value: { type: "literal", content: "0" } } },
    { name: "temperature", input: { type: "literal", value: { type: "literal", content: "0.5" } } },
];
const DATA_TEMPLATES = {
    "1": { nodeMeta: { title: "开始", icon: "icon-start" }, outputs: [{ name: "query", type: "string", description: "用户输入", required: true }] },
    "2": { nodeMeta: { title: "结束", icon: "icon-end" }, inputs: { terminatePlan: "returnVariables" }, inputParameters: [] },
    "3": {
        nodeMeta: { title: "大模型", icon: "icon-llm" },
        inputs: { llmParam: LLM_PARAM("你是一个乐于助人的助手。", "回答这个问题：{{query}}。直接给出答案。") },
        outputs: [{ name: "output", type: "string" }],
    },
    "4": { nodeMeta: { title: "插件", icon: "icon-plugin" }, inputs: { apiParam: [] } },
    "5": {
        nodeMeta: { title: "代码", icon: "icon-code" },
        inputs: {
            // direct runner 契约：async def main(args)（args.params 为输入参数），asyncio.run 调用
            code: "async def main(args):\n    return {\"result\": args.params.get(\"query\", \"\")}",
            language: 3, // 3=Python 5=JavaScript（convertCodeLanguage 仅认 3/5）
        },
        outputs: [{ name: "result", type: "string" }],
    },
    "6": {
        // 知识库检索（官方画布示例 examples/knowledge.json，type 6）：
        //   outputs 固定 outputList（list<object{output}>）；datasetParam 七项检索策略；
        //   inputParameters 固定 Query（引用上游或字面量）
        nodeMeta: { title: "知识库检索", icon: "icon-knowledge-query" },
        outputs: [{ type: "list", name: "outputList", schema: { type: "object", schema: [{ type: "string", name: "output" }] } }],
        inputs: {
            datasetParam: [
                { name: "datasetList", input: { type: "list", schema: { type: "string" }, value: { type: "literal", content: [] } } },
                { name: "topK", input: { type: "integer", value: { type: "literal", content: 5 } } },
                { name: "useRerank", input: { type: "boolean", value: { type: "literal", content: false } } },
                { name: "useRewrite", input: { type: "boolean", value: { type: "literal", content: false } } },
                { name: "isPersonalOnly", input: { type: "boolean", value: { type: "literal", content: false } } },
                { name: "minScore", input: { type: "float", value: { type: "literal", content: 0.5 } } },
                { name: "strategy", input: { type: "integer", value: { type: "literal", content: 1 } } },
            ],
            inputParameters: [{ name: "Query", input: { type: "string", value: { type: "literal", content: "" } } }],
        },
    },
    "8": {
        nodeMeta: { title: "分支", icon: "icon-condition" },
        inputs: {
            // Selector：branches[i].condition.conditions[] = {operator(1=Equal), left(引用), right(字面值)}
            // 出口端口：第 1 分支="true"、第 N 分支="true_N"、else="false"（normalizePorts 语义）
            branches: [
                { condition: { logic: 1, conditions: [] } },
            ],
        },
    },
    "12": {
        nodeMeta: { title: "SQL自定义", icon: "icon-database" },
        // DatabaseCustomSQL：sql 模板 + databaseInfoList（数据库绑定，需先建库）+ inputParameters 变量映射
        inputs: { databaseInfoList: [], sql: "", inputParameters: [] },
        outputs: [{ type: "list", name: "outputList", schema: { type: "object", schema: [] } }, { name: "rowNum", type: "integer" }],
    },
    "13": {
        nodeMeta: { title: "输出", icon: "icon-output" },
        inputs: { content: { type: "string", value: { type: "literal", content: "处理完成" } }, streamingOutput: true },
    },
    "9": {
        // SubWorkflow：workflowId（已发布/草稿流程）+ 输入映射；内核 toSubWorkflowNodeSchema
        // 按 workflowId 自动加载子画布构建嵌套 schema（to_schema.go），空版本 = 草稿
        nodeMeta: { title: "工作流", icon: "icon-workflow" },
        inputs: { workflowId: "", workflowVersion: "", inputParameters: [] },
    },
    "60": {
        // 知识库删除（knowledge_deleter.go adaptor）：datasetParam[0].input.value.content=[knowledgeID]，
        // 输入映射固定 documentID（string）→ 删除该文档
        nodeMeta: { title: "知识库删除", icon: "icon-knowledge-delete" },
        outputs: [{ name: "documentID", type: "string" }, { name: "isDeleted", type: "boolean" }],
        inputs: {
            datasetParam: [
                { name: "datasetList", input: { type: "list", schema: { type: "string" }, value: { type: "literal", content: [] } } },
            ],
            inputParameters: [{ name: "documentID", input: { type: "string", value: { type: "literal", content: "" } } }],
        },
    },
    "31": {
        // 注释（Comment）：纯画布标注，内核 blockTypeToSkip 跳过执行；note 为文本
        nodeMeta: { title: "注释", icon: "icon-comment" },
        inputs: { comment: { schemaType: "comment", note: "" } },
        size: { width: 240, height: 96 },
    },
    // ── 数据库四件套（42/43/44/46）：databaseInfoList 选外部表（负 ID 字符串）+ 结构化参数 ──
    // 条件行（adapt.go buildClauseFromParams）：[[left(列名), operation(枚举), right(引用/常量)]]
    "43": {
        // 查询数据（Query）：fieldList 空 = 全列；limit 必须 > 0（内核 Build 校验）
        nodeMeta: { title: "查询数据", icon: "icon-database" },
        outputs: [{ name: "outputList", type: "list", schema: { type: "object", properties: [] } }, { name: "rowNum", type: "integer" }],
        inputs: { databaseInfoList: [], selectParam: { condition: null, orderByList: [], limit: 50, fieldList: [] } },
    },
    "46": {
        // 新增数据（Insert）：fieldInfo[[left(列名), right(值映射)]]
        nodeMeta: { title: "新增数据", icon: "icon-database" },
        outputs: [{ name: "outputList", type: "list", schema: { type: "object", properties: [] } }, { name: "rowNum", type: "integer" }],
        inputs: { databaseInfoList: [], insertParam: { fieldInfo: [] } },
    },
    "42": {
        // 更新数据（Update）：condition WHERE + fieldInfo SET
        nodeMeta: { title: "更新数据", icon: "icon-database" },
        outputs: [{ name: "outputList", type: "list", schema: { type: "object", properties: [] } }, { name: "rowNum", type: "integer" }],
        inputs: { databaseInfoList: [], updateParam: { condition: { conditionList: [], logic: "AND" }, fieldInfo: [] } },
    },
    "44": {
        // 删除数据（Delete）：condition WHERE
        nodeMeta: { title: "删除数据", icon: "icon-database" },
        outputs: [{ name: "outputList", type: "list", schema: { type: "object", properties: [] } }, { name: "rowNum", type: "integer" }],
        inputs: { databaseInfoList: [], deleteParam: { condition: { conditionList: [], logic: "AND" } } },
    },
    // ── 循环族：容器节点（blocks/edges 顶层字段承载体内图，内核 NodeToNodeSchema 递归转换）──
    "21": {
        // 循环（Loop）：loopType array/count/infinite；数组参数（inputParameters 中 list 项）逐轮
        // 注入体内（体内节点 ref {blockID: 本节点, name: 参数名} = 当前元素）；中间变量跨轮传递。
        // outputs 为 Param 形态（input.ref 指向体内节点输出 = 每轮聚合）；数组 BlockInput.Type 用 "list"
        nodeMeta: { title: "循环", icon: "icon-loop" },
        outputs: [{ name: "outputList", input: { type: "list", schema: { type: "string" }, value: { type: "ref", content: { blockID: "", name: "", source: "block-output" } } } }],
        inputs: {
            loopType: "array",
            loopCount: { type: "integer", value: { type: "literal", content: "3" } },
            inputParameters: [{ name: "item", input: { type: "list", schema: { type: "string" }, value: { type: "ref", content: { blockID: "", name: "", source: "block-output" } } } }],
            variableParameters: [],
        },
    },
    "28": {
        // 批处理（Batch）：batchSize/concurrentSize 顶层 BlockInput（字符串数字）；outputs 同 Loop Param 形态
        nodeMeta: { title: "批处理", icon: "icon-batch" },
        outputs: [{ name: "outputList", input: { type: "list", schema: { type: "string" }, value: { type: "ref", content: { blockID: "", name: "", source: "block-output" } } } }],
        inputs: {
            batchSize: { type: "integer", value: { type: "literal", content: "100" } },
            concurrentSize: { type: "integer", value: { type: "literal", content: "10" } },
            inputParameters: [{ name: "item", input: { type: "list", schema: { type: "string" }, value: { type: "ref", content: { blockID: "", name: "", source: "block-output" } } } }],
            variableParameters: [],
        },
    },
    "19": {
        // 终止循环（Break）：体内专用，无配置；内核置 $break 中间变量结束循环
        nodeMeta: { title: "终止循环", icon: "icon-break" },
        inputs: {},
    },
    "29": {
        // 继续循环（Continue）：体内专用，无配置；跳过本轮剩余步骤
        nodeMeta: { title: "继续循环", icon: "icon-continue" },
        inputs: {},
    },
    "20": {
        // 设置变量（VariableAssignerWithinLoop）：体内专用；left=中间变量 ref、right=值映射
        nodeMeta: { title: "设置变量", icon: "icon-variable" },
        inputs: { inputParameters: [] },
    },
    "15": {
        // TextProcessor（concat）：concatResult=模板（{{var}} 插值）、arrayItemConcatChar=数组拼接符、
        // 其余项为模板变量映射（与 drawer formMode 15 序列化一致）
        nodeMeta: { title: "文本处理", icon: "icon-text" },
        inputs: {
            method: "concat",
            concatParams: [{ name: "concatResult", input: { type: "string", value: { type: "literal", content: "" } } }],
        },
        outputs: [{ name: "output", type: "string" }],
    },
    "32": {
        nodeMeta: { title: "聚合", icon: "icon-variable-merge" },
        // VariableAggregator：mergeGroups []*Param（每项 name=分组名，input=引用某上游输出）
        inputs: { mergeGroups: [] },
        outputs: [{ name: "output", type: "string" }],
    },
    "45": {
        nodeMeta: { title: "HTTP 请求", icon: "icon-http" },
        // HttpRequestNode：内嵌提升到 inputs 顶层（apiInfo/headers/params/body/setting）
        inputs: {
            apiInfo: { method: "GET", url: "https://httpbin.org/get" },
            headers: [],
            params: [],
            body: { bodyType: "none", bodyData: null },
            setting: { timeoutMs: 30000, retryTimes: 3 },
        },
        outputs: [{ name: "statusCode", type: "integer" }, { name: "responseBody", type: "string" }],
    },
    // 意图识别（22）：LLM 分类 query 到预设意图；出口 branch_0..N + default
    "22": {
        nodeMeta: { title: "意图识别", icon: "icon-intent" },
        outputs: [{ name: "classificationId", type: "integer" }, { name: "reason", type: "string" }],
        inputs: {
            chatHistorySetting: { enableChatHistory: false, chatHistoryRound: 3 },
            inputParameters: [{ name: "query", input: { type: "string", value: { type: "literal", content: "" } } }],
            llmParam: {
                modelType: "69010", modelName: "agnes-2.5-flash", responseFormat: 2, temperature: 1, maxTokens: 4096,
                systemPrompt: { type: "string", value: { type: "literal", content: "" } },
                prompt: { type: "string", value: { type: "literal", content: "{{query}}" } },
            },
            intents: [{ name: "意图1" }],
            mode: "all",
        },
    },
    // JSON 序列化（58）：任意变量 → JSON 字符串
    "58": {
        nodeMeta: { title: "JSON 序列化", icon: "icon-to-json" },
        inputs: { inputParameters: [{ name: "input", input: { type: "string", value: { type: "literal", content: "" } } }] },
        outputs: [{ name: "output", type: "string" }],
    },
    // JSON 反序列化（59）：JSON 字符串 → 变量（输出类型可编辑，默认 object）
    "59": {
        nodeMeta: { title: "JSON 反序列化", icon: "icon-from-json" },
        inputs: { inputParameters: [{ name: "input", input: { type: "string", value: { type: "literal", content: "" } } }] },
        outputs: [{ name: "output", type: "object" }],
    },
    // 知识库写入（27）：文本内容写入指定知识库（官方示例 examples/knowledge.json type 27）
    "27": {
        nodeMeta: { title: "知识库写入", icon: "icon-knowledge-write" },
        outputs: [{ name: "documentId", type: "string" }, { name: "fileName", type: "string" }, { name: "fileUrl", type: "string" }],
        inputs: {
            inputParameters: [{ name: "knowledge", input: { type: "string", value: { type: "literal", content: "" } } }],
            datasetParam: [{ name: "datasetList", input: { type: "list", schema: { type: "string" }, value: { type: "literal", content: [] } } }],
        },
    },
};

// MCP / 图片生成 / 参数提取器 = HTTP(45) 的场景预设（内核无原生节点，走 HTTP API）
const HTTP_PRESETS = {
    // 参数提取器（对标 Dify Parameter Extractor）：LLM 按 schema 提取结构化字段，
    // 落地为 LLM(responseFormat=2 JSON) → 通用 JSON 解析 API 预设（表单驱动生成）
    extractor: {
        nodeMeta: { title: "参数提取器", icon: "icon-extract" },
        inputs: {
            apiInfo: { method: "GET", url: "https://httpbin.org/get" },
            headers: [],
            params: [],
            body: { bodyType: "none", bodyData: null },
            setting: { timeoutMs: 30000, retryTimes: 3 },
        },
        outputs: [{ name: "output", type: "object", description: "提取的字段集合" }],
    },
    mcp: {
        nodeMeta: { title: "MCP", icon: "icon-tool" },
        inputs: {
            apiInfo: { method: "POST", url: "http://localhost:9200/mcp" },
            headers: [{ name: "Content-Type", input: { type: "string", value: { type: "literal", content: "application/json" } } }],
            params: [],
            body: {
                bodyType: "json",
                bodyData: { json: '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"{tool_name}","arguments":{}}}' },
            },
            setting: { timeoutMs: 60000, retryTimes: 1 },
        },
        outputs: [{ name: "statusCode", type: "integer" }, { name: "responseBody", type: "string" }],
    },
    imagegen: {
        nodeMeta: { title: "图片生成", icon: "icon-image" },
        inputs: {
            apiInfo: { method: "POST", url: "https://api.example.com/v1/images/generations" },
            headers: [
                { name: "Content-Type", input: { type: "string", value: { type: "literal", content: "application/json" } } },
                { name: "Authorization", input: { type: "string", value: { type: "literal", content: "Bearer {api_key}" } } },
            ],
            params: [],
            body: {
                bodyType: "json",
                bodyData: { json: '{"model":"{model}","prompt":"{prompt}","n":1,"size":"1024x1024"}' },
            },
            setting: { timeoutMs: 120000, retryTimes: 1 },
        },
        outputs: [{ name: "statusCode", type: "integer" }, { name: "responseBody", type: "string" }],
    },
};

// ── Vue Flow 独立实例（id 隔离，避免与 FlowPanel 画布共享全局 store）──
const FLOW_ID = "coze-flow-canvas";
const {
    nodes: nodesRef, edges: edgesRef, setNodes, setEdges,
    addNodes, addEdges, removeNodes, removeEdges, getSelectedNodes, getSelectedEdges,
    fitView, screenToFlowCoordinate, onConnect, onNodeDragStop, findNode,
} = useVueFlow({ id: FLOW_ID });

// 画布顶层字段透传（除 nodes/edges 外原样保留）
let canvasRest = {};
let seq = 0; // 新节点自增 ID（coze 数字 ID 空间大，取时间戳避免碰撞）

// ── 画布 JSON → Vue Flow ──
// 循环/批处理容器（21/28）：raw.blocks（体内节点）/ raw.edges（体内边）→ Vue Flow 父子节点
// （child.parentNode + extent:"parent"）；体内自动起点/终点边（loop-function-inline-*）
// 在序列化时推导，加载时过滤不显示。
function isAutoInnerEdge(e, containerRaw) {
    const cid = String(containerRaw.id);
    const outPort = String(containerRaw.type) === "28" ? "batch-function-inline-output" : "loop-function-inline-output";
    const inPort = String(containerRaw.type) === "28" ? "batch-function-inline-input" : "loop-function-inline-input";
    return (String(e.sourceNodeID) === cid && e.sourcePortID === outPort) || (String(e.targetNodeID) === cid && e.targetPortID === inPort);
}

function loadCanvas(str) {
    let c = {};
    try { c = JSON.parse(str || "{}"); } catch { c = {}; }
    canvasRest = { ...c };
    delete canvasRest.nodes;
    delete canvasRest.edges;
    const vn = [];
    const ve = [];
    for (const [i, n] of (c.nodes || []).entries()) {
        // Kaelvoris 补丁（2026-10）：旧画布标题可能在 meta.nodeMeta 或缺失——归一化到 data.nodeMeta.title
        if (n.data && typeof n.data === "object") {
            if (!n.data.nodeMeta) n.data.nodeMeta = {};
            if (!n.data.nodeMeta.title) {
                const legacy = n.meta && n.meta.nodeMeta && n.meta.nodeMeta.title;
                n.data.nodeMeta.title = legacy || typeName(String(n.type));
            }
        }
        const t = String(n.type);
        const pos = (n.meta && n.meta.position) || { x: 60 + (i % 4) * 280, y: 50 + Math.floor(i / 4) * 170 };
        // 容器节点：大尺寸 + 体内子节点
        if (CONTAINER_TYPES.includes(t)) {
            vn.push({
                id: String(n.id),
                type: "cozenode",
                position: pos,
                style: { width: `${CONTAINER_W}px`, height: `${CONTAINER_H}px` },
                data: { raw: n },
            });
            (n.blocks || []).forEach((b, bi) => {
                if (b.data && typeof b.data === "object") {
                    if (!b.data.nodeMeta) b.data.nodeMeta = {};
                    if (!b.data.nodeMeta.title) b.data.nodeMeta.title = typeName(String(b.type));
                }
                vn.push({
                    id: String(b.id),
                    type: "cozenode",
                    parentNode: String(n.id),
                    extent: "parent",
                    position: (b.meta && b.meta.position) || { x: 40 + (bi % 2) * 280, y: 64 + Math.floor(bi / 2) * 130 },
                    data: { raw: b },
                });
            });
            (n.edges || []).filter((e) => !isAutoInnerEdge(e, n)).forEach((e, ei) => {
                ve.push({
                    id: `ei-${e.sourceNodeID}-${e.targetNodeID}-${i}-${ei}`,
                    source: String(e.sourceNodeID),
                    target: String(e.targetNodeID),
                    sourceHandle: e.sourcePortID || null,
                    type: "cozeedge",
                    animated: false,
                });
            });
            continue;
        }
        vn.push({ id: String(n.id), type: "cozenode", position: pos, data: { raw: n } });
    }
    (c.edges || []).forEach((e, i) => {
        ve.push({
            id: `e-${e.sourceNodeID}-${e.targetNodeID}-${i}`,
            source: String(e.sourceNodeID),
            target: String(e.targetNodeID),
            // 分支出口端口（selector）：coze 的 sourcePortID ↔ Vue Flow 的 sourceHandle
            sourceHandle: e.sourcePortID || null,
            type: "cozeedge",
            animated: false,
        });
    });
    setNodes(vn);
    setEdges(ve);
    nextTick(() => fitView({ padding: 0.15, maxZoom: 1.1 }));
}

// ── Vue Flow → 画布 JSON（保存时调用）──
// Kaelvoris 补丁（2026-10）：内核 vo.Inputs 要求 inputParameters 位于 data.inputs 下；
// 旧画布（或手工 JSON）可能放在 data 顶层——导出时归一化；并为 LLM 节点自动
// 提取提示词模板变量为输入映射（缺了它单节点试运行输入会被过滤、{{var}} 不替换）。
function normalizeNodeInputs(raw, allNodes, allEdges) {
    if (!raw || !raw.data) return raw;
    const d = raw.data;
    if (!d.inputs) d.inputs = {};
    if (Array.isArray(d.inputParameters)) {
        if (!Array.isArray(d.inputs.inputParameters) || d.inputs.inputParameters.length === 0) {
            d.inputs.inputParameters = d.inputParameters;
        }
        delete d.inputParameters;
    }
    if (String(d.type) === "3" && !(d.inputs.inputParameters || []).length) {
        // 提取 llmParam 中 systemPrompt/prompt 的 {{var}} 模板变量
        const texts = (d.inputs.llmParam || [])
            .filter((p) => p && (p.name === "systemPrompt" || p.name === "prompt"))
            .map((p) => {
                const v = p.input && p.input.value;
                return v && v.content != null ? String(v.content) : "";
            });
        const vars = [...new Set((texts.join(" ").match(/\{\{\s*([\w.]+)\s*\}\}/g) || [])
            .map((s) => s.replace(/[{}\s]/g, "").split(".")[0]))];
        const upstream = (allEdges || []).filter((e) => e.targetNodeID === raw.id).map((e) => e.sourceNodeID);
        const findUp = (name) => {
            for (const uid of upstream) {
                const un = (allNodes || []).find((x) => x.id === uid);
                const outs = (un && un.data && un.data.outputs) || [];
                const v = outs.find((o) => o.name === name);
                if (v) return { blockID: uid, name: v.name };
            }
            return null;
        };
        const params = vars
            .map((name) => ({ name, up: findUp(name) }))
            .filter((x) => x.up)
            .map((x) => ({ name: x.name, input: { type: "string", value: { type: "ref", content: { blockID: x.up.blockID, name: x.up.name, source: "block-output" } } } }));
        if (params.length) d.inputs.inputParameters = params;
    }
    return raw;
}

// 体内自动边推导（序列化时补全，内核 EdgeToConnection 语义）：
//   入度 0 的体内节点 → 起点（loop-function-inline-output，从容器流出）
//   出度 0 且非 Break/Continue → 终点（loop-function-inline-input，回到容器 = 体内 END）
function autoInnerEdges(containerRaw, kidRaws, innerEdges) {
    const cid = String(containerRaw.id);
    const t = String(containerRaw.type);
    const outPort = t === "28" ? "batch-function-inline-output" : "loop-function-inline-output";
    const inPort = t === "28" ? "batch-function-inline-input" : "loop-function-inline-input";
    const inDeg = {}, outDeg = {};
    kidRaws.forEach((k) => { inDeg[k.id] = 0; outDeg[k.id] = 0; });
    innerEdges.forEach((e) => {
        if (inDeg[String(e.targetNodeID)] !== undefined) inDeg[String(e.targetNodeID)]++;
        if (outDeg[String(e.sourceNodeID)] !== undefined) outDeg[String(e.sourceNodeID)]++;
    });
    const out = [];
    kidRaws.forEach((k) => {
        const kt = String(k.type);
        if (!inDeg[k.id] && kt !== "19" && kt !== "29") out.push({ sourceNodeID: cid, targetNodeID: k.id, sourcePortID: outPort });
        if (!outDeg[k.id] && kt !== "19" && kt !== "29") out.push({ sourceNodeID: k.id, targetNodeID: cid, targetPortID: inPort });
    });
    return out;
}

function exportCanvas() {
    const allNodes = nodesRef.value || [];
    const topNodes = allNodes.filter((n) => !n.parentNode);
    const topEdges = (edgesRef.value || []).filter((e) => {
        const sn = allNodes.find((x) => x.id === e.source);
        const tn = allNodes.find((x) => x.id === e.target);
        return !(sn && sn.parentNode) && !(tn && tn.parentNode); // 排除体内边
    });
    const raws = topNodes.map((n) => {
        const raw = JSON.parse(JSON.stringify(n.data.raw || {}));
        raw.meta = { ...(raw.meta || {}), position: { x: Math.round(n.position.x), y: Math.round(n.position.y) } };
        // 容器节点：体内子节点聚合进 raw.blocks / raw.edges（含自动起点/终点边）
        if (CONTAINER_TYPES.includes(String(raw.type))) {
            const kids = allNodes.filter((c) => c.parentNode === n.id);
            const kidRaws = kids.map((k) => {
                const kr = JSON.parse(JSON.stringify(k.data.raw || {}));
                kr.meta = { ...(kr.meta || {}), position: { x: Math.round(k.position.x), y: Math.round(k.position.y) } };
                return kr;
            });
            const innerEdges = (edgesRef.value || [])
                .filter((e) => kids.some((k) => k.id === e.source) && kids.some((k) => k.id === e.target))
                .map((e) => ({ sourceNodeID: e.source, targetNodeID: e.target, sourcePortID: e.sourceHandle || "" }));
            raw.blocks = kidRaws.map((kr) => normalizeNodeInputs(kr, kidRaws, innerEdges));
            raw.edges = [...innerEdges, ...autoInnerEdges(raw, kidRaws, innerEdges)];
        }
        return normalizeNodeInputs(raw, topNodes, topEdges);
    });
    const edges = topEdges.map((e) => ({
        sourceNodeID: e.source,
        targetNodeID: e.target,
        // 分支出口端口（selector 多分支路由）
        sourcePortID: e.sourceHandle || "",
    }));
    return JSON.stringify({ ...canvasRest, nodes: raws, edges });
}

// ── 外部 canvas 变化 → 重新载入 ──
watch(() => props.canvas, (v) => { if (v != null) loadCanvas(v); }, { immediate: false });
onMounted(() => { if (props.canvas) loadCanvas(props.canvas); });

// ── 节点库拖拽落画布 ──
function onPaletteDragStart(evt, t) {
    evt.dataTransfer.setData("application/x-coze-node", t.id);
    evt.dataTransfer.setData("application/x-coze-palette", t.key); // 区分同类型预设（MCP/图片生成）
}
function onDrop(evt) {
    const typeID = evt.dataTransfer && evt.dataTransfer.getData("application/x-coze-node");
    if (!typeID) return;
    const pos = screenToFlowCoordinate({ x: evt.clientX, y: evt.clientY });
    // 落点容器检测（循环/批处理内拖入体内节点）
    const container = findContainerAt(pos);
    if (LOOP_ONLY_TYPES.includes(typeID) && !container) {
        Message.warning("终止/继续/设置变量节点只能拖入「循环」或「批处理」容器内");
        return;
    }
    if (CONTAINER_TYPES.includes(typeID) && container) {
        Message.warning("循环/批处理内不能再嵌套循环或批处理");
        return;
    }
    const key = evt.dataTransfer.getData("application/x-coze-palette");
    if (container) {
        // 体内节点：相对容器坐标 + parentNode 约束
        addCozeNode(typeID, { x: Math.round(pos.x - container.position.x), y: Math.round(pos.y - container.position.y) }, key, container.id);
    } else {
        addCozeNode(typeID, pos, key, "");
    }
}

// 落点所在容器（优先用渲染后实际尺寸 dimensions）
function findContainerAt(pos) {
    return (nodesRef.value || []).find((n) => {
        if (!CONTAINER_TYPES.includes(String(n.data.raw && n.data.raw.type))) return false;
        const w = (n.dimensions && n.dimensions.width) || CONTAINER_W;
        const h = (n.dimensions && n.dimensions.height) || CONTAINER_H;
        return pos.x >= n.position.x && pos.x <= n.position.x + w && pos.y >= n.position.y && pos.y <= n.position.y + h;
    });
}

function addCozeNode(typeID, pos, paletteKey, parentId) {
    // 同类型多预设（45=HTTP/MCP/图片生成）按 palette key 取 meta，避免 byID 覆盖
    const key = paletteKey || defaultKeyByID[typeID];
    const meta = byKey[key] || byKey[defaultKeyByID[typeID]] || {};
    if (!meta.key) return;
    if (meta.draggable === false) {
        // 开始/结束节点唯一：已存在则拒绝再加（体内节点不受此限）
        const exists = (nodesRef.value || []).some((n) => n.data.raw && n.data.raw.type === typeID);
        if (exists) { Message.warning(`「${meta.name}」节点只能有一个`); return; }
    }
    seq = seq || Date.now();
    const id = String(seq++);
    // 场景预设（MCP/图片生成共用 HTTP 类型）：优先取专属模板
    const preset = paletteKey && HTTP_PRESETS[PRESET_BY_KEY[paletteKey]];
    const tpl = JSON.parse(JSON.stringify(preset || DATA_TEMPLATES[typeID] || {}));
    // 节点标题固化到 data.nodeMeta（持久化 + 节点卡显示），未配置时用节点库名
    tpl.nodeMeta = { ...(tpl.nodeMeta || {}), title: (tpl.nodeMeta && tpl.nodeMeta.title) || meta.name };
    let node;
    if (parentId) {
        // 体内节点：parentNode + extent 约束在容器内拖动
        node = { id, type: "cozenode", parentNode: parentId, extent: "parent", position: pos, data: { raw: { id, type: typeID, meta: {}, data: tpl }, preset: paletteKey || "" } };
    } else if (CONTAINER_TYPES.includes(typeID)) {
        // 容器节点：固定大尺寸（嵌套画布）
        node = { id, type: "cozenode", position: pos, style: { width: `${CONTAINER_W}px`, height: `${CONTAINER_H}px` }, data: { raw: { id, type: typeID, meta: {}, data: tpl }, preset: paletteKey || "" } };
    } else {
        node = { id, type: "cozenode", position: pos, data: { raw: { id, type: typeID, meta: {}, data: tpl }, preset: paletteKey || "" } };
    }
    pushHistory();
    addNodes([node]);
    emitChange();
}

// ── 连线 ──
onConnect((conn) => {
    if (conn.source === conn.target) { Message.warning("不能连接到自身"); return; }
    // 跨容器连线禁止：体内节点只能与同一容器内的节点互连
    const sn = findNode(conn.source), tn = findNode(conn.target);
    const sp = sn && sn.parentNode, tp = tn && tn.parentNode;
    if ((sp || tp) && sp !== tp) { Message.warning("循环体内节点只能与同一容器内的节点连接"); return; }
    pushHistory();
    addEdges([{ id: `e-${conn.source}-${conn.target}-${Date.now()}`, source: conn.source, target: conn.target, type: "cozeedge" }]);
    emitChange();
});

// ── 节点点击 → 表单化配置抽屉（CozeNodeDrawer：按类型渲染表单 + 试运行）──
const selectedNode = ref(null);
const drawerVisible = ref(false);
const upstreamVars = ref([]); // 当前节点上游变量清单（引用下拉数据源）

function collectUpstreamVars(nodeID) {
    // 沿边反向 BFS 收集全部上游节点的 outputs（引用下拉分组展示）
    const incoming = {};
    (edgesRef.value || []).forEach((e) => { (incoming[e.target] ||= []).push(e.source); });
    const visited = new Set([nodeID]);
    const queue = [nodeID];
    const ups = [];
    while (queue.length) {
        const cur = queue.shift();
        (incoming[cur] || []).forEach((src) => {
            if (visited.has(src)) return;
            visited.add(src);
            queue.push(src);
            const n = findNode(src);
            const raw = n && n.data && n.data.raw;
            const outs = (raw && raw.data && raw.data.outputs) || [];
            const title2 = (raw && raw.meta && raw.meta.nodeMeta && raw.meta.nodeMeta.title) || typeName(raw && raw.type);
            ups.push({ nodeId: src, nodeName: title2, vars: outs.filter((o) => o && o.name) });
        });
    }
    return ups;
}

function onNodeClick({ node }) {
    selectedNode.value = node;
    upstreamVars.value = collectUpstreamVars(node.id);
    // 体内节点：附加「循环变量」分组（容器数组参数 + 中间变量，ref blockID = 容器 ID）
    if (node.parentNode) {
        const pn = findNode(node.parentNode);
        const praw = pn && pn.data && pn.data.raw;
        const pins = (praw && praw.data && praw.data.inputs) || {};
        const vars = [];
        (pins.inputParameters || []).forEach((p) => { if (p && p.name) vars.push({ name: p.name }); });
        (pins.variableParameters || []).forEach((p) => { if (p && p.name) vars.push({ name: p.name }); });
        if (vars.length) {
            const pnTitle = (praw.data.nodeMeta && praw.data.nodeMeta.title) || typeName(praw.type);
            upstreamVars.value = [...upstreamVars.value, { nodeId: node.parentNode, nodeName: `循环变量（${pnTitle}）`, vars }];
        }
    }
    // 容器节点（循环/批处理）：附加「体内节点」分组（体内节点 outputs，供输出映射 ref 选择）
    if (isContainer({ data: node.data })) {
        (nodesRef.value || [])
            .filter((c) => c.parentNode === node.id)
            .forEach((k) => {
                const kraw = k.data.raw;
                const kouts = ((kraw.data && kraw.data.outputs) || []).filter((o) => o && o.name);
                if (!kouts.length) return;
                const kTitle = (kraw.data.nodeMeta && kraw.data.nodeMeta.title) || typeName(kraw.type);
                upstreamVars.value = [...upstreamVars.value, { nodeId: k.id, nodeName: `体内：${kTitle}`, vars: kouts }];
            });
    }
    drawerVisible.value = true;
}

// 抽屉「应用」：表单序列化结果写回节点 raw（历史快照 + 变更通知）
function applyDrawer(raw) {
    const node = selectedNode.value;
    if (!node) return;
    pushHistory();
    node.data.raw = raw;
    Message.success("节点配置已更新（保存草稿后生效）");
    emitChange();
}

// ── 试运行：转发给父面板 handler（保存草稿 → CozeTestNode → 返回结果）──
function onNodeTestRun(input) {
    if (!props.testRunHandler || !props.workflowId || !selectedNode.value) {
        return Promise.reject(new Error("试运行不可用"));
    }
    return props.testRunHandler(props.workflowId, selectedNode.value.id, input);
}

// ── 自定义边几何（贝塞尔路径 + 删除按钮定位）──
function edgePath(props) {
    const p = getBezierPath({
        sourceX: props.sourceX, sourceY: props.sourceY,
        targetX: props.targetX, targetY: props.targetY,
        sourcePosition: props.sourcePosition, targetPosition: props.targetPosition,
    });
    return { path: p[0], labelX: p[1], labelY: p[2] };
}

// ── 变更通知 ──
onNodeDragStop(() => emitChange());
function emitChange() {
    emit("change", exportCanvas());
}

defineExpose({ exportCanvas });

// ── 历史快照（撤销/重做，≤50 步）──
const history = ref([]);
const redoStack = ref([]);
function pushHistory() {
    history.value.push(exportCanvas());
    if (history.value.length > 50) history.value.shift();
    redoStack.value = [];
}
function restore(snapshot) {
    setNodes([]);
    setEdges([]);
    loadCanvas(snapshot);
    emitChange();
}
function undo() {
    if (!history.value.length) { Message.info("没有可撤销的操作"); return; }
    redoStack.value.push(exportCanvas());
    restore(history.value.pop());
}
function redo() {
    if (!redoStack.value.length) { Message.info("没有可重做的操作"); return; }
    history.value.push(exportCanvas());
    restore(redoStack.value.pop());
}

// ── 复制 / 粘贴（选中节点深拷贝，偏移 40px，新 ID；容器连同体内节点整体复制）──
let clipboard = [];
function copySelected() {
    const selN = getSelectedNodes.value || [];
    if (!selN.length) return;
    clipboard = selN
        .filter((n) => {
            const t = n.data.raw && n.data.raw.type;
            if (t === "1" || t === "2") return false; // 开始/结束不复制
            return !n.parentNode; // 体内节点不单独复制（随容器整体复制）
        })
        .map((n) => JSON.parse(JSON.stringify({ raw: n.data.raw, x: n.position.x, y: n.position.y })));
    if (clipboard.length) Message.success(`已复制 ${clipboard.length} 个节点`);
}
function pasteClipboard() {
    if (!clipboard.length) return;
    pushHistory();
    seq = seq || Date.now();
    const newIds = {};
    clipboard.forEach((c) => { newIds[c.raw.id] = String(seq++); });
    const newNodes = [];
    clipboard.forEach((c) => {
        const raw = { ...c.raw, id: newIds[c.raw.id] };
        // 容器：体内 blocks 全部重编 ID（edges 保留结构，导出时重推自动边）
        if (CONTAINER_TYPES.includes(String(raw.type)) && Array.isArray(raw.blocks)) {
            raw.blocks = raw.blocks.map((b) => ({ ...b, id: String(seq++) }));
            raw.edges = (raw.edges || []).map((e) => ({ ...e }));
        }
        const vfNode = { id: raw.id, type: "cozenode", position: { x: c.x + 40, y: c.y + 40 }, data: { raw } };
        newNodes.push(vfNode);
        // 体内节点重建为 Vue Flow 子节点（相对坐标沿用）
        if (CONTAINER_TYPES.includes(String(raw.type)) && Array.isArray(raw.blocks)) {
            raw.blocks.forEach((b) => {
                newNodes.push({
                    id: b.id, type: "cozenode", parentNode: raw.id, extent: "parent",
                    position: (b.meta && b.meta.position) || { x: 40, y: 64 },
                    data: { raw: b },
                });
            });
        }
    });
    addNodes(newNodes);
    // 内部引用（粘贴簇内互连）重映射到新节点；外部引用断开
    // 完整 old→new 映射：顶层节点 + 容器体内节点
    const idMap = { ...newIds };
    newNodes.forEach((n) => {
        const raw = n.data.raw;
        if (n.parentNode && !idMap[raw.id]) idMap[raw.id] = raw.id; // 体内新 ID 已是新的
    });
    clipboard.forEach((c) => {
        if (CONTAINER_TYPES.includes(String(c.raw.type)) && Array.isArray(c.raw.blocks)) {
            const container = newNodes.find((n) => n.id === newIds[c.raw.id]);
            (c.raw.blocks || []).forEach((b, bi) => {
                idMap[b.id] = container.data.raw.blocks[bi].id;
            });
        }
    });
    const ids = new Set(Object.keys(idMap));
    const remap = (content) => {
        if (content && content.blockID && idMap[content.blockID]) content.blockID = idMap[content.blockID];
    };
    newNodes.forEach((n) => {
        ((n.data.raw.data && n.data.raw.data.inputs && n.data.raw.data.inputs.inputParameters) || []).forEach((p) => {
            const v = p.input && p.input.value;
            if (v && v.type === "ref") remap(v.content);
        });
    });
    emitChange();
}

// ── 删除 ──
function deleteSelected() {
    const selN = getSelectedNodes.value || [];
    const selE = getSelectedEdges.value || [];
    if (!selN.length && !selE.length) return;
    pushHistory();
    // 开始/结束节点不可删
    const del = selN.filter((n) => {
        const t = n.data.raw && n.data.raw.type;
        if (t === "1" || t === "2") { Message.warning("开始/结束节点不可删除"); return false; }
        return true;
    });
    // 容器删除连带体内节点（Vue Flow 不会自动级联删除 children）
    const kids = [];
    del.forEach((n) => {
        if (CONTAINER_TYPES.includes(String(n.data.raw && n.data.raw.type))) {
            (nodesRef.value || []).forEach((c) => { if (c.parentNode === n.id) kids.push(c); });
        }
    });
    if (del.length || kids.length) removeNodes([...del, ...kids]);
    if (selE.length) removeEdges(selE);
    selectedNode.value = null;
    drawerVisible.value = false;
    emitChange();
}
function removeEdge(id) {
    pushHistory();
    removeEdges([id]);
    emitChange();
}
function onKeydown(e) {
    if (isEditing()) return;
    if (e.key === "Delete" || e.key === "Backspace") deleteSelected();
    else if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "z" && !e.shiftKey) { e.preventDefault(); undo(); }
    else if ((e.ctrlKey || e.metaKey) && (e.key.toLowerCase() === "y" || (e.key.toLowerCase() === "z" && e.shiftKey))) { e.preventDefault(); redo(); }
    else if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "c") { e.preventDefault(); copySelected(); }
    else if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "v") { e.preventDefault(); pasteClipboard(); }
}
function isEditing() {
    const el = document.activeElement;
    return el && (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable);
}
onMounted(() => window.addEventListener("keydown", onKeydown));
onUnmounted(() => window.removeEventListener("keydown", onKeydown));
</script>

<template>
  <div class="coze-canvas-wrap">
    <!-- 节点库（拖拽源） -->
    <aside class="coze-palette">
      <div class="palette-title">节点库（拖到画布）</div>
      <div
        v-for="t in NODE_TYPES"
        :key="t.key"
        class="palette-item"
        draggable="true"
        @dragstart="onPaletteDragStart($event, t)"
      >
        <span class="dot" :style="{ background: t.color }" />
        {{ t.name }}
      </div>
      <div class="palette-tip">Delete 删除 · Ctrl+C/V 复制粘贴 · Ctrl+Z 撤销</div>
    </aside>

    <!-- 画布 -->
    <div class="coze-flow-host" @drop="onDrop" @dragover.prevent>
      <VueFlow :id="FLOW_ID" :min-zoom="0.2" :max-zoom="2" :edges-updatable="true"
        @node-click="onNodeClick" @pane-click="selectedNode = null; drawerVisible = false">
        <Background :gap="18" />
        <Controls />
        <MiniMap pannable zoomable />

        <template #node-cozenode="nodeProps">
          <div class="coze-node" :class="{ selected: nodeProps.selected, 'coze-container': isContainer(nodeProps) }">
            <div class="node-bar" :style="{ background: typeColor(nodeProps.data.raw && nodeProps.data.raw.type, nodeProps.data.preset) }" />
            <div class="node-head">
              <span class="node-title">
                {{ (nodeProps.data.raw && nodeProps.data.raw.data && nodeProps.data.raw.data.nodeMeta && nodeProps.data.raw.data.nodeMeta.title) || (nodeProps.data.raw && nodeProps.data.raw.meta && nodeProps.data.raw.meta.nodeMeta && nodeProps.data.raw.meta.nodeMeta.title) || typeName(nodeProps.data.raw && nodeProps.data.raw.type) }}
              </span>
            </div>
            <div class="node-sub">{{ nodeSubTitle(nodeProps) }}</div>
            <Handle type="target" position="top" class="coze-handle" />
            <!-- 分支节点多出口：第 1 分支=true、第 N 分支=true_N、else=false（normalizePorts 语义） -->
            <template v-if="nodeProps.data.raw && nodeProps.data.raw.type === '8'">
              <Handle
                v-for="(p, pi) in selectorPorts(nodeProps)"
                :key="p.id"
                :id="p.id"
                type="source" position="bottom"
                :style="{ left: p.left, background: p.color }"
                class="coze-handle"
              />
            </template>
            <!-- 意图识别多出口：branch_0..N（每意图一个）→ default（官方语义） -->
            <template v-else-if="nodeProps.data.raw && nodeProps.data.raw.type === '22'">
              <Handle
                v-for="(p, pi) in intentPorts(nodeProps)"
                :key="p.id"
                :id="p.id"
                type="source" position="bottom"
                :style="{ left: p.left, background: p.color }"
                class="coze-handle"
              />
            </template>
            <Handle v-else type="source" position="bottom" class="coze-handle" />
          </div>
        </template>

        <!-- 自定义边：悬停显示删除按钮（对标 Dify 边交互） -->
        <template #edge-cozeedge="edgeProps">
          <BaseEdge :id="edgeProps.id" :path="edgePath(edgeProps).path" :style="edgeProps.style" class="coze-edge" />
          <EdgeLabelRenderer>
            <button
              class="edge-del"
              :style="{ transform: `translate(-50%,-50%) translate(${edgePath(edgeProps).labelX}px,${edgePath(edgeProps).labelY}px)` }"
              title="删除连线"
              @click.stop="removeEdge(edgeProps.id)"
            >×</button>
          </EdgeLabelRenderer>
        </template>
      </VueFlow>
    </div>

    <!-- 节点配置抽屉（表单化 + 单节点试运行） -->
    <CozeNodeDrawer
      v-model:visible="drawerVisible"
      :node="selectedNode"
      :upstream-vars="upstreamVars"
      :test-run-handler="onNodeTestRun"
      :models-loader="modelsLoader"
      :mcp-servers-loader="mcpServersLoader"
      :mcp-tools-loader="mcpToolsLoader"
      :image-gen-loader="imageGenLoader"
      :kb-loader="kbLoader"
      :kb-create-handler="kbCreateHandler"
      :db-loader="dbLoader"
      :wf-loader="wfLoader"
      @apply="applyDrawer"
    />
  </div>
</template>

<style scoped>
.coze-canvas-wrap {
  display: flex;
  height: 100%;
  min-height: 420px;
  gap: 8px;
}
.coze-palette {
  width: 132px;
  flex: none;
  border: 1px solid var(--color-border-2, #e5e6eb);
  border-radius: 8px;
  padding: 8px;
  overflow-y: auto;
  background: var(--color-bg-2, #fff);
}
.palette-title { font-size: 12px; color: var(--color-text-3, #86909c); margin-bottom: 6px; }
.palette-item {
  display: flex; align-items: center; gap: 6px;
  font-size: 13px; padding: 5px 6px; border-radius: 6px;
  cursor: grab; user-select: none;
}
.palette-item:hover { background: var(--color-fill-2, #f2f3f5); }
.palette-item .dot { width: 8px; height: 8px; border-radius: 50%; flex: none; }
.palette-tip { margin-top: 10px; font-size: 11px; color: var(--color-text-4, #c9cdd4); line-height: 1.6; }

.coze-flow-host {
  flex: 1;
  min-width: 0;
  border: 1px solid var(--color-border-2, #e5e6eb);
  border-radius: 8px;
  overflow: hidden;
  background: var(--color-bg-1, #fafafa);
}

/* 自定义节点卡 */
.coze-node {
  width: 190px;
  background: var(--color-bg-2, #fff);
  border: 1px solid var(--color-border-2, #e5e6eb);
  border-radius: 8px;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.06);
  overflow: hidden;
  position: relative;
  padding: 8px 10px 8px 16px;
  cursor: pointer;
}
.coze-node.selected { border-color: #165dff; box-shadow: 0 0 0 2px rgba(22, 93, 255, 0.2); }
/* 循环/批处理容器：撑满节点尺寸（node.style 定宽高），虚线边界 + 淡紫底，体内节点叠放其中 */
.coze-node.coze-container {
  width: 100%;
  height: 100%;
  background: rgba(114, 46, 209, 0.04);
  border: 1.5px dashed rgba(114, 46, 209, 0.45);
  border-radius: 12px;
  box-shadow: none;
}
.coze-node.coze-container.selected { border-color: #722ed1; box-shadow: 0 0 0 2px rgba(114, 46, 209, 0.2); }
.node-bar { position: absolute; left: 0; top: 0; bottom: 0; width: 4px; }
.node-head { display: flex; align-items: center; gap: 6px; }
.node-title { font-size: 13px; font-weight: 600; color: var(--color-text-1, #1d2129); }
.node-sub { font-size: 11px; color: var(--color-text-3, #86909c); margin-top: 2px; }
.coze-handle { width: 9px; height: 9px; background: #165dff; border: 2px solid #fff; }

/* 自定义边 + 删除按钮（EdgeLabelRenderer 渲染于 portal 层，按钮常显半透明，悬停高亮） */
.coze-edge { stroke: #94a3b8; stroke-width: 2; }
.edge-del {
  position: absolute;
  z-index: 10;
  width: 16px; height: 16px;
  border-radius: 50%;
  border: 1px solid var(--color-border-2, #e5e6eb);
  background: var(--color-bg-2, #fff);
  color: var(--color-text-2, #4e5969);
  font-size: 11px; line-height: 1;
  cursor: pointer;
  opacity: 0.4;
  pointer-events: all;
  transition: all 0.15s;
}
.edge-del:hover { opacity: 1; background: #f53f3f; color: #fff; border-color: #f53f3f; }

.raw-editor {
  width: 100%;
  font-family: Consolas, Monaco, monospace;
  font-size: 12px;
  line-height: 1.5;
  border: 1px solid var(--color-border-2, #e5e6eb);
  border-radius: 6px;
  padding: 8px;
  resize: vertical;
  background: var(--color-bg-2, #fff);
  color: var(--color-text-1, #1d2129);
}
.drawer-body { display: flex; flex-direction: column; gap: 12px; }
.field label { display: block; font-size: 12px; color: var(--color-text-2, #4e5969); margin-bottom: 4px; }
.drawer-actions { display: flex; justify-content: space-between; }
</style>
