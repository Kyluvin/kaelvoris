package desktop

// employee_panel.go —— 数字员工管理面板（Wails 绑定，规格 §2/§11.5/§14）：
//
//	员工 CRUD（新建/编辑/状态/复制/删除）+ 安全摘要（§14「MCP白名单 5 项 ·
//	目录 2 个 · 只读开」）+ 进化提案审批闭环（§11.5 写草稿 → 审批 → 落地）
//	+ 审计日志查询（§8.2 audit.jsonl）+ EHR 组织架构「人事档案」+ A2A 群聊。
//
// 事件推送约定（前端 index.html/main.js 消费）：
//   - kaelvoris:employees → 员工列表变化（CRUD/状态/复制后）
//   - kaelvoris:proposals → 待审批提案变化（审批/拒绝后；新提案落 pending 目录
//     后前端打开面板时拉取 ListAllProposals 刷新）
//   - kaelvoris:groups → 群聊列表变化；kaelvoris:group_msg → 群消息；
//     kaelvoris:group_task_done → 群任务完成

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"kaelvoris/internal/employee"
	"kaelvoris/internal/mcp"
	"kaelvoris/internal/plugin"
	"kaelvoris/internal/remote"
)

// EmployeeView 员工列表条目（Employee + 面板展示派生字段）。
type EmployeeView struct {
	employee.Employee
	SecuritySummary string   `json:"securitySummary"`        // §14 安全摘要文案
	WorkspacePath   string   `json:"workspacePath"`          // 工作空间绝对路径
	Channels        []string `json:"channels,omitempty"`     // 绑定该员工的启用渠道名
	Runnable        bool     `json:"runnable"`               // 是否可执行任务（composer 选择器置灰）
	Proposals       int      `json:"proposals"`              // 待审批提案数（徽标）
	MemoryFiles     []string `json:"memoryFiles,omitempty"`  // memory/ 已沉淀文件名
	DeptName        string   `json:"deptName,omitempty"`     // 部门名称（EHR 派生）
	PositionName    string   `json:"positionName,omitempty"` // 岗位名称（EHR 派生）
	ManagerName     string   `json:"managerName,omitempty"`  // 直属上级姓名（EHR 派生）
}

// employeeView 组装单条视图。chNames 可传 nil（单条查询时自动读取渠道配置）；
// 列表路径由 ListEmployees 预读一次批量复用，避免逐员工重复读盘（N+1）。
// orgSnapshot 可传预读的组织字典快照（列表路径复用；单条路径不传自动拉取）。
// proposals 预取的提案列表（列表路径由 ListEmployees 并行预取复用；传 nil
// 时单条路径自动拉取——此前列表路径内层仍串行扫描一遍 pending 目录，与
// 外层并行预取重复，N 员工 × 双份目录 IO 白做一遍）。
func (a *App) employeeView(e employee.Employee, chNames []string, proposals []employee.Proposal, orgSnapshot ...employee.OrgView) EmployeeView {
	if chNames == nil {
		for _, c := range a.ListChannels() {
			if c.Enabled && c.EmployeeID == e.ID {
				chNames = append(chNames, c.Name)
			}
		}
	}
	if proposals == nil {
		proposals, _ = a.empStore.ListProposals(e.ID)
	}
	ws := e.Workspace(a.empStore.Dir())
	memFiles := []string{}
	if entries, err := os.ReadDir(filepath.Join(ws, "memory")); err == nil {
		for _, en := range entries {
			if !en.IsDir() && strings.HasSuffix(en.Name(), ".md") {
				memFiles = append(memFiles, en.Name())
			}
		}
	}
	// EHR 派生名称回填（组织字典：部门/岗位/直属上级名称；供面板与角色提示词段）。
	a.resolveEmpOrgNames(&e, orgSnapshot...)
	return EmployeeView{
		Employee:        e,
		SecuritySummary: e.SecuritySummary(),
		WorkspacePath:   ws,
		Channels:        chNames,
		Runnable:        e.Runnable(false),
		Proposals:       len(proposals),
		MemoryFiles:     memFiles,
		DeptName:        e.DeptName,
		PositionName:    e.PositionName,
		ManagerName:     e.ManagerName,
	}
}

// pushEmployees 推送员工列表事件（kaelvoris:employees）。
func (a *App) pushEmployees() {
	if a.ctx == nil {
		return
	}
	uiEmit(a.ctx, "kaelvoris:employees", a.ListEmployees())
}

// pushProposals 推送待审批提案事件（kaelvoris:proposals）。
func (a *App) pushProposals() {
	if a.ctx == nil {
		return
	}
	uiEmit(a.ctx, "kaelvoris:proposals", a.ListAllProposals())
}

