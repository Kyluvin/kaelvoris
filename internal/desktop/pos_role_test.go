// pos_role_test.go 业界标准岗位-角色 RBAC 扩展单测（用户需求：用户→岗位→角色→权限，
// 用户直绑角色仅兜底）：
//   - SavePosition RoleIDs 映射校验：不存在角色拒绝 / 去重去空 / 往返不丢；
//   - DeleteRole 引用门禁：被岗位映射或员工直绑引用时拒绝并列出来源；
//   - applyEmpRole 岗位角色链：岗位映射角色与员工直绑角色收紧式合并
//     （岗位角色禁用 MCP 时即使员工未配置也生效——主路径不因未直绑而失效）。
//
// 不依赖 Wails 上下文：App 仅构造 empStore/orgStore（binding_model_test.go 同款）。
package desktop

import (
	"strings"
	"testing"

	"kaelvoris/internal/employee"
)

// seedRole 建角色字典条目。
func seedRole(t *testing.T, a *App, name string) employee.Role {
	t.Helper()
	r, err := a.SaveRole(employee.Role{Name: name})
	if err != nil {
		t.Fatalf("SaveRole(%s): %v", name, err)
	}
	return r
}

// TestSavePosition_RoleIDsValidation 岗位-角色映射校验：负路径拒绝 + 正路径去重往返。
func TestSavePosition_RoleIDsValidation(t *testing.T) {
	a := newBindingTestApp(t)
	r1 := seedRole(t, a, "开发工程师")
	r2 := seedRole(t, a, "只读访客")

	// 正路径：多角色 + 重复项去重 + 空白剔除。
	p, err := a.SavePosition(employee.Position{Name: "后端岗", RoleIDs: []string{r1.ID, " " + r2.ID + " ", r1.ID, ""}})
	if err != nil {
		t.Fatalf("合法映射应保存成功: %v", err)
	}
	if len(p.RoleIDs) != 2 || p.RoleIDs[0] != r1.ID || p.RoleIDs[1] != r2.ID {
		t.Fatalf("映射应去重去空: %v", p.RoleIDs)
	}
	// 往返不丢。
	got := a.orgStore.View()
	var saved *employee.Position
	for i := range got.Positions {
		if got.Positions[i].ID == p.ID {
			saved = &got.Positions[i]
		}
	}
	if saved == nil || len(saved.RoleIDs) != 2 {
		t.Fatalf("岗位角色映射应落盘: %+v", saved)
	}

	// 负路径：角色不存在 → 拒绝（不静默丢弃）。
	if _, err := a.SavePosition(employee.Position{Name: "坏映射", RoleIDs: []string{"role_ghost"}}); err == nil || !strings.Contains(err.Error(), "角色不存在") {
		t.Fatalf("不存在角色应拒绝: %v", err)
	}
	// Update 同样校验。
	p.RoleIDs = []string{"role_ghost"}
	if _, err := a.SavePosition(p); err == nil || !strings.Contains(err.Error(), "角色不存在") {
		t.Fatalf("Update 坏映射应拒绝: %v", err)
	}
}

// TestDeleteRole_RefGuard 角色删除门禁：被岗位映射/员工直绑引用时拒绝并列出来源。
func TestDeleteRole_RefGuard(t *testing.T) {
	a := newBindingTestApp(t)
	rUsed := seedRole(t, a, "被引用角色")
	rFree := seedRole(t, a, "自由角色")
	p := seedPosition(t, a, "引用岗位")
	if _, err := a.SavePosition(employee.Position{ID: p.ID, Name: p.Name, RoleIDs: []string{rUsed.ID}}); err != nil {
		t.Fatalf("岗位映射: %v", err)
	}
	emp := seedEmployee(t, a, "直绑员工", p.ID)
	if _, err := a.SaveEmployee(employee.Employee{ID: emp.ID, Name: emp.Name, PositionID: p.ID, RoleIDs: []string{rFree.ID}}); err != nil {
		t.Fatalf("员工直绑角色: %v", err)
	}

	// 岗位映射 + 员工直绑均拒绝且列出岗位名。
	if err := a.DeleteRole(rUsed.ID); err == nil || !strings.Contains(err.Error(), "引用岗位") {
		t.Fatalf("岗位映射引用应拒绝: %v", err)
	}
	if err := a.DeleteRole(rFree.ID); err == nil || !strings.Contains(err.Error(), "直绑员工") {
		t.Fatalf("员工直绑引用应拒绝: %v", err)
	}
	// 解除岗位映射后可删（员工未引用 rUsed）。
	if _, err := a.SavePosition(employee.Position{ID: p.ID, Name: p.Name}); err != nil {
		t.Fatalf("岗位清映射: %v", err)
	}
	if err := a.DeleteRole(rUsed.ID); err != nil {
		t.Fatalf("解除映射后应可删: %v", err)
	}
}

