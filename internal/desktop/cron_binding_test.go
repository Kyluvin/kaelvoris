// cron_binding_test.go 岗位分配×双区权限 Phase 2-② cron 执行链路（doc/09-设计方案）：
//   - cronRunner：绑定员工直跑（分支①） / 岗位池 random（分支③） /
//     round_robin 游标推进并持久化（§2.1） / 降级留痕（§2-②/§10：日志+会话） /
//     池空失败留痕（§10 不静默降级） / 绑定全空裸跑（分支④ 存量语义不变）；
//   - resolveCronEmployee 游标回写：仅 round_robin 推进（random 恒 0），
//     经 cron Store Update 随 jobs.json 原子落盘；
//   - syncCronSession 员工岗位快照固化（§6.1：EmployeeID + PositionID +
//     AssignRule；裸跑时清空快照，与 job 配置保持一致）。
//
// 不依赖 Wails 上下文与真实 LLM：App 仅构造 store/cronStore/empStore/orgStore；
// cronRunner 送执行链经 SendMessageWithEmployee → go runTask 异步起跑，测试环境
// 无 API Key 时 runTask 经 buildKernel 校验提前退出（uiEmit ctx==nil 安全旁路，
// 不触碰 runs/registry），仅产生日志事件，不影响会话侧断言。
package desktop

import (
	"strings"
	"testing"

	"kaelvoris/internal/cron"
	"kaelvoris/internal/employee"
	"kaelvoris/internal/session"
	"kaelvoris/pkg/message"
)

// newCronBindingTestApp 构造无 Wails 上下文的测试 App（session/cron/employee/org
// 存储各自独立临时子目录，避免文件名冲突与相互污染）。
func newCronBindingTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	st := session.NewStore(dir + "/sessions")
	cs, err := cron.NewStore(dir + "/cron")
	if err != nil {
		t.Fatalf("cron store: %v", err)
	}
	emp, err := employee.NewStore(dir + "/employees")
	if err != nil {
		t.Fatalf("employee store: %v", err)
	}
	org, err := employee.NewOrgStore(dir + "/employees")
	if err != nil {
		t.Fatalf("org store: %v", err)
	}
	// session.NewStore 带 200ms 后台 flusher；Close 停止 flusher 并落盘脏数据，
	// 否则测试返回后 flusher 仍向 t.TempDir 写文件，与 RemoveAll 清理竞态
	// （复现：-count=2 时报 unlinkat "The directory is not empty."）。
	t.Cleanup(func() { st.Close() })
	return &App{store: st, cronStore: cs, empStore: emp, orgStore: org}
}

// seedCronEmployee 经 SaveEmployee 落一个可运行员工（Status=enabled 缺省 + 岗位必填）。
func seedCronEmployee(t *testing.T, a *App, id, name, positionID string, empStatus string) employee.Employee {
	t.Helper()
	if empStatus == "" {
		empStatus = employee.EmpActive
	}
	e, err := a.SaveEmployee(employee.Employee{
		ID: id, Name: name, Status: employee.StatusEnabled,
		PositionID: positionID, EmpStatus: empStatus,
	})
	if err != nil {
		t.Fatalf("SaveEmployee(%s): %v", name, err)
	}
	return e
}

// addBoundJob 直插带绑定三元组的任务（AddCronJob 尚无绑定参数，绑定字段经
// Store 落库；Expr 取合法 5 段式避免校验失败）。
func addBoundJob(t *testing.T, a *App, employeeID, positionID, rule string, lastIndex int) cron.Job {
	t.Helper()
	j, err := a.cronStore.Add(cron.Job{
		Name: "巡检", Expr: "0 9 * * *", Task: "每日巡检",
		EmployeeID: employeeID, PositionID: positionID,
		AssignRule: rule, LastAssignedIndex: lastIndex,
	})
	if err != nil {
		t.Fatalf("cronStore.Add: %v", err)
	}
	return j
}

