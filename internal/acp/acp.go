// Package acp ACP（Agent Client Protocol）客户端运行时。
//
// ACP 是 Zed 推出的开放协议（JSON-RPC 2.0 over stdio / NDJSON 帧），
// 用于编辑器/客户端与编码 agent（Claude Code、Gemini CLI、Codex 等）
// 之间的标准化通信。kaelvoris 以 ACP client 身份接入：配置 agent 启动
// 命令后，任务文本经 session/prompt 派发给外部 agent 执行，流式更新经
// session/update 通知回传，映射为 ClineMessage 进入统一事件桥（聊天流/
// 会话持久化/远程镜像零改动）。
//
// 协议要点（对齐 agentclientprotocol.com schema，简版实现）：
//   - 客户端 → agent：initialize / session/new / session/prompt /
//     session/cancel；反向请求 fs/read_text_file、fs/write_text_file、
//     session/request_permission。
//   - agent → 客户端通知：session/update（agent_message_chunk 流式增量、
//     tool_call / tool_call_update 工具卡片）；响应帧只消费与请求同 ID 者。
//   - 认证：initialize 响应可带 authMethods；本实现不处理 auth（配置的
//     命令自含认证，如 claude-code-acp 读 ~/.claude 登录态）。
//
// 不实现（边界）：agent 侧能力（terminal/output 等 agent → client 请求
// 只拒绝并回 method not found，Claude Code/Gemini 适配器对此均有降级）；
// modes/prompts 扩展方法。
package acp

import (
	"encoding/json"
)

// 协议版本（对齐 ACP v1：schema 版本随时间演进，agent 一般向后兼容）。
const protocolVersion = 1

// ClientCapabilities 客户端能力声明（initialize 参数）。
// fs 能力声明：readTextFile/writeTextFile 反向请求可用（agent 的编辑
// 动作经此落盘到 kaelvoris 配置的工作区）。
type clientCapabilities struct {
	Fs struct {
		ReadTextFile  bool `json:"readTextFile"`
		WriteTextFile bool `json:"writeTextFile"`
	} `json:"fs"`
	Terminal bool `json:"terminal,omitempty"`
}

// AgentInfo initialize 响应中的 agent 元信息。
type AgentInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// AuthMethod initialize 响应中的认证方式（仅透传展示，本实现不代登录）。
type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// InitializeResult initialize 握手结果。
type InitializeResult struct {
	ProtocolVersion int          `json:"protocolVersion"`
	AgentInfo       *AgentInfo   `json:"agentInfo,omitempty"`
	AuthMethods     []AuthMethod `json:"authMethods,omitempty"`
}

// AgentConfig 单个 ACP agent 的启动配置（~/.kaelvoris/acp-agents.json 数组）。
type AgentConfig struct {
	Name    string            `json:"name"`           // 显示名（如 "Claude Code"）
	Command string            `json:"command"`        // 启动命令（整行支持：'npx -y @zed-industries/claude-code-acp'）
	Args    []string          `json:"args,omitempty"` // 附加参数（Command 含空白且 Args 为空时整行拆分）
	Env     map[string]string `json:"env,omitempty"`  // 额外环境变量
	Cwd     string            `json:"cwd,omitempty"`  // 默认工作目录（空 = 会话工作区覆盖）
	Enabled bool              `json:"enabled"`        // 启用
	// Protocol 接入协议：""（默认）= ACP；"codex" = Codex app-server（codexbridge.go）。
	Protocol string `json:"protocol,omitempty"`
}

