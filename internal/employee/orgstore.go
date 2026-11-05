package employee

// orgstore.go 组织架构存储（~/.kaelvoris/employees/org.json）：
//
// 三类主数据单文件持久化（全量数组 + 原子替换写，模式对齐 employee.Store）；
// 引用完整性校验（删除部门前须移走子部门/岗位/员工；删除岗位/角色前须无员工
// 引用；编码唯一）。所有操作「直接录入、直接生效」，无审核流程。

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// OrgStore 组织架构主数据持久化。
type OrgStore struct {
	dir         string
	mu          sync.Mutex
	departments []Department
	positions   []Position
	roles       []Role
	// empRef 员工列表共享快照钩子（BindEmployeeRef 注入；nil = 回退文件读取）。
	empRef func() []Employee
}

// NewOrgStore 加载组织数据（org.json 缺失 = 空组织；损坏跳过不致命）。
func NewOrgStore(dir string) (*OrgStore, error) {
	s := &OrgStore{dir: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.dataPath())
	if err == nil {
		_ = s.jsonLoad(b)
	}
	return s, nil
}

func (s *OrgStore) dataPath() string { return joinPath(s.dir, "org.json") }

// saveLocked 序列化三个主数据切片（departments/positions/roles 为未导出字段，
// 直接 Marshal(s) 会写成空对象 {} 导致重载后数据丢失）。
func (s *OrgStore) saveLocked() error {
	b, err := jsonMarshalIndent(struct {
		Departments []Department `json:"departments"`
		Positions   []Position   `json:"positions"`
		Roles       []Role       `json:"roles"`
	}{
		Departments: s.departments,
		Positions:   s.positions,
		Roles:       s.roles,
	})
	if err != nil {
		return err
	}
	tmp := s.dataPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.dataPath())
}

// jsonLoad 从磁盘加载三个主数据切片（与 saveLocked 布局对齐；未导出字段
// 数组无法直接 Unmarshal 到 s，须经导出结构中转）。
func (s *OrgStore) jsonLoad(b []byte) error {
	var raw struct {
		Departments []Department `json:"departments"`
		Positions   []Position   `json:"positions"`
		Roles       []Role       `json:"roles"`
	}
	if err := jsonUnmarshal(b, &raw); err != nil {
		return err
	}
	s.departments = raw.Departments
	s.positions = raw.Positions
	s.roles = raw.Roles
	return nil
}

// View 组织架构全量视图（三字典；部门按 SortOrder→创建时间排序，
// 岗位/角色按创建时间排序）。
func (s *OrgStore) View() OrgView {
	s.mu.Lock()
	defer s.mu.Unlock()
	deps := make([]Department, len(s.departments))
	copy(deps, s.departments)
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].SortOrder != deps[j].SortOrder {
			return deps[i].SortOrder < deps[j].SortOrder
		}
		return deps[i].CreatedAt.Before(deps[j].CreatedAt)
	})
	pos := make([]Position, len(s.positions))
	copy(pos, s.positions)
	roles := make([]Role, len(s.roles))
	copy(roles, s.roles)
	return OrgView{Departments: deps, Positions: pos, Roles: roles}
}

// ---------- 部门 ----------

