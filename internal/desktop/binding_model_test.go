// binding_model_test.go 岗位分配×双区权限 Phase 1-D（doc/09-设计方案）：
//   - ChannelConfig 绑定字段（PositionID/AssignRule/LastAssignedIndex，§3）
//     旧 JSON 兼容（omitempty 缺省可解析、往返不丢）；
//   - SaveChannels §4.4 保存校验正负路径：裸跑拒绝 / 员工缺岗位拒绝 /
//     规则非法拒绝 / 岗位与员工不一致拒绝 / 回填岗位快照 / 落盘往返；
//   - SaveEmployee 岗位必填（§3/§4.4，存量不阻断——直接走 Store 的旧数据不受影响）；
//   - DeletePosition 资源占用门禁（渠道 / cron 引用时拒绝并列出来源，§4 验收）；
//   - ListResourceBindings / SaveResourceBinding 聚合读写往返（§5.6）。
//
// 不依赖 Wails 上下文：App 仅构造 store/cronStore/empStore/orgStore；
// 渠道路径经 channelsConfigPathOverride 重定向临时目录（不碰真实
// ~/.kaelvoris/channels.json）。停用渠道不触发任何通道启动（startChannels
// 只对 Enabled 渠道起 goroutine），reloadChannels 对测试配置无网络副作用。
package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kaelvoris/internal/cron"
	"kaelvoris/internal/employee"
	"kaelvoris/internal/session"
)

// newBindingTestApp 构造无 Wails 上下文的测试 App（含员工/组织/cron 存储
// 与渠道配置路径重定向；各测试独立临时目录，互不污染）。
func newBindingTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	channelsConfigPathOverride = filepath.Join(dir, "channels.json")
	t.Cleanup(func() { channelsConfigPathOverride = "" })
	st := session.NewStore(filepath.Join(dir, "sessions"))
	t.Cleanup(st.Close) // 停后台刷盘，防 t.TempDir 清理与 flusher 写盘竞态（Windows RemoveAll 非空报错）
	cs, err := cron.NewStore(filepath.Join(dir, "cron"))
	if err != nil {
		t.Fatalf("cron store: %v", err)
	}
	emp, err := employee.NewStore(filepath.Join(dir, "employees"))
	if err != nil {
		t.Fatalf("employee store: %v", err)
	}
	org, err := employee.NewOrgStore(filepath.Join(dir, "employees"))
	if err != nil {
		t.Fatalf("org store: %v", err)
	}
	return &App{store: st, cronStore: cs, empStore: emp, orgStore: org}
}

// seedPosition 建岗位字典。
func seedPosition(t *testing.T, a *App, name string) employee.Position {
	t.Helper()
	p, err := a.SavePosition(employee.Position{Name: name})
	if err != nil {
		t.Fatalf("SavePosition(%s): %v", name, err)
	}
	return p
}

// seedEmployee 建可运行员工（绑定指定岗位）。
func seedEmployee(t *testing.T, a *App, name, positionID string) employee.Employee {
	t.Helper()
	e, err := a.SaveEmployee(employee.Employee{Name: name, PositionID: positionID})
	if err != nil {
		t.Fatalf("SaveEmployee(%s): %v", name, err)
	}
	return e
}

// TestChannelConfig_BindingJSONCompat §3：旧 JSON（无绑定字段）反序列化
// 零值合法；新字段序列化/反序列化往返不丢（omitempty 兼容旧数据）。
func TestChannelConfig_BindingJSONCompat(t *testing.T) {
	legacy := `[{"type":"im","name":"旧渠道","webhookUrl":"https://e.com/h"}]`
	var out []ChannelConfig
	if err := json.Unmarshal([]byte(legacy), &out); err != nil {
		t.Fatalf("旧 JSON 兼容解析失败: %v", err)
	}
	if len(out) != 1 || out[0].PositionID != "" || out[0].AssignRule != "" || out[0].LastAssignedIndex != 0 {
		t.Fatalf("旧数据应解析为零值绑定字段: %+v", out)
	}
	b, err := json.Marshal(ChannelConfig{Type: "im", Name: "n", PositionID: "p1", AssignRule: "round_robin", LastAssignedIndex: 5})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ChannelConfig
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.PositionID != "p1" || back.AssignRule != "round_robin" || back.LastAssignedIndex != 5 {
		t.Fatalf("绑定字段往返丢失: %s", b)
	}
	// 未绑定时 omitempty 不落 JSON（旧消费方零感知）。
	min, _ := json.Marshal(ChannelConfig{Type: "im", Name: "n"})
	if strings.Contains(string(min), "positionId") || strings.Contains(string(min), "lastAssignedIndex") {
		t.Fatalf("未绑定字段不应序列化: %s", min)
	}
}

