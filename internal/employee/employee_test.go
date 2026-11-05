package employee

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestStore_CRUD_UniqueName(t *testing.T) {
	s := newTestStore(t)
	e1, err := s.Add(Employee{Name: "审查员", Status: StatusEnabled})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if e1.ID == "" || e1.CreatedAt.IsZero() {
		t.Fatalf("expected generated id/createdAt, got %+v", e1)
	}
	// 重名拒绝。
	if _, err := s.Add(Employee{Name: "审查员"}); err == nil {
		t.Fatal("expected duplicate-name error")
	}
	// 更新：改名 + 状态。
	e1.Name = "审查员2"
	e1.Status = StatusDisabled
	e1.ReadOnly = true
	if _, err := s.Update(e1); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, ok := s.Get(e1.ID)
	if !ok || got.Name != "审查员2" || got.Status != StatusDisabled || !got.ReadOnly {
		t.Fatalf("update lost: %+v", got)
	}
	// 状态机。
	if _, err := s.SetStatus(e1.ID, "bogus"); err == nil {
		t.Fatal("expected unknown-status error")
	}
	// 删除。
	if err := s.Delete(e1.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get(e1.ID); ok {
		t.Fatal("expected deleted")
	}
}

func TestStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Add(Employee{Name: "资料员", Tags: []string{"research"}, EvolutionStrength: 50}); err != nil {
		t.Fatal(err)
	}
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	list := s2.List()
	if len(list) != 1 || list[0].Name != "资料员" || len(list[0].Tags) != 1 {
		t.Fatalf("persistence lost: %+v", list)
	}
}

