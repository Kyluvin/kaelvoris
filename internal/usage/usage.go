// Package usage 会话级 token/cost 计量（Claude Code /cost 与 /context 对齐补全）：
//
// 引擎在每轮 LLM 请求结束回填 include_usage 真实用量（streamUsage 尾包）时
// 调用 Record；Usage 汇总随会话持久化，并经 say usage 事件实时推送前端
// （顶栏/会话头部展示 tokens ↑↓ Σ 与请求数）。
//
// 估算兜底：服务端未回传 usage 尾包时按字符启发式估算，保证任何后端都有
// 可用计量（对齐「估算兜底」路径，估算量与 condense.EstimateText 同口径）。
package usage

import (
	"sync"

	"kaelvoris/internal/llm"
)

// Totals 累计用量快照（JSON 持久化友好）。
type Totals struct {
	Requests         int     `json:"requests"`           // LLM 请求次数
	PromptTokens     int64   `json:"prompt_tokens"`      // 输入 token 累计
	CompletionTokens int64   `json:"completion_tokens"`  // 输出 token 累计
	TotalTokens      int64   `json:"total_tokens"`       // 总 token（服务端口径累加）
	EstimatedTokens  int64   `json:"estimated_tokens"`   // 估算补充（usage 缺失请求的启发式估算）
	CostUSD          float64 `json:"cost_usd,omitempty"` // 估算成本（美元；0 = 未配置单价）
}

// Counter 会话级累计计量器（并发安全；每任务一个实例，随引擎生命周期）。
type Counter struct {
	mu sync.Mutex
	t  Totals
	// promptCost/completionCost 每百万 token 单价（美元；0 = 不计费仅计 token）。
	promptCost     float64
	completionCost float64
	// contextWindow 上下文窗口（token），注入 context 类展示；0 = 未知。
	contextWindow int
	// ---- 对标增强 E1（第四批，对标 SWE-agent cost_limit / per_instance_call_limit）----
	// costLimitUSD 单任务成本上限（美元；<=0 = 不限）。累计 CostUSD 超过该值
	// 时 OverLimit 返回超限标记（引擎据此优雅终止任务，已完成进度保留）。
	costLimitUSD float64
	// callLimit LLM 请求次数上限（<=0 = 不限）。累计 Requests 达到该值时
	// OverLimit 返回超限标记（与成本上限双轨，各自独立判定）。
	callLimit int
}

// New 创建计量器（无限额；向后兼容旧调用方）。
func New(promptCost, completionCost float64, contextWindow int) *Counter {
	return NewWithLimits(promptCost, completionCost, contextWindow, 0, 0)
}

// NewWithLimits 创建带护栏的计量器（E1，对标 mini-swe-agent AgentConfig：
// cost_limit 默认 3.0 超过即停、per_instance_call_limit 调用次数独立轨；
// 两者 <=0 = 不限）。宿主按 desktop.json costLimitUSD / 单任务配置注入。
func NewWithLimits(promptCost, completionCost float64, contextWindow int, costLimitUSD float64, callLimit int) *Counter {
	return &Counter{
		promptCost:     promptCost,
		completionCost: completionCost,
		contextWindow:  contextWindow,
		costLimitUSD:   costLimitUSD,
		callLimit:      callLimit,
	}
}

// OverLimit 判定当前累计是否超出预算（E1）：成本优先（CostUSD > 上限，
// 对齐 mini "Stop agent after exceeding (!) this cost" 超过才停），其次
// 调用次数（Requests >= 上限，第 N 次调用完成即达）。返回超限类别（"cost"
// / "calls"）、上限值与是否超限；未配置限额（<=0）恒不超限。
func (c *Counter) OverLimit() (kind string, limit float64, ok bool) {
	if c == nil {
		return "", 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.costLimitUSD > 0 && c.t.CostUSD > c.costLimitUSD {
		return "cost", c.costLimitUSD, true
	}
	if c.callLimit > 0 && c.t.Requests >= c.callLimit {
		return "calls", float64(c.callLimit), true
	}
	return "", 0, false
}

// Record 记录一次请求用量：真实 usage 尾包优先；u==nil 时按消息文本启发式
// 估算兜底（字符数/4，与 condense 包估算同口径量级），保证任何后端可计量。
func (c *Counter) Record(u *llm.Usage, promptText, completionText string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t.Requests++
	if u != nil {
		c.t.PromptTokens += int64(u.PromptTokens)
		c.t.CompletionTokens += int64(u.CompletionTokens)
		c.t.TotalTokens += int64(u.TotalTokens)
	} else {
		estIn := (len(promptText) + 3) / 4
		estOut := (len(completionText) + 3) / 4
		c.t.PromptTokens += int64(estIn)
		c.t.CompletionTokens += int64(estOut)
		c.t.TotalTokens += int64(estIn + estOut)
		c.t.EstimatedTokens += int64(estIn + estOut)
	}
	if c.promptCost > 0 || c.completionCost > 0 {
		in := float64(c.t.PromptTokens) / 1e6
		out := float64(c.t.CompletionTokens) / 1e6
		c.t.CostUSD = in*c.promptCost + out*c.completionCost
	}
}

// Snapshot 取当前累计快照。
func (c *Counter) Snapshot() Totals {
	if c == nil {
		return Totals{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// ContextWindow 模型上下文窗口（0 = 未知）。
func (c *Counter) ContextWindow() int {
	if c == nil {
		return 0
	}
	return c.contextWindow
}

// Limits 护栏配置快照（全局日额累计复用同口径单价计算时读取）。
func (c *Counter) Limits() (costLimitUSD float64, callLimit int) {
	if c == nil {
		return 0, 0
	}
	return c.costLimitUSD, c.callLimit
}