// TestSaveChannels_RejectBareRun §4.4/§2-④：启用渠道全空绑定（裸跑）拒绝保存；
// 停用渠道全空 = 存量语义放行。
func TestSaveChannels_RejectBareRun(t *testing.T) {
	a := newBindingTestApp(t)
	if err := a.SaveChannels([]ChannelConfig{{Type: "im", Name: "裸跑", Enabled: true, WebhookURL: "https://e.com/h"}}); err == nil || !strings.Contains(err.Error(), "裸跑") {
		t.Fatalf("启用裸跑渠道应拒绝: %v", err)
	}
	if err := a.SaveChannels([]ChannelConfig{{Type: "im", Name: "停用", WebhookURL: "https://e.com/h"}}); err != nil {
		t.Fatalf("停用渠道全空绑定应放行: %v", err)
	}
}

// TestSaveChannels_EmployeeMissingPosition §4.4「指定员工须有岗位」：绑定
// 未分配岗位的员工拒绝保存（含 SaveEmployee 岗位必填前置校验）。
func TestSaveChannels_EmployeeMissingPosition(t *testing.T) {
	a := newBindingTestApp(t)
	// 直接经 Store 造存量无岗员工（绕过面板校验，模拟存量数据）。
	noPos, err := a.empStore.Add(employee.Employee{Name: "存量无岗"})
	if err != nil {
		t.Fatalf("存量无岗员工构造: %v", err)
	}
	if _, err := a.SaveEmployee(employee.Employee{Name: "面板缺岗"}); err == nil || !strings.Contains(err.Error(), "岗位") {
		t.Fatalf("SaveEmployee 岗位必填校验失效: %v", err)
	}
	if err := a.SaveChannels([]ChannelConfig{{Type: "im", Name: "IM", Enabled: true, WebhookURL: "https://e.com/h", EmployeeID: noPos.ID}}); err == nil || !strings.Contains(err.Error(), "岗位") {
		t.Fatalf("绑定无岗员工应拒绝: %v", err)
	}
}

// TestSaveChannels_BackfillSnapshot §3：绑定员工保存自动回填员工当前岗位为
// positionId 快照；round_robin 游标随配置落盘往返。
func TestSaveChannels_BackfillSnapshot(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "后端工程师")
	emp := seedEmployee(t, a, "张三", p.ID)
	ch := ChannelConfig{Type: "im", Name: "IM", Enabled: true, WebhookURL: "https://e.com/h", EmployeeID: emp.ID}
	if err := a.SaveChannels([]ChannelConfig{ch}); err != nil {
		t.Fatalf("合法绑定应保存成功: %v", err)
	}
	got := a.ListChannels()
	if len(got) != 1 || got[0].PositionID != p.ID {
		t.Fatalf("岗位快照应回填并落盘: %+v", got)
	}
	// 游标 + 规则随配置落盘往返（§2.1 ponytail：游标落盘即可）。
	got[0].AssignRule = "round_robin"
	got[0].LastAssignedIndex = 7
	if err := a.SaveChannels(got); err != nil {
		t.Fatalf("游标落盘: %v", err)
	}
	got = a.ListChannels()
	if got[0].AssignRule != "round_robin" || got[0].LastAssignedIndex != 7 {
		t.Fatalf("游标/规则往返失败: %+v", got[0])
	}
}

