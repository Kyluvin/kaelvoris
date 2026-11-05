// state.go —— 条目状态机与自动迭代节流。
//
// 状态流转规则（合法迁移表）：
//
//	proposed  → approved | rejected | deleted
//	approved  → developing（开始开发）| proposed（撤回）
//	developing→ done | failed | blocked（尝试超上限，不再自动开发）
//	failed    → approved（重试：回队）| proposed（回审核）
//	blocked   → approved（人工重跑：清封存原因回队；不再自动开发）
//	done/rejected：终态（仅可人工删除或手动回拨）。
//
// 自动迭代（autoDev）防失控约束：
//   - 单条开发会话复用固定 ID（roadmap_<itemID>），引擎级防重入；
//   - 每日开发上限（autoDevDailyLimit，默认 12 条）：超限当日暂停自动开发，
//     次日自动恢复——对齐 cron 调度器 MaxFailStreak 熔断思想，防预算风暴；
//   - 自动规划每轮最多产出 maxPlanItems 条提案，超出丢弃。
package roadmap

import (
	"sync"
	"time"
)

// CanTransition 状态流转校验（引擎与面板共用同一规则）。
func CanTransition(from, to string) bool {
	switch from {
	case StatusProposed:
		return to == StatusApproved || to == StatusRejected
	case StatusApproved:
		return to == StatusDeveloping || to == StatusProposed
	case StatusDeveloping:
		return to == StatusDone || to == StatusFailed || to == StatusBlocked
	case StatusFailed:
		return to == StatusApproved || to == StatusProposed
	case StatusBlocked:
		return to == StatusApproved // 人工重跑回队（MarkRetry；不再自动开发）
	default: // done / rejected：终态
		return false
	}
}

// NextRunAt 自动迭代的下一次完整循环时间（规划 → 开发 → 复盘为一个周期）。
// 间隔默认 2h（对齐自主 Agent 项目常见的 continuous-run 心跳粒度；
// 可经 AutoDevInterval 覆盖）。最近一次循环结束时间传入，未运行过返回零值。
func NextRunAt(lastFinished time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		interval = 2 * time.Hour
	}
	if lastFinished.IsZero() {
		return time.Now()
	}
	return lastFinished.Add(interval)
}

// AutoDevState 自动迭代运行态（进程级；panel 层持有，不持久化——重启后
// 由 items.json 的 developing 状态自然恢复续跑）。
type AutoDevState struct {
	mu         sync.Mutex
	Running    bool      // 自动循环在跑
	LastRunAt  time.Time // 最近一次完整循环时间
	TodayCount int       // 今日已开发条目数（熔断计数）
	TodayKey   string    // 计数归属日（YYYY-MM-DD，跨天清零）
	LastPlanAt time.Time // 最近一次自动规划时间（节流：最小间隔 30 分钟）
	// ZeroRounds 连续「规划产出全被驳回」轮数（零通过熔断计数：连续 3 轮
	// 全驳回当日暂停自动规划，防「规划→全驳回→零消耗→立刻再规划」空转烧钱）。
	ZeroRounds int
}

// AllowAutoDev 今日额度判断（超出每日上限返回 false）。
func (s *AutoDevState) AllowAutoDev(dailyLimit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDay()
	if dailyLimit > 0 && s.TodayCount >= dailyLimit {
		return false
	}
	return true
}

// RecordDev 记一次开发完成（done 或 failed 均计数）。
func (s *AutoDevState) RecordDev() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDay()
	s.TodayCount++
}

// RecordZeroRound 记一轮「规划产出全部被驳回」（零通过）；返回累计连续轮数。
func (s *AutoDevState) RecordZeroRound() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDay()
	s.ZeroRounds++
	return s.ZeroRounds
}

// ResetZeroRounds 本轮规划有提案通过审核：清零熔断计数。
func (s *AutoDevState) ResetZeroRounds() {
	s.mu.Lock()
	s.ZeroRounds = 0
	s.mu.Unlock()
}

// PlanBlockedByZero 零通过熔断判定：连续 autoDevMaxZeroRounds 轮规划产出
// 全被驳回时，当日暂停自动规划（审核/开发不受影响）——治标（防空转）配合
// 驳回反馈回注（治本：提升通过率）。跨天由 rollDay 清零自动恢复。
func (s *AutoDevState) PlanBlockedByZero() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDay()
	return s.ZeroRounds >= AutoDevMaxZeroRounds
}

// AutoDevMaxZeroRounds 连续全驳回熔断阈值（超过即当日暂停自动规划；导出供
// panel 层提示文案使用）。
const AutoDevMaxZeroRounds = 3

// rollDay 跨天清零计数（加锁调用）。
func (s *AutoDevState) rollDay() {
	key := time.Now().Format("2006-01-02")
	if s.TodayKey != key {
		s.TodayKey = key
		s.TodayCount = 0
		s.ZeroRounds = 0
	}
}
