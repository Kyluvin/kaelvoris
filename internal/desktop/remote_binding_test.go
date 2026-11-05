// remote_binding_test.go 岗位分配×双区权限 Phase 2-G（doc/09-设计方案 §4.3）：
//   - 群聊链路（runGroupEmployeeTask）双区 RoleSection 断言：empWS/sessionWS
//     均传入、同目录收敛单区、员工直选不走岗位池（§4.3 表注）；
//   - 群会话快照落盘：Session.EmployeeID + PositionID（§2 固化语义，@员工
//     直选语义下 positionId 取员工档案当前岗位）；
//   - runRemoteTask 绑定解析三态：绑定员工可运行（固定）/ 绑定员工不可运行
//     （降级换人）/ 全空（裸跑存量语义），解析结果经会话快照固化可落盘回读；
//   - 在途修复防回退：群聊 panic 防护 + 解析器接线源码断言。
//
// 不依赖 Wails 上下文与网络：解析器为纯函数（internal/employee/resolve.go 文件
// 头「调用方固化会话快照」约定），测试以「等价入参 → 同一解析器」构造三态断言，
// 临时目录 Store 验证快照持久化；不真实触发 LLM/引擎轮次。
package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kaelvoris/internal/employee"
	"kaelvoris/internal/remote"
	"kaelvoris/internal/session"
)

// TestGroupChain_RoleSectionDualZone 群聊双区（§4.3 群聊行/§7.1 缺口①）：
// runGroupEmployeeTask 的 RoleSection 双区调用语义——员工区（empWS）+ 会话区
// （sessionWS=run.workspace）双参传入；两区同目录时 RoleSection 内部收敛单区
// （存量行为零差异），两区不同目录时双区文案齐备（员工自定义 WorkspaceRoot 场景，
// 网关 dirAllowed OR 语义双区同时可达，网关单测见 internal/employee/gateway 包既有测试，不重复建）。
func TestGroupChain_RoleSectionDualZone(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "群聊岗")
	emp := seedEmployee(t, a, "群成员甲", p.ID)

	empWS := emp.Workspace(a.empStore.Dir())

	// 双区同目录（群任务现状：会话工作区 = 员工工作空间）→ 收敛单区旧文案。
	same := emp.RoleSection(empWS, empWS)
	if !strings.Contains(same, "专属工作空间") {
		t.Fatalf("同目录收敛应保留员工区文案: %q", same)
	}
	if strings.Contains(same, "当前任务工作区") {
		t.Fatalf("同目录收敛不应出现双区边界文案: %q", same)
	}

	// 双区不同目录（员工自定义工作空间，群会话工作区独立）：两区均声明。
	emp.WorkspaceRoot = filepath.Join(a.empStore.Dir(), "custom", emp.ID)
	dualEmpWS := emp.Workspace(a.empStore.Dir())
	sessionWS := filepath.Join(t.TempDir(), "group-session-ws")
	dual := emp.RoleSection(dualEmpWS, sessionWS)
	if !strings.Contains(dual, "当前任务工作区") || !strings.Contains(dual, sessionWS) {
		t.Fatalf("会话区（工具 cwd）应声明: %q", dual)
	}
	if !strings.Contains(dual, "专属工作空间") || !strings.Contains(dual, dualEmpWS) {
		t.Fatalf("员工区（记忆/进化沉淀）应声明: %q", dual)
	}
	_ = same
	_ = empWS
}