// TestApplyEmpRole_PositionChain 岗位→角色链合并：岗位映射角色在员工未直绑时
// 仍然生效（收紧式）；员工直绑角色继续叠加；停用角色不叠加。
func TestApplyEmpRole_PositionChain(t *testing.T) {
	a := newBindingTestApp(t)
	dept, err := a.SaveDepartment(employee.Department{Name: "研发部"})
	if err != nil {
		t.Fatalf("部门: %v", err)
	}
	rPos := seedRole(t, a, "岗位只读角色")  // 岗位映射：只读
	rEmp := seedRole(t, a, "员工黑名单角色") // 直绑：MCP 黑名单
	rOff := seedRole(t, a, "停用角色")    // 停用：不叠加
	// rPos 补上只读权限包（seedRole 仅建名字典）。
	if _, err := a.SaveRole(employee.Role{Name: rPos.Name, ID: rPos.ID, ReadOnly: true}); err != nil {
		t.Fatalf("岗位只读角色保存: %v", err)
	}
	off := employee.Role{Name: rOff.Name, ID: rOff.ID, Status: employee.OrgDisabled, MCPDisabled: true}
	if _, err := a.SaveRole(off); err != nil {
		t.Fatalf("停用角色保存: %v", err)
	}

	p, err := a.SavePosition(employee.Position{Name: "链路岗", DeptID: dept.ID, RoleIDs: []string{rPos.ID, rOff.ID}})
	if err != nil {
		t.Fatalf("岗位: %v", err)
	}
	emp, err := a.SaveEmployee(employee.Employee{Name: "链路员工", PositionID: p.ID, RoleIDs: []string{rEmp.ID}})
	if err != nil {
		t.Fatalf("员工: %v", err)
	}

	// 员工自身配置：可写 + MCP 未配置。
	e := emp
	e.ReadOnly = false
	e.MCPMode = employee.MCPBlacklist
	e.MCPTools = []string{"mcp_dangerous_*"}
	a.applyEmpRole(&e)

	// 岗位角色（只读）生效——即使员工未直绑该角色。
	if !e.ReadOnly {
		t.Fatal("岗位映射角色的只读应收紧生效")
	}
	// 直绑角色（MCP 黑名单）继续叠加。
	if e.MCPMode != employee.MCPBlacklist || len(e.MCPTools) != 1 {
		t.Fatalf("直绑角色黑名单应保留: mode=%q tools=%v", e.MCPMode, e.MCPTools)
	}
	// 停用角色不叠加：MCP 未被禁用（若叠加了 off 角色，MCPDisabled 会置 true 且
	// MCPMode 清空）。
	if e.MCPDisabled {
		t.Fatal("停用角色不应叠加（MCP 未禁用）")
	}

	// 岗位无映射、员工无直绑 → 配置保持原样（新建一个无角色岗位）。
	pPlain, err := a.SavePosition(employee.Position{Name: "无角色岗"})
	if err != nil {
		t.Fatalf("无角色岗位: %v", err)
	}
	emp2, _ := a.SaveEmployee(employee.Employee{Name: "无角色员工", PositionID: pPlain.ID})
	e2 := emp2
	e2.MCPMode = employee.MCPWhitelist
	e2.MCPTools = []string{"fs_read"}
	a.applyEmpRole(&e2)
	if e2.MCPMode != employee.MCPWhitelist || len(e2.MCPTools) != 1 || e2.ReadOnly {
		t.Fatalf("无角色员工配置不应被改写: %+v", e2)
	}

	// Store 层查询：RoleIDsByPosition 返回映射副本。
	if got := a.orgStore.RoleIDsByPosition(p.ID); len(got) != 2 {
		t.Fatalf("RoleIDsByPosition 应返回 2 项: %v", got)
	}
	if got := a.orgStore.RoleIDsByPosition("nope"); got != nil {
		t.Fatalf("不存在岗位应返回 nil: %v", got)
	}
}