// TestSaveChannels_NegativePaths §4.4 负路径集合：分配规则非法 / 岗位不存在 /
// 员工不存在 / 员工不可运行 / 绑定岗位与员工当前岗位不一致。
func TestSaveChannels_NegativePaths(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "测试岗")
	emp := seedEmployee(t, a, "李四", p.ID)
	other := seedPosition(t, a, "其他岗")
	base := ChannelConfig{Type: "im", Name: "IM", Enabled: true, WebhookURL: "https://e.com/h"}

	cases := []struct {
		name string
		mut  func(*ChannelConfig)
		want string // 错误信息须含的关键词
	}{
		{"规则非法", func(c *ChannelConfig) { c.AssignRule = "chaos" }, "分配规则"},
		{"岗位不存在", func(c *ChannelConfig) { c.PositionID = "pos_ghost" }, "岗位不存在"},
		{"员工不存在", func(c *ChannelConfig) { c.EmployeeID = "emp_ghost" }, "不存在"},
		{"员工停用", func(c *ChannelConfig) {
			c.EmployeeID = emp.ID
			c.PositionID = p.ID
			c.Enabled = false
			c.EmployeeID = emp.ID
		}, ""},
	}
	// 员工停用：先造停用员工再校验（Runnable=false → 渠道不就绪）。
	off, _ := a.empStore.Add(employee.Employee{Name: "停用者", PositionID: p.ID})
	if _, err := a.SetEmployeeStatus(off.ID, employee.StatusDisabled); err != nil {
		t.Fatalf("停用员工: %v", err)
	}
	offCh := base
	offCh.EmployeeID = off.ID
	offCh.PositionID = p.ID
	if err := a.SaveChannels([]ChannelConfig{offCh}); err == nil || !strings.Contains(err.Error(), "未启用") {
		t.Fatalf("绑定停用员工应拒绝: %v", err)
	}
	// 绑定员工 + 岗位与员工当前岗位不一致（§3 调岗不自动跟随）。
	mix := base
	mix.EmployeeID = emp.ID
	mix.PositionID = other.ID
	if err := a.SaveChannels([]ChannelConfig{mix}); err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("绑定岗位与员工当前岗位不一致应拒绝: %v", err)
	}
	for _, tc := range cases {
		if tc.want == "" {
			continue // 占位用例跳过（停用员工已单独断言）
		}
		c := base
		tc.mut(&c)
		if err := a.SaveChannels([]ChannelConfig{c}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s 应拒绝: %v", tc.name, err)
		}
	}
	_ = other
}

// TestSaveChannels_PositionOnlyBinding §2-③/§4.4：仅绑岗位（池空允许保存，
// 触发时兜底报错），岗位须存在。
func TestSaveChannels_PositionOnlyBinding(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "轮转岗")
	ch := ChannelConfig{Type: "im", Name: "IM", Enabled: true, WebhookURL: "https://e.com/h", PositionID: p.ID, AssignRule: "round_robin"}
	if err := a.SaveChannels([]ChannelConfig{ch}); err != nil {
		t.Fatalf("仅岗位绑定应保存成功: %v", err)
	}
	if got := a.ListChannels(); len(got) != 1 || got[0].PositionID != p.ID || got[0].AssignRule != "round_robin" {
		t.Fatalf("岗位绑定未落盘: %+v", got)
	}
	if err := a.SaveChannels([]ChannelConfig{ChannelConfig{Type: "im", Name: "IM2", Enabled: true, WebhookURL: "https://e.com/h", PositionID: "pos_ghost"}}); err == nil || !strings.Contains(err.Error(), "岗位不存在") {
		t.Fatalf("岗位不存在应拒绝: %v", err)
	}
}