// TestGroupChain_SessionSnapshotPersisted 群会话快照（§2 固化语义 + §4.3）：
// 解析出员工后 Session.EmployeeID + PositionID（员工当前岗位）固化落盘；
// 会话不存在时惰性建轻量记录（ChannelID=group）；存量会话复用路径幂等回写。
func TestGroupChain_SessionSnapshotPersisted(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "快照岗")
	emp := seedEmployee(t, a, "快照员", p.ID)

	// 群任务员工执行链的快照落盘段（runGroupEmployeeTask 内联语义）：
	// store 无记录 → 惰性建会话并固化 EmployeeID + 岗位快照。
	sessionID := "group-g1-gt1-" + emp.ID
	ws := emp.Workspace(a.empStore.Dir())
	if _, err := a.store.Get(sessionID); err == nil {
		t.Fatalf("前置：快照前会话应不存在")
	}
	a.store.Save(&session.Session{
		ID:         sessionID,
		ChannelID:  "group",
		Mode:       "code",
		Workspace:  ws,
		EmployeeID: emp.ID,
		PositionID: emp.PositionID,
	})
	// 快照回读（store.Get 磁盘往返，验落盘不丢）。
	got, err := a.store.Get(sessionID)
	if err != nil || got == nil {
		t.Fatalf("群会话快照未落盘: %v", err)
	}
	back := got.Snapshot()
	if back.EmployeeID != emp.ID || back.PositionID != p.ID {
		t.Fatalf("快照字段缺失: emp=%q pos=%q", back.EmployeeID, back.PositionID)
	}
	if back.ChannelID != "group" {
		t.Fatalf("群会话 ChannelID 应为 group: %q", back.ChannelID)
	}
	// 存量会话复用路径：SetEmployeeID + SetPositionSnapshot 幂等回写。
	got.SetEmployeeID(emp.ID)
	got.SetPositionSnapshot(emp.PositionID, "")
	a.store.Save(got)
	if s2, _ := a.store.Get(sessionID); s2.Snapshot().EmployeeID != emp.ID || s2.Snapshot().PositionID != p.ID {
		t.Fatalf("复用路径快照回写失败: %+v", s2.Snapshot())
	}
}

// TestRunRemoteTask_ResolveBindingThreeStates Phase 2-④（§4.3 远程行）：
// runRemoteTask 的 ResolveAssignment 三态语义（等价入参直调同一解析器 + 会话
// 快照固化断言；不真实起引擎轮次）：
//   - 绑定员工可运行 → 固定该员工（分支①）；
//   - 绑定员工不可运行 → 降级从岗位池换人（分支②，§11-1 推荐）；
//   - 全空 → 裸跑存量语义（分支④），岗位快照清空。
func TestRunRemoteTask_ResolveBindingThreeStates(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "远程岗")
	empOn := seedEmployee(t, a, "在线员工", p.ID)
	// 不可运行员工（停用，同岗位池另有人）。
	empOff, err := a.empStore.Add(employee.Employee{Name: "停用员工", PositionID: p.ID})
	if err != nil {
		t.Fatalf("停用员工构造: %v", err)
	}
	if _, err := a.SetEmployeeStatus(empOff.ID, employee.StatusDisabled); err != nil {
		t.Fatalf("停用: %v", err)
	}

	// ① 固定：绑定员工可运行 → 固定该员工。
	r, _, err := employee.ResolveAssignment(a.empStore.List(), employee.Assignment{EmployeeID: empOn.ID, PositionID: p.ID})
	if err != nil || r.EmployeeID != empOn.ID || r.Degraded {
		t.Fatalf("分支①固定解析失败: %v %v", r, err)
	}

	// ② 降级：绑定员工不可运行 → 按规则从岗位池选取（池 = 绑定岗位）。
	r2, _, err := employee.ResolveAssignment(a.empStore.List(), employee.Assignment{EmployeeID: empOff.ID, PositionID: p.ID})
	if err != nil {
		t.Fatalf("分支②降级解析失败: %v", err)
	}
	if !r2.Degraded || r2.EmployeeID == "" || r2.EmployeeID == empOff.ID {
		t.Fatalf("分支②应降级换人: %+v", r2)
	}
	if r2.EmployeeID != empOn.ID {
		t.Fatalf("降级应命中同岗位池内可运行员工: %+v", r2)
	}

	// ③ 只绑岗位：岗位池选取（分支③）。
	r3, _, err := employee.ResolveAssignment(a.empStore.List(), employee.Assignment{PositionID: p.ID})
	if err != nil || r3.EmployeeID != empOn.ID {
		t.Fatalf("分支③岗位池解析失败: %+v %v", r3, err)
	}

	// ④ 裸跑：全空 → 零值结果（存量语义，不强制）。
	r4, _, err := employee.ResolveAssignment(a.empStore.List(), employee.Assignment{})
	if err != nil || r4.EmployeeID != "" {
		t.Fatalf("分支④裸跑应零值通过: %+v %v", r4, err)
	}

	// 快照固化（解析结果 → Session）：与 runRemoteTask 内联路径同构。
	sess := &session.Session{ID: "remote-snap-1", ChannelID: "remote", Workspace: "D:/ws"}
	sess.SetEmployeeID(r.EmployeeID)
	sess.SetPositionSnapshot(r.Employee.PositionID, "")
	a.store.Save(sess)
	if s2, _ := a.store.Get("remote-snap-1"); s2.Snapshot().EmployeeID != r.EmployeeID || s2.Snapshot().PositionID != p.ID {
		t.Fatalf("远程会话快照未固化: %+v", s2.Snapshot())
	}

	// 解析失败负路径：绑定不存在的员工 → 显式报错（不静默裸跑，§2 池空语义同源）。
	if _, _, err := employee.ResolveAssignment(a.empStore.List(), employee.Assignment{EmployeeID: "emp_ghost", PositionID: p.ID}); err == nil {
		t.Fatalf("绑定不存在员工应报错")
	}
}

