<script setup>
// ═════════════════════════════════════════════════════════════════
// AI Flow 节点配置抽屉（表单化，对标 Dify 的节点配置面板）。
//   - 按节点类型渲染结构化表单（开始/结束/大模型/代码/输出），其余类型
//     回退 JSON 编辑
//   - 变量引用控件：引用上游节点输出（block-output ref）或常量文本，
//     上游变量清单由画布收集后经 props 传入
//   - 表单 ↔ coze 画布 JSON 双向转换（apply 时序列化回 raw.data 结构，
//     字段形态与后端 nodes/* adaptor 期望一致，见 workflow_llm_test.go）
//   - 单节点试运行：输入按节点已声明参数生成表单，调用后端 NodeDebug
//     （CozeTestNode），展示状态/耗时/输出/失败原因
// ═════════════════════════════════════════════════════════════════
import { computed, ref, watch } from "vue";
import { Message } from "@arco-design/web-vue";
import { IconPlayCircle } from "@arco-design/web-vue/es/icon";

const props = defineProps({
    visible: { type: Boolean, default: false },
    node: { type: Object, default: null },        // vue-flow 节点（data.raw 为画布节点 JSON）
    upstreamVars: { type: Array, default: () => [] }, // [{nodeId, nodeName, vars: [{name,type}]}]
    testRunHandler: { type: Function, default: null }, // async (input) => CozeRunResult（父面板：保存草稿 + CozeTestNode）
    modelsLoader: { type: Function, default: null },   // async () => [{id, name}]（CozeListModels，模型下拉）
    mcpServersLoader: { type: Function, default: null }, // async () => [{name, transport, url, headers, enabled}]（ListMCPServers）
    mcpToolsLoader: { type: Function, default: null },   // async (serverName) => [{name, description}]（CozeListMCPTools）
    imageGenLoader: { type: Function, default: null },   // async () => {configured, baseURL, model, apiKey}（CozeGetImageGenConfig）
    kbLoader: { type: Function, default: null },         // async () => [{id, name, description}]（CozeListKnowledgeBases）
    kbCreateHandler: { type: Function, default: null },  // async (name, content) => {id, name}（CozeCreateKnowledge）
    wfLoader: { type: Function, default: null },         // async () => [{id, name, desc}]（CozeListWorkflows，子流程节点）
});
const emit = defineEmits(["update:visible", "apply"]);

// ── 节点类型元数据（与 CozeCanvas 对齐）──
const TYPE_NAMES = { "1": "开始", "2": "结束", "3": "大模型", "4": "插件", "5": "代码", "6": "知识库检索", "8": "分支", "9": "工作流", "12": "SQL自定义", "13": "输出", "15": "文本处理", "19": "终止循环", "20": "设置变量", "21": "循环", "22": "意图识别", "27": "知识库写入", "28": "批处理", "29": "继续循环", "31": "注释", "32": "聚合", "42": "更新数据", "43": "查询数据", "44": "删除数据", "45": "HTTP 请求", "46": "新增数据", "58": "JSON 序列化", "59": "JSON 反序列化", "60": "知识库删除" };
const tplHint = "{{变量名}}";     // 模板占位提示（插值内不能直接写花括号）
const tplExample = "{{query}}";
const nodeType = computed(() => (props.node?.data?.raw?.type) || "");
const typeName = computed(() => TYPE_NAMES[nodeType.value] || `节点(${nodeType.value})`);
const formMode = computed(() => {
    if (["1", "2", "3", "5", "13", "8", "32", "45", "6", "22", "58", "59", "27", "15", "12", "9", "60", "31", "43", "46", "42", "44", "21", "28", "19", "29", "20"].includes(nodeType.value)) return nodeType.value;
    return "json"; // 其余类型回退 JSON 编辑
});

// ── 模型下拉（父面板注入 modelsLoader → CozeListModels）──
const modelOptions = ref([]);   // [{id, name}]
const modelLoading = ref(false);
watch(() => props.visible, async (v) => {
    if (!v || modelOptions.value.length || !props.modelsLoader) return;
    modelLoading.value = true;
    try { modelOptions.value = (await props.modelsLoader()) || []; } catch { /* 下拉不可用时保持空 */ }
    modelLoading.value = false;
});
// 当前选中模型（llmParam.modelType ↔ 下拉联动）
const selectedModelID = computed({
    get: () => String(llmForm.value.modelType || ""),
    set: (id) => {
        const m = modelOptions.value.find((x) => x.id === String(id));
        llmForm.value.modelType = id;
        if (m) llmForm.value.modleName = m.name;
    },
});

// ── 表单状态（每次打开按 raw 重建）──
const title = ref("");
const outputs = ref([]);        // [{name, type, description, required}]
const endMappings = ref([]);    // [{name, ref: {mode, blockID, varName, literal}}]
const llmForm = ref({ system: "", user: "", temperature: 0.5, responseFormat: 0, modelType: "", modleName: "" });
const codeForm = ref({ language: 3, code: "", inputs: [] });
const outputForm = ref({ contentMode: "literal", contentLiteral: "", contentRefBlock: "", contentRefVar: "", streaming: true });
const httpForm = ref({ method: "GET", url: "", bodyType: "none", bodyJSON: "", headers: [], bodyFields: [], bodyRaw: false });  // HTTP(45)；bodyFields: [{key, mode, ref, literal}]（JSON 请求体键值对，免写 JSON）；bodyRaw: 高级模式（复杂 JSON 直写）
// ── MCP 级联选择（HTTP 预设节点：data.preset==="mcp"）──
const isMCPNode = computed(() => props.node?.data?.preset === "mcp");
const mcpServers = ref([]);   // 设置中的 MCP 服务器 [{name, transport, url, headers, enabled}]
const mcpTools = ref([]);     // 所选服务器的工具 [{name, description}]
const mcpLoading = ref(false);
const mcpForm = ref({ server: "", tool: "", argsRows: [] });   // argsRows: [{key, mode: "ref"|"literal", ref, literal}]（键值对参数，免写 JSON）
// ── 图片生成节点（HTTP 预设：data.preset==="imagegen"，用设置中的图片生成模型）──
const isImageGenNode = computed(() => props.node?.data?.preset === "imagegen");
const igModel = ref({ configured: false, baseURL: "", model: "" }); // 设置中的模型（只读展示）
const igForm = ref({ prompt: "", size: "1024x1024" });
// ── 参数提取器（HTTP 预设：data.preset==="extractor"，对标 Dify Parameter Extractor）──
// 运行态仍复用内核 HTTPRequester（表单生成文本提取 API 请求），只做提取不联网时回退字面量
const isExtractorNode = computed(() => props.node?.data?.preset === "extractor");
const exForm = ref({ text: "", fields: [] });   // fields: [{name, desc, type, required}]
const branchForm = ref({ branches: [] });   // 分支(8)：[{conditions: [{left, operator, right}]}]
const aggForm = ref({ groups: [] });        // 聚合(32)：[{name, ref}]
// ── 知识库（检索 6 / 写入 27 共用数据源；kbLoader → CozeListKnowledgeBases）──
const kbList = ref([]);                     // [{id, name, description}]
const kbLoading = ref(false);
const kbForm = ref({ query: { mode: "literal", blockID: "", varName: "", literal: "" }, ids: [], topK: 5, minScore: 0.5, strategy: 1 });
const kbWriteForm = ref({ content: { mode: "literal", blockID: "", varName: "", literal: "" }, id: "" });
const kbCreating = ref(false);
const kbNewName = ref("");
const kbNewContent = ref("");
// ── 意图识别(22)：意图列表编辑 + query 输入 ──
const intentForm = ref({ query: { mode: "literal", blockID: "", varName: "", literal: "" }, intents: [""] });
// ── JSON 序列化(58)/反序列化(59)：单输入引用 + 输出类型 ──
const jsonIOForm = ref({ input: { mode: "ref", blockID: "", varName: "", literal: "" }, outType: "object" });
// ── 文本处理(15)：模板（{{变量}} 插值）+ 输入映射 + 数组拼接符（对标 Dify 模板转换）──
const tplForm = ref({ tpl: "", concatChar: "", inputs: [] });
// ── SQL 自定义(12)：数据源选择 + SQL 模板 + 输入映射 + 输出列 ──
const sqlForm = ref({ dbID: "", sql: "", inputs: [] });
const dbList = ref([]);         // 数据源列表 [{id, name, desc, fields}]（dbLoader）
const dbLoading = ref(false);
// ── 工作流(9 SubWorkflow)：选择已有流程 + 输入映射（内核运行时自动加载子画布）──
const subwfForm = ref({ wfID: "", inputs: [] });
const wfList = ref([]);         // 可选流程 [{id, name, desc}]（wfLoader）
const wfLoading = ref(false);
// ── 知识库删除(60)：知识库下拉（kbLoader）+ documentID 输入映射 ──
const kbDelForm = ref({ kbID: "", doc: { mode: "literal", blockID: "", varName: "", literal: "" } });
// ── 注释(31)：纯画布标注文本（内核跳过执行）──
const commentForm = ref("");
// ── 数据库四件套(42/43/44/46)：dsID=外部表（负 ID 字符串）+ WHERE 条件行 + 字段值映射行 + limit ──
const dbNodeForm = ref({ dsID: "", conditions: [], logic: "AND", fields: [], limit: 0 });
const DB_OPERATORS = [
    { value: "EQUAL", label: "等于" }, { value: "NOT_EQUAL", label: "不等于" },
    { value: "GREATER", label: "大于" }, { value: "LESS", label: "小于" },
    { value: "GREATER_OR_EQUAL", label: "大于等于" }, { value: "LESS_OR_EQUAL", label: "小于等于" },
    { value: "LIKE", label: "包含（LIKE）" }, { value: "NOT_LIKE", label: "不包含" },
    { value: "IN", label: "在列表（IN）" }, { value: "NOT_IN", label: "不在列表" },
    { value: "IS_NULL", label: "为空" }, { value: "IS_NOT_NULL", label: "不为空" },
];
// 数据库四件套节点类型判定（loaders/表单模板复用）
const isDBNode = computed(() => ["43", "46", "42", "44"].includes(nodeType.value));
// ── 循环/批处理容器(21/28)：循环类型 + 数组输入映射 + 中间变量 + 输出 ──
const loopForm = ref({ loopType: "array", loopCount: 3, arrays: [], vars: [] });
const batchForm = ref({ batchSize: 100, concurrentSize: 10, arrays: [] });
const LOOP_TYPES = [
    { value: "array", label: "按数组（逐元素循环）" },
    { value: "count", label: "按指定次数" },
    { value: "infinite", label: "无限循环（需体内「终止循环」）" },
];
const VAR_TYPES = ["string", "integer", "number", "boolean", "object", "list"];
// ── 设置变量(20)：体内专用，left=中间变量 + right=值映射 ──
const setvarForm = ref({ rows: [] });
const rawText = ref("");

// HTTP 请求方法选项
const HTTP_METHODS = ["GET", "POST", "PUT", "DELETE", "PATCH"];
// 分支条件操作符（vo.OperatorType：1=Equal 2=NotEqual 8=Contain 9=NotContain 10=Empty 11=NotEmpty 14=GreaterThan 16=LessThan）
const OPERATORS = [
    { value: 1, label: "等于" }, { value: 2, label: "不等于" },
    { value: 8, label: "包含" }, { value: 9, label: "不包含" },
    { value: 10, label: "为空" }, { value: 11, label: "不为空" },
    { value: 14, label: "大于" }, { value: 16, label: "小于" },
];

// ── 引用/常量 双形态工具（coze BlockInput 形态）──
function refToForm(input) {
    const v = input && input.value;
    if (v && v.type === "ref" && v.content) {
        return { mode: "ref", blockID: v.content.blockID || "", varName: v.content.name || "", literal: "" };
    }
    return { mode: "literal", blockID: "", varName: "", literal: typeof (v && v.content) === "string" ? v.content : "" };
}
function formToRef(f, inputType) {
    if (f.mode === "ref" && f.blockID && f.varName) {
        return { type: inputType, value: { type: "ref", content: { blockID: f.blockID, name: f.varName, source: "block-output" } } };
    }
    return { type: inputType, value: { type: "literal", content: f.literal } };
}

