// LLM / Tool / Condition 节点移植（对齐 nodes/llm/llm.py、nodes/tool/tool.py、
// nodes/condition/{condition,conidition_case}.py、common/condition.py）。
//
// 桥接约定（蓝本 LLMService / GptsToolsDao+ToolExecutor 的 kaelvoris 等价物）：
//   - LLMInvoker   ← internal/llm.Client（宿主注入；流式帧经 Callback.OnStreamMsg）
//   - ToolExecutor ← internal/tools.Registry（JSON 参数串协议与蓝本 tool.invoke 同构）
package bisheng

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ---------- 桥接接口 ----------

// LLMMessage LLM 消息（role: system|user|assistant）。
type LLMMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// LLMInvoker 模型调用器（非流式；流式场景宿主在实现内自行桥接
// Callback.OnStreamMsg，与蓝本 LLMNodeCallbackHandler 同构）。
type LLMInvoker interface {
	Invoke(messages []LLMMessage) (string, error)
}

// ToolExecutor 工具执行器：name → JSON 参数串 → 输出（蓝本
// ToolExecutor.init_by_tool_id_sync(...).invoke(input) 等价）。
type ToolExecutor interface {
	Execute(name, argsJSON string) (string, error)
	// Has 工具是否存在（蓝本 GptsToolsDao.get_tool_by_tool_key 校验）。
	Has(name string) bool
}

// ---------- LLMNode（蓝本 LLMNode，剥离 batch_variable/image_prompt/usage 统计）----------

// LLMNode LLM 节点：system_prompt + user_prompt 模板渲染 → 模型调用 →
// output 写回变量池。output_user 时同步写会话历史（蓝本 save_context）。
type LLMNode struct {
	BaseNode
	inv        LLMInvoker
	sysPrompt  string
	userPrompt string
	model      string   // 模型名（宿主桥接选择；蓝本 model_id）
	outputUser bool     // 结果是否推送会话流
}

// initLLM 构造期校验（蓝本 __init__ 的 PromptTemplateParser extract +
// LLMService.get_bisheng_llm_sync；模型解析延迟到 Invoke 由宿主决定）。
func (n *LLMNode) initLLM(inv LLMInvoker) error {
	if inv == nil {
		return fmt.Errorf("llm 节点 %s 缺少 LLMInvoker（宿主未配置模型）", n.nodeName)
	}
	n.inv = inv
	p := n.Params()
	n.sysPrompt = str(p["system_prompt"])
	n.userPrompt = str(p["user_prompt"])
	n.model = str(p["model"])
	n.outputUser = toBool(p["output_user"])
	if n.userPrompt == "" && n.sysPrompt == "" {
		return fmt.Errorf("llm 节点 %s 缺少 prompt 配置", n.nodeName)
	}
	return nil
}

// toBool any → bool（true/「true」/非零数字）。
func toBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "True"
	case float64:
		return t != 0
	}
	return false
}

// Run 蓝本 _run（single 模式；batch_variable 商业特性不移植）。
func (n *LLMNode) Run(uniqueID string) map[string]any {
	return n.baseRun(uniqueID, func() (map[string]any, any) {
		// 模板变量替换（蓝本 _run_once：逐变量 get_other_node_variable）
		sysVars := ExtractTemplateVars(n.sysPrompt)
		userVars := ExtractTemplateVars(n.userPrompt)
		sysMap := map[string]any{}
		for _, v := range sysVars {
			sysMap[v] = n.GetOtherNodeVariable(v)
		}
		userMap := map[string]any{}
		for _, v := range userVars {
			userMap[v] = n.GetOtherNodeVariable(v)
		}
		system := FormatTemplate(n.sysPrompt, sysMap)
		user := FormatTemplate(n.userPrompt, userMap)

		var msgs []LLMMessage
		if system != "" {
			msgs = append(msgs, LLMMessage{Role: "system", Content: system})
		}
		msgs = append(msgs, LLMMessage{Role: "user", Content: user})

		out, err := n.inv.Invoke(msgs)
		if err != nil {
			panic(fmt.Errorf("llm 节点 %s 调用失败: %w", n.nodeName, err))
		}
		// 流式帧已由宿主在 Invoke 实现内推送（蓝本 on_stream_msg/
		// on_stream_over 由 LLMNodeCallbackHandler 在 invoke 内回调；
		// 非流式实现可在此补一发完整消息）。
		if n.outputUser {
			n.State.SaveContext(out, "AI")
		}
		log := [][]map[string]any{{
			{"key": "system_prompt", "value": system, "type": "params"},
			{"key": "user_prompt", "value": user, "type": "params"},
			{"key": n.ID() + ".output", "value": out, "type": "variable"},
		}}
		return map[string]any{"output": out}, log
	})
}

