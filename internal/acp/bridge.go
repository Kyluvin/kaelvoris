// bridge.go ACP session/update → ClineMessage 映射桥。
//
// ACP 的 update 流（agent_message_chunk / tool_call / tool_call_update /
// agent_thought_chunk / plan）映射为 pkg/message.ClineMessage，复用统一
// 事件桥（makeSayBridge）：前端聊天流零改动（partial 流式合并 / 工具卡 /
// 待办联动全部复用既有渲染路径）。
package acp

import (
	"encoding/json"
	"strings"
	"time"

	"kaelvoris/pkg/message"
)

// UpdateMapper 把 session/update 映射为 ClineMessage（含流式累计状态）。
// 生命周期 = 一次 turn（Prompt 前创建，turn 结束丢弃）；HandleUpdate 由
// Client 消费 goroutine 串行调用，无需加锁。
type UpdateMapper struct {
	textBuf    strings.Builder // 正文流式累计
	thoughtBuf strings.Builder // 思考流式累计
	// onSay 消息出口（app 层 makeSayBridge 语义：落历史 + EventsEmit）。
	onSay func(message.ClineMessage)
	// Final stopReason（Prompt 响应到达后设置；宿主层读取）。
	FinalReason StopReason
}

// NewUpdateMapper 创建映射器（onSay 为空 = 仅累计不输出）。
func NewUpdateMapper(onSay func(message.ClineMessage)) *UpdateMapper {
	return &UpdateMapper{onSay: onSay}
}

// say 推送一条消息（onSay 为空 = 丢弃）。
func (u *UpdateMapper) say(m message.ClineMessage) {
	if u.onSay != nil {
		u.onSay(m)
	}
}

// HandleUpdate 消费一条 session/update（Client.updateFn 串行调用）。
func (u *UpdateMapper) HandleUpdate(up SessionUpdate, raw []byte) {
	switch up.SessionUpdate {
	case "agent_message_chunk":
		if up.Content != nil && up.Content.Text != "" {
			u.textBuf.WriteString(up.Content.Text)
			u.say(message.ClineMessage{
				TS: nowNano(), Type: message.TypeSay, Say: message.SayText,
				Text: u.textBuf.String(), Partial: true,
			})
		}
	case "agent_thought_chunk":
		if up.Content != nil && up.Content.Text != "" {
			u.thoughtBuf.WriteString(up.Content.Text)
			u.say(message.ClineMessage{
				TS: nowNano(), Type: message.TypeSay, Say: message.SayReasoning,
				Text: u.thoughtBuf.String(), Partial: true,
			})
		}
	case "tool_call":
		u.emitTool(up, "pending")
	case "tool_call_update":
		u.emitTool(up, up.Status)
	default:
		// 未知 update：忽略（协议演进兼容）。
	}
}

// HandleCodex 消费一条 codex app-server 通知（method + params），映射为
// SessionUpdate 后复用 HandleUpdate（正文 delta / 思考 delta / 工具卡）。
// 由 CodexClient.updateFn 串行调用（对齐 UpdateMapper 单线程语义）。
// 注意：codex 通知方法与 ACP session/update 不同（item/agentMessage/delta 等），
// 此处独立解析，不依赖 codexbridge.go 的映射（已删 MapNotification）。
func (u *UpdateMapper) HandleCodex(method string, params []byte) {
	switch method {
	case "item/agentMessage/delta":
		var p struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(params, &p) == nil && p.Delta != "" {
			u.HandleUpdate(SessionUpdate{SessionUpdate: "agent_message_chunk", Content: &ContentBlock{Type: "text", Text: p.Delta}}, params)
		}
	case "item/reasoning/textDelta":
		var p struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(params, &p) == nil && p.Delta != "" {
			u.HandleUpdate(SessionUpdate{SessionUpdate: "agent_thought_chunk", Content: &ContentBlock{Type: "text", Text: p.Delta}}, params)
		}
	case "item/started", "item/completed":
		var p struct {
			Item codexThreadItem `json:"item"`
		}
		if json.Unmarshal(params, &p) == nil {
			u.handleCodexItem(p.Item, method == "item/started")
		}
	default:
		// 其余通知（turn/started、plan delta、outputDelta 等）忽略（协议演进兼容）。
	}
}

// handleCodexItem item 通知 → 工具卡（commandExecution/fileChange/mcpToolCall）。
func (u *UpdateMapper) handleCodexItem(it codexThreadItem, started bool) {
	status := "completed"
	if started {
		status = "pending"
	}
	var title, kind, output string
	switch it.Type {
	case "commandExecution":
		title, kind, output = it.Command, "execute", it.AggregatedOutput
	case "fileChange":
		title, kind = "文件变更", "edit"
	case "mcpToolCall":
		title, kind = it.Server+"/"+it.Tool, "other"
	default:
		// agentMessage/plan/reasoning 等非工具 item：忽略（正文走 delta 流）。
		return
	}
	up := SessionUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    it.ID,
		Title:         title,
		Kind:          kind,
		Status:        status,
	}
	if output != "" {
		up.rawContent, _ = json.Marshal([]ContentBlock{{Type: "text", Text: output}})
	}
	u.emitTool(up, status)
}

// FinalText 输出定型全文（turn 结束时调用一次：partial=false 触发 AppendMerged
// 清理流式帧；空 turn 返回 ""）。
func (u *UpdateMapper) FinalText() string {
	t := u.textBuf.String()
	if strings.TrimSpace(t) == "" {
		return ""
	}
	u.say(message.ClineMessage{
		TS: nowNano(), Type: message.TypeSay, Say: message.SayText,
		Text: t, Partial: false,
	})
	return t
}

// emitTool 工具卡（say tool；text 为 ToolCallInfo JSON，对齐引擎工具消息格式，
// 前端 parseToolMessage 直接渲染）。
func (u *UpdateMapper) emitTool(up SessionUpdate, status string) {
	var out strings.Builder
	for _, c := range up.ContentBlocks() {
		if c.Text != "" {
			if out.Len() > 0 {
				out.WriteString("\n")
			}
			out.WriteString(c.Text)
		}
	}
	info := message.ToolCallInfo{
		Tool: "acp_" + kindLabel(up.Kind),
		Cmd:  up.Title,
	}
	if s := out.String(); s != "" {
		info.Args = s
	}
	u.say(message.ClineMessage{
		TS: nowNano(), Type: message.TypeSay, Say: message.SayTool,
		Text: mustJSON(info),
	})
}

// kindLabel 工具类别 → 中文标签（read/edit/execute/...）。
func kindLabel(kind string) string {
	switch kind {
	case "read":
		return "读"
	case "edit":
		return "改"
	case "delete":
		return "删"
	case "move":
		return "移"
	case "search":
		return "搜"
	case "execute":
		return "执行"
	case "think":
		return "思考"
	case "fetch":
		return "取"
	default:
		return "工具"
	}
}

// mustJSON 序列化（失败 = "{}"）。
func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// nowNano 当前纳秒时间戳。
func nowNano() int64 { return time.Now().UnixNano() }
