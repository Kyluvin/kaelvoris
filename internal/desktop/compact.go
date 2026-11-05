// compact.go —— 手动会话压缩（/compact，对标 Claude Code /compact +
// OpenCode compaction agent 手动触发语义）。
//
// 背景：自动压缩仅治「单任务内上下文超限」；跨轮长会话（用户连续数日续聊）
// 的历史逐轮累积进 ResumeDigest/最近消息，恢复质量随长度衰减。用户需要
// 「主动压缩一次会话」的能力：把全部历史提炼为结构化摘要并落盘，之后的
// 续聊以摘要 + 新消息为基线（对齐 Claude Code /compact 的"conversation
// is summarized and future messages build on the summary"）。
//
// 与自动压缩的差异：
//   - 不需要引擎运行态（可对历史会话离线执行）；
//   - 输入源 = 会话持久化 Messages（而非引擎 apiMessages）；
//   - 旧摘要合并：CondenseRecord 已有摘要时经 SummarizeConversationKeepPrev
//     合并重摘（修自动压缩二次触发摘要衰减的同一语义）；
//   - 产物 = 新 CondenseRecord（摘要正文 + 时间），前端聊天流推
//     context_condensed 卡片，续聊经 ResumeDigest 注入完整摘要。
//
// 边界：空会话/无有效对话返回错误（不空转 LLM）；摘要失败上抛错误由
// Wails 绑定层转前端提示（手动操作需明确结果，区别于后台静默语义）。
package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"kaelvoris/internal/condense"
	"kaelvoris/internal/llm"
	"kaelvoris/internal/session"
	"kaelvoris/pkg/message"
)

// compactMessages 会话消息 → LLM 消息序列（压缩输入）。
// 语义对齐 engine RunInMode 的 apiMessages 构造：首条 user 消息 = 任务起点，
// user_feedback/助手文本/工具结果依次排列；跳过流式 partial 与旁路帧
// （api_req/ai_context/usage/progress——非对话内容，进摘要只会加噪）。
func compactMessages(msgs []message.ClineMessage) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Partial {
			continue
		}
		switch {
		case m.Type == message.TypeAsk:
			out = append(out, llm.Message{Role: llm.RoleUser, Content: m.Text})
		case m.Say == message.SayUserFeedback:
			out = append(out, llm.Message{Role: llm.RoleUser, Content: m.Text})
		case m.Say == message.SayText:
			if strings.TrimSpace(m.Text) != "" {
				out = append(out, llm.Message{Role: llm.RoleAssistant, Content: m.Text})
			}
		case m.Say == message.SayTool:
			var info message.ToolCallInfo
			if jsonUnmarshalToolInfo(m.Text, &info) {
				out = append(out, llm.Message{
					Role:    llm.RoleAssistant,
					Content: "[tool call] " + info.Tool + compactToolArg(info),
				})
			}
		case m.Say == message.SayError:
			out = append(out, llm.Message{Role: llm.RoleUser, Content: "[error] " + m.Text})
		case m.Say == message.SayContextCondensed:
			// 历史压缩通知：正文含上一轮摘要统计，保留为参考消息
			//（摘要正文本体由 prevSummary 传入，无需从通知提取）。
			out = append(out, llm.Message{Role: llm.RoleUser, Content: "[context condensed] " + m.Text})
		}
	}
	return out
}

// compactToolArg 工具调用消息的参数摘要（路径/命令等关键定位信息）。
func compactToolArg(info message.ToolCallInfo) string {
	switch {
	case info.Path != "":
		return " → " + info.Path
	case info.Cmd != "":
		return " → " + truncateRunes(info.Cmd, 120)
	case info.Query != "":
		return " → " + truncateRunes(info.Query, 80)
	}
	return ""
}

// jsonUnmarshalToolInfo 轻量包装（compact.go 内避免直接依赖 encoding/json 命名冲突）。
func jsonUnmarshalToolInfo(text string, out *message.ToolCallInfo) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	return json.Unmarshal([]byte(text), out) == nil
}

