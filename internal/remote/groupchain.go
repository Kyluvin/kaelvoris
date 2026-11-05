package remote

// groupchain.go 多员工协作编排（群聊 group.* 协议核心机制，三模式）：
//
//   - chain    接力（RunEmployeeChain）：按序执行，后员工输入含前序产出；
//   - parallel 并行分工（RunParallelChain）：所有员工独立执行同一指令，互不注入前序产出；
//   - review   评审打磨（RunReviewChain）：第 1 位产出、第 2 位评审修订（AutoGen reflection）。
//
// App 层（remote_group.go remoteGroupTask）用真实 runGroupEmployeeTask 适配
// Runner；单测用假 runner，无需 LLM/engine。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// GroupChainMaxRounds 接力链员工数量上限：防失控循环（有人误把几十个员工
// 全选进来，逐个执行会拖垮节点）。ponytail: 固定上限 3，需要无限对话/批量
// 全员接力再按 maxRounds 参数化扩展。
const GroupChainMaxRounds = 3

// 协作模式常量（协议 Mode 字段取值；调研 AutoGen SelectorGroupChat/RoundRobin/
// Swarm 后收敛为三模式，语义映射见各 Run 函数注释）。
const (
	GroupModeChain    = "chain"    // 接力：按序执行，后员工输入含前序产出（默认，向后兼容）
	GroupModeParallel = "parallel" // 并行分工：所有员工独立执行同一指令，互不可见
	GroupModeReview   = "review"   // 评审打磨：第 1 位产出，第 2 位评审修订
)

// NormalizeGroupMode 归一化协作模式：空/未知一律回退 chain——
// 旧客户端不发 mode 字段、新客户端传异常值时都不能破坏既有接力行为。
func NormalizeGroupMode(s string) string {
	switch s {
	case GroupModeParallel:
		return GroupModeParallel
	case GroupModeReview:
		return GroupModeReview
	default:
		return GroupModeChain
	}
}

// SplitAssignedIDs 解析 AssignedTo 的逗号分隔员工 ID 列表（去空项）。
func SplitAssignedIDs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if id := strings.TrimSpace(p); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// chainStepClip 单段产出注入下一位员工输入的截断上限（rune 数）。
// 防超长产出把下一位员工的上下文撑爆（对齐 clipTaskResult 的长文截断处理）。
const chainStepClip = 2000

// StepResult 接力链单步执行结果（与 GroupStepResult 同构；独立类型便于单测断言）。
type StepResult struct {
	EmployeeID string
	Name       string
	Result     string
}

// Runner 单员工执行接口：App 层用真实 runGroupEmployeeTask 适配，
// 单测注入假 runner（无 LLM、无 engine 依赖）。
type Runner interface {
	Run(ctx context.Context, employeeID, instruction string) (string, error)
}

// RunEmployeeChain 按序执行员工接力（核心编排，纯逻辑可单测）：
//   - 第 N 步输入 = 原始指令 + 前序产出（每段格式「【员工名】<产出>」，截断 chainStepClip）；
//   - 员工按 assigned 顺序逐个执行，结果按序收集；某员工失败立即中止并返回错误；
//   - 员工数超过 maxRounds 时只执行前 maxRounds 个（截断防失控；默认 GroupChainMaxRounds）。
//
// nameOf 为员工 ID → 展示名解析（nil 时回退用 ID 注明作者）；App 层传
// grp.Members 快照解析，单测可注入固定名字。
func RunEmployeeChain(ctx context.Context, runner Runner, assigned []string, instruction string, maxRounds int, nameOf func(string) string) ([]StepResult, error) {
	if maxRounds <= 0 || maxRounds > GroupChainMaxRounds {
		maxRounds = GroupChainMaxRounds // ponytail: 固定上限 3，需要无限对话再扩展
	}
	if len(assigned) > maxRounds {
		assigned = assigned[:maxRounds]
	}
	results := make([]StepResult, 0, len(assigned))
	for _, empID := range assigned {
		empID = strings.TrimSpace(empID)
		if empID == "" {
			continue
		}
		input := buildChainInput(instruction, results)
		out, err := runner.Run(ctx, empID, input)
		if err != nil {
			return results, err
		}
		results = append(results, StepResult{EmployeeID: empID, Name: chainName(nameOf, empID), Result: out})
	}
	return results, nil
}