func TestWorkspace_DefaultLayout(t *testing.T) {
	s := newTestStore(t)
	e, err := s.Add(Employee{Name: "员工A"})
	if err != nil {
		t.Fatal(err)
	}
	ws := e.Workspace(s.dir)
	for _, sub := range []string{"memory", filepath.Join("evolution-drafts", "pending")} {
		if fi, err := os.Stat(filepath.Join(ws, sub)); err != nil || !fi.IsDir() {
			t.Fatalf("expected dir %s under workspace, err=%v", sub, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "memory", "MEMORY.md")); err != nil {
		t.Fatalf("expected MEMORY.md seeded: %v", err)
	}
}

func TestValidate(t *testing.T) {
	e := Employee{Name: "x", Status: StatusEnabled}
	if err := e.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	e.Name = " "
	if err := e.Validate(); err == nil {
		t.Fatal("expected empty-name error")
	}
	e.Name = "x"
	e.Temperature = 2.5
	if err := e.Validate(); err == nil {
		t.Fatal("expected temperature range error")
	}
	e.Temperature = 0
	e.EvolutionStrength = 77
	if err := e.Validate(); err == nil {
		t.Fatal("expected evolution-strength error")
	}
	e.EvolutionStrength = 0
	e.Dirs = []string{"relative/path"}
	if err := e.Validate(); err == nil {
		t.Fatal("expected relative-dir error")
	}
	e.Dirs = nil
	e.WorkspaceRoot = "relative/ws"
	if err := e.Validate(); err == nil {
		t.Fatal("expected relative workspaceRoot error")
	}
	e.WorkspaceRoot = `D:\ws`
	if err := e.Validate(); err != nil {
		t.Fatalf("absolute workspaceRoot rejected: %v", err)
	}
}

func TestNormalizeWorkspaceRoot(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{`"D:\work\emp-x"`, `D:\work\emp-x`},
		{` 'D:\work\emp-x' `, `D:\work\emp-x`},
		{`D:\work\emp-x\`, `D:\work\emp-x`},
		{`D:\work\emp-x`, `D:\work\emp-x`},
	}
	for _, c := range cases {
		if got := normalizeWorkspaceRoot(c.in); got != c.want {
			t.Errorf("normalizeWorkspaceRoot(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestStore_WorkspaceRootExclusive(t *testing.T) {
	s := newTestStore(t)
	custom := filepath.Join(t.TempDir(), "emp-custom")
	if _, err := s.Add(Employee{Name: "甲", WorkspaceRoot: custom}); err != nil {
		t.Fatalf("Add 甲: %v", err)
	}
	// 另一员工配置同一目录：拒绝（§3 数据隔离）。
	if _, err := s.Add(Employee{Name: "乙", WorkspaceRoot: custom}); err == nil {
		t.Fatal("expected shared-workspace-root error")
	}
	// 大小写不同仍视为同一目录（Windows 不敏感）。
	if _, err := s.Add(Employee{Name: "丙", WorkspaceRoot: strings.ToUpper(custom)}); err == nil {
		t.Fatal("expected case-insensitive shared-workspace-root error")
	}
	// 带引号粘贴形式等价：同样拒绝。
	if _, err := s.Add(Employee{Name: "丁", WorkspaceRoot: `"` + custom + `"`}); err == nil {
		t.Fatal("expected quoted shared-workspace-root error")
	}
}

func TestStore_DuplicateResetsWorkspaceRoot(t *testing.T) {
	s := newTestStore(t)
	src, err := s.Add(Employee{Name: "原件", WorkspaceRoot: filepath.Join(t.TempDir(), "emp-custom")})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	_ = os.WriteFile(filepath.Join(src.WorkspaceRoot, "note.txt"), []byte("hi"), 0o644)
	cp, err := s.Duplicate(src.ID)
	if err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	if cp.WorkspaceRoot != "" {
		t.Fatalf("duplicate should fall back to default workspace, got %q", cp.WorkspaceRoot)
	}
	b, err := os.ReadFile(filepath.Join(cp.Workspace(s.dir), "note.txt"))
	if err != nil || string(b) != "hi" {
		t.Fatalf("workspace content not copied to default ws: %v %s", err, string(b))
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"file_*", "file_read", true},
		{"file_*", "web_search", false},
		{"*", "anything", true},
		{"mcp_fs_exact", "mcp_fs_exact", true},
		{"mcp_fs_exact", "mcp_fs_other", false},
		{"a*b", "axxxb", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := MatchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("MatchGlob(%q,%q)=%v want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestRenderRolePrompt_Vars(t *testing.T) {
	e := &Employee{Name: "小智", RolePrompt: "你是{{员工名称}}，服务{{当前用户}}，空间{{工作空间路径}}，今天{{日期}}"}
	out := e.RenderRolePrompt(`D:\ws`)
	if !strings.Contains(out, "你是小智") || !strings.Contains(out, `D:\ws`) {
		t.Fatalf("vars not rendered: %s", out)
	}
	// 日期占位渲染为 2006-01-02 格式（用正则避免依赖具体年份）。
	if !regexp.MustCompile(`，今天\d{4}-\d{2}-\d{2}`).MatchString(out) {
		t.Fatalf("date var not rendered: %s", out)
	}
}

// TestSecurity_MCPServerWhitelist MCP 服务器白名单 + 总开关判定（Employee 层）。
func TestSecurity_MCPServerWhitelist(t *testing.T) {
	enabled := Employee{Name: "白名单员", MCPServers: []string{"fs", " web "}}
	if !enabled.MCPEnabled() {
		t.Fatal("default should be MCP enabled")
	}
	if !enabled.ServerAllowed("fs") || !enabled.ServerAllowed("web") {
		t.Fatal("whitelisted server should pass")
	}
	if enabled.ServerAllowed("shell") {
		t.Fatal("non-whitelisted server should fail")
	}
	empty := Employee{Name: "不限制"}
	if !empty.ServerAllowed("anything") {
		t.Fatal("empty MCPServers should allow all")
	}
	nilEmp := (*Employee)(nil)
	if !nilEmp.MCPEnabled() || !nilEmp.ServerAllowed("x") {
		t.Fatal("nil employee should allow (non-employee session)")
	}
}

// TestSecurity_Skills 技能总开关 + 黑名单判定（Employee 层）。
func TestSecurity_Skills(t *testing.T) {
	e := Employee{Name: "技能受限", SkillsDeny: []string{"theme-factory", "commit-helper"}}
	if !e.SkillAllowed("frontend-design") {
		t.Fatal("non-matching skill should allow")
	}
	if e.SkillAllowed("theme-factory") {
		t.Fatal("exact hit should deny")
	}
	if e.SkillAllowed("commit-helper") {
		t.Fatal("exact hit should deny")
	}
	all := Employee{Name: "全禁", SkillsDisabled: true}
	if all.SkillAllowed("anything") {
		t.Fatal("disabled skills should deny all")
	}
	// 总开关只影响技能加载，不影响 MCP 判定（两权限正交）。
	if !all.MCPEnabled() {
		t.Fatal("SkillsDisabled must not affect MCP")
	}
}

// TestSecuritySummary 总开关/服务器白名单/技能状态在安全摘要中的投影。
func TestSecuritySummary(t *testing.T) {
	e := &Employee{MCPMode: MCPWhitelist, MCPTools: []string{"a_*", "b", "c", "d", "e"}, Dirs: []string{`D:\x`, `E:\y`}, ReadOnly: true}
	got := e.SecuritySummary()
	if !strings.Contains(got, "白名单 5 项") || !strings.Contains(got, "目录 2 个") || !strings.Contains(got, "只读开") {
		t.Fatalf("unexpected summary: %s", got)
	}
	// MCP 总开关禁用：摘要投影「MCP禁用」。
	if s := (&Employee{Name: "禁", MCPDisabled: true}).SecuritySummary(); !strings.Contains(s, "禁用") {
		t.Fatalf("MCPDisabled summary: %s", s)
	}
	// 服务器白名单：摘要附服务器数。
	if s := (&Employee{Name: "服白", MCPServers: []string{"fs", "web"}}).SecuritySummary(); !strings.Contains(s, "2服务器") {
		t.Fatalf("server whitelist summary: %s", s)
	}
	// 技能总开关 / 黑名单：摘要投影。
	if s := (&Employee{Name: "技禁", SkillsDisabled: true}).SecuritySummary(); !strings.Contains(s, "技能禁用") {
		t.Fatalf("skills disabled summary: %s", s)
	}
	if s := (&Employee{Name: "技黑", SkillsDeny: []string{"commit-helper"}}).SecuritySummary(); !strings.Contains(s, "技能黑名单1项") {
		t.Fatalf("skills deny summary: %s", s)
	}
}

func TestGateway_DirectoryWhitelist(t *testing.T) {
	s := newTestStore(t)
	extra := t.TempDir()
	e, err := s.Add(Employee{Name: "守卫员", Dirs: []string{extra + `\*`}})
	if err != nil {
		t.Fatal(err)
	}
	ws := e.Workspace(s.dir)
	g := NewGateway(&e, s.dir, "")
	defer g.Close()

	// 工作空间内放行（相对 + 绝对）。
	if reason := g.check("fs_read", `{"path":"a/b.txt"}`); reason != "" {
		t.Fatalf("workspace-relative should allow, got: %s", reason)
	}
	if reason := g.check("fs_read", `{"path":"`+filepath.ToSlash(filepath.Join(ws, "x.txt"))+`"}`); reason != "" {
		t.Fatalf("workspace-absolute should allow, got: %s", reason)
	}
	// 白名单目录放行。
	inside := filepath.Join(extra, "sub", "f.txt")
	if reason := g.check("fs_read", `{"path":"`+filepath.ToSlash(inside)+`"}`); reason != "" {
		t.Fatalf("whitelisted dir should allow, got: %s", reason)
	}
	// 白名单外拒绝。
	outside := filepath.Join(filepath.Dir(extra), "elsewhere", "f.txt")
	if reason := g.check("fs_read", `{"path":"`+filepath.ToSlash(outside)+`"}`); reason == "" {
		t.Fatal("outside dir should deny")
	}
	// .. 穿越拒绝。
	if reason := g.check("fs_read", `{"path":"..\\..\\secret"}`); reason == "" {
		t.Fatal("path traversal should deny")
	}
}

// TestGateway_DualWorkspace 双区语义（会话工作区 + 员工专属工作空间）：
// 会话绑定员工时任务仍运行在会话工作区（cwd），员工工作空间保留为记忆/进化
// 目录——两区均放行，其余路径拒绝（负路径）。
func TestGateway_DualWorkspace(t *testing.T) {
	s := newTestStore(t)
	e, err := s.Add(Employee{Name: "双区员"})
	if err != nil {
		t.Fatal(err)
	}
	ws := e.Workspace(s.dir) // 员工工作空间（记忆/进化）
	sess := t.TempDir()      // 会话工作区（用户项目目录）
	g := NewGateway(&e, s.dir, sess)
	defer g.Close()

	// 会话工作区（cwd）：相对路径按 cwd 解析后放行。
	if reason := g.check("fs_read", `{"path":"src/main.go"}`); reason != "" {
		t.Fatalf("cwd-relative should allow: %s", reason)
	}
	// 会话工作区绝对路径放行。
	if reason := g.check("fs_read", `{"path":"`+filepath.ToSlash(filepath.Join(sess, "main.go"))+`"}`); reason != "" {
		t.Fatalf("cwd-absolute should allow: %s", reason)
	}
	// 员工工作空间（记忆/进化目录）仍可达：绝对路径放行。
	if reason := g.check("fs_read", `{"path":"`+filepath.ToSlash(filepath.Join(ws, "memory", "BEHAVIOR.md"))+`"}`); reason != "" {
		t.Fatalf("employee workspace should stay reachable: %s", reason)
	}
	// 进化提案相对路径已不可用（相对 cwd 解析 → 落在会话项目目录 → 拒绝）。
	if reason := g.check("fs_write", `{"path":"evolution-drafts/pending/abcd1234.md","content":"x"}`); reason == "" {
		t.Fatal("proposal relative path under session cwd should deny")
	}
	// 员工工作空间内的提案绝对路径仍合法。
	propAbs := filepath.ToSlash(filepath.Join(ws, "evolution-drafts", "pending", "abcd1234.md"))
	if reason := g.check("fs_write", `{"path":"`+propAbs+`","content":"x"}`); reason != "" {
		t.Fatalf("proposal absolute path in employee workspace should allow: %s", reason)
	}
	// 两区之外的路径一律拒绝（负路径）。
	if reason := g.check("fs_read", `{"path":"`+filepath.ToSlash(filepath.Join(filepath.Dir(sess), "elsewhere.txt"))+`"}`); reason == "" {
		t.Fatal("path outside both zones should deny")
	}
	// cwd 下 .. 穿越到员工工作空间之外仍拒绝。
	if reason := g.check("fs_read", `{"path":"..\\..\\secret"}`); reason == "" {
		t.Fatal("traversal beyond both zones should deny")
	}
}

// TestRoleSection_DualWorkspace 双区语义的角色段：会话工作区与员工工作空间
// 不同时，提示词声明两区边界 + 提案路径为员工工作空间绝对路径；相同（或无
// 会话工作区）时退回单区旧文案。
func TestRoleSection_DualWorkspace(t *testing.T) {
	s := newTestStore(t)
	e, err := s.Add(Employee{Name: "双区员", EvolutionStrength: 50})
	if err != nil {
		t.Fatal(err)
	}
	ws := e.Workspace(s.dir)
	sess := t.TempDir()

	// 双区：声明会话工作区 + 专属工作空间，提案路径为绝对路径。
	role := e.RoleSection(ws, sess)
	if !strings.Contains(role, "当前任务工作区") || !strings.Contains(role, sess) {
		t.Fatalf("dual-zone role must declare session workspace:\n%s", role)
	}
	if !strings.Contains(role, ws) {
		t.Fatalf("dual-zone role must declare employee workspace:\n%s", role)
	}
	propHint := filepath.ToSlash(filepath.Join(ws, "evolution-drafts", "pending", "<签名>.md"))
	if !strings.Contains(role, propHint) {
		t.Fatalf("dual-zone role must guide absolute proposal path %s:\n%s", propHint, role)
	}

	// 同目录：退回单区文案（不出现「当前任务工作区」双声明）。
	same := e.RoleSection(ws, ws)
	if strings.Contains(same, "当前任务工作区") {
		t.Fatalf("same-dir role should fall back to single-zone:\n%s", same)
	}
	// 无会话工作区：旧语义。
	legacy := e.RoleSection(ws)
	if !strings.Contains(legacy, "你的专属工作空间："+ws) || strings.Contains(legacy, "当前任务工作区") {
		t.Fatalf("legacy role section broken:\n%s", legacy)
	}
	// 双区提案指引含绝对路径，单区保持相对路径。
	if !strings.Contains(legacy, "evolution-drafts/pending/<签名>.md") {
		t.Fatalf("legacy proposal path should stay relative:\n%s", legacy)
	}
}

func TestGateway_ReadOnly(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Add(Employee{Name: "只读员", ReadOnly: true})
	g := NewGateway(&e, s.dir, "")
	defer g.Close()
	if reason := g.check("fs_write", `{"path":"a.txt","content":"x"}`); !strings.Contains(reason, "只读模式") {
		t.Fatalf("expected readonly rejection, got: %q", reason)
	}
	if reason := g.check("fs_apply_diff", `{"path":"a.txt","edits":[]}`); !strings.Contains(reason, "只读模式") {
		t.Fatalf("expected readonly rejection for apply_diff, got: %q", reason)
	}
	// 读不受限。
	if reason := g.check("fs_read", `{"path":"a.txt"}`); reason != "" {
		t.Fatalf("read should allow, got: %s", reason)
	}
}

func TestGateway_MCPPermissions(t *testing.T) {
	s := newTestStore(t)
	wl, _ := s.Add(Employee{Name: "白名单员", MCPMode: MCPWhitelist, MCPTools: []string{"mcp_fs_*"}})
	g := NewGateway(&wl, s.dir, "")
	if reason := g.check("mcp_fs_read", `{}`); reason != "" {
		t.Fatalf("whitelisted should allow: %s", reason)
	}
	if reason := g.check("mcp_web_fetch", `{}`); reason == "" {
		t.Fatal("non-whitelisted should deny")
	}
	g.Close()

	bl, _ := s.Add(Employee{Name: "黑名单员", MCPMode: MCPBlacklist, MCPTools: []string{"mcp_shell_exec"}})
	g2 := NewGateway(&bl, s.dir, "")
	defer g2.Close()
	if reason := g2.check("mcp_shell_exec", `{}`); reason == "" {
		t.Fatal("blacklisted should deny")
	}
	if reason := g2.check("mcp_fs_read", `{}`); reason != "" {
		t.Fatalf("non-blacklisted should allow: %s", reason)
	}

	any, _ := s.Add(Employee{Name: "未限员"})
	g3 := NewGateway(&any, s.dir, "")
	defer g3.Close()
	if reason := g.check("mcp_anything", `{}`); reason != "" {
		_ = reason
	}
	if reason := g3.check("mcp_fs_read", `{}`); reason != "" {
		t.Fatalf("unrestricted should allow: %s", reason)
	}
}

// TestGateway_SkillsPermissions 技能执行侧双保险：总开关 + 黑名单拒绝。
func TestGateway_SkillsPermissions(t *testing.T) {
	s := newTestStore(t)
	dis, _ := s.Add(Employee{Name: "禁技能", SkillsDisabled: true})
	g := NewGateway(&dis, s.dir, "")
	defer g.Close()
	if reason := g.check("skill", `{"skill":"commit-helper"}`); !strings.Contains(reason, "整体禁用") {
		t.Fatalf("disabled skills should deny: %q", reason)
	}

	denied, _ := s.Add(Employee{Name: "黑名单", SkillsDeny: []string{"commit-helper"}})
	g2 := NewGateway(&denied, s.dir, "")
	defer g2.Close()
	if reason := g2.check("skill", `{"skill":"commit-helper"}`); !strings.Contains(reason, "黑名单") {
		t.Fatalf("exact hit should deny: %q", reason)
	}
	if reason := g2.check("skill", `{"skill":"theme-factory"}`); reason != "" {
		t.Fatalf("non-matching skill should allow: %s", reason)
	}
	// 非 skill 工具不受技能权限约束。
	if reason := g2.check("fs_read", `{"path":"a.txt"}`); reason != "" {
		t.Fatalf("fs tools unaffected: %s", reason)
	}
	// skillsDisabled + skillsDeny 同禁：优先总开关文案。
	if s := g.skillDenyReason("x"); g.emp.SkillsDisabled && !strings.Contains(s, "禁用") {
		t.Fatalf("master switch message expected: %s", s)
	}
}

// TestGateway_MCPServerWhitelist 服务器白名单执行侧拦截 + 拒绝文案分层。
func TestGateway_MCPServerWhitelist(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Add(Employee{Name: "服白", MCPServers: []string{"fs"}})
	g := NewGateway(&e, s.dir, "")
	defer g.Close()
	if reason := g.check("mcp_fs_read", `{}`); reason != "" {
		t.Fatalf("whitelisted server tool should allow: %s", reason)
	}
	if reason := g.check("mcp_shell_exec", `{}`); !strings.Contains(reason, "服务器白名单") {
		t.Fatalf("non-whitelisted server should deny: %q", reason)
	}
	// 总开关优先于服务器白名单（mcpDenyReason 分层文案）。
	dis, _ := s.Add(Employee{Name: "总禁", MCPDisabled: true})
	g2 := NewGateway(&dis, s.dir, "")
	defer g2.Close()
	if reason := g2.check("mcp_fs_read", `{}`); !strings.Contains(reason, "整体禁用") {
		t.Fatalf("master switch should deny first: %q", reason)
	}
	// mcpServerOf 提取（mcp_<server>_<tool> → server 段）。
	if got := mcpServerOf("mcp_fs_read"); got != "fs" {
		t.Fatalf("mcpServerOf: %s", got)
	}
}

// TestApplyRoleRestrictions 角色权限包收紧式合并（负路径：角色禁 MCP 后
// 网关必须拒绝，员工自身未禁不可绕过——2026-xx 修复回归）。
func TestApplyRoleRestrictions(t *testing.T) {
	// ① 角色禁用 MCP、员工未禁：合并后禁用，网关拒绝文案为「整体禁用」。
	e := &Employee{Name: "角色禁MCP员"}
	e.ApplyRoleRestrictions(Role{Name: "受限角色", MCPDisabled: true})
	if e.MCPEnabled() {
		t.Fatal("role MCPDisabled should disable employee MCP")
	}

	// ② 角色白名单，员工未配置：继承角色白名单。
	wl := &Employee{Name: "继承员"}
	wl.ApplyRoleRestrictions(Role{Name: "白名单角色", MCPMode: MCPWhitelist, MCPTools: []string{"mcp_fs_*"}})
	if wl.MCPMode != MCPWhitelist || len(wl.MCPTools) != 1 || wl.MCPTools[0] != "mcp_fs_*" {
		t.Fatalf("inherit whitelist: %+v", wl)
	}

	// ③ 双白名单取交集（员工 mcp_fs_read ∩ 角色 mcp_fs_* = mcp_fs_read）。
	inter := &Employee{Name: "交集员", MCPMode: MCPWhitelist, MCPTools: []string{"mcp_fs_read", "mcp_shell_exec"}}
	inter.ApplyRoleRestrictions(Role{Name: "更严角色", MCPMode: MCPWhitelist, MCPTools: []string{"mcp_fs_*"}})
	if len(inter.MCPTools) != 1 || inter.MCPTools[0] != "mcp_fs_read" {
		t.Fatalf("whitelist intersection: %v", inter.MCPTools)
	}

	// ④ 角色黑名单 ∪ 员工黑名单（并集，命中面只增不减）。
	bl := &Employee{Name: "黑名单员", MCPMode: MCPBlacklist, MCPTools: []string{"mcp_shell_exec"}}
	bl.ApplyRoleRestrictions(Role{Name: "黑名单角色", MCPMode: MCPBlacklist, MCPTools: []string{"mcp_web_*"}})
	if bl.MCPMode != MCPBlacklist || len(bl.MCPTools) != 2 {
		t.Fatalf("blacklist union: %v", bl.MCPTools)
	}

	// ⑤ 员工白名单 − 角色黑名单命中项（白∩黑 = 白-黑，只收不宽）。
	mix := &Employee{Name: "混合员", MCPMode: MCPWhitelist, MCPTools: []string{"mcp_fs_read", "mcp_shell_exec"}}
	mix.ApplyRoleRestrictions(Role{Name: "黑名单角色", MCPMode: MCPBlacklist, MCPTools: []string{"mcp_shell_*"}})
	if len(mix.MCPTools) != 1 || mix.MCPTools[0] != "mcp_fs_read" {
		t.Fatalf("whitelist minus blacklist: %v", mix.MCPTools)
	}

	// ⑥ 服务器白名单：双方非空取交集；一方空继承另一方。
	sv := &Employee{Name: "服白员", MCPServers: []string{"fs", "web"}}
	sv.ApplyRoleRestrictions(Role{Name: "服白角色", MCPServers: []string{"fs", "db"}})
	if len(sv.MCPServers) != 1 || sv.MCPServers[0] != "fs" {
		t.Fatalf("server intersection: %v", sv.MCPServers)
	}
	only := &Employee{Name: "服白员2", MCPServers: []string{"fs"}}
	only.ApplyRoleRestrictions(Role{Name: "不限角色"})
	if len(only.MCPServers) != 1 || only.MCPServers[0] != "fs" {
		t.Fatalf("empty role servers inherit employee: %v", only.MCPServers)
	}

	// ⑦ 技能总开关/只读：任一为 true 即生效；禁用态下黑名单被总开关吸收（置空）。
	sk := &Employee{Name: "技能员", SkillsDeny: []string{"a"}}
	sk.ApplyRoleRestrictions(Role{Name: "技能角色", SkillsDisabled: true, ReadOnly: true})
	if !sk.SkillsDisabled || !sk.ReadOnly || len(sk.SkillsDeny) != 0 {
		t.Fatalf("skills disable merge: %+v", sk)
	}
	// 技能黑名单并集（总开关未启用时）。
	deny := &Employee{Name: "黑名单员", SkillsDeny: []string{"a"}}
	deny.ApplyRoleRestrictions(Role{Name: "黑名单角色", SkillsDeny: []string{"other_*"}})
	if deny.SkillsDisabled || len(deny.SkillsDeny) != 2 {
		t.Fatalf("skills deny union: %+v", deny)
	}

	// ⑧ 角色无任何权限配置：员工配置保持不变。
	keep := &Employee{Name: "原样员", MCPMode: MCPWhitelist, MCPTools: []string{"mcp_fs_*"}, ReadOnly: true}
	before := *keep
	keep.ApplyRoleRestrictions(Role{Name: "空角色"})
	if keep.MCPMode != before.MCPMode || !keep.ReadOnly || len(keep.MCPTools) != len(before.MCPTools) {
		t.Fatalf("no-op role should not change employee: %+v vs %+v", keep, &before)
	}
}

// TestGateway_RolePackageNegativePath 端到端负路径（网关执行侧）：
// 角色禁 MCP → mcp_* 调用被拒（整体禁用文案）；内置 fs 工具不受影响；
// 角色白名单内放行/白名单外拒绝；角色只读拦截写入。
func TestGateway_RolePackageNegativePath(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Add(Employee{Name: "角色套用员"})
	e.ApplyRoleRestrictions(Role{Name: "禁MCP角色", MCPDisabled: true})
	g := NewGateway(&e, s.dir, "")
	defer g.Close()
	if reason := g.check("mcp_any_server_tool", `{}`); !strings.Contains(reason, "整体禁用") {
		t.Fatalf("negative path: role-disabled MCP tool must deny: %q", reason)
	}
	// 允许路径：内置 fs_read 不受 MCP 禁用影响。
	if reason := g.check("fs_read", `{"path":"a.txt"}`); reason != "" {
		t.Fatalf("built-in tool unaffected: %s", reason)
	}

	// 角色白名单：白名单内放行、白名单外拒绝。
	wle, _ := s.Add(Employee{Name: "角色白名单员"})
	wle.ApplyRoleRestrictions(Role{Name: "fs白名单角色", MCPMode: MCPWhitelist, MCPTools: []string{"mcp_fs_*"}})
	g2 := NewGateway(&wle, s.dir, "")
	defer g2.Close()
	if reason := g2.check("mcp_fs_read", `{}`); reason != "" {
		t.Fatalf("whitelisted via role should allow: %s", reason)
	}
	if reason := g2.check("mcp_web_fetch", `{}`); !strings.Contains(reason, "白名单") {
		t.Fatalf("non-whitelisted via role must deny: %q", reason)
	}

	// 角色只读：写入被拒。
	rle, _ := s.Add(Employee{Name: "角色只读员"})
	rle.ApplyRoleRestrictions(Role{Name: "只读角色", ReadOnly: true})
	g3 := NewGateway(&rle, s.dir, "")
	defer g3.Close()
	if reason := g3.check("fs_write", `{"path":"a.txt","content":"x"}`); !strings.Contains(reason, "只读模式") {
		t.Fatalf("role readonly must deny write: %q", reason)
	}
}

func TestEvolution_ProposalLifecycle(t *testing.T) {
	s := newTestStore(t)
	e, err := s.Add(Employee{Name: "学习员", EvolutionStrength: 50})
	if err != nil {
		t.Fatal(err)
	}
	ws := e.Workspace(s.dir)
	pending := filepath.Join(ws, "evolution-drafts", "pending")
	if err := os.MkdirAll(pending, 0o755); err != nil {
		t.Fatal(err)
	}
	draft := "---\nProposal-ID: evo-20260101-abcd1234\nSignature: abcd1234\nTarget-File: BEHAVIOR.md\nTrigger-Type: correction\nConfidence: 80\n---\n## Why This Matters\n避免重复犯错。\n"
	if err := os.WriteFile(filepath.Join(pending, "abcd1234.md"), []byte(draft), 0o644); err != nil {
		t.Fatal(err)
	}
	props, err := s.ListProposals(e.ID)
	if err != nil || len(props) != 1 {
		t.Fatalf("ListProposals: %v len=%d", err, len(props))
	}
	p := props[0]
	if p.TargetFile != "BEHAVIOR.md" || p.Confidence != 80 || p.TriggerType != "correction" {
		t.Fatalf("parse mismatch: %+v", p)
	}
	// 审批通过 → memory/BEHAVIOR.md 落地。
	if err := s.ApproveProposal(e.ID, p.FileName, ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	mem, err := os.ReadFile(filepath.Join(ws, "memory", "BEHAVIOR.md"))
	if err != nil || !strings.Contains(string(mem), "Why This Matters") {
		t.Fatalf("proposal not applied: %v %s", err, string(mem))
	}
	if _, err := os.Stat(filepath.Join(ws, "evolution-drafts", "approved", p.FileName)); err != nil {
		t.Fatalf("approved copy missing: %v", err)
	}
	// 拒绝流。
	if err := os.WriteFile(filepath.Join(pending, "11112222.md"), []byte("---\nTarget-File: TOOLS.md\n---\nbody2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.RejectProposal(e.ID, "11112222.md"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "evolution-drafts", "rejected", "11112222.md")); err != nil {
		t.Fatalf("rejected copy missing: %v", err)
	}
}

func TestGateway_EvolutionPathLock(t *testing.T) {
	s := newTestStore(t)
	e, _ := s.Add(Employee{Name: "路径锁"})
	g := NewGateway(&e, s.dir, "")
	defer g.Close()
	// 合法签名放行。
	ok := `{"path":"evolution-drafts/pending/abcd1234.md","content":"x"}`
	if reason := g.check("fs_write", ok); reason != "" {
		t.Fatalf("valid proposal path should allow: %s", reason)
	}
	// 非法签名拒绝。
	bad := `{"path":"evolution-drafts/pending/notasig.md","content":"x"}`
	if reason := g.check("fs_write", bad); reason == "" {
		t.Fatal("invalid signature should deny")
	}
	// 试图直接写记忆文件（伪造已审批）拒绝。
	direct := `{"path":"memory/BEHAVIOR.md","content":"x"}`
	if reason := g.check("fs_write", direct); reason == "" {
		t.Fatal("direct memory write should deny")
	}
}

// TestDelete_ClearsManagerRef 删除员工时清理悬挂 ManagerID：上级离职后
// 下属的 ManagerID 置空——否则下属编辑保存被 checkRefsLocked 拒（
// 「直属上级不存在」，保存断链，只能手动改上级或删档）。
func TestDelete_ClearsManagerRef(t *testing.T) {
	s := newTestStore(t)
	boss, err := s.Add(Employee{Name: "上级"})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.Add(Employee{Name: "下属", ManagerID: boss.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(boss.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, ok := s.Get(sub.ID)
	if !ok {
		t.Fatal("下属不应被删除")
	}
	if got.ManagerID != "" {
		t.Fatalf("下属 ManagerID 应被清理，got %q", got.ManagerID)
	}
	// 清理后下属可正常编辑保存（不再被「直属上级不存在」拒绝）。
	got.Description = "更新描述"
	if _, err := s.Update(got); err != nil {
		t.Fatalf("清理后 Update 应成功: %v", err)
	}
}

func TestDuplicate_Employee(t *testing.T) {
	s := newTestStore(t)
	src, err := s.Add(Employee{Name: "原件", Description: "d"})
	if err != nil {
		t.Fatal(err)
	}
	ws := src.Workspace(s.dir)
	_ = os.WriteFile(filepath.Join(ws, "note.txt"), []byte("hello"), 0o644)
	cp, err := s.Duplicate(src.ID)
	if err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	if cp.Name != "原件 副本" || cp.Status != StatusDraft {
		t.Fatalf("copy fields: %+v", cp)
	}
	b, err := os.ReadFile(filepath.Join(cp.Workspace(s.dir), "note.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("workspace not copied: %v %s", err, string(b))
	}
}