// TestGroupChain_PanicGuardPreserved 在途 panic 防护回归（并发会话修复不许回退）：
// remoteGroupTask 的接力执行 goroutine 带 recover 防护——runGroupChain 调用点
// 被 defer/recover 包裹，panic 转 finishGroupTask 失败落群（「一@员工就闪退」修复）。
func TestGroupChain_PanicGuardPreserved(t *testing.T) {
	src := string(readSource(t, "remote_group.go"))
	// 2026-10 多模式改造：runGroupChain 新增 mode 参数，调用点同步更新。
	for _, frag := range []string{"群聊任务链 panic（已拦截）", "runGroupChain(st, grp.ID, task, assigned, mode)", "recover()"} {
		if !strings.Contains(src, frag) {
			t.Fatalf("群聊 panic 防护结构缺失片段: %q", frag)
		}
	}
	// 双区 RoleSection 调用在位（单参旧版不得回归）。
	if !strings.Contains(src, "emp.RoleSection(ws, run.workspace)") {
		t.Fatalf("群聊双区 RoleSection 调用缺失（须为 empWS, sessionWS 双参）")
	}
	// 会话快照固化在位。
	if !strings.Contains(src, "SetPositionSnapshot(emp.PositionID") {
		t.Fatalf("群会话岗位快照固化缺失")
	}
}

// TestRunRemoteTask_ResolverWiring runRemoteTask 解析器接线源码断言（防回退）：
// Phase 2-④ 的三要素在位——ResolveAssignment 接入、岗位快照固化、员工回写
// taskRun（员工内核/双区网关生效路径）。
func TestRunRemoteTask_ResolverWiring(t *testing.T) {
	src := string(readSource(t, "remote_panel.go"))
	for _, frag := range []string{
		"employee.ResolveAssignment(a.empStore.List()",
		"sess.SetPositionSnapshot(r.Employee.PositionID, strings.TrimSpace(sess.Snapshot().AssignRule))",
		"AssignRule: strings.TrimSpace(sess.Snapshot().AssignRule)", // 快照规则透传（降级/岗位池重选时沿用，防退化为 random）
		"employeeID: sess.Snapshot().EmployeeID",
	} {
		if !strings.Contains(src, frag) {
			t.Fatalf("runRemoteTask 解析器接线缺失片段: %q", frag)
		}
	}
}

// readSource 读取工作区根包源文件（测试断言用；os.ReadFile 薄封装）。
func readSource(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// groupTaskFixture 群任务最小构造（防 remote 包 import 漂移的编译期锚点）。
func groupTaskFixture(gid, instruction string) remote.GroupTask {
	return remote.GroupTask{GroupID: gid, Instruction: instruction, Status: "running"}
}

var _ = groupTaskFixture
