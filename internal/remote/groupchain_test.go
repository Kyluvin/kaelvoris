package remote

// groupchain_test.go 多员工接力编排单测：无 LLM、无 engine ——
// 假 runner 记录每次收到的指令并返回固定产出，验证：
//  1. 执行顺序（emp-writer → emp-analyst）；
//  2. 员工 B 的输入包含员工 A 的产出（注明作者）；
//  3. 结果按执行顺序收集；
//  4. maxRounds 截断（4 人只执行前 3 人）；
//  5. 中途失败立即中止。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner 记录调用序列（员工 ID + 指令）。
type fakeRunner struct {
	calls []string // "empID\x00instruction"
}

func (f *fakeRunner) Run(_ context.Context, employeeID, instruction string) (string, error) {
	f.calls = append(f.calls, employeeID+"\x00"+instruction)
	return "产出(" + employeeID + "): " + instruction, nil
}

func TestRunEmployeeChain_TwoEmployees(t *testing.T) {
	f := &fakeRunner{}
	nameOf := func(id string) string {
		m := map[string]string{"emp-writer": "写作专员", "emp-analyst": "数据分析师"}
		return m[id]
	}
	results, err := RunEmployeeChain(context.Background(), f, []string{"emp-writer", "emp-analyst"}, "写一篇产品介绍", 3, nameOf)
	if err != nil {
		t.Fatalf("RunEmployeeChain: %v", err)
	}
	// 1) 执行顺序 + 次数。
	if len(f.calls) != 2 {
		t.Fatalf("应执行 2 次，实际 %d", len(f.calls))
	}
	if !strings.HasPrefix(f.calls[0], "emp-writer\x00") {
		t.Fatalf("第 1 位应执行 emp-writer，实际 %q", f.calls[0])
	}
	if !strings.HasPrefix(f.calls[1], "emp-analyst\x00") {
		t.Fatalf("第 2 位应执行 emp-analyst，实际 %q", f.calls[1])
	}
	// 2) 员工 A 的产出注入员工 B 的输入（注明作者「写作专员」）。
	body := strings.TrimPrefix(f.calls[1], "emp-analyst\x00")
	if !strings.Contains(body, "写一篇产品介绍") {
		t.Fatalf("员工 B 输入应含原始指令: %q", body)
	}
	if !strings.Contains(body, "【写作专员】") {
		t.Fatalf("员工 B 输入应含员工 A 产出（注明作者）: %q", body)
	}
	if !strings.Contains(body, "产出(emp-writer)") {
		t.Fatalf("员工 B 输入应含员工 A 产出内容: %q", body)
	}
	// 3) 员工 A 的输入不应包含任何前序产出（首员工无前置）。
	if strings.Contains(strings.TrimPrefix(f.calls[0], "emp-writer\x00"), "【") {
		t.Fatalf("员工 A 输入不应含前序产出: %q", f.calls[0])
	}
	// 4) 结果按序收集（带展示名）。
	if len(results) != 2 {
		t.Fatalf("结果应 2 条，实际 %d", len(results))
	}
	if results[0].EmployeeID != "emp-writer" || results[0].Name != "写作专员" {
		t.Fatalf("结果 0 异常: %+v", results[0])
	}
	if results[1].EmployeeID != "emp-analyst" || results[1].Name != "数据分析师" {
		t.Fatalf("结果 1 异常: %+v", results[1])
	}
}

func TestRunEmployeeChain_MaxRoundsTruncate(t *testing.T) {
	f := &fakeRunner{}
	results, err := RunEmployeeChain(context.Background(), f,
		[]string{"emp-a", "emp-b", "emp-c", "emp-d"}, "指令", GroupChainMaxRounds, nil)
	if err != nil {
		t.Fatalf("RunEmployeeChain: %v", err)
	}
	// 4 人只执行前 3 人（防失控循环）。
	if len(f.calls) != 3 {
		t.Fatalf("应截断执行 3 次，实际 %d", len(f.calls))
	}
	if len(results) != 3 {
		t.Fatalf("结果应 3 条，实际 %d", len(results))
	}
	for _, r := range results {
		if r.EmployeeID == "emp-d" {
			t.Fatalf("不应执行第 4 位员工: %+v", r)
		}
	}
}

// errRunner 指定员工返回错误（仿真员工不可用/执行失败）。
type errRunner struct {
	failOn string
	calls  []string
}

func (f *errRunner) Run(_ context.Context, employeeID, instruction string) (string, error) {
	f.calls = append(f.calls, employeeID)
	if employeeID == f.failOn {
		return "", fmt.Errorf("员工不可用: %s", employeeID)
	}
	return "OK", nil
}

func TestRunEmployeeChain_FailStops(t *testing.T) {
	f := &errRunner{failOn: "emp-b"}
	results, err := RunEmployeeChain(context.Background(), f,
		[]string{"emp-a", "emp-b", "emp-c"}, "指令", 3, nil)
	if err == nil {
		t.Fatal("应返回错误（第 2 位员工失败）")
	}
	if !strings.Contains(err.Error(), "emp-b") {
		t.Fatalf("错误应注明失败员工: %v", err)
	}
	// 已执行 2 位（emp-a 成功 + emp-b 失败），第 3 位不得执行。
	if len(f.calls) != 2 {
		t.Fatalf("失败后应中止（执行 2 次），实际 %d", len(f.calls))
	}
	if len(results) != 1 {
		t.Fatalf("失败前成功结果应 1 条，实际 %d", len(results))
	}
}