// ── 打开时按类型解析 raw → 表单 ──
watch(() => props.visible, (v) => {
    if (!v || !props.node) return;
    const raw = JSON.parse(JSON.stringify(props.node.data.raw || {}));
    const data = raw.data || {};
    const meta = raw.meta || {};
    // Kaelvoris 补丁（2026-10）：标题固化为 data.nodeMeta.title（addCozeNode 写入），兼容旧 meta.nodeMeta 位置
    title.value = (data.nodeMeta && data.nodeMeta.title) || (meta.nodeMeta && meta.nodeMeta.title) || typeName.value;
    outputs.value = JSON.parse(JSON.stringify(data.outputs || []));
    endMappings.value = [];
    llmForm.value = { system: "", user: "", temperature: 0.5, responseFormat: 0, modelType: "", modleName: "" };
    codeForm.value = { language: 3, code: "", inputs: [] };
    outputForm.value = { contentMode: "literal", contentLiteral: "", contentRefBlock: "", contentRefVar: "", streaming: true };
    httpForm.value = { method: "GET", url: "", bodyType: "none", bodyJSON: "", headers: [] };
    branchForm.value = { branches: [] };
    aggForm.value = { groups: [] };
    mcpForm.value = { server: "", tool: "", argsRows: [] };
    mcpTools.value = [];
    httpForm.value = { method: "GET", url: "", bodyType: "none", bodyJSON: "", headers: [], bodyFields: [], bodyRaw: false };
    kbForm.value = { query: { mode: "literal", blockID: "", varName: "", literal: "" }, ids: [], topK: 5, minScore: 0.5, strategy: 1 };
    kbWriteForm.value = { content: { mode: "literal", blockID: "", varName: "", literal: "" }, id: "" };
    intentForm.value = { query: { mode: "literal", blockID: "", varName: "", literal: "" }, intents: [""] };
    jsonIOForm.value = { input: { mode: "ref", blockID: "", varName: "", literal: "" }, outType: "object" };
    tplForm.value = { tpl: "", concatChar: "", inputs: [] };
    sqlForm.value = { dbID: "", sql: "", inputs: [] };
    subwfForm.value = { wfID: "", inputs: [] };
    kbDelForm.value = { kbID: "", doc: { mode: "literal", blockID: "", varName: "", literal: "" } };
    commentForm.value = "";
    dbNodeForm.value = { dsID: "", conditions: [], logic: "AND", fields: [], limit: 0 };
    loopForm.value = { loopType: "array", loopCount: 3, arrays: [], vars: [], out: { mode: "ref", blockID: "", varName: "", literal: "" } };
    batchForm.value = { batchSize: 100, concurrentSize: 10, arrays: [], out: { mode: "ref", blockID: "", varName: "", literal: "" } };
    setvarForm.value = { rows: [] };
    exForm.value = { text: "", fields: [] };
    kbNewName.value = "";
    kbNewContent.value = "";
    rawText.value = JSON.stringify(data, null, 2);
    // MCP 预设节点：拉取服务器列表并回显（data.mcpRef 保存上次选择）
    if (isMCPNode.value) {
        loadMCPServers();
        const mr = data.mcpRef || {};
        mcpForm.value.server = typeof mr.server === "string" ? mr.server : "";
        mcpForm.value.tool = typeof mr.tool === "string" ? mr.tool : "";
        // 从请求体回显 arguments（tools/call.params.arguments → 键值对行）
        try {
            const bj = JSON.parse(httpForm.value.bodyJSON || (inputs.body && inputs.body.bodyData && inputs.body.bodyData.json) || "{}");
            mcpForm.value.argsRows = mcpRowsFromArgs(bj.params && bj.params.arguments || {});
        } catch { mcpForm.value.argsRows = []; }
        if (mcpForm.value.server) loadMCPTools(true);
    }
    // 图片生成预设节点：拉取设置中的图片生成模型，并从请求体回显提示词/尺寸
    if (isImageGenNode.value) loadImageGenSetup(httpForm.value.bodyJSON, inputs);
    // 参数提取器预设节点：回显提取字段（data.exRef 固化：fields + 文本来源）
    if (isExtractorNode.value) loadExtractorSetup(data);
    // 知识库检索/写入：拉取知识库列表（CozeListKnowledgeBases）
    if (nodeType.value === "6" || nodeType.value === "27") loadKnowledgeBases();
    // SQL 自定义：拉取数据源列表（CozeListDatabases，设置 → 数据源中管理）
    if (nodeType.value === "12") loadDatabases();
    // 数据库四件套：拉取数据源列表（表单中过滤出已配置表名的连接）
    if (isDBNode.value) loadDatabases();
    // 工作流（子流程）：拉取流程列表（CozeListWorkflows）
    if (nodeType.value === "9") loadWorkflows();
    // 知识库删除：拉取知识库列表
    if (nodeType.value === "60") loadKnowledgeBases();

    const inputs = data.inputs || {};
    switch (formMode.value) {
        case "2": // 结束：inputParameters → 输出映射
            endMappings.value = (inputs.inputParameters || []).map((p) => ({
                name: p.name || "",
                ref: refToForm(p.input),
            }));
            break;
        case "3": { // 大模型：llmParam 数组按 name 分发
            const lp = {};
            (inputs.llmParam || []).forEach((p) => {
                if (p.name && p.input && p.input.value) lp[p.name] = p.input.value.content;
            });
            llmForm.value = {
                system: typeof lp.systemPrompt === "string" ? lp.systemPrompt : "",
                user: typeof lp.prompt === "string" ? lp.prompt : "",
                temperature: Number(lp.temperature ?? 0.5),
                responseFormat: Number(lp.responseFormat ?? 0),
                modelType: typeof lp.modelType === "string" || typeof lp.modelType === "number" ? String(lp.modelType) : "",
                modleName: typeof lp.modleName === "string" ? lp.modleName : "",
            };
            break;
        }
        case "5": { // 代码：code/language + inputParameters 输入映射
            codeForm.value = {
                language: inputs.language === 5 ? 5 : 3,
                code: inputs.code || "",
                inputs: (inputs.inputParameters || []).map((p) => ({ name: p.name || "", ref: refToForm(p.input) })),
            };
            break;
        }
        case "8": { // 分支：branches[i].condition.conditions
            branchForm.value = {
                branches: (inputs.branches || []).map((b) => ({
                    conditions: ((b.condition && b.condition.conditions) || []).map((cd) => ({
                        left: refToForm(cd.left && cd.left.input),
                        operator: cd.operator ?? 1,
                        right: refToForm(cd.right && cd.right.input),
                    })),
                })),
            };
            break;
        }
        case "32": { // 聚合：mergeGroups []*Param
            aggForm.value = {
                groups: (inputs.mergeGroups || []).map((g) => ({ name: g.name || "", ref: refToForm(g.input) })),
            };
            break;
        }
        case "45": { // HTTP：apiInfo/headers/body（JSON 体扁平对象还原键值对，嵌套走高级模式）
            const ai = inputs.apiInfo || {};
            const body = inputs.body || {};
            const bj = (body.bodyData && body.bodyData.json) || "";
            const fields = body.bodyType === "json" ? httpFieldsFromBody(bj) : null;
            httpForm.value = {
                method: ai.method || "GET",
                url: ai.url || "",
                bodyType: body.bodyType || "none",
                bodyJSON: bj,
                headers: (inputs.headers || []).map((h) => ({
                    name: h.name || "",
                    ref: refToForm(h.input),
                })),
                bodyFields: fields || [],
                bodyRaw: body.bodyType === "json" && fields === null, // 嵌套/非对象 JSON → 高级模式
            };
            break;
        }
        case "13": { // 输出：content BlockInput + streamingOutput
            const f = refToForm(inputs.content);
            outputForm.value = {
                contentMode: f.mode,
                contentLiteral: f.literal,
                contentRefBlock: f.blockID,
                contentRefVar: f.varName,
                streaming: !!inputs.streamingOutput,
            };
            break;
        }
        case "6": { // 知识库检索：datasetParam 策略 + Query 输入
            const dp = inputs.datasetParam || [];
            const find = (n) => dp.find((x) => x.name === n) || {};
            const num = (n, dft) => {
                const v = find(n).input && find(n).input.value && find(n).input.value.content;
                return v == null ? dft : Number(v);
            };
            const idsInp = find("datasetList");
            const qInp = (inputs.inputParameters || []).find((p) => p.name === "Query");
            kbForm.value = {
                query: refToForm(qInp && qInp.input),
                ids: idsInp.input && idsInp.input.value && Array.isArray(idsInp.input.value.content) ? idsInp.input.value.content.map(String) : [],
                topK: num("topK", 5),
                minScore: num("minScore", 0.5),
                strategy: num("strategy", 1),
            };
            break;
        }
        case "22": { // 意图识别：intents 列表 + query 输入
            const qInp = (inputs.inputParameters || []).find((p) => p.name === "query");
            intentForm.value = {
                query: refToForm(qInp && qInp.input),
                intents: (inputs.intents || []).map((it) => it.name || ""),
            };
            if (!intentForm.value.intents.length) intentForm.value.intents = [""];
            break;
        }
        case "27": { // 知识库写入：内容 + 目标知识库（单选）
            const cInp = (inputs.inputParameters || []).find((p) => p.name === "knowledge");
            const dp = inputs.datasetParam || [];
            const idsInp = dp.find((x) => x.name === "datasetList");
            kbWriteForm.value = {
                content: refToForm(cInp && cInp.input),
                id: idsInp && idsInp.value && Array.isArray(idsInp.value.content) && idsInp.value.content.length ? String(idsInp.value.content[0]) : "",
            };
            break;
        }
        case "58": { // JSON 序列化：input 引用
            const iInp = (inputs.inputParameters || [])[0];
            jsonIOForm.value = { ...(jsonIOForm.value), input: refToForm(iInp && iInp.input) };
            break;
        }
        case "59": { // JSON 反序列化：input 引用 + 输出类型
            const iInp = (inputs.inputParameters || [])[0];
            const outT = (outputs.value || [])[0];
            jsonIOForm.value = {
                input: refToForm(iInp && iInp.input),
                outType: (outT && outT.type) || "object",
            };
            break;
        }
        case "15": { // 文本处理：concatParams（concatResult=模板 / arrayItemConcatChar=拼接符 / 其余=输入映射）
            const cp = inputs.concatParams || [];
            const tplP = cp.find((x) => x.name === "concatResult");
            const charP = cp.find((x) => x.name === "arrayItemConcatChar");
            tplForm.value = {
                tpl: tplP && tplP.input && typeof tplP.input.value === "object" ? String(tplP.input.value.content ?? "") : "",
                concatChar: charP && charP.input && typeof charP.input.value === "object" ? String(charP.input.value.content ?? "") : "",
                inputs: cp
                    .filter((x) => x.name !== "concatResult" && x.name !== "arrayItemConcatChar")
                    .map((p) => ({ name: p.name || "", ref: refToForm(p.input) })),
            };
            break;
        }
        case "12": { // SQL 自定义：数据源 + sql 模板 + 输入映射
            const ds = (inputs.databaseInfoList || [])[0];
            sqlForm.value = {
                dbID: ds && ds.databaseInfoID ? String(ds.databaseInfoID) : "",
                sql: typeof inputs.sql === "string" ? inputs.sql : "",
                inputs: (inputs.inputParameters || []).map((p) => ({ name: p.name || "", ref: refToForm(p.input) })),
            };
            break;
        }
        case "9": { // 工作流（子流程）：workflowId + 输入映射
            subwfForm.value = {
                wfID: typeof inputs.workflowId === "string" ? inputs.workflowId : "",
                inputs: (inputs.inputParameters || []).map((p) => ({ name: p.name || "", ref: refToForm(p.input) })),
            };
            break;
        }
        case "60": { // 知识库删除：datasetParam[0] 知识库 + documentID 输入映射
            const dp = (inputs.datasetParam || [])[0] || {};
            const dl = dp.input && dp.input.value && Array.isArray(dp.input.value.content) ? dp.input.value.content : [];
            const docP = (inputs.inputParameters || []).find((p) => p.name === "documentID");
            kbDelForm.value = {
                kbID: dl.length ? String(dl[0]) : "",
                doc: refToForm(docP && docP.input),
            };
            break;
        }
        case "31": { // 注释：note 文本（对象形态取 content/content.text，字符串直接用）
            const c = inputs.comment || {};
            const n = c.note;
            if (typeof n === "string") commentForm.value = n;
            else if (n && typeof n === "object") commentForm.value = String(n.content ?? n.text ?? "");
            break;
        }
        case "43": { // 查询数据：外部表 + 条件组 + limit
            const ds = (inputs.databaseInfoList || [])[0];
            const sp = inputs.selectParam || {};
            dbNodeForm.value = {
                dsID: ds && ds.databaseInfoID ? String(ds.databaseInfoID) : "",
                conditions: parseDBConditions(sp.condition),
                logic: sp.condition && sp.condition.logic === "OR" ? "OR" : "AND",
                fields: [],
                limit: typeof sp.limit === "number" && sp.limit > 0 ? sp.limit : 50,
            };
            break;
        }
        case "46": { // 新增数据：外部表 + 字段值映射
            const ds = (inputs.databaseInfoList || [])[0];
            const ip = inputs.insertParam || {};
            dbNodeForm.value = {
                dsID: ds && ds.databaseInfoID ? String(ds.databaseInfoID) : "",
                conditions: [],
                logic: "AND",
                fields: parseDBFieldInfo(ip.fieldInfo),
                limit: 0,
            };
            break;
        }
        case "42": { // 更新数据：外部表 + WHERE 条件 + SET 字段
            const ds = (inputs.databaseInfoList || [])[0];
            const up = inputs.updateParam || {};
            dbNodeForm.value = {
                dsID: ds && ds.databaseInfoID ? String(ds.databaseInfoID) : "",
                conditions: parseDBConditions(up.condition),
                logic: up.condition && up.condition.logic === "OR" ? "OR" : "AND",
                fields: parseDBFieldInfo(up.fieldInfo),
                limit: 0,
            };
            break;
        }
        case "44": { // 删除数据：外部表 + WHERE 条件
            const ds = (inputs.databaseInfoList || [])[0];
            const dp = inputs.deleteParam || {};
            dbNodeForm.value = {
                dsID: ds && ds.databaseInfoID ? String(ds.databaseInfoID) : "",
                conditions: parseDBConditions(dp.condition),
                logic: dp.condition && dp.condition.logic === "OR" ? "OR" : "AND",
                fields: [],
                limit: 0,
            };
            break;
        }
        case "21": { // 循环：类型 + 数组映射 + 中间变量 + 输出映射（Param 形态 ref 指向体内节点输出）
            const arrs = (inputs.inputParameters || []).map((p) => ({ name: p.name || "", ref: refToForm(p.input) }));
            const vars = (inputs.variableParameters || []).map((p) => ({
                name: p.name || "",
                type: (p.input && p.input.type) || "string",
                init: p.input && p.input.value && typeof p.input.value.content === "string" ? p.input.value.content : "",
            }));
            const outParam = (raw.data.outputs || []).find((o) => o && o.name === "outputList" && o.input);
            loopForm.value = {
                loopType: typeof inputs.loopType === "string" ? inputs.loopType : "array",
                loopCount: inputs.loopCount && inputs.loopCount.value ? Number(inputs.loopCount.value.content) || 3 : 3,
                arrays: arrs.length ? arrs : [{ name: "item", ref: { mode: "ref", blockID: "", varName: "", literal: "" } }],
                vars,
                out: outParam ? refToForm(outParam.input) : { mode: "ref", blockID: "", varName: "", literal: "" },
            };
            break;
        }
        case "28": { // 批处理：批量/并发 + 数组映射 + 输出映射
            const arrs = (inputs.inputParameters || []).map((p) => ({ name: p.name || "", ref: refToForm(p.input) }));
            const outParam = (raw.data.outputs || []).find((o) => o && o.name === "outputList" && o.input);
            batchForm.value = {
                batchSize: inputs.batchSize && inputs.batchSize.value ? Number(inputs.batchSize.value.content) || 100 : 100,
                concurrentSize: inputs.concurrentSize && inputs.concurrentSize.value ? Number(inputs.concurrentSize.value.content) || 10 : 10,
                arrays: arrs.length ? arrs : [{ name: "item", ref: { mode: "ref", blockID: "", varName: "", literal: "" } }],
                out: outParam ? refToForm(outParam.input) : { mode: "ref", blockID: "", varName: "", literal: "" },
            };
            break;
        }
        case "20": { // 设置变量：left=中间变量 ref（取 name）+ right=值映射
            setvarForm.value = {
                rows: (inputs.inputParameters || []).map((p) => ({
                    varName: p.left && p.left.value && p.left.value.content ? String(p.left.value.content.name ?? "") : "",
                    ref: refToForm(p.right),
                })),
            };
            break;
        }
    }
});

// ── 输出变量编辑 ──
function addOutput() { outputs.value.push({ name: `var${outputs.value.length + 1}`, type: "string", description: "" }); }
function removeOutput(i) { outputs.value.splice(i, 1); }
function addMapping() { endMappings.value.push({ name: `field${endMappings.value.length + 1}`, ref: { mode: "ref", blockID: "", varName: "", literal: "" } }); }
function addCodeInput() { codeForm.value.inputs.push({ name: `param${codeForm.value.inputs.length + 1}`, ref: { mode: "ref", blockID: "", varName: "", literal: "" } }); }

// ── 分支(8) 编辑 ──
const emptyCond = () => ({ left: { mode: "ref", blockID: "", varName: "", literal: "" }, operator: 1, right: { mode: "literal", blockID: "", varName: "", literal: "" } });
function addBranch() { branchForm.value.branches.push({ conditions: [emptyCond()] }); }
function removeBranch(i) { branchForm.value.branches.splice(i, 1); }
function addCond(bi) { branchForm.value.branches[bi].conditions.push(emptyCond()); }
function removeCond(bi, ci) { branchForm.value.branches[bi].conditions.splice(ci, 1); }

// ── 聚合(32) 编辑 ──
function addAggGroup() { aggForm.value.groups.push({ name: `group${aggForm.value.groups.length + 1}`, ref: { mode: "ref", blockID: "", varName: "", literal: "" } }); }
function removeAggGroup(i) { aggForm.value.groups.splice(i, 1); }

