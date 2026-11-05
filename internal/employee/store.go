package employee

// store.go 员工持久化（~/.kaelvoris/employees/employees.json，模式对齐 cron.Store）：
// 全量数组单文件 + 原子替换写；读取时一次性加载。

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Store 员工定义持久化（employees.json）+ 每员工工作空间/提案目录管理。
type Store struct {
	dir       string // 员工根目录（含 employees.json 与 <id>/ 子目录）
	mu        sync.Mutex
	employees []Employee
	// idx 惰性 ID 索引（Get 热路径：网关装配/渠道收口每次任务触发都查）；
	// 写操作后置 nil 失效，下次 Get 重建（写少读多场景最优）。
	idx map[string]int
}

// NewStore 创建存储并加载既有数据。
func NewStore(dir string) (*Store, error) {
	s := &Store{dir: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.dataPath())
	if err == nil {
		_ = jsonUnmarshal(b, &s.employees)
	}
	return s, nil
}

func (s *Store) dataPath() string { return joinPath(s.dir, "employees.json") }

func (s *Store) saveLocked() error {
	b, err := jsonMarshalIndent(s.employees)
	if err != nil {
		return err
	}
	tmp := s.dataPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	s.idx = nil // 写后失效惰性索引（下次 Get 重建）
	return os.Rename(tmp, s.dataPath())
}

// getIndexLocked 惰性 ID→下标索引（锁内调用；员工表 O(n) 建一次，Get O(1)）。
func (s *Store) getIndexLocked() map[string]int {
	if s.idx == nil {
		s.idx = make(map[string]int, len(s.employees))
		for i, e := range s.employees {
			s.idx[e.ID] = i
		}
	}
	return s.idx
}

// Dir 返回员工存储根目录（app 层构建网关/读取工作空间用）。
func (s *Store) Dir() string { return s.dir }

// List 全部员工（按创建时间正序）。
func (s *Store) List() []Employee {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Employee, len(s.employees))
	copy(out, s.employees)
	return out
}

// Get 按 ID 读取（惰性索引 O(1)；索引失效时重建一次 O(n)）。
func (s *Store) Get(id string) (Employee, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.getIndexLocked()[id]; ok && i < len(s.employees) && s.employees[i].ID == id {
		return s.employees[i], true
	}
	return Employee{}, false
}

// GetByName 按名称查找（唯一性校验用）。
func (s *Store) GetByName(name string) (Employee, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.employees {
		if e.Name == name {
			return e, true
		}
	}
	return Employee{}, false
}

// NewID 生成员工 ID（emp_ + 随机 8 字节十六进制）。
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hexEncode(b)
}