func TestSplitAssignedIDs(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"emp-writer", 1},
		{"emp-writer, emp-analyst", 2},
		{" emp-writer , , emp-analyst ", 2},
	}
	for _, c := range cases {
		if got := len(SplitAssignedIDs(c.in)); got != c.want {
			t.Fatalf("SplitAssignedIDs(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ---- RunParallelChain 并行分工模式 ----

// parallelRunner 并发安全假 runner（fakeRunner.calls 无锁，并行下会数据竞争，
// 故并行模式测试专用）：记录每次调用，可注入延迟（打乱完成序）与失败。
type parallelRunner struct {
	mu    sync.Mutex
	calls []string // "empID\x00instruction"
	delay map[string]time.Duration
	fail  map[string]error
}

func (p *parallelRunner) Run(_ context.Context, employeeID, instruction string) (string, error) {
	if d := p.delay[employeeID]; d > 0 {
		time.Sleep(d)
	}
	p.mu.Lock()
	p.calls = append(p.calls, employeeID+"\x00"+instruction)
	p.mu.Unlock()
	if err := p.fail[employeeID]; err != nil {
		return "", err
	}
	return "产出(" + employeeID + ")", nil
}

func TestRunParallelChain_OrderAndConcurrency(t *testing.T) {
	// delay 让后分配的员工先完成（emp-a 睡最久），验证结果仍按 assigned 原顺序
	// 整理（而非完成顺序）；同时验证各员工输入互不注入前序产出。
	p := &parallelRunner{delay: map[string]time.Duration{
		"emp-a": 60 * time.Millisecond,
		"emp-b": 20 * time.Millisecond,
		"emp-c": 1 * time.Millisecond,
	}}
	results, err := RunParallelChain(context.Background(), p,
		[]string{"emp-a", "emp-b", "emp-c"}, "同一指令", nil)
	if err != nil {
		t.Fatalf("RunParallelChain: %v", err)
	}
	// 1) 3 个员工全部执行。
	if len(p.calls) != 3 {
		t.Fatalf("应并行执行 3 次，实际 %d", len(p.calls))
	}
	// 2) 结果按 assigned 原顺序（emp-a 最慢完成但仍在第 0 位）。
	if len(results) != 3 {
		t.Fatalf("结果应 3 条，实际 %d", len(results))
	}
	for i, want := range []string{"emp-a", "emp-b", "emp-c"} {
		if results[i].EmployeeID != want {
			t.Fatalf("结果[%d] = %s, want %s（应按 assigned 原顺序而非完成序）", i, results[i].EmployeeID, want)
		}
	}
	// 3) 每个员工输入 = 原始指令本身（独立上下文，无「【…】」前序产出注入）。
	for _, call := range p.calls {
		body := strings.SplitN(call, "\x00", 2)[1]
		if body != "同一指令" {
			t.Fatalf("并行员工输入应=原始指令，实际 %q", body)
		}
		if strings.Contains(body, "【") {
			t.Fatalf("并行员工输入不应含前序产出: %q", body)
		}
	}
}

func TestRunParallelChain_PartialFailure(t *testing.T) {
	// emp-b 失败：不中断其他员工（与接力链 fail-fast 的区别），
	// 成功结果保留 + 聚合错误非 nil 且可定位失败员工。
	p := &parallelRunner{fail: map[string]error{"emp-b": errors.New("员工不可用")}}
	results, err := RunParallelChain(context.Background(), p,
		[]string{"emp-a", "emp-b", "emp-c"}, "指令", nil)
	if err == nil {
		t.Fatal("存在失败时应返回聚合错误")
	}
	if !strings.Contains(err.Error(), "emp-b") || !strings.Contains(err.Error(), "员工不可用") {
		t.Fatalf("聚合错误应可定位失败员工: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("成功结果应保留 2 条，实际 %d", len(results))
	}
	if len(p.calls) != 3 {
		t.Fatalf("失败不应中断其他员工（应执行 3 次），实际 %d", len(p.calls))
	}
	for _, r := range results {
		if r.EmployeeID == "emp-b" {
			t.Fatalf("失败员工不应出现在成功结果中: %+v", r)
		}
	}
}

func TestRunParallelChain_AllFailed(t *testing.T) {
	p := &parallelRunner{fail: map[string]error{
		"emp-a": errors.New("x"), "emp-b": errors.New("y"),
	}}
	results, err := RunParallelChain(context.Background(), p,
		[]string{"emp-a", "emp-b"}, "指令", nil)
	if err == nil {
		t.Fatal("全部失败时应返回聚合错误")
	}
	if len(results) != 0 {
		t.Fatalf("全部失败时结果应为空，实际 %d 条", len(results))
	}
}

func TestRunParallelChain_SingleAndTruncate(t *testing.T) {
	// 单员工：等价单步执行。
	p := &parallelRunner{}
	results, err := RunParallelChain(context.Background(), p, []string{"emp-a"}, "指令", nil)
	if err != nil || len(results) != 1 || results[0].EmployeeID != "emp-a" {
		t.Fatalf("单员工并行异常: err=%v results=%+v", err, results)
	}
	// 截断：4 人只执行前 3 人（同接力链防失控上限）。
	p2 := &parallelRunner{}
	results2, err := RunParallelChain(context.Background(), p2,
		[]string{"emp-a", "emp-b", "emp-c", "emp-d"}, "指令", nil)
	if err != nil {
		t.Fatalf("RunParallelChain: %v", err)
	}
	if len(p2.calls) != 3 || len(results2) != 3 {
		t.Fatalf("应截断执行 3 人: calls=%d results=%d", len(p2.calls), len(results2))
	}
	for _, r := range results2 {
		if r.EmployeeID == "emp-d" {
			t.Fatalf("不应执行第 4 位员工: %+v", r)
		}
	}
}

// ---- RunReviewChain 评审打磨模式 ----

func TestRunReviewChain_TwoStepReview(t *testing.T) {
	f := &fakeRunner{}
	nameOf := func(id string) string {
		m := map[string]string{"emp-writer": "写作专员", "emp-critic": "评审专家"}
		return m[id]
	}
	results, err := RunReviewChain(context.Background(), f,
		[]string{"emp-writer", "emp-critic"}, "写一篇产品介绍", nameOf)
	if err != nil {
		t.Fatalf("RunReviewChain: %v", err)
	}
	// 1) 第 1 步：产出者输入 = 原始指令（无评审指令、无前序产出）。
	first := strings.TrimPrefix(f.calls[0], "emp-writer\x00")
	if first != "写一篇产品介绍" {
		t.Fatalf("产出者输入应为原始指令: %q", first)
	}
	// 2) 第 2 步：评审者输入含原始指令 + 第 1 步产出（注明作者）+ 明确评审指令。
	second := strings.TrimPrefix(f.calls[1], "emp-critic\x00")
	if !strings.Contains(second, "写一篇产品介绍") {
		t.Fatalf("评审者输入应含原始指令: %q", second)
	}
	if !strings.Contains(second, "【写作专员】") || !strings.Contains(second, "产出(emp-writer)") {
		t.Fatalf("评审者输入应含第 1 步产出（注明作者）: %q", second)
	}
	if !strings.Contains(second, "请评审以上产出") || !strings.Contains(second, "修订后的最终版本") {
		t.Fatalf("评审者输入应含明确评审指令: %q", second)
	}
	// 3) 第 2 步产出注明评审修订版。
	if !strings.HasPrefix(results[1].Result, "【评审修订版】") {
		t.Fatalf("第 2 步产出应注明评审修订版: %q", results[1].Result)
	}
	if !strings.HasPrefix(results[0].Result, "产出(emp-writer)") {
		t.Fatalf("第 1 步产出不应有评审前缀: %q", results[0].Result)
	}
}

func TestRunReviewChain_SingleEmployee(t *testing.T) {
	// 单员工退化为等价 chain 单步：无评审指令、无修订版前缀。
	f := &fakeRunner{}
	results, err := RunReviewChain(context.Background(), f, []string{"emp-writer"}, "指令", nil)
	if err != nil {
		t.Fatalf("RunReviewChain: %v", err)
	}
	if len(f.calls) != 1 || len(results) != 1 {
		t.Fatalf("单员工应只执行 1 次: calls=%d results=%d", len(f.calls), len(results))
	}
	if strings.Contains(f.calls[0], "请评审以上产出") {
		t.Fatalf("单员工输入不应含评审指令: %q", f.calls[0])
	}
	if strings.HasPrefix(results[0].Result, "【评审修订版】") {
		t.Fatalf("单员工产出不应有评审前缀: %q", results[0].Result)
	}
}

func TestRunReviewChain_TruncateToTwo(t *testing.T) {
	// 超过 2 人截断取前 2（评审是两角色协作）。
	f := &fakeRunner{}
	results, err := RunReviewChain(context.Background(), f,
		[]string{"emp-a", "emp-b", "emp-c"}, "指令", nil)
	if err != nil {
		t.Fatalf("RunReviewChain: %v", err)
	}
	if len(f.calls) != 2 || len(results) != 2 {
		t.Fatalf("应截断执行 2 人: calls=%d results=%d", len(f.calls), len(results))
	}
	if results[1].EmployeeID != "emp-b" {
		t.Fatalf("评审者应取第 2 位员工: %+v", results[1])
	}
}

// ---- NormalizeGroupMode 模式归一化 ----

func TestNormalizeGroupMode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", GroupModeChain},
		{"chain", GroupModeChain},
		{"parallel", GroupModeParallel},
		{"review", GroupModeReview},
		{"bogus", GroupModeChain}, // 未知值回退 chain，不破坏既有接力行为
	}
	for _, c := range cases {
		if got := NormalizeGroupMode(c.in); got != c.want {
			t.Fatalf("NormalizeGroupMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
