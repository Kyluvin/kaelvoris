// 结构节点（对齐 nodes/start/start.py、nodes/end/end.py、
// nodes/output/output.py、nodes/output/output_fake.py）。
package bisheng

import (
	"encoding/json"
	"strings"
	"time"
)

// ---------- StartNode（蓝本 StartNode）----------

// StartNode 开始节点：初始化当前时间/会话窗口，产出 current_time /
// chat_history / preset_question / custom_variables 全局变量，推送开场
// 引导语与引导问题。蓝本的用户体系（user_info/部门/角色）不移植。
type StartNode struct {
	BaseNode
	userInfo string // 宿主注入的运行用户信息（可选；蓝本 UserDao 等价物）
}

// SetUserInfo 注入用户信息 JSON（宿主桥接点；蓝本 _user_info）。
func (n *StartNode) SetUserInfo(info string) { n.userInfo = info }

// Run 蓝本 _run（经 baseRun 包装）。
func (n *StartNode) Run(uniqueID string) map[string]any {
	return n.baseRun(uniqueID, func() (map[string]any, any) {
		p := n.Params()
		currentTime := time.Now().Format("2006-01-02 15:04:05") // 蓝本 %Y-%m-%d %H:%M:%S
		n.State.SetHistoryK(toInt(p["chat_history"], 10))

		if gw := str(p["guide_word"]); gw != "" {
			n.Callback.OnGuideWord(GuideWordData{NodeID: n.ID(), Name: n.Name(), UniqueID: uniqueID, GuideWord: gw})
		}
		if gq := strSlice(p["guide_question"]); len(gq) > 0 {
			n.Callback.OnGuideQuestion(GuideQuestionData{NodeID: n.ID(), Name: n.Name(), UniqueID: uniqueID, GuideQuestion: gq})
		}

		// preset_question: [{key,value}] → dict（蓝本 convert preset_question）
		preset := map[string]any{}
		if list, ok := p["preset_question"].([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					if k := str(m["key"]); k != "" {
						preset[k] = m["value"]
					}
				}
			}
		}
		// custom_variables: [{key,value}] → dict（蓝本 convert custom vars）
		custom := map[string]any{}
		if list, ok := p["custom_variables"].([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					if k := str(m["key"]); k != "" {
						custom[k] = m["value"]
					}
				}
			}
		}
		result := map[string]any{
			"current_time":     currentTime,
			"chat_history":     "",
			"preset_question":  preset,
			"custom_variables": custom,
		}
		if n.userInfo != "" {
			result["user_info"] = n.userInfo
		}
		log := []map[string]any{
			{"key": "current_time", "value": currentTime, "type": "params"},
			{"key": "preset_question", "value": preset, "type": "params"},
		}
		return result, [][]map[string]any{log}
	})
}

func (n *StartNode) parseLog(result map[string]any) any { return nil }

// ---------- EndNode（蓝本 EndNode：_run 返回 None）----------

// EndNode 结束节点：无输出，引擎遇 END 类型收尾。
type EndNode struct{ BaseNode }

func (n *EndNode) Run(uniqueID string) map[string]any {
	return n.baseRun(uniqueID, func() (map[string]any, any) { return nil, nil })
}

func (n *EndNode) parseLog(map[string]any) any { return nil }

// ---------- OutputNode（蓝本 OutputNode，剥离 minio/引用注册）----------

// OutputNode 输出节点：渲染消息推送用户；output_result 支持三种交互：
//
//	message  纯消息展示，直接透传到目标节点
//	input    展示消息后中断，等用户输入（写回 {node_id}.output_result）
//	choose   展示消息 + 选项，等用户选择（按选中项 sourceHandle 路由）
type OutputNode struct {
	BaseNode
	outputType  string           // message | input | choose（蓝本 output_result.type）
	outputResult string          // output_result.value（message 型直接透传）
	outputMsg   string           // 输出消息模板
	outputFiles []map[string]any // 输出文件（蓝本 minio share link，此处仅透传元数据）
	// handledResult 用户回填后的 output_result（蓝本 _handled_output_result）
	handledResult string
	// nextNodeID 非 choose 交互的固定后继（蓝本 _next_node_id）
	nextNodeID []string
}

// initOutput 构造期展开参数（蓝本 OutputNode.__init__）。
func (n *OutputNode) initOutput() {
	p := n.Params()
	// output_result: {type, value}（嵌套结构，容忍 any/map 形态）
	if or, ok := p["output_result"].(map[string]any); ok {
		n.outputType = str(or["type"])
		n.outputResult = str(or["value"])
	}
	// 蓝本兼容：output_msg 或 message 二选一（{msg, files}）
	src, ok := p["output_msg"]
	if !ok {
		src = p["message"]
	}
	if m, ok := src.(map[string]any); ok {
		n.outputMsg = str(m["msg"])
		if files, ok := m["files"].([]any); ok {
			for _, f := range files {
				if fm, ok := f.(map[string]any); ok {
					n.outputFiles = append(n.outputFiles, fm)
				}
			}
		}
	}
	// 非 choose 交互：固定后继 = 全部目标（蓝本 _next_node_id）
	n.nextNodeID = n.GetNextNodeID("right_handle")
	if len(n.nextNodeID) == 0 {
		for _, e := range n.TargetEdges {
			n.nextNodeID = append(n.nextNodeID, e.Target)
		}
	}
}

