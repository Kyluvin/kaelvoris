// Package condense 上下文治理（P3/B5）：移植自 Zoo-Code
// 参考/Zoo-Code-3.82.2/src/core/context-management/index.ts 的滑动窗口截断
// （truncateConversation）与 token 估算兜底档。
//
// 与 Zoo-Code 的实现差异（Go 侧约束所致）：
//   - llm.Message 是严格 OpenAI wire 形状（自定义 MarshalJSON，vLLM/Pydantic
//     严格校验拒绝未知字段，见 BUG-1），truncationParent / isTruncationMarker
//     元数据不能写入消息结构。截断簿记改为 Agent 层的平行 hidden []bool，
//     截断标记消息以内容前缀（TruncationMarkerPrefix）识别；
//   - Zoo-Code（Anthropic 协议）以「消息对」为粒度做 even-rounding；OpenAI
//     协议下 assistant(tool_calls) 与其后所有 role=tool 消息构成原子组，
//     截断以「消息组」为单位，天然保证不产生孤儿 tool 消息（严格校验的
//     服务端会拒绝无前置 assistant 的 tool 消息）；
//   - LLM 摘要压缩主路径（SummarizeConversation，对齐 codex-rs compact.rs
//     auto-compact 与 Zoo-Code summarizeConversation）已补齐：超阈值时优先
//     以 LLM 摘要替换旧历史（摘要以标记消息注入保留边界），LLM 失败时
//     回退滑动窗口截断（TruncateConversation）。
//   - 摘要依赖以窄回调 Summarizer 注入（避免 condense → agent 反向依赖；
//     引擎侧用 llm.Client.Generate 适配）。
//
// 非破坏性语义：截断不删除消息数据，仅标记隐藏；完整 apiMessages 始终保留
// 在引擎侧，VisibleView 输出实际发送给 API 的可见视图。
package condense

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"kaelvoris/internal/llm"
)

// DefaultFrac 默认截断比例（对齐 Zoo-Code manageContext 兜底 0.5）。
const DefaultFrac = 0.5

// TruncationMarkerPrefix 截断标记消息的内容前缀。
// llm.Message 无元数据字段，标记消息以该前缀识别（isTruncationMarker 等价物）。
const TruncationMarkerPrefix = "[Sliding window truncation:"

// TruncationResult 截断结果（对齐 Zoo-Code TruncationResult）。
type TruncationResult struct {
	Messages        []llm.Message // 截断后的完整消息数组（含插入的标记消息）
	Hidden          []bool        // 与 Messages 平行的隐藏标记（true = 不发送）
	TruncationID    string        // 本次截断 ID（对齐 truncationId）
	MessagesRemoved int           // 本次新隐藏的消息条数
}

// IsMarker 判断消息是否为截断/摘要标记（前缀族识别：两类标记消息均不参与
// 截断分组计数，永久保留可见，对齐 Zoo-Code isTruncationMarker 语义）。
func IsMarker(m llm.Message) bool {
	if m.Role != llm.RoleUser {
		return false
	}
	return strings.HasPrefix(m.Content, TruncationMarkerPrefix) ||
		strings.HasPrefix(m.Content, SummarizeMarkerPrefix)
}