// ── HTTP(45) 编辑 ──
function addHttpHeader() { httpForm.value.headers.push({ name: "", ref: { mode: "literal", blockID: "", varName: "", literal: "" } }); }
function removeHttpHeader(i) { httpForm.value.headers.splice(i, 1); }
// ── HTTP JSON 请求体键值对（免写 JSON：key + 引用/字面量行 → 扁平对象）──
function addHttpBodyField() {
    httpForm.value.bodyFields.push({ key: "", mode: "literal", ref: { mode: "ref", blockID: "", varName: "", literal: "" }, literal: "" });
}
function removeHttpBodyField(i) { httpForm.value.bodyFields.splice(i, 1); }
// 键值对行 → JSON 请求体（引用值渲染为 {{blockID.varName}}，内核 TemplateRender 用同名输入参数替换）
function httpBodyFromFields() {
    const obj = {};
    for (const f of httpForm.value.bodyFields || []) {
        const k = (f.key || "").trim();
        if (!k) continue;
        obj[k] = f.mode === "ref" ? `{{${f.ref.blockID ? `${f.ref.blockID}.` : ""}${f.ref.varName || ""}}}` : (f.literal ?? "");
    }
    return JSON.stringify(obj, null, 2);
}
// JSON 请求体 → 键值对行（仅扁平对象可还原；嵌套/数组返回 null 走高级模式）
function httpFieldsFromBody(json) {
    let obj;
    try { obj = JSON.parse(json || "{}"); } catch { return null; }
    if (!obj || typeof obj !== "object" || Array.isArray(obj)) return null;
    const toRef = (tpl) => {
        const m = String(tpl).match(/^\{\{\s*([\w.]+)\s*\}\}$/);
        const name = m ? m[1].split(".").pop() : "";
        let blockID = "";
        for (const u of props.upstreamVars || []) {
            if ((u.vars || []).some((v) => v.name === name)) { blockID = u.nodeId; break; }
        }
        return { mode: "ref", blockID, varName: name, literal: "" };
    };
    return Object.entries(obj).map(([k, v]) => (
        typeof v === "string" && /^\{\{[\s\w.]+\}\}$/.test(v)
            ? { key: k, ...toRef(v) }
            : { key: k, mode: "literal", ref: { mode: "ref", blockID: "", varName: "", literal: "" }, literal: String(v ?? "") }
    ));
}

// ── MCP 级联选择（服务器 → 工具 → 参数，选中即生成 tools/call 请求）──
async function loadMCPServers() {
    if (!props.mcpServersLoader) return;
    try { mcpServers.value = (await props.mcpServersLoader()) || []; } catch { mcpServers.value = []; }
}
async function loadMCPTools(keepTool) {
    if (!mcpForm.value.server || !props.mcpToolsLoader) return;
    mcpLoading.value = true;
    try {
        mcpTools.value = (await props.mcpToolsLoader(mcpForm.value.server)) || [];
        // 回显模式：工具列表拉回后若上次选的工具存在则保留
        if (keepTool && !mcpTools.value.some((t) => t.name === mcpForm.value.tool)) mcpForm.value.tool = "";
        if (!keepTool) mcpForm.value.tool = "";
    } catch (e) {
        mcpTools.value = [];
        Message.error(String(e && e.message || e));
    } finally { mcpLoading.value = false; }
}
// 选择服务器：同步 URL/方法/请求头到 HTTP 表单（运行时按 HTTP 节点真实执行）
async function onMCPServerChange() {
    mcpTools.value = [];
    mcpForm.value.tool = "";
    const s = mcpServers.value.find((x) => x.name === mcpForm.value.server);
    if (s) {
        httpForm.value.method = "POST";
        httpForm.value.url = s.url || "";
        httpForm.value.headers = Object.entries(s.headers || {}).map(([k, v]) => ({
            name: k, ref: { mode: "literal", blockID: "", varName: "", literal: String(v) },
        }));
    }
    await loadMCPTools(false);
}
// ── MCP 参数键值对 → arguments 对象（免写 JSON：key + 引用/字面量行）──
function mcpArgsFromRows() {
    const obj = {};
    for (const r of mcpForm.value.argsRows || []) {
        const k = (r.key || "").trim();
        if (!k) continue;
        obj[k] = r.mode === "ref" ? `{{${r.ref.blockID ? `${r.ref.blockID}.` : ""}${r.ref.varName || ""}}}` : (r.literal ?? "");
    }
    return obj;
}
function addMCPArg() {
    mcpForm.value.argsRows.push({ key: "", mode: "literal", ref: { mode: "ref", blockID: "", varName: "", literal: "" }, literal: "" });
}
function removeMCPArg(i) { mcpForm.value.argsRows.splice(i, 1); }
// 解析回显：从 arguments 对象（含 {{var}} 字符串值）还原键值对行
function mcpRowsFromArgs(args) {
    const toRef = (tpl) => {
        const m = String(tpl).match(/^\{\{\s*([\w.]+)\s*\}\}$/);
        const name = m ? m[1].split(".").pop() : "";
        // 找到上游节点 ID 以精确回显；找不到则仅回显变量名（apply 时按名匹配上游）
        let blockID = "";
        for (const u of props.upstreamVars || []) {
            if ((u.vars || []).some((v) => v.name === name)) { blockID = u.nodeId; break; }
        }
        return { mode: "ref", blockID, varName: name, literal: "" };
    };
    return Object.entries(args || {}).map(([k, v]) => (
        typeof v === "string" && /^\{\{[\s\w.]+\}\}$/.test(v)
            ? { key: k, ...toRef(v) }
            : { key: k, mode: "literal", ref: { mode: "ref", blockID: "", varName: "", literal: "" }, literal: String(v ?? "") }
    ));
}
// 选择工具：重建 JSON-RPC tools/call 请求体（arguments 由键值对行生成）
function onMCPToolChange() {
    httpForm.value.bodyType = "json";
    httpForm.value.bodyJSON = JSON.stringify({
        jsonrpc: "2.0", id: 1, method: "tools/call",
        params: { name: mcpForm.value.tool, arguments: mcpArgsFromRows() },
    }, null, 2);
}

// 图片生成节点打开时：拉取设置中的模型配置 + 从请求体回显提示词/尺寸
async function loadImageGenSetup(bodyJSON, inputs) {
    if (props.imageGenLoader) {
        try { igModel.value = (await props.imageGenLoader()) || { configured: false }; } catch { igModel.value = { configured: false }; }
    }
    try {
        const bj = JSON.parse(bodyJSON || (inputs.body && inputs.body.bodyData && inputs.body.bodyData.json) || "{}");
        igForm.value = { prompt: typeof bj.prompt === "string" ? bj.prompt : "", size: typeof bj.size === "string" ? bj.size : "1024x1024" };
    } catch { igForm.value = { prompt: "", size: "1024x1024" }; }
}

// ── 知识库（检索 6 / 写入 27）：列表拉取 + 新建 ──
async function loadKnowledgeBases() {
    if (!props.kbLoader) return;
    kbLoading.value = true;
    try { kbList.value = (await props.kbLoader()) || []; } catch (e) { kbList.value = []; Message.error(String(e && e.message || e)); }
    kbLoading.value = false;
}
// 新建知识库（名称 + 一段文本内容）：成功后刷新列表并选中
async function createKnowledge() {
    if (!props.kbCreateHandler) { Message.warning("知识库创建不可用"); return; }
    if (!kbNewName.value.trim() || !kbNewContent.value.trim()) { Message.error("请填写知识库名称与内容"); return; }
    kbCreating.value = true;
    try {
        const kb = await props.kbCreateHandler(kbNewName.value.trim(), kbNewContent.value);
        Message.success(`知识库「${kb.name}」已创建`);
        kbNewName.value = ""; kbNewContent.value = "";
        await loadKnowledgeBases();
        // 按节点类型选中新库（检索多选 / 写入单选）
        if (nodeType.value === "6" && !kbForm.value.ids.includes(kb.id)) kbForm.value.ids.push(kb.id);
        if (nodeType.value === "27") kbWriteForm.value.id = kb.id;
    } catch (e) {
        Message.error("创建失败: " + String(e && e.message || e));
    } finally { kbCreating.value = false; }
}

// 参数提取器：从 data.exRef 回显提取字段与文本来源
function loadExtractorSetup(data) {
    const ex = data.exRef || {};
    exForm.value = {
        text: typeof ex.text === "string" ? ex.text : "",
        fields: Array.isArray(ex.fields) ? JSON.parse(JSON.stringify(ex.fields)) : [],
    };
    if (!exForm.value.fields.length) exForm.value.fields = [{ name: "", desc: "", type: "string", required: true }];
}
function addExField() { exForm.value.fields.push({ name: "", desc: "", type: "string", required: true }); }
function removeExField(i) { exForm.value.fields.splice(i, 1); }

// SQL 节点打开时：拉取数据源列表（设置 → 数据源中管理的表）
async function loadDatabases() {
    if (!props.dbLoader) return;
    dbLoading.value = true;
    try { dbList.value = (await props.dbLoader()) || []; } catch (e) { dbList.value = []; Message.error(String(e && e.message || e)); }
    dbLoading.value = false;
}
// 工作流（子流程 9）：拉取流程列表（wfLoader = CozeListWorkflows）
async function loadWorkflows() {
    if (!props.wfLoader) return;
    wfLoading.value = true;
    try { wfList.value = (await props.wfLoader()) || []; } catch (e) { wfList.value = []; Message.error(String(e && e.message || e)); }
    wfLoading.value = false;
}

// ── 数据库四件套解析辅助 ──
// 可用于四件套的数据源（仅含已配置表名的连接）
const dbNodeOptions = computed(() => dbList.value.filter((d) => d.tableName));

// 解析内核 DBCondition（conditionList[[left,operation,right]]）→ 表单条件行
function parseDBConditions(condition) {
    if (!condition || !Array.isArray(condition.conditionList)) return [];
    return condition.conditionList.map((params) => {
        const left = (params || []).find((p) => p && p.name === "left");
        const op = (params || []).find((p) => p && p.name === "operation");
        const right = (params || []).find((p) => p && p.name === "right");
        return {
            col: left && left.input && left.input.value ? String(left.input.value.content ?? "") : "",
            op: op && op.input && op.input.value ? String(op.input.value.content ?? "EQUAL") : "EQUAL",
            ref: refToForm(right && right.input),
        };
    });
}

// 解析内核 fieldInfo（[[left(列名), right(值映射)]]）→ 表单字段行
function parseDBFieldInfo(fieldInfo) {
    if (!Array.isArray(fieldInfo)) return [];
    return fieldInfo.map((params) => {
        const p0 = (params || [])[0];
        const p1 = (params || [])[1];
        return {
            name: p0 && p0.input && p0.input.value ? String(p0.input.value.content ?? "") : "",
            ref: refToForm(p1 && p1.input),
        };
    });
}

// 表单条件行 → 内核 DBCondition（conditionList[[left,operation,right]] + logic）
function buildDBCondition(conditions, logic) {
    const list = conditions
        .filter((c) => c.col.trim())
        .map((c) => [
            { name: "left", input: { type: "string", value: { type: "literal", content: c.col.trim() } } },
            { name: "operation", input: { type: "string", value: { type: "literal", content: c.op } } },
            { name: "right", input: formToRef(c.ref, "string") },
        ]);
    return { conditionList: list, logic: logic === "OR" ? "OR" : "AND" };
}

// 表单字段行 → 内核 fieldInfo（[[left(列名), right(值映射)]]）
function buildDBFieldInfo(fields) {
    return fields
        .filter((f) => f.name.trim())
        .map((f) => [
            { name: "left", input: { type: "string", value: { type: "literal", content: f.name.trim() } } },
            { name: "right", input: formToRef(f.ref, "string") },
        ]);
}

// ── 变量下拉选项（按上游节点分组）──
const varOptions = computed(() => props.upstreamVars.map((u) => ({
    label: `${u.nodeName} 的输出`,
    options: (u.vars || []).map((v) => ({ label: `${v.name} (${v.type || "string"})`, value: `${u.nodeId}::${v.name}` })),
})));
function onVarPick(refForm, val) {
    const [blockID, name] = String(val || "").split("::");
    refForm.blockID = blockID; refForm.varName = name;
}

