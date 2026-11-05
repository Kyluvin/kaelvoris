// Package employee 数字员工（Digital Employee）：
//
// 对齐《数字员工需求规格说明书》：AI Agent 实例的生命周期管理（§2）、
// 独立工作空间（§3）、MCP 工具权限（§4）、角色提示词（§5）、模型配置（§6）、
// 目录访问控制（§7）、统一权限网关 + 审计（§8）、进化提案（§11）。
//
// 职责边界：本包提供模型/存储/网关/提案的纯 Go 实现；引擎注入与 Wails
// 绑定在应用层（employee_panel.go / app.go buildRunEngine）完成。
//
// 2026-12 EHR 整改：员工档案扩展为标准人事字段（工号/性别/生日/手机/邮箱/
// 入职/在职状态/部门/岗位/角色/直属上级），组织主数据（部门树/岗位/角色
// 字典）见 org.go / orgstore.go。所有人事字段「直接录入、直接生效」，无审核流程。
package employee

import (
	"fmt"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// 员工运行状态（§2.2 状态管理：决定 Agent 是否可被会话/渠道/定时任务引用）。
const (
	StatusEnabled  = "enabled"  // 启用：可被会话/渠道/定时任务引用
	StatusDisabled = "disabled" // 停用：保留配置，不可被引用
	StatusDraft    = "draft"    // 草稿：配置未完成，仅支持测试运行
	StatusArchived = "archived" // 归档：只读保留，不可运行
)

// MCP 权限模式（§4.1）。
const (
	MCPUnset     = ""          // 未配置：全部 MCP 工具可用
	MCPWhitelist = "whitelist" // 白名单：仅 MCPTools 匹配的工具可用
	MCPBlacklist = "blacklist" // 黑名单：MCPTools 匹配的工具被拒绝
)

// 员工人事状态（EHR 在职状态；与运行状态 Status 解耦）。
// 人事状态为离职/停用时，员工不可运行（Runnable 一并拦截，见下）。
const (
	EmpActive    = "active"    // 在职
	EmpProbation = "probation" // 试用期（试岗）
	EmpSuspended = "suspended" // 停用（人事挂起）
	EmpResigned  = "resigned"  // 离职（保留档案，不可运行）
)

// 员工性别（EHR 标准枚举；空 = 未填）。
const (
	GenderMale   = "male"   // 男
	GenderFemale = "female" // 女
	GenderOther  = "other"  // 其他
)

// empIDPattern 工号格式（EHR 档案号；字母数字，最长 20）。
var empIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,20}$`)

// phonePattern 手机号宽松校验（6~20 位数字，允许 + 前缀）。
var phonePattern = regexp.MustCompile(`^\+?[0-9]{6,20}$`)

// emailPattern 邮箱宽松校验。
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// datePattern YYYY-MM-DD（1900-01-01 ~ 2100-12-31 宽松）。
var datePattern = regexp.MustCompile(`^(19|20)\d{2}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])$`)

// Employee 一个数字员工实例（AI Agent 配置 + EHR 人事档案的持久化单元）。
type Employee struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`                  // 必填且唯一（§2.1）
	Avatar      string    `json:"avatar,omitempty"`      // emoji / 图标标识
	Description string    `json:"description,omitempty"` // 一句话职责描述
	Tags        []string  `json:"tags,omitempty"`        // 标签（便于检索/筛选）
	Public      bool      `json:"public"`                // 是否对团队成员可见（单机版仅记录）
	Status      string    `json:"status"`                // enabled | disabled | draft | archived
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`

	// ---- EHR 人事档案（组织管控字段，全部直接录入直接生效）----
	// EmpNo 工号（EHR 档案号；非空时全局唯一，用于展示/检索/报表）。
	EmpNo string `json:"empNo,omitempty"`
	// Gender 性别（male | female | other；空 = 未填）。
	Gender string `json:"gender,omitempty"`
	// Birthday 出生日期（YYYY-MM-DD；空 = 未填）。
	Birthday string `json:"birthday,omitempty"`
	// Phone 手机号（宽松校验；空 = 未填）。
	Phone string `json:"phone,omitempty"`
	// Email 邮箱（宽松校验；空 = 未填）。
	Email string `json:"email,omitempty"`
	// HireDate 入职日期（YYYY-MM-DD；空 = 未填）。
	HireDate string `json:"hireDate,omitempty"`
	// EmpStatus 人事状态（active | probation | suspended | resigned；默认 active）。
	EmpStatus string `json:"empStatus,omitempty"`
	// DeptID 所属部门（org.json Department.ID；空 = 未分配）。
	DeptID string `json:"deptId,omitempty"`
	// PositionID 岗位（org.json Position.ID；空 = 未分配）。
	PositionID string `json:"positionId,omitempty"`
	// RoleID 权限角色（org.json Role.ID；兼容存量单角色；读取时已合并入 RoleIDs）。
	RoleID string `json:"roleId,omitempty"`
	// RoleIDs 权限角色列表（业界 RBAC：用户-角色多对多；员工同时持多角色时
	// 运行期按「收紧式合并」叠加——任一角色禁用 MCP/技能/只读即生效，
	// 白名单取并集面收紧，详见 applyEmpRole 合并语义）。RoleID 为首位兼容位：
	// 读取归一化时 RoleID 非空且不在 RoleIDs 前端则前插，写入时同步回填。
	RoleIDs []string `json:"roleIds,omitempty"`
	// ManagerID 直属上级（员工 ID；空 = 无；不可引用自己/成环）。
	ManagerID string `json:"managerId,omitempty"`

	// ---- 派生展示字段（非持久化；面板/装配层按组织字典回填，供提示词段与面板展示）----
	// DeptName 部门名称展示（DeptID → org.json Department.Name）。
	DeptName string `json:"-"`
	// PositionName 岗位名称展示（PositionID → org.json Position.Name）。
	PositionName string `json:"-"`
	// ManagerName 直属上级姓名展示（ManagerID → 员工 Name）。
	ManagerName string `json:"-"`

	// ---- §3 工作空间 ----
	// WorkspaceRoot 自定义工作空间根（空 = 默认 ~/.kaelvoris/employees/<ID>/workspace）。
	WorkspaceRoot string `json:"workspaceRoot,omitempty"`

	// ---- §4 MCP 工具权限 ----
	// MCPMode："" 未配置（全部可用）| whitelist | blacklist。
	// MCPTools 工具名规则（支持通配符 file_*；匹配对象为 mcp_<server>_<tool> 全名）。
	MCPMode  string   `json:"mcpMode,omitempty"`
	MCPTools []string `json:"mcpTools,omitempty"`
	// MCPDisabled MCP 总开关（用户需求 2026-xx：员工可整体禁用 MCP）：true 时
	// 定义侧不连接任何 MCP 服务器、不注册 mcp_* 工具、提示词不出现 MCP 能力
	// 描述；执行侧网关（Gateway）兜底拒绝。与 MCPMode/MCPTools 正交叠加。
	MCPDisabled bool `json:"mcpDisabled,omitempty"`
	// MCPServers MCP 服务器白名单（服务器名 = sanitizeToolName 的 server 段；
	// 空 = 不限制；非空 = 仅名单内服务器可连接，白名单进一步细到工具级由
	// MCPMode+MCPTools 承担）。仅 whitelist 场景语义明确，黑名单模式下忽略。
	MCPServers []string `json:"mcpServers,omitempty"`

	// ---- 技能（SKILLS）权限 ----
	// SkillsDisabled 技能总开关：true 时提示词不注入 <available_skills>、
	// skill 工具不注册、执行侧网关兜底拒绝（三路封死）。
	SkillsDisabled bool `json:"skillsDisabled,omitempty"`
	// SkillsDeny 技能黑名单（名称支持通配符 web_*；命中即拒绝加载）。仅拦截
	// skill 工具调用，提示词由 SkillsSection 边界声明兜底。
	SkillsDeny []string `json:"skillsDeny,omitempty"`

	// ---- §5 角色提示词 ----
	// RolePrompt 多段角色设定模板，支持变量：{{员工名称}} {{当前用户}} {{工作空间路径}} {{日期}}。
	RolePrompt string `json:"rolePrompt,omitempty"`

	// ---- §6 模型配置 ----
	Model       string  `json:"model,omitempty"`       // 空 = 使用全局模型
	Temperature float64 `json:"temperature,omitempty"` // 0~2（0 = 不下发，用服务端默认）
	TopP        float64 `json:"topP,omitempty"`
	MaxTokens   int     `json:"maxTokens,omitempty"`

	// ---- §7 目录访问控制 ----
	// Dirs 允许访问的额外目录（绝对路径白名单；支持前缀通配 D:\data\*）。
	// 工作空间默认始终可访问。
	Dirs     []string `json:"dirs,omitempty"`
	ReadOnly bool     `json:"readOnly"` // 只读模式：全部写操作禁用（§7.3）

	// ---- §11 自学习（进化强度）----
	// EvolutionStrength：100 激进（主动提案）/ 50 稳健（高置信才提案）/ 0 关闭。
	EvolutionStrength int `json:"evolutionStrength,omitempty"`
}

// Runnable 员工是否可执行任务（§2.2 运行状态 + EHR 人事状态双重门槛）：
// 运行状态：启用可运行；草稿仅测试运行；停用/归档一律拒绝。
// 人事状态：在职/试用期可运行；停用/离职一律拒绝（EHR 人事管控）。
func (e *Employee) Runnable(testRun bool) bool {
	if e == nil {
		return false
	}
	switch e.Status {
	case StatusEnabled:
	case StatusDraft:
		if !testRun {
			return false
		}
	default:
		return false
	}
	// EHR 人事状态门槛（缺省按「在职」处理，兼容旧数据）。
	switch e.EmpStatus {
	case EmpResigned, EmpSuspended:
		return false
	default:
		return true
	}
}

// Workspace 返回员工工作空间绝对路径（§3）：
// WorkspaceRoot 非空用之；否则默认 <root>/数字员工/<ID>/workspace。
func (e *Employee) Workspace(root string) string {
	if e == nil {
		return ""
	}
	if e.WorkspaceRoot != "" {
		return e.WorkspaceRoot
	}
	return filepath.Join(root, "数字员工", e.ID, "workspace")
}

// ---- MCP / SKILLS 权限判定（装配层与 Gateway 共用；空配置 = 全部放行）----

// MCPEnabled MCP 总开关：MCPDisabled=true 时模型完全不可见 mcp_* 工具
// （定义侧不连接/不注册 + 执行侧网关拒绝，双保险）。
func (e *Employee) MCPEnabled() bool {
	return e == nil || !e.MCPDisabled
}

// ServerAllowed MCP 服务器白名单判定（MCPServers 空 = 不限制）：
// server 名对应 mcp_<server>_<tool> 全名的 server 段（见 mcp 包 sanitizeToolName）。
func (e *Employee) ServerAllowed(server string) bool {
	if e == nil || len(e.MCPServers) == 0 {
		return true
	}
	for _, s := range e.MCPServers {
		if strings.TrimSpace(s) == server {
			return true
		}
	}
	return false
}

// ApplyRoleRestrictions 运行期套用角色权限包（收紧式合并，只减不加）：
// Role 字典的权限包在装配时叠加到员工有效配置，角色侧限制不可被员工自身
// 配置绕过（修复 2026-xx：角色设了 MCP 禁用但对话仍可调用——此前角色权限
// 包从未被套用，roleId 仅存档）。合并规则：
//   - 总开关/只读（布尔禁用项）：员工或角色任一为 true 即 true（不可放宽）；
//   - MCP 服务器白名单：两者非空取交集，一方空继承另一方（空 = 不限制）；
//   - MCP 模式：角色未配置继承员工；角色白名单 ∩ 员工白名单取交集收紧；
//     任一方黑名单 → 合并为黑名单并取规则并集（并集只会拒绝更多，不放宽）；
//   - 技能黑名单：并集（命中面只增不减）；目录白名单/提示词/模型等不走角色。
func (e *Employee) ApplyRoleRestrictions(r Role) {
	if r.MCPDisabled {
		e.MCPDisabled = true
		e.MCPMode = MCPUnset
		e.MCPTools = nil
		e.MCPServers = nil
	}
	if r.SkillsDisabled {
		e.SkillsDisabled = true
		e.SkillsDeny = nil
	}
	if r.ReadOnly {
		e.ReadOnly = true
	}
	// 服务器白名单交集（一方空 = 不限制，取另一方）。
	e.MCPServers = intersectNonEmpty(e.MCPServers, r.MCPServers)
	// 工具清单模式合并（角色未配置 = 不叠加）。
	switch r.MCPMode {
	case MCPWhitelist:
		switch e.MCPMode {
		case MCPWhitelist:
			// 双白名单取交集，交集为空时保留空集（= 全部拒绝，收紧语义）。
			e.MCPTools = intersectGlob(e.MCPTools, r.MCPTools)
		case MCPBlacklist:
			// 白∩黑 = 黑名单规则外的白名单项；空 = 全拒。
			var out []string
			for _, t := range e.MCPTools {
				if !matchAnyGlob(r.MCPTools, t) {
					out = append(out, t)
				}
			}
			e.MCPTools = out
		default:
			e.MCPMode = MCPWhitelist
			e.MCPTools = r.MCPTools
		}
	case MCPBlacklist:
		if e.MCPMode == MCPWhitelist {
			// 员工白名单剔除角色黑名单命中的项（命中即拒绝，只收不宽）。
			var out []string
			for _, t := range e.MCPTools {
				if !matchAnyGlob(r.MCPTools, t) {
					out = append(out, t)
				}
			}
			e.MCPTools = out
		} else {
			// 未配置或黑名单 → 黑名单并集。
			e.MCPMode = MCPBlacklist
			e.MCPTools = append(append([]string{}, e.MCPTools...), r.MCPTools...)
		}
	}
	if e.SkillsDeny == nil && len(r.SkillsDeny) > 0 && !e.SkillsDisabled {
		e.SkillsDeny = append([]string{}, r.SkillsDeny...)
	} else if len(r.SkillsDeny) > 0 && !e.SkillsDisabled {
		e.SkillsDeny = append(e.SkillsDeny, r.SkillsDeny...)
	}
}

// NormalizeRoleIDs 角色列表归一化（读/写共用，导出供面板装配层调用）：
// RoleID 非空且不在 RoleIDs 前端则前插（兼容存量单角色）；去重去空；
// RoleID 始终与首位同步（旧读取方只看 RoleID 不丢角色）。RoleIDs 空 = 无角色。
func (e *Employee) NormalizeRoleIDs() {
	e.normalizeRoleIDs()
}

// normalizeRoleIDs 角色列表归一化实现（包内读/写路径共用）。
func (e *Employee) normalizeRoleIDs() {
	if e == nil {
		return
	}
	primary := strings.TrimSpace(e.RoleID)
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	if primary != "" {
		add(primary)
	}
	for _, id := range e.RoleIDs {
		add(id)
	}
	e.RoleIDs = out
	e.RoleID = ""
	if len(out) > 0 {
		e.RoleID = out[0]
	}
}

// matchAnyGlob 任一规则命中即 true（空规则集 = 永不命中）。
func matchAnyGlob(rules []string, name string) bool {
	for _, r := range rules {
		if MatchGlob(r, name) {
			return true
		}
	}
	return false
}

// intersectGlob 白∩白：员工白名单中保留同时被角色规则匹配的项；
// 员工未配置白名单（空 = 全部可用）时结果 = 角色白名单。
func intersectGlob(empTools, roleRules []string) []string {
	if len(empTools) == 0 {
		return append([]string{}, roleRules...)
	}
	var out []string
	for _, t := range empTools {
		if matchAnyGlob(roleRules, t) {
			out = append(out, t)
		}
	}
	return out
}

// intersectNonEmpty 服务器白名单交集（空切片 = 不限制；一方空继承另一方；
// 双方非空取交集，交集为空 = 全部拒绝，收紧语义）。
func intersectNonEmpty(a, b []string) []string {
	if len(a) == 0 {
		return append([]string{}, b...)
	}
	if len(b) == 0 {
		return append([]string{}, a...)
	}
	var out []string
	for _, x := range a {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		for _, y := range b {
			if strings.TrimSpace(y) == x {
				out = append(out, x)
				break
			}
		}
	}
	return out
}

// SkillAllowed 技能加载判定：总开关禁用一律拒绝；否则按黑名单
// （SkillsDeny 空 = 不限制）MatchGlob 匹配技能名。
func (e *Employee) SkillAllowed(name string) bool {
	if e == nil || e.SkillsDisabled {
		return false
	}
	for _, r := range e.SkillsDeny {
		if MatchGlob(r, name) {
			return false
		}
	}
	return true
}

// activeServers 去空白后的服务器白名单（提示词展示用）。
func (e *Employee) activeServers() []string {
	var out []string
	for _, s := range e.MCPServers {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// SecuritySummary 安全摘要（§14：「白名单 5 项 · 目录 2 个 · 只读开」）。
func (e *Employee) SecuritySummary() string {
	toolN := 0
	mode := "未限制"
	switch e.MCPMode {
	case MCPWhitelist:
		mode = "白名单"
		toolN = len(e.MCPTools)
	case MCPBlacklist:
		mode = "黑名单"
		toolN = len(e.MCPTools)
	}
	ro := "只读关"
	if e.ReadOnly {
		ro = "只读开"
	}
	if !e.MCPEnabled() {
		mode = "禁用"
		toolN = 0
	} else if n := len(e.activeServers()); n > 0 {
		mode = fmt.Sprintf("%s·%d服务器", mode, n)
	}
	skillState := ""
	if e.SkillsDisabled {
		skillState = " · 技能禁用"
	} else if n := len(e.SkillsDeny); n > 0 {
		skillState = fmt.Sprintf(" · 技能黑名单%d项", n)
	}
	return fmt.Sprintf("MCP%s %d 项 · 目录 %d 个 · %s%s", mode, toolN, len(e.Dirs), ro, skillState)
}

// Validate 校验员工配置（新建/更新共用：Agent 配置 + EHR 人力字段）。
func (e *Employee) Validate() error {
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("员工名称不能为空")
	}
	if len([]rune(strings.TrimSpace(e.Name))) > 50 {
		return fmt.Errorf("员工名称过长（最多 50 字符）")
	}
	switch e.Status {
	case "", StatusEnabled, StatusDisabled, StatusDraft, StatusArchived:
	default:
		return fmt.Errorf("未知状态: %s", e.Status)
	}
	switch e.MCPMode {
	case MCPUnset, MCPWhitelist, MCPBlacklist:
	default:
		return fmt.Errorf("未知 MCP 权限模式: %s", e.MCPMode)
	}
	if e.Temperature < 0 || e.Temperature > 2 {
		return fmt.Errorf("temperature 取值 0~2")
	}
	if e.TopP < 0 || e.TopP > 1 {
		return fmt.Errorf("top_p 取值 0~1")
	}
	if e.MaxTokens < 0 {
		return fmt.Errorf("max_tokens 不能为负")
	}
	if e.EvolutionStrength != 0 && e.EvolutionStrength != 50 && e.EvolutionStrength != 100 {
		return fmt.Errorf("进化强度仅支持 100 / 50 / 0")
	}
	for _, d := range e.Dirs {
		if strings.TrimSpace(d) == "" {
			continue
		}
		if !filepath.IsAbs(filepath.Clean(strings.TrimSpace(d))) {
			return fmt.Errorf("目录白名单必须是绝对路径: %s", d)
		}
	}
	if root := strings.TrimSpace(e.WorkspaceRoot); root != "" && !filepath.IsAbs(filepath.Clean(root)) {
		return fmt.Errorf("自定义工作空间必须是绝对路径: %s", e.WorkspaceRoot)
	}
	return e.validateHR()
}

// validateHR EHR 人事字段校验（宽松：仅填了才校验格式；日期/性别/状态枚举）。
func (e *Employee) validateHR() error {
	if no := strings.TrimSpace(e.EmpNo); no != "" {
		if !empIDPattern.MatchString(no) {
			return fmt.Errorf("工号仅支持字母/数字/-/_，最长 20: %s", no)
		}
	}
	switch e.Gender {
	case "", GenderMale, GenderFemale, GenderOther:
	default:
		return fmt.Errorf("未知性别: %s（仅支持 male / female / other）", e.Gender)
	}
	for name, v := range map[string]string{
		"出生日期": e.Birthday,
		"入职日期": e.HireDate,
	} {
		if v != "" && !datePattern.MatchString(v) {
			return fmt.Errorf("%s格式应为 YYYY-MM-DD: %s", name, v)
		}
	}
	if p := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(e.Phone), "-", ""), " ", ""); p != "" {
		if !phonePattern.MatchString(p) {
			return fmt.Errorf("手机号格式不正确: %s", e.Phone)
		}
	}
	if em := strings.TrimSpace(e.Email); em != "" && !emailPattern.MatchString(em) {
		return fmt.Errorf("邮箱格式不正确: %s", em)
	}
	switch e.EmpStatus {
	case "", EmpActive, EmpProbation, EmpSuspended, EmpResigned:
	default:
		return fmt.Errorf("未知人事状态: %s（仅支持 active / probation / suspended / resigned）", e.EmpStatus)
	}
	return nil
}

// RenderRolePrompt 渲染角色提示词模板变量（§5.2）：
// {{员工名称}} {{当前用户}} {{工作空间路径}} {{日期}}。
// 返回注入系统提示词的完整文本（附加安全规则由 Gateway.RulesSection 生成）。
func (e *Employee) RenderRolePrompt(ws string) string {
	userName := ""
	if u, err := user.Current(); err == nil {
		userName = u.Username
		if i := strings.LastIndexAny(userName, `\/`); i >= 0 {
			userName = userName[i+1:]
		}
	}
	rp := e.RolePrompt
	if strings.TrimSpace(rp) == "" {
		rp = "你是「" + e.Name + "」。" + e.Description
	}
	repl := strings.NewReplacer(
		"{{员工名称}}", e.Name,
		"{{当前用户}}", userName,
		"{{工作空间路径}}", ws,
		"{{日期}}", time.Now().Format("2006-01-02"),
	)
	return repl.Replace(rp)
}

// RoleSection 组装注入系统提示词的完整角色段（角色设定 + 组织身份 + 目录边界 + 只读声明）。
// sessionWS 为会话工作区（工具实际 cwd；非空时声明双区边界：任务主战场在会话
// 工作区，员工工作空间仅记忆/进化沉淀），空 = 独立员工运行态（旧语义）。
func (e *Employee) RoleSection(ws string, sessionWS ...string) string {
	var b strings.Builder
	b.WriteString("# 数字员工角色设定\n\n")
	b.WriteString("本会话由数字员工「" + e.Name + "」接管。请始终以该角色行事：\n\n")
	if rp := strings.TrimSpace(e.RenderRolePrompt(ws)); rp != "" {
		b.WriteString(rp + "\n\n")
	}
	// EHR 组织身份（组织架构/岗位/角色上下文）。
	if e.EmpNo != "" || e.DeptID != "" || e.PositionID != "" {
		b.WriteString("## 组织身份\n\n")
		b.WriteString("- 工号：" + e.EmpNo + "\n")
		if e.DeptName != "" {
			b.WriteString("- 部门：" + e.DeptName + "\n")
		}
		if e.PositionName != "" {
			b.WriteString("- 岗位：" + e.PositionName + "\n")
		}
		if e.ManagerName != "" {
			b.WriteString("- 直属上级：" + e.ManagerName + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("## 工作目录边界\n\n")
	// 双区语义：会话工作区非空且与员工工作空间不同目录时声明两区边界
	//（任务主战场在会话工作区，员工工作空间保留为记忆/进化目录）；
	// 同目录或无会话工作区时退回单区旧文案。
	sessWS := strings.TrimSpace(sessionWS0(sessionWS))
	if sessWS != "" {
		if sessAbs, err := filepath.Abs(sessWS); err == nil {
			if empAbs, err := filepath.Abs(ws); err == nil && strings.EqualFold(filepath.Clean(sessAbs), filepath.Clean(empAbs)) {
				sessWS = "" // 同目录收敛为单区
			}
		}
	}
	if sessWS != "" {
		// 双区：任务在会话工作区执行，员工工作空间为专属记忆/进化目录
		//（两者均在网关白名单内，其余路径一律拒绝）。
		b.WriteString("- 当前任务工作区（工具默认目录）：" + sessWS + "\n")
		b.WriteString("- 你的专属工作空间（记忆与进化提案所在）：" + ws + "\n")
	} else {
		b.WriteString("- 你的专属工作空间：" + ws + "（文件操作默认在此目录内）\n")
	}
	if len(e.activeDirs()) > 0 {
		b.WriteString("- 额外允许访问的目录（白名单之外一律拒绝）：" + strings.Join(e.activeDirs(), " ; ") + "\n")
	}
	b.WriteString("- 不要尝试访问上述之外的任何路径；越权访问会被安全网关拦截。\n\n")
	if e.ReadOnly {
		b.WriteString("## 只读模式\n\n")
		b.WriteString("该数字员工处于只读模式：禁止一切写操作（文件写入/编辑/命令行写文件）。")
		b.WriteString("如任务确需写入，请向用户说明并建议切换员工或调整配置。\n\n")
	}
	if sec := e.permSections(); sec != "" {
		b.WriteString(sec)
	}
	if e.EvolutionStrength > 0 {
		b.WriteString(e.evolutionSection(ws, sessionWS))
	}
	return b.String()
}

// permSections MCP/技能权限边界段（提示词侧声明 + 网关执行侧拦截双保险）。
func (e *Employee) permSections() string {
	var b strings.Builder
	// MCP 边界：总开关禁用 → 全部不可用；服务器白名单 → 仅名单内可用。
	if !e.MCPEnabled() {
		b.WriteString("## MCP 工具权限\n\n")
		b.WriteString("本数字员工已整体禁用 MCP 扩展工具：不要尝试调用任何 mcp_ 前缀的工具")
		b.WriteString("（调用会被安全网关拒绝）。请用内置工具完成任务。\n\n")
	} else if len(e.MCPServers) > 0 {
		b.WriteString("## MCP 工具权限\n\n")
		b.WriteString("本数字员工仅允许使用以下 MCP 服务器的工具：" + strings.Join(e.activeServers(), "、") + "。")
		b.WriteString("其他服务器的 mcp_* 工具调用会被安全网关拒绝。\n\n")
	}
	// 技能边界：总开关禁用 → 不可加载任何技能；黑名单 → 列出禁用项。
	if e.SkillsDisabled {
		b.WriteString("## 技能（SKILLS）权限\n\n")
		b.WriteString("本数字员工已禁用全部技能（SKILLS）：不要尝试调用 skill 工具（会被安全网关拒绝）。\n\n")
	} else if n := len(e.SkillsDeny); n > 0 {
		b.WriteString("## 技能（SKILLS）边界\n\n")
		b.WriteString("以下技能已被禁用（黑名单，匹配 " + itoa(n) + " 条规则）：不可加载，如需相关能力请向用户说明。\n")
	}
	return b.String()
}

// activeDirs 过滤空白后的目录白名单。
func (e *Employee) activeDirs() []string {
	out := make([]string, 0, len(e.Dirs))
	for _, d := range e.Dirs {
		if s := strings.TrimSpace(d); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (e *Employee) evolutionSection(ws string, sessionWS []string) string {
	attitude := "仅当发现明确、可复用的改进点时提案（稳健模式，置信度高才写）"
	if e.EvolutionStrength >= 100 {
		attitude = "积极观察可改进点并提案（激进模式）"
	}
	var b strings.Builder
	b.WriteString("## 自学习（进化提案）\n\n")
	b.WriteString("本员工启用了自学习（强度 " + itoa(e.EvolutionStrength) + "）。" + attitude + "\n\n")
	b.WriteString("学习信号（任务过程中出现以下情况时记录）：\n")
	b.WriteString("1. 纠正（correction）：用户纠正了你的输出；\n")
	b.WriteString("2. 偏好（preference）：用户表达了明确偏好（格式/风格/工具选择）；\n")
	b.WriteString("3. 工作流（workflow）：确认了可复用的多步流程；\n")
	b.WriteString("4. 困境（struggle）：同一问题反复尝试失败后找到解法。\n\n")
	if e.EvolutionStrength >= 100 {
		b.WriteString("一般性观察也可提案；但无实质证据不要提案（低噪音原则）。\n\n")
	}
	b.WriteString("提案流程（唯一允许的沉淀路径——不要直接改写记忆文件）：\n")
	b.WriteString("1. 用 fs_write 将提案写入：`" + e.draftDirAbs(ws, sessionWS) + "`（签名 = 8 位小写十六进制，由内容哈希截取；同签名文件已存在则跳过）；\n")
	b.WriteString("2. 文件格式：以 `---` 包裹的 YAML 头 + 正文，头字段：\n")
	b.WriteString("   Proposal-ID: evo-<YYYYMMDD>-<签名> / Signature: <签名> / Target-File: BEHAVIOR.md|TOOLS.md|MEMORY.md|WORKFLOWS.md / Trigger-Type: correction|preference|workflow|struggle / Confidence: 0-100\n")
	b.WriteString("3. 正文分节：## Why This Matters / ## Evidence / ## Duplicate Check / ## Proposed Change / ## Apply Plan。\n")
	b.WriteString("4. 提案等待用户审批后由系统落地，你只需告知用户\"已提交进化提案，等待审批\"。\n\n")
	b.WriteString("路由参考：行为偏好→BEHAVIOR.md；工具用法→TOOLS.md；事实记忆→MEMORY.md；流程→WORKFLOWS.md（均位于 workspace/memory/ 下，可先 fs_read 查重）。\n")
	return b.String()
}

// sessionWS0 取会话工作区首个非空值（RoleSection 可变参便捷访问）。
func sessionWS0(sessionWS []string) string {
	if len(sessionWS) == 0 {
		return ""
	}
	return sessionWS[0]
}

// draftDirAbs 进化提案 pending 目录的写入指引：双区语义（sessionWS 非空）下
// 工具相对路径按会话工作区解析，提案必须用员工工作空间内的绝对路径
// （网关 evolutionPathOK 校验员工工作空间前缀，防提案写进会话项目目录丢失）。
func (e *Employee) draftDirAbs(ws string, sessionWS []string) string {
	if sess := strings.TrimSpace(sessionWS0(sessionWS)); sess != "" {
		if sessAbs, err := filepath.Abs(sess); err == nil {
			if empAbs, err := filepath.Abs(ws); err == nil && !strings.EqualFold(filepath.Clean(sessAbs), filepath.Clean(empAbs)) {
				// 双区：提案固定写员工工作空间（审批/落地均从该目录读取）。
				return filepath.ToSlash(joinPath(ws, "evolution-drafts", "pending", "<签名>.md"))
			}
		}
	}
	return "evolution-drafts/pending/<签名>.md"
}

// itoa 小整数转字符串（避免仅为它引入 strconv）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// MatchGlob 通配符匹配（§4.2）：仅支持尾部 `*`（前缀通配，如 file_*）
// 与单独 `*`（全匹配）。用于 MCP 工具白/黑名单。
func MatchGlob(pattern, name string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if i := strings.Index(pattern, "*"); i >= 0 {
		prefix, suffix := pattern[:i], pattern[i+1:]
		if strings.Contains(suffix, "*") {
			// 多个 *：退化为首尾匹配（中间任意）
			return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix)
		}
		return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) &&
			len(name) >= len(prefix)+len(suffix)
	}
	return pattern == name
}