// IsConditionNode choose 型为互斥节点（蓝本 is_condition_node）。
func (n *OutputNode) IsConditionNode() bool { return n.outputType == "choose" }

// RouteNode 路由：choose 按用户选择项查后继（蓝本 route_node）。
func (n *OutputNode) RouteNode() string {
	if n.outputType == "choose" {
		if ids := n.GetNextNodeID(n.handledResult); len(ids) > 0 {
			return ids[0]
		}
		return ""
	}
	if len(n.nextNodeID) > 0 {
		return n.nextNodeID[0]
	}
	return ""
}

// GetInputSchema input/choose 型返回表单描述（蓝本 get_input_schema：
// 返回 group_params 供前端渲染；message 型 nil = 不交互）。
func (n *OutputNode) GetInputSchema() any {
	if n.outputType != "input" && n.outputType != "choose" {
		return nil
	}
	// choose 型选项列表：出边 sourceHandle 与 options 对应（前端选择器）
	if n.outputType == "choose" {
		var opts []map[string]any
		for _, e := range n.TargetEdges {
			opts = append(opts, map[string]any{"value": e.SourceHandle, "target": e.Target})
		}
		return map[string]any{"type": "choose", "options": opts, "msg": n.outputMsg, "key": "output_result"}
	}
	return map[string]any{"type": "input", "msg": n.outputMsg, "key": "output_result"}
}

// HandleInput 用户回填（蓝本 handle_input：写会话历史 + output_result 变量）。
func (n *OutputNode) HandleInput(input map[string]any) {
	raw, ok := input["output_result"]
	if !ok {
		for k, v := range input {
			n.Params()[k] = v
		}
		return
	}
	b, _ := json.Marshal(input)
	n.State.SaveContext(string(b), "human")
	n.handledResult = str(raw)
	n.State.SetVariable(n.ID(), "output_result", n.handledResult)
}

// Run 蓝本 _run：渲染消息 → on_output_msg / on_output_input / on_output_choose。
func (n *OutputNode) Run(uniqueID string) map[string]any {
	return n.baseRun(uniqueID, func() (map[string]any, any) {
		// 模板变量替换（蓝本 parse_output_msg → parse_template_msg）
		parsedMsg, _ := n.ParseMsgWithVariables(n.outputMsg)
		result := map[string]any{
			"message": parsedMsg,
			"output_result": n.handledResult,
		}
		if n.outputType == "message" {
			result["output_result"] = n.outputResult
		}
		switch n.outputType {
		case "choose":
			var opts []map[string]any
			for _, e := range n.TargetEdges {
				opts = append(opts, map[string]any{"value": e.SourceHandle, "target": e.Target})
			}
			n.Callback.OnOutputChoose(OutputMsgChooseData{
				UniqueID: uniqueID, NodeID: n.ID(), Name: n.Name(), Msg: parsedMsg,
				Key: "output_result", Options: opts,
			})
		case "input":
			n.Callback.OnOutputInput(OutputMsgInputData{
				UniqueID: uniqueID, NodeID: n.ID(), Name: n.Name(), Msg: parsedMsg,
				Key: "output_result", InputMsg: n.outputResult,
			})
		default:
			n.Callback.OnOutputMsg(OutputMsgData{
				UniqueID: uniqueID, NodeID: n.ID(), Name: n.Name(),
				Msg: parsedMsg, Files: n.outputFiles,
			})
		}
		log := []map[string]any{{"key": "output_msg", "value": parsedMsg, "type": "params"}}
		return result, [][]map[string]any{log}
	})
}

func (n *OutputNode) parseLog(map[string]any) any { return nil }

// ---------- OutputFakeNode（蓝本 output_fake.py）----------

// OutputFakeNode output 节点的影子节点：output 节点执行后引擎在此中断
// 等用户输入；用户回填后 fake 节点补发 on_node_end 并按路由放行。
// 蓝本 run 直接透传 state，不产生业务输出。
type OutputFakeNode struct {
	BaseNode
	output *OutputNode // 关联的 output 节点
}

func (n *OutputFakeNode) GetInputSchema() any { return n.output.GetInputSchema() }

func (n *OutputFakeNode) Run(uniqueID string) map[string]any {
	// 用户已回填（choose 型手动选择 / input 型文本）：补发 end 事件
	//（蓝本 OutputFakeNode 不发事件，end 由 output.run 的 finally 分支在
	// 中断场景缺位 —— 此处补齐保证前端轨迹完整）。
	n.Callback.OnNodeEnd(NodeEndData{
		UniqueID: uniqueID, NodeID: n.output.ID(), Name: n.output.Name(),
		InputData: n.output.otherVars,
	})
	return nil
}

func (n *OutputFakeNode) parseLog(map[string]any) any { return nil }

// ---------- 公共小工具 ----------

// str any → string（nil 安全）。
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// toInt any → int（默认值兜底）。
func toInt(v any, def int) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		var n int
		if _, err := sscanfInt(t, &n); err == nil {
			return n
		}
	}
	return def
}

// strSlice any → []string（JSON 数组/切片容忍）。
func strSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		var out []string
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