// ── 应用：表单 → raw.data 序列化 ──
function apply() {
    const node = props.node;
    if (!node) return;
    const raw = JSON.parse(JSON.stringify(node.data.raw || {}));
    raw.meta = { ...(raw.meta || {}), nodeMeta: { ...((raw.meta || {}).nodeMeta || {}), title: title.value } };

    let data;
    try {
        switch (formMode.value) {
            case "1": data = { ...raw.data, nodeMeta: { title: title.value }, outputs: outputs.value.filter((o) => o.name) }; break;
            case "2":
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: { terminatePlan: "returnVariables", inputParameters: endMappings.value.filter((m) => m.name).map((m) => ({ name: m.name, input: formToRef(m.ref, "string") })) },
                    outputs: outputs.value.filter((o) => o.name),
                };
                break;
            case "3": {
                // llmParam：数值参数必须传 JSON 字符串（adaptor Content.(string) 强转）
                const mk = (n, c) => ({ name: n, input: { type: "literal", value: { type: "literal", content: c } } });
                // 提示词模板变量 → inputParameters（内核 SetInputsForNodeSchema 从这里生成
                // 节点输入 schema；缺了它单节点试运行的输入会被过滤、{{var}} 不替换）
                const tplVars = [...new Set(
                    (`${llmForm.value.system} ${llmForm.value.user}`.match(/\{\{\s*([\w.]+)\s*\}\}/g) || [])
                        .map((s) => s.replace(/[{}\s]/g, "").split(".")[0]),
                )];
                const findUpstream = (name) => {
                    for (const u of props.upstreamVars || []) {
                        const v = (u.vars || []).find((x) => x.name === name);
                        if (v) return { blockID: u.nodeId, name: v.name };
                    }
                    return null;
                };
                const inputParams = tplVars
                    .map((name) => ({ name, up: findUpstream(name) }))
                    .filter((x) => x.up)
                    .map((x) => ({
                        name: x.name,
                        input: { type: "string", value: { type: "ref", content: { blockID: x.up.blockID, name: x.up.name, source: "block-output" } } },
                    }));
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        llmParam: [
                            mk("modelType", llmForm.value.modelType || "69010"),
                            mk("modleName", llmForm.value.modleName || "agnes-2.5-flash"),
                            mk("systemPrompt", llmForm.value.system),
                            mk("prompt", llmForm.value.user),
                            mk("responseFormat", String(llmForm.value.responseFormat)),
                            mk("temperature", String(llmForm.value.temperature)),
                        ],
                        inputParameters: inputParams,
                    },
                    outputs: outputs.value.filter((o) => o.name),
                };
                break;
            }
            case "5":
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        code: codeForm.value.code,
                        language: codeForm.value.language, // 3=Python 5=JavaScript
                        inputParameters: codeForm.value.inputs.filter((m) => m.name).map((m) => ({ name: m.name, input: formToRef(m.ref, "string") })),
                    },
                    outputs: outputs.value.filter((o) => o.name),
                };
                break;
            case "13":
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        content: formToRef({ mode: outputForm.value.contentMode, blockID: outputForm.value.contentRefBlock, varName: outputForm.value.contentRefVar, literal: outputForm.value.contentLiteral }, "string"),
                        streamingOutput: outputForm.value.streaming,
                    },
                };
                break;
            case "8": {
                // 分支：branches[i].condition.conditions（logic=1 OR；left 引用、right 字面值）
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        branches: branchForm.value.branches.map((b) => ({
                            condition: {
                                logic: 1,
                                conditions: b.conditions.map((cd) => ({
                                    operator: cd.operator,
                                    left: { name: "left", input: formToRef(cd.left, "string") },
                                    right: { name: "right", input: formToRef(cd.right, "string") },
                                })),
                            },
                        })),
                    },
                };
                break;
            }
            case "32":
                // 聚合：mergeGroups []*Param（分组名 + 引用上游输出）
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        mergeGroups: aggForm.value.groups.filter((g) => g.name).map((g) => ({ name: g.name, input: formToRef(g.ref, "string") })),
                    },
                    outputs: outputs.value.filter((o) => o.name),
                };
                break;
            case "6": {
                // 知识库检索：datasetParam 七项（官方形态）+ Query 输入
                const ids = kbForm.value.ids.map(String);
                if (!ids.length) { Message.error("请至少选择一个知识库"); return; }
                const kbParam = (n, t, c) => ({ name: n, input: { type: t, value: { type: "literal", content: c } } });
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        datasetParam: [
                            kbParam("datasetList", "list", ids),
                            kbParam("topK", "integer", Number(kbForm.value.topK) || 5),
                            kbParam("useRerank", "boolean", false),   // 本地检索暂不支持重排
                            kbParam("useRewrite", "boolean", false),  // 本地检索暂不支持改写
                            kbParam("isPersonalOnly", "boolean", false),
                            kbParam("minScore", "float", Number(kbForm.value.minScore) || 0.5),
                            kbParam("strategy", "integer", Number(kbForm.value.strategy) || 1),
                        ],
                        inputParameters: [{ name: "Query", input: formToRef(kbForm.value.query, "string") }],
                    },
                    // outputs 固定 outputList（官方形态，不开放编辑）
                    outputs: [{ type: "list", name: "outputList", schema: { type: "object", schema: [{ type: "string", name: "output" }] } }],
                };
                break;
            }
            case "22": {
                // 意图识别：intents 列表 + query 输入 + llmParam（responseFormat=2 JSON 分类）
                const intents = intentForm.value.intents.map((s) => s.trim()).filter(Boolean);
                if (!intents.length) { Message.error("请至少配置一个意图"); return; }
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        chatHistorySetting: { enableChatHistory: false, chatHistoryRound: 3 },
                        inputParameters: [{ name: "query", input: formToRef(intentForm.value.query, "string") }],
                        llmParam: {
                            modelType: "69010", modelName: "agnes-2.5-flash", responseFormat: 2, temperature: 1, maxTokens: 4096,
                            systemPrompt: { type: "string", value: { type: "literal", content: "" } },
                            prompt: { type: "string", value: { type: "literal", content: "{{query}}" } },
                        },
                        intents: intents.map((name) => ({ name })),
                        mode: "all",
                    },
                    // 输出固定：classificationId + reason
                    outputs: [{ name: "classificationId", type: "integer" }, { name: "reason", type: "string" }],
                };
                break;
            }
            case "27": {
                // 知识库写入：内容（引用/字面量）+ 目标知识库（单选）
                if (!kbWriteForm.value.id) { Message.error("请选择要写入的知识库"); return; }
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        inputParameters: [{ name: "knowledge", input: formToRef(kbWriteForm.value.content, "string") }],
                        datasetParam: [{ name: "datasetList", input: { type: "list", schema: { type: "string" }, value: { type: "literal", content: [String(kbWriteForm.value.id)] } } }],
                    },
                    outputs: [{ name: "documentId", type: "string" }, { name: "fileName", type: "string" }, { name: "fileUrl", type: "string" }],
                };
                break;
            }
            case "58": {
                // JSON 序列化：input（引用/字面量）→ output 字符串
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: { inputParameters: [{ name: "input", input: formToRef(jsonIOForm.value.input, "string") }] },
                    outputs: [{ name: "output", type: "string" }],
                };
                break;
            }
            case "59": {
                // JSON 反序列化：input（字符串引用）→ 指定类型输出
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: { inputParameters: [{ name: "input", input: formToRef(jsonIOForm.value.input, "string") }] },
                    outputs: [{ name: "output", type: jsonIOForm.value.outType || "object" }],
                };
                break;
            }
            case "15": {
                // 文本处理：method=concat + concatParams（concatResult 模板 / arrayItemConcatChar 拼接符 / 模板变量映射）
                const tplVars = [...new Set(
                    (tplForm.value.tpl.match(/\{\{\s*([\w.]+)\s*\}\}/g) || [])
                        .map((s) => s.replace(/[{}\s]/g, "").split(".")[0]),
                )];
                // 已声明映射优先，模板变量缺失时补字面量占位（引用可在映射行切换）
                const declared = new Map(tplForm.value.inputs.filter((m) => m.name).map((m) => [m.name, m]));
                const concatParams = [
                    { name: "concatResult", input: { type: "string", value: { type: "literal", content: tplForm.value.tpl } } },
                    { name: "arrayItemConcatChar", input: { type: "string", value: { type: "literal", content: tplForm.value.concatChar || "" } } },
                    ...tplVars.map((name) => {
                        const m = declared.get(name);
                        return { name, input: formToRef(m ? m.ref : { mode: "literal", literal: "" }, "string") };
                    }),
                ];
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: { method: "concat", concatParams },
                    outputs: [{ name: "output", type: "string" }],
                };
                break;
            }
            case "12": {
                // SQL 自定义：数据源（databaseInfoID=online 库 ID）+ sql 模板 + 输入映射
                if (!sqlForm.value.dbID) { Message.error("请选择数据源（先到「设置 → 数据源」创建）"); return; }
                if (!sqlForm.value.sql.trim()) { Message.error("请填写 SQL 语句"); return; }
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        databaseInfoList: [{ databaseInfoID: String(sqlForm.value.dbID) }],
                        sql: sqlForm.value.sql,
                        inputParameters: sqlForm.value.inputs.filter((m) => m.name).map((m) => ({ name: m.name, input: formToRef(m.ref, "string") })),
                    },
                    outputs: outputs.value.filter((o) => o.name),
                };
                break;
            }
            case "9": {
                // 工作流（子流程）：workflowId（内核运行时自动加载子画布构建嵌套 schema）
                if (!subwfForm.value.wfID) { Message.error("请选择要引用的工作流"); return; }
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        workflowId: subwfForm.value.wfID,
                        workflowVersion: "", // 空 = 草稿（调试语义，与内核 QType FromDraft 对齐）
                        inputParameters: subwfForm.value.inputs.filter((m) => m.name).map((m) => ({ name: m.name, input: formToRef(m.ref, "string") })),
                    },
                };
                break;
            }
            case "60": {
                // 知识库删除：datasetParam[0]（知识库 ID 数组）+ documentID 输入映射
                if (!kbDelForm.value.kbID) { Message.error("请选择知识库"); return; }
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        datasetParam: [
                            { name: "datasetList", input: { type: "list", schema: { type: "string" }, value: { type: "literal", content: [kbDelForm.value.kbID] } } },
                        ],
                        inputParameters: [{ name: "documentID", input: formToRef(kbDelForm.value.doc, "string") }],
                    },
                    outputs: [{ name: "documentID", type: "string" }, { name: "isDeleted", type: "boolean" }],
                };
                break;
            }
            case "31": {
                // 注释：note 文本（纯画布标注，内核 blockTypeToSkip 跳过执行）
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: { comment: { schemaType: "comment", note: commentForm.value } },
                    size: raw.data && raw.data.size ? raw.data.size : { width: 240, height: 96 },
                };
                break;
            }
            case "43": {
                // 查询数据：外部表（负 ID）+ 条件组 + limit（内核 Build 校验 limit > 0）
                if (!dbNodeForm.value.dsID) { Message.error("请选择数据表（先到「设置 → 数据源」配置表名）"); return; }
                if (!(dbNodeForm.value.limit > 0)) { Message.error("查询行数 limit 必须大于 0"); return; }
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        databaseInfoList: [{ databaseInfoID: dbNodeForm.value.dsID }],
                        selectParam: {
                            condition: buildDBCondition(dbNodeForm.value.conditions, dbNodeForm.value.logic),
                            orderByList: [],
                            limit: dbNodeForm.value.limit,
                            fieldList: [], // 空 = 全列（内核 FieldID 为 int64，外部连接用全列 + 列名动态输出）
                        },
                    },
                    outputs: raw.data && raw.data.outputs && raw.data.outputs.length ? raw.data.outputs : [
                        { name: "outputList", type: "list", schema: { type: "object", properties: [] } },
                        { name: "rowNum", type: "integer" },
                    ],
                };
                break;
            }
            case "46": {
                // 新增数据：外部表 + 字段值映射（列名白名单校验交给后端 safeIdent）
                if (!dbNodeForm.value.dsID) { Message.error("请选择数据表（先到「设置 → 数据源」配置表名）"); return; }
                if (!dbNodeForm.value.fields.some((f) => f.name.trim())) { Message.error("请至少配置一个字段值"); return; }
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        databaseInfoList: [{ databaseInfoID: dbNodeForm.value.dsID }],
                        insertParam: { fieldInfo: buildDBFieldInfo(dbNodeForm.value.fields) },
                    },
                    outputs: raw.data && raw.data.outputs && raw.data.outputs.length ? raw.data.outputs : [
                        { name: "outputList", type: "list", schema: { type: "object", properties: [] } },
                        { name: "rowNum", type: "integer" },
                    ],
                };
                break;
            }
            case "42": {
                // 更新数据：外部表 + WHERE 条件 + SET 字段映射
                if (!dbNodeForm.value.dsID) { Message.error("请选择数据表（先到「设置 → 数据源」配置表名）"); return; }
                if (!dbNodeForm.value.fields.some((f) => f.name.trim())) { Message.error("请至少配置一个要更新的字段"); return; }
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        databaseInfoList: [{ databaseInfoID: dbNodeForm.value.dsID }],
                        updateParam: {
                            condition: buildDBCondition(dbNodeForm.value.conditions, dbNodeForm.value.logic),
                            fieldInfo: buildDBFieldInfo(dbNodeForm.value.fields),
                        },
                    },
                    outputs: raw.data && raw.data.outputs && raw.data.outputs.length ? raw.data.outputs : [
                        { name: "outputList", type: "list", schema: { type: "object", properties: [] } },
                        { name: "rowNum", type: "integer" },
                    ],
                };
                break;
            }
            case "44": {
                // 删除数据：外部表 + WHERE 条件
                if (!dbNodeForm.value.dsID) { Message.error("请选择数据表（先到「设置 → 数据源」配置表名）"); return; }
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        databaseInfoList: [{ databaseInfoID: dbNodeForm.value.dsID }],
                        deleteParam: { condition: buildDBCondition(dbNodeForm.value.conditions, dbNodeForm.value.logic) },
                    },
                    outputs: raw.data && raw.data.outputs && raw.data.outputs.length ? raw.data.outputs : [
                        { name: "outputList", type: "list", schema: { type: "object", properties: [] } },
                        { name: "rowNum", type: "integer" },
                    ],
                };
                break;
            }
            case "21": {
                // 循环：loopType + 数组输入（array ref）+ 中间变量（literal 初值）；
                // 输出 ref 指向体内节点输出（逐轮聚合）或中间变量（内核自动识别 ParentIntermediate）
                if (loopForm.value.loopType === "array" && !loopForm.value.arrays.some((m) => m.name.trim())) { Message.error("按数组循环至少需要配置一个数组输入映射"); return; }
                if (loopForm.value.loopType === "count" && !(loopForm.value.loopCount > 0)) { Message.error("循环次数必须大于 0"); return; }
                const loopInputs = {
                    loopType: loopForm.value.loopType,
                    inputParameters: loopForm.value.arrays
                        .filter((m) => m.name.trim())
                        .map((m) => ({ name: m.name.trim(), input: { ...formToRef(m.ref, "list"), schema: { type: "string" } } })),
                    variableParameters: loopForm.value.vars
                        .filter((v) => v.name.trim())
                        .map((v) => ({ name: v.name.trim(), input: { type: v.type || "string", value: { type: "literal", content: v.init || "" } } })),
                };
                if (loopForm.value.loopType === "count") {
                    loopInputs.loopCount = { type: "integer", value: { type: "literal", content: String(loopForm.value.loopCount) } };
                }
                // 输出 Param 形态：ref 指向体内节点输出（每轮聚合）；未配置 ref 时保留原 outputs
                const loopOutputs = loopForm.value.out.mode === "ref" && loopForm.value.out.blockID
                    ? [{ name: "outputList", input: { ...formToRef(loopForm.value.out, "list"), schema: { type: "string" } } }]
                    : (raw.data.outputs || []);
                data = { ...raw.data, nodeMeta: { title: title.value }, inputs: loopInputs, outputs: loopOutputs };
                break;
            }
            case "28": {
                // 批处理：batchSize/concurrentSize（顶层 BlockInput，字符串数字）+ 数组输入
                if (!batchForm.value.arrays.some((m) => m.name.trim())) { Message.error("批处理至少需要配置一个数组输入映射"); return; }
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        batchSize: { type: "integer", value: { type: "literal", content: String(batchForm.value.batchSize || 100) } },
                        concurrentSize: { type: "integer", value: { type: "literal", content: String(batchForm.value.concurrentSize || 10) } },
                        inputParameters: batchForm.value.arrays
                            .filter((m) => m.name.trim())
                            .map((m) => ({ name: m.name.trim(), input: { ...formToRef(m.ref, "list"), schema: { type: "string" } } })),
                        variableParameters: [],
                    },
                    // 输出 Param 形态：ref 指向体内节点输出（按批聚合）；未配置 ref 时保留原 outputs
                    outputs: batchForm.value.out.mode === "ref" && batchForm.value.out.blockID
                        ? [{ name: "outputList", input: { ...formToRef(batchForm.value.out, "list"), schema: { type: "string" } } }]
                        : (raw.data.outputs || []),
                };
                break;
            }
            case "19": {
                // 终止循环：无配置（内核置 $break 中间变量结束循环）
                data = { ...raw.data, nodeMeta: { title: title.value }, inputs: {} };
                break;
            }
            case "29": {
                // 继续循环：无配置（跳过本轮剩余步骤）
                data = { ...raw.data, nodeMeta: { title: title.value }, inputs: {} };
                break;
            }
            case "20": {
                // 设置变量：left=中间变量 ref（blockID=所属容器，内核自动识别 ParentIntermediate）
                if (!props.node || !props.node.parentNode) { Message.error("设置变量节点必须在循环或批处理容器内"); return; }
                if (!setvarForm.value.rows.some((r) => r.varName.trim())) { Message.error("请至少配置一个变量赋值"); return; }
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    inputs: {
                        inputParameters: setvarForm.value.rows
                            .filter((r) => r.varName.trim())
                            .map((r) => ({
                                left: { type: "string", value: { type: "ref", content: { blockID: props.node.parentNode, name: r.varName.trim(), source: "block-output" } } },
                                right: formToRef(r.ref, "string"),
                            })),
                    },
                };
                break;
            }
            case "45":
                // 参数提取器预设：把提取字段生成为 LLM 指令模板（文本经上游引用注入 URL 查询/请求体）
                if (isExtractorNode.value) {
                    const fields = exForm.value.fields.filter((f) => f.name.trim());
                    if (!fields.length) { Message.error("请至少配置一个提取字段（字段名）"); return; }
                    if (!exForm.value.text.trim() && !/\{\{\s*\w+\s*\}\}/.test(exForm.value.text)) {
                        Message.error("请填写待提取文本或用 {{变量名}} 引用上游输出");
                        return;
                    }
                    // schema 描述 + 指令模板（固化到 exRef 供回显；执行语义：文本 + 字段说明经模板渲染）
                    const schemaDesc = fields.map((f) => `- ${f.name} (${f.type}${f.required ? "" : ", 可选"})：${f.desc || f.name}`).join("\n");
                    httpForm.value.method = "GET";
                    httpForm.value.url = "https://httpbin.org/get";
                    httpForm.value.bodyType = "none";
                    httpForm.value.bodyJSON = "";
                    httpForm.value.headers = [];
                    data = {
                        ...raw.data,
                        nodeMeta: { title: title.value },
                        exRef: {
                            text: exForm.value.text,
                            fields: fields.map((f) => ({ name: f.name.trim(), desc: f.desc || "", type: f.type || "string", required: !!f.required })),
                        },
                        inputs: {
                            apiInfo: { method: httpForm.value.method, url: httpForm.value.url },
                            headers: [], params: [],
                            body: { bodyType: "none", bodyData: null },
                            setting: { timeoutMs: 30000, retryTimes: 3 },
                        },
                        outputs: fields.map((f) => ({ name: f.name.trim(), type: f.type || "string", description: f.desc || "" })),
                    };
                    // schemaDesc 挂到 nodeMeta.description（预览用，不参与内核执行）
                    data.nodeMeta.description = `提取字段：\n${schemaDesc}`;
                    break;
                }
                // 图片生成预设：用设置中的图片生成模型生成 images/generations 请求
                if (isImageGenNode.value) {
                    if (!igModel.value.configured) {
                        Message.error("图片生成模型未配置：请到「设置 → 图片生成模型」填写后重试");
                        return;
                    }
                    const prompt = igForm.value.prompt || "";
                    if (!prompt.trim() && !/\{\{\s*\w+\s*\}\}/.test(prompt)) {
                        Message.error("请填写提示词（可使用 {{变量名}} 引用上游输出）");
                        return;
                    }
                    httpForm.value.method = "POST";
                    httpForm.value.url = `${igModel.value.baseURL}/images/generations`;
                    httpForm.value.bodyType = "json";
                    httpForm.value.bodyJSON = JSON.stringify(
                        { model: igModel.value.model, prompt, n: 1, size: igForm.value.size || "1024x1024" },
                        null, 2,
                    );
                    httpForm.value.headers = [{ name: "Content-Type", ref: { mode: "literal", blockID: "", varName: "", literal: "application/json" } }];
                    // OpenAI 兼容端点：配置了 key 才带认证头（本地网关场景可空）
                    if (igModel.value.apiKey) {
                        httpForm.value.headers.push({ name: "Authorization", ref: { mode: "literal", blockID: "", varName: "", literal: `Bearer ${igModel.value.apiKey}` } });
                    }
                    outputs.value = outputs.value.filter((o) => o.name).length
                        ? outputs.value
                        : [{ name: "output", type: "string", description: "图片生成响应（data[0].url 或 b64_json）" }];
                }
                // MCP 预设：参数编辑框最终回写请求体（工具选中时保证 tools/call 最新）
                if (isMCPNode.value && mcpForm.value.tool) onMCPToolChange();
                // JSON 请求体：键值对模式由行生成（覆盖 bodyJSON），高级模式沿用直写 JSON
                if (httpForm.value.bodyType === "json" && !httpForm.value.bodyRaw) {
                    httpForm.value.bodyJSON = httpBodyFromFields();
                }
                // HTTP：内嵌提升字段（apiInfo/headers/params/body）
                data = {
                    ...raw.data,
                    nodeMeta: { title: title.value },
                    // MCP 选择固化（server/tool 回显用；内核反序列化忽略未知字段）
                    mcpRef: isMCPNode.value ? { server: mcpForm.value.server, tool: mcpForm.value.tool } : (raw.data && raw.data.mcpRef) || undefined,
                    inputs: {
                        apiInfo: { method: httpForm.value.method, url: httpForm.value.url },
                        headers: httpForm.value.headers.filter((h) => h.name).map((h) => ({ name: h.name, input: formToRef(h.ref, "string") })),
                        params: raw.data && raw.data.inputs && raw.data.inputs.params || [],
                        body: httpForm.value.bodyType === "json"
                            ? { bodyType: "json", bodyData: { json: httpForm.value.bodyJSON } }
                            : { bodyType: "none", bodyData: null },
                        setting: (raw.data && raw.data.inputs && raw.data.inputs.setting) || { timeoutMs: 30000, retryTimes: 3 },
                    },
                    outputs: outputs.value.filter((o) => o.name),
                };
                break;
            default: // json 兜底
                data = JSON.parse(rawText.value);
        }
    } catch (e) {
        Message.error("配置不合法: " + e);
        return;
    }
    raw.data = data;
    emit("apply", raw);
    emit("update:visible", false);
}