// TestDeletePosition_OccupiedReject §4.4/§5.5-③：岗位被 ChannelConfig /
// cron.Job 引用时拒绝删除，错误信息列出占用来源；员工引用沿用 OrgStore
// 既有校验；无占用正常删除。
func TestDeletePosition_OccupiedReject(t *testing.T) {
	a := newBindingTestApp(t)
	pChan := seedPosition(t, a, "渠道岗")
	pJob := seedPosition(t, a, "任务岗")
	pOther := seedPosition(t, a, "旁岗")
	emp := seedEmployee(t, a, "王五", pOther.ID) // 员工引用其它岗位，不干扰资源项门禁验证
	_ = emp

	// 渠道 + cron 同时占用 → 错误列出两类来源。
	ch := ChannelConfig{Type: "im", Name: "占用渠道", Enabled: true, WebhookURL: "https://e.com/h", PositionID: pJob.ID}
	if err := a.SaveChannels([]ChannelConfig{ch}); err != nil {
		t.Fatalf("渠道保存: %v", err)
	}
	if _, err := a.cronStore.Add(cron.Job{Name: "占用任务", Expr: "@daily", Task: "跑", PositionID: pJob.ID}); err != nil {
		t.Fatalf("cron 保存: %v", err)
	}
	err := a.DeletePosition(pJob.ID)
	if err == nil || !strings.Contains(err.Error(), "定时任务") || !strings.Contains(err.Error(), "占用渠道") {
		t.Fatalf("占用岗位应拒绝并列出来源: %v", err)
	}
	// cron 仍在 → 拒绝（未解绑渠道前不删除）；pChan 无占用可删。
	if err := a.DeletePosition(pChan.ID); err != nil {
		t.Fatalf("无占用岗位删除应成功: %v", err)
	}
	// 解绑 cron → 仅剩渠道占用。
	jobs := a.cronStore.List()
	jobs[0].PositionID = ""
	if _, err := a.cronStore.Update(jobs[0]); err != nil {
		t.Fatalf("cron 解绑: %v", err)
	}
	if err := a.DeletePosition(pJob.ID); err == nil || !strings.Contains(err.Error(), "渠道") {
		t.Fatalf("渠道占用应拒绝: %v", err)
	}

	// 会话纯岗位绑定（EmployeeID 空 + PositionID 非空）是活引用：触发时从
	// 岗位池选人，岗位被删后报「岗位下无可运行数字员工」无根因提示。
	sessPos := &session.Session{ID: "sess-del-pos", Title: "纯岗位会话", PositionID: pJob.ID, CreatedAt: time.Now()}
	a.store.Save(sessPos)
	chs := a.ListChannels()
	chs[0].Enabled = false
	chs[0].PositionID = ""
	if err := a.SaveChannels(chs); err != nil {
		t.Fatalf("渠道停用解绑: %v", err)
	}
	if err := a.DeletePosition(pJob.ID); err == nil || !strings.Contains(err.Error(), "会话") {
		t.Fatalf("会话纯岗位绑定应拒绝删除: %v", err)
	}

	// 清会话引用后删除成功。员工+岗位快照会话（EmployeeID 非空）不拦：
	// 岗位仅快照展示，员工引用由 DeleteEmployee 拦。
	got, _ := a.store.Get(sessPos.ID)
	got.PositionID = ""
	a.store.Save(got)
	if err := a.DeletePosition(pJob.ID); err != nil {
		t.Fatalf("解绑后删除应成功: %v", err)
	}
	// 员工引用仍被 OrgStore 拒绝（既有校验路径未回退）。
	pRef := seedPosition(t, a, "员工引用岗")
	if _, err := a.SaveEmployee(employee.Employee{Name: "引用者", PositionID: pRef.ID}); err != nil {
		t.Fatalf("员工保存: %v", err)
	}
	if err := a.DeletePosition(pRef.ID); err == nil || !strings.Contains(err.Error(), "员工") {
		t.Fatalf("员工引用岗位应拒绝删除: %v", err)
	}
}

