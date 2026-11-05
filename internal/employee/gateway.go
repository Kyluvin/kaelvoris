package employee

// gateway.go 统一权限网关（§4 MCP 工具权限 + §7 目录访问控制 + §8 审计）。
//
// 挂接点：agent.Engine.HookPreToolUse —— 工具执行前拦截，返回
// (allow, newArgs, blockMsg)。拒绝时 blockMsg 回填模型（结构化拒绝原因，
// 对齐 §4.3 工具边界守卫与 §7.4 越权拦截）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// writeTools 需要写权限的工具（只读模式一律拒绝，§7.3）。
var writeTools = map[string]bool{
	"fs_write":      true,
	"fs_apply_diff": true,
}

// pathArgTools 从 JSON 参数中提取 path 字段的工具（目录白名单检查对象）。
var pathArgTools = map[string]bool{
	"fs_read":       true,
	"fs_write":      true,
	"fs_apply_diff": true,
	"fs_list":       true,
	"fs_search":     true,
}

// Gateway 数字员工权限网关：MCP 工具过滤 + 目录白名单 + 只读守卫 + 审计。
// 一个任务运行期独占一个实例（员工配置在运行期不可变，无并发竞争）。
type Gateway struct {
	emp       *Employee
	workspace string // 员工工作空间绝对路径
	cwd       string // 会话工作区（工具相对路径解析基准；空 = 独立员工运行态）

	auditMu sync.Mutex
	audit   *os.File // audit.jsonl 追加句柄（nil = 审计仅走 slog）
}

