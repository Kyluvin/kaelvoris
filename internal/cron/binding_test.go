package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJobBindingJSONRoundTrip 新字段序列化/反序列化往返（doc/09 §3）。
func TestJobBindingJSONRoundTrip(t *testing.T) {
	j := Job{
		ID:                "cron_x",
		Name:              "日报",
		Expr:              "0 9 * * *",
		Task:              "生成日报",
		EmployeeID:        "emp_zhang",
		PositionID:        "pos_dev",
		AssignRule:        AssignRuleRoundRobin,
		LastAssignedIndex: 3,
	}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	// 序列化键名对齐文档（§3：employeeId/positionId/assignRule）。
	for _, key := range []string{`"employeeId":"emp_zhang"`, `"positionId":"pos_dev"`, `"assignRule":"round_robin"`, `"lastAssignedIndex":3`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("序列化输出缺少 %s：%s", key, b)
		}
	}
	var back Job
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.EmployeeID != j.EmployeeID || back.PositionID != j.PositionID ||
		back.AssignRule != j.AssignRule || back.LastAssignedIndex != j.LastAssignedIndex {
		t.Fatalf("往返不一致：%+v vs %+v", back, j)
	}
}

// TestJobOldJSONCompat 旧 JSON（无新字段）反序列化兼容不报错、零值语义
// 为未绑定（doc/09 §10：存量 cron 无员工字段，字段为空保持现状不迁移）。
func TestJobOldJSONCompat(t *testing.T) {
	old := `[{"id":"cron_old","name":"存量任务","expr":"0 9 * * *","task":"跑","enabled":true}]`
	var jobs []Job
	if err := json.Unmarshal([]byte(old), &jobs); err != nil {
		t.Fatalf("旧数据加载应不报错: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("应解析出 1 条，得到 %d", len(jobs))
	}
	j := jobs[0]
	if j.EmployeeID != "" || j.PositionID != "" || j.AssignRule != "" || j.LastAssignedIndex != 0 {
		t.Fatalf("旧数据新字段应全为零值: %+v", j)
	}
	// 旧数据经 validateJob 应合法（存量裸跑不拦）。
	if err := validateJob(j); err != nil {
		t.Fatalf("存量无绑定任务应通过校验: %v", err)
	}
}

// TestStoreOldJobsFileCompat 旧 jobs.json 落盘文件经 NewStore 正常加载。
func TestStoreOldJobsFileCompat(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cron")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := `[{"id":"cron_old","name":"旧任务","expr":"0 9 * * *","task":"跑","enabled":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(dir, "jobs.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	jobs := st.List()
	if len(jobs) != 1 || jobs[0].EmployeeID != "" || jobs[0].PositionID != "" {
		t.Fatalf("旧文件加载异常: %+v", jobs)
	}
	// 在旧数据基础上 Update 带绑定字段，往返落盘后重开仍可读。
	jobs[0].EmployeeID = "emp_a"
	jobs[0].PositionID = "pos_a"
	jobs[0].AssignRule = AssignRuleRandom
	if _, err := st.Update(jobs[0]); err != nil {
		t.Fatalf("旧任务更新绑定应成功: %v", err)
	}
	st2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st2.Get(jobs[0].ID)
	if !ok || got.EmployeeID != "emp_a" || got.PositionID != "pos_a" || got.AssignRule != AssignRuleRandom {
		t.Fatalf("绑定字段落盘往返不一致: %+v", got)
	}
}

// TestValidateBinding 校验辅助正负路径（§4.4 组合校验）。
func TestValidateBinding(t *testing.T) {
	cases := []struct {
		name        string
		employeeID  string
		positionID  string
		assignRule  string
		wantErr     bool
		errContains string
	}{
		{name: "全空裸跑合法", wantErr: false},
		{name: "仅岗位+random", positionID: "pos_dev", assignRule: AssignRuleRandom, wantErr: false},
		{name: "仅岗位+round_robin", positionID: "pos_dev", assignRule: AssignRuleRoundRobin, wantErr: false},
		{name: "仅岗位+least_loaded 枚举位放行", positionID: "pos_dev", assignRule: AssignRuleLeastLoaded, wantErr: false},
		{name: "员工+岗位", employeeID: "emp_a", positionID: "pos_dev", assignRule: AssignRuleRandom, wantErr: false},
		{name: "员工必带岗位", employeeID: "emp_a", wantErr: true, errContains: "岗位"},
		{name: "全空带规则拒绝", assignRule: "random", wantErr: true, errContains: "分配规则"},
		{name: "非法规则拒绝", positionID: "pos_dev", assignRule: "round-robin", wantErr: true, errContains: "非法分配规则"},
	}
	for _, c := range cases {
		err := ValidateBinding(c.employeeID, c.positionID, c.assignRule)
		if c.wantErr && err == nil {
			t.Errorf("%s: 应拒绝，得到 nil", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: 应通过，得到 %v", c.name, err)
		}
		if c.wantErr && err != nil && c.errContains != "" && !strings.Contains(err.Error(), c.errContains) {
			t.Errorf("%s: 错误应含 %q，得到 %v", c.name, c.errContains, err)
		}
	}
}

// TestAddUpdateBindingGate validateJob 已接入 Add/Update：非法绑定在存储
// 层即拒绝（§4.4 校验负路径），合法绑定正常落盘。
func TestAddUpdateBindingGate(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "cron"))
	if err != nil {
		t.Fatal(err)
	}
	// 负路径：员工无岗位 → 拒绝。
	if _, err := st.Add(Job{Name: "坏", Expr: "* * * * *", Task: "t", EmployeeID: "emp_a"}); err == nil {
		t.Fatal("指定员工无岗位应被 Add 拒绝")
	}
	// 负路径：全空携带分配规则 → 拒绝。
	if _, err := st.Add(Job{Name: "坏", Expr: "* * * * *", Task: "t", AssignRule: AssignRuleRandom}); err == nil {
		t.Fatal("未绑定却带分配规则应被 Add 拒绝")
	}
	// 正路径：员工+岗位绑定落盘成功。
	j, err := st.Add(Job{Name: "好", Expr: "0 9 * * *", Task: "t", Enabled: true,
		EmployeeID: "emp_a", PositionID: "pos_dev", AssignRule: AssignRuleRoundRobin})
	if err != nil {
		t.Fatalf("合法绑定应保存成功: %v", err)
	}
	// Update 同样校验：改成员工无岗位 → 拒绝。
	j.PositionID = ""
	if _, err := st.Update(j); err == nil {
		t.Fatal("Update 移除岗位快照应被拒绝")
	}
	// Update 合法绑定（含轮转游标）→ 成功且持久化。
	j.PositionID = "pos_dev"
	j.LastAssignedIndex = 1
	if _, err := st.Update(j); err != nil {
		t.Fatalf("合法 Update 应成功: %v", err)
	}
	got, ok := st.Get(j.ID)
	if !ok || got.LastAssignedIndex != 1 {
		t.Fatalf("轮转游标应随 Update 持久化: %+v", got)
	}
}