// TestResourceBindings_RoundTrip §5.6：ListResourceBindings 聚合渠道+cron
// 两类资源绑定；SaveResourceBinding 写回与资源项配置同一份数据（§3.1 等效
// 绑定语义），非法资源类型/ID 拒绝。
func TestResourceBindings_RoundTrip(t *testing.T) {
	a := newBindingTestApp(t)
	pA := seedPosition(t, a, "绑定岗A")
	pB := seedPosition(t, a, "绑定岗B")
	empA := seedEmployee(t, a, "钱七", pA.ID)

	// 渠道往返：先绑定（回填快照）→ List 可见 → 改绑岗位。
	if err := a.SaveChannels([]ChannelConfig{{Type: "im", Name: "RT渠道", Enabled: true, WebhookURL: "https://e.com/h", EmployeeID: empA.ID}}); err != nil {
		t.Fatalf("渠道保存: %v", err)
	}
	if err := a.SaveResourceBinding("channel", "im:RT渠道", "", pA.ID, "random"); err != nil {
		t.Fatalf("渠道改绑岗位: %v", err)
	}
	got := a.ListChannels()
	if len(got) != 1 || got[0].EmployeeID != "" || got[0].PositionID != pA.ID || got[0].AssignRule != "random" {
		t.Fatalf("渠道绑定写回未生效: %+v", got)
	}
	if ch := a.ListChannels()[0]; ch.Enabled && ch.PositionID == "" {
		t.Fatalf("启用渠道岗位绑定丢失")
	}

	// cron 往返。
	job, err := a.cronStore.Add(cron.Job{Name: "RT任务", Expr: "@hourly", Task: "跑"})
	if err != nil {
		t.Fatalf("cron 添加: %v", err)
	}
	if err := a.SaveResourceBinding("cron", job.ID, "", pB.ID, "round_robin"); err != nil {
		t.Fatalf("cron 绑定写回: %v", err)
	}
	if j, _ := a.cronStore.Get(job.ID); j.PositionID != pB.ID || j.AssignRule != "round_robin" {
		t.Fatalf("cron 绑定未落库: %+v", j)
	}
	// cron 解绑 = 裸跑合法（存量语义）。
	if err := a.SaveResourceBinding("cron", job.ID, "", "", ""); err != nil {
		t.Fatalf("cron 解绑应放行: %v", err)
	}

	// 聚合视图包含两类资源（无会话场景：无会话数据时 session 不出现）。
	bindings := a.ListResourceBindings()
	if len(bindings) != 2 {
		t.Fatalf("应聚合 2 条资源绑定: %+v", bindings)
	}
	seen := map[string]ResourceBinding{}
	for _, b := range bindings {
		seen[b.Type] = b
	}
	if cb, ok := seen["channel"]; !ok || cb.ID != "im:RT渠道" || cb.PositionID != pA.ID {
		t.Fatalf("渠道聚合条目错误: %+v", cb)
	}
	if jb, ok := seen["cron"]; !ok || jb.ID != job.ID || jb.PositionID != "" {
		t.Fatalf("cron 聚合条目错误: %+v", jb)
	}

	if err := a.SaveResourceBinding("channel", "im:不存在", "", pA.ID, ""); err == nil {
		t.Fatalf("渠道不存在应拒绝")
	}
	if err := a.SaveResourceBinding("channel", "im:RT渠道", "", "", ""); err == nil || !strings.Contains(err.Error(), "裸跑") {
		t.Fatalf("启用渠道解绑回裸跑应被既有校验拒绝: %v", err)
	}
	if err := a.SaveResourceBinding("bogus", "x", "", "", ""); err == nil {
		t.Fatalf("未知资源类型应拒绝")
	}
}

