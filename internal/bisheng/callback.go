// 回调接口 + 事件载荷（逐字对齐 callback/base_callback.py + callback/event.py）。
// 引擎执行过程中经 Callback 推送节点级事件；宿主（桌面端/CLI/IM）自行桥接到
// ClineMessage say 流（kaelvoris 事件桥语义）。
package bisheng

// NodeStartData 节点开始事件。
type NodeStartData struct {
	UniqueID string `json:"unique_id"` // 本次执行唯一 ID
	NodeID   string `json:"node_id"`   // 节点唯一 ID
	Name     string `json:"name"`      // 节点名称
}

// NodeEndData 节点结束事件（蓝本 NodeEndData extends NodeStartData）。
type NodeEndData struct {
	UniqueID  string           `json:"unique_id"`
	NodeID    string           `json:"node_id"`
	Name      string           `json:"name"`
	Reason    string           `json:"reason,omitempty"`   // 执行错误原因（成功为空）
	LogData   any              `json:"log_data,omitempty"` // 节点运行日志（parse_log 产物）
	InputData map[string]any   `json:"input_data,omitempty"` // 本节点消费的上游变量
}

// UserInputData 用户输入请求事件（engine 判定中断时回调）。
type UserInputData struct {
	NodeID      string `json:"node_id"`
	Name        string `json:"name"`
	InputSchema any    `json:"input_schema"`
}

// GuideWordData 开场引导语（start 节点）。
type GuideWordData struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	UniqueID  string `json:"unique_id,omitempty"`
	GuideWord string `json:"guide_word"`
}

// GuideQuestionData 开场引导问题列表（start 节点）。
type GuideQuestionData struct {
	NodeID        string   `json:"node_id"`
	Name          string   `json:"name"`
	UniqueID      string   `json:"unique_id,omitempty"`
	GuideQuestion []string `json:"guide_question"`
}

// OutputMsgData output 节点消息输出事件。
type OutputMsgData struct {
	UniqueID  string          `json:"unique_id"`
	NodeID    string          `json:"node_id"`
	Name      string          `json:"name"`
	Msg       string          `json:"msg"`
	Files     []map[string]any `json:"files,omitempty"`
	OutputKey string          `json:"output_key,omitempty"`
}

// OutputMsgInputData output 节点（input 型交互）等待用户输入事件。
type OutputMsgInputData struct {
	UniqueID  string `json:"unique_id,omitempty"`
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Msg       string `json:"msg"`
	Key       string `json:"key"`        // 用户输入写入的变量 key
	InputMsg  string `json:"input_msg"`  // 默认输入值
}

// OutputMsgChooseData output 节点（choose 型交互）等待用户选择事件。
type OutputMsgChooseData struct {
	UniqueID string           `json:"unique_id,omitempty"`
	NodeID   string           `json:"node_id"`
	Name     string           `json:"name"`
	Msg      string           `json:"msg"`
	Key      string           `json:"key"`     // 用户选择写入的变量 key
	Options  []map[string]any `json:"options"` // 选项（target_handle → 下游路由）
}

// StreamMsgData LLM 节点流式输出帧。
type StreamMsgData struct {
	UniqueID string `json:"unique_id"`
	NodeID   string `json:"node_id"`
	Name     string `json:"name"`
	Msg      string `json:"msg"`
	OutputKey string `json:"output_key"`
}

// Callback 引擎回调接口（蓝本 BaseCallback 的虚方法集；宿主按需实现，
// 空操作由 NoopCallback 提供）。蓝本事件一一对应，未移植商业版事件
// （citation/usage 归属企业设施）。
type Callback interface {
	OnNodeStart(data NodeStartData)
	OnNodeEnd(data NodeEndData)
	OnUserInput(data UserInputData)
	OnGuideWord(data GuideWordData)
	OnGuideQuestion(data GuideQuestionData)
	OnStreamMsg(data StreamMsgData)
	OnOutputMsg(data OutputMsgData)
	OnOutputInput(data OutputMsgInputData)
	OnOutputChoose(data OutputMsgChooseData)
}

// NoopCallback 空回调（全部事件静默；CLI/测试场景）。
type NoopCallback struct{}

func (NoopCallback) OnNodeStart(NodeStartData)       {}
func (NoopCallback) OnNodeEnd(NodeEndData)           {}
func (NoopCallback) OnUserInput(UserInputData)       {}
func (NoopCallback) OnGuideWord(GuideWordData)       {}
func (NoopCallback) OnGuideQuestion(GuideQuestionData) {}
func (NoopCallback) OnStreamMsg(StreamMsgData)       {}
func (NoopCallback) OnOutputMsg(OutputMsgData)       {}
func (NoopCallback) OnOutputInput(OutputMsgInputData) {}
func (NoopCallback) OnOutputChoose(OutputMsgChooseData) {}
