// governor.go —— 上下文治理策略链（策略模式收敛，2026-12 优化）：
// 把引擎主循环内联的三层压缩逻辑（LLM 摘要 → 滑动窗口截断 → 硬裁剪）
// 抽为可独立替换的策略（Policy），Governor 按注册顺序执行；每个策略自行
// 判断是否需要执行（估算已在预算内即 no-op），逐步把载荷压回预算。
//
// 与原内联实现的行为对齐说明：
//   - 三层顺序、预算判定、非破坏性 hidden 语义逐字保留；
//   - 差异（可接受）：原实现「摘要成功后即使仍超预算也跳过滑动窗口、直达
//     硬裁剪」；策略链下摘要成功但仍超预算时会先试滑动窗口（隐藏旧组）再
//     硬裁剪兜底——两者最终都由硬裁剪保证预算内，端到端测试未锚定该组合。
//   - say 文案不在本包生成：引擎层按 StepResult 映射为 context_condensed /
//     context_truncated 事件（保持既有文案与断言兼容）。
package condense

import "kaelvoris/internal/llm"

// StepKind 策略类别（引擎层据此生成 say 文案与回调）。
type StepKind string

const (
	StepSummarize StepKind = "summarize" // LLM 摘要压缩主路径
	StepTruncate  StepKind = "truncate"  // 滑动窗口截断
	StepHardTrim  StepKind = "hard_trim" // 强制裁剪兜底
)

// StepStatus 单步执行状态。
type StepStatus string

const (
	StepSkipped StepStatus = "skipped" // 估算已在预算内 / 前置条件不满足
	StepApplied StepStatus = "applied" // 已执行并产生压缩效果
	StepFailed  StepStatus = "failed"  // 执行失败（仅摘要：LLM 失败/空摘要）
)

// StepResult 单步执行结果（Messages/Hidden 仅在 Applied 时携带新视图）。
type StepResult struct {
	Kind     StepKind
	Status   StepStatus
	Messages []llm.Message
	Hidden   []bool
	Removed  int    // 摘要替换 / 截断隐藏的消息条数（截断为多轮累计）
	Kept     int    // 摘要保留的最近原文条数（仅 summarize）
	Summary  string // 摘要正文（仅 summarize）
	Passes   int    // 截断执行轮数（仅 truncate）
}

// Policy 上下文治理策略（策略模式）：单个把估算压回预算的步骤。
type Policy interface {
	// Kind 策略类别。
	Kind() StepKind
	// Apply 执行策略。实现约定：估算已在预算内时原样返回（Status=skipped，
	// Messages/Hidden 原样透传），不产生副作用。
	Apply(msgs []llm.Message, hidden []bool, budget int) StepResult
}

// Governor 策略链执行器：按注册顺序执行策略，前一步的输出作为后一步的输入。
type Governor struct {
	policies []Policy
}

// NewGovernor 组装策略链（顺序即执行顺序）。
func NewGovernor(policies ...Policy) *Governor {
	return &Governor{policies: policies}
}

// Run 依次执行策略，返回每步结果（含 skipped）；Applied 步骤的视图向后传递。
func (g *Governor) Run(msgs []llm.Message, hidden []bool, budget int) []StepResult {
	outs := make([]StepResult, 0, len(g.policies))
	for _, p := range g.policies {
		r := p.Apply(msgs, hidden, budget)
		r.Kind = p.Kind()
		outs = append(outs, r)
		if r.Status == StepApplied {
			msgs, hidden = r.Messages, r.Hidden
		}
	}
	return outs
}

// SummarizePolicy LLM 摘要压缩策略（codex compact.rs 语义）：把待压缩区交给
// Summarizer 生成结构化摘要替换旧历史。失败（nil/错误/空摘要）返回 failed，
// 由引擎计 condenseFails 后靠后续策略兜底。
type SummarizePolicy struct {
	Summarizer Summarizer
	KeepRecent int // 摘要保留最近 N 条原文（0 = 全部可入摘要）
}

// Kind 实现 Policy。
func (p *SummarizePolicy) Kind() StepKind { return StepSummarize }

// Apply 实现 Policy。
func (p *SummarizePolicy) Apply(msgs []llm.Message, hidden []bool, budget int) StepResult {
	if p.Summarizer == nil || EstimateTokens(msgs) <= budget {
		return StepResult{Kind: p.Kind(), Status: StepSkipped, Messages: msgs, Hidden: hidden}
	}
	res := SummarizeConversationKeep(msgs, hidden, p.Summarizer, p.KeepRecent)
	if !res.Condensed {
		return StepResult{Kind: p.Kind(), Status: StepFailed, Messages: msgs, Hidden: hidden}
	}
	return StepResult{
		Kind: p.Kind(), Status: StepApplied,
		Messages: res.Messages, Hidden: res.Hidden,
		Removed: res.MessagesRemoved, Kept: res.KeptRecent, Summary: res.SummaryText,
	}
}

// truncateMaxPasses 滑动窗口迭代上限（对齐原内联实现的 6 轮）。
const truncateMaxPasses = 6

// TruncatePolicy 滑动窗口截断策略：迭代隐藏最旧可见组直至预算内或无可隐藏，
// 第 2 轮起加大隐藏比例（0.8）加速收敛（413 修复语义）。
type TruncatePolicy struct {
	Frac float64 // 首轮隐藏比例（<=0 或 >=1 取 DefaultFrac）
}

// Kind 实现 Policy。
func (p *TruncatePolicy) Kind() StepKind { return StepTruncate }

// Apply 实现 Policy。
func (p *TruncatePolicy) Apply(msgs []llm.Message, hidden []bool, budget int) StepResult {
	out := StepResult{Kind: p.Kind(), Status: StepSkipped, Messages: msgs, Hidden: hidden}
	if EstimateTokens(msgs) <= budget {
		return out
	}
	frac := p.Frac
	if frac <= 0 || frac >= 1 {
		frac = DefaultFrac
	}
	for pass := 1; pass <= truncateMaxPasses; pass++ {
		if EstimateTokens(msgs) <= budget {
			break
		}
		res := TruncateConversation(msgs, hidden, frac)
		if res.MessagesRemoved == 0 {
			break // 仅剩头部保护消息，截断已到极限 → 交由硬裁剪兜底
		}
		out.Removed += res.MessagesRemoved
		out.Passes = pass
		msgs, hidden = res.Messages, res.Hidden
	}
	if out.Passes > 0 {
		out.Status = StepApplied
		out.Messages, out.Hidden = msgs, hidden
	}
	return out
}

// HardTrimPolicy 强制裁剪兜底策略（413 第三层）：摘要与滑动窗口都无法把
// 估算压入预算时，按预算比例就地裁剪残留超大消息（hardTrimVisible）。
type HardTrimPolicy struct{}

// Kind 实现 Policy。
func (p *HardTrimPolicy) Kind() StepKind { return StepHardTrim }

// Apply 实现 Policy。
func (p *HardTrimPolicy) Apply(msgs []llm.Message, hidden []bool, budget int) StepResult {
	if EstimateTokens(msgs) <= budget {
		return StepResult{Kind: p.Kind(), Status: StepSkipped, Messages: msgs, Hidden: hidden}
	}
	if hardTrimVisible(msgs, hidden, budget) {
		return StepResult{Kind: p.Kind(), Status: StepApplied, Messages: msgs, Hidden: hidden}
	}
	return StepResult{Kind: p.Kind(), Status: StepSkipped, Messages: msgs, Hidden: hidden}
}