// rpcRequest JSON-RPC 2.0 请求。
type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// rpcResponse JSON-RPC 2.0 响应。
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError JSON-RPC 错误对象。
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// rpcMessage 通用入站帧（响应与请求/通知同构，先按 ID 字段分流）。
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ContentBlock session/update 载荷中的内容块（ACP 侧：text / image 等）。
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Data / MimeType 图片块（agent 回传截图等场景，简版仅文本透传）。
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// SessionUpdate session/update 通知的 update 载荷（按 update 分流）。
// Content 为 message_chunk 类单块；ContentBlocks 为 tool_call 类内容数组
// （同 json 键 content——chunk 与 tool_call 互斥出现，分流后各取所需）。
type SessionUpdate struct {
	SessionUpdate string `json:"sessionUpdate"` // agent_message_chunk | tool_call | tool_call_update | ...
	// agent_message_chunk / agent_thought_chunk（单内容块）
	Content *ContentBlock `json:"content,omitempty"`
	// tool_call / tool_call_update
	ToolCallID string          `json:"toolCallId,omitempty"`
	Title      string          `json:"title,omitempty"`
	Kind       string          `json:"kind,omitempty"` // read | edit | delete | move | search | execute | think | fetch | other
	Status     string          `json:"status,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
	// rawContent 内容块数组（tool_call 回传输出/摘要；custom tag 避开
	// 与 Content 的 json 键冲突，由 UnmarshalJSON 二次解析）。
	rawContent json.RawMessage `json:"-"`
}

// UnmarshalJSON 自定义反序列化：content 字段双形态（单块对象 / 数组），
// 先试单块再试数组（ACP message_chunk 为单块，tool_call 为数组）。
func (s *SessionUpdate) UnmarshalJSON(b []byte) error {
	type alias struct {
		SessionUpdate string          `json:"sessionUpdate"`
		ToolCallID    string          `json:"toolCallId"`
		Title         string          `json:"title"`
		Kind          string          `json:"kind"`
		Status        string          `json:"status"`
		RawInput      json.RawMessage `json:"rawInput"`
		// content 双形态：单块对象（message_chunk）/ 数组（tool_call），
		// RawMessage 先收后按形态解析（强类型单块遇数组帧会整体失败）。
		Content json.RawMessage `json:"content"`
	}
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	s.SessionUpdate = a.SessionUpdate
	s.ToolCallID = a.ToolCallID
	s.Title = a.Title
	s.Kind = a.Kind
	s.Status = a.Status
	s.RawInput = a.RawInput
	// content 双形态：单块对象（message_chunk）或数组（tool_call）。
	if len(a.Content) > 0 {
		var single ContentBlock
		if json.Unmarshal(a.Content, &single) == nil && (single.Type != "" || single.Text != "") {
			s.Content = &single
			s.rawContent, _ = json.Marshal([]ContentBlock{single})
		} else {
			var arr []ContentBlock
			if json.Unmarshal(a.Content, &arr) == nil {
				s.rawContent, _ = json.Marshal(arr)
			}
		}
	}
	return nil
}

// ContentBlocks 返回工具卡内容块数组（message_chunk 场景返回单块包装）。
func (s *SessionUpdate) ContentBlocks() []ContentBlock {
	if len(s.rawContent) > 0 {
		var arr []ContentBlock
		if json.Unmarshal(s.rawContent, &arr) == nil {
			return arr
		}
	}
	if s.Content != nil {
		return []ContentBlock{*s.Content}
	}
	return nil
}

// ToolCallUpdate request_permission 载荷中 toolCall 字段的形状（审批卡展示用）。
type ToolCallUpdate = SessionUpdate

// StopReason session/prompt 响应的 stopReason。
type StopReason string

const (
	StopEndTurn     StopReason = "end_turn"
	StopMaxTokens   StopReason = "max_tokens"
	StopMaxRequests StopReason = "max_turn_requests"
	StopRefusal     StopReason = "refusal"
	StopCancelled   StopReason = "cancelled"
)

// PromptResult session/prompt 响应。
type PromptResult struct {
	StopReason StopReason `json:"stopReason"`
}

// PermissionOption session/request_permission 的选项（kind ∈ allow_once |
// allow_always | reject_once | reject_always；OptionID 为 agent 生成的标识）。
type PermissionOption struct {
	Kind     string `json:"kind"`
	Name     string `json:"name,omitempty"`
	OptionID string `json:"optionId"`
}

// PermissionRequestPayload session/request_permission 反向请求载荷。
type PermissionRequestPayload struct {
	SessionID string `json:"sessionId"`
	// ToolCall 触发审批的工具调用摘要（ToolCallUpdate 形状：title/kind/rawInput）。
	ToolCall json.RawMessage    `json:"toolCall,omitempty"`
	Options  []PermissionOption `json:"options"`
}