// TestResourceBindings_SessionTab 会话资源聚合 + SetSessionBinding 写路径：
//   - 聚合：非子会话全部入表（ParentID 空），子会话不入；
//   - 正路径：纯岗位绑定 / 员工+岗位快照回填 / 全空解绑；
//   - 负路径：会话不存在 / 员工不存在 / 岗位不存在。
func TestResourceBindings_SessionTab(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "会话绑岗位")
	emp := seedEmployee(t, a, "孙八", p.ID)

	// 造两个会话：一个独立，一个委派子会话（不入聚合）。
	parent := &session.Session{ID: "sess-p", Title: "父会话", CreatedAt: time.Now()}
	a.store.Save(parent)
	child := &session.Session{ID: "sess-p-s1", Title: "子会话", ParentID: parent.ID, CreatedAt: time.Now()}
	a.store.Save(child)

	bindings := a.ListResourceBindings()
	var sess *ResourceBinding
	childSeen := false
	for i := range bindings {
		if bindings[i].Type != "session" {
			continue
		}
		if bindings[i].ID == parent.ID {
			sess = &bindings[i]
		} else if bindings[i].ID == child.ID {
			childSeen = true
		}
	}
	if sess == nil || childSeen {
		t.Fatalf("会话聚合应含父会话且不含子会话: %+v", bindings)
	}

	// 纯岗位绑定 → 会话 PositionID 落快照。
	if err := a.SaveResourceBinding("session", parent.ID, "", p.ID, "round_robin"); err != nil {
		t.Fatalf("会话绑岗位: %v", err)
	}
	got, _ := a.store.Get(parent.ID)
	if got.PositionID != p.ID || got.AssignRule != "round_robin" || got.EmployeeID != "" {
		t.Fatalf("会话岗位绑定未落库: %+v", got)
	}

	// 员工+岗位：岗位缺省自动回填员工当前岗位（快照回填语义）。
	if err := a.SetSessionBinding(parent.ID, emp.ID, "", ""); err != nil {
		t.Fatalf("会话绑员工: %v", err)
	}
	got, _ = a.store.Get(parent.ID)
	if got.EmployeeID != emp.ID || got.PositionID != p.ID {
		t.Fatalf("员工岗位快照回填失败: %+v", got)
	}

	// 全空 = 解绑回普通会话。
	if err := a.SetSessionBinding(parent.ID, "", "", ""); err != nil {
		t.Fatalf("会话解绑: %v", err)
	}
	got, _ = a.store.Get(parent.ID)
	if got.EmployeeID != "" || got.PositionID != "" {
		t.Fatalf("会话解绑失败: %+v", got)
	}

	// 负路径：会话/员工/岗位不存在。
	if err := a.SetSessionBinding("nope", "", p.ID, ""); err == nil {
		t.Fatal("会话不存在应拒绝")
	}
	if err := a.SetSessionBinding(parent.ID, "no-emp", p.ID, ""); err == nil {
		t.Fatal("员工不存在应拒绝")
	}
	if err := a.SetSessionBinding(parent.ID, "", "no-pos", ""); err == nil {
		t.Fatal("岗位不存在应拒绝")
	}
}

// TestSaveEmployee_PositionRequired §3/§4.4：新增/编辑岗位必填 + 岗位存在性；
// 存量员工（不经 SaveEmployee）不被改写。
func TestSaveEmployee_PositionRequired(t *testing.T) {
	a := newBindingTestApp(t)
	if _, err := a.SaveEmployee(employee.Employee{Name: "缺岗"}); err == nil || !strings.Contains(err.Error(), "岗位") {
		t.Fatalf("缺岗位应拒绝: %v", err)
	}
	if _, err := a.SaveEmployee(employee.Employee{Name: "幽灵岗", PositionID: "pos_ghost"}); err == nil || !strings.Contains(err.Error(), "岗位不存在") {
		t.Fatalf("岗位不存在应拒绝: %v", err)
	}
	p := seedPosition(t, a, "正式岗")
	e, err := a.SaveEmployee(employee.Employee{Name: "有岗", PositionID: p.ID})
	if err != nil || e.PositionID != p.ID {
		t.Fatalf("合法保存失败: %v %v", e, err)
	}
}