// TruncateConversation 滑动窗口截断：将最旧的一批可见消息标记为隐藏，
// 并在保留边界插入 user 角色标记消息（移植 Zoo-Code truncateConversation）。
//
// 语义（对齐 Zoo-Code，frac 缺省 0.5）：
//   - system 提示与首条用户任务消息恒保留（skip-first-visible 的 Go 适配：
//     kaelvoris 的 apiMessages[0] 为 system，任务消息紧随其后）；
//   - 隐藏以「消息组」为单位（assistant + 其全部 tool 结果整组隐藏），
//     替代 Zoo-Code even-rounding 的成对保证；
//   - 已隐藏消息与历史标记保持不动（非破坏性，数据不被删除）；
//   - 无可隐藏消息时原样返回（MessagesRemoved = 0）。
func TruncateConversation(msgs []llm.Message, hidden []bool, frac float64) TruncationResult {
	if frac <= 0 || frac >= 1 {
		frac = DefaultFrac
	}
	id := newTruncationID()
	hidden = padHidden(hidden, len(msgs))

	groups := visibleGroups(msgs, hidden)
	if len(groups) == 0 {
		return TruncationResult{Messages: msgs, Hidden: hidden, TruncationID: id}
	}

	// 头部保护：groups[0] = system 提示；groups[1] = 首条用户任务消息。
	// （对齐 Zoo-Code skip first visible 语义：首条可见消息永远保留。）
	keepHead := 1
	if msgs[groups[0][0]].Role == llm.RoleSystem && len(groups) > 1 {
		keepHead = 2
	}
	if len(groups) <= keepHead {
		return TruncationResult{Messages: msgs, Hidden: hidden, TruncationID: id}
	}

	remove := int(float64(len(groups)-keepHead) * frac)
	if remove <= 0 {
		return TruncationResult{Messages: msgs, Hidden: hidden, TruncationID: id}
	}

	// 标记隐藏（非破坏性：仅置 hidden 标记，不修改消息数据）。
	removedCount := 0
	for _, g := range groups[keepHead : keepHead+remove] {
		removedCount += len(g)
		for _, idx := range g {
			hidden[idx] = true
		}
	}

	// 标记消息插入保留边界：最后一条被隐藏消息之后、第一条保留消息之前
	// （对齐 Zoo-Code 的 marker ts = firstKeptTs - 1 的位置语义；Go 侧按
	// 数组位置插入即可）。全部可隐藏消息被隐藏时插入末尾。
	insertAt := len(msgs)
	if keepHead+remove < len(groups) {
		insertAt = groups[keepHead+remove][0]
	}
	marker := llm.Message{
		Role: llm.RoleUser,
		Content: fmt.Sprintf("%s %d messages hidden to reduce context]",
			TruncationMarkerPrefix, removedCount),
	}

	out := make([]llm.Message, 0, len(msgs)+1)
	out = append(out, msgs[:insertAt]...)
	out = append(out, marker)
	out = append(out, msgs[insertAt:]...)

	outHidden := make([]bool, 0, len(msgs)+1)
	outHidden = append(outHidden, hidden[:insertAt]...)
	outHidden = append(outHidden, false) // 标记消息始终可见
	outHidden = append(outHidden, hidden[insertAt:]...)

	return TruncationResult{
		Messages:        out,
		Hidden:          outHidden,
		TruncationID:    id,
		MessagesRemoved: removedCount,
	}
}

// VisibleView 输出实际发送给 API 的可见视图：过滤已隐藏消息。
// 无隐藏消息时原样返回（零拷贝）；截断簿记不进入 wire 结构。
func VisibleView(msgs []llm.Message, hidden []bool) []llm.Message {
	h := padHidden(hidden, len(msgs))
	any := false
	for _, v := range h {
		if v {
			any = true
			break
		}
	}
	if !any {
		return msgs
	}
	out := make([]llm.Message, 0, len(msgs))
	for i, m := range msgs {
		if !h[i] {
			out = append(out, m)
		}
	}
	return out
}

// EstimateTokens 启发式 token 估算（无 tokenizer 依赖，量级兜底档）：
// CJK/全角字符 ≈ 1 token/字，其余 ≈ 4 字符/token 向上取整；
// 图片 ≈ 800 token/张；每条消息固定 4 token 结构开销；tool_call 8 token。
// 用于请求前的上下文占比判断（Zoo-Code 使用真实 usage，这里以估算替代）。
func EstimateTokens(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += 4
		total += EstimateText(m.Content)
		total += 800 * len(m.Images)
		for _, tc := range m.ToolCalls {
			total += 8 + EstimateText(tc.Arguments)
		}
	}
	return total
}

// EstimateText 启发式文本 token 估算：CJK/全角字符 ≈ 1 token/字，
// 其余 ≈ 4 字符/token 向上取整。
func EstimateText(s string) int {
	if s == "" {
		return 0
	}
	ascii, wide := 0, 0
	for _, r := range s {
		if r >= 0x2E80 { // CJK 部首起始：中日韩/全角/Emoji 一律按宽字符
			wide++
		} else {
			ascii++
		}
	}
	return wide + (ascii+3)/4
}

