// 节点基座（对齐 common/node.py NodeType/BaseNodeData + nodes/base.py BaseNode
// + nodes/node_manage.py NodeFactory）。
package bisheng

import (
	"fmt"
	"strings"
)

// NodeType 节点类型枚举（逐字对齐 common/node.py NodeType）。
const (
	NodeStart     = "start"
	NodeEnd       = "end"
	NodeInput     = "input"
	NodeAgent     = "agent"
	NodeCode      = "code"
	NodeCondition = "condition"
	NodeLLM       = "llm"
	NodeOutput    = "output"
	NodeQARetriever = "qa_retriever"
	NodeRAG       = "rag"
	NodeReport    = "report"
	NodeTool      = "tool"
	NodeKnowledgeRetriever = "knowledge_retriever"
	NodeNote      = "note" // 注释节点：仅供画布展示，不执行（蓝本跳过）
)

// NodeParams 参数定义（蓝本 NodeParams：画布上可配的节点参数）。
type NodeParams struct {
	Key         string `json:"key"`
	Label       string `json:"label,omitempty"`
	Value       any    `json:"value,omitempty"`
	Type        string `json:"type,omitempty"` // 变量类型（text/int/file/...）
	Help        string `json:"help,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Options     any    `json:"options,omitempty"`
}

// NodeGroupParams 参数分组（蓝本 NodeGroupParams）。
type NodeGroupParams struct {
	Name        string       `json:"name,omitempty"`
	Params      []NodeParams `json:"params"`
	Description string       `json:"description,omitempty"`
}

// BaseNodeData 节点配置（蓝本 BaseNodeData；前端画布 node.data 直传）。
type BaseNodeData struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	GroupParams []NodeGroupParams `json:"group_params,omitempty"`
	ToolKey     string            `json:"tool_key,omitempty"` // tool 节点专用
}

// ParamMap 展开 group_params → 扁平 key→value（蓝本 init_data 的 node_params）。
func (d *BaseNodeData) ParamMap() map[string]any {
	out := map[string]any{}
	for _, g := range d.GroupParams {
		for _, p := range g.Params {
			out[p.Key] = p.Value
		}
	}
	return out
}

// Node 节点实例接口（蓝本 BaseNode 抽象类；run 为引擎统一入口，
// _run 由具体节点实现 —— Go 用小写 exec 表达下划线约定）。
type Node interface {
	ID() string
	Type() string
	Name() string
	// Run 执行入口（蓝本 run：max_steps 熔断 + on_node_start/end 事件 +
	// 结果写回变量池）。返回写入变量池的 key→value（nil = 无输出）。
	Run(uniqueID string) map[string]any
	// GetInputSchema 需要用户输入时返回表单描述（蓝本 get_input_schema；
	// nil = 不需要交互）。
	GetInputSchema() any
	// IsConditionNode 是否互斥路由节点（condition / choose 型 output）。
	IsConditionNode() bool
	// RouteNode 互斥节点路由：返回下一节点 ID（蓝本 route_node）。
	RouteNode() string
	// HandleInput 用户中断后回填输入（蓝本 handle_input）。
	HandleInput(input map[string]any)
	// Stop 用户请求停止（蓝本 stop：置 stop_flag，下次 Run 抛 IgnoreException）。
	Stop()
	// parseLog 节点运行日志（蓝本 parse_log；外层=轮次，内层=逐条日志）。
	parseLog(result map[string]any) any
}

// BaseNode 公共基座（字段与蓝本 BaseNode.__init__ 一一对应）。
type BaseNode struct {
	nodeID   string
	nodeType string
	nodeName string
	data     BaseNodeData // 原始节点配置（蓝本 node_data）

	TargetEdges []EdgeBase // 本节点出边（get_next_node_id 路由用）
	State       *State     // 全局状态
	MaxSteps    int        // 单节点最大执行次数（熔断）
	Callback    Callback

	nodeParams map[string]any   // 展开后的节点参数（蓝本 node_params）
	otherVars  map[string]any   // 本节点消费的上游变量（蓝本 other_node_variable）
	stopFlag   bool             // 蓝本 stop_flag
	curStep    int              // 蓝本 current_step
}

// InitBaseNode 节点构造公共段（蓝本 BaseNode.__init__）。
func InitBaseNode(n *BaseNode, data BaseNodeData, st *State, targetEdges []EdgeBase, maxSteps int, cb Callback) {
	n.nodeID, n.nodeType, n.nodeName = data.ID, data.Type, data.Name
	n.data = data
	n.TargetEdges = targetEdges
	n.State = st
	n.MaxSteps = maxSteps
	n.Callback = cb
	n.nodeParams = data.ParamMap()
	n.otherVars = map[string]any{}
}

func (n *BaseNode) ID() string         { return n.nodeID }
func (n *BaseNode) Type() string       { return n.nodeType }
func (n *BaseNode) Name() string       { return n.nodeName }
func (n *BaseNode) Stop()              { n.stopFlag = true }
func (n *BaseNode) IsConditionNode() bool { return false }
func (n *BaseNode) RouteNode() string  { return "" }
func (n *BaseNode) GetInputSchema() any { return nil }

// Params 节点参数（各节点 init 时读取）。
func (n *BaseNode) Params() map[string]any { return n.nodeParams }

// HandleInput 默认回填：并入 node_params（蓝本 BaseNode.handle_input）。
func (n *BaseNode) HandleInput(input map[string]any) {
	for k, v := range input {
		n.nodeParams[k] = v
	}
}

// GetOtherNodeVariable 从全局变量池取上游变量（蓝本 get_other_node_variable，
// 同时记录到 other_node_variable 供 on_node_end 的 input_data 上报）。
func (n *BaseNode) GetOtherNodeVariable(key string) any {
	v := n.State.GetVariableByStr(key, 0)
	n.otherVars[key] = v
	return v
}

// GetNextNodeID 按 sourceHandle 找后继（蓝本 get_next_node_id）。
func (n *BaseNode) GetNextNodeID(sourceHandle string) []string {
	var out []string
	for _, e := range n.TargetEdges {
		if e.SourceHandle == sourceHandle {
			out = append(out, e.Target)
		}
	}
	return out
}

// ParseMsgWithVariables 模板变量替换（蓝本 parse_msg_with_variables：
// PromptTemplateParser extract → 逐变量取上游值 → format）。
// 返回替换后文本与引用到的变量 key 列表。
func (n *BaseNode) ParseMsgWithVariables(msg string) (string, []string) {
	vars := ExtractTemplateVars(msg)
	if len(vars) == 0 {
		return msg, nil
	}
	varMap := map[string]any{}
	for _, one := range vars {
		varMap[one] = n.GetOtherNodeVariable(one)
	}
	return FormatTemplate(msg, varMap), vars
}

// RunCount 当前执行次数（蓝本 current_step，测试断言用）。
func (n *BaseNode) RunCount() int { return n.curStep }

// baseRun 蓝本 run 的公共段：熔断 → on_node_start → exec → 写变量池 →
// on_node_end。exec 为具体节点的 _run；返回 (输出, 日志)。
func (n *BaseNode) baseRun(uniqueID string, exec func() (map[string]any, any)) map[string]any {
	if n.stopFlag {
		panic(stopErr{})
	}
	if n.MaxSteps > 0 && n.curStep >= n.MaxSteps {
		panic(ignoreErr("%s -- has run more than the maximum number of times.", n.nodeName))
	}
	n.Callback.OnNodeStart(NodeStartData{UniqueID: uniqueID, NodeID: n.nodeID, Name: n.nodeName})

	var reason string
	var logData any
	defer func() {
		// output 型交互节点的 end 事件由 fake 节点在用户输入后补发
		//（蓝本：reason 存在或非 output 节点才发 on_node_end）。
		if reason != "" || n.nodeType != NodeOutput {
			n.Callback.OnNodeEnd(NodeEndData{
				UniqueID: uniqueID, NodeID: n.nodeID, Name: n.nodeName,
				Reason: reason, LogData: logData, InputData: n.otherVars,
			})
		}
	}()

	defer func() {
		if r := recover(); r != nil {
			if ie, ok := r.(*ignoreError); ok {
				reason = ie.msg
				panic(ie) // 继续上抛给引擎（蓝本 raise e）
			}
			if se, ok := r.(stopError); ok {
				reason = "stop by user"
				panic(se)
			}
			panic(r)
		}
	}()

	result, log := exec()
	logData = log
	for key, value := range result {
		n.State.SetVariable(n.nodeID, key, value)
	}
	n.curStep++
	return result
}

// stopError 用户停止（stop() 置位后触发；蓝本 IgnoreException("stop by user")）。
type stopError struct{}

func (stopError) Error() string { return "stop by user" }

type stopErr = stopError

// ---------- 工厂（蓝本 node_manage.py NodeFactory.instance_node）----------

// Deps 节点工厂依赖注入：LLM / 工具执行器由宿主桥接（bisheng 包零依赖
// internal/llm、internal/tools，与蓝本 LLMService/ToolExecutor 同构）。
type Deps struct {
	LLM  LLMInvoker  // llm/agent 节点模型调用（nil 则 llm 节点报错）
	Tool ToolExecutor // tool 节点执行器（nil 则 tool 节点报错）
}

// InstanceNode 按类型实例化节点（蓝本 NodeFactory.instance_node）。
// 未移植类型（agent/code/qa_retriever/rag/report/knowledge_retriever）返回
// 明确错误 —— 商业/重依赖节点，kaelvoris 侧由 internal/flow 节点覆盖。
func InstanceNode(data BaseNodeData, st *State, edges *EdgeManage, maxSteps int, cb Callback, deps Deps) (Node, error) {
	if strings.TrimSpace(data.ID) == "" {
		return nil, fmt.Errorf("node must have attribute id")
	}
	targetEdges := edges.GetTargetEdges(data.ID)
	switch data.Type {
	case NodeStart:
		n := &StartNode{}
		InitBaseNode(&n.BaseNode, data, st, targetEdges, maxSteps, cb)
		return n, nil
	case NodeEnd:
		n := &EndNode{}
		InitBaseNode(&n.BaseNode, data, st, targetEdges, maxSteps, cb)
		return n, nil
	case NodeLLM:
		n := &LLMNode{}
		InitBaseNode(&n.BaseNode, data, st, targetEdges, maxSteps, cb)
		if err := n.initLLM(deps.LLM); err != nil {
			return nil, err
		}
		return n, nil
	case NodeTool:
		n := &ToolNode{}
		InitBaseNode(&n.BaseNode, data, st, targetEdges, maxSteps, cb)
		if err := n.initTool(deps.Tool); err != nil {
			return nil, err
		}
		return n, nil
	case NodeCondition:
		n := &ConditionNode{}
		InitBaseNode(&n.BaseNode, data, st, targetEdges, maxSteps, cb)
		if err := n.initConditions(); err != nil {
			return nil, err
		}
		return n, nil
	case NodeInput:
		n := &InputNode{}
		InitBaseNode(&n.BaseNode, data, st, targetEdges, maxSteps, cb)
		return n, nil
	case NodeOutput:
		n := &OutputNode{}
		InitBaseNode(&n.BaseNode, data, st, targetEdges, maxSteps, cb)
		n.initOutput()
		return n, nil
	default:
		return nil, fmt.Errorf("node type %s not supported in kaelvoris port (支持: start/end/llm/tool/condition/input/output)", data.Type)
	}
}
