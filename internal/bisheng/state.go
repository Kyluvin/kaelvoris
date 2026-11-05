// 全局状态管理（逐字对齐 graph/graph_state.py GraphState）：
// 变量池 variables_pool {node_id: {key: value}} + 会话历史（蓝本为
// ConversationBufferWindowMemory(k)，此处等价实现带窗口截断）。
package bisheng

import (
	"fmt"
	"strings"
	"sync"
)

// State GraphState：所有节点共享的全局状态。
type State struct {
	mu        sync.Mutex
	variables map[string]map[string]any // 变量池：node_id → key → value
	history   []Msg                     // 会话历史（human/ai 交替，多轮交互场景非严格 1:1）
	historyK  int                       // 窗口大小（start 节点 chat_history 参数，默认 10）
}

// NewState 初始化状态（蓝本 GraphState() 默认构造）。
func NewState() *State {
	return &State{variables: map[string]map[string]any{}, historyK: 10}
}

// SetVariable 节点输出写入全局变量池（graph_state.set_variable）。
func (s *State) SetVariable(nodeID, key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.variables == nil {
		s.variables = map[string]map[string]any{}
	}
	if s.variables[nodeID] == nil {
		s.variables[nodeID] = map[string]any{}
	}
	s.variables[nodeID][key] = value
}

// GetVariable 从变量池取数（graph_state.get_variable）；key == "chat_history"
// 时返回窗口内会话历史的 buffer_string 形式（蓝本 get_history_memory）。
func (s *State) GetVariable(nodeID, key string, historyCount int) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "chat_history" {
		return s.historyString(historyCount)
	}
	if s.variables == nil {
		return nil
	}
	if nodeVars, ok := s.variables[nodeID]; ok {
		return nodeVars[key]
	}
	return nil
}

// GetVariableByStr 点分路径取数（graph_state.get_variable_by_str）：
// "node_id.key" 或带下标 "node_id.key#index"（下标仅 list 类型生效，越界返回 nil）。
func (s *State) GetVariableByStr(contactKey string, historyCount int) any {
	nodeID, varKey, idx := splitVarPath(contactKey)
	val := s.GetVariable(nodeID, varKey, historyCount)
	if idx != nil {
		if list, ok := val.([]any); ok && *idx >= 0 && *idx < len(list) {
			return list[*idx]
		}
		return nil
	}
	return val
}

// splitVarPath 解析 "node_id.key#index"（首 '.' 分割，'#' 分割下标）。
func splitVarPath(key string) (nodeID, varKey string, idx *int) {
	dot := strings.Index(key, ".")
	if dot < 0 {
		return key, "", nil
	}
	nodeID, varKey = key[:dot], key[dot+1:]
	if hash := strings.Index(varKey, "#"); hash >= 0 {
		n := 0
		if _, err := fmt.Sscanf(varKey[hash+1:], "%d", &n); err == nil {
			idx = &n
		}
		varKey = varKey[:hash]
	}
	return nodeID, varKey, idx
}

// SaveContext 保存会话消息（graph_state.save_context；msg_sender: human|AI）。
func (s *State) SaveContext(content, msgSender string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	role := "ai"
	if msgSender == "human" {
		role = "human"
	}
	s.history = append(s.history, Msg{Role: role, Content: content})
}

// historyString 会话历史的 buffer_string（蓝本 get_buffer_string：
// "Human: ..."/"AI: ..." 逐条换行，窗口截断取最近 k 条）。
func (s *State) historyString(count int) string {
	k := s.historyK
	if count > 0 {
		k = count
	}
	if k <= 0 || len(s.history) == 0 {
		return ""
	}
	msgs := s.history
	if len(msgs) > k {
		msgs = msgs[len(msgs)-k:]
	}
	var sb strings.Builder
	for _, m := range msgs {
		prefix := "AI"
		if m.Role == "human" {
			prefix = "Human"
		}
		sb.WriteString(prefix)
		sb.WriteString(": ")
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}

// HistoryList 最近 count 条消息（蓝本 get_history_list）。
func (s *State) HistoryList(count int) []Msg {
	s.mu.Lock()
	defer s.mu.Unlock()
	if count <= 0 || count > len(s.history) {
		if len(s.history) == 0 {
			return nil
		}
		count = len(s.history)
	}
	out := make([]Msg, count)
	copy(out, s.history[len(s.history)-count:])
	return out
}

// SetHistoryK 设置会话窗口（start 节点初始化 chat_history 时调用）。
func (s *State) SetHistoryK(k int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k > 0 {
		s.historyK = k
	}
}

// AllVariables 全量变量（键 = "node_id.key"，蓝本 get_all_variables；
// preset_question / custom_variables 的子键展开为 "node_id.key#sub"）。
func (s *State) AllVariables() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	ret := map[string]any{}
	for nodeID, nodeVars := range s.variables {
		for key, value := range nodeVars {
			ret[nodeID+"."+key] = value
			if key == "preset_question" || key == "custom_variables" {
				if m, ok := value.(map[string]any); ok {
					for k, v := range m {
						ret[fmt.Sprintf("%s.%s#%s", nodeID, key, k)] = v
					}
				}
			}
		}
	}
	return ret
}