// TestDeleteEmployee_OccupiedReject 删除员工门禁（与 DeletePosition 同口径）：
// 渠道（含停用，绑定是休眠引用）/ cron 任务 / 会话绑定任一引用即拒并列出
// 来源；无占用正常删除。修复背景：此前仅拦启用渠道，cron/会话绑定被删员工
// 后触发时报「绑定的数字员工不存在」，用户不知根因是删除未拦。
func TestDeleteEmployee_OccupiedReject(t *testing.T) {
	a := newBindingTestApp(t)
	p := seedPosition(t, a, "删除门禁岗")
	eCh := seedEmployee(t, a, "渠道员工", p.ID)
	eJob := seedEmployee(t, a, "任务员工", p.ID)
	eSess := seedEmployee(t, a, "会话员工", p.ID)
	eFree := seedEmployee(t, a, "自由员工", p.ID)

	// 渠道占用（启用渠道绑定 eCh）。
	if err := a.SaveChannels([]ChannelConfig{{Type: "im", Name: "员工占用渠道", Enabled: true, WebhookURL: "https://e.com/h", EmployeeID: eCh.ID}}); err != nil {
		t.Fatalf("渠道保存: %v", err)
	}
	if err := a.DeleteEmployee(eCh.ID); err == nil || !strings.Contains(err.Error(), "渠道") {
		t.Fatalf("启用渠道占用应拒绝: %v", err)
	}

	// 停用渠道同样拦（绑定是休眠引用，重新启用时校验；删员工后重启渠道报
	// 「员工不存在」无根因）。
	chs := a.ListChannels()
	chs[0].Enabled = false
	if err := a.SaveChannels(chs); err != nil {
		t.Fatalf("渠道停用: %v", err)
	}
	if err := a.DeleteEmployee(eCh.ID); err == nil || !strings.Contains(err.Error(), "渠道") {
		t.Fatalf("停用渠道占用应拒绝: %v", err)
	}

	// cron 占用。
	job, err := a.cronStore.Add(cron.Job{Name: "员工占用任务", Expr: "@daily", Task: "跑", EmployeeID: eJob.ID, PositionID: p.ID})
	if err != nil {
		t.Fatalf("cron 保存: %v", err)
	}
	if err := a.DeleteEmployee(eJob.ID); err == nil || !strings.Contains(err.Error(), "定时任务") {
		t.Fatalf("cron 占用应拒绝: %v", err)
	}

	// 会话占用（非子会话）。
	sess := &session.Session{ID: "sess-del-emp", Title: "绑定会话", EmployeeID: eSess.ID, CreatedAt: time.Now()}
	a.store.Save(sess)
	if err := a.DeleteEmployee(eSess.ID); err == nil || !strings.Contains(err.Error(), "会话") {
		t.Fatalf("会话占用应拒绝: %v", err)
	}

	// 无占用 → 正常删除。
	if err := a.DeleteEmployee(eFree.ID); err != nil {
		t.Fatalf("无占用员工删除应成功: %v", err)
	}
	if _, ok := a.empStore.Get(eFree.ID); ok {
		t.Fatal("删除后员工不应存在")
	}
	_ = job
}

// TestSaveChannels_LegacyChannelRoundTrip 存量渠道（无绑定字段旧 JSON）重保存
// 兼容：停用渠道无绑定合法 + 绑定字段往返不丢。
func TestSaveChannels_LegacyChannelRoundTrip(t *testing.T) {
	a := newBindingTestApp(t)
	path := channelsConfigPath()
	if err := os.WriteFile(path, []byte(`[{"type":"im","name":"旧渠道","webhookUrl":"https://e.com/hook"}]`), 0o600); err != nil {
		t.Fatalf("预置旧配置: %v", err)
	}
	got := a.ListChannels()
	if len(got) != 1 || got[0].Type != "im" {
		t.Fatalf("渠道读取失败: %+v", got)
	}
	if err := a.SaveChannels(got); err != nil {
		t.Fatalf("存量渠道重保存应放行: %v", err)
	}
	if got := a.ListChannels(); len(got) != 1 || got[0].Type != "im" || got[0].Name != "旧渠道" {
		t.Fatalf("重保存往返失败: %+v", got)
	}
}
