// Package flow 流程编排：基于 Eino (github.com/cloudwego/eino) compose.Chain 的
// 可视化流程定义与运行引擎。
//
// 节点类型对齐 Eino 官方标准组件体系（eino/components/*）与 compose.Chain
// 的官方 Append* 节点：
//
//	template  → prompt.ChatTemplate（FromMessages，FString {var}）   [AppendChatTemplate]
//	llm       → model.BaseChatModel（chatModel 适配 llm.Client）     [AppendChatModel]
//	tools     → compose.ToolsNode（Eino tool.InvokableTool 适配）    [AppendToolsNode]
//	loader    → document.Loader（本地文件 → schema.Document）        [AppendLoader]
//	transformer → document.Transformer（文档拆分/合并）              [AppendTransformer*]
//	embedding → embedding.Embedder（文本向量化）                     [AppendEmbedding]
//	indexer   → indexer.Indexer（内存向量索引）                       [AppendIndexer]
//	retriever → retriever.Retriever（内存向量检索）                   [AppendRetriever]
//	knowledge → 知识库检索（Dify knowledge-retrieval 对齐，持久向量库）    [回调 KB]
//	passthrough → compose.AppendPassthrough（原样透传）               [AppendPassthrough]
//
// 每个节点经 Compile 后 Invoke 顺序执行；节点间传递 *State（变量 + 消息历史 +
// 文档集 + 上一步输出），支持逐步回调（供 UI 跟踪）。
package flow

import (
	"fmt"
	"time"
)

// StepType 步骤（节点）类型 —— 与 Eino 官方组件一一对应。
const (
	StepTemplate    = "template"    // prompt.ChatTemplate：FString {var} 渲染消息
	StepLLM         = "llm"         // model.BaseChatModel：调用对话模型
	StepTool        = "tools"       // tool.InvokableTool：调用 Kaelvoris 工具（ToolsNode）
	StepLoader      = "loader"      // document.Loader：读取本地文件为文档
	StepTransformer = "transformer" // document.Transformer：文档拆分/合并
	StepEmbedding   = "embedding"   // embedding.Embedder：文本向量化
	StepIndexer     = "indexer"     // indexer.Indexer：文档入内存向量索引
	StepRetriever   = "retriever"   // retriever.Retriever：按查询检索文档
	StepPassthrough = "passthrough" // compose.AppendPassthrough：透传

	// 结构性节点（2026-10 UI 升级：画布编排语义补全）：
	StepStart  = "start"  // 开始节点：流程唯一入口标记（执行时透传，不产生业务步骤）
	StepEnd    = "end"    // 结束节点：流程唯一出口标记（执行时透传，不产生业务步骤）
	StepBranch = "branch" // 条件分支：按出边条件表达式路由（仅图执行，不出现在线性 Steps）

	// 代码转换（2026-10 Dify 语义融入）：Python/JS 代码执行节点（template-transform 对齐）。
	// Dify 对应节点：api/core/workflow/nodes/code（沙箱执行代码、输出变量写回变量池）。
		StepTransform = "transform"

		// HTTP 请求（2026-10 Dify/Coze http-request 对齐）：方法（Step.Role）
		// + 请求定义 JSON（Step.Template），响应状态码/响应体写回变量池。
		StepHTTP = "http"

		// 变量赋值（2026-10 Dify variable-assigner/variable-aggregator 对齐）：
		// Step.Name = 目标变量名，值表达式渲染后写入用户变量表与输出池。
		StepAssigner = "assigner"

		// 参数提取器（2026-10 Dify parameter-extractor 对齐）：LLM 按参数清单
		// 从输入文本提取结构化 JSON，必填校验后逐参数写回变量池。
		StepExtractor = "extractor"

		// 迭代（2026-10 Dify iteration / Coze loop 对齐）：列表逐项跑子步骤链，
		// 输出聚合写回变量池。
		StepIteration = "iteration"

		// 知识库检索（2026-10 Dify 语义融入）：knowledge-retrieval 对齐。
	// Dify 对应节点：api/core/workflow/nodes/knowledge-retrieval（查询渲染 →
	// 知识库 TopK 检索 → 结果写回变量池供下游 {nodeID.output} 引用）。
	// 检索后端为持久向量库（internal/vector，设置 → 向量库 索引产物），
	// 经 Runner.KB 回调注入（flow 包不反向依赖 vector，保持内核零耦合）。
		StepKnowledge = "knowledge"

		// 子流程（2026-10-27 Coze subworkflow / Dify workflow 节点对齐）：
		// 在当前流程中调用另一条已保存流程（流程复用的正式机制）。
		// Step.Template 定义 JSON：{"flow": "<flowID>", "vars": {"k": "{expr}"}}，
		// vars 逐值渲染后作为子流程的输入变量；子流程最终输出写回
		// {nodeID.output}。执行经 Runner.SubFlow 回调注入（flow 包不反向
		// 依赖宿主流程存储，保持内核零耦合，与 KB/MCPResolver 同套路）。
		StepSubflow = "subflow"
	)

	// StepTypes 全部合法节点类型（保存校验用）。
	var StepTypes = map[string]bool{
		StepTemplate: true, StepLLM: true, StepTool: true,
		StepLoader: true, StepTransformer: true, StepEmbedding: true,
		StepIndexer: true, StepRetriever: true, StepPassthrough: true,
		StepStart: true, StepEnd: true, StepBranch: true,
		StepTransform: true, StepKnowledge: true,
		StepHTTP: true, StepAssigner: true, StepExtractor: true, StepIteration: true,
		StepSubflow: true,
	}