// AddDepartment 新建部门（Name/Code/（非空）全局唯一；ParentID 须存在。
// 直接生效，无审核）。
func (s *OrgStore) AddDepartment(d Department) (Department, error) {
	d.Name = strings.TrimSpace(d.Name)
	d.Code = strings.TrimSpace(d.Code)
	if err := d.Validate(); err != nil {
		return d, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.departments {
		if x.Name == d.Name {
			return d, fmt.Errorf("部门名称已存在: %s", d.Name)
		}
		if d.Code != "" && x.Code == d.Code {
			return d, fmt.Errorf("部门编码已存在: %s", d.Code)
		}
	}
	if d.ParentID != "" && !s.deptExistsLocked(d.ParentID) {
		return d, fmt.Errorf("上级部门不存在: %s", d.ParentID)
	}
	if strings.TrimSpace(d.ID) == "" {
		d.ID = newID()
	}
	if d.Status == "" {
		d.Status = OrgActive
	}
	now := time.Now()
	d.CreatedAt = now
	d.UpdatedAt = now
	s.departments = append(s.departments, d)
	if err := s.saveLocked(); err != nil {
		return d, err
	}
	return d, nil
}

// UpdateDepartment 更新部门（名称/编码唯一性；循环层级检测：改组父级
// 不允许把部门挂到自己或自己的子孙下）。
func (s *OrgStore) UpdateDepartment(d Department) (Department, error) {
	d.Name = strings.TrimSpace(d.Name)
	d.Code = strings.TrimSpace(d.Code)
	if err := d.Validate(); err != nil {
		return d, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.deptIndexLocked(d.ID)
	if idx < 0 {
		return d, errors.New("部门不存在: " + d.ID)
	}
	for i, x := range s.departments {
		if i == idx {
			continue
		}
		if x.Name == d.Name {
			return d, fmt.Errorf("部门名称已存在: %s", d.Name)
		}
		if d.Code != "" && x.Code == d.Code {
			return d, fmt.Errorf("部门编码已存在: %s", d.Code)
		}
	}
	if d.ParentID != "" {
		if !s.deptExistsLocked(d.ParentID) {
			return d, fmt.Errorf("上级部门不存在: %s", d.ParentID)
		}
		if s.isDescendantLocked(d.ParentID, d.ID) {
			return d, errors.New("不能把部门挂到自己的下级部门下（层级循环）")
		}
	}
	d.CreatedAt = s.departments[idx].CreatedAt
	d.UpdatedAt = time.Now()
	s.departments[idx] = d
	if err := s.saveLocked(); err != nil {
		return d, err
	}
	return d, nil
}

// DeleteDepartment 删除部门（引用完整性：有子部门/岗位/员工引用时拒绝）。
func (s *OrgStore) DeleteDepartment(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deptIndexLocked(id) < 0 {
		return errors.New("部门不存在: " + id)
	}
	for _, x := range s.departments {
		if x.ParentID == id {
			return errors.New("该部门下存在子部门，请先移动或删除子部门")
		}
	}
	for _, p := range s.positions {
		if p.DeptID == id {
			return errors.New("该部门下存在岗位引用，请先调整岗位归属")
		}
		// 岗位范围（ScopeDeptIDs）也属引用：删掉后岗位 UpdatePosition 会被
		// checkPositionScopeLocked 拒绝（范围部门不存在），形成保存断链。
		for _, d := range p.ScopeDeptIDs {
			if d == id {
				return errors.New("该部门被岗位「" + p.Name + "」的范围引用，请先调整岗位范围")
			}
		}
	}
	// 部门被员工引用（员工 DeptID = id）：复用 refByEmployeesLocked，避免重复读文件逻辑。
	if s.refByEmployeesLocked(func(e Employee) string { return e.DeptID }, id) {
		return errors.New("该部门下存在员工，请先调整员工归属部门")
	}
	for i, x := range s.departments {
		if x.ID == id {
			s.departments = append(s.departments[:i], s.departments[i+1:]...)
			return s.saveLocked()
		}
	}
	return nil
}

// DeptTree 部门树视图（前端渲染组织树；含每节点统计信息由面板层补充）。
type DeptTree struct {
	Department
	Children []*DeptTree `json:"children,omitempty"`
}

// DeptTree 构建部门树（顶层按 SortOrder；NodeStats 由员工面板按 ID 聚合）。
func (s *OrgStore) DeptTree() []*DeptTree {
	s.mu.Lock()
	defer s.mu.Unlock()
	byParent := map[string][]*DeptTree{}
	for i := range s.departments {
		d := s.departments[i]
		if d.ParentID != "" && !s.deptExistsLocked(d.ParentID) {
			// 孤儿节点（父被删）：挂到顶层展示，不静默丢失。
			d.ParentID = ""
		}
		node := &DeptTree{Department: d}
		byParent[d.ParentID] = append(byParent[d.ParentID], node)
	}
	var build func(parent string) []*DeptTree
	build = func(parent string) []*DeptTree {
		list := byParent[parent]
		// 循环依赖兜底：最多递归 64 层防环。
		sort.Slice(list, func(i, j int) bool {
			if list[i].SortOrder != list[j].SortOrder {
				return list[i].SortOrder < list[j].SortOrder
			}
			return list[i].CreatedAt.Before(list[j].CreatedAt)
		})
		for _, n := range list {
			n.Children = build(n.ID)
		}
		return list
	}
	return build("")
}

// ---------- 岗位 ----------

// AddPosition 新建岗位（名称可在不同部门重复；编码非空时唯一；DeptID 须存在）。
func (s *OrgStore) AddPosition(p Position) (Position, error) {
	p.Name = strings.TrimSpace(p.Name)
	p.Code = strings.TrimSpace(p.Code)
	if err := p.Validate(); err != nil {
		return p, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.DeptID != "" && !s.deptExistsLocked(p.DeptID) {
		return p, fmt.Errorf("归属部门不存在: %s", p.DeptID)
	}
	if err := s.checkPositionScopeLocked(&p); err != nil {
		return p, err
	}
	if p.Code != "" {
		for _, x := range s.positions {
			if x.Code == p.Code {
				return p, fmt.Errorf("岗位编码已存在: %s", p.Code)
			}
		}
	}
	if strings.TrimSpace(p.ID) == "" {
		p.ID = newID()
	}
	if p.Status == "" {
		p.Status = OrgActive
	}
	now := time.Now()
	p.CreatedAt = now
	p.UpdatedAt = now
	s.positions = append(s.positions, p)
	if err := s.saveLocked(); err != nil {
		return p, err
	}
	return p, nil
}

// UpdatePosition 更新岗位。
func (s *OrgStore) UpdatePosition(p Position) (Position, error) {
	p.Name = strings.TrimSpace(p.Name)
	p.Code = strings.TrimSpace(p.Code)
	if err := p.Validate(); err != nil {
		return p, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.posIndexLocked(p.ID)
	if idx < 0 {
		return p, errors.New("岗位不存在: " + p.ID)
	}
	if p.DeptID != "" && !s.deptExistsLocked(p.DeptID) {
		return p, fmt.Errorf("归属部门不存在: %s", p.DeptID)
	}
	if err := s.checkPositionScopeLocked(&p); err != nil {
		return p, err
	}
	if p.Code != "" {
		for i, x := range s.positions {
			if i != idx && x.Code == p.Code {
				return p, fmt.Errorf("岗位编码已存在: %s", p.Code)
			}
		}
	}
	p.CreatedAt = s.positions[idx].CreatedAt
	p.UpdatedAt = time.Now()
	s.positions[idx] = p
	if err := s.saveLocked(); err != nil {
		return p, err
	}
	return p, nil
}

// checkPositionScopeLocked 岗位范围校验（锁内调用）：全公司范围清空指定范围；
// 指定范围逐个校验部门存在性 + 去重（误填不存在的部门直接拒绝，不静默丢弃）。
func (s *OrgStore) checkPositionScopeLocked(p *Position) error {
	if p.ScopeAll {
		p.ScopeDeptIDs = nil
		return nil
	}
	if len(p.ScopeDeptIDs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range p.ScopeDeptIDs {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		if !s.deptExistsLocked(d) {
			return fmt.Errorf("岗位范围部门不存在: %s", d)
		}
		seen[d] = true
		out = append(out, d)
	}
	p.ScopeDeptIDs = out
	return nil
}

// RoleIDsByPosition 岗位映射的角色 ID 列表（未找到岗位 = nil；供面板层
// 员工有效角色解析：员工直绑角色 ∪ 岗位映射角色）。
func (s *OrgStore) RoleIDsByPosition(positionID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.positions {
		if p.ID == positionID {
			return append([]string{}, p.RoleIDs...)
		}
	}
	return nil
}

// RoleExists 角色存在性（零拷贝：锁内 O(n) 扫描，替代调用方 View().Roles
// 全量三字典拷贝后遍历——校验热路径每次省 departments/positions 两份拷贝）。
func (s *OrgStore) RoleExists(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.roleIndexLocked(id) >= 0
}

// PositionExists 岗位存在性（零拷贝，同 RoleExists）。
func (s *OrgStore) PositionExists(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posIndexLocked(id) >= 0
}

// DeletePosition 删除岗位（有员工引用时拒绝）。
func (s *OrgStore) DeletePosition(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.posIndexLocked(id) < 0 {
		return errors.New("岗位不存在: " + id)
	}
	if s.refByEmployeesLocked(func(e Employee) string { return e.PositionID }, id) {
		return errors.New("该岗位已被员工引用，请先调整员工岗位")
	}
	for i, x := range s.positions {
		if x.ID == id {
			s.positions = append(s.positions[:i], s.positions[i+1:]...)
			return s.saveLocked()
		}
	}
	return nil
}

// ---------- 角色 ----------

// AddRole 新建角色（名称/编码唯一）。
func (s *OrgStore) AddRole(r Role) (Role, error) {
	r.Name = strings.TrimSpace(r.Name)
	r.Code = strings.TrimSpace(r.Code)
	if err := r.Validate(); err != nil {
		return r, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.roles {
		if x.Name == r.Name {
			return r, fmt.Errorf("角色名称已存在: %s", r.Name)
		}
		if r.Code != "" && x.Code == r.Code {
			return r, fmt.Errorf("角色编码已存在: %s", r.Code)
		}
	}
	if strings.TrimSpace(r.ID) == "" {
		r.ID = newID()
	}
	if r.Status == "" {
		r.Status = OrgActive
	}
	now := time.Now()
	r.CreatedAt = now
	r.UpdatedAt = now
	s.roles = append(s.roles, r)
	if err := s.saveLocked(); err != nil {
		return r, err
	}
	return r, nil
}

// UpdateRole 更新角色。
func (s *OrgStore) UpdateRole(r Role) (Role, error) {
	r.Name = strings.TrimSpace(r.Name)
	r.Code = strings.TrimSpace(r.Code)
	if err := r.Validate(); err != nil {
		return r, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.roleIndexLocked(r.ID)
	if idx < 0 {
		return r, errors.New("角色不存在: " + r.ID)
	}
	for i, x := range s.roles {
		if i == idx {
			continue
		}
		if x.Name == r.Name {
			return r, fmt.Errorf("角色名称已存在: %s", r.Name)
		}
		if r.Code != "" && x.Code == r.Code {
			return r, fmt.Errorf("角色编码已存在: %s", r.Code)
		}
	}
	r.CreatedAt = s.roles[idx].CreatedAt
	r.UpdatedAt = time.Now()
	s.roles[idx] = r
	if err := s.saveLocked(); err != nil {
		return r, err
	}
	return r, nil
}

// DeleteRole 删除角色（有员工引用时拒绝）。
//
// 兜底口径与面板层 DeleteRole 一致：员工侧查 RoleIDs 列表（含 RoleID
// 首位兼容位；normalizeRoleIDs 保证两者同步，列表口径不漏直绑多角色）；
// 岗位映射 RoleIDs 由面板层拦截，Store 层不跨字典校验（与部门/岗位删除
// 同层：仅员工文件引用兜底）。
func (s *OrgStore) DeleteRole(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.roleIndexLocked(id) < 0 {
		return errors.New("角色不存在: " + id)
	}
	if s.refByEmployeesLocked(func(e Employee) string {
		e.NormalizeRoleIDs()
		for _, rid := range e.RoleIDs {
			if rid == id {
				return rid
			}
		}
		return ""
	}, id) {
		return errors.New("该角色已被员工引用，请先调整员工角色")
	}
	for i, x := range s.roles {
		if x.ID == id {
			s.roles = append(s.roles[:i], s.roles[i+1:]...)
			return s.saveLocked()
		}
	}
	return nil
}

// ---------- 通用内部工具 ----------

func (s *OrgStore) deptExistsLocked(id string) bool { return s.deptIndexLocked(id) >= 0 }

// 以下三个 indexLocked 为 O(n) 线性扫描：组织字典规模（几十条目）下
// 分支预测友好、无 map 重建开销，实测快于维护索引——小数据量不加索引
// （ponytail：性能瓶颈出现前不预优化）。
func (s *OrgStore) deptIndexLocked(id string) int {
	for i, x := range s.departments {
		if x.ID == id {
			return i
		}
	}
	return -1
}
func (s *OrgStore) posIndexLocked(id string) int {
	for i, x := range s.positions {
		if x.ID == id {
			return i
		}
	}
	return -1
}
func (s *OrgStore) roleIndexLocked(id string) int {
	for i, x := range s.roles {
		if x.ID == id {
			return i
		}
	}
	return -1
}

// isDescendantLocked ancestor 是否在 target 的子孙链上（防层级循环）。
func (s *OrgStore) isDescendantLocked(ancestor, target string) bool {
	cur := ancestor
	seen := 0
	for cur != "" && seen <= len(s.departments) {
		if cur == target {
			return true
		}
		next := ""
		for i := range s.departments {
			if s.departments[i].ID == cur {
				next = s.departments[i].ParentID
				break
			}
		}
		cur = next
		seen++
	}
	return false
}

// refByEmployeesLocked 是否存在员工引用（field 提取被引字典 ID）。
// 员工列表优先取 s.empRef（运行时注入的共享快照钩子，零 IO）；
// 钩子未注入时回退读 employees.json（旧测试/独立构造场景，行为不变）。
func (s *OrgStore) refByEmployeesLocked(field func(Employee) string, id string) bool {
	if s.empRef != nil {
		for _, e := range s.empRef() {
			if field(e) == id {
				return true
			}
		}
		return false
	}
	// 兜底：文件级引用检查（员工 Store 已加载的最新数据由面板层传参校验更稳，
	// 这里作为退路：避免跨 Store 死锁，不做锁交叉）。
	path := joinPath(s.dir, "employees.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var list []Employee
	if err := jsonUnmarshal(b, &list); err != nil {
		return false
	}
	for _, e := range list {
		if field(e) == id {
			return true
		}
	}
	return false
}

// BindEmployeeRef 注入员工列表共享快照钩子（app 启动时装配：
// s.empRef = empStore.List，删除校验从「每次读盘反序列化」降为内存遍历）。
func (s *OrgStore) BindEmployeeRef(fn func() []Employee) { s.empRef = fn }
