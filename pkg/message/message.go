// Package message 移植自 Zoo-Code 的 ClineMessage 协议
// （参考/Zoo-Code-3.82.2/apps/cli/docs/AGENT_LOOP.md 与 @roo-code/types）。
//
// ClineMessage 是 Agent 内核与所有消息通道（桌面/飞书/微信）之间的唯一事件协议：
//   - say：信息展示，不阻塞 Agent
//   - ask：需要用户应答，阻塞 Agent 循环直至通道回传应答
package message

import "time"

// MessageType 消息大类：ask（阻塞等待应答）或 say（信息流）。
type MessageType string

const (
	TypeAsk MessageType = "ask"
	TypeSay MessageType = "say"
)

// AskType ask 消息细分类型（对齐 Zoo-Code ClineAsk）。
type AskType string

const (
	// AskFollowup 提问等待文本回答（阻塞）。
	AskFollowup AskType = "followup"
	// AskTool 请求执行文件/搜索等工具（阻塞，等待批准）。
	AskTool AskType = "tool"
	// AskCommand 请求执行终端命令（阻塞，等待批准）。
	AskCommand AskType = "command"
	// AskCommandOutput 命令执行中，输出流（非阻塞）。
	AskCommandOutput AskType = "command_output"
	// AskAPIReqFailed API 请求失败，询问重试（阻塞）。
	AskAPIReqFailed AskType = "api_req_failed"
	// AskMistakeLimit 连续错误达到上限（阻塞）。
	AskMistakeLimit AskType = "mistake_limit_reached"
	// AskAutoApprovalMax 自动审批请求数达到上限（阻塞）。
	AskAutoApprovalMax AskType = "auto_approval_max_req_reached"
	// AskResumeTask 任务暂停，询问恢复（阻塞）。
	AskResumeTask AskType = "resume_task"
	// AskCompletionResult 任务完成（阻塞，等待新任务或反馈）。
	AskCompletionResult AskType = "completion_result"
)

// SayType say 消息细分类型（对齐 Zoo-Code ClineSay）。
type SayType string

const (
	SayText                        SayType = "text"             // LLM 思考/说明文本
	SayReasoning                   SayType = "reasoning"        // 推理模型思考内容（对齐 Zoo-Code ClineSay "reasoning"，partial 流式）
	SayAPIReqStarted               SayType = "api_req_started"  // API 请求开始（text 为 JSON 载荷）
	SayAPIReqFinished              SayType = "api_req_finished" // API 请求结束
	SayAPIReqRetried               SayType = "api_req_retried"  // API 请求重试
	SayError                       SayType = "error"            // 错误信息
	SayTool                        SayType = "tool"             // 工具调用展示（text 为 JSON 载荷）
	SayUserFeedback                SayType = "user_feedback"    // 用户反馈回显
	SayShellIntegrationStarted     SayType = "shell_integration_started"
	SayCompletedWithCheckoutErrors SayType = "completed_with_checkout_errors"
	// P2 编排事件（对齐 Zoo-Code new_task / switch_mode 消息）。
	SaySubtaskStarted  SayType = "new_task_started"  // 子任务委派开始（text: 说明）
	SaySubtaskFinished SayType = "new_task_finished" // 子任务结束（text: 结果摘要）
	SayModeSwitched    SayType = "mode_switched"     // 模式切换完成（text: 新模式）
	// P2.7-0b AI 交互上下文快照：每次调用 LLM 前发送（text 为 apiMessages
	// JSON 数组）。仅经事件桥推送前端「查看 AI 上下文」，不落会话历史。
	SayAIContext SayType = "ai_context"
	// P3/B5 上下文治理：滑动窗口截断触发通知（text: 说明）。
	SayContextTruncated SayType = "context_truncated"
	// Codex 对齐补全：LLM 摘要压缩主路径触发通知（text: 说明）。
	SayContextCondensed SayType = "context_condensed"
	// Codex 对齐补全：危险命令检测强制审批通知（text: 命令与原因）。
	SayDangerBlocked SayType = "danger_blocked"
	// 全AI自动决策（用户需求 2026-10）：ask_followup_question 在自动决策模式下
	// 不阻塞等用户，问题以本类型留痕（text: 问题原文），前端展示为提示卡。
	SayAutoDecision SayType = "auto_decision"
	// Codex 对齐补全：本轮文件变更 net diff 摘要（text: 摘要文本）。
	SayTurnDiff SayType = "turn_diff"
	// 聊天内生图（generate_image）：图片卡片消息（Images 为 data URL 数组，
	// text 为空；base64 不入 LLM 上下文，仅聊天流展示 + 会话历史）。
	SayImage SayType = "image"
	// 长任务心跳（2026-10 复盘 R2：用户反复提交 = 「看不到进展」）：每 N 轮
	// LLM 请求由引擎主动推送的进度提示（text: 一句话进度摘要）。仅经事件桥
	// 推送前端展示，不落会话历史（语义对齐 ai_context / usage 的旁路帧）。
	SayProgress SayType = "progress"
	// 媒体生成（generate_video / generate_speech）：视频/音频卡片消息。
	// Images 复用为 data URL 数组（音频实时直播；视频体积大留空），
	// text 为 MediaPayload JSON 载荷（path/fileName/mime/kind，历史落盘）。
	SayVideo SayType = "video"
	SayAudio SayType = "audio"
	// Claude Code /cost 对齐：会话级 token/cost 累计推送（text 为 usage.Totals
	// JSON；仅实时推送前端展示，不落会话历史，语义对齐 ai_context）。
	SayUsage SayType = "usage"
	// 任务结束会话总结（会话 hook 产出）：text 为任务总结摘要正文（Markdown），
	// 供前端在聊天流尾部渲染「会话总结」卡片（与 turn_diff 类似，非模型输出）。
	SayTaskSummary SayType = "task_summary"
	// 对标增强 B1（对标 Cline Checkpoints）：任务收尾检查点摘要（text 为
	// 快照统计文本，前端可展示「本任务改了哪些文件、可回滚」卡片）。
	SayCheckpoint SayType = "checkpoint"
	// 对标增强 C1（对标 Aider auto-lint）：写类工具后检查失败反馈摘要
	// （text 为检查未通过说明；工具结果已同步回灌模型，本消息仅前端展示）。
	SayVerify SayType = "verify"
	// 对标增强 C3（对标 Aider auto-commits）：任务收尾 git 自动提交结果
	// （text 为 commit hash + 文件数；失败时为失败原因）。
	SayGitAutocommit SayType = "git_autocommit"
	// 对标增强 D1（对标 Claude Code /rewind）：检查点回滚完成通知（text 为
	// 「已回滚 <path>（安全快照 <id>）」；前端展示用，非模型输出）。
	SayCheckpointFrontend SayType = "checkpoint_frontend"
	// 对标增强 E1（第四批，对标 SWE-agent cost_limit）：成本/调用护栏触发通知
	// （text 为触发类别与上限；前端展示用，非模型输出）。
	SayCostLimit SayType = "cost_limit"
	// 对标增强 E3（对标 Plandex errorFallback）：主模型失败自动降级备用模型通知
	// （text 为降级说明；前端展示用，非模型输出）。
	SayFallback SayType = "fallback"
)