func (n *LLMNode) parseLog(map[string]any) any { return nil }

// ---------- ToolNode（蓝本 ToolNode，剥离 web_search 引用标注）----------

// ToolNode 工具节点：node_params 中除 output 外的键均为工具入参（值先做
// 模板变量替换），执行后输出写回 {node_id}.output。
type ToolNode struct {
	BaseNode
	exec ToolExecutor
}

// initTool 构造期校验（蓝本 GptsToolsDao.get_tool_by_tool_key 存在性检查）。
func (n *ToolNode) initTool(exec ToolExecutor) error {
	if exec == nil {
		return fmt.Errorf("tool 节点 %s 缺少 ToolExecutor", n.nodeName)
	}
	name := n.toolName()
	if !exec.Has(name) {
		return fmt.Errorf("tool %s 不存在", name)
	}
	n.exec = exec
	return nil
}

// toolName 工具名（蓝本 tool_key → 工具注册名）。
func (n *ToolNode) toolName() string {
	// tool_key 优先 data 层（蓝本 node_data.tool_key）；参数层兜底；最后
	// 兼容测试/画布直传场景：无 tool_key 时用节点名。
	if n.data.ToolKey != "" {
		return n.data.ToolKey
	}
	if tk := str(n.Params()["tool_key"]); tk != "" {
		return tk
	}
	return n.nodeName
}

// Run 蓝本 _run：parse_tool_input → invoke → {output}。
func (n *ToolNode) Run(uniqueID string) map[string]any {
	return n.baseRun(uniqueID, func() (map[string]any, any) {
		input := map[string]any{}
		var logs []map[string]any
		for key, val := range n.Params() {
			if key == "output" || key == "tool_key" {
				continue
			}
			parsed, _ := n.ParseMsgWithVariables(renderValue(val))
			if parsed == "" {
				continue
			}
			input[key] = parsed
			logs = append(logs, map[string]any{"key": key, "value": parsed, "type": "params"})
		}
		out, err := n.exec.Execute(n.toolName(), toJSONStr(input))
		if err != nil {
			panic(fmt.Errorf("tool %s 执行失败: %w", n.toolName(), err))
		}
		logs = append(logs, map[string]any{"key": "output", "value": out, "type": "params"})
		return map[string]any{"output": out}, [][]map[string]any{logs}
	})
}

func (n *ToolNode) parseLog(map[string]any) any { return nil }

// ---------- ConditionNode（蓝本 ConditionNode + ConditionCases）----------

// ComparisonType 比较符（逐字对齐 common/condition.py ComparisonType）。
const (
	CmpEqual                = "equals"
	CmpNotEqual             = "not_equals"
	CmpGreaterThan          = "greater_than"
	CmpLessThan             = "less_than"
	CmpGreaterThanOrEqual   = "greater_than_or_equal"
	CmpLessThanOrEqual      = "less_than_or_equal"
	CmpContains             = "contains"
	CmpNotContains          = "not_contains"
	CmpIsEmpty              = "is_empty"
	CmpIsNotEmpty           = "is_not_empty"
	CmpStartsWith           = "starts_with"
	CmpEndsWith             = "ends_with"
	CmpRegex                = "regex"
)

// LogicType 逻辑连接（common/condition.py LogicType）。
const (
	LogicAnd = "and"
	LogicOr  = "or"
)

// ConditionOne 单条条件（蓝本 ConditionOne）。
type ConditionOne struct {
	LeftVar            string `json:"left_var"`            // 左变量（node_id.key 引用）
	ComparisonOperator string `json:"comparison_operation"` // 比较符
	RightValueType     string `json:"right_value_type"`     // const | ref
	RightValue         string `json:"right_value"`
}

// Evaluate 求值（蓝本 ConditionOne.evaluate；结果值记录由节点层负责）。
func (c *ConditionOne) Evaluate(get func(string) any) bool {
	left := renderValue(get(c.LeftVar))
	right := c.RightValue
	if c.RightValueType == "ref" && right != "" {
		right = renderValue(get(right))
	}
	return compareValues(c.ComparisonOperator, left, right)
}

