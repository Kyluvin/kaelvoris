// resolve.go —— 岗位分配解析器 ResolveAssignment（Phase 1-A）：
//
// 《岗位分配与双区权限设计方案》（doc/09-设计方案/岗位分配与双区权限设计方案.md）
// §2 四分支「找人逻辑」的唯一分配入口（渠道 / cron / 会话 / 远程共用）：
//
//	入参 binding{employeeId, positionId, assignRule}
//	1. employeeId 非空 && Runnable   → 固定该员工
//	2. employeeId 非空 && !Runnable  → 降级：按 assignRule 从岗位池选取（§11-1 推荐）
//	3. employeeId 空 && positionId   → 按 assignRule 从岗位池选取
//	4. 都为空                        → 裸跑（存量语义：cron / 普通会话不强制）
//
// 设计约束（纯函数）：本函数不触磁盘、不持锁、不落会话快照——员工候选集由
// 调用方从 Store 传入（快照式输入），round_robin 游标（lastAssignedIndex）
// 由调用方传入传出（存资源项配置并原子落盘，§2.1 ponytail 约定：游标落盘
// 即可，无需独立游标存储）。分配结果由调用方固化进会话（Session.EmployeeID
// 快照，会话生命周期内不重选，§2 固化语义），因此多次调用须传同一游标/池序。
package employee

import (
	"fmt"
	"math/rand"
	"strings"
)

// randIntn 策略层随机源（strategy.go 用；收口一处便于测试替换）。
func randIntn(n int) int { return rand.Intn(n) }

// AssignRule 分配规则策略（§2.1 三枚举；least_loaded 仅留枚举位，二期实现）。
const (
	AssignRandom      = "random"       // 纯随机（默认；空值也按 random）
	AssignRoundRobin  = "round_robin"  // 按资源项维度轮转（游标由调用方落盘）
	AssignLeastLoaded = "least_loaded" // 运行中 taskRun 数最少（二期枚举位）
)

// Assignment 资源项绑定输入（binding.employeeId + positionId + assignRule）。
// 与设计方案 §3 单一事实源对齐：这三字段只存资源项配置（ChannelConfig /
// cron.Job），本结构仅作解析器入参快照，不持久化。
type Assignment struct {
	EmployeeID string // 绑定员工（空 = 只绑岗位或裸跑）
	PositionID string // 绑定岗位快照（绑定员工保存时回填员工当前岗位，§3）
	AssignRule string // random | round_robin | least_loaded；空 = random
	// LastIndex round_robin 游标（资源项配置内的 lastAssignedIndex）；
	// 仅 AssignRoundRobin 消费，解析后由返回值带出下一游标。
	LastIndex int
	// TestRun 草稿员工仅测试运行可用（透传 Employee.Runnable(testRun)）。
	TestRun bool
	// Load least_loaded 策略的员工运行计数器（nil = 全 0）；其他策略忽略。
	// 由调用方注入（桌面端读 taskRun 表），保持解析器纯函数语义。
	Load AssignLoadFunc
}

// ResolvedAssignment 解析结果：
//   - EmployeeID/Employee：本次执行的员工（裸跑分支两者为空值，调用方走
//     普通会话内核）；池选取场景 PoolSize 提供池规模回溯（如告警展示）。
//   - Degraded：是否发生「绑定员工不可运行 → 按分配规则从岗位池降级」分支（§2-②），
//     降级事件须记入会话 + 日志留痕（§10）。
type ResolvedAssignment struct {
	EmployeeID string
	Employee   Employee
	PoolSize   int
	Degraded   bool
}

// ResolveAssignment 四分支解析（纯函数；见文件头注释）：
//
// 分支②降级语义按 §11-1 推荐项：绑定员工不可运行 → 按 assignRule 从岗位池
// 降级选取（池 = PositionID，为空则取员工当前岗位），不可配置关闭。
// 池空不静默降级：显式报错「岗位 X 下无可运行数字员工」（§2 池空语义）。
// 游标越界（如池缩小 / 配置被手改）自动回绕取模，不报错（§2.1 轮转语义）。
func ResolveAssignment(employees []Employee, binding Assignment) (ResolvedAssignment, int, error) {
	// 分支④：员工与岗位都为空 → 裸跑（渠道侧启用校验拒绝，cron/普通会话存量语义）。
	if strings.TrimSpace(binding.EmployeeID) == "" && strings.TrimSpace(binding.PositionID) == "" {
		return ResolvedAssignment{}, 0, nil
	}

	// 分支①：绑定员工可运行 → 固定该员工（资格：Runnable + 有岗位）。
	if id := strings.TrimSpace(binding.EmployeeID); id != "" {
		e, ok := findEmployee(employees, id)
		if !ok {
			return ResolvedAssignment{}, 0, fmt.Errorf("绑定的数字员工不存在: %s", id)
		}
		if e.Runnable(binding.TestRun) {
			if e.PositionID == "" {
				return ResolvedAssignment{}, 0, fmt.Errorf("数字员工「%s」未分配岗位，请先在员工表单中指定岗位", e.Name)
			}
			return ResolvedAssignment{EmployeeID: e.ID, Employee: e}, 0, nil
		}
		// 分支②：降级——按 assignRule 从岗位池选取（§11-1 推荐项，不可配置关闭）。
		// 池岗位：资源项绑定的 positionId；为空回退员工当前岗位（§2-② 池定义）。
		posID := strings.TrimSpace(binding.PositionID)
		if posID == "" {
			posID = e.PositionID
		}
		r, next, err := pickFromPool(employees, posID, binding)
		if err != nil {
			return ResolvedAssignment{}, 0, fmt.Errorf("员工「%s」不可运行且无降级池: %w", e.Name, err)
		}
		r.Degraded = true
		return r, next, nil
	}

	// 分支③：只绑岗位 → 按 assignRule 从岗位池选取。
	return pickFromPool(employees, strings.TrimSpace(binding.PositionID), binding)
}

// pickFromPool 岗位池选取（分支②③共用）：池 = 该岗位下 Runnable 员工
// （排除不可运行者，保持员工表原始顺序），经策略注册表（strategy.go）选取。
// 返回解析结果与 round_robin 的下一游标（其他策略恒 0，不推进）。
// 空池显式报错，不静默降级（§2 池空语义：调用方把错误回给触发方）。
func pickFromPool(employees []Employee, positionID string, binding Assignment) (ResolvedAssignment, int, error) {
	var pool []Employee
	for _, e := range employees {
		if e.PositionID == positionID && e.Runnable(binding.TestRun) {
			pool = append(pool, e)
		}
	}
	if len(pool) == 0 {
		return ResolvedAssignment{}, 0, fmt.Errorf("岗位 %s 下无可运行数字员工", positionID)
	}

	idx := strategyFor(binding.AssignRule)(pool, assignCtx{
		LastIndex: binding.LastIndex,
		Load:      binding.Load,
	})
	next := 0
	if strings.EqualFold(strings.TrimSpace(binding.AssignRule), AssignRoundRobin) {
		next = binding.LastIndex + 1 // 游标单调递增，池缩放/重启取模自适应（旧语义）
	}
	return ResolvedAssignment{
		EmployeeID: pool[idx].ID,
		Employee:   pool[idx],
		PoolSize:   len(pool),
	}, next, nil
}

// findEmployee 按 ID 查找员工快照（锁在调用方 Store；此处仅遍历入参切片）。
func findEmployee(employees []Employee, id string) (Employee, bool) {
	for _, e := range employees {
		if e.ID == id {
			return e, true
		}
	}
	return Employee{}, false
}