// NewGateway 创建网关并打开审计文件（审计打开失败不阻断任务，仅降级日志）。
// cwd 为会话工作区（工具相对路径的实际解析基准）：会话绑定员工时任务仍
// 运行在会话自己的工作区（用户项目目录），员工工作空间仅保留为专属
// 记忆/进化目录；两者均允许访问（cwd 恒放行 + 工作空间恒放行）。
// cwd 空 = 旧语义（独立员工运行态，相对路径按员工工作空间解析）。
func NewGateway(emp *Employee, root, cwd string) *Gateway {
	g := &Gateway{emp: emp, workspace: normalizeRealPath(emp.Workspace(root))}
	if c := strings.TrimSpace(cwd); c != "" {
		// 空 cwd 保持零值（"." 会被 hasDirPrefix 误判为放行当前目录）。
		g.cwd = normalizeRealPath(c)
	}
	auditDir := joinPath(root, "数字员工", emp.ID)
	if err := os.MkdirAll(auditDir, 0o755); err == nil {
		f, err := os.OpenFile(joinPath(auditDir, "audit.jsonl"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			g.audit = f
		}
	}
	return g
}

// Close 关闭审计文件（任务结束时调用）。
func (g *Gateway) Close() {
	if g == nil {
		return
	}
	g.auditMu.Lock()
	if g.audit != nil {
		_ = g.audit.Close()
		g.audit = nil
	}
	g.auditMu.Unlock()
}

// auditLog 追加一条审计记录（§8.2：何时/哪个工具/允许或拒绝/原因）。
func (g *Gateway) auditLog(tool, args, decision, reason string) {
	rec := map[string]any{
		"ts":       time.Now().Format(time.RFC3339),
		"employee": g.emp.Name,
		"tool":     tool,
		"args":     truncateRunes(args, 500),
		"decision": decision,
	}
	if reason != "" {
		rec["reason"] = reason
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	g.auditMu.Lock()
	defer g.auditMu.Unlock()
	if g.audit == nil {
		slog.Info("employee.audit", "record", string(b))
		return
	}
	_, _ = g.audit.Write(append(b, '\n'))
}

// Hook 工具执行前网关检查（对齐 HookPreToolUse 签名）。
func (g *Gateway) Hook(tool, args string) (bool, string, string) {
	reason := g.check(tool, args)
	if reason == "" {
		g.auditLog(tool, args, "allow", "")
		return true, "", ""
	}
	g.auditLog(tool, args, "deny", reason)
	return false, "", reason
}

// check 三类规则检查（只读 → 目录 → MCP → 提案路径锁定），返回拒绝原因（空 = 放行）。
func (g *Gateway) check(tool, args string) string {
	// §7.3 只读模式：写类工具一律拒绝。
	if g.emp.ReadOnly && isWriteTool(tool) {
		return "该数字员工处于只读模式，不支持写操作：" + tool +
			"。如任务确需写入，请向用户说明并建议调整员工配置。"
	}
	// §7 目录访问控制：路径参数校验白名单。
	if p, ok := pathArgOfTool(tool, args); ok && !g.dirAllowed(p) {
		return "越权拦截：路径 " + p + " 不在该数字员工允许访问的目录内" +
			"（工作空间：" + g.workspace + "）。如需访问其他目录，请联系用户调整目录白名单。"
	}
	// 技能（SKILLS）权限（§4 同层）：总开关优先 + 黑名单，双保险执行侧拦截。
	// （定义侧 skill 工具不注册/提示词不注入；此处网关兜底，防旧版会话残留调用。）
	if tool == "skill" {
		if g.emp.SkillsDisabled {
			return g.skillDenyReason(skillNameOf(args))
		}
		if name := skillNameOf(args); name != "" && !g.emp.SkillAllowed(name) {
			return g.skillDenyReason(name)
		}
	}
	// §4 MCP 工具权限（仅约束 mcp_* 前缀的远程工具）。
	// mcp_activate 是宿主内置的惰性加载管理工具（非远程工具），不受 MCP
	// 白名单约束（否则 server 段 "activate" 永不在白名单 → 惰性加载对员工全废）。
	if tool == "mcp_activate" {
		return ""
	}
	if strings.HasPrefix(tool, "mcp_") && !g.mcpAllowed(tool) {
		return g.mcpDenyReason(tool)
	}
	// §11.5 提案路径锁定：进化草稿只允许写 pending/<8位签名>.md；
	// 正式记忆（memory/*.md）只能由用户审批后由系统落地，模型直写一律拒绝。
	if tool == "fs_write" {
		if p, ok := pathArgOfTool(tool, args); ok {
			if strings.Contains(p, "evolution-drafts") && !g.evolutionPathOK(p) {
				return "进化提案只能写入 evolution-drafts/pending/<8位签名>.md（草稿等待审批），" +
					"不允许直接写入 " + p + "；正式记忆由用户审批后由系统落地。"
			}
			if g.memoryPathOK(p) {
				return "越权拦截：不允许直接写入记忆文件 " + p +
					"（防止伪造已审批记忆）。若本次任务有值得沉淀的经验，请按进化提案流程" +
					"写入 evolution-drafts/pending/<8位签名>.md，由用户审批后自动落地。"
			}
		}
	}
	return ""
}

// memoryPathOK 判断路径是否为员工工作空间内受保护的记忆文件
// （memory/ 目录下的 *.md，含初始化种子 BEHAVIOR/TOOLS/MEMORY/WORKFLOWS.md）。
func (g *Gateway) memoryPathOK(rawPath string) bool {
	p := strings.TrimSpace(rawPath)
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(g.relBase(), p)
	}
	abs = filepath.Clean(abs)
	if !hasDirPrefix(abs, joinPath(g.workspace, "memory")) {
		return false
	}
	return strings.EqualFold(filepath.Ext(abs), ".md")
}

// dirAllowed 目录白名单判断（§7.1/§7.2）：
// cwd（会话工作区）与员工工作空间始终允许；Dirs 白名单支持前缀通配
// （dir\*）；拒绝 .. 穿越；符号链接解析后真实路径仍须落白名单内。
// 相对路径按 relBase()（cwd 优先）解析，与工具进程实际 cwd 一致。
func (g *Gateway) dirAllowed(rawPath string) bool {
	p := strings.TrimSpace(rawPath)
	if p == "" {
		return true
	}
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(g.relBase(), p)
	}
	abs = filepath.Clean(abs)
	// 符号链接逃逸检测（文件可能尚不存在：EvalLinks 失败时解析父目录保留叶子名）。
	if real, err := evalLinks(abs); err == nil {
		abs = real
	} else {
		parent, leaf := filepath.Split(abs)
		if realParent, err2 := evalLinks(filepath.Clean(parent)); err2 == nil {
			abs = filepath.Join(realParent, leaf)
		}
	}
	// cwd（会话工作区）与员工工作空间内一律放行。
	if hasDirPrefix(abs, g.workspace) || sameDir(abs, g.workspace) ||
		hasDirPrefix(abs, g.cwd) || sameDir(abs, g.cwd) {
		return true
	}
	// 白名单逐项匹配（支持前缀通配 D:\data\*）。
	for _, d := range g.emp.Dirs {
		d = normalizeDir(d)
		if d == "" {
			continue
		}
		if strings.HasSuffix(d, "*") {
			base := strings.TrimSuffix(d, "*")
			base = strings.TrimSuffix(base, string(filepath.Separator))
			if base != "" && hasDirPrefix(abs, base) {
				return true
			}
			continue
		}
		if hasDirPrefix(abs, d) || sameDir(abs, d) {
			return true
		}
	}
	return false
}