// TestCronRunner_FixedEmployee 分支①：绑定员工可运行 → 固定执行并固化会话快照。
func TestCronRunner_FixedEmployee(t *testing.T) {
	a := newCronBindingTestApp(t)
	p := seedPosition(t, a, "开发")
	seedCronEmployee(t, a, "emp_a", "甲", p.ID, "")
	j := addBoundJob(t, a, "emp_a", p.ID, "", 0)

	sessID, err := a.cronRunner(j)
	if err != nil {
		t.Fatalf("cronRunner: %v", err)
	}
	if sessID != "cron_"+j.ID {
		t.Fatalf("sessID mismatch: %s", sessID)
	}
	// 会话壳可能尚未由异步 runTask 创建（首轮），直接断言解析与游标语义。
	if got, _, err := a.resolveCronEmployee(j); err != nil || got != "emp_a" {
		t.Fatalf("resolve = (%q, %v), want emp_a/nil", got, err)
	}
}

// TestCronRunner_PositionPoolRandom 分支③：只绑岗位 random → 从池中选人。
func TestCronRunner_PositionPoolRandom(t *testing.T) {
	a := newCronBindingTestApp(t)
	p := seedPosition(t, a, "开发")
	seedCronEmployee(t, a, "emp_a", "甲", p.ID, "")
	seedCronEmployee(t, a, "emp_b", "乙", p.ID, "")
	j := addBoundJob(t, a, "", p.ID, "", 0)

	got, degraded, err := a.resolveCronEmployee(j)
	if err != nil || degraded {
		t.Fatalf("pool random: got (%q, %v, %v)", got, degraded, err)
	}
	if got != "emp_a" && got != "emp_b" {
		t.Fatalf("pool pick = %q, want emp_a|emp_b", got)
	}
	// 游标不推进（random 恒 0）。
	if jj, _ := a.cronStore.Get(j.ID); jj.LastAssignedIndex != 0 {
		t.Fatalf("random 不应推进游标, got %d", jj.LastAssignedIndex)
	}
}

// TestCronRunner_RoundRobinCursor round_robin：游标推进并经 Store Update 持久化
// （§2.1 随 jobs.json 原子落盘；轮转顺序按池内员工原始序）。
func TestCronRunner_RoundRobinCursor(t *testing.T) {
	a := newCronBindingTestApp(t)
	p := seedPosition(t, a, "开发")
	seedCronEmployee(t, a, "emp_a", "甲", p.ID, "")
	seedCronEmployee(t, a, "emp_b", "乙", p.ID, "")
	seedCronEmployee(t, a, "emp_c", "丙", p.ID, "")
	j := addBoundJob(t, a, "", p.ID, employee.AssignRoundRobin, 0)

	// 第 1 次触发：游标 0 → 选池第 1 人（甲），落库游标 = 1。
	got1, _, err := a.resolveCronEmployee(j)
	if err != nil {
		t.Fatalf("rr#1: %v", err)
	}
	if got1 != "emp_a" {
		t.Fatalf("rr#1 pick = %q, want emp_a", got1)
	}
	jj, _ := a.cronStore.Get(j.ID)
	if jj.LastAssignedIndex != 1 {
		t.Fatalf("rr#1 cursor = %d, want 1", jj.LastAssignedIndex)
	}

	// 第 2 次触发（用落库后的 job 快照，模拟调度器下轮触发）：乙 → 游标 2。
	j2, _ := a.cronStore.Get(j.ID)
	got2, _, err := a.resolveCronEmployee(j2)
	if err != nil {
		t.Fatalf("rr#2: %v", err)
	}
	if got2 != "emp_b" {
		t.Fatalf("rr#2 pick = %q, want emp_b", got2)
	}
	if jj, _ := a.cronStore.Get(j.ID); jj.LastAssignedIndex != 2 {
		t.Fatalf("rr#2 cursor = %d, want 2", jj.LastAssignedIndex)
	}

	// 第 3 次 → 丙，第 4 次回绕 → 甲（模池长，§2.1 轮转语义）。
	// 用落库后的最新游标（模拟调度器每轮 tickOnce 经 store.List 取最新 job）。
	want := []string{"emp_c", "emp_a"}
	cur, _ := a.cronStore.Get(j.ID)
	for i, w := range want {
		got, _, err := a.resolveCronEmployee(cur)
		if err != nil {
			t.Fatalf("rr iter %d: %v", i, err)
		}
		if got != w {
			t.Fatalf("rr iter %d pick = %q, want %q", i, got, w)
		}
		next, _ := a.cronStore.Get(j.ID)
		if next.LastAssignedIndex != i+3 {
			t.Fatalf("rr iter %d cursor = %d, want %d", i, next.LastAssignedIndex, i+3)
		}
		cur = next
	}
}

