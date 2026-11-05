package employee

// org.go 组织架构与人力资源字典（EHR 人员与组织管控）：
//
// 标准 EHR 的三类主数据 + 员工档案的组织归属字段：
//   - Department 部门：树状层级（ParentID 组织单元树），编码/负责人/排序/状态；
//   - Position   岗位：岗位字典，归属部门 + 职级（如 P5/M2） + 职责描述；
//   - Role       角色：权限角色字典（名称/编码 + 权限包快照：MCP 白黑名单、
//     目录白名单、只读），供员工快速套用（录入时复制到员工配置，运行期独立）。
//
// 设计原则（用户需求）：完全模拟公司人员管理化，无审核流程——全部为
// 「直接录入、直接生效」的 CRUD；引用约束（删部门需先移走子部门/员工等）
// 在 Store 层校验并给出可操作提示。
//
// 持久化：~/.kaelvoris/employees/org.json（与 employees.json 同级；损坏跳过不致命）。

import (
	"fmt"
	"strings"
	"time"
)

// 部门/岗位/角色字典状态（组织主数据生命周期，简单两态；删除走物理删）。
const (
	OrgActive   = "active"   // 启用
	OrgDisabled = "disabled" // 停用（保留档案，不再可被新建员工选择）
)

// Department 部门（组织架构节点，树状层级；ER 部门通常为「组织单元」）。
type Department struct {
	ID        string    `json:"id"`
	Code      string    `json:"code,omitempty"`      // 部门编码（如 TD-01；非空时全局唯一）
	Name      string    `json:"name"`                // 部门名称（全局唯一）
	ParentID  string    `json:"parentId,omitempty"`  // 上级部门 ID（空 = 顶级节点）
	LeaderID  string    `json:"leaderId,omitempty"`  // 负责人（员工 ID）
	SortOrder int       `json:"sortOrder,omitempty"` // 同级排序（小的在前）
	Status    string    `json:"status"`              // active | disabled
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Position 岗位（岗位字典；同一岗位名可存在于不同部门，编码全局唯一）。
type Position struct {
	ID          string `json:"id"`
	Code        string `json:"code,omitempty"`        // 岗位编码（如 ENG-001；非空时全局唯一）
	Name        string `json:"name"`                  // 岗位名称（如 后端工程师）
	DeptID      string `json:"deptId,omitempty"`      // 归属部门 ID（空 = 通用岗位）
	Level       string `json:"level,omitempty"`       // 职级（如 P1~P9 / M1~M5，自由文本）
	Description string `json:"description,omitempty"` // 岗位职责/任职要求
	// 岗位范围（业界 RBAC「岗位数据/业务范围」语义）：左侧导航按部门查看岗位时
	// 除归属部门外，范围含该部门的岗位一并展示。
	ScopeAll     bool     `json:"scopeAll,omitempty"`     // 全公司范围（忽略 ScopeDeptIDs）
	ScopeDeptIDs []string `json:"scopeDeptIds,omitempty"` // 指定部门范围（多选；空且非全公司 = 仅归属部门）
	// RoleIDs 岗位-角色多对多映射（业界 RBAC：岗位=职责编制，角色=权限集，
	// 经岗位间接授权是主路径；用户直绑角色仅兜底临时授权）。员工有效角色 =
	// 直绑角色（首位）∪ 岗位映射角色，运行期按「收紧式合并」叠加（applyEmpRole）。
	RoleIDs   []string  `json:"roleIds,omitempty"`
	Status    string    `json:"status"` // active | disabled
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// InDeptScope 岗位是否落在部门范围（左侧导航点部门查看岗位的判定）：
// 全公司范围恒 true；指定范围含 deptId（或归属部门 = deptId）→ true。
// deptIds 为所选部门及其子树的 ID 集合（子树展开由调用方计算）。
func (p *Position) InDeptScope(deptIds map[string]bool) bool {
	if p == nil {
		return false
	}
	if p.ScopeAll {
		return true
	}
	if deptIds[p.DeptID] {
		return true
	}
	for _, d := range p.ScopeDeptIDs {
		if deptIds[d] {
			return true
		}
	}
	return false
}

// ScopeSummary 岗位范围摘要文案（岗位卡徽标）。
func (p *Position) ScopeSummary() string {
	if p == nil {
		return ""
	}
	if p.ScopeAll {
		return "全公司"
	}
	n := len(p.ScopeDeptIDs)
	if n == 0 {
		return ""
	}
	return "范围 " + itoa(n) + " 部门"
}

// Role 角色（权限角色字典；录入员工时按角色套用权限包快照）。
type Role struct {
	ID   string `json:"id"`
	Code string `json:"code,omitempty"` // 角色编码（如 R-DEV；非空时全局唯一）
	Name string `json:"name"`           // 角色名称（如 开发工程师）
	Desc string `json:"description,omitempty"`
	// 权限包快照（新员工按此初始化 MCP/目录/只读配置；仅录入时套用，运行期以员工自身配置为准）。
	MCPMode  string   `json:"mcpMode,omitempty"`
	MCPTools []string `json:"mcpTools,omitempty"`
	// MCP 总开关 + 服务器白名单（与 Employee 同名四字段语义一致：总开关禁用后模型完全不可见 mcp_* 工具；服务器白名单为空 = 不限制）。
	MCPDisabled bool     `json:"mcpDisabled,omitempty"`
	MCPServers  []string `json:"mcpServers,omitempty"`
	ReadOnly    bool     `json:"readOnly,omitempty"`
	// 技能（SKILLS）权限包：总开关 + 黑名单（与 Employee 同名四字段语义一致）。
	SkillsDisabled bool      `json:"skillsDisabled,omitempty"`
	SkillsDeny     []string  `json:"skillsDeny,omitempty"`
	Status         string    `json:"status"` // active | disabled
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// OrgView 组织架构整体视图（前端组织 Tab 数据源；三字典一次拉取）。
type OrgView struct {
	Departments []Department `json:"departments"`
	Positions   []Position   `json:"positions"`
	Roles       []Role       `json:"roles"`
}

// orgValidateStatus 校验字典状态值。
func orgValidateStatus(status string) error {
	switch status {
	case "", OrgActive, OrgDisabled:
		return nil
	default:
		return fmt.Errorf("未知状态: %s（仅支持 active / disabled）", status)
	}
}

// Validate 部门校验。
func (d *Department) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("部门名称不能为空")
	}
	if len([]rune(strings.TrimSpace(d.Name))) > 50 {
		return fmt.Errorf("部门名称过长（最多 50 字符）")
	}
	return orgValidateStatus(d.Status)
}

// Validate 岗位校验。
func (p *Position) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("岗位名称不能为空")
	}
	if len([]rune(strings.TrimSpace(p.Name))) > 50 {
		return fmt.Errorf("岗位名称过长（最多 50 字符）")
	}
	return orgValidateStatus(p.Status)
}

// Validate 角色校验。
func (r *Role) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("角色名称不能为空")
	}
	if len([]rune(strings.TrimSpace(r.Name))) > 50 {
		return fmt.Errorf("角色名称过长（最多 50 字符）")
	}
	if err := orgValidateStatus(r.Status); err != nil {
		return err
	}
	switch r.MCPMode {
	case MCPUnset, MCPWhitelist, MCPBlacklist:
	default:
		return fmt.Errorf("未知 MCP 权限模式: %s", r.MCPMode)
	}
	return nil
}