// evalLinks filepath.EvalLinks 的别名（Windows 上 os.PathSeparator 语义已由调用方处理）。
func evalLinks(p string) (string, error) { return filepath.EvalSymlinks(p) }

// normalizeRealPath 规范化路径到真实长名（Windows 8.3 短路径修复）：
// TempDir 等场景返回 ADMINI~1 短名，而 EvalSymlinks 解析被检路径时得到
// 长名，两侧基准不一致会导致白名单前缀比对失配（越权误拦）。策略：
// 路径存在 → EvalSymlinks 取真实长名；不存在 → 逐级解析最近存在的祖先，
// 拼回剩余叶子。失败时回退 Clean 原路径。
func normalizeRealPath(p string) string {
	p = filepath.Clean(strings.TrimSpace(p))
	if p == "" {
		return p
	}
	if real, err := evalLinks(p); err == nil {
		return real
	}
	// 逐级解析：找到最深的存在祖先，解析后拼回剩余部分。
	rest := ""
	cur := p
	for {
		parent, leaf := filepath.Split(cur)
		parent = filepath.Clean(parent)
		if parent == cur || len(parent) <= 3 { // 到达盘根
			break
		}
		if real, err := evalLinks(parent); err == nil {
			return filepath.Join(real, leaf, rest)
		}
		rest = filepath.Join(leaf, rest)
		cur = parent
	}
	return p
}