// ── 单节点试运行 ──
// 结构节点不可试运行：开始节点只声明输入（无执行逻辑），结束节点依赖上游数据
//（单节点图执行会报 720701013 no tasks to execute）。
const canTest = computed(() => nodeType.value !== "1" && nodeType.value !== "2");
const testInput = ref({});       // 参数名 → 输入值
const testing = ref(false);
const testResult = ref(null);

// 已声明的输入参数名（end=映射名 / code=输入映射名 / llm=prompt 模板 {{var}} / 其他=空）
const declaredInputs = computed(() => {
    switch (formMode.value) {
        case "2": return endMappings.value.map((m) => m.name).filter(Boolean);
        case "5": return codeForm.value.inputs.map((m) => m.name).filter(Boolean);
        case "3": {
            const names = new Set();
            const tpl = `${llmForm.value.system} ${llmForm.value.user}`;
            (tpl.match(/\{\{\s*([\w.]+)\s*\}\}/g) || []).forEach((s) => names.add(s.replace(/[{}\s]/g, "").split(".")[0]));
            return [...names];
        }
        default: return [];
    }
});
watch(declaredInputs, (names) => {
    const next = {};
    names.forEach((n) => { next[n] = testInput.value[n] ?? ""; });
    testInput.value = next;
}, { immediate: true });

async function runTest() {
    if (!props.testRunHandler) { Message.warning("试运行不可用"); return; }
    testing.value = true;
    testResult.value = null;
    try {
        testResult.value = await props.testRunHandler({ ...testInput.value });
        if (testResult.value.status === "success") Message.success(`试运行成功（${testResult.value.durationMs}ms）`);
        else Message.error(`试运行失败: ${testResult.value.failReason || testResult.value.status}`);
    } catch (e) {
        testResult.value = { status: "fail", failReason: String(e && e.message || e) };
        Message.error("试运行失败: " + e);
    } finally {
        testing.value = false;
    }
}
function close() { emit("update:visible", false); }

// 试运行结果展示：output 空对象时回退 outputRaw（防节点输出解析为空时误显示 {}）
const displayTestOutput = computed(() => {
    const r = testResult.value || {};
    const out = r.output;
    if (out && typeof out === "object" && Object.keys(out).length > 0) {
        return JSON.stringify(out, null, 2);
    }
    if (typeof out === "string" && out) return out;
    return r.outputRaw || "{}";
});
</script>