// Step 流程步骤（节点）。
type Step struct {
	Type string `json:"type"` // template|llm|tools|loader|transformer|embedding|indexer|retriever|passthrough
	// Role template：system | user | assistant；transform：python | javascript（默认 python）
	Role string `json:"role,omitempty"`
	// Template template 步骤模板文本；transformer：split(拆分)/join(合并)；retriever/indexer 相关查询可含 {var}
	// transform 步骤：代码脚本（语言由 Role 字段复用，见上）
	Template string `json:"template,omitempty"`
	// Name 步骤显示名（前端可改）；tools 步骤为工具名（Kaelvoris Registry 注册名）
	Name string `json:"name,omitempty"`
	// Model llm 节点模型覆盖（空 = 用 Runner 全局 LLM；Dify LLM 节点
	// model 对齐：同画布多 llm 节点可各选模型）。
	Model string `json:"model,omitempty"`
	// TopK retriever 检索条数（默认 3）；knowledge 知识库检索条数（默认 8）
	TopK int `json:"topK,omitempty"`
	// MCPServers llm 节点引用的 MCP 服务器名清单（设置 → MCP 面板同名条目；
	// 执行时连接这些服务器，其全部工具以 function calling 暴露给模型，
	// 模型可多轮调用后产出最终回复；空 = 不挂任何 MCP 工具）。
	MCPServers []string `json:"mcpServers,omitempty"`
	// Skills llm 节点引用的技能名清单（技能库 SKILL.md 同名条目；执行时
	// 加载技能正文注入 system 消息（低优先级，排在模板 system 之后），
	// 模型按技能指引回答；空 = 不注入技能）。
	Skills []string `json:"skills,omitempty"`
	// MinScore knowledge 知识库检索相似度阈值（0 = 用库默认 0.35）
	MinScore float64 `json:"minScore,omitempty"`
}

// Flow 一次流程编排定义（持久化为 ~/.kaelvoris/flows/<id>.json）。
type Flow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Steps       []Step `json:"steps"`
	// AppID 所属应用 ID；空 = 归入「默认应用」（旧数据兼容）。
	AppID string `json:"appId,omitempty"`
	// Canvas 画布布局与节点配置；nil = 旧数据，保存时由 CanvasFromSteps 自动生成。
	Canvas    *Canvas `json:"canvas,omitempty"`
	CreatedAt int64   `json:"createdAt"`
	UpdatedAt int64   `json:"updatedAt"`
}

// Pos 画布节点坐标。
type Pos struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// CanvasNode 画布节点：稳定 ID + 类型 + 坐标 + 步骤配置（复用 Step）。
type CanvasNode struct {
	ID   string `json:"id"`   // 稳定节点 ID（n_<ts>_<seq>）
	Type string `json:"type"` // 同 Step.Type（9 类）
	Pos  Pos    `json:"pos"`
	Step Step   `json:"step"`
}

// CanvasEdge 画布连线：源 → 目标节点 ID；Expr 仅 branch 节点出边使用
// （条件表达式，可含 {var}，空串 = else 兜底分支，运行时求值路由）。
type CanvasEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	// Expr 条件表达式（仅 branch 源节点出边有效），如 "{score} > 80"、
	// "{name} contains vip"；空 = else 兜底分支（至多一条）。
	Expr string `json:"expr,omitempty"`
}

// Canvas 画布拓扑（P1 为单向线性链）。
type Canvas struct {
	Nodes []CanvasNode `json:"nodes"`
	Edges []CanvasEdge `json:"edges"`
}