// mcpAllowed MCP 三层判定（§4.1）：总开关 → 服务器白名单 → 工具白/黑名单。
func (g *Gateway) mcpAllowed(fullName string) bool {
	if !g.emp.MCPEnabled() {
		return false
	}
	if !g.emp.ServerAllowed(mcpServerOf(fullName)) {
		return false
	}
	switch g.emp.MCPMode {
	case MCPWhitelist:
		for _, rule := range g.emp.MCPTools {
			if MatchGlob(rule, fullName) {
				return true
			}
		}
		return false
	case MCPBlacklist:
		for _, rule := range g.emp.MCPTools {
			if MatchGlob(rule, fullName) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

// evolutionPathOK 校验提案写入路径：<workspace>/evolution-drafts/pending/<sig>.md，
// 签名为 8 位小写十六进制（§11.5 提案 ID 规范）。双区语义下绝对要求落在
// 员工工作空间内（相对路径按 cwd 解析后会落在会话项目目录 → 拒绝，
// 防提案写错位置后审批链路永远看不到）。
func (g *Gateway) evolutionPathOK(rawPath string) bool {
	p := strings.TrimSpace(rawPath)
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(g.relBase(), p)
	}
	abs = filepath.Clean(abs)
	// 提案固定写员工工作空间（审批/落地均从该目录读取）：双区语义下
	// 相对路径按 cwd 解析后会落在会话项目目录 → 拒绝，防提案写丢。
	if !hasDirPrefix(abs, g.workspace) {
		return false
	}
	base := filepath.Base(abs)
	if filepath.Base(filepath.Dir(abs)) != "pending" || !strings.HasSuffix(base, ".md") {
		return false
	}
	sig := strings.TrimSuffix(base, ".md")
	if len(sig) != 8 {
		return false
	}
	for _, c := range sig {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// relBase 相对路径解析基准：会话绑定员工时为会话工作区（cwd），
// 独立员工运行态（无 cwd）为员工工作空间——与工具进程实际 cwd 一致。
func (g *Gateway) relBase() string {
	if g.cwd != "" {
		return g.cwd
	}
	return g.workspace
}

// mcpModeName 权限模式中文名。
func mcpModeName(mode string) string {
	switch mode {
	case MCPWhitelist:
		return "白名单"
	case MCPBlacklist:
		return "黑名单"
	default:
		return "未限制"
	}
}

func isWriteTool(name string) bool { return writeTools[name] }

// pathArgOfTool 从工具 JSON 参数提取 path（fs 系工具）。
// 解析失败返回 ("", false) → 跳过目录检查（交由工具自身报参数错误）。
func pathArgOfTool(tool, args string) (string, bool) {
	if !pathArgTools[tool] || strings.TrimSpace(args) == "" {
		return "", false
	}
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil || p.Path == "" {
		return "", false
	}
	return p.Path, true
}

// truncateRunes 按字符截断（审计记录体积控制）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ContentSignature 提案内容签名（§11.5：8 位小写十六进制，sha256 截取）。
func ContentSignature(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:8]
}

// skillNameOf 从 skill 工具 JSON 参数提取技能名；解析失败返回空串（跳过检查，
// 交由工具自身报参数错误）。
func skillNameOf(args string) string {
	if strings.TrimSpace(args) == "" {
		return ""
	}
	var p struct {
		Skill string `json:"skill"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return ""
	}
	return strings.TrimSpace(p.Skill)
}

// mcpServerOf 从 mcp_<server>_<tool> 全名提取 server 段
// （与 internal/mcp sanitizeToolName 命名规则对齐：中间段为 server，末段为 tool）。
func mcpServerOf(fullName string) string {
	rest := strings.TrimPrefix(fullName, "mcp_")
	idx := strings.LastIndex(rest, "_")
	if idx <= 0 {
		return ""
	}
	return rest[:idx]
}

// mcpDenyReason MCP 拒绝文案：分层提示（总开关 / 服务器白名单 / 工具清单）。
func (g *Gateway) mcpDenyReason(tool string) string {
	if !g.emp.MCPEnabled() {
		return "MCP 已对该数字员工整体禁用：无法调用远程工具 " + tool +
			"。请改用内置工具完成任务，或请联系用户调整员工权限。"
	}
	if !g.emp.ServerAllowed(mcpServerOf(tool)) {
		return "MCP 服务器权限拒绝：服务器 " + mcpServerOf(tool) + " 不在该数字员工的服务器白名单内。" +
			"请联系用户调整 MCP 服务器白名单。"
	}
	return "工具权限拒绝：" + tool + " 不在该数字员工的" +
		mcpModeName(g.emp.MCPMode) + "清单内。请联系用户调整 MCP 工具权限。"
}

// skillDenyReason 技能拒绝文案：区分总开关与黑名单命中。
func (g *Gateway) skillDenyReason(skill string) string {
	if g.emp.SkillsDisabled {
		return "技能（SKILLS）已对该数字员工整体禁用：无法加载技能 " + skill +
			"。请直接按通用流程完成任务，或请联系用户调整员工权限。"
	}
	return "技能权限拒绝：技能 " + skill + " 被该数字员工的技能黑名单拒绝。" +
		"请改用其他方式完成任务，或请联系用户调整技能黑名单。"
}