// TestCronRunner_DegradedSessionNote 分支②：绑定员工不可运行 → 降级池选 +
// 双留痕（slog.Warn + 会话 say 留痕，§2-②/§10）。
func TestCronRunner_DegradedSessionNote(t *testing.T) {
	a := newCronBindingTestApp(t)
	p := seedPosition(t, a, "开发")
	seedCronEmployee(t, a, "emp_off", "丁", p.ID, employee.EmpResigned) // EHR 离职不可运行
	seedCronEmployee(t, a, "emp_b", "乙", p.ID, "")
	j := addBoundJob(t, a, "emp_off", p.ID, "", 0)

	// 预置 cron 会话（降级留痕需要会话已存在；模拟历史触发已建会话）。
	sessID := "cron_" + j.ID
	a.store.Save(&session.Session{ID: sessID, ChannelID: "desktop"})

	_, err := a.cronRunner(j)
	if err != nil {
		t.Fatalf("cronRunner degraded: %v", err)
	}
	s, gerr := a.store.Get(sessID)
	if gerr != nil || s == nil {
		t.Fatalf("会话应存在: %v", gerr)
	}
	// 快照固化：解析出的池内员工 + 岗位 + 规则（§6.1）。
	snap := s.Snapshot()
	if snap.EmployeeID == "" || snap.EmployeeID == "emp_off" {
		t.Fatalf("快照员工应为池内可运行者, got %q", snap.EmployeeID)
	}
	if snap.PositionID != p.ID {
		t.Fatalf("快照岗位 = %q, want %q", snap.PositionID, p.ID)
	}
	// 降级留痕：最后一条消息含降级说明。
	if n := len(snap.Messages); n == 0 || !strings.Contains(snap.Messages[n-1].Text, "降级分配") {
		t.Fatalf("降级事件应留痕会话, msgs=%d", n)
	}
}

// TestCronRunner_EmptyPoolError 池空：显式错误返回（调度器记 failed + 重试/
// 熔断链路），不静默降级、不静默裸跑（§10）；游标不推进。
func TestCronRunner_EmptyPoolError(t *testing.T) {
	a := newCronBindingTestApp(t)
	p := seedPosition(t, a, "空池岗")
	j := addBoundJob(t, a, "", p.ID, "", 0)

	_, _, err := a.resolveCronEmployee(j)
	if err == nil || !strings.Contains(err.Error(), "无可运行数字员工") {
		t.Fatalf("pool-empty must error, got: %v", err)
	}
	// 任务不送执行链：cronRunner 返回错误。
	if _, err := a.cronRunner(j); err == nil {
		t.Fatal("cronRunner pool-empty must fail")
	}
	// 游标未推进。
	if jj, _ := a.cronStore.Get(j.ID); jj.LastAssignedIndex != 0 {
		t.Fatalf("pool-empty cursor = %d, want 0", jj.LastAssignedIndex)
	}
}