// visibleGroups 将可见消息分区为原子组：assistant 消息与其后随的全部
// role=tool 结果构成一组（OpenAI 协议要求 tool 消息紧跟发起调用的
// assistant；整组隐藏保证 wire 合法性）。截断标记消息不属于任何组
// （对齐 Zoo-Code：isTruncationMarker 不参与截断计数，永久保留）。
func visibleGroups(msgs []llm.Message, hidden []bool) [][]int {
	var groups [][]int
	var cur []int
	for i := range msgs {
		if hidden[i] || IsMarker(msgs[i]) {
			continue
		}
		if len(cur) > 0 && msgs[i].Role != llm.RoleTool {
			groups = append(groups, cur)
			cur = nil
		}
		cur = append(cur, i)
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// padHidden 将 hidden 补齐到 n 长（缺省可见）。
func padHidden(hidden []bool, n int) []bool {
	if len(hidden) >= n {
		return hidden
	}
	out := make([]bool, n)
	copy(out, hidden)
	return out
}

// newTruncationID 生成截断 ID（crypto/rand 十六进制）。
func newTruncationID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "trunc-unknown"
	}
	return "trunc-" + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// LLM 摘要压缩主路径（对齐 codex-rs core/src/compact.rs 的 auto-compact 与
// Zoo-Code summarizeConversation）：超阈值时不再粗暴隐藏旧消息，而是把
// 「待压缩区」交给 LLM 生成结构化摘要，以摘要替换旧历史——保留任务目标、
// 关键决策、文件路径与未完成事项，损失远小于滑动窗口截断。
// ---------------------------------------------------------------------------

// Summarizer 摘要生成回调：输入待压缩历史的可见文本视图，返回摘要文本。
// 以窄接口注入（引擎侧用 llm.Client 适配），condense 包不直接依赖 LLM 客户端，
// 便于测试桩注入（对齐 codex compact 的 ModelClient 依赖反转）。
type Summarizer func(ctx context.Context, toSummarize []llm.Message) (string, error)

// SummarizeResult 摘要压缩结果。
type SummarizeResult struct {
	Messages        []llm.Message // 摘要后的完整消息数组（含摘要标记消息）
	Hidden          []bool        // 与 Messages 平行的隐藏标记
	Condensed       bool          // 是否执行了摘要压缩
	MessagesRemoved int           // 被摘要替换（隐藏）的消息条数
	SummaryText     string        // LLM 生成的摘要正文
	KeptRecent      int           // 保留原文的最近消息条数（keepRecent 语义；近期细节零损失）
}

// SummarizeMarkerPrefix 摘要标记消息的内容前缀（以标记消息承载摘要，
// IsMarker 语义扩展：前缀族识别，避免占用 wire 元数据字段）。
const SummarizeMarkerPrefix = "[Conversation summarized:"

// DefaultSummarizePrompt 摘要生成指令。对齐 Claude Code /compact 的结构化
// 8 段协议（8-section compact summary）：每段标题原样保留，逐段填写——
// 相比笼统「总结对话」，结构化模板强制覆盖任务目标/用户消息全量/文件与
// 错误/待办/下一步；二次压缩与续聊恢复均依赖段落标题锚点提取（prevSummary
// 合并语义见 SummarizeConversationKeep）。
// 用户可在设置面板「提示词」配置 summarize 段覆盖（空 = 使用内置默认）。
const DefaultSummarizePrompt = `You are a conversation summarizer. Summarize the conversation above as a ` +
	`structured context record using EXACTLY the following section headers (each on its own line). ` +
	`Replace each section's placeholder comment with content; keep file paths, IDs, commands and ` +
	`error messages verbatim. Be concise but information-dense; omit pleasantries and raw tool dumps.

` +
	`## Primary Request and Intent
` +
	`<!-- The user's full task statement, all explicit requirements and constraints (quote key wording verbatim). -->

` +
	`## Key Technical Concepts
` +
	`<!-- Concepts, commands, APIs, libraries, conventions and patterns established or requested. -->

` +
	`## Files and Code Sections
` +
	`<!-- Every file read, written or edited, with full path and a one-line note of its purpose / what was changed. Preserve code structure for future edits. -->

` +
	`## Errors and fixes
` +
	`<!-- All errors encountered (build/test/runtime) and how they were fixed. List even unresolved ones explicitly. -->

` +
	`## Problem Solving
` +
	`<!-- Problem-solving methods, root causes identified, trade-off decisions and their rationale. -->

` +
	`## All User Messages
` +
	`<!-- ALL user (non-tool-result) messages, summarized chronologically in a numbered list. Never drop one. -->

` +
	`## Pending Tasks
` +
	`<!-- Incomplete work: pending / in_progress todo items and explicitly deferred items; empty = write "None". -->

` +
	`## Current Work
` +
	`<!-- What was being done immediately before this summary, with concrete state (file/step/test status). -->

` +
	`## Next Step
` +
	`<!-- The single concrete next action, tied to the most recent user message and Current Work; empty = write "None". -->`

// IsSummarizeMarker 判断消息是否为摘要标记消息。
func IsSummarizeMarker(m llm.Message) bool {
	return m.Role == llm.RoleUser && strings.HasPrefix(m.Content, SummarizeMarkerPrefix)
}

// MarkerSummary 从摘要标记消息中提取摘要正文（前缀行与收尾 "]" 之外的部分）。
// 非摘要标记消息返回空串。供手动 /compact 与引擎层把历史摘要回传给 LLM 合并。
func MarkerSummary(m llm.Message) string {
	if !IsSummarizeMarker(m) {
		return ""
	}
	c := m.Content
	// 跳过首行（"[Conversation summarized: N messages condensed. Previous context summary:"）。
	if i := strings.Index(c, "\n"); i >= 0 {
		c = c[i+1:]
	} else {
		return ""
	}
	c = strings.TrimSpace(c)
	// 去掉末尾的闭合 "]"（marker 以 "]" 收尾；摘要正文含 "" 时取最后一个）。
	if strings.HasSuffix(c, "]") {
		c = strings.TrimRight(c, "]")
	}
	return strings.TrimSpace(c)
}

// SummarizeConversation LLM 摘要压缩主路径（对齐 codex compact.rs auto inline
// compact：EstimateTokens 超阈值 → 摘要替换旧历史 → 摘要以标记消息注入保留边界）。
//
// 语义（Zoo-Code summarizeConversation + codex compact 融合）：
//   - 头部保护：system 提示与首条用户任务消息恒保留（visibleGroups[0..keepHead)）；
//   - 保留最近 keepRecent 组原文（规划板 2026-10 需求：摘要仅覆盖较早历史，
//     最近 N 条消息保持原文，近期细节零损失；keepRecent <= 0 退化为旧行为——
//     全部非保护组均进入待压缩区）；
//   - 待压缩区 = 头部保护之后、最近 keepRecent 组之前的全部可见消息；已被
//     滑动窗口隐藏的消息不参与；
//   - 摘要成功：待压缩区整体隐藏（非破坏性），在保留边界插入 user 角色
//     摘要标记消息（SummarizeMarkerPrefix + LLM 摘要正文）；
//   - summarizer 为 nil / 返回空摘要或错误：原样返回 Condensed=false，
//     不修改任何状态（调用方回退 TruncateConversation）；
//   - 可压缩区不足（只有头部保护消息）：原样返回。
func SummarizeConversation(msgs []llm.Message, hidden []bool, summarizer Summarizer) SummarizeResult {
	return SummarizeConversationKeep(msgs, hidden, summarizer, 0)
}

// SummarizeConversationKeep SummarizeConversation 的 keepRecent 扩展版：
// 摘要只替换较早历史，最近 keepRecent 个可见组原文保留（近期上下文不动）。
//
// 旧摘要合并（prevSummary，Claude Code /compact 二次压缩对齐）：长会话中
// 摘要压缩可能触发多次。若不合并，第二次压缩仅基于「上次压缩后的增量」
// 生成摘要，第一次压缩的要点（任务目标/早期文件/错误）随旧 marker 一起
// 被隐藏 → 摘要信息单调衰减，续聊必丢早期记忆。prevSummary 非空时把
// 上一次摘要正文作为附加消息置于待压缩区末尾交给 LLM，由模型输出合并后
// 的完整摘要（结构化 8 段协议天然支持同段合并更新）。
func SummarizeConversationKeep(msgs []llm.Message, hidden []bool, summarizer Summarizer, keepRecent int) SummarizeResult {
	return SummarizeConversationKeepPrev(msgs, hidden, summarizer, keepRecent, "")
}

// SummarizeConversationKeepPrev 同 SummarizeConversationKeep，另支持传入
// 上一次压缩的摘要正文（prevSummary）合并重摘——引擎层从会话存储
// CondenseRecord.Summary 取值传入，手动 /compact 亦复用同一入口。
func SummarizeConversationKeepPrev(msgs []llm.Message, hidden []bool, summarizer Summarizer, keepRecent int, prevSummary string) SummarizeResult {
	hidden = padHidden(hidden, len(msgs))

	fail := SummarizeResult{Messages: msgs, Hidden: hidden}
	if summarizer == nil {
		return fail
	}

	groups := visibleGroups(msgs, hidden)
	if len(groups) == 0 {
		return fail
	}

	// 头部保护与 TruncateConversation 一致：system + 首条任务消息。
	keepHead := 1
	if msgs[groups[0][0]].Role == llm.RoleSystem && len(groups) > 1 {
		keepHead = 2
	}
	if len(groups) <= keepHead {
		return fail
	}

	// 待压缩区 = 头部保护之后、最近 keepRecent 组之前的全部可见组
	//（end 为待压缩区右开边界：倒数第 keepRecent 组的起始索引）。
	sumEnd := len(groups) - keepRecent
	if sumEnd < keepHead {
		sumEnd = keepHead
	}
	if sumEnd <= keepHead {
		return fail
	}

	// 待压缩区 = 头部保护之后、保留区之前的可见消息（groups[keepHead:sumEnd]）。
	var toSummarize []llm.Message
	condenseIdx := make([]int, 0, len(msgs))
	for _, g := range groups[keepHead:sumEnd] {
		condenseIdx = append(condenseIdx, g...)
		for _, idx := range g {
			toSummarize = append(toSummarize, msgs[idx])
		}
	}
	if len(toSummarize) == 0 {
		return fail
	}

	// 旧摘要合并：prevSummary 非空时作为附加消息置于待压缩区末尾（见函数注释），
	// LLM 据此输出「合并后的完整摘要」而非仅增量；空时行为与旧版完全一致。
	if p := strings.TrimSpace(prevSummary); p != "" {
		toSummarize = append(toSummarize, llm.Message{
			Role:    llm.RoleUser,
			Content: "[Previous context summary from an earlier compaction — merge its still-relevant content into the new summary:]\n\n" + p,
		})
	}

	summary, err := summarizer(context.Background(), toSummarize)
	if err != nil || strings.TrimSpace(summary) == "" {
		return fail // 调用方回退滑动窗口截断
	}

	// 保留原文的最近消息计数（kept = 保留区全部可见消息条数）。
	kept := 0
	for _, g := range groups[sumEnd:] {
		kept += len(g)
	}

	// 待压缩区整体隐藏（非破坏性），摘要以 user 标记消息插入保留边界。
	removed := 0
	for _, idx := range condenseIdx {
		hidden[idx] = true
		removed++
	}

	insertAt := len(msgs)
	if sumEnd < len(groups) {
		insertAt = groups[sumEnd][0]
	}
	marker := llm.Message{
		Role: llm.RoleUser,
		Content: fmt.Sprintf("%s %d messages condensed. Previous context summary:\n\n%s]",
			SummarizeMarkerPrefix, removed, summary),
	}

	out := make([]llm.Message, 0, len(msgs)+1)
	out = append(out, msgs[:insertAt]...)
	out = append(out, marker)
	out = append(out, msgs[insertAt:]...)

	outHidden := make([]bool, 0, len(msgs)+1)
	outHidden = append(outHidden, hidden[:insertAt]...)
	outHidden = append(outHidden, false) // 摘要标记消息始终可见
	outHidden = append(outHidden, hidden[insertAt:]...)

	return SummarizeResult{
		Messages:        out,
		Hidden:          outHidden,
		Condensed:       true,
		MessagesRemoved: removed,
		SummaryText:     summary,
		KeptRecent:      kept,
	}
}

// LLMClientSummarizer 将 llm.Client 适配为 Summarizer（引擎侧注入）：
// 非流式单次请求，复用主模型（对齐 codex compact 复用主模型生成摘要）。
func LLMClientSummarizer(client *llm.Client) Summarizer {
	return LLMClientSummarizerWithPrompt(client, "")
}

// LLMClientSummarizerWithPrompt 同上，但允许用户自定义摘要指令（设置面板
// 「提示词」summarize 段；prompt 为空回退内置 DefaultSummarizePrompt）。
func LLMClientSummarizerWithPrompt(client *llm.Client, prompt string) Summarizer {
	if strings.TrimSpace(prompt) == "" {
		prompt = DefaultSummarizePrompt
	}
	return func(ctx context.Context, toSummarize []llm.Message) (string, error) {
		reqMsgs := make([]llm.Message, 0, len(toSummarize)+2)
		reqMsgs = append(reqMsgs, llm.Message{Role: llm.RoleSystem, Content: prompt})
		reqMsgs = append(reqMsgs, toSummarize...)
		reqMsgs = append(reqMsgs, llm.Message{Role: llm.RoleUser, Content: prompt})
		resp, err := client.Generate(ctx, llm.ChatRequest{Messages: reqMsgs})
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}
