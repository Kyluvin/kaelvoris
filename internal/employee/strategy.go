package employee

// strategy.go 岗位池分配策略（策略模式收口，原散在 pickFromPool 的 switch）：
//
//	策略 = 纯函数 (pool, ctx) → 选中下标；注册表驱动，新增策略实现 assignStrategy
//	后 RegisterAssignStrategy 即插入（无需改解析器主干）。
//	现有三策略：random（默认）/ round_robin（游标）/ least_loaded（运行中 taskRun
//	数最少；并列取先，池序即员工表原始序——快照由调用方从 Store.List 传入，稳定）。
//
// 纯函数约定延续 resolve.go：本文件不触盘、不持锁；Load 计数器由调用方传入。

import "strings"

// AssignLoadFunc 查询员工当前运行中任务数（least_loaded 策略注入点；
// 桌面端实现读 taskRun 表，测试传桩函数）。
type AssignLoadFunc func(employeeID string) int

// assignCtx 策略上下文（解析器透传，策略只读）。
type assignCtx struct {
	LastIndex int            // round_robin 游标（资源项配置内单调计数）
	Load      AssignLoadFunc // 员工运行计数（least_loaded 用；nil = 全 0）
	TestRun   bool           // 草稿员工仅测试可用（池过滤已用，策略层无感）
}

// assignStrategy 策略接口（返回池内选中下标）。
type assignStrategy func(pool []Employee, ctx assignCtx) int

// assignStrategies 策略注册表（key = AssignRule 归一化小写；空 = random）。
var assignStrategies = map[string]assignStrategy{
	AssignRandom:      randomStrategy,
	AssignRoundRobin:  roundRobinStrategy,
	AssignLeastLoaded: leastLoadedStrategy,
}

// strategyFor 按 AssignRule 取策略（未知名/空 = random 兜底，与旧版行为一致）。
func strategyFor(rule string) assignStrategy {
	if s, ok := assignStrategies[strings.ToLower(strings.TrimSpace(rule))]; ok {
		return s
	}
	return randomStrategy
}

// randomStrategy 纯随机。
func randomStrategy(pool []Employee, ctx assignCtx) int {
	_ = ctx
	return randIntn(len(pool))
}

// roundRobinStrategy 单调计数器取模：idx = LastIndex mod 池长；
// 负值（配置手改）取模自适应。下一游标由 ResolveAssignment 统一推进。
func roundRobinStrategy(pool []Employee, ctx assignCtx) int {
	idx := ctx.LastIndex % len(pool)
	if idx < 0 {
		idx += len(pool)
	}
	return idx
}

// leastLoadedStrategy 运行中任务数最少者（并列取池中靠前者；Load nil = 全 0 → 恒选首位）。
func leastLoadedStrategy(pool []Employee, ctx assignCtx) int {
	best := 0
	bestLoad := -1
	for i, e := range pool {
		load := 0
		if ctx.Load != nil {
			load = ctx.Load(e.ID)
		}
		if bestLoad < 0 || load < bestLoad {
			best, bestLoad = i, load
			if load == 0 {
				break // 最优不可能再小，提前收敛
			}
		}
	}
	return best
}