// TestCronRunner_NoBindingLegacy 裸跑（§2-④ 存量语义）：绑定全空 → 恒空员工 ID、
// 不写游标、会话快照清空（旧行为不变；SendMessageWithEmployee 等价旧 SendMessage）。
func TestCronRunner_NoBindingLegacy(t *testing.T) {
	a := newCronBindingTestApp(t)
	j := addBoundJob(t, a, "", "", "", 0)

	sessID, err := a.cronRunner(j)
	if err != nil {
		t.Fatalf("bare cronRunner: %v", err)
	}
	if sessID != "cron_"+j.ID {
		t.Fatalf("sessID mismatch: %s", sessID)
	}
	got, degraded, err := a.resolveCronEmployee(j)
	if err != nil || got != "" || degraded {
		t.Fatalf("bare resolve = (%q, %v, %v), want bare-run", got, degraded, err)
	}
	if jj, _ := a.cronStore.Get(j.ID); jj.LastAssignedIndex != 0 {
		t.Fatalf("bare cursor = %d, want 0", jj.LastAssignedIndex)
	}
	// syncCronSession 裸跑：快照员工/岗位均清空（等价旧同步行为）。
	sessID2 := "cron_" + j.ID
	a.store.Save(&session.Session{ID: sessID2, ChannelID: "desktop"})
	a.syncCronSession(sessID2, j, "")
	s, _ := a.store.Get(sessID2)
	if snap := s.Snapshot(); snap.EmployeeID != "" || snap.PositionID != "" || snap.AssignRule != "" {
		t.Fatalf("bare snapshot should be cleared, got %+v", snap)
	}
}

// TestSyncCronSession_SnapshotBinding 员工岗位快照固化（§6.1）：
// empID + job 岗位/规则写入会话；员工调岗后会话快照不漂移（值快照而非引用）。
func TestSyncCronSession_SnapshotBinding(t *testing.T) {
	a := newCronBindingTestApp(t)
	p := seedPosition(t, a, "开发")
	seedCronEmployee(t, a, "emp_a", "甲", p.ID, "")
	j := addBoundJob(t, a, "emp_a", p.ID, employee.AssignRoundRobin, 0)

	sessID := "cron_" + j.ID
	a.store.Save(&session.Session{ID: sessID, ChannelID: "desktop"})
	a.syncCronSession(sessID, j, "emp_a")

	s, err := a.store.Get(sessID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	snap := s.Snapshot()
	if snap.EmployeeID != "emp_a" || snap.PositionID != p.ID || snap.AssignRule != employee.AssignRoundRobin {
		t.Fatalf("snapshot mismatch: %+v", snap)
	}
}

// TestCronRunner_FixedEmployeeSessionBinding 端到端：cronRunner 固定员工 →
// 预置会话被固化快照 + SendMessageWithEmployee 携带员工 ID（运行中入队或
// 异步执行均不报错；会话侧断言快照字段）。
func TestCronRunner_FixedEmployeeSessionBinding(t *testing.T) {
	a := newCronBindingTestApp(t)
	p := seedPosition(t, a, "开发")
	seedCronEmployee(t, a, "emp_a", "甲", p.ID, "")
	j := addBoundJob(t, a, "emp_a", p.ID, "", 0)

	sessID := "cron_" + j.ID
	a.store.Save(&session.Session{ID: sessID, ChannelID: "desktop"})

	if _, err := a.cronRunner(j); err != nil {
		t.Fatalf("cronRunner: %v", err)
	}
	s, err := a.store.Get(sessID)
	if err != nil || s == nil {
		t.Fatalf("session: %v", err)
	}
	snap := s.Snapshot()
	if snap.EmployeeID != "emp_a" || snap.PositionID != p.ID {
		t.Fatalf("session binding mismatch: emp=%q pos=%q", snap.EmployeeID, snap.PositionID)
	}
	// 无降级：无 say 留痕消息。
	for _, m := range snap.Messages {
		if m.Type == message.TypeSay && strings.Contains(m.Text, "降级分配") {
			t.Fatal("non-degraded run must not append degrade note")
		}
	}
}
