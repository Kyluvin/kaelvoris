package employee

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestOrgStore(t *testing.T) *OrgStore {
	t.Helper()
	s, err := NewOrgStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOrgStore: %v", err)
	}
	return s
}

// seedEmpJSON 向组织目录写入员工数据（模拟员工 Store 落盘的 employees.json，
// 供 OrgStore 引用完整性兜底检查读文件）。
func seedEmpJSON(t *testing.T, dir string, emps []Employee) {
	t.Helper()
	b, err := jsonMarshalIndent(emps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "employees.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOrgStore_DeptCRUD(t *testing.T) {
	s := newTestOrgStore(t)

	// 顶级部门 + 子部门。
	d1, err := s.AddDepartment(Department{Name: "研发部"})
	if err != nil {
		t.Fatalf("Add 研发部: %v", err)
	}
	if d1.ID == "" || d1.Status != OrgActive || d1.CreatedAt.IsZero() {
		t.Fatalf("unexpected dept fields: %+v", d1)
	}
	d2, err := s.AddDepartment(Department{Name: "前端组", ParentID: d1.ID})
	if err != nil {
		t.Fatalf("Add 前端组: %v", err)
	}
	// 编码唯一。
	backend, err := s.AddDepartment(Department{Name: "后端组", Code: "TD-01"})
	if err != nil {
		t.Fatalf("Add 后端组(code): %v", err)
	}
	if _, err := s.AddDepartment(Department{Name: "测试部", Code: "TD-01"}); err == nil {
		t.Fatal("expected duplicate dept code error")
	}
	// 父不存在。
	if _, err := s.AddDepartment(Department{Name: "孤儿", ParentID: "no-such"}); err == nil {
		t.Fatal("expected missing-parent error")
	}
	// 更新：改名 + 排序。
	d1.Name = "技术研发部"
	d1.SortOrder = 2
	if _, err := s.UpdateDepartment(d1); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// 循环层级：不能挂到自己下级下。
	if _, err := s.UpdateDepartment(Department{ID: d1.ID, Name: "技术研发部", ParentID: d2.ID}); err == nil {
		t.Fatal("expected cycle error")
	}
	// 正常改父（顶级被挪为 d1 子部门：把运维部挂到 d1 下，先移走）。
	d3, _ := s.AddDepartment(Department{Name: "运维部"})
	if _, err := s.UpdateDepartment(Department{ID: d3.ID, Name: "运维部", ParentID: d1.ID}); err != nil {
		t.Fatalf("Update parent: %v", err)
	}
	// 删除有子部门的父：拒绝。
	if err := s.DeleteDepartment(d1.ID); err == nil {
		t.Fatal("expected delete-with-children error")
	}
	// 删除叶子成功（前端组）。
	if err := s.DeleteDepartment(d2.ID); err != nil {
		t.Fatalf("Delete 前端组: %v", err)
	}
	// 移走运维部（改回顶级）后删除。
	if _, err := s.UpdateDepartment(Department{ID: d3.ID, Name: "运维部"}); err != nil {
		t.Fatalf("move 运维部 to top: %v", err)
	}
	if err := s.DeleteDepartment(d3.ID); err != nil {
		t.Fatalf("Delete 运维部: %v", err)
	}
	if err := s.DeleteDepartment(backend.ID); err != nil {
		t.Fatalf("Delete 后端组: %v", err)
	}
	if err := s.DeleteDepartment(d1.ID); err != nil {
		t.Fatalf("Delete 技术研发部: %v", err)
	}
	// 持久化：重开 store 仍为空。
	s2, _ := NewOrgStore(s.dir)
	if got := s2.View(); len(got.Departments) != 0 {
		t.Fatalf("expected empty after deletes, got %+v", got.Departments)
	}
}

func TestOrgStore_DeptTreeAndSort(t *testing.T) {
	s := newTestOrgStore(t)
	root, _ := s.AddDepartment(Department{Name: "公司", SortOrder: 1})
	a, _ := s.AddDepartment(Department{Name: "A部", ParentID: root.ID, SortOrder: 2})
	b, _ := s.AddDepartment(Department{Name: "B部", ParentID: root.ID, SortOrder: 1})
	a1, _ := s.AddDepartment(Department{Name: "A-1组", ParentID: a.ID})

	tree := s.DeptTree()
	if len(tree) != 1 || tree[0].ID != root.ID || tree[0].Department.Name != "公司" {
		t.Fatalf("unexpected tree root: %+v", tree)
	}
	if len(tree[0].Children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(tree[0].Children))
	}
	// SortOrder 排序：B(1) 在 A(2) 前。
	if tree[0].Children[0].ID != b.ID || tree[0].Children[1].ID != a.ID {
		t.Fatalf("sort order wrong: %+v", tree[0].Children)
	}
	// 孙级挂接。
	if len(tree[0].Children[1].Children) != 1 || tree[0].Children[1].Children[0].ID != a1.ID {
		t.Fatalf("expected A-1 under A: %+v", tree[0].Children[1])
	}
}

