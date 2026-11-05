// InputNode 移植（对齐 nodes/input/input.py 核心交互语义；剥离 minio 下载、
// 临时知识库摄取、文件解析管线 —— 宿主经 HandleInput 直接回填解析后内容）。
//
// 蓝本两类交互：
//   - dialog（对话式）：会话流内追加输入框，user_input + 附件
//   - form（表单式）：按 group_params 定义逐字段收集，file 型字段由宿主处理
//
// 中断语义：引擎首次执行到 input 节点 → GetInputSchema 非空 → 状态置
// INPUT 并回调 OnUserInput；用户提交经 HandleInput 回填后从断点续跑。
package bisheng

import (
	"encoding/json"
)

// InputNode 用户输入节点。
type InputNode struct{ BaseNode }

// dialogInput 对话式输入判定（蓝本 is_dialog_input：group_params 无表单字段
// 且启用 dialog 参数；此处按参数存在性判定）。
func (n *InputNode) dialogInput() bool {
	p := n.Params()
	_, hasUserInput := p["user_input"]
	return hasUserInput && !hasFormFields(n.data.GroupParams)
}

// GetInputSchema 表单描述（蓝本 get_input_schema：dialog 型返回 user_input
// 定义；form 型返回 group_params 全量字段供前端渲染）。
func (n *InputNode) GetInputSchema() any {
	p := n.Params()
	if n.dialogInput() {
		schema := map[string]any{
			"type": "dialog",
			"key":  "user_input",
			"msg":  str(p["guide_word"]),
		}
		if v, ok := p["user_input"]; ok && v != nil {
			schema["placeholder"] = v
		}
		return schema
	}
	// form：字段集 = group_params（label/type/required/placeholder 已内嵌）
	return map[string]any{"type": "form", "params": n.data.GroupParams}
}

// HandleInput 用户回填（蓝本 handle_input：并入 node_params；form 型字段
// 值经 Run 写回变量池）。
func (n *InputNode) HandleInput(input map[string]any) {
	for k, v := range input {
		n.Params()[k] = v
	}
}

// Run 蓝本 _run：dialog 型产出 user_input（+ 附件透传字段）；form 型逐字段
// 产出（file 型字段值由宿主解析后回填，本节点仅透传）。输入内容写会话历史。
func (n *InputNode) Run(uniqueID string) map[string]any {
	return n.baseRun(uniqueID, func() (map[string]any, any) {
		var result map[string]any
		if n.dialogInput() {
			result = map[string]any{"user_input": str(n.Params()["user_input"])}
			// 附件字段透传（蓝本 dialog_files / dialog_files_content；
			// kaelvoris 侧宿主已解析，直接透传 content/path）
			for _, k := range []string{"dialog_files", "dialog_files_content", "dialog_file_paths"} {
				if v, ok := n.Params()[k]; ok && v != nil {
					result[k] = v
				}
			}
			content := ""
			if c, ok := result["dialog_files_content"]; ok {
				content = str(c) + "\n"
			}
			n.State.SaveContext(content+result["user_input"].(string), "human")
		} else {
			result = map[string]any{}
			human := ""
			for key, value := range n.Params() {
				// 跳过非数据参数（guide_word 等展示字段）
				if isMetaParam(key) {
					continue
				}
				result[key] = value
				human += key + ": " + renderValue(value) + "\n"
			}
			n.State.SaveContext(human, "human")
		}
		// parse_log（蓝本：非 file 字段逐条 {node_id.key} variable 日志）
		var logs []map[string]any
		for k, v := range result {
			logs = append(logs, map[string]any{"key": n.ID() + "." + k, "value": v, "type": "variable"})
		}
		return result, [][]map[string]any{logs}
	})
}

func (n *InputNode) parseLog(map[string]any) any { return nil }

// isMetaParam 展示类参数不写变量池（蓝本 node_params 中的非表单键）。
func isMetaParam(key string) bool {
	switch key {
	case "guide_word", "guide_question", "chat_history", "output_user", "dialog_file_accept":
		return true
	}
	return false
}

// hasFormFields group_params 是否含表单字段。
func hasFormFields(groups []NodeGroupParams) bool {
	for _, g := range groups {
		if len(g.Params) > 0 {
			return true
		}
	}
	return false
}

// jsonOf 调试辅助（input 附件结构打印）。
func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