<template>
  <a-drawer :visible="visible" :width="460" :title="`配置 · ${typeName}`" unmount-on-close @cancel="close" @close="close">
    <div class="nd-body">
      <div class="field">
        <label>节点标题</label>
        <a-input v-model="title" size="small" />
      </div>

      <!-- 开始：输出变量列表 -->
      <template v-if="formMode === '1'">
        <div class="field">
          <label>输出变量（运行输入将按这些名称提供）</label>
          <div v-for="(o, i) in outputs" :key="i" class="var-row">
            <a-input v-model="o.name" size="mini" placeholder="变量名" style="width: 120px" />
            <a-select v-model="o.type" size="mini" style="width: 90px">
              <a-option value="string">string</a-option>
              <a-option value="integer">integer</a-option>
              <a-option value="number">number</a-option>
              <a-option value="boolean">boolean</a-option>
              <a-option value="object">object</a-option>
              <a-option value="array">array</a-option>
            </a-select>
            <a-input v-model="o.description" size="mini" placeholder="描述" style="flex: 1" />
            <a-button size="mini" status="danger" @click="removeOutput(i)">删</a-button>
          </div>
          <a-button size="mini" long @click="addOutput">+ 添加变量</a-button>
        </div>
      </template>

      <!-- 结束：输出映射 -->
      <template v-else-if="formMode === '2'">
        <div class="field">
          <label>输出映射（返回变量 ← 上游引用或常量）</label>
          <div v-for="(m, i) in endMappings" :key="i" class="var-col">
            <div class="var-row">
              <a-input v-model="m.name" size="mini" placeholder="返回变量名" style="width: 130px" />
              <a-button size="mini" status="danger" @click="endMappings.splice(i, 1)">删</a-button>
            </div>
            <div class="var-row">
              <a-radio-group v-model="m.ref.mode" size="mini" type="button">
                <a-radio value="ref">引用</a-radio>
                <a-radio value="literal">常量</a-radio>
              </a-radio-group>
              <a-select v-if="m.ref.mode === 'ref'" size="mini" placeholder="选择上游变量" style="flex: 1" allow-search
                :model-value="m.ref.blockID ? `${m.ref.blockID}::${m.ref.varName}` : ''"
                @change="(v) => onVarPick(m.ref, v)">
                <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                  <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-input v-else v-model="m.ref.literal" size="mini" placeholder="常量文本" style="flex: 1" />
            </div>
          </div>
          <a-button size="mini" long @click="addMapping">+ 添加映射</a-button>
        </div>
      </template>

      <!-- 大模型 -->
      <template v-else-if="formMode === '3'">
        <div class="field">
          <label>模型</label>
          <a-select v-model="selectedModelID" size="small" allow-search :loading="modelLoading"
            placeholder="选择模型" @dropdown-scroll-bottom="() => {}">
            <a-option v-for="m in modelOptions" :key="m.id" :value="m.id">{{ m.name }}</a-option>
          </a-select>
          <div v-if="llmForm.modleName" class="hint">当前：{{ llmForm.modleName }}（modelType={{ llmForm.modelType }}）</div>
        </div>
        <div class="field">
          <label>System 提示词</label>
          <textarea v-model="llmForm.system" class="nd-textarea" rows="4" spellcheck="false" />
        </div>
        <div class="field">
          <label>User 提示词（用 {{ tplHint }} 引用上游输出，如 {{ tplExample }}）</label>
          <textarea v-model="llmForm.user" class="nd-textarea" rows="4" spellcheck="false" />
        </div>
        <div class="field">
          <label>温度 {{ llmForm.temperature }}</label>
          <a-slider v-model="llmForm.temperature" :min="0" :max="1" :step="0.1" />
        </div>
        <div class="field">
          <label>输出格式</label>
          <a-select v-model="llmForm.responseFormat" size="small">
            <a-option :value="0">文本</a-option>
            <a-option :value="1">Markdown</a-option>
            <a-option :value="2">JSON</a-option>
          </a-select>
        </div>
        <div class="field">
          <label>输出变量</label>
          <div v-for="(o, i) in outputs" :key="i" class="var-row">
            <a-input v-model="o.name" size="mini" placeholder="变量名" style="width: 120px" />
            <a-input v-model="o.description" size="mini" placeholder="描述" style="flex: 1" />
            <a-button size="mini" status="danger" @click="removeOutput(i)">删</a-button>
          </div>
          <a-button size="mini" long @click="addOutput">+ 添加变量</a-button>
        </div>
      </template>

      <!-- 代码 -->
      <template v-else-if="formMode === '5'">
        <div class="field">
          <label>语言</label>
          <a-radio-group v-model="codeForm.language" size="small" type="button">
            <a-radio :value="3">Python</a-radio>
            <a-radio :value="5">JavaScript</a-radio>
          </a-radio-group>
        </div>
        <div class="field">
          <label>代码（Python 约定：async def main(args: Args) -> Output，输入经 args.params 取）</label>
          <textarea v-model="codeForm.code" class="nd-textarea nd-code" rows="9" spellcheck="false" />
        </div>
        <div class="field">
          <label>输入参数（注入 args.params）</label>
          <div v-for="(m, i) in codeForm.inputs" :key="i" class="var-col">
            <div class="var-row">
              <a-input v-model="m.name" size="mini" placeholder="参数名" style="width: 130px" />
              <a-button size="mini" status="danger" @click="codeForm.inputs.splice(i, 1)">删</a-button>
            </div>
            <div class="var-row">
              <a-radio-group v-model="m.ref.mode" size="mini" type="button">
                <a-radio value="ref">引用</a-radio>
                <a-radio value="literal">常量</a-radio>
              </a-radio-group>
              <a-select v-if="m.ref.mode === 'ref'" size="mini" placeholder="选择上游变量" style="flex: 1" allow-search
                :model-value="m.ref.blockID ? `${m.ref.blockID}::${m.ref.varName}` : ''"
                @change="(v) => onVarPick(m.ref, v)">
                <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                  <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-input v-else v-model="m.ref.literal" size="mini" placeholder="常量文本" style="flex: 1" />
            </div>
          </div>
          <a-button size="mini" long @click="addCodeInput">+ 添加输入</a-button>
        </div>
        <div class="field">
          <label>输出变量</label>
          <div v-for="(o, i) in outputs" :key="i" class="var-row">
            <a-input v-model="o.name" size="mini" placeholder="变量名" style="width: 120px" />
            <a-button size="mini" status="danger" @click="removeOutput(i)">删</a-button>
          </div>
          <a-button size="mini" long @click="addOutput">+ 添加变量</a-button>
        </div>
      </template>

      <!-- 输出节点 -->
      <template v-else-if="formMode === '13'">
        <div class="field">
          <label>内容</label>
          <a-radio-group v-model="outputForm.contentMode" size="mini" type="button">
            <a-radio value="literal">文本模板</a-radio>
            <a-radio value="ref">引用变量</a-radio>
          </a-radio-group>
          <a-select v-if="outputForm.contentMode === 'ref'" size="small" placeholder="选择上游变量" style="margin-top: 6px" allow-search
            :model-value="outputForm.contentRefBlock ? `${outputForm.contentRefBlock}::${outputForm.contentRefVar}` : ''"
            @change="(v) => { const [b, n] = String(v).split('::'); outputForm.contentRefBlock = b; outputForm.contentRefVar = n; }">
            <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
              <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
            </a-optgroup>
          </a-select>
          <textarea v-else v-model="outputForm.contentLiteral" class="nd-textarea" rows="4" style="margin-top: 6px" spellcheck="false" />
        </div>
        <div class="field">
          <a-checkbox v-model="outputForm.streaming">流式输出</a-checkbox>
        </div>
      </template>

      <!-- 分支：条件编辑器（每分支可多条条件，OR 连接；出口端口见画布节点底部） -->
      <template v-else-if="formMode === '8'">
        <div class="field">
          <label>分支条件（满足即走该分支出口；都不满足走 else）</label>
          <div v-for="(b, bi) in branchForm.branches" :key="bi" class="branch-card">
            <div class="branch-head">
              <span>分支 {{ bi + 1 }}（出口 true{{ bi > 0 ? `_${bi}` : "" }}）</span>
              <a-button size="mini" status="danger" @click="removeBranch(bi)">删分支</a-button>
            </div>
            <div v-for="(cd, ci) in b.conditions" :key="ci" class="var-row cond-row">
              <a-select size="mini" placeholder="左值（引用）" style="width: 110px" allow-search
                :model-value="cd.left.blockID ? `${cd.left.blockID}::${cd.left.varName}` : ''"
                @change="(v) => onVarPick(cd.left, v)">
                <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                  <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-select v-model="cd.operator" size="mini" style="width: 84px">
                <a-option v-for="op in OPERATORS" :key="op.value" :value="op.value">{{ op.label }}</a-option>
              </a-select>
              <a-input v-model="cd.right.literal" size="mini" placeholder="比较值" style="flex: 1" />
              <a-button size="mini" status="danger" @click="removeCond(bi, ci)">×</a-button>
            </div>
            <a-button size="mini" long @click="addCond(bi)">+ 加条件（OR）</a-button>
          </div>
          <a-button size="mini" long @click="addBranch">+ 添加分支</a-button>
        </div>
      </template>

      <!-- 聚合：分组引用列表 -->
      <template v-else-if="formMode === '32'">
        <div class="field">
          <label>聚合分组（每个分组引用一个上游输出，命中即取值）</label>
          <div v-for="(g, i) in aggForm.groups" :key="i" class="var-col">
            <div class="var-row">
              <a-input v-model="g.name" size="mini" placeholder="分组名" style="width: 130px" />
              <a-button size="mini" status="danger" @click="removeAggGroup(i)">删</a-button>
            </div>
            <div class="var-row">
              <a-radio-group v-model="g.ref.mode" size="mini" type="button">
                <a-radio value="ref">引用</a-radio>
                <a-radio value="literal">常量</a-radio>
              </a-radio-group>
              <a-select v-if="g.ref.mode === 'ref'" size="mini" placeholder="选择上游变量" style="flex: 1" allow-search
                :model-value="g.ref.blockID ? `${g.ref.blockID}::${g.ref.varName}` : ''"
                @change="(v) => onVarPick(g.ref, v)">
                <a-optgroup v-for="u in varOptions" :key="u.label" :label="u.label">
                  <a-option v-for="o in u.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-input v-else v-model="g.ref.literal" size="mini" placeholder="常量文本" style="flex: 1" />
            </div>
          </div>
          <a-button size="mini" long @click="addAggGroup">+ 添加分组</a-button>
        </div>
      </template>

      <!-- 文本处理（模板转换）：模板 + 输入映射 -->
      <template v-else-if="formMode === '15'">
        <div class="field">
          <label>文本模板（{'{'}{'{'}变量名{'}'}{'}'} 插值引用输入映射）</label>
          <textarea v-model="tplForm.tpl" class="nd-textarea" rows="4" placeholder="例如：{'{'}{'{'}name{'}'}{'}'} 你好，你的订单 {'{'}{'{'}order{'}'}{'}'} 已发货" spellcheck="false" />
        </div>
        <div class="field">
          <label>数组拼接符（输入为数组时的元素连接符）</label>
          <a-input v-model="tplForm.concatChar" size="small" placeholder="例如：, 或换行" />
        </div>
        <div class="field">
          <label>输入映射（模板中的变量名 ← 上游引用或常量）</label>
          <div v-for="(m, i) in tplForm.inputs" :key="i" class="var-col">
            <div class="var-row">
              <a-input v-model="m.name" size="mini" placeholder="变量名" style="width: 130px" />
              <a-button size="mini" status="danger" @click="tplForm.inputs.splice(i, 1)">删</a-button>
            </div>
            <div class="var-row">
              <a-radio-group v-model="m.ref.mode" size="mini" type="button">
                <a-radio value="ref">引用</a-radio>
                <a-radio value="literal">常量</a-radio>
              </a-radio-group>
              <a-select v-if="m.ref.mode === 'ref'" size="mini" placeholder="选择上游变量" style="flex: 1" allow-search
                :model-value="m.ref.blockID ? `${m.ref.blockID}::${m.ref.varName}` : ''"
                @change="(v) => onVarPick(m.ref, v)">
                <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                  <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-input v-else v-model="m.ref.literal" size="mini" placeholder="常量文本" style="flex: 1" />
            </div>
          </div>
          <a-button size="mini" long @click="tplForm.inputs.push({ name: '', ref: { mode: 'ref', blockID: '', varName: '', literal: '' } })">+ 添加映射</a-button>
        </div>
        <div class="field">
          <label>输出（固定）</label>
          <div class="hint">output：渲染后的字符串</div>
        </div>
      </template>

      <!-- SQL 自定义：数据源 + SQL 模板 + 输入映射 -->
      <template v-else-if="formMode === '12'">
        <div class="field">
          <label>
            数据源（外部 MySQL 连接）
            <a-button size="mini" style="margin-left: 8px" :loading="dbLoading" @click="loadDatabases">刷新</a-button>
          </label>
          <div class="hint">先到「设置 → 扩展 → 数据源」新建 MySQL 连接，再回到此处点「刷新」选择</div>
          <div v-if="!dbList.length && !dbLoading" class="hint" style="color: #f53f3f; margin-top: 4px">
            暂无数据源：请先到「设置 → 扩展 → 数据源」新建连接，然后点上方「刷新」
          </div>
          <a-select v-else v-model="sqlForm.dbID" size="small" placeholder="选择数据源连接" allow-search :loading="dbLoading" style="margin-top: 6px">
            <a-option v-for="d in dbList" :key="d.id" :value="d.id">
              {{ d.name }}{{ d.desc ? `（${d.desc}）` : "" }}
            </a-option>
          </a-select>
          <div v-if="sqlForm.dbID" class="hint">
            {{ ((dbList.find((d) => d.id === sqlForm.dbID) || {}).desc) || "" }}
          </div>
        </div>
        <div class="field">
          <label>SQL 语句（仅 SELECT 查询返回行集；{'{'}{'{'}变量名{'}'}{'}'} 插值引用输入映射）</label>
          <textarea v-model="sqlForm.sql" class="nd-textarea nd-code" rows="6" placeholder="例如：SELECT * FROM 表名 WHERE name = '{'{'name{'}'}'}'" spellcheck="false" />
        </div>
        <div class="field">
          <label>输入映射（SQL 中的变量名 ← 上游引用或常量）</label>
          <div v-for="(m, i) in sqlForm.inputs" :key="i" class="var-col">
            <div class="var-row">
              <a-input v-model="m.name" size="mini" placeholder="变量名" style="width: 130px" />
              <a-button size="mini" status="danger" @click="sqlForm.inputs.splice(i, 1)">删</a-button>
            </div>
            <div class="var-row">
              <a-radio-group v-model="m.ref.mode" size="mini" type="button">
                <a-radio value="ref">引用</a-radio>
                <a-radio value="literal">常量</a-radio>
              </a-radio-group>
              <a-select v-if="m.ref.mode === 'ref'" size="mini" placeholder="选择上游变量" style="flex: 1" allow-search
                :model-value="m.ref.blockID ? `${m.ref.blockID}::${m.ref.varName}` : ''"
                @change="(v) => onVarPick(m.ref, v)">
                <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                  <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-input v-else v-model="m.ref.literal" size="mini" placeholder="常量文本" style="flex: 1" />
            </div>
          </div>
          <a-button size="mini" long @click="sqlForm.inputs.push({ name: '', ref: { mode: 'ref', blockID: '', varName: '', literal: '' } })">+ 添加映射</a-button>
        </div>
        <div class="field">
          <label>输出变量（行数据列表 + 行数）</label>
          <div class="hint">输出默认 outputList（行数据）/ rowNum（行数）；查询列在 SQL 中自行指定</div>
        </div>
      </template>

      <!-- 工作流（子流程）：选择已有流程 + 输入映射 -->
      <template v-else-if="formMode === '9'">
        <div class="field">
          <label>引用的工作流（运行时内核自动加载其画布并嵌套执行）</label>
          <div v-if="!wfList.length && !wfLoading" class="hint" style="color: #f53f3f">
            暂无可选流程：请先在「AI Flow」列表中创建工作流
          </div>
          <a-select v-else v-model="subwfForm.wfID" size="small" placeholder="选择工作流" allow-search :loading="wfLoading">
            <a-option v-for="w in wfList" :key="w.id" :value="w.id">
              {{ w.name }}{{ w.desc ? `（${w.desc}）` : "" }}
            </a-option>
          </a-select>
        </div>
        <div class="field">
          <label>输入映射（子流程「开始」节点的参数 ← 上游引用或常量）</label>
          <div v-for="(m, i) in subwfForm.inputs" :key="i" class="var-col">
            <div class="var-row">
              <a-input v-model="m.name" size="mini" placeholder="参数名" style="width: 130px" />
              <a-button size="mini" status="danger" @click="subwfForm.inputs.splice(i, 1)">删</a-button>
            </div>
            <div class="var-row">
              <a-radio-group v-model="m.ref.mode" size="mini" type="button">
                <a-radio value="ref">引用</a-radio>
                <a-radio value="literal">常量</a-radio>
              </a-radio-group>
              <a-select v-if="m.ref.mode === 'ref'" size="mini" placeholder="选择上游变量" style="flex: 1" allow-search
                :model-value="m.ref.blockID ? `${m.ref.blockID}::${m.ref.varName}` : ''"
                @change="(v) => onVarPick(m.ref, v)">
                <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                  <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-input v-else v-model="m.ref.literal" size="mini" placeholder="常量文本" style="flex: 1" />
            </div>
          </div>
          <a-button size="mini" long @click="subwfForm.inputs.push({ name: '', ref: { mode: 'ref', blockID: '', varName: '', literal: '' } })">+ 添加映射</a-button>
        </div>
        <div class="field">
          <label>输出</label>
          <div class="hint">输出 = 子流程「结束」节点定义的输出变量</div>
        </div>
      </template>

      <!-- 知识库删除：选择知识库 + documentID 输入映射 -->
      <template v-else-if="formMode === '60'">
        <div class="field">
          <label>知识库（删除该库中的指定文档）</label>
          <div v-if="!kbList.length && !kbLoading" class="hint" style="color: #f53f3f">
            暂无知识库：请先在知识库中创建
          </div>
          <a-select v-else v-model="kbDelForm.kbID" size="small" placeholder="选择知识库" allow-search :loading="kbLoading">
            <a-option v-for="k in kbList" :key="k.id" :value="k.id">{{ k.name }}</a-option>
          </a-select>
        </div>
        <div class="field">
          <label>documentID（要删除的文档 ID：上游引用或常量）</label>
          <div class="var-row">
            <a-radio-group v-model="kbDelForm.doc.mode" size="mini" type="button">
              <a-radio value="ref">引用</a-radio>
              <a-radio value="literal">常量</a-radio>
            </a-radio-group>
            <a-select v-if="kbDelForm.doc.mode === 'ref'" size="mini" placeholder="选择上游变量" style="flex: 1" allow-search
              :model-value="kbDelForm.doc.blockID ? `${kbDelForm.doc.blockID}::${kbDelForm.doc.varName}` : ''"
              @change="(v) => onVarPick(kbDelForm.doc, v)">
              <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
              </a-optgroup>
            </a-select>
            <a-input v-else v-model="kbDelForm.doc.literal" size="mini" placeholder="文档 ID" style="flex: 1" />
          </div>
        </div>
        <div class="field">
          <label>输出</label>
          <div class="hint">documentID（文档 ID）/ isDeleted（是否删除成功）</div>
        </div>
      </template>

      <!-- 注释：纯画布标注（不参与执行） -->
      <template v-else-if="formMode === '31'">
        <div class="field">
          <label>注释内容（显示在画布上，不参与执行）</label>
          <textarea v-model="commentForm" class="nd-textarea" rows="5" placeholder="输入画布注释文本…" spellcheck="false" />
        </div>
      </template>

      <!-- 数据库四件套：数据表下拉（仅显示已配置表名的连接） -->
      <template v-else-if="formMode === '43' || formMode === '46' || formMode === '42' || formMode === '44'">
        <div class="field">
          <label>数据表（设置 → 数据源中绑定表名的 MySQL 连接）</label>
          <div v-if="!dbNodeOptions.length && !dbLoading" class="hint" style="color: #f53f3f">
            暂无可用数据表：请先到「设置 → 数据源」新建连接并填写「绑定表名」，再回到此处点「刷新」
          </div>
          <div v-else class="var-row">
            <a-select v-model="dbNodeForm.dsID" size="small" placeholder="选择数据表" allow-search :loading="dbLoading" style="flex: 1">
              <a-option v-for="d in dbNodeOptions" :key="d.id" :value="d.id">
                {{ d.name }}（{{ d.tableName }}）
              </a-option>
            </a-select>
            <a-button size="mini" :loading="dbLoading" @click="loadDatabases">刷新</a-button>
          </div>
        </div>

        <!-- 查询数据：limit -->
        <div v-if="formMode === '43'" class="field">
          <label>查询行数（LIMIT，必须 &gt; 0）</label>
          <a-input-number v-model="dbNodeForm.limit" size="small" :min="1" :max="10000" placeholder="50" />
        </div>

        <!-- WHERE 条件（查询/更新/删除） -->
        <div v-if="formMode !== '46'" class="field">
          <label>过滤条件（WHERE，列名白名单：英文/数字/下划线）</label>
          <div v-for="(c, i) in dbNodeForm.conditions" :key="i" class="var-col">
            <div class="var-row">
              <a-input v-model="c.col" size="mini" placeholder="列名" style="width: 120px" />
              <a-select v-model="c.op" size="mini" style="width: 110px">
                <a-option v-for="o in DB_OPERATORS" :key="o.value" :value="o.value">{{ o.label }}</a-option>
              </a-select>
              <a-button size="mini" status="danger" @click="dbNodeForm.conditions.splice(i, 1)">删</a-button>
            </div>
            <div v-if="!['IS_NULL', 'IS_NOT_NULL'].includes(c.op)" class="var-row">
              <a-radio-group v-model="c.ref.mode" size="mini" type="button">
                <a-radio value="ref">引用</a-radio>
                <a-radio value="literal">常量</a-radio>
              </a-radio-group>
              <a-select v-if="c.ref.mode === 'ref'" size="mini" placeholder="选择上游变量" style="flex: 1" allow-search
                :model-value="c.ref.blockID ? `${c.ref.blockID}::${c.ref.varName}` : ''"
                @change="(v) => onVarPick(c.ref, v)">
                <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                  <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-input v-else v-model="c.ref.literal" size="mini" placeholder="比较值（IN 用逗号分隔）" style="flex: 1" />
            </div>
          </div>
          <div v-if="dbNodeForm.conditions.length > 1" class="var-row" style="margin-top: 4px">
            <span class="hint">条件组合方式</span>
            <a-radio-group v-model="dbNodeForm.logic" size="mini" type="button">
              <a-radio value="AND">且（AND）</a-radio>
              <a-radio value="OR">或（OR）</a-radio>
            </a-radio-group>
          </div>
          <a-button size="mini" long @click="dbNodeForm.conditions.push({ col: '', op: 'EQUAL', ref: { mode: 'ref', blockID: '', varName: '', literal: '' } })">+ 添加条件</a-button>
        </div>

        <!-- 字段值映射（新增/更新） -->
        <div v-if="formMode === '46' || formMode === '42'" class="field">
          <label>{{ formMode === '46' ? "写入字段（INSERT 列 = 值）" : "更新字段（SET 列 = 值）" }}</label>
          <div v-for="(f, i) in dbNodeForm.fields" :key="i" class="var-col">
            <div class="var-row">
              <a-input v-model="f.name" size="mini" placeholder="列名" style="width: 120px" />
              <a-button size="mini" status="danger" @click="dbNodeForm.fields.splice(i, 1)">删</a-button>
            </div>
            <div class="var-row">
              <a-radio-group v-model="f.ref.mode" size="mini" type="button">
                <a-radio value="ref">引用</a-radio>
                <a-radio value="literal">常量</a-radio>
              </a-radio-group>
              <a-select v-if="f.ref.mode === 'ref'" size="mini" placeholder="选择上游变量" style="flex: 1" allow-search
                :model-value="f.ref.blockID ? `${f.ref.blockID}::${f.ref.varName}` : ''"
                @change="(v) => onVarPick(f.ref, v)">
                <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                  <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-input v-else v-model="f.ref.literal" size="mini" placeholder="字段值" style="flex: 1" />
            </div>
          </div>
          <a-button size="mini" long @click="dbNodeForm.fields.push({ name: '', ref: { mode: 'ref', blockID: '', varName: '', literal: '' } })">+ 添加字段</a-button>
        </div>

        <div class="field">
          <label>输出</label>
          <div class="hint">outputList（行数据列表，键 = 列名）/ rowNum（行数或影响行数）</div>
        </div>
      </template>

      <!-- 循环：类型 + 数组输入 + 中间变量（循环体在画布容器内编辑） -->
      <template v-else-if="formMode === '21'">
        <div class="field">
          <label>循环方式</label>
          <a-select v-model="loopForm.loopType" size="small">
            <a-option v-for="o in LOOP_TYPES" :key="o.value" :value="o.value">{{ o.label }}</a-option>
          </a-select>
        </div>
        <div v-if="loopForm.loopType === 'count'" class="field">
          <label>循环次数</label>
          <a-input-number v-model="loopForm.loopCount" size="small" :min="1" :max="100000" />
        </div>
        <div class="field">
          <label>数组输入映射（数组参数名 ← 上游数组；体内节点引用该参数名 = 当前元素）</label>
          <div v-for="(m, i) in loopForm.arrays" :key="i" class="var-col">
            <div class="var-row">
              <a-input v-model="m.name" size="mini" placeholder="参数名（如 item）" style="width: 130px" />
              <a-button size="mini" status="danger" @click="loopForm.arrays.splice(i, 1)">删</a-button>
            </div>
            <div class="var-row">
              <a-select v-if="m.ref.mode === 'ref'" size="mini" placeholder="选择上游数组变量" style="flex: 1" allow-search
                :model-value="m.ref.blockID ? `${m.ref.blockID}::${m.ref.varName}` : ''"
                @change="(v) => onVarPick(m.ref, v)">
                <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                  <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-input v-else v-model="m.ref.literal" size="mini" placeholder="逗号分隔的字面量数组" style="flex: 1" />
            </div>
            <a-radio-group v-model="m.ref.mode" size="mini" type="button">
              <a-radio value="ref">引用</a-radio>
              <a-radio value="literal">常量</a-radio>
            </a-radio-group>
          </div>
          <a-button size="mini" long @click="loopForm.arrays.push({ name: '', ref: { mode: 'ref', blockID: '', varName: '', literal: '' } })">+ 添加数组输入</a-button>
        </div>
        <div class="field">
          <label>中间变量（跨轮传递；体内用「设置变量」更新，输出可引用）</label>
          <div v-for="(v, i) in loopForm.vars" :key="i" class="var-row">
            <a-input v-model="v.name" size="mini" placeholder="变量名" style="width: 110px" />
            <a-select v-model="v.type" size="mini" style="width: 90px">
              <a-option v-for="t in VAR_TYPES" :key="t" :value="t">{{ t }}</a-option>
            </a-select>
            <a-input v-model="v.init" size="mini" placeholder="初值" style="flex: 1" />
            <a-button size="mini" status="danger" @click="loopForm.vars.splice(i, 1)">删</a-button>
          </div>
          <a-button size="mini" long @click="loopForm.vars.push({ name: '', type: 'string', init: '' })">+ 添加中间变量</a-button>
        </div>
        <div class="field">
          <label>输出映射（outputList ← 体内节点输出，每轮结果聚合为数组）</label>
          <div class="var-row">
            <a-input model-value="outputList" size="mini" disabled style="width: 110px" />
            <a-select size="mini" placeholder="选择体内节点输出变量" style="flex: 1" allow-search
              :model-value="loopForm.out.blockID ? `${loopForm.out.blockID}::${loopForm.out.varName}` : ''"
              @change="(v) => onVarPick(loopForm.out, v)">
              <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
              </a-optgroup>
            </a-select>
          </div>
          <div class="hint">体内节点输出需先在画布中拖入循环体节点；此处下拉可选「体内：xx」分组</div>
        </div>
        <div class="field">
          <label>循环体</label>
          <div class="hint">在画布上把节点拖入本节点容器内并连线（体内可引用上方参数名与中间变量）</div>
        </div>
      </template>

      <!-- 批处理：批量/并发 + 数组输入 -->
      <template v-else-if="formMode === '28'">
        <div class="field">
          <label>批量大小（batchSize，一次批内最多处理条数）</label>
          <a-input-number v-model="batchForm.batchSize" size="small" :min="1" :max="100000" />
        </div>
        <div class="field">
          <label>并发数（concurrentSize）</label>
          <a-input-number v-model="batchForm.concurrentSize" size="small" :min="1" :max="100" />
        </div>
        <div class="field">
          <label>数组输入映射（数组参数名 ← 上游数组；体内节点引用该参数名 = 当前元素）</label>
          <div v-for="(m, i) in batchForm.arrays" :key="i" class="var-col">
            <div class="var-row">
              <a-input v-model="m.name" size="mini" placeholder="参数名（如 item）" style="width: 130px" />
              <a-button size="mini" status="danger" @click="batchForm.arrays.splice(i, 1)">删</a-button>
            </div>
            <div class="var-row">
              <a-select v-if="m.ref.mode === 'ref'" size="mini" placeholder="选择上游数组变量" style="flex: 1" allow-search
                :model-value="m.ref.blockID ? `${m.ref.blockID}::${m.ref.varName}` : ''"
                @change="(v) => onVarPick(m.ref, v)">
                <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                  <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-input v-else v-model="m.ref.literal" size="mini" placeholder="逗号分隔的字面量数组" style="flex: 1" />
            </div>
            <a-radio-group v-model="m.ref.mode" size="mini" type="button">
              <a-radio value="ref">引用</a-radio>
              <a-radio value="literal">常量</a-radio>
            </a-radio-group>
          </div>
          <a-button size="mini" long @click="batchForm.arrays.push({ name: '', ref: { mode: 'ref', blockID: '', varName: '', literal: '' } })">+ 添加数组输入</a-button>
        </div>
        <div class="field">
          <label>输出映射（outputList ← 体内节点输出，按批聚合为数组）</label>
          <div class="var-row">
            <a-input model-value="outputList" size="mini" disabled style="width: 110px" />
            <a-select size="mini" placeholder="选择体内节点输出变量" style="flex: 1" allow-search
              :model-value="batchForm.out.blockID ? `${batchForm.out.blockID}::${batchForm.out.varName}` : ''"
              @change="(v) => onVarPick(batchForm.out, v)">
              <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
              </a-optgroup>
            </a-select>
          </div>
          <div class="hint">体内节点输出需先在画布中拖入批处理体节点；此处下拉可选「体内：xx」分组</div>
        </div>
        <div class="field">
          <label>批处理体</label>
          <div class="hint">在画布上把节点拖入本节点容器内并连线</div>
        </div>
      </template>

      <!-- 终止循环：体内专用，无配置 -->
      <template v-else-if="formMode === '19'">
        <div class="field">
          <label>终止循环（Break）</label>
          <div class="hint">执行到本节点时立即跳出整个循环（置于体内分支的「满足条件」路径上即可，如配合分支节点使用）</div>
        </div>
      </template>

      <!-- 继续循环：体内专用，无配置 -->
      <template v-else-if="formMode === '29'">
        <div class="field">
          <label>继续循环（Continue）</label>
          <div class="hint">执行到本节点时跳过本轮剩余步骤，直接进入下一轮</div>
        </div>
      </template>

      <!-- 设置变量：体内专用，中间变量赋值 -->
      <template v-else-if="formMode === '20'">
        <div class="field">
          <label>变量赋值（中间变量名 ← 上游引用或常量）</label>
          <div v-for="(r, i) in setvarForm.rows" :key="i" class="var-col">
            <div class="var-row">
              <a-input v-model="r.varName" size="mini" placeholder="中间变量名" style="width: 130px" />
              <a-button size="mini" status="danger" @click="setvarForm.rows.splice(i, 1)">删</a-button>
            </div>
            <div class="var-row">
              <a-radio-group v-model="r.ref.mode" size="mini" type="button">
                <a-radio value="ref">引用</a-radio>
                <a-radio value="literal">常量</a-radio>
              </a-radio-group>
              <a-select v-if="r.ref.mode === 'ref'" size="mini" placeholder="选择变量来源" style="flex: 1" allow-search
                :model-value="r.ref.blockID ? `${r.ref.blockID}::${r.ref.varName}` : ''"
                @change="(v) => onVarPick(r.ref, v)">
                <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                  <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-input v-else v-model="r.ref.literal" size="mini" placeholder="赋值内容" style="flex: 1" />
            </div>
          </div>
          <a-button size="mini" long @click="setvarForm.rows.push({ varName: '', ref: { mode: 'ref', blockID: '', varName: '', literal: '' } })">+ 添加赋值</a-button>
        </div>
        <div class="field">
          <label>说明</label>
          <div class="hint">变量名需与所属循环/批处理节点中配置的「中间变量」一致（区分大小写）</div>
        </div>
      </template>

      <!-- 知识库检索：Query + 知识库多选 + 检索策略（对标 Coze 官方） -->
      <template v-else-if="formMode === '6'">
        <div class="field">
          <label>查询内容（Query）</label>
          <a-radio-group v-model="kbForm.query.mode" size="mini" type="button">
            <a-radio value="ref">引用变量</a-radio>
            <a-radio value="literal">固定文本</a-radio>
          </a-radio-group>
          <a-select v-if="kbForm.query.mode === 'ref'" size="small" placeholder="选择上游变量" style="margin-top: 6px" allow-search
            :model-value="kbForm.query.blockID ? `${kbForm.query.blockID}::${kbForm.query.varName}` : ''"
            @change="(v) => onVarPick(kbForm.query, v)">
            <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
              <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
            </a-optgroup>
          </a-select>
          <textarea v-else v-model="kbForm.query.literal" class="nd-textarea" rows="3" style="margin-top: 6px" placeholder="输入要检索的内容，可使用 {{ tplHint }} 引用上游输出" spellcheck="false" />
        </div>
        <div class="field">
          <label>知识库（可多选）</label>
          <div v-if="!kbList.length && !kbLoading" class="hint" style="color: #f53f3f">
            暂无知识库：请先在下方创建
          </div>
          <a-select v-else v-model="kbForm.ids" size="small" placeholder="选择知识库" multiple allow-search :loading="kbLoading">
            <a-option v-for="kb in kbList" :key="kb.id" :value="kb.id">{{ kb.name }}</a-option>
          </a-select>
        </div>
        <div class="field">
          <label>新建知识库（快速创建文本库）</label>
          <a-input v-model="kbNewName" size="small" placeholder="知识库名称" style="margin-bottom: 6px" />
          <textarea v-model="kbNewContent" class="nd-textarea" rows="4" placeholder="知识内容（将作为一篇文档切片入库）" spellcheck="false" />
          <a-button size="small" long style="margin-top: 6px" :loading="kbCreating" @click="createKnowledge">+ 创建知识库</a-button>
        </div>
        <div class="field">
          <label>最大召回数量（topK）</label>
          <a-input-number v-model="kbForm.topK" size="small" :min="1" :max="50" />
        </div>
        <div class="field">
          <label>最小匹配度（0~1，低于此分数的召回会被过滤）</label>
          <a-input-number v-model="kbForm.minScore" size="small" :min="0" :max="1" :step="0.1" />
        </div>
        <div class="field">
          <label>检索策略</label>
          <a-select v-model="kbForm.strategy" size="small">
            <a-option :value="1">混合检索（全文 + 向量）</a-option>
            <a-option :value="0">语义检索（向量）</a-option>
            <a-option :value="2">全文检索（关键词）</a-option>
          </a-select>
        </div>
        <div class="field">
          <label>输出（固定）</label>
          <div class="hint">outputList：Array&lt;Object&gt;，每项含 output（切片文本），按相关性降序</div>
        </div>
      </template>

      <!-- 意图识别：query + 意图列表（出口 branch_0..N + default） -->
      <template v-else-if="formMode === '22'">
        <div class="field">
          <label>用户输入（query）</label>
          <a-radio-group v-model="intentForm.query.mode" size="mini" type="button">
            <a-radio value="ref">引用变量</a-radio>
            <a-radio value="literal">固定文本</a-radio>
          </a-radio-group>
          <a-select v-if="intentForm.query.mode === 'ref'" size="small" placeholder="选择上游变量" style="margin-top: 6px" allow-search
            :model-value="intentForm.query.blockID ? `${intentForm.query.blockID}::${intentForm.query.varName}` : ''"
            @change="(v) => onVarPick(intentForm.query, v)">
            <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
              <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
            </a-optgroup>
          </a-select>
          <a-input v-else v-model="intentForm.query.literal" size="small" style="margin-top: 6px" placeholder="固定文本（一般引用开始节点的用户输入）" />
        </div>
        <div class="field">
          <label>意图列表（每个意图一个出口，命中即走对应分支）</label>
          <div v-for="(it, i) in intentForm.intents" :key="i" class="var-row">
            <a-tag size="small" color="arcoblue">branch_{{ i }}</a-tag>
            <a-input v-model="intentForm.intents[i]" size="mini" placeholder="意图描述，如：咨询退款" style="flex: 1" />
            <a-button size="mini" status="danger" @click="intentForm.intents.splice(i, 1)">删</a-button>
          </div>
          <a-button size="mini" long @click="intentForm.intents.push('')">+ 添加意图</a-button>
          <div class="hint">所有意图都不命中时走 default 出口。输出：classificationId（命中意图序号）、reason（判断理由）</div>
        </div>
      </template>

      <!-- 知识库写入：内容 + 目标知识库 -->
      <template v-else-if="formMode === '27'">
        <div class="field">
          <label>写入内容</label>
          <a-radio-group v-model="kbWriteForm.content.mode" size="mini" type="button">
            <a-radio value="ref">引用变量</a-radio>
            <a-radio value="literal">固定文本</a-radio>
          </a-radio-group>
          <a-select v-if="kbWriteForm.content.mode === 'ref'" size="small" placeholder="选择上游变量" style="margin-top: 6px" allow-search
            :model-value="kbWriteForm.content.blockID ? `${kbWriteForm.content.blockID}::${kbWriteForm.content.varName}` : ''"
            @change="(v) => onVarPick(kbWriteForm.content, v)">
            <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
              <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
            </a-optgroup>
          </a-select>
          <textarea v-else v-model="kbWriteForm.content.literal" class="nd-textarea" rows="4" style="margin-top: 6px" placeholder="要写入知识库的文本" spellcheck="false" />
        </div>
        <div class="field">
          <label>目标知识库</label>
          <div v-if="!kbList.length && !kbLoading" class="hint" style="color: #f53f3f">暂无知识库：请先在下方创建</div>
          <a-select v-else v-model="kbWriteForm.id" size="small" placeholder="选择知识库" allow-search :loading="kbLoading">
            <a-option v-for="kb in kbList" :key="kb.id" :value="kb.id">{{ kb.name }}</a-option>
          </a-select>
        </div>
        <div class="field">
          <label>新建知识库（快速创建文本库）</label>
          <a-input v-model="kbNewName" size="small" placeholder="知识库名称" style="margin-bottom: 6px" />
          <textarea v-model="kbNewContent" class="nd-textarea" rows="4" placeholder="初始知识内容" spellcheck="false" />
          <a-button size="small" long style="margin-top: 6px" :loading="kbCreating" @click="createKnowledge">+ 创建知识库</a-button>
        </div>
        <div class="field">
          <label>输出（固定）</label>
          <div class="hint">documentId / fileName / fileUrl</div>
        </div>
      </template>

      <!-- JSON 序列化 / 反序列化 -->
      <template v-else-if="formMode === '58' || formMode === '59'">
        <div class="field">
          <label>{{ formMode === '58' ? '输入变量（任意类型 → JSON 字符串）' : '输入变量（JSON 字符串 → 变量）' }}</label>
          <a-radio-group v-model="jsonIOForm.input.mode" size="mini" type="button">
            <a-radio value="ref">引用</a-radio>
            <a-radio value="literal">常量</a-radio>
          </a-radio-group>
          <a-select v-if="jsonIOForm.input.mode === 'ref'" size="small" placeholder="选择上游变量" style="margin-top: 6px" allow-search
            :model-value="jsonIOForm.input.blockID ? `${jsonIOForm.input.blockID}::${jsonIOForm.input.varName}` : ''"
            @change="(v) => onVarPick(jsonIOForm.input, v)">
            <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
              <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
            </a-optgroup>
          </a-select>
          <textarea v-else v-model="jsonIOForm.input.literal" class="nd-textarea nd-code" rows="3" style="margin-top: 6px" spellcheck="false" />
        </div>
        <div v-if="formMode === '59'" class="field">
          <label>输出变量类型</label>
          <a-select v-model="jsonIOForm.outType" size="small">
            <a-option value="object">object</a-option>
            <a-option value="string">string</a-option>
            <a-option value="integer">integer</a-option>
            <a-option value="number">number</a-option>
            <a-option value="boolean">boolean</a-option>
            <a-option value="array">array</a-option>
          </a-select>
        </div>
      </template>

      <!-- HTTP 请求（含 MCP / 图片生成预设，同一内核类型 45） -->
      <template v-else-if="formMode === '45'">
        <!-- MCP 预设：服务器/工具级联选择（来自设置中的 MCP 配置，http/sse 传输） -->
        <template v-if="isMCPNode">
          <div class="field">
            <label>MCP 服务器（设置中已添加，仅支持 http/sse 传输）</label>
            <a-select v-model="mcpForm.server" size="small" placeholder="选择 MCP 服务器" allow-search
              :loading="mcpLoading && !mcpTools.length" @change="onMCPServerChange">
              <a-option v-for="s in mcpServers" :key="s.name" :value="s.name"
                :disabled="!s.enabled || (s.transport !== 'http' && s.transport !== 'sse')">
                {{ s.name }}{{ s.transport !== 'http' && s.transport !== 'sse' ? '（stdio，不支持）' : (!s.enabled ? '（已禁用）' : '') }}
              </a-option>
            </a-select>
          </div>
          <div class="field">
            <label>MCP 工具</label>
            <a-select v-model="mcpForm.tool" size="small" placeholder="先选择服务器，再选择工具" allow-search
              :disabled="!mcpForm.server" :loading="mcpLoading" @change="onMCPToolChange">
              <a-option v-for="t in mcpTools" :key="t.name" :value="t.name">
                {{ t.name }}{{ t.description ? ` — ${t.description.slice(0, 40)}` : '' }}
              </a-option>
            </a-select>
          </div>
          <div class="field">
            <label>工具参数（键值对；值可引用上游变量）</label>
            <div v-for="(r, i) in mcpForm.argsRows" :key="i" class="var-col">
              <div class="var-row">
                <a-input v-model="r.key" size="mini" placeholder="参数名" style="width: 130px" />
                <a-radio-group v-model="r.mode" size="mini" type="button">
                  <a-radio value="literal">字面量</a-radio>
                  <a-radio value="ref">引用</a-radio>
                </a-radio-group>
                <a-button size="mini" status="danger" @click="removeMCPArg(i)">删</a-button>
              </div>
              <div class="var-row">
                <a-select v-if="r.mode === 'ref'" size="mini" placeholder="选择上游变量" style="flex: 1" allow-search
                  :model-value="r.ref.blockID ? `${r.ref.blockID}::${r.ref.varName}` : ''"
                  @change="(v) => onVarPick(r.ref, v)">
                  <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                    <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                  </a-optgroup>
                </a-select>
                <a-input v-else v-model="r.literal" size="mini" placeholder="参数值" style="flex: 1" />
              </div>
            </div>
            <a-button size="mini" long @click="addMCPArg">+ 添加参数</a-button>
          </div>
        </template>
        <!-- 参数提取器预设：提取字段 + 待提取文本（对标 Dify Parameter Extractor） -->
        <template v-if="isExtractorNode">
          <div class="field">
            <label>待提取文本（{'{'}{'{'}变量名{'}'}{'}'} 引用上游输出）</label>
            <textarea v-model="exForm.text" class="nd-textarea" rows="3" placeholder="例如：{'{'}{'{'}query{'}'}{'}'}" spellcheck="false" />
          </div>
          <div class="field">
            <label>提取字段（每个字段成为输出变量）</label>
            <div v-for="(f, i) in exForm.fields" :key="i" class="var-col">
              <div class="var-row">
                <a-input v-model="f.name" size="mini" placeholder="字段名" style="width: 110px" />
                <a-select v-model="f.type" size="mini" style="width: 90px">
                  <a-option value="string">string</a-option>
                  <a-option value="integer">integer</a-option>
                  <a-option value="number">number</a-option>
                  <a-option value="boolean">boolean</a-option>
                </a-select>
                <a-checkbox v-model="f.required">必填</a-checkbox>
                <a-button size="mini" status="danger" @click="removeExField(i)">删</a-button>
              </div>
              <a-input v-model="f.desc" size="mini" placeholder="字段说明（帮助模型识别该字段）" />
            </div>
            <a-button size="mini" long @click="addExField">+ 添加字段</a-button>
          </div>
          <div class="field">
            <label>输出</label>
            <div class="hint">每个字段生成一个输出变量，供下游节点引用</div>
          </div>
        </template>
        <!-- 图片生成预设：使用设置中的图片生成模型（OpenAI images API 兼容） -->
        <template v-if="isImageGenNode">
          <div class="field">
            <label>图片生成模型（设置中已配置）</label>
            <div v-if="igModel.configured" class="ig-model">
              <div><b>{{ igModel.model }}</b></div>
              <div class="hint">{{ igModel.baseURL }}</div>
            </div>
            <div v-else class="hint" style="color: #f53f3f">
              未配置：请到「设置 → 图片生成模型」填写 OpenAI 兼容端点（BaseURL / 模型 / API Key）
            </div>
          </div>
          <div class="field">
            <label>提示词（{'{'}{'{'}变量名{'}'}{'}'} 引用上游输出）</label>
            <textarea v-model="igForm.prompt" class="nd-textarea" rows="4" placeholder="例如：一只戴着宇航员头盔的橘猫，皮克斯风格" spellcheck="false" />
          </div>
          <div class="field">
            <label>尺寸</label>
            <a-select v-model="igForm.size" size="small">
              <a-option value="1024x1024">1024x1024</a-option>
              <a-option value="512x512">512x512</a-option>
              <a-option value="256x256">256x256</a-option>
              <a-option value="1792x1024">1792x1024（横版）</a-option>
              <a-option value="1024x1792">1024x1792（竖版）</a-option>
            </a-select>
          </div>
        </template>
        <!-- HTTP 原始表单：MCP / 图片生成预设隐藏（配置已自动生成） -->
        <div v-if="!isMCPNode && !isImageGenNode" class="field">
          <label>请求方法与 URL（URL 中 {'{'}var{'}'} 会被同名输入参数替换）</label>
          <div class="var-row">
            <a-select v-model="httpForm.method" size="mini" style="width: 90px">
              <a-option v-for="m in HTTP_METHODS" :key="m" :value="m">{{ m }}</a-option>
            </a-select>
            <a-input v-model="httpForm.url" size="mini" placeholder="https://..." style="flex: 1" />
          </div>
        </div>
        <div v-if="!isMCPNode && !isImageGenNode" class="field">
          <label>请求头</label>
          <div v-for="(h, i) in httpForm.headers" :key="i" class="var-row">
            <a-input v-model="h.name" size="mini" placeholder="Header 名" style="width: 130px" />
            <a-input v-model="h.ref.literal" size="mini" placeholder="值" style="flex: 1" />
            <a-button size="mini" status="danger" @click="removeHttpHeader(i)">删</a-button>
          </div>
          <a-button size="mini" long @click="addHttpHeader">+ 添加请求头</a-button>
        </div>
        <div v-if="!isMCPNode && !isImageGenNode" class="field">
          <label>请求体类型</label>
          <a-radio-group v-model="httpForm.bodyType" size="mini" type="button">
            <a-radio value="none">无</a-radio>
            <a-radio value="json">JSON</a-radio>
          </a-radio-group>
        </div>
        <div v-if="!isMCPNode && !isImageGenNode && httpForm.bodyType === 'json' && !httpForm.bodyRaw" class="field">
          <label>
            JSON 请求体（键值对，值可引用上游变量）
            <a-button size="mini" type="text" style="margin-left: 8px" @click="httpForm.bodyRaw = true">高级模式</a-button>
          </label>
          <div v-for="(f, i) in httpForm.bodyFields" :key="i" class="var-col">
            <div class="var-row">
              <a-input v-model="f.key" size="mini" placeholder="字段名" style="width: 130px" />
              <a-radio-group v-model="f.mode" size="mini" type="button">
                <a-radio value="literal">字面量</a-radio>
                <a-radio value="ref">引用</a-radio>
              </a-radio-group>
              <a-button size="mini" status="danger" @click="removeHttpBodyField(i)">删</a-button>
            </div>
            <div class="var-row">
              <a-select v-if="f.mode === 'ref'" size="mini" placeholder="选择上游变量" style="flex: 1" allow-search
                :model-value="f.ref.blockID ? `${f.ref.blockID}::${f.ref.varName}` : ''"
                @change="(v) => onVarPick(f.ref, v)">
                <a-optgroup v-for="g in varOptions" :key="g.label" :label="g.label">
                  <a-option v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</a-option>
                </a-optgroup>
              </a-select>
              <a-input v-else v-model="f.literal" size="mini" placeholder="值" style="flex: 1" />
            </div>
          </div>
          <a-button size="mini" long @click="addHttpBodyField">+ 添加字段</a-button>
        </div>
        <div v-if="!isMCPNode && !isImageGenNode && httpForm.bodyType === 'json' && httpForm.bodyRaw" class="field">
          <label>
            JSON 请求体（高级模式：嵌套/数组结构）
            <a-button size="mini" type="text" style="margin-left: 8px" @click="httpForm.bodyRaw = false">键值对模式</a-button>
          </label>
          <textarea v-model="httpForm.bodyJSON" class="nd-textarea nd-code" rows="6" spellcheck="false" />
        </div>
        <div class="field">
          <label>输出变量</label>
          <div v-for="(o, i) in outputs" :key="i" class="var-row">
            <a-input v-model="o.name" size="mini" placeholder="变量名" style="width: 120px" />
            <a-input v-model="o.description" size="mini" placeholder="描述" style="flex: 1" />
            <a-button size="mini" status="danger" @click="removeOutput(i)">删</a-button>
          </div>
          <a-button size="mini" long @click="addOutput">+ 添加变量</a-button>
        </div>
      </template>

      <!-- 其他类型：JSON 兜底 -->
      <template v-else>
        <div class="field">
          <label>节点配置（JSON）</label>
          <textarea v-model="rawText" class="nd-textarea nd-code" rows="14" spellcheck="false" />
        </div>
      </template>

      <!-- 试运行 -->
      <div v-if="declaredInputs.length" class="field">
        <label>试运行输入（{{ typeName }} 依赖的变量）</label>
        <div v-for="n in declaredInputs" :key="n" class="var-row">
          <span class="input-name">{{ n }}</span>
          <a-input v-model="testInput[n]" size="mini" placeholder="测试值" style="flex: 1" />
        </div>
      </div>

      <div class="nd-actions">
        <a-button v-if="canTest" size="small" :loading="testing" @click="runTest">
          <IconPlayCircle /> 试运行
        </a-button>
        <a-button size="small" type="primary" @click="apply">应用</a-button>
      </div>

      <div v-if="testResult" class="test-result">
        <div class="tr-head">
          <a-tag v-if="testResult.status === 'success'" color="green" size="small">成功 · {{ testResult.durationMs }}ms</a-tag>
          <a-tag v-else color="red" size="small">失败</a-tag>
        </div>
        <pre class="tr-body">{{ testResult.failReason || displayTestOutput }}</pre>
      </div>
    </div>
  </a-drawer>
