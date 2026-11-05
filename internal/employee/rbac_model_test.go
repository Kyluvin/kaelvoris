package employee

// rbac_model_test.go 业界标准岗位-角色 RBAC 模型扩展单测（2026-12 权限模块优化）：
//   - Employee.NormalizeRoleIDs：RoleID/RoleIDs 归一化互同步（兼容存量单角色）；
//   - Position.InDeptScope / ScopeSummary：岗位范围（全公司/指定多部门）判定；
//   - OrgStore.checkPositionScopeLocked：范围部门存在性校验（Add/Update 双路径）。

import (
	"strings"
	"testing"
)

func TestNormalizeRoleIDs(t *testing.T) {
	// 存量单角色（仅 RoleID）→ 归一化进 RoleIDs 首位，RoleID 保持同步。
	e := Employee{RoleID: "r1"}
	e.NormalizeRoleIDs()
	if len(e.RoleIDs) != 1 || e.RoleIDs[0] != "r1" || e.RoleID != "r1" {
		t.Fatalf("存量单角色归一化失败: RoleID=%q RoleIDs=%v", e.RoleID, e.RoleIDs)
	}

	// 前端传多角色（RoleIDs 首位非 RoleID）→ RoleID 前插保序。
	e = Employee{RoleID: "r1", RoleIDs: []string{"r2", "r1", " r3 ", "r2", ""}}
	e.NormalizeRoleIDs()
	if len(e.RoleIDs) != 3 || e.RoleIDs[0] != "r1" || e.RoleIDs[1] != "r2" || e.RoleIDs[2] != "r3" {
		t.Fatalf("多角色归一化失败: %v", e.RoleIDs)
	}
	if e.RoleID != "r1" {
		t.Fatalf("RoleID 应与首位同步: %q", e.RoleID)
	}

	// 全空 → 双清。
	e = Employee{}
	e.NormalizeRoleIDs()
	if e.RoleID != "" || len(e.RoleIDs) != 0 {
		t.Fatalf("空角色归一化应双清: %q %v", e.RoleID, e.RoleIDs)
	}
}

func TestPositionInDeptScope(t *testing.T) {
	ids := map[string]bool{"d1": true, "d2": true, "d1-c1": true} // 含子树展开

	// 全公司范围：任意部门命中。
	p := Position{ScopeAll: true}
	if !p.InDeptScope(ids) || !p.InDeptScope(map[string]bool{}) {
		t.Fatal("全公司范围应恒命中")
	}

	// 指定多部门：命中其一即真。
	p = Position{DeptID: "d0", ScopeDeptIDs: []string{"d2", "d9"}}
	if !p.InDeptScope(ids) {
		t.Fatal("指定范围 d2 应命中")
	}
	if p.InDeptScope(map[string]bool{"d1": true}) {
		t.Fatal("范围外部门不应命中")
	}

	// 无范围字段：回退归属部门（含子树）。
	p = Position{DeptID: "d1"}
	if !p.InDeptScope(ids) {
		t.Fatal("归属部门 d1（含子树）应命中")
	}
	p = Position{DeptID: "d0"}
	if p.InDeptScope(ids) {
		t.Fatal("非范围内岗位不应命中")
	}

	if got := (&Position{ScopeAll: true}).ScopeSummary(); got != "全公司" {
		t.Fatalf("全公司摘要 = %q", got)
	}
	if got := (&Position{ScopeDeptIDs: []string{"a", "b", "c"}}).ScopeSummary(); got != "范围 3 部门" {
		t.Fatalf("多部门摘要 = %q", got)
	}
}

func TestOrgStore_PositionScopeValidation(t *testing.T) {
	s := seedOrgStore(t)
	dept, err := s.AddDepartment(Department{Name: "研发部"})
	if err != nil {
		t.Fatalf("AddDepartment: %v", err)
	}

	// 正路径：指定范围多部门。
	p, err := s.AddPosition(Position{Name: "后端工程师", DeptID: dept.ID, ScopeAll: false, ScopeDeptIDs: []string{dept.ID}})
	if err != nil {
		t.Fatalf("AddPosition 带范围: %v", err)
	}
	if len(p.ScopeDeptIDs) != 1 {
		t.Fatalf("范围应保留 1 项: %v", p.ScopeDeptIDs)
	}

	// 负路径：范围含不存在的部门 → 拒绝。
	_, err = s.AddPosition(Position{Name: "坏范围", ScopeDeptIDs: []string{"nope"}})
	if err == nil || !strings.Contains(err.Error(), "岗位范围部门不存在") {
		t.Fatalf("范围部门不存在应拒绝: %v", err)
	}

	// 全公司范围清空指定范围。
	p2, err := s.AddPosition(Position{Name: "全公司岗", ScopeAll: true, ScopeDeptIDs: []string{dept.ID}})
	if err != nil {
		t.Fatalf("AddPosition 全公司: %v", err)
	}
	if len(p2.ScopeDeptIDs) != 0 {
		t.Fatalf("全公司范围应清空指定范围: %v", p2.ScopeDeptIDs)
	}

	// Update 同样校验：改成不存在部门 → 拒绝。
	p.ScopeDeptIDs = []string{"nope"}
	if _, err := s.UpdatePosition(p); err == nil || !strings.Contains(err.Error(), "岗位范围部门不存在") {
		t.Fatalf("Update 范围校验失效: %v", err)
	}
}

// seedOrgStore 建空组织存储（临时目录，随测试清理）。
func seedOrgStore(t *testing.T) *OrgStore {
	t.Helper()
	s, err := NewOrgStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOrgStore: %v", err)
	}
	return s
}