// AskResponse 用户对 ask 消息的应答（对齐 Zoo-Code ClineAskResponse）。
type AskResponse string

const (
	// ResponseYes 批准（yesButtonClicked）。
	ResponseYes AskResponse = "yesButtonClicked"
	// ResponseNo 拒绝（noButtonClicked）。
	ResponseNo AskResponse = "noButtonClicked"
	// ResponseMessageResponse 以文本方式回答（messageResponse）。
	ResponseMessageResponse AskResponse = "messageResponse"
)

// ClineMessage 内核统一事件（对齐 Zoo-Code ClineMessage）。
type ClineMessage struct {
	TS      int64       `json:"ts"`                // 唯一时间戳标识
	Type    MessageType `json:"type"`              // ask | say
	Ask     AskType     `json:"ask,omitempty"`     // Type==ask 时有效
	Say     SayType     `json:"say,omitempty"`     // Type==say 时有效
	Text    string      `json:"text,omitempty"`    // 内容：纯文本或 JSON 载荷
	Partial bool        `json:"partial,omitempty"` // 流式进行中标记
	Images  []string    `json:"images,omitempty"`  // 附件（data URL / 路径）
}

// NewSay 构造 say 消息。
func NewSay(say SayType, text string) ClineMessage {
	return ClineMessage{TS: time.Now().UnixNano(), Type: TypeSay, Say: say, Text: text}
}

// NewAsk 构造非 partial 的 ask 消息（将阻塞 Agent）。
func NewAsk(ask AskType, text string) ClineMessage {
	return ClineMessage{TS: time.Now().UnixNano(), Type: TypeAsk, Ask: ask, Text: text}
}

// APIReqInfo api_req_started 消息的 JSON 载荷（对齐 ClineApiReqInfo）。
// cost 为空表示请求仍在进行中（流式检测依据，见 AGENT_LOOP.md isStreaming）。
type APIReqInfo struct {
	APIProtocol string   `json:"apiProtocol"`
	Request     string   `json:"request,omitempty"`
	TokensIn    int      `json:"tokensIn,omitempty"`
	TokensOut   int      `json:"tokensOut,omitempty"`
	Cost        *float64 `json:"cost,omitempty"`
	DurationMS  int64    `json:"durationMS,omitempty"`
}

// ToolCallInfo say tool / ask tool 消息的 JSON 载荷。
type ToolCallInfo struct {
	Tool   string `json:"tool"`
	Path   string `json:"path,omitempty"`
	Diff   string `json:"diff,omitempty"`
	Source string `json:"source,omitempty"`
	Query  string `json:"query,omitempty"`
	Regex  string `json:"regex,omitempty"`
	Cmd    string `json:"command,omitempty"`
	// Args 原始参数 JSON（桌面端渲染 diff/content/todos 详情用）。
	Args string `json:"args,omitempty"`
}