// CanvasFromSteps 旧数据兼容：按 Steps 顺序生成纵向链式画布
// （x 固定 0，y 每节点递增 120px），节点 ID 规则 n<i>_<ts>，
// 并自动补齐 start/end 结构节点（EnsureStartEnd），旧数据无感升级。
func CanvasFromSteps(f *Flow) *Canvas {
	ts := time.Now().UnixMilli()
	c := &Canvas{
		Nodes: make([]CanvasNode, 0, len(f.Steps)),
		Edges: make([]CanvasEdge, 0, len(f.Steps)),
	}
	var prev string
	for i, s := range f.Steps {
		id := fmt.Sprintf("n%d_%d", i, ts)
		c.Nodes = append(c.Nodes, CanvasNode{
			ID:   id,
			Type: s.Type,
			Pos:  Pos{X: 0, Y: float64(i) * 120},
			Step: s,
		})
		if prev != "" {
			c.Edges = append(c.Edges, CanvasEdge{ID: fmt.Sprintf("e%d_%d", i, ts), Source: prev, Target: id})
		}
		prev = id
	}
	_ = EnsureStartEnd(c) // 旧数据升级：补齐 start/end 结构节点（链式画布必可补）
	return c
}

// Summary 流程列表条目。
type Summary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	StepCount   int    `json:"stepCount"`
	UpdatedAt   int64  `json:"updatedAt"`
}

// Summary 生成列表摘要。
func (f *Flow) Summary() Summary {
	return Summary{
		ID:          f.ID,
		Name:        f.Name,
		Description: f.Description,
		StepCount:   len(f.Steps),
		UpdatedAt:   f.UpdatedAt,
	}
}

// AppSummary 应用列表条目（flowapps 列表用）。
type AppSummary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Desc      string `json:"desc,omitempty"`
	Icon      string `json:"icon,omitempty"`
	FlowCount int    `json:"flowCount"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// FlowApp 应用（工作流容器）：持久化为 ~/.kaelvoris/flowapps/<id>.json。
// Vars/Config 为 P2 全局变量池与默认配置；AppKey 为 P3 对外密钥（字段先行，逻辑分期）。
type FlowApp struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Desc      string            `json:"desc,omitempty"`
	Icon      string            `json:"icon,omitempty"`
	Vars      map[string]string `json:"vars,omitempty"`
	Config    map[string]string `json:"config,omitempty"`
	AppKey    string            `json:"appKey,omitempty"`
	CreatedAt int64             `json:"createdAt"`
	UpdatedAt int64             `json:"updatedAt"`
}

// Summary 生成应用列表摘要（FlowCount 由绑定层填充）。
func (a *FlowApp) Summary() AppSummary {
	return AppSummary{
		ID:        a.ID,
		Name:      a.Name,
		Desc:      a.Desc,
		Icon:      a.Icon,
		CreatedAt: a.CreatedAt,
		UpdatedAt: a.UpdatedAt,
	}
}

// StepTrace 单步运行轨迹（回调 + 结果展示）。
type StepTrace struct {
	Index  int    `json:"index"`          // 步骤序号（0 基，拓扑执行序）
	Type   string `json:"type"`           // 节点类型（同 Step.Type）
	Name   string `json:"name,omitempty"` // 步骤名
	Render string `json:"render"`         // 渲染文本 / 模型输出 / 工具结果 / 检索摘要
	Err    string `json:"err,omitempty"`  // 失败原因
	// NodeID 画布节点稳定 ID（分支流程下 Index 不再可靠对应线性链，
	// 前端高亮优先按 NodeID 映射；旧事件消费方按 Index 兕底）。
	NodeID string `json:"nodeId,omitempty"`
	// Docs knowledge 节点命中条数（Dify knowledge-retrieval 对齐，前端展示用）。
	Docs int `json:"docs,omitempty"`
	// HTTPStatus http 节点响应状态码（Dify http-request 对齐，前端展示用）。
	HTTPStatus int `json:"httpStatus,omitempty"`
}

// RunResult 一次运行结果。
type RunResult struct {
	FlowID   string      `json:"flowId"`
	Output   string      `json:"output"`           // 最终 assistant 输出（最后 llm 步骤）
	Steps    []StepTrace `json:"steps"`            // 逐步轨迹
	Duration int64       `json:"durationMs"`       // 总耗时毫秒
	DryRun   bool        `json:"dryRun,omitempty"` // dry-run：跳过 llm/tools 步骤
}
