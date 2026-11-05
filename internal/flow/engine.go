// 流程编排运行引擎：每个节点桥接 Eino 官方标准组件（eino/components），
// 经 compose.Chain Compile 后 Invoke 顺序执行：
//
//	template    → 官方 prompt.ChatTemplate（FromMessages + FString） [AppendChatTemplate]
//	llm         → 官方 model.BaseChatModel（chatModel 适配 llm.Client）[AppendChatModel]
//	tools       → 官方 tool.InvokableTool（einoTool 适配 Kaelvoris Registry）[AppendToolsNode]
//	loader      → document.Loader（读本地文件 → schema.Document）      [AppendLoader]
//	transformer → document.Transformer（split 拆分 / join 合并）       [AppendTransformer]
//	embedding   → embedding.Embedder（本地哈希向量，dim=256）          [AppendEmbedding]
//	indexer     → indexer.Indexer（内存余弦向量索引）                  [AppendIndexer]
//	retriever   → retriever.Retriever（内存 TopK 检索）                [AppendRetriever]
//	knowledge   → 知识库检索（Dify knowledge-retrieval，持久向量库）    [回调 KB]
//	passthrough → 官方 Passthrough 原样透传                            [AppendPassthrough]
//
// 链上以 Lambda 桥接节点调用官方组件接口方法（Format/InvokableRun/Load/
// Transform/EmbedStrings/Store/Retrieve），组件与官方 Append* 语义一一对应，
// 后续可平滑切换为原生 Append* 拓扑。LLM/工具复用本应用 llm.Client 与
// tools.Registry。
package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/document"
	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/indexer"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/prompt"
	"github.com/cloudwego/eino/components/retriever"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"kaelvoris/internal/llm"
	"kaelvoris/internal/tools"
)

// state Chain 节点间传递的运行状态。
type state struct {
	vars   map[string]any     // 模板变量
	msgs   []*schema.Message  // 对话上下文
	docs   []*schema.Document // 文档流（loader/transformer/indexer/retriever）
	steps  []StepTrace
	output string    // 最后 llm 步骤输出
	index  *memIndex // indexer 节点产物（供 retriever 引用）
	// pools 节点输出变量池（2026-10 Dify 语义融入）：键 = 画布节点 ID，
	// 值 = 该节点输出变量集（output / result / docs）。下游模板与分支条件
	// 以 {nodeID.output} 点分路径引用（FString 与 pyfmt 嵌套取值同构）。
	pools map[string]map[string]any
	// pendingExtract extractor 节点待写回参数（execStepNode 捕获后由
	// RunGraph 以 NodeID 键逐参数写回变量池）。
	pendingExtract map[string]any
	// pendingIterItems iteration 节点待写回子项数组（同上，写为 items）。
	pendingIterItems []string
	// pendingHTTPBody http 节点待写回响应体（覆盖通用轨迹文本写回，
	// 下游 {nodeID.output} 引用真实响应体而非状态摘要）。
	pendingHTTPBody string
	// pendingAppended iteration 子轨迹并入主轨迹的帧数（RunGraph 主循环
	// 据此推进 idx，保证轨迹 Index 单调）。
	pendingAppended int
}

// mergedVars 合并用户变量与节点输出变量池（池优先；transform 脚本取值用）。
func (st *state) mergedVars() map[string]any {
	m := make(map[string]any, len(st.vars)+len(st.pools))
	for k, v := range st.vars {
		m[k] = v
	}
	for k, v := range st.pools {
		m[k] = v
	}
	return m
}

// putPool 向指定节点写入输出变量（nil 接收器初始化）。
func (st *state) putPool(nodeID string, out map[string]any) {
	if st.pools == nil {
		st.pools = map[string]map[string]any{}
	}
	st.pools[nodeID] = out
}

// condSatisfied 画布入边条件门禁（Dify branch 同构语义，2026-10 融入）：
// 入边无条件 = 恒真；有条件则按当前变量池求值，未命中 → 跳过该节点及其
// 下游子图（求值失败视为未命中，不中断流程）。
func (r *Runner) condSatisfied(edges []CanvasEdge, target string, st *state) bool {
	for _, e := range edges {
		if e.Target != target {
			continue
		}
		e := strings.TrimSpace(e.Expr)
		if e == "" {
			continue
		}
		hit, err := evalBranchExpr(context.Background(), e, st.mergedVars())
		if err != nil || !hit {
			return false
		}
	}
	return true
}

// chatModel 将 *llm.Client 适配为 Eino model.BaseChatModel（官方 components/model 接口）。
type chatModel struct {
	client *llm.Client
}

// Generate 实现 model.BaseChatModel（非流式）。
func (c *chatModel) Generate(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	req := llm.ChatRequest{}
	for _, m := range input {
		req.Messages = append(req.Messages, llm.Message{
			Role:    string(m.Role),
			Content: m.Content,
		})
	}
	out, err := c.client.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	return &schema.Message{Role: schema.Assistant, Content: out.Content}, nil
}

// Stream 实现 model.BaseChatModel（编排走 Invoke；此处汇总为单 chunk 流）。
func (c *chatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := c.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()
	return sr, nil
}

// einoTool 将 Kaelvoris tools.Tool 适配为 Eino tool.InvokableTool（官方接口）。
type einoTool struct {
	t tools.Tool
}

// Info 实现 Eino tool.BaseTool。编排场景工具参数由节点属性中的 JSON 串
// 直接透传给 InvokableRun（不走 LLM function-calling 序列化），故无需
// ParamsOneOf（Kaelvoris Parameters() 为 JSONSchema map，与 Eino
// *jsonschema.Schema 类型不同，避免强行转换）。
func (a *einoTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: a.t.Name(),
		Desc: a.t.Description(),
	}, nil
}

// InvokableRun 实现 Eino tool.InvokableTool：JSON 参数串直接透传
// （Kaelvoris 工具协议即 JSON 参数串，语义一致）。
func (a *einoTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...einotool.Option) (string, error) {
	return a.t.Execute(ctx, argumentsInJSON)
}

// einoLoader 将本地文件读取适配为 Eino document.Loader（官方接口）。
type einoLoader struct {
	workspace string
}

// Load 实现 document.Loader：URI 支持绝对路径 / 相对工作区路径 / file:// 前缀。
func (l *einoLoader) Load(ctx context.Context, src document.Source, _ ...document.LoaderOption) ([]*schema.Document, error) {
	p := strings.TrimPrefix(src.URI, "file://")
	if !strings.Contains(p, ":") && !strings.HasPrefix(p, "/") && l.workspace != "" {
		p = l.workspace + string(os.PathSeparator) + p
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", src.URI, err)
	}
	return []*schema.Document{{
		ID:       src.URI,
		Content:  string(b),
		MetaData: map[string]any{"uri": src.URI},
	}}, nil
}

// einoTransformer 文档拆分/合并（实现 Eino document.Transformer 官方接口）。
type einoTransformer struct {
	mode string // split | join
}

func (t *einoTransformer) Transform(ctx context.Context, src []*schema.Document, _ ...document.TransformerOption) ([]*schema.Document, error) {
	if t.mode == "join" {
		var sb strings.Builder
		for i, d := range src {
			if i > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(d.Content)
		}
		return []*schema.Document{{ID: "joined", Content: sb.String()}}, nil
	}
	// split：按空行分段（每段一个文档，保留来源元数据）。
	var out []*schema.Document
	for _, d := range src {
		for i, seg := range strings.Split(d.Content, "\n\n") {
			seg = strings.TrimSpace(seg)
			if seg == "" {
				continue
			}
			out = append(out, &schema.Document{
				ID:       fmt.Sprintf("%s#%d", d.ID, i),
				Content:  seg,
				MetaData: map[string]any{"source": d.ID},
			})
		}
	}
	return out, nil
}