// Add 新建员工：校验（含 EHR 组织引用：工号唯一/上级存在且非自己/无环）+
// 初始化工作空间骨架（workspace/memory + evolution-drafts）。直接生效，无审核。
func (s *Store) Add(e Employee) (Employee, error) {
	e.Name = strings.TrimSpace(e.Name)
	e.WorkspaceRoot = normalizeWorkspaceRoot(e.WorkspaceRoot)
	e.normalizeRoleIDs()
	if err := e.Validate(); err != nil {
		return e, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkRefsLocked(&e, ""); err != nil {
		return e, err
	}
	for _, x := range s.employees {
		if x.Name == e.Name {
			return e, fmt.Errorf("员工名称已存在: %s", e.Name)
		}
	}
	if no := strings.TrimSpace(e.EmpNo); no != "" {
		for _, x := range s.employees {
			if x.EmpNo == no {
				return e, fmt.Errorf("工号已存在: %s", no)
			}
		}
	}
	// 自定义工作空间目录独占（§3 数据隔离）：禁止两个员工共用同一目录。
	if e.WorkspaceRoot != "" {
		for _, x := range s.employees {
			if x.ID == e.ID {
				continue
			}
			if x.WorkspaceRoot != "" && sameDir(x.WorkspaceRoot, e.WorkspaceRoot) {
				return e, fmt.Errorf("自定义工作空间已被员工「%s」占用，请更换目录", x.Name)
			}
		}
	}
	if strings.TrimSpace(e.ID) == "" {
		e.ID = newID()
	}
	now := time.Now()
	if e.Status == "" {
		e.Status = StatusEnabled
	}
	if e.EmpStatus == "" {
		e.EmpStatus = EmpActive
	}
	e.CreatedAt = now
	e.UpdatedAt = now
	s.employees = append(s.employees, e)
	if err := s.saveLocked(); err != nil {
		return e, err
	}
	if err := s.InitWorkspace(e); err != nil {
		return e, fmt.Errorf("工作空间初始化失败: %w", err)
	}
	return e, nil
}

// Update 更新员工（保留 CreatedAt；名称/工号唯一性校验；上级引用校验）。
func (s *Store) Update(e Employee) (Employee, error) {
	e.Name = strings.TrimSpace(e.Name)
	e.WorkspaceRoot = normalizeWorkspaceRoot(e.WorkspaceRoot)
	e.normalizeRoleIDs()
	if err := e.Validate(); err != nil {
		return e, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, x := range s.employees {
		if x.ID == e.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return e, errors.New("员工不存在: " + e.ID)
	}
	if err := s.checkRefsLocked(&e, e.ID); err != nil {
		return e, err
	}
	for i, x := range s.employees {
		if i != idx && x.Name == e.Name {
			return e, fmt.Errorf("员工名称已存在: %s", e.Name)
		}
	}
	if no := strings.TrimSpace(e.EmpNo); no != "" {
		for i, x := range s.employees {
			if i != idx && x.EmpNo == no {
				return e, fmt.Errorf("工号已存在: %s", no)
			}
		}
	}
	// 自定义工作空间目录独占（§3 数据隔离）：禁止与其他员工共用同一目录。
	if e.WorkspaceRoot != "" {
		for _, x := range s.employees {
			if x.ID == e.ID {
				continue
			}
			if x.WorkspaceRoot != "" && sameDir(x.WorkspaceRoot, e.WorkspaceRoot) {
				return e, fmt.Errorf("自定义工作空间已被员工「%s」占用，请更换目录", x.Name)
			}
		}
	}
	e.CreatedAt = s.employees[idx].CreatedAt
	e.UpdatedAt = time.Now()
	s.employees[idx] = e
	if err := s.saveLocked(); err != nil {
		return e, err
	}
	if err := s.InitWorkspace(e); err != nil {
		return e, nil // 目录初始化失败不阻断保存（只读环境兜底）
	}
	return e, nil
}

// checkRefsLocked 校验 EHR 组织引用（锁内调用；selfID 空 = 新建）：
//   - ManagerID 必须存在且不得为自己；
//   - ManagerID 链不得成环（含间接自引用）。
//
// 说明：DeptID/PositionID/RoleID 的字典存在性由 OrgStore 引用完整性兜底
// （删除字典前先校验员工引用）；此处校验上级关系（员工表自身引用）。
func (s *Store) checkRefsLocked(e *Employee, selfID string) error {
	if m := strings.TrimSpace(e.ManagerID); m != "" {
		if m == selfID {
			return errors.New("直属上级不能是自己")
		}
		if e.ID != "" && m == e.ID {
			return errors.New("直属上级不能是自己")
		}
		found := false
		for _, x := range s.employees {
			if x.ID == m {
				found = true
				break
			}
		}
		if !found {
			return errors.New("直属上级不存在: " + m)
		}
		// 上级链回溯，防 A→B→A 环（含新建时 e.ID 未定：以 ManagerID 为起点回溯）。
		cur := m
		seen := 0
		for cur != "" && seen <= len(s.employees) {
			if cur == selfID || (e.ID != "" && cur == e.ID) {
				return errors.New("直属上级关系形成循环（请调整）")
			}
			next := ""
			for _, x := range s.employees {
				if x.ID == cur {
					next = x.ManagerID
					break
				}
			}
			if next == cur {
				return errors.New("直属上级关系形成循环（请调整）")
			}
			cur = next
			seen++
		}
	}
	return nil
}

// SetStatus 更新状态（§2.2：启用/停用/草稿/归档）。
func (s *Store) SetStatus(id, status string) (Employee, error) {
	switch status {
	case StatusEnabled, StatusDisabled, StatusDraft, StatusArchived:
	default:
		return Employee{}, fmt.Errorf("未知状态: %s", status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.employees {
		if s.employees[i].ID == id {
			s.employees[i].Status = status
			s.employees[i].UpdatedAt = time.Now()
			if err := s.saveLocked(); err != nil {
				return Employee{}, err
			}
			return s.employees[i], nil
		}
	}
	return Employee{}, errors.New("员工不存在: " + id)
}

// SetEmpStatus 更新人事状态（EHR：在职/试用/停用/离职；离职 = 不可运行）。
func (s *Store) SetEmpStatus(id, empStatus string) (Employee, error) {
	switch empStatus {
	case EmpActive, EmpProbation, EmpSuspended, EmpResigned:
	default:
		return Employee{}, fmt.Errorf("未知人事状态: %s", empStatus)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.employees {
		if s.employees[i].ID == id {
			s.employees[i].EmpStatus = empStatus
			s.employees[i].UpdatedAt = time.Now()
			if err := s.saveLocked(); err != nil {
				return Employee{}, err
			}
			return s.employees[i], nil
		}
	}
	return Employee{}, errors.New("员工不存在: " + id)
}

// Delete 删除员工（工作空间目录保留——用户数据不静默删除，§8.4 数据隔离）。
// 悬挂引用清理：其他员工的 ManagerID 指向被删者时置空（上级离职后下属
// 编辑保存会被 checkRefsLocked 拒——「直属上级不存在」，保存断链；EHR 语义
// = 上级离职时下属暂归无上级，不改下属部门/岗位）。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.employees {
		if e.ID == id {
			s.employees = append(s.employees[:i], s.employees[i+1:]...)
			// 清理其他员工指向被删者的直属上级引用（上级离职后下属暂归无上级）。
			for j := range s.employees {
				if s.employees[j].ManagerID == id {
					s.employees[j].ManagerID = ""
					s.employees[j].UpdatedAt = time.Now()
				}
			}
			return s.saveLocked()
		}
	}
	return errors.New("员工不存在: " + id)
}

// Duplicate 复制员工（§2.3：配置复制 + 工作空间文件复制；新员工置草稿态）。
func (s *Store) Duplicate(id string) (Employee, error) {
	src, ok := s.Get(id)
	if !ok {
		return Employee{}, errors.New("员工不存在: " + id)
	}
	cp := src
	cp.ID = ""
	cp.Name = strings.TrimSpace(src.Name) + " 副本"
	cp.Status = StatusDraft
	// EHR 复制语义：工号必复（新工号），上级/部门/岗位/角色随复制（人事档案复制）。
	cp.EmpNo = ""
	cp.ManagerID = ""
	cp.UpdatedAt = time.Now()
	cp.CreatedAt = time.Now()
	// §3 数据隔离：副本回落默认工作空间，避免与源员工共写同一自定义目录
	//（copyWorkspace 会把源工作空间内容复制到副本的默认工作空间）。
	cp.WorkspaceRoot = ""
	out, err := s.Add(cp)
	if err != nil {
		return out, err
	}
	// 工作空间内容复制（memory/evolution-drafts 随行）。
	s.copyWorkspace(src, out)
	return out, nil
}

// InitWorkspace 初始化员工工作空间骨架（§3.2 + §11.4 记忆路由文件）。
func (s *Store) InitWorkspace(e Employee) error {
	ws := e.Workspace(s.dir)
	for _, d := range []string{
		ws,
		joinPath(ws, "memory"),
		joinPath(ws, "evolution-drafts", "pending"),
		joinPath(ws, "evolution-drafts", "approved"),
		joinPath(ws, "evolution-drafts", "rejected"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// 记忆路由文件占位（空文件；引擎可 fs_read/fs_write 维护）。
	for _, f := range []string{"BEHAVIOR.md", "TOOLS.md", "MEMORY.md", "WORKFLOWS.md"} {
		p := joinPath(ws, "memory", f)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			_ = os.WriteFile(p, []byte("# "+trimExt(f)+"\n\n（由进化提案审批后自动沉淀；数字员工可按需补充。）\n"), 0o644)
		}
	}
	return nil
}

func (s *Store) copyWorkspace(from, to Employee) {
	src := from.Workspace(s.dir)
	dst := to.Workspace(s.dir)
	copyTree(src, dst)
}