// Compact 手动压缩会话（Wails 绑定，前端聊天输入 /compact 触发）：
// 会话全部历史 → LLM 结构化摘要（8 段协议）→ CondenseRecord 落盘。
// 有旧摘要时自动合并（二次压缩不丢早期要点）。返回摘要正文供前端展示。
// 会话运行中拒绝（执行侧正在累积新消息，压缩结果即刻过期）。
func (a *App) Compact(sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", errors.New("缺少会话 ID")
	}
	if a.IsSessionRunning(sessionID) {
		return "", errors.New("会话运行中，请等待任务完成后再压缩")
	}
	sess, err := a.store.Get(sessionID)
	if err != nil || sess == nil {
		return "", errors.New("会话不存在")
	}
	snap := sess.Snapshot()
	if len(snap.Messages) == 0 {
		return "", errors.New("会话无历史消息，无需压缩")
	}
	msgs := compactMessages(snap.Messages)
	if len(msgs) == 0 {
		return "", errors.New("会话无有效对话内容，无需压缩")
	}

	// 辅助模型分流（E2 同源）：压缩属辅助环节，配置了辅助模型走便宜小模型。
	cfg := a.GetConfig()
	client := llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	summarizer := condense.LLMClientSummarizerWithPrompt(client, a.promptStore.Get("summarize"))

	// 旧摘要合并：CondenseRecord 已有摘要 → SummarizeConversationKeepPrev
	// 把旧摘要并入待压缩区（修二次压缩摘要衰减）。
	prev := ""
	if snap.Condense != nil && snap.Condense.Condensed {
		prev = snap.Condense.Summary
	}
	// keepRecent=0：手动压缩语义 = 全量提炼（保留近期原文由 ResumeDigest
	// 的最近消息窗口承担，此处不必保留，最大化压缩率）。
	// 头部无 system 消息（compactMessages 不产生 system），keepHead 自动退化 1。
	res := condense.SummarizeConversationKeepPrev(msgs, nil, summarizer, 0, prev)
	if !res.Condensed {
		return "", errors.New("压缩失败：LLM 未返回有效摘要，请稍后重试或检查模型配置")
	}

	sess.SetCondense(&session.CondenseRecord{
		Condensed:       true,
		RemovedMessages: res.MessagesRemoved,
		KeptMessages:    res.KeptRecent,
		Summary:         res.SummaryText,
		CondensedAt:     time.Now(),
	})
	a.store.Save(sess) // 异步刷盘

	// 聊天流推送压缩卡片（前端渲染 context_condensed；落会话历史留痕）。
	say := a.makeSayBridge(sess, nil, sessionID)
	say(message.NewSay(message.SayContextCondensed,
		fmt.Sprintf("已手动压缩会话：%d 条历史消息提炼为结构化摘要（%d 字），之后的对话将基于摘要继续",
			res.MessagesRemoved, len([]rune(res.SummaryText)))))

	slog.Info("手动压缩会话完成", "session", sessionID, "removed", res.MessagesRemoved, "summaryChars", len(res.SummaryText))
	return res.SummaryText, nil
}

// buildCompactDigest /compact 后续聊的恢复摘要增强源：压缩记录存在且
// 比最近一次消息更新时，摘要即会话基线（对齐 Claude Code compact 后
// "future messages build on the summary"——ResumeDigest 注入完整摘要 +
// 压缩时间之后的新消息，不再回放压缩前的旧对话）。
// 返回空串 = 无新鲜压缩记录（走既有 ResumeDigestFor 全量逻辑）。
func buildCompactDigest(snap session.SessionData, memoryDoc string) string {
	c := snap.Condense
	if c == nil || !c.Condensed || strings.TrimSpace(c.Summary) == "" {
		return ""
	}
	// 压缩时间晚于最后一条消息 = 压缩后无新对话：摘要即最新基线。
	var lastTS int64
	for i := len(snap.Messages) - 1; i >= 0; i-- {
		if !snap.Messages[i].Partial {
			lastTS = snap.Messages[i].TS
			break
		}
	}
	if lastTS > 0 && c.CondensedAt.UnixNano() < lastTS {
		return "" // 压缩后已有新对话：回到全量恢复逻辑（摘要段照常注入）
	}

	var b strings.Builder
	b.WriteString("---- 会话恢复上下文（本会话已被压缩，以下为压缩摘要，请基于此继续，不要重新开始） ----\n\n")
	if len(snap.Todos) > 0 {
		b.WriteString("[当前任务清单]\n")
		for _, t := range snap.Todos {
			mark := "[ ]"
			switch t.Status {
			case "completed":
				mark = "[x]"
			case "in_progress":
				mark = "[-]"
			}
			b.WriteString(mark + " " + t.Content + "\n")
		}
		b.WriteString("\n")
	}
	if memoryDoc != "" {
		b.WriteString("[会话记忆文档（MEMORY.md）]\n" + memoryDoc + "\n\n")
	}
	b.WriteString("[会话压缩摘要（全部早期对话已提炼为以下结构化记录）]\n")
	b.WriteString(c.Summary + "\n")
	b.WriteString("\n---- 恢复上下文结束 ----\n")
	return b.String()
}