// ListEmployees 员工列表（按创建时间正序；含安全摘要/工作空间/绑定渠道/提案数）。
func (a *App) ListEmployees() []EmployeeView {
	if a.empStore == nil {
		return []EmployeeView{}
	}
	all := a.empStore.List()
	out := make([]EmployeeView, 0, len(all))
	// 渠道配置预读一次（文件 IO），批量映射员工→渠道名，避免逐员工重读（N+1）。
	chOf := map[string][]string{}
	for _, c := range a.ListChannels() {
		if c.Enabled && c.EmployeeID != "" {
			chOf[c.EmployeeID] = append(chOf[c.EmployeeID], c.Name)
		}
	}
	// 组织字典预读一次（resolveEmpOrgNames 原本逐员工拉全量 OrgView，3×N 扫描）。
	org := employee.OrgView{}
	if a.orgStore != nil {
		org = a.orgStore.View()
	}
	// 提案扫描并行化：每员工的 pending 目录 IO（原串行扫描，员工多时明显）。
	var wg sync.WaitGroup
	props := make([][]employee.Proposal, len(all))
	for i, e := range all {
		wg.Add(1)
		go func(i int, e employee.Employee) {
			defer wg.Done()
			p, _ := a.empStore.ListProposals(e.ID)
			props[i] = p
		}(i, e)
	}
	wg.Wait()
	for i, e := range all {
		v := a.employeeView(e, chOf[e.ID], props[i], org)
		out = append(out, v)
	}
	return out
}

// GetEmployee 员工详情（编辑器弹窗回填）。
func (a *App) GetEmployee(id string) (EmployeeView, error) {
	e, ok := a.empStore.Get(id)
	if !ok {
		return EmployeeView{}, errors.New("员工不存在: " + id)
	}
	return a.employeeView(e, nil, nil), nil
}

// SaveEmployee 新建/更新员工（ID 空 = 新建；§2.1 名称唯一校验由 Store 保证）。
// doc/09-设计方案 §3/§4.4：新增/编辑岗位必填（硬校验）——干活的数字员工
// 必须全部关联岗位（需求①强约束）。存量兼容（§3 表格语义）：存量员工无岗位
// 不阻断运行，仅保存路径校验（不改存量数据；面板待办清单引导下次补齐）。
func (a *App) SaveEmployee(e employee.Employee) (employee.Employee, error) {
	e.Name = strings.TrimSpace(e.Name)
	e.PositionID = strings.TrimSpace(e.PositionID)
	if e.PositionID == "" {
		return e, errors.New("数字员工必须分配岗位（doc/09-设计方案 §3 岗位强约束）: " + e.Name)
	}
	if !a.positionExists(e.PositionID) {
		return e, errors.New("岗位不存在: " + e.PositionID)
	}
	var out employee.Employee
	var err error
	if strings.TrimSpace(e.ID) == "" {
		out, err = a.empStore.Add(e)
	} else {
		// 指定 ID：已存在则更新，不存在则带 ID 新建（Add 支持外部指定 ID，
		// 修复调用方带预设 ID 首次保存报「员工不存在」——Update 语义仅适用于既有条目）。
		if _, ok := a.empStore.Get(strings.TrimSpace(e.ID)); ok {
			out, err = a.empStore.Update(e)
		} else {
			out, err = a.empStore.Add(e)
		}
	}
	if err != nil {
		return out, err
	}
	a.pushEmployees()
	return out, nil
}

// SetEmployeeStatus 状态管理（§2.2：enabled | disabled | draft | archived）。
// 停用/归档后：运行中任务不受影响（§9.1 生效版本语义），新任务回退普通会话；
// 绑定该员工的启用渠道不再就绪（SaveChannels 校验 + 引擎侧 Runnable 拦截）。
func (a *App) SetEmployeeStatus(id, status string) (employee.Employee, error) {
	e, err := a.empStore.SetStatus(id, status)
	if err != nil {
		return employee.Employee{}, err
	}
	a.pushEmployees()
	return e, nil
}

// SetEmpStatus 人事状态管理（EHR：active | probation | suspended | resigned）。
// 与运行状态（Status）解耦：离职/人事停用后员工不可运行（Runnable 拦截），
// 档案保留可随时改回在职。直接生效，无审核。
func (a *App) SetEmpStatus(id, empStatus string) (employee.Employee, error) {
	e, err := a.empStore.SetEmpStatus(id, empStatus)
	if err != nil {
		return employee.Employee{}, err
	}
	a.pushEmployees()
	return e, nil
}

// DuplicateEmployee 复制员工（§2.3 模板化：配置 + 工作空间全复制，新员工置草稿态）。
func (a *App) DuplicateEmployee(id string) (employee.Employee, error) {
	e, err := a.empStore.Duplicate(id)
	if err != nil {
		return employee.Employee{}, err
	}
	a.pushEmployees()
	return e, nil
}

