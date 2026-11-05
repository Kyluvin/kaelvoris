package employee

// resolve_test.go —— ResolveAssignment 四分支正负路径单测
//（设计文档 §9 测试门禁第 1 条：固定 / 降级 / 池随机 / 轮转 / 池空 / 都空）。

import (
	"strings"
	"testing"
)

// resolveFixture 构造员工快照（不落盘：ResolveAssignment 为纯函数，直接喂切片）。
func resolveFixture() []Employee {
	return []Employee{
		{ID: "emp_a", Name: "甲", Status: StatusEnabled, PositionID: "pos_dev"},
		{ID: "emp_b", Name: "乙", Status: StatusEnabled, PositionID: "pos_dev"},
		{ID: "emp_c", Name: "丙", Status: StatusEnabled, PositionID: "pos_dev"},
		{ID: "emp_off", Name: "丁", Status: StatusDisabled, PositionID: "pos_dev"},                       // 运行停用
		{ID: "emp_qa", Name: "戊", Status: StatusEnabled, PositionID: "pos_qa"},                          // 唯一 QA
		{ID: "emp_res", Name: "己", Status: StatusEnabled, EmpStatus: EmpResigned, PositionID: "pos_qa"}, // 离职（EHR 门槛）
		{ID: "emp_nopos", Name: "无岗", Status: StatusEnabled},                                            // 无岗位
	}
}

// TestResolve_FixedEmployee 分支①：绑定员工可运行 → 固定该员工（快照原样返回）。
func TestResolve_FixedEmployee(t *testing.T) {
	emps := resolveFixture()
	r, _, err := ResolveAssignment(emps, Assignment{EmployeeID: "emp_a", PositionID: "pos_dev"})
	if err != nil {
		t.Fatalf("fixed branch: %v", err)
	}
	if r.EmployeeID != "emp_a" || r.Employee.Name != "甲" || r.Degraded {
		t.Fatalf("fixed branch mismatch: %+v", r)
	}
}

// TestResolve_FixedEmployee_Negative 分支①负路径：员工不存在 / 无岗位。
func TestResolve_FixedEmployee_Negative(t *testing.T) {
	emps := resolveFixture()
	if _, _, err := ResolveAssignment(emps, Assignment{EmployeeID: "emp_ghost", PositionID: "pos_dev"}); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("expected missing-employee error, got: %v", err)
	}
	// Runnable + 无岗位 → 拒绝（§2-① 资格校验：有岗位）。
	if _, _, err := ResolveAssignment(emps, Assignment{EmployeeID: "emp_nopos"}); err == nil || !strings.Contains(err.Error(), "未分配岗位") {
		t.Fatalf("expected missing-position error, got: %v", err)
	}
}

// TestResolve_Degraded 分支②：绑定员工不可运行 → 按 assignRule 从岗位池降级
// （§11-1 推荐项：降级不可配置关闭；负路径即池空报错，见 EmptyPool 测试）。
func TestResolve_Degraded(t *testing.T) {
	emps := resolveFixture()
	// 丁 运行停用 → 从绑定岗位 pos_dev 池选取（池内只剩 甲/乙/emp_off 三人，
	// emp_off 自身不可运行被排除）。
	r, _, err := ResolveAssignment(emps, Assignment{EmployeeID: "emp_off", PositionID: "pos_dev", AssignRule: AssignRoundRobin})
	if err != nil {
		t.Fatalf("degraded branch: %v", err)
	}
	if !r.Degraded {
		t.Fatal("degraded event must be flagged (会话+日志留痕依据)")
	}
	if r.EmployeeID == "" || r.EmployeeID == "emp_off" || r.Employee.PositionID != "pos_dev" {
		t.Fatalf("degraded pick outside runnable pool: %+v", r)
	}
	if r.PoolSize == 0 {
		t.Fatalf("pool metadata expected, got %+v", r)
	}
}

// TestResolve_Degraded_PositionFallback 分支②：binding.PositionID 为空时
// 回退员工当前岗位为池（§2-② 池定义）。
func TestResolve_Degraded_PositionFallback(t *testing.T) {
	emps := resolveFixture()
	// emp_res 离职（EHR 不可运行）且绑定未回填岗位 → 池回退其当前岗位 pos_qa；
	// 池内唯一可运行者 emp_qa 与被降级者 emp_res 同岗位 → 降级成功且绝不选中自己。
	r, _, err := ResolveAssignment(emps, Assignment{EmployeeID: "emp_res"})
	if err != nil {
		t.Fatalf("fallback degrade: %v", err)
	}
	if !r.Degraded || r.EmployeeID != "emp_qa" || r.Employee.PositionID != "pos_qa" {
		t.Fatalf("fallback pool should pick from employee current position: %+v", r)
	}
}

// TestResolve_Degraded_EmptyPool 分支②③共用负路径：岗位池空 → 显式报错，
// 不静默降级（§2 池空语义；任务书「禁用降级」负路径 = 降级不可执行时拒绝）。
func TestResolve_Degraded_EmptyPool(t *testing.T) {
	emps := resolveFixture()
	// 岗位绑定但岗位下无人 → 显式报错。
	_, _, err := ResolveAssignment(emps, Assignment{PositionID: "pos_void"})
	if err == nil || !strings.Contains(err.Error(), "岗位 pos_void 下无可运行数字员工") {
		t.Fatalf("expected empty-pool error, got: %v", err)
	}
	// 无效岗位 ID（字典中不存在）同池空语义报错。
	if _, _, err := ResolveAssignment(emps, Assignment{PositionID: "pos_not_exist"}); err == nil || !strings.Contains(err.Error(), "岗位 pos_not_exist 下无可运行数字员工") {
		t.Fatalf("expected invalid-position error, got: %v", err)
	}
}