// RunParallelChain 并行分工模式（AutoGen Swarm 语义：无依赖任务各员工并行执行）：
//   - 所有员工并行执行同一原始指令，每个员工独立上下文，不注入前序产出
//     （与接力链 buildChainInput 的核心区别：并行员工互不可见，避免相互带偏）；
//   - 结果按 assigned 原顺序整理（预分配切片按索引写，goroutine + WaitGroup）；
//   - 单个员工失败不中断其他员工（与接力链 fail-fast 的关键区别）：
//     存在失败时返回聚合错误（errors.Join 多行「员工 X 执行失败: …」），
//     成功的结果照常返回；全部失败时结果为空 + 聚合错误；
//   - 员工数超过 GroupChainMaxRounds 截断（同接力链防失控）。
func RunParallelChain(ctx context.Context, runner Runner, assigned []string, instruction string, nameOf func(string) string) ([]StepResult, error) {
	if len(assigned) > GroupChainMaxRounds {
		assigned = assigned[:GroupChainMaxRounds]
	}
	ids := make([]string, 0, len(assigned))
	for _, id := range assigned {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	type runOut struct {
		result string
		err    error
	}
	raw := make([]runOut, len(ids)) // 预分配按索引写：保证结果顺序 = assigned 原顺序
	var wg sync.WaitGroup
	for i, empID := range ids {
		wg.Add(1)
		go func(i int, empID string) {
			defer wg.Done()
			out, err := runner.Run(ctx, empID, instruction)
			raw[i] = runOut{result: out, err: err}
		}(i, empID)
	}
	wg.Wait()
	var (
		results []StepResult
		errs    []error
	)
	for i, empID := range ids {
		if err := raw[i].err; err != nil {
			// 错误不吞：失败员工在聚合错误中可定位（用 ID，比展示名更唯一可诊断）。
			errs = append(errs, fmt.Errorf("员工 %s 执行失败: %w", empID, err))
			continue
		}
		results = append(results, StepResult{EmployeeID: empID, Name: chainName(nameOf, empID), Result: raw[i].result})
	}
	// errors.Join 对空切片/全 nil 返回 nil，仅存在失败时才有聚合错误。
	return results, errors.Join(errs...)
}

// RunReviewChain 评审打磨模式（AutoGen primary+critic reflection pattern）：
//   - 第 1 位员工按原始指令产出（primary）；
//   - 第 2 位员工作为评审者（critic）：输入 = 原始指令 + 第 1 位产出（复用
//     buildChainInput 注明作者）+ 明确评审指令（指出错误遗漏并直接给出修订版）；
//     第 2 步产出注明「评审修订版」，群消息广播时用户可直接辨识；
//   - len(assigned)==1 退化为单员工执行（等价 chain 单步，无评审环节）；
//   - 超过 2 人截断取前 2（评审是两角色协作，多余人选无意义）；
//   - 第 1 步失败立即中止（评审无从进行，保持接力链 fail-fast 语义）。
func RunReviewChain(ctx context.Context, runner Runner, assigned []string, instruction string, nameOf func(string) string) ([]StepResult, error) {
	if len(assigned) > 2 {
		assigned = assigned[:2]
	}
	var results []StepResult
	for _, empID := range assigned {
		empID = strings.TrimSpace(empID)
		if empID == "" {
			continue
		}
		// 复用 buildChainInput：首位员工 results 为空 → 输入即原始指令；
		// 评审位员工拿到「原始指令 + 第 1 位产出（注明作者）」。
		input := buildChainInput(instruction, results)
		if len(results) > 0 {
			// 评审指令仅在有前序产出时注入（对齐 AutoGen critic 反思提示）。
			input += "\n\n请评审以上产出：指出事实错误与遗漏，并直接给出修订后的最终版本"
		}
		out, err := runner.Run(ctx, empID, input)
		if err != nil {
			return results, err
		}
		r := StepResult{EmployeeID: empID, Name: chainName(nameOf, empID), Result: out}
		if len(results) > 0 {
			r.Result = "【评审修订版】\n" + out
		}
		results = append(results, r)
	}
	return results, nil
}

// buildChainInput 组拼第 N 步员工输入：原始指令 + 前序各员工产出（注明作者）。
func buildChainInput(instruction string, prev []StepResult) string {
	var b strings.Builder
	b.WriteString(instruction)
	for _, r := range prev {
		b.WriteString("\n\n【" + r.Name + "】" + clipChain(r.Result))
	}
	return b.String()
}

// chainName 解析员工展示名（nameOf 为 nil 时回退员工 ID）。
func chainName(nameOf func(string) string, empID string) string {
	if nameOf != nil {
		if n := nameOf(empID); n != "" {
			return n
		}
	}
	return empID
}

// clipChain 截断单段产出（rune 安全）。
func clipChain(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > chainStepClip {
		return string(r[:chainStepClip]) + "…"
	}
	return string(r)
}