// memIndex 内存向量索引（indexer/retriever 共享载体）。
type memIndex struct {
	mu   sync.Mutex
	docs []*schema.Document
	vecs [][]float64
}

// einoIndexer 内存余弦相似度索引（实现 Eino indexer.Indexer 官方接口）。
type einoIndexer struct {
	ix *memIndex
}

func (n *einoIndexer) Store(ctx context.Context, docs []*schema.Document, _ ...indexer.Option) ([]string, error) {
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.ID)
		n.ix.mu.Lock()
		n.ix.docs = append(n.ix.docs, d)
		n.ix.vecs = append(n.ix.vecs, hashVector(d.Content, embedDim))
		n.ix.mu.Unlock()
	}
	return ids, nil
}

// einoRetriever 在 memIndex 文档集中做余弦 TopK 检索
// （实现 Eino retriever.Retriever 官方接口）。
type einoRetriever struct {
	ix   *memIndex
	topK int
	emb  embedding.Embedder
}

func (r *einoRetriever) Retrieve(ctx context.Context, query string, opts ...retriever.Option) ([]*schema.Document, error) {
	o := retriever.GetCommonOptions(&retriever.Options{TopK: nil}, opts...)
	k := r.topK
	if k <= 0 {
		k = 3
	}
	if o.TopK != nil && *o.TopK > 0 {
		k = *o.TopK
	}
	var qv []float64
	if r.emb != nil {
		vecs, err := r.emb.EmbedStrings(ctx, []string{query})
		if err != nil {
			return nil, err
		}
		qv = vecs[0]
	} else {
		qv = hashVector(query, embedDim)
	}
	r.ix.mu.Lock()
	defer r.ix.mu.Unlock()
	type scored struct {
		d *schema.Document
		s float64
	}
	var out []scored
	for i, d := range r.ix.docs {
		out = append(out, scored{d: d, s: dot(r.ix.vecs[i], qv)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].s > out[j].s })
	var docs []*schema.Document
	for i := 0; i < len(out) && i < k; i++ {
		docs = append(docs, out[i].d)
	}
	return docs, nil
}

// einoEmbedder 本地哈希词袋 Embedder（实现 Eino embedding.Embedder 官方接口；
// 无外部依赖，生产可替换 OpenAI/Ollama 等实现）。
type einoEmbedder struct {
	dim int
}

func (e *einoEmbedder) EmbedStrings(ctx context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	out := make([][]float64, 0, len(texts))
	for _, t := range texts {
		out = append(out, hashVector(t, e.dim))
	}
	return out, nil
}

const embedDim = 256

// hashVector 确定性词袋哈希向量（L2 归一化），本地检索演示/测试用。
func hashVector(text string, dim int) []float64 {
	v := make([]float64, dim)
	for _, w := range strings.FieldsFunc(strings.ToLower(text), isWordRune) {
		v[int(fnv32(w))%dim]++
	}
	n := 0.0
	for _, x := range v {
		n += x * x
	}
	if n > 0 {
		inv := 1 / math.Sqrt(n)
		for i := range v {
			v[i] *= inv
		}
	}
	return v
}

func fnv32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func isWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r >= 0x4e00 && r <= 0x9fff)
}

func dot(a, b []float64) float64 {
	n := 0.0
	for i := range a {
		n += a[i] * b[i]
	}
	return n
}

// Runner 流程运行器。
type Runner struct {
	// LLM 对话模型客户端（nil 时仅允许 dry-run / 纯模板流程）。
	LLM *llm.Client
	// Tools Kaelvoris 工具注册中心（nil 时 tools 节点报错）。
	Tools *tools.Registry
	// Workspace 工具与 loader 的工作区根目录。
	Workspace string
	// OnStep 每步完成回调（nil = 忽略）。
	OnStep func(StepTrace)
	// MCPResolver llm 节点 MCP 引用解析器（2026-10 新增）：按服务器名清单
	// 连接 MCP 服务器，返回 name→tools.Registry（已注册 mcp_<server>_<tool>
	// 全部工具）与关闭函数；nil 或返回 nil = 该节点不挂 MCP 工具。
	// 连接关闭由解析器契约约定：调用方（llm 节点执行器）在 LLM 轮次结束后
	// 必须调用返回的 close。
	MCPResolver func(ctx context.Context, servers []string) (map[string]*tools.Registry, closeFunc, error)
	// SkillLoader llm 节点技能引用加载器：按技能名返回 SKILL.md 正文
	// （frontmatter 之后的 markdown）；nil 或技能不存在 = 跳过该技能。
	SkillLoader func(name string) (string, error)
	// KB 知识库检索回调（knowledge 节点后端，2026-10 Dify knowledge-retrieval
	// 对齐）：入参（查询文本, TopK, 最小相似度），返回（格式化检索文本, 命中数,
	// 错误）。由宿主注入（桌面端桥接 internal/vector 持久向量库）；
	// nil 时 knowledge 节点报错（与 LLM/Tools 同语义）。
	KB func(ctx context.Context, query string, topK int, minScore float64) (string, int, error)
	// LLMFor llm 节点模型选择回调（Dify LLM 节点 model 对齐）：按节点
	// Step.Model 返回专用客户端，空串/未知模型回退全局 LLM；宿主注入
	// （桌面端桥接 ListModels 同源的模型服务）。nil = 全部用 LLM。
	LLMFor func(ctx context.Context, model string) *llm.Client
	// SubFlow 子流程执行回调（2026-10-27 Coze subworkflow / Dify workflow
	// 节点对齐）：入参（目标流程 ID, 输入变量, dryRun），返回子流程完整
	// RunResult。宿主注入（桌面端桥接 loadFlows + runFlowCore 同一执行链路）；
	// nil 时 subflow 节点报错（与 KB/LLM 同语义）。
	SubFlow func(ctx context.Context, flowID string, vars map[string]any, dryRun bool) (*RunResult, error)
}

// llmFor 解析 llm 节点实际使用的客户端：优先 LLMFor(step.Model)，
// 未命中（回调为空/模型为空/未知模型）回退全局 LLM。
func (r *Runner) llmFor(ctx context.Context, step Step) (*llm.Client, error) {
	if step.Model != "" && r.LLMFor != nil {
		if c := r.LLMFor(ctx, step.Model); c != nil {
			return c, nil
		}
		return nil, fmt.Errorf("模型 %q 不可用（请检查模型服务配置或改用全局默认模型）", step.Model)
	}
	if r.LLM == nil {
		return nil, fmt.Errorf("运行器未配置模型客户端")
	}
	return r.LLM, nil
}

// closeFunc 资源关闭函数别名（MCP 连接等）。
type closeFunc = func()

// trace 回调轨迹。
func (r *Runner) trace(t StepTrace) {
	if r.OnStep != nil {
		r.OnStep(t)
	}
}

// templateVars 提取 {var} 占位符名称集合（FString 变量，供变量提示）。
func templateVars(tpl string) []string {
	var out []string
	i := 0
	for i < len(tpl) {
		if tpl[i] == '{' {
			j := strings.IndexByte(tpl[i+1:], '}')
			if j < 0 {
				break
			}
			name := tpl[i+1 : i+1+j]
			if name != "" && !strings.ContainsAny(name, "{}") {
				out = append(out, name)
			}
			i += j + 2
			continue
		}
		i++
	}
	return out
}

// stepRole 模板步骤角色归一化。
func stepRole(role string) schema.RoleType {
	switch role {
	case "system":
		return schema.System
	case "assistant":
		return schema.Assistant
	default:
		return schema.User
	}
}

// invokeLLMIn llm 节点执行上下文（Invoke 与图执行两路共用）。
type invokeLLMIn struct {
	step    Step
	msgs    []*schema.Message // 当前对话上下文（含模板渲染产物）
	verbose bool              // 轨迹透出细节（MCP 工具调用摘要等）
}