// compareValues 十三比较符（蓝本 compare_two_value）。
func compareValues(op, left, right string) bool {
	switch op {
	case CmpEqual:
		return left == right
	case CmpNotEqual:
		return left != right
	case CmpContains:
		return strings.Contains(left, right)
	case CmpNotContains:
		return !strings.Contains(left, right)
	case CmpIsEmpty:
		return left == ""
	case CmpIsNotEmpty:
		return left != ""
	case CmpStartsWith:
		return strings.HasPrefix(left, right)
	case CmpEndsWith:
		return strings.HasSuffix(left, right)
	case CmpRegex:
		re, err := regexp.Compile(right)
		return err == nil && re.MatchString(left)
	case CmpGreaterThan, CmpGreaterThanOrEqual, CmpLessThan, CmpLessThanOrEqual:
		lf, lerr := strconv.ParseFloat(strings.TrimSpace(left), 64)
		rf, rerr := strconv.ParseFloat(strings.TrimSpace(right), 64)
		if lerr != nil || rerr != nil {
			return false
		}
		switch op {
		case CmpGreaterThan:
			return lf > rf
		case CmpGreaterThanOrEqual:
			return lf >= rf
		case CmpLessThan:
			return lf < rf
		default:
			return lf <= rf
		}
	default:
		return false // 蓝本 raise；此处按未命中处理（路由走 else 分支）
	}
}

// ConditionCase 一组条件（蓝本 ConditionCases：operator + conditions）。
type ConditionCase struct {
	ID         string         `json:"id"` // case ID（出边 sourceHandle）
	Operator   string         `json:"operator,omitempty"`
	Conditions []ConditionOne `json:"conditions,omitempty"`
}

// Evaluate 组求值（蓝本 evaluate_conditions：空 conditions = else 恒真）。
func (c *ConditionCase) Evaluate(get func(string) any) bool {
	if len(c.Conditions) == 0 {
		return true
	}
	op := c.Operator
	if op == "" {
		op = LogicAnd
	}
	for _, cond := range c.Conditions {
		hit := cond.Evaluate(get)
		if op == LogicAnd {
			if !hit {
				return false
			}
		} else if hit {
			return true
		}
	}
	return op == LogicAnd
}

// ConditionNode 条件路由节点。
type ConditionNode struct {
	BaseNode
	cases  []ConditionCase
	nextID string // 命中分支的后继（蓝本 _next_node_id）
}

// initConditions 解析 condition 参数（蓝本 __init__）。
func (n *ConditionNode) initConditions() error {
	raw := n.Params()["condition"]
	b, err := marshalJSON(raw)
	if err != nil {
		return fmt.Errorf("condition 节点 %s 参数无效: %w", n.nodeName, err)
	}
	if err := unmarshalJSON(b, &n.cases); err != nil {
		return fmt.Errorf("condition 节点 %s 参数无效: %w", n.nodeName, err)
	}
	if len(n.cases) == 0 {
		return fmt.Errorf("condition 节点 %s 缺少 condition 参数", n.nodeName)
	}
	return nil
}

// IsConditionNode 互斥路由节点。
func (n *ConditionNode) IsConditionNode() bool { return true }

// Run 蓝本 _run：逐 case 求值，首个命中出边路由；全不命中走 right_handle
// 默认分支（蓝本 get_next_node_id('right_handle')）。
func (n *ConditionNode) Run(uniqueID string) map[string]any {
	return n.baseRun(uniqueID, func() (map[string]any, any) {
		var logs []map[string]any
		for i := range n.cases {
			c := &n.cases[i]
			for j := range c.Conditions {
				cond := &c.Conditions[j]
				left := n.GetOtherNodeVariable(cond.LeftVar)
				logs = append(logs, map[string]any{"key": cond.LeftVar, "value": left, "type": "variable"})
				if cond.RightValueType == "ref" && cond.RightValue != "" {
					n.GetOtherNodeVariable(cond.RightValue)
				}
			}
			if c.Evaluate(func(k string) any { return n.State.GetVariableByStr(k, 0) }) {
				if ids := n.GetNextNodeID(c.ID); len(ids) > 0 {
					n.nextID = ids[0]
					return nil, [][]map[string]any{logs}
				}
			}
		}
		// else 兜底（蓝本：next_node_ids 为 None 时走 right_handle）
		if ids := n.GetNextNodeID("right_handle"); len(ids) > 0 {
			n.nextID = ids[0]
		} else {
			n.nextID = ""
		}
		return nil, [][]map[string]any{logs}
	})
}

// RouteNode 路由（蓝本 route_node 返回 _next_node_id）。
func (n *ConditionNode) RouteNode() string { return n.nextID }

func (n *ConditionNode) parseLog(map[string]any) any { return nil }