// DeleteEmployee 删除员工（工作空间目录保留，用户数据不静默删除，§8.4）。
// 资源占用门禁（与 DeletePosition 同口径）：渠道（含停用）/ cron 任务 /
// 会话绑定任一引用即拒并列出来源——否则删除后 cron/远程触发时报「绑定的
// 数字员工不存在」终止任务（桌面会话运行时静默降级，资源项触发链路无降级
// 池），用户却不知根因是这里。停用渠道同样拦：绑定是休眠引用（重新启用时
// 校验），删员工后重新启用渠道报「员工不存在」，错误信息列出占用来源可解。
func (a *App) DeleteEmployee(id string) error {
	var holders []string
	for _, c := range a.ListChannels() {
		if c.EmployeeID == id {
			holders = append(holders, "渠道「"+c.Name+"」")
		}
	}
	for _, j := range a.cronStore.List() {
		if j.EmployeeID == id {
			holders = append(holders, "定时任务「"+j.Name+"」")
		}
	}
	if a.store != nil {
		for _, s := range a.store.List() {
			if s.EmployeeID == id && s.ParentID == "" {
				holders = append(holders, "会话「"+s.Title+"」")
			}
		}
	}
	if len(holders) > 0 {
		return errors.New("该员工已被资源项绑定，请先解除绑定: " + strings.Join(holders, "、"))
	}
	if err := a.empStore.Delete(id); err != nil {
		return err
	}
	a.pushEmployees()
	return nil
}

// ---------- 进化提案审批（§11.5）----------

// ProposalView 提案 + 所属员工名（前端审批卡展示）。
type ProposalView struct {
	employee.Proposal
	EmployeeName string `json:"employeeName"`
	TargetName   string `json:"targetName"` // BEHAVIOR/TOOLS/WORKFLOWS/MEMORY 展示名
}

// ListProposals 某员工的待审批提案。
func (a *App) ListProposals(empID string) ([]employee.Proposal, error) {
	return a.empStore.ListProposals(empID)
}

// ListAllProposals 全部员工的待审批提案（侧栏徽标 + 提案审批卡数据源）。
func (a *App) ListAllProposals() []ProposalView {
	views := []ProposalView{}
	if a.empStore == nil {
		return views
	}
	for _, e := range a.empStore.List() {
		props, err := a.empStore.ListProposals(e.ID)
		if err != nil {
			continue
		}
		for _, p := range props {
			views = append(views, ProposalView{Proposal: p, EmployeeName: e.Name, TargetName: proposalTargetName(p.TargetFile)})
		}
	}
	sort.Slice(views, func(i, j int) bool { return views[i].CreatedAt.Before(views[j].CreatedAt) })
	return views
}