</template>

<style scoped>
.nd-body { display: flex; flex-direction: column; gap: 14px; }
.field label { display: block; font-size: 12px; color: var(--color-text-2, #4e5969); margin-bottom: 4px; }
.var-row { display: flex; align-items: center; gap: 6px; margin-bottom: 6px; }
.var-col { border: 1px dashed var(--color-border-2, #e5e6eb); border-radius: 6px; padding: 8px; margin-bottom: 6px; }
.branch-card { border: 1px solid var(--color-border-2, #e5e6eb); border-radius: 8px; padding: 8px; margin-bottom: 8px; }
.branch-head { display: flex; align-items: center; justify-content: space-between; font-size: 12px; font-weight: 600; margin-bottom: 6px; }
.cond-row { flex-wrap: nowrap; }
.hint { font-size: 11px; color: var(--color-text-3, #86909c); margin-top: 4px; }
.nd-textarea {
  width: 100%; font-size: 12px; line-height: 1.5;
  border: 1px solid var(--color-border-2, #e5e6eb); border-radius: 6px; padding: 8px;
  resize: vertical; background: var(--color-bg-2, #fff); color: var(--color-text-1, #1d2129);
}
.nd-code { font-family: Consolas, Monaco, monospace; }
.nd-actions { display: flex; justify-content: flex-end; gap: 8px; border-top: 1px solid var(--color-border-2, #e5e6eb); padding-top: 10px; }
.input-name { font-family: Consolas, monospace; font-size: 12px; color: #165dff; min-width: 70px; }
.test-result { border: 1px solid var(--color-border-2, #e5e6eb); border-radius: 8px; padding: 8px; }
.tr-head { margin-bottom: 6px; }
.tr-body {
  margin: 0; font-family: Consolas, monospace; font-size: 12px; line-height: 1.5;
  max-height: 220px; overflow: auto; white-space: pre-wrap;
  background: var(--color-fill-1, #f7f8fa); border-radius: 6px; padding: 8px;
}
</style>