// invokeLLM llm 节点公共执行器（2026-10 升级：MCP/技能引用接入）：
//  1. 技能引用（step.Skills）：SkillLoader 逐个加载正文，拼为一段 system
//     消息追加在上下文尾部（模板 system 之后、user 之前语义等价——Eino 链
//     上下文即消息数组，追加段不破坏既有消息次序）；
//  2. MCP 引用（step.MCPServers）：MCPResolver 连接服务器 → 其工具以
//     function calling 定义暴露给模型 → 模型发起 tool_calls 时执行并回填
//     tool 消息再请求，循环至无工具调用（上限 invokeLLMMaxRounds 轮防失控）；
//  3. 无引用时退化为原单次 Generate（零行为变化）。
//
// 返回最终 assistant 输出；close（MCP 连接，可能为 nil）由调用方 defer。
func (r *Runner) invokeLLM(ctx context.Context, in invokeLLMIn) (out string, closeRes func(), err error) {
	msgs := in.msgs
	// ── 技能注入：引用技能的正文拼为一条 system 附加段 ──
	if r.SkillLoader != nil && len(in.step.Skills) > 0 {
		var sb strings.Builder
		loaded := 0
		for _, name := range in.step.Skills {
			body, lerr := r.SkillLoader(strings.TrimSpace(name))
			if lerr != nil || strings.TrimSpace(body) == "" {
				continue // 单个技能加载失败降级跳过（不阻断流程）
			}
			if loaded > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString("## 技能：")
			sb.WriteString(strings.TrimSpace(name))
			sb.WriteString("\n\n")
			sb.WriteString(body)
			loaded++
		}
		if loaded > 0 {
			msgs = append(msgs, &schema.Message{
				Role:    schema.System,
				Content: "以下为本节点引用的技能指引，回答时遵循：\n\n" + sb.String(),
			})
		}
	}
	// ── MCP 工具挂载 ──
	var toolDefs []llm.ToolDef
	callables := map[string]tools.Tool{}
	var mcpClosers []func()
	if r.MCPResolver != nil && len(in.step.MCPServers) > 0 {
		regs, closeRes, rerr := r.MCPResolver(ctx, in.step.MCPServers)
		if rerr != nil {
			return "", nil, fmt.Errorf("MCP 服务器连接失败: %w", rerr)
		}
		if closeRes != nil {
			mcpClosers = append(mcpClosers, closeRes)
		}
		// 多 server 工具合并进单个注册表空间（重名后者覆盖前者——与任务级
		// buildMCPTools 全局命名空间语义一致）。
		merged := tools.NewRegistry()
		for _, reg := range regs {
			for _, name := range reg.Names() {
				if t, ok := reg.Get(name); ok {
					_ = merged.Register(t)
				}
			}
		}
		for _, name := range merged.Names() {
			t, ok := merged.Get(name)
			if !ok {
				continue
			}
			callables[name] = t
			toolDefs = append(toolDefs, llm.ToolDef{
				Type: "function",
				Function: llm.ToolFunc{
					Name:        t.Name(),
					Description: t.Description(),
					Parameters:  t.Parameters(),
				},
			})
		}
	}
	closeRes = func() {
		for _, c := range mcpClosers {
			if c != nil {
				c()
			}
		}
	}

	// ── 模型选择（2026-10 融合 Dify 节点级 model）：优先 LLMFor(step.Model)，
	// 未命中回退全局 LLM；两者皆缺 = 未配置模型客户端。
	cm, lerr := r.llmFor(ctx, in.step)
	if lerr != nil {
		return "", closeRes, lerr
	}
	// ── 对话循环：无工具 = 单次 Generate；有工具 = function calling 循环 ──
	conv := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		conv = append(conv, llm.Message{Role: string(m.Role), Content: m.Content})
	}
	const maxRounds = invokeLLMMaxRounds
	var finalText string
	for round := 0; ; round++ {
		resp, gerr := cm.Generate(ctx, llm.ChatRequest{Messages: conv, Tools: toolDefs})
		if gerr != nil {
			return "", closeRes, gerr
		}
		if len(resp.ToolCalls) == 0 || round >= maxRounds-1 {
			finalText = resp.Content
			break
		}
		// assistant 工具调用消息入上下文（OpenAI 规范：assistant.tool_calls
		// 必须先于对应 tool 消息）。
		conv = append(conv, llm.Message{Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})
		if in.verbose {
			names := make([]string, 0, len(resp.ToolCalls))
			for _, tc := range resp.ToolCalls {
				names = append(names, tc.Name)
			}
			r.trace(StepTrace{Index: -1, Type: in.step.Type, Name: in.step.Name, Render: "🔧 MCP 工具调用: " + strings.Join(names, ", ")})
		}
		// 逐个执行工具调用并回填 tool 消息。
		for _, tc := range resp.ToolCalls {
			var res string
			if t, ok := callables[tc.Name]; ok {
				res, gerr = t.Execute(ctx, tc.Arguments)
				if gerr != nil {
					res = "工具执行失败: " + gerr.Error() // 失败结果回填给模型自行纠正
				}
			} else {
				res = "工具未注册: " + tc.Name
			}
			conv = append(conv, llm.Message{Role: llm.RoleTool, Content: res, ToolCallID: tc.ID})
		}
	}
	return finalText, closeRes, nil
}

// invokeLLMMaxRounds llm 节点 MCP function calling 循环轮次上限（防模型
// 无限调用工具失控；达到上限后强制收口取当前文本输出）。
const invokeLLMMaxRounds = 8

// renderVars 容错扫描渲染（2026-10-27 缺陷修复）：此前直接用 FString
// Format，但 FString 把一切花括号当占位符——JSON 文本中的结构花括号
//（如 tools 入参 {"q": "{kw}"}、http body）会触发 could not find key
// 渲染失败，调用方吞错后原样透传，{var} 实际不生效。现改为仅把
// 「合法引用形态」（标识符/点分路径 {var} / {nodeID.output}）当占位符，
// 其余花括号（JSON 结构）原样保留；缺失引用仍报错（语义与 FString 一致）。
func renderVars(ctx context.Context, tpl string, vars map[string]any) (string, error) {
	_ = ctx
	if !strings.Contains(tpl, "{") {
		return tpl, nil
	}
	var sb strings.Builder
	missing := ""
	i := 0
	for i < len(tpl) {
		j := strings.IndexByte(tpl[i:], '{')
		if j < 0 {
			sb.WriteString(tpl[i:])
			break
		}
		sb.WriteString(tpl[i : i+j])
		k := strings.IndexByte(tpl[i+j:], '}')
		if k < 0 {
			sb.WriteString(tpl[i+j:])
			break
		}
			inner := tpl[i+j+1 : i+j+k]
			if isVarRef(inner) {
				if v, ok := lookupVar(vars, inner); ok {
					sb.WriteString(fmt.Sprint(v))
				} else {
					if missing == "" {
						missing = inner
					}
					sb.WriteString(tpl[i+j : i+j+k+1]) // 原样保留，最后统一报错
				}
				i += j + k + 1
			} else {
				// 非引用（JSON 结构等）：仅保留当前 '{' 并前进 1 字符继续扫描，
				// 嵌套在内部的 {var}（如 {"q": "{kw}"}）仍可命中替换。
				sb.WriteByte('{')
				i += j + 1
			}
	}
	if missing != "" {
		return "", fmt.Errorf("could not find key: %s", missing)
	}
	return sb.String(), nil
}

// isVarRef 花括号内容是否为合法变量引用形态：标识符段（字母/下划线开头，
// 字母数字下划线组成）以点相连，如 name / n173_1.output / app.vars.key。
func isVarRef(s string) bool {
	if s == "" {
		return false
	}
	for _, seg := range strings.Split(s, ".") {
		if seg == "" {
			return false
		}
		for ri := 0; ri < len(seg); ri++ {
			c := seg[ri]
			switch {
			case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			case ri > 0 && c >= '0' && c <= '9':
			default:
				return false
			}
		}
		if seg[0] >= '0' && seg[0] <= '9' {
			return false
		}
	}
	return true
}