// proposalTargetName 记忆路由文件展示名（§11.4 路由目标）。
func proposalTargetName(t string) string {
	switch strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(t, "memory/"), `memory\`))) {
	case "BEHAVIOR.MD":
		return "行为记忆"
	case "TOOLS.MD":
		return "工具经验"
	case "WORKFLOWS.MD":
		return "工作流"
	default:
		return "通用记忆"
	}
}

// ApproveProposal 审批通过：草稿迁 approved/ + 正文按 Target-File 落地 memory/
// （§11.5；modifiedContent 非空 = 用户修改后落地）。落地后员工下一任务生效（§11.6）。
func (a *App) ApproveProposal(empID, fileName, modifiedContent string) error {
	if err := a.empStore.ApproveProposal(empID, fileName, modifiedContent); err != nil {
		return err
	}
	a.pushProposals()
	a.pushEmployees()
	return nil
}

// RejectProposal 审批拒绝：草稿迁 rejected/ 留痕（可回溯）。
func (a *App) RejectProposal(empID, fileName string) error {
	if err := a.empStore.RejectProposal(empID, fileName); err != nil {
		return err
	}
	a.pushProposals()
	a.pushEmployees()
	return nil
}

// ---------- 审计日志（§8.2 audit.jsonl，只读查询）----------

// AuditEntry 审计日志条目（JSONL 行解析结果，字段对齐 Gateway.auditLog）。
type AuditEntry struct {
	Time     string `json:"ts"`
	Employee string `json:"employee"`
	Tool     string `json:"tool"`
	Args     string `json:"args,omitempty"`
	Decision string `json:"decision"` // allow | deny
	Reason   string `json:"reason,omitempty"`
}

// AuditEntry 上限：单条审计记录（含截断后 args）的最大字节量，超出则丢弃该行。
const auditMaxLineBytes = 8 << 10

// ListAuditLogs 读取员工审计日志（倒序取 limit 条；文件不存在 = 空列表）。
// 流式倒读：从文件尾向前按块扫描，仅保留尾部 ~64KB 窗口——长期运行的员工
// audit.jsonl 会持续增长，全量 ReadFile+Split 的内存与延迟随历史线性膨胀；
// 倒序取 limit 条只需尾部数据。窗口内不足 limit 条时如实返回（UI 显示近期）。
func (a *App) ListAuditLogs(empID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	e, ok := a.empStore.Get(empID)
	if !ok {
		return nil, errors.New("员工不存在: " + empID)
	}
	p := filepath.Join(a.empStore.Dir(), "数字员工", e.ID, "audit.jsonl")
	f, err := os.Open(p)
	if err != nil {
		return []AuditEntry{}, nil // 尚无审计（从未执行任务）= 空列表
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return []AuditEntry{}, nil
	}
	// 窗口：最多读尾部 64KB（覆盖 500 条 × 128B 的典型行远有余）。
	const window = 64 << 10
	offset := info.Size() - window
	if offset < 0 {
		offset = 0
	}
	b := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(b, offset); err != nil && err != io.EOF {
		return []AuditEntry{}, nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	out := []AuditEntry{}
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || len(line) > auditMaxLineBytes {
			continue
		}
		var en AuditEntry
		if json.Unmarshal([]byte(line), &en) == nil && en.Tool != "" {
			out = append(out, en)
		}
	}
	return out, nil
}

// OpenWorkspaceInExplorer 在系统文件管理器打开员工工作空间（运维便利）。
func (a *App) OpenWorkspaceInExplorer(id string) error {
	e, ok := a.empStore.Get(id)
	if !ok {
		return errors.New("员工不存在: " + id)
	}
	ws := e.Workspace(a.empStore.Dir())
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return err
	}
	return openInExplorer(abs)
}

// ---------- 组织架构 / 岗位 / 角色（EHR 人员与组织管控）----------
//
// 绑定方法经 frontend/wailsjs 自动生成（App.GetOrg / SaveDepartment 等）。
// 全部直接录入、直接生效（无审核流程）；引用约束由 OrgStore 校验。

// GetOrg 组织架构全量视图（部门 + 岗位 + 角色；前端组织 Tab 数据源）。
func (a *App) GetOrg() employee.OrgView {
	if a.orgStore == nil {
		return employee.OrgView{}
	}
	return a.orgStore.View()
}

// OrgTree 部门树（前端组织树渲染；节点统计由前端按员工列表聚合）。
func (a *App) OrgTree() []*employee.DeptTree {
	if a.orgStore == nil {
		return []*employee.DeptTree{}
	}
	return a.orgStore.DeptTree()
}

// SaveDepartment 新建/更新部门（ID 空 = 新建；直接生效）。
func (a *App) SaveDepartment(d employee.Department) (employee.Department, error) {
	if a.orgStore == nil {
		return d, errors.New("组织存储未初始化")
	}
	var out employee.Department
	var err error
	if strings.TrimSpace(d.ID) == "" {
		out, err = a.orgStore.AddDepartment(d)
	} else {
		out, err = a.orgStore.UpdateDepartment(d)
	}
	if err != nil {
		return out, err
	}
	a.pushEmployees()
	return out, nil
}

// DeleteDepartment 删除部门（引用完整性校验：有子部门/岗位/员工时拒绝）。
func (a *App) DeleteDepartment(id string) error {
	if a.orgStore == nil {
		return errors.New("组织存储未初始化")
	}
	if err := a.orgStore.DeleteDepartment(id); err != nil {
		return err
	}
	a.pushEmployees()
	return nil
}

// SavePosition 新建/更新岗位（ID 空 = 新建；直接生效）。RoleIDs 岗位-角色
// 映射逐个校验角色字典存在性（误填直接拒绝，不静默丢弃——业界 RBAC 主路径，
// 映射断链 = 权限静默失效，必须拦在保存口）。
func (a *App) SavePosition(p employee.Position) (employee.Position, error) {
	if a.orgStore == nil {
		return p, errors.New("组织存储未初始化")
	}
	roleIDs := make([]string, 0, len(p.RoleIDs))
	seen := map[string]bool{}
	for _, r := range p.RoleIDs {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		if !a.roleExists(r) {
			return p, errors.New("角色不存在: " + r)
		}
		seen[r] = true
		roleIDs = append(roleIDs, r)
	}
	p.RoleIDs = roleIDs
	var out employee.Position
	var err error
	if strings.TrimSpace(p.ID) == "" {
		out, err = a.orgStore.AddPosition(p)
	} else {
		out, err = a.orgStore.UpdatePosition(p)
	}
	if err != nil {
		return out, err
	}
	a.pushEmployees()
	return out, nil
}

// roleExists 角色字典存在性（岗位 RoleIDs 映射校验用；零拷贝锁内扫描）。
func (a *App) roleExists(roleID string) bool {
	return a.orgStore.RoleExists(roleID)
}

// DeletePosition 删除岗位（doc/09-设计方案 §4.4：资源占用校验——被
// Employee / ChannelConfig / cron.Job 任一引用时拒绝删除，错误信息列出
// 占用来源；Employee 引用沿用 OrgStore 既有校验，资源项占用在此聚合）。
func (a *App) DeletePosition(id string) error {
	if a.orgStore == nil {
		return errors.New("组织存储未初始化")
	}
	// 资源项占用前置校验（§4 验收：被渠道/cron 引用时拒绝删除）。
	var holders []string
	for _, c := range a.ListChannels() {
		if c.PositionID == id {
			holders = append(holders, "渠道「"+c.Name+"」")
		}
	}
	for _, j := range a.cronStore.List() {
		if j.PositionID == id {
			holders = append(holders, "定时任务「"+j.Name+"」")
		}
	}
	// 会话纯岗位绑定（EmployeeID 空 + PositionID 非空）是活引用：触发时经
	// ResolveAssignment 从岗位池选人（remote_panel 首条消息路径），岗位被删后
	// 报「岗位 X 下无可运行数字员工」无根因提示。EmployeeID 非空时岗位仅快照
	// 展示（员工引用由 DeleteEmployee 拦），不重复拦。委派子会话跟随父会话。
	if a.store != nil {
		for _, s := range a.store.List() {
			if s.ParentID == "" && s.EmployeeID == "" && s.PositionID == id {
				holders = append(holders, "会话「"+s.Title+"」")
			}
		}
	}
	if len(holders) > 0 {
		return errors.New("该岗位已被资源项绑定，请先解除绑定: " + strings.Join(holders, "、"))
	}
	if err := a.orgStore.DeletePosition(id); err != nil {
		return err // 员工引用拒绝（OrgStore 既有校验，含「岗位不存在」）
	}
	a.pushEmployees()
	return nil
}

// positionExists 岗位字典存在性（渠道/员工/资源绑定校验共用；orgStore 未
// 初始化或岗位 ID 为空时按 true 兑底——空岗位由 cron.ValidateBinding 拦）。
func (a *App) positionExists(posID string) bool {
	if posID == "" || a.orgStore == nil {
		return true
	}
	return a.orgStore.PositionExists(posID)
}

// ---------- 资源绑定（doc/09-设计方案 §5.5/§5.6：资源管理面板）----------
//
// 单一事实源（§3.1）：绑定字段只存资源项配置侧，本组方法为聚合读写视图，
// 写路径转发 SaveChannels / UpdateCronJob 既有校验，不新增平行校验（§11-4）。
// 本期覆盖渠道（channel）+ 定时任务（cron）两类资源；会话资源维持既有
// SetSessionEmployee 交互（§6.1：桌面会话不新增直接绑岗位）。

// ResourceBinding 统一资源绑定条目（ListResourceBindings 返回；同构
// §5.5 岗位视图行与三 Tab 行，SaveResourceBinding 按 type+id 定位写回）。
type ResourceBinding struct {
	Type       string `json:"type"` // channel | cron | session
	ID         string `json:"id"`   // 渠道 type:name 复合标识 / cron Job ID / 会话 ID
	Name       string `json:"name"` // 展示名（渠道名 / 任务名 / 会话标题）
	Enabled    bool   `json:"enabled"`
	Running    bool   `json:"running,omitempty"`    // 会话运行中（运行中拒绝改绑定）
	EmployeeID string `json:"employeeId,omitempty"` // binding.employeeId
	PositionID string `json:"positionId,omitempty"` // binding.positionId（岗位快照）
	AssignRule string `json:"assignRule,omitempty"` // random | round_robin | least_loaded；空 = random
}

// ListResourceBindings 聚合渠道 + 定时任务 + 会话三类资源的绑定（§5.6；
// 资源管理面板与岗位视图「该岗位下挂了哪些资源」共用数据源）。
// 会话资源：EmployeeID/PositionID/AssignRule 随会话持久化（Summary 已带岗位
// 快照，§5.2 会话 Tab 数据源），改绑走 SetSessionBinding（运行中拒绝）。
func (a *App) ListResourceBindings() []ResourceBinding {
	out := []ResourceBinding{}
	for _, c := range a.ListChannels() {
		out = append(out, ResourceBinding{
			Type:       "channel",
			ID:         channelBindingID(c.Type, c.Name),
			Name:       c.Name,
			Enabled:    c.Enabled,
			EmployeeID: strings.TrimSpace(c.EmployeeID),
			PositionID: strings.TrimSpace(c.PositionID),
			AssignRule: strings.TrimSpace(c.AssignRule),
		})
	}
	for _, j := range a.cronStore.List() {
		out = append(out, ResourceBinding{
			Type:       "cron",
			ID:         j.ID,
			Name:       j.Name,
			Enabled:    j.Enabled,
			EmployeeID: strings.TrimSpace(j.EmployeeID),
			PositionID: strings.TrimSpace(j.PositionID),
			AssignRule: strings.TrimSpace(j.AssignRule),
		})
	}
	running := map[string]bool{}
	a.runsMu.Lock()
	for id := range a.runs {
		running[id] = true
	}
	a.runsMu.Unlock()
	for _, s := range a.store.List() {
		if s.ParentID != "" {
			continue // 委派子会话不作为可绑定资源项展示（跟随父会话员工）
		}
		out = append(out, ResourceBinding{
			Type:       "session",
			ID:         s.ID,
			Name:       s.Title,
			Running:    s.Running,
			EmployeeID: strings.TrimSpace(s.EmployeeID),
			PositionID: strings.TrimSpace(s.PositionID),
			AssignRule: strings.TrimSpace(s.AssignRule),
		})
	}
	return out
}

// channelBindingID 渠道资源项标识（channels.json 无主键：type+name 复合）。
func channelBindingID(typ, name string) string { return typ + ":" + name }

// SaveResourceBinding 统一绑定写入口（§5.6：内部转发既有校验，不另起炉灶）：
//   - channel：按 type:name 定位，改 EmployeeID/PositionID/AssignRule 后整表
//     回写 SaveChannels（自动获得 §4.4 全部校验 + 岗位快照回填 + 热重载）；
//   - cron：取 Job 改绑定三字段后走 UpdateCronJob（cron.ValidateBinding 在
//     store 层自动生效）；
//   - session：改会话 EmployeeID + 岗位快照（SetSessionBinding：运行中拒绝、
//     员工存在性/岗位字典校验同源，§6.1 会话资源可直接绑岗位或员工）。
//
// 全空绑定 = 解绑回裸跑（启用渠道被既有校验拒绝；cron/会话合法 = 存量语义）。
func (a *App) SaveResourceBinding(resourceType, resourceID, employeeID, positionID, assignRule string) error {
	empID := strings.TrimSpace(employeeID)
	posID := strings.TrimSpace(positionID)
	switch resourceType {
	case "session":
		return a.SetSessionBinding(resourceID, empID, posID, strings.TrimSpace(assignRule))
	case "channel":
		typ, name, ok := strings.Cut(resourceID, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return errors.New("渠道资源标识无效（应为 type:name）: " + resourceID)
		}
		channels := a.ListChannels()
		found := false
		for i := range channels {
			if channels[i].Type == typ && channels[i].Name == name {
				channels[i].EmployeeID = empID
				channels[i].PositionID = posID
				channels[i].AssignRule = strings.TrimSpace(assignRule)
				found = true
				break
			}
		}
		if !found {
			return errors.New("渠道不存在: " + resourceID)
		}
		return a.SaveChannels(channels) // 整表替换 + §4.4 校验 + 热重载
	case "cron":
		for _, j := range a.cronStore.List() {
			if j.ID == strings.TrimSpace(resourceID) {
				j.EmployeeID = empID
				j.PositionID = posID
				j.AssignRule = strings.TrimSpace(assignRule)
				_, err := a.UpdateCronJob(j)
				return err
			}
		}
		return errors.New("定时任务不存在: " + resourceID)
	default:
		return errors.New("不支持的资源类型（channel|cron）: " + resourceType)
	}
}

// SetSessionBinding 会话绑定写路径（资源管理面板会话 Tab）：改会话员工绑定
// + 岗位快照（empID 空且 posID 非空 = 纯岗位绑定，首条消息时经渠道同一收口
// ResolveAssignment 从岗位池选人；全空 = 解绑回普通会话）。语义复用
// SetSessionEmployee：运行中拒绝（本轮已按旧配置运行）；员工存在性校验同源。
func (a *App) SetSessionBinding(id, employeeID, positionID, assignRule string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("会话不能为空")
	}
	s, err := a.store.Get(id)
	if err != nil || s == nil {
		return errors.New("会话不存在")
	}
	if a.IsSessionRunning(id) {
		return errors.New("任务运行中，请等待完成或停止后再切换")
	}
	empID := strings.TrimSpace(employeeID)
	posID := strings.TrimSpace(positionID)
	if empID != "" {
		if _, ok := a.empStore.Get(empID); !ok {
			return errors.New("数字员工不存在: " + empID)
		}
		if posID == "" {
			// 选员工自动带出岗位快照（§5.3 快照回填语义，与 BindEditor 同步）。
			if e, ok := a.empStore.Get(empID); ok {
				posID = e.PositionID
			}
		}
	}
	if posID != "" && !a.positionExists(posID) {
		return errors.New("岗位不存在: " + posID)
	}
	s.SetEmployeeID(empID)
	s.SetPositionSnapshot(posID, strings.TrimSpace(assignRule))
	a.store.Save(s)
	a.pushSessionsChanged()
	return nil
}

// SaveRole 新建/更新角色（ID 空 = 新建；直接生效）。
func (a *App) SaveRole(r employee.Role) (employee.Role, error) {
	if a.orgStore == nil {
		return r, errors.New("组织存储未初始化")
	}
	var out employee.Role
	var err error
	if strings.TrimSpace(r.ID) == "" {
		out, err = a.orgStore.AddRole(r)
	} else {
		out, err = a.orgStore.UpdateRole(r)
	}
	if err != nil {
		return out, err
	}
	a.pushEmployees()
	return out, nil
}

// DeleteRole 删除角色（被员工直绑或岗位映射引用时拒绝，并列出来源——
// 岗位-角色是权限主路径，静默断链会导致岗位下员工权限收缩）。
func (a *App) DeleteRole(id string) error {
	if a.orgStore == nil {
		return errors.New("组织存储未初始化")
	}
	var holders []string
	for _, e := range a.empStore.List() {
		e.NormalizeRoleIDs()
		for _, rid := range e.RoleIDs {
			if rid == id {
				holders = append(holders, "员工「"+e.Name+"」")
				break
			}
		}
	}
	for _, p := range a.orgStore.View().Positions {
		for _, rid := range p.RoleIDs {
			if rid == id {
				holders = append(holders, "岗位「"+p.Name+"」")
				break
			}
		}
	}
	if len(holders) > 0 {
		return errors.New("该角色已被引用，请先解除绑定: " + strings.Join(holders, "、"))
	}
	if err := a.orgStore.DeleteRole(id); err != nil {
		return err
	}
	a.pushEmployees()
	return nil
}

// ---------- §4 MCP / SKILLS 权限：工具目录（前端权限勾选数据源）----------

// MCPToolInfo 目录中的单个工具（FullName = mcp_<server>_<tool>，与员工
// MCPTools 通配、Gateway 拦截使用的是同一全名）。
type MCPToolInfo struct {
	Name        string          `json:"name"`     // 服务器侧原始工具名
	FullName    string          `json:"fullName"` // mcp_<server>_<tool>（sanitize 后，模型可见名）
	Desc        string          `json:"desc,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"` // 参数 JSON Schema（MCP 设置页「查看工具」展示用；权限目录场景为空）
}

// MCPServerCatalog 单个 MCP 服务器的目录条目（Server 即员工 MCPServers
// 白名单所填的服务器名）。
type MCPServerCatalog struct {
	Server string        `json:"server"`
	Source string        `json:"source"` // config（mcp-servers.json）| plugin
	Tools  []MCPToolInfo `json:"tools"`
}

// ListMCPToolCatalog MCP 工具目录：连接全部已启用的 MCP 服务器（手动配置 +
// 插件自动发现），枚举各自工具，返回 server→tools 供前端勾选服务器/工具白名单。
// 仅供面板按需调用（连接失败的服务器跳过并附错误，不阻断）；员工侧过滤不在此做
// （目录展示全量可选集，实际生效由员工配置 + Gateway 双保险）。
func (a *App) ListMCPToolCatalog() []MCPServerCatalog {
	var out []MCPServerCatalog
	seen := map[string]bool{}
	add := func(name, source string, cfg mcp.ServerConfig) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		entry := MCPServerCatalog{Server: name, Source: source, Tools: []MCPToolInfo{}}
		mctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cli, _, err := mcp.RegisterAll(mctx, cfg, func(fullName string, t *mcp.RemoteTool) error {
			entry.Tools = append(entry.Tools, MCPToolInfo{Name: t.ServerTool, FullName: fullName, Desc: t.Desc})
			return nil
		})
		if err != nil {
			log.Printf("ListMCPToolCatalog: 服务器 %s 连接失败（已跳过）: %v", name, err)
			return
		}
		cli.Close()
		out = append(out, entry)
	}
	for _, p := range plugin.List() {
		if !plugin.IsEnabled(p.ID) {
			continue
		}
		name, transport, command, url, args, env, headers, ok := p.MCPServerOf()
		if !ok || (transport == "stdio" && strings.TrimSpace(command) == "") || (transport != "stdio" && strings.TrimSpace(url) == "") {
			continue
		}
		if (transport == "stdio" || transport == "") && command != "" && !filepath.IsAbs(command) {
			command = filepath.Join(p.Dir, command)
		}
		add(name, "plugin", mcp.ServerConfig{Name: name, Command: command, Args: args, Env: env, URL: url, Headers: headers})
	}
	for _, s := range a.loadMCPServers() {
		if !s.Enabled {
			continue
		}
		switch s.Transport {
		case "", "stdio":
			if strings.TrimSpace(s.Command) == "" {
				continue
			}
		case "sse", "http":
			if strings.TrimSpace(s.URL) == "" {
				continue
			}
		default:
			continue
		}
		add(s.Name, "config", mcp.ServerConfig{Name: s.Name, Command: s.Command, Args: s.Args, Env: s.Env, URL: s.URL, Headers: s.Headers})
	}
	return out
}

// applyEmpRole 运行期套用角色权限包（role.json → 员工有效配置，收紧式合并）：
// 员工有效角色 = 直绑角色（RoleIDs，兼容位 RoleID 归一化后为首位）∪ 岗位
// 映射角色（Position.RoleIDs，业界 RBAC 主路径；直绑仅兜底临时授权）。
// 逐个按收紧式合并叠加（ApplyRoleRestrictions 只减不加，多角色依次叠加
// 仍只减不加，叠加顺序不影响结果）；停用/不存在的角色跳过不叠加。
// 调用点：buildRunEngine 装配员工后（定义侧 buildMCPTools / 提示词注入与
// 执行侧 Gateway 均读员工有效配置，合并一次双路生效）。
func (a *App) applyEmpRole(e *employee.Employee) {
	if e == nil || a.orgStore == nil {
		return
	}
	e.NormalizeRoleIDs()
	roleIDs := append([]string{}, e.RoleIDs...)
	// 岗位映射角色并入（去重；直绑在前=优先级展示序，合并语义与顺序无关）。
	if posID := strings.TrimSpace(e.PositionID); posID != "" {
		seen := map[string]bool{}
		for _, id := range roleIDs {
			seen[id] = true
		}
		for _, id := range a.orgStore.RoleIDsByPosition(posID) {
			if !seen[id] {
				seen[id] = true
				roleIDs = append(roleIDs, id)
			}
		}
	}
	if len(roleIDs) == 0 {
		return
	}
	roles := a.orgStore.View().Roles
	byID := make(map[string]employee.Role, len(roles))
	for _, r := range roles {
		if r.Status != employee.OrgDisabled {
			byID[r.ID] = r
		}
	}
	for _, id := range roleIDs {
		if r, ok := byID[id]; ok {
			e.ApplyRoleRestrictions(r)
		}
	}
}

// resolveEmpOrgNames 回填员工的组织身份名称（DeptName/PositionName/ManagerName；
// 运行期构建引擎与面板视图共用，供角色提示词段与前端展示）。批量路径传 snapshot
// （ListEmployees 预读一次 OrgView 复用，避免 N 个员工各自拉全量字典——原来每次
// 调用都做三遍 O(n) 扫描，N 员工 × 3 字典 → O(N×M)；单条路径 snapshot 传 nil
// 自动拉取，保持旧签名兼容）。
func (a *App) resolveEmpOrgNames(e *employee.Employee, snapshot ...employee.OrgView) {
	if e == nil || a.orgStore == nil {
		return
	}
	org := employee.OrgView{}
	if len(snapshot) > 0 {
		org = snapshot[0]
	} else {
		org = a.orgStore.View()
	}
	for _, d := range org.Departments {
		if d.ID == e.DeptID {
			e.DeptName = d.Name
			break
		}
	}
	for _, p := range org.Positions {
		if p.ID == e.PositionID {
			e.PositionName = p.Name
			break
		}
	}
	if e.ManagerID != "" {
		if m, ok := a.empStore.Get(e.ManagerID); ok {
			e.ManagerName = m.Name
		}
	}
}

// ---------- A2A 群聊（桌面端入口；节点侧 remote_group.go 为群状态权威）----------
//
// 绑定方法经 frontend/wailsjs 自动生成（App.GroupList / GroupCreate / GroupMsg …）。
// 说明：移动端经服务端 group.* 路由到本节点；桌面端直接在节点侧调用 remoteGroup*
//（本机即群状态权威），无需服务端在线。事件：kaelvoris:groups（群列表变化）、
// kaelvoris:group_msg（新群消息）、kaelvoris:group_task_done（群任务完成）。

// GroupList 群聊列表（节点侧群状态快照，含成员与最新消息摘要）。
func (a *App) GroupList() []remote.Group {
	st := a.ensureGroupStore()
	return st.all()
}

// GroupCreate 发起群聊（名称 + 员工成员 ID 列表；群助手自动加入）。
func (a *App) GroupCreate(name string, members []string) (remote.Group, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return remote.Group{}, errors.New("请输入群名称")
	}
	// 直接在节点侧建群（本地权威），建群后广播 kaelvoris:groups。
	a.remoteGroupCreate(remote.GroupServerReq{Name: name, Members: members})
	a.pushGroups()
	st := a.ensureGroupStore()
	all := st.all()
	if len(all) > 0 {
		return all[len(all)-1], nil
	}
	return remote.Group{}, errors.New("群聊创建失败")
}

// GroupMsg 发送群消息（用户发言入群历史并广播）。
func (a *App) GroupMsg(groupID, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("消息不能为空")
	}
	a.remoteGroupMsg(remote.GroupServerReq{GroupID: groupID, Text: text})
	a.pushGroupMsg(groupID)
	return nil
}

// GroupTask 派发群写任务（群助手协调 → 成员员工协作执行 → 汇总回群）。
// mode 协作模式：chain 接力（默认）/ parallel 并行分工 / review 评审打磨。
func (a *App) GroupTask(groupID, instruction, assignedTo, mode string) error {
	if strings.TrimSpace(instruction) == "" {
		return errors.New("任务指令不能为空")
	}
	a.remoteGroupTask(remote.GroupServerReq{GroupID: groupID, Instruction: instruction, AssignedTo: assignedTo, Mode: mode})
	return nil
}

// GroupJoin 添加员工到群。
func (a *App) GroupJoin(groupID, agentID string) error {
	if groupID == "" || agentID == "" {
		return errors.New("参数无效")
	}
	a.remoteGroupJoin(remote.GroupServerReq{GroupID: groupID, AgentID: agentID})
	a.pushGroups()
	return nil
}

// GroupLeave 移出员工。
func (a *App) GroupLeave(groupID, agentID string) error {
	if groupID == "" || agentID == "" {
		return errors.New("参数无效")
	}
	a.remoteGroupLeave(remote.GroupServerReq{GroupID: groupID, AgentID: agentID})
	a.pushGroups()
	return nil
}

// GroupMessages 群消息历史（打开群聊时拉取快照）。
func (a *App) GroupMessages(groupID string) []remote.GroupMessage {
	st := a.ensureGroupStore()
	return st.msgs(groupID)
}

// ---------- 群聊事件推送 ----------

// pushGroups 推送群列表（kaelvoris:groups）。
func (a *App) pushGroups() {
	a.emitEvent("kaelvoris:groups", a.GroupList())
}

// pushGroupMsg 推送群消息（kaelvoris:group_msg；一次性带该群全量历史便于前端覆盖渲染）。
func (a *App) pushGroupMsg(groupID string) {
	st := a.ensureGroupStore()
	msgs := st.msgs(groupID)
	if len(msgs) > 0 {
		a.emitEvent("kaelvoris:group_msg", map[string]any{"groupId": groupID, "messages": msgs})
	}
}

// pushGroupTaskDone 推送群任务完成（kaelvoris:group_task_done）。
func (a *App) pushGroupTaskDone(task remote.GroupTask) {
	a.emitEvent("kaelvoris:group_task_done", task)
}
