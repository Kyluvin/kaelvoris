// channel_assign_test.go 岗位分配×双区权限 Phase 2-① 四渠道收口（doc/09-设计方案）：
//   - resolveAssignmentForChannel 正路径：固定（分支①）/ 池随机（分支③）/
//     round_robin 游标回写并随 channels.json 原子落盘（§2.1）；
//   - 降级留痕（§2-②/§10）：绑定员工不可运行 → 池选取 + 日志 + 会话可见提示
//     （emitAssignNotice 以 say text 落会话历史）；
//   - 裸跑存量语义（分支④）：绑定全空 → 零值结果、不拦不写（启用渠道在保存侧
//     已被拒，运行侧不重复拦截，§4.4）；
//   - 快照固化：applyChannelAssignSnapshot → Session.SetEmployeeID +
//     SetPositionSnapshot（生命周期不重选，§6.1）。
//
// 不依赖真实网络与 Wails 上下文：渠道实例用停用形态手工构造（仅 cfg/app 字段，
// 不 Start 不连网）；渠道路径经 channelsConfigPathOverride 重定向临时目录
// （参照 binding_model_test.go 做法，不碰真实 ~/.kaelvoris/channels.json）。
package desktop

import (
	"strings"
	"testing"

	"kaelvoris/internal/employee"
	"kaelvoris/internal/session"
)

// newAssignTestApp 构造无 Wails 上下文的测试 App + 渠道配置路径重定向
// （直接复用 binding_model_test.go 的 newBindingTestApp；保持独立封装以便
// 本文件后续追加渠道形态字段）。
func newAssignTestApp(t *testing.T) *App {
	t.Helper()
	return newBindingTestApp(t)
}

// TestChannelAssign_FixedEmployee 分支①：绑定员工可运行 → 固定该员工，
// 会话快照 EmployeeID + 岗位 + 规则固化（§4.1/§6.1）。
func TestChannelAssign_FixedEmployee(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "客服岗")
	emp := seedEmployee(t, a, "张三", p.ID)
	cfg := ChannelConfig{Type: "im", Name: "IM固定", EmployeeID: emp.ID, PositionID: p.ID, AssignRule: "random"}

	res, ok := resolveAssignmentForChannel(a, &cfg, "测试")
	if !ok || res.employeeID != emp.ID || res.degraded {
		t.Fatalf("fixed = (%+v, %v), want fixed %s", res, ok, emp.ID)
	}
	if res.positionID != p.ID {
		t.Fatalf("快照岗位 = %q, want %q", res.positionID, p.ID)
	}
	// 快照固化进会话（SetEmployeeID + SetPositionSnapshot，§6.1）。
	sess := &session.Session{ID: "t1", ChannelID: "im"}
	applyChannelAssignSnapshot(sess, res)
	snap := sess.Snapshot()
	if snap.EmployeeID != emp.ID || snap.PositionID != p.ID || snap.AssignRule != "random" {
		t.Fatalf("会话快照固化失败: %+v", snap)
	}
}

// TestChannelAssign_PositionPoolRandom 分支③：只绑岗位 random → 池选一人，
// 游标不推进（random 恒 0，§2.1）。
func TestChannelAssign_PositionPoolRandom(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "客服池")
	seedEmployee(t, a, "甲", p.ID)
	seedEmployee(t, a, "乙", p.ID)
	cfg := ChannelConfig{Type: "im", Name: "IM池选", PositionID: p.ID}

	res, ok := resolveAssignmentForChannel(a, &cfg, "测试")
	if !ok || res.employeeID == "" || res.degraded {
		t.Fatalf("pool random = (%+v, %v)", res, ok)
	}
	if res.poolSize != 2 {
		t.Fatalf("池规模 = %d, want 2", res.poolSize)
	}
	// 游标不落盘（random 恒 0；未保存渠道配置，断言无任何写入副作用）。
	chs := a.ListChannels()
	if len(chs) != 0 {
		t.Fatalf("random 不应写渠道配置/游标: %+v", chs)
	}
}

// TestChannelAssign_RoundRobinCursor 分支③ round_robin：游标随解析推进并
// 回写 channels.json（Type+Name 定位整表替换，§2.1 原子落盘）。
func TestChannelAssign_RoundRobinCursor(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "轮转岗")
	empA := seedEmployee(t, a, "甲", p.ID)
	empB := seedEmployee(t, a, "乙", p.ID)
	if err := a.SaveChannels([]ChannelConfig{{Type: "im", Name: "IM轮转", Enabled: true, WebhookURL: "https://e.com/h", PositionID: p.ID, AssignRule: employee.AssignRoundRobin}}); err != nil {
		t.Fatalf("渠道保存: %v", err)
	}
	// 池序 = 员工表创建序（保序语义，resolve.go pickFromPool 保持原始顺序）。
	wantPick := []string{empA.ID, empB.ID, empA.ID}
	for i, want := range wantPick {
		cfg := ChannelConfig{Type: "im", Name: "IM轮转", PositionID: p.ID, AssignRule: employee.AssignRoundRobin, LastAssignedIndex: i}
		res, ok := resolveAssignmentForChannel(a, &cfg, "测试")
		if !ok || res.employeeID != want {
			t.Fatalf("rr#%d = (%+v, %v), want %s", i+1, res, ok, want)
		}
		// 游标已随 channels.json 落盘推进（i+1）。
		got := a.ListChannels()
		if len(got) != 1 || got[0].LastAssignedIndex != i+1 {
			t.Fatalf("rr#%d 游标落盘 = %+v, want %d", i+1, got, i+1)
		}
	}
}