// TestResolve_PositionRandom 分支③ random：结果必在池内（可运行 + 岗位匹配），
// 不可运行员工永不入选。
func TestResolve_PositionRandom(t *testing.T) {
	emps := resolveFixture()
	poolIDs := map[string]bool{"emp_a": true, "emp_b": true, "emp_c": true}
	for i := 0; i < 20; i++ {
		r, next, err := ResolveAssignment(emps, Assignment{PositionID: "pos_dev"}) // AssignRule 空 = random
		if err != nil {
			t.Fatalf("random pick: %v", err)
		}
		if !poolIDs[r.EmployeeID] {
			t.Fatalf("random picked outside pool: %+v", r)
		}
		if next != 0 {
			t.Fatalf("random must not advance cursor, got %d", next)
		}
		if r.Degraded {
			t.Fatalf("branch③ must not be flagged degraded: %+v", r)
		}
	}
}

// TestResolve_PositionRoundRobin 分支③ round_robin：顺序轮转 + 游标回绕
// （越界/负值取模，配置手改或池缩小时不报错，§2.1）。
func TestResolve_PositionRoundRobin(t *testing.T) {
	emps := []Employee{
		{ID: "emp_a", Name: "甲", Status: StatusEnabled, PositionID: "pos_dev"},
		{ID: "emp_b", Name: "乙", Status: StatusEnabled, PositionID: "pos_dev"},
		{ID: "emp_c", Name: "丙", Status: StatusEnabled, PositionID: "pos_dev"},
	}
	// 顺序轮转：0 → 1 → 2 → 0（游标由调用方传入传出，解析器纯函数）。
	want := []string{"emp_a", "emp_b", "emp_c", "emp_a"}
	cursor := 0
	for i, wantID := range want {
		r, next, err := ResolveAssignment(emps, Assignment{PositionID: "pos_dev", AssignRule: AssignRoundRobin, LastIndex: cursor})
		if err != nil {
			t.Fatalf("round_robin pick %d: %v", i, err)
		}
		if r.EmployeeID != wantID {
			t.Fatalf("round_robin seq %d: got %s want %s", i, r.EmployeeID, wantID)
		}
		if next != cursor+1 {
			t.Fatalf("round_robin next cursor: got %d want %d", next, cursor+1)
		}
		cursor = next
	}
	// 游标越界回绕：池 3 人、LastIndex=5 → 选取索引 = 5 mod 3 = 2，下一游标 6（单调计数）。
	r, next, err := ResolveAssignment(emps, Assignment{PositionID: "pos_dev", AssignRule: AssignRoundRobin, LastIndex: 5})
	if err != nil || r.EmployeeID != "emp_c" || next != 6 {
		t.Fatalf("cursor wrap-forward: r=%+v next=%d err=%v", r, next, err)
	}
	// 负游标回绕：LastIndex=-1 → 选取索引 = 2，下一游标 0。
	if r, next, _ = ResolveAssignment(emps, Assignment{PositionID: "pos_dev", AssignRule: AssignRoundRobin, LastIndex: -1}); r.EmployeeID != "emp_c" || next != 0 {
		t.Fatalf("cursor wrap-negative: r=%+v next=%d", r, next)
	}
}

// TestResolve_BareRun 分支④：员工与岗位都为空 → 裸跑零值（无错误；
// 渠道侧启用校验负责拒绝，cron/普通会话保持存量语义）。
func TestResolve_BareRun(t *testing.T) {
	r, next, err := ResolveAssignment(resolveFixture(), Assignment{})
	if err != nil || next != 0 || r.EmployeeID != "" || r.Degraded {
		t.Fatalf("bare-run must return zero-value result: %+v next=%d err=%v", r, next, err)
	}
}

// TestResolve_LeastLoadedPlaceholder least_loaded 二期枚举位（§11-2）：
// 本期按 random 兜底，返回员工必须在池内且可运行。
func TestResolve_LeastLoaded(t *testing.T) {
	emps := resolveFixture()
	r, _, err := ResolveAssignment(emps, Assignment{PositionID: "pos_dev", AssignRule: AssignLeastLoaded})
	if err != nil {
		t.Fatalf("least_loaded fallback: %v", err)
	}
	if r.EmployeeID != "emp_a" && r.EmployeeID != "emp_b" && r.EmployeeID != "emp_c" {
		t.Fatalf("least_loaded fallback pick outside runnable pool: %+v", r)
	}
}

// TestResolve_TestRunDraft 草稿员工仅测试运行可用（Runnable(testRun) 透传）。
func TestResolve_TestRun(t *testing.T) {
	emps := []Employee{
		{ID: "emp_draft", Name: "草稿员", Status: StatusDraft, PositionID: "pos_dev"},
	}
	// 正式运行：草稿不进池 → 池空报错。
	if _, _, err := ResolveAssignment(emps, Assignment{PositionID: "pos_dev"}); err == nil {
		t.Fatal("draft employee must be excluded from pool in production run")
	}
	// 测试运行（testRun=true）→ 可选取。
	if r, _, err := ResolveAssignment(emps, Assignment{PositionID: "pos_dev", TestRun: true}); err != nil || r.EmployeeID != "emp_draft" {
		t.Fatalf("test-run draft should be pickable: %+v err=%v", r, err)
	}
}
