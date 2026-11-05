// Package bisheng —— BISHENG Workflow 编排内核的 Go 移植（蓝本：
// dataelement/bisheng src/backend/bisheng/workflow/，Apache-2.0）。
//
// 移植范围（对齐蓝本目录，语义逐字对应）：
//
//	workflow.go      ← graph/workflow.py + common/workflow.py   Workflow 门面 / 状态机
//	engine.go        ← graph/graph_engine.py                    图执行器（langgraph StateGraph 的零依赖等价实现）
//	state.go         ← graph/graph_state.py                     全局状态（变量池 + 会话历史）
//	edges.go         ← edges/edges.py                           EdgeManage（source/target 双向索引 + 分支遍历）
//	callback.go      ← callback/base_callback.py + event.py     回调接口 + 事件载荷
//	nodes.go         ← common/node.py + nodes/base.py + node_manage.py  节点基座 + 工厂
//	nodes_basic.go   ← nodes/{start,end,output,output_fake}.py  结构节点
//	nodes_input.go   ← nodes/input/input.py（核心交互语义）      用户输入节点（表单/对话）
//	nodes_llm.go     ← nodes/llm/llm.py                          LLM 节点
//	nodes_tool.go    ← nodes/tool/tool.py                        工具节点
//	nodes_condition.go ← nodes/condition/*.py + common/condition.py  条件路由（13 比较符 + and/or）
//	prompt.go        ← nodes/prompt_template.py                  PromptTemplateParser（{var} 提取/替换）
//
// 与蓝本的刻意差异（保持 kaelvoris 零依赖约定，AGENTS.md）：
//   - langgraph StateGraph/interrupt_before → 自研步进执行器（engine.go）：
//     「激活边 + 静态入边就绪」模型，语义等价（条件互斥分支汇合不阻塞、
//     fan-in 汇聚等待全部前驱、循环靠 MaxSteps 熔断）；
//   - bisheng 企业设施（Milvus/minio/用户租户体系/Celery/批处理 batch_variable/
//     多模态 image_prompt/code 节点）不移植 —— code 节点在 bisheng 自身默认禁用
//     （无沙箱安全风险，见其 CODE_NODE_DISABLED_MESSAGE），kaelvoris 已有
//     internal/flow transform 节点覆盖确定性加工场景；
//   - LLM / 工具执行经 LLMInvoker / ToolExecutor 接口注入（桥接 internal/llm、
//     internal/tools），本包仅依赖标准库。
package bisheng

// WorkflowStatus 工作流状态机（逐字对齐 common/workflow.py WorkflowStatus）。
const (
	StatusWaiting   = "WAITING"     // 等待异步调度（蓝本保留值，本实现不产生）
	StatusRunning   = "RUNNING"     // 运行中
	StatusSuccess   = "SUCCESS"     // 执行完成
	StatusFailed    = "FAILED"      // 执行失败
	StatusInput     = "INPUT"       // 等待用户输入（中断）
	StatusInputOver = "INPUT_OVER"  // 已输入（蓝本保留值，Continue 后回到 RUNNING）
)

// Msg 对话消息（graph_state 会话历史单元，对应 langchain HumanMessage/AIMessage）。
type Msg struct {
	Role    string `json:"role"`    // human | ai
	Content string `json:"content"`
}

// ignoreError 忽略类错误（对齐 utils/exceptions.py IgnoreException）：
// 节点主动判定「流程到此为止」（如 max_steps 超限、用户停止），引擎捕获取
// FAILED 并把消息作为 reason 上报（蓝本同样 FAILED，仅日志级别不同）。
type ignoreError struct{ msg string }

func (e *ignoreError) Error() string { return e.msg }

func ignoreErr(format string, a ...any) error {
	return &ignoreError{msg: fmtSprintf(format, a...)}
}