func TestOrgStore_PositionRoleCRUD(t *testing.T) {
	s := newTestOrgStore(t)
	dept, _ := s.AddDepartment(Department{Name: "产品部"})

	pos, err := s.AddPosition(Position{Name: "产品经理", DeptID: dept.ID, Level: "M1"})
	if err != nil {
		t.Fatalf("AddPosition: %v", err)
	}
	if pos.ID == "" || pos.Status != OrgActive {
		t.Fatalf("unexpected position: %+v", pos)
	}
	if _, err := s.AddPosition(Position{Name: "产品经理2", Code: "PM-01"}); err != nil {
		t.Fatalf("AddPosition(code): %v", err)
	}
	if _, err := s.AddPosition(Position{Name: "产品经理3", Code: "PM-01"}); err == nil {
		t.Fatal("expected duplicate position code error")
	}
	// 岗位挂不存在部门。
	if _, err := s.AddPosition(Position{Name: "幽灵岗", DeptID: "no-such"}); err == nil {
		t.Fatal("expected missing-dept position error")
	}
	// 更新 + 删除。
	pos.Level = "M2"
	if _, err := s.UpdatePosition(pos); err != nil {
		t.Fatalf("UpdatePosition: %v", err)
	}
	if err := s.DeletePosition(pos.ID); err != nil {
		t.Fatalf("DeletePosition: %v", err)
	}

	role, err := s.AddRole(Role{Name: "开发工程师", Code: "R-DEV", MCPMode: MCPWhitelist, MCPTools: []string{"fs_read"}})
	if err != nil {
		t.Fatalf("AddRole: %v", err)
	}
	if _, err := s.AddRole(Role{Name: "开发工程师2", Code: "R-DEV"}); err == nil {
		t.Fatal("expected duplicate role code error")
	}
	// 未知 MCP 模式拒绝。
	if _, err := s.AddRole(Role{Name: "坏角色", MCPMode: "bogus"}); err == nil {
		t.Fatal("expected unknown MCP mode error")
	}
	role.Desc = "负责研发"
	if _, err := s.UpdateRole(role); err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	if err := s.DeleteRole(role.ID); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
}

func TestOrgStore_EmployeeRefGuard(t *testing.T) {
	s := newTestOrgStore(t)
	dept, _ := s.AddDepartment(Department{Name: "研发部"})
	pos, _ := s.AddPosition(Position{Name: "后端工程师", DeptID: dept.ID})
	role, _ := s.AddRole(Role{Name: "工程师"})
	// 员工表被删空后仍保留引用：删除字典时兜底拦截（模拟 employees.json 仍含员工）。
	seedEmpJSON(t, s.dir, []Employee{{ID: "e1", Name: "张三", DeptID: dept.ID, PositionID: pos.ID, RoleID: role.ID}})

	if err := s.DeleteDepartment(dept.ID); err == nil {
		t.Fatal("expected dept delete blocked by employee ref")
	}
	if err := s.DeletePosition(pos.ID); err == nil {
		t.Fatal("expected position delete blocked by employee ref")
	}
	if err := s.DeleteRole(role.ID); err == nil {
		t.Fatal("expected role delete blocked by employee ref")
	}
	// 员工清空后引用解除：可删除。
	seedEmpJSON(t, s.dir, nil)
	if err := s.DeletePosition(pos.ID); err != nil {
		t.Fatalf("DeletePosition after refs cleared: %v", err)
	}
	if err := s.DeleteRole(role.ID); err != nil {
		t.Fatalf("DeleteRole after refs cleared: %v", err)
	}
	if err := s.DeleteDepartment(dept.ID); err != nil {
		t.Fatalf("DeleteDepartment after refs cleared: %v", err)
	}
}