// TestChannelAssign_Degraded 分支②：绑定员工不可运行 → 按分配规则从岗位池降级
// 选取 + 双留痕（slog.Warn 日志 + 会话可见提示，§10）。
func TestChannelAssign_Degraded(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "离职岗")
	// 直接经 Store 造离职员工（EHR 状态不可运行）+ 池内可运行替补。
	off, err := a.empStore.Add(employee.Employee{Name: "丁", PositionID: p.ID, EmpStatus: employee.EmpResigned})
	if err != nil {
		t.Fatalf("离职员工构造: %v", err)
	}
	backup := seedEmployee(t, a, "替补", p.ID)
	cfg := ChannelConfig{Type: "lark", Name: "Lark降级", EmployeeID: off.ID, PositionID: p.ID}

	res, ok := resolveAssignmentForChannel(a, &cfg, "测试")
	if !ok || !res.degraded {
		t.Fatalf("degraded = (%+v, %v), want degraded", res, ok)
	}
	if res.employeeID != backup.ID {
		t.Fatalf("降级选取 = %q, want %q", res.employeeID, backup.ID)
	}
	if res.notice == "" || !containsAny(res.notice, "降级", "不可运行") {
		t.Fatalf("降级留痕提示缺失: %q", res.notice)
	}
	// 固化快照含降级提示（emitAssignNotice 把 notice 以 say text 落会话历史）。
	sess := &session.Session{ID: "deg", ChannelID: "lark"}
	applyChannelAssignSnapshot(sess, res)
	run := &taskRun{id: "deg", todo: nil}
	emitAssignNotice(a, sess, run, "deg", res.notice)
	snap := sess.Snapshot()
	if snap.EmployeeID != res.employeeID {
		t.Fatalf("降级会话快照员工 = %q, want %q", snap.EmployeeID, res.employeeID)
	}
	if n := len(snap.Messages); n == 0 || !strings.Contains(snap.Messages[n-1].Text, "降级") {
		t.Fatalf("降级事件应留痕会话（say text）, msgs=%d", n)
	}
}

// TestChannelAssign_BareRunLegacy 分支④：绑定全空（裸跑）→ 零值结果、ok=true、
// 不报错不写游标（启用渠道在 SaveChannels 侧已被拒，运行侧不重复拦截，§4.4）。
func TestChannelAssign_BareRun(t *testing.T) {
	a := newBindingTestApp(t)
	cfg := ChannelConfig{Type: "im", Name: "裸跑渠道"} // 绑定全空
	res, ok := resolveAssignmentForChannel(a, &cfg, "测试")
	if !ok || res.employeeID != "" || res.positionID != "" || res.degraded {
		t.Fatalf("bare = (%+v, %v), want 零值结果", res, ok)
	}
	// 快照固化对裸跑恒空（会话保持普通会话语义，不拦不写）。
	sess := &session.Session{ID: "bare", ChannelID: "im"}
	applyChannelAssignSnapshot(sess, res)
	snap := sess.Snapshot()
	if snap.EmployeeID != "" || snap.PositionID != "" || snap.AssignRule != "" {
		t.Fatalf("裸跑会话快照应为空: %+v", snap)
	}
	// channels.json 无游标写入。
	if chs := a.ListChannels(); len(chs) != 0 {
		t.Fatalf("裸跑不应触碰渠道配置: %+v", chs)
	}
}

// TestChannelAssign_EmptyPoolError 池空：显式报错不静默降级（§2 池空语义），
// 错误经 errMsg 带回触发方（渠道回执给用户），游标不推进。
func TestChannelAssign_EmptyPoolError(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "空池触发岗")
	cfg := ChannelConfig{Type: "wecom", Name: "Wecom空池", PositionID: p.ID}

	res, ok := resolveAssignmentForChannel(a, &cfg, "测试")
	if ok || res.errMsg == "" || !containsAny(res.errMsg, "无可运行数字员工") {
		t.Fatalf("池空应报错: (%+v, %v)", res, ok)
	}
	if chs := a.ListChannels(); len(chs) != 0 {
		t.Fatalf("池空不应写游标: %+v", chs)
	}
}

// TestChannelAssign_CursorSurvivesPanelSave 并发防护验证：游标回写后面板
// SaveChannels 整表重存不丢游标（channelsFileMutex 序列化语义）。
func TestChannelAssign_CursorSurvivesPanelSave(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "并发岗")
	empA := seedEmployee(t, a, "甲", p.ID)
	seedEmployee(t, a, "乙", p.ID)
	if err := a.SaveChannels([]ChannelConfig{{Type: "im", Name: "IM并发", Enabled: true, WebhookURL: "https://e.com/h", PositionID: p.ID, AssignRule: employee.AssignRoundRobin}}); err != nil {
		t.Fatalf("渠道保存: %v", err)
	}
	// 解析（游标推进落盘 1；PositionID 从落盘配置带出，避免裸跑分支）。
	cfg := ChannelConfig{Type: "im", Name: "IM并发", PositionID: p.ID, AssignRule: employee.AssignRoundRobin}
	if res, ok := resolveAssignmentForChannel(a, &cfg, "测试"); !ok || res.employeeID != empA.ID {
		t.Fatalf("rr#1 = (%+v, %v), want %s", res, ok, empA.ID)
	}
	// 面板重存（模拟 SaveChannels 并发保存，不含游标字段变更）。
	chs := a.ListChannels()
	if err := a.SaveChannels(chs); err != nil {
		t.Fatalf("面板重存: %v", err)
	}
	// 游标仍为 1（回写与保存互斥，先回写后保存不丢）。
	if got := a.ListChannels(); len(got) != 1 || got[0].LastAssignedIndex != 1 {
		t.Fatalf("游标应随保存保留: %+v", got)
	}
}

// containsAny 任一子串命中（测试断言辅助）。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