// lookupVar 点分路径取值：顶层查合并变量表（用户变量 + 节点输出池），
// 后续段逐层下钻 map[string]any（{nodeID.output} 同构）。
func lookupVar(vars map[string]any, path string) (any, bool) {
	var cur any = vars
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// Run 编译并执行流程。vars 为模板变量；dryRun 跳过 llm/tools 步骤。
func (r *Runner) Run(ctx context.Context, f *Flow, vars map[string]any, dryRun bool) (*RunResult, error) {
	if len(f.Steps) == 0 {
		return nil, fmt.Errorf("流程无步骤")
	}
	if vars == nil {
		vars = map[string]any{}
	}

	start := time.Now()
	st := &state{vars: vars, pools: map[string]map[string]any{}}

	// 构建 Eino Chain：每个节点一个 Lambda（桥接官方组件），顺序串联。
	// 模板渲染走官方 prompt.ChatTemplate.Format（FString，{var} 语义）；
	// LLM 经 chatModel（Eino model.BaseChatModel 适配）→ llm.Client.Generate；
	// 工具经 Eino tool.InvokableTool（einoTool 适配 Kaelvoris Registry）；
	// 文档/向量组件实现 Eino document/embedding/indexer/retriever 官方接口。
	chain := compose.NewChain[*state, *state]()
	for i, s := range f.Steps {
		idx, step := i, s
		switch step.Type {
		case StepTemplate:
			// 官方 ChatTemplate：prompt.FromMessages + FString。
			tplNode := prompt.FromMessages(schema.FString, &schema.Message{Role: stepRole(step.Role), Content: step.Template})
			chain.AppendLambda(compose.InvokableLambda(
				func(ctx context.Context, in *state) (*state, error) {
					trace := StepTrace{Index: idx, Type: StepTemplate, Name: step.Name}
					rendered, err := tplNode.Format(ctx, in.vars)
					if err != nil {
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, fmt.Errorf("步骤 %d 模板渲染失败: %w", idx+1, err)
					}
					if len(rendered) == 0 {
						err := fmt.Errorf("模板渲染结果为空")
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					in.msgs = append(in.msgs, rendered...)
					trace.Render = rendered[0].Content
					in.steps = append(in.steps, trace)
					r.trace(trace)
					return in, nil
				},
			), compose.WithNodeKey(fmt.Sprintf("tpl_%d", idx)))
		case StepLLM:
			if r.LLM == nil && r.LLMFor == nil {
				return nil, fmt.Errorf("步骤 %d 为 llm 节点，但运行器未配置模型客户端", idx+1)
			}
			if dryRun {
				chain.AppendLambda(compose.InvokableLambda(
					func(ctx context.Context, in *state) (*state, error) {
						trace := StepTrace{Index: idx, Type: StepLLM, Name: step.Name, Render: "(dry-run 跳过)"}
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, nil
					},
				), compose.WithNodeKey(fmt.Sprintf("llm_%d", idx)))
				continue
			}
			// LLM 节点：统一走 invokeLLM（融合 2026-10 两侧能力）：
			//   - 本地 Coze 语义：MCP 引用 → function calling 循环 + 技能注入，
			//     未引用时退化为等价单次 Generate（语义不变）；
			//   - 远程 Dify 语义：节点自带提示词（step.Template 非空则 {var}
			//     渲染后以 user 消息追加）+ 节点级模型选择（step.Model 经 llmFor）。
			chain.AppendLambda(compose.InvokableLambda(
				func(ctx context.Context, in *state) (*state, error) {
					trace := StepTrace{Index: idx, Type: StepLLM, Name: step.Name}
					if p := strings.TrimSpace(step.Template); p != "" {
						if rendered, rerr := renderVars(ctx, p, in.vars); rerr == nil && strings.TrimSpace(rendered) != "" {
							in.msgs = append(in.msgs, &schema.Message{Role: schema.User, Content: rendered})
						}
					}
					out, closeRes, err := r.invokeLLM(ctx, invokeLLMIn{step: step, msgs: in.msgs})
					if closeRes != nil {
						defer closeRes()
					}
					if err != nil {
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, fmt.Errorf("步骤 %d 模型调用失败: %w", idx+1, err)
					}
					trace.Render = out
					in.output = out
					in.msgs = append(in.msgs, &schema.Message{Role: schema.Assistant, Content: out})
					in.steps = append(in.steps, trace)
					r.trace(trace)
					return in, nil
				},
			), compose.WithNodeKey(fmt.Sprintf("llm_%d", idx)))
		case StepTool:
			// 工具节点：Kaelvoris Registry 工具经 Eino tool.InvokableTool 适配执行
			// （等价 compose.AppendToolsNode 单工具路径；入参支持 {var} 渲染的 JSON）。
			if dryRun {
				chain.AppendLambda(compose.InvokableLambda(
					func(ctx context.Context, in *state) (*state, error) {
						trace := StepTrace{Index: idx, Type: StepTool, Name: step.Name, Render: "(dry-run 跳过)"}
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, nil
					},
				), compose.WithNodeKey(fmt.Sprintf("tool_%d", idx)))
				continue
			}
			chain.AppendLambda(compose.InvokableLambda(
				func(ctx context.Context, in *state) (*state, error) {
					trace := StepTrace{Index: idx, Type: StepTool, Name: step.Name}
					if strings.TrimSpace(step.Name) == "" {
						err := fmt.Errorf("工具节点未指定工具名")
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					gt, ok := r.Tools.Get(step.Name)
					if !ok {
						err := fmt.Errorf("工具未注册: %s", step.Name)
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					args := step.Template
					if args != "" {
						if rendered, ferr := renderVars(ctx, args, in.mergedVars()); ferr == nil {
							args = rendered
						}
					}
					res, err := gt.Execute(ctx, args)
					if err != nil {
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, fmt.Errorf("步骤 %d 工具 %s 执行失败: %w", idx+1, step.Name, err)
					}
					trace.Render = res
					in.msgs = append(in.msgs, &schema.Message{Role: schema.Tool, Content: res})
					in.steps = append(in.steps, trace)
					r.trace(trace)
					return in, nil
				},
			), compose.WithNodeKey(fmt.Sprintf("tool_%d", idx)))
		case StepLoader:
			// 官方 document.Loader：读取本地文件为 schema.Document。
			chain.AppendLambda(compose.InvokableLambda(
				func(ctx context.Context, in *state) (*state, error) {
					trace := StepTrace{Index: idx, Type: StepLoader, Name: step.Name}
					uri := step.Template
					if uri != "" {
						if rendered, ferr := renderVars(ctx, uri, in.vars); ferr == nil {
							uri = rendered
						}
					}
					ld := &einoLoader{workspace: r.Workspace}
					docs, err := ld.Load(ctx, document.Source{URI: uri})
					if err != nil {
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					in.docs = append(in.docs, docs...)
					trace.Render = fmt.Sprintf("载入 %d 篇文档：%s", len(docs), uri)
					in.steps = append(in.steps, trace)
					r.trace(trace)
					return in, nil
				},
			), compose.WithNodeKey(fmt.Sprintf("loader_%d", idx)))
		case StepTransformer:
			// 官方 document.Transformer：split（空行拆分）/ join（合并）。
			mode := step.Template
			if mode != "join" {
				mode = "split"
			}
			tr := &einoTransformer{mode: mode}
			chain.AppendLambda(compose.InvokableLambda(
				func(ctx context.Context, in *state) (*state, error) {
					trace := StepTrace{Index: idx, Type: StepTransformer, Name: step.Name}
					n := len(in.docs)
					out, err := tr.Transform(ctx, in.docs)
					if err != nil {
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					in.docs = out
					trace.Render = fmt.Sprintf("%s：%d → %d 篇文档", mode, n, len(out))
					in.steps = append(in.steps, trace)
					r.trace(trace)
					return in, nil
				},
			), compose.WithNodeKey(fmt.Sprintf("trans_%d", idx)))
		case StepEmbedding:
			// 官方 embedding.Embedder：文档向量化（本地哈希实现）。
			chain.AppendLambda(compose.InvokableLambda(
				func(ctx context.Context, in *state) (*state, error) {
					trace := StepTrace{Index: idx, Type: StepEmbedding, Name: step.Name}
					if len(in.docs) == 0 {
						err := fmt.Errorf("无文档可向量化（请前置 loader 节点）")
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					em := &einoEmbedder{dim: embedDim}
					texts := make([]string, 0, len(in.docs))
					for _, d := range in.docs {
						texts = append(texts, d.Content)
					}
					vecs, err := em.EmbedStrings(ctx, texts)
					if err != nil {
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					// 向量附到文档元数据（演示：indexer 用本地哈希重算，保持一致）。
					for i, d := range in.docs {
						if d.MetaData == nil {
							d.MetaData = map[string]any{}
						}
						d.MetaData["__vec__"] = vecs[i]
					}
					trace.Render = fmt.Sprintf("向量化 %d 篇文档（dim=%d）", len(vecs), embedDim)
					in.steps = append(in.steps, trace)
					r.trace(trace)
					return in, nil
				},
			), compose.WithNodeKey(fmt.Sprintf("embed_%d", idx)))
		case StepIndexer:
			// 官方 indexer.Indexer：文档入内存向量索引。
			chain.AppendLambda(compose.InvokableLambda(
				func(ctx context.Context, in *state) (*state, error) {
					trace := StepTrace{Index: idx, Type: StepIndexer, Name: step.Name}
					if len(in.docs) == 0 {
						err := fmt.Errorf("无文档可索引（请先 loader/transformer 节点）")
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					ix := &memIndex{}
					nd := &einoIndexer{ix: ix}
					ids, err := nd.Store(ctx, in.docs)
					if err != nil {
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					in.index = ix // 供后续 retriever 节点引用
					in.docs = nil
					trace.Render = fmt.Sprintf("已索引 %d 篇文档（内存向量索引）", len(ids))
					in.steps = append(in.steps, trace)
					r.trace(trace)
					return in, nil
				},
			), compose.WithNodeKey(fmt.Sprintf("index_%d", idx)))
		case StepRetriever:
			// 官方 retriever.Retriever：内存 TopK 检索，结果写入文档流。
			chain.AppendLambda(compose.InvokableLambda(
				func(ctx context.Context, in *state) (*state, error) {
					trace := StepTrace{Index: idx, Type: StepRetriever, Name: step.Name}
					query := step.Template
					if query != "" {
						if rendered, ferr := renderVars(ctx, query, in.vars); ferr == nil {
							query = rendered
						}
					}
					if in.index == nil {
						err := fmt.Errorf("无可用索引（请先 indexer 节点）")
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					rt := &einoRetriever{ix: in.index, topK: step.TopK}
					docs, err := rt.Retrieve(ctx, query)
					if err != nil {
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					in.docs = docs
					trace.Render = fmt.Sprintf("检索 %q → %d 篇", query, len(docs))
					in.steps = append(in.steps, trace)
					r.trace(trace)
					return in, nil
				},
			), compose.WithNodeKey(fmt.Sprintf("retriever_%d", idx)))
		case StepPassthrough:
			// 官方 Passthrough：原样透传（调试/占位节点）。
			chain.AppendLambda(compose.InvokableLambda(
				func(ctx context.Context, in *state) (*state, error) {
					trace := StepTrace{Index: idx, Type: StepPassthrough, Name: step.Name, Render: "(透传)"}
					in.steps = append(in.steps, trace)
					r.trace(trace)
					return in, nil
				},
			), compose.WithNodeKey(fmt.Sprintf("pass_%d", idx)))
		case StepKnowledge:
			// 知识库检索节点（Dify knowledge-retrieval 对齐）：链路径同样
			// 经 Runner.KB 回调检索持久向量库；查询支持 {var} 渲染。
			chain.AppendLambda(compose.InvokableLambda(
				func(ctx context.Context, in *state) (*state, error) {
					trace := StepTrace{Index: idx, Type: StepKnowledge, Name: step.Name}
					query := step.Template
					if query != "" {
						if rendered, ferr := renderVars(ctx, query, in.vars); ferr == nil {
							query = rendered
						}
					}
					if strings.TrimSpace(query) == "" {
						err := fmt.Errorf("知识库节点未配置查询语句")
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					if r.KB == nil {
						err := fmt.Errorf("步骤 %d 为 knowledge 节点，但运行器未配置知识库回调", idx+1)
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, err
					}
					if dryRun {
						trace.Render = fmt.Sprintf("(dry-run 检索 %q，未调用)", query)
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, nil
					}
					topK := step.TopK
					if topK <= 0 {
						topK = 8
					}
					out, hits, err := r.KB(ctx, query, topK, step.MinScore)
					if err != nil {
						trace.Err = err.Error()
						in.steps = append(in.steps, trace)
						r.trace(trace)
						return in, fmt.Errorf("步骤 %d 知识库检索失败: %w", idx+1, err)
					}
					in.docs = []*schema.Document{{ID: "kb", Content: out, MetaData: map[string]any{"hits": hits}}}
					in.output = out
					trace.Render = out
					trace.Docs = hits
					in.steps = append(in.steps, trace)
					r.trace(trace)
					return in, nil
				},
			), compose.WithNodeKey(fmt.Sprintf("kb_%d", idx)))
		case StepTransform:
			// 代码转换节点（Dify template-transform 对齐）：不接入 Eino Chain，
			// 画布保存时直接路由到 RunGraph 图执行路径（见 flows.go RunFlow）。
			return nil, fmt.Errorf("transform 节点仅支持画布图执行（请保存画布后运行）")
		default:
			return nil, fmt.Errorf("步骤 %d 类型未知: %s", idx+1, step.Type)
		}
	}

	// Compile + Invoke（Eino 编排执行）。
	runnable, err := chain.Compile(ctx)
	if err != nil {
		return nil, fmt.Errorf("流程编译失败: %w", err)
	}
	final, err := runnable.Invoke(ctx, st)
	if err != nil {
		res := &RunResult{FlowID: f.ID, Steps: finalSteps(final, f), DryRun: dryRun}
		return res, err
	}

	// 节点输出写回变量池（链路径无 NodeID，按执行序占位 ID 供引用）。
	if final.output != "" {
		st.putPool("output", map[string]any{"output": final.output})
	}
	res := &RunResult{
		FlowID:   f.ID,
		Output:   final.output,
		Steps:    final.steps,
		Duration: time.Since(start).Milliseconds(),
		DryRun:   dryRun,
	}
	return res, nil
}

// ── 分支流程图执行（2026-10 升级）────────────────────────────
// 有 branch 节点的画布走 RunGraph：图解释执行（条件路由 + 逐步回调）；
// 无分支仍走 Run 的 Eino Chain 路径。两条路径共享 execStepNode 执行器。

// finalSteps 兜底：Compile 出错时尽力保留已有轨迹。
func finalSteps(st *state, f *Flow) []StepTrace {
	if st != nil && len(st.steps) > 0 {
		return st.steps
	}
	return nil
}

// evalCond 求值分支条件表达式（可含 {var}，先渲染变量再解析布尔）。
// 支持语法（空串 = else 兜底分支恒真）：
//   - “{x} > 10” “{a} >= 3” “{b} < 2” “{c} == foo” “{d} != bar”
//     （> >= < <= == !=，左右操作数先经 {var} 渲染，字符串比较去引号）
//   - “{name} contains vip”（包含子串）
//   - “{flag}”（非空且非 "false"/"0" 即真）
func evalBranchExpr(ctx context.Context, expr string, vars map[string]any) (bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return true, nil // else 兜底分支
	}
	// 先渲染变量（渲染失败按原串继续，条件求值容错）
	if strings.Contains(expr, "{") {
		if rendered, err := renderVars(ctx, expr, vars); err == nil {
			expr = rendered
		}
	}
	expr = strings.TrimSpace(expr)
	for _, op := range []string{">=", "<=", "!=", "==", ">", "<", "contains"} {
		if idx := strings.Index(expr, op); idx > 0 {
			left := strings.TrimSpace(expr[:idx])
			right := strings.TrimSpace(expr[idx+len(op):])
			if op == "contains" {
				return strings.Contains(left, strings.Trim(right, "\"'")), nil
			}
			lf, lerr := strconv.ParseFloat(strings.Trim(left, "\""), 64)
			rf, rerr := strconv.ParseFloat(strings.Trim(right, "\""), 64)
			if lerr == nil && rerr == nil {
				switch op {
				case ">":
					return lf > rf, nil
				case ">=":
					return lf >= rf, nil
				case "<":
					return lf < rf, nil
				case "<=":
					return lf <= rf, nil
				case "==":
					return lf == rf, nil
				case "!=":
					return lf != rf, nil
				}
			}
			switch op {
			case "==":
				return strings.Trim(left, "\"") == strings.Trim(right, "\""), nil
			case "!=":
				return strings.Trim(left, "\"") != strings.Trim(right, "\""), nil
			}
		}
	}
	// 无操作符：非空且非 false/0 即真
	low := strings.ToLower(expr)
	return low != "" && low != "false" && low != "0" && low != "null", nil
}

// pickBranchPath 分支路由：按边声明顺序求值首个为真的条件；全不命中走 else（空表达式）。
// 返回命中的 target 节点 ID；无命中且无 else 返回错误。
func pickBranch(edges []CanvasEdge, sourceID string, vars map[string]any) (string, error) {
	var elseTarget string
	for _, e := range edges {
		if e.Source != sourceID {
			continue
		}
		expr := strings.TrimSpace(e.Expr)
		if expr == "" {
			if elseTarget == "" {
				elseTarget = e.Target
			}
			continue
		}
		hit, err := evalBranchExpr(context.Background(), expr, vars)
		if err != nil {
			return "", fmt.Errorf("分支条件 %q 求值失败: %w", e.Expr, err)
		}
		if hit {
			return e.Target, nil
		}
	}
	if elseTarget != "" {
		return elseTarget, nil
	}
	return "", fmt.Errorf("分支节点 %s 无命中条件且无 else 兑底分支", sourceID)
}

// ── 分支流程图解释执行器（2026-10 升级）────────────────────
// 设计说明：Eino compose.Graph 的 Branch API 要求静态注册目标键集合且不支持
// 条件表达式随画布动态变更，而流程画布拓扑由用户保存时任意定制（branch 数量
// 与目标均动态），故引擎对分支流程采用与 Chain 同语义的解释执行器
// （沿画布 next 表路由 + 逐步 OnStep 回调），单链流程仍走 Run 的 Eino Chain。
// 两条路径共享 execStepNode 执行器，节点语义完全一致，后续可平滑切换原生 Graph。

// execStepNode 单业务节点执行（Run 的 chain Lambda 与图解释执行共用）：
// 按 step.Type 分派执行并追加轨迹，语义与 Run 内 switch 各 case 一致。
func (r *Runner) execStepNode(ctx context.Context, st *state, step Step, idx int, dryRun bool) error {
	trace := StepTrace{Index: idx, Type: step.Type, Name: step.Name}
	// 渲染取值统一用 mergedVars（用户变量 + 节点输出变量池）：任意节点可引用
	// 上游输出 {nodeID.output}（Dify 语义，2026-10 融入）。
	mvars := st.mergedVars()
	switch step.Type {
	case StepTemplate:
		tplNode := prompt.FromMessages(schema.FString, &schema.Message{Role: stepRole(step.Role), Content: step.Template})
		rendered, err := tplNode.Format(ctx, mvars)
		if err != nil || len(rendered) == 0 {
			if err == nil {
				err = fmt.Errorf("模板渲染结果为空")
			}
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return fmt.Errorf("步骤 %d 模板渲染失败: %w", idx+1, err)
		}
		st.msgs = append(st.msgs, rendered...)
		trace.Render = rendered[0].Content
	case StepLLM:
		if dryRun {
			trace.Render = "(dry-run 跳过)"
			break
		}
		// 节点自带提示词（与 Run 链路径同语义）：{var} 渲染后以 user 消息追加。
		if p := strings.TrimSpace(step.Template); p != "" {
			if rendered, rerr := renderVars(ctx, p, mvars); rerr == nil && strings.TrimSpace(rendered) != "" {
				st.msgs = append(st.msgs, &schema.Message{Role: schema.User, Content: rendered})
			}
		}
		// 统一走 invokeLLM（融合两侧能力：MCP/技能注入 + 节点级模型选择）。
		text, closeRes, err := r.invokeLLM(ctx, invokeLLMIn{step: step, msgs: st.msgs})
		if closeRes != nil {
			defer closeRes()
		}
		if err != nil {
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return fmt.Errorf("步骤 %d 模型调用失败: %w", idx+1, err)
		}
		trace.Render = text
		st.output = text
		st.msgs = append(st.msgs, &schema.Message{Role: schema.Assistant, Content: text})
	case StepTool:
		if dryRun {
			trace.Render = "(dry-run 跳过)"
			break
		}
		if strings.TrimSpace(step.Name) == "" {
			err := fmt.Errorf("工具节点未指定工具名")
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return err
		}
		gt, ok := r.Tools.Get(step.Name)
		if !ok {
			err := fmt.Errorf("工具未注册: %s", step.Name)
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return err
		}
		args := step.Template
		if args != "" {
			if rendered, ferr := renderVars(ctx, args, mvars); ferr == nil {
				args = rendered
			}
		}
		res, err := gt.Execute(ctx, args)
		if err != nil {
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return fmt.Errorf("步骤 %d 工具 %s 执行失败: %w", idx+1, step.Name, err)
		}
		trace.Render = res
		st.msgs = append(st.msgs, &schema.Message{Role: schema.Tool, Content: res})
	case StepLoader:
		uri := step.Template
		if uri != "" {
			if rendered, ferr := renderVars(ctx, uri, mvars); ferr == nil {
				uri = rendered
			}
		}
		ld := &einoLoader{workspace: r.Workspace}
		docs, err := ld.Load(ctx, document.Source{URI: uri})
		if err != nil {
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return err
		}
		st.docs = append(st.docs, docs...)
		trace.Render = fmt.Sprintf("载入 %d 篇文档：%s", len(docs), uri)
	case StepTransformer:
		mode := step.Template
		if mode != "join" {
			mode = "split"
		}
		tr := &einoTransformer{mode: mode}
		n := len(st.docs)
		out, err := tr.Transform(ctx, st.docs)
		if err != nil {
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return err
		}
		st.docs = out
		trace.Render = fmt.Sprintf("%s：%d → %d 篇文档", mode, n, len(out))
	case StepEmbedding:
		if len(st.docs) == 0 {
			err := fmt.Errorf("无文档可向量化（请前置 loader 节点）")
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return err
		}
		em := &einoEmbedder{dim: embedDim}
		texts := make([]string, 0, len(st.docs))
		for _, d := range st.docs {
			texts = append(texts, d.Content)
		}
		vecs, err := em.EmbedStrings(ctx, texts)
		if err != nil {
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return err
		}
		for i, d := range st.docs {
			if d.MetaData == nil {
				d.MetaData = map[string]any{}
			}
			d.MetaData["__vec__"] = vecs[i]
		}
		trace.Render = fmt.Sprintf("向量化 %d 篇文档（dim=%d）", len(vecs), embedDim)
	case StepIndexer:
		if len(st.docs) == 0 {
			err := fmt.Errorf("无文档可索引（请先 loader/transformer 节点）")
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return err
		}
		ix := &memIndex{}
		nd := &einoIndexer{ix: ix}
		ids, err := nd.Store(ctx, st.docs)
		if err != nil {
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return err
		}
		st.index = ix
		st.docs = nil
		trace.Render = fmt.Sprintf("已索引 %d 篇文档（内存向量索引）", len(ids))
	case StepRetriever:
		query := step.Template
		if query != "" {
			if rendered, ferr := renderVars(ctx, query, mvars); ferr == nil {
				query = rendered
			}
		}
		if st.index == nil {
			err := fmt.Errorf("无可用索引（请先 indexer 节点）")
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return err
		}
		rt := &einoRetriever{ix: st.index, topK: step.TopK}
		docs, err := rt.Retrieve(ctx, query)
		if err != nil {
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return err
		}
		st.docs = docs
		trace.Render = fmt.Sprintf("检索 %q → %d 篇", query, len(docs))
	case StepPassthrough:
		trace.Render = "(透传)"
	case StepKnowledge:
		// 知识库检索（Dify knowledge-retrieval 对齐）：查询支持 {var} 渲染
		// （含上游节点输出 {nodeID.output}），TopK/阈值经 Runner.KB 回调
		// 检索持久向量库，结果写回变量池供下游引用。
		query := step.Template
		if query != "" {
			if rendered, ferr := renderVars(ctx, query, mvars); ferr == nil {
				query = rendered
			}
		}
		if strings.TrimSpace(query) == "" {
			err := fmt.Errorf("知识库节点未配置查询语句")
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return err
		}
		if r.KB == nil {
			err := fmt.Errorf("步骤 %d 为 knowledge 节点，但运行器未配置知识库回调", idx+1)
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return err
		}
		if dryRun {
			trace.Render = fmt.Sprintf("(dry-run 检索 %q，未调用)", query)
			break
		}
		topK := step.TopK
		if topK <= 0 {
			topK = 8
		}
		out, hits, err := r.KB(ctx, query, topK, step.MinScore)
		if err != nil {
			trace.Err = err.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return fmt.Errorf("步骤 %d 知识库检索失败: %w", idx+1, err)
		}
		st.docs = []*schema.Document{{ID: "kb", Content: out, MetaData: map[string]any{"hits": hits}}}
		st.output = out
		trace.Render = out
		trace.Docs = hits
	case StepHTTP:
		// HTTP 请求（Dify http-request 对齐）：定义 JSON 渲染 → 发请求 →
		// 状态码/响应体写回变量池（dry-run 仅渲染不发送）。
		tr2, body, code, herr := r.execHTTP(ctx, st, step, dryRun)
		if herr != nil {
			trace.Err = herr.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return fmt.Errorf("步骤 %d HTTP 请求失败: %w", idx+1, herr)
		}
		trace.Render = tr2
		trace.HTTPStatus = code
		if !dryRun {
			st.pendingHTTPBody = body
			st.output = body
		}
	case StepAssigner:
		// 变量赋值（Dify variable-assigner 对齐）：值表达式渲染 → 写入
		// 用户变量表（Step.Name 为目标变量名）。
		val, aerr := r.execAssigner(ctx, st, step)
		if aerr != nil {
			trace.Err = aerr.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return aerr
		}
		trace.Render = val
	case StepExtractor:
		// 参数提取器（Dify parameter-extractor 对齐）：LLM 提取 → JSON 严格
		// 解析 + 必填校验 → 结果/逐参数写回变量池（调用方以 NodeID 键再包一层）。
		resJSON, parsed, eerr := r.execExtractor(ctx, st, step, dryRun)
		if eerr != nil {
			trace.Err = eerr.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return fmt.Errorf("步骤 %d 参数提取失败: %w", idx+1, eerr)
		}
		trace.Render = resJSON
		st.output = resJSON
		if parsed != nil {
			st.pendingExtract = parsed
		}
	case StepIteration:
		// 迭代（Dify iteration / Coze loop 对齐）：列表逐项跑子步骤链，
		// 输出聚合（轨迹透出子项步骤）。
		joined, items, ierr := r.execIteration(ctx, st, step, idx, dryRun)
		if ierr != nil {
			trace.Err = ierr.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return fmt.Errorf("步骤 %d 迭代失败: %w", idx+1, ierr)
		}
		trace.Render = joined
		st.output = joined
		st.pendingIterItems = items
		// 聚合帧 Index 排在全部子帧之后（子帧已并入 st.steps）。
		trace.Index += st.pendingAppended
	case StepTransform:
		// 代码转换（迭代子步骤与图执行共用）：纯计算，dry-run 也真实执行
		//（与 RunGraph 主链 transform 节点同语义）。
		out, terr := r.execTransform(ctx, st, step)
		if terr != nil {
			trace.Err = terr.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return terr
		}
		trace.Render = out
		st.output = out
	case StepSubflow:
		// 子流程（Coze subworkflow 对齐）：回调执行目标流程，输出与
		// 子轨迹并入；dry-run 透传（子流程自行按需跳过外呼）。
		out, appended, serr := r.execSubflow(ctx, st, step, dryRun)
		if serr != nil {
			trace.Err = serr.Error()
			st.steps = append(st.steps, trace)
			r.trace(trace)
			return fmt.Errorf("步骤 %d 子流程执行失败: %w", idx+1, serr)
		}
		trace.Render = out
		st.output = out
		st.pendingAppended += appended
	default:
		err := fmt.Errorf("步骤 %d 类型未知: %s", idx+1, step.Type)
		trace.Err = err.Error()
		st.steps = append(st.steps, trace)
		r.trace(trace)
		return err
	}
	st.steps = append(st.steps, trace)
	r.trace(trace)
	return nil
}

// RunGraph 图执行路径（有 branch 节点的画布）：沿拓扑自解释执行，
// branch 节点求值路由、业务节点复用 execStepNode，轨迹携 NodeID 供前端按节点高亮。
func (r *Runner) RunGraph(ctx context.Context, f *Flow, c *Canvas, vars map[string]any, dryRun bool) (*RunResult, error) {
	if err := ValidateCanvas(c); err != nil {
		return nil, fmt.Errorf("画布校验失败: %w", err)
	}
	if vars == nil {
		vars = map[string]any{}
	}
	start0 := time.Now()
	st := &state{vars: vars, pools: map[string]map[string]any{}}
	byID := make(map[string]*CanvasNode, len(c.Nodes))
	for i := range c.Nodes {
		byID[c.Nodes[i].ID] = &c.Nodes[i]
	}
	next := map[string][]string{}
	for _, e := range c.Edges {
		next[e.Source] = append(next[e.Source], e.Target)
	}
	var startID string
	for _, n := range c.Nodes {
		if n.Type == StepStart {
			startID = n.ID
		}
	}
	// 执行序计数器（跨分支全局递增，保证轨迹 Index 单调）
	idx := 0
	cur := startID
	visited := 0
	for cur != "" {
		n := byID[cur]
		visited++
		if visited > len(c.Nodes)+1 {
			return nil, fmt.Errorf("执行超出节点数（可能存在环）")
		}
		switch n.Type {
		case StepStart:
			cur = nextOf1(next, cur)
		case StepEnd:
			// 结束输出模板（Dify answer/end 输出对齐）：Step.Template 非空时
			// 渲染写回 Output（覆盖默认“最后 llm 输出”），支持 {var} 与
			// {nodeID.output} 引用；空 = 保持原语义。
			if tpl := strings.TrimSpace(n.Step.Template); tpl != "" {
				if rendered, rerr := renderVars(ctx, tpl, st.mergedVars()); rerr == nil {
					st.output = rendered
					st.steps = append(st.steps, StepTrace{Index: idx, Type: StepEnd, Name: n.Step.Name, NodeID: n.ID, Render: rendered})
					r.trace(st.steps[len(st.steps)-1])
					idx++
				}
			}
			cur = "" // 结束
		case StepBranch:
			nx, err := pickBranch(c.Edges, cur, st.mergedVars())
			if err != nil {
				return &RunResult{FlowID: f.ID, Steps: st.steps, DryRun: dryRun, Duration: time.Since(start0).Milliseconds()}, err
			}
			cur = nx
		case StepTransform:
			// 入边条件门禁（无条件入边恒真；分支下游双入边场景可按条件跳过）。
			if !r.condSatisfied(c.Edges, cur, st) {
				cur = nextOf1(next, cur)
				continue
			}
			trace := StepTrace{Index: idx, Type: StepTransform, Name: n.Step.Name, NodeID: n.ID}
			// transform 为纯计算（无副作用），dry-run 也真实执行：
			// 试运行才能验证变量链路（下游 {nodeID.output} 引用与分支条件）。
			out, terr := r.execTransform(ctx, st, n.Step)
			if terr != nil {
				trace.Err = terr.Error()
				st.steps = append(st.steps, trace)
				r.trace(trace)
				return &RunResult{FlowID: f.ID, Steps: st.steps, DryRun: dryRun, Duration: time.Since(start0).Milliseconds()}, fmt.Errorf("节点 %s 转换失败: %w", n.Step.Name, terr)
			}
			trace.Render = out
			// 输出写回变量池：{<nodeID>.output} 供下游模板与分支条件引用。
			st.putPool(n.ID, map[string]any{"output": out})
			st.output = out
			st.steps = append(st.steps, trace)
			r.trace(trace)
			idx++
			cur = nextOf1(next, cur)
		default:
			// 业务节点：步骤 type 归一化后执行（轨迹 Index 用执行序）
			step := n.Step
			step.Type = n.Type
			// 入边条件门禁（Dify branch 同构：条件入边未命中 → 跳过节点及下游）。
			if !r.condSatisfied(c.Edges, cur, st) {
				cur = nextOf1(next, cur)
				continue
			}
			if err := r.execStepNode(ctx, st, step, idx, dryRun); err != nil {
				// 轨迹携带 NodeID（execStepNode 内部已 r.trace；这里为结果补 NodeID）
				if k := len(st.steps); k > 0 {
					st.steps[k-1].NodeID = n.ID
				}
				return &RunResult{FlowID: f.ID, Steps: st.steps, DryRun: dryRun, Duration: time.Since(start0).Milliseconds()}, fmt.Errorf("节点 %s 执行失败: %w", n.Step.Name, err)
			}
			if k := len(st.steps); k > 0 {
				st.steps[k-1].NodeID = n.ID
			}
			// 节点输出写回变量池（Dify 语义）：output = llm 输出 / 工具结果 / 转换结果，
			// 下游模板与分支条件以 {<nodeID>.output} 点分路径引用。
			if k := len(st.steps); k > 0 && st.steps[k-1].Err == "" {
				if out := strings.TrimSpace(st.steps[k-1].Render); out != "" {
					st.putPool(n.ID, map[string]any{"output": out})
				}
			}
			// extractor 逐参数写回：{<nodeID>.<paramName>}（Dify parameter-
			// extractor 输出变量语义，下游直接引用单个参数）。
			if st.pendingExtract != nil {
				for pk, pv := range st.pendingExtract {
					if pool := st.pools[n.ID]; pool != nil {
						pool[pk] = pv
					}
				}
				st.pendingExtract = nil
			}
			// iteration 子项数组写回：{<nodeID>.items} = JSON 数组（下游模板/
			// transform 再加工）。
			if st.pendingIterItems != nil {
				if pool := st.pools[n.ID]; pool != nil {
					b, _ := json.Marshal(st.pendingIterItems)
					pool["items"] = string(b)
				}
				st.pendingIterItems = nil
			}
			// http 响应体覆盖：{<nodeID>.output} 引用真实响应体（而非状态摘要）。
			if k := len(st.steps); k > 0 && st.steps[k-1].Type == StepHTTP && st.pendingHTTPBody != "" {
				if pool := st.pools[n.ID]; pool != nil {
					pool["output"] = st.pendingHTTPBody
				}
				st.pendingHTTPBody = ""
			}
			// http 状态码写回：{<nodeID>.status}（数值，分支条件可直接比较）。
			if k := len(st.steps); k > 0 && st.steps[k-1].Type == StepHTTP && st.steps[k-1].HTTPStatus > 0 {
				if pool := st.pools[n.ID]; pool != nil {
					pool["status"] = st.steps[k-1].HTTPStatus
				}
			}
			idx++
			// iteration 子轨迹帧数推进（子帧 Index 已占位，聚合帧随后单调）。
			idx += st.pendingAppended
			st.pendingAppended = 0
			cur = nextOf1(next, cur)
		}
	}
	return &RunResult{
		FlowID:   f.ID,
		Output:   st.output,
		Steps:    st.steps,
		Duration: time.Since(start0).Milliseconds(),
		DryRun:   dryRun,
	}, nil
}
func nextOf1(next map[string][]string, cur string) string {
	if ns := next[cur]; len(ns) > 0 {
		return ns[0]
	}
	return ""
}

// TraceVars 列出流程全部模板变量（供 UI 提示需要填写的变量）。
func TraceVars(f *Flow) []string {
	seen := map[string]bool{}
	var out []string
	collect := func(tpl string) {
		for _, v := range templateVars(tpl) {
			// 点分路径（{nodeID.output}）为节点输出引用，非待填写变量，过滤。
			if strings.Contains(v, ".") {
				continue
			}
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	for _, s := range f.Steps {
		if s.Type == StepTransform {
			// transform 脚本用双大括号引用变量（{{x}} / {{fn x}} / {{x + 1}}），
			// 单独提取（同样过滤点分路径）。
			for _, m := range transformVarRe.FindAllStringSubmatch(s.Template, -1) {
				collect("{" + m[1] + "}")
			}
			continue
		}
		collect(s.Template)
	}
	// 画布模式：扫描全部节点的模板与分支条件表达式（用户变量不含点分
	// 路径，{nodeID.output} 引用天然被过滤，不会误列为需填写变量）。
	// start 节点输入变量定义（Dify 开始节点对齐）：Step.Name 存逗号分隔
	// 变量名，运行时即无需从模板反推。
	if f.Canvas != nil {
		for _, n := range f.Canvas.Nodes {
			switch n.Type {
			case StepStart:
				for _, v := range startVars(n.Step.Name) {
					collect("{" + v + "}")
				}
				continue
			case StepTransform:
				for _, m := range transformVarRe.FindAllStringSubmatch(n.Step.Template, -1) {
					collect("{" + m[1] + "}")
				}
				continue
			}
			collect(n.Step.Template)
		}
		for _, e := range f.Canvas.Edges {
			collect(e.Expr)
		}
	}
	return out
}

// startVars 解析 start 节点输入变量定义（Step.Name 逗号/空白分隔变量名），
// 去重保序；保留字（"开始"/"start"，旧画布默认显示名）与空串过滤。
// 空 = 未定义（回退模板反扫）。
func startVars(def string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range strings.FieldsFunc(def, func(r rune) bool { return r == ',' || r == '，' || r == ' ' || r == '\t' || r == '\n' }) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] || v == "开始" || v == "start" {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// transformVarRe transform 脚本变量提取：{{x}} / {{fn x}} / {{x + 1}} 中的变量名。
var transformVarRe = regexp.MustCompile(
	`\{\{\s*(?:upper|lower|trim|len)?\s*([A-Za-z_][A-Za-z0-9_.]*)`)