// TestOrgStore_DeptScopeRefGuard 删除部门被岗位范围（ScopeDeptIDs）引用时拒绝：
// 否则删后岗位 UpdatePosition 会被 checkPositionScopeLocked 拒绝（范围部门不存在），
// 形成保存断链。同时验证员工多角色（RoleIDs 列表）引用兜底拦截。
func TestOrgStore_DeptScopeRefGuard(t *testing.T) {
	s := newTestOrgStore(t)
	main, _ := s.AddDepartment(Department{Name: "主部门"})
	scope, _ := s.AddDepartment(Department{Name: "范围部门"})
	pos, _ := s.AddPosition(Position{Name: "区域专员", DeptID: main.ID, ScopeDeptIDs: []string{scope.ID}})
	role, _ := s.AddRole(Role{Name: "多角色"})

	// 岗位范围引用：拒绝。
	if err := s.DeleteDepartment(scope.ID); err == nil || !strings.Contains(err.Error(), "范围引用") {
		t.Fatalf("expected scope-ref block, got: %v", err)
	}
	// 移除岗位范围后可删。
	pos.ScopeDeptIDs = nil
	if _, err := s.UpdatePosition(pos); err != nil {
		t.Fatalf("clear scope: %v", err)
	}
	if err := s.DeleteDepartment(scope.ID); err != nil {
		t.Fatalf("DeleteDepartment after scope cleared: %v", err)
	}

	// 员工 RoleIDs 列表（非 RoleID 单字段）引用兜底拦截。
	seedEmpJSON(t, s.dir, []Employee{{ID: "e2", Name: "李四", RoleIDs: []string{role.ID, "r-other"}}})
	if err := s.DeleteRole(role.ID); err == nil {
		t.Fatal("expected role delete blocked by RoleIDs list ref")
	}
	seedEmpJSON(t, s.dir, nil)
	if err := s.DeleteRole(role.ID); err != nil {
		t.Fatalf("DeleteRole after RoleIDs cleared: %v", err)
	}
}

func TestOrgStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewOrgStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s1.AddDepartment(Department{Name: "市场部", Code: "MK-01"})
	_, _ = s1.AddPosition(Position{Name: "市场专员", DeptID: d.ID})
	_, _ = s1.AddRole(Role{Name: "市场员"})

	s2, err := NewOrgStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	v := s2.View()
	if len(v.Departments) != 1 || v.Departments[0].Name != "市场部" || v.Departments[0].Code != "MK-01" {
		t.Fatalf("dept persistence lost: %+v", v.Departments)
	}
	if len(v.Positions) != 1 || len(v.Roles) != 1 {
		t.Fatalf("position/role persistence lost: %+v", v)
	}
}

func TestOrgStore_Validate(t *testing.T) {
	cases := []struct {
		name string
		d    Department
		want bool // true = 期望报错
	}{
		{"空名", Department{Name: "  "}, true},
		{"名称超长", Department{Name: strings.Repeat("长", 51)}, true},
		{"非法状态", Department{Name: "x", Status: "bogus"}, true},
		{"合法", Department{Name: "x", Status: OrgDisabled}, false},
	}
	for _, c := range cases {
		err := c.d.Validate()
		if (err != nil) != c.want {
			t.Errorf("%s: Validate() err=%v wantErr=%v", c.name, err, c.want)
		}
	}
	p := Position{Name: "x"}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid position rejected: %v", err)
	}
	r := Role{Name: "x", MCPMode: MCPUnset}
	if err := r.Validate(); err != nil {
		t.Fatalf("valid role rejected: %v", err)
	}
}

func TestOrgStore_CorruptFileFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "org.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewOrgStore(dir)
	if err != nil {
		t.Fatalf("corrupt org.json should not fail init: %v", err)
	}
	if got := s.View(); len(got.Departments) != 0 {
		t.Fatalf("expected empty view after corrupt file, got %+v", got)
	}
}
